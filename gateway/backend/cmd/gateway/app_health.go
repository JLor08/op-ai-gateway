// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"errors"
	"log"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/gateway"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync"
	"time"
)

// App-health loop tunables are package vars, not consts, so tests can shrink
// them (notably appHealthRetryGap -> ~1ms) to stay fast.
var (
	// appHealthDefaultInterval is the fail-open probe cadence used when the
	// health_check_interval_seconds setting cannot be read.
	appHealthDefaultInterval = 30 * time.Second
	// appHealthRetryGap is the wait between a failed probe and its single retry.
	appHealthRetryGap = 2 * time.Second
	// appHealthProbeConcurrency bounds concurrent probes within one server.
	appHealthProbeConcurrency = 8
)

// healthStore is the store surface the app-health loop needs. *store.SQLiteStore
// and *routing.MemoryStore both satisfy it.
type healthStore interface {
	AIServers(ctx context.Context) ([]routing.AIServer, error)
	AIServerByID(ctx context.Context, id string) (routing.AIServer, error)
	ApplicationsByServer(ctx context.Context, serverID string) ([]routing.Application, error)
	SetServerHealth(ctx context.Context, serverID, health string) error
	MappingsByApplication(ctx context.Context, applicationID string) ([]routing.ModelMapping, error)
	UpdateMappingContextProbe(ctx context.Context, id string, contextSize int, at time.Time) error
	InsertServerAvailabilitySample(ctx context.Context, sample routing.ServerAvailabilitySample) error
}

// availabilityHeartbeat: the health loop writes an availability sample at least
// this often per server even when the state is unchanged (anchors "still up" and
// bounds observer-gap resolution). Package var so tests can shorten it.
var availabilityHeartbeat = 5 * time.Minute

// availWriteState is the last availability sample written for a server, used to
// dedup unchanged states between heartbeats: a new sample is written only when
// the (health, agentReporting) state changed or the heartbeat is due.
type availWriteState struct {
	health           string
	agentReporting   bool
	netbirdConnected bool
	at               time.Time
}

// maxProbedContextSize is the ceiling for a probed n_ctx; a larger value is
// treated as a misbehaving upstream and ignored (never persisted).
const maxProbedContextSize = 100_000_000 // reject absurd n_ctx from a misbehaving upstream

// netbirdOnlyOffMeshError is recorded as an off-mesh application's LastError when
// the netbird_only outbound restriction forces it unreachable, so the reason it
// dropped from routing / the offered-models set is visible in the health snapshot.
const netbirdOnlyOffMeshError = "netbird_only: off-mesh server excluded"

// healthSettings is the system-settings source the loop reads the probe
// interval from each cycle.
type healthSettings interface {
	SystemSettings(ctx context.Context) (map[string]string, error)
}

// modelSyncer reconciles an application's model mappings against its upstream —
// the same reconciliation as the manual "sync models" button — returning an
// error when the upstream model listing fails (in which case no mappings
// change). It backs the model_sync health-check mode: a successful reconcile
// means the application is reachable. It also supplies the loop's ONE way to
// reach a live *portal.Service: ActiveAgentPresenceTimeoutSeconds is the
// env-aware effective system-wide agent-presence-timeout default (honors both
// a saved System Settings value AND, absent one,
// OP_AI_GATEWAY_AGENT_PRESENCE_TIMEOUT_SECONDS — the same value the "Agent"
// status column uses), consulted by appHealthCycleConfig so the availability
// loop and that column never diverge. Satisfied by *portal.Service.
type modelSyncer interface {
	SyncApplicationModelsForApp(ctx context.Context, server routing.AIServer, app routing.Application) (portal.SyncResultDTO, error)
	ActiveAgentPresenceTimeoutSeconds(ctx context.Context) int
}

// agentRegistryBundle is the set of per-server ServerAgent registries the app-health
// loop touches: it reads presence recency, and at the end of a COMPLETE cycle it has
// to bound EVERY such registry to the live server set. It is deliberately ONE
// parameter — this call chain already carries a dozen positional arguments, and each
// future registry would otherwise add another.
//
// An interface (rather than a struct) is what keeps a plain `nil` a valid argument:
// the two use sites below are nil-guarded, so passing nil behaves exactly like the
// nil-safe registry pointers did before. *gateway.AgentPresenceRegistry satisfies it
// on its own, so a test that only cares about presence can still pass just that.
type agentRegistryBundle interface {
	ReportingWithin(serverID string, window time.Duration) bool
	Retain(live map[string]struct{})
}

