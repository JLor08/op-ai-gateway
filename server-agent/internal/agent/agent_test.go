// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-server-agent/internal/certinstall"
	"op-ai-server-agent/internal/collector"
	"op-ai-server-agent/internal/config"
	"op-ai-server-agent/internal/proxy"
	runtimectl "op-ai-server-agent/internal/runtime"
	"op-ai-server-agent/internal/sample"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHost returns a fixed Host with no error.
type fakeHost struct{ h *sample.Host }

func (f fakeHost) Collect(ctx context.Context) (*sample.Host, error) { return f.h, nil }

// fakeGPU returns a fixed set of GPUs with no error.
type fakeGPU struct{ gpus []sample.GPU }

func (f fakeGPU) Name() string                                      { return "fake" }
func (f fakeGPU) Available() bool                                   { return true }
func (f fakeGPU) Collect(ctx context.Context) ([]sample.GPU, error) { return f.gpus, nil }

// capturePoster records every pushed sample under a mutex.
type capturePoster struct {
	mu      sync.Mutex
	samples []*sample.Sample
}

func (p *capturePoster) Post(ctx context.Context, s *sample.Sample) error {
	p.mu.Lock()
	p.samples = append(p.samples, s)
	p.mu.Unlock()
	return nil
}

func (p *capturePoster) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.samples)
}

func (p *capturePoster) first() *sample.Sample {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.samples) == 0 {
		return nil
	}
	return p.samples[0]
}

func (p *capturePoster) last() *sample.Sample {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.samples) == 0 {
		return nil
	}
	return p.samples[len(p.samples)-1]
}

// errPoster always fails, counting the number of push attempts.
type errPoster struct {
	mu       sync.Mutex
	attempts int
}

func (p *errPoster) Post(ctx context.Context, s *sample.Sample) error {
	p.mu.Lock()
	p.attempts++
	p.mu.Unlock()
	return errors.New("boom")
}

func (p *errPoster) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts
}

// waitUntil polls cond until it is true or the deadline elapses.
func waitUntil(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}

func TestRunCollectsMergesPushes(t *testing.T) {
	host := fakeHost{h: &sample.Host{MemTotalBytes: 2048, CPUUtilPct: 12.5}}
	gpu := fakeGPU{gpus: []sample.GPU{{Index: 0, Name: "FakeGPU", UtilPct: 55}}}
	poster := &capturePoster{}

	cfg := config.Config{Interval: 20 * time.Millisecond}
	a := New(cfg, host, []collector.GPUCollector{gpu}, nil, nil, nil, nil, poster, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	waitUntil(t, 2*time.Second, func() bool { return poster.count() >= 3 })
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}

	got := poster.first()
	if got == nil {
		t.Fatal("no sample captured")
	}
	if got.Host == nil {
		t.Errorf("sample Host is nil, want the fake host")
	}
	if len(got.GPUs) != 1 || got.GPUs[0].Name != "FakeGPU" {
		t.Errorf("sample GPUs = %+v, want one GPU named FakeGPU", got.GPUs)
	}
	if got.AgentVersion != Version {
		t.Errorf("AgentVersion = %q, want %q", got.AgentVersion, Version)
	}
	if got.OS != runtime.GOOS {
		t.Errorf("OS = %q, want %q", got.OS, runtime.GOOS)
	}
	if got.Arch != runtime.GOARCH {
		t.Errorf("Arch = %q, want %q", got.Arch, runtime.GOARCH)
	}
	// Feature negotiation (design spec §9): collectOnce must actually
	// populate Capabilities -- a legacy agent that leaves it empty is
	// normalized to {} downstream, but a current build reports its real
	// feature set. The version is reported ONCE, as the top-level
	// agent_version asserted above, and deliberately NOT repeated inside
	// capabilities (see capabilitiesTemplate's comment).
	if len(got.Capabilities) == 0 {
		t.Fatal("Capabilities is empty; want collectOnce to populate it")
	}
	var caps map[string]json.RawMessage
	if err := json.Unmarshal(got.Capabilities, &caps); err != nil {
		t.Fatalf("Capabilities does not decode as a JSON object: %v (raw=%s)", err, got.Capabilities)
	}
	if _, dup := caps["agent_version"]; dup {
		t.Errorf("Capabilities carries agent_version (%s); the version must be reported ONCE, via the top-level agent_version field, so a version bump has a single place to touch", got.Capabilities)
	}
	if len(caps) != 1 {
		t.Errorf("Capabilities has %d keys (%s), want exactly one (features)", len(caps), got.Capabilities)
	}
	var features []string
	if err := json.Unmarshal(caps["features"], &features); err != nil {
		t.Fatalf("Capabilities.features does not decode as a string array: %v (raw=%s)", err, got.Capabilities)
	}
	// Derived from the registry, not a second copy of it: what a sample must
	// carry is exactly what this binary declares, in registry order --
	// TestFeatureRegistry is what guards the contents of that registry.
	if !reflect.DeepEqual(features, FeatureNames()) {
		t.Errorf("Capabilities.features = %v, want %v (the declared registry, in order)", features, FeatureNames())
	}
}

// fakeScraper returns fixed active/queue counters with no error.
type fakeScraper struct{ active, queue int }

func (f fakeScraper) Scrape(ctx context.Context) (int, int, error) { return f.active, f.queue, nil }

// errGPU always fails its collect (e.g. a wedged/absent CLI).
type errGPU struct{}

func (errGPU) Name() string    { return "errgpu" }
func (errGPU) Available() bool { return true }
func (errGPU) Collect(ctx context.Context) ([]sample.GPU, error) {
	return nil, errors.New("gpu boom")
}

// The scraper's active/queue counters are merged into the pushed sample
// alongside host + GPUs (the scraper dependency, nil in the base test, is
// actually exercised here).
func TestRunMergesScraper(t *testing.T) {
	host := fakeHost{h: &sample.Host{MemTotalBytes: 2048}}
	gpu := fakeGPU{gpus: []sample.GPU{{Index: 0, Name: "FakeGPU"}}}
	poster := &capturePoster{}

	cfg := config.Config{Interval: 20 * time.Millisecond}
	a := New(cfg, host, []collector.GPUCollector{gpu}, fakeScraper{active: 4, queue: 2}, nil, nil, nil, poster, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	waitUntil(t, 2*time.Second, func() bool { return poster.count() >= 1 })
	cancel()
	<-done

	got := poster.first()
	if got == nil {
		t.Fatal("no sample captured")
	}
	if got.Host == nil || len(got.GPUs) != 1 {
		t.Errorf("sample host/gpus not merged: host=%v gpus=%+v", got.Host, got.GPUs)
	}
	if got.ActiveRequests != 4 || got.QueueDepth != 2 {
		t.Errorf("scraper counters not merged: active=%d queue=%d, want 4/2", got.ActiveRequests, got.QueueDepth)
	}
}

// A failing GPU collector must NOT drop the whole sample: the loop still pushes
// a sample carrying the host data (and no GPUs), and keeps ticking.
func TestRunSurvivesCollectorError(t *testing.T) {
	host := fakeHost{h: &sample.Host{MemTotalBytes: 1024, CPUUtilPct: 9}}
	poster := &capturePoster{}

	cfg := config.Config{Interval: 15 * time.Millisecond}
	a := New(cfg, host, []collector.GPUCollector{errGPU{}}, nil, nil, nil, nil, poster, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	waitUntil(t, 2*time.Second, func() bool { return poster.count() >= 2 })
	cancel()
	<-done

	got := poster.first()
	if got == nil || got.Host == nil {
		t.Fatal("sample with host data should be pushed despite the GPU error")
	}
	if len(got.GPUs) != 0 {
		t.Errorf("GPUs = %+v, want none (the only collector errored)", got.GPUs)
	}
}

func TestRunSurvivesPostError(t *testing.T) {
	host := fakeHost{h: &sample.Host{MemTotalBytes: 1024}}
	poster := &errPoster{}

	cfg := config.Config{Interval: 10 * time.Millisecond}
	a := New(cfg, host, nil, nil, nil, nil, nil, poster, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	// Despite every push erroring, the loop must keep ticking.
	waitUntil(t, 2*time.Second, func() bool { return poster.count() >= 2 })
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return cleanly after a stream of post errors")
	}
}

// fakePower returns fixed nullable CPU/system watts.
type fakePower struct{ cpu, system *float64 }

func (f fakePower) Name() string    { return "fakepower" }
func (f fakePower) Available() bool { return true }
func (f fakePower) Collect(context.Context) (*float64, *float64, error) {
	return f.cpu, f.system, nil
}

func TestRunCollectsPower(t *testing.T) {
	cpu := 55.5
	host := fakeHost{h: &sample.Host{MemTotalBytes: 2048}}
	poster := &capturePoster{}
	cfg := config.Config{Interval: 20 * time.Millisecond}
	a := New(cfg, host, nil, nil, nil, fakePower{cpu: &cpu, system: nil}, nil, poster, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	waitUntil(t, 2*time.Second, func() bool { return poster.count() >= 1 })
	cancel()
	<-done

	got := poster.first()
	if got == nil || got.Host == nil {
		t.Fatal("no sample/host captured")
	}
	if got.Host.CPUPowerW == nil || *got.Host.CPUPowerW != 55.5 {
		t.Fatalf("CPUPowerW = %v, want 55.5", got.Host.CPUPowerW)
	}
	if got.Host.SystemPowerW != nil {
		t.Fatalf("SystemPowerW = %v, want nil", *got.Host.SystemPowerW)
	}
}

// fakeTemp returns a fixed nullable CPU temperature.
type fakeTemp struct{ val *float64 }

func (f fakeTemp) Name() string    { return "faketemp" }
func (f fakeTemp) Available() bool { return true }
func (f fakeTemp) Collect(context.Context) (*float64, error) {
	return f.val, nil
}

func TestRunCollectsTemp(t *testing.T) {
	temp := 58.5
	host := fakeHost{h: &sample.Host{MemTotalBytes: 2048}}
	poster := &capturePoster{}
	cfg := config.Config{Interval: 20 * time.Millisecond}
	a := New(cfg, host, nil, nil, nil, nil, fakeTemp{val: &temp}, poster, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	waitUntil(t, 2*time.Second, func() bool { return poster.count() >= 1 })
	cancel()
	<-done

	got := poster.first()
	if got == nil || got.Host == nil {
		t.Fatal("no sample/host captured")
	}
	if got.Host.CPUTempC == nil || *got.Host.CPUTempC != 58.5 {
		t.Fatalf("CPUTempC = %v, want 58.5", got.Host.CPUTempC)
	}
}

// A nil temp collector must leave CPUTempC untouched (nil) — mirrors the
// power dependency being optional.
func TestRunNilTempCollectorLeavesTempNil(t *testing.T) {
	host := fakeHost{h: &sample.Host{MemTotalBytes: 2048}}
	poster := &capturePoster{}
	cfg := config.Config{Interval: 20 * time.Millisecond}
	a := New(cfg, host, nil, nil, nil, nil, nil, poster, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	waitUntil(t, 2*time.Second, func() bool { return poster.count() >= 1 })
	cancel()
	<-done

	got := poster.first()
	if got == nil || got.Host == nil {
		t.Fatal("no sample/host captured")
	}
	if got.Host.CPUTempC != nil {
		t.Fatalf("CPUTempC = %v, want nil (no temp collector configured)", *got.Host.CPUTempC)
	}
}

// fakeReporterPoster is both a poster and a reporter (both senders satisfy both).
type fakeReporterPoster struct {
	mu      sync.Mutex
	posts   int
	reports []*sample.SystemReport
}

func (f *fakeReporterPoster) Post(_ context.Context, _ *sample.Sample) error {
	f.mu.Lock()
	f.posts++
	f.mu.Unlock()
	return nil
}

func (f *fakeReporterPoster) PostSystemReport(_ context.Context, r *sample.SystemReport) error {
	f.mu.Lock()
	f.reports = append(f.reports, r)
	f.mu.Unlock()
	return nil
}

func (f *fakeReporterPoster) reportCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reports)
}

func TestRunSendsSystemReportAtStartup(t *testing.T) {
	frp := &fakeReporterPoster{}
	cfg := config.Config{Interval: 10 * time.Millisecond, SystemReportInterval: time.Hour}
	a := New(cfg, nil, nil, nil, nil, nil, nil, frp, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel: Run collects hardware once, sends it, then returns.
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if frp.reportCount() < 1 {
		t.Fatalf("system report not sent at startup (count=%d)", frp.reportCount())
	}
	if frp.reports[0].AgentVersion != Version {
		t.Fatalf("report agent_version = %q, want %q", frp.reports[0].AgentVersion, Version)
	}
}

// fakeCertSyncer is a certSyncer test double: it counts Sync calls, can
// optionally block a Sync call until the test releases it (to prove the
// telemetry loop's cadence survives a slow/stuck sync, and that two
// near-simultaneous triggers coalesce into one in-flight call rather than
// running concurrently), and returns a configurable Report.
type fakeCertSyncer struct {
	mu      sync.Mutex
	calls   int
	report  certinstall.Report
	changed bool
	err     error
	block   bool
	release chan struct{}
}

type fakeTrustSyncer struct {
	mu           sync.Mutex
	calls        int
	fingerprints []string
}

func (f *fakeTrustSyncer) Refresh(context.Context) error {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return nil
}

func (f *fakeTrustSyncer) DurableFingerprints() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.fingerprints...)
}

func (f *fakeTrustSyncer) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

type trustWakePoster struct {
	capturePoster
	wake chan struct{}
}

func (p *trustWakePoster) TrustUpdates() <-chan struct{} { return p.wake }

func TestAgentRefreshesTrustAtStartupWakeAndFifteenMinuteBackstop(t *testing.T) {
	old := trustRefreshInterval
	trustRefreshInterval = 20 * time.Millisecond
	t.Cleanup(func() { trustRefreshInterval = old })
	poster := &trustWakePoster{wake: make(chan struct{}, 1)}
	syncer := &fakeTrustSyncer{}
	cfg := config.Config{Interval: time.Hour, SystemReportInterval: time.Hour, CertMode: config.CertModeOff}
	a := New(cfg, nil, nil, nil, nil, nil, nil, poster, nil, syncer)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()
	waitUntil(t, time.Second, func() bool { return syncer.count() >= 1 })
	poster.wake <- struct{}{}
	waitUntil(t, time.Second, func() bool { return syncer.count() >= 2 })
	waitUntil(t, time.Second, func() bool { return syncer.count() >= 3 })
	cancel()
	<-done
}

func TestAgentModeOffReportsDurableTrustWithoutInstalledLeaf(t *testing.T) {
	poster := &capturePoster{}
	certs := newFakeCertSyncer()
	certs.setReport(certinstall.Report{Mode: config.CertModeOff, CAFingerprints: []string{"shared", "leaf-ca"}})
	trust := &fakeTrustSyncer{fingerprints: []string{"shared", "gateway-ca"}}
	a := New(config.Config{CertMode: config.CertModeOff}, nil, nil, nil, nil, nil, nil, poster, certs, trust)
	a.seedCertReport()
	a.collectOnce(context.Background())
	got := poster.first()
	if got == nil {
		t.Fatal("no sample")
	}
	if got.CertFingerprint != "" || got.CertMode != config.CertModeOff {
		t.Fatalf("leaf=%q mode=%q", got.CertFingerprint, got.CertMode)
	}
	want := []string{"shared", "gateway-ca", "leaf-ca"}
	if len(got.CertCAFingerprints) != len(want) {
		t.Fatalf("roots=%v", got.CertCAFingerprints)
	}
	for i := range want {
		if got.CertCAFingerprints[i] != want[i] {
			t.Fatalf("roots=%v want=%v", got.CertCAFingerprints, want)
		}
	}
}

func TestAgentReportsDurableTrustBeforeLegacyRootsSoGatewayCapRetainsIt(t *testing.T) {
	legacy := make([]string, 9)
	for i := range legacy {
		legacy[i] = fmt.Sprintf("%064x", i+1)
	}
	trustRoot := fmt.Sprintf("%064x", 100)
	certs := newFakeCertSyncer()
	certs.setReport(certinstall.Report{Mode: config.CertModeFiles, CAFingerprints: append(legacy, trustRoot)})
	trust := &fakeTrustSyncer{fingerprints: []string{trustRoot}}
	poster := &capturePoster{}
	a := New(config.Config{CertMode: config.CertModeFiles}, nil, nil, nil, nil, nil, nil, poster, certs, trust)
	a.seedCertReport()
	a.collectOnce(context.Background())

	got := poster.first()
	if got == nil {
		t.Fatal("no sample")
	}
	if len(got.CertCAFingerprints) != 10 || got.CertCAFingerprints[0] != trustRoot {
		t.Fatalf("root order does not prioritize durable trust: count=%d trust_first=%v", len(got.CertCAFingerprints), len(got.CertCAFingerprints) > 0 && got.CertCAFingerprints[0] == trustRoot)
	}
	for i := range legacy {
		if got.CertCAFingerprints[i+1] != legacy[i] {
			t.Fatalf("legacy root order changed at index %d", i)
		}
	}
	// The gateway sanitizer retains only the first eight roots. This is the
	// contract that prevents a long legacy bundle from displacing current trust.
	transmittedAfterGatewayCap := got.CertCAFingerprints[:8]
	if transmittedAfterGatewayCap[0] != trustRoot {
		t.Fatal("gateway cap would discard the current durable trust root")
	}
}

func TestAgentMainGatesTrustRefresherToHTTPSWithConfiguredTrustSource(t *testing.T) {
	src, err := os.ReadFile("../../main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	for _, required := range []string{
		`if shouldRefreshGatewayTrust(cfg) {`,
		`if err != nil || !strings.EqualFold(u.Scheme, "https") {`,
		`return cfg.CAFile != "" || cfg.CACacheFile != "" || cfg.CAPEM != "" || cfg.CertDir != ""`,
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("main trust-refresh gate missing required predicate %q", required)
		}
	}
	if got := strings.Count(body, "trust.NewRefresher("); got != 1 {
		t.Fatalf("main constructs trust refresher in %d/1 locations", got)
	}
	// SA-3: main builds one agent.Deps and assigns TrustSync exactly once,
	// guarded by this same nil check, before branching on transport to fill in
	// Deps.Poster -- no more per-transport-branch duplication of the guard (the
	// prior 4-way if/else New(...) ladder this replaced re-asserted it once per
	// branch, a live divergence hazard between the WS and POST paths).
	if got := strings.Count(body, "if trustRefresher != nil {"); got != 1 {
		t.Fatalf("main guards typed-nil trust refresher in %d/1 locations", got)
	}
}

func newFakeCertSyncer() *fakeCertSyncer {
	return &fakeCertSyncer{release: make(chan struct{})}
}

func (f *fakeCertSyncer) Sync(ctx context.Context) (certinstall.Report, bool, error) {
	f.mu.Lock()
	f.calls++
	block := f.block
	rep, changed, err := f.report, f.changed, f.err
	f.mu.Unlock()
	if block {
		<-f.release
	}
	return rep, changed, err
}

func (f *fakeCertSyncer) Report() certinstall.Report {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.report
}

func (f *fakeCertSyncer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeCertSyncer) setReport(r certinstall.Report) {
	f.mu.Lock()
	f.report = r
	f.mu.Unlock()
}

func (f *fakeCertSyncer) setBlocking(b bool) {
	f.mu.Lock()
	f.block = b
	f.mu.Unlock()
}

// TestCertPollIntervalResolvesByTransportUnlessExplicit pins the automatic
// interval resolution: WebSocket transport uses a distant 6h backstop (it
// already gets a push via cert_update + a wake on every reconnect); POST has
// neither, so it polls every 15m; and an explicitly configured positive value
// (already floored at 1m by config.Load, so not re-tested here) always wins
// over either automatic default.
func TestCertPollIntervalResolvesByTransportUnlessExplicit(t *testing.T) {
	if certPollIntervalWS != 6*time.Hour {
		t.Fatalf("certPollIntervalWS = %v, want 6h", certPollIntervalWS)
	}
	if certPollIntervalPOST != 15*time.Minute {
		t.Fatalf("certPollIntervalPOST = %v, want 15m", certPollIntervalPOST)
	}
	cases := []struct {
		name string
		cfg  config.Config
		want time.Duration
	}{
		{"automatic websocket", config.Config{Transport: config.TransportWebSocket}, certPollIntervalWS},
		{"automatic post", config.Config{Transport: config.TransportPost}, certPollIntervalPOST},
		{"explicit wins over websocket", config.Config{Transport: config.TransportWebSocket, CertPollInterval: 3 * time.Minute}, 3 * time.Minute},
		{"explicit wins over post", config.Config{Transport: config.TransportPost, CertPollInterval: 90 * time.Second}, 90 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := certPollInterval(c.cfg); got != c.want {
				t.Fatalf("certPollInterval(%+v) = %v, want %v", c.cfg, got, c.want)
			}
		})
	}
}

