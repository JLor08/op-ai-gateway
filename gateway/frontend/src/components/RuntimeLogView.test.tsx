// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { maxRenderedEntries, RuntimeLogView } from './RuntimeLogView';
import { messages, type Locale } from '../i18n';
import type { RuntimeLogBatch, RuntimeLogState } from '../api';

// This project's vitest setup does not auto-clean between tests (see
// vitest.setup.ts), so a previous render's DOM would otherwise still satisfy a
// queryBy* assertion about the current one.
afterEach(cleanup);

/**
 * Renders the view with a controllable subscription, so a test can drive the
 * exact frame sequence the backend produces: a `status` frame, then the
 * agent's scrollback, then live batches.
 */
for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  function renderLogView(allInstances?: boolean, instances?: readonly number[]) {
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
        allInstances={allInstances}
        instances={instances}
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

  describe(`RuntimeLogView [${locale}]`, () => {
    // #340: a replicable spec's instances share its log buffer, instance k's
    // lines prefixed [#k]; the view says so for such a spec only.
    it('names the [#k] prefix of a replicable spec, and nothing of instances otherwise', () => {
      renderLogView(true);
      expect(screen.getByText(t.runtimeLogsAllInstances)).toBeInTheDocument();
      cleanup();

      renderLogView();
      expect(screen.getByText(t.runtimeLogsIntro)).toBeInTheDocument();
      expect(screen.queryByText(t.runtimeLogsAllInstances)).not.toBeInTheDocument();
    });

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

    // --- what THIS view's own display cap discards, and in what unit ---------

    // 'ü✓😀\n' is 5 UTF-16 code units and 10 UTF-8 bytes. The fixture has to make
    // the two diverge: `text.length` and a real byte count agree on every ASCII
    // string, so an ASCII fixture would pass under either and could not tell the
    // view's counter apart from the agent's `dropped_bytes`, which is real bytes.
    const wideLine = 'ü✓😀\n';
    const wideLineBytes = 10;
    const wideLineCodeUnits = 5;

    it('counts the head a trimmed SCROLLBACK discards instead of reporting nothing', () => {
      // The agent's retained buffer is operator-sized and can be far longer than
      // this view renders. Slicing the tail off it and resetting the counter made
      // the oldest entries vanish with no counter and no notice -- the one loss
      // this view did not surface, against its own rule that every gap is stated.
      const h = renderLogView();
      h.state('streaming');
      h.batch({
        spec_id: 'spec-a',
        scrollback: true,
        entries: Array.from({ length: maxRenderedEntries + 2 }, () => ({ text: wideLine })),
      });
      expect(screen.getByText(t.runtimeLogsTrimmed(2 * wideLineBytes))).toBeInTheDocument();
    });

    it('reports the trimmed head in BYTES -- the same unit as the agent dropped_bytes beside it', () => {
      const h = renderLogView();
      h.state('streaming');
      // Exactly the cap: nothing is trimmed by this batch.
      h.batch({
        spec_id: 'spec-a',
        scrollback: true,
        entries: Array.from({ length: maxRenderedEntries }, () => ({ text: wideLine })),
      });
      // Two more arrive, so the two OLDEST (wide) entries slide off the top.
      h.batch({ spec_id: 'spec-a', entries: [{ text: 'ascii\n' }, { text: 'ascii\n' }] });

      // The code-unit sum is the number the old `text.length` accumulator
      // produced. Asserting it is absent is what makes this fixture discriminate
      // between the two units at all.
      expect(
        screen.queryByText(t.runtimeLogsTrimmed(2 * wideLineCodeUnits)),
      ).not.toBeInTheDocument();
      expect(screen.getByText(t.runtimeLogsTrimmed(2 * wideLineBytes))).toBeInTheDocument();
    });

    it('distinguishes a marker lost UPSTREAM from one this view trimmed itself', () => {
      const h = renderLogView();
      h.state('streaming');

      // (1) The agent sent its whole retained history and it begins with output:
      // the marker really is gone, and reopening will not bring it back.
      h.batch({ spec_id: 'spec-a', scrollback: true, entries: [{ text: '…already running\n' }] });
      expect(screen.getByText(t.runtimeCommandNotRetained)).toBeInTheDocument();

      // (2) A history longer than the display cap: THIS view cut the head, marker
      // included, while the agent still holds it. The trimmed notice rendered
      // directly above already says "reopen the window to reload the agent's
      // retained history in full" -- an alert next to it must not answer that
      // the agent no longer has it.
      h.batch({
        spec_id: 'spec-a',
        scrollback: true,
        entries: [
          { event: 'started', pid: 4711, command: { binary: '/opt/a', args: [] } },
          ...Array.from({ length: maxRenderedEntries + 1 }, () => ({ text: wideLine })),
        ],
      });
      expect(screen.queryByText(t.runtimeCommandNotRetained)).not.toBeInTheDocument();
      expect(screen.getByText(t.runtimeCommandTrimmedHere)).toBeInTheDocument();
    });

    it('names the file-mode env redaction as its own reason, not as the placeholder rule', () => {
      // `masked` and `env_redacted` are two different facts: a ${AGENT_ENV:NAME}
      // span replaced by its own placeholder, versus a file-mode agent
      // withholding every value the spec's own env sets. They were one flag under
      // one sentence, so a file-mode operator read HF_TOKEN=••• under an alert
      // that explains only placeholders.
      expect(typeof t.runtimeCommandEnvRedacted).toBe('string');

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
              binary: '/opt/a',
              args: ['--port', '54331'],
              env: ['HF_TOKEN=•••', 'PATH=/usr/bin'],
              env_redacted: true,
            },
          },
        ],
      });
      fireEvent.click(screen.getByRole('button', { name: `▸ ${t.runtimeCommandTitle}` }));
      expect(screen.getByText(t.runtimeCommandEnvRedacted)).toBeInTheDocument();
      // Nothing came from a placeholder here, so the placeholder sentence must
      // not be the one the operator is asked to act on.
      expect(screen.queryByText(t.runtimeCommandMasked)).not.toBeInTheDocument();
    });

    it('states BOTH reasons when a file-mode spec also resolved a placeholder into an argument', () => {
      // The file-mode env rule is additional to the placeholder rule, never a
      // replacement for it (server-agent command.go), so the two sentences stack.
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
              args: ['--api-key', '${AGENT_ENV:HF_TOKEN}'],
              env: ['HF_TOKEN=•••'],
              masked: true,
              env_redacted: true,
            },
          },
        ],
      });
      fireEvent.click(screen.getByRole('button', { name: `▸ ${t.runtimeCommandTitle}` }));
      expect(screen.getByText(t.runtimeCommandMasked)).toBeInTheDocument();
      expect(screen.getByText(t.runtimeCommandEnvRedacted)).toBeInTheDocument();
    });

    // --- per-instance tabs ---------------------------------------------------
    //
    // Every instance of a replicable spec writes into the spec's one buffer, and
    // each entry names its instance. "All" shows the interleaved history as it
    // arrives, [#k] prefixes included and instance 0's lines marked [#0]; one tab
    // per instance shows that instance's output alone, the prefix the agent put
    // there removed.

    const tabNames = () => screen.queryAllByRole('tab').map((tab) => tab.textContent);
    const logText = () => screen.getByRole('log').textContent ?? '';
    const selectTab = (name: string) => fireEvent.click(screen.getByRole('tab', { name }));

    it('shows no tabs for a single-process spec, nor for an older agent without the instance field', () => {
      const h = renderLogView();
      h.state('streaming');
      h.batch({
        spec_id: 'spec-a',
        scrollback: true,
        entries: [{ event: 'started', pid: 7 }, { text: 'plain output\n' }],
      });
      expect(screen.queryAllByRole('tab')).toHaveLength(0);
      expect(logText()).toContain('plain output');
    });

    it('offers All and one tab per instance of a replicable spec, in ascending order, All selected', () => {
      renderLogView(true, [2, 0, 1]);
      expect(tabNames()).toEqual([
        t.runtimeLogsTabAll,
        t.runtimeLogsTabInstance(0),
        t.runtimeLogsTabInstance(1),
        t.runtimeLogsTabInstance(2),
      ]);
      expect(screen.getByRole('tab', { name: t.runtimeLogsTabAll })).toHaveAttribute(
        'aria-selected',
        'true',
      );
    });

    it('names the tab list apart from the log, and links every tab to the log it filters', () => {
      renderLogView(true, [0, 1]);
      const log = screen.getByRole('log');
      const tablist = screen.getByRole('tablist');
      expect(log).toHaveAccessibleName(t.runtimeLogsTitle);
      expect(tablist).toHaveAccessibleName(t.runtimeLogsTabsLabel);
      expect(t.runtimeLogsTabsLabel).not.toBe(t.runtimeLogsTitle);
      expect(log.id).not.toBe('');
      for (const tab of screen.getAllByRole('tab')) {
        expect(tab).toHaveAttribute('aria-controls', log.id);
      }
    });

    it("shows only an instance's own output in its tab, the [#k] prefix removed, and everything in All", () => {
      const h = renderLogView(true, [0, 1]);
      h.state('streaming');
      h.batch({
        spec_id: 'spec-a',
        scrollback: true,
        entries: [
          { event: 'started', pid: 100 },
          { event: 'started', pid: 101, instance: 1, command: { binary: '/opt/one' } },
          { text: 'zero says hi\n' },
          { text: '[#1] first\n[#1] second\n', instance: 1 },
          { event: 'exited', exit_code: 3, instance: 1 },
        ],
      });

      // All: the agent's prefixes and every instance's markers included.
      expect(logText()).toContain('zero says hi');
      expect(logText()).toContain('[#1] first\n[#1] second\n');
      expect(logText()).toContain(t.runtimeLogsProcessStarted(100));
      expect(logText()).toContain(t.runtimeLogsProcessStarted(101));

      selectTab(t.runtimeLogsTabInstance(1));
      expect(logText()).toContain('first\nsecond\n');
      expect(logText()).not.toContain('[#1]');
      expect(logText()).not.toContain('zero says hi');
      expect(logText()).toContain(t.runtimeLogsProcessStarted(101));
      expect(logText()).toContain(t.runtimeLogsProcessExited(3));
      expect(logText()).not.toContain(t.runtimeLogsProcessStarted(100));
      // The marker keeps its command in its instance's tab.
      expect(
        screen.getByRole('button', { name: `▸ ${t.runtimeCommandTitle}` }),
      ).toBeInTheDocument();

      selectTab(t.runtimeLogsTabInstance(0));
      expect(logText()).toContain('zero says hi');
      expect(logText()).toContain(t.runtimeLogsProcessStarted(100));
      expect(logText()).not.toContain('first');
      expect(logText()).not.toContain(t.runtimeLogsProcessStarted(101));
    });

    it('does not strip a chunk that continues a line, even when it starts with the prefix text', () => {
      // The agent prefixes a line once, when its first byte arrives. A later
      // chunk of the same line carries no prefix of its own, so text there that
      // looks like one is the process's own output and stays.
      const h = renderLogView(true, [0, 2]);
      h.state('streaming');
      h.batch({
        spec_id: 'spec-a',
        scrollback: true,
        entries: [
          { event: 'started', pid: 102, instance: 2 },
          { text: '[#2] head ', instance: 2 },
          { text: 'interleaved zero\n' },
        ],
      });
      h.batch({
        spec_id: 'spec-a',
        entries: [{ text: '[#2] literal\n[#2] next line\n', instance: 2 }],
      });
      selectTab(t.runtimeLogsTabInstance(2));
      expect(logText()).toContain('head [#2] literal\nnext line\n');
      expect(logText()).not.toContain('interleaved zero');
    });

    it('prefixes the lines of instance 0 with [#0] in All, opening each line once, and not in its tab', () => {
      // The agent writes instance 0's bytes unprefixed. Beside the [#k] lines of
      // the others, All marks them [#0], tracking the line state the way the
      // agent's own prefixing does.
      const h = renderLogView(true, [0, 1]);
      h.state('streaming');
      h.batch({
        spec_id: 'spec-a',
        scrollback: true,
        entries: [
          { event: 'started', pid: 100 },
          { text: 'zero a\nzero b ' },
          { text: '[#1] one\n', instance: 1 },
          // Continues instance 0's open line: no prefix on its first line.
          { text: 'continued\nzero c\n' },
          { text: 'half ' },
          // A gap may have ended the line: the next chunk opens one.
          { dropped_bytes: 50 },
          { text: 'after gap\n' },
          { text: 'open ' },
          // A generation marker opens a new line too.
          { event: 'exited', exit_code: 1 },
          { event: 'started', pid: 101 },
        ],
      });
      // A blank line is a line; the empty piece after the final newline is not.
      h.batch({ spec_id: 'spec-a', entries: [{ text: 'next gen\n\n' }] });

      expect(logText()).toContain('[#0] zero a\n[#0] zero b [#1] one\ncontinued\n[#0] zero c\n');
      expect(logText()).toContain(`[#0] half \n${t.runtimeLogsDropped(50)}\n[#0] after gap\n`);
      expect(logText()).toContain('[#0] open ');
      expect(logText()).toMatch(/\[#0\] next gen\n\[#0\] \n$/);
      // Markers and gap notices are the portal's own text, never prefixed.
      expect(logText()).not.toContain('[#0] ──');
      expect(logText()).not.toContain(`[#0] ${t.runtimeLogsDropped(50)}`);

      selectTab(t.runtimeLogsTabInstance(0));
      expect(logText()).not.toContain('[#0]');
      expect(logText()).toContain('zero a\nzero b continued\nzero c\nhalf ');
      expect(logText()).toMatch(/next gen\n\n$/);

      selectTab(t.runtimeLogsTabInstance(1));
      expect(logText()).toContain('one\n');
      expect(logText()).not.toContain('[#');
    });

    it('adds no [#0] prefix for a spec whose view shows no tabs', () => {
      const h = renderLogView(false, [0]);
      h.state('streaming');
      h.batch({
        spec_id: 'spec-a',
        scrollback: true,
        entries: [{ event: 'started', pid: 7 }, { text: 'plain output\nsecond line\n' }],
      });
      expect(screen.queryAllByRole('tab')).toHaveLength(0);
      expect(logText()).toContain('plain output\nsecond line\n');
      expect(logText()).not.toContain('[#0]');
    });

    it('shows every gap in every tab, because a gap cannot be attributed to an instance', () => {
      const h = renderLogView(true, [0, 1]);
      h.state('streaming');
      h.batch({
        spec_id: 'spec-a',
        scrollback: true,
        entries: [
          { text: '[#1] after the gap\n', instance: 1, dropped_bytes: 4096 },
          { text: 'zero\n' },
        ],
      });
      h.batch({ spec_id: 'spec-a', entries: [{ dropped_bytes: 77 }] });
      for (const tab of [
        t.runtimeLogsTabAll,
        t.runtimeLogsTabInstance(0),
        t.runtimeLogsTabInstance(1),
      ]) {
        selectTab(tab);
        expect(logText()).toContain(t.runtimeLogsDropped(4096));
        expect(logText()).toContain(t.runtimeLogsDropped(77));
      }
      selectTab(t.runtimeLogsTabInstance(0));
      expect(logText()).not.toContain('after the gap');
    });

    it("states the view's own trim in every tab", () => {
      const h = renderLogView(true, [0, 1]);
      h.state('streaming');
      h.batch({
        spec_id: 'spec-a',
        scrollback: true,
        entries: Array.from({ length: maxRenderedEntries + 2 }, () => ({ text: wideLine })),
      });
      selectTab(t.runtimeLogsTabInstance(1));
      expect(screen.getByText(t.runtimeLogsTrimmed(2 * wideLineBytes))).toBeInTheDocument();
    });

    it('grows the tab set with every instance the stream names, and keeps it across a scrollback', () => {
      const h = renderLogView();
      h.state('streaming');
      h.batch({ spec_id: 'spec-a', scrollback: true, entries: [{ text: 'zero\n' }] });
      expect(screen.queryAllByRole('tab')).toHaveLength(0);
      expect(screen.queryByText(t.runtimeLogsAllInstances)).not.toBeInTheDocument();

      h.batch({ spec_id: 'spec-a', entries: [{ text: '[#3] three\n', instance: 3 }] });
      expect(tabNames()).toEqual([
        t.runtimeLogsTabAll,
        t.runtimeLogsTabInstance(0),
        t.runtimeLogsTabInstance(3),
      ]);
      // Once instances are told apart, the view also says how.
      expect(screen.getByText(t.runtimeLogsAllInstances)).toBeInTheDocument();

      // A reconnect's fresh history no longer holds instance 3; its tab stays.
      h.batch({ spec_id: 'spec-a', scrollback: true, entries: [{ text: 'zero\n' }] });
      expect(tabNames()).toContain(t.runtimeLogsTabInstance(3));
    });

    it('explains an instance tab that has no output yet instead of leaving it blank', () => {
      const h = renderLogView(true, [0, 1]);
      h.state('streaming');
      h.batch({ spec_id: 'spec-a', scrollback: true, entries: [{ text: 'zero\n' }] });
      expect(screen.queryByText(t.runtimeLogsInstanceEmpty(1))).not.toBeInTheDocument();

      selectTab(t.runtimeLogsTabInstance(1));
      expect(screen.getByText(t.runtimeLogsInstanceEmpty(1))).toBeInTheDocument();

      // A gap alone is not the instance's output: the sentence stays.
      h.batch({ spec_id: 'spec-a', entries: [{ dropped_bytes: 10 }] });
      expect(screen.getByText(t.runtimeLogsInstanceEmpty(1))).toBeInTheDocument();

      h.batch({ spec_id: 'spec-a', entries: [{ text: '[#1] up\n', instance: 1 }] });
      expect(screen.queryByText(t.runtimeLogsInstanceEmpty(1))).not.toBeInTheDocument();
    });

    it('explains instance 0 having no output even when a gap is in the history', () => {
      // A gap-only entry names no instance, and no instance means 0 on the
      // wire -- but the gap is nobody's output, instance 0's no more than any
      // other's, so it must not silence instance 0's sentence.
      const h = renderLogView(true, [0, 1]);
      h.state('streaming');
      h.batch({
        spec_id: 'spec-a',
        scrollback: true,
        entries: [{ text: '[#1] up\n', instance: 1 }],
      });
      h.batch({ spec_id: 'spec-a', entries: [{ dropped_bytes: 77 }] });

      selectTab(t.runtimeLogsTabInstance(0));
      expect(screen.getByText(t.runtimeLogsInstanceEmpty(0))).toBeInTheDocument();
      expect(screen.getByText(t.runtimeLogsDropped(77), { exact: false })).toBeInTheDocument();

      h.batch({ spec_id: 'spec-a', entries: [{ text: 'zero\n' }] });
      expect(screen.queryByText(t.runtimeLogsInstanceEmpty(0))).not.toBeInTheDocument();
    });

    it('opens on All every time, whichever tab was selected when it closed', () => {
      const api = { subscribeRuntimeLogs: vi.fn(() => () => {}) };
      const props = {
        onClose: () => {},
        api,
        t,
        serverId: 'srv-1',
        specId: 'spec-a',
        title: 'qwen-coder',
        allInstances: true,
        instances: [0, 1],
      };
      const view = render(<RuntimeLogView open {...props} />);
      selectTab(t.runtimeLogsTabInstance(1));
      expect(screen.getByRole('tab', { name: t.runtimeLogsTabInstance(1) })).toHaveAttribute(
        'aria-selected',
        'true',
      );
      view.rerender(<RuntimeLogView open={false} {...props} />);
      view.rerender(<RuntimeLogView open {...props} />);
      expect(screen.getByRole('tab', { name: t.runtimeLogsTabAll })).toHaveAttribute(
        'aria-selected',
        'true',
      );
    });

    it("opens on the opening row's instance tab every time, whichever tab was selected when it closed", () => {
      const api = { subscribeRuntimeLogs: vi.fn(() => () => {}) };
      const props = {
        onClose: () => {},
        api,
        t,
        serverId: 'srv-1',
        specId: 'spec-a',
        title: 'qwen-coder',
        allInstances: true,
        instances: [0, 1, 2],
        initialTab: 1,
      };
      const view = render(<RuntimeLogView open {...props} />);
      expect(screen.getByRole('tab', { name: t.runtimeLogsTabInstance(1) })).toHaveAttribute(
        'aria-selected',
        'true',
      );
      selectTab(t.runtimeLogsTabAll);
      view.rerender(<RuntimeLogView open={false} {...props} />);
      view.rerender(<RuntimeLogView open {...props} />);
      expect(screen.getByRole('tab', { name: t.runtimeLogsTabInstance(1) })).toHaveAttribute(
        'aria-selected',
        'true',
      );
    });

    it('opens a single-instance spec on All, so All is selected once the stream names a further instance', () => {
      // No tabs at opening: the opening row's instance tab falls back to All,
      // and stays there when tabs appear later, rather than jumping to it.
      let pushBatch: (batch: RuntimeLogBatch) => void = () => {};
      const api = {
        subscribeRuntimeLogs: vi.fn(
          (_serverId: string, _specId: string, onBatch: (batch: RuntimeLogBatch) => void) => {
            pushBatch = onBatch;
            return () => {};
          },
        ),
      };
      render(
        <RuntimeLogView
          open
          onClose={() => {}}
          api={api}
          t={t}
          serverId="srv-1"
          specId="spec-a"
          title="qwen-coder"
          instances={[0]}
          initialTab={0}
        />,
      );
      expect(screen.queryAllByRole('tab')).toHaveLength(0);

      act(() => pushBatch({ spec_id: 'spec-a', entries: [{ text: '[#3] three\n', instance: 3 }] }));
      expect(screen.getByRole('tab', { name: t.runtimeLogsTabAll })).toHaveAttribute(
        'aria-selected',
        'true',
      );
    });

    it("does not open a new generation's command block because it landed on an expanded row's index", () => {
      const h = renderLogView();
      h.state('streaming');
      // A FULL window whose last entry is a generation marker.
      h.batch({
        spec_id: 'spec-a',
        scrollback: true,
        entries: [
          ...Array.from({ length: maxRenderedEntries - 1 }, () => ({ text: 'x\n' })),
          {
            event: 'started',
            pid: 101,
            at: '2026-08-25T10:00:00Z',
            command: { binary: '/opt/a', args: ['--port', '40001'] },
          },
        ],
      });
      // The operator expands it.
      fireEvent.click(screen.getByRole('button', { name: `▸ ${t.runtimeCommandTitle}` }));
      expect(screen.getByText('40001')).toBeInTheDocument();

      // One more marker arrives, so the window slides by one and the NEW marker
      // lands on the index the expanded one held.
      h.batch({
        spec_id: 'spec-a',
        entries: [
          {
            event: 'started',
            pid: 202,
            at: '2026-08-25T10:00:05Z',
            command: { binary: '/opt/a', args: ['--port', '40002'] },
          },
        ],
      });

      // The new generation must not inherit a toggle it never held. (The
      // operator's own expansion is still lost to the slide: the row it moved to
      // is a fresh mount. That is the index key's own cost, unchanged here --
      // what this pins is that "expanded" never crosses BETWEEN generations.)
      expect(screen.queryByText('40002')).not.toBeInTheDocument();
    });
  });
}
