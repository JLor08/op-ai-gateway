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

`routing.RuntimeSpec` gains `APITokenMode string` and `APIToken string` (the
sealed value; routing never decrypts it, mirroring `Application.APIToken`).

Type `routing.RuntimeAPITokenMode` (`"off"|"set"|"random"|"app"`) with consts, in
a new small file (mirrors `visible_devices_mode.go`).

**No new column carries the token in plaintext.** The DB only ever holds the
sealed value.

---

## 4. The `${API_TOKEN}` placeholder + resolution

- **New placeholder token `${API_TOKEN}`**, valid in Env values and Args.
- **Resolution is agent-side**, mirroring `${AGENT_ENV:NAME}`: the agent's
  `ExpandPlaceholders` replaces `${API_TOKEN}` with the token value, and the span
  is **masked** in the resolved-command log frame and the file-mode report
  exactly like an `${AGENT_ENV}` span (reuse the existing masking path so the
  token never appears in logs/reports).
- **The token reaches the agent in a dedicated wire field**, not embedded in the
  Env JSON: the gateway's runtime-config push adds `api_token` to the wire
  `runtime.Spec` (server-agent `types.go`), carrying the **decrypted** token the
  selected mode resolves to (see §5). The agent resolves `${API_TOKEN}` from that
  field. An empty field with a `${API_TOKEN}` in Env/Args is a launch-time hard
  error (same class as an unresolved `${AGENT_ENV}`).
- The gateway decrypts the token **only** when building the pushed config
  (`capture.OpenSecret`), never storing it decrypted.

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
- `set`/`random` → `Target.APIToken = spec.APIToken` (sealed), `APITokenHeader =
  ""` (Bearer).
