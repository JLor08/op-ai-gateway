// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
)

// runtimeEnsureFeature is the agent-DECLARED capability name meaning: this
// agent's router answers POST /ensure/{model}, which starts a managed child
// and waits until it is healthy without forwarding a request to it. It is not
// one of this gateway's own advertised features (gatewayAgentFeatures): the
// gateway only consumes it, through s.AgentFeatures.Has, to decide whether an
// images-only agent child can be loaded at all (loadRefusal).
const runtimeEnsureFeature = "runtime_ensure"

// The refusals a manual run makes on a mapping it cannot run as asked,
// before it reserves the server. All three are 409 at the trigger: the
// request is well-formed, and what conflicts with it is the mapping's
// configuration or its agent. They are stable API error codes, each with its
// own portal label.
const (
	// codeBenchmarkImagesOnly: the mapping serves only images
	// (mappingSpecIsImagesOnly), and the run would send it a chat prompt it
	// cannot answer, or it is a Load of a mapping that nothing can start
	// without a request. An application- or server-scope run answers it too
	// when every mapping in its scope serves only images, so it has nothing
	// to measure (refuseNothingRunnable).
	codeBenchmarkImagesOnly = "benchmark.images_only"
	msgBenchmarkImagesOnly  = "this model, or every model in the run's scope, serves only images, so no chat prompt is sent"

	// codeBenchmarkAgentEnsureUnsupported: an images-only agent child loads
	// only through the agent's ensure route, and the server's agent has not
	// declared runtime_ensure. That is an older agent, or one that has not
	// reported since the gateway restarted: the feature registry is in memory.
	codeBenchmarkAgentEnsureUnsupported = "benchmark.agent_ensure_unsupported"
	msgBenchmarkAgentEnsureUnsupported  = "this model serves only images, and starting it without a chat prompt needs the agent feature runtime_ensure (agent 0.8.0 or newer), or the agent has not reported since the gateway restarted"

	// codeBenchmarkSpecForceStopped: the mapping's runtime spec carries the
	// admin override force_stopped. The agent refuses every start while it is
	// set, and a Load must not override desired state (ADR-026).
	codeBenchmarkSpecForceStopped = "benchmark.spec_force_stopped"
	msgBenchmarkSpecForceStopped  = "this model's runtime spec is force-stopped; clear the admin override before loading it"
)

// The refusal sentinels, one per code above. writeBenchmarkError maps each to
// its 409 (benchmarkErrRows).
var (
	errBenchmarkImagesOnly             = errors.New(codeBenchmarkImagesOnly)
	errBenchmarkAgentEnsureUnsupported = errors.New(codeBenchmarkAgentEnsureUnsupported)
	errBenchmarkSpecForceStopped       = errors.New(codeBenchmarkSpecForceStopped)
)

// The reasons loadRefusal gives, a closed set. Each is the suffix of the
// error code the Load starter answers for it (benchmark.<reason>, through
// loadRefusalErrs).
const (
	loadRefusalImagesOnly             = "images_only"
	loadRefusalAgentEnsureUnsupported = "agent_ensure_unsupported"
	loadRefusalSpecForceStopped       = "spec_force_stopped"
)

// loadRefusalErrs maps each loadRefusal reason to the sentinel the Load
// starter answers.
var loadRefusalErrs = map[string]error{
	loadRefusalImagesOnly:             errBenchmarkImagesOnly,
	loadRefusalAgentEnsureUnsupported: errBenchmarkAgentEnsureUnsupported,
	loadRefusalSpecForceStopped:       errBenchmarkSpecForceStopped,
}

// benchmarkSkippedImagesOnly is BenchmarkResult.Skipped for a mapping an
// application- or server-scope run did not measure because it serves only
// images. It is the field's one value.
const benchmarkSkippedImagesOnly = "images_only"

