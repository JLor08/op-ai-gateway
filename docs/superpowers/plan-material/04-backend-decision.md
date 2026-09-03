# Plan material — Area 04: Backend request-time pass-through decision + error codes

Scope: the gateway-side, request-time decision that turns the effective
`EndpointMode` carried on the routing `Target` into one of three outcomes for a
Codex (`/v1/responses`) or Claude Code (`/v1/messages`) request — **passthrough
→ proxy raw**, **translate → fall through to compat**, **disabled → reject with a
stable error code + 4xx (never fall through)** — plus registration of the two new
stable error codes.

All paths below are under the worktree
`/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/api-variant-modes`.

This area **consumes** from Area 01 (routing/store) and **produces** the error
codes + the decision for the handlers. See the INTERFACES section for the exact
coupling and ordering.

---

## 1. CURRENT STATE (exact excerpts + file:line)

### 1.1 The decision helper — `gateway/backend/internal/gateway/native_passthrough.go:31-43`

Currently a two-way (translate vs pass-through) boolean read straight off the
Target's `NativeResponses`/`NativeMessages`:

```go
// native_passthrough.go:35-43
func nativePassthroughEnabled(target routing.Target, apiFlavor string) (string, bool) {
	switch apiFlavor {
	case "openai_responses":
		return "/v1/responses", target.NativeResponses
	case "anthropic_messages":
		return "/v1/messages", target.NativeMessages
	}
	return "", false
}
```

### 1.2 `upstreamPath` uses that helper — `native_passthrough.go:62-73`

```go
// native_passthrough.go:62-73
func upstreamPath(target routing.Target, apiFlavor string) string {
	if target.Provider == "" {
		return ""
	}
	if p, native := nativePassthroughEnabled(target, apiFlavor); native {  // :66
		return p
	}
	if target.Provider == routing.ProviderOllama {
		return "/api/chat"
	}
	return "/v1/chat/completions"
}
```

(The `KNOWN LIMIT` doc block at `native_passthrough.go:55-61` references the
"native-flagged application" — wording to refresh to "passthrough-mode".)

### 1.3 The decision + fall-through in `tryProxyNative` — `native_passthrough.go:153-166`

This is the **exact block that changes** from two-way to three-way. Today, after
`resolveTarget` succeeds:

```go
// native_passthrough.go:153-166
	path, enabled := nativePassthroughEnabled(target, apiFlavor)
	if !enabled {
		// The resolved application does NOT have native passthrough enabled for this
		// endpoint, so the request falls back to the (lossy, text-only) translate
		// path — which rejects rich Codex/Claude multi-turn bodies. This log names
		// the exact application + flag state so a missing toggle is obvious.
		slog.Debug("native passthrough not applied: not enabled on the resolved application",
			"path", r.URL.Path, "api_flavor", apiFlavor, "model", model,
			"server", s.serverName(target.ServerID),
			"native_responses", target.NativeResponses, "native_messages", target.NativeMessages)  // :162 reads removed fields
		return false
	}
	s.proxyNative(w, r, token, target, path, raw, req)
	return true
```

`start := time.Now()` is at the top of `tryProxyNative` (`native_passthrough.go:121`)
and `req := pf.Req` / `model := req.Model` at `:122-123` — both in scope for the
new disabled branch's `recordUsage` call.

### 1.4 The two call sites in the handlers — `inference_handlers.go`

`handleOpenAIResponses` (`inference_handlers.go:86-159`) calls it at **:126**:

```go
// inference_handlers.go:119-128
	if model != "" {
		pf, handled = s.inferencePreflight(w, r, token, raw, inferenceShape{apiFlavor: "openai_responses", endpoint: endpointResponses, model: model, stream: stream})
		if handled {
			return
		}
		// Native passthrough: if the resolved application supports Codex natively,
		// proxy the raw body to the upstream /v1/responses instead of translating.
		if s.tryProxyNative(w, r, token, raw, "openai_responses", pf) {  // :126
			return
		}
	}
```

`handleAnthropicMessages` (`inference_handlers.go:161-214`) calls it at **:188**
with `"anthropic_messages"`. Both fall through, on `false`, to
`compat.ParseOpenAIResponses` / `compat.ParseAnthropicMessages` (`:130` / `:192`)
and the translate completion path. **No handler change is required** — the
handlers already branch on `tryProxyNative`'s bool return (`true` = handled,
`false` = translate). The disabled rejection is handled entirely inside
`tryProxyNative` returning `true` (handled), so the fall-through is skipped.

