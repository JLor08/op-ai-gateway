// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// This file is the router port (design doc §6.1): the single HTTP port the
// gateway talks to for a server_agent application, which routes every
// inference request to the right managed model process, starting it first
// if necessary. Seven route classes:
//
//   - GET /health, GET /v1/health -- always 200 while the router is up.
//     "Reachability means the router accepts, not that a model is warm" --
//     otherwise a cold server falls out of routing (the gateway's
//     application health probe has a 3s timeout and flips an application
//     unreachable after ONE failed cycle) and can never warm up.
//   - GET /running, GET /v1/models -- the currently-loaded set (llama-swap
//     shape) and the full managed set including cold specs (OpenAI shape),
//     respectively. Neither blocks on anything a model is doing: both read
//     straight off Manager's already-non-blocking Status()/LoadedModels()
//     (Task 14's serialized owner answers these on its own command channel,
//     interleaved with -- never behind -- a pending admission/start).
//   - GET /upstream/{model}/props -- the GET-only, allowlisted passthrough
//     to a RUNNING managed child's /props (issue #58): model from the PATH
//     (prefix/suffix decomposition, so ids containing "/" work), resolved
//     by the manager's address rule (ResolveAddress) to the instance it
//     names and then to that instance's running row in Status() -- NEVER an
//     ensure, a probe must not start or keep alive a child -- and relayed
//     byte-verbatim so the gateway's evidence rule reads the child's own
//     document. The allowlist is /props, GET
//     /upstream/{model}/v1/audio/voices (issue #251, the "runtime_voices"
//     feature: an audio.cpp child's voice listing, same never-start shape,
//     outbound path /v1/audio/voices with no query) and GET
//     /upstream/{model}/v1/models (the
//     "runtime_model_params" feature: an audio.cpp child's model listing
//     with each model's declared request params, outbound path /v1/models
//     with the inbound query, so ?include_params=true reaches the child);
//     widening it further is #49-2/#55 business.
//   - POST /ensure/{model} -- start a managed child without forwarding a
//     request to it: model from the PATH (everything after "/ensure/", so
//     ids containing "/" work), EnsureRunning under the request context,
//     then 200 {"status":"running"}. Bodiless, so a router without this
//     route answers it from serveProxy with 404 runtime.model_not_managed
//     before EnsureRunning and never starts a child for it; any body a
//     caller sends anyway is read and discarded, never forwarded. No
//     heartbeats: a failure is a genuine HTTP status from the sentinel table
//     below.
//   - POST /stop/{model} -- begin a graceful drain of ONE instance on the
//     operator's request, persisting nothing (Manager.Stop): model from the
//     PATH like /ensure, then 200 {"state":"stopping"|"stopped"} or a 404/409
//     refusal. Bodiless, so a router without this route answers it from
//     serveProxy with 404 runtime.model_not_managed and stops nothing; the
//     gateway calls it only on an agent that declares "runtime_stop".
//   - GET /realtime/{model} -- the one route that upgrades a connection: a
//     WebSocket relay to a managed child's /v1/realtime (the "realtime_proxy"
//     feature), model from the PATH like /live, the admission held for the
//     whole session, which the router bounds itself -- an idle timeout, a
//     message limit, and Close ending every session (router_realtime.go).
//     Every other route still refuses an upgrade (deadlineWriter.Hijack).
//   - everything else -- the model-routed reverse proxy. The model comes from
//     the JSON body or, when the request is multipart/form-data (an image
//     edit or an audio transcription, "multipart_proxy" in the agent's
//     feature registry), from the form's "model" part, read the way the
//     gateway reads it (peekMultipartModel). A multipart request is always a
//     plain relay, and the router is blind to the path it was sent to.
//
// INSTANCE ADDRESSES (#340). A model may name one instance of a replicable
// spec: "m" is instance 0, "m#k" instance k, read by the manager's own rule
// (Manager.ResolveAddress -- an exact upstream match wins first, so a real
// model whose name merely contains '#' stays itself). Every route reaches the
// instance the address names, and the child -- every instance is launched with
// the base name m -- sees m in its place: the top-level JSON model
// (rewriteModelField, spliced in place, every other byte as sent), the
// multipart model field (rebuildMultipartModel, every other part as sent), a
// live relay's model= and the value of an /upstream probe's model parameter.
// Instance 0, which is every request of a spec that never replicates, is never
// rewritten and passes byte for byte. A request the router forwards is
// admitted with EnsureServing, which never starts a burst instance (409
// runtime.instance_no_capacity when none is up); only POST /ensure/{model}
// (EnsureRunning) may start one, which is how the gateway adds capacity. The
// address is read before admission and again after it (addressMoved): a
// config push between the two would otherwise hand the child a name chosen
// for another reading, so such a request is refused with the same 409.
// /running lists running instances by address, /v1/models every spec once, by
// its base name.
//
// STREAMING ASYMMETRY (the whole point of this design): request bodies are
// buffered (bounded by maxBodyBytes) because `model`/`stream` must be read
// out of the JSON (or the multipart form) before the router knows where to
// send it. Responses are
// NEVER buffered -- httputil.ReverseProxy with FlushInterval:-1 for the
// plain path, and a hand-rolled splice-with-heartbeat loop for the streaming
// path, both flush after every write so SSE tokens (and any other streamed
// bytes) reach the client as they are produced.
//
// COLD-START HEARTBEATS, LAZILY COMMITTED (design doc §8.3, brief: "once
// admission succeeds, commit 200" -- after, not before): a request with
// "stream":true does NOT commit 200 + SSE headers up front. It arms a
// heartbeatInterval ticker and starts waiting on EnsureServing (then, on
// success, the upstream round trip, then the wait for the first response
// body byte) with that ticker still running. The FIRST time anything would
// actually need to be written to the client -- a heartbeat tick fires
// before the current phase resolves, or the current phase resolves with
// real data to write -- a single idempotent commit() finally sends 200 +
// SSE headers. Until that moment, nothing has been written at all, so a
// failure at any phase (model not managed, policy refusal, a synchronously
// failing exec, an immediate non-2xx from an already-admitted child, ...)
// is still reportable as a genuine HTTP status via the same sentinelCode()
// mapping the non-streaming path uses -- exactly like a non-streaming
// request would get. Only once a heartbeat has actually fired, or real
// bytes have actually started flowing, does the router commit to SSE; a
// failure from that point on can no longer be a status code (see ACCEPTED
// TRADE-OFF below). heartbeatInterval covers BOTH the cold-start wait
// (EnsureServing pending) and a time-to-first-token wait on an
// already-warm child (the upstream request pending), which the design doc
// treats as the same "silent window" failure class. Once real bytes start
// flowing, heartbeats stop for good and the upstream body is spliced
// through verbatim -- the router never re-frames the child's own SSE
// lines.
//
// ACCEPTED TRADE-OFF, deliberately implemented and documented here (design
// doc §8.3, §15): once 200 HAS been committed for a streaming request (a
// heartbeat fired, or real data started flowing, before the eventual
// outcome was known), no later failure -- EnsureServing erroring, the
// child dying mid-response -- can be reported as an HTTP status any more.
// Each is instead reported as a terminal SSE frame
// (`data: {"error":{"code":"...","message":"..."}}\n\n`) carrying the exact
// same stable code the non-streaming path would have used for that
// failure (§6.5's sentinel table, reproduced in sentinelCode below). The
// lazy commit means this trade-off is now paid only when it is actually
// earned by a genuinely slow phase, not on every streaming request
// regardless of how fast it resolves.
//
// A non-2xx from an already-admitted child is deliberately NOT part of
// this trade-off (C1): it is the model server's own legitimate response
// (a 400 "context length exceeded", a 422 for a bad tool schema, ...), not
// a router-level failure, and the brief only prescribes the error-frame
// treatment for a non-2xx AFTER heartbeats began. So a non-2xx arriving
// before anything is committed is forwarded verbatim -- status, headers,
// body -- exactly like the non-streaming path would for the identical
// response (forwardUpstreamResponse); only a non-2xx arriving AFTER 200 is
// already committed falls into the terminal-frame trade-off above, since
// by then there is genuinely no HTTP status left to give it.
package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxBodyBytes bounds the buffered read of an inbound request body (needed
// to extract `model`/`stream` before the destination is known), and the
// discarded read of an ensure request's body. A request body beyond this is
// rejected with 413 before any admission decision is even attempted.
const maxBodyBytes = 32 << 20

// maxMultipartModelBytes bounds the read of the one multipart part the router
// reads, the form's "model" field. It is the gateway's own bound on a text field
// of an image edit (maxImageEditFieldBytes) and of a transcription
// (maxTranscriptionFieldBytes), so the parsers agree on which model fields are
// too large to be a model name at all.
const maxMultipartModelBytes = 1 << 20

// headerContentType is the Content-Type header name, read off the inbound request
// to tell a multipart body from a JSON one and set on the router's own responses.
const headerContentType = "Content-Type"

// heartbeatInterval is the cadence of `: keepalive` SSE comment lines during
// a streaming request's silent window (cold start or time-to-first-token).
// Package-level var, not const, so a test can shrink it -- the
// manager.go/client.backoffBase/certinstall.hookTimeout convention this
// module already follows throughout.
var heartbeatInterval = 10 * time.Second

// writeDeadline bounds how long the router waits for a single write to the
// DOWNSTREAM client to complete. The upstream leg (router -> child)
// deliberately has no timeout of its own -- the gateway owns that
// deadline (router struct's own doc comment) -- but a stalled downstream
// reader is a different failure mode entirely: without this, a client
// that simply stops reading pins the response-copying goroutine, and the
// deferred release() with it, for as long as it stalls, making that spec
// permanently un-evictable from the outside -- the same failure mode the
// manager already had to fix once for its own admission path (I5).
// Refreshed after every successful write rather than set once up front,
// so a slow-but-still-reading client is never penalized for a long
// response's cumulative time. Package-level var, not const, so a test can
// shrink it.
var writeDeadline = 30 * time.Second

// refreshWriteDeadline extends w's write deadline by writeDeadline from
// now, best-effort: not every http.ResponseWriter supports a write
// deadline (e.g. httptest.NewRecorder does not, returning
// http.ErrNotSupported), and a caller that cannot set one is no worse off
// than before this existed.
func refreshWriteDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(writeDeadline))
}

// deadlineWriter wraps an http.ResponseWriter, calling refreshWriteDeadline
// before every Write -- used by the plain (non-streaming) proxy path so a
// downstream client that stops reading cannot pin httputil.ReverseProxy's
// own internal body-copy loop (and the deferred release() with it)
// indefinitely, the same I5 concern the streaming path's flushWriter
// addresses for its own copy loop. Declares its own Flush method
// (delegating via a type assertion) so it still satisfies http.Flusher for
// ReverseProxy's FlushInterval:-1 check -- embedding the http.ResponseWriter
// interface alone promotes Header/Write/WriteHeader but NOT Flush, which is
// a separate interface.
type deadlineWriter struct {
	http.ResponseWriter
}

func (dw deadlineWriter) Write(p []byte) (int, error) {
	refreshWriteDeadline(dw.ResponseWriter)
	return dw.ResponseWriter.Write(p)
}

