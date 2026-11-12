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
		dir          decisionsDirection
		status       int
		body         string
		wantCode     string
		wantContains []string
		wantAbsent   string
	}{
		{"forward known message", decisionsForward, http.StatusBadRequest, `{"error":{"message":"too many"}}`, codeDecisionsUpstreamError, []string{"the System One upstream answered 400", "too many"}, "OpenAI Decisions"},
		{"forward generic", decisionsForward, http.StatusBadGateway, `nope`, codeDecisionsUpstreamError, []string{"the System One upstream answered 502"}, "OpenAI Decisions"},
		{"forward 501 guidance with the upstream's message", decisionsForward, http.StatusNotImplemented, `{"error":{"message":"no read strategy"}}`, codeDecisionsUpstreamError, []string{"no read strategy", decisionsNotImplementedGuidance}, ""},
		{"forward 501 guidance without one", decisionsForward, http.StatusNotImplemented, ``, codeDecisionsUpstreamError, []string{decisionsNotImplementedGuidance}, ""},
		{"reverse known message", decisionsReverse, http.StatusBadRequest, `{"error":{"message":"too many"}}`, codeSystemOneUpstreamError, []string{"the OpenAI Decisions upstream answered 400", "too many"}, "System One upstream"},
		{"reverse detail", decisionsReverse, http.StatusUnprocessableEntity, `{"detail":"bad input"}`, codeSystemOneUpstreamError, []string{"the OpenAI Decisions upstream answered 422", "bad input"}, "System One upstream"},
		{"reverse generic", decisionsReverse, http.StatusBadGateway, `nope`, codeSystemOneUpstreamError, []string{"the OpenAI Decisions upstream answered 502"}, "System One upstream"},
		{"reverse 501 guidance", decisionsReverse, http.StatusNotImplemented, ``, codeSystemOneUpstreamError, []string{systemOneNotImplementedGuidance}, decisionsNotImplementedGuidance},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := decisionsUpstreamErrorBody(tc.dir, tc.status, []byte(tc.body))
			msg := requireDecisionsEnvelope(t, out, tc.wantCode, tc.wantContains)
			if tc.wantAbsent != "" && strings.Contains(msg, tc.wantAbsent) {
				t.Fatalf("message = %q, want it not to contain %q", msg, tc.wantAbsent)
			}
		})
	}
}

// requireDecisionsEnvelope requires out to be a wantCode envelope of type
// upstream_error whose message contains every one of wantContains, and returns
// the message.
func requireDecisionsEnvelope(t *testing.T, out []byte, wantCode string, wantContains []string) string {
	t.Helper()
	var env struct {
		Error struct {
			Code, Message, Type string
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("not JSON: %v; %s", err, out)
	}
	if env.Error.Code != wantCode || env.Error.Type != "upstream_error" {
		t.Fatalf("envelope = %+v, want %s/upstream_error", env.Error, wantCode)
	}
	for _, want := range wantContains {
		if !strings.Contains(env.Error.Message, want) {
			t.Fatalf("message = %q, want it to contain %q", env.Error.Message, want)
		}
	}
	return env.Error.Message
}
