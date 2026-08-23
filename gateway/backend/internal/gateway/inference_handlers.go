// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/compat"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strconv"
	"strings"
	"time"
)

func (s *Server) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireWebAnyScope(w, r, scopeGatewayUse, scopeLLMInvoke)
	if !ok {
		return
	}
	liftInferenceDeadlines(w)
	raw, ok := readRawJSONUnlimited(w, r)
	if !ok {
		return
	}
	req, err := compat.ParseOpenAIChatCompletions(raw)
	if err != nil {
		slog.Warn("inference request rejected: invalid body", "path", r.URL.Path, "api_flavor", "openai_chat", "err", err)
		writeRequestError(w, err)
		return
	}
	if token.ID == "" { // session principal (not a bearer token): honor optional run-as
		if runAsID := strings.TrimSpace(r.Header.Get(runAsHeaderName)); runAsID != "" {
			runAs, rErr := s.Portal.AuthorizeRunAsToken(r.Context(), token, runAsID)
			if rErr != nil {
				writePortalTokenError(w, rErr)
				return
			}
			token = runAs
		}
	}
	// Chat has no native-passthrough path, so the preflight gate always runs
	// against the fully-parsed request (no probe needed) -- see
	// inferencePreflight's doc comment for the shared gate order.
	pf, handled := s.inferencePreflight(w, r, token, raw, inferenceShape{apiFlavor: "openai_chat_completions", endpoint: endpointChat, model: req.Model, stream: req.Stream})
	if handled {
		return
	}
	req = pf.mergeInto(req)
	s.logInferenceRequest(r, token, req)
	if req.Stream {
		s.completeStream(w, r, token, req, raw)
		return
	}
	s.complete(w, r, token, req, raw, func(resp provider.Response) any {
		return compat.OpenAIChatResponse("chatcmpl_mock", req.Model, resp.Text, resp.ToolCalls, resp.Usage)
	})
}

// logInferenceRequest emits a Debug line for every inference request as it enters
// the completion path — the headline hook for diagnosing client↔gateway problems
// (e.g. a Codex client hitting /v1/responses) in the portal Logs view. It never
// logs the bearer token secret, only the principal's ids.
func (s *Server) logInferenceRequest(r *http.Request, token auth.Token, req inference.Request) {
	slog.Debug("inference request",
		"path", r.URL.Path,
		"api_flavor", req.APIFlavor,
		"model", req.Model,
		"stream", req.Stream,
		"messages", len(req.Messages),
		"token_id", token.ID,
		"user_id", token.UserID,
		"remote_addr", r.RemoteAddr)
}

