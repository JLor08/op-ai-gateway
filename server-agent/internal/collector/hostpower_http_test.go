// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHost is a scriptable hostpower endpoint. mode: "ok", "503", "close"
// (hijack and drop the connection), "garbage", "big".
type fakeHost struct {
	mu     sync.Mutex
	mode   string
	vmid   string
	energy uint64
	maxUJ  uint64
	paths  []string
	auths  []string
	srv    *httptest.Server
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	f := &fakeHost{mode: "ok", vmid: "100"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHost) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	mode, vmid, energy, maxUJ := f.mode, f.vmid, f.energy, f.maxUJ
	f.mu.Unlock()
	switch mode {
	case "503":
		http.Error(w, "not live", http.StatusServiceUnavailable)
	case "close":
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	case "redirect":
		http.Redirect(w, r, "/elsewhere/v1/energy", http.StatusFound)
	case "garbage":
		_, _ = w.Write([]byte("not json"))
	case "big":
		_, _ = w.Write([]byte(strings.Repeat("x", 1<<20)))
	default:
		_, _ = fmt.Fprintf(w, `{"vmid":%q,"energy_uj":%d,"max_energy_range_uj":%d}`, vmid, energy, maxUJ)
	}
}

func (f *fakeHost) set(mode string, energy uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode, f.energy = mode, energy
}

func (f *fakeHost) auth(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.auths[i]
}

func (f *fakeHost) requested(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.paths {
		if p == path {
			return true
		}
	}
	return false
}

func (f *fakeHost) lastPath() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paths[len(f.paths)-1]
}

// step sets the host state, advances the clock to base+at and collects once.
type hpStepper struct {
	t    *testing.T
	f    *fakeHost
	c    *hostpowerHTTPCollector
	base time.Time
	cur  time.Time
}

func newStepper(t *testing.T, token string) *hpStepper {
	t.Helper()
	f := newFakeHost(t)
	c := newHostpowerHTTPCollector(f.srv.URL, token, NewHostpowerHTTPClient())
	s := &hpStepper{t: t, f: f, c: c, base: time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)}
	s.cur = s.base
	c.now = func() time.Time { return s.cur }
	return s
}

func (s *hpStepper) collect(mode string, energy uint64, at time.Duration) (cpu, system *float64) {
	s.t.Helper()
	s.f.set(mode, energy)
	s.cur = s.base.Add(at)
	cpu, system, err := s.c.Collect(context.Background())
	if err != nil {
		s.t.Fatalf("Collect returned error: %v", err)
	}
	return cpu, system
}

func wantNil(t *testing.T, label string, v *float64) {
	t.Helper()
	if v != nil {
		t.Fatalf("%s = %v, want nil", label, *v)
	}
}

func wantWatts(t *testing.T, label string, v *float64, want float64) {
	t.Helper()
	if v == nil {
		t.Fatalf("%s = nil, want %v W", label, want)
	}
	if *v < want-0.001 || *v > want+0.001 {
		t.Fatalf("%s = %v, want %v W", label, *v, want)
	}
}

func TestHostpowerHTTPIdentity(t *testing.T) {
	c := newHostpowerHTTPCollector("", "", NewHostpowerHTTPClient())
	if c.Name() != "hostpower" {
		t.Fatalf("Name() = %q", c.Name())
	}
	if c.Available() {
		t.Fatal("Available() = true for an empty base URL")
	}
	nilClient := newHostpowerHTTPCollector("http://192.0.2.1:8080", "", nil)
	if !nilClient.Available() {
		t.Fatal("Available() = false for a set base URL")
	}
	assertProxyFree(t, nilClient.client) // the fallback client built for a nil argument
}

func TestHostpowerHTTPFirstSampleNil(t *testing.T) {
	s := newStepper(t, "")
	cpu, system := s.collect("ok", 1_000_000, 0)
	wantNil(t, "cpu", cpu)
	wantNil(t, "system", system)
}

func TestHostpowerHTTPWatts(t *testing.T) {
	s := newStepper(t, "")
	s.collect("ok", 1_000_000, 0)
	cpu, system := s.collect("ok", 16_000_000, time.Second)
	wantWatts(t, "cpu", cpu, 15)
	wantNil(t, "system", system)
}

