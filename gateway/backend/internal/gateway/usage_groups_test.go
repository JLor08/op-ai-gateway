// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"strings"
	"testing"
)

// TestHandlePortalUsageGroupsRejectsUnknownDimension proves group_by=bogus is a
// 400 with the usage.group_by_invalid code (via portal.ErrUsageGroupByInvalid).
func TestHandlePortalUsageGroupsRejectsUnknownDimension(t *testing.T) {
	srv := NewTestServer()

	req := httptest.NewRequest(http.MethodGet, "/api/portal/usage/groups?group_by=bogus", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "usage.group_by_invalid" {
		t.Fatalf("error code = %q, want usage.group_by_invalid", body.Error.Code)
	}
}

// TestHandlePortalUsageGroupsReturnsData proves a valid dimension returns
// 200 {data, group_by} and folds a recorded completion into a model group.
func TestHandlePortalUsageGroupsReturnsData(t *testing.T) {
	srv := NewTestServer()

	// Drive one completion so there is a usage row for "qwen-coder".
	comp := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`))
	comp.Header.Set("Authorization", "Bearer dev-secret")
	compRec := httptest.NewRecorder()
	srv.ServeHTTP(compRec, comp)
	if compRec.Code != http.StatusOK {
		t.Fatalf("completion status = %d, body = %s", compRec.Code, compRec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/portal/usage/groups?group_by=model", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data    []portal.UsageGroupDTO `json:"data"`
		GroupBy string                 `json:"group_by"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.GroupBy != "model" {
		t.Fatalf("group_by = %q, want model", body.GroupBy)
	}
	found := false
	for _, g := range body.Data {
		if g.Key == "qwen-coder" {
			found = true
			if g.Count < 1 {
				t.Fatalf("qwen-coder group Count = %d, want >= 1", g.Count)
			}
		}
	}
	if !found {
		t.Fatalf("no qwen-coder group in %+v", body.Data)
	}
}
