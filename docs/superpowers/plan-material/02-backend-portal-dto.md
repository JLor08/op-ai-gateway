# Plan material — Area 02: Backend portal DTOs + validation (applications + runtime specs)

Scope: `gateway/backend/internal/portal/service_applications.go` and
`service_runtime.go`. Everything below is quoted from the worktree
`/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/api-variant-modes`
(origin/main content). Module path is `op-ai-gateway`; tests live under
`gateway/backend`.

This area OWNS the portal-facing request/response DTOs and their validation. It
DOES NOT own: the `routing.Application` / `routing.RuntimeSpec` struct fields,
the `EndpointMode` type, the store columns/CRUD/migration, the resolver
`Target`, or the frontend. Those are consumed from / produced by other areas
(see §3 Interfaces).

---

## 1. CURRENT STATE (exact excerpts, file:line)

### 1.1 The type this area CONSUMES (routing area produces it)

`routing.Application` — `store.go:496`, native flags at `store.go:519-524`:

```go
	// NativeResponses / NativeMessages enable per-application native passthrough:
	// when set, a client request to /v1/responses (Codex) resp. /v1/messages
	// (Claude Code / Anthropic) is proxied raw to the upstream's same native path
	// instead of being translated through the internal inference representation.
	NativeResponses bool
	NativeMessages  bool
```

`routing.RuntimeSpec` — `store.go:1242-1280`. It has `APIFlavors`? **No** — it
has no flavor field and no mode field today. (`APIFlavors []string` exists on
`Application` at `store.go:502`, but **not** on `RuntimeSpec`.) The routing/store
area must add `APIFlavors []string`, `ResponsesMode EndpointMode`,
`MessagesMode EndpointMode` to `RuntimeSpec`, and rename the two `Application`
bools to `ResponsesMode`/`MessagesMode EndpointMode`.

Flavor constants — `store.go:46-47`:

```go
	APIFlavorOpenAI    = "openai"
	APIFlavorAnthropic = "anthropic"
```

There is **no** `EndpointMode` type anywhere in `internal/` yet
(`grep EndpointMode` → 0 hits outside this plan). The routing/store area
introduces it; this area imports it as `routing.EndpointMode` (see §3).

### 1.2 `ApplicationDTO` — the two bool fields to replace

`service_applications.go:125-129`:

```go
	// NativeResponses / NativeMessages: when true the gateway proxies the raw
	// client body straight to the upstream's native endpoint (Codex /v1/responses
	// resp. Claude Code /v1/messages) instead of translating it.
	NativeResponses bool `json:"native_responses"`
	NativeMessages  bool `json:"native_messages"`
```

### 1.3 `CreateApplicationRequest` — plain bools

`service_applications.go:195-196`:

```go
		NativeResponses                  bool     `json:"native_responses"`
		NativeMessages                   bool     `json:"native_messages"`
```

### 1.4 `UpdateApplicationRequest` — pointer bools (keep-if-nil)

`service_applications.go:234-235`:

```go
		NativeResponses              *bool     `json:"native_responses,omitempty"`
		NativeMessages               *bool     `json:"native_messages,omitempty"`
```

### 1.5 Create mapping — where defaults are applied on create

`service_applications.go:375-407` builds `routing.Application{...}`; the native
fields at `service_applications.go:392-393`:

```go
			NativeResponses:                  req.NativeResponses,
			NativeMessages:                   req.NativeMessages,
```

**How defaults are applied on create today:** they are NOT — `req.NativeResponses`
is a plain `bool`, so an absent field defaults to Go's zero value `false`
(= "translate" today). There is no `normalize*` call for these two. (Contrast
`normalizeApplicationFlavors(req.APIFlavors)` at `service_applications.go:310`,
which defaults empty→both.) The new enum needs an explicit
default-to-`passthrough` on create — see Task A2.

### 1.6 Update apply block — pointer/keep-if-nil

`service_applications.go:598-603`:

```go
	if req.NativeResponses != nil {
		app.NativeResponses = *req.NativeResponses
	}
	if req.NativeMessages != nil {
		app.NativeMessages = *req.NativeMessages
	}
```

Note the repo's validate-before-mutate discipline: `UpdateApplication`
validates every fallible field into locals *before* the mutation block
(`service_applications.go:456-541`), then applies. The mode fields must follow
the same shape (validate at ~line 528 area, apply in the block).

### 1.7 `applicationDTO()` builder — the two DTO fields

`service_applications.go:742-784`; native fields at `service_applications.go:763-764`:

```go
		NativeResponses:              app.NativeResponses,
		NativeMessages:               app.NativeMessages,
```

### 1.8 `normalizeApplicationFlavors` + `ErrApplicationFlavorInvalid`