// Flush refreshes the write deadline before flushing, rather than relying on
// a Write having just happened. httputil.ReverseProxy's maxLatencyWriter
// currently always calls Write immediately before Flush under
// FlushInterval:-1, so the refresh in Write does cover today's only caller
// -- but that is a property of ReverseProxy's internals, not of this type,
// and Flush is where the bytes actually leave for the client. A flush that
// blocks on a stalled reader with no deadline armed is the unbounded hang
// this wrapper exists to prevent, so it arms one itself.
func (dw deadlineWriter) Flush() {
	refreshWriteDeadline(dw.ResponseWriter)
	if f, ok := dw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the wrapped writer, the codebase-wide convention for a
// ResponseWriter wrapper (gateway/backend/internal/gateway/server.go's
// accessLogResponseWriter). It is what lets http.NewResponseController
// reach through this wrapper to the real connection -- so a future caller
// that hands a deadlineWriter (rather than the bare w) to a
// ResponseController still gets working deadline/flush control instead of
// http.ErrNotSupported.
//
// Deliberately NOT forwarded: io.ReaderFrom. Promoting ReadFrom would let
// io.Copy bypass Write entirely and with it the deadline refresh that is
// this type's whole purpose -- the opposite of robustness. For ReaderFrom,
// simply not declaring the method is enough; for http.Hijacker it is NOT
// (see Hijack below). (Anything reached through Unwrap, including
// ResponseController's own Flush, is by definition outside the
// Write-refresh discipline; callers wanting the deadline must go through
// Write/Flush above.)
func (dw deadlineWriter) Unwrap() http.ResponseWriter {
	return dw.ResponseWriter
}

// Hijack refuses, explicitly, and stops the Unwrap chain here.
//
// Fix round 1, F2 -- correcting this type's earlier claim that http.Hijacker
// was "left unforwarded" because the method is not declared:
// http.ResponseController walks the Unwrap chain for EVERY optional method,
// Hijack included (net/http's responsecontroller.go: "case Hijacker", then
// "case rwUnwrapper"), and httputil.ReverseProxy reaches Hijack by exactly
// that route when a child answers 101 Switching Protocols. So adding Unwrap
// promoted Hijack, and the hijack SUCCEEDED: ReverseProxy then blocks
// splicing the two raw connections with no write deadline (a deadlineWriter
// is out of the picture once the connection is raw), no request-context
// cancellation, and no rescue from Server.Close (hijacked connections are
// untracked, so stopRouterLocked's restart cannot close it) -- and
// servePlainProxy's deferred release() never runs. One un-evictable spec
// holding its VRAM for the agent's lifetime: precisely the failure mode this
// wrapper exists to prevent, reintroduced through the door opened for
// SetWriteDeadline.
//
// http.ErrNotSupported specifically, because ReverseProxy tests for it
// (errors.Is(hijackErr, http.ErrNotSupported)) and answers the client
// through ErrorHandler -- the pre-Unwrap 502, with a prompt release() --
// instead of failing some other way.
//
// GET /realtime/{model} is the one route that upgrades, and it never runs
// through a deadlineWriter: it hands coder/websocket the server's own writer
// and solves the three problems above itself (serveRealtime).
func (dw deadlineWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}

// hopByHopHeaders are stripped from the outbound request the router builds
// by hand for the streaming path (RFC 7230 §6.1). httputil.ReverseProxy
// already does this internally for the plain path; this list exists only
// because the streaming path bypasses ReverseProxy to interleave heartbeats
// with the upstream round trip.
var hopByHopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// managerPort is the router's view of *Manager: deliberately small and
// unexported so a hand-written fake can stand in wherever a test needs to
// observe or control something the real Manager+stub-child combination
// cannot easily produce (this module's house pattern -- no mocking
// framework is used or added; see e.g. router_test.go's countingManager,
// which wraps a real Manager to count release() calls independently of the
// real EnsureRunning's own internal idempotency guard). nil is a valid
// managerPort: every handler below treats "no manager" as "nothing is
// managed" rather than panicking.
//
// Two ensures, because the router asks two different questions (#340): an
// ensure (POST /ensure/{model}) may start any instance it names, a burst one
// included -- that is how the gateway adds capacity -- while a request the
// router forwards (EnsureServing) is served by a burst instance only when one
// is already up. ResolveAddress is the manager's own address rule, which the
// router reads to hand an instance's child its base model name.
type managerPort interface {
	EnsureRunning(ctx context.Context, upstreamModel string) (endpoint string, release func(), err error)
	EnsureServing(ctx context.Context, upstreamModel string) (endpoint string, release func(), err error)
	ResolveAddress(model string) (base string, k int, ok bool)
	LoadedModels() []string
	Status() []Status
}

// NewRouter returns the router-port handler. The router serves BOTH
// "/health" and "/v1/health" as always-200 liveness paths (the agent does not
// know which HealthCheckPath the gateway application row configures, so it
// answers both; the portal's server_agent type default is "/v1/health").
func NewRouter(m *Manager) http.Handler {
	// M6: a nil *Manager wrapped in the managerPort interface value is a
	// non-nil interface holding a nil pointer (the classic Go trap) -- the
	// router's own "rt.m != nil" nil-guards would never fire, silently
	// defeating the documented "nil is a valid managerPort" property
	// (managerPort's doc comment above). Pass a literal untyped nil
	// instead so it is genuinely nil on the other side.
	if m == nil {
		return newRouter(nil)
	}
	return newRouter(m)
}

// router is the concrete handler behind NewRouter; split out from it so
// tests can construct one directly over a managerPort fake without going
// through the exported *Manager-typed constructor.
type router struct {
	m managerPort
	// transport is shared by every proxied request (plain and streaming
	// alike): a bare http.Transport with no timeouts of its own -- the
	// gateway owns request deadlines end to end, per the design doc's own
	// framing of this feature as a component that must never add a new
	// timer to that chain.
	transport http.RoundTripper
	// realtime tracks the open GET /realtime/{model} sessions, so Close can
	// end them (router_realtime.go).
	realtime *realtimeSessions
	// realtimeIdle is a realtime session's idle timeout (realtimeIdleTimeout),
	// realtimeCloseWait how long Close waits for the sessions it ends
	// (realtimeCloseBound), and realtimeCauseWait how long a session that
	// ended on a failed write waits for the read naming the cause
	// (realtimeCauseGrace). Fields rather than constants so a test can set
	// them on its own router.
	realtimeIdle      time.Duration
	realtimeCloseWait time.Duration
	realtimeCauseWait time.Duration
}

func newRouter(m managerPort) *router {
	return &router{
		m:                 m,
		transport:         &http.Transport{},
		realtime:          newRealtimeSessions(),
		realtimeIdle:      realtimeIdleTimeout,
		realtimeCloseWait: realtimeCloseBound,
		realtimeCauseWait: realtimeCauseGrace,
	}
}

func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// M9: every control path answers one method -- GET (GET /realtime/{model}
	// included), or POST for /ensure/{model}, /stop/{model}, /live/{model} and
	// /rpc/...; a different method on one of these paths falls
	// through to the model-routed proxy instead (the ordinary "everything
	// else" case), rather than getting the control treatment regardless
	// of method.
	isGet := r.Method == http.MethodGet
	switch {
	case isGet && (r.URL.Path == "/health" || r.URL.Path == "/v1/health"):
		rt.serveHealth(w, r)
	case isGet && r.URL.Path == "/running":
		rt.serveRunning(w, r)
	case isGet && r.URL.Path == "/v1/models":
		rt.serveModels(w, r)
	case isGet && strings.HasPrefix(r.URL.Path, "/upstream/"):
		rt.serveUpstream(w, r)
	case isGet && strings.HasPrefix(r.URL.Path, realtimePathPrefix):
		rt.serveRealtime(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, ensurePathPrefix):
		rt.serveEnsure(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, stopPathPrefix):
		rt.serveStop(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, livePathPrefix):
		rt.serveLive(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, rpcPathPrefix):
		rt.serveRPC(w, r)
	default:
		rt.serveProxy(w, r)
	}
}

// healthResponse is served unconditionally: liveness means "the router
// accepts", never "a model is warm". See the package doc above for why this
// must never block.
type healthResponse struct {
	Status string `json:"status"`
}

func (rt *router) serveHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

// runningEntry/runningResponse mirror llama-swap's /running shape exactly,
// so the gateway's existing LoadedModelsFormat:"llama_swap" detection keeps
// working unchanged against a server_agent application. Each entry is one
// running instance by its address (#340, LoadedModels): "m" for instance 0,
// "m#k" for a further one.
type runningEntry struct {
	Model string `json:"model"`
	State string `json:"state"`
}

type runningResponse struct {
	Running []runningEntry `json:"running"`
}

func (rt *router) serveRunning(w http.ResponseWriter, _ *http.Request) {
	var loaded []string
	if rt.m != nil {
		loaded = rt.m.LoadedModels()
	}
	entries := make([]runningEntry, 0, len(loaded))
	for _, name := range loaded {
		entries = append(entries, runningEntry{Model: name, State: "ready"})
	}
	writeJSON(w, http.StatusOK, runningResponse{Running: entries})
}

// modelEntry/modelsResponse mirror the OpenAI /v1/models list shape. Unlike
// /running, this lists EVERY managed (enabled) spec -- cold ones included --
// since they are servable on demand; the gateway's model-sync health mode
// and model listing both read this endpoint expecting the full set. A spec is
// listed once, by its base name, however many instances it runs (#340): an
// instance is a process of a model, not a model of its own, so only the
// instance-0 rows -- which every spec has -- are listed. An rpc_server member
// serves no model and is never listed.
type modelEntry struct {
	ID     string `json:"id"`
	Object string `json:"object"`
}

type modelsResponse struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
}

func (rt *router) serveModels(w http.ResponseWriter, _ *http.Request) {
	var statuses []Status
	if rt.m != nil {
		statuses = rt.m.Status()
	}
	data := make([]modelEntry, 0, len(statuses))
	for _, st := range statuses {
		if st.Instance != 0 || st.Kind == SpecKindRPCServer {
			continue
		}
		data = append(data, modelEntry{ID: st.Model, Object: "model"})
	}
	writeJSON(w, http.StatusOK, modelsResponse{Object: "list", Data: data})
}

// upstreamPropsSuffix, upstreamVoicesSuffix and upstreamModelsSuffix are the
// child endpoints the /upstream passthrough allowlists -- both the inbound
// suffix match and the outbound path.
const (
	upstreamPropsSuffix  = "/props"
	upstreamVoicesSuffix = "/v1/audio/voices"
	upstreamModelsSuffix = "/v1/models"
)

// upstreamEndpoint is one allowlisted child endpoint of the /upstream
// passthrough.
type upstreamEndpoint struct {
	// path is both the inbound suffix after the model and the outbound path on
	// the child.
	path string
	// forwardQuery relays the inbound query string to the child. /props keeps
	// it (llama.cpp reads e.g. ?model=); the voices read drops it, because
	// audio.cpp ignores ?model= (it lists the voices of the model it has
	// loaded), so the request's model only selects the child. The models
	// read keeps it: audio.cpp adds each model's declared request params
	// only when asked with ?include_params=true.
	forwardQuery bool
}

var upstreamEndpoints = []upstreamEndpoint{
	{path: upstreamPropsSuffix, forwardQuery: true},
	{path: upstreamVoicesSuffix, forwardQuery: false},
	{path: upstreamModelsSuffix, forwardQuery: true},
}

