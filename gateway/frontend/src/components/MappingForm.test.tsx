// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MappingForm, type MappingFormValues } from './MappingForm';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import type { BenchmarkStatus, PortalModelMapping } from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;

function makeMapping(overrides: Partial<PortalModelMapping> = {}): PortalModelMapping {
  return {
    id: 'map_1',
    application_id: 'app_1',
    gateway_model_name: 'gw-model',
    app_model_name: 'app-model',
    status: 'active',
    created_at: '2026-07-16T12:00:00Z',
    gen_tokens_per_second: 0,
    prompt_tokens_per_second: 0,
    load_time_ms: 0,
    context_size: 0,
    is_mtp: false,
    vision_capable: false,
    capabilities: [],
    energy_wh_per_token: 0,
    metrics_locked: false,
    metrics_source: '',
    metrics_updated_at: null,
    max_concurrency: 0,
    recommended_concurrency: 0,
    gen_tokens_per_second_at_capacity: 0,
    ...overrides,
  };
}

const idle: BenchmarkStatus = {
  running: false,
  server_id: 'srv_1',
  scope: 'application',
  total: 0,
  done: 0,
};

function renderForm(opts: { appNameReadOnly?: boolean; row?: PortalModelMapping | null } = {}) {
  const submitted: MappingFormValues[] = [];
  const api = {
    activeBenchmarks: vi.fn(async () => []),
    benchmarkStatus: vi.fn(async () => idle),
    probeMappingContext: vi.fn(async () => idle),
  } as unknown as Pick<PortalApi, 'activeBenchmarks' | 'benchmarkStatus' | 'probeMappingContext'>;
  const view = (nextRow: PortalModelMapping | null) => (
    <ToastProvider>
      <MappingForm
        t={t}
        api={api}
        serverId="srv_1"
        contextProbePath=""
        row={nextRow}
        appNameReadOnly={opts.appNameReadOnly}
        busy={false}
        onSubmit={(values) => submitted.push(values)}
        onCancel={() => {}}
        pollIntervalMs={0}
      />
    </ToastProvider>
  );
  const initial = opts.row === undefined ? makeMapping() : opts.row;
  const { rerender } = render(view(initial));
  // Hands the OPEN form a new `row` prop without remounting it -- what a
  // background list refresh does to a caller that passes a live row.
  return { submitted, refreshRow: (next: PortalModelMapping) => rerender(view(next)) };
}

afterEach(cleanup);

// One determined capability row, the shape the DTO's `capabilities` array
// carries.
function capRow(capability: string, verdict: 'yes' | 'no') {
  return { capability, verdict, source: 'llama_cpp_props', checked_at: '2026-07-16T12:00:00Z' };
}

// The capability controls are non-native MUI Selects (shared/SelectField).
async function pickOption(comboLabel: string, optionLabel: string) {
  fireEvent.mouseDown(screen.getByRole('combobox', { name: comboLabel }));
  fireEvent.click(await screen.findByRole('option', { name: optionLabel }));
}

async function save() {
  fireEvent.click(screen.getByRole('button', { name: t.mappingSave }));
}