Error sentinel — `service_applications.go:35`:

```go
	ErrApplicationFlavorInvalid         = errors.New("application.flavor_invalid")
```

Function — `service_applications.go:1194-1214` (defaults empty→both, validates,
dedups):

```go
func normalizeApplicationFlavors(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}, nil
	}
	out := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for _, candidate := range raw {
		flavor := strings.TrimSpace(candidate)
		switch flavor {
		case routing.APIFlavorOpenAI, routing.APIFlavorAnthropic:
		default:
			return nil, ErrApplicationFlavorInvalid
		}
		if _, dup := seen[flavor]; dup {
			continue
		}
		seen[flavor] = struct{}{}
		out = append(out, flavor)
	}
	return out, nil
}
```

Called on create (`service_applications.go:310`) and update
(`service_applications.go:479`). This area will reuse it for the spec's
`api_flavors` too — see Task R2 and the DRY note there.

### 1.9 `RuntimeSpecGPUDTO` — unchanged, quoted for context

`service_runtime.go:293-297`:

```go
type RuntimeSpecGPUDTO struct {
	Index          int `json:"index"`
	VRAMEstimateMB int `json:"vram_estimate_mb"`
	VRAMMeasuredMB int `json:"vram_measured_mb"`
}
```

### 1.10 `RuntimeSpecDTO` — add trio here

`service_runtime.go:299-327` (full struct). No flavor/mode fields today; ends:

```go
	SetVisibleDevices bool                `json:"set_visible_devices"`
	GPUs              []RuntimeSpecGPUDTO `json:"gpus"`
}
```

### 1.11 `PutRuntimeSpecRequest` — add trio here (full-document upsert)

`service_runtime.go:329-353`. Ends:

```go
	SetVisibleDevices bool                `json:"set_visible_devices"`
	GPUs              []RuntimeSpecGPUDTO `json:"gpus"`
}
```

### 1.12 `putRuntimeSpec` — validation + spec build + parent app IS in scope

Signature `service_runtime.go:441`:

```go
func (s *Service) putRuntimeSpec(ctx context.Context, mapping routing.ModelMapping, app routing.Application, server routing.AIServer, req PutRuntimeSpecRequest) (RuntimeSpecDTO, error) {
```

The parent `server_agent` application is the `app` parameter — **already in
scope** in the create/upsert path. So a backend pre-fill from the parent app IS
possible here without a new store read. (Per spec §5.4 the FRONTEND pre-fills;
the backend just stores explicit values + a sane default. See Task R3.)

Validation block starts `service_runtime.go:445` (binary), admin_state switch
`service_runtime.go:457-462`, GPUs `service_runtime.go:463`, env `:466-470`,
visible-devices `:471-473`. The spec-build `routing.RuntimeSpec{...}` is
`service_runtime.go:520-540`; the new fields are set there (from `req`).

The `routing.RuntimeSpec` currently has no flavor/mode fields (§1.1), so
`spec.APIFlavors = ...` etc. depend on the routing/store area landing first.

### 1.13 `runtimeSpecDTO()` builder — add trio to the returned DTO

`service_runtime.go:685-729`; return literal `service_runtime.go:708-728`.
New fields read from `spec.APIFlavors` / `spec.ResponsesMode` /
`spec.MessagesMode`.

Also `GetRuntimeSpec`'s not-configured early return
`service_runtime.go:371` must seed sane empty values for the new fields:

```go
	if !ok {
		return RuntimeSpecDTO{MappingID: mapping.ID, Args: []string{}, Env: map[string]string{}, GPUs: []RuntimeSpecGPUDTO{}}, nil
	}
```

→ add `APIFlavors: []string{}` (or both defaults — decide with frontend; the
frontend pre-fills on create so `[]string{}` here is fine and avoids implying a
stored value) and the two modes as `""`. Recommendation: leave modes `""` and
`APIFlavors: []string{}` on the not-configured shape, since `Configured:false`
already tells the frontend to use its own pre-fill (§5.4).

### 1.14 AgentRuntimeSpecDTO / agent wire — CONFIRMED MUST STAY UNCHANGED

`AgentRuntimeSpecDTO` struct `service_runtime.go:1236-1258` — has **no**
`api_flavors`/`responses_mode`/`messages_mode`, and per spec §9 **must not gain
them**. Builder `agentRuntimeSpecDTO()` `service_runtime.go:1483-1528` — do not
touch. Assembly `AgentRuntimeConfig()` `service_runtime.go:1339-1449` and
`agentRuntimeConfigDTO()` `:1536-1556` — do not touch.

