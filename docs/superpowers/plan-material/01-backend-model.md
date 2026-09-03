# Plan material — Area 01: Backend domain model + routing/resolver + candidacy + EndpointMode enum

Spec: `docs/superpowers/specs/2026-09-03-api-variant-endpoint-modes-design.md`
Worktree root (all paths below are relative to it): `/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/api-variant-modes`
Go module: `op-ai-gateway`, rooted at `gateway/backend` (so `import "op-ai-gateway/internal/routing"`).
Run tests from `gateway/backend`: `go test ./internal/routing/... -count=1`.

> **CRITICAL CORRECTION to the spec (read first).** Spec §4 states *"`targetFrom`
> in `resolver.go` sets these when it builds the target (it already has the
> resolved mapping/spec in scope for `server_agent`)."* **This is false in the
> actual code.** `targetFrom` is a free function `func targetFrom(server, app,
> mapping, apiFlavor) Target` (resolver.go:890) with **no `ctx`, no `*Resolver`
> receiver, and no spec access**. The resolver **never reads `RuntimeSpec`** —
> `resolverStore` (resolver.go:245-254) has no runtime-spec method, and no code
> path in `resolver.go` calls `RuntimeSpecByMapping`. Making the `Target` carry
> the effective spec-or-app values therefore requires **extending `resolverStore`
> with `RuntimeSpecByMapping` and turning `targetFrom` into a `*Resolver` method
> that takes `ctx` and returns `(Target, error)`** (Task 4). This is the single
> biggest deviation between spec text and code; the plan must budget for it.

---

## 1. CURRENT STATE (exact excerpts with file:line)

### 1a. Flavor consts — `gateway/backend/internal/routing/store.go:46-47`
```go
	APIFlavorOpenAI    = "openai"
	APIFlavorAnthropic = "anthropic"
```

### 1b. `Application` struct — the two bools to replace — `store.go:496-591`
Relevant fields (the full struct spans 496-591). `APIFlavors` at line 502:
```go
	APIFlavors         []string
```
The bools to replace — `store.go:519-524`:
```go
	// NativeResponses / NativeMessages enable per-application native passthrough:
	// when set, a client request to /v1/responses (Codex) resp. /v1/messages
	// (Claude Code / Anthropic) is proxied raw to the upstream's same native path
	// instead of being translated through the internal inference representation.
	NativeResponses bool
	NativeMessages  bool
```
`Application.Type` (line 499, `string`) holds the provider const (e.g. `ProviderServerAgent = "server_agent"`, store.go:24) — the candidacy/targetFrom code branches on it.

### 1c. `RuntimeSpec` struct — needs 3 new fields — `store.go:1242-1280`
Current end of the struct (the insertion point is right after `SetVisibleDevices`, before `CreatedAt`):
```go
	SetVisibleDevices bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}
```
Note: RuntimeSpec today has **no slice/pointer fields** — a fact two memory-store
comments rely on for "the map value is already a safe copy" (see 1g). Adding
`APIFlavors []string` **breaks that invariant**.

### 1d. `applicationHasAPIFlavor` — the coarse gate — `store.go:1453-1461`
```go
// applicationHasAPIFlavor reports whether the application serves the flavor.
func applicationHasAPIFlavor(app Application, apiFlavor string) bool {
	for _, candidate := range app.APIFlavors {
		if candidate == apiFlavor {
			return true
		}
	}
	return false
}
```

### 1e. `Target` struct — the two bools to replace — `resolver.go:48-74`
```go
	// NativeResponses / NativeMessages mirror the resolved application's
	// native-passthrough flags, so the handler can decide whether to proxy the raw
	// client body to the upstream (Codex /v1/responses resp. Claude Code
	// /v1/messages) instead of translating it.
	NativeResponses bool
	NativeMessages  bool
```

### 1f. `resolverStore` interface (no spec method today) — `resolver.go:245-254`
```go
type resolverStore interface {
	ActiveMappingsForModel(ctx context.Context, gatewayModel string, apiFlavor string) ([]MappingCandidate, error)
	TelemetryByServer(ctx context.Context, serverID string) (ServerTelemetry, bool, error)
	Affinity(ctx context.Context, key AffinityKey) (RouteAffinity, bool, error)
	UpsertAffinity(ctx context.Context, affinity RouteAffinity) error
	DeleteAffinity(ctx context.Context, key AffinityKey) error
	ApplicationByID(ctx context.Context, id string) (Application, error)
	AIServerByID(ctx context.Context, id string) (AIServer, error)
	MappingsByApplication(ctx context.Context, applicationID string) ([]ModelMapping, error)
}
```
`*MemoryStore` and `*store.SQLStore` (via the tracing decorator) already implement
`RuntimeSpecByMapping` (store.go:1369; memory_store.go:2267; sqlite_runtime.go:74),
so adding it to this interface breaks no `NewResolver` caller. The only hand-rolled
test double, `groupReadCountingStore` (group_speed_order_test.go:28-42), **embeds
`resolverStore`** and overrides only two methods, so it inherits the new method for
free.

### 1g. `NormalizeAPIFlavor` — coarsening — `resolver.go:571-581`
```go
func NormalizeAPIFlavor(apiFlavor string) string {
	normalized := strings.ToLower(strings.TrimSpace(apiFlavor))
	switch {
	case strings.HasPrefix(normalized, "openai"):
		return APIFlavorOpenAI
	case strings.HasPrefix(normalized, "anthropic"):
		return APIFlavorAnthropic
	default:
		return normalized
	}
}
```

