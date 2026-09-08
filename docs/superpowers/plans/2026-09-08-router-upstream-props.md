# Router upstream /props passthrough (issue #58) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The server-agent's runtime router gains `GET /upstream/{model}/props` (GET-only, allowlist exactly `/props`, never starts a child), and the gateway's `{model}`-template app-health probe pass extends to `server_agent` applications with per-mapping `SpecUpstreamAuth` — so an api-key-protected llama.cpp child's `timings_per_token` verdict is finally recoverable.

**Architecture:** One new case in the router's hand-written ServeHTTP switch dispatches on path prefix/suffix (model ids may contain `/`), resolves the child via `Manager.Status()` only, and relays a plain GET round-trip byte-verbatim. Gateway-side, the existing `{model}` context/live-progress pass gets an implicit probe path for `server_agent` apps, gated fail-closed on the new agent-declared feature `runtime_upstream_props`, and builds its upstream-auth context per mapping instead of per application. A typed `provider.ErrAuthRejected` distinguishes "wrong token" from "upstream down".

**Tech Stack:** Go 1.26 (two modules: `op-ai-server-agent`, `op-ai-gateway`), stdlib HTTP, existing house test patterns (httptest children, numeric hit counters, `newHealthTestStore`/`fakeProber`).

**Spec:** `docs/superpowers/specs/2026-09-08-router-upstream-props-design.md` (approved). Issue: #58.

## Global Constraints

- **Never touch:** the agent loopback probe (`server-agent/internal/collector/probe.go` — conclusive set `{404,401,403,405}`, `(verdict, stable)` contract), both `detectLiveProgressSupport` copies (`probe.go:286`, `gateway/backend/internal/provider/model_info.go:165` — byte-identical, and this change touches neither), `writeBackRuntimeLiveProgress`, `UpdateMappingLiveProgressSupport`'s SQL, the router's body-dispatch (`serveProxy`) contract. No store columns, no migration, no frontend change.
- New sentinel codes, exact strings: `runtime.model_not_running` (404), `runtime.upstream_endpoint_not_allowed` (404). Existing codes reused exactly: `runtime.model_not_managed` (404), `runtime.upstream_gone` (502).
- Feature name, exact string: `runtime_upstream_props`. Agent version: `0.6.0` → `0.7.0`. Implicit gateway probe path, exact string: `/upstream/{model}/props`.
- Outbound path from the router to the child is **exactly `/props`** — never the inbound `/upstream/...` path. Response relayed unmodified (the `"role":"router"` detector gate depends on the child's own document).
- `EnsureRunning` is never called on the new route — no start, no `inFlight`/`lastUsed` touch.
- Logging idioms: `log.Printf` in `cmd/gateway/app_health.go`, `slog` in `internal/gateway` and server-agent — never mix.
- Every new/changed test must FAIL with its production change reverted (revert-verification); run it before claiming the task done.
- Per-module CI gates before every commit: `~/go/bin/golangci-lint fmt --diff` (must be empty) and `~/go/bin/golangci-lint run` in the touched module dir, plus `go test ./...` for that module. Known pre-existing failure to ignore: `go test ./internal/gateway/ -race` fails on `TestPushRuntimeConfigNeverPushesTheEmptyDocumentOnAStoreFailure` (issue #53).
- Branch `router-upstream-props`, worktree `/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/router-upstream-props`. Never commit to `main`. Never use bare `git stash`.

---

### Task 1: The router route — `GET /upstream/{model}/props`

**Files:**
- Modify: `server-agent/internal/runtime/router.go` (package doc ~:4-21, imports ~:81-95, ServeHTTP switch :275-291, new handler after `serveModels`)
- Test: `server-agent/internal/runtime/router_test.go`

**Interfaces:**
- Consumes: `managerPort.Status() []Status` (already in the interface, :232-236), `hopByHopHeaders`, `forwardUpstreamResponse`, `writeError`, `rt.transport`.
- Produces: the route itself — Task 4's gateway pass probes it; Task 6 documents it.

- [ ] **Step 1: Write the failing tests**

Add to `router_test.go`. A new fake (the house pattern — hand-written, no framework):

```go
// statusManager is a managerPort fake for the /upstream/{model}/props route:
// Status() is fixed, and EnsureRunning must never be called (the route's
// whole contract is that a probe never starts a child) -- ensures counts it.
type statusManager struct {
	statuses []Status
	ensures  atomic.Int64
}

func (m *statusManager) EnsureRunning(context.Context, string) (string, func(), error) {
	m.ensures.Add(1)
	return "", nil, ErrModelNotManaged
}
func (m *statusManager) LoadedModels() []string { return nil }
func (m *statusManager) Status() []Status       { return m.statuses }
```

Helper to decode the error envelope (reuse the test file's existing decode idiom if one exists; otherwise):

```go
func decodeErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body %q)", err, rec.Body.String())
	}
	return env.Error.Code
}
```

The tests:

```go
// TestRouterUpstreamPropsForwardsToRunningChild: the headline case. A running
// child receives GET at path EXACTLY /props with the inbound Authorization
// and custom token header intact; the child's body/status come back verbatim;
// EnsureRunning is never called. The model id carries a slash (HF style) --
// the prefix/suffix decomposition this route exists to get right.
func TestRouterUpstreamPropsForwardsToRunningChild(t *testing.T) {
	var hits atomic.Int32
	var gotPath, gotAuth, gotKey string
	child := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotPath, gotAuth, gotKey = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"default_generation_settings":{"params":{"timings_per_token":true}}}`))
	}))
	defer child.Close()
	port := childPort(t, child.URL) // helper below

	m := &statusManager{statuses: []Status{{SpecID: "s1", Model: "org/model", State: StateRunning, Port: port}}}
	rt := newRouter(m)

	req := httptest.NewRequest(http.MethodGet, "/upstream/org/model/props", nil)
	req.Header.Set("Authorization", "Bearer spec-tok")
	req.Header.Set("X-Api-Key", "raw-tok")
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("child hits = %d, want 1", got)
	}
	if gotPath != "/props" {
		t.Fatalf("child saw path %q, want exactly /props (the inbound /upstream path must never be forwarded)", gotPath)
	}
	if gotAuth != "Bearer spec-tok" || gotKey != "raw-tok" {
		t.Fatalf("credentials not forwarded verbatim: Authorization=%q X-Api-Key=%q", gotAuth, gotKey)
	}
	if want := `{"default_generation_settings":{"params":{"timings_per_token":true}}}`; rec.Body.String() != want {
		t.Fatalf("body not relayed verbatim: %q", rec.Body.String())
	}
	if n := m.ensures.Load(); n != 0 {
		t.Fatalf("EnsureRunning called %d times, want 0 (the probe must never start a child)", n)
	}
}

