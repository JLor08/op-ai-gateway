// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
)

// runLoadModel warm-loads tgt's model on tgt's specific server (forcing a load if not resident),
// then best-effort updates the loaded registry so the model-servers SSE reflects it immediately.
// Reuses the benchmark server reservation (mutually exclusive with benchmarks + live traffic). Does
// NOT persist and does NOT call Release: like runContextProbe, the terminal status must LINGER so
// the frontend's benchmarkStatus poll can read it. The deferred finish() frees the server on EVERY
// exit (including panic).
func (s *Server) runLoadModel(ctx context.Context, run *benchmarkRun, serverID string, tgt benchmarkTarget) {
	res := BenchmarkResult{MappingID: tgt.mapping.ID, GatewayModelName: tgt.mapping.GatewayModelName}
	defer func() {
		run.addResult(res)
		run.finish(res.Error)
		s.Benchmarks.publish(serverID, run.snapshot())
	}()

	if _, err := s.ensureResidentForRun(ctx, tgt); err != nil {
		res.Error = err.Error()
		return
	}
	res.Loaded = true
}

// ensureResidentForRun is the LOAD CORE, shared by the load run and the VRAM
// benchmark: make tgt's model resident on tgt's server, and report whether it
// was ALREADY resident before we touched it.
//
// IT LOADS BY GENERATING, and that is load-bearing rather than incidental:
// there is no non-generating load path anywhere in this code, so by the time
// this returns the model has both loaded AND served a complete one-token
// generation. A backend that allocates its KV cache lazily on first use has
// therefore necessarily already done so -- which is why the VRAM run has no
// second "send one tiny generation" step. Two windows for one observation
// would double the exposure to a drifting neighbour and to the reservation
// being held open, in exchange for a number that cannot differ.
//
// THE alreadyResident RETURN IS A CONTAMINATION SIGNAL, not a convenience.
// The core short-circuits on a resident model, so a caller that has just
// confirmed the model STOPPED and still gets true is being told that
// something it could not stop is serving that model. The load run ignores the
// value (it only wants the model up); the VRAM run reports inconclusive on
// it, because a delta measured against a baseline that already contains the
// model is a definitive ~0.
func (s *Server) ensureResidentForRun(ctx context.Context, tgt benchmarkTarget) (alreadyResident bool, err error) {
	streamer, ok := s.Provider.(provider.StreamingClient)
	if !ok {
		return false, errBenchmarkNoStreaming
	}
	target, req := benchmarkTargetReq(tgt)
	req.MaxTokens = 1 // minimal — we only want the model loaded

	if s.modelResident(ctx, target, tgt) {
		return true, nil
	}
	if _, _, err := s.streamOnce(ctx, streamer, target, req); err != nil {
		return false, err
	}
	s.reflectLoadedAfterLoad(ctx, target, tgt)
	return false, nil
}

// modelResident best-effort reports whether tgt's upstream model is already loaded on tgt's server.
// False on any error / no probe configured.
func (s *Server) modelResident(ctx context.Context, target routing.Target, tgt benchmarkTarget) bool {
	lister, ok := s.Provider.(provider.LoadedModelLister)
	if !ok || strings.TrimSpace(tgt.app.LoadedModelsPath) == "" || strings.TrimSpace(tgt.mapping.AppModelName) == "" {
		return false
	}
	probeCtx := s.upstreamAuthCtx(ctx, target)
	loaded, err := modelLoaded(probeCtx, lister, target, tgt.app, tgt.mapping.AppModelName)
	return err == nil && loaded
}

// reflectLoadedAfterLoad best-effort re-probes the app's loaded set and writes it to the gateway-poll
// registry, so the model-servers SSE flips the row to loaded immediately instead of waiting for the
// next health-poll pass. No-op when the app has no loaded-models endpoint / no registry.
func (s *Server) reflectLoadedAfterLoad(ctx context.Context, target routing.Target, tgt benchmarkTarget) {
	if s.LoadedModels == nil {
		return
	}
	lister, ok := s.Provider.(provider.LoadedModelLister)
	if !ok || strings.TrimSpace(tgt.app.LoadedModelsPath) == "" {
		return
	}
	probeCtx := s.upstreamAuthCtx(ctx, target)
	names, err := lister.LoadedModels(probeCtx, target, tgt.app.LoadedModelsPath, tgt.app.LoadedModelsFormat)
	if err != nil {
		return
	}
	s.LoadedModels.SetGatewayProbe(tgt.app.ID, names) // publishes to the model-servers SSE
}
