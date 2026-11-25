// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"op-ai-server-agent/internal/collector"
	"strings"
	"sync"
	"time"
)

// reasonBadConnAddr is the auth-rejection reason for an unparsable connection address.
const reasonBadConnAddr = "bad connection address"

// Request limits. The source limit applies before identification and
// protects the MAC-mode resolver's wait from a flood; it is generous so that
// many guests behind one NAT address stay under it. The VM limit applies once
// a request is tied to a VM and holds each VM to a little above a 1 Hz
// poller's needs.
const (
	sourceRatePerSecond = 100.0
	sourceRateBurst     = 200.0
	vmRatePerSecond     = 10.0
	vmRateBurst         = 20.0
	limitIdleTTL        = time.Minute
	stateSweepEvery     = time.Second
	maxSources          = 10000
	maxVMs              = 10000

	// rejectLogEvery bounds the Warn lines per (source IP, reason).
	rejectLogEvery = 10 * time.Minute
	maxLogKeys     = 10000
)

// Rejection reasons of the two request limits.
const (
	reasonRateSource = "rate limited (source)"
	reasonRateVM     = "rate limited (vm)"
)

// AuthModes selects which identification modes the HTTP endpoint accepts.
type AuthModes struct{ MAC, Token bool }

// ParseAuthModes parses the -auth flag: a comma list of "mac" and "token".
func ParseAuthModes(s string) (AuthModes, error) {
	var m AuthModes
	if strings.TrimSpace(s) == "" {
		return m, errors.New("hostpower: -auth must list at least one of mac, token")
	}
	for _, part := range strings.Split(s, ",") {
		switch strings.TrimSpace(part) {
		case "mac":
			m.MAC = true
		case "token":
			m.Token = true
		default:
			return AuthModes{}, fmt.Errorf("hostpower: unknown -auth mode %q (want mac or token)", part)
		}
	}
	return m, nil
}

type counterSource interface {
	LiveCounter(vmid string) (uint64, bool)
}

type vmResolver interface {
	Resolve(ctx context.Context, remote, local netip.AddrPort) (string, error)
}

type tokenLookup interface {
	Lookup(token string) (string, bool)
}

// bucket is one token bucket.
type bucket struct {
	tokens   float64
	refilled time.Time
	lastSeen time.Time
}

// limiter holds one token bucket per key. Idle buckets are swept lazily (an
// idle bucket is full again, so dropping it changes nothing) and the map
// never exceeds max keys. It is not safe for concurrent use.
type limiter struct {
	rate, burst float64
	max         int
	buckets     map[string]*bucket
	lastSweep   time.Time
}

func newLimiter(rate, burst float64, maxKeys int) *limiter {
	return &limiter{rate: rate, burst: burst, max: maxKeys, buckets: make(map[string]*bucket)}
}

func (l *limiter) allow(key string, now time.Time) bool {
	if now.Sub(l.lastSweep) >= stateSweepEvery {
		l.lastSweep = now
		for k, b := range l.buckets {
			if now.Sub(b.lastSeen) > limitIdleTTL {
				delete(l.buckets, k)
			}
		}
	}
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.max {
			evictOldest(l.buckets, func(b *bucket) time.Time { return b.lastSeen })
		}
		b = &bucket{tokens: l.burst, refilled: now}
		l.buckets[key] = b
	}
	if el := now.Sub(b.refilled).Seconds(); el > 0 {
		b.tokens = min(l.burst, b.tokens+el*l.rate)
		b.refilled = now
	}
	b.lastSeen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// evictOldest deletes the entry of m with the oldest time.
func evictOldest[K comparable, V any](m map[K]V, at func(V) time.Time) {
	var oldestKey K
	var oldest time.Time
	first := true
	for k, v := range m {
		if t := at(v); first || t.Before(oldest) {
			oldestKey, oldest, first = k, t, false
		}
	}
	delete(m, oldestKey)
}

// logKey identifies one rejection log stream.
type logKey struct{ ip, reason string }

type handler struct {
	modes    AuthModes
	counters counterSource
	resolver vmResolver
	tokens   tokenLookup
	log      *slog.Logger
	now      func() time.Time

	mu        sync.Mutex
	sources   *limiter             // per source IP, before identification
	vms       *limiter             // per VM ID, after identification
	logged    map[logKey]time.Time // last Warn per (source IP, reason)
	lastLogGC time.Time
}