// childPort extracts the loopback port an httptest server listens on, so a
// Status entry can point the route at it.
func childPort(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse httptest URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse httptest port: %v", err)
	}
	return port
}

// TestRouterUpstreamPropsColdModelNeverStarts: a managed-but-cold spec
// answers 404 runtime.model_not_running, and the REAL manager still reports
// it StateStopped afterwards -- asserting on the manager, not just the
// response (the TestRouterModelsListsAllManagedSpecs pattern).
func TestRouterUpstreamPropsColdModelNeverStarts(t *testing.T) {
	m := newTestManager(t, allowlistPolicy())
	spec := baseSpec("s1", "cold-model")   // adapt to the file's real helpers
	applySpecs(t, m, spec)                 // adapt: however sibling tests install specs
	rt := newRouter(m)

	req := httptest.NewRequest(http.MethodGet, "/upstream/cold-model/props", nil)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeErrorCode(t, rec); code != "runtime.model_not_running" {
		t.Fatalf("code = %q, want runtime.model_not_running", code)
	}
	for _, st := range m.Status() {
		if st.State != StateStopped {
			t.Fatalf("spec %s is %q after the probe, want %q -- the probe started a child", st.SpecID, st.State, StateStopped)
		}
	}
}

// TestRouterUpstreamPropsUnknownModelAndNilManager: both answer the existing
// runtime.model_not_managed, and so does an empty model segment.
func TestRouterUpstreamPropsUnknownModelAndNilManager(t *testing.T) {
	cases := []struct {
		name string
		m    managerPort
		path string
	}{
		{"unknown model", &statusManager{}, "/upstream/nope/props"},
		{"nil manager", nil, "/upstream/nope/props"},
		{"empty model", &statusManager{}, "/upstream//props"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newRouter(tc.m)
			rec := httptest.NewRecorder()
			rt.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if code := decodeErrorCode(t, rec); code != "runtime.model_not_managed" {
				t.Fatalf("code = %q, want runtime.model_not_managed", code)
			}
		})
	}
}

// TestRouterUpstreamPropsAllowlistRefusesOtherEndpoints: /props is the entire
// allowlist. A running child must see ZERO traffic for a refused path.
func TestRouterUpstreamPropsAllowlistRefusesOtherEndpoints(t *testing.T) {
	var hits atomic.Int32
	child := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer child.Close()
	m := &statusManager{statuses: []Status{{SpecID: "s1", Model: "m", State: StateRunning, Port: childPort(t, child.URL)}}}
	rt := newRouter(m)

	for _, path := range []string{"/upstream/m/completion", "/upstream/m/props/", "/upstream/props", "/upstream/m/metrics"} {
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", path, rec.Code)
		}
		if code := decodeErrorCode(t, rec); code != "runtime.upstream_endpoint_not_allowed" {
			t.Fatalf("%s: code = %q, want runtime.upstream_endpoint_not_allowed", path, code)
		}
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("child hits = %d, want 0 (a refused path must never reach the child)", got)
	}
}

// TestRouterUpstreamPropsNonGETFallsThroughToBodyDispatch: the M9 contract --
// the new case is isGet-guarded, so a POST at the same path lands in
// serveProxy and gets the body-dispatch 404, NOT the allowlist refusal.
func TestRouterUpstreamPropsNonGETFallsThroughToBodyDispatch(t *testing.T) {
	rt := newRouter(&statusManager{})
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/upstream/m/props", strings.NewReader(`{}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeErrorCode(t, rec); code != "runtime.model_not_managed" {
		t.Fatalf("code = %q, want runtime.model_not_managed (serveProxy's empty-model answer -- proof of fall-through)", code)
	}
}

// TestRouterUpstreamPropsUpstreamGoneOnDialFailure: the Status snapshot can
// race an idle drain -- a dead port answers 502 runtime.upstream_gone.
func TestRouterUpstreamPropsUpstreamGoneOnDialFailure(t *testing.T) {
	child := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	port := childPort(t, child.URL)
	child.Close() // the port is now dead
	m := &statusManager{statuses: []Status{{SpecID: "s1", Model: "m", State: StateRunning, Port: port}}}
	rt := newRouter(m)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/upstream/m/props", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if code := decodeErrorCode(t, rec); code != "runtime.upstream_gone" {
		t.Fatalf("code = %q, want runtime.upstream_gone", code)
	}
}
```

Adapt the two `// adapt` lines in the cold-model test to the file's real manager-construction helpers (`newTestManager`, `baseSpec`, and however sibling tests like `TestRouterModelsListsAllManagedSpecs` install specs — read that test at `router_test.go:221` first and copy its setup verbatim). If stubchild tests self-skip on this platform, keep the cold-model test on the real manager anyway — it never starts the child, so the stub binary is never executed; if the manager cannot even be constructed without it, fall back to a `statusManager` carrying a `StateStopped` status with `Port: 0` and keep the `ensures == 0` assertion as the no-start proof.

- [ ] **Step 2: Run the tests, verify they fail**

Run: `cd server-agent && go test ./internal/runtime/ -run TestRouterUpstreamProps -v`
Expected: every new test FAILS — today these paths fall into `serveProxy` and answer `runtime.model_not_managed` (so the allowlist, not-running, forward, and upstream-gone tests all fail on wrong code/status).

- [ ] **Step 3: Implement the route**

In `router.go`:

1. Imports: add `"strconv"` and `"strings"`.
2. Package doc (~:4-21): the "Three route classes" list gains a fourth bullet between the control paths and the catch-all:

```go
//   - GET /upstream/{model}/props -- the GET-only, allowlisted passthrough
//     to a RUNNING managed child's /props (issue #58): model from the PATH
//     (prefix/suffix decomposition, so ids containing "/" work), resolved
//     via Status() only -- NEVER EnsureRunning, a probe must not start or
//     keep alive a child -- and relayed byte-verbatim so the gateway's
//     evidence rule reads the child's own document. The allowlist is
//     exactly /props; widening it is #49-2/#55 business.
```

