# API-Variant Endpoint Modes (Codex / Claude Code) — Design

Date: 2026-09-03
Branch: `api-variant-modes`
Status: design approved (brainstorming), pending plan

## 1. Problem & Goal

An application (and, for `server_agent` apps, each runtime spec) must be able to
say — per coding-agent endpoint — whether the gateway should **disable**,
**translate**, or **pass through** that endpoint's traffic:

- **Codex** uses the OpenAI **Responses API**, `POST /v1/responses`.
- **Claude Code** uses the Anthropic **Messages API**, `POST /v1/messages`.

Today this is expressed as two booleans, `native_responses` /
`native_messages` ("Native Durchreichung" checkboxes), which only encode
*translate vs pass-through* — there is no per-endpoint *disabled* state, and
there is no per-model control for `server_agent` apps.

**Goal:** replace the two booleans with a single three-state control per
endpoint, surfaced as a dropdown:

| German (UI) | English | Meaning |
|---|---|---|
| **Deaktiviert** | Disabled | the endpoint is not served by this app/model |
| **Umwandlung** | Translate | client body is translated to `/v1/chat/completions` (lossy, as today) |
| **Durchreichen** | Pass-through | client body is proxied raw to the upstream's native path (as `native_*=true` today) |

And add the **identical** control set — the `openai`/`anthropic`
"API-Varianten" checkboxes **and** both dropdowns — to each `server_agent`
runtime spec, so every managed model can be configured independently.

## 2. Terminology & Current State (as of `origin/main` e7a3809)

- **`api_flavors`** (`APIFlavors []string`, values `openai` / `anthropic`) — the
  base "API-Varianten" capability. Gates routing eligibility. Defaults to both.
  Labeled **"API-Varianten"** in the UI (`i18n` key `applicationFlavors`).
  - `openai` covers **both** `/v1/chat/completions` (opencode, portal chat) and
    `/v1/responses` (Codex).
  - `anthropic` covers `/v1/messages` (+ `/v1/messages/count_tokens`) — Claude
    Code. This is the **only** Anthropic inference endpoint.
- **`native_responses` / `native_messages`** (bool) — translate (`false`) vs
  raw pass-through (`true`) for the Codex / Claude Code endpoint, once the app is
  selected. Decided per request in
  `internal/gateway/native_passthrough.go:nativePassthroughEnabled`.
- The pass-through-vs-translate decision is made **entirely gateway-side**. For
  `server_agent` apps the gateway resolves the request's `model` → mapping →
  runtime spec before dispatch; the agent's runtime router is a **catch-all
  reverse proxy** that forwards *any* path (including `/v1/responses` and
  `/v1/messages`) to the child process, routing only on the top-level `model`
  field (`server-agent/internal/runtime/router.go`, arch doc
  `agent-runtime-manager.md` §4). **No agent-side code is involved in this
  decision.**

Key source anchors:

- Frontend app form: `gateway/frontend/src/components/ApplicationSection.tsx`
  (native `CheckboxGroup` at the `native_responses`/`native_messages` block).
- Frontend spec form:
  `gateway/frontend/src/components/RuntimeAdminSection.tsx` (spec edit/create form).
- Type defaults: `gateway/frontend/src/components/shared/applicationTypeDefaults.ts`.
- Frontend types: `gateway/frontend/src/api/models.ts`,
  `gateway/frontend/src/api/runtime.ts`. i18n: `gateway/frontend/src/i18n.ts`.
- Backend app model: `gateway/backend/internal/routing/store.go`
  (`Application`, `RuntimeSpec`, `applicationHasAPIFlavor`, `ApplicationEndpoint`).
- Resolver/target: `gateway/backend/internal/routing/resolver.go`
  (`Target`, `targetFrom`, `NormalizeAPIFlavor`).
- Candidate selection: `gateway/backend/internal/routing/memory_store.go`.
- Portal DTOs: `gateway/backend/internal/portal/service_applications.go`,
  `gateway/backend/internal/portal/service_runtime.go`.
- Store schema/CRUD: `gateway/backend/internal/store/migrate.go`,
  `sqlite_applications.go`, the runtime-spec CRUD, and
  `application_column_parity_test.go`.
- Request-time decision: `gateway/backend/internal/gateway/native_passthrough.go`,
  `inference_handlers.go`.

## 3. Data Model

### 3.1 Endpoint mode type

