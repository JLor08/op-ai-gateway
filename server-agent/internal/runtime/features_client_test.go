// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestFeaturesClientFetchesAndCachesETag proves the happy path: a 200 with
// an ETag header is decoded into the feature list, and the NEXT call sends
// that etag back as If-None-Match.
func TestFeaturesClientFetchesAndCachesETag(t *testing.T) {
	var calls int32
	var gotIfNoneMatch []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		gotIfNoneMatch = append(gotIfNoneMatch, r.Header.Get("If-None-Match"))
		if r.URL.Path != "/api/agent/v1/features" {
			t.Errorf("path = %q, want /api/agent/v1/features", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("ETag", `"abc123"`)
		if n == 1 {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"features":["runtime_manager"]}`))
			return
		}
		// Second call: gateway says nothing changed.
		w.WriteHeader(http.StatusNotModified)
	}))
	defer ts.Close()

	c := NewFeaturesClient(ts.URL, "tok", nil)

	got, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 1 || got[0] != "runtime_manager" {
		t.Fatalf("Fetch = %v, want [runtime_manager]", got)
	}

	got2, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch (304): %v", err)
	}
	if len(got2) != 1 || got2[0] != "runtime_manager" {
		t.Fatalf("Fetch after 304 = %v, want cached [runtime_manager]", got2)
	}
	if len(gotIfNoneMatch) != 2 || gotIfNoneMatch[1] != `"abc123"` {
		t.Fatalf("second request If-None-Match = %v, want the first response's ETag echoed back", gotIfNoneMatch)
	}
}

// TestFeaturesClient404IsEmptySetNilError proves an older gateway lacking
// this endpoint is NOT treated as an error: 404 -> empty set, nil error.
func TestFeaturesClient404IsEmptySetNilError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	c := NewFeaturesClient(ts.URL, "tok", nil)
	got, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch on 404: expected nil error, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Fetch on 404 = %v, want empty set", got)
	}
}

// TestFeaturesClientTransientErrorReturnsLastKnown proves a transport error
// after a prior successful fetch degrades to the last known set with a nil
// error -- a blip must never look like "the gateway declares no features."
func TestFeaturesClientTransientErrorReturnsLastKnown(t *testing.T) {
	var fail atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("ETag", `"e1"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"features":["runtime_manager"]}`))
	}))
	defer ts.Close()

	c := NewFeaturesClient(ts.URL, "tok", nil)
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("first fetch: %v", err)
	}

	fail.Store(true)
	got, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch on transient 503: expected nil error, got %v", err)
	}
	if len(got) != 1 || got[0] != "runtime_manager" {
		t.Fatalf("Fetch on transient error = %v, want cached [runtime_manager]", got)
	}
}
