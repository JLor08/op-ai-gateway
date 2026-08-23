// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { LogsView } from './LogsView';
import { messages } from '../i18n';
import type { LogRecord } from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;

function rec(over: Partial<LogRecord> = {}): LogRecord {
  return { t: '2026-07-22T12:00:00.000Z', level: 'INFO', msg: 'hello world', ...over };
}

type Captured = {
  onSnapshot?: (records: LogRecord[], level: string) => void;
  onRecord?: (record: LogRecord) => void;
  onStatus?: (s: 'open' | 'error') => void;
};

function makeApi(over: { records?: LogRecord[]; level?: string; tracingEnabled?: boolean } = {}) {
  const captured: Captured = {};
  const unsubscribe = vi.fn();
  const subscribeLogs = vi.fn(
    (
      onSnapshot: (records: LogRecord[], level: string) => void,
      onRecord: (record: LogRecord) => void,
      onStatus?: (s: 'open' | 'error') => void,
    ) => {
      captured.onSnapshot = onSnapshot;
      captured.onRecord = onRecord;
      captured.onStatus = onStatus;
      return unsubscribe;
    },
  );
  const api: Pick<
    PortalApi,
    'getTracing' | 'logs' | 'setLogLevel' | 'setTracing' | 'subscribeLogs'
  > = {
    logs: vi.fn(async () => ({ records: over.records ?? [rec()], level: over.level ?? 'info' })),
    setLogLevel: vi.fn(async (level: string) => ({ level })),
    subscribeLogs,
    getTracing: vi.fn(async () => ({
      enabled: over.tracingEnabled ?? false,
      otlp_endpoint_set: false,
    })),
    setTracing: vi.fn(async (enabled: boolean) => ({ enabled, otlp_endpoint_set: false })),
  };
  return { api, captured, unsubscribe };
}

afterEach(() => cleanup());

describe('LogsView', () => {
  it('renders seeded records from api.logs()', async () => {
    const { api } = makeApi({ records: [rec({ msg: 'seeded-line' })] });
    render(<LogsView t={t} api={api} />);
    expect(await screen.findByText('seeded-line')).toBeInTheDocument();
  });

  it('shows the empty state when there are no records', async () => {
    const { api } = makeApi({ records: [] });
    render(<LogsView t={t} api={api} />);
    expect(await screen.findByText(t.logsEmpty)).toBeInTheDocument();
  });

  it('changes the level via the dropdown and calls api.setLogLevel', async () => {
    const { api } = makeApi();
    render(<LogsView t={t} api={api} />);
    await screen.findByText('hello world');
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.logsLevelLabel }));
    fireEvent.click(await screen.findByRole('option', { name: t.logsLevelDebug }));
    expect(api.setLogLevel).toHaveBeenCalledWith('debug');
  });

  it('appends a live record pushed through the subscribe callback', async () => {
    const { api, captured } = makeApi({ records: [rec({ msg: 'first' })] });
    render(<LogsView t={t} api={api} />);
    await screen.findByText('first');
    act(() => captured.onRecord!(rec({ msg: 'live-line', level: 'WARN' })));
    expect(await screen.findByText('live-line')).toBeInTheDocument();
  });

  it('replaces records from a subscribe snapshot', async () => {
    const { api, captured } = makeApi({ records: [rec({ msg: 'initial' })] });
    render(<LogsView t={t} api={api} />);
    await screen.findByText('initial');
    act(() => captured.onSnapshot!([rec({ msg: 'snap-line' })], 'warn'));
    expect(await screen.findByText('snap-line')).toBeInTheDocument();
    expect(screen.queryByText('initial')).not.toBeInTheDocument();
  });

  it('pausing freezes the live stream (a pushed record is ignored)', async () => {
    const { api, captured } = makeApi({ records: [rec({ msg: 'before-pause' })] });
    render(<LogsView t={t} api={api} />);
    await screen.findByText('before-pause');
    // The toggle shows the current state (t.logsLive while live); clicking pauses.
    fireEvent.click(screen.getByRole('button', { name: t.logsLive }));
    act(() => captured.onRecord!(rec({ msg: 'while-paused' })));
    expect(screen.queryByText('while-paused')).not.toBeInTheDocument();
  });

  it('clears the visible records on the Clear button', async () => {
    const { api } = makeApi({ records: [rec({ msg: 'clear-me' })] });
    render(<LogsView t={t} api={api} />);
    await screen.findByText('clear-me');
    fireEvent.click(screen.getByRole('button', { name: t.logsClear }));
    expect(screen.queryByText('clear-me')).not.toBeInTheDocument();
    expect(screen.getByText(t.logsEmpty)).toBeInTheDocument();
  });

  it('unsubscribes on unmount', async () => {
    const { api, unsubscribe } = makeApi();
    const { unmount } = render(<LogsView t={t} api={api} />);
    await screen.findByText('hello world');
    unmount();
    expect(unsubscribe).toHaveBeenCalled();
  });

  it('offers trace as the first, most-verbose level option', async () => {
    const { api } = makeApi();
    render(<LogsView t={t} api={api} />);
    await screen.findByText('hello world');
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.logsLevelLabel }));
    const options = await screen.findAllByRole('option');
    expect(options[0]).toHaveTextContent(t.logsLevelTrace);
    fireEvent.click(await screen.findByRole('option', { name: t.logsLevelTrace }));
    expect(api.setLogLevel).toHaveBeenCalledWith('trace');
  });

  it('loads the tracing state via api.getTracing() on mount', async () => {
    const { api } = makeApi({ tracingEnabled: true });
    render(<LogsView t={t} api={api} />);
    expect(api.getTracing).toHaveBeenCalled();
    const toggle = await screen.findByRole('switch', { name: t.logsTracingLabel });
    expect(toggle).toBeChecked();
  });

  it('toggles tracing via api.setTracing() and reflects the response', async () => {
    const { api } = makeApi({ tracingEnabled: false });
    render(<LogsView t={t} api={api} />);
    const toggle = await screen.findByRole('switch', { name: t.logsTracingLabel });
    expect(toggle).not.toBeChecked();
    fireEvent.click(toggle);
    expect(api.setTracing).toHaveBeenCalledWith(true);
    await screen.findByRole('switch', { name: t.logsTracingLabel, checked: true });
  });
});
