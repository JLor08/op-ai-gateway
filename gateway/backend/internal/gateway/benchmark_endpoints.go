// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"time"
)

const (
	codeBenchmarkAlreadyRunning = "benchmark.already_running"
	msgBenchmarkAlreadyRunning  = "a benchmark is already running on this server"

	codeBenchmarkServerInUse = "benchmark.server_in_use"
	msgBenchmarkServerInUse  = "the server has in-flight requests; try again when idle"

	codeBenchmarkRequestFailed = "benchmark.request_failed"
	msgBenchmarkRequestFailed  = "benchmark request failed"
)

// handlePortalBenchmarksActive lists the running benchmarks the caller may see (one per
// server), so the AI-Server list can show a live "running" indicator without polling each
// server. Results are visibility-filtered via the same no-leak server-ownership gate the
// status endpoint uses (server.go handlePortalServerItem benchmark/status branch).
func (s *Server) handlePortalBenchmarksActive(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	all := s.Benchmarks.ActiveRuns()
	visible := make([]BenchmarkStatus, 0, len(all))
	for _, run := range all {
		if _, err := s.Portal.GetServer(r.Context(), token, run.ServerID); err != nil {
			continue // not visible to this caller — drop silently (no existence leak)
		}
		visible = append(visible, run)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": visible})
}

// startBenchmark authorizes the {scope,id} benchmark request, reserves the
// target server (which excludes it from NEW routing via the resolver's
// ServerBusyChecker), enforces an idle gate, and launches the run in the
// background. It responds 202 with the initial status; 409 if a run is already
// in flight on the server OR the server still has in-flight requests; otherwise
// an error status via writeBenchmarkError. scope is "mapping"|"application"|"server";
// mode is "speed"|"capacity"|"both"|"vision" (already validated by parseBenchmarkMode).
//
// Every mode sends each target a chat prompt, so before the reservation the
// scope's views are partitioned (partitionBenchmarkViews): a view that serves
// only images is skipped, and one whose runtime spec cannot be read is not
// measured either, because the answer is unknown and is not guessed at. A
// scope with no runnable view reserves nothing and answers 409
// benchmark.images_only when every view serves only images, else 500
// benchmark.request_failed; a mapping scope's single view therefore gets
// exactly those answers. Otherwise the skipped and unreadable views are
// recorded as the run's first results, before it measures anything, so Total
// counts every view and Done reaches it.
//
// The partition and TryStart are not atomic. A flavor edit in between is
// benign either way: a view that became images-only is prompted and its
// result records the failure, and one that stopped being images-only is
// skipped for this run.
func (s *Server) startBenchmark(w http.ResponseWriter, r *http.Request, token auth.Token, scope, id, mode string) {
	server, views, err := s.Portal.AuthorizeBenchmarkScope(r.Context(), token, scope, id)
	if err != nil {
		writeBenchmarkError(w, err)
		return
	}
	if s.refuseBusyServer(w, server.ID) {
		return
	}
	part := s.partitionBenchmarkViews(r.Context(), views)
	if part.refuseNothingRunnable(w) {
		return
	}
	// The run outlives the HTTP request (the client only triggers it), so it runs
	// on a background context that TryStart's cancel func tears down when the run
	// finishes / is released.
	ctx, cancel := context.WithCancel(context.Background())
	run, ok := s.Benchmarks.TryStart(server.ID, scope, mode, len(views), time.Now().UTC(), cancel)
	if !ok {
		cancel()
		writeJSON(w, http.StatusConflict, apierror.Response(codeBenchmarkAlreadyRunning, msgBenchmarkAlreadyRunning, ""))
		return
	}
	// Idle gate: the server is now RESERVED (ServerBusy reports true) so it is
	// excluded from new routing; if it STILL has in-flight requests, release the
	// reservation and refuse so a benchmark never competes with live traffic.
	// A tiny reserve-then-check race remains (a request that resolved just before
	// the reservation took effect) — acceptable for a manual operator action.
	if s.Active != nil && s.Active.CountByServerName(server.Name) > 0 {
		s.Benchmarks.Release(server.ID)
		cancel()
		writeJSON(w, http.StatusConflict, apierror.Response(codeBenchmarkServerInUse, msgBenchmarkServerInUse, ""))
		return
	}
	for _, res := range part.recorded {
		run.addResult(res)
	}
	go func() {
		defer cancel()
		s.runBenchmark(ctx, run, server.ID, part.runnable, mode)
	}()
	writeJSON(w, http.StatusAccepted, s.Benchmarks.Status(server.ID))
}

// startContextProbe authorizes the mapping, reserves its server (mutually exclusive with
// benchmarks + excluded from routing), idle-gates, and launches a background context-size probe
// that warm-loads the model then reads its context via the app's context_probe_path. 202 + the
// initial status; 409 if the server is busy or has in-flight requests. The result is REPORTED via
// the status poll (results[].context_size), NOT persisted.
//
// The warm-load is a chat prompt, so before the reservation the probe reads the mapping's
// runtime spec once (starterSpec) and refuses a mapping that serves only images with 409
// benchmark.images_only. The probe's target is built from that same spec.
//
// The check and TryStart are not atomic: a mapping that becomes images-only in between is
// warm-loaded anyway, and the failed warm-load is the probe's result.
func (s *Server) startContextProbe(w http.ResponseWriter, r *http.Request, token auth.Token, mappingID string) {
	server, views, err := s.Portal.AuthorizeBenchmarkScope(r.Context(), token, "mapping", mappingID)
	if err != nil {
		writeBenchmarkError(w, err)
		return
	}
	if s.refuseBusyServer(w, server.ID) {
		return
	}
	v := views[0] // mapping scope → exactly one view
	spec, hasSpec, ok := s.starterSpec(r.Context(), w, v)
	if !ok {
		return
	}
	if mappingSpecIsImagesOnly(v.App, spec, hasSpec) {
		writeBenchmarkError(w, errBenchmarkImagesOnly)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	run, ok := s.Benchmarks.TryStart(server.ID, "context-probe", "context", len(views), time.Now().UTC(), cancel)
	if !ok {
		cancel()
		writeJSON(w, http.StatusConflict, apierror.Response(codeBenchmarkAlreadyRunning, msgBenchmarkAlreadyRunning, ""))
		return
	}
	if s.Active != nil && s.Active.CountByServerName(server.Name) > 0 {
		s.Benchmarks.Release(server.ID)
		cancel()
		writeJSON(w, http.StatusConflict, apierror.Response(codeBenchmarkServerInUse, msgBenchmarkServerInUse, ""))
		return
	}
	tgt := s.benchmarkTargetFor(r.Context(), v.Server, v.App, v.Mapping, spec, hasSpec)
	go func() {
		defer cancel()
		s.runContextProbe(ctx, run, server.ID, tgt)
	}()
	writeJSON(w, http.StatusAccepted, s.Benchmarks.Status(server.ID))
}

// startLoadModel loads a mapping's model on its server, idle-gated. Clones startContextProbe: owner/
// admin gate (AuthorizeBenchmarkScope, 404-no-leak), reserve the server (TryStart → routing
// exclusion), refuse if in-flight requests exist (409 server_in_use), else run the load in the
// background and return 202 + status. runLoadModel frees the server on completion (no Release here).
//
// Before the reservation it reads the mapping's runtime spec once (starterSpec) and asks
// loadRefusal whether the Load can succeed: a refusal answers its 409 and reserves nothing. A
// Load that passes is built from that same spec (loadTargetFor), so an images-only agent child
// is loaded without generating.
//
// The check and TryStart are not atomic: a spec that becomes force_stopped in between is not
// refused, and the agent's 503 for it keeps the Load retrying until loadUntilServable's bound,
// after which the failure is the Load's result.
func (s *Server) startLoadModel(w http.ResponseWriter, r *http.Request, token auth.Token, mappingID string) {
	server, views, err := s.Portal.AuthorizeBenchmarkScope(r.Context(), token, "mapping", mappingID)
	if err != nil {
		writeBenchmarkError(w, err)
		return
	}
	if s.refuseBusyServer(w, server.ID) {
		return
	}
	v := views[0] // mapping scope → exactly one view
	spec, hasSpec, ok := s.starterSpec(r.Context(), w, v)
	if !ok {
		return
	}
	if reason := loadRefusal(v.App, spec, hasSpec, s.AgentFeatures.Has(server.ID, runtimeEnsureFeature)); reason != "" {
		writeBenchmarkError(w, loadRefusalErrs[reason])
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	run, ok := s.Benchmarks.TryStart(server.ID, "load", "load", 1, time.Now().UTC(), cancel)
	if !ok {
		cancel()
		writeJSON(w, http.StatusConflict, apierror.Response(codeBenchmarkAlreadyRunning, msgBenchmarkAlreadyRunning, ""))
		return
	}
	if s.Active != nil && s.Active.CountByServerName(server.Name) > 0 {
		s.Benchmarks.Release(server.ID)
		cancel()
		writeJSON(w, http.StatusConflict, apierror.Response(codeBenchmarkServerInUse, msgBenchmarkServerInUse, ""))
		return
	}
	tgt := s.loadTargetFor(r.Context(), v, spec, hasSpec)
	go func() {
		defer cancel()
		s.runLoadModel(ctx, run, server.ID, tgt)
	}()
	writeJSON(w, http.StatusAccepted, s.Benchmarks.Status(server.ID))
}

// startVRAMProbe loads a mapping's model ALONE on its server and measures
// what it costs, so an operator can resolve an unknown VRAM demand
// deliberately. It is its OWN endpoint rather than a fifth ?mode= value, and
// deliberately not an extension of the load run:
//
//   - not a mode, because every mode is a PER-TARGET measurement inside a
//     fan-out loop over an application's or a server's mappings, while this
//     run drains the whole server ONCE and then loads exactly ONE model. As a
//     mode it would either silently measure only the first target, or
//     drain-and-reload the server N times inside one reservation -- and on the
//     server scope a run an operator reads as "measure my models" would stop
//     every model on the box;
//   - not the load run, because the "Load" button would then stop every other
//     model on the server with no affordance warning about it.
//
// THE PRECONDITIONS RUN BEFORE THE RESERVATION, not after. Every one of them
// is read-only, so a refused run never reserves the server, never excludes it
// from routing, and -- the assertion that matters -- never writes a single
// spec. AuthorizeBenchmarkScope is the authorization for everything the run
// later does without a principal of its own, so it stays first.
//
// A busy server is refused next, before the plan reads anything: a VRAM run's
// drain stores force_stopped on every enabled spec, so a second probe during
// it would otherwise be told about the running probe's own overrides instead
// of the run itself. The target is built from a fail-closed read of the
// mapping's spec (starterSpec), which gives the load its credential. The plan
// does not reuse that read: vramRunPlan reads the application's specs itself
// (RuntimeSpecsByApplication) to enumerate the fleet it drains, so the
// target's spec and the plan's fleet come from two reads of the store.
func (s *Server) startVRAMProbe(w http.ResponseWriter, r *http.Request, token auth.Token, mappingID string) {
	server, views, err := s.Portal.AuthorizeBenchmarkScope(r.Context(), token, "mapping", mappingID)
	if err != nil {
		writeBenchmarkError(w, err)
		return
	}
	if s.refuseBusyServer(w, server.ID) {
		return
	}
	v := views[0] // mapping scope → exactly one view
	spec, hasSpec, ok := s.starterSpec(r.Context(), w, v)
	if !ok {
		return
	}
	tgt := s.benchmarkTargetFor(r.Context(), v.Server, v.App, v.Mapping, spec, hasSpec)
	plan, err := s.vramRunPlan(r.Context(), tgt)
	if err != nil {
		writeVRAMProbeError(w, err)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	run, ok := s.Benchmarks.TryStart(server.ID, "vram-probe", "vram", 1, time.Now().UTC(), cancel)
	if !ok {
		cancel()
		writeJSON(w, http.StatusConflict, apierror.Response(codeBenchmarkAlreadyRunning, msgBenchmarkAlreadyRunning, ""))
		return
	}
	if s.Active != nil && s.Active.CountByServerName(server.Name) > 0 {
		s.Benchmarks.Release(server.ID)
		cancel()
		writeJSON(w, http.StatusConflict, apierror.Response(codeBenchmarkServerInUse, msgBenchmarkServerInUse, ""))
		return
	}
	go func() {
		defer cancel()
		s.runVRAMProbe(ctx, run, server.ID, tgt, plan)
	}()
	writeJSON(w, http.StatusAccepted, s.Benchmarks.Status(server.ID))
}

// writeVRAMProbeError answers a VRAM-probe precondition refusal as 409 with
// its own stable code and a message that names the blocking condition or
// spec; anything else falls through to the shared benchmark mapper.
//
// 409 rather than 400 for every one of them: none is a malformed request. Each
// is a conflict with the server's current state -- its agent's configuration
// source, its declared capabilities (runtime_manager, and runtime_ensure for an
// images-only target), its hardware, or an override an operator already set.
func writeVRAMProbeError(w http.ResponseWriter, err error) {
	var refusal *vramRefusal
	if errors.As(err, &refusal) {
		writeJSON(w, http.StatusConflict, apierror.Response(refusal.code, refusal.msg, ""))
		return
	}
	writeBenchmarkError(w, err)
}

// parseBenchmarkMode reads + validates the optional ?mode query param for a
// benchmark trigger. Empty defaults to "speed" (byte-identical to the pre-mode
// behavior); "speed"|"capacity"|"both"|"vision" pass through; anything else writes
// an HTTP 400 and returns ok=false so the caller aborts.
func parseBenchmarkMode(w http.ResponseWriter, r *http.Request) (string, bool) {
	switch mode := r.URL.Query().Get("mode"); mode {
	case "":
		return "speed", true
	case "speed", "capacity", "both", "vision":
		return mode, true
	default:
		writeJSON(w, http.StatusBadRequest, apierror.Response("benchmark.mode_invalid", "unknown benchmark mode", ""))
		return "", false
	}
}

// benchmarkErrRows are writeBenchmarkError's mapper-specific rows (checked
// before sharedErrorMap); portal.ErrMappingNotFound, portal.ErrApplicationNotFound
// and portal.ErrServerNotFound map identically elsewhere and live in
// sharedErrorMap instead. store.ErrNotFound maps to a different code in
// other mappers, so it must stay here.
var benchmarkErrRows = []errRow{
	{err: portal.ErrBenchmarkNoModels, status: http.StatusBadRequest, code: "benchmark.no_models", msg: "no models to benchmark in this scope"},
	{err: portal.ErrBenchmarkScopeInvalid, status: http.StatusBadRequest, code: "benchmark.scope_invalid", msg: "unknown benchmark scope"},
	{err: store.ErrNotFound, status: http.StatusNotFound, code: "benchmark.not_found", msg: "not found"},
	{err: errBenchmarkImagesOnly, status: http.StatusConflict, code: codeBenchmarkImagesOnly, msg: msgBenchmarkImagesOnly},
	{err: errBenchmarkAgentEnsureUnsupported, status: http.StatusConflict, code: codeBenchmarkAgentEnsureUnsupported, msg: msgBenchmarkAgentEnsureUnsupported},
	{err: errBenchmarkSpecForceStopped, status: http.StatusConflict, code: codeBenchmarkSpecForceStopped, msg: msgBenchmarkSpecForceStopped},
}

// writeBenchmarkError maps the AuthorizeBenchmarkScope sentinels and the
// starters' refusals to HTTP status codes. The not-found sentinels
// (mapping/application/server) all map to 404 (no existence leak); the two
// benchmark-specific sentinels map to 400, and the three refusals
// (benchmark_refusals.go) to 409.
func writeBenchmarkError(w http.ResponseWriter, err error) {
	writeMappedError(w, err, benchmarkErrRows, http.StatusInternalServerError, codeBenchmarkRequestFailed, msgBenchmarkRequestFailed)
}

// writeBenchmarkRequestFailed answers 500 benchmark.request_failed, the code
// writeBenchmarkError falls back to, for a starter whose own store read failed.
// It writes the code directly rather than mapping the store error, so no store
// sentinel can turn the failure into another status.
func writeBenchmarkRequestFailed(w http.ResponseWriter) {
	writeJSON(w, http.StatusInternalServerError, apierror.Response(codeBenchmarkRequestFailed, msgBenchmarkRequestFailed, ""))
}
