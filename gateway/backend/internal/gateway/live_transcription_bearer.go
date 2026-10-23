// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// liveBearerBusyCode is the wire code a bearer live-transcription request is refused with
// when the route already has an active session. It is API surface: a programmatic client
// branches on it to back off. 503 (not 409) because it is transient -- the slot frees when
// the active session ends -- and it is the HTTP twin of the WebSocket path's
// StatusTryAgainLater busy close.
const liveBearerBusyCode = "transcription.live_busy"

// handleBearerLiveTranscription serves POST /v1/audio/transcriptions/live (and its
// /openai alias): a bearer-authenticated, chunked full-duplex HTTP session that mirrors
// audio.cpp's own native /v1/audio/transcriptions/live, so a programmatic (non-browser)
// client can treat the gateway as a drop-in. It is the bearer HTTP twin of the browser's
// WebSocket transport (handleLiveTranscription): same relay/billing core, a different
// transport.
//
// The request body is raw interleaved PCM streamed chunked; the parameters ride in the
// QUERY string (model, sample_rate, channels, sample_format, language); the response is the
// upstream's own SSE (transcript.text.delta -> transcript.text.done -> [DONE]) relayed
// down verbatim and flushed per write. The client ends the session by finishing its chunked
// body (the terminating chunk) -- there is NO WebSocket framing, no ticket, no CSRF and no
// {"type":"end"} control frame.
//
// Auth is handleOpenAITranscription's own: requireInternalOrBearerAnyScope admits a bearer
// token (every ordinary API client) or the internal trusted-loopback pair, DELIBERATELY
// not the browser session cookie (a logged-in page never calls an inference endpoint
// directly). The gate (param validation + resolve + the transcription, streaming and
// fullduplex_proxy verdicts) is resolveLiveTarget, the SAME helper the portal ticket
// endpoint runs, so a model is admitted here exactly when it is admitted there.
func (s *Server) handleBearerLiveTranscription(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireInternalOrBearerAnyScope(w, r, scopeGatewayUse, scopeLLMInvoke)
	if !ok {
		return
	}
	q := r.URL.Query()
	model := strings.TrimSpace(q.Get("model"))
	if model == "" {
		writeJSON(w, http.StatusBadRequest, apierror.Response(transcriptionModelRequired, "model is required", ""))
		return
	}
	params := liveParamsFromQuery(q)
	target, ok := s.resolveLiveTarget(w, r, token, model, params)
	if !ok {
		return
	}
	s.relayBearerLiveTranscription(w, r, token, target, params)
}

// liveParamsFromQuery reads the four audio.cpp /live parameters out of the request query.
// sample_rate and channels are parsed as integers; a value that is absent or not an integer
// becomes 0, which liveByteRate then rejects as out of range (a 400), so a malformed number
// is refused rather than guessed at. language is trimmed, as the ticket endpoint trims it.
func liveParamsFromQuery(q url.Values) liveParams {
	sampleRate, _ := strconv.Atoi(q.Get("sample_rate"))
	channels, _ := strconv.Atoi(q.Get("channels"))
	return liveParams{
		SampleRate: sampleRate,
		Channels:   channels,
		Format:     q.Get("sample_format"),
		Language:   strings.TrimSpace(q.Get("language")),
	}
}

