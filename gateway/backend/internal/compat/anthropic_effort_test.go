// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package compat

import (
	"errors"
	"op-ai-gateway/internal/inference"
	"testing"
)

// effortBody builds a minimal Messages body carrying the given raw JSON value as
// `output_config` (the empty string omits the key entirely).
func effortBody(outputConfig string) []byte {
	extra := ""
	if outputConfig != "" {
		extra = `,"output_config":` + outputConfig
	}
	return []byte(`{"model":"claude-compatible-coder","messages":[{"role":"user","content":"hi"}]` + extra + `}`)
}

func TestParseAnthropicMessagesWithOptionsReadsEffortWhenEnabled(t *testing.T) {
	enabled := ParseAnthropicOptions{EffortTranslationEnabled: true}
	cases := []struct {
		name string
		body string
		want string
	}{
		{"low", `{"effort":"low"}`, "low"},
		{"verbatim, no enum", `{"effort":"xhigh"}`, "xhigh"},
		{"trailing space trimmed", `{"effort":"high "}`, "high"},
		{"blank stays unset", `{"effort":"   "}`, ""},
		{"null effort stays unset", `{"effort":null}`, ""},
		{"absent effort stays unset", `{"format":{"type":"json_schema"}}`, ""},
		{"null output_config stays unset", `null`, ""},
		{"no output_config at all", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := ParseAnthropicMessagesWithOptions(effortBody(tc.body), enabled)
			if err != nil {
				t.Fatalf("ParseAnthropicMessagesWithOptions returned %v", err)
			}
			if req.ReasoningEffort != tc.want {
				t.Fatalf("ReasoningEffort = %q, want %q", req.ReasoningEffort, tc.want)
			}
			if req.ReasoningEffortViaTemplateKwargs {
				t.Fatal("the parser must never set the template-kwargs directive; that is the gateway policy's job")
			}
		})
	}
}

func TestParseAnthropicMessagesWithOptionsRejectsMalformedEffortWhenEnabled(t *testing.T) {
	enabled := ParseAnthropicOptions{EffortTranslationEnabled: true}
	for name, body := range map[string]string{
		"numeric effort":      `{"effort":3}`,
		"object effort":       `{"effort":{"level":"low"}}`,
		"boolean effort":      `{"effort":true}`,
		"array effort":        `{"effort":["low"]}`,
		"non-object config":   `"low"`,
		"array output_config": `["low"]`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseAnthropicMessagesWithOptions(effortBody(body), enabled)
			var ie *inference.Error
			if !errors.As(err, &ie) {
				t.Fatalf("err = %v (%T), want *inference.Error", err, err)
			}
			if ie.Code != "messages.invalid_output_config" {
				t.Fatalf("Code = %q, want messages.invalid_output_config", ie.Code)
			}
		})
	}
}

// TestParseAnthropicMessagesIgnoresOutputConfigWhenDisabled is the regression
// guard for the token-flag-off path: output_config is not inspected AT ALL, so a
// request that carries one (well-formed, effort-bearing, or outright malformed)
// parses exactly as it did before #230 -- never newly rejected, no effort read.
func TestParseAnthropicMessagesIgnoresOutputConfigWhenDisabled(t *testing.T) {
	bodies := map[string]string{
		"string effort":       `{"effort":"low"}`,
		"non-string effort":   `{"effort":3}`,
		"object effort":       `{"effort":{"level":"low"}}`,
		"non-object config":   `"low"`,
		"array output_config": `["low"]`,
	}
	parsers := map[string]func([]byte) (inference.Request, error){
		"ParseAnthropicMessages": ParseAnthropicMessages,
		"WithOptions(disabled)": func(raw []byte) (inference.Request, error) {
			return ParseAnthropicMessagesWithOptions(raw, ParseAnthropicOptions{})
		},
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			for label, parse := range parsers {
				req, err := parse(effortBody(body))
				if err != nil {
					t.Fatalf("%s rejected a disabled request carrying output_config %s: %v", label, body, err)
				}
				if req.ReasoningEffort != "" {
					t.Fatalf("%s: ReasoningEffort = %q, want empty (flag off)", label, req.ReasoningEffort)
				}
			}
		})
	}
}

// TestCountAnthropicTokensIgnoresOutputConfig pins that the token counter stays
// on the disabled path: a malformed output_config must not newly fail it either.
func TestCountAnthropicTokensIgnoresOutputConfig(t *testing.T) {
	if _, err := CountAnthropicTokens(effortBody(`{"effort":3}`)); err != nil {
		t.Fatalf("CountAnthropicTokens rejected a malformed output_config: %v", err)
	}
}
