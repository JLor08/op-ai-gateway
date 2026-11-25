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

const (
	rateLimitPerSecond = 10.0
	rateLimitBurst     = 20.0
	sourceIdleTTL      = time.Minute
	sourceSweepEvery   = time.Second
	maxSources         = 10000
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

// sourceState is the per-source-IP rate-limit bucket and log state.
type sourceState struct {
	tokens     float64
	refilled   time.Time
	lastSeen   time.Time
	lastReason string // non-empty while the source's latest verdict is a rejection
}

type handler struct {
	modes    AuthModes
	counters counterSource
	resolver vmResolver
	tokens   tokenLookup
	log      *slog.Logger
	now      func() time.Time

	mu        sync.Mutex
	sources   map[string]*sourceState
	lastSweep time.Time
}

func newHandler(modes AuthModes, counters counterSource, resolver vmResolver, tokens tokenLookup, log *slog.Logger, now func() time.Time) http.Handler {
	return &handler{
		modes: modes, counters: counters, resolver: resolver, tokens: tokens,
		log: log, now: now, sources: make(map[string]*sourceState),
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ip := sourceIP(r.RemoteAddr)
	if !h.allow(ip) {
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
		h.noteRejected(ip, mode, reason)
		plain(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	energy, live := h.counters.LiveCounter(vmid)
	if !live {
		h.noteRejected(ip, mode, "not live")
		plain(w, http.StatusServiceUnavailable, "not ready")
		return
	}
	h.noteAccepted(ip, mode)
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
		return "", "mac", "bad connection address", false
	}
	laddr, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if laddr == nil {
		return "", "mac", "bad connection address", false
	}
	local, err := netip.ParseAddrPort(laddr.String())
	if err != nil {
		return "", "mac", "bad connection address", false
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

// state returns ip's entry, creating it (full bucket) when absent. Caller holds mu.
func (h *handler) state(ip string, now time.Time) *sourceState {
	if now.Sub(h.lastSweep) >= sourceSweepEvery {
		h.lastSweep = now
		for k, s := range h.sources {
			if now.Sub(s.lastSeen) > sourceIdleTTL {
				delete(h.sources, k)
			}
		}
	}
	s, ok := h.sources[ip]
	if ok {
		return s
	}
	if len(h.sources) >= maxSources {
		var oldestKey string
		var oldest time.Time
		first := true
		for k, v := range h.sources {
			if first || v.lastSeen.Before(oldest) {
				oldestKey, oldest, first = k, v.lastSeen, false
			}
		}
		delete(h.sources, oldestKey)
	}
	s = &sourceState{tokens: rateLimitBurst, refilled: now, lastSeen: now}
	h.sources[ip] = s
	return s
}

func (h *handler) allow(ip string) bool {
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.state(ip, now)
	if el := now.Sub(s.refilled).Seconds(); el > 0 {
		s.tokens = min(rateLimitBurst, s.tokens+el*rateLimitPerSecond)
		s.refilled = now
	}
	s.lastSeen = now
	if s.tokens < 1 {
		return false
	}
	s.tokens--
	return true
}

func (h *handler) noteRejected(ip, mode, reason string) {
	h.mu.Lock()
	s := h.state(ip, h.now())
	changed := s.lastReason != reason
	s.lastReason = reason
	h.mu.Unlock()
	if changed {
		h.log.Warn("hostpower: request rejected", "remote", ip, "reason", reason, "mode", mode)
	}
}

func (h *handler) noteAccepted(ip, mode string) {
	h.mu.Lock()
	s := h.state(ip, h.now())
	recovered := s.lastReason != ""
	s.lastReason = ""
	h.mu.Unlock()
	if recovered {
		h.log.Info("hostpower: request accepted again", "remote", ip, "mode", mode)
	}
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
	}
	srv.SetKeepAlivesEnabled(false)
	return srv
}