// TestNewCertTickerNilForOffModeOrNoSyncer pins newCertTicker's disabled case:
// both a nil ticker AND a nil channel (so Run's select needs no conditional
// branch to keep this case dormant), for cert_mode=off (with a syncer
// present) and for a nil syncer (with cert_mode NOT off) alike.
func TestNewCertTickerNilForOffModeOrNoSyncer(t *testing.T) {
	fc := newFakeCertSyncer()
	off := &Agent{cfg: config.Config{CertMode: config.CertModeOff, CertPollInterval: time.Millisecond}, certSync: fc}
	if ticker, c := off.newCertTicker(); ticker != nil || c != nil {
		t.Fatalf("newCertTicker() with cert_mode=off = %v, %v; want nil, nil", ticker, c)
	}

	noSyncer := &Agent{cfg: config.Config{CertMode: config.CertModeFiles, CertPollInterval: time.Millisecond}}
	if ticker, c := noSyncer.newCertTicker(); ticker != nil || c != nil {
		t.Fatalf("newCertTicker() with a nil certSync = %v, %v; want nil, nil", ticker, c)
	}

	on := &Agent{cfg: config.Config{CertMode: config.CertModeFiles, CertPollInterval: time.Hour}, certSync: fc}
	ticker, c := on.newCertTicker()
	if ticker == nil || c == nil {
		t.Fatal("newCertTicker() with cert_mode=files and a syncer wired = nil ticker/channel, want a real one")
	}
	ticker.Stop()
}

// TestSeedCertReportPopulatesFromDiskBeforeAnySync proves seedCertReport is a
// synchronous, non-Sync read: it must populate certReport from
// certSync.Report() alone, without ever calling Sync.
func TestSeedCertReportPopulatesFromDiskBeforeAnySync(t *testing.T) {
	fc := newFakeCertSyncer()
	fc.setReport(certinstall.Report{Fingerprint: "already-on-disk", Mode: config.CertModeFiles})
	a := &Agent{certSync: fc}

	a.seedCertReport()

	a.certReportMu.Lock()
	got := a.certReport
	a.certReportMu.Unlock()
	if got.Fingerprint != "already-on-disk" {
		t.Fatalf("certReport.Fingerprint = %q after seed, want %q", got.Fingerprint, "already-on-disk")
	}
	if fc.count() != 0 {
		t.Fatalf("Sync was called %d time(s) by seedCertReport, want 0 (Report() only)", fc.count())
	}
}

// TestCollectOnceCarriesCertReportFields proves collectOnce copies the
// mutex-guarded certReport (however it got there -- seed or a completed sync)
// onto every outgoing Sample's four cert_* fields.
func TestCollectOnceCarriesCertReportFields(t *testing.T) {
	fc := newFakeCertSyncer()
	notAfter := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	fc.setReport(certinstall.Report{
		Fingerprint:    "fp-123",
		NotAfter:       notAfter,
		Mode:           config.CertModeFiles,
		CAFingerprints: []string{"root-a", "root-b"},
	})

	host := fakeHost{h: &sample.Host{MemTotalBytes: 1024}}
	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := New(cfg, host, nil, nil, nil, nil, nil, poster, fc)
	a.seedCertReport()

	a.collectOnce(context.Background())

	got := poster.first()
	if got == nil {
		t.Fatal("no sample captured")
	}
	if got.CertFingerprint != "fp-123" {
		t.Errorf("CertFingerprint = %q, want fp-123", got.CertFingerprint)
	}
	if !got.CertNotAfter.Equal(notAfter) {
		t.Errorf("CertNotAfter = %v, want %v", got.CertNotAfter, notAfter)
	}
	if got.CertMode != config.CertModeFiles {
		t.Errorf("CertMode = %q, want %q", got.CertMode, config.CertModeFiles)
	}
	if len(got.CertCAFingerprints) != 2 || got.CertCAFingerprints[0] != "root-a" {
		t.Errorf("CertCAFingerprints = %+v, want [root-a root-b]", got.CertCAFingerprints)
	}
}

// TestSyncCertRecordsTheReturnedReportForCollectOnce proves syncCert's
// write-back: once a triggered sync completes, the Report it returned is
// exactly what a subsequent collectOnce copies onto the outgoing Sample --
// closing the gap between seedCertReport (tested in isolation above) and an
// ACTUAL completed sync updating the same mutex-guarded field.
func TestSyncCertRecordsTheReturnedReportForCollectOnce(t *testing.T) {
	fc := newFakeCertSyncer()
	fc.setReport(certinstall.Report{Fingerprint: "fresh-fp", Mode: config.CertModeFiles})

	host := fakeHost{h: &sample.Host{MemTotalBytes: 1024}}
	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour, CertMode: config.CertModeFiles}
	a := New(cfg, host, nil, nil, nil, nil, nil, poster, fc)

	a.triggerCertSync(context.Background())
	waitUntil(t, time.Second, func() bool { return !a.certSyncing.Load() })
	if fc.count() != 1 {
		t.Fatalf("Sync call count = %d, want 1", fc.count())
	}

	a.collectOnce(context.Background())
	got := poster.first()
	if got == nil {
		t.Fatal("no sample captured")
	}
	if got.CertFingerprint != "fresh-fp" {
		t.Fatalf("CertFingerprint = %q after a completed sync, want fresh-fp", got.CertFingerprint)
	}
}

// TestCertModeOffHasNoTickerNoFetchAndSampleCarriesOff is the Task 5b
// requirement verbatim: cert_mode=off means no ticker, no fetch ever (proven
// here even against a WS-style wake AND a deliberately provocative short
// CertPollInterval that WOULD have fired many times had the ticker existed),
// and every outgoing Sample still truthfully reports cert_mode:"off".
func TestCertModeOffHasNoTickerNoFetchAndSampleCarriesOff(t *testing.T) {
	fc := newFakeCertSyncer()
	fc.setReport(certinstall.Report{Mode: config.CertModeOff})

	host := fakeHost{h: &sample.Host{MemTotalBytes: 1024}}
	poster := &capturePoster{}
	cfg := config.Config{Interval: 10 * time.Millisecond, CertMode: config.CertModeOff, CertPollInterval: 15 * time.Millisecond}
	a := New(cfg, host, nil, nil, nil, nil, nil, poster, fc)

	if ticker, c := a.newCertTicker(); ticker != nil || c != nil {
		t.Fatalf("newCertTicker() = %v, %v with cert_mode=off; want nil, nil", ticker, c)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	// Simulate a wake arriving anyway (mirrors a WS reconnect, which fires
	// regardless of this agent's local cert_mode -- the gateway cannot know
	// it) via the same trigger path Run's select loop uses. It must be a
	// no-op.
	a.triggerCertSync(ctx)

	waitUntil(t, time.Second, func() bool { return poster.count() >= 8 })
	cancel()
	<-done

	if got := fc.count(); got != 0 {
		t.Fatalf("Sync was called %d time(s) with cert_mode=off; want 0 (the agent must never ask)", got)
	}
	got := poster.first()
	if got == nil {
		t.Fatal("no sample captured")
	}
	if got.CertMode != config.CertModeOff {
		t.Fatalf("sample cert_mode = %q, want %q", got.CertMode, config.CertModeOff)
	}
}

// TestTriggerCertSyncCoalescesConcurrentSignals is the Task 5b requirement
// "zwei gleichzeitige Weck-Signale erzeugen einen Sync": two triggers that
// land while a sync is already in flight must produce exactly one Sync call,
// and the atomic flag must release once that call completes so a later
// trigger can start a fresh one.
func TestTriggerCertSyncCoalescesConcurrentSignals(t *testing.T) {
	fc := newFakeCertSyncer()
	fc.setBlocking(true)
	a := &Agent{cfg: config.Config{CertMode: config.CertModeFiles}, certSync: fc}

	a.triggerCertSync(context.Background())
	// This second call happens while certSyncing is ALREADY true -- set
	// synchronously by CompareAndSwap inside the first call, strictly before
	// it even spawns its goroutine, and no goroutine scheduling occurs
	// between these two sequential calls on this single test goroutine. So
	// this call is deterministically guaranteed to find the flag held and
	// spawn nothing; it is not a race the test is hoping to win.
	a.triggerCertSync(context.Background())

	waitUntil(t, time.Second, func() bool { return fc.count() >= 1 })
	// Give a wrongly-spawned second goroutine every chance to also have
	// entered Sync by now, then assert it never did.
	time.Sleep(30 * time.Millisecond)
	if got := fc.count(); got != 1 {
		t.Fatalf("Sync call count while blocked = %d, want exactly 1 (both signals must coalesce into one in-flight sync)", got)
	}

	fc.unblock()
	waitUntil(t, time.Second, func() bool { return !a.certSyncing.Load() })

	// The flag must not be stuck: a later trigger starts a fresh sync.
	fc.setBlocking(false)
	a.triggerCertSync(context.Background())
	waitUntil(t, time.Second, func() bool { return fc.count() == 2 })
}

// unblock releases a Sync call parked on f.release. Must be called at most
// once per fakeCertSyncer (a second call would panic on an already-closed
// channel), matching every test's single block/unblock cycle.
func (f *fakeCertSyncer) unblock() {
	close(f.release)
}

// TestRunKeepsTelemetryCadenceWhileCertSyncBlocked is the Task 5b requirement
// "Der Loop bleibt bei einem 60-s-Sync bei seiner 1-s-Telemetrie-Kadenz": a
// certificate sync that never returns must not stall collectOnce, because
// syncCert runs on its own goroutine (triggerCertSync), never inline in Run's
// select loop.
func TestRunKeepsTelemetryCadenceWhileCertSyncBlocked(t *testing.T) {
	host := fakeHost{h: &sample.Host{MemTotalBytes: 1024}}
	poster := &capturePoster{}
	fc := newFakeCertSyncer()
	fc.setBlocking(true) // never unblocked in this test: the startup sync hangs forever.

	cfg := config.Config{Interval: 15 * time.Millisecond, CertMode: config.CertModeFiles, CertPollInterval: time.Hour}
	a := New(cfg, host, nil, nil, nil, nil, nil, poster, fc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	waitUntil(t, 2*time.Second, func() bool { return poster.count() >= 6 })
	cancel()
	<-done

	if got := fc.count(); got != 1 {
		t.Fatalf("cert sync calls = %d, want exactly 1 (the startup sync, still blocked in flight)", got)
	}
}

// fakeProxyDriver is a certProxyDriver test double counting the two hooks the
// agent drives from syncCert on the certificate-poll cadence.
type fakeProxyDriver struct {
	mu       sync.Mutex
	syncs    int
	reloads  int
	statuses []proxy.RouteStatus
}

func (f *fakeProxyDriver) SyncRoutes(context.Context) { f.mu.Lock(); f.syncs++; f.mu.Unlock() }
func (f *fakeProxyDriver) ReloadCert()                { f.mu.Lock(); f.reloads++; f.mu.Unlock() }
func (f *fakeProxyDriver) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.syncs, f.reloads
}

// Status returns the fake's configured route statuses (set via setStatuses),
// satisfying certProxyDriver's Task 4 addition.
func (f *fakeProxyDriver) Status() []proxy.RouteStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statuses
}

func (f *fakeProxyDriver) setStatuses(s []proxy.RouteStatus) {
	f.mu.Lock()
	f.statuses = s
	f.mu.Unlock()
}

// TestSyncCertDrivesProxyDriver pins the cert_mode=proxy wiring inside syncCert:
// routes are refreshed on EVERY tick (SyncRoutes), but the leaf is hot-swapped
// (ReloadCert) ONLY when the installer reports a real install (changed).
func TestSyncCertDrivesProxyDriver(t *testing.T) {
	cfg := config.Config{Interval: time.Hour, CertMode: config.CertModeProxy}

	// A changed install -> SyncRoutes once AND ReloadCert once.
	fc := newFakeCertSyncer()
	fc.setReport(certinstall.Report{Fingerprint: "fp", Mode: config.CertModeProxy})
	fc.mu.Lock()
	fc.changed = true
	fc.mu.Unlock()
	pd := &fakeProxyDriver{}
	a := New(cfg, nil, nil, nil, nil, nil, nil, &capturePoster{}, fc)
	a.SetCertProxyDriver(pd)
	a.syncCert(context.Background())
	if s, r := pd.counts(); s != 1 || r != 1 {
		t.Fatalf("changed install: syncs=%d reloads=%d, want 1,1", s, r)
	}

	// No change -> SyncRoutes still runs, ReloadCert does NOT.
	fc2 := newFakeCertSyncer()
	fc2.setReport(certinstall.Report{Fingerprint: "fp", Mode: config.CertModeProxy})
	pd2 := &fakeProxyDriver{}
	a2 := New(cfg, nil, nil, nil, nil, nil, nil, &capturePoster{}, fc2)
	a2.SetCertProxyDriver(pd2)
	a2.syncCert(context.Background())
	if s, r := pd2.counts(); s != 1 || r != 0 {
		t.Fatalf("unchanged: syncs=%d reloads=%d, want 1,0", s, r)
	}
}

// TestCollectOnceCarriesProxyRoutes pins Certificates P4 Task 4: collectOnce
// populates Sample.ProxyRoutes from the proxy driver's Status() when one is
// installed (cert_mode=proxy), and leaves it empty/omitted when there is no
// driver at all (off/files -- main.go never calls SetCertProxyDriver there).
func TestCollectOnceCarriesProxyRoutes(t *testing.T) {
	cfg := config.Config{Interval: time.Hour, CertMode: config.CertModeProxy}

	pd := &fakeProxyDriver{}
	pd.setStatuses([]proxy.RouteStatus{{Listen: 8600, TLSActive: true}})
	poster := &capturePoster{}
	a := New(cfg, nil, nil, nil, nil, nil, nil, poster, nil)
	a.SetCertProxyDriver(pd)
	a.collectOnce(context.Background())

	got := poster.first()
	if got == nil {
		t.Fatal("no sample posted")
	}
	if len(got.ProxyRoutes) != 1 || got.ProxyRoutes[0].Listen != 8600 || !got.ProxyRoutes[0].TLSActive {
		t.Fatalf("ProxyRoutes = %+v, want [{8600 true}]", got.ProxyRoutes)
	}

	// No proxy driver installed at all (the off/files case): ProxyRoutes must
	// stay empty, never populated from a stale/unrelated source.
	poster2 := &capturePoster{}
	a2 := New(cfg, nil, nil, nil, nil, nil, nil, poster2, nil)
	a2.collectOnce(context.Background())
	got2 := poster2.first()
	if got2 == nil {
		t.Fatal("no sample posted (no driver case)")
	}
	if len(got2.ProxyRoutes) != 0 {
		t.Fatalf("ProxyRoutes = %+v, want empty with no proxy driver installed", got2.ProxyRoutes)
	}

	// Driver installed but reporting ZERO routes (a proxy agent whose routes are
	// all still pending -- e.g. no leaf yet): ProxyRoutes must stay nil so it is
	// omitted on the wire, matching the no-driver shape rather than serializing a
	// non-nil empty slice.
	pd3 := &fakeProxyDriver{}
	pd3.setStatuses([]proxy.RouteStatus{}) // driver present, zero observed routes
	poster3 := &capturePoster{}
	a3 := New(cfg, nil, nil, nil, nil, nil, nil, poster3, nil)
	a3.SetCertProxyDriver(pd3)
	a3.collectOnce(context.Background())
	got3 := poster3.first()
	if got3 == nil {
		t.Fatal("no sample posted (zero-route driver case)")
	}
	if got3.ProxyRoutes != nil {
		t.Fatalf("ProxyRoutes = %+v, want nil (omitted) when the driver reports zero routes", got3.ProxyRoutes)
	}
}

// fakeRuntimeDriver is a runtimeDriver test double: it counts Sync calls,
// records the last pushed payload, and returns a configurable Status slice
// (steppedRuntimeDriver below is the one that holds a Sync open). It also
// satisfies runtimeTransitionsWaker via its own trans channel field, which
// NewFromDeps discovers via a type assertion exactly like Deps.Poster's
// certWaker/trustWaker.
type fakeRuntimeDriver struct {
	mu          sync.Mutex
	calls       int
	lastPushed  json.RawMessage
	statuses    []runtimectl.Status
	trans       chan struct{}
	active      atomic.Bool // fix round 1, I3: defaults to false, matching the real Driver's honest "not yet negotiated" zero value
	resendCalls int
	appliedETag string
}

func newFakeRuntimeDriver() *fakeRuntimeDriver {
	return &fakeRuntimeDriver{}
}

func (f *fakeRuntimeDriver) Sync(_ context.Context, pushed json.RawMessage) {
	f.mu.Lock()
	f.calls++
	f.lastPushed = pushed
	f.mu.Unlock()
}

func (f *fakeRuntimeDriver) Status() []runtimectl.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statuses
}

func (f *fakeRuntimeDriver) Transitions() <-chan struct{} { return f.trans }

func (f *fakeRuntimeDriver) setStatuses(s []runtimectl.Status) {
	f.mu.Lock()
	f.statuses = s
	f.mu.Unlock()
}

func (f *fakeRuntimeDriver) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeRuntimeDriver) lastPush() json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPushed
}

// Active satisfies runtimeDriver's new (fix round 1, I3) required method.
// Defaults to false -- a test that wants collectOnce's runtime block
// engaged must call setActive(true) explicitly, matching the real Driver's
// honest "not yet negotiated" starting state now that main.go no longer
// blocks startup on a one-shot features probe.
func (f *fakeRuntimeDriver) Active() bool { return f.active.Load() }

// AppliedConfigETag satisfies the optional runtimeConfigAcknowledger
// interface, returning whatever setAppliedETag last stored ("" by default,
// matching a driver that has applied no gateway document).
func (f *fakeRuntimeDriver) AppliedConfigETag() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.appliedETag
}

func (f *fakeRuntimeDriver) setAppliedETag(etag string) {
	f.mu.Lock()
	f.appliedETag = etag
	f.mu.Unlock()
}

func (f *fakeRuntimeDriver) setActive(b bool) { f.active.Store(b) }

// ResendReport satisfies the optional runtimeReportResender interface
// (fix round 1, I5), counting calls so tests can prove Run's reportTicker
// cadence actually reaches it.
func (f *fakeRuntimeDriver) ResendReport(context.Context) {
	f.mu.Lock()
	f.resendCalls++
	f.mu.Unlock()
}

func (f *fakeRuntimeDriver) resendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resendCalls
}

// runtimeWakePoster is a poster that also implements runtimeWaker, mirroring
// trustWakePoster exactly.
type runtimeWakePoster struct {
	capturePoster
	wake chan json.RawMessage
}

func (p *runtimeWakePoster) RuntimeUpdates() <-chan json.RawMessage { return p.wake }