3. ServeHTTP switch: update the M9 comment ("all three control paths" → "the control paths"), and add the case after `/v1/models`:

```go
	case isGet && strings.HasPrefix(r.URL.Path, "/upstream/"):
		rt.serveUpstreamProps(w, r)
```

4. The handler, after `serveModels`:

```go
// serveUpstreamProps handles GET /upstream/{model}/props (issue #58): the
// llama-swap-style upstream passthrough, restricted to a GET of exactly
// /props on a RUNNING child. The gateway probes an api-key-protected child's
// live-progress capability through it, attaching the spec's token -- which
// this handler, like the proxy paths, forwards verbatim (Authorization and
// custom token headers are not hop-by-hop).
//
// Deliberate properties, each one a guardrail from the issue:
//   - Status() only, never EnsureRunning: llama-swap's own /upstream route
//     boots a cold model on contact -- a probe that starts children is a
//     hazard, not a feature. No inFlight/lastUsed touch either, so the
//     probe never keeps an idle child alive.
//   - Prefix/suffix decomposition, not segment matching: upstream model ids
//     are only TrimSpace-validated and may contain "/" (HF-style
//     "org/model"); everything between "/upstream/" and the trailing
//     "/props" is the model. provider.ExpandModelPath on the gateway side
//     keeps "/" literal, so the two ends agree.
//   - The outbound path is EXACTLY /props -- never the inbound path -- and
//     the response is relayed unmodified: the evidence rule's
//     "role":"router" gate must see the child's own document.
//   - Managed-but-cold gets its own sentinel (runtime.model_not_running):
//     §4.3's error codes are never collapsed, and "no spec" vs "not
//     running right now" are different diagnoses.
func (rt *router) serveUpstreamProps(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/upstream/")
	if !strings.HasSuffix(rest, "/props") {
		writeError(w, http.StatusNotFound, "runtime.upstream_endpoint_not_allowed",
			"only /props may be requested through /upstream/{model}")
		return
	}
	model := strings.TrimSuffix(rest, "/props")
	if model == "" || rt.m == nil {
		writeError(w, http.StatusNotFound, "runtime.model_not_managed",
			"no active launch spec for this model")
		return
	}
	port, found := 0, false
	for _, st := range rt.m.Status() {
		if st.Model != model {
			continue
		}
		found = true
		if st.State == StateRunning && st.Port != 0 {
			port = st.Port
			break // first running entry wins (byUpstream dispatch collapses duplicates the same way)
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "runtime.model_not_managed",
			"no active launch spec for this model")
		return
	}
	if port == 0 {
		writeError(w, http.StatusNotFound, "runtime.model_not_running",
			"model is managed but not running; this probe never starts a child")
		return
	}
	target := "http://127.0.0.1:" + strconv.Itoa(port) + "/props"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "runtime.upstream_gone", err.Error())
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
		writeError(w, http.StatusBadGateway, "runtime.upstream_gone", err.Error())
		return
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close, best-effort
	forwardUpstreamResponse(w, nil, resp)
}
```

(If gocritic/gofumpt object to the `port, found := 0, false` tuple or the nolint placement, follow the linter — behavior over style.)

- [ ] **Step 4: Run the tests, verify they pass**

Run: `cd server-agent && go test ./internal/runtime/ -v -run TestRouter`
Expected: all new tests PASS, all existing `TestRouter*` tests still PASS (the new case must not shadow any existing dispatch — the four control paths and the proxy fall-through are covered by existing tests).

- [ ] **Step 5: Revert-verify**

`git stash` is FORBIDDEN (shared stack). Instead: `git diff server-agent/internal/runtime/router.go > /tmp/t1.patch && git checkout -- server-agent/internal/runtime/router.go`, run the new tests (expect FAIL each for the right reason — wrong code/status, zero forwards), then `git apply /tmp/t1.patch` and re-run (PASS).

- [ ] **Step 6: Lint + full module test + commit**

```bash
cd server-agent && ~/go/bin/golangci-lint fmt --diff && ~/go/bin/golangci-lint run && go test ./...
```

Commit:

```bash
git add server-agent/internal/runtime/router.go server-agent/internal/runtime/router_test.go
git commit -m "feat(agent): GET /upstream/{model}/props -- the router's allowlisted upstream passthrough

The GET-only, never-starting probe route for issue #58: model from the
path (prefix/suffix decomposition so HF-style ids with slashes work),
child resolved via Status() only, request headers forwarded verbatim
minus hop-by-hop, outbound path exactly /props, response relayed
unmodified. New sentinels runtime.model_not_running and
runtime.upstream_endpoint_not_allowed; dial failures map to the existing
runtime.upstream_gone."
```

---

### Task 2: Agent version 0.7.0 + declared feature `runtime_upstream_props`

**Files:**
- Modify: `server-agent/internal/agent/agent.go` (Version doc block + const, ~:95-113)
- Modify: `server-agent/internal/agent/features.go` (registry tail, after `runtime_model_probe` ~:141)
- Test: `server-agent/internal/agent/features_test.go` (or the file that already asserts the registry — find it with `grep -rn "runtime_model_probe" server-agent/internal/agent/*_test.go` and extend in place)

**Interfaces:**
- Produces: the feature name `runtime_upstream_props` (exact string) that Task 4's gateway gate checks; wire-carried via the existing `capabilitiesJSON()` — no plumbing change.

- [ ] **Step 1: Write the failing test**

```go
// TestFeaturesDeclareRuntimeUpstreamProps pins the #58 capability: the
// gateway's app-health pass fail-closed-gates its /upstream/{model}/props
// probing on this exact name, so a rename here silently turns that probing
// off for every agent.
func TestFeaturesDeclareRuntimeUpstreamProps(t *testing.T) {
	found := false
	for _, f := range Features {
		if f.Name == "runtime_upstream_props" {
			found = true
			if f.Since != "0.7.0" {
				t.Fatalf("runtime_upstream_props Since = %q, want 0.7.0", f.Since)
			}
		}
	}
	if !found {
		t.Fatal("Features does not declare runtime_upstream_props")
	}
	if Version != "0.7.0" {
		t.Fatalf("Version = %q, want 0.7.0 (one bump per shipped change; 0.6.0 has shipped)", Version)
	}
}
```