func TestHostpowerHTTPDecreaseWithZeroRangeIsZero(t *testing.T) {
	s := newStepper(t, "")
	s.collect("ok", 5_000_000, 0)
	cpu, _ := s.collect("ok", 1_000_000, time.Second)
	wantWatts(t, "cpu", cpu, 0)
}

func TestHostpowerHTTPWraparoundUsesRange(t *testing.T) {
	s := newStepper(t, "")
	s.f.mu.Lock()
	s.f.maxUJ = 20_000_000
	s.f.mu.Unlock()
	s.collect("ok", 19_000_000, 0)
	cpu, _ := s.collect("ok", 4_000_000, time.Second) // (20-19)+4 = 5 J
	wantWatts(t, "cpu", cpu, 5)
}

func TestHostpowerHTTPErrorRebaselines(t *testing.T) {
	s := newStepper(t, "")
	_, _ = s.collect("ok", 1_000_000, 0)
	cpu, _ := s.collect("503", 0, time.Second)
	wantNil(t, "503", cpu)
	cpu, _ = s.collect("ok", 16_000_000, 2*time.Second)
	wantNil(t, "first after error", cpu)
	cpu, _ = s.collect("ok", 31_000_000, 3*time.Second)
	wantWatts(t, "second after error", cpu, 15)
}

func TestHostpowerHTTPBadJSONIsNilAndRebaselines(t *testing.T) {
	s := newStepper(t, "")
	s.collect("ok", 1_000_000, 0)
	cpu, _ := s.collect("garbage", 0, time.Second)
	wantNil(t, "garbage", cpu)
	cpu, _ = s.collect("ok", 16_000_000, 2*time.Second)
	wantNil(t, "first after garbage", cpu)
}

func TestHostpowerHTTPVMIDChangeRebaselines(t *testing.T) {
	s := newStepper(t, "")
	s.collect("ok", 1_000_000, 0)
	s.f.mu.Lock()
	s.f.vmid = "101"
	s.f.mu.Unlock()
	cpu, _ := s.collect("ok", 16_000_000, time.Second)
	wantNil(t, "after vmid change", cpu)
	cpu, _ = s.collect("ok", 31_000_000, 2*time.Second)
	wantWatts(t, "next sample", cpu, 15)
}

func TestHostpowerHTTPSendsTokenOnlyWhenSet(t *testing.T) {
	s := newStepper(t, "ophp_x")
	s.collect("ok", 1, 0)
	if got := s.f.auth(0); got != "Bearer ophp_x" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer ophp_x")
	}
	s = newStepper(t, "")
	s.collect("ok", 1, 0)
	if got := s.f.auth(0); got != "" {
		t.Fatalf("Authorization = %q, want none", got)
	}
}

func TestHostpowerHTTPBaseURLForms(t *testing.T) {
	for suffix, want := range map[string]string{"/": "/v1/energy", "/hp": "/hp/v1/energy", "/hp/": "/hp/v1/energy", "": "/v1/energy"} {
		f := newFakeHost(t)
		c := newHostpowerHTTPCollector(f.srv.URL+suffix, "", NewHostpowerHTTPClient())
		if _, _, err := c.Collect(context.Background()); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if got := f.lastPath(); got != want {
			t.Fatalf("base %q: requested %q, want %q", suffix, got, want)
		}
	}
}

// Host restart: connection drop, 503 while the VM is not live, then the counter
// restarts from a lower value. Nothing is reported until two clean samples exist.
func TestHostpowerHTTPHostRestart(t *testing.T) {
	s := newStepper(t, "")
	cpu, _ := s.collect("ok", 5_000_000, 0)
	wantNil(t, "baseline", cpu)
	cpu, _ = s.collect("close", 0, time.Second)
	wantNil(t, "dropped connection", cpu)
	cpu, _ = s.collect("503", 0, 2*time.Second)
	wantNil(t, "503", cpu)
	cpu, _ = s.collect("ok", 5_000_000, 3*time.Second)
	wantNil(t, "new baseline", cpu)
	cpu, _ = s.collect("ok", 20_000_000, 4*time.Second)
	wantWatts(t, "after restart", cpu, 15)
}

