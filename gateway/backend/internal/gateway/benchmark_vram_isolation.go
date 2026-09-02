// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"sort"
	"time"
)

// The four refusals a VRAM run makes BEFORE it writes a single spec. Each is
// a state in which the isolation the run promises cannot be achieved by any
// gateway-side write, so refusing costs the operator one error message while
// proceeding costs them every model on the server plus a number that means
// nothing. All four are HTTP 409 at the trigger.
//
// They are stable API error codes: the portal switches on them to render an
// actionable message, each with its own German and English key.
const (
	// codeBenchmarkVRAMNotAgentManaged: the target's process is not
	// agent-managed, so it is not in the spec set the run enumerates, cannot
	// be force-stopped, and cannot be restarted by clearing an override.
	// AuthorizeBenchmarkScope gates OWNERSHIP, not application type, so this
	// really does reach the runner.
	codeBenchmarkVRAMNotAgentManaged = "benchmark.vram_not_agent_managed"
	msgBenchmarkVRAMNotAgentManaged  = "this model is not an agent-managed process, so it cannot be isolated for a VRAM measurement"

	// codeBenchmarkVRAMIsolationUnavailable: the gateway cannot reach the
	// agent's runtime driver at all -- file mode, or an agent that has not
	// declared the runtime_manager feature. Every write would return 200 and
	// stop nothing.
	codeBenchmarkVRAMIsolationUnavailable = "benchmark.vram_isolation_unavailable"

	// codeBenchmarkVRAMNoGPUSamples: the server's latest telemetry carries no
	// GPU sample, so there is nothing to difference and no per-process
	// measurer either. Nothing to measure, so nothing to drain for.
	codeBenchmarkVRAMNoGPUSamples = "benchmark.vram_no_gpu_samples"
	msgBenchmarkVRAMNoGPUSamples  = "this server reports no GPU, so there is no VRAM to measure"

	// codeBenchmarkVRAMIsolationBlocked: a spec already carries an
	// operator-owned override, or a spec OTHER than the target is pinned. The
	// message names the blocking spec.
	codeBenchmarkVRAMIsolationBlocked = "benchmark.vram_isolation_blocked"
)

// The two conditions that DEGRADE a run's confidence without making it
// pointless, reported on the result so the operator can weigh the number.
// Closed vocabulary, persisted inside vram_json, one i18n key each -- the same
// discipline as the inconclusive reasons.
const (
	// vramWarningNonManagedApplications: the server also hosts active
	// applications the agent does not manage (llama-swap coexisting with the
	// managed runtime is an explicit migration path). Refusing would not
	// improve isolation -- those processes are outside the agent's control
	// either way -- and would make the feature unusable on exactly those
	// deployments. A STATIC neighbour cancels out of the delta; a MOVING one
	// trips the stability gate; one serving the target model itself is caught
	// as already-resident -- WHERE THAT CHECK IS AVAILABLE AT ALL, see
	// vramWarningResidencyUnknown.
	vramWarningNonManagedApplications = "non_managed_applications"
	// vramWarningPostTransportAgent: no open agent WebSocket, so nothing this
	// run writes reaches the agent before its next runtime poll -- the drain
	// does not even begin until then and the server is held for correspondingly
	// longer. The run says so rather than refusing, which would cost the run
	// for the same result.
	vramWarningPostTransportAgent = "post_transport_agent"
	// vramWarningResidencyUnknown: the run could not check whether the target
	// model was ALREADY being served by something it had not stopped, so the
	// already_resident signal was unavailable rather than negative.
	//
	// That check is the loaded-models probe, and it needs an application-level
	// `loaded_models_path` -- operator-entered, with no default, and empty on
	// most agent-managed applications, whose child sits behind the agent's own
	// router. Without it modelResident answers "not resident" for a model that
	// may well be resident: the baseline then already contains the model and
	// the ~0 delta surfaces at the floor gate as below_floor, whose next
	// action ("the window missed the allocation, measure again when the server
	// is quiet") fails identically every time. This warning is what keeps the
	// operator from acting on that wrong reason. It also covers a probe that
	// ERRORED, for the same reason: an unanswered question is not a no.
	vramWarningResidencyUnknown = "residency_unknown"
)