**Why (spec §9):** the agent's runtime router forwards `/v1/responses` and
`/v1/messages` verbatim to the child process and routes only on the top-level
`model` field; the disabled/translate/passthrough decision is taken by the
gateway *before* dispatch. The three new spec fields are gateway-side only and
never enter `AgentRuntimeSpecDTO` / `runtime.Spec`. Therefore no ServerAgent
version bump and no `server-agent/` change. A guard test (Task R4) pins this.

---

## 2. PROPOSED TDD TASKS (ordered, bite-sized)

Test framework: **stdlib `testing`, no testify.** Table-driven with
`cases := []struct{...}` + `t.Run(tc.name, ...)`; assertions via
`t.Fatalf`; error checks via `errors.Is(err, tc.wantErr)`. Fixtures:
`newServerTestService(t, now) (*Service, *routing.MemoryStore)`
(`service_test.go:1574`), `createTestServer(t, svc, name, domain)`
(`service_applications_test.go:20`), `ownerToken()` /`adminToken()`
(`service_test.go:1598,1610`), and for specs
`seedServerAgentApplication(t, routeStore, serverID, now)`
(`service_runtime_test.go:30`) + `svc.CreateMapping(...)`.

Run everything for this area with:

```
cd gateway/backend && go test ./internal/portal/...
```

Single test: `go test ./internal/portal/ -run TestName -count=1 -v`.

**Dependency note:** Tasks A1/R1 (the DTO field renames + spec fields) will not
COMPILE until the routing/store area has renamed `Application.NativeResponses`→
`ResponsesMode EndpointMode` etc. and added the `RuntimeSpec` trio + defined
`routing.EndpointMode`. Sequence the plan so the routing/store struct+type task
lands first (or in the same task-bundle commit). The tests below are written
against the post-rename world.

### Task A0 — shared validation helper + error sentinel (scaffolding, folded in)

Add to `service_applications.go` (near the other `Err*` at `:30-81` and the
other `normalize*` helpers ~`:1194`):

```go
// ErrApplicationEndpointModeInvalid rejects a responses_mode/messages_mode that
// is not one of the three EndpointMode values. HTTP 400 (a malformed request),
// mirroring ErrApplicationFlavorInvalid.
ErrApplicationEndpointModeInvalid = errors.New("application.endpoint_mode_invalid")
```

```go
// validEndpointMode reports whether raw (trimmed) is one of the three endpoint
// modes, returning the typed value. Callers wrap their own domain error so the
// application and runtime-spec write paths keep DISTINCT stable error codes.
func validEndpointMode(raw string) (routing.EndpointMode, bool) {
	switch m := routing.EndpointMode(strings.TrimSpace(raw)); m {
	case routing.EndpointModeDisabled, routing.EndpointModeTranslate, routing.EndpointModePassthrough:
		return m, true
	default:
		return "", false
	}
}
```

No standalone test — it is exercised through A1/A2/R2 below. (`strings` and
`routing` are already imported in this file.)

### Task A1 — `ApplicationDTO` emits `responses_mode`/`messages_mode`

**Failing test** — add to `service_applications_test.go`:

```go
func TestApplicationDTOCarriesEndpointModes(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	dto, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
		ResponsesMode: string(routing.EndpointModeTranslate),
		MessagesMode:  string(routing.EndpointModeDisabled),
	})
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if dto.ResponsesMode != string(routing.EndpointModeTranslate) {
		t.Fatalf("responses_mode = %q, want translate", dto.ResponsesMode)
	}
	if dto.MessagesMode != string(routing.EndpointModeDisabled) {
		t.Fatalf("messages_mode = %q, want disabled", dto.MessagesMode)
	}
}
```

Run: `go test ./internal/portal/ -run TestApplicationDTOCarriesEndpointModes -count=1`
→ **fails to compile** (unknown fields) then fails assertion.

**Minimal implementation:**

`service_applications.go:125-129` — replace the two bool DTO fields:

```go
	// ResponsesMode / MessagesMode are the per-endpoint EndpointMode
	// (disabled | translate | passthrough) for Codex /v1/responses resp.
	// Claude Code /v1/messages. Serialized as the lowercase enum string.
	ResponsesMode string `json:"responses_mode"`
	MessagesMode  string `json:"messages_mode"`
```

`service_applications.go:763-764` — in `applicationDTO()`:

```go
		ResponsesMode:                string(app.ResponsesMode),
		MessagesMode:                 string(app.MessagesMode),
```

→ test passes.

### Task A2 — create defaults to `passthrough` when absent + rejects unknown

**Failing tests** — add cases to `TestCreateApplicationValidation`
(`service_applications_test.go:451-489`) and a default test:

```go
{
	name: "bad responses_mode",
	mutate: func(req CreateApplicationRequest) CreateApplicationRequest {
		req.ResponsesMode = "bogus"
		return req
	},
	wantErr: ErrApplicationEndpointModeInvalid,
},
{
	name: "bad messages_mode",
	mutate: func(req CreateApplicationRequest) CreateApplicationRequest {
		req.MessagesMode = "sideways"
		return req
	},
	wantErr: ErrApplicationEndpointModeInvalid,
},
```

```go
func TestCreateApplicationEndpointModeDefaultsToPassthrough(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	dto, err := svc.CreateApplication(context.Background(), ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
		// ResponsesMode / MessagesMode absent
	})
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if dto.ResponsesMode != string(routing.EndpointModePassthrough) ||
		dto.MessagesMode != string(routing.EndpointModePassthrough) {
		t.Fatalf("defaults = %q/%q, want passthrough/passthrough", dto.ResponsesMode, dto.MessagesMode)
	}
}
```

**Minimal implementation:**

`service_applications.go:195-196` — replace bools in `CreateApplicationRequest`:

```go
		ResponsesMode                    string   `json:"responses_mode"`
		MessagesMode                     string   `json:"messages_mode"`
```

In `CreateApplication`, alongside the other normalizers (after
`normalizeApplicationFlavors` at `:310`):

```go
	responsesMode := routing.EndpointModePassthrough
	if strings.TrimSpace(req.ResponsesMode) != "" {
		m, ok := validEndpointMode(req.ResponsesMode)
		if !ok {
			return ApplicationDTO{}, ErrApplicationEndpointModeInvalid
		}
		responsesMode = m
	}
	messagesMode := routing.EndpointModePassthrough
	if strings.TrimSpace(req.MessagesMode) != "" {
		m, ok := validEndpointMode(req.MessagesMode)
		if !ok {
			return ApplicationDTO{}, ErrApplicationEndpointModeInvalid
		}
		messagesMode = m
	}
```

`service_applications.go:392-393` — in the `routing.Application{...}` literal:

```go
			ResponsesMode:                    responsesMode,
			MessagesMode:                     messagesMode,
```

Run both: `go test ./internal/portal/ -run 'TestCreateApplicationValidation|TestCreateApplicationEndpointModeDefaultsToPassthrough' -count=1` → pass.

> Default rationale (spec §6): every supported upstream now serves both native
> endpoints, so the uniform create default is `passthrough`. Migration backfill
> maps the OLD bools (`native_*=true→passthrough`, `false→translate`) — that is
> the store/migration area, not here; this default only governs a fresh create
> whose caller sent nothing.

### Task A3 — update keeps-if-nil, validates non-nil, rejects unknown

**Failing test:**

```go
func TestUpdateApplicationEndpointModes(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
	}) // both default passthrough
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}

	// keep-if-nil: update an unrelated field, modes untouched.
	got, err := svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{
		Weight: intPtr(3),
	})
	if err != nil {
		t.Fatalf("UpdateApplication(weight): %v", err)
	}
	if got.ResponsesMode != string(routing.EndpointModePassthrough) {
		t.Fatalf("keep-if-nil failed: responses_mode = %q", got.ResponsesMode)
	}

	// explicit set.
	disabled := string(routing.EndpointModeDisabled)
	got, err = svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{
		ResponsesMode: &disabled,
	})
	if err != nil {
		t.Fatalf("UpdateApplication(responses_mode): %v", err)
	}
	if got.ResponsesMode != disabled || got.MessagesMode != string(routing.EndpointModePassthrough) {
		t.Fatalf("set failed: %q / %q", got.ResponsesMode, got.MessagesMode)
	}

	// non-nil unknown (incl. explicit "") rejected.
	bad := "bogus"
	if _, err := svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{
		MessagesMode: &bad,
	}); !errors.Is(err, ErrApplicationEndpointModeInvalid) {
		t.Fatalf("bad update err = %v, want ErrApplicationEndpointModeInvalid", err)
	}
}
```

> Check `intPtr` exists in the portal test package; if not, use an inline
> `w := 3; ... Weight: &w`. (`service_applications_test.go` already constructs
> `*int`/`*bool` pointers inline in several places — grep for `func intPtr` and
> fall back to inline if absent.)

**Minimal implementation:**

`service_applications.go:234-235` — replace pointer bools:

```go
		ResponsesMode                *string   `json:"responses_mode,omitempty"`
		MessagesMode                 *string   `json:"messages_mode,omitempty"`
```

In `UpdateApplication`, validate-before-mutate (near the other validated locals,
~`service_applications.go:511-527`):

