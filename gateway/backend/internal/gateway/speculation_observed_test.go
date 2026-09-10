// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"fmt"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// capabilityCallCounter wraps the test server's real routing.Store and COUNTS
// the two capability calls the speculation writer can make. Counting is the
// point: the once-per-mapping-per-lifetime property is about CALLS, not about
// the row -- a writer that re-wrote the identical row on every completion
// would leave the stored row indistinguishable from a correct one, so a test
// that only read the row back would pass against exactly the defect these
// tests exist to catch.
//
// Every other method is the embedded store's own, so the reads and writes
// that do happen hit the real MemoryStore and the row can be verified for
// real (through counter.Store, which does not count).
type capabilityCallCounter struct {
	routing.Store
	mu     sync.Mutex
	reads  int
	writes int
}

func (c *capabilityCallCounter) MappingCapabilities(ctx context.Context, mappingID string) ([]routing.CapabilityRow, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.Store.MappingCapabilities(ctx, mappingID)
}

func (c *capabilityCallCounter) UpsertMappingCapabilities(ctx context.Context, mappingID string, rows []routing.CapabilityRow) error {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return c.Store.UpsertMappingCapabilities(ctx, mappingID, rows)
}

func (c *capabilityCallCounter) counts() (reads, writes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads, c.writes
}

// speculationTestServer is a test server whose capability calls are counted.
func speculationTestServer(t *testing.T) (*Server, *capabilityCallCounter) {
	t.Helper()
	srv := NewTestServer()
	calls := &capabilityCallCounter{Store: srv.Routes}
	srv.Routes = calls
	return srv, calls
}

// speculatingCompletion drives ONE completion through recordUsage on the
// seeded mapping, with draftTokens as what the upstream reported. The target
// deliberately leaves OpportunisticMetrics unset: a capability verdict is not
// a metric, so it must not inherit that application's metric opt-in (see
// routing.MappingStore.UpsertMappingCapabilities on why capability writes
// carry no metrics gate at all).
//
// Safe to call from several goroutines: it only ever calls t.Helper().
func speculatingCompletion(t *testing.T, srv *Server, draftTokens int, id string) {
	t.Helper()
	target := routing.Target{RouteID: seedMappingID, ServerID: "mock-host-comp"}
	resp := provider.Response{Usage: inference.Usage{
		InputTokens: 10, OutputTokens: 12, TotalTokens: 22, DraftTokens: draftTokens,
	}}
	srv.recordUsage(time.Now(), auth.Token{UserID: "usr_x"}, inference.Request{Model: "qwen-coder"},
		target, resp, "", "success", usageMeta{}, id, nil)
}

// speculationRow reads the seeded mapping's speculation_observed row straight
// from the underlying store, so verifying the row never disturbs the counts.
func speculationRow(t *testing.T, store routing.Store) (routing.CapabilityRow, bool) {
	t.Helper()
	rows, err := store.MappingCapabilities(context.Background(), seedMappingID)
	if err != nil {
		t.Fatalf("mapping capabilities: %v", err)
	}
	row, ok := routing.CapabilityRowsByName(rows)[routing.CapabilitySpeculationObserved]
	return row, ok
}

// A completion whose usage reported drafted tokens records the observation as
// a capability row on the SERVING mapping (Target.RouteID -- the same id the
// usage event is attributed to), with the provenance of the document it was
// read from and nothing else.
func TestRecordUsageWritesTheSpeculationVerdictOnDraftedTokens(t *testing.T) {
	srv, calls := speculationTestServer(t)

	speculatingCompletion(t, srv, 7, "req_spec_first")

	row, ok := speculationRow(t, calls.Store)
	if !ok {
		t.Fatalf("no %q row after a completion reporting 7 drafted tokens -- the observation is the whole feature", routing.CapabilitySpeculationObserved)
	}
	if row.Verdict != routing.CapabilityYes {
		t.Fatalf("verdict = %q, want %q (the only verdict this source can honestly carry)", row.Verdict, routing.CapabilityYes)
	}
	if row.Source != routing.CapabilitySourceLlamaCppTimings {
		t.Fatalf("source = %q, want %q -- provenance must name the document it read, not a probe that never ran", row.Source, routing.CapabilitySourceLlamaCppTimings)
	}
	if row.CheckedAt.IsZero() {
		t.Fatal("CheckedAt is zero -- an operator reading the tooltip needs to know WHEN this was observed")
	}
	reads, writes := calls.counts()
	if writes != 1 {
		t.Fatalf("capability writes = %d, want exactly 1", writes)
	}
	if reads != 1 {
		t.Fatalf("capability reads = %d, want exactly 1 (the rank comparison needs the stored row once, on the first sighting only)", reads)
	}
}

