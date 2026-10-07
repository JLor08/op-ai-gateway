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
// such probe. There is no spec-type logic either: the router forwards to any
// running child, and a child that serves no /props answers 404, which a
// caller treats as "no context size from this probe".
func EffectiveContextProbePath(app Application, agentHasUpstreamProps bool) string {
	if p := strings.TrimSpace(app.ContextProbePath); p != "" {
		return p
	}
	if app.Type == ProviderServerAgent && agentHasUpstreamProps {
		return AgentRouterContextProbePath
	}
	return ""
}
