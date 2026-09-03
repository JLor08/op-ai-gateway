// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync/atomic"
	"time"
)

// jsonContentType is the usage-record ContentType this file's translate-path
// error/mismatch branches stamp below (the actual client body is always JSON
// here, unlike the native-proxy path which forwards the upstream's own
// Content-Type).
const jsonContentType = "application/json"

// Stable error codes (AGENTS.md: dotted lowercase noun.reason; the code string is
// the wire contract) for a coding-agent endpoint the resolved app/spec has
// DISABLED. Written inline like model.not_allowed — not sentinel-mapped.
const (
	codeResponsesEndpointDisabled = "responses.endpoint_disabled"
	codeMessagesEndpointDisabled  = "messages.endpoint_disabled"
	msgEndpointDisabled           = "endpoint disabled for this model"
	// statusEndpointDisabled: "this model does not serve this endpoint here"
	// (absence) — 404, matching the codebase's model-scoped-not-found posture.
	// (Alternative aligned with model.not_allowed: http.StatusForbidden.)
	statusEndpointDisabled = http.StatusNotFound
)

// endpointDisabledError returns the stable code + HTTP status for a disabled
// coding-agent endpoint, per client API flavor.
func endpointDisabledError(apiFlavor string) (string, int) {
	if apiFlavor == "anthropic_messages" {
		return codeMessagesEndpointDisabled, statusEndpointDisabled
	}
	return codeResponsesEndpointDisabled, statusEndpointDisabled
}

// endpointModeFor returns the upstream native path for the given client API flavor
// and the EFFECTIVE EndpointMode the resolved Target carries for it. The Target's
// mode is the resolved runtime spec's value for a server_agent app (resolved before
// dispatch), else the application's — targetFrom (routing/resolver.go) sets it.
// Codex uses the OpenAI Responses API (/v1/responses); Claude Code uses the
// Anthropic Messages API (/v1/messages). A flavor that is neither yields ("", ""),
// which every caller treats as translate.
func endpointModeFor(target routing.Target, apiFlavor string) (string, routing.EndpointMode) {
	switch apiFlavor {
	case "openai_responses":
		return "/v1/responses", target.ResponsesMode
	case "anthropic_messages":
		return "/v1/messages", target.MessagesMode
	}
	return "", ""
}

// targetServesFlavor reports whether target's effective APIFlavors (the resolved
// application's, or — for a server_agent mapping — the resolved runtime spec's;
// see targetFrom) includes the request's COARSE api flavor ("openai"/"anthropic").
// This is the other half of the effective-served rule the docs promise (§6 /
// ADR-033): "served iff flavor ∈ APIFlavors AND mode != disabled" — endpointModeFor
// only ever reads the mode half. For an ORDINARY app this is a no-op check
// (candidacy already excluded a flavor-less app before dispatch ever saw it — see
// applicationServesEndpoint); for a server_agent app it is the ONLY place the
// spec's (possibly narrower than the app's) flavor set is enforced, since
// candidacy deliberately gates that app type on its own coarser, app-level
// flavors only (the spec is the per-model authority, knowable only post-resolve).
func targetServesFlavor(target routing.Target, apiFlavor string) bool {
	coarse := routing.NormalizeAPIFlavor(apiFlavor)
	for _, f := range target.APIFlavors {
		if f == coarse {
			return true
		}
	}
	return false
}

// upstreamPath returns the endpoint PATH the gateway calls on the upstream for a
// RESOLVED target + client API flavor: the native passthrough path when the
// effective mode for that flavor is passthrough, otherwise the built-in
// translation's chat-completions path (per provider — ollama speaks /api/chat, all
// OpenAI-compatible providers speak /v1/chat/completions). It returns "" for an
// unresolved target (e.g. a resolve failure, where no upstream was called). This
// mirrors the paths hardcoded in the provider clients (openai_compatible.go,
// ollama.go) and in proxyNative, kept here in one gateway-visible place so the
// persisted usage row + the live ActiveRequest agree on the value.
//
// KNOWN LIMIT (cosmetic, diagnostic field only): a translate handler derives the
// value from its own (re-)resolved target. If, on the same request, routing flips
// to a passthrough-mode application between tryProxyNative's resolve and the
// handler's resolve (the documented idempotent double-resolve — requires two
// applications for one model with different endpoint modes), this reports the
// native path while the provider actually called the translate path. Not
// client-reachable in practice and only mislabels one column; see
// docs/implementation-status.md.
func upstreamPath(target routing.Target, apiFlavor string) string {
	if target.Provider == "" {
		return ""
	}
	if p, mode := endpointModeFor(target, apiFlavor); mode == routing.EndpointModePassthrough {
		return p
	}
	if target.Provider == routing.ProviderOllama {
		return "/api/chat"
	}
	return "/v1/chat/completions"
}