// relayBearerLiveTranscription runs the full-duplex HTTP relay for an already-gated bearer
// session and bills it once. It owns the lifecycle, the twin of relayLiveTranscription's on
// the WebSocket path:
//
//   - ONE per-route session slot, acquired before any relay and released exactly once (a
//     busy route is a clean 503 before a byte moves, with NO usage row -- nothing was
//     relayed);
//   - ONE context, cancelled on any exit, which tears down the upstream request;
//   - the client's PCM pumped into the upstream request body through an io.Pipe by one
//     copy goroutine, counting the payload and latching a clean end-of-input (r.Body's EOF,
//     the terminating chunk) versus a truncated one (any other read error);
//   - a DEFERRED teardown that can leave no goroutine blocked: it closes the pipe reader
//     (unblocking both the transport's read of it and the copy goroutine's pipe write),
//     expires the request read deadline (unblocking a copy goroutine still waiting on
//     r.Body.Read -- the one thing cancelling the context cannot interrupt, because r.Body
//     is the server's own connection, not the client request this ctx bounds), cancels the
//     context, joins, and bills exactly once.
//
// EnableFullDuplex lets the gateway read the client's streaming body while it writes the SSE
// down; liftInferenceDeadlines removes the control-plane read/write deadlines so a long
// session is not cut at 30s (the teardown re-expires the read deadline on purpose, to end
// the copy goroutine, which is why it is set here and not left lifted).
func (s *Server) relayBearerLiveTranscription(w http.ResponseWriter, r *http.Request, token auth.Token, target routing.Target, params liveParams) {
	// One live session per route (audio.cpp serves one /live per model). A second concurrent
	// session on the same route is refused 503 BEFORE any relay, so nothing is billed and no
	// upstream is touched -- the HTTP twin of the WebSocket busy close.
	release, acquired := s.liveSessions.acquire(target.RouteID)
	if !acquired {
		writeJSON(w, http.StatusServiceUnavailable, apierror.Response(liveBearerBusyCode,
			"a live transcription session is already active for this model; retry once it ends", ""))
		return
	}
	defer release()

	liftInferenceDeadlines(w)
	rc := http.NewResponseController(w)
	// HTTP/1.1: read the client's request body while writing the response. Best-effort: a
	// writer without the capability (never the production server) simply does not interleave.
	_ = rc.EnableFullDuplex()

	start := time.Now()
	id := nextRequestID()

	// The one context: cancelled on any exit tears down the upstream request and the pipe
	// feeder together. Derived from the request, but the copy goroutine deliberately reads
	// r.Body, not this ctx (r.Body belongs to the server connection, not this client call).
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// The upstream request body is the read side of this pipe; the copy goroutine writes the
	// inbound PCM into the write side and counts the payload.
	pr, pw := io.Pipe()
	var pcmBytes int64
	var endReceived atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		liveBodyCopyLoop(r.Body, cancel, pw, &pcmBytes, &endReceived)
	}()

	scan := s.newTranscriptionStreamScan(endpointTranscription, false)
	var resp *provider.ProxyResponse
	var upErr error

	// Teardown + billing, DEFERRED so they are airtight even on a panic. Leak-free and in
	// order (the twin of relayLiveTranscription's teardown):
	//  1. Unblock the pipe from BOTH sides: the transport's read of pr and a copy goroutine
	//     blocked on pw.Write both return. The transport uses io.NopCloser, so it never
	//     closes pr for us.
	//  2. Expire the request read deadline, which is the ONLY way to unblock a copy goroutine
	//     still blocked on r.Body.Read when the gateway tears the session down from its own
	//     side (a non-2xx upstream, a dial failure, or the upstream ending) while the client
	//     is still streaming: cancelling ctx aborts the upstream call but never touches
	//     r.Body, the server connection's own reader. A copy goroutine that already saw EOF
	//     (the clean path) has returned before this runs, so expiring the deadline is a no-op
	//     for it.
	//  3. Cancel the one context: aborts the upstream request.
	//  4. Join: pcmBytes and endReceived are stable once the copy goroutine returns.
	//  5. Bill exactly once, through the SAME classifier the WebSocket path uses.
	defer func() {
		_ = pr.CloseWithError(errLiveRelayDone)
		_ = rc.SetReadDeadline(time.Now())
		cancel()
		wg.Wait()
		s.recordLiveUsage(start, id, r.URL.Path, target, params, bearerLiveAttribution(token), target.APIFlavor, liveUsageResult{
			endRequested: endReceived.Load(),
			scan:         scan,
			pcmBytes:     atomic.LoadInt64(&pcmBytes),
			upErr:        upErr,
			resp:         resp,
		})
	}()

	// Open the full-duplex upstream request. ProxyNativeDuplex returns as soon as the
	// response headers arrive, with the body still open and the request body (pr) still being
	// read; it applies no target.Timeout (the deadline is ctx's).
	resp, upErr = s.proxyLiveDuplex(ctx, target, params, pr)
	if upErr != nil {
		code := completionErrorCode(upErr)
		slog.Error("bearer live transcription: upstream dial failed", "id", id, "model", target.Model, "server", s.serverName(target.ServerID), "code", code, "err", upErr)
		writeJSON(w, completionHTTPStatus(upErr), completionErrorResponse(upErr))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.relayBearerLiveUpstreamError(w, resp)
		slog.Warn("bearer live transcription: upstream non-2xx", "id", id, "status", resp.StatusCode, "model", target.Model, "server", s.serverName(target.ServerID))
		return
	}
	// The idle watchdog + per-chunk write deadline, the streamed-transcription relay's own
	// guard (proxyNative): the first SSE frame is a cold-start time-to-first-token, so the
	// first-byte budget is coldStartBudget; every frame then re-arms the watchdog to idle
	// (nativeCopier.writeChunk), and a write that then BLOCKS -- a client that keeps streaming
	// PCM but stops READING the SSE -- is cut by the write deadline after idle, so it can
	// never hold the one per-route session slot open forever and deny every future session on
	// the model. On idle the watchdog cancels the one context, which tears the whole relay
	// down and releases the slot. A genuine dictation pause is not cut here: with no PCM the
	// upstream produces no SSE, and audio.cpp's own idle_timeout_ms ends the stream first (a
	// clean EOF the relay bills normally). When idle is 0 (the operator disabled it) the copier
	// is built without a watchdog, exactly as every other streamed relay is.
	idle := s.streamIdleTimeout
	var watchdog *time.Timer
	if idle > 0 {
		watchdog = time.AfterFunc(coldStartBudget(target, idle), cancel)
		defer watchdog.Stop()
	}
	s.relayBearerLiveSSE(w, resp, scan, watchdog, idle)
}