func (s *Server) handleOpenAIResponses(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireAnyScope(w, r, scopeGatewayUse, scopeLLMInvoke)
	if !ok {
		return
	}
	liftInferenceDeadlines(w)
	raw, ok := readRawJSONUnlimited(w, r)
	if !ok {
		return
	}
	// The gate (override resolution, server-override re-auth, model allowlist,
	// admission, session extraction) must run EXACTLY ONCE for this request
	// even though native passthrough and the translate fallback below are two
	// distinct code paths for the SAME client request. So the gate runs here,
	// via inferencePreflight, against a minimal tolerant probe of the raw body
	// (sniffRoutingModel) -- BEFORE the lossy compat parse, which would reject
	// a rich Codex body -- and its result (pf) is threaded into BOTH
	// tryProxyNative and the translate path below; neither re-runs the gate.
	//
	// A probe that can't read a model (malformed body, or a field type that
	// doesn't survive even the tolerant probe) never reaches the gate here --
	// mirroring the invariant that a parse failure must never consume
	// admission budget -- and native passthrough is skipped entirely; the real
	// (flavor-specific) parse below produces the proper error, or, on the rare
	// body that yields no probe model yet still parses (e.g. a whitespace-only
	// "model" string, which satisfies Validate()'s non-empty check), the gate
	// runs there instead, against the parsed model, exactly once either way.
	model, stream := sniffRoutingModel(raw)
	var pf preflight
	var handled bool
	if model != "" {
		pf, handled = s.inferencePreflight(w, r, token, raw, inferenceShape{apiFlavor: "openai_responses", endpoint: endpointResponses, model: model, stream: stream})
		if handled {
			return
		}
		// Native passthrough: if the resolved application supports Codex natively,
		// proxy the raw body to the upstream /v1/responses instead of translating.
		if s.tryProxyNative(w, r, token, raw, "openai_responses", pf) {
			return
		}
	}
	req, err := compat.ParseOpenAIResponses(raw)
	if err != nil {
		// This request was NOT handled by native passthrough (see tryProxyNative
		// above) and the translate path can't represent a rich Codex body. The
		// usual cause is that the application serving this model doesn't have
		// "native passthrough (Codex)" enabled, or it wasn't reachable.
		slog.Warn("inference request rejected: invalid body", "path", r.URL.Path, "api_flavor", "openai_responses", "err", err,
			"hint", "for a Codex client, enable native passthrough (Codex) on the application serving this model; set log level to debug to see why passthrough did not apply")
		writeRequestError(w, err)
		return
	}
	if model == "" {
		// The probe found no usable model but the strict parse succeeded anyway
		// (see the comment above); run the gate now, against the parsed model,
		// since it never ran above.
		pf, handled = s.inferencePreflight(w, r, token, raw, inferenceShape{apiFlavor: "openai_responses", endpoint: endpointResponses, model: req.Model, stream: req.Stream})
		if handled {
			return
		}
	}
	req = pf.mergeInto(req)
	s.logInferenceRequest(r, token, req)
	if req.Stream {
		s.completeStreamResponses(w, r, token, req, raw)
		return
	}
	s.complete(w, r, token, req, raw, func(resp provider.Response) any {
		return compat.OpenAIResponsesResponse("resp_mock", req.Model, resp.Text, resp.Reasoning, resp.ToolCalls, resp.Usage)
	})
}

func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireAnyScope(w, r, scopeGatewayUse, scopeLLMInvoke)
	if !ok {
		return
	}
	liftInferenceDeadlines(w)
	raw, ok := readRawJSONUnlimited(w, r)
	if !ok {
		return
	}
	// See the identical comment in handleOpenAIResponses -- same dedup-needed
	// reasoning (and the same single-gate structure) applies to /v1/messages.
	model, stream := sniffRoutingModel(raw)
	var pf preflight
	var handled bool
	if model != "" {
		pf, handled = s.inferencePreflight(w, r, token, raw, inferenceShape{apiFlavor: "anthropic_messages", endpoint: endpointMessages, model: model, stream: stream})
		if handled {
			return
		}
		// Native passthrough: if the resolved application supports Claude Code natively,
		// proxy the raw body to the upstream /v1/messages instead of translating (this
		// is also the only way Anthropic streaming works, since the translate path
		// rejects it).
		if s.tryProxyNative(w, r, token, raw, "anthropic_messages", pf) {
			return
		}
	}
	req, err := compat.ParseAnthropicMessages(raw)
	if err != nil {
		slog.Warn("inference request rejected: invalid body", "path", r.URL.Path, "api_flavor", "anthropic_messages", "err", err,
			"hint", "for a Claude Code client, enable native passthrough (Claude Code) on the application serving this model; set log level to debug to see why passthrough did not apply")
		writeRequestError(w, err)
		return
	}
	if model == "" {
		pf, handled = s.inferencePreflight(w, r, token, raw, inferenceShape{apiFlavor: "anthropic_messages", endpoint: endpointMessages, model: req.Model, stream: req.Stream})
		if handled {
			return
		}
	}
	req = pf.mergeInto(req)
	s.logInferenceRequest(r, token, req)
	if req.Stream {
		s.completeStreamAnthropic(w, r, token, req, raw)
		return
	}
	s.complete(w, r, token, req, raw, func(resp provider.Response) any {
		return compat.AnthropicMessageResponse("msg_mock", req.Model, resp.Text, resp.Reasoning, resp.ToolCalls, resp.FinishReason, resp.Usage)
	})
}

