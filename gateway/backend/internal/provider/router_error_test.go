// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"testing"
)

// A chat call refused by an agent router keeps the router's code and status (a
// *RouterError), buffered and streamed alike, so the gateway can tell an
// instance that is gone from any other failure; the status still maps to the
// provider error it always did. A refusal that is not the router's envelope
// carries no code, and neither does any upstream that is not an agent router:
// its answer is the plain provider error its status maps to, whatever its body
// says, exactly as before agent refusals were read.
func TestChatCallsKeepTheRouterCode(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		status   int
		body     string
		wantCode string
	}{
		{"instance gone", routing.ProviderServerAgent, http.StatusConflict, `{"error":{"code":"runtime.instance_no_capacity","message":"runtime.instance_no_capacity"}}`, "runtime.instance_no_capacity"},
		{"model not managed", routing.ProviderServerAgent, http.StatusNotFound, `{"error":{"code":"runtime.model_not_managed","message":"no spec"}}`, "runtime.model_not_managed"},
		{"not an envelope", routing.ProviderServerAgent, http.StatusNotFound, `not found`, ""},
		{"an upstream that is no agent router", routing.ProviderVLLM, http.StatusNotFound, `{"error":{"code":"runtime.model_not_managed","message":"no spec"}}`, ""},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		t.Cleanup(srv.Close)
		client := NewOpenAICompatibleClient(srv.Client())
		target := routing.Target{Endpoint: srv.URL, Provider: tc.provider, ProviderModel: "m#2"}
		req := inference.Request{Model: "m", Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}}
		calls := map[string]func() error{
			"Complete": func() error {
				_, err := client.Complete(context.Background(), target, req)
				return err
			},
			"CompleteStream": func() error {
				return client.CompleteStream(context.Background(), target, req, func(inference.StreamEvent) error { return nil })
			},
		}
		for call, run := range calls {
			t.Run(tc.name+"/"+call, func(t *testing.T) {
				err := run()
				if !errors.Is(err, ErrUnavailable) {
					t.Fatalf("err = %v, want ErrUnavailable", err)
				}
				var re *RouterError
				if got := errors.As(err, &re); got != (tc.wantCode != "") {
					t.Fatalf("errors.As(err, *RouterError) = %v, want %v (err %v)", got, tc.wantCode != "", err)
				}
				if re != nil && (re.Code != tc.wantCode || re.Status != tc.status) {
					t.Fatalf("RouterError = %q/%d, want %q/%d", re.Code, re.Status, tc.wantCode, tc.status)
				}
				if tc.provider != routing.ProviderServerAgent && err.Error() != unavailableStatus(tc.status).Error() {
					t.Fatalf("err = %q, want the status's own provider error %q", err, unavailableStatus(tc.status))
				}
			})
		}
	}
}
