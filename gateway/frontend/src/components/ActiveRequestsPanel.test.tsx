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
