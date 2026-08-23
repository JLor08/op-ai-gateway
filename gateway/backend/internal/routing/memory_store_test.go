// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/storeerr"
	"testing"
	"time"
)

func TestMemoryStoreSetServerHealth(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	if err := store.CreateAIServer(ctx, AIServer{ID: "host_1", Name: "Host 1", Provider: ProviderMock, Endpoint: "mock://host", Status: ServerStatusActive, HealthStatus: HealthUnknown, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer returned %v", err)
	}
	if err := store.SetServerHealth(ctx, "host_1", HealthDegraded); err != nil {
		t.Fatalf("SetServerHealth returned %v", err)
	}
	got, err := store.AIServerByID(ctx, "host_1")
	if err != nil {
		t.Fatalf("AIServerByID returned %v", err)
	}
	if got.HealthStatus != HealthDegraded {
		t.Fatalf("HealthStatus = %q, want %q", got.HealthStatus, HealthDegraded)
	}
	// Other fields are untouched.
	if got.Name != "Host 1" || got.Status != ServerStatusActive {
		t.Fatalf("SetServerHealth clobbered fields: %#v", got)
	}
	if err := store.SetServerHealth(ctx, "missing", HealthHealthy); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("SetServerHealth(unknown) = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreNetbirdNarrowWrites(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	if err := store.CreateAIServer(ctx, AIServer{
		ID: "srvnb", Name: "NB", Domain: "old.local", Provider: ProviderMock, Endpoint: "mock://nb",
		Status: ServerStatusActive, HealthStatus: HealthUnknown,
		NetbirdEnabled: true, NetbirdSetupKeyID: "sk-1", NetbirdGroupID: "grp-1",
		NetbirdPeerID: "peer-1", NetbirdConnected: true, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	// Round-trip: create carries all 5 fields (copyAIServer copies by value).
	got, _ := store.AIServerByID(ctx, "srvnb")
	if !got.NetbirdEnabled || got.NetbirdSetupKeyID != "sk-1" || got.NetbirdGroupID != "grp-1" || got.NetbirdPeerID != "peer-1" || !got.NetbirdConnected {
		t.Fatalf("after create: %#v", got)
	}

	// UpdateServerNetbirdKey touches only enabled + setup_key_id + group_id.
	if err := store.UpdateServerNetbirdKey(ctx, "srvnb", true, "sk-2", "grp-2"); err != nil {
		t.Fatalf("UpdateServerNetbirdKey: %v", err)
	}
	got, _ = store.AIServerByID(ctx, "srvnb")
	if !got.NetbirdEnabled || got.NetbirdSetupKeyID != "sk-2" || got.NetbirdGroupID != "grp-2" || got.Domain != "old.local" || got.NetbirdPeerID != "peer-1" || !got.NetbirdConnected {
		t.Fatalf("after key write: %#v", got)
	}

	// UpdateServerNetbirdState touches only domain + peer_id + connected.
	if err := store.UpdateServerNetbirdState(ctx, "srvnb", "peer.nb.cloud", "peer-2", false); err != nil {
		t.Fatalf("UpdateServerNetbirdState: %v", err)
	}
	got, _ = store.AIServerByID(ctx, "srvnb")
	if got.Domain != "peer.nb.cloud" || got.NetbirdPeerID != "peer-2" || got.NetbirdConnected || got.NetbirdSetupKeyID != "sk-2" || got.NetbirdGroupID != "grp-2" {
		t.Fatalf("after state write: %#v", got)
	}

	// UpdateServerNetbirdLink sets enabled + peer_id and RESETS connected (here the
	// prior state left connected=false; set it true first to prove the reset).
	if err := store.UpdateServerNetbirdState(ctx, "srvnb", "peer.nb.cloud", "peer-2", true); err != nil {
		t.Fatalf("seed connected: %v", err)
	}
	if err := store.UpdateServerNetbirdLink(ctx, "srvnb", true, "peer-3"); err != nil {
		t.Fatalf("UpdateServerNetbirdLink: %v", err)
	}
	got, _ = store.AIServerByID(ctx, "srvnb")
	if !got.NetbirdEnabled || got.NetbirdPeerID != "peer-3" || got.NetbirdConnected || got.Domain != "peer.nb.cloud" || got.NetbirdSetupKeyID != "sk-2" || got.NetbirdGroupID != "grp-2" {
		t.Fatalf("after link write: %#v", got)
	}
	// Disabling via the link editor flips the flag off + clears the peer id.
	if err := store.UpdateServerNetbirdLink(ctx, "srvnb", false, ""); err != nil {
		t.Fatalf("UpdateServerNetbirdLink(disable): %v", err)
	}
	got, _ = store.AIServerByID(ctx, "srvnb")
	if got.NetbirdEnabled || got.NetbirdPeerID != "" || got.NetbirdConnected {
		t.Fatalf("after link disable: %#v", got)
	}

	// UpdateServerNetbirdAllowPing sets only netbird_allow_ping (the value round-trips).
	if err := store.UpdateServerNetbirdAllowPing(ctx, "srvnb", true); err != nil {
		t.Fatalf("UpdateServerNetbirdAllowPing: %v", err)
	}
	got, _ = store.AIServerByID(ctx, "srvnb")
	if !got.NetbirdAllowPing || got.NetbirdSetupKeyID != "sk-2" || got.NetbirdGroupID != "grp-2" || got.Domain != "peer.nb.cloud" {
		t.Fatalf("after allow-ping write: %#v", got)
	}
	if err := store.UpdateServerNetbirdAllowPing(ctx, "srvnb", false); err != nil {
		t.Fatalf("UpdateServerNetbirdAllowPing(false): %v", err)
	}
	got, _ = store.AIServerByID(ctx, "srvnb")
	if got.NetbirdAllowPing {
		t.Fatalf("after allow-ping false: %#v", got)
	}

	if err := store.UpdateServerNetbirdKey(ctx, "missing", true, "x", "y"); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("UpdateServerNetbirdKey(unknown) = %v, want ErrNotFound", err)
	}
	if err := store.UpdateServerNetbirdState(ctx, "missing", "d", "p", true); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("UpdateServerNetbirdState(unknown) = %v, want ErrNotFound", err)
	}
	if err := store.UpdateServerNetbirdAllowPing(ctx, "missing", true); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("UpdateServerNetbirdAllowPing(unknown) = %v, want ErrNotFound", err)
	}
	if err := store.UpdateServerNetbirdLink(ctx, "missing", true, "p"); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("UpdateServerNetbirdLink(unknown) = %v, want ErrNotFound", err)
	}
}

