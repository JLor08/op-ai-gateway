# Anthropic translate prompt caching — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Inject Anthropic `cache_control` breakpoints into the translate request, behind a flag with a hybrid auto-switch, so repeated context is billed at cache rates and cache tokens appear in the Activity list.

**Architecture:** The decision is made at the gateway layer (reads the flag, inspects the resolved target + request) and carried on `inference.Request.PromptCache`; the Anthropic translate builder (`anthropicRequestBody`) honors it and places breakpoints. Non-Anthropic providers ignore it. Native passthrough is untouched.

**Tech Stack:** Go backend (`gateway/backend`, module `op-ai-gateway`), React/TS portal (`gateway/frontend`), SQLite + Postgres stores, SonarQube.

**Spec:** `docs/superpowers/specs/2026-10-10-anthropic-translate-prompt-caching-design.md`

## Global Constraints

- Repo-facing text (code, comments, commits, PRs, docs) in **English**; chat may be German.
- **No attribution lines** in commits or the PR.
- Push over **SSH** (the `gh` token 403s on push).
- Flag-gated, **default off**; off = today's behavior, byte-identical render.
- Scope: Anthropic **translate** builder (`anthropicRequestBody`) only — api-key **and** subscription-masquerade. Native-passthrough relay (`native_passthrough.go`) is **not** touched.
- **v1 TTL = 5-minute ephemeral only** (`cache_control: {type:"ephemeral"}`, no `ttl` field, no beta header). 1-hour TTL is a documented follow-up (needs the Anthropic extended-cache-ttl beta header).
- Auto-switch (hybrid C): cache iff `flagOn && anthropic-translate target && estPrefixTokens ≥ modelMinimum && (hasAssistantHistory || SessionSource=="chat")`.
- Breakpoints: last system block (rendered as a block array) + the last content block of the last message; ≤2 in v1 (limit 4).
- **No Activity-display change** (cache-read tile is already default-visible; cache-write stays hidden by default).
- Store/migration change ⇒ run the **Postgres leg**. The flag is a key/value system setting — expected to need **no migration**; confirm, and only run the Postgres leg if a migration proves necessary.
- Pre-PR gates: Go build/vet/`go test ./...` + golangci-lint (gofumpt/gocritic); frontend vitest + build + lint + **prettier format:check** (a frontend toggle is in scope); lint-docs; **SonarQube branch-findings = 0**.
- `api_tracing_gen.go` is gowrap-generated — regenerate with `go generate`, never hand-edit.

## Review Focus

- **Prefix non-determinism** → caching writes every turn and never reads (pure +25% surcharge): a per-request value in the cached prefix (system text, tool order, JSON key order) must not vary turn-to-turn. Pinned by Task 3's render-stability test.
- **Empty system text (api-key, no system)** → there is no system block to mark: the builder must still render validly (place only the last-turn breakpoint, no system breakpoint). Pinned in Task 2.
- **Flag off / directive nil** → the rendered body must be **byte-identical** to today (no `cache_control`, system stays a plain string). Pinned in Task 2.
- **Non-Anthropic / non-translate target** with the flag on → no `cache_control` anywhere (OpenAI providers ignore the directive; the gateway never sets it for them). Pinned in Task 3.
- **Subscription masquerade** (system already a block array) → `cache_control` lands on the last system block without disturbing the first (exact `claudeCodeSystemPrompt`) block. Pinned in Task 2.

---

## Task 1: Flag — `anthropic_prompt_caching_enabled` system setting + Portal accessor

**Files:**
- Modify: `gateway/backend/internal/portal/service_system_settings.go` (key const, DTO read field, update field + write, reader)
- Modify: `gateway/backend/internal/portal/api.go` (interface: new method)
- Regenerate: `gateway/backend/internal/portal/api_tracing_gen.go` (`go generate ./...`)
- Test: `gateway/backend/internal/portal/service_system_settings_test.go` (extend)

**Interfaces:**
- Produces: `Service.AnthropicPromptCachingEnabled(ctx context.Context) bool`; system setting key `anthropic_prompt_caching_enabled` (bool, default false); `SystemSettingsDTO.AnthropicPromptCachingEnabled bool`; `UpdateSystemSettingsRequest.AnthropicPromptCachingEnabled *bool`.

