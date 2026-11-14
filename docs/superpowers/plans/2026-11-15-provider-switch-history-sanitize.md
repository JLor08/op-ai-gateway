# Provider-Switch History Sanitizing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep Codex / Claude Code sessions working when the target changes mid-session, by removing reasoning/thinking items the new target cannot read on the native passthrough.

**Architecture:** One new gateway-level unit (`internal/gateway/switch_history*.go`) with three parts: deterministic shape rules (stateless, gated by a byte scan), an optimistic single retry on two recognised 400s, and a per-session memo of rejected blob hashes with cleanup. `proxyNative` calls it next to `rewriteModelField`; the first fix inside the OpenAI subscription provider client is moved here.

**Tech Stack:** Go (module `op-ai-gateway`, `gateway/backend`), `encoding/json` with `json.RawMessage` / `UseNumber`, `crypto/sha256`, standard `testing` + `httptest`.

**Spec:** `docs/superpowers/specs/2026-11-15-provider-switch-history-sanitize-design.md` (read it first; the probe table there is the source of every fixture error body).

## Global Constraints

- Applies only to a JSON native passthrough whose `pfReq.APIFlavor` is `openai_responses` or `anthropic_messages`.
- A body that needs no change is returned as the **same slice** (byte-identical); edited bodies are value-lossless (`json.RawMessage`, numbers untouched).
- The payload capture keeps recording `rel.raw` (the client's bytes); nothing here writes to `rel.raw`.
- Retry at most once per client request, only before any response byte is written, only on: OpenAI error `code` `invalid_encrypted_content`; Anthropic error message containing ``Invalid `signature` in `thinking` block``. Never on 429/5xx/other 400s.
- Memo key: client session id (`extractClientSession(...).ClientSession`) + `Target.VendorAccountID` (or `ServerID`+`InstanceKey` for local) + flavor. No session id ⇒ no memo.
- Memo limits (spec proposals): grace period 1 h since a hash was last matched, hard TTL 24 h per entry, cap 10 000 entries with LRU eviction.
- Every removal/retry logs one `slog.Debug` line with flavor, target class and counts; never the blobs.
- Run Go commands with `GOTOOLCHAIN=go1.26.5` (Go 1.27 trips the issue-387 compile guard in `internal/gateway`).
- No `docs/superpowers/**` in the final PR diff.

## Review Focus

1. **A ChatGPT session that never switched** (only own reasoning items, `content: []`, valid `encrypted_content`) must be forwarded byte-identical — Task 1 test `TestSanitizeSwitchHistoryOwnChatGPTSessionUntouched`.
2. **Assistant turn emptied by removal** (Anthropic turn holding only an unsigned thinking block) — removed and neighbouring user turns merged — Task 1 test `TestSanitizeSwitchHistoryMergesUsersAfterEmptiedTurn`.
3. **A non-matching 400 or a 429** must not be retried and must reach the client unchanged — Task 4 test `TestProxyNativeDoesNotRetryUnrelatedErrors`.
4. **Second failure after the filtered retry** is relayed as-is, no loop — Task 4 test `TestProxyNativeRelaysSecondFailureWithoutLooping`.
5. **Claude Code subagent sharing a session id** with a history lacking the memo's hashes must not clear them on that request — Task 3 test `TestRejectedBlobMemoKeepsUnmatchedHashesUntilGrace`.

---

### Task 1: Deterministic shape rules

**Files:**
- Create: `gateway/backend/internal/gateway/switch_history.go`
- Create: `gateway/backend/internal/gateway/switch_history_test.go`
- Modify: `gateway/backend/internal/inference/types.go` (add `const APIFlavorAnthropicMessages = "anthropic_messages"` next to `APIFlavorOpenAIResponses`)

**Interfaces:**
- Produces:
  - `type switchTargetClass int` with `switchClassNone`, `switchClassOpenAIVendor`, `switchClassAnthropicVendor`, `switchClassLocal`
  - `func switchTargetClassOf(t routing.Target) switchTargetClass` — `vendor_openai_subscription`/`vendor_openai` → OpenAI; `vendor_anthropic` → Anthropic; `vendor_gemini` → None; every non-vendor provider → Local
  - `func sanitizeSwitchHistory(raw []byte, flavor string, class switchTargetClass) (out []byte, dropped, stripped int)`

- [ ] **Step 1: Write the failing table test** `TestSanitizeSwitchHistoryRules` with one case per row of the spec's step-1 table, fixtures from the probe table:
  - OpenAI class: minted item (`reasoning_text`, `encrypted_content:""`) dropped; `null`/missing/non-string enc dropped; own item with non-empty `content` → `content` key removed, `stripped==1`; own item with `content:[]` untouched.
  - Local class, responses: encrypted-only item (no text in `content`/`summary`) dropped; item with text and `encrypted_content` → `encrypted_content` removed; item with text only untouched.
  - Anthropic class: `thinking` with `signature:""`/missing dropped; signed `thinking` and `redacted_thinking` kept.
  - Local class, messages: nothing changes.
  - Assert `dropped`/`stripped` counts and the decoded `input`/`messages` item types.
- [ ] **Step 2: Add** `TestSanitizeSwitchHistoryNoOpReturnsSameSlice` (string `input`, missing, `null`, non-JSON, `{"seed":12345678901234567890,...}`, other flavor, `switchClassNone`): assert `&out[0] == &raw[0]` and `dropped+stripped==0`; `TestSanitizeSwitchHistoryOwnChatGPTSessionUntouched` (Review Focus 1); `TestSanitizeSwitchHistoryMergesUsersAfterEmptiedTurn` (Review Focus 2: user, assistant[unsigned thinking], user → one user turn whose content is both users' blocks in order; string content becomes a `text` block).
- [ ] **Step 3: Run** `GOTOOLCHAIN=go1.26.5 go test ./internal/gateway -run SanitizeSwitchHistory` — expect FAIL (undefined).
- [ ] **Step 4: Implement** in `switch_history.go`. Gate first with one `bytes.Contains`-style pass over trigger patterns (`"reasoning_text"`, `"encrypted_content":""`, `"encrypted_content":null`, `"signature":""`, and for Local+responses `"encrypted_content"`, `"type":"reasoning"`); no hit ⇒ return `raw`. Then decode top level as `map[string]json.RawMessage`, edit only the items that change, re-marshal only when something changed.
- [ ] **Step 5: Run** the same command — expect PASS.
- [ ] **Step 6: Commit** `feat(gateway): shape rules for foreign reasoning in replayed history`.

### Task 2: Blob hashes, blob removal, refusal matcher

**Files:**
- Modify: `gateway/backend/internal/gateway/switch_history.go`
- Modify: `gateway/backend/internal/gateway/switch_history_test.go`

**Interfaces:**
- Consumes: Task 1's JSON item helpers and the empty-turn merge.
- Produces:
  - `type blobHash [32]byte` (SHA-256 of the blob string)
  - `func opaqueBlobHashes(raw []byte, flavor string) []blobHash` — responses: `encrypted_content` of `reasoning` items; messages: `signature` of `thinking`, `data` of `redacted_thinking` (assistant turns)
  - `func removeBlobs(raw []byte, flavor string, drop map[blobHash]struct{}) (out []byte, removed int)` — same-slice on no match, empty-turn merge applies
  - `func isForeignReasoningRefusal(status int, body []byte) bool`

- [ ] **Step 1: Write failing tests** `TestOpaqueBlobHashes`, `TestRemoveBlobsDropsOnlyListedBlobs` (new-target blob with another hash survives), `TestIsForeignReasoningRefusal` with the exact probe bodies: O4 body → true; A2/A3 body → true; O2 `array_above_max_length` → false (step 1 handles it); 429, 500, a 400 `invalid_request_error` about `max_tokens` → false.
- [ ] **Step 2: Run** `GOTOOLCHAIN=go1.26.5 go test ./internal/gateway -run 'OpaqueBlob|RemoveBlobs|ForeignReasoningRefusal'` — FAIL.
- [ ] **Step 3: Implement** the three functions; the matcher requires status 400 and reads JSON `error.code` (OpenAI) or `error.message` (Anthropic) — no substring match on the raw body.
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit** `feat(gateway): opaque blob hashing and foreign-reasoning refusal matcher`.

### Task 3: Rejected-blob memo

**Files:**
- Create: `gateway/backend/internal/gateway/switch_history_memo.go`
- Create: `gateway/backend/internal/gateway/switch_history_memo_test.go`

**Interfaces:**
- Consumes: `blobHash` (Task 2).
- Produces:
  - `type rejectedBlobKey struct{ Session, Target, Flavor string }`
  - `type rejectedBlobMemo struct` (mutex, map, LRU list, `now func() time.Time`)
  - `func newRejectedBlobMemo(now func() time.Time) *rejectedBlobMemo` with constants `rejectedBlobGrace = time.Hour`, `rejectedBlobTTL = 24 * time.Hour`, `rejectedBlobCap = 10000`
  - `func (m *rejectedBlobMemo) Record(k rejectedBlobKey, hashes []blobHash)`
  - `func (m *rejectedBlobMemo) Has(k rejectedBlobKey) bool` — O(1), no parse, used as the gate
  - `func (m *rejectedBlobMemo) Match(k rejectedBlobKey, present []blobHash) map[blobHash]struct{}` — returns the stored hashes present in this request and stamps them last-matched; prunes hashes past grace and entries past TTL

- [ ] **Step 1: Write failing tests** with a fake clock: `TestRejectedBlobMemoRecordAndMatch`; `TestRejectedBlobMemoKeepsUnmatchedHashesUntilGrace` (Review Focus 5: a `Match` with none present at +10 min keeps them; at +61 min without a match they are gone and `Has` is false); `TestRejectedBlobMemoTTL` (+24 h 1 min ⇒ gone even if matched); `TestRejectedBlobMemoCapEvictsLeastRecent` (use a small cap via an unexported field); `TestRejectedBlobMemoConcurrent` (`go test -race`).
- [ ] **Step 2: Run** `GOTOOLCHAIN=go1.26.5 go test -race ./internal/gateway -run RejectedBlobMemo` — FAIL.
- [ ] **Step 3: Implement**; `Record` on an existing key unions the sets (the set only grows).
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit** `feat(gateway): bounded per-session memo of rejected reasoning blobs`.

### Task 4: Wire into `proxyNative`, move the first fix

**Files:**
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` (`nativeRelay` struct ~L1251; edit block after `rewriteModelField` ~L674; after `peekStreamingRefusal` block ~L846)
- Modify: `gateway/backend/internal/gateway/server.go` (`Server` field `switchMemo *rejectedBlobMemo`, set in `New` with `time.Now`)
- Modify: `gateway/backend/internal/provider/openai_responses.go` (delete `sanitizeResponsesReasoning`, `ProxyNative` back to plain `doNativeProxy`)
- Modify: `gateway/backend/internal/provider/openai_responses_test.go` (delete the two sanitizer tests; their cases already live in Task 1)
- Create: `gateway/backend/internal/gateway/switch_history_proxy_test.go`

**Interfaces:**
- Consumes: everything above; `extractClientSession` result `si` (already computed in `proxyNative`), `peekStreamingRefusal`'s peek-and-replay pattern (`replayedBody`).
- Produces: `nativeRelay.foreignRetried bool` (set on the retry so there is exactly one); `func peekForeignReasoningRefusal(resp *provider.ProxyResponse) bool` (bounded peek, body replayed).

- [ ] **Step 1: Write failing integration tests** (use `newNativeProxyTestServer` + a scripted proxy provider recording every body it receives and answering from a queue):
  - `TestProxyNativeStripsMintedReasoningBeforeSend` — Responses body with a minted item to a local target: provider receives it without the item, capture still holds the client body.
  - `TestProxyNativeRetriesOnceOnForeignReasoningRefusal` — first answer O4 400, second 200: two provider calls, second body has no `reasoning` item, client gets 200, exactly one usage row.
  - `TestProxyNativeFollowUpFilteredWithoutFailedAttempt` — same session header (`session_id`), next request carries the old blob plus a new one: exactly one provider call, old blob removed, new blob kept.
  - `TestProxyNativeDoesNotRetryUnrelatedErrors` (Review Focus 3: a 400 `max_tokens` body and a 429 → one call, body relayed unchanged).
  - `TestProxyNativeRelaysSecondFailureWithoutLooping` (Review Focus 4: two O4 answers → two calls, client gets the second 400).
  - `TestProxyNativeNoSessionIDRetriesEachTime` — no session header: two requests ⇒ four calls.
- [ ] **Step 2: Run** `GOTOOLCHAIN=go1.26.5 go test ./internal/gateway -run 'ProxyNative(Strips|Retries|FollowUp|DoesNotRetry|RelaysSecond|NoSessionID)'` — FAIL.
- [ ] **Step 3: Implement**: after `rewriteModelField` (JSON relays of the two flavors only) apply `sanitizeSwitchHistory`, then — if `s.switchMemo.Has(key)` — `removeBlobs(upstreamBody, flavor, s.switchMemo.Match(key, opaqueBlobHashes(...)))`. After the upstream call, beside the audio retry: if `!rel.foreignRetried && peekForeignReasoningRefusal(resp)`, record `opaqueBlobHashes(upstreamBody)` under the key (when a session id exists), build the filtered body with `removeBlobs`, release resources exactly like the audio retry, set `rel.foreignRetried = true`, `rel.upstreamBody = filtered`, and call `s.proxyNative(w, r, rel)`. If the filtered body equals the sent body, do not retry.
- [ ] **Step 4: Delete** the provider-side fix and its two tests.
- [ ] **Step 5: Run** `GOTOOLCHAIN=go1.26.5 go test -count=1 ./internal/gateway ./internal/provider ./internal/archtest` — PASS.
- [ ] **Step 6: Commit** `feat(gateway): sanitize and retry foreign reasoning on the native passthrough`.

### Task 5: Documentation, ADR, full verification

**Files:**
- Modify: `docs/architecture/cross-cutting/compatibility-and-inference.md` (§3.2: replace the interim "Foreign reasoning on the ChatGPT-subscription passthrough" bullet; §3.3 thinking paragraph; §5 table "byte-for-byte" cells; §6 passthrough bullet "with only its `model` field rewritten"; add the spec's probe table under §6)
- Modify: `docs/architecture/cross-cutting/external-vendor-accounts.md` (§4.1 "Only `model` is rewritten" / "The body is verbatim except two edits"; §4.2 subscription and api-key passthrough bullets)
- Modify: `docs/architecture/09-architecture-decisions.md` (new ADR at the end: drop-not-convert, shape rules + optimistic retry + memo, rejected tracking variant, VERIFY-LIVE rows)
- Delete before the PR: `docs/superpowers/` (all of it on this branch)

- [ ] **Step 1: Write** the doc changes; every claim matches the code (rules, error matchers, memo limits) and names the VERIFY-LIVE rows.
- [ ] **Step 2: Run** `make lint` from the repo root — expect `check-docs: OK` and 0 lint issues (use `GOTOOLCHAIN=go1.26.5` for golangci-lint).
- [ ] **Step 3: Run** `GOTOOLCHAIN=go1.26.5 go test -count=1 ./...` in `gateway/backend` — all ok.
- [ ] **Step 4: Run** the Sonar gate natively: `SONAR_SCANNER_MODE=native make sonar-gate`, then `make sonar-findings && make sonar-branch-findings` — no branch-attributed findings (or each one fixed).
- [ ] **Step 5: Remove** `docs/superpowers/`, verify `git diff --name-only origin/main...HEAD` lists no `docs/superpowers/` path.
- [ ] **Step 6: Commit** `docs: provider-switch history sanitizing (architecture, ADR)`.
