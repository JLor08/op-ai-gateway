// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package account

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"testing"
	"time"
)

func newTestService(t *testing.T) (*Service, *portal.MemoryDirectory, *fixedClock) {
	t.Helper()
	dir := portal.NewMemoryDirectory(auth.NewTokenStore())
	clock := &fixedClock{now: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)}
	svc := NewService(Deps{
		Users:             dir,
		Sessions:          dir,
		SetPasswordTokens: dir,
		SettingsVolatile:  true,
	}, Config{
		IdleTTL:         12 * time.Hour,
		MaxTTL:          168 * time.Hour,
		Clock:           clock.Now,
		SecretGenerator: sequentialSecrets(),
		IDGenerator:     sequentialIDs(),
	})
	return svc, dir, clock
}

type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time { return c.now }

func sequentialSecrets() func() (string, error) {
	n := 0
	return func() (string, error) { n++; return "secret-" + itoa(n), nil }
}

func sequentialIDs() func() string {
	n := 0
	return func() string { n++; return "id-" + itoa(n) }
}

func itoa(n int) string { return time.Duration(n).String() }

func ptr[T any](v T) *T { return &v }

func seedActiveUser(t *testing.T, dir *portal.MemoryDirectory, id, email, password, role string) {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	now := time.Now().UTC()
	if err := dir.CreateUser(context.Background(), store.User{ID: id, Email: email, DisplayName: id, Role: role, Status: store.UserStatusActive, PreferredLanguage: "de", PasswordHash: hash, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

func TestLoginAndResolveSession(t *testing.T) {
	svc, dir, clock := newTestService(t)
	seedActiveUser(t, dir, "usr_1", "a@example.test", "password-1", "admin")

	_, _, err := svc.Login(context.Background(), "a@example.test", "wrong-password")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password should be invalid, got %v", err)
	}
	user, secret, err := svc.Login(context.Background(), "A@Example.test", "password-1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if user.ID != "usr_1" || secret == "" {
		t.Fatalf("unexpected login result: %+v secret=%q", user, secret)
	}

	resolved, err := svc.ResolveSession(context.Background(), secret)
	if err != nil {
		t.Fatalf("resolve session: %v", err)
	}
	if resolved.ID != "usr_1" {
		t.Fatalf("resolved wrong user: %+v", resolved)
	}

	// Idle expiry.
	clock.now = clock.now.Add(13 * time.Hour)
	if _, err := svc.ResolveSession(context.Background(), secret); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("idle session should be invalid, got %v", err)
	}
}

func TestAuthenticatePasswordAndIssueSession(t *testing.T) {
	svc, dir, _ := newTestService(t)
	seedActiveUser(t, dir, "usr_1", "a@example.test", "password-1", "admin")

	if _, err := svc.AuthenticatePassword(context.Background(), "a@example.test", "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password should be invalid, got %v", err)
	}

	u, err := svc.AuthenticatePassword(context.Background(), "a@example.test", "password-1")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if u.ID != "usr_1" {
		t.Fatalf("unexpected authenticated user: %+v", u)
	}

	// No session should have been created yet.
	if _, err := svc.ResolveSession(context.Background(), "secret-1"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("expected no session yet, got %v", err)
	}

	secret, err := svc.IssueSession(context.Background(), u)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	if secret == "" {
		t.Fatalf("expected non-empty secret")
	}
	resolved, err := svc.ResolveSession(context.Background(), secret)
	if err != nil {
		t.Fatalf("resolve session: %v", err)
	}
	if resolved.ID != "usr_1" {
		t.Fatalf("resolved wrong user: %+v", resolved)
	}
}

