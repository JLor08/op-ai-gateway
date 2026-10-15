# Design: per-token Claude Code reasoning-effort compatibility (#230)

Status: draft for review. Branch-local (removed before the PR; durable content
folds into `docs/architecture/`).

## 1. Goal

Add an opt-in **per-token** setting `claude_effort_compatibility` (default off)
that makes Claude Code's reasoning-effort selection (sent as Anthropic Messages
`output_config.effort`, e.g. `/effort low|medium|xhigh`) actually control the
reasoning effort of **chat-template-driven local backends** (Qwen 3.8 on
llama.cpp and vLLM), on both the native Messages-passthrough path and the
Anthropic→OpenAI translate path. Independent of `claude_model_aliases`.

## 2. The decisive research finding (why the design is one mechanism)

Qwen's chat template — including the deployed fix
(`froggeric/Qwen-Fixed-Chat-Templates`, `chat_template.jinja`) — reads effort
**only** from the jinja variable `reasoning_effort`, which servers populate from
**`chat_template_kwargs`**. A jinja template cannot read a raw request field. So
for effort to take effect, the value must reach the backend as
`chat_template_kwargs.reasoning_effort`. Verified per backend:

| Backend / endpoint | What sets Qwen's `reasoning_effort` | Status |
|---|---|---|
| llama.cpp native `/v1/messages` | `chat_template_kwargs.reasoning_effort` ONLY (`output_config.effort` + top-level `reasoning_effort` are dropped by its Anthropic→OAI converter at pinned `08698c81`) | verified (source) |
| llama.cpp `/v1/chat/completions` | top-level `reasoning_effort` OR `chat_template_kwargs.reasoning_effort` (parser maps top-level into kwargs) | verified |
| vLLM `/v1/chat/completions` | `chat_template_kwargs.reasoning_effort` always; top-level only on current/main builds | kwargs verified; top-level version-dependent |
| vLLM native `/v1/messages` | its Anthropic endpoint understands `output_config.effort` on capable builds; `chat_template_kwargs` robust | version-gated |
| Ollama | no reliable per-level effort for Qwen; its Anthropic route has no effort field; no `chat_template_kwargs` injection | out of scope |

**Conclusion:** deliver effort as `chat_template_kwargs.reasoning_effort` on
**both** paths, for the `chat_template_kwargs`-capable backends (**llama.cpp and
vLLM**). Top-level `reasoning_effort` is additionally sent on the translate path
(harmless, and sufficient for llama.cpp / newest vLLM), but is **not enough on
its own** for vLLM, so it is not the primary lever.

## 3. Scope

**In:** the token setting (user + service tokens) with **management-only**
authorization; a resolved-type backend selector (llama.cpp + vLLM, direct and
agent-managed); native-passthrough injection of `chat_template_kwargs.reasoning_effort`;
translate-path emission of the same via the OpenAI-compatible provider; reading
`output_config.effort`; validation and the strict on/off contract; portal UI +
i18n; docs/OpenAPI.

**Out:** Ollama effort; a thinking on/off switch or `thinking.budget_tokens`
translation; a graphical effort selector in Claude Desktop; changing native
Anthropic / other native providers; any gateway-side remapping of effort names.

## 4. Design

### 4.1 Token setting `claude_effort_compatibility` (bool, default off)

Mirror the `claude_model_aliases` boolean end to end (same plumbing, **separate
and independent** switch; enabling aliases must not enable this):
`store.TokenRecord` + `auth.Token`, DTOs (`TokenDTO`/`ServiceTokenDTO` bool;
`CreateTokenRequest`/`CreateServiceTokenRequest` plain bool; `UpdateTokenRequest`
`*bool` PATCH-preserve), create/update handlers, memory/SQLite/Postgres stores,
**migration v89** (v88 is the current tail), run-as-token propagation
(`AuthorizeRunAsToken`), and the portal token editors with German/English i18n
(label "Claude Code effort compatibility"; help text noting the upstream model
must support it). Settable at user-token create/update and service-token create.

### 4.2 Management-only authorization (the new security requirement)

