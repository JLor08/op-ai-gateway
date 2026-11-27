// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDismissGPUVendorNote(t *testing.T) {
	ctx := context.Background()
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_note")
	if err := srv.Routes.SetServerOwners(ctx, "mock-host-qwen", []string{"usr_dev"}); err != nil {
		t.Fatalf("SetServerOwners: %v", err)
	}
	if err := srv.ingestSystemReport(ctx, "mock-host-qwen", gpuReportBody("nvidia", "amd")); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if s, _ := srv.Routes.AIServerByID(ctx, "mock-host-qwen"); !s.GPUVendorNote {
		t.Fatal("the mixed backfill raised no note")
	}

	const path = "/api/portal/servers/mock-host-qwen/gpu-vendor-note/dismiss"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodGet, path, ``))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, path, ``))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d, want 204, body = %s", rec.Code, rec.Body.String())
	}
	s, _ := srv.Routes.AIServerByID(ctx, "mock-host-qwen")
	if s.GPUVendorNote || s.GPULegacyVendor != "nvidia" {
		t.Fatalf("after dismiss: note %v legacy %q, want false nvidia", s.GPUVendorNote, s.GPULegacyVendor)
	}

	// An id the caller cannot see answers like an unknown one (the portal
	// service test covers the foreign principal).
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers/srv_missing/gpu-vendor-note/dismiss", ``))
	if rec.Code != http.StatusNotFound || errorBodyOf(t, rec) != "server.not_found" {
		t.Fatalf("unknown server: %d %s", rec.Code, rec.Body.String())
	}
}
