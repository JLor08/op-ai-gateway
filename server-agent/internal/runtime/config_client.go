// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// This file is where the agent-managed model runtime gets its desired-state
// document from (design doc docs/superpowers/specs/2026-08-25-agent-runtime-manager-design.md
// §10.2): the gateway, over ETag-conditional GET with a disk fallback, or an
// operator-authored local file. Both implementations return the exact same
// Config type ParseConfig already knows how to build and validate -- the
// mode is a source switch, not a second parser or a second validation path.
//
// THE DISCIPLINE THAT MATTERS MOST, carried over verbatim from
// internal/proxy's RoutesClient/Driver and internal/certinstall's Installer:
// a transient failure must never tear down a running set. A transport
// error, a non-2xx/304/404 status, an unparseable body, or an unchanged
// mtime all return the LAST KNOWN-GOOD config with changed=false and a nil
// error -- the caller (Task 18's driver, feeding Manager.Apply) can always
// trust that Load never asks it to stop what is already working because the
// gateway (or the operator's file) had a bad moment.
package runtime

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"op-ai-server-agent/internal/gwapi"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// runtimeConfigPath is the gateway route GatewaySource polls, on both the
// NetBird-bound agent mux and the (netbird_only-gated) public mux -- the
// same dual-mux contract certinstall's certificate endpoint and proxy's
// routes endpoint answer on.
const runtimeConfigPath = "/api/agent/v1/runtime-config"

// Source is where the desired runtime config comes from. Both
// implementations return the SAME document type -- the mode is a source
// switch, not a second code path (spec §10.2).
type Source interface {
	// Load returns the current config. changed=false means "same as last
	// Load" (ETag match / mtime unchanged). A transport error returns the
	// last known-good config with changed=false -- transient gateway errors
	// keep current state (the RoutesClient discipline).
	Load(ctx context.Context) (cfg Config, changed bool, err error)
}

// emptyConfig is the safe, inert default before any cache, gateway fetch, or
// WS push has ever succeeded: every collection is non-nil, matching
// ParseConfig's own normalization guarantee, so a caller that ranges over a
// freshly constructed Source's first-returned Config never has to nil-check.
// An empty Specs list simply means "manage nothing yet", never an error.
func emptyConfig() Config {
	return Config{
		GPUBudgets: []GPUBudget{},
		Specs:      []Spec{},
		Coresident: [][2]string{},
	}
}

// sameETag reports whether a and b name the same document. An empty etag
// never counts as a match against anything -- including another empty
// string -- so a document that (unexpectedly) carries no etag at all is
// always treated as changed rather than silently swallowed by a cold-start
// ""=="" comparison. Mirrors the identical guard in owner.applyConfig
// (manager.go) for the same reason.
func sameETag(a, b string) bool {
	return a != "" && a == b
}