Unlike `claude_model_aliases`, changing this setting must be a **management**
operation: an authorized portal **session** (cookie + CSRF) OR an **admin-scope
bearer** — never an inference-only bearer (`gateway:use`/`llm:invoke`), including
on its own token, and including creation with the opt-in enabled. Today
`CreateToken`/`UpdateToken` gate only on `requireWebScope(gateway:use)`, which
accepts a user bearer (every user bearer carries `gateway:use`) — insufficient.

Enforce in the **service layer** (single chokepoint, before any write), only
when the new flag is being set/changed:

- predicate: allow iff `owner.ID == "" || isAdmin(owner)` — a session/loopback
  principal has `ID == ""` (`sessionPrincipal`), a bearer has `ID != ""`;
  `isAdmin` = `HasScope("admin")`. CSRF is already enforced for sessions inside
  `authenticateWebSession`, so no re-check.
- `CreateToken` (only when `req.ClaudeEffortCompatibility` is true), `UpdateToken`
  (only when the `*bool` is non-nil), and `CreateServiceToken` (when true).
- reject with `portal.ErrPrincipalForbidden` → HTTP 403 `principal_forbidden`,
  **before** mutating `record` or writing. Service tokens are already excluded by
  `requireWebScope`'s `IsService` rejection.
- wiring: add an `errors.Is(err, portal.ErrPrincipalForbidden)` row to the
  hand-inlined error map in the `CreateToken` POST handler (it otherwise falls
  through to 500); `UpdateToken`/rotate already map 403 via `writePortalTokenError`.

`claude_model_aliases` keeps its current (non-management-gated) behavior.

### 4.3 Backend selector — `chat_template_kwargs`-capable, by resolved type

Effort delivery applies only to backends whose chat template reads
`chat_template_kwargs.reasoning_effort`: **llama.cpp and vLLM**, direct and
agent-managed. Determine from the **resolved** `routing.Target`, mirroring the
`wantsResponsesLiveTimings` precedent:

```
kind := target.Provider
if target.Provider == routing.ProviderServerAgent {
    kind = target.LiveProgressSpecType   // "" for a direct provider app
}
capable := kind == routing.ProviderLlamaCPP || kind == routing.ProviderVLLM
```

This accepts a direct `llama_cpp`/`vllm` app and an agent-managed `llama_cpp`/`vllm`
runtime, and excludes `llama_swap` (proxies to an arbitrary downstream),
`ollama`, generic OpenAI-compatible, and native Anthropic. **Never** infer from
alias name, model family, or the presence of `/v1/messages`. Add a small helper
(e.g. `targetUsesChatTemplateKwargs(target)`), pinned by a cross-vocabulary test
like the live-timings one.

### 4.4 Reading `output_config.effort`

`output_config` has **zero** existing footprint (it is a new field; note
`reasoning.effort`→`ReasoningEffort` on the OpenAI/Responses path is unrelated).
Read it losslessly where each path needs it, via a typed sub-decode so absent /
wrong-type / present-object are distinguishable:

- Value is a JSON **string**; **trim** surrounding whitespace; forward the name
  **unchanged** (no Qwen enum, no `high`→`xhigh` mapping — the template aliases
  itself). Missing / null / blank ⇒ **no supplied effort** (inject nothing, no
  default). Unrelated `output_config` properties are ignored.
- Malformed `output_config.effort` (non-string) on an **applicable, opted-in**
  route ⇒ invalid-request **before** any upstream call. Do not apply this
  validation to unaffected routes or when the flag is off.

### 4.5 Native Messages-passthrough path (primary)

In `tryProxyNative`/`proxyNative` (`internal/gateway/native_passthrough.go`),
after target resolution and only when: the token flag is on, the resolved target
is `chat_template_kwargs`-capable (§4.3), and `MessagesMode == passthrough` — add
a **third value-lossless body edit** (after the model-rewrite and
`timings_per_token` edits, before dispatch), using the same `map[string]any` +
`json.Decoder` with `UseNumber` + `json.Marshal` idiom and returning a **fresh
slice** (never mutate `rel.raw`, which is re-read for payload capture):