// effectiveProviderModel is the model name actually sent to the upstream AI-server
// application: the per-model provider override (target.ProviderModel) when set,
// else the requested model passed through unchanged. It mirrors the provider
// layer's providerModel() (translate path) and rewriteModelField()'s fallback
// (native passthrough), so the provider_model shown in Activity always reflects
// what the application received — never blank.
func effectiveProviderModel(target routing.Target, requested string) string {
	if target.ProviderModel != "" {
		return target.ProviderModel
	}
	return requested
}

// sniffRoutingModel tolerantly extracts the routing model (+ stream flag) from
// a raw request body via a minimal untyped probe, BEFORE any flavor-specific
// (compat) parse. This is what lets the admission gate (inferencePreflight)
// run ahead of tryProxyNative even for a rich Codex/Claude Code body that the
// lossy compat parser would reject outright.
//
// Tolerant: a malformed body (or one whose "model" field doesn't survive this
// probe, e.g. a JSON type mismatch) yields an empty model. The caller must
// treat that as "the gate cannot run yet" — never call inferencePreflight or
// resolveModelOverride with it — and fall straight through to the real,
// flavor-specific parse for a proper request-error response; see the callers
// in inference_handlers.go for exactly how that bail is structured.
func sniffRoutingModel(raw []byte) (model string, stream bool) {
	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(raw, &probe)
	return strings.TrimSpace(probe.Model), probe.Stream
}