// The VRAM run's bounds. EVERY ONE IS REASONED, NOT MEASURED -- vars (the
// coldLoadPollGap precedent) so tests shorten them, and each needs validating
// against a real fleet before it is treated as settled.
var (
	// vramIsolationBindDelay is how long the agent is given to have APPLIED
	// this run's overrides before any spec is recorded as isolated. It is the
	// agent's own runtime-poll interval (agent/agent.go's
	// runtimePollInterval), and it is TRANSPORT-INDEPENDENT on purpose.
	//
	// It used to be two values, picked by AgentStreams.hasConn: two seconds for
	// a WS-connected agent, the poll interval otherwise. That gave the WS push
	// the standing of a delivery, and it has none. There is no acknowledgement
	// anywhere in this protocol (§3.5), the push runs in a detached goroutine
	// that returns silently when the derive or the marshal fails, and
	// NotifyRuntimeConfig sends to zero connections when the socket closed
	// after the probe or drops the frame with a slog.Debug when a send queue
	// is full -- in each case the override binds on the next poll anyway, while
	// the run had already confirmed the fleet and reported Isolated: true. The
	// probe was also taken BEFORE the drain wrote anything, so even a truthful
	// answer said nothing about the transport at write time.
	//
	// The poll is the one delivery mechanism that is guaranteed: the agent
	// re-fetches the whole document on every poll and on every reconnect, so
	// one interval measured from the write always covers a full cycle. An open
	// WebSocket makes the push likely to arrive sooner, and NOTHING here rests
	// on it; what it still decides is the operator-facing warning
	// (vramWarningPostTransportAgent), because a drain that does not even begin
	// for a minute is worth telling them about.
	vramIsolationBindDelay = 60 * time.Second
	// vramIsolationDrainBound is how long a spec that HAD a live process is
	// given to drain, on top of the binding delay.
	vramIsolationDrainBound = 60 * time.Second
	// vramRestoreTimeout bounds the deferred restore. It runs on a context
	// that is NOT the run's own, so it needs its own deadline.
	vramRestoreTimeout = 30 * time.Second
	// vramHistoryWriteTimeout bounds the best-effort history-row write, which
	// runs on the same not-the-run's context as the restore for the same
	// reason: a cancelled run must still record what it did.
	vramHistoryWriteTimeout = 5 * time.Second
)

// vramRefusal is a precondition refusal carrying a stable API error code and
// an operator-facing message that names the blocking condition (or spec). The
// endpoint answers 409 with both.
type vramRefusal struct {
	code string
	msg  string
}

func (e *vramRefusal) Error() string { return e.code + ": " + e.msg }

// vramRunPlanned is everything the run needs that was decided (and
// authorized) before it started: which specs make up the fleet to drain,
// which of them is the target, how long the transport takes to bind an
// override, and the conditions that degrade confidence.
type vramRunPlanned struct {
	targetSpecID string
	// specIDs is every ENABLED spec of the server's agent-managed
	// application, ascending, THE TARGET AMONG THEM.
	specIDs   []string
	bindDelay time.Duration
	warnings  []string
	// baseline is the latest GPU-bearing sample the preconditions read. It
	// decides which cards are watched and supplies each card's fingerprint;
	// the actual baseline numbers come from a fresh stable window.
	baseline routing.TelemetrySample
	// unifiedMemory marks a host whose per-GPU figures are unified SYSTEM
	// memory rather than dedicated VRAM.
	unifiedMemory bool
}

