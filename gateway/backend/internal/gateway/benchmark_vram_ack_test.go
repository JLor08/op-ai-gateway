// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The isolation ACKNOWLEDGEMENT: the agent reports which runtime-config
// document it has APPLIED, and that report -- not a blind wait -- is what
// proves this run's force_stopped overrides landed.
//
// Everything here turns on one property of the document's ETag, and every test
// below would be meaningless without it: the ETag is a sha256 digest over the
// document with the ETag field itself blanked (portal.agentRuntimeConfigETag),
// so it is a DETERMINISTIC FUNCTION OF CONTENT. The gateway can therefore
// derive the exact value an agent holding a given document must report, and
// equality is a real proof rather than a shared counter.
//
// The tests are written against the derived value on purpose. A test that
// asserted against a hand-written string would pass just as happily if the
// comparison were deleted and any non-empty acknowledgement accepted -- which
// is the one mutation this feature must not survive.

// vramDerivedETag is the ETag the gateway itself derives for serverID's
// CURRENT runtime-config document: exactly what an agent that has applied that
// document must report back.
func (f *vramFixture) vramDerivedETag(t *testing.T) string {
	t.Helper()
	dto, err := f.srv.Portal.AgentRuntimeConfig(context.Background(), "srv1")
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	if dto.ETag == "" {
		t.Fatal("the derived runtime-config document carries no ETag")
	}
	return dto.ETag
}

// ack makes the server's agent report etag as the document it has applied,
// which is what the ~1 s telemetry ingest does in production.
func (f *vramFixture) ack(etag string) {
	f.srv.RuntimeStatus.SetAppliedConfigETag("srv1", etag)
}

// declareAck puts the agent in the acknowledging class: it declares both the
// managed-runtime feature the run already requires and the acknowledgement
// feature this file is about.
func (f *vramFixture) declareAck() {
	f.srv.AgentFeatures.Set("srv1", []string{"runtime_manager", runtimeConfigAckFeature})
}

// drainAll writes this run's overrides the way the run itself does, so the
// document the gateway derives afterwards is the one an acknowledging agent
// would be applying.
func (f *vramFixture) drainAll(t *testing.T) []string {
	t.Helper()
	drained, err := f.srv.vramDrain(context.Background(), []string{f.siblingSpec, f.targetSpec})
	if err != nil {
		t.Fatalf("vramDrain: %v", err)
	}
	return drained
}