// benchmarkSpecUnreadable prefixes the error an application- or server-scope
// run records for a mapping whose runtime spec could not be read.
const benchmarkSpecUnreadable = "runtime spec unreadable; not benchmarked (no chat prompt sent): "

// mappingRuntimeSpec is the fail-closed read of a mapping's runtime spec that
// the manual runs and the benchmark scheduler decide on. Only a server_agent
// application's mapping can have a spec, so any other application answers
// (zero, false, nil) without a store read. A server_agent mapping answers the
// store's own (spec, ok, err): no spec is (zero, false, nil), and a failed
// read is an error the caller refuses or skips on. Guessing the
// application's flavors instead would send the very chat prompt the
// images-only check exists to stop.
func (s *Server) mappingRuntimeSpec(ctx context.Context, app routing.Application, mappingID string) (routing.RuntimeSpec, bool, error) {
	if app.Type != routing.ProviderServerAgent {
		return routing.RuntimeSpec{}, false, nil
	}
	return s.Routes.RuntimeSpecByMapping(ctx, mappingID)
}

// mappingSpecIsImagesOnly reports whether a mapping's EFFECTIVE flavors are
// images-only (routing.FlavorsAreImagesOnly), resolved by
// routing.EffectiveFields from a spec the caller already read, with the
// precedence routing.Resolver.targetFrom applies: a server_agent mapping's
// spec is the authority whenever it has one, even one stored as [], and the
// application's list stands otherwise.
func mappingSpecIsImagesOnly(app routing.Application, spec routing.RuntimeSpec, hasSpec bool) bool {
	return routing.FlavorsAreImagesOnly(routing.EffectiveFields(app, spec, hasSpec).APIFlavors)
}

// loadRefusal is the one decision whether a Load of a mapping can succeed:
// "" when it can, else one of the loadRefusal* reasons. It reads nothing but
// its arguments: the mapping's application, the spec the caller already read
// (hasSpec: whether the mapping has one), and ensureSupported, whether the
// mapping's server declared runtime_ensure.
//
// A mapping that serves only images cannot load by generating, because the
// Load's one-token chat completion is exactly what it cannot answer:
//   - a server_agent child loads through the agent's ensure route instead, so
//     it is refused only while its agent has not declared that route;
//   - on any other application nothing can start it without a request, so it
//     is refused outright.
//
// Then a spec carrying the admin override force_stopped refuses every Load,
// text and images-only alike. The images-only reasons come first because
// clearing the override would not make such a Load possible.
func loadRefusal(app routing.Application, spec routing.RuntimeSpec, hasSpec bool, ensureSupported bool) string {
	agent := app.Type == routing.ProviderServerAgent
	if mappingSpecIsImagesOnly(app, spec, hasSpec) {
		if !agent {
			return loadRefusalImagesOnly
		}
		if !ensureSupported {
			return loadRefusalAgentEnsureUnsupported
		}
	}
	if agent && hasSpec && spec.AdminState == vramAdminStateForceStopped {
		return loadRefusalSpecForceStopped
	}
	return ""
}

// loadTargetFor builds a Load's target from the spec its starter read, for a
// mapping loadRefusal did not refuse. Such a mapping, if it serves only
// images, is a server_agent child whose agent declared runtime_ensure, so its
// target loads without generating.
func (s *Server) loadTargetFor(ctx context.Context, v portal.BenchmarkTargetView, spec routing.RuntimeSpec, hasSpec bool) benchmarkTarget {
	tgt := s.benchmarkTargetFor(ctx, v.Server, v.App, v.Mapping, spec, hasSpec)
	tgt.loadWithoutGenerating = mappingSpecIsImagesOnly(v.App, spec, hasSpec)
	return tgt
}

