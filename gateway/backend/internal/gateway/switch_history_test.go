// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
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
			body:      `{"input":[{"type":"reasoning","summary":[]},` + msg + `]}`,
			wantTypes: []string{"message"}, wantDropped: 1,
		},
		{
			name: "openai drops non-string enc", flavor: flavorResp, class: switchClassOpenAIVendor, key: "input",
			body:      `{"input":[{"type":"reasoning","encrypted_content":42},` + msg + `]}`,
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
