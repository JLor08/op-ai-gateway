// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"testing"
)

// The capabilities field rides the SAME shared ingest core both transports
// funnel through (see agent_cert_ingest_test.go's identical framing for the
// cert_* fields), so a report over the POST path lands in AgentFeatures.
func TestIngestTelemetryParsesFeatures(t *testing.T) {
	srv := NewTestServer()
	body := `{"host":{"cpu_util_pct":10},"capabilities":{"features":["runtime_manager"],"agent_version":"0.2.0"}}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !srv.AgentFeatures.Has("mock-host-qwen", "runtime_manager") {
		t.Fatal("declared feature was not recorded")
	}
}

// A malformed (wrong-shape) capabilities blob must NEVER reject the sample --
// it is tolerated as an empty feature set, mirroring sanitizeAgentCertReport's
// "garbage in, nothing trusted, sample still accepted" discipline for the
// cert_* fields.
func TestIngestTelemetryMalformedCapabilitiesLeavesFeaturesEmptyButAccepted(t *testing.T) {
	srv := NewTestServer()
	body := `{"host":{"cpu_util_pct":10},"capabilities":"not-an-object"}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("a malformed capabilities blob must not reject the sample: %v", err)
	}
	if srv.AgentFeatures.Has("mock-host-qwen", "runtime_manager") {
		t.Fatal("a malformed capabilities blob must not report any feature as declared")
	}
}

// A legacy agent sending capabilities:{} (today's real-world default, per
// agent_ingest.go's ProviderHealth/Capabilities doc) must ingest cleanly with
// an empty (not "has runtime_manager") feature set.
func TestIngestTelemetryEmptyCapabilitiesObjectLeavesFeaturesEmpty(t *testing.T) {
	srv := NewTestServer()
	req, raw := ingestReq(t, validIngestAgentBody) // capabilities: {}
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if srv.AgentFeatures.Has("mock-host-qwen", "runtime_manager") {
		t.Fatal("an empty capabilities object must not report any feature")
	}
}

// A later sample with no features REPLACES the prior declaration (a
// downgrade/restart must be visible), matching
// agentFeaturesRegistry.Set's full-snapshot-not-delta contract.
func TestIngestTelemetryLaterEmptyCapabilitiesClearsFeatures(t *testing.T) {
	srv := NewTestServer()
	req1, raw1 := ingestReq(t, `{"host":{"cpu_util_pct":10},"capabilities":{"features":["runtime_manager"]}}`)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req1, raw1); err != nil {
		t.Fatalf("ingest 1: %v", err)
	}
	if !srv.AgentFeatures.Has("mock-host-qwen", "runtime_manager") {
		t.Fatal("first sample should have recorded the feature")
	}
	req2, raw2 := ingestReq(t, validIngestAgentBody) // capabilities: {}
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req2, raw2); err != nil {
		t.Fatalf("ingest 2: %v", err)
	}
	if srv.AgentFeatures.Has("mock-host-qwen", "runtime_manager") {
		t.Fatal("a later sample with no features must clear the previous declaration")
	}
}

// TestParseAgentCapabilitiesWrongElementTypes pins the last uncovered shape of
// parseAgentCapabilities' tolerance contract: a capabilities object whose
// "features" key is present but is not a string array. The blob-is-not-an-
// object case has coverage above; these are the cases where the object parses
// and only the VALUE is wrong. All must yield nil (an empty declared set),
// never a partially populated list and never an error that could reject the
// telemetry sample the blob rode in on.
//
// Correct by construction today — json.Unmarshal into a struct fails the
// whole document on a type mismatch, so no partial list can escape — which is
// why this was deferred as coverage-completeness only. It is worth pinning
// because "tolerate and drop" is a behavioural promise the ingest path depends
// on, and a future hand-rolled decoder here (to become more tolerant, e.g.
// keeping the string elements of a mixed array) would silently break it.
func TestParseAgentCapabilitiesWrongElementTypes(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"features is a number", `{"features": 5}`},
		{"features is a string", `{"features": "runtime_manager"}`},
		{"features is an object", `{"features": {"runtime_manager": true}}`},
		{"features is an array of numbers", `{"features": [1, 2]}`},
		{"features mixes a valid string with a number", `{"features": ["runtime_manager", 5]}`},
		{"features is null", `{"features": null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseAgentCapabilities([]byte(tc.raw)); got != nil {
				t.Fatalf("parseAgentCapabilities(%s) = %#v, want nil", tc.raw, got)
			}
		})
	}
}

// TestIngestTelemetryWrongTypedFeaturesAcceptedAsEmpty is the end-to-end half
// of the case above: the sample must still be accepted and no feature
// recorded, matching the malformed-blob test's discipline.
func TestIngestTelemetryWrongTypedFeaturesAcceptedAsEmpty(t *testing.T) {
	srv := NewTestServer()
	body := `{"host":{"cpu_util_pct":10},"capabilities":{"features":5}}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("a wrong-typed features value must not reject the sample: %v", err)
	}
	if srv.AgentFeatures.Has("mock-host-qwen", "runtime_manager") {
		t.Fatal("a wrong-typed features value must not report any feature as declared")
	}
}
