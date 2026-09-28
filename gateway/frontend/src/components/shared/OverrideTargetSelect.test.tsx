// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { ModelOption } from '../../api';
import { messages, type Locale } from '../../i18n';
import {
  OverrideTargetSelect,
  overrideTargetOptions,
  overrideTargets,
} from './OverrideTargetSelect';

afterEach(cleanup);

// One name per case the pickers tell apart: served under an API, served under
// none (flavors []), and a group served under one.
const models: ModelOption[] = [
  { id: 'qwen3-32b', display_name: 'qwen3-32b', flavors: ['openai'], loading_on_count: 0 },
  { id: 'sd-child', display_name: 'SD child', flavors: [], loading_on_count: 0 },
  {
    id: 'fast-group',
    display_name: 'fast-group',
    flavors: ['anthropic'],
    loading_on_count: 0,
    is_group: true,
  },
];

describe('overrideTargets', () => {
  it('offers only the names whose flavors are non-empty', () => {
    const targets = overrideTargets(models, null);
    expect(targets.options.map((m) => m.id)).toEqual(['qwen3-32b', 'fast-group']);
    expect([...targets.refused]).toEqual([['sd-child', 'SD child']]);
  });

  it('subtracts the refused names from a server listing and keeps the names Models() lacks', () => {
    // 'hidden-model' is absent from Models(), as a hidden name is, so nothing
    // subtracts it and it stays. 'sd-child' has a Models() row with flavors
    // [], so it goes.
    const targets = overrideTargets(models, [
      { id: 'hidden-model', display_name: 'hidden-model' },
      { id: 'qwen3-32b', display_name: 'qwen3-32b' },
      { id: 'sd-child', display_name: 'sd-child' },
    ]);
    expect(targets.options.map((m) => m.id)).toEqual(['hidden-model', 'qwen3-32b']);
  });
});

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  describe(`overrideTargetOptions [${locale}]`, () => {
    const targets = overrideTargets(models, null);

    it('lists the empty choice and every offered target', () => {
      expect(overrideTargetOptions(t, targets, '')).toEqual([
        { value: '', label: '-' },
        { value: 'qwen3-32b', label: 'qwen3-32b' },
        { value: 'fast-group', label: 'fast-group' },
      ]);
    });

    it('appends a refused current value, labelled unavailable', () => {
      expect(overrideTargetOptions(t, targets, 'sd-child')).toEqual([
        { value: '', label: '-' },
        { value: 'qwen3-32b', label: 'qwen3-32b' },
        { value: 'fast-group', label: 'fast-group' },
        { value: 'sd-child', label: `SD child ${t.tokenOverrideTargetUnavailable}` },
      ]);
    });

    it('appends nothing for an offered current value', () => {
      expect(overrideTargetOptions(t, targets, 'qwen3-32b')).toHaveLength(3);
    });

    it('appends nothing for a saved hidden name, which has no Models() row', () => {
      expect(overrideTargetOptions(t, targets, 'hidden-model')).toHaveLength(3);
    });
  });

  describe(`OverrideTargetSelect [${locale}]`, () => {
    const targets = overrideTargets(models, null);

    it('shows a refused current value with the unavailable marker', () => {
      render(
        <OverrideTargetSelect
          id="target"
          label="Target"
          value="sd-child"
          onChange={vi.fn()}
          targets={targets}
          t={t}
        />,
      );
      expect(screen.getByRole('combobox', { name: 'Target' })).toHaveValue(
        `SD child ${t.tokenOverrideTargetUnavailable}`,
      );
      expect(screen.getByTestId('searchable-select-unavailable')).toBeInTheDocument();
      expect(screen.getByTitle(t.errorPortalTokenModelOverrideInvalid)).toBeInTheDocument();
    });

    // 'hidden-model' stands for a callable hidden name: Models() drops it, so
    // it has no row and is not among the options, and the save accepts it.
    it('shows no marker for a saved hidden name that has no Models() row', () => {
      render(
        <OverrideTargetSelect
          id="target"
          label="Target"
          value="hidden-model"
          onChange={vi.fn()}
          targets={targets}
          t={t}
        />,
      );
      expect(screen.queryByTestId('searchable-select-unavailable')).not.toBeInTheDocument();
      expect(screen.queryByTitle(t.errorPortalTokenModelOverrideInvalid)).not.toBeInTheDocument();
    });

    it('shows no marker for an offered current value, and offers no refused name', async () => {
      render(
        <OverrideTargetSelect
          id="target"
          label="Target"
          value="qwen3-32b"
          onChange={vi.fn()}
          targets={targets}
          t={t}
        />,
      );
      expect(screen.queryByTestId('searchable-select-unavailable')).not.toBeInTheDocument();
      fireEvent.mouseDown(screen.getByRole('combobox', { name: 'Target' }));
      expect(await screen.findByRole('option', { name: 'fast-group' })).toBeInTheDocument();
      expect(screen.queryByRole('option', { name: /SD child/ })).not.toBeInTheDocument();
    });
  });
}
