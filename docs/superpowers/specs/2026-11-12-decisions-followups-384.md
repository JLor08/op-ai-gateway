## Summary

These are follow-ups from the reviews of #373 Phases 1–4 (#376, #378, #379, #383). Each one was deferred on purpose and none is security-relevant: in every case the gateway refuses rather than widening access. They are collected here so they don't get lost. File references are at `main` @ `f35484f1`.

Live verification of #373 (d3 on vLLM, a Qwen3 passthrough, vllm-sr, `gpt-6-luna` on an OpenAI api-key account, the official SDK) is **not** in scope here; it stays with #373. Emulation is #380.

## Code follow-ups

1. **Refusal rows after resolve record an empty target.**
   - `recordDecisionsRefusal` (`gateway/backend/internal/gateway/systemone_handler.go` ~:193) passes `routing.Target{}` to `recordUsage`.
   - That covers a refusal made after a target was resolved: `systemone.endpoint_disabled` / `decisions.endpoint_disabled`, the post-resolve `routing.model_not_capable`, and a translator refusal. The Activity view then cannot show which server or route refused the request.
   - The responses/messages endpoint-disabled refusal records the resolved target (`native_passthrough.go`, `relayPassthroughTarget`).
   - Pass the resolved target where one exists.
2. **No re-selection after the post-resolve route check.**
   - The pre-resolve candidate filter is route-aware only for ordinary applications. A `server_agent` candidate's spec is unknown before resolve.
   - So a `server_agent` mapping whose spec has no route answers 404 `routing.model_not_capable`, even when another mapping for the same name could serve.
   - ADR-098/100 document this. Fixing it would mean excluding the instance and re-resolving, like `relayAgain`.
3. **The redirect hint is not route-aware for ordinary mappings.**
   - `capableNames` (`internal/portal/service_model_offering.go` ~:388) judges `any_decision` by verdict and kind, not by the effective route. A forced mode without its verdict is the case it misses.
   - The unknown-model redirect can therefore suggest a model that the resolver then refuses.
   - Align it with the resolver's route-aware candidate filter (`decisionRouteServes`).
4. **A group whose members all lack a route answers `routing.no_model_route`, not `routing.model_not_capable`.**
   - Under `fallback_only`, that can fall through to a same-named vendor model on a name collision.
   - This matches Phase 1 behavior, but it is worth deciding explicitly.
5. **The vllm-sr supported major is hard-coded in the portal.**
   - The text "(5)" appears in `gateway/frontend/src/i18n.ts` (~:2048, ~:5590, ~:6003). It duplicates `routing.VllmSRSupportedAPIMajor` and will drift on the next major.
   - Fix: expose the constant (for example on the model-servers row) and interpolate it.
6. **A score probability `value` of `1.0` is rejected.**
   - The reverse translator parses the score index with `strconv.Atoi(probKeyString(raw))` (`internal/compat/decisions_reverse.go` ~:733), so a float-encoded integer fails the translation.
   - OpenAI documents `value` as an int, so this is low risk. Fix: parse as a number and require it to be integral.
7. **The two decision probes run sequentially on an unreachable host.**
   - The System One and `decision_openai` probes each use `systemOneProbeTimeout = 30 s` (`cmd/gateway/app_health.go` ~:69). On a blackholed host, a mapping holds its detached pass for up to 60 s.
   - This is harmless, because the pass is detached and guarded per application, but it is undocumented.

## Cosmetic / test hygiene

