// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// records the last pushed payload, can optionally block a Sync call until
// the test releases it (proving single-flight coalescing, mirroring
// fakeCertSyncer exactly), and returns a configurable Status slice. It also
// satisfies runtimeTransitionsWaker via its own trans channel field, which
// NewFromDeps discovers via a type assertion exactly like Deps.Poster's
// certWaker/trustWaker.
type fakeRuntimeDriver struct {
	mu          sync.Mutex
	calls       int
	lastPushed  json.RawMessage
	statuses    []runtimectl.Status
	block       bool
	release     chan struct{}
	trans       chan struct{}
	active      atomic.Bool // fix round 1, I3: defaults to false, matching the real Driver's honest "not yet negotiated" zero value
	resendCalls int
	appliedETag string
}

func newFakeRuntimeDriver() *fakeRuntimeDriver {
	return &fakeRuntimeDriver{release: make(chan struct{})}
}

func (f *fakeRuntimeDriver) Sync(_ context.Context, pushed json.RawMessage) {
	f.mu.Lock()
	f.calls++
	f.lastPushed = pushed
	block := f.block
	f.mu.Unlock()
	if block {
		<-f.release
	}
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

func (f *fakeRuntimeDriver) setBlocking(b bool) {
	f.mu.Lock()
	f.block = b
	f.mu.Unlock()
}

func (f *fakeRuntimeDriver) unblock() { close(f.release) }

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
// whole feature depends on (task-18-brief.md): an Agent built with a nil
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
// that gates the override on "a.runtimeDriver != nil" alone (see
// task-18-report.md's I3 fix log for the pasted failure).
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

// TestTriggerRuntimeSyncCoalescesConcurrentSignals mirrors
// TestTriggerCertSyncCoalescesConcurrentSignals verbatim, for
// triggerRuntimeSync's own single-flight CompareAndSwap.
func TestTriggerRuntimeSyncCoalescesConcurrentSignals(t *testing.T) {
	drv := newFakeRuntimeDriver()
	drv.setBlocking(true)
	a := &Agent{cfg: config.Config{}, runtimeDriver: drv}

	a.triggerRuntimeSync(context.Background(), nil)
	// Deterministic, not a race: runtimeSyncing is set synchronously by
	// CompareAndSwap inside the first call, strictly before it spawns its
	// goroutine, and no goroutine scheduling occurs between these two
	// sequential calls on this single test goroutine.
	a.triggerRuntimeSync(context.Background(), nil)

	waitUntil(t, time.Second, func() bool { return drv.count() >= 1 })
	time.Sleep(30 * time.Millisecond)
	if got := drv.count(); got != 1 {
		t.Fatalf("Sync call count while blocked = %d, want exactly 1 (both signals must coalesce into one in-flight sync)", got)
	}

	drv.unblock()
	waitUntil(t, time.Second, func() bool { return !a.runtimeSyncing.Load() })

	drv.setBlocking(false)
	a.triggerRuntimeSync(context.Background(), nil)
	waitUntil(t, time.Second, func() bool { return drv.count() == 2 })
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
// live-progress capability determined, because probeRuntimeChildLiveProgress
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
	if rs := got.Runtimes[0]; rs.LiveProgressSupport != "" {
		t.Errorf("LiveProgressSupport = %q, want %q (unknown) for a non-2xx /props response, even one carrying a well-formed supported body", rs.LiveProgressSupport, "")
	}
}

// TestProbeRuntimeChildLiveProgressIgnoresContextCache is step 2's direct
// proof: a runtimeCtxCache hit (a prior context probe on this exact PID
// generation already succeeded) must NOT be read as evidence that the
// live-progress capability was ever determined. probeRuntimeChildLiveProgress
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
	a.probeRuntimeChildLiveProgress(context.Background(), srv.Client(), base, st, &rs)

	if hits := atomic.LoadInt32(&propsHits); hits != 1 {
		t.Fatalf("/props hits = %d, want 1 (a context-cache hit must not skip the capability probe)", hits)
	}
	if rs.LiveProgressSupport != "supported" {
		t.Fatalf("LiveProgressSupport = %q, want %q (the freshly-probed verdict, not a value implied by the context cache)", rs.LiveProgressSupport, "supported")
	}

	// A second call now hits the CAPABILITY cache this probe itself just
	// populated -- no second GET.
	var rs2 sample.RuntimeSample
	a.probeRuntimeChildLiveProgress(context.Background(), srv.Client(), base, st, &rs2)
	if hits := atomic.LoadInt32(&propsHits); hits != 1 {
		t.Errorf("/props hits after 2nd call = %d, want 1 (a determined verdict must be cached)", hits)
	}
	if rs2.LiveProgressSupport != "supported" {
		t.Errorf("LiveProgressSupport (cache hit) = %q, want %q", rs2.LiveProgressSupport, "supported")
	}
}

// The following tests cover the resource-usage fix (issue #51/#52 follow-up,
// task 4 report "Concerns" item 2): caching the STABLE half of an
// undetermined ("") verdict -- a 404, or a well-formed non-/props body --
// while still retrying the TRANSIENT half (a connection refused, a timeout,
// an unparseable body) every cycle. See probeRuntimeChildLiveProgress's
// "Caching policy" doc comment for the full distinction.

// TestCollectOnceRuntimeLiveProgressNotFoundCachedAcrossCycles is required
// test 1: a child whose /props answers 404 is probed once and never again,
// across several further collect cycles, for as long as its pid lives. The
// hit counter is asserted numerically after every cycle, not inferred from
// the verdict alone -- a coincidental pass (e.g. a fixture where "" already
// equals "") would not catch a reverted fix that re-asks every cycle.
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
		if rs := got.Runtimes[0]; rs.LiveProgressSupport != "" {
			t.Errorf("cycle %d: LiveProgressSupport = %q, want %q (unknown -- a 404 is not a real verdict)", cycle, rs.LiveProgressSupport, "")
		}
		if hits := atomic.LoadInt32(&propsHits); hits != 1 {
			t.Fatalf("cycle %d: /props hits = %d, want 1 (a 404 is conclusive: cache it and never ask again for this pid)", cycle, hits)
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
		a.probeRuntimeChildLiveProgress(context.Background(), client, base, st, &rs)
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