// refuseBusyServer answers 409 benchmark.already_running when serverID
// already has a run, and reports whether it did. Every manual starter asks
// right after authorizing and before it reads a spec: a VRAM run's drain
// stores force_stopped on every enabled spec, so a starter that read first
// would answer a refusal about that run's own override.
func (s *Server) refuseBusyServer(w http.ResponseWriter, serverID string) bool {
	if !s.Benchmarks.ServerBusy(serverID) {
		return false
	}
	writeJSON(w, http.StatusConflict, apierror.Response(codeBenchmarkAlreadyRunning, msgBenchmarkAlreadyRunning, ""))
	return true
}

// starterSpec is a mapping-scope starter's one fail-closed read of the
// mapping's runtime spec (mappingRuntimeSpec). The starter's refusals and its
// run's target are both built from what it returns, so the target carries the
// row the check saw. A failed read answers 500 benchmark.request_failed and
// is logged with the mapping id; ok is false then, and the starter returns
// without reserving the server.
func (s *Server) starterSpec(ctx context.Context, w http.ResponseWriter, v portal.BenchmarkTargetView) (spec routing.RuntimeSpec, hasSpec, ok bool) {
	spec, hasSpec, err := s.mappingRuntimeSpec(ctx, v.App, v.Mapping.ID)
	if err != nil {
		slog.Warn("benchmark: runtime spec read failed; run refused", "mapping_id", v.Mapping.ID, "err", err)
		writeBenchmarkRequestFailed(w)
		return routing.RuntimeSpec{}, false, false
	}
	return spec, hasSpec, true
}

// benchmarkViewPartition is startBenchmark's split of a scope's views, made
// before the reservation (partitionBenchmarkViews).
type benchmarkViewPartition struct {
	// runnable are the targets the run measures, in view order.
	runnable []benchmarkTarget
	// recorded are the results the run records before it measures anything,
	// in view order: a skip for each view that serves only images, and an
	// error for each view whose spec read failed. Neither gets a history row.
	recorded []BenchmarkResult
	// unreadable counts the views whose spec read failed.
	unreadable int
}

// partitionBenchmarkViews reads each view's runtime spec once
// (mappingRuntimeSpec) and sorts the view into runnable, skipped (it serves
// only images, so it cannot answer the run's chat prompt) or unreadable (the
// read failed, so whether it serves only images is unknown). A runnable
// target is built from the spec that was read. An unreadable view is logged
// with its mapping id.
func (s *Server) partitionBenchmarkViews(ctx context.Context, views []portal.BenchmarkTargetView) benchmarkViewPartition {
	var p benchmarkViewPartition
	for _, v := range views {
		spec, hasSpec, err := s.mappingRuntimeSpec(ctx, v.App, v.Mapping.ID)
		switch {
		case err != nil:
			slog.Warn("benchmark: runtime spec read failed; mapping not benchmarked", "mapping_id", v.Mapping.ID, "err", err)
			p.unreadable++
			p.recorded = append(p.recorded, BenchmarkResult{MappingID: v.Mapping.ID, GatewayModelName: v.Mapping.GatewayModelName, Error: benchmarkSpecUnreadable + err.Error()})
		case mappingSpecIsImagesOnly(v.App, spec, hasSpec):
			p.recorded = append(p.recorded, BenchmarkResult{MappingID: v.Mapping.ID, GatewayModelName: v.Mapping.GatewayModelName, Skipped: benchmarkSkippedImagesOnly})
		default:
			p.runnable = append(p.runnable, s.benchmarkTargetFor(ctx, v.Server, v.App, v.Mapping, spec, hasSpec))
		}
	}
	return p
}

// refuseNothingRunnable answers a partition with no runnable view, and
// reports whether it did: 409 benchmark.images_only when every view serves
// only images, and 500 benchmark.request_failed when a spec read failed,
// because then an images-only refusal would be a guess.
func (p benchmarkViewPartition) refuseNothingRunnable(w http.ResponseWriter) bool {
	switch {
	case len(p.runnable) > 0:
		return false
	case p.unreadable > 0:
		writeBenchmarkRequestFailed(w)
	default:
		writeBenchmarkError(w, errBenchmarkImagesOnly)
	}
	return true
}
