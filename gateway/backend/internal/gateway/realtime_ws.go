// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// The API surface of GET /v1/realtime's own refusals.
const (
	// realtimeModelRequired: the handshake names no model (the query's model is
	// absent or blank). 400.
	realtimeModelRequired = "realtime.model_required"
	// realtimeUpgradeRequired: the request is not a WebSocket handshake the gateway
	// can accept: 426 without the upgrade itself, 400 for a malformed one (a version
	// other than 13, a missing or malformed key).
	realtimeUpgradeRequired = "realtime.upgrade_required"
)

// codeRealtimeAcceptFailed is the usage row's error code of a session whose upstream
// was dialed but whose client handshake could not be completed (the response writer
// cannot be hijacked). It is never sent to a client: websocket.Accept answers it.
const codeRealtimeAcceptFailed = "realtime.accept_failed"

// realtimeCloseCodePrefix prefixes the usage row's error code of a session that
// ended with a close status other than 1000, 1001 or 1005: "realtime.close_1011".
const realtimeCloseCodePrefix = "realtime.close_"

// realtimeSubprotocol is the subprotocol OpenAI's Realtime clients offer, and the only
// one the gateway ever selects.
const realtimeSubprotocol = "realtime"

// realtimeAPIKeyProtocolPrefix is the OpenAI browser convention for carrying an API
// key in the WebSocket handshake, which a browser cannot give an Authorization
// header: the client offers "openai-insecure-api-key.<key>" as a subprotocol.
const realtimeAPIKeyProtocolPrefix = "openai-insecure-api-key."

// realtimeMessageLimit bounds one message read from either side of a session, the
// agent router's own bound. A message one byte over it ends the session with 1009 on
// both sides; nothing larger is ever buffered.
const realtimeMessageLimit = 4 << 20

// realtimeIdleTimeout is how long a session may go without a message in either
// direction before the gateway ends it (pings do not count). It equals the agent
// router's own bound, which normally ends a silent session first; this one exists so
// that a session whose agent connection is lost without a FIN cannot hold its active
// request -- and so its instance's capacity -- for good.
const realtimeIdleTimeout = 15 * time.Minute

// realtimeCauseGrace is how long a session that ended on a failed write waits for the
// read that names the cause (settleRealtimeEnd).
const realtimeCauseGrace = time.Second

// realtimeMaxTargetAttempts bounds the resolves one realtime handshake makes while it
// looks for a target that can serve it (selectRealtimeTarget).
const realtimeMaxTargetAttempts = 8

// realtimeInputBytesPerSecond is the byte rate of the audio a client appends:
// PCM16 mono at 16 kHz, the input format sglang-omni's realtime sessions document
// and assert. It turns appended bytes into billed audio seconds.
const realtimeInputBytesPerSecond = 32000

// realtimeUsagePath is the provider path a realtime usage row records: the endpoint
// the agent relays the session to, as every other row names the endpoint served
// (the agent route the gateway dials is the active request's provider path).
const realtimeUsagePath = "/v1/realtime"

// The close reasons the gateway itself sends. Reasons a peer sent are relayed as
// they are.
const (
	// realtimeReasonIdle is sglang-omni's (and the agent router's) own idle reason,
	// so a client sees one shape whichever of them timed the session out.
	realtimeReasonIdle     = "idle_timeout"
	realtimeReasonShutdown = "gateway shutting down"
	realtimeReasonTooBig   = "message too big"
)

// The two client events billing reads. Only frames that contain the type at all are
// decoded.
var (
	realtimeAppendEvent = []byte("input_audio_buffer.append")
	realtimeDoneEvent   = []byte("response.done")
)

// realtimeInferenceShape is the shape handleRealtime hands the one preflight gate.
// The flavor is the coarse openai_audio family realtime is served in, the endpoint
// labels the session (endpointRealtime), and requiredCapabilities is
// [routing.CapabilityRealtime]: the resolver refuses a model without a literal
// realtime:yes verdict, and the capability list keeps the request off the
// vendor-account path, as speech's and transcription's do.
func realtimeInferenceShape(model string) inferenceShape {
	return inferenceShape{
		apiFlavor:            routing.APIFlavorOpenAIAudio,
		endpoint:             endpointRealtime,
		model:                model,
		stream:               true,
		requiredCapabilities: []string{routing.CapabilityRealtime},
	}
}