// serveUpstream handles GET /upstream/{model}/props (issue #58), GET
// /upstream/{model}/v1/audio/voices (issue #251) and GET
// /upstream/{model}/v1/models: the llama-swap-style upstream passthrough,
// restricted to a GET of exactly one allowlisted endpoint on a RUNNING child.
// The gateway probes an api-key-protected child's live-progress capability
// (/props), an audio.cpp child's voice listing (/v1/audio/voices, the
// "runtime_voices" feature) and its model listing with declared request
// params (/v1/models?include_params=true, the "runtime_model_params"
// feature) through it, attaching the spec's token -- which this handler, like
// the proxy paths, forwards verbatim (Authorization and custom token headers
// are not hop-by-hop).
//
// Deliberate properties, each one a guardrail from the issue:
//   - Read-only, never an ensure: the model is resolved by the manager's
//     address rule (ResolveAddress) and its running process found in
//     Status(), and nothing else is asked of the manager. llama-swap's own
//     /upstream route boots a cold model on contact -- a probe that starts
//     children is a hazard, not a feature. No inFlight/lastUsed touch either,
//     so the probe never keeps an idle child alive.
//   - Prefix/suffix decomposition, not segment matching: upstream model ids
//     are only TrimSpace-validated and may contain "/" (HF-style
//     "org/model"); everything between "/upstream/" and the trailing
//     allowlisted suffix is the model. provider.ExpandModelPath on the
//     gateway side keeps "/" literal, so the two ends agree.
//   - The outbound path is EXACTLY the allowlisted suffix -- never the
//     inbound path -- and the response is relayed unmodified: the evidence
//     rule's "role":"router" gate must see the child's own document, and a
//     voice listing must reach the gateway's parser as the child wrote it.
//   - Managed-but-cold gets its own sentinel (runtime.model_not_running):
//     §4.3's error codes are never collapsed, and "no spec" vs "not
//     running right now" are different diagnoses. An instance address its
//     spec allows but no process serves (a burst instance not started, or
//     retired) is managed and not running, the same 404
//     runtime.model_not_running; an address its spec does not allow is 404
//     runtime.model_not_managed, like an unknown model.
//   - An instance address reaches exactly the process of the instance it
//     names, and a model parameter in the forwarded query carries the base
//     name instead (upstreamChildQuery). An address whose reading moved while
//     the running row was looked up is refused with 409
//     runtime.instance_no_capacity (addressMoved).
func (rt *router) serveUpstream(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/upstream/")
	for _, ep := range upstreamEndpoints {
		if model, ok := strings.CutSuffix(rest, ep.path); ok {
			rt.relayToRunningChild(w, r, model, ep)
			return
		}
	}
	writeError(w, http.StatusNotFound, "runtime.upstream_endpoint_not_allowed",
		"only /props, /v1/audio/voices and /v1/models may be requested through /upstream/{model}")
}

// relayToRunningChild is the read-only relay behind serveUpstream: resolve
// model to a RUNNING child's port and GET ep.path on it, never starting one.
//
// The model is resolved first, by the manager's own rule (ResolveAddress), so
// whether it is managed at all is the same answer every other route gets --
// 404 runtime.model_not_managed when it is not -- and then to the running row
// of the instance it names in Status(), so the probe reaches exactly the
// process the address names (#340): an instance address the row of that
// instance, a literal name that merely contains '#' its own spec's instance-0
// row -- never the row of an instance whose address collided with it while that
// instance drains. An address its spec allows but no process serves (a burst
// instance not started, or retired) is managed but not running: 404
// runtime.model_not_running. The two reads are two owner turns, so an instance
// address is read again before anything is relayed, and one whose reading
// moved in between is refused with 409 runtime.instance_no_capacity
// (addressMoved).
func (rt *router) relayToRunningChild(w http.ResponseWriter, r *http.Request, model string, ep upstreamEndpoint) {
	if model == "" || rt.m == nil {
		writeError(w, http.StatusNotFound, codeModelNotManaged,
			"no active launch spec for this model")
		return
	}
	// Every name is resolved here, an instance-0 one included: whether it is
	// managed at all decides between the two 404s below. Only a name that
	// parses as an instance address is read again (addressMoved).
	_, parsed := ParseInstanceAddress(model)
	addr := modelAddress{name: model, resolved: parsed > 0}
	addr.base, addr.k, addr.managed = rt.m.ResolveAddress(model)
	if !addr.managed {
		writeError(w, http.StatusNotFound, codeModelNotManaged,
			"no active launch spec for this model")
		return
	}
	port := runningPort(rt.m.Status(), model, addr.k)
	if port == 0 {
		writeError(w, http.StatusNotFound, "runtime.model_not_running",
			"model is managed but not running; this probe never starts a child")
		return
	}
	// The row was read in an owner turn of its own: a probe whose address moved
	// meanwhile is refused like a forwarded request, rather than handed to the
	// process the earlier reading named.
	if rt.addressMoved(addr) {
		writeSentinelError(w, errAddressMoved(model))
		return
	}
	target := "http://127.0.0.1:" + strconv.Itoa(port) + ep.path
	if ep.forwardQuery && r.URL.RawQuery != "" {
		target += "?" + upstreamChildQuery(r.URL.RawQuery, addr.base, addr.k)
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, codeUpstreamGone, err.Error())
		return
	}
	req.Header = r.Header.Clone()
	for _, h := range hopByHopHeaders {
		req.Header.Del(h)
	}
	// Same I2 reasoning as buildUpstreamRequest: never forward the caller's
	// Accept-Encoding, so the Transport negotiates and transparently
	// decompresses on our behalf and the relayed bytes are the decoded body.
	req.Header.Del("Accept-Encoding")
	resp, err := rt.transport.RoundTrip(req)
	if err != nil {
		// The Status snapshot can race an idle drain: the child was running
		// a moment ago and the port is dead now. Same code the proxy paths
		// use for "something went wrong reaching an admitted child".
		writeError(w, http.StatusBadGateway, codeUpstreamGone, err.Error())
		return
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close, best-effort
	forwardUpstreamResponse(w, nil, resp)
}

// runningPort returns the port of the RUNNING process of instance k reported
// under the address model, or 0 when none is running. Among duplicate
// upstream_model specs -- whose collapse in byUpstream is an arbitrary map-order
// winner -- the first running row wins: a probe must read a live child.
func runningPort(statuses []Status, model string, k int) int {
	for _, st := range statuses {
		if st.Model == model && st.Instance == k && st.State == StateRunning && st.Port != 0 {
			return st.Port
		}
	}
	return 0
}

// upstreamChildQuery is the query the /upstream relay forwards: the inbound one
// byte for byte, except that for an instance address (k >= 1) the value of each
// model parameter becomes the base name -- the only name the instance's child
// knows -- query-escaped. Every other parameter keeps its place and its
// encoding, and the model parameter its key as written: a parameter is a model
// one when its key, unescaped as url.ParseQuery would, is "model". A model
// parameter with no '=' (a model that is empty) gets the base as well.
func upstreamChildQuery(raw, base string, k int) string {
	if k == 0 || raw == "" {
		return raw
	}
	params := strings.Split(raw, "&")
	for i, param := range params {
		key, _, _ := strings.Cut(param, "=")
		if name, err := url.QueryUnescape(key); err != nil || name != "model" {
			continue
		}
		params[i] = key + "=" + url.QueryEscape(base)
	}
	return strings.Join(params, "&")
}

// ensurePathPrefix is the path prefix of POST /ensure/{model}; everything
// after it is the model.
const ensurePathPrefix = "/ensure/"

// stopPathPrefix is the path prefix of POST /stop/{model}; everything after
// it is the instance address.
const stopPathPrefix = "/stop/"

// livePathPrefix is the path prefix of POST /live/{model}; everything after it
// is the model. Unlike the model-routed proxy, this route reads the model from
// the PATH and never buffers the body, so it can carry a full-duplex streaming
// upload (live transcription, issue #266).
const livePathPrefix = "/live/"

// liveChildPath is the audio.cpp child endpoint POST /live/{model} relays to:
// the streaming transcription endpoint, which consumes a chunked audio upload
// and emits SSE transcript events for the life of the connection.
const liveChildPath = "/v1/audio/transcriptions/live"

// ensureResponse is POST /ensure/{model}'s success body.
type ensureResponse struct {
	Status string `json:"status"`
}

// serveEnsure handles POST /ensure/{model}: start the model's managed child
// if it is not running, wait until it is healthy, and answer 200
// {"status":"running"} -- forwarding nothing to the child. It is how the
// gateway loads a child whose server answers no chat completion (an
// images-only model), which the model-routed proxy can only reach by
// forwarding a request that child cannot serve. It is also how the gateway
// adds capacity (#340): an instance address "m#k" ensures that instance,
// creating it when its spec allows it -- the one route that may start a
// burst instance.
//
// Deliberate properties:
//   - The model is the decoded PATH after "/ensure/", so an id containing
//     "/" works (provider.ExpandModelPath on the gateway side escapes each
//     segment and keeps "/"). Any body is read and discarded, never
//     forwarded: net/http notices a client that leaves only once the body
//     has been read to its end, and the request context below depends on
//     that. A body over maxBodyBytes is refused with 413
//     runtime.request_too_large before EnsureRunning, as serveProxy refuses
//     one.
//   - The gateway's call is bodiless on purpose: a router without this
//     route hands the request to serveProxy, which answers a body that names
//     no model with 404 runtime.model_not_managed before EnsureRunning, so
//     an older agent never starts a child for it.
//   - One long call, no heartbeats. EnsureRunning bounds the wait (the
//     spec's startup timeout, plus its admission wait when that is above 0;
//     an admission wait of 0 lasts until the client leaves). A burst instance
//     never waits for admission: it starts into room that is free right now,
//     and the call lasts its startup, or it is refused at once. None of the
//     bundled hops between the gateway and this handler (the gateway's
//     transport, the agent's TLS proxy, this router's server) has an idle
//     timer; a proxy an operator adds in between must allow the whole wait,
//     or it cuts the call. Through the bundled hops a failure therefore stays
//     a genuine HTTP status from sentinelCode: 404 model_not_managed (an
//     unknown model, or an instance address its spec does not allow), 503
//     admission_blocked (a force-stopped spec, or admission refused), 504
//     start_timeout, 502 start_failed or not_permitted, 409
//     instance_no_capacity (a burst instance that does not fit into free
//     room, or one that is draining), 409 instance_demand_unknown (a burst
//     instance whose demand is still unknown -- no declared estimate, and no
//     running sibling measured yet), or 502 upstream_gone for any other
//     failure (e.g. the manager shutting down). servePlainProxy holds a cold
//     start of instance 0 or of a protected instance the same way; it never
//     starts a burst instance, and answers 409 instance_no_capacity instead.
//   - The request context is EnsureRunning's context. A client that leaves
//     while its request is queued drops its waiter, and nothing starts. One
//     that leaves while the child is starting gets an error and so nothing to
//     release; the start still comes up with nothing in flight, and the idle
//     policy applies to it as after any request.
//   - The header ClusterStartHeader with the value "1" makes the call the
//     gateway's cluster start (Manager.EnsureClusterHead), the only call that
//     starts a cold cluster head: it probes the head's member endpoints first
//     and answers 503 runtime.cluster_member_unreachable when one stays
//     unreachable for the head's startup bound, exec'ing nothing. Without the
//     header a cold head is refused with 503 runtime.cluster_not_ready, as a
//     proxied request for it is. For any other model the header changes
//     nothing.
//   - release() runs BEFORE the answer is written, since nothing is proxied:
//     the start counts as one use (LastUsed is stamped) and leaves nothing in
//     flight. The answer is written only while the client is still there.
//     Nothing is persisted: force_running and pinned are untouched.
func (rt *router) serveEnsure(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimPrefix(r.URL.Path, ensurePathPrefix)
	if rt.m == nil || model == "" {
		writeSentinelError(w, ErrModelNotManaged)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		writeBodyReadError(w, err)
		return
	}
	ensure := rt.m.EnsureRunning
	if heads, ok := rt.m.(clusterHeadPort); ok && r.Header.Get(ClusterStartHeader) == "1" {
		ensure = heads.EnsureClusterHead
	}
	_, release, err := ensure(r.Context(), model)
	if err != nil {
		writeSentinelError(w, err)
		return
	}
	release()
	if r.Context().Err() != nil {
		return // the client left as the start succeeded; nobody reads an answer
	}
	refreshWriteDeadline(w)
	writeJSON(w, http.StatusOK, ensureResponse{Status: "running"})
}

