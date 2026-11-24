// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Vendor group failover fixtures, shared by the translate and passthrough tests.
// The group token belongs to usr_va_a (newVendorAccountSettingsTestServer's
// owner) and covers every account and every group that user owns.
const (
	vgOwner      = "usr_va_a"
	vgSecret     = "va-group-secret"
	vgGroupID    = "vag_failover"
	vgUpstream   = "model-x"
	vgGroupModel = "grp/" + vgUpstream
)

// newVendorGroupTestServer is the vendor-account test server with the master flag
// on, the dispatch cipher, provider p, and a bearer (vgSecret) whose policy covers
// all of usr_va_a's accounts and groups.
func newVendorGroupTestServer(t *testing.T, p provider.Client) (*Server, *routing.MemoryStore, *capture.Cipher) {
	t.Helper()
	srv, routes, _ := newVendorAccountSettingsTestServer(t, true)
	cipher := newDispatchCipher(t)
	srv.Cipher = cipher
	srv.Provider = p
	enableVendorAccountsFlag(t, srv)
	srv.Tokens.(*auth.TokenStore).AddPlainToken(auth.Token{
		ID: "tok_va_group", UserID: vgOwner, Name: "Group Token", Active: true, Scopes: []string{"gateway:use"},
		VendorAccess: auth.VendorAccess{All: true, IncludeGroups: true},
	}, vgSecret)
	return srv, routes, cipher
}

// seedVendorGroupAccount stores one ACTIVE api-key account of vendor owned by
// usr_va_a, serving upstream under its own prefix; baseURL is its root (ignored by
// the fixed-endpoint vendors) and key its plain api key, sealed with cipher.
func seedVendorGroupAccount(t *testing.T, routes *routing.MemoryStore, cipher *capture.Cipher, id, vendor, baseURL, prefix, key string) {
	t.Helper()
	sealed, err := capture.SealSecret(cipher, false, key)
	if err != nil {
		t.Fatalf("SealSecret: %v", err)
	}
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	if err := routes.CreateVendorAccount(ctx, routing.VendorAccount{
		ID: id, OwnerUserID: vgOwner, Vendor: vendor, AuthType: routing.VendorAuthAPIKey, Name: id,
		Status: routing.VendorAccountStatusActive, APIKey: sealed, BaseURL: baseURL, ModelPrefix: prefix, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateVendorAccount %s: %v", id, err)
	}
	flavor := routing.APIFlavorOpenAI
	if vendor == routing.VendorAnthropic {
		flavor = routing.APIFlavorAnthropic
	}
	if err := routes.SetVendorAccountModels(ctx, id, []routing.VendorAccountModel{
		{AccountID: id, GatewayModel: prefix + vgUpstream, UpstreamModel: vgUpstream, APIFlavor: flavor},
	}); err != nil {
		t.Fatalf("SetVendorAccountModels %s: %v", id, err)
	}
}

// seedVendorGroup stores an ACTIVE group of vendor owned by usr_va_a under prefix,
// with members in order.
func seedVendorGroup(t *testing.T, routes *routing.MemoryStore, id, vendor, prefix string, members ...string) {
	t.Helper()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	if err := routes.CreateVendorAccountGroup(ctx, routing.VendorAccountGroup{
		ID: id, OwnerUserID: vgOwner, Vendor: vendor, Name: id, ModelPrefix: prefix, Status: routing.VendorAccountGroupStatusActive,
		ThresholdFiveHourPct: routing.DefaultVendorGroupThresholdPct, ThresholdWeeklyPct: routing.DefaultVendorGroupThresholdPct, ThresholdCreditsPct: routing.DefaultVendorGroupThresholdPct,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateVendorAccountGroup: %v", err)
	}
	if err := routes.SetVendorAccountGroupMembers(ctx, id, members); err != nil {
		t.Fatalf("SetVendorAccountGroupMembers: %v", err)
	}
}

// vgPost sends body to path with the group bearer and returns the recorder.
func vgPost(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+vgSecret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// assertOneVendorUsageRow checks the request left exactly one usage row, attributed
// to accountID and groupID.
func assertOneVendorUsageRow(t *testing.T, srv *Server, accountID, groupID string) {
	t.Helper()
	events := srv.Usage.ByUser(vgOwner)
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want exactly 1 (only the served attempt records): %+v", len(events), events)
	}
	if events[0].AccountID != accountID || events[0].VendorGroupID != groupID {
		t.Fatalf("usage row account/group = %q/%q, want %q/%q", events[0].AccountID, events[0].VendorGroupID, accountID, groupID)
	}
}

// chatVendorStub is an OpenAI-compatible upstream: it counts its calls and answers
// either a limit status (with retry-after) or a chat completion naming itself, as
// a buffered document or an SSE stream following the request's "stream" field.
type chatVendorStub struct {
	srv        *httptest.Server
	name       string
	calls      atomic.Int32
	limit      int // 0 = answer 200
	retryAfter string
}

func newChatVendorStub(t *testing.T, name string, limit int, retryAfter string) *chatVendorStub {
	t.Helper()
	st := &chatVendorStub{name: name, limit: limit, retryAfter: retryAfter}
	st.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if st.limit != 0 {
			if st.retryAfter != "" {
				w.Header().Set("Retry-After", st.retryAfter)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(st.limit)
			_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_exceeded","message":"limit of `+st.name+`"}}`)
			return
		}
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"from `+st.name+`"}}]}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"from `+st.name+`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	t.Cleanup(st.srv.Close)
	return st
}

