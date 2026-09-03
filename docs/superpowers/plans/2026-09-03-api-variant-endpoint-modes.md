# API-Variant Endpoint Modes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the two `native_responses`/`native_messages` booleans with a three-state per-endpoint `EndpointMode` (`disabled` | `translate` | `passthrough`) for Codex (`/v1/responses`) and Claude Code (`/v1/messages`), surfaced as flavor-gated dropdowns on the application form AND — as the identical control block — on every `server_agent` runtime spec.

**Architecture:** Gateway-side only. A new `routing.EndpointMode` type carried on `Application`, `RuntimeSpec`, and the routing `Target`; the resolver surfaces the *effective* (spec-for-`server_agent`, else app) modes onto the `Target`; endpoint-aware candidacy excludes an ordinary app whose mode is `disabled`; `native_passthrough.go` turns the effective mode into passthrough/translate/reject. A shared React `ApiVariantControls` component renders the checkboxes + dropdowns for both forms. No `server-agent/` change, no ServerAgent version bump (the agent's runtime router forwards `/v1/responses` and `/v1/messages` verbatim and routes on `model`).

**Tech Stack:** Go 1.26 (`gateway/backend`, module `op-ai-gateway`), React/TypeScript/Vite (`gateway/frontend`), three store drivers (memory `routing.MemoryStore`; sqlite + postgres via `store.SQLStore` and the `dialect` seam). Go tests: stdlib `testing` + `net/http/httptest`, table-driven, no testify. Frontend tests: Vitest + `@testing-library/react` (jsdom).

## Global Constraints

- **Stable error codes are the wire contract** — dotted lowercase `noun.reason`; the sentinel/const string IS the code (AGENTS.md). New codes: `responses.endpoint_disabled`, `messages.endpoint_disabled` (dispatch, HTTP **404**); `application.endpoint_mode_invalid`, `runtime_spec.endpoint_mode_invalid`, `runtime_spec.flavor_invalid` (validation, HTTP 400).
- **Canonical Go names (use verbatim):** `type EndpointMode string` in `internal/routing`; consts `EndpointModeDisabled="disabled"`, `EndpointModeTranslate="translate"`, `EndpointModePassthrough="passthrough"`; `DefaultEndpointMode=EndpointModePassthrough`. Fields `Application.ResponsesMode/MessagesMode EndpointMode` (replace the bools); `RuntimeSpec.APIFlavors []string` + `ResponsesMode/MessagesMode EndpointMode`; `Target.APIFlavors []string` + `ResponsesMode/MessagesMode EndpointMode` (replace the bools).
- **Columns (all `text not null default ''`, api_flavors default `'[]'`):** `applications.responses_mode`, `applications.messages_mode`; `agent_runtime_specs.api_flavors`, `agent_runtime_specs.responses_mode`, `agent_runtime_specs.messages_mode`. Migration number **72**, name `application_endpoint_modes`. The old `native_responses`/`native_messages` columns stay inert (spec §7 — never DROP, never re-add to the frozen baseline).
- **Canonical frontend names:** `export type EndpointMode = 'disabled' | 'translate' | 'passthrough'` in `src/api/models.ts`; JSON keys `responses_mode`/`messages_mode` (applications) and `api_flavors`/`responses_mode`/`messages_mode` (runtime specs). Shared component `src/components/shared/ApiVariantControls.tsx` with props `{ t, apiFlavors, responsesMode, messagesMode, onFlavorsChange, onResponsesModeChange, onMessagesModeChange }`. i18n keys (both `de` and `en`): `applicationResponsesMode`, `applicationMessagesMode`, `applicationModeDisabled`, `applicationModeTranslate`, `applicationModePassthrough`.
- **Uniform create default = `passthrough`** for every application type and for a spec with an absent field (design §6, research-validated). **Migration backfill maps the old bools** (`native_*=1 → 'passthrough'`, `0 → 'translate'`) so no existing app/spec changes behaviour on upgrade. Spec backfill snapshots the parent app via `mapping_id → model_mappings → applications`.
- **AgentRuntimeSpecDTO / `runtime.Spec` / `server-agent/` MUST NOT change** (spec §9). Guard test in B4.
- **Persistence rules:** append-only migrations; wide values need wide columns (ADR-005 — `text` is fine); keep all three drivers green; verify Postgres with `OP_AI_GATEWAY_TEST_POSTGRES_DSN`.
- **i18n parity is compile-enforced** (`PortalMessages = typeof de`, `en: PortalMessages`) — every new key goes in BOTH locales in the same commit. Add German + English together.
- **Backend build coupling (READ THIS):** Renaming the `Application`/`Target` bool fields breaks `internal/store`, `internal/portal`, and `internal/gateway` until each is updated. Tasks B1→B5 are sequenced so **each task's own package tests pass** after it, but the **whole-module gate `make test-go` (and `go build ./...`) only goes green after B5.** This is expected and called out per task; do not try to run the full module build mid-sequence.
- **Frontend tsc coupling:** vitest (esbuild) does not type-check, so a per-file `npm test -- <file>` runs green while sibling `PortalApplication`/`RuntimeSpec` fixtures are stale. `npx tsc --noEmit` / `npm run build` is the gate that catches the fixture ripple; each frontend task that changes a DTO type fixes its own ripple so tsc is green at the task boundary.
- **Every new source file** starts with the repo header:
  `// SPDX-License-Identifier: AGPL-3.0-only` / `// Copyright (C) 2026 OnPrem AI Gateway contributors`.

## File Structure

**Backend — created**
- `gateway/backend/internal/routing/endpoint_mode.go` — the `EndpointMode` type + `Valid`/`OrDefault`/`ParseEndpointMode`.
- `gateway/backend/internal/routing/endpoint_mode_test.go`, `resolver_endpoint_mode_test.go`.
- `gateway/backend/internal/store/migration72_endpoint_modes_test.go`.

**Backend — modified**
- `internal/routing/store.go` (Application/RuntimeSpec fields, `applicationServesEndpoint`), `resolver.go` (`Target`, `resolverStore`, `targetFrom` method, candidacy/affinity filter), `memory_store.go` (`copyRuntimeSpec`).
- `internal/store/migrate.go` (migration 72), `sqlite_applications.go`, `sqlite_runtime.go`, `application_column_parity_test.go`, `routing_store_conformance_test.go`.
- `internal/portal/service_applications.go`, `service_runtime.go`.
- `internal/gateway/native_passthrough.go`, and the seed helpers in `server_test.go` / `native_passthrough_test.go`.

**Frontend — created**
- `gateway/frontend/src/components/shared/ApiVariantControls.tsx` + `.test.tsx`.

**Frontend — modified**
- `src/api/models.ts`, `src/api/runtime.ts`, `src/i18n.ts`.
- `src/components/ApplicationSection.tsx`, `src/components/RuntimeAdminSection.tsx`, `src/components/shared/applicationTypeDefaults.ts`.
- Test-fixture ripple: `src/App.test.tsx`, `src/components/{ApplicationSection,RuntimeAdminSection,ServerList,MappingSection,BenchmarkSection}.test.tsx`, `applicationTypeDefaults.test.ts`, `i18n.test.ts`.

**Docs — modified**
- `docs/architecture/cross-cutting/compatibility-and-inference.md`, `agent-runtime-manager.md`, `docs/architecture/reference/api-surface.md` + the data-model reference; ADR log entry.

## Detailed per-task code (task material)

The exact current-code excerpts (with `file:line`) and the full failing-test + implementation code bodies for every task live alongside this plan in **`docs/superpowers/plan-material/`** — one bundle per subsystem, verified against the branch source:

| Bundle | Covers plan tasks |
|---|---|
| `01-backend-model.md` | B1, B2 |
| `02-backend-portal-dto.md` | B4 |
| `03-backend-store-migration.md` | B3 |
| `04-backend-decision.md` | B5 |
| `05-frontend-app-form.md` | F1, F2, F3 |
| `06-frontend-spec-form-i18n.md` | F4 (already reconciled to the locked `onFlavorsChange` / `applicationMode*` names) |

Where a task step says "verbatim from `0X-*.md`", read that bundle section for the full code. These plan-material files are branch-local and are removed by Task Z1 (with the rest of `docs/superpowers/`) before the PR. If a bundle's suggested name ever conflicts with this plan's **Global Constraints**, the plan wins.

---

## Task B1: `EndpointMode` type (routing)

**Files:**
- Create: `gateway/backend/internal/routing/endpoint_mode.go`
- Test: `gateway/backend/internal/routing/endpoint_mode_test.go`

**Interfaces:**
- Produces: `type EndpointMode string`; `EndpointModeDisabled/Translate/Passthrough`; `DefaultEndpointMode = EndpointModePassthrough`; `(EndpointMode).String()/Valid()/OrDefault()`; `ParseEndpointMode(string) (EndpointMode, error)`.
- Consumes: nothing. This task is purely additive — the whole module still compiles after it.

- [ ] **Step 1: Write the failing test** — `internal/routing/endpoint_mode_test.go`

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

func TestEndpointModeValid(t *testing.T) {
	for _, m := range []EndpointMode{EndpointModeDisabled, EndpointModeTranslate, EndpointModePassthrough} {
		if !m.Valid() {
			t.Errorf("%q should be valid", m)
		}
	}
	for _, m := range []EndpointMode{"", "PASSTHROUGH", "proxy", "native"} {
		if m.Valid() {
			t.Errorf("%q should be invalid", m)
		}
	}
}

func TestParseEndpointMode(t *testing.T) {
	cases := []struct {
		in      string
		want    EndpointMode
		wantErr bool
	}{
		{"disabled", EndpointModeDisabled, false},
		{"translate", EndpointModeTranslate, false},
		{"passthrough", EndpointModePassthrough, false},
		{"  Passthrough ", EndpointModePassthrough, false},
		{"", DefaultEndpointMode, false},
		{"native", "", true},
	}
	for _, c := range cases {
		got, err := ParseEndpointMode(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseEndpointMode(%q): want error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseEndpointMode(%q): unexpected error %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseEndpointMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEndpointModeOrDefault(t *testing.T) {
	if got := EndpointMode("").OrDefault(); got != EndpointModePassthrough {
		t.Errorf(`("").OrDefault() = %q, want passthrough`, got)
	}
	if got := EndpointModeTranslate.OrDefault(); got != EndpointModeTranslate {
		t.Errorf("translate.OrDefault() = %q, want translate", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd gateway/backend && go test ./internal/routing/ -run 'EndpointMode' -count=1`
Expected: build failure `undefined: EndpointMode`.

- [ ] **Step 3: Write minimal implementation** — `internal/routing/endpoint_mode.go`

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"fmt"
	"strings"
)

// EndpointMode is the three-state per-endpoint control that replaced the old
// native_responses / native_messages booleans: for the Codex /v1/responses and
// Claude Code /v1/messages endpoints, whether the gateway disables the endpoint,
// translates the body to /v1/chat/completions, or proxies it raw (pass-through)
// to the upstream's native path. Serialized as its lowercase string, stored text.
type EndpointMode string

const (
	EndpointModeDisabled    EndpointMode = "disabled"
	EndpointModeTranslate   EndpointMode = "translate"
	EndpointModePassthrough EndpointMode = "passthrough"

	// DefaultEndpointMode is the value a fresh application/spec gets when the
	// flavor is enabled: pass-through, because every supported upstream now
	// serves both native endpoints (design §6).
	DefaultEndpointMode = EndpointModePassthrough
)

func (m EndpointMode) String() string { return string(m) }

// Valid reports whether m is one of the three defined modes.
func (m EndpointMode) Valid() bool {
	switch m {
	case EndpointModeDisabled, EndpointModeTranslate, EndpointModePassthrough:
		return true
	default:
		return false
	}
}

// OrDefault resolves an unset ("") mode to DefaultEndpointMode; every other
// value is returned untouched. The read-time default a zero-value in-memory
// Application still needs even though the migration backfills a non-empty value.
func (m EndpointMode) OrDefault() EndpointMode {
	if m == "" {
		return DefaultEndpointMode
	}
	return m
}

// ParseEndpointMode trims + lowercases, maps "" to DefaultEndpointMode, and
// rejects any unrecognized value (a stable validation failure at the DTO edge).
func ParseEndpointMode(s string) (EndpointMode, error) {
	switch m := EndpointMode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return DefaultEndpointMode, nil
	case EndpointModeDisabled, EndpointModeTranslate, EndpointModePassthrough:
		return m, nil
	default:
		return "", fmt.Errorf("routing: invalid endpoint mode %q", s)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd gateway/backend && go test ./internal/routing/ -run 'EndpointMode' -count=1`
Expected: PASS. Also `go build ./...` still passes (additive).

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/routing/endpoint_mode.go gateway/backend/internal/routing/endpoint_mode_test.go
git commit -m "feat(routing): add EndpointMode type for the three-state endpoint control"
```

---

## Task B2: Routing structs, resolver spec-resolution, endpoint-aware candidacy, memory copy

**Files:**
- Modify: `internal/routing/store.go` (Application `:519-524`, RuntimeSpec `:1277`, add `applicationServesEndpoint` after `:1461`), `internal/routing/resolver.go` (`Target` `:64-69`, `resolverStore` `:245-254`, `targetFrom` `:890-906` → method, candidacy at `:416-421`, affinity `:388`/`:583`/`:603`, override `:516`, group `eligibleCandidates` `:955`), `internal/routing/memory_store.go` (`copyRuntimeSpec`, readers `:2254`/`:2261`/`:2267`/`:2281`/`:2290`).
- Test: `internal/routing/resolver_endpoint_mode_test.go` (new), additions to `internal/routing/memory_store_test.go`.

**Interfaces:**
- Consumes: `EndpointMode` (B1).
- Produces: `Application.ResponsesMode/MessagesMode`; `RuntimeSpec.APIFlavors/ResponsesMode/MessagesMode`; `Target.APIFlavors/ResponsesMode/MessagesMode`; `resolverStore.RuntimeSpecByMapping`; `(*Resolver).targetFrom(ctx, server, app, mapping, apiFlavor) (Target, error)`; `applicationServesEndpoint(app, fineFlavor) bool`; `copyRuntimeSpec(RuntimeSpec) RuntimeSpec`. Effective-mode semantics: `server_agent` → resolved spec's values (app fallback when no spec); else app's.

> **Correction to spec §4:** the resolver does NOT read `RuntimeSpec` today (`targetFrom` is a free function with no ctx/spec access, `resolverStore` has no spec method). This task adds that access.

- [ ] **Step 1: Write the failing tests** — `internal/routing/resolver_endpoint_mode_test.go`

Include the four tests verbatim from plan-material `01-backend-model.md` §Task 3–5: `TestTargetCarriesAppEndpointModes`, `TestTargetCarriesSpecModesForServerAgent`, `TestTargetFallsBackToAppModesWhenServerAgentHasNoSpec`, `TestCandidacyExcludesResponsesDisabledOrdinaryApp`, `TestCandidacyDoesNotModeGateServerAgent`. Also add `TestMemoryStoreRuntimeSpecIsolatesAPIFlavors` to `memory_store_test.go` (from `01-backend-model.md` §Task 2). Key assertions:
- app-level modes ride onto `Target.ResponsesMode/MessagesMode`;
- a `server_agent` mapping's resolved spec WINS over the app's modes; no-spec falls back to the app;
- an `openai_responses` request is excluded (`ErrNoModelRoute`) when an *ordinary* app has `ResponsesMode=disabled`, while `openai_chat_completions` still routes;
- a `server_agent` app with app-level `ResponsesMode=disabled` STILL survives candidacy (spec is the authority), and its resolved spec's `passthrough` reaches the Target;
- `UpsertRuntimeSpec`/readers deep-copy `APIFlavors`.

- [ ] **Step 2: Run to verify they fail**

Run: `cd gateway/backend && go test ./internal/routing/ -run 'Target|Candidacy|MemoryStoreRuntimeSpecIsolates' -count=1`
Expected: compile failure (`unknown field ResponsesMode`), then assertion failures.

- [ ] **Step 3: Implement** (all within `internal/routing`)

1. `store.go:519-524` — replace the two bools:
```go
	// ResponsesMode / MessagesMode are the per-application three-state endpoint
	// controls (design 2026-09-03) that replaced native_responses/native_messages:
	// for Codex /v1/responses resp. Claude Code /v1/messages — EndpointModeDisabled
	// (not served), EndpointModeTranslate (translate to /v1/chat/completions), or
	// EndpointModePassthrough (proxy raw). Whether the endpoint is served at all
	// also depends on the matching APIFlavors entry (see applicationServesEndpoint).
	ResponsesMode EndpointMode
	MessagesMode  EndpointMode
```
2. `store.go` RuntimeSpec (insert after `SetVisibleDevices bool`, `:1277`):
```go
	// APIFlavors / ResponsesMode / MessagesMode: the per-spec snapshot of the same
	// controls the parent application carries. For a server_agent mapping the
	// RESOLVED spec is the sole authority for its model's flavors + endpoint modes;
	// the application's values are only the create-time template and the no-spec
	// fallback. Gateway-side only — never on AgentRuntimeSpecDTO / the agent wire.
	APIFlavors    []string
	ResponsesMode EndpointMode
	MessagesMode  EndpointMode
```
3. `store.go` — add `applicationServesEndpoint` (from `01-backend-model.md` Task 5 impl 1) after `applicationHasAPIFlavor` (`:1461`): openai_responses requires `openai` flavor AND (`Type==ProviderServerAgent` OR `ResponsesMode!=disabled`); anthropic_messages the anthropic/MessagesMode analogue; default → `applicationHasAPIFlavor(app, NormalizeAPIFlavor(fineFlavor))`.
4. `resolver.go:64-69` — replace the `Target` bools with `APIFlavors []string` + `ResponsesMode/MessagesMode EndpointMode` (comment from `01-backend-model.md` Task 3 impl 2).
5. `resolver.go:245-254` — add `RuntimeSpecByMapping(ctx context.Context, mappingID string) (RuntimeSpec, bool, error)` to `resolverStore`.
6. `resolver.go:890-906` — replace the free `targetFrom` with the `(*Resolver)` method that resolves the spec for `ProviderServerAgent` and deep-copies flavors (full body in `01-backend-model.md` Task 4 impl 2). Update all four call sites: `:473`, `:538`, `:636`, `:1313` (bodies in Task 4 impl 3).
7. `resolver.go` — add `filterServesEndpoint(cands, fineFlavor)` (Task 5 impl 2), insert it at `:416-421` before the empty check; thread the fine flavor into `resolveAffinity` (`:388` call, `:583` signature, `:603` gate → `applicationServesEndpoint`); insert `mine = filterServesEndpoint(mine, req.APIFlavor)` after `:516` in `resolveServerOverride`; and apply `filterServesEndpoint(cands, g.req.APIFlavor)` inside `eligibleCandidates` (`:955`) so the group path is endpoint-aware too.
8. `memory_store.go` — add `copyRuntimeSpec` (deep-copies `APIFlavors`); apply on write in `UpsertRuntimeSpec` (`:2254`, `:2261`) and on read in `RuntimeSpecByMapping`/`RuntimeSpecByID`/`RuntimeSpecsByApplication`; update the two stale "no slice/pointer fields" comments.
9. Sweep the `internal/routing` **test files** for `NativeResponses`/`NativeMessages` literals and migrate them to `ResponsesMode`/`MessagesMode` so the package compiles (`grep -rn 'NativeResponses\|NativeMessages' internal/routing`). The `resolverStore` test double `groupReadCountingStore` embeds the interface, so the new `RuntimeSpecByMapping` needs no edit there.

- [ ] **Step 4: Run to verify pass**

Run: `cd gateway/backend && go test ./internal/routing/... -count=1`
Expected: PASS (whole `routing` package incl. existing tests). **Note:** `go build ./...` and other packages will NOT compile yet — that is expected until B5 (see Global Constraints).

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/routing/
git commit -m "feat(routing): carry effective endpoint modes on Target + endpoint-aware candidacy

Replace the Application/Target native_* bools with EndpointMode, add the per-spec
snapshot fields, resolve the spec's effective modes for server_agent, and exclude
a disabled ordinary app from that endpoint's candidacy. Cross-package build stays
red until the store/portal/gateway tasks land."
```

---

## Task B3: Store — migration 72, CRUD, parity, conformance

**Files:**
- Modify: `internal/store/migrate.go` (register `{version:72,...}` at `:105`, add `migration72Up`), `sqlite_applications.go` (five SQL lists `:25/:89/:146/:161/:479` + binds + `scanApplication`/`scanMappingCandidate`), `sqlite_runtime.go` (`UpsertRuntimeSpec` cols/binds `:22-44`, `runtimeSpecCols`/`runtimeSpecColsPrefixed` `:60-72`, `scanRuntimeSpec` `:329-346`), `application_column_parity_test.go`, `routing_store_conformance_test.go`.
- Test: `internal/store/migration72_endpoint_modes_test.go` (new).

**Interfaces:**
- Consumes: the `routing` struct fields (B2), `encodeAPIFlavors`/`decodeAPIFlavors`.
- Produces: the five persisted columns + migration 72; all three drivers round-trip the new fields.

- [ ] **Step 1: Write the failing tests** — new `internal/store/migration72_endpoint_modes_test.go`

Use `TestMigration72BackfillApplicationModes` and `TestMigration72SnapshotRuntimeSpec` verbatim from `03-backend-store-migration.md` Task 4 (both wrapped in `forEachDialect`, seeding the pre-72 legacy shape via `mustExec`, calling `reinvokeMigration72`, then asserting `passthrough`/`translate` and the app→spec snapshot). Add the `mustExec` + `reinvokeMigration72` helpers from that bundle. Also extend `TestRoutingStoreRuntimeSpecs` (`routing_store_conformance_test.go:529`) fixture + assertion and add `TestRoutingStoreApplicationEndpointModes` (bundle Task 2/3) so all three drivers round-trip.

- [ ] **Step 2: Run to verify they fail**

Run:
```bash
cd gateway/backend && go test ./internal/store/ -run 'TestMigration72|TestRoutingStoreApplicationEndpointModes|TestRoutingStoreRuntimeSpecs' -count=1
```
Expected: build failure (`migration72Up` undefined / unknown struct fields), then value mismatches.

- [ ] **Step 3: Implement**

1. `migrate.go` — register `{version: 72, name: "application_endpoint_modes", up: migration72Up}` at `:105` and add `migration72Up` verbatim from `03-backend-store-migration.md` Task 4 (adds the two app mode columns `text not null default ''` + backfills via `case when native_responses <> 0 then 'passthrough' else 'translate' end where responses_mode = ''`; adds the spec trio (`api_flavors text not null default '[]'`, two modes) + snapshots from the parent app — postgres `UPDATE ... FROM model_mappings JOIN applications`, sqlite correlated subqueries; both guarded on `= ''`).
2. `sqlite_applications.go` — swap `native_responses`/`native_messages` → `responses_mode`/`messages_mode` in all five SQL lists and binds; in `scanApplication`/`scanMappingCandidate` delete the `int64` bool locals and scan straight into `&app.ResponsesMode`/`&app.MessagesMode` (`database/sql` scans `text` into `*EndpointMode` since its underlying type is `string`).
3. `sqlite_runtime.go` — add `api_flavors, responses_mode, messages_mode` (one fixed position: after `set_visible_devices`, before `created_at`) to the INSERT list, the `on conflict` set-clause, both `runtimeSpecCols*` consts, and `scanRuntimeSpec`; encode with `encodeAPIFlavors(spec.APIFlavors)` on write and `decodeAPIFlavors` on read; bind `string(spec.ResponsesMode)`/`string(spec.MessagesMode)`.
4. `application_column_parity_test.go` — shrink `applicationParityBools` `[6]→[4]` (drop the two native rows, update the `2^r>=` comment), replace the `NativeResponses`/`NativeMessages` `want` fields with per-row-varying `ResponsesMode`/`MessagesMode` strings, and drop `native_*` from both `names` lists (bundle Task 5).

- [ ] **Step 4: Run to verify pass** (both drivers)

Run:
```bash
cd gateway/backend && go test ./internal/store/... ./internal/routing/... -count=1
OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://…?sslmode=disable' go test ./internal/store/... -count=1
```
Expected: PASS on memory + sqlite + postgres. (`internal/portal` and `internal/gateway` still don't build — expected until B5.)

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/store/
git commit -m "feat(store): persist endpoint modes + migration 72 (bool->mode backfill, app->spec snapshot)"
```

---

## Task B4: Portal DTOs + validation (applications + runtime specs) + agent-wire guard

**Files:**
- Modify: `internal/portal/service_applications.go` (DTO `:125-129`, Create `:195-196`, Update `:234-235`, create-map `:392-393`, update-apply `:598-603`, DTO builder `:763-764`, add `ErrApplicationEndpointModeInvalid` near `:35` + `validEndpointMode`), `service_runtime.go` (RuntimeSpecDTO, PutRuntimeSpecRequest, `putRuntimeSpec` validation + spec-build, `runtimeSpecDTO`, `GetRuntimeSpec` not-configured branch, add `ErrRuntimeSpecEndpointModeInvalid`/`ErrRuntimeSpecFlavorInvalid` + `normalizeRuntimeSpecFlavors`).
- Test: `service_applications_test.go`, `service_runtime_test.go`.

**Interfaces:**
- Consumes: `routing.EndpointMode` + the renamed struct fields (B2), `routing.APIFlavor*`.
- Produces: JSON `responses_mode`/`messages_mode` (applications), `api_flavors`/`responses_mode`/`messages_mode` (runtime specs); the three validation error codes; `validEndpointMode`, `normalizeRuntimeSpecFlavors`. Guarantees `AgentRuntimeSpecDTO` unchanged.

- [ ] **Step 1: Write the failing tests**

Add, verbatim from `02-backend-portal-dto.md`: `TestApplicationDTOCarriesEndpointModes`, the two `TestCreateApplicationValidation` bad-mode cases + `TestCreateApplicationEndpointModeDefaultsToPassthrough`, `TestUpdateApplicationEndpointModes` (keep-if-nil, explicit set, reject non-nil unknown incl. `""`), `TestRuntimeSpecDTOCarriesFlavorsAndModes`, the two `TestPutRuntimeSpecValidation` bad cases + `TestPutRuntimeSpecModeAndFlavorDefaults`, `TestPutRuntimeSpecDoesNotInheritAppModes` (backend defaults to passthrough, does NOT inherit the parent app), and the guard `TestAgentRuntimeConfigOmitsFlavorsAndModes` (marshals `AgentRuntimeConfig` and asserts none of `api_flavors`/`responses_mode`/`messages_mode` appear).

- [ ] **Step 2: Run to verify they fail**

Run: `cd gateway/backend && go test ./internal/portal/... -count=1`
Expected: compile failure (unknown DTO fields) then assertion failures.

- [ ] **Step 3: Implement** (from `02-backend-portal-dto.md` Tasks A0–A3, R1–R4)

- `ApplicationDTO`/`CreateApplicationRequest`/`UpdateApplicationRequest`: replace the two bool fields with `responses_mode`/`messages_mode` (`string`; Update is `*string`). Add `ErrApplicationEndpointModeInvalid = errors.New("application.endpoint_mode_invalid")` and `validEndpointMode(raw) (routing.EndpointMode, bool)`.
- `CreateApplication`: default each mode to `passthrough` when the field is blank, else `validEndpointMode` or `ErrApplicationEndpointModeInvalid`; write into the `routing.Application` literal.
- `UpdateApplication`: validate-before-mutate for each non-nil pointer (reject unknown incl. `""`), then apply in the mutate block.
- `applicationDTO()`: emit `string(app.ResponsesMode)`/`string(app.MessagesMode)`.
- `RuntimeSpecDTO`/`PutRuntimeSpecRequest`: add `APIFlavors []string` (json `api_flavors`) + `ResponsesMode`/`MessagesMode string`. Add `ErrRuntimeSpecEndpointModeInvalid = errors.New("runtime_spec.endpoint_mode_invalid")`, `ErrRuntimeSpecFlavorInvalid = errors.New("runtime_spec.flavor_invalid")`, and `normalizeRuntimeSpecFlavors` (a copy of `normalizeApplicationFlavors` with the spec error code — keep the separate honest code, spec §13).
- `putRuntimeSpec`: validate `respMode`/`msgMode` (default passthrough) + `flavors, err := normalizeRuntimeSpecFlavors(req.APIFlavors)`; set them in the `routing.RuntimeSpec` literal. **Do NOT read the parent `app` to inherit** (frontend pre-fills; backend only defaults absent fields — spec §5.4/§12).
- `runtimeSpecDTO()`: emit `append([]string{}, spec.APIFlavors...)` (never JSON `null`) + the two mode strings. `GetRuntimeSpec` not-configured branch: add `APIFlavors: []string{}` (modes stay `""`; `Configured:false` tells the frontend to pre-fill).

- [ ] **Step 4: Run to verify pass**

Run: `cd gateway/backend && go test ./internal/portal/... -count=1`
Expected: PASS. (`internal/gateway` still doesn't build — expected until B5.)

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/portal/
git commit -m "feat(portal): endpoint-mode DTOs + validation for applications and runtime specs

Agent wire unchanged (guard test); backend defaults absent fields to passthrough,
does not inherit the parent app (frontend snapshots)."
```

---

## Task B5: Gateway dispatch — three-way decision + disabled rejection + error codes

**Files:**
- Modify: `internal/gateway/native_passthrough.go` (`nativePassthroughEnabled` → `endpointModeFor` `:31-43`, `upstreamPath` `:66`, the decision block `:153-166`, add the code/status consts + `endpointDisabledError`, refresh the two stale doc blocks), `internal/gateway/server_test.go` + `native_passthrough_test.go` (seed helpers).
- Test: `internal/gateway/native_passthrough_test.go`, `server_test.go`.

**Interfaces:**
- Consumes: `routing.EndpointMode`, `Target.ResponsesMode/MessagesMode` (effective; B2).
- Produces: `responses.endpoint_disabled`/`messages.endpoint_disabled` (HTTP 404); the contract that `tryProxyNative` returns `true` for both `passthrough` and `disabled` (handled, don't translate) and `false` only for `translate`.

- [ ] **Step 1: Write the failing tests**

First refactor the seed helper: replace `newNativeProxyTestServer` body with `newNativeModeTestServer(prov, responsesMode, messagesMode routing.EndpointMode)` + a bool-compat wrapper (`true→passthrough`, `false→translate`) via `modeFromBool` — full code in `04-backend-decision.md` Task 04.0; also fix `newCapAdmissionTestServer:563` (`NativeResponses:true` → `ResponsesMode: routing.EndpointModePassthrough`). Then add `TestEndpointModeForMapsFlavorToTargetMode`, `TestOpenAIResponsesDisabledEndpointRejects`, `TestAnthropicMessagesDisabledEndpointRejects`, and `TestResponsesEndpointModeTable` (all verbatim from `04-backend-decision.md` Tasks 04.1–04.2 — the disabled tests use a SIMPLE body that would translate+succeed with 200, so a 404 proves the disabled branch fired and did not fall through; they also assert `proxyCalls==0` and one `status="error"` usage event with the stable code + `HTTPStatus==404`).

- [ ] **Step 2: Run to verify they fail**

Run: `cd gateway/backend && go test ./internal/gateway/ -run 'EndpointModeFor|DisabledEndpointRejects|ResponsesEndpointModeTable' -count=1`
Expected: compile failure (`endpointModeFor` undefined), then — for the disabled tests — a 200 (current fall-through to translate) instead of 404.

- [ ] **Step 3: Implement** (from `04-backend-decision.md` Tasks 04.1–04.2)

- Replace `nativePassthroughEnabled` with `endpointModeFor(target, apiFlavor) (string, routing.EndpointMode)` returning `("/v1/responses", target.ResponsesMode)` / `("/v1/messages", target.MessagesMode)` / `("", "")`.
- `upstreamPath:66` → `if p, mode := endpointModeFor(target, apiFlavor); mode == routing.EndpointModePassthrough { return p }`.
- Replace the decision block `:153-166` with the three-way `switch mode`: `passthrough` → `proxyNative` + return true; `disabled` → build the stable code+status via `endpointDisabledError(apiFlavor)`, `writeJSONCaptured(w, status, apierror.Response(code, msgEndpointDisabled, ""))`, `recordUsage(... "error" ... HTTPStatus:status ...)`, return true (never translate); `default` (translate or empty `""`) → log + return false.
- Add the consts `codeResponsesEndpointDisabled`/`codeMessagesEndpointDisabled`/`msgEndpointDisabled`/`statusEndpointDisabled = http.StatusNotFound` and `endpointDisabledError(apiFlavor)`.
- Refresh the two stale doc blocks (`:31-34`, `:55-61`) and the removed-field debug log.

- [ ] **Step 4: Run to verify pass — WHOLE MODULE now green**

Run:
```bash
cd gateway/backend && go test ./internal/gateway/ -count=1
make test-go   # from repo root: cd gateway/backend && go test ./...
```
Expected: PASS across the whole module (this is the task that closes the cross-package build coupling). If any stray test still constructs `routing.Application{NativeResponses:…}` or reads a removed field, sweep it now.

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/gateway/
git commit -m "feat(gateway): three-way endpoint decision; reject disabled Codex/Claude endpoints (404)"
```

---

## Task F1: `EndpointMode` type + shared `ApiVariantControls` + i18n option keys

**Files:**
- Modify: `gateway/frontend/src/api/models.ts` (add `EndpointMode` union after `:211`), `src/i18n.ts` (five new keys in `de` after `:376` and `en` after `:2471`).
- Create: `src/components/shared/ApiVariantControls.tsx` + `src/components/shared/ApiVariantControls.test.tsx`.

**Interfaces:**
- Produces: `EndpointMode` union; `ApiVariantControls` with props `{ t, apiFlavors, responsesMode, messagesMode, onFlavorsChange, onResponsesModeChange, onMessagesModeChange }`; i18n keys `applicationResponsesMode`, `applicationMessagesMode`, `applicationModeDisabled`, `applicationModeTranslate`, `applicationModePassthrough`.
- Consumes: `CheckboxGroup`, `SelectField`, `Translation`.

> **Name reconciliation (locked):** the shared component's flavor handler is `onFlavorsChange` (not `onApiFlavorsChange`); the option i18n keys are `applicationMode*` (not `applicationEndpointMode*`). Both forms and both frontend test bundles use these.

- [ ] **Step 1: Write the failing test** — `src/components/shared/ApiVariantControls.test.tsx`

Use the five `it(...)` cases verbatim from `05-frontend-app-form.md` Task 1a (Harness with `useState`; asserts both checkboxes checked → dropdowns enabled showing Durchreichen; unchecking `openai` disables the Codex dropdown + shows Deaktiviert while `anthropic` is unaffected; re-checking restores the stored `translate`; the three options render in order `Deaktiviert/Umwandlung/Durchreichen` and report the chosen one; toggling a checkbox calls `onFlavorsChange(['openai'])`).

- [ ] **Step 2: Run to verify it fails**

Run: `cd gateway/frontend && npm test -- src/components/shared/ApiVariantControls.test.tsx`
Expected: FAIL — `Cannot find module './ApiVariantControls'` / unknown i18n keys.

- [ ] **Step 3: Implement**

- `src/api/models.ts` after `:211`:
```ts
// Per-endpoint serving mode for the two coding-agent APIs (Codex /v1/responses,
// Claude Code /v1/messages): 'disabled' = not served, 'translate' = converted to
// /v1/chat/completions (lossy), 'passthrough' = proxied raw to the native path.
export type EndpointMode = 'disabled' | 'translate' | 'passthrough';
```
- `src/i18n.ts` — add to `de` after `applicationNativeNote` (`:376`) and the mirror in `en` (`:2471`):
```ts
// de
applicationResponsesMode: 'Codex (Responses-API)',
applicationMessagesMode: 'Claude Code (Anthropic Messages)',
applicationModeDisabled: 'Deaktiviert',
applicationModeTranslate: 'Umwandlung',
applicationModePassthrough: 'Durchreichen',
// en
applicationResponsesMode: 'Codex (Responses API)',
applicationMessagesMode: 'Claude Code (Anthropic Messages)',
applicationModeDisabled: 'Disabled',
applicationModeTranslate: 'Translate',
applicationModePassthrough: 'Pass-through',
```
- Create `src/components/shared/ApiVariantControls.tsx` verbatim from `05-frontend-app-form.md` Task 1b(iii) (checkboxes via `CheckboxGroup`; two `SelectField` dropdowns whose `value={openaiEnabled ? responsesMode : 'disabled'}` and `disabled={!openaiEnabled}`; the note via `t.applicationNativeNote`). Prop names exactly: `onFlavorsChange`, `onResponsesModeChange`, `onMessagesModeChange`.

- [ ] **Step 4: Run to verify it passes**

Run: `cd gateway/frontend && npm test -- src/components/shared/ApiVariantControls.test.tsx`
Expected: PASS (5/5). `npx tsc --noEmit` still green (additive so far).

- [ ] **Step 5: Commit**

```bash
git add gateway/frontend/src/api/models.ts gateway/frontend/src/i18n.ts gateway/frontend/src/components/shared/ApiVariantControls.tsx gateway/frontend/src/components/shared/ApiVariantControls.test.tsx
git commit -m "feat(portal-ui): shared ApiVariantControls (flavor checkboxes + endpoint-mode dropdowns)"
```

---

## Task F2: `applicationTypeDefaults` → passthrough for every type

**Files:**
- Modify: `src/components/shared/applicationTypeDefaults.ts` (interface `:14-23`, per-type table `:26-94`, import `:4`).
- Test: `src/components/shared/applicationTypeDefaults.test.ts`.

**Interfaces:**
- Produces: `TypeDefaults.responsesMode/messagesMode: EndpointMode` (replace `nativeResponses/nativeMessages`), all defaulting to `'passthrough'`.

- [ ] **Step 1: Write the failing test** — rewrite the three `toEqual` snapshots (ollama/llama_swap/server_agent) to `responsesMode: 'passthrough', messagesMode: 'passthrough'` and replace the `nativeResponses` migrate assertion with the three migrate cases from `05-frontend-app-form.md` Task 2a (customized field kept; already-passthrough mode is a no-op across a type switch; a customized mode is preserved).

- [ ] **Step 2: Run to verify it fails**

Run: `cd gateway/frontend && npm test -- src/components/shared/applicationTypeDefaults.test.ts`
Expected: FAIL (defaults still carry `nativeResponses`/`nativeMessages`).

- [ ] **Step 3: Implement** — import `EndpointMode`; replace the two interface fields with `responsesMode/messagesMode: EndpointMode`; in every one of the six table entries replace the `nativeResponses/nativeMessages` pair with `responsesMode: 'passthrough', messagesMode: 'passthrough'` (update the stale per-line comments); `migrateTypeFields` is unchanged (field-agnostic).

- [ ] **Step 4: Run to verify it passes**

Run: `cd gateway/frontend && npm test -- src/components/shared/applicationTypeDefaults.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gateway/frontend/src/components/shared/applicationTypeDefaults.ts gateway/frontend/src/components/shared/applicationTypeDefaults.test.ts
git commit -m "feat(portal-ui): default both endpoint modes to passthrough for every application type"
```

---

## Task F3: `models.ts` DTO swap + `ApplicationSection` wiring + fixture ripple

**Files:**
- Modify: `src/api/models.ts` (`PortalApplication:42-44`, `CreateApplicationRequest:86-87`, `UpdateApplicationRequest:122-123`), `src/components/ApplicationSection.tsx` (state `:227-228`, `openCreate:304-305`, `handleTypeChange:325-335`, `openEdit:362-363`, `buildBody:407-408`, JSX `:826-849`, delete `toggleFlavor:118-120`), and the fixture ripple in `src/App.test.tsx`, `src/components/{ApplicationSection,RuntimeAdminSection,ServerList,MappingSection,BenchmarkSection}.test.tsx`.
- Test: `src/components/ApplicationSection.test.tsx`.

**Interfaces:**
- Produces: `PortalApplication.responses_mode/messages_mode: EndpointMode` (required); the two optional on Create/Update requests. `ApplicationSection` renders `ApiVariantControls`.
- Consumes: `ApiVariantControls`, `EndpointMode` (F1).

- [ ] **Step 1: Write the failing test** — swap `makeApp:76-77` to `responses_mode: 'passthrough', messages_mode: 'passthrough'`; replace the `describe('ApplicationSection native passthrough toggles')` block (`:787-832`) with the `describe('ApplicationSection API-variant endpoint modes')` block from `05-frontend-app-form.md` Task 3a (default Durchreichen + `responses_mode:'passthrough'` on create; edit populates `translate`/`disabled` and saves a change; unchecking `openai` disables the Codex dropdown + shows Deaktiviert but still sends the stored mode and `api_flavors:['anthropic']`).

- [ ] **Step 2: Run to verify it fails**

Run: `cd gateway/frontend && npm test -- src/components/ApplicationSection.test.tsx`
Expected: FAIL (form still renders checkboxes / sends `native_*`).

- [ ] **Step 3: Implement**

- `models.ts`: replace the three DTO field pairs with `responses_mode`/`messages_mode` (`PortalApplication` required `EndpointMode`; Create/Update optional `EndpointMode`).
- `ApplicationSection.tsx`: import `ApiVariantControls` + `EndpointMode`; delete `toggleFlavor`; state `const [responsesMode,setResponsesMode]=useState<EndpointMode>('passthrough')` (+messages); `openCreate` → `setResponsesMode(d.responsesMode)`/`setMessagesMode(d.messagesMode)`; `handleTypeChange` passes/reads `responsesMode`/`messagesMode`; `openEdit` → `setResponsesMode(app.responses_mode)` (+messages); `buildBody` → `responses_mode: responsesMode`/`messages_mode: messagesMode` (`api_flavors: flavors` stays); replace the two `CheckboxGroup`s + caption (`:826-849`) with `<ApiVariantControls t={t} apiFlavors={flavors} responsesMode={responsesMode} messagesMode={messagesMode} onFlavorsChange={setFlavors} onResponsesModeChange={setResponsesMode} onMessagesModeChange={setMessagesMode} />`.
- Fixture ripple (tsc): in every listed test file replace `native_responses`/`native_messages` literals with `responses_mode`/`messages_mode: 'passthrough'`; in `App.test.tsx` the create-echo backend reads `body.responses_mode ?? 'passthrough'` (and messages). Grep `native_responses` / `native_messages` across `src/` to confirm none remain.

- [ ] **Step 4: Run to verify it passes + tsc green**

Run: `cd gateway/frontend && npm test -- src/components/ApplicationSection.test.tsx && npx tsc --noEmit`
Expected: PASS and tsc clean. (`RuntimeAdminSection.test.tsx` fixtures may still need F4's runtime fields — if tsc flags `RuntimeSpec` there, it is fixed in F4; the `PortalApplication` half is fixed here.)

- [ ] **Step 5: Commit**

```bash
git add gateway/frontend/src/api/models.ts gateway/frontend/src/components/ApplicationSection.tsx gateway/frontend/src/App.test.tsx gateway/frontend/src/components/*.test.tsx
git commit -m "feat(portal-ui): application form uses endpoint-mode dropdowns (drop native_* booleans)"
```

---

## Task F4: `runtime.ts` types + `RuntimeAdminSection` per-spec controls (snapshot pre-fill) + spec i18n test

**Files:**
- Modify: `src/api/runtime.ts` (`RuntimeSpec` gains `api_flavors`/`responses_mode`/`messages_mode`; import `EndpointMode`), `src/components/RuntimeAdminSection.tsx` (state after `:2042`, `openCreate:2146` snapshot, `hydrateSpecFields:2120`, `buildSpecBody:2359`, JSX after `:3344`, imports), and the `RuntimeAdminSection.test.tsx` fixtures (`makeSpec:134-156`, `application:73-107`, any `PutRuntimeSpecRequest` literal).
- Test: `src/components/RuntimeAdminSection.test.tsx`, `src/i18n.test.ts`.

**Interfaces:**
- Consumes: `EndpointMode`, `PortalApplication.responses_mode/messages_mode` (F3), `ApiVariantControls` (F1).
- Produces: `RuntimeSpec.api_flavors/responses_mode/messages_mode` (and, via `Omit`, on `PutRuntimeSpecRequest`).

- [ ] **Step 1: Write the failing tests** — add to `RuntimeAdminSection.test.tsx` the four cases from `06-frontend-spec-form-i18n.md` Tasks 06.2–06.4 (controls render in the create form; create pre-fills from the parent app — `api_flavors:['openai']` → openai checked/anthropic unchecked, Codex dropdown shows the app's `translate`; edit shows the spec's stored values; the PUT body carries `api_flavors`/`responses_mode`/`messages_mode`). Update the `makeSpec` factory + `application` fixture + any `PutRuntimeSpecRequest` literal to include the three new fields. Add the i18n key-presence test from Task 06.5 but referencing the reconciled keys `applicationModeDisabled/Translate/Passthrough` (+ `applicationResponsesMode`/`applicationMessagesMode`).

- [ ] **Step 2: Run to verify they fail**

Run: `cd gateway/frontend && npm test -- src/components/RuntimeAdminSection.test.tsx src/i18n.test.ts`
Expected: FAIL (labels not found; unknown fields).

- [ ] **Step 3: Implement**

- `runtime.ts`: `import type { EndpointMode } from './models';` and add to `interface RuntimeSpec` (after `gpus`) `api_flavors: string[]; responses_mode: EndpointMode; messages_mode: EndpointMode;` (do NOT edit `PutRuntimeSpecRequest` — the `Omit` pulls them in).
- `RuntimeAdminSection.tsx`: import `ApiVariantControls` + `EndpointMode`; add state `specApiFlavors`/`specResponsesMode`/`specMessagesMode` (defaults `[]`/`'passthrough'`/`'passthrough'`); in `openCreate` snapshot from the in-scope `application` prop (`setSpecApiFlavors([...application.api_flavors]); setSpecResponsesMode(application.responses_mode); setSpecMessagesMode(application.messages_mode);`); in `hydrateSpecFields` set them from the loaded `spec`; render `<ApiVariantControls t={t} apiFlavors={specApiFlavors} responsesMode={specResponsesMode} messagesMode={specMessagesMode} onFlavorsChange={setSpecApiFlavors} onResponsesModeChange={setSpecResponsesMode} onMessagesModeChange={setSpecMessagesMode} />` after the `runtimeSpecConfigSection` heading (`:3344`); add the three keys to the `buildSpecBody` literal (`:2359`). `specBodyWithAdminState:481` needs no edit (rest-spread carries them).

- [ ] **Step 4: Run to verify pass + whole suite + tsc**

Run:
```bash
cd gateway/frontend && npm test && npm run build
```
Expected: whole Vitest suite PASS and tsc/build clean (all fixture ripple resolved).

- [ ] **Step 5: Commit**

```bash
git add gateway/frontend/src/api/runtime.ts gateway/frontend/src/components/RuntimeAdminSection.tsx gateway/frontend/src/components/RuntimeAdminSection.test.tsx gateway/frontend/src/i18n.test.ts
git commit -m "feat(portal-ui): per-spec API-variant controls, snapshot-pre-filled from the application"
```

---

## Task F5: Remove orphaned i18n keys + reword the note

**Files:** Modify `src/i18n.ts`, and reword `applicationNativeNote`.

- [ ] **Step 1:** Grep `applicationNativeLegend|applicationNativeResponses|applicationNativeMessages` across `gateway/frontend/src/` (including tests). Confirm the only remaining references are the definitions (the F3 rewrite dropped the test usages).
- [ ] **Step 2:** Remove those three keys from BOTH `de` and `en`. Reword `applicationNativeNote` in both locales to describe the three modes (Deaktiviert / Umwandlung / Durchreichen) rather than a boolean toggle.
- [ ] **Step 3:** Run `cd gateway/frontend && npm test && npm run build`. Expected: PASS + tsc clean (a leftover reference is a compile error — fix it).
- [ ] **Step 4: Commit**

```bash
git add gateway/frontend/src/i18n.ts
git commit -m "chore(i18n): drop orphaned native-passthrough keys, reword the endpoint note"
```

---

## Task D1: Architecture docs + api-surface + ADR

**Files:** `docs/architecture/cross-cutting/compatibility-and-inference.md`, `agent-runtime-manager.md`, `docs/architecture/reference/api-surface.md`, the data-model reference doc, `docs/architecture/09-architecture-decisions.md`.

- [ ] **Step 1:** Rewrite the native-passthrough section of `compatibility-and-inference.md` as the three-state `EndpointMode` model (disabled/translate/passthrough), the effective-served rule, and the new `responses.endpoint_disabled`/`messages.endpoint_disabled` (404) codes.
- [ ] **Step 2:** In `agent-runtime-manager.md`, document per-spec `api_flavors`+modes, the snapshot-from-app decision, gateway-side enforcement, and an explicit "no agent change / no version bump" note (with the router-forwards-verbatim rationale).
- [ ] **Step 3:** Update `api-surface.md`: application DTO fields (`responses_mode`/`messages_mode` replacing `native_*`), runtime-spec DTO fields (`api_flavors`/`responses_mode`/`messages_mode`), and the five new error codes with their statuses; update the data-model reference for the new columns. Add a short ADR ("endpoint modes replace native_* booleans; independent disable; per-spec snapshot").
- [ ] **Step 4:** Run the docs gate: `make lint-docs` (from repo root). Expected: PASS (links/anchors resolve, every doc reachable from the README index).
- [ ] **Step 5: Commit**

```bash
git add docs/architecture/
git commit -m "docs(arch): three-state endpoint modes, per-spec snapshot, new error codes"
```

---

## Task V1: Full verification + quality gate

- [ ] **Step 1:** Backend: `make test-go` (repo root) and, if Docker/Postgres available, `OP_AI_GATEWAY_TEST_POSTGRES_DSN=… go test ./internal/store/... ./internal/routing/...`. Expected: PASS on all drivers.
- [ ] **Step 2:** Frontend: `cd gateway/frontend && npm test && npm run build && npm run lint`. Expected: PASS.
- [ ] **Step 3:** Go lint + docs: `make lint`. Expected: PASS.
- [ ] **Step 4:** Optional e2e sanity: run the relevant `gateway/e2e` suite if one covers application/runtime editing; note in the PR if skipped and why.
- [ ] **Step 5:** SonarQube local gate if the environment allows: `make sonar-up` (once) then `make sonar-gate`; `make sonar-branch-findings` to narrow to this branch. Act on new-code findings; if Docker/server unavailable, say so explicitly in the PR rather than staying silent.
- [ ] **Step 6:** Confirm no ServerAgent change: `git diff --name-only main...HEAD | grep '^server-agent/'` returns nothing, and `server-agent/internal/agent/agent.go const Version` is unchanged.

---

## Task Z1: Branch cleanup (final step before PR)

Per AGENTS.md: fold anything durable into `docs/architecture/` (done in D1), then remove the branch-local working files and open the PR (do not merge).

- [ ] **Step 1:** `git rm -r docs/superpowers` and remove `docs/implementation-status.md` if present on the branch.
- [ ] **Step 2:** Verify: `git diff --name-only main...HEAD` shows neither `docs/superpowers/**` nor `docs/implementation-status.md`.
- [ ] **Step 3:** Commit the removal, push the branch, and open a PR (squash-merge; human reviewer merges).

```bash
git rm -r docs/superpowers
git commit -m "chore: remove branch-local working files before PR"
git push -u origin api-variant-modes
```

---

## Sequencing summary

Backend must land in order **B1 → B2 → B3 → B4 → B5** (the module build is red between B2 and B5 by design; `make test-go` goes green at B5). Frontend **F1 → F2 → F3 → F4 → F5** (tsc goes green at F3 for `PortalApplication`, F4 for `RuntimeSpec`). Backend and frontend are independent and can proceed in parallel. **D1** any time after the code lands; **V1** after all code + docs; **Z1** last.