// agentRegistries is the production bundle: it delegates the presence read and fans
// the end-of-cycle Retain out to every registry. Both fields are nil-safe pointers,
// so a partially-wired bundle degrades instead of panicking.
type agentRegistries struct {
	presence    *gateway.AgentPresenceRegistry
	certReports *gateway.AgentCertReportRegistry
	transport   *gateway.AgentTransportRegistry
	// proxyStatus records what each ServerAgent reports as ACTUALLY running on
	// its local TLS-proxy routes (Certificates P4 Task 9); nil-safe. Its Retain
	// takes map[string]bool (not map[string]struct{} like its siblings — the
	// exact interface Task 10's switch reconcile consumes), so Retain below
	// converts the shared live set once per cycle.
	proxyStatus *gateway.AgentProxyStatusRegistry
	// runtimeStatus prunes the agent-runtime-manager per-server status/
	// file-mode registry (agent-runtime-manager Task 9, gateway.
	// NewRuntimeStatusRegistry). An inline interface, not a named concrete
	// type: gateway.runtimeStatusRegistry is unexported, so this field can
	// only ever be spelled structurally -- Go's interface satisfaction is
	// structural, so the exported constructor's *runtimeStatusRegistry return
	// value satisfies this without either side needing to name it. Left as
	// the interface's zero value (nil) in any test/wiring that does not care
	// about it; the explicit nil check in Retain below guards that (unlike
	// the other fields here, a nil INTERFACE cannot forward a method call to
	// a nil-safe receiver -- calling a method on a nil interface panics
	// regardless of the underlying method's own nil-receiver handling).
	runtimeStatus interface {
		Retain(live map[string]struct{})
	}
}

func (a agentRegistries) ReportingWithin(serverID string, window time.Duration) bool {
	return a.presence.ReportingWithin(serverID, window)
}

func (a agentRegistries) Retain(live map[string]struct{}) {
	a.presence.Retain(live)
	a.certReports.Retain(live)
	a.transport.Retain(live)
	if a.runtimeStatus != nil {
		a.runtimeStatus.Retain(live)
	}
	liveBool := make(map[string]bool, len(live))
	for id := range live {
		liveBool[id] = true
	}
	a.proxyStatus.Retain(liveBool)
}

// reportingWithin / retain are the nil-guarded accessors every use site goes through,
// so a nil bundle (the many tests that pass no registry at all) is a no-op / false
// rather than a nil-interface panic.
func reportingWithin(agents agentRegistryBundle, serverID string, window time.Duration) bool {
	if agents == nil {
		return false
	}
	return agents.ReportingWithin(serverID, window)
}

func retainAgents(agents agentRegistryBundle, live map[string]struct{}) {
	if agents == nil {
		return
	}
	agents.Retain(live)
}

// appHealthRunner holds the FIXED collaborators for one app-health loop
// instance -- the dependencies that never change across a cycle. Building one
// struct (instead of threading each collaborator as its own positional
// parameter through runOnce/runForServer/probeServer) is what collapses the
// ~14-20 parameter lists those used to carry down to the handful that
// genuinely vary per call: see cycleCfg (the once-per-cycle settings
// snapshot) and cycleState (the mutable, cross-server accumulators).
type appHealthRunner struct {
	store        healthStore
	prober       provider.Prober
	syncer       modelSyncer
	registry     *gateway.AppHealthRegistry
	loaded       *gateway.LoadedModelRegistry
	agents       agentRegistryBundle
	groups       *gateway.GroupRegistry
	settings     healthSettings
	probeTimeout time.Duration
	cipher       *capture.Cipher
	now          func() time.Time
}

// cycleCfg is the once-per-cycle settings snapshot read via
// appHealthCycleConfig (plus the cycle's timestamp): every server in one
// fleet pass, or the one server in a scoped pass, sees the SAME snapshot.
type cycleCfg struct {
	systemSeconds        int
	netbirdOnly          bool
	agentPresenceDefault int
	tNow                 time.Time
}

// cycleState bundles the mutable, cross-server bits threaded through one
// probe cycle: lastProbed/lastAvail persist across cycles (owned by the
// runAppHealthLoop goroutine and shared between its fleet and scoped passes),
// while seen/liveApps are throwaway per runOnce/runForServer call -- reset at
// the start of each.
type cycleState struct {
	lastProbed map[string]time.Time
	lastAvail  map[string]availWriteState
	seen       map[string]bool
	liveApps   map[string]struct{}
}

// cycleConfig reads the once-per-cycle settings snapshot (see
// appHealthCycleConfig) and stamps it with the cycle's timestamp.
func (r *appHealthRunner) cycleConfig(ctx context.Context) cycleCfg {
	systemSeconds, netbirdOnly, agentPresenceDefault := appHealthCycleConfig(ctx, r.settings, r.syncer)
	return cycleCfg{
		systemSeconds:        systemSeconds,
		netbirdOnly:          netbirdOnly,
		agentPresenceDefault: agentPresenceDefault,
		tNow:                 r.now(),
	}
}