// The once-per-mapping-per-lifetime property, asserted on the CALL COUNT: a
// second completion on the same mapping must not query the store at all --
// neither the read that would feed the rank comparison nor the write itself.
// Asserting the row instead would prove nothing here: a re-written identical
// row looks exactly like an untouched one.
func TestRecordUsageWritesTheSpeculationVerdictOncePerMappingPerLifetime(t *testing.T) {
	srv, calls := speculationTestServer(t)

	speculatingCompletion(t, srv, 7, "req_spec_first")
	speculatingCompletion(t, srv, 9, "req_spec_second")

	reads, writes := calls.counts()
	if writes != 1 {
		t.Fatalf("capability writes after two speculating completions = %d, want 1 -- every relayed completion would otherwise re-write the same row forever", writes)
	}
	if reads != 1 {
		t.Fatalf("capability reads after two speculating completions = %d, want 1 -- the in-memory set exists so a recorded mapping costs NO query per request", reads)
	}
}

// Zero drafted tokens is "no evidence", never "does not speculate": the
// upstream emits the key only when it is > 0, so absence is equally what a
// cache hit, a short completion or a non-llama.cpp upstream produce. Such a
// completion must not reach the store at all -- neither with a "no" verdict
// (there is no honest negative) nor with a read that costs a query per
// request on every non-speculating mapping in the fleet.
func TestRecordUsageWithoutDraftedTokensNeverTouchesTheCapabilityStore(t *testing.T) {
	srv, calls := speculationTestServer(t)

	speculatingCompletion(t, srv, 0, "req_spec_none")

	reads, writes := calls.counts()
	if reads != 0 || writes != 0 {
		t.Fatalf("capability reads/writes = %d/%d, want 0/0 -- a mapping that never speculates must never be touched", reads, writes)
	}
	if row, ok := speculationRow(t, calls.Store); ok {
		t.Fatalf("row %+v written with no drafted tokens -- absence of the key is NOT evidence, and a %q here would be a permanent false claim", row, routing.CapabilityNo)
	}
}

// The rank rule, exercised end to end through the writer rather than argued
// from routing's unit tests: an operator's verdict (rank 3) outranks this
// source (rank 1, via capabilitySourceRank's default branch), so the
// observation is dropped BEFORE the store write -- and the write is skipped
// entirely rather than issued and ignored, because nothing in the store
// applies the rule for a caller.
func TestRecordUsageNeverOverwritesAManualSpeculationVerdict(t *testing.T) {
	srv, calls := speculationTestServer(t)
	// Deliberately the OPPOSITE verdict: an operator who has answered "this
	// deployment does not speculate" is exactly whose answer must survive a
	// contradicting observation.
	manual := routing.CapabilityRow{
		Capability: routing.CapabilitySpeculationObserved,
		Verdict:    routing.CapabilityNo,
		Source:     routing.CapabilitySourceManual,
		CheckedAt:  time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC),
	}
	if err := calls.Store.UpsertMappingCapabilities(context.Background(), seedMappingID, []routing.CapabilityRow{manual}); err != nil {
		t.Fatalf("seed manual row: %v", err)
	}

	speculatingCompletion(t, srv, 7, "req_spec_manual")

	row, ok := speculationRow(t, calls.Store)
	if !ok {
		t.Fatal("the manual row disappeared")
	}
	if row.Verdict != routing.CapabilityNo || row.Source != routing.CapabilitySourceManual {
		t.Fatalf("row = %+v, want the operator's %q/%q untouched", row, routing.CapabilityNo, routing.CapabilitySourceManual)
	}
	if _, writes := calls.counts(); writes != 0 {
		t.Fatalf("capability writes = %d, want 0 -- the rank rule must be asked (routing.WritableCapabilityRows) before the store call, not after", writes)
	}
}