// tryProxyNative decides whether to handle the request via native passthrough,
// given a preflight the caller already computed for this request (see
// inferencePreflight — override resolution, server-override re-authorization,
// the model allowlist, admission, and session extraction all ran EXACTLY ONCE
// there, before this was ever called; tryProxyNative no longer runs any of its
// own copy of that gate). It resolves the target from pf.Req and — only if the
// resolved application has the matching native flag — proxies the raw body to
// the upstream and returns true. On any resolve failure or a non-native
// application it returns false, leaving the caller's existing translate path
// to handle (and properly error-record) the request, reusing the SAME pf
// rather than re-running the gate a second time.
func (s *Server) tryProxyNative(w http.ResponseWriter, r *http.Request, token auth.Token, raw []byte, apiFlavor string, pf preflight) bool {
	start := time.Now()
	req := pf.Req
	model := req.Model
	// The native decision must be made BEFORE the lossy compat parse (which would
	// reject rich Codex/Claude bodies), so it resolves here. When the app is NOT
	// native, the caller's translate path resolves once more — an extra, idempotent
	// resolve on the /v1/responses + /v1/messages non-native path only (chat
	// completions is untouched). We accept that over threading a pre-resolved target
	// through the battle-tested complete/completeStream* functions.
	target, err := s.resolveTarget(r.Context(), token, req)
	if err != nil {
		// An admission-queue rejection (CP4: timeout or full) is TERMINAL for the request —
		// surface the 503 here rather than returning false, otherwise the translate path
		// would re-resolve and WAIT on the queue a second time (up to 2x the timeout). We
		// record it exactly like the translate error path so the usage row + capture are
		// consistent. Mark the request handled (true).
		if errors.Is(err, routing.ErrAdmissionQueueTimeout) || errors.Is(err, routing.ErrAdmissionQueueFull) {
			id := nextRequestID()
			capturing := s.capturingEnabled(token)
			ireq := req
			slog.Warn("native passthrough admission rejected", "path", r.URL.Path, "api_flavor", apiFlavor, "model", model, "code", completionErrorCode(err), "status", completionHTTPStatus(err))
			body := writeCompletionErrorCaptured(w, err)
			s.recordUsage(start, token, ireq, routing.Target{}, provider.Response{}, completionErrorCode(err), "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: completionHTTPStatus(err), ContentType: jsonContentType}, id, buildCaptureInput(capturing, token.UserID, token.Secret, r, raw, w.Header(), body, completionHTTPStatus(err), apiFlavor))
			return true
		}
		// Routing failed (no route for the model, or the application is currently
		// unreachable). Passthrough can't apply; the translate path will re-resolve
		// and record the proper error (or, for a rich Codex/Claude body, fail at
		// parse). Surface WHY here so the cause is visible in the portal Logs view.
		slog.Debug("native passthrough not applied: routing failed", "path", r.URL.Path, "api_flavor", apiFlavor, "model", model, "err", err)
		return false
	}
	path, mode := endpointModeFor(target, apiFlavor)
	// The effective-served rule is a CONJUNCT (§6 / ADR-033): flavor ∈ APIFlavors
	// AND mode != disabled. endpointModeFor above only ever answers the mode half;
	// for a coding-agent endpoint (path != "") whose coarse flavor the resolved
	// target no longer serves, treat it as disabled here — the one place a
	// server_agent app's per-model spec flavor is enforceable (candidacy only
	// checked the app-level flavor; see targetServesFlavor). A no-op for an
	// ordinary app, whose flavor-less state already excluded it at candidacy.
	if path != "" && mode != routing.EndpointModeDisabled && !targetServesFlavor(target, apiFlavor) {
		mode = routing.EndpointModeDisabled
	}
	switch mode {
	case routing.EndpointModePassthrough:
		s.proxyNative(w, r, token, target, path, raw, req)
		return true
	case routing.EndpointModeDisabled:
		// The resolved application (or, for a server_agent app, the resolved runtime
		// spec whose effective mode targetFrom surfaced onto the Target) has this
		// coding-agent endpoint turned OFF. For an ordinary app, candidate
		// eligibility already excluded it; for a server_agent app the per-model
		// disabled state is only knowable HERE, after model resolution — so this is
		// the one place that rejects it, with a stable error code + 4xx, and WITHOUT
		// falling through to the (lossy) translate path. Recorded as a failed usage
		// event against the resolved target so the rejection is visible in
		// Activity/Logs, mirroring the admission-timeout terminal branch above.
		id := nextRequestID()
		capturing := s.capturingEnabled(token)
		code, status := endpointDisabledError(apiFlavor)
		slog.Debug("native passthrough rejected: endpoint disabled",
			"path", r.URL.Path, "api_flavor", apiFlavor, "model", model,
			"server", s.serverName(target.ServerID), "code", code, "status", status)
		body := writeJSONCaptured(w, status, apierror.Response(code, msgEndpointDisabled, ""))
		s.recordUsage(start, token, req, target, provider.Response{}, code, "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: status, ContentType: jsonContentType}, id, buildCaptureInput(capturing, token.UserID, token.Secret, r, raw, w.Header(), body, status, apiFlavor))
		return true
	default:
		// translate (or an unpopulated "" mode — treated as translate, the safe
		// fall-back): hand off to the compat translate path exactly as a non-native
		// app did before. The rich-body reject happens at that path's parse.
		slog.Debug("native passthrough not applied: endpoint mode is translate",
			"path", r.URL.Path, "api_flavor", apiFlavor, "model", model,
			"server", s.serverName(target.ServerID),
			"responses_mode", string(target.ResponsesMode), "messages_mode", string(target.MessagesMode))
		return false
	}
}

// proxyNative forwards the raw client body to the upstream's native endpoint and
// streams the raw response back byte-for-byte, so protocol-specific content (Codex
// tool calls, reasoning items, Claude Code content blocks) is preserved exactly. It
// mirrors completeStream's idle-watchdog / write-deadline / capture / usage-record
// machinery. The only body edit is rewriting the `model` field to the upstream's
// mapped name (lossless; all other fields untouched).
func (s *Server) proxyNative(w http.ResponseWriter, r *http.Request, token auth.Token, target routing.Target, path string, raw []byte, pfReq inference.Request) {
	start := time.Now()
	id := nextRequestID()
	capturing := s.capturingEnabled(token)
	// SessionID mirrors the translate path so usage/activity rows carry it for
	// native-passthrough traffic too (it's also what keyed the routing affinity).
	nativeEndpoint := endpointResponses
	if routing.NormalizeAPIFlavor(pfReq.APIFlavor) == routing.APIFlavorAnthropic {
		nativeEndpoint = endpointMessages
	}
	si := extractClientSession(r.Header, raw, nativeEndpoint)
	req := inference.Request{
		Model:           pfReq.Model,
		RequestedModel:  pfReq.RequestedModel,
		APIFlavor:       pfReq.APIFlavor,
		Stream:          pfReq.Stream,
		SessionID:       si.ExplicitHeader,
		ClientSessionID: si.ClientSession,
		SessionSource:   si.Source,
		AgentID:         si.AgentID,
	}
	serverName := s.serverName(target.ServerID)

	proxyClient, ok := s.Provider.(provider.NativeProxyClient)
	if !ok {
		body := writeJSONCaptured(w, http.StatusBadGateway, apierror.Response("provider.unavailable", "native passthrough not supported", ""))
		s.recordUsage(start, token, req, target, provider.Response{}, "provider.unavailable", "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: http.StatusBadGateway, ContentType: jsonContentType}, id, buildCaptureInput(capturing, token.UserID, token.Secret, r, raw, w.Header(), body, http.StatusBadGateway, pfReq.APIFlavor))
		return
	}

	upstreamBody := rewriteModelField(raw, target.ProviderModel)

	// Deadline policy: a stream uses an idle watchdog (cancel on no upstream
	// activity for `idle`), a buffered completion uses a total timeout. Both cancel
	// the upstream request context.
	idle := s.streamIdleTimeout
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var idledOut atomic.Bool
	var watchdog *time.Timer
	switch {
	case pfReq.Stream && idle > 0:
		watchdog = time.AfterFunc(idle, func() { idledOut.Store(true); cancel() })
		defer watchdog.Stop()
	case !pfReq.Stream && target.Timeout > 0:
		var tcancel context.CancelFunc
		ctx, tcancel = context.WithTimeout(ctx, target.Timeout)
		defer tcancel()
	}

	s.Active.Add(ActiveRequest{ID: id, UserID: token.UserID, TokenID: token.ID, TokenName: token.Name, ServiceID: token.ServiceID, ServiceName: token.ServiceName, ServerName: serverName, ServerID: target.ServerID, Model: pfReq.Model, RequestedModel: pfReq.RequestedModel, APIFlavor: pfReq.APIFlavor, ReqPath: r.URL.Path, ProviderPath: path, ProviderModel: effectiveProviderModel(target, pfReq.Model), SessionID: si.ClientSession, SessionSource: si.Source, AgentID: si.AgentID, Stream: pfReq.Stream, StartedAt: start})
	defer s.Active.Remove(id)

	slog.Debug("inference request (native passthrough)", "path", r.URL.Path, "api_flavor", pfReq.APIFlavor, "model", pfReq.Model, "stream", pfReq.Stream, "server", serverName, "upstream_path", path, "token_id", token.ID, "user_id", token.UserID)

	// Attach the resolved application's per-app upstream credential (fail-open).
	ctx = s.upstreamAuthCtx(ctx, target)
	resp, err := proxyClient.ProxyNative(ctx, target, path, upstreamBody)
	if err != nil {
		// Pre-response failure: nothing written to the client yet, so return a JSON error.
		code := completionErrorCode(err)
		httpStatus := completionHTTPStatus(err)
		slog.Error("inference native passthrough failed", "path", r.URL.Path, "api_flavor", pfReq.APIFlavor, "model", pfReq.Model, "server", serverName, "code", code, "err", err)
		body := writeCompletionErrorCaptured(w, err)
		s.recordUsage(start, token, req, target, provider.Response{}, code, "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: httpStatus, ContentType: jsonContentType}, id, buildCaptureInput(capturing, token.UserID, token.Secret, r, raw, w.Header(), body, httpStatus, pfReq.APIFlavor))
		return
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = jsonContentType
	}
	w.Header().Set("Content-Type", contentType)
	if pfReq.Stream {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
	}
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	rc := http.NewResponseController(w)

	// Copy the upstream body to the client, flushing each chunk (so SSE frames
	// reach the client live), and re-arming the idle watchdog + write deadline on
	// activity. respBuf tees a bounded copy (~1 MB) that feeds BOTH usage parsing
	// (always) and capture (only when enabled — buildCaptureInput drops it
	// otherwise). A response larger than the cap may lose a trailing usage frame;
	// token accounting for passthrough is explicitly best-effort.
	var respBuf bytes.Buffer
	copier := &nativeCopier{w: w, rc: rc, flusher: flusher, watchdog: watchdog, idle: idle, respBuf: &respBuf, capBytes: s.captureMaxBytes}
	copyErr := copier.run(resp.Body)

	status, errorCode := s.nativeTerminalStatus(r, resp.StatusCode, pfReq, serverName, idledOut.Load(), copyErr, start)

	usg := parsePassthroughUsage(pfReq.APIFlavor, respBuf.Bytes())
	s.recordUsage(start, token, req, target, provider.Response{Usage: usg}, errorCode, status, usageMeta{ReqPath: r.URL.Path, HTTPStatus: resp.StatusCode, ContentType: contentType}, id, buildCaptureInput(capturing, token.UserID, token.Secret, r, raw, w.Header(), respBuf.Bytes(), resp.StatusCode, pfReq.APIFlavor))
}

// nativeCopier bundles everything proxyNative's body-copy needs: the client
// writer (+ flusher/write-deadline controller), the idle watchdog to re-arm on
// activity, and the bounded tee buffer feeding usage parsing/capture.
type nativeCopier struct {
	w        http.ResponseWriter
	rc       *http.ResponseController
	flusher  http.Flusher
	watchdog *time.Timer
	idle     time.Duration
	respBuf  *bytes.Buffer
	capBytes int
}

// run streams the upstream body to the client chunk by chunk and returns the
// terminal copy error (nil on a clean EOF). Extracted from proxyNative —
// behavior-identical: same read/write order, same error precedence (a write
// error terminates before the read error is inspected).
func (c *nativeCopier) run(body io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if writeErr := c.writeChunk(buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				return readErr
			}
			return nil
		}
	}
}

// writeChunk forwards one upstream chunk: re-arm the idle watchdog + write
// deadline (streams only), write, flush (so SSE frames reach the client
// live), and tee into the bounded respBuf.
func (c *nativeCopier) writeChunk(chunk []byte) error {
	if c.watchdog != nil {
		c.watchdog.Reset(c.idle)
		_ = c.rc.SetWriteDeadline(time.Now().Add(c.idle))
	}
	if _, err := c.w.Write(chunk); err != nil {
		return err
	}
	if c.flusher != nil {
		c.flusher.Flush()
	}
	if c.respBuf.Len() <= c.capBytes {
		c.respBuf.Write(chunk)
	}
	return nil
}

// nativeTerminalStatus classifies a finished native-passthrough exchange into
// the (status, errorCode) pair recordUsage stores, with the same precedence
// the translate path uses: idle timeout > client disconnect > copy error >
// non-2xx upstream > success. Extracted verbatim from proxyNative —
// behavior-identical, including the log lines and their levels.
func (s *Server) nativeTerminalStatus(r *http.Request, upstreamStatus int, pfReq inference.Request, serverName string, idledOut bool, copyErr error, start time.Time) (string, string) {
	// The idle-timeout log line reports the configured watchdog interval, which
	// is always the server-wide streamIdleTimeout (proxyNative arms the watchdog
	// from exactly this value).
	idle := s.streamIdleTimeout
	status := "success"
	errorCode := ""
	upstreamOK := upstreamStatus >= 200 && upstreamStatus < 300
	clientGone := r.Context().Err() != nil && !idledOut
	switch {
	case idledOut:
		status = "error"
		errorCode = "provider.stream_idle_timeout"
		slog.Error("inference native passthrough idle timeout", "path", r.URL.Path, "api_flavor", pfReq.APIFlavor, "model", pfReq.Model, "server", serverName, "idle", idle.String())
	case clientGone:
		status = "error"
		errorCode = "provider.client_disconnected"
		slog.Debug("inference native passthrough client disconnected", "path", r.URL.Path, "api_flavor", pfReq.APIFlavor, "model", pfReq.Model, "server", serverName)
	case copyErr != nil:
		status = "error"
		errorCode = "provider.stream_copy_error"
		slog.Error("inference native passthrough copy error", "path", r.URL.Path, "api_flavor", pfReq.APIFlavor, "model", pfReq.Model, "server", serverName, "err", copyErr)
	case !upstreamOK:
		// The upstream error body was forwarded verbatim; just record the status.
		status = "error"
		errorCode = fmt.Sprintf("upstream.%d", upstreamStatus)
		slog.Warn("inference native passthrough upstream error", "path", r.URL.Path, "api_flavor", pfReq.APIFlavor, "model", pfReq.Model, "server", serverName, "status", upstreamStatus)
	default:
		slog.Debug("inference native passthrough ok", "path", r.URL.Path, "api_flavor", pfReq.APIFlavor, "model", pfReq.Model, "server", serverName, "status", upstreamStatus, "duration_ms", time.Since(start).Milliseconds())
	}
	return status, errorCode
}

// rewriteModelField returns raw with its top-level "model" field set to
// providerModel. When a rewrite is needed the body is re-serialized, so it is
// VALUE-lossless (semantically identical JSON: every other field is preserved,
// numbers keep full precision via json.Number) but not byte-identical (keys may be
// reordered and <>& HTML-escaped) — harmless to any JSON parser. If providerModel
// is empty, the body is not a JSON object, or the model already matches, the
// ORIGINAL bytes are returned unchanged (a true no-op).
func rewriteModelField(raw []byte, providerModel string) []byte {
	if providerModel == "" {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return raw
	}
	if cur, _ := obj["model"].(string); cur == providerModel {
		return raw
	}
	obj["model"] = providerModel
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

// parsePassthroughUsage best-effort extracts token counts from a proxied upstream
// response (stream or buffered) so the Activity view still shows tokens. It never
// fails: absent fields yield zero. Responses uses input_tokens/output_tokens on
// response.usage (or top-level for the buffered body); Anthropic uses
// message.usage (message_start) + top-level usage (message_delta).
func parsePassthroughUsage(apiFlavor string, body []byte) inference.Usage {
	var u inference.Usage
	take := func(dst *int, v int) {
		if v > *dst {
			*dst = v
		}
	}
	for _, payload := range jsonPayloads(body) {
		switch apiFlavor {
		case "openai_responses":
			var m struct {
				Usage    *responsesUsage `json:"usage"`
				Response *struct {
					Usage *responsesUsage `json:"usage"`
				} `json:"response"`
			}
			if json.Unmarshal(payload, &m) != nil {
				continue
			}
			for _, uu := range []*responsesUsage{m.Usage, nested(m.Response)} {
				if uu == nil {
					continue
				}
				take(&u.InputTokens, uu.InputTokens)
				take(&u.OutputTokens, uu.OutputTokens)
				take(&u.TotalTokens, uu.TotalTokens)
				// Responses input_tokens ALREADY includes the cached subset (OpenAI
				// semantics), so only the cached count is lifted out — InputTokens is
				// left untouched (matches the translate/chat path).
				take(&u.CachedTokens, uu.InputTokensDetails.CachedTokens)
			}
		case "anthropic_messages":
			var m struct {
				Usage   *anthropicUsage `json:"usage"`
				Message *struct {
					Usage *anthropicUsage `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(payload, &m) != nil {
				continue
			}
			for _, uu := range []*anthropicUsage{m.Usage, nestedAnthropic(m.Message)} {
				if uu == nil {
					continue
				}
				// Anthropic reports input_tokens EXCLUDING the prompt-cache tokens
				// (cache reads + creations are separate buckets). The canonical
				// inference.Usage uses OpenAI semantics where InputTokens INCLUDES the
				// cached subset (see compat.AnthropicInputTokens, the inverse), so the
				// cache buckets are folded back in — keeping the value consistent with
				// the translate path for the same prompt.
				take(&u.InputTokens, uu.InputTokens+uu.CacheReadInputTokens+uu.CacheCreationInputTokens)
				take(&u.OutputTokens, uu.OutputTokens)
				take(&u.CachedTokens, uu.CacheReadInputTokens)
				take(&u.CacheWriteTokens, uu.CacheCreationInputTokens)
			}
		}
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	return u
}

type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		// CachedTokens is the prompt-cache-served subset of input_tokens (OpenAI
		// Responses reports it under input_tokens_details; input_tokens already
		// includes it).
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// CacheReadInputTokens / CacheCreationInputTokens are Anthropic's prompt-cache
	// buckets, reported SEPARATELY from (and not included in) input_tokens.
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func nested(r *struct {
	Usage *responsesUsage `json:"usage"`
},
) *responsesUsage {
	if r == nil {
		return nil
	}
	return r.Usage
}

func nestedAnthropic(m *struct {
	Usage *anthropicUsage `json:"usage"`
},
) *anthropicUsage {
	if m == nil {
		return nil
	}
	return m.Usage
}

// jsonPayloads returns the JSON objects to inspect for usage: every SSE `data:`
// line's payload when the body is an event stream, otherwise the whole body as a
// single payload. SSE detection is per-line (a line that *starts* with `data:`) so
// a buffered JSON body that merely contains the substring "data:" inside a string
// value (a data URI, "metadata:", …) is correctly treated as one JSON object, not
// mistaken for an event stream. (A JSON "data" key serializes as `"data":`, which
// never matches the `data:` line prefix.)
func jsonPayloads(body []byte) [][]byte {
	var payloads [][]byte
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			continue
		}
		payloads = append(payloads, []byte(payload))
	}
	if len(payloads) == 0 {
		return [][]byte{body}
	}
	return payloads
}