// writeCacheFile atomically (over)writes path with data: a temp file created
// DIRECTLY INSIDE the target directory (never os.CreateTemp(""), which
// resolves through $TMPDIR and can land on a different filesystem -- the
// certinstall.writeTempFile precedent this copies), dot-prefixed so a
// partial file is identifiable, chmod 0600 (the document is not supposed to
// carry secrets -- env values are ${AGENT_ENV:NAME} placeholders resolved
// locally -- but a file describing what runs on the host is not
// world-readable material either), then renamed into place. A failure at
// any point leaves whatever was previously at path untouched.
//
// A package-level var, like certinstall.writeTempFile, so a test can inject
// a failure without touching real filesystem permissions (fragile across
// OSes, and meaningless when a test runs as root).
var writeCacheFile = func(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// loadCachedConfig reads and parses path, if present, as the initial
// last-known-good config. A missing file, a read error, or a parse error
// (a corrupted or hand-edited cache) all just mean "no usable cache yet" --
// never a construction-time failure.
func loadCachedConfig(path string) (Config, bool) {
	if path == "" {
		return Config{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, false
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		slog.Debug("runtime: runtime-config disk cache parse failed; ignoring stale cache", "path", path, "error", err)
		return Config{}, false
	}
	return cfg, true
}

// GatewaySource fetches the runtime-config document from
// GET /api/agent/v1/runtime-config with ETag-based conditional GETs, and
// persists the last good document to a disk cache so the agent can start
// (and keep running) model processes before its first successful gateway
// contact, and survive a restart while the gateway is unreachable.
//
// It follows the RoutesClient/Installer transient-error discipline
// (internal/proxy/routes_client.go, internal/certinstall/fetch.go): a
// transport error, a non-2xx/304/404 status, or an unparseable body never
// tears down the caller's running set -- Load returns the last known-good
// config with changed=false and a nil error.
//
// The etag this type tracks and sends as If-None-Match is Config.ETag
// itself -- the wire document's own "etag" field -- rather than the HTTP
// ETag response header: the two are guaranteed identical on the wire
// (task-7-report.md), Config.ETag is the ONLY etag ApplyPushed's
// WS-delivered document ever carries (a WS frame has no HTTP header at
// all), and the gateway's own conditional-GET matching accepts an unquoted
// value equally well as a quoted one. Using a single representation for
// both origins is what keeps "one document, one reconciler" (spec §10.2)
// true of the etag bookkeeping too, not just the parser.
type GatewaySource struct {
	base  string // gatewayURL, trimmed of any trailing '/'
	token string
	http  *http.Client

	cachePath string

	mu                  sync.Mutex
	cached              Config // last known-good config: from the disk cache, a live fetch, or a WS push
	etag                string // cached.ETag, tracked so a cold-start Config{} still has a well-defined ""
	loggedMissing       bool   // true once a 404 has been logged, reset by any non-404 response
	loggedInsecureToken bool   // true once the I2 insecure-token-transport warning has fired; never reset (the base's scheme cannot change for this source's lifetime)
}

// NewGatewaySource builds a GatewaySource. gatewayURL is joined with
// runtimeConfigPath by plain string concatenation (via gwapi.Endpoint) --
// deliberately NOT url.JoinPath -- so a gateway reachable at a base path
// (e.g. "https://gw/base") is preserved rather than normalized away. A nil
// client gets a 30s-timeout default, matching RoutesClient/Installer.
//
// cachePath, if present and parseable, is loaded immediately as the initial
// last-known-good config, so a Load call made before the gateway is
// reachable (e.g. right after an agent restart while the gateway is down)
// still returns a real config via Load's own transient-error path instead
// of an empty one.
func NewGatewaySource(gatewayURL, token string, client *http.Client, cachePath string) *GatewaySource {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	s := &GatewaySource{
		base:      gwapi.TrimBase(gatewayURL),
		token:     token,
		http:      client,
		cachePath: cachePath,
		cached:    emptyConfig(),
	}
	if cfg, ok := loadCachedConfig(cachePath); ok {
		s.cached = cfg
		s.etag = cfg.ETag
	}
	return s
}

// Load implements Source. See the type doc above for the transient-error
// discipline; every non-200 outcome below returns the SAME (current,
// false, nil) shape.
func (s *GatewaySource) Load(ctx context.Context) (Config, bool, error) {
	s.mu.Lock()
	knownETag := s.etag
	current := s.cached
	s.mu.Unlock()

	ep := &gwapi.Endpoint{Base: s.base, Token: s.token, Client: s.http}
	resp, err := ep.GetConditional(ctx, runtimeConfigPath, knownETag)
	if err != nil {
		slog.Debug("runtime: gateway runtime-config fetch failed; keeping current config", "error", err)
		return current, false, nil
	}
	defer gwapi.DrainLimited(resp) // allow connection reuse

	switch resp.StatusCode {
	case http.StatusNotModified:
		s.clearMissingFlag()
		return current, false, nil
	case http.StatusNotFound:
		// The gateway does not have this endpoint (an older build). A
		// transiently downgraded gateway must not tear down a running set;
		// logged at Debug, and only once per consecutive streak of 404s.
		s.warnMissingOnce()
		return current, false, nil
	case http.StatusOK:
		// handled below
	default:
		slog.Debug("runtime: gateway runtime-config fetch returned unexpected status; keeping current config", "status", resp.StatusCode)
		return current, false, nil
	}
	s.clearMissingFlag()

	raw, err := io.ReadAll(gwapi.LimitReader(resp))
	if err != nil {
		slog.Debug("runtime: gateway runtime-config response read failed; keeping current config", "error", err)
		return current, false, nil
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		slog.Debug("runtime: gateway runtime-config response parse failed; keeping current config", "error", err)
		return current, false, nil
	}
	s.checkInsecureToken(cfg)

	changed := !sameETag(cfg.ETag, knownETag)
	s.persist(cfg)
	return cfg, changed, nil
}

// ApplyPushed hands a WS-pushed full runtime-config document (Task 17) to
// the source: it validates, and -- only when it actually differs from what
// is already held -- persists the disk cache and updates the tracked etag.
// A stale or equal etag (the document is byte-for-byte the same desired
// state, however it arrived) returns changed=false, not an error: every
// frame is self-contained and idempotent, last-one-wins.
//
// A malformed document is reported as a non-nil error WITHOUT disturbing
// the held config: the returned Config is still the last known-good one and
// changed is false, so a caller that applies the returned config
// unconditionally (rather than branching on the error) can never tear a
// running set down over one corrupt frame.
func (s *GatewaySource) ApplyPushed(raw []byte) (Config, bool, error) {
	cfg, err := ParseConfig(raw)
	if err != nil {
		s.mu.Lock()
		current := s.cached
		s.mu.Unlock()
		return current, false, err
	}
	s.checkInsecureToken(cfg)

	s.mu.Lock()
	knownETag := s.etag
	s.mu.Unlock()

	if sameETag(cfg.ETag, knownETag) {
		return cfg, false, nil
	}

	s.persist(cfg)
	return cfg, true, nil
}

// persist updates the in-memory last-known-good state and best-effort
// writes the disk cache. A disk-write failure is logged at Debug and never
// surfaced: cfg is already a validated, live document -- worth adopting in
// memory regardless of whether it could also be saved for the NEXT restart
// -- and writeCacheFile's own atomic-rename discipline guarantees the
// PREVIOUS cache file is left untouched by a failed write.
func (s *GatewaySource) persist(cfg Config) {
	s.mu.Lock()
	s.cached = cfg
	s.etag = cfg.ETag
	s.mu.Unlock()

	if s.cachePath == "" {
		return
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		slog.Debug("runtime: marshal runtime config for disk cache failed", "error", err)
		return
	}
	if err := writeCacheFile(s.cachePath, raw); err != nil {
		slog.Debug("runtime: write runtime config disk cache failed", "error", err)
	}
}

func (s *GatewaySource) warnMissingOnce() {
	s.mu.Lock()
	already := s.loggedMissing
	s.loggedMissing = true
	s.mu.Unlock()
	if !already {
		slog.Debug("runtime: gateway runtime-config endpoint not found (older gateway build?); keeping current config")
	}
}

func (s *GatewaySource) clearMissingFlag() {
	s.mu.Lock()
	s.loggedMissing = false
	s.mu.Unlock()
}

// configHasAPIToken reports whether cfg carries a non-empty per-spec API
// token (Spec.APIToken, types.go) in ANY of its specs. It never looks at the
// token's value, only its presence -- the caller (checkInsecureToken) uses
// this purely to decide whether the I2 warning below is even in play.
func configHasAPIToken(cfg Config) bool {
	for i := range cfg.Specs {
		if cfg.Specs[i].APIToken != "" {
			return true
		}
	}
	return false
}

// insecureBase reports whether base clearly names an http:// (not https://)
// scheme. Deliberately conservative: a base with no scheme at all, or
// anything this cannot confidently classify, is NOT flagged -- the design
// note this backs (§9) asks for a warning on a known-insecure transport, not
// a guess about an ambiguous one. base is GatewaySource.base, already
// gwapi.TrimBase-normalized (trailing '/' stripped, nothing else changed),
// so a plain case-insensitive prefix check matches how it is actually
// stored.
func insecureBase(base string) bool {
	return strings.HasPrefix(strings.ToLower(base), "http://")
}

// checkInsecureToken is the single call site both Load (the http.StatusOK
// path) and ApplyPushed use, right after ParseConfig succeeds, to decide
// whether the just-applied config should trip the I2 warning: a runtime
// config with a non-empty per-spec API token, applied over a gateway base
// this source can confidently classify as http://. Checking on every
// applied config (rather than only on the first) is deliberate and cheap --
// warnInsecureTokenOnce's own once-guard is what prevents log spam, and the
// gateway's scheme cannot change over this source's lifetime anyway.
func (s *GatewaySource) checkInsecureToken(cfg Config) {
	if configHasAPIToken(cfg) && insecureBase(s.base) {
		s.warnInsecureTokenOnce()
	}
}

// warnInsecureTokenOnce logs, at WARN and only once per GatewaySource
// lifetime, that an applied runtime config carries a per-spec API token
// while the configured gateway URL is not https: on that transport the
// decrypted token -- like the agent's own bearer credential already does --
// crosses the gateway<->agent channel in clear (design doc §9: any non-off
// token mode "requires or at minimum LOUDLY WARNS for an https gateway
// URL"; security review I2). This is an agent-side LOG warning only -- it
// never blocks, refuses, or downgrades anything; the portal cannot make this
// call at all, since it has no way to know the agent's own configured
// gateway URL scheme.
//
// Mirrors warnMissingOnce's dedup shape exactly (same mutex discipline), but
// the flag is never cleared: unlike a 404 streak, the gateway's scheme is
// fixed for the life of one GatewaySource, so there is no "streak reset" to
// detect and a single log line is the complete, permanent answer for this
// process.
//
// The message and its one attached attr are BOTH chosen to never include
// the token value (or anything derived from it) -- only the fact of an
// insecure scheme, which is not a secret.
func (s *GatewaySource) warnInsecureTokenOnce() {
	s.mu.Lock()
	already := s.loggedInsecureToken
	s.loggedInsecureToken = true
	s.mu.Unlock()
	if !already {
		slog.Warn("runtime: applying a runtime config that carries an API token over a non-https gateway URL; the token will cross the gateway<->agent channel in clear -- configure an https gateway URL",
			"scheme", "http")
	}
}

// FileSource reads the runtime-config document from a local,
// operator-authored file -- the SAME JSON schema the gateway serves (spec
// §10.2: one parser, one validation, one reconciler; the mode is a source
// switch, not a second code path). Load polls the file's mtime rather than
// unconditionally re-reading and re-parsing it on every call.
type FileSource struct {
	path string

	mu       sync.Mutex
	cached   Config
	seenMod  time.Time
	haveSeen bool // distinguishes "never successfully stat'd" from a legitimate zero seenMod

	lastErr   ParseErrorCode
	lastErrAt time.Time
}

// NewFileSource builds a FileSource. Nothing is read at construction time;
// the first Load call performs the initial read.
func NewFileSource(path string) *FileSource {
	return &FileSource{path: path, cached: emptyConfig()}
}

// Load implements Source. A missing file, an unreadable one, an unparseable
// one, or one whose mtime has not changed since the last successfully
// processed read all return the last known-good config with changed=false
// and a nil error -- a bad moment on disk never tears down a running set.
//
// Every one of those EXCEPT the unchanged-mtime case also records the
// failure via LastParseError, for the upward file-mode report: never
// logged-and-forgotten, and never swallowed. The unchanged case must stay
// silent, since it is the steady state every poll lands in.
func (s *FileSource) Load(_ context.Context) (Config, bool, error) {
	info, err := os.Stat(s.path)
	if err != nil {
		return s.recordAccessFailure(err), false, nil
	}

	s.mu.Lock()
	unchanged := s.haveSeen && info.ModTime().Equal(s.seenMod)
	current := s.cached
	s.mu.Unlock()
	if unchanged {
		return current, false, nil
	}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		// Reached by its own right (an unreadable but stat-able file) and
		// by the stat->read race (the file was removed in between), which
		// is why this classifies rather than assuming "unreadable".
		return s.recordAccessFailure(err), false, nil
	}

	cfg, err := ParseConfig(raw)
	if err != nil {
		slog.Debug("runtime: local runtime-config file parse failed; keeping current config", "path", s.path, "error", err)
		s.mu.Lock()
		// The mtime is recorded even on a parse failure -- this exact
		// broken content has now been seen, so an operator who does not
		// touch the file again does not pay a re-read+re-parse cost on
		// every subsequent poll.
		s.seenMod = info.ModTime()
		s.haveSeen = true
		// The CODE, never err.Error(): this value is sent upward to the
		// gateway, and a parse error routinely quotes the offending line of
		// a file that may legitimately hold a plaintext secret. The full
		// detail stays in the local Debug log above. See ParseErrorCode.
		s.lastErr = ClassifyParseError(err)
		s.lastErrAt = time.Now()
		current := s.cached
		s.mu.Unlock()
		return current, false, nil
	}

	s.mu.Lock()
	s.cached = cfg
	s.seenMod = info.ModTime()
	s.haveSeen = true
	// Cleared only by this, the NEXT successful parse -- the same
	// convention as Spec's own LastError (types.go): a fixed file should
	// stop reporting a resolved problem.
	s.lastErr = ""
	s.lastErrAt = time.Time{}
	s.mu.Unlock()
	return cfg, true, nil
}

// recordAccessFailure records a stat or read failure the same way the parse
// branch records a parse failure, and returns the last known-good config for
// the caller to hand back. Splitting it out keeps both call sites honest:
// they used to differ from the parse branch only by omission, and the
// omission was invisible.
//
// THREE THINGS HAPPEN HERE, each for its own reason.
//
//  1. haveSeen is CLEARED. Without it a file that comes back with an mtime
//     the source has already seen -- restored from a backup that preserves
//     timestamps, or an atomic replace by a copy of the same bytes -- takes
//     the unchanged-mtime shortcut, never re-parses, and so never clears
//     lastErr: the portal would report a failure that has been over for
//     hours. The cost is that a transient read error forces one re-read and
//     re-parse of possibly identical content (Load then returns
//     changed=true once); applying an unchanged config is idempotent, and a
//     stuck error banner is not.
//  2. The failure is recorded only when the CODE CHANGES. The condition
//     that produced it is usually persistent (a wrong path stays wrong), so
//     lastErrAt is the moment the failure was first seen and does not creep
//     forward with every poll. The report still carries the code on every
//     send -- what is suppressed is the restamping, not the reporting.
//  3. The log line follows the same edge, for the same reason: the file is
//     polled continuously, and a Debug line per poll for a condition that
//     is not changing is how a log stops being read.
func (s *FileSource) recordAccessFailure(err error) Config {
	code := classifyFileAccessError(err)

	s.mu.Lock()
	s.haveSeen = false
	s.seenMod = time.Time{}
	edge := s.lastErr != code
	if edge {
		s.lastErr = code
		s.lastErrAt = time.Now()
	}
	current := s.cached
	s.mu.Unlock()

	if edge {
		// The path and the full error are LOCAL diagnosis only; upward,
		// only the code travels. Same rule as the parse branch below.
		slog.Debug("runtime: local runtime-config file could not be read; keeping current config", "path", s.path, "code", code, "error", err)
	}
	return current
}

// LastParseError returns the most recent load failure's wire CLASSIFICATION
// CODE and timestamp, cleared by the next successful parse. An empty code
// (with a zero time) means no failure is currently on record. The code --
// never the underlying error text -- is what the file-mode report carries
// upward; see ParseErrorCode for why, including why a set named for parsing
// also covers a missing or unreadable file.
//
// The timestamp is when the CURRENT failure was recorded, not when it was
// last re-observed: a persistent failure keeps its original time until the
// code changes or a successful parse clears it.
func (s *FileSource) LastParseError() (ParseErrorCode, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr, s.lastErrAt
}
