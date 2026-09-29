// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"reflect"
	"slices"
	"testing"
	"time"
)

// commentedStream is an upstream SSE body with three comment lines between its
// data frames: the agent router's keepalive before the first event, another one
// between two deltas, and a bare ':' before the usage frame.
const commentedStream = ": keepalive\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
	": keepalive\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n" +
	":\n\n" +
	"data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

// TestCompleteStreamReportsEachCommentLineToTheActivityHook pins the hook's
// contract: it fires once per comment line, at the point in the stream where the
// line arrives, and never for a data line or a blank line. The events emit sees
// are identical with and without the hook, so a caller that installs it changes
// nothing about the stream itself.
func TestCompleteStreamReportsEachCommentLineToTheActivityHook(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, commentedStream)
	}))
	defer upstream.Close()
	client := NewOpenAICompatibleClient(upstream.Client())
	target := routing.Target{Endpoint: upstream.URL, ProviderModel: "m", Timeout: 5 * time.Second}

	run := func(ctx context.Context, order *[]string) []inference.StreamEvent {
		t.Helper()
		var events []inference.StreamEvent
		err := client.CompleteStream(ctx, target, streamTestRequest(), func(ev inference.StreamEvent) error {
			events = append(events, ev)
			*order = append(*order, string(ev.Type))
			return nil
		})
		if err != nil {
			t.Fatalf("CompleteStream returned %v", err)
		}
		return events
	}

	var plainOrder []string
	without := run(context.Background(), &plainOrder)
	var hookedOrder []string
	with := run(WithStreamActivity(context.Background(), func() {
		hookedOrder = append(hookedOrder, "comment")
	}), &hookedOrder)

	if len(without) != 3 || without[0].Text != "Hel" || without[1].Text != "lo" ||
		without[2].Type != inference.StreamEventCompleted || without[2].Usage == nil || without[2].Usage.OutputTokens != 2 {
		t.Fatalf("events without the hook = %#v, want the deltas Hel, lo and a Completed event with 2 output tokens", without)
	}
	if !reflect.DeepEqual(with, without) {
		t.Fatalf("events with the hook = %#v, want the same events as without it: %#v", with, without)
	}
	wantOrder := []string{"comment", "text_delta", "comment", "text_delta", "comment", "completed"}
	if !slices.Equal(hookedOrder, wantOrder) {
		t.Fatalf("hook and emit order = %v, want %v (one hook call per comment line, where it arrives)", hookedOrder, wantOrder)
	}
}

// TestStreamActivityFromABareContextIsNil pins the unset case: no hook is
// reported for a context that never carried one, and installing a nil hook
// leaves the context unchanged.
func TestStreamActivityFromABareContextIsNil(t *testing.T) {
	ctx := context.Background()
	if StreamActivityFrom(ctx) != nil {
		t.Fatal("StreamActivityFrom on a bare context returned a hook, want nil")
	}
	if WithStreamActivity(ctx, nil) != ctx {
		t.Fatal("WithStreamActivity with a nil hook returned a new context, want ctx unchanged")
	}
}
