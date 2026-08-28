// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, render, screen } from '@testing-library/react';
import { RuntimeLogView } from './RuntimeLogView';
import { messages } from '../i18n';
import type { RuntimeLogBatch, RuntimeLogState } from '../api';

const t = messages.de;

// This project's vitest setup does not auto-clean between tests (see
// vitest.setup.ts), so a previous render's DOM would otherwise still satisfy a
// queryBy* assertion about the current one.
afterEach(cleanup);

/**
 * Renders the view with a controllable subscription, so a test can drive the
 * exact frame sequence the backend produces: a `status` frame, then the
 * agent's scrollback, then live batches.
 */
function renderLogView() {
  let pushBatch: (batch: RuntimeLogBatch) => void = () => {};
  let pushState: (state: RuntimeLogState) => void = () => {};
  let unsubscribes = 0;
  const subscribedSpecIds: string[] = [];

  const api = {
    subscribeRuntimeLogs: vi.fn(
      (
        _serverId: string,
        specId: string,
        onBatch: (batch: RuntimeLogBatch) => void,
        onState: (state: RuntimeLogState) => void,
      ) => {
        subscribedSpecIds.push(specId);
        pushBatch = onBatch;
        pushState = onState;
        return () => {
          unsubscribes++;
        };
      },
    ),
  };

  const view = render(
    <RuntimeLogView
      open
      onClose={() => {}}
      api={api}
      t={t}
      serverId="srv-1"
      specId="spec-a"
      title="qwen-coder"
    />,
  );
  return {
    view,
    api,
    subscribedSpecIds,
    unsubscribes: () => unsubscribes,
    batch: (batch: RuntimeLogBatch) => act(() => pushBatch(batch)),
    state: (state: RuntimeLogState) => act(() => pushState(state)),
  };
}

describe('RuntimeLogView', () => {
  it('subscribes on open and unsubscribes on unmount -- the subscription IS the stream request', () => {
    const h = renderLogView();
    expect(h.subscribedSpecIds).toEqual(['spec-a']);
    h.view.unmount();
    // Not housekeeping: this is what tells the agent to stop streaming.
    expect(h.unsubscribes()).toBe(1);
  });

  it('shows the scrollback and then appends live output', () => {
    const h = renderLogView();
    h.state('streaming');
    h.batch({
      spec_id: 'spec-a',
      scrollback: true,
      entries: [{ text: 'loading weights\n' }],
    });
    expect(screen.getByRole('log')).toHaveTextContent('loading weights');

    h.batch({ spec_id: 'spec-a', entries: [{ text: 'CUDA error: out of memory\n' }] });
    const log = screen.getByRole('log');
    expect(log).toHaveTextContent('loading weights');
    expect(log).toHaveTextContent('CUDA error: out of memory');
  });

  it('RESETS on a second scrollback rather than duplicating the history', () => {
    // An agent reconnect delivers a fresh scrollback. Appending it would show
    // the operator every line twice and make a crash loop unreadable.
    const h = renderLogView();
    h.state('streaming');
    h.batch({ spec_id: 'spec-a', scrollback: true, entries: [{ text: 'first-line\n' }] });
    h.batch({ spec_id: 'spec-a', entries: [{ text: 'live-line\n' }] });
    h.batch({ spec_id: 'spec-a', scrollback: true, entries: [{ text: 'first-line\n' }] });

    const text = screen.getByRole('log').textContent ?? '';
    expect(text.match(/first-line/g)).toHaveLength(1);
    expect(text).not.toContain('live-line');
  });

  it('renders a dropped-bytes gap instead of showing silence', () => {
    const h = renderLogView();
    h.state('streaming');
    h.batch({
      spec_id: 'spec-a',
      scrollback: true,
      entries: [{ text: 'after the gap\n', dropped_bytes: 4096 }],
    });
    expect(screen.getByRole('log')).toHaveTextContent(t.runtimeLogsDropped(4096));
  });

  it('renders generation boundaries as portal-authored text, from the closed event set', () => {
    // The wording is ours precisely because an agent must not be able to put
    // free text where the operator reads a portal statement -- the backend
    // allow-lists the event kind for the same reason.
    const h = renderLogView();
    h.state('streaming');
    h.batch({
      spec_id: 'spec-a',
      scrollback: true,
      entries: [
        { event: 'started', pid: 4711 },
        { text: 'attempt one\n' },
        { event: 'exited', exit_code: 1 },
        { event: 'started', pid: 4802 },
        { text: 'attempt two\n' },
      ],
    });
    const log = screen.getByRole('log');
    expect(log).toHaveTextContent(t.runtimeLogsProcessStarted(4711));
    expect(log).toHaveTextContent(t.runtimeLogsProcessExited(1));
    expect(log).toHaveTextContent(t.runtimeLogsProcessStarted(4802));
    // Both attempts are present: restarts append, they do not replace.
    expect(log).toHaveTextContent('attempt one');
    expect(log).toHaveTextContent('attempt two');
  });

  it('names the reason for an empty window rather than leaving it blank', () => {
    // This is the whole point of the feature negotiation: an unexplained empty
    // window is indistinguishable from "this model prints nothing", which is
    // the question the operator opened this to answer.
    const h = renderLogView();

    // Before anything arrives: connecting, not "nothing to see".
    expect(screen.getByText(t.runtimeLogsWaiting)).toBeInTheDocument();

    h.state('unsupported');
    expect(screen.getByText(t.runtimeLogsUnsupported)).toBeInTheDocument();

    h.state('offline');
    expect(screen.getByText(t.runtimeLogsOffline)).toBeInTheDocument();

    // A connected, capable agent that delivered an EMPTY history -- what an
    // agent restart leaves behind. It must say the buffer is empty, not stay
    // silent about it.
    h.state('streaming');
    h.batch({ spec_id: 'spec-a', scrollback: true, entries: [] });
    expect(screen.getByText(t.runtimeLogsEmptyBuffer)).toBeInTheDocument();
  });

  it('stops naming a reason once real output is on screen', () => {
    const h = renderLogView();
    h.state('streaming');
    h.batch({ spec_id: 'spec-a', scrollback: true, entries: [{ text: 'output\n' }] });
    expect(screen.queryByText(t.runtimeLogsEmptyBuffer)).not.toBeInTheDocument();
    expect(screen.queryByText(t.runtimeLogsWaiting)).not.toBeInTheDocument();
  });
});