- read `output_config.effort`; if absent/blank ⇒ no-op (forward unchanged).
- merge into `chat_template_kwargs.reasoning_effort`, **preserving** unrelated
  `chat_template_kwargs`. If the same value already exists ⇒ do nothing; if a
  **conflicting** value exists ⇒ invalid-request; if existing `chat_template_kwargs`
  is a **non-object** ⇒ invalid-request.
- native invalid-request rejections use `writeJSONCaptured` + `recordUsage`
  (like the existing `EndpointModeDisabled` rejection), **not** `writeRequestError`,
  so the rejection is captured and shows in Activity/Logs. Use a stable new code
  (e.g. `messages.invalid_output_config`).
- Keep `output_config`, `thinking`, messages, tools and everything else intact
  (apart from the existing model rewrite). Native Anthropic and other native
  providers retain their original passthrough — the capability + mode gates
  exclude them. **Update** the two "exactly two edits" contract comments and the
  `nativeRelay` doc to describe the third edit.

### 4.6 Translate path (Anthropic Messages → OpenAI `/v1/chat/completions`)

- Parser: add `ParseAnthropicMessagesWithOptions(raw, opts)` with
  `opts.EffortTranslationEnabled bool`; `ParseAnthropicMessages` delegates with
  `false` (and `CountAnthropicTokens` stays false). When **on**, read
  `output_config.effort` (§4.4) and set `req.ReasoningEffort` (neutral, as the
  existing Responses parser does). When **off**, do **not** read `output_config`
  at all — exactly today's tolerant behavior (a request with `output_config` must
  not be newly rejected). The gateway handler computes the token flag and passes it.
- Emission of `chat_template_kwargs`: after target resolution, a provider-neutral
  policy step (mirroring `applyAnthropicCachePolicy`) sets an **internal directive**
  (`json:"-"`) on `inference.Request` when the resolved target is
  `chat_template_kwargs`-capable (§4.3) and `req.ReasoningEffort` is set via this
  feature. The OpenAI-compatible provider honors it by emitting
  `chat_template_kwargs.reasoning_effort` (merging with any existing kwargs, same
  preserve/no-op/conflict/non-object rules as §4.5) **in addition to** the
  existing top-level `reasoning_effort` (from `req.ReasoningEffort`). A
  **direct** OpenAI chat request that happens to set `reasoning_effort` does
  **not** get the directive, so its behavior is unchanged (top-level only). If
  the translation target uses OpenAI **Responses**, the existing `reasoning.effort`
  rendering is preserved (local Qwen is chat-completions, so this is an edge case).

### 4.7 What is unchanged

Native Anthropic and other native Messages passthrough; Ollama; generic
OpenAI-compatible and direct OpenAI chat/Responses requests; streaming vs
non-streaming parity; routing, model permissions/allowlists, provisioning,
quotas, and requested/served-model attribution. With the flag **off**, every path
retains its exact previous behavior. The native llama.cpp adaptation is kept
**separate** from the generic inference model and applied after resolution,
before dispatch; a passthrough route is never forced into translation.

## 5. Key design decisions

- **One mechanism, both paths:** `chat_template_kwargs.reasoning_effort` is the
  only field that reaches Qwen's template on every backend/version; it is applied
  natively (body edit) on passthrough and via the provider on translate.
- **Generalized to llama.cpp + vLLM** (not llama.cpp only) via a resolved-type
  selector — the user's explicit requirement; the selector cleanly excludes
  Ollama/llama_swap/generic-OpenAI/native-Anthropic.
- **Forward effort verbatim** — the Qwen template aliases names itself; a
  different Qwen template might only read low/medium/high (documented limitation).
- **Management-only auth** enforced in the service layer with
  `owner.ID=="" || isAdmin`, independent of the alias flag.
- **Provider-neutral request** — effort rides as neutral `ReasoningEffort` plus
  an internal directive (like `PromptCache`); the parser gets a bool option, never
  token storage.

## 6. Integration points (map)

