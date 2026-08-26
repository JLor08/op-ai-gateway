// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-server-agent/internal/sample"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testSample() *sample.Sample {
	return &sample.Sample{
		AgentVersion: "0.1.0",
		OS:           "linux",
		Arch:         "amd64",
		Host: &sample.Host{
			CPUUtilPct:    42.5,
			MemUsedBytes:  1000,
			MemTotalBytes: 2000,
		},
		GPUs: []sample.GPU{{Index: 0, Name: "TestGPU", UtilPct: 88}},
	}
}

// wireSample mirrors the gateway's decode tags to prove the JSON round-trips.
type wireSample struct {
	AgentVersion   string          `json:"agent_version"`
	ProviderHealth json.RawMessage `json:"provider_health"`
	Capabilities   json.RawMessage `json:"capabilities"`
	Host           *struct {
		CPUUtilPct float64 `json:"cpu_util_pct"`
	} `json:"host"`
	GPUs []struct {
		Name    string  `json:"name"`
		UtilPct float64 `json:"util_pct"`
	} `json:"gpus"`
	// Phase 2 certificate-distribution fields (mirrors the gateway's
	// agentTelemetryRequest tags exactly).
	CertFingerprint    string    `json:"cert_fingerprint"`
	CertNotAfter       time.Time `json:"cert_not_after"`
	CertMode           string    `json:"cert_mode"`
	CertCAFingerprints []string  `json:"cert_ca_fingerprints"`
}

