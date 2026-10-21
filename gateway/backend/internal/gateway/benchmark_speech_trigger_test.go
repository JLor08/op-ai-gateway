// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"op-ai-gateway/internal/routing"
	"sync"
	"testing"
	"time"
)

// TriggerSpeechRateBenchmark is how the health loop starts the speech rate benchmark
// (#263): the programmatic twin of POST .../benchmark?mode=speech for ONE mapping, with
// no principal, through the registry's one-run-per-server slot, asynchronously. These
// tests drive it against the same audio.cpp-shaped stub as the manual benchmark.

// speechTriggerEntities reads the server, application and mapping a health tick would
// hand the trigger, from the fixture's store.
func speechTriggerEntities(t *testing.T, s *Server, mappingID string) (routing.AIServer, routing.Application, routing.ModelMapping) {
	t.Helper()
	ctx := context.Background()
	mp, err := s.Routes.MappingByID(ctx, mappingID)
	if err != nil {
		t.Fatalf("MappingByID(%s): %v", mappingID, err)
	}
	app, err := s.Routes.ApplicationByID(ctx, mp.ApplicationID)
	if err != nil {
		t.Fatalf("ApplicationByID(%s): %v", mp.ApplicationID, err)
	}
	server, err := s.Routes.AIServerByID(ctx, app.ServerID)
	if err != nil {
		t.Fatalf("AIServerByID(%s): %v", app.ServerID, err)
	}
	return server, app, mp
}

func speechTrigger(t *testing.T, s *Server, mappingID, identity string) bool {
	t.Helper()
	server, app, mp := speechTriggerEntities(t, s, mappingID)
	return s.TriggerSpeechRateBenchmark(context.Background(), server, app, mp, identity)
}

// speechIdentityRows reads a mapping's companion identity cache rows.
func speechIdentityRows(t *testing.T, s *Server, mappingID string) []routing.ModelMappingCacheRow {
	t.Helper()
	rows, err := s.Routes.ModelMappingCache(context.Background(), mappingID, routing.ModelMappingCacheOutputByteRateModel)
	if err != nil {
		t.Fatalf("ModelMappingCache(%s, identity): %v", mappingID, err)
	}
	return rows
}

// orderedCacheWrites wraps the route store and records, in order, the cache type of
// every UpsertModelMappingCache call.
type orderedCacheWrites struct {
	routing.Store
	mu    sync.Mutex
	order []string
}

func (o *orderedCacheWrites) UpsertModelMappingCache(ctx context.Context, row routing.ModelMappingCacheRow) error {
	o.mu.Lock()
	o.order = append(o.order, row.CacheType)
	o.mu.Unlock()
	return o.Store.UpsertModelMappingCache(ctx, row)
}

func (o *orderedCacheWrites) writes() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.order...)
}

// A trigger on a plain speech mapping starts exactly one speech run on its server:
// one non-stream synthesis, the rate cached under the mapping id (the id the streamed
// relay reads it by), a kind=speech history row, and the loaded-model identity the
// health loop observed recorded beside the rate -- written AFTER it, so a crash
// between the two leaves a rate with no identity (the health loop adopts the current
// one), never an identity vouching for a rate that was not written.
func TestTriggerSpeechRateBenchmarkRunsOneBenchmarkAndRecordsTheIdentity(t *testing.T) {
	up := newSpeechBenchUpstream(t, answerWAV(buildPCMWav(t, 24000, 1, 16, 48000), true))
	s := newSpeechBenchFixture(t, up)
	writes := &orderedCacheWrites{Store: s.Routes}
	s.Routes = writes

	if !speechTrigger(t, s, "sb_tts", "/models/tts-a.gguf") {
		t.Fatal("TriggerSpeechRateBenchmark = false, want true: an idle server with a plain speech mapping starts a run")
	}
	st := finishedSpeechRun(t, s)
	if st.Mode != "speech" || st.Scope != "speech-rate" || st.Error != "" || st.Total != 1 || st.Done != 1 {
		t.Fatalf("run = mode %q scope %q err %q total %d done %d, want a finished one-mapping speech run in scope speech-rate", st.Mode, st.Scope, st.Error, st.Total, st.Done)
	}
	if res := speechBenchResult(t, st, "sb_tts"); res.Error != "" || res.OutputByteRate != 48000 {
		t.Fatalf("result = %+v, want a measured result with output byte rate 48000", res)
	}
	if rate, ok := s.cachedOutputByteRate(context.Background(), "sb_tts"); !ok || rate != 48000 {
		t.Fatalf("cachedOutputByteRate(sb_tts) = (%d, %v), want (48000, true)", rate, ok)
	}
	rows := speechIdentityRows(t, s, "sb_tts")
	if len(rows) != 1 || rows[0].ScopeKey != "" || rows[0].Payload != "/models/tts-a.gguf" || rows[0].UpdatedAt.IsZero() {
		t.Fatalf("identity cache rows = %+v, want one scope-\"\" row holding the observed identity", rows)
	}
	if got, want := writes.writes(), []string{routing.ModelMappingCacheOutputByteRate, routing.ModelMappingCacheOutputByteRateModel}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("cache writes in order = %v, want %v (the rate first, then the identity it was measured under)", got, want)
	}
	if calls := up.received(); len(calls) != 1 || calls[0].path != "/v1/audio/speech" || calls[0].body["model"] != sbTTSModel {
		t.Fatalf("upstream calls = %+v, want exactly one synthesis of %q", calls, sbTTSModel)
	}
	if runs := speechBenchRuns(t, s, "sb_tts"); len(runs) != 1 || runs[0].Kind != "speech" || runs[0].Error != "" {
		t.Fatalf("history = %+v, want one successful kind=speech row", runs)
	}
	// The payload of the rate itself is untouched by the companion: still the plain
	// decimal the streamed relay parses.
	if rows := speechBenchRateRows(t, s, "sb_tts"); len(rows) != 1 || rows[0].Payload != "48000" {
		t.Fatalf("rate rows = %+v, want one plain-decimal row", rows)
	}
}

