// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestBuildReportRedactsEnvValues is the load-bearing test for this file:
// grepping the MARSHALED BYTES for the secret, not asserting on the Go
// struct -- asserting on the struct would pass while the JSON still carried
// the plaintext value, since a struct-level check can't see past a
// forgotten redaction step in the marshal path.
func TestBuildReportRedactsEnvValues(t *testing.T) {
	const secret = "hf_secret_123"
	cfg := Config{
		RouterListen: 8081,
		Specs: []Spec{
			{
				ID:  "local-spec-1",
				Env: map[string]string{"HF_TOKEN": secret, "OTHER": "also-secret-value"},
			},
		},
	}
	at := time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)

	raw, err := BuildReport(cfg, "file", "", at)
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	if strings.Contains(string(raw), secret) {
		t.Fatalf("secret leaked into the marshaled report: %s", raw)
	}
	if strings.Contains(string(raw), "also-secret-value") {
		t.Fatalf("second secret leaked into the marshaled report: %s", raw)
	}
	if !strings.Contains(string(raw), `"HF_TOKEN"`) {
		t.Fatalf("expected the env KEY to survive redaction: %s", raw)
	}
	if !strings.Contains(string(raw), `"OTHER"`) {
		t.Fatalf("expected the second env KEY to survive redaction: %s", raw)
	}
	if !strings.Contains(string(raw), envRedactedMask) {
		t.Fatalf("expected the redaction mask %q to appear in place of every value: %s", envRedactedMask, raw)
	}

	// source and collected_at round-trip.
	if !strings.Contains(string(raw), `"source":"file"`) {
		t.Errorf("source did not round-trip: %s", raw)
	}
	if !strings.Contains(string(raw), `"collected_at":"2026-08-26T09:00:00Z"`) {
		t.Errorf("collected_at did not round-trip: %s", raw)
	}
	// parse_error is omitempty and was passed as "" -- must not appear.
	if strings.Contains(string(raw), "parse_error") {
		t.Errorf("empty parse_error should be omitted: %s", raw)
	}

	// Decode fully and check the struct shape too (belt and suspenders --
	// the grep above is what actually proves the redaction, this just
	// proves the document still parses as the documented Report shape).
	var decoded Report
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if decoded.Source != "file" {
		t.Errorf("decoded source = %q, want file", decoded.Source)
	}
	var decodedCfg Config
	if err := json.Unmarshal(decoded.Config, &decodedCfg); err != nil {
		t.Fatalf("decode nested config: %v", err)
	}
	if len(decodedCfg.Specs) != 1 || decodedCfg.Specs[0].Env["HF_TOKEN"] != envRedactedMask {
		t.Errorf("nested config env not redacted as expected: %+v", decodedCfg.Specs)
	}
}

// TestRedactConfigEnvRedactsAPIToken is the security guard for I4: a
// hand-written FILE-MODE spec (or a gateway-pushed token in push mode) can
// carry a literal api_token, and redactConfigEnv must mask it exactly as it
// masks env VALUES, so the token never crosses the report wire in clear. It
// greps the MARSHALED bytes (not the struct) for the literal, and also proves
// redactConfigEnv keeps its no-mutation contract: the caller's cfg still holds
// the original token after the call.
func TestRedactConfigEnvRedactsAPIToken(t *testing.T) {
	const secret = "literal-secret"
	cfg := Config{
		Specs: []Spec{
			{ID: "with-token", APIToken: secret, Env: map[string]string{"HF_TOKEN": "env-secret"}},
			{ID: "no-token"}, // APIToken == "" must stay ""
		},
	}

	redacted := redactConfigEnv(cfg)

	raw, err := json.Marshal(redacted)
	if err != nil {
		t.Fatalf("marshal redacted config: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("api token leaked into the marshaled config: %s", raw)
	}
	// The existing env-value redaction still holds.
	if strings.Contains(string(raw), "env-secret") {
		t.Fatalf("env value leaked into the marshaled config: %s", raw)
	}
	if !strings.Contains(string(raw), envRedactedMask) {
		t.Fatalf("expected the redaction mask %q to appear: %s", envRedactedMask, raw)
	}

	// The redacted struct: token spec masked, no-token spec still empty.
	if redacted.Specs[0].APIToken != envRedactedMask {
		t.Errorf("spec[0] APIToken = %q, want the mask %q", redacted.Specs[0].APIToken, envRedactedMask)
	}
	if redacted.Specs[1].APIToken != "" {
		t.Errorf("spec[1] had no token; APIToken = %q, want \"\" (absence must stay distinguishable)", redacted.Specs[1].APIToken)
	}

	// No-mutation contract: the caller's cfg is untouched.
	if cfg.Specs[0].APIToken != secret {
		t.Errorf("redactConfigEnv mutated the caller's cfg: spec[0] APIToken = %q, want %q", cfg.Specs[0].APIToken, secret)
	}
}

// TestBuildReportParseErrorRoundTrips proves the parse_error CODE (and a
// non-file source) survive BuildReport unchanged. The parameter is a
// ParseErrorCode, not free text, so "round-trips" is all this function is
// allowed to do with it -- see ParseErrorCode for why the field carries a
// code at all.
func TestBuildReportParseErrorRoundTrips(t *testing.T) {
	raw, err := BuildReport(Config{}, "gateway", ParseErrorDuplicateSpecID, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if !strings.Contains(string(raw), `"source":"gateway"`) {
		t.Errorf("source did not round-trip: %s", raw)
	}
	if !strings.Contains(string(raw), `"parse_error":"duplicate_spec_id"`) {
		t.Errorf("parse_error did not round-trip: %s", raw)
	}
}

// TestBuildReportZeroConfigNeverEmitsNull proves a Config that never went
// through ParseConfig (e.g. a fresh zero value, as a parse-error report
// might legitimately pass) still marshals every collection as [] / {},
// never as JSON null -- the same discipline ParseConfig itself guarantees,
// re-applied here since BuildReport's input is not guaranteed to have come
// from ParseConfig.
func TestBuildReportZeroConfigNeverEmitsNull(t *testing.T) {
	raw, err := BuildReport(Config{}, "file", "config file is empty", time.Now())
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if strings.Contains(string(raw), "null") {
		t.Fatalf("zero-value config must never marshal any collection as null: %s", raw)
	}
	var decoded Report
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if !strings.Contains(string(decoded.Config), `"specs":[]`) {
		t.Errorf("expected specs:[] in nested config: %s", decoded.Config)
	}
	if !strings.Contains(string(decoded.Config), `"gpu_budgets":[]`) {
		t.Errorf("expected gpu_budgets:[] in nested config: %s", decoded.Config)
	}
	if !strings.Contains(string(decoded.Config), `"coresident":[]`) {
		t.Errorf("expected coresident:[] in nested config: %s", decoded.Config)
	}
}