// ackWhateverIsDerived is a WELL-BEHAVED acknowledging agent, compressed: it
// re-derives the server's current document and reports having applied it,
// every tick, for as long as the returned stop function has not been called.
//
// A whole run needs this rather than one static value because the run itself
// changes the document twice -- the drain, then the target's own release --
// and an agent reconciles after each. Nothing about the wait's proof is
// weakened by the agent being prompt: the comparison is still against a
// digest the gateway derived, and the stale/garbled cases above drive the
// failing directions directly.
func (f *vramFixture) ackWhateverIsDerived(t *testing.T) func() {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				dto, err := f.srv.Portal.AgentRuntimeConfig(context.Background(), "srv1")
				if err == nil {
					f.ack(dto.ETag)
				}
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// --- the fallback: an agent that will never acknowledge --------------------

// TestVRAMIsolationKeepsTheBindingDelayWithoutTheAckFeature is the
// no-regression half, and it is first for that reason: an agent that has not
// declared the acknowledgement feature will never report an applied ETag, so
// the wait must keep today's behaviour exactly -- the blind binding delay, and
// a confirmation once it has elapsed.
//
// Falling back is not a nicety. The gateway negotiates by NAME and never by
// version (ADR-025), so "no acknowledgement will ever arrive" is a first-class
// state of the fleet, not a fault: an older agent binary is expected to be in
// the field indefinitely, and a wait that hung on it would make the whole
// feature regress for every server that has not been upgraded.
func TestVRAMIsolationKeepsTheBindingDelayWithoutTheAckFeature(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.drive(t)
	// The default fixture agent declares runtime_manager only.
	f.drainAll(t)
	// An acknowledgement is even PRESENT and correct -- it must be ignored,
	// because an agent that did not declare the feature never sends one, so a
	// value here can only be stale.
	f.ack(f.vramDerivedETag(t))

	bind := 120 * time.Millisecond
	start := time.Now()
	res := f.srv.vramAwaitIsolation(context.Background(), "srv1",
		[]string{f.siblingSpec, f.targetSpec}, map[string]bool{},
		vramIsolationProofPolicy{bindDelay: bind})
	elapsed := time.Since(start)

	if !res.confirmed {
		t.Fatalf("isolation not confirmed; evidence = %v, reason = %q", res.evidence, res.reason)
	}
	if elapsed < bind {
		t.Fatalf("confirmed after %v, before the %v binding delay: the fallback standard was skipped", elapsed, bind)
	}
	for _, specID := range []string{f.siblingSpec, f.targetSpec} {
		if res.evidence[specID] != vramEvidenceNoProcessAtWrite {
			t.Fatalf("%s evidence = %q, want %q", specID, res.evidence[specID], vramEvidenceNoProcessAtWrite)
		}
	}
}

// --- the fast path --------------------------------------------------------

// TestVRAMIsolationConfirmsAsSoonAsTheAgentAcknowledgesTheDocument is the
// point of the whole change: an acknowledging agent's report that it has
// applied THIS document is the proof, so the wait completes as soon as it
// arrives instead of burning a binding delay that exists only because nothing
// on the wire said "applied".
//
// The binding delay is set far longer than the test's own patience, so a wait
// that still consulted it would fail on the clock rather than on the assertion.
func TestVRAMIsolationConfirmsAsSoonAsTheAgentAcknowledgesTheDocument(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.declareAck()
	f.drive(t)
	f.drainAll(t)
	f.ack(f.vramDerivedETag(t))

	bind := 30 * time.Second
	start := time.Now()
	res := f.srv.vramAwaitIsolation(context.Background(), "srv1",
		[]string{f.siblingSpec, f.targetSpec}, map[string]bool{},
		vramIsolationProofPolicy{acknowledged: true, bindDelay: bind})
	elapsed := time.Since(start)

	if !res.confirmed {
		t.Fatalf("an acknowledged document did not confirm; evidence = %v, reason = %q", res.evidence, res.reason)
	}
	if elapsed >= bind {
		t.Fatalf("confirmed only after %v: the acknowledgement did not replace the binding delay", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("confirmed after %v, want promptly on the acknowledgement", elapsed)
	}
}

// TestVRAMIsolationRefusesAStaleAcknowledgement is the whole load-bearing
// comparison, and it is written against a REAL, DIFFERENT digest rather than
// an invented string precisely so that deleting the comparison cannot make it
// pass: the value the agent reports here is the ETag of the document as it
// stood BEFORE the drain, i.e. an agent that is still holding a configuration
// in which nothing is force-stopped.
//
// That document can never be mistaken for a drained one, and the reason is
// structural rather than lucky: the run REFUSES to start against any
// pre-existing override (vramEnumerateFleet), so a pre-drain document always
// differs from a post-drain one in at least one spec's admin_state -- and
// because the ETag is a content digest, differing content means a differing
// digest.
func TestVRAMIsolationRefusesAStaleAcknowledgement(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.declareAck()
	f.drive(t)

	stale := f.vramDerivedETag(t) // nothing is force-stopped yet
	f.drainAll(t)
	fresh := f.vramDerivedETag(t)
	if stale == fresh {
		t.Fatal("the drain did not change the document's ETag: the whole comparison would be vacuous")
	}
	f.ack(stale)

	oldBound := vramIsolationDrainBound
	vramIsolationDrainBound = 120 * time.Millisecond
	t.Cleanup(func() { vramIsolationDrainBound = oldBound })

	res := f.srv.vramAwaitIsolation(context.Background(), "srv1",
		[]string{f.siblingSpec, f.targetSpec}, map[string]bool{},
		vramIsolationProofPolicy{acknowledged: true, bindDelay: 10 * time.Millisecond})

	if res.confirmed {
		t.Fatalf("a stale acknowledgement was accepted as proof: evidence = %v", res.evidence)
	}
	if res.reason != vramInconclusiveIsolationUnacknowledged {
		t.Fatalf("reason = %q, want %q: an agent that declared the feature and never confirmed is not the same failure as a spec that never went quiet",
			res.reason, vramInconclusiveIsolationUnacknowledged)
	}
	if len(res.evidence) != 0 {
		t.Fatalf("evidence = %v, want none: no frame in the wait was admissible", res.evidence)
	}
}

// TestVRAMIsolationRefusesAGarbledAcknowledgement is the same rule against the
// other input class: the reported value is agent-supplied free text, and the
// comparison is against a digest the GATEWAY derived, so nothing an agent can
// invent confirms an isolation.
func TestVRAMIsolationRefusesAGarbledAcknowledgement(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.declareAck()
	f.drive(t)
	f.drainAll(t)
	f.ack("applied")

	oldBound := vramIsolationDrainBound
	vramIsolationDrainBound = 120 * time.Millisecond
	t.Cleanup(func() { vramIsolationDrainBound = oldBound })

	res := f.srv.vramAwaitIsolation(context.Background(), "srv1",
		[]string{f.siblingSpec, f.targetSpec}, map[string]bool{},
		vramIsolationProofPolicy{acknowledged: true, bindDelay: 10 * time.Millisecond})
	if res.confirmed {
		t.Fatalf("an invented acknowledgement was accepted as proof: evidence = %v", res.evidence)
	}
}

// --- the design trap: the ETag covers the WHOLE document -------------------

// TestVRAMIsolationAcceptsTheAcknowledgementAfterAMidWaitDocumentChange is the
// trap a naive equality wait falls into, driven directly.
//
// The ETag covers the whole document, not just the specs' admin_state, so ANY
// write that reaches the derivation changes it -- and the benchmark's
// reservation does not cover all of them: it gates the launch-spec PUT
// (409 runtime_spec.server_benchmarking) and deliberately nothing else, so a
// per-GPU budget write, a mapping rename, a router-port change and the agent's
// own measured-VRAM write-back all still land. An agent then applies the NEW
// document and reports ITS etag, and a wait pinned to the one value captured
// after the drain would never match -- burning its whole bound and reporting a
// timeout for a fleet that was correctly drained the entire time.
//
// The change is made AFTER the wait has started, on purpose: the gate derives
// the document once when it is built, so a change made before that would be
// picked up by the seed and this test would pass with the re-derivation
// deleted. It was written that way first, and the mutation survived.
func TestVRAMIsolationAcceptsTheAcknowledgementAfterAMidWaitDocumentChange(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.declareAck()
	f.drive(t)
	f.drainAll(t)
	// The value the gate seeds itself with, and the one the agent will NOT be
	// reporting: nothing is acknowledged yet.
	seeded := f.vramDerivedETag(t)

	acked := make(chan string, 1)
	// A third-party write the reservation does not gate, and the one that is
	// GUARANTEED to land during a real run: the agent's own measured-VRAM
	// write-back, which the notification rule deliberately exempts and which
	// arrives at telemetry cadence.
	change := time.AfterFunc(30*time.Millisecond, func() {
		if err := f.mem.UpdateRuntimeSpecGPUMeasured(context.Background(), f.targetSpec, 0, 17123); err != nil {
			return
		}
		dto, err := f.srv.Portal.AgentRuntimeConfig(context.Background(), "srv1")
		if err != nil {
			return
		}
		f.ack(dto.ETag)
		acked <- dto.ETag
	})
	defer change.Stop()

	// bindDelay is only half of the BOUND under the acknowledged standard, so
	// it is kept small: this test's failure mode is an exhausted bound, and a
	// 30 s delay here would make a regression cost half a minute of suite time
	// rather than three seconds.
	res := f.srv.vramAwaitIsolation(context.Background(), "srv1",
		[]string{f.siblingSpec, f.targetSpec}, map[string]bool{},
		vramIsolationProofPolicy{acknowledged: true, bindDelay: time.Second})
	if !res.confirmed {
		t.Fatalf("a mid-wait document change defeated the acknowledgement; reason = %q, evidence = %v", res.reason, res.evidence)
	}
	select {
	case got := <-acked:
		if got == seeded {
			t.Fatal("the mid-wait write did not change the document: the trap is not being driven")
		}
	default:
		t.Fatal("the wait confirmed before the mid-wait change landed: it cannot have been the changed document that proved it")
	}
}

// TestVRAMIsolationReportsIsolationLostWhenTheDocumentStopsDrainingTheFleet is
// the OTHER side of that re-derivation, and it is why re-deriving is a proof
// rather than a convenience: the wait must accept a changed document only
// after checking that the change kept the overrides.
//
// Here an operator clears one drained spec's override mid-wait. The agent
// dutifully applies the new document and reports it -- and that document lets
// the sibling start. Confirming on it would report `isolated: true` for a run
// whose isolation had already been revoked, which is the exact class of lie
// this feature exists to end. The run says `isolation_lost` instead: the same
// reason, and the same operator action, as a sibling seen running again at the
// end of the measurement.
//
// The same check guards the gate's initial derive, so a takeover that lands
// between the drain and the wait is caught by this one branch too.
func TestVRAMIsolationReportsIsolationLostWhenTheDocumentStopsDrainingTheFleet(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.declareAck()
	f.drive(t)
	f.drainAll(t)

	revoke := time.AfterFunc(30*time.Millisecond, func() {
		// Somebody clears the sibling's override, so the fleet is no longer
		// drained -- and the agent applies and acknowledges that document.
		if _, err := f.srv.Portal.SetBenchmarkRuntimeSpecAdminState(
			context.Background(), f.siblingSpec, vramAdminStateForceStopped, ""); err != nil {
			return
		}
		dto, err := f.srv.Portal.AgentRuntimeConfig(context.Background(), "srv1")
		if err == nil {
			f.ack(dto.ETag)
		}
	})
	defer revoke.Stop()

	oldBound := vramIsolationDrainBound
	vramIsolationDrainBound = 3 * time.Second
	t.Cleanup(func() { vramIsolationDrainBound = oldBound })

	res := f.srv.vramAwaitIsolation(context.Background(), "srv1",
		[]string{f.siblingSpec, f.targetSpec}, map[string]bool{},
		vramIsolationProofPolicy{acknowledged: true, bindDelay: 10 * time.Millisecond})
	if res.confirmed {
		t.Fatalf("a document that no longer force-stops the fleet was accepted as proof: evidence = %v", res.evidence)
	}
	if res.reason != vramInconclusiveIsolationLost {
		t.Fatalf("reason = %q, want %q", res.reason, vramInconclusiveIsolationLost)
	}
}

// vramFlakyRuntimeConfigStore fails the FIRST n AIServerByID reads and then
// behaves normally. AIServerByID is the first read
// portal.Service.AgentRuntimeConfig makes, and a real (not fake) portal over a
// real store is the only way to drive a derive failure at all: the failure has
// to originate below the portal, because the portal is what turns a store
// error into the error the gate sees.
type vramFlakyRuntimeConfigStore struct {
	routing.Store
	failures atomic.Int32
}

func (s *vramFlakyRuntimeConfigStore) AIServerByID(ctx context.Context, id string) (routing.AIServer, error) {
	if s.failures.Add(-1) >= 0 {
		return routing.AIServer{}, errors.New("vram test: transient store failure")
	}
	s.failures.Store(0)
	return s.Store.AIServerByID(ctx, id)
}

// TestVRAMIsolationRetriesADeriveThatFailedTransiently is a defect this branch
// introduced and then found by reading rather than by a failing test, which is
// why it gets one.
//
// The gate answers each distinct acknowledgement at most once, so that a
// steady stream of identical ones costs a single store read instead of one per
// telemetry sample. The first version recorded the acknowledgement as answered
// BEFORE it knew the derive had worked -- so one transient store error burned
// that acknowledgement's only chance: the agent goes on reporting the same
// correct value every second, the gate keeps answering from a cache that never
// learned anything, and the run reaches its bound reporting
// `isolation_unacknowledged` over a document that had been applied all along.
//
// TWO failures are injected, because there are two derives to defeat: the
// gate's own seed, and the first evaluation of the reported value.
func TestVRAMIsolationRetriesADeriveThatFailedTransiently(t *testing.T) {
	flaky := &vramFlakyRuntimeConfigStore{Store: routing.NewMemoryStore()}
	f := newVRAMFixture(t, vramFixtureOpts{store: flaky})
	f.seedLatestSample()
	f.declareAck()
	f.drive(t)
	f.drainAll(t)
	// Derived while the store is still healthy: this is the value a promptly
	// reconciling agent reports, and the one the gate must eventually accept.
	f.ack(f.vramDerivedETag(t))
	flaky.failures.Store(2)

	res := f.srv.vramAwaitIsolation(context.Background(), "srv1",
		[]string{f.siblingSpec, f.targetSpec}, map[string]bool{},
		vramIsolationProofPolicy{acknowledged: true, bindDelay: time.Second})
	if !res.confirmed {
		t.Fatalf("a transient derive failure permanently defeated a correct acknowledgement; reason = %q, evidence = %v",
			res.reason, res.evidence)
	}
}

// --- ingest, the registry, and the negotiated name ------------------------

// TestRuntimeStatusRegistryAppliedConfigETagIsASnapshot pins the registry
// half's one rule: each telemetry sample is a FULL snapshot, so an agent that
// stops reporting an applied ETag (a downgrade, a restart before its first
// reconcile) must read as "nothing acknowledged" on the very next sample --
// never as a stale confirmation that some document is still applied.
func TestRuntimeStatusRegistryAppliedConfigETagIsASnapshot(t *testing.T) {
	reg := NewRuntimeStatusRegistry()
	if got := reg.AppliedConfigETag("srv1"); got != "" {
		t.Fatalf("a server that never reported = %q, want empty", got)
	}
	reg.SetAppliedConfigETag("srv1", "abc")
	if got := reg.AppliedConfigETag("srv1"); got != "abc" {
		t.Fatalf("AppliedConfigETag = %q, want %q", got, "abc")
	}
	reg.SetAppliedConfigETag("srv1", "")
	if got := reg.AppliedConfigETag("srv1"); got != "" {
		t.Fatalf("a sample that reported nothing left %q behind", got)
	}
	// Nil-safe, like every other method on this registry.
	var nilReg *runtimeStatusRegistry
	nilReg.SetAppliedConfigETag("srv1", "abc")
	if got := nilReg.AppliedConfigETag("srv1"); got != "" {
		t.Fatalf("nil registry = %q, want empty", got)
	}
}

// TestRuntimeStatusRegistryRetainPrunesTheAppliedConfigETag: a deleted
// server's acknowledgement must not sit in memory for the rest of the
// process's life, exactly like its file-mode flag and its status snapshot.
func TestRuntimeStatusRegistryRetainPrunesTheAppliedConfigETag(t *testing.T) {
	reg := NewRuntimeStatusRegistry()
	reg.SetAppliedConfigETag("gone", "abc")
	reg.SetAppliedConfigETag("live", "def")
	reg.Retain(map[string]struct{}{"live": {}})
	if got := reg.AppliedConfigETag("gone"); got != "" {
		t.Fatalf("a pruned server kept %q", got)
	}
	if got := reg.AppliedConfigETag("live"); got != "def" {
		t.Fatalf("a live server lost its acknowledgement: %q", got)
	}
}

// TestAppliedRuntimeConfigETagIsClamped bounds what one agent can make the
// gateway hold. The comparison that matters is against a digest the gateway
// derived itself, so an over-long value can never confirm anything -- what the
// clamp buys is the memory bound, the same reason clampHardwareString exists.
func TestAppliedRuntimeConfigETagIsClamped(t *testing.T) {
	long := strings.Repeat("f", maxAppliedConfigETag+64)
	got := clampAppliedConfigETag(long)
	if len(got) != maxAppliedConfigETag {
		t.Fatalf("clamped length = %d, want %d", len(got), maxAppliedConfigETag)
	}
	if got := clampAppliedConfigETag("  abc  "); got != "abc" {
		t.Fatalf("clampAppliedConfigETag(%q) = %q, want %q", "  abc  ", got, "abc")
	}
}

// TestIngestTelemetryRecordsTheAppliedRuntimeConfigETag drives the field over
// the SAME shared ingest core both transports funnel through, so the wire name
// itself is pinned: `runtime_config_applied_etag`, top level, alongside
// agent_version -- the precedent for a diagnostic about the agent rather than
// about one managed process.
//
// A rename on either side of the two Go modules silently disables the whole
// fast path (the field decodes as empty, the wait falls back), which is the
// safe direction and exactly the kind of silence a test has to make loud.
func TestIngestTelemetryRecordsTheAppliedRuntimeConfigETag(t *testing.T) {
	srv := NewTestServer()
	body := `{"host":{"cpu_util_pct":10},"runtime_config_applied_etag":"` + strings.Repeat("a", 64) + `"}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := srv.RuntimeStatus.AppliedConfigETag("mock-host-qwen"); got != strings.Repeat("a", 64) {
		t.Fatalf("AppliedConfigETag = %q, want the reported digest", got)
	}
}

// TestIngestTelemetryWithoutAnAcknowledgementClearsIt: a legacy agent, or one
// that restarted before its first reconcile, sends nothing here -- and the
// prior acknowledgement must go with it, because each sample is a full
// snapshot. A stale confirmation left standing is how a wait would confirm an
// isolation against a document nobody is holding any more.
func TestIngestTelemetryWithoutAnAcknowledgementClearsIt(t *testing.T) {
	srv := NewTestServer()
	srv.RuntimeStatus.SetAppliedConfigETag("mock-host-qwen", "previously-applied")
	req, raw := ingestReq(t, validIngestAgentBody) // no runtime_config_applied_etag
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := srv.RuntimeStatus.AppliedConfigETag("mock-host-qwen"); got != "" {
		t.Fatalf("a sample carrying no acknowledgement left %q standing", got)
	}
}

// TestGatewayDeclaresTheRuntimeConfigAckFeature: the gateway must DECLARE the
// name too. A feature is active only when both sides declare it, and this side
// is what the agent's own fetch of GET /api/agent/v1/features reads.
func TestGatewayDeclaresTheRuntimeConfigAckFeature(t *testing.T) {
	var found bool
	for _, name := range gatewayAgentFeatures {
		if name == runtimeConfigAckFeature {
			found = true
		}
	}
	if !found {
		t.Fatalf("gatewayAgentFeatures = %v, want it to declare %q", gatewayAgentFeatures, runtimeConfigAckFeature)
	}
}

// --- the plan, and the proof it records -----------------------------------

// TestVRAMRunPlanRecordsWhichProofStandardApplies: the run reads the declared
// feature ONCE, before it writes anything, and carries the answer -- the same
// discipline as the plan's warnings. What an operator gets from it is the
// report's own account of WHICH proof was used, since "the agent confirmed it
// applied this document" and "we waited a minute and saw no process" are
// different strengths of evidence.
func TestVRAMRunPlanRecordsWhichProofStandardApplies(t *testing.T) {
	for _, tc := range []struct {
		name     string
		features []string
		want     string
	}{
		{name: "acknowledging agent", features: []string{"runtime_manager", runtimeConfigAckFeature}, want: vramProofConfigAcknowledged},
		{name: "older agent", features: []string{"runtime_manager"}, want: vramProofBindDelay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newVRAMFixture(t, vramFixtureOpts{})
			f.seedLatestSample()
			f.srv.AgentFeatures.Set("srv1", tc.features)
			plan, err := f.srv.vramRunPlan(context.Background(), f.target)
			if err != nil {
				t.Fatalf("vramRunPlan: %v", err)
			}
			if got := vramIsolationPolicyOf(plan).proof(); got != tc.want {
				t.Fatalf("proof = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVRAMReportNamesTheProofItUsed drives the whole run and reads the report
// an operator would: the closed proof vocabulary has to reach the payload, or
// the distinction it exists to make never reaches anybody.
func TestVRAMReportNamesTheProofItUsed(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.declareAck()
	f.seedLatestSample()
	f.drive(t)
	// Acknowledge whatever the run derives, continuously: the run drains
	// first, so the document it wants acknowledged does not exist yet here.
	stop := f.ackWhateverIsDerived(t)
	defer stop()

	res := vramOneResult(t, f.run(t))
	if res.VRAM == nil {
		t.Fatal("the run produced no report")
	}
	if res.VRAM.IsolationProof != vramProofConfigAcknowledged {
		t.Fatalf("isolation proof = %q, want %q", res.VRAM.IsolationProof, vramProofConfigAcknowledged)
	}
	if !res.VRAM.Isolated {
		t.Fatalf("isolated = false; evidence = %v, inconclusive = %q, err = %q",
			res.VRAM.IsolationEvidence, res.VRAM.Inconclusive, res.Error)
	}
}

// --- the standard the report CLAIMS is the standard the wait APPLIED --------

// TestVRAMIsolationPolicyIsOneDerivation pins the join itself: the wait's
// policy and the report's proof string come out of ONE derivation from the
// plan, so the two cannot say different things.
//
// It used to be two reads of plan.acknowledged -- one at the report's
// IsolationProof, one at the wait's argument -- with nothing tying them
// together, so an edit to either line could have made the report claim "the
// agent confirmed it applied this document" over a wait that had in fact
// applied the timed fallback. For a feature whose entire purpose is that a
// confirmed isolation is PROVABLE, that is the one divergence that must be
// impossible; TestVRAMReportsTheProofTheWaitActuallyApplied below drives the
// same guarantee end to end, through the run.
func TestVRAMIsolationPolicyIsOneDerivation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		acknowledged bool
		want         string
	}{
		{name: "acknowledging agent", acknowledged: true, want: vramProofConfigAcknowledged},
		{name: "older agent", acknowledged: false, want: vramProofBindDelay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := vramRunPlanned{acknowledged: tc.acknowledged, bindDelay: 7 * time.Second}
			policy := vramIsolationPolicyOf(plan)
			// The policy the WAIT is handed carries the plan's own decision...
			if policy.acknowledged != plan.acknowledged {
				t.Fatalf("policy.acknowledged = %v, want the plan's %v", policy.acknowledged, plan.acknowledged)
			}
			if policy.bindDelay != plan.bindDelay {
				t.Fatalf("policy.bindDelay = %v, want the plan's %v", policy.bindDelay, plan.bindDelay)
			}
			// ...and the string the REPORT carries is read off that same policy,
			// never off the plan a second time.
			if got := policy.proof(); got != tc.want {
				t.Fatalf("policy.proof() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVRAMReportsTheProofTheWaitActuallyApplied drives the WHOLE run under
// both standards and checks the report's claim against what the wait
// demonstrably did, because that is the only way to catch the two drifting.
//
// The discriminator is the clock, and it is the one signal that cannot be
// faked by whichever value the report happens to carry:
//
//   - ACKNOWLEDGED: the binding delay is set far longer than the test's
//     patience. A run that reports `config_acknowledged` while its wait had
//     actually applied the fallback would have to sit out that delay, so it
//     fails on elapsed time rather than on the string.
//   - FALLBACK: the agent acknowledges NOTHING (it does not declare the
//     feature), so a wait that had applied the acknowledged standard could
//     never admit a frame at all -- the run would come back inconclusive with
//     `isolation_unacknowledged` instead of a confirmed isolation.
func TestVRAMReportsTheProofTheWaitActuallyApplied(t *testing.T) {
	t.Run("acknowledged", func(t *testing.T) {
		f := newVRAMFixture(t, vramFixtureOpts{})
		// Long enough that applying the fallback is unmistakable, short enough
		// that a regression costs seconds rather than a minute.
		vramIsolationBindDelay = 6 * time.Second
		f.declareAck()
		f.seedLatestSample()
		f.drive(t)
		stop := f.ackWhateverIsDerived(t)
		defer stop()

		start := time.Now()
		res := vramOneResult(t, f.run(t))
		elapsed := time.Since(start)

		if res.VRAM == nil {
			t.Fatal("the run produced no report")
		}
		if res.VRAM.IsolationProof != vramProofConfigAcknowledged {
			t.Fatalf("isolation proof = %q, want %q", res.VRAM.IsolationProof, vramProofConfigAcknowledged)
		}
		if !res.VRAM.Isolated {
			t.Fatalf("isolated = false; evidence = %v, inconclusive = %q, err = %q",
				res.VRAM.IsolationEvidence, res.VRAM.Inconclusive, res.Error)
		}
		if elapsed >= vramIsolationBindDelay {
			t.Fatalf("the whole run took %v, at least the %v binding delay: the report claims %q but the wait applied the fallback",
				elapsed, vramIsolationBindDelay, vramProofConfigAcknowledged)
		}
	})

	t.Run("fallback", func(t *testing.T) {
		f := newVRAMFixture(t, vramFixtureOpts{})
		// Long enough to be measurable against the shrunk timings, short
		// enough to keep the suite quick.
		vramIsolationBindDelay = 400 * time.Millisecond
		// The default fixture agent declares runtime_manager only, and
		// deliberately acknowledges nothing.
		f.seedLatestSample()
		f.drive(t)

		start := time.Now()
		res := vramOneResult(t, f.run(t))
		elapsed := time.Since(start)

		if res.VRAM == nil {
			t.Fatal("the run produced no report")
		}
		if res.VRAM.IsolationProof != vramProofBindDelay {
			t.Fatalf("isolation proof = %q, want %q", res.VRAM.IsolationProof, vramProofBindDelay)
		}
		if !res.VRAM.Isolated {
			t.Fatalf("isolated = false; evidence = %v, inconclusive = %q, err = %q",
				res.VRAM.IsolationEvidence, res.VRAM.Inconclusive, res.Error)
		}
		if elapsed < vramIsolationBindDelay {
			t.Fatalf("the whole run took %v, less than the %v binding delay: the report claims %q but the wait never waited it out",
				elapsed, vramIsolationBindDelay, vramProofBindDelay)
		}
	})
}

// --- what an acknowledgement is an acknowledgement OF ----------------------

// TestVRAMDocumentDrains is the direct test of the predicate the whole
// acknowledgement rests on: an agent's report that it applied a document
// proves an isolation only if THAT DOCUMENT force-stops every spec this run
// enumerated.
//
// The last case is the fail-closed direction its doc block argues for at
// length and nothing exercised: a spec MISSING from the document. Such a spec
// is in fact quiet -- a spec the agent is never told about is not run by it --
// and inferring that is exactly what this function may not do, because its job
// is to decide what an acknowledgement PROVES and the document says nothing
// about a spec it does not mention. Deleting the check (letting an absent spec
// pass) makes this case go red.
func TestVRAMDocumentDrains(t *testing.T) {
	spec := func(id, adminState string) portal.AgentRuntimeSpecDTO {
		return portal.AgentRuntimeSpecDTO{ID: id, AdminState: adminState}
	}
	for _, tc := range []struct {
		name    string
		specs   []portal.AgentRuntimeSpecDTO
		specIDs []string
		want    bool
	}{
		{
			name:    "every enumerated spec is force-stopped",
			specs:   []portal.AgentRuntimeSpecDTO{spec("a", vramAdminStateForceStopped), spec("b", vramAdminStateForceStopped)},
			specIDs: []string{"a", "b"},
			want:    true,
		},
		{
			name: "a spec outside the enumeration is irrelevant",
			specs: []portal.AgentRuntimeSpecDTO{
				spec("a", vramAdminStateForceStopped), spec("b", vramAdminStateForceStopped), spec("other", ""),
			},
			specIDs: []string{"a", "b"},
			want:    true,
		},
		{
			name:    "one override cleared -- an operator's mid-wait Clear override",
			specs:   []portal.AgentRuntimeSpecDTO{spec("a", vramAdminStateForceStopped), spec("b", "")},
			specIDs: []string{"a", "b"},
			want:    false,
		},
		{
			name:    "one override taken over -- an operator's mid-wait Force start",
			specs:   []portal.AgentRuntimeSpecDTO{spec("a", vramAdminStateForceStopped), spec("b", "force_running")},
			specIDs: []string{"a", "b"},
			want:    false,
		},
		{
			name:    "a spec MISSING from the document proves nothing about it",
			specs:   []portal.AgentRuntimeSpecDTO{spec("a", vramAdminStateForceStopped)},
			specIDs: []string{"a", "b"},
			want:    false,
		},
		{
			name:    "an empty document proves nothing either",
			specs:   nil,
			specIDs: []string{"a"},
			want:    false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dto := portal.AgentRuntimeConfigDTO{Specs: tc.specs, ETag: "digest"}
			if got := vramDocumentDrains(dto, tc.specIDs); got != tc.want {
				t.Fatalf("vramDocumentDrains = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestVRAMIsolationGateKeepsEveryVerifiedDocument pins that `accepted` is a
// growing SET and not a one-slot cache of the newest document -- the property
// its own field comment argues for, and the one the re-derivation test cannot
// reach.
//
// The gate does not control the ORDER in which an agent reports digests: the
// value it reads is whatever document that agent last finished applying, and
// the gateway's own document may have moved on twice while the agent was
// applying the first. So the contract is stated over the gate itself: a
// document this run has already derived and found to force-stop the whole
// fleet stays admissible after a LATER one has been verified. A newest-only
// slot answers the second acknowledgement here with a non-confirmation --
// re-deriving finds the current document, which is not the one being reported
// -- and burns the bound reporting an unacknowledged isolation over a fleet
// that was drained the whole time.
func TestVRAMIsolationGateKeepsEveryVerifiedDocument(t *testing.T) {
	ctx := context.Background()
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.declareAck()
	f.drive(t)
	f.drainAll(t)

	// Document 1: what the gate seeds itself with, immediately after the drain.
	first := f.vramDerivedETag(t)
	gate := f.srv.newVRAMIsolationGate(ctx, "srv1", []string{f.siblingSpec, f.targetSpec},
		vramIsolationProofPolicy{acknowledged: true})

	// Document 2: a third-party write the benchmark's reservation does not gate
	// -- the agent's own measured-VRAM write-back -- which still force-stops
	// every enumerated spec.
	if err := f.mem.UpdateRuntimeSpecGPUMeasured(ctx, f.targetSpec, 0, 17123); err != nil {
		t.Fatalf("UpdateRuntimeSpecGPUMeasured: %v", err)
	}
	second := f.vramDerivedETag(t)
	if first == second {
		t.Fatal("the mid-wait write did not change the document: this test would be vacuous")
	}
	f.ack(second)
	if !gate.admits(ctx) {
		t.Fatal("the gate refused an acknowledgement of the CURRENT document")
	}

	// And now the agent reports the earlier one again. It was derived and
	// verified by this very run, so it is still a proof -- but it is no longer
	// the document the store holds, so only a gate that KEPT it can say so.
	f.ack(first)
	if !gate.admits(ctx) {
		t.Fatalf("the gate dropped a document it had already verified: accepted = %v", gate.accepted)
	}
	if gate.lost {
		t.Fatal("the gate reported the isolation lost: both documents force-stop the whole fleet")
	}
}

// --- the end-to-end path: an acknowledgement through ingest ----------------

// TestVRAMIsolationConfirmsFromAnAcknowledgementDrivenThroughIngest is the
// feature's own path, driven whole: the ONLY thing feeding both the status
// frames and the acknowledgement here is ingestTelemetrySample, i.e. the same
// shared core both agent transports funnel through.
//
// Every other test in this file writes the registries directly, which is right
// for the branches they drive but leaves the seam between the wire and the wait
// unpinned -- and that seam is where the whole feature lives. What this covers
// that nothing else does: the JSON field name decodes, the clamp does not
// mangle a real digest, the registry write happens on the ingest path at all,
// and the frame that wakes the wait carries an acknowledgement the wait can
// already read.
func TestVRAMIsolationConfirmsFromAnAcknowledgementDrivenThroughIngest(t *testing.T) {
	ctx := context.Background()
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.declareAck()
	f.drainAll(t)
	applied := f.vramDerivedETag(t)

	// One agent, reporting through the real ingest core on the real telemetry
	// cadence, compressed: every sample carries both specs as stopped and the
	// digest of the document it has applied.
	body := `{"agent_version":"0.3.0","reported_at":"2026-09-02T10:00:00Z","os":"linux","arch":"amd64",` +
		`"host":{"cpu_util_pct":10},"capabilities":{"features":["runtime_manager","runtime_config_ack"]},` +
		`"runtime_config_applied_etag":"` + applied + `",` +
		`"runtimes":[{"spec_id":"rspec_target","state":"stopped"},{"spec_id":"rspec_sib","state":"stopped"}]}`
	req, raw := ingestReq(t, body)
	// Once synchronously, so the isolation wait below is not racing the ticker
	// for its first frame.
	if err := f.srv.ingestTelemetrySample(ctx, "srv1", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				_ = f.srv.ingestTelemetrySample(ctx, "srv1", req, raw)
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done })

	// Kept to three seconds rather than the 30 the direct fast-path test uses:
	// this test's failure mode is an exhausted bound, and the bound is what a
	// regression would cost the suite.
	bind := 3 * time.Second
	start := time.Now()
	res := f.srv.vramAwaitIsolation(ctx, "srv1", []string{f.siblingSpec, f.targetSpec}, map[string]bool{},
		vramIsolationProofPolicy{acknowledged: true, bindDelay: bind})
	elapsed := time.Since(start)

	if !res.confirmed {
		t.Fatalf("an acknowledgement carried over the wire did not confirm; reason = %q, evidence = %v",
			res.reason, res.evidence)
	}
	if elapsed >= bind {
		t.Fatalf("confirmed only after %v: the acknowledgement did not reach the wait", elapsed)
	}
}

// TestIngestRecordsTheAcknowledgementBeforePublishingTheFrame pins the one
// ordering ingestTelemetrySample itself calls "a contract, not tidiness": the
// applied-config ETag is recorded BEFORE the status snapshot is published.
//
// What the order buys: the isolation wait is WOKEN by a published frame and
// then reads the acknowledgement registry, so recording first is what makes
// "the acknowledgement is at least as fresh as the frame that woke me" true.
// Reversed, a frame can reach a subscriber that then reads the PREVIOUS
// acknowledgement, drops the frame as inadmissible, and waits for the next
// sample -- which costs a telemetry interval every time and, on a run whose
// only remaining frame was that one, the whole bound.
//
// IT IS A SOURCE-ORDER ASSERTION, and deliberately so, for the same reason
// cmd/gateway's TestPostgresDepsWiresTheCertCipher is: the property has no
// deterministic runtime observation. Both statements are non-blocking and
// adjacent, and publish's fan-out hands the frame to a channel rather than to
// a goroutine the test can synchronize with -- so a concurrent observer that
// wins the race under the reversed order does so by scheduling luck, and a
// test resting on that is either flaky or (far more likely, since a channel
// send does not preempt the sender) green against the very mutation it exists
// to catch. Swapping the two statements makes this test red, which is exactly
// the guarantee that was missing.
// TestVRAMIsolationConfirmsFromAnAcknowledgementDrivenThroughIngest covers the
// half that IS observable: that both facts reach the wait over the wire at all.
func TestIngestRecordsTheAcknowledgementBeforePublishingTheFrame(t *testing.T) {
	body := goFuncSource(t, "agent_ingest.go", "ingestTelemetrySample")
	const (
		record  = "s.RuntimeStatus.SetAppliedConfigETag(serverID,"
		publish = "s.RuntimeStatus.publish(serverID,"
	)
	recordAt := strings.Index(body, record)
	publishAt := strings.Index(body, publish)
	if recordAt < 0 {
		t.Fatalf("ingestTelemetrySample no longer contains %q: the acknowledgement is not recorded on the ingest path at all", record)
	}
	if publishAt < 0 {
		t.Fatalf("ingestTelemetrySample no longer contains %q", publish)
	}
	if recordAt > publishAt {
		t.Fatalf("ingestTelemetrySample publishes the status frame BEFORE recording the acknowledgement: a subscriber woken by that frame can read the previous acknowledgement (offsets: record %d, publish %d)",
			recordAt, publishAt)
	}
}

// goFuncSource returns the source text of the named function or method in the
// given file of THIS package -- the technique cmd/gateway/main_test.go's
// funcSource uses, widened to methods (the receiver is not matched: no name in
// this package is declared twice).
func goFuncSource(t *testing.T, file, name string) string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, decl := range parsed.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != name || fd.Body == nil {
			continue
		}
		return string(src[fset.Position(fd.Body.Pos()).Offset:fset.Position(fd.Body.End()).Offset])
	}
	t.Fatalf("function %s not found in %s", name, file)
	return ""
}
