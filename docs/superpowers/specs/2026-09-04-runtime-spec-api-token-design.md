# Runtime-Spec API Token — Design Spec

**Goal:** Let a runtime spec require an API token on its launched model-server
process, and have the gateway authenticate to that process with the same token —
via a `${API_TOKEN}` placeholder the operator drops into the existing Env/Args,
backed by a per-spec sealed secret with four modes (off / operator-set / random /
use-the-app-token).

**Security note:** This feature handles a secret end-to-end (seal at rest,
decrypt in transit over the agent channel, inject into a child process,
authenticate upstream). The token must never be returned to the UI, never
logged, and never persisted in plaintext.

---

## 1. Background — what already exists (do not rebuild)

- **Per-application upstream token.** `routing.Application.APIToken` (+
  `APITokenHeader`) is an operator-set, **write-only**, **sealed** token. It is
  sealed with the general **capture cipher** (`capture.SealSecret(s.cipher, …)`,
  `ErrKeyRequired` on a disk store with no key), stored in
  `applications.api_token` / `api_token_header`, reported to the UI only as
  `APITokenSet bool` (presence, never value), copied verbatim (sealed) onto
  `routing.Target`, and decrypted at the edge in `internal/gateway/server.go`
  `upstreamAuthCtx` → `capture.OpenSecret(s.Cipher, target.APIToken)` →
  `provider.WithUpstreamAuth(ctx, target.APITokenHeader, token)` →
  `applyUpstreamAuth` (default `Authorization: Bearer <token>`, else the raw
  custom header; never logged). **This is the model to mirror for the write-only
  secret and the per-request auth.**

- **The `${AGENT_ENV:NAME}` posture.** `routing.RuntimeSpec.Env` is deliberately
  "never secrets at rest": values are placeholders (`${AGENT_ENV:NAME}`, `${PORT}`,
  `${MODEL}`, `${HOST_GPU_IDS}`, `${CUDA_DEVICES}`/`${VULKAN_DEVICES}`/`${METAL_DEVICES}`)
  resolved **agent-side** in `server-agent` `policy_local.go` `ExpandPlaceholders`.
  `${AGENT_ENV:NAME}` is the one path a secret takes to a child *without* touching
  the gateway DB or the wire. This feature adds a secret that is **gateway-stored**
  and delivered over the (encrypted, authenticated) agent channel — a conscious,
  documented divergence, but exactly the posture `app.APIToken` already accepts.

- **Spec ↔ app ↔ router topology.** One `server_agent` Application per server =
  one **router** port (`app.Port`). One RuntimeSpec per mapping (unique key
  `mapping_id`); one Application → many mappings → many specs. Each child process
  is loopback-only (`127.0.0.1:<ListenPort>`), launched by exactly one spec, and
  fronted by the router, which routes by the request's `model` field and forwards
  the inbound `Authorization` header **verbatim** to the selected child. So a
  gateway that sends `Bearer <T>` reaches the child as `Bearer <T>` — verbatim
  forwarding is enough; no router change is needed.

- **Placeholder validation precedent.** `${..._DEVICES}` + `visible_devices_mode`
  already pair "a placeholder in Args/Env" with "a mode" and validate the two are
  consistent (400 `runtime_spec.visible_devices_args_no_placeholder`). This
  feature follows the same shape.

- **Capability negotiation.** `server-agent` `agent.Features` (append-only,
  name-equality gating), current `const Version = "0.4.0"`. A new flag → MINOR
  bump (enforced by `TestFeatureRegistry`).

---

## 2. The four modes (per spec)

A spec gains `api_token_mode`:

| Mode | Meaning | Where the token value comes from |
|---|---|---|
| `off` (default) | No token; `${API_TOKEN}` is not allowed | — |
| `set` | Operator sets a write-only token | the spec's sealed `api_token` |
| `random` | Gateway generates + stores a random token (nobody sees it) | the spec's sealed `api_token`, generated |
| `app` | Reuse the application's token | `app.APIToken` (sealed, per-app) |

In every non-`off` mode the operator writes the token into the child by putting
the placeholder **`${API_TOKEN}`** in the spec's **Env** (e.g.
`VLLM_API_KEY=${API_TOKEN}`) or **Args** (e.g. `--api-key`, `${API_TOKEN}` on the
next line). "Which variable" is thus the operator's existing Env/Args choice,
guided by the portal hint (§7).

