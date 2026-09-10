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
	"sync"
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
