// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package helper

import (
	"os"
	"os/user"
	"strconv"
	"strings"
	"testing"
)

// nonRootUser returns a non-root account to resolve: the current user if it
// is not root and has a passwd entry, otherwise "nobody".
func nonRootUser(t *testing.T) *user.User {
	t.Helper()
	if os.Getuid() != 0 {
		if u, err := user.LookupId(strconv.Itoa(os.Getuid())); err == nil {
			return u
		}
	}
	u, err := user.Lookup("nobody")
	if err != nil {
		t.Skipf("no non-root account with a passwd entry: %v", err)
	}
	return u
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("atoi(%q): %v", s, err)
	}
	return n
}

func TestResolveIdentityNumeric(t *testing.T) {
	u := nonRootUser(t)
	got, err := ResolveIdentity(u.Uid, "")
	if err != nil {
		t.Fatalf("ResolveIdentity(%q, \"\"): %v", u.Uid, err)
	}
	want := Identity{UID: atoi(t, u.Uid), GID: atoi(t, u.Gid)}
	if got != want {
		t.Fatalf("ResolveIdentity(%q) = %+v, want %+v (primary gid)", u.Uid, got, want)
	}
}

func TestResolveIdentityByName(t *testing.T) {
	u := nonRootUser(t)
	got, err := ResolveIdentity(u.Username, "")
	if err != nil {
		t.Fatalf("ResolveIdentity(%q, \"\"): %v", u.Username, err)
	}
	want := Identity{UID: atoi(t, u.Uid), GID: atoi(t, u.Gid)}
	if got != want {
		t.Fatalf("ResolveIdentity(%q) = %+v, want %+v", u.Username, got, want)
	}
}

// A numeric uid without a passwd entry is accepted as-is, but then the
// primary group cannot be derived and must be given explicitly.
func TestResolveIdentityNumericWithoutPasswdEntry(t *testing.T) {
	const ghost = "3999999"
	if _, err := user.LookupId(ghost); err == nil {
		t.Skipf("uid %s unexpectedly has a passwd entry", ghost)
	}
	got, err := ResolveIdentity(ghost, "3999998")
	if err != nil {
		t.Fatalf("ResolveIdentity(%q, explicit gid): %v", ghost, err)
	}
	if want := (Identity{UID: 3999999, GID: 3999998}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if _, err := ResolveIdentity(ghost, ""); err == nil {
		t.Fatalf("ResolveIdentity(%q, \"\") succeeded; want error (no primary gid)", ghost)
	}
}

func TestResolveIdentityRefusesRoot(t *testing.T) {
	for _, in := range []string{"0", "root"} {
		_, err := ResolveIdentity(in, "")
		if err == nil {
			t.Fatalf("ResolveIdentity(%q) succeeded; want error", in)
		}
		if !strings.Contains(err.Error(), "helper: -agent-user must not be root") {
			t.Fatalf("ResolveIdentity(%q) error = %q; want the must-not-be-root message", in, err)
		}
	}
	// Explicit group does not change the verdict.
	if _, err := ResolveIdentity("0", "1000"); err == nil {
		t.Fatal("ResolveIdentity(\"0\", \"1000\") succeeded; want error")
	}
}

func TestResolveIdentityRejectsBadInput(t *testing.T) {
	for _, in := range []string{"", "-5", "no-such-user-op-helper-x"} {
		if id, err := ResolveIdentity(in, "1000"); err == nil {
			t.Fatalf("ResolveIdentity(%q) = %+v; want error", in, id)
		}
	}
	u := nonRootUser(t)
	for _, g := range []string{"-5", "no-such-group-op-helper-x"} {
		if id, err := ResolveIdentity(u.Uid, g); err == nil {
			t.Fatalf("ResolveIdentity(%q, %q) = %+v; want error", u.Uid, g, id)
		}
	}
}

func TestResolveIdentityExplicitGroup(t *testing.T) {
	u := nonRootUser(t)
	gid := os.Getgid()
	got, err := ResolveIdentity(u.Uid, strconv.Itoa(gid))
	if err != nil {
		t.Fatalf("ResolveIdentity numeric group: %v", err)
	}
	if want := (Identity{UID: atoi(t, u.Uid), GID: gid}); got != want {
		t.Fatalf("numeric group: got %+v, want %+v", got, want)
	}

	// By name: gid 0's group (root/wheel) always has an entry and differs
	// from the user's primary group, so the explicit group visibly wins.
	g, err := user.LookupGroupId("0")
	if err != nil {
		t.Skipf("gid 0 has no group entry: %v", err)
	}
	got, err = ResolveIdentity(u.Uid, g.Name)
	if err != nil {
		t.Fatalf("ResolveIdentity group by name %q: %v", g.Name, err)
	}
	if want := (Identity{UID: atoi(t, u.Uid), GID: 0}); got != want {
		t.Fatalf("named group: got %+v, want %+v", got, want)
	}
}

func TestAuthorizePeer(t *testing.T) {
	want := Identity{UID: 1000, GID: 2000}
	cases := []struct {
		peer   int
		ok     bool
		reason string
	}{
		{1000, true, ""},
		{0, false, "uid mismatch"}, // root is not implicitly allowed
		{1001, false, "uid mismatch"},
		{2000, false, "uid mismatch"}, // the gid is never an authorization
		{-1, false, "uid mismatch"},
	}
	for _, c := range cases {
		ok, reason := authorizePeer(c.peer, want)
		if ok != c.ok || reason != c.reason {
			t.Errorf("authorizePeer(%d) = (%v, %q), want (%v, %q)", c.peer, ok, reason, c.ok, c.reason)
		}
	}
}
