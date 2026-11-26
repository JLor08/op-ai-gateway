// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Binary kinds in a server's inventory.
const (
	BinaryKindLlamaServer = "llama_server"
	BinaryKindRPCServer   = "rpc_server"
	BinaryKindVLLM        = "vllm"
	BinaryKindOther       = "other"
)

// Execution modes of an inventory row.
const (
	BinaryExecutionNative = "native"
	BinaryExecutionWSL    = "wsl"
)

const binaryWSLScheme = "wsl://"

// Limits on operator-entered inventory fields.
const (
	maxBinaryPathLen    = 4096
	maxBinaryVersionLen = 256
)

var (
	// ErrBinaryPathInvalid rejects an empty, relative or oversized path.
	ErrBinaryPathInvalid = errors.New("binary.path_invalid")
	// ErrBinaryKindInvalid rejects a kind outside llama_server, rpc_server,
	// vllm and other.
	ErrBinaryKindInvalid = errors.New("binary.kind_invalid")
	// ErrBinaryVersionInvalid rejects an oversized version, build or commit.
	ErrBinaryVersionInvalid = errors.New("binary.version_invalid")
	// ErrBinaryNotFound reports a path that is not in the server's inventory.
	ErrBinaryNotFound = errors.New("binary.not_found")
	// ErrBinaryAgentOwned rejects a manual write over an agent-reported row;
	// only its version override can be changed.
	ErrBinaryAgentOwned = errors.New("binary.agent_owned")
)

// ServerBinaryDTO is one inventory row as the portal shows it.
// EffectiveVersion is the version the gateway acts on: the operator's
// override when set, else the detected one.
type ServerBinaryDTO struct {
	Path             string `json:"path"`
	Kind             string `json:"kind"`
	Version          string `json:"version"`
	VersionOverride  string `json:"version_override"`
	EffectiveVersion string `json:"effective_version"`
	Build            string `json:"build"`
	Commit           string `json:"commit"`
	VersionSource    string `json:"version_source"`
	Source           string `json:"source"`
	Allowed          bool   `json:"allowed"`
	// RPCProtocol is the RPC protocol version ("X.Y.Z") the agent probed:
	// an rpc_server's own, a llama_server's from the rpc_server in its
	// directory; "" when unknown, and always "" on a manual row.
	RPCProtocol string    `json:"rpc_protocol"`
	ReportedAt  time.Time `json:"reported_at"`
	// Execution is "native" or "wsl", derived from Path. Distro is the WSL
	// distro ("" for native); ExecPath is what a run spec's binary field
	// takes: the Linux path for a WSL row, Path itself for a native one.
	Execution string `json:"execution"`
	Distro    string `json:"distro"`
	ExecPath  string `json:"exec_path"`
}

// PutServerBinaryRequest is the body of a manual binary write. On POST every
// field but Path is the row's content. On PUT a nil pointer leaves the field
// unchanged; the manual fields (Kind, Version, Build, Commit) only apply to
// a manual row, while VersionOverride ("" clears it) applies to any row.
type PutServerBinaryRequest struct {
	Path            string  `json:"path"`
	Kind            *string `json:"kind"`
	Version         *string `json:"version"`
	Build           *string `json:"build"`
	Commit          *string `json:"commit"`
	VersionOverride *string `json:"version_override"`
}

// parseWSLBinaryPath splits a wsl://<distro>/<linux path> inventory path. It
// mirrors Parse in the server-agent (internal/wslentry/wslentry.go),
// its twin: lowercase scheme, distro up to the first "/" matching the run-spec
// distro pattern, Linux path = the rest including that "/", no NUL, no
// backslash, verbatim. Keep both in step.
func parseWSLBinaryPath(p string) (distro, linuxPath string, ok bool) {
	if !strings.HasPrefix(p, binaryWSLScheme) {
		return "", "", false
	}
	rest := p[len(binaryWSLScheme):]
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return "", "", false
	}
	distro, linuxPath = rest[:i], rest[i:]
	if !runtimeSpecWSLDistroPattern.MatchString(distro) || strings.ContainsAny(linuxPath, "\x00\\") {
		return "", "", false
	}
	return distro, linuxPath, true
}