// clusterHeadPort is the part of *Manager the cluster start needs, asserted
// per request like stopPort; a manager without it serves a marked ensure as an
// unmarked one.
type clusterHeadPort interface {
	EnsureClusterHead(ctx context.Context, model string) (endpoint string, release func(), err error)
}

// conditionalStopPort is the part of *Manager the conditional stop (POST
// /stop/{model}?if_evictable=1) needs, asserted per request like stopPort.
type conditionalStopPort interface {
	StopIfEvictable(ctx context.Context, model string) (StopState, error)
}

// stopPort is the part of *Manager that POST /stop/{model} needs. It is kept
// apart from managerPort, and asserted per request, so the many managerPort
// fakes of the other routes need not grow a method they never use; a manager
// without it stops nothing and answers as for an unmanaged model.
type stopPort interface {
	Stop(ctx context.Context, model string) (StopState, error)
}

// stopResponse is POST /stop/{model}'s success body.
type stopResponse struct {
	State StopState `json:"state"`
}

// serveStop handles POST /stop/{model}: the transient stop of one instance
// (Manager.Stop). It begins a graceful drain and answers at once -- it does
// not wait for the process to exit; Status() and the agent's telemetry report
// that -- and persists nothing, so demand may start the instance again.
//
// Deliberate properties, shared with serveEnsure:
//   - The model is the decoded PATH after "/stop/", an instance address ("m"
//     or "m#k", the "#" percent-encoded on the wire), so an id containing "/"
//     works. Any body is read and discarded; one over maxBodyBytes is refused
//     with 413 runtime.request_too_large before Stop.
//   - Bodiless on the gateway's side on purpose: a router without this route
//     hands the request to serveProxy, which answers a body that names no
//     model with 404 runtime.model_not_managed, so an older agent stops
//     nothing.
//
// Answers: 200 {"state":"stopping"} when a drain began or was already
// running (also for a force_stopped spec's draining instance), 200
// {"state":"stopped"} when the instance has no process (idempotent), 404
// runtime.model_not_managed for an address that is not managed (also an empty
// model, or no manager), 409 runtime.instance_protected for an instance inside
// the protected floor, 409 runtime.instance_not_highest while a higher
// instance of the spec has a process, or 502 runtime.upstream_gone for any
// other failure, such as the manager shutting down.
//
// With ?if_evictable=1 it is the conditional stop (Manager.StopIfEvictable):
// the same answers, plus 409 runtime.instance_busy when the agent's own
// eviction rule would not let the instance go right now. A manager that cannot
// stop conditionally refuses it the same way rather than stopping
// unconditionally.
func (rt *router) serveStop(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimPrefix(r.URL.Path, stopPathPrefix)
	stopper, ok := rt.m.(stopPort)
	if !ok || model == "" {
		writeSentinelError(w, ErrModelNotManaged)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		writeBodyReadError(w, err)
		return
	}
	stop := stopper.Stop
	if v := r.URL.Query().Get("if_evictable"); v == "1" || v == "true" {
		stop = refuseConditionalStop
		if cond, ok := rt.m.(conditionalStopPort); ok {
			stop = cond.StopIfEvictable
		}
	}
	state, err := stop(r.Context(), model)
	if err != nil {
		writeSentinelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stopResponse{State: state})
}

// refuseConditionalStop answers a conditional stop a manager cannot judge.
func refuseConditionalStop(context.Context, string) (StopState, error) {
	return "", fmt.Errorf("%w: this agent cannot stop conditionally", ErrInstanceBusy)
}

// rpcPathPrefix is the path prefix of the member routes, POST
// /rpc/{action}/{specID}: everything after the action's "/" is the member's
// spec ID.
const rpcPathPrefix = "/rpc/"

// memberPort is the part of *Manager the member routes need, asserted per
// request like stopPort, so the managerPort fakes of the other routes need not
// grow it; a manager without it manages no member.
type memberPort interface {
	EnsureMember(ctx context.Context, specID string, reserve time.Duration) error
	ReleaseMember(ctx context.Context, specID string) error
	StopMember(ctx context.Context, specID string, force bool) (StopState, error)
	PurgeMemberCache(ctx context.Context, specID string) error
}

// memberResponse is the success body of every member route.
type memberResponse struct {
	State string `json:"state"`
}

// codeBadRequest answers a member route whose query cannot be used.
const codeBadRequest = "runtime.bad_request"

// serveRPC handles the member routes, which address an rpc_server member by
// its spec ID -- a member has no upstream model, so the model routes never
// reach it, and these never reach a model spec:
//
//   - POST /rpc/ensure/{specID}?reserve_seconds=N starts the member and
//     answers 200 {"state":"running"} once it accepts TCP connections, with a
//     reservation of N seconds armed (Manager.EnsureMember). One long call like
//     POST /ensure/{model}, with the same refusals: 503 admission_blocked, 502
//     not_permitted or start_failed, 504 start_timeout. N is a whole number
//     from 0 (no reservation, the default) to a day; anything else is 400
//     runtime.bad_request, refused before anything starts.
//   - POST /rpc/release/{specID} ends the member's reservation and hold and
//     answers 200 {"state":"released"} (Manager.ReleaseMember).
//   - POST /rpc/stop/{specID}[?force=1] stops the member with the answers of
//     POST /stop/{model}, and 409 runtime.cluster_member_in_use while it is
//     held or reserved unless forced (Manager.StopMember).
//   - POST /rpc/purge-cache/{specID} removes the member's tensor cache for its
//     head and answers 200 {"state":"purged"}, 409
//     runtime.cluster_member_in_use while it is held or reserved, or 500
//     runtime.cache_purge_failed when the filesystem refuses
//     (Manager.PurgeMemberCache).
//
// An unknown action, an empty or unknown spec ID, a spec that is not a member,
// and a router without a manager all answer 404 runtime.model_not_managed. Any
// body is read and discarded, as on the other control routes.
func (rt *router) serveRPC(w http.ResponseWriter, r *http.Request) {
	action, specID, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, rpcPathPrefix), "/")
	members, ok := rt.m.(memberPort)
	if !ok || specID == "" {
		writeSentinelError(w, ErrModelNotManaged)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		writeBodyReadError(w, err)
		return
	}
	switch action {
	case "ensure":
		rt.serveMemberEnsure(w, r, members, specID)
	case "release":
		rt.answerMember(w, members.ReleaseMember(r.Context(), specID), "released")
	case "stop":
		force := r.URL.Query().Get("force")
		state, err := members.StopMember(r.Context(), specID, force == "1" || force == "true")
		rt.answerMember(w, err, string(state))
	case "purge-cache":
		rt.answerMember(w, members.PurgeMemberCache(r.Context(), specID), "purged")
	default:
		writeSentinelError(w, ErrModelNotManaged)
	}
}

// serveMemberEnsure is POST /rpc/ensure/{specID} (see serveRPC).
func (rt *router) serveMemberEnsure(w http.ResponseWriter, r *http.Request, members memberPort, specID string) {
	reserve, ok := parseReserveSeconds(r.URL.Query().Get("reserve_seconds"))
	if !ok {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"reserve_seconds must be a whole number of seconds from 0 to "+strconv.Itoa(int(maxMemberReservation/time.Second)))
		return
	}
	if err := members.EnsureMember(r.Context(), specID, reserve); err != nil {
		writeSentinelError(w, err)
		return
	}
	if r.Context().Err() != nil {
		return // the client left as the start succeeded; nobody reads an answer
	}
	refreshWriteDeadline(w)
	writeJSON(w, http.StatusOK, memberResponse{State: "running"})
}

