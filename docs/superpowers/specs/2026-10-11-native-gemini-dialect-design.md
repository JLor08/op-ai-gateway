# Design — Native Google Gemini dialect client (sub-project C)

Date: 2026-10-11
Branch: `feat/native-gemini-dialect` (off `main` 26cb084)
Status: approved (design conversation), pending written-spec review
Issue: #206. Part of the "more providers" / "Gemini-compatible gateway" arc.

## 1. Goal

A **native** Google Gemini dialect: the gateway speaks Gemini's own
`generateContent` / `streamGenerateContent` wire directly (render the neutral
`inference.Request` → Gemini, parse Gemini → neutral), as a first-class provider
alongside the current OpenAI-compatibility **shim**. Modeled on
`provider.AnthropicClient`.

This is the **outbound** native client (#206) **and** the reusable Gemini↔neutral
translation foundation that the **inbound** Gemini endpoint (#204 — where Antigravity
connects, passthrough/translate) plugs into next. The two share one translation
package.

## 2. Scope decision (from the design conversation)

- **Approach A (confirmed):** native outbound client at the **current** neutral model's
  expressiveness — **no neutral-model surgery**. The exotic Gemini features that the
  neutral model can't carry (structured output, safety settings, non-image media, fine
  thinking control) are **not** forced through translate; they are delivered **losslessly
  by #204's passthrough** (raw-body relay) and/or by the broad-fidelity translate
  extension deferred to **#208**.
- **Why:** the neutral `inference.Request` is a lowest-common-denominator chat model
  (text+image, function tools, tool_choice, max_tokens, temperature, stop,
  `reasoning_effort`). Honoring the exotic features via *translate* would require
  extending it + teaching every provider + every inbound parser — that is #208. The
  user's actual goal (lossless Antigravity → Gemini) is a **passthrough** story (#204),
  which needs none of that.