// TestCollectOnceRuntimeNilOmitsRuntimesKey pins the no-op invariant this
// whole feature depends on
// (docs/architecture/cross-cutting/telemetry-usage-observability.md §8.3.2,
// the absent-vs-empty rules on "runtimes"): an Agent built with a nil
// RuntimeDriver -- every pre-Task-18 test construction, and every agent
// that never negotiates runtime_manager -- must produce a BYTE-IDENTICAL
// telemetry sample to one built before this feature existed. The "runtimes"
// key must be absent entirely from the marshaled JSON, not present-and-empty.
func TestCollectOnceRuntimeNilOmitsRuntimesKey(t *testing.T) {
	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := New(cfg, nil, nil, nil, nil, nil, nil, poster, nil)
	a.collectOnce(context.Background())

	got := poster.first()
	if got == nil {
		t.Fatal("no sample posted")
	}
	if got.Runtimes != nil {
		t.Fatalf("Runtimes = %+v, want nil (no runtime driver installed)", got.Runtimes)
	}
	got.Normalize()
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "runtimes") {
		t.Fatalf("marshaled sample contains a \"runtimes\" key with no runtime driver installed: %s", raw)
	}
}

// TestCollectOnceRuntimePopulatesRuntimesAndOverridesLoadedModels proves
// the non-nil path: driver.Status() maps to Runtimes (including measured
// VRAM and last_error), and LoadedModels is set AUTHORITATIVELY from the
// manager (running states only) -- overriding whatever a separately
// configured model-status lister would otherwise have reported.
func TestCollectOnceRuntimePopulatesRuntimesAndOverridesLoadedModels(t *testing.T) {
	poster := &capturePoster{}
	drv := newFakeRuntimeDriver()
	drv.setActive(true) // fix round 1, I3: the override is gated on Active(), not merely a driver existing
	since := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	failedAt := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID:       "rspec_a",
			Model:        "qwen-coder",
			State:        runtimectl.StateRunning,
			Since:        since,
			PID:          111,
			Port:         9001,
			InFlight:     2,
			Restarts:     1,
			MeasuredVRAM: map[int]int{1: 8000, 0: 21234},
		},
		{
			SpecID: "rspec_b",
			Model:  "llama-small",
			State:  runtimectl.StateCrashed,
			Since:  since,
			LastError: &runtimectl.LastError{
				Message:    "boom",
				At:         failedAt,
				ExitCode:   1,
				Failures:   3,
				StderrTail: "oom",
			},
		},
	})

	// A model-status lister that WOULD populate LoadedModels if the runtime
	// driver did not override it -- proving the override actually happens,
	// not merely that it is absent by coincidence.
	loaded := stubLoadedLister{models: []string{"stale-model-from-scraper"}}

	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Loaded: loaded, Poster: poster, RuntimeDriver: drv})
	a.collectOnce(context.Background())

	got := poster.first()
	if got == nil {
		t.Fatal("no sample posted")
	}
	if len(got.Runtimes) != 2 {
		t.Fatalf("Runtimes len = %d, want 2: %+v", len(got.Runtimes), got.Runtimes)
	}
	r0 := got.Runtimes[0]
	if r0.SpecID != "rspec_a" || r0.Model != "qwen-coder" || r0.State != "running" {
		t.Errorf("Runtimes[0] identity = %+v", r0)
	}
	if r0.PID != 111 || r0.Port != 9001 || r0.InFlight != 2 || r0.Restarts != 1 {
		t.Errorf("Runtimes[0] counters = %+v", r0)
	}
	if len(r0.GPUs) != 2 || r0.GPUs[0].Index != 0 || r0.GPUs[0].VRAMMeasuredMB != 21234 || r0.GPUs[1].Index != 1 || r0.GPUs[1].VRAMMeasuredMB != 8000 {
		t.Errorf("Runtimes[0].GPUs = %+v, want sorted [{0 21234} {1 8000}]", r0.GPUs)
	}
	r1 := got.Runtimes[1]
	if r1.LastError == nil || r1.LastError.Message != "boom" || r1.LastError.ExitCode != 1 || r1.LastError.Failures != 3 || r1.LastError.StderrTail != "oom" {
		t.Errorf("Runtimes[1].LastError = %+v", r1.LastError)
	}

	if len(got.LoadedModels) != 1 || got.LoadedModels[0] != "qwen-coder" {
		t.Fatalf("LoadedModels = %v, want [qwen-coder] (authoritative from the manager, overriding the model-status lister)", got.LoadedModels)
	}
}

// stubLoadedLister is a minimal collector.LoadedModelLister fake.
type stubLoadedLister struct{ models []string }

func (s stubLoadedLister) Available() bool { return true }
func (s stubLoadedLister) Collect(context.Context) ([]string, error) {
	return s.models, nil
}

// TestCollectOnceInactiveRuntimeDriverDoesNotOverrideLoadedModels pins fix
// round 1's I3 trap, exactly as the review named it: main.go now
// constructs the runtime driver UNCONDITIONALLY, so "a.runtimeDriver !=
// nil" is no longer sufficient to know whether the runtime feature is
// doing anything right now. Before the fix, a driver that exists but has
// never (yet) negotiated runtime_manager active (Active()==false --
// startup, a drain, a gateway that never declares the feature) would still
// unconditionally overwrite LoadedModels with an empty list, wiping a
// non-runtime agent's REAL loaded-model set reported by its own
// model-status scraper. This test fails against a version of collectOnce
// that gates the override on "a.runtimeDriver != nil" alone.
func TestCollectOnceInactiveRuntimeDriverDoesNotOverrideLoadedModels(t *testing.T) {
	poster := &capturePoster{}
	drv := newFakeRuntimeDriver() // Active() defaults to false -- never negotiated (yet)
	loaded := stubLoadedLister{models: []string{"real-model-from-scraper"}}

	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Loaded: loaded, Poster: poster, RuntimeDriver: drv})
	a.collectOnce(context.Background())

	got := poster.first()
	if got == nil {
		t.Fatal("no sample posted")
	}
	if got.Runtimes != nil {
		t.Fatalf("Runtimes = %+v, want nil (driver present but not Active)", got.Runtimes)
	}
	if len(got.LoadedModels) != 1 || got.LoadedModels[0] != "real-model-from-scraper" {
		t.Fatalf("LoadedModels = %v, want [real-model-from-scraper] -- an inactive runtime driver must NOT wipe the real model-status scraper's result", got.LoadedModels)
	}
}

// TestResendRuntimeReportPiggybacksOnSystemReportTicker pins fix round 1's
// I5: the runtime driver's periodic file-mode-report resend rides the SAME
// ticker cadence as sendSystemReport, not a transport-aware mechanism of
// its own.
func TestResendRuntimeReportPiggybacksOnSystemReportTicker(t *testing.T) {
	poster := &capturePoster{}
	drv := newFakeRuntimeDriver()
	cfg := config.Config{Interval: time.Hour, SystemReportInterval: 15 * time.Millisecond}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	waitUntil(t, time.Second, func() bool { return drv.resendCount() >= 2 })
	cancel()
	<-done
}

// TestRuntimeTransitionsWakeTriggersImmediateSample proves the
// runtimeTransitions doorbell: a spec state transition produces an
// immediate extra Post, without waiting for the (very long, in this test)
// telemetry interval.
func TestRuntimeTransitionsWakeTriggersImmediateSample(t *testing.T) {
	poster := &capturePoster{}
	drv := newFakeRuntimeDriver()
	drv.trans = make(chan struct{}, 1)
	cfg := config.Config{Interval: time.Hour, SystemReportInterval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	waitUntil(t, time.Second, func() bool { return poster.count() >= 1 })
	base := poster.count()

	drv.trans <- struct{}{}
	waitUntil(t, time.Second, func() bool { return poster.count() > base })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

// TestRuntimeWakePassesPushedPayloadToSync proves the runtimeWake channel
// plumbing end to end: a poster implementing runtimeWaker delivers a pushed
// payload straight through triggerRuntimeSync into Driver.Sync.
func TestRuntimeWakePassesPushedPayloadToSync(t *testing.T) {
	poster := &runtimeWakePoster{wake: make(chan json.RawMessage, 1)}
	drv := newFakeRuntimeDriver()
	cfg := config.Config{Interval: time.Hour, SystemReportInterval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	// The initial startup sync (triggerRuntimeSync(ctx, nil) inside Run)
	// races the wake below; wait for at least one Sync call before pushing,
	// so the assertion is unambiguous about which call carried the payload.
	waitUntil(t, time.Second, func() bool { return drv.count() >= 1 })

	payload := json.RawMessage(`{"router_listen":9000,"specs":[]}`)
	poster.wake <- payload
	waitUntil(t, time.Second, func() bool { return drv.count() >= 2 })
	if got := drv.lastPush(); string(got) != string(payload) {
		t.Fatalf("last Sync payload = %s, want %s", got, payload)
	}

	cancel()
	<-done
}

// steppedRuntimeDriver is a runtimeDriver whose every Sync announces its
// payload on entered and then waits for one value on proceed, so a test
// decides exactly when each sync ends. It records every payload in call
// order and the highest number of Syncs that ever ran at once.
type steppedRuntimeDriver struct {
	entered chan json.RawMessage
	proceed chan struct{}

	mu          sync.Mutex
	pushes      []string
	inFlight    int
	maxInFlight int
}

func newSteppedRuntimeDriver() *steppedRuntimeDriver {
	return &steppedRuntimeDriver{entered: make(chan json.RawMessage), proceed: make(chan struct{})}
}

func (d *steppedRuntimeDriver) Sync(_ context.Context, pushed json.RawMessage) {
	d.mu.Lock()
	d.inFlight++
	d.maxInFlight = max(d.maxInFlight, d.inFlight)
	d.pushes = append(d.pushes, string(pushed))
	d.mu.Unlock()
	d.entered <- pushed
	<-d.proceed
	d.mu.Lock()
	d.inFlight--
	d.mu.Unlock()
}

func (d *steppedRuntimeDriver) Status() []runtimectl.Status { return nil }
func (d *steppedRuntimeDriver) Active() bool                { return false }

// expectSync waits for the next Sync to start and fails unless it carries want
// ("" for a nil payload, the "resync over HTTP" wake).
func (d *steppedRuntimeDriver) expectSync(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-d.entered:
		if string(got) != want {
			t.Fatalf("Sync started with payload %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no Sync started; want one with payload %q", want)
	}
}

// finish ends the Sync that expectSync last saw start.
func (d *steppedRuntimeDriver) finish(t *testing.T) {
	t.Helper()
	select {
	case d.proceed <- struct{}{}:
	case <-time.After(2 * time.Second):
		t.Fatal("no Sync was waiting to finish")
	}
}

func (d *steppedRuntimeDriver) record() (pushes []string, maxInFlight int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.pushes...), d.maxInFlight
}

// runtimeSyncIdle reports whether no runtime sync runs and none is owed: once
// it holds, no goroutine is left that could start another Sync.
func runtimeSyncIdle(a *Agent) bool {
	a.runtimeSync.mu.Lock()
	defer a.runtimeSync.mu.Unlock()
	return !a.runtimeSync.running && !a.runtimeSync.owed
}

// TestTriggerRuntimeSyncOwesOneSyncToWakesDuringASync pins the trailing sync:
// wakes that arrive while a sync runs are not dropped but coalesce into
// exactly one more sync, with the payload of the last of them; a wake during
// that trailing sync owes one of its own; and no two Syncs ever overlap.
// Until 0.8.1 the second and later wakes were simply dropped, and the agent
// rested on the older document until the next push or the 60 s poll.
func TestTriggerRuntimeSyncOwesOneSyncToWakesDuringASync(t *testing.T) {
	drv := newSteppedRuntimeDriver()
	a := &Agent{runtimeDriver: drv}
	ctx := context.Background()

	a.triggerRuntimeSync(ctx, json.RawMessage(`{"etag":"d1"}`))
	drv.expectSync(t, `{"etag":"d1"}`)
	// Three wakes during d1's sync, the poll ticker's nil among them. They
	// are recorded synchronously, on this goroutine, so this is not a race.
	a.triggerRuntimeSync(ctx, json.RawMessage(`{"etag":"d2"}`))
	a.triggerRuntimeSync(ctx, nil)
	a.triggerRuntimeSync(ctx, json.RawMessage(`{"etag":"d3"}`))
	a.runtimeSync.mu.Lock()
	running, owed, next := a.runtimeSync.running, a.runtimeSync.owed, string(a.runtimeSync.next)
	a.runtimeSync.mu.Unlock()
	if !running || !owed || next != `{"etag":"d3"}` {
		t.Fatalf("after three wakes during a sync: running=%v owed=%v next=%q, want true true {\"etag\":\"d3\"} (owed to the running sync, latest wins)", running, owed, next)
	}

	drv.finish(t)
	drv.expectSync(t, `{"etag":"d3"}`) // one trailing sync, with the latest wake
	a.triggerRuntimeSync(ctx, json.RawMessage(`{"etag":"d4"}`))
	drv.finish(t)
	drv.expectSync(t, `{"etag":"d4"}`) // the trailing sync owed its own wake one more
	drv.finish(t)
	waitUntil(t, time.Second, func() bool { return runtimeSyncIdle(a) })

	pushes, maxInFlight := drv.record()
	if want := []string{`{"etag":"d1"}`, `{"etag":"d3"}`, `{"etag":"d4"}`}; !reflect.DeepEqual(pushes, want) {
		t.Fatalf("Sync payloads = %q, want %q", pushes, want)
	}
	if maxInFlight != 1 {
		t.Fatalf("%d Syncs ran at once, want at most 1", maxInFlight)
	}

	// Nothing is stuck: a later wake starts a fresh sync of its own.
	a.triggerRuntimeSync(ctx, nil)
	drv.expectSync(t, "")
	drv.finish(t)
	waitUntil(t, time.Second, func() bool { return runtimeSyncIdle(a) })
}

// TestTriggerRuntimeSyncTrailingSyncKeepsALateNil pins the other half of
// latest-wins: a nil that arrives after a pushed document during a sync (a
// reconnect, which may have missed pushes, or the poll ticker) makes the
// trailing sync resync over HTTP rather than apply the earlier document.
func TestTriggerRuntimeSyncTrailingSyncKeepsALateNil(t *testing.T) {
	drv := newSteppedRuntimeDriver()
	a := &Agent{runtimeDriver: drv}
	ctx := context.Background()

	a.triggerRuntimeSync(ctx, json.RawMessage(`{"etag":"d1"}`))
	drv.expectSync(t, `{"etag":"d1"}`)
	a.triggerRuntimeSync(ctx, json.RawMessage(`{"etag":"d2"}`))
	a.triggerRuntimeSync(ctx, nil)
	drv.finish(t)
	drv.expectSync(t, "")
	drv.finish(t)
	waitUntil(t, time.Second, func() bool { return runtimeSyncIdle(a) })

	if pushes, _ := drv.record(); !reflect.DeepEqual(pushes, []string{`{"etag":"d1"}`, ""}) {
		t.Fatalf("Sync payloads = %q, want [d1, nil]", pushes)
	}
}

// TestTriggerRuntimeSyncRunsNoTrailingSyncAfterCancel: once Run's context is
// cancelled the agent is shutting down, and a wake still owed is not run.
func TestTriggerRuntimeSyncRunsNoTrailingSyncAfterCancel(t *testing.T) {
	drv := newSteppedRuntimeDriver()
	a := &Agent{runtimeDriver: drv}
	ctx, cancel := context.WithCancel(context.Background())

	a.triggerRuntimeSync(ctx, json.RawMessage(`{"etag":"d1"}`))
	drv.expectSync(t, `{"etag":"d1"}`)
	a.triggerRuntimeSync(ctx, json.RawMessage(`{"etag":"d2"}`))
	cancel()
	drv.finish(t)
	waitUntil(t, time.Second, func() bool { return runtimeSyncIdle(a) })

	if pushes, _ := drv.record(); !reflect.DeepEqual(pushes, []string{`{"etag":"d1"}`}) {
		t.Fatalf("Sync payloads = %q, want only d1 (no trailing sync after cancel)", pushes)
	}
}

// TestTriggerRuntimeSyncEndsOnTheLastWakeUnderABurst hammers the trigger
// while syncs run on their own goroutine, with scheduling jitter and no
// blocking. First one producer, Run's own shape: whatever the interleaving,
// the last Sync carries the last wake, Syncs never overlap, and they apply
// the wakes in order. Then four producers at once, which only contend the
// lock harder: a final wake after they stop must still be the last Sync. A
// release of "running" outside the critical section that finds nothing owed
// -- a wake landing between the two is recorded as owed with no goroutine
// left to run it -- fails it only by chance, because no single interleaving
// can force a wake into that window;
// TestTriggerRuntimeSyncEndsOnTheLaterOfTwoOrderedWakes aims one at it in
// every round.
func TestTriggerRuntimeSyncEndsOnTheLastWakeUnderABurst(t *testing.T) {
	const n = 500
	for round := 0; round < 20; round++ {
		drv := &orderedRuntimeDriver{}
		a := &Agent{runtimeDriver: drv}
		ctx := context.Background()
		for i := 1; i <= n; i++ {
			a.triggerRuntimeSync(ctx, json.RawMessage(strconv.Itoa(i)))
			if i%3 == 0 {
				runtime.Gosched()
			}
		}
		waitUntil(t, 2*time.Second, func() bool { return runtimeSyncIdle(a) })
		last, maxInFlight, ordered := drv.result()
		if last != n || maxInFlight != 1 || !ordered {
			t.Fatalf("one producer, round %d: last Sync payload %d (want %d), %d Syncs at once (want 1), in order %v", round, last, n, maxInFlight, ordered)
		}
	}
	for round := 0; round < 20; round++ {
		drv := &orderedRuntimeDriver{}
		a := &Agent{runtimeDriver: drv}
		ctx := context.Background()
		// A wake is owed only to a running sync: owed without running is
		// a wake nobody will run. A watcher checks that throughout.
		var orphaned atomic.Bool
		stop := make(chan struct{})
		watched := make(chan struct{})
		go func() {
			defer close(watched)
			for {
				select {
				case <-stop:
					return
				default:
				}
				a.runtimeSync.mu.Lock()
				if a.runtimeSync.owed && !a.runtimeSync.running {
					orphaned.Store(true)
				}
				a.runtimeSync.mu.Unlock()
				runtime.Gosched()
			}
		}()
		var wg sync.WaitGroup
		for p := 0; p < 4; p++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 1; i <= n; i++ {
					a.triggerRuntimeSync(ctx, json.RawMessage(strconv.Itoa(i)))
					runtime.Gosched()
				}
			}()
		}
		wg.Wait()
		a.triggerRuntimeSync(ctx, json.RawMessage(strconv.Itoa(n+1)))
		waitUntil(t, 2*time.Second, func() bool { return runtimeSyncIdle(a) })
		close(stop)
		<-watched
		if orphaned.Load() {
			t.Fatalf("four producers, round %d: a wake was owed while no sync ran", round)
		}
		if last, maxInFlight, _ := drv.result(); last != n+1 || maxInFlight != 1 {
			t.Fatalf("four producers, round %d: last Sync payload %d (want the final wake %d), %d Syncs at once (want 1)", round, last, n+1, maxInFlight)
		}
	}
}