- Setting plumbing: `internal/store/{models.go,sqlite_token.go,migrate.go}` (v89),
  `internal/auth/token_store.go`, `internal/portal/{service.go,service_services.go,
  memory_directory.go}`, `internal/gateway/portal_token_endpoints.go` (CreateToken
  error-map row), frontend `src/components/TokenList.tsx` / `ServiceTokensSection.tsx`,
  `src/api/{tokens,services}.ts`, `src/i18n.ts`.
- Auth gate: `internal/portal/service.go` (CreateToken/UpdateToken), `service_services.go`
  (CreateServiceToken), `internal/portal/authz.go` (`isAdmin`, `ErrPrincipalForbidden`).
- Selector: `internal/routing/store.go` (provider consts), `internal/routing/resolver.go`
  (`Target.Provider`/`MessagesMode`/`LiveProgressSpecType`), model on
  `internal/gateway/responses_live_timings.go`.
- Native path: `internal/gateway/native_passthrough.go` (`proxyNative` edits; the
  two-edits contract comments), `internal/gateway/inference_handlers.go`.
- Translate path: `internal/compat/anthropic.go` (`ParseAnthropicMessagesWithOptions`),
  `internal/inference/types.go` (`ReasoningEffort` + the new internal directive),
  `internal/provider/openai_compatible.go` (emit `chat_template_kwargs`),
  a policy step akin to `internal/gateway/anthropic_cache_policy.go`.

## 7. Testing strategy

TDD. Must map to the issue's acceptance criteria, and the primary case is a
**native** one:

- Setting lifecycle across memory/SQLite/Postgres (round-trip, omitted-PATCH
  preserve, restart, rotation, run-as); alias opt-in and effort opt-in independent.
- **Authorization:** an inference-only bearer cannot create-with-opt-in or
  update the flag (on itself or another token) → 403, no write; a session and an
  admin-bearer succeed.
- **Primary native llama.cpp test:** an opted-in request to a resolved llama.cpp
  passthrough target produces an upstream **Messages** request containing
  `chat_template_kwargs.reasoning_effort` (not an OpenAI request), with
  `output_config`/`thinking`/messages/tools preserved; same/conflicting existing
  kwargs follow the contract; non-object kwargs → invalid-request. Cover vLLM
  passthrough too. Streaming and non-streaming.
- **Translate path:** an opted-in request to a resolved llama.cpp/vLLM translate
  target produces an OpenAI chat request carrying `chat_template_kwargs.reasoning_effort`
  (and top-level `reasoning_effort`); low/medium/xhigh captured; streaming both ways;
  direct model IDs, generated aliases, and manual overrides.
- **Selector:** llama_swap, ollama, generic OpenAI-compatible and native Anthropic
  targets get **no** adaptation; agent-managed llama.cpp/vLLM do.
- **On/off + validation:** flag off ⇒ a request carrying `output_config` is
  unchanged and never rejected; missing/null/blank ⇒ no effort, no default;
  malformed effort on an applicable route ⇒ invalid-request with no upstream call;
  native Anthropic/direct OpenAI unaffected.
- **Manual smoke** against the deployed llama.cpp build + froggeric template:
  verify the template actually consumes the injected value (not just that the
  gateway sends it).

## 8. Documentation

Update `compatibility-and-inference.md` (the two delivery paths + the
`chat_template_kwargs` contract + backend scope), `routing-and-model-selection.md`
(the resolved-type selector), `security-auth-rbac.md` (the management-only setting
+ auth predicate), `persistence.md` + `reference/data-model.md` (migration v89),
`reference/api-surface.md` + OpenAPI (the token field; the native body addition),
and a doc of the per-session `/effort` commands + Qwen levels (low/medium/xhigh,
xhigh default; `/effort auto` is unreliable) and the "upstream must support it" /
"thinking on-off is separate" caveats.

## 9. Limitations

Only chat-template-kwargs-capable backends (llama.cpp, vLLM) are covered; Ollama
is out of scope. Top-level `reasoning_effort` alone is unreliable on released vLLM
(hence `chat_template_kwargs`). A Qwen chat template that does not alias effort
names would see the raw value; the gateway does not remap. An upstream llama.cpp
fix that maps `output_config.effort` itself would be an alternative to the native
adaptation; verify against the deployed build/template before relying on either.