// runAppHealthLoop probes application reachability and derives per-server health
// on an interval read from health_check_interval_seconds each cycle (re-read
// like the prune loop re-reads retention). It returns when ctx is cancelled.
//
// runner bundles every dependency (store/prober/syncer/registry/loaded/agents/
// groups/settings/probeTimeout/cipher/now) instead of taking each as its own
// parameter -- the caller builds it exactly like the test files already do to
// exercise runOnce/runForServer directly (see app_health_test.go). now
// defaults to time.Now().UTC when the caller leaves it nil (the production
// path via startAppHealthLoop).
func runAppHealthLoop(ctx context.Context, runner *appHealthRunner, serverTrigger <-chan string) {
	if runner.now == nil {
		runner.now = func() time.Time { return time.Now().UTC() }
	}
	// Per-application last-probe times so each application is probed on its own
	// effective cadence (its custom HealthCheckIntervalSeconds, else the system
	// setting). Persists across cycles; runOnce updates it in place.
	// Per-server last availability sample written, so the sampling below can dedup
	// unchanged states between heartbeats. Persists across cycles like lastProbed.
	state := &cycleState{
		lastProbed: make(map[string]time.Time),
		lastAvail:  make(map[string]availWriteState),
	}
	// Probe once immediately so server health + application reachability are
	// accurate within a probe cycle of STARTUP rather than only after the first
	// interval tick. The registry is lenient (unknown -> reachable) until then.
	select {
	case <-ctx.Done():
		return
	default:
	}
	interval := runner.runOnce(ctx, state)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case sid := <-serverTrigger:
			runner.runForServer(ctx, sid, state)
		case <-ticker.C:
			if next := runner.runOnce(ctx, state); next != interval {
				interval = next
				ticker.Reset(interval)
			}
		}
	}
}

// startAppHealthLoop launches runAppHealthLoop in a goroutine and returns its
// cancel func. It is a package var so a test can substitute a fake prober + fake
// store + short interval and observe the goroutine start/stop, mirroring
// startCapturePruneLoop. serverTrigger (may be nil) is the per-server immediate
// reaction channel — see appHealthRunner.runForServer; a nil channel is safe (a
// `case <-nil` in the loop select never fires).
var startAppHealthLoop = func(runner *appHealthRunner, serverTrigger <-chan string) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	go runAppHealthLoop(ctx, runner, serverTrigger)
	return cancel
}

// appHealthInterval reads the probe cadence from settings, failing open to
// appHealthDefaultInterval on a read error.
func appHealthInterval(ctx context.Context, settings healthSettings) time.Duration {
	values, err := settings.SystemSettings(ctx)
	if err != nil {
		return appHealthDefaultInterval
	}
	return time.Duration(portal.HealthCheckIntervalSeconds(values)) * time.Second
}

// appHealthCycleConfig reads the once-per-cycle system settings in a single pass:
// the probe cadence (seconds), the netbird_only outbound-restriction toggle, and
// the system-wide agent-presence-timeout default (seconds). The cadence and
// netbird_only fail OPEN on a settings read error — the default cadence and
// netbird_only=false — so a settings glitch can never blackhole the whole fleet
// by marking every off-mesh server unreachable; netbird_only takes at most one
// health interval to take effect (it is re-read each cycle). The agent-presence
// default is read via syncer.ActiveAgentPresenceTimeoutSeconds — the loop's ONE
// path to a live *portal.Service (modelSyncer is satisfied by *portal.Service at
// all production call sites) — so it is the SAME env-aware effective default the
// "Agent" status column uses (a saved System Settings value wins, else
// OP_AI_GATEWAY_AGENT_PRESENCE_TIMEOUT_SECONDS); nil syncer (a test double that
// does not wire one) falls back to the hardcoded portal.DefaultAgentPresenceTimeoutSeconds.
func appHealthCycleConfig(ctx context.Context, settings healthSettings, syncer modelSyncer) (systemSeconds int, netbirdOnly bool, agentPresenceDefault int) {
	agentPresenceDefault = portal.DefaultAgentPresenceTimeoutSeconds
	if syncer != nil {
		agentPresenceDefault = syncer.ActiveAgentPresenceTimeoutSeconds(ctx)
	}
	values, err := settings.SystemSettings(ctx)
	if err != nil {
		return int(appHealthDefaultInterval / time.Second), false, agentPresenceDefault
	}
	return portal.HealthCheckIntervalSeconds(values), portal.NetbirdOnly(values), agentPresenceDefault
}

