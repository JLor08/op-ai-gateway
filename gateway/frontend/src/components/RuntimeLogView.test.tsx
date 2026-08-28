// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
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
  // --- the resolved command, inline with its generation's marker -----------

  it("shows the command the agent executed, attached to that generation's start marker", () => {
    const h = renderLogView();
    h.state('streaming');
    h.batch({
      spec_id: 'spec-a',
      scrollback: true,
      entries: [
        {
          event: 'started',
          pid: 4711,
          command: {
            binary: '/opt/llama/llama-server',
            args: ['--port', '54331', '--ctx-size', '262144'],
            work_dir: '/srv/models',
            env: ['PATH=/usr/bin', 'CUDA_VISIBLE_DEVICES=2,3'],
          },
        },
        { text: 'loading weights\n' },
      ],
    });

    // The marker and the command live in the same block, inside the log body:
    // the operator reads them where they are already looking, and no rule is
    // needed to say which generation the command belongs to.
    const log = screen.getByRole('log');
    expect(log).toHaveTextContent(t.runtimeLogsProcessStarted(4711));
    expect(log).toHaveTextContent(t.runtimeCommandTitle);

    // Collapsed by default: a real command is thirty-odd lines, and burying the
    // output would defeat the view.
    expect(screen.queryByText('54331')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: `▸ ${t.runtimeCommandTitle}` }));
    expect(screen.getByText('54331')).toBeInTheDocument();
    expect(screen.getByText('--ctx-size')).toBeInTheDocument();
    expect(screen.getByText('/srv/models')).toBeInTheDocument();
    expect(screen.getByText('CUDA_VISIBLE_DEVICES=2,3')).toBeInTheDocument();
    // Nothing was masked, so the block must not claim anything was.
    expect(screen.queryByText(t.runtimeCommandMasked)).not.toBeInTheDocument();
  });

  it('gives every generation in a crash loop its own command, not just the latest', () => {
    const h = renderLogView();
    h.state('streaming');
    // Three attempts, each with its own resolved ${PORT}. A single "latest
    // command" view could not show that they differed -- which is exactly the
    // kind of thing an operator is hunting for.
    h.batch({
      spec_id: 'spec-a',
      scrollback: true,
      entries: [
        { event: 'started', pid: 101, command: { binary: '/opt/a', args: ['--port', '40001'] } },
        { text: 'attempt one\n' },
        { event: 'exited', exit_code: 1 },
        { event: 'started', pid: 202, command: { binary: '/opt/a', args: ['--port', '40002'] } },
        { text: 'attempt two\n' },
        { event: 'exited', exit_code: 1 },
        { event: 'started', pid: 303, command: { binary: '/opt/a', args: ['--port', '40003'] } },
      ],
    });

    const log = screen.getByRole('log');
    expect(log).toHaveTextContent(t.runtimeLogsProcessStarted(101));
    expect(log).toHaveTextContent(t.runtimeLogsProcessStarted(303));

    const toggles = screen.getAllByRole('button', { name: `▸ ${t.runtimeCommandTitle}` });
    expect(toggles).toHaveLength(3);
    // Each one expands its OWN generation's command.
    toggles.forEach((toggle) => fireEvent.click(toggle));
    expect(screen.getByText('40001')).toBeInTheDocument();
    expect(screen.getByText('40002')).toBeInTheDocument();
    expect(screen.getByText('40003')).toBeInTheDocument();
  });

  it('renders a masked value as its own placeholder, says so in words, and offers no copy button', () => {
    const h = renderLogView();
    h.state('streaming');
    h.batch({
      spec_id: 'spec-a',
      scrollback: true,
      entries: [
        {
          event: 'started',
          pid: 4711,
          command: {
            binary: '/opt/vllm/vllm',
            args: ['--api-key', '${AGENT_ENV:HF_TOKEN}', '--port', '54331'],
            env: ['HF_TOKEN=${AGENT_ENV:HF_TOKEN}'],
            masked: true,
          },
        },
      ],
    });
    fireEvent.click(screen.getByRole('button', { name: `▸ ${t.runtimeCommandTitle}` }));

    // The mask IS the placeholder: unmistakably not a value, and it names the
    // variable the operator needs to check on the host.
    expect(screen.getByText('${AGENT_ENV:HF_TOKEN}')).toBeInTheDocument();
    expect(screen.getByText('HF_TOKEN=${AGENT_ENV:HF_TOKEN}')).toBeInTheDocument();
    expect(screen.getByText(t.runtimeCommandMasked)).toBeInTheDocument();
    // The useful half survives the mask.
    expect(screen.getByText('54331')).toBeInTheDocument();

    // No copy affordance, deliberately: even unmasked this is not a runnable
    // command line, so a copy button would promise reproduction and hand over
    // a broken paste.
    const buttons = screen.getAllByRole('button').map((b) => b.textContent ?? '');
    expect(buttons.some((label) => /kopier|copy/i.test(label))).toBe(false);
  });

  it('opens expanded for a generation that never started, because then the command is all there is', () => {
    const h = renderLogView();
    h.state('streaming');
    h.batch({
      spec_id: 'spec-a',
      scrollback: true,
      entries: [
        {
          event: 'start_failed',
          command: { binary: '/opt/not-installed', args: ['--port', '54331'] },
        },
      ],
    });

    // Its own marker kind, with no pid: a pid-0 "started" would claim output
    // begins here.
    expect(screen.getByRole('log')).toHaveTextContent(t.runtimeLogsProcessStartFailed);
    // Expanded without being asked: there is no output to bury, and nothing
    // else to look at.
    expect(screen.getByText(t.runtimeCommandStartFailedHint)).toBeInTheDocument();
    expect(screen.getByText('/opt/not-installed')).toBeInTheDocument();
  });

  it('states that a generation is missing its command rather than letting it read as "there was none"', () => {
    const h = renderLogView();
    h.state('streaming');
    // A long-running process whose opening marker has been evicted from the
    // agent's bounded buffer: output with no marker before it. The accepted cost
    // of attaching the command to a record inside the ring -- and it must be
    // said out loud, exactly like a dropped-bytes gap.
    h.batch({
      spec_id: 'spec-a',
      scrollback: true,
      entries: [{ text: '…already running\n', dropped_bytes: 4096 }, { text: 'more output\n' }],
    });
    expect(screen.getByText(t.runtimeCommandNotRetained)).toBeInTheDocument();

    // A later generation carries its own command again, and the notice goes.
    h.batch({
      spec_id: 'spec-a',
      scrollback: true,
      entries: [{ event: 'started', pid: 9, command: { binary: '/opt/a', args: [] } }],
    });
    expect(screen.queryByText(t.runtimeCommandNotRetained)).not.toBeInTheDocument();
  });

  it('states that arguments or env entries are missing rather than showing a short list as a complete one', () => {
    const h = renderLogView();
    h.state('streaming');
    h.batch({
      spec_id: 'spec-a',
      scrollback: true,
      entries: [
        {
          event: 'started',
          pid: 1,
          command: { binary: '/opt/a', args: ['--port'], truncated: true },
        },
      ],
    });
    fireEvent.click(screen.getByRole('button', { name: `▸ ${t.runtimeCommandTitle}` }));
    expect(screen.getByText(t.runtimeCommandTruncated)).toBeInTheDocument();
  });

  it('reports a work_dir the agent did not set as inherited, not as blank', () => {
    const h = renderLogView();
    h.state('streaming');
    h.batch({
      spec_id: 'spec-a',
      scrollback: true,
      entries: [{ event: 'started', pid: 1, command: { binary: '/opt/a', args: [], env: [] } }],
    });
    fireEvent.click(screen.getByRole('button', { name: `▸ ${t.runtimeCommandTitle}` }));
    expect(screen.getByText(t.runtimeCommandWorkDirInherited)).toBeInTheDocument();
    expect(screen.getByText(t.runtimeCommandArgsNone)).toBeInTheDocument();
    expect(screen.getByText(t.runtimeCommandEnvNone)).toBeInTheDocument();
  });
});
