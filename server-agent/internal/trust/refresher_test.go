// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package trust

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRefresherConditionalGETInstallsValidBundle(t *testing.T) {
	root := newTestCA(t, "refresh-root")
	cache := filepath.Join(t.TempDir(), "server-agent-ca.pem")
	store, err := New(Options{CACacheFile: cache})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer refresh-token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		if call == 2 && r.Header.Get("If-None-Match") != `"bundle-v1"` {
			t.Errorf("If-None-Match=%q", r.Header.Get("If-None-Match"))
		}
		w.Header().Set("ETag", `"bundle-v1"`)
		if call == 2 {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write(root.pem)
	}))
	defer srv.Close()
	r := NewRefresher(srv.URL, "refresh-token", srv.Client(), store)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if got, err := os.ReadFile(cache); err != nil || string(got) != string(root.pem) {
		t.Fatalf("cache_matches=%v cache_bytes=%d want_bytes=%d err=%v", err == nil && string(got) == string(root.pem), len(got), len(root.pem), err)
	}
	want := []string{fingerprint(root.cert)}
	if got := r.DurableFingerprints(); !reflect.DeepEqual(got, want) {
		t.Fatalf("fingerprints=%v want=%v", got, want)
	}
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("conditional refresh: %v", err)
	}
}

func TestRefresherLeavesStoreUntouchedOnBadPEMAuthAndServerErrors(t *testing.T) {
	oldRoot := newTestCA(t, "old-root")
	want := []string{fingerprint(oldRoot.cert)}
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		wantErr error
	}{
		{"bad pem", http.StatusOK, "not pem", nil},
		{"unauthorized", http.StatusUnauthorized, "denied", ErrRefreshUnauthorized},
		{"forbidden", http.StatusForbidden, "denied", ErrRefreshUnauthorized},
		{"not found", http.StatusNotFound, "missing", ErrRefreshNotFound},
		{"server error", http.StatusInternalServerError, "internal secret detail", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := filepath.Join(t.TempDir(), "ca.pem")
			if err := os.WriteFile(cache, oldRoot.pem, 0o644); err != nil {
				t.Fatal(err)
			}
			store, err := New(Options{CACacheFile: cache})
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			r := NewRefresher(srv.URL, "token", srv.Client(), store)
			refreshErr := r.Refresh(context.Background())
			if refreshErr == nil || (tc.wantErr != nil && !errors.Is(refreshErr, tc.wantErr)) {
				t.Fatalf("refresh_failed=%v named_error=%v", refreshErr != nil, tc.wantErr == nil || errors.Is(refreshErr, tc.wantErr))
			}
			if tc.status == http.StatusInternalServerError && strings.Contains(refreshErr.Error(), tc.body) {
				t.Fatal("server response body escaped into refresh error")
			}
			if got := r.DurableFingerprints(); !reflect.DeepEqual(got, want) {
				t.Fatalf("fingerprints=%v want=%v", got, want)
			}
			if got, _ := os.ReadFile(cache); string(got) != string(oldRoot.pem) {
				t.Fatalf("cache mutated: cache_bytes=%d want_bytes=%d", len(got), len(oldRoot.pem))
			}
		})
	}
}

func TestRefresherSerializesFetchInstallAndETag(t *testing.T) {
	rootA := newTestCA(t, "refresh-a")
	rootB := newTestCA(t, "refresh-b")
	cache := filepath.Join(t.TempDir(), "ca.pem")
	store, err := New(Options{CACacheFile: cache})
	if err != nil {
		t.Fatal(err)
	}
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	secondIfNone := ""
	thirdIfNone := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		switch call {
		case 1:
			close(firstStarted)
			<-releaseFirst
			w.Header().Set("ETag", `"a"`)
			_, _ = w.Write(rootA.pem)
		case 2:
			mu.Lock()
			secondIfNone = req.Header.Get("If-None-Match")
			mu.Unlock()
			close(secondStarted)
			w.Header().Set("ETag", `"b"`)
			_, _ = w.Write(rootB.pem)
		default:
			mu.Lock()
			thirdIfNone = req.Header.Get("If-None-Match")
			mu.Unlock()
			w.WriteHeader(http.StatusNotModified)
		}
	}))
	defer srv.Close()
	r := NewRefresher(srv.URL, "token", srv.Client(), store)
	errs := make(chan error, 2)
	go func() { errs <- r.Refresh(context.Background()) }()
	<-firstStarted
	go func() { errs <- r.Refresh(context.Background()) }()

	overlapped := false
	select {
	case <-secondStarted:
		overlapped = true
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("Refresh: %v", err)
		}
	}
	if overlapped {
		t.Error("second refresh overlapped the first fetch/install/ETag transaction")
	}
	mu.Lock()
	gotSecondIfNone := secondIfNone
	mu.Unlock()
	if gotSecondIfNone != `"a"` {
		t.Errorf("second If-None-Match=%q, want first committed ETag", gotSecondIfNone)
	}
	if got, err := os.ReadFile(cache); err != nil || string(got) != string(rootB.pem) {
		t.Fatalf("final cache matches bundle B=%v bytes=%d err=%v", err == nil && string(got) == string(rootB.pem), len(got), err)
	}
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotThirdIfNone := thirdIfNone
	mu.Unlock()
	if gotThirdIfNone != `"b"` {
		t.Fatalf("final If-None-Match=%q, want ETag for bundle B", gotThirdIfNone)
	}
}

func TestRefresherRejectsBundleOverOneMiBWithoutMutation(t *testing.T) {
	oldRoot := newTestCA(t, "oversize-last-good")
	cache := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(cache, oldRoot.pem, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := New(Options{CACacheFile: cache})
	if err != nil {
		t.Fatal(err)
	}
	oversized := make([]byte, maxRefreshBundleBytes+1)
	for i := range oversized {
		oversized[i] = 'x'
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(oversized)
	}))
	defer srv.Close()
	err = NewRefresher(srv.URL, "token", srv.Client(), store).Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds 1048576 bytes") {
		t.Fatalf("oversize rejected=%v named_limit_error=%v", err != nil, err != nil && strings.Contains(err.Error(), "exceeds 1048576 bytes"))
	}
	if got, readErr := os.ReadFile(cache); readErr != nil || string(got) != string(oldRoot.pem) {
		t.Fatalf("cache_unchanged=%v bytes=%d err=%v", readErr == nil && string(got) == string(oldRoot.pem), len(got), readErr)
	}
}

func TestRefresherCanceledCallerDoesNotWaitBehindInflightRefresh(t *testing.T) {
	root := newTestCA(t, "context-gate")
	store, err := New(Options{CACacheFile: filepath.Join(t.TempDir(), "ca.pem")})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write(root.pem)
	}))
	defer srv.Close()
	r := NewRefresher(srv.URL, "token", srv.Client(), store)
	firstDone := make(chan error, 1)
	go func() { firstDone <- r.Refresh(context.Background()) }()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	secondDone := make(chan error, 1)
	go func() { secondDone <- r.Refresh(ctx) }()
	select {
	case err := <-secondDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("second Refresh error=%v, want context cancellation", err)
		}
		close(release)
		if err := <-firstDone; err != nil {
			t.Fatal(err)
		}
	case <-time.After(80 * time.Millisecond):
		close(release)
		<-firstDone
		<-secondDone
		t.Fatal("canceled Refresh waited behind an in-flight network request")
	}
}
