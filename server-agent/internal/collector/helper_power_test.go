// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeHelper serves a scriptable /v1/rapl on a Unix socket.
type fakeHelper struct {
	mu      sync.Mutex
	domains []HelperRAPLDomain
	status  int
	path    string
}

func newFakeHelper(t *testing.T) *fakeHelper {
	t.Helper()
	f := &fakeHelper{status: http.StatusOK}
	f.path = serveHelperSocket(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		st, d := f.status, f.domains
		f.mu.Unlock()
		if st != http.StatusOK {
			http.Error(w, "x", st)
			return
		}
		_ = json.NewEncoder(w).Encode(HelperRAPL{Domains: d})
	}))
	return f
}

func (f *fakeHelper) set(status int, d ...HelperRAPLDomain) {
	f.mu.Lock()
	f.status, f.domains = status, d
	f.mu.Unlock()
}

func dom(zone, name string, uj, maxUJ uint64) HelperRAPLDomain {
	return HelperRAPLDomain{Zone: zone, Name: name, EnergyUJ: uj, MaxEnergyRangeUJ: maxUJ}
}

type helperRig struct {
	f    *fakeHelper
	c    *helperPowerCollector
	base time.Time
	t    *testing.T
}

func newHelperRig(t *testing.T) *helperRig {
	f := newFakeHelper(t)
	c := newHelperPowerCollector(NewHelperHTTPClient(f.path))
	r := &helperRig{f: f, c: c, base: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC), t: t}
	c.now = func() time.Time { return r.base }
	return r
}

// collect advances the clock by sec seconds and collects.
func (r *helperRig) collect(sec int) (*float64, *float64) {
	r.t.Helper()
	r.base = r.base.Add(time.Duration(sec) * time.Second)
	cpu, sys, err := r.c.Collect(context.Background())
	noErr(r.t, err)
	return cpu, sys
}

const bigRange = 1 << 40

func TestHelperPowerWatts(t *testing.T) {
	r := newHelperRig(t)
	eq(t, "helper", r.c.Name())
	if !r.c.Available() {
		t.Fatal("Available() = false")
	}
	r.f.set(200, dom("intel-rapl:0", "package-0", 0, bigRange), dom("intel-rapl:1", "package-1", 0, bigRange),
		dom("intel-rapl:2", "psys", 0, bigRange), dom("intel-rapl:0:0", "core", 0, bigRange))
	cpu, sys := r.collect(0)
	wantNil(t, "cpu", cpu)
	wantNil(t, "sys", sys)
	r.f.set(200, dom("intel-rapl:0", "package-0", 10e6, bigRange), dom("intel-rapl:1", "package-1", 5e6, bigRange),
		dom("intel-rapl:2", "psys", 40e6, bigRange), dom("intel-rapl:0:0", "core", 99e6, bigRange))
	cpu, sys = r.collect(1)
	wantWatts(t, "cpu", cpu, 15)
	wantWatts(t, "sys", sys, 40)
}

func TestHelperPowerWraparound(t *testing.T) {
	r := newHelperRig(t)
	r.f.set(200, dom("z0", "package-0", 990e6, 1000e6))
	r.collect(0)
	r.f.set(200, dom("z0", "package-0", 5e6, 1000e6)) // (1000-990)+5 = 15 J
	cpu, _ := r.collect(1)
	wantWatts(t, "cpu", cpu, 15)
}

func TestHelperPowerFailureResetsBaseline(t *testing.T) {
	r := newHelperRig(t)
	r.f.set(200, dom("z0", "package-0", 0, bigRange))
	r.collect(0)
	r.f.set(500)
	cpu, sys := r.collect(1)
	wantNil(t, "cpu on failure", cpu)
	wantNil(t, "sys on failure", sys)
	r.f.set(200, dom("z0", "package-0", 100e6, bigRange))
	cpu, _ = r.collect(1)
	wantNil(t, "cpu first after failure (no delta across the gap)", cpu)
	r.f.set(200, dom("z0", "package-0", 110e6, bigRange))
	cpu, _ = r.collect(1)
	wantWatts(t, "cpu after fresh baseline", cpu, 10)
}

func TestHelperPowerZoneSetChange(t *testing.T) {
	r := newHelperRig(t)
	r.f.set(200, dom("intel-rapl:0", "package-0", 0, bigRange), dom("intel-rapl:1", "package-1", 0, bigRange))
	r.collect(0)
	r.f.set(200, dom("intel-rapl:0", "package-0", 10e6, bigRange)) // zone 1 vanishes
	cpu, _ := r.collect(1)
	wantWatts(t, "cpu without zone 1", cpu, 10)
	r.f.set(200, dom("intel-rapl:0", "package-0", 20e6, bigRange), dom("intel-rapl:1", "package-1", 500e6, bigRange))
	cpu, _ = r.collect(1)
	wantWatts(t, "cpu with zone 1 back (no delta for it)", cpu, 10)
	r.f.set(200, dom("intel-rapl:0", "package-0", 30e6, bigRange), dom("intel-rapl:1", "package-1", 505e6, bigRange))
	cpu, _ = r.collect(1)
	wantWatts(t, "cpu with both zones", cpu, 15)
}

func TestHelperPowerMissingSocketQuiet(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "hp")
	noErr(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	var buf bytes.Buffer
	c := newHelperPowerCollector(NewHelperHTTPClient(filepath.Join(dir, "none.sock")))
	c.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	for range 3 {
		cpu, sys, err := c.Collect(context.Background())
		noErr(t, err)
		if cpu != nil || sys != nil {
			t.Fatalf("got (%v, %v), want nils", cpu, sys)
		}
	}
	if buf.Len() != 0 {
		t.Fatalf("logged above Debug: %q", buf.String())
	}
}

func TestHelperPowerLogsInfoOnce(t *testing.T) {
	r := newHelperRig(t)
	var buf bytes.Buffer
	r.c.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	r.f.set(200, dom("z0", "package-0", 0, bigRange))
	r.collect(0)
	r.collect(1)
	if n := bytes.Count(buf.Bytes(), []byte("helper: serving RAPL counters")); n != 1 {
		t.Fatalf("info logged %d times, want 1: %q", n, buf.String())
	}
}

func TestHelperEnabled(t *testing.T) {
	origGOOS, origEuid := helperGOOS, helperGeteuid
	t.Cleanup(func() { helperGOOS, helperGeteuid = origGOOS, origEuid })
	cases := []struct {
		goos   string
		euid   int
		socket string
		want   bool
	}{
		{"linux", 1000, "/s", true},
		{"linux", 0, "/s", false},
		{"linux", 1000, "  ", false},
		{"windows", 1000, "/s", false},
	}
	for _, tc := range cases {
		helperGOOS, helperGeteuid = tc.goos, func() int { return tc.euid }
		eq(t, tc.want, helperEnabled(tc.socket))
	}
}
