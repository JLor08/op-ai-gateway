// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func postWith(h http.Handler, path, body string, header map[string]string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	h.ServeHTTP(rec, req)
	return rec
}

// TestColdHeadRefusesPlainRequest: every route that could start a cold head
// without the cluster-start marker answers 503 runtime.cluster_not_ready, and
// nothing is exec'd.
func TestColdHeadRefusesPlainRequest(t *testing.T) {
	shrinkTimings(t)
	inv := filepath.Join(t.TempDir(), "inv")
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{clusterHead(inv, fakeMember(t))}})
	h := newRouter(m)

	for _, tc := range []struct {
		name, path, body string
		header           map[string]string
	}{
		{name: "proxied", path: "/v1/echo", body: `{"model":"m"}`},
		{name: "unmarked ensure", path: "/ensure/m"},
		{name: "ensure with another marker value", path: "/ensure/m", header: map[string]string{ClusterStartHeader: "0"}},
		{name: "live", path: "/live/m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postWith(h, tc.path, tc.body, tc.header)
			wantRPCError(t, rec, http.StatusServiceUnavailable, "runtime.cluster_not_ready")
		})
	}
	if n := execCount(t, inv); n != 0 {
		t.Fatalf("head exec'd %d times, want 0", n)
	}
}

// TestClusterStartHeaderStartsHead: the marked ensure probes the members and
// starts the head; once it runs, a plain request is served by it.
func TestClusterStartHeaderStartsHead(t *testing.T) {
	shrinkTimings(t)
	inv := filepath.Join(t.TempDir(), "inv")
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{clusterHead(inv, fakeMember(t), fakeMember(t))}})
	h := newRouter(m)

	rec := postWith(h, "/ensure/m", "", map[string]string{ClusterStartHeader: "1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeJSON[ensureBody](t, rec.Body); got.Status != "running" {
		t.Fatalf("body = %+v, want status running", got)
	}
	rec = postWith(h, "/v1/echo", `{"model":"m"}`, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"model":"m"`) {
		t.Fatalf("proxied request: status = %d body=%s, want the head's echo", rec.Code, rec.Body.String())
	}
	rec = postWith(h, "/ensure/m", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unmarked ensure of a running head: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if n := execCount(t, inv); n != 1 {
		t.Fatalf("head exec'd %d times, want 1", n)
	}
}

// TestClusterStartAnswersUnreachable: a marked ensure whose probe gives up is
// 503 runtime.cluster_member_unreachable, and the head is not exec'd.
func TestClusterStartAnswersUnreachable(t *testing.T) {
	shrinkTimings(t)
	inv := filepath.Join(t.TempDir(), "inv")
	spec := clusterHead(inv, unreachableAddr(t))
	spec.StartupTimeoutSeconds = 1
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})

	rec := postWith(newRouter(m), "/ensure/m", "", map[string]string{ClusterStartHeader: "1"})
	wantRPCError(t, rec, http.StatusServiceUnavailable, "runtime.cluster_member_unreachable")
	if n := execCount(t, inv); n != 0 {
		t.Fatalf("head exec'd %d times, want 0", n)
	}
}

// TestRouterConditionalStop: POST /stop/{model}?if_evictable=1 answers 409
// runtime.instance_busy while the head serves a request, and drains it once
// it is idle.
func TestRouterConditionalStop(t *testing.T) {
	shrinkTimings(t)
	inv := filepath.Join(t.TempDir(), "inv")
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{clusterHead(inv, fakeMember(t))}})
	h := newRouter(m)
	startHead(t, m)

	ctx := t.Context()
	_, release, err := m.EnsureServing(ctx, "m")
	if err != nil {
		t.Fatalf("EnsureServing: %v", err)
	}
	wantRPCError(t, postWith(h, "/stop/m?if_evictable=1", "", nil), http.StatusConflict, "runtime.instance_busy")
	release()
	waitUntil(t, 5*time.Second, "request released", func() bool {
		s := statusFor(m, "rspec_h")
		return s != nil && s.InFlight == 0
	})
	rec := postWith(h, "/stop/m?if_evictable=1", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeJSON[stopBody](t, rec.Body); got.State != "stopping" {
		t.Fatalf("body = %+v, want state stopping", got)
	}
}

// A router whose manager cannot stop conditionally refuses rather than
// stopping unconditionally.
func TestRouterConditionalStopWithoutSupportRefuses(t *testing.T) {
	m := &stopStubManager{state: StopStopping}
	wantRPCError(t, postWith(newRouter(m), "/stop/m?if_evictable=1", "", nil), http.StatusConflict, "runtime.instance_busy")
	if len(m.models) != 0 {
		t.Fatalf("Stop was called for %q, want no unconditional stop", m.models)
	}
}
