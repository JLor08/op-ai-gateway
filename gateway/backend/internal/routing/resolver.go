// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/storeerr"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

var (
	ErrNoModelRoute  = errors.New("routing.no_model_route")
	ErrNoHealthyHost = errors.New("routing.no_healthy_host")
	// ErrAdmissionQueueTimeout: an unpinned request waited for a free concurrency slot up
	// to the per-app admission_queue_timeout_seconds without one freeing. Maps to HTTP 503.
	ErrAdmissionQueueTimeout = errors.New("routing.admission_queue_timeout")
	// ErrAdmissionQueueFull: the admission queue was already at its configured max depth.
	// Maps to HTTP 503.
	ErrAdmissionQueueFull = errors.New("routing.admission_queue_full")
	// ErrServerOverrideServerUnavailable: a server-override request named a server that
	// exists (offers the requested model) but is currently unusable for routing — either
	// disabled, or unhealthy/unreachable and the override did not force through it.
	ErrServerOverrideServerUnavailable = errors.New("routing.server_override_server_unavailable")
	// ErrServerOverrideModelUnavailable: a server-override request named a server that has
	// no live (active mapping + active app) offering of the requested model.
	ErrServerOverrideModelUnavailable = errors.New("routing.server_override_model_unavailable")
)

// errAllAtCapacity is an INTERNAL sentinel from selectCandidate: every candidate is at its
// effective cap AND an admission controller is wired, so Resolve should queue + retry.
var errAllAtCapacity = errors.New("routing: all candidates at capacity")

type Target struct {
	RouteID       string // serving model mapping id (kept for usage attribution)
	ServerID      string
	Provider      string
	Endpoint      string
	Model         string
	ProviderModel string
	Timeout       time.Duration
	APIFlavor     string
	// APIToken is the resolved application's per-app upstream credential, still
	// SEALED (enc:/plain:). Routing never decrypts it; the cipher-holding gateway
	// layers open it and attach it to the upstream call. Empty = no upstream auth.
	APIToken string
	// APITokenHeader is an optional custom header name for APIToken; empty ⇒ the
	// default "Authorization: Bearer <token>".
	APITokenHeader string
	// NativeResponses / NativeMessages mirror the resolved application's
	// native-passthrough flags, so the handler can decide whether to proxy the raw
	// client body to the upstream (Codex /v1/responses resp. Claude Code
	// /v1/messages) instead of translating it.
	NativeResponses bool
	NativeMessages  bool
	// OpportunisticMetrics mirrors the resolved application's per-app toggle: when
	// set, a successful real inference EWMA-updates the served mapping's throughput
	// metrics from the usage event.
	OpportunisticMetrics bool
}

// ReachabilityChecker reports whether an application is currently reachable.
// The resolver gates candidate selection and affinity reuse through it so that
// applications whose active reachability probe is failing are not routed to. A
// nil checker (passed to NewResolver) means all-reachable — lenient, preserving
// the pre-reachability behavior.
type ReachabilityChecker interface {
	Reachable(appID string) bool
}

// ServerBusyChecker reports whether a server is temporarily unavailable for routing
// (e.g. a benchmark is running on it). A nil checker means never busy, preserving
// the pre-benchmark behavior.
type ServerBusyChecker interface {
	ServerBusy(serverID string) bool
}

// LoadedModelChecker reports which upstream (app) model names are currently loaded for
// an application on a server, so selection can prefer a server that already has the
// requested model resident (avoiding a cold load/swap). A nil checker (or empty result)
// means loaded state is unknown — no candidate is treated as loaded, so selection is
// unchanged. Satisfied by *gateway.LoadedModelRegistry.
type LoadedModelChecker interface {
	LoadedAppModels(appID, serverID string) []string
}

// ServerActivityChecker reports a server's live inference activity for swap-protection:
// how many requests are in flight and when the last one completed. A nil checker means
// activity is unknown, so no server is protected — selection is unchanged (the P4b
// no-op invariant), exactly like a nil ServerBusyChecker / LoadedModelChecker.
type ServerActivityChecker interface {
	ServerActivity(serverID string) (inFlight int, lastCompletedAt time.Time)
}

// AdmissionController parks an unpinned request until a slot frees on one of serverIDs
// (returns nil => Resolve retries selection), or bounds it out (ErrAdmissionQueueTimeout /
// ErrAdmissionQueueFull / a context error). A nil controller disables queuing: selectCandidate
// keeps the CP3 all-at-cap fail-open. Satisfied by *gateway.admissionQueue.
type AdmissionController interface {
	WaitForSlot(ctx context.Context, serverIDs []string, timeout time.Duration) error
}

// ProvisioningGate decides which of a set of candidate server ids the principal
// may use under resource-group provisioning + the enforcement mode. A nil gate
// (unset) allows all (no-op invariant). Returns a map keyed by server id with
// value true for ALLOWED servers. Satisfied by a Task 3 adapter over the
// resource_group_provisions store.
type ProvisioningGate interface {
	AllowedServerIDs(ctx context.Context, principal auth.Token, serverIDs []string) (map[string]bool, error)
}

// GroupPolicy is a group's selection policy: how members are ordered, which are
// eligible, and how failover behaves. Passed as one value so the seam stays
// readable and extensible.
type GroupPolicy struct {
	FailoverMode            string
	MemberOrder             string
	LoadedOnly              bool
	ClimbSpeedMarginPercent int
	MinTokensPerSecond      float64
	MinSpeedFallback        string
}

// GroupResolver exposes the model-group config to the hot path. A nil resolver
// means no groups exist (preserving the pre-feature single-model behavior — the
// No-Op invariant). Satisfied by *gateway.GroupRegistry.
type GroupResolver interface {
	// Group returns a group's ordered members and its selection policy when name is
	// an active group; ok=false when name is not a group.
	Group(name string) (members []GroupMember, policy GroupPolicy, ok bool)
	// DirectAllowed reports whether a direct (non-group) request for modelName is
	// permitted. false only when the model's ModelSetting.Visibility == "locked"
	// (a group-only model, reachable solely via a group). Model-level, not membership.
	DirectAllowed(modelName string) bool
}

// ModelWarmer triggers a best-effort background load of a gateway model (climb_up
// load-ahead). nil = no-op (climb_up then only switches when a member is already
// loaded by other traffic). Warm MUST be non-blocking and deduplicated. Wired in
// Task 3; the field/setter exist now so Task 3 is a pure additive change.
type ModelWarmer interface {
	Warm(ctx context.Context, gatewayModelName string)
}

// sessionReservation tracks, per server, the distinct sticky sessions recently pinned
// there (an affinity hit OR a fresh pin), so the capacity cap can reserve one slot per
// live session and keep new/unpinned traffic from filling the slots active conversations
// will return to (Decision 5). Purely in-memory + volatile; a request keyed by an empty
// id is not tracked. BOTH touch (on write) and activeCount (on read) prune entries older
// than the window, so a touched server's map stays bounded by its live sessions even when
// the cap's read-time prune never runs for it (an uncapped max_concurrency==0 mapping, or
// a server that only serves pinned traffic and so never appears as a selectCandidate
// candidate) — without the write-time prune those maps would grow with cumulative, not
// live, sessions.
type sessionReservation struct {
	mu       sync.Mutex
	window   time.Duration
	byServer map[string]map[string]time.Time // serverID -> reservationKey -> lastSeen
}

func newSessionReservation(window time.Duration) *sessionReservation {
	return &sessionReservation{window: window, byServer: map[string]map[string]time.Time{}}
}