// handleRealtime serves GET /v1/realtime (and its /openai alias): an OpenAI
// Realtime-style WebSocket relayed to sglang-omni's /v1/realtime through the agent
// router's GET /realtime/{model} route. The gateway translates nothing: messages
// cross byte for byte with their type, and are read only to count what the session
// is billed for.
//
// Before the upgrade, in order -- every refusal is a plain HTTP answer and nothing
// is spent on a request refused before step 5:
//
//  1. GET only (405).
//  2. Auth: a bearer key with gateway:use or llm:invoke (or the internal loopback
//     pair), exactly as every /v1 audio endpoint takes it -- never the browser
//     session cookie. A client that cannot set a header (a browser) may instead
//     offer the subprotocol "openai-insecure-api-key.<key>" (OpenAI's convention);
//     that key then authenticates exactly as a bearer would (realtimeAuthRequest).
//     401/403 before any upgrade.
//  3. The request must be a WebSocket handshake websocket.Accept will take
//     (realtimeHandshakeRefusal): 426 or 400 realtime.upgrade_required otherwise, so
//     a stray GET never spends admission or starts a model.
//  4. model is the query's model, required: 400 realtime.model_required.
//  5. The one admission of the session: inferencePreflight over
//     realtimeInferenceShape (model override, server override, allowlist,
//     rate/quota/budget) -- once, however long the session lasts.
//  6. The target: only an agent that declared realtime_proxy can serve a session
//     (realtimeTargetServed); selectRealtimeTarget resolves until it finds one, or
//     answers the routing refusal a model with no candidate gets.
//
// The session (serveRealtimeSession) is then registered in Active Requests, the
// agent is dialed, and only once that dial has succeeded is the client's socket
// accepted; a failed dial is answered 502 (or 503) before any upgrade.
//
// Origin: Accept runs with InsecureSkipVerify, so a page on any origin may open a
// session. That is deliberate and safe here. Cross-site WebSocket hijacking needs an
// ambient credential the browser attaches by itself, and this endpoint accepts none:
// not the session cookie (step 2's legs exclude it), and the internal loopback pair
// and the bearer are headers a page's WebSocket cannot set. The only credential a
// page can send is a key in the subprotocol, which it must already hold -- the
// OpenAI browser convention this endpoint exists to support, from the API client's
// own origin.
//
// The key in a subprotocol is a secret on the wire: the gateway selects
// "realtime" alone (Accept only ever echoes a value from its own list), never logs
// the offer, and never forwards it -- the agent is dialed with a header of the
// gateway's own (provider.NativeWebSocketDialer).
func (s *Server) handleRealtime(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireInternalOrBearerAnyScope(w, realtimeAuthRequest(r), scopeGatewayUse, scopeLLMInvoke)
	if !ok {
		return
	}
	if refusal := realtimeHandshakeRefusal(r); refusal != nil {
		if refusal.status == http.StatusUpgradeRequired {
			w.Header().Set("Upgrade", "websocket")
		}
		w.Header().Set("Sec-WebSocket-Version", "13")
		writeJSON(w, refusal.status, apierror.Response(realtimeUpgradeRequired, refusal.message, ""))
		return
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		writeJSON(w, http.StatusBadRequest, apierror.Response(realtimeModelRequired, "model is required", ""))
		return
	}
	pf, handled := s.inferencePreflight(w, r, token, nil, realtimeInferenceShape(model))
	if handled {
		return
	}
	// The resolve may wait in the admission queue and the dial waits for the agent to
	// start the model, both far longer than the control-plane deadlines; an expired
	// read deadline would also cancel the request context and with it the dial. A
	// hijacked connection has no deadline at all (net/http clears them).
	liftInferenceDeadlines(w)
	req := pf.Req
	req.AdmissionStart = time.Now()
	target, err := s.selectRealtimeTarget(r.Context(), req, func(ctx context.Context, again inference.Request) (routing.Target, error) {
		return s.resolveTarget(ctx, &token, again)
	})
	if err != nil {
		writeRealtimeRefusal(w, r, req, err)
		return
	}
	s.serveRealtimeSession(w, r, token, req, target)
}

