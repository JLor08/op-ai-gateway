// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

// TestUsageEventVendorGroupIDRoundTrip proves usage_events.vendor_group_id is
// written by Record and read back by ByUser, All and the paged Query alike,
// next to account_id, and that an event outside a group reads back empty.
func TestUsageEventVendorGroupIDRoundTrip(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
		if err := s.CreateUser(ctx, newTestUser("u_vgu", "vgu@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		grouped := testUsageEvent("ue_vgu_1", "u_vgu", "tok_1", "success")
		grouped.AccountID, grouped.VendorGroupID = "va_1", "vag_1"
		plain := testUsageEvent("ue_vgu_2", "u_vgu", "tok_1", "success")
		plain.AccountID = "va_2"
		for _, e := range []usage.Event{grouped, plain} {
			if err := s.Record(e); err != nil {
				t.Fatalf("record %s: %v", e.ID, err)
			}
		}

		check := func(source string, events []usage.Event) {
			t.Helper()
			byID := map[string]usage.Event{}
			for _, e := range events {
				byID[e.ID] = e
			}
			if e := byID["ue_vgu_1"]; e.AccountID != "va_1" || e.VendorGroupID != "vag_1" {
				t.Errorf("%s grouped event AccountID/VendorGroupID = %q/%q, want va_1/vag_1", source, e.AccountID, e.VendorGroupID)
			}
			if e := byID["ue_vgu_2"]; e.AccountID != "va_2" || e.VendorGroupID != "" {
				t.Errorf("%s plain event AccountID/VendorGroupID = %q/%q, want va_2/empty", source, e.AccountID, e.VendorGroupID)
			}
		}
		check("ByUser", s.ByUser("u_vgu"))
		check("All", s.All())
		page, err := s.Query(usage.Query{ScopeAll: true, Page: 1, Limit: 50})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		rows := make([]usage.Event, 0, len(page.Data))
		for _, r := range page.Data {
			rows = append(rows, r.Event)
		}
		check("Query", rows)
	})
}
