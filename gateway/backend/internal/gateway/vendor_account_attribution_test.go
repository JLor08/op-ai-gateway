// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// TestRecordUsageAttributesVendorAccountID proves recordUsage stamps the usage
// event with the serving target's VendorAccountID (M6a per-account attribution),
// the same shape ServiceID/ProjectID attribution already follows: it is read off
// the resolved Target, needing no extra store round-trip.
func TestRecordUsageAttributesVendorAccountID(t *testing.T) {
	srv := NewTestServer()
	target := routing.Target{RouteID: "vendor:acc_x:gpt-4o", Provider: routing.ProviderVendorOpenAI, VendorAccountID: "acc_x"}
	srv.recordUsage(time.Now(), auth.Token{ID: "tok", UserID: "usr_v"}, inference.Request{Model: "gpt-4o"}, target, provider.Response{}, "", "success", usageMeta{}, "req_vendor_attr", nil)

	events := srv.Usage.ByUser("usr_v")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].AccountID != "acc_x" {
		t.Fatalf("recorded AccountID = %q, want acc_x", events[0].AccountID)
	}
}

// TestRecordUsageVendorAccountIDEmptyForNonVendor is the no-op-invariant
// regression: a self-hosted target carries no VendorAccountID, so the recorded
// event's AccountID must be "" (the overwhelming default, byte-identical to
// pre-M6a behavior).
func TestRecordUsageVendorAccountIDEmptyForNonVendor(t *testing.T) {
	srv := NewTestServer()
	target := routing.Target{RouteID: "map_1", Provider: routing.ProviderOllama, ServerID: "srv1"}
	srv.recordUsage(time.Now(), auth.Token{ID: "tok", UserID: "usr_s"}, inference.Request{Model: "m"}, target, provider.Response{}, "", "success", usageMeta{}, "req_selfhosted_attr", nil)

	events := srv.Usage.ByUser("usr_s")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].AccountID != "" {
		t.Fatalf("self-hosted event AccountID = %q, want empty", events[0].AccountID)
	}
}
