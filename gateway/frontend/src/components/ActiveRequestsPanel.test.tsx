// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { ActiveRequestsPanel } from './ActiveRequestsPanel';
import { messages } from '../i18n';
import type { ActiveRequest } from '../api';

const t = messages.de;

function makeActive(overrides: Partial<ActiveRequest> = {}): ActiveRequest {
  return {
    id: 'act_1',
    user_id: 'usr_1',
    user_name: 'Live User',
    token_id: 'tok_1',
    token_name: 'Live Token',
    model: 'live-model',
    // Deliberately different from `model` (as if a token override were in
    // play) so the model and requested-model columns never collide on text.
    requested_model: 'client-model',
    server_name: 'live-server',
    api_flavor: 'openai',
    req_path: '/v1/chat/completions',
    provider_path: '/v1/chat/completions',
    provider_model: 'upstream-live-model',
    stream: true,
    started_at: '2026-07-16T12:00:00.000Z',
    // Defaults mean "not measured" so a test that doesn't care about live
    // metrics gets the never-measured convention for free.
    output_tokens: 0,
    tokens_per_second: 0,
    tokens_per_second_source: '',
    ttft_ms: 0,
    ...overrides,
  };
}

afterEach(cleanup);

describe('ActiveRequestsPanel live metrics columns', () => {
  it('shows the em-dash for a request with no measurement and the rate with one decimal otherwise', () => {
    render(
      <ActiveRequestsPanel
        t={t}
        active={[
          // ttft_ms set to a distinct non-zero value so the em-dash assertion
          // below can only match the live_tps cell in this row.
          makeActive({
            id: 'act_none',
            tokens_per_second: 0,
            tokens_per_second_source: '',
            ttft_ms: 111,
          }),
          makeActive({
            id: 'act_upstream',
            model: 'live-model-2',
            tokens_per_second: 21.5,
            tokens_per_second_source: 'upstream',
          }),
        ]}
        effectiveScope="own"
      />,
    );

    const noneRow = screen.getByRole('cell', { name: 'live-model' }).closest('tr')!;
    expect(within(noneRow).getByRole('cell', { name: '—' })).toBeInTheDocument();

    const measuredRow = screen.getByRole('cell', { name: 'live-model-2' }).closest('tr')!;
    expect(within(measuredRow).getByRole('cell', { name: '21.5' })).toBeInTheDocument();
  });

  it('renders the TTFT cell as an em-dash when unmeasured and "<n> ms" otherwise', () => {
    render(
      <ActiveRequestsPanel
        t={t}
        active={[
          // tokens_per_second set to a distinct non-zero value so the em-dash
          // assertion below can only match the ttft cell in this row.
          makeActive({ id: 'act_no_ttft', ttft_ms: 0, tokens_per_second: 9.9 }),
          makeActive({ id: 'act_ttft', model: 'live-model-2', ttft_ms: 480 }),
        ]}
        effectiveScope="own"
      />,
    );

    const noTtftRow = screen.getByRole('cell', { name: 'live-model' }).closest('tr')!;
    expect(within(noTtftRow).getByRole('cell', { name: '—' })).toBeInTheDocument();

    const ttftRow = screen.getByRole('cell', { name: 'live-model-2' }).closest('tr')!;
    expect(within(ttftRow).getByRole('cell', { name: '480 ms' })).toBeInTheDocument();
  });

  it('names the provenance in the tooltip, including the token count for a gateway-computed rate', () => {
    render(
      <ActiveRequestsPanel
        t={t}
        active={[
          makeActive({
            tokens_per_second: 33.3,
            tokens_per_second_source: 'gateway',
            output_tokens: 50,
          }),
        ]}
        effectiveScope="own"
      />,
    );

    const cell = screen.getByText('33.3');
    expect(cell).toHaveAttribute('title', expect.stringContaining('50'));
  });

  it('names the upstream-reported provenance in the tooltip', () => {
    render(
      <ActiveRequestsPanel
        t={t}
        active={[makeActive({ tokens_per_second: 12.0, tokens_per_second_source: 'upstream' })]}
        effectiveScope="own"
      />,
    );

    expect(screen.getByText('12.0')).toHaveAttribute('title', t.activityLiveTpsUpstream);
  });

  it('distinguishes a measured rate that rounds to 0.0 from a measured zero', () => {
    render(
      <ActiveRequestsPanel
        t={t}
        active={[
          // A real, upstream-exact count over a stalled generation window:
          // 1 token / 25s. Rendering "0.0" here would make a genuine measurement
          // indistinguishable from no throughput at all.
          makeActive({
            id: 'act_tiny',
            tokens_per_second: 0.04,
            tokens_per_second_source: 'gateway',
            output_tokens: 1,
            ttft_ms: 333,
          }),
          // The never-measured row, for contrast: still the shared em-dash.
          makeActive({
            id: 'act_none',
            model: 'live-model-2',
            tokens_per_second: 0,
            tokens_per_second_source: '',
            ttft_ms: 444,
          }),
        ]}
        effectiveScope="own"
      />,
    );

    const tinyRow = screen.getByRole('cell', { name: 'live-model' }).closest('tr')!;
    expect(within(tinyRow).getByRole('cell', { name: '<0.1' })).toBeInTheDocument();
    expect(within(tinyRow).queryByRole('cell', { name: '0.0' })).not.toBeInTheDocument();
    // The provenance tooltip still says it was computed from the reported count.
    expect(screen.getByText('<0.1')).toHaveAttribute('title', expect.stringContaining('1'));

    const noneRow = screen.getByRole('cell', { name: 'live-model-2' }).closest('tr')!;
    expect(within(noneRow).getByRole('cell', { name: '—' })).toBeInTheDocument();
  });

  it('names the never-measured provenance in the tooltip', () => {
    render(
      <ActiveRequestsPanel
        t={t}
        // ttft_ms set to a distinct non-zero value so the em-dash text query
        // below resolves to the single live_tps cell (which carries the
        // tooltip), not also the ttft cell (which never has one).
        active={[makeActive({ tokens_per_second: 0, tokens_per_second_source: '', ttft_ms: 222 })]}
        effectiveScope="own"
      />,
    );

    expect(screen.getByText('—')).toHaveAttribute('title', t.activityLiveTpsNone);
  });
});

