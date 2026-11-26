// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"op-ai-gateway/internal/routing"
	"testing"
)

// TestVendorAccountUsageManager: the portal half of who may be shown a vendor
// account's plan-usage figures (#407). It is the management-view part of
// authorizeVendorAccountRead: system scope, or a CM of an account that is
// `shared` or `full` by its effective level. The owner is false on purpose (the
// gateway checks the owner from the routing target), and so are a member, a
// stranger and an unknown id.
func TestVendorAccountUsageManager(t *testing.T) {
	e := newVendorMemberEnv(t)
	shared := e.accounts[routing.VendorShareLevelShared]
	e.link(shared.ID)

	cases := []struct {
		name string
		tok  func() (bool, error)
		want bool
	}{
		{"system_scope", func() (bool, error) {
			return e.svc.VendorAccountUsageManager(e.ctx, token("usr_s", "system", "admin"), shared.ID)
		}, true},
		{"cm_of_shared_account", func() (bool, error) {
			return e.svc.VendorAccountUsageManager(e.ctx, token("usr_cm", "admin"), shared.ID)
		}, true},
		{"owner", func() (bool, error) {
			return e.svc.VendorAccountUsageManager(e.ctx, token(vendorAuthzOwnerID), shared.ID)
		}, false},
		{"member", func() (bool, error) {
			return e.svc.VendorAccountUsageManager(e.ctx, e.member, shared.ID)
		}, false},
		{"stranger", func() (bool, error) {
			return e.svc.VendorAccountUsageManager(e.ctx, token("usr_stranger"), shared.ID)
		}, false},
		{"unknown_id", func() (bool, error) {
			return e.svc.VendorAccountUsageManager(e.ctx, token("usr_cm", "admin"), "vacc_does_not_exist")
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.tok()
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}

	// A CM link that outlives its level grants nothing: lower the account to
	// private (the admin-group link stays) and the CM is no longer a manager.
	t.Run("cm_after_downgrade_to_private", func(t *testing.T) {
		if err := e.routes.UpdateVendorAccountShareLevel(e.ctx, shared.ID, routing.VendorShareLevelNone); err != nil {
			t.Fatalf("downgrade: %v", err)
		}
		got, err := e.svc.VendorAccountUsageManager(e.ctx, token("usr_cm", "admin"), shared.ID)
		if err != nil || got {
			t.Errorf("got %v, %v, want false, nil", got, err)
		}
	})
}