// realtimeAuthRequest is the request handleRealtime authenticates: r itself, unless r
// carries no Authorization header and offers a key as the subprotocol
// "openai-insecure-api-key.<key>" -- then a clone of r whose Authorization is that
// key as a bearer. An Authorization header always wins over a subprotocol key. Only
// the authentication reads the clone; the handshake is accepted from r.
func realtimeAuthRequest(r *http.Request) *http.Request {
	if r.Header.Get("Authorization") != "" {
		return r
	}
	key := realtimeSubprotocolKey(r.Header)
	if key == "" {
		return r
	}
	ar := r.Clone(r.Context())
	ar.Header.Set("Authorization", "Bearer "+key)
	return ar
}

// realtimeSubprotocolKey returns the key of the first offered
// "openai-insecure-api-key.<key>" subprotocol, "" when none is offered. The key keeps
// its case: an API key is case-sensitive.
func realtimeSubprotocolKey(h http.Header) string {
	for _, v := range h.Values("Sec-WebSocket-Protocol") {
		for part := range strings.SplitSeq(v, ",") {
			if key, ok := strings.CutPrefix(strings.TrimSpace(part), realtimeAPIKeyProtocolPrefix); ok && key != "" {
				return key
			}
		}
	}
	return ""
}

// realtimeRefusal is the answer to a request that is not a handshake Accept takes.
type realtimeRefusal struct {
	status  int
	message string
}

// realtimeHandshakeRefusal checks r the way websocket.Accept will (its
// verifyClientRequest), before anything is spent on it: HTTP/1.1 or later with
// Connection: upgrade and Upgrade: websocket (426 otherwise), Sec-WebSocket-Version
// 13 and exactly one Sec-WebSocket-Key that decodes to 16 bytes (400 otherwise). The
// method was checked already. It returns nil for a handshake Accept takes, so the
// only Accept failure left after a dial is a writer that cannot be hijacked.
func realtimeHandshakeRefusal(r *http.Request) *realtimeRefusal {
	if !r.ProtoAtLeast(1, 1) || !realtimeHeaderHasToken(r.Header, "Connection", "upgrade") || !realtimeHeaderHasToken(r.Header, "Upgrade", "websocket") {
		return &realtimeRefusal{status: http.StatusUpgradeRequired, message: "GET /v1/realtime must be a WebSocket handshake"}
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return &realtimeRefusal{status: http.StatusBadRequest, message: "the WebSocket handshake must carry Sec-WebSocket-Version 13"}
	}
	keys := r.Header.Values("Sec-WebSocket-Key")
	if len(keys) != 1 {
		return &realtimeRefusal{status: http.StatusBadRequest, message: "the WebSocket handshake must carry exactly one Sec-WebSocket-Key"}
	}
	if key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keys[0])); err != nil || len(key) != 16 {
		return &realtimeRefusal{status: http.StatusBadRequest, message: "the WebSocket handshake's Sec-WebSocket-Key must be 16 bytes, base64-encoded"}
	}
	return nil
}

