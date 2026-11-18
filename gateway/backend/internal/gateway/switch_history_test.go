// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"testing"
)

const (
	flavorResp = inference.APIFlavorOpenAIResponses
	flavorMsgs = inference.APIFlavorAnthropicMessages
)

// decodeList decodes body[key] as a list of generic objects.
func decodeList(t *testing.T, body []byte, key string) []map[string]any {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, body)
	}
	var items []map[string]any
	if err := json.Unmarshal(top[key], &items); err != nil {
		t.Fatalf("%s is not a list: %v", key, err)
	}
	return items
}

func itemTypes(items []map[string]any) []string {
	var out []string
	for _, it := range items {
		s, _ := it["type"].(string)
		out = append(out, s)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSanitizeSwitchHistoryRules(t *testing.T) {
	const msg = `{"type":"message","role":"user","content":"hi"}`
	cases := []struct {
		name         string
		flavor       string
		class        switchTargetClass
		key          string
		body         string
		wantTypes    []string
		wantDropped  int
		wantStripped int
		check        func(t *testing.T, items []map[string]any)
	}{
		{
			name: "openai drops minted reasoning_text item with empty enc", flavor: flavorResp, class: switchClassOpenAIVendor, key: "input",
			body:      `{"input":[` + msg + `,{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"x"}],"encrypted_content":""}]}`,
			wantTypes: []string{"message"}, wantDropped: 1,
		},
		{
			name: "openai drops null enc", flavor: flavorResp, class: switchClassOpenAIVendor, key: "input",
			body:      `{"input":[{"type":"reasoning","encrypted_content":null,"summary":[]},` + msg + `]}`,
			wantTypes: []string{"message"}, wantDropped: 1,
		},
		{
			name: "openai drops missing enc", flavor: flavorResp, class: switchClassOpenAIVendor, key: "input",
			body:      `{"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"x"}]},` + msg + `]}`,
			wantTypes: []string{"message"}, wantDropped: 1,
		},
		{
			name: "openai drops non-string enc", flavor: flavorResp, class: switchClassOpenAIVendor, key: "input",
			body:      `{"input":[{"type":"reasoning","encrypted_content":42,"content":[{"type":"reasoning_text","text":"x"}]},` + msg + `]}`,
			wantTypes: []string{"message"}, wantDropped: 1,
		},
		{
			name: "openai strips content of own item", flavor: flavorResp, class: switchClassOpenAIVendor, key: "input",
			body:      `{"input":[{"type":"reasoning","id":"rs_1","encrypted_content":"gAAA","summary":[],"content":[{"type":"reasoning_text","text":"x"}]}]}`,
			wantTypes: []string{"reasoning"}, wantStripped: 1,
			check: func(t *testing.T, items []map[string]any) {
				if _, ok := items[0]["content"]; ok {
					t.Fatalf("content key must be removed: %v", items[0])
				}
				if items[0]["encrypted_content"] != "gAAA" || items[0]["id"] != "rs_1" {
					t.Fatalf("other fields must survive: %v", items[0])
				}
			},
		},
		{
			name: "openai keeps own item with empty content", flavor: flavorResp, class: switchClassOpenAIVendor, key: "input",
			body:      `{"input":[{"type":"reasoning","encrypted_content":"gAAA","summary":[],"content":[]}]}`,
			wantTypes: []string{"reasoning"},
		},
		{
			name: "local drops encrypted-only item", flavor: flavorResp, class: switchClassLocal, key: "input",
			body:      `{"input":[{"type":"reasoning","encrypted_content":"gAAA","summary":[]},` + msg + `]}`,
			wantTypes: []string{"message"}, wantDropped: 1,
		},
		{
			name: "local strips encrypted_content from item with text", flavor: flavorResp, class: switchClassLocal, key: "input",
			body:      `{"input":[{"type":"reasoning","encrypted_content":"gAAA","summary":[{"type":"summary_text","text":"thought"}]}]}`,
			wantTypes: []string{"reasoning"}, wantStripped: 1,
			check: func(t *testing.T, items []map[string]any) {
				if _, ok := items[0]["encrypted_content"]; ok {
					t.Fatalf("encrypted_content must be removed: %v", items[0])
				}
			},
		},
		{
			name: "local keeps text-only item", flavor: flavorResp, class: switchClassLocal, key: "input",
			body:      `{"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"thought"}]}]}`,
			wantTypes: []string{"reasoning"},
		},
		{
			name: "anthropic drops empty and missing signature", flavor: flavorMsgs, class: switchClassAnthropicVendor, key: "messages",
			body: `{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":[` +
				`{"type":"thinking","thinking":"a","signature":""},{"type":"thinking","thinking":"b"},{"type":"text","text":"t"}]}]}`,
			wantTypes: []string{"", ""}, wantDropped: 2,
			check: func(t *testing.T, items []map[string]any) {
				blocks := items[1]["content"].([]any)
				if len(blocks) != 1 || blocks[0].(map[string]any)["type"] != "text" {
					t.Fatalf("only the text block must remain: %v", blocks)
				}
			},
		},
		{
			name: "anthropic keeps signed thinking and redacted_thinking", flavor: flavorMsgs, class: switchClassAnthropicVendor, key: "messages",
			body: `{"messages":[{"role":"assistant","content":[` +
				`{"type":"thinking","thinking":"a","signature":"sig"},{"type":"redacted_thinking","data":"d"}]}]}`,
			wantTypes: []string{""},
		},
		{
			name: "local messages unchanged", flavor: flavorMsgs, class: switchClassLocal, key: "messages",
			body:      `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"a","signature":""}]}]}`,
			wantTypes: []string{""},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.body)
			out, dropped, stripped := sanitizeSwitchHistory(raw, tc.flavor, tc.class)
			if dropped != tc.wantDropped || stripped != tc.wantStripped {
				t.Fatalf("dropped/stripped = %d/%d, want %d/%d", dropped, stripped, tc.wantDropped, tc.wantStripped)
			}
			items := decodeList(t, out, tc.key)
			if got := itemTypes(items); !equalStrings(got, tc.wantTypes) {
				t.Fatalf("types = %v, want %v", got, tc.wantTypes)
			}
			if tc.check != nil {
				tc.check(t, items)
			}
			if string(raw) != tc.body {
				t.Fatal("input slice was modified")
			}
		})
	}
}