func newHandler(modes AuthModes, counters counterSource, resolver vmResolver, tokens tokenLookup, log *slog.Logger, now func() time.Time) http.Handler {
	return &handler{
		modes: modes, counters: counters, resolver: resolver, tokens: tokens,
		log: log, now: now,
		sources: newLimiter(sourceRatePerSecond, sourceRateBurst, maxSources),
		vms:     newLimiter(vmRatePerSecond, vmRateBurst, maxVMs),
		logged:  make(map[logKey]time.Time),
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ip := sourceIP(r.RemoteAddr)
	if !h.allow(h.sources, ip) {
		h.noteRejected(ip, "", "", reasonRateSource)
		plain(w, http.StatusTooManyRequests, "too many requests")
		return
	}
	if r.URL.Path != collector.HostpowerEnergyPath {
		plain(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		plain(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	vmid, mode, reason, ok := h.identify(r)
	if !ok {
		h.noteRejected(ip, mode, "", reason)
		plain(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !h.allow(h.vms, vmid) {
		h.noteRejected(ip, mode, vmid, reasonRateVM)
		plain(w, http.StatusTooManyRequests, "too many requests")
		return
	}

	energy, live := h.counters.LiveCounter(vmid)
	if !live {
		h.noteRejected(ip, mode, vmid, "not live")
		plain(w, http.StatusServiceUnavailable, "not ready")
		return
	}
	h.log.Debug("hostpower: request served", "remote", ip, "vmid", vmid, "mode", mode)
	body, err := json.Marshal(collector.HostpowerEnergy{
		VMID: vmid, EnergyUJ: energy, MaxEnergyRangeUJ: MaxEnergyRangeUJ,
	})
	if err != nil {
		plain(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(body, '\n'))
}

// identify is the single place that decides which VM a request speaks for. A
// present Authorization header (even an empty one) selects token mode
// exclusively (no MAC fallback); without one, MAC mode applies if enabled. mode is "token", "mac" or "none".
func (h *handler) identify(r *http.Request) (vmid, mode, reason string, ok bool) {
	if _, present := r.Header["Authorization"]; present {
		auth := r.Header.Get("Authorization")
		if !h.modes.Token {
			return "", "token", "token mode disabled", false
		}
		scheme, tok, found := strings.Cut(auth, " ")
		tok = strings.TrimSpace(tok)
		if !found || !strings.EqualFold(scheme, "Bearer") || tok == "" {
			return "", "token", "malformed authorization", false
		}
		id, valid := h.tokens.Lookup(tok)
		if !valid {
			return "", "token", "unknown token", false
		}
		return id, "token", "", true
	}
	if !h.modes.MAC {
		return "", "none", "no credentials and mac mode disabled", false
	}
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return "", "mac", reasonBadConnAddr, false
	}
	laddr, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if laddr == nil {
		return "", "mac", reasonBadConnAddr, false
	}
	local, err := netip.ParseAddrPort(laddr.String())
	if err != nil {
		return "", "mac", reasonBadConnAddr, false
	}
	id, err := h.resolver.Resolve(r.Context(), remote, local)
	if err != nil {
		var re *ResolveError
		if errors.As(err, &re) {
			return "", "mac", re.Reason, false
		}
		return "", "mac", "resolver error", false
	}
	return id, "mac", "", true
}

func sourceIP(remoteAddr string) string {
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return ap.Addr().Unmap().String()
	}
	return remoteAddr
}

func (h *handler) allow(l *limiter, key string) bool {
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	return l.allow(key, now)
}

// noteRejected logs a rejected request at Warn, at most once every
// rejectLogEvery per (source IP, reason), so neither a misconfigured guest
// nor many guests behind one NAT address flood the log. vmid is empty when
// the request was not identified. Never logs a credential.
func (h *handler) noteRejected(ip, mode, vmid, reason string) {
	now := h.now()
	h.mu.Lock()
	if now.Sub(h.lastLogGC) >= stateSweepEvery {
		h.lastLogGC = now
		for k, at := range h.logged {
			if now.Sub(at) >= rejectLogEvery {
				delete(h.logged, k)
			}
		}
	}
	key := logKey{ip: ip, reason: reason}
	last, seen := h.logged[key]
	emit := !seen || now.Sub(last) >= rejectLogEvery
	if emit {
		if !seen && len(h.logged) >= maxLogKeys {
			evictOldest(h.logged, func(t time.Time) time.Time { return t })
		}
		h.logged[key] = now
	}
	h.mu.Unlock()
	if !emit {
		return
	}
	attrs := []any{"remote", ip, "reason", reason}
	if mode != "" {
		attrs = append(attrs, "mode", mode)
	}
	if vmid != "" {
		attrs = append(attrs, "vmid", vmid)
	}
	h.log.Warn("hostpower: request rejected", attrs...)
}

func plain(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = fmt.Fprintln(w, msg)
}

// NewServer builds the HTTP server around a caller-created listener's handler.
// Keep-alive is off on purpose: MAC mode needs a fresh SYN observation for every
// request (design §4.4).
func NewServer(listen string, h http.Handler) *http.Server {
	srv := &http.Server{
		Addr:              listen,
		Handler:           h,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    4096,
		// Connection-level errors (TLS handshakes from scanners or mis-pinned
		// guests, mostly) go to Debug so they cannot flood the log.
		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug),
	}
	srv.SetKeepAlivesEnabled(false)
	return srv
}
