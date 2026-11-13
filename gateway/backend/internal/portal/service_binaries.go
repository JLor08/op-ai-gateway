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
	Path             string    `json:"path"`
	Kind             string    `json:"kind"`
	Version          string    `json:"version"`
	VersionOverride  string    `json:"version_override"`
	EffectiveVersion string    `json:"effective_version"`
	Build            string    `json:"build"`
	Commit           string    `json:"commit"`
	VersionSource    string    `json:"version_source"`
	Source           string    `json:"source"`
	Allowed          bool      `json:"allowed"`
	ReportedAt       time.Time `json:"reported_at"`
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
// mirrors ParseWSLEntry in the server-agent (internal/runtime/wsl_allowlist.go),
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

func serverBinaryDTO(b routing.ServerBinary) ServerBinaryDTO {
	execution, distro, execPath := BinaryExecutionNative, "", b.Path
	if d, lp, ok := parseWSLBinaryPath(b.Path); ok {
		execution, distro, execPath = BinaryExecutionWSL, d, lp
	}
	return ServerBinaryDTO{
		Execution: execution, Distro: distro, ExecPath: execPath,
		Path: b.Path, Kind: b.Kind, Version: b.Version, VersionOverride: b.VersionOverride,
		EffectiveVersion: b.EffectiveVersion(), Build: b.Build, Commit: b.Commit,
		VersionSource: b.VersionSource, Source: b.Source, Allowed: b.Allowed, ReportedAt: b.ReportedAt,
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
	// version string, the form llama-server --version prints.
	versionBuildCommitRE = regexp.MustCompile(`(?i)build\s+(\d+)\s*,\s*commit\s+([0-9a-f]{7,40})`)
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
// then).
func binaryIdentity(b routing.ServerBinary) (commit, build string) {
	if b.VersionOverride == "" {
		return strings.ToLower(strings.TrimSpace(b.Commit)), normalizeBuild(b.Build)
	}
	v := b.VersionOverride
	if m := versionBuildCommitRE.FindStringSubmatch(v); m != nil {
		return strings.ToLower(m[2]), normalizeBuild(m[1])
	}
	if m := versionTagRE.FindStringSubmatch(v); m != nil {
		build = normalizeBuild(m[1])
	}
	if m := versionHexRE.FindStringSubmatch(v); m != nil && hasHexLetterRE.MatchString(m[1]) {
		commit = strings.ToLower(m[1])
	}
	return commit, build
}

func normalizeBuild(b string) string {
	b = strings.ToLower(strings.TrimSpace(b))
	return strings.TrimPrefix(b, "b")
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

// binariesSameVersion reports whether two binaries are the same build: commit
// equality when both have a commit, else build equality when both have a
// build, else unknown (false).
func binariesSameVersion(a, b routing.ServerBinary) bool {
	ac, ab := binaryIdentity(a)
	bc, bb := binaryIdentity(b)
	if ac != "" && bc != "" {
		return commitsMatch(ac, bc)
	}
	if ab != "" && bb != "" {
		return ab == bb
	}
	return false
}

// MatchingRPCBinary returns the rpc_server binary on serverID whose version
// equals head's (commit, else build); with several matches the first by path.
// Agent-reported rows the agent does not allow are skipped (they would fail
// to start); manual rows carry no allowlist status and stay eligible. ok is
// false when the server has none, or the versions are unknown.
func (s *Service) MatchingRPCBinary(ctx context.Context, serverID string, head routing.ServerBinary) (routing.ServerBinary, bool, error) {
	rows, err := s.routes.ServerBinaries(ctx, serverID) // ordered by path
	if err != nil {
		return routing.ServerBinary{}, false, err
	}
	var best routing.ServerBinary
	found := false
	for _, b := range rows {
		if b.Kind != BinaryKindRPCServer || !binariesSameVersion(head, b) {
			continue
		}
		if b.Source == routing.BinarySourceAgent && !b.Allowed {
			continue
		}
		if !found || b.Path < best.Path {
			best, found = b, true
		}
	}
	return best, found, nil
}