In scope: the native outbound client, the `geminiwire` package (outbound halves +
shared wire structs), a native-vs-shim selection, native discovery/validation,
registration, capture redaction, labels, docs. Out of scope (→ #204): the inbound
Gemini HTTP endpoint + passthrough flavor/EndpointMode wiring. Out (→ #208): broad
translate fidelity. Out (deferred): grounding/Google Search, code execution.

## 3. Provider kind, vendor id, selection

- New provider-adapter kind **`ProviderVendorGemini = "vendor_gemini"`** (`routing/store.go`,
  after `ProviderVendorOpenAISubscription`). `IsOpenAIVendorProvider` stays false for it
  (no edit — it is OpenAI-subscription-specific).
- **Selection = a distinct vendor id + preset** (not a per-account toggle, not a global
  switch): new **`VendorGoogleNative = "google_native"`** vendor id; the existing shim
  `VendorGoogle = "google"` stays. **No DB migration** — `vendor_accounts.vendor` is free
  text with no CHECK. Users keep a shim and a native account side by side; rollback =
  delete the account.
- `VendorPreset` gains a **`Dialect string`** field (`""` = OpenAI wire, `"gemini"` =
  native). New preset entry `VendorGoogleNative` (DefaultBaseURL
  `https://generativelanguage.googleapis.com`, `PathPrefix "/v1beta"` [portal
  discovery/validation only], `ValidateVia "models"`, `StripModelsPrefix "models/"`,
  `Dialect "gemini"`).
- **Predicate split** (so the registry's two meanings don't collide):
  - `IsPresetVendor(v)` = in the registry → the preset-account semantics (api-key only,
    configurable root, fail-closed on empty BaseURL, discovery-on-create, empty static
    seed). Used at `resolver.go` fail-closed (vendorAccountModelTarget), `CreateVendorAccount`
    (force api_key + base_url normalize + discover-on-create), `vendor_catalog.vendorAPIFlavor`.
  - `IsOpenAICompatibleVendor(v)` = `IsPresetVendor(v) && Dialect==""` → the OpenAI-wire
    resolver branch, the OpenAI-compatible discovery/validate.
  - `IsNativeGeminiVendor(v)` = `Dialect=="gemini"` → checked **first** in the resolver
    branch, discovery, and validation.
- Resolver (`vendorAccountTarget`): add, **before** the generic `IsOpenAICompatibleVendor`
  branch, `else if IsNativeGeminiVendor(acc.Vendor) { provider = ProviderVendorGemini;
  endpoint = acc.BaseURL; tokenHeader = "x-goog-api-key"; pathPrefix stays "" }`. Keep
  `APIFlavors = [openai, anthropic]`, both endpoint modes zero (translate) — preserves
  served-flavors parity with `service_vendor_listing.go`. Order matters: the native test
  must precede the generic one (else `OpenAIPathPrefixFor` would wrongly set the OpenAI
  path prefix). The empty-BaseURL fail-closed in `vendorAccountModelTarget` must use
  `IsPresetVendor` (covers native too).

## 4. The `geminiwire` package (`internal/geminiwire`, leaf, imports only `inference`)

Holds the Gemini wire structs + converters, so both directions live in one place
(round-trip-testable) and #204 reuses it. archtest edits in the same change: add
`"internal/geminiwire": {"internal/inference"}`; add `internal/geminiwire` to
`allowedDeps["internal/provider"]`; add a TestForbiddenEdges sub-test pinning it to
inference only. (It cannot return `provider.Response` — provider imports it — so it
returns a package-local result struct the client converts.)

Exported for C (outbound):
- Wire structs: `GenerateContentRequest`/`Response`, `Content`, `Part` (text | inlineData
  | fileData | functionCall | functionResponse, + `thought`, `thoughtSignature`),
  `Tool`/`FunctionDeclaration`, `ToolConfig`, `GenerationConfig` (+ `thinkingConfig`),
  `SafetySetting` (struct present for #204/round-trip; C doesn't emit it), `Candidate`,
  `UsageMetadata`. camelCase on the wire; the (future) inbound parser accepts snake_case too.
- `RenderRequest(req inference.Request, model string) (GenerateContentRequest, error)` (+
  marshal) — stream is encoded in the URL action by the client, not the body.
- `ParseResponse([]byte) (Result, error)` and `ParseStreamChunk([]byte) (Result, bool, error)`
  where `Result{Text, Reasoning, ToolCalls []inference.ToolCall, Usage inference.Usage,
  FinishReason string}`.
- Reserved for #204 (not built now): `ParseRequest`, `RenderResponse`, `RenderStreamChunk`.

## 5. Translation detail (the render/parse rules)

- **System:** join `system`/`developer` message text with `\n\n` → `systemInstruction`
  (text-only; precedent `anthropicMessages`).
- **Contents:** `user`/`assistant`→`user`/`model`; merge consecutive same-role turns;
  drop empty turns. `tool` messages → a `user` turn of `functionResponse` parts (merge
  parallel results into one turn).
- **Multimodal:** `ContentImage` data-URI → `inlineData{mimeType,data}` (mime from the
  data-URI); an http(s) image URL → `fileData` (best-effort; VERIFY-LIVE whether external
  URIs are accepted — else leave as text/skip). Non-image media is out of scope (neutral
  has no slot).
- **Tools:** `Tool{Name,Description,Parameters}` → `functionDeclarations[].parametersJsonSchema`
  (full JSON Schema; VERIFY-LIVE per model, fall back to `parameters` OpenAPI-subset if a
  model rejects it). `tool_choice` → `toolConfig.functionCallingConfig` (auto→AUTO,
  none→NONE, required→ANY, {name}→ANY+allowedFunctionNames).
- **generationConfig:** `temperature`, `maxOutputTokens` (= MaxTokens), `stopSequences`
  (= Stop). **Thinking:** `reasoning_effort` → `thinkingConfig` — budget for 2.5
  (minimal/low→1024, medium→8192, high→24576), level for 3.x (low/medium/high; minimal
  only where allowed) — keyed off a small model-family heuristic on `ProviderModel`.
  Omit sampling the neutral model doesn't carry (topP/topK/seed/penalties/candidateCount);
  **do not** send a trailing `role:model` turn (Gemini 400s — only an issue for prefill,
  rare in translate).
- **thoughtSignature + function name via the tool-call id (critical):** Gemini-3 function
  calling 400s without `thoughtSignature` echoed back, and `functionResponse` needs the
  function **name** — neither has a neutral slot. Encode both into `ToolCall.ID` (a small
  reversible scheme, e.g. a prefixed base64 of `{name, sig}`), which every dialect echoes
  verbatim (`tool_call_id`/`call_id`/`tool_use_id`): the renderer decodes the id to restore
  `functionCall.id` + `thoughtSignature` on the model turn and the `name` on the
  `functionResponse`. Cross-provider history with no signature: VERIFY-LIVE the
  `skip_thought_signature_validator` fallback vs failing loud. Wrap non-object tool text as
  `{"result": text}`.
- **Response parse:** `candidates[0].content.parts` → text parts (thought!=true) = `Text`,
  thought==true = `Reasoning`, `functionCall` = `ToolCalls` (Arguments = compact JSON,
  "{}" when absent; ID = encode the part id + its `thoughtSignature`). **FinishReason:**
  STOP→"stop" (→"tool_calls" if any functionCall part), MAX_TOKENS→"length",
  SAFETY/RECITATION/BLOCKLIST/PROHIBITED_CONTENT/SPII/LANGUAGE/IMAGE_*→"content_filter",
  MALFORMED_FUNCTION_CALL/MISSING_THOUGHT_SIGNATURE/OTHER→"stop". A prompt-blocked response
  (HTTP 200, no candidates, `promptFeedback.blockReason`) → empty Text + "content_filter"
  (parser must not assume `candidates[0]`).
- **Usage:** InputTokens = `promptTokenCount` (already includes cache), CachedTokens =
  `cachedContentTokenCount`, OutputTokens = `candidatesTokenCount + thoughtsTokenCount`
  (OpenAI convention: output includes reasoning; keeps input+cached+output==total),
  TotalTokens = `totalTokenCount`. No neutral reasoning-token field (folds into output).

## 6. The native client (`internal/provider/gemini.go`)

`type GeminiClient struct{ http *http.Client }`; `NewGeminiClient(httpClient)` (nil →
http.DefaultClient); `var _ Client, _ StreamingClient, _ NativeProxyClient`. API-version
is a client const (`geminiAPIVersion = "/v1beta"`), like Anthropic's path const.

- **Complete:** apply `target.Timeout`; `body = geminiwire.RenderRequest(req,
  providerModel(target,req))` (TrimPrefix `models/` defensively, `url.PathEscape` the model);
  POST `endpointURL(target.Endpoint, "/v1beta/models/"+model+":generateContent")`;
  Content-Type + `applyUpstreamAuth` (sends x-goog-api-key) + capture RecordRequest; map
  errors (ctx deadline→ErrTimeout, transport→ErrUnavailable, non-2xx→`unavailableStatus`
  **with a bounded body snippet** for 400 INVALID_ARGUMENT diagnostics); `WriteResponse` to
  the sink; `geminiwire.ParseResponse` → `provider.Response`.
- **CompleteStream:** no timeout; POST `:streamGenerateContent?alt=sse`; tee into
  `CaptureSinkFrom(ctx).ResponseWriter()`; scan SSE (`data:` lines, blank-line delimited,
  CRLF-tolerant, **no `[DONE]`**); each chunk → `geminiwire.ParseStreamChunk`; emit text/
  reasoning deltas; buffer whole tool calls and emit before the terminal; derive completion
  from a chunk carrying `finishReason`; take the **last** (cumulative) `usageMetadata`;
  a mid-stream `{"error":{...}}` line or EOF-without-finishReason → `ErrUnavailable`.
- **ProxyNative** (implemented now for #204 reuse; cheap): `doNativeProxy` to the
  caller-composed path (may carry `:streamGenerateContent?alt=sse`).
- **Redirect safety:** use `withoutRedirects(c.http)` on the translate path too (net/http
  does not strip the custom `x-goog-api-key` on a cross-host redirect; the account root is
  owner-supplied). No `ModelLister`/`Prober` (vendor discovery is in the portal).
- **main.go `providerClients`:** add `routing.ProviderVendorGemini: provider.NewGeminiClient(appHTTPClient)`.

## 7. Discovery, validation, capture, labels, frontend

- **Capture redaction (SECURITY):** add `"x-goog-api-key"` to `redactedCaptureHeaders`
  (`gateway/capture.go`) + extend its test — otherwise a native key is stored in clear on
  payload capture.
- **Native discovery:** `vendorauth.DiscoverGeminiModels(ctx, http, modelsURL, apiKey)` —
  header `x-goog-api-key`, `GET {base}/v1beta/models?pageSize=1000` + follow
  `nextPageToken` (bounded), parse `{models:[{name:"models/…",displayName,
  supportedGenerationMethods}]}`, keep only entries whose methods contain `generateContent`,
  strip `models/`. New portal seam field `GeminiAPIKey` + default; branch in
  `discoverAPIKeyModels` before the OpenAI-compatible branch. (`StripModelsPrefix` on the
  preset still applies to Slug/DisplayName.)
- **Native validate:** `vendorauth.ValidateGeminiAPIKey(ctx, http, probeURL, apiKey)` —
  x-goog-api-key to `{base}/v1beta/models`; uniform classification (VERIFY-LIVE: bad native
  key may be 401 not 400 → still "unverifiable" under the uniform rule, acceptable).
- **Usage:** native preset `UsageVia ""` → no pull (Gemini has no usage API). No change.
- **`native_passthrough.upstreamPath`:** add a `vendor_gemini` case (usage-row label) —
  else it mislabels as `/v1/chat/completions`.
- **Labels/frontend:** add the `google_native` id to the TS vendor union + the preset tuple
  (`vendorPresets.ts`) + `vendorLabel` (de+en i18n: "Google Gemini (native)" / "(nativ)")
  + a `case 'gemini'` in `vendorLabel`/`vendorServerLabel` (the provider kind
  `vendor_gemini` → "gemini"). The create form (base-URL field, api-key-only) works once
  the id is in the tuple.

## 8. Name collisions

A shim `google` account and a native `google_native` account serving the same model names
collide at resolve time (first active account by id wins). Document + advise a distinct
`model_prefix` on one; per-token vendor access already rejects duplicate names for
non-"all" tokens.

## 9. Testing & verification

- `geminiwire` (table/round-trip, pure, no network): render (system lift, role merge,
  functionResponse turn, images, tools→parametersJsonSchema, tool_choice, thinking
  budget-vs-level by model, thoughtSignature+name id round-trip); parse (text/thought/
  functionCall split, finishReason table, usage arithmetic, prompt-block no-candidates);
  SSE chunk parse (cumulative usage keep-last, finishReason completion, mid-stream error,
  no-[DONE]).
- `GeminiClient` (httptest): Complete, CompleteStream, ProxyNative; x-goog-api-key header
  present; timeout on Complete only; redirect not followed; 400 body snippet surfaced.
- Resolver: a `google_native` account → `vendor_gemini` target (Endpoint, x-goog-api-key,
  translate modes, APIFlavors [openai,anthropic]); the shim `google` account unchanged;
  empty-BaseURL fail-closed.
- Portal: native discovery (paging, filter, strip) + validate; create forces api_key +
  discovers on create; the predicate split keeps shim behavior intact.
- `capture` redaction test gains x-goog-api-key; `main_test` wires the kind; `native_passthrough`
  label test gains the case; frontend vitest (vendor list, label, create form) + i18n parity.
- **Gates:** go build/vet/`go test ./...` + golangci; **no Postgres leg** (no schema change);
  frontend gates; `check-docs`; SonarQube branch-findings = 0.

## 10. Docs
New **ADR-054** (native Gemini dialect; supersedes ADR-052(e) "native deferred to C"):
record the provider kind + vendor id + `Dialect` preset selection, `geminiwire` as the
shared translation home, x-goog-api-key + capture redaction, thoughtSignature-via-id,
translate-only (passthrough = #204, broad fidelity = #208). Update
`external-vendor-accounts.md` (§1 vendor table, §4.4 + registry table, §6.1 discovery,
served matrix, §10 VERIFY-LIVE), `api-surface.md` (vendor id lists), `03-context-and-scope.md`.

## 11. VERIFY-LIVE at implementation (no real key in the design session)
- Native rejects an API key sent as `Authorization: Bearer` (use x-goog-api-key only); the
  shim accepts x-goog-api-key or only Bearer.
- Bad native key status (400 INVALID_ARGUMENT vs 401 ACCESS_TOKEN_TYPE_UNSUPPORTED) → the
  validate classification.
- `/v1beta/models` exact shape, default page size, `supportedGenerationMethods` vocabulary.
- `parametersJsonSchema` accepted per model (else fall back to `parameters`).
- Streaming: usageMetadata cumulative + keep-last; functionCall parts whole; mid-stream
  error frame shape; finishReason "" on partials.
- The `thoughtSignature` dummy-string behavior for cross-provider history; which parallel
  functionCall parts carry signatures.
- Per-model quirks (sampling ignored/400 on 3.6+, trailing model turn 400, thinkingLevel vs
  thinkingBudget per family) — drives conservative rendering.
