// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "strings"

// The agent router's own probe contract (agent-runtime-manager §4.1,
// ADR-037). A server_agent application's router serves these routes itself,
// so the gateway derives them instead of reading them off the application:
// the portal stores the three probe fields empty for this type.
const (
	// AgentRouterLoadedModelsPath is the router's loaded-state route. It
	// answers in llama-swap's shape and lists only the specs that are
	// running.
	AgentRouterLoadedModelsPath = "/running"
	// AgentRouterLoadedModelsFormat is the LoadedModelsFormat that parses
	// AgentRouterLoadedModelsPath.
	AgentRouterLoadedModelsFormat = "llama_swap"
	// AgentRouterContextProbePath is the {model}-template context probe for
	// a server_agent application whose ContextProbePath is empty. The router
	// forwards it to the running child's /props with the request's
	// credential intact, so a probe carrying the mapping's spec token reaches
	// an api-key-protected child, which the agent's own token-less loopback
	// probe cannot. It never starts a child: a cold one answers 404
	// runtime.model_not_running. An operator-set ContextProbePath always wins
	// over this default.
	AgentRouterContextProbePath = "/upstream/{model}/props"
	// AgentRouterUpstreamPath is the {model}-template base of the router's
	// per-model passthrough routes (AgentRouterContextProbePath is this plus
	// /props). A caller that hands a provider client the base as the target's
	// Endpoint lets the client append the route's own path: the voices pass
	// aims the audio.cpp voices lister at it, and the lister appends
	// /v1/audio/voices (#251).
	AgentRouterUpstreamPath = "/upstream/{model}"
)

// EffectiveLoadedModelsProbe resolves the loaded-models path and format a
// reader probes for app. A stored path that is not blank wins, returned with
// its format exactly as stored. Otherwise a server_agent application gets
// the router's /running in llama-swap format, and any other application gets
// ("", ""), meaning not tracked.
//
// There is no feature gate: every agent router serves /running, and a failed
// probe degrades to "cannot confirm", never to a wrong answer. The two
// readers that write the loaded-model registry do not use this: for an agent
// application the agent's own report is the truth there.
func EffectiveLoadedModelsProbe(app Application) (path, format string) {
	if strings.TrimSpace(app.LoadedModelsPath) != "" {
		return app.LoadedModelsPath, app.LoadedModelsFormat
	}
	if app.Type == ProviderServerAgent {
		return AgentRouterLoadedModelsPath, AgentRouterLoadedModelsFormat
	}
	return "", ""
}

// EffectiveContextProbePath resolves the context probe path for app: its
// trimmed ContextProbePath when that is not blank; otherwise, for a
// server_agent application whose agent declared the router's props
// passthrough (agentHasUpstreamProps), AgentRouterContextProbePath;
// otherwise "", meaning no probe.
//
// The gate is fail-closed: an agent without the route answers 404 for every
// such probe. This is the application-level answer and knows no spec type: a
// caller that probes the router default for one mapping narrows it with
// EffectiveMappingContextProbePath.
func EffectiveContextProbePath(app Application, agentHasUpstreamProps bool) string {
	if p := strings.TrimSpace(app.ContextProbePath); p != "" {
		return p
	}
	if app.Type == ProviderServerAgent && agentHasUpstreamProps {
		return AgentRouterContextProbePath
	}
	return ""
}

// EffectiveMappingContextProbePath is EffectiveContextProbePath for one
// mapping whose resolved runtime spec is spec (the zero spec when it has
// none). It returns the same path, except that the router default
// (AgentRouterContextProbePath, used only when app.ContextProbePath is blank)
// becomes "" when the spec's effective type (EffectiveRuntimeSpecType) is one
// whose server serves no /props (runtimeSpecTypeServesProps).
//
// The router forwards /upstream/{model}/props to the running child's /props
// whatever the child is. A child that serves no /props answers 404, and the
// app-health pass asked it again after its retry gap on every cadence tick
// (the application's health-check interval, 30 s by default), which kept a
// steady stream of 404s in a sglang-omni child's log. Such a child carries
// no llama.cpp /props document, so the probe never yielded a context size or
// a capability verdict from it: its context size, where it has one, comes
// from the agent's own per-type probe (DeriveProbePaths). An operator-set
// ContextProbePath still wins.
func EffectiveMappingContextProbePath(app Application, spec RuntimeSpec, agentHasUpstreamProps bool) string {
	p := EffectiveContextProbePath(app, agentHasUpstreamProps)
	if strings.TrimSpace(app.ContextProbePath) == "" && !runtimeSpecTypeServesProps(EffectiveRuntimeSpecType(spec)) {
		return ""
	}
	return p
}

// runtimeSpecTypeServesProps reports whether a server of type t may answer a
// GET /props. It is false for every type known to serve none: vllm, tgi,
// ollama, vllm_sr and sglang read their context size from another path
// (DeriveProbePaths), and stable_diffusion_cpp, audio_cpp and sglang_omni
// have no context size to read. llama_cpp serves /props; custom stays true
// because it is the detection fallback and may be a llama-server under
// another name; an unknown value stays true, as before this check existed.
func runtimeSpecTypeServesProps(t RuntimeSpecType) bool {
	switch t {
	case RuntimeSpecTypeVLLM, RuntimeSpecTypeTGI, RuntimeSpecTypeOllama,
		RuntimeSpecTypeStableDiffusionCpp, RuntimeSpecTypeAudioCpp,
		RuntimeSpecTypeVllmSR, RuntimeSpecTypeSGLang, RuntimeSpecTypeSGLangOmni:
		return false
	default:
		return true
	}
}