// The gateway-side mirror of the agent's closed runtime-state set
// (server-agent/internal/runtime/types.go's State constants). A local mirror,
// not an import: the two Go modules share no code, and the whole point of a
// closed set is that this side states independently what it will accept --
// the same posture as runtimeReportParseErrorCodes.
//
// The split is by ONE question: does this spec have a process to stop?
var (
	// vramStatesWithProcess: a spec here HAS a live process, so a
	// force_stopped write produces a real drain and therefore a real
	// TRANSITION to wait for.
	vramStatesWithProcess = map[string]bool{
		"running": true, "starting": true, "draining": true,
	}
	// vramStatesNoProcess: a spec here has NO live process, so a
	// force_stopped write does nothing at all -- no state change, no frame.
	// It is already isolated and must be CONFIRMED, never awaited: waiting
	// for a transition that will never arrive is what turns an
	// already-quiet server into an isolation timeout.
	vramStatesNoProcess = map[string]bool{
		"stopped": true, "pending_vram_unknown": true, "not_permitted": true,
		"crashed": true, "start_failed": true, "backoff": true,
	}
)

// vramStateNoProcess reports whether state is one this gateway RECOGNIZES as
// having no live process. An unrecognized state (a future agent build) is
// false, which is the fail-closed direction: the run reports an isolation
// timeout rather than claiming an isolation it cannot justify.
func vramStateNoProcess(state string) bool { return vramStatesNoProcess[state] }

// vramStateBySpec indexes one runtime-status frame -- or the registry's
// snapshot, which carries the same rows -- by spec id, so the two isolation
// checks read a spec's state by lookup instead of by scanning.
//
// A repeated spec id keeps the LAST row, which is the registry's own
// last-write-wins view of a spec: a frame is a snapshot of distinct specs, so
// a duplicate is either an update or malformed, and in both readings the later
// row is the one to believe.
func vramStateBySpec(statuses []RuntimeStatusDTO) map[string]string {
	out := make(map[string]string, len(statuses))
	for _, status := range statuses {
		out[status.SpecID] = status.State
	}
	return out
}

