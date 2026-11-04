// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
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
	"sync/atomic"
	"time"
)

// liveBearerBusyCode is the wire code a bearer live-transcription request is refused with
// when no instance it could use has a free live slot and it cannot wait for one (a mapping
// without instances, whose one slot is taken). It is API surface: a programmatic client
// branches on it to back off. 503 (not 409) because it is transient -- the slot frees when
// the active session ends -- and it is the HTTP twin of the WebSocket path's
// StatusTryAgainLater busy close.
const liveBearerBusyCode = "transcription.live_busy"

// liveSlotAcquireHook is a TEST-ONLY seam: when non-nil, acquireLiveSession calls it with
// the instance key it is about to acquire, after the selection and before the acquire, so
// a test can take that slot in between -- the race acquireLiveSession re-selects on. It is
// nil in production, and a test that sets it must restore nil in t.Cleanup and must not
// run in parallel (it is a package variable).
var liveSlotAcquireHook func(instanceKey string)

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
	// admit=true: this endpoint is where a live session's single rate/quota/budget
	// admission is spent -- for a bearer client directly, and for a portal session
	// through the WebSocket's internal loopback. The portal ticket mint resolves
	// with admit=false so the one admission is never double-counted. The gate runs
	// once; the selection after it may run again (acquireLiveSession, and the
	// relay's re-selection of a vanished instance) without spending it again.
	req, ok := s.liveGate(w, r, token, model, params, true)
	if !ok {
		return
	}
	req.LiveSession = true
	sess := liveSession{token: token, req: req}
	if sess.target, sess.release, ok = s.acquireLiveSessionOrRefuse(w, r, &sess.token, model, req); !ok {
		return
	}
	s.relayBearerLiveTranscription(w, r, token, &sess, params)
}

// liveSession is a bearer live session's hold on the instance it is relayed to: the
// request it was selected with (re-selection excludes from it), the token resolves
// refresh, the target, and the release of that instance's live slot.
type liveSession struct {
	token   auth.Token
	req     inference.Request
	target  routing.Target
	release func()
}

// acquireLiveSessionOrRefuse is acquireLiveSession for the handler: a refusal is written
// (writeLiveRefusal) and ok is false.
func (s *Server) acquireLiveSessionOrRefuse(w http.ResponseWriter, r *http.Request, token *auth.Token, model string, req inference.Request) (routing.Target, func(), bool) {
	target, release, err := s.acquireLiveSession(r.Context(), token, req)
	if err != nil {
		writeLiveRefusal(w, r, model, err)
		return routing.Target{}, nil, false
	}
	return target, release, true
}

