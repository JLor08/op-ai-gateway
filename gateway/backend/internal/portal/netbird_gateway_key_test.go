// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestCreateGatewaySetupKey: on a module-configured Service, minting a gateway
// setup key returns a non-empty key + the ready-to-paste `netbird up` command,
// resolve-or-creates the "op-gw-portal" gateway group, and puts THAT group id in
// the key's auto_groups (and nothing else). Mutation-check: (a) using a different
// group name would leave op-gw-portal uncreated → groupIDByName == "" fails; (b)
// omitting the group from auto_groups → the auto_groups assertion fails.
func TestCreateGatewaySetupKey(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)

	key, command, err := svc.CreateGatewaySetupKey(context.Background(), systemToken())
	if err != nil {
		t.Fatalf("CreateGatewaySetupKey = %v, want nil", err)
	}
	if key != "nbkey-secret-value" {
		t.Fatalf("key = %q, want the generated key", key)
	}
	if want := "netbird up --management-url " + fake.srv.URL + " --setup-key nbkey-secret-value"; command != want {
		t.Fatalf("command = %q, want %q", command, want)
	}

	// The "op-gw-portal" gateway group was resolve-or-created.
	gid := fake.groupIDByName("op-gw-portal")
	if gid == "" {
		t.Fatalf("op-gw-portal group not resolved/created (groups=%v)", fake.autoGroups())
	}
	// The setup key's auto_groups is EXACTLY that group id (no more, no less).
	got := fake.autoGroups()
	if len(got) != 1 || got[0] != gid {
		t.Fatalf("auto_groups = %v, want [%q] (only the op-gw-portal group)", got, gid)
	}
}

// TestCreateGatewaySetupKeyModuleDisabled: with the module off the mint returns
// ErrNetbirdModuleDisabled and makes NO NetBird call.
func TestCreateGatewaySetupKeyModuleDisabled(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	// Configured but DISABLED: if the mint wrongly ran it would hit the fake.
	enableNetbird(t, svc, fake.srv.URL, false)

	if _, _, err := svc.CreateGatewaySetupKey(context.Background(), systemToken()); !errors.Is(err, ErrNetbirdModuleDisabled) {
		t.Fatalf("CreateGatewaySetupKey(module off) = %v, want ErrNetbirdModuleDisabled", err)
	}
	if fake.count() != 0 {
		t.Fatalf("netbird requests = %d, want 0 (module off => no call)", fake.count())
	}
}
