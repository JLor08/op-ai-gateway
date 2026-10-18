// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  errorLabelByCode,
  formatChatRunErrorCode,
  formatDate,
  formatMetric,
  formatPortalError,
} from './format';
import { PortalApiError } from '../../api';
import { messages } from '../../i18n';
describe('formatDate', () => {
  it('returns the fallback for null/empty/invalid input', () => {
    expect(formatDate(null, 'never')).toBe('never');
    expect(formatDate(undefined, 'never')).toBe('never');
    expect(formatDate('', 'never')).toBe('never');
    expect(formatDate('not-a-date', 'never')).toBe('never');
  });
  it('formats a valid ISO timestamp to a non-raw, non-fallback string', () => {
    const out = formatDate('2026-07-12T07:22:01Z', 'never');
    expect(out).not.toBe('never');
    expect(out).not.toBe('2026-07-12T07:22:01Z');
    expect(out).toContain('2026');
  });
});

describe('formatMetric', () => {
  it('renders a value with the requested number of decimals', () => {
    expect(formatMetric(12.3456, 2)).toBe('12.35');
    expect(formatMetric(8, 2)).toBe('8.00');
    // Energy per token is a very small number and needs the long tail.
    expect(formatMetric(0.0000001234, 10)).toBe('0.0000001234');
  });

  it('renders whole numbers without a decimal point at zero decimals', () => {
    expect(formatMetric(1500, 0)).toBe('1500');
  });

  it('renders the em-dash placeholder for missing or zero values', () => {
    // The backend reports "not measured" as 0, which this list has always shown
    // as the placeholder rather than a misleading 0.00.
    expect(formatMetric(0, 2)).toBe('—');
    expect(formatMetric(null, 2)).toBe('—');
    expect(formatMetric(undefined, 2)).toBe('—');
  });
});

/**
 * `errorLabelByCode` had no direct test at all — 96 hand-maintained entries
 * mapping a backend error sentinel to a portal label, grown by every task that
 * touched a new endpoint. Task 19's twelve additions were deferred for exactly
 * that reason: covering only the new ones would have made the convention
 * inconsistent in the other direction.
 *
 * What is covered here is the WHOLE map, and deliberately NOT as a 96-row
 * table of expected labels. Such a table is a second copy of the same data:
 * whoever edits the map edits the table the same way, in the same commit, so it
 * catches nothing while making every future entry cost two edits. What it
 * cannot catch is precisely the realistic defect — a new entry pointed at the
 * neighbouring label by copy-paste.
 *
 * The invariants below do catch that (a reused label is a duplicate, and there
 * is exactly one documented duplicate pair), they cover entries that do not
 * exist yet, and each of them can genuinely fail. `formatPortalError`'s own
 * branches — mapped code, unmapped code, plain Error, non-Error — are pinned
 * separately, since they are what every call site actually reaches.
 */
