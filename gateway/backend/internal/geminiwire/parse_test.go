package geminiwire

import "testing"

func TestParseResponseTextAndThought(t *testing.T) {
	raw := []byte(`{"candidates":[{"content":{"role":"model","parts":[
		{"text":"thinking...","thought":true},
		{"text":"the answer"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":4,"candidatesTokenCount":5,"thoughtsTokenCount":3,"totalTokenCount":18}}`)
	r, err := ParseResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if r.Text != "the answer" || r.Reasoning != "thinking..." {
		t.Fatalf("text/reasoning = %q / %q", r.Text, r.Reasoning)
	}
	if r.FinishReason != "stop" {
		t.Fatalf("finish = %q", r.FinishReason)
	}
	// InputTokens = prompt (incl. cache); Output = candidates + thoughts; Total kept.
	if r.Usage.InputTokens != 10 || r.Usage.CachedTokens != 4 || r.Usage.OutputTokens != 8 || r.Usage.TotalTokens != 18 {
		t.Fatalf("usage = %+v", r.Usage)
	}
}

func TestParseResponseFunctionCall(t *testing.T) {
	raw := []byte(`{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"get_weather","args":{"city":"NYC"}},"thoughtSignature":"sig1"}]},
		"finishReason":"STOP"}]}`)
	r, err := ParseResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "get_weather" || r.ToolCalls[0].Arguments != `{"city":"NYC"}` {
		t.Fatalf("toolcalls = %+v", r.ToolCalls)
	}
	// finishReason becomes tool_calls when a functionCall is present
	if r.FinishReason != "tool_calls" {
		t.Fatalf("finish = %q", r.FinishReason)
	}
	// id round-trips the name + signature
	name, sig, ok := decodeToolCallID(r.ToolCalls[0].ID)
	if !ok || name != "get_weather" || sig != "sig1" {
		t.Fatalf("id decode = %q/%q/%v", name, sig, ok)
	}
}

func TestParseResponseFinishReasonTable(t *testing.T) {
	cases := map[string]string{
		"STOP": "stop", "MAX_TOKENS": "length", "SAFETY": "content_filter",
		"RECITATION": "content_filter", "PROHIBITED_CONTENT": "content_filter",
		"SPII": "content_filter", "BLOCKLIST": "content_filter",
		"MALFORMED_FUNCTION_CALL": "stop", "OTHER": "stop", "": "",
	}
	for in, want := range cases {
		if got := mapFinishReason(in, false); got != want {
			t.Fatalf("mapFinishReason(%q,false) = %q, want %q", in, got, want)
		}
	}
}