// resolveModelOverride returns the gateway model to use for a REQUESTED model given
// the token's override configuration: an exact per-model map entry wins; otherwise
// the catch-all (token.ModelOverride) applies; otherwise the requested model is
// returned unchanged. It is the single source of truth for the one shared gate
// (inferencePreflight) both the translate handlers and native passthrough
// (tryProxyNative, via the preflight it consumes) resolve their effective model
// through.
func resolveModelOverride(token auth.Token, requested string) string {
	if mapped, ok := token.ModelOverrideMap[requested]; ok && strings.TrimSpace(mapped) != "" {
		return mapped
	}
	if token.ModelOverride != "" {
		return token.ModelOverride
	}
	return requested
}

// errServerOverrideForbidden is returned by applyServerOverride when the caller
// (or the caller's token) requested a server override for a server it may not
// manage RIGHT NOW — see AuthorizeServerManage. Callers must map it to a 403
// (writeServerOverrideForbidden) and MUST NOT fall through to any other
// resolution path for the request.
var errServerOverrideForbidden = errors.New("server_override.forbidden")

// applyServerOverride is the runtime security boundary for the server_override
// feature (design: an operator/token can pin every request to one specific
// AI-server, bypassing resource-group provisioning/affinity/maintenance-status —
// see inference.Request.ServerOverrideID and routing.Resolver.resolveServerOverride).
//
// It determines the requested override — TOKEN-FIRST precedence: the effective
// token's own configured ServerOverride GOVERNS whenever it is set, and the
// X-OP-Server-Override chat header is consulted only as a fallback when the
// token carries no override of its own. A run-as token is a deliberate server
// pin; when a chat runs AS that token, the token's override governs and the
// chat's own override is ignored (the frontend locks the chat's server-override
// control to match the run-as token's override, so the two can never disagree
// in the UI — but this function is the actual boundary regardless of what the
// UI shows). id and force are resolved TOGETHER from whichever source wins, so
// force always follows the SAME source as id — a token-sourced id can never
// pick up the chat header's force flag (or vice versa).
//
// It then RE-AUTHORIZES the caller against that specific server via
// s.Portal.AuthorizeServerManage BEFORE ever stamping req.ServerOverrideID /
// req.ServerOverrideForceUnreachable. This re-check is required even though a
// token's ServerOverride is already self-healed at write time (see
// portal.Service.validateServerOverride): the token can outlive the
// server-management grant that set it (its owner may lose can_manage_servers on
// that server, or the server may be reassigned, at any later point), so every
// routed request re-verifies against CURRENT authorization rather than trusting
// the stored field.
//
// The two headers are set ONLY by the gateway's own background chat-run executor
// calling itself over the internal trusted-loopback path (see authenticateWeb) —
// an external client can never inject them because nginx blanks both at the
// public edge (deploy/nginx/*.conf, deploy/k8s/nginx-configmap.yaml), mirroring
// X-OP-Internal-Auth/-User. Even so, AuthorizeServerManage — not the header's
// provenance — is the actual boundary: a spoofed or stale value is rejected
// here, never silently honored.
//
// A blank result (no token override, no header) is a strict no-op: req is
// returned UNCHANGED and no AuthorizeServerManage call is made, so an ordinary
// request without any server_override configured pays zero extra cost and is
// byte-identical to before this feature.
func (s *Server) applyServerOverride(ctx context.Context, r *http.Request, token auth.Token, req inference.Request) (inference.Request, error) {
	var id string
	var force bool
	if token.ServerOverride != "" {
		id = token.ServerOverride
		force = token.ServerOverrideForceUnreachable
	} else {
		id = strings.TrimSpace(r.Header.Get(serverOverrideHeaderName))
		force = r.Header.Get(serverOverrideForceHeaderName) == "1"
	}
	if id == "" {
		return req, nil
	}
	if err := s.Portal.AuthorizeServerManage(ctx, token, id); err != nil {
		return req, errServerOverrideForbidden
	}
	req.ServerOverrideID = id
	req.ServerOverrideForceUnreachable = force
	return req, nil
}