// realtimeHeaderHasToken reports whether one of key's comma-separated values is
// token, in any letter case.
func realtimeHeaderHasToken(h http.Header, key, token string) bool {
	for _, v := range h.Values(key) {
		for t := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// selectRealtimeTarget resolves req until it finds a target that can serve a
// realtime session: an agent that declared realtime_proxy (realtimeTargetServed)
// whose effective flavors include openai_audio (targetServesFlavor, the
// effective-served conjunct every audio relay applies). The resolver cannot see
// agent features, so a mapping of the model on an agent without the feature may win
// the selection over one with it. Such a target is excluded by its instance key
// (inference.Request.ExcludeInstanceKeys, the exclusion re-selection uses) and req is
// resolved again, so a model mapped on both reaches the agent that serves it, and an
// agent without the feature is never dialed.
//
// The loop ends:
//   - at a served target;
//   - with routing.ErrNoModelRoute -- the answer a model without any candidate gets,
//     as the speech relay gives it -- after realtimeMaxTargetAttempts resolves, at a
//     target that addresses no instance (nothing to exclude), or at a target already
//     excluded (a pin that ignores the exclusion would only be answered again);
//   - with the resolver's own refusal, kept as it is, except that a "nothing left"
//     refusal (no route, no healthy host) after an exclusion is the no-candidate
//     refusal too: what is left is no target that serves.
//
// resolve is the resolve (resolveTarget in production; a test seam).
func (s *Server) selectRealtimeTarget(ctx context.Context, req inference.Request, resolve func(context.Context, inference.Request) (routing.Target, error)) (routing.Target, error) {
	for attempt := 1; ; attempt++ {
		target, err := resolve(ctx, req)
		if err != nil {
			if len(req.ExcludeInstanceKeys) > 0 && (errors.Is(err, routing.ErrNoHealthyHost) || errors.Is(err, routing.ErrNoModelRoute)) {
				return routing.Target{}, routing.ErrNoModelRoute
			}
			return routing.Target{}, err
		}
		if s.realtimeTargetServed(target) && targetServesFlavor(target, req.APIFlavor) {
			return target, nil
		}
		slog.Debug("realtime: the resolved target cannot serve a realtime session; excluding it",
			"model", req.Model, "server", target.ServerID, "instance", target.InstanceKey, "provider", target.Provider, "attempt", attempt)
		if target.InstanceKey == "" || slices.Contains(req.ExcludeInstanceKeys, target.InstanceKey) || attempt >= realtimeMaxTargetAttempts {
			return routing.Target{}, routing.ErrNoModelRoute
		}
		req = withLostInstance(req, target)
	}
}

// writeRealtimeRefusal answers a handshake whose target selection was refused, as
// the speech relay answers its routing refusal. Like the bearer live session's gate
// it records no usage row: nothing was relayed.
func writeRealtimeRefusal(w http.ResponseWriter, r *http.Request, req inference.Request, err error) {
	if errors.Is(err, routing.ErrAdmissionQueueTimeout) || errors.Is(err, routing.ErrAdmissionQueueFull) {
		slog.Warn("realtime session admission rejected", "path", r.URL.Path, "model", req.Model, "code", completionErrorCode(err))
	} else {
		slog.Debug("realtime session rejected: routing failed", "path", r.URL.Path, "model", req.Model, "code", completionErrorCode(err), "err", err)
	}
	writeJSON(w, completionHTTPStatus(err), completionErrorResponse(err))
}

// realtimeUpstreamPath is the agent route a session on target is dialed at:
// /realtime/<upstream model>, the model (an instance address m#k included) escaped
// per path segment, which the agent decodes back.
func realtimeUpstreamPath(target routing.Target) string {
	return provider.ExpandModelPath("/realtime/{model}", effectiveProviderModel(target, target.Model))
}

// realtimeUpstreamQuery is the query the agent is dialed with: the client's, minus
// model -- the agent names the child's model itself, from the path.
func realtimeUpstreamQuery(q url.Values) string {
	forwarded := url.Values{}
	for name, values := range q {
		if name != "model" {
			forwarded[name] = values
		}
	}
	return forwarded.Encode()
}

// realtimeOutcome is what a session's usage row says about how it ended: the HTTP
// status (101 for a session that upgraded, the refusal's status for one that did
// not), the row status and its error code.
type realtimeOutcome struct {
	httpStatus int
	status     string
	errorCode  string
	upgraded   bool
}

// serveRealtimeSession runs one session on target, from its active request to its
// usage row. Exactly one usage row is recorded for it, and its active request removed,
// on every path, by one deferred teardown: a failed dial, a failed accept, and every
// end of a relayed session.
//
// The active request is registered before the dial, which can take as long as the
// agent needs to start the model: from then on the session counts on its instance and
// shows in Active Requests. The agent is dialed with r's context, so a client that
// leaves during the dial abandons it; once the socket is accepted the session ends
// only as relayRealtime describes.
func (s *Server) serveRealtimeSession(w http.ResponseWriter, r *http.Request, token auth.Token, req inference.Request, target routing.Target) {
	start := time.Now()
	id := nextRequestID()
	path := realtimeUpstreamPath(target)
	s.Active.Add(ActiveRequest{
		ID: id, UserID: token.UserID, TokenID: token.ID, TokenName: token.Name,
		ServiceID: token.ServiceID, ServiceName: token.ServiceName,
		ServerName: s.serverName(target.ServerID), ServerID: target.ServerID, InstanceKey: target.InstanceKey,
		Model: req.Model, RequestedModel: req.RequestedModel,
		APIFlavor: req.APIFlavor, BillingUnit: billingUnitFor(req.APIFlavor),
		ReqPath: r.URL.Path, ProviderPath: path, ProviderModel: effectiveProviderModel(target, req.Model),
		SessionID: req.ClientSessionID, SessionSource: req.SessionSource, AgentID: req.AgentID,
		Stream: true, StartedAt: start,
	})
	var meter realtimeMeter
	// Until the session says otherwise, it failed; a panic below is recorded so.
	out := realtimeOutcome{httpStatus: http.StatusBadGateway, status: "error", errorCode: provider.ErrUnavailable.Error()}
	defer func() {
		s.recordRealtimeUsage(start, id, r, token, req, target, out, &meter)
		s.Active.Remove(id)
	}()

	upstream, err := s.dialRealtimeUpstream(r.Context(), target, path, r.URL.Query())
	if err != nil {
		status, code := completionHTTPStatus(err), completionErrorCode(err)
		if r.Context().Err() != nil {
			code = errCodeClientDisconnected
		}
		slog.Warn("realtime: the upstream dial failed", "id", id, "model", req.Model, "server", s.serverName(target.ServerID), "instance", target.InstanceKey, "code", code, "err", err)
		writeJSON(w, status, completionErrorResponse(err))
		out = realtimeOutcome{httpStatus: status, status: "error", errorCode: code}
		return
	}
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{realtimeSubprotocol}, InsecureSkipVerify: true})
	if err != nil {
		// Accept has answered the client itself. The handshake was checked before
		// anything was spent, so only a writer that cannot be hijacked gets here.
		// Nothing was relayed, and nothing reads the upstream socket, so it is dropped.
		_ = upstream.CloseNow()
		slog.Error("realtime: accepting the client's socket failed after the upstream dial", "id", id, "model", req.Model, "err", err)
		out = realtimeOutcome{httpStatus: http.StatusInternalServerError, status: "error", errorCode: codeRealtimeAcceptFailed}
		return
	}
	client.SetReadLimit(realtimeMessageLimit)
	upstream.SetReadLimit(realtimeMessageLimit)
	end := s.relayRealtime(r.Context(), client, upstream, &meter)
	out = realtimeOutcomeOf(end)
	slog.Debug("realtime session ended", "id", id, "model", req.Model, "code", int(end.code), "cause", end.cause.String())
}