// isWSLBinaryPath reports whether p names a binary inside a WSL distro: a
// well-formed wsl:// path or, defensively, anything whose lowercased form
// starts with "wsl:" (a look-alike is never a native path either).
func isWSLBinaryPath(p string) bool {
	if _, _, ok := parseWSLBinaryPath(p); ok {
		return true
	}
	return len(p) >= 4 && strings.EqualFold(p[:4], "wsl:")
}

func serverBinaryDTO(b routing.ServerBinary) ServerBinaryDTO {
	execution, distro, execPath := BinaryExecutionNative, "", b.Path
	if d, lp, ok := parseWSLBinaryPath(b.Path); ok {
		execution, distro, execPath = BinaryExecutionWSL, d, lp
	}
	return ServerBinaryDTO{
		Execution: execution, Distro: distro, ExecPath: execPath,
		Path: b.Path, Kind: b.Kind, Version: b.Version, VersionOverride: b.VersionOverride,
		EffectiveVersion: b.EffectiveVersion(), Build: b.Build, Commit: b.Commit,
		VersionSource: b.VersionSource, Source: b.Source, Allowed: b.Allowed, RPCProtocol: b.RPCProtocol,
		ReportedAt: b.ReportedAt,
	}
}

func validBinaryKind(kind string) bool {
	switch kind {
	case BinaryKindLlamaServer, BinaryKindRPCServer, BinaryKindVLLM, BinaryKindOther:
		return true
	}
	return false
}

// validBinaryPath accepts absolute POSIX or Windows paths (the agent may run
// on either) and well-formed wsl:// paths; the gateway only stores the string.
// A string starting with wsl:// that breaks the form is refused.
func validBinaryPath(p string) bool {
	if p == "" || len(p) > maxBinaryPathLen || strings.ContainsRune(p, 0) {
		return false
	}
	if strings.HasPrefix(p, binaryWSLScheme) {
		_, _, ok := parseWSLBinaryPath(p)
		return ok
	}
	if path.IsAbs(p) {
		return true
	}
	// Windows drive path (C:\x or C:/x) or UNC path (\\host\share).
	if len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		(p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z') {
		return true
	}
	return strings.HasPrefix(p, `\\`)
}

// ServerBinaries lists serverID's binary inventory (agent-reported and
// manual rows).
func (s *Service) ServerBinaries(ctx context.Context, p auth.Token, serverID string) ([]ServerBinaryDTO, error) {
	server, err := s.authorizeServer(ctx, p, serverID)
	if err != nil {
		return nil, err
	}
	rows, err := s.routes.ServerBinaries(ctx, server.ID)
	if err != nil {
		return nil, err
	}
	out := make([]ServerBinaryDTO, 0, len(rows))
	for _, b := range rows {
		out = append(out, serverBinaryDTO(b))
	}
	return out, nil
}

func validBinaryText(vals ...*string) bool {
	for _, v := range vals {
		if v != nil && len(*v) > maxBinaryVersionLen {
			return false
		}
	}
	return true
}

// CreateManualServerBinary adds an operator-entered row. A path the agent
// already reports is refused with ErrBinaryAgentOwned.
func (s *Service) CreateManualServerBinary(ctx context.Context, p auth.Token, serverID string, req PutServerBinaryRequest) (ServerBinaryDTO, error) {
	server, err := s.authorizeServer(ctx, p, serverID)
	if err != nil {
		return ServerBinaryDTO{}, err
	}
	if !validBinaryPath(req.Path) {
		return ServerBinaryDTO{}, ErrBinaryPathInvalid
	}
	kind := BinaryKindOther
	if req.Kind != nil {
		kind = *req.Kind
	}
	if !validBinaryKind(kind) {
		return ServerBinaryDTO{}, ErrBinaryKindInvalid
	}
	if !validBinaryText(req.Version, req.Build, req.Commit, req.VersionOverride) {
		return ServerBinaryDTO{}, ErrBinaryVersionInvalid
	}
	// The inventory decides an automatic member's binary and every member's
	// version in its head's generation, so each cluster of the server follows
	// a write -- also a partial one.
	defer s.NotifyClustersOfServer(ctx, server.ID)
	row := routing.ServerBinary{ServerID: server.ID, Path: req.Path, Kind: kind, ReportedAt: s.clock().UTC()}
	applyBinaryVersionFields(&row, req)
	if err := s.routes.UpsertManualServerBinary(ctx, row); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return ServerBinaryDTO{}, ErrBinaryAgentOwned
		}
		return ServerBinaryDTO{}, err
	}
	if req.VersionOverride != nil && *req.VersionOverride != "" {
		if err := s.routes.SetServerBinaryVersionOverride(ctx, server.ID, req.Path, *req.VersionOverride); err != nil {
			return ServerBinaryDTO{}, err
		}
	}
	return s.binaryDTOByPath(ctx, server.ID, req.Path)
}

