// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"context"
	"errors"
	"fmt"
	"op-ai-server-agent/internal/certinstall"
	"op-ai-server-agent/internal/collector"
	"op-ai-server-agent/internal/config"
	"op-ai-server-agent/internal/proxy"
	"op-ai-server-agent/internal/sample"
	"os"
	"runtime"
	"strings"
	"sync"
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