// touch records/refreshes a reservation for (serverID, key) at now, pruning that server's
// stale entries first so the map stays bounded by live sessions. A zero window or an empty
// serverID/key is a no-op. Nil-safe.
func (s *sessionReservation) touch(serverID, key string, now time.Time) {
	if s == nil || s.window <= 0 || serverID == "" || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.byServer[serverID]
	if m == nil {
		m = map[string]time.Time{}
		s.byServer[serverID] = m
	}
	// Prune this server's stale entries on write so the map is bounded by live sessions
	// even for servers the cap's read-time activeCount prune never visits (maxC==0 /
	// pinned-only servers).
	for existing, seen := range m {
		if now.Sub(seen) >= s.window {
			delete(m, existing)
		}
	}
	m[key] = now
}

// activeCount returns the number of distinct live reservations on serverID (lastSeen
// within the window), pruning stale entries. Nil-safe / zero-window => 0.
func (s *sessionReservation) activeCount(serverID string, now time.Time) int {
	if s == nil || s.window <= 0 || serverID == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.byServer[serverID]
	if m == nil {
		return 0
	}
	live := 0
	for k, seen := range m {
		if now.Sub(seen) >= s.window {
			delete(m, k)
			continue
		}
		live++
	}
	if len(m) == 0 {
		delete(s.byServer, serverID)
	}
	return live
}

// resolverStore is the narrow slice of Store the Resolver actually calls (see
// score_servers.go's and this file's r.store.* call sites) — Store's other
// ~109 methods (server/service/resource-group/limits/certificate management,
// etc.) are irrelevant to routing resolution. *MemoryStore and
// *store.SQLStore already implement it structurally (Go interfaces are
// implicit), so every existing NewResolver call site is unchanged.
type resolverStore interface {
	ActiveMappingsForModel(ctx context.Context, gatewayModel string, apiFlavor string) ([]MappingCandidate, error)
	TelemetryByServer(ctx context.Context, serverID string) (ServerTelemetry, bool, error)
	Affinity(ctx context.Context, key AffinityKey) (RouteAffinity, bool, error)
	UpsertAffinity(ctx context.Context, affinity RouteAffinity) error
	DeleteAffinity(ctx context.Context, key AffinityKey) error
	ApplicationByID(ctx context.Context, id string) (Application, error)
	AIServerByID(ctx context.Context, id string) (AIServer, error)
	MappingsByApplication(ctx context.Context, applicationID string) ([]ModelMapping, error)
}

type Resolver struct {
	store             resolverStore
	clock             func() time.Time
	checker           ReachabilityChecker
	busy              ServerBusyChecker
	loaded            LoadedModelChecker
	activity          ServerActivityChecker
	swapProtectWindow time.Duration
	reservation       *sessionReservation
	admission         AdmissionController
	provisioning      ProvisioningGate
	groups            GroupResolver
	warmer            ModelWarmer
	legacyAffinity    atomic.Bool // true => affinity keys on the explicit header (legacy); false (default) => ClientSessionID
}

func NewResolver(store resolverStore, clock func() time.Time, checker ReachabilityChecker) *Resolver {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Resolver{store: store, clock: clock, checker: checker}
}

// SetServerBusyChecker installs the routing exclusion for benchmarking servers.
// Leaving it unset (nil) means no server is ever considered busy.
func (r *Resolver) SetServerBusyChecker(c ServerBusyChecker) { r.busy = c }

// SetLoadedModelChecker installs the prefer-already-loaded partition source.
// Leaving it unset (nil) means loaded state is unknown, so selection is unchanged.
func (r *Resolver) SetLoadedModelChecker(c LoadedModelChecker) { r.loaded = c }

// SetServerActivityChecker installs the swap-protection activity source and the recency
// window. window <= 0 disables the recency component (an in-flight request still protects).
func (r *Resolver) SetServerActivityChecker(c ServerActivityChecker, window time.Duration) {
	r.activity = c
	r.swapProtectWindow = window
}

// SetSessionReservation installs the session-slot reservation tracker used by the
// capacity cap (Decision 5). window <= 0 (or leaving it unset) disables reservation
// tracking, so effectiveCap == max_concurrency. Nil-safe.
func (r *Resolver) SetSessionReservation(window time.Duration) {
	if window > 0 {
		r.reservation = newSessionReservation(window)
	}
}

// SetAdmissionController installs the CP4 admission queue. Nil (unset) disables queuing
// (CP3 fail-open on all-at-cap).
func (r *Resolver) SetAdmissionController(c AdmissionController) { r.admission = c }

// SetProvisioningGate installs the resource-group provisioning filter. Leaving it
// unset (nil) means every candidate server is allowed for every principal (the
// no-op invariant) — the concrete gate (a later task) enforces resource-group
// membership + the configured enforcement mode.
func (r *Resolver) SetProvisioningGate(g ProvisioningGate) { r.provisioning = g }

// SetGroupResolver installs the model-group config source. Leaving it unset (nil)
// means no groups exist, so Resolve is byte-identical to the single-model path
// (the No-Op invariant).
func (r *Resolver) SetGroupResolver(g GroupResolver) { r.groups = g }

// SetModelWarmer installs the climb_up load-ahead warmer. Leaving it unset (nil)
// makes climb_up purely passive (switch only when a higher-priority member is
// already loaded by other traffic). Consumed by Task 3.
func (r *Resolver) SetModelWarmer(w ModelWarmer) { r.warmer = w }

// SetAffinitySessionMode selects the affinity session-key source. legacy=true keys
// on the explicit X-OP-AI-Gateway-Session-ID header only (byte-identical to the
// pre-feature behavior); legacy=false (the zero value / default) keys on the
// extracted ClientSessionID. Safe to call live (atomic).
func (r *Resolver) SetAffinitySessionMode(legacy bool) { r.legacyAffinity.Store(legacy) }

// resolveTracer is resolved ONCE from the OTel global provider (installed by
// internal/tracing.Setup) and reused per call, so Resolve avoids the two
// process-global tracer-provider lookup mutexes otel.Tracer(name) takes on every
// call — keeping the disabled (default) path cheap. It is resolved directly from
// the global rather than via internal/tracing because the generated routing.Store
// tracing decorator lives in package tracing and imports routing, so routing must
// not import tracing (import cycle). Set at package-init, never reassigned.
var resolveTracer = otel.Tracer("op-ai-gateway")