describe('formatPortalError', () => {
  it('renders a mapped code as "code: localized label", in both locales', () => {
    const err = new PortalApiError(409, 'mapping.gateway_name_conflict', 'raw server text');
    expect(formatPortalError(err, messages.de)).toBe(
      `mapping.gateway_name_conflict: ${messages.de.errorMappingGatewayNameConflict}`,
    );
    expect(formatPortalError(err, messages.en)).toBe(
      `mapping.gateway_name_conflict: ${messages.en.errorMappingGatewayNameConflict}`,
    );
    // The raw server message is deliberately NOT shown when a label exists.
    expect(formatPortalError(err, messages.de)).not.toContain('raw server text');
  });

  it("falls back to the server's own message for an unmapped code, still naming the code", () => {
    // Forward compatibility: a newer backend sentinel must degrade to whatever
    // the server said, never to an empty string or a misleading label.
    const err = new PortalApiError(500, 'some.future_sentinel', 'something specific broke');
    expect(formatPortalError(err, messages.de)).toBe(
      'some.future_sentinel: something specific broke',
    );
  });

  it('passes a plain Error through by message and stringifies anything else', () => {
    expect(formatPortalError(new Error('network down'), messages.de)).toBe('network down');
    expect(formatPortalError('just a string', messages.de)).toBe('just a string');
    expect(formatPortalError(undefined, messages.de)).toBe('undefined');
  });

  // A proxy's 413 (nginx's own HTML page, no JSON error body) is named
  // request.body_too_large by transport.ts's request(); this pins the label
  // it renders to, in both locales.
  it('names a proxy 413 as request.body_too_large, in both locales', () => {
    const err = new PortalApiError(413, 'request.body_too_large', 'request body too large');
    expect(formatPortalError(err, messages.de)).toContain(messages.de.errorRequestBodyTooLarge);
    expect(formatPortalError(err, messages.en)).toContain(messages.en.errorRequestBodyTooLarge);
  });

  // A benchmark starter's 500 (writeBenchmarkError's fallback) renders its own
  // label, not the server's bare "benchmark request failed".
  it('labels a benchmark starter 500 as benchmark.request_failed, in both locales', () => {
    const err = new PortalApiError(500, 'benchmark.request_failed', 'benchmark request failed');
    expect(formatPortalError(err, messages.de)).toBe(
      `benchmark.request_failed: ${messages.de.errorBenchmarkRequestFailed}`,
    );
    expect(formatPortalError(err, messages.en)).toBe(
      `benchmark.request_failed: ${messages.en.errorBenchmarkRequestFailed}`,
    );
  });

  // The starters' four refusals before they reserve the server, each with its
  // OWN label: they send the operator to different places (a model that only
  // generates images, one that only generates speech, an agent update, an admin
  // override).
  it.each([
    ['benchmark.images_only', 'errorBenchmarkImagesOnly'],
    ['benchmark.audio_only', 'errorBenchmarkAudioOnly'],
    ['benchmark.agent_ensure_unsupported', 'errorBenchmarkAgentEnsureUnsupported'],
    ['benchmark.spec_force_stopped', 'errorBenchmarkSpecForceStopped'],
  ] as const)('labels the %s refusal with %s, in both locales', (code, key) => {
    const err = new PortalApiError(409, code, 'raw server text');
    expect(formatPortalError(err, messages.de)).toBe(`${code}: ${messages.de[key]}`);
    expect(formatPortalError(err, messages.en)).toBe(`${code}: ${messages.en[key]}`);
  });

  // The refusals the vendor model discovery adds: a malformed model prefix, a
  // stored credential the models refresh cannot open, the models and the usage
  // refresh's own 500s and a malformed Codex client_version. Each renders its localized label, never the
  // server's English text, in both locales.
  it.each([
    [400, 'vendor_account.model_prefix_invalid', 'errorVendorAccountModelPrefixInvalid'],
    [400, 'vendor_account.base_url_required', 'errorVendorAccountBaseUrlRequired'],
    [400, 'vendor_account.base_url_invalid', 'errorVendorAccountBaseUrlInvalid'],
    [409, 'vendor_account.credential_unreadable', 'errorVendorAccountCredentialUnreadable'],
    [500, 'vendor_account.refresh_failed', 'errorVendorAccountRefreshFailed'],
    [500, 'vendor_account.usage_refresh_failed', 'errorVendorAccountUsageRefreshFailed'],
    [
      400,
      'system.vendor_openai_codex_client_version_invalid',
      'errorSystemVendorOpenAICodexClientVersionInvalid',
    ],
  ] as const)('labels %i %s with %s, in both locales', (status, code, key) => {
    const err = new PortalApiError(status, code, 'raw server text');
    for (const locale of ['de', 'en'] as const) {
      const label = messages[locale][key];
      expect(label.length).toBeGreaterThan(0);
      expect(formatPortalError(err, messages[locale])).toBe(`${code}: ${label}`);
    }
    expect(messages.de[key]).not.toBe(messages.en[key]);
  });

  // Sharing a vendor account (#183): the share-level and management-group
  // endpoints' refusals, each rendered as its localized label in both locales
  // (not the server's English text), including the two 409 sharing switches an
  // admin meets because the portal does not read them.
  it.each([
    [409, 'vendor_accounts.sharing_disabled', 'errorVendorAccountsSharingDisabled'],
    [
      409,
      'vendor_accounts.subscription_sharing_disabled',
      'errorVendorSubscriptionSharingDisabled',
    ],
    [400, 'vendor_account.share_level_unchanged', 'errorVendorAccountShareLevelUnchanged'],
    [400, 'vendor_account.subscription_share_capped', 'errorVendorAccountSubscriptionShareCapped'],
    [400, 'vendor_account.admin_group_required', 'errorVendorAccountAdminGroupRequired'],
    [400, 'vendor_account.admin_group_invalid', 'errorVendorAccountAdminGroupInvalid'],
    [
      400,
      'vendor_account.admin_group_parent_mismatch',
      'errorVendorAccountAdminGroupParentMismatch',
    ],
    [400, 'vendor_account.root_mismatch', 'errorVendorAccountRootMismatch'],
    [400, 'vendor_account.not_shared', 'errorVendorAccountNotShared'],
    // The resource-group link (both the owner's self-service join and the group
    // manager's add): the account's containment root is not the group's.
    [
      400,
      'resource_group.vendor_account_system_group_mismatch',
      'errorResourceGroupVendorAccountSystemGroupMismatch',
    ],
  ] as const)('labels %i %s with %s, in both locales', (status, code, key) => {
    const err = new PortalApiError(status, code, 'raw server text');
    for (const locale of ['de', 'en'] as const) {
      const label = messages[locale][key];
      expect(label.length).toBeGreaterThan(0);
      expect(formatPortalError(err, messages[locale])).toBe(`${code}: ${label}`);
    }
    expect(messages.de[key]).not.toBe(messages.en[key]);
  });
});

