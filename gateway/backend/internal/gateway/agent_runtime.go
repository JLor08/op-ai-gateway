// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"strings"
	"time"
)

// handleAgentRuntimeConfig serves ONE ServerAgent the desired agent-managed
// runtime state for its OWN server (agent-runtime-manager spec §11): which
// model processes may run, with what command line, on which GPUs, which
// pairs may be co-resident, and the per-GPU VRAM budgets.
//
// Like handleAgentProxyRoutes, the target server comes ONLY from the agent
// token (authenticateAgent's ExtractBearerSecret -> HashSecret ->
// LookupAgentToken prologue): there is no parameter, path segment, or body
// field that can redirect the lookup, so one agent can never read another
// server's runtime configuration.
//
// A conditional GET (If-None-Match against the opaque ETag, which covers the
// exact document) answers 304 with no body -- the steady state, so an idle
// fleet does not re-fetch an unchanged configuration on every poll. The ETag
// also doubles as the WS push / file-mode schema version (design spec §10).
func (s *Server) handleAgentRuntimeConfig(w http.ResponseWriter, r *http.Request) {
	// Set on EVERY response path (before method/auth checks), matching every
	// other Bearer-token agent endpoint: this traffic flows through the public
	// listener behind fronting infra by default and must never be cached there.
	w.Header().Set("Cache-Control", "no-store")
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	principal, ok := s.authenticateAgent(w, r)
	if !ok {
		return
	}
	serverID := principal.ServerID
	if s.Portal == nil {
		writeJSON(w, http.StatusOK, portal.AgentRuntimeConfigDTO{
			GPUBudgets: []portal.AgentGPUBudgetDTO{},
			Specs:      []portal.AgentRuntimeSpecDTO{},
			Coresident: [][2]string{},
		})
		return
	}
	dto, err := s.Portal.AgentRuntimeConfig(r.Context(), serverID)
	if err != nil {
		// Deliberately static: err.Error() could carry store internals.
		slog.Error("agent runtime config derivation failed", "server_id", serverID, "err", err)
		writeJSON(w, http.StatusInternalServerError, apierror.Response("runtime_config.read_failed", "could not derive runtime config", ""))
		return
	}
	if dto.GPUBudgets == nil {
		dto.GPUBudgets = []portal.AgentGPUBudgetDTO{}
	}
	if dto.Specs == nil {
		dto.Specs = []portal.AgentRuntimeSpecDTO{}
	}
	if dto.Coresident == nil {
		dto.Coresident = [][2]string{}
	}
	for i := range dto.Specs {
		if dto.Specs[i].Args == nil {
			dto.Specs[i].Args = []string{}
		}
		if dto.Specs[i].Env == nil {
			dto.Specs[i].Env = map[string]string{}
		}
		if dto.Specs[i].GPUs == nil {
			dto.Specs[i].GPUs = []portal.AgentRuntimeSpecGPUDTO{}
		}
	}

	etag := `"` + dto.ETag + `"`
	w.Header().Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), dto.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// pushRuntimeConfigTimeout bounds the s.Portal.AgentRuntimeConfig store read
// PushRuntimeConfig performs -- a package-level var (not const), following
// the established shrink-in-tests pattern (chat_runs.go's
// runCheckpointInterval) so a test can drive it down to milliseconds without
// sleeping out a real multi-second deadline. 5s matches capture.go's
// persistCapture timeout for its SaveCapture call, not model_warmer.go's 60s
// warmCallTimeout: AgentRuntimeConfig is a bounded local store-read assembly
// (a handful of RuntimeStore reads), the same shape and cost class as
// persisting one capture row, not a network round trip to an upstream
// inference server loading a model (what warmCallTimeout's 60s budgets for).
var pushRuntimeConfigTimeout = 5 * time.Second