func TestHostpowerHTTPCapsBody(t *testing.T) {
	s := newStepper(t, "")
	s.collect("ok", 1_000_000, 0)
	done := make(chan *float64, 1)
	go func() {
		cpu, _ := s.collect("big", 0, time.Second)
		done <- cpu
	}()
	select {
	case cpu := <-done:
		wantNil(t, "oversized body", cpu)
	case <-time.After(5 * time.Second):
		t.Fatal("Collect hung on an oversized body")
	}
}

func assertProxyFree(t *testing.T, c *http.Client) {
	t.Helper()
	if c.Timeout != time.Second {
		t.Fatalf("Timeout = %v, want 1s", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatalf("Transport = %#v, want *http.Transport with nil Proxy", c.Transport)
	}
}

func TestHostpowerHTTPClientSettings(t *testing.T) {
	assertProxyFree(t, NewHostpowerHTTPClient())
}

func TestHostpowerHTTPDoesNotFollowRedirects(t *testing.T) {
	s := newStepper(t, "")
	s.collect("ok", 1_000_000, 0)
	cpu, _ := s.collect("redirect", 0, time.Second)
	wantNil(t, "redirect", cpu)
	if s.f.requested("/elsewhere/v1/energy") {
		t.Fatal("collector followed a redirect")
	}
	cpu, _ = s.collect("ok", 16_000_000, 2*time.Second)
	wantNil(t, "first after redirect", cpu)
	cpu, _ = s.collect("ok", 31_000_000, 3*time.Second)
	wantWatts(t, "second after redirect", cpu, 15)
}

// recordLogs points the collector at a text handler on a buffer at Debug.
func recordLogs(c *hostpowerHTTPCollector) *syncBuffer {
	buf := &syncBuffer{}
	c.log = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return buf
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestHostpowerHTTPLogsFailuresWithoutToken(t *testing.T) {
	s := newStepper(t, "ophp_guestsecret")
	logs := recordLogs(s.c)
	s.collect("503", 0, 0)
	s.collect("garbage", 0, time.Second)
	out := logs.String()
	for _, want := range []string{
		"level=DEBUG", `msg="hostpower: request failed"`, "status 503",
		"url=" + s.f.srv.URL + "/v1/energy", "not the energy JSON",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{"ophp_guestsecret", "Bearer", "Authorization"} {
		if strings.Contains(out, leak) {
			t.Fatalf("log leaks %q:\n%s", leak, out)
		}
	}
}

func TestHostpowerHTTPLogsTransportError(t *testing.T) {
	c := newHostpowerHTTPCollector("http://user:pw-secret@127.0.0.1:1", "ophp_guestsecret", NewHostpowerHTTPClient())
	logs := recordLogs(c)
	if cpu, _, err := c.Collect(context.Background()); cpu != nil || err != nil {
		t.Fatalf("Collect = %v, %v", cpu, err)
	}
	out := logs.String()
	if !strings.Contains(out, `msg="hostpower: request failed"`) || !strings.Contains(out, "127.0.0.1:1") {
		t.Fatalf("log = %s", out)
	}
	for _, leak := range []string{"ophp_guestsecret", "pw-secret"} {
		if strings.Contains(out, leak) {
			t.Fatalf("log leaks %q:\n%s", leak, out)
		}
	}
}

func TestHostpowerHTTPLogsServedVMIDOnFirstSuccessAndChange(t *testing.T) {
	s := newStepper(t, "")
	logs := recordLogs(s.c)
	infos := func() int {
		return strings.Count(logs.String(), `level=INFO msg="hostpower: host serves this VM's counter"`)
	}
	s.collect("ok", 1_000_000, 0)
	s.collect("ok", 2_000_000, time.Second)
	s.collect("503", 0, 2*time.Second)
	s.collect("ok", 3_000_000, 3*time.Second)
	if n := infos(); n != 1 {
		t.Fatalf("infos = %d, want 1 (first success only):\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "vmid=100") || !strings.Contains(logs.String(), "url="+s.f.srv.URL) {
		t.Fatalf("info lacks vmid/url:\n%s", logs.String())
	}
	s.f.mu.Lock()
	s.f.vmid = "101"
	s.f.mu.Unlock()
	s.collect("ok", 4_000_000, 4*time.Second)
	if n := infos(); n != 2 || !strings.Contains(logs.String(), "vmid=101") {
		t.Fatalf("infos = %d after a vmid change, want 2:\n%s", n, logs.String())
	}
}