### 1h. `Resolve` — where the fine flavor is lost + candidacy/affinity call sites — `resolver.go`
- `resolver.go:349` — the fine→coarse coarsening (the request's fine flavor is
  **not** kept after this line):
  ```go
	apiFlavor := NormalizeAPIFlavor(req.APIFlavor)
  ```
  `req.APIFlavor` (inference.Request, `internal/inference/types.go:110`) carries the
  **fine** value; the coding-agent fine values are `"openai_responses"`,
  `"anthropic_messages"`, and `"openai_chat_completions"` (produced in
  `internal/gateway/inference_handlers.go:55,120,145,180,200` and
  `internal/compat/{openai,anthropic}.go`).
- `resolver.go:388` — affinity reuse entry (has `req.APIFlavor` in scope):
  ```go
		target, ok, err := r.resolveAffinity(ctx, key, now)
  ```
- `resolver.go:412` — coarse candidate read:
  ```go
	candidates, err := r.store.ActiveMappingsForModel(ctx, req.Model, apiFlavor)
  ```
- `resolver.go:416-421` — provisioning filter + the empty check (insertion point for
  the candidacy filter is between them):
  ```go
	candidates, err = r.filterProvisioned(ctx, token, candidates)
	if err != nil {
		return Target{}, err
	}
	if len(candidates) == 0 {
		return Target{}, ErrNoModelRoute
	}
  ```
- `resolver.go:473` — main-path target build:
  ```go
		target := targetFrom(selected.Server, selected.Application, selected.Mapping, apiFlavor)
  ```

### 1i. Affinity reuse flavor gate — `resolver.go:603` (inside `resolveAffinity`, 583-637)
```go
	if app.AffinityTTLSeconds <= 0 || app.ServerID != affinity.ServerID || app.Status != ServerStatusActive || !applicationHasAPIFlavor(app, key.APIFlavor) || (r.checker != nil && !r.checker.Reachable(app.ID)) {
```
`key.APIFlavor` here is **coarse** (`AffinityKey` is built at resolver.go:363 with the
normalized `apiFlavor`). The target build at the end of `resolveAffinity`:
```go
	return targetFrom(server, app, mapping, key.APIFlavor), true, nil   // resolver.go:636
```

### 1j. `resolveServerOverride` — candidate build — `resolver.go:505-539`
Builds `mine []MappingCandidate` from `ActiveMappingsForModel` (coarse) at 506-518, then
`return targetFrom(c.Server, c.Application, c.Mapping, apiFlavor), nil` at 538.

### 1k. `targetFrom` — the free function to method-ize — `resolver.go:890-906`
```go
func targetFrom(server AIServer, app Application, mapping ModelMapping, apiFlavor string) Target {
	return Target{
		RouteID:              mapping.ID,
		ServerID:             server.ID,
		Provider:             app.Type,
		Endpoint:             ApplicationEndpoint(server, app),
		Model:                mapping.GatewayModelName,
		ProviderModel:        mapping.AppModelName,
		Timeout:              time.Duration(app.TimeoutMS) * time.Millisecond,
		APIFlavor:            apiFlavor,
		APIToken:             app.APIToken,
		APITokenHeader:       app.APITokenHeader,
		NativeResponses:      app.NativeResponses,
		NativeMessages:       app.NativeMessages,
		OpportunisticMetrics: app.OpportunisticMetricsEnabled,
	}
}
```
**All four `targetFrom` call sites:** resolver.go:473 (main), :538 (server override),
:636 (affinity), :1313 (group `serve` closure, which has `ctx`, `r`, and `g.req` in
scope).

### 1l. Candidate flavor gate (memory driver) — `memory_store.go:1182-1211`, gate at :1197
```go
		app, ok := m.applications[mapping.ApplicationID]
		if !ok || app.Status != ServerStatusActive || !applicationHasAPIFlavor(app, apiFlavor) {
			continue
		}
```
`apiFlavor` here is the coarse value passed by the resolver. **The store gate stays
coarse and unchanged** — the endpoint-aware refinement is a resolver-side post-filter
(see Task 5 rationale). The SQLite/Postgres candidate query has the identical coarse
gate (`sqlite_applications.go:573-582`), so keeping the refinement in Go is what keeps
all three drivers uniform with zero SQL change.

### 1m. Memory-store safe-copy sites that break when RuntimeSpec gains a slice
- `memory_store.go:2133-2134` — the pattern to mirror:
  ```go
  func copyApplication(app Application) Application {
  	app.APIFlavors = append([]string(nil), app.APIFlavors...)
  ```
- `memory_store.go:2261` (Upsert insert) and `:2254` (Upsert update-in-place) store the
  spec value **by value** — with a new slice field these must deep-copy.
- `memory_store.go:2267-2276` `RuntimeSpecByMapping`, `:2281-2286` `RuntimeSpecByID`,
  `:2290-2301` `RuntimeSpecsByApplication` — each returns the stored value directly and
  each carries the now-stale comment *"RuntimeSpec has no slice/pointer fields, so the
  map value is already a safe copy."*

### 1n. Direct consumers of the renamed fields (compile-break surface, other areas)
`Application.NativeResponses/NativeMessages`:
- `internal/store/sqlite_applications.go:48-49,113-114,557-558,646-647` (CRUD read/write)
- `internal/portal/service_applications.go:392-393,598-602,763-764` (DTOs)
- `internal/store/conformance_test.go`, `internal/store/application_column_parity_test.go`

`Target.NativeResponses/NativeMessages`:
- `internal/gateway/native_passthrough.go:35-43` (`nativePassthroughEnabled`) and `:162`
  (a log line). Current consumer:
  ```go
  func nativePassthroughEnabled(target routing.Target, apiFlavor string) (string, bool) {
  	switch apiFlavor {
  	case "openai_responses":
  		return "/v1/responses", target.NativeResponses
  	case "anthropic_messages":
  		return "/v1/messages", target.NativeMessages
  	}
  	return "", false
  }
  ```
