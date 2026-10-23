// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

// The bearer live-transcription endpoint (POST /v1/audio/transcriptions/live) is the
// non-browser twin of the portal's WebSocket transport: a chunked full-duplex HTTP
// session that mirrors audio.cpp's own /live. These tests drive it through a REAL
// httptest server (full-duplex needs a genuine connection) on the SAME transcription
// routing fixtures the buffered endpoint uses, pointed at the WebSocket tests'
// full-duplex mock child, so the gate and the relay are held to the same routing.

// devBearerHeader is the dev bearer the transcription fixtures seed (usr_dev / tok_dev,
// gateway:use).
var devBearerHeader = map[string]string{"Authorization": "Bearer dev-secret"}

// bearerLiveQuery is a valid 16 kHz mono s16le query for model: 32000 bytes = 1.0 second.
func bearerLiveQuery(model string) string {
	return "model=" + model + "&sample_rate=16000&channels=1&sample_format=s16le"
}

// startBearerLive opens a chunked full-duplex POST to the bearer live endpoint: the
// request body is the returned pipe (PCM is written to it; closing it is the terminating
// chunk that ends the session cleanly). Do returns once the gateway's SSE response headers
// arrive, with the body still open -- the full-duplex handshake.
func startBearerLive(t *testing.T, ctx context.Context, ts *httptest.Server, query string, hdr map[string]string) (*io.PipeWriter, *http.Response) {
	t.Helper()
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/audio/transcriptions/live?"+query, pr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return pw, resp
}

// readBearerSSE returns the payload of the next "data:" line, failing when none arrives in
// d (a blocked read is what a buffering relay looks like, so the bound turns a deadlock
// into an attributable failure).
func readBearerSSE(t *testing.T, br *bufio.Reader, d time.Duration) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				done <- result{err: err}
				return
			}
			if line = strings.TrimRight(line, "\r\n"); strings.HasPrefix(line, "data:") {
				done <- result{line: strings.TrimSpace(strings.TrimPrefix(line, "data:"))}
				return
			}
		}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("reading the SSE response: %v", res.err)
		}
		return res.line
	case <-time.After(d):
		t.Fatalf("no SSE data within %s: the relay buffered instead of streaming", d)
		return ""
	}
}