// UpdateServerEnergyConfig sets only the four energy columns; a distinct sibling
// (agent_presence_timeout_seconds) must survive, and every argument round-trips.
func TestMemoryStoreEnergyConfig(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	if err := store.CreateAIServer(ctx, AIServer{
		ID: "srven", Name: "EN", Domain: "en.local", Provider: ProviderMock, Endpoint: "mock://en",
		Status: ServerStatusActive, HealthStatus: HealthUnknown,
		AgentPresenceTimeoutSeconds: 42,
		EstimatedWatts:              100, IdleWatts: 25, PricePerKwh: 0.3, Pue: 1.5, PriceUnit: "usd",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	got, _ := store.AIServerByID(ctx, "srven")
	if got.EstimatedWatts != 100 || got.IdleWatts != 25 || got.PricePerKwh != 0.3 || got.Pue != 1.5 || got.PriceUnit != "usd" {
		t.Fatalf("after create: %#v", got)
	}
	if err := store.UpdateServerEnergyConfig(ctx, "srven", 200, 60, 0.12, 1.25, "eur"); err != nil {
		t.Fatalf("UpdateServerEnergyConfig: %v", err)
	}
	got, _ = store.AIServerByID(ctx, "srven")
	if got.EstimatedWatts != 200 || got.IdleWatts != 60 || got.PricePerKwh != 0.12 || got.Pue != 1.25 || got.PriceUnit != "eur" || got.AgentPresenceTimeoutSeconds != 42 || got.Domain != "en.local" {
		t.Fatalf("after energy write (clobbered a sibling?): %#v", got)
	}
	if err := store.UpdateServerEnergyConfig(ctx, "missing", 1, 1, 1, 1, "eur"); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("UpdateServerEnergyConfig(unknown) = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreUpsertsTelemetryAndAffinity(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	if err := store.CreateAIServer(ctx, AIServer{ID: "host_1", Name: "Host 1", Provider: ProviderMock, Endpoint: "mock://host", Status: ServerStatusActive, HealthStatus: HealthUnknown, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer returned %v", err)
	}
	if err := store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "host_1", ReportedAt: now, ActiveRequests: 2, QueueDepth: 1, LatencyMS: 120, ErrorRate: 0.01, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry returned %v", err)
	}
	telemetry, ok, err := store.TelemetryByServer(ctx, "host_1")
	if err != nil {
		t.Fatalf("TelemetryByServer returned %v", err)
	}
	if !ok || telemetry.ActiveRequests != 2 || telemetry.QueueDepth != 1 {
		t.Fatalf("telemetry = %#v ok=%v", telemetry, ok)
	}

	affinity := RouteAffinity{ID: "aff_1", APITokenID: "tok_dev", UserID: "usr_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, SessionID: "", ApplicationID: "app_1", ServerID: "host_1", ExpiresAt: now.Add(30 * time.Minute), LastUsedAt: now, CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertAffinity(ctx, affinity); err != nil {
		t.Fatalf("UpsertAffinity returned %v", err)
	}
	got, ok, err := store.Affinity(ctx, AffinityKey{APITokenID: "tok_dev", Model: "qwen-coder", APIFlavor: APIFlavorOpenAI, SessionID: ""})
	if err != nil {
		t.Fatalf("Affinity returned %v", err)
	}
	if !ok || got.ApplicationID != "app_1" || got.ServerID != "host_1" {
		t.Fatalf("affinity = %#v ok=%v", got, ok)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMemoryStoreTelemetrySamples(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	m := NewMemoryStore()
	must(t, m.CreateAIServer(ctx, AIServer{ID: "host_1", Name: "Host 1", Provider: ProviderMock, Endpoint: "mock://host", Status: ServerStatusActive, HealthStatus: HealthUnknown, CreatedAt: base, UpdatedAt: base}))

	for i := 0; i < 6; i++ {
		sample := TelemetrySample{
			ServerID:   "host_1",
			ReportedAt: base.Add(time.Duration(i) * time.Minute),
			CPUUtilPct: float64(10 + i),
			GPUs:       []GPUSample{{Index: 0, Name: "RTX 4090", UtilPct: 88, TempC: 71}},
			Net:        []NetSample{{Name: "eth0", RxBytes: 1000, TxBytes: 2000}},
		}
		must(t, m.InsertTelemetrySample(ctx, sample))
	}

	// Full window: all 6, ascending reported_at, nested GPU/Net round-trip on [0].
	got, err := m.TelemetrySamples(ctx, "host_1", base, base.Add(10*time.Minute), 100)
	must(t, err)
	if len(got) != 6 {
		t.Fatalf("expected 6 samples, got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].ReportedAt.Before(got[i-1].ReportedAt) {
			t.Fatalf("samples not ascending at %d: %v before %v", i, got[i].ReportedAt, got[i-1].ReportedAt)
		}
	}
	if len(got[0].GPUs) != 1 || got[0].GPUs[0].Name != "RTX 4090" || got[0].GPUs[0].TempC != 71 {
		t.Fatalf("gpu round-trip mismatch: %+v", got[0].GPUs)
	}
	if len(got[0].Net) != 1 || got[0].Net[0].RxBytes != 1000 {
		t.Fatalf("net round-trip mismatch: %+v", got[0].Net)
	}

	// Window filter: [base+2m, base+4m] inclusive → 3 samples.
	win, err := m.TelemetrySamples(ctx, "host_1", base.Add(2*time.Minute), base.Add(4*time.Minute), 100)
	must(t, err)
	if len(win) != 3 {
		t.Fatalf("expected 3 samples in [+2m,+4m], got %d", len(win))
	}

	// Decimation: cap to 3, still ascending, first == oldest, last == newest.
	dec, err := m.TelemetrySamples(ctx, "host_1", base, base.Add(10*time.Minute), 3)
	must(t, err)
	// Exactly 3 evenly-spaced points (indices {0,2,5} = {base, base+2m, base+5m}),
	// not a loose 1..3 bound — mirrors the store conformance assertion so both
	// decimators keep the same interior-sampling contract under regression.
	if len(dec) != 3 {
		t.Fatalf("expected exactly 3 decimated samples, got %d", len(dec))
	}
	for i := 1; i < len(dec); i++ {
		if !dec[i].ReportedAt.After(dec[i-1].ReportedAt) {
			t.Fatalf("decimated not strictly ascending at %d: %v not after %v", i, dec[i].ReportedAt, dec[i-1].ReportedAt)
		}
	}
	if !dec[1].ReportedAt.Equal(base.Add(2 * time.Minute)) {
		t.Fatalf("decimated interior mis-spaced: got %v want %v", dec[1].ReportedAt, base.Add(2*time.Minute))
	}
	if !dec[0].ReportedAt.Equal(base) {
		t.Fatalf("decimated first is not oldest: got %v want %v", dec[0].ReportedAt, base)
	}
	if !dec[len(dec)-1].ReportedAt.Equal(base.Add(5 * time.Minute)) {
		t.Fatalf("decimated last is not newest: got %v want %v", dec[len(dec)-1].ReportedAt, base.Add(5*time.Minute))
	}

	// Returned slice is a defensive copy: mutating a nested GPU value must not
	// leak into a subsequent re-query.
	got[0].GPUs[0].UtilPct = 999
	reget, err := m.TelemetrySamples(ctx, "host_1", base, base.Add(10*time.Minute), 100)
	must(t, err)
	if reget[0].GPUs[0].UtilPct != 88 {
		t.Fatalf("TelemetrySamples leaked mutable GPU slice: %v", reget[0].GPUs[0].UtilPct)
	}

	// Prune: drop samples older than base+3m → only [+3m,+4m,+5m] remain.
	must(t, m.PruneTelemetrySamples(ctx, base.Add(3*time.Minute)))
	after, err := m.TelemetrySamples(ctx, "host_1", base, base.Add(10*time.Minute), 100)
	must(t, err)
	if len(after) != 3 {
		t.Fatalf("expected 3 samples after prune, got %d", len(after))
	}
	if !after[0].ReportedAt.Equal(base.Add(3 * time.Minute)) {
		t.Fatalf("post-prune oldest should be +3m, got %v", after[0].ReportedAt)
	}

	// FK: inserting for an unknown server classifies as ErrNotFound.
	if err := m.InsertTelemetrySample(ctx, TelemetrySample{ServerID: "nope", ReportedAt: base}); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown server, got %v", err)
	}
}

// TestMemoryStoreServerHardware exercises the memory-store twin of
// UpsertServerHardware/ServerHardwareByServer (mirrors the store conformance
// subtest, TestConformanceServerHardware, so the same absent/round-trip/overwrite/
// FK contract is guarded on the memory path too — the conformance suite only runs
// against *SQLStore). Gutting UpsertServerHardware/ServerHardwareByServer to a
// no-op must fail this test.
func TestMemoryStoreServerHardware(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	m := NewMemoryStore()
	must(t, m.CreateAIServer(ctx, AIServer{ID: "host_1", Name: "Host 1", Provider: ProviderMock, Endpoint: "mock://host", Status: ServerStatusActive, HealthStatus: HealthUnknown, CreatedAt: base, UpdatedAt: base}))

	// Absent -> (zero, false, nil).
	if _, ok, err := m.ServerHardwareByServer(ctx, "host_1"); err != nil || ok {
		t.Fatalf("ServerHardwareByServer(absent) = ok=%v err=%v, want ok=false", ok, err)
	}

	// Insert, then round-trip.
	first := ServerHardware{ServerID: "host_1", CollectedAt: base, ReportJSON: `{"agent_version":"1.0"}`, UpdatedAt: base}
	must(t, m.UpsertServerHardware(ctx, first))
	got, ok, err := m.ServerHardwareByServer(ctx, "host_1")
	must(t, err)
	if !ok || got.ReportJSON != `{"agent_version":"1.0"}` || !got.CollectedAt.Equal(base) || !got.UpdatedAt.Equal(base) {
		t.Fatalf("round-trip = %#v ok=%v", got, ok)
	}

	// Upsert overwrites the same server's row (still exactly one; report replaced).
	second := ServerHardware{ServerID: "host_1", CollectedAt: base.Add(time.Minute), ReportJSON: `{"agent_version":"2.0"}`, UpdatedAt: base.Add(time.Minute)}
	must(t, m.UpsertServerHardware(ctx, second))
	got, ok, err = m.ServerHardwareByServer(ctx, "host_1")
	must(t, err)
	if !ok || got.ReportJSON != `{"agent_version":"2.0"}` || !got.CollectedAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("after overwrite = %#v ok=%v", got, ok)
	}

	// FK: upserting for an unknown server classifies as ErrNotFound (mirrors
	// InsertTelemetrySample's unknown-server classification above).
	orphan := ServerHardware{ServerID: "nope", CollectedAt: base, ReportJSON: "{}", UpdatedAt: base}
	if err := m.UpsertServerHardware(ctx, orphan); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown server, got %v", err)
	}
}

