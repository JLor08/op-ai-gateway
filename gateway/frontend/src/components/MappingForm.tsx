// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef, useState, type SubmitEvent } from 'react';
import {
  Box,
  Button,
  Checkbox,
  CircularProgress,
  FormControlLabel,
  Typography,
} from '@mui/material';
import type { ApplicationStatus, PortalModelMapping } from '../api';
import type { Translation, PortalApi } from './shared/types';
import { formatPortalError } from './shared/format';
import { applicationStatusOptions, applicationStatusLabelByKey } from './shared/application';
import { Field } from './shared/Field';
import { SelectField } from './shared/SelectField';
import { useToast } from './shared/ToastProvider';
import { pollBenchmarkStatus } from './shared/benchmark';

/**
 * Everything the mask edits, named exactly like the mapping API's fields so a
 * caller can spread it into a Create/Update body.
 *
 * The form always emits the COMPLETE object. Which of these keys a caller is
 * entitled to SEND is a call-site fact -- see `appNameReadOnly` below -- so it
 * is decided at the call site, not smuggled in here.
 */
export type MappingFormValues = {
  gateway_model_name: string;
  app_model_name: string;
  status: ApplicationStatus;
  gen_tokens_per_second: number;
  prompt_tokens_per_second: number;
  load_time_ms: number;
  context_size: number;
  energy_wh_per_token: number;
  is_mtp: boolean;
  vision_capable: boolean;
  metrics_locked: boolean;
  max_concurrency: number;
  recommended_concurrency: number;
  gen_tokens_per_second_at_capacity: number;
};

// Parse a free-text numeric input into a non-negative number; blank/invalid → 0
// (the backend treats 0 as "unknown").
const num = (s: string) => {
  const n = Number(s.trim());
  return s.trim() === '' || Number.isNaN(n) || n < 0 ? 0 : n;
};

const text = (n: number | undefined) => (n ? String(n) : '');

/**
 * The model-mapping create/edit mask, defined ONCE for the two screens that
 * offer it: `MappingSection` (an ordinary application) and
 * `RuntimeAdminSection`'s "model mapping" tab (a `server_agent` application).
 * The requirement is literally "the same edit form", and this is the half that
 * drifts SILENTLY when it is copied -- fourteen pieces of field state, their
 * hydration and a fourteen-key body; a fifteenth metric added to one copy is
 * invisible, unlike a missing column.
 *
 * One flag, `appNameReadOnly`, and it is an OWNERSHIP boundary rather than a
 * convenience -- read its comment before touching it.
 *
 * INITIALISATION is lazy from `row` and never re-synced from props: the caller
 * forces a fresh mask with `key={row?.id ?? 'create'}`. A `useEffect` sync is
 * where staleness bugs live (an in-flight edit silently reset by a background
 * list refresh), so there is deliberately none.
 */
