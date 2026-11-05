// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// rpcStateBody is the success body of every /rpc/* route as the wire carries
// it, declared here so these tests pin the wire shape itself.
type rpcStateBody struct {
	State string `json:"state"`
}

// onDemandMember is a TCP-listening stub member on a free loopback port that
// nothing starts on its own: no admin state, so only an ensure brings it up.
func onDemandMember(t *testing.T, id string) Spec {
	t.Helper()
	spec := memberSpec(id, "rspec_h", "-tcp-listen")
	spec.RPC.BindIP = "127.0.0.1"
	spec.RPC.Port = freeTCPPort(t)
	return spec
}

func postRPC(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
	return rec
}

func wantRPCState(t *testing.T, rec *httptest.ResponseRecorder, state string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeJSON[rpcStateBody](t, rec.Body); got.State != state {
		t.Fatalf("body = %+v, want state %q", got, state)
	}
}

func wantRPCError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, status, rec.Body.String())
	}
	if got := decodeErrorCode(t, rec); got != code {
		t.Fatalf("error code = %q, want %q", got, code)
	}
}

// memberRPC returns the member's rpc status block, failing when the row or the
// block is missing.
func memberRPC(t *testing.T, m *Manager, id string) RPCStatus {
	t.Helper()
	s := statusFor(m, id)
	if s == nil || s.RPC == nil {
		t.Fatalf("status of %s = %+v, want a row with an rpc block", id, s)
	}
	return *s.RPC
}

// TestRPCEnsureStartsMemberAndReserves: the ensure answers only once the
// member accepts TCP connections, and arms a reservation that ends N seconds
// from the answer.
func TestRPCEnsureStartsMemberAndReserves(t *testing.T) {
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{onDemandMember(t, "rpcm_a")}})

	before := time.Now()
	rec := postRPC(newRouter(m), "/rpc/ensure/rpcm_a?reserve_seconds=30")
	after := time.Now()
	wantRPCState(t, rec, "running")

	s := statusFor(m, "rpcm_a")
	if s.State != StateRunning {
		t.Fatalf("state = %s, want running once the ensure answered", s.State)
	}
	if !probeTCP(net.JoinHostPort("127.0.0.1", strconv.Itoa(s.Port)), time.Second) {
		t.Fatal("the member does not accept TCP connections although the ensure answered 200")
	}
	until := memberRPC(t, m, "rpcm_a").ReservedUntil
	if until.Before(before.Add(30*time.Second)) || until.After(after.Add(30*time.Second)) {
		t.Fatalf("reserved_until = %v, want now+30s (between %v and %v)", until, before.Add(30*time.Second), after.Add(30*time.Second))
	}
}

// TestRPCEnsureUnknownSpec404: an id no spec has, an empty id and a router
// without a manager all answer 404 runtime.model_not_managed.
func TestRPCEnsureUnknownSpec404(t *testing.T) {
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{onDemandMember(t, "rpcm_a")}})
	for _, path := range []string{"/rpc/ensure/rpcm_nope", "/rpc/ensure/", "/rpc/ensure/rpcm_a%231", "/rpc/unknown/rpcm_a"} {
		wantRPCError(t, postRPC(newRouter(m), path), http.StatusNotFound, codeModelNotManaged)
	}
	wantRPCError(t, postRPC(newRouter(nil), "/rpc/ensure/rpcm_a"), http.StatusNotFound, codeModelNotManaged)
	if s := statusFor(m, "rpcm_a"); s.State != StateStopped {
		t.Fatalf("member state = %s, want stopped: no refused route may start it", s.State)
	}
}

// TestRPCRoutesRejectModelSpecs: a model spec is never reached through the
// member routes, by spec id or by model name, and a member is never reached
// through the model ensure.
func TestRPCRoutesRejectModelSpecs(t *testing.T) {
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{baseSpec("spec-a", "model-a"), onDemandMember(t, "rpcm_a")}})
	rt := newRouter(m)
	for _, action := range []string{"ensure", "release", "stop", "purge-cache"} {
		for _, id := range []string{"spec-a", "model-a"} {
			wantRPCError(t, postRPC(rt, "/rpc/"+action+"/"+id), http.StatusNotFound, codeModelNotManaged)
		}
	}
	wantRPCError(t, postRPC(rt, "/ensure/rpcm_a"), http.StatusNotFound, codeModelNotManaged)
	for _, id := range []string{"spec-a", "rpcm_a"} {
		if s := statusFor(m, id); s.State != StateStopped {
			t.Fatalf("%s state = %s, want stopped", id, s.State)
		}
	}
}