// PushRuntimeConfig is the gateway half of the WS push path (agent-runtime-
// manager design spec §10, Phase 2 Task 8): a best-effort, feature-gated
// push of serverID's CURRENT runtime-config document -- the SAME
// AgentRuntimeConfig assembly handleAgentRuntimeConfig's GET path uses -- to
// every open agent WebSocket connection for that server, so a portal click
// reaches a connected agent immediately instead of waiting for the agent's
// next Task-7 poll. See AgentStreamRegistry.NotifyRuntimeConfig for why the
// frame carries the WHOLE document rather than a command or a delta.
//
// Two fail-closed preconditions gate delivery, checked before any store
// read: the connected agent must have DECLARED the runtime_manager feature
// (s.AgentFeatures.Has) -- an agent binary that never declared support would
// not understand this frame -- and the server must not be running in file
// mode (s.RuntimeStatus.IsFileMode) -- a file-mode agent manages its runtime
// from local disk config, not this push/pull loop. Neither gate failing is
// an error; it is simply nothing to push (the next poll or reconnect is
// always the backstop).
//
// Runs entirely in a goroutine: the intended caller is the portal write-path
// hook (portal.ServiceDeps.OnRuntimeConfigChanged / Service.
// SetRuntimeConfigChangedHook), wired to this method in
// cmd/gateway/main.go's buildGatewayServer via the
// gateway.ServerDeps.SetRuntimeConfigChangedHook handoff (buildRuntime hands
// portalService's own setter forward through ServerDeps, since portalService
// is constructed before this Server is, and the setter needs a bound
// PushRuntimeConfig -- see that ServerDeps field's doc for the full
// construction-order rationale). The hook's contract is "synchronous but
// guaranteed fast" because it fires from inside a runtime-spec CRUD write
// that still holds its own serializing lock -- the store read (s.Portal.
// AgentRuntimeConfig) and JSON marshal this method performs are both too
// slow to do inline there.
//
// The store read is bounded by pushRuntimeConfigTimeout (never
// context.Background() unbounded): one goroutine is spawned per portal
// write, so an unbounded call stuck on lock contention or a wedged query
// would accumulate goroutines without limit under sustained write pressure.
//
// Nil-safe throughout (a nil s.Portal, s.AgentFeatures, s.RuntimeStatus, or
// s.AgentStreams all degrade to "nothing pushed," never a panic), matching
// every other best-effort notifier in this package.
func (s *Server) PushRuntimeConfig(serverID string) {
	go func() {
		if !s.AgentFeatures.Has(serverID, "runtime_manager") || s.RuntimeStatus.IsFileMode(serverID) {
			return
		}
		if s.Portal == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), pushRuntimeConfigTimeout)
		defer cancel()
		dto, err := s.Portal.AgentRuntimeConfig(ctx, serverID)
		if err != nil {
			slog.Debug("push runtime config: derive failed", "server_id", serverID, "err", err)
			return
		}
		b, err := json.Marshal(dto)
		if err != nil {
			slog.Debug("push runtime config: marshal failed", "server_id", serverID, "err", err)
			return
		}
		s.AgentStreams.NotifyRuntimeConfig(serverID, b)
	}()
}

// --- Task 9: file-mode runtime report ingest --------------------------------

// runtimeReportEnvMask replaces every env VALUE (keys stay visible) in a
// reported file-mode config before it is stored. Defense in depth (design
// spec §10.2): the agent is REQUIRED to redact env values itself before
// sending (a local config file may legitimately hold a plaintext secret,
// e.g. an HF_TOKEN, that belongs to the server operator) -- but the gateway
// must never trust that. Re-parsing Config into a typed struct and
// overwriting every value here means a buggy or compromised agent cannot
// make the gateway persist a secret, regardless of what it actually sent.
const runtimeReportEnvMask = "•••"

// runtimeReportParseErrorGeneric is the fixed, content-free string
// substituted for a file-mode report's ParseError whenever it is not one of
// the classification codes the agent's wire contract allows. ParseError is
// PURELY diagnostic -- the operator needs to know the local config file
// failed to parse and roughly why, never any of the offending content -- so
// the safe outcome is the DEFAULT here, not a fallback bolted onto a
// keep-by-default rule.
const runtimeReportParseErrorGeneric = "config parse error"

// runtimeReportParseErrorCodes is the ALLOW-LIST half of the file-mode
// parse-error wire contract. The producing side is the agent's
// runtime.ParseErrorCode closed set (server-agent/internal/runtime/types.go),
// which documents the same contract from the other end; these string values
// and that set must change together, and the agent carries a test that fails
// on a rename for exactly that reason. Deliberately a literal map rather than
// an import: the two Go modules share no code, and the whole point of a
// closed set is that this side states independently what it will accept.
//
// Not listed here (including the agent's own defensive "unclassified") means
// the value degrades to runtimeReportParseErrorGeneric.
var runtimeReportParseErrorCodes = map[string]bool{
	"json_syntax":       true,
	"duplicate_spec_id": true,
}