// parseReserveSeconds reads reserve_seconds: absent is no reservation, and a
// value must be a whole number of seconds within maxMemberReservation.
func parseReserveSeconds(v string) (time.Duration, bool) {
	if v == "" {
		return 0, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > int(maxMemberReservation/time.Second) {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

// answerMember writes a member route's answer: the refusal err maps to, or 200
// with state.
func (rt *router) answerMember(w http.ResponseWriter, err error, state string) {
	if err != nil {
		writeSentinelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, memberResponse{State: state})
}

// serveLive handles POST /live/{model}: the full-duplex relay to a managed
// audio.cpp child's streaming transcription endpoint (issue #266). It is the
// one proxied route that reads the model from the PATH rather than the body --
// precisely so it never has to buffer the body to route it (serveProxy's
// io.ReadAll), and can instead stream the inbound chunked audio upload straight
// to the child while relaying the child's SSE transcript events back on the
// same connection.
//
// Deliberate properties:
//   - The model is the decoded PATH after "/live/", the same prefix decode
//     serveEnsure does, so an id containing "/" works. An empty model or a nil
//     manager is 404 runtime.model_not_managed, exactly as serveEnsure answers.
//   - It is a plain request, so it is admitted with EnsureServing: an instance
//     address (#340) reaches a burst instance only while one is up, and is
//     refused with 409 runtime.instance_no_capacity otherwise. The address is
//     read again once admitted, and a request whose address moved meanwhile is
//     refused the same way (ensureServing, addressMoved).
//   - release() is deferred AFTER a successful EnsureServing and runs exactly
//     once on every exit path (clean completion, upstream error, client
//     disconnect) -- unlike serveEnsure, which releases immediately because it
//     forwards nothing; here the child admission must be held for the whole
//     relay.
//   - EnableFullDuplex lets the server interleave reading r.Body with writing
//     the SSE response on the one HTTP/1.1 connection. It is enabled at the TOP,
//     before the model guard and EnsureServing, so EVERY response path -- the
//     refusals included -- is full-duplex: net/http otherwise drains up to
//     ~256 KiB of the unread streaming body before writing a refusal, so a
//     refusal written with full-duplex still off would make the client stream
//     ~8 s of PCM before learning it was refused. An error from it (e.g. a test
//     recorder that does not support it) is tolerated by falling through; the
//     production in-process path supports it.
//   - The body is streamed, never buffered: r.Body is handed to the child
//     request directly with ContentLength left unset, so Go's HTTP/1.1
//     transport sends it chunked in a background goroutine and returns the
//     response once the child's headers arrive -- the full-duplex behavior this
//     route needs.
//   - The child URL is {childBase}/v1/audio/transcriptions/live with query =
//     the inbound RawQuery plus model=<decoded path model> (audio.cpp reads the
//     model from its query), forwarding the inbound sample_rate/channels/
//     sample_format/language params unchanged. For an instance address the
//     model in the query is the base name (modelAddress.childModel), the only
//     name the instance's child knows. Any other model is appended exactly as
//     named, since audio.cpp reads the value verbatim -- so a literal model
//     whose name contains '#' reaches the child cut at that '#', which starts
//     the URL's fragment, and no escaping can deliver it to a reader that does
//     not decode.
//   - A RoundTrip failure (child unreachable) is 502 runtime.upstream_gone, the
//     same code the other proxy paths use for a failure reaching an admitted
//     child.
func (rt *router) serveLive(w http.ResponseWriter, r *http.Request) {
	// Interleave reading r.Body with writing the response on the same HTTP/1.1
	// connection. Tolerate an error by falling through (see the doc comment): a
	// writer that does not support it is no worse off than before. This MUST
	// happen before the first write on ANY path -- the refusals below included:
	// net/http, on a non-full-duplex connection, drains up to ~256 KiB of the
	// unread request body before it writes the response, so a refusal written
	// while full-duplex was still off would make the client stream ~8 s of
	// 16 kHz mono s16le PCM into the void before the refusal reached it.
	_ = http.NewResponseController(w).EnableFullDuplex()

	model := strings.TrimPrefix(r.URL.Path, livePathPrefix)
	if rt.m == nil || model == "" {
		writeSentinelError(w, ErrModelNotManaged)
		return
	}
	addr := rt.readAddress(model)
	childModel := model
	if base, ok := addr.childModel(); ok {
		childModel = base
	}
	endpoint, release, err := rt.ensureServing(r.Context(), addr)
	if err != nil {
		writeSentinelError(w, err)
		return
	}
	defer release()

	target := endpoint + liveChildPath + "?" + liveChildQuery(r.URL.RawQuery, childModel)
	// Stream r.Body straight through: do NOT io.ReadAll it, and leave
	// ContentLength unset so the Transport sends it chunked (Request.outgoing
	// Length reports -1 for a non-nil body with no declared length).
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, r.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, codeUpstreamGone, err.Error())
		return
	}
	req.Header = r.Header.Clone()
	for _, h := range hopByHopHeaders {
		req.Header.Del(h)
	}
	req.Header.Del("Content-Length") // the streamed body has no declared length; send it chunked
	// I2: never forward the caller's Accept-Encoding, so the Transport
	// negotiates and transparently decompresses on our behalf and the relayed
	// SSE bytes are the decoded body (see buildUpstreamRequest's own note).
	req.Header.Del("Accept-Encoding")

	resp, err := rt.transport.RoundTrip(req)
	if err != nil {
		// The child is unreachable (or died before answering): the same code
		// the other proxy paths use for a failure reaching an admitted child.
		writeError(w, http.StatusBadGateway, codeUpstreamGone, err.Error())
		return
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close, best-effort

	// Relay status + headers + body back flushed per write, so SSE transcript
	// deltas reach the gateway as the child produces them.
	flusher, _ := w.(http.Flusher)
	forwardUpstreamResponse(w, flusher, resp)
}

// liveChildQuery builds the child's query string for serveLive: the inbound
// params (the forwarded sample_rate/channels/sample_format/language, carried
// through unchanged) with model set to the decoded PATH model, since audio.cpp
// reads the model from its own query. The path model is AUTHORITATIVE -- it is
// what the route was dispatched on and what EnsureServing admitted (its base
// name, for an instance address) -- so a stray inbound model= is overwritten
// rather than appended, which would otherwise leave two model params and let
// Query().Get("model") read the inbound one. (The /upstream relay keeps its
// query as sent instead and replaces only a model parameter's value,
// upstreamChildQuery: a probe's other parameters are the child's business.)
func liveChildQuery(inbound, model string) string {
	values, _ := url.ParseQuery(inbound) // partial parse on error is fine; model is set authoritatively below
	values.Del("model")                  // drop any inbound model= so the authoritative path model is the only one
	base := values.Encode()
	// audio.cpp reads the `model` query value VERBATIM (no URL-decode), so a model id with
	// a ':' must reach it with a LITERAL colon; values.Encode would escape it to %3A and
	// audio.cpp would fail to match ("unknown model id: …%3A…"). A ':' is legal unescaped
	// in a query and a model id carries no query delimiter, so append it literally.
	if base != "" {
		base += "&"
	}
	return base + "model=" + model
}

// writeBodyReadError answers a request whose body, read through a
// maxBodyBytes MaxBytesReader, could not be read: 413
// runtime.request_too_large for a body over the bound, and the sentinel
// mapping of the read error otherwise.
func writeBodyReadError(w http.ResponseWriter, err error) {
	var mbErr *http.MaxBytesError
	if errors.As(err, &mbErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "runtime.request_too_large",
			fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes))
		return
	}
	writeSentinelError(w, fmt.Errorf("runtime: read request body: %w", err))
}

// modelStreamPeek is the minimal shape the router reads out of a proxied
// request body: just enough to route it and decide whether to heartbeat.
// encoding/json fills Model from a top-level key "model" in any letter case.
// The body itself (OpenAI/Anthropic-shaped or otherwise) is forwarded byte for
// byte, with one exception: when Model is an instance address, the value of
// each top-level key read as the model is replaced by the base name in place
// (rewriteModelField), and every other byte still passes through as sent.
type modelStreamPeek struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// serveProxy is the model-routed reverse proxy: every request that is not
// one of the fixed control routes above -- the four GET-only ones (health,
// running, models, and the /upstream/{model}/props probe) and POST
// /ensure/{model}. It buffers the body (bounded), extracts the model (and, for
// JSON, stream), hands the child the base model when the model is an instance
// address (readAddress, then rewriteModelField or rebuildMultipartModel), and
// hands off to the plain or streaming path.
//
// The body is read as a multipart form when the request's Content-Type is
// multipart/form-data (an image edit or an audio transcription) and as JSON
// otherwise; see
// peekMultipartModel. The Content-Type decides the shape, and a request that
// declares the multipart one is never read as JSON, whatever its bytes look like.
// A multipart request is always relayed by servePlainProxy: it has no stream
// notion (the gateway refuses an edit or a transcription that asks to stream), so
// a form field that happens to be named stream is not read at all.
func (rt *router) serveProxy(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeBodyReadError(w, err)
		return
	}

	// A media type that fails to parse for a reason other than its parameters
	// comes back empty, so it falls through to the JSON default exactly as it
	// always has. One that names multipart/form-data but carries a bad
	// parameter (mime.ErrInvalidMediaParameter) still comes back as
	// multipart/form-data, and is refused below rather than read as JSON.
	mediaType, params, mtErr := mime.ParseMediaType(r.Header.Get(headerContentType))
	if mediaType == "multipart/form-data" {
		var model string
		if mtErr == nil {
			model = peekMultipartModel(body, params["boundary"])
		}
		if model == "" {
			writeModelNotNamed(w)
			return
		}
		addr := rt.readAddress(model)
		contentType := ""
		if base, ok := addr.childModel(); ok {
			rebuilt, rebuiltType, err := rebuildMultipartModel(body, params["boundary"], base)
			if err != nil {
				// Not expected: peekMultipartModel has walked this very body to
				// its end. The request cannot be forwarded with the instance
				// address in it, so it is refused rather than relayed as is.
				slog.Error("runtime: rewriting the model of a multipart body failed", "model", model, "error", err)
				writeSentinelError(w, fmt.Errorf("runtime: rewrite the model of a multipart body: %w", err))
				return
			}
			body, contentType = rebuilt, rebuiltType
		}
		rt.servePlainProxy(w, r, addr, body, contentType)
		return
	}

	var peek modelStreamPeek
	if err := json.Unmarshal(body, &peek); err != nil || peek.Model == "" {
		writeModelNotNamed(w)
		return
	}
	addr := rt.readAddress(peek.Model)
	if base, ok := addr.childModel(); ok {
		rewritten, err := rewriteModelField(body, base)
		if err != nil {
			// Not expected: the body has just been decoded, and its model routed
			// on. The request cannot be forwarded with the instance address in
			// it, so it is refused rather than relayed as is -- exactly like a
			// multipart body that cannot be rebuilt.
			slog.Error("runtime: rewriting the model of a JSON body failed", "model", peek.Model, "error", err)
			writeSentinelError(w, fmt.Errorf("runtime: rewrite the model of a JSON body: %w", err))
			return
		}
		body = rewritten
	}

	if peek.Stream {
		rt.serveStreamProxy(w, r, addr, body)
		return
	}
	rt.servePlainProxy(w, r, addr, body, "")
}

// modelAddress is the router's reading of the model a request names, taken by
// the manager's own rule (Manager.ResolveAddress: an exact upstream match wins
// first, so a real model whose name merely contains '#' stays itself) before
// the request is admitted. It decides the name the child receives: an INSTANCE
// ADDRESS, "m#k" naming instance k >= 1 of a managed replicable spec, is handed
// to the child with the base name m instead -- every instance's child is
// launched with the base name (${MODEL} expands to it, specForInstance) and
// knows nothing of "m#k" -- while any other name reaches the child exactly as
// sent.
//
// A name that does not even parse as an instance address can never be one --
// the rule reads it as an exact name or as nothing, instance 0 either way -- so
// it is read without an owner round trip (resolved false): a request for
// instance 0, which is every request of a spec that never replicates, costs the
// owner exactly what it did before instances existed, and nothing about it is
// rewritten or read again.
type modelAddress struct {
	name     string // the model as the request names it
	base     string // ResolveAddress's base upstream model, when resolved
	k        int    // ResolveAddress's instance index, when resolved
	managed  bool   // ResolveAddress's ok, when resolved
	resolved bool   // ResolveAddress was asked: name parses as an instance address
}

// readAddress reads model by the manager's rule; see modelAddress.
func (rt *router) readAddress(model string) modelAddress {
	a := modelAddress{name: model}
	if rt.m == nil {
		return a
	}
	if _, k := ParseInstanceAddress(model); k == 0 {
		return a
	}
	a.base, a.k, a.managed = rt.m.ResolveAddress(model)
	a.resolved = true
	return a
}

// childModel returns the base name the child must see in place of the
// requested name, and whether it must -- only for an instance address k >= 1.
func (a modelAddress) childModel() (base string, ok bool) {
	if !a.resolved || !a.managed || a.k == 0 {
		return "", false
	}
	return a.base, true
}

// addressMoved reports whether a's name reads differently now than when a was
// taken. Reading the address and admitting the request are two owner turns
// (ResolveAddress, then the ensure, which resolves the name again on its own),
// and a config push can land between them: it can make an instance address
// another spec's literal model, turn a literal model containing '#' into an
// instance address of another spec (its own spec removed), or make an unknown
// name an address. The request would then be admitted under one reading and
// carry the name the other one chose -- a child receiving the base name of a
// different spec, or an address it does not know. So once the request is
// admitted the name is read again, and a reading that moved refuses it
// (errAddressMoved). Only a resolved reading can move: a name that does not
// parse as an instance address is instance 0 or nothing under any push.
//
// What is left is a push in that window together with a second push that puts
// the reading back before it is taken again: the request was then admitted
// under the reading in between, which this cannot see. Closing that too would
// take the ensure itself to report the reading it admitted under.
func (rt *router) addressMoved(a modelAddress) bool {
	if !a.resolved {
		return false
	}
	base, k, managed := rt.m.ResolveAddress(a.name)
	return base != a.base || k != a.k || managed != a.managed
}