Introduce a small enum (Go `type EndpointMode string`) with three values,
serialized as lowercase strings in JSON and stored as `text`:

```
disabled | translate | passthrough
```

### 3.2 Application level

Replace the two booleans with two `EndpointMode` fields:

- `Application.ResponsesMode EndpointMode` (was `NativeResponses bool`)
- `Application.MessagesMode  EndpointMode` (was `NativeMessages  bool`)

`api_flavors` stays exactly as-is (the base capability checkboxes remain).

**Effective-served rule** (the single source of truth for both eligibility and
the pass-through decision):

```
responsesServed = ("openai"    ∈ api_flavors) && ResponsesMode != disabled
messagesServed  = ("anthropic" ∈ api_flavors) && MessagesMode  != disabled
```

So there are two independent ways an endpoint is "off": the flavor checkbox is
unchecked, **or** the mode is `disabled` while the flavor stays checked. The
second is the newly requested independent-disable (e.g. serve plain
`/v1/chat/completions` but reject Codex `/v1/responses`). The stored mode is not
forced when a flavor is unchecked — the effective rule already treats an
unchecked flavor as off, and the UI renders the dropdown as Deaktiviert+disabled
(§5.3). This keeps writes decoupled and lets re-checking a flavor restore the
prior mode.

Note (honest asymmetry): for `anthropic`, `/v1/messages` is the only inference
endpoint, so "anthropic checked + MessagesMode=disabled" serves nothing
Anthropic — behaviourally identical to unchecking `anthropic`. Both controls are
kept for symmetry with the `openai` case, where chat-completions genuinely
survives a disabled Codex endpoint.

### 3.3 Runtime spec level (`server_agent`)

Each `RuntimeSpec` gains the **identical** trio, stored explicitly (snapshot,
never null after backfill/create):

- `RuntimeSpec.APIFlavors []string`
- `RuntimeSpec.ResponsesMode EndpointMode`
- `RuntimeSpec.MessagesMode  EndpointMode`

For a `server_agent` app, the **resolved spec is the sole authority** for its
model's flavors + modes. The application-level values serve two roles only:
(a) the pre-fill template for a newly created spec, and (b) the fallback for a
mapping that has no spec at all. The gateway already resolves `model → mapping →
spec` before dispatch; the resolver surfaces the spec's effective values onto
the routing `Target` so downstream code reads them uniformly (§4).

These three fields are **gateway-side only**. They are **not** added to
`AgentRuntimeSpecDTO` or the agent's `runtime.Spec` wire type — the agent does
not need them (it forwards paths verbatim). Therefore **no ServerAgent version
bump** and **no `server-agent/` change**.

## 4. Backend Enforcement

The routing `Target` carries the **effective** values for the request:

- Ordinary app: `Target.{APIFlavors,ResponsesMode,MessagesMode}` = the app's.
- `server_agent` app with a resolved spec: the **spec's** values; if the
  resolved mapping has no spec, the **app's** values.

`targetFrom` in `resolver.go` sets these when it builds the target (it already
has the resolved mapping/spec in scope for `server_agent`).

Two enforcement points, both reading the effective `Target` values:

1. **Candidate eligibility.** For an `openai_responses` request an app is only a
   candidate when `responsesServed` is true for the effective values; for
   `anthropic_messages`, when `messagesServed` is true. Plain
   `openai_chat_completions` requests keep using only the coarse `openai` flavor
   check (a disabled Codex endpoint must not remove chat-completions
   eligibility). This refines the existing coarse flavor gate
   (`applicationHasAPIFlavor` / `memory_store.go` candidate loop /
   `resolver.go` affinity reuse) to be endpoint-aware for the two
   coding-agent endpoints.

2. **Pass-through decision.** `native_passthrough.go` reads the effective mode:
   - `passthrough` → proxy the raw body to the native path (as today when
     `native_*=true`).
   - `translate` → fall through to the compat translate path (as today when
     `native_*=false`).
   - `disabled` → reject. In practice eligibility (point 1) already excludes an
     ordinary app; for `server_agent` a per-model `disabled` is only knowable
     after model resolution, so this is the point that rejects it, with a new
     **stable error code** (`responses.endpoint_disabled` /
     `messages.endpoint_disabled`) and an HTTP 4xx. Stable error codes are a
     hard project constraint (AGENTS.md).

`internal/compat` is unchanged — the translate path and its lossiness are
exactly as they are today.

## 5. Frontend / UX

### 5.1 Shared component