// TestTriggerRuntimeSyncEndsOnTheLaterOfTwoOrderedWakes aims a second wake at
// the end of the sync the first one started, round after round: one producer
// sends two wakes in order, yielding between them on every other round, while
// four goroutines contend the lock and check that a wake is never owed while
// no sync runs. Each round must end on the second wake. A runRuntimeSyncs that
// finds nothing owed and clears running in two critical sections fails it
// whenever a wake lands between the two, which no single round can force; over
// all its rounds, on a machine with more than one CPU, one nearly always does.
func TestTriggerRuntimeSyncEndsOnTheLaterOfTwoOrderedWakes(t *testing.T) {
	const rounds = 20000
	drv := &orderedRuntimeDriver{}
	a := &Agent{runtimeDriver: drv}
	ctx := context.Background()
	var orphaned atomic.Bool
	stop := make(chan struct{})
	var contenders sync.WaitGroup
	for g := 0; g < 4; g++ {
		contenders.Add(1)
		go func() {
			defer contenders.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				a.runtimeSync.mu.Lock()
				if a.runtimeSync.owed && !a.runtimeSync.running {
					orphaned.Store(true)
				}
				a.runtimeSync.mu.Unlock()
				runtime.Gosched()
			}
		}()
	}
	defer func() { close(stop); contenders.Wait() }()
	for r := 1; r <= rounds; r++ {
		a.triggerRuntimeSync(ctx, json.RawMessage(strconv.Itoa(2*r-1)))
		if r%2 == 0 {
			runtime.Gosched()
		}
		a.triggerRuntimeSync(ctx, json.RawMessage(strconv.Itoa(2*r)))
		deadline := time.Now().Add(2 * time.Second)
		for !runtimeSyncIdle(a) {
			if orphaned.Load() {
				t.Fatalf("round %d: a wake was owed while no sync ran", r)
			}
			if time.Now().After(deadline) {
				t.Fatalf("round %d: the sync chain never ended", r)
			}
			runtime.Gosched()
		}
		if last, maxInFlight, _ := drv.result(); last != 2*r || maxInFlight != 1 {
			t.Fatalf("round %d: last Sync payload %d (want the second wake %d), %d Syncs at once (want 1)", r, last, 2*r, maxInFlight)
		}
	}
}

// orderedRuntimeDriver is a non-blocking runtimeDriver for the burst tests: it
// yields inside every Sync, and records the last payload, whether payloads
// only ever increased, and the most Syncs that ran at once.
type orderedRuntimeDriver struct {
	mu          sync.Mutex
	last        int
	ordered     bool
	started     bool
	inFlight    int
	maxInFlight int
}

func (d *orderedRuntimeDriver) Sync(_ context.Context, pushed json.RawMessage) {
	n, _ := strconv.Atoi(string(pushed))
	d.mu.Lock()
	d.inFlight++
	d.maxInFlight = max(d.maxInFlight, d.inFlight)
	if !d.started {
		d.started, d.ordered = true, true
	} else if n <= d.last {
		d.ordered = false
	}
	d.last = n
	d.mu.Unlock()
	runtime.Gosched()
	d.mu.Lock()
	d.inFlight--
	d.mu.Unlock()
}

func (d *orderedRuntimeDriver) Status() []runtimectl.Status { return nil }
func (d *orderedRuntimeDriver) Active() bool                { return false }

func (d *orderedRuntimeDriver) result() (last, maxInFlight int, ordered bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.last, d.maxInFlight, d.ordered
}

// runtimeAndCertWakePoster delivers runtime-config wakes and, on a second
// channel, certificate wakes. With both channels unbuffered and no certSync,
// a certificate wake is a no-op case of Run's select whose send returns only
// once Run is back in that select -- a barrier proving Run has finished
// handling the runtime wake it received before.
type runtimeAndCertWakePoster struct {
	capturePoster
	wake chan json.RawMessage
	cert chan struct{}
}

func (p *runtimeAndCertWakePoster) RuntimeUpdates() <-chan json.RawMessage { return p.wake }
func (p *runtimeAndCertWakePoster) CertUpdates() <-chan struct{}           { return p.cert }

func (p *runtimeAndCertWakePoster) deliver(t *testing.T, doc json.RawMessage) {
	t.Helper()
	select {
	case p.wake <- doc:
	case <-time.After(2 * time.Second):
		t.Fatal("Run never received the runtime-config wake")
	}
}

func (p *runtimeAndCertWakePoster) barrier(t *testing.T) {
	t.Helper()
	select {
	case p.cert <- struct{}{}:
	case <-time.After(2 * time.Second):
		t.Fatal("Run never came back to its select")
	}
}

// TestRuntimeWakeDuringASyncIsSyncedAfterIt is the trailing sync end to end
// through Run's select loop: a pushed document that arrives while the
// previous one's sync runs is synced right after it, not left to the poll.
func TestRuntimeWakeDuringASyncIsSyncedAfterIt(t *testing.T) {
	poster := &runtimeAndCertWakePoster{wake: make(chan json.RawMessage), cert: make(chan struct{})}
	drv := newSteppedRuntimeDriver()
	a := NewFromDeps(config.Config{Interval: time.Hour, SystemReportInterval: time.Hour}, Deps{Poster: poster, RuntimeDriver: drv})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	drv.expectSync(t, "") // the startup sync
	drv.finish(t)
	poster.deliver(t, json.RawMessage(`{"etag":"d1"}`))
	drv.expectSync(t, `{"etag":"d1"}`)
	poster.deliver(t, json.RawMessage(`{"etag":"d2"}`))
	poster.barrier(t) // Run has handed d2 to triggerRuntimeSync while d1's sync runs
	drv.finish(t)
	drv.expectSync(t, `{"etag":"d2"}`)
	drv.finish(t)
	waitUntil(t, time.Second, func() bool { return runtimeSyncIdle(a) })

	cancel()
	<-done
}

// TestRuntimeWakeDuringASyncEndsOnTheLatestDocument is the defect's own shape
// against the real Driver, GatewaySource and Manager: two pushed documents
// closer together than one sync (whose features round trip the fake gateway
// holds open) must leave the manager on the second. Until 0.8.1 it stayed on
// the first until the next push or the 60 s poll.
func TestRuntimeWakeDuringASyncEndsOnTheLatestDocument(t *testing.T) {
	doc := func(etag, adminState string) json.RawMessage {
		return json.RawMessage(`{"router_listen":0,"specs":[{"id":"rs_1","model":"m1","upstream_model":"m1","binary":"/not/allowed/server","args":[],"env":{},"gpus":[],"health_path":"/health","admin_state":"` + adminState + `"}],"coresident":[],"gpu_budgets":[],"etag":"` + etag + `"}`)
	}
	var hold atomic.Bool
	var featureFetches atomic.Int64
	held := make(chan struct{}, 1)
	release := make(chan struct{})
	// releaseFeatures ends the hold: the held features round trip answers,
	// and no later one is held. The test calls it once d2 has reached the
	// trigger, and a deferred call ends the hold on every other way out, a
	// t.Fatal included, because gw.Close waits for the handler.
	releaseFeatures := sync.OnceFunc(func() {
		hold.Store(false)
		close(release)
	})
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/agent/v1/features":
			featureFetches.Add(1)
			if hold.Load() {
				held <- struct{}{}
				<-release
			}
			_, _ = w.Write([]byte(`{"features":["runtime_manager"]}`))
		case "/api/agent/v1/runtime-config":
			_, _ = w.Write(doc("doc0", ""))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gw.Close()
	m := runtimectl.NewManager(runtimectl.ManagerOptions{Policy: runtimectl.LocalPolicy{}, Getenv: func(string) string { return "" }})
	defer m.Close()
	src := runtimectl.NewGatewaySource(gw.URL, "tok", nil, filepath.Join(t.TempDir(), "runtime-cache.json"))
	drv := runtimectl.NewDriver(m, src, runtimectl.NewFeaturesClient(gw.URL, "tok", nil), nil, "")
	defer drv.Close()
	poster := &runtimeAndCertWakePoster{wake: make(chan json.RawMessage), cert: make(chan struct{})}
	a := NewFromDeps(config.Config{Interval: time.Hour, SystemReportInterval: time.Hour}, Deps{Poster: poster, RuntimeDriver: drv})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	defer func() { cancel(); <-done }()
	// Deferred last, so it runs first: before the deferred cancel waits for
	// Run, whose sync may be the one held, and before gw.Close.
	defer releaseFeatures()

	waitUntil(t, 3*time.Second, func() bool { return m.AppliedETag() == "doc0" && runtimeSyncIdle(a) })
	hold.Store(true)
	poster.deliver(t, doc("d1", ""))
	select {
	case <-held: // d1's sync is in its features round trip
	case <-time.After(2 * time.Second):
		t.Fatal("d1's sync never reached the gateway")
	}
	poster.deliver(t, doc("d2", "force_stopped"))
	poster.barrier(t) // d2 reached triggerRuntimeSync during d1's sync
	releaseFeatures()
	waitUntil(t, 3*time.Second, func() bool { return runtimeSyncIdle(a) })

	if got := m.AppliedETag(); got != "d2" {
		t.Fatalf("manager holds %q after two pushes during one sync, want the later d2", got)
	}
	if got := featureFetches.Load(); got != 3 {
		t.Fatalf("features fetches = %d, want 3 (startup, d1, the trailing sync for d2)", got)
	}
}

// TestNewRuntimeTickerNilWithoutDriver proves the nil-channel discipline:
// no driver -> no ticker, mirroring TestNewCertTickerNilForOffModeOrNoSyncer.
func TestNewRuntimeTickerNilWithoutDriver(t *testing.T) {
	a := &Agent{}
	ticker, ch := a.newRuntimeTicker()
	if ticker != nil || ch != nil {
		t.Fatalf("newRuntimeTicker() with no driver = (%v, %v), want (nil, nil)", ticker, ch)
	}
}

// portFromURL extracts the numeric port an httptest server is listening on.
func portFromURL(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse url %q: %v", rawURL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse port from %q: %v", rawURL, err)
	}
	return port
}

// TestCollectOnceRuntimeProbesMetricsAndContext is Task 9's core proof: a
// StateRunning child with MetricsPath/ContextProbePath set gets its /metrics
// scraped on EVERY collectOnce (active/queue always fresh), while its
// context endpoint is probed only ONCE and then cached (keyed by SpecID+PID)
// across repeated cycles -- the whole point of caching a value that cannot
// change without a restart.
func TestCollectOnceRuntimeProbesMetricsAndContext(t *testing.T) {
	var contextHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			// vLLM Prometheus metric names (verified against upstream, see
			// internal/collector/scrape.go).
			_, _ = w.Write([]byte("vllm:num_requests_running 4\nvllm:num_requests_waiting 2\n"))
		case "/v1/models":
			atomic.AddInt32(&contextHits, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"m1","max_model_len":8192}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID:           "rspec_1",
			Model:            "qwen-coder",
			State:            runtimectl.StateRunning,
			PID:              4242,
			Port:             portFromURL(t, srv.URL),
			Type:             "vllm",
			MetricsPath:      "/metrics",
			ContextProbePath: "/v1/models",
		},
	})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	a.collectOnce(context.Background())
	a.collectOnce(context.Background())

	if got := poster.count(); got != 2 {
		t.Fatalf("posted samples = %d, want 2", got)
	}
	got := poster.last()
	if got == nil || len(got.Runtimes) != 1 {
		t.Fatalf("Runtimes = %+v", got)
	}
	rs := got.Runtimes[0]
	if rs.ContextSize != 8192 {
		t.Errorf("ContextSize = %d, want 8192", rs.ContextSize)
	}
	if rs.ActiveRequests != 4 {
		t.Errorf("ActiveRequests = %d, want 4", rs.ActiveRequests)
	}
	if rs.QueueDepth != 2 {
		t.Errorf("QueueDepth = %d, want 2", rs.QueueDepth)
	}
	if hits := atomic.LoadInt32(&contextHits); hits != 1 {
		t.Fatalf("context probe hits across 2 cycles = %d, want 1 (cached)", hits)
	}
}

// TestCollectOnceRuntimeContextCacheInvalidatesOnProbeConfigChange is FIX 3's
// proof: the runtime manager's config reconciliation can change a RUNNING
// spec's ContextProbePath (or Type) WITHOUT restarting the process -- same
// PID. A cache keyed on PID alone would keep serving the size probed against
// the OLD path forever; the cache must also invalidate on a probe-config
// change and re-probe using the NEW path.
func TestCollectOnceRuntimeContextCacheInvalidatesOnProbeConfigChange(t *testing.T) {
	var v1Hits, v2Hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			atomic.AddInt32(&v1Hits, 1)
			_, _ = w.Write([]byte(`{"data":[{"id":"m1","max_model_len":8192}]}`))
		case "/v2/models":
			atomic.AddInt32(&v2Hits, 1)
			_, _ = w.Write([]byte(`{"data":[{"id":"m1","max_model_len":4096}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	baseStatus := runtimectl.Status{
		SpecID:           "rspec_3",
		Model:            "qwen-coder",
		State:            runtimectl.StateRunning,
		PID:              4242, // unchanged across both cycles -- no restart.
		Port:             portFromURL(t, srv.URL),
		Type:             "vllm",
		ContextProbePath: "/v1/models",
	}
	drv.setStatuses([]runtimectl.Status{baseStatus})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	a.collectOnce(context.Background())
	first := poster.first()
	if first == nil || len(first.Runtimes) != 1 {
		t.Fatalf("Runtimes (cycle 1) = %+v", first)
	}
	if got := first.Runtimes[0].ContextSize; got != 8192 {
		t.Fatalf("ContextSize (cycle 1) = %d, want 8192", got)
	}
	if hits := atomic.LoadInt32(&v1Hits); hits != 1 {
		t.Fatalf("/v1/models hits after cycle 1 = %d, want 1", hits)
	}

	// The spec's probe config changes (operator edit; runtime manager
	// reconciliation) WITHOUT a restart: same SpecID, same PID, new
	// ContextProbePath.
	changed := baseStatus
	changed.ContextProbePath = "/v2/models"
	drv.setStatuses([]runtimectl.Status{changed})

	a.collectOnce(context.Background())
	last := poster.last()
	if last == nil || len(last.Runtimes) != 1 {
		t.Fatalf("Runtimes (cycle 2) = %+v", last)
	}
	if got := last.Runtimes[0].ContextSize; got != 4096 {
		t.Errorf("ContextSize (cycle 2) = %d, want 4096 (must re-probe the NEW path, not serve the cached old-path value)", got)
	}
	if hits := atomic.LoadInt32(&v2Hits); hits != 1 {
		t.Errorf("/v2/models hits after cycle 2 = %d, want 1 (a pid-only cache key would never hit the new path)", hits)
	}
}

// TestCollectOnceRuntimeContextCacheInvalidatesOnModelChange is #54 task 4's
// cache-invalidation proof: the runtime manager can repoint a RUNNING
// `ollama serve` spec at a different model WITHOUT restarting the process --
// same PID, same Type, same ContextProbePath. A cache keyed on
// (pid, type, path) alone (the pre-#54 key) would keep serving the size
// probed against the OLD model forever, because none of those three ever
// changes; the cache must also invalidate on a Model change and re-probe
// with the NEW model.
//
// The fake ollama server ties its answer to the REQUESTED model (read from
// the POST body), not to the path or any fixed body, so a stale re-probe
// that still sent the old model would be caught by the wrong context size
// coming back, not just by the hit counter.
func TestCollectOnceRuntimeContextCacheInvalidatesOnModelChange(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&hits, 1)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Model {
		case "llama3":
			_, _ = w.Write([]byte(`{"model_info":{"llama.context_length":8192}}`))
		case "llama3:70b":
			_, _ = w.Write([]byte(`{"model_info":{"llama.context_length":4096}}`))
		default:
			http.Error(w, "unexpected model", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	baseStatus := runtimectl.Status{
		SpecID:           "rspec_model_change",
		Model:            "llama3",
		State:            runtimectl.StateRunning,
		PID:              4343, // unchanged across both cycles -- no restart.
		Port:             portFromURL(t, srv.URL),
		Type:             "ollama",
		ContextProbePath: "/api/show",
	}
	drv.setStatuses([]runtimectl.Status{baseStatus})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	a.collectOnce(context.Background())
	first := poster.first()
	if first == nil || len(first.Runtimes) != 1 {
		t.Fatalf("Runtimes (cycle 1) = %+v", first)
	}
	if got := first.Runtimes[0].ContextSize; got != 8192 {
		t.Fatalf("ContextSize (cycle 1) = %d, want 8192", got)
	}
	// TWO /api/show POSTs, not one, and both are this spec's own: the
	// CONTEXT probe (this test's subject) and -- since task 5 branched
	// probeRuntimeChildProps onto ProbeOllamaVerdicts for an ollama-typed
	// child -- the CAPABILITY probe, which reads the same document to answer
	// a different question. That is the same shape a llama_cpp child has
	// always had (its context probe and its capability probe each GET
	// /props once per lifetime); only the baseline this counter starts from
	// moved. What the test pins is unchanged, and the guard below still
	// holds: both probes are once-per-child-lifetime, so a model-blind
	// context-cache key would leave the count at 2 after cycle 2.
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("/api/show hits after cycle 1 = %d, want 2 (one context probe + one capability probe)", got)
	}

	// The spec's MODEL changes (operator edit; runtime manager
	// reconciliation) WITHOUT a restart: same SpecID, same PID, same Type,
	// same ContextProbePath, new Model.
	changed := baseStatus
	changed.Model = "llama3:70b"
	drv.setStatuses([]runtimectl.Status{changed})

	a.collectOnce(context.Background())
	last := poster.last()
	if last == nil || len(last.Runtimes) != 1 {
		t.Fatalf("Runtimes (cycle 2) = %+v", last)
	}
	if got := last.Runtimes[0].ContextSize; got != 4096 {
		t.Errorf("ContextSize (cycle 2) = %d, want 4096 (must re-probe with the NEW model, not serve the cached old-model value)", got)
	}
	// FOUR now, not three, and the increment is the fix round's second
	// decision: BOTH caches invalidate on a model change. The context probe
	// re-ran (this test's own subject, #54) and so did the CAPABILITY probe
	// -- runtimeCapabilityEntry gained model for the same reason
	// runtimeCtxEntry did, because one `ollama serve` process serves many
	// models and a (SpecID, PID)-only key served the previous model's
	// verdicts for the life of that PID. The assertion's MEANING is
	// unchanged: a model-blind context-cache key still leaves this counter
	// short and still hands cycle 2 the stale 8192 above.
	if got := atomic.LoadInt32(&hits); got != 4 {
		t.Errorf("/api/show hits after cycle 2 = %d, want 4 (the context probe AND the capability probe each re-ran for the new model) -- a model-blind cache key would never re-probe", got)
	}
}

// TestCollectOnceRuntimeContextRouterModeReported pins issue #55: a llama.cpp
// server in multi-model ROUTER mode answers GET /props with a dummy carrying
// "role": "router" and n_ctx 0. Rather than take the non-positive-size exit
// (recorded as "unreachable", indistinguishable from a broken server), the
// context probe reports the mode itself as "router", and no size is recorded.
func TestCollectOnceRuntimeContextRouterModeReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"role":"router","model_path":"none","default_generation_settings":{"n_ctx":0}}`))
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID:           "rspec_router",
			Model:            "qwen-coder",
			State:            runtimectl.StateRunning,
			PID:              5151,
			Port:             portFromURL(t, srv.URL),
			Type:             "llama_cpp",
			ContextProbePath: "/props",
		},
	})

	poster := &capturePoster{}
	a := NewFromDeps(config.Config{Interval: time.Hour}, Deps{Poster: poster, RuntimeDriver: drv})

	a.collectOnce(context.Background())
	first := poster.first()
	if first == nil || len(first.Runtimes) != 1 {
		t.Fatalf("Runtimes = %+v", first)
	}
	if got := first.Runtimes[0].ContextProbe; got != "router" {
		t.Errorf("ContextProbe = %q, want %q (router mode must be distinguishable from unreachable)", got, "router")
	}
	if got := first.Runtimes[0].ContextSize; got != 0 {
		t.Errorf("ContextSize = %d, want 0 (a router dummy's n_ctx is not a measured size)", got)
	}
}