// dialRealtimeUpstream dials the agent's realtime route for target with the
// target's upstream credential and nothing of the client's handshake.
func (s *Server) dialRealtimeUpstream(ctx context.Context, target routing.Target, path string, query url.Values) (*websocket.Conn, error) {
	dialer, ok := s.Provider.(provider.NativeWebSocketDialer)
	if !ok {
		return nil, fmt.Errorf("%w: the native provider cannot dial a WebSocket", provider.ErrUnavailable)
	}
	conn, _, err := dialer.DialWebSocket(s.upstreamAuthCtx(ctx, target), target, path, realtimeUpstreamQuery(query), nil)
	return conn, err
}

// realtimeOutcomeOf is the outcome of a relayed session that ended with end: a
// success when it closed normally (1000), went away (1001) or closed without a status
// (1005), an error row naming the close status otherwise. Either way the HTTP status
// is the handshake's 101.
func realtimeOutcomeOf(end realtimeEnd) realtimeOutcome {
	out := realtimeOutcome{httpStatus: http.StatusSwitchingProtocols, status: "success", upgraded: true}
	switch end.code {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway, websocket.StatusNoStatusRcvd:
	default:
		out.status, out.errorCode = "error", realtimeCloseCodePrefix+strconv.Itoa(int(end.code))
	}
	return out
}

