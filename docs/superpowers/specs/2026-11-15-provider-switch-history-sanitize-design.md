# Provider switch: sanitize foreign reasoning in replayed history

Status: draft for approval (2026-11-15). Branch `fix/codex-passthrough-foreign-reasoning`.

## Intent

A Codex or Claude Code session must survive a change of target in the middle of
the session: local model to vendor, vendor to local model, and vendor to vendor,
for the OpenAI Responses and the Anthropic Messages protocol. Today it can break,
because the client replays its whole history on every request and the gateway
relays that history verbatim on the native passthrough. Items the previous target
produced reach a new target that cannot read them, and it rejects them.

The trigger (2026-11-14): Codex started on a local Qwen model (served through the
gateway's translate path), then switched to `gpt-6-astra` on a ChatGPT
subscription. The backend answered 400 `Invalid 'input[153].content': array too
long. Expected an array with maximum length 0` (`array_above_max_length`). Every
further request of that session failed the same way. The live probes (below)
show that the vendor → local direction breaks too.

## Decided with the user

- **Drop foreign items, do not convert them.** A target cannot use another
  target's reasoning (encrypted, signed, or model-specific). Converting it into
  text would change the conversation and is not representable everywhere.
- **One central place in the gateway**, in `proxyNative`
  (`internal/gateway/native_passthrough.go`), next to `rewriteModelField`.
- **Optimistic for what cannot be recognised by shape.** Send the body
  unchanged; only when the target answers with a recognised "foreign reasoning"
  400, remove the old blobs and retry once.
- **Remember per session** which blobs a target rejected, so follow-up requests
  are filtered up front instead of failing again; forget them once they stop
  appearing (context compaction).
- **No performance cost for requests that need nothing:** a byte scan gates every
  parse; the retry path costs nothing until a target actually rejects.
- **Live probes** run by me through the user's gateway with a temporary token
  (done 2026-11-15, results below). The PR ships with unit/integration tests; the
  combinations the user's accounts cannot exercise are marked VERIFY-LIVE.

## Goals

- No session breaks because of reasoning/thinking items another target produced.
- One place, one rule table, one test matrix, one documentation section.
- A body that needs no change is forwarded byte-for-byte as today.
- The payload capture keeps recording the client's bytes (as for every other edit).
- Continuity of the *new* target's own reasoning is kept from the switch on.

## Non-goals

- Converting or summarizing foreign reasoning for the new model.
- Item types other than reasoning/thinking (e.g. `additional_tools`,
  `web_search_call`, `item_reference`) — follow-up only if a switch is shown to
  break on them.
- The translate path: its renderers already drop assistant reasoning
  (`openaiResponsesInput`, `anthropicMessages`).
- Gemini and the OpenAI-compatible vendors: translate-only, no passthrough.

## Anti-goals

- Touching a request that does not involve a switch (byte-identity must hold).
- Removing an item the target itself issued and needs (a ChatGPT reasoning item
  with valid `encrypted_content`, a signed Anthropic thinking block).
- Filtering a whole session's reasoning after a switch (degrades OpenAI quality;
  the new target's own blocks must survive).
- Retrying on anything but the recognised errors (no retry on 429, 5xx, ordinary
  validation errors); never more than one retry per request.
- Silent rewrites an operator cannot see: removals and retries are logged at
  debug level with counts.

## Approaches considered

1. **Per provider client** (how the first fix was built, in
   `OpenAIResponsesClient.ProxyNative`): rules spread over three clients, and
   `OpenAICompatibleClient` serves many target kinds. Rejected.
2. **Central sanitizer in `proxyNative`, table-driven**, plus an optimistic
   retry with a per-session memo. **Chosen.**
3. **Mark gateway-minted items and remove only marked ones**: covers local →
   vendor only and changes what clients receive. Rejected as the sole mechanism.

An alternative the user proposed and we refined: track per session the last
target (account + model), set a "moved" flag on a change, try optimistically,
and set a session-wide "filter" flag after a successful filtered retry. Kept: the
optimistic try and the per-session memory. Changed: (a) the error itself is the
signal, so no target tracking is needed (tracking fails after a restart, across
instances, for sessions started elsewhere, for Claude Code subagents sharing one
session id, and on A → B → A); (b) the memory holds the *hashes of the rejected
blobs* instead of a session-wide filter flag, so the new target's own reasoning
is kept.

## Live probe results (2026-11-15, through the live gateway, verbatim passthrough)

| # | Target | Replayed history | Result |
|---|---|---|---|
| O1 | ChatGPT subscription (`gpt-5.6-luna`) | baseline | 200; reasoning item with `encrypted_content` (1976 B), `content: []` |
| O2 | ChatGPT subscription | gateway-minted reasoning (`reasoning_text`, `encrypted_content: ""`) | 400 `array_above_max_length` on `input[n].content` |
| O3 | ChatGPT subscription | its own reasoning item | 200 |
| O3b | ChatGPT subscription | its own item + non-empty `content` | 400 `array_above_max_length` |
| O4 | ChatGPT subscription | own item, tampered / garbage `encrypted_content` | 400 `invalid_encrypted_content` ("The encrypted content for item rs_… could not be verified") |
| O5 | ChatGPT subscription, `gpt-6-astra` | blob issued for `gpt-5.6-luna` (same account) | 200 |
| A1 | Anthropic subscription (`claude-haiku-4-5`) | baseline tool call, thinking on | 200; `thinking` (signed) + `tool_use` |
| A2 | Anthropic | thinking with `signature: ""` (gateway-minted), tool loop | 400 ``messages.1.content.0: Invalid `signature` in `thinking` block`` |
| A3 | Anthropic | tampered signature, tool loop | 400, same message |
| A4 | Anthropic | thinking block removed, tool loop, thinking **on** | 200 |
| A5 | Anthropic | thinking block removed, tool loop, thinking off | 200 |
| A6 | Anthropic, `claude-haiku-5-5` | block signed for `claude-haiku-4-5` (same account) | 200 |
| L1 | local Qwen (`qwen3.8-27b-fp8`), `/v1/messages` | signed Anthropic thinking | 200 |
| L2 | local Qwen, `/v1/messages` | `redacted_thinking` | 200 |
| L3 | local Qwen, `/v1/responses` | ChatGPT reasoning (encrypted only) | 400 `Encrypted content is not supported. (parameter=input)` |
| L3g | local Gemma (`gemma-4-E4B…`), `/v1/responses` | ChatGPT reasoning (encrypted only) | 400 `item['content'] is empty` |
| L4 | local Qwen, `/v1/responses` | reasoning with `reasoning_text`, no encryption | 200 |

Not probeable with the user's accounts (VERIFY-LIVE): an OpenAI **api-key**
target, an Anthropic **api-key** target, and a switch between **two accounts of
the same vendor** (expected to behave like O4/A3, since a foreign account's blob
cannot be decrypted or verified).

Consequences: model changes within one account need nothing (O5, A6); dropping a
thinking block in a tool loop is accepted even with thinking on (A4), so
`thinking` never has to be switched off; vendor → local breaks today on the
Responses endpoint (L3, L3g).

## Mechanism

Applies only to the JSON body of a native passthrough whose fine flavor is
`openai_responses` (`input` array) or `anthropic_messages` (assistant turns'
`content` arrays). Target class from `Target.Provider`: OpenAI subscription
(`vendor_openai_subscription`), OpenAI api-key (`vendor_openai`), Anthropic
(`vendor_anthropic`, api-key or subscription), local (every non-vendor provider).

### Step 1 — deterministic shape rules (stateless, always)

Items the target provably cannot read are removed before the first send:

| Endpoint | Target class | Drop | Strip |
|---|---|---|---|
| `/v1/responses` | OpenAI subscription / api-key | `reasoning` items with empty, missing or non-string `encrypted_content` (O2) | `content` of the remaining `reasoning` items, only when it is a non-empty array (O3b) |
| `/v1/responses` | local | `reasoning` items with no readable text in `content` or `summary` (L3, L3g) | `encrypted_content` of the remaining ones (L3) |
| `/v1/messages` | Anthropic | `thinking` blocks with empty or missing `signature` (A2) | nothing |
| `/v1/messages` | local | nothing needed (L1, L2) | nothing |

An Anthropic assistant turn left with no content block is removed as a whole
turn, and the two user turns that then meet are merged (content arrays
concatenated in order), so a local chat template that requires strict
alternation still gets it.

### Step 2 — optimistic send, one retry on a recognised error

Everything step 1 does not remove (valid-looking encrypted blobs and signatures
that may belong to another account) is sent unchanged. If the target answers
**before any byte is streamed** with a 400 that matches:

- OpenAI: error `code` `invalid_encrypted_content` (O4);
- Anthropic: error message matching ``Invalid `signature` in `thinking` block`` (A2/A3);

then the gateway parses the body once, records the hashes of **all** opaque blobs
in it (`encrypted_content` of reasoning items; `signature` of thinking blocks and
`data` of `redacted_thinking` blocks) in the session memo, removes those items
(applying the empty-turn rule), and sends the request once more. Its answer,
success or error, goes to the client. A non-matching error is relayed as today.
The recorded set is safe because the first failing request after a switch only
carries blobs from before the switch.

### Step 3 — per-session memo for follow-up requests

- Key: client session id (`session_id` for Codex, `x-claude-code-session-id` for
  Claude Code, the extractor the routing affinity already uses) + target account
  id + endpoint. Value: set of blob hashes, each with a last-matched time.
- On every passthrough request: a map lookup. Only when the session has an entry
  for this target is the body parsed, and only items whose blob hash is in the set
  are removed. New blobs from the new target have other hashes and stay.
- A → B → A needs nothing special: A never rejected its own blobs.
- Concurrent requests of one session are harmless: the set only grows.
- A request without a session id gets steps 1 and 2 but no memo (it retries
  per request).

### Cleanup

- A hash is dropped once it has not been matched for a grace period (proposed:
  1 h); an entry is dropped with its last hash. This handles context compaction,
  after which the old blobs never reappear.
- Not deleted on the first request without a match: Claude Code subagents share a
  session id but carry their own history, so a hash missing from one subagent's
  request can still be in the main thread's.
- Hard TTL (proposed: 24 h) and an overall size cap with LRU eviction bound the
  memory. Losing the memo (restart, other instance) costs at most one more retry.

### Cost on requests that need nothing

- Step 1 is gated by one byte scan for the trigger patterns (e.g.
  `"reasoning_text"`, `"encrypted_content":""`/`null`, an empty `"signature"`,
  and for local targets `"encrypted_content"`). Measured on a 1 MB Codex-like body:
  ~0.8 ms for a three-pattern scan without a hit vs. ~2.4 ms for a full parse;
  one combined pass is cheaper still. `rewriteModelField` already parses every
  passthrough body today. A false positive costs one parse, never a wrong result.
- Step 2 costs nothing until a target rejects; the body is already in memory.
- Step 3 costs one map lookup.

## Where it lives

- New file(s) in `internal/gateway`, e.g. `switch_history.go`: the pure step-1
  function, the blob-hash extraction/removal, the error matcher, and the memo
  (bounded, TTL, injectable clock for tests).
- `proxyNative` calls step 1 + 3 after `rewriteModelField` (JSON relays only) and
  wraps the upstream call for step 2. The retry happens before any response byte
  reaches the client; usage is recorded once, for the request that was answered.
- The first fix (`sanitizeResponsesReasoning` in
  `internal/provider/openai_responses.go`) moves into step 1 and is removed from
  the provider client, its tests re-expressed as table cases.

## Verification

- Table-driven unit tests per rule row and per probe row above, byte-identity for
  every no-op shape, the empty-turn merge, the error matcher against the recorded
  error bodies, and the memo (hit, miss, grace-period cleanup, TTL, cap) with a
  fake clock.
- `proxyNative` integration tests with a stub upstream: step 1 per target class;
  step 2 (stub answers the recorded 400 once, then 200; a non-matching 400 is not
  retried; a second failure is relayed); step 3 (follow-up filtered without a
  failed attempt).
- Full backend suite (`GOTOOLCHAIN=go1.26.5`), `golangci-lint`, archtest with
  `-count=1`, Sonar via native `sonar-scanner` + `sonar-branch-findings`.
- After deploy: repeat O2 → switch scenario end to end with Codex.

## Documentation

- `compatibility-and-inference.md`: §3.2/§3.3 (the rule and the retry), §5 and §6
  (the passthrough is verbatim except `model` and this sanitiser/retry), with the
  probe table.
- `external-vendor-accounts.md` §4.1/§4.2: "verbatim" statements get the exception.
- New ADR in `09-architecture-decisions.md`: drop-not-convert, shape rules +
  optimistic retry + per-session memo, the rejected tracking variant, limits.

## Left to the builder

File and function names, log wording, the exact byte-scan patterns, hash
function (any fast collision-resistant one), memo sizes (grace period, TTL, cap
as proposed above unless measurements argue otherwise), test fixture shapes.