**`random` semantics (confirmed):** a random token the *gateway* generates and
stores, invisible to any human, rotatable on demand / on save — **not** a fresh
token on every process (re)start (the gateway cannot observe the router's lazy
process starts, and it must know the token to authenticate). A "Rotate now"
action regenerates it.

---

## 3. Data model + migration

New columns on `agent_runtime_specs` (migration **74**, additive, append-only):

- `api_token_mode text not null default 'off'` — one of `off|set|random|app`.
- `api_token text not null default ''` — the **sealed** token (`SealSecret`
  envelope: `enc:…` / `plain:…`), used only by `set` and `random`. Empty for
  `off` and `app`.
- `api_token_header_source text not null default 'app'` — where the transmission
  header comes from: `app` (inherit `Application.APITokenHeader`, **the default**)
  or `custom` (use `api_token_header` below). Lets a spec reuse the application's
  header convention *or* set its own. When `app` is chosen, the portal shows
  read-only WHICH header that resolves to (§7).
- `api_token_header text not null default ''` — the **custom** transmission header
  (used only when `api_token_header_source = custom`), mirroring
  `Application.APITokenHeader`. Empty ⇒ `Authorization: Bearer <token>` (the
  default); a value sends the raw token under that header name (e.g. `X-Api-Key`).
  **The header must match what the backend expects** (vLLM: `Bearer` only;
  llama.cpp: `Bearer` or `X-Api-Key`) — a mismatch leaves the child effectively
  unauthenticated, so the portal hint (§7) states this and validation checks the
  header *shape* (§6).

**Effective header** the gateway sends: `api_token_header_source = app` →
`Application.APITokenHeader`; else → `spec.api_token_header` (empty ⇒ Bearer).

`routing.RuntimeSpec` gains `APITokenMode string`, `APIToken string` (the sealed
value; routing never decrypts it, mirroring `Application.APIToken`),
`APITokenHeaderSource string`, and `APITokenHeader string`.

Type `routing.RuntimeAPITokenMode` (`"off"|"set"|"random"|"app"`) with consts, in
a new small file (mirrors `visible_devices_mode.go`).

**No new column carries the token in plaintext.** The DB only ever holds the
sealed value.

---

## 4. The `${API_TOKEN}` placeholder + resolution

- **New placeholder token `${API_TOKEN}`, valid ONLY in Env values — NOT in Args**
  (security review C1). A token in argv is world-readable (`/proc/<pid>/cmdline`,
  `ps aux`) and is echoed by vLLM/llama.cpp at startup (captured into the agent's
  stderr tail and reported up), which defeats this feature's own co-tenant threat
  model (§9) and contradicts the agent's existing rule "secrets go in env, never in
  args" (`server-agent/internal/runtime/report.go`). All three primary backends
  accept the key via env, so this costs nothing. `${API_TOKEN}` in Args is a hard
  validation error (§6). *(This narrows the original "env, fallback argument" to
  env-only for the secret — see the open decision at the end of §6.)*
- **Resolution is agent-side, with its OWN provenance span** — NOT a literal reuse
  of `${AGENT_ENV}`. Today only the `${AGENT_ENV:NAME}` branch records a masked
  span; `${PORT}`/`${MODEL}`/`${*_DEVICES}` record none and appear in clear. So
  `${API_TOKEN}` must, in `expandSpec`, record its own secret span (label
  `${API_TOKEN}`) so `ResolvedCommand.Masked` replaces it in the log frame — the
  gateway never re-masks (`runtime_logs.go`). **This is new masking code, not a
  reuse of the AGENT_ENV path.**
- **The token reaches the agent in a dedicated wire field**, not embedded in the
  Env JSON: the runtime-config push adds `api_token` to the wire `runtime.Spec`
  (server-agent `types.go`), carrying the **decrypted** token the selected mode
  resolves to (§5). The agent resolves `${API_TOKEN}` from that field. `${API_TOKEN}`
  in Env with an empty field is a launch-time hard error (unresolved-placeholder
  class).
- The gateway decrypts the token **only** when building the pushed config
  (`capture.OpenSecret`), never storing it decrypted. **On a decrypt failure it
  pushes an EMPTY token (fail-closed)** so the agent hard-errors at launch — never
  a garbled/partial token, never silently dropping the requirement.

---

## 5. Auth flow (both directions), per mode

Two things must happen so the child both *requires* and *accepts* the token:

**(a) Inject into the child (so it requires the token).** When the gateway
assembles the runtime config it pushes to the agent for a spec, it resolves the
mode to a concrete token:
- `set`/`random` → `OpenSecret(spec.APIToken)`.
- `app` → `OpenSecret(app.APIToken)`.
- `off` → none.

It sends that value in the wire `Spec.api_token` field; the agent injects it via
`${API_TOKEN}` into the child's env/args. (The push travels the existing
encrypted, authenticated agent channel.)

**(b) Authenticate on each request (so the child accepts).** The gateway resolves
the **per-mapping** token into `routing.Target` in `resolver.go` `targetFrom`
(which already loads the spec for `server_agent` apps):
- `set`/`random` → `Target.APIToken = spec.APIToken` (sealed);
  `Target.APITokenHeader` = the **effective header** (§3): `app.APITokenHeader`
  when `api_token_header_source = app`, else `spec.api_token_header` (empty ⇒ Bearer).
- `app` → `Target.APIToken = app.APIToken`; `Target.APITokenHeader` = the same
  effective-header rule (default source `app` ⇒ `app.APITokenHeader`, i.e. today's
  behaviour).
- `off` → today's behaviour (app token or none).

The existing edge path (`server.go` `upstreamAuthCtx` → `OpenSecret` →
`WithUpstreamAuth` → `applyUpstreamAuth`) then sends `Authorization: Bearer
<token>`; the router forwards it verbatim to the child. **No change to the
provider/edge auth code and no router change.**

Net: the child is launched requiring `<T>` and every gateway request to it
carries `<T>`. Same `<T>` on both sides because the gateway owns it.

**(c) The benchmark/capacity paths must resolve the per-spec token too**
(security review I3). `benchmark_runner.go` builds its `routing.Target` DIRECTLY
from `app.APIToken`, bypassing `targetFrom`; a `set`/`random` child would then 401
every scheduled-benchmark and capacity probe (fail-closed but breaks measurement).
The per-mapping token resolution must be applied in those Target builders too (or
routed through a shared resolver). Cross-mapping is safe: `targetFrom` keys the
spec by `mapping.ID`, and a routing disagreement yields a 401 at the child, never
an open child.

---

## 6. Validation (portal `PutRuntimeSpec`)

Mirroring the `${..._DEVICES}` rules; new 400 error codes:

- `runtime_spec.api_token_mode_invalid` — mode ∉ {off, set, random, app}.
- `runtime_spec.api_token_in_args` — `${API_TOKEN}` appears in Args (forbidden;
  argv is world-readable, §4/C1). Env only.
- `runtime_spec.api_token_no_placeholder` — mode ≠ off but `${API_TOKEN}` appears
  in no Env value (the token would be stored/generated but never reach the child).
- `runtime_spec.api_token_placeholder_without_mode` — `${API_TOKEN}` used while
  mode = off (a placeholder that resolves to nothing).
- `runtime_spec.api_token_app_unset` — mode = app but the application has no
  `APIToken` set (`app.APIToken == ""`), i.e. nothing to reuse.