// errAddressMoved is the refusal of a request whose address moved while it was
// admitted (addressMoved). It is a 409 runtime.instance_no_capacity, like a
// plain request for a burst instance that is not serving: the gateway selects
// again, and its next request reads the address as it is now.
func errAddressMoved(model string) error {
	return fmt.Errorf("runtime: %q names something else than when the request was read; select again: %w", model, ErrInstanceNoCapacity)
}

// ensureServing admits a request the router forwards (EnsureServing) for the
// model a names, and gives the admission back -- release, then errAddressMoved
// -- when the address moved meanwhile (addressMoved).
func (rt *router) ensureServing(ctx context.Context, a modelAddress) (endpoint string, release func(), err error) {
	endpoint, release, err = rt.m.EnsureServing(ctx, a.name)
	if err != nil {
		return "", nil, err
	}
	if rt.addressMoved(a) {
		release()
		return "", nil, errAddressMoved(a.name)
	}
	return endpoint, release, nil
}

// rewriteModelField returns raw with the value of its top-level model replaced
// by model, and every other byte exactly as it was. The new value is spliced
// into the place of the old one, so the keys keep their order, the numbers
// their digits (1.50 stays 1.50, 1e3 stays 1e3), the strings their escapes and
// the body its spacing, and nothing is HTML-escaped: a child that hashes, signs
// or caches a request body sees, for instance k, the bytes instance 0 would
// have seen. (Re-serializing the body would keep every VALUE but reorder the
// keys and rewrite the escapes.)
//
// The value is found by a token walk over the top-level object: the decoder
// hands each value to valueLen, and its InputOffset is the end of that value.
// Every top-level key that encoding/json reads into modelStreamPeek.Model --
// "model" in any letter case, read after unescaping -- whose value is a string
// gets the new value, so whichever of them the router routed on, the instance
// address is never left standing for the child to read. A value that already
// names model keeps its bytes. It is an error for raw not to be a JSON object
// with such a key, which serveProxy has already established by routing on it.
func rewriteModelField(raw []byte, model string) ([]byte, error) {
	literal, err := jsonString(model)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if !nextIsDelim(dec, '{') {
		return nil, errors.New("the body is not a JSON object")
	}
	out := make([]byte, 0, len(raw)+len(literal))
	copied, found := 0, false // copied: raw[:copied] is in out already
	for dec.More() {
		key, start, end, err := nextMember(dec)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(key, "model") || raw[start] != '"' {
			continue
		}
		found = true
		if keepsModelValue(raw[start:end], model) {
			continue
		}
		out = append(append(out, raw[copied:start]...), literal...)
		copied = end
	}
	if !nextIsDelim(dec, '}') {
		return nil, errors.New("the body's top-level object does not end")
	}
	if !found {
		return nil, errors.New("the body has no top-level model string")
	}
	if copied == 0 {
		return raw, nil
	}
	return append(out, raw[copied:]...), nil
}

// nextIsDelim reports whether dec's next token is the delimiter d.
func nextIsDelim(dec *json.Decoder, d json.Delim) bool {
	tok, err := dec.Token()
	return err == nil && tok == d
}

// nextMember reads the next key of the object dec is inside and steps over its
// value, returning the key and where the value's bytes sit in the input
// (start inclusive, end exclusive).
func nextMember(dec *json.Decoder) (key string, start, end int, err error) {
	tok, err := dec.Token()
	if err != nil {
		return "", 0, 0, fmt.Errorf("read a key: %w", err)
	}
	key, _ = tok.(string)
	var n valueLen
	if err := dec.Decode(&n); err != nil {
		return "", 0, 0, fmt.Errorf("read the value of %q: %w", key, err)
	}
	end = int(dec.InputOffset())
	return key, end - int(n), end, nil
}

// keepsModelValue reports whether the string literal value keeps its bytes:
// it already names model, or it does not decode as a string.
func keepsModelValue(value []byte, model string) bool {
	var current string
	err := json.Unmarshal(value, &current)
	return err != nil || current == model
}

// valueLen records the length of the JSON value it is decoded from, and keeps
// none of it: rewriteModelField's token walk steps over every top-level value
// without copying it (a body may carry megabytes of base64 media).
type valueLen int

func (n *valueLen) UnmarshalJSON(b []byte) error {
	*n = valueLen(len(b))
	return nil
}

// jsonString returns s as a JSON string literal, with <, > and & left as they
// are, like the rest of a body rewriteModelField splices it into.
func jsonString(s string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// rebuildMultipartModel returns raw as a new multipart body, and its
// Content-Type (a fresh boundary), with the value of the form's "model" field
// replaced by model -- the gateway's transcription and image-edit rebuilds
// (rebuildTranscriptionBody, rebuildImageEditBody in
// gateway/backend/internal/gateway) on this side of the module boundary,
// reduced to the one edit the router makes. The model field is written anew as
// a plain form field; every other part is copied with its header and its bytes,
// in its place. The parts are read raw (NextRawPart), so not even a part's
// transfer encoding is undone on the way through.
//
// The model part is found the way peekMultipartModel finds it (FormName), and
// raw is a body that function has already walked to its end, so an error here
// is not expected.
func rebuildMultipartModel(raw []byte, boundary, model string) ([]byte, string, error) {
	mr := multipart.NewReader(bytes.NewReader(raw), boundary)
	var out bytes.Buffer
	out.Grow(len(raw) + len(model) + 512)
	mw := multipart.NewWriter(&out)
	for {
		part, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("read part: %w", err)
		}
		if part.FormName() == "model" {
			if err := mw.WriteField("model", model); err != nil {
				return nil, "", fmt.Errorf("write the model field: %w", err)
			}
			continue
		}
		pw, err := mw.CreatePart(part.Header)
		if err != nil {
			return nil, "", fmt.Errorf("create part %q: %w", part.FormName(), err)
		}
		if _, err := io.Copy(pw, part); err != nil {
			return nil, "", fmt.Errorf("copy part %q: %w", part.FormName(), err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", fmt.Errorf("close the multipart writer: %w", err)
	}
	return out.Bytes(), mw.FormDataContentType(), nil
}

// writeModelNotNamed answers a proxied request whose body does not name a
// model to route on: 404 runtime.model_not_managed, for a JSON body and a
// multipart one alike, before anything is started.
func writeModelNotNamed(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, codeModelNotManaged, "request body does not name a managed model")
}

// peekMultipartModel returns the value of the "model" field of a multipart/
// form-data body, or "" when there is none the router will route on: no
// boundary, a malformed body, no model part, an empty model, a model over
// maxMultipartModelBytes, or more than one model part.
//
// SECURITY: this reads the form the way the gateway does and must keep doing
// so. The gateway peeks the model out of the same bytes (parseImageEditForm,
// gateway/backend/internal/gateway/images_edit_handler.go, and parseTranscriptionForm,
// transcription_handler.go in the same package), decides routing and
// the operator's model allowlist on it, and then this router starts a process
// on its own reading. Were the two to read different models out of one body, an
// allowlisted name in the one and an arbitrary managed name in the other would
// bypass the allowlist. So the model is, exactly as there: a part of
// mime/multipart.NewReader over the buffered body whose FormName() is "model" --
// FormName() is "" unless the part's disposition is form-data, so a
// `Content-Disposition: attachment; name="model"` part is not the model, nor is
// anything in the preamble or the epilogue, which the Reader skips. It is NOT a
// scan for the text name="model": do not replace this with one, and do not
// loosen what FormName() decides.
//
// Like the gateway it walks the WHOLE body even once the model is in hand, so a
// repeated model part (two answers to "which model": the gateway refuses it, and
// the router must not pick one of them) and a part the Reader cannot get to (a
// header block it cannot parse, a body that ends inside one; the gateway answers
// "malformed multipart body") are caught here too, instead of routing a body the
// gateway would not have forwarded. Only the model part is read into memory,
// bounded; every other part is skipped by the Reader, so its content is not
// validated here -- the gateway reads the prompt and image parts and refuses a
// body whose own are damaged, and the router relays what the gateway forwarded.
// What the router must and does agree on is the model.
func peekMultipartModel(body []byte, boundary string) string {
	if boundary == "" {
		return ""
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	var model string
	seen := false
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return model
		}
		if err != nil {
			return ""
		}
		if part.FormName() != "model" {
			continue
		}
		if seen {
			return ""
		}
		seen = true
		value, err := io.ReadAll(io.LimitReader(part, maxMultipartModelBytes+1))
		if err != nil || len(value) > maxMultipartModelBytes {
			return ""
		}
		model = string(value)
	}
}

