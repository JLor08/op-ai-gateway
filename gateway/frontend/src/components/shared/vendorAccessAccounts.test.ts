// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import type { VendorAccount } from '../../api';
import { messages } from '../../i18n';
import {
  offerableVendorAccounts,
  patchVendorAccessEntry,
  toggleVendorAccessEntry,
  vendorAccessEntryDTO,
  vendorAccessEntryInvalid,
  vendorAccessEntryState,
  vendorAccountOptionLabel,
} from './vendorAccessAccounts';

// Only the fields the eligibility rule reads.
type Row = Pick<VendorAccount, 'id' | 'share_level' | 'is_owner'>;

const own = (id: string, share_level: VendorAccount['share_level'] = 'none'): Row => ({
  id,
  share_level,
  is_owner: true,
});
// A co-manager's row: the management view of somebody else's account.
const comanaged = (id: string, share_level: 'shared' | 'full' = 'shared'): Row => ({
  id,
  share_level,
  is_owner: false,
});
// A member's row: the minimal view (share_level withheld, which is NOT `none`).
const member = (id: string): Row => ({
  id,
  share_level: '',
  is_owner: false,
});
// A row nobody can legitimately produce: a level `none` grants nothing, so the
// account is not shared with anybody but its owner.
const unsharedForeign = (id: string): Row => ({
  id,
  share_level: 'none',
  is_owner: false,
});

const ids = (rows: Row[]) => rows.map((row) => row.id);

// Spec 2026-10-15 section 8: the per-token opt-in names an account the token's
// PRINCIPAL can route to. The list rows are rendered for the VIEWER, so the DTO
// can never prove reach for a co-manager (a co-manager who is also provisioned
// into the account's resource group reads the management view, the widest tier,
// exactly like a co-manager who is not): the picker therefore declines only
// what is certainly not reachable, and the backend save check plus routing stay
// the gate.
describe('offerableVendorAccounts', () => {
  describe('for a user token (the owning user is the principal)', () => {
    it('splits the caller own accounts from the accounts shared with them, keeping the list order', () => {
      const rows = [
        own('a1'),
        comanaged('c1'),
        own('a2', 'shared'),
        member('m1'),
        comanaged('c2', 'full'),
      ];
      const { own: ownRows, shared } = offerableVendorAccounts(rows, 'user');
      expect(ids(ownRows)).toEqual(['a1', 'a2']);
      expect(ids(shared)).toEqual(['c1', 'm1', 'c2']);
    });

    it('offers an own account at every share level, including the unshared one', () => {
      const rows = [own('n', 'none'), own('s', 'shared'), own('f', 'full')];
      expect(ids(offerableVendorAccounts(rows, 'user').own)).toEqual(['n', 's', 'f']);
    });

    it('offers a member (routing-reachable) account', () => {
      expect(ids(offerableVendorAccounts([member('m')], 'user').shared)).toEqual(['m']);
    });

    it('offers a co-managed account too: the DTO cannot tell a co-manager who is also a member from one who is not', () => {
      expect(
        ids(
          offerableVendorAccounts([comanaged('c', 'shared'), comanaged('d', 'full')], 'user')
            .shared,
        ),
      ).toEqual(['c', 'd']);
    });

    it("never offers somebody else's account whose level is none: it is certainly not reachable", () => {
      const result = offerableVendorAccounts([unsharedForeign('x')], 'user');
      expect(result.own).toEqual([]);
      expect(result.shared).toEqual([]);
    });
  });

  describe('for a service token (a service owns nothing)', () => {
    it('has no own accounts: even the creator own account is, for the service, just a shared one', () => {
      const rows = [own('a', 'shared'), own('b', 'full')];
      const result = offerableVendorAccounts(rows, 'service');
      expect(result.own).toEqual([]);
      expect(ids(result.shared)).toEqual(['a', 'b']);
    });

    it('declines an own account that is not shared: it grants a service nothing', () => {
      const result = offerableVendorAccounts([own('private', 'none')], 'service');
      expect(result.shared).toEqual([]);
    });

    it('offers co-managed and member accounts', () => {
      const rows = [comanaged('c'), member('m')];
      expect(ids(offerableVendorAccounts(rows, 'service').shared)).toEqual(['c', 'm']);
    });

    it('declines a foreign row whose level is none', () => {
      expect(offerableVendorAccounts([unsharedForeign('x')], 'service').shared).toEqual([]);
    });
  });

  it('keeps the rows themselves (it is a filter, not a projection)', () => {
    const row = member('m');
    expect(offerableVendorAccounts([row], 'user').shared[0]).toBe(row);
  });
});