// acquireLiveSession selects an instance for a live session (req.LiveSession, so the
// resolver passes over a held slot, and parks the session when every instance of a
// replicable spec is held) and takes that instance's live slot, returning the target
// and the slot's release.
//
// The selection reads the slots as a snapshot, so another session can take the chosen
// slot before this one acquires it. An instance-capable target that loses that race is
// selected again with its instance excluded (req.ExcludeInstanceKeys): the resolver
// never returns an excluded instance, so each round tries a new one and the loop ends
// within the candidates there are -- answering errTranscriptionLiveBusy once they are
// all excluded. A target without instances keeps today's answer at once: its one slot is
// taken, errTranscriptionLiveBusy. Any other refusal (a routing error, a live verdict,
// a queue timeout) is returned as it is.
func (s *Server) acquireLiveSession(ctx context.Context, token *auth.Token, req inference.Request) (routing.Target, func(), error) {
	for {
		target, err := s.liveTarget(ctx, token, req)
		if err != nil {
			if len(req.ExcludeInstanceKeys) > 0 && errors.Is(err, routing.ErrNoHealthyHost) {
				return routing.Target{}, nil, errTranscriptionLiveBusy
			}
			return routing.Target{}, nil, err
		}
		if slices.Contains(req.ExcludeInstanceKeys, target.InstanceKey) {
			// Only a selection that ignores the exclusion (none does) could get here; it
			// would only lose the same slot again.
			return routing.Target{}, nil, errTranscriptionLiveBusy
		}
		if hook := liveSlotAcquireHook; hook != nil {
			hook(target.InstanceKey)
		}
		if release, ok := s.liveSessions.acquire(target.InstanceKey); ok {
			return target, release, nil
		}
		if !target.InstanceCapable {
			return routing.Target{}, nil, errTranscriptionLiveBusy
		}
		slog.Debug("live transcription: the selected instance's live slot was taken; selecting again", "model", req.Model, "instance", target.InstanceKey)
		req = withLostInstance(req, target)
	}
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
//   - ONE per-instance session slot, acquired by the handler before any relay
//     (acquireLiveSession: a session that cannot get one is refused there before a byte
//     moves, with NO usage row -- nothing was relayed) and released exactly once, here,
//     ahead of the session's active-request row, whose removal wakes the admission queue:
//     a session parked for a slot then finds it free;
//   - ONE context, cancelled on any exit, which tears down the upstream request;
//   - the client's PCM pumped into the upstream request body through a liveFeed by one
//     copy goroutine, counting the payload and latching a clean end-of-input (r.Body's EOF,
//     the terminating chunk) versus a truncated one (any other read error);
//   - ONE re-selection of an instance k >= 1 the agent refuses as gone (see
//     instance_reselect.go): the session moves to another instance -- its slot, its
//     active-request row and its billing with it -- and the new attempt is sent the
//     session's audio from its first byte (liveFeed.restart), so the client never sees
//     the refusal;
//   - for a client that sent Expect: 100-continue, an explicit 100 Continue written before the
//     success response, so the SSE 200 can never reach it ahead of its 100 Continue (see
//     sendContinue and #333);
//   - a DEFERRED teardown that can leave no goroutine blocked: it closes the feed's pipe
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
//
// token is the bearer token the session is attributed to; sess carries the selection the
// handler made, and is moved by a re-selection.
func (s *Server) relayBearerLiveTranscription(w http.ResponseWriter, r *http.Request, token auth.Token, sess *liveSession, params liveParams) {
	liftInferenceDeadlines(w)
	rc := http.NewResponseController(w)
	// HTTP/1.1: read the client's request body while writing the response. Best-effort: a
	// writer without the capability (never the production server) simply does not interleave.
	_ = rc.EnableFullDuplex()

	start := time.Now()
	id := nextRequestID()

	// Register the live session in the Active Requests view (GET /api/portal/usage/active)
	// for its whole lifetime, the same way the chat/stream/native paths do, so an operator
	// sees a live session holding its instance's one /live slot. Added once the slot is held
	// (a busy refusal is written by the handler first, and nothing is relayed or billed there)
	// and removed on every exit path by the defer -- clean end, truncation, dial failure, panic.
	// No Progress: like the buffered inference path, a transcription session has no output
	// tokens to count; its billable measure is the input audio (audio_second), billed once at
	// the end by the teardown below. register runs again for a re-selected session: the one
	// id moves to the new instance.
	register := func() {
		target := sess.target
		s.Active.Add(ActiveRequest{
			ID: id, UserID: token.UserID, TokenID: token.ID, TokenName: token.Name,
			ServiceID: token.ServiceID, ServiceName: token.ServiceName,
			ServerName: s.serverName(target.ServerID), ServerID: target.ServerID, InstanceKey: target.InstanceKey,
			Model: target.Model, RequestedModel: strings.TrimSpace(r.URL.Query().Get("model")),
			APIFlavor: target.APIFlavor, BillingUnit: usage.BillingUnitAudioSecond,
			ReqPath: r.URL.Path, ProviderPath: liveUpstreamPath(target, params),
			ProviderModel: effectiveProviderModel(target, target.Model),
			Stream:        true, StartedAt: start,
		})
	}
	register()
	// The slot is released before the row is removed: removing the row is what wakes a
	// session parked in the admission queue for this server, and it must find the slot free.
	defer func() {
		sess.release()
		s.Active.Remove(id)
	}()

	// The one context: cancelled on any exit tears down the upstream request and the pipe
	// feeder together. Derived from the request, but the copy goroutine deliberately reads
	// r.Body, not this ctx (r.Body belongs to the server connection, not this client call).
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// The upstream request body is the feed's current pipe; the copy goroutine writes the
	// inbound PCM into it and counts the payload. A first attempt at an instance k >= 1 may
	// be re-selected, so its feed keeps what it sends for a replay.
	feed := newLiveFeed(sess.target.Instance() >= 1)
	var pcmBytes int64
	var endReceived atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		liveBodyCopyLoop(r.Body, cancel, feed, &pcmBytes, &endReceived)
	}()

	scan := s.newTranscriptionStreamScan(endpointTranscription, false)
	var resp *provider.ProxyResponse
	var upErr error

	// Teardown + billing, DEFERRED so they are airtight even on a panic. Leak-free and in
	// order (the twin of relayLiveTranscription's teardown):
	//  1. Unblock the feed's pipe from BOTH sides: the transport's read of it and a copy
	//     goroutine blocked writing into it both return. The transport uses io.NopCloser, so
	//     it never closes the pipe for us.
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
	//
	// It bills sess.target as it is then: the instance a re-selection moved the session to.
	defer func() {
		feed.close()
		_ = rc.SetReadDeadline(time.Now())
		cancel()
		wg.Wait()
		s.recordLiveUsage(start, id, r.URL.Path, sess.target, params, bearerLiveAttribution(token), liveUsageResult{
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
	resp, upErr = s.proxyLiveDuplex(ctx, sess.target, params, feed.body())
	if upErr == nil && sess.target.Instance() >= 1 && peekInstanceGone(resp) {
		resp, upErr = s.reselectLiveSession(ctx, sess, feed, params, resp, register)
	}
	target := sess.target
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
	// Accepted: no attempt follows, so the feed stops keeping audio for one.
	feed.accept()
	// The idle watchdog + per-chunk write deadline, the streamed-transcription relay's own
	// guard (proxyNative): the first SSE frame is a cold-start time-to-first-token, so the
	// first-byte budget is coldStartBudget; every frame then re-arms the watchdog to idle
	// (nativeCopier.writeChunk), and a write that then BLOCKS -- a client that keeps streaming
	// PCM but stops READING the SSE -- is cut by the write deadline after idle, so it can
	// never hold its instance's one session slot open forever and deny every future session
	// on it. On idle the watchdog cancels the one context, which tears the whole relay
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
	// ORDER 100 Continue BEFORE the final 200. A client that sent Expect: 100-continue (the
	// voice loopback, openLiveLoopback) holds its streaming body until it sees a 100 Continue
	// or a final response. Go's server sends that 100 Continue by itself on the FIRST
	// r.Body.Read -- the copy goroutine's, which is scheduled independently of the dial that
	// just succeeded and, under CPU contention, can run after it. A 200 written ahead of that
	// read reaches the client FIRST, a Go client then takes the final response for the server
	// refusing the body and ABORTS it, and the relay reads no PCM at all (pcm_bytes=0, no
	// clean end): a live voice turn died with stream_error (#333). Writing the 100 Continue
	// ourselves, right here, makes the order independent of that scheduling: it is one
	// synchronous write ahead of the 200, and it also disables the automatic one, so a late
	// first read adds none.
	//
	// Success path only: every refusal above (the busy 503, a dial failure, a non-2xx upstream)
	// is untouched. The #331 concurrency is untouched too: the copy goroutine keeps feeding the
	// upstream body during the dial, and the 100 Continue it triggers there is what lets the
	// client stream that early PCM; this line only guarantees the 100 Continue is never later
	// than the 200, and at worst repeats one (an informational response a client skips). It
	// waits for nothing, so it cannot hold the 200 back for a client that has not yet sent PCM.
	if requestExpectsContinue(r) {
		sendContinue(w)
	}
	s.relayBearerLiveSSE(w, resp, scan, watchdog, idle)
}

// reselectLiveSession moves a session whose instance k >= 1 refused it as gone (resp, read
// by peekInstanceGone) to another instance, once, and returns the new attempt's answer. The
// selection excludes the lost instance and takes the new instance's slot
// (acquireLiveSession); the feed then starts the new attempt with the session's audio from
// its first byte. Only then is the old attempt let go: its slot released, its answer
// closed, the session's active-request row moved (register) -- in that order, so the lost
// instance's count is never read while it still holds the session. When there is nowhere
// to go -- no other instance, or audio the feed could no longer keep -- the session stays
// as it was and resp, the refusal, is returned to be relayed.
func (s *Server) reselectLiveSession(ctx context.Context, sess *liveSession, feed *liveFeed, params liveParams, resp *provider.ProxyResponse, register func()) (*provider.ProxyResponse, error) {
	lost := sess.target
	req := withLostInstance(sess.req, lost)
	token := sess.token
	target, release, err := s.acquireLiveSession(ctx, &token, req)
	if err != nil {
		slog.Warn("bearer live transcription: the routed instance is gone and re-selection failed", "model", lost.Model, "instance", lost.InstanceKey, "err", err)
		return resp, nil
	}
	body, ok := feed.restart()
	if !ok {
		release()
		slog.Warn("bearer live transcription: the routed instance is gone and the session's audio can no longer be replayed", "model", lost.Model, "instance", lost.InstanceKey)
		return resp, nil
	}
	slog.Info("bearer live transcription: the routed instance is gone; re-selected once", "model", lost.Model, "lost", lost.InstanceKey, "instance", target.InstanceKey)
	_ = resp.Body.Close()
	sess.release()
	sess.token, sess.req, sess.target, sess.release = token, req, target, release
	register()
	return s.proxyLiveDuplex(ctx, target, params, body)
}

// requestExpectsContinue reports whether Go's server will answer this request's first body read
// with an automatic 100 Continue, so the relay owes the client one ahead of the 200. It mirrors
// net/http's own condition exactly: the FIRST Expect header value carries the 100-continue token
// (a list separated by comma, space or tab, case-insensitive), the request is HTTP/1.1 or later,
// and its body is not declared empty (a Content-Length of 0 is never auto-100ed; a chunked body,
// ContentLength -1, is). A client that did not ask is never sent a 100 Continue.
func requestExpectsContinue(r *http.Request) bool {
	if !r.ProtoAtLeast(1, 1) || r.ContentLength == 0 {
		return false
	}
	for _, token := range strings.FieldsFunc(r.Header.Get("Expect"), func(c rune) bool {
		return c == ',' || c == ' ' || c == '\t'
	}) {
		if strings.EqualFold(token, "100-continue") {
			return true
		}
	}
	return false
}

// sendContinue writes a 100 Continue informational response and flushes it. net/http's
// WriteHeader does both for a 1xx, and it also disables the automatic 100 Continue the first
// body read would otherwise send. accessLogResponseWriter forwards a 1xx without recording it,
// so the access log and the span still see the 200 as the response status.
func sendContinue(w http.ResponseWriter) {
	w.WriteHeader(http.StatusContinue)
}

// liveBodyCopyStartHook is a TEST-ONLY seam: when non-nil, liveBodyCopyLoop calls it once,
// before its first read of the request body, so a test can hold the copy goroutine back and
// reproduce the scheduling in which the upstream accepts before that first read (#333). It
// is nil in production, and a test that sets it must restore nil in t.Cleanup and must not
// run in parallel (it is a package variable).
var liveBodyCopyStartHook func()

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
func liveBodyCopyLoop(body io.Reader, cancel context.CancelFunc, feed *liveFeed, pcmBytes *int64, endReceived *atomic.Bool) {
	if hook := liveBodyCopyStartHook; hook != nil {
		hook()
	}
	buf := make([]byte, 32*1024)
	for {
		rn, rerr := body.Read(buf)
		if rn > 0 {
			if !feed.write(buf[:rn]) {
				return // the feed was torn down; the relay is already ending
			}
			atomic.AddInt64(pcmBytes, int64(rn))
		}
		if rerr != nil {
			if rerr == io.EOF {
				endReceived.Store(true)
				feed.end(io.EOF) // clean EOF -> terminating chunk upstream
			} else {
				feed.end(errLiveClientGone) // truncated -> no terminating chunk
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