// liveBodyCopyLoop pumps the client's inbound PCM into the upstream request body through pw
// and counts the payload. It is the SOLE writer of pw and the bearer path's twin of the
// WebSocket liveReadLoop:
//
//   - a read of PCM is written to pw (and so streamed upstream) and its length added to
//     pcmBytes; a write that fails (the pipe was torn down in teardown) ends the loop, the
//     relay already ending;
//   - a CLEAN EOF (io.EOF: the client finished its chunked body with the terminating chunk)
//     latches endReceived and closes pw cleanly, which the transport turns into the
//     terminating chunk audio.cpp needs to emit transcript.text.done + [DONE];
//   - any OTHER read error (the client dropped mid-body, or teardown expired the read
//     deadline) aborts the upstream body with no terminating chunk (so audio.cpp treats the
//     input as truncated, no done event -- a loud zero) and cancels the one context, tearing
//     the relay down.
func liveBodyCopyLoop(body io.Reader, cancel context.CancelFunc, pw *io.PipeWriter, pcmBytes *int64, endReceived *atomic.Bool) {
	buf := make([]byte, 32*1024)
	for {
		rn, rerr := body.Read(buf)
		if rn > 0 {
			n, werr := pw.Write(buf[:rn])
			if werr != nil {
				return // the pipe was torn down; the relay is already ending
			}
			atomic.AddInt64(pcmBytes, int64(n))
		}
		if rerr != nil {
			if rerr == io.EOF {
				endReceived.Store(true)
				_ = pw.Close() // clean EOF -> terminating chunk upstream
			} else {
				_ = pw.CloseWithError(errLiveClientGone) // truncated -> no terminating chunk
				cancel()
			}
			return
		}
	}
}

// relayBearerLiveSSE relays the upstream's own SSE response to the client VERBATIM, flushed
// per write, and feeds the completion scanner so billing knows whether the stream ended the
// way a delivered-whole transcription does (transcript.text.done then [DONE]). It reuses the
// streamed-transcription copier (nativeCopier) with its client writer, the transcript scan
// and -- so a client that stops reading cannot block the write forever -- the idle watchdog +
// per-chunk write deadline; the capture tee and usage scanner stay off, so the SSE is as
// unbuffered here as on the buffered streamed-transcription relay. A failed or timed-out write
// (the client went away or stopped reading) ends the copy; billing then sees an incomplete
// session and loud-zeros it.
func (s *Server) relayBearerLiveSSE(w http.ResponseWriter, resp *provider.ProxyResponse, scan *transcriptionStreamScan, watchdog *time.Timer, idle time.Duration) {
	w.Header().Set(contentTypeHeader, liveSSEContentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	// Flush the response headers BEFORE any body byte, exactly as audio.cpp's own /live
	// does: a full-duplex client waits for the response before it begins streaming PCM, and
	// the first SSE frame is itself causally downstream of that PCM, so without this the two
	// directions deadlock (the client never sends, the gateway never flushes). The copier
	// flushes every subsequent frame as it arrives.
	if flusher != nil {
		flusher.Flush()
	}
	copier := &nativeCopier{w: w, rc: http.NewResponseController(w), flusher: flusher, watchdog: watchdog, idle: idle, transcript: scan}
	_ = copier.run(resp.Body)
}

// relayBearerLiveUpstreamError relays a non-2xx upstream answer (the agent's 404
// model_not_managed / 503 admission_blocked, or a child error audio.cpp answered before the
// SSE upgrade) to the client under the upstream's own status, with its (bounded) body and
// Content-Type. The body is small -- audio.cpp answers an error as a one-line object -- and
// is read bounded by the capture cap, the same bound relayTranscriptionUpstreamError uses.
// The row the deferred teardown then records is an error row (status "error",
// upstream.<code>) with a loud-zero quantity: nothing was transcribed.
func (s *Server) relayBearerLiveUpstreamError(w http.ResponseWriter, resp *provider.ProxyResponse) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, int64(s.captureMaxBytes)))
	contentType := resp.Header.Get(contentTypeHeader)
	if contentType == "" {
		contentType = jsonContentType
	}
	w.Header().Set(contentTypeHeader, contentType)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}