// vramRunPlan runs every refusal a VRAM run makes before it writes anything,
// and returns the plan the run then executes. It has exactly ONE call site,
// the trigger endpoint, so each refusal is a 409 the operator sees.
//
// It is deliberately NOT re-run at the top of the run body, and the
// difference matters to a reader: two of these gates are volatile facts an
// agent report can flip between the trigger and the run (IsFileMode and the
// declared feature set are both written by telemetry ingest), and runVRAMProbe
// re-checks exactly those two through vramIsolationUnavailable rather than
// re-planning. The enumeration-dependent refusals below -- a pre-existing
// override anywhere, a pinned sibling, a target with no enabled spec -- are
// therefore evaluated ONCE, before the reservation. Nothing re-evaluates them
// afterwards, which is why the run's defence against an override that appears
// mid-run is the restore's compare-and-set rather than a second refusal.
//
// The order is deliberate: the cheapest, most structural refusal first
// (the target is not agent-managed at all), then the two reachability gates,
// then the "nothing to measure" gate, and only then the enumeration-dependent
// isolation refusals -- so an operator sees the most actionable message
// rather than whichever condition happens to be checked first.
func (s *Server) vramRunPlan(ctx context.Context, tgt benchmarkTarget) (vramRunPlanned, error) {
	serverID := tgt.server.ID

	// P1: the target's process must be agent-managed, or "the target among
	// them" is simply false -- it is not in the spec set enumerated below, a
	// force_stopped write could not stop it, and the full-document writer
	// would refuse the write anyway, AFTER the siblings were already drained.
	if tgt.app.Type != routing.ProviderServerAgent {
		return vramRunPlanned{}, &vramRefusal{code: codeBenchmarkVRAMNotAgentManaged, msg: msgBenchmarkVRAMNotAgentManaged}
	}

	// P2/P3: can a gateway-side write reach the agent's runtime driver at
	// all? Both are the gates PushRuntimeConfig itself fail-closes on.
	if reason, unavailable := s.vramIsolationUnavailable(ctx, serverID); unavailable {
		return vramRunPlanned{}, &vramRefusal{code: codeBenchmarkVRAMIsolationUnavailable, msg: reason}
	}

	// P4: a host with no GPU collector emits no GPU sample at all, so there
	// is nothing to difference -- and no per-process measurer either, since
	// that needs a GPU too. Read from the live per-GPU ring, which is what
	// the delta itself reads: a gateway that has just restarted therefore
	// refuses until the next sample arrives (at most ~1 s), which is the safe
	// direction.
	baseline, ok := s.ServerPerf.latest(serverID)
	if !ok || len(baseline.GPUs) == 0 {
		return vramRunPlanned{}, &vramRefusal{code: codeBenchmarkVRAMNoGPUSamples, msg: msgBenchmarkVRAMNoGPUSamples}
	}

	// D2.1/D2.2: enumerate every ENABLED spec of the target's own agent-managed
	// application, refusing on either blocking condition. At most one
	// server_agent application exists per server (portal enforcement plus a
	// partial unique index), and P1 established that tgt.app IS it, so this one
	// read covers the whole server.
	specs, err := s.Routes.RuntimeSpecsByApplication(ctx, tgt.app.ID)
	if err != nil {
		return vramRunPlanned{}, err
	}
	specIDs, targetSpecID, err := vramEnumerateFleet(specs, tgt.mapping.ID)
	if err != nil {
		return vramRunPlanned{}, err
	}
	planned := vramRunPlanned{baseline: baseline, specIDs: specIDs, targetSpecID: targetSpecID}
	// P1 again, by the other route: an agent-managed application whose target
	// mapping has no ENABLED spec has no agent-managed process to measure --
	// the agent's router has nothing to route to, so the load would fail
	// after the fleet was already drained.
	if planned.targetSpecID == "" {
		return vramRunPlanned{}, &vramRefusal{code: codeBenchmarkVRAMNotAgentManaged, msg: msgBenchmarkVRAMNotAgentManaged}
	}

	// P5: a declared GPU index this host does not report can never hold still,
	// so every stability window would burn its bound and the run would report
	// baseline_unstable -- after draining the fleet, and with a next action
	// that can never work. See codeBenchmarkVRAMDeclaredGPUMissing.
	if err := s.vramDeclaredGPUMissing(ctx, planned.targetSpecID, baseline); err != nil {
		return vramRunPlanned{}, err
	}

	// The binding delay is the agent's guaranteed poll interval, WHATEVER the
	// transport looks like -- an unacknowledged push cannot shorten it (see
	// vramIsolationBindDelay). Q10's answer therefore reduces to its warning
	// half: an agent with no open WebSocket is told about, never refused.
	planned.bindDelay = vramIsolationBindDelay
	planned.warnings = s.vramPlanWarnings(ctx, serverID, tgt.app.ID)
	// The Apple label. The gateway is hardware-agnostic everywhere else, but
	// a figure read from unified SYSTEM memory reported as VRAM is a wrong
	// number rather than a vague one, and the reported OS is the only thing
	// that distinguishes it.
	if summary, ok, err := s.Routes.TelemetryByServer(ctx, serverID); err == nil && ok {
		planned.unifiedMemory = summary.OS == "darwin"
	}
	return planned, nil
}