Mirror `vendor_accounts_enabled` exactly (it is the reference implementation in this file).

- [ ] **Step 1: Write the failing test**

Extend the system-settings test (find the existing `vendor_accounts_enabled` round-trip test with `grep -n "VendorAccountsEnabled" gateway/backend/internal/portal/service_system_settings_test.go` and mirror it):

```go
func TestAnthropicPromptCachingSetting(t *testing.T) {
	svc := newSystemSettingsFixture(t) // existing harness used by the vendor_accounts test
	ctx := context.Background()

	// default off
	if got := svc.AnthropicPromptCachingEnabled(ctx); got {
		t.Fatalf("default = %v, want false", got)
	}
	dto, err := svc.SystemSettings(ctx /* + whatever args the existing test passes */)
	if err != nil { t.Fatal(err) }
	if dto.AnthropicPromptCachingEnabled { t.Fatalf("DTO default should be false") }

	// enable, read back
	on := true
	if _, err := svc.UpdateSystemSettings(ctx, /* owner/token as the existing test */ UpdateSystemSettingsRequest{AnthropicPromptCachingEnabled: &on}); err != nil {
		t.Fatal(err)
	}
	if got := svc.AnthropicPromptCachingEnabled(ctx); !got {
		t.Fatalf("after enable = %v, want true", got)
	}
}
```

(Adapt the fixture/arg shape to the existing `vendor_accounts_enabled` test in the same file — do not invent a new harness.)

- [ ] **Step 2: Run to verify it fails**

Run: `cd gateway/backend && go test ./internal/portal/ -run TestAnthropicPromptCachingSetting -v`
Expected: FAIL — `AnthropicPromptCachingEnabled` / the DTO field / the request field undefined.

- [ ] **Step 3: Add the key + DTO + update + reader (service_system_settings.go)**

Next to the `vendorAccountsEnabledKey` const and its usages:
- `const anthropicPromptCachingEnabledKey = "anthropic_prompt_caching_enabled"`
- `SystemSettingsDTO`: add `AnthropicPromptCachingEnabled bool `json:"anthropic_prompt_caching_enabled"``; set it in the DTO assembly from the values map (mirror the `VendorAccountsEnabled(values)` line) with a `boolSetting(values, anthropicPromptCachingEnabledKey, false)`-style read (reuse whatever bool-reader the file uses).
- `UpdateSystemSettingsRequest`: add `AnthropicPromptCachingEnabled *bool `json:"anthropic_prompt_caching_enabled"``; in the update, mirror the vendor block: `if req.AnthropicPromptCachingEnabled != nil { writes = append(writes, settingWrite{anthropicPromptCachingEnabledKey, strconv.FormatBool(*req.AnthropicPromptCachingEnabled)}) }`.
- Add the reader method:

```go
// AnthropicPromptCachingEnabled reports the anthropic_prompt_caching_enabled master
// flag (off by default): when on, the Anthropic translate path places cache_control
// breakpoints on eligible requests. See docs .../external-* and ADR.
func (s *Service) AnthropicPromptCachingEnabled(ctx context.Context) bool {
	// mirror VendorAccountsEnabled: read the single bool setting
	...
}
```
(Copy the body of `VendorAccountsEnabled` and swap the key.)

- [ ] **Step 4: Add to the Portal API interface + regenerate the tracing wrapper**

- `api.go`: add `AnthropicPromptCachingEnabled(context.Context) bool` next to `VendorAccountsEnabled`.
- Run `cd gateway/backend && go generate ./...` to regenerate `api_tracing_gen.go` (do NOT hand-edit it). Verify the generated method appears.

- [ ] **Step 5: Run to verify it passes**

Run: `cd gateway/backend && go build ./... && go test ./internal/portal/ -run TestAnthropicPromptCaching -v`
Expected: PASS. Confirm `git diff --stat` shows `api_tracing_gen.go` changed only by the new generated method.

- [ ] **Step 6: Store conformance / migration check**

Confirm the setting is a key/value row needing no schema change: `grep -n "settingWrite\|system_settings" gateway/backend/internal/store/*.go` — if `system_settings` is a generic key→value table (it is for `vendor_accounts_enabled`), no migration is needed. If a migration IS required, add it (next number, `addColumnIfMissing` pattern) and run the Postgres leg. Note the outcome in the report.

