# Design: per-token automatic Claude-compatible model aliases (#225)

Status: draft for review. Branch-local (removed before the PR; durable content
folds into `docs/architecture/`).

## 1. Goal

Add an opt-in **per-token** setting that makes the gateway automatically expose
every eligible local model/group offered to that token as an
`anthropic-local-<stable hash>` alias — in model discovery **and** inference
resolution — and attach the original gateway model ID as discovery metadata.

Two outcomes:

- Claude Desktop (and bundled Claude Code) can select local models despite its
  client-side model-ID filter that rejects family substrings such as `qwen`,
  `gpt`. A neutral `anthropic-local-…` ID passes the filter; an honest
  `display_name` still shows the real model.
- An external config-sync script can discover the alias→original-model mapping
  from discovery metadata, with only an inference token (no token-management API).

Genuine Anthropic model IDs are never aliased or altered.

## 2. Verified client behavior (context, from the issue)

Claude Desktop 2.37337.1 / Claude Code 2.1.295 (macOS, 2026-10-15): the client
filters the discovery row's `id` (or `name` in an explicit `inferenceModels`
entry) and **rejects** several non-Anthropic family substrings; it accepts
`anthropic-local-556f772a70b1`. It does **not** scan `display_name`,
`labelOverride`, or unknown extension attributes for those names, and it
discards unknown discovery attributes it does not use. These are observations of
the tested client version, not a documented Anthropic guarantee; the feature
stays opt-in and the ID filter may change.

## 3. Scope

**In:** the token setting (user + service tokens), a shared alias-derivation
function, generated discovery rows + resolution, the `original_model_id` /
honest `display_name` metadata, collision/stability handling, portal UI + i18n,
and the canonical doc/OpenAPI updates.

**Out (separate work):** installing/scheduling/distributing a desktop sync
script (we provide the gateway contract + a documented mapping example only);
turning an incompatible backend into an Anthropic-compatible one; configuring
Claude's per-model context/vision/reasoning controls. Response-model rewriting
is **not** required (the verified client accepts the served model ID even when
it differs from the requested alias).

## 4. Design

### 4.1 Token setting `claude_model_aliases` (bool, default off)

Mirror the in-tree `unknown_model_redirect` boolean end-to-end (the exact
template for an opt-in, default-false, per-token flag):

- **Domain:** add `ClaudeModelAliases bool` to `store.TokenRecord`
  ([store/models.go:117](gateway/backend/internal/store/models.go)) and a twin
  on the request-time `auth.Token`
  ([auth/token_store.go:27](gateway/backend/internal/auth/token_store.go)).
- **DTOs** ([portal/service.go](gateway/backend/internal/portal/service.go)):
  `TokenDTO` (`omitempty` bool), `CreateTokenRequest` (plain `bool`; absent ⇒
  false = correct default), `UpdateTokenRequest` (`*bool`; nil ⇒ field omitted ⇒
  keep current — PATCH-preserve).
- **Handlers:** `CreateToken` (record literal), `UpdateToken`
  (`if req.ClaudeModelAliases != nil { record.ClaudeModelAliases = *… }`). No
  validator needed (plain opt-in bool).
- **Service tokens** ([portal/service_services.go](gateway/backend/internal/portal/service_services.go)):
  same underlying column; add to the create path + `ServiceTokenDTO` (read).
  Service tokens have **no field-PATCH update**, so the flag is set at creation
  and read back — not patched later (consistent with existing service-token
  settings). This is the agreed behavior.
- **Stores (all three):** `store/sqlite_token.go` (insert column list,
  `tokenColumns`, `UpdateTokenMetadata` SET, `scanToken` as `int64 != 0`, and
  `LookupBearer`→`auth.Token` assembly) backs sqlite+postgres via the `dialect`
  seam; `portal/memory_directory.go` mirrors it (record store + `auth.Token`).