```go
	var responsesMode, messagesMode routing.EndpointMode
	if req.ResponsesMode != nil {
		m, ok := validEndpointMode(*req.ResponsesMode)
		if !ok {
			return ApplicationDTO{}, ErrApplicationEndpointModeInvalid
		}
		responsesMode = m
	}
	if req.MessagesMode != nil {
		m, ok := validEndpointMode(*req.MessagesMode)
		if !ok {
			return ApplicationDTO{}, ErrApplicationEndpointModeInvalid
		}
		messagesMode = m
	}
```

Replace the apply block `service_applications.go:598-603`:

```go
	if req.ResponsesMode != nil {
		app.ResponsesMode = responsesMode
	}
	if req.MessagesMode != nil {
		app.MessagesMode = messagesMode
	}
```

Run: `go test ./internal/portal/ -run TestUpdateApplicationEndpointModes -count=1` → pass.

> Semantics note for the plan author: on update a **non-nil empty string** is an
> explicit invalid value → rejected (there is no "clear to empty" for a
> three-state enum; nil already means keep). This mirrors the retired pointer
> bools where nil=keep and a value=set, and it is the honest reading of "reject
> unknown."

### Task R1 — `RuntimeSpecDTO` + `PutRuntimeSpecRequest` gain the trio

**Failing test** — add to `service_runtime_test.go`:

```go
func TestRuntimeSpecDTOCarriesFlavorsAndModes(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen", AppModelName: "qwen"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled:       true,
		Binary:        "/usr/local/bin/llama-server",
		APIFlavors:    []string{routing.APIFlavorOpenAI},
		ResponsesMode: string(routing.EndpointModeTranslate),
		MessagesMode:  string(routing.EndpointModeDisabled),
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if len(dto.APIFlavors) != 1 || dto.APIFlavors[0] != routing.APIFlavorOpenAI {
		t.Fatalf("api_flavors = %#v", dto.APIFlavors)
	}
	if dto.ResponsesMode != string(routing.EndpointModeTranslate) || dto.MessagesMode != string(routing.EndpointModeDisabled) {
		t.Fatalf("modes = %q/%q", dto.ResponsesMode, dto.MessagesMode)
	}
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if len(got.APIFlavors) != 1 || got.ResponsesMode != string(routing.EndpointModeTranslate) {
		t.Fatalf("read-back = %#v", got)
	}
}
```

**Minimal implementation:**

`RuntimeSpecDTO` (`service_runtime.go:325-326` area) — add before `GPUs`:

```go
	APIFlavors    []string `json:"api_flavors"`
	ResponsesMode string   `json:"responses_mode"`
	MessagesMode  string   `json:"messages_mode"`
```

`PutRuntimeSpecRequest` (`service_runtime.go:351-352` area) — add the same three
fields (strings + `[]string`, full-document upsert, no pointers).

`putRuntimeSpec` spec-build literal `service_runtime.go:520-540` — add
(after the validation from Task R2 computes `flavors/respMode/msgMode`):

```go
		APIFlavors:                  flavors,
		ResponsesMode:               respMode,
		MessagesMode:                msgMode,
```

`runtimeSpecDTO()` return literal `service_runtime.go:708-728` — add:

```go
		APIFlavors:    append([]string{}, spec.APIFlavors...),
		ResponsesMode: string(spec.ResponsesMode),
		MessagesMode:  string(spec.MessagesMode),
```

`GetRuntimeSpec` not-configured return `service_runtime.go:371` — add
`APIFlavors: []string{}` so the slice never marshals as JSON null (modes stay
`""`; `Configured:false` signals the frontend to pre-fill).

Run: `go test ./internal/portal/ -run TestRuntimeSpecDTOCarriesFlavorsAndModes -count=1` → pass (after R2 lands the validation).

### Task R2 — spec validation: modes + flavors, with defaults

**Failing test** — add cases to `TestPutRuntimeSpecValidation`
(`service_runtime_test.go:161-206`):

```go
{
	name:      "bad responses_mode",
	mappingID: agentMapping.ID,
	mutate:    func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { r.ResponsesMode = "bogus"; return r },
	wantErr:   ErrRuntimeSpecEndpointModeInvalid,
},
{
	name:      "bad api_flavor",
	mappingID: agentMapping.ID,
	mutate:    func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { r.APIFlavors = []string{"openai", "bogus"}; return r },
	wantErr:   ErrRuntimeSpecFlavorInvalid,
},
```

Plus a defaults test:

```go
func TestPutRuntimeSpecModeAndFlavorDefaults(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server",
		// APIFlavors / ResponsesMode / MessagesMode absent
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if len(dto.APIFlavors) != 2 {
		t.Fatalf("api_flavors default = %#v, want both", dto.APIFlavors)
	}
	if dto.ResponsesMode != string(routing.EndpointModePassthrough) ||
		dto.MessagesMode != string(routing.EndpointModePassthrough) {
		t.Fatalf("mode defaults = %q/%q, want passthrough", dto.ResponsesMode, dto.MessagesMode)
	}
}
```