// UpdateServerBinary edits the row at req.Path: a manual row's fields, and on
// any row the version override.
func (s *Service) UpdateServerBinary(ctx context.Context, p auth.Token, serverID string, req PutServerBinaryRequest) (ServerBinaryDTO, error) {
	server, err := s.authorizeServer(ctx, p, serverID)
	if err != nil {
		return ServerBinaryDTO{}, err
	}
	if err := validateBinaryUpdate(req); err != nil {
		return ServerBinaryDTO{}, err
	}
	row, ok, err := s.binaryRow(ctx, server.ID, req.Path)
	if err != nil {
		return ServerBinaryDTO{}, err
	}
	if !ok {
		return ServerBinaryDTO{}, ErrBinaryNotFound
	}
	defer s.NotifyClustersOfServer(ctx, server.ID) // as in CreateManualServerBinary
	if req.Kind != nil || req.Version != nil || req.Build != nil || req.Commit != nil {
		if row.Source != routing.BinarySourceManual {
			return ServerBinaryDTO{}, ErrBinaryAgentOwned
		}
		if req.Kind != nil {
			row.Kind = *req.Kind
		}
		applyBinaryVersionFields(&row, req)
		if err := s.routes.UpsertManualServerBinary(ctx, row); err != nil {
			return ServerBinaryDTO{}, mapBinaryStoreErr(err)
		}
	}
	if req.VersionOverride != nil {
		if err := s.routes.SetServerBinaryVersionOverride(ctx, server.ID, req.Path, *req.VersionOverride); err != nil {
			return ServerBinaryDTO{}, mapBinaryStoreErr(err)
		}
	}
	return s.binaryDTOByPath(ctx, server.ID, req.Path)
}

// validateBinaryUpdate is UpdateServerBinary's request validation: the
// path, a given kind and the length of every version text, in that order.
func validateBinaryUpdate(req PutServerBinaryRequest) error {
	if !validBinaryPath(req.Path) {
		return ErrBinaryPathInvalid
	}
	if req.Kind != nil && !validBinaryKind(*req.Kind) {
		return ErrBinaryKindInvalid
	}
	if !validBinaryText(req.Version, req.Build, req.Commit, req.VersionOverride) {
		return ErrBinaryVersionInvalid
	}
	return nil
}

// applyBinaryVersionFields sets the version, build and commit the request
// gives on row; an absent field keeps the row's value.
func applyBinaryVersionFields(row *routing.ServerBinary, req PutServerBinaryRequest) {
	if req.Version != nil {
		row.Version = *req.Version
	}
	if req.Build != nil {
		row.Build = *req.Build
	}
	if req.Commit != nil {
		row.Commit = *req.Commit
	}
}

// DeleteManualServerBinary removes an operator-entered row; an agent-owned
// row cannot be deleted (it disappears when the agent stops reporting it).
func (s *Service) DeleteManualServerBinary(ctx context.Context, p auth.Token, serverID, binaryPath string) error {
	server, err := s.authorizeServer(ctx, p, serverID)
	if err != nil {
		return err
	}
	row, ok, err := s.binaryRow(ctx, server.ID, binaryPath)
	if err != nil {
		return err
	}
	if !ok {
		return ErrBinaryNotFound
	}
	if row.Source != routing.BinarySourceManual {
		return ErrBinaryAgentOwned
	}
	if err := s.routes.DeleteManualServerBinary(ctx, server.ID, binaryPath); err != nil {
		return mapBinaryStoreErr(err)
	}
	s.NotifyClustersOfServer(ctx, server.ID) // as in CreateManualServerBinary
	return nil
}

func mapBinaryStoreErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return ErrBinaryNotFound
	case errors.Is(err, store.ErrConflict):
		return ErrBinaryAgentOwned
	}
	return err
}

func (s *Service) binaryRow(ctx context.Context, serverID, binaryPath string) (routing.ServerBinary, bool, error) {
	rows, err := s.routes.ServerBinaries(ctx, serverID)
	if err != nil {
		return routing.ServerBinary{}, false, err
	}
	for _, b := range rows {
		if b.Path == binaryPath {
			return b, true, nil
		}
	}
	return routing.ServerBinary{}, false, nil
}

func (s *Service) binaryDTOByPath(ctx context.Context, serverID, binaryPath string) (ServerBinaryDTO, error) {
	row, ok, err := s.binaryRow(ctx, serverID, binaryPath)
	if err != nil {
		return ServerBinaryDTO{}, err
	}
	if !ok {
		return ServerBinaryDTO{}, ErrBinaryNotFound
	}
	return serverBinaryDTO(row), nil
}

// BinaryVersionFor returns the inventory row at path on serverID, so a caller
// can read the version of the binary a runtime spec names; ok is false when
// the path is not in the inventory. It is a pure lookup: Source and Allowed
// are exposed on the row, not filtered on.
func (s *Service) BinaryVersionFor(ctx context.Context, serverID, binaryPath string) (routing.ServerBinary, bool, error) {
	return s.binaryRow(ctx, serverID, binaryPath)
}

var (
	// versionBuildCommitRE reads "(build 1, commit 2bc46ba)" out of a
	// version string, the form llama-server --version prints; a source
	// tarball build prints "commit unknown".
	versionBuildCommitRE = regexp.MustCompile(`(?i)build\s+(\d+)\s*,\s*commit\s+([0-9a-f]{7,40}|unknown)`)
	versionTagRE         = regexp.MustCompile(`(?i)(?:^|[^0-9a-z])b(\d+)(?:$|[^0-9a-z])`)
	versionHexRE         = regexp.MustCompile(`(?i)(?:^|[^0-9a-z])([0-9a-f]{7,40})(?:$|[^0-9a-z])`)
	hasHexLetterRE       = regexp.MustCompile(`(?i)[a-f]`)
)

// Override text is read in this order: "build N, commit H" (the llama-server
// --version form) gives both; else a release tag "bN" gives the build and a
// 7-40 character hex token containing at least one a-f letter gives the
// commit (an all-digit token such as a date is not a commit).
//
// binaryIdentity is what matching compares: the commit and the build, taken
// from the row's own fields or, when the operator overrode the version, read
// out of the override text (the detected fields describe the wrong build
// then). A build without git metadata (a source tarball) prints "build 0,
// commit unknown": both count as absent, from the fields and from an
// override alike, so two such builds never look identical.
func binaryIdentity(b routing.ServerBinary) (commit, build string) {
	if b.VersionOverride == "" {
		return normalizeCommit(b.Commit), normalizeBuild(b.Build)
	}
	v := b.VersionOverride
	if m := versionBuildCommitRE.FindStringSubmatch(v); m != nil {
		return normalizeCommit(m[2]), normalizeBuild(m[1])
	}
	if m := versionTagRE.FindStringSubmatch(v); m != nil {
		build = normalizeBuild(m[1])
	}
	if m := versionHexRE.FindStringSubmatch(v); m != nil && hasHexLetterRE.MatchString(m[1]) {
		commit = normalizeCommit(m[1])
	}
	return commit, build
}

// normalizeCommit lowercases a commit; "unknown" is no commit.
func normalizeCommit(c string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	if c == "unknown" {
		return ""
	}
	return c
}

// normalizeBuild strips a build's "b" prefix; build 0 is no build.
func normalizeBuild(b string) string {
	b = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(b)), "b")
	if strings.TrimLeft(b, "0") == "" {
		return ""
	}
	return b
}

