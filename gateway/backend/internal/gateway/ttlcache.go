// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/ttlcache"
	"sync"
	"time"
)

// settingCache memoizes one periodically-refreshed value (typically a
// system_settings read) behind a TTL, so a value read on every request in a
// hot path -- the edge/mesh plaintext-refusal switches, the vendor-account
// flags, the agent-presence and energy-PUE defaults, the netbird_only
// source-check peer IP -- does not pay for a store round trip (or, for the peer
// IP, a live NetBird call) per request.
//
// It is an alias of ttlcache.Cache, where the mechanism, its two usage patterns
// (TTL-only and invalidatable) and the invalidation-race handling are documented.
// The implementation moved out of this package so internal/portal, which cannot
// import gateway, can share it for the sharing flags it reads on the routing hot
// path (portal.Service.vendorSharingFlagsCached).
//
// NOT a fit for every TTL cache on *Server: cachedGatewayPeerDNS
// (agent_binaries.go) deliberately holds ITS mutex across the load call so
// concurrent misses single-flight into one live NetBird resolution; see
// ttlcache.Cache.
type settingCache[T any] = ttlcache.Cache[T]

// warnThrottle rate-limits a per-key log line to at most once per interval,
// with the FIRST occurrence of any key always logged immediately. It backs
// the edge/mesh plaintext-gate refusal Warns (shouldLogEdgeGateRefusal /
// shouldLogMeshGateRefusal): without it, a single retrying client hammering a
// refused path would emit one Warn per attempt and evict the log ring's other
// entries -- destroying the very record a locked-out operator needs to read.
//
// The map is bounded by pruning every entry older than interval on each
// ADMITTED (ShouldLog returning true) call, so only keys logged within the
// last interval are retained, and a key is only ever inserted when it is
// actually admitted -- never on a suppressed call -- so a caller cannot grow
// the map by probing keys that never get logged.
type warnThrottle struct {
	mu sync.Mutex
	at map[string]time.Time
}

// ShouldLog reports whether a log line for key at now should be emitted.
func (w *warnThrottle) ShouldLog(key string, now time.Time, interval time.Duration) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if last, ok := w.at[key]; ok && now.Sub(last) < interval {
		return false
	}
	if w.at == nil {
		w.at = make(map[string]time.Time)
	}
	for k, t := range w.at {
		if now.Sub(t) >= interval {
			delete(w.at, k)
		}
	}
	w.at[key] = now
	return true
}