// TestCollectOnceRuntimeContextZeroSizeNotCached is FIX 4's proof: a context
// probe that succeeds but returns a non-positive size (a JSON field present
// but literally 0) is effectively "unknown" and must NOT be cached as final
// -- it must be retried next cycle like a failed probe, so a transient 0
// (e.g. the child still warming up) can recover once real data is served.
func TestCollectOnceRuntimeContextZeroSizeNotCached(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		n := atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write([]byte(`{"data":[{"id":"m1","max_model_len":0}]}`))
		} else {
			_, _ = w.Write([]byte(`{"data":[{"id":"m1","max_model_len":8192}]}`))
		}
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID:           "rspec_4",
			Model:            "qwen-coder",
			State:            runtimectl.StateRunning,
			PID:              4343,
			Port:             portFromURL(t, srv.URL),
			Type:             "vllm",
			ContextProbePath: "/v1/models",
		},
	})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	a.collectOnce(context.Background())
	first := poster.first()
	if first == nil || len(first.Runtimes) != 1 {
		t.Fatalf("Runtimes (cycle 1) = %+v", first)
	}
	if got := first.Runtimes[0].ContextSize; got != 0 {
		t.Fatalf("ContextSize (cycle 1) = %d, want 0", got)
	}
	// FIX 7: this cycle takes the non-positive-size exit, one of the probe's
	// three "unreachable" exits, and asserted only the resulting zero -- the
	// very thing the reachability field exists to distinguish from a real
	// measurement.
	if got := first.Runtimes[0].ContextProbe; got != "unreachable" {
		t.Errorf("ContextProbe (cycle 1) = %q, want %q (a non-positive size is not a successful probe)", got, "unreachable")
	}
	if hits := atomic.LoadInt32(&hits); hits != 1 {
		t.Fatalf("probe hits after cycle 1 = %d, want 1", hits)
	}

	a.collectOnce(context.Background())
	last := poster.last()
	if last == nil || len(last.Runtimes) != 1 {
		t.Fatalf("Runtimes (cycle 2) = %+v", last)
	}
	if got := last.Runtimes[0].ContextSize; got != 8192 {
		t.Errorf("ContextSize (cycle 2) = %d, want 8192 (a cached 0 would never re-probe)", got)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("probe hits after cycle 2 = %d, want 2 (a 0-size result must not be cached)", got)
	}
}

// TestCollectOnceRuntimeProbeUnreachableLeavesZero proves a StateRunning
// child whose probe endpoints are unreachable still produces a complete,
// error-free sample: ContextSize/ActiveRequests/QueueDepth all stay 0, and
// collectOnce neither panics nor fails the cycle.
func TestCollectOnceRuntimeProbeUnreachableLeavesZero(t *testing.T) {
	// Bind then immediately close: the returned port is refusing
	// connections, not merely unassigned, which is what "unreachable" means
	// here (a wedged/dead child, not a slow one).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID:           "rspec_2",
			Model:            "broken",
			State:            runtimectl.StateRunning,
			PID:              99,
			Port:             port,
			Type:             "vllm",
			MetricsPath:      "/metrics",
			ContextProbePath: "/v1/models",
		},
	})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	a.collectOnce(context.Background())

	got := poster.first()
	if got == nil || len(got.Runtimes) != 1 {
		t.Fatalf("Runtimes = %+v", got)
	}
	rs := got.Runtimes[0]
	if rs.ContextSize != 0 || rs.ActiveRequests != 0 || rs.QueueDepth != 0 {
		t.Errorf("unreachable-probe fields = %+v, want all zero", rs)
	}
	// FIX 7 of the final whole-branch review: the numbers staying zero is only
	// half the story, and the half that used to be indistinguishable from a
	// genuinely idle child. BOTH probes failed here (a port refusing
	// connections), so both must SAY so -- this is the test named for the
	// unreachable case, and it asserted nothing about the two fields that
	// exist to report it.
	if rs.MetricsProbe != "unreachable" {
		t.Errorf("MetricsProbe = %q, want %q (the scrape hit a refusing port)", rs.MetricsProbe, "unreachable")
	}
	if rs.ContextProbe != "unreachable" {
		t.Errorf("ContextProbe = %q, want %q (the context probe hit the same refusing port)", rs.ContextProbe, "unreachable")
	}
}

// TestCollectOnceRuntimeProbeStatesBothOK proves a StateRunning child whose
// /metrics and context endpoints both serve valid responses reports
// MetricsProbe=="ok" and ContextProbe=="ok" -- the three-state reachability
// fields Task 2 adds alongside the existing numeric probe results.
func TestCollectOnceRuntimeProbeStatesBothOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			_, _ = w.Write([]byte("vllm:num_requests_running 1\nvllm:num_requests_waiting 0\n"))
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"m1","max_model_len":8192}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID:           "rspec_both_ok",
			Model:            "qwen-coder",
			State:            runtimectl.StateRunning,
			PID:              5001,
			Port:             portFromURL(t, srv.URL),
			Type:             "vllm",
			MetricsPath:      "/metrics",
			ContextProbePath: "/v1/models",
		},
	})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	a.collectOnce(context.Background())

	got := poster.last()
	if got == nil || len(got.Runtimes) != 1 {
		t.Fatalf("Runtimes = %+v", got)
	}
	rs := got.Runtimes[0]
	if rs.MetricsProbe != "ok" {
		t.Errorf("MetricsProbe = %q, want %q", rs.MetricsProbe, "ok")
	}
	if rs.ContextProbe != "ok" {
		t.Errorf("ContextProbe = %q, want %q", rs.ContextProbe, "ok")
	}
}

// TestCollectOnceRuntimeProbeStatesMetricsRefusedContextOK proves a
// StateRunning child whose /metrics endpoint refuses to serve a response
// (the connection is hung up on, not merely 404) reports
// MetricsProbe=="unreachable" while an independently-working context probe
// on the SAME server still reports ContextProbe=="ok" -- the two states are
// computed and set independently.
func TestCollectOnceRuntimeProbeStatesMetricsRefusedContextOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			// Simulate a refusing endpoint: hijack the connection and hang up
			// without writing any HTTP response, forcing the client's Do to
			// return an error (a forgotten --metrics flag serving nothing
			// resembles this far more than a clean 404 would, since Scrape
			// does not itself check status codes).
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "hijack unsupported", http.StatusInternalServerError)
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			_ = conn.Close()
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"m1","max_model_len":8192}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID:           "rspec_metrics_refused",
			Model:            "qwen-coder",
			State:            runtimectl.StateRunning,
			PID:              5002,
			Port:             portFromURL(t, srv.URL),
			Type:             "vllm",
			MetricsPath:      "/metrics",
			ContextProbePath: "/v1/models",
		},
	})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	a.collectOnce(context.Background())

	got := poster.last()
	if got == nil || len(got.Runtimes) != 1 {
		t.Fatalf("Runtimes = %+v", got)
	}
	rs := got.Runtimes[0]
	if rs.MetricsProbe != "unreachable" {
		t.Errorf("MetricsProbe = %q, want %q", rs.MetricsProbe, "unreachable")
	}
	if rs.ContextProbe != "ok" {
		t.Errorf("ContextProbe = %q, want %q", rs.ContextProbe, "ok")
	}
}

// TestCollectOnceRuntimeProbeStatesEmptyMetricsPathNA proves a child with no
// MetricsPath configured (a runtime type/spec with no known metrics
// endpoint) reports MetricsProbe=="na" rather than "unreachable" -- "na"
// means "not applicable", not "failed".
func TestCollectOnceRuntimeProbeStatesEmptyMetricsPathNA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1","max_model_len":8192}]}`))
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID:           "rspec_no_metrics_path",
			Model:            "qwen-coder",
			State:            runtimectl.StateRunning,
			PID:              5003,
			Port:             portFromURL(t, srv.URL),
			Type:             "vllm",
			MetricsPath:      "",
			ContextProbePath: "/v1/models",
		},
	})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	a.collectOnce(context.Background())

	got := poster.last()
	if got == nil || len(got.Runtimes) != 1 {
		t.Fatalf("Runtimes = %+v", got)
	}
	rs := got.Runtimes[0]
	if rs.MetricsProbe != "na" {
		t.Errorf("MetricsProbe = %q, want %q", rs.MetricsProbe, "na")
	}
	if rs.ContextProbe != "ok" {
		t.Errorf("ContextProbe = %q, want %q", rs.ContextProbe, "ok")
	}
}

// TestCollectOnceRuntimeProbeStatesEmptyContextPathNA proves a child with no
// ContextProbePath configured reports ContextProbe=="na" rather than
// "unreachable".
func TestCollectOnceRuntimeProbeStatesEmptyContextPathNA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("vllm:num_requests_running 1\nvllm:num_requests_waiting 0\n"))
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID:           "rspec_no_context_path",
			Model:            "qwen-coder",
			State:            runtimectl.StateRunning,
			PID:              5004,
			Port:             portFromURL(t, srv.URL),
			Type:             "vllm",
			MetricsPath:      "/metrics",
			ContextProbePath: "",
		},
	})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	a.collectOnce(context.Background())

	got := poster.last()
	if got == nil || len(got.Runtimes) != 1 {
		t.Fatalf("Runtimes = %+v", got)
	}
	rs := got.Runtimes[0]
	if rs.ContextProbe != "na" {
		t.Errorf("ContextProbe = %q, want %q", rs.ContextProbe, "na")
	}
	if rs.MetricsProbe != "ok" {
		t.Errorf("MetricsProbe = %q, want %q", rs.MetricsProbe, "ok")
	}
}

// TestCollectOnceRuntimeProbeStatesContextZeroSizeUnreachable proves a
// context probe that succeeds at the HTTP level but yields a non-positive
// size (the deliberate "unknown, don't cache" case already covered by
// TestCollectOnceRuntimeContextZeroSizeNotCached) reports
// ContextProbe=="unreachable", not "ok" -- a size of 0 is not a successful
// probe.
func TestCollectOnceRuntimeProbeStatesContextZeroSizeUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1","max_model_len":0}]}`))
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID:           "rspec_zero_size",
			Model:            "qwen-coder",
			State:            runtimectl.StateRunning,
			PID:              5005,
			Port:             portFromURL(t, srv.URL),
			Type:             "vllm",
			ContextProbePath: "/v1/models",
		},
	})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	a.collectOnce(context.Background())

	got := poster.last()
	if got == nil || len(got.Runtimes) != 1 {
		t.Fatalf("Runtimes = %+v", got)
	}
	rs := got.Runtimes[0]
	if rs.ContextProbe != "unreachable" {
		t.Errorf("ContextProbe = %q, want %q", rs.ContextProbe, "unreachable")
	}
}

// TestCollectOnceRuntimeProbeStatesContextCacheHitOK covers the context
// probe's cache-HIT exit, which is the branch that runs on every collect
// cycle after the first for every running child -- the steady state, and the
// only "ok" exit that reports reachability without touching the endpoint at
// all. It had no test: TestCollectOnceRuntimeProbesMetricsAndContext proves
// the SIZE survives the cache (hits stay at 1), but never looked at
// ContextProbe, so a cache hit returning "" or "na" would have gone
// unnoticed and every steady-state row would have lost its context chip a
// cycle after loading.
//
// Cycle 1 probes and reports "ok"; cycle 2 must still report "ok" while the
// endpoint is NOT re-queried (hits stay 1) -- proving the state comes from
// the cache-hit branch, not from a second live probe.
func TestCollectOnceRuntimeProbeStatesContextCacheHitOK(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1","max_model_len":8192}]}`))
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID:           "rspec_cache_hit",
			Model:            "qwen-coder",
			State:            runtimectl.StateRunning,
			PID:              5007, // unchanged across both cycles: no restart.
			Port:             portFromURL(t, srv.URL),
			Type:             "vllm",
			ContextProbePath: "/v1/models",
		},
	})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	a.collectOnce(context.Background())
	first := poster.first()
	if first == nil || len(first.Runtimes) != 1 {
		t.Fatalf("Runtimes (cycle 1) = %+v", first)
	}
	if got := first.Runtimes[0].ContextProbe; got != "ok" {
		t.Fatalf("ContextProbe (cycle 1) = %q, want %q (the live probe succeeded)", got, "ok")
	}

	a.collectOnce(context.Background())
	last := poster.last()
	if last == nil || len(last.Runtimes) != 1 {
		t.Fatalf("Runtimes (cycle 2) = %+v", last)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("context probe hits after cycle 2 = %d, want 1 -- cycle 2 must be a CACHE HIT for this test to cover that exit", got)
	}
	if got := last.Runtimes[0].ContextProbe; got != "ok" {
		t.Errorf("ContextProbe (cycle 2, cache hit) = %q, want %q (a cached size means a prior probe on this generation succeeded)", got, "ok")
	}
	if got := last.Runtimes[0].ContextSize; got != 8192 {
		t.Errorf("ContextSize (cycle 2, cache hit) = %d, want 8192", got)
	}
}

// TestCollectOnceRuntimeLiveProgressCustomTypeSupported is task 4's core
// recovery proof: a "custom"-typed child -- the effective type
// routing.DeriveProbePaths gives NO context path at all, so ContextProbe
// stays "na" and nothing is ever fetched for context -- still gets its
// live-progress capability determined, because probeRuntimeChildProps
// GETs collector.LiveProgressProbePath ("/props") unconditionally, never
// gated on Type or ContextProbePath. Without this, a llama.cpp build
// launched without an explicit Type would never be probed for this
// capability at all.
func TestCollectOnceRuntimeLiveProgressCustomTypeSupported(t *testing.T) {
	var propsHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&propsHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":8192,"params":{"timings_per_token":false}}}`))
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID: "rspec_custom_supported",
			Model:  "mystery-model",
			State:  runtimectl.StateRunning,
			PID:    6001,
			Port:   portFromURL(t, srv.URL),
			Type:   "custom",
			// No MetricsPath/ContextProbePath -- routing.DeriveProbePaths
			// gives a "custom" spec neither, exactly mirroring the real
			// wire shape the gateway would send for it.
		},
	})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	a.collectOnce(context.Background())
	got := poster.last()
	if got == nil || len(got.Runtimes) != 1 {
		t.Fatalf("Runtimes = %+v", got)
	}
	rs := got.Runtimes[0]
	if rs.ContextProbe != "na" {
		t.Errorf("ContextProbe = %q, want %q (a custom spec has no context path)", rs.ContextProbe, "na")
	}
	if rs.LiveProgressSupport != "supported" {
		t.Errorf("LiveProgressSupport = %q, want %q (the custom-type recovery case)", rs.LiveProgressSupport, "supported")
	}
	if hits := atomic.LoadInt32(&propsHits); hits != 1 {
		t.Errorf("/props hits = %d, want 1 (the capability probe must GET /props for a custom-typed child)", hits)
	}
}

// TestCapabilitiesSampleCarriesEveryVerdictAsARow proves capabilitiesSample
// converts each named collector.Capabilities field into its own
// CapabilityVerdict entry, keyed by name, and skips every undetermined
// ("") field entirely -- there must be no entry at all for Video/Audio here,
// mirroring the store's row-absence-means-unknown model (#49-2, task 2).
func TestCapabilitiesSampleCarriesEveryVerdictAsARow(t *testing.T) {
	got := capabilitiesSample(collector.Capabilities{Vision: "yes", Tools: "no"}, sample.CapabilitySourceLlamaCppProps)
	if got == nil {
		t.Fatal("capabilitiesSample = nil, want a non-nil pointer")
	}
	if got.Source != sample.CapabilitySourceLlamaCppProps {
		t.Errorf("Source = %q, want %q -- the caller's probe name is carried onto the wrapper, not re-derived downstream", got.Source, sample.CapabilitySourceLlamaCppProps)
	}
	want := []sample.CapabilityVerdict{
		{Name: "vision", Verdict: "yes"},
		{Name: "tools", Verdict: "no"},
	}
	if !reflect.DeepEqual(got.Verdicts, want) {
		t.Errorf("Verdicts = %+v, want exactly %+v (no entry for undetermined video/audio)", got.Verdicts, want)
	}
}

// TestCapabilitiesSampleAllEmptyIsNonNilAndEmpty proves an all-empty
// collector.Capabilities -- e.g. the zero value a router-mode /props dummy or
// a 401/403 CONCLUSIVE refusal detects -- still yields a non-nil pointer
// whose Verdicts is itself non-nil and empty. This is the distinction the
// whole pointer wrapper exists for: nil means "this agent predates
// capability detection", while this non-nil-but-empty value means
// "detection ran and determined nothing".
func TestCapabilitiesSampleAllEmptyIsNonNilAndEmpty(t *testing.T) {
	got := capabilitiesSample(collector.Capabilities{}, sample.CapabilitySourceOllamaAPIShow)
	if got == nil {
		t.Fatal("capabilitiesSample = nil, want a non-nil pointer even when every field is undetermined")
	}
	if got.Source != sample.CapabilitySourceOllamaAPIShow {
		t.Errorf("Source = %q, want %q -- \"this detector ran and determined nothing\" names the detector too, and it costs no row either way", got.Source, sample.CapabilitySourceOllamaAPIShow)
	}
	if got.Verdicts == nil {
		t.Error("Verdicts = nil, want a non-nil (but empty) slice")
	}
	if len(got.Verdicts) != 0 {
		t.Errorf("Verdicts = %+v, want empty", got.Verdicts)
	}
}