func TestParseResponsePromptBlockedNoCandidates(t *testing.T) {
	raw := []byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`)
	r, err := ParseResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Text != "" || r.FinishReason != "content_filter" {
		t.Fatalf("blocked result = %+v", r)
	}
}

func TestParseResponseDecodeError(t *testing.T) {
	if _, err := ParseResponse([]byte("not json")); err == nil {
		t.Fatal("want decode error")
	}
}

func TestParseStreamChunkDeltasAndDone(t *testing.T) {
	r1, done1, err := ParseStreamChunk([]byte(`{"candidates":[{"content":{"parts":[{"text":"Hel"}]}}]}`), 0)
	if err != nil || done1 || r1.Text != "Hel" {
		t.Fatalf("chunk1 = %+v done=%v err=%v", r1, done1, err)
	}
	r2, done2, err := ParseStreamChunk([]byte(`{"candidates":[{"content":{"parts":[{"text":"lo"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`), 0)
	if err != nil || !done2 || r2.Text != "lo" || r2.FinishReason != "stop" {
		t.Fatalf("chunk2 = %+v done=%v err=%v", r2, done2, err)
	}
	if r2.Usage.InputTokens != 3 || r2.Usage.OutputTokens != 2 || r2.Usage.TotalTokens != 5 {
		t.Fatalf("chunk2 usage = %+v", r2.Usage)
	}
}

func TestParseStreamChunkFunctionCall(t *testing.T) {
	r, done, err := ParseStreamChunk([]byte(`{"candidates":[{"content":{"parts":[
		{"functionCall":{"name":"f","args":{}}}]},"finishReason":"STOP"}]}`), 0)
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "f" || r.ToolCalls[0].Arguments != "{}" {
		t.Fatalf("toolcalls = %+v", r.ToolCalls)
	}
	if r.FinishReason != "tool_calls" {
		t.Fatalf("finish = %q", r.FinishReason)
	}
}

func TestMapFinishReasonFamiliesAndToolCallOverride(t *testing.T) {
	for _, in := range []string{"LANGUAGE", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION", "IMAGE_OTHER"} {
		if got := mapFinishReason(in, false); got != "content_filter" {
			t.Fatalf("mapFinishReason(%q,false) = %q, want content_filter", in, got)
		}
	}
	// A present functionCall turns only the "everything else" family into
	// tool_calls; length and content_filter are never overridden.
	cases := map[string]string{
		"STOP": "tool_calls", "MISSING_THOUGHT_SIGNATURE": "tool_calls", "SOMETHING_NEW": "tool_calls",
		"MAX_TOKENS": "length", "SAFETY": "content_filter", "": "",
	}
	for in, want := range cases {
		if got := mapFinishReason(in, true); got != want {
			t.Fatalf("mapFinishReason(%q,true) = %q, want %q", in, got, want)
		}
	}
}

func TestParseResponseFunctionCallArgsAbsentOrNull(t *testing.T) {
	for name, part := range map[string]string{
		"absent": `{"functionCall":{"name":"f"}}`,
		"null":   `{"functionCall":{"name":"f","args":null}}`,
	} {
		r, err := ParseResponse([]byte(`{"candidates":[{"content":{"parts":[` + part + `]},"finishReason":"STOP"}]}`))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(r.ToolCalls) != 1 || r.ToolCalls[0].Arguments != "{}" {
			t.Fatalf("%s: toolcalls = %+v", name, r.ToolCalls)
		}
	}
}

func TestParseStreamChunkPromptBlockedIsDone(t *testing.T) {
	r, done, err := ParseStreamChunk([]byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`), 0)
	if err != nil || !done || r.FinishReason != "content_filter" {
		t.Fatalf("blocked chunk = %+v done=%v err=%v", r, done, err)
	}
}

func TestParseStreamChunkDecodeError(t *testing.T) {
	if _, _, err := ParseStreamChunk([]byte("not json"), 0); err == nil {
		t.Fatal("want decode error")
	}
}

func TestParseStreamChunkThoughtDeltaAndNoUsage(t *testing.T) {
	r, done, err := ParseStreamChunk([]byte(`{"candidates":[{"content":{"parts":[{"text":"hmm","thought":true}]}}]}`), 0)
	if err != nil || done || r.Reasoning != "hmm" || r.Text != "" {
		t.Fatalf("thought chunk = %+v done=%v err=%v", r, done, err)
	}
	if r.Usage.InputTokens != 0 || r.Usage.OutputTokens != 0 || r.Usage.TotalTokens != 0 || r.Usage.CachedTokens != 0 {
		t.Fatalf("usage without usageMetadata must be zero, got %+v", r.Usage)
	}
}

func TestParseResponseCandidateWithoutContent(t *testing.T) {
	// Real Gemini returns a candidate with no content on a safety stop.
	r, err := ParseResponse([]byte(`{"candidates":[{"finishReason":"SAFETY"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Text != "" || r.Reasoning != "" || len(r.ToolCalls) != 0 || r.FinishReason != "content_filter" {
		t.Fatalf("no-content result = %+v", r)
	}
}

func TestParseResponseEmptyParts(t *testing.T) {
	r, err := ParseResponse([]byte(`{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Text != "" || r.Reasoning != "" || len(r.ToolCalls) != 0 || r.FinishReason != "stop" {
		t.Fatalf("empty-parts result = %+v", r)
	}
}

func TestParseStreamChunkFunctionCallWithoutFinishReason(t *testing.T) {
	// A functionCall chunk before the terminal chunk: the tool call is returned,
	// but the chunk is not done and carries no finish reason (per-chunk mapping).
	r, done, err := ParseStreamChunk([]byte(`{"candidates":[{"content":{"parts":[
		{"functionCall":{"name":"f","args":{"a":1}},"thoughtSignature":"s"}]}}]}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	if done || r.FinishReason != "" {
		t.Fatalf("done=%v finish=%q, want false/empty", done, r.FinishReason)
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "f" || r.ToolCalls[0].Arguments != `{"a":1}` {
		t.Fatalf("toolcalls = %+v", r.ToolCalls)
	}
}

func TestParseResponsePromptBlockedKeepsUsage(t *testing.T) {
	// Usage is still read when there are no candidates (a blocked prompt is billed).
	r, err := ParseResponse([]byte(`{"promptFeedback":{"blockReason":"SAFETY"},
		"usageMetadata":{"promptTokenCount":7,"totalTokenCount":7}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.FinishReason != "content_filter" {
		t.Fatalf("finish = %q", r.FinishReason)
	}
	if r.Usage.InputTokens != 7 || r.Usage.OutputTokens != 0 || r.Usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v", r.Usage)
	}
}

func TestParseStreamChunkWhitespaceFinishReasonIsNotDone(t *testing.T) {
	// done and FinishReason are derived from the same mapped value, so a
	// whitespace-only raw finishReason cannot yield done=true with an empty reason.
	r, done, err := ParseStreamChunk([]byte(`{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"  "}]}`), 0)
	if err != nil || done || r.FinishReason != "" || r.Text != "x" {
		t.Fatalf("chunk = %+v done=%v err=%v", r, done, err)
	}
}
