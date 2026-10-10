# Design — Anthropic prompt caching on the translate path

Date: 2026-10-10
Branch: `feat/anthropic-prompt-caching` (off `main` c293af9)
Status: design, pending user review

## 1. Goal & root cause

The gateway's Anthropic **translate** request builder (`anthropicRequestBody`,
`provider/anthropic_messages.go`) never emits a `cache_control` breakpoint, so
Anthropic never writes or reads its prompt cache on any translate request (portal
chat and any external OpenAI-chat→Anthropic client). Result: `cache_creation_input_tokens`
and `cache_read_input_tokens` come back 0 every turn — the capture/store/display
pipeline is fully capable but has nothing to show, and repeated chat context is
re-billed at full price every turn.

Goal: inject `cache_control` breakpoints into the translate request, behind a
flag, with an automatic switch that turns caching on only when it is economically
sensible — so cache writes/reads appear in the Activity list **and** repeated
context is billed at cache rates.

## 2. Mechanics (from the Claude API prompt-caching reference)

- `cache_control: {type: "ephemeral"}` marks a breakpoint; the cached prefix is
  everything rendered up to and including that block. Render order `tools → system
  → messages`. Max **4** breakpoints/request; each walks back ≤20 positions to find
  a prior entry.
- **Minimum cacheable prefix is model-dependent; below it, `cache_control` is a
  silent no-op** (`cache_creation=0`, no error, no cost): 512 (Opus 5.x / Sonnet 5.5
  / Haiku 5.5 / Fable 5.x), 1024 (Opus 4.8 / Sonnet 5 / Sonnet 4.x), 2048 (Opus 4.7),
  4096 (Opus 4.6/4.5 / Haiku 4.5).
- Pricing: read ~0.1× input (0.05× Opus 5.5); write **1.25× (5-min TTL)**, 2× (1-h).
  Break-even at **2** requests (5-min). A read refreshes the entry timer.
- The only waste case is a **large, never-reused one-shot** (pays +25% write, no read).

## 3. Decisions (locked in brainstorming)

1. **Scope:** all Anthropic **translate** traffic (api-key + subscription-masquerade);
   the native-passthrough relay is untouched (it already forwards the client's own
   `cache_control`).
2. **Flag-gated, default off.** Off = today's behavior, byte-identical.
3. **Auto-switch = hybrid (C):** cache when the prefix is big enough to cache AND
   likely to be reused (see §5).
4. **Breakpoints:** last system block (stable prefix = tools+system) + a moving
   breakpoint on the last block of the latest turn; ≤2 in v1.
5. **TTL:** `ephemeral` 5-min default; 1-h selectable via config.
6. **Display:** no change. The cache-read (`cached_tokens`) tile and group column are
   already default-visible and will populate once the flag is on; the
   `cache_write_tokens` tile/column stay hidden by default (enable manually to inspect
   writes). → Feature is backend-only (+ docs).

## 4. Architecture — decision up, placement down

- **Decision (gateway/inference layer):** has the flag, the model, the messages, and
  the session source. Computes a cache directive and attaches it to the neutral
  `inference.Request`.
- **Placement (provider builder):** `anthropicRequestBody` reads the directive and
  places breakpoints. The provider stays decoupled from flags/sessions — it only
  honors a directive. Non-Anthropic providers ignore it.

New field on `inference.Request` (neutral carrier that already flows into
`AnthropicClient.Complete`/`CompleteStream` → `anthropicRequestBody`):

```go
// PromptCache, when non-nil and Enabled, asks a provider that supports prompt
// caching (Anthropic translate) to place cache_control breakpoints. nil / !Enabled
// = no caching (today's behavior). Providers without caching ignore it.
type PromptCacheDirective struct {
    Enabled bool
    TTL     string // "" or "5m" → ephemeral default; "1h" → ttl:"1h"
}
```

The gateway sets `req.PromptCache` right before dispatch, next to where it resolves
the target / builds the stream. Everything else about the request is unchanged.

## 5. The auto-switch (hybrid C)

Computed at the gateway layer:

```
enableCache =
    flagOn
 && targetProviderIsAnthropicTranslate          // ProviderVendorAnthropic / subscription-Anthropic translate
 && estPrefixTokens >= modelMinimum(targetModel) // per-model min table (§2)
 && (hasHistory || isChatSession)
```

- `estPrefixTokens`: cheap char estimate (`len/4`) over tools + system (+ prior
  turns). Only needs to be approximate — below the real minimum Anthropic no-ops for
  free, so erring toward caching is safe.
- `modelMinimum`: small table keyed by the upstream model family (default 1024 for
  an unknown model — conservative).
- `hasHistory`: the request carries ≥1 completed prior turn (an assistant message in
  the history) — proof the prefix is being resent/reused.