func TestSanitizeSwitchHistoryNoOpReturnsSameSlice(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		flavor string
		class  switchTargetClass
	}{
		{"string input", `{"input":"reasoning please"}`, flavorResp, switchClassOpenAIVendor},
		{"missing input", `{"reasoning":{"effort":"high"}}`, flavorResp, switchClassLocal},
		{"null input", `{"input":null,"reasoning":{}}`, flavorResp, switchClassOpenAIVendor},
		{"non-json", `not json "reasoning"`, flavorResp, switchClassOpenAIVendor},
		{"large number", `{"seed":12345678901234567890,"input":[{"type":"reasoning","encrypted_content":"x"}]}`, flavorResp, switchClassOpenAIVendor},
		{"other flavor", `{"input":[{"type":"reasoning","encrypted_content":""}]}`, "openai_chat", switchClassOpenAIVendor},
		{"class none", `{"input":[{"type":"reasoning","encrypted_content":""}]}`, flavorResp, switchClassNone},
		{"anthropic no thinking", `{"messages":[{"role":"user","content":"x"}]}`, flavorMsgs, switchClassAnthropicVendor},
		{"messages string content", `{"messages":[{"role":"assistant","content":"thinking"}]}`, flavorMsgs, switchClassAnthropicVendor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.body)
			out, dropped, stripped := sanitizeSwitchHistory(raw, tc.flavor, tc.class)
			if dropped+stripped != 0 {
				t.Fatalf("counts = %d/%d, want 0", dropped, stripped)
			}
			if len(out) != len(raw) || &out[0] != &raw[0] {
				t.Fatalf("expected the same slice back")
			}
		})
	}
}

