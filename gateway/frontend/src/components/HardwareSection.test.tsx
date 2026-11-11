// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, it, expect, afterEach, vi } from 'vitest';
import { act, render, screen, cleanup, fireEvent, waitFor, within } from '@testing-library/react';
import { HardwareSection } from './HardwareSection';
import { messages, type Locale } from '../i18n';
import type { PortalServer, HardwareResponse } from '../api';

afterEach(() => cleanup());

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

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

  describe(`HardwareSection [${locale}]`, () => {
    it('renders CPU, memory and GPU details from a populated report', async () => {
      const api = makeApi(populated);
      renderSection(api);
      await waitFor(() => expect(api.serverHardware).toHaveBeenCalledWith('srv-1'));
      expect(await screen.findByText('AMD Ryzen 9')).toBeTruthy();
      expect(screen.getByText('RTX 4090')).toBeTruthy();
      expect(screen.getByText('550.1')).toBeTruthy();
      expect(screen.getByText('DIMM0')).toBeTruthy();
    });

    it('shows the agent clock offset next to the agent version', async () => {
      const api = makeApi(populated);
      render(
        <HardwareSection
          t={t}
          api={api}
          server={{ ...server, clock_offset_ms: 120 } as PortalServer}
        />,
      );
      expect(await screen.findByText(t.agentClockOffset)).toBeTruthy();
      expect(screen.getByTestId('agent-clock-offset')).toHaveTextContent('+120 ms');
    });

    it('shows an empty state when no report is available', async () => {
      const api = makeApi({ available: false });
      renderSection(api);
      await waitFor(() => expect(api.serverHardware).toHaveBeenCalled());
      expect(await screen.findByText(t.hardwareNoReport)).toBeTruthy();
    });
  });
}

describe('HardwareSection agent clock offset', () => {
  const t = messages.de;

  function renderWithOffset(clock_offset_ms: number | null | undefined) {
    const api = { serverHardware: vi.fn(async () => populatedReport()) };
    return render(
      <HardwareSection
        t={t}
        api={api}
        server={{ id: 'srv-1', name: 'GPU-Box', clock_offset_ms } as unknown as PortalServer}
      />,
    );
  }

  it.each([
    [120, '+120 ms'],
    [-80, '-80 ms'],
    [0, '0 ms'],
    [-2500, '-2500 ms'],
  ])('shows an offset of %d ms signed as %s', async (ms, text) => {
    renderWithOffset(ms);
    expect(await screen.findByTestId('agent-clock-offset')).toHaveTextContent(text);
  });

  it('flags an offset above 2 s in either direction and leaves a small one plain', async () => {
    renderWithOffset(2500);
    expect(await screen.findByTestId('agent-clock-offset')).toHaveAttribute('data-warning', 'true');
    cleanup();
    renderWithOffset(-2001);
    expect(await screen.findByTestId('agent-clock-offset')).toHaveAttribute('data-warning', 'true');
    cleanup();
    renderWithOffset(2000);
    expect(await screen.findByTestId('agent-clock-offset')).toHaveAttribute(
      'data-warning',
      'false',
    );
  });

  it('says the offset is unknown until the agent reported after a gateway start', async () => {
    renderWithOffset(null);
    expect(await screen.findByTestId('agent-clock-offset')).toHaveTextContent(
      t.agentClockOffsetUnknown,
    );
    cleanup();
    renderWithOffset(undefined);
    expect(await screen.findByTestId('agent-clock-offset')).toHaveTextContent(
      t.agentClockOffsetUnknown,
    );
  });

  it('marks a flagged offset with a named warning icon, not by colour alone', async () => {
    renderWithOffset(2500);
    const offset = await screen.findByTestId('agent-clock-offset');
    expect(within(offset).getByRole('img', { name: t.agentClockOffsetWarnLabel })).toBeTruthy();
    cleanup();
    renderWithOffset(120);
    const plain = await screen.findByTestId('agent-clock-offset');
    expect(within(plain).queryByRole('img')).toBeNull();
  });

  it('opens the diagnostic tooltip from the keyboard', async () => {
    renderWithOffset(120);
    const offset = await screen.findByTestId('agent-clock-offset');
    expect(offset).toHaveAttribute('tabindex', '0');
    fireEvent.keyDown(document.body, { key: 'Tab' });
    act(() => offset.focus());
    expect(await screen.findByRole('tooltip')).toHaveTextContent(t.agentClockOffsetHelp);
  });

  it('explains in a tooltip that the offset is a diagnostic, and why a large one matters', async () => {
    renderWithOffset(120);
    fireEvent.mouseOver(await screen.findByTestId('agent-clock-offset'));
    const tip = await screen.findByRole('tooltip');
    expect(tip).toHaveTextContent(t.agentClockOffsetHelp);
    expect(tip).not.toHaveTextContent(t.agentClockOffsetWarn);
    cleanup();
    renderWithOffset(3000);
    fireEvent.mouseOver(await screen.findByTestId('agent-clock-offset'));
    expect(await screen.findByRole('tooltip')).toHaveTextContent(t.agentClockOffsetWarn);
  });
});

// A minimal available report: the System group (and with it the offset row)
// renders only for one.
function populatedReport(): HardwareResponse {
  return {
    available: true,
    collected_at: '2026-08-04T09:00:00Z',
    report: {
      collected_at: '2026-08-04T09:00:00Z',
      agent_version: '1.2.3',
      os: 'linux',
      arch: 'amd64',
      cpu: {},
      memory: { total_bytes: 0, modules: [] },
      mainboard: {},
      bios: {},
      gpus: [],
    },
  } as unknown as HardwareResponse;
}