// A run already in flight on the server owns its one slot: a second trigger starts
// nothing, answers false, and does not disturb the first (one synthesis, not two).
func TestTriggerSpeechRateBenchmarkDoesNotStartASecondRun(t *testing.T) {
	release := make(chan struct{})
	inner := answerWAV(buildPCMWav(t, 24000, 1, 16, 48000), false)
	up := newSpeechBenchUpstream(t, func(w http.ResponseWriter, model string) {
		<-release
		inner(w, model)
	})
	s := newSpeechBenchFixture(t, up)

	if !speechTrigger(t, s, "sb_tts", "/models/a") {
		t.Fatal("first trigger = false, want true")
	}
	if !s.Benchmarks.ServerBusy(sbServer) {
		t.Fatal("the server is not reserved while the run is in flight")
	}
	if speechTrigger(t, s, "sb_tts", "/models/a") {
		t.Fatal("second trigger = true while a run is in flight, want false (the one-run-per-server slot is taken)")
	}
	close(release)
	st := finishedSpeechRun(t, s)
	if st.Error != "" || st.Total != 1 {
		t.Fatalf("run = %+v, want the first run, untouched, finished", st)
	}
	if calls := up.received(); len(calls) != 1 {
		t.Fatalf("upstream calls = %d, want exactly one synthesis", len(calls))
	}
	// The slot is free again: a later trigger starts a new run.
	if !speechTrigger(t, s, "sb_tts", "/models/a") {
		t.Fatal("trigger after the run finished = false, want true")
	}
	finishedSpeechRun(t, s)
}

// The trigger is no wider than the manual benchmark: a clone model (a plain request
// 500s without a reference voice), a transcriber and a mapping with no speech verdict
// start nothing -- no reservation, no request, no rows.
func TestTriggerSpeechRateBenchmarkSkipsMappingsThatAreNotPlainSpeech(t *testing.T) {
	up := newSpeechBenchUpstream(t, answerWAV(buildPCMWav(t, 24000, 1, 16, 48000), false))
	s := newSpeechBenchFixture(t, up)
	for _, id := range []string{"sb_clone", "sb_asr"} {
		if speechTrigger(t, s, id, "/models/"+id) {
			t.Fatalf("trigger on %s = true, want false: not a plain text-to-speech mapping", id)
		}
		if s.Benchmarks.ServerBusy(sbServer) || s.Benchmarks.Status(sbServer).Running {
			t.Fatalf("a refused trigger on %s reserved the server: %+v", id, s.Benchmarks.Status(sbServer))
		}
		if rows := speechBenchRateRows(t, s, id); len(rows) != 0 {
			t.Fatalf("%s gained a rate row: %+v", id, rows)
		}
		if rows := speechIdentityRows(t, s, id); len(rows) != 0 {
			t.Fatalf("%s gained an identity row: %+v", id, rows)
		}
	}
	if calls := up.received(); len(calls) != 0 {
		t.Fatalf("upstream calls = %+v, want none", calls)
	}
}

// A failed synthesis writes neither half: the rate already cached and the identity it
// was measured under both stay as they were. A new identity recorded over an old rate
// would vouch for a rate nobody re-measured, which is exactly the stale bill the
// identity exists to prevent.
func TestTriggerSpeechRateBenchmarkFailureLeavesTheCacheAlone(t *testing.T) {
	up := newSpeechBenchUpstream(t, func(w http.ResponseWriter, _ string) {
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	})
	s := newSpeechBenchFixture(t, up)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, row := range []routing.ModelMappingCacheRow{
		{MappingID: "sb_tts", CacheType: routing.ModelMappingCacheOutputByteRate, Payload: "88200", UpdatedAt: now},
		{MappingID: "sb_tts", CacheType: routing.ModelMappingCacheOutputByteRateModel, Payload: "/models/old", UpdatedAt: now},
	} {
		if err := s.Routes.UpsertModelMappingCache(ctx, row); err != nil {
			t.Fatalf("seed %s: %v", row.CacheType, err)
		}
	}

	if !speechTrigger(t, s, "sb_tts", "/models/new") {
		t.Fatal("trigger = false, want true: the run starts, and it is the synthesis that fails")
	}
	st := finishedSpeechRun(t, s)
	if res := speechBenchResult(t, st, "sb_tts"); res.Error == "" {
		t.Fatalf("result = %+v, want the synthesis failure recorded", res)
	}
	if rate, ok := s.cachedOutputByteRate(ctx, "sb_tts"); !ok || rate != 88200 {
		t.Fatalf("cached rate after a failed run = (%d, %v), want the earlier 88200 untouched", rate, ok)
	}
	if rows := speechIdentityRows(t, s, "sb_tts"); len(rows) != 1 || rows[0].Payload != "/models/old" {
		t.Fatalf("identity rows after a failed run = %+v, want the earlier identity untouched", rows)
	}
}