`inferencePreflight` (`inference_handlers.go:496-526`) is unchanged — the
endpoint-mode decision is **post-resolve** (needs the Target), so it lives in
`tryProxyNative`, not the pre-resolve gate.

### 1.5 The Target fields this area reads (produced by Area 01) — `routing/resolver.go:48-74`, set at `:890-906`

Today the Target carries the two booleans (Area 01 replaces these with the two
`EndpointMode` fields):

```go
// resolver.go:64-69  (inside type Target struct)
	// NativeResponses / NativeMessages mirror the resolved application's
	// native-passthrough flags, so the handler can decide whether to proxy the raw
	// client body to the upstream ...
	NativeResponses bool
	NativeMessages  bool
```

```go
// resolver.go:890-906  targetFrom(...) — Area 01 makes these the EFFECTIVE (spec-or-app) modes
	NativeResponses:      app.NativeResponses,   // :902
	NativeMessages:       app.NativeMessages,    // :903
```

### 1.6 Error-code conventions to match

- Stable codes are dotted, lowercase `noun.reason` strings; **the code string is
  the wire contract** (AGENTS.md; `docs/architecture/reference/api-surface.md:294-295`
  — "the sentinel's own message string **is** the wire code").
- Routing sentinels: `routing/resolver.go:26-42` — e.g.
  `ErrNoModelRoute = errors.New("routing.no_model_route")`,
  `ErrAdmissionQueueTimeout = errors.New("routing.admission_queue_timeout")`.
- Pre-upstream inference gates that reject with a plain inline
  `apierror.Response(code, msg, "")` + status (the pattern the new code follows):
  - `writeModelNotAllowed` — `inference_handlers.go:339-341` → **403** `model.not_allowed`.
  - `writeServerOverrideForbidden` — `inference_handlers.go:304-306` → **403** `server_override.forbidden`.
  - `writeLimitDenied` — `inference_handlers.go:538-558` → 429/402 `limit.*`.
- Model-scoped "not served here" already uses **404** in the codebase (e.g.
  `mapping.not_found`, `runtime.model_not_managed`; `api-surface.md:151,561`).
- The apierror JSON envelope (`internal/apierror/errors.go:6-24`):
  ```json
  {"error":{"code":"...","message":"...","request_id":"..."}}
  ```
  written via `writeJSON` / `writeJSONCaptured` (`server.go:1750-1767`).
- These new codes are **not** sentinel-mapped through `sharedErrorMap` /
  `writeMappedError` (`error_map.go`) — that table is for `error`-sentinel
  matching in the portal/admin mappers. Inference-path codes are written inline
  (like `model.not_allowed`), so we register plain string consts + an inline
  write, matching `codeRequestInvalidJSON` at `auth.go:65-70`.
- `completionHTTPStatus` / `completionErrorCode` (`inference_complete.go:787-824`)
  map **provider/routing** errors; the disabled rejection is a deliberate
  policy 4xx, not a provider error, so it is **not** added there.

---

## 2. INTERFACES

### PRODUCES (other areas consume)

- **New stable error codes** (canonical names from the spec — used verbatim):
  - `responses.endpoint_disabled`
  - `messages.endpoint_disabled`
  - Registered as package consts in `native_passthrough.go`:
    `codeResponsesEndpointDisabled`, `codeMessagesEndpointDisabled`.
  - **HTTP status: `404 Not Found`** (recommended) via one const
    `statusEndpointDisabled`. Rationale: semantically "this model does not serve
    this endpoint here" (absence), matching the codebase's model-scoped-404
    posture and avoiding the auth implication of 403. **DECISION TO CONFIRM**
    (spec §13 leaves status open): the model.not_allowed-aligned alternative is
    **403 Forbidden**; the const makes it a one-line change either way.
  - Docs to update (Area "docs"): `api-surface.md` inference table (add the two
    codes + status), `compatibility-and-inference.md` native-passthrough section.
- **Decision behavior** the handlers rely on (contract with the two call sites at
  `inference_handlers.go:126` / `:188`): `tryProxyNative` returns `true` for both
  `passthrough` (proxied) and `disabled` (rejected) — i.e. "handled, do not
  translate" — and `false` only for `translate`.

### CONSUMES (from Area 01 — routing/store)