// Concurrency: several completions on the same mapping can land at once (that
// is the normal shape of a busy gateway, and the first-ever sighting is
// exactly when they all see an unrecorded mapping). Exactly one of them may
// reach the store.
func TestRecordUsageWritesTheSpeculationVerdictOnceUnderConcurrentCompletions(t *testing.T) {
	srv, calls := speculationTestServer(t)
	const completions = 8

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range completions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			speculatingCompletion(t, srv, 7, fmt.Sprintf("req_spec_conc_%d", i))
		}(i)
	}
	close(start)
	wg.Wait()

	reads, writes := calls.counts()
	if writes != 1 {
		t.Fatalf("capability writes from %d concurrent speculating completions = %d, want 1", completions, writes)
	}
	if reads != 1 {
		t.Fatalf("capability reads from %d concurrent speculating completions = %d, want 1 -- the set must be CLAIMED under the lock, not merely consulted", completions, reads)
	}
	if row, ok := speculationRow(t, calls.Store); !ok || row.Source != routing.CapabilitySourceLlamaCppTimings {
		t.Fatalf("row = %+v (present=%v), want one %q verdict", row, ok, routing.CapabilitySourceLlamaCppTimings)
	}
}

// The agent trust boundary, for the one capability this gateway observes
// ITSELF. reservedAgentCapabilityNames must contain
// routing.CapabilitySpeculationObserved, so a verdict carrying that name in
// an agent's OPEN verdict list never becomes a row -- and the direction that
// matters is not the obvious one.
//
// A "no" from the open list ties the gateway's own row at rank 1 (both
// llama_cpp_props and llama_cpp_timings take capabilitySourceRank's default),
// and a tie is writable by design, because for a CADENCE-driven writer the
// one that lost repairs its own row on the next tick. This writer has no next
// tick: claimSpeculationObserved holds the mapping for the whole process
// lifetime, so once overwritten the false verdict stands until a restart.
// That combination -- rank-1 writable, never rewritten -- is why the name
// belongs on the list rather than merely being unlikely to arrive.
//
// It is NOT unique to this capability, and an earlier wording of this
// comment said it was: "mtp"'s rank-1 "legacy" row is written only for a
// brand-new mapping (portal.legacyMTPCapabilityRow, at both creation sites)
// and nothing re-derives it afterwards, so it has the same shape -- which is
// why that name is on the same list. What differs is the repair left over,
// not the shape: an operator's rank-3 "manual" row is reachable from the
// mapping form for "mtp" and not for this capability, whose only routine
// repair is the next speculating completion after a restart.
//
// Both verdicts are refused, and each subtest is falsifiable in its own way,
// because the two would-be harms are different:
//
//   - "no" over a real observation is the OVERWRITE. The mapping is seeded
//     with exactly the row the gateway writes (yes/llama_cpp_timings), so a
//     row that got through would flip a stored verdict and be visible.
//   - "yes" with nothing on file is the FABRICATION -- the "mtp" harm
//     verbatim, an unvetted publisher string presented to the operator as an
//     attested capability. Nothing is seeded, so a row that got through
//     would appear from nowhere and be visible. (Seeding here would hide it:
//     rule 2 of routing.WritableCapabilityRows compares verdict and rank
//     only, so an agent "yes" over the gateway's "yes" is dropped as
//     unchanged whatever this list says.)
//
// And "nothing was written" cannot pass vacuously in either subtest: the
// same sample carries a "vision" verdict, which must still land -- the
// reserved rule drops rows, never the pass.
func TestIngestDropsAnAgentReportedSpeculationVerdict(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, spec, verdict string
		seedObservation     bool
	}{
		{"a false negative would overwrite the gateway's own observation, permanently", "rspec_spec_no", routing.CapabilityNo, true},
		{"an unvetted positive would fabricate one out of a publisher's string", "rspec_spec_yes", routing.CapabilityYes, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := withCapturedSlogAtTheDefaultLevel(t)
			srv := NewTestServer()
			seedRuntimeIngestSpec(t, srv, tc.spec, false)
			mappingID := "map_" + tc.spec
			if tc.seedObservation {
				seedCapabilityRow(t, srv, mappingID, routing.CapabilitySpeculationObserved,
					routing.CapabilityYes, routing.CapabilitySourceLlamaCppTimings)
			}
			counting := countingRowStore(srv)

			req, raw := ingestReq(t, capabilitiesBody(tc.spec,
				`{"verdicts":[{"name":"`+routing.CapabilitySpeculationObserved+`","verdict":"`+tc.verdict+`"},`+
					`{"name":"vision","verdict":"yes"}],"source":"`+routing.CapabilitySourceLlamaCppProps+`"}`))
			if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
				t.Fatalf("ingest: %v", err)
			}

			if sent := counting.lastSent(); len(sent) != 1 || sent[0].Capability != routing.CapabilityVision {
				t.Fatalf("the write carried %+v, want exactly the vision row -- an agent-reported %q must never reach the store, and the rest of the pass must still land", sent, routing.CapabilitySpeculationObserved)
			}
			if tc.seedObservation {
				assertCapabilityRow(t, srv, mappingID, routing.CapabilitySpeculationObserved,
					routing.CapabilityYes, routing.CapabilitySourceLlamaCppTimings)
			} else if row, ok := capabilityRow(t, srv, mappingID, routing.CapabilitySpeculationObserved); ok {
				t.Fatalf("a %q row exists (%+v), want none -- only a relayed completion's own drafted tokens may create one", routing.CapabilitySpeculationObserved, row)
			}
			assertCapabilityRow(t, srv, mappingID, routing.CapabilityVision,
				routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
			if recs := buf.Snapshot(); !findLogRecord(recs, "WARN", "reserved internal capability names") {
				t.Fatalf("no WARN record naming the reserved drop at the gateway's own default level (info); records = %+v -- a dropped write that cannot heal on its own must be readable in a default deployment", recs)
			}
		})
	}
}