// TestMemoryStoreServerAvailabilitySamples exercises MemoryStore's use of the
// shared availability reduction (routing.ReduceAvailabilitySamples, extracted
// from this package and the store package's former duplicate in RT-2). It
// mirrors the store conformance subtest so the SAME transition/gap/prune
// contract is guarded on the memory path too (also covered, more narrowly, by
// the store package's memory-vs-SQL conformance harness). Mutating the
// `||`->`&&` in the `changed` condition, or dropping the gap-boundary term,
// must fail this test.
func TestMemoryStoreServerAvailabilitySamples(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	m := NewMemoryStore()
	must(t, m.CreateAIServer(ctx, AIServer{ID: "host_1", Name: "Host 1", Provider: ProviderMock, Endpoint: "mock://host1", Status: ServerStatusActive, HealthStatus: HealthUnknown, CreatedAt: base, UpdatedAt: base}))
	must(t, m.CreateAIServer(ctx, AIServer{ID: "host_2", Name: "Host 2", Provider: ProviderMock, Endpoint: "mock://host2", Status: ServerStatusActive, HealthStatus: HealthUnknown, CreatedAt: base, UpdatedAt: base}))

	insert := func(serverID string, offset time.Duration, health string, reachable, active int, agent bool) {
		must(t, m.InsertServerAvailabilitySample(ctx, ServerAvailabilitySample{
			ServerID:       serverID,
			ReportedAt:     base.Add(offset),
			Health:         health,
			ReachableCount: reachable,
			ActiveCount:    active,
			AgentReporting: agent,
		}))
	}
	hasAt := func(got []ServerAvailabilitySample, offset time.Duration) bool {
		for _, g := range got {
			if g.ReportedAt.Equal(base.Add(offset)) {
				return true
			}
		}
		return false
	}

	// (a) Contiguous same-state run collapses to its endpoints (the redundant
	// middle heartbeat @+1m is dropped) while every transition is kept. A no-op
	// reduce would return all 6 rows.
	insert("host_1", 0, HealthHealthy, 1, 1, true)                // run start
	insert("host_1", time.Minute, HealthHealthy, 1, 1, true)      // redundant heartbeat
	insert("host_1", 2*time.Minute, HealthHealthy, 1, 1, true)    // run end (pre-transition)
	insert("host_1", 3*time.Minute, HealthUnhealthy, 0, 0, true)  // transition: health
	insert("host_1", 4*time.Minute, HealthUnhealthy, 0, 0, false) // transition: agent presence
	insert("host_1", 5*time.Minute, HealthHealthy, 1, 1, false)   // transition: health back

	got, err := m.ServerAvailabilitySamples(ctx, "host_1", base.Add(-time.Minute), base.Add(6*time.Minute), 10000)
	must(t, err)
	if len(got) != 5 {
		t.Fatalf("expected 5 reduced samples (redundant heartbeat dropped), got %d: %+v", len(got), got)
	}
	if hasAt(got, time.Minute) {
		t.Fatalf("redundant contiguous heartbeat @+1m should be dropped: %+v", got)
	}
	for _, off := range []time.Duration{0, 2 * time.Minute, 3 * time.Minute, 4 * time.Minute, 5 * time.Minute} {
		if !hasAt(got, off) {
			t.Fatalf("expected transition/endpoint sample @%v kept: %+v", off, got)
		}
	}
	if got[0].Health != HealthHealthy || !got[0].AgentReporting || got[0].ReachableCount != 1 || got[0].ActiveCount != 1 {
		t.Fatalf("first sample round-trip mismatch: %+v", got[0])
	}
	// gap_before: host_1 is sampled continuously (<= 1m spacing), so no reduced
	// sample may carry GapBefore (a within-floor predecessor yields false).
	for _, g := range got {
		if g.GapBefore {
			t.Fatalf("host_1 has no >gap-floor spacing; no reduced sample should carry GapBefore: %+v", g)
		}
	}

	// (b) Gap boundary: a same-state sample separated by > the 10m gap floor keeps
	// BOTH boundaries; the contiguous @+1m heartbeat still drops.
	insert("host_2", 0, HealthHealthy, 1, 0, false)
	insert("host_2", time.Minute, HealthHealthy, 1, 0, false)    // redundant heartbeat
	insert("host_2", 2*time.Minute, HealthHealthy, 1, 0, false)  // gap boundary (near)
	insert("host_2", 42*time.Minute, HealthHealthy, 1, 0, false) // gap boundary (far, 40m gap)

	gap, err := m.ServerAvailabilitySamples(ctx, "host_2", base.Add(-time.Minute), base.Add(50*time.Minute), 10000)
	must(t, err)
	if len(gap) != 3 {
		t.Fatalf("expected 3 gap-reduced samples, got %d: %+v", len(gap), gap)
	}
	if hasAt(gap, time.Minute) {
		t.Fatalf("redundant contiguous heartbeat @+1m should be dropped (gap): %+v", gap)
	}
	if !hasAt(gap, 2*time.Minute) || !hasAt(gap, 42*time.Minute) {
		t.Fatalf("both gap boundaries (@+2m and @+42m) must be kept: %+v", gap)
	}
	// gap_before flag: the FAR side of the > gap-floor spacing (@+42m, raw predecessor
	// @+2m is 40m away) carries GapBefore=true; the NEAR side (@+2m, raw predecessor
	// @+1m is 1m away) stays false. Set only on read by the reduction pre-pass.
	findAt := func(got []ServerAvailabilitySample, offset time.Duration) (ServerAvailabilitySample, bool) {
		for _, g := range got {
			if g.ReportedAt.Equal(base.Add(offset)) {
				return g, true
			}
		}
		return ServerAvailabilitySample{}, false
	}
	if far, ok := findAt(gap, 42*time.Minute); !ok || !far.GapBefore {
		t.Fatalf("expected @+42m GapBefore=true (raw >gap-floor predecessor): %+v", gap)
	}
	if near, ok := findAt(gap, 2*time.Minute); !ok || near.GapBefore {
		t.Fatalf("expected @+2m GapBefore=false (within-floor raw predecessor): %+v", gap)
	}

	// (c) FK: unknown server → ErrNotFound.
	if err := m.InsertServerAvailabilitySample(ctx, ServerAvailabilitySample{ServerID: "nope", ReportedAt: base, Health: HealthHealthy}); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown server, got %v", err)
	}

	// (d) Prune drops samples older than the cutoff.
	must(t, m.PruneServerAvailabilitySamples(ctx, base.Add(3*time.Minute)))
	after, err := m.ServerAvailabilitySamples(ctx, "host_1", base.Add(-time.Minute), base.Add(6*time.Minute), 10000)
	must(t, err)
	if len(after) != 3 {
		t.Fatalf("expected 3 samples after prune, got %d: %+v", len(after), after)
	}
	if !after[0].ReportedAt.Equal(base.Add(3 * time.Minute)) {
		t.Fatalf("post-prune oldest should be +3m, got %v", after[0].ReportedAt)
	}

	// (e) NetBird dimension: a connected->disconnected->connected run (health + agent
	// held constant) must be preserved as transitions by the memory state key, and
	// netbird_connected must round-trip (value copy on insert). A state key that
	// ignored NetbirdConnected would collapse the middle sample (n=3, i=1) to 2.
	must(t, m.CreateAIServer(ctx, AIServer{ID: "host_3", Name: "Host 3", Provider: ProviderMock, Endpoint: "mock://host3", Status: ServerStatusActive, HealthStatus: HealthUnknown, CreatedAt: base, UpdatedAt: base}))
	insertNB := func(offset time.Duration, connected bool) {
		must(t, m.InsertServerAvailabilitySample(ctx, ServerAvailabilitySample{
			ServerID:         "host_3",
			ReportedAt:       base.Add(offset),
			Health:           HealthHealthy,
			ReachableCount:   1,
			ActiveCount:      1,
			AgentReporting:   true,
			NetbirdConnected: connected,
		}))
	}
	insertNB(0, true)
	insertNB(time.Minute, false)
	insertNB(2*time.Minute, true)
	nb, err := m.ServerAvailabilitySamples(ctx, "host_3", base.Add(-time.Minute), base.Add(3*time.Minute), 10000)
	must(t, err)
	if len(nb) != 3 {
		t.Fatalf("expected 3 NetBird-transition samples (memory state key must fold NetbirdConnected), got %d: %+v", len(nb), nb)
	}
	if !nb[0].NetbirdConnected || nb[1].NetbirdConnected || !nb[2].NetbirdConnected {
		t.Fatalf("NetBird connectivity did not round-trip: %+v", nb)
	}
}