// newCompatGroupServer wires two OpenAI-compatible accounts (acc_a at a, acc_b at
// b) into one group (vgGroupID, prefix "grp/"), in that order, behind the real
// OpenAI-compatible client.
func newCompatGroupServer(t *testing.T, a, b *chatVendorStub) *Server {
	t.Helper()
	srv, routes, cipher := newVendorGroupTestServer(t, provider.NewMultiplexer(map[string]provider.Client{
		routing.ProviderVendorOpenAI: provider.NewOpenAICompatibleClient(&http.Client{}),
	}, provider.NewMock()))
	seedVendorGroupAccount(t, routes, cipher, "acc_a", routing.VendorOpenAICompatible, a.srv.URL, "a/", "sk-a")
	seedVendorGroupAccount(t, routes, cipher, "acc_b", routing.VendorOpenAICompatible, b.srv.URL, "b/", "sk-b")
	seedVendorGroup(t, routes, vgGroupID, routing.VendorOpenAICompatible, "grp/", "acc_a", "acc_b")
	return srv
}

const vgChatBody = `{"model":"` + vgGroupModel + `","messages":[{"role":"user","content":"hi"}]}`

func TestVendorLimitCooldownUntil(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	epoch := func(d time.Duration) string { return strconv.FormatInt(now.Add(d).Unix(), 10) }
	cases := []struct {
		name       string
		header     http.Header
		retryAfter time.Duration
		want       time.Time
	}{
		{"retry-after before the unified reset", http.Header{"Anthropic-Ratelimit-Unified-Reset": {epoch(time.Hour)}}, 30 * time.Second, now.Add(30 * time.Second)},
		{"unified reset before retry-after", http.Header{"Anthropic-Ratelimit-Unified-Reset": {epoch(20 * time.Second)}}, 5 * time.Minute, now.Add(20 * time.Second)},
		{"no headers: the default", http.Header{}, 0, now.Add(routing.DefaultVendorLimitCooldown)},
		{"exhausted codex secondary window", http.Header{
			"X-Codex-Primary-Used-Percent": {"40"}, "X-Codex-Primary-Reset-At": {epoch(10 * time.Second)},
			"X-Codex-Secondary-Used-Percent": {"100"}, "X-Codex-Secondary-Reset-At": {epoch(2 * time.Hour)},
		}, 0, now.Add(2 * time.Hour)},
		{"earliest exhausted codex window", http.Header{
			"X-Codex-Primary-Used-Percent": {"100"}, "X-Codex-Primary-Reset-At": {epoch(3 * time.Hour)},
			"X-Codex-Secondary-Used-Percent": {"100"}, "X-Codex-Secondary-Reset-At": {epoch(2 * time.Hour)},
		}, 0, now.Add(2 * time.Hour)},
		{"a codex window below 100 says nothing", http.Header{"X-Codex-Primary-Used-Percent": {"99"}, "X-Codex-Primary-Reset-At": {epoch(10 * time.Second)}}, 0, now.Add(routing.DefaultVendorLimitCooldown)},
		{"a reset already past says nothing", http.Header{"Anthropic-Ratelimit-Unified-Reset": {epoch(-time.Minute)}}, 0, now.Add(routing.DefaultVendorLimitCooldown)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := vendorLimitCooldownUntil(tc.header, tc.retryAfter, now); !got.Equal(tc.want) {
				t.Fatalf("vendorLimitCooldownUntil = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestWithExcludedVendorAccountCopiesTheSlice(t *testing.T) {
	orig := inference.Request{ExcludeVendorAccountIDs: make([]string, 1, 4)}
	orig.ExcludeVendorAccountIDs[0] = "acc_a"
	got := withExcludedVendorAccount(orig, "acc_b")
	again := withExcludedVendorAccount(orig, "acc_c")
	if len(orig.ExcludeVendorAccountIDs) != 1 {
		t.Fatalf("the caller's request changed: %v", orig.ExcludeVendorAccountIDs)
	}
	if strings.Join(got.ExcludeVendorAccountIDs, ",") != "acc_a,acc_b" || strings.Join(again.ExcludeVendorAccountIDs, ",") != "acc_a,acc_c" {
		t.Fatalf("exclusions = %v / %v, want acc_a,acc_b / acc_a,acc_c (no shared backing array)", got.ExcludeVendorAccountIDs, again.ExcludeVendorAccountIDs)
	}
}

// TestTranslateFailsOverOnVendorLimit: member 1 of a group answers a buffered chat
// completion with 429; the client gets member 2's answer, member 1 is cooled down
// for the model until its Retry-After, and the one usage row is member 2's.
func TestTranslateFailsOverOnVendorLimit(t *testing.T) {
	a := newChatVendorStub(t, "A", http.StatusTooManyRequests, "30")
	b := newChatVendorStub(t, "B", 0, "")
	srv := newCompatGroupServer(t, a, b)

	before := time.Now()
	rec := vgPost(t, srv, "/v1/chat/completions", vgChatBody)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "from B") {
		t.Fatalf("status = %d body = %s, want 200 with member 2's answer", rec.Code, rec.Body.String())
	}
	if a.calls.Load() != 1 || b.calls.Load() != 1 {
		t.Fatalf("upstream calls A=%d B=%d, want 1 each", a.calls.Load(), b.calls.Load())
	}
	assertOneVendorUsageRow(t, srv, "acc_b", vgGroupID)
	until, ok := srv.VendorLimitCooldowns().Until("acc_a", vgUpstream, time.Now())
	if !ok {
		t.Fatal("member 1 has no limit cooldown for the model")
	}
	if until.Before(before.Add(29*time.Second)) || until.After(time.Now().Add(31*time.Second)) {
		t.Fatalf("cooldown until %s, want about now+30s (Retry-After)", until)
	}
	if _, ok := srv.VendorLimitCooldowns().Until("acc_b", vgUpstream, time.Now()); ok {
		t.Fatal("the serving member must not be cooled down")
	}
}

// TestTranslateStreamFailsOverOnVendorLimitBeforeDelivery: the streamed twin -- a
// 429 before the first event moves the stream to member 2, whose events the client
// gets on the one stream it has.
func TestTranslateStreamFailsOverOnVendorLimitBeforeDelivery(t *testing.T) {
	a := newChatVendorStub(t, "A", http.StatusPaymentRequired, "")
	b := newChatVendorStub(t, "B", 0, "")
	srv := newCompatGroupServer(t, a, b)

	rec := vgPost(t, srv, "/v1/chat/completions", `{"model":"`+vgGroupModel+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "from B") || strings.Contains(body, "provider.unavailable") {
		t.Fatalf("status = %d body = %s, want member 2's stream without an error frame", rec.Code, body)
	}
	if a.calls.Load() != 1 || b.calls.Load() != 1 {
		t.Fatalf("upstream calls A=%d B=%d, want 1 each", a.calls.Load(), b.calls.Load())
	}
	assertOneVendorUsageRow(t, srv, "acc_b", vgGroupID)
	if _, ok := srv.VendorLimitCooldowns().Until("acc_a", vgUpstream, time.Now()); !ok {
		t.Fatal("member 1 has no limit cooldown after its 402")
	}
}

// TestTranslateLastMemberLimitAnswersOriginalError: every member answers 429 --
// each is called once, and the client gets the translate path's error for a limit
// (502 provider.unavailable).
func TestTranslateLastMemberLimitAnswersOriginalError(t *testing.T) {
	a := newChatVendorStub(t, "A", http.StatusTooManyRequests, "")
	b := newChatVendorStub(t, "B", http.StatusTooManyRequests, "")
	srv := newCompatGroupServer(t, a, b)

	rec := vgPost(t, srv, "/v1/chat/completions", vgChatBody)

	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "provider.unavailable") {
		t.Fatalf("status = %d body = %s, want 502 provider.unavailable", rec.Code, rec.Body.String())
	}
	if a.calls.Load() != 1 || b.calls.Load() != 1 {
		t.Fatalf("upstream calls A=%d B=%d, want exactly one each (two in total)", a.calls.Load(), b.calls.Load())
	}
	assertOneVendorUsageRow(t, srv, "acc_b", vgGroupID)
}

// TestTranslateSingleAccountVendorLimitUnchanged: an account reached by its own
// name (no group) keeps today's behaviour -- one upstream call, 502.
func TestTranslateSingleAccountVendorLimitUnchanged(t *testing.T) {
	a := newChatVendorStub(t, "A", http.StatusTooManyRequests, "")
	b := newChatVendorStub(t, "B", 0, "")
	srv := newCompatGroupServer(t, a, b)

	rec := vgPost(t, srv, "/v1/chat/completions", `{"model":"a/`+vgUpstream+`","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body = %s, want 502", rec.Code, rec.Body.String())
	}
	if a.calls.Load() != 1 || b.calls.Load() != 0 {
		t.Fatalf("upstream calls A=%d B=%d, want A once and B never", a.calls.Load(), b.calls.Load())
	}
	assertOneVendorUsageRow(t, srv, "acc_a", "")
	if _, ok := srv.VendorLimitCooldowns().Until("acc_a", vgUpstream, time.Now()); ok {
		t.Fatal("a single-account request must not set a group cooldown")
	}
}

// scriptedStreamProvider is a provider whose stream for each vendor account is
// scripted: it emits the account's events, then returns its error. It counts
// calls per account.
type scriptedStreamProvider struct {
	mu     sync.Mutex
	calls  map[string]int
	events map[string][]inference.StreamEvent
	errs   map[string]error
}

func (p *scriptedStreamProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, provider.ErrUnavailable
}

func (p *scriptedStreamProvider) CompleteStream(_ context.Context, target routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	p.mu.Lock()
	p.calls[target.VendorAccountID]++
	p.mu.Unlock()
	for _, ev := range p.events[target.VendorAccountID] {
		if err := emit(ev); err != nil {
			return err
		}
	}
	return p.errs[target.VendorAccountID]
}

func (p *scriptedStreamProvider) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		n += c
	}
	return n
}

// TestTranslateStreamDoesNotFailOverAfterDelivery (Review Focus 1): member 1
// streams one chunk, then its stream fails with a limit error. The client already
// has that chunk, so the request stays on member 1: no second upstream call, and
// the stream ends with the existing error frame.
func TestTranslateStreamDoesNotFailOverAfterDelivery(t *testing.T) {
	p := &scriptedStreamProvider{
		calls:  map[string]int{},
		events: map[string][]inference.StreamEvent{"acc_a": {{Type: inference.StreamEventTextDelta, Text: "partial from A"}}},
		errs:   map[string]error{"acc_a": &provider.LimitError{Status: http.StatusTooManyRequests, RetryAfter: time.Minute}},
	}
	srv, routes, cipher := newVendorGroupTestServer(t, p)
	seedVendorGroupAccount(t, routes, cipher, "acc_a", routing.VendorOpenAICompatible, "http://a.invalid", "a/", "sk-a")
	seedVendorGroupAccount(t, routes, cipher, "acc_b", routing.VendorOpenAICompatible, "http://b.invalid", "b/", "sk-b")
	seedVendorGroup(t, routes, vgGroupID, routing.VendorOpenAICompatible, "grp/", "acc_a", "acc_b")

	rec := vgPost(t, srv, "/v1/chat/completions", `{"model":"`+vgGroupModel+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "partial from A") || !strings.Contains(body, "provider.unavailable") {
		t.Fatalf("status = %d body = %s, want the partial stream and the provider error frame", rec.Code, body)
	}
	if p.total() != 1 || p.calls["acc_b"] != 0 {
		t.Fatalf("upstream calls = %v, want exactly one, to acc_a", p.calls)
	}
	assertOneVendorUsageRow(t, srv, "acc_a", vgGroupID)
}

// headerRecordingStreamProvider records each account's scripted response headers
// with the request's capture sink (as the Anthropic and Responses clients do, even
// for a refusal) and answers with that account's error.
type headerRecordingStreamProvider struct {
	scriptedStreamProvider
	headers map[string]http.Header
}

func (p *headerRecordingStreamProvider) CompleteStream(ctx context.Context, target routing.Target, req inference.Request, emit provider.StreamEmit) error {
	if h, ok := p.headers[target.VendorAccountID]; ok {
		provider.CaptureSinkFrom(ctx).RecordResponseHeaders(h)
	}
	return p.scriptedStreamProvider.CompleteStream(ctx, target, req, emit)
}

// TestTranslateStreamFailoverAttemptGetsItsOwnCaptureSink: a refused attempt's
// headers stay that member's. Member 1's 429 carries exhausted Codex windows and is
// recorded with the capture sink; member 2's 429 records nothing. The request's one
// usage row is member 2's, and its scrape must not file member 1's windows under
// member 2.
func TestTranslateStreamFailoverAttemptGetsItsOwnCaptureSink(t *testing.T) {
	codex := http.Header{"X-Codex-Primary-Used-Percent": {"100"}, "X-Codex-Primary-Reset-At": {strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)}}
	p := &headerRecordingStreamProvider{
		scriptedStreamProvider: scriptedStreamProvider{
			calls:  map[string]int{},
			events: map[string][]inference.StreamEvent{},
			errs: map[string]error{
				"acc_a": &provider.LimitError{Status: http.StatusTooManyRequests, Header: codex},
				"acc_b": &provider.LimitError{Status: http.StatusTooManyRequests, Header: http.Header{}},
			},
		},
		headers: map[string]http.Header{"acc_a": codex},
	}
	srv, routes, cipher := newVendorGroupTestServer(t, p)
	seedVendorGroupAccount(t, routes, cipher, "acc_a", routing.VendorOpenAICompatible, "http://a.invalid", "a/", "sk-a")
	seedVendorGroupAccount(t, routes, cipher, "acc_b", routing.VendorOpenAICompatible, "http://b.invalid", "b/", "sk-b")
	seedVendorGroup(t, routes, vgGroupID, routing.VendorOpenAICompatible, "grp/", "acc_a", "acc_b")

	rec := vgPost(t, srv, "/v1/chat/completions", `{"model":"`+vgGroupModel+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if !strings.Contains(rec.Body.String(), "provider.unavailable") || p.total() != 2 {
		t.Fatalf("body = %s calls = %v, want both members tried once and the error frame", rec.Body.String(), p.calls)
	}
	assertOneVendorUsageRow(t, srv, "acc_b", vgGroupID)
	if u, ok, err := routes.VendorAccountUsageByID(context.Background(), "acc_a"); err != nil || !ok || u.FiveHourPct != 100 {
		t.Fatalf("member 1 usage = %+v ok=%v err=%v, want its own exhausted window scraped", u, ok, err)
	}
	if u, ok, _ := routes.VendorAccountUsageByID(context.Background(), "acc_b"); ok {
		t.Fatalf("member 2 usage = %+v, want none: member 1's headers were filed under it", u)
	}
}