- **Migration v88** (current tail is v87): add
  `addColumnIfMissing(ctx, tx, dl, "api_tokens", "claude_model_aliases integer not null default 0")`,
  following `migration63Up`. The `default 0` DDL backfills existing rows to false
  in the same ALTER — no separate backfill. A boolean is `integer not null
  default 0` on both dialects, so ADR-005's wide-column rule does not apply, but
  add a migrations-table row to `docs/architecture/reference/data-model.md`. The
  `schema_column_coverage_test.go` guard requires a fixture that seeds the bool
  TRUE.
- **Portal:** checkbox in `TokenList.tsx` (and the service-token create form),
  types in `api/tokens.ts` (`PortalToken` / `CreateTokenRequest` /
  `UpdateTokenRequest`), and DE+EN keys in `i18n.ts` (`de` is canonical;
  `en: PortalMessages` enforces parity at compile time). Add an `i18n.test.ts`
  spec for the new keys, matching convention. Suggested label: **Claude Desktop
  model aliases**.

### 4.2 Alias derivation (one shared function)

A single derivation function produces the alias set, used by **both** discovery
and resolution so they cannot disagree (issue requirement). It takes the token's
currently-offered model/group views and returns `map[aliasID]originalGatewayID`
(plus the honest display name per alias).

**Eligibility** — generate an alias for a model/group iff:

- it is offered to this token (the same views discovery already builds), and
- it serves the Anthropic Messages flow —
  `routing.MappingServesAPIFlavor(app, eff, "anthropic")` (native or via the
  translate path), i.e. `anthropic` in effective flavors AND
  `MessagesMode != Disabled`
  ([routing/served_flavors.go:107](gateway/backend/internal/routing/served_flavors.go)), and
- it is a **local** model/group, not a genuine Anthropic model and not a
  vendor-account model (do not infer provider identity from a display label when
  routing/source info is available), and
- it is not image-only (`FlavorsAreImagesOnly`), not Messages-disabled, not
  suppressed/hidden/locked from this token's discovery, and not a hidden group
  member.

Generate **only from real model/group identities** — never from already
generated aliases or manually offered alias rows. This is structurally
guaranteed by deriving from the pre-suppression real-model views (`preSuppress`,
which excludes aliases), the same source `applyOverrideAliases` uses
([portal/service_model_offering.go:94](gateway/backend/internal/portal/service_model_offering.go)).

**The identity hashed is the gateway model/group ID** — `GatewayModelName`
([routing/store.go:820](gateway/backend/internal/routing/store.go)) (or the group
name) — never the upstream `AppModelName`, display label, list position, loaded
state, or selected group member. A renamed gateway ID is a new identity.

**Alias ID format:** `anthropic-local-` + the first 12 **lowercase hexadecimal**
characters of SHA-256 over the UTF-8 gateway ID. Example: `qwen3.8-fast-loaded`
→ `anthropic-local-556f772a70b1`.

**Filter-safe fallback (hexadecimal, per the agreed decision):** if the chosen
12-hex window contains a denylisted family substring (only `[0-9a-f]` words can
occur in hex — e.g. `abab`), **slide the 12-char window along the 64-hex SHA-256
digest** (offset 0,1,2,…) and take the first window with no denylisted
substring. The alias stays uniformly hexadecimal — no decimal/format switch.
The denylist is a documented constant of known client-blocked family markers
(e.g. `qwen`, `gpt`, …); only hex-expressible entries can ever trigger the slide.
If no clean window exists in the digest (astronomically unlikely), the alias is
skipped with a diagnostic (treated like a collision). The final encoding is
documented and unit-tested so future changes do not rename existing aliases.

**Collision policy (deterministic):**

- Two distinct real gateway IDs hashing to the same alias ID → **skip both**
  automatic aliases and emit a clear diagnostic (never silently route one alias
  to two targets).
- An automatic alias ID colliding with an existing manual `model_override_map`
  key, or with a real model/group name → the manual rule / real identity wins;
  the automatic alias is skipped (and no automatic metadata is published that
  would claim a different target).

