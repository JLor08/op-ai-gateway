// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"slices"
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
// it is tolerated as no declaration at all, mirroring sanitizeAgentCertReport's
// "garbage in, nothing trusted, sample still accepted" discipline for the
// cert_* fields. No feature reads as declared, and the server reads as not
// heard from (Known false) rather than as an agent known to declare nothing,
// even after an earlier readable declaration: the blob says nothing about the
// agent's binary, and a runtime-config document that depends on what it
// declares is held until a readable declaration arrives.
func TestIngestTelemetryMalformedCapabilitiesLeavesFeaturesEmptyButAccepted(t *testing.T) {
	srv := NewTestServer()
	declared, declaredRaw := ingestReq(t, `{"host":{"cpu_util_pct":10},"capabilities":{"features":["runtime_manager"]}}`)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", declared, declaredRaw); err != nil {
		t.Fatalf("ingest a declaration: %v", err)
	}
	body := `{"host":{"cpu_util_pct":10},"capabilities":"not-an-object"}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("a malformed capabilities blob must not reject the sample: %v", err)
	}
	if srv.AgentFeatures.Has("mock-host-qwen", "runtime_manager") {
		t.Fatal("a malformed capabilities blob must not report any feature as declared")
	}
	if srv.AgentFeatures.Known("mock-host-qwen") {
		t.Fatal("a malformed capabilities blob left the server's declaration known, want it forgotten")
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
	if !srv.AgentFeatures.Known("mock-host-qwen") {
		t.Fatal("an empty capabilities object is a declaration of nothing, want the server known")
	}
}

// TestAgentFeaturesRegistryKnown pins the third state beside Has: a server
// whose agent declared a set -- an empty one included -- is known, one never
// heard from, forgotten, or evicted is not, and a nil registry knows nothing.
func TestAgentFeaturesRegistryKnown(t *testing.T) {
	var none *agentFeaturesRegistry
	none.Forget("srv")
	if none.Known("srv") {
		t.Fatal("a nil registry knows a server")
	}
	r := NewAgentFeaturesRegistry()
	if r.Known("srv") {
		t.Fatal("a server never heard from is known")
	}
	r.Set("srv", nil)
	if !r.Known("srv") || r.Has("srv", "runtime_manager") {
		t.Fatalf("after an empty declaration: Known %v Has %v, want known without the feature", r.Known("srv"), r.Has("srv", "runtime_manager"))
	}
	r.Forget("srv")
	if r.Known("srv") {
		t.Fatal("a forgotten server is still known")
	}
	r.Set("srv", []string{"runtime_manager"})
	r.Retain(map[string]struct{}{})
	if r.Known("srv") {
		t.Fatal("an evicted server is still known")
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
//
// A blob that does not parse is also reported UNREADABLE (readable false), so
// the ingest forgets the server's declaration instead of recording an empty
// one: it says nothing about the agent's binary. A "features": null parses,
// and declares nothing.
func TestParseAgentCapabilitiesWrongElementTypes(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		readable  bool
	}{
		{"features is a number", `{"features": 5}`, false},
		{"features is a string", `{"features": "runtime_manager"}`, false},
		{"features is an object", `{"features": {"runtime_manager": true}}`, false},
		{"features is an array of numbers", `{"features": [1, 2]}`, false},
		{"features mixes a valid string with a number", `{"features": ["runtime_manager", 5]}`, false},
		{"features is null", `{"features": null}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, readable := parseAgentCapabilities([]byte(tc.raw))
			if got != nil {
				t.Fatalf("parseAgentCapabilities(%s) = %#v, want nil", tc.raw, got)
			}
			if readable != tc.readable {
				t.Fatalf("parseAgentCapabilities(%s) readable = %v, want %v", tc.raw, readable, tc.readable)
			}
		})
	}
}

// TestParseAgentCapabilitiesReadable: an absent blob is an agent that declares
// nothing (an older binary), which is a readable declaration; a blob that is
// not an object is not.
func TestParseAgentCapabilitiesReadable(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      []string
		readable  bool
	}{
		{"absent", ``, nil, true},
		{"an empty object", `{}`, nil, true},
		{"a declaration", `{"features":["runtime_manager"]}`, []string{"runtime_manager"}, true},
		{"not an object", `"not-an-object"`, nil, false},
		{"not JSON", `{`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, readable := parseAgentCapabilities([]byte(tc.raw))
			if !slices.Equal(got, tc.want) || readable != tc.readable {
				t.Fatalf("parseAgentCapabilities(%q) = (%v, %v), want (%v, %v)", tc.raw, got, readable, tc.want, tc.readable)
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