describe('MappingForm capability controls', () => {
  it('offers three states and seeds each control from the capability ROW', async () => {
    // A determined "no" and a determined "yes" must seed DIFFERENTLY from
    // nothing-determined -- and the folded booleans cannot tell the first two
    // apart from the third (both are `false`), which is why the row array
    // exists. `is_mtp` is deliberately set to a value that CONTRADICTS the
    // rows here: if the control ever regressed to seeding from the boolean,
    // this fixture is what catches it.
    renderForm({
      row: makeMapping({
        is_mtp: true,
        vision_capable: true,
        capabilities: [capRow('vision', 'no')],
      }),
    });

    expect(screen.getByRole('combobox', { name: t.mappingVisionCapable }).textContent).toBe(
      t.mappingCapabilityNo,
    );
    expect(screen.getByRole('combobox', { name: t.mappingIsMtp }).textContent).toBe(
      t.mappingCapabilityUnknown,
    );

    // Three options, unknown included as a real selectable value.
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.mappingVisionCapable }));
    expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual([
      t.mappingCapabilityUnknown,
      t.mappingCapabilityYes,
      t.mappingCapabilityNo,
    ]);
    fireEvent.keyDown(document.activeElement ?? document.body, { key: 'Escape' });
  });

  it('says what UNKNOWN means PER capability, and only while unknown is selected', async () => {
    // The copy must not promise uniform re-detection. `vision` really does
    // come back from a llama.cpp /props upstream; `mtp` is never re-probed on
    // an existing mapping, so resetting it discards the (display-only, since
    // task 1 deleted the scorer's flat MTP bonus) verdict until a human sets
    // it again. One shared "let detection decide again" would be a promise
    // the gateway does not keep.
    renderForm({ row: makeMapping({ capabilities: [capRow('vision', 'yes')] }) });

    // mtp has no row -> unknown -> its own hint is on screen. vision is "yes",
    // so no hint at all.
    expect(screen.getByText(t.mappingIsMtpUnknownHint)).toBeInTheDocument();
    expect(screen.queryByText(t.mappingVisionCapableUnknownHint)).not.toBeInTheDocument();
    expect(t.mappingIsMtpUnknownHint).not.toBe(t.mappingVisionCapableUnknownHint);

    await pickOption(t.mappingVisionCapable, t.mappingCapabilityUnknown);
    expect(screen.getByText(t.mappingVisionCapableUnknownHint)).toBeInTheDocument();
  });

  it('sends an EMPTY verdict when a control moves TO unknown, and no legacy boolean', async () => {
    const { submitted } = renderForm({
      row: makeMapping({ capabilities: [capRow('vision', 'yes'), capRow('mtp', 'yes')] }),
    });
    await pickOption(t.mappingVisionCapable, t.mappingCapabilityUnknown);
    await save();

    await waitFor(() => expect(submitted).toHaveLength(1));
    // Only the moved control, and only in the authoritative field: '' asks
    // for the row to be DELETED, which is the only way back out of a `manual`
    // verdict. The UNTOUCHED control contributes no entry at all -- not a
    // value restating what the form merely read, and not an empty one either.
    expect(submitted[0].capability_verdicts).toEqual({ vision: '' });
    // Never the legacy booleans for these two: naming a capability here AND
    // sending its boolean is a 400 (two statements about one row), and a
    // boolean cannot express '' in the first place.
    expect(submitted[0]).not.toHaveProperty('vision_capable');
    expect(submitted[0]).not.toHaveProperty('is_mtp');
  });

  it('sends the verdict for a moved control and NOTHING for an untouched one', async () => {
    const { submitted } = renderForm({
      row: makeMapping({ capabilities: [capRow('vision', 'yes'), capRow('mtp', 'yes')] }),
    });
    // yes -> no is a VERDICT, not a clear: it writes a manual "no" row, just
    // as permanent as the "yes" it replaces.
    await pickOption(t.mappingVisionCapable, t.mappingCapabilityNo);
    await save();

    await waitFor(() => expect(submitted).toHaveLength(1));
    expect(submitted[0].capability_verdicts).toEqual({ vision: 'no' });
    expect(submitted[0]).not.toHaveProperty('vision_capable');
    expect(submitted[0]).not.toHaveProperty('is_mtp');
  });

  it('sends "no" for a control moved from UNKNOWN, which a boolean could not state', async () => {
    // The half of the defect that lives on this side of the wire. With
    // nothing determined the control reads unknown, and an operator picking
    // *Nein* is stating something real -- a `manual` "no" is what stops a
    // later probe from writing "yes". The old form expressed that as
    // `vision_capable: false`, which the backend compared against the
    // two-state fold of the stored rows, where a missing row is ALSO `false`:
    // no difference, nothing written, an ordinary 200, and the re-opened form
    // read unknown again. The verdict has to travel as a stated value.
    const { submitted } = renderForm({ row: makeMapping({ capabilities: [] }) });
    expect(screen.getByRole('combobox', { name: t.mappingVisionCapable }).textContent).toBe(
      t.mappingCapabilityUnknown,
    );
    await pickOption(t.mappingVisionCapable, t.mappingCapabilityNo);
    await save();

    await waitFor(() => expect(submitted).toHaveLength(1));
    expect(submitted[0].capability_verdicts).toEqual({ vision: 'no' });
    expect(submitted[0]).not.toHaveProperty('vision_capable');
  });

  it('sends neither key when a control is moved away and back to its seed', async () => {
    // The seed diff is against what the form OPENED with, not against the
    // last render: a round trip is not a change, and must not mint a manual
    // row out of a value nobody decided.
    const { submitted } = renderForm({
      row: makeMapping({ capabilities: [capRow('vision', 'yes')] }),
    });
    await pickOption(t.mappingVisionCapable, t.mappingCapabilityUnknown);
    await pickOption(t.mappingVisionCapable, t.mappingCapabilityYes);
    await save();

    await waitFor(() => expect(submitted).toHaveLength(1));
    expect(submitted[0]).not.toHaveProperty('capability_verdicts');
  });

  it('never emits an empty verdict from the CREATE form', async () => {
    // Nothing is on file for a mapping that does not exist yet, so every seed
    // is unknown and an operator who opens a control and puts it back has
    // changed nothing. A create that carried `{vision: ''}` would be asking
    // to delete a row under a mapping id that has none.
    const { submitted } = renderForm({ row: null });
    expect(screen.getByRole('combobox', { name: t.mappingVisionCapable }).textContent).toBe(
      t.mappingCapabilityUnknown,
    );
    await pickOption(t.mappingVisionCapable, t.mappingCapabilityYes);
    await pickOption(t.mappingVisionCapable, t.mappingCapabilityUnknown);
    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), { target: { value: 'gw' } });
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app' } });
    fireEvent.click(screen.getByRole('button', { name: t.mappingCreate }));

    await waitFor(() => expect(submitted).toHaveLength(1));
    expect(submitted[0]).not.toHaveProperty('capability_verdicts');
    expect(submitted[0]).not.toHaveProperty('vision_capable');
  });

  it('diffs against the value the form OPENED with, not a row refreshed under it', async () => {
    // The seed is CAPTURED at mount, exactly as ApplicationSection captures
    // proxy_excluded's. Diffing against the live `row` prop instead would let
    // a background list refresh -- or a probe writing the row while the form
    // is open -- turn an untouched control into a write: the prop would move,
    // the control would not, and the save would report the difference as an
    // operator verdict. A `manual` row outranks every probe permanently, so
    // that write is not recoverable from the portal by anything but a reset.
    const { submitted, refreshRow } = renderForm({
      row: makeMapping({ capabilities: [capRow('vision', 'yes')] }),
    });
    expect(screen.getByRole('combobox', { name: t.mappingVisionCapable }).textContent).toBe(
      t.mappingCapabilityYes,
    );

    // The row underneath now says "no" -- the operator touched nothing.
    refreshRow(makeMapping({ capabilities: [capRow('vision', 'no')] }));
    await save();

    await waitFor(() => expect(submitted).toHaveLength(1));
    expect(submitted[0]).not.toHaveProperty('capability_verdicts');
    expect(submitted[0]).not.toHaveProperty('vision_capable');
  });

  it('leaves metrics_locked a CHECKBOX', () => {
    // A policy flag over the numeric metrics, not a capability -- ADR-039 is
    // explicit that it does not guard the capability table any more, so it
    // must NOT acquire a third state along with its two neighbours.
    renderForm();
    expect(screen.getByRole('checkbox', { name: t.mappingMetricsLocked })).toBeInTheDocument();
    expect(
      screen.queryByRole('checkbox', { name: t.mappingVisionCapable }),
    ).not.toBeInTheDocument();
    expect(screen.queryByRole('checkbox', { name: t.mappingIsMtp })).not.toBeInTheDocument();
  });
});

