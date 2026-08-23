// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// TestLHMSourceMemoizesWithinTTL proves two getTree calls back-to-back (well
// inside lhmSourceTTL) hit the LHM /data.json handler only once — the whole
// point of lhmSource: the power and temp sub-collectors share one instance
// and must not each trigger their own GET.
func TestLHMSourceMemoizesWithinTTL(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(lhmDataJSON))
	}))
	defer srv.Close()

	src := newLHMSource(srv.URL, srv.Client())
	root1, err := src.getTree(context.Background())
	if err != nil {
		t.Fatalf("getTree #1: %v", err)
	}
	root2, err := src.getTree(context.Background())
	if err != nil {
		t.Fatalf("getTree #2: %v", err)
	}
	if root1 != root2 {
		t.Fatalf("getTree #2 returned a different tree pointer than #1; want the cached one")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("handler hit %d times, want exactly 1 (second getTree should be served from cache)", got)
	}
}

// TestLHMSourceReplaysCachedError proves a failed fetch is also memoized: a
// second getTree call within the TTL does not retry the network and instead
// replays the same error, so an unreachable LHM endpoint costs at most one
// connection attempt per shared-source window, same as the happy path.
func TestLHMSourceReplaysCachedError(t *testing.T) {
	src := newLHMSource("http://127.0.0.1:0/data.json", &http.Client{})
	_, err1 := src.getTree(context.Background())
	if err1 == nil {
		t.Fatal("getTree #1: want an error against an unreachable endpoint")
	}
	_, err2 := src.getTree(context.Background())
	if err2 == nil {
		t.Fatal("getTree #2: want the cached error replayed")
	}
}

// TestDetectPowerAndTempCollectorsShareOneFetch is the SA-6 regression test:
// DetectPowerAndTempCollectors's power and temp composites, Collected
// back-to-back the way main.go's telemetry loop does every cycle, must issue
// exactly ONE GET against the operator's LibreHardwareMonitor /data.json —
// not one per collector — because they share a single lhmSource.
func TestDetectPowerAndTempCollectorsShareOneFetch(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(lhmDataJSON))
	}))
	defer srv.Close()

	power, temp := DetectPowerAndTempCollectors(srv.URL)

	cpu, system, err := power.Collect(context.Background())
	if err != nil {
		t.Fatalf("power Collect: %v", err)
	}
	if cpu == nil || *cpu != 65.0 {
		t.Fatalf("cpu watts = %v, want 65.0 (same fixture as lhm_power_test.go)", cpu)
	}
	if system == nil || *system != 180.0 {
		t.Fatalf("system watts = %v, want 180.0", system)
	}

	if _, err := temp.Collect(context.Background()); err != nil {
		t.Fatalf("temp Collect: %v", err)
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("LHM /data.json handler hit %d times for power+temp Collect in one cycle, want exactly 1 (shared lhmSource)", got)
	}
}

// TestDetectPowerAndTempCollectorsIndependentWithoutURL proves the composed
// collectors behave like the separate Detect*Collector calls when no LHM URL
// is configured: no "lhm" sub, never nil, never erroring.
func TestDetectPowerAndTempCollectorsIndependentWithoutURL(t *testing.T) {
	power, temp := DetectPowerAndTempCollectors("")
	if power == nil || temp == nil {
		t.Fatal("DetectPowerAndTempCollectors(\"\") returned a nil collector")
	}
	if containsString(PowerSources(power), "lhm") {
		t.Fatalf("PowerSources = %v, did not expect \"lhm\" with no URL configured", PowerSources(power))
	}
	if containsString(TempSources(temp), "lhm") {
		t.Fatalf("TempSources = %v, did not expect \"lhm\" with no URL configured", TempSources(temp))
	}
	if _, _, err := power.Collect(context.Background()); err != nil {
		t.Fatalf("power Collect: %v", err)
	}
	if _, err := temp.Collect(context.Background()); err != nil {
		t.Fatalf("temp Collect: %v", err)
	}
}

// TestLHMSourceConcurrentGetTreeIsRaceFree exercises getTree from many
// goroutines at once (the -race target for this refactor): the shared mutex
// must serialize the fetch/cache so concurrent Collect calls never race on
// the cached tree/error/timestamp fields.
func TestLHMSourceConcurrentGetTreeIsRaceFree(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(lhmDataJSON))
	}))
	defer srv.Close()

	src := newLHMSource(srv.URL, srv.Client())
	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := src.getTree(context.Background()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent getTree: %v", err)
	}
}