Extract the whole control block into one reusable component (working name
`ApiVariantControls`) rendering: the `openai`/`anthropic` checkboxes and the two
mode dropdowns (`SelectField`). Both `ApplicationSection` and
`RuntimeAdminSection` consume it, so the two forms are guaranteed identical.
Props: current `apiFlavors`, `responsesMode`, `messagesMode`, and change
handlers; the component owns the gating logic in §5.3.

### 5.2 Dropdown options

Each dropdown offers the three `EndpointMode` values with i18n labels
(Deaktiviert / Umwandlung / Durchreichen; Disabled / Translate / Pass-through).
The Codex dropdown is labeled for the Responses API, the Claude Code dropdown
for the Anthropic Messages API (reuse/adjust existing
`applicationNativeResponses` / `applicationNativeMessages` strings).

### 5.3 Gating rule (matches the request literally)

- If the endpoint's flavor checkbox is **unchecked**: the dropdown is
  **disabled** and displays **Deaktiviert** (regardless of the stored mode).
- If **checked**: the dropdown is enabled and all three values are selectable;
  it shows the stored mode. On a fresh app/spec the default is **Durchreichen**
  (§6).

Codex dropdown ↔ `openai` checkbox; Claude Code dropdown ↔ `anthropic` checkbox.

### 5.4 Spec form pre-fill (snapshot)

When **creating** a runtime spec, the block is pre-filled from the parent
`server_agent` application's current `api_flavors` + modes. On save the spec
stores explicit values (the existing runtime-spec model is a full-document
upsert with no field inheritance; this fits it). When **editing**, the spec's
stored values are shown. Later changes to the application do **not** propagate to
existing specs — only new specs pick up the new template.

### 5.5 Type defaults

