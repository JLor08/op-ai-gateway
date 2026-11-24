// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
	"time"
)

// seedSessionVendorAccounts gives user userID two vendor accounts (a/, b/) that
// each serve gpt-4o.
func seedSessionVendorAccounts(t *testing.T, srv *Server, userID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	for _, p := range []string{"a", "b"} {
		id := "acc_" + p
		if err := srv.Routes.CreateVendorAccount(ctx, routing.VendorAccount{
			ID: id, OwnerUserID: userID, Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey,
			Name: id, Status: routing.VendorAccountStatusActive, ModelPrefix: p + "/", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if err := srv.Routes.SetVendorAccountModels(ctx, id, []routing.VendorAccountModel{
			{GatewayModel: p + "/gpt-4o", UpstreamModel: "gpt-4o", APIFlavor: routing.APIFlavorOpenAI},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

type chatRowVendorAccess struct {
	All           bool `json:"all"`
	IncludeGroups bool `json:"include_groups"`
	Accounts      []struct {
		AccountID string `json:"account_id"`
	} `json:"accounts"`
}

// chatRowOf reads the synthetic chat-session row from GET /api/portal/tokens and
// returns its raw vendor_access (nil when omitted).
func chatRowOf(t *testing.T, srv *Server, cookie *http.Cookie) *chatRowVendorAccess {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/portal/tokens", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tokens: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			IsChatSession bool                 `json:"is_chat_session"`
			VendorAccess  *chatRowVendorAccess `json:"vendor_access"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, d := range body.Data {
		if d.IsChatSession {
			return d.VendorAccess
		}
	}
	t.Fatal("no chat session row")
	return nil
}

func chatSettingsSetup(t *testing.T) (*Server, *http.Cookie) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_cv", "cv@example.test", "password-1", "user")
	seedSessionVendorAccounts(t, srv, "usr_cv")
	return srv, loginAs(t, srv, "cv@example.test", "password-1")
}

func TestChatSettingsVendorAccess(t *testing.T) {
	t.Run("saves an own account and reads it back", func(t *testing.T) {
		srv, cookie := chatSettingsSetup(t)
		rec := doJSON(t, srv, cookie, http.MethodPut, "/api/portal/chat-settings",
			`{"log_communication":false,"secret":false,"vendor_access":{"all":false,"accounts":[{"account_id":"acc_a"}]}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
		}
		va := chatRowOf(t, srv, cookie)
		if va == nil || va.All || len(va.Accounts) != 1 || va.Accounts[0].AccountID != "acc_a" {
			t.Fatalf("row = %+v", va)
		}
	})
	t.Run("booleans only leave the policy untouched", func(t *testing.T) {
		srv, cookie := chatSettingsSetup(t)
		doJSON(t, srv, cookie, http.MethodPut, "/api/portal/chat-settings",
			`{"vendor_access":{"all":false,"accounts":[{"account_id":"acc_b"}]}}`)
		rec := doJSON(t, srv, cookie, http.MethodPut, "/api/portal/chat-settings", `{"log_communication":true,"secret":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
		}
		va := chatRowOf(t, srv, cookie)
		if va == nil || len(va.Accounts) != 1 || va.Accounts[0].AccountID != "acc_b" {
			t.Fatalf("policy changed by a booleans-only PUT: %+v", va)
		}
	})
	t.Run("explicit empty policy is present, not unset", func(t *testing.T) {
		srv, cookie := chatSettingsSetup(t)
		rec := doJSON(t, srv, cookie, http.MethodPut, "/api/portal/chat-settings", `{"vendor_access":{"all":false,"accounts":[]}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
		}
		va := chatRowOf(t, srv, cookie)
		if va == nil || va.All || va.IncludeGroups || va.Accounts == nil || len(va.Accounts) != 0 {
			t.Fatalf("row = %+v, want {all:false, accounts:[]}", va)
		}
	})
	t.Run("unset user shows all and groups", func(t *testing.T) {
		srv, cookie := chatSettingsSetup(t)
		va := chatRowOf(t, srv, cookie)
		if va == nil || !va.All || !va.IncludeGroups {
			t.Fatalf("row = %+v", va)
		}
	})
	t.Run("invalid id and conflict are 400 and store nothing", func(t *testing.T) {
		cases := []struct{ name, body, code string }{
			{"invalid", `{"vendor_access":{"accounts":[{"account_id":"acc_nope"}]}}`, "portal.token_vendor_access_invalid"},
			{"conflict", `{"vendor_access":{"accounts":[{"account_id":"acc_a","prefix_override":{"enabled":true,"value":""}},{"account_id":"acc_b","prefix_override":{"enabled":true,"value":""}}]}}`, "portal.token_vendor_access_conflict"},
		}
		for _, tc := range cases {
			srv, cookie := chatSettingsSetup(t)
			rec := doJSON(t, srv, cookie, http.MethodPut, "/api/portal/chat-settings", tc.body)
			if rec.Code != http.StatusBadRequest || errorBodyOf(t, rec) != tc.code {
				t.Fatalf("%s: %d %s", tc.name, rec.Code, rec.Body.String())
			}
			if va := chatRowOf(t, srv, cookie); va == nil || !va.All {
				t.Fatalf("%s: policy stored despite the error: %+v", tc.name, va)
			}
		}
	})
}

func TestChatSettingsVendorAccessBearer(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_cb", "cb@example.test", "password-1", "user")
	now := time.Now().UTC()
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_cb", UserID: "usr_cb", Name: "cb", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "cb-secret"); err != nil {
		t.Fatal(err)
	}
	put := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/portal/chat-settings", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer cb-secret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	rec := put(`{"log_communication":true,"vendor_access":{"all":false,"accounts":[]}}`)
	if rec.Code != http.StatusForbidden || errorBodyOf(t, rec) != "portal.token_forbidden" {
		t.Fatalf("bearer with vendor_access: %d %s", rec.Code, rec.Body.String())
	}
	user, err := dir.UserByID(context.Background(), "usr_cb")
	if err != nil {
		t.Fatal(err)
	}
	if user.ChatVendorAccess != "" || user.ChatLogCommunication {
		t.Fatalf("something was stored by the refused request: %+v", user)
	}
	if rec = put(`{"log_communication":true,"secret":false}`); rec.Code != http.StatusOK {
		t.Fatalf("bearer booleans-only: %d %s", rec.Code, rec.Body.String())
	}
	if user, _ = dir.UserByID(context.Background(), "usr_cb"); !user.ChatLogCommunication {
		t.Fatalf("booleans not stored: %+v", user)
	}
}