// servePlainProxy handles a non-streaming request: EnsureServing, map any
// pre-forward failure via the sentinel table, otherwise reverse-proxy with
// immediate per-write flushing (never buffer the response -- this holds for
// EVERY proxied response, streaming or not: TestRouterNoResponseBuffering
// exercises exactly this path).
//
// addr is the router's reading of the name the request was routed on (an
// instance address included), body what the child receives -- already
// carrying the base model when that name is an instance address (serveProxy).
// contentType, when not empty, replaces the request's own: a multipart body
// rebuilt with the base model has a boundary of its own.
func (rt *router) servePlainProxy(w http.ResponseWriter, r *http.Request, addr modelAddress, body []byte, contentType string) {
	model := addr.name
	if rt.m == nil {
		writeSentinelError(w, ErrModelNotManaged)
		return
	}
	endpoint, release, err := rt.ensureServing(r.Context(), addr)
	if err != nil {
		writeSentinelError(w, err)
		return
	}
	defer release()

	target, err := url.Parse(endpoint)
	if err != nil {
		// endpoint is always manager-produced ("http://127.0.0.1:<port>");
		// a parse failure here would be an internal bug, not a
		// client-facing condition, but there is no HTTP status left to
		// invent for it beyond the generic upstream-gone mapping.
		writeSentinelError(w, fmt.Errorf("runtime: invalid upstream endpoint %q: %w", endpoint, err))
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = rt.transport
	proxy.FlushInterval = -1 // flush after every write; never buffer a streamed response
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Warn("runtime: router upstream request failed", "model", model, "error", err)
		writeSentinelError(w, fmt.Errorf("runtime: upstream request failed: %w", err))
	}

	outReq := r.Clone(r.Context())
	outReq.Body = io.NopCloser(bytes.NewReader(body))
	outReq.ContentLength = int64(len(body))
	if contentType != "" {
		outReq.Header.Set(headerContentType, contentType)
	}
	// F2, second half: do not OFFER the child a protocol switch. This is not
	// hop-by-hop hygiene -- httputil.ReverseProxy already strips hop-by-hop
	// headers from the outbound request -- it is declining to forward the
	// upgrade: ReverseProxy deliberately re-adds Connection/Upgrade after that
	// strip when the inbound request carried them, so without this an
	// upgrade-shaped client request reaches the child as an upgrade request.
	// The router cannot honour a switch (it buffers request bodies to route on
	// `model`, and deadlineWriter.Hijack refuses), so a child that switched
	// would be abandoned mid-protocol. Also the same posture as the streaming
	// path, which strips both headers via hopByHopHeaders.
	//
	// What the strip does NOT do is stop the raw child connection from leaking
	// (fix round 2, G3 -- this comment previously offered exactly that as one
	// of its benefits, and it is false). EVERY early return in
	// handleUpgradeResponse sits ahead of ReverseProxy's own
	// `defer res.Body.Close()` (reverseproxy.go:605, past the 101 branch at
	// 574-579), so whichever branch a 101 leaves through, the upstream body --
	// i.e. the raw connection to the child -- is never closed. The strip only
	// changes WHICH branch: from Hijack's ErrNotSupported refusal (see
	// deadlineWriter.Hijack above) to the EqualFold mismatch at
	// reverseproxy.go:827-829. A child answering 101 unsolicited still leaks
	// its connection until the child or the OS gives up.
	//
	// The trade is deliberate and it is the right one. What the strip buys is
	// that the router never ASKS for the state it cannot honour: without it an
	// ordinary upgrade-shaped client request (websocket headers plus a JSON
	// body naming a model) made a COMPLIANT child switch protocols and then be
	// abandoned, so the leak was reachable from client traffic; with it, only a
	// child that switches protocols nobody offered can reach it at all. One
	// leaked TCP connection to a local child process, in exchange for never
	// abandoning one mid-protocol.
	//
	// With this in place ReverseProxy can no longer reach a hijack through its
	// 101 path at all, and the two halves of the fix should not be mistaken
	// for each other: a 101 whose body the Transport will hand over as a raw
	// connection ALWAYS carries a non-empty Upgrade token (net/http's
	// isProtocolSwitchHeader), which can no longer match the now-empty
	// requested type, so handleUpgradeResponse exits through ErrorHandler; a
	// 101 without that token exits one branch earlier, on the non-writable
	// body, exactly as it already did before either fix (verified out of
	// tree). deadlineWriter.Hijack's refusal is kept because it is a property
	// of the WRAPPER rather than of this one call site: it holds for any
	// future router path, and it means this Del pair is not the only thing
	// standing between a promoted http.Hijacker and a pinned release().
	outReq.Header.Del("Connection")
	outReq.Header.Del("Upgrade")
	// I5: wrap w so a downstream client that stops reading cannot pin
	// ReverseProxy's own internal body-copy goroutine (and the deferred
	// release() above with it) indefinitely -- the plain-path counterpart
	// of the streaming path's flushWriter doing the same for its own copy
	// loop.
	proxy.ServeHTTP(deadlineWriter{w}, outReq)
}

// serveStreamProxy handles a "stream":true request with a LAZY commit (see
// the package doc's COLD-START HEARTBEATS section): nothing is written to
// w until either a heartbeat actually fires or real data is actually ready
// to write. sseCommit.commit is the single idempotent function that sends
// 200 + SSE headers the first time either of those happens; every error path
// checks committed via sseCommit.fail instead of always emitting an SSE frame,
// so a failure fast enough to beat the first heartbeat -- model not
// managed, a policy refusal, a synchronously failing exec, an immediate
// non-2xx from an already-admitted child -- is still a genuine HTTP status,
// exactly like the non-streaming path.
//
// addr and body are servePlainProxy's. The address is read again once the
// request is admitted (ensureServing) inside the ensure phase, so a heartbeat
// keeps covering that owner round trip too.
func (rt *router) serveStreamProxy(w http.ResponseWriter, r *http.Request, addr modelAddress, body []byte) {
	flusher, _ := w.(http.Flusher)
	s := &sseCommit{w: w, flusher: flusher}

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	if rt.m == nil {
		s.fail(ErrModelNotManaged)
		return
	}

	type ensureResult struct {
		endpoint string
		release  func()
		err      error
	}
	ensureCh := make(chan ensureResult, 1)
	go func() {
		endpoint, release, err := rt.ensureServing(r.Context(), addr)
		ensureCh <- ensureResult{endpoint: endpoint, release: release, err: err}
	}()
	eres := heartbeatWait(ticker, ensureCh, w, flusher, s.commit)
	if eres.err != nil {
		s.fail(eres.err)
		return
	}
	defer eres.release()

	resp := rt.openUpstreamStream(r, ticker, s, eres.endpoint, body)
	if resp == nil {
		return
	}
	defer resp.Body.Close()
	spliceUpstreamStream(r.Context(), ticker, s, resp)
}

// sseCommit is serveStreamProxy's lazy commit: nothing is written to w until
// commit is first called, and fail picks the error envelope by whether that
// has happened yet.
type sseCommit struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	committed bool
}

// commit is the single idempotent function that sends 200 + SSE headers the
// first time a heartbeat fires or real data is ready to write.
func (s *sseCommit) commit() {
	if s.committed {
		return
	}
	s.committed = true
	refreshWriteDeadline(s.w) // I5: about to write headers -- start the deadline before, not after
	s.w.Header().Set(headerContentType, "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.Header().Set("Connection", "keep-alive")
	s.w.WriteHeader(http.StatusOK)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// fail reports err as a genuine HTTP status if nothing has been committed yet
// (sentinelCode via the same envelope the non-streaming path uses), or as a
// terminal SSE frame if 200 was already sent -- the ACCEPTED TRADE-OFF the
// package doc describes, now paid only when a heartbeat or real data actually
// forced the commit.
func (s *sseCommit) fail(err error) {
	if !s.committed {
		writeSentinelError(s.w, err)
		return
	}
	writeSSEError(s.w, s.flusher, err)
}

// openUpstreamStream sends r's upstream request to endpoint with heartbeats
// ticking, and returns the child's 2xx response for the caller to splice (and
// close). It returns nil once it has answered the client itself: the request
// could not be built or sent, or the child answered with a non-2xx.
func (rt *router) openUpstreamStream(r *http.Request, ticker *time.Ticker, s *sseCommit, endpoint string, body []byte) *http.Response {
	outReq, err := rt.buildUpstreamRequest(r, endpoint, body)
	if err != nil {
		s.fail(fmt.Errorf("runtime: build upstream request: %w", err))
		return nil
	}

	type roundTripResult struct {
		resp *http.Response
		err  error
	}
	rtCh := make(chan roundTripResult, 1)
	go func() {
		resp, err := rt.transport.RoundTrip(outReq)
		rtCh <- roundTripResult{resp: resp, err: err}
	}()
	rres := heartbeatWait(ticker, rtCh, s.w, s.flusher, s.commit)
	if rres.err != nil {
		s.fail(fmt.Errorf("runtime: upstream request failed: %w", rres.err))
		return nil
	}
	resp := rres.resp
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		// C1: an already-admitted child answering with its own non-2xx
		// (e.g. 400 "context length exceeded") is a legitimate response,
		// not a router-level failure. If nothing has been committed to
		// SSE yet, forward it exactly as the plain (non-streaming) path
		// would for the identical response -- status, headers, and body
		// verbatim -- so a streaming caller sees the same diagnostics a
		// non-streaming caller gets. Only once 200 is ALREADY committed
		// (a heartbeat fired, or real data already flowed) does a non-2xx
		// become the ACCEPTED TRADE-OFF terminal frame instead: the
		// brief prescribes that treatment for a non-2xx "after heartbeats
		// began", not unconditionally, which the lazy commit exists to
		// distinguish.
		if !s.committed {
			forwardUpstreamResponse(s.w, s.flusher, resp)
			return nil
		}
		s.fail(fmt.Errorf("runtime: upstream returned status %d", resp.StatusCode))
		return nil
	}
	return resp
}

// spliceUpstreamStream copies the child's 2xx streaming response to the
// client.
func spliceUpstreamStream(ctx context.Context, ticker *time.Ticker, s *sseCommit, resp *http.Response) {
	fw := flushWriter{w: s.w, f: s.flusher}

	// Wait for the first body byte with heartbeats still ticking (a warm
	// child's time-to-first-token is the same silent-window class as a cold
	// start, design doc §8); once it arrives, heartbeats stop for good and
	// the rest is spliced through verbatim -- never re-framed.
	type readResult struct {
		n   int
		err error
	}
	buf := make([]byte, 32*1024)
	firstCh := make(chan readResult, 1)
	go func() {
		n, err := resp.Body.Read(buf)
		firstCh <- readResult{n: n, err: err}
	}()
	fres := heartbeatWaitCtx(ctx, ticker, firstCh, s.w, s.flusher, resp.Body, s.commit)
	if fres.n > 0 {
		// Real data is ready: commit now if a heartbeat never happened to
		// fire first (the common warm-model case) -- commit is idempotent,
		// so this is a no-op if it already fired. Written through fw (not
		// a direct w.Write) so this first chunk gets the same I5 write-
		// deadline refresh as every later one.
		s.commit()
		if _, werr := fw.Write(buf[:fres.n]); werr != nil {
			return // client gone; nothing further to do
		}
	}
	if fres.err != nil {
		if errors.Is(fres.err, io.EOF) {
			// A genuinely empty but successful stream: still commit
			// explicitly (rather than relying on net/http's implicit
			// default-200-on-return) since this path IS a successful
			// outcome of a stream request.
			s.commit()
			return
		}
		// M7: heartbeatWaitCtx force-closed resp.Body because r.Context()
		// fired (the client disconnected) -- that produces a non-EOF read
		// error here too, but it must be reported the same way
		// spliceWithCancel already reports the identical event for the
		// LATER read (as no error: there is no one left to report an
		// error TO), not as a s.fail(runtime.upstream_gone) call that
		// would otherwise try to write a real HTTP status to a connection
		// that is already gone.
		if ctx.Err() != nil {
			return
		}
		s.fail(fmt.Errorf("runtime: upstream body read failed: %w", fres.err))
		return
	}

	s.commit() // defensive: reachable only if Read legally returned (0, nil), which committing again is a no-op for
	if err := spliceWithCancel(ctx, fw, resp.Body); err != nil {
		// The child died mid-response after already sending real bytes: no
		// HTTP status is available any more (spent long ago), but an SSE
		// client can still parse one more terminal frame appended after
		// legitimate data -- design doc §6.4 "child dies mid-request".
		s.fail(fmt.Errorf("runtime: upstream body read failed: %w", err))
	}
}

// heartbeatWait blocks until resultCh yields a value, calling commit() (see
// serveStreamProxy) and writing a `: keepalive\n\n` SSE comment on every
// tick of ticker in the meantime. ticker is shared across every phase of
// one streaming request (EnsureServing, the upstream round trip, the wait
// for the first body byte) so the interval keeps counting continuously
// across phase boundaries instead of resetting at each one. commit is
// idempotent, so calling it from every phase's heartbeatWait/heartbeatWaitCtx
// call is safe regardless of which phase's ticker fire (if any) actually
// commits first.
//
// Used only for phases where abandoning the wait early is NOT safe: in
// particular the ensure phase, where a client disconnecting cannot
// simply be treated as "done" -- if EnsureServing goes on to succeed anyway
// (a narrow but real race between admission completing and the caller's ctx
// firing, which the ensure itself resolves cleanly either way), the
// resulting release() MUST still be called or the spec becomes permanently
// un-evictable (EnsureRunning's own doc comment). See heartbeatWaitCtx for
// the phases where early abandonment is safe.
func heartbeatWait[T any](ticker *time.Ticker, resultCh <-chan T, w http.ResponseWriter, flusher http.Flusher, commit func()) T {
	for {
		select {
		case v := <-resultCh:
			return v
		case <-ticker.C:
			commit()
			writeHeartbeat(w, flusher)
		}
	}
}

// heartbeatWaitCtx is heartbeatWait's cancellation-aware sibling, for the
// phases where the producer feeding resultCh is blocked on body reading a
// live *http.Response and abandoning the wait early IS safe: closing body
// unblocks the producer's pending Read with an error, resultCh still always
// yields a value (the producer is never abandoned/leaked), and there is no
// release() at stake here (that already happened, or never will, in the
// ensure phase above). This exists because relying on ctx
// cancellation to propagate all the way through an *already-returned*
// http.Transport.RoundTrip's response-body reads turned out, empirically
// (TestRouterReleaseCalledExactlyOnce/client_disconnect_midstream), not to
// be prompt enough on its own -- explicitly closing body on ctx.Done() is
// the reliable version of the same idea.
func heartbeatWaitCtx[T any](ctx context.Context, ticker *time.Ticker, resultCh <-chan T, w http.ResponseWriter, flusher http.Flusher, body io.Closer, commit func()) T {
	for {
		select {
		case v := <-resultCh:
			return v
		case <-ticker.C:
			commit()
			writeHeartbeat(w, flusher)
		case <-ctx.Done():
			body.Close() //nolint:errcheck // best-effort: only unblocking a pending Read, not reporting an outcome
			return <-resultCh
		}
	}
}

// spliceWithCancel copies src to dst until EOF, an error, or ctx firing --
// in which case src is closed to unblock the copy promptly rather than
// leaving it to whatever latency the underlying transport's own ctx
// awareness happens to have (see heartbeatWaitCtx). A client disconnect
// (ctx firing) is reported as no error: there is no one left to report an
// error TO, and it is not an upstream failure.
func spliceWithCancel(ctx context.Context, dst io.Writer, src io.ReadCloser) error {
	copyDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(dst, src)
		copyDone <- err
	}()
	select {
	case err := <-copyDone:
		return err
	case <-ctx.Done():
		src.Close() //nolint:errcheck // best-effort: only unblocking the pending Read
		<-copyDone  //nolint:errcheck // drain so the goroutine above is never leaked; its error is not an upstream failure
		return nil
	}
}

