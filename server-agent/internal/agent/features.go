// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"encoding/json"
	"log/slog"
	"op-ai-server-agent/internal/sample"
)

// Feature is one named capability this agent binary ships. Behavior gates on
// NAME EQUALITY against the gateway's declared set -- NEVER on version
// compare: a version gate breaks under forks, backports, and custom builds,
// while a name states plainly what the binary can actually do. A feature is
// active only when BOTH this agent and the connected gateway declare its
// name (see ActiveFeatures); an unknown name on either side is ignored --
// that is forward compatibility, not an error.
type Feature struct {
	Name  string // snake_case, unique across Features
	Since string // SemVer ("x.y.z") at which the feature shipped; must be <= Version
}

// Features is the append-only registry of every named capability this agent
// binary ships. Adding an entry REQUIRES a MINOR version bump to Version in
// the same change -- TestFeatureRegistry (features_test.go) enforces this by
// failing whenever an entry's Since outruns Version, turning a forgotten
// version bump into a failing test instead of a review comment. That guard
// cannot catch a forgotten PATCH bump after an unrelated change; keeping
// PATCH bumps honest for non-feature changes stays a process rule.
//
// Never remove or rename an entry once shipped: an older agent binary or a
// gateway that remembers a stale name must keep resolving it the same way.
// Retire a feature by simply having the gateway stop declaring it.
var Features = []Feature{
	{Name: "runtime_manager", Since: "0.2.0"},
}

// FeatureNames returns every feature name in Features, in registry order.
func FeatureNames() []string {
	names := make([]string, len(Features))
	for i, f := range Features {
		names[i] = f.Name
	}
	return names
}

// ActiveFeatures returns the intersection of this agent's Features with the
// gateway's declared set: a feature is active only when both sides name it.
// A nil/empty gateway set -- a legacy gateway that predates feature
// negotiation, or a 404 from GET /api/agent/v1/features -- yields no active
// features, which is exactly a legacy agent's behavior: unchanged.
func ActiveFeatures(gateway []string) []string {
	if len(gateway) == 0 {
		return nil
	}
	declared := make(map[string]bool, len(gateway))
	for _, name := range gateway {
		declared[name] = true
	}
	var active []string
	for _, name := range FeatureNames() {
		if declared[name] {
			active = append(active, name)
		}
	}
	return active
}

// capabilitiesJSON builds the capabilities object collectOnce attaches to
// every telemetry sample: this agent's declared feature names plus its own
// version (informational only -- the gateway negotiates on the features list
// by name, never by comparing this version string). It never returns a nil
// or otherwise invalid json.RawMessage: on the practically-impossible
// json.Marshal failure it falls back to sample.EmptyCapabilities, the same
// "nothing to report" literal sample.Normalize substitutes for an absent
// value, so this field is always a valid JSON object on the wire.
func capabilitiesJSON() json.RawMessage {
	payload := struct {
		Features     []string `json:"features"`
		AgentVersion string   `json:"agent_version"`
	}{
		Features:     FeatureNames(),
		AgentVersion: Version,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		slog.Error("capabilities marshal failed", "err", err)
		return sample.EmptyCapabilities()
	}
	return raw
}