func TestMemoryStoreActiveMappingsForModel(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	m := NewMemoryStore()
	must(t, m.CreateAIServer(ctx, AIServer{ID: "srv_1", Name: "S1", Domain: "s1.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	must(t, m.CreateApplication(ctx, Application{ID: "app_ok", ServerID: "srv_1", Type: ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{APIFlavorOpenAI}, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, m.CreateApplication(ctx, Application{ID: "app_disabled", ServerID: "srv_1", Type: ProviderVLLM, Port: 8001, Scheme: "https", APIFlavors: []string{APIFlavorOpenAI}, Status: ServerStatusDisabled, CreatedAt: now, UpdatedAt: now}))
	must(t, m.CreateApplication(ctx, Application{ID: "app_noflavor", ServerID: "srv_1", Type: ProviderVLLM, Port: 8002, Scheme: "https", APIFlavors: []string{APIFlavorAnthropic}, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	// active mapping on the eligible application
	must(t, m.CreateMapping(ctx, ModelMapping{ID: "map_ok", ApplicationID: "app_ok", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	// disabled mapping (excluded)
	must(t, m.CreateMapping(ctx, ModelMapping{ID: "map_off", ApplicationID: "app_ok", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5-off", Status: ServerStatusDisabled, CreatedAt: now, UpdatedAt: now}))
	// active mapping but on a disabled application (excluded)
	must(t, m.CreateMapping(ctx, ModelMapping{ID: "map_disabled_app", ApplicationID: "app_disabled", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	// active mapping on an application lacking the requested flavor (excluded for openai)
	must(t, m.CreateMapping(ctx, ModelMapping{ID: "map_noflavor", ApplicationID: "app_noflavor", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))

	got, err := m.ActiveMappingsForModel(ctx, "qwen-coder", APIFlavorOpenAI)
	must(t, err)
	if len(got) != 1 {
		t.Fatalf("candidates = %d, want 1: %#v", len(got), got)
	}
	c := got[0]
	if c.Mapping.ID != "map_ok" || c.Application.ID != "app_ok" || c.Server.ID != "srv_1" {
		t.Fatalf("candidate = %#v", c)
	}
	// returned copies must be defensive
	c.Application.APIFlavors[0] = "mutated"
	again, err := m.ActiveMappingsForModel(ctx, "qwen-coder", APIFlavorOpenAI)
	must(t, err)
	if again[0].Application.APIFlavors[0] != APIFlavorOpenAI {
		t.Fatalf("ActiveMappingsForModel leaked mutable slice: %#v", again[0].Application.APIFlavors)
	}
	// unknown model → empty
	none, err := m.ActiveMappingsForModel(ctx, "missing", APIFlavorOpenAI)
	must(t, err)
	if len(none) != 0 {
		t.Fatalf("unknown model candidates = %#v, want none", none)
	}
}

func TestMemoryStoreApplicationHealthIntervalRoundtripAndDefault(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	m := NewMemoryStore()
	must(t, m.CreateAIServer(ctx, AIServer{ID: "srv_1", Name: "S", Domain: "s.test", Status: ServerStatusActive, HealthStatus: HealthUnknown, CreatedAt: now, UpdatedAt: now}))

	// Custom interval round-trips through CreateApplication + ApplicationByID +
	// ApplicationsByServer.
	must(t, m.CreateApplication(ctx, Application{ID: "app_interval", ServerID: "srv_1", Type: ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{APIFlavorOpenAI}, Status: ServerStatusActive, HealthCheckIntervalSeconds: 45, CreatedAt: now, UpdatedAt: now}))
	got, err := m.ApplicationByID(ctx, "app_interval")
	must(t, err)
	if got.HealthCheckIntervalSeconds != 45 {
		t.Fatalf("ApplicationByID health_check_interval_seconds = %d, want 45", got.HealthCheckIntervalSeconds)
	}
	list, err := m.ApplicationsByServer(ctx, "srv_1")
	must(t, err)
	if len(list) != 1 || list[0].HealthCheckIntervalSeconds != 45 {
		t.Fatalf("ApplicationsByServer = %#v, want one app with interval 45", list)
	}

	// UpdateApplication changes the interval.
	upd := got
	upd.HealthCheckIntervalSeconds = 5
	must(t, m.UpdateApplication(ctx, upd))
	updated, err := m.ApplicationByID(ctx, "app_interval")
	must(t, err)
	if updated.HealthCheckIntervalSeconds != 5 {
		t.Fatalf("updated health_check_interval_seconds = %d, want 5", updated.HealthCheckIntervalSeconds)
	}

	// A create that omits the interval reads back the 0 default.
	must(t, m.CreateApplication(ctx, Application{ID: "app_default", ServerID: "srv_1", Type: ProviderVLLM, Port: 8001, Scheme: "https", APIFlavors: []string{APIFlavorOpenAI}, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	gotDflt, err := m.ApplicationByID(ctx, "app_default")
	must(t, err)
	if gotDflt.HealthCheckIntervalSeconds != 0 {
		t.Fatalf("default health_check_interval_seconds = %d, want 0", gotDflt.HealthCheckIntervalSeconds)
	}
}

func TestMemoryStoreApplicationsAndMappings(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	now := time.Now().UTC()
	must(t, m.CreateAIServer(ctx, AIServer{ID: "srv_1", Name: "S", Domain: "s.test", Status: ServerStatusActive, HealthStatus: HealthUnknown, CreatedAt: now, UpdatedAt: now}))

	app := Application{ID: "app_1", ServerID: "srv_1", Type: ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{APIFlavorOpenAI}, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	must(t, m.CreateApplication(ctx, app))

	dup := app
	dup.ID = "app_dup"
	if err := m.CreateApplication(ctx, dup); !errors.Is(err, storeerr.ErrConflict) {
		t.Fatalf("duplicate port err = %v, want ErrConflict", err)
	}

	// A different server may reuse the same port without conflict.
	must(t, m.CreateAIServer(ctx, AIServer{ID: "srv_2", Name: "S2", Domain: "s2.test", Status: ServerStatusActive, HealthStatus: HealthUnknown, CreatedAt: now, UpdatedAt: now}))
	other := Application{ID: "app_other_srv", ServerID: "srv_2", Type: ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{APIFlavorOpenAI}, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	must(t, m.CreateApplication(ctx, other))

	app2 := Application{ID: "app_2", ServerID: "srv_1", Type: ProviderOllama, Port: 8001, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	must(t, m.CreateApplication(ctx, app2))

	apps, err := m.ApplicationsByServer(ctx, "srv_1")
	must(t, err)
	if len(apps) != 2 {
		t.Fatalf("apps = %d, want 2", len(apps))
	}
	if apps[0].ID != "app_1" || apps[1].ID != "app_2" {
		t.Fatalf("apps not sorted by id: %#v", apps)
	}

	got, err := m.ApplicationByID(ctx, "app_1")
	must(t, err)
	if got.Port != 8000 {
		t.Fatalf("ApplicationByID port = %d, want 8000", got.Port)
	}
	got.APIFlavors[0] = "mutated"
	again, err := m.ApplicationByID(ctx, "app_1")
	must(t, err)
	if again.APIFlavors[0] != APIFlavorOpenAI {
		t.Fatalf("ApplicationByID leaked mutable slice: %#v", again)
	}

	if _, err := m.ApplicationByID(ctx, "missing"); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("ApplicationByID missing err = %v, want ErrNotFound", err)
	}

	update := got
	update.Status = ServerStatusDisabled
	must(t, m.UpdateApplication(ctx, update))
	updated, err := m.ApplicationByID(ctx, "app_1")
	must(t, err)
	if updated.Status != ServerStatusDisabled {
		t.Fatalf("UpdateApplication status = %q, want disabled", updated.Status)
	}
	if err := m.UpdateApplication(ctx, Application{ID: "missing", ServerID: "srv_1"}); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("UpdateApplication missing err = %v, want ErrNotFound", err)
	}

	must(t, m.CreateMapping(ctx, ModelMapping{ID: "map_1", ApplicationID: "app_1", GatewayModelName: "qwen", AppModelName: "qwen2.5", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, m.CreateMapping(ctx, ModelMapping{ID: "map_2", ApplicationID: "app_2", GatewayModelName: "llama", AppModelName: "llama3", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))

	byApp, err := m.MappingsByApplication(ctx, "app_1")
	must(t, err)
	byServer, err := m.MappingsByServer(ctx, "srv_1")
	must(t, err)
	if len(byApp) != 1 || len(byServer) != 2 {
		t.Fatalf("byApp=%d byServer=%d", len(byApp), len(byServer))
	}
	if byServer[0].ID != "map_1" || byServer[1].ID != "map_2" {
		t.Fatalf("byServer not sorted by id: %#v", byServer)
	}

	mapping, err := m.MappingByID(ctx, "map_1")
	must(t, err)
	if mapping.AppModelName != "qwen2.5" {
		t.Fatalf("MappingByID = %#v", mapping)
	}
	if _, err := m.MappingByID(ctx, "missing"); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("MappingByID missing err = %v, want ErrNotFound", err)
	}

	mappingUpdate := mapping
	mappingUpdate.Status = ServerStatusDisabled
	must(t, m.UpdateMapping(ctx, mappingUpdate))
	updatedMapping, err := m.MappingByID(ctx, "map_1")
	must(t, err)
	if updatedMapping.Status != ServerStatusDisabled {
		t.Fatalf("UpdateMapping status = %q, want disabled", updatedMapping.Status)
	}
	if err := m.UpdateMapping(ctx, ModelMapping{ID: "missing"}); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("UpdateMapping missing err = %v, want ErrNotFound", err)
	}

	// Deleting an application cascades its mappings.
	must(t, m.DeleteApplication(ctx, "app_1"))
	if got, _ := m.MappingsByApplication(ctx, "app_1"); len(got) != 0 {
		t.Fatalf("mappings should cascade on app delete, got %#v", got)
	}
	if _, err := m.ApplicationByID(ctx, "app_1"); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("ApplicationByID after delete err = %v, want ErrNotFound", err)
	}
	if err := m.DeleteApplication(ctx, "app_1"); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("DeleteApplication missing err = %v, want ErrNotFound", err)
	}
	if err := m.DeleteMapping(ctx, "missing"); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("DeleteMapping missing err = %v, want ErrNotFound", err)
	}

	// Deleting the server cascades its remaining applications and mappings.
	must(t, m.DeleteAIServer(ctx, "srv_1"))
	if got, _ := m.ApplicationsByServer(ctx, "srv_1"); len(got) != 0 {
		t.Fatalf("applications should cascade on server delete, got %#v", got)
	}
	if got, _ := m.MappingsByServer(ctx, "srv_1"); len(got) != 0 {
		t.Fatalf("mappings should cascade on server delete, got %#v", got)
	}
	if _, err := m.ApplicationByID(ctx, "app_2"); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("app_2 should be gone after server delete, err = %v", err)
	}
	if _, err := m.MappingByID(ctx, "map_2"); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("map_2 should be gone after server delete, err = %v", err)
	}

	// The other server's application/port is unaffected.
	if _, err := m.ApplicationByID(ctx, "app_other_srv"); err != nil {
		t.Fatalf("app_other_srv should be unaffected, err = %v", err)
	}
}

func TestMemoryStoreServerOwners(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	now := time.Now().UTC()
	if err := m.CreateAIServer(ctx, AIServer{ID: "srv_1", Name: "S1", Domain: "s1", Status: ServerStatusActive, HealthStatus: HealthUnknown, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := m.SetServerOwners(ctx, "srv_1", []string{"usr_a", "usr_b"}); err != nil {
		t.Fatalf("SetServerOwners: %v", err)
	}
	owners, err := m.ServerOwners(ctx, "srv_1")
	if err != nil || len(owners) != 2 {
		t.Fatalf("owners = %#v err=%v", owners, err)
	}
	byOwner, err := m.ServersByOwner(ctx, "usr_a")
	if err != nil || len(byOwner) != 1 || byOwner[0].ID != "srv_1" {
		t.Fatalf("by owner = %#v err=%v", byOwner, err)
	}
	if err := m.DeleteAIServer(ctx, "srv_1"); err != nil {
		t.Fatalf("DeleteAIServer: %v", err)
	}
	if got, _ := m.ServersByOwner(ctx, "usr_a"); len(got) != 0 {
		t.Fatalf("owners should be gone after delete, got %#v", got)
	}
}

func TestMemoryStoreAgentTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	m := NewMemoryStore()
	must(t, m.CreateAIServer(ctx, AIServer{ID: "srv_1", Name: "S1", Domain: "s1.test", Status: ServerStatusActive, HealthStatus: HealthUnknown, CreatedAt: now, UpdatedAt: now}))

	// none yet
	if _, ok, err := m.AgentTokenByServer(ctx, "srv_1"); err != nil || ok {
		t.Fatalf("AgentTokenByServer before create = ok %v err %v", ok, err)
	}
	// unknown server → ErrNotFound
	if err := m.UpsertAgentToken(ctx, AgentToken{ID: "agt_x", ServerID: "missing", SecretPrefix: "opaigw_", CreatedAt: now, UpdatedAt: now}, "hash-x"); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("Upsert unknown server = %v, want ErrNotFound", err)
	}
	// create
	must(t, m.UpsertAgentToken(ctx, AgentToken{ID: "agt_1", ServerID: "srv_1", SecretPrefix: "opaigw_a", CreatedAt: now, UpdatedAt: now}, "hash-1"))
	got, ok, err := m.AgentTokenByServer(ctx, "srv_1")
	must(t, err)
	if !ok || got.ID != "agt_1" || got.SecretPrefix != "opaigw_a" {
		t.Fatalf("AgentTokenByServer = %#v ok=%v", got, ok)
	}
	// lookup by hash → server id, bumps last_used_at
	serverID, ok, err := m.LookupAgentToken(ctx, "hash-1")
	must(t, err)
	if !ok || serverID != "srv_1" {
		t.Fatalf("LookupAgentToken = %q ok=%v", serverID, ok)
	}
	if again, _, _ := m.AgentTokenByServer(ctx, "srv_1"); again.LastUsedAt == nil {
		t.Fatalf("LookupAgentToken did not bump last_used_at")
	}
	// rotate: new hash replaces old; old hash no longer resolves
	must(t, m.UpsertAgentToken(ctx, AgentToken{ID: "agt_2", ServerID: "srv_1", SecretPrefix: "opaigw_b", CreatedAt: now, UpdatedAt: now}, "hash-2"))
	if _, ok, _ := m.LookupAgentToken(ctx, "hash-1"); ok {
		t.Fatalf("old hash still resolves after rotate")
	}
	if sid, ok, _ := m.LookupAgentToken(ctx, "hash-2"); !ok || sid != "srv_1" {
		t.Fatalf("new hash after rotate = %q ok=%v", sid, ok)
	}
	// revoke (idempotent)
	must(t, m.DeleteAgentTokenByServer(ctx, "srv_1"))
	must(t, m.DeleteAgentTokenByServer(ctx, "srv_1"))
	if _, ok, _ := m.AgentTokenByServer(ctx, "srv_1"); ok {
		t.Fatalf("token still present after revoke")
	}
	// cross-server hash clash → ErrConflict
	must(t, m.CreateAIServer(ctx, AIServer{ID: "srv_2", Name: "S2", Domain: "s2.test", Status: ServerStatusActive, HealthStatus: HealthUnknown, CreatedAt: now, UpdatedAt: now}))
	must(t, m.UpsertAgentToken(ctx, AgentToken{ID: "agt_s1", ServerID: "srv_1", SecretPrefix: "p", CreatedAt: now, UpdatedAt: now}, "hash-shared"))
	if err := m.UpsertAgentToken(ctx, AgentToken{ID: "agt_dup", ServerID: "srv_2", SecretPrefix: "p", CreatedAt: now, UpdatedAt: now}, "hash-shared"); !errors.Is(err, storeerr.ErrConflict) {
		t.Fatalf("reusing another server's hash = %v, want ErrConflict", err)
	}
	// cascade: recreate token then delete server
	must(t, m.UpsertAgentToken(ctx, AgentToken{ID: "agt_3", ServerID: "srv_1", SecretPrefix: "opaigw_c", CreatedAt: now, UpdatedAt: now}, "hash-3"))
	must(t, m.DeleteAIServer(ctx, "srv_1"))
	if _, ok, _ := m.LookupAgentToken(ctx, "hash-3"); ok {
		t.Fatalf("agent token survived server delete (no cascade)")
	}
}

func TestMemoryStoreTelemetrySamplePowerRoundTrip(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store := NewMemoryStore()
	if err := store.CreateAIServer(ctx, AIServer{ID: "host_p", Name: "P", Provider: ProviderMock, Endpoint: "mock://p", Status: ServerStatusActive, HealthStatus: HealthUnknown, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	cpu := 42.0
	if err := store.InsertTelemetrySample(ctx, TelemetrySample{ServerID: "host_p", ReportedAt: now, CPUPowerW: &cpu, SystemPowerW: nil}); err != nil {
		t.Fatalf("InsertTelemetrySample: %v", err)
	}
	got, err := store.TelemetrySamples(ctx, "host_p", now.Add(-time.Minute), now.Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("TelemetrySamples: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 sample, got %d", len(got))
	}
	if got[0].CPUPowerW == nil || *got[0].CPUPowerW != 42.0 {
		t.Fatalf("CPUPowerW round-trip = %v, want 42.0", got[0].CPUPowerW)
	}
	if got[0].SystemPowerW != nil {
		t.Fatalf("SystemPowerW = %v, want nil", *got[0].SystemPowerW)
	}
}

func TestMemoryStoreTelemetrySampleTempRoundTrip(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store := NewMemoryStore()
	if err := store.CreateAIServer(ctx, AIServer{ID: "host_t", Name: "T", Provider: ProviderMock, Endpoint: "mock://t", Status: ServerStatusActive, HealthStatus: HealthUnknown, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	temp := 58.5
	if err := store.InsertTelemetrySample(ctx, TelemetrySample{ServerID: "host_t", ReportedAt: now, CPUTempC: &temp}); err != nil {
		t.Fatalf("InsertTelemetrySample: %v", err)
	}
	if err := store.InsertTelemetrySample(ctx, TelemetrySample{ServerID: "host_t", ReportedAt: now.Add(time.Second), CPUTempC: nil}); err != nil {
		t.Fatalf("InsertTelemetrySample (nil): %v", err)
	}
	got, err := store.TelemetrySamples(ctx, "host_t", now.Add(-time.Minute), now.Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("TelemetrySamples: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 samples, got %d", len(got))
	}
	if got[0].CPUTempC == nil || *got[0].CPUTempC != 58.5 {
		t.Fatalf("CPUTempC round-trip = %v, want 58.5", got[0].CPUTempC)
	}
	if got[1].CPUTempC != nil {
		t.Fatalf("CPUTempC = %v, want nil", *got[1].CPUTempC)
	}
}

// --- Services (Phase 1 service accounts, migration v40) --------------------
//
// These cover the 10 MemoryStore service methods added alongside the v40
// schema (CreateService/UpdateService/ServiceByID/Services/ServicesByDelegate/
// DeleteService/SetServiceDelegates/ServiceDelegates/SetServiceAllowedModels/
// ServiceAllowedModels), mirroring the SQLite conformance coverage.

func TestMemoryStoreCreateServiceAndByID(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	m := NewMemoryStore()

	// Empty Status defaults to active (mirrors the SQL column default).
	if err := m.CreateService(ctx, Service{ID: "svc_1", Name: "Svc 1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	got, err := m.ServiceByID(ctx, "svc_1")
	if err != nil {
		t.Fatalf("ServiceByID: %v", err)
	}
	if got.Name != "Svc 1" || got.Status != ServerStatusActive {
		t.Fatalf("ServiceByID = %#v, want Name=Svc 1 Status=active", got)
	}

	// Duplicate id -> ErrConflict.
	if err := m.CreateService(ctx, Service{ID: "svc_1", Name: "dup", CreatedAt: now, UpdatedAt: now}); !errors.Is(err, storeerr.ErrConflict) {
		t.Fatalf("CreateService duplicate id = %v, want ErrConflict", err)
	}

	// Unknown id -> ErrNotFound.
	if _, err := m.ServiceByID(ctx, "svc_missing"); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("ServiceByID(unknown) = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreServices(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	m := NewMemoryStore()
	must(t, m.CreateService(ctx, Service{ID: "svc_b", Name: "B", CreatedAt: now, UpdatedAt: now}))
	must(t, m.CreateService(ctx, Service{ID: "svc_a", Name: "A", CreatedAt: now, UpdatedAt: now}))

	all, err := m.Services(ctx)
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if len(all) != 2 || all[0].ID != "svc_a" || all[1].ID != "svc_b" {
		t.Fatalf("Services = %#v, want [svc_a, svc_b] sorted by id", all)
	}
}

func TestMemoryStoreUpdateService(t *testing.T) {
	ctx := context.Background()
	created := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	updated := created.Add(time.Hour)
	m := NewMemoryStore()
	must(t, m.CreateService(ctx, Service{ID: "svc_1", Name: "Original", Status: ServerStatusActive, CreatedAt: created, UpdatedAt: created}))

	if err := m.UpdateService(ctx, Service{ID: "svc_1", Name: "Renamed", Description: "d", Status: ServerStatusDisabled, UpdatedAt: updated}); err != nil {
		t.Fatalf("UpdateService: %v", err)
	}
	got, err := m.ServiceByID(ctx, "svc_1")
	if err != nil {
		t.Fatalf("ServiceByID: %v", err)
	}
	if got.Name != "Renamed" || got.Description != "d" || got.Status != ServerStatusDisabled {
		t.Fatalf("UpdateService result = %#v", got)
	}
	// created_at is never touched by UpdateService, mirroring the SQL UPDATE.
	if !got.CreatedAt.Equal(created) {
		t.Fatalf("CreatedAt = %v, want unchanged %v", got.CreatedAt, created)
	}
	if !got.UpdatedAt.Equal(updated) {
		t.Fatalf("UpdatedAt = %v, want %v", got.UpdatedAt, updated)
	}

	if err := m.UpdateService(ctx, Service{ID: "svc_missing", Name: "x"}); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("UpdateService(unknown) = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreServiceDelegatesAndByDelegate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	m := NewMemoryStore()
	must(t, m.CreateService(ctx, Service{ID: "svc_1", Name: "Svc 1", CreatedAt: now, UpdatedAt: now}))
	must(t, m.CreateService(ctx, Service{ID: "svc_2", Name: "Svc 2", CreatedAt: now, UpdatedAt: now}))

	// Unknown service id -> ErrNotFound, even for an empty set.
	if err := m.SetServiceDelegates(ctx, "svc_missing", nil); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("SetServiceDelegates(unknown) = %v, want ErrNotFound", err)
	}
	// Duplicate user id within the set -> ErrConflict.
	dup := []ServiceDelegate{{UserID: "usr_a", CanManageSettings: false}, {UserID: "usr_a", CanManageSettings: true}}
	if err := m.SetServiceDelegates(ctx, "svc_1", dup); !errors.Is(err, storeerr.ErrConflict) {
		t.Fatalf("SetServiceDelegates(duplicate) = %v, want ErrConflict", err)
	}

	if err := m.SetServiceDelegates(ctx, "svc_1", []ServiceDelegate{
		{UserID: "usr_full", CanManageSettings: true},
		{UserID: "usr_token", CanManageSettings: false},
	}); err != nil {
		t.Fatalf("SetServiceDelegates: %v", err)
	}
	must(t, m.SetServiceDelegates(ctx, "svc_2", []ServiceDelegate{{UserID: "usr_full", CanManageSettings: false}}))

	delegates, err := m.ServiceDelegates(ctx, "svc_1")
	if err != nil {
		t.Fatalf("ServiceDelegates: %v", err)
	}
	if len(delegates) != 2 || delegates[0].UserID != "usr_full" || !delegates[0].CanManageSettings ||
		delegates[1].UserID != "usr_token" || delegates[1].CanManageSettings {
		t.Fatalf("ServiceDelegates = %#v, want [usr_full(full), usr_token(token)] sorted by user id", delegates)
	}
	// An unknown/no-delegate service returns an empty, non-nil slice.
	empty, err := m.ServiceDelegates(ctx, "svc_missing")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("ServiceDelegates(unknown) = %#v err=%v, want empty non-nil slice", empty, err)
	}

	// SetServiceDelegates REPLACES wholesale: usr_full is dropped from svc_1 here.
	must(t, m.SetServiceDelegates(ctx, "svc_1", []ServiceDelegate{{UserID: "usr_token", CanManageSettings: false}}))
	delegates, err = m.ServiceDelegates(ctx, "svc_1")
	if err != nil || len(delegates) != 1 || delegates[0].UserID != "usr_token" {
		t.Fatalf("ServiceDelegates after replace = %#v err=%v", delegates, err)
	}

	// ServicesByDelegate matches usr_full at EITHER stage: full on svc_1 (until
	// replaced above) and token-level on svc_2.
	byFull, err := m.ServicesByDelegate(ctx, "usr_full")
	if err != nil {
		t.Fatalf("ServicesByDelegate: %v", err)
	}
	if len(byFull) != 1 || byFull[0].ID != "svc_2" {
		t.Fatalf("ServicesByDelegate(usr_full) = %#v, want [svc_2] (svc_1 replaced above)", byFull)
	}
	byToken, err := m.ServicesByDelegate(ctx, "usr_token")
	if err != nil || len(byToken) != 1 || byToken[0].ID != "svc_1" {
		t.Fatalf("ServicesByDelegate(usr_token) = %#v err=%v, want [svc_1]", byToken, err)
	}
	if byNone, err := m.ServicesByDelegate(ctx, "usr_stranger"); err != nil || len(byNone) != 0 {
		t.Fatalf("ServicesByDelegate(stranger) = %#v err=%v, want empty", byNone, err)
	}
}

func TestMemoryStoreServiceAllowedModels(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	m := NewMemoryStore()
	must(t, m.CreateService(ctx, Service{ID: "svc_1", Name: "Svc 1", CreatedAt: now, UpdatedAt: now}))

	// Unknown service id -> ErrNotFound, even for an empty set.
	if err := m.SetServiceAllowedModels(ctx, "svc_missing", nil); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("SetServiceAllowedModels(unknown) = %v, want ErrNotFound", err)
	}
	// Duplicate model name within the set -> ErrConflict.
	if err := m.SetServiceAllowedModels(ctx, "svc_1", []string{"model-a", "model-a"}); !errors.Is(err, storeerr.ErrConflict) {
		t.Fatalf("SetServiceAllowedModels(duplicate) = %v, want ErrConflict", err)
	}

	// A fresh service's allowlist is empty ("every model allowed").
	empty, err := m.ServiceAllowedModels(ctx, "svc_1")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("ServiceAllowedModels(fresh) = %#v err=%v, want empty non-nil slice", empty, err)
	}

	if err := m.SetServiceAllowedModels(ctx, "svc_1", []string{"zeta", "alpha"}); err != nil {
		t.Fatalf("SetServiceAllowedModels: %v", err)
	}
	models, err := m.ServiceAllowedModels(ctx, "svc_1")
	if err != nil {
		t.Fatalf("ServiceAllowedModels: %v", err)
	}
	if len(models) != 2 || models[0] != "alpha" || models[1] != "zeta" {
		t.Fatalf("ServiceAllowedModels = %#v, want [alpha, zeta] sorted", models)
	}

	// SetServiceAllowedModels REPLACES wholesale.
	must(t, m.SetServiceAllowedModels(ctx, "svc_1", []string{"beta"}))
	models, err = m.ServiceAllowedModels(ctx, "svc_1")
	if err != nil || len(models) != 1 || models[0] != "beta" {
		t.Fatalf("ServiceAllowedModels after replace = %#v err=%v", models, err)
	}
}

func TestMemoryStoreDeleteServiceCascades(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	m := NewMemoryStore()
	must(t, m.CreateService(ctx, Service{ID: "svc_1", Name: "Svc 1", CreatedAt: now, UpdatedAt: now}))
	must(t, m.SetServiceDelegates(ctx, "svc_1", []ServiceDelegate{{UserID: "usr_a", CanManageSettings: true}}))
	must(t, m.SetServiceAllowedModels(ctx, "svc_1", []string{"model-a"}))

	if err := m.DeleteService(ctx, "svc_1"); err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	if _, err := m.ServiceByID(ctx, "svc_1"); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("ServiceByID after delete = %v, want ErrNotFound", err)
	}
	// The cascade (delegates + allowlist) is gone too, though a plain read of an
	// unknown id is an empty slice rather than an error (mirrors the "no rows"
	// SQL semantics — Service*/ServiceAllowedModels never error on a missing id).
	if delegates, err := m.ServiceDelegates(ctx, "svc_1"); err != nil || len(delegates) != 0 {
		t.Fatalf("ServiceDelegates after delete = %#v err=%v, want empty", delegates, err)
	}
	if models, err := m.ServiceAllowedModels(ctx, "svc_1"); err != nil || len(models) != 0 {
		t.Fatalf("ServiceAllowedModels after delete = %#v err=%v, want empty", models, err)
	}
	if byDelegate, err := m.ServicesByDelegate(ctx, "usr_a"); err != nil || len(byDelegate) != 0 {
		t.Fatalf("ServicesByDelegate after delete = %#v err=%v, want empty", byDelegate, err)
	}

	// Unknown id -> ErrNotFound.
	if err := m.DeleteService(ctx, "svc_missing"); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("DeleteService(unknown) = %v, want ErrNotFound", err)
	}
}

// TestMemoryStoreServerAdminGroups covers the routing.MemoryStore mirror of
// the server<->admin-group linkage (admin-group permissions Phase B,
// migration v50, Task 2): SetServerAdminGroup/RemoveServerAdminGroup/
// ServerAdminGroups/ServersByAdminGroups + UpdateServerSystemGroup, plus the
// server-delete cascade. (Group-delete cascade is a SQL-store-only FK — the
// routing.MemoryStore mirror cannot observe a user-group deletion, which
// lives in the separate portal.MemoryDirectory; see the store's conformance
// suite for the SQL-side cascade.)
func TestMemoryStoreServerAdminGroups(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	now := time.Now().UTC()
	if err := m.CreateAIServer(ctx, AIServer{ID: "srv_sag", Name: "SAG", Domain: "sag", Status: ServerStatusActive, HealthStatus: HealthUnknown, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}

	// A missing server -> ErrNotFound (mirrors the SQL FK violation).
	if err := m.SetServerAdminGroup(ctx, "srv_missing", "ugrp_a"); err != storeerr.ErrNotFound {
		t.Fatalf("SetServerAdminGroup(missing server) = %v, want ErrNotFound", err)
	}

	if err := m.SetServerAdminGroup(ctx, "srv_sag", "ugrp_a"); err != nil {
		t.Fatalf("SetServerAdminGroup(a): %v", err)
	}
	// Idempotent re-link: no error, no duplicate.
	if err := m.SetServerAdminGroup(ctx, "srv_sag", "ugrp_a"); err != nil {
		t.Fatalf("re-SetServerAdminGroup(a): %v", err)
	}
	if err := m.SetServerAdminGroup(ctx, "srv_sag", "ugrp_b"); err != nil {
		t.Fatalf("SetServerAdminGroup(b): %v", err)
	}
	groups, err := m.ServerAdminGroups(ctx, "srv_sag")
	if err != nil || len(groups) != 2 {
		t.Fatalf("ServerAdminGroups = %#v err=%v, want 2 entries", groups, err)
	}

	byA, err := m.ServersByAdminGroups(ctx, []string{"ugrp_a"})
	if err != nil || len(byA) != 1 || byA[0].ID != "srv_sag" {
		t.Fatalf("ServersByAdminGroups(a) = %#v err=%v", byA, err)
	}
	// Deduped: a server linked to both groups appears once when both are queried.
	byBoth, err := m.ServersByAdminGroups(ctx, []string{"ugrp_a", "ugrp_b"})
	if err != nil || len(byBoth) != 1 || byBoth[0].ID != "srv_sag" {
		t.Fatalf("ServersByAdminGroups(a,b) = %#v err=%v, want a single srv_sag", byBoth, err)
	}
	// Empty input -> empty output, no scan.
	byNone, err := m.ServersByAdminGroups(ctx, []string{})
	if err != nil || len(byNone) != 0 {
		t.Fatalf("ServersByAdminGroups([]) = %#v err=%v, want empty", byNone, err)
	}

	if err := m.RemoveServerAdminGroup(ctx, "srv_sag", "ugrp_a"); err != nil {
		t.Fatalf("RemoveServerAdminGroup(a): %v", err)
	}
	// Re-remove is a no-op, non-error.
	if err := m.RemoveServerAdminGroup(ctx, "srv_sag", "ugrp_a"); err != nil {
		t.Fatalf("re-RemoveServerAdminGroup(a): %v", err)
	}
	groups, err = m.ServerAdminGroups(ctx, "srv_sag")
	if err != nil || len(groups) != 1 || groups[0] != "ugrp_b" {
		t.Fatalf("ServerAdminGroups after remove = %#v err=%v, want [ugrp_b]", groups, err)
	}

	if err := m.UpdateServerSystemGroup(ctx, "srv_sag", "ugrp_sys"); err != nil {
		t.Fatalf("UpdateServerSystemGroup: %v", err)
	}
	got, err := m.AIServerByID(ctx, "srv_sag")
	if err != nil || got.SystemGroupID != "ugrp_sys" {
		t.Fatalf("AIServerByID SystemGroupID = %q err=%v, want ugrp_sys", got.SystemGroupID, err)
	}
	if err := m.UpdateServerSystemGroup(ctx, "srv_missing", "ugrp_sys"); err != storeerr.ErrNotFound {
		t.Fatalf("UpdateServerSystemGroup(missing) = %v, want ErrNotFound", err)
	}

	// Server-delete cascade: ServerAdminGroups reads empty afterward.
	if err := m.DeleteAIServer(ctx, "srv_sag"); err != nil {
		t.Fatalf("DeleteAIServer: %v", err)
	}
	if got, _ := m.ServerAdminGroups(ctx, "srv_sag"); len(got) != 0 {
		t.Fatalf("ServerAdminGroups after server delete = %#v, want empty", got)
	}
	if got, _ := m.ServersByAdminGroups(ctx, []string{"ugrp_b"}); len(got) != 0 {
		t.Fatalf("ServersByAdminGroups after server delete = %#v, want empty", got)
	}
}

// TestMemoryStoreServiceAdminGroups is the SERVICES analog of
// TestMemoryStoreServerAdminGroups (admin-group permissions Phase C).
func TestMemoryStoreServiceAdminGroups(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	now := time.Now().UTC()
	if err := m.CreateService(ctx, Service{ID: "svc_sag", Name: "SAG", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateService: %v", err)
	}

	// A missing service -> ErrNotFound (mirrors the SQL FK violation).
	if err := m.SetServiceAdminGroup(ctx, "svc_missing", "ugrp_a"); err != storeerr.ErrNotFound {
		t.Fatalf("SetServiceAdminGroup(missing service) = %v, want ErrNotFound", err)
	}

	if err := m.SetServiceAdminGroup(ctx, "svc_sag", "ugrp_a"); err != nil {
		t.Fatalf("SetServiceAdminGroup(a): %v", err)
	}
	// Idempotent re-link: no error, no duplicate.
	if err := m.SetServiceAdminGroup(ctx, "svc_sag", "ugrp_a"); err != nil {
		t.Fatalf("re-SetServiceAdminGroup(a): %v", err)
	}
	if err := m.SetServiceAdminGroup(ctx, "svc_sag", "ugrp_b"); err != nil {
		t.Fatalf("SetServiceAdminGroup(b): %v", err)
	}
	groups, err := m.ServiceAdminGroups(ctx, "svc_sag")
	if err != nil || len(groups) != 2 {
		t.Fatalf("ServiceAdminGroups = %#v err=%v, want 2 entries", groups, err)
	}

	byA, err := m.ServicesByAdminGroups(ctx, []string{"ugrp_a"})
	if err != nil || len(byA) != 1 || byA[0].ID != "svc_sag" {
		t.Fatalf("ServicesByAdminGroups(a) = %#v err=%v", byA, err)
	}
	// Deduped: a service linked to both groups appears once when both are queried.
	byBoth, err := m.ServicesByAdminGroups(ctx, []string{"ugrp_a", "ugrp_b"})
	if err != nil || len(byBoth) != 1 || byBoth[0].ID != "svc_sag" {
		t.Fatalf("ServicesByAdminGroups(a,b) = %#v err=%v, want a single svc_sag", byBoth, err)
	}
	// Empty input -> empty output, no scan.
	byNone, err := m.ServicesByAdminGroups(ctx, []string{})
	if err != nil || len(byNone) != 0 {
		t.Fatalf("ServicesByAdminGroups([]) = %#v err=%v, want empty", byNone, err)
	}

	if err := m.RemoveServiceAdminGroup(ctx, "svc_sag", "ugrp_a"); err != nil {
		t.Fatalf("RemoveServiceAdminGroup(a): %v", err)
	}
	// Re-remove is a no-op, non-error.
	if err := m.RemoveServiceAdminGroup(ctx, "svc_sag", "ugrp_a"); err != nil {
		t.Fatalf("re-RemoveServiceAdminGroup(a): %v", err)
	}
	groups, err = m.ServiceAdminGroups(ctx, "svc_sag")
	if err != nil || len(groups) != 1 || groups[0] != "ugrp_b" {
		t.Fatalf("ServiceAdminGroups after remove = %#v err=%v, want [ugrp_b]", groups, err)
	}

	if err := m.UpdateServiceSystemGroup(ctx, "svc_sag", "ugrp_sys"); err != nil {
		t.Fatalf("UpdateServiceSystemGroup: %v", err)
	}
	got, err := m.ServiceByID(ctx, "svc_sag")
	if err != nil || got.SystemGroupID != "ugrp_sys" {
		t.Fatalf("ServiceByID SystemGroupID = %q err=%v, want ugrp_sys", got.SystemGroupID, err)
	}
	if err := m.UpdateServiceSystemGroup(ctx, "svc_missing", "ugrp_sys"); err != storeerr.ErrNotFound {
		t.Fatalf("UpdateServiceSystemGroup(missing) = %v, want ErrNotFound", err)
	}

	// Service-delete cascade: ServiceAdminGroups reads empty afterward.
	if err := m.DeleteService(ctx, "svc_sag"); err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	if got, _ := m.ServiceAdminGroups(ctx, "svc_sag"); len(got) != 0 {
		t.Fatalf("ServiceAdminGroups after service delete = %#v, want empty", got)
	}
	if got, _ := m.ServicesByAdminGroups(ctx, []string{"ugrp_b"}); len(got) != 0 {
		t.Fatalf("ServicesByAdminGroups after service delete = %#v, want empty", got)
	}
}

// TestMemoryStoreResourceGroups is the RESOURCE-GROUPS analog of
// TestMemoryStoreServerAdminGroups/TestMemoryStoreServiceAdminGroups
// (Resource Groups Phase 1). Unlike those, a resource group carries TWO
// distinct joins — resource_group_admin_groups (management, mirrors
// service_admin_groups) and resource_group_servers (membership, a distinct
// relationship) — so this also exercises the AI-server-delete cascade on
// resourceGroupServers specifically (the reverse-keyed map cascade that
// DeleteAIServer must walk, since resourceGroupServers is keyed
// resourceGroupID -> serverID, not the other way around).
func TestMemoryStoreResourceGroups(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	now := time.Now().UTC()
	if err := m.CreateResourceGroup(ctx, ResourceGroup{ID: "rgrp_m", Name: "RG M", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateResourceGroup: %v", err)
	}
	if err := m.CreateAIServer(ctx, AIServer{ID: "srv_rgm_1", Name: "RGM1", Domain: "rgm1", Status: ServerStatusActive, HealthStatus: HealthUnknown, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer(1): %v", err)
	}
	if err := m.CreateAIServer(ctx, AIServer{ID: "srv_rgm_2", Name: "RGM2", Domain: "rgm2", Status: ServerStatusActive, HealthStatus: HealthUnknown, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer(2): %v", err)
	}

	// Duplicate id -> ErrConflict.
	if err := m.CreateResourceGroup(ctx, ResourceGroup{ID: "rgrp_m", Name: "dup", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != storeerr.ErrConflict {
		t.Fatalf("CreateResourceGroup(dup) = %v, want ErrConflict", err)
	}

	// --- admin-group linkage (management) ---

	// A missing resource group -> ErrNotFound (mirrors the SQL FK violation).
	if err := m.SetResourceGroupAdminGroup(ctx, "rgrp_missing", "ugrp_a"); err != storeerr.ErrNotFound {
		t.Fatalf("SetResourceGroupAdminGroup(missing rg) = %v, want ErrNotFound", err)
	}

	if err := m.SetResourceGroupAdminGroup(ctx, "rgrp_m", "ugrp_a"); err != nil {
		t.Fatalf("SetResourceGroupAdminGroup(a): %v", err)
	}
	// Idempotent re-link: no error, no duplicate.
	if err := m.SetResourceGroupAdminGroup(ctx, "rgrp_m", "ugrp_a"); err != nil {
		t.Fatalf("re-SetResourceGroupAdminGroup(a): %v", err)
	}
	if err := m.SetResourceGroupAdminGroup(ctx, "rgrp_m", "ugrp_b"); err != nil {
		t.Fatalf("SetResourceGroupAdminGroup(b): %v", err)
	}
	groups, err := m.ResourceGroupAdminGroups(ctx, "rgrp_m")
	if err != nil || len(groups) != 2 {
		t.Fatalf("ResourceGroupAdminGroups = %#v err=%v, want 2 entries", groups, err)
	}

	byA, err := m.ResourceGroupsByAdminGroups(ctx, []string{"ugrp_a"})
	if err != nil || len(byA) != 1 || byA[0].ID != "rgrp_m" {
		t.Fatalf("ResourceGroupsByAdminGroups(a) = %#v err=%v", byA, err)
	}
	// Deduped: a resource group linked to both groups appears once when both
	// are queried.
	byBoth, err := m.ResourceGroupsByAdminGroups(ctx, []string{"ugrp_a", "ugrp_b"})
	if err != nil || len(byBoth) != 1 || byBoth[0].ID != "rgrp_m" {
		t.Fatalf("ResourceGroupsByAdminGroups(a,b) = %#v err=%v, want a single rgrp_m", byBoth, err)
	}
	// Empty input -> empty output, no scan.
	byNone, err := m.ResourceGroupsByAdminGroups(ctx, []string{})
	if err != nil || len(byNone) != 0 {
		t.Fatalf("ResourceGroupsByAdminGroups([]) = %#v err=%v, want empty", byNone, err)
	}

	if err := m.RemoveResourceGroupAdminGroup(ctx, "rgrp_m", "ugrp_a"); err != nil {
		t.Fatalf("RemoveResourceGroupAdminGroup(a): %v", err)
	}
	// Re-remove is a no-op, non-error.
	if err := m.RemoveResourceGroupAdminGroup(ctx, "rgrp_m", "ugrp_a"); err != nil {
		t.Fatalf("re-RemoveResourceGroupAdminGroup(a): %v", err)
	}
	groups, err = m.ResourceGroupAdminGroups(ctx, "rgrp_m")
	if err != nil || len(groups) != 1 || groups[0] != "ugrp_b" {
		t.Fatalf("ResourceGroupAdminGroups after remove = %#v err=%v, want [ugrp_b]", groups, err)
	}

	if err := m.UpdateResourceGroupSystemGroup(ctx, "rgrp_m", "ugrp_sys"); err != nil {
		t.Fatalf("UpdateResourceGroupSystemGroup: %v", err)
	}
	got, err := m.ResourceGroupByID(ctx, "rgrp_m")
	if err != nil || got.SystemGroupID != "ugrp_sys" {
		t.Fatalf("ResourceGroupByID SystemGroupID = %q err=%v, want ugrp_sys", got.SystemGroupID, err)
	}
	if err := m.UpdateResourceGroupSystemGroup(ctx, "rgrp_missing", "ugrp_sys"); err != storeerr.ErrNotFound {
		t.Fatalf("UpdateResourceGroupSystemGroup(missing) = %v, want ErrNotFound", err)
	}

	// UpdateResourceGroup never touches created_at or system_group_id.
	if err := m.UpdateResourceGroup(ctx, ResourceGroup{ID: "rgrp_m", Name: "RG M renamed", Status: ServerStatusDisabled, CreatedAt: now.Add(time.Hour), UpdatedAt: now}); err != nil {
		t.Fatalf("UpdateResourceGroup: %v", err)
	}
	got, err = m.ResourceGroupByID(ctx, "rgrp_m")
	if err != nil || got.Name != "RG M renamed" || got.Status != ServerStatusDisabled || !got.CreatedAt.Equal(now) || got.SystemGroupID != "ugrp_sys" {
		t.Fatalf("ResourceGroupByID after update = %#v err=%v, want renamed/disabled/created_at+system_group_id unchanged", got, err)
	}
	if err := m.UpdateResourceGroup(ctx, ResourceGroup{ID: "rgrp_missing", Name: "x"}); err != storeerr.ErrNotFound {
		t.Fatalf("UpdateResourceGroup(missing) = %v, want ErrNotFound", err)
	}

	// --- server-membership linkage (distinct from the admin-group join) ---

	// A missing resource group / missing server -> ErrNotFound (mirrors the
	// SQL FK violation on either column).
	if err := m.SetResourceGroupServer(ctx, "rgrp_missing", "srv_rgm_1"); err != storeerr.ErrNotFound {
		t.Fatalf("SetResourceGroupServer(missing rg) = %v, want ErrNotFound", err)
	}
	if err := m.SetResourceGroupServer(ctx, "rgrp_m", "srv_rgm_missing"); err != storeerr.ErrNotFound {
		t.Fatalf("SetResourceGroupServer(missing server) = %v, want ErrNotFound", err)
	}

	if err := m.SetResourceGroupServer(ctx, "rgrp_m", "srv_rgm_1"); err != nil {
		t.Fatalf("SetResourceGroupServer(1): %v", err)
	}
	// Idempotent re-link: no error, no duplicate.
	if err := m.SetResourceGroupServer(ctx, "rgrp_m", "srv_rgm_1"); err != nil {
		t.Fatalf("re-SetResourceGroupServer(1): %v", err)
	}
	if err := m.SetResourceGroupServer(ctx, "rgrp_m", "srv_rgm_2"); err != nil {
		t.Fatalf("SetResourceGroupServer(2): %v", err)
	}
	serverIDs, err := m.ResourceGroupServers(ctx, "rgrp_m")
	if err != nil || len(serverIDs) != 2 {
		t.Fatalf("ResourceGroupServers = %#v err=%v, want 2 entries", serverIDs, err)
	}
	byServer1, err := m.ResourceGroupsByServer(ctx, "srv_rgm_1")
	if err != nil || len(byServer1) != 1 || byServer1[0].ID != "rgrp_m" {
		t.Fatalf("ResourceGroupsByServer(1) = %#v err=%v", byServer1, err)
	}

	if err := m.RemoveResourceGroupServer(ctx, "rgrp_m", "srv_rgm_2"); err != nil {
		t.Fatalf("RemoveResourceGroupServer(2): %v", err)
	}
	// Re-remove is a no-op, non-error.
	if err := m.RemoveResourceGroupServer(ctx, "rgrp_m", "srv_rgm_2"); err != nil {
		t.Fatalf("re-RemoveResourceGroupServer(2): %v", err)
	}
	serverIDs, err = m.ResourceGroupServers(ctx, "rgrp_m")
	if err != nil || len(serverIDs) != 1 || serverIDs[0] != "srv_rgm_1" {
		t.Fatalf("ResourceGroupServers after remove = %#v err=%v, want [srv_rgm_1]", serverIDs, err)
	}

	// AI-server-delete cascade: only ITS resource_group_servers row is
	// dropped (the admin-group linkage is untouched, proving the two joins
	// are independent).
	if err := m.DeleteAIServer(ctx, "srv_rgm_1"); err != nil {
		t.Fatalf("DeleteAIServer: %v", err)
	}
	if got, _ := m.ResourceGroupServers(ctx, "rgrp_m"); len(got) != 0 {
		t.Fatalf("ResourceGroupServers after server delete = %#v, want empty", got)
	}
	if got, _ := m.ResourceGroupsByServer(ctx, "srv_rgm_1"); len(got) != 0 {
		t.Fatalf("ResourceGroupsByServer after server delete = %#v, want empty", got)
	}
	if got, _ := m.ResourceGroupAdminGroups(ctx, "rgrp_m"); len(got) != 1 || got[0] != "ugrp_b" {
		t.Fatalf("ResourceGroupAdminGroups after server delete = %#v, want unchanged [ugrp_b]", got)
	}

	// Re-link a server so the resource-group-delete cascade below exercises
	// BOTH joins (admin-group ugrp_b + server srv_rgm_2 present).
	if err := m.SetResourceGroupServer(ctx, "rgrp_m", "srv_rgm_2"); err != nil {
		t.Fatalf("re-link server 2: %v", err)
	}

	// Resource-group-delete cascade: BOTH ResourceGroupAdminGroups AND
	// ResourceGroupServers read empty afterward.
	if err := m.DeleteResourceGroup(ctx, "rgrp_m"); err != nil {
		t.Fatalf("DeleteResourceGroup: %v", err)
	}
	if got, _ := m.ResourceGroupAdminGroups(ctx, "rgrp_m"); len(got) != 0 {
		t.Fatalf("ResourceGroupAdminGroups after resource group delete = %#v, want empty", got)
	}
	if got, _ := m.ResourceGroupServers(ctx, "rgrp_m"); len(got) != 0 {
		t.Fatalf("ResourceGroupServers after resource group delete = %#v, want empty", got)
	}
	if got, _ := m.ResourceGroupsByAdminGroups(ctx, []string{"ugrp_b"}); len(got) != 0 {
		t.Fatalf("ResourceGroupsByAdminGroups after resource group delete = %#v, want empty", got)
	}
	if _, err := m.ResourceGroupByID(ctx, "rgrp_m"); err != storeerr.ErrNotFound {
		t.Fatalf("ResourceGroupByID after delete = %v, want ErrNotFound", err)
	}
	if err := m.DeleteResourceGroup(ctx, "rgrp_m"); err != storeerr.ErrNotFound {
		t.Fatalf("DeleteResourceGroup(missing) = %v, want ErrNotFound", err)
	}
}

// TestMemoryStoreCertificateByServerDeterministic pins review finding F1.4 on
// the memory store: there is no unique constraint on server_id, so two rows
// can end up linked to the same server, and the pick must be the SAME row --
// the lowest domain -- on every call, not whatever a map iteration happens to
// return first. (The SQL side of this is pinned in
// internal/store/conformance_test.go's TestConformanceCertificates, which
// runs against both sqlite and postgres.)
func TestMemoryStoreCertificateByServerDeterministic(t *testing.T) {
	m := NewMemoryStore()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := m.CreateAIServer(ctx, AIServer{ID: "srv_1", Name: "srv", Domain: "srv.example.test", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := m.UpsertCertificate(ctx, Certificate{Domain: "z2.example.test", Kind: "server", ServerID: "srv_1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertCertificate z2: %v", err)
	}
	if err := m.UpsertCertificate(ctx, Certificate{Domain: "a1.example.test", Kind: "server", ServerID: "srv_1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertCertificate a1: %v", err)
	}
	for i := 0; i < 5; i++ {
		got, err := m.CertificateByServer(ctx, "srv_1")
		if err != nil {
			t.Fatalf("CertificateByServer call %d: %v", i, err)
		}
		if got.Domain != "a1.example.test" {
			t.Fatalf("CertificateByServer call %d = %q, want the lowest domain a1.example.test (nondeterministic pick)", i, got.Domain)
		}
	}
}

// TestMemoryStoreUpsertCertificateRejectsUnknownServer pins review finding
// F1.5: the SQL store's UpsertCertificate maps a foreign-key violation on
// server_id to storeerr.ErrNotFound; without the mirroring check the memory
// store silently accepted a certificate row for a server that does not
// exist -- exactly the class of bug that passes every memory-backed test and
// only fails against a real database.
func TestMemoryStoreUpsertCertificateRejectsUnknownServer(t *testing.T) {
	m := NewMemoryStore()
	ctx := context.Background()
	now := time.Now().UTC()

	if err := m.UpsertCertificate(ctx, Certificate{
		Domain: "orphan.example.test", Kind: "server", ServerID: "no-such-server", CreatedAt: now, UpdatedAt: now,
	}); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("UpsertCertificate with an unknown server_id = %v, want storeerr.ErrNotFound", err)
	}
	if _, err := m.CertificateByDomain(ctx, "orphan.example.test"); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("the rejected row must not have been stored, CertificateByDomain = %v", err)
	}

	// A certificate with no server (the gateway's own, or a "public" domain)
	// is unaffected -- ServerID=="" never gates on server existence.
	if err := m.UpsertCertificate(ctx, Certificate{
		Domain: "gw.example.test", Kind: "gateway", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertCertificate with no ServerID must not be rejected: %v", err)
	}

	// A KNOWN server is accepted.
	if err := m.CreateAIServer(ctx, AIServer{ID: "srv_1", Name: "srv", Domain: "srv.example.test", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := m.UpsertCertificate(ctx, Certificate{
		Domain: "srv.example.test", Kind: "server", ServerID: "srv_1", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertCertificate with a known server_id must succeed: %v", err)
	}
}
