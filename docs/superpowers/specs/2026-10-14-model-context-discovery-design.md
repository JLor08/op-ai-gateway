# Model context-size discovery (opencode) — design

**Issue:** #171. **Branch:** `feat/model-context-discovery` (off `main` after
#218). **Repo-facing text:** English.

## Goal

Make a current opencode (and other LM-Studio/standard clients) discover each
gateway model's context window, and remove the dead LM-Studio-v0 listing that no
current client reads. Three coordinated parts plus the correction of four wrong
doc/code claims.

## Background (verified against `main`, f21585d)

- `GET /v1/models` + `/openai/v1/models` → `handleOpenAIModels`
  (`internal/gateway/server.go:1510`): GET, `requireAnyScope(gateway:use,
  llm:invoke)` (bearer-only; a service token's sole `llm:invoke` is accepted),
  body `{object:"list", data:[{id, object:"model", owned_by:"op-ai-gateway"}]}`
  built from **ids-only** `Portal.ModelsForFlavor(token, openai)`. No context
  field.
- `GET /api/v0/models` → `handleLMStudioModels` (`server.go:1578`): GET,
  `requireScope(gateway:use)` (stricter — a service token gets 403), body
  `{object:"list", data:[…]}` from `lmStudioModelsFromDTOs(openAIModelDTOs(
  Portal.Models(token).Data))` (`server.go:1536`), items `{id, object, type:
  "llm", state}` plus `max_context_length` when known and `loaded_context_length`
  when also loaded. This is the only listing that carries a context size, and a
  stale comment + three docs claim opencode reads it. No current opencode does
  (see #171): opencode v1 discovers nothing for config providers; opencode v2's
  LM Studio provider discovers from LM Studio's native **`GET /api/v1/models`**,
  which the gateway 404s.
- `openAIModelDTOs` (`server.go:1568`): keeps the `Portal.Models()` rows whose
  `Flavors` contain `openai` — the same names `/v1/models` lists (pinned by an
  existing id-equivalence test). Shared source for A and B; kept.
- `ModelDTO` (`internal/portal/service.go:1238`) already carries the fields A/B
  need: `ID`, `DisplayName`, `ContextSize` (minimum known `>0` across offering
  mappings; `0` = unknown), `Vision` (AND-fold, fail-closed), `Loaded`,
  `LoadedOn`. It has **no** `tools` field and **no** provider/application-type
  field.
- `scripts/gen-opencode-config.sh` is the only in-repo consumer of
  `/api/v0/models`: it reads it with a bearer and writes a v1 `opencode.json`
  (`limit.context = max_context_length`, `limit.output = min(floor(context/4),
  8192)`, context-less models skipped).

## Decisions (settled in brainstorming)

1. Scope = A + B + C, and **remove** `/api/v0/models` (not deprecate).
2. Tools policy = advertise `trained_for_tool_use: true` for **every** chat
   (`llm`) model. No current deployment runs a native `ollama` application (the
   one place the gateway drops tools), so the "true for all" caveat does not
   bite. The Ollama tools-drop itself is tracked separately as **#219** (the
   issue's Option 3) and is out of scope here.
3. No `ModelDTO` change: the tools policy is a constant `true`, so no
   provider-type field is needed.

## Part A — serve `GET /api/v1/models` (LM Studio v2 shape)

**Routes (exact patterns, next to the model routes at `server.go:1295-1297`):**
`/api/v1/models` and `/openai/api/v1/models`, one handler. **Not**
`/anthropic/api/v1/models` — `/anthropic` has no chat-completions route, so a
model discovered there could not be chatted with. opencode derives the path by
stripping a trailing `/v1` from its configured base URL and appending
`/api/v1/models`, so `<gw>/v1` → `/api/v1/models` and `<gw>/openai/v1` →
`/openai/api/v1/models`.

**Auth:** `requireAnyScope(gateway:use, llm:invoke)`, bearer-only — identical to
`/v1/models`, so a service token can discover (an existing test requires service
tokens to reach `/v1/models` and `count_tokens`). opencode sends the token as
`Bearer <settings.apiKey>`.

**Body:** `{"models": [ … ]}`, one entry per
`openAIModelDTOs(Portal.Models(token).Data)` row (same ids as `/v1/models`).
Each entry (LM Studio v2 contract — opencode's decode is strict: a missing
required key, a wrong type, or a `null` array fails the WHOLE response; extra
keys are ignored):

| Key | Value | Notes |
|---|---|---|
| `type` | `"llm"` | chat rows only (no embeddings route) |
| `key` | `ModelDTO.ID` | groups and offered aliases included |
| `display_name` | `ModelDTO.DisplayName` | currently the id |
| `max_context_length` | `ModelDTO.ContextSize` | **always present, even `0`** (v2 requires it; at `0` opencode keeps its 200000 default) |
| `loaded_instances` | `[{"config": {"context_length": ContextSize}}]` if `Loaded`, else `[]` | same context value either way |
| `capabilities` | `{"vision": Vision, "trained_for_tool_use": true}` | `trained_for_tool_use` constant `true` (decision 2); `vision` from the fail-closed `Vision` fold |

**Encoding invariants:** `models` and every `loaded_instances` MUST encode as
`[]`, never `null` (build with `make([]T, 0, n)`) — a nil slice → `null` → decode
failure.

**Unknown context (`ContextSize == 0`):** list the model anyway with
`max_context_length: 0`. Omitting it would hide the model (discovery replaces
the provider's model list). Document the fix: set the mapping's `context_size`
(probe or manual) or opencode's `limit.context`.

**Helper:** a new `lmStudioV1ModelsFromDTOs([]portal.ModelDTO) []map[string]any`
beside the removed v0 helper's slot.

## Part B — add `context_length` to `/v1/models`

Change `handleOpenAIModels` to source rows from
`openAIModelDTOs(Portal.Models(token).Data)` instead of the ids-only
`ModelsForFlavor` (same ids — the id-equivalence is already test-pinned; cost is
~`+0.05–2.7 ms`, 11–27%, acceptable and shared with A). Each entry becomes
`{id, object:"model", owned_by:"op-ai-gateway", context_length:N}` with
**`context_length` omitted when `0`** — the open opencode discovery PRs read
`context_length ?? fallback` and `0` is not nullish, so emitting `0` would block
their fallback; llama-swap omits it too. The body type changes from
`map[string]string` to `map[string]any` to carry the int. `/openai/v1/models`
shares the handler and gains the field automatically. No released opencode reads
this yet; it is the standard location other tools (OpenRouter/Together/llama-swap,
the unreleased generic-discovery PRs, the ported script) read, and it is cheap.

## Part C — remove `/api/v0/models`, fix the claims, port the script

**Remove:** the route (`server.go:1297`), `handleLMStudioModels`
(`server.go:1578`) and `lmStudioModelsFromDTOs` (`server.go:1536`). Keep
`openAIModelDTOs` (A and B use it; fix its comment, which references
`/api/v0/models`). After removal `/api/v0/models` falls to the catch-all and
404s `request.not_found`.

**Port `scripts/gen-opencode-config.sh`:** read `<base>/v1/models` with the
bearer and use the new `context_length` (same selection rule: skip
context-less models; same `limit.context` / `limit.output = min(floor(context/4),
8192)` output heuristic; the token stored as `options.apiKey`). Optionally accept
the token as `{env:OP_GATEWAY_TOKEN}` instead of a literal.

**Correct the claims** (all four name opencode as the v0 reader; wrong):
- `server.go` — the stale comment on the (now-removed) v0 handler is deleted with
  it; also the `openAIModelDTOs` comment and the `Models()` comments that mention
  `/api/v0/models` or opencode auto-detection.
- `docs/architecture/cross-cutting/compatibility-and-inference.md` — the
  "auto-detects … via the LM-Studio-shaped `GET /api/v0/models`" passage →
  describe `/api/v1/models` + `context_length` on `/v1/models`.
- `docs/architecture/reference/api-surface.md` — the route table (drop
  `/api/v0/models`, add `/api/v1/models` + `/openai/api/v1/models`, add
  `context_length` to the `/v1/models` row) and the "LM-Studio-aware clients such
  as opencode" sentence; fix the wrong **auth** cells ("Session-or-bearer" for
  `/anthropic/v1/models` and the old v0 row → bearer-only).
- `docs/architecture/reference/openapi.yaml` — drop the v0 path, add the v1
  path(s) and the `/v1/models` `context_length` field; remove the wrong
  `sessionCookie` security on the model listings.
- ADR: update **ADR-045** (the LM-Studio-v0 emulation decision) — record that v0
  is retired, `/api/v1/models` serves the LM Studio v2 shape, `/v1/models` gains
  `context_length`, and the tools policy; OR add a superseding ADR and mark 045
  superseded. (Pick one in the plan; amending 045 is lighter.) Touch the
  `11-risks` entry that references the v0 emulation.

## Testing

- **`/api/v1/models`:** both path patterns; `requireAnyScope` auth (bearer 200,
  **service token 200**, session cookie 401); `{"models": …}` shape with
  `max_context_length` present at `0`; `loaded_instances` `[]` vs populated from
  `Loaded`; `capabilities.vision` from the fold; `capabilities.trained_for_tool_use
  == true`; `models`/`loaded_instances` encode as `[]` not `null` (assert on raw
  JSON bytes); ids equal `/v1/models`.
- **`/v1/models`:** `context_length` present when `>0`, **omitted at `0`**; ids
  unchanged from the old `ModelsForFlavor` path (keep/extend the equivalence
  test); `/openai/v1/models` shares the field.
- **Removal:** `/api/v0/models` now 404s; `lmStudioModelsFromDTOs` and
  `handleLMStudioModels` gone (grep); `openAIModelDTOs` retained.
- **Script:** `gen-opencode-config.sh` reads `/v1/models`, emits the same
  `opencode.json` block from `context_length` (a jq/shape test on a sample body).
- **Docs:** `make lint-docs` green; no doc still claims opencode reads
  `/api/v0/models`.

## Global constraints & gates

- English repo text. `docs/superpowers/**` is branch-local and removed before the
  PR.
- Pre-PR gates (mirroring CI): both Go modules `golangci-lint fmt --diff` + `run`
  + `go test` with the Postgres DSN; frontend `format:check` + `lint` + `build` +
  `test` (no frontend change expected here, but run it); `make lint-docs`; the
  local Sonar branch-findings (judge by new-code/line-attributed findings).
- No local host names in any repo-facing text.

## Scope / out of scope

- **In:** A (`/api/v1/models`), B (`context_length` on `/v1/models`), C (remove
  `/api/v0/models`, fix claims, port the script, ADR).
- **Out:** the Ollama tools-drop (#219); per-model `max_output_tokens` (the
  gateway has none; not synthesized — the opencode PRs that need it are
  unreleased); any portal UI change (this is discovery/API only); serving any
  third-party LM-Studio plugin's stricter schema (`opencode-lmstudio`).

## Open risk

The LM Studio v2 contract (required keys, strict decode, `[]`-not-`null`) is from
opencode's pinned source as recorded in #171 (read, not run against a live
opencode). The encoding invariants and the shape table above encode it; the
tests assert the structure the decode requires. A live opencode-against-gateway
check is a good pre-release smoke test but is not in CI.