- `runtime_spec.api_token_header_invalid` — `api_token_header_source` ∉
  {app, custom}, OR (source = custom) `api_token_header` is not a valid header-name
  shape (reuse the application's `checkHeaderName`: token chars, no colon/space;
  empty is valid ⇒ Bearer). When source = app, `api_token_header` is ignored (the
  app's header is used).
- `set` (new value) **and `random`** (generated value) on a **disk store without a
  cipher** → **seal-or-400 BEFORE any persist**: `capture.SealSecret` returns
  `capture.ErrKeyRequired` (no plaintext-to-disk fallback, verified in
  `capture/secret.go`); surface it as 400 and never write a `plain:` token or a
  half-persisted mode (security review I5; mirror `service_applications.go`).
  `Rotate` follows the same gate. Random values are generated with `crypto/rand`
  by reusing the portal's existing `generateSecret()` / `compactRandomHexWithError`.

Write-only semantics (mirror `app.APIToken`):
- Create: `api_token` accepted, sealed, never returned.
- Update: `*string` sentinel — `nil` = keep, `""` = clear, value = replace-and-seal.
- DTO reports `api_token_set bool` (presence only) + `api_token_mode`, never the
  value.
- `random`: the value is generated **server-side** (crypto-random, e.g. 32 bytes
  base64url) when the operator selects `random` (or hits Rotate); it is sealed and
  stored, never shown.

**Header ↔ backend coupling (general):** the transmission header — `spec.APITokenHeader`
for set/random, `app.APITokenHeader` for app — decides how the gateway *sends* the
token; the child requires it in the header *it* expects. Default (empty ⇒ Bearer)
is correct for vLLM/llama.cpp/TGI. A non-empty custom header only works for a
backend that accepts it (e.g. llama.cpp's `X-Api-Key`); against a Bearer-only
backend it leaves the child effectively unauthenticated. The portal states this in
the hint and shows an inline warning when a non-empty header is combined with a
mode whose token is actually injected. The header *shape* is validated
(`api_token_header_invalid`); the header↔backend *match* is the operator's
responsibility (hinted, not hard-enforced — the gateway cannot know the child's
expectation). App-mode with a custom header fails **closed** (the child sees no
`Authorization`, returns 401 — down, not open), so a warning is acceptable.

**OPEN DECISION (needs your sign-off):** the security review recommends **env-only**
for the secret — `${API_TOKEN}` forbidden in Args — because a token in argv is
world-readable (`ps aux`, `/proc/<pid>/cmdline`) and defeats the co-tenant
protection this feature promises. That narrows your original "Umgebungsvariable
(Fallback Argument)" to **env-only**. This spec assumes env-only. If a backend you
use accepts the key ONLY via a flag, the alternative is to permit `${API_TOKEN}` in
Args behind a loud "leaks via process listing" warning — your call.

---

## 7. Portal (spec editor) + the hint

A small **"API-Token (Upstream-Absicherung)"** section on the runtime-spec form:
- Mode select: `Aus` / `Operator setzt` / `Zufällig (vom Gateway erzeugt)` /
  `App-Token verwenden`.
- For `set`: a write-only token field (like the app token field) + the
  `api_token_set` "gesetzt"/"nicht gesetzt" indicator.
- For `random`: a "Neu erzeugen (rotieren)" button; note that the value is never
  shown.
- **Übermittlungs-Header** (shown when mode ≠ off): a source select —
  `Von der Anwendung übernehmen` (**default**) or `Eigener Header`.
  - `Von der Anwendung`: a **read-only display of the effective app header** so the
    operator sees what is inherited — `Authorization: Bearer (Standard)` when
    `app.APITokenHeader` is empty, otherwise the application's custom header name.
  - `Eigener Header`: a free-text header field, placeholder/default
    `Authorization: Bearer` when empty; inline warning when a non-empty custom
    header is entered (must match the backend, §6).
- For `app` token mode: a note that the application's token is used; the header
  still follows the source select above (default: the app's).
- A hint block (shown when mode ≠ off) telling the operator to reference the token
  with **`${API_TOKEN}`** in Env or Args, plus the per-backend variable table:

  | Backend | Ins **Env**-Feld setzen | Client-Header |
  |---|---|---|
  | vLLM | `VLLM_API_KEY=${API_TOKEN}` | `Authorization: Bearer` |
  | llama.cpp (`llama-server`) | `LLAMA_API_KEY=${API_TOKEN}` | `Authorization: Bearer` (oder `X-Api-Key`) |
  | TGI (HF) | `API_KEY=${API_TOKEN}` | `Authorization: Bearer` |

  (Env only — `${API_TOKEN}` in Args wird abgelehnt, §4/§6.)
- **Structural unsupported-backend warning** (security review I6): the backend is
  known from `app.Type`, so when a non-`off` mode is chosen for a backend that does
  not enforce inbound auth headlessly — **Ollama** (none), **llama-swap** (only YAML
  `apiKeys`), **LM Studio** (only GUI toggle) — the portal shows a **per-backend
  banner** stating that this token cannot secure that model server, rather than a
  generic table note an operator skims past. Per the user's decision it *warns*, it
  does not block.

i18n: all new strings in de + en (parity is compile-enforced).

---

## 8. Agent (`server-agent`) changes

- Wire `runtime.Spec` gains `APIToken string` (json `api_token`) — the resolved,
  decrypted token for this spec's mode (empty when `off`). It is the ONE shared
  struct (agent wire + file-mode parse) — hence the file-mode handling below.
- `ExpandPlaceholders`/`expandSpec`: resolve `${API_TOKEN}` (Env only) from
  `Spec.APIToken`; `${API_TOKEN}` present with an empty `APIToken` is a hard error.
  **Record a NEW secret provenance span for it** (label `${API_TOKEN}`) so
  `ResolvedCommand.Masked` replaces it — new masking code, because the `${AGENT_ENV}`
  span path is provenance-specific and `${PORT}`/`${MODEL}`/`${*_DEVICES}` are not
  masked (security review I1).
- **File mode (security review I4):** a hand-written file-mode config could carry a
  literal `api_token`. `redactConfigEnv` today masks only env *values*, so it would
  send that literal upward in `BuildReport`. Redact the top-level `Spec.APIToken`
  in `redactConfigEnv` — the gateway ingest DTO has no such field and drops it, but
  it must not cross the wire in clear.
- New capability flag `runtime_api_token` (Since `0.5.0`); `const Version` bump
  `0.4.0` → `0.5.0`.
- No random generation and no router change in the agent (the gateway owns the
  token) — the agent's security-critical surface stays minimal: receive, substitute,
  mask, redact.

---

## 9. Security properties & trade-offs

- The token is **sealed at rest** (capture cipher; `ErrKeyRequired` refuses a
  keyless disk store), **never returned** to the UI (`api_token_set` only),
  **never logged** (masked in resolved-command + reports; provider already never
  logs it).
- It travels the gateway→agent channel **authenticated always, but encrypted only
  when the gateway URL is `https`** (security review I2). The agent accepts an
  `http://` gateway URL (`server-agent/internal/config/config.go`), on which the
  pushed decrypted token — and the agent's own bearer credential — travel in clear.
  **Therefore any non-`off` mode requires (or at minimum loudly warns for) an
  `https` gateway URL.** This is the conscious divergence from `${AGENT_ENV}`'s
  "never on the wire" rule — the same posture `app.APIToken` has upstream, and it
  only holds on TLS.
- `random` removes the human from the loop entirely (no one ever sees the value);
  `set` keeps the operator in control; `app` reuses an existing secret.
- The child binds loopback (`127.0.0.1`) and the agent **router** may bind all
  interfaces unauthenticated: the token is (a) defense-in-depth against a co-tenant
  process on the agent host, and (b) the guard that stops an externally-reachable
  router from exposing an unauthenticated model server. **This holds ONLY for
  backends that actually enforce the token** (§7/I6).

---

## 10. Out of scope / non-goals

- **Per-process-start token rotation** (a brand-new token on every child launch).
  Would need agent-side generation + a back-report or router header-injection;
  explicitly deferred (the confirmed requirement is a stored, gateway-managed
  random token).
- **Securing Ollama / LM Studio / llama-swap** — not headless-configurable; the
  portal only hints.
- **Custom inbound-header per spec** for set/random (Bearer only). `app` mode
  inherits `app.APITokenHeader` with a warning.
- Rewriting the router to inject per-child headers (unnecessary — verbatim forward
  + gateway-owned token suffices).

---

## 11. Components touched (for the plan)

1. Store/migration: migration 74; `routing.RuntimeSpec` fields; `RuntimeAPITokenMode`;
   sqlite CRUD; conformance tests.
2. Portal service: DTO (`api_token_mode`, `api_token_header_source`,
   `api_token_header`, `api_token_set`, write-only `api_token` sentinel; plus a
   read-only `app_api_token_header` echo so the UI can show the inherited header),
   validation + **6 error codes** (incl. `api_token_in_args`), env-only + header
   source/shape checks, **seal-or-400 for set AND random** (`crypto/rand` via
   `generateSecret`), rotate, resolve-and-push the decrypted token into the
   agent-wire spec (**fail-closed empty on decrypt error**), per-mapping
   effective-header + Target token in `resolver.go`, **and in the benchmark/capacity
   Target builders** (`benchmark_runner.go`, security review I3), plus an
   https-gateway-URL precondition warning/guard for non-`off` modes (I2).
3. Gateway endpoints: map the 6 sentinels → 400.
4. `server-agent`: wire `api_token`; `${API_TOKEN}` (Env-only) resolution with a
   **new secret provenance span + masking** (I1); **redact `Spec.APIToken` in
   `redactConfigEnv`** for file-mode reports (I4); feature flag `runtime_api_token`;
   Version 0.5.0.
5. Frontend: TS type, mode select + write-only field + rotate, hints + table +
   unsupported-backend note, i18n de/en, validation surfacing.
6. Docs: `agent-runtime-manager.md` (placeholder catalog + the token modes +
   the on-the-wire posture), `api-surface.md` (DTO fields + 4 error codes),
   `data-model.md` (migration 74 columns), ADR.
7. Full verification incl. Postgres + Sonar + version rule; cleanup + PR.