func TestSanitizeSwitchHistoryEditPreservesLargeNumbers(t *testing.T) {
	raw := []byte(`{"seed":12345678901234567890,"input":[{"type":"reasoning","encrypted_content":""},{"type":"message","n":1.50}]}`)
	out, dropped, _ := sanitizeSwitchHistory(raw, flavorResp, switchClassOpenAIVendor)
	if dropped != 1 {
		t.Fatalf("dropped = %d", dropped)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	if string(top["seed"]) != "12345678901234567890" {
		t.Fatalf("seed = %s", top["seed"])
	}
	if got := string(top["input"]); got != `[{"type":"message","n":1.50}]` {
		t.Fatalf("input = %s", got)
	}
}

// Review Focus 1: a ChatGPT-subscription session that never switched must be
// forwarded untouched.
func TestSanitizeSwitchHistoryOwnChatGPTSessionUntouched(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-luna","reasoning":{"effort":"high"},"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"reasoning","id":"rs_1","summary":[],"content":[],"encrypted_content":"gAAAAAB"},` +
		`{"type":"function_call","call_id":"c1","name":"sh","arguments":"{}"}]}`)
	out, dropped, stripped := sanitizeSwitchHistory(raw, flavorResp, switchClassOpenAIVendor)
	if dropped+stripped != 0 || &out[0] != &raw[0] || len(out) != len(raw) {
		t.Fatalf("own session must be returned unchanged: %d/%d", dropped, stripped)
	}
}

// Review Focus 2: removing the only block of an assistant turn removes the turn
// and the users around it merge into one.
func TestSanitizeSwitchHistoryMergesUsersAfterEmptiedTurn(t *testing.T) {
	raw := []byte(`{"messages":[` +
		`{"role":"user","content":"first"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":""}]},` +
		`{"role":"user","content":[{"type":"text","text":"second"},{"type":"image","source":{}}]}]}`)
	out, dropped, _ := sanitizeSwitchHistory(raw, flavorMsgs, switchClassAnthropicVendor)
	if dropped != 1 {
		t.Fatalf("dropped = %d", dropped)
	}
	msgs := decodeList(t, out, "messages")
	if len(msgs) != 1 || msgs[0]["role"] != "user" {
		t.Fatalf("want one user turn, got %v", msgs)
	}
	blocks := msgs[0]["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %v", blocks)
	}
	first := blocks[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "first" {
		t.Fatalf("string content must become a text block first: %v", first)
	}
	if blocks[1].(map[string]any)["text"] != "second" || blocks[2].(map[string]any)["type"] != "image" {
		t.Fatalf("order not preserved: %v", blocks)
	}
}

func TestSwitchTargetClassOf(t *testing.T) {
	cases := map[string]switchTargetClass{
		routing.ProviderVendorOpenAI:             switchClassOpenAIVendor,
		routing.ProviderVendorOpenAISubscription: switchClassOpenAIVendor,
		routing.ProviderVendorAnthropic:          switchClassAnthropicVendor,
		routing.ProviderVendorGemini:             switchClassNone,
		routing.ProviderVllmSR:                   switchClassLocal,
		"llama_cpp":                              switchClassLocal,
	}
	for provider, want := range cases {
		if got := switchTargetClassOf(routing.Target{Provider: provider}); got != want {
			t.Errorf("%s: class %d, want %d", provider, got, want)
		}
	}
}

func TestSanitizeSwitchHistoryGateSkipsParseForOwnSession(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-luna","reasoning":{"effort":"high"},"input":[` +
		`{"type":"reasoning","id":"rs_1","summary":[],"content":[],"encrypted_content":"gAAAAABpZ3x9Qk1"},` +
		`{"type":"function_call","call_id":"c1","name":"sh","arguments":"{}"},` +
		`{"type":"reasoning","id":"rs_2","summary":[],"content":[],"encrypted_content":"gAAAAABpZ3yTT0Q"},` +
		`{"type":"reasoning","id":"rs_3","summary":[],"content":[],"encrypted_content":"gAAAAABpZ3zZZ9w"}]}`)
	before := switchHistoryParses.Load()
	out, dropped, stripped := sanitizeSwitchHistory(raw, flavorResp, switchClassOpenAIVendor)
	if dropped+stripped != 0 || &out[0] != &raw[0] {
		t.Fatalf("want same slice, got %d/%d", dropped, stripped)
	}
	if n := switchHistoryParses.Load() - before; n != 0 {
		t.Fatalf("own session must not be parsed, parses = %d", n)
	}
	// Local messages: no rules, no scan.
	msgs := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","signature":""}]}]}`)
	if out, _, _ := sanitizeSwitchHistory(msgs, flavorMsgs, switchClassLocal); &out[0] != &msgs[0] {
		t.Fatal("local messages must return raw")
	}
	if n := switchHistoryParses.Load() - before; n != 0 {
		t.Fatalf("parses = %d", n)
	}
}

func TestSanitizeSwitchHistoryGateToleratesWhitespace(t *testing.T) {
	out, dropped, _ := sanitizeSwitchHistory(
		[]byte("{\"input\": [{\"type\": \"reasoning\", \"encrypted_content\" \n: \t\"\"}, {\"type\":\"message\"}]}"),
		flavorResp, switchClassOpenAIVendor)
	if dropped != 1 || len(decodeList(t, out, "input")) != 1 {
		t.Fatalf("whitespace-formatted minted item must be dropped: %d %s", dropped, out)
	}
	out, dropped, _ = sanitizeSwitchHistory(
		[]byte(`{"messages": [{"role":"assistant","content":[{"type":"thinking","signature" : null}]}]}`),
		flavorMsgs, switchClassAnthropicVendor)
	if dropped != 1 || len(decodeList(t, out, "messages")) != 0 {
		t.Fatalf("null signature must be dropped: %d %s", dropped, out)
	}
}