// A trigger with no identity (the model's file is not stated) records no identity
// row: an empty identity says nothing, and it must not overwrite one that does.
func TestTriggerSpeechRateBenchmarkWithoutAnIdentityRecordsNone(t *testing.T) {
	up := newSpeechBenchUpstream(t, answerWAV(buildPCMWav(t, 24000, 1, 16, 48000), false))
	s := newSpeechBenchFixture(t, up)
	if err := s.Routes.UpsertModelMappingCache(context.Background(), routing.ModelMappingCacheRow{
		MappingID: "sb_tts", CacheType: routing.ModelMappingCacheOutputByteRateModel, Payload: "/models/kept", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	if !speechTrigger(t, s, "sb_tts", "") {
		t.Fatal("trigger = false, want true")
	}
	finishedSpeechRun(t, s)
	if rate, ok := s.cachedOutputByteRate(context.Background(), "sb_tts"); !ok || rate != 48000 {
		t.Fatalf("cached rate = (%d, %v), want (48000, true)", rate, ok)
	}
	if rows := speechIdentityRows(t, s, "sb_tts"); len(rows) != 1 || rows[0].Payload != "/models/kept" {
		t.Fatalf("identity rows = %+v, want the stored identity left as it was", rows)
	}
}

// The manual benchmark (POST ...?mode=speech) knows no loaded-model identity, so it
// records none. The health loop adopts the identity it observes for a rate that
// carries none.
func TestManualSpeechBenchmarkRecordsNoIdentity(t *testing.T) {
	up := newSpeechBenchUpstream(t, answerWAV(buildPCMWav(t, 24000, 1, 16, 48000), false))
	s := newSpeechBenchFixture(t, up)
	if status, code, body := postSpeechBenchmark(t, s, "/api/portal/mappings/sb_tts/benchmark?mode=speech"); status != http.StatusAccepted {
		t.Fatalf("POST = %d %q (%s), want 202", status, code, body)
	}
	finishedSpeechRun(t, s)
	if rows := speechBenchRateRows(t, s, "sb_tts"); len(rows) != 1 {
		t.Fatalf("rate rows = %+v, want one", rows)
	}
	if rows := speechIdentityRows(t, s, "sb_tts"); len(rows) != 0 {
		t.Fatalf("identity rows = %+v, want none from a manual run", rows)
	}
}

// Like the scheduled benchmark, the trigger never competes with live traffic: it takes
// the reservation, sees in-flight requests on the server, gives the reservation back and
// answers false, so the health loop asks again on a later tick. Nothing is sent upstream.
func TestTriggerSpeechRateBenchmarkDefersWhileTheServerHasInFlightRequests(t *testing.T) {
	up := newSpeechBenchUpstream(t, answerWAV(buildPCMWav(t, 24000, 1, 16, 48000), false))
	s := newSpeechBenchFixture(t, up)
	s.Active.Add(ActiveRequest{ID: "req_live", ServerName: "Speech Host", APIFlavor: "openai", StartedAt: time.Now()})

	if speechTrigger(t, s, "sb_tts", "/models/a") {
		t.Fatal("trigger = true on a server with an in-flight request, want false")
	}
	if s.Benchmarks.ServerBusy(sbServer) {
		t.Fatal("the deferred trigger still holds the server's reservation")
	}
	if calls := up.received(); len(calls) != 0 {
		t.Fatalf("upstream calls = %+v, want none", calls)
	}
	s.Active.Remove("req_live")
	if !speechTrigger(t, s, "sb_tts", "/models/a") {
		t.Fatal("trigger once the server is idle = false, want true")
	}
	finishedSpeechRun(t, s)
}

// A Server with no route store or no registry has nothing to run on: false, no panic.
func TestTriggerSpeechRateBenchmarkWithoutStoreIsANoOp(t *testing.T) {
	if (&Server{}).TriggerSpeechRateBenchmark(context.Background(), routing.AIServer{ID: "s"}, routing.Application{ID: "a"}, routing.ModelMapping{ID: "m"}, "x") {
		t.Fatal("trigger on a bare Server = true, want false")
	}
}