export function MappingForm({
  t,
  api,
  serverId,
  contextProbePath,
  row,
  appNameReadOnly = false,
  busy,
  onSubmit,
  onCancel,
  pollIntervalMs,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'activeBenchmarks' | 'benchmarkStatus' | 'probeMappingContext'>;
  /** Scope of the "is this server busy" poll that gates the probe button. */
  serverId: string;
  /** The owning application's `context_probe_path`; '' disables the probe button. */
  contextProbePath: string;
  /** The mapping being edited, or null for the create form (no probe button). */
  row: PortalModelMapping | null;
  /**
   * READ-ONLY, NOT DISABLED, and it marks an ownership boundary.
   *
   * The RUNTIME SPEC owns the application model name: it is the spec's
   * `upstream_model`, the one thing `${MODEL}` expands to when the agent builds
   * the process's argv (gateway service_runtime.go folds it in; the agent's
   * ExpandPlaceholders substitutes it, and an empty one is a terminal
   * `not_permitted` at launch). Changing it while a process is running re-keys
   * the agent's upstream route to a name the live process does not serve. That
   * decision belongs behind the form that shows the args, not on a mapping tab
   * an operator visits casually -- so on that tab the field is shown (the
   * portal never warns about `${MODEL}` with an empty upstream name) but not
   * edited, and the caller OMITS it from the PATCH.
   *
   * Do not "helpfully" re-enable it. Be precise about what that buys, though:
   * the split removes the routine CLOBBER, not the race. `Service.UpdateMapping`
   * loads the row, applies the pointer fields and writes the WHOLE struct back
   * with no compare-and-set, so two PATCHes in flight at once still lose an
   * update -- the later writer reverts the earlier writer's field even though it
   * never sent that key. Omission means this form stops overwriting the spec
   * form's field on EVERY save; the residual lost update is a backend contract
   * gap, recorded in `docs/architecture/11-risks-and-technical-debt.md` §11.1.
   * Nothing server-side enforces the boundary either -- no mapping endpoint
   * special-cases `server_agent`.
   */
  appNameReadOnly?: boolean;
  busy: boolean;
  onSubmit: (values: MappingFormValues) => void;
  onCancel: () => void;
  // Benchmark status-poll cadence (ms); injectable so tests drive the loop
  // without a real 2s wait. Defaults to the shared helper's cadence.
  pollIntervalMs?: number;
}>) {
  const { showError } = useToast();
  const editing = row !== null;

  const [gatewayName, setGatewayName] = useState(() => row?.gateway_model_name ?? '');
  const [appName, setAppName] = useState(() => row?.app_model_name ?? '');
  const [status, setStatus] = useState<ApplicationStatus>(() => row?.status ?? 'active');
  const [contextSize, setContextSize] = useState(() => text(row?.context_size));
  const [energyWhPerToken, setEnergyWhPerToken] = useState(() => text(row?.energy_wh_per_token));
  const [genTps, setGenTps] = useState(() => text(row?.gen_tokens_per_second));
  const [promptTps, setPromptTps] = useState(() => text(row?.prompt_tokens_per_second));
  const [loadTimeMs, setLoadTimeMs] = useState(() => text(row?.load_time_ms));
  const [isMtp, setIsMtp] = useState(() => row?.is_mtp ?? false);
  const [visionCapable, setVisionCapable] = useState(() => row?.vision_capable ?? false);
  const [metricsLocked, setMetricsLocked] = useState(() => row?.metrics_locked ?? false);
  const [maxConcurrency, setMaxConcurrency] = useState(() => text(row?.max_concurrency));
  const [recommendedConcurrency, setRecommendedConcurrency] = useState(() =>
    text(row?.recommended_concurrency),
  );
  const [genTpsAtCapacity, setGenTpsAtCapacity] = useState(() =>
    text(row?.gen_tokens_per_second_at_capacity),
  );

  // Manual context-size probe: running state + whether this server is busy with
  // a benchmark/probe run (polled while editing so the button disables).
  const [probing, setProbing] = useState(false);
  const [serverBusy, setServerBusy] = useState(false);
  // Guards the async probe against a setState after the component unmounts mid-run.
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  // While the edit form is open, poll the running benchmarks so the probe button
  // disables whenever THIS server is busy (a benchmark OR our own probe run appears
  // here → the button stays disabled until it finishes). Mirrors the ServerList chip
  // cadence (~3s). The create form has no probe → no poll.
  const editingId = row?.id ?? '';
  useEffect(() => {
    if (!editingId) {
      setServerBusy(false);
      return;
    }
    let cancelled = false;
    const tick = () => {
      api
        .activeBenchmarks()
        .then((runs) => {
          if (!cancelled) setServerBusy(runs.some((r) => r.server_id === serverId));
        })
        .catch(() => {
          /* non-blocking — the button just falls back to its other gates */
        });
    };
    tick();
    const id = setInterval(tick, 3000);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [api, serverId, editingId]);

  // Manual context-size probe: warm-load the model + read its context via the app's
  // context_probe_path, then fill the field (no auto-save). POST → poll the benchmark
  // status to completion → read this mapping's reported context_size. Errors / 0 →
  // toast, field unchanged.
  async function probeContext(target: PortalModelMapping) {
    setProbing(true);
    try {
      await api.probeMappingContext(target.id);
      const pollStatus = await pollBenchmarkStatus(api, serverId, { intervalMs: pollIntervalMs });
      if (!mountedRef.current) return;
      const result = (pollStatus.results ?? []).find((r) => r.mapping_id === target.id);
      const ctxSize = result?.context_size ?? 0;
      if (ctxSize > 0) {
        setContextSize(String(ctxSize)); // fill only — the user saves via Save
      } else {
        showError(
          result?.error
            ? `${t.mappingProbeContextFailed}: ${result.error}`
            : t.mappingProbeContextFailed,
        );
      }
    } catch (err) {
      // 409 (already_running / server_in_use) + any poll/network failure land here.
      if (mountedRef.current) showError(formatPortalError(err, t));
    } finally {
      if (mountedRef.current) setProbing(false);
    }
  }

  function submit(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    onSubmit({
      gateway_model_name: gatewayName,
      app_model_name: appName,
      status,
      gen_tokens_per_second: num(genTps),
      prompt_tokens_per_second: num(promptTps),
      load_time_ms: num(loadTimeMs),
      context_size: num(contextSize),
      energy_wh_per_token: num(energyWhPerToken),
      is_mtp: isMtp,
      vision_capable: visionCapable,
      metrics_locked: metricsLocked,
      max_concurrency: num(maxConcurrency),
      recommended_concurrency: num(recommendedConcurrency),
      gen_tokens_per_second_at_capacity: num(genTpsAtCapacity),
    });
  }

  return (
    <Box
      component="form"
      onSubmit={submit}
      sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}
    >
      <Field
        id="mapping-gateway-name"
        label={t.mappingGatewayName}
        value={gatewayName}
        onChange={(e) => setGatewayName(e.target.value)}
        required
      />
      <Field
        id="mapping-app-name"
        label={t.mappingAppName}
        value={appName}
        // The no-op is load-bearing, not decoration: jsdom's fireEvent.change
        // fires on a readOnly input, so a live handler would still drive state
        // and give a test a false green on a write no real user can perform.
        onChange={appNameReadOnly ? () => {} : (e) => setAppName(e.target.value)}
        required
        // readOnly, never `disabled`: this field's whole job here is to be READ
        // (a spec's args are written against this name), and a readonly input is
        // barred from HTML constraint validation, so `required` cannot block
        // submit either.
        {...(appNameReadOnly
          ? { inputProps: { readOnly: true }, helperText: t.mappingAppNameReadOnly }
          : {})}
      />
      <SelectField
        id="mapping-status"
        label={t.tableStatus}
        value={status}
        onChange={(e) => setStatus(e.target.value as ApplicationStatus)}
      >
        {applicationStatusOptions.map((s) => (
          <option value={s} key={s}>
            {t[applicationStatusLabelByKey[s]]}
          </option>
        ))}
      </SelectField>
      <Box sx={{ display: 'grid', gap: 1.25 }}>
        <Typography variant="subtitle2" component="h3" sx={{ mt: 0.5 }}>
          {t.mappingMetricsSection}
        </Typography>
        <Typography variant="caption" color="text.secondary">
          {t.mappingMetricsHint}
        </Typography>
        <Field
          id="mapping-context-size"
          type="number"
          label={t.mappingContextSize}
          value={contextSize}
          onChange={(e) => setContextSize(e.target.value)}
          inputProps={{ min: 0, step: 1 }}
        />
        {row && (
          <Box>
            <Button
              type="button"
              variant="outlined"
              size="small"
              disabled={probing || serverBusy || contextProbePath.trim() === ''}
              startIcon={probing ? <CircularProgress size={16} color="inherit" /> : undefined}
              onClick={() => void probeContext(row)}
            >
              {probing ? t.mappingProbeContextRunning : t.mappingProbeContext}
            </Button>
          </Box>
        )}
        <Field
          id="mapping-energy-wh-per-token"
          type="number"
          label={t.mappingEnergyWhPerToken}
          value={energyWhPerToken}
          onChange={(e) => setEnergyWhPerToken(e.target.value)}
          inputProps={{ min: 0, step: 'any' }}
        />
        <Field
          id="mapping-gen-tps"
          type="number"
          label={t.mappingGenTokensPerSecond}
          value={genTps}
          onChange={(e) => setGenTps(e.target.value)}
          inputProps={{ min: 0, step: 'any' }}
        />
        <Field
          id="mapping-prompt-tps"
          type="number"
          label={t.mappingPromptTokensPerSecond}
          value={promptTps}
          onChange={(e) => setPromptTps(e.target.value)}
          inputProps={{ min: 0, step: 'any' }}
        />
        <Field
          id="mapping-load-ms"
          type="number"
          label={t.mappingLoadTimeMs}
          value={loadTimeMs}
          onChange={(e) => setLoadTimeMs(e.target.value)}
          inputProps={{ min: 0, step: 1 }}
        />
        <Field
          id="mapping-max-concurrency"
          type="number"
          label={t.mappingMaxConcurrency}
          value={maxConcurrency}
          onChange={(e) => setMaxConcurrency(e.target.value)}
          inputProps={{ min: 0, step: 1 }}
        />
        <Field
          id="mapping-recommended-concurrency"
          type="number"
          label={t.mappingRecommendedConcurrency}
          value={recommendedConcurrency}
          onChange={(e) => setRecommendedConcurrency(e.target.value)}
          inputProps={{ min: 0, step: 1 }}
        />
        <Field
          id="mapping-gen-tps-at-capacity"
          type="number"
          label={t.mappingGenTpsAtCapacity}
          value={genTpsAtCapacity}
          onChange={(e) => setGenTpsAtCapacity(e.target.value)}
          inputProps={{ min: 0, step: 'any' }}
        />
        <FormControlLabel
          control={<Checkbox checked={isMtp} onChange={(e) => setIsMtp(e.target.checked)} />}
          label={t.mappingIsMtp}
        />
        <FormControlLabel
          control={
            <Checkbox
              checked={visionCapable}
              onChange={(e) => setVisionCapable(e.target.checked)}
            />
          }
          label={t.mappingVisionCapable}
        />
        <FormControlLabel
          control={
            <Checkbox
              checked={metricsLocked}
              onChange={(e) => setMetricsLocked(e.target.checked)}
            />
          }
          label={t.mappingMetricsLocked}
        />
      </Box>
      <Box sx={{ display: 'flex', gap: 1.5 }}>
        <Button type="submit" variant="contained" disabled={busy}>
          {editing ? t.mappingSave : t.mappingCreate}
        </Button>
        <Button type="button" variant="text" color="secondary" onClick={onCancel}>
          {t.cancel}
        </Button>
      </Box>
    </Box>
  );
}