- [ ] **Step 7: Commit**

```bash
git add gateway/backend/internal/portal/service_system_settings.go gateway/backend/internal/portal/api.go gateway/backend/internal/portal/api_tracing_gen.go gateway/backend/internal/portal/service_system_settings_test.go
git commit -m "feat: anthropic_prompt_caching_enabled system setting + Portal accessor"
```

---

## Task 2: Provider — place `cache_control` in the Anthropic translate builder

**Files:**
- Modify: `gateway/backend/internal/inference/types.go` (new `PromptCacheDirective` + `Request.PromptCache`)
- Modify: `gateway/backend/internal/provider/anthropic_messages.go` (cache-control type + block fields + caching render + `anthropicRequestBody`)
- Test: `gateway/backend/internal/provider/anthropic_messages_test.go` (extend) — or a new `anthropic_cache_test.go`

**Interfaces:**
- Produces: `inference.PromptCacheDirective{Enabled bool; TTL string}`, `inference.Request.PromptCache *PromptCacheDirective`; the builder honors it.
- Consumes: nothing from other tasks (the gateway sets `PromptCache` in Task 3).

- [ ] **Step 1: Add the directive type (inference/types.go)**

```go
// PromptCacheDirective asks a provider that supports prompt caching (Anthropic
// translate) to place cache_control breakpoints on this request. nil or
// Enabled=false means no caching (today's behavior). Providers without caching
// support ignore it. The gateway computes it from the flag + request shape
// (see gateway.applyAnthropicCachePolicy); the provider only honors it.
type PromptCacheDirective struct {
	Enabled bool
	TTL     string // "" / "5m" = ephemeral default; "1h" reserved (not emitted in v1)
}
```
Add `PromptCache *PromptCacheDirective `json:"-"`` to `Request` (json-ignored — internal only).

- [ ] **Step 2: Write the failing placement test (provider)**

```go
func TestAnthropicRequestBodyCacheControl(t *testing.T) {
	req := inference.Request{
		Messages: []inference.Message{
			{Role: inference.RoleSystem, Content: inference.TextContent("you are helpful and this system text is long enough")},
			{Role: inference.RoleUser, Content: inference.TextContent("hi")},
			{Role: inference.RoleAssistant, Content: inference.TextContent("hello")},
			{Role: inference.RoleUser, Content: inference.TextContent("again")},
		},
		PromptCache: &inference.PromptCacheDirective{Enabled: true},
	}
	target := routing.Target{ProviderModel: "claude-sonnet-5-5"}

	raw, err := anthropicRequestBody(target, req, true)
	if err != nil { t.Fatal(err) }
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil { t.Fatal(err) }

	// system is an array whose LAST block carries cache_control: ephemeral
	sys, ok := body["system"].([]any)
	if !ok { t.Fatalf("system is not a block array: %T", body["system"]) }
	last := sys[len(sys)-1].(map[string]any)
	if cc, ok := last["cache_control"].(map[string]any); !ok || cc["type"] != "ephemeral" {
		t.Fatalf("no ephemeral cache_control on last system block: %v", last)
	}
	// last message's last content block carries cache_control
	msgs := body["messages"].([]any)
	lastMsg := msgs[len(msgs)-1].(map[string]any)
	lastContent := lastMsg["content"].([]any)
	lb := lastContent[len(lastContent)-1].(map[string]any)
	if _, ok := lb["cache_control"]; !ok {
		t.Fatalf("no cache_control on last turn's last block: %v", lb)
	}

	// flag off → byte-identical to no directive
	reqOff := req; reqOff.PromptCache = nil
	off, _ := anthropicRequestBody(target, reqOff, true)
	bare, _ := anthropicRequestBody(target, inference.Request{Messages: req.Messages}, true)
	if string(off) != string(bare) {
		t.Fatalf("directive-nil render differs from bare render")
	}
	if strings.Contains(string(off), "cache_control") {
		t.Fatalf("flag-off body must not contain cache_control")
	}
}
```
(Adapt `inference.Message` construction to the real constructors — `grep -n "func TextContent\|RoleSystem\|RoleUser" gateway/backend/internal/inference/*.go`.)