// runOnce runs one probe cycle: for every server, probe each ACTIVE
// application whose per-application interval has elapsed — its custom
// HealthCheckIntervalSeconds, else the system-wide setting read this cycle —
// reusing the last observed reachability for applications not yet due; then
// derive and persist each server's health from the current reachability of all
// its active applications. It updates state.lastProbed in place and returns the
// next wake interval: the finest active application cadence, floored at the
// minimum allowed probe interval, so the caller can pace the ticker.
func (r *appHealthRunner) runOnce(ctx context.Context, state *cycleState) time.Duration {
	cfg := r.cycleConfig(ctx)
	state.seen = make(map[string]bool)
	// Start from the system cadence so an all-default (or empty) fleet keeps
	// waking at the system interval; narrow to the finest custom cadence below.
	nextSeconds := cfg.systemSeconds

	// Live id sets for the loaded-model registry's Retain (drops entries for
	// deleted apps/servers). enumComplete gates Retain so a transient
	// per-server list error does not spuriously evict live entries.
	state.liveApps = make(map[string]struct{})
	liveServers := make(map[string]struct{})
	enumComplete := true

	servers, err := r.store.AIServers(ctx)
	if err != nil {
		log.Printf("app health: list servers failed: %v", err)
		return clampWakeInterval(nextSeconds)
	}
	for _, server := range servers {
		liveServers[server.ID] = struct{}{}
		var ok bool
		nextSeconds, ok = r.probeServer(ctx, server, cfg, state, nextSeconds)
		if !ok {
			enumComplete = false
		}
	}
	// Drop last-probe times for applications that no longer exist.
	for id := range state.lastProbed {
		if !state.seen[id] {
			delete(state.lastProbed, id)
		}
	}
	// Evict loaded-model entries for deleted apps/servers so the registry does not
	// grow unbounded over a long-running process. Skipped on an incomplete
	// enumeration so a transient store error can't wrongly evict live entries.
	if enumComplete {
		r.loaded.Retain(state.liveApps, liveServers)
		// Bound the agent-presence + availability-dedup state to live servers so a
		// deleted server does not leave a stale entry behind (nil-safe on presence).
		retainAgents(r.agents, liveServers)
		for id := range state.lastAvail {
			if _, live := liveServers[id]; !live {
				delete(state.lastAvail, id)
			}
		}
		// Refresh the model-group registry snapshot as the periodic backstop (the
		// portal refreshes it synchronously after each group / model-setting write;
		// this catches any missed / out-of-band change). Best-effort: RefreshGroups
		// only swaps its snapshot on success, so a transient store error keeps the
		// last good view. Nil-safe.
		_ = r.groups.RefreshGroups(ctx)
	}
	return clampWakeInterval(nextSeconds)
}

