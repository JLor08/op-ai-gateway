// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"log/slog"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"sort"
	"strings"
	"time"
)

// Caps on the binary inventory one sample may carry; the agent's allowlist
// is small, so anything beyond these is a malformed or hostile report.
const (
	maxBinariesPerSample  = 256
	maxBinaryPathBytes    = 4096
	maxBinaryFieldBytes   = 256
	binaryVersionSrcAgent = "agent"
	binaryVersionSrcPath  = "path"
)

// agentBinarySample is one entry of the telemetry sample's binaries field,
// the agent's report of one allowlisted binary.
type agentBinarySample struct {
	Path          string `json:"path"`
	Kind          string `json:"kind"`
	Version       string `json:"version"`
	Build         string `json:"build"`
	Commit        string `json:"commit"`
	VersionSource string `json:"version_source"`
	Allowed       bool   `json:"allowed"`
}

// sanitizeBinarySamples turns the reported entries into inventory rows:
// entries without a plausible path are dropped, an unknown kind becomes
// "other", an unknown version source becomes "", text fields are clamped,
// duplicate paths keep the first entry and the list is capped.
func sanitizeBinarySamples(in []agentBinarySample, serverID string, now time.Time) []routing.ServerBinary {
	out := make([]routing.ServerBinary, 0, min(len(in), maxBinariesPerSample))
	seen := make(map[string]struct{}, len(in))
	for _, b := range in {
		if len(out) >= maxBinariesPerSample {
			break
		}
		if b.Path == "" || len(b.Path) > maxBinaryPathBytes || strings.ContainsRune(b.Path, 0) {
			continue
		}
		if _, dup := seen[b.Path]; dup {
			continue
		}
		seen[b.Path] = struct{}{}
		kind := b.Kind
		switch kind {
		case portal.BinaryKindLlamaServer, portal.BinaryKindRPCServer, portal.BinaryKindOther:
		default:
			kind = portal.BinaryKindOther
		}
		src := b.VersionSource
		if src != binaryVersionSrcAgent && src != binaryVersionSrcPath {
			src = ""
		}
		out = append(out, routing.ServerBinary{
			ServerID: serverID, Path: b.Path, Kind: kind,
			Version: clampBinaryField(b.Version), Build: clampBinaryField(b.Build), Commit: clampBinaryField(b.Commit),
			VersionSource: src, Allowed: b.Allowed, ReportedAt: now,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func clampBinaryField(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	if len(s) > maxBinaryFieldBytes {
		return strings.ToValidUTF8(s[:maxBinaryFieldBytes], "")
	}
	return s
}

// writeBackBinaries replaces the server's agent-reported inventory with the
// sample's binaries (manual rows and version overrides survive, see
// routing.Store.ReplaceAgentServerBinaries). An absent field is "unchanged".
// Best-effort: a store failure is logged and never rejects the sample.
func (s *Server) writeBackBinaries(ctx context.Context, serverID string, reported *[]agentBinarySample, now time.Time) {
	if reported == nil || s.Routes == nil {
		return
	}
	rows := sanitizeBinarySamples(*reported, serverID, now)
	stored, err := s.Routes.ServerBinaries(ctx, serverID)
	if err != nil {
		slog.Warn("agent binary inventory read failed", "server_id", serverID, "err", err)
		return
	}
	keepKnownBinaryVersions(rows, stored)
	changed := binaryInventoryChanged(stored, rows)
	if err := s.Routes.ReplaceAgentServerBinaries(ctx, serverID, rows); err != nil {
		slog.Warn("agent binary inventory write failed", "server_id", serverID, "err", err)
		return
	}
	// The inventory decides an automatic cluster member's binary and every
	// member's version in its head's generation, so a change re-derives
	// every server sharing a cluster with this one. The agent also resends
	// its inventory unchanged (on every reconnect and on the system-report
	// cadence, so a lost frame or a failed write heals); such a resend only
	// refreshes the rows' report time and notifies nobody.
	if changed && s.Portal != nil {
		s.Portal.NotifyClustersOfServer(ctx, serverID)
	}
}

// binaryInventoryChanged reports whether writing rows as the server's agent
// inventory changes what stored holds, apart from the report time: a path
// added or gone, or a reported field of a path that differs. A reported path
// a manual row owns is skipped by the write, so it changes nothing.
func binaryInventoryChanged(stored, rows []routing.ServerBinary) bool {
	agent := make(map[string]routing.ServerBinary, len(stored))
	manual := make(map[string]bool, len(stored))
	for _, b := range stored {
		if b.Source == routing.BinarySourceAgent {
			agent[b.Path] = b
		} else {
			manual[b.Path] = true
		}
	}
	written := 0
	for _, r := range rows {
		if manual[r.Path] {
			continue
		}
		written++
		b, ok := agent[r.Path]
		if !ok || b.Kind != r.Kind || b.Version != r.Version || b.Build != r.Build || b.Commit != r.Commit ||
			b.VersionSource != r.VersionSource || b.Allowed != r.Allowed {
			return true
		}
	}
	return written != len(agent)
}

// keepKnownBinaryVersions keeps the identity (version, build, commit) the
// agent's --version probe found for a path when a re-report of that path
// carries none from a probe: the probe failed or timed out (a busy host at
// agent start), and the agent then reports only what the path says, if
// anything. Taking that as the binary's identity would withhold every
// automatic cluster whose binary it is, or -- for a path with a build tag --
// move the head's generation until the probe succeeds again, which relaunches
// the head twice. A report the agent probed replaces the kept identity, and a
// report without any identity keeps whatever the row had.
//
// Limitation: the inventory carries no file identity (size, mtime or digest),
// so a binary replaced at the same path whose first probe fails keeps the
// replaced binary's identity until a probe of the new file succeeds (the agent
// retries with a backoff of up to an hour). An rpc-server row takes its
// identity from a llama-server in the same directory; if that sibling leaves
// the allowlist for good, nothing probes the rpc-server again, and its row
// keeps the sibling's last identity for as long as the agent reports the
// path. The cluster start's own version and banner checks still catch a
// mismatch.
func keepKnownBinaryVersions(rows, stored []routing.ServerBinary) {
	known := make(map[string]routing.ServerBinary, len(stored))
	for _, b := range stored {
		if b.Source == routing.BinarySourceAgent && (b.Version != "" || b.Build != "" || b.Commit != "") {
			known[b.Path] = b
		}
	}
	for i := range rows {
		r := &rows[i]
		b, ok := known[r.Path]
		if !ok || r.VersionSource == binaryVersionSrcAgent {
			continue
		}
		bare := r.Version == "" && r.Build == "" && r.Commit == ""
		if b.VersionSource == binaryVersionSrcAgent || bare {
			r.Version, r.Build, r.Commit, r.VersionSource = b.Version, b.Build, b.Commit, b.VersionSource
		}
	}
}
