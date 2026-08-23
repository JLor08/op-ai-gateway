// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, it, expect, afterEach, vi } from 'vitest';
import { render, screen, cleanup, waitFor } from '@testing-library/react';
import { HardwareSection } from './HardwareSection';
import { messages } from '../i18n';
import type { PortalServer, HardwareResponse } from '../api';

const t = messages.de;
afterEach(() => cleanup());

const server = { id: 'srv-1', name: 'GPU-Box' } as unknown as PortalServer;

function makeApi(resp: HardwareResponse) {
  return { serverHardware: vi.fn(async () => resp) };
}

function renderSection(api: ReturnType<typeof makeApi>) {
  return render(<HardwareSection t={t} api={api} server={server} />);
}

const populated: HardwareResponse = {
  available: true,
  collected_at: '2026-08-04T09:00:00Z',
  updated_at: '2026-08-04T09:00:00Z',
  report: {
    collected_at: '2026-08-04T09:00:00Z',
    agent_version: '1.2.3',
    os: 'ubuntu 24.04',
    arch: 'amd64',
    cpu: {
      model: 'AMD Ryzen 9',
      vendor: 'AuthenticAMD',
      physical_cores: 16,
      logical_threads: 32,
      base_mhz: 3400,
    },
    memory: {
      total_bytes: 68719476736,
      modules: [{ locator: 'DIMM0', size_bytes: 34359738368, type: 'DDR5', speed_mhz: 5600 }],
    },
    mainboard: { vendor: 'ASUS', product: 'X670E', version: '1.0' },
    bios: { vendor: 'AMI', version: '2801' },
    gpus: [
      {
        index: 0,
        name: 'RTX 4090',
        uuid: 'GPU-abc',
        driver_version: '550.1',
        memory_total_bytes: 25757220864,
      },
    ],
  },
};

describe('HardwareSection', () => {
  it('renders CPU, memory and GPU details from a populated report', async () => {
    const api = makeApi(populated);
    renderSection(api);
    await waitFor(() => expect(api.serverHardware).toHaveBeenCalledWith('srv-1'));
    expect(await screen.findByText('AMD Ryzen 9')).toBeTruthy();
    expect(screen.getByText('RTX 4090')).toBeTruthy();
    expect(screen.getByText('550.1')).toBeTruthy();
    expect(screen.getByText('DIMM0')).toBeTruthy();
  });

  it('shows an empty state when no report is available', async () => {
    const api = makeApi({ available: false });
    renderSection(api);
    await waitFor(() => expect(api.serverHardware).toHaveBeenCalled());
    expect(await screen.findByText(t.hardwareNoReport)).toBeTruthy();
  });
});
