// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"strings"
	"testing"
)

func TestStrataPassthroughMediaRefused(t *testing.T) {
	msg := func(block string) string {
		return `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[` + block + `]}]}`
	}
	cases := []struct {
		name    string
		body    string
		refused bool
	}{
		{"anthropic base64", msg(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0="}}`), false},
		{"anthropic base64 beside a stray path", msg(`{"type":"image","source":{"type":"base64","data":"x","path":"/etc/passwd"}}`), true},
		{"anthropic base64 beside a file url", msg(`{"type":"image","source":{"type":"base64","data":"x","url":"file:///etc/passwd"}}`), true},
		{"anthropic base64 beside a non-string url", msg(`{"type":"image","source":{"type":"base64","data":"x","url":7}}`), true},
		{"anthropic base64 beside an https url", msg(`{"type":"image","source":{"type":"base64","data":"x","url":"https://example.test/a.png"}}`), false},
		{"anthropic https url", msg(`{"type":"image","source":{"type":"url","url":"https://images.example.test/x.png"}}`), false},
		{"anthropic http url", msg(`{"type":"image","source":{"type":"url","url":"http://images.example.test/x.png"}}`), false},
		{"anthropic data url", msg(`{"type":"image","source":{"type":"url","url":"data:image/png;base64,iVBORw0="}}`), false},
		{"anthropic bare path url", msg(`{"type":"image","source":{"type":"url","url":"/etc/passwd"}}`), true},
		{"anthropic file url", msg(`{"type":"image","source":{"type":"url","url":"file:///etc/passwd"}}`), true},
		{"anthropic upper file url", msg(`{"type":"image","source":{"type":"url","url":"FILE:///etc/passwd"}}`), true},
		{"anthropic odd-cased http", msg(`{"type":"image","source":{"type":"url","url":"HTTP://images.example.test/x.png"}}`), true},
		{"anthropic odd-cased data", msg(`{"type":"image","source":{"type":"url","url":"DATA:image/png;base64,iVBORw0="}}`), true},
		{"anthropic leading space", msg(`{"type":"image","source":{"type":"url","url":" https://x/y.png"}}`), true},
		{"anthropic path source", msg(`{"type":"image","source":{"type":"path","path":"/etc/passwd"}}`), true},
		{"anthropic path key without type", msg(`{"type":"image","source":{"path":"secret.png"}}`), true},
		{"anthropic url source without a url", msg(`{"type":"image","source":{"type":"url"}}`), true},
		{"anthropic non-string url", msg(`{"type":"image","source":{"type":"url","url":7}}`), true},
		{"anthropic string source", msg(`{"type":"image","source":"/etc/passwd"}`), true},
		{"anthropic document path", msg(`{"type":"document","source":{"type":"url","url":"/etc/passwd"}}`), true},
		{"anthropic document text source", msg(`{"type":"document","source":{"type":"text","media_type":"text/plain","data":"hi"}}`), false},
		{"anthropic tool_result nested path", msg(`{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{"type":"url","url":"/etc/passwd"}}]}`), true},
		{"anthropic tool_result nested base64", msg(`{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{"type":"base64","data":"x"}}]}`), false},
		{"anthropic text mentioning a path", msg(`{"type":"text","text":"/etc/passwd file:///x"}`), false},
		{"responses string image_url path", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"file:///etc/passwd"}]}]}`, true},
		{"responses bare path", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"/etc/passwd"}]}]}`, true},
		{"responses object image_url path", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":{"url":"/etc/passwd"}}]}]}`, true},
		{"responses https", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://images.example.test/x.png"}]}]}`, false},
		{"responses data", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,iVBORw0="}]}]}`, false},
		{"responses odd-cased https", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"HTTPS://x/y.png"}]}]}`, true},
		{"responses function output nested", `{"model":"m","input":[{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","image_url":"/etc/passwd"}]}]}`, true},
		{"responses chat-style image_url part", `{"model":"m","input":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"/etc/passwd"}}]}]}`, true},
		{"responses text only", `{"model":"m","input":"look at /etc/passwd"}`, false},
		{"responses input_image without a reference", `{"model":"m","input":[{"role":"user","content":[{"type":"input_image"}]}]}`, false},
		{"malformed json", `{"model":"m","messages":[{"content":[{"type":"image","source":{"url":"/etc/passwd"`, false},
		{"empty body", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := strataPassthroughMediaRefused([]byte(tc.body)); got != tc.refused {
				t.Fatalf("strataPassthroughMediaRefused = %v, want %v", got, tc.refused)
			}
		})
	}
}

