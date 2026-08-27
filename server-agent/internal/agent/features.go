// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"encoding/json"
)

// Feature is one named capability this agent binary ships. Behavior gates on
// NAME EQUALITY against the gateway's declared set -- NEVER on version
// compare: a version gate breaks under forks, backports, and custom builds,
// while a name states plainly what the binary can actually do. A feature is
// active only when BOTH this agent and the connected gateway declare its
// name; an unknown name on either side is ignored -- that is forward
// compatibility, not an error.
//
// This side of the negotiation only DECLARES: FeatureNames() rides on every
// telemetry sample as capabilities.features (capabilitiesJSON below). The
// intersection itself is computed where the behavior it gates actually
// lives -- runtime.Driver.featureActive, against runtime.FeaturesClient's
// fetch of GET /api/agent/v1/features. internal/runtime cannot import this
// package (internal/agent already imports internal/runtime for the runtime
// driver seam, so the reverse edge would be an import cycle), which is why
// runtime declares its own runtimeManagerFeature constant naming the
// identical string rather than referencing this registry.
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

// capabilitiesTemplate holds the capabilities object's bytes, marshaled
// ONCE at package initialization: Features is a compile-time registry, so
// the bytes are identical for the whole process lifetime and there is
// nothing per-sample about them.
//
// It carries ONLY the declared feature names. The agent version is NOT
// repeated here: it already rides on every sample as the top-level
// agent_version field (sample.Sample.AgentVersion), which is what the
// gateway persists (routing.ServerTelemetry.AgentVersion) and what the
// portal renders (hardware section, runtime report). The gateway's own
// capabilities parser reads nothing but "features"
// (gateway/backend/internal/gateway/agent_ingest.go's
// agentCapabilitiesReport). Reporting the same fact through two wire
// fields is the shape that rots -- one eventually gets updated alone -- so
// a version bump now has exactly ONE place to touch: the Version constant
// in agent.go.
//
// mustMarshalCapabilities PANICS instead of falling back to a "{}"
// literal. json.Marshal cannot fail for a struct holding nothing but a
// []string, so the old fallback branch was both unreachable and untestable
// -- and it was the wrong fallback anyway: shipping "{}" would silently
// declare no features at all, which on the gateway side deactivates
// runtime_manager and stops every managed model process, explained only by
// one log line. Panicking at package init instead runs on every process
// start and in every test binary that touches this package, so a future
// edit that ever makes this payload unmarshalable (a channel, a func, a
// NaN float) fails immediately, loudly, and identically everywhere,
// instead of rotting as a branch nothing can reach.
var capabilitiesTemplate = mustMarshalCapabilities()

func mustMarshalCapabilities() json.RawMessage {
	raw, err := json.Marshal(struct {
		Features []string `json:"features"`
	}{Features: FeatureNames()})
	if err != nil {
		panic("agent: marshal capabilities: " + err.Error())
	}
	return raw
}

// capabilitiesJSON returns the capabilities object for one telemetry
// sample: a FRESH copy of capabilitiesTemplate on every call, never the
// package-level slice itself. json.RawMessage is a []byte, and the same
// reasoning sample.EmptyCapabilities records applies here -- a caller that
// ever wrote through a Sample.Capabilities value would otherwise corrupt
// the template for every later sample in the process. Copying ~30 bytes is
// far cheaper than the marshal it replaces, so nothing is lost by keeping
// the aliasing bug impossible rather than merely unlikely.
func capabilitiesJSON() json.RawMessage {
	out := make(json.RawMessage, len(capabilitiesTemplate))
	copy(out, capabilitiesTemplate)
	return out
}