// requireOneBearerLiveRow polls for the single recorded usage row and asserts its unit and
// quantity, returning it for the caller's attribution checks.
func requireOneBearerLiveRow(t *testing.T, srv *Server, wantUnit string, wantQuantity float64) usage.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows := srv.Usage.All()
		if len(rows) == 1 {
			ev := rows[0]
			if ev.BillingUnit != wantUnit || ev.BillingQuantity != wantQuantity {
				t.Fatalf("billing = %v %q, want %v %q", ev.BillingQuantity, ev.BillingUnit, wantQuantity, wantUnit)
			}
			return ev
		}
		if len(rows) > 1 {
			t.Fatalf("expected exactly one usage row, got %d", len(rows))
		}
		if time.Now().After(deadline) {
			t.Fatalf("no usage row recorded within deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// postBearerLiveRecorder drives the endpoint through the mux with a recorder: enough for
// the refusals that answer BEFORE any relay (auth, params, the streaming/served gate),
// which never read the body or upgrade to full-duplex.
func postBearerLiveRecorder(t *testing.T, srv *Server, query string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions/live?"+query, strings.NewReader(""))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// A streaming-capable model streams PCM up and relays the upstream's SSE down live, and a
// clean end-of-input (the terminating chunk) bills pcmBytes / byteRate: 32000 bytes at
// 16 kHz mono s16le (32000 bytes/s) is 1.0 second. The row is attributed to the bearer's
// own user and token and to the resolved route, and the forwarded upstream query carries the
// mapped model name and the billed rate triple (billing-rate == forwarded-rate).
func TestBearerLiveTranscriptionStreamsAndBills(t *testing.T) {
	child := newMockLiveChild(t)
	srv, _ := newTranscriptionCapableTestServer(t, child.server.URL)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pw, resp := startBearerLive(t, ctx, ts, bearerLiveQuery("asr-model"), devBearerHeader)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != liveSSEContentType {
		t.Fatalf("Content-Type = %q, want %q", ct, liveSSEContentType)
	}
	br := bufio.NewReader(resp.Body)

	// PCM up -> a delta comes back live, before the client ends its body.
	if _, err := pw.Write(make([]byte, 32000)); err != nil {
		t.Fatalf("write pcm: %v", err)
	}
	if first := readBearerSSE(t, br, 5*time.Second); !strings.Contains(first, "transcript.text.delta") {
		t.Fatalf("first SSE = %q, want a transcript.text.delta", first)
	}

	// The terminating chunk -> the upstream emits done + [DONE].
	_ = pw.Close()
	sawDone := false
	for {
		line := readBearerSSE(t, br, 5*time.Second)
		if strings.Contains(line, "transcript.text.done") {
			sawDone = true
			break
		}
	}
	if !sawDone {
		t.Fatal("never received transcript.text.done")
	}

	ev := requireOneBearerLiveRow(t, srv, usage.BillingUnitAudioSecond, 1.0)
	if ev.RouteID != "route-asr" || ev.UserID != "usr_dev" || ev.TokenID != "tok_dev" {
		t.Fatalf("attribution = route=%q user=%q token=%q, want route-asr/usr_dev/tok_dev", ev.RouteID, ev.UserID, ev.TokenID)
	}
	if ev.Status != "success" {
		t.Fatalf("status = %q, want success", ev.Status)
	}
	if child.path() != "/v1/audio/transcriptions/live" {
		t.Fatalf("child path = %q, want /v1/audio/transcriptions/live", child.path())
	}
	if !strings.Contains(child.query(), "model=whisper_x") || !strings.Contains(child.query(), "sample_rate=16000") || !strings.Contains(child.query(), "sample_format=s16le") {
		t.Fatalf("forwarded query %q lacks model=whisper_x and the billed rate triple", child.query())
	}
}

// Billing is attributed to the bearer token's service/project: a token carrying a project
// bills a row with that project (a service token works on this endpoint, unlike the portal
// ticket endpoint which refuses one). The clean session bills 1.0 second as above.
func TestBearerLiveTranscriptionAttributesTokenProject(t *testing.T) {
	child := newMockLiveChild(t)
	srv, dir := newTranscriptionCapableTestServer(t, child.server.URL)
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	if err := dir.CreateProject(ctx, store.Project{ID: "proj_notes", Name: "Call Notes", OwnerUserID: "usr_dev", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := dir.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_proj", UserID: "usr_dev", ProjectID: "proj_notes", Name: "Project token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "proj-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	runCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pw, resp := startBearerLive(t, runCtx, ts, bearerLiveQuery("asr-model"), map[string]string{"Authorization": "Bearer proj-secret"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	if _, err := pw.Write(make([]byte, 32000)); err != nil {
		t.Fatalf("write pcm: %v", err)
	}
	_ = readBearerSSE(t, br, 5*time.Second)
	_ = pw.Close()
	for {
		line := readBearerSSE(t, br, 5*time.Second)
		if strings.Contains(line, "transcript.text.done") {
			break
		}
	}

	ev := requireOneBearerLiveRow(t, srv, usage.BillingUnitAudioSecond, 1.0)
	if ev.UserID != "usr_dev" || ev.TokenID != "tok_proj" {
		t.Fatalf("UserID/TokenID = %q/%q, want usr_dev/tok_proj", ev.UserID, ev.TokenID)
	}
	if ev.ProjectID != "proj_notes" || ev.ProjectName != "Call Notes" {
		t.Fatalf("ProjectID/ProjectName = %q/%q, want proj_notes/Call Notes", ev.ProjectID, ev.ProjectName)
	}
	if ev.ServiceID != "" {
		t.Fatalf("ServiceID = %q, want empty for a user token", ev.ServiceID)
	}
}

// A client that drops its body WITHOUT the terminating chunk is a truncated session: the
// upstream is aborted (no done event) and the row is a loud zero. It stays status="success"
// (the upstream answered 2xx; only delivery was cut), never over-classified as an error row.
func TestBearerLiveTranscriptionTruncatedLoudZero(t *testing.T) {
	child := newMockLiveChild(t)
	srv, _ := newTranscriptionCapableTestServer(t, child.server.URL)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pw, resp := startBearerLive(t, ctx, ts, bearerLiveQuery("asr-model"), devBearerHeader)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	if _, err := pw.Write(make([]byte, 16000)); err != nil {
		t.Fatalf("write pcm: %v", err)
	}
	_ = readBearerSSE(t, br, 5*time.Second)
	// Abort the body WITHOUT a terminating chunk (a reset, not a clean close).
	_ = pw.CloseWithError(errors.New("client gone"))

	ev := requireOneBearerLiveRow(t, srv, usage.BillingUnitAudioSecond, 0)
	if ev.Status != "success" {
		t.Fatalf("status = %q, want success (truncated is success + loud zero, not an error row)", ev.Status)
	}
}

// A non-2xx UPSTREAM answer is relayed to the client under the upstream's own status and
// recorded as an ERROR row (status "error" + upstream.<code>) with a loud-zero quantity --
// never a self-contradictory success row that hides a real backend failure. Reaching the
// row also proves teardown unblocks the still-streaming request-body copy via the read
// deadline (the client sent one frame and nothing more, so only that can end the copy loop).
func TestBearerLiveTranscriptionNon2xxUpstreamErrorRow(t *testing.T) {
	child := newMockLiveChild(t)
	child.statusCode = http.StatusNotFound
	srv, _ := newTranscriptionCapableTestServer(t, child.server.URL)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pw, resp := startBearerLive(t, ctx, ts, bearerLiveQuery("asr-model"), devBearerHeader)
	defer resp.Body.Close()
	// Mirror the mic flow: a PCM frame follows the request immediately.
	_, _ = pw.Write(make([]byte, 320))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 relayed from the upstream", resp.StatusCode)
	}

	ev := requireOneBearerLiveRow(t, srv, usage.BillingUnitAudioSecond, 0)
	if ev.Status != "error" {
		t.Fatalf("status = %q, want error", ev.Status)
	}
	if ev.ErrorCode != "upstream.404" {
		t.Fatalf("errorCode = %q, want upstream.404", ev.ErrorCode)
	}
	if ev.HTTPStatus != http.StatusNotFound {
		t.Fatalf("httpStatus = %d, want 404", ev.HTTPStatus)
	}
	_ = pw.Close()
}

// A second concurrent session on the same route is refused 503 (the busy code) before any
// relay, and records no usage row for the pure busy-reject. The first session keeps the slot
// until it ends.
func TestBearerLiveTranscriptionBusyRejectsSecondSession(t *testing.T) {
	child := newMockLiveChild(t)
	srv, _ := newTranscriptionCapableTestServer(t, child.server.URL)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Session 1 holds the slot: established once it has exchanged a frame.
	pw1, resp1 := startBearerLive(t, ctx, ts, bearerLiveQuery("asr-model"), devBearerHeader)
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("session 1 status = %d, want 200", resp1.StatusCode)
	}
	br1 := bufio.NewReader(resp1.Body)
	if _, err := pw1.Write(make([]byte, 320)); err != nil {
		t.Fatalf("session1 pcm: %v", err)
	}
	_ = readBearerSSE(t, br1, 5*time.Second)

	// Session 2 on the same route -> 503 busy, before any relay (the recorder never
	// reaches the full-duplex upgrade on this path).
	rec := postBearerLiveRecorder(t, srv, bearerLiveQuery("asr-model"), devBearerHeader)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("session 2 status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), liveBearerBusyCode) {
		t.Fatalf("session 2 body = %s, want the busy code %q", rec.Body.String(), liveBearerBusyCode)
	}

	// Release session 1.
	_ = pw1.Close()
	for {
		line := readBearerSSE(t, br1, 5*time.Second)
		if strings.Contains(line, "transcript.text.done") {
			break
		}
	}
	// Exactly one usage row: session 1's clean session. The busy reject recorded none.
	requireOneBearerLiveRow(t, srv, usage.BillingUnitAudioSecond, 320.0/32000.0)
}

// AVAILABILITY: a client that establishes a session and then goes idle -- the half-open
// shape, the twin of the WebSocket ping guard -- must not hold the single per-route slot
// forever. The idle watchdog (streamIdleTimeout, armed on the SSE relay) fires, tears the
// relay down and RELEASES the slot, so a later session on the same model is not denied 503
// forever. A short idle makes the test fast; the slot's release is asserted directly on the
// registry.
func TestBearerLiveTranscriptionIdleClientReleasesSlot(t *testing.T) {
	child := newMockLiveChild(t)
	srv, _ := newTranscriptionCapableTestServer(t, child.server.URL)
	srv.streamIdleTimeout = 300 * time.Millisecond
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pw, resp := startBearerLive(t, ctx, ts, bearerLiveQuery("asr-model"), devBearerHeader)
	defer resp.Body.Close()
	defer pw.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	// Establish: one PCM frame -> one delta (the copier writes it, which arms the per-chunk
	// write deadline and re-arms the idle watchdog to the idle budget).
	if _, err := pw.Write(make([]byte, 320)); err != nil {
		t.Fatalf("write pcm: %v", err)
	}
	_ = readBearerSSE(t, br, 5*time.Second)

	// The client now goes idle: it neither streams more PCM nor reads the SSE. The watchdog
	// must fire within ~idle, tear the relay down and release the per-route slot. The slot is
	// busy until it does, so acquiring it succeeds only once the idle session has been reaped.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if release, ok := srv.liveSessions.acquire("route-asr"); ok {
			release()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the per-route slot was never released after the client went idle: the idle watchdog did not reap the session")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The reaped session is a loud zero (no terminating chunk, no done event), success + 0.
	ev := requireOneBearerLiveRow(t, srv, usage.BillingUnitAudioSecond, 0)
	if ev.Status != "success" {
		t.Fatalf("status = %q, want success (an idle-reaped session is a loud zero, not an error row)", ev.Status)
	}
}

// A missing bearer is refused 401 with no relay (the upstream is never called).
func TestBearerLiveTranscriptionMissingBearerUnauthorized(t *testing.T) {
	child := newMockLiveChild(t)
	srv, _ := newTranscriptionCapableTestServer(t, child.server.URL)

	rec := postBearerLiveRecorder(t, srv, bearerLiveQuery("asr-model"), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	if child.path() != "" {
		t.Fatalf("the upstream was called (%q) for an unauthenticated request", child.path())
	}
	if n := len(srv.Usage.All()); n != 0 {
		t.Fatalf("usage rows = %d, want 0 for a rejected request", n)
	}
}

// THE BOUNDARY: a browser session cookie must NOT reach this endpoint (the bearer legs are
// requireInternalOrBearerAnyScope's, deliberately without the session cookie). A 401 proves
// auth refused -- had the cookie been accepted the request would have failed later and
// differently.
func TestBearerLiveTranscriptionRefusesASessionCookie(t *testing.T) {
	srv, dir := newChatTestServer(t)
	seedLoginUser(t, dir, "usr_live", "live@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "live@example.test", "password-1")

	rec := postBearerLiveRecorder(t, srv, bearerLiveQuery("asr-model"), map[string]string{
		"Cookie":       cookie.Name + "=" + cookie.Value,
		csrfHeaderName: "1",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 from a real session cookie: %s", rec.Code, rec.Body.String())
	}
}

// A model whose transcription_streaming verdict is not yes (an offline model) cannot take a
// live session: 409, the SAME code the portal ticket endpoint answers (the gate is shared).
func TestBearerLiveTranscriptionNonStreamingModelConflict(t *testing.T) {
	child := newMockLiveChild(t)
	srv, _ := newTranscriptionCapableTestServer(t, child.server.URL)

	rec := postBearerLiveRecorder(t, srv, bearerLiveQuery("asr-offline"), devBearerHeader)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), liveTranscriptionNotStreaming) {
		t.Fatalf("body = %s, want %q", rec.Body.String(), liveTranscriptionNotStreaming)
	}
	if child.path() != "" {
		t.Fatalf("the upstream was called for a non-streaming model")
	}
}

// Invalid query params are refused 400 with the ticket endpoint's own params code, before
// any upstream is contacted: an unknown sample_format here.
func TestBearerLiveTranscriptionInvalidParamsBadRequest(t *testing.T) {
	child := newMockLiveChild(t)
	srv, _ := newTranscriptionCapableTestServer(t, child.server.URL)

	rec := postBearerLiveRecorder(t, srv, "model=asr-model&sample_rate=16000&channels=1&sample_format=s24le", devBearerHeader)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), liveTranscriptionParamsInvalid) {
		t.Fatalf("body = %s, want %q", rec.Body.String(), liveTranscriptionParamsInvalid)
	}
}

// A blank model is refused 400 with the transcription endpoint's own model-required code,
// before the params are even looked at.
func TestBearerLiveTranscriptionBlankModelBadRequest(t *testing.T) {
	child := newMockLiveChild(t)
	srv, _ := newTranscriptionCapableTestServer(t, child.server.URL)

	rec := postBearerLiveRecorder(t, srv, "sample_rate=16000&channels=1&sample_format=s16le", devBearerHeader)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), transcriptionModelRequired) {
		t.Fatalf("body = %s, want %q", rec.Body.String(), transcriptionModelRequired)
	}
}