// commitsMatch is case-insensitive equality, also accepting a prefix match
// when the shorter side has at least 7 characters (an abbreviated hash).
func commitsMatch(a, b string) bool {
	if a == b {
		return true
	}
	short, long := a, b
	if len(short) > len(long) {
		short, long = long, short
	}
	return len(short) >= 7 && strings.HasPrefix(long, short)
}

// rpcCompat is how a head's llama-server and a member's ggml-rpc-server
// relate (rpcCompatibility).
type rpcCompat int

const (
	// rpcCompatUnknown: nothing comparable on the two binaries.
	rpcCompatUnknown rpcCompat = iota
	// rpcCompatIdentical: the same build (commit, else build number).
	rpcCompatIdentical
	// rpcCompatProtocol: another build speaking the same RPC protocol.
	rpcCompatProtocol
	// rpcMismatchProtocol: both report an RPC protocol, and they differ.
	rpcMismatchProtocol
	// rpcMismatchBuild: no protocol on one side, and the known,
	// comparable build identities differ.
	rpcMismatchBuild
)

// compatible reports a pair the head can drive: identical or the same
// protocol.
func (c rpcCompat) compatible() bool { return c == rpcCompatIdentical || c == rpcCompatProtocol }

// mismatch reports a pair known not to talk to each other.
func (c rpcCompat) mismatch() bool { return c == rpcMismatchProtocol || c == rpcMismatchBuild }

// rpcProtocolOf is b's RPC protocol when well-formed, else "".
func rpcProtocolOf(b routing.ServerBinary) string {
	if routing.ValidRPCProtocol(b.RPCProtocol) {
		return b.RPCProtocol
	}
	return ""
}

// rpcCompatibility relates head (a llama_server) and member (an
// rpc_server). When both report an RPC protocol it decides: equal (all of
// X.Y.Z -- llama.cpp checks only major and minor, but a patch bump has
// shifted the op numbers) is compatible, identical when the build identity
// matches too; different is a protocol mismatch. Otherwise the build
// identity decides: commit equality when both have a commit, else build
// equality when both have a build -- identical when equal, a build mismatch
// when not -- else unknown. The protocol is the binary's own (probed from
// the rpc-server), so a version override does not touch it.
func rpcCompatibility(head, member routing.ServerBinary) rpcCompat {
	identity := buildIdentityCompat(head, member)
	hp, mp := rpcProtocolOf(head), rpcProtocolOf(member)
	switch {
	case hp == "" || mp == "":
		return identity
	case hp != mp:
		return rpcMismatchProtocol
	case identity == rpcCompatIdentical:
		return rpcCompatIdentical
	}
	return rpcCompatProtocol
}

// buildIdentityCompat compares the build identities alone: identical,
// build mismatch, or unknown when the two carry no identity of the same
// kind.
func buildIdentityCompat(a, b routing.ServerBinary) rpcCompat {
	ac, ab := binaryIdentity(a)
	bc, bb := binaryIdentity(b)
	var same bool
	switch {
	case ac != "" && bc != "":
		same = commitsMatch(ac, bc)
	case ab != "" && bb != "":
		same = ab == bb
	default:
		return rpcCompatUnknown
	}
	if same {
		return rpcCompatIdentical
	}
	return rpcMismatchBuild
}

// The incompatible_reason values of an automatic cluster candidate.
const (
	rpcRejectNoBinary           = "no_rpc_binary"
	rpcRejectProtocolMismatch   = "protocol_mismatch"
	rpcRejectBuildMismatch      = "build_mismatch"
	rpcRejectVersionUnknown     = "version_unknown"
	rpcRejectHeadVersionUnknown = "head_version_unknown"
)

// rpcMemberScan is what one read of a server's inventory says about its
// ggml-rpc-servers against a head binary: the best compatible row, else why
// there is none.
type rpcMemberScan struct {
	match          routing.ServerBinary
	matched        bool
	reason         string
	memberProtocol string
}

