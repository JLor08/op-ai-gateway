// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// This file is the router port (design doc §6.1): the single HTTP port the
// gateway talks to for a server_agent application, which routes every
// inference request to the right managed model process, starting it first
// if necessary. Three route classes:
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
//   - everything else -- the model-routed reverse proxy.
//
// STREAMING ASYMMETRY (the whole point of this design): request bodies are
// buffered (bounded by maxBodyBytes) because `model`/`stream` must be read
// out of the JSON before the router knows where to send it. Responses are
// NEVER buffered -- httputil.ReverseProxy with FlushInterval:-1 for the
// plain path, and a hand-rolled splice-with-heartbeat loop for the streaming
// path, both flush after every write so SSE tokens (and any other streamed
// bytes) reach the client as they are produced.
//
// COLD-START HEARTBEATS, LAZILY COMMITTED (design doc §8.3, brief: "once
// admission succeeds, commit 200" -- after, not before): a request with
// "stream":true does NOT commit 200 + SSE headers up front. It arms a
// heartbeatInterval ticker and starts waiting on EnsureRunning (then, on
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
// (EnsureRunning pending) and a time-to-first-token wait on an
// already-warm child (the upstream request pending), which the design doc
// treats as the same "silent window" failure class. Once real bytes start
// flowing, heartbeats stop for good and the upstream body is spliced
// through verbatim -- the router never re-frames the child's own SSE
// lines.
//
// ACCEPTED TRADE-OFF, deliberately implemented and documented here (design
// doc §8.3, §15): once 200 HAS been committed for a streaming request (a
// heartbeat fired, or real data started flowing, before the eventual
// outcome was known), no later failure -- EnsureRunning erroring, the
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
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// maxBodyBytes bounds the buffered read of an inbound request body (needed
// to extract `model`/`stream` before the destination is known). A request
// body beyond this is rejected with 413 before any admission decision is
// even attempted.
const maxBodyBytes = 32 << 20

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
type managerPort interface {
	EnsureRunning(ctx context.Context, upstreamModel string) (endpoint string, release func(), err error)
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
}

func newRouter(m managerPort) *router {
	return &router{
		m:         m,
		transport: &http.Transport{},
	}
}

func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// M9: the brief specifies GET for all three control paths; a
	// different method on one of these exact paths falls through to the
	// model-routed proxy instead (the ordinary "everything else" case),
	// rather than getting the always-200 treatment regardless of method.
	isGet := r.Method == http.MethodGet
	switch {
	case isGet && (r.URL.Path == "/health" || r.URL.Path == "/v1/health"):
		rt.serveHealth(w, r)
	case isGet && r.URL.Path == "/running":
		rt.serveRunning(w, r)
	case isGet && r.URL.Path == "/v1/models":
		rt.serveModels(w, r)
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
// working unchanged against a server_agent application.
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
// and model listing both read this endpoint expecting the full set.
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
		data = append(data, modelEntry{ID: st.Model, Object: "model"})
	}
	writeJSON(w, http.StatusOK, modelsResponse{Object: "list", Data: data})
}

// modelStreamPeek is the minimal shape the router reads out of a proxied
// request body: just enough to route it and decide whether to heartbeat.
// Every other field of the real request (OpenAI/Anthropic-shaped or
// otherwise) passes through untouched, byte for byte.
type modelStreamPeek struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// serveProxy is the model-routed reverse proxy: every request that is not
// one of the three fixed control paths above. It buffers the body (bounded),
// extracts model/stream, and hands off to the plain or streaming path.
func (rt *router) serveProxy(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "runtime.request_too_large",
				fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes))
			return
		}
		writeSentinelError(w, fmt.Errorf("runtime: read request body: %w", err))
		return
	}

	var peek modelStreamPeek
	if err := json.Unmarshal(body, &peek); err != nil || peek.Model == "" {
		writeError(w, http.StatusNotFound, "runtime.model_not_managed", "request body does not name a managed model")
		return
	}

	if peek.Stream {
		rt.serveStreamProxy(w, r, peek.Model, body)
		return
	}
	rt.servePlainProxy(w, r, peek.Model, body)
}

