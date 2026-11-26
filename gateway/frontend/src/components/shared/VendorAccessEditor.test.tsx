// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { messages, type Locale } from '../../i18n';
import type { VendorAccount, VendorAccountGroup } from '../../api';
import {
  VendorAccessEditor,
  buildVendorAccessDTO,
  emptyVendorAccessDraft,
  vendorAccessDraftFromDTO,
  vendorAccessDraftInvalid,
  type VendorAccessDraft,
} from './VendorAccessEditor';

afterEach(cleanup);

function account(overrides: Partial<VendorAccount> = {}): VendorAccount {
  return {
    id: 'va_own',
    name: 'Own Claude',
    is_owner: true,
    share_level: 'manage',
    shared_by: '',
    ...overrides,
  } as VendorAccount;
}

function group(overrides: Partial<VendorAccountGroup> = {}): VendorAccountGroup {
  return { id: 'vag_1', name: 'Pool', model_prefix: 'pool/', ...overrides } as VendorAccountGroup;
}

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  function setup(
    value: VendorAccessDraft,
    opts: {
      accounts?: VendorAccount[] | null;
      groups?: VendorAccountGroup[] | null;
      disabled?: boolean;
    } = {},
  ) {
    const onChange = vi.fn();
    render(
      <VendorAccessEditor
        value={value}
        onChange={onChange}
        accounts={
          opts.accounts === undefined
            ? [
                account(),
                account({
                  id: 'va_shared',
                  name: 'Team',
                  is_owner: false,
                  shared_by: 'ann',
                  share_level: 'shared',
                }),
              ]
            : opts.accounts
        }
        groups={opts.groups === undefined ? [group()] : opts.groups}
        t={t}
        idPrefix="x"
        labels={{ heading: t.tokenVendorAccessLabel, note: t.tokenVendorAccessNote }}
        disabled={opts.disabled}
      />,
    );
    return onChange;
  }

  describe(`VendorAccessEditor [${locale}]`, () => {
    it('keeps "incl. groups" disabled until "all" is ticked', () => {
      setup(emptyVendorAccessDraft());
      expect(screen.getByLabelText(t.tokenVendorAccessIncludeGroups)).toBeDisabled();
    });

    it('ticking all reports the draft; unticking all also clears include-groups', () => {
      const onChange = setup({ ...emptyVendorAccessDraft(), all: true, includeGroups: true });
      fireEvent.click(screen.getByLabelText(t.tokenVendorAccessAll));
      expect(onChange).toHaveBeenCalledWith({
        ...emptyVendorAccessDraft(),
        all: false,
        includeGroups: false,
      });
    });

    it('hides the own accounts under all but still offers shared ones and the all hint', () => {
      setup({ ...emptyVendorAccessDraft(), all: true });
      expect(screen.queryByLabelText('Own Claude')).not.toBeInTheDocument();
      expect(screen.getByLabelText(/Team/)).toBeInTheDocument();
      expect(screen.getByText(t.tokenVendorAccessAllHint)).toBeInTheDocument();
    });

    it('reports a toggled account and a toggled group', () => {
      const onChange = setup(emptyVendorAccessDraft());
      fireEvent.click(screen.getByLabelText('Own Claude'));
      expect(onChange).toHaveBeenLastCalledWith({
        ...emptyVendorAccessDraft(),
        accounts: [{ accountId: 'va_own', overrideEnabled: false, prefix: '' }],
      });
      fireEvent.click(screen.getByLabelText(/Pool/));
      expect(onChange).toHaveBeenLastCalledWith({
        ...emptyVendorAccessDraft(),
        groups: [{ accountId: 'vag_1', overrideEnabled: false, prefix: '' }],
      });
    });

    it('renders the override controls of an opted-in account and reports edits', () => {
      const draft = {
        ...emptyVendorAccessDraft(),
        accounts: [{ accountId: 'va_own', overrideEnabled: true, prefix: 'me/' }],
      };
      const onChange = setup(draft);
      const prefix = screen.getByLabelText(t.tokenVendorAccessPrefixLabel);
      expect(prefix).toHaveValue('me/');
      fireEvent.change(prefix, { target: { value: 'you/' } });
      expect(onChange).toHaveBeenCalledWith({
        ...draft,
        accounts: [{ accountId: 'va_own', overrideEnabled: true, prefix: 'you/' }],
      });
    });

    it('shows the empty hint once the list loaded with nothing to offer', () => {
      setup(emptyVendorAccessDraft(), { accounts: [], groups: [] });
      expect(screen.getByText(t.tokenVendorAccessNone)).toBeInTheDocument();
    });

    it('disables every control when disabled', () => {
      setup(emptyVendorAccessDraft(), { disabled: true });
      for (const box of screen.getAllByRole('checkbox')) expect(box).toBeDisabled();
    });
  });
}

describe('vendor access draft helpers', () => {
  const accounts = [account(), account({ id: 'va_shared', is_owner: false, shared_by: 'ann' })];

  it('builds the explicit list, dropping entries the loaded lists no longer offer', () => {
    const draft: VendorAccessDraft = {
      all: false,
      includeGroups: false,
      accounts: [
        { accountId: 'va_own', overrideEnabled: true, prefix: ' a/ ' },
        { accountId: 'va_gone', overrideEnabled: false, prefix: '' },
      ],
      groups: [{ accountId: 'vag_gone', overrideEnabled: false, prefix: '' }],
    };
    expect(buildVendorAccessDTO(draft, accounts, [group()])).toEqual({
      all: false,
      accounts: [{ account_id: 'va_own', prefix_override: { enabled: true, value: 'a/' } }],
    });
  });

  it('builds the zero policy as an explicit object, never null', () => {
    expect(buildVendorAccessDTO(emptyVendorAccessDraft(), accounts, [])).toEqual({
      all: false,
      accounts: [],
    });
  });

  it('builds all with include_groups and only the shared entries', () => {
    const draft: VendorAccessDraft = {
      all: true,
      includeGroups: true,
      accounts: [
        { accountId: 'va_own', overrideEnabled: false, prefix: '' },
        { accountId: 'va_shared', overrideEnabled: false, prefix: '' },
      ],
      groups: [],
    };
    expect(buildVendorAccessDTO(draft, accounts, [])).toEqual({
      all: true,
      include_groups: true,
      accounts: [{ account_id: 'va_shared' }],
    });
  });

  it('round-trips a stored policy and flags a malformed enabled override', () => {
    const draft = vendorAccessDraftFromDTO({
      all: true,
      include_groups: true,
      groups: [{ group_id: 'vag_1', prefix_override: { enabled: true, value: 'p/' } }],
    });
    expect(draft).toEqual({
      all: true,
      includeGroups: true,
      accounts: [],
      groups: [{ accountId: 'vag_1', overrideEnabled: true, prefix: 'p/' }],
    });
    expect(vendorAccessDraftInvalid(draft, null, null)).toBe(false);
    expect(
      vendorAccessDraftInvalid(
        {
          ...draft,
          groups: [{ accountId: 'vag_1', overrideEnabled: true, prefix: 'bad prefix!' }],
        },
        null,
        null,
      ),
    ).toBe(true);
  });
});