`applicationTypeDefaults.ts`: the two mode defaults become **`passthrough`** for
every application type (see §6). `migrateTypeFields` continues to migrate these
two fields on a type change (a field still holding the old type's default adopts
the new type's default; a customised field is left alone).

## 6. Defaults (research-backed)

Every supported upstream now serves **both** native endpoints (web research,
2026-09-03):

| Upstream | `/v1/responses` | `/v1/messages` |
|---|---|---|
| Ollama | ✅ v0.13.3 (non-stateful only) | ✅ v0.14.0 (Jan 2026) |
| vLLM | ✅ v0.10.0 | ✅ v0.11.1 |
| llama.cpp `llama-server` | ✅ 2026-01-21 (Responses→Chat shim, maturing) | ✅ 2025-11-28 (tool use needs `--jinja`) |
| llama-swap | ✅ routes both (backend-dependent) | ✅ routes both |
| LiteLLM proxy | ✅ v1.63.11 | ✅ v1.67 |

The old per-type "translate" exceptions (Ollama-Codex, `server_agent`) are
therefore obsolete. **The uniform default for every type is `passthrough`
(Durchreichen)** when the flavor is enabled, matching the original request.
Caveats (Ollama Responses is non-stateful; llama.cpp Responses compliance is
still maturing) are acceptable — pass-through is ≥ translate in fidelity, and an
operator can switch any endpoint to Umwandlung.

## 7. Storage & Migration

New append-only migration:

1. **`applications`**: add `responses_mode text`, `messages_mode text`. Backfill
   from the existing booleans: `native_responses = 1 → 'passthrough'`, `0 →
   'translate'` (same for messages). No app becomes `disabled` on upgrade →
   behaviour preserved.
2. **`agent_runtime_specs`**: add `api_flavors text`, `responses_mode text`,
   `messages_mode text`. Backfill each existing spec from its parent application
   (join `agent_runtime_specs.mapping_id → mappings → applications`): copy the
   app's `api_flavors` and its just-backfilled `responses_mode` /
   `messages_mode`. This is the "Snapshot aus App" decision — existing specs
   become explicit and independent, and the upgrade changes no behaviour.

The old `native_responses` / `native_messages` columns are superseded; new code
no longer reads or writes them. Physical column removal is **out of scope** for
this branch (append-only migration discipline, three drivers); they remain as
inert columns. The baseline `create table` in `migrate.go` keeps its existing
columns; only the additive migration and the new-column reads/writes change.

Must stay green: all three store drivers (memory / sqlite / postgres), the
`dialect` seam, `application_column_parity_test.go` (extend for the new
columns), and Postgres verification via `OP_AI_GATEWAY_TEST_POSTGRES_DSN`
(wide-value/wide-column ADR-005 — `text` columns are fine).

## 8. API Surface (DTOs)

- `ApplicationDTO`: `responses_mode` / `messages_mode` become the enum strings
  (replace the two booleans). `CreateApplicationRequest`: enum values (optional,
  defaulted). `UpdateApplicationRequest`: pointer/optional enum (keep-if-absent
  semantics, mirroring the current pointer bools). Validate against the three
  allowed values; reject unknown → a stable validation error.
- `RuntimeSpecDTO` / `PutRuntimeSpecRequest`: add `api_flavors`,
  `responses_mode`, `messages_mode` (full-document upsert, validated).
- `AgentRuntimeSpecDTO` and the agent wire type: **unchanged**.
- Frontend `api/models.ts` (`PortalApplication`, create/update requests) and
  `api/runtime.ts` (`RuntimeSpec`, `PutRuntimeSpecRequest`): mirror the above.
- `api-surface.md` and the data-model reference doc: update.

## 9. No Agent Change / No Version Bump

Justification (recorded here so a reviewer need not re-derive it): the runtime
router forwards `/v1/responses` and `/v1/messages` verbatim to child processes
and routes only on the top-level `model` field; the disabled/translate/
passthrough decision is taken by the gateway before dispatch. The three new spec
fields never enter `AgentRuntimeSpecDTO` / `runtime.Spec`. Hence
`server-agent/internal/agent/agent.go const Version` is **not** bumped and
`server-agent/` is untouched.

## 10. Testing (TDD)

Backend:
- `EndpointMode` parse/normalize/validate; unknown value rejected.
- Migration: bool→enum backfill for `applications`; app→spec snapshot backfill
  for `agent_runtime_specs`; idempotency; no behaviour change; column parity;
  Postgres.
- Candidacy: `openai_responses` request excludes an app whose effective
  `responses_mode = disabled` or lacks `openai`; `openai_chat_completions`
  request unaffected by `responses_mode`. Same for `anthropic_messages`.
- `native_passthrough` decision for all three modes, ordinary app and
  `server_agent` (effective = spec value), incl. the `disabled` rejection with
  the stable error code.
- Resolver: `Target` carries effective (spec-or-app) values for `server_agent`;
  app values otherwise.

Frontend:
- `ApiVariantControls`: dropdown disabled+Deaktiviert when its flavor is
  unchecked; enabled with three options when checked; default Durchreichen.
- App form + spec form build the correct payload (enum strings, api_flavors).
- Spec create pre-fills from the parent app; spec edit shows stored values.
- Type defaults = passthrough; `migrateTypeFields` on type change.

e2e: optionally extend an existing application/runtime suite to cover the
disabled-endpoint rejection and a pass-through/translate round-trip.

## 11. Docs to Update (same branch)

- `docs/architecture/cross-cutting/compatibility-and-inference.md` — native
  passthrough section becomes the three-state model.
- `docs/architecture/cross-cutting/agent-runtime-manager.md` — per-spec
  flavors+modes, snapshot-from-app, gateway-side enforcement, explicit "no agent
  change / no version bump".
- `docs/architecture/reference/` — data model (new columns/fields) and
  api-surface (DTO changes), new stable error codes.
- ADR log — a short ADR for "endpoint mode replaces native_* booleans;
  independent disable; per-spec snapshot" if it clears the bar.
- `README.md` / screenshots only if the application form screenshot changes
  materially.

## 12. Out of Scope

- Physical removal of the `native_responses` / `native_messages` columns.
- Any change to the compat translation itself.
- Cross-shape conversion (Responses ↔ Messages); conversion is always toward the
  internal model → `/v1/chat/completions`, unchanged.
- Any `server-agent/` change or wire-protocol change.
- Dynamic (non-snapshot) inheritance of app values into existing specs.

## 13. Open / To Confirm During Plan

- Exact new stable error code strings and HTTP status for the `disabled`
  rejection (align with the existing `runtime.*` / provider error-code style).
- Exact runtime-spec store CRUD file names and the precise cross-driver SQL for
  the app→spec snapshot join (SQLite `UPDATE ... FROM` vs correlated subquery;
  Postgres `UPDATE ... FROM`; memory-store backfill in Go).
- Whether the coding-agent dropdown labels reuse the existing
  `applicationNativeResponses` / `applicationNativeMessages` i18n strings or get
  fresh keys.