describe('MappingForm ownership boundary', () => {
  it('leaves both model names editable by default', () => {
    renderForm();
    // BEHAVIOURAL: an ordinary application has no runtime spec, so this screen
    // owns both names and neither field may be locked.
    expect(document.querySelector('#mapping-app-name')).not.toHaveAttribute('readonly');
    expect(document.querySelector('#mapping-gateway-name')).not.toHaveAttribute('readonly');
  });

  it('locks the application model name and cannot be typed into when the spec owns it', async () => {
    const { submitted } = renderForm({ appNameReadOnly: true });

    // STRUCTURAL: the attribute is the only thing a real browser honours, and
    // jsdom will happily fire a change event on a readOnly input regardless.
    const appField = document.querySelector('#mapping-app-name');
    expect(appField).toHaveAttribute('readonly');
    expect(screen.getByText(t.mappingAppNameReadOnly)).toBeInTheDocument();
    // ...and the gateway name stays editable: the MAPPING owns that one.
    expect(document.querySelector('#mapping-gateway-name')).not.toHaveAttribute('readonly');

    // BEHAVIOURAL, and the reason the read-only field carries a no-op onChange:
    // without it this fireEvent would still drive React state and the value
    // below would be the typed one, i.e. a write no real user can perform.
    fireEvent.change(appField!, { target: { value: 'typed-by-a-test' } });
    fireEvent.click(screen.getByRole('button', { name: t.mappingSave }));

    await waitFor(() => expect(submitted).toHaveLength(1));
    expect(submitted[0].app_model_name).toBe('app-model');
  });
});