// redactRuntimeReportParseError keeps a file-mode report's parse_error only
// when it is exactly one of the agent's classification codes, and returns the
// fixed runtimeReportParseErrorGeneric constant for anything else. An EMPTY
// input stays empty -- "this agent reported no parse failure" is not a
// redaction case at all.
//
// This is defense in depth of the same kind as runtimeReportEnvMask, on a
// neighbouring field: a config-loader error routinely quotes the offending
// source line verbatim, and in this schema that line may legitimately hold a
// plaintext secret (an HF_TOKEN written directly rather than as an
// ${AGENT_ENV:NAME} placeholder). The gateway must never store agent-chosen
// free text here, however well-behaved the agent is expected to be.
//
// TWO EARLIER RULES, AND WHY BOTH FAILED -- worth keeping, because each was a
// reasonable-looking answer to the half of the problem it could see:
//
//  1. Round 1 kept everything up to the first ':' and passed a colon-less
//     string through untouched. Review found two leak shapes: a colon-less
//     message leaked verbatim (nothing to cut), and a secret sitting BEFORE
//     the first colon survived (the split kept precisely the wrong half).
//  2. Round 2 therefore kept the prefix ONLY when it looked like a bare
//     classification token (no whitespace/quote/'=', bounded length). That
//     closed the leak and broke the field: the actual producer is
//     runtime.ParseConfig, whose every error begins "runtime: ", so the ONE
//     reachable non-generic output for every malformed file an operator could
//     write was the single word "runtime" -- a token that looks like a
//     meaningful subsystem tag and carries no information whatsoever. It also
//     rewrote the EMPTY string (the healthy case, since parse_error is
//     omitempty) to "config parse error", so every file-mode agent whose
//     config parsed perfectly was stored, and rendered in the portal, as one
//     that had failed to parse -- with the portal suppressing the config view
//     on exactly that field.
//
// An allow-list over a closed set ends the fight between redaction and
// diagnosis instead of picking a winner: the wire contract STATES what this
// field may contain, both sides can be read against it, and free text has no
// path in at all.
func redactRuntimeReportParseError(s string) string {
	if s == "" {
		return ""
	}
	if runtimeReportParseErrorCodes[s] {
		return s
	}
	return runtimeReportParseErrorGeneric
}

// agentRuntimeReportSpecGPU mirrors AgentRuntimeSpecGPUDTO (portal's
// runtime-config wire shape, §11) -- the file-mode local config uses EXACTLY
// that schema (design spec §10.2: "one parser, one validation, one
// reconciler; the mode is a source switch"). A local gateway-side mirror,
// not a reuse of the portal type, matching this file's existing convention
// for every other agent wire struct (agentSystemReport, agentTelemetryRequest).
type agentRuntimeReportSpecGPU struct {
	Index  int `json:"index"`
	VRAMMB int `json:"vram_mb"`
}

// agentRuntimeReportSpec mirrors AgentRuntimeSpecDTO. Env is the ONLY field
// this schema uses to carry secrets (design spec §4.6: "env values are
// referential"), so it is the sole redaction target below.
type agentRuntimeReportSpec struct {
	ID                          string                      `json:"id"`
	Model                       string                      `json:"model"`
	UpstreamModel               string                      `json:"upstream_model"`
	Binary                      string                      `json:"binary"`
	Args                        []string                    `json:"args"`
	Env                         map[string]string           `json:"env"`
	WorkDir                     string                      `json:"work_dir,omitempty"`
	GPUs                        []agentRuntimeReportSpecGPU `json:"gpus"`
	ListenPort                  int                         `json:"listen_port"`
	HealthPath                  string                      `json:"health_path"`
	HealthTimeoutSeconds        int                         `json:"health_timeout_seconds"`
	StartupTimeoutSeconds       int                         `json:"startup_timeout_seconds"`
	IdleTimeoutSeconds          int                         `json:"idle_timeout_seconds"`
	AdmissionWaitTimeoutSeconds int                         `json:"admission_wait_timeout_seconds"`
	Pinned                      bool                        `json:"pinned"`
	AdminState                  string                      `json:"admin_state"`
}

// agentRuntimeReportGPUBudget mirrors AgentGPUBudgetDTO.
type agentRuntimeReportGPUBudget struct {
	Index    int `json:"index"`
	BudgetMB int `json:"budget_mb"`
}