// The claim's ATOMICITY, which is a different property from the claim's
// existence: TestRecordUsage...OnceUnderConcurrentCompletions pins that the
// set is consulted at all (remove the guard and its read count goes to 8),
// but it cannot see a claim split into a non-atomic check-then-set, because
// recordUsage puts several serializing mutexes in front of the window and
// two completions effectively never land inside it.
//
// So this test does not go through recordUsage. It calls the unexported
// claimSpeculationObserved directly -- 64 goroutines released at once by
// close(start), against a fresh zero-valued Server per iteration, which is
// also what pins that the lazily created map is nil-safe from &Server{}.
//
// It cannot false-FAIL: one critical section makes a second "true" for the
// same id impossible regardless of timing or machine, so a correct
// implementation passes at any iteration count. Its weakness is the
// false-PASS direction, which is the acceptable one and is what the
// iteration count buys down -- measured against a split claim, this
// configuration detects it in 5 runs out of 6, typically within the first
// dozen iterations, for about half a second.
func TestClaimSpeculationObservedIsAtomicUnderConcurrentFirstSightings(t *testing.T) {
	const goroutines, iterations = 64, 3000
	for i := range iterations {
		srv := &Server{}
		var claims atomic.Int32
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range goroutines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if srv.claimSpeculationObserved(seedMappingID) {
					claims.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if got := claims.Load(); got != 1 {
			t.Fatalf("iteration %d: %d of %d goroutines claimed the SAME unclaimed mapping, want exactly 1 -- the lookup and the insert must happen in ONE critical section, or the first sighting costs one store read per goroutine that got through", i, got, goroutines)
		}
	}
}

// An observation dropped because it was outranked leaves a record. Being
// outranked is the normal outcome once an operator has answered, so this is
// Debug rather than Warn -- but it must exist at some level, because this
// writer holds its claim for the process's lifetime and therefore never
// re-derives what it dropped. Without the line, an operator asking why the
// chip never appeared for a mapping that demonstrably speculates has nothing
// anywhere to read.
//
// The capture is at Debug on purpose here, unlike the reserved-name test
// above: Debug is the RIGHT level for this drop, so a test capturing at info
// would be asserting the wrong thing.
func TestRecordUsageLogsASpeculationObservationItCannotWrite(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	defer restore()
	srv, calls := speculationTestServer(t)
	manual := routing.CapabilityRow{
		Capability: routing.CapabilitySpeculationObserved,
		Verdict:    routing.CapabilityNo,
		Source:     routing.CapabilitySourceManual,
		CheckedAt:  time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC),
	}
	if err := calls.Store.UpsertMappingCapabilities(context.Background(), seedMappingID, []routing.CapabilityRow{manual}); err != nil {
		t.Fatalf("seed manual row: %v", err)
	}

	speculatingCompletion(t, srv, 7, "req_spec_outranked_log")

	recs := buf.Snapshot()
	for _, r := range recs {
		if r.Level != "DEBUG" || !strings.Contains(r.Msg, "no writable row") {
			continue
		}
		if r.Attrs["mapping"] != seedMappingID {
			t.Fatalf("the record names mapping %v, want %q -- a line an operator cannot attribute to a mapping is not a diagnostic", r.Attrs["mapping"], seedMappingID)
		}
		if r.Attrs["capability"] != routing.CapabilitySpeculationObserved {
			t.Fatalf("the record names capability %v, want %q", r.Attrs["capability"], routing.CapabilitySpeculationObserved)
		}
		return
	}
	t.Fatalf("no DEBUG record about the dropped observation; records = %+v", recs)
}