// The byte gate trips but the body cannot be rewritten: the same slice comes back.
func TestSanitizeSwitchHistoryGateHitButUnrewritableIsNoOp(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		flavor string
		class  switchTargetClass
	}{
		{"string input with trigger", `{"input":"a reasoning_text b"}`, flavorResp, switchClassOpenAIVendor},
		{"non-json with trigger", `oops "reasoning_text"`, flavorResp, switchClassOpenAIVendor},
		{"null input, trigger elsewhere", `{"input":null,"note":"reasoning_text"}`, flavorResp, switchClassOpenAIVendor},
		{"local string input with trigger", `{"input":"x","note":"encrypted_content"}`, flavorResp, switchClassLocal},
		{"messages as string", `{"messages":"x","signature":""}`, flavorMsgs, switchClassAnthropicVendor},
		{"string content, signature elsewhere", `{"messages":[{"role":"assistant","content":"hi"}],"signature":""}`, flavorMsgs, switchClassAnthropicVendor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.body)
			out, dropped, stripped := sanitizeSwitchHistory(raw, tc.flavor, tc.class)
			if dropped+stripped != 0 || len(out) != len(raw) || &out[0] != &raw[0] {
				t.Fatalf("expected same slice and zero counts, got %d/%d", dropped, stripped)
			}
		})
	}
}

func TestSanitizeSwitchHistoryLocalCodexBodyWithReasoningConfigNotParsed(t *testing.T) {
	raw := []byte(`{"model":"qwen","reasoning":{"effort":"low"},"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	before := switchHistoryParses.Load()
	out, dropped, stripped := sanitizeSwitchHistory(raw, flavorResp, switchClassLocal)
	if dropped+stripped != 0 || &out[0] != &raw[0] {
		t.Fatalf("want same slice, got %d/%d", dropped, stripped)
	}
	if n := switchHistoryParses.Load() - before; n != 0 {
		t.Fatalf("parses = %d, want 0", n)
	}
}

func hashOf(s string) blobHash { return sha256.Sum256([]byte(s)) }

func TestOpaqueBlobHashes(t *testing.T) {
	resp := []byte(`{"input":[{"type":"message","role":"user","content":"x"},{"type":"reasoning","encrypted_content":"AAA"},{"type":"reasoning","encrypted_content":""},{"type":"reasoning","summary":[]}]}`)
	got := opaqueBlobHashes(resp, flavorResp)
	if len(got) != 1 || got[0] != hashOf("AAA") {
		t.Fatalf("responses hashes = %v", got)
	}
	msgs := []byte(`{"messages":[{"role":"user","content":[{"type":"thinking","signature":"U"}]},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"S1"},{"type":"redacted_thinking","data":"D1"},{"type":"thinking","signature":""},{"type":"text","text":"hi"}]},` +
		`{"role":"assistant","content":"plain"}]}`)
	got = opaqueBlobHashes(msgs, flavorMsgs)
	if len(got) != 2 || got[0] != hashOf("S1") || got[1] != hashOf("D1") {
		t.Fatalf("messages hashes = %v", got)
	}
	if opaqueBlobHashes([]byte(`not json`), flavorMsgs) != nil || opaqueBlobHashes(resp, "other") != nil {
		t.Fatal("bad body / unknown flavor must yield nil")
	}
}

func TestRemoveBlobsDropsOnlyListedBlobs(t *testing.T) {
	drop := map[blobHash]struct{}{hashOf("OLD"): {}}

	resp := []byte(`{"model":"m","input":[{"type":"reasoning","encrypted_content":"OLD"},{"type":"message","role":"user","content":"x"},{"type":"reasoning","encrypted_content":"NEW"}]}`)
	out, n := removeBlobs(resp, flavorResp, drop)
	if n != 1 {
		t.Fatalf("removed = %d", n)
	}
	items := decodeList(t, out, "input")
	if len(items) != 2 || items[1]["encrypted_content"] != "NEW" {
		t.Fatalf("items = %v", items)
	}

	msgs := []byte(`{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":[{"type":"thinking","signature":"OLD"}]},` +
		`{"role":"user","content":"b"},{"role":"assistant","content":[{"type":"redacted_thinking","data":"NEW"},{"type":"thinking","signature":"OLD"},{"type":"text","text":"t"}]}]}`)
	out, n = removeBlobs(msgs, flavorMsgs, drop)
	if n != 2 {
		t.Fatalf("removed = %d", n)
	}
	ms := decodeList(t, out, "messages")
	if len(ms) != 2 {
		t.Fatalf("want empty turn dropped and users merged, got %v", ms)
	}
	if c := ms[0]["content"].([]any); len(c) != 2 {
		t.Fatalf("merged user content = %v", c)
	}
	if c := ms[1]["content"].([]any); len(c) != 2 {
		t.Fatalf("assistant content = %v", c)
	}
}