// probeServer runs one probe+derive+sample pass for a SINGLE server: probe
// each active application whose per-application cadence is due (reusing the last
// observed reachability otherwise), run the loaded-model + context-size probe
// passes, derive+persist the server health, and append an availability sample when
// changed / heartbeat-due. It mutates state.lastProbed/lastAvail/seen/liveApps in
// place and returns the finest cadence it saw (narrowed from nextSeconds) plus
// ok=false only when the application list could not be read (the fleet caller
// then skips Retain). The caller supplies the once-per-cycle cfg so every server
// in a fleet pass sees a consistent snapshot.
func (r *appHealthRunner) probeServer(ctx context.Context, server routing.AIServer, cfg cycleCfg, state *cycleState, nextSeconds int) (int, bool) {
	apps, err := r.store.ApplicationsByServer(ctx, server.ID)
	if err != nil {
		log.Printf("app health: list applications for %s failed: %v", server.ID, err)
		return nextSeconds, false
	}
	for _, app := range apps {
		state.liveApps[app.ID] = struct{}{}
	}
	active := make([]routing.Application, 0, len(apps))
	for _, app := range apps {
		if app.Status == routing.ServerStatusActive {
			active = append(active, app)
		}
	}

	// netbird_only outbound restriction: when the toggle is ON, a server that
	// is NOT a NetBird peer is off-mesh and must never be dialed. offMesh is
	// enforced per application below (before the always_reachable short-circuit)
	// and also suppresses this server's loaded-model + context probes so no pass
	// dials it. NetBird-enabled servers are unaffected (their reachability still
	// comes from the real probe over the tunnel); with the toggle OFF this is a
	// no-op (byte-identical to prior behaviour).
	offMesh := cfg.netbirdOnly && !server.NetbirdEnabled

	reachable := make([]bool, len(active))
	var wg sync.WaitGroup
	sem := make(chan struct{}, appHealthProbeConcurrency)
	for i := range active {
		app := active[i]
		state.seen[app.ID] = true
		if offMesh {
			// netbird_only ON: this off-mesh server is never dialed. Force the
			// application unreachable HERE — before the always_reachable
			// short-circuit and instead of any probe — so both routing (the
			// resolver) and the offered-models set drop it via the shared
			// reachability registry.
			r.registry.Set(app.ID, false, cfg.tNow, netbirdOnlyOffMeshError)
			state.lastProbed[app.ID] = cfg.tNow
			reachable[i] = false
			continue
		}
		mode := routing.EffectiveHealthCheckMode(app)
		if mode == routing.HealthCheckModeAlwaysReachable {
			// Always reachable: no probe, no cadence — set it every cycle.
			r.registry.Set(app.ID, true, cfg.tNow, "")
			state.lastProbed[app.ID] = cfg.tNow
			reachable[i] = true
			continue
		}
		eff := routing.EffectiveHealthCheckIntervalSeconds(app, cfg.systemSeconds, portal.MinHealthCheckIntervalSeconds, portal.MaxHealthCheckIntervalSeconds)
		if eff < nextSeconds {
			nextSeconds = eff
		}
		if last, ok := state.lastProbed[app.ID]; ok && cfg.tNow.Sub(last) < time.Duration(eff)*time.Second {
			// Not due yet: keep the last observed reachability so server health
			// still reflects this application.
			reachable[i] = r.registry.Reachable(app.ID)
			continue
		}
		state.lastProbed[app.ID] = cfg.tNow
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, app routing.Application, mode string) {
			defer wg.Done()
			defer func() { <-sem }()
			var ok bool
			var errMsg string
			// model_sync checks reachability via the model-discovery endpoint
			// and reconciles the application's mappings as a side effect on
			// success; health_path (and any legacy default) does an HTTP probe.
			if mode == routing.HealthCheckModeModelSync {
				ok, errMsg = syncApplication(ctx, r.syncer, server, app, r.probeTimeout)
			} else {
				// Attach the app's per-app upstream credential to the probe (fail-open).
				token, _ := capture.OpenSecret(r.cipher, app.APIToken)
				pctx := provider.WithUpstreamAuth(ctx, app.APITokenHeader, token)
				ok, errMsg = probeApplication(pctx, r.prober, server, app, r.probeTimeout)
			}
			r.registry.Set(app.ID, ok, cfg.tNow, errMsg)
			reachable[idx] = ok
		}(i, app, mode)
	}

	// Loaded-model probe pass: for every active application that declares a
	// status path, GET it on the application's own cadence and record the
	// result in the loaded-model registry (the gateway-poll source). This is
	// independent of the health-check mode — even an always_reachable app can
	// expose a loaded-models endpoint — so it runs as its own pass rather than
	// inside the health branch above. It shares wg+sem with the health probes.
	// A distinct "loaded:"+id cadence key keeps it from colliding with the
	// health cadence; seen[key] is marked for EVERY such app (due or not) so
	// the cleanup below does not drop a not-yet-due app's last-probe time.
	// Skipped entirely for an off-mesh server under netbird_only so this pass
	// never dials it.
	if r.loaded != nil && !offMesh {
		lister, hasLister := r.prober.(provider.LoadedModelLister)
		for i := range active {
			app := active[i]
			if !hasLister || strings.TrimSpace(app.LoadedModelsPath) == "" {
				continue
			}
			key := "loaded:" + app.ID
			state.seen[key] = true
			eff := routing.EffectiveHealthCheckIntervalSeconds(app, cfg.systemSeconds, portal.MinHealthCheckIntervalSeconds, portal.MaxHealthCheckIntervalSeconds)
			if eff < nextSeconds {
				nextSeconds = eff
			}
			if last, ok := state.lastProbed[key]; ok && cfg.tNow.Sub(last) < time.Duration(eff)*time.Second {
				continue
			}
			state.lastProbed[key] = cfg.tNow
			wg.Add(1)
			sem <- struct{}{}
			go func(app routing.Application) {
				defer wg.Done()
				defer func() { <-sem }()
				target := routing.Target{
					Provider: app.Type,
					Endpoint: routing.ApplicationEndpoint(server, app),
					Timeout:  r.probeTimeout,
				}
				// Attach the app's per-app upstream credential to the probe (fail-open).
				token, _ := capture.OpenSecret(r.cipher, app.APIToken)
				pctx := provider.WithUpstreamAuth(ctx, app.APITokenHeader, token)
				models, err := lister.LoadedModels(pctx, target, app.LoadedModelsPath, app.LoadedModelsFormat)
				if err != nil {
					// Retry once after a short gap before clearing, mirroring
					// probeApplication's anti-flap behaviour, so a single transient
					// blip does not drop the loaded set for a whole cycle.
					select {
					case <-ctx.Done():
						return
					case <-time.After(appHealthRetryGap):
					}
					models, err = lister.LoadedModels(pctx, target, app.LoadedModelsPath, app.LoadedModelsFormat)
				}
				if err != nil {
					// A persistently failing probe clears the app's gateway-poll set
					// so a stale "loaded" badge does not linger after the endpoint dies.
					r.loaded.SetGatewayProbe(app.ID, nil)
					return
				}
				r.loaded.SetGatewayProbe(app.ID, models)
			}(app)
		}
	}

	// Context-size probe pass: for every active application that declares a
	// ContextProbePath, GET it on the application's own cadence (llama.cpp
	// /props), match each reported model to a mapping by AppModelName, and
	// persist its context_size (metrics_source "probe"). Like the loaded pass
	// this is independent of the health-check mode and runs on its own "ctx:"+id
	// cadence key (seen-marked for EVERY such app so cleanup keeps its last-probe
	// time), sharing wg+sem so wg.Wait() below awaits it too. It is purely a
	// STORE write — no routing/selection change (a later phase consumes it) — and
	// the store's metrics_locked guard makes a manual pin win atomically.
	// Skipped entirely for an off-mesh server under netbird_only so this pass
	// never dials it.
	if r.prober != nil && !offMesh {
		ctxProber, hasCtxProber := r.prober.(provider.ModelInfoProber)
		for i := range active {
			app := active[i]
			if !hasCtxProber || strings.TrimSpace(app.ContextProbePath) == "" {
				continue
			}
			key := "ctx:" + app.ID
			state.seen[key] = true
			eff := routing.EffectiveHealthCheckIntervalSeconds(app, cfg.systemSeconds, portal.MinHealthCheckIntervalSeconds, portal.MaxHealthCheckIntervalSeconds)
			if eff < nextSeconds {
				nextSeconds = eff
			}
			if last, ok := state.lastProbed[key]; ok && cfg.tNow.Sub(last) < time.Duration(eff)*time.Second {
				continue
			}
			state.lastProbed[key] = cfg.tNow
			wg.Add(1)
			sem <- struct{}{}
			go func(app routing.Application) {
				defer wg.Done()
				defer func() { <-sem }()
				target := routing.Target{
					Provider: app.Type,
					Endpoint: routing.ApplicationEndpoint(server, app),
					Timeout:  r.probeTimeout,
				}
				// Attach the app's per-app upstream credential to the probe (fail-open).
				token, _ := capture.OpenSecret(r.cipher, app.APIToken)
				pctx := provider.WithUpstreamAuth(ctx, app.APITokenHeader, token)

				if strings.Contains(app.ContextProbePath, "{model}") {
					// Per-model endpoint: substitute {model} with each genuinely-loaded
					// model's upstream name and attribute the returned context size
					// DIRECTLY to that mapping — sidestepping the reported-name match the
					// single-probe path relies on. Gated on the loaded-model registry so a
					// not-currently-loaded model is never probed nor attributed.
					loadedSet := map[string]struct{}{}
					for _, name := range r.loaded.LoadedAppModels(app.ID, server.ID) {
						loadedSet[name] = struct{}{}
					}
					if len(loadedSet) == 0 {
						return // nothing known-loaded -> can't guarantee loadedness -> probe nothing
					}
					mappings, err := r.store.MappingsByApplication(ctx, app.ID)
					if err != nil {
						return
					}
					for _, mp := range mappings {
						if mp.Status != routing.ServerStatusActive || mp.MetricsLocked || mp.AppModelName == "" {
							continue
						}
						if _, ok := loadedSet[mp.AppModelName]; !ok {
							continue // only probe genuinely-loaded models
						}
						probePath := provider.ExpandModelPath(app.ContextProbePath, mp.AppModelName)
						infos, perr := ctxProber.ProbeModelInfo(pctx, target, probePath)
						if perr != nil {
							select {
							case <-ctx.Done():
								return
							case <-time.After(appHealthRetryGap):
							}
							infos, perr = ctxProber.ProbeModelInfo(pctx, target, probePath)
						}
						if perr != nil {
							continue
						}
						ctxSize := provider.PickModelContextSize(infos, mp.AppModelName)
						if ctxSize <= 0 || ctxSize > maxProbedContextSize || ctxSize == mp.ContextSize {
							continue
						}
						_ = r.store.UpdateMappingContextProbe(ctx, mp.ID, ctxSize, r.now())
					}
					return
				}

				infos, err := ctxProber.ProbeModelInfo(pctx, target, app.ContextProbePath)
				if err != nil {
					// Retry once after a short gap before giving up, mirroring the
					// health + loaded probes' anti-flap behaviour.
					select {
					case <-ctx.Done():
						return
					case <-time.After(appHealthRetryGap):
					}
					infos, err = ctxProber.ProbeModelInfo(pctx, target, app.ContextProbePath)
				}
				if err != nil || len(infos) == 0 {
					// A failed/empty probe leaves the stored context_size as-is (unlike
					// the loaded pass, an absent context size must not be "cleared").
					return
				}
				mappings, err := r.store.MappingsByApplication(ctx, app.ID)
				if err != nil {
					return
				}
				for _, info := range infos {
					// Ignore unknown (0) or absurd values; the store guards metrics_locked.
					if info.ContextSize <= 0 || info.ContextSize > maxProbedContextSize {
						continue
					}
					for _, mp := range mappings {
						// Skip a locked mapping client-side too (avoids a wasted DB
						// round-trip every cadence for a locked-divergent row); the
						// store's atomic metrics_locked = 0 guard stays the source of
						// truth. Skip an unchanged value so provenance does not churn.
						if mp.AppModelName == info.Name && !mp.MetricsLocked && mp.ContextSize != info.ContextSize {
							_ = r.store.UpdateMappingContextProbe(ctx, mp.ID, info.ContextSize, r.now())
						}
					}
				}
			}(app)
		}
	}
	wg.Wait()

	reachableCount := 0
	for _, ok := range reachable {
		if ok {
			reachableCount++
		}
	}
	health := deriveServerHealth(len(active), reachableCount)
	if err := r.store.SetServerHealth(ctx, server.ID, health); err != nil {
		log.Printf("app health: set health for %s failed: %v", server.ID, err)
	}

	// Event-sourced availability sampling: append a sample whenever the derived
	// (health, agent-reporting) state changes or the heartbeat is due, so the
	// availability history captures every transition without churning a row per
	// cycle. Best-effort — a write error is logged and never disrupts the loop or
	// SetServerHealth; lastAvail is updated ONLY on a successful insert so a
	// failed write is retried next cycle. agentPresence is nil-safe (-> false).
	// The window is the EFFECTIVE per-server agent-presence timeout — the
	// server's own AgentPresenceTimeoutSeconds override when set, else the
	// system-wide default read this cycle — mirroring the same computation
	// serverDTO uses for the "Agent" status column, so the availability
	// timeline and that column always agree.
	agentWindow := time.Duration(routing.EffectiveAgentPresenceTimeoutSeconds(server, cfg.agentPresenceDefault, portal.MinAgentPresenceTimeoutSeconds, portal.MaxAgentPresenceTimeoutSeconds)) * time.Second
	agentReporting := reportingWithin(r.agents, server.ID, agentWindow)
	netbirdConnected := server.NetbirdConnected
	prev, seenAvail := state.lastAvail[server.ID]
	changed := !seenAvail || prev.health != health ||
		prev.agentReporting != agentReporting || prev.netbirdConnected != netbirdConnected
	heartbeatDue := seenAvail && cfg.tNow.Sub(prev.at) >= availabilityHeartbeat
	if changed || heartbeatDue {
		sample := routing.ServerAvailabilitySample{
			ServerID:         server.ID,
			ReportedAt:       cfg.tNow,
			Health:           health,
			ReachableCount:   reachableCount,
			ActiveCount:      len(active),
			AgentReporting:   agentReporting,
			NetbirdConnected: netbirdConnected,
		}
		if err := r.store.InsertServerAvailabilitySample(ctx, sample); err != nil {
			log.Printf("app health: insert availability sample for %s failed: %v", server.ID, err)
		} else {
			state.lastAvail[server.ID] = availWriteState{health: health, agentReporting: agentReporting, netbirdConnected: netbirdConnected, at: cfg.tNow}
		}
	}
	return nextSeconds, true
}