// recordRealtimeUsage records a session's one usage row. Its measure, by the billing
// XOR (usage.ValidateBillingXOR), is exactly one of:
//   - tokens, when any response.done carried a usage object: the sum over all of them
//     (realtimeMeter.observeUpstream), token-metered;
//   - otherwise audio seconds, when the client appended audio: its decoded bytes over
//     realtimeInputBytesPerSecond;
//   - otherwise a zero in audio seconds, which a session that upgraded logs as an
//     error (a relayed session nothing can be billed for); a session refused before
//     the upgrade logged its refusal already.
func (s *Server) recordRealtimeUsage(start time.Time, id string, r *http.Request, token auth.Token, req inference.Request, target routing.Target, out realtimeOutcome, meter *realtimeMeter) {
	meta := usageMeta{ReqPath: r.URL.Path, HTTPStatus: out.httpStatus, Endpoint: endpointRealtime}
	if !out.upgraded {
		meta.ContentType = jsonContentType
	}
	var resp provider.Response
	switch {
	case meter.usageSeen:
		resp.Usage = inference.Usage{
			InputTokens:  int(meter.inputTokens),
			OutputTokens: int(meter.outputTokens),
			TotalTokens:  int(meter.inputTokens + meter.outputTokens),
		}
	case meter.appendBytes > 0:
		meta.BillingUnit = usage.BillingUnitAudioSecond
		meta.BillingQuantity = float64(meter.appendBytes) / realtimeInputBytesPerSecond
	default:
		meta.BillingUnit = usage.BillingUnitAudioSecond
		if out.upgraded {
			slog.Error("realtime session billed zero: no response.done carried usage and no audio was appended",
				"id", id, "path", meta.ReqPath, "model", req.Model, "server", s.serverName(target.ServerID), "status", out.status, "code", out.errorCode)
		}
	}
	req.Stream = true
	s.recordUsage(start, token, req, target, resp, out.errorCode, out.status, meta, id, nil, nil)
}

// realtimeMeter counts what a session is billed for while its frames are relayed. The
// client pump alone writes appendBytes and the upstream pump alone the token fields;
// both are read only once the pumps have exited (relayRealtime joins them), so no
// lock is needed.
type realtimeMeter struct {
	appendBytes  int64
	usageSeen    bool
	inputTokens  int64
	outputTokens int64
}

// observeClient counts the decoded audio of a client's input_audio_buffer.append that
// was relayed upstream. A frame that is not a text JSON object of that type, or whose
// audio is not base64, counts nothing: billing never alters or refuses a frame.
func (m *realtimeMeter) observeClient(typ websocket.MessageType, data []byte) {
	if typ != websocket.MessageText || !bytes.Contains(data, realtimeAppendEvent) {
		return
	}
	var ev struct {
		Type  string `json:"type"`
		Audio string `json:"audio"`
	}
	if json.Unmarshal(data, &ev) != nil || ev.Type != string(realtimeAppendEvent) || ev.Audio == "" {
		return
	}
	n, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(ev.Audio)))
	if err != nil {
		return
	}
	m.appendBytes += n
}