// TestCapabilitiesSampleEmitsImageBetweenNamedFieldsAndExtra pins where the
// image verdict lands on the wire: after the four other named fields and
// before every Extra entry, in both directions, and not at all when it is
// undetermined. The position is the load-bearing part. The gateway keeps the
// FIRST entry for a name, so an "image" that also appears in Extra (Extra
// can only say "yes") must lose to the structured field, and the second case
// shows exactly that collision.
func TestCapabilitiesSampleEmitsImageBetweenNamedFieldsAndExtra(t *testing.T) {
	cases := []struct {
		name string
		caps collector.Capabilities
		want []sample.CapabilityVerdict
	}{
		{
			name: "yes follows the four named fields and precedes Extra",
			caps: collector.Capabilities{Vision: "yes", Video: "no", Audio: "no", Tools: "yes", Image: "yes", Extra: []string{"thinking"}},
			want: []sample.CapabilityVerdict{
				{Name: "vision", Verdict: "yes"},
				{Name: "video", Verdict: "no"},
				{Name: "audio", Verdict: "no"},
				{Name: "tools", Verdict: "yes"},
				{Name: "image", Verdict: "yes"},
				{Name: "thinking", Verdict: "yes"},
			},
		},
		{
			name: "no is carried, ahead of an Extra entry of the same name",
			caps: collector.Capabilities{Image: "no", Extra: []string{"image"}},
			want: []sample.CapabilityVerdict{
				{Name: "image", Verdict: "no"},
				{Name: "image", Verdict: "yes"},
			},
		},
		{
			name: "undetermined is omitted",
			caps: collector.Capabilities{Vision: "yes", Extra: []string{"thinking"}},
			want: []sample.CapabilityVerdict{
				{Name: "vision", Verdict: "yes"},
				{Name: "thinking", Verdict: "yes"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := capabilitiesSample(tc.caps, sample.CapabilitySourceSdcppCapabilities)
			if got == nil {
				t.Fatal("capabilitiesSample = nil, want a non-nil pointer")
			}
			if !reflect.DeepEqual(got.Verdicts, tc.want) {
				t.Errorf("Verdicts = %+v, want exactly %+v", got.Verdicts, tc.want)
			}
			// The literal, not the constant: the gateway matches this string
			// byte for byte, so the constant's value is what is under test.
			if got.Source != "sdcpp_capabilities" {
				t.Errorf("Source = %q, want %q", got.Source, "sdcpp_capabilities")
			}
		})
	}
}

// TestCollectOnceRuntimeCapabilitiesCachedAcrossCycles proves the widened
// cache carries capabilities too, exactly as it already does for
// LiveProgressSupport (TestCollectOnceRuntimeLiveProgressApiKeyRefusalCachedAcrossCycles
// and siblings): a determined verdict set from one /props document is
// fetched once and reused across every later collect cycle for the same
// pid, so a second cycle re-probes nothing -- and the sample still reports
// the capability verdicts from the cached entry.
func TestCollectOnceRuntimeCapabilitiesCachedAcrossCycles(t *testing.T) {
	var propsHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&propsHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"default_generation_settings":{"params":{"timings_per_token":false}},
		                        "modalities":{"vision":true,"video":false,"audio":false},
		                        "chat_template_caps":{"supports_tools":true}}`))
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID: "rspec_caps_cached",
			Model:  "vision-model",
			State:  runtimectl.StateRunning,
			PID:    6020,
			Port:   portFromURL(t, srv.URL),
			Type:   "custom",
		},
	})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	want := &sample.Capabilities{
		Verdicts: []sample.CapabilityVerdict{
			{Name: "vision", Verdict: "yes"},
			{Name: "video", Verdict: "no"},
			{Name: "audio", Verdict: "no"},
			{Name: "tools", Verdict: "yes"},
		},
		// Source is part of the expected value since #54: the /props probe
		// NAMES itself on the wire, so the gateway attributes its rows to
		// the document they came from instead of inferring the provenance.
		Source: sample.CapabilitySourceLlamaCppProps,
	}
	const cycles = 3
	for cycle := 1; cycle <= cycles; cycle++ {
		a.collectOnce(context.Background())
		got := poster.last()
		if got == nil || len(got.Runtimes) != 1 {
			t.Fatalf("cycle %d: Runtimes = %+v", cycle, got)
		}
		rs := got.Runtimes[0]
		if rs.LiveProgressSupport != "supported" {
			t.Errorf("cycle %d: LiveProgressSupport = %q, want %q", cycle, rs.LiveProgressSupport, "supported")
		}
		if rs.Capabilities == nil || !reflect.DeepEqual(*rs.Capabilities, *want) {
			t.Errorf("cycle %d: Capabilities = %+v, want %+v", cycle, rs.Capabilities, want)
		}
		if hits := atomic.LoadInt32(&propsHits); hits != 1 {
			t.Fatalf("cycle %d: /props hits = %d, want 1 (a determined verdict set -- capabilities included -- must be cached across cycles)", cycle, hits)
		}
	}
}

// TestCollectOnceRuntimeCapabilitiesPidChangeRearms proves a PID change
// re-arms the question for capabilities exactly as it does for the
// live-progress verdict (TestCollectOnceRuntimeLiveProgressPidChangeRearmsStableUnknown):
// a restarted child under a new pid is re-probed -- the hit counter grows --
// and the sample after the restart carries the freshly-probed capabilities,
// not a stale copy read through the old pid's cache entry.
func TestCollectOnceRuntimeCapabilitiesPidChangeRearms(t *testing.T) {
	var propsHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&propsHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"default_generation_settings":{"params":{"timings_per_token":false}},
		                        "modalities":{"vision":true,"video":false,"audio":false},
		                        "chat_template_caps":{"supports_tools":true}}`))
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	pid := 6021
	setStatuses := func() {
		drv.setStatuses([]runtimectl.Status{
			{
				SpecID: "rspec_caps_pid_rearm",
				State:  runtimectl.StateRunning,
				PID:    pid,
				Port:   portFromURL(t, srv.URL),
				Type:   "custom",
			},
		})
	}
	setStatuses()

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	want := &sample.Capabilities{
		Verdicts: []sample.CapabilityVerdict{
			{Name: "vision", Verdict: "yes"},
			{Name: "video", Verdict: "no"},
			{Name: "audio", Verdict: "no"},
			{Name: "tools", Verdict: "yes"},
		},
		// Source is part of the expected value since #54: the /props probe
		// NAMES itself on the wire, so the gateway attributes its rows to
		// the document they came from instead of inferring the provenance.
		Source: sample.CapabilitySourceLlamaCppProps,
	}

	a.collectOnce(context.Background())
	a.collectOnce(context.Background())
	if hits := atomic.LoadInt32(&propsHits); hits != 1 {
		t.Fatalf("/props hits before restart = %d, want 1 (cached across cycles for the same pid)", hits)
	}
	got := poster.last()
	if got == nil || len(got.Runtimes) != 1 {
		t.Fatalf("Runtimes = %+v", got)
	}
	if rs := got.Runtimes[0]; rs.Capabilities == nil || !reflect.DeepEqual(*rs.Capabilities, *want) {
		t.Fatalf("Capabilities before restart = %+v, want %+v", rs.Capabilities, want)
	}

	// The child restarts: same SpecID, a new pid.
	pid = 6022
	setStatuses()
	a.collectOnce(context.Background())
	if hits := atomic.LoadInt32(&propsHits); hits != 2 {
		t.Errorf("/props hits after restart = %d, want 2 (a changed pid must re-arm the question for capabilities too)", hits)
	}
	got = poster.last()
	if got == nil || len(got.Runtimes) != 1 {
		t.Fatalf("Runtimes = %+v", got)
	}
	if rs := got.Runtimes[0]; rs.Capabilities == nil || !reflect.DeepEqual(*rs.Capabilities, *want) {
		t.Errorf("Capabilities after restart = %+v, want %+v", rs.Capabilities, want)
	}
}

// TestCollectOnceRuntimeCapabilityCacheInvalidatesOnModelChange is the
// capability cache's own #54: the runtime manager can repoint a RUNNING
// `ollama serve` spec at a different model WITHOUT restarting the process --
// same SpecID, same PID, same Type -- and ONE Ollama process serves MANY
// models, so that is a routine reconfiguration rather than an exotic one.
// A cache keyed on (SpecID, PID) alone kept serving the verdict set probed
// against the OLD model for the entire life of that PID: a text-only model
// would inherit the vision model's vision=yes, which is a perfectly
// ordinary-looking answer and would surface as a wrong routing decision, not
// as an error. The cache must invalidate on a Model change and re-probe with
// the new model, exactly as runtimeCtxCache has since task 4.
//
// The fake server ties its answer to the REQUESTED model (read from the POST
// body), not to the path or a fixed body -- the #54 discipline -- so a stale
// re-probe that still sent the old model is caught by the wrong VERDICTS
// coming back and not merely by the hit counter. The spec carries no
// ContextProbePath on purpose: the context probe is not this test's subject,
// and leaving it out keeps every /api/show hit here the capability probe's
// own.
func TestCollectOnceRuntimeCapabilityCacheInvalidatesOnModelChange(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&hits, 1)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Model {
		case "llama3.2-vision:11b":
			_, _ = w.Write([]byte(`{"capabilities":["completion","vision"]}`))
		case "qwen3:8b":
			_, _ = w.Write([]byte(`{"capabilities":["completion","tools"]}`))
		default:
			http.Error(w, "unexpected model", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	baseStatus := runtimectl.Status{
		SpecID: "rspec_caps_model_change",
		Model:  "llama3.2-vision:11b",
		State:  runtimectl.StateRunning,
		PID:    6040, // unchanged across both cycles -- no restart.
		Port:   portFromURL(t, srv.URL),
		Type:   "ollama",
	}
	drv.setStatuses([]runtimectl.Status{baseStatus})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	wantVision := &sample.Capabilities{
		Verdicts: []sample.CapabilityVerdict{{Name: "vision", Verdict: "yes"}},
		Source:   sample.CapabilitySourceOllamaAPIShow,
	}
	a.collectOnce(context.Background())
	first := poster.first()
	if first == nil || len(first.Runtimes) != 1 {
		t.Fatalf("Runtimes (cycle 1) = %+v", first)
	}
	if rs := first.Runtimes[0]; rs.Capabilities == nil || !reflect.DeepEqual(*rs.Capabilities, *wantVision) {
		t.Fatalf("Capabilities (cycle 1) = %+v, want %+v", rs.Capabilities, wantVision)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("/api/show hits after cycle 1 = %d, want 1", got)
	}

	// The spec's MODEL changes (operator edit; runtime manager
	// reconciliation) WITHOUT a restart: same SpecID, same PID, same Type,
	// new Model.
	changed := baseStatus
	changed.Model = "qwen3:8b"
	drv.setStatuses([]runtimectl.Status{changed})

	wantTools := &sample.Capabilities{
		Verdicts: []sample.CapabilityVerdict{{Name: "tools", Verdict: "yes"}},
		Source:   sample.CapabilitySourceOllamaAPIShow,
	}
	a.collectOnce(context.Background())
	last := poster.last()
	if last == nil || len(last.Runtimes) != 1 {
		t.Fatalf("Runtimes (cycle 2) = %+v", last)
	}
	if rs := last.Runtimes[0]; rs.Capabilities == nil || !reflect.DeepEqual(*rs.Capabilities, *wantTools) {
		t.Errorf("Capabilities (cycle 2) = %+v, want %+v (the NEW model's declared set, not the cached old model's)", rs.Capabilities, wantTools)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("/api/show hits after cycle 2 = %d, want 2 -- a model-blind capability-cache key would never re-probe", got)
	}
}

// TestCollectOnceRuntimeCapabilitiesOllamaProbesAPIShow is task 5's wiring
// proof: an "ollama"-typed running child has its capability verdict set read
// from POST /api/show (collector.ProbeOllamaVerdicts), never from /props.
//
// The fake server ASSERTS the request it receives instead of discarding it,
// the discipline #54 established -- a probe silently issuing the wrong
// method or path is the exact class of bug that shipped while the request
// was never asserted: path /api/show, method POST, body {"model": st.Model}
// verbatim. /props is counted separately and must stay at ZERO. Ollama
// serves no /props at all, so an unbranched probe would spend a round trip
// to learn nothing and report no capability at all.
//
// The expected verdict set carries three of detectOllamaCapabilities' rules
// through the whole wire path: "vision"/"tools" land on the STRUCTURED
// fields (which capabilitiesSample emits first), an unrecognised "thinking"
// lands in Extra as a "yes", and "completion" is dropped entirely --
// upstream ASSUMES that one rather than detecting it, so it is not evidence
// of anything. Nothing is ever "no", and there is deliberately no
// video/audio entry: Ollama's array is not exhaustive, so an absent name
// means "Ollama did not tell us", never a denial. LiveProgressSupport stays
// "" for that same reason -- Ollama exposes no timings_per_token-style
// surface, and an unknown must not become a permanent false "unsupported".
func TestCollectOnceRuntimeCapabilitiesOllamaProbesAPIShow(t *testing.T) {
	var showHits, propsHits int32
	var mu sync.Mutex
	var gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" {
			if r.URL.Path == "/props" {
				atomic.AddInt32(&propsHits, 1)
			}
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&showHits, 1)
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotMethod, gotBody = r.Method, string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"capabilities":["completion","vision","tools","thinking"]}`))
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID: "rspec_ollama_caps",
			Model:  "llama3.2-vision:11b",
			State:  runtimectl.StateRunning,
			PID:    6030,
			Port:   portFromURL(t, srv.URL),
			Type:   "ollama",
		},
	})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	want := &sample.Capabilities{
		Verdicts: []sample.CapabilityVerdict{
			{Name: "vision", Verdict: "yes"},
			{Name: "tools", Verdict: "yes"},
			{Name: "thinking", Verdict: "yes"},
		},
		// The point of #54's fix round: the sample says WHERE these verdicts
		// came from, so an Ollama-declared verdict never reaches an operator
		// stamped with llama.cpp's source. The gateway cannot derive this --
		// the wire carries no runtime type -- and must not guess it.
		Source: sample.CapabilitySourceOllamaAPIShow,
	}
	const cycles = 3
	for cycle := 1; cycle <= cycles; cycle++ {
		a.collectOnce(context.Background())
		got := poster.last()
		if got == nil || len(got.Runtimes) != 1 {
			t.Fatalf("cycle %d: Runtimes = %+v", cycle, got)
		}
		rs := got.Runtimes[0]
		if rs.Capabilities == nil || !reflect.DeepEqual(*rs.Capabilities, *want) {
			t.Fatalf("cycle %d: Capabilities = %+v, want %+v (Ollama's declared names, structured fields first, completion dropped)", cycle, rs.Capabilities, want)
		}
		if rs.LiveProgressSupport != "" {
			t.Errorf("cycle %d: LiveProgressSupport = %q, want %q -- Ollama exposes no timings_per_token surface, and an unknown must never become a denial", cycle, rs.LiveProgressSupport, "")
		}
		if hits := atomic.LoadInt32(&showHits); hits != 1 {
			t.Fatalf("cycle %d: /api/show hits = %d, want 1 (a determined verdict set must be cached for this pid)", cycle, hits)
		}
		if hits := atomic.LoadInt32(&propsHits); hits != 0 {
			t.Fatalf("cycle %d: /props hits = %d, want 0 -- an ollama-typed child must never be asked for /props", cycle, hits)
		}
	}

	mu.Lock()
	method, body := gotMethod, gotBody
	mu.Unlock()
	if method != http.MethodPost {
		t.Errorf("/api/show method = %q, want %q (upstream is POST-only: since v0.7.0 a GET answers 405)", method, http.MethodPost)
	}
	if want := `{"model":"llama3.2-vision:11b"}`; body != want {
		t.Errorf("/api/show body = %q, want %q (the probed model is the spec's own)", body, want)
	}
}

// TestCollectOnceRuntimeCapabilitiesLlamaCppStillProbesProps pins the
// fallback half of the probe's branch: only "ollama" and
// "stable_diffusion_cpp" are claimed by name, and every other type --
// "llama_cpp" here, "custom" in the tests above, which matters most because
// "custom" is the type-detection FALLBACK and gets no derived probe path at
// all -- still GETs /props and is byte-identical to before the branch
// existed. /api/show is counted and must stay at zero.
//
// Inverting the branch's condition (probing /api/show for everything BUT
// ollama) fails here on both counters at once.
func TestCollectOnceRuntimeCapabilitiesLlamaCppStillProbesProps(t *testing.T) {
	var showHits, propsHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			if r.URL.Path == "/api/show" {
				atomic.AddInt32(&showHits, 1)
			}
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&propsHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"default_generation_settings":{"params":{"timings_per_token":true}},
		                        "modalities":{"vision":true,"video":false,"audio":false},
		                        "chat_template_caps":{"supports_tools":true}}`))
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID: "rspec_llamacpp_caps_unchanged",
			Model:  "qwen-coder",
			State:  runtimectl.StateRunning,
			PID:    6031,
			Port:   portFromURL(t, srv.URL),
			Type:   "llama_cpp",
		},
	})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	a.collectOnce(context.Background())
	got := poster.last()
	if got == nil || len(got.Runtimes) != 1 {
		t.Fatalf("Runtimes = %+v", got)
	}
	rs := got.Runtimes[0]
	want := &sample.Capabilities{
		Verdicts: []sample.CapabilityVerdict{
			{Name: "vision", Verdict: "yes"},
			{Name: "video", Verdict: "no"},
			{Name: "audio", Verdict: "no"},
			{Name: "tools", Verdict: "yes"},
		},
		// Source is part of the expected value since #54: the /props probe
		// NAMES itself on the wire, so the gateway attributes its rows to
		// the document they came from instead of inferring the provenance.
		Source: sample.CapabilitySourceLlamaCppProps,
	}
	if rs.Capabilities == nil || !reflect.DeepEqual(*rs.Capabilities, *want) {
		t.Errorf("Capabilities = %+v, want %+v (a llama.cpp /props document's own verdicts, including the \"no\"s only that document can justify)", rs.Capabilities, want)
	}
	if rs.LiveProgressSupport != "supported" {
		t.Errorf("LiveProgressSupport = %q, want %q (the /props verdict, unaffected by the ollama branch)", rs.LiveProgressSupport, "supported")
	}
	if hits := atomic.LoadInt32(&propsHits); hits != 1 {
		t.Errorf("/props hits = %d, want 1", hits)
	}
	if hits := atomic.LoadInt32(&showHits); hits != 0 {
		t.Errorf("/api/show hits = %d, want 0 -- only an ollama-typed child may be asked for it", hits)
	}
}

// fakeGatewayFeatures is a gatewayFeatureSource whose declared set a test can
// change between calls, which is how a gateway upgrade looks to a child that
// keeps running. It counts every Fetch.
type fakeGatewayFeatures struct {
	mu    sync.Mutex
	names []string
	err   error
	calls int
}

func (f *fakeGatewayFeatures) Fetch(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.names, f.err
}

func (f *fakeGatewayFeatures) set(names []string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names, f.err = names, err
}

func (f *fakeGatewayFeatures) fetches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// The production source main.go wires in must satisfy the agent's interface.
var _ gatewayFeatureSource = (*runtimectl.FeaturesClient)(nil)

// probeChild is a fake managed child that records every request as
// "METHOD path" and answers only the routes it was given, 404 elsewhere. The
// routes are literals, never the collector's path constants: comparing a
// constant with itself would not notice it drifting from the path the real
// server serves.
type probeChild struct {
	srv *httptest.Server

	mu     sync.Mutex
	routes map[string]string // "METHOD path" -> JSON body
	seen   map[string]int    // "METHOD path" -> request count
}

func newProbeChild(t *testing.T, routes map[string]string) *probeChild {
	t.Helper()
	c := &probeChild{routes: make(map[string]string, len(routes)), seen: map[string]int{}}
	for k, v := range routes {
		c.routes[k] = v
	}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		c.mu.Lock()
		c.seen[key]++
		body, ok := c.routes[key]
		c.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// serve replaces the body one route answers with.
func (c *probeChild) serve(key, body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.routes[key] = body
}

// requests returns a copy of every request seen so far.
func (c *probeChild) requests() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.seen))
	for k, v := range c.seen {
		out[k] = v
	}
	return out
}

// status returns a running Status of specType served by this child, and the
// loopback base URL probeRuntimeChild would build for it.
func (c *probeChild) status(t *testing.T, specType string, pid int) (runtimectl.Status, string) {
	t.Helper()
	st := runtimectl.Status{
		SpecID: "rspec_" + specType,
		Model:  "some-model",
		State:  runtimectl.StateRunning,
		PID:    pid,
		Port:   portFromURL(t, c.srv.URL),
		Type:   specType,
	}
	return st, "http://127.0.0.1:" + strconv.Itoa(st.Port)
}

const (
	sdcppCapabilitiesRoute = "GET /sdcpp/v1/capabilities"
	sdcppImageYesDoc       = `{"supported_modes":["img_gen"],"model":{"stem":"flux1-dev"}}`
	sdcppImageNoDoc        = `{"supported_modes":["vid_gen"],"model":{"stem":"wan2.1"}}`
)

// sdcppImage is the capability set a stable_diffusion_cpp child reports:
// the image verdict alone, under the literal source the gateway matches.
func sdcppImage(verdict string) *sample.Capabilities {
	return &sample.Capabilities{
		Verdicts: []sample.CapabilityVerdict{{Name: "image", Verdict: verdict}},
		Source:   "sdcpp_capabilities",
	}
}