func (r *Resolver) Resolve(ctx context.Context, token auth.Token, req inference.Request) (Target, error) {
	// When tracing is disabled the global provider yields a cheap non-recording span.
	ctx, span := resolveTracer.Start(ctx, "routing.Resolve")
	defer span.End()
	if span.IsRecording() {
		span.SetAttributes(attribute.String("model", req.Model))
	}
	if r == nil || r.store == nil {
		return Target{}, ErrNoModelRoute
	}
	now := r.clock()
	apiFlavor := NormalizeAPIFlavor(req.APIFlavor)
	// Server-override force-branch: an explicit req.ServerOverrideID short-circuits
	// EVERYTHING below it (model-group dispatch, affinity, the ActiveMappingsForModel
	// candidate block) — it is a distinct, minimal routing path, not a filter layered
	// onto the normal one. See resolveServerOverride for exactly which checks still
	// apply (model-on-server + reachability/force) and which are deliberately bypassed
	// (provisioning gate, affinity, the maintenance-status exclusion).
	if req.ServerOverrideID != "" {
		return r.resolveServerOverride(ctx, req, apiFlavor)
	}
	affinitySession := req.ClientSessionID
	if r.legacyAffinity.Load() {
		affinitySession = req.SessionID
	}
	key := AffinityKey{APITokenID: token.ID, Model: req.Model, APIFlavor: apiFlavor, SessionID: affinitySession}
	// Model-group dispatch. Gated on r.groups != nil so a resolver with no group
	// seam is byte-identical to today (the No-Op invariant): Group returns ok=false
	// for a plain model and DirectAllowed returns true (not locked), so the whole
	// block is transparent.
	if r.groups != nil {
		if members, policy, ok := r.groups.Group(req.Model); ok {
			if !r.groups.DirectAllowed(req.Model) {
				return Target{}, ErrNoModelRoute // a locked group: not directly requestable
			}
			return r.resolveGroup(ctx, token, req, key, apiFlavor, members, policy, now)
		}
		if !r.groups.DirectAllowed(req.Model) {
			return Target{}, ErrNoModelRoute // a locked group-only model requested directly
		}
	}
	if token.ID != "" {
		target, ok, err := r.resolveAffinity(ctx, key, now)
		if err != nil {
			return Target{}, err
		}
		if ok {
			allowed := true
			if r.provisioning != nil {
				okMap, perr := r.provisioning.AllowedServerIDs(ctx, token, []string{target.ServerID})
				if perr != nil {
					return Target{}, perr
				}
				allowed = okMap[target.ServerID]
			}
			if allowed {
				r.reservation.touch(target.ServerID, affinityID(key), now)
				return target, nil
			}
			// The pinned server is no longer provisioned for this principal: ignore the
			// pin (do not return it) and fall through to fresh candidate selection below,
			// which itself re-applies filterProvisioned so a blocked server is excluded
			// there too.
		}
	}

	candidates, err := r.store.ActiveMappingsForModel(ctx, req.Model, apiFlavor)
	if err != nil {
		return Target{}, fmt.Errorf("resolve mappings: %w", err)
	}
	candidates, err = r.filterProvisioned(ctx, token, candidates)
	if err != nil {
		return Target{}, err
	}
	if len(candidates) == 0 {
		return Target{}, ErrNoModelRoute
	}

	// Admission-queue params (CP4): the servers to watch for a freeing slot, and the wait
	// timeout = the MAX admission_queue_timeout_seconds across the candidate apps (0 = at
	// least one app is willing to wait unboundedly => ctx-only wait). Computed once — the
	// model's candidate set is stable across a short wait.
	serverIDs := make([]string, 0, len(candidates))
	seenServer := map[string]struct{}{}
	queueTimeoutSecs := 0
	for _, c := range candidates {
		if _, dup := seenServer[c.Server.ID]; !dup {
			seenServer[c.Server.ID] = struct{}{}
			serverIDs = append(serverIDs, c.Server.ID)
		}
		if c.Application.AdmissionQueueTimeoutSeconds > queueTimeoutSecs {
			queueTimeoutSecs = c.Application.AdmissionQueueTimeoutSeconds
		}
	}
	queueTimeout := time.Duration(queueTimeoutSecs) * time.Second
	// A bounded timeout is an absolute WALL-CLOCK deadline, NOT a fresh per-wait budget:
	// each WaitForSlot gets the REMAINING time. Otherwise the queue's internal liveness
	// re-check (which returns after a sub-second interval to force a cap re-read) would
	// re-arm the full timeout every iteration and a request under sustained saturation
	// would never hit its configured 503. queueTimeout<=0 stays unbounded (ctx-only wait).
	var deadline time.Time
	if queueTimeout > 0 {
		deadline = now.Add(queueTimeout)
	}

	for {
		selected, ok, err := r.selectCandidate(ctx, candidates, req, now)
		if errors.Is(err, errAllAtCapacity) {
			// All candidates at cap + a wired controller: park until a slot frees, then retry.
			wait := queueTimeout // 0 => unbounded (ctx-only)
			if queueTimeout > 0 {
				if wait = deadline.Sub(r.clock()); wait <= 0 {
					return Target{}, ErrAdmissionQueueTimeout // deadline already elapsed
				}
			}
			if werr := r.admission.WaitForSlot(ctx, serverIDs, wait); werr != nil {
				return Target{}, werr // ErrAdmissionQueueTimeout / ErrAdmissionQueueFull / ctx err
			}
			now = r.clock()
			continue
		}
		if err != nil {
			return Target{}, err
		}
		if !ok {
			return Target{}, ErrNoHealthyHost
		}
		target := targetFrom(selected.Server, selected.Application, selected.Mapping, apiFlavor)
		if token.ID != "" && selected.Application.AffinityTTLSeconds > 0 {
			if err := r.store.UpsertAffinity(ctx, RouteAffinity{
				ID:            affinityID(key),
				APITokenID:    token.ID,
				UserID:        token.UserID,
				Model:         req.Model,
				APIFlavor:     apiFlavor,
				SessionID:     key.SessionID,
				ApplicationID: selected.Application.ID,
				ServerID:      selected.Server.ID,
				ExpiresAt:     now.Add(time.Duration(selected.Application.AffinityTTLSeconds) * time.Second),
				LastUsedAt:    now,
				CreatedAt:     now,
				UpdatedAt:     now,
			}); err != nil {
				return Target{}, fmt.Errorf("store affinity: %w", err)
			}
			r.reservation.touch(selected.Server.ID, affinityID(key), now)
		}
		return target, nil
	}
}

// resolveServerOverride forces routing to exactly req.ServerOverrideID, bypassing the
// provisioning gate, affinity, and the maintenance-status exclusion (serverSelectable is
// deliberately NOT consulted here — a maintenance-status server IS routable via an
// override). The gateway handler has already re-authorized the principal for this
// specific server before calling Resolve; this method only performs the ROUTING checks:
// the requested model must actually be offered by that server (a live active mapping on
// an active application), and the server must be enabled + reachable unless the caller
// explicitly forces through an unhealthy/unreachable one.
func (r *Resolver) resolveServerOverride(ctx context.Context, req inference.Request, apiFlavor string) (Target, error) {
	candidates, err := r.store.ActiveMappingsForModel(ctx, req.Model, apiFlavor)
	if err != nil {
		return Target{}, fmt.Errorf("resolve mappings: %w", err)
	}
	var mine []MappingCandidate
	for _, c := range candidates {
		if c.Server.ID == req.ServerOverrideID {
			mine = append(mine, c)
		}
	}
	if len(mine) == 0 {
		return Target{}, ErrServerOverrideModelUnavailable
	}
	server := mine[0].Server
	if server.Status == ServerStatusDisabled {
		return Target{}, ErrServerOverrideServerUnavailable
	}
	// Reachability mirrors the two signals serverSelectable + the affinity/selection path
	// use (server health + application reachability), EXCEPT the maintenance-status
	// exclusion baked into serverSelectable (which this override deliberately bypasses —
	// ServerStatusMaintenance is neither ServerStatusDisabled above nor unhealthy here, so
	// it falls through to routed).
	reachable := server.HealthStatus != HealthUnhealthy
	if r.checker != nil {
		reachable = reachable && r.checker.Reachable(mine[0].Application.ID)
	}
	if !reachable && !req.ServerOverrideForceUnreachable {
		return Target{}, ErrServerOverrideServerUnavailable
	}
	// One server usually offers exactly one mapping for a given gateway model; pick the
	// first (deterministic: the store's ActiveMappingsForModel result is stably ordered).
	c := mine[0]
	return targetFrom(c.Server, c.Application, c.Mapping, apiFlavor), nil
}

// filterProvisioned drops candidates whose server the principal may not use under
// resource-group provisioning. A nil gate or empty input is a no-op (unchanged) —
// the invariant that keeps every existing call site byte-identical when no
// ProvisioningGate is wired. A gate error is propagated to the caller (the
// concrete gate decides fail-open vs fail-closed per the enforcement mode).
func (r *Resolver) filterProvisioned(ctx context.Context, principal auth.Token, cands []MappingCandidate) ([]MappingCandidate, error) {
	if r.provisioning == nil || len(cands) == 0 {
		return cands, nil
	}
	ids := make([]string, 0, len(cands))
	seen := map[string]bool{}
	for _, c := range cands {
		if !seen[c.Server.ID] {
			seen[c.Server.ID] = true
			ids = append(ids, c.Server.ID)
		}
	}
	allowed, err := r.provisioning.AllowedServerIDs(ctx, principal, ids)
	if err != nil {
		return nil, err
	}
	out := cands[:0:0]
	for _, c := range cands {
		if allowed[c.Server.ID] {
			out = append(out, c)
		}
	}
	return out, nil
}

