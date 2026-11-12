// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MappingForm, type MappingFormValues } from './MappingForm';
import { ToastProvider } from './shared/ToastProvider';
import { messages, type Locale } from '../i18n';
import type { BenchmarkStatus, PortalModelMapping } from '../api';
import type { PortalApi } from './shared/types';

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

// Both locales, not just German (issue #89): run the whole suite once per locale
// so the component's English strings get the same real rendering assertions.
for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  function renderForm(
    opts: {
      appNameReadOnly?: boolean;
      row?: PortalModelMapping | null;
      contextProbePath?: string;
      contextProbeDerived?: boolean;
      benchmarkStatus?: () => Promise<BenchmarkStatus>;
    } = {},
  ) {
    const submitted: MappingFormValues[] = [];
    const api = {
      activeBenchmarks: vi.fn(async () => []),
      benchmarkStatus: vi.fn(opts.benchmarkStatus ?? (async () => idle)),
      probeMappingContext: vi.fn(async () => idle),
    } as unknown as Pick<PortalApi, 'activeBenchmarks' | 'benchmarkStatus' | 'probeMappingContext'>;
    const view = (nextRow: PortalModelMapping | null) => (
      <ToastProvider>
        <MappingForm
          t={t}
          api={api}
          serverId="srv_1"
          contextProbePath={opts.contextProbePath ?? ''}
          contextProbeDerived={opts.contextProbeDerived}
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

  describe(`MappingForm capability controls [${locale}]`, () => {
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

    it('sends the image verdict when the control is moved to yes', async () => {
      const { submitted } = renderForm({ row: makeMapping({ capabilities: [] }) });
      await pickOption(t.mappingImageCapable, t.mappingCapabilityYes);
      await save();

      await waitFor(() => expect(submitted).toHaveLength(1));
      expect(submitted[0].capability_verdicts).toEqual({ image: 'yes' });
    });

    it('sends an empty image verdict when the control moves back to unknown', async () => {
      const { submitted } = renderForm({
        row: makeMapping({ capabilities: [capRow('image', 'yes')] }),
      });
      await pickOption(t.mappingImageCapable, t.mappingCapabilityUnknown);
      await save();

      await waitFor(() => expect(submitted).toHaveLength(1));
      // '' asks for the row to be DELETED, which is the only way back out of a
      // `manual` verdict.
      expect(submitted[0].capability_verdicts).toEqual({ image: '' });
    });

    it('does not offer a "no" option for image, because the backend refuses it', async () => {
      renderForm({ row: makeMapping({ capabilities: [] }) });
      fireEvent.mouseDown(screen.getByRole('combobox', { name: t.mappingImageCapable }));
      expect(
        await screen.findByRole('option', { name: t.mappingCapabilityUnknown }),
      ).toBeInTheDocument();
      expect(screen.getByRole('option', { name: t.mappingCapabilityYes })).toBeInTheDocument();
      // reservedManualVerdicts refuses (image, no): a manual no outranks every
      // automated source forever and would mask the sd-server capability
      // probe. A control that always 400s is worse than an absent one.
      expect(screen.queryByRole('option', { name: t.mappingCapabilityNo })).not.toBeInTheDocument();
    });

    it('shows an image unknown hint of its own', async () => {
      renderForm({ row: makeMapping({ capabilities: [] }) });
      expect(screen.getByText(t.mappingImageCapableUnknownHint)).toBeInTheDocument();
      // Image has its own automated writer (sd-server's capability document),
      // reached for a different population than vision's /props probe, so the
      // two hints must not share one text.
      expect(t.mappingImageCapableUnknownHint).not.toBe(t.mappingVisionCapableUnknownHint);
    });

    it("shows a probe's image no, without offering it as a choice", async () => {
      // The sdcpp_capabilities probe is the one writer entitled to state
      // image: no. The control must SHOW that verdict -- a Select whose value
      // has no option renders blank, which reads as "nothing on file" -- while
      // still never offering "no" as a pick, since the backend refuses a manual
      // one.
      const { submitted } = renderForm({
        row: makeMapping({
          capabilities: [
            {
              capability: 'image',
              verdict: 'no',
              source: 'sdcpp_capabilities',
              checked_at: '2026-09-25T12:00:00Z',
            },
          ],
        }),
      });
      const combo = screen.getByRole('combobox', { name: t.mappingImageCapable });
      expect(combo.textContent).toBe(t.mappingCapabilityNo);

      fireEvent.mouseDown(combo);
      const no = await screen.findByRole('option', { name: t.mappingCapabilityNo });
      expect(no).toHaveAttribute('aria-disabled', 'true');
      fireEvent.keyDown(screen.getByRole('listbox'), { key: 'Escape' });

      // Untouched, it is not sent: the probe's row stays the probe's.
      await save();
      await waitFor(() => expect(submitted).toHaveLength(1));
      expect(submitted[0]).not.toHaveProperty('capability_verdicts');
    });

    // image_edit is the operator's OPT-OUT from a capability the gateway derives
    // from image (image:yes and no image_edit:no row). `unknown` therefore means
    // "derived from image" and `no` is the opt-out; `yes` is inert (the gate
    // derives it), so it is not a choice. This is the mirror image of the image
    // control above, which offers yes but never no.
    it('offers unknown and no for image_edit, and never yes -- the gate derives it', async () => {
      renderForm({ row: makeMapping({ capabilities: [] }) });
      fireEvent.mouseDown(screen.getByRole('combobox', { name: t.mappingImageEdit }));
      expect(
        await screen.findByRole('option', { name: t.mappingCapabilityUnknown }),
      ).toBeInTheDocument();
      expect(screen.getByRole('option', { name: t.mappingCapabilityNo })).toBeInTheDocument();
      expect(
        screen.queryByRole('option', { name: t.mappingCapabilityYes }),
      ).not.toBeInTheDocument();
    });

    it('sends the image_edit opt-out and nothing for image when moved to no', async () => {
      const { submitted } = renderForm({
        row: makeMapping({ capabilities: [capRow('image', 'yes')] }),
      });
      await pickOption(t.mappingImageEdit, t.mappingCapabilityNo);
      await save();

      await waitFor(() => expect(submitted).toHaveLength(1));
      // Only the control that moved is sent: the opt-out removes edit, it never
      // touches the image verdict beside it.
      expect(submitted[0].capability_verdicts).toEqual({ image_edit: 'no' });
    });

    it('seeds image_edit from the capability ROW and resets it with an empty verdict', async () => {
      const { submitted } = renderForm({
        row: makeMapping({ capabilities: [capRow('image', 'yes'), capRow('image_edit', 'no')] }),
      });
      expect(screen.getByRole('combobox', { name: t.mappingImageEdit }).textContent).toBe(
        t.mappingCapabilityNo,
      );
      await pickOption(t.mappingImageEdit, t.mappingCapabilityUnknown);
      await save();

      await waitFor(() => expect(submitted).toHaveLength(1));
      // '' deletes the row, which is how the opt-out is taken back: the mapping
      // derives edit from image again.
      expect(submitted[0].capability_verdicts).toEqual({ image_edit: '' });
    });

    it('sends nothing for an untouched image_edit control', async () => {
      const { submitted } = renderForm({
        row: makeMapping({ capabilities: [capRow('image_edit', 'no')] }),
      });
      await save();

      await waitFor(() => expect(submitted).toHaveLength(1));
      // Re-stating a verdict the form merely READ would launder it into a fresh
      // manual row; an untouched control contributes no entry.
      expect(submitted[0]).not.toHaveProperty('capability_verdicts');
    });

    it('shows an image_edit unknown hint of its own, and only while it is unknown', async () => {
      renderForm({ row: makeMapping({ capabilities: [] }) });
      expect(screen.getByText(t.mappingImageEditUnknownHint)).toBeInTheDocument();
      // The edit hint says "derived from image"; it must not be mistaken for the
      // image control's own detection text.
      expect(t.mappingImageEditUnknownHint).not.toBe(t.mappingImageCapableUnknownHint);
      cleanup();
      renderForm({ row: makeMapping({ capabilities: [capRow('image_edit', 'no')] }) });
      expect(screen.queryByText(t.mappingImageEditUnknownHint)).not.toBeInTheDocument();
    });

    it('shows a stored inert image_edit yes without offering it as a pick', async () => {
      // A yes row can exist (the vocabulary is open) but adds nothing: the gate
      // derives it. The control must SHOW it -- a Select whose value has no
      // option renders blank, which reads as nothing on file -- while never
      // offering yes as a choice. Moving it to unknown deletes the row.
      const { submitted } = renderForm({
        row: makeMapping({ capabilities: [capRow('image_edit', 'yes')] }),
      });
      const combo = screen.getByRole('combobox', { name: t.mappingImageEdit });
      expect(combo.textContent).toBe(t.mappingCapabilityYes);
      fireEvent.mouseDown(combo);
      const yes = await screen.findByRole('option', { name: t.mappingCapabilityYes });
      expect(yes).toHaveAttribute('aria-disabled', 'true');
      fireEvent.keyDown(screen.getByRole('listbox'), { key: 'Escape' });

      await save();
      await waitFor(() => expect(submitted).toHaveLength(1));
      expect(submitted[0]).not.toHaveProperty('capability_verdicts');
    });

    // speech and transcription are the audio twins of the image control: yes is
    // the operator's override of the automatic audio.cpp /v1/models probe (and
    // the fallback for an older agent or an unlisted model), and a selectable no
    // is never offered because (speech, no) and (transcription, no) are
    // reserved -- a manual no would outrank, and so permanently mask, that
    // probe. The probe's own no is shown, disabled.
    for (const control of [
      {
        capability: 'speech',
        label: t.mappingSpeechCapable,
        hint: t.mappingSpeechCapableUnknownHint,
      },
      {
        capability: 'transcription',
        label: t.mappingTranscriptionCapable,
        hint: t.mappingTranscriptionCapableUnknownHint,
      },
    ] as const) {
      const { capability, label, hint } = control;

      it(`offers unknown and yes for ${capability}, never a selectable no`, async () => {
        renderForm({ row: makeMapping({ capabilities: [] }) });
        fireEvent.mouseDown(screen.getByRole('combobox', { name: label }));
        expect(
          await screen.findByRole('option', { name: t.mappingCapabilityUnknown }),
        ).toBeInTheDocument();
        expect(screen.getByRole('option', { name: t.mappingCapabilityYes })).toBeInTheDocument();
        expect(
          screen.queryByRole('option', { name: t.mappingCapabilityNo }),
        ).not.toBeInTheDocument();
      });

      it(`sends the ${capability} verdict when the control is moved to yes`, async () => {
        const { submitted } = renderForm({ row: makeMapping({ capabilities: [] }) });
        await pickOption(label, t.mappingCapabilityYes);
        await save();

        await waitFor(() => expect(submitted).toHaveLength(1));
        expect(submitted[0].capability_verdicts).toEqual({ [capability]: 'yes' });
      });

      it(`sends nothing for an untouched ${capability} control`, async () => {
        const { submitted } = renderForm({ row: makeMapping({ capabilities: [] }) });
        await save();

        await waitFor(() => expect(submitted).toHaveLength(1));
        expect(submitted[0]).not.toHaveProperty('capability_verdicts');
      });

      it(`shows a probe's ${capability} no as a disabled option, without offering it`, async () => {
        // The audio.cpp /v1/models probe is the one writer entitled to state
        // no. The control must SHOW that verdict -- a Select whose value has no
        // option renders blank, which reads as unknown -- while never offering
        // no as a pick.
        const { submitted } = renderForm({
          row: makeMapping({
            capabilities: [
              {
                capability,
                verdict: 'no',
                source: 'audiocpp_models',
                checked_at: '2026-09-25T12:00:00Z',
              },
            ],
          }),
        });
        const combo = screen.getByRole('combobox', { name: label });
        expect(combo.textContent).toBe(t.mappingCapabilityNo);

        fireEvent.mouseDown(combo);
        const no = await screen.findByRole('option', { name: t.mappingCapabilityNo });
        expect(no).toHaveAttribute('aria-disabled', 'true');
        fireEvent.keyDown(screen.getByRole('listbox'), { key: 'Escape' });

        // Untouched, it is not sent: the probe's row stays the probe's.
        await save();
        await waitFor(() => expect(submitted).toHaveLength(1));
        expect(submitted[0]).not.toHaveProperty('capability_verdicts');
      });

      it(`shows a ${capability} unknown hint of its own`, async () => {
        renderForm({ row: makeMapping({ capabilities: [] }) });
        expect(screen.getByText(hint)).toBeInTheDocument();
        // Each audio capability names its own probe task and its own
        // enablement, so no two hints -- neither the pair nor any sibling
        // control's -- may share one text.
        expect(hint).not.toBe(t.mappingImageCapableUnknownHint);
        expect(hint).not.toBe(t.mappingVisionCapableUnknownHint);
        expect(hint).not.toBe(t.mappingImageEditUnknownHint);
        expect(t.mappingSpeechCapableUnknownHint).not.toBe(
          t.mappingTranscriptionCapableUnknownHint,
        );
      });
    }

    // decision is the speech twin for System One decision models: yes is the
    // operator's override of the decision probe, and (decision, no) is reserved
    // for that probe, so no is shown only when it is already stored.
    describe('decision verdict', () => {
      const label = t.mappingDecisionCapable;

      it('offers unknown and yes, never a selectable no', async () => {
        renderForm({ row: makeMapping({ capabilities: [] }) });
        fireEvent.mouseDown(screen.getByRole('combobox', { name: label }));
        expect(
          await screen.findByRole('option', { name: t.mappingCapabilityUnknown }),
        ).toBeInTheDocument();
        expect(screen.getByRole('option', { name: t.mappingCapabilityYes })).toBeInTheDocument();
        expect(
          screen.queryByRole('option', { name: t.mappingCapabilityNo }),
        ).not.toBeInTheDocument();
        expect(screen.getByText(t.mappingDecisionCapableUnknownHint)).toBeInTheDocument();
      });

      it('sends yes once picked and nothing when untouched', async () => {
        const { submitted } = renderForm({ row: makeMapping({ capabilities: [] }) });
        await pickOption(label, t.mappingCapabilityYes);
        await save();
        await waitFor(() => expect(submitted).toHaveLength(1));
        expect(submitted[0].capability_verdicts).toEqual({ decision: 'yes' });
      });

      it('shows a stored no as a disabled option and does not resend it', async () => {
        const { submitted } = renderForm({
          row: makeMapping({
            capabilities: [
              {
                capability: 'decision',
                verdict: 'no',
                source: 'decision_probe',
                checked_at: '2026-09-25T12:00:00Z',
              },
            ],
          }),
        });
        const combo = screen.getByRole('combobox', { name: label });
        expect(combo.textContent).toBe(t.mappingCapabilityNo);
        fireEvent.mouseDown(combo);
        const no = await screen.findByRole('option', { name: t.mappingCapabilityNo });
        expect(no).toHaveAttribute('aria-disabled', 'true');
        fireEvent.keyDown(screen.getByRole('listbox'), { key: 'Escape' });
        await save();
        await waitFor(() => expect(submitted).toHaveLength(1));
        expect(submitted[0]).not.toHaveProperty('capability_verdicts');
      });
    });

    // decision_openai is decision's twin for a model that answers OpenAI
    // Decisions natively: the same operator-yes rule, (decision_openai, no)
    // reserved for the probe.
    describe('decision_openai verdict', () => {
      const label = t.mappingDecisionOpenaiCapable;

      it('offers unknown and yes, never a selectable no, with the unknown hint', () => {
        renderForm({ row: makeMapping({ capabilities: [] }) });
        fireEvent.mouseDown(screen.getByRole('combobox', { name: label }));
        expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual([
          t.mappingCapabilityUnknown,
          t.mappingCapabilityYes,
        ]);
        expect(screen.getByText(t.mappingDecisionOpenaiCapableUnknownHint)).toBeInTheDocument();
      });

      it('sends yes once picked and nothing when untouched', async () => {
        const { submitted } = renderForm({ row: makeMapping({ capabilities: [] }) });
        await pickOption(label, t.mappingCapabilityYes);
        await save();
        await waitFor(() => expect(submitted).toHaveLength(1));
        expect(submitted[0].capability_verdicts).toEqual({ decision_openai: 'yes' });
      });

      it('shows a stored no as a disabled option and does not resend it', async () => {
        const { submitted } = renderForm({
          row: makeMapping({
            capabilities: [
              {
                capability: 'decision_openai',
                verdict: 'no',
                source: 'decision_probe',
                checked_at: '2026-09-25T12:00:00Z',
              },
            ],
          }),
        });
        const combo = screen.getByRole('combobox', { name: label });
        expect(combo.textContent).toBe(t.mappingCapabilityNo);
        fireEvent.mouseDown(combo);
        const no = await screen.findByRole('option', { name: t.mappingCapabilityNo });
        expect(no).toHaveAttribute('aria-disabled', 'true');
        fireEvent.keyDown(screen.getByRole('listbox'), { key: 'Escape' });
        await save();
        await waitFor(() => expect(submitted).toHaveLength(1));
        expect(submitted[0]).not.toHaveProperty('capability_verdicts');
      });
    });

    it('shows the mapping decisions route beside the decision verdicts', () => {
      renderForm({ row: makeMapping({ decisions_route: 'translate' }) });
      expect(
        screen.getByText(t.mappingDecisionsRoute(t.mappingDecisionsRouteTranslate)),
      ).toBeInTheDocument();
    });

    it('shows no decisions route when the mapping has none', () => {
      renderForm({ row: makeMapping({ decisions_route: '' }) });
      expect(
        screen.queryByText(t.mappingDecisionsRoute(t.mappingDecisionsRouteTranslate)),
      ).not.toBeInTheDocument();
      expect(screen.queryByText(t.mappingDecisionsRoute(''))).not.toBeInTheDocument();
    });

    // embedding and rerank mark a model's ROLE (a vector encoder / a reranker,
    // not a text generator). Unlike the audio and image controls above, nothing
    // probes either one and neither pair is reserved, so there is no automated
    // writer a manual verdict could mask: the control offers the full three
    // states exactly like vision, and the operator's verdict is the source of
    // truth.
    for (const control of [
      {
        capability: 'embedding',
        label: t.mappingEmbeddingCapable,
        hint: t.mappingEmbeddingCapableUnknownHint,
      },
      {
        capability: 'rerank',
        label: t.mappingRerankCapable,
        hint: t.mappingRerankCapableUnknownHint,
      },
    ] as const) {
      const { capability, label, hint } = control;

      it(`offers unknown, yes and no for ${capability}`, async () => {
        renderForm({ row: makeMapping({ capabilities: [] }) });
        fireEvent.mouseDown(screen.getByRole('combobox', { name: label }));
        await screen.findByRole('option', { name: t.mappingCapabilityUnknown });
        expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual([
          t.mappingCapabilityUnknown,
          t.mappingCapabilityYes,
          t.mappingCapabilityNo,
        ]);
        fireEvent.keyDown(screen.getByRole('listbox'), { key: 'Escape' });
      });

      it(`seeds ${capability} from the capability ROW`, () => {
        renderForm({ row: makeMapping({ capabilities: [capRow(capability, 'yes')] }) });
        expect(screen.getByRole('combobox', { name: label }).textContent).toBe(
          t.mappingCapabilityYes,
        );
      });

      it(`sends the ${capability} verdict when the control is moved to yes`, async () => {
        const { submitted } = renderForm({ row: makeMapping({ capabilities: [] }) });
        await pickOption(label, t.mappingCapabilityYes);
        await save();

        await waitFor(() => expect(submitted).toHaveLength(1));
        expect(submitted[0].capability_verdicts).toEqual({ [capability]: 'yes' });
      });

      it(`sends an explicit ${capability} no, which no probe could ever contradict`, async () => {
        const { submitted } = renderForm({ row: makeMapping({ capabilities: [] }) });
        await pickOption(label, t.mappingCapabilityNo);
        await save();

        await waitFor(() => expect(submitted).toHaveLength(1));
        expect(submitted[0].capability_verdicts).toEqual({ [capability]: 'no' });
      });

      it(`sends an empty ${capability} verdict when a stored yes is reset to unknown`, async () => {
        const { submitted } = renderForm({
          row: makeMapping({ capabilities: [capRow(capability, 'yes')] }),
        });
        await pickOption(label, t.mappingCapabilityUnknown);
        await save();

        await waitFor(() => expect(submitted).toHaveLength(1));
        expect(submitted[0].capability_verdicts).toEqual({ [capability]: '' });
      });

      it(`sends nothing for an untouched ${capability} control`, async () => {
        const { submitted } = renderForm({
          row: makeMapping({ capabilities: [capRow(capability, 'yes')] }),
        });
        await save();

        await waitFor(() => expect(submitted).toHaveLength(1));
        expect(submitted[0]).not.toHaveProperty('capability_verdicts');
      });

      it(`shows a ${capability} unknown hint of its own, and only while it is unknown`, async () => {
        renderForm({ row: makeMapping({ capabilities: [] }) });
        expect(screen.getByText(hint)).toBeInTheDocument();
        expect(hint).not.toBe(t.mappingVisionCapableUnknownHint);
        expect(hint).not.toBe(t.mappingImageCapableUnknownHint);
        expect(hint).not.toBe(t.mappingSpeechCapableUnknownHint);
        expect(hint).not.toBe(t.mappingTranscriptionCapableUnknownHint);

        await pickOption(label, t.mappingCapabilityYes);
        expect(screen.queryByText(hint)).not.toBeInTheDocument();
      });
    }

    it('gives embedding and rerank hints that differ from one another', () => {
      expect(t.mappingEmbeddingCapableUnknownHint).not.toBe(t.mappingRerankCapableUnknownHint);
      expect(t.mappingEmbeddingCapable).not.toBe(t.mappingRerankCapable);
    });

    it('sends both role verdicts together when both controls are moved', async () => {
      const { submitted } = renderForm({ row: makeMapping({ capabilities: [] }) });
      await pickOption(t.mappingEmbeddingCapable, t.mappingCapabilityYes);
      await pickOption(t.mappingRerankCapable, t.mappingCapabilityYes);
      await save();

      await waitFor(() => expect(submitted).toHaveLength(1));
      expect(submitted[0].capability_verdicts).toEqual({ embedding: 'yes', rerank: 'yes' });
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

  describe(`MappingForm context probe [${locale}]`, () => {
    it('says the probe is still pending, not failed, when the status poll reaches its cap', async () => {
      // The run never ends while the real poller runs to its cap.
      renderForm({
        contextProbePath: '/props',
        benchmarkStatus: async () => ({ ...idle, running: true }),
      });
      fireEvent.click(screen.getByRole('button', { name: t.mappingProbeContext }));

      expect(
        await screen.findByText(t.mappingContextProbePending, undefined, { timeout: 5000 }),
      ).toBeInTheDocument();
      expect(screen.queryByText('benchmark poll timed out')).toBeNull();
    });

    it('reports a status-poll failure as the error it is, not as pending', async () => {
      renderForm({
        contextProbePath: '/props',
        benchmarkStatus: async () => {
          throw new Error('network down');
        },
      });
      fireEvent.click(screen.getByRole('button', { name: t.mappingProbeContext }));

      expect(await screen.findByText('network down')).toBeInTheDocument();
      expect(screen.queryByText(t.mappingContextProbePending)).toBeNull();
    });

    // The probe loads the model with a chat prompt, which the gateway refuses
    // for an images-only mapping (benchmark.images_only). The button says so
    // before the click, in a hint it is described by.
    it('disables the probe for an images-only mapping and names the reason beside it', () => {
      renderForm({ contextProbePath: '/props', row: makeMapping({ images_only: true }) });
      const button = screen.getByRole('button', { name: t.mappingProbeContext });
      expect(button).toBeDisabled();
      expect(button).toHaveAccessibleDescription(t.mappingProbeContextImagesOnly);
    });

    it('offers the probe for a text mapping, with no images-only hint', async () => {
      renderForm({ contextProbePath: '/props', row: makeMapping({ images_only: false }) });
      const button = screen.getByRole('button', { name: t.mappingProbeContext });
      await waitFor(() => expect(button).toBeEnabled());
      expect(button).not.toHaveAccessibleDescription();
      expect(screen.queryByText(t.mappingProbeContextImagesOnly)).not.toBeInTheDocument();
    });

    it('disables the probe without a context_probe_path', async () => {
      renderForm({ contextProbePath: '', row: makeMapping({ images_only: false }) });
      // Let the busy poll answer first, so the assertion reads the settled
      // button.
      await act(async () => {});
      expect(screen.getByRole('button', { name: t.mappingProbeContext })).toBeDisabled();
    });

    // A server_agent application carries no context_probe_path: the gateway
    // derives the probe from the agent router or the agent's telemetry.
    it('offers the probe without a context_probe_path when the gateway derives it', async () => {
      renderForm({
        contextProbePath: '',
        contextProbeDerived: true,
        row: makeMapping({ images_only: false }),
      });
      const button = screen.getByRole('button', { name: t.mappingProbeContext });
      await waitFor(() => expect(button).toBeEnabled());
      expect(button).not.toHaveAccessibleDescription();
    });

    it('still disables a derived probe for an images-only mapping', async () => {
      renderForm({
        contextProbePath: '',
        contextProbeDerived: true,
        row: makeMapping({ images_only: true }),
      });
      await act(async () => {});
      const button = screen.getByRole('button', { name: t.mappingProbeContext });
      expect(button).toBeDisabled();
      expect(button).toHaveAccessibleDescription(t.mappingProbeContextImagesOnly);
    });

    // The speech twin: the gateway refuses the same chat-prompt probe for a
    // mapping whose effective flavors name openai_audio and no text flavor
    // (benchmark.audio_only), and the button says so the same way.
    it('disables the probe for an audio-only mapping and names the reason beside it', () => {
      renderForm({ contextProbePath: '/props', row: makeMapping({ audio_only: true }) });
      const button = screen.getByRole('button', { name: t.mappingProbeContext });
      expect(button).toBeDisabled();
      expect(button).toHaveAccessibleDescription(t.mappingProbeContextAudioOnly);
      expect(screen.queryByText(t.mappingProbeContextImagesOnly)).not.toBeInTheDocument();
    });

    it('still disables a derived probe for an audio-only mapping', async () => {
      renderForm({
        contextProbePath: '',
        contextProbeDerived: true,
        row: makeMapping({ audio_only: true }),
      });
      await act(async () => {});
      const button = screen.getByRole('button', { name: t.mappingProbeContext });
      expect(button).toBeDisabled();
      expect(button).toHaveAccessibleDescription(t.mappingProbeContextAudioOnly);
    });

    it('offers the probe for a mapping that is not audio-only, with no audio-only hint', async () => {
      renderForm({
        contextProbePath: '/props',
        row: makeMapping({ images_only: false, audio_only: false }),
      });
      const button = screen.getByRole('button', { name: t.mappingProbeContext });
      await waitFor(() => expect(button).toBeEnabled());
      expect(button).not.toHaveAccessibleDescription();
      expect(screen.queryByText(t.mappingProbeContextAudioOnly)).not.toBeInTheDocument();
    });

    it('disables the probe for a decisions-only mapping and names the reason', () => {
      renderForm({ contextProbePath: '/props', row: makeMapping({ decisions_only: true }) });
      const button = screen.getByRole('button', { name: t.mappingProbeContext });
      expect(button).toBeDisabled();
      expect(button).toHaveAccessibleDescription(t.mappingProbeContextDecisionsOnly);
    });

    // A list naming both openai_images and openai_audio (and no text flavor)
    // is both: one hint is shown, the images-only one, whose sentence names a
    // flavor that holds.
    it('shows a single hint for a mapping that is both images-only and audio-only', () => {
      renderForm({
        contextProbePath: '/props',
        row: makeMapping({ images_only: true, audio_only: true }),
      });
      const button = screen.getByRole('button', { name: t.mappingProbeContext });
      expect(button).toBeDisabled();
      expect(button).toHaveAccessibleDescription(t.mappingProbeContextImagesOnly);
      expect(screen.queryByText(t.mappingProbeContextAudioOnly)).not.toBeInTheDocument();
    });

    // The create form has no mapping to probe yet.
    it('offers no probe on the create form', () => {
      renderForm({ contextProbePath: '/props', row: null });
      expect(screen.getByLabelText(t.mappingContextSize)).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: t.mappingProbeContext })).not.toBeInTheDocument();
    });
  });

  describe(`MappingForm ownership boundary [${locale}]`, () => {
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

  describe(`MappingForm metric diffing (issue #65) [${locale}]`, () => {
    const machineMetricKeys = [
      'context_size',
      'gen_tokens_per_second',
      'prompt_tokens_per_second',
      'load_time_ms',
      'energy_wh_per_token',
      'max_concurrency',
      'recommended_concurrency',
      'gen_tokens_per_second_at_capacity',
    ] as const;

    it('omits every unchanged metric so a concurrently probed value is not reverted', async () => {
      const { submitted } = renderForm({
        row: makeMapping({ context_size: 8192, gen_tokens_per_second: 40 }),
      });
      await save();
      await waitFor(() => expect(submitted).toHaveLength(1));
      for (const key of machineMetricKeys) {
        expect(submitted[0]).not.toHaveProperty(key);
      }
      // The operator-owned fields are still always sent.
      expect(submitted[0]).toHaveProperty('status');
      expect(submitted[0]).toHaveProperty('metrics_locked');
    });

    it('sends ONLY the metric the operator actually changed', async () => {
      const { submitted } = renderForm({ row: makeMapping({ context_size: 8192 }) });
      fireEvent.change(screen.getByLabelText(t.mappingContextSize), { target: { value: '4096' } });
      await save();
      await waitFor(() => expect(submitted).toHaveLength(1));
      expect(submitted[0].context_size).toBe(4096);
      for (const key of machineMetricKeys) {
        if (key === 'context_size') continue;
        expect(submitted[0]).not.toHaveProperty(key);
      }
    });
  });

  // #340: the audio metrics sit beside the text metrics, under the same lock,
  // and follow the same rule: an unchanged one is the benchmark's and is not
  // sent. The real-time concurrency and its limit are one result.
  describe(`MappingForm audio metrics [${locale}]`, () => {
    const audioKeys = [
      'audio_rate_x',
      'audio_ttfa_ms',
      'audio_realtime_concurrency',
      'audio_concurrency_limit',
    ] as const;

    const measured = () =>
      makeMapping({
        audio_rate_x: 2.5,
        audio_ttfa_ms: 300,
        audio_realtime_concurrency: 2,
        audio_concurrency_limit: 'resources',
      });

    function countField() {
      return screen.getByLabelText(t.mappingAudioRealtimeConcurrency) as HTMLInputElement;
    }

    it('shows the stored audio metrics on the edit form', () => {
      renderForm({ row: measured() });
      expect(screen.getByText(t.mappingAudioMetricsSection)).toBeInTheDocument();
      expect((screen.getByLabelText(t.mappingAudioRateX) as HTMLInputElement).value).toBe('2.5');
      expect((screen.getByLabelText(t.mappingAudioTtfaMs) as HTMLInputElement).value).toBe('300');
      expect(countField().value).toBe('2');
      expect(
        screen.getByRole('combobox', { name: t.mappingAudioConcurrencyLimit }),
      ).toHaveTextContent(t.mappingAudioConcurrencyLimitResources);
    });

    // The create request carries no audio metrics: a value typed there would
    // be dropped without a word.
    it('offers no audio metrics on the create form', () => {
      renderForm({ row: null });
      expect(screen.queryByText(t.mappingAudioMetricsSection)).not.toBeInTheDocument();
      expect(screen.queryByLabelText(t.mappingAudioRateX)).not.toBeInTheDocument();
    });

    it('sends no audio metric the operator did not change', async () => {
      const { submitted } = renderForm({ row: measured() });
      await save();
      await waitFor(() => expect(submitted).toHaveLength(1));
      for (const key of audioKeys) expect(submitted[0]).not.toHaveProperty(key);
    });

    it('sends a changed rate and time on their own', async () => {
      const { submitted } = renderForm({ row: measured() });
      fireEvent.change(screen.getByLabelText(t.mappingAudioRateX), { target: { value: '3' } });
      fireEvent.change(screen.getByLabelText(t.mappingAudioTtfaMs), { target: { value: '200' } });
      await save();
      await waitFor(() => expect(submitted).toHaveLength(1));
      expect(submitted[0]).toMatchObject({ audio_rate_x: 3, audio_ttfa_ms: 200 });
      expect(submitted[0]).not.toHaveProperty('audio_realtime_concurrency');
      expect(submitted[0]).not.toHaveProperty('audio_concurrency_limit');
    });

    it('sends the count with its limit when only the count changes', async () => {
      const { submitted } = renderForm({ row: measured() });
      fireEvent.change(countField(), { target: { value: '3' } });
      await save();
      await waitFor(() => expect(submitted).toHaveLength(1));
      expect(submitted[0]).toMatchObject({
        audio_realtime_concurrency: 3,
        audio_concurrency_limit: 'resources',
      });
    });

    it('sends the count with its limit when only the limit changes', async () => {
      const { submitted } = renderForm({ row: measured() });
      await pickOption(t.mappingAudioConcurrencyLimit, t.mappingAudioConcurrencyLimitPerformance);
      await save();
      await waitFor(() => expect(submitted).toHaveLength(1));
      expect(submitted[0]).toMatchObject({
        audio_realtime_concurrency: 2,
        audio_concurrency_limit: 'performance',
      });
    });

    // An unknown limit clears both: the count field empties and locks, and the
    // limit goes alone -- a count beside it would be a count without a limit.
    it('clears the count with an unknown limit and sends the limit alone', async () => {
      const { submitted } = renderForm({ row: measured() });
      await pickOption(t.mappingAudioConcurrencyLimit, t.mappingAudioConcurrencyLimitUnknown);
      expect(countField().value).toBe('');
      expect(countField()).toBeDisabled();
      await save();
      await waitFor(() => expect(submitted).toHaveLength(1));
      expect(submitted[0].audio_concurrency_limit).toBe('');
      expect(submitted[0]).not.toHaveProperty('audio_realtime_concurrency');
    });

    // A count needs a limit, so it is entered only once one is chosen.
    it('takes a count only once a limit is chosen', async () => {
      const { submitted } = renderForm({ row: makeMapping() });
      expect(countField()).toBeDisabled();
      await pickOption(t.mappingAudioConcurrencyLimit, t.mappingAudioConcurrencyLimitCap);
      expect(countField()).toBeEnabled();
      fireEvent.change(countField(), { target: { value: '4' } });
      await save();
      await waitFor(() => expect(submitted).toHaveLength(1));
      expect(submitted[0]).toMatchObject({
        audio_realtime_concurrency: 4,
        audio_concurrency_limit: 'cap',
      });
    });

    // A detour through "unknown" is no edit: the measured count comes back
    // with its limit, and an unchanged pair is left to the benchmark.
    it('keeps the measured count when the limit goes to unknown and back', async () => {
      const { submitted } = renderForm({ row: measured() });
      await pickOption(t.mappingAudioConcurrencyLimit, t.mappingAudioConcurrencyLimitUnknown);
      expect(countField().value).toBe('');
      await pickOption(t.mappingAudioConcurrencyLimit, t.mappingAudioConcurrencyLimitResources);
      expect(countField().value).toBe('2');
      await save();
      await waitFor(() => expect(submitted).toHaveLength(1));
      expect(submitted[0]).not.toHaveProperty('audio_realtime_concurrency');
      expect(submitted[0]).not.toHaveProperty('audio_concurrency_limit');
    });

    it('sends the measured count beside a new limit chosen after unknown', async () => {
      const { submitted } = renderForm({ row: measured() });
      await pickOption(t.mappingAudioConcurrencyLimit, t.mappingAudioConcurrencyLimitUnknown);
      await pickOption(t.mappingAudioConcurrencyLimit, t.mappingAudioConcurrencyLimitPerformance);
      await save();
      await waitFor(() => expect(submitted).toHaveLength(1));
      expect(submitted[0]).toMatchObject({
        audio_realtime_concurrency: 2,
        audio_concurrency_limit: 'performance',
      });
    });

    // Beside a limit, 0 is a result -- "not real-time even alone", a ceiling
    // the scaler obeys -- so a blank count is never saved as one: the form
    // names the missing count and does not save.
    it('refuses to save a chosen limit without its count', async () => {
      const { submitted } = renderForm({ row: makeMapping() });
      await pickOption(t.mappingAudioConcurrencyLimit, t.mappingAudioConcurrencyLimitPerformance);
      expect(countField().value).toBe('');
      expect(
        screen.getByText(t.mappingAudioRealtimeConcurrencyRequired('performance')),
      ).toBeInTheDocument();
      expect(countField()).toHaveAttribute('aria-invalid', 'true');
      await save();
      await act(async () => {
        await Promise.resolve();
      });
      expect(submitted).toHaveLength(0);

      fireEvent.change(countField(), { target: { value: '0' } });
      expect(
        screen.queryByText(t.mappingAudioRealtimeConcurrencyRequired('performance')),
      ).not.toBeInTheDocument();
      await save();
      await waitFor(() => expect(submitted).toHaveLength(1));
      expect(submitted[0]).toMatchObject({
        audio_realtime_concurrency: 0,
        audio_concurrency_limit: 'performance',
      });
    });

    // The field is required beside a limit, which stops the Save button in a
    // browser; the form is submitted directly here, past that constraint
    // validation, to prove the form's own check for a browser that skips it.
    it('refuses to save a measured count cleared beside its limit', async () => {
      const { submitted } = renderForm({ row: measured() });
      fireEvent.change(countField(), { target: { value: '' } });
      expect(
        screen.getByText(t.mappingAudioRealtimeConcurrencyRequired('resources')),
      ).toBeInTheDocument();
      expect(countField()).toBeRequired();
      fireEvent.submit(countField().closest('form')!);
      await act(async () => {
        await Promise.resolve();
      });
      expect(submitted).toHaveLength(0);
      expect(countField()).toHaveFocus();
    });

    // The missing count is asked for in the words of the limit beside it: a
    // 0 is "not real-time" only with performance, and a lower bound, "≥ 0",
    // with resources or the cap.
    it.each(['resources', 'cap'] as const)(
      'says beside %s that a count of 0 is a lower bound',
      async (limit) => {
        renderForm({ row: makeMapping() });
        await pickOption(
          t.mappingAudioConcurrencyLimit,
          limit === 'resources'
            ? t.mappingAudioConcurrencyLimitResources
            : t.mappingAudioConcurrencyLimitCap,
        );
        const help = screen.getByText(t.mappingAudioRealtimeConcurrencyRequired(limit));
        expect(help.textContent).toContain('≥ 0');
        expect(help.textContent).not.toContain(t.audioRealtimeConcurrency(0, 'performance'));
        await pickOption(t.mappingAudioConcurrencyLimit, t.mappingAudioConcurrencyLimitPerformance);
        expect(
          screen.getByText(t.mappingAudioRealtimeConcurrencyRequired('performance')).textContent,
        ).toContain(t.audioRealtimeConcurrency(0, 'performance'));
      },
    );

    // The hint says what an empty field or a 0 means per field: not measured
    // for the rate and the time, a result for the count.
    it('does not call a count of 0 unknown in its hint', () => {
      renderForm({ row: measured() });
      const hint = screen.getByText(t.mappingAudioMetricsHint);
      expect(hint.textContent).not.toMatch(/0 = (unbekannt|unknown)/i);
      expect(hint.textContent).toContain(t.audioRealtimeConcurrency(0, 'performance'));
      expect(hint.textContent).toContain(t.audioRealtimeConcurrency(0, 'resources'));
    });
  });
}