// writeServerOverrideForbidden writes the 403 response for a server_override
// request that AuthorizeServerManage rejected (see applyServerOverride). The
// caller must return immediately after calling this — never fall through to any
// other resolution path for the request.
func writeServerOverrideForbidden(w http.ResponseWriter) {
	writeJSON(w, http.StatusForbidden, apierror.Response("server_override.forbidden", "server override forbidden for this token", ""))
}

// modelAllowed is the service-account model-allowlist admission gate (service
// accounts, Phase 1 §5.4). It must be called with the EFFECTIVE gateway model —
// i.e. AFTER resolveModelOverride has been applied — so an override can never be
// used to reach a model outside the allowlist by requesting a different name
// that maps to it. A user token (IsService()==false, and AllowedModels is
// always empty for one) is never affected: this is a no-op for every request
// except a service token with a non-empty allowlist. An empty allowlist on a
// service token is ALSO a no-op (every model allowed) — the allowlist is
// opt-in, not deny-by-default. Called from the one shared gate
// (inferencePreflight) at the point where the effective model is known, and
// always BEFORE Resolve/any upstream call — for both the translate handlers
// and native passthrough (tryProxyNative), which consumes the same preflight
// result rather than calling this again.
func modelAllowed(token auth.Token, effectiveModel string) bool {
	if !token.IsService() || len(token.AllowedModels) == 0 {
		return true
	}
	for _, allowed := range token.AllowedModels {
		if allowed == effectiveModel {
			return true
		}
	}
	return false
}

// writeModelNotAllowed writes the 403 model.not_allowed response for a service
// token whose allowlist excludes the effective model (see modelAllowed). Written
// BEFORE any Resolve/upstream call, so a disallowed model never reaches an
// AI-server — mirroring the other pre-resolve rejections in this file (e.g. an
// invalid body via writeRequestError), it is not recorded as a usage event since
// no upstream was contacted.
func writeModelNotAllowed(w http.ResponseWriter) {
	writeJSON(w, http.StatusForbidden, apierror.Response("model.not_allowed", "model not allowed for this token", ""))
}

// principalFor resolves the Principal a request's rate/quota/budget limits
// (PrincipalLimiter, design spec Phase 2 §3) are checked against, from the
// EFFECTIVE auth token (post model-override / run-as resolution -- the same
// token modelAllowed is called with):
//
//   - token.IsService() (a service token, Phase 1) -> ONLY the service
//     principal, keyed on ServiceID.
//   - else token.UserID != "" (an ordinary user-owned API token, OR the
//     session/chat-loopback pseudo-principal that authenticateWeb/the internal
//     trusted-loopback branch populate -- both set UserID and leave ID empty)
//     -> ONLY the user principal, keyed on UserID.
//   - else (neither -- should not occur for any token that passed auth) ->
//     ok=false: the caller skips principal-limit enforcement entirely for this
//     request rather than resolving to some degenerate shared bucket.
//
// "Kein Stacking" (design spec §3: a service-token request is checked ONLY
// against service limits, a user/session request ONLY against user limits,
// never both) falls out of the ordering: IsService() is checked FIRST and
// returns immediately, so even a token that (hypothetically; real minting
// never does this) carried both a ServiceID and a UserID would resolve to the
// service principal only. Admins are NOT exempt (design spec §3): an admin's
// own user token resolves to their own user principal exactly like any other
// user's.
func principalFor(token auth.Token) (Principal, bool) {
	if token.IsService() {
		return Principal{Type: routing.PrincipalTypeService, ID: token.ServiceID}, true
	}
	if token.UserID != "" {
		return Principal{Type: routing.PrincipalTypeUser, ID: token.UserID}, true
	}
	return Principal{}, false
}