func NormalizeAPIFlavor(apiFlavor string) string {
	normalized := strings.ToLower(strings.TrimSpace(apiFlavor))
	switch {
	case strings.HasPrefix(normalized, "openai"):
		return APIFlavorOpenAI
	case strings.HasPrefix(normalized, "anthropic"):
		return APIFlavorAnthropic
	default:
		return normalized
	}
}

func (r *Resolver) resolveAffinity(ctx context.Context, key AffinityKey, now time.Time) (Target, bool, error) {
	affinity, ok, err := r.store.Affinity(ctx, key)
	if err != nil {
		return Target{}, false, fmt.Errorf("lookup affinity: %w", err)
	}
	if !ok {
		return Target{}, false, nil
	}
	if !affinity.ExpiresAt.After(now) {
		_ = r.store.DeleteAffinity(ctx, key)
		return Target{}, false, nil
	}
	app, err := r.store.ApplicationByID(ctx, affinity.ApplicationID)
	if errors.Is(err, storeerr.ErrNotFound) {
		_ = r.store.DeleteAffinity(ctx, key)
		return Target{}, false, nil
	}
	if err != nil {
		return Target{}, false, fmt.Errorf("load affinity application: %w", err)
	}
	if app.AffinityTTLSeconds <= 0 || app.ServerID != affinity.ServerID || app.Status != ServerStatusActive || !applicationHasAPIFlavor(app, key.APIFlavor) || (r.checker != nil && !r.checker.Reachable(app.ID)) {
		_ = r.store.DeleteAffinity(ctx, key)
		return Target{}, false, nil
	}
	server, err := r.store.AIServerByID(ctx, app.ServerID)
	if errors.Is(err, storeerr.ErrNotFound) {
		_ = r.store.DeleteAffinity(ctx, key)
		return Target{}, false, nil
	}
	if err != nil {
		return Target{}, false, fmt.Errorf("load affinity server: %w", err)
	}
	if !serverSelectable(server) {
		_ = r.store.DeleteAffinity(ctx, key)
		return Target{}, false, nil
	}
	if r.busy != nil && r.busy.ServerBusy(server.ID) {
		_ = r.store.DeleteAffinity(ctx, key)
		return Target{}, false, nil
	}
	mapping, ok, err := r.activeMappingForApplication(ctx, app.ID, key.Model)
	if err != nil {
		return Target{}, false, err
	}
	if !ok {
		_ = r.store.DeleteAffinity(ctx, key)
		return Target{}, false, nil
	}
	affinity.LastUsedAt = now
	affinity.UpdatedAt = now
	if err := r.store.UpsertAffinity(ctx, affinity); err != nil {
		return Target{}, false, fmt.Errorf("update affinity: %w", err)
	}
	return targetFrom(server, app, mapping, key.APIFlavor), true, nil
}

// estInputTokens is a deliberately LENIENT estimate of a request's prompt token count
// (word count via strings.Fields over each message's text, times ~1.3 tokens/word); it
// under-counts dense/code/CJK text, which is the correct-by-design direction for a
// filter that should reject ONLY what clearly cannot fit and otherwise fails open in
// selectCandidate. Images/tool JSON are ignored (Text() is text-only).
func estInputTokens(req inference.Request) int {
	words := 0
	for _, msg := range req.Messages {
		words += len(strings.Fields(msg.Text()))
	}
	return int(float64(words)*1.3) + 1
}

// requestFitsContext reports whether a request needing `need` tokens fits a mapping
// whose context window is `contextSize`. An unknown context (<= 0) always "fits".
func requestFitsContext(contextSize, need int) bool {
	return contextSize <= 0 || need <= contextSize
}

func (r *Resolver) selectCandidate(ctx context.Context, candidates []MappingCandidate, req inference.Request, now time.Time) (MappingCandidate, bool, error) {
	model := req.Model
	// A pathological or overflowing MaxTokens that drives need non-positive simply
	// disables the filter (every mapping "fits", so the pool is unchanged) — fail-safe,
	// route anyway; no clamp is needed.
	need := estInputTokens(req) + req.MaxTokens

	// Pass 1: the reachable/selectable/non-busy candidates (the existing gates).
	reachable := make([]MappingCandidate, 0, len(candidates))
	for _, c := range candidates {
		if !serverSelectable(c.Server) {
			continue
		}
		if r.checker != nil && !r.checker.Reachable(c.Application.ID) {
			continue
		}
		if r.busy != nil && r.busy.ServerBusy(c.Server.ID) {
			continue
		}
		reachable = append(reachable, c)
	}
	if len(reachable) == 0 {
		return MappingCandidate{}, false, nil
	}

	// Context-fit hard filter, with fail-open: keep candidates whose known context
	// window can hold the request; if that empties the set, fall back to all reachable
	// (better to answer than to refuse) and warn.
	pool := make([]MappingCandidate, 0, len(reachable))
	for _, c := range reachable {
		if requestFitsContext(c.Mapping.ContextSize, need) {
			pool = append(pool, c)
		}
	}
	if len(pool) == 0 {
		slog.Warn("context-fit filter empty; routing without it", "model", model, "need_tokens", need)
		pool = reachable
	}

	// Capacity cap (reservation-aware), CP3: PREFER candidates whose server is below its
	// effective concurrency ceiling for new/unpinned traffic —
	// effectiveCap = max_concurrency − activeReservedSessions(server). Gated on BOTH a
	// known max_concurrency (>0) AND a live activity signal (r.activity != nil, the source
	// of the in-flight load k) — otherwise a no-op (today's behaviour). A pinned request
	// never reaches here (resolveAffinity returns first). FAIL-OPEN on BOTH emptiness AND
	// viability (exactly like swap-protection): if the under-cap subset is empty, OR it
	// yields no Score-viable pick, fall back to the full pool rather than refuse a request
	// an at-capacity-but-viable server could still serve (the upstream then queues it — CP4
	// replaces this fallback with the admission queue). The emptiness-only variant was a
	// verification-caught MAJOR: it could strip the sole viable-but-at-cap candidate and
	// leave only under-cap non-viable ones → ErrNoHealthyHost. So max_concurrency==0
	// everywhere (the under-cap subset == the full pool) is exactly the pre-CP3 behaviour.
	if r.activity != nil {
		capped := make([]MappingCandidate, 0, len(pool))
		for _, c := range pool {
			maxC := c.Mapping.MaxConcurrency
			if maxC <= 0 {
				capped = append(capped, c) // unknown capacity => no cap
				continue
			}
			k, _ := r.activity.ServerActivity(c.Server.ID)
			effectiveCap := maxC - r.reservation.activeCount(c.Server.ID, now)
			if k < effectiveCap {
				capped = append(capped, c)
			}
		}
		if len(capped) == 0 {
			if r.admission != nil {
				// Every candidate is at its effective cap: let Resolve queue + retry (CP4)
				// instead of failing open. Pinned requests never reach here (resolveAffinity
				// returns earlier), so only unpinned traffic queues. A nil controller keeps the
				// CP3 fail-open below.
				return MappingCandidate{}, false, errAllAtCapacity
			}
			slog.Warn("capacity-cap filter empty; routing without it (all candidates at capacity)", "model", model)
		} else if len(capped) < len(pool) {
			if sel, ok, err := r.selectWithSwapProtection(ctx, capped, model, now); err != nil || ok {
				return sel, ok, err
			}
			slog.Warn("capacity-cap survivors non-viable; routing without it", "model", model)
		}
	}
	return r.selectWithSwapProtection(ctx, pool, model, now)
}