// Only POST is served.
func TestBearerLiveTranscriptionRefusesOtherMethods(t *testing.T) {
	child := newMockLiveChild(t)
	srv, _ := newTranscriptionCapableTestServer(t, child.server.URL)

	req := httptest.NewRequest(http.MethodGet, "/v1/audio/transcriptions/live?"+bearerLiveQuery("asr-model"), nil)
	for k, v := range devBearerHeader {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// TestBearerLiveTranscriptionRegistersActiveRequest pins that a live session shows in the
// Active Requests view (GET /api/portal/usage/active) for its whole lifetime, like the
// chat/stream/native paths -- an operator must be able to see a live session holding a
// model's single /live slot. The mock child's onServe hook snapshots s.Active WHILE the
// relay is in flight; after the relay ends the deferred Remove has run and nothing lingers.
func TestBearerLiveTranscriptionRegistersActiveRequest(t *testing.T) {
	child := newMockLiveChild(t)
	srv, _ := newTranscriptionCapableTestServer(t, child.server.URL)

	duringCh := make(chan []ActiveRequest, 1)
	child.onServe = func() { duringCh <- srv.Active.Snapshot() }

	rec := postBearerLiveRecorder(t, srv, bearerLiveQuery("asr-model"), devBearerHeader)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	// DURING the relay (the child was serving) the live session is registered, attributed,
	// and shaped like a streaming audio_second request.
	var during []ActiveRequest
	select {
	case during = <-duringCh:
	case <-time.After(3 * time.Second):
		t.Fatal("the upstream child was never contacted; the relay did not run")
	}
	if len(during) != 1 {
		t.Fatalf("active snapshot during the relay = %d entries, want exactly one live session: %+v", len(during), during)
	}
	got := during[0]
	if got.ReqPath != "/v1/audio/transcriptions/live" {
		t.Errorf("ReqPath = %q, want /v1/audio/transcriptions/live", got.ReqPath)
	}
	if got.BillingUnit != usage.BillingUnitAudioSecond {
		t.Errorf("BillingUnit = %q, want %q", got.BillingUnit, usage.BillingUnitAudioSecond)
	}
	if !got.Stream {
		t.Errorf("Stream = false, want true (a live session is a streaming relay)")
	}
	if got.RequestedModel != "asr-model" {
		t.Errorf("RequestedModel = %q, want asr-model (the client's requested model)", got.RequestedModel)
	}
	if got.Model == "" || got.UserID == "" || got.TokenID == "" {
		t.Errorf("live row not fully attributed: Model=%q UserID=%q TokenID=%q", got.Model, got.UserID, got.TokenID)
	}

	// AFTER the relay ends the deferred Remove has run: no live session lingers in the view.
	if after := srv.Active.Snapshot(); len(after) != 0 {
		t.Fatalf("live session still in the Active view after end: %+v", after)
	}
}