**Minimal implementation:**

Add sentinels in `service_runtime.go` (near `:32-70`):

```go
	// ErrRuntimeSpecEndpointModeInvalid rejects a responses_mode/messages_mode
	// that is not one of the three EndpointMode values. HTTP 400.
	ErrRuntimeSpecEndpointModeInvalid = errors.New("runtime_spec.endpoint_mode_invalid")
	// ErrRuntimeSpecFlavorInvalid rejects an api_flavors entry that is not
	// openai/anthropic. HTTP 400.
	ErrRuntimeSpecFlavorInvalid = errors.New("runtime_spec.flavor_invalid")
```

In `putRuntimeSpec`, in the validate-before-mutate section (after the env loop
`service_runtime.go:466-470`, before the `args` marshal):

```go
	respMode := routing.EndpointModePassthrough
	if strings.TrimSpace(req.ResponsesMode) != "" {
		m, ok := validEndpointMode(req.ResponsesMode)
		if !ok {
			return RuntimeSpecDTO{}, ErrRuntimeSpecEndpointModeInvalid
		}
		respMode = m
	}
	msgMode := routing.EndpointModePassthrough
	if strings.TrimSpace(req.MessagesMode) != "" {
		m, ok := validEndpointMode(req.MessagesMode)
		if !ok {
			return RuntimeSpecDTO{}, ErrRuntimeSpecEndpointModeInvalid
		}
		msgMode = m
	}
	flavors, err := normalizeRuntimeSpecFlavors(req.APIFlavors)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
```

`validEndpointMode` is the shared helper from Task A0 (same package). For
flavors, add a thin wrapper in `service_runtime.go` so the error code is
spec-scoped rather than borrowing `application.flavor_invalid`:

```go
// normalizeRuntimeSpecFlavors is normalizeApplicationFlavors with the
// runtime-spec error code. Defaults empty→both, dedups, validates.
func normalizeRuntimeSpecFlavors(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}, nil
	}
	out := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for _, candidate := range raw {
		flavor := strings.TrimSpace(candidate)
		switch flavor {
		case routing.APIFlavorOpenAI, routing.APIFlavorAnthropic:
		default:
			return nil, ErrRuntimeSpecFlavorInvalid
		}
		if _, dup := seen[flavor]; dup {
			continue
		}
		seen[flavor] = struct{}{}
		out = append(out, flavor)
	}
	return out, nil
}
```

> DRY alternative (flag for the plan author): reuse `normalizeApplicationFlavors`
> directly and accept that a bad flavor on a **spec** write surfaces the code
> `application.flavor_invalid`. That is one fewer function but a slightly
> dishonest error code on the runtime-spec endpoint. **Recommendation:** keep the
> separate `ErrRuntimeSpecFlavorInvalid` (honest, matches the `runtime_spec.*`
> family already in this file) — the duplication is ~15 trivial lines. Decide at
> plan time; spec §13 lists "align error codes with the runtime.* style" as an
> open item, which favors the scoped code.

Run: `go test ./internal/portal/ -run 'TestPutRuntimeSpecValidation|TestPutRuntimeSpecModeAndFlavorDefaults' -count=1` → pass.

### Task R3 — spec CREATE pre-fill: FRONTEND pre-fills; backend stores + defaults

