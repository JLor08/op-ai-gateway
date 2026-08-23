// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

const (
	perfOwnerSecret = "owner-secret"
	perfOtherSecret = "other-secret"
	perfServerID    = "srv_perf"
)

// newPerfTestServer builds a *Server with a memory route store holding one server
// (srv_perf) owned by usr_a, plain bearer tokens for the owner (usr_a) and a
// non-owner (usr_b), and a live ServerPerf registry. The portal service uses the
// default (real) clock so a seeded now-anchored window falls inside the query.
func newPerfTestServer(t *testing.T) (*Server, *routing.MemoryStore) {
	t.Helper()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_a", Email: "a@example.test", DisplayName: "Owner A", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	dir.AddUser(store.User{ID: "usr_b", Email: "b@example.test", DisplayName: "Other B", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_a", UserID: "usr_a", Name: "Owner Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, perfOwnerSecret); err != nil {
		t.Fatalf("CreatePlainToken owner: %v", err)
	}
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_b", UserID: "usr_b", Name: "Other Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, perfOtherSecret); err != nil {
		t.Fatalf("CreatePlainToken other: %v", err)
	}
	routeStore := routing.NewMemoryStore()
	if err := routeStore.CreateAIServer(context.Background(), routing.AIServer{ID: perfServerID, Name: "Perf Host", Domain: "perf.example.test", Provider: routing.ProviderMock, Endpoint: "mock://perf", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.SetServerOwners(context.Background(), perfServerID, []string{"usr_a"}); err != nil {
		t.Fatalf("SetServerOwners: %v", err)
	}
	recorder := usage.NewRecorder()
	svc := portal.NewService(portal.ServiceDeps{Users: dir, Tokens: dir, Usage: recorder, Routes: routeStore})
	srv := New(ServerDeps{
		Tokens:     tokens,
		Usage:      recorder,
		Routes:     routeStore,
		Portal:     svc,
		ServerPerf: NewServerPerfRegistry(),
	})
	return srv, routeStore
}

func perfErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var parsed struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal error body: %v (%s)", err, string(body))
	}
	return parsed.Error.Code
}

