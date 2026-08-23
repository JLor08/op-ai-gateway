// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"log/slog"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync"
	"time"
)

const (
	// warmCooldown deduplicates repeated Warm calls for the same gateway model: a Warm
	// within this window of the last completed/attempted warm is skipped. Keeps a climb_up
	// group from firing a load on every request while a higher-priority member is still
	// loading.
	warmCooldown = 60 * time.Second
	// warmCallTimeout is the absolute ceiling on the whole background warm (resolve + probe
	// + load stream). A warm runs on context.Background (it has no client to end it), so a
	// wedged upstream MUST NOT hang the goroutine forever. streamOnce is additionally
	// idle-watchdog-bounded; this is the outer bound.
	warmCallTimeout = 60 * time.Second
)

// modelWarmer implements routing.ModelWarmer: a best-effort, non-blocking, deduplicated
// background load of a gateway model's best candidate (the climb_up load-ahead). It streams
// a 1-token request DIRECTLY through the provider (like a benchmark) so it records NO usage,
// no billing, and never touches the Active registry. Every upstream call is timeout-bounded
// so a stalled upstream can't leak a goroutine. Both maps are guarded by mu. Nil-safe.
type modelWarmer struct {
	srv      *Server
	mu       sync.Mutex
	inflight map[string]struct{}  // gateway model names with a warm currently running
	lastWarm map[string]time.Time // gateway model name -> last completed/attempted warm (cooldown)
}

func newModelWarmer(s *Server) *modelWarmer {
	return &modelWarmer{
		srv:      s,
		inflight: make(map[string]struct{}),
		lastWarm: make(map[string]time.Time),
	}
}

// Warm triggers a best-effort background load of gatewayModelName. It is NON-BLOCKING and
// deduplicated: a call for a model already warming, or warmed within warmCooldown, returns
// immediately without spawning a second load. Nil-safe (a nil warmer is a no-op).
func (w *modelWarmer) Warm(_ context.Context, gatewayModelName string) {
	if w == nil || w.srv == nil {
		return
	}
	name := strings.TrimSpace(gatewayModelName)
	if name == "" {
		return
	}
	w.mu.Lock()
	if _, running := w.inflight[name]; running {
		w.mu.Unlock()
		return
	}
	if last, ok := w.lastWarm[name]; ok && time.Since(last) < warmCooldown {
		w.mu.Unlock()
		return
	}
	w.inflight[name] = struct{}{}
	w.mu.Unlock()
	go w.warmOnce(name)
}

// warmOnce resolves the model's best (first reachable) candidate, skips warming a model that
// is already resident, else streams a 1-token request to force the load. All failures degrade
// silently (Debug-logged). Bounded by warmCallTimeout. On exit it clears the in-flight marker
// and records the cooldown timestamp so a repeat Warm within warmCooldown is skipped.
func (w *modelWarmer) warmOnce(name string) {
	defer func() {
		w.mu.Lock()
		delete(w.inflight, name)
		w.lastWarm[name] = time.Now()
		w.mu.Unlock()
	}()

	s := w.srv
	streamer, ok := s.Provider.(provider.StreamingClient)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), warmCallTimeout)
	defer cancel()

	// Resolve candidates for the model. ActiveMappingsForModel filters by API flavor, so try
	// each known flavor and take the first that yields a candidate (the load is flavor-agnostic).
	var cands []routing.MappingCandidate
	for _, flavor := range []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic} {
		got, err := s.Routes.ActiveMappingsForModel(ctx, name, flavor)
		if err != nil {
			slog.Debug("model warm: resolve candidates failed", "model", name, "flavor", flavor, "err", err)
			return
		}
		if len(got) > 0 {
			cands = got
			break
		}
	}
	if len(cands) == 0 {
		return // nothing to warm (not a real model, or no active mapping)
	}
	cand := w.pickCandidate(cands)
	bt := benchmarkTarget{server: cand.Server, app: cand.Application, mapping: cand.Mapping}
	target, req := benchmarkTargetReq(bt)
	if target.Timeout <= 0 {
		target.Timeout = warmCallTimeout // a wedged upstream must not hang the warm
	}
	req.MaxTokens = 1 // minimal — we only want the model loaded

	// Skip warming a model that is already resident (a load-ahead of a loaded model is wasted
	// work). Best-effort: a probe error falls through to the warm.
	if lister, ok := s.Provider.(provider.LoadedModelLister); ok &&
		strings.TrimSpace(bt.app.LoadedModelsPath) != "" && strings.TrimSpace(bt.mapping.AppModelName) != "" {
		probeCtx := s.upstreamAuthCtx(ctx, target)
		if loaded, err := modelLoaded(probeCtx, lister, target, bt.app, bt.mapping.AppModelName); err == nil && loaded {
			return
		}
	}

	// Force the load. streamOnce attaches the per-app upstream credential, runs an always-on
	// idle watchdog, and goes DIRECT to the provider — so it records NO usage/billing and never
	// registers in Active. The result is discarded (best-effort load-ahead).
	if _, _, err := s.streamOnce(ctx, streamer, target, req); err != nil {
		slog.Debug("model warm: load stream failed", "model", name, "err", err)
	}
}

// pickCandidate returns the first candidate whose application is currently reachable, else the
// first candidate (a load may still succeed on a cold-start-lenient registry). Caller guarantees
// len(cands) > 0.
func (w *modelWarmer) pickCandidate(cands []routing.MappingCandidate) routing.MappingCandidate {
	for _, c := range cands {
		if w.srv.AppHealth == nil || w.srv.AppHealth.Reachable(c.Application.ID) {
			return c
		}
	}
	return cands[0]
}
