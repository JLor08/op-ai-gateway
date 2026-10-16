// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import type { VendorAccount } from '../../api';
import { vendorAccountAccess } from './vendorAccountAccess';

// Only the fields the access rules read.
type Viewed = Pick<VendorAccount, 'auth_type' | 'share_level' | 'is_owner'>;

const own = (share_level: VendorAccount['share_level'], auth_type: VendorAccount['auth_type']) =>
  ({ share_level, auth_type, is_owner: true }) satisfies Viewed;
const managed = (
  share_level: VendorAccount['share_level'],
  auth_type: VendorAccount['auth_type'],
) => ({ share_level, auth_type, is_owner: false }) satisfies Viewed;

// The rules mirror spec 2026-10-15 section 6: the backend is the gate, the UI only
// declines to OFFER what the viewer could not do.
describe('vendorAccountAccess', () => {
  describe('the owner', () => {
    it.each([
      // level, auth type, admin, subscription sharing on, raise targets, lower targets,
      // may edit the groups
      ['none', 'api_key', true, true, ['shared', 'full'], [], false],
      ['none', 'api_key', false, true, [], [], false],
      ['shared', 'api_key', true, true, ['full'], ['none'], true],
      ['shared', 'api_key', false, true, [], ['none'], true],
      ['full', 'api_key', true, true, [], ['shared', 'none'], true],
      ['full', 'api_key', false, true, [], ['shared', 'none'], true],
      // A subscription account is raised to `shared` only, never `full` (spec 11).
      ['none', 'subscription', true, true, ['shared'], [], false],
      ['shared', 'subscription', true, true, [], ['none'], true],
      // ...and only while vendor_subscription_sharing_enabled is on (#236): the backend
      // answers the raise 409 otherwise, so it is not offered. An api-key account
      // ignores the switch, and so does lowering (never refused by it).
      ['none', 'subscription', true, false, [], [], false],
      ['none', 'api_key', true, false, ['shared', 'full'], [], false],
      ['shared', 'subscription', true, false, [], ['none'], true],
      ['shared', 'api_key', true, false, ['full'], ['none'], true],
    ] as const)(
      'at %s on %s (admin %s, subscription sharing %s) may raise to %j, lower to %j, edit the groups: %s',
      (level, authType, admin, subscriptionSharing, raise, lower, editGroups) => {
        const access = vendorAccountAccess(own(level, authType), admin, subscriptionSharing);
        expect(access.tier).toBe('owner');
        expect(access.raiseTargets).toEqual(raise);
        expect(access.lowerTargets).toEqual(lower);
        expect(access.canEditAdminGroups).toBe(editGroups);
      },
    );

    it.each(['none', 'shared', 'full'] as const)(
      'operates and holds the credential at %s, subscription or not',
      (level) => {
        for (const authType of ['api_key', 'subscription'] as const) {
          const access = vendorAccountAccess(own(level, authType), false);
          expect(access.canOperate).toBe(true);
          expect(access.canManageCredential).toBe(true);
          expect(access.canRunUpstreamOps).toBe(true);
        }
      },
    );

    it('shows the share area to an admin at none and to anyone once it is shared, never to a non-admin at none', () => {
      expect(vendorAccountAccess(own('none', 'api_key'), true).showSharePanel).toBe(true);
      expect(vendorAccountAccess(own('none', 'api_key'), false).showSharePanel).toBe(false);
      expect(vendorAccountAccess(own('shared', 'api_key'), false).showSharePanel).toBe(true);
      expect(vendorAccountAccess(own('full', 'api_key'), true).showSharePanel).toBe(true);
    });

    it('shows no share area for an admin on a personal subscription account while subscription sharing is off, and keeps it once the account is shared', () => {
      // Nothing to raise to (#236), so nothing to show: the notice that explains it is
      // the system settings' business, not this area's.
      expect(vendorAccountAccess(own('none', 'subscription'), true, false).showSharePanel).toBe(
        false,
      );
      expect(vendorAccountAccess(own('none', 'subscription'), true, true).showSharePanel).toBe(
        true,
      );
      // An already-shared one keeps its level and the lowering action.
      expect(vendorAccountAccess(own('shared', 'subscription'), true, false).showSharePanel).toBe(
        true,
      );
    });
  });

  describe('a co-manager (a non-owner reading the management view)', () => {
    it('at shared operates but holds no credential, groups or lowering, and is never offered a raise', () => {
      const access = vendorAccountAccess(managed('shared', 'api_key'), true);
      expect(access.tier).toBe('manager');
      expect(access.canOperate).toBe(true);
      expect(access.canManageCredential).toBe(false);
      expect(access.canEditAdminGroups).toBe(false);
      expect(access.raiseTargets).toEqual([]);
      expect(access.lowerTargets).toEqual([]);
      // The level is still shown, read-only.
      expect(access.showSharePanel).toBe(true);
    });

    it('at full holds everything the owner does except raising', () => {
      const access = vendorAccountAccess(managed('full', 'api_key'), true);
      expect(access.canOperate).toBe(true);
      expect(access.canManageCredential).toBe(true);
      expect(access.canRunUpstreamOps).toBe(true);
      expect(access.canEditAdminGroups).toBe(true);
      expect(access.raiseTargets).toEqual([]);
      expect(access.lowerTargets).toEqual(['shared', 'none']);
    });

    it('may run the upstream calls (test, refresh, usage pull) on an api-key account but never on a subscription', () => {
      for (const level of ['shared', 'full'] as const) {
        expect(vendorAccountAccess(managed(level, 'api_key'), false).canRunUpstreamOps).toBe(true);
        // The subscription exception (spec 11): they open the owner's single-use token.
        expect(vendorAccountAccess(managed(level, 'subscription'), false).canRunUpstreamOps).toBe(
          false,
        );
      }
      // ...while a co-manager of a shared subscription still operates the rest.
      expect(vendorAccountAccess(managed('shared', 'subscription'), false).canOperate).toBe(true);
    });

    it('reaches nothing at none (a system-scope reader of somebody else is read-only)', () => {
      const access = vendorAccountAccess(managed('none', 'api_key'), true);
      expect(access.tier).toBe('manager');
      expect(access.canOperate).toBe(false);
      expect(access.canManageCredential).toBe(false);
      expect(access.canRunUpstreamOps).toBe(false);
      expect(access.canEditAdminGroups).toBe(false);
      expect(access.raiseTargets).toEqual([]);
      expect(access.lowerTargets).toEqual([]);
      expect(access.showSharePanel).toBe(false);
    });
  });

  describe('a member (the minimal view: share_level is withheld)', () => {
    it.each([
      // The member DTO's zero value, and anything else the portal does not know.
      '',
      'bogus',
    ])('reads share_level %j as a member with no reach at all (deny by default)', (level) => {
      const access = vendorAccountAccess(
        {
          share_level: level as VendorAccount['share_level'],
          auth_type: 'api_key',
          is_owner: false,
        },
        true,
      );
      expect(access).toEqual({
        tier: 'member',
        level: 'none',
        canOperate: false,
        canManageCredential: false,
        canRunUpstreamOps: false,
        canEditAdminGroups: false,
        raiseTargets: [],
        lowerTargets: [],
        showSharePanel: false,
      });
    });
  });
});