// TestProbeRuntimeChildPropsSdcppReadsItsCapabilityDocument is the branch's
// wiring proof: when the gateway declares capability_source_sdcpp, a
// stable_diffusion_cpp child is asked exactly one thing, GET
// /sdcpp/v1/capabilities, and never /props (sd-server serves none). The
// verdict is cached per (pid, type, model) like every other probe's, and a
// cached child costs no child request, and no gateway round trip inside the
// gate's memo interval. A new pid asks again, and the new answer replaces
// the old one.
func TestProbeRuntimeChildPropsSdcppReadsItsCapabilityDocument(t *testing.T) {
	child := newProbeChild(t, map[string]string{sdcppCapabilitiesRoute: sdcppImageYesDoc})
	features := &fakeGatewayFeatures{names: []string{"runtime_manager", "capability_source_sdcpp"}}
	a := NewFromDeps(config.Config{}, Deps{GatewayFeatures: features})
	st, base := child.status(t, "stable_diffusion_cpp", 7001)
	probe := func() sample.RuntimeSample {
		var rs sample.RuntimeSample
		a.probeRuntimeChildProps(context.Background(), child.srv.Client(), base, st, &rs)
		return rs
	}

	rs := probe()
	if got, want := child.requests(), map[string]int{sdcppCapabilitiesRoute: 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("child requests = %v, want exactly %v (the capability document, and no /props)", got, want)
	}
	if want := sdcppImage("yes"); rs.Capabilities == nil || !reflect.DeepEqual(*rs.Capabilities, *want) {
		t.Fatalf("Capabilities = %+v, want %+v", rs.Capabilities, want)
	}
	if rs.LiveProgressSupport != "" {
		t.Errorf("LiveProgressSupport = %q, want \"\" (sd-server has no timings surface)", rs.LiveProgressSupport)
	}

	rs = probe()
	if got, want := child.requests(), map[string]int{sdcppCapabilitiesRoute: 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("child requests after a same-pid call = %v, want still %v (a conclusive answer is cached)", got, want)
	}
	if want := sdcppImage("yes"); rs.Capabilities == nil || !reflect.DeepEqual(*rs.Capabilities, *want) {
		t.Errorf("Capabilities (cache hit) = %+v, want %+v", rs.Capabilities, want)
	}
	if n := features.fetches(); n != 1 {
		t.Errorf("gateway feature fetches = %d, want 1 (a cached child re-checks the gate from the memo, not the gateway)", n)
	}

	child.serve(sdcppCapabilitiesRoute, sdcppImageNoDoc)
	st.PID = 7002
	rs = probe()
	if got, want := child.requests(), map[string]int{sdcppCapabilitiesRoute: 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("child requests after a restart = %v, want %v (a new pid asks again)", got, want)
	}
	if want := sdcppImage("no"); rs.Capabilities == nil || !reflect.DeepEqual(*rs.Capabilities, *want) {
		t.Errorf("Capabilities (new pid) = %+v, want %+v (the new process's answer, not the cached one)", rs.Capabilities, want)
	}
}

// TestProbeRuntimeChildPropsSdcppWaitsForTheGatewayFeature pins the gate:
// while the gateway does not declare capability_source_sdcpp, a
// stable_diffusion_cpp child is asked NOTHING, the sample carries no
// capability set and no live-progress verdict, and nothing is cached. The
// child serves a conclusive document throughout, so a probe that ran would
// both be seen and be cached. Once the gateway starts declaring the feature,
// the first call after the agent next asks the gateway probes, with the same
// pid: a gateway upgrade takes effect without restarting the child. When the
// agent asks again is gatewayFeatureRecheckInterval's business, pinned by
// TestGatewayDeclaresAsksOncePerIntervalForEverySdcppChild.
func TestProbeRuntimeChildPropsSdcppWaitsForTheGatewayFeature(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		err   error
	}{
		{"empty set", []string{}, nil},
		{"other features only", []string{"runtime_manager", "runtime_upstream_props"}, nil},
		{"fetch error", nil, errors.New("simulated features fetch failure")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			child := newProbeChild(t, map[string]string{sdcppCapabilitiesRoute: sdcppImageYesDoc})
			features := &fakeGatewayFeatures{names: tc.names, err: tc.err}
			a := NewFromDeps(config.Config{}, Deps{GatewayFeatures: features})
			clk := newFakeClock()
			a.now = clk.now
			st, base := child.status(t, "stable_diffusion_cpp", 7010)

			for cycle := 1; cycle <= 3; cycle++ {
				var rs sample.RuntimeSample
				a.probeRuntimeChildProps(context.Background(), child.srv.Client(), base, st, &rs)
				if got := child.requests(); len(got) != 0 {
					t.Fatalf("cycle %d: child requests = %v, want none (the gateway cannot accept this child's document yet)", cycle, got)
				}
				if rs.Capabilities != nil {
					t.Errorf("cycle %d: Capabilities = %+v, want nil", cycle, rs.Capabilities)
				}
				if rs.LiveProgressSupport != "" {
					t.Errorf("cycle %d: LiveProgressSupport = %q, want \"\"", cycle, rs.LiveProgressSupport)
				}
				if _, ok := a.runtimeCapabilityCache[st.SpecID]; ok {
					t.Fatalf("cycle %d: runtimeCapabilityCache has an entry for %q, want none (a skipped probe must not be cached)", cycle, st.SpecID)
				}
			}

			features.set([]string{"capability_source_sdcpp"}, nil)
			clk.advance(gatewayFeatureRecheckInterval)
			var rs sample.RuntimeSample
			a.probeRuntimeChildProps(context.Background(), child.srv.Client(), base, st, &rs)
			if got, want := child.requests(), map[string]int{sdcppCapabilitiesRoute: 1}; !reflect.DeepEqual(got, want) {
				t.Fatalf("child requests after the gateway declares the feature = %v, want %v (an upgrade is picked up without a child restart)", got, want)
			}
			if want := sdcppImage("yes"); rs.Capabilities == nil || !reflect.DeepEqual(*rs.Capabilities, *want) {
				t.Errorf("Capabilities after the upgrade = %+v, want %+v", rs.Capabilities, want)
			}
		})
	}
}

// TestProbeRuntimeChildPropsSdcppWithoutAFeatureSourceProbesNothing pins that
// an agent built without Deps.GatewayFeatures reads it as "not declared":
// such an agent cannot know that the gateway accepts the sdcpp source, so it
// sends what an older gateway expects, which is nothing for this child.
func TestProbeRuntimeChildPropsSdcppWithoutAFeatureSourceProbesNothing(t *testing.T) {
	child := newProbeChild(t, map[string]string{sdcppCapabilitiesRoute: sdcppImageYesDoc})
	a := NewFromDeps(config.Config{}, Deps{})
	st, base := child.status(t, "stable_diffusion_cpp", 7020)

	for cycle := 1; cycle <= 2; cycle++ {
		var rs sample.RuntimeSample
		a.probeRuntimeChildProps(context.Background(), child.srv.Client(), base, st, &rs)
		if got := child.requests(); len(got) != 0 {
			t.Fatalf("cycle %d: child requests = %v, want none", cycle, got)
		}
		if rs.Capabilities != nil || rs.LiveProgressSupport != "" {
			t.Errorf("cycle %d: Capabilities = %+v, LiveProgressSupport = %q, want nil and \"\"", cycle, rs.Capabilities, rs.LiveProgressSupport)
		}
		if _, ok := a.runtimeCapabilityCache[st.SpecID]; ok {
			t.Fatalf("cycle %d: runtimeCapabilityCache has an entry for %q, want none", cycle, st.SpecID)
		}
	}
}

// blockingGatewayFeatures is a gatewayFeatureSource that hangs the way a
// black-holed gateway does: Fetch returns only once its ctx is done, and then
// the way FeaturesClient.Fetch answers a transport error, with its last
// known-good set (none here) and a nil error. Closing release also unblocks
// it, so a failing test does not leave the goroutine behind.
type blockingGatewayFeatures struct {
	release chan struct{}
}

func (f *blockingGatewayFeatures) Fetch(ctx context.Context) ([]string, error) {
	select {
	case <-ctx.Done():
	case <-f.release:
	}
	return nil, nil
}

// TestProbeRuntimeChildPropsSdcppBoundsTheGatewayQuestion pins that the
// gateway question is bounded like every other per-child call. It runs on
// the collect loop, so without its own bound a gateway that never answers
// would hold the whole loop (telemetry, wake handling, the cert and trust
// tickers) for as long as the features client's HTTP timeout allows, once
// per uncached stable_diffusion_cpp child per cycle. The fake returns only
// when its context ends, and the caller's context never does. The call must
// still come back within about collectTimeout, having probed and cached
// nothing.
func TestProbeRuntimeChildPropsSdcppBoundsTheGatewayQuestion(t *testing.T) {
	child := newProbeChild(t, map[string]string{sdcppCapabilitiesRoute: sdcppImageYesDoc})
	features := &blockingGatewayFeatures{release: make(chan struct{})}
	t.Cleanup(func() { close(features.release) })
	a := NewFromDeps(config.Config{}, Deps{GatewayFeatures: features})
	st, base := child.status(t, "stable_diffusion_cpp", 7040)

	var rs sample.RuntimeSample
	done := make(chan struct{})
	go func() {
		a.probeRuntimeChildProps(context.Background(), child.srv.Client(), base, st, &rs)
		close(done)
	}()
	const grace = time.Second
	select {
	case <-done:
	case <-time.After(collectTimeout + grace):
		t.Fatalf("probeRuntimeChildProps still blocked after %v on a gateway that never answers, want it back within about collectTimeout (%v)", collectTimeout+grace, collectTimeout)
	}
	if got := child.requests(); len(got) != 0 {
		t.Errorf("child requests = %v, want none (an unanswered gateway question declares nothing)", got)
	}
	if rs.Capabilities != nil || rs.LiveProgressSupport != "" {
		t.Errorf("Capabilities = %+v, LiveProgressSupport = %q, want nil and \"\"", rs.Capabilities, rs.LiveProgressSupport)
	}
	if _, ok := a.runtimeCapabilityCache[st.SpecID]; ok {
		t.Errorf("runtimeCapabilityCache has an entry for %q, want none", st.SpecID)
	}
}

// TestProbeRuntimeChildPropsOtherTypesIgnoreTheSdcppFeature pins that the
// feature changes nothing for any other type: with capability_source_sdcpp
// declared, a llama_cpp or custom child still GETs /props, an ollama child
// still POSTs /api/show, none of them is asked for the sd capability
// document, and none of them costs a gateway round trip, neither on the
// probe nor on the cache hit that follows it (the hit path re-checks the
// gate only for an entry whose source is sdcpp_capabilities).
func TestProbeRuntimeChildPropsOtherTypesIgnoreTheSdcppFeature(t *testing.T) {
	const (
		propsRoute = "GET /props"
		showRoute  = "POST /api/show"
	)
	routes := map[string]string{
		propsRoute:             `{"default_generation_settings":{"params":{"timings_per_token":true}},"modalities":{"vision":true}}`,
		showRoute:              `{"capabilities":["completion","vision"]}`,
		sdcppCapabilitiesRoute: sdcppImageYesDoc,
	}
	cases := []struct {
		specType   string
		wantRoute  string
		wantSource string
	}{
		{"llama_cpp", propsRoute, "llama_cpp_props"},
		{"custom", propsRoute, "llama_cpp_props"},
		{"ollama", showRoute, "ollama_api_show"},
	}
	for _, tc := range cases {
		t.Run(tc.specType, func(t *testing.T) {
			child := newProbeChild(t, routes)
			features := &fakeGatewayFeatures{names: []string{"capability_source_sdcpp"}}
			a := NewFromDeps(config.Config{}, Deps{GatewayFeatures: features})
			st, base := child.status(t, tc.specType, 7030)

			var rs sample.RuntimeSample
			a.probeRuntimeChildProps(context.Background(), child.srv.Client(), base, st, &rs)
			if got, want := child.requests(), map[string]int{tc.wantRoute: 1}; !reflect.DeepEqual(got, want) {
				t.Fatalf("child requests = %v, want exactly %v", got, want)
			}
			if rs.Capabilities == nil || rs.Capabilities.Source != tc.wantSource {
				t.Errorf("Capabilities = %+v, want source %q", rs.Capabilities, tc.wantSource)
			}

			var hit sample.RuntimeSample
			a.probeRuntimeChildProps(context.Background(), child.srv.Client(), base, st, &hit)
			if got, want := child.requests(), map[string]int{tc.wantRoute: 1}; !reflect.DeepEqual(got, want) {
				t.Fatalf("child requests after a same-pid call = %v, want still %v (a conclusive answer is cached)", got, want)
			}
			if hit.Capabilities == nil || hit.Capabilities.Source != tc.wantSource {
				t.Errorf("Capabilities (cache hit) = %+v, want source %q", hit.Capabilities, tc.wantSource)
			}
			if n := features.fetches(); n != 0 {
				t.Errorf("gateway feature fetches = %d, want 0 (only a stable_diffusion_cpp child consults the gateway's feature set)", n)
			}
		})
	}
}

// fakeClock is the injectable clock the gateway-feature memo reads: a test
// advances it instead of waiting out gatewayFeatureRecheckInterval, the same
// seam as the client package's fakeClock for the WebSocket backoff.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_000_000, 0)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// sdcppFleet returns a runtime driver reporting n running
// stable_diffusion_cpp children, each its own spec and pid, all served by
// child, and the agent collecting from it with the gateway features source
// and clock given.
func sdcppFleet(t *testing.T, child *probeChild, n int, features gatewayFeatureSource, clk *fakeClock) (*Agent, *capturePoster) {
	t.Helper()
	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	statuses := make([]runtimectl.Status, n)
	for i := range statuses {
		st, _ := child.status(t, "stable_diffusion_cpp", 7100+i)
		st.SpecID = fmt.Sprintf("rspec_sd_%d", i)
		statuses[i] = st
	}
	drv.setStatuses(statuses)
	poster := &capturePoster{}
	a := NewFromDeps(config.Config{Interval: time.Hour}, Deps{Poster: poster, RuntimeDriver: drv, GatewayFeatures: features})
	a.now = clk.now
	return a, poster
}

// TestGatewayDeclaresAsksOncePerIntervalForEverySdcppChild pins the memo on
// the gateway feature question. The gateway here knows the sd spec type but
// not the source (the #144 build), so no child is ever cached and every one
// of them reaches the gate on every collect cycle. Before the memo that was
// one authenticated GET per child per cycle; now every child in every cycle
// shares one answer until gatewayFeatureRecheckInterval has passed, and the
// next cycle after that asks exactly once more.
func TestGatewayDeclaresAsksOncePerIntervalForEverySdcppChild(t *testing.T) {
	const children, cycles = 3, 5
	child := newProbeChild(t, map[string]string{sdcppCapabilitiesRoute: sdcppImageYesDoc})
	features := &fakeGatewayFeatures{names: []string{"runtime_manager", "runtime_logs", "runtime_config_ack"}}
	clk := newFakeClock()
	a, _ := sdcppFleet(t, child, children, features, clk)

	step := (gatewayFeatureRecheckInterval - time.Second) / cycles
	for cycle := 1; cycle <= cycles; cycle++ {
		a.collectOnce(context.Background())
		if n := features.fetches(); n != 1 {
			t.Fatalf("cycle %d: gateway feature fetches = %d, want 1 (%d children, in every cycle so far inside the interval, share one answer)", cycle, n, children)
		}
		clk.advance(step)
	}
	if got := child.requests(); len(got) != 0 {
		t.Fatalf("child requests = %v, want none (the gateway does not declare the source)", got)
	}

	// Just short of the interval: still the memo.
	a.collectOnce(context.Background())
	if n := features.fetches(); n != 1 {
		t.Fatalf("gateway feature fetches just short of the interval = %d, want still 1", n)
	}

	clk.advance(gatewayFeatureRecheckInterval)
	a.collectOnce(context.Background())
	if n := features.fetches(); n != 2 {
		t.Fatalf("gateway feature fetches after the interval = %d, want 2 (one more for every child in the cycle)", n)
	}
	a.collectOnce(context.Background())
	if n := features.fetches(); n != 2 {
		t.Fatalf("gateway feature fetches on the cycle after that = %d, want still 2", n)
	}
}

// TestGatewayDeclaresPicksUpAChangedAnswerAfterTheInterval pins the latency
// the memo buys: a gateway that starts declaring the source (an upgrade to
// #154) is not seen before gatewayFeatureRecheckInterval has passed since
// the last question, and is seen on the first cycle after it, for every
// child at once and without restarting any of them.
func TestGatewayDeclaresPicksUpAChangedAnswerAfterTheInterval(t *testing.T) {
	const children = 2
	child := newProbeChild(t, map[string]string{sdcppCapabilitiesRoute: sdcppImageYesDoc})
	features := &fakeGatewayFeatures{names: []string{"runtime_manager"}}
	clk := newFakeClock()
	a, poster := sdcppFleet(t, child, children, features, clk)

	a.collectOnce(context.Background())
	features.set([]string{"runtime_manager", "capability_source_sdcpp"}, nil)
	clk.advance(gatewayFeatureRecheckInterval - time.Second)
	a.collectOnce(context.Background())
	if got := child.requests(); len(got) != 0 {
		t.Fatalf("child requests inside the interval = %v, want none (the memo still says the source is not declared)", got)
	}
	for _, rs := range poster.last().Runtimes {
		if rs.Capabilities != nil {
			t.Fatalf("%s: Capabilities inside the interval = %+v, want nil", rs.SpecID, rs.Capabilities)
		}
	}

	clk.advance(time.Second)
	a.collectOnce(context.Background())
	if got, want := child.requests(), map[string]int{sdcppCapabilitiesRoute: children}; !reflect.DeepEqual(got, want) {
		t.Fatalf("child requests after the interval = %v, want %v (every child probes once the new answer is in)", got, want)
	}
	for _, rs := range poster.last().Runtimes {
		if want := sdcppImage("yes"); rs.Capabilities == nil || !reflect.DeepEqual(*rs.Capabilities, *want) {
			t.Errorf("%s: Capabilities after the interval = %+v, want %+v", rs.SpecID, rs.Capabilities, want)
		}
	}
	if n := features.fetches(); n != 2 {
		t.Errorf("gateway feature fetches = %d, want 2 (one per interval, not one per child)", n)
	}
}

// TestProbeRuntimeChildPropsSdcppRechecksTheGateOnACacheHit pins the
// downgrade half. A child whose verdict was cached while the gateway
// declared the source must stop sending it once the gateway no longer does
// (a rollback to a build that would drop the sample's capability rows with a
// warning every cycle), and must resume once it is declared again. The
// cached entry is kept throughout, so resuming costs the child nothing: it
// is asked exactly once, before the downgrade.
func TestProbeRuntimeChildPropsSdcppRechecksTheGateOnACacheHit(t *testing.T) {
	child := newProbeChild(t, map[string]string{sdcppCapabilitiesRoute: sdcppImageYesDoc})
	features := &fakeGatewayFeatures{names: []string{"capability_source_sdcpp"}}
	a := NewFromDeps(config.Config{}, Deps{GatewayFeatures: features})
	clk := newFakeClock()
	a.now = clk.now
	st, base := child.status(t, "stable_diffusion_cpp", 7050)
	probe := func() sample.RuntimeSample {
		var rs sample.RuntimeSample
		a.probeRuntimeChildProps(context.Background(), child.srv.Client(), base, st, &rs)
		return rs
	}

	if rs := probe(); rs.Capabilities == nil || !reflect.DeepEqual(*rs.Capabilities, *sdcppImage("yes")) {
		t.Fatalf("Capabilities = %+v, want %+v", rs.Capabilities, sdcppImage("yes"))
	}

	features.set([]string{"runtime_manager"}, nil)
	clk.advance(gatewayFeatureRecheckInterval)
	rs := probe()
	if rs.Capabilities != nil || rs.LiveProgressSupport != "" {
		t.Fatalf("after the downgrade: Capabilities = %+v, LiveProgressSupport = %q, want nil and \"\" (the gateway no longer accepts the source)", rs.Capabilities, rs.LiveProgressSupport)
	}
	entry, ok := a.runtimeCapabilityCache[st.SpecID]
	if !ok || entry.source != "sdcpp_capabilities" || entry.verdicts.Caps.Image != "yes" {
		t.Fatalf("cache entry after the downgrade = %+v (present %v), want the image yes entry kept", entry, ok)
	}

	features.set([]string{"capability_source_sdcpp"}, nil)
	clk.advance(gatewayFeatureRecheckInterval)
	if rs := probe(); rs.Capabilities == nil || !reflect.DeepEqual(*rs.Capabilities, *sdcppImage("yes")) {
		t.Fatalf("after the gateway declares the source again: Capabilities = %+v, want %+v", rs.Capabilities, sdcppImage("yes"))
	}
	if got, want := child.requests(), map[string]int{sdcppCapabilitiesRoute: 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("child requests = %v, want %v (the kept entry answers; the child is not asked again)", got, want)
	}
	if n := features.fetches(); n != 3 {
		t.Errorf("gateway feature fetches = %d, want 3 (one per interval)", n)
	}
}