// observeUpstream adds the usage of a response.done whose response.usage is an object:
// prompt_tokens (or input_tokens) and completion_tokens (or output_tokens). A null or
// absent usage, or a frame that does not decode, adds nothing.
func (m *realtimeMeter) observeUpstream(typ websocket.MessageType, data []byte) {
	if typ != websocket.MessageText || !bytes.Contains(data, realtimeDoneEvent) {
		return
	}
	var ev struct {
		Type     string `json:"type"`
		Response *struct {
			Usage *struct {
				PromptTokens     *float64 `json:"prompt_tokens"`
				CompletionTokens *float64 `json:"completion_tokens"`
				InputTokens      *float64 `json:"input_tokens"`
				OutputTokens     *float64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &ev) != nil || ev.Type != string(realtimeDoneEvent) || ev.Response == nil || ev.Response.Usage == nil {
		return
	}
	u := ev.Response.Usage
	m.usageSeen = true
	m.inputTokens += realtimeTokenCount(u.PromptTokens, u.InputTokens)
	m.outputTokens += realtimeTokenCount(u.CompletionTokens, u.OutputTokens)
}

// realtimeTokenCount is the first present of a usage field's two spellings, as a
// whole non-negative count.
func realtimeTokenCount(primary, alternate *float64) int64 {
	v := primary
	if v == nil {
		v = alternate
	}
	if v == nil || *v < 0 {
		return 0
	}
	return int64(*v)
}

// realtimeSide names one end of a session.
type realtimeSide int

const (
	realtimeSideNone     realtimeSide = iota // the gateway itself ended the session
	realtimeSideClient                       // the client's socket
	realtimeSideUpstream                     // the agent's socket
)

func (s realtimeSide) String() string {
	switch s {
	case realtimeSideClient:
		return "client"
	case realtimeSideUpstream:
		return "upstream"
	}
	return "gateway"
}

// lostReason is the close reason the other side gets when this one is lost without
// a close frame.
func (s realtimeSide) lostReason() string {
	return s.String() + " connection lost"
}

// other is the opposite side.
func (s realtimeSide) other() realtimeSide {
	if s == realtimeSideUpstream {
		return realtimeSideClient
	}
	return realtimeSideUpstream
}

// realtimeEnd is why a session ended: the status and reason both sides are closed
// with, and the side whose socket caused it (realtimeSideNone when the gateway ended
// it). write marks an end a failed write reported, which names the cause's socket as
// lost though its own read usually says why a moment later (settleRealtimeEnd).
type realtimeEnd struct {
	code   websocket.StatusCode
	reason string
	cause  realtimeSide
	write  bool
}

// realtimeIdleBound is a session's idle bound: the Server's realtimeIdle when set
// (tests), realtimeIdleTimeout otherwise.
func (s *Server) realtimeIdleBound() time.Duration {
	if s.realtimeIdle > 0 {
		return s.realtimeIdle
	}
	return realtimeIdleTimeout
}

// relayRealtime pumps messages between client and upstream until the session ends,
// closes both sides and returns why it ended -- the agent router's relay, mirrored, so
// a session ends the same way on both hops. One goroutine per direction reads a
// message from one side and writes it to the other with its type; meter observes
// each one (an upstream message as it is read, since the upstream did that work
// whether or not the client is still there to get it; a client message once it was
// relayed); this goroutine watches the pumps, the idle timer and the shutdown.
//
// The session ends at the first of:
//   - a side's close frame: the other side is closed with the same status and reason.
//     A status that may not be sent on becomes 1011 (sendableRealtimeCode); a close
//     frame without a status (1005) is relayed as one.
//   - a side lost without a close frame (a read or write error): the other side is
//     closed with 1011. A failed write waits briefly for the read that names the real
//     cause (settleRealtimeEnd).
//   - a message over realtimeMessageLimit: coder/websocket has already sent the
//     offending side 1009, and the other side is closed with 1009 too.
//   - the idle bound without a message in either direction (pings do not count):
//     both sides are closed with 1008 "idle_timeout".
//   - the request context ending or the gateway shutting down: both sides are closed
//     with 1001.
//
// It returns only once both sockets are closed and both pumps have exited, so the
// caller's usage row and active-request removal follow the session on both sides.
func (s *Server) relayRealtime(ctx context.Context, client, upstream *websocket.Conn, meter *realtimeMeter) realtimeEnd {
	activity := make(chan struct{}, 1)
	ends := make(chan realtimeEnd, 2) // each pump reports exactly once, so a report never blocks
	var pumps sync.WaitGroup
	pumps.Add(2)
	go func() {
		defer pumps.Done()
		pumpRealtime(client, upstream, realtimeSideClient, activity, ends, nil, meter.observeClient)
	}()
	go func() {
		defer pumps.Done()
		pumpRealtime(upstream, client, realtimeSideUpstream, activity, ends, meter.observeUpstream, nil)
	}()

	idleBound := s.realtimeIdleBound()
	idle := time.NewTimer(idleBound)
	defer idle.Stop()
	var end realtimeEnd
wait:
	for {
		select {
		case <-activity:
			idle.Reset(idleBound)
		case end = <-ends:
			end = settleRealtimeEnd(end, ends, realtimeCauseGrace)
			break wait
		case <-idle.C:
			end = realtimeEnd{code: websocket.StatusPolicyViolation, reason: realtimeReasonIdle, cause: realtimeSideNone}
			break wait
		case <-ctx.Done():
			end = realtimeEnd{code: websocket.StatusGoingAway, reason: realtimeReasonShutdown, cause: realtimeSideNone}
			break wait
		case <-s.lifetime().Done():
			end = realtimeEnd{code: websocket.StatusGoingAway, reason: realtimeReasonShutdown, cause: realtimeSideNone}
			break wait
		}
	}

	closeRealtime(client, upstream, end)
	pumps.Wait() // both sockets are closed now, so both pumps' reads fail at once
	return end
}

// settleRealtimeEnd returns the end a session closes with, given the first end a pump
// reported. A read's end is final. A failed write's is not: when side X closes, the
// pump writing to X fails at once with X "lost" and races X's own pump, which reads
// X's close frame with its real status. So a write's end waits up to wait for the
// other pump's end and prefers it when a read reported it. Each pump reports exactly
// once, so the next end on ends is the other pump's.
func settleRealtimeEnd(first realtimeEnd, ends <-chan realtimeEnd, wait time.Duration) realtimeEnd {
	if !first.write {
		return first
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case next := <-ends:
		if !next.write {
			return next
		}
	case <-timer.C:
	}
	return first
}

// pumpRealtime relays messages from src to dst until either fails, reports why on
// ends exactly once, and returns once src can no longer be read. Every message read
// signals activity without blocking; onRead sees it as read, onWritten once dst took
// it (either may be nil).
//
// A failed write ends the session but not the reading: the pump goes on reading src,
// discarding (onRead still sees each message), until src is closed, so src keeps a
// reader for its close handshake. The context is never canceled: a canceled read or
// write makes coder/websocket drop the connection without a close frame, and the
// session closes its sockets with one instead.
func pumpRealtime(src, dst *websocket.Conn, from realtimeSide, activity chan<- struct{}, ends chan<- realtimeEnd, onRead, onWritten func(websocket.MessageType, []byte)) {
	ctx := context.Background()
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			ends <- realtimeEndOfRead(from, err)
			return
		}
		select {
		case activity <- struct{}{}:
		default:
		}
		if onRead != nil {
			onRead(typ, data)
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			to := from.other()
			ends <- realtimeEnd{code: websocket.StatusInternalError, reason: to.lostReason(), cause: to, write: true}
			drainRealtime(ctx, src, onRead)
			return
		}
		if onWritten != nil {
			onWritten(typ, data)
		}
	}
}

