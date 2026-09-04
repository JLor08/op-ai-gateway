# Runtime-Spec API Token Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every runtime spec (per model/mapping) an upstream API token — reuse the application's token (default), an operator-set write-only token, a gateway-generated random token, or explicitly none — that the gateway both injects into the child model server (via a new `${API_TOKEN}` placeholder) and sends on every request, so a model server bound beyond loopback is not left unauthenticated.

**Architecture:** The **gateway owns and stores** the token (sealed with the capture cipher, mirroring `Application.APIToken`). It resolves the mode to a concrete value at two points: (a) it pushes the **decrypted** token to the agent in a new wire `Spec.api_token` field, and the agent substitutes it into the child's env/args wherever the operator wrote `${API_TOKEN}`; (b) it authenticates every request per-mapping through the existing `routing.Target` → `upstreamAuthCtx` → `applyUpstreamAuth` edge path. No router change and no agent-side generation — the agent's new surface is: receive one field, substitute, mask, redact. This mirrors the already-merged GPU-selection feature (migration 73, `VisibleDevicesMode`, the `${..._DEVICES}` placeholder, its agent flag, its portal select+hints) almost one-for-one; **read that code and mirror it** for every mechanical parallel, and write fresh code only for the security-critical pieces called out below.

**Tech Stack:** Go 1.x (gateway backend `gateway/backend`, agent module `server-agent`), modernc sqlite + Postgres (dual-driver store), React/TypeScript portal (`gateway/frontend`), `crypto/rand`, the `capture` sealing package.

## Global Constraints

Every task's requirements implicitly include this section. Values are verbatim and load-bearing.

- **Migration 74** on `agent_runtime_specs`, additive + append-only, mirroring `store/migrate.go` `migration73Up` (`addColumnIfMissing`, aborts boot on failure, replays cleanly on a fresh install). Four new columns:
  - `api_token_mode text not null default 'app'` — one of `off|set|random|app`.
  - `api_token text not null default ''` — the **sealed** token (`SealSecret` envelope `enc:…`/`plain:…`); non-empty only for `set`/`random`.
  - `api_token_header_source text not null default 'app'` — `app|custom`.
  - `api_token_header text not null default ''` — the custom transmission header.