// agentRuntimeReportConfig mirrors AgentRuntimeConfigDTO (the runtime-config
// document shape, §11) -- the local file a file-mode agent reads uses this
// exact schema, and this is what a file-mode report's Config field carries
// back up. Re-parsing the reported Config into this fully-typed struct (never
// json.RawMessage passthrough) is itself part of the defense-in-depth: an
// unknown field a buggy/compromised agent might inject is silently dropped by
// the unmarshal, never round-tripped into the stored canonical blob -- the
// SAME re-marshal-what-you-validated technique sanitizeSystemReport uses.
type agentRuntimeReportConfig struct {
	RouterListen int                           `json:"router_listen"`
	MaxProcesses int                           `json:"max_processes"`
	GPUBudgets   []agentRuntimeReportGPUBudget `json:"gpu_budgets"`
	Specs        []agentRuntimeReportSpec      `json:"specs"`
	Coresident   [][2]string                   `json:"coresident"`
	ETag         string                        `json:"etag,omitempty"`
}

// agentRuntimeReport mirrors the agent's file-mode upward report (design
// spec §10.2): which config source produced Config, when the agent last
// (re)loaded it, any parse error from that load (Config may legitimately be
// the zero value alongside a non-empty ParseError -- a broken file keeps the
// agent's last-good runtime running but still reports why disk state
// couldn't be adopted), and the effective config itself.
type agentRuntimeReport struct {
	Source      string          `json:"source"`
	CollectedAt time.Time       `json:"collected_at"`
	ParseError  string          `json:"parse_error,omitempty"`
	Config      json.RawMessage `json:"config"`
}

// errAgentRuntimeReportInvalid: the runtime-report payload failed to parse
// (POST -> 400 agent.runtime_report_invalid; WS -> skip the frame, keep
// streaming). Mirrors errAgentSystemReportInvalid.
var errAgentRuntimeReportInvalid = errors.New("agent runtime report: invalid payload")

// runtimeReportInvalidError wraps a concrete parse error while matching
// errAgentRuntimeReportInvalid via errors.Is (so the POST 400 body carries
// detail), mirroring systemReportInvalidError.
type runtimeReportInvalidError struct{ cause error }

func (e *runtimeReportInvalidError) Error() string { return e.cause.Error() }
func (e *runtimeReportInvalidError) Unwrap() error { return e.cause }
func (e *runtimeReportInvalidError) Is(target error) bool {
	return target == errAgentRuntimeReportInvalid
}

// sanitizeRuntimeReportConfig re-parses raw into the fully-typed
// agentRuntimeReportConfig, masks every spec's env values, normalizes every
// collection-shaped field to non-nil, and re-marshals canonically. An empty
// or unparseable raw (a file-mode agent reporting a load failure via
// ParseError, or a forward-incompatible/hostile payload) yields "{}" rather
// than an error -- Config failing to parse must never reject the WHOLE
// report, which still carries meaningful Source/ParseError/CollectedAt.
func sanitizeRuntimeReportConfig(raw json.RawMessage) json.RawMessage {
	var cfg agentRuntimeReportConfig
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			cfg = agentRuntimeReportConfig{}
		}
	}
	if cfg.GPUBudgets == nil {
		cfg.GPUBudgets = []agentRuntimeReportGPUBudget{}
	}
	if cfg.Coresident == nil {
		cfg.Coresident = [][2]string{}
	}
	if cfg.Specs == nil {
		cfg.Specs = []agentRuntimeReportSpec{}
	}
	for i := range cfg.Specs {
		if cfg.Specs[i].Env == nil {
			cfg.Specs[i].Env = map[string]string{}
		}
		for k := range cfg.Specs[i].Env {
			cfg.Specs[i].Env[k] = runtimeReportEnvMask
		}
		if cfg.Specs[i].Args == nil {
			cfg.Specs[i].Args = []string{}
		}
		if cfg.Specs[i].GPUs == nil {
			cfg.Specs[i].GPUs = []agentRuntimeReportSpecGPU{}
		}
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		// Impossible for these field types; fall back to an empty object
		// (mirrors sanitizeSystemReport's equivalent fallback).
		return json.RawMessage("{}")
	}
	return json.RawMessage(b)
}