// runForServer runs a single scoped health pass for one server, out of
// band from the fleet cadence — the immediate reaction to a ServerAgent
// reactivation. It reads the once-per-cycle cfg, loads the one server, and runs
// probeServer against the SHARED state.lastProbed/lastAvail maps (so the fleet loop
// then treats these apps as freshly probed). It does NOT run Retain or prune
// lastProbed (fleet-only concerns), and does not touch the ticker cadence. It MUST
// be called from the app-health loop goroutine so the shared maps stay race-free.
func (r *appHealthRunner) runForServer(ctx context.Context, serverID string, state *cycleState) {
	server, err := r.store.AIServerByID(ctx, serverID)
	if err != nil {
		log.Printf("app health (server %s): lookup failed: %v", serverID, err)
		return
	}
	cfg := r.cycleConfig(ctx)
	state.seen = make(map[string]bool)         // throwaway: no lastProbed cleanup in a scoped pass
	state.liveApps = make(map[string]struct{}) // throwaway: no Retain in a scoped pass
	_, _ = r.probeServer(ctx, server, cfg, state, cfg.systemSeconds)
}

// clampWakeInterval floors the loop's wake cadence at the minimum allowed probe
// interval (and guards a non-positive value) so a stray value can't spin the loop.
func clampWakeInterval(seconds int) time.Duration {
	if seconds < portal.MinHealthCheckIntervalSeconds {
		seconds = portal.MinHealthCheckIntervalSeconds
	}
	return time.Duration(seconds) * time.Second
}

