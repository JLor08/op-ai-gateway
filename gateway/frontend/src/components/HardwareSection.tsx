// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box, Table, TableBody, TableCell, TableHead, TableRow, Typography } from '@mui/material';
import type { HardwareReport, PortalServer } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { Panel } from './shared/Panel';
import { useLatestFetch } from './shared/useLatestFetch';

// formatBytes renders a byte count as GiB (one decimal), or "—" for 0.
function formatBytes(n: number | undefined): string {
  if (!n || n <= 0) return '—';
  return `${(n / (1024 * 1024 * 1024)).toFixed(1)} GiB`;
}

function dash(s: string | undefined): string {
  return s && s.trim() !== '' ? s : '—';
}

// A labeled key/value grid group (System / CPU / Mainboard / BIOS).
function KVGroup({ title, rows }: Readonly<{ title: string; rows: [string, string][] }>) {
  const visible = rows.filter(([, v]) => v !== '—');
  if (visible.length === 0) return null;
  return (
    <Box>
      <Typography variant="subtitle2" sx={{ mb: 0.5 }}>
        {title}
      </Typography>
      <Table size="small">
        <TableBody>
          {rows.map(([k, v]) => (
            <TableRow key={k}>
              <TableCell sx={{ color: 'text.secondary', width: '40%', border: 0, py: 0.25 }}>
                {k}
              </TableCell>
              <TableCell sx={{ border: 0, py: 0.25 }}>{v}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </Box>
  );
}

/**
 * Per-server hardware inventory sub-view. Fetches the latest static hardware report
 * ONCE on mount (mirrors AvailabilitySection minus the polling — hardware is static).
 * Tolerant of missing fields (renders "—" / omits an empty group). Leaf: `{ t, api,
 * server }`.
 */
export function HardwareSection({
  t,
  api,
  server,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'serverHardware'>;
  server: PortalServer;
}>) {
  const hardware = useLatestFetch(() => api.serverHardware(server.id), [api, server.id]);
  const pending = hardware.status === 'idle' || hardware.status === 'loading';
  // A load failure and an available:false/no-report response both render the
  // same "no report" panel (hardware.data stays null on error, never
  // overwriting whatever the last successful load produced -- see
  // useLatestFetch's latest-wins guard).
  const report: HardwareReport | null =
    hardware.data?.available && hardware.data.report ? hardware.data.report : null;
  const collectedAt = report ? (hardware.data?.collected_at ?? report.collected_at ?? '') : '';

  if (pending) {
    return (
      <Panel titleId="hardware-heading" title={server.name}>
        <Typography color="text.secondary">{t.loading}</Typography>
      </Panel>
    );
  }
  if (!report) {
    return (
      <Panel titleId="hardware-heading" title={server.name}>
        <Typography color="text.secondary">{t.hardwareNoReport}</Typography>
      </Panel>
    );
  }

  const collected = collectedAt ? new Date(collectedAt).toLocaleString() : '—';

  return (
    <Panel titleId="hardware-heading" title={server.name}>
      <Box
        sx={{
          display: 'grid',
          gridTemplateColumns: { xs: '1fr', md: 'repeat(2, minmax(0, 1fr))' },
          gap: 2,
        }}
      >
        <KVGroup
          title={t.hardwareSystem}
          rows={[
            [t.hardwareOs, dash(report.os)],
            [t.hardwareKernel, dash(report.kernel)],
            [t.hardwareArch, dash(report.arch)],
            [t.hardwareHostname, dash(report.hostname)],
            [t.hardwareAgentVersion, dash(report.agent_version)],
            [t.hardwareCollectedAt, collected],
          ]}
        />
        <KVGroup
          title={t.hardwareCpu}
          rows={[
            [t.hardwareCpuModel, dash(report.cpu?.model)],
            [t.hardwareCpuVendor, dash(report.cpu?.vendor)],
            [
              t.hardwareCpuCores,
              report.cpu?.physical_cores ? String(report.cpu.physical_cores) : '—',
            ],
            [
              t.hardwareCpuThreads,
              report.cpu?.logical_threads ? String(report.cpu.logical_threads) : '—',
            ],
            [t.hardwareBaseClock, report.cpu?.base_mhz ? `${report.cpu.base_mhz} MHz` : '—'],
          ]}
        />
        <KVGroup
          title={t.hardwareMainboard}
          rows={[
            [t.hardwareVendor, dash(report.mainboard?.vendor)],
            [t.hardwareProduct, dash(report.mainboard?.product)],
            [t.hardwareVersion, dash(report.mainboard?.version)],
          ]}
        />
        <KVGroup
          title={t.hardwareBios}
          rows={[
            [t.hardwareVendor, dash(report.bios?.vendor)],
            [t.hardwareVersion, dash(report.bios?.version)],
          ]}
        />
      </Box>

      <Box sx={{ mt: 2 }}>
        <Typography variant="subtitle2" sx={{ mb: 0.5 }}>
          {t.hardwareMemory}
        </Typography>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>
          {t.hardwareRamTotal}: {formatBytes(report.memory?.total_bytes)}
        </Typography>
        {report.memory?.modules && report.memory.modules.length > 0 ? (
          <Box sx={{ overflowX: 'auto' }}>
            <Table size="small">
              <TableHead>
                <TableRow>
                  <TableCell>{t.hardwareDimmLocator}</TableCell>
                  <TableCell>{t.hardwareDimmSize}</TableCell>
                  <TableCell>{t.hardwareDimmType}</TableCell>
                  <TableCell>{t.hardwareDimmSpeed}</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {report.memory.modules.map((m, i) => (
                  <TableRow key={`${m.locator ?? 'dimm'}-${i}`}>
                    <TableCell>{dash(m.locator)}</TableCell>
                    <TableCell>{formatBytes(m.size_bytes)}</TableCell>
                    <TableCell>{dash(m.type)}</TableCell>
                    <TableCell>{m.speed_mhz ? `${m.speed_mhz} MHz` : '—'}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Box>
        ) : (
          <Typography variant="body2" color="text.secondary">
            {t.hardwareNoModules}
          </Typography>
        )}
      </Box>

      {report.gpus && report.gpus.length > 0 && (
        <Box sx={{ mt: 2 }}>
          <Typography variant="subtitle2" sx={{ mb: 0.5 }}>
            {t.hardwareGpus}
          </Typography>
          <Box sx={{ overflowX: 'auto' }}>
            <Table size="small">
              <TableHead>
                <TableRow>
                  <TableCell>{t.hardwareGpuIndex}</TableCell>
                  <TableCell>{t.hardwareGpuName}</TableCell>
                  <TableCell>{t.hardwareGpuVram}</TableCell>
                  <TableCell>{t.hardwareGpuDriver}</TableCell>
                  <TableCell>{t.hardwareGpuUuid}</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {report.gpus.map((g) => (
                  <TableRow key={`${g.index}-${g.name}`}>
                    <TableCell>{g.index}</TableCell>
                    <TableCell>{dash(g.name)}</TableCell>
                    <TableCell>{formatBytes(g.memory_total_bytes)}</TableCell>
                    <TableCell>{dash(g.driver_version)}</TableCell>
                    <TableCell>{dash(g.uuid)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Box>
        </Box>
      )}
    </Panel>
  );
}