// selectWithSwapProtection applies the swap-protection filter to pool, then selects.
// A NOT-loaded candidate whose server is actively serving (a request in flight, or a
// completion within swapProtectWindow) would evict that live model if routed here — prefer
// to exclude it this pass to avoid thrashing an in-use model. A loaded candidate is never
// protected (serving the requested model is no swap). Fail-open on BOTH emptiness AND
// viability: if protection removes some candidates but the survivors yield no viable pick
// (or it removes them all), fall back to the full pool rather than refuse a request a busy
// server could still serve. Gated on r.activity != nil (the no-op invariant).
func (r *Resolver) selectWithSwapProtection(ctx context.Context, pool []MappingCandidate, model string, now time.Time) (MappingCandidate, bool, error) {
	if r.activity != nil {
		protected := make([]MappingCandidate, 0, len(pool))
		for _, c := range pool {
			if !r.swapProtected(c, now) {
				protected = append(protected, c)
			}
		}
		if len(protected) == 0 {
			slog.Warn("swap-protection filter empty; routing without it", "model", model)
		} else if len(protected) < len(pool) {
			if sel, ok, err := r.selectFromPool(ctx, protected, model, now); err != nil || ok {
				return sel, ok, err
			}
			slog.Warn("swap-protection survivors non-viable; routing without it", "model", model)
		}
	}
	return r.selectFromPool(ctx, pool, model, now)
}

// selectFromPool applies the prefer-loaded partition (dominant, fail-open) and then the
// bounded-tiebreak score to choose a candidate from pool. Returns ok=false if no
// candidate in pool is viable (Score gate).
func (r *Resolver) selectFromPool(ctx context.Context, pool []MappingCandidate, model string, now time.Time) (MappingCandidate, bool, error) {
	// Prefer already-loaded (DOMINANT over the score): if any candidate in the pool has
	// the requested model resident on its server, prefer that partition — avoiding a cold
	// load/swap. When loaded state is unknown (nil checker / no data), no candidate is
	// "loaded", so the pool is unchanged (the P4a no-op invariant). This is the "avoid
	// swaps" precedence; argmaxByScore then ranks within the chosen partition. The
	// preference is dominant only when the loaded partition yields a viable pick — like
	// the context-fit filter above it, it FAILS OPEN: if none of the loaded candidates is
	// Score-viable it spills over to the full pool rather than refuse a request an
	// unloaded server could serve.
	if r.loaded != nil {
		loadedPool := make([]MappingCandidate, 0, len(pool))
		for _, c := range pool {
			if modelLoadedOn(r.loaded, c) {
				loadedPool = append(loadedPool, c)
			}
		}
		if len(loadedPool) > 0 {
			if sel, ok, err := r.argmaxByScore(ctx, loadedPool, model, now); err != nil || ok {
				return sel, ok, err
			}
		}
	}
	return r.argmaxByScore(ctx, pool, model, now)
}

// swapProtected reports whether routing to candidate c would evict a live model: c's
// server is actively serving (in flight, or a completion within swapProtectWindow) AND
// c is not already loaded for the requested model on that server (a loaded candidate is
// no swap). The r.loaded != nil guard also prevents a nil-checker panic in modelLoadedOn.
func (r *Resolver) swapProtected(c MappingCandidate, now time.Time) bool {
	if r.loaded != nil && modelLoadedOn(r.loaded, c) {
		return false
	}
	inFlight, lastCompletedAt := r.activity.ServerActivity(c.Server.ID)
	if inFlight > 0 {
		return true
	}
	if r.swapProtectWindow > 0 && !lastCompletedAt.IsZero() && now.Sub(lastCompletedAt) < r.swapProtectWindow {
		return true
	}
	return false
}

// modelLoadedOn reports whether the candidate's upstream model is currently loaded on
// its server/application, per the loaded-model checker.
func modelLoadedOn(checker LoadedModelChecker, c MappingCandidate) bool {
	for _, name := range checker.LoadedAppModels(c.Application.ID, c.Server.ID) {
		if name == c.Mapping.AppModelName {
			return true
		}
	}
	return false
}

func (r *Resolver) argmaxByScore(ctx context.Context, pool []MappingCandidate, model string, now time.Time) (MappingCandidate, bool, error) {
	var selected MappingCandidate
	bestScore := 0.0
	found := false
	for _, candidate := range pool {
		telemetry, ok, err := r.store.TelemetryByServer(ctx, candidate.Server.ID)
		if err != nil {
			return MappingCandidate{}, false, fmt.Errorf("load server telemetry: %w", err)
		}
		k := telemetry.ActiveRequests
		if r.activity != nil {
			inflight, _ := r.activity.ServerActivity(candidate.Server.ID)
			k = inflight
		}
		route := scoringRoute(candidate, telemetry, ok, k, model)
		score, ok := Score(route, model, now)
		if !ok {
			continue
		}
		if !found || score > bestScore {
			selected = candidate
			bestScore = score
			found = true
		}
	}
	return selected, found, nil
}

// candidateEffectiveGenTPS is a candidate's load-aware effective generation speed,
// assembled exactly as argmaxByScore assembles its scoring route so the floor and
// the ordering agree with the scorer. An unmeasured mapping yields 0.
func (r *Resolver) candidateEffectiveGenTPS(ctx context.Context, c MappingCandidate, model string) (float64, error) {
	telemetry, ok, err := r.store.TelemetryByServer(ctx, c.Server.ID)
	if err != nil {
		return 0, fmt.Errorf("load server telemetry: %w", err)
	}
	k := telemetry.ActiveRequests
	if r.activity != nil {
		inflight, _ := r.activity.ServerActivity(c.Server.ID)
		k = inflight
	}
	return effectiveGenTPS(scoringRoute(c, telemetry, ok, k, model)), nil
}

func (r *Resolver) activeMappingForApplication(ctx context.Context, applicationID string, gatewayModel string) (ModelMapping, bool, error) {
	mappings, err := r.store.MappingsByApplication(ctx, applicationID)
	if err != nil {
		return ModelMapping{}, false, fmt.Errorf("load application mappings: %w", err)
	}
	for _, mapping := range mappings {
		if mapping.Status == ServerStatusActive && mapping.GatewayModelName == gatewayModel {
			return mapping, true, nil
		}
	}
	return ModelMapping{}, false, nil
}

func serverSelectable(server AIServer) bool {
	return server.Status == ServerStatusActive && server.HealthStatus != HealthUnhealthy
}

func targetFrom(server AIServer, app Application, mapping ModelMapping, apiFlavor string) Target {
	return Target{
		RouteID:              mapping.ID,
		ServerID:             server.ID,
		Provider:             app.Type,
		Endpoint:             ApplicationEndpoint(server, app),
		Model:                mapping.GatewayModelName,
		ProviderModel:        mapping.AppModelName,
		Timeout:              time.Duration(app.TimeoutMS) * time.Millisecond,
		APIFlavor:            apiFlavor,
		APIToken:             app.APIToken,
		APITokenHeader:       app.APITokenHeader,
		NativeResponses:      app.NativeResponses,
		NativeMessages:       app.NativeMessages,
		OpportunisticMetrics: app.OpportunisticMetricsEnabled,
	}
}

func affinityID(key AffinityKey) string {
	sum := sha256.Sum256([]byte(key.APITokenID + "\x00" + key.Model + "\x00" + key.APIFlavor + "\x00" + key.SessionID))
	return "aff_" + hex.EncodeToString(sum[:12])
}

// memberStatus classifies a group member's availability for the priority walk.
// memberNoMapping (no live mapping at all) is kept distinct from memberUnavailable
// (has live mappings but all gated) so resolveGroup can pick ErrNoModelRoute vs
// ErrNoHealthyHost per §3g.
type memberStatus int