// probeApplication probes one application, retrying once after appHealthRetryGap
// on failure. It returns reachability and the last error message (empty when
// reachable).
func probeApplication(ctx context.Context, prober provider.Prober, server routing.AIServer, app routing.Application, probeTimeout time.Duration) (bool, string) {
	target := routing.Target{
		Provider: app.Type,
		Endpoint: routing.ApplicationEndpoint(server, app),
		Timeout:  probeTimeout,
	}
	if err := prober.Probe(ctx, target, app.HealthCheckPath); err == nil {
		return true, ""
	}
	select {
	case <-time.After(appHealthRetryGap):
	case <-ctx.Done():
		return false, ctx.Err().Error()
	}
	if err := prober.Probe(ctx, target, app.HealthCheckPath); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// syncApplication reconciles one application's model mappings via the system
// model syncer (model_sync mode) and reports REACHABILITY of the upstream. It
// retries once after appHealthRetryGap when the upstream listing fails.
//
// Reachability tracks the upstream model-discovery call only: the syncer returns
// portal.ErrApplicationSyncFailed exactly when ListModels fails (or is
// unavailable), so that — and only that — marks the application unreachable. Any
// OTHER error is a local reconcile/persistence problem AFTER the upstream
// answered; it is logged but must NOT take a healthy upstream out of routing
// (this is why health_path mode, which never touches the store, cannot be
// knocked offline by a local DB hiccup either).
func syncApplication(ctx context.Context, syncer modelSyncer, server routing.AIServer, app routing.Application, probeTimeout time.Duration) (bool, string) {
	if syncer == nil {
		return false, "model syncer unavailable"
	}
	if reachable, errMsg := attemptModelSync(ctx, syncer, server, app, probeTimeout); reachable {
		return true, errMsg
	}
	select {
	case <-time.After(appHealthRetryGap):
	case <-ctx.Done():
		return false, ctx.Err().Error()
	}
	return attemptModelSync(ctx, syncer, server, app, probeTimeout)
}

// attemptModelSync runs one reconcile and classifies the outcome into
// reachability. The reconcile's ListModels uses http.DefaultClient (no client
// timeout) with a zero Target.Timeout, so each attempt is bounded with
// probeTimeout via the context to keep a hung upstream from stalling the probe
// cycle.
func attemptModelSync(ctx context.Context, syncer modelSyncer, server routing.AIServer, app routing.Application, probeTimeout time.Duration) (bool, string) {
	attemptCtx := ctx
	if probeTimeout > 0 {
		var cancel context.CancelFunc
		attemptCtx, cancel = context.WithTimeout(ctx, probeTimeout)
		defer cancel()
	}
	_, err := syncer.SyncApplicationModelsForApp(attemptCtx, server, app)
	switch {
	case err == nil:
		return true, ""
	case errors.Is(err, portal.ErrApplicationSyncFailed):
		// Upstream model listing failed -> unreachable.
		return false, err.Error()
	default:
		// The upstream answered (listing succeeded); a local reconcile/store
		// error must not take the application offline. Log and stay reachable.
		log.Printf("app health: model_sync reconcile for %s hit a local error (upstream reachable): %v", app.ID, err)
		return true, ""
	}
}

// deriveServerHealth maps active/reachable application counts to a server health
// state: all reachable -> healthy; some -> degraded; none (including zero active
// applications) -> unhealthy.
func deriveServerHealth(activeCount, reachableCount int) string {
	switch {
	case activeCount == 0:
		return routing.HealthUnhealthy
	case reachableCount == activeCount:
		return routing.HealthHealthy
	case reachableCount == 0:
		return routing.HealthUnhealthy
	default:
		return routing.HealthDegraded
	}
}