func TestPostSendsBearerAndBody(t *testing.T) {
	var gotAuth string
	var gotBody wireSample
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if r.URL.Path != "/api/agent/v1/telemetry" {
			t.Errorf("path = %q, want /api/agent/v1/telemetry", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := New(ts.URL, "secret", nil)
	if err := c.Post(context.Background(), testSample()); err != nil {
		t.Fatalf("Post: %v", err)
	}

	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer secret")
	}
	if gotBody.Host == nil || gotBody.Host.CPUUtilPct != 42.5 {
		t.Errorf("host.cpu_util_pct not round-tripped: %+v", gotBody.Host)
	}
	if len(gotBody.GPUs) != 1 || gotBody.GPUs[0].Name != "TestGPU" {
		t.Errorf("gpus[0].name not round-tripped: %+v", gotBody.GPUs)
	}
	if string(gotBody.ProviderHealth) != "{}" {
		t.Errorf("provider_health = %s, want {}", gotBody.ProviderHealth)
	}
	if string(gotBody.Capabilities) != "{}" {
		t.Errorf("capabilities = %s, want {}", gotBody.Capabilities)
	}
}

// TestPostSendsCertFieldsAndDefaultsCAFingerprintsToEmpty is the POST-client
// half of the Task 5b sample round-trip requirement: the four Phase 2
// certificate fields reach the wire under their exact gateway tags, and an
// unset CertCAFingerprints marshals as "[]" (Client.Post calls sm.Normalize()
// before marshaling), never as a JSON null.
func TestPostSendsCertFieldsAndDefaultsCAFingerprintsToEmpty(t *testing.T) {
	notAfter := time.Date(2027, 6, 5, 4, 3, 2, 0, time.UTC)
	var raw json.RawMessage
	var gotBody wireSample
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		raw = body
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	s := testSample()
	s.CertFingerprint = "abc123"
	s.CertNotAfter = notAfter
	s.CertMode = "files"
	// CertCAFingerprints left nil on purpose: Normalize() must default it.

	c := New(ts.URL, "secret", nil)
	if err := c.Post(context.Background(), s); err != nil {
		t.Fatalf("Post: %v", err)
	}

	if gotBody.CertFingerprint != "abc123" {
		t.Errorf("cert_fingerprint = %q, want abc123", gotBody.CertFingerprint)
	}
	if !gotBody.CertNotAfter.Equal(notAfter) {
		t.Errorf("cert_not_after = %v, want %v", gotBody.CertNotAfter, notAfter)
	}
	if gotBody.CertMode != "files" {
		t.Errorf("cert_mode = %q, want files", gotBody.CertMode)
	}
	if gotBody.CertCAFingerprints == nil {
		t.Errorf("cert_ca_fingerprints decoded as nil (should always decode into a non-nil, possibly empty, slice for a JSON [])")
	}
	if !strings.Contains(string(raw), `"cert_ca_fingerprints":[]`) {
		t.Errorf("raw JSON missing cert_ca_fingerprints:[]; got %s", raw)
	}
	if strings.Contains(string(raw), "null") {
		t.Errorf("raw JSON contains null; got %s", raw)
	}
}

func TestPostRetriesOn503(t *testing.T) {
	old := backoffBase
	backoffBase = time.Millisecond
	defer func() { backoffBase = old }()

	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := New(ts.URL, "secret", nil)
	if err := c.Post(context.Background(), testSample()); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

func TestPostNoRetryOn401(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	c := New(ts.URL, "secret", nil)
	err := c.Post(context.Background(), testSample())
	if err == nil {
		t.Fatal("Post: expected error on 401, got nil")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls = %d, want 1 (no retry on 4xx)", got)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error message leaks the token: %q", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error message should include the status: %q", err)
	}
}

func TestPostSystemReportSendsBearerBodyAndPath(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody sample.SystemReport
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := New(ts.URL, "sekret", nil)
	rep := &sample.SystemReport{AgentVersion: "1.2.3", CPU: sample.CPUInfo{Model: "X", LogicalThreads: 8}}
	if err := c.PostSystemReport(context.Background(), rep); err != nil {
		t.Fatalf("PostSystemReport: %v", err)
	}
	if gotAuth != "Bearer sekret" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotPath != "/api/agent/v1/system-report" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody.AgentVersion != "1.2.3" || gotBody.CPU.Model != "X" {
		t.Fatalf("body = %#v", gotBody)
	}
}

// TestPostRuntimeReport proves the file-mode upward report POST: bearer
// auth, the exact path, the body sent through byte-for-byte (BuildReport in
// internal/runtime has already redacted and marshaled it -- this transport
// must not touch it further), and the same retry-on-5xx discipline as
// telemetry/system-report (postBody is shared).
func TestPostRuntimeReport(t *testing.T) {
	old := backoffBase
	backoffBase = time.Millisecond
	defer func() { backoffBase = old }()

	var calls int32
	var gotAuth, gotPath string
	var gotBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		gotBody = body
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := New(ts.URL, "secret", nil)
	raw := json.RawMessage(`{"source":"file","collected_at":"2026-08-26T09:00:00Z","config":{}}`)
	if err := c.PostRuntimeReport(context.Background(), raw); err != nil {
		t.Fatalf("PostRuntimeReport: %v", err)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer secret")
	}
	if gotPath != "/api/agent/v1/runtime-report" {
		t.Errorf("path = %q, want /api/agent/v1/runtime-report", gotPath)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("calls = %d, want 3 (retry on 503)", got)
	}
	if string(gotBody) != string(raw) {
		t.Errorf("body = %s, want %s (byte-for-byte, untouched)", gotBody, raw)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestNewUsesInjectedHTTPClient(t *testing.T) {
	called := false
	injected := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		if got := req.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q, want bearer token", got)
		}
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	c := New("https://gateway.invalid", "secret", injected)
	if err := c.Post(context.Background(), testSample()); err != nil {
		t.Fatalf("Post with injected client: %v", err)
	}
	if !called {
		t.Fatal("injected HTTP client transport was not used")
	}
}

func TestNewNilHTTPClientUsesTenSecondTimeout(t *testing.T) {
	c := New("https://gateway.example", "secret", nil)
	if got := c.http.Timeout; got != 10*time.Second {
		t.Fatalf("default HTTP timeout = %v, want 10s", got)
	}
}

func TestPostUsesInjectedHTTPClientForTLS(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer tls-secret" {
			t.Errorf("Authorization = %q, want TLS bearer token", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "tls-secret", srv.Client())
	if err := c.Post(context.Background(), testSample()); err != nil {
		t.Fatalf("Post over injected TLS client: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("TLS calls = %d, want 1", got)
	}
}
