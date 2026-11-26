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
	binaryWSLScheme       = "wsl://"
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
	// RPCProtocol is the protocol version "X.Y.Z" the agent probed from an
	// rpc_server binary; absent when it did not or could not.
	RPCProtocol string `json:"rpc_protocol"`
}

// sanitizeBinarySamples turns the reported entries into inventory rows:
// entries without a plausible path are dropped, an unknown kind becomes
// "other", an unknown version source becomes "", text fields are clamped,
// a malformed RPC protocol becomes "" (see routing.ValidRPCProtocol),
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
		case portal.BinaryKindLlamaServer, portal.BinaryKindRPCServer, portal.BinaryKindVLLM, portal.BinaryKindOther:
		default:
			kind = portal.BinaryKindOther
		}
		src := b.VersionSource
		if src != binaryVersionSrcAgent && src != binaryVersionSrcPath {
			src = ""
		}
		protocol := b.RPCProtocol
		if !routing.ValidRPCProtocol(protocol) {
			protocol = ""
		}
		out = append(out, routing.ServerBinary{
			ServerID: serverID, Path: b.Path, Kind: kind,
			Version: clampBinaryField(b.Version), Build: clampBinaryField(b.Build), Commit: clampBinaryField(b.Commit),
			VersionSource: src, Allowed: b.Allowed, RPCProtocol: protocol, ReportedAt: now,
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
			b.VersionSource != r.VersionSource || b.Allowed != r.Allowed || b.RPCProtocol != r.RPCProtocol {
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
//
// The RPC protocol follows the same idea on its own (keepKnownRPCProtocols,
// after the identity above is settled): a re-report of an rpc_server row that
// carries no protocol (the agent's protocol probe failed or timed out) keeps
// the protocol the row had while its version, build and commit equal the
// stored row's, so the portal's protocol matching does not flap to "unknown"
// and back; a binary replaced at the path whose --version probe failed too
// keeps the replaced binary's identity and so its protocol, as above. A
// report that carries one replaces it. A llama_server row never
// keeps its own; it takes the protocol of its directory's rpc_server rows
// (deriveLlamaServerProtocols).
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
	keepKnownRPCProtocols(rows, stored)
	deriveLlamaServerProtocols(rows)
}

// keepKnownRPCProtocols carries the stored RPC protocol of an agent
// rpc_server row over to a re-report of the same path that has none, as
// long as the re-report (with the identity keepKnownBinaryVersions kept)
// carries the stored row's version, build and commit. Another identity is
// another binary at that path -- replaced, its first protocol probe failed --
// whose protocol is unknown until a probe succeeds; keeping the replaced
// binary's would match clusters on a protocol the file may not speak.
func keepKnownRPCProtocols(rows, stored []routing.ServerBinary) {
	known := make(map[string]routing.ServerBinary, len(stored))
	for _, b := range stored {
		if b.Source == routing.BinarySourceAgent && b.Kind == portal.BinaryKindRPCServer && b.RPCProtocol != "" {
			known[b.Path] = b
		}
	}
	for i := range rows {
		r := &rows[i]
		if r.Kind != portal.BinaryKindRPCServer || r.RPCProtocol != "" {
			continue
		}
		if b, ok := known[r.Path]; ok && r.Version == b.Version && r.Build == b.Build && r.Commit == b.Commit {
			r.RPCProtocol = b.RPCProtocol
		}
	}
}

// deriveLlamaServerProtocols gives every protocol-less llama_server row the
// protocol of the rpc_server rows in its directory (binarySiblingDirKey) --
// the agent's pairing, redone on the rows as written so that a kept
// rpc_server protocol reaches its llama-server too. Without it a restart
// whose rpc-server probe fails would leave the rpc_server row its protocol
// but strip the llama_server row's, withholding exactly the protocol-only
// clusters (head and member of different builds) until the probe succeeds.
// The rpc_server rows of the directory must agree; rows of different
// protocols, or none carrying one, leave the llama_server without one.
func deriveLlamaServerProtocols(rows []routing.ServerBinary) {
	protoByDir := map[string]string{}
	disagree := map[string]bool{}
	for _, r := range rows {
		if r.Kind != portal.BinaryKindRPCServer || r.RPCProtocol == "" {
			continue
		}
		dir := binarySiblingDirKey(r.Path)
		if dir == "" {
			continue
		}
		if p, seen := protoByDir[dir]; seen && p != r.RPCProtocol {
			disagree[dir] = true
		}
		protoByDir[dir] = r.RPCProtocol
	}
	for i := range rows {
		r := &rows[i]
		if r.Kind != portal.BinaryKindLlamaServer || r.RPCProtocol != "" {
			continue
		}
		if dir := binarySiblingDirKey(r.Path); dir != "" && !disagree[dir] {
			r.RPCProtocol = protoByDir[dir]
		}
	}
}

// binarySiblingDirKey is the directory a reported path sits in, the key the
// agent pairs binaries by: everything before the last "/" or "\" (the
// agent reports Windows paths with either), a wsl://<distro> prefix included
// so two distros never pair. "" for a path without a directory.
func binarySiblingDirKey(p string) string {
	prefix, rest := "", p
	if strings.HasPrefix(p, binaryWSLScheme) {
		prefix, rest = binaryWSLScheme, p[len(binaryWSLScheme):]
	}
	i := strings.LastIndexAny(rest, "/\\")
	if i < 0 {
		return ""
	}
	return prefix + rest[:i]
}