- gateway tests: `internal/gateway/native_passthrough_test.go`,
  `principal_limits_wiring_test.go`, `server_test.go`, `service_token_scope_test.go`,
  and `cmd/gateway/native_passthrough_test.go`.

> These other-area edits are **out of scope for this area's tasks** but the plan must
> sequence them: removing the `Application`/`Target` bool fields will not compile the
> `store`, `portal`, and `gateway` packages until those are updated. The **`routing`
> package compiles and tests independently**, so this area's TDD can go green on its own,
> but the full-tree `go build ./...` / `go test ./...` gates only pass once the store,
> portal, and gateway areas land the mode migration too.

---

## 2. PROPOSED TDD TASKS (ordered, bite-sized, with real code)

### Task 1 — `EndpointMode` type + parsing/validation (new file)

**New file:** `gateway/backend/internal/routing/endpoint_mode.go`

**Red — write the test first:** `gateway/backend/internal/routing/endpoint_mode_test.go`
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
		{"  Passthrough ", EndpointModePassthrough, false}, // trimmed + lowercased
		{"", DefaultEndpointMode, false},                   // absent -> default
		{"native", "", true},                               // unknown rejected
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
	if got := EndpointModeDisabled.OrDefault(); got != EndpointModeDisabled {
		t.Errorf("disabled.OrDefault() = %q, want disabled", got)
	}
}
```
Run: `cd gateway/backend && go test ./internal/routing/ -run 'EndpointMode' -count=1`
Expected **before** impl: build failure `undefined: EndpointMode` (red).

**Green — minimal implementation:** `endpoint_mode.go`
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
// to the upstream's native path. Serialized as its lowercase string in JSON and
// stored as text.
type EndpointMode string

const (
	EndpointModeDisabled    EndpointMode = "disabled"
	EndpointModeTranslate   EndpointMode = "translate"
	EndpointModePassthrough EndpointMode = "passthrough"

	// DefaultEndpointMode is the value a fresh application/spec gets when the
	// flavor is enabled: pass-through, because every supported upstream now serves
	// both native endpoints (design §6).
	DefaultEndpointMode = EndpointModePassthrough
)

// String returns the mode's stored/wire string.
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

// OrDefault resolves an unset ("") mode to DefaultEndpointMode, leaving every
// other value untouched — the read-time default an in-memory zero Application
// still needs even though the migration backfills a non-empty value at rest.
func (m EndpointMode) OrDefault() EndpointMode {
	if m == "" {
		return DefaultEndpointMode
	}
	return m
}

// ParseEndpointMode normalizes and validates an incoming mode string (DTO or
// column): it trims and lowercases, maps "" to DefaultEndpointMode, and rejects
// any unrecognized value (a stable validation failure at the DTO edge).
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
Expected **after**: PASS. (Portal DTO validation — a separate area — should call
`ParseEndpointMode` and surface a stable validation error on the `true` case; whether
`""` defaults or errors at the DTO edge is that area's call, this helper supports both.)

---

### Task 2 — `RuntimeSpec` gains `APIFlavors` + `ResponsesMode` + `MessagesMode`; fix memory copy

**Red — test:** append to `gateway/backend/internal/routing/memory_store_test.go` (uses the
existing `must` helper at memory_store_test.go:189):
```go
func TestMemoryStoreRuntimeSpecIsolatesAPIFlavors(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	m := NewMemoryStore()
	must(t, m.CreateAIServer(ctx, AIServer{ID: "srv_1", Domain: "h", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	must(t, m.CreateApplication(ctx, Application{ID: "app_1", ServerID: "srv_1", Type: ProviderServerAgent, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, m.CreateMapping(ctx, ModelMapping{ID: "map_1", ApplicationID: "app_1", GatewayModelName: "m", AppModelName: "m", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))

	spec := RuntimeSpec{ID: "spec_1", MappingID: "map_1", APIFlavors: []string{APIFlavorOpenAI, APIFlavorAnthropic}, ResponsesMode: EndpointModePassthrough, MessagesMode: EndpointModeTranslate, CreatedAt: now, UpdatedAt: now}
	must(t, m.UpsertRuntimeSpec(ctx, spec))

	spec.APIFlavors[0] = "mutated" // caller mutates its own slice AFTER the write
	got, ok, err := m.RuntimeSpecByMapping(ctx, "map_1")
	must(t, err)
	if !ok {
		t.Fatal("spec not found")
	}
	if got.APIFlavors[0] != APIFlavorOpenAI {
		t.Fatalf("UpsertRuntimeSpec leaked the caller slice: %#v", got.APIFlavors)
	}
	if got.ResponsesMode != EndpointModePassthrough || got.MessagesMode != EndpointModeTranslate {
		t.Fatalf("modes not round-tripped: %q / %q", got.ResponsesMode, got.MessagesMode)
	}
	got.APIFlavors[1] = "mutated2" // caller mutates the RETURNED slice
	again, _, err := m.RuntimeSpecByMapping(ctx, "map_1")
	must(t, err)
	if again.APIFlavors[1] != APIFlavorAnthropic {
		t.Fatalf("RuntimeSpecByMapping leaked a mutable slice: %#v", again.APIFlavors)
	}
}
```
Run: `cd gateway/backend && go test ./internal/routing/ -run TestMemoryStoreRuntimeSpecIsolatesAPIFlavors -count=1`
Expected **before**: build failure `unknown field 'APIFlavors' in struct literal of type RuntimeSpec` (red).

**Green — implementation:**

1. `store.go` — extend the `RuntimeSpec` struct (insert after `SetVisibleDevices bool`,
   store.go:1277):
   ```go
	SetVisibleDevices bool
	// APIFlavors / ResponsesMode / MessagesMode are the per-spec snapshot of the
	// API-variant capability + the two coding-agent endpoint modes (design
	// 2026-09-03). For a server_agent mapping the RESOLVED spec is the sole
	// authority for its model's flavors + modes; the parent application's values
	// are only the create-time template and the no-spec fallback. Gateway-side
	// only — never added to AgentRuntimeSpecDTO or the agent wire type.
	APIFlavors    []string
	ResponsesMode EndpointMode
	MessagesMode  EndpointMode
	CreatedAt     time.Time
	UpdatedAt     time.Time
   ```

2. `memory_store.go` — add a deep-copy helper next to `copyApplication` (memory_store.go:2133):
   ```go
   // copyRuntimeSpec returns a spec whose APIFlavors slice is independent of the
   // argument's, so a stored/returned spec never aliases the caller's slice.
   func copyRuntimeSpec(spec RuntimeSpec) RuntimeSpec {
   	spec.APIFlavors = append([]string(nil), spec.APIFlavors...)
   	return spec
   }
   ```
   Then apply it on write in `UpsertRuntimeSpec` (memory_store.go:2254 and :2261) and on
   read in the three readers (`:2261`→store `copyRuntimeSpec(spec)`, and each of
   `RuntimeSpecByMapping`/`RuntimeSpecByID`/`RuntimeSpecsByApplication` returns
   `copyRuntimeSpec(...)`), and update the two now-stale "no slice/pointer fields, safe
   copy" comments to say the slice is deep-copied.

Expected **after**: PASS.

> The **store persistence** of these three columns (SQLite/Postgres `agent_runtime_specs`
> read/write in `sqlite_runtime.go` — `UpsertRuntimeSpec` col list at :20-45,
> `runtimeSpecCols` at :60-63, `runtimeSpecColsPrefixed` at :69-72, `scanRuntimeSpec`) and
> the migration/backfill are the **store area's** tasks. Follow the existing
> `encodeAPIFlavors`/`decodeAPIFlavors` JSON-in-text pattern
> (`sqlite_applications.go:731-737`) for `api_flavors`, and store the two modes as plain
> `text`. This area only defines the Go fields + memory driver.

---

### Task 3 — Replace the two `Application`/`Target` bools with `ResponsesMode`/`MessagesMode`; `targetFrom` carries app-level values

This is the breaking rename. Keep `targetFrom` a free function **in this task** (app-level
values only); Task 4 adds the spec lookup. Doing so keeps `routing` compiling in one step.

**Red — test:** new file `gateway/backend/internal/routing/resolver_endpoint_mode_test.go`
```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"testing"
	"time"
)

func TestTargetCarriesAppEndpointModes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	// Give both seeded apps both flavors + explicit modes.
	for _, id := range []string{"app_fast", "app_slow"} {
		app, err := store.ApplicationByID(ctx, id)
		must(t, err)
		app.APIFlavors = []string{APIFlavorOpenAI, APIFlavorAnthropic}
		app.ResponsesMode = EndpointModeTranslate
		app.MessagesMode = EndpointModePassthrough
		must(t, store.UpdateApplication(ctx, app))
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok", UserID: "u", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat_completions"})
	must(t, err)
	if target.ResponsesMode != EndpointModeTranslate {
		t.Fatalf("target.ResponsesMode = %q, want translate", target.ResponsesMode)
	}
	if target.MessagesMode != EndpointModePassthrough {
		t.Fatalf("target.MessagesMode = %q, want passthrough", target.MessagesMode)
	}
	if len(target.APIFlavors) != 2 {
		t.Fatalf("target.APIFlavors = %v, want the app's two flavors", target.APIFlavors)
	}
}
```
Run: `cd gateway/backend && go test ./internal/routing/ -run TestTargetCarriesAppEndpointModes -count=1`
Expected **before**: build failure `unknown field 'ResponsesMode' in ... Target` (red).

**Green — implementation:**

1. `store.go:519-524` — replace the `NativeResponses`/`NativeMessages` block with:
   ```go
	// ResponsesMode / MessagesMode are the per-application three-state endpoint
	// controls (design 2026-09-03) that replaced the native_responses /
	// native_messages booleans: for the Codex /v1/responses and Claude Code
	// /v1/messages endpoints respectively — EndpointModeDisabled (not served),
	// EndpointModeTranslate (translate to /v1/chat/completions, the old
	// native_*=false) or EndpointModePassthrough (proxy raw to the upstream's
	// native path, the old native_*=true). Whether the endpoint is served at all
	// also depends on the matching APIFlavors entry (see applicationServesEndpoint).
	ResponsesMode EndpointMode
	MessagesMode  EndpointMode
   ```

2. `resolver.go:64-69` — replace the `Target` bool block with:
   ```go
	// APIFlavors / ResponsesMode / MessagesMode are the EFFECTIVE API-variant
	// capability + endpoint modes for this request: the resolved application's
	// values for an ordinary app, or the resolved RuntimeSpec's values for a
	// server_agent mapping that has a spec (the app's values when it has none —
	// see targetFrom). The dispatch layer (native_passthrough) reads them to pick
	// disabled / translate / pass-through and to enforce responsesServed /
	// messagesServed.
	APIFlavors    []string
	ResponsesMode EndpointMode
	MessagesMode  EndpointMode
   ```

3. `resolver.go:890-906` — update `targetFrom` (still a free function this task):
   ```go
   		APIFlavor:            apiFlavor,
   		APIToken:             app.APIToken,
   		APITokenHeader:       app.APITokenHeader,
   		APIFlavors:           append([]string(nil), app.APIFlavors...),
   		ResponsesMode:        app.ResponsesMode,
   		MessagesMode:         app.MessagesMode,
   		OpportunisticMetrics: app.OpportunisticMetricsEnabled,
   ```
   (drop the two `NativeResponses`/`NativeMessages` lines.)

Expected **after**: PASS **within the `routing` package**. The `store`/`portal`/`gateway`
packages will not build until their bool references (§1n) are migrated by their areas.

---

### Task 4 — `targetFrom` resolves the spec for `server_agent` (effective spec-or-app values)

**Red — test:** append to `resolver_endpoint_mode_test.go`
```go
func TestTargetCarriesSpecModesForServerAgent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	must(t, store.CreateAIServer(ctx, AIServer{ID: "srv", Domain: "srv.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	// App level says translate/translate — the spec must WIN over this.
	must(t, store.CreateApplication(ctx, Application{ID: "app", ServerID: "srv", Type: ProviderServerAgent, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI, APIFlavorAnthropic}, ResponsesMode: EndpointModeTranslate, MessagesMode: EndpointModeTranslate, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.CreateMapping(ctx, ModelMapping{ID: "map", ApplicationID: "app", GatewayModelName: "m", AppModelName: "m", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv", ReportedAt: now, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}))
	must(t, store.UpsertRuntimeSpec(ctx, RuntimeSpec{ID: "spec", MappingID: "map", APIFlavors: []string{APIFlavorOpenAI}, ResponsesMode: EndpointModePassthrough, MessagesMode: EndpointModeDisabled, CreatedAt: now, UpdatedAt: now}))

	resolver := NewResolver(store, func() time.Time { return now }, nil)
	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok", UserID: "u", Active: true}, inference.Request{Model: "m", APIFlavor: "openai_responses"})
	must(t, err)
	if target.ResponsesMode != EndpointModePassthrough {
		t.Fatalf("target.ResponsesMode = %q, want passthrough (spec wins over app's translate)", target.ResponsesMode)
	}
	if target.MessagesMode != EndpointModeDisabled {
		t.Fatalf("target.MessagesMode = %q, want disabled (spec value)", target.MessagesMode)
	}
	if len(target.APIFlavors) != 1 || target.APIFlavors[0] != APIFlavorOpenAI {
		t.Fatalf("target.APIFlavors = %v, want [openai] (spec value)", target.APIFlavors)
	}
}

func TestTargetFallsBackToAppModesWhenServerAgentHasNoSpec(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	must(t, store.CreateAIServer(ctx, AIServer{ID: "srv", Domain: "srv.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	must(t, store.CreateApplication(ctx, Application{ID: "app", ServerID: "srv", Type: ProviderServerAgent, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, ResponsesMode: EndpointModeTranslate, MessagesMode: EndpointModePassthrough, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.CreateMapping(ctx, ModelMapping{ID: "map", ApplicationID: "app", GatewayModelName: "m", AppModelName: "m", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv", ReportedAt: now, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}))
	// No UpsertRuntimeSpec -> app values are the fallback.

	resolver := NewResolver(store, func() time.Time { return now }, nil)
	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok", UserID: "u", Active: true}, inference.Request{Model: "m", APIFlavor: "openai_responses"})
	must(t, err)
	if target.ResponsesMode != EndpointModeTranslate {
		t.Fatalf("target.ResponsesMode = %q, want translate (app fallback)", target.ResponsesMode)
	}
}
```
Run: `cd gateway/backend && go test ./internal/routing/ -run 'TestTarget.*ServerAgent|FallsBack' -count=1`
Expected **before**: FAIL — `target.ResponsesMode = "translate", want passthrough` (app value
leaks; the free `targetFrom` never consults the spec).

**Green — implementation:**

1. `resolver.go:245-254` — add to `resolverStore`:
   ```go
	RuntimeSpecByMapping(ctx context.Context, mappingID string) (RuntimeSpec, bool, error)
   ```

2. `resolver.go:890-906` — replace the free `targetFrom` with a `*Resolver` method:
   ```go
   func (r *Resolver) targetFrom(ctx context.Context, server AIServer, app Application, mapping ModelMapping, apiFlavor string) (Target, error) {
   	flavors, responsesMode, messagesMode := app.APIFlavors, app.ResponsesMode, app.MessagesMode
   	if app.Type == ProviderServerAgent {
   		// For a server_agent mapping the RESOLVED spec is the sole authority for
   		// its model's flavors + modes; the app's values are only the fallback for
   		// a mapping that has no spec at all (design §3.3/§4).
   		spec, ok, err := r.store.RuntimeSpecByMapping(ctx, mapping.ID)
   		if err != nil {
   			return Target{}, fmt.Errorf("load runtime spec: %w", err)
   		}
   		if ok {
   			flavors, responsesMode, messagesMode = spec.APIFlavors, spec.ResponsesMode, spec.MessagesMode
   		}
   	}
   	return Target{
   		RouteID:              mapping.ID,
   		ServerID:             server.ID,
   		Provider:             app.Type,
   		Endpoint:             ApplicationEndpoint(server, app),
   		Model:                mapping.GatewayModelName,
   		ProviderModel:        mapping.AppModelName,
   		Timeout:              time.Duration(app.TimeoutMS) * time.Millisecond,
   		APIFlavor:            apiFlavor,
   		APIToken:             app.APIToken,
   		APITokenHeader:       app.APITokenHeader,
   		APIFlavors:           append([]string(nil), flavors...),
   		ResponsesMode:        responsesMode,
   		MessagesMode:         messagesMode,
   		OpportunisticMetrics: app.OpportunisticMetricsEnabled,
   	}, nil
   }
   ```
   (`fmt` is already imported in resolver.go.)

3. Update the four call sites:
   - `resolver.go:473`:
     ```go
     		target, err := r.targetFrom(ctx, selected.Server, selected.Application, selected.Mapping, apiFlavor)
     		if err != nil {
     			return Target{}, err
     		}
     ```
     (`err` already exists in the loop scope from `selectCandidate`; `target` is new, so `:=` is valid.)
   - `resolver.go:538` (resolveServerOverride): `return r.targetFrom(ctx, c.Server, c.Application, c.Mapping, apiFlavor)`
   - `resolver.go:636` (resolveAffinity):
     ```go
     	target, err := r.targetFrom(ctx, server, app, mapping, key.APIFlavor)
     	if err != nil {
     		return Target{}, false, err
     	}
     	return target, true, nil
     ```
   - `resolver.go:1313` (group `serve` closure): `return r.targetFrom(ctx, sel.Server, sel.Application, sel.Mapping, g.apiFlavor)`

Expected **after**: PASS. (Existing tests still pass — ordinary apps go through the
`app.Type != server_agent` branch, identical values.)

---

### Task 5 — Endpoint-aware candidacy + affinity reuse (fine-flavor refinement)

Rationale note for the plan: the store's candidate gate and the `AffinityKey` are both
**coarse** (`openai`/`anthropic`) because the resolver coarsens `req.APIFlavor` at
resolver.go:349 before it reaches the store. So the endpoint-aware refinement is a
**resolver-side post-filter** keyed on the fine `req.APIFlavor` — this keeps all three
store drivers uniform with **zero SQL change** (mirrors how `filterProvisioned` post-filters
`ActiveMappingsForModel`). `server_agent` apps are gated on the coarse flavor **only** at
candidacy: their authoritative per-model mode lives on the resolved spec and is enforced at
dispatch (native_passthrough), per spec §4 point 2 ("a per-model disabled is only knowable
after model resolution").

**Red — test:** append to `resolver_endpoint_mode_test.go`
```go
func TestCandidacyExcludesResponsesDisabledOrdinaryApp(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now) // both apps: Type=mock, APIFlavors=[openai]
	for _, id := range []string{"app_fast", "app_slow"} {
		app, err := store.ApplicationByID(ctx, id)
		must(t, err)
		app.ResponsesMode = EndpointModeDisabled
		must(t, store.UpdateApplication(ctx, app))
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)

	// Codex (openai_responses) -> no route (both ordinary apps disabled).
	if _, err := resolver.Resolve(ctx, auth.Token{ID: "t1", UserID: "u", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_responses"}); err != ErrNoModelRoute {
		t.Fatalf("openai_responses: err = %v, want ErrNoModelRoute", err)
	}
	// Plain chat-completions is UNAFFECTED by ResponsesMode (coarse openai gate).
	if _, err := resolver.Resolve(ctx, auth.Token{ID: "t2", UserID: "u", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat_completions"}); err != nil {
		t.Fatalf("openai_chat_completions should still route: %v", err)
	}
}

func TestCandidacyDoesNotModeGateServerAgent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	must(t, store.CreateAIServer(ctx, AIServer{ID: "srv", Domain: "srv.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	// App-level ResponsesMode=disabled, but a server_agent app must STILL be a
	// candidate — the spec is the authority, enforced later at dispatch.
	must(t, store.CreateApplication(ctx, Application{ID: "app", ServerID: "srv", Type: ProviderServerAgent, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, ResponsesMode: EndpointModeDisabled, MessagesMode: EndpointModeDisabled, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.CreateMapping(ctx, ModelMapping{ID: "map", ApplicationID: "app", GatewayModelName: "m", AppModelName: "m", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv", ReportedAt: now, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}))
	must(t, store.UpsertRuntimeSpec(ctx, RuntimeSpec{ID: "spec", MappingID: "map", APIFlavors: []string{APIFlavorOpenAI}, ResponsesMode: EndpointModePassthrough, MessagesMode: EndpointModeDisabled, CreatedAt: now, UpdatedAt: now}))

	resolver := NewResolver(store, func() time.Time { return now }, nil)
	target, err := resolver.Resolve(ctx, auth.Token{ID: "t", UserID: "u", Active: true}, inference.Request{Model: "m", APIFlavor: "openai_responses"})
	must(t, err) // NOT ErrNoModelRoute: server_agent survives candidacy
	if target.ResponsesMode != EndpointModePassthrough {
		t.Fatalf("target.ResponsesMode = %q, want passthrough (spec)", target.ResponsesMode)
	}
}
```
Run: `cd gateway/backend && go test ./internal/routing/ -run 'Candidacy' -count=1`
Expected **before**: `TestCandidacyExcludesResponsesDisabledOrdinaryApp` FAILS — the
`openai_responses` request still routes (no candidacy refinement yet), so `err` is nil, not
`ErrNoModelRoute`.

**Green — implementation:**

1. `store.go` — add next to `applicationHasAPIFlavor` (after store.go:1461):
   ```go
   // applicationServesEndpoint refines applicationHasAPIFlavor with the per-endpoint
   // mode for the two coding-agent endpoints. For an openai_responses request an
   // ORDINARY app must carry the openai flavor AND not have ResponsesMode==disabled;
   // anthropic_messages is the anthropic/MessagesMode analogue. A server_agent app is
   // gated on the coarse flavor only here — its authoritative per-model mode lives on
   // the resolved RuntimeSpec and is enforced at dispatch (native_passthrough), so
   // candidacy must not exclude it on the app-level fallback. Any other flavor value
   // (openai_chat_completions, or an already-coarse openai/anthropic) uses the plain
   // flavor gate, so plain chat-completions is never removed by a disabled Codex
   // endpoint.
   func applicationServesEndpoint(app Application, fineFlavor string) bool {
   	switch fineFlavor {
   	case "openai_responses":
   		if !applicationHasAPIFlavor(app, APIFlavorOpenAI) {
   			return false
   		}
   		return app.Type == ProviderServerAgent || app.ResponsesMode != EndpointModeDisabled
   	case "anthropic_messages":
   		if !applicationHasAPIFlavor(app, APIFlavorAnthropic) {
   			return false
   		}
   		return app.Type == ProviderServerAgent || app.MessagesMode != EndpointModeDisabled
   	default:
   		return applicationHasAPIFlavor(app, NormalizeAPIFlavor(fineFlavor))
   	}
   }
   ```

2. `resolver.go` — add a slice post-filter (e.g. next to `filterProvisioned`):
   ```go
   // filterServesEndpoint drops candidates whose application does not serve the
   // request's FINE api flavor once the per-endpoint mode is applied. It refines only
   // the two coding-agent endpoints (openai_responses / anthropic_messages); for
   // openai_chat_completions and any coarse flavor it is a no-op, so the coarse
   // openai/anthropic gate the store already applied stands unchanged.
   func filterServesEndpoint(cands []MappingCandidate, fineFlavor string) []MappingCandidate {
   	if fineFlavor != "openai_responses" && fineFlavor != "anthropic_messages" {
   		return cands
   	}
   	out := cands[:0:0]
   	for _, c := range cands {
   		if applicationServesEndpoint(c.Application, fineFlavor) {
   			out = append(out, c)
   		}
   	}
   	return out
   }
   ```

3. `resolver.go:416-421` — insert the filter after `filterProvisioned`, before the empty check:
   ```go
   	candidates, err = r.filterProvisioned(ctx, token, candidates)
   	if err != nil {
   		return Target{}, err
   	}
   	candidates = filterServesEndpoint(candidates, req.APIFlavor)
   	if len(candidates) == 0 {
   		return Target{}, ErrNoModelRoute
   	}
   ```

4. `resolver.go` — endpoint-aware affinity reuse. Thread the fine flavor into
   `resolveAffinity`:
   - call site resolver.go:388: `target, ok, err := r.resolveAffinity(ctx, key, req.APIFlavor, now)`
   - signature (resolver.go:583): `func (r *Resolver) resolveAffinity(ctx context.Context, key AffinityKey, fineFlavor string, now time.Time) (Target, bool, error)`
   - the gate at resolver.go:603 — replace `!applicationHasAPIFlavor(app, key.APIFlavor)`
     with `!applicationServesEndpoint(app, fineFlavor)`.
   (Backward-compatible: for chat/coarse flavors `applicationServesEndpoint` hits the
   `default` branch = the same coarse gate, so
   `TestResolverBreaksAffinityWhenApplicationDropsFlavor` (resolver_test.go:709) still
   passes.)

5. **Recommended for consistency** (spec lists "resolver.go affinity reuse" and candidacy;
   the override path is the same class): `resolveServerOverride` — after building `mine`
   (resolver.go:516), insert `mine = filterServesEndpoint(mine, req.APIFlavor)` before the
   `len(mine) == 0 { return ErrServerOverrideModelUnavailable }` check.

Expected **after**: PASS.

> **Group path (flag, plan-author decision):** the model-group member resolution
> (`eligibleCandidates`, resolver.go:955) uses the coarse `apiFlavor` and does **not**
> currently see the fine value. To make a Codex request that targets a *group* equally
> endpoint-aware at candidacy, thread `g.req.APIFlavor` into `eligibleCandidates` and apply
> `filterServesEndpoint` there too. The spec's §10 candidacy tests do not explicitly cover
> groups, and dispatch-time enforcement (native_passthrough) still catches a group route to
> a disabled endpoint, so this is a consistency improvement rather than a correctness gap —
> call it out but it can be a small follow-on task.

---

## 3. INTERFACES

### This area PRODUCES (consumed by store, portal, gateway, frontend-mirroring areas)

- **`type EndpointMode string`** with consts `EndpointModeDisabled = "disabled"`,
  `EndpointModeTranslate = "translate"`, `EndpointModePassthrough = "passthrough"`;
  `DefaultEndpointMode = EndpointModePassthrough`. Methods `String() string`,
  `Valid() bool`, `OrDefault() EndpointMode`; func `ParseEndpointMode(string) (EndpointMode, error)`.
  File: `internal/routing/endpoint_mode.go`. **Matches the spec's canonical names verbatim.**
- **`Application.ResponsesMode EndpointMode`** and **`Application.MessagesMode EndpointMode`**
  (replace `NativeResponses`/`NativeMessages bool`). `Application.APIFlavors []string` unchanged.
- **`RuntimeSpec.APIFlavors []string`**, **`RuntimeSpec.ResponsesMode EndpointMode`**,
  **`RuntimeSpec.MessagesMode EndpointMode`** (new).
- **`Target.APIFlavors []string`**, **`Target.ResponsesMode EndpointMode`**,
  **`Target.MessagesMode EndpointMode`** (replace `Target.NativeResponses`/`NativeMessages`).
  These carry the **effective** (spec-or-app) values. Consumed by
  `internal/gateway/native_passthrough.go` (rewire `nativePassthroughEnabled` to read the
  modes: `passthrough`→proxy, `translate`→translate, `disabled`→reject).
- **`applicationServesEndpoint(app Application, fineFlavor string) bool`** — the
  endpoint-aware effective-served predicate (unexported; other routing-internal callers only).
- **`resolverStore` gains `RuntimeSpecByMapping(ctx, mappingID) (RuntimeSpec, bool, error)`**.
- The `targetFrom` signature becomes `func (r *Resolver) targetFrom(ctx, server, app, mapping, apiFlavor) (Target, error)` (internal to routing).

### Column / JSON names this area's fields map to (owned by the store + portal areas)

- `applications`: `responses_mode`, `messages_mode` (text). `api_flavors` unchanged.
- `agent_runtime_specs`: `api_flavors`, `responses_mode`, `messages_mode` (text;
  `api_flavors` via the existing `encodeAPIFlavors`/`decodeAPIFlavors` JSON pattern).
- DTO JSON keys: `responses_mode`, `messages_mode` (applications + runtime specs),
  `api_flavors` (runtime specs). Values are the three `EndpointMode` strings.

### This area CONSUMES from other areas

- **New stable error codes** (owned by the gateway dispatch area; used at
  native_passthrough's `disabled` rejection, **not** in routing): `responses.endpoint_disabled`,
  `messages.endpoint_disabled`. Routing's `disabled` handling is limited to candidacy
  exclusion (ordinary apps → `ErrNoModelRoute`); routing does not emit these codes.
- **Store/migration area** must persist the new columns and backfill
  (`native_responses=1 → passthrough`, `0 → translate`; app→spec snapshot). Column-parity
  and Postgres tests extend to the new columns.
- **Frontend** mirrors the enum string union `'disabled' | 'translate' | 'passthrough'` and
  the shared `ApiVariantControls` component (no dependency the other way).

### Fine-flavor facts the plan must hold constant

- Fine values reaching `req.APIFlavor`: `"openai_responses"`, `"anthropic_messages"`,
  `"openai_chat_completions"` (also legacy short `"openai_chat"` in older tests).
- `NormalizeAPIFlavor` coarsens by prefix: `openai*` → `"openai"`, `anthropic*` →
  `"anthropic"`; anything else passes through lowercased/trimmed. The store candidate gate,
  the `AffinityKey.APIFlavor`, and `Target.APIFlavor` are all the **coarse** value.

---

## 4. GOTCHAS

1. **Spec §4's "targetFrom already has the spec in scope" is wrong** (see the banner at the
   top). The resolver has no spec access today; Task 4 adds it (interface method +
   ctx/error signature + 4 call-site updates). This is the load-bearing correction.
2. **RuntimeSpec's new `[]string` breaks the memory-store safe-copy invariant.** Two
   comments (memory_store.go:2265-2266, 2278-2280) explicitly claim "no slice/pointer
   fields." Add `copyRuntimeSpec` and apply it on write (`UpsertRuntimeSpec`, both branches)
   and on read (three readers), and fix those comments. The Task 2 test pins this.
3. **The candidacy refinement must stay in Go (resolver post-filter), not in the store
   SQL.** The store gate only ever sees the coarse flavor; a Go post-filter keeps
   memory/sqlite/postgres uniform and needs no migration to the candidate query. Do **not**
   change `ActiveMappingsForModel`'s signature.
4. **`server_agent` is gated on coarse flavor only at candidacy.** Mode-gating it on the
   app-level fallback would wrongly exclude a model whose spec re-enables the endpoint;
   dispatch (native_passthrough, other area) is the authority for the per-spec `disabled`.
   `TestCandidacyDoesNotModeGateServerAgent` pins this.
5. **Cross-package compile break.** Removing `Application`/`Target` bools breaks `store`,
   `portal`, `gateway` (and their tests) until those areas migrate (§1n lists every file).
   The `routing` package compiles/tests on its own, so this area's TDD is green in
   isolation, but the full-tree gates (`go build ./...`, `go test ./...`) only pass once the
   store/portal/gateway mode work lands. Sequence accordingly.
6. **Test framework/conventions:** Go stdlib `testing`, table-driven, `t.Fatalf`/`t.Errorf`;
   run from `gateway/backend` with `go test ./internal/routing/... -count=1`. Reuse the
   `must(t, err)` helper (memory_store_test.go:189) and `seededResolverStore(t, now)`
   (resolver_test.go:15). New-field tests fail as **compile errors** first (the repo's
   accepted red state); behavior tests fail on the assertion.
7. **Seed viability for `Resolve` in tests:** hand-rolled stores must set
   `Priority/Weight/TimeoutMS/AffinityTTLSeconds` on the app and `UpsertTelemetry` for the
   server, or `Score` may reject the only candidate and yield `ErrNoHealthyHost` instead of
   the intended result (mirror `seededResolverStore`).
8. **`targetFrom` must deep-copy `APIFlavors` into `Target`** (`append([]string(nil), …)`) —
   the value comes from the app's or spec's stored slice; without the copy a `Target`
   consumer could mutate store-owned backing state. `copyApplication` already isolates on
   read, but the spec branch reads a fresh slice each call, so copy in `targetFrom` too.
9. **Affinity reuse stays backward-compatible.** `applicationServesEndpoint`'s `default`
   branch is byte-identical to the old coarse gate, so existing affinity tests
   (resolver_test.go:709 etc.) that pass `"openai_chat"` are unaffected; only
   `openai_responses`/`anthropic_messages` gain the mode check.
10. **`groupReadCountingStore` (group_speed_order_test.go:28) embeds `resolverStore`**, so
    the new interface method needs no test-double edit. The only real `NewResolver` caller
    outside routing is `internal/gateway/server.go:586` passing `deps.Routes` (the full
    `Store`), which already satisfies the new method.