- `isChatSession`: the request is a portal-chat run (available at the gateway layer
  via the session source) — catches turn 1 of a chat so writes appear from the first
  answer. If plumbing the session flag to the decision point is awkward, `hasHistory`
  alone is the floor (writes then appear from turn 2); confirm at implementation.

Rationale: writes happen only when the prefix is both large enough to actually cache
and likely to be read again, so the +25% write is essentially always recovered;
large one-shots (no history, not chat) are never cached.

## 6. Breakpoint placement (provider)

When `req.PromptCache.Enabled`:

1. Add an optional `CacheControl *anthropicCacheControl` field (`json:"cache_control,omitempty"`)
   to `anthropicSystemBlock` and `anthropicBlock`. New type:
   `type anthropicCacheControl struct { Type string `json:"type"`; TTL string `json:"ttl,omitempty"` }`
   (`Type` always `"ephemeral"`; `TTL` set only for 1-h).
2. **System breakpoint:** render `system` as a block array (via a caching-aware
   variant of `anthropicSystemField`) and put `cache_control` on the **last** block:
   - api-key path (today a plain string): wrap as one `text` block carrying `cache_control`.
   - subscription masquerade (already an array; first block = `claudeCodeSystemPrompt`):
     put `cache_control` on the last block (caches the large stable masquerade prefix —
     a strong win).
   - empty system (api-key, no system text): no system block → skip the system
     breakpoint; the last-turn breakpoint still caches the conversation prefix.
3. **Moving breakpoint:** put `cache_control` on the **last content block of the last
   message** (the most-recently-appended turn).
4. Never exceed 4 breakpoints (v1 places ≤2). Long single turns (>20 positions)
   needing intermediate breakpoints are a follow-up.

Off (directive nil/disabled): `anthropicSystemField` and the blocks render exactly as
today (plain string system, no `cache_control`) — byte-identical.

## 7. Prefix stability (correctness)

Caching only *reads* if the rendered prefix is byte-identical across turns. The
translate render re-renders the same conversation each turn, so it should be stable,
but confirm no per-request nondeterminism enters the cached prefix (no timestamp in
system, deterministic tool ordering/JSON keys). **Acceptance test:** a 2-turn
conversation produces `cache_read_input_tokens > 0` on turn 2 (otherwise caching
writes every turn for a pure +25% surcharge).

## 8. Config / flag

- New system-settings key(s) via `service_system_settings.go` (same key/value
  mechanism as `vendor_accounts_enabled`): `anthropic_prompt_caching_enabled` (bool,
  default false) and `anthropic_prompt_caching_ttl` (`"5m"` default | `"1h"`).
  Surfaced in the SystemSettings DTO + update path + the portal settings UI toggle.
- The key/value settings store takes a new key at the code level; **confirm whether
  it needs a schema migration** (the value table is generic key/value, so likely
  not). If it does, run the **Postgres leg**; otherwise no store change.

## 9. Display

**No frontend change.** The cache-read (`cached_tokens`) stat tile and group column
are already `defaultVisible: true` and will start showing real values once the flag
is on and traffic caches. `cache_write_tokens` stays hidden by default (tile, group,
and per-row table) — enable it manually to inspect writes. (Rationale: the read tile
already confirms caching is working; the write counter is a drill-down, not a default.)

## 10. Testing & verification

- Go: the auto-switch decision matrix (flag on/off × history y/n × prefix ≥/< min ×
  chat vs not × Anthropic vs other provider); `cache_control` placement in the
  rendered body for api-key (string→block) and subscription (masquerade array) paths;
  the empty-system edge; flag-off render byte-identical to today; TTL 5m/1h emitted
  correctly; native-passthrough path unchanged.
- A prefix-stability test (two consecutive turns share a byte-identical cached prefix).
- No frontend change → no frontend test for this feature.
- Gates: build/vet/`go test ./...` + golangci; lint-docs; SonarQube branch-findings = 0.
  Postgres leg only if §8 needs a migration. (Frontend vitest/build/lint unaffected —
  no frontend change — but the repo's frontend CI still runs.)

## 11. Out of scope

- Native-passthrough caching (client already sends its own `cache_control`).
- Intermediate breakpoints for very long single turns (>20 positions).
- `cache_control` on non-translate or non-Anthropic paths.
- OpenAI caching (automatic upstream; nothing to send).

## 12. Confirm at implementation

- Exact system-settings plumbing + whether a migration is required (→ Postgres leg).
- Whether the session-source (`isChatSession`) signal is cleanly available at the
  decision point; if not, ship with `hasHistory` as the reuse gate (writes from turn 2).
- Exact upstream-model string available at the decision point for the min-table lookup
  (`target.ProviderModel`).
