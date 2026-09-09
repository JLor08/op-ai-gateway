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
import type { ApplicationStatus, CapabilityVerdictInput, PortalModelMapping } from '../api';
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
  /**
   * The capability verdicts the operator actually STATED: one entry per
   * control they moved -- unlike every other key here, which is always
   * emitted -- carrying the value they moved it to ('yes', 'no', or '' for
   * unknown, which DELETES the row).
   *
   * An untouched control contributes no entry at all. A capability row whose
   * source is `manual` outranks every probe and the vision benchmark for as
   * long as it stands, so restating a value this form merely READ would
   * launder an untouched control into an operator verdict.
   *
   * The legacy `is_mtp`/`vision_capable` booleans are deliberately NOT part of
   * this type: they cannot express a negative verdict or unknown (the backend
   * compares them against a two-state fold, where a stored 'no' and a missing
   * row are the same `false`), and sending one beside an entry here for the
   * same capability is a 400. The backend keeps its own
   * differs-from-what-is-stored guard behind this; both halves stay, because
   * the form's seed and the store's row can disagree -- a probe can write
   * while the form is open.
   */
  capability_verdicts?: Record<string, CapabilityVerdictInput>;
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
 * The three states of a capability control. '' is UNKNOWN and it is a real,
 * selectable value, not a placeholder: in the store, unknown is the ABSENCE of
 * a row, so there is no third verdict to write -- moving a control here
 * DELETES the row instead.
 *
 * An alias of the wire type rather than a second declaration, so the control's
 * states and the values `capability_verdicts` accepts cannot drift.
 */
type CapabilityChoice = CapabilityVerdictInput;

/**
 * What a capability control is seeded with: the matching capability ROW's
 * verdict, or '' when the mapping has no row for it.
 *
 * Deliberately NOT the folded `is_mtp`/`vision_capable` boolean, which is true
 * only for a "yes" row and so reads a determined "no" and a missing row as the
 * same `false`. Seeding from that boolean could never show unknown, which is
 * the whole state this control exists to expose.
 */
function capabilitySeed(row: PortalModelMapping | null, capability: string): CapabilityChoice {
  const verdict = row?.capabilities?.find((c) => c.capability === capability)?.verdict;
  return verdict === 'yes' || verdict === 'no' ? verdict : '';
}

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
  // Each capability control carries its VALUE and the value the form OPENED
  // with. The seed is what submit() diffs against, so "did the operator change
  // this?" stays a question about what was loaded -- mirroring
  // ApplicationSection's proxy_excluded seed, and for the same reason: a
  // background list refresh must not be able to turn an untouched control into
  // a write. Both are lazy from `row` and never re-synced (see the component's
  // own note on initialisation).
  //
  // The seed state is declared FIRST and the value state is initialised from
  // it, so `capabilitySeed` runs once per control rather than twice. Sharing
  // the computed value is safe precisely because a CapabilityChoice is an
  // immutable string -- do not extend this pattern to a control whose state is
  // an object or an array, where the two would then alias one another.
  const [isMtpSeed] = useState<CapabilityChoice>(() => capabilitySeed(row, 'mtp'));
  const [isMtp, setIsMtp] = useState<CapabilityChoice>(isMtpSeed);
  const [visionCapableSeed] = useState<CapabilityChoice>(() => capabilitySeed(row, 'vision'));
  const [visionCapable, setVisionCapable] = useState<CapabilityChoice>(visionCapableSeed);
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
    // The capability half of the body, and the ONLY part of this form that is
    // conditional. Two outcomes per control, from the seed diff:
    //
    //   unchanged -> no entry at all. This is what keeps a save made for an
    //                unrelated reason (a context-size fix) from minting a
    //                permanent `manual` row out of a value the form only ever
    //                read.
    //   moved     -> one entry holding the value it was moved TO. 'yes'/'no'
    //                is an operator verdict; '' DELETES the row, because there
    //                is no third verdict to write.
    //
    // The legacy `is_mtp`/`vision_capable` booleans are never sent for these
    // two capabilities: they cannot express a negative or unknown, and sending
    // one beside an entry for the same capability is a 400.
    //
    // On the create form every seed is '' and nothing is on file, so the ''
    // value is unreachable there by construction -- an operator who opens the
    // control and puts it back has changed nothing.
    const verdicts: Record<string, CapabilityVerdictInput> = {};
    for (const control of [
      { capability: 'mtp', value: isMtp, seed: isMtpSeed },
      { capability: 'vision', value: visionCapable, seed: visionCapableSeed },
    ] as const) {
      if (control.value === control.seed) continue;
      verdicts[control.capability] = control.value;
    }
    const capabilities: Pick<MappingFormValues, 'capability_verdicts'> =
      Object.keys(verdicts).length > 0 ? { capability_verdicts: verdicts } : {};
    onSubmit({
      gateway_model_name: gatewayName,
      app_model_name: appName,
      status,
      gen_tokens_per_second: num(genTps),
      prompt_tokens_per_second: num(promptTps),
      load_time_ms: num(loadTimeMs),
      context_size: num(contextSize),
      energy_wh_per_token: num(energyWhPerToken),
      ...capabilities,
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
        {...(appNameReadOnly ? { readOnly: true, helperText: t.mappingAppNameReadOnly } : {})}
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
        {/* THREE states, not a checkbox, and `unknown` is the empty option --
            the shared SelectField renders it as a real, selectable value with
            a permanently shrunk label, exactly as three other screens already
            do for their "follow global"/"auto" empty state. A tri-state
            checkbox exists nowhere in this portal; do not build one.

            The helper line appears only while UNKNOWN is selected, and it says
            something DIFFERENT per capability because the honest answer is
            different: vision comes back on its own from a real llama.cpp
            /props upstream, MTP never comes back at all. One uniform "let
            detection decide again" would be a promise the gateway does not
            keep. */}
        <SelectField
          id="mapping-is-mtp"
          label={t.mappingIsMtp}
          value={isMtp}
          onChange={(e) => setIsMtp(e.target.value as CapabilityChoice)}
          {...(isMtp === '' ? { helperText: t.mappingIsMtpUnknownHint } : {})}
        >
          <option value="">{t.mappingCapabilityUnknown}</option>
          <option value="yes">{t.mappingCapabilityYes}</option>
          <option value="no">{t.mappingCapabilityNo}</option>
        </SelectField>
        <SelectField
          id="mapping-vision-capable"
          label={t.mappingVisionCapable}
          value={visionCapable}
          onChange={(e) => setVisionCapable(e.target.value as CapabilityChoice)}
          {...(visionCapable === '' ? { helperText: t.mappingVisionCapableUnknownHint } : {})}
        >
          <option value="">{t.mappingCapabilityUnknown}</option>
          <option value="yes">{t.mappingCapabilityYes}</option>
          <option value="no">{t.mappingCapabilityNo}</option>
        </SelectField>
        {/* metrics_locked stays a CHECKBOX: it is a policy flag over the
            numeric metrics, not a capability, and ADR-039 is explicit that it
            does not guard the capability table at all any more. */}
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
