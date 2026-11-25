// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

func statusResponse(status int, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(""))}
}

// TestVendorStatusErrorClassifiesLimitStatuses pins the limit-error contract: a
// vendor's 429 or 402 is a *LimitError carrying the status, the parsed
// Retry-After and the response headers, and it is still ErrUnavailable, so the
// final client mapping (502 provider.unavailable) is unchanged.
func TestVendorStatusErrorClassifiesLimitStatuses(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusPaymentRequired} {
		h := http.Header{}
		h.Set("Retry-After", "120")
		h.Set("Anthropic-Ratelimit-Unified-Reset", "1900000000")
		err := vendorStatusError(statusResponse(status, h))
		var le *LimitError
		if !errors.As(err, &le) {
			t.Fatalf("status %d: err = %v, want a *LimitError", status, err)
		}
		if le.Status != status || le.RetryAfter != 120*time.Second {
			t.Fatalf("status %d: LimitError = {Status %d, RetryAfter %s}, want {%d, 2m0s}", status, le.Status, le.RetryAfter, status)
		}
		if le.Header.Get("Anthropic-Ratelimit-Unified-Reset") != "1900000000" {
			t.Fatalf("status %d: LimitError.Header lost the vendor header: %v", status, le.Header)
		}
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("status %d: a LimitError must stay ErrUnavailable: %v", status, err)
		}
		// The headers are a copy: the response's own map can be reused freely.
		h.Set("Retry-After", "1")
		if le.Header.Get("Retry-After") != "120" {
			t.Fatalf("status %d: LimitError.Header aliases the response headers", status)
		}
	}
}

func TestVendorStatusErrorParsesRetryAfterHTTPDate(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", time.Now().Add(90*time.Second).UTC().Format(http.TimeFormat))
	var le *LimitError
	if !errors.As(vendorStatusError(statusResponse(http.StatusTooManyRequests, h)), &le) {
		t.Fatal("429 is not a LimitError")
	}
	if le.RetryAfter < 80*time.Second || le.RetryAfter > 91*time.Second {
		t.Fatalf("RetryAfter = %s, want about 90s from an HTTP-date Retry-After", le.RetryAfter)
	}
}

func TestVendorStatusErrorWithoutRetryAfter(t *testing.T) {
	var le *LimitError
	if !errors.As(vendorStatusError(statusResponse(http.StatusTooManyRequests, nil)), &le) {
		t.Fatal("429 is not a LimitError")
	}
	if le.RetryAfter != 0 {
		t.Fatalf("RetryAfter = %s, want 0 when the vendor sent none", le.RetryAfter)
	}
}

// TestVendorStatusErrorOtherStatusesAreNoLimitError keeps every other status on
// the unavailableStatus mapping (503 still retryable, 401 still an auth refusal).
func TestVendorStatusErrorOtherStatusesAreNoLimitError(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusUnauthorized, http.StatusBadRequest, 529} {
		err := vendorStatusError(statusResponse(status, nil))
		var le *LimitError
		if errors.As(err, &le) {
			t.Fatalf("status %d must not be a LimitError", status)
		}
		if err.Error() != unavailableStatus(status).Error() {
			t.Fatalf("status %d: err = %q, want %q", status, err, unavailableStatus(status))
		}
	}
}

// TestVendorClientsReturnLimitErrorOn429 drives every vendor client the translate
// path uses, buffered and streamed, against an upstream answering 429: each must
// hand back a *LimitError with the upstream's Retry-After and headers, so the
// gateway can fail a group over before anything reached the client.
func TestVendorClientsReturnLimitErrorOn429(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.Header().Set("X-Codex-Primary-Used-Percent", "100")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	t.Cleanup(upstream.Close)
	ctx := context.Background()
	req := inference.Request{Model: "m", Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}}
	noop := func(inference.StreamEvent) error { return nil }
	target := func(provider string) routing.Target {
		return routing.Target{Endpoint: upstream.URL, Provider: provider, ProviderModel: "m", Timeout: 5 * time.Second}
	}
	cases := map[string]func() error{
		"anthropic complete": func() error {
			_, err := NewAnthropicClient(upstream.Client()).Complete(ctx, target(routing.ProviderVendorAnthropic), req)
			return err
		},
		"anthropic stream": func() error {
			return NewAnthropicClient(upstream.Client()).CompleteStream(ctx, target(routing.ProviderVendorAnthropic), req, noop)
		},
		"openai responses complete": func() error {
			_, err := NewOpenAIResponsesClient(upstream.Client()).Complete(ctx, target(routing.ProviderVendorOpenAISubscription), req)
			return err
		},
		"openai responses stream": func() error {
			return NewOpenAIResponsesClient(upstream.Client()).CompleteStream(ctx, target(routing.ProviderVendorOpenAISubscription), req, noop)
		},
		"openai compatible complete": func() error {
			_, err := NewOpenAICompatibleClient(upstream.Client()).Complete(ctx, target(routing.ProviderVendorOpenAI), req)
			return err
		},
		"openai compatible stream": func() error {
			return NewOpenAICompatibleClient(upstream.Client()).CompleteStream(ctx, target(routing.ProviderVendorOpenAI), req, noop)
		},
		"gemini complete": func() error {
			_, err := NewGeminiClient(upstream.Client()).Complete(ctx, target(routing.ProviderVendorGemini), req)
			return err
		},
		"gemini stream": func() error {
			return NewGeminiClient(upstream.Client()).CompleteStream(ctx, target(routing.ProviderVendorGemini), req, noop)
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			err := call()
			var le *LimitError
			if !errors.As(err, &le) {
				t.Fatalf("err = %v, want a *LimitError", err)
			}
			if le.Status != http.StatusTooManyRequests || le.RetryAfter != 7*time.Second {
				t.Fatalf("LimitError = {Status %d, RetryAfter %s}, want {429, 7s}", le.Status, le.RetryAfter)
			}
			if le.Header.Get("X-Codex-Primary-Used-Percent") != "100" {
				t.Fatalf("LimitError.Header = %v, want the upstream's headers", le.Header)
			}
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("err = %v, want it to stay ErrUnavailable", err)
			}
		})
	}
}

// TestVendorClientLimitErrorKeepsUpstreamSnippet: the clients that log the
// vendor's own explanation for a refusal keep doing so for a limit error.
func TestVendorClientLimitErrorKeepsUpstreamSnippet(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"usage_limit_reached"}}`)
	}))
	t.Cleanup(upstream.Close)
	req := inference.Request{Model: "m", Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}}}
	err := NewOpenAIResponsesClient(upstream.Client()).CompleteStream(context.Background(), routing.Target{Endpoint: upstream.URL, Provider: routing.ProviderVendorOpenAISubscription, ProviderModel: "m"}, req, func(inference.StreamEvent) error { return nil })
	var le *LimitError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want a *LimitError", err)
	}
	if !strings.Contains(err.Error(), "usage_limit_reached") {
		t.Fatalf("err = %q, want the vendor's snippet in the message", err)
	}
}