// scanRPCBinaries reads serverID's eligible rpc_server rows against head.
// Agent-reported rows the agent does not allow are skipped (they would fail
// to start); manual rows carry no allowlist status and stay eligible. A
// wsl:// row is never eligible: an automatic member runs natively, and a WSL
// binary cannot serve as a cluster member. Among compatible rows an
// identical build wins over a protocol match, then the first by path.
// Without one the reason is, in this order: no eligible row, a protocol
// mismatch (naming the first mismatching row's protocol by path), a build
// mismatch, else unknown versions; a head without any version is
// head_version_unknown before the inventory is read.
func (s *Service) scanRPCBinaries(ctx context.Context, serverID string, head routing.ServerBinary) (rpcMemberScan, error) {
	if !binaryVersionKnown(head) {
		return rpcMemberScan{reason: rpcRejectHeadVersionUnknown}, nil
	}
	rows, err := s.routes.ServerBinaries(ctx, serverID)
	if err != nil {
		return rpcMemberScan{}, err
	}
	slices.SortFunc(rows, func(a, b routing.ServerBinary) int { return strings.Compare(a.Path, b.Path) })
	var t rpcScanTally
	for _, b := range rows {
		t.add(head, b)
	}
	return t.result(), nil
}

// rpcScanTally accumulates the rows of one scanRPCBinaries pass: the best
// compatible row so far and the evidence for the reason if none is found.
// It is split out so the per-row decision and the final verdict are each
// small enough to read on their own.
type rpcScanTally struct {
	out              rpcMemberScan
	best             rpcCompat
	eligible         bool
	protocolMismatch bool
	buildMismatch    bool
}

// rpcMemberEligible reports whether b may serve as an automatic cluster
// member: an rpc_server row, not under wsl://, and -- for agent-reported
// rows -- allowed by the agent. Manual rows carry no allowlist status.
func rpcMemberEligible(b routing.ServerBinary) bool {
	if b.Kind != BinaryKindRPCServer || isWSLBinaryPath(b.Path) {
		return false
	}
	return b.Source != routing.BinarySourceAgent || b.Allowed
}

// add folds one row (in path order) into the tally against head.
func (t *rpcScanTally) add(head, b routing.ServerBinary) {
	if !rpcMemberEligible(b) {
		return
	}
	t.eligible = true
	switch c := rpcCompatibility(head, b); {
	case c.compatible():
		t.keepIfBetter(b, c)
	case c == rpcMismatchProtocol:
		if !t.protocolMismatch {
			t.out.memberProtocol = rpcProtocolOf(b)
		}
		t.protocolMismatch = true
	case c == rpcMismatchBuild:
		t.buildMismatch = true
	}
}

// keepIfBetter records b as the match when there is none yet, or when it is
// an identical build and the current match is not (identical beats a mere
// protocol match; among equals the first by path stays).
func (t *rpcScanTally) keepIfBetter(b routing.ServerBinary, c rpcCompat) {
	if !t.out.matched || (c == rpcCompatIdentical && t.best != rpcCompatIdentical) {
		t.out.match, t.out.matched, t.best = b, true, c
	}
}

// result is the scan: the match, else the reason in the documented order.
func (t *rpcScanTally) result() rpcMemberScan {
	out := t.out
	switch {
	case out.matched:
		out.memberProtocol = ""
	case !t.eligible:
		out.reason = rpcRejectNoBinary
	case t.protocolMismatch:
		out.reason = rpcRejectProtocolMismatch
	case t.buildMismatch:
		out.reason = rpcRejectBuildMismatch
	default:
		out.reason = rpcRejectVersionUnknown
	}
	return out
}

// MatchingRPCBinary returns the rpc_server binary on serverID that head's
// llama-server can drive (rpcCompatibility): an identical build before one
// of the same RPC protocol, then the first by path, among the eligible rows
// (scanRPCBinaries). ok is false when the server has none, or the versions
// are unknown.
func (s *Service) MatchingRPCBinary(ctx context.Context, serverID string, head routing.ServerBinary) (routing.ServerBinary, bool, error) {
	scan, err := s.scanRPCBinaries(ctx, serverID, head)
	return scan.match, scan.matched, err
}