// A raw /v1/messages or /v1/responses body to a strata application is checked before it
// is relayed: a model-host image reference is refused 400 chat.media_url_unsupported
// and never reaches the upstream, while base64 and https images still pass.
func TestStrataPassthroughRefusesHostPathImages(t *testing.T) {
	messages := func(source string) string {
		return `{"model":"strata-model","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"image","source":` + source + `}]}]}`
	}
	responses := func(imageURL string) string {
		return `{"model":"strata-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"x"},{"type":"input_image","image_url":"` + imageURL + `"}]}]}`
	}
	cases := []struct {
		name, path, body, wantPath string
		refused                    bool
	}{
		{"messages url path", "/v1/messages", messages(`{"type":"url","url":"/etc/passwd"}`), "", true},
		{"messages path source", "/v1/messages", messages(`{"type":"path","path":"/etc/passwd"}`), "", true},
		{"messages anthropic prefix path", "/anthropic/v1/messages", messages(`{"type":"url","url":"file:///etc/passwd"}`), "", true},
		{"responses file url", "/v1/responses", responses("file:///etc/passwd"), "", true},
		{"messages base64", "/v1/messages", messages(`{"type":"base64","media_type":"image/png","data":"iVBORw0="}`), "/v1/messages", false},
		{"messages https", "/v1/messages", messages(`{"type":"url","url":"https://images.example.test/x.png"}`), "/v1/messages", false},
		{"responses https", "/v1/responses", responses("https://images.example.test/x.png"), "/v1/responses", false},
		{"responses data", "/v1/responses", responses("data:image/png;base64,iVBORw0="), "/v1/responses", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newWireUpstream(t)
			srv := newStrataTestServer(t, up.URL)
			rec := postImagesWithHeaders(t, srv, tc.path, tc.body, map[string]string{"Authorization": "Bearer dev-secret"})
			if !tc.refused {
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
				}
				if got := calls.all(); len(got) != 1 || got[0].path != tc.wantPath {
					t.Fatalf("upstream calls = %+v, want one %s relay", got, tc.wantPath)
				}
				return
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "chat.media_url_unsupported")
			if got := calls.all(); len(got) != 0 {
				t.Fatalf("upstream calls = %+v, want none", got)
			}
			if strings.Contains(rec.Body.String(), "passwd") {
				t.Fatalf("refusal echoes the value: %s", rec.Body.String())
			}
		})
	}
}

// Only strata is scanned: a vllm target in passthrough mode relays the same body raw.
func TestVLLMPassthroughWithPathImageStillRelays(t *testing.T) {
	up, calls := newWireUpstream(t)
	spec := autoOmniPassthroughSpec()
	spec.Binary = "/opt/vllm/bin/vllm"
	srv := newWireSpecTestServer(t, up.URL, spec)
	body := `{"model":"agent-omni","max_tokens":16,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"/etc/passwd"}}]}]}`
	rec := postImagesWithHeaders(t, srv, "/v1/messages", body, map[string]string{"Authorization": "Bearer dev-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := calls.all(); len(got) != 1 || got[0].path != "/v1/messages" {
		t.Fatalf("upstream calls = %+v, want one raw /v1/messages relay", got)
	}
}
