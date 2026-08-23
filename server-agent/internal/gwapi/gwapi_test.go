// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gwapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrimBase(t *testing.T) {
	cases := map[string]string{
		"https://gw":       "https://gw",
		"https://gw/":      "https://gw",
		"https://gw/base":  "https://gw/base",
		"https://gw/base/": "https://gw/base",
		"https://gw///":    "https://gw",
	}
	for in, want := range cases {
		if got := TrimBase(in); got != want {
			t.Errorf("TrimBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBearerValue(t *testing.T) {
	if got := BearerValue("tok"); got != "Bearer tok" {
		t.Errorf("BearerValue = %q, want %q", got, "Bearer tok")
	}
}

func TestNewEndpointTrimsBaseOnce(t *testing.T) {
	ep := NewEndpoint("https://gw/base/", "tok", http.DefaultClient)
	if ep.Base != "https://gw/base" {
		t.Errorf("Base = %q, want %q", ep.Base, "https://gw/base")
	}
	if ep.Token != "tok" {
		t.Errorf("Token = %q, want %q", ep.Token, "tok")
	}
	if ep.Client != http.DefaultClient {
		t.Error("Client not retained")
	}
}

// TestEndpointGetConditionalPreservesBasePathAndHeaders proves the plain
// string-concatenation path join (never url.JoinPath): a base with an
// existing path segment must not be normalized away, and both the bearer
// header and a non-empty etag's If-None-Match must reach the server.
func TestEndpointGetConditionalPreservesBasePathAndHeaders(t *testing.T) {
	var gotPath, gotAuth, gotINM string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotINM = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ep := NewEndpoint(srv.URL+"/base/", "s3cr3t", srv.Client())
	resp, err := ep.GetConditional(context.Background(), "/v1/thing", `"etag-1"`)
	if err != nil {
		t.Fatalf("GetConditional: %v", err)
	}
	DrainLimited(resp)

	if gotPath != "/base/v1/thing" {
		t.Errorf("path = %q, want %q (base path must be preserved)", gotPath, "/base/v1/thing")
	}
	if gotAuth != "Bearer s3cr3t" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer s3cr3t")
	}
	if gotINM != `"etag-1"` {
		t.Errorf("If-None-Match = %q, want %q", gotINM, `"etag-1"`)
	}
}

// TestEndpointGetOmitsIfNoneMatch proves the unconditional Get sends no
// If-None-Match header at all.
func TestEndpointGetOmitsIfNoneMatch(t *testing.T) {
	var sawHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Get("If-None-Match") != ""
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ep := NewEndpoint(srv.URL, "tok", srv.Client())
	resp, err := ep.Get(context.Background(), "/x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	DrainLimited(resp)
	if sawHeader {
		t.Error("Get sent an If-None-Match header, want none")
	}
}

func TestConditionalGetIsTheLowLevelPrimitive(t *testing.T) {
	var gotAuth, gotINM string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotINM = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	resp, err := ConditionalGet(context.Background(), srv.Client(), srv.URL+"/ca", "tok", `"v1"`)
	if err != nil {
		t.Fatalf("ConditionalGet: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("status = %d, want 304", resp.StatusCode)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotINM != `"v1"` {
		t.Errorf("If-None-Match = %q", gotINM)
	}
}

func TestResponseETagPrefersHeaderOverBodyFallback(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("ETag", `  "hdr"  `)
	if got := ResponseETag(resp, "body"); got != `"hdr"` {
		t.Errorf("ResponseETag = %q, want %q (header, trimmed)", got, `"hdr"`)
	}
}

func TestResponseETagFallsBackToQuotedBodyField(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	if got := ResponseETag(resp, "body-etag"); got != `"body-etag"` {
		t.Errorf("ResponseETag = %q, want %q", got, `"body-etag"`)
	}
}

func TestResponseETagEmptyWhenNeitherPresent(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	if got := ResponseETag(resp, ""); got != "" {
		t.Errorf("ResponseETag = %q, want empty", got)
	}
}

func TestDrainAllowsBodyToBeFullyConsumed(t *testing.T) {
	resp := &http.Response{Body: io.NopCloser(newRepeatReader('a', 10))}
	Drain(resp)
	if n, _ := resp.Body.Read(make([]byte, 1)); n != 0 {
		t.Error("body not drained")
	}
}

func TestDrainLimitedBoundsToMaxResponseBytes(t *testing.T) {
	// A body larger than MaxResponseBytes must not make DrainLimited block
	// reading the whole thing -- it only discards up to the bound.
	resp := &http.Response{Body: io.NopCloser(newRepeatReader('x', MaxResponseBytes+1))}
	DrainLimited(resp) // must return promptly; a hang here is the failure mode
}

// repeatReader yields n copies of b, then EOF.
type repeatReader struct {
	b byte
	n int64
}

func newRepeatReader(b byte, n int64) *repeatReader { return &repeatReader{b: b, n: n} }

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	m := int64(len(p))
	if m > r.n {
		m = r.n
	}
	for i := int64(0); i < m; i++ {
		p[i] = r.b
	}
	r.n -= m
	return int(m), nil
}