// The row shapes a NATIVE-PASSTHROUGH stream puts on this panel, one test per
// cell of the per-flavor table in
// docs/architecture/cross-cutting/telemetry-usage-observability.md §8.4.3, under
// "Native passthrough is on this panel too". Three of them reach the panel for the
// first time with the passthrough bridge: the DTO's keys did not change, but these
// COMBINATIONS of them did not previously occur.
//
// Each pins the absence as hard as the value, in both directions, so no later
// change can satisfy a cell by counting SSE deltas as tokens: with the upstream's
// exact count asserted on the row, a delta-derived contribution shows up as a
// wrong number rather than merely as a number where there should be none.
//
// The api_flavor / req_path / provider_path / output_tokens fields in these fixtures
// document which real row each case IS; they are not flavor coverage, because the
// asserted cells do not read them — the panel renders the live cells from
// tokens_per_second, its source and ttft_ms alone.
describe('ActiveRequestsPanel native-passthrough row shapes', () => {
  it('shows a TTFT beside an em-dash rate when the stream has a first-content stamp and nothing else', () => {
    // openai_responses passthrough, mid-generation, with timings_per_token set by
    // NOBODY -- neither the client nor the operator's Responses live-timings
    // switch: the first content frame stamped a TTFT and the upstream attached no
    // `timings` to any partial, so there is neither a rate to report nor a
    // predicted_n to count. So: a real TTFT, no count, no rate -- and the rate
    // cell must read as not-applicable, never as a zero (which would read as
    // "stalled").
    render(
      <ActiveRequestsPanel
        t={t}
        active={[
          makeActive({
            id: 'act_pt_ttft_only',
            api_flavor: 'openai_responses',
            req_path: '/v1/responses',
            provider_path: '/v1/responses',
            ttft_ms: 640,
            output_tokens: 0,
            tokens_per_second: 0,
            tokens_per_second_source: '',
          }),
        ]}
        effectiveScope="own"
      />,
    );

    const row = screen.getByRole('cell', { name: 'live-model' }).closest('tr')!;
    expect(within(row).getByRole('cell', { name: '640 ms' })).toBeInTheDocument();
    expect(within(row).getByRole('cell', { name: '—' })).toBeInTheDocument();
    // The live-rate cell itself, addressed by its provenance tooltip rather than by
    // its text — the absence must not read as a measurement of nothing. A query for
    // '0' or '0.0' could not fail here (no visible column of this panel renders
    // either string in any state), whereas this pins that ONE cell's own text: it is
    // violated by any mutation that renders a zero rate as a number (formatMetric's
    // falsy guard no longer mapping 0 to the shared em-dash yields '0.0') and,
    // unlike the row-wide query above, it cannot be satisfied by some OTHER cell
    // holding the dash.
    expect(within(row).getByTitle(t.activityLiveTpsNone).textContent).toBe('—');
    expect(screen.getByText('—')).toHaveAttribute('title', t.activityLiveTpsNone);
  });

  it('shows an upstream-reported rate on a row whose token count is still zero, and claims no count', () => {
    // Same stream WITH timings_per_token set -- by the client, or by the
    // operator's Responses live-timings switch: llama.cpp attaches its own
    // `timings` to the partial frames, so a real rate arrives. The count can
    // still be 0, because the mid-stream count is `timings.predicted_n` and
    // nothing else: a partial whose `timings` object carries a rate and no
    // predicted_n reports a rate and no count, the shape pinned by the first case
    // of TestPassthroughResponsesPartialPredictedNDecidesTheMidStreamCount
    // (passthrough_progress_test.go). ("The Responses partials carry no usage"
    // was the reason until predicted_n was read; it is no longer one.) "rate
    // present, tokens absent" is therefore still a legitimate row here, so the
    // tooltip for an upstream-reported rate must make no claim about a count -- a
    // "computed from 0 tokens" reading would be false in exactly the case that
    // produces this row.
    render(
      <ActiveRequestsPanel
        t={t}
        active={[
          makeActive({
            id: 'act_pt_upstream_rate',
            api_flavor: 'openai_responses',
            req_path: '/v1/responses',
            provider_path: '/v1/responses',
            ttft_ms: 640,
            output_tokens: 0,
            tokens_per_second: 42.5,
            tokens_per_second_source: 'upstream',
          }),
        ]}
        effectiveScope="own"
      />,
    );

    const cell = screen.getByText('42.5');
    expect(cell).toHaveAttribute('title', t.activityLiveTpsUpstream);
    // No figure at all in the tooltip, so the row's 0 cannot leak into it.
    expect(cell.getAttribute('title')).not.toMatch(/\d/);
  });

  it("shows the terminal frame's exact count and the rate derived from it before the row leaves the panel", () => {
    // A `response.completed` frame carries the upstream's own final
    // response.usage.output_tokens, published while the row is still active — so
    // the count and a rate become visible for the short window before the row
    // drops out. This fixture's `gateway` label is the shape an upstream whose
    // terminal frame carries no `timings` of its own produces: real llama.cpp
    // always attaches its own `timings` to that frame and would label the row
    // `upstream` instead — both cases are pinned, in both directions, by
    // TestPassthroughResponsesTerminalUsageBecomesVisibleBeforeTheRowLeaves
    // (passthrough_progress_test.go). Only the DTO shape is under test here: the
    // tooltip is asserted WHOLE against the interpolated string, so any
    // delta-derived contribution added to the upstream's exact count fails.
    render(
      <ActiveRequestsPanel
        t={t}
        active={[
          makeActive({
            id: 'act_pt_terminal',
            api_flavor: 'openai_responses',
            req_path: '/v1/responses',
            provider_path: '/v1/responses',
            ttft_ms: 640,
            output_tokens: 40,
            tokens_per_second: 26.7,
            tokens_per_second_source: 'gateway',
          }),
        ]}
        effectiveScope="own"
      />,
    );

    const cell = screen.getByText('26.7');
    expect(cell).toHaveAttribute('title', t.activityLiveTpsGateway.replace('{n}', '40'));
  });

  it('shows the anthropic_messages row complete: TTFT, and a gateway rate over the cumulative count', () => {
    // llama.cpp attaches no `timings` to any Anthropic frame, so the derived rate
    // is the ONLY source for this flavor — but message_delta carries a cumulative
    // output_tokens from the first one on, so the count it is derived from is
    // exact and present throughout the stream.
    render(
      <ActiveRequestsPanel
        t={t}
        active={[
          makeActive({
            id: 'act_pt_anthropic',
            api_flavor: 'anthropic_messages',
            req_path: '/v1/messages',
            provider_path: '/v1/messages',
            ttft_ms: 310,
            output_tokens: 17,
            tokens_per_second: 8.4,
            tokens_per_second_source: 'gateway',
          }),
        ]}
        effectiveScope="own"
      />,
    );

    const row = screen.getByRole('cell', { name: 'live-model' }).closest('tr')!;
    expect(within(row).getByRole('cell', { name: '310 ms' })).toBeInTheDocument();
    expect(within(row).getByRole('cell', { name: '8.4' })).toBeInTheDocument();
    expect(screen.getByText('8.4')).toHaveAttribute(
      'title',
      t.activityLiveTpsGateway.replace('{n}', '17'),
    );
  });

  it('leaves both live cells as em-dashes for a buffered passthrough request', () => {
    // docs/architecture/cross-cutting/telemetry-usage-observability.md §8.4.3,
    // under "Native passthrough is on this panel too": a buffered passthrough
    // response gets no progress struct at all — there are no frames to time and
    // no first-content stamp can form, so a TTFT would be the total request
    // duration wearing this column's label. The DTO therefore reports zeros with
    // an empty source, and BOTH live cells must read as not-applicable rather
    // than as a measured 0.
    render(
      <ActiveRequestsPanel
        t={t}
        active={[
          makeActive({
            id: 'act_pt_buffered',
            api_flavor: 'openai_responses',
            req_path: '/v1/responses',
            provider_path: '/v1/responses',
            stream: false,
          }),
        ]}
        effectiveScope="own"
      />,
    );

    const row = screen.getByRole('cell', { name: 'live-model' }).closest('tr')!;
    // Both cells are addressed individually rather than by counting em-dashes in the
    // row: the stream column renders an EN dash for `stream: false`, so a count would
    // hold only while two visually near-identical glyphs stay distinct — normalising
    // them, a plausible cosmetic edit, would fail here as a phantom live-cell
    // regression. The live_tps cell is the one carrying the provenance tooltip; the
    // ttft cell carries none, so it is taken positionally off its own column header
    // (ListTable renders header and body cells from the same visible-column list).
    expect(within(row).getByTitle(t.activityLiveTpsNone).textContent).toBe('—');
    const headers = screen.getAllByRole('columnheader').map((h) => h.textContent ?? '');
    const ttftIndex = headers.findIndex((h) => h.includes(t.activityColTTFT));
    expect(ttftIndex).toBeGreaterThanOrEqual(0);
    expect(within(row).getAllByRole('cell')[ttftIndex].textContent).toBe('—');
    // '0 ms' is the one shape of a zero this row can actually produce: it is what the
    // ttft cell renders if its `> 0` guard is dropped, since the DTO's ttft_ms here is
    // 0. A bare '0' is not — no visible column of this panel renders it in any state —
    // so the two em-dash assertions above are what pin the live cells.
    expect(within(row).queryByRole('cell', { name: '0 ms' })).not.toBeInTheDocument();
  });
});

