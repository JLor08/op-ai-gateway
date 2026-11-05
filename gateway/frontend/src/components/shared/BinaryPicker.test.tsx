// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { BinaryPicker, useServerBinaries } from './BinaryPicker';
import { messages } from '../../i18n';
import type { ServerBinary } from '../../api';

const t = messages.de;

function bin(over: Partial<ServerBinary>): ServerBinary {
  return {
    path: '/opt/llama/llama-server',
    kind: 'llama_server',
    version: 'b1234',
    version_override: '',
    effective_version: 'b1234',
    build: '',
    commit: '',
    version_source: 'agent',
    source: 'agent',
    allowed: true,
    reported_at: '',
    ...over,
  };
}

const inventory = [
  bin({}),
  bin({ path: '/opt/llama/rpc-server', kind: 'rpc_server', effective_version: 'b1234' }),
  bin({ path: '/opt/old/llama-server', allowed: false, effective_version: '' }),
];

afterEach(cleanup);

function Harness({
  kind,
  onValue,
}: {
  kind: 'llama_server' | 'rpc_server';
  onValue: (v: string) => void;
}) {
  const [value, setValue] = useState('');
  return (
    <BinaryPicker
      id="pick"
      label="Binary"
      t={t}
      kind={kind}
      value={value}
      binaries={inventory}
      onChange={(v) => {
        setValue(v);
        onValue(v);
      }}
    />
  );
}

describe('BinaryPicker', () => {
  it('offers only binaries of the wanted kind with version and allow mark', () => {
    render(<Harness kind="llama_server" onValue={() => {}} />);
    fireEvent.mouseDown(screen.getByLabelText('Binary'));
    expect(screen.getByText('/opt/llama/llama-server')).toBeTruthy();
    expect(screen.queryByText('/opt/llama/rpc-server')).toBeNull();
    expect(screen.getByText(t.binariesNotAllowed)).toBeTruthy();
    expect(screen.getByText(t.binariesVersionUnknown)).toBeTruthy();
  });

  it('fills the value from a picked option', () => {
    const onValue = vi.fn();
    render(<Harness kind="rpc_server" onValue={onValue} />);
    fireEvent.mouseDown(screen.getByLabelText('Binary'));
    fireEvent.click(screen.getByText('/opt/llama/rpc-server'));
    expect(onValue).toHaveBeenLastCalledWith('/opt/llama/rpc-server');
  });

  it('still accepts a free-text path', () => {
    const onValue = vi.fn();
    render(<Harness kind="llama_server" onValue={onValue} />);
    fireEvent.change(screen.getByLabelText('Binary'), { target: { value: '/unknown/bin' } });
    expect(onValue).toHaveBeenLastCalledWith('/unknown/bin');
  });
});

function HookProbe({ api }: { api: { serverBinaries?: () => Promise<{ data: ServerBinary[] }> } }) {
  const list = useServerBinaries(api, 'srv_1');
  return <span data-testid="n">{list.length}</span>;
}

describe('useServerBinaries', () => {
  it('loads the inventory', async () => {
    render(<HookProbe api={{ serverBinaries: () => Promise.resolve({ data: inventory }) }} />);
    await waitFor(() => expect(screen.getByTestId('n').textContent).toBe('3'));
  });

  it('is empty when the call fails or the api cannot list', async () => {
    render(<HookProbe api={{ serverBinaries: () => Promise.reject(new Error('x')) }} />);
    render(<HookProbe api={{}} />);
    await new Promise((r) => setTimeout(r, 10));
    for (const el of screen.getAllByTestId('n')) expect(el.textContent).toBe('0');
  });
});
