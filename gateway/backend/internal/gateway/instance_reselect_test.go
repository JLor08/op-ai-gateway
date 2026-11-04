// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
)

// Only the router's refusals of an address it does not serve, each under the
// status the agent answers it with, say an instance is gone.
func TestIsInstanceGoneError(t *testing.T) {
	envelope := func(status int, code string) error {
		return provider.RouterStatusError(status, []byte(fmt.Sprintf(`{"error":{"code":%q}}`, code)))
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"404 model not managed", envelope(http.StatusNotFound, "runtime.model_not_managed"), true},
		{"404 model not running", envelope(http.StatusNotFound, "runtime.model_not_running"), true},
		{"409 instance no capacity", envelope(http.StatusConflict, "runtime.instance_no_capacity"), true},
		{"409 instance demand unknown", envelope(http.StatusConflict, "runtime.instance_demand_unknown"), true},
		{"wrapped", fmt.Errorf("relay: %w", envelope(http.StatusConflict, "runtime.instance_no_capacity")), true},
		{"a known code under another status", envelope(http.StatusServiceUnavailable, "runtime.instance_no_capacity"), false},
		{"another router code", envelope(http.StatusServiceUnavailable, "runtime.admission_blocked"), false},
		{"a 404 without an envelope", provider.RouterStatusError(http.StatusNotFound, []byte("not found")), false},
		{"another error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := isInstanceGoneError(tc.err); got != tc.want {
			t.Errorf("%s: isInstanceGoneError = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A request is re-selected only off an instance k >= 1, and only once.
func TestReselectsLostInstance(t *testing.T) {
	gone := provider.RouterStatusError(http.StatusConflict, []byte(`{"error":{"code":"runtime.instance_no_capacity"}}`))
	k2 := routing.Target{InstanceKey: "map#2"}
	if !reselectsLostInstance(k2, inference.Request{}, gone) {
		t.Fatal("instance 2 gone: not re-selected, want re-selected")
	}
	if reselectsLostInstance(routing.Target{InstanceKey: "map#0"}, inference.Request{}, gone) {
		t.Fatal("instance 0: re-selected, want never")
	}
	again := withLostInstance(inference.Request{}, k2)
	if reselectsLostInstance(routing.Target{InstanceKey: "map#3"}, again, gone) {
		t.Fatal("a re-selection: re-selected again, want once only")
	}
	orig := inference.Request{ExcludeInstanceKeys: []string{"map#1"}}
	more := withLostInstance(orig, k2)
	if len(orig.ExcludeInstanceKeys) != 1 || strings.Join(more.ExcludeInstanceKeys, ",") != "map#1,map#2" {
		t.Fatalf("withLostInstance: caller's %v, result %v; want the caller's untouched and the result extended", orig.ExcludeInstanceKeys, more.ExcludeInstanceKeys)
	}
}

// closeRecorder is a response body that records whether it was closed.
type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error {
	c.closed = true
	return nil
}

// Peeking reads only a 404 or 409, and every answer keeps its body byte for
// byte. A gone instance's answer has its connection released at once; any
// other answer is left open for its relay.
func TestPeekInstanceGoneKeepsTheBody(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{http.StatusConflict, `{"error":{"code":"runtime.instance_no_capacity"}}`, true},
		{http.StatusNotFound, `{"error":{"code":"other"}}`, false},
		{http.StatusInternalServerError, `{"error":{"code":"runtime.instance_no_capacity"}}`, false},
	} {
		orig := &closeRecorder{Reader: strings.NewReader(tc.body)}
		resp := &provider.ProxyResponse{StatusCode: tc.status, Body: orig}
		if got := peekInstanceGone(resp); got != tc.want {
			t.Errorf("status %d %s: gone = %v, want %v", tc.status, tc.body, got, tc.want)
		}
		if orig.closed != tc.want {
			t.Errorf("status %d %s: original body closed = %v, want %v", tc.status, tc.body, orig.closed, tc.want)
		}
		if rest, _ := io.ReadAll(resp.Body); string(rest) != tc.body {
			t.Errorf("status %d: body after the peek = %q, want %q", tc.status, rest, tc.body)
		}
	}
}