- [ ] **Step 3: Run to verify it fails**

Run: `cd gateway/backend && go test ./internal/provider/ -run TestAnthropicRequestBodyCacheControl -v`
Expected: FAIL — system is a plain string / no cache_control.

- [ ] **Step 4: Implement placement (anthropic_messages.go)**

1. New type + helper:
```go
type anthropicCacheControl struct {
	Type string `json:"type"`           // always "ephemeral" in v1
	TTL  string `json:"ttl,omitempty"`  // reserved for 1h; unset in v1
}
func ephemeralCacheControl(_ string) *anthropicCacheControl { return &anthropicCacheControl{Type: "ephemeral"} }
```
2. Add `CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`` to `anthropicSystemBlock` and `anthropicBlock`.
3. Caching-aware system render — new func that always returns a block array when caching, placing `cache_control` on the last block:
```go
// anthropicSystemFieldCached renders the system field as a block array with
// cache_control on the LAST block. For the masquerade the array already exists
// (first block = claudeCodeSystemPrompt); for the plain path it wraps the joined
// string as one block. Returns nil when there is no system text and no masquerade
// (no block to mark — the caller still marks the last turn).
func anthropicSystemFieldCached(system, masquerade string, cc *anthropicCacheControl) any {
	field := anthropicSystemField(system, masquerade) // reuse existing logic
	switch v := field.(type) {
	case []anthropicSystemBlock:
		if len(v) == 0 { return v }
		v[len(v)-1].CacheControl = cc
		return v
	case string:
		return []anthropicSystemBlock{{Type: "text", Text: v, CacheControl: cc}}
	default: // nil (empty system, no masquerade)
		return field
	}
}
```
4. In `anthropicRequestBody`, after `body` is built, apply caching when directed:
```go
if req.PromptCache != nil && req.PromptCache.Enabled {
	cc := ephemeralCacheControl(req.PromptCache.TTL)
	body.System = anthropicSystemFieldCached(system, target.Masquerade, cc)
	if n := len(body.Messages); n > 0 {
		m := &body.Messages[n-1]
		if c := len(m.Content); c > 0 {
			m.Content[c-1].CacheControl = cc
		}
	}
}
```
(Place this right after the existing `body := anthropicRequest{...}` block, before tools/temperature so it does not interfere.)

- [ ] **Step 5: Run to verify it passes (+ empty-system & masquerade cases)**

Add two more sub-tests before running: (a) `target.Masquerade = routing.MasqueradeClaudeCode` → system array's FIRST block is still the exact `claudeCodeSystemPrompt` and the LAST block carries cache_control; (b) a request with no system text and `Enabled:true` → no system cache block, body still valid JSON, last-turn block still marked.
Run: `cd gateway/backend && go test ./internal/provider/ -run TestAnthropicRequestBodyCacheControl -v && go vet ./internal/provider/ && golangci-lint run`
Expected: PASS / clean.

- [ ] **Step 6: Commit**

```bash
git add gateway/backend/internal/inference/types.go gateway/backend/internal/provider/anthropic_messages.go gateway/backend/internal/provider/anthropic_messages_test.go
git commit -m "feat: Place cache_control in the Anthropic translate builder when directed"
```

---

## Task 3: Gateway — decide and set `PromptCache` at dispatch

**Files:**
- Create: `gateway/backend/internal/gateway/anthropic_cache_policy.go` (decision + helpers)
- Modify: `gateway/backend/internal/gateway/inference_complete.go:54` (apply at `providerReq := req`)
- Modify: `gateway/backend/internal/gateway/stream_session.go:131` (apply at `providerReq := req`)
- Modify: `gateway/backend/internal/portal/api.go` consumer side — the gateway `s.Portal` already exposes `AnthropicPromptCachingEnabled` from Task 1.
- Test: `gateway/backend/internal/gateway/anthropic_cache_policy_test.go`

**Interfaces:**
- Consumes: `inference.PromptCacheDirective` (Task 2), `s.Portal.AnthropicPromptCachingEnabled(ctx)` (Task 1), `routing.Target` (`.Provider`, `.ProviderModel`, `.Masquerade`), `inference.Request` (`.Messages`, `.Tools`, `.SessionSource`).
- Produces: `(*Server).applyAnthropicCachePolicy(ctx, target, *inference.Request)` — sets `req.PromptCache` in place when eligible.

- [ ] **Step 1: Write the failing decision test**

```go
func TestApplyAnthropicCachePolicy(t *testing.T) {
	s := &Server{Portal: cachingPortalStub{on: true}} // stub returning AnthropicPromptCachingEnabled=true
	antTarget := routing.Target{Provider: routing.ProviderVendorAnthropic, ProviderModel: "claude-sonnet-5-5"}
	bigHistory := []inference.Message{
		{Role: inference.RoleSystem, Content: inference.TextContent(strings.Repeat("x", 4096))},
		{Role: inference.RoleUser, Content: inference.TextContent("q1")},
		{Role: inference.RoleAssistant, Content: inference.TextContent("a1")},
		{Role: inference.RoleUser, Content: inference.TextContent("q2")},
	}
	cases := []struct{ name string; target routing.Target; req inference.Request; want bool }{
		{"anthropic + history + big", antTarget, inference.Request{Messages: bigHistory}, true},
		{"anthropic + chat session, no history yet", antTarget, inference.Request{SessionSource: "chat", Messages: bigHistory[:2]}, true},
		{"anthropic + one-shot (no history, not chat)", antTarget, inference.Request{Messages: bigHistory[:2]}, false},
		{"anthropic + too small prefix", antTarget, inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: inference.TextContent("hi")}, {Role: inference.RoleAssistant, Content: inference.TextContent("yo")}, {Role: inference.RoleUser, Content: inference.TextContent("x")}}}, false},
		{"non-anthropic target", routing.Target{Provider: routing.ProviderVendorOpenAI, ProviderModel: "gpt-4o"}, inference.Request{Messages: bigHistory}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			s.applyAnthropicCachePolicy(context.Background(), tc.target, &req)
			got := req.PromptCache != nil && req.PromptCache.Enabled
			if got != tc.want { t.Fatalf("= %v, want %v", got, tc.want) }
		})
	}

	// flag off → never set, even for the eligible case
	sOff := &Server{Portal: cachingPortalStub{on: false}}
	req := inference.Request{Messages: bigHistory}
	sOff.applyAnthropicCachePolicy(context.Background(), antTarget, &req)
	if req.PromptCache != nil { t.Fatalf("flag off must not set PromptCache") }
}
```
(Use the gateway test package's existing Server/Portal stubbing — `grep -n "Portal:" gateway/backend/internal/gateway/*_test.go` for the stub pattern; add `AnthropicPromptCachingEnabled` to that stub.)

- [ ] **Step 2: Run to verify it fails**

Run: `cd gateway/backend && go test ./internal/gateway/ -run TestApplyAnthropicCachePolicy -v`
Expected: FAIL — `applyAnthropicCachePolicy` undefined.

- [ ] **Step 3: Implement the policy (anthropic_cache_policy.go)**

```go
package gateway

import (
	"context"
	"strings"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
)

// anthropicCacheMinTokens is the model's minimum cacheable prefix (tokens). Below
// it, Anthropic ignores cache_control for free, so an unknown model defaults to the
// conservative 1024. Source: Claude prompt-caching reference.
func anthropicCacheMinTokens(model string) int {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "opus-4-6"), strings.Contains(m, "opus-4-5"), strings.Contains(m, "haiku-4-5"):
		return 4096
	case strings.Contains(m, "opus-4-7"):
		return 2048
	case strings.Contains(m, "opus-5"), strings.Contains(m, "sonnet-5-5"), strings.Contains(m, "haiku-5-5"), strings.Contains(m, "fable-5"):
		return 512
	default:
		return 1024 // Opus 4.8, Sonnet 5, Sonnet 4.x, and unknown
	}
}

// estPrefixTokens is a cheap char/4 estimate of the cacheable prefix (system +
// developer text + prior turns + tools). It only needs to clear the model minimum;
// below it caching is a free no-op, so erring high is safe.
func estPrefixTokens(req inference.Request) int {
	chars := 0
	for _, m := range req.Messages {
		chars += len(m.Text())
	}
	for _, tl := range req.Tools {
		chars += len(tl.Name) + len(tl.Description)
	}
	return chars / 4
}

func hasAssistantHistory(msgs []inference.Message) bool {
	for _, m := range msgs {
		if m.Role == inference.RoleAssistant {
			return true
		}
	}
	return false
}

func isAnthropicTranslateTarget(t routing.Target) bool {
	return t.Provider == routing.ProviderVendorAnthropic // translate path; subscription-anthropic also resolves to this provider
}

// applyAnthropicCachePolicy sets req.PromptCache when the hybrid auto-switch says
// caching is worthwhile: flag on, Anthropic translate target, the estimated prefix
// clears the model minimum, and the prefix is likely reused (has assistant history
// or is a portal-chat session). Otherwise it leaves req.PromptCache nil.
func (s *Server) applyAnthropicCachePolicy(ctx context.Context, target routing.Target, req *inference.Request) {
	if s.Portal == nil || !s.Portal.AnthropicPromptCachingEnabled(ctx) {
		return
	}
	if !isAnthropicTranslateTarget(target) {
		return
	}
	if estPrefixTokens(*req) < anthropicCacheMinTokens(target.ProviderModel) {
		return
	}
	if !hasAssistantHistory(req.Messages) && req.SessionSource != "chat" {
		return
	}
	req.PromptCache = &inference.PromptCacheDirective{Enabled: true} // TTL "" = 5m ephemeral
}
```
(Confirm `routing.ProviderVendorAnthropic` is the exact constant, and whether subscription-Anthropic translate carries the same `Provider` value; if subscription uses a distinct constant, OR it in. `grep -n "ProviderVendorAnthropic\|ProviderVendorAnthropicSubscription" gateway/backend/internal/routing/store.go`.)

- [ ] **Step 4: Wire it at the two dispatch hooks**

- `inference_complete.go` after `providerReq := req` (line ~54) and after `providerReq.Model = target.ProviderModel` if present there: `s.applyAnthropicCachePolicy(provCtx, target, &providerReq)`.
- `stream_session.go` after `providerReq := req` / `providerReq.Model = target.ProviderModel` (lines ~131-133): `s.applyAnthropicCachePolicy(ctx, target, &providerReq)`.
(Apply AFTER `providerReq.Model = target.ProviderModel` so nothing downstream overwrites it; the policy reads `target.ProviderModel` directly, so order there is independent.)

- [ ] **Step 5: Add the render-stability test (Review Focus: prefix non-determinism)**

In the gateway or provider test, assert two consecutive turns of a growing conversation share a byte-identical cached prefix (determinism), without a live API:

```go
func TestAnthropicCachedPrefixIsStableAcrossTurns(t *testing.T) {
	target := routing.Target{ProviderModel: "claude-sonnet-5-5"}
	sys := inference.Message{Role: inference.RoleSystem, Content: inference.TextContent(strings.Repeat("sys ", 300))}
	turn1 := inference.Request{Messages: []inference.Message{sys, {Role: inference.RoleUser, Content: inference.TextContent("q1")}, {Role: inference.RoleAssistant, Content: inference.TextContent("a1")}, {Role: inference.RoleUser, Content: inference.TextContent("q2")}}, PromptCache: &inference.PromptCacheDirective{Enabled: true}}
	turn2 := turn1
	turn2.Messages = append(append([]inference.Message{}, turn1.Messages...), inference.Message{Role: inference.RoleAssistant, Content: inference.TextContent("a2")}, inference.Message{Role: inference.RoleUser, Content: inference.TextContent("q3")})

	b1, _ := anthropicRequestBody(target, turn1, true)
	b2, _ := anthropicRequestBody(target, turn2, true)
	// The system field must be byte-identical between turns EXCEPT the moving marker.
	// Assert the rendered `system` array is identical across both bodies.
	var m1, m2 map[string]any
	json.Unmarshal(b1, &m1); json.Unmarshal(b2, &m2)
	s1, _ := json.Marshal(m1["system"]); s2, _ := json.Marshal(m2["system"])
	if string(s1) != string(s2) {
		t.Fatalf("system (cached stable prefix) differs across turns:\n%s\n%s", s1, s2)
	}
}
```
(This test lives where `anthropicRequestBody` is accessible — the provider package. If Task 3's policy test is in the gateway package, put this stability test in the provider test file instead; note it in the report.)

- [ ] **Step 6: Run to verify it passes**

Run: `cd gateway/backend && go build ./... && go test ./internal/gateway/ ./internal/provider/ -run 'AnthropicCache|CachePolicy|CachedPrefix' -v && golangci-lint run`
Expected: PASS / clean.

- [ ] **Step 7: Commit**

```bash
git add gateway/backend/internal/gateway/anthropic_cache_policy.go gateway/backend/internal/gateway/anthropic_cache_policy_test.go gateway/backend/internal/gateway/inference_complete.go gateway/backend/internal/gateway/stream_session.go gateway/backend/internal/provider/anthropic_messages_test.go
git commit -m "feat: Gateway auto-switch sets Anthropic prompt-cache directive at dispatch"
```

---

## Task 4: Frontend — System Settings toggle

**Files:**
- Modify: `gateway/frontend/src/api/system.ts` (DTO read + update field)
- Modify: `gateway/frontend/src/components/SystemSettings.tsx` (toggle)
- Modify: `gateway/frontend/src/i18n.ts` (de + en labels)
- Test: `gateway/frontend/src/components/SystemSettings.test.tsx` (extend), `gateway/frontend/src/i18n.test.ts` (extend)

**Interfaces:**
- Consumes: the backend field `anthropic_prompt_caching_enabled` (Task 1).

- [ ] **Step 1: Add the wire field (api/system.ts)**

Add `anthropic_prompt_caching_enabled: boolean;` to the SystemSettings DTO type (next to `vendor_accounts_enabled` at :83) and `anthropic_prompt_caching_enabled?: boolean;` to the update request type (next to :499).

- [ ] **Step 2: Add i18n keys + the failing i18n test**

Add to BOTH `de` and `en` blocks (next to the vendor-accounts settings labels — `grep -n "vendorAccountsEnabled\|VendorAccounts" gateway/frontend/src/i18n.ts`):
```ts
settingsAnthropicPromptCaching: 'Anthropic Prompt-Caching', // en: 'Anthropic prompt caching'
settingsAnthropicPromptCachingHelp: 'Cacht wiederholten Kontext bei Anthropic-Modellen (spart Kosten; experimentell).', // en: 'Caches repeated context for Anthropic models (saves cost; experimental).'
```
Add an `i18n.test.ts` parity block for the two keys (copy the existing settings-keys block).
Run: `cd gateway/frontend && npx vitest run src/i18n.test.ts` → fail (missing keys) → pass after adding.

- [ ] **Step 3: Write the failing component test (SystemSettings.test.tsx)**

Mirror the existing `vendor_accounts_enabled` toggle test (`grep -n "vendor_accounts_enabled\|VendorAccounts" gateway/frontend/src/components/SystemSettings.test.tsx`): render settings with the fake api, assert the Anthropic-prompt-caching switch shows the stored value, toggling it calls the update api with `{ anthropic_prompt_caching_enabled: true }`.

- [ ] **Step 4: Run to verify it fails**

Run: `cd gateway/frontend && npx vitest run src/components/SystemSettings.test.tsx`
Expected: FAIL — no such toggle.

- [ ] **Step 5: Implement the toggle (SystemSettings.tsx)**

Add a switch row mirroring the `vendor_accounts_enabled` toggle (same component/handler pattern), bound to `anthropic_prompt_caching_enabled`, labelled `t.settingsAnthropicPromptCaching` with helper `t.settingsAnthropicPromptCachingHelp`. Place it near the vendor-accounts toggle (both are experimental flags).

- [ ] **Step 6: Run to verify it passes + full frontend gate**

Run: `cd gateway/frontend && npx vitest run src/components/SystemSettings.test.tsx src/i18n.test.ts && npx tsc --noEmit && npm run lint && npm run format:check && npm run build`
Expected: PASS / clean.

- [ ] **Step 7: Commit**

```bash
git add gateway/frontend/src/api/system.ts gateway/frontend/src/components/SystemSettings.tsx gateway/frontend/src/i18n.ts gateway/frontend/src/components/SystemSettings.test.tsx gateway/frontend/src/i18n.test.ts
git commit -m "feat: System Settings toggle for Anthropic prompt caching"
```

---

## Task 5: Documentation

**Files:**
- Modify: `docs/architecture/cross-cutting/external-vendor-accounts.md` (or the translate/provider doc — whichever documents the Anthropic translate path) — new section on translate prompt caching.
- Modify: `docs/architecture/reference/api-surface.md` (the new system setting `anthropic_prompt_caching_enabled`).
- Create/append: a new **ADR** in `docs/architecture/09-architecture-decisions.md` (ADRs are `## ADR-NNN` entries there — use the next number).
- Modify: any system-settings reference doc that enumerates flags (mirror where `vendor_accounts_enabled` is documented — `grep -rn "vendor_accounts_enabled" docs/`).

**Interfaces:** none (docs).

- [ ] **Step 1: Document the feature**
  - The translate-path caching: what it does, the hybrid auto-switch rule (flag + Anthropic translate + prefix ≥ model min + history-or-chat), breakpoint placement (system block + last turn), 5m TTL, the prefix-stability requirement, and that native passthrough is unaffected.
  - The new `anthropic_prompt_caching_enabled` setting (default off, experimental) + the UI toggle; note no Activity-display change (read tile already visible; write hidden by default).
  - ADR: record the decision (translate-path caching, flag-gated, hybrid auto-switch, decision-at-gateway/placement-at-provider, 5m-only v1, 1h deferred).

- [ ] **Step 2: Verify docs**

Run: `make lint-docs` (the known branch-local `docs/superpowers/` citation failure is expected and disappears when branch-local files are removed before the PR; any other failure must be fixed).

- [ ] **Step 3: Commit**

```bash
git add docs/
git commit -m "docs: Document Anthropic translate prompt caching + the enable setting"
```

---

## Verification (before PR)

- Backend: `cd gateway/backend && go build ./... && go vet ./... && go test ./...` + `golangci-lint run` (+ `golangci-lint fmt --diff`). **Postgres leg only if Task 1 needed a migration** (the flag is a key/value setting — expected not to).
- Frontend: `cd gateway/frontend && npm ci && npx vitest run --coverage --retry=2 && npm run build && npm run lint && npm run format:check` (the `--retry=2` dodges the #197 `chooseAuthType` flake).
- Docs: `make lint-docs`.
- **SonarQube gate:** `make sonar-up`, scan, `make sonar-findings && make sonar-branch-findings` → 0 branch findings; note it in the PR.
- Remove branch-local files (`docs/superpowers/`), push over SSH, open the PR, bind + get_status, `make sonar-down`.

## Follow-ups (out of v1 scope)

- **1-hour TTL** (`cache_control: {type:"ephemeral", ttl:"1h"}`) — needs the Anthropic extended-cache-ttl beta header on the translate `post()`; verify the current beta string, add the header when any block uses 1h, expose the TTL in the setting + UI.
- Intermediate breakpoints for very long single turns (>20 positions).

## Self-review notes (coverage check)

- Spec §3 scope (translate, api-key+subscription, native-passthrough untouched) → Task 2 placement + Task 3 target gate. §4 architecture (decision up / placement down) → Task 2 (`PromptCache` on Request) + Task 3 (`applyAnthropicCachePolicy`). §5 auto-switch → Task 3 decision matrix. §6 placement → Task 2. §7 prefix stability → Task 3 Step 5. §8 flag → Task 1. §9 no display change → honored (no frontend Activity change; the only frontend change is the settings toggle). §10 testing → each task + Verification. §12 confirm-at-impl items → called out inline (settings migration, session signal [SessionSource is on the request — resolved], provider-model string).
- Deviation: TTL is 5m-only in v1 (spec said 1h optional) — deferred to Follow-ups with rationale (beta-header). Flag the user on this at handoff.
- Type names consistent: `inference.PromptCacheDirective`/`Request.PromptCache`; `anthropicCacheControl`/`ephemeralCacheControl`/`anthropicSystemFieldCached`; `Server.applyAnthropicCachePolicy`/`anthropicCacheMinTokens`/`estPrefixTokens`/`hasAssistantHistory`/`isAnthropicTranslateTarget`; `Service.AnthropicPromptCachingEnabled` + key `anthropic_prompt_caching_enabled`.