// TestRPCEnsureRefusals: a force-stopped member is refused with 503
// admission_blocked, and an unusable reservation with 400.
func TestRPCEnsureRefusals(t *testing.T) {
	stopped := onDemandMember(t, "rpcm_s")
	stopped.AdminState = "force_stopped"
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{stopped, onDemandMember(t, "rpcm_a")}})
	rt := newRouter(m)

	wantRPCError(t, postRPC(rt, "/rpc/ensure/rpcm_s?reserve_seconds=5"), http.StatusServiceUnavailable, "runtime.admission_blocked")
	for _, q := range []string{"-1", "abc", "1.5", "86401", "9223372036854775807"} {
		wantRPCError(t, postRPC(rt, "/rpc/ensure/rpcm_a?reserve_seconds="+q), http.StatusBadRequest, "runtime.bad_request")
	}
	if s := statusFor(m, "rpcm_a"); s.State != StateStopped {
		t.Fatalf("member state = %s, want stopped after refused ensures", s.State)
	}
}

// TestRPCEnsureTerminalVerdictIsNotPermitted: a member whose start earned a
// terminal verdict answers 502 not_permitted, the first time and every time
// after, without starting again.
func TestRPCEnsureTerminalVerdictIsNotPermitted(t *testing.T) {
	shrinkTimings(t)
	spec := memberSpec("rpcm_a", "rspec_h", "-tcp-listen", "-print", memberBannerPrint(1))
	spec.RPC.BindIP = "127.0.0.1"
	spec.RPC.Port = freeTCPPort(t)
	spec.GPUs = []SpecGPU{{Index: 0}, {Index: 1}}
	spec.DeviceBackend = "cuda"
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})
	rt := newRouter(m)

	wantRPCError(t, postRPC(rt, "/rpc/ensure/rpcm_a?reserve_seconds=5"), http.StatusBadGateway, "runtime.not_permitted")
	wantRPCError(t, postRPC(rt, "/rpc/ensure/rpcm_a?reserve_seconds=5"), http.StatusBadGateway, "runtime.not_permitted")
	if r := memberRPC(t, m, "rpcm_a"); !r.ReservedUntil.IsZero() {
		t.Fatalf("reserved_until = %v, want none for a member that never ran", r.ReservedUntil)
	}
}

// TestRPCStopRefusedWhileHeld: a held member, and a merely reserved one, refuse
// a stop with 409 cluster_member_in_use and keep running.
func TestRPCStopRefusedWhileHeld(t *testing.T) {
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{onDemandMember(t, "rpcm_h"), onDemandMember(t, "rpcm_r")}})
	rt := newRouter(m)

	wantRPCState(t, postRPC(rt, "/rpc/ensure/rpcm_h"), "running")
	m.SetMemberHold("rpcm_h", "rspec_h", time.Now())
	wantRPCState(t, postRPC(rt, "/rpc/ensure/rpcm_r?reserve_seconds=30"), "running")

	for _, id := range []string{"rpcm_h", "rpcm_r"} {
		wantRPCError(t, postRPC(rt, "/rpc/stop/"+id), http.StatusConflict, "runtime.cluster_member_in_use")
		wantRPCError(t, postRPC(rt, "/rpc/stop/"+id+"?force=0"), http.StatusConflict, "runtime.cluster_member_in_use")
	}
	time.Sleep(100 * time.Millisecond)
	for _, id := range []string{"rpcm_h", "rpcm_r"} {
		if s := statusFor(m, id); s.State != StateRunning {
			t.Fatalf("%s state = %s, want running after a refused stop", id, s.State)
		}
	}
	if r := memberRPC(t, m, "rpcm_h"); r.HeldBy != "rspec_h" || r.HeldSince.IsZero() {
		t.Fatalf("rpc = %+v, want held by rspec_h with a since", r)
	}
}

// TestRPCStopForce: force=1 stops a held member, and the hold leaves with the
// process.
func TestRPCStopForce(t *testing.T) {
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{onDemandMember(t, "rpcm_a")}})
	rt := newRouter(m)

	wantRPCState(t, postRPC(rt, "/rpc/ensure/rpcm_a?reserve_seconds=30"), "running")
	m.SetMemberHold("rpcm_a", "rspec_h", time.Now())
	wantRPCState(t, postRPC(rt, "/rpc/stop/rpcm_a?force=1"), "stopping")
	waitUntil(t, 5*time.Second, "member stopped", func() bool {
		return statusFor(m, "rpcm_a").State == StateStopped
	})
	if r := memberRPC(t, m, "rpcm_a"); r.HeldBy != "" || !r.HeldSince.IsZero() || !r.ReservedUntil.IsZero() {
		t.Fatalf("rpc = %+v, want neither hold nor reservation once the member stopped", r)
	}
	wantRPCState(t, postRPC(rt, "/rpc/stop/rpcm_a"), "stopped")
}

