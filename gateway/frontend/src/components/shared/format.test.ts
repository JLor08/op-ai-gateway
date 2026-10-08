// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { errorLabelByCode, formatDate, formatMetric, formatPortalError } from './format';
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

  // The starters' three refusals before they reserve the server, each with its
  // OWN label: they send the operator to three different places (a model that
  // only generates images, an agent update, an admin override).
  it.each([
    ['benchmark.images_only', 'errorBenchmarkImagesOnly'],
    ['benchmark.agent_ensure_unsupported', 'errorBenchmarkAgentEnsureUnsupported'],
    ['benchmark.spec_force_stopped', 'errorBenchmarkSpecForceStopped'],
  ] as const)('labels the %s refusal with %s, in both locales', (code, key) => {
    const err = new PortalApiError(409, code, 'raw server text');
    expect(formatPortalError(err, messages.de)).toBe(`${code}: ${messages.de[key]}`);
    expect(formatPortalError(err, messages.en)).toBe(`${code}: ${messages.en[key]}`);
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
   * fails before the run starts. The last three are the starters' refusals
   * before the reservation: `errBenchmarkImagesOnly`,
   * `errBenchmarkAgentEnsureUnsupported` and `errBenchmarkSpecForceStopped`,
   * rows in the same `benchmarkErrRows`. The VRAM probe answers the second of
   * them too, under this same code rather than a `benchmark.vram_*` one.
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
    'mapping.capability_reserved',
    'gateway.chat_run_timeout',
    'gateway.chat_run_image_dispatch_failed',
    'gateway.chat_run_no_image',
    'gateway.chat_run_image_format_unknown',
    'gateway.chat_run_image_response_unreadable',
    'gateway.chat_run_commit_failed',
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
   * `internal/gateway/images_handler.go`). Pinned as literals for the same
   * reason as the group above.
   */
  const imagesWireCodes = [
    'images.prompt_required',
    'images.stream_unsupported',
    'images.response_format_unsupported',
    'images.upstream_error',
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
   * The vendor-account ("Anbieter") page's twenty-one refusal codes, pinned as
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
   * `connect_failed`. The five CRUD 500 `vendor_account.*_failed`
   * fallbacks (list/create/get/update/delete) are left unmapped on purpose:
   * they carry the server's own message.
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
  ] as const;

  it('carries every vendor-account refusal code, by its exact wire string', () => {
    for (const code of vendorAccountWireCodes) {
      expect(
        errorLabelByCode[code],
        `${code} is not mapped: the operator sees raw English`,
      ).toBeDefined();
    }
    // Both directions: a twenty-second `vendor_account.*` code added to the map
    // without being named here fails too.
    expect(
      entries
        .filter(([code]) => code.startsWith('vendor_account.'))
        .map(([code]) => code)
        .sort(),
    ).toEqual(vendorAccountWireCodes.slice().sort());
  });

  // The master-flag 409 (`portalVendorAccountErrRows`' first row). Its code is
  // `vendor_accounts.` (plural) -- a different prefix from the twenty-one above -- so
  // the both-directions check does not cover it: pin it on its own.
  it('maps the vendor-accounts module-disabled refusal by its exact wire string', () => {
    expect(errorLabelByCode['vendor_accounts.module_disabled']).toBe(
      'errorVendorAccountsModuleDisabled',
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