// servePlainProxy handles a non-streaming request: EnsureRunning, map any
// pre-forward failure via the sentinel table, otherwise reverse-proxy with
// immediate per-write flushing (never buffer the response -- this holds for
// EVERY proxied response, streaming or not: TestRouterNoResponseBuffering
// exercises exactly this path).
func (rt *router) servePlainProxy(w http.ResponseWriter, r *http.Request, model string, body []byte) {
	if rt.m == nil {
		writeSentinelError(w, ErrModelNotManaged)
		return
	}
	endpoint, release, err := rt.m.EnsureRunning(r.Context(), model)
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
	// F2, second half: do not OFFER the child a protocol switch. This is not
	// hop-by-hop hygiene -- httputil.ReverseProxy already strips hop-by-hop
	// headers from the outbound request -- it is declining to forward the
	// upgrade: ReverseProxy deliberately re-adds Connection/Upgrade after that
	// strip when the inbound request carried them, so without this an
	// upgrade-shaped client request reaches the child as an upgrade request.
	// The router cannot honour a switch (it buffers request bodies to route on
	// `model`, and deadlineWriter.Hijack refuses), so a child that switched
	// would be abandoned mid-protocol -- and on ReverseProxy's non-Hijacker
	// path the upstream 101's body, i.e. the raw child connection, is never
	// closed, leaking it until the child or the OS gives up. Not offering the
	// switch means no child ever gets into that state. It does NOT replace
	// deadlineWriter.Hijack's refusal: a 101 no client asked for still reaches
	// handleUpgradeResponse, and one whose Upgrade token is absent matches the
	// (now empty) requested type, so the refusal is what stops the hijack
	// there. Also the same posture as the streaming path, which strips both
	// headers via hopByHopHeaders.
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
// to write. commit() is the single idempotent function that sends 200 + SSE
// headers the first time either of those happens; every error path below
// checks committed via failStream instead of always emitting an SSE frame,
// so a failure fast enough to beat the first heartbeat -- model not
// managed, a policy refusal, a synchronously failing exec, an immediate
// non-2xx from an already-admitted child -- is still a genuine HTTP status,
// exactly like the non-streaming path.
func (rt *router) serveStreamProxy(w http.ResponseWriter, r *http.Request, model string, body []byte) {
	flusher, _ := w.(http.Flusher)
	fw := flushWriter{w: w, f: flusher}

	committed := false
	commit := func() {
		if committed {
			return
		}
		committed = true
		refreshWriteDeadline(w) // I5: about to write headers -- start the deadline before, not after
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		if flusher != nil {
			flusher.Flush()
		}
	}
	// failStream reports err as a genuine HTTP status if nothing has been
	// committed yet (sentinelCode via the same envelope the non-streaming
	// path uses), or as a terminal SSE frame if 200 was already sent --
	// the ACCEPTED TRADE-OFF the package doc describes, now paid only when
	// a heartbeat or real data actually forced the commit.
	failStream := func(err error) {
		if !committed {
			writeSentinelError(w, err)
			return
		}
		writeSSEError(w, flusher, err)
	}

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	if rt.m == nil {
		failStream(ErrModelNotManaged)
		return
	}

	type ensureResult struct {
		endpoint string
		release  func()
		err      error
	}
	ensureCh := make(chan ensureResult, 1)
	go func() {
		endpoint, release, err := rt.m.EnsureRunning(r.Context(), model)
		ensureCh <- ensureResult{endpoint: endpoint, release: release, err: err}
	}()
	eres := heartbeatWait(ticker, ensureCh, w, flusher, commit)
	if eres.err != nil {
		failStream(eres.err)
		return
	}
	defer eres.release()

	outReq, err := rt.buildUpstreamRequest(r, eres.endpoint, body)
	if err != nil {
		failStream(fmt.Errorf("runtime: build upstream request: %w", err))
		return
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
	rres := heartbeatWait(ticker, rtCh, w, flusher, commit)
	if rres.err != nil {
		failStream(fmt.Errorf("runtime: upstream request failed: %w", rres.err))
		return
	}
	resp := rres.resp
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
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
		if !committed {
			forwardUpstreamResponse(w, flusher, resp)
			return
		}
		failStream(fmt.Errorf("runtime: upstream returned status %d", resp.StatusCode))
		return
	}

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
	fres := heartbeatWaitCtx(r.Context(), ticker, firstCh, w, flusher, resp.Body, commit)
	if fres.n > 0 {
		// Real data is ready: commit now if a heartbeat never happened to
		// fire first (the common warm-model case) -- commit is idempotent,
		// so this is a no-op if it already fired. Written through fw (not
		// a direct w.Write) so this first chunk gets the same I5 write-
		// deadline refresh as every later one.
		commit()
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
			commit()
			return
		}
		// M7: heartbeatWaitCtx force-closed resp.Body because r.Context()
		// fired (the client disconnected) -- that produces a non-EOF read
		// error here too, but it must be reported the same way
		// spliceWithCancel already reports the identical event for the
		// LATER read (as no error: there is no one left to report an
		// error TO), not as a failStream(runtime.upstream_gone) call that
		// would otherwise try to write a real HTTP status to a connection
		// that is already gone.
		if r.Context().Err() != nil {
			return
		}
		failStream(fmt.Errorf("runtime: upstream body read failed: %w", fres.err))
		return
	}

	commit() // defensive: reachable only if Read legally returned (0, nil), which committing again is a no-op for
	if err := spliceWithCancel(r.Context(), fw, resp.Body); err != nil {
		// The child died mid-response after already sending real bytes: no
		// HTTP status is available any more (spent long ago), but an SSE
		// client can still parse one more terminal frame appended after
		// legitimate data -- design doc §6.4 "child dies mid-request".
		failStream(fmt.Errorf("runtime: upstream body read failed: %w", err))
	}
}