const (
	memberNoMapping   memberStatus = iota // no live mapping at all (unknown-model material)
	memberUnavailable                     // has live mapping(s) but all gated / non-viable
	memberOK                              // a candidate was selected (sel valid)
	memberAtCapacity                      // every candidate is at its effective cap (queue material)
)

// modeClimbUp is the climb_up value of GroupPolicy.FailoverMode.
const modeClimbUp = "climb_up"

// eligibleCandidates resolves one group member's candidates and applies every
// eligibility filter the group's policy imposes: the store read, the provisioning gate,
// the speed floor, then the loaded-only filter. Both the priority walk (selectMember)
// and the speed ordering go through it, so they share ONE definition of eligibility
// instead of two that can drift — the order can never rank a member the walk would
// refuse.
//
// live reports whether the member had ANY live, provisioned mapping BEFORE the policy
// filters ran. It is what keeps the two distinguishable "empty" outcomes apart:
//
//   - no candidates, live == false: the member has no live mapping at all — unknown-model
//     material (memberNoMapping, mapping to ErrNoModelRoute).
//   - no candidates, live == true: the member is real and otherwise routable, but every
//     candidate is gated by the floor or the loaded-only filter (memberUnavailable,
//     mapping to ErrNoHealthyHost) — gated by policy, exactly like a down/busy/non-viable
//     candidate, not "unknown".
//
// Conflating the two would misreport a live-but-gated member as an unknown model. The
// provisioning gate deliberately runs BEFORE live is taken (mirroring the main Resolve
// path): a member whose only live mappings sit on servers the principal is not
// provisioned for is live == false — the same no-leak posture the codebase uses elsewhere
// (404 rather than a distinguishable "exists but forbidden" signal).
//
// The speed floor (GroupPolicy.MinTokensPerSecond) is applied CANDIDATE by candidate: a
// candidate whose effective generation speed falls short is dropped before selectCandidate
// ever sees it, so a member is never served on a too-slow candidate just because its fast
// one is busy. A zero floor disables that pass entirely (the no-op invariant — no extra
// store read, cands unchanged), and likewise a false LoadedOnly. The loaded-only filter is
// additionally a no-op when r.loaded is nil: unknown loaded state excludes nothing, so a
// group without a wired checker never becomes a dead end from that filter alone.
func (r *Resolver) eligibleCandidates(ctx context.Context, token auth.Token, name, apiFlavor string, policy GroupPolicy) (cands []MappingCandidate, live bool, err error) {
	cands, err = r.store.ActiveMappingsForModel(ctx, name, apiFlavor)
	if err != nil {
		return nil, false, fmt.Errorf("resolve member mappings: %w", err)
	}
	cands, err = r.filterProvisioned(ctx, token, cands)
	if err != nil {
		return nil, false, err
	}
	if len(cands) == 0 {
		return nil, false, nil
	}
	if policy.MinTokensPerSecond > 0 {
		kept := make([]MappingCandidate, 0, len(cands))
		for _, c := range cands {
			tps, tErr := r.candidateEffectiveGenTPS(ctx, c, name)
			if tErr != nil {
				return nil, true, tErr
			}
			if tps >= policy.MinTokensPerSecond {
				kept = append(kept, c)
			}
		}
		cands = kept
	}
	if policy.LoadedOnly && r.loaded != nil {
		kept := make([]MappingCandidate, 0, len(cands))
		for _, c := range cands {
			if modelLoadedOn(r.loaded, c) {
				kept = append(kept, c)
			}
		}
		cands = kept
	}
	return cands, true, nil
}

// selectMember resolves one group member's eligible candidates and reports its status.
// For a memberAtCapacity result it also returns the member's distinct candidate server
// ids and the MAX admission_queue_timeout_seconds across those candidates (queue
// material, mirroring the main Resolve admission-param computation). memberAtCapacity is
// only possible when an AdmissionController is wired (selectCandidate returns
// errAllAtCapacity only then); otherwise an all-at-cap member fails open inside
// selectCandidate and comes back memberOK, so a group without a wired admission
// controller never queues.
//
// Eligibility (and the memberNoMapping/memberUnavailable distinction it drives) lives in
// eligibleCandidates; this function only turns that outcome into a status and selects.
func (r *Resolver) selectMember(ctx context.Context, token auth.Token, name, apiFlavor string, req inference.Request, now time.Time, policy GroupPolicy) (MappingCandidate, memberStatus, []string, int, error) {
	cands, live, err := r.eligibleCandidates(ctx, token, name, apiFlavor, policy)
	if err != nil {
		return MappingCandidate{}, memberUnavailable, nil, 0, err
	}
	if len(cands) == 0 {
		if !live {
			return MappingCandidate{}, memberNoMapping, nil, 0, nil // no live mapping at all
		}
		return MappingCandidate{}, memberUnavailable, nil, 0, nil // live, but every candidate gated
	}
	selected, ok, serr := r.selectCandidate(ctx, cands, req, now)
	if errors.Is(serr, errAllAtCapacity) {
		return MappingCandidate{}, memberAtCapacity, distinctServerIDs(cands), maxQueueTimeoutSecs(cands), nil
	}
	if serr != nil {
		return MappingCandidate{}, memberUnavailable, nil, 0, serr
	}
	if !ok {
		return MappingCandidate{}, memberUnavailable, nil, 0, nil
	}
	return selected, memberOK, nil, 0, nil
}

// orderMembersBySpeed returns members re-sorted by their fastest ELIGIBLE candidate's
// effective generation speed, descending. Eligibility is eligibleCandidates — the very
// filters the walk applies — so the order can never rank a member the walk would refuse,
// and a member scores on the candidate it would actually be served on rather than on one
// the policy has already excluded. The metric is the load-aware effective speed, never the
// raw stored gen_tokens_per_second, so it agrees with the scorer and the speed floor.
//
// A member with no measurement, and equally one whose candidates are all gated, scores 0
// and therefore sorts LAST; SliceStable compares speed only, so ties keep the manual order
// and a group with no measurements anywhere behaves exactly as it did before speed ordering
// existed.
//
// Cost: this reads EVERY member's mappings (and their telemetry) on every request, where a
// priority-ordered walk stops at the first available member. That is the price of the
// ordering, and only groups that opt into it pay it.
func (r *Resolver) orderMembersBySpeed(ctx context.Context, token auth.Token, members []GroupMember, apiFlavor string, policy GroupPolicy) ([]GroupMember, error) {
	speed := make(map[string]float64, len(members))
	for _, m := range members {
		if _, done := speed[m.MemberGatewayName]; done {
			continue // a duplicated member name is scored once
		}
		best := 0.0
		cands, _, err := r.eligibleCandidates(ctx, token, m.MemberGatewayName, apiFlavor, policy)
		if err != nil {
			return nil, err
		}
		for _, c := range cands {
			tps, tErr := r.candidateEffectiveGenTPS(ctx, c, m.MemberGatewayName)
			if tErr != nil {
				return nil, tErr
			}
			if tps > best {
				best = tps
			}
		}
		speed[m.MemberGatewayName] = best
	}
	out := append([]GroupMember(nil), members...) // never reorder the caller's slice
	sort.SliceStable(out, func(i, j int) bool {
		return speed[out[i].MemberGatewayName] > speed[out[j].MemberGatewayName]
	})
	return out, nil
}

// speedMarginMet reports whether the candidate is enough faster than the pinned one to
// justify moving a session: strictly MORE than the configured margin above it. A margin of
// 0 is a legitimate value meaning "no margin required" — any strictly faster candidate then
// wins; only a negative margin is clamped. An unmeasured pin (<= 0) always yields true —
// anything measurable beats unknown.
func (r *Resolver) speedMarginMet(ctx context.Context, best, pinned MappingCandidate, bestModel, pinnedModel string, policy GroupPolicy) (bool, error) {
	pinnedTPS, err := r.candidateEffectiveGenTPS(ctx, pinned, pinnedModel)
	if err != nil {
		return false, err
	}
	if pinnedTPS <= 0 {
		return true, nil
	}
	bestTPS, err := r.candidateEffectiveGenTPS(ctx, best, bestModel)
	if err != nil {
		return false, err
	}
	margin := policy.ClimbSpeedMarginPercent
	if margin < 0 {
		margin = 0
	}
	return bestTPS > pinnedTPS*(1+float64(margin)/100), nil
}