// vramEnumerateFleet is D2.1 and D2.2 together: which ENABLED specs make up
// the fleet this run has to drain, which of them is the target, and the two
// conditions under which no gateway-side write can produce the isolation the
// run promises.
//
// A DISABLED spec is nothing the agent ever runs, so it is no part of the
// fleet to drain. The result is sorted so the drain, the restore and the
// isolation evidence all name the fleet in one order.
//
// It takes the specs already read rather than reading them itself, so
// vramRunPlan keeps the whole store-error path -- a refusal here is always a
// decision about the fleet, never an I/O failure.
func vramEnumerateFleet(specs []routing.RuntimeSpec, targetMappingID string) (specIDs []string, targetSpecID string, err error) {
	for _, spec := range specs {
		if !spec.Enabled {
			continue
		}
		specIDs = append(specIDs, spec.ID)
		if spec.MappingID == targetMappingID {
			targetSpecID = spec.ID
		}
		// D2.2, first refusal: an operator-owned override anywhere -- THE
		// TARGET'S OWN INCLUDED -- is what makes the restore unambiguous.
		// Refusing here means the state to restore is always exactly "", so
		// the run never has to reconstruct what an override was; after a
		// gateway restart it could not know.
		if spec.AdminState != "" {
			return nil, "", &vramRefusal{
				code: codeBenchmarkVRAMIsolationBlocked,
				msg:  "spec " + spec.ID + " already carries the admin override " + spec.AdminState + "; clear it first",
			}
		}
		// D2.2, second refusal: a pinned SIBLING. Not because a pinned spec
		// cannot be drained -- force_stopped outranks pinned -- but because
		// pinned is an operator's standing instruction that this model stays
		// up, and silently breaking it for a benchmark is a worse surprise
		// than refusing and naming it. The TARGET may be pinned: stopping the
		// target is the point of the run.
		if spec.Pinned && spec.MappingID != targetMappingID {
			return nil, "", &vramRefusal{
				code: codeBenchmarkVRAMIsolationBlocked,
				msg:  "spec " + spec.ID + " is pinned to stay running; unpin it first",
			}
		}
	}
	sort.Strings(specIDs)
	return specIDs, targetSpecID, nil
}

// vramPlanWarnings collects the conditions that DEGRADE a run's confidence
// without making it pointless, in the fixed order the result reports them.
//
// They are gathered together because they share one posture -- neither may
// refuse -- and separately from the refusals above because that posture is the
// whole distinction: a refusal costs the operator a message, a warning costs
// them nothing and buys them the context to weigh the number they get. Neither
// condition is read again during the run; the run carries the strings.
func (s *Server) vramPlanWarnings(ctx context.Context, serverID, agentAppID string) []string {
	var warnings []string
	// Q10's warning half: an agent with no open WebSocket is told about, never
	// refused -- the drain does not even begin until its next runtime poll.
	if !s.AgentStreams.hasConn(serverID) {
		warnings = append(warnings, vramWarningPostTransportAgent)
	}
	// Q5: warn on a non-managed neighbour, do not refuse.
	if s.vramHasNonManagedApplications(ctx, serverID, agentAppID) {
		warnings = append(warnings, vramWarningNonManagedApplications)
	}
	return warnings
}

// vramIsolationUnavailable reports whether a gateway-side runtime write can
// reach this server's agent at all, and why not.
//
// File mode is checked TWICE, on purpose. The volatile flag is set from the
// agent's own upward report on ingest, so it reads false for a file-mode
// server until the first report arrives after a gateway restart -- exactly
// the window in which a run would drain a fleet whose agent never reads the
// document. The durable cross-check is the persisted report. A stale `file`
// report on a server since switched back to gateway mode produces a false
// refusal, which is the safe direction and is one report cycle away from
// fixing itself.
//
// Neither gate is a guarantee the document ARRIVED -- there is no
// acknowledgement anywhere. They are guarantees the gateway TRIED. What turns
// "tried" into evidence is the isolation wait plus the Isolated contract.
func (s *Server) vramIsolationUnavailable(ctx context.Context, serverID string) (string, bool) {
	if s.RuntimeStatus.IsFileMode(serverID) || s.vramReportedFileMode(ctx, serverID) {
		return "this server's agent is configured from a local file, so a gateway-side isolation would change nothing", true
	}
	if !s.AgentFeatures.Has(serverID, "runtime_manager") {
		return "this server's agent has not declared the managed-runtime capability, so it applies no runtime configuration", true
	}
	return "", false
}

