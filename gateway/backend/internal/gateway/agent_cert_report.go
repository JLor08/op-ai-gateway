// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"strings"
	"sync"
	"time"
)

// maxAgentCertCAFingerprints caps how many CA-root fingerprints one agent report
// may carry. A real bundle holds the current root plus, during a rotation overlap,
// the previous one; the cap only stops an oversized/hostile payload from growing
// the registry.
const maxAgentCertCAFingerprints = 8

// AgentCertReport is what a ServerAgent last reported about the certificate it has
// ACTUALLY installed (Phase 2 distribution): the leaf fingerprint it wrote to disk,
// that leaf's parsed not_after, its cert_mode, and the fingerprints of the roots in
// the ca.pem bundle it holds. ReportedAt is when the gateway accepted the report, so
// a stale claim is visible rather than silently trusted.
type AgentCertReport struct {
	Fingerprint    string
	CAFingerprints []string
	Mode           string
	NotAfter       time.Time
	ReportedAt     time.Time
}

// AgentCertReportRegistry remembers, per server, that last report. In-memory and
// nil-safe: one shared instance is written by the agent-ingest path (POST and
// WebSocket both funnel through Server.ingestTelemetrySample), read by the portal
// (the certificate list's "installed" column and the CA-rotation propagation brake),
// and pruned by the app-health loop.
//
// Entries do NOT expire on their own: "installed ✓" must keep reading true while an
// agent is merely quiet — liveness is AgentPresenceRegistry's job, and ReportedAt
// makes age visible. Retain bounds the map to live servers.
type AgentCertReportRegistry struct {
	mu   sync.RWMutex
	seen map[string]AgentCertReport
	now  func() time.Time
}

func NewAgentCertReportRegistry() *AgentCertReportRegistry {
	return &AgentCertReportRegistry{
		seen: make(map[string]AgentCertReport),
		now:  func() time.Time { return time.Now().UTC() },
	}
}

// Report records rep for serverID, stamping ReportedAt. No-op on nil or an empty id.
//
// A report carrying neither a leaf nor durable CA roots deletes the entry only
// for mode=="off" and is otherwise ignored. A root-only report is retained even
// in off mode: it proves gateway transport trust without claiming that a server
// leaf is installed.
func (r *AgentCertReportRegistry) Report(serverID string, rep AgentCertReport) {
	if r == nil || serverID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if rep.Fingerprint == "" && len(rep.CAFingerprints) == 0 {
		if rep.Mode == "off" {
			delete(r.seen, serverID)
		}
		return
	}
	rep.CAFingerprints = append([]string(nil), rep.CAFingerprints...)
	rep.ReportedAt = r.now()
	r.seen[serverID] = rep
}

// Get returns the server's last report. The CAFingerprints slice is COPIED, so a
// caller cannot mutate registry state. nil registry / never-reported -> ok=false.
func (r *AgentCertReportRegistry) Get(serverID string) (AgentCertReport, bool) {
	if r == nil {
		return AgentCertReport{}, false
	}
	r.mu.RLock()
	rep, ok := r.seen[serverID]
	r.mu.RUnlock()
	if !ok {
		return AgentCertReport{}, false
	}
	rep.CAFingerprints = append([]string(nil), rep.CAFingerprints...)
	return rep, true
}

// Retain evicts entries for servers not in the live set (bounds memory).
func (r *AgentCertReportRegistry) Retain(live map[string]struct{}) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.seen {
		if _, ok := live[id]; !ok {
			delete(r.seen, id)
		}
	}
}

// agentCertModes are the only cert_mode values a report may claim; anything else is
// dropped to "" so a typo cannot look like a meaningful state.
var agentCertModes = map[string]bool{"off": true, "files": true, "proxy": true}

// normalizeCertFingerprint lowercases + trims a reported fingerprint and accepts it
// ONLY as 64 lowercase hex characters (a SHA-256 digest, the form
// certissue.FingerprintPEM produces). Any other shape returns "" — a foreign format
// must never be able to produce a false "installed ✓" by accident, nor a false
// "propagated" in the CA-rotation brake.
func normalizeCertFingerprint(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) != 64 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return s
}

// sanitizeAgentCertReport turns the four raw report fields of an agent telemetry
// payload into a trustworthy AgentCertReport. Every value is normalized or dropped;
// the CA list is capped at maxAgentCertCAFingerprints and de-duplicated in order.
func sanitizeAgentCertReport(fingerprint string, notAfter time.Time, mode string, caFingerprints []string) AgentCertReport {
	rep := AgentCertReport{Fingerprint: normalizeCertFingerprint(fingerprint)}
	if m := strings.ToLower(strings.TrimSpace(mode)); agentCertModes[m] {
		rep.Mode = m
	}
	if !notAfter.IsZero() {
		rep.NotAfter = notAfter.UTC()
	}
	seen := make(map[string]bool, len(caFingerprints))
	for _, raw := range caFingerprints {
		fp := normalizeCertFingerprint(raw)
		if fp == "" || seen[fp] {
			continue
		}
		seen[fp] = true
		rep.CAFingerprints = append(rep.CAFingerprints, fp)
		if len(rep.CAFingerprints) == maxAgentCertCAFingerprints {
			break
		}
	}
	return rep
}

// CertReport adapts the registry to portal.AgentCertReportReader (the portal package
// must not import internal/gateway).
func (r *AgentCertReportRegistry) CertReport(serverID string) (fingerprint string, caFingerprints []string, mode string, notAfter time.Time, reportedAt time.Time, ok bool) {
	rep, found := r.Get(serverID)
	if !found {
		return "", nil, "", time.Time{}, time.Time{}, false
	}
	return rep.Fingerprint, rep.CAFingerprints, rep.Mode, rep.NotAfter, rep.ReportedAt, true
}
