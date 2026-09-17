// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

// Every existing recordUsage path must write a token-metered row: usageMeta is
// built as a KEYED literal at all ten call sites, so the pair is the Go zero
// value unless a producer sets it. This is what makes the whole change a no-op
// for LLM traffic, independently of migration v81's column defaults.
func TestRecordUsageWritesTokenMeteredByDefault(t *testing.T) {
	srv := NewTestServer()

	srv.recordUsage(
		time.Now().Add(-100*time.Millisecond),
		auth.Token{ID: "tok_user", UserID: "usr_billing"},
		inference.Request{Model: "qwen-coder"},
		routing.Target{ServerID: "mock-host-comp"},
		provider.Response{Usage: inference.Usage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30}},
		"", "success",
		usageMeta{ReqPath: "/v1/chat/completions", HTTPStatus: 200, ContentType: "application/json"},
		"req_billing_default", nil,
	)

	events := srv.Usage.ByUser("usr_billing")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	got := events[0]
	if got.BillingUnit != usage.BillingUnitTokens {
		t.Errorf("BillingUnit = %q, want %q", got.BillingUnit, usage.BillingUnitTokens)
	}
	if got.BillingQuantity != 0 {
		t.Errorf("BillingQuantity = %v, want 0", got.BillingQuantity)
	}
	if err := usage.ValidateBillingXOR(got); err != nil {
		t.Errorf("recorded event violates the XOR: %v", err)
	}
}

// A usageMeta carrying a unit reaches the row unchanged -- the seam #71/#68/#69
// will use.
func TestRecordUsageCarriesBillingPairFromUsageMeta(t *testing.T) {
	srv := NewTestServer()

	srv.recordUsage(
		time.Now().Add(-2*time.Second),
		auth.Token{ID: "tok_user", UserID: "usr_billing_img"},
		inference.Request{Model: "sd-turbo"},
		routing.Target{ServerID: "mock-host-comp"},
		provider.Response{},
		"", "success",
		usageMeta{
			ReqPath:         "/v1/images/generations",
			HTTPStatus:      200,
			ContentType:     "application/json",
			BillingUnit:     usage.BillingUnitImage,
			BillingQuantity: 2,
		},
		"req_billing_image", nil,
	)

	events := srv.Usage.ByUser("usr_billing_img")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	got := events[0]
	if got.BillingUnit != usage.BillingUnitImage || got.BillingQuantity != 2 {
		t.Errorf("got (%q, %v), want (%q, 2)", got.BillingUnit, got.BillingQuantity, usage.BillingUnitImage)
	}
	if err := usage.ValidateBillingXOR(got); err != nil {
		t.Errorf("recorded event violates the XOR: %v", err)
	}
}

// A violating row is still recorded -- unmodified -- AND the violation is logged
// at Error, because dropping the row would lose a request from billing and
// repairing it would destroy the evidence, while staying silent would leave a
// wrong producer undetectable.
//
// The log assertion is what gives this test teeth. The other two tests here call
// usage.ValidateBillingXOR on the recorded row themselves, so they pass whether
// or not recordUsage validates anything; "recorded unmodified" is likewise
// satisfied by a recordUsage that never validates at all. Deleting recordUsage's
// ValidateBillingXOR block -- the single enforcement point ADR-041 and
// telemetry-usage-observability.md §8.4.1 both promise -- fails only this
// assertion.
func TestRecordUsageRecordsAnXORViolationUnmodified(t *testing.T) {
	srv := NewTestServer()
	buf, restore := withCapturedSlog(t)
	t.Cleanup(restore)

	srv.recordUsage(
		time.Now().Add(-time.Second),
		auth.Token{ID: "tok_user", UserID: "usr_billing_bad"},
		inference.Request{Model: "sd-turbo"},
		routing.Target{ServerID: "mock-host-comp"},
		provider.Response{Usage: inference.Usage{OutputTokens: 7, TotalTokens: 7}},
		"", "success",
		usageMeta{ReqPath: "/v1/images/generations", HTTPStatus: 200, BillingUnit: usage.BillingUnitImage, BillingQuantity: 1},
		"req_billing_violation", nil,
	)

	events := srv.Usage.ByUser("usr_billing_bad")
	if len(events) != 1 {
		t.Fatalf("want the violating row recorded, got %d events", len(events))
	}
	if events[0].OutputTokens != 7 || events[0].BillingUnit != usage.BillingUnitImage {
		t.Errorf("the row must be recorded unmodified, got %+v", events[0])
	}

	// The violation itself must be observable to an operator, at Error. The
	// message is matched by substring and the request id by attr, so a violation
	// is traceable back to the one request that caused it.
	recs := buf.Snapshot()
	var logged *logbuffer.Record
	for i := range recs {
		if recs[i].Level == "ERROR" && strings.Contains(recs[i].Msg, "usage billing contract violated") {
			logged = &recs[i]
			break
		}
	}
	if logged == nil {
		t.Fatalf("want an ERROR log for the XOR violation, got %d records: %+v", len(recs), recs)
	}
	if logged.Attrs["id"] != "req_billing_violation" {
		t.Errorf("id attr = %v, want req_billing_violation", logged.Attrs["id"])
	}
	if logged.Attrs["req_path"] != "/v1/images/generations" {
		t.Errorf("req_path attr = %v, want /v1/images/generations", logged.Attrs["req_path"])
	}
}