// heartbeatWait blocks until resultCh yields a value, calling commit() (see
// serveStreamProxy) and writing a `: keepalive\n\n` SSE comment on every
// tick of ticker in the meantime. ticker is shared across every phase of
// one streaming request (EnsureRunning, the upstream round trip, the wait
// for the first body byte) so the interval keeps counting continuously
// across phase boundaries instead of resetting at each one. commit is
// idempotent, so calling it from every phase's heartbeatWait/heartbeatWaitCtx
// call is safe regardless of which phase's ticker fire (if any) actually
// commits first.
//
// Used only for phases where abandoning the wait early is NOT safe: in
// particular the EnsureRunning phase, where a client disconnecting cannot
// simply be treated as "done" -- if EnsureRunning goes on to succeed anyway
// (a narrow but real race between admission completing and the caller's ctx
// firing, which EnsureRunning itself resolves cleanly either way), the
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
// EnsureRunning phase above). This exists because relying on ctx
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
		<-copyDone  // drain so the goroutine above is never leaked
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

// sentinelCode maps a Manager error to the design doc §6.5 stable wire code
// and HTTP status. Any error that is not one of the five named sentinels
// (including a raw upstream connection failure or non-2xx status, which
// carry no Manager sentinel of their own) falls through to
// runtime.upstream_gone/502 -- the same code the design doc assigns to "the
// child died during the request", which is the closest fit for "something
// went wrong reaching or using an otherwise-admitted child".
func sentinelCode(err error) (code string, status int) {
	switch {
	case errors.Is(err, ErrModelNotManaged):
		return "runtime.model_not_managed", http.StatusNotFound
	case errors.Is(err, ErrStartFailed):
		return "runtime.start_failed", http.StatusBadGateway
	case errors.Is(err, ErrStartTimeout):
		return "runtime.start_timeout", http.StatusGatewayTimeout
	case errors.Is(err, ErrAdmissionBlocked):
		return "runtime.admission_blocked", http.StatusServiceUnavailable
	case errors.Is(err, ErrNotPermitted):
		return "runtime.not_permitted", http.StatusBadGateway
	default:
		return "runtime.upstream_gone", http.StatusBadGateway
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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) //nolint:errcheck // best-effort: the client may already be gone
}