- **Default mode is `app`, deliberately** (not `off`): today every mapping already sends `Application.APIToken` at the edge, so backfilling existing rows to `app` preserves that exactly. A default of `off` would silently switch upstream auth OFF for every already-authenticated app on upgrade. A blank mode from the DTO normalizes to `app`; a blank header source normalizes to `app` (mirrors `VisibleDevicesMode` empty→`env`).
- `routing.RuntimeSpec` (in `routing/store.go`) gains: `APITokenMode string`, `APIToken string` (**sealed**; routing NEVER decrypts it), `APITokenHeaderSource string`, `APITokenHeader string`.
- New file `routing/runtime_api_token_mode.go` (mirror `routing/visible_devices_mode.go`): type `RuntimeAPITokenMode string` with consts `RuntimeAPITokenModeOff="off"`, `RuntimeAPITokenModeSet="set"`, `RuntimeAPITokenModeRandom="random"`, `RuntimeAPITokenModeApp="app"`; type `RuntimeAPITokenHeaderSource string` with `RuntimeAPITokenHeaderSourceApp="app"`, `RuntimeAPITokenHeaderSourceCustom="custom"`.
- **Sealing uses the portal capture cipher `s.cipher`** (the same one `service_applications.go` uses for `app.APIToken`), NOT `deps.CertCipher`. Seal via `capture.SealSecret(s.cipher, s.settingsVolatile, plain)`; open via `capture.OpenSecret(s.cipher, stored)`. On a keyless disk store `SealSecret` returns `capture.ErrKeyRequired` — surface as **400 BEFORE any persist**, never write a `plain:` token or a half-persisted mode.
- **`${API_TOKEN}` placeholder** is: **required** for `set`/`random` (else `api_token_no_placeholder`), **optional** for `app`, **forbidden** for `off` (else `api_token_placeholder_without_mode`). Valid in Env values (recommended) OR Args (allowed, but drives a loud non-blocking portal warning — argv is world-readable via `ps aux`/`/proc/<pid>/cmdline` and echoed in the model server's own startup logs).
- **Four** error sentinels (400), all `errors.New("runtime_spec.<code>")`, mirroring `ErrRuntimeSpecVisibleDevicesModeInvalid`:
  - `ErrRuntimeSpecAPITokenModeInvalid` → `runtime_spec.api_token_mode_invalid`
  - `ErrRuntimeSpecAPITokenNoPlaceholder` → `runtime_spec.api_token_no_placeholder`
  - `ErrRuntimeSpecAPITokenPlaceholderWithoutMode` → `runtime_spec.api_token_placeholder_without_mode`
  - `ErrRuntimeSpecAPITokenHeaderInvalid` → `runtime_spec.api_token_header_invalid`
  - **There is NO `app_unset` error.** `app` with an empty `app.APIToken` is valid = "auth off for this mapping" (a portal hint, not a 400).
- **Write-only token** (mirror `app.APIToken` in `service_applications.go`): create seals the given value; update uses a `*string` sentinel — `nil` = keep, `""` = clear, value = replace-and-seal. The DTO returns `api_token_set bool` (presence only) + never the value. `random` values are generated server-side with `crypto/rand` by reusing `generateSecret()` (`portal/service.go:4106`).
- **DTO** (`RuntimeSpecDTO`) adds read fields `api_token_mode`, `api_token_set`, `api_token_header_source`, `api_token_header`, plus read-only echoes of the parent application `app_api_token_set bool` and `app_api_token_header string`. `PutRuntimeSpecRequest` adds `api_token_mode`, `api_token_header_source`, `api_token_header`, write-only `api_token *string`, and `api_token_rotate bool`.
- **Effective header** the gateway sends: source `custom` ⇒ `spec.api_token_header`; else ⇒ `app.APITokenHeader`. Empty ⇒ `Authorization: Bearer <token>`.
- **Wire**: `server-agent` `runtime/types.go` `Spec` gains `APIToken string \`json:"api_token"\`` carrying the **decrypted** token for the mode. The gateway's push builder resolves mode→value with `capture.OpenSecret`; **on a decrypt error it pushes an EMPTY token (fail-closed)**, never a partial/garbled value.
- **Auth-b (per request)**: `routing/resolver.go` `targetFrom` and all three `gateway/benchmark_runner.go` Target builders (`:123`, `:524`, `:544`) set `Target.APIToken` + `Target.APITokenHeader` per-mapping via the shared `routing.SpecUpstreamAuth`. `off`→no token (overrides app); `app`→app token; `set`/`random`→spec token. The value in `Target.APIToken` is the **sealed** string (the edge `upstreamAuthCtx` already `OpenSecret`s it).
- **Agent**: resolve `${API_TOKEN}` (Env values + Args) from `Spec.APIToken` in `expandSpec`; a `${API_TOKEN}` with an empty `Spec.APIToken` is a hard launch error. Record a **NEW secret provenance span** (label `${API_TOKEN}`) so `maskSecretSpans` scrubs it from the reported command — this is new masking code, NOT a reuse of the `${AGENT_ENV}` path. `report.go` `redactConfigEnv` must additionally redact the top-level `Spec.APIToken` for file-mode reports. New capability flag `runtime_api_token` (Since `0.5.0`) in `agent/features.go`; bump `agent/agent.go:89` `const Version` `0.4.0` → `0.5.0` (a MINOR bump; `TestFeatureRegistry` enforces every flag's `Since` ≤ `Version`).
- **Security**: sealed at rest; never returned to the UI; never logged; the decrypted token crosses the gateway→agent channel authenticated always but encrypted only when the gateway URL is `https` — so a non-`off` mode warns (non-blocking) when the configured gateway URL scheme is not `https`.
- **Never masked, always cleartext**: `${PORT}`, `${MODEL}`, `${*_DEVICES}` record no span (unchanged). Only `${AGENT_ENV:*}` and now `${API_TOKEN}` record secret spans.
- **Branching/PR (AGENTS.md)**: work only in this worktree; never commit to or merge into `main`. `docs/superpowers/**` is deleted before the PR.
- **Frontend CI**: the "Frontend (portal)" job runs `npm run format:check` (prettier), which local `test`/`build`/`lint` miss — run it before finishing.

---

## File Structure

**Gateway backend (`gateway/backend/internal`):**
- `routing/runtime_api_token_mode.go` *(new)* — the two enum types + consts, and the shared `SpecUpstreamAuth`/`effectiveAPITokenHeader` helpers (auth-b, sealed).
- `routing/store.go` *(modify)* — `RuntimeSpec` gains 4 fields.
- `routing/resolver.go` *(modify)* — `targetFrom` sets per-mapping Target auth via `SpecUpstreamAuth`.
- `store/migrate.go` *(modify)* — migration 74.
- `store/*` sqlite CRUD *(modify)* — persist/scan the 4 columns (every site that already handles `visible_devices_mode`).
- `portal/service_runtime.go` *(modify)* — DTO + put-request fields, validation, seal/rotate, error sentinels.
- `portal/service_runtime_push.go` or the existing agent-push builder *(modify)* — resolve-and-push decrypted token, fail-closed; https warning.
- `gateway/benchmark_runner.go` *(modify)* — three Target builders route through `SpecUpstreamAuth`.
- `gateway/portal_runtime_endpoints.go` *(modify)* — map the 4 sentinels → 400.

**Agent (`server-agent/internal`):**
- `runtime/types.go` *(modify)* — `Spec.APIToken` wire field.
- `runtime/policy_local.go` *(modify)* — `${API_TOKEN}` resolution + new secret span.
- `runtime/report.go` *(modify)* — `redactConfigEnv` redacts `Spec.APIToken`.
- `agent/features.go` + `agent/agent.go` *(modify)* — flag + Version.

**Frontend (`gateway/frontend/src`):**
- runtime-spec editor component + its types + i18n (de/en) — mirror the GPU-selection UI.

**Docs (`docs/architecture`):** `agent-runtime-manager.md`, `api-surface.md`, `data-model.md`, a new ADR.

---

## Task 1: Store — migration 74, routing types, sqlite CRUD

**Files:**
- Create: `gateway/backend/internal/routing/runtime_api_token_mode.go`
- Modify: `gateway/backend/internal/routing/store.go` (RuntimeSpec struct, near the `VisibleDevicesMode` field ~:1282)
- Modify: `gateway/backend/internal/store/migrate.go` (migrations slice ~:106, new `migration74Up` after `migration73Up` ~:3237)
- Modify: sqlite CRUD for `agent_runtime_specs` — every INSERT/UPDATE/SELECT/scan that already names `visible_devices_mode` (find them: `grep -rn "visible_devices_mode" gateway/backend/internal/store`)
- Test: the store conformance/round-trip test that already covers `VisibleDevicesMode` (find it: `grep -rln "VisibleDevicesMode" gateway/backend/internal/store/*_test.go`)

**Interfaces:**
- Produces: `routing.RuntimeAPITokenMode` + consts `RuntimeAPITokenModeOff/Set/Random/App`; `routing.RuntimeAPITokenHeaderSource` + consts `RuntimeAPITokenHeaderSourceApp/Custom`; `routing.RuntimeSpec.{APITokenMode, APIToken, APITokenHeaderSource, APITokenHeader}` (all `string`). Migration `74` named `runtime_spec_api_token`.
- Consumes: `store.addColumnIfMissing`, `store.execTx` (migration helpers, see `migration73Up`).

- [ ] **Step 1: Write the failing store round-trip test.** In the store test file that round-trips a `RuntimeSpec` with `VisibleDevicesMode`, add a case that persists a spec with `APITokenMode: "set"`, `APIToken: "enc:deadbeef"`, `APITokenHeaderSource: "custom"`, `APITokenHeader: "X-Api-Key"`, reads it back, and asserts all four fields equal what was written. Add a second case asserting a spec written with empty api-token fields reads back `APITokenMode == "app"` (the column default) — i.e. persist a spec that omits the mode and confirm the DB default lands. Use the existing test's construction helpers verbatim; only add the new fields/assertions.

- [ ] **Step 2: Run it to verify it fails.** Run: `cd gateway/backend && go test ./internal/store/ -run <RoundTripTestName> -v`. Expected: FAIL — compile error (`RuntimeSpec` has no field `APITokenMode`) or missing columns.

- [ ] **Step 3: Add the routing types file.** Create `routing/runtime_api_token_mode.go` mirroring `routing/visible_devices_mode.go`'s header/comment style:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

// RuntimeAPITokenMode is the per-spec choice of WHERE the upstream API token
// the gateway injects into (and sends to) the child model server comes from.
// Default (and every pre-feature row) is "app" = reuse Application.APIToken,
// which preserves today's edge behaviour. Serialized as its lowercase string,
// stored text (mirrors VisibleDevicesMode). Validated at the DTO edge
// (portal.validRuntimeAPITokenMode), not by a method here.
type RuntimeAPITokenMode string

const (
	RuntimeAPITokenModeOff    RuntimeAPITokenMode = "off"
	RuntimeAPITokenModeSet    RuntimeAPITokenMode = "set"
	RuntimeAPITokenModeRandom RuntimeAPITokenMode = "random"
	RuntimeAPITokenModeApp    RuntimeAPITokenMode = "app"
)

// RuntimeAPITokenHeaderSource selects whether the transmission header is
// inherited from the application ("app", the default) or set per-spec ("custom").
type RuntimeAPITokenHeaderSource string

const (
	RuntimeAPITokenHeaderSourceApp    RuntimeAPITokenHeaderSource = "app"
	RuntimeAPITokenHeaderSourceCustom RuntimeAPITokenHeaderSource = "custom"
)
```

- [ ] **Step 4: Add the four `RuntimeSpec` fields.** In `routing/store.go`, next to `VisibleDevicesMode`, add:

```go
	// APITokenMode is "app"|"set"|"random"|"off": where the upstream API token
	// for this mapping's child comes from. Default "app" (reuse the app token).
	APITokenMode string
	// APIToken is the SEALED per-spec token (SealSecret envelope), used only by
	// "set"/"random". Routing NEVER decrypts it — the edge does.
	APIToken string
	// APITokenHeaderSource is "app"|"custom": inherit the app's transmission
	// header or use APITokenHeader below.
	APITokenHeaderSource string
	// APITokenHeader is the custom transmission header (source=="custom" only);
	// empty ⇒ Authorization: Bearer.
	APITokenHeader string
```

- [ ] **Step 5: Add migration 74.** In the migrations slice (after the version-73 entry) add `{version: 74, name: "runtime_spec_api_token", up: migration74Up},`. Add the function next to `migration73Up`, mirroring its `addColumnIfMissing` calls exactly:

```go
// migration74Up adds the runtime-spec API-token columns (design 2026-09-04),
// additive + append-only like migration73Up. Defaults preserve behaviour:
// mode "app" keeps sending Application.APIToken (a default of "off" would
// silently disable upstream auth for every authenticated app on upgrade).
func migration74Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	if err := addColumnIfMissing(ctx, tx, dl, "agent_runtime_specs",
		"api_token_mode text not null default 'app'"); err != nil {
		return err
	}
	if err := addColumnIfMissing(ctx, tx, dl, "agent_runtime_specs",
		"api_token text not null default ''"); err != nil {
		return err
	}
	if err := addColumnIfMissing(ctx, tx, dl, "agent_runtime_specs",
		"api_token_header_source text not null default 'app'"); err != nil {
		return err
	}
	if err := addColumnIfMissing(ctx, tx, dl, "agent_runtime_specs",
		"api_token_header text not null default ''"); err != nil {
		return err
	}
	return nil
}
```

- [ ] **Step 6: Wire the four columns through sqlite CRUD.** For every INSERT, UPDATE, SELECT and `rows.Scan`/`row.Scan` in `internal/store` that already lists `visible_devices_mode`, add `api_token_mode`, `api_token`, `api_token_header_source`, `api_token_header` in the same clause, bound to/scanned from the matching `RuntimeSpec` fields (append `&spec.APITokenMode, &spec.APIToken, &spec.APITokenHeaderSource, &spec.APITokenHeader` to the scan targets in the same order the columns appear). Keep the column order identical across INSERT/SELECT.

- [ ] **Step 7: Run the store tests to verify they pass.** Run: `cd gateway/backend && go test ./internal/store/ ./internal/routing/ -v`. Expected: PASS (round-trip + default-value cases green).

- [ ] **Step 8: Commit.**

```bash
git add gateway/backend/internal/routing/runtime_api_token_mode.go gateway/backend/internal/routing/store.go gateway/backend/internal/store/
git commit -m "feat(store): migration 74 + RuntimeSpec API-token fields (default app)"
```

---

## Task 2: Portal — DTO read fields + app echoes

**Files:**
- Modify: `gateway/backend/internal/portal/service_runtime.go` (`RuntimeSpecDTO` struct ~:323; `GetRuntimeSpec` ~:404; the empty-spec default literal at ~:414; the DTO-assembly function that maps `routing.RuntimeSpec` + parent app → DTO)
- Test: the portal test that asserts `GetRuntimeSpec` DTO contents (find it: `grep -rln "GetRuntimeSpec\|RuntimeSpecDTO" gateway/backend/internal/portal/*_test.go`)

**Interfaces:**
- Consumes: `routing.RuntimeSpec` fields (Task 1); the parent `routing.Application.{APIToken, APITokenHeader}`.
- Produces: `RuntimeSpecDTO.{APITokenMode string, APITokenSet bool, APITokenHeaderSource string, APITokenHeader string, AppAPITokenSet bool, AppAPITokenHeader string}` with json tags `api_token_mode`, `api_token_set`, `api_token_header_source`, `api_token_header`, `app_api_token_set`, `app_api_token_header`.

- [ ] **Step 1: Write the failing DTO test.** Extend the GET test: store a spec with mode `set`, a sealed `api_token`, header source `custom`, header `X-Api-Key`, under an application whose `APIToken` is set and whose `APITokenHeader` is `Authorization`. Assert the returned DTO has `APITokenMode=="set"`, `APITokenSet==true`, `APITokenHeaderSource=="custom"`, `APITokenHeader=="X-Api-Key"`, `AppAPITokenSet==true`, `AppAPITokenHeader=="Authorization"`, and that the raw sealed token value appears NOWHERE in the marshaled JSON.

- [ ] **Step 2: Run it to verify it fails.** Run: `cd gateway/backend && go test ./internal/portal/ -run <GetRuntimeSpecTestName> -v`. Expected: FAIL (unknown DTO fields).

- [ ] **Step 3: Add the DTO fields.** In `RuntimeSpecDTO`, next to `VisibleDevicesMode`:

```go
	// APITokenMode is "app"|"set"|"random"|"off" (design §2). Default "app".
	APITokenMode string `json:"api_token_mode"`
	// APITokenSet reports presence of a per-spec token (set/random); the VALUE
	// is never on the wire.
	APITokenSet bool `json:"api_token_set"`
	// APITokenHeaderSource is "app"|"custom"; APITokenHeader is the custom header.
	APITokenHeaderSource string `json:"api_token_header_source"`
	APITokenHeader       string `json:"api_token_header"`
	// AppAPITokenSet / AppAPITokenHeader echo the parent application's token
	// presence and header (read-only) so the portal can render the inherited
	// header and the "app has no token ⇒ auth off" hint under app mode.
	AppAPITokenSet    bool   `json:"app_api_token_set"`
	AppAPITokenHeader string `json:"app_api_token_header"`
```

- [ ] **Step 4: Populate them in DTO assembly.** Where `VisibleDevicesMode` is copied from the `routing.RuntimeSpec` into the DTO, add: `APITokenMode` = spec mode normalized to `app` when empty; `APITokenSet = spec.APIToken != ""`; `APITokenHeaderSource` normalized to `app` when empty; `APITokenHeader = spec.APITokenHeader`; `AppAPITokenSet = app.APIToken != ""`; `AppAPITokenHeader = app.APITokenHeader`. In the empty-spec default literal at ~:414 add `APITokenMode: string(routing.RuntimeAPITokenModeApp), APITokenHeaderSource: string(routing.RuntimeAPITokenHeaderSourceApp), AppAPITokenSet: app.APIToken != "", AppAPITokenHeader: app.APITokenHeader` (thread the parent app into that construction if it is not already available).

- [ ] **Step 5: Run the portal DTO test to verify it passes.** Run: `cd gateway/backend && go test ./internal/portal/ -run <GetRuntimeSpecTestName> -v`. Expected: PASS.

- [ ] **Step 6: Commit.**

```bash
git add gateway/backend/internal/portal/service_runtime.go
git commit -m "feat(portal): runtime-spec API-token DTO read fields + app echoes"
```

---

## Task 3: Portal — validation + 4 error sentinels

**Files:**
- Modify: `gateway/backend/internal/portal/service_runtime.go` (error sentinels block ~:76-78; `PutRuntimeSpecRequest` struct ~:380; the validation function that returns `ErrRuntimeSpecVisibleDevicesModeInvalid` ~:763)
- Test: the portal validation test covering `visible_devices_mode` invalid (find it: `grep -rln "VisibleDevicesModeInvalid\|api_token" gateway/backend/internal/portal/*_test.go`)

**Interfaces:**
- Consumes: `routing.RuntimeAPITokenMode`/`RuntimeAPITokenHeaderSource` consts (Task 1); `checkHeaderName` (`service_applications.go:892`).
- Produces: sentinels `ErrRuntimeSpecAPITokenModeInvalid`, `ErrRuntimeSpecAPITokenNoPlaceholder`, `ErrRuntimeSpecAPITokenPlaceholderWithoutMode`, `ErrRuntimeSpecAPITokenHeaderInvalid`; `PutRuntimeSpecRequest.{APITokenMode string, APITokenHeaderSource string, APITokenHeader string, APIToken *string, APITokenRotate bool}` (json `api_token_mode`, `api_token_header_source`, `api_token_header`, `api_token`, `api_token_rotate`); helper `specHasAPITokenPlaceholder(env map[string]string, args []string) bool`; helper `validRuntimeAPITokenMode(string) bool`.

- [ ] **Step 1: Write the failing validation test (table-driven).** Cases, each calling the runtime-spec validation with a `PutRuntimeSpecRequest` and asserting `errors.Is(err, <sentinel>)` (or nil):
  - mode `"bogus"` → `ErrRuntimeSpecAPITokenModeInvalid`.
  - mode `"set"`, Env/Args contain NO `${API_TOKEN}` → `ErrRuntimeSpecAPITokenNoPlaceholder`.
  - mode `"random"`, no placeholder → `ErrRuntimeSpecAPITokenNoPlaceholder`.
  - mode `"app"`, no placeholder → **nil** (optional for app).
  - mode `"off"`, Env has `X=${API_TOKEN}` → `ErrRuntimeSpecAPITokenPlaceholderWithoutMode`.
  - mode `"set"`, Env has `VLLM_API_KEY=${API_TOKEN}`, header source `"custom"`, header `"Bad Header:"` → `ErrRuntimeSpecAPITokenHeaderInvalid`.
  - header source `"bogus"` → `ErrRuntimeSpecAPITokenHeaderInvalid`.
  - mode `"set"`, placeholder present, header source `"app"` → **nil** (valid).
  - mode `"app"`, `app.APIToken == ""` → **nil** (no app_unset error).

- [ ] **Step 2: Run it to verify it fails.** Run: `cd gateway/backend && go test ./internal/portal/ -run <ValidationTestName> -v`. Expected: FAIL (unknown sentinels / fields).

- [ ] **Step 3: Add the sentinels and put-request fields.** Next to `ErrRuntimeSpecVisibleDevicesModeInvalid`:

```go
	ErrRuntimeSpecAPITokenModeInvalid            = errors.New("runtime_spec.api_token_mode_invalid")
	ErrRuntimeSpecAPITokenNoPlaceholder          = errors.New("runtime_spec.api_token_no_placeholder")
	ErrRuntimeSpecAPITokenPlaceholderWithoutMode = errors.New("runtime_spec.api_token_placeholder_without_mode")
	ErrRuntimeSpecAPITokenHeaderInvalid          = errors.New("runtime_spec.api_token_header_invalid")
```

In `PutRuntimeSpecRequest`, next to its `VisibleDevicesMode`:

```go
	APITokenMode         string  `json:"api_token_mode"`
	APITokenHeaderSource string  `json:"api_token_header_source"`
	APITokenHeader       string  `json:"api_token_header"`
	// APIToken is write-only: nil = keep, "" = clear, value = replace-and-seal.
	APIToken *string `json:"api_token"`
	// APITokenRotate, when true under mode "random", forces regeneration.
	APITokenRotate bool `json:"api_token_rotate"`
```

- [ ] **Step 4: Add the validation.** In the validation function (the one already checking `visible_devices_mode`), after normalizing an empty `APITokenMode` to `app` and an empty `APITokenHeaderSource` to `app`, add:

```go
	mode := req.APITokenMode
	if mode == "" {
		mode = string(routing.RuntimeAPITokenModeApp)
	}
	if !validRuntimeAPITokenMode(mode) {
		return ErrRuntimeSpecAPITokenModeInvalid
	}
	hasPlaceholder := specHasAPITokenPlaceholder(req.Env, req.Args)
	switch routing.RuntimeAPITokenMode(mode) {
	case routing.RuntimeAPITokenModeSet, routing.RuntimeAPITokenModeRandom:
		if !hasPlaceholder {
			return ErrRuntimeSpecAPITokenNoPlaceholder
		}
	case routing.RuntimeAPITokenModeOff:
		if hasPlaceholder {
			return ErrRuntimeSpecAPITokenPlaceholderWithoutMode
		}
	}
	src := req.APITokenHeaderSource
	if src == "" {
		src = string(routing.RuntimeAPITokenHeaderSourceApp)
	}
	switch routing.RuntimeAPITokenHeaderSource(src) {
	case routing.RuntimeAPITokenHeaderSourceApp:
		// header inherited from the app; req.APITokenHeader ignored.
	case routing.RuntimeAPITokenHeaderSourceCustom:
		if _, err := checkHeaderName(req.APITokenHeader); err != nil {
			return ErrRuntimeSpecAPITokenHeaderInvalid
		}
	default:
		return ErrRuntimeSpecAPITokenHeaderInvalid
	}
```

Add the helpers (place near the validation function):

```go
func validRuntimeAPITokenMode(s string) bool {
	switch routing.RuntimeAPITokenMode(s) {
	case routing.RuntimeAPITokenModeOff, routing.RuntimeAPITokenModeSet,
		routing.RuntimeAPITokenModeRandom, routing.RuntimeAPITokenModeApp:
		return true
	}
	return false
}

// specHasAPITokenPlaceholder reports whether the literal "${API_TOKEN}" appears
// in any Env value or any Args element.
func specHasAPITokenPlaceholder(env map[string]string, args []string) bool {
	const ph = "${API_TOKEN}"
	for _, v := range env {
		if strings.Contains(v, ph) {
			return true
		}
	}
	for _, a := range args {
		if strings.Contains(a, ph) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 5: Run the validation test to verify it passes.** Run: `cd gateway/backend && go test ./internal/portal/ -run <ValidationTestName> -v`. Expected: PASS (all matrix cases).

- [ ] **Step 6: Commit.**

```bash
git add gateway/backend/internal/portal/service_runtime.go
git commit -m "feat(portal): runtime-spec API-token validation + 4 error codes"
```

---

## Task 4: Portal — seal-on-write, random generation, rotate

**Files:**
- Modify: `gateway/backend/internal/portal/service_runtime.go` (`PutRuntimeSpec` ~:435 — the persist path, after validation, before the store write)
- Test: the portal put test (same file as Task 3's test)

**Interfaces:**
- Consumes: `capture.SealSecret(s.cipher, s.settingsVolatile, plain)`, `capture.ErrKeyRequired`, `generateSecret()` (`portal/service.go:4106`), the write-only sentinel pattern from `service_applications.go` (`Update` branch ~:666-676).
- Produces: the persisted `routing.RuntimeSpec` carrying a sealed `APIToken` (or `""`); `PutRuntimeSpec` returns `ErrKeyRequired` unwrapped for a keyless store on set/random.

- [ ] **Step 1: Write the failing seal/rotate tests.**
  - **set stores sealed, never echoes:** PUT mode `set` with `api_token: "s3cr3t"` and `VLLM_API_KEY=${API_TOKEN}` → the stored `RuntimeSpec.APIToken` is a sealed envelope (`strings.HasPrefix(stored, "enc:") || strings.HasPrefix(stored, "plain:")`), decrypts back to `"s3cr3t"`, the returned DTO has `APITokenSet==true`, and `"s3cr3t"` appears nowhere in the DTO JSON.
  - **random generates + seals:** PUT mode `random` (no `api_token` field) with the placeholder → stored token non-empty + sealed + `APITokenSet==true`; the value is not any caller-supplied string.
  - **update sentinel:** PUT mode `set`, `api_token: null` keeps the prior stored value; `api_token: ""` clears it (`APITokenSet==false`).
  - **rotate:** PUT mode `random`, `api_token_rotate: true` → stored token differs from the previous stored token.
  - **keyless disk store → 400, nothing persisted:** with a disk store and no cipher, PUT mode `set` (or `random`) → `errors.Is(err, capture.ErrKeyRequired)` and the spec's stored `APIToken` is unchanged (still empty).

- [ ] **Step 2: Run it to verify it fails.** Run: `cd gateway/backend && go test ./internal/portal/ -run <PutRuntimeSpecTokenTestName> -v`. Expected: FAIL.

- [ ] **Step 3: Implement the seal/rotate persist logic.** In `PutRuntimeSpec`, after validation and before writing the spec, compute the sealed token to persist, mirroring `service_applications.go`'s update-sentinel handling:

```go
	mode := req.APITokenMode
	if mode == "" {
		mode = string(routing.RuntimeAPITokenModeApp)
	}
	sealedToken := existing.APIToken // keep by default (existing = the loaded spec)
	switch routing.RuntimeAPITokenMode(mode) {
	case routing.RuntimeAPITokenModeRandom:
		if existing.APIToken == "" || req.APITokenRotate {
			raw, err := generateSecret()
			if err != nil {
				return RuntimeSpecDTO{}, err
			}
			sealed, err := capture.SealSecret(s.cipher, s.settingsVolatile, raw)
			if err != nil {
				return RuntimeSpecDTO{}, err // ErrKeyRequired ⇒ 400, nothing persisted
			}
			sealedToken = sealed
		}
	case routing.RuntimeAPITokenModeSet:
		if req.APIToken != nil { // nil = keep
			sealed, err := capture.SealSecret(s.cipher, s.settingsVolatile, *req.APIToken)
			if err != nil {
				return RuntimeSpecDTO{}, err
			}
			sealedToken = sealed // "" seals to "" ⇒ cleared
		}
	default: // app, off — no per-spec token stored
		sealedToken = ""
	}
```

Assign `spec.APITokenMode = mode`, `spec.APIToken = sealedToken`, `spec.APITokenHeaderSource` (normalized), `spec.APITokenHeader` (only meaningful for `custom`) before the store write. **Compute `sealedToken` BEFORE the store write** so an `ErrKeyRequired` returns without persisting anything.

- [ ] **Step 4: Run the seal/rotate tests to verify they pass.** Run: `cd gateway/backend && go test ./internal/portal/ -run <PutRuntimeSpecTokenTestName> -v`. Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add gateway/backend/internal/portal/service_runtime.go
git commit -m "feat(portal): seal set/random runtime-spec tokens (seal-or-400), rotate"
```

---

## Task 5: Portal — resolve-and-push decrypted token to the agent (fail-closed) + https warning

**Files:**
- Modify: the agent-push builder that converts a `routing.RuntimeSpec` into the wire `runtime.Spec` pushed to the agent (find it: `grep -rn "VisibleDevicesMode" gateway/backend/internal | grep -i "push\|agent\|wire\|config"` — it maps `visible_devices_mode` into the pushed payload; the same site maps `api_token`)
- Modify/Create: a small helper file for `resolvePushToken` + the https check
- Test: a push-builder test (mirror the one asserting `visible_devices_mode` reaches the pushed payload)

**Interfaces:**
- Consumes: `capture.OpenSecret(s.cipher, sealed)`; `routing.RuntimeSpec` + parent `routing.Application`; the wire `runtime.Spec` struct field `APIToken` (Task 9 adds the agent-side json tag; the gateway's push DTO must carry the same field — add it to the gateway's push struct if it is a distinct type).
- Produces: pushed wire spec whose `api_token` = the decrypted token for the mode (`set`/`random`→`OpenSecret(spec.APIToken)`; `app`→`OpenSecret(app.APIToken)`; `off`→`""`), empty on any decrypt error; helper `resolvePushToken(spec routing.RuntimeSpec, app routing.Application) string`.

- [ ] **Step 1: Write the failing push test.**
  - mode `set`, sealed `spec.APIToken` decrypting to `"tok-set"` → pushed spec `APIToken == "tok-set"`.
  - mode `app`, `app.APIToken` decrypting to `"tok-app"` → pushed `APIToken == "tok-app"`.
  - mode `off` → pushed `APIToken == ""`.
  - **fail-closed:** mode `set` with a `spec.APIToken` that `OpenSecret` cannot decrypt (e.g. `"enc:garbage"` under a mismatched/absent cipher) → pushed `APIToken == ""` (NOT the sealed bytes, NOT a partial value) and no error propagates that would push the raw value.

- [ ] **Step 2: Run it to verify it fails.** Run: `cd gateway/backend && go test ./internal/portal/ -run <PushTokenTestName> -v`. Expected: FAIL.

- [ ] **Step 3: Implement `resolvePushToken` (fail-closed).**

```go
// resolvePushToken returns the DECRYPTED upstream token to inject into the child
// for this spec's mode, or "" for off/app-unset. It is the ONLY place the gateway
// decrypts the token, and it NEVER stores the plaintext. On any decrypt failure it
// returns "" (fail-closed): the agent then hard-errors at launch on an unresolved
// ${API_TOKEN}, rather than the child booting with a garbled/partial secret.
func (s *Service) resolvePushToken(spec routing.RuntimeSpec, app routing.Application) string {
	var sealed string
	switch routing.RuntimeAPITokenMode(spec.APITokenMode) {
	case routing.RuntimeAPITokenModeSet, routing.RuntimeAPITokenModeRandom:
		sealed = spec.APIToken
	case routing.RuntimeAPITokenModeApp, "":
		sealed = app.APIToken
	default: // off
		return ""
	}
	if sealed == "" {
		return ""
	}
	tok, err := capture.OpenSecret(s.cipher, sealed)
	if err != nil {
		// Never log the value; a fail-closed empty is the safe outcome.
		return ""
	}
	return tok
}
```

Set the pushed wire spec's `APIToken` from `resolvePushToken(spec, app)` at the same place `VisibleDevicesMode` is mapped into the push payload.

- [ ] **Step 4: Add the https non-blocking warning.** When the resolved mode is non-`off` and the configured gateway URL scheme is not `https`, log one warning (no token value) at push-build time, e.g. `s.log.Warn("runtime-spec API token pushed over a non-https gateway URL; token travels in clear", "mapping_id", spec.MappingID)`. Locate the gateway's own public/base URL in settings (`grep -rn "PublicURL\|GatewayURL\|BaseURL\|https" gateway/backend/internal/config`); if no such value is available gateway-side, add the warning keyed off the agent connection's scheme where the push is sent. Keep it non-blocking. (The portal also surfaces this to the operator in Task 15's UI; this step is the server-side log guard.)

- [ ] **Step 5: Run the push test to verify it passes.** Run: `cd gateway/backend && go test ./internal/portal/ -run <PushTokenTestName> -v`. Expected: PASS.

- [ ] **Step 6: Commit.**

```bash
git add gateway/backend/internal/portal/
git commit -m "feat(portal): push decrypted runtime-spec token to agent, fail-closed; https warning"
```

---

## Task 6: Auth-b — shared `SpecUpstreamAuth` + `resolver.go` `targetFrom`

**Files:**
- Modify: `gateway/backend/internal/routing/runtime_api_token_mode.go` (add the shared helpers)
- Modify: `gateway/backend/internal/routing/resolver.go` (`targetFrom`; the `APIToken: app.APIToken,` literal at ~:960)
- Test: the resolver test that builds a `Target` for a `server_agent` app (find it: `grep -rln "targetFrom\|APIToken" gateway/backend/internal/routing/*_test.go`)

**Interfaces:**
- Consumes: `routing.RuntimeSpec`, `routing.Application`.
- Produces: `routing.SpecUpstreamAuth(spec RuntimeSpec, app Application) (token, header string)` returning the **sealed** token + effective header per mode (off→`"",""`; app→`app.APIToken`,effHeader; set/random→`spec.APIToken`,effHeader; empty mode treated as app); `routing.effectiveAPITokenHeader(spec, app) string`.

- [ ] **Step 1: Write the failing resolver/helper test.**
  - `SpecUpstreamAuth` with mode `off` → `("", "")`.
  - mode `app`, `app.APIToken="enc:app"`, `app.APITokenHeader="Authorization"`, header source `app` → `("enc:app", "Authorization")`.
  - mode `set`, `spec.APIToken="enc:spec"`, header source `custom`, header `"X-Api-Key"` → `("enc:spec", "X-Api-Key")`.
  - mode `set`, header source `app`, `app.APITokenHeader="X-App"` → `("enc:spec", "X-App")`.
  - empty mode (zero spec) → app fallback: `(app.APIToken, app.APITokenHeader)`.
  - Then a `targetFrom` case: for a `server_agent` app+mapping whose spec is mode `set` with `spec.APIToken="enc:spec"`, the built `Target.APIToken == "enc:spec"` and `Target.APITokenHeader` is the effective header; for mode `off`, `Target.APIToken == ""` even when `app.APIToken != ""` (off overrides the app token).

- [ ] **Step 2: Run it to verify it fails.** Run: `cd gateway/backend && go test ./internal/routing/ -run <ResolverAuthTestName> -v`. Expected: FAIL.

- [ ] **Step 3: Add the shared helpers** to `runtime_api_token_mode.go`:

```go
// SpecUpstreamAuth returns the SEALED upstream token and the effective
// transmission header the gateway must attach to requests for one mapping's
// server_agent child, per the spec's api_token_mode. Routing never decrypts —
// the edge (server.go upstreamAuthCtx → OpenSecret) does. A zero-value spec
// (empty mode) is treated as "app" = today's behaviour.
func SpecUpstreamAuth(spec RuntimeSpec, app Application) (token, header string) {
	mode := spec.APITokenMode
	if mode == "" {
		mode = string(RuntimeAPITokenModeApp)
	}
	switch RuntimeAPITokenMode(mode) {
	case RuntimeAPITokenModeOff:
		return "", ""
	case RuntimeAPITokenModeSet, RuntimeAPITokenModeRandom:
		return spec.APIToken, effectiveAPITokenHeader(spec, app)
	default: // app
		return app.APIToken, effectiveAPITokenHeader(spec, app)
	}
}

// effectiveAPITokenHeader is app.APITokenHeader unless the spec sets a custom one.
func effectiveAPITokenHeader(spec RuntimeSpec, app Application) string {
	if RuntimeAPITokenHeaderSource(spec.APITokenHeaderSource) == RuntimeAPITokenHeaderSourceCustom {
		return spec.APITokenHeader
	}
	return app.APITokenHeader
}
```

- [ ] **Step 4: Route `targetFrom` through it.** In `resolver.go`, where the `server_agent` spec is loaded and `Target` built (~:960), replace the direct `APIToken: app.APIToken` (and its `APITokenHeader`) with the resolved pair: after loading `spec`, `tok, hdr := SpecUpstreamAuth(spec, app)` and set `Target.APIToken = tok`, `Target.APITokenHeader = hdr`. For code paths with no loaded spec (non-`server_agent`), behaviour is unchanged (they keep `app.APIToken`/`app.APITokenHeader` directly, which equals `SpecUpstreamAuth` on a zero spec).

- [ ] **Step 5: Run the resolver test to verify it passes.** Run: `cd gateway/backend && go test ./internal/routing/ -run <ResolverAuthTestName> -v`. Expected: PASS.

- [ ] **Step 6: Commit.**

```bash
git add gateway/backend/internal/routing/
git commit -m "feat(routing): per-mapping upstream auth via SpecUpstreamAuth in targetFrom"
```

---

## Task 7: Auth-b — benchmark/capacity Target builders (security review I3)

**Files:**
- Modify: `gateway/backend/internal/gateway/benchmark_runner.go` (three Target builders at `:123`, `:524`, `:544`, each `APIToken: tgt.app.APIToken, APITokenHeader: tgt.app.APITokenHeader`)
- Test: the benchmark-runner test covering Target construction (find it: `grep -rln "benchmark" gateway/backend/internal/gateway/*_test.go`)

**Interfaces:**
- Consumes: `routing.SpecUpstreamAuth` (Task 6); the `tgt` context (`tgt.app`, `tgt.server`, and — for a `server_agent` app — the mapping/spec it is probing).
- Produces: benchmark/capacity `Target`s whose `APIToken`/`APITokenHeader` match the per-mapping spec, not blindly the app token.

- [ ] **Step 1: Write the failing benchmark Target test.** For a `server_agent` app whose probed mapping's spec is mode `set` with `spec.APIToken="enc:spec"`, assert the `Target` the benchmark runner builds carries `APIToken == "enc:spec"` (not `app.APIToken`). For a non-`server_agent` app (or a spec in `app`/`off` mode), assert the existing behaviour is preserved (app token for app mode, empty for off).

- [ ] **Step 2: Run it to verify it fails.** Run: `cd gateway/backend && go test ./internal/gateway/ -run <BenchmarkTargetTestName> -v`. Expected: FAIL (Target still carries app token for a set-mode spec).

- [ ] **Step 3: Route the three builders through `SpecUpstreamAuth`.** At each of the three sites, load the `RuntimeSpec` for the mapping being probed (the same way `benchmark_runner.go` already resolves the mapping/spec for a `server_agent` probe — if the runner does not currently hold the spec, load it via the store keyed by the mapping id it targets), then `tok, hdr := routing.SpecUpstreamAuth(spec, tgt.app)` and set `APIToken: tok, APITokenHeader: hdr`. When no per-mapping spec applies (non-`server_agent`), pass a zero `routing.RuntimeSpec{}` so `SpecUpstreamAuth` falls back to the app token (behaviour-preserving). Factor the "load spec + resolve auth" into one small helper if it reads cleanly across the three sites.

- [ ] **Step 4: Run the benchmark test to verify it passes.** Run: `cd gateway/backend && go test ./internal/gateway/ -run <BenchmarkTargetTestName> -v`. Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add gateway/backend/internal/gateway/benchmark_runner.go
git commit -m "fix(gateway): benchmark/capacity Targets resolve the per-mapping token (I3)"
```

---

## Task 8: Gateway endpoints — map the 4 sentinels → 400

**Files:**
- Modify: `gateway/backend/internal/gateway/portal_runtime_endpoints.go` (the runtime-spec PUT handler's error→status switch that already maps `ErrRuntimeSpecVisibleDevicesModeInvalid` → 400)
- Test: the endpoint test for that handler (find it: `grep -rln "portal_runtime_endpoints\|VisibleDevicesModeInvalid" gateway/backend/internal/gateway/*_test.go`)

**Interfaces:**
- Consumes: the four sentinels from Task 3.
- Produces: HTTP 400 + the sentinel's `runtime_spec.*` code string for each.

- [ ] **Step 1: Write the failing endpoint test.** POST/PUT a runtime spec with mode `"bogus"` → HTTP 400 and the response body's error code is `runtime_spec.api_token_mode_invalid`. Add one case per sentinel (mode invalid, no placeholder, placeholder without mode, header invalid).

- [ ] **Step 2: Run it to verify it fails.** Run: `cd gateway/backend && go test ./internal/gateway/ -run <RuntimeEndpointTestName> -v`. Expected: FAIL (falls through to 500).

- [ ] **Step 3: Add the four sentinels to the error switch.** In the same `switch`/`errors.Is` chain that maps `ErrRuntimeSpecVisibleDevicesModeInvalid` to `http.StatusBadRequest`, add `ErrRuntimeSpecAPITokenModeInvalid`, `ErrRuntimeSpecAPITokenNoPlaceholder`, `ErrRuntimeSpecAPITokenPlaceholderWithoutMode`, `ErrRuntimeSpecAPITokenHeaderInvalid` → the same 400 branch (the code string is `err.Error()` per the existing convention). Also map `capture.ErrKeyRequired` from a runtime-spec PUT to 400 if it is not already mapped centrally.

- [ ] **Step 4: Run the endpoint test to verify it passes.** Run: `cd gateway/backend && go test ./internal/gateway/ -run <RuntimeEndpointTestName> -v`. Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add gateway/backend/internal/gateway/portal_runtime_endpoints.go
git commit -m "feat(gateway): map runtime-spec API-token errors to 400"
```

---

## Task 9: Agent — wire field + `${API_TOKEN}` resolution + new masking span (SECURITY-CRITICAL)

> Dispatch this task on the most capable model (Opus). It is the core secret-handling change in the agent.

**Files:**
- Modify: `server-agent/internal/runtime/types.go` (`Spec` struct ~:38, near `VisibleDevicesMode` ~:70)
- Modify: `server-agent/internal/runtime/policy_local.go` (`expandSpec` ~:849; the `expand` closure ~:852; the span-recording site ~:953; `ExpandPlaceholders` ~:791 is the exported entry)
- Test: `server-agent/internal/runtime/policy_local_test.go` (the tests around placeholder expansion + masking; find the `${AGENT_ENV}` span/mask tests)

**Interfaces:**
- Consumes: `Spec.APIToken` (new wire field); the existing `secretSpan{start,end,name}` (`policy_local.go:814`) and `maskSecretSpans` (`command.go:279`).
- Produces: `${API_TOKEN}` resolved from `Spec.APIToken` in Env values and Args; a recorded `secretSpan` covering each substituted range so `ResolvedCommand.Masked` scrubs it; a hard error when `${API_TOKEN}` is present but `Spec.APIToken == ""`.

- [ ] **Step 1: Write the failing agent tests.**
  - **Env resolution + mask:** `expandSpec` (via `ExpandPlaceholders`) with `Spec.APIToken="tok-abc"` and `Env` containing `VLLM_API_KEY=${API_TOKEN}` → the resolved env value equals `VLLM_API_KEY=tok-abc`, and the masked/reported command does NOT contain `tok-abc` (it shows the placeholder or `****`, exactly like the `${AGENT_ENV}` mask test asserts).
  - **Args resolution + mask:** `Args` `["--api-key", "${API_TOKEN}"]`, `Spec.APIToken="tok-abc"` → resolved args `["--api-key", "tok-abc"]`; masked reported command does not contain `tok-abc`.
  - **hard error on empty token:** `${API_TOKEN}` present with `Spec.APIToken==""` → `ExpandPlaceholders` returns a non-nil error (unresolved-placeholder class).
  - **no false masking:** a spec with NO `${API_TOKEN}` and a non-empty `Spec.APIToken` records no `${API_TOKEN}` span (the token is not injected anywhere, nothing to mask).

- [ ] **Step 2: Run them to verify they fail.** Run: `cd server-agent && go test ./internal/runtime/ -run <PlaceholderTokenTestName> -v`. Expected: FAIL (no `APIToken` field / placeholder not resolved).

- [ ] **Step 3: Add the wire field.** In `runtime/types.go` `Spec`, near `VisibleDevicesMode`:

```go
	// APIToken is the DECRYPTED upstream token the gateway resolved for this
	// spec's api_token_mode (empty for off / app-unset). The agent substitutes
	// it wherever the operator wrote ${API_TOKEN} in Env/Args, and masks it in
	// the reported command. Never persisted by the agent; redacted in file-mode
	// reports (report.go).
	APIToken string `json:"api_token"`
```

- [ ] **Step 4: Resolve `${API_TOKEN}` with its own span.** In the `expand` closure in `expandSpec`, add a `${API_TOKEN}` branch alongside the existing placeholder handling. It must: (a) substitute `spec.APIToken`; (b) return an error if `spec.APIToken == ""`; (c) record a `secretSpan{start, end, name: "API_TOKEN"}` over the substituted range — mirroring the `${AGENT_ENV:NAME}` span code at ~:953 but as its OWN branch (do NOT fold it into the AGENT_ENV branch). Sketch (adapt to the closure's actual variable names — `b` is the `strings.Builder`, `spans` the accumulator):

```go
		// inside the placeholder-dispatch switch/if-chain, a new case:
		case token == "API_TOKEN": // matched "${API_TOKEN}"
			if spec.APIToken == "" {
				return "", nil, fmt.Errorf("unresolved ${API_TOKEN}: no token provided for this spec")
			}
			start := b.Len()
			b.WriteString(spec.APIToken)
			spans = append(spans, secretSpan{start: start, end: b.Len(), name: "API_TOKEN"})
```

Ensure `${API_TOKEN}` is recognized by the same tokenizer that recognizes `${PORT}`/`${MODEL}`/`${AGENT_ENV:...}` (add it to the placeholder name set / dispatch). Confirm `maskSecretSpans` already replaces any recorded span regardless of `name`, so no change is needed there — the mask is span-driven, and the new branch feeds it a span.

- [ ] **Step 5: Run the agent tests to verify they pass.** Run: `cd server-agent && go test ./internal/runtime/ -run <PlaceholderTokenTestName> -v`. Expected: PASS.

- [ ] **Step 6: Commit.**

```bash
git add server-agent/internal/runtime/types.go server-agent/internal/runtime/policy_local.go
git commit -m "feat(agent): resolve ${API_TOKEN} from wire Spec.APIToken with masked span"
```

---

## Task 10: Agent — redact `Spec.APIToken` in file-mode reports (SECURITY-CRITICAL)

> Dispatch on the most capable model (Opus). Prevents a literal token in a hand-written file-mode config from crossing the wire in clear.

**Files:**
- Modify: `server-agent/internal/runtime/report.go` (`redactConfigEnv` ~:105; called from `BuildReport` ~:80)
- Test: `server-agent/internal/runtime/report_test.go` (the `redactConfigEnv` test)

**Interfaces:**
- Consumes: `Config` → its `Spec`s, each now with `APIToken` (Task 9).
- Produces: a redacted `Config` in which every spec's `APIToken` is masked (e.g. set to a fixed `"****"` sentinel or empty), in addition to the existing Env-value redaction.

- [ ] **Step 1: Write the failing redaction test.** Build a `Config` with a spec whose `APIToken == "literal-secret"`, call `redactConfigEnv`, marshal the result, and assert `"literal-secret"` does NOT appear in the output (and the existing Env redaction still holds).

- [ ] **Step 2: Run it to verify it fails.** Run: `cd server-agent && go test ./internal/runtime/ -run <RedactConfigEnvTestName> -v`. Expected: FAIL (token present in output).

- [ ] **Step 3: Redact the field.** In `redactConfigEnv`, in the per-spec copy loop, after the Env map is redacted, set the copied spec's `APIToken` to the redaction sentinel when non-empty (mirror the Env-value redaction constant already used in this function). Keep it a copy — do not mutate the caller's `Config`.

- [ ] **Step 4: Run the redaction test to verify it passes.** Run: `cd server-agent && go test ./internal/runtime/ -run <RedactConfigEnvTestName> -v`. Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add server-agent/internal/runtime/report.go
git commit -m "fix(agent): redact Spec.APIToken in file-mode reports (I4)"
```

---

## Task 11: Agent — capability flag + Version bump

**Files:**
- Modify: `server-agent/internal/agent/features.go` (`var Features` ~:43)
- Modify: `server-agent/internal/agent/agent.go` (`const Version` :89)
- Test: the feature-registry test (`grep -rln "TestFeatureRegistry\|Since" server-agent/internal/agent/*_test.go`)

**Interfaces:**
- Consumes: the `Feature{Name, Since}` shape already in `features.go`.
- Produces: a `Feature{Name: "runtime_api_token", Since: "0.5.0"}` entry; `Version = "0.5.0"`.

- [ ] **Step 1: Write/adjust the failing test.** Assert `Features` contains a feature named `runtime_api_token` with `Since == "0.5.0"`, and that `TestFeatureRegistry`'s invariant (every `Since` ≤ `Version`) holds with `Version == "0.5.0"`.

- [ ] **Step 2: Run it to verify it fails.** Run: `cd server-agent && go test ./internal/agent/ -run TestFeatureRegistry -v`. Expected: FAIL (feature missing / Since > Version at 0.4.0).

- [ ] **Step 3: Add the flag + bump Version.** Append to `Features` (append-only, keep ordering convention): `{Name: "runtime_api_token", Since: "0.5.0"}` (match the exact struct field names/style of the neighbouring entries). Change `const Version = "0.4.0"` to `const Version = "0.5.0"`.

- [ ] **Step 4: Run the agent tests to verify they pass.** Run: `cd server-agent && go test ./internal/agent/ -v`. Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add server-agent/internal/agent/features.go server-agent/internal/agent/agent.go
git commit -m "feat(agent): capability flag runtime_api_token; Version 0.5.0"
```

---

## Task 12: Frontend — types + state

**Files:**
- Modify: the runtime-spec TypeScript type + the API client mapper (find them: `grep -rln "visible_devices_mode\|visibleDevicesMode" gateway/frontend/src`)
- Test: the frontend unit test for that mapper, if one exists (mirror the `visibleDevicesMode` coverage)

**Interfaces:**
- Produces: TS fields `apiTokenMode`, `apiTokenSet`, `apiTokenHeaderSource`, `apiTokenHeader`, `appApiTokenSet`, `appApiTokenHeader` on the runtime-spec type; the PUT payload type gains `apiToken?: string | null`, `apiTokenRotate?: boolean`, `apiTokenMode`, `apiTokenHeaderSource`, `apiTokenHeader`.

- [ ] **Step 1: Write/extend the failing type-mapper test** (if the repo has one for `visibleDevicesMode`): assert a GET response with the six `api_token*`/`app_api_token*` fields maps to the camelCase TS fields, and a form state maps back to the snake_case PUT payload including `api_token`/`api_token_rotate`. If no mapper test exists, skip to Step 3 and rely on the component test in Task 13.

- [ ] **Step 2: Run it to verify it fails** (if written). Run: `cd gateway/frontend && npm test -- <mapperTest>`. Expected: FAIL.

- [ ] **Step 3: Add the fields** to the runtime-spec type and PUT payload type, mirroring `visibleDevicesMode`'s snake↔camel mapping in the API client.

- [ ] **Step 4: Run the test to verify it passes** (if written). Expected: PASS. Otherwise run `cd gateway/frontend && npm run build` to confirm the types compile.

- [ ] **Step 5: Commit.**

```bash
git add gateway/frontend/src
git commit -m "feat(frontend): runtime-spec API-token types"
```

---

## Task 13: Frontend — mode select + write-only token field + rotate + app-unset hint

**Files:**
- Modify: the runtime-spec editor component (the one rendering the `VisibleDevicesMode` select + the GPU hints — `grep -rln "visibleDevicesMode\|VisibleDevicesMode" gateway/frontend/src`)
- Modify: i18n de/en resource files (the keys added here; parity is compile-enforced)
- Test: the component test for the editor (mirror the visible-devices-mode select test)

**Interfaces:**
- Consumes: Task 12 types.
- Produces: a mode `<SelectField>` (default `App-Token verwenden`), a write-only token input shown for `set`, a "Neu erzeugen (rotieren)" button shown for `random`, and the app-unset hint under `app`.

- [ ] **Step 1: Write the failing component test.** Render the editor for a spec whose mode is `app` and whose `appApiTokenSet` is `false`; assert the app-unset hint text is shown. Switch mode to `set`; assert the write-only token field appears and the `api_token_set` indicator renders. Switch to `random`; assert the rotate button appears and no plaintext token field is shown.

- [ ] **Step 2: Run it to verify it fails.** Run: `cd gateway/frontend && npm test -- <EditorTest>`. Expected: FAIL.

- [ ] **Step 3: Implement the mode UI.** Add the mode `<SelectField>` mirroring the `VisibleDevicesMode` select, options in order `App-Token verwenden` / `Operator setzt` / `Zufällig (vom Gateway erzeugt)` / `Aus`, default `app`. For `set`: a write-only token input (mirror the app-token field) bound to the PUT `apiToken` sentinel + the `apiTokenSet` "gesetzt/nicht gesetzt" indicator. For `random`: a rotate button that sets `apiTokenRotate: true` on the next save; state that the value is never shown. For `app` with `appApiTokenSet === false`: render the hint "Die Anwendung hat keinen API-Token hinterlegt — die Authentifizierung ist für diese Zuordnung ausgeschaltet (wie bei der Anwendung selbst)." Add all strings to de + en.

- [ ] **Step 4: Run the component test to verify it passes.** Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add gateway/frontend/src
git commit -m "feat(frontend): runtime-spec API-token mode select, write-only field, rotate, app-unset hint"
```

---

## Task 14: Frontend — header source select + inherited-header display + custom-header warning

**Files:**
- Modify: the same editor component + i18n de/en
- Test: the editor component test

**Interfaces:**
- Consumes: Task 12/13 state; `appApiTokenHeader`.
- Produces: a header-source `<SelectField>` (`Von der Anwendung übernehmen` default / `Eigener Header`), a read-only inherited-header display for `app`, a custom header field + warning for `custom`, shown only when mode ≠ `off`.

- [ ] **Step 1: Write the failing component test.** With mode ≠ `off` and header source `app` and `appApiTokenHeader === ""`, assert the display reads `Authorization: Bearer (Standard)`; with `appApiTokenHeader === "X-Api-Key"`, assert it reads `X-Api-Key`. Switch source to `custom`, type a non-empty header, assert the mismatch warning renders. With mode `off`, assert the whole header block is hidden.

- [ ] **Step 2: Run it to verify it fails.** Run: `cd gateway/frontend && npm test -- <EditorTest>`. Expected: FAIL.

- [ ] **Step 3: Implement the header UI.** Header-source `<SelectField>` default `app`. For `app`: read-only display of the effective app header (`Authorization: Bearer (Standard)` when `appApiTokenHeader` is empty, else the header name). For `custom`: a free-text header field, default/placeholder `Authorization: Bearer`, plus an inline warning that a non-empty custom header must match what the backend expects (against a Bearer-only backend it leaves the child unauthenticated). Hide the block when mode is `off`. Add strings to de + en.

- [ ] **Step 4: Run the component test to verify it passes.** Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add gateway/frontend/src
git commit -m "feat(frontend): runtime-spec token header source + inherited-header display + warning"
```

---

## Task 15: Frontend — hints (backend var table + Args warning + structural backend banner)

**Files:**
- Modify: the same editor component + i18n de/en
- Test: the editor component test

**Interfaces:**
- Consumes: Task 13/14 state; the spec's `Args` field value; the parent application `type` (`app.Type`) for the backend banner.
- Produces: the hint block (shown when mode ≠ `off`), the per-backend variable table, a loud `${API_TOKEN}`-in-Args warning, and the structural unsupported-backend banner.

- [ ] **Step 1: Write the failing component test.**
  - With mode ≠ `off`, assert the hint block + the variable table (vLLM `VLLM_API_KEY=${API_TOKEN}`, llama.cpp `LLAMA_API_KEY=${API_TOKEN}`, TGI `API_KEY=${API_TOKEN}`) render.
  - Put `${API_TOKEN}` into the Args field → assert the loud Args-leak warning renders; remove it → warning gone.
  - Set `app.Type` to `ollama` (and separately `lmstudio`, `llama-swap`) with mode ≠ `off` → assert the per-backend unsupported banner renders; set it to `vllm` → banner gone.

- [ ] **Step 2: Run it to verify it fails.** Run: `cd gateway/frontend && npm test -- <EditorTest>`. Expected: FAIL.

- [ ] **Step 3: Implement the hints.** Render (when mode ≠ `off`): the instruction to reference the token as `${API_TOKEN}` in Env (recommended) or Args; the three-row backend table with the Env/Args/Client-Header columns, the Args column marked `⚠ auslesbar`; a prominent non-blocking warning whenever the Args field contains `${API_TOKEN}` (readable via process listing / server logs); and a per-backend banner when `app.Type ∈ {ollama, lmstudio, llama-swap}` stating the token cannot secure that model server headlessly (warn, not block). Add all strings to de + en. (Backend type string values: confirm against the app-type enum in the frontend — `grep -rn "ollama\|lmstudio\|llama-swap\|llama_swap" gateway/frontend/src`.)

- [ ] **Step 4: Run the component test to verify it passes.** Expected: PASS.

- [ ] **Step 5: Run prettier + build.** Run: `cd gateway/frontend && npm run format:check && npm run build && npm run lint`. Expected: all clean (fix formatting with `npm run format` if `format:check` fails).

- [ ] **Step 6: Commit.**

```bash
git add gateway/frontend/src
git commit -m "feat(frontend): runtime-spec token hints, Args-leak warning, unsupported-backend banner"
```

---

## Task 16: Docs — architecture + ADR

**Files:**
- Modify: `docs/architecture/agent-runtime-manager.md` (the placeholder catalog + the token modes + the on-the-wire posture)
- Modify: `docs/architecture/api-surface.md` (the runtime-spec DTO fields + the 4 error codes)
- Modify: `docs/architecture/data-model.md` (migration 74 columns)
- Create: `docs/architecture/decisions/NNNN-runtime-spec-api-token.md` (next ADR number — check the directory)
- Test: none (docs) — verify the placeholder catalog now lists `${API_TOKEN}` and note where it differs from the non-masked placeholders.

- [ ] **Step 1: Update `agent-runtime-manager.md`.** Add `${API_TOKEN}` to the placeholder catalog, documenting: valid in Env (recommended) + Args (warned); resolved from the wire `Spec.api_token`; masked in the reported command via its own secret span (contrast with `${PORT}`/`${MODEL}`/`${*_DEVICES}`, which are not masked); a hard launch error when present with an empty token. Document the four modes (`app` default / `set` / `random` / `off`), the seal-at-rest + fail-closed-on-decrypt posture, and the https-on-the-wire requirement.

- [ ] **Step 2: Update `api-surface.md`.** Add the runtime-spec DTO read fields (`api_token_mode`, `api_token_set`, `api_token_header_source`, `api_token_header`, `app_api_token_set`, `app_api_token_header`), the write-only `api_token` sentinel + `api_token_rotate`, and the four `runtime_spec.api_token_*` 400 codes.

- [ ] **Step 3: Update `data-model.md`.** Document migration 74's four `agent_runtime_specs` columns with their defaults and the deliberate `app` default rationale (back-compat).

- [ ] **Step 4: Write the ADR.** Record the decision: gateway-owned token (not agent-generated), delivered by wire field + authenticated per-mapping at the edge; default `app` for back-compat; `${API_TOKEN}` masking as new code; Args allowed with a loud warning; structural per-backend limits (Ollama/LM Studio/llama-swap). Mirror the format of the newest existing ADR in that directory.

- [ ] **Step 5: Commit.**

```bash
git add docs/architecture
git commit -m "docs: runtime-spec API token — runtime-manager, api-surface, data-model, ADR"
```

---

## Task 17: Full verification, cleanup, PR

**Files:** none (verification + housekeeping).

- [ ] **Step 1: Backend tests (sqlite driver).** Run: `cd gateway/backend && go test ./...`. Expected: PASS.

- [ ] **Step 2: Backend tests (Postgres driver).** Run the repo's Postgres test path (check `AGENTS.md` / Makefile for the exact target, e.g. `make test-go-postgres` or a `TEST_DATABASE_URL`-gated run). Expected: PASS — this exercises migration 74 on Postgres.

- [ ] **Step 3: Agent module tests.** Run: `cd server-agent && go test ./...`. Expected: PASS (including `TestFeatureRegistry` at Version 0.5.0).

- [ ] **Step 4: Frontend gates.** Run: `cd gateway/frontend && npm run format:check && npm run lint && npm run build && npm test`. Expected: all clean.

- [ ] **Step 5: Cross-module version/capability rule.** Run the repo's capability/version consistency check (the test that pins the agent `Version` ↔ gateway's expected features — `grep -rln "runtime_api_token\|Version" gateway/backend/internal` and run that package's tests). Expected: the gateway recognizes `runtime_api_token`.

- [ ] **Step 6: Sonar / static analysis** if the repo runs it locally (check `AGENTS.md`). Address any new blocker.

- [ ] **Step 7: Grep for leaks.** Confirm the token value is never logged or returned: `grep -rn "APIToken" gateway/backend/internal/portal gateway/backend/internal/routing server-agent/internal/runtime | grep -i "log\|print\|json"` — verify every hit is either a masked/redacted path, a sealed value, or the `api_token_set` boolean.

- [ ] **Step 8: Remove branch-local working files.** Run: `git rm -r docs/superpowers && git commit -m "chore: remove branch-local superpowers docs before PR"`. (Per AGENTS.md, `docs/superpowers/**` never reaches `main`.)

- [ ] **Step 9: Push + open the PR** against `main`. Ensure the branch is a real branch (not detached HEAD): `git branch --show-current` should print `runtime-spec-api-token`; if empty, `git switch -c runtime-spec-api-token` first. Then `git push -u origin runtime-spec-api-token` and open the PR with a summary of the four modes, the back-compat default, the security posture (sealed, masked, fail-closed, https), and the structural backend limits.

---

## Self-Review

**Spec coverage** (§ → task):
- §2 modes (default `app`) → Tasks 1, 3, 4, 6, 13. `off`/`app`/`set`/`random` semantics → Tasks 4 (persist), 5 (push), 6 (auth-b).
- §3 data model / migration 74 → Task 1. Sealed-only-at-rest → Tasks 1, 4.
- §4 `${API_TOKEN}` (Env+Args), own masking span, wire field, fail-closed decrypt → Tasks 5 (fail-closed push), 9 (resolution+span). Args warning → Task 15.
- §5 auth (a) push → Task 5; auth (b) targetFrom → Task 6; (c) benchmark builders → Task 7.
- §6 validation, 4 error codes, seal-or-400 for set+random, write-only sentinel, header shape → Tasks 3, 4; endpoint 400 mapping → Task 8; DTO echoes → Task 2.
- §7 portal (mode select, write-only, rotate, header source + inherited display, hints table, Args warning, backend banner, app-unset hint) → Tasks 13, 14, 15. i18n parity → Tasks 13–15.
- §8 agent (wire, resolution, span+mask, file-mode redact, flag, Version) → Tasks 9, 10, 11.
- §9 security (sealed/never-returned/never-logged; https on the wire) → Tasks 4 (seal), 2 (no value in DTO), 5 (https warning + fail-closed), 9/10 (mask+redact), 17 (leak grep).
- §10 out-of-scope (no per-start rotation, no router change, no agent generation) → respected: no such tasks.
- §11 components 1–7 → Tasks 1 / 2–7 / 8 / 9–11 / 12–15 / 16 / 17.

**Placeholder scan:** No "TBD"/"handle edge cases"/"similar to Task N" — mechanical mirror steps name the exact source file+line and the exact values/columns/fields to copy, and every novel/security step carries real code. "Find it: `grep …`" instructions point at a concrete, existing symbol (`visible_devices_mode`) whose sites the implementer must extend — verifiable, not vague.

**Type consistency:** `RuntimeAPITokenMode`/`RuntimeAPITokenHeaderSource` consts (Task 1) are used verbatim in Tasks 3/4/6; `SpecUpstreamAuth(spec, app) (token, header)` (Task 6) is consumed by Task 7; `resolvePushToken(spec, app) string` (Task 5) is portal-only; the four `ErrRuntimeSpecAPIToken*` sentinels (Task 3) are mapped in Task 8; the wire `Spec.APIToken`/`json:"api_token"` (Task 9) is set by the push builder (Task 5) and redacted (Task 10); DTO json tags (`api_token_mode`, `api_token_set`, `api_token_header_source`, `api_token_header`, `app_api_token_set`, `app_api_token_header`) match across Tasks 2 and 12–15.