- `app` → leave the existing `Target.APIToken = app.APIToken`, `APITokenHeader =
  app.APITokenHeader` (today's behaviour — nothing to change).
- `off` → today's behaviour (app token or none).

The existing edge path (`server.go` `upstreamAuthCtx` → `OpenSecret` →
`WithUpstreamAuth` → `applyUpstreamAuth`) then sends `Authorization: Bearer
<token>`; the router forwards it verbatim to the child. **No change to the
provider/edge auth code and no router change.**

Net: the child is launched requiring `<T>` and every gateway request to it
carries `<T>`. Same `<T>` on both sides because the gateway owns it.

---

## 6. Validation (portal `PutRuntimeSpec`)

Mirroring the `${..._DEVICES}` rules; new 400 error codes:

- `runtime_spec.api_token_mode_invalid` — mode ∉ {off, set, random, app}.
- `runtime_spec.api_token_no_placeholder` — mode ≠ off but `${API_TOKEN}` appears
  in neither Env nor Args (the token would be stored/generated but never reach the
  child).
- `runtime_spec.api_token_placeholder_without_mode` — `${API_TOKEN}` used while
  mode = off (a placeholder that resolves to nothing).
- `runtime_spec.api_token_app_unset` — mode = app but the application has no
  `APIToken` set (`app.APIToken == ""`), i.e. nothing to reuse.
- `set` mode with a new token value on a **disk store without a cipher** →
  surface `capture.ErrKeyRequired` as a 400 before persisting (mirror
  `service_applications.go`).

Write-only semantics (mirror `app.APIToken`):
- Create: `api_token` accepted, sealed, never returned.
- Update: `*string` sentinel — `nil` = keep, `""` = clear, value = replace-and-seal.
- DTO reports `api_token_set bool` (presence only) + `api_token_mode`, never the
  value.
- `random`: the value is generated **server-side** (crypto-random, e.g. 32 bytes
  base64url) when the operator selects `random` (or hits Rotate); it is sealed and
  stored, never shown.

**App-token header edge:** in `app` mode, if `app.APITokenHeader` is a non-empty
custom header, the gateway sends the token under that header, but a child that
speaks OpenAI-style (vLLM etc.) expects `Authorization: Bearer`. This is a
pre-existing app-config concern; the portal shows an inline warning in `app` mode
when a custom header is set. Not a hard error.

---

## 7. Portal (spec editor) + the hint

A small **"API-Token (Upstream-Absicherung)"** section on the runtime-spec form:
- Mode select: `Aus` / `Operator setzt` / `Zufällig (vom Gateway erzeugt)` /
  `App-Token verwenden`.
- For `set`: a write-only token field (like the app token field) + the
  `api_token_set` "gesetzt"/"nicht gesetzt" indicator.
- For `random`: a "Neu erzeugen (rotieren)" button; note that the value is never
  shown.
- For `app`: the custom-header warning when applicable; otherwise a note that the
  application's token is used.
- A hint block (shown when mode ≠ off) telling the operator to reference the token
  with **`${API_TOKEN}`** in Env or Args, plus the per-backend variable table:

  | Backend | Env (empfohlen) | Fallback-Argument | Client-Header |
  |---|---|---|---|
  | vLLM | `VLLM_API_KEY=${API_TOKEN}` | `--api-key ${API_TOKEN}` | `Authorization: Bearer` |
  | llama.cpp (`llama-server`) | `LLAMA_API_KEY=${API_TOKEN}` | `--api-key ${API_TOKEN}` | `Authorization: Bearer` |
  | TGI (HF) | `API_KEY=${API_TOKEN}` | `--api-key ${API_TOKEN}` | `Authorization: Bearer` |

  and the **honest** note: **Ollama, LM Studio und llama-swap prüfen eingehende
  Requests nicht headless** (Ollama gar nicht; llama-swap nur per YAML `apiKeys`;
  LM Studio nur per GUI-Schalter) — dort kann dieser Token den Modell-Server nicht
  absichern. (Per the user's decision, the portal only *hints*; it does not block
  those backends.)

i18n: all new strings in de + en (parity is compile-enforced).

---

## 8. Agent (`server-agent`) changes

- Wire `runtime.Spec` gains `APIToken string` (json `api_token`) — the resolved,
  decrypted token for this spec's mode (empty when `off`).
- `ExpandPlaceholders`: resolve `${API_TOKEN}` from `Spec.APIToken`; unresolved
  (`${API_TOKEN}` present but `Spec.APIToken` empty) is a hard error; **mask the
  span** in `resolvedCommand`/report exactly like `${AGENT_ENV}`.
- New capability flag `runtime_api_token` (Since `0.5.0`); `const Version` bump
  `0.4.0` → `0.5.0`.
- No random generation and no router change in the agent (the gateway owns the
  token) — this keeps the security-critical surface in the agent minimal (only:
  receive a token, substitute it, mask it).

---

## 9. Security properties & trade-offs

- The token is **sealed at rest** (capture cipher; `ErrKeyRequired` refuses a
  keyless disk store), **never returned** to the UI (`api_token_set` only),
  **never logged** (masked in resolved-command + reports; provider already never
  logs it).
- It travels the gateway→agent channel **decrypted but encrypted-in-transit**
  (the existing authenticated agent transport) — the same posture `app.APIToken`
  already has toward the upstream. This is the conscious divergence from the
  `${AGENT_ENV}` "never on the wire" rule; documented in the architecture docs.
- `random` removes the human from the loop entirely (no one ever sees the value);
  `set` keeps the operator in control; `app` reuses an existing secret.
- Children are loopback-only, so this is defense-in-depth (a co-tenant process on
  the agent host can no longer hit the model server) **and** a guard against an
  operator binary that binds beyond loopback.

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
2. Portal service: DTO (`api_token_mode`, `api_token_set`, write-only `api_token`
   sentinel), validation + 4 error codes, seal on write, `${API_TOKEN}`
   placeholder-consistency checks, random generation + rotate, resolve-and-push
   the decrypted token into the agent-wire spec, per-mapping Target token in
   `resolver.go` for set/random.
3. Gateway endpoints: map the 4 sentinels → 400.
4. `server-agent`: wire `api_token`; `${API_TOKEN}` resolution + masking; feature
   flag `runtime_api_token`; Version 0.5.0.
5. Frontend: TS type, mode select + write-only field + rotate, hints + table +
   unsupported-backend note, i18n de/en, validation surfacing.
6. Docs: `agent-runtime-manager.md` (placeholder catalog + the token modes +
   the on-the-wire posture), `api-surface.md` (DTO fields + 4 error codes),
   `data-model.md` (migration 74 columns), ADR.
7. Full verification incl. Postgres + Sonar + version rule; cleanup + PR.