// TestProbeRuntimeChildPropsRetypedChildProbesItsNewDocument pins the spec
// type in the capability cache key. A spec with no explicit type and an
// sd-server binary is "custom" under a gateway that predates the
// stable_diffusion_cpp type and "stable_diffusion_cpp" under one that knows
// it. The type is launch metadata, so a gateway upgrade retypes the spec
// with the same pid and model. Without the type in the key, the /props 404
// cached while the child was "custom" would keep answering, and the sd
// document would not be read until the child restarted.
func TestProbeRuntimeChildPropsRetypedChildProbesItsNewDocument(t *testing.T) {
	child := newProbeChild(t, map[string]string{sdcppCapabilitiesRoute: sdcppImageYesDoc})
	features := &fakeGatewayFeatures{names: []string{"capability_source_sdcpp"}}
	a := NewFromDeps(config.Config{}, Deps{GatewayFeatures: features})
	st, base := child.status(t, "custom", 7060)

	var rs sample.RuntimeSample
	a.probeRuntimeChildProps(context.Background(), child.srv.Client(), base, st, &rs)
	if got, want := child.requests(), map[string]int{"GET /props": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("child requests as custom = %v, want exactly %v", got, want)
	}
	if rs.Capabilities == nil || rs.Capabilities.Source != "llama_cpp_props" || len(rs.Capabilities.Verdicts) != 0 {
		t.Fatalf("Capabilities as custom = %+v, want an empty llama_cpp_props set (a conclusive 404)", rs.Capabilities)
	}

	st.Type = "stable_diffusion_cpp"
	rs = sample.RuntimeSample{}
	a.probeRuntimeChildProps(context.Background(), child.srv.Client(), base, st, &rs)
	if got, want := child.requests(), map[string]int{"GET /props": 1, sdcppCapabilitiesRoute: 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("child requests after the retype = %v, want %v (the new type names a different document)", got, want)
	}
	if want := sdcppImage("yes"); rs.Capabilities == nil || !reflect.DeepEqual(*rs.Capabilities, *want) {
		t.Errorf("Capabilities after the retype = %+v, want %+v", rs.Capabilities, want)
	}
}

// TestCollectOnceRuntimeLiveProgressUnsupported covers a real, determined
// "unsupported" verdict: a llama.cpp /props document whose
// default_generation_settings.params object is present but lacks
// timings_per_token (an older build).
func TestCollectOnceRuntimeLiveProgressUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":8192,"params":{"n_predict":-1}}}`))
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID:           "rspec_unsupported",
			Model:            "old-llama",
			State:            runtimectl.StateRunning,
			PID:              6002,
			Port:             portFromURL(t, srv.URL),
			Type:             "llama_cpp",
			ContextProbePath: "/props",
		},
	})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	a.collectOnce(context.Background())
	got := poster.last()
	if got == nil || len(got.Runtimes) != 1 {
		t.Fatalf("Runtimes = %+v", got)
	}
	rs := got.Runtimes[0]
	if rs.ContextProbe != "ok" || rs.ContextSize != 8192 {
		t.Errorf("ContextProbe/ContextSize = %q/%d, want %q/8192", rs.ContextProbe, rs.ContextSize, "ok")
	}
	if rs.LiveProgressSupport != "unsupported" {
		t.Errorf("LiveProgressSupport = %q, want %q", rs.LiveProgressSupport, "unsupported")
	}
}

// TestCollectOnceRuntimeLiveProgressUnreachable proves a failed /props probe
// reports LiveProgressSupport as "" (unknown), never an error, and never
// anything that blocks the sample. The server answers every request with a
// non-2xx status but a WOULD-BE "supported" body, and the test counts
// /props hits: this pins that the probe actually ran and its status check
// (not merely a refused connection, which a zero-value field would satisfy
// even if the whole capability probe were deleted -- see
// TestProbeLiveProgressSupport_Unreachable's identical concern) is what
// produces "".
//
// It also pins the nil-vs-all-empty Capabilities invariant on its TRANSIENT
// side (final-review must-fix 3b): 503 is NOT in ProbePropsVerdicts'
// CONCLUSIVE set (only 404/401/403/405 are), so stable comes back false and
// probeRuntimeChildProps returns before ever reaching capabilitiesSample.
// rs.Capabilities must therefore be exactly nil here -- "no conclusive
// answer yet" -- never the non-nil all-empty struct a CONCLUSIVE refusal
// leaves behind (TestCollectOnceRuntimeLiveProgressNotFoundCachedAcrossCycles
// covers that side).
func TestCollectOnceRuntimeLiveProgressUnreachable(t *testing.T) {
	var propsHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&propsHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":8192,"params":{"timings_per_token":false}}}`))
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID: "rspec_capability_unreachable",
			Model:  "qwen-coder",
			State:  runtimectl.StateRunning,
			PID:    6003,
			Port:   portFromURL(t, srv.URL),
			Type:   "custom",
		},
	})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	a.collectOnce(context.Background())
	got := poster.last()
	if got == nil || len(got.Runtimes) != 1 {
		t.Fatalf("Runtimes = %+v", got)
	}
	if hits := atomic.LoadInt32(&propsHits); hits != 1 {
		t.Fatalf("/props hits = %d, want 1 (the probe must actually run for this test to cover its failure path)", hits)
	}
	rs := got.Runtimes[0]
	if rs.LiveProgressSupport != "" {
		t.Errorf("LiveProgressSupport = %q, want %q (unknown) for a non-2xx /props response, even one carrying a well-formed supported body", rs.LiveProgressSupport, "")
	}
	if rs.Capabilities != nil {
		t.Errorf("Capabilities = %+v, want nil -- a TRANSIENT probe failure (503, not in the CONCLUSIVE set) has no conclusive answer yet and must not be cached as either nil-forever or an all-empty verdict", rs.Capabilities)
	}
}

// TestProbeRuntimeChildLiveProgressIgnoresContextCache is step 2's direct
// proof: a runtimeCtxCache hit (a prior context probe on this exact PID
// generation already succeeded) must NOT be read as evidence that the
// live-progress capability was ever determined. probeRuntimeChildProps
// keeps its own cache (runtimeCapabilityCache), so it must still issue the
// live /props GET here and return the verdict that fetch actually produces
// -- not skip the probe, and not fabricate a verdict from the unrelated
// context cache entry.
//
// If the two caches were ever merged (e.g. a context-cache hit short-
// circuiting the capability probe), this test would fail two ways at once:
// the fake server's hit count would stay 0 (the GET never happened), and
// rs.LiveProgressSupport would stay "" instead of the "supported" the live
// response actually carries.
func TestProbeRuntimeChildLiveProgressIgnoresContextCache(t *testing.T) {
	var propsHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&propsHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":4096,"params":{"timings_per_token":false}}}`))
	}))
	defer srv.Close()

	st := runtimectl.Status{
		SpecID:           "rspec_ctx_cache_only",
		State:            runtimectl.StateRunning,
		PID:              6004,
		Port:             portFromURL(t, srv.URL),
		Type:             "llama_cpp",
		ContextProbePath: "/props",
	}

	a := &Agent{
		// A context probe on this EXACT (pid, type, path) generation
		// already succeeded and is cached -- runtimeCapabilityCache is left
		// nil: the capability was never determined.
		runtimeCtxCache: map[string]runtimeCtxEntry{
			st.SpecID: {pid: st.PID, specType: st.Type, contextProbePath: st.ContextProbePath, size: 4096},
		},
	}

	base := "http://127.0.0.1:" + strconv.Itoa(st.Port)
	var rs sample.RuntimeSample
	a.probeRuntimeChildProps(context.Background(), srv.Client(), base, st, &rs)

	if hits := atomic.LoadInt32(&propsHits); hits != 1 {
		t.Fatalf("/props hits = %d, want 1 (a context-cache hit must not skip the capability probe)", hits)
	}
	if rs.LiveProgressSupport != "supported" {
		t.Fatalf("LiveProgressSupport = %q, want %q (the freshly-probed verdict, not a value implied by the context cache)", rs.LiveProgressSupport, "supported")
	}

	// A second call now hits the CAPABILITY cache this probe itself just
	// populated -- no second GET.
	var rs2 sample.RuntimeSample
	a.probeRuntimeChildProps(context.Background(), srv.Client(), base, st, &rs2)
	if hits := atomic.LoadInt32(&propsHits); hits != 1 {
		t.Errorf("/props hits after 2nd call = %d, want 1 (a determined verdict must be cached)", hits)
	}
	if rs2.LiveProgressSupport != "supported" {
		t.Errorf("LiveProgressSupport (cache hit) = %q, want %q", rs2.LiveProgressSupport, "supported")
	}
}

// The following tests cover the resource-usage fix (issue #51/#52
// follow-up): caching the STABLE half of an
// undetermined ("") verdict -- a 404, or a well-formed non-/props body --
// while still retrying the TRANSIENT half (a connection refused, a timeout,
// an unparseable body) every cycle. See probeRuntimeChildProps's
// "Caching policy" doc comment for the full distinction.

// TestCollectOnceRuntimeLiveProgressNotFoundCachedAcrossCycles is required
// test 1: a child whose /props answers 404 is probed once and never again,
// across several further collect cycles, for as long as its pid lives. The
// hit counter is asserted numerically after every cycle, not inferred from
// the verdict alone -- a coincidental pass (e.g. a fixture where "" already
// equals "") would not catch a reverted fix that re-asks every cycle.
//
// It also pins the nil-vs-all-empty Capabilities invariant on its CONCLUSIVE
// side (final-review must-fix 3a): a 404 sits in ProbePropsVerdicts'
// CONCLUSIVE set, so stable comes back true and probeRuntimeChildProps still
// reaches capabilitiesSample, which always returns a non-nil pointer even
// over a zero-value collector.Capabilities. rs.Capabilities must therefore be
// a non-nil, ALL-EMPTY struct here -- "detection ran, determined nothing" --
// never the nil reserved for a still-transient probe
// (TestCollectOnceRuntimeLiveProgressUnreachable covers that side).
func TestCollectOnceRuntimeLiveProgressNotFoundCachedAcrossCycles(t *testing.T) {
	var propsHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&propsHits, 1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID: "rspec_notfound_stable",
			Model:  "ollama-mystery",
			State:  runtimectl.StateRunning,
			PID:    6010,
			Port:   portFromURL(t, srv.URL),
			Type:   "custom",
		},
	})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	const cycles = 4
	for cycle := 1; cycle <= cycles; cycle++ {
		a.collectOnce(context.Background())
		got := poster.last()
		if got == nil || len(got.Runtimes) != 1 {
			t.Fatalf("cycle %d: Runtimes = %+v", cycle, got)
		}
		rs := got.Runtimes[0]
		if rs.LiveProgressSupport != "" {
			t.Errorf("cycle %d: LiveProgressSupport = %q, want %q (unknown -- a 404 is not a real verdict)", cycle, rs.LiveProgressSupport, "")
		}
		if rs.Capabilities == nil {
			t.Errorf("cycle %d: Capabilities = nil, want a non-nil all-empty struct -- a 404 is a CONCLUSIVE refusal (\"detection ran, determined nothing\"), distinct from a still-transient probe's nil", cycle)
		} else if len(rs.Capabilities.Verdicts) != 0 {
			t.Errorf("cycle %d: Capabilities.Verdicts = %+v, want empty", cycle, rs.Capabilities.Verdicts)
		}
		if hits := atomic.LoadInt32(&propsHits); hits != 1 {
			t.Fatalf("cycle %d: /props hits = %d, want 1 (a 404 is conclusive: cache it and never ask again for this pid)", cycle, hits)
		}
	}
}

// TestCollectOnceRuntimeLiveProgressApiKeyRefusalCachedAcrossCycles is the
// final-review F1 case, and the one that is NOT hypothetical: a managed child
// launched as `llama-server --api-key ${API_TOKEN}` (a first-class supported
// spec shape) answers /props with 401, because llama.cpp marks only /health
// and /v1/health as public. The agent's probe cannot authenticate --
// runtime.Status carries no token (issue #58) -- so that refusal is as
// conclusive as a 404 and must be cached: before this fix a 401 was
// classified transient, so the agent re-GETs /props on EVERY collect cycle
// (1 s default, 250 ms floor) for the child's entire lifetime, plus one
// slog.Debug line per attempt.
//
// Asserted with a numeric hit counter after every cycle, exactly like its
// 404 sibling above: the verdict is "" both before and after the fix, so a
// verdict-only assertion would pass with the fix reverted.
func TestCollectOnceRuntimeLiveProgressApiKeyRefusalCachedAcrossCycles(t *testing.T) {
	var propsHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&propsHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":401,"message":"Invalid API Key","type":"authentication_error"}}`))
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID: "rspec_apikey_stable",
			Model:  "llama-behind-a-key",
			State:  runtimectl.StateRunning,
			PID:    6013,
			Port:   portFromURL(t, srv.URL),
			Type:   "custom",
		},
	})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	const cycles = 4
	for cycle := 1; cycle <= cycles; cycle++ {
		a.collectOnce(context.Background())
		got := poster.last()
		if got == nil || len(got.Runtimes) != 1 {
			t.Fatalf("cycle %d: Runtimes = %+v", cycle, got)
		}
		if rs := got.Runtimes[0]; rs.LiveProgressSupport != "" {
			t.Errorf("cycle %d: LiveProgressSupport = %q, want %q (unknown -- a 401 is not a verdict about the build's request schema)", cycle, rs.LiveProgressSupport, "")
		}
		if hits := atomic.LoadInt32(&propsHits); hits != 1 {
			t.Fatalf("cycle %d: /props hits = %d, want 1 (a 401 is conclusive: cache it and never ask again for this pid)", cycle, hits)
		}
	}
}

// TestCollectOnceRuntimeLiveProgressOtherShapeCachedAcrossCycles is required
// test 2: a child whose /props answers with a well-formed body that simply
// isn't a llama.cpp /props document (a vLLM-shaped body here) is also probed
// once and never again -- the same stable-cache treatment as a 404, proven
// with a numeric hit counter across several further collect cycles.
func TestCollectOnceRuntimeLiveProgressOtherShapeCachedAcrossCycles(t *testing.T) {
	var propsHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&propsHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1","max_model_len":4096}]}`))
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setStatuses([]runtimectl.Status{
		{
			SpecID: "rspec_othershape_stable",
			Model:  "vllm-mystery",
			State:  runtimectl.StateRunning,
			PID:    6011,
			Port:   portFromURL(t, srv.URL),
			Type:   "custom",
		},
	})

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	const cycles = 4
	for cycle := 1; cycle <= cycles; cycle++ {
		a.collectOnce(context.Background())
		got := poster.last()
		if got == nil || len(got.Runtimes) != 1 {
			t.Fatalf("cycle %d: Runtimes = %+v", cycle, got)
		}
		if rs := got.Runtimes[0]; rs.LiveProgressSupport != "" {
			t.Errorf("cycle %d: LiveProgressSupport = %q, want %q (unknown -- a vLLM body is not a llama.cpp /props document)", cycle, rs.LiveProgressSupport, "")
		}
		if hits := atomic.LoadInt32(&propsHits); hits != 1 {
			t.Fatalf("cycle %d: /props hits = %d, want 1 (a well-formed non-/props body is conclusive: cache it and never ask again for this pid)", cycle, hits)
		}
	}
}

// erroringRoundTripper is a fake http.RoundTripper that fails every request
// the way a refused connection would (no HTTP response is ever produced),
// counting how many times it was asked. It exists to give the
// "connection refused" case below a numeric hit counter: a real refused
// connection never runs a server-side handler, so counting attempts is only
// possible at the client's own transport.
type erroringRoundTripper struct {
	calls int32
}

func (rt *erroringRoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	atomic.AddInt32(&rt.calls, 1)
	return nil, errors.New("simulated connection refused")
}

// TestProbeRuntimeChildLiveProgressConnectionRefusedRetries is required test
// 3: a child whose /props connection is refused is retried on every cycle
// (the TRANSIENT case), so the hit count grows -- it must never settle into
// the "asked once" pattern the two stable tests above pin. Also confirms no
// runtimeCapabilityCache entry is ever created for this pid, which is the
// production mechanism that would otherwise stop the retries.
func TestProbeRuntimeChildLiveProgressConnectionRefusedRetries(t *testing.T) {
	rt := &erroringRoundTripper{}
	client := &http.Client{Transport: rt}

	st := runtimectl.Status{
		SpecID: "rspec_conn_refused",
		State:  runtimectl.StateRunning,
		PID:    6012,
		Port:   1,
		Type:   "custom",
	}
	base := "http://127.0.0.1:" + strconv.Itoa(st.Port)

	a := &Agent{}
	const cycles = 3
	for cycle := 1; cycle <= cycles; cycle++ {
		var rs sample.RuntimeSample
		a.probeRuntimeChildProps(context.Background(), client, base, st, &rs)
		if rs.LiveProgressSupport != "" {
			t.Errorf("cycle %d: LiveProgressSupport = %q, want %q (unknown)", cycle, rs.LiveProgressSupport, "")
		}
		if calls := atomic.LoadInt32(&rt.calls); calls != int32(cycle) {
			t.Fatalf("cycle %d: RoundTrip calls = %d, want %d (a connection-refused probe must be retried every cycle, never cached)", cycle, calls, cycle)
		}
	}
	if _, ok := a.runtimeCapabilityCache[st.SpecID]; ok {
		t.Errorf("runtimeCapabilityCache has an entry for %q, want none (a transient failure must never be cached)", st.SpecID)
	}
}

// TestCollectOnceRuntimeLiveProgressPidChangeRearmsStableUnknown is required
// test 4, specifically for the new stable-"" cache slot this fix adds: a
// child that is cached as a conclusive "" (a 404) must be re-asked after it
// restarts under a new pid, exactly like a cached "supported"/"unsupported"
// verdict already was before this fix.
func TestCollectOnceRuntimeLiveProgressPidChangeRearmsStableUnknown(t *testing.T) {
	var propsHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&propsHits, 1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	pid := 6013
	setStatuses := func() {
		drv.setStatuses([]runtimectl.Status{
			{
				SpecID: "rspec_pid_rearm",
				State:  runtimectl.StateRunning,
				PID:    pid,
				Port:   portFromURL(t, srv.URL),
				Type:   "custom",
			},
		})
	}
	setStatuses()

	poster := &capturePoster{}
	cfg := config.Config{Interval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: poster, RuntimeDriver: drv})

	a.collectOnce(context.Background())
	a.collectOnce(context.Background())
	if hits := atomic.LoadInt32(&propsHits); hits != 1 {
		t.Fatalf("/props hits before restart = %d, want 1 (cached across cycles for the same pid)", hits)
	}

	// The child restarts: same SpecID, a new pid.
	pid = 6014
	setStatuses()
	a.collectOnce(context.Background())
	if hits := atomic.LoadInt32(&propsHits); hits != 2 {
		t.Errorf("/props hits after restart = %d, want 2 (a changed pid must re-arm the question, even for a cached stable \"\")", hits)
	}
	got := poster.last()
	if got == nil || len(got.Runtimes) != 1 {
		t.Fatalf("Runtimes = %+v", got)
	}
	if rs := got.Runtimes[0]; rs.LiveProgressSupport != "" {
		t.Errorf("LiveProgressSupport after restart = %q, want %q", rs.LiveProgressSupport, "")
	}
}
