// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stopBody is POST /stop/{model}'s success body as the wire carries it,
// declared here rather than borrowed from router.go so these tests pin the
// wire shape itself.
type stopBody struct {
	State string `json:"state"`
}

// stopStubManager is a managerPort fake that also stops: it records the model
// Stop was asked for and answers with state and err. Every test using it drives
// ServeHTTP synchronously on the test goroutine, so the fields need no locking.
type stopStubManager struct {
	statusManager
	state  StopState
	err    error
	models []string
}

func (m *stopStubManager) Stop(_ context.Context, model string) (StopState, error) {
	m.models = append(m.models, model)
	return m.state, m.err
}

// TestRouterStopAnswers maps every answer of Manager.Stop to its HTTP status
// and body: 200 with the state, or the error envelope of the refusal. The
// instance address travels percent-encoded ("#" would start a fragment), and
// the route reads the decoded path.
func TestRouterStopAnswers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      StopState
		err        error
		wantStatus int
		wantState  string
		wantCode   string
	}{
		{name: "stopping", state: StopStopping, wantStatus: http.StatusOK, wantState: "stopping"},
		{name: "stopped", state: StopStopped, wantStatus: http.StatusOK, wantState: "stopped"},
		{name: "not managed", err: ErrModelNotManaged, wantStatus: http.StatusNotFound, wantCode: "runtime.model_not_managed"},
		{name: "protected", err: ErrInstanceProtected, wantStatus: http.StatusConflict, wantCode: "runtime.instance_protected"},
		{name: "not highest", err: ErrInstanceNotHighest, wantStatus: http.StatusConflict, wantCode: "runtime.instance_not_highest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &stopStubManager{state: tc.state, err: tc.err}
			rec := httptest.NewRecorder()
			newRouter(m).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/stop/m%232", nil))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if len(m.models) != 1 || m.models[0] != "m#2" {
				t.Fatalf("Stop was asked for %q, want exactly [\"m#2\"]: the path names the instance address", m.models)
			}
			if tc.wantCode != "" {
				if code := decodeErrorCode(t, rec); code != tc.wantCode {
					t.Fatalf("error code = %q, want %q", code, tc.wantCode)
				}
				return
			}
			if got := decodeJSON[stopBody](t, rec.Body); got.State != tc.wantState {
				t.Fatalf("body = %+v, want state %q", got, tc.wantState)
			}
		})
	}
}

// TestRouterStopNotManagedWithoutAManager: a nil manager, an empty model and a
// manager that cannot stop all answer 404 runtime.model_not_managed, and an
// empty model never reaches the manager.
func TestRouterStopNotManagedWithoutAManager(t *testing.T) {
	for _, tc := range []struct {
		name string
		rt   *router
		path string
	}{
		{name: "nil manager", rt: newRouter(nil), path: "/stop/m"},
		{name: "empty model", rt: newRouter(&stopStubManager{state: StopStopping}), path: "/stop/"},
		{name: "manager without Stop", rt: newRouter(&statusManager{}), path: "/stop/m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.rt.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != "runtime.model_not_managed" {
				t.Fatalf("code = %q, want runtime.model_not_managed", code)
			}
			if m, ok := tc.rt.m.(*stopStubManager); ok && len(m.models) != 0 {
				t.Fatalf("Stop asked for %q, want no call for an empty model", m.models)
			}
		})
	}
}

// TestRouterStopBody: any body is read and discarded (the path names the
// instance), and one over the bound is refused with 413 before Stop is asked.
func TestRouterStopBody(t *testing.T) {
	t.Run("within the bound", func(t *testing.T) {
		m := &stopStubManager{state: StopStopping}
		rec := httptest.NewRecorder()
		newRouter(m).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/stop/m", strings.NewReader(`{"model":"other"}`)))
		if rec.Code != http.StatusOK || len(m.models) != 1 || m.models[0] != "m" {
			t.Fatalf("status = %d, Stop asked for %q; want 200 and exactly [\"m\"]", rec.Code, m.models)
		}
	})
	t.Run("over the bound", func(t *testing.T) {
		m := &stopStubManager{state: StopStopping}
		rec := httptest.NewRecorder()
		big := strings.Repeat("a", maxBodyBytes+1)
		newRouter(m).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/stop/m", strings.NewReader(big)))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
		}
		if len(m.models) != 0 {
			t.Fatalf("Stop asked for %q, want no call for a refused body", m.models)
		}
	})
}

// TestRouterStopOtherMethodsFallThrough: the route answers POST only; any other
// method on the path lands in the model-routed proxy, which stops nothing.
func TestRouterStopOtherMethodsFallThrough(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			m := &stopStubManager{state: StopStopping}
			rec := httptest.NewRecorder()
			newRouter(m).ServeHTTP(rec, httptest.NewRequest(method, "/stop/m", nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 from the proxy; body=%s", rec.Code, rec.Body.String())
			}
			if len(m.models) != 0 {
				t.Fatalf("Stop asked for %q, want none: only POST stops through /stop/", m.models)
			}
		})
	}
}

// TestRouterStopEndToEnd drives the route against a real Manager and child:
// the highest instance drains and is retired, a repeat answers stopped, a
// protected instance and a lower one are refused with their 409 codes, and
// an unknown address is 404.
func TestRouterStopEndToEnd(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	spec := replicableSpec("S", "org/m")
	spec.MinInstances = 1
	m.Apply(Config{Specs: []Spec{spec}})
	for _, model := range []string{"org/m", "org/m#1", "org/m#2"} {
		_, release := ensureModel(t, m, model)
		release()
	}
	srv := httptest.NewServer(NewRouter(m))
	defer srv.Close()

	post := func(model string) (int, string) {
		t.Helper()
		resp, err := http.Post(srv.URL+"/stop/"+model, "", nil)
		if err != nil {
			t.Fatalf("POST /stop/%s: %v", model, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return resp.StatusCode, decodeJSON[stopBody](t, resp.Body).State
		}
		return resp.StatusCode, decodeJSON[errorEnvelope](t, resp.Body).Error.Code
	}

	for _, step := range []struct {
		model      string
		wantStatus int
		want       string
	}{
		{"org/m%231", http.StatusConflict, "runtime.instance_not_highest"},
		{"org/m", http.StatusConflict, "runtime.instance_protected"},
		{"ghost", http.StatusNotFound, "runtime.model_not_managed"},
		{"org/m%232", http.StatusOK, "stopping"},
	} {
		if status, got := post(step.model); status != step.wantStatus || got != step.want {
			t.Fatalf("POST /stop/%s = %d %q, want %d %q", step.model, status, got, step.wantStatus, step.want)
		}
	}
	waitUntil(t, 5*time.Second, "instance 2 retired after the stop", func() bool {
		return instanceStatus(m, "S", 2) == nil
	})
	if status, got := post("org/m%232"); status != http.StatusOK || got != "stopped" {
		t.Fatalf("repeated POST /stop/org/m#2 = %d %q, want 200 stopped", status, got)
	}
	if status, got := post("org/m%231"); status != http.StatusOK || got != "stopping" {
		t.Fatalf("POST /stop/org/m#1 once it is the highest = %d %q, want 200 stopping", status, got)
	}
}