describe('errorLabelByCode (whole-map invariants)', () => {
  const entries = Object.entries(errorLabelByCode) as [string, keyof typeof messages.de][];

  it('is not empty (guards against the map itself being emptied or renamed away)', () => {
    expect(entries.length).toBeGreaterThan(50);
  });

  it('resolves every entry to a non-empty label in de AND en', () => {
    for (const [code, key] of entries) {
      const de = messages.de[key];
      const en = messages.en[key];
      expect(typeof de, `${code} -> ${String(key)} (de)`).toBe('string');
      expect(typeof en, `${code} -> ${String(key)} (en)`).toBe('string');
      expect(String(de).length, `${code} -> ${String(key)} (de) is empty`).toBeGreaterThan(0);
      expect(String(en).length, `${code} -> ${String(key)} (en) is empty`).toBeGreaterThan(0);
    }
  });

  it('keys every entry by the backend sentinel convention, dotted snake_case', () => {
    // The codes are read verbatim from the Go sentinels (e.g.
    // portal/service_runtime.go) and from the errRow tables in the gateway
    // endpoint files. A camelCased or spaced key here silently never matches.
    for (const [code] of entries) {
      expect(code, `${code} is not a dotted snake_case sentinel`).toMatch(
        /^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$/,
      );
    }
  });

  it('points every entry at an error* label, never at some other message', () => {
    // A code pointed at a non-error key (a warning, a field name) renders as a
    // plausible sentence in the wrong register and reads as a portal bug.
    for (const [code, key] of entries) {
      expect(String(key), `${code} -> ${String(key)} is not an error* key`).toMatch(/^error[A-Z]/);
    }
  });

  /**
   * The VRAM benchmark's own wire codes, pinned as LITERALS on purpose.
   *
   * The whole-map invariants above cover entries that do not exist yet, but
   * none of them can catch the failure that matters for these six: the code
   * STRING drifting apart from the backend's. A code the map does not carry is
   * not an error the portal reports badly -- `formatPortalError` falls back to
   * the raw English the server sent, so a 409 an operator is meant to act on
   * ("this server is in file mode", "the spec declares GPU 3 and the host has
   * two") arrives untranslated and unexplained, in a portal that is otherwise
   * fully localized. Nothing else in this package names these strings.
   *
   * They are declared in Go as `codeBenchmarkVRAM*`
   * (`internal/gateway/benchmark_vram_isolation.go`,
   * `benchmark_vram_confidence.go`) and as the `ErrRuntimeSpecServerBenchmarking`
   * sentinel's own message, wired to a 409 in the `errRow` table in
   * `portal_runtime_endpoints.go`. Renaming one there means editing this list
   * and the map together; the Go side pins its own literals for the four
   * isolation refusals and the declared-GPU one, so only
   * `runtime_spec.server_benchmarking` has no guard on its half.
   */
  const vramWireCodes = [
    'benchmark.vram_not_agent_managed',
    'benchmark.vram_isolation_unavailable',
    'benchmark.vram_no_gpu_samples',
    'benchmark.vram_isolation_blocked',
    'benchmark.vram_declared_gpu_missing',
    'runtime_spec.server_benchmarking',
  ] as const;

  it('carries every VRAM-benchmark refusal code, by its exact wire string', () => {
    for (const code of vramWireCodes) {
      expect(
        errorLabelByCode[code],
        `${code} is not mapped: the operator sees raw English`,
      ).toBeDefined();
    }
    // Both directions, so the list cannot go stale the way the reason
    // vocabularies did: a sixth `benchmark.vram_*` refusal added to the map
    // without being named here fails too.
    expect(
      entries
        .filter(([code]) => code.startsWith('benchmark.vram_'))
        .map(([code]) => code)
        .sort(),
    ).toEqual(
      vramWireCodes
        .filter((code) => code.startsWith('benchmark.vram_'))
        .slice()
        .sort(),
    );
  });

  /**
   * The benchmark starters' own codes other than the VRAM ones above, pinned
   * as LITERALS for the same reason: the whole-map invariants cannot catch a
   * code STRING drifting from the backend's, and an unmapped code reaches the
   * toast as the raw English the server sent.
   *
   * Declared in Go as `codeBenchmarkAlreadyRunning` and
   * `codeBenchmarkServerInUse` (`internal/gateway/benchmark_endpoints.go`),
   * as the `portal.ErrBenchmarkNoModels` sentinel's row in `benchmarkErrRows`
   * there, and as `writeBenchmarkError`'s 500 fallback code
   * (`benchmark.request_failed`), which a starter answers when a store read
   * fails before the run starts. The last four are the starters' refusals
   * before the reservation: `errBenchmarkImagesOnly`, its speech twin
   * `errBenchmarkAudioOnly`, `errBenchmarkAgentEnsureUnsupported` and
   * `errBenchmarkSpecForceStopped`, rows in the same `benchmarkErrRows`. The
   * VRAM probe answers the third of them too, under this same code rather than
   * a `benchmark.vram_*` one.
   *
   * The starters also answer three other codes that this list leaves out on
   * purpose, because no portal action can trigger them:
   * `benchmark.mode_invalid` and `benchmark.scope_invalid`
   * (`parseBenchmarkMode`, `benchmark_endpoints.go`), since the scope is fixed
   * by the URL path and the mode comes from the portal's own fixed selector;
   * and `benchmark.not_found`, since every store read behind
   * `writeBenchmarkError` folds a missing row into `mapping.not_found` (or the
   * application/server equivalent) before it can return `store.ErrNotFound`.
   */
  const benchmarkWireCodes = [
    'benchmark.already_running',
    'benchmark.server_in_use',
    'benchmark.no_models',
    'benchmark.request_failed',
    'benchmark.images_only',
    'benchmark.audio_only',
    'benchmark.agent_ensure_unsupported',
    'benchmark.spec_force_stopped',
  ] as const;

  it('carries every non-VRAM benchmark code a portal action can receive, by its exact wire string', () => {
    for (const code of benchmarkWireCodes) {
      expect(
        errorLabelByCode[code],
        `${code} is not mapped: the operator sees raw English`,
      ).toBeDefined();
    }
    // Both directions: a `benchmark.*` code outside the VRAM prefix added to
    // the map without being named here fails too.
    expect(
      entries
        .filter(([code]) => code.startsWith('benchmark.') && !code.startsWith('benchmark.vram_'))
        .map(([code]) => code)
        .sort(),
    ).toEqual(benchmarkWireCodes.slice().sort());
  });

  /**
   * Part 1's three live-timings refusals, pinned as LITERALS for the same
   * reason the VRAM codes above are: the whole-map invariants cannot catch a
   * code STRING drifting from the backend's, and an unmapped code is not an
   * error reported badly -- `formatPortalError` falls back to the raw English
   * the server sent, in a portal that is otherwise fully localized.
   *
   * Declared in Go as the sentinels
   * `portal.ErrApplicationResponsesLiveTimingsUnsupported` /
   * `...Conflict` / `portal.ErrRuntimeSpecResponsesLiveTimingsUnsupported`,
   * wired to these exact codes in the `errRow` tables in
   * `internal/gateway/portal_application_endpoints.go` and
   * `portal_runtime_endpoints.go`. Two application codes, not one: the same
   * refusal answers 400 when the request supplied the incapable type and 409
   * when the type came from the stored row.
   */
  const liveTimingsWireCodes = [
    'application.responses_live_timings_unsupported',
    'application.responses_live_timings_conflict',
    'runtime_spec.responses_live_timings_unsupported',
  ] as const;

  it('carries every live-timings refusal code, by its exact wire string', () => {
    for (const code of liveTimingsWireCodes) {
      expect(
        errorLabelByCode[code],
        `${code} is not mapped: the operator sees raw English`,
      ).toBeDefined();
    }
    // Both directions, like the VRAM list above: a fourth
    // `*.responses_live_timings_*` code added to the map without being named
    // here fails too.
    expect(
      entries
        .filter(([code]) => code.includes('responses_live_timings'))
        .map(([code]) => code)
        .sort(),
    ).toEqual(liveTimingsWireCodes.slice().sort());
  });

  /**
   * Task 8's chat-run-lifecycle codes, plus the mapping capability-form
   * refusal reachable from the operator form this feature added, pinned as
   * LITERALS for the same reason the VRAM/live-timings lists above are: the
   * whole-map invariants cannot catch a code STRING drifting from the
   * backend's, and an unmapped code is not an error reported badly --
   * `formatPortalError` falls back to the raw English the server sent.
   *
   * Declared in Go as `portal.ErrChatTooLarge` (service_chats.go),
   * `gateway.ErrRunAlreadyActive` / `ErrTooManyRuns` (chat_runs.go),
   * `portal.ErrMappingCapabilityReserved` (service_applications.go, wired to
   * this code in portal_mapping_endpoints.go's `errRow` table), and the
   * three `gateway.chat_run_*` string constants in chat_runs.go /
   * chat_runs_images.go (`runTimedOutMessage`,
   * `imageRunDispatchFailedMessage`, `imageRunNoImageMessage`,
   * `imageRunFormatUnknownMessage`, `imageRunResponseUnreadableMessage`,
   * `chatRunCommitFailedMessage`).
   *
   * The four `gateway.chat_run_*` codes of a SPEECH (text-to-speech) run are
   * `speechRunDispatchFailedMessage`, `speechRunNoAudioMessage`,
   * `speechRunResponseUnreadableMessage` and `speechRunReservedFieldMessage`
   * (chat_runs_audio.go): the audio twins of the image-run codes above, worded
   * about speech because the image labels talk about images.
   *
   * The five `gateway.chat_run_*` codes of a TRANSCRIPTION (speech-to-text) run are
   * `transcriptionRunDispatchFailedMessage`, `transcriptionRunSourceUnreadableMessage`,
   * `transcriptionRunResponseUnreadableMessage`, `transcriptionRunNoTranscriptMessage`
   * and `transcriptionRunReservedFieldMessage` (chat_runs_transcription.go): the
   * speech codes' counterparts for the opposite direction, worded about audio going
   * in and a transcript coming out.
   *
   * `portal.chat_run_history_too_large` is `portal.ErrChatRunHistoryTooLarge`
   * (service_chats.go, row in error_map.go): the run's image history, resolved
   * from the chat-blob store for inference, exceeded its bound. A 400 at run
   * start, so it reaches the toast through `formatPortalError`.
   *
   * `portal.chat_not_found` rides in this group because it shares the
   * `portal.chat_` prefix the both-directions assertion below filters on. It
   * is NOT a run-lifecycle code and predates this feature: it is
   * `portal.ErrChatNotFound` / `store.ErrNotFound` mapped by `error_map.go`,
   * `portal_chat_endpoints.go` and `chat_run_endpoints.go`, and every chat
   * writer (save, send, rename, delete) can raise it against a chat deleted
   * in another tab.
   */
  const chatRunWireCodes = [
    'portal.chat_too_large',
    'portal.chat_not_found',
    'portal.chat_run_active',
    'portal.chat_run_limit',
    'portal.chat_run_history_too_large',
    'mapping.capability_reserved',
    'gateway.chat_run_timeout',
    'gateway.chat_run_image_dispatch_failed',
    'gateway.chat_run_no_image',
    'gateway.chat_run_image_format_unknown',
    'gateway.chat_run_image_response_unreadable',
    // An image EDIT whose source cannot be read as an image (ADR-043 R1):
    // `imageRunSourceUnreadableMessage`, chat_runs_images.go.
    'gateway.chat_run_image_source_unreadable',
    // ...and one whose source is a type the edit route does not read (a generated or
    // uploaded webp/gif): `imageRunSourceUnsupportedMessage`, chat_runs_images.go.
    'gateway.chat_run_image_source_unsupported',
    'gateway.chat_run_commit_failed',
    // A speech run's own terminal codes (chat_runs_audio.go). The reserved-field
    // one is the run refusing, before anything is sent, a passthrough key it owns.
    'gateway.chat_run_speech_dispatch_failed',
    'gateway.chat_run_no_audio',
    'gateway.chat_run_speech_response_unreadable',
    'gateway.chat_run_speech_reserved_field',
    // A transcription run's own terminal codes (chat_runs_transcription.go). The
    // source-unreadable one is the run refusing, before anything is sent, a clip it
    // cannot read as audio (it never transcribes a different clip than the user
    // attached), and the no-transcript one is a 2xx that held no text.
    'gateway.chat_run_transcription_dispatch_failed',
    'gateway.chat_run_transcription_source_unreadable',
    'gateway.chat_run_transcription_response_unreadable',
    'gateway.chat_run_transcription_no_transcript',
    'gateway.chat_run_transcription_reserved_field',
  ] as const;

  it('carries every chat-run-lifecycle and mapping-capability code, by its exact wire string', () => {
    for (const code of chatRunWireCodes) {
      expect(
        errorLabelByCode[code],
        `${code} is not mapped: the operator sees raw English`,
      ).toBeDefined();
    }
    // Both directions, like the lists above: an eleventh `portal.chat_*` /
    // `gateway.chat_run*` / `mapping.capability_reserved` code added to the
    // map without being named here fails too.
    expect(
      entries
        .filter(
          ([code]) =>
            code.startsWith('portal.chat_') ||
            code.startsWith('gateway.chat_run') ||
            code === 'mapping.capability_reserved',
        )
        .map(([code]) => code)
        .sort(),
    ).toEqual(chatRunWireCodes.slice().sort());
  });

  /**
   * The four `images.*` codes Task 7 made reachable from a chat run, not
   * just from a direct API client (validateImagesRequest's two refusals,
   * plus the run's own relay of a non-2xx upstream body via
   * `upstreamErrorCode` / `imagesUpstreamErrorCode`, all in
   * `internal/gateway/images_handler.go`), plus the three
   * `images.blob_*` refusals of the chat binary payload store. Pinned as
   * literals for the same reason as the group above.
   */
  const imagesWireCodes = [
    'images.prompt_required',
    'images.stream_unsupported',
    'images.response_format_unsupported',
    'images.upstream_error',
    // The chat binary payload store's own refusals (portal.ErrChatBlob*,
    // service_chats.go; rows in error_map.go): a run-start 400 when an
    // uploaded or edited image is not png/jpeg/webp/gif, exceeds the per-image
    // cap, or an edited_history entry names an image this chat does not own.
    'images.blob_type_unsupported',
    'images.blob_too_large',
    'images.blob_not_owned',
    // The two refusals of POST /v1/images/edits a portal edit turn can hit
    // (relayed by upstreamErrorCode): an edit with no image part
    // (`imagesImageRequired`, images_edit_handler.go) and an edit whose target
    // is an agent that has not declared multipart_proxy (`codeImageEditUnavailable`,
    // 404). The two refusals only a mask can cause join them with the portal's
    // mask upload (`imagesMaskInvalid`, `imagesEditUnsupportedFormat`, error_map.go;
    // raised by convertImageEditMask and relayed by upstreamErrorCode).
    'images.image_required',
    'images.edit_unavailable',
    'images.mask_invalid',
    'images.edit_unsupported_format',
  ] as const;

  it('carries every images.* refusal code reachable from a chat run, by its exact wire string', () => {
    for (const code of imagesWireCodes) {
      expect(
        errorLabelByCode[code],
        `${code} is not mapped: the operator sees raw English`,
      ).toBeDefined();
    }
    expect(
      entries
        .filter(([code]) => code.startsWith('images.'))
        .map(([code]) => code)
        .sort(),
    ).toEqual(imagesWireCodes.slice().sort());
  });

  // An image edit's refusals reach a chat run as the terminal CODE (the loopback
  // endpoint's error.code, relayed by upstreamErrorCode), so each must map to its
  // own localized sentence in BOTH locales -- and, as a run carries no server
  // text to fall back on, never degrade to the bare code.
  it.each([
    ['images.image_required', 'errorImagesImageRequired'],
    ['images.edit_unavailable', 'errorImagesEditUnavailable'],
    ['images.mask_invalid', 'errorImagesMaskInvalid'],
    ['images.edit_unsupported_format', 'errorImagesEditUnsupportedFormat'],
    ['gateway.chat_run_image_source_unreadable', 'errorChatRunImageSourceUnreadable'],
    ['gateway.chat_run_image_source_unsupported', 'errorChatRunImageSourceUnsupported'],
  ] as const)('labels the image-edit code %s with %s, in both locales', (code, key) => {
    expect(errorLabelByCode[code]).toBe(key);
    for (const locale of ['de', 'en'] as const) {
      const t = messages[locale];
      expect(t[key].length).toBeGreaterThan(0);
      expect(formatChatRunErrorCode(code, t)).toBe(t[key]);
      expect(formatChatRunErrorCode(code, t)).not.toBe(code);
    }
    expect(messages.de[key]).not.toBe(messages.en[key]);
  });

  /**
   * The four `speech.*` codes of POST /v1/audio/speech a chat run can surface,
   * the speech twins of the `images.*` group above (the run relays the loopback
   * endpoint's error.code through `upstreamErrorCode`). Declared in Go as
   * `speechInputRequired`, `speechResponseFormatUnsupported` and
   * `speechStreamUnsupported` (speech_handler.go, validateSpeechRequest) and
   * `speechUpstreamErrorCode` (the gateway's own classification of an audio.cpp
   * error that is not already an OpenAI error object). Pinned as literals for the
   * same reason as the groups above.
   *
   * `speech.model_required` is unmapped on purpose, exactly as the images
   * endpoint's `images.model_required` is: a run always sends the thread's pinned
   * model, so the portal can never raise it. `speech.response_format_unsupported`
   * is mapped although the run states response_format itself (a passthrough key
   * of that name is refused as `gateway.chat_run_speech_reserved_field` before
   * anything is sent), because the endpoint can answer it to any client and the
   * run relays whatever code comes back; `speech.stream_unsupported` IS reachable
   * from the portal, through a `stream_format` passthrough key (the one
   * streaming-related field the run does not reserve).
   */
  const speechWireCodes = [
    'speech.input_required',
    'speech.response_format_unsupported',
    'speech.stream_unsupported',
    'speech.upstream_error',
  ] as const;

  it('carries every speech.* code reachable from a chat run, by its exact wire string', () => {
    for (const code of speechWireCodes) {
      expect(
        errorLabelByCode[code],
        `${code} is not mapped: the user sees the raw wire code`,
      ).toBeDefined();
    }
    expect(
      entries
        .filter(([code]) => code.startsWith('speech.'))
        .map(([code]) => code)
        .sort(),
    ).toEqual(speechWireCodes.slice().sort());
  });

  // A speech run's failures reach the transcript as the terminal CODE (the run
  // carries no server text), so each must map to its own localized sentence in BOTH
  // locales and never degrade to the bare code -- the speech twin of the image-edit
  // table above. The label keys are pinned too, so a code cannot be re-pointed at an
  // image sentence (which would tell a speech user about an image).
  it.each([
    ['gateway.chat_run_speech_dispatch_failed', 'errorChatRunSpeechDispatchFailed'],
    ['gateway.chat_run_no_audio', 'errorChatRunNoAudio'],
    ['gateway.chat_run_speech_response_unreadable', 'errorChatRunSpeechResponseUnreadable'],
    ['gateway.chat_run_speech_reserved_field', 'errorChatRunSpeechReservedField'],
    ['speech.input_required', 'errorSpeechInputRequired'],
    ['speech.response_format_unsupported', 'errorSpeechResponseFormatUnsupported'],
    ['speech.stream_unsupported', 'errorSpeechStreamUnsupported'],
    ['speech.upstream_error', 'errorSpeechUpstreamError'],
  ] as const)('labels the speech code %s with %s, in both locales', (code, key) => {
    expect(errorLabelByCode[code]).toBe(key);
    for (const locale of ['de', 'en'] as const) {
      const t = messages[locale];
      expect(t[key].length).toBeGreaterThan(0);
      expect(formatChatRunErrorCode(code, t)).toBe(t[key]);
      expect(formatChatRunErrorCode(code, t)).not.toBe(code);
      // A toast for the endpoint's own refusal shows the label after the code,
      // never the server's English.
      if (code.startsWith('speech.')) {
        expect(formatPortalError(new PortalApiError(400, code, 'raw server text'), t)).toBe(
          `${code}: ${t[key]}`,
        );
      }
    }
    expect(messages.de[key]).not.toBe(messages.en[key]);
  });

  /**
   * The five `transcription.*` codes of POST /v1/audio/transcriptions a chat run can
   * surface, the transcription twins of the `speech.*` group above (the run relays the
   * loopback endpoint's error.code through `upstreamErrorCode`). Declared in Go as
   * `transcriptionFileRequired`, `transcriptionResponseFormatUnsupported` and
   * `transcriptionStreamUnsupported` (transcription_handler.go,
   * validateTranscriptionRequest), `codeTranscriptionUnavailable` (the 404 of a target
   * agent that has not declared multipart_proxy, the image edit's
   * `images.edit_unavailable` twin) and `transcriptionUpstreamErrorCode` (the gateway's
   * own classification of an audio.cpp error that is not already an OpenAI error
   * object). Pinned as literals for the same reason as the groups above.
   *
   * Two are unmapped on purpose. `transcription.model_required` is, exactly as
   * `speech.model_required` and `images.model_required` are: a run always sends the
   * thread's pinned model, so the portal can never raise it. `transcription.invalid`
   * (a structurally broken form) is, as the image edit's twin `request.invalid` is: the
   * run builds its own form, so only a field over the endpoint's 1 MiB bound could
   * raise it. `transcription.file_required` is mapped although the run attaches its
   * clip itself (a clip that decodes to nothing ends the run as
   * `gateway.chat_run_transcription_source_unreadable` before anything is sent), and so
   * is `transcription.response_format_unsupported` although the run reserves
   * response_format: the endpoint can answer both to any client and the run relays
   * whatever code comes back, as `speech.response_format_unsupported` is for speech.
   * `transcription.stream_unsupported` IS reachable from the portal, through a
   * `stream_format` passthrough key (the one streaming-related field the run does not
   * reserve). The gateway emits it only for a non-boolean `stream` and for any
   * `stream_format`; `stream: true` itself is valid for a streaming model.
   */
  const transcriptionWireCodes = [
    'transcription.file_required',
    'transcription.response_format_unsupported',
    'transcription.stream_unsupported',
    'transcription.unavailable',
    'transcription.upstream_error',
  ] as const;

  it('carries every transcription.* code reachable from a chat run, by its exact wire string', () => {
    for (const code of transcriptionWireCodes) {
      expect(
        errorLabelByCode[code],
        `${code} is not mapped: the user sees the raw wire code`,
      ).toBeDefined();
    }
    expect(
      entries
        .filter(([code]) => code.startsWith('transcription.'))
        .map(([code]) => code)
        .sort(),
    ).toEqual(transcriptionWireCodes.slice().sort());
  });

  // A transcription run's failures reach the transcript as the terminal CODE (the run
  // carries no server text), so each must map to its own localized sentence in BOTH
  // locales and never degrade to the bare code -- the transcription twin of the speech
  // table above. The label keys are pinned too, so a code cannot be re-pointed at a
  // speech or image sentence (which would tell a transcription user about synthesized
  // audio or an image).
  it.each([
    ['gateway.chat_run_transcription_dispatch_failed', 'errorChatRunTranscriptionDispatchFailed'],
    [
      'gateway.chat_run_transcription_source_unreadable',
      'errorChatRunTranscriptionSourceUnreadable',
    ],
    [
      'gateway.chat_run_transcription_response_unreadable',
      'errorChatRunTranscriptionResponseUnreadable',
    ],
    ['gateway.chat_run_transcription_no_transcript', 'errorChatRunNoTranscript'],
    ['gateway.chat_run_transcription_reserved_field', 'errorChatRunTranscriptionReservedField'],
    ['transcription.file_required', 'errorTranscriptionFileRequired'],
    ['transcription.response_format_unsupported', 'errorTranscriptionResponseFormatUnsupported'],
    ['transcription.stream_unsupported', 'errorTranscriptionStreamUnsupported'],
    ['transcription.unavailable', 'errorTranscriptionUnavailable'],
    ['transcription.upstream_error', 'errorTranscriptionUpstreamError'],
  ] as const)('labels the transcription code %s with %s, in both locales', (code, key) => {
    expect(errorLabelByCode[code]).toBe(key);
    for (const locale of ['de', 'en'] as const) {
      const t = messages[locale];
      expect(t[key].length).toBeGreaterThan(0);
      expect(formatChatRunErrorCode(code, t)).toBe(t[key]);
      expect(formatChatRunErrorCode(code, t)).not.toBe(code);
      // A toast for the endpoint's own refusal shows the label after the code,
      // never the server's English.
      if (code.startsWith('transcription.')) {
        expect(formatPortalError(new PortalApiError(400, code, 'raw server text'), t)).toBe(
          `${code}: ${t[key]}`,
        );
      }
    }
    expect(messages.de[key]).not.toBe(messages.en[key]);
  });

  /**
   * The four refusals the chat binary payload store added to a chat run's
   * start. Each is a 400 the server words in English ("image is too large");
   * the portal must show its own localized label for it in BOTH locales --
   * after the code, as every other portal error does -- and never the raw
   * server text.
   */
  const blobStoreWireCodes = [
    'images.blob_type_unsupported',
    'images.blob_too_large',
    'images.blob_not_owned',
    'portal.chat_run_history_too_large',
  ] as const;

  it.each([
    ['de', messages.de],
    ['en', messages.en],
  ] as const)(
    'renders a localized label, not the raw server text, for each payload-store refusal (%s)',
    (_locale, t) => {
      for (const code of blobStoreWireCodes) {
        const out = formatPortalError(new PortalApiError(400, code, 'raw server text'), t);
        expect(out, code).not.toContain('raw server text');
        expect(out, code).toBe(`${code}: ${t[errorLabelByCode[code]!]}`);
        // A run-terminal error carries only the code: it must map too, and
        // degrade to the label rather than the bare code.
        expect(formatChatRunErrorCode(code, t), code).toBe(t[errorLabelByCode[code]!]);
      }
    },
  );

  /**
   * Fix round, finding 1: keeping a non-200's body (Deliverable 2) made
   * these nine codes reach the chat toast from the TEXT path for the first
   * time -- previously the branch discarded the body entirely and showed a
   * flat "upstream status ..." line, so nothing was unmapped because
   * nothing reached the map. Read verbatim from Go:
   * `completionErrorCode`/`completionErrorResponse` in
   * `internal/gateway/inference_complete.go` build eight of the nine from
   * the sentinels in `internal/routing/resolver.go` and
   * `internal/provider/client.go`; `model.not_allowed` is the odd one out,
   * written directly by `writeModelNotAllowed` in
   * `internal/gateway/inference_handlers.go`, reachable from the same
   * loopback hop.
   */
  const completionWireCodes = [
    'routing.no_healthy_host',
    'routing.no_model_route',
    'routing.model_not_capable',
    'routing.admission_queue_full',
    'routing.admission_queue_timeout',
    'provider.unavailable',
    'provider.timeout',
    'provider.invalid_response',
    'model.not_allowed',
  ] as const;

  it('carries every completion-error code now reachable from the chat text path, by its exact wire string', () => {
    for (const code of completionWireCodes) {
      expect(
        errorLabelByCode[code],
        `${code} is not mapped: the operator sees raw English`,
      ).toBeDefined();
    }
    expect(
      entries
        .filter(
          ([code]) =>
            code.startsWith('routing.') ||
            code.startsWith('provider.') ||
            code === 'model.not_allowed',
        )
        .map(([code]) => code)
        .sort(),
    ).toEqual(completionWireCodes.slice().sort());
  });

  /**
   * The vendor-account ("Anbieter") page's twenty-four refusal codes, pinned as
   * LITERALS for the same reason as the lists above: the whole-map invariants
   * cannot catch a code STRING drifting from the backend's, and an unmapped
   * code reaches the toast as the raw English the server sent.
   *
   * Declared in Go as the `ErrVendorAccount*` sentinels in
   * `internal/portal/service.go` (plus `CodeVendorAccountNotFound`) and the
   * `capture.ErrKeyRequired` row, all wired to these exact codes in
   * `portalVendorAccountErrRows`
   * (`internal/gateway/portal_vendor_account_endpoints.go`). The last ten are
   * the subscription-connect ones (`internal/portal/service_vendor_connect.go`
   * and `service_vendor_device_connect.go`): seven errRow codes plus
   * `connect_failed`, the connect handlers' 500 fallback, which is mapped
   * deliberately (its server message is only a generic English sentence), plus
   * the two the OpenAI-only device-code flow adds. `connect_invalid_credentials` is the
   * import's 400 for a token the vendor definitively rejected (nothing stored), and
   * `check_failed` the connection test's 500 fallback, mapped for the same reason as
   * `connect_failed`. `credential_unreadable` is the 409 of the connection test and of
   * the models refresh for a stored credential that cannot be opened (a lost key, a
   * corrupt blob). `model_prefix_invalid` is the 400 of create and update for a
   * malformed model prefix, `base_url_required` / `base_url_invalid` the 400s of
   * create for a configurable-root account's missing (Custom) or malformed base URL, `refresh_failed` the models refresh's 500 fallback and
   * `usage_refresh_failed` the usage refresh's, both mapped for the same reason as
   * `check_failed`. The last seven are the sharing ones (#183;
   * `internal/portal/service_vendor_sharing.go`): `share_level_unchanged`,
   * `subscription_share_capped`, `admin_group_required`, `admin_group_invalid`,
   * `admin_group_parent_mismatch`, `root_mismatch` and `not_shared`.
   * `share_level_invalid` is unmapped on purpose: the portal only sends none, shared
   * or full. The five CRUD 500
   * `vendor_account.*_failed` fallbacks (list/create/get/update/delete) are left
   * unmapped on purpose: they carry the server's own message.
   */
  const vendorAccountWireCodes = [
    'vendor_account.not_found',
    'vendor_account.name_required',
    'vendor_account.vendor_invalid',
    'vendor_account.auth_type_invalid',
    'vendor_account.status_invalid',
    'vendor_account.forbidden',
    'vendor_account.api_key_not_allowed',
    'vendor_account.api_key_invalid',
    'vendor_account.api_key_key_required',
    'vendor_account.not_subscription',
    'vendor_account.connect_token_required',
    'vendor_account.connect_code_required',
    'vendor_account.connect_state',
    'vendor_account.connect_rejected',
    'vendor_account.connect_invalid_credentials',
    'vendor_account.connect_upstream_failed',
    'vendor_account.connect_key_required',
    'vendor_account.connect_failed',
    'vendor_account.device_not_supported',
    'vendor_account.device_connect_state',
    'vendor_account.check_failed',
    'vendor_account.credential_unreadable',
    'vendor_account.model_prefix_invalid',
    'vendor_account.base_url_required',
    'vendor_account.base_url_invalid',
    'vendor_account.refresh_failed',
    'vendor_account.usage_refresh_failed',
    'vendor_account.share_level_unchanged',
    'vendor_account.subscription_share_capped',
    'vendor_account.admin_group_required',
    'vendor_account.admin_group_invalid',
    'vendor_account.admin_group_parent_mismatch',
    'vendor_account.root_mismatch',
    'vendor_account.not_shared',
  ] as const;

  it('carries every vendor-account refusal code, by its exact wire string', () => {
    for (const code of vendorAccountWireCodes) {
      expect(
        errorLabelByCode[code],
        `${code} is not mapped: the operator sees raw English`,
      ).toBeDefined();
    }
    // Both directions: a further `vendor_account.*` code added to the map
    // without being named here fails too.
    expect(
      entries
        .filter(([code]) => code.startsWith('vendor_account.'))
        .map(([code]) => code)
        .sort(),
    ).toEqual(vendorAccountWireCodes.slice().sort());
  });

  // The master-flag 409 (`portalVendorAccountErrRows`' first row). Its code is
  // `vendor_accounts.` (plural) -- a different prefix from the codes above -- so
  // the both-directions check does not cover it: pin it on its own.
  it('maps the vendor-accounts module-disabled refusal by its exact wire string', () => {
    expect(errorLabelByCode['vendor_accounts.module_disabled']).toBe(
      'errorVendorAccountsModuleDisabled',
    );
  });

  // The vendor_subscriptions_enabled switch is off: creating or connecting a
  // subscription account is a 409 with this code (portalVendorAccountErrRows).
  it('maps the vendor subscriptions-disabled refusal by its exact wire string', () => {
    expect(errorLabelByCode['vendor_accounts.subscriptions_disabled']).toBe(
      'errorVendorSubscriptionsDisabled',
    );
  });

  // The two sharing switches' 409s (`vendor_accounts.` plural, so outside the
  // both-directions check above): the master sharing switch and the narrower
  // subscription-sharing one (portalVendorAccountErrRows).
  it('maps the vendor sharing-disabled refusals by their exact wire strings', () => {
    expect(errorLabelByCode['vendor_accounts.sharing_disabled']).toBe(
      'errorVendorAccountsSharingDisabled',
    );
    expect(errorLabelByCode['vendor_accounts.subscription_sharing_disabled']).toBe(
      'errorVendorSubscriptionSharingDisabled',
    );
  });

  // The per-token vendor-access refusals (`portal.token_vendor_access_*`, the
  // errRow table in portal_token_endpoints.go), pinned as literals: a code the
  // map does not carry falls back to the raw English the server sent.
  it('maps the token vendor-access refusals by their exact wire strings', () => {
    expect(errorLabelByCode['portal.token_vendor_access_invalid']).toBe(
      'errorTokenVendorAccessInvalid',
    );
    expect(errorLabelByCode['portal.token_vendor_access_conflict']).toBe(
      'errorTokenVendorAccessConflict',
    );
  });

  it('reuses a label for two codes only where that is deliberate', () => {
    // The realistic defect in a hand-maintained map this size is a new entry
    // pointed at its neighbour's label by copy-paste. Every shared label is
    // therefore listed here explicitly; `request.failed` /
    // `request.invalid_response` are one event to the operator ("the request
    // did not work"), which is why they share one.
    const documentedSharedLabels: Record<string, string[]> = {
      errorRequestFailed: ['request.failed', 'request.invalid_response'],
    };
    const codesByLabel = new Map<string, string[]>();
    for (const [code, key] of entries) {
      const list = codesByLabel.get(String(key)) ?? [];
      list.push(code);
      codesByLabel.set(String(key), list);
    }
    for (const [label, codes] of codesByLabel) {
      if (codes.length === 1) continue;
      expect(
        codes.slice().sort(),
        `${codes.join(' + ')} share the label ${label}; add them to documentedSharedLabels if that is intended`,
      ).toEqual((documentedSharedLabels[label] ?? []).slice().sort());
    }
  });
});
