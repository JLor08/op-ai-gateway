// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureDebug redirects the default slog logger to a buffer at debug level for the
// duration of the test, restoring the previous logger afterward.
func captureDebug(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestLHMCollectLogsQueryAttempt: a successful LHM query emits a debug line naming
// the query and the URL, so `-v` shows the agent is talking to LibreHardwareMonitor.
func TestLHMCollectLogsQueryAttempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(lhmDataJSON))
	}))
	defer srv.Close()

	buf := captureDebug(t)
	c := newLHMPowerCollector(srv.URL, srv.Client())
	if _, _, err := c.Collect(context.Background()); err != nil {
		t.Fatalf("Collect err: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "lhm power") || !strings.Contains(out, srv.URL) {
		t.Fatalf("expected a debug log mentioning the LHM query + url; got:\n%s", out)
	}
}

// TestLHMCollectLogsNon2xx: a non-2xx response is logged (with a reason), not silent.
func TestLHMCollectLogsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	buf := captureDebug(t)
	c := newLHMPowerCollector(srv.URL, srv.Client())
	_, _, _ = c.Collect(context.Background())
	out := buf.String()
	if !strings.Contains(out, "lhm power") || !strings.Contains(out, srv.URL) {
		t.Fatalf("expected a debug log on non-2xx mentioning the url; got:\n%s", out)
	}
}

// TestPowerSourcesReflectsLHM: PowerSources lists "lhm" iff an LHM URL is configured —
// the startup line that tells the operator whether LHM is even a source.
func TestPowerSourcesReflectsLHM(t *testing.T) {
	with := PowerSources(DetectPowerCollector("http://127.0.0.1:8085/data.json"))
	if !containsString(with, "lhm") {
		t.Fatalf("expected 'lhm' among power sources when a URL is set, got %v", with)
	}
	without := PowerSources(DetectPowerCollector(""))
	if containsString(without, "lhm") {
		t.Fatalf("did not expect 'lhm' among power sources with no URL, got %v", without)
	}
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