// admitPrincipal is the PrincipalLimiter admission gate (design spec §6.1/
// §6.3), called at the SAME pre-Resolve choke point as modelAllowed: after
// model-override resolution, before any Resolve/upstream call. It resolves the
// request's Principal (principalFor) and, only when one applies, consults
// s.Limiter.Admit; on a denial it writes the mapped HTTP response
// (writeLimitDenied) and reports handled=true so the caller returns
// immediately -- mirroring writeModelNotAllowed, a denied request never
// reaches Resolve/an AI-server and is never recorded as a usage event.
//
// s.Limiter.Admit is itself nil-safe (a nil *PrincipalLimiter, a nil-backed
// store, or an unconfigured principal all simply allow), so calling this
// unconditionally at every inference entry point is the no-op invariant
// (design spec §10): with nothing configured for anyone, behavior is
// byte-identical to before this feature existed.
//
// admitPrincipal has exactly ONE call site in the whole package --
// inferencePreflight -- which itself runs exactly once per client request
// (see its doc comment). Earlier this gate had four call sites (the three
// translate handlers below plus tryProxyNative) and native passthrough could
// fall through to the translate handler for the SAME request, so a mutable
// per-request "admission marker" seeded into the request context was needed
// to stop the second call site from double-consuming a principal's rate
// budget. Now that every caller goes through inferencePreflight, that marker
// is unnecessary and has been removed: there is structurally only one call
// per request, not a dedup of several.
func (s *Server) admitPrincipal(w http.ResponseWriter, r *http.Request, token auth.Token) (handled bool) {
	p, ok := principalFor(token)
	if !ok {
		return false
	}
	allow, reason, retryAfter := s.Limiter.Admit(r.Context(), p)
	if allow {
		return false
	}
	writeLimitDenied(w, reason, retryAfter)
	return true
}

// preflight is the outcome of a successful inferencePreflight call: the
// effective model (post model-override), the re-authorized server-override
// verdict, and the extracted client-session info for one inference request --
// everything the shared gate resolves, packaged as an inference.Request with
// no Messages/Tools (those are flavor-specific and filled in separately by
// whichever parse produced the caller's own req). A translate handler copies
// Req's fields onto its own fully-parsed request (see mergeInto); native
// passthrough (tryProxyNative) uses Req directly for Resolve.
type preflight struct {
	Req inference.Request
}

// mergeInto copies the preflight's resolved model, server-override verdict,
// and session fields onto a fully-parsed inference.Request, leaving every
// other field (Messages, Tools, ToolChoice, …) exactly as the flavor-specific
// compat.Parse* call produced it. Every translate handler below calls this
// exactly once, instead of re-deriving any of these fields itself.
func (pf preflight) mergeInto(req inference.Request) inference.Request {
	req.Model = pf.Req.Model
	req.ServerOverrideID = pf.Req.ServerOverrideID
	req.ServerOverrideForceUnreachable = pf.Req.ServerOverrideForceUnreachable
	req.SessionID = pf.Req.SessionID
	req.ClientSessionID = pf.Req.ClientSessionID
	req.SessionSource = pf.Req.SessionSource
	req.AgentID = pf.Req.AgentID
	return req
}

// inferenceShape is the caller-supplied description of one inference request
// that inferencePreflight needs before it can run the shared gate: which wire
// flavor it is, which session-extraction rules apply (endpoint), and the
// caller-supplied model/stream -- either the fully-parsed request's fields
// (chat) or the pre-parse routing probe's (responses/messages; see
// sniffRoutingModel). Bundled purely to keep inferencePreflight's own
// parameter list short; it carries no behavior of its own.
type inferenceShape struct {
	apiFlavor string
	endpoint  sessionEndpoint
	model     string
	stream    bool
}