- **Leftover effort-test names in the Phase 2 token tests.** The `decisions_confidence_normalize` tests reuse names and comments from the effort tests: `cec@example.test` (`internal/store/conformance_test.go` ~:634) and `"effort-off"` (`internal/portal/service_token_test.go` ~:709, ~:819, ~:881).
- **Helper and constant names still say System One** although they now serve both probes: `logSystemOneNoVerdict`, `systemOneRetryBackoff` and `systemOneRefusedBackoffCap` (`cmd/gateway/app_health.go`).
- **Stale doc comment on `ModelDTO.DecisionOpenAI`** (`internal/portal/service.go` ~:1483). It calls the field "Decision's twin", although `Decision` now also folds the System One route.
- **Test gaps:**
  - 502 paths that don't assert their usage row;
  - no mid-read-break test for the buffered decisions path;
  - multi-key refusal in the reverse translator;
  - raw answer order when the upstream reorders its answers;
  - the warning tested only at helper level (`api_flavors_text_on_vllm_sr`).
- **Normalizing passthrough changes `Content-Type`.** A normalizing passthrough sets `Content-Type: application/json` even when the body is left unchanged.

## Further cosmetic / robustness items (checked against `main` @ `f35484f1`)

- **Empty choice value rejected.** `internal/compat/decisions.go` ~:699 uses the empty string as its "no choice" sentinel, so the legal choice value `""` fails the translation with "choice answer names no choice". Use a presence flag, or refuse `""` on the request side as `decisions.question_invalid`.
- **Misleading translator messages:**
  - An `input_text` instruction part without `text` reports "question instruction parts of type \"input_text\" are not supported" (`decisions.go` ~:434-435).
  - A choice whose `description` is not a string reports "choice N is not an object" (~:464).
- **Empty input becomes an empty state.** `input: []`, or a user message with empty content, reaches the upstream as `state: ""` instead of being refused (`decodeDecisionsInput` / `decodeDecisionsItems`, `decisions.go` ~:168ff).
- **Refusal reported as an upstream error.** 422 `systemone.refused` is sent with error `type` `upstream_error` (`internal/gateway/decisions_response.go` ~:25), although it is a model refusal, not an upstream failure.
- **Three near-identical portal folds.** `decisionFlagsByName`, `decisionOpenAIFlagsByName` and `anyDecisionFlagsByName` (`internal/portal/service.go` ~:3494-3560) walk the same views with the same loop shape; consolidate them.
- **Only the first mode error is reported.** `refuseIncapableDecisionModes` (`internal/portal/service_applications.go` ~:2114) returns only the `decisions_mode` error when both `decisions_mode: passthrough` and `systemone_mode: translate` are invalid for the kind.
- **Re-added row is outside the row cap.** The decision-model row re-added on an OpenAI api-key refresh sits outside `maxDiscoveredModels` (`internal/portal/service_vendor_discovery.go` ~:87), so a full 1000-row listing stores 1001 rows. This is harmless, but it is undocumented.
- **`foldNoulCriteria` has no complexity headroom.** It (`internal/compat/decisions_reverse.go` ~:393) is at cognitive complexity 15 by the reviewers' hand count, the Sonar limit. Any further branch needs a split.
- **Underflowing numbers count as default.** `isDefaultJSON` (`decisions_reverse.go` ~:100ff) treats an underflowing number such as `priority: 1e-400` as `0`, so it counts as a default value.
- **529 test does not use the 5.0 envelope.** The 529 case in `TestDecisionsTranslateUpstreamErrorsAreNormalized` (`internal/gateway/decisions_handler_test.go` ~:547) uses an `{error:{…}}` body, not vllm-sr 5.0's flat `{detail, code}` envelope. Add a flat-envelope 529 case.
- **Unused frontend field.** The `decision_openai` field on the model type (`gateway/frontend/src/api/models.ts` ~:615) is declared but never read. The mapping form and the model-servers chip read the capability rows instead. Use it or drop it.
- **The `gpt-6-luna` note appears only on the Providers page** (`providersIntro` in `i18n.ts`). The decisions-mode note does not mention that an OpenAI api-key account serves both decision endpoints.

## Done when

- Items 1, 3, 5 and 6 are fixed, with tests.
- Items 2, 4 and 7 are either fixed or explicitly confirmed as intended in ADR-100.
- The cosmetic list is cleared.

Refs #373


