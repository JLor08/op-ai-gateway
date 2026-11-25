// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package helper

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"op-ai-server-agent/internal/collector"
	"op-ai-server-agent/internal/sample"
	"sync"
	"time"
)

// Request limits and cache window. One agent polls at about 1 Hz, so the
// bucket is generous; the cache bounds sysfs reads however fast clients poll.
const (
	ratePerSecond = 20.0
	rateBurst     = 40.0
	raplCacheTTL  = 250 * time.Millisecond
)

type peerUIDKey struct{}

// peerUIDFrom returns the peer uid ConnContext attached to ctx.
func peerUIDFrom(ctx context.Context) (uint32, bool) {
	uid, ok := ctx.Value(peerUIDKey{}).(uint32)
	return uid, ok
}

// handler serves the two read-only helper endpoints.
type handler struct {
	rapl    func() []collector.HelperRAPLDomain
	modules []sample.MemoryModule
	now     func() time.Time
	log     *slog.Logger

	mu       sync.Mutex
	tokens   float64
	refilled time.Time
	cached   []collector.HelperRAPLDomain
	cachedAt time.Time
	hasCache bool
}

// newHandler builds the helper's HTTP handler. modules is the startup DIMM
// inventory, served unchanged; rapl is called at most once per 250 ms.
func newHandler(rapl func() []collector.HelperRAPLDomain, modules []sample.MemoryModule, now func() time.Time, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	if modules == nil {
		modules = []sample.MemoryModule{}
	}
	return &handler{rapl: rapl, modules: modules, now: now, log: log, tokens: rateBurst}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != collector.HelperRAPLPath && r.URL.Path != collector.HelperMemoryModulesPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.allow() {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	var body any
	if r.URL.Path == collector.HelperRAPLPath {
		body = collector.HelperRAPL{Domains: h.raplDomains()}
	} else {
		body = collector.HelperMemoryModules{Modules: h.modules}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.log.Debug("helper: write response", "err", err)
		return
	}
	if uid, ok := peerUIDFrom(r.Context()); ok {
		h.log.Debug("request served", "path", r.URL.Path, "peer_uid", uid)
	} else {
		h.log.Debug("request served", "path", r.URL.Path)
	}
}

// allow takes one token from the bucket.
func (h *handler) allow() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if el := now.Sub(h.refilled).Seconds(); el > 0 {
		h.tokens = min(rateBurst, h.tokens+el*ratePerSecond)
	}
	h.refilled = now
	if h.tokens < 1 {
		return false
	}
	h.tokens--
	return true
}

// raplDomains returns the cached reading, refreshing it when older than the TTL.
func (h *handler) raplDomains() []collector.HelperRAPLDomain {
	h.mu.Lock()
	defer h.mu.Unlock()
	if now := h.now(); !h.hasCache || now.Sub(h.cachedAt) > raplCacheTTL {
		h.cached = h.rapl()
		if h.cached == nil {
			h.cached = []collector.HelperRAPLDomain{}
		}
		h.cachedAt, h.hasCache = now, true
	}
	return h.cached
}

// newServer builds the HTTP server with the same timeouts and header limit as
// the hostpower service. connContext attaches the peer uid to each request.
func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    4096,
		ConnContext:       connContext,
	}
}