func TestServerPerfHistoryEndpoint(t *testing.T) {
	srv, routeStore := newPerfTestServer(t)
	base := time.Now().UTC().Add(-3 * time.Minute)
	for i := 0; i < 3; i++ {
		if err := routeStore.InsertTelemetrySample(context.Background(), routing.TelemetrySample{
			ServerID:   perfServerID,
			ReportedAt: base.Add(time.Duration(i) * time.Minute),
			CPUUtilPct: float64(10 + i),
			GPUs:       []routing.GPUSample{{Index: 0, Name: "RTX 4090", UUID: "gpu-0", UtilPct: 88, TempC: 71}},
			Net:        []routing.NetSample{{Name: "eth0", RxBytes: 1000, TxBytes: 2000}},
		}); err != nil {
			t.Fatalf("InsertTelemetrySample[%d]: %v", i, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/portal/servers/"+perfServerID+"/perf?window=15m", nil)
	req.Header.Set("Authorization", "Bearer "+perfOwnerSecret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var dto perfHistoryDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if len(dto.Points) != 3 {
		t.Fatalf("points = %d, want 3", len(dto.Points))
	}
	if dto.From == "" || dto.To == "" {
		t.Fatalf("from/to = %q/%q, want both present", dto.From, dto.To)
	}
	if dto.Points[0].CPUUtilPct != 10 {
		t.Fatalf("points[0].cpu_util_pct = %v, want 10", dto.Points[0].CPUUtilPct)
	}
	if len(dto.Points[0].GPUs) != 1 || dto.Points[0].GPUs[0].UUID != "gpu-0" {
		t.Fatalf("points[0].gpus = %#v, want one gpu with uuid gpu-0", dto.Points[0].GPUs)
	}
	if len(dto.Points[0].Net) != 1 || dto.Points[0].Net[0].RxBytes != 1000 {
		t.Fatalf("points[0].net = %#v, want one nic rx=1000", dto.Points[0].Net)
	}
}

// TestPerfPointFromSampleSubSecondPrecision proves the wire `t` field carries
// millisecond resolution, not whole-second RFC3339. The agent's default
// collection cadence is now 1s, so two samples less than a second apart must
// render as distinct `t` strings for a downstream consumer (e.g. the energy
// reconciler integrating power over a request's time window) to tell them
// apart.
func TestPerfPointFromSampleSubSecondPrecision(t *testing.T) {
	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	p1 := perfPointFromSample(routing.TelemetrySample{ReportedAt: base.Add(250 * time.Millisecond)})
	p2 := perfPointFromSample(routing.TelemetrySample{ReportedAt: base.Add(750 * time.Millisecond)})

	if p1.T == p2.T {
		t.Fatalf("sub-second samples rendered the same wire timestamp: %q == %q", p1.T, p2.T)
	}
	if !strings.Contains(p1.T, ".250") {
		t.Fatalf("p1.T = %q, want millisecond fraction .250", p1.T)
	}
	if !strings.Contains(p2.T, ".750") {
		t.Fatalf("p2.T = %q, want millisecond fraction .750", p2.T)
	}
	// Round-trips through the standard parser (what the frontend's `new
	// Date(p.t)` effectively does) preserving the millisecond value.
	parsed, err := time.Parse(time.RFC3339Nano, p1.T)
	if err != nil {
		t.Fatalf("parse p1.T: %v", err)
	}
	if !parsed.Equal(base.Add(250 * time.Millisecond)) {
		t.Fatalf("parsed p1.T = %v, want %v", parsed, base.Add(250*time.Millisecond))
	}
}

func TestServerPerfHistoryEndpointForbidden(t *testing.T) {
	srv, _ := newPerfTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/servers/"+perfServerID+"/perf?window=15m", nil)
	req.Header.Set("Authorization", "Bearer "+perfOtherSecret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found", code)
	}
}

func TestServerPerfEventsSnapshotAndDelta(t *testing.T) {
	srv, _ := newPerfTestServer(t)
	// Pre-seed the ring so the snapshot carries a point.
	srv.ServerPerf.publish(routing.TelemetrySample{ServerID: perfServerID, ReportedAt: time.Now().UTC(), CPUUtilPct: 11})

	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/portal/servers/"+perfServerID+"/perf/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+perfOwnerSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)

	// First frame: snapshot with the pre-seeded ring point.
	event, data := readPerfSSEFrame(t, reader, 3*time.Second)
	if event != "snapshot" {
		t.Fatalf("first event = %q, want snapshot", event)
	}
	var snap perfSnapshotDTO
	if err := json.Unmarshal([]byte(data), &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v (%s)", err, data)
	}
	if len(snap.Points) != 1 || snap.Points[0].CPUUtilPct != 11 {
		t.Fatalf("snapshot points = %#v, want one point cpu=11", snap.Points)
	}

	// A live publish arrives as a `sample` frame.
	srv.ServerPerf.publish(routing.TelemetrySample{ServerID: perfServerID, ReportedAt: time.Now().UTC(), CPUUtilPct: 42})
	event, data = readPerfSSEFrame(t, reader, 3*time.Second)
	if event != "sample" {
		t.Fatalf("delta event = %q, want sample", event)
	}
	var point perfPointDTO
	if err := json.Unmarshal([]byte(data), &point); err != nil {
		t.Fatalf("unmarshal sample: %v (%s)", err, data)
	}
	if point.CPUUtilPct != 42 {
		t.Fatalf("sample cpu_util_pct = %v, want 42", point.CPUUtilPct)
	}
}

func TestServerPerfEventsForbidden(t *testing.T) {
	srv, _ := newPerfTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/servers/"+perfServerID+"/perf/events", nil)
	req.Header.Set("Authorization", "Bearer "+perfOtherSecret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found", code)
	}
}

func TestBenchmarkEventsSnapshotAndProgress(t *testing.T) {
	srv, _ := newPerfTestServer(t)
	// Pre-start a run so the initial snapshot carries a running status.
	run, ok := srv.Benchmarks.TryStart(perfServerID, "server", "speed", 2, time.Now().UTC(), func() {})
	if !ok {
		t.Fatalf("TryStart failed")
	}
	run.addResult(BenchmarkResult{MappingID: "m1", GatewayModelName: "gw-1"})

	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/portal/servers/"+perfServerID+"/benchmark/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+perfOwnerSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)

	// First frame: snapshot reflecting the running status.
	event, data := readPerfSSEFrame(t, reader, 3*time.Second)
	if event != "snapshot" {
		t.Fatalf("first event = %q, want snapshot", event)
	}
	var snap BenchmarkStatus
	if err := json.Unmarshal([]byte(data), &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v (%s)", err, data)
	}
	if !snap.Running || snap.ServerID != perfServerID || snap.Done != 1 {
		t.Fatalf("snapshot = %#v, want running srv=%s Done 1", snap, perfServerID)
	}

	// A live publish arrives as a `progress` frame.
	srv.Benchmarks.publish(perfServerID, BenchmarkStatus{Running: false, ServerID: perfServerID, Total: 2, Done: 2})
	event, data = readPerfSSEFrame(t, reader, 3*time.Second)
	if event != "progress" {
		t.Fatalf("delta event = %q, want progress", event)
	}
	var prog BenchmarkStatus
	if err := json.Unmarshal([]byte(data), &prog); err != nil {
		t.Fatalf("unmarshal progress: %v (%s)", err, data)
	}
	if prog.Running || prog.Done != 2 {
		t.Fatalf("progress = %#v, want terminal Done 2", prog)
	}
}

func TestBenchmarkEventsForbidden(t *testing.T) {
	srv, _ := newPerfTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/servers/"+perfServerID+"/benchmark/events", nil)
	req.Header.Set("Authorization", "Bearer "+perfOtherSecret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found", code)
	}
}

// readPerfSSEFrame reads one SSE frame (event + data), skipping comment/heartbeat
// lines, and fails the test if no frame arrives within timeout. The blocking read
// runs in a goroutine so a stalled stream can't hang the test forever.
func readPerfSSEFrame(t *testing.T, r *bufio.Reader, timeout time.Duration) (string, string) {
	t.Helper()
	type frame struct{ event, data string }
	ch := make(chan frame, 1)
	errCh := make(chan error, 1)
	go func() {
		var event, data string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				errCh <- err
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				if event != "" || data != "" {
					ch <- frame{event, data}
					return
				}
				continue
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			if v, ok := strings.CutPrefix(line, "event:"); ok {
				event = strings.TrimSpace(v)
			} else if v, ok := strings.CutPrefix(line, "data:"); ok {
				data = strings.TrimSpace(v)
			}
		}
	}()
	select {
	case f := <-ch:
		return f.event, f.data
	case err := <-errCh:
		t.Fatalf("read SSE frame: %v", err)
	case <-time.After(timeout):
		t.Fatalf("timed out after %s waiting for an SSE frame", timeout)
	}
	return "", ""
}