// drainRealtime reads src, discarding, until it fails.
func drainRealtime(ctx context.Context, src *websocket.Conn, onRead func(websocket.MessageType, []byte)) {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return
		}
		if onRead != nil {
			onRead(typ, data)
		}
	}
}

// realtimeEndOfRead maps the error that ended a read from side to the session's end:
// a close frame's own status and reason, 1009 for a message over the limit, and 1011
// for a side lost without a close frame.
func realtimeEndOfRead(side realtimeSide, err error) realtimeEnd {
	var ce websocket.CloseError
	switch {
	case errors.As(err, &ce):
		return realtimeEnd{code: sendableRealtimeCode(ce.Code), reason: ce.Reason, cause: side}
	case errors.Is(err, websocket.ErrMessageTooBig):
		return realtimeEnd{code: websocket.StatusMessageTooBig, reason: realtimeReasonTooBig, cause: side}
	default:
		return realtimeEnd{code: websocket.StatusInternalError, reason: side.lostReason(), cause: side}
	}
}

// sendableRealtimeCode is code when a close frame may carry it on (RFC 6455 §7.4:
// 1000-1003, 1007-1014, 3000-4999), and 1011 otherwise. 1005, "no status received",
// is kept: coder/websocket sends it as a close frame without a status, which is
// exactly what the peer sent.
func sendableRealtimeCode(code websocket.StatusCode) websocket.StatusCode {
	switch {
	case code == websocket.StatusNoStatusRcvd,
		code >= websocket.StatusNormalClosure && code <= websocket.StatusUnsupportedData,
		code >= websocket.StatusInvalidFramePayloadData && code <= websocket.StatusBadGateway,
		code >= 3000 && code <= 4999:
		return code
	default:
		return websocket.StatusInternalError
	}
}

// closeRealtime closes both sides of a session for end, at the same time, and returns
// once both are closed: the side that caused the end without a handshake (its close
// frame was received or sent already, or it is gone -- and it is the one socket that
// can have the unread rest of an oversized message pending, which a handshake would
// first have to read), any other with end's status and reason. coder/websocket bounds
// that handshake itself (5 s to write the close frame, 5 s for the answer).
func closeRealtime(client, upstream *websocket.Conn, end realtimeEnd) {
	var wg sync.WaitGroup
	closeSide := func(conn *websocket.Conn, side realtimeSide) {
		defer wg.Done()
		if side == end.cause {
			_ = conn.CloseNow()
			return
		}
		// A peer that closed without a status leaves end.code at 1005, which
		// coder/websocket sends as a close frame without a status: the other side gets
		// exactly what the peer sent.
		_ = conn.Close(end.code, end.reason)
	}
	wg.Add(2)
	go closeSide(client, realtimeSideClient)
	go closeSide(upstream, realtimeSideUpstream)
	wg.Wait()
}