**Stability:** alias IDs are stable across list reordering, restarts, metadata
changes, load/unload events, and group-member changes, because they depend only
on the gateway ID + the fixed encoding + the fixed denylist. No generated rows
are persisted into the user's `model_override_map`; the set is derived from
current state each time (new eligible models appear, removed ones disappear).

### 4.3 Discovery integration

When the token's flag is on, a **generated-alias overlay** runs alongside the
existing `applyOverrideAliases` on the suppressed/usage path (`Models()` /
`ModelsForFlavor`), **not** on admin `ManageModels()` (admin keeps showing real
models). Each generated alias row is built like an `Offer:true, HideTarget:false`
override onto its real target: it inherits the target's full row payload (flavor
set from `preSuppress`, loaded/offered state, context size, vision/image/
image_edit/tool capabilities, group flag, availability) — do not invent
capabilities because the ID starts with `anthropic`; keep unknown values unknown.

**Metadata (agreed placement):** add
`OriginalModelID string json:"original_model_id,omitempty"` to `ModelDTO`
([portal/service.go:1238](gateway/backend/internal/portal/service.go)), set only
on generated alias rows to the real gateway ID; and set the alias row's
`DisplayName` to the **honest** label (the target's display name when available,
otherwise the original gateway ID — never an Anthropic product name). In the
listing serializers, emit `original_model_id` and `display_name` as **top-level
siblings of `capabilities`** (not nested, not under an `op_ai_gw` namespace),
only for generated alias rows:

- `/v1/models` + `/openai/v1/models`: the `handleOpenAIModels` item map
  ([server.go:1530](gateway/backend/internal/gateway/server.go)) — add the two
  sibling keys next to `capabilities`.
- `/api/v1/models` (LM Studio v2): `lmStudioV1ModelsFromDTOs` — already carries
  `display_name`; add `original_model_id`.
- `/anthropic/v1/models`: `handleAnthropicModels`
  ([server.go:1643](gateway/backend/internal/gateway/server.go)) — currently
  consumes IDs only; route the DTO metadata through so the same mapping + honest
  label appear, preserving the Anthropic response shape and required fields.

**Flavor semantics preserved:** `/v1/models` stays OpenAI-chat-flavored
(`openAIModelDTOs` keeps only `openai`-flavor rows) — an Anthropic-only alias is
**not** added there merely to populate a Claude picker; it surfaces on
`/anthropic/v1/models`. Do not attach original-target metadata to unrelated
manual aliases (those may intentionally hide a target).

### 4.4 Inference resolution

Server-side resolution via the shared preflight
([inference_handlers.go:508](gateway/backend/internal/gateway/inference_handlers.go)),
so native and translated paths (including streaming and tool calls) behave
consistently. Extend `resolveModelOverride`
([inference_handlers.go:223](gateway/backend/internal/gateway/inference_handlers.go))
to this precedence:

1. explicit `model_override_map` entry for the requested ID;
2. **generated alias** (only if the token flag is on), resolved **once** to its
   real gateway model/group via the shared derivation function — no recursive
   second override;
3. existing catch-all `model_override`;
4. existing unknown-model redirect (unchanged rules).

A matching **manual** rule always wins; if it already represents the same
alias/target, avoid a duplicate listing row and respect its `offer`/`hide_target`;
if it conflicts, publish no automatic metadata claiming a different target.
Apply all existing access checks to the **effective target** (service allowlists,
locked models, server restrictions, provisioning, capability checks, rate
limits, quotas, budgets); preserve the discovery-visibility vs
invocation-permission distinction (allowlists restrict invocation, not
discovery). Preserve the requested alias and the effective target in the
existing usage/diagnostic fields. Disabling the flag removes the generated
discovery rows **and** generated resolution; explicit manual rules stay intact;
a disabled/removed/stale alias follows existing failure/redirect behavior — never
a secret fallback or reuse of an alias for another model.

### 4.5 External script contract

