// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package account

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"sync/atomic"
	"testing"
	"time"
)

// touchCountingSessions counts TouchSession writes on top of a real session
// store, so a test can tell a resolve that refreshes the session's last-seen
// time from one that does not.
type touchCountingSessions struct {
	SessionStore
	touches atomic.Int64
}

func (s *touchCountingSessions) TouchSession(ctx context.Context, id string, lastSeenAt time.Time) error {
	s.touches.Add(1)
	return s.SessionStore.TouchSession(ctx, id, lastSeenAt)
}

// TestResolveSessionDetailNoTouchValidatesWithoutWriting pins the passive
// resolve a high-fan-out read (the chat-image <img> GETs) uses: it answers the
// same question as ResolveSessionDetail -- who is this, is the session still
// valid -- but issues no TouchSession write, and therefore does not extend the
// idle window either.
func TestResolveSessionDetailNoTouchValidatesWithoutWriting(t *testing.T) {
	dir := portal.NewMemoryDirectory(auth.NewTokenStore())
	sessions := &touchCountingSessions{SessionStore: dir}
	clock := &fixedClock{now: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)}
	svc := NewService(Deps{Users: dir, Sessions: sessions, SetPasswordTokens: dir, SettingsVolatile: true}, Config{
		IdleTTL: 12 * time.Hour, MaxTTL: 168 * time.Hour, Clock: clock.Now,
		SecretGenerator: sequentialSecrets(), IDGenerator: sequentialIDs(),
	})
	ctx := context.Background()
	seedActiveUser(t, dir, "usr_1", "a@example.test", "password-1", "admin")
	_, secret, err := svc.Login(ctx, "a@example.test", "password-1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	user, session, err := svc.ResolveSessionDetailNoTouch(ctx, secret)
	if err != nil {
		t.Fatalf("no-touch resolve: %v", err)
	}
	if user.ID != "usr_1" || session.UserID != "usr_1" {
		t.Fatalf("resolved user %q / session user %q, want usr_1", user.ID, session.UserID)
	}
	if n := sessions.touches.Load(); n != 0 {
		t.Fatalf("the no-touch resolve wrote %d session touches, want 0", n)
	}

	// It does not extend the idle window: used at 11h, the session is still
	// valid, but being passive it leaves last_seen at the login time, so at 13h
	// it is idle-expired even though it was "used" two hours earlier.
	clock.now = clock.now.Add(11 * time.Hour)
	if _, _, err := svc.ResolveSessionDetailNoTouch(ctx, secret); err != nil {
		t.Fatalf("no-touch resolve at 11h: %v", err)
	}
	clock.now = clock.now.Add(2 * time.Hour)
	if _, _, err := svc.ResolveSessionDetailNoTouch(ctx, secret); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("idle-expired session via no-touch resolve: err = %v, want ErrSessionInvalid", err)
	}
	if n := sessions.touches.Load(); n != 0 {
		t.Fatalf("no-touch resolves wrote %d session touches in total, want 0", n)
	}
}

// TestResolveSessionDetailNoTouchRefusesWhatTheTouchingResolveRefuses: skipping
// the write must not loosen validation. An empty or unknown secret and a
// disabled user are ErrSessionInvalid, and the touching resolve (the control)
// is the one that writes.
func TestResolveSessionDetailNoTouchRefusesWhatTheTouchingResolveRefuses(t *testing.T) {
	dir := portal.NewMemoryDirectory(auth.NewTokenStore())
	sessions := &touchCountingSessions{SessionStore: dir}
	svc := NewService(Deps{Users: dir, Sessions: sessions, SetPasswordTokens: dir, SettingsVolatile: true}, Config{
		IdleTTL: 12 * time.Hour, MaxTTL: 168 * time.Hour,
	})
	ctx := context.Background()
	seedActiveUser(t, dir, "usr_1", "a@example.test", "password-1", "admin")
	_, secret, err := svc.Login(ctx, "a@example.test", "password-1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	for _, bad := range []string{"", "no-such-secret"} {
		if _, _, err := svc.ResolveSessionDetailNoTouch(ctx, bad); !errors.Is(err, ErrSessionInvalid) {
			t.Fatalf("secret %q: err = %v, want ErrSessionInvalid", bad, err)
		}
	}

	// Control: the touching resolve writes.
	if _, _, err := svc.ResolveSessionDetail(ctx, secret); err != nil {
		t.Fatalf("touching resolve: %v", err)
	}
	if n := sessions.touches.Load(); n != 1 {
		t.Fatalf("touching resolve wrote %d touches, want 1", n)
	}

	// A disabled user's session is refused (and cleaned up) like everywhere else.
	user, err := dir.UserByID(ctx, "usr_1")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	user.Status = store.UserStatusDisabled
	if err := dir.UpdateUser(ctx, user); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if _, _, err := svc.ResolveSessionDetailNoTouch(ctx, secret); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("disabled user: err = %v, want ErrSessionInvalid", err)
	}
	if _, err := dir.SessionBySecret(ctx, auth.HashSecret(secret)); err == nil {
		t.Fatal("the session of a disabled user must be deleted by the no-touch resolve too")
	}
}
