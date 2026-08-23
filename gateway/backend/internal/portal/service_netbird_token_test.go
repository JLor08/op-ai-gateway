// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/netbird"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- fake NetBird server (user + PAT endpoints) --------------------------

// fakeNetbirdToken is a minimal, stateful NetBird admin-API stand-in covering
// exactly the endpoints RotateNetbirdToken/NetbirdTokenStatus/resolveStoredTokenMeta
// need: /api/users (current-user resolution), /api/groups (Ping/verify), and the
// per-user PAT CRUD. It tracks created/deleted token ids and the Authorization
// header seen on the last /api/groups call so a test can assert the verify call
// used the NEW plain token.
type fakeNetbirdToken struct {
	t      *testing.T
	now    time.Time
	userID string

	mu             sync.Mutex
	tokens         map[string]*netbird.Token
	order          []string // insertion order, for the "sole token" resolution tests
	nextID         int
	deleted        []string
	createCount    int
	pingFail       bool
	lastGroupsAuth string

	srv *httptest.Server
}

func newFakeNetbirdToken(t *testing.T, now time.Time) *fakeNetbirdToken {
	t.Helper()
	f := &fakeNetbirdToken{
		t:      t,
		now:    now,
		userID: "u1",
		tokens: map[string]*netbird.Token{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/users", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": f.userID, "is_current": true, "is_service_user": false},
		})
	})
	mux.HandleFunc("/api/groups", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.lastGroupsAuth = r.Header.Get("Authorization")
		fail := f.pingFail
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	tokensPath := "/api/users/" + f.userID + "/tokens"
	mux.HandleFunc(tokensPath, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			out := make([]netbird.Token, 0, len(f.order))
			for _, id := range f.order {
				out = append(out, *f.tokens[id])
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(out)
		case http.MethodPost:
			var body struct {
				Name      string `json:"name"`
				ExpiresIn int    `json:"expires_in"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.createCount++
			f.nextID++
			id := fmt.Sprintf("tok-%d", f.nextID)
			exp := f.now.AddDate(0, 0, body.ExpiresIn).UTC().Format(time.RFC3339)
			tok := netbird.Token{ID: id, Name: body.Name, ExpirationDate: exp}
			f.tokens[id] = &tok
			f.order = append(f.order, id)
			f.mu.Unlock()
			plain := "nbp_" + id
			_ = json.NewEncoder(w).Encode(map[string]any{
				"plain_token":           plain,
				"personal_access_token": tok,
			})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc(tokensPath+"/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, tokensPath+"/")
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			tok, ok := f.tokens[id]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(*tok)
		case http.MethodDelete:
			f.mu.Lock()
			_, ok := f.tokens[id]
			if ok {
				delete(f.tokens, id)
				for i, existing := range f.order {
					if existing == id {
						f.order = append(f.order[:i], f.order[i+1:]...)
						break
					}
				}
				f.deleted = append(f.deleted, id)
			}
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// seedToken directly registers a token (bypassing CreateToken) so a test can set
// up a pre-existing "old" token without going through the rotation flow.
func (f *fakeNetbirdToken) seedToken(id string, expiresInDays int) netbird.Token {
	f.mu.Lock()
	defer f.mu.Unlock()
	exp := f.now.AddDate(0, 0, expiresInDays).UTC().Format(time.RFC3339)
	tok := netbird.Token{ID: id, Name: netbirdManagedTokenName, ExpirationDate: exp}
	f.tokens[id] = &tok
	f.order = append(f.order, id)
	return tok
}

func (f *fakeNetbirdToken) tokenExists(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.tokens[id]
	return ok
}

func (f *fakeNetbirdToken) wasDeleted(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.deleted {
		if d == id {
			return true
		}
	}
	return false
}

func (f *fakeNetbirdToken) tokenCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.order)
}

func (f *fakeNetbirdToken) createCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCount
}

func (f *fakeNetbirdToken) setPingFail(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pingFail = fail
}

func (f *fakeNetbirdToken) lastVerifyAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastGroupsAuth
}

// --- service test harness -------------------------------------------------

func newTokenTestService(t *testing.T, now time.Time, settings SystemSettingsStore) *Service {
	t.Helper()
	if settings == nil {
		settings = NewMemorySystemSettings()
	}
	dir := NewMemoryDirectory(auth.NewTokenStore())
	return NewService(ServiceDeps{
		Users:          dir,
		Routes:         routing.NewMemoryStore(),
		SystemSettings: settings,
		Cipher:         newTestCipher(t),
		Clock:          func() time.Time { return now },
	})
}

// failOnKeySettings wraps a SystemSettingsStore and makes SetSystemSetting fail
// (without persisting) for one specific key, once armed via setActive(true), to
// exercise RotateNetbirdToken's persist-failure rollback path without also
// breaking the initial setup writes (enableNetbird itself writes netbird_token).
type failOnKeySettings struct {
	*MemorySystemSettings
	mu      sync.Mutex
	failKey string
	active  bool
}

func newFailOnKeySettings(failKey string) *failOnKeySettings {
	return &failOnKeySettings{MemorySystemSettings: NewMemorySystemSettings(), failKey: failKey}
}

func (f *failOnKeySettings) setActive(active bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active = active
}

func (f *failOnKeySettings) SetSystemSetting(ctx context.Context, key, value string, now time.Time) error {
	f.mu.Lock()
	fail := f.active && key == f.failKey
	f.mu.Unlock()
	if fail {
		return errors.New("simulated store failure")
	}
	return f.MemorySystemSettings.SetSystemSetting(ctx, key, value, now)
}

// --- daysRemaining -----------------------------------------------------

func TestDaysRemainingRounding(t *testing.T) {
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		exp  string
		want int
	}{
		{"empty", "", 0},
		{"malformed", "not-a-date", 0},
		{"already-past", now.Add(-time.Hour).Format(time.RFC3339), 0},
		{"exactly-now", now.Format(time.RFC3339), 0},
		{"exactly-24h", now.Add(24 * time.Hour).Format(time.RFC3339), 1},
		{"36h-rounds-up-to-2", now.Add(36 * time.Hour).Format(time.RFC3339), 2},
		{"25h-rounds-up-to-2", now.Add(25 * time.Hour).Format(time.RFC3339), 2},
		{"one-second", now.Add(time.Second).Format(time.RFC3339), 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := daysRemaining(c.exp, now); got != c.want {
				t.Fatalf("daysRemaining(%q) = %d, want %d", c.exp, got, c.want)
			}
		})
	}
}

// --- RotateNetbirdToken: success -------------------------------------------

// TestRotateNetbirdTokenSuccess: with a known old token id, rotation creates a
// fresh token, verifies it (with the NEW plain token, per the header assertion),
// persists the sealed new token + its id/expiry, and best-effort deletes the old
// token. The plaintext token is never returned by the result.
func TestRotateNetbirdTokenSuccess(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	fake := newFakeNetbirdToken(t, now)
	old := fake.seedToken("old-1", 5) // old token, 5 days left
	svc := newTokenTestService(t, now, nil)
	enableNetbird(t, svc, fake.srv.URL, true)
	svc.waitNetbirdResolve()
	// Pin the stored id directly to the seeded old token (resolve-on-save would
	// have set it already since there's exactly one token, but assert it here so
	// the test doesn't depend on that side channel).
	mustSetSetting(t, svc, netbirdTokenIDKey, old.ID)
	mustSetSetting(t, svc, netbirdTokenExpiresAtKey, old.ExpirationDate)

	res, err := svc.RotateNetbirdToken(context.Background(), systemToken())
	if err != nil {
		t.Fatalf("RotateNetbirdToken = %v, want nil", err)
	}
	if res.OldUnknown {
		t.Fatalf("OldUnknown = true, want false (old id was known)")
	}
	if !res.OldDeleted {
		t.Fatalf("OldDeleted = false, want true")
	}
	if res.DaysRemaining != 365 {
		t.Fatalf("DaysRemaining = %d, want 365", res.DaysRemaining)
	}
	wantExp := now.AddDate(0, 0, netbirdTokenValidityDays).UTC().Format(time.RFC3339)
	if res.ExpirationDate != wantExp {
		t.Fatalf("ExpirationDate = %q, want %q", res.ExpirationDate, wantExp)
	}
	// The plaintext token must never appear on the result (only metadata fields
	// exist on the struct — a compile-time guarantee — but also assert none of the
	// created plain values leak via JSON marshal, in case a future field is added
	// carelessly).
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "nbp_") {
		t.Fatalf("result JSON leaks a plaintext token: %s", raw)
	}

	// Old token deleted in NetBird; the new one exists and is NOT the old id.
	if !fake.wasDeleted(old.ID) {
		t.Fatalf("old token %s was not deleted", old.ID)
	}
	if fake.tokenCount() != 1 {
		t.Fatalf("token count = %d, want 1 (only the new token remains)", fake.tokenCount())
	}

	// The verify call (Ping) used the NEW plain token, not the old stored one.
	auth := fake.lastVerifyAuth()
	if auth == "" || auth == "Token nbtok" {
		t.Fatalf("verify Authorization = %q, want the NEW plain token, not the old stored one", auth)
	}
	if !strings.HasPrefix(auth, "Token nbp_") {
		t.Fatalf("verify Authorization = %q, want prefix %q", auth, "Token nbp_")
	}

	// The stored (sealed) token now decrypts to a value consistent with the new
	// token's Authorization header used for the verify call.
	cfg, ok, err := svc.NetbirdConfig(context.Background())
	if err != nil || !ok {
		t.Fatalf("NetbirdConfig after rotate: ok=%v err=%v", ok, err)
	}
	if "Token "+cfg.Token != auth {
		t.Fatalf("stored token %q does not match the verified plain token (auth=%q)", cfg.Token, auth)
	}

	// The stored id/expiry now point at the new token.
	values, err := svc.settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if values[netbirdTokenIDKey] == old.ID {
		t.Fatalf("netbird_token_id still points at the old (deleted) token")
	}
	if values[netbirdTokenExpiresAtKey] != wantExp {
		t.Fatalf("netbird_token_expires_at = %q, want %q", values[netbirdTokenExpiresAtKey], wantExp)
	}
}

// TestRotateNetbirdTokenForbidsNonSystem proves the PT-2 Part 2b internal authz
// guard: a principal without the "system" scope is rejected with
// ErrPrincipalForbidden and makes ZERO NetBird calls -- neither a new token nor
// a delete of the old one. The HTTP-level gate (requireWebScope("system") in
// handleSystemNetbirdRotateToken) is defense-in-depth on TOP of this, not
// instead of it.
func TestRotateNetbirdTokenForbidsNonSystem(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	fake := newFakeNetbirdToken(t, now)
	old := fake.seedToken("old-1", 5)
	svc := newTokenTestService(t, now, nil)
	enableNetbird(t, svc, fake.srv.URL, true)
	svc.waitNetbirdResolve()
	mustSetSetting(t, svc, netbirdTokenIDKey, old.ID)
	mustSetSetting(t, svc, netbirdTokenExpiresAtKey, old.ExpirationDate)

	for _, tc := range []struct {
		name string
		tok  auth.Token
	}{
		{"plain admin (no system scope)", adminToken()},
		{"owner", ownerToken()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.RotateNetbirdToken(context.Background(), tc.tok); !errors.Is(err, ErrPrincipalForbidden) {
				t.Fatalf("RotateNetbirdToken(%s) err = %v, want ErrPrincipalForbidden", tc.name, err)
			}
		})
	}
	if fake.wasDeleted(old.ID) {
		t.Fatal("old token was deleted despite ErrPrincipalForbidden")
	}
	if fake.tokenCount() != 1 {
		t.Fatalf("token count = %d, want 1 (no new token minted)", fake.tokenCount())
	}
	values, err := svc.settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if values[netbirdTokenIDKey] != old.ID {
		t.Fatalf("netbird_token_id = %q, want it unchanged at %q", values[netbirdTokenIDKey], old.ID)
	}

	// The flip side: a system-scoped principal succeeds exactly as before the
	// guard was added.
	if _, err := svc.RotateNetbirdToken(context.Background(), systemToken()); err != nil {
		t.Fatalf("RotateNetbirdToken(system): %v", err)
	}
	if !fake.wasDeleted(old.ID) {
		t.Fatal("system principal must still be able to rotate (old token should now be deleted)")
	}
}

// --- RotateNetbirdToken: verify failure -> rollback ------------------------

// TestRotateNetbirdTokenVerifyFailure: the fake accepts the create call but
// rejects the verify (Ping/groups) call with the new token. The orphan new token
// must be deleted and the stored token/id/expiry must be UNCHANGED; an error is
// returned.
func TestRotateNetbirdTokenVerifyFailure(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	fake := newFakeNetbirdToken(t, now)
	old := fake.seedToken("old-1", 5)
	svc := newTokenTestService(t, now, nil)
	enableNetbird(t, svc, fake.srv.URL, true)
	svc.waitNetbirdResolve()
	mustSetSetting(t, svc, netbirdTokenIDKey, old.ID)
	mustSetSetting(t, svc, netbirdTokenExpiresAtKey, old.ExpirationDate)

	before, err := svc.settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	beforeToken, beforeID, beforeExp := before[netbirdTokenKey], before[netbirdTokenIDKey], before[netbirdTokenExpiresAtKey]

	fake.setPingFail(true)
	_, err = svc.RotateNetbirdToken(context.Background(), systemToken())
	if err == nil {
		t.Fatalf("RotateNetbirdToken = nil, want an error (verify failed)")
	}
	if !errors.Is(err, netbird.ErrAuth) {
		t.Fatalf("err = %v, want netbird.ErrAuth", err)
	}

	// Exactly one token was created (the orphan) and it must now be deleted; the
	// old token must survive untouched.
	if fake.createCalls() != 1 {
		t.Fatalf("createCalls = %d, want 1", fake.createCalls())
	}
	if fake.tokenCount() != 1 {
		t.Fatalf("token count = %d, want 1 (only the surviving old token)", fake.tokenCount())
	}
	if !fake.tokenExists(old.ID) {
		t.Fatalf("old token %s was deleted, want it to survive", old.ID)
	}
	if fake.wasDeleted(old.ID) {
		t.Fatalf("old token %s was deleted, want it untouched", old.ID)
	}

	// The stored token/id/expiry are exactly what they were before the attempt.
	after, err := svc.settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if after[netbirdTokenKey] != beforeToken {
		t.Fatalf("netbird_token changed on verify failure: before=%q after=%q", beforeToken, after[netbirdTokenKey])
	}
	if after[netbirdTokenIDKey] != beforeID {
		t.Fatalf("netbird_token_id changed on verify failure: before=%q after=%q", beforeID, after[netbirdTokenIDKey])
	}
	if after[netbirdTokenExpiresAtKey] != beforeExp {
		t.Fatalf("netbird_token_expires_at changed on verify failure: before=%q after=%q", beforeExp, after[netbirdTokenExpiresAtKey])
	}
}

// --- RotateNetbirdToken: persist failure -> rollback -----------------------

// TestRotateNetbirdTokenPersistFailureRollback: create + verify succeed, but the
// FIRST persistence write (netbird_token_id) fails. The orphan new token must be
// deleted, the stored token/id/expiry must be UNCHANGED, and the old token must
// survive (the code never reaches the old-delete step).
func TestRotateNetbirdTokenPersistFailureRollback(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	fake := newFakeNetbirdToken(t, now)
	old := fake.seedToken("old-1", 5)

	settings := newFailOnKeySettings(netbirdTokenIDKey)
	svc := newTokenTestService(t, now, settings)
	enableNetbird(t, svc, fake.srv.URL, true) // setup writes succeed (not yet armed)
	svc.waitNetbirdResolve()
	mustSetSetting(t, svc, netbirdTokenIDKey, old.ID)
	mustSetSetting(t, svc, netbirdTokenExpiresAtKey, old.ExpirationDate)

	before, err := svc.settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	beforeToken, beforeID, beforeExp := before[netbirdTokenKey], before[netbirdTokenIDKey], before[netbirdTokenExpiresAtKey]

	settings.setActive(true)
	_, err = svc.RotateNetbirdToken(context.Background(), systemToken())
	if err == nil {
		t.Fatalf("RotateNetbirdToken = nil, want an error (persist failed)")
	}
	if err.Error() != "simulated store failure" {
		t.Fatalf("err = %v, want the simulated store failure", err)
	}

	if fake.createCalls() != 1 {
		t.Fatalf("createCalls = %d, want 1", fake.createCalls())
	}
	if fake.tokenCount() != 1 || !fake.tokenExists(old.ID) {
		t.Fatalf("expected only the old token %s to survive, tokens=%v", old.ID, fake.order)
	}
	if fake.wasDeleted(old.ID) {
		t.Fatalf("old token %s was deleted, want it untouched (persist failure never reaches old-delete)", old.ID)
	}

	settings.setActive(false) // read back without the fault armed
	after, err := svc.settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if after[netbirdTokenKey] != beforeToken || after[netbirdTokenIDKey] != beforeID || after[netbirdTokenExpiresAtKey] != beforeExp {
		t.Fatalf("stored token/id/expiry changed on persist failure: before=(%q,%q,%q) after=(%q,%q,%q)",
			beforeToken, beforeID, beforeExp, after[netbirdTokenKey], after[netbirdTokenIDKey], after[netbirdTokenExpiresAtKey])
	}
}

// TestRotateNetbirdTokenPersistFailureMidLoopKeepsOldToken: create + verify
// succeed and the FIRST metadata write (netbird_token_id) succeeds, but a LATER
// write (netbird_token_expires_at) fails. Because the credential (netbird_token)
// is written LAST, it must still hold the OLD token — deleting the new orphan
// then never bricks the module. Regression guard for the write-ordering fix: with
// the old token-first ordering, netbird_token would already be the new token that
// the rollback deletes, leaving a bricked module.
func TestRotateNetbirdTokenPersistFailureMidLoopKeepsOldToken(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	fake := newFakeNetbirdToken(t, now)
	old := fake.seedToken("old-1", 5)

	settings := newFailOnKeySettings(netbirdTokenExpiresAtKey)
	svc := newTokenTestService(t, now, settings)
	enableNetbird(t, svc, fake.srv.URL, true) // setup writes succeed (not yet armed)
	svc.waitNetbirdResolve()
	mustSetSetting(t, svc, netbirdTokenIDKey, old.ID)
	mustSetSetting(t, svc, netbirdTokenExpiresAtKey, old.ExpirationDate)

	before, err := svc.settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	beforeToken := before[netbirdTokenKey]

	settings.setActive(true)
	if _, err = svc.RotateNetbirdToken(context.Background(), systemToken()); err == nil {
		t.Fatalf("RotateNetbirdToken = nil, want an error (mid-loop persist failed)")
	}

	// The new token must be created then deleted (orphan cleanup); only the old
	// token survives, and it is never deleted.
	if fake.createCalls() != 1 {
		t.Fatalf("createCalls = %d, want 1", fake.createCalls())
	}
	if fake.tokenCount() != 1 || !fake.tokenExists(old.ID) {
		t.Fatalf("expected only the old token %s to survive, tokens=%v", old.ID, fake.order)
	}
	if fake.wasDeleted(old.ID) {
		t.Fatalf("old token %s was deleted, want it untouched", old.ID)
	}

	// THE brick-detector: the stored credential must still be the OLD token, not
	// the new one that was just deleted from NetBird.
	settings.setActive(false)
	after, err := svc.settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if after[netbirdTokenKey] != beforeToken {
		t.Fatalf("stored netbird_token changed on a mid-loop persist failure (module would be bricked): before=%q after=%q", beforeToken, after[netbirdTokenKey])
	}
}

// --- RotateNetbirdToken: unknown old id ------------------------------------

// TestRotateNetbirdTokenUnknownOldID: no stored old id, and the user has more
// than one token in NetBird (ambiguous) -> OldUnknown:true and no delete call is
// made for any of them.
func TestRotateNetbirdTokenUnknownOldID(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	fake := newFakeNetbirdToken(t, now)
	a := fake.seedToken("a-1", 5)
	b := fake.seedToken("b-1", 10)
	svc := newTokenTestService(t, now, nil)
	enableNetbird(t, svc, fake.srv.URL, true)
	svc.waitNetbirdResolve() // multi-token -> resolve-on-save clears id/expiry to ""

	values, err := svc.settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if values[netbirdTokenIDKey] != "" {
		t.Fatalf("netbird_token_id = %q, want empty (ambiguous multi-token resolve-on-save)", values[netbirdTokenIDKey])
	}

	res, err := svc.RotateNetbirdToken(context.Background(), systemToken())
	if err != nil {
		t.Fatalf("RotateNetbirdToken = %v, want nil", err)
	}
	if !res.OldUnknown {
		t.Fatalf("OldUnknown = false, want true")
	}
	if res.OldDeleted {
		t.Fatalf("OldDeleted = true, want false (never guess among multiple tokens)")
	}
	if fake.wasDeleted(a.ID) || fake.wasDeleted(b.ID) {
		t.Fatalf("an old token was deleted despite ambiguity: a-deleted=%v b-deleted=%v", fake.wasDeleted(a.ID), fake.wasDeleted(b.ID))
	}
	// Both original tokens plus the new one now exist.
	if fake.tokenCount() != 3 {
		t.Fatalf("token count = %d, want 3 (a, b, and the new one)", fake.tokenCount())
	}
}

// --- module disabled --------------------------------------------------------

func TestRotateNetbirdTokenModuleDisabled(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	fake := newFakeNetbirdToken(t, now)
	svc := newTokenTestService(t, now, nil)
	enableNetbird(t, svc, fake.srv.URL, false)

	if _, err := svc.RotateNetbirdToken(context.Background(), systemToken()); !errors.Is(err, ErrNetbirdModuleDisabled) {
		t.Fatalf("RotateNetbirdToken(module off) = %v, want ErrNetbirdModuleDisabled", err)
	}
	if fake.createCalls() != 0 {
		t.Fatalf("createCalls = %d, want 0 (module off => no NetBird call)", fake.createCalls())
	}
}

// --- NetbirdTokenStatus ------------------------------------------------------

// TestNetbirdTokenStatusKnown: a single stored token resolves to Known:true with
// the correct fields and a days_remaining rounded up from the fixed clock.
func TestNetbirdTokenStatusKnown(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	fake := newFakeNetbirdToken(t, now)
	tok := fake.seedToken("tok-x", 10)
	svc := newTokenTestService(t, now, nil)
	enableNetbird(t, svc, fake.srv.URL, true)
	svc.waitNetbirdResolve()

	status, err := svc.NetbirdTokenStatus(context.Background())
	if err != nil {
		t.Fatalf("NetbirdTokenStatus = %v, want nil", err)
	}
	if !status.Known {
		t.Fatalf("Known = false, want true")
	}
	if status.Name != tok.Name {
		t.Fatalf("Name = %q, want %q", status.Name, tok.Name)
	}
	if status.ExpirationDate != tok.ExpirationDate {
		t.Fatalf("ExpirationDate = %q, want %q", status.ExpirationDate, tok.ExpirationDate)
	}
	if status.DaysRemaining != 10 {
		t.Fatalf("DaysRemaining = %d, want 10", status.DaysRemaining)
	}
	raw, _ := json.Marshal(status)
	if strings.Contains(string(raw), "nbtok") || strings.Contains(string(raw), "nbp_") {
		t.Fatalf("status JSON leaks a token value: %s", raw)
	}
}

// TestNetbirdTokenStatusUnknownModuleOff: module disabled -> Known:false, no error,
// and no NetBird call at all.
func TestNetbirdTokenStatusUnknownModuleOff(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	fake := newFakeNetbirdToken(t, now)
	svc := newTokenTestService(t, now, nil)
	enableNetbird(t, svc, fake.srv.URL, false)

	status, err := svc.NetbirdTokenStatus(context.Background())
	if err != nil {
		t.Fatalf("NetbirdTokenStatus = %v, want nil", err)
	}
	if status.Known {
		t.Fatalf("Known = true, want false (module off)")
	}
	if fake.createCalls() != 0 {
		t.Fatalf("createCalls = %d, want 0", fake.createCalls())
	}
}

// TestNetbirdTokenStatusUnknownAmbiguous: no stored id and more than one token ->
// Known:false (never guesses).
func TestNetbirdTokenStatusUnknownAmbiguous(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	fake := newFakeNetbirdToken(t, now)
	fake.seedToken("a-1", 5)
	fake.seedToken("b-1", 10)
	svc := newTokenTestService(t, now, nil)
	enableNetbird(t, svc, fake.srv.URL, true)
	svc.waitNetbirdResolve()

	status, err := svc.NetbirdTokenStatus(context.Background())
	if err != nil {
		t.Fatalf("NetbirdTokenStatus = %v, want nil", err)
	}
	if status.Known {
		t.Fatalf("Known = true, want false (ambiguous, no stored id)")
	}
}

// --- resolve-on-save ---------------------------------------------------------

// TestResolveOnSaveSingleToken: enabling NetBird with exactly one existing token
// resolves the id/expiry via the best-effort background hook.
func TestResolveOnSaveSingleToken(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	fake := newFakeNetbirdToken(t, now)
	tok := fake.seedToken("solo-1", 20)
	svc := newTokenTestService(t, now, nil)
	enableNetbird(t, svc, fake.srv.URL, true)
	svc.waitNetbirdResolve()

	values, err := svc.settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if values[netbirdTokenIDKey] != tok.ID {
		t.Fatalf("netbird_token_id = %q, want %q", values[netbirdTokenIDKey], tok.ID)
	}
	if values[netbirdTokenExpiresAtKey] != tok.ExpirationDate {
		t.Fatalf("netbird_token_expires_at = %q, want %q", values[netbirdTokenExpiresAtKey], tok.ExpirationDate)
	}
}

// TestResolveOnSaveMultiToken: enabling with more than one existing token clears
// id/expiry (ambiguous -> not gateway-managed until the first rotation).
func TestResolveOnSaveMultiToken(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	fake := newFakeNetbirdToken(t, now)
	fake.seedToken("m-1", 5)
	fake.seedToken("m-2", 15)
	svc := newTokenTestService(t, now, nil)
	enableNetbird(t, svc, fake.srv.URL, true)
	svc.waitNetbirdResolve()

	values, err := svc.settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if values[netbirdTokenIDKey] != "" {
		t.Fatalf("netbird_token_id = %q, want empty (ambiguous)", values[netbirdTokenIDKey])
	}
	if values[netbirdTokenExpiresAtKey] != "" {
		t.Fatalf("netbird_token_expires_at = %q, want empty (ambiguous)", values[netbirdTokenExpiresAtKey])
	}
}

// TestResolveOnSaveClearToken: clearing the stored token (NetbirdToken:"") clears
// id/expiry SYNCHRONOUSLY, with no NetBird call.
func TestResolveOnSaveClearToken(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	fake := newFakeNetbirdToken(t, now)
	tok := fake.seedToken("solo-1", 20)
	svc := newTokenTestService(t, now, nil)
	enableNetbird(t, svc, fake.srv.URL, true)
	svc.waitNetbirdResolve()

	// Sanity: the single-token resolve did run first.
	values, err := svc.settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if values[netbirdTokenIDKey] != tok.ID {
		t.Fatalf("precondition failed: netbird_token_id = %q, want %q", values[netbirdTokenIDKey], tok.ID)
	}

	before := fake.createCalls()
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdToken: strPtr(""),
	}); err != nil {
		t.Fatalf("UpdateSystemSettings(clear token) = %v, want nil", err)
	}
	// Synchronous: no need to wait for a goroutine.
	values, err = svc.settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if values[netbirdTokenIDKey] != "" {
		t.Fatalf("netbird_token_id = %q, want empty after clearing the token", values[netbirdTokenIDKey])
	}
	if values[netbirdTokenExpiresAtKey] != "" {
		t.Fatalf("netbird_token_expires_at = %q, want empty after clearing the token", values[netbirdTokenExpiresAtKey])
	}
	if fake.createCalls() != before {
		t.Fatalf("createCalls changed on clear (%d -> %d), want no NetBird call", before, fake.createCalls())
	}
}

// --- MaybeRotateNetbirdToken --------------------------------------------------

// TestMaybeRotateNetbirdTokenThresholdZeroDisabled: an explicit threshold of 0
// disables auto-rotation entirely, even for an ALREADY-EXPIRED stored token.
// daysRemaining floors at 0, so "daysRemaining > threshold" (0 > 0 = false) does
// NOT by itself block this case -- the dedicated threshold<=0 guard is what does.
// Mutation-proof: dropping that guard turns this into a rotation.
func TestMaybeRotateNetbirdTokenThresholdZeroDisabled(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	fake := newFakeNetbirdToken(t, now)
	fake.seedToken("old-1", -1) // already expired
	svc := newTokenTestService(t, now, nil)
	enableNetbird(t, svc, fake.srv.URL, true)
	svc.waitNetbirdResolve() // single token -> id/expiry resolved

	zero := 0
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdTokenRotateBeforeDays: &zero,
	}); err != nil {
		t.Fatalf("UpdateSystemSettings(threshold=0): %v", err)
	}

	svc.MaybeRotateNetbirdToken(context.Background())
	if fake.createCalls() != 0 {
		t.Fatalf("createCalls = %d, want 0 (threshold<=0 must disable auto-rotation)", fake.createCalls())
	}
}

// TestMaybeRotateNetbirdTokenMissingIDOrExpiry: a token not yet resolved to an id
// AND expiry (not "gateway-managed") is never auto-rotated, even when otherwise
// near expiry.
func TestMaybeRotateNetbirdTokenMissingIDOrExpiry(t *testing.T) {
	for _, tc := range []struct {
		name        string
		clearID     bool
		clearExpiry bool
	}{
		{"missing-id", true, false},
		{"missing-expiry", false, true},
		{"missing-both", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
			fake := newFakeNetbirdToken(t, now)
			fake.seedToken("old-1", 5) // near expiry, would otherwise trigger
			svc := newTokenTestService(t, now, nil)
			enableNetbird(t, svc, fake.srv.URL, true)
			svc.waitNetbirdResolve()

			if tc.clearID {
				mustSetSetting(t, svc, netbirdTokenIDKey, "")
			}
			if tc.clearExpiry {
				mustSetSetting(t, svc, netbirdTokenExpiresAtKey, "")
			}

			svc.MaybeRotateNetbirdToken(context.Background())
			if fake.createCalls() != 0 {
				t.Fatalf("createCalls = %d, want 0 (missing id/expiry must skip auto-rotation)", fake.createCalls())
			}
		})
	}
}

// TestMaybeRotateNetbirdTokenNotNearExpiry: daysRemaining(expiry) > threshold ->
// no rotation.
func TestMaybeRotateNetbirdTokenNotNearExpiry(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	fake := newFakeNetbirdToken(t, now)
	fake.seedToken("old-1", 30) // 30 days left
	svc := newTokenTestService(t, now, nil)
	enableNetbird(t, svc, fake.srv.URL, true)
	svc.waitNetbirdResolve()

	fourteen := 14
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdTokenRotateBeforeDays: &fourteen,
	}); err != nil {
		t.Fatalf("UpdateSystemSettings(threshold=14): %v", err)
	}

	svc.MaybeRotateNetbirdToken(context.Background())
	if fake.createCalls() != 0 {
		t.Fatalf("createCalls = %d, want 0 (30 days remaining > 14-day threshold)", fake.createCalls())
	}
}

// TestMaybeRotateNetbirdTokenWithinThresholdRotates: within the threshold and
// gateway-managed (known id+expiry) -> DOES auto-rotate, advancing the stored
// token/id/expiry and best-effort deleting the old token.
func TestMaybeRotateNetbirdTokenWithinThresholdRotates(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	fake := newFakeNetbirdToken(t, now)
	old := fake.seedToken("old-1", 5) // 5 days left
	svc := newTokenTestService(t, now, nil)
	enableNetbird(t, svc, fake.srv.URL, true)
	svc.waitNetbirdResolve()

	fourteen := 14
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdTokenRotateBeforeDays: &fourteen,
	}); err != nil {
		t.Fatalf("UpdateSystemSettings(threshold=14): %v", err)
	}

	svc.MaybeRotateNetbirdToken(context.Background())
	if fake.createCalls() != 1 {
		t.Fatalf("createCalls = %d, want 1 (5 days remaining <= 14-day threshold must auto-rotate)", fake.createCalls())
	}
	if !fake.wasDeleted(old.ID) {
		t.Fatalf("old token %s was not deleted after auto-rotation", old.ID)
	}
	values, err := svc.settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if values[netbirdTokenIDKey] == old.ID || values[netbirdTokenIDKey] == "" {
		t.Fatalf("netbird_token_id = %q, want it advanced to a new token id", values[netbirdTokenIDKey])
	}
	wantExp := now.AddDate(0, 0, netbirdTokenValidityDays).UTC().Format(time.RFC3339)
	if values[netbirdTokenExpiresAtKey] != wantExp {
		t.Fatalf("netbird_token_expires_at = %q, want %q (advanced to the new token's expiry)", values[netbirdTokenExpiresAtKey], wantExp)
	}
}

// TestMaybeRotateNetbirdTokenAtExactThresholdRotates: a token with EXACTLY the
// threshold days remaining must rotate — the gate is `daysRemaining > threshold`
// -> skip, so at daysRemaining == threshold it is NOT skipped (the "within
// threshold" boundary is inclusive). Guards against a future `>`->`>=` off-by-one.
func TestMaybeRotateNetbirdTokenAtExactThresholdRotates(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	fake := newFakeNetbirdToken(t, now)
	fake.seedToken("old-1", 14) // exactly 14 days left
	svc := newTokenTestService(t, now, nil)
	enableNetbird(t, svc, fake.srv.URL, true)
	svc.waitNetbirdResolve()

	fourteen := 14
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdTokenRotateBeforeDays: &fourteen,
	}); err != nil {
		t.Fatalf("UpdateSystemSettings(threshold=14): %v", err)
	}

	svc.MaybeRotateNetbirdToken(context.Background())
	if fake.createCalls() != 1 {
		t.Fatalf("createCalls = %d, want 1 (exactly-14 days left must rotate at a 14-day threshold)", fake.createCalls())
	}
}

// TestMaybeRotateNetbirdTokenCooldownSuppressesImmediateRetry: after one
// auto-rotation attempt, a second call within the cooldown window is suppressed
// even when the (now-advanced) stored expiry is artificially forced back into a
// near-expiry state -- i.e. the ONLY thing blocking the second call is the
// cooldown gate, not the near-expiry check. Mutation-proof: dropping the cooldown
// gate turns the second call into a second rotation (createCalls -> 2).
func TestMaybeRotateNetbirdTokenCooldownSuppressesImmediateRetry(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	fake := newFakeNetbirdToken(t, now)
	fake.seedToken("old-1", 5)
	svc := newTokenTestService(t, now, nil)
	enableNetbird(t, svc, fake.srv.URL, true)
	svc.waitNetbirdResolve()

	fourteen := 14
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdTokenRotateBeforeDays: &fourteen,
	}); err != nil {
		t.Fatalf("UpdateSystemSettings(threshold=14): %v", err)
	}

	svc.MaybeRotateNetbirdToken(context.Background())
	if fake.createCalls() != 1 {
		t.Fatalf("createCalls after first attempt = %d, want 1", fake.createCalls())
	}

	// Force the (now-advanced, 365-day) stored expiry back into a near-expiry
	// state, so the near-expiry check alone would allow a second rotation --
	// isolating the cooldown as the only remaining gate.
	mustSetSetting(t, svc, netbirdTokenExpiresAtKey, now.AddDate(0, 0, 3).UTC().Format(time.RFC3339))

	svc.MaybeRotateNetbirdToken(context.Background())
	if fake.createCalls() != 1 {
		t.Fatalf("createCalls after second (cooldown-window) attempt = %d, want still 1 (cooldown must suppress it)", fake.createCalls())
	}
}

// mustSetSetting is a small test helper writing one KV setting directly through
// the Service's settings store (bypassing the request-validated UpdateSystemSettings
// path), for seeding the runtime-managed netbird_token_id/_expires_at fields the
// public API does not expose for direct write.
func mustSetSetting(t *testing.T, svc *Service, key, value string) {
	t.Helper()
	if err := svc.settings.SetSystemSetting(context.Background(), key, value, svc.clock()); err != nil {
		t.Fatalf("SetSystemSetting(%s): %v", key, err)
	}
}