// TestRPCReleaseEndsReservationAndHold: release clears both, after which a
// plain stop succeeds; releasing a member with neither is a 200 as well.
func TestRPCReleaseEndsReservationAndHold(t *testing.T) {
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{onDemandMember(t, "rpcm_a")}})
	rt := newRouter(m)

	wantRPCState(t, postRPC(rt, "/rpc/release/rpcm_a"), "released")
	wantRPCState(t, postRPC(rt, "/rpc/ensure/rpcm_a?reserve_seconds=30"), "running")
	m.SetMemberHold("rpcm_a", "rspec_h", time.Now())
	wantRPCState(t, postRPC(rt, "/rpc/release/rpcm_a"), "released")
	if r := memberRPC(t, m, "rpcm_a"); r.HeldBy != "" || !r.ReservedUntil.IsZero() {
		t.Fatalf("rpc = %+v, want neither hold nor reservation after release", r)
	}
	if s := statusFor(m, "rpcm_a"); s.State != StateRunning {
		t.Fatalf("state = %s, want running: release ends the claim, not the process", s.State)
	}
	wantRPCState(t, postRPC(rt, "/rpc/stop/rpcm_a"), "stopping")
}

// cacheMember is an on-demand member with the tensor cache on, under a test
// manager whose config_files_dir is root, with the free-disk floor satisfied.
func cacheMember(t *testing.T, root string) (*Manager, string) {
	t.Helper()
	origFree := freeDiskMB
	freeDiskMB = func(string) (int, error) { return 1 << 20, nil }
	t.Cleanup(func() { freeDiskMB = origFree })
	spec := onDemandMember(t, "rpcm_a")
	spec.RPC.CacheEnabled = true
	m := cacheTestManager(t, root)
	m.Apply(Config{Specs: []Spec{spec}})
	return m, filepath.Join(root, "rpc", "rspec_h")
}

// TestPurgeCacheRefusedWhileHeld: the cache of a held or reserved member is
// kept (409); once released it is purged.
func TestPurgeCacheRefusedWhileHeld(t *testing.T) {
	shrinkTimings(t)
	m, dir := cacheMember(t, t.TempDir())
	rt := newRouter(m)

	wantRPCState(t, postRPC(rt, "/rpc/ensure/rpcm_a?reserve_seconds=30"), "running")
	if err := os.WriteFile(filepath.Join(dir, "blob"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantRPCError(t, postRPC(rt, "/rpc/purge-cache/rpcm_a"), http.StatusConflict, "runtime.cluster_member_in_use")
	wantRPCState(t, postRPC(rt, "/rpc/release/rpcm_a"), "released")
	m.SetMemberHold("rpcm_a", "rspec_h", time.Now())
	wantRPCError(t, postRPC(rt, "/rpc/purge-cache/rpcm_a"), http.StatusConflict, "runtime.cluster_member_in_use")
	if _, err := os.Stat(filepath.Join(dir, "blob")); err != nil {
		t.Fatalf("cache touched although the member is held: %v", err)
	}

	m.SetMemberHold("rpcm_a", "", time.Time{})
	wantRPCState(t, postRPC(rt, "/rpc/purge-cache/rpcm_a"), "purged")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("cache dir still there after purge: %v", err)
	}
}

// TestPurgeCacheRemovesHeadCache: a member that is not running has the cache
// of its head purged on demand; purging again (nothing there) is a 200 too.
func TestPurgeCacheRemovesHeadCache(t *testing.T) {
	m, dir := cacheMember(t, t.TempDir())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blob"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := newRouter(m)
	wantRPCState(t, postRPC(rt, "/rpc/purge-cache/rpcm_a"), "purged")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("cache dir still there after purge: %v", err)
	}
	wantRPCState(t, postRPC(rt, "/rpc/purge-cache/rpcm_a"), "purged")
	wantRPCError(t, postRPC(rt, "/rpc/purge-cache/rpcm_nope"), http.StatusNotFound, codeModelNotManaged)
}

// TestPurgeCacheFailureIs500: a cache the filesystem refuses to move is
// reported as 500 runtime.cache_purge_failed, not as an upstream failure.
func TestPurgeCacheFailureIs500(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	root := t.TempDir()
	m, dir := cacheMember(t, root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rpcDir := filepath.Dir(dir)
	if err := os.Chmod(rpcDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(rpcDir, 0o700) })

	wantRPCError(t, postRPC(newRouter(m), "/rpc/purge-cache/rpcm_a"), http.StatusInternalServerError, "runtime.cache_purge_failed")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("cache dir gone although the purge failed: %v", err)
	}
}