func TestLoginRejectsDisabledUser(t *testing.T) {
	svc, dir, _ := newTestService(t)
	seedActiveUser(t, dir, "usr_d", "d@example.test", "password-1", "user")
	u, _ := dir.UserByEmail(context.Background(), "d@example.test")
	u.Status = store.UserStatusDisabled
	if err := dir.UpdateUser(context.Background(), u); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, _, err := svc.Login(context.Background(), "d@example.test", "password-1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("disabled login should be invalid, got %v", err)
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	svc, dir, _ := newTestService(t)
	seedActiveUser(t, dir, "usr_l", "l@example.test", "password-1", "user")
	_, secret, err := svc.Login(context.Background(), "l@example.test", "password-1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := svc.Logout(context.Background(), secret); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := svc.ResolveSession(context.Background(), secret); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("logged-out session should be invalid, got %v", err)
	}
	_ = dir
}

func seedInvitedUser(t *testing.T, svc *Service, dir *portal.MemoryDirectory, id, email string) string {
	t.Helper()
	now := time.Now().UTC()
	if err := dir.CreateUser(context.Background(), store.User{ID: id, Email: email, DisplayName: id, Role: "user", Status: store.UserStatusInvited, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed invited user: %v", err)
	}
	secret, err := svc.issueSetPasswordToken(context.Background(), id, now)
	if err != nil {
		t.Fatalf("issue invite: %v", err)
	}
	return secret
}

func TestSetPasswordRedeemsInvite(t *testing.T) {
	svc, dir, _ := newTestService(t)
	secret := seedInvitedUser(t, svc, dir, "usr_i", "i@example.test")

	if _, _, err := svc.SetPassword(context.Background(), secret, "short"); !errors.Is(err, auth.ErrPasswordTooWeak) {
		t.Fatalf("weak password should fail, got %v", err)
	}
	user, sessionSecret, err := svc.SetPassword(context.Background(), secret, "password-123")
	if err != nil {
		t.Fatalf("set password: %v", err)
	}
	if user.Status != store.UserStatusActive || sessionSecret == "" {
		t.Fatalf("user should be active with a session: %+v %q", user, sessionSecret)
	}
	// Single-use.
	if _, _, err := svc.SetPassword(context.Background(), secret, "password-123"); !errors.Is(err, ErrSetPasswordTokenInvalid) {
		t.Fatalf("reused token should be invalid, got %v", err)
	}
	// Login now works.
	if _, _, err := svc.Login(context.Background(), "i@example.test", "password-123"); err != nil {
		t.Fatalf("login after set-password: %v", err)
	}
}

func TestChangePassword(t *testing.T) {
	svc, dir, _ := newTestService(t)
	seedActiveUser(t, dir, "usr_c", "c@example.test", "password-old", "user")
	if err := svc.ChangePassword(context.Background(), "usr_c", "wrong", "password-new"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong current password should fail, got %v", err)
	}
	if err := svc.ChangePassword(context.Background(), "usr_c", "password-old", "short"); !errors.Is(err, auth.ErrPasswordTooWeak) {
		t.Fatalf("weak new password should fail, got %v", err)
	}
	if err := svc.ChangePassword(context.Background(), "usr_c", "password-old", "password-new"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if _, _, err := svc.Login(context.Background(), "c@example.test", "password-new"); err != nil {
		t.Fatalf("login with new password: %v", err)
	}
}

func TestInviteUserAndReissue(t *testing.T) {
	svc, _, _ := newTestService(t)
	user, secret, err := svc.InviteUser(context.Background(), InviteInput{Email: "New@Example.test", DisplayName: "New", Role: "user"}, true)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if user.Status != store.UserStatusInvited || user.Email != "new@example.test" || secret == "" {
		t.Fatalf("unexpected invite result: %+v %q", user, secret)
	}
	if _, _, err := svc.InviteUser(context.Background(), InviteInput{Email: "new@example.test", DisplayName: "Dup", Role: "user"}, true); !errors.Is(err, ErrUserConflict) {
		t.Fatalf("duplicate email should conflict, got %v", err)
	}
	if _, _, err := svc.InviteUser(context.Background(), InviteInput{Email: "", DisplayName: "x", Role: "user"}, true); !errors.Is(err, ErrEmailRequired) {
		t.Fatalf("blank email should fail, got %v", err)
	}
	if _, _, err := svc.InviteUser(context.Background(), InviteInput{Email: "bad@example.test", DisplayName: "x", Role: "superadmin"}, true); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("bad role should fail, got %v", err)
	}
	// Reissue invalidates the previous invite.
	_, newSecret, err := svc.ReissueInvite(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("reissue: %v", err)
	}
	if _, _, err := svc.SetPassword(context.Background(), secret, "password-123"); !errors.Is(err, ErrSetPasswordTokenInvalid) {
		t.Fatalf("old invite should be invalid after reissue, got %v", err)
	}
	if _, _, err := svc.SetPassword(context.Background(), newSecret, "password-123"); err != nil {
		t.Fatalf("new invite should work: %v", err)
	}
}

func TestUpdateUserAppliesChatFlags(t *testing.T) {
	svc, dir, _ := newTestService(t)
	seedActiveUser(t, dir, "usr_chat", "chat@example.test", "password-1", "user")

	// Both pointers set -> both applied.
	updated, err := svc.UpdateUser(context.Background(), "usr_chat", UserUpdate{ChatLogCommunication: ptr(true), ChatSecret: ptr(true)}, false)
	if err != nil {
		t.Fatalf("apply chat flags: %v", err)
	}
	if !updated.ChatLogCommunication || !updated.ChatSecret {
		t.Fatalf("chat flags not applied: %+v", updated)
	}

	// Nil pointers leave the stored values unchanged.
	unchanged, err := svc.UpdateUser(context.Background(), "usr_chat", UserUpdate{}, false)
	if err != nil {
		t.Fatalf("nil update: %v", err)
	}
	if !unchanged.ChatLogCommunication || !unchanged.ChatSecret {
		t.Fatalf("nil chat-flag pointers must leave values unchanged: %+v", unchanged)
	}

	// A single pointer updates only that flag.
	off, err := svc.UpdateUser(context.Background(), "usr_chat", UserUpdate{ChatLogCommunication: ptr(false)}, false)
	if err != nil {
		t.Fatalf("selective update: %v", err)
	}
	if off.ChatLogCommunication || !off.ChatSecret {
		t.Fatalf("selective chat-flag update wrong: %+v", off)
	}
}

func TestUpdateUserLastAdminGuard(t *testing.T) {
	svc, dir, _ := newTestService(t)
	seedActiveUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")
	seedActiveUser(t, dir, "usr_user", "user@example.test", "password-1", "user")

	// Cannot demote the only admin.
	role := "user"
	if _, err := svc.UpdateUser(context.Background(), "usr_admin", UserUpdate{Role: &role}, true); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demoting last admin should fail, got %v", err)
	}
	// Cannot disable the only admin.
	status := store.UserStatusDisabled
	if _, err := svc.UpdateUser(context.Background(), "usr_admin", UserUpdate{Status: &status}, true); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("disabling last admin should fail, got %v", err)
	}
	// Promote the second user to admin, then the first can be demoted.
	adminRole := "admin"
	if _, err := svc.UpdateUser(context.Background(), "usr_user", UserUpdate{Role: &adminRole}, true); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if _, err := svc.UpdateUser(context.Background(), "usr_admin", UserUpdate{Role: &role}, true); err != nil {
		t.Fatalf("demote after second admin exists: %v", err)
	}
}