With only an inference token, a script can: fetch models; recognize alias rows by
`original_model_id`; generate Desktop `inferenceModels` entries with `name = id`
and `labelOverride = display_name` (falling back to the original ID for the
label); keep genuine Anthropic entries unchanged; and omit duplicate original
local entries. We document this mapping (with `modelDiscoveryEnabled: false`) as
the example; the script itself is out of scope.

## 5. Key design decisions

- **Computed, not materialized.** Aliases are derived from current state each
  request, never written into the manual `model_override_map` — so new/removed
  models track automatically and the user's manual map is untouched.
- **One shared derivation function** feeds discovery and resolution, so a listed
  alias and a resolvable alias can never diverge.
- **Hexadecimal throughout, sliding-window fallback.** Keeps every alias uniform
  hex (matching `556f772a70b1`); the fallback slides the window over the digest
  rather than switching encodings.
- **Metadata sibling of `capabilities`.** `original_model_id` + honest
  `display_name` at the model-row top level, reusing the existing extension layer
  rather than a new namespace.
- **Single resolution / no alias-of-alias** is preserved structurally (one
  lookup; targets validated against alias-free `preSuppress`).

## 6. Integration points (map)

See §4 anchors. Primary files: `internal/portal/service.go`,
`internal/portal/service_model_offering.go`,
`internal/portal/service_services.go`, `internal/portal/memory_directory.go`,
`internal/store/{models.go,sqlite_token.go,migrate.go,dialect.go}`,
`internal/auth/token_store.go`, `internal/gateway/server.go`,
`internal/gateway/inference_handlers.go`, `internal/routing/served_flavors.go`;
frontend `src/components/TokenList.tsx`, `src/api/tokens.ts`, `src/i18n.ts`.

## 7. Testing strategy

TDD throughout (subagent-driven implementation). Coverage must map to the
issue's acceptance criteria:

- **Setting lifecycle:** create/read/update, omitted-PATCH preserve, restart,
  rotation, across memory/sqlite/postgres (conformance suites + the column-
  coverage guard fixture seeding the bool TRUE). Postgres leg provisioned via
  `OP_AI_GATEWAY_TEST_POSTGRES_DSN`.
- **Derivation:** a stable alias for a Qwen/GPT-OSS gateway ID; genuine Anthropic
  IDs unchanged; hash stability across reorder/restart/metadata/load/group-member
  changes; the hex fallback (a crafted ID whose 12-hex window contains `abab`
  slides deterministically); collisions (two IDs → same hash skip both +
  diagnostic; alias vs manual rule / real name).
- **Discovery:** `original_model_id` + honest `display_name` as siblings of
  `capabilities` on all three listings for alias rows only; flavor filters
  intact (anthropic-only/openai-only/images-only/Messages-disabled/hidden/locked/
  groups covered); admin `ManageModels` shows real models, not aliases;
  capabilities/context inherited incl. unknown.
- **Resolution:** precedence (manual > generated > catch-all > redirect), single
  resolution, access checks on the effective target, requested-alias/effective-
  target in usage; native + translate, streaming + tool calls; disabling removes
  generated rows + resolution; no alias-of-alias; `offer:false`/`hide_target`
  honored.
- **Cross-token isolation:** another token cannot gain access through an alias.
- **Desktop smoke (manual):** a generated row converts to a valid
  `inferenceModels` entry; real label + a text response + a harmless tool round
  trip on the tested Desktop/CLI versions.

## 8. Documentation updates

Update the canonical docs in the same branch: compatibility-and-inference
(discovery metadata + alias contract), routing-and-model-selection (precedence,
eligibility), persistence (migration v88, the new column) +
`reference/data-model.md` migrations table, security-auth-rbac (token setting,
discovery-vs-invocation), the API surface / OpenAPI (`claude_model_aliases` field
+ the `original_model_id`/`display_name` discovery fields), and portal i18n notes.

## 9. Limitations

Naming/discovery/routing only — does not make an incompatible backend
Anthropic-compatible, does not guarantee tool support per model, does not
auto-configure Claude's per-model context/vision/reasoning. The client ID filter
may change; the feature stays opt-in with truthful labels + original-ID metadata.