// ingestRuntimeReport is the transport-agnostic core shared by the POST
// handler (handleAgentRuntimeReport) and the WS reader (handleAgentStream
// case "runtime_report"), mirroring ingestSystemReport exactly: parse,
// sanitize (clamp Source, redact+clamp ParseError, redact Config's env
// values), existence-check the server, and upsert server_runtime_reports.
// Returns the same typed sentinels the system-report path uses so each
// transport maps them itself.
//
// After a successful upsert, it flips RuntimeStatusRegistry's per-server
// file-mode flag (report.Source == "file") -- consulted by PushRuntimeConfig
// so the gateway stops sending runtime_config frames to a file-mode agent
// (which would discard them anyway; this is belt-and-suspenders, not the
// only guard).
func (s *Server) ingestRuntimeReport(ctx context.Context, serverID string, raw json.RawMessage) error {
	var req agentRuntimeReport
	if err := json.Unmarshal(raw, &req); err != nil {
		return &runtimeReportInvalidError{cause: err}
	}
	now := time.Now().UTC()
	req.Source = clampHardwareString(strings.TrimSpace(req.Source))
	req.ParseError = clampHardwareString(redactRuntimeReportParseError(req.ParseError))
	collectedAt := req.CollectedAt
	if collectedAt.IsZero() {
		collectedAt = now
	}
	req.CollectedAt = collectedAt
	req.Config = sanitizeRuntimeReportConfig(req.Config)
	canonical, err := json.Marshal(req)
	if err != nil {
		// Impossible for these field types; fall back to an empty object
		// (mirrors sanitizeSystemReport's equivalent fallback).
		canonical = []byte("{}")
	}
	if _, err := s.Routes.AIServerByID(ctx, serverID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			slog.Warn("agent runtime report rejected: unknown server", "server_id", serverID)
			return errAgentUnknownServer
		}
		slog.Error("agent runtime report: server lookup failed", "server_id", serverID, "err", err)
		return &storeTelemetryError{code: "agent.server_lookup_failed", message: "server lookup failed", cause: err}
	}
	report := routing.ServerRuntimeReport{ServerID: serverID, CollectedAt: collectedAt, ReportJSON: string(canonical), UpdatedAt: now}
	if err := s.Routes.UpsertServerRuntimeReport(ctx, report); err != nil {
		slog.Error("agent runtime report: upsert failed", "server_id", serverID, "err", err)
		return &storeTelemetryError{code: "agent.runtime_report_failed", message: "runtime report upsert failed", cause: err}
	}
	// Deliberately AFTER the store write succeeded, mirroring every other
	// post-write registry update in this package: a report is evidence about
	// this agent's own config source, and stamping it while the report
	// itself failed to persist would claim freshness the gateway does not
	// have.
	s.RuntimeStatus.SetFileMode(serverID, req.Source == "file")
	slog.Debug("agent runtime report stored", "server_id", serverID, "source", req.Source)
	return nil
}

// agentRuntimeReportErrRows is writeAgentRuntimeReportError's one
// mapper-specific row (errAgentRuntimeReportInvalid keeps its dynamic message
// via msgFn); errAgentUnknownServer maps identically in writeAgentIngestError
// and writeAgentSystemReportError and lives in sharedErrorMap instead.
var agentRuntimeReportErrRows = []errRow{
	{err: errAgentRuntimeReportInvalid, status: http.StatusBadRequest, code: "agent.runtime_report_invalid", msgFn: func(err error) string { return err.Error() }},
}

// writeAgentRuntimeReportError maps an ingestRuntimeReport error to an HTTP
// response (POST only; the WS reader ignores the code and closes).
func writeAgentRuntimeReportError(w http.ResponseWriter, err error) {
	if writeMappedError(w, err, agentRuntimeReportErrRows, 0, "", "") {
		return
	}
	var se *storeTelemetryError
	if errors.As(err, &se) {
		writeJSON(w, http.StatusInternalServerError, apierror.Response(se.code, se.message, ""))
		return
	}
	writeJSON(w, http.StatusInternalServerError, apierror.Response("agent.runtime_report_failed", "runtime report ingest failed", ""))
}

// handleAgentRuntimeReport is the POST /api/agent/v1/runtime-report endpoint
// (mirrors handleAgentSystemReport): bearer -> LookupAgentToken -> readRawJSON
// -> ingest.
func (s *Server) handleAgentRuntimeReport(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	principal, ok := s.authenticateAgent(w, r)
	if !ok {
		return
	}
	serverID := principal.ServerID
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	if err := s.ingestRuntimeReport(r.Context(), serverID, raw); err != nil {
		writeAgentRuntimeReportError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "server_id": serverID})
}