func TestUpdateUserDisableRevokesSessions(t *testing.T) {
	svc, dir, _ := newTestService(t)
	seedActiveUser(t, dir, "usr_a1", "a1@example.test", "password-1", "admin")
	seedActiveUser(t, dir, "usr_target", "target@example.test", "password-1", "user")
	_, secret, err := svc.Login(context.Background(), "target@example.test", "password-1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	status := store.UserStatusDisabled
	if _, err := svc.UpdateUser(context.Background(), "usr_target", UserUpdate{Status: &status}, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := svc.ResolveSession(context.Background(), secret); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("disabled user session should be revoked, got %v", err)
	}
	_ = dir
}

func TestSetPasswordRejectsDisabledUser(t *testing.T) {
	svc, dir, _ := newTestService(t)
	user, secret, err := svc.InviteUser(context.Background(), InviteInput{Email: "d@example.test", DisplayName: "D", Role: "user"}, true)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	u, err := dir.UserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	u.Status = store.UserStatusDisabled
	if err := dir.UpdateUser(context.Background(), u); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, _, err := svc.SetPassword(context.Background(), secret, "password-123"); !errors.Is(err, ErrSetPasswordTokenInvalid) {
		t.Fatalf("disabled user should not be able to set password, got %v", err)
	}
	reloaded, err := dir.UserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if reloaded.Status != store.UserStatusDisabled {
		t.Fatalf("user should still be disabled, got %q", reloaded.Status)
	}
}

func TestDisableUserInvalidatesSetPasswordTokens(t *testing.T) {
	svc, dir, _ := newTestService(t)
	seedActiveUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")
	user, secret, err := svc.InviteUser(context.Background(), InviteInput{Email: "e@example.test", DisplayName: "E", Role: "user"}, true)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	disabledStatus := store.UserStatusDisabled
	if _, err := svc.UpdateUser(context.Background(), user.ID, UserUpdate{Status: &disabledStatus}, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, _, err := svc.SetPassword(context.Background(), secret, "password-123"); !errors.Is(err, ErrSetPasswordTokenInvalid) {
		t.Fatalf("set-password token should be invalidated by disable, got %v", err)
	}
}

func TestInviteUserSystemAdminRequiresSystemActor(t *testing.T) {
	svc, _, _ := newTestService(t)

	// A non-system actor cannot invite a system_admin.
	if _, _, err := svc.InviteUser(context.Background(), InviteInput{Email: "a@x.test", Role: "system_admin"}, false); !errors.Is(err, ErrForbiddenRole) {
		t.Fatalf("non-system actor inviting system_admin should be forbidden, got %v", err)
	}
	// A system actor can.
	user, _, err := svc.InviteUser(context.Background(), InviteInput{Email: "a@x.test", Role: "system_admin"}, true)
	if err != nil {
		t.Fatalf("system actor inviting system_admin should succeed, got %v", err)
	}
	if user.Role != "system_admin" {
		t.Fatalf("invited user role = %q, want system_admin", user.Role)
	}
}

func TestUpdateUserSystemAdminTransitionRequiresSystemActor(t *testing.T) {
	svc, dir, _ := newTestService(t)
	seedActiveUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")

	// Promoting an admin to system_admin requires a system actor.
	if _, err := svc.UpdateUser(context.Background(), "usr_admin", UserUpdate{Role: ptr("system_admin")}, false); !errors.Is(err, ErrForbiddenRole) {
		t.Fatalf("non-system actor promoting to system_admin should be forbidden, got %v", err)
	}
	if _, err := svc.UpdateUser(context.Background(), "usr_admin", UserUpdate{Role: ptr("system_admin")}, true); err != nil {
		t.Fatalf("system actor promoting to system_admin should succeed, got %v", err)
	}

	// Demoting a system_admin is also guarded.
	seedActiveUser(t, dir, "usr_sys", "sys@example.test", "password-1", "system_admin")
	if _, err := svc.UpdateUser(context.Background(), "usr_sys", UserUpdate{Role: ptr("admin")}, false); !errors.Is(err, ErrForbiddenRole) {
		t.Fatalf("non-system actor demoting a system_admin should be forbidden, got %v", err)
	}
}

// TestUpdateUserForbidsNonSystemActorAgainstSystemAdmin proves the guarantee the
// admin-group support-operations policy relies on (spec 2026-08-10 follow-up,
// revised): a non-system actor may NOT edit OR deactivate a system_admin, even
// with NO role change in the request -- the guard keys on the TARGET's current
// role, so a plain profile edit and a Status=disabled deactivate are both
// rejected. (The allowed support operations -- limits / re-invite / TOTP reset --
// do not flow through UpdateUser, so they are unaffected.)
func TestUpdateUserForbidsNonSystemActorAgainstSystemAdmin(t *testing.T) {
	svc, dir, _ := newTestService(t)
	seedActiveUser(t, dir, "usr_sys", "sys@example.test", "password-1", "system_admin")

	// Edit (profile field, no role change) by a non-system actor -> forbidden.
	if _, err := svc.UpdateUser(context.Background(), "usr_sys", UserUpdate{DisplayName: ptr("Renamed")}, false); !errors.Is(err, ErrForbiddenRole) {
		t.Fatalf("non-system actor editing a system_admin should be forbidden, got %v", err)
	}
	// Deactivate (Status=disabled, no role change) by a non-system actor -> forbidden.
	if _, err := svc.UpdateUser(context.Background(), "usr_sys", UserUpdate{Status: ptr("disabled")}, false); !errors.Is(err, ErrForbiddenRole) {
		t.Fatalf("non-system actor deactivating a system_admin should be forbidden, got %v", err)
	}
	// Positive control: a system actor CAN edit the system_admin's profile.
	if _, err := svc.UpdateUser(context.Background(), "usr_sys", UserUpdate{DisplayName: ptr("Renamed")}, true); err != nil {
		t.Fatalf("system actor editing a system_admin should succeed, got %v", err)
	}
}

func TestLastSystemAdminGuard(t *testing.T) {
	svc, dir, _ := newTestService(t)
	// One active system_admin plus other non-system users.
	seedActiveUser(t, dir, "usr_sys", "sys@example.test", "password-1", "system_admin")
	seedActiveUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")
	seedActiveUser(t, dir, "usr_user", "user@example.test", "password-1", "user")

	// Demoting the only system_admin fails.
	if _, err := svc.UpdateUser(context.Background(), "usr_sys", UserUpdate{Role: ptr("admin")}, true); !errors.Is(err, ErrLastSystemAdmin) {
		t.Fatalf("demoting last system_admin should fail, got %v", err)
	}
	// Disabling the only system_admin fails.
	if _, err := svc.UpdateUser(context.Background(), "usr_sys", UserUpdate{Status: ptr(store.UserStatusDisabled)}, true); !errors.Is(err, ErrLastSystemAdmin) {
		t.Fatalf("disabling last system_admin should fail, got %v", err)
	}

	// With a second active system_admin, demoting one succeeds.
	seedActiveUser(t, dir, "usr_sys2", "sys2@example.test", "password-1", "system_admin")
	if _, err := svc.UpdateUser(context.Background(), "usr_sys", UserUpdate{Role: ptr("admin")}, true); err != nil {
		t.Fatalf("demoting one of two system_admins should succeed, got %v", err)
	}
}

func TestLastAdminGuardCountsSystemAdmin(t *testing.T) {
	svc, dir, _ := newTestService(t)
	seedActiveUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")
	seedActiveUser(t, dir, "usr_sys", "sys@example.test", "password-1", "system_admin")

	// Disabling the plain admin succeeds because the system_admin is admin-capable,
	// so the last-admin guard does not trip.
	if _, err := svc.UpdateUser(context.Background(), "usr_admin", UserUpdate{Status: ptr(store.UserStatusDisabled)}, true); err != nil {
		t.Fatalf("disabling admin while a system_admin remains should succeed, got %v", err)
	}
}

func TestSystemAdminModeEnterExit(t *testing.T) {
	svc, dir, clock := newTestService(t)
	ctx := context.Background()
	seedActiveUser(t, dir, "usr_sa", "sa@example.test", "password-123", "system_admin")
	u, err := dir.UserByID(ctx, "usr_sa")
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	secret, err := svc.IssueSession(ctx, u)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	// require-password: wrong password rejected.
	if err := svc.EnterSystemAdminMode(ctx, secret, "wrong", true); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password = %v, want ErrInvalidCredentials", err)
	}
	// require-password: correct password elevates.
	if err := svc.EnterSystemAdminMode(ctx, secret, "password-123", true); err != nil {
		t.Fatalf("enter (correct pw): %v", err)
	}
	_, sess, err := svc.ResolveSessionDetail(ctx, secret)
	if err != nil {
		t.Fatalf("ResolveSessionDetail: %v", err)
	}
	if !sess.ElevatedUntil.After(clock.Now().Add(10 * time.Minute)) {
		t.Fatalf("ElevatedUntil = %v, want ~now+15m", sess.ElevatedUntil)
	}
	// exit clears.
	if err := svc.ExitSystemAdminMode(ctx, secret); err != nil {
		t.Fatalf("exit: %v", err)
	}
	_, sess, _ = svc.ResolveSessionDetail(ctx, secret)
	if !sess.ElevatedUntil.IsZero() {
		t.Fatalf("ElevatedUntil after exit = %v, want zero", sess.ElevatedUntil)
	}
	// require-password=false: no password needed.
	if err := svc.EnterSystemAdminMode(ctx, secret, "", false); err != nil {
		t.Fatalf("enter (no-pw mode): %v", err)
	}
}

func TestSystemAdminModeRejectsNonSystemAdmin(t *testing.T) {
	svc, dir, _ := newTestService(t)
	ctx := context.Background()
	seedActiveUser(t, dir, "usr_a", "a@example.test", "password-123", "admin")
	u, err := dir.UserByID(ctx, "usr_a")
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	secret, err := svc.IssueSession(ctx, u)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	if err := svc.EnterSystemAdminMode(ctx, secret, "password-123", true); !errors.Is(err, ErrNotSystemAdmin) {
		t.Fatalf("admin enter = %v, want ErrNotSystemAdmin", err)
	}
	if err := svc.EnterSystemAdminMode(ctx, secret, "", false); !errors.Is(err, ErrNotSystemAdmin) {
		t.Fatalf("admin enter (no-pw) = %v, want ErrNotSystemAdmin", err)
	}
}