// vramReportedFileMode reads the DURABLE half of the file-mode check: the
// persisted upward report's config source. A missing report, a read failure
// or an unparseable payload all read as "not file mode" -- the same
// fail-open-on-this-gate posture as IsFileMode's own default, because the
// volatile flag is the primary signal and this is the after-a-restart
// backstop, not a second opinion that may withhold a run on its own.
func (s *Server) vramReportedFileMode(ctx context.Context, serverID string) bool {
	if s.Routes == nil {
		return false
	}
	report, ok, err := s.Routes.ServerRuntimeReportByServer(ctx, serverID)
	if err != nil || !ok {
		return false
	}
	var decoded struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal([]byte(report.ReportJSON), &decoded); err != nil {
		return false
	}
	return decoded.Source == "file"
}

// vramHasNonManagedApplications reports whether the server hosts an ACTIVE
// application other than its agent-managed one. Those processes are outside
// the agent's control, so the run cannot drain them -- it warns (Q5) and
// leaves the honesty gates to catch any contamination they cause.
func (s *Server) vramHasNonManagedApplications(ctx context.Context, serverID, agentAppID string) bool {
	apps, err := s.Routes.ApplicationsByServer(ctx, serverID)
	if err != nil {
		return false
	}
	for _, app := range apps {
		if app.ID == agentAppID || app.Status != routing.ServerStatusActive {
			continue
		}
		return true
	}
	return false
}

// vramLiveProcessBySpec reads which specs had a LIVE PROCESS according to the
// most recent runtime-status snapshot -- the classification the isolation wait
// uses to decide which EVIDENCE label a spec earns.
//
// It is at most one sample old, and that is fine, because the label is only a
// hint: the actual frame's state does the work either way. A spec that
// exited on its own just before the write is classified as live, and the
// stopped frame that follows confirms it as a transition. A spec that started
// just before the write is classified as idle, and the frame then shows it
// RUNNING, so the wait keeps waiting rather than confirming it. Neither
// misclassification can manufacture evidence.
func (s *Server) vramLiveProcessBySpec(serverID string) map[string]bool {
	out := map[string]bool{}
	for _, status := range s.RuntimeStatus.statusSnapshot(serverID) {
		if vramStatesWithProcess[status.State] {
			out[status.SpecID] = true
		}
	}
	return out
}

// vramDrain writes admin_state: force_stopped to every spec, THE TARGET AMONG
// THEM, and returns what it actually wrote so the caller's deferred restore
// clears exactly that set.
//
// Writing it only to the RUNNING specs would leave a window in which a
// request through the agent's own router starts an idle one mid-measurement;
// writing it only to the OTHERS would leave the target itself resident, which
// destroys the measurement outright -- the load core short-circuits on an
// already-resident model, so the baseline would already contain it and the
// delta would be a definitive ~0.
//
// The write is compare-and-set against "" (the enumeration already refused
// any pre-existing override), so a concurrent operator override between the
// enumeration and the write is refused rather than clobbered. On any failure
// it stops and returns what it drained plus the error: the caller restores
// that set and reports the failure.
func (s *Server) vramDrain(ctx context.Context, specIDs []string) ([]string, error) {
	drained := make([]string, 0, len(specIDs))
	for _, specID := range specIDs {
		if _, err := s.Portal.SetBenchmarkRuntimeSpecAdminState(ctx, specID, "", "force_stopped"); err != nil {
			return drained, err
		}
		drained = append(drained, specID)
	}
	return drained, nil
}