// firstAvailable walks members top-down — in whatever order the slice arrives in, which is
// the manual priority order or the speed-sorted one — and returns the first memberOK member
// (its name + selection). If none is OK it returns the accumulated at-capacity server ids
// across the skipped members and the MAX queue timeout over those members (for queuing on
// the union), plus anyLive = whether ANY member had at
// least one live mapping (a member with live-but-gated candidates or at-capacity
// counts; a member with no mapping does not). anyLive lets resolveGroup pick
// ErrNoModelRoute (no live mapping anywhere) vs ErrNoHealthyHost (§3g). A hard
// store/selection error is propagated.
func (r *Resolver) firstAvailable(ctx context.Context, token auth.Token, members []GroupMember, apiFlavor string, req inference.Request, now time.Time, policy GroupPolicy) (string, MappingCandidate, []string, int, bool, error) {
	var atCap []string
	queueTimeoutSecs := 0
	anyLive := false
	for _, m := range members {
		sel, status, ids, secs, err := r.selectMember(ctx, token, m.MemberGatewayName, apiFlavor, req, now, policy)
		if err != nil {
			return "", MappingCandidate{}, nil, 0, false, err
		}
		switch status {
		case memberOK:
			return m.MemberGatewayName, sel, atCap, queueTimeoutSecs, true, nil
		case memberAtCapacity:
			anyLive = true
			atCap = append(atCap, ids...)
			if secs > queueTimeoutSecs {
				queueTimeoutSecs = secs
			}
		case memberUnavailable:
			anyLive = true // has a live mapping, just gated / non-viable
		}
		// memberNoMapping contributes nothing (no live mapping).
	}
	return "", MappingCandidate{}, atCap, queueTimeoutSecs, anyLive, nil
}

// groupPin returns the concrete member a group session is pinned to, or "" when there
// is no usable pin. A pin is usable only when it exists, is unexpired, carries a
// non-empty ResolvedModel, and that member is still part of the group. A stale/invalid
// (but present) pin is best-effort deleted; a transient store error yields "" without a
// delete (never tears down a good pin on a blip).
func (r *Resolver) groupPin(ctx context.Context, key AffinityKey, members []GroupMember, now time.Time) string {
	aff, ok, err := r.store.Affinity(ctx, key)
	if err != nil || !ok {
		return ""
	}
	if aff.ExpiresAt.After(now) && aff.ResolvedModel != "" && memberNameExists(members, aff.ResolvedModel) {
		return aff.ResolvedModel
	}
	_ = r.store.DeleteAffinity(ctx, key)
	return ""
}