func TestRemoveBlobsNoMatchReturnsSameSlice(t *testing.T) {
	raw := []byte(`{"input":[{"type":"reasoning","encrypted_content":"NEW"}]}`)
	orig := append([]byte(nil), raw...)
	for _, drop := range []map[blobHash]struct{}{nil, {hashOf("OLD"): {}}} {
		out, n := removeBlobs(raw, flavorResp, drop)
		if n != 0 || &out[0] != &raw[0] || len(out) != len(raw) {
			t.Fatalf("no-match must return the same slice (n=%d)", n)
		}
	}
	if !bytes.Equal(raw, orig) {
		t.Fatal("input was written to")
	}
}

func TestIsForeignReasoningRefusal(t *testing.T) {
	o4 := `{"error":{"message":"The encrypted content for item rs_1 could not be verified.","type":"invalid_request_error","param":null,"code":"invalid_encrypted_content"}}`
	a2 := "{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"messages.1.content.0: Invalid `signature` in `thinking` block\"}}"
	o2 := `{"error":{"message":"Invalid 'input[3].content': array too long.","type":"invalid_request_error","param":"input[3].content","code":"array_above_max_length"}}`
	mt := `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: must be greater than 0"}}`
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"O4", 400, o4, true},
		{"A2/A3", 400, a2, true},
		{"O2 array_above_max_length", 400, o2, false},
		{"max_tokens 400", 400, mt, false},
		{"429 with O4 body", http.StatusTooManyRequests, o4, false},
		{"500 with A2 body", 500, a2, false},
		{"raw substring only", 400, "not json invalid_encrypted_content", false},
		{"empty", 400, "", false},
	}
	for _, c := range cases {
		if got := isForeignReasoningRefusal(c.status, []byte(c.body)); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// An unswitched local session replays its own reasoning items (llama.cpp:
// reasoning_text plus "encrypted_content":"", vLLM likely null). Neither shape
// may be parsed or rewritten.
func TestSanitizeSwitchHistoryLocalOwnReasoningShapesNotParsed(t *testing.T) {
	const tail = `{"type":"message","role":"user","content":"next"}]}`
	shapes := map[string]string{
		"llama.cpp empty":   `{"input":[{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"x"}],"summary":[],"encrypted_content":""},` + tail,
		"vllm null":         `{"input":[{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"x"}],"summary":[],"encrypted_content":null},` + tail,
		"spaced empty":      "{\"input\":[{\"type\":\"reasoning\",\"encrypted_content\" : \"\",\"summary\":[]}," + tail,
		"text only, no key": `{"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"x"}]},` + tail,
	}
	for name, body := range shapes {
		t.Run(name, func(t *testing.T) {
			raw := []byte(body)
			before := switchHistoryParses.Load()
			out, dropped, stripped := sanitizeSwitchHistory(raw, flavorResp, switchClassLocal)
			if dropped+stripped != 0 || &out[0] != &raw[0] || len(out) != len(raw) {
				t.Fatalf("want same slice, got %d/%d", dropped, stripped)
			}
			if n := switchHistoryParses.Load() - before; n != 0 {
				t.Fatalf("parses = %d, want 0", n)
			}
		})
	}
}

// A vendor-shaped item (non-empty blob) next to an own item is still handled.
func TestSanitizeSwitchHistoryLocalVendorBlobStillHandled(t *testing.T) {
	raw := []byte(`{"input":[{"type":"reasoning","encrypted_content":"gAAA","summary":[]},` +
		`{"type":"reasoning","encrypted_content":"gAAB","summary":[{"type":"summary_text","text":"t"}]},` +
		`{"type":"reasoning","encrypted_content":"","content":[{"type":"reasoning_text","text":"own"}]}]}`)
	out, dropped, stripped := sanitizeSwitchHistory(raw, flavorResp, switchClassLocal)
	if dropped != 1 || stripped != 1 {
		t.Fatalf("dropped/stripped = %d/%d, want 1/1", dropped, stripped)
	}
	items := decodeList(t, out, "input")
	if len(items) != 2 {
		t.Fatalf("unexpected items: %v", items)
	}
	if _, ok := items[0]["encrypted_content"]; ok {
		t.Fatalf("blob must be stripped: %v", items[0])
	}
}