- `type EndpointMode string` and consts (canonical, in package `routing`):
  `EndpointModeDisabled = "disabled"`, `EndpointModeTranslate = "translate"`,
  `EndpointModePassthrough = "passthrough"`.
- `routing.Target.ResponsesMode` / `routing.Target.MessagesMode` of type
  `EndpointMode`, set by `targetFrom` to the **effective** value (spec's for a
  `server_agent` app with a resolved spec; else the app's). This area is agnostic
  to the source — it reads whatever effective mode the Target already carries.
- `routing.Application.ResponsesMode` / `MessagesMode` (replacing the two bools) —
  consumed **only** through `targetFrom`/`Target`; this area does not read
  `Application` directly.

### Ordering dependency (for the plan author)

Area 01 must land **first**: the `EndpointMode` type + consts,
`Target.ResponsesMode/MessagesMode`, `Application.ResponsesMode/MessagesMode`, and
`targetFrom` populating the effective values. This area's code + the shared test
seed helper (which today sets the removed `NativeResponses`/`NativeMessages`
fields — §4) will not **compile** until that rename is in.

---

## 3. PROPOSED TDD TASKS (ordered, bite-sized, real code)

> Test framework: standard library `testing` + `net/http/httptest`, table-driven,
> in-package (`package gateway`). This area's tests live in
> `internal/gateway/native_passthrough_test.go` and `internal/gateway/server_test.go`
> (the shared seed helpers). Run commands assume `cd gateway/backend`.
>
> Existing sibling tests to mirror exactly:
> `TestOpenAIResponsesNativePassthroughProxiesRawBody` (`server_test.go:1950`) and
> `TestOpenAIResponsesNonNativeUsesTranslatePath` (`server_test.go:2004`).

### Task 04.0 (scaffolding, folded in) — mode-explicit seed helper

The shared helper `newNativeProxyTestServer` (`server_test.go:1918-1948`) sets
`NativeResponses`/`NativeMessages` on the seeded `routing.Application`
(`server_test.go:1932`). After Area 01's field rename this no longer compiles.
Refactor it to a mode-explicit helper and keep the bool wrapper for the existing
call sites (`true → passthrough`, `false → translate`, which preserves every
existing caller's meaning):

```go
// server_test.go — replaces the current newNativeProxyTestServer body.

// newNativeModeTestServer seeds one vLLM upstream + app + mapping (gateway model
// "gw-model" -> upstream "upstream-model") with EXPLICIT per-endpoint modes, so a
// test can drive disabled/translate/passthrough directly.
func newNativeModeTestServer(prov provider.Client, responsesMode, messagesMode routing.EndpointMode) *Server {
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		panic(err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	ctx := context.Background()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-native", Name: "Native Upstream", Domain: "native.example.test", Provider: routing.ProviderVLLM, Endpoint: "http://native.example.test:8000", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		panic(err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-native", ServerID: "srv-native", Type: routing.ProviderVLLM, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, ResponsesMode: responsesMode, MessagesMode: messagesMode, CreatedAt: now, UpdatedAt: now}); err != nil {
		panic(err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-native", ApplicationID: "app-native", GatewayModelName: "gw-model", AppModelName: "upstream-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		panic(err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-native", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		panic(err)
	}
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: prov,
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

// newNativeProxyTestServer keeps the existing bool contract (true => passthrough,
// false => translate) so every current call site is unchanged.
func newNativeProxyTestServer(prov provider.Client, nativeResponses, nativeMessages bool) *Server {
	return newNativeModeTestServer(prov, modeFromBool(nativeResponses), modeFromBool(nativeMessages))
}

func modeFromBool(b bool) routing.EndpointMode {
	if b {
		return routing.EndpointModePassthrough
	}
	return routing.EndpointModeTranslate
}
```

Also update `newCapAdmissionTestServer` (`native_passthrough_test.go:544-581`),
which sets `NativeResponses: true` at `:563` → change to
`ResponsesMode: routing.EndpointModePassthrough`. (`NewTestServer` at
`server_test.go:4560` seeds a translate app that sets **no** native flag; after
the rename its modes are the zero value `""`, which the decision treats as
translate — see the empty-mode note in Task 04.2 — so it needs no change beyond
compiling. If Area 01 requires non-empty modes on every app, set both to
`routing.EndpointModeTranslate` there.)

No behavioural assertion of its own — this task is green when the package builds
and the two pre-existing passthrough/translate sibling tests still pass:

```
go test ./internal/gateway/ -run 'TestOpenAIResponsesNativePassthroughProxiesRawBody|TestOpenAIResponsesNonNativeUsesTranslatePath' -count=1
```

### Task 04.1 — `endpointModeFor` maps flavor → (path, effective mode)

**Failing test** (new, `native_passthrough_test.go`):

```go
func TestEndpointModeForMapsFlavorToTargetMode(t *testing.T) {
	target := routing.Target{
		Provider:      routing.ProviderVLLM,
		ResponsesMode: routing.EndpointModePassthrough,
		MessagesMode:  routing.EndpointModeDisabled,
	}
	cases := []struct {
		apiFlavor string
		wantPath  string
		wantMode  routing.EndpointMode
	}{
		{"openai_responses", "/v1/responses", routing.EndpointModePassthrough},
		{"anthropic_messages", "/v1/messages", routing.EndpointModeDisabled},
		{"openai_chat_completions", "", ""}, // neither coding-agent endpoint
	}
	for _, tc := range cases {
		t.Run(tc.apiFlavor, func(t *testing.T) {
			path, mode := endpointModeFor(target, tc.apiFlavor)
			if path != tc.wantPath || mode != tc.wantMode {
				t.Fatalf("endpointModeFor(%q) = (%q, %q), want (%q, %q)", tc.apiFlavor, path, mode, tc.wantPath, tc.wantMode)
			}
		})
	}
}
```

Run — expect **compile failure** (`endpointModeFor` undefined), then FAIL→PASS:
```
go test ./internal/gateway/ -run TestEndpointModeForMapsFlavorToTargetMode -count=1
```

**Minimal implementation** — replace `nativePassthroughEnabled`
(`native_passthrough.go:31-43`):

```go
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
```

And update `upstreamPath` (`native_passthrough.go:66`) — only `passthrough` uses
the native path:

```go
	if p, mode := endpointModeFor(target, apiFlavor); mode == routing.EndpointModePassthrough {
		return p
	}
```

### Task 04.2 — the three-way decision + disabled rejection (ordinary app)

**Failing tests** (new, `native_passthrough_test.go`) — a disabled sibling for
each endpoint, plus a "translate still translates" and "passthrough still
proxies" table (the latter two mostly re-cover the existing siblings but pin the
mode-explicit helper). Key new coverage is the **disabled rejection body/code**:

```go
// TestOpenAIResponsesDisabledEndpointRejects proves a resolved target whose
// EFFECTIVE ResponsesMode is disabled is REJECTED with the stable code + 4xx and
// is NOT translated: the simple body used here WOULD translate+succeed (200) if it
// fell through, so a 404 proves the disabled branch fired, not the parser.
func TestOpenAIResponsesDisabledEndpointRejects(t *testing.T) {
	prov := &recordingProxyProvider{respBody: "unused"}
	srv := newNativeModeTestServer(prov, routing.EndpointModeDisabled, routing.EndpointModeTranslate)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gw-model","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "responses.endpoint_disabled" {
		t.Fatalf("error code = %q, want responses.endpoint_disabled", code)
	}
	if prov.proxyCalls != 0 {
		t.Fatalf("ProxyNative calls = %d, want 0 (disabled must not proxy)", prov.proxyCalls)
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1 (the rejection is recorded)", len(events))
	}
	if events[0].Status != "error" || events[0].ErrorCode != "responses.endpoint_disabled" {
		t.Fatalf("usage event = %+v, want status=error error_code=responses.endpoint_disabled", events[0])
	}
	if events[0].HTTPStatus != http.StatusNotFound {
		t.Fatalf("usage http_status = %d, want 404", events[0].HTTPStatus)
	}
}

// TestAnthropicMessagesDisabledEndpointRejects — the /v1/messages mirror.
func TestAnthropicMessagesDisabledEndpointRejects(t *testing.T) {
	prov := &recordingProxyProvider{respBody: "unused"}
	srv := newNativeModeTestServer(prov, routing.EndpointModeTranslate, routing.EndpointModeDisabled)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gw-model","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "messages.endpoint_disabled" {
		t.Fatalf("error code = %q, want messages.endpoint_disabled", code)
	}
	if prov.proxyCalls != 0 {
		t.Fatalf("ProxyNative calls = %d, want 0", prov.proxyCalls)
	}
	events := srv.Usage.All()
	if len(events) != 1 || events[0].ErrorCode != "messages.endpoint_disabled" {
		t.Fatalf("usage events = %+v, want one messages.endpoint_disabled error", events)
	}
}

// TestResponsesEndpointModeTable pins all three modes on the ordinary app for the
// Responses endpoint in one place.
func TestResponsesEndpointModeTable(t *testing.T) {
	cases := []struct {
		mode          routing.EndpointMode
		wantStatus    int
		wantProxy     int
		wantErrorCode string // "" => success
	}{
		{routing.EndpointModePassthrough, http.StatusOK, 1, ""},
		{routing.EndpointModeTranslate, http.StatusOK, 0, ""},
		{routing.EndpointModeDisabled, http.StatusNotFound, 0, "responses.endpoint_disabled"},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"}
			srv := newNativeModeTestServer(prov, tc.mode, routing.EndpointModeTranslate)
			// A SIMPLE body so the translate path can succeed (proves translate != reject).
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gw-model","input":"hi"}`))
			req.Header.Set("Authorization", "Bearer dev-secret")
			rec := httptest.NewRecorder()

			srv.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("mode %s: status = %d, want %d, body=%s", tc.mode, rec.Code, tc.wantStatus, rec.Body.String())
			}
			if prov.proxyCalls != tc.wantProxy {
				t.Fatalf("mode %s: proxyCalls = %d, want %d", tc.mode, prov.proxyCalls, tc.wantProxy)
			}
			if tc.wantErrorCode != "" && errorBodyOf(t, rec) != tc.wantErrorCode {
				t.Fatalf("mode %s: error code = %q, want %q", tc.mode, errorBodyOf(t, rec), tc.wantErrorCode)
			}
		})
	}
}
```

Run — expect **fail** (disabled currently falls through to translate → the
Responses simple body translates and returns 200, not 404), then PASS:
```
go test ./internal/gateway/ -run 'TestOpenAIResponsesDisabledEndpointRejects|TestAnthropicMessagesDisabledEndpointRejects|TestResponsesEndpointModeTable' -count=1 -v
```

**Minimal implementation** — replace the decision block
(`native_passthrough.go:153-166`) with the three-way switch, and register the
codes/status + helper. All required imports (`apierror`, `provider`, `net/http`,
`log/slog`, `routing`) are already present in the file:

```go
	path, mode := endpointModeFor(target, apiFlavor)
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
```

New registrations (top of `native_passthrough.go`, near `jsonContentType` at
`:29`):

```go
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
```

### Task 04.3 — server_agent: effective mode = the resolved spec's value

At **this** layer the decision is source-agnostic: it reads
`Target.ResponsesMode`/`MessagesMode`. Two complementary coverages:

1. **Owned by Area 01 (resolver test):** for a `server_agent` app whose resolved
   spec has `ResponsesMode = disabled` while the parent app has
   `ResponsesMode = passthrough`, `targetFrom`/`Resolve` yields a Target with
   `ResponsesMode = disabled` (spec wins). Cross-reference in the plan so it is
   not dropped; it does not belong in Area 04.

2. **Owned by Area 04 (handler integration), added once Area 01 ships a
   server_agent seed helper:** a `server_agent` app with `ResponsesMode =
   passthrough` at the app but a resolved spec `ResponsesMode = disabled` still
   gets **404 `responses.endpoint_disabled`** end-to-end (proving the handler
   honors the spec-derived effective mode, not the app's). Skeleton:

```go
// Requires Area 01's server_agent + runtime-spec memory-store seed helper (e.g.
// newServerAgentModeTestServer(appMode, specMode routing.EndpointMode)).
func TestServerAgentResponsesDisabledSpecWinsRejects(t *testing.T) {
	prov := &recordingProxyProvider{respBody: "unused"}
	srv := newServerAgentModeTestServer(t, prov, routing.EndpointModePassthrough, routing.EndpointModeDisabled) // app=passthrough, spec=disabled
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gw-model","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound || errorBodyOf(t, rec) != "responses.endpoint_disabled" {
		t.Fatalf("status/code = %d/%s, want 404/responses.endpoint_disabled (spec disabled must win over app passthrough)", rec.Code, rec.Body.String())
	}
	if prov.proxyCalls != 0 {
		t.Fatalf("proxyCalls = %d, want 0", prov.proxyCalls)
	}
}
```

Run (after Area 01):
```
go test ./internal/gateway/ -run TestServerAgentResponsesDisabledSpecWinsRejects -count=1
```

### Task 04.4 — full-package + module gates

```
go test ./internal/gateway/ -count=1        # this area
make test-go                                 # from repo root (all Go packages)
```

Expect green. `provider_path_test.go` (`TestProviderPathRecordedForTranslate`
`:21`, `TestProviderPathEmptyOnResolveFailure` `:62`) exercises `upstreamPath` via
the full server and must stay green — the `upstreamPath` change keeps the
translate/empty behavior identical (only `passthrough` returns the native path,
same as `native==true` before).

---

## 4. GOTCHAS (do not miss)

- **Compile coupling on Area 01.** The seed helpers set the removed
  `NativeResponses`/`NativeMessages` on `routing.Application` in **three** places —
  `newNativeProxyTestServer` (`server_test.go:1932`), `newCapAdmissionTestServer`
  (`native_passthrough_test.go:563`), and (implicitly, via the zero value) any app
  seeded without a native flag. The whole `gateway` package will not build until
  Area 01 renames the Application fields **and** these helpers are updated
  (Task 04.0). Sequence Area 01 first.
- **Empty-mode = translate.** A Target that carries `EndpointMode("")` (an app
  seeded without an explicit mode — e.g. `NewTestServer`) must be treated as
  translate by the decision (`default:` arm) and by `upstreamPath` (only
  `== EndpointModePassthrough` uses the native path). Do **not** treat `""` as
  disabled — that would reject legitimate translate traffic. Post-migration
  (spec §7) production rows are always backfilled non-empty; `""` is a
  test/defensive path only.
- **Disabled is rejected AFTER resolve, inside `tryProxyNative` — not in
  `inferencePreflight`.** The per-model disabled state is only knowable once the
  model resolves to a Target (critical for `server_agent`, where the spec carries
  it). Keep it out of the pre-resolve gate.
- **Must NOT fall through to translate on disabled.** Returning `false` from
  `tryProxyNative` hands the request to `compat.Parse*` + the translate path. The
  disabled branch must return `true` (handled). The disabled tests deliberately
  use a **simple** body that *would* translate+succeed (200) so a regression to
  fall-through is caught as a 200 instead of 404.
- **Usage recording on rejection is a deliberate choice** (mirrors the
  admission-timeout terminal branch at `native_passthrough.go:137-144`, which
  records). Records against the **resolved** `target` (server/model attribution),
  `status="error"`, `ContentType=jsonContentType`, `HTTPStatus=statusEndpointDisabled`.
  If the spec owner prefers the pre-upstream-gate posture (no usage row, like
  `model.not_allowed`), drop the `recordUsage` line and the two usage assertions —
  flag this in the plan. (Recommended: record, for operator visibility of rejected
  Codex/Claude traffic.)
- **HTTP status is an open spec item (§13).** Recommended **404**; the const
  `statusEndpointDisabled` isolates the choice. The 403 alternative aligns with
  `model.not_allowed`/`server_override.forbidden`. Whatever is chosen, update the
  usage-event `HTTPStatus` assertion and the `api-surface.md` doc row together.
- **Two independent "off" signals.** An `openai_responses` request can be off
  because the `openai` flavor is unchecked (coarse gate — Area 01 candidate
  eligibility) **or** because `ResponsesMode = disabled` (this area, post-resolve).
  Plain `openai_chat_completions` must stay served when only the Responses
  endpoint is disabled — Area 04 never touches the chat path (`handleOpenAIChat`
  at `inference_handlers.go:23` never calls `tryProxyNative`), so this holds by
  construction. Ensure Area 01's candidate-eligibility refinement does not remove
  chat eligibility for a disabled Responses endpoint.
- **No provider/agent/wire change.** `endpointModeFor` reads only the Target;
  `proxyNative`/`compat` are untouched. No `AgentRuntimeSpecDTO`/`runtime.Spec`
  field, no ServerAgent version bump (spec §9).
- **`completionHTTPStatus`/`completionErrorCode` stay untouched** — the disabled
  code is a policy 4xx written inline, not a provider/routing error mapped there.
- **Rename ripple:** `nativePassthroughEnabled` → `endpointModeFor` has exactly two
  call sites, both in `native_passthrough.go` (`:66`, `:153`). Refresh the two
  stale doc blocks that say "native passthrough enabled"/"native-flagged" at
  `native_passthrough.go:31-34` and `:55-61`, and the debug log at `:159-163` that
  reads the removed bool fields.
