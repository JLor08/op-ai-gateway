# Router upstream /props passthrough — design (issue #58)

**Goal:** A `custom`-typed managed child launched with `--api-key`, whose `/props`
carries `timings_per_token`, ends with a persisted `supported` verdict — recovered
by the gateway probing through a new GET-only router route, carrying the spec's
token, which the gateway already holds and already attaches on every inference
request to the same child.

**Approved design (operator's proposal, issue #58):** the server-agent's runtime
router gains a llama-swap-style upstream passthrough, `GET /upstream/{model}/props`,
and the gateway's existing `{model}`-template app-health probe pass extends to
`server_agent` applications, attaching per-mapping `SpecUpstreamAuth` instead of the
application token.

## 1. Corrected premise (differs from the issue text)

The issue's rationale "only the gateway has the token" is factually wrong: the
gateway pushes the **decrypted** token in the runtime-config document
(`portal.Service.resolvePushToken` → `AgentRuntimeSpecDTO.APIToken`,
`service_runtime.go:1993-2019`), the agent holds it in `runtime.Spec.APIToken` for
`${API_TOKEN}` expansion, and even persists it (0600) in its runtime-config cache
(`config_client.go:294-311` — the comment-vs-persist inconsistency is issue #61,
out of scope here).

The design survives on the **accurate** rationale, and every document this change
touches must use it:

- The router **deliberately injects no auth of its own** — it only forwards the
  inbound `Authorization` header verbatim to the selected child
  (`agent-runtime-manager.md` §3.2: "no router change was needed … because the
  agent's router already forwards the inbound Authorization header verbatim").
  Credential attachment belongs at the gateway edge, where it already happens for
  inference (`Server.upstreamAuthCtx`, `server.go:1636-1654`).
- `runtimectl.Status` stays token-free: Status structs are copied into reported
  and logged places, so plumbing the token onto Status (the issue's rejected
  alternative) remains rejected — that half of the original reasoning still holds.

## 2. Agent side: the route

### 2.1 Dispatch

File: `server-agent/internal/runtime/router.go`. New case in `(*router).ServeHTTP`'s
manual switch (`router.go:275-291`), before the `serveProxy` default:

```go
case isGet && strings.HasPrefix(r.URL.Path, "/upstream/"):
    rt.serveUpstreamProps(w, r)
```

(`strings` is a new import for router.go.) The handler:

1. Path shape: must end in `/props`. `rest := strings.TrimPrefix(r.URL.Path,
   "/upstream/")`; if `!strings.HasSuffix(rest, "/props")` → **404, new sentinel
   `runtime.upstream_endpoint_not_allowed`**, message
   `only /props may be requested through /upstream/{model}` — the allowlist
   refusal the issue's verification demands. `model := strings.TrimSuffix(rest,
   "/props")`; empty model → 404 `runtime.model_not_managed` (mirrors
   serveProxy's empty-model handling).
   - Prefix/suffix decomposition — NOT segment matching — because upstream model
     ids are only TrimSpace-validated and may contain `/` (HF-style
     `org/model`); `provider.ExpandModelPath` keeps `/` literal while escaping
     each segment, so the decoded `r.URL.Path` carries the model with literal
     slashes and "everything between the prefix and the last `/props`" is the
     model, unambiguously (a model literally named `x/props` produces
     `/upstream/x/props/props` and decomposes correctly).
2. Lookup: **`rt.m.Status()` only — never `EnsureRunning`** (Status is already in
   the `managerPort` interface, `router.go:232-236`; `rt.m == nil` → 404
   `runtime.model_not_managed`). Scan for `st.Model == model` (exact match, the
   same equality `byUpstream` dispatch uses):
   - no entry → 404 `runtime.model_not_managed` (existing meaning: no active
     launch spec).
   - entry exists, but none with `st.State == StateRunning && st.Port != 0` →
     **404, new sentinel `runtime.model_not_running`**, message
     `model is managed but not running; this probe never starts a child`.
     §4.3's discipline (codes are never collapsed) is why cold gets its own code.
   - multiple specs sharing one `UpstreamModel`: first running entry wins
     (dispatch's `byUpstream` collapses to one winner anyway).
3. Forward: a dedicated plain GET round-trip (NOT `buildUpstreamRequest`, which is
   streaming-shaped; NOT `target := endpoint + r.URL.Path`, which would forward
   the `/upstream/...` path):
   - URL: `http://127.0.0.1:<st.Port>/props` — the outbound path is **exactly
     `/props`**, never the inbound path; inbound `RawQuery` forwarded verbatim.
   - Request headers: clone inbound minus `hopByHopHeaders` (existing list,
     `router.go:218-221`) minus `Accept-Encoding` (same reason
     `buildUpstreamRequest` documents: let the transport negotiate, so the body
     we relay is decoded bytes). `Authorization` — and any custom token header —
     flows through verbatim: it is absent from the strip lists, and the router
     requires no inbound auth of its own (`driver.go:611-618`), so nothing
     collides.
   - Context: the inbound `r.Context()` (the gateway bounds it with its probe
     timeout); no extra timeout of the router's own.
   - Response: status + headers minus hop-by-hop (reuse the existing
     response-side strip idiom, `router.go:837-844`), body streamed through
     unmodified — **byte-verbatim**, because the detector's `"role":"router"`
     gate depends on receiving the child's own document.
   - Dial/transport error (idle-drain race between the Status snapshot and the
     GET): the existing upstream-failure sentinel (`runtime.upstream_gone`,
     502). The gateway treats it as a transient probe failure and skips.
4. **No accounting:** no `EnsureRunning`, no `release()`, no `inFlight`/`lastUsed`
   touch — the probe never keeps an idle child alive. A model that idles out
   simply answers `runtime.model_not_running` until real traffic starts it.
5. Non-GET under `/upstream/`: NOT matched by the new case (it is `isGet`-guarded)
   — falls through to `serveProxy` like any other path, per the documented M9
   contract (a body naming a managed model gets proxied with the original
   `/upstream/...` path, which the child 404s; no body → `model_not_managed`).
   This is the existing fall-through rule, restated in docs, not new behavior.

New sentinel wiring: two entries in the router's error plumbing
(`sentinelCode`/`writeSentinelError` region, `router.go:867-916`), emitting the
existing `{"error":{"code","message"}}` envelope.

### 2.2 Version and declared feature

- `server-agent/internal/agent/agent.go:113`: `const Version = "0.6.0"` →
  `"0.7.0"`. The Version block's rule is one bump per shipped change; 0.6.0 is
  deployed, this route is the next shipped change.
- `server-agent/internal/agent/features.go`: append
  `{Name: "runtime_upstream_props", Since: "0.7.0"}` with a doc comment in the
  registry's house style. Unlike most entries (declared "for the portal's
  benefit"), this one is a **real gate**: the gateway probes
  `/upstream/{model}/props` only against agents that declare it (fail-closed,
  the `PushRuntimeConfig` precedent — never talk to an agent without positive
  evidence it understands the route; an old agent would 404 every probe
  forever).

### 2.3 Comment/doc surfaces that go stale (all updated in this change)

`router.go:9-14` package doc (control path list), `NewRouter`'s doc comment, the
ServeHTTP switch comment ("three control paths"), and the §4.1/§5.3 "four paths"
prose (see §5 below).

## 3. Gateway side: the probe pass

File: `gateway/backend/cmd/gateway/app_health.go`, the `{model}`-template branch
of the context/live-progress pass (`app_health.go:580-683`).

### 3.1 Implicit probe path, feature-gated

```go
const runtimeUpstreamPropsFeature = "runtime_upstream_props" // agent-declared
const serverAgentPropsProbePath   = "/upstream/{model}/props"
```

Effective probe path where the pass today reads `app.ContextProbePath`:

- `probePath := strings.TrimSpace(app.ContextProbePath)`; if it is empty AND
  `app.Type == routing.ProviderServerAgent` AND the agent declares
  `runtime_upstream_props`, then `probePath = serverAgentPropsProbePath`.
- An operator-set `ContextProbePath` on a `server_agent` app **wins** over the
  implicit default (least surprise; it also keeps an escape hatch).
- Everything downstream is the existing pass: cadence key `"ctx:"+app.ID`,
  loaded-set gate (unchanged — the api-key-protected running child IS in the
  loaded set, because loadedness comes from manager state via agent telemetry,
  not from probing), `provider.ExpandModelPath`, `ProbeModelInfo`, one retry
  after `appHealthRetryGap`, `PickModelContextSize` + `PickModelLiveProgressSupport`,
  compare-to-stored writes via `UpdateMappingContextProbe` (SQL-guarded by
  `metrics_locked`) and `UpdateMappingLiveProgressSupport` (deliberately
  unguarded). The context write coming along is intended: same evidence from
  the same document the agent's own probe reads; both writers compare-to-stored,
  so they converge instead of fighting.

Feature-gate plumbing: the runner's `agents agentRegistryBundle` already holds the
same `agentFeaturesRegistry` instance production wires into `ServerDeps`
(`main.go:996`, `main.go:1089`), and the registry has `Has(serverID, feature)
bool` (`runtime_registry.go`, fail-closed on nil/unknown). Widen:

- `agentRegistryBundle` interface (`app_health.go:105`) gains
  `HasFeature(serverID, feature string) bool`.
- `agentRegistries.agentFeatures`'s inline structural interface
  (`app_health.go:143`) gains `Has(serverID, feature string) bool`; the concrete
  type already satisfies it. `HasFeature` nil-guards the field (nil → false),
  and a nil-bundle helper mirrors `reportingWithin`.
- Every fake bundle in tests grows the method (false default).

### 3.2 Per-mapping auth

Today the pass builds ONE auth context per application (`app.APIToken` via
`capture.OpenSecret` + `provider.WithUpstreamAuth`, `app_health.go:607-609`),
before the mapping loop. For `server_agent` applications the credential is per
mapping:

- `healthStore` (`app_health.go:34-47`) gains
  `RuntimeSpecsByApplication(ctx context.Context, appID string)
  ([]routing.RuntimeSpec, error)` — already implemented by both `*store.SQLiteStore`
  (`sqlite_runtime.go:98-148`, one join, each `RuntimeSpec` carries `MappingID`)
  and `*routing.MemoryStore` (`memory_store.go:2326`). Every fake `healthStore`
  in `app_health_test.go` grows the method.
- In the `{model}` branch, when `app.Type == routing.ProviderServerAgent`: load
  the specs once per app into `map[mappingID]routing.RuntimeSpec` (a
  read/list error logs `log.Printf` and degrades to the empty map — see below);
  per mapping, `token, header := routing.SpecUpstreamAuth(spec, app)` (zero-value
  spec for a mapping without one → the documented app-token fallback, exactly
  `Resolver.targetFrom`'s behavior, `resolver.go:1048-1091`), then
  `tok, _ := capture.OpenSecret(r.cipher, token)` (fail-open, the existing
  idiom) and `pctx := provider.WithUpstreamAuth(ctx, header, tok)` built
  **inside** the mapping loop. Mode `off` returns an empty token and
  `WithUpstreamAuth` attaches nothing — correct for an unauthenticated child.
- Non-`server_agent` applications keep the existing once-per-app app-token
  context, byte-for-byte.

The `SpecUpstreamAuth`-over-`app.APIToken` precedent is the benchmark runner
(`benchmark_runner.go:135-158`).

### 3.3 The misconfigured-token signal

On this path a 401/403 means a **wrong/missing token** (the gateway holds the
credential; an unauthenticated child ignores extra tokens), not "cannot
determine". Two pieces:

1. `gateway/backend/internal/provider/client.go`: `unavailableStatus` gains a
   typed sentinel `ErrAuthRejected` for 401 and 403, wrapped alongside
   `ErrUnavailable` exactly the way 503 wraps `ErrUpstreamStarting` today
   (`client.go:25-42`). No behavior change for any existing caller (`errors.Is`
   checks stay true for `ErrUnavailable`). String-matching the status out of the
   error text is explicitly rejected as fragile.
2. In the `{model}` branch, after the retry also fails:
   `if errors.Is(err, provider.ErrAuthRejected)` →
   `log.Printf("app health: model info probe for app %s model %q rejected by the upstream (401/403): check the runtime spec's API token", app.ID, mp.AppModelName)`
   — `log.Printf` because that is this file's logging idiom throughout. No
   verdict is written (unchanged: probe errors skip). The line repeats each
   failing cycle (~30s) by design — it is the operator signal, and it stops when
   the token is fixed.

The **agent's loopback probe stays byte-for-byte unchanged**, including its
conclusive-refusal handling of 401/403 (the distinct log is gateway-side only —
widening `ProbeLiveProgressSupport`'s return to distinguish statuses would
change a pinned regression anchor for no consumer).

## 4. Coexistence of the two writers (spelled out)

`live_progress_support` now has two writers: telemetry ingest
(`writeBackRuntimeLiveProgress`) and the gateway pass. They are the **same
evidence rule applied to the same child document**, so no precedence is needed:

- api-key-protected child: loopback gets 401 → stable `""` → ingest never writes
  (`""` skip). Gateway probe with the token yields the verdict. No conflict —
  this is the headline case.
- unprotected child: both read the same `/props` → same verdict → compare-to-
  stored makes the second writer a no-op.
- child replaced by a different build: the loopback cache re-arms on PID change,
  the gateway probes every cycle; both converge on the current truth.

## 5. Regression anchors (unchanged by this change)

- `ProbeLiveProgressSupport`'s `(verdict, stable)` contract and conclusive set
  `{404, 401, 403, 405}` (`probe.go:237-247`); `runtimeCapabilityCache` keying
  (SpecID + PID re-arm) and caching of stable `""`.
- Both `detectLiveProgressSupport` copies stay byte-identical
  (`probe.go:286`, `model_info.go:165`), including the `"role":"router"` gate —
  this change touches neither.
- `writeBackRuntimeLiveProgress`: `""` skip, compare-to-stored, per-sample
  `runtime_model_probe` gate, cross-server Warn rejection, no `metrics_locked`.
- `UpdateMappingLiveProgressSupport` writes only `live_progress_support` +
  `live_progress_checked_at`.
- The router's body-dispatch contract (`serveProxy`, byte-for-byte forwarding,
  32MiB body cap) and the four existing control routes.
- No new store columns, no migration, no portal/frontend change (the verdict
  column shipped with #52/#59).

## 6. Documentation

All in `docs/architecture/` (check-docs gates: links/anchors resolve, index
reachability, no unregistered flags):

- `cross-cutting/agent-runtime-manager.md` §4.1: "Four fixed GET-only paths" →
  five; new route row (GET-only, allowlist exactly `/props`, never starts a
  child, model from the path with prefix/suffix decomposition). §4.2/§4.3: the
  path-dispatched GET is a **new contract beside** the byte-for-byte forwarding
  contract, documented as such; error table gains `runtime.model_not_running`
  (404) and `runtime.upstream_endpoint_not_allowed` (404). §3.2's
  token-forwarding subsection gains the pointer that the gateway's `/props`
  probe rides the same verbatim forwarding. §10's "Known gap … issue #58"
  paragraph: rewritten — the gap closes via the gateway-through-router probe,
  not via threading the token into the loopback probe.
- `cross-cutting/telemetry-usage-observability.md` §8.4.3: the "the gateway
  cannot reach a managed server_agent child's /props itself" rationale is
  rewritten (it now can, through the route, for declaring agents; the loopback
  probe remains the fast path for unprotected children).
- `reference/api-surface.md` §5.3: "exactly four GET control paths" → five; new
  row. (No `openapi.yaml` entry — the router port is deliberately outside the
  gateway's OpenAPI surface.)
- `09-architecture-decisions.md`: **ADR-037** — "The runtime router grows a
  GET-only per-model /props passthrough; the gateway probes through it with the
  spec's token" (context: #52's gap + the corrected premise from §1; decision:
  the operator's llama-swap-mirroring design with the never-start guardrail;
  consequence: two converging writers, a real capability gate, allowlist
  widening reserved for #49-2/#55).
- `reference/config-env.md`: untouched (no new env/flag).

## 7. Verification (tests; every new/changed test revert-verified)

Agent (`server-agent/internal/runtime/router_test.go`, house patterns:
httptest children / `fixedEndpointManager`-style fakes / real stubchild manager
where starting matters, numeric hit counters):

1. **Forward:** GET `/upstream/<model>/props` against a running child (httptest
   upstream + fake managerPort whose Status lists it running on that port)
   reaches the child at path exactly `/props`, with the inbound
   `Authorization` header intact and a custom token header intact; the response
   body/status arrive byte-verbatim; child hit count == 1.
2. **Never starts:** real manager (stubchild), cold spec → 404
   `runtime.model_not_running`, and `Status()` still reports `StateStopped`
   for every spec (the `TestRouterModelsListsAllManagedSpecs` assertion
   pattern) — assert on the manager, not just the response.
3. **Unknown model** → 404 `runtime.model_not_managed`; **nil manager** → same.
4. **Allowlist:** GET `/upstream/<model>/completion` → 404
   `runtime.upstream_endpoint_not_allowed`; child hit count == 0.
5. **Slash-model:** model `org/model` via `/upstream/org/model/props` resolves
   and forwards (round-trips `provider.ExpandModelPath`'s output shape).
6. **Non-GET fall-through:** POST `/upstream/<model>/props` lands in body
   dispatch (extends the `TestRouterControlPathsRequireGET` pattern).

Gateway (`gateway/backend/cmd/gateway/app_health_test.go`, house patterns:
`newHealthTestStore` + `fakeProber` + `runner.runOnce`, write-count assertions):

7. server_agent app, feature declared, empty `ContextProbePath` → prober called
   with `/upstream/<model>/props` per loaded active mapping; verdict persisted
   once, not again on an identical cycle (the
   `…PersistsOnceNotAgainOnAnIdenticalCycle` template).
8. Feature **not** declared → prober call count == 0 for that app.
9. Operator-set `ContextProbePath` on a server_agent app is used verbatim (no
   implicit override).
10. **Per-mapping credential:** one integration-shaped test using the real
    provider client against an httptest server asserting the received
    `Authorization: Bearer <spec-token>` (and a custom-header variant) — the
    ctx-carried auth is unexported, so assert on the wire, not the context.
11. **401/403:** upstream answers 401 → no verdict write (count == 0);
    `provider` unit test pins `errors.Is(unavailableStatus(401), ErrAuthRejected)`
    and 403, and that 404/500 are NOT `ErrAuthRejected`.
12. Non-server_agent apps: existing pass behavior unchanged — existing tests
    keep passing with no assertion changes (fakes growing the two new interface
    methods is the only permitted edit; a changed assertion is a red flag).

Agent features (`server-agent/internal/agent/features_test.go` or sibling):
`runtime_upstream_props` present with `Since: "0.7.0"`; `Version == "0.7.0"`.

## 8. Scope discipline

- Allowlist stays **exactly `/props`**. Widening (e.g. `/slots` for #49-2's MTP
  detection, `/metrics`) is those issues' business; the route's shape (prefix
  dispatch + endpoint allowlist) is deliberately ready for it.
- No change to the loopback probe, the evidence rule, the ingest path, the
  store schema, or the portal.
- Issue #61 (token-persist comment inconsistency) is separate.