**Decision (per spec §5.4 + §3.3): the FRONTEND pre-fills.** The runtime-spec
API is a full-document upsert with no field inheritance
(`PutRuntimeSpecRequest` doc `service_runtime.go:329-332`), and the frontend's
spec-create form is pre-filled from the parent `server_agent` application's
current `api_flavors` + modes (§5.4), so the request already carries explicit
values. The backend therefore does **not** read the parent app to inherit — it
stores what the request sends and only supplies a sane default when a field is
**absent** (Task R2's defaults: flavors→both, modes→passthrough).

**Confirm the backend still needs a default when absent:** yes — an API client
(not the portal frontend) may PUT a spec omitting the trio; without R2's
defaults those become empty/`""`, which is not a valid stored snapshot (spec
§3.3 says specs are "stored explicitly, never null after backfill/create"). The
R2 defaults satisfy that.

**No backend pre-fill-from-app task is needed.** `putRuntimeSpec` DOES have the
parent `app` in scope (`service_runtime.go:441`), so if the plan later decides
the backend should mirror the app when the request omits the fields, the hook
exists — but per the approved spec that is explicitly NOT the chosen design
(§5.4: "the existing runtime-spec model is a full-document upsert with no field
inheritance; this fits it"; §12 Out of Scope: "Dynamic inheritance of app values
into existing specs"). **Flag any deviation loudly** — do not silently add an
app→spec read in the backend, it contradicts the spec.

Optional guard test to pin the decision (that absent fields default rather than
inherit the parent app's non-default values):

```go
func TestPutRuntimeSpecDoesNotInheritAppModes(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	// Seed a server_agent app whose modes are non-default (disabled).
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	// (seed helper sets APIFlavors:[openai]; extend it or store.Update to set
	//  ResponsesMode=disabled for a sharper assertion — see GOTCHAS on the seed.)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server",
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	// Backend default is passthrough, NOT the app's stored value.
	if dto.ResponsesMode != string(routing.EndpointModePassthrough) {
		t.Fatalf("responses_mode = %q, want passthrough (no backend inheritance)", dto.ResponsesMode)
	}
}
```

No new implementation — this passes once R2 lands. (Its value is documenting the
"no backend inheritance" contract.)

### Task R4 — GUARD: agent wire stays unchanged

**Failing/guard test** — pins spec §9. A cheap way that does not depend on
reflection over unexported shapes: assert the agent runtime-config JSON for a
spec carrying non-default flavors/modes contains none of the three keys.

```go
func TestAgentRuntimeConfigOmitsFlavorsAndModes(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server",
		APIFlavors: []string{routing.APIFlavorOpenAI}, ResponsesMode: string(routing.EndpointModeDisabled),
	}); err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	cfg, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, k := range []string{"api_flavors", "responses_mode", "messages_mode"} {
		if strings.Contains(string(raw), k) {
			t.Fatalf("agent runtime-config leaked %q: %s", k, raw)
		}
	}
}
```

Run: `go test ./internal/portal/ -run TestAgentRuntimeConfigOmitsFlavorsAndModes -count=1`.
Passes as long as `AgentRuntimeSpecDTO`/`agentRuntimeSpecDTO` are left untouched.
(`json` and `strings` already imported in `service_runtime_test.go`.)

---

## 3. INTERFACES

### PRODUCES (this area's names other areas consume)

Portal DTOs / wire JSON (canonical names — match the spec):
- `ApplicationDTO.ResponsesMode string` json `responses_mode`
- `ApplicationDTO.MessagesMode  string` json `messages_mode`
- `CreateApplicationRequest.ResponsesMode/MessagesMode string` (optional; default `passthrough`)
- `UpdateApplicationRequest.ResponsesMode/MessagesMode *string` (keep-if-nil; reject non-nil unknown)
- `RuntimeSpecDTO.APIFlavors []string` json `api_flavors`
- `RuntimeSpecDTO.ResponsesMode/MessagesMode string` json `responses_mode`/`messages_mode`
- `PutRuntimeSpecRequest.APIFlavors []string`, `.ResponsesMode/.MessagesMode string`
- Stable error codes (new): `application.endpoint_mode_invalid`
  (`ErrApplicationEndpointModeInvalid`), `runtime_spec.endpoint_mode_invalid`
  (`ErrRuntimeSpecEndpointModeInvalid`), `runtime_spec.flavor_invalid`
  (`ErrRuntimeSpecFlavorInvalid`).
- Helpers: `validEndpointMode(raw string) (routing.EndpointMode, bool)` in
  `service_applications.go`; `normalizeRuntimeSpecFlavors([]string)` in
  `service_runtime.go`.

The **frontend** area consumes these exact JSON keys
(`responses_mode`/`messages_mode`/`api_flavors`) in `api/models.ts` +
`api/runtime.ts` (spec §8). The **gateway handler / error-mapper** area maps the
three new sentinels to HTTP 400. Note the `native_*` DTO/request JSON keys are
**removed** on this branch — the frontend and any api-surface doc must drop them.

### CONSUMES (produced by the routing/store area — must land first)

- `type EndpointMode string` and consts
  `EndpointModeDisabled="disabled"`, `EndpointModeTranslate="translate"`,
  `EndpointModePassthrough="passthrough"` — **canonical, from spec §3.1.**
  Package: expected `routing` (the flavor consts `APIFlavorOpenAI/Anthropic`
  already live at `routing/store.go:46-47`, and `Application`/`RuntimeSpec` are
  in `routing`, so `routing.EndpointMode` is the natural home; this area imports
  `op-ai-gateway/internal/routing` already). **Deviation to flag:** if the
  routing area instead puts it in a sub-package, update the qualifier in every
  helper above.
- `routing.Application.ResponsesMode EndpointMode`,
  `routing.Application.MessagesMode EndpointMode` (replacing
  `NativeResponses`/`NativeMessages bool` at `store.go:523-524`).
- `routing.RuntimeSpec.APIFlavors []string`,
  `routing.RuntimeSpec.ResponsesMode/MessagesMode EndpointMode` (new on
  `store.go:1242-1280`).
- `routing.APIFlavorOpenAI` / `routing.APIFlavorAnthropic` (existing).

Also downstream (NOT this area, listed so names stay consistent): resolver
`Target.NativeResponses/NativeMessages bool` (`resolver.go:64-69`) becomes
`Target.APIFlavors []string` + `Target.ResponsesMode/MessagesMode EndpointMode`;
`native_passthrough.go` reads the effective mode and emits the new stable error
codes `responses.endpoint_disabled` / `messages.endpoint_disabled` (spec §4) —
those two are the ENFORCEMENT area's codes, distinct from this area's three
VALIDATION codes above; do not conflate them.

---

## 4. GOTCHAS

- **Test framework:** stdlib `testing`, **no testify**. Table tests +
  `t.Run` + `errors.Is` + `t.Fatalf`. Copy the exact shape of
  `TestCreateApplicationValidation` (`service_applications_test.go:444`) and
  `TestPutRuntimeSpecValidation` (`service_runtime_test.go:138`). Run:
  `cd gateway/backend && go test ./internal/portal/...`.
- **Compile dependency ordering:** the DTO renames (A1) and spec fields (R1)
  reference `routing.EndpointMode` and the renamed/added `routing` struct
  fields. Sequence the routing/store struct+type task BEFORE (or same commit as)
  these portal tasks, or `go build ./internal/portal/` fails. State this
  explicitly in the plan so an executor doesn't start with a red compile.
- **`native_*` has other readers to migrate in lockstep:** `resolver.go:902-903`
  (`NativeResponses: app.NativeResponses`) and `store/sqlite_applications.go`
  (columns at lines 25, 89, 146, 161, 479 + `migrate.go:528-529,784-791`) still
  reference the old bools. Renaming the struct fields breaks those until their
  areas update. This area only touches the portal package, but the plan must
  fan the rename out. (Not this area's tests, but they'll go red on the shared
  build.)
- **AgentRuntimeSpecDTO must NOT change (spec §9):** do not add the trio to
  `service_runtime.go:1236-1258` or its builder `:1483-1528`. Task R4 guards it.
  No ServerAgent version bump; `server-agent/` untouched.
- **`seedServerAgentApplication` sets `APIFlavors: []string{routing.APIFlavorOpenAI}`
  only** (`service_runtime_test.go:38`) and NO modes — after the routing rename
  it will zero-value `ResponsesMode`/`MessagesMode` to `""`. For any spec test
  that wants a non-default parent-app mode (e.g. the R3 guard's sharper
  assertion), extend the seed struct literal or do a `routeStore.UpdateApplication`
  with explicit modes. Also: this seed bypasses `CreateApplication`, so it does
  NOT exercise the create default — keep A2's default test on the real
  `CreateApplication` path.
- **JSON-null-slice trap (caught twice on this branch — see
  `agentRuntimeConfigDTO` doc `service_runtime.go:1530-1535`):** `RuntimeSpecDTO.APIFlavors`
  must serialize as `[]`, never `null`. Use `append([]string{}, spec.APIFlavors...)`
  in `runtimeSpecDTO` and seed `APIFlavors: []string{}` in `GetRuntimeSpec`'s
  not-configured branch (`service_runtime.go:371`).
- **Cross-driver / parity (spec §7):** the new columns
  (`applications.responses_mode/messages_mode`,
  `agent_runtime_specs.api_flavors/responses_mode/messages_mode`, all `text`)
  and `application_column_parity_test.go` are the store area's job — but this
  area's round-trip tests (A1, R1) implicitly rely on the MemoryStore carrying
  the new struct fields. MemoryStore stores the struct directly, so it needs no
  column work, but the sqlite/postgres CRUD + parity test MUST land or the
  round-trip passes on memory and fails the conformance suite. Note it as a
  cross-area gate; Postgres via `OP_AI_GATEWAY_TEST_POSTGRES_DSN`
  (`store/conformance_test.go`, `store/postgres_test.go`).
- **Backfill vs create default are different rules, don't cross them (spec §6/§7):**
  create default here = `passthrough` (uniform). Migration backfill (store area)
  = old bool mapping (`true→passthrough`, `false→translate`) to preserve
  existing behaviour. This area's tests must not assert migration behaviour and
  vice-versa.
- **Update empty-string semantics:** decided as reject (non-nil `""` → invalid).
  If the plan prefers "empty = keep", change A3's helper call to treat `""` as a
  no-op; but that muddies "reject unknown" and diverges from the pointer-bool
  precedent. Recommend reject.
