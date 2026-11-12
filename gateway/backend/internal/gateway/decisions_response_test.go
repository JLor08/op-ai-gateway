// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDecisionsUpstreamMessage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // "" = no known shape
	}{
		{"error object", `{"error":{"message":"bad","type":"BadRequestError"}}`, "bad"},
		{"detail string", `{"detail":"Not Found"}`, "Not Found"},
		{"detail list", `{"detail":[{"loc":["body"],"msg":"field required"},{"msg":"second"}]}`, "field required; second"},
		{"detail list without msg", `{"detail":[{"loc":["body"]}]}`, ""},
		{"blank message", `{"error":{"message":"  "}}`, ""},
		{"error string is not a known shape", `{"error":"plain"}`, ""},
		{"not json", `<html>`, ""},
		{"empty", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decisionsUpstreamMessage([]byte(tc.body)); got != tc.want {
				t.Fatalf("decisionsUpstreamMessage(%s) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestDecisionsUpstreamErrorBody(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         string
		wantContains []string
	}{
		{"known message", http.StatusBadRequest, `{"error":{"message":"too many"}}`, []string{"too many"}},
		{"generic", http.StatusBadGateway, `nope`, []string{"the System One upstream answered 502"}},
		{"501 guidance with the upstream's message", http.StatusNotImplemented, `{"error":{"message":"no read strategy"}}`, []string{"no read strategy", decisionsNotImplementedGuidance}},
		{"501 guidance without one", http.StatusNotImplemented, ``, []string{decisionsNotImplementedGuidance}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireDecisionsEnvelope(t, decisionsUpstreamErrorBody(tc.status, []byte(tc.body)), tc.wantContains)
		})
	}
}

// requireDecisionsEnvelope requires out to be a decisions.upstream_error
// envelope whose message contains every one of wantContains.
func requireDecisionsEnvelope(t *testing.T, out []byte, wantContains []string) {
	t.Helper()
	var env struct {
		Error struct {
			Code, Message, Type string
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("not JSON: %v; %s", err, out)
	}
	if env.Error.Code != codeDecisionsUpstreamError || env.Error.Type != "upstream_error" {
		t.Fatalf("envelope = %+v, want decisions.upstream_error/upstream_error", env.Error)
	}
	for _, want := range wantContains {
		if !strings.Contains(env.Error.Message, want) {
			t.Fatalf("message = %q, want it to contain %q", env.Error.Message, want)
		}
	}
}