- [ ] **Step 2: Run it, verify it fails** — `cd server-agent && go test ./internal/agent/ -run TestFeaturesDeclareRuntimeUpstreamProps -v` → FAIL ("does not declare").

- [ ] **Step 3: Implement**

`features.go`, appended after the `runtime_model_probe` entry, in the registry's comment style:

```go
	// runtime_upstream_props: this agent's runtime router serves
	// GET /upstream/{model}/props -- the GET-only, allowlisted passthrough to
	// a RUNNING managed child's /props (issue #58; it never starts a child,
	// and the allowlist is exactly /props). Unlike most entries above, the
	// GATEWAY gates real behavior on this flag, fail-closed (the
	// PushRuntimeConfig precedent): its app-health {model} probe pass only
	// sends /upstream/{model}/props probes at an agent with positive
	// evidence the route exists -- an older agent would answer 404
	// runtime.model_not_managed for every such probe, forever.
	//
	// Since is 0.7.0, this branch's single bump (see agent.go's Version
	// block): the rule is one bump per SHIPPED CHANGE, never per commit, and
	// the binary that first carries this name is the same 0.7.0 that first
	// carries the route.
	{Name: "runtime_upstream_props", Since: "0.7.0"},
```

`agent.go`: extend the Version doc block with the bump paragraph (matching the existing 0.5.0→0.6.0 paragraph's shape) and flip the constant:

```go
// 0.6.0 -> 0.7.0 is the single bump for the router-upstream-props branch
// (issue #58): the runtime router now serves GET /upstream/{model}/props,
// the GET-only allowlisted passthrough the gateway probes an
// api-key-protected child's live-progress capability through.
// agent.Features declares "runtime_upstream_props", MINOR -- and unlike
// the earlier portal-informational flags, the gateway genuinely gates on
// this one (fail-closed), so deploying this agent is what turns the
// gateway-side probing on.
const Version = "0.7.0"
```

- [ ] **Step 4: Run it, verify it passes** — same command, PASS; then `go test ./internal/agent/ ./internal/runtime/` (existing feature-count/capabilities tests may pin the registry — if one asserts an exact feature list, extend that list, never weaken the assertion).

- [ ] **Step 5: Revert-verify** — revert `features.go` + `agent.go` (same patch/checkout dance as Task 1 Step 5), new test FAILS, re-apply, PASSES.

- [ ] **Step 6: Lint + commit**

```bash
cd server-agent && ~/go/bin/golangci-lint fmt --diff && ~/go/bin/golangci-lint run && go test ./...
git add server-agent/internal/agent/agent.go server-agent/internal/agent/features.go server-agent/internal/agent/features_test.go
git commit -m "feat(agent): version 0.7.0 declares runtime_upstream_props

The gateway fail-closed-gates its /upstream/{model}/props probing on this
declared capability (the PushRuntimeConfig precedent), so the bump is what
turns gateway-side capability probing on for this agent."
```

---

### Task 3: `provider.ErrAuthRejected` — a typed 401/403 sentinel

**Files:**
- Modify: `gateway/backend/internal/provider/client.go` (`unavailableStatus`, :33-42)
- Test: `gateway/backend/internal/provider/client_test.go` (create if absent; check `ls gateway/backend/internal/provider/*_test.go` first and land beside the existing tests for this file if one exists)

**Interfaces:**
- Produces: `provider.ErrAuthRejected` (exported), unwrapping to `ErrUnavailable` — Task 5's misconfig log consumes it via `errors.Is`.

- [ ] **Step 1: Write the failing test**

```go
// TestUnavailableStatusTagsAuthRejection pins the #58 contract: 401/403 are
// programmatically distinguishable (the app-health pass logs a
// misconfigured-token signal on them) while still being ErrUnavailable to
// every existing caller; nothing else carries the tag.
func TestUnavailableStatusTagsAuthRejection(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		err := unavailableStatus(status)
		if !errors.Is(err, ErrAuthRejected) {
			t.Fatalf("status %d: not ErrAuthRejected: %v", status, err)
		}
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("status %d: lost the ErrUnavailable wrap: %v", status, err)
		}
	}
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		if errors.Is(unavailableStatus(status), ErrAuthRejected) {
			t.Fatalf("status %d must NOT be ErrAuthRejected", status)
		}
	}
	if !errors.Is(unavailableStatus(http.StatusServiceUnavailable), ErrUpstreamStarting) {
		t.Fatal("503 lost its ErrUpstreamStarting tag")
	}
}
```

- [ ] **Step 2: Run it, verify it fails** — `cd gateway/backend && go test ./internal/provider/ -run TestUnavailableStatusTagsAuthRejection -v` → compile error (`ErrAuthRejected` undefined) counts as the failing state.

- [ ] **Step 3: Implement**

In `client.go`, below `ErrUpstreamStarting`:

```go
// ErrAuthRejected is the subset of ErrUnavailable meaning the upstream
// REFUSED the request's credential: 401 (Unauthorized) or 403 (Forbidden).
// It unwraps to ErrUnavailable, so every existing errors.Is(err,
// ErrUnavailable) check is unaffected -- the same wrapping contract as
// ErrUpstreamStarting above. The app-health probe pass uses it to tell
// "the runtime spec's API token is wrong or missing" (an operator
// misconfiguration worth a distinct log line, issue #58) apart from "the
// upstream is down" -- string-matching the status out of the error text
// would be fragile. Build the wrapped error with unavailableStatus so
// 401/403 get this tag.
var ErrAuthRejected = fmt.Errorf("%w (upstream rejected the credential)", ErrUnavailable)
```

and rewrite `unavailableStatus`:

```go
// unavailableStatus wraps a non-2xx upstream status as ErrUnavailable,
// tagging a 503 additionally as ErrUpstreamStarting (a retryable "still
// loading" signal) and a 401/403 as ErrAuthRejected (a credential the
// upstream refused).
func unavailableStatus(status int) error {
	base := ErrUnavailable
	switch status {
	case http.StatusServiceUnavailable:
		base = ErrUpstreamStarting
	case http.StatusUnauthorized, http.StatusForbidden:
		base = ErrAuthRejected
	}
	return fmt.Errorf("%w: upstream status %d", base, status)
}
```

- [ ] **Step 4: Run it, verify it passes**; then `go test ./internal/provider/` (no existing caller may change behavior — every `errors.Is(…, ErrUnavailable)` stays true).

- [ ] **Step 5: Revert-verify** — revert `client.go`; the test must fail to COMPILE (that is a valid revert-failure); restore.

- [ ] **Step 6: Lint + commit**

```bash
cd gateway/backend && ~/go/bin/golangci-lint fmt --diff && ~/go/bin/golangci-lint run && go test ./internal/provider/
git add gateway/backend/internal/provider/client.go gateway/backend/internal/provider/client_test.go
git commit -m "feat(gateway): unavailableStatus tags 401/403 as provider.ErrAuthRejected

Typed sibling of ErrUpstreamStarting so the app-health pass can log a
misconfigured-token signal without string-matching error text (#58).
Unwraps to ErrUnavailable; no existing caller changes behavior."
```

---

### Task 4: Gateway probe pass — implicit path + fail-closed feature gate

**Files:**
- Modify: `gateway/backend/cmd/gateway/app_health.go` (consts near :27-30; `agentRegistryBundle` :105; `agentRegistries.agentFeatures` inline interface :143 + new method; the ctx pass gate :580-596 and the goroutine's path uses :611-655, :684+)
- Test: `gateway/backend/cmd/gateway/app_health_test.go`

**Interfaces:**
- Consumes: `runtime_upstream_props` (Task 2's exact string), `routing.ProviderServerAgent`, the runner's existing `agents agentRegistryBundle` (production already carries the `agentFeaturesRegistry` with `Has(serverID, feature) bool`).
- Produces: `hasAgentFeature(agents, serverID, feature) bool` helper; the goroutine signature `go func(app routing.Application, probePath string)` and `probePath` replacing every `app.ContextProbePath` read inside the pass — Task 5 builds on this exact shape.

- [ ] **Step 1: Write the failing tests**

First read `TestRunAppHealthOnceContextProbeTemplateProbesLoadedOnly` (`app_health_test.go:575`) and `TestRunAppHealthOnceLiveProgressSupportPersistsOnceNotAgainOnAnIdenticalCycle` (:710) and copy their setup (store/prober/loaded-registry/runner construction) verbatim — the snippets below name only what differs. A fake bundle:

```go
// fakeAgentBundle satisfies agentRegistryBundle for feature-gate tests:
// ReportingWithin/Retain are inert, HasFeature answers from a static map.
type fakeAgentBundle struct{ features map[string][]string }

func (f fakeAgentBundle) ReportingWithin(string, time.Duration) bool { return false }
func (f fakeAgentBundle) Retain(map[string]struct{})                 {}
func (f fakeAgentBundle) HasFeature(serverID, feature string) bool {
	for _, name := range f.features[serverID] {
		if name == feature {
			return true
		}
	}
	return false
}
```

The tests (names final, bodies adapted from the two templates):

1. `TestRunAppHealthOnceServerAgentImplicitPropsPathProbesPerLoadedMapping` — app `Type: routing.ProviderServerAgent`, `ContextProbePath: ""`, bundle declares `runtime_upstream_props` for the server; loaded registry reports `"up"` loaded; `prober.modelInfoByPath["/upstream/up/props"]` returns `[]provider.ModelInfo{{Name: "up", LiveProgressSupport: "supported"}}`. Assert: `prober.probedPath("/upstream/up/props")`, `st.liveProgressSetCount() == 1`; run an identical second cycle (advance the fake clock past the cadence, the :710 template's trick) → still `== 1`.
2. `TestRunAppHealthOnceServerAgentWithoutFeatureNeverProbes` — same, but the bundle map is empty (and a second subtest: bundle nil). Assert `prober.ctxProbeCallCount() == 0` and `st.liveProgressSetCount() == 0`.
3. `TestRunAppHealthOnceServerAgentOperatorPathWins` — same app but `ContextProbePath: "/custom/{model}/info"`, feature declared. Assert `prober.probedPath("/custom/up/info")` and NOT `prober.probedPath("/upstream/up/props")`.
4. `TestRunAppHealthOnceNonServerAgentNeverGetsImplicitPath` — a `llama_swap`-typed app with empty `ContextProbePath`, feature (nonsensically) declared for the server. Assert `ctxProbeCallCount() == 0` — the implicit path is type-gated, not feature-only.

- [ ] **Step 2: Run them, verify they fail** — `cd gateway/backend && go test ./cmd/gateway/ -run 'TestRunAppHealthOnceServerAgent|TestRunAppHealthOnceNonServerAgent' -v`. Tests 1/3 fail (no probe happens with an empty path today — and 3 actually PASSES today; that is fine, it is the pin that Task 4 must not break; note it in the report). Tests 2/4 may pass trivially before the change — after implementing, revert-verification is what proves they bite (Step 5).

- [ ] **Step 3: Implement**

1. Consts (beside `appHealthRetryGap`):

```go
// runtimeUpstreamPropsFeature is the agent-DECLARED capability naming the
// runtime router's GET /upstream/{model}/props passthrough (issue #58). The
// {model} probe pass below only sends such probes at an agent with positive
// evidence the route exists -- fail-closed, the PushRuntimeConfig precedent
// -- because an older agent answers 404 runtime.model_not_managed for every
// such probe, forever, and silent no-op traffic each cadence tick is
// exactly what a capability gate exists to prevent.
const runtimeUpstreamPropsFeature = "runtime_upstream_props"

// serverAgentPropsProbePath is the implicit {model}-template probe path for
// a server_agent application whose operator left app.ContextProbePath
// empty: the agent's router forwards it to the RUNNING child's /props with
// the request's credential intact, so the pass recovers the live-progress
// verdict of an api-key-protected child (issue #58) -- the case the agent's
// own token-less loopback probe conclusively cannot determine. An
// operator-set ContextProbePath always wins over this default.
const serverAgentPropsProbePath = "/upstream/{model}/props"
```

2. `agentRegistryBundle` gains a method (extend the interface's doc comment's list of duties too):

```go
type agentRegistryBundle interface {
	ReportingWithin(serverID string, window time.Duration) bool
	Retain(live map[string]struct{})
	// HasFeature reports whether serverID's agent declared feature in its
	// last telemetry sample. false is the fail-closed default (nil registry,
	// never-reported server) -- the same contract as
	// agentFeaturesRegistry.Has, which backs it in production.
	HasFeature(serverID, feature string) bool
}
```

3. `agentRegistries.agentFeatures`'s inline structural interface (:143) grows `Has(serverID, feature string) bool` (the unexported concrete type already satisfies it — the same structural-satisfaction reasoning its comment documents), plus:

```go
func (a agentRegistries) HasFeature(serverID, feature string) bool {
	if a.agentFeatures == nil {
		return false // nil interface field: same fail-closed default as the registry itself
	}
	return a.agentFeatures.Has(serverID, feature)
}
```

4. Nil-bundle accessor beside `reportingWithin`:

```go
// hasAgentFeature is the nil-guarded accessor mirroring reportingWithin: a
// nil bundle (tests that pass no registries) means no evidence, so false.
func hasAgentFeature(agents agentRegistryBundle, serverID, feature string) bool {
	if agents == nil {
		return false
	}
	return agents.HasFeature(serverID, feature)
}
```

5. The ctx pass gate (:584-586) becomes:

```go
			probePath := strings.TrimSpace(app.ContextProbePath)
			if probePath == "" && app.Type == routing.ProviderServerAgent &&
				hasAgentFeature(r.agents, server.ID, runtimeUpstreamPropsFeature) {
				// server_agent implicit default (issue #58): probe the router's
				// GET-only /props passthrough per loaded mapping. Fail-closed on
				// the agent's declared capability -- an agent without the route
				// would 404 every probe -- and an operator-set ContextProbePath
				// above always wins.
				probePath = serverAgentPropsProbePath
			}
			if !hasCtxProber || probePath == "" {
				continue
			}
```

6. Thread `probePath` into the goroutine: `go func(app routing.Application, probePath string) { … }(app, probePath)`, and replace the three `app.ContextProbePath` reads inside it (`strings.Contains(…)`, `provider.ExpandModelPath(…)`, and the single-probe `ProbeModelInfo(pctx, target, app.ContextProbePath)`) with `probePath`. No other line in the goroutine changes in this task.

7. Every fake `agentRegistryBundle` in the test file gains `HasFeature(string, string) bool { return false }` (find them: `grep -n "ReportingWithin" cmd/gateway/*_test.go`); tests that pass a nil bundle need nothing.

- [ ] **Step 4: Run the tests, verify they pass** — the four new tests plus the full package: `go test ./cmd/gateway/`. Existing tests must pass with NO assertion edits (fakes gaining the new method is the only permitted edit — a changed assertion means the pass's non-server_agent behavior drifted).

- [ ] **Step 5: Revert-verify** — revert `app_health.go` only (keep the test file): tests 1 and 3's probe assertions FAIL, test 2/4 still pass (they pin absence — their bite is proven the other way: temporarily hard-code `probePath = serverAgentPropsProbePath` without the feature/type conditions and watch 2 and 4 fail; then restore the real implementation).

- [ ] **Step 6: Lint + commit**

```bash
cd gateway/backend && ~/go/bin/golangci-lint fmt --diff && ~/go/bin/golangci-lint run && go test ./cmd/gateway/
git add gateway/backend/cmd/gateway/app_health.go gateway/backend/cmd/gateway/app_health_test.go
git commit -m "feat(gateway): the {model} probe pass covers server_agent apps through the router

Implicit probe path /upstream/{model}/props for a server_agent application
with no operator-set ContextProbePath, gated fail-closed on the agent's
declared runtime_upstream_props capability (the PushRuntimeConfig
precedent). Operator-set paths win; non-server_agent apps are untouched."
```

---

### Task 5: Per-mapping `SpecUpstreamAuth` + the misconfigured-token log

**Files:**
- Modify: `gateway/backend/cmd/gateway/app_health.go` (`healthStore` :34-47; the ctx-pass goroutine's auth block :607-609 and the `{model}` branch's probe/error handling)
- Test: `gateway/backend/cmd/gateway/app_health_test.go`

**Interfaces:**
- Consumes: Task 3's `provider.ErrAuthRejected`; Task 4's `probePath` goroutine shape; `routing.SpecUpstreamAuth(spec, app) (token, header string)` (`runtime_api_token_mode.go:35`, returns the SEALED token); `capture.OpenSecret`; store method `RuntimeSpecsByApplication(ctx, appID) ([]routing.RuntimeSpec, error)` (already on `*store.SQLiteStore` `sqlite_runtime.go:98-148` and `*routing.MemoryStore` `memory_store.go:2326`; each `RuntimeSpec` carries `MappingID`).
- Produces: nothing downstream — this completes the gateway side.

- [ ] **Step 1: Write the failing tests**

1. `TestRunAppHealthOnceServerAgentProbeCarriesSpecToken` — the wire-level credential test (the ctx-carried auth is unexported, so assert on the wire, not the context). Shape:
   - An `httptest.Server` standing in for the agent router: it records `r.Header.Get("Authorization")` and `r.Header.Get("X-Api-Key")` per request path and answers a minimal `/upstream/<model>/props` body: `{"default_generation_settings":{"params":{"timings_per_token":true}}}`.
   - A REAL prober, not `fakeProber`: construct the same provider client the multiplexer wires for `server_agent` (read `cmd/gateway/main.go`'s `providerClients` ~:1244-1259 for the exact constructor — it is the OpenAI-compatible client — and build it with `http.DefaultClient` or the httptest client).
   - Store/server/app arranged so `routing.ApplicationEndpoint(server, app)` resolves to the httptest URL (parse the URL; set `server.Domain` to its host, `app.Port` to its port, `app.Scheme` `"http"` — read how `ApplicationEndpoint` composes at `internal/routing/store.go:769-790` and mirror).
   - Two mappings with two specs: mapping A's spec `{MappingID: mpA.ID, APITokenMode: "set", APIToken: "plain:tok-a"}` (default header ⇒ expect `Authorization: Bearer tok-a` on `/upstream/a/props`); mapping B's spec `{MappingID: mpB.ID, APITokenMode: "set", APIToken: "plain:tok-b", APITokenHeaderSource: "custom", APITokenHeader: "X-Api-Key"}` (expect raw `tok-b` under `X-Api-Key` on `/upstream/b/props`, and NO Authorization header). Verify `capture.OpenSecret(nil, "plain:…")` returns the plaintext (read its contract first; if a cipher is required, build the test cipher the way sibling tests in this package do — `grep -n "capture.NewCipher\|OpenSecret" cmd/gateway/*_test.go`).
   - `fakeHealthStore` gains a `runtimeSpecs []routing.RuntimeSpec` field and the method `RuntimeSpecsByApplication(ctx, appID)` returning it (plus a `specsErr` toggle for test 3).
   - Assert both headers arrived as expected AND `liveProgressSetCount() == 2`.
2. `TestRunAppHealthOnceServerAgentAuthRejectedLogsAndNeverWrites` — `fakeProber` gains a per-path error injector: `modelInfoErrValue map[string]error`; when set for a path, `ProbeModelInfo` returns `(nil, thatError)`. Inject `fmt.Errorf("wrapped: %w", provider.ErrAuthRejected)` for `/upstream/up/props`. Capture stdlib log: `var buf bytes.Buffer; log.SetOutput(&buf); t.Cleanup(func() { log.SetOutput(os.Stderr) })`. Assert `liveProgressSetCount() == 0` and `strings.Contains(buf.String(), "check the runtime spec's API token")`.
3. `TestRunAppHealthOnceServerAgentSpecReadFailureFallsBackToAppToken` — `specsErr` set; probing proceeds (probe count ≥ 1); with the app carrying `APIToken: "plain:app-tok"`, the wire test's httptest handler (reuse the shape from test 1) sees `Authorization: Bearer app-tok` — the zero-value-spec fallback is `SpecUpstreamAuth`'s documented app-token behavior.

- [ ] **Step 2: Run them, verify they fail** — compile failure first (`RuntimeSpecsByApplication` missing on `healthStore` fakes is NOT yet the production interface — the fake method alone compiles; the assertions then fail because the pass still sends the app token / doesn't log). Get each to its meaningful failure.

- [ ] **Step 3: Implement**

1. `healthStore` gains (with the interface's comment style):

```go
	// RuntimeSpecsByApplication lists the runtime specs joined to the app's
	// mappings (RuntimeSpec.MappingID keys back to the mapping). The {model}
	// pass uses it to build PER-MAPPING upstream credentials for a
	// server_agent application (routing.SpecUpstreamAuth -- the resolver and
	// benchmark-runner precedent) instead of the app-level token: each
	// mapping's child can carry its own api key (issue #58).
	RuntimeSpecsByApplication(ctx context.Context, appID string) ([]routing.RuntimeSpec, error)
```

2. In the goroutine, directly after the existing app-token `pctx` block (:607-609), load the specs once per app:

```go
				// For a server_agent application the credential is per MAPPING
				// (issue #58): each mapping's spec can carry its own upstream
				// token, and the router forwards whatever header we attach
				// verbatim to the child. Loaded once per app; a read error
				// degrades to the empty map -- every lookup then yields the
				// zero-value spec, whose SpecUpstreamAuth answer is the
				// documented app-token fallback, i.e. exactly the pre-#58
				// behavior of this pass.
				specByMapping := map[string]routing.RuntimeSpec{}
				if app.Type == routing.ProviderServerAgent {
					specs, serr := r.store.RuntimeSpecsByApplication(ctx, app.ID)
					if serr != nil {
						log.Printf("app health: runtime specs for app %s failed: %v (probing with the app token)", app.ID, serr)
					}
					for _, sp := range specs {
						specByMapping[sp.MappingID] = sp
					}
				}
```

3. In the `{model}` branch's mapping loop, before the first `ProbeModelInfo` call:

```go
						// Per-mapping credential (issue #58): SpecUpstreamAuth
						// resolves mode off/set/random/app against this mapping's
						// spec (zero-value spec => app token, the resolver's exact
						// fallback); the token is SEALED, so OpenSecret it exactly
						// like the app token above (fail-open).
						mctx := pctx
						if app.Type == routing.ProviderServerAgent {
							specToken, header := routing.SpecUpstreamAuth(specByMapping[mp.ID], app)
							specTok, _ := capture.OpenSecret(r.cipher, specToken)
							mctx = provider.WithUpstreamAuth(ctx, header, specTok)
						}
```

and use `mctx` in BOTH `ProbeModelInfo` calls of the `{model}` branch (first attempt and retry). The single-probe (non-`{model}`) branch keeps `pctx` untouched.

4. The misconfig log, in the `{model}` branch's persistent-failure arm (`if perr != nil { continue }` after the retry):

```go
						if perr != nil {
							if errors.Is(perr, provider.ErrAuthRejected) {
								// On this path a 401/403 is a MISCONFIGURED token,
								// not "cannot determine": the gateway holds the
								// credential, and an unauthenticated child ignores
								// extra tokens. Repeats each failing cycle by
								// design -- it is the operator signal, and it
								// stops when the token is fixed (issue #58).
								log.Printf("app health: model info probe for app %s model %q rejected by the upstream (401/403): check the runtime spec's API token", app.ID, mp.AppModelName)
							}
							continue
						}
```

5. Imports: `app_health.go` gains `"errors"` (if absent). Every fake `healthStore` in the test file gains the `RuntimeSpecsByApplication` method.

- [ ] **Step 4: Run the tests, verify they pass** — the three new tests plus `go test ./cmd/gateway/` (Task 4's four tests must still pass — the auth change must not alter which paths get probed).

- [ ] **Step 5: Revert-verify** — revert `app_health.go` to the Task 4 state (`git diff HEAD -- gateway/backend/cmd/gateway/app_health.go > /tmp/t5.patch; git checkout HEAD -- gateway/backend/cmd/gateway/app_health.go` — note the test file stays): test 1 fails (app token arrives instead of spec tokens), test 2 fails (no log line), test 3 fails to compile or fails on the fallback. Re-apply (`git apply /tmp/t5.patch`), all pass.

- [ ] **Step 6: Lint + commit**

```bash
cd gateway/backend && ~/go/bin/golangci-lint fmt --diff && ~/go/bin/golangci-lint run && go test ./cmd/gateway/ ./internal/provider/
git add gateway/backend/cmd/gateway/app_health.go gateway/backend/cmd/gateway/app_health_test.go
git commit -m "feat(gateway): the server_agent {model} probe carries the spec's token per mapping

SpecUpstreamAuth per mapping (the resolver/benchmark-runner precedent)
instead of the app token, sealed-token OpenSecret at the edge, app-token
fallback on a spec read failure. A 401/403 now logs the distinct
misconfigured-token signal via provider.ErrAuthRejected instead of being
indistinguishable from a down upstream (#58)."
```

---

### Task 6: Documentation — route contract, rewritten rationales, ADR-037

**Files:**
- Modify: `docs/architecture/cross-cutting/agent-runtime-manager.md` (§4.1 :830-841, §4.3 :888-897, §3.2 subsection :505-513, §10 known-gap :2830-2848, the "app-health does not special-case server_agent" line :2714-2717)
- Modify: `docs/architecture/cross-cutting/telemetry-usage-observability.md` (§8.4.3 :781-796)
- Modify: `docs/architecture/reference/api-surface.md` (§5.3 :737-757)
- Modify: `docs/architecture/09-architecture-decisions.md` (append ADR-037 after ADR-036 :648)

(Line numbers are pre-change anchors — re-locate by heading text.)

**Interfaces:** none — prose. Every claim below is implemented by Tasks 1-5; do not document anything they did not build.

- [ ] **Step 1: agent-runtime-manager.md**

- §4.1: "Four fixed GET-only paths" → "Five fixed GET-only routes"; add the table row: `GET /upstream/{model}/props` — the allowlisted upstream passthrough (issue #58): model from the path (prefix/suffix decomposition — ids may contain `/`), resolved via `Status()` only, **never** starts or keeps alive a child, request headers minus hop-by-hop forwarded verbatim (the runtime-spec token rides `Authorization`/custom header exactly as on inference), outbound path exactly `/props`, response relayed unmodified. Note this is the router's first PATH-parameter dispatch — a new contract BESIDE §4.2's byte-for-byte body-dispatch, not a change to it; non-GET on the path falls through to model routing (the existing M9 rule).
- §4.3 error table: add `runtime.model_not_running` (404 — managed but not currently running; the probe route never starts a child) and `runtime.upstream_endpoint_not_allowed` (404 — the passthrough allowlist is exactly `/props`); keep the "collapsing codes destroys diagnoses" framing.
- §3.2 (the runtime-spec-token subsection): append one sentence — the gateway's `/upstream/{model}/props` capability probe rides the same verbatim `Authorization` forwarding, which is why the api-key'd child's verdict is recoverable without the agent attaching anything.
- §10's "Known gap … issue #58" paragraph: rewrite. The gap closes via the **gateway** probing through the router's passthrough with `SpecUpstreamAuth` per mapping — NOT by threading the token into the loopback probe (the previously sketched remedy; rejected because `runtimectl.Status` is copied into reported/logged places, and the correct place to attach a credential is the gateway edge where it already happens for inference). The loopback probe stays the fast path for unprotected children; its 401/403-conclusive caching is unchanged; both writers apply the same evidence rule and converge via compare-to-stored.
- :2714-2717 ("the app-health probe does not special-case server_agent"): update — it now does, in exactly one place: the implicit `{model}` probe path + per-mapping credential, gated on the agent-declared `runtime_upstream_props`.

- [ ] **Step 2: telemetry-usage-observability.md §8.4.3**

Rewrite the "the gateway cannot reach a managed server_agent child's /props itself" rationale: it now can — `GET /upstream/{model}/props`, for agents declaring `runtime_upstream_props` — which is how an api-key-protected child (whose loopback probe conclusively refuses) gets its verdict. Keep the duplicate-detector paragraph intact (the two copies and their never-drift rule are unchanged). Preserve every existing heading/anchor (inbound links).

- [ ] **Step 3: api-surface.md §5.3**

"serves exactly four GET control paths" → five; add the route row with the never-starts + allowlist properties and the two new error codes. (No `openapi.yaml` change — the router port is deliberately outside the gateway's OpenAPI surface, per that file's scope comment.)

- [ ] **Step 4: ADR-037**

Append, in the log's context→decision→consequence shape, status Accepted:

**ADR-037 — The runtime router grows a GET-only per-model `/props` passthrough; the gateway probes through it with the spec's token.** Context: #52 left an api-key-protected child's `timings_per_token` verdict undeterminable (the loopback probe carries no credential; 401/403 are conclusive refusals) — a regression for `custom`-typed protected children. The originally sketched remedy (thread the sealed token to the agent's probe) was rejected: `runtimectl.Status` is copied into reports and logs, and the agent-holds-no-token premise turned out false anyway (the runtime-config push carries it — issue #61) — the accurate invariant is that the ROUTER injects no credential of its own and forwards `Authorization` verbatim. Decision: mirror llama-swap's `/upstream/{model}/…` shape with the guardrails llama-swap taught — GET-only, allowlist exactly `/props`, `Status()`-only resolution that never starts a child, model from the path — and let the gateway's `{model}` app-health pass probe through it, attaching `SpecUpstreamAuth` per mapping, fail-closed-gated on the agent-declared `runtime_upstream_props`. Consequence: two writers for `live_progress_support` that converge (same evidence rule, compare-to-stored, `""` never writes); a 401/403 on this path is a typed, logged operator misconfiguration (`provider.ErrAuthRejected`); allowlist widening (`/slots` for #49-2, router-mode for #55) is deliberately those issues' business.

- [ ] **Step 5: Verify docs gates**

Run: `./scripts/check-docs.sh` and `./scripts/check-docs.test.sh`
Expected: clean (anchors resolve, index reachability holds, no unregistered flag named). Also grep the two stale-count phrases are gone: `grep -rn "exactly four\|Four fixed" docs/architecture/` → no hits for the router sections.

- [ ] **Step 6: Commit**

```bash
git add docs/architecture/
git commit -m "docs: the router's /upstream/{model}/props contract, rewritten #58 rationales, ADR-037

Five control routes (§4.1 + api-surface §5.3), two new stable error
codes (§4.3), the §10 known-gap and telemetry §8.4.3 paragraphs
rewritten to the gateway-through-router design, and ADR-037 recording the
decision with the corrected token-holding premise (issue #61)."
```

---

## Final verification (after all tasks, before the PR)

- Both modules: lint + full tests (`server-agent`, `gateway/backend`), frontend untouched (`git status` shows no frontend diff), `./scripts/check-docs.sh`.
- Sonar sequence from repo root: `make sonar-up sonar-gate sonar-findings sonar-branch-findings sonar-down` — judge by `Attributed: N on lines this branch changed` (want 0).
- `docs/superpowers/` and `docs/implementation-status.md` are removed in the PR-preparation step (finishing-a-development-branch), never in these tasks.
