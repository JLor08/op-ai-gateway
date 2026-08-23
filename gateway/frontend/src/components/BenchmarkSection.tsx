// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useMemo, useRef, useState } from 'react';
import {
  Alert,
  Box,
  Button,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material';
import CheckCircleIcon from '@mui/icons-material/CheckCircle';
import {
  PortalApiError,
  type BenchmarkResult,
  type BenchmarkRunDTO,
  type BenchmarkStatus,
  type PortalApplication,
  type PortalModelMapping,
  type PortalServer,
} from '../api';
import type { PortalApi, Translation } from './shared/types';
import { Panel } from './shared/Panel';
import { SelectField } from './shared/SelectField';
import { pollBenchmarkStatus } from './shared/benchmark';
import { formatPortalError } from './shared/format';

// Which slice of a server the start form is pre-scoped to. Exported so the three
// entry points (server row / app panel / mapping row — Task 4) can open the area
// scoped to what the operator clicked.
export type BenchmarkScope =
  | { kind: 'server' }
  | { kind: 'application'; id: string; name: string }
  | { kind: 'mapping'; id: string; name: string };

type ScopeKind = BenchmarkScope['kind'];
type BenchType = 'speed' | 'capacity' | 'both' | 'vision';

/** One live-result line: an error takes priority, then a vision-capability
 * probe result, then a capacity-ramp result, else the plain speed reading. */
function benchmarkResultLine(r: BenchmarkResult, t: Translation): string {
  if (r.error) return r.error;
  if (r.vision_capable !== undefined) {
    return `${t.benchmarkVision}: ${r.vision_capable ? '✓' : '✗'}`;
  }
  if (r.max_concurrency) {
    return `${t.benchmarkMaxConcurrency} ${r.max_concurrency}, ${t.benchmarkRecommendedConcurrency} ${r.recommended_concurrency}`;
  }
  return `${r.gen_tokens_per_second} tok/s, ${r.load_time_ms} ms`;
}

/**
 * The live-progress panel — MOVED verbatim from MappingSection's inline panel
 * (the done/total header + current_concurrency + one line per per-model result),
 * re-homed to read from a `status: BenchmarkStatus` prop instead of MappingSection
 * state. Renders only while `status.running`.
 */
function RunningPanel({ t, status }: Readonly<{ t: Translation; status: BenchmarkStatus }>) {
  return (
    <Box
      aria-label={t.benchmarkLive}
      sx={{
        mb: 2,
        p: 1.5,
        border: 1,
        borderColor: 'divider',
        borderRadius: 1,
        bgcolor: 'action.hover',
      }}
    >
      <Typography variant="subtitle2" component="h3">
        {t.benchmarkLive} — {t.benchmarkProgress}: {status.done}/{status.total}
        {status.current_concurrency
          ? ` — ${t.benchmarkCurrentConcurrency}: ${status.current_concurrency}`
          : ''}
      </Typography>
      <Box sx={{ display: 'grid', gap: 0.25, mt: 0.5 }}>
        {(status.results ?? []).map((r) => (
          <Typography key={r.mapping_id} variant="body2" color="text.secondary">
            {r.gateway_model_name}: {benchmarkResultLine(r, t)}
          </Typography>
        ))}
      </Box>
    </Box>
  );
}

/**
 * The benchmark-history tables — MOVED verbatim from MappingSection's history
 * dialog body (a speed table for non-capacity runs + a capacity section with a
 * per-level curve table), re-homed to read a `runs: BenchmarkRunDTO[] | null`
 * prop instead of MappingSection state: null → render nothing (not yet loaded),
 * `[]` → the "no benchmarks yet" note.
 */
function HistoryTables({ t, runs }: Readonly<{ t: Translation; runs: BenchmarkRunDTO[] | null }>) {
  if (runs === null) return null;
  if (runs.length === 0)
    return <Typography color="text.secondary">{t.benchmarkHistoryEmpty}</Typography>;
  return (
    <>
      {runs.some((r) => r.kind !== 'capacity' && r.kind !== 'vision') && (
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>{t.benchmarkRunAt}</TableCell>
              <TableCell align="right">{t.mappingGenTokensPerSecond}</TableCell>
              <TableCell align="right">{t.mappingPromptTokensPerSecond}</TableCell>
              <TableCell align="right">{t.mappingLoadTimeMs}</TableCell>
              <TableCell align="right">{t.mappingContextSize}</TableCell>
              <TableCell>{t.tableStatus}</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {runs
              .filter((r) => r.kind !== 'capacity' && r.kind !== 'vision')
              .map((run) => (
                <TableRow key={run.id}>
                  <TableCell>{new Date(run.created_at).toLocaleString()}</TableCell>
                  <TableCell align="right">{run.gen_tokens_per_second || '—'}</TableCell>
                  <TableCell align="right">{run.prompt_tokens_per_second || '—'}</TableCell>
                  <TableCell align="right">{run.load_time_ms || '—'}</TableCell>
                  <TableCell align="right">{run.context_size || '—'}</TableCell>
                  <TableCell>
                    {run.error ? (
                      <Typography variant="body2" color="error">
                        {run.error}
                      </Typography>
                    ) : (
                      <CheckCircleIcon
                        fontSize="small"
                        color="success"
                        titleAccess={t.benchmarkDone}
                      />
                    )}
                  </TableCell>
                </TableRow>
              ))}
          </TableBody>
        </Table>
      )}
      {runs.some((r) => r.kind === 'capacity') && (
        <Box sx={{ mt: 2 }}>
          <Typography variant="subtitle2" component="h3" sx={{ mb: 1 }}>
            {t.benchmarkCapacityRuns}
          </Typography>
          {runs
            .filter((r) => r.kind === 'capacity')
            .map((run) => (
              <Box
                key={run.id}
                sx={{ mb: 2, p: 1, border: 1, borderColor: 'divider', borderRadius: 1 }}
              >
                <Typography variant="body2">
                  {new Date(run.created_at).toLocaleString()}
                  {run.error ? ` — ${run.error}` : ''}
                </Typography>
                {run.capacity && (
                  <>
                    <Typography variant="body2" color="text.secondary">
                      {t.benchmarkMaxConcurrency}: {run.capacity.max_concurrency} ·{' '}
                      {t.benchmarkRecommendedConcurrency}: {run.capacity.recommended_concurrency} ·{' '}
                      {t.benchmarkGenTpsAtCapacity}:{' '}
                      {run.capacity.gen_tokens_per_second_at_capacity || '—'} ·{' '}
                      {t.benchmarkMemoryObserved}: {run.capacity.memory_observed ? '✓' : '—'}
                    </Typography>
                    {run.capacity.levels && run.capacity.levels.length > 0 && (
                      <Table size="small" sx={{ mt: 0.5 }}>
                        <TableHead>
                          <TableRow>
                            <TableCell>{t.benchmarkColConcurrency}</TableCell>
                            <TableCell align="right">{t.benchmarkColAggregateTps}</TableCell>
                            <TableCell align="right">{t.benchmarkColLatency}</TableCell>
                            <TableCell align="right">{t.benchmarkColErrors}</TableCell>
                            <TableCell>{t.benchmarkLevelStop}</TableCell>
                          </TableRow>
                        </TableHead>
                        <TableBody>
                          {run.capacity.levels.map((lv) => (
                            <TableRow key={lv.concurrency}>
                              <TableCell>{lv.concurrency}</TableCell>
                              <TableCell align="right">
                                {lv.aggregate_tokens_per_second
                                  ? lv.aggregate_tokens_per_second.toFixed(1)
                                  : '—'}
                              </TableCell>
                              <TableCell align="right">{lv.mean_latency_ms || '—'}</TableCell>
                              <TableCell align="right">{lv.errors || '—'}</TableCell>
                              <TableCell>{lv.stop_reason || '—'}</TableCell>
                            </TableRow>
                          ))}
                        </TableBody>
                      </Table>
                    )}
                  </>
                )}
              </Box>
            ))}
        </Box>
      )}
      {runs.some((r) => r.kind === 'vision') && (
        <Box sx={{ mt: 2 }}>
          <Typography variant="subtitle2" component="h3" sx={{ mb: 1 }}>
            {t.benchmarkVisionRuns}
          </Typography>
          {runs
            .filter((r) => r.kind === 'vision')
            .map((run) => (
              <Box
                key={run.id}
                sx={{ mb: 1, p: 1, border: 1, borderColor: 'divider', borderRadius: 1 }}
              >
                <Typography variant="body2">{new Date(run.created_at).toLocaleString()}</Typography>
                {run.error ? (
                  <Typography variant="body2" color="error">
                    {run.error}
                  </Typography>
                ) : (
                  <Typography variant="body2" color="text.secondary">
                    {t.benchmarkVision}: {run.vision_capable ? '✓' : '✗'}
                  </Typography>
                )}
              </Box>
            ))}
        </Box>
      )}
    </>
  );
}

/**
 * Consolidated per-server benchmark area (mirrors PerformanceSection's lifecycle:
 * subscribe on mount, interval-poll while a run is active, cleanup on unmount).
 * Three states share one panel:
 *  - RUNNING: the live-progress panel (fed by the SSE, resolved by the poll).
 *  - FREE: a start form (scope + optional app/mapping + type + Start).
 *  - HISTORY: an inline per-mapping run history + a "last completed" line.
 *
 * Resumable: on (re)mount `subscribeBenchmark` replays the in-progress run's
 * snapshot, so re-entering the area shows a run started elsewhere. The status
 * POLL — not the SSE — is the COMPLETION AUTHORITY: a dropped terminal SSE frame
 * can't leave the area stuck "running"; the poll resolves `running=false`, then
 * refreshes the models list + the shown mapping's history.
 */
export function BenchmarkSection({
  t,
  api,
  server,
  initialScope,
  onModelsChanged,
  pollIntervalMs,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'applications'
    | 'benchmarkApplication'
    | 'benchmarkMapping'
    | 'benchmarkServer'
    | 'benchmarkStatus'
    | 'mappingBenchmarks'
    | 'mappings'
    | 'subscribeBenchmark'
  >;
  server: PortalServer;
  initialScope: BenchmarkScope;
  onModelsChanged?: () => void;
  // Status-poll cadence (ms); injectable so tests drive the loop without a real
  // 2s wait. Defaults to the shared helper's cadence.
  pollIntervalMs?: number;
}>) {
  const [liveStatus, setLiveStatus] = useState<BenchmarkStatus | null>(null);
  const [starting, setStarting] = useState(false);
  const [startError, setStartError] = useState<string>('');

  const [scopeKind, setScopeKind] = useState<ScopeKind>(initialScope.kind);
  const [appId, setAppId] = useState(initialScope.kind === 'application' ? initialScope.id : '');
  const [mappingId, setMappingId] = useState(
    initialScope.kind === 'mapping' ? initialScope.id : '',
  );
  const [benchType, setBenchType] = useState<BenchType>('speed');

  const [apps, setApps] = useState<PortalApplication[]>([]);
  const [mappings, setMappings] = useState<PortalModelMapping[]>([]);
  const [history, setHistory] = useState<BenchmarkRunDTO[] | null>(null);
  const [historyMappingId, setHistoryMappingId] = useState(
    initialScope.kind === 'mapping' ? initialScope.id : '',
  );
  // Latest-wins token: a fetch only applies its result if it is still the most
  // recent request, so a slow response for a since-switched mapping can't win.
  const historyReqRef = useRef(0);
  // The mapping the history section is CURRENTLY showing (kept in a ref so the
  // completion path refreshes whatever the user is viewing now, not a value the
  // poll captured at run-start — otherwise completion would snap the picker back).
  const historyMappingIdRef = useRef(initialScope.kind === 'mapping' ? initialScope.id : '');
  // Bumped to re-arm the completion poll after it gives up (see the poll effect).
  const [pollNonce, setPollNonce] = useState(0);

  const running = Boolean(liveStatus?.running);

  // Load the shown mapping's recent runs; guarded by the latest-wins token.
  function loadHistory(id: string) {
    const token = ++historyReqRef.current;
    setHistoryMappingId(id);
    historyMappingIdRef.current = id;
    return api
      .mappingBenchmarks(id)
      .then((runs) => {
        if (historyReqRef.current === token) setHistory(runs);
      })
      .catch(() => {
        if (historyReqRef.current === token) setHistory([]);
      });
  }

  // Live frames (SSE) — resumable: on (re)mount the snapshot reflects any
  // in-progress run so re-entry shows it immediately.
  useEffect(() => {
    return api.subscribeBenchmark(server.id, setLiveStatus);
  }, [api, server.id]);

  // Completion authority: while a run is active, poll the per-server status to
  // completion. On completion set the final status, refresh the models list, and
  // refresh the shown mapping's history. A dropped terminal SSE frame is thus
  // recovered by the poll rather than leaving the area stuck "running".
  useEffect(() => {
    if (!running) return;
    let cancelled = false;
    const onDone = (final: BenchmarkStatus) => {
      if (cancelled) return;
      setLiveStatus(final);
      onModelsChanged?.();
      if (historyMappingIdRef.current) void loadHistory(historyMappingIdRef.current);
    };
    pollBenchmarkStatus(api, server.id, { intervalMs: pollIntervalMs })
      .then(onDone)
      .catch(() => {
        // The poll gave up (its ~5-min cap or a burst of consecutive errors). Re-read
        // ground truth: a run that finished while its terminal SSE frame dropped must
        // not leave us stuck showing "running". If it is genuinely still running,
        // re-arm the poll (bump the nonce → this effect re-runs → a fresh poll) so the
        // completion authority survives a long run rather than dying silently.
        if (cancelled) return;
        api
          .benchmarkStatus(server.id)
          .then((fresh) => {
            if (cancelled) return;
            if (fresh.running) {
              setLiveStatus(fresh);
              setPollNonce((n) => n + 1);
            } else {
              onDone(fresh);
            }
          })
          .catch(() => {
            if (!cancelled) setPollNonce((n) => n + 1);
          });
      });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [api, server.id, running, pollNonce]);

  // Load the server's apps once (drives the scope selectors + history picker).
  useEffect(() => {
    let cancelled = false;
    api
      .applications(server.id)
      .then((r) => {
        if (!cancelled) setApps(r.data ?? []);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [api, server.id]);

  // Load the chosen app's mappings (for the mapping scope selector + the history
  // picker default). Follows the explicit app choice, else the first app.
  useEffect(() => {
    const targetApp = appId || (apps[0]?.id ?? '');
    if (!targetApp) {
      setMappings([]);
      return;
    }
    let cancelled = false;
    api
      .mappings(targetApp)
      .then((r) => {
        if (!cancelled) setMappings(r.data ?? []);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [api, appId, apps]);

  // A mapping-scoped entry loads that mapping's history immediately on mount.
  useEffect(() => {
    if (initialScope.kind === 'mapping') void loadHistory(initialScope.id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Otherwise default the history picker to the first available mapping.
  useEffect(() => {
    if (!historyMappingId && mappings.length > 0) void loadHistory(mappings[0].id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mappings]);

  async function start() {
    setStartError('');
    setStarting(true);
    try {
      const mode = benchType;
      let initial: BenchmarkStatus;
      if (scopeKind === 'server') initial = await api.benchmarkServer(server.id, mode);
      else if (scopeKind === 'application')
        initial = await api.benchmarkApplication(appId || apps[0]?.id || '', mode);
      else initial = await api.benchmarkMapping(mappingId || mappings[0]?.id || '', mode);
      // Flip to the running view; the poll + SSE take over from here.
      setLiveStatus(initial);
    } catch (err) {
      // 409 already_running / server_in_use → inline notice (localized).
      setStartError(err instanceof PortalApiError ? formatPortalError(err, t) : String(err));
    } finally {
      setStarting(false);
    }
  }

  const lastCompleted = useMemo(() => {
    if (!history || history.length === 0) return null;
    return history[0]; // newest-first
  }, [history]);

  return (
    <Panel titleId="benchmark-heading" title={`${t.benchmarkArea} — ${server.name}`}>
      {running ? (
        <RunningPanel t={t} status={liveStatus!} />
      ) : (
        <Stack spacing={2}>
          {startError && <Alert severity="warning">{startError}</Alert>}
          <SelectField
            id="benchmark-scope"
            label={t.benchmarkScope}
            value={scopeKind}
            onChange={(e) => setScopeKind(e.target.value as ScopeKind)}
          >
            <option value="server">{t.benchmarkScopeServer}</option>
            <option value="application">{t.benchmarkScopeApplication}</option>
            <option value="mapping">{t.benchmarkScopeMapping}</option>
          </SelectField>
          {scopeKind !== 'server' && (
            <SelectField
              id="benchmark-app"
              label={t.benchmarkScopeApplication}
              value={appId || apps[0]?.id || ''}
              onChange={(e) => {
                setAppId(e.target.value);
                setMappingId('');
              }}
            >
              {apps.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.endpoint}
                </option>
              ))}
            </SelectField>
          )}
          {scopeKind === 'mapping' && (
            <SelectField
              id="benchmark-mapping"
              label={t.benchmarkScopeMapping}
              value={mappingId || mappings[0]?.id || ''}
              onChange={(e) => setMappingId(e.target.value)}
            >
              {mappings.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.gateway_model_name}
                </option>
              ))}
            </SelectField>
          )}
          <SelectField
            id="benchmark-type"
            label={t.benchmarkType}
            value={benchType}
            onChange={(e) => setBenchType(e.target.value as BenchType)}
          >
            <option value="speed">{t.benchmarkTypeSpeed}</option>
            <option value="capacity">{t.benchmarkTypeCapacity}</option>
            <option value="both">{t.benchmarkTypeBoth}</option>
            <option value="vision">{t.benchmarkTypeVision}</option>
          </SelectField>
          <Box>
            <Button variant="contained" onClick={() => void start()} disabled={starting}>
              {t.benchmarkStart}
            </Button>
          </Box>
        </Stack>
      )}

      <Box sx={{ mt: 3 }}>
        <Typography variant="subtitle2" component="h3" sx={{ mb: 1 }}>
          {t.benchmarkHistory}
        </Typography>
        {lastCompleted && (
          <Typography variant="body2" sx={{ mb: 1 }}>
            {t.benchmarkLastCompleted}: {new Date(lastCompleted.created_at).toLocaleString()}
          </Typography>
        )}
        {mappings.length > 0 && (
          <Box sx={{ mb: 1.5, maxWidth: 360 }}>
            <SelectField
              id="benchmark-history-mapping"
              label={t.benchmarkHistory}
              value={historyMappingId || mappings[0]?.id || ''}
              onChange={(e) => void loadHistory(e.target.value)}
            >
              {mappings.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.gateway_model_name}
                </option>
              ))}
            </SelectField>
          </Box>
        )}
        <HistoryTables t={t} runs={history} />
      </Box>
    </Panel>
  );
}