// vramAwaitIsolation waits until every spec carries THIS RUN'S OWN evidence
// that it is not running, and returns that evidence plus whether the whole set
// was confirmed. ok == false is a genuine isolation timeout.
//
// THE WATERMARK, and why it is not a timestamp. A stopped frame that predates
// this run's write proves nothing, and no frame carries an arrival time of
// its own. So the discipline is structural: this subscribes AFTER the write,
// and it DISCARDS the subscription's snapshot entirely. subscribe registers
// the channel under the registry's own lock before returning, and publish
// collects its targets under that same lock -- so any frame delivered to this
// channel was published after the registration completed, which was after
// the write. Channel ordering IS the watermark, and it needs no clock
// comparison between the gateway and the agent.
//
// THE BINDING DELAY GATES BOTH HALVES OF THE PARTITION, and that is the whole
// of what "evidence" means here. Until vramIsolationBindDelay has elapsed
// since the write, the agent may not hold the document at all, so no frame in
// that window says anything about this run: the delay is applied first and the
// partition only afterwards.
//
// It used to gate one half only. A spec with no live process was confirmed
// after the delay, but a spec that HAD one was confirmed by the first
// no-process frame with no delay whatsoever -- on the reasoning that a stop
// TRANSITION is itself the proof the override arrived. It is not. A spec's own
// exit looks exactly the same on the wire: an idle timeout (which the run's own
// reservation makes likely, since it leaves every sibling idle), or a crash
// landing in `crashed`/`backoff` -- both no-process states, and both states the
// agent RESTARTS from. Confirming on that frame claimed an applied override
// from a self-exit, and the spec was then one backoff timer or one router
// request away from running straight through the measurement.
//
// It also made vramLiveProcessBySpec's staleness matter. That classification
// is up to one telemetry interval old, so a spec that had ALREADY exited before
// the write can be labelled live -- and under the old order that mislabel
// bought the delay-free branch, i.e. the stronger label, for exactly the case
// the delay exists for. With the delay applied first, the misclassification
// changes which evidence STRING is recorded and nothing else, which is what
// that function's doc block always claimed.
//
// THE PARTITION, and why both halves are still needed. After the delay, a spec
// that HAD a live process is waited for until a no-process state appears; a
// spec that had NO live process produces no state change and no frame of its
// own -- a force_stopped write against it does nothing -- so it can only be
// CONFIRMED, never awaited. Waiting for a transition that will never arrive is
// what turns an already-quiet server into an isolation timeout.
//
// Partial evidence is returned even on a timeout, so the report can be
// audited rather than believed.
//
// This function owns only the ADMISSIBILITY of a frame -- the watermark, the
// binding delay, the bound and the cancellation. What an admissible frame
// proves about each still-pending spec is vramFrameEvidence.
func (s *Server) vramAwaitIsolation(ctx context.Context, serverID string, specIDs []string, liveAtWrite map[string]bool, bindDelay time.Duration) (map[string]string, bool) {
	evidence := map[string]string{}
	if len(specIDs) == 0 {
		return evidence, false
	}
	_, frames, unsub := s.RuntimeStatus.subscribe(serverID)
	defer unsub()

	pending := make(map[string]struct{}, len(specIDs))
	for _, specID := range specIDs {
		pending[specID] = struct{}{}
	}
	bindDeadline := time.Now().Add(bindDelay)
	timer := time.NewTimer(bindDelay + vramIsolationDrainBound)
	defer timer.Stop()

	for len(pending) > 0 {
		select {
		case <-ctx.Done():
			return evidence, false
		case <-timer.C:
			return evidence, false
		case frame, open := <-frames:
			if !open {
				return evidence, false
			}
			if time.Now().Before(bindDeadline) {
				// The override has not necessarily landed yet, so NOTHING on
				// this frame is evidence -- for either half of the partition.
				// See the doc block above.
				continue
			}
			for specID, label := range vramFrameEvidence(frame, pending, liveAtWrite) {
				evidence[specID] = label
				delete(pending, specID)
			}
		}
	}
	return evidence, true
}

