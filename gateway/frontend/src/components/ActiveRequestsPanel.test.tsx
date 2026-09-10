// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, render, screen, within } from '@testing-library/react';
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
// docs/superpowers/specs/2026-09-10-passthrough-progress-design.md §3. Three of
// them reach the panel for the first time with the passthrough bridge: the DTO's
// keys did not change, but these COMBINATIONS of them did not previously occur.
//
// Each pins the absence as hard as the value, in both directions, so no later
// change can satisfy a cell by counting SSE deltas as tokens: with the upstream's
// exact count asserted on the row, a delta-derived contribution shows up as a
// wrong number rather than merely as a number where there should be none.
describe('ActiveRequestsPanel native-passthrough row shapes', () => {
  it('shows a TTFT beside an em-dash rate when the stream has a first-content stamp and nothing else', () => {
    // openai_responses passthrough, mid-generation, client did NOT set
    // timings_per_token: the first content frame stamped a TTFT, the *.delta
    // partials carry no usage, and the upstream attached no timings. So: a real
    // TTFT, no count, no rate — and the rate cell must read as not-applicable,
    // never as a zero (which would read as "stalled").
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
    // Neither shape of a zero — the absence is not a measurement of nothing.
    expect(within(row).queryByRole('cell', { name: '0.0' })).not.toBeInTheDocument();
    expect(within(row).queryByRole('cell', { name: '0' })).not.toBeInTheDocument();
    expect(screen.getByText('—')).toHaveAttribute('title', t.activityLiveTpsNone);
  });

  it('shows an upstream-reported rate on a row whose token count is still zero, and claims no count', () => {
    // Same stream WITH the client's timings_per_token: llama.cpp attaches its own
    // `timings` to the partial frames, so a real rate arrives while output_tokens
    // stays 0 (the Responses partials carry no usage). "rate present, tokens
    // absent" is a legitimate row here, so the tooltip for an upstream-reported
    // rate must make no claim about a count — a "computed from 0 tokens" reading
    // would be false in exactly the case that produces this row.
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
    // The same stream's response.completed carries the upstream's own final
    // response.usage.output_tokens, which is published while the row is still
    // active — so the count and a window-derived ("gateway") rate become visible
    // for the short window before the row drops out. The tooltip is asserted
    // WHOLE against the interpolated string: the count is the upstream's exact
    // figure, and any delta-derived contribution added to it fails here.
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
    // Spec §4: a buffered passthrough response gets no progress struct at all —
    // there are no frames to time and no first-content stamp can form, so a TTFT
    // would be the total request duration wearing this column's label. The DTO
    // therefore reports zeros with an empty source, and BOTH live cells must read
    // as not-applicable rather than as a measured 0.
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
    expect(within(row).getAllByRole('cell', { name: '—' })).toHaveLength(2);
    expect(within(row).queryByRole('cell', { name: '0' })).not.toBeInTheDocument();
    expect(within(row).queryByRole('cell', { name: '0 ms' })).not.toBeInTheDocument();
  });
});
