# Claude Code effort compatibility — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An opt-in per-token setting (`claude_effort_compatibility`, management-only) that delivers Claude Code's `output_config.effort` to Qwen on llama.cpp/vLLM as `chat_template_kwargs.reasoning_effort`, on both the native Messages-passthrough path and the Anthropic→OpenAI translate path.

**Architecture:** One mechanism — `chat_template_kwargs.reasoning_effort` (the only field Qwen's chat template reads) — applied natively (a third lossless body edit on passthrough) and via the OpenAI-compatible provider (on translate), scoped to `chat_template_kwargs`-capable backends (llama.cpp + vLLM, direct and agent-managed) by resolved target type. The setting change is a management operation (session or admin bearer).

**Tech Stack:** Go 1.26 backend (`gateway/backend`, module `op-ai-gateway`); React/TS/Vite portal; memory/sqlite/postgres stores; append-only migrations.

**Spec:** `docs/superpowers/specs/2026-10-15-claude-effort-compatibility-design.md` (read it alongside this plan — it carries the verified research and the full contract).

## Global Constraints

- TDD for every behavior change; repo-facing text (comments, tests, docs, commits) in English.
- Keep memory/SQLite/Postgres consistent; append-only versioned migration **v89** (v88 is the current tail); verify store changes with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set. A bool column is `integer not null default 0`.
- Deliver effort as `chat_template_kwargs.reasoning_effort`. Forward the effort value **verbatim** (trim whitespace only) — no Qwen enum, no `high`→`xhigh` mapping, **no default** when none supplied.
- Merge rules wherever `chat_template_kwargs.reasoning_effort` is written: preserve unrelated `chat_template_kwargs`; same value already present → no-op; conflicting value → invalid-request; existing `chat_template_kwargs` is a non-object → invalid-request.
- **Flag off ⇒ exactly today's behavior** on every path; a request carrying `output_config` must never be newly rejected when off; the new validations apply only on an applicable, opted-in route.
- The setting is **independent** of `claude_model_aliases` (separate switch; enabling aliases must not enable this).
- Changing the setting is **management-only**: a portal session (CSRF already enforced) or an admin-scope bearer; never an inference-only bearer, including on its own token, including creation-with-opt-in. Reject before any write.
- Keep the inference request **provider-neutral**: effort rides as `ReasoningEffort` + an internal `json:"-"` directive; the parser gets a bool option, never token storage.
- Native Anthropic/other native passthrough, Ollama, generic OpenAI-compatible, direct OpenAI/Responses: unchanged.
- Portal i18n: German + English keys together (`i18n.ts`, compile-time parity); add component wiring tests + an i18n.test spec.
- No ServerAgent version bump (no `server-agent/` change).

## Review Focus

Inputs the spec implies that a naive task could leave unexercised (each gets a test in the owning task):

1. **An inference-only bearer** must not enable/disable the flag — on another token OR on itself, nor create-with-opt-in — → 403, no write (T3).
2. **Selector precision:** `llama_swap`, `ollama`, generic OpenAI-compatible and native Anthropic get NO adaptation; **agent-managed** llama.cpp/vLLM DO; a **direct** llama.cpp app (LiveProgressSpecType=="") is matched via `Provider` (T5/T6/T7).
3. **Flag off / missing-blank effort:** a request with `output_config` is unchanged and never rejected when off; missing/null/blank effort injects nothing (no default) (T6/T7).
4. **Conflict / non-object `chat_template_kwargs`:** a conflicting existing value, or a non-object `chat_template_kwargs`, → invalid-request before upstream; a matching value → no-op; unrelated kwargs preserved (T6/T7).
5. **Direct OpenAI request unaffected:** a direct `/v1/chat/completions` carrying `reasoning_effort` to a llama.cpp/vLLM target must NOT get gateway-injected `chat_template_kwargs` (the directive is only set on the effort-compat translate path) (T7).

---

## File Structure & Shared Interfaces

Define in the earliest owning task; reference verbatim elsewhere.

- `store.TokenRecord.ClaudeEffortCompatibility bool`; `auth.Token.ClaudeEffortCompatibility bool`.
- DTO JSON key `claude_effort_compatibility`: `TokenDTO`/`ServiceTokenDTO` (bool), `CreateTokenRequest`/`CreateServiceTokenRequest` (bool), `UpdateTokenRequest` (`*bool`).
- Migration **v89** `token_claude_effort_compatibility` → `api_tokens.claude_effort_compatibility integer not null default 0`.
- Auth predicate helper (portal): `func principalMayManageTokenSetting(owner auth.Token) bool { return owner.ID == "" || isAdmin(owner) }`; reject with `portal.ErrPrincipalForbidden`.
- Selector (gateway): `func targetUsesChatTemplateKwargs(target routing.Target) bool` — `kind := target.Provider; if kind == routing.ProviderServerAgent { kind = target.LiveProgressSpecType }; return kind == routing.ProviderLlamaCPP || kind == routing.ProviderVLLM`.
- Parser: `type ParseAnthropicOptions struct { EffortTranslationEnabled bool }`; `func ParseAnthropicMessagesWithOptions(raw []byte, opts ParseAnthropicOptions) (inference.Request, error)`; `ParseAnthropicMessages` delegates `{}`.
- `inference.Request.ReasoningEffortViaTemplateKwargs bool` with `json:"-"` (internal directive; when true the OpenAI-compatible provider also emits `chat_template_kwargs.reasoning_effort`).
- Native helpers (gateway): `func anthropicEffortFromBody(raw []byte) (effort string, present bool, err error)` (reads `output_config.effort`, trims, `err` on non-string); `func injectChatTemplateReasoningEffort(raw []byte, effort string) ([]byte, error)` (merge rules; fresh slice). Stable error code `messages.invalid_output_config`.
- Frontend: `claude_effort_compatibility?: boolean` on `PortalToken`/`CreateTokenRequest`/`UpdateTokenRequest` (`api/tokens.ts`) and the service-token types (`api/services.ts`); i18n keys `tokenClaudeEffort` + `tokenClaudeEffortHint`.

---

## Phase A — Token setting + authorization

### Task 1: Persist the flag across all three stores (+ run-as)

**Files:** `internal/store/{models.go,sqlite_token.go,migrate.go}`, `internal/auth/token_store.go`, `internal/portal/memory_directory.go`, `internal/portal/service.go` (`AuthorizeRunAsToken`); tests in the store/portal conformance files.

**Interfaces:** produces `TokenRecord.ClaudeEffortCompatibility`, `auth.Token.ClaudeEffortCompatibility`, migration v89.

- [ ] **Step 1 — RED conformance test.** Create a token with `ClaudeEffortCompatibility: true`, read back across memory + sqlite (+ postgres with `OP_AI_GATEWAY_TEST_POSTGRES_DSN`); created without → false. Add a `TestAuthorizeRunAsTokenCarriesClaudeEffortCompatibility` beside the existing run-as tests.
- [ ] **Step 2 — Run, watch fail** (`go test ./internal/store/ ./internal/portal/ ./internal/auth/`; provision postgres).
- [ ] **Step 3 — Implement:** add the field to `TokenRecord`/`auth.Token`; register `{version: 89, name: "token_claude_effort_compatibility", up: migration89Up}` after v88, `migration89Up` = `addColumnIfMissing(..., "api_tokens", "claude_effort_compatibility integer not null default 0")` (model on `migration88Up`); add the column to `sqlite_token.go` (insert list, `tokenColumns`, `UpdateTokenMetadata` SET, `scanToken` `int64 != 0`, `LookupBearer` assembly); mirror in `memory_directory.go` (record + auth.Token at all mirror sites incl. `SetServiceTokensState`); carry it in `AuthorizeRunAsToken`.
- [ ] **Step 4 — Run, watch pass.**
- [ ] **Step 5 — Update the migration-40 rebuild test's hardcoded column list** if it lists api_tokens columns (as #225 did).
- [ ] **Step 6 — Commit** (`feat(store): persist per-token claude_effort_compatibility flag (#230)`).

### Task 2: Token API — expose the flag (create/update/read, user + service)

**Files:** `internal/portal/service.go` (`TokenDTO`, `CreateTokenRequest`, `UpdateTokenRequest`, `CreateToken`, `UpdateToken`, `tokenDTO`), `internal/portal/service_services.go` (`ServiceTokenDTO`, `CreateServiceTokenRequest`, create path, `serviceTokenDTO`); tests.

**Interfaces:** consumes Task 1; produces the DTO field `claude_effort_compatibility`.

- [ ] **Step 1 — RED:** user create+read (true/omitted→false); PATCH-preserve (omitted keeps, `*true`/`*false` change); service create+read. Model exactly on the `claude_model_aliases` DTO tests.
- [ ] **Step 2 — Watch fail** (`go test ./internal/portal/ -run Token`).
- [ ] **Step 3 — Implement** the DTO fields + record wiring + the `if req.X != nil` PATCH guard, mirroring `ClaudeModelAliases`. (The management-auth gate is Task 3 — do NOT add it here.)
- [ ] **Step 4 — Watch pass.**
- [ ] **Step 5 — Commit** (`feat(portal): expose claude_effort_compatibility on token create/update/read (#230)`).

### Task 3: Management-only authorization for the setting (security-critical)

**Files:** `internal/portal/service.go` (`CreateToken`, `UpdateToken`), `internal/portal/service_services.go` (`CreateServiceToken`), `internal/portal/authz.go` (add `principalMayManageTokenSetting` if not present; reuse `isAdmin`/`ErrPrincipalForbidden`), `internal/gateway/portal_token_endpoints.go` (CreateToken error-map row); tests.

**Interfaces:** consumes Task 2; produces the management gate.

- [ ] **Step 1 — RED (reject paths).** With a fake inference-only bearer principal (`owner.ID != ""`, `HasScope("admin") == false`): `CreateToken` with `ClaudeEffortCompatibility: true` → `ErrPrincipalForbidden`, and NOTHING written (assert the store has no new token); `UpdateToken` with `*true` (and with `*false`) on its own token → `ErrPrincipalForbidden`, record unchanged; `CreateServiceToken` with the flag true → forbidden.
- [ ] **Step 2 — RED (allow paths).** A session principal (`owner.ID == ""`) and an admin bearer (`owner.ID != "" && HasScope("admin")`) succeed in create+update. Setting an UNRELATED field (e.g. `LogCommunication`) as an inference bearer still works (gate is scoped to this flag only). Creating/updating WITHOUT touching this flag as an inference bearer still works.
- [ ] **Step 3 — Watch fail.**
- [ ] **Step 4 — Implement:** in `CreateToken`, when `req.ClaudeEffortCompatibility`, require `principalMayManageTokenSetting(owner)` else return `ErrPrincipalForbidden` BEFORE assembling/writing the record; in `UpdateToken`, guard the `if req.ClaudeEffortCompatibility != nil { ... }` block the same way, before persisting; same in `CreateServiceToken`. Add the `errors.Is(err, portal.ErrPrincipalForbidden)` row to the CreateToken POST handler's inline error map (so it returns 403, not 500).
- [ ] **Step 5 — Watch pass; gateway HTTP test** that a bearer PATCH enabling the flag returns 403 and does not change the token.
- [ ] **Step 6 — Commit** (`feat(portal): require management auth to change claude_effort_compatibility (#230)`).

### Task 4: Portal UI toggle + i18n

**Files:** `gateway/frontend/src/api/tokens.ts`, `src/api/services.ts`, `src/components/TokenList.tsx`, `src/components/ServiceTokensSection.tsx`, `src/i18n.ts`, `src/i18n.test.ts`, `TokenList.test.tsx`, `ServicesView.test.tsx`.

- [ ] **Step 1 — RED i18n test** for `tokenClaudeEffort` + hint in `de` and `en`.
- [ ] **Step 2 — Watch fail** (`cd gateway/frontend && npm ci` first — fresh worktree has no node_modules — then `npm test -- i18n`).
- [ ] **Step 3 — Implement** the checkbox (user + service create forms) mirroring the `claude_model_aliases`/`unknown_redirect` pattern (state/reset/hydrate/submit in both create+update bodies), the three TS types (+ service types in `services.ts`), and DE/EN keys (label "Claude Code effort compatibility"; hint noting the upstream model must support it).
- [ ] **Step 4 — Component wiring tests** (as #225 learned): `TokenList.test.tsx` (create body, edit hydrate, update body) + `ServicesView.test.tsx` (service create body) assert the flag is sent; select the checkbox by accessible name.
- [ ] **Step 5 — Verify** `npm test`, `npm run build`, `npm run format:check`.
- [ ] **Step 6 — Commit** (`feat(portal-ui): Claude Code effort compatibility token toggle + i18n (#230)`).

---

## Phase B — Backend selector

### Task 5: `targetUsesChatTemplateKwargs` (resolved-type, llama.cpp + vLLM)

**Files:** `internal/gateway/` (a new small file or beside `responses_live_timings.go`); test.

**Interfaces:** produces `targetUsesChatTemplateKwargs(target routing.Target) bool`.

- [ ] **Step 1 — RED table test.** True for: direct `llama_cpp` (Provider=="llama_cpp", LiveProgressSpecType==""), direct `vllm`, `server_agent` with LiveProgressSpecType "llama_cpp" or "vllm". False for: `server_agent` with "ollama"/"tgi"/other, `llama_swap`, `ollama`, generic/"" provider, native anthropic/vendor.
- [ ] **Step 2 — Watch fail.**
- [ ] **Step 3 — Implement** the helper exactly per Shared Interfaces (mirror `wantsResponsesLiveTimings`'s Provider→LiveProgressSpecType substitution). Do NOT reuse `LiveTimingsCapableKind` (different policy set).
- [ ] **Step 4 — Watch pass; gofmt/vet/golangci-lint.**
- [ ] **Step 5 — Commit** (`feat(gateway): select chat_template_kwargs-capable targets by resolved type (#230)`).

---

## Phase C — Native Messages-passthrough injection (primary)

### Task 6: Inject `chat_template_kwargs.reasoning_effort` into the native body

**Files:** `internal/gateway/native_passthrough.go` (the effort extraction + merge helpers, the third edit in `proxyNative`, the pre-dispatch validation in `tryProxyNative`, the two-edits-only contract comments + `nativeRelay` doc), `internal/gateway/inference_handlers.go` if the flag must be threaded; tests.

**Interfaces:** consumes Task 1 (`token.ClaudeEffortCompatibility`), Task 5 (`targetUsesChatTemplateKwargs`); produces `anthropicEffortFromBody`, `injectChatTemplateReasoningEffort`.

- [ ] **Step 1 — RED.** With `token.ClaudeEffortCompatibility` on and a resolved **llama.cpp passthrough** target, a Messages request `{"output_config":{"effort":"medium"},...}` reaches the upstream as a **Messages** body containing `chat_template_kwargs.reasoning_effort=="medium"`, with `output_config`/`thinking`/messages/tools intact and only-the-model rewritten. Use the existing native-passthrough test harness (a recording upstream). Assert the UPSTREAM body, not an OpenAI body.
- [ ] **Step 2 — RED (rules + gating).** conflicting existing `chat_template_kwargs.reasoning_effort` → invalid-request (`messages.invalid_output_config`), no upstream call, captured/recorded; matching value → no-op (unchanged body); non-object `chat_template_kwargs` → invalid-request; unrelated `chat_template_kwargs` preserved; missing/blank effort → no-op. Flag off → body unchanged (no injection, no rejection). A non-capable target (llama_swap/ollama/vendor) → no injection even with the flag on. vLLM passthrough → injected.
- [ ] **Step 3 — Watch fail.**
- [ ] **Step 4 — Implement:** `anthropicEffortFromBody` (typed sub-decode: `output_config.effort` as `json.RawMessage`; non-string → error) and `injectChatTemplateReasoningEffort` (decode map + UseNumber, get-or-create `chat_template_kwargs` object, apply merge rules, Marshal, fresh slice). In `tryProxyNative`'s passthrough arm (after resolution), gate on `token.ClaudeEffortCompatibility && targetUsesChatTemplateKwargs(target)`; extract+validate; on malformed reject via `writeJSONCaptured`+`recordUsage` (like the `EndpointModeDisabled` branch) and return true; else apply the edit to the relayed body (in `proxyNative`, after the timings edit, within the `jsonContentType` regime). **Update** the two "exactly two edits" comments and the `nativeRelay` doc to describe the third edit.
- [ ] **Step 5 — Watch pass; streaming + non-streaming both covered; gofmt/vet/golangci-lint.**
- [ ] **Step 6 — Commit** (`feat(gateway): inject chat_template_kwargs.reasoning_effort on llama.cpp/vLLM Messages passthrough (#230)`).

---

## Phase D — Translate path

### Task 7: Translate `output_config.effort` on the Anthropic→OpenAI path

**Files:** `internal/compat/anthropic.go` (`ParseAnthropicMessagesWithOptions`, `ParseAnthropicOptions`; keep `ParseAnthropicMessages`/`CountAnthropicTokens` delegating false), `internal/inference/types.go` (the `ReasoningEffortViaTemplateKwargs` directive), `internal/gateway/inference_handlers.go` (pass the flag into parse; a post-resolution policy step setting the directive when the target is capable), `internal/provider/openai_compatible.go` (emit `chat_template_kwargs.reasoning_effort` when the directive is set, plus the existing top-level `reasoning_effort`); tests.

**Interfaces:** consumes Task 1 + Task 5; produces the parser option + the directive + provider emission.

- [ ] **Step 1 — RED (parser).** `ParseAnthropicMessagesWithOptions(body, {EffortTranslationEnabled:true})` with `output_config.effort:"low"` → `req.ReasoningEffort=="low"` (trimmed). With `EffortTranslationEnabled:false` → `req.ReasoningEffort==""` and NO error even for a malformed `output_config` (today's tolerance). With true + malformed effort (non-string) → an `*inference.Error`.
- [ ] **Step 2 — RED (translate dispatch).** An opted-in Anthropic Messages request routed to a resolved **llama.cpp/vLLM translate** target produces an OpenAI `/v1/chat/completions` upstream body carrying `chat_template_kwargs.reasoning_effort` AND top-level `reasoning_effort`; low/medium/xhigh captured; streaming both ways. A **direct** OpenAI chat request to a llama.cpp target that sets `reasoning_effort` → top-level only, NO gateway-injected `chat_template_kwargs` (directive not set). A non-capable translate target → top-level `reasoning_effort` only (existing behavior), no `chat_template_kwargs`.
- [ ] **Step 3 — Watch fail.**
- [ ] **Step 4 — Implement:** the parser option (read `output_config.effort` only when enabled; malformed→`*inference.Error`); `inference_handlers.go` passes `token.ClaudeEffortCompatibility`; a post-resolution policy step (akin to `applyAnthropicCachePolicy`) sets `req.ReasoningEffortViaTemplateKwargs = true` when `token.ClaudeEffortCompatibility && targetUsesChatTemplateKwargs(target) && req.ReasoningEffort != ""`; `openai_compatible.go` emits `chat_template_kwargs.reasoning_effort` (merge rules) when the directive is set, keeping the existing top-level `reasoning_effort`.
- [ ] **Step 5 — Watch pass; gofmt/vet/golangci-lint.**
- [ ] **Step 6 — Commit** (`feat: translate output_config.effort to chat_template_kwargs on the Anthropic translate path (#230)`).

---

## Phase E — Documentation

### Task 8: Docs + OpenAPI + data-model

**Files:** `docs/architecture/cross-cutting/compatibility-and-inference.md`, `routing-and-model-selection.md`, `security-auth-rbac.md`, `persistence.md`, `reference/data-model.md` (v89 row + api_tokens column), `reference/api-surface.md` + the OpenAPI yaml; a `/effort` command note.

- [ ] **Step 1 — Write** the two delivery paths + the `chat_template_kwargs` contract + backend scope (compat), the resolved-type selector (routing), the management-only setting + auth predicate (security), the v89 migration + column (persistence/data-model), the `claude_effort_compatibility` token field + the native body addition (api-surface/OpenAPI), and the `/effort low|medium|xhigh` doc with Qwen levels (xhigh default; `/effort auto` unreliable) + the "upstream must support it" / "thinking on-off separate" caveats.
- [ ] **Step 2 — `make lint-docs`** → check-docs: OK.
- [ ] **Step 3 — Commit** (`docs: document Claude Code effort compatibility (#230)`).

---

## Final (handled by subagent-driven-development)

Opus whole-branch review; full verification (`make test-go`, `make lint-go`, `make lint-docs`, Postgres leg, Sonar `sonar-scan`→`sonar-branch-findings`, frontend `npm test`/`build`/`format:check`); fold durable notes into `docs/architecture/`; remove `docs/superpowers/`; push + PR (Auto-fix on).