// vramFrameEvidence reports what ONE admissible status frame proves about the
// specs still pending -- spec id to evidence label, empty when the frame moved
// nothing. The caller has already established that the frame is admissible at
// all (it arrived on a post-write subscription, and the binding delay has
// elapsed); this decides only what an admissible frame says.
//
// It returns the labels rather than writing them, so the wait's own evidence
// map and pending set are mutated in exactly one place. A spec absent from the
// frame, or present in a state this gateway does not recognize as
// process-free, earns nothing and stays pending -- the fail-closed direction,
// which surfaces as an isolation timeout rather than as an isolation this run
// cannot justify.
//
// WHICH label is the only thing liveAtWrite decides, and that is why its
// staleness is harmless: both labels are evidence of the same fact (this spec
// is not running), and they differ in what they say about HOW it got there.
// See vramLiveProcessBySpec.
func vramFrameEvidence(frame []RuntimeStatusDTO, pending map[string]struct{}, liveAtWrite map[string]bool) map[string]string {
	stateBySpec := vramStateBySpec(frame)
	var found map[string]string
	for specID := range pending {
		state, present := stateBySpec[specID]
		if !present || !vramStateNoProcess(state) {
			continue
		}
		if found == nil {
			found = make(map[string]string, len(pending))
		}
		if liveAtWrite[specID] {
			found[specID] = vramEvidenceStoppedAfterWrite
			continue
		}
		found[specID] = vramEvidenceNoProcessAtWrite
	}
	return found
}

// vramRestore clears every override this run set, back to exactly "" -- the
// only value it ever has to restore, because the run refused to start against
// a pre-existing one.
//
// It runs on a context DERIVED FROM but not cancelled with the run's
// (context.WithoutCancel plus its own deadline), because the run's context is
// cancelled precisely when the restore matters most: when the run finishes,
// errors, or is cancelled by the operator.
//
// Each spec is RE-READ immediately before it is written, inside the
// compare-and-set writer. A full-document replace of a spec captured BEFORE
// the run would revert every field an operator edited DURING it -- and a
// launch spec is exactly what an operator opens while a model is stopped.
//
// IT RETURNS TWO SETS, BECAUSE "THE RESTORE DID NOT HAPPEN" MEANS TWO
// DIFFERENT THINGS AND THE PORTAL TURNS ONE OF THEM INTO AN INSTRUCTION.
//
//   - takenOver: the freshly-read admin_state is no longer this run's
//     force_stopped, so nothing is written -- somebody else owns the field
//     now. That is an operator's mid-run "Force start" (force_running) or
//     "Clear override" (""), and in BOTH the spec is no longer force-stopped.
//     Reporting it as a failure rendered "these specs are still force_stopped
//     and have to be cleared by hand": false, and an instruction that STOPS a
//     model the operator had just deliberately started.
//   - failed: the write itself could not be made -- a store error. This is the
//     one case where the override really is still in place and really does
//     need clearing by hand.
//
// The compare-and-set already returns the distinction
// (portal.ErrRuntimeSpecAdminStateConflict); it was being thrown away.
//
// A DELETED spec is neither -- its override went with it.
//
// This is a deliberate divergence from the portal's own start/stop
// discipline, which chose NOT to clear an override on a timeout because it
// cannot tell a wedged child from a slow one. A benchmark can: it created
// these overrides, so leaving them is strictly worse than clearing them.
func (s *Server) vramRestore(ctx context.Context, drained []string) (failed, takenOver []string) {
	if len(drained) == 0 {
		return nil, nil
	}
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), vramRestoreTimeout)
	defer cancel()
	for _, specID := range drained {
		_, err := s.Portal.SetBenchmarkRuntimeSpecAdminState(restoreCtx, specID, "force_stopped", "")
		switch {
		case err == nil:
		case errors.Is(err, portal.ErrRuntimeSpecNotFound):
			// The spec was deleted mid-run; its override went with it.
		case errors.Is(err, portal.ErrRuntimeSpecAdminStateConflict):
			slog.Info("vram benchmark: an admin override was taken over during the run", "spec_id", specID)
			takenOver = append(takenOver, specID)
		default:
			slog.Warn("vram benchmark: could not restore an admin override", "spec_id", specID, "err", err)
			failed = append(failed, specID)
		}
	}
	return failed, takenOver
}