// upsertGroupPin stores/refreshes the group affinity pinned to the resolved member.
// It NEVER writes an empty ResolvedModel (that would be an unusable row that also leaks
// a capacity reservation) and only pins at all when the token carries an id and the
// serving application enables affinity. Mirrors the main Resolve pin: on an UpsertAffinity
// failure it returns the error (a pin that could not be stored is a hard failure, exactly
// as the single-model path treats it).
func (r *Resolver) upsertGroupPin(ctx context.Context, token auth.Token, key AffinityKey, name string, sel MappingCandidate, now time.Time) error {
	if token.ID == "" || name == "" || sel.Application.AffinityTTLSeconds <= 0 {
		return nil
	}
	id := affinityID(key)
	if err := r.store.UpsertAffinity(ctx, RouteAffinity{
		ID:            id,
		APITokenID:    token.ID,
		UserID:        token.UserID,
		Model:         key.Model, // the group name
		ResolvedModel: name,      // the concrete served member (never empty here)
		APIFlavor:     key.APIFlavor,
		SessionID:     key.SessionID,
		ApplicationID: sel.Application.ID,
		ServerID:      sel.Server.ID,
		ExpiresAt:     now.Add(time.Duration(sel.Application.AffinityTTLSeconds) * time.Second),
		LastUsedAt:    now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		return fmt.Errorf("store group affinity: %w", err)
	}
	r.reservation.touch(sel.Server.ID, id, now)
	return nil
}

// resolveGroup runs resolveGroupOnce under progressively relaxed eligibility filters.
// The order encodes the spec's precedence: the loaded-only filter is dropped before the
// speed floor is, and the floor is dropped only when min_speed_fallback says so. Each
// relaxation is CUMULATIVE — built by mutating one carried-forward `relaxed` value, not by
// rebuilding from the original `policy` — so the ladder is monotone: once a filter is
// dropped it STAYS dropped in every later attempt. Rebuilding from `policy` each time would
// let a later attempt resurrect an earlier-dropped filter (e.g. loaded_only reappearing once
// the floor is also dropped), silently breaking both settings' documented promise that
// nothing eligible => the restriction is dropped, never a dead end. The first attempt that
// resolves wins; the LAST attempt's error is the one returned, so the caller still sees
// ErrNoModelRoute vs ErrNoHealthyHost from a real walk.
func (r *Resolver) resolveGroup(ctx context.Context, token auth.Token, req inference.Request, key AffinityKey, apiFlavor string, members []GroupMember, policy GroupPolicy, now time.Time) (Target, error) {
	attempts := []GroupPolicy{policy}
	relaxed := policy
	if policy.LoadedOnly {
		relaxed.LoadedOnly = false
		attempts = append(attempts, relaxed)
	}
	if policy.MinTokensPerSecond > 0 && policy.MinSpeedFallback == MinSpeedFallbackIgnore {
		relaxed.MinTokensPerSecond = 0
		attempts = append(attempts, relaxed)
	}
	var lastErr error
	for _, attempt := range attempts {
		target, err := r.resolveGroupOnce(ctx, token, req, key, apiFlavor, members, attempt, now)
		if err == nil {
			return target, nil
		}
		if !errors.Is(err, ErrNoHealthyHost) && !errors.Is(err, ErrNoModelRoute) {
			return Target{}, err // a real failure (store error, admission timeout) — never retried
		}
		lastErr = err
	}
	return Target{}, lastErr
}

// resolveGroupOnce expands a group into its ordered members and serves the first
// available one (failover) under ONE fixed policy, honoring a sticky pin. Two failover
// modes (§3f):
//
//   - sticky: the pin is preferred while it is available; if it is down or at-capacity the
//     turn falls through walkPriority and re-pins, but never climbs back to a
//     higher-priority member.
//   - climb_up: like sticky, but when the pin IS available AND a higher-priority member is
//     also available, it either switches to the better member NOW (when that member is
//     already loaded — a free climb, no cold-start stall) or fires a best-effort background
//     r.warmer.Warm and keeps serving the pin THIS turn (a later turn climbs once the better
//     member is loaded). The asymmetry: climbing UP happens only when the target is loaded
//     (you already have a working pin — never take a cold start to climb); falling DOWN when
//     the pin is unavailable is immediate even to a cold member (you have no working model).
//     So climb_up differs from sticky only when the pin is available AND a higher-priority
//     member is available. A nil r.warmer makes climb_up purely passive (it only switches
//     once a higher-priority member is loaded by other traffic).
//
// Under member_order=speed the members are re-sorted by measured speed first, so
// everything above reads "faster" wherever it says "higher-priority", and a climb_up climb
// additionally has to clear the group's climb speed margin before it moves a session.
//
// Errors map per §3g: zero members OR no member with any live mapping -> ErrNoModelRoute
// (unknown model); members with live mappings but all gated (down/busy/non-viable/at-cap)
// -> ErrNoHealthyHost.
func (r *Resolver) resolveGroupOnce(ctx context.Context, token auth.Token, req inference.Request, key AffinityKey, apiFlavor string, members []GroupMember, policy GroupPolicy, now time.Time) (Target, error) {
	if len(members) == 0 {
		return Target{}, ErrNoModelRoute
	}

	// Speed order: re-sort the member slice, because everything downstream reads order
	// from it (firstAvailable walks it; memberIndex reads a member's rank from its
	// position), so a re-sort here makes the whole rest of the function — the pin check,
	// the climb comparison, the walk — mean "fastest" wherever it said "highest priority".
	// The pass is skipped entirely for the default priority order, so that path costs
	// exactly what it did before. Ordering happens per ATTEMPT, not once for the whole
	// relaxation ladder, because eligibility (and therefore a member's score) depends on
	// this attempt's filters: an attempt that has dropped loaded_only must rank members on
	// the candidates it can actually use.
	if policy.MemberOrder == MemberOrderSpeed {
		ordered, err := r.orderMembersBySpeed(ctx, token, members, apiFlavor, policy)
		if err != nil {
			return Target{}, err
		}
		members = ordered
	}

	// serve builds the target for a selected member and (re)pins the group affinity to it.
	serve := func(name string, sel MappingCandidate) (Target, error) {
		if err := r.upsertGroupPin(ctx, token, key, name, sel, now); err != nil {
			return Target{}, err
		}
		return targetFrom(sel.Server, sel.Application, sel.Mapping, apiFlavor), nil
	}

	// Check the pin's availability exactly once; the climb dance runs ONLY when the pin is
	// available. A pin that is down or at-capacity falls straight through to the walk (an
	// immediate fall-DOWN, even to a cold member — the intended asymmetry, no warm).
	if pinned := r.groupPin(ctx, key, members, now); pinned != "" {
		pinSel, pinStatus, _, _, err := r.selectMember(ctx, token, pinned, apiFlavor, req, now, policy)
		if err != nil {
			return Target{}, err
		}
		if pinStatus == memberOK {
			if policy.FailoverMode == modeClimbUp {
				// Is a better-ranked member available? The pin is memberOK, so firstAvailable
				// stops at-or-before it in the effective order → best index <= pin index (the
				// memberIndex guard below is belt-and-suspenders for that invariant).
				best, bestSel, _, _, _, ferr := r.firstAvailable(ctx, token, members, apiFlavor, req, now, policy)
				if ferr != nil {
					return Target{}, ferr
				}
				if best != "" && best != pinned && memberIndex(members, best) < memberIndex(members, pinned) {
					// Under speed ordering "better" means "faster", and a marginally faster
					// member is not worth moving a live session to: require the configured
					// margin first. This gate sits ON TOP of the free-climb rule below, never
					// replacing it — and it is consulted only for a speed-ordered group, so a
					// priority-ordered one climbs exactly as it always did.
					if policy.MemberOrder == MemberOrderSpeed {
						met, mErr := r.speedMarginMet(ctx, bestSel, pinSel, best, pinned, policy)
						if mErr != nil {
							return Target{}, mErr
						}
						if !met {
							// Not materially faster: keep the session where it is, and do not warm
							// a member we would refuse to climb to anyway.
							return serve(pinned, pinSel)
						}
					}
					if r.memberLoaded(bestSel) {
						return serve(best, bestSel) // CLIMB: the better member is already loaded
					}
					if r.warmer != nil && !policy.LoadedOnly {
						r.warmer.Warm(ctx, best) // load-ahead (non-blocking); keep serving the pin this turn
					}
					// Fall through to serve the pin below; a later turn climbs once best is loaded.
				}
			}
			return serve(pinned, pinSel)
		}
		// Pin down OR at-capacity: fall through to walkPriority + re-pin (no warm).
	}

	// Fresh request or a fallen-through pin: walk priorities; queue only if every member
	// is at capacity (§3c/§3h). The admission timeout is an absolute wall-clock deadline
	// established on the first queue so the queue's internal liveness re-check cannot re-arm
	// the full budget every iteration (mirrors the main Resolve deadline semantics).
	var deadline time.Time
	deadlineSet := false
	for {
		name, sel, atCap, queueTimeoutSecs, anyLive, err := r.firstAvailable(ctx, token, members, apiFlavor, req, now, policy)
		if err != nil {
			return Target{}, err
		}
		if name != "" {
			return serve(name, sel)
		}
		if len(atCap) > 0 && r.admission != nil {
			queueTimeout := time.Duration(queueTimeoutSecs) * time.Second
			if queueTimeout > 0 && !deadlineSet {
				deadline = now.Add(queueTimeout)
				deadlineSet = true
			}
			wait := time.Duration(0) // 0 => unbounded (ctx-only)
			// Once a finite deadline is established it is enforced on EVERY subsequent
			// wake — even if the current at-capacity union is now all-unbounded (e.g. the
			// sole bounded-timeout member went down mid-queue). A finite wait must never
			// be silently promoted back to unbounded.
			if deadlineSet {
				if wait = deadline.Sub(r.clock()); wait <= 0 {
					return Target{}, ErrAdmissionQueueTimeout
				}
			}
			if werr := r.admission.WaitForSlot(ctx, distinctStrings(atCap), wait); werr != nil {
				return Target{}, werr // ErrAdmissionQueueTimeout / ErrAdmissionQueueFull / ctx err
			}
			now = r.clock()
			continue
		}
		// No servable member and nothing to queue. §3g: if NO member has any live
		// mapping the group is effectively an unknown model (ErrNoModelRoute); if
		// members DO have live mappings but all are gated it is ErrNoHealthyHost.
		if !anyLive {
			return Target{}, ErrNoModelRoute
		}
		return Target{}, ErrNoHealthyHost
	}
}

// memberNameExists reports whether name is one of the group's member gateway names.
func memberNameExists(members []GroupMember, name string) bool {
	for _, m := range members {
		if m.MemberGatewayName == name {
			return true
		}
	}
	return false
}

// memberIndex returns the priority index of name among the group's members (0 = highest
// priority), or -1 when name is not a member. Used by the climb_up dance to confirm a
// candidate is strictly higher-priority than the current pin.
func memberIndex(members []GroupMember, name string) int {
	for i, m := range members {
		if m.MemberGatewayName == name {
			return i
		}
	}
	return -1
}

// memberLoaded reports whether the selected candidate's upstream model is currently loaded
// on its server (per the loaded-model checker). Nil-checker-safe: unknown loaded state is
// treated as NOT loaded, so climb_up never claims a cold member is loaded and thus never
// climbs into a cold start.
func (r *Resolver) memberLoaded(sel MappingCandidate) bool {
	return r.loaded != nil && modelLoadedOn(r.loaded, sel)
}

// distinctServerIDs returns the deduped server ids of a candidate slice (order-stable).
func distinctServerIDs(cands []MappingCandidate) []string {
	seen := make(map[string]struct{}, len(cands))
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		if _, dup := seen[c.Server.ID]; dup {
			continue
		}
		seen[c.Server.ID] = struct{}{}
		out = append(out, c.Server.ID)
	}
	return out
}

// distinctStrings dedupes a string slice (order-stable), used to build the queue union
// across at-capacity members (a server may serve more than one member).
func distinctStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// maxQueueTimeoutSecs returns the MAX admission_queue_timeout_seconds across a candidate
// slice's applications (0 = at least one app is willing to wait unboundedly).
func maxQueueTimeoutSecs(cands []MappingCandidate) int {
	maxSecs := 0
	for _, c := range cands {
		if c.Application.AdmissionQueueTimeoutSeconds > maxSecs {
			maxSecs = c.Application.AdmissionQueueTimeoutSeconds
		}
	}
	return maxSecs
}
