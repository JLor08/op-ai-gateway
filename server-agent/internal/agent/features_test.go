// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// semverLTE reports whether a <= b for two "x.y.z" SemVer strings (no
// pre-release/build metadata -- neither Version nor any Feature.Since ever
// carries one). Test-only: production code negotiates by NAME EQUALITY, never
// by comparing versions, so this helper exists solely to guard the registry
// itself (TestFeatureRegistry below), not to gate any runtime behavior.
func semverLTE(a, b string) bool {
	pa, oka := parseSemver(a)
	pb, okb := parseSemver(b)
	if !oka || !okb {
		return false
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return true
}

func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func TestSemverLTEHelper(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.1.0", "0.2.0", true},
		{"0.2.0", "0.2.0", true},
		{"0.3.0", "0.2.0", false},
		{"1.0.0", "0.9.9", false},
		{"0.9.9", "1.0.0", true},
	}
	for _, c := range cases {
		if got := semverLTE(c.a, c.b); got != c.want {
			t.Errorf("semverLTE(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
	if semverLTE("not-a-version", "0.2.0") {
		t.Error("semverLTE should reject a malformed version")
	}
}

// TestFeatureRegistry is the version guard: it fails the build the moment a
// feature entry's Since outruns agent.Version, which is exactly what
// happens when a new Feature is appended without the accompanying MINOR
// bump. It also enforces the registry's two other invariants: every name is
// snake_case, and no name repeats.
func TestFeatureRegistry(t *testing.T) {
	nameRe := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	seen := map[string]bool{}
	for _, f := range Features {
		if !nameRe.MatchString(f.Name) {
			t.Errorf("feature name %q is not snake_case", f.Name)
		}
		if seen[f.Name] {
			t.Errorf("duplicate feature %q", f.Name)
		}
		seen[f.Name] = true
		if !semverLTE(f.Since, Version) {
			t.Errorf("feature %q Since %s > Version %s — bump agent.Version", f.Name, f.Since, Version)
		}
	}
}

func TestFeatureNamesMatchesRegistry(t *testing.T) {
	names := FeatureNames()
	if len(names) != len(Features) {
		t.Fatalf("FeatureNames() len = %d, want %d", len(names), len(Features))
	}
	for i, f := range Features {
		if names[i] != f.Name {
			t.Errorf("FeatureNames()[%d] = %q, want %q", i, names[i], f.Name)
		}
	}
}

// TestCapabilitiesJSONShape pins the wire shape: exactly one key,
// "features", carrying the registry names. agent_version must NOT appear --
// it is reported once, as the sample's own top-level field, so a version
// bump has a single place to touch.
func TestCapabilitiesJSONShape(t *testing.T) {
	raw := capabilitiesJSON()
	if raw == nil {
		t.Fatal("capabilitiesJSON() returned nil, want a valid JSON object")
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("capabilitiesJSON() is not a JSON object: %v (raw=%s)", err, raw)
	}
	if _, dup := keys["agent_version"]; dup {
		t.Errorf("capabilitiesJSON() carries agent_version (%s); the version belongs on the sample's top-level agent_version field only", raw)
	}
	if len(keys) != 1 {
		t.Errorf("capabilitiesJSON() has %d keys (%s), want exactly one (features)", len(keys), raw)
	}
	var features []string
	if err := json.Unmarshal(keys["features"], &features); err != nil {
		t.Fatalf("capabilitiesJSON().features is not a string array: %v (raw=%s)", err, raw)
	}
	if len(features) != len(Features) {
		t.Fatalf("capabilitiesJSON().features = %v, want the %d registry names", features, len(Features))
	}
	for i, f := range Features {
		if features[i] != f.Name {
			t.Errorf("capabilitiesJSON().features[%d] = %q, want %q", i, features[i], f.Name)
		}
	}
}

// TestCapabilitiesJSONReturnsAFreshSliceEachCall is the aliasing guard:
// capabilitiesJSON hands out a copy of a package-level template, so a
// caller that ever wrote through the json.RawMessage it received must not
// be able to corrupt every later sample's capabilities -- the same class of
// bug sample.EmptyCapabilities exists as a function to prevent.
func TestCapabilitiesJSONReturnsAFreshSliceEachCall(t *testing.T) {
	first := capabilitiesJSON()
	want := string(first)
	for i := range first {
		first[i] = 'X'
	}
	if got := string(capabilitiesJSON()); got != want {
		t.Fatalf("after overwriting a returned capabilities slice, the next call returned %q, want %q -- capabilitiesJSON must copy, never alias the package-level template", got, want)
	}
}

// TestRuntimeConfigAckFeatureIsDeclared pins the acknowledgement feature's
// exact wire NAME and the version it ships in. The name is the whole
// negotiation: the gateway checks this literal string before it will wait
// for an acknowledgement instead of falling back to its fixed isolation
// delay, and nothing in this module gates on it -- so a rename here breaks
// the gateway silently, with no compiler and no other test to catch it.
// This is the same two-sides-of-one-contract reasoning that makes
// internal/runtime carry its own copy of the runtime_manager literal.
func TestRuntimeConfigAckFeatureIsDeclared(t *testing.T) {
	const name = "runtime_config_ack"
	for _, f := range Features {
		if f.Name != name {
			continue
		}
		if f.Since != "0.3.0" {
			t.Fatalf("feature %q Since = %q, want 0.3.0 (the branch's single bump)", name, f.Since)
		}
		return
	}
	t.Fatalf("Features does not declare %q; the gateway cannot know an acknowledgement will ever arrive: %+v", name, Features)
}

// TestGPUSelectionFeatureIsDeclared pins the GPU-selection feature's exact wire
// NAME and the version it ships in. The gateway checks this literal string
// before it tells the portal the connected agent can honor a custom GPU order
// and expand the ${..._DEVICES} placeholders; a rename here would break that
// negotiation silently. Same two-sides-of-one-contract reasoning as
// TestRuntimeConfigAckFeatureIsDeclared.
func TestGPUSelectionFeatureIsDeclared(t *testing.T) {
	const name = "gpu_selection"
	for _, f := range Features {
		if f.Name != name {
			continue
		}
		if f.Since != "0.4.0" {
			t.Fatalf("feature %q Since = %q, want 0.4.0 (the branch's single bump)", name, f.Since)
		}
		return
	}
	t.Fatalf("Features does not declare %q; the gateway cannot know the agent honors GPU order / device placeholders: %+v", name, Features)
}

// TestRuntimeAPITokenFeatureIsDeclared pins the runtime-API-token feature's
// exact wire NAME and the version it ships in. The gateway/portal check this
// literal string before trusting that a runtime-spec api_token will actually
// be resolved, masked, and redacted by the connected agent rather than
// passed through literally or leaked in a report; a rename here would break
// that negotiation silently. Same two-sides-of-one-contract reasoning as
// TestRuntimeConfigAckFeatureIsDeclared and TestGPUSelectionFeatureIsDeclared.
func TestRuntimeAPITokenFeatureIsDeclared(t *testing.T) {
	const name = "runtime_api_token"
	for _, f := range Features {
		if f.Name != name {
			continue
		}
		if f.Since != "0.5.0" {
			t.Fatalf("feature %q Since = %q, want 0.5.0 (the branch's single bump)", name, f.Since)
		}
		return
	}
	t.Fatalf("Features does not declare %q; the gateway/portal cannot know a runtime-spec API token will be honored by this agent: %+v", name, Features)
}

// TestRuntimeModelProbeFeatureIsDeclared pins the runtime-model-probe
// feature's exact wire NAME and the version it ships in. The gateway/portal check
// this literal string before trusting that the connected agent probes each
// managed child model server for its context size and live request metrics
// and reports them in the per-runtime telemetry sample
// (sample.RuntimeSample.ContextSize/ActiveRequests/QueueDepth); a rename
// here would break that negotiation silently. Same
// two-sides-of-one-contract reasoning as TestRuntimeConfigAckFeatureIsDeclared,
// TestGPUSelectionFeatureIsDeclared, and TestRuntimeAPITokenFeatureIsDeclared.
func TestRuntimeModelProbeFeatureIsDeclared(t *testing.T) {
	const name = "runtime_model_probe"
	for _, f := range Features {
		if f.Name != name {
			continue
		}
		if f.Since != "0.6.0" {
			t.Fatalf("feature %q Since = %q, want 0.6.0 (the feature's shipped version)", name, f.Since)
		}
		return
	}
	t.Fatalf("Features does not declare %q; the gateway/portal cannot know this agent probes managed model servers for context size and live request metrics: %+v", name, Features)
}

// TestFeaturesDeclareRuntimeUpstreamProps pins the #58 capability: the
// gateway's app-health pass fail-closed-gates its /upstream/{model}/props
// probing on this exact name, so a rename here silently turns that probing
// off for every agent.
func TestFeaturesDeclareRuntimeUpstreamProps(t *testing.T) {
	found := false
	for _, f := range Features {
		if f.Name == "runtime_upstream_props" {
			found = true
			if f.Since != "0.7.0" {
				t.Fatalf("runtime_upstream_props Since = %q, want 0.7.0", f.Since)
			}
		}
	}
	if !found {
		t.Fatal("Features does not declare runtime_upstream_props")
	}
}

// TestFeaturesDeclareRuntimeEnsure pins the ensure-route capability: the
// gateway sends POST /ensure/{model} -- the Load and the VRAM probe of an
// images-only managed child -- only to an agent that declares this exact
// name, and refuses those runs for every other agent, so a rename here
// silently refuses them on every agent.
func TestFeaturesDeclareRuntimeEnsure(t *testing.T) {
	found := false
	for _, f := range Features {
		if f.Name == "runtime_ensure" {
			found = true
			if f.Since != "0.8.0" {
				t.Fatalf("runtime_ensure Since = %q, want 0.8.0", f.Since)
			}
		}
	}
	if !found {
		t.Fatal("Features does not declare runtime_ensure")
	}
}

// TestFeaturesDeclareMultipartProxy pins the multipart-edit capability: the
// gateway relays POST /v1/images/edits (a multipart/form-data body) to an
// agent-launched child only when this agent declares this exact name -- a
// router without the multipart branch json-unmarshals the body, finds no
// model, and answers 404 runtime.model_not_managed, which cannot be told apart
// from an unknown model -- so a rename here silently refuses every edit to an
// agent target. The gateway spells the same string in its own constant
// (multipartProxyFeature, gateway/backend/internal/gateway/agent_features.go);
// this module cannot import it.
func TestFeaturesDeclareMultipartProxy(t *testing.T) {
	found := false
	for _, f := range Features {
		if f.Name == "multipart_proxy" {
			found = true
			if f.Since != "0.9.0" {
				t.Fatalf("multipart_proxy Since = %q, want 0.9.0", f.Since)
			}
		}
	}
	if !found {
		t.Fatal("Features does not declare multipart_proxy")
	}
	if !slices.Contains(FeatureNames(), "multipart_proxy") {
		t.Fatal("FeatureNames() does not carry multipart_proxy, so it never reaches the gateway in capabilities.features")
	}
	// The Version assertion moved to TestFeaturesDeclareRuntimeConfigFiles,
	// beside the newest feature (see the bump-rule note there).
}

// TestFeaturesDeclareRuntimeConfigFiles pins the config-file provisioning
// capability: a launch spec's config_files facet (#257) is written by this
// agent before the model process starts, and the GATEWAY/PORTAL warns when the
// connected agent predates it -- an older agent ignores the unknown field and
// launches the child with the file missing -- so a rename here silently
// disables that warning. The agent itself always honors what it receives; the
// flag exists for the gateway's benefit. The gateway spells the same string in
// its own constant; this module cannot import it.
func TestFeaturesDeclareRuntimeConfigFiles(t *testing.T) {
	found := false
	for _, f := range Features {
		if f.Name == "runtime_config_files" {
			found = true
			if f.Since != "0.10.0" {
				t.Fatalf("runtime_config_files Since = %q, want 0.10.0", f.Since)
			}
		}
	}
	if !found {
		t.Fatal("Features does not declare runtime_config_files")
	}
	if !slices.Contains(FeatureNames(), "runtime_config_files") {
		t.Fatal("FeatureNames() does not carry runtime_config_files, so it never reaches the gateway in capabilities.features")
	}
	// The Version assertion moved to TestFeaturesDeclareRuntimeVoices, beside
	// the newest feature (see the bump-rule note there).
}

// TestFeaturesDeclareRuntimeVoices pins the voices-route capability: this
// agent's runtime router serves GET /upstream/{model}/v1/audio/voices (#251), a
// Status-only passthrough to a RUNNING audio.cpp child's voice listing, and the
// GATEWAY fetches a child's voices only from an agent that declares this exact
// name, fail-closed -- an older router answers 404 runtime.model_not_managed
// for every such read, which cannot be told apart from an unmanaged model -- so
// a rename here silently stops every agent child's voices from being cached.
// The gateway spells the same string in its own constant
// (runtimeVoicesFeature, gateway/backend/internal/gateway/agent_features.go);
// this module cannot import it.
func TestFeaturesDeclareRuntimeVoices(t *testing.T) {
	found := false
	for _, f := range Features {
		if f.Name == "runtime_voices" {
			found = true
			if f.Since != "0.11.0" {
				t.Fatalf("runtime_voices Since = %q, want 0.11.0", f.Since)
			}
		}
	}
	if !found {
		t.Fatal("Features does not declare runtime_voices")
	}
	if !slices.Contains(FeatureNames(), "runtime_voices") {
		t.Fatal("FeatureNames() does not carry runtime_voices, so it never reaches the gateway in capabilities.features")
	}
	// The version assertion lives here, beside the newest feature, and it
	// is the forcing function for the repository's bump rule: a change that
	// moves Version has to come THROUGH this test and state which rule it
	// followed. 0.8.0 was MINOR because agent.Features gained
	// runtime_ensure; 0.8.1 and 0.8.2 were PATCH because they gained nothing
	// (0.8.2 added the image_options telemetry field, gated on the
	// gateway-declared capability_source_sdcpp, which is not an agent.Features
	// entry); 0.9.0 was MINOR because agent.Features gained multipart_proxy;
	// 0.9.1 and 0.9.2 were PATCH because they gained nothing (they probe an
	// audio_cpp child's /v1/models, gated on the gateway-declared
	// capability_source_audiocpp, which is not an agent.Features entry
	// either); 0.10.0 was MINOR because agent.Features gained
	// runtime_config_files; 0.10.1 and 0.10.2 were PATCH (the speech_clone
	// verdict, then the "clone" -> "clon" task-string correction, both read from
	// the child's /v1/models task on the same gateway-declared
	// capability_source_audiocpp source, which is not an agent.Features entry);
	// 0.11.0 is MINOR because agent.Features gained runtime_voices. A Since
	// records the version a feature shipped in, so runtime_ensure's stays 0.8.0,
	// multipart_proxy's stays 0.9.0, runtime_config_files' stays 0.10.0 and
	// runtime_voices' is 0.11.0.
	if Version != "0.11.0" {
		t.Fatalf("Version = %q, want 0.11.0 (agent.Features gained runtime_voices, so the bump is MINOR)", Version)
	}
}