// inferencePreflight is the ONE shared pre-dispatch gate for every inference
// request, replacing four near-duplicate copies (handleOpenAIChat,
// handleOpenAIResponses, handleAnthropicMessages, and tryProxyNative each ran
// their own copy of this exact sequence, in two different orders). It runs,
// in order:
//
//  1. model-override resolution (resolveModelOverride) against the caller-
//     supplied model -- for a translate-only handler (chat) this is the
//     fully-parsed request's model; for an endpoint with a native-passthrough
//     path (responses, messages) this is the routing model sniffed from the
//     raw body BEFORE the (lossy) flavor-specific parse, so a rich Codex/
//     Claude Code body that the compat parser would reject still reaches the
//     gate. The caller is responsible for never invoking this with an empty
//     model (an unparseable/malformed body, or one with no usable "model"
//     field at all) -- that case must bail out before ANY gate runs (see the
//     callers below), so a parse failure never consumes admission budget.
//  2. applyServerOverride (re-authorizes a token/header server pin) --
//     TERMINAL on failure (403).
//  3. modelAllowed (service-account model allowlist) -- TERMINAL on failure
//     (403), checked against the EFFECTIVE (post-override) model.
//  4. admitPrincipal (rate/quota/budget) -- TERMINAL on failure (429/402).
//  5. extractClientSession -- pure, never fails.
//
// Called EXACTLY ONCE per client request: the three translate handlers below
// call it before their own parse (chat) or before attempting native
// passthrough (responses, messages), and tryProxyNative -- which used to run
// its own copy of steps 1-5, in a different order, on every native attempt --
// now takes the preflight this produced and never re-runs any of it. This is
// what makes admitPrincipal single-execution without the mutable per-request
// "admission marker" the two native-capable handlers used to seed: there is
// simply no second call site left to dedup.
func (s *Server) inferencePreflight(w http.ResponseWriter, r *http.Request, token auth.Token, raw []byte, shape inferenceShape) (preflight, bool) {
	req := inference.Request{Model: resolveModelOverride(token, shape.model), APIFlavor: shape.apiFlavor, Stream: shape.stream}
	req, sErr := s.applyServerOverride(r.Context(), r, token, req)
	if sErr != nil {
		writeServerOverrideForbidden(w)
		return preflight{}, true
	}
	if !modelAllowed(token, req.Model) {
		writeModelNotAllowed(w)
		return preflight{}, true
	}
	if s.admitPrincipal(w, r, token) {
		return preflight{}, true
	}
	si := extractClientSession(r.Header, raw, shape.endpoint)
	req.SessionID = si.ExplicitHeader
	req.ClientSessionID = si.ClientSession
	req.SessionSource = si.Source
	req.AgentID = si.AgentID
	return preflight{Req: req}, false
}

// writeLimitDenied writes the HTTP response for a PrincipalLimiter.Admit
// denial, mapping each limit reason to its stable error code and status per
// design spec §8: rate-limit and both quotas -> 429, cost budget -> 402. Only
// the rate-limit reason carries a Retry-After header (whole seconds, floored
// at 1 so a sub-second computed wait never reads as "retry immediately") --
// quota/budget denials give no concrete retry time (the period reset is
// calendar-aligned, not a short countdown). Written BEFORE any Resolve/
// upstream call, mirroring writeModelNotAllowed, so a denied request never
// reaches an AI-server and leaks no principal-internal detail (a static
// message per reason, never a store error).
func writeLimitDenied(w http.ResponseWriter, reason string, retryAfter time.Duration) {
	status := http.StatusTooManyRequests
	code := "limit.rate_limited"
	message := "rate limit exceeded"
	switch reason {
	case "request_quota":
		code, message = "limit.request_quota_exceeded", "request quota exceeded"
	case "token_quota":
		code, message = "limit.token_quota_exceeded", "token quota exceeded"
	case "cost_budget":
		status, code, message = http.StatusPaymentRequired, "limit.cost_budget_exceeded", "cost budget exceeded"
	}
	if reason == "rate" {
		seconds := int(math.Ceil(retryAfter.Seconds()))
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
	}
	writeJSON(w, status, apierror.Response(code, message, ""))
}

func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	// A service token (scope llm:invoke only) is a utility/read call here too —
	// no upstream inference, no billing, no allowlist bypass — so it is treated
	// the same as gateway:use (service-accounts Phase 1 §13).
	if _, ok := s.requireAnyScope(w, r, scopeGatewayUse, scopeLLMInvoke); !ok {
		return
	}
	liftInferenceDeadlines(w)
	raw, ok := readRawJSONUnlimited(w, r)
	if !ok {
		return
	}
	count, err := compat.CountAnthropicTokens(raw)
	if err != nil {
		writeRequestError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, count)
}