// The live output-tokens column. It renders the DTO field the panel has carried
// since the passthrough bridge (`output_tokens`, already populated for
// anthropic_messages) -- there is no new wire field and no new type member here,
// only a column that shows one.
describe('ActiveRequestsPanel live output-tokens column', () => {
  it('stays hidden by default and is offered in the column menu', async () => {
    render(
      <ActiveRequestsPanel
        t={t}
        active={[makeActive({ output_tokens: 12 })]}
        effectiveScope="own"
      />,
    );

    // Hidden by default, and deliberately so. This panel renders a
    // never-measured metric as the shared em-dash, and output_tokens is 0 -- "the
    // upstream reported none" -- on most rows, so a visible count column would put
    // a SECOND em-dash cell on those rows and make five of this file's existing
    // cases ambiguous: getByRole('cell', { name: '—' }) and getByText('—') both
    // throw on more than one match, and the failure ("Found multiple elements")
    // invites the wrong repair -- loosening the query, which would retire the
    // invariant that a measured zero never renders as "0".
    expect(screen.queryByRole('columnheader', { name: t.activityColLiveOutputTokens })).toBeNull();

    // Hidden is not the same as absent: the operator who wants the count must be
    // able to switch it on, so the column menu has to offer it, unticked.
    fireEvent.click(screen.getByRole('button', { name: t.listColumns }));
    const entry = await screen.findByRole('checkbox', {
      name: t.activityColLiveOutputTokens,
    });
    expect(entry).not.toBeChecked();
  });

  it('shows the upstream count when switched on, and a zero as the shared em-dash', () => {
    // ListTable persists column visibility at `table.<storageKey>.hidden`,
    // mirrored to localStorage at `op.pref.` + key; an EMPTY hidden set is the
    // state of an operator who has switched every optional column on. Seeded
    // rather than clicked because an open MUI column menu is a Modal that
    // aria-hides the rest of the page, and getByRole skips an aria-hidden
    // subtree -- the row queries below would find nothing. vitest.setup.ts
    // clears localStorage after every test, so this leaks into none of them.
    window.localStorage.setItem('op.pref.table.op.activeRequests.hidden', JSON.stringify([]));
    render(
      <ActiveRequestsPanel
        t={t}
        active={[
          // A mid-stream openai_responses passthrough row: the count is
          // llama.cpp's own timings.predicted_n off a partial frame, and the rate
          // is derived over that exact count because the same partial reported no
          // rate of its own (the measured predicted_per_second series opens at
          // 0.0). Pinned backend-side by
          // TestPassthroughResponsesPartialPredictedNDecidesTheMidStreamCount
          // (passthrough_progress_test.go); asserted here only as a row shape.
          makeActive({
            id: 'act_live_count',
            api_flavor: 'openai_responses',
            req_path: '/v1/responses',
            provider_path: '/v1/responses',
            ttft_ms: 640,
            output_tokens: 12,
            tokens_per_second: 24.0,
            tokens_per_second_source: 'gateway',
          }),
          // The same flavor before any timings-bearing partial arrived. 0 means
          // "the upstream reported none", which is not a measurement of zero
          // tokens, so the cell must read as the shared em-dash.
          makeActive({
            id: 'act_no_count',
            model: 'live-model-2',
            api_flavor: 'openai_responses',
            req_path: '/v1/responses',
            provider_path: '/v1/responses',
            ttft_ms: 640,
            output_tokens: 0,
          }),
        ]}
        effectiveScope="own"
      />,
    );

    // Addressed by column index rather than by cell text: with every optional
    // column on, several cells in these rows are em-dashes, so a text query could
    // be satisfied by the wrong one. ListTable renders header and body cells from
    // the same visible-column list, so the index is shared.
    const headers = screen.getAllByRole('columnheader').map((h) => h.textContent ?? '');
    const countIndex = headers.findIndex((h) => h.includes(t.activityColLiveOutputTokens));
    expect(countIndex).toBeGreaterThanOrEqual(0);

    const countRow = screen.getByRole('cell', { name: 'live-model' }).closest('tr')!;
    expect(within(countRow).getAllByRole('cell')[countIndex].textContent).toBe('12');

    const noneRow = screen.getByRole('cell', { name: 'live-model-2' }).closest('tr')!;
    expect(within(noneRow).getAllByRole('cell')[countIndex].textContent).toBe('—');
  });
});