describe('vendorAccountOptionLabel', () => {
  for (const locale of ['de', 'en'] as const) {
    const t = messages[locale];

    it(`names an own account by its name alone [${locale}]`, () => {
      expect(vendorAccountOptionLabel({ name: 'Work', is_owner: true, shared_by: '' }, t)).toBe(
        'Work',
      );
    });

    it(`adds the owner of a shared account, which tells same-named accounts apart [${locale}]`, () => {
      expect(
        vendorAccountOptionLabel({ name: 'Team', is_owner: false, shared_by: 'Alice' }, t),
      ).toBe(`Team (${t.vendorAccountSharedBy} Alice)`);
    });

    it(`falls back to the name when the owner is not known [${locale}]`, () => {
      expect(vendorAccountOptionLabel({ name: 'Team', is_owner: false, shared_by: '' }, t)).toBe(
        'Team',
      );
    });
  }
});

describe('vendor access entries', () => {
  it('reads an override as on only when it is present and enabled', () => {
    expect(vendorAccessEntryState({ account_id: 'a' })).toEqual({
      accountId: 'a',
      overrideEnabled: false,
      prefix: '',
    });
    expect(
      vendorAccessEntryState({ account_id: 'a', prefix_override: { enabled: false, value: 'x/' } })
        .overrideEnabled,
    ).toBe(false);
    expect(
      vendorAccessEntryState({ account_id: 'a', prefix_override: { enabled: true, value: 'x/' } }),
    ).toEqual({ accountId: 'a', overrideEnabled: true, prefix: 'x/' });
  });

  it('writes an override that is off without prefix_override, and an enabled one trimmed', () => {
    expect(
      vendorAccessEntryDTO({ accountId: 'a', overrideEnabled: false, prefix: 'leak/' }),
    ).toEqual({ account_id: 'a' });
    expect(vendorAccessEntryDTO({ accountId: 'a', overrideEnabled: true, prefix: ' x/ ' })).toEqual(
      {
        account_id: 'a',
        prefix_override: { enabled: true, value: 'x/' },
      },
    );
    expect(vendorAccessEntryDTO({ accountId: 'a', overrideEnabled: true, prefix: '' })).toEqual({
      account_id: 'a',
      prefix_override: { enabled: true, value: '' },
    });
  });

  it('judges only an enabled override prefix', () => {
    expect(vendorAccessEntryInvalid({ accountId: 'a', overrideEnabled: false, prefix: '?' })).toBe(
      false,
    );
    expect(vendorAccessEntryInvalid({ accountId: 'a', overrideEnabled: true, prefix: '?' })).toBe(
      true,
    );
    expect(vendorAccessEntryInvalid({ accountId: 'a', overrideEnabled: true, prefix: 'ok/' })).toBe(
      false,
    );
  });

  it('toggles an entry in with its override off and out again, and patches only the named one', () => {
    const one = toggleVendorAccessEntry([], 'a');
    expect(one).toEqual([{ accountId: 'a', overrideEnabled: false, prefix: '' }]);
    const two = toggleVendorAccessEntry(one, 'b');
    const patched = patchVendorAccessEntry(two, 'b', { overrideEnabled: true, prefix: 'b/' });
    expect(patched).toEqual([
      { accountId: 'a', overrideEnabled: false, prefix: '' },
      { accountId: 'b', overrideEnabled: true, prefix: 'b/' },
    ]);
    expect(toggleVendorAccessEntry(patched, 'a')).toEqual([patched[1]]);
  });
});