func writeHeartbeat(w http.ResponseWriter, flusher http.Flusher) {
	refreshWriteDeadline(w) // I5
	if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
		return // client gone; the next resultCh receive will still resolve and release() will still run
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// writeSSEError emits the terminal SSE error frame described in the package
// doc's ACCEPTED TRADE-OFF section: the same stable §6.5 code the
// non-streaming path would have used for err, carried in-stream since the
// HTTP status was already committed.
//
// It arms its own write deadline (I5) instead of relying on the caller
// having just refreshed one. Today every reachable call site does happen to
// leave a fresh deadline behind -- writeSSEError only runs once committed is
// true, and commit() refreshes before writing the headers -- but that is a
// coincidence of the current control flow, not a property of this function.
// A refactor that decouples the commit from the failure path (or one that
// lets a long silent phase elapse between them) would reintroduce exactly
// the unbounded hang the deadline exists to prevent, on the very last write
// of a request: a stalled reader would pin this goroutine and the deferred
// release() with it, making the spec un-evictable. One line here removes
// that dependency permanently.
func writeSSEError(w http.ResponseWriter, flusher http.Flusher, err error) {
	code, _ := sentinelCode(err)
	payload, marshalErr := json.Marshal(errorEnvelope{Error: errorBody{Code: code, Message: err.Error()}})
	if marshalErr != nil {
		return // cannot happen for this static shape; nothing sensible to do if it somehow did
	}
	refreshWriteDeadline(w) // I5: about to write; never inherit whatever deadline the caller happened to leave
	if _, werr := fmt.Fprintf(w, "data: %s\n\n", payload); werr != nil {
		return
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// buildUpstreamRequest constructs the outbound request for the streaming
// path (the plain path delegates this to httputil.ReverseProxy instead):
// method, path, and query preserved verbatim, hop-by-hop headers stripped,
// body replaced with the already-buffered bytes.
func (rt *router) buildUpstreamRequest(r *http.Request, endpoint string, body []byte) (*http.Request, error) {
	target := endpoint + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = r.Header.Clone()
	for _, h := range hopByHopHeaders {
		req.Header.Del(h)
	}
	req.Header.Del("Content-Length") // req.ContentLength (set below) is authoritative for the buffered body
	// I2: forwarding the caller's own Accept-Encoding lets a compressing
	// child return a gzipped body that http.Transport does NOT
	// transparently decompress (it only does that for a request where IT
	// added the header itself). A 2xx response then gets spliced straight
	// through as compressed bytes under the router's own fixed
	// Content-Type: text/event-stream, with no matching Content-Encoding
	// of its own to explain them -- silently corrupt SSE output. Deleting
	// it lets the Transport request and decompress on our behalf, the same
	// way every other stdlib HTTP call in this codebase already works by
	// never setting it itself.
	req.Header.Del("Accept-Encoding")
	req.ContentLength = int64(len(body))
	return req, nil
}

// forwardUpstreamResponse copies resp's status, headers (minus hop-by-hop),
// and body verbatim to w -- used when nothing has been committed to SSE
// yet and the upstream response should be forwarded exactly as the plain
// (non-streaming) path's httputil.ReverseProxy would for the identical
// response (C1): a non-2xx status from an already-admitted child is the
// model server's own legitimate answer (e.g. 400 "context length
// exceeded"), not a router-level failure, and a streaming caller must see
// the same status/headers/body a non-streaming caller would. Never called
// once committed is true -- headers are already sent by then, and a
// non-2xx from that point on is the ACCEPTED TRADE-OFF terminal frame
// instead (see package doc).
func forwardUpstreamResponse(w http.ResponseWriter, flusher http.Flusher, resp *http.Response) {
	dst := w.Header()
	for k, vv := range resp.Header {
		if isHopByHopHeader(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	refreshWriteDeadline(w) // I5
	w.WriteHeader(resp.StatusCode)
	fw := flushWriter{w: w, f: flusher}
	if _, err := io.Copy(fw, resp.Body); err != nil {
		slog.Warn("runtime: router failed to forward upstream body", "status", resp.StatusCode, "error", err)
	}
}

func isHopByHopHeader(key string) bool {
	for _, h := range hopByHopHeaders {
		if key == h {
			return true
		}
	}
	return false
}

// flushWriter flushes the underlying ResponseWriter after every Write, so
// io.Copy never accumulates bytes the client hasn't seen yet.
type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	refreshWriteDeadline(fw.w) // I5
	n, err := fw.w.Write(p)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

// errorEnvelope/errorBody reproduce the gateway's HTTP error envelope by
// hand: this module imports nothing from the gateway (op-ai-server-agent is
// a standalone module), so the shape is duplicated deliberately -- the same
// choice internal/client/ws.go already makes for the WebSocket frame
// envelope (see streamFrame there).
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Stable wire codes duplicated-by-string often enough that they live as
// constants; the full code table is sentinelCode below and design doc par.4.3.
const (
	codeModelNotManaged = "runtime.model_not_managed"
	codeUpstreamGone    = "runtime.upstream_gone"
)

// sentinelCode maps a Manager error to the design doc §6.5 stable wire code
// and HTTP status. Any error that is not one of the named sentinels
// (including a raw upstream connection failure or non-2xx status, which
// carry no Manager sentinel of their own) falls through to
// runtime.upstream_gone/502 -- the same code the design doc assigns to "the
// child died during the request", which is the closest fit for "something
// went wrong reaching or using an otherwise-admitted child".
//
// The two instance refusals (#340) are 409s: the request conflicts with what
// this host can serve under that address right now, while another host, or
// another instance, may well serve it -- the gateway's cue to select again,
// never an error to surface. runtime.instance_no_capacity answers an ensure of
// a burst instance that does not fit into free room or is draining, a request
// the router forwards for a burst instance that is not serving (none running
// or starting), and a request whose address moved while it was admitted
// (errAddressMoved); runtime.instance_demand_unknown answers an ensure of a
// burst instance whose demand is not known yet, which the gateway retries
// shortly, by which time a sibling has usually been measured. The stop
// refusals (POST /stop/{model}, POST /rpc/stop/{specID}) are 409s as well: the stop conflicts with the
// spec's protected floor (runtime.instance_protected) or with the ordering of
// its instances (runtime.instance_not_highest), and the operator resolves either
// by changing the spec or stopping the higher instance first. A member's
// (runtime.cluster_member_in_use) conflicts with the head holding it: the
// cluster is stopped head first. runtime.cache_purge_failed (500) is the agent's
// own filesystem refusing to remove a member cache. The cluster refusals are
// 503s, "not now, the gateway orchestrates and retries": a cold cluster head
// asked for without the cluster start (runtime.cluster_not_ready), and a
// cluster start whose member probe gave up (runtime.cluster_member_unreachable).
// A conditional stop the eviction rule refuses is a 409
// (runtime.instance_busy): the stop conflicts with the instance's use.
func sentinelCode(err error) (code string, status int) {
	switch {
	case errors.Is(err, ErrModelNotManaged):
		return codeModelNotManaged, http.StatusNotFound
	case errors.Is(err, ErrStartFailed):
		return "runtime.start_failed", http.StatusBadGateway
	case errors.Is(err, ErrStartTimeout):
		return "runtime.start_timeout", http.StatusGatewayTimeout
	case errors.Is(err, ErrAdmissionBlocked):
		return "runtime.admission_blocked", http.StatusServiceUnavailable
	case errors.Is(err, ErrNotPermitted):
		return "runtime.not_permitted", http.StatusBadGateway
	case errors.Is(err, ErrInstanceNoCapacity):
		return "runtime.instance_no_capacity", http.StatusConflict
	case errors.Is(err, ErrInstanceDemandUnknown):
		return "runtime.instance_demand_unknown", http.StatusConflict
	case errors.Is(err, ErrInstanceProtected):
		return "runtime.instance_protected", http.StatusConflict
	case errors.Is(err, ErrInstanceNotHighest):
		return "runtime.instance_not_highest", http.StatusConflict
	case errors.Is(err, ErrMemberInUse):
		return "runtime.cluster_member_in_use", http.StatusConflict
	case errors.Is(err, ErrCachePurgeFailed):
		return "runtime.cache_purge_failed", http.StatusInternalServerError
	case errors.Is(err, ErrClusterNotReady):
		return "runtime.cluster_not_ready", http.StatusServiceUnavailable
	case errors.Is(err, ErrClusterMemberUnreachable):
		return codeClusterMemberUnreach, http.StatusServiceUnavailable
	case errors.Is(err, ErrInstanceBusy):
		return "runtime.instance_busy", http.StatusConflict
	default:
		return codeUpstreamGone, http.StatusBadGateway
	}
}

// writeSentinelError writes err's sentinelCode mapping as the JSON error
// envelope -- the shared implementation behind every pre-forward failure in
// the non-streaming path.
func writeSentinelError(w http.ResponseWriter, err error) {
	code, status := sentinelCode(err)
	writeError(w, status, code, err.Error())
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: errorBody{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set(headerContentType, "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) //nolint:errcheck // best-effort: the client may already be gone
}
