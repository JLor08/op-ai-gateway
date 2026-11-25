// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const warnUnreadable = "WARN hostpower: token store unreadable, keeping previous tokens"

func tokenPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "cfg", "tokens.json")
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newTestStore(path string) (*TokenStore, *fakeClock, *recordingHandler) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	h := &recordingHandler{}
	s := NewTokenStore(path)
	s.now = clk.now
	s.log = slog.New(h)
	return s, clk, h
}

func TestCreateTokenStoresOnlyHash(t *testing.T) {
	path := tokenPath(t)
	tok, err := CreateToken(path, "100")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "ophp_") || len(strings.TrimPrefix(tok, "ophp_")) != 43 {
		t.Fatalf("bad token shape %q", tok)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ophp_") || strings.Contains(string(raw), tok) {
		t.Fatal("file contains plaintext token")
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(path))
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", di.Mode().Perm())
	}
}

func TestCreateTokenReplacesExisting(t *testing.T) {
	path := tokenPath(t)
	old, _ := CreateToken(path, "100")
	fresh, err := CreateToken(path, "100")
	if err != nil {
		t.Fatal(err)
	}
	s, _, _ := newTestStore(path)
	if _, ok := s.Lookup(old); ok {
		t.Fatal("old token still valid")
	}
	if v, ok := s.Lookup(fresh); !ok || v != "100" {
		t.Fatalf("new token: %q %v", v, ok)
	}
}

func TestRevokeAndList(t *testing.T) {
	path := tokenPath(t)
	for _, id := range []string{"200", "100", "1000"} {
		if _, err := CreateToken(path, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := RevokeToken(path, "200"); err != nil {
		t.Fatal(err)
	}
	if err := RevokeToken(path, "999"); err != nil {
		t.Fatalf("absent revoke: %v", err)
	}
	got, err := ListTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].VMID != "100" || got[1].VMID != "1000" {
		t.Fatalf("list = %+v", got)
	}
	if got[0].Created.IsZero() {
		t.Fatal("created not parsed")
	}
}

func TestListTokensMissingFile(t *testing.T) {
	got, err := ListTokens(tokenPath(t))
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestCreateTokenRejectsNonNumericVMID(t *testing.T) {
	for _, id := range []string{"", "abc", "10a", "../1", "-1", "0100", "12345678901"} {
		path := tokenPath(t)
		if _, err := CreateToken(path, id); err == nil {
			t.Errorf("vmid %q accepted", id)
		}
		if _, err := os.Stat(path); err == nil {
			t.Errorf("vmid %q created a file", id)
		}
	}
}

func TestCreateTokenRefusesUnparsableStore(t *testing.T) {
	path := tokenPath(t)
	if _, err := CreateToken(path, "100"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateToken(path, "101"); err == nil {
		t.Fatal("expected error rather than clobbering a corrupt store")
	}
}

func TestTokenStoreLookup(t *testing.T) {
	path := tokenPath(t)
	tok, _ := CreateToken(path, "100")
	other, _ := CreateToken(path, "101")
	s, _, _ := newTestStore(path)
	if v, ok := s.Lookup(tok); !ok || v != "100" {
		t.Fatalf("got %q %v", v, ok)
	}
	if v, ok := s.Lookup(other); !ok || v != "101" {
		t.Fatalf("got %q %v", v, ok)
	}
	for _, bad := range []string{"", "ophp_", "ophp_wrong", strings.TrimPrefix(tok, "ophp_"), tok + "x"} {
		if _, ok := s.Lookup(bad); ok {
			t.Errorf("token %q accepted", bad)
		}
	}
}

func TestTokenStoreLookupDuplicateDigestNoMatch(t *testing.T) {
	path := tokenPath(t)
	tok, _ := CreateToken(path, "100")
	raw, _ := os.ReadFile(path)
	// Hand-copy VM 100's entry to VM 101 so two entries share one digest.
	dup := strings.Replace(string(raw), `"100"`, `"101": `+extractEntry(t, string(raw))+`, "100"`, 1)
	if err := os.WriteFile(path, []byte(dup), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _, _ := newTestStore(path)
	if _, ok := s.Lookup(tok); ok {
		t.Fatal("ambiguous digest matched")
	}
}

func extractEntry(t *testing.T, raw string) string {
	t.Helper()
	start := strings.Index(raw[1:], "{") + 1
	end := strings.Index(raw[start:], "}") + start + 1
	return raw[start:end]
}

func TestTokenStorePicksUpLateFile(t *testing.T) {
	path := tokenPath(t)
	s, clk, _ := newTestStore(path)
	if _, ok := s.Lookup("ophp_x"); ok {
		t.Fatal("matched on missing file")
	}
	tok, _ := CreateToken(path, "100")
	clk.t = clk.t.Add(time.Second)
	if v, ok := s.Lookup(tok); !ok || v != "100" {
		t.Fatalf("late file: %q %v", v, ok)
	}
}

func TestTokenStoreReloadRateLimited(t *testing.T) {
	path := tokenPath(t)
	first, _ := CreateToken(path, "100")
	s, clk, _ := newTestStore(path)
	if _, ok := s.Lookup(first); !ok {
		t.Fatal("first token")
	}
	second, _ := CreateToken(path, "101")
	clk.t = clk.t.Add(500 * time.Millisecond)
	if _, ok := s.Lookup(second); ok {
		t.Fatal("change seen within the same second")
	}
	clk.t = clk.t.Add(500 * time.Millisecond)
	if v, ok := s.Lookup(second); !ok || v != "101" {
		t.Fatalf("after 1s: %q %v", v, ok)
	}
}

func TestTokenStoreRevokeTakesEffect(t *testing.T) {
	path := tokenPath(t)
	tok, _ := CreateToken(path, "100")
	s, clk, _ := newTestStore(path)
	if _, ok := s.Lookup(tok); !ok {
		t.Fatal("token")
	}
	if err := RevokeToken(path, "100"); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Second)
	if _, ok := s.Lookup(tok); ok {
		t.Fatal("revoked token still valid")
	}
}

func TestTokenStoreCorruptKeepsLastGood(t *testing.T) {
	path := tokenPath(t)
	tok, _ := CreateToken(path, "100")
	s, clk, h := newTestStore(path)
	if _, ok := s.Lookup(tok); !ok {
		t.Fatal("token")
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		clk.t = clk.t.Add(time.Second)
		if _, ok := s.Lookup(tok); !ok {
			t.Fatal("last good set lost")
		}
	}
	if n := h.count(warnUnreadable); n != 1 {
		t.Fatalf("warn records = %d, want 1", n)
	}
	// Recovers once the file parses again.
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Second)
	if _, ok := s.Lookup(tok); ok {
		t.Fatal("token valid after store emptied")
	}
}

func TestTokenStoreSkipsMalformedDigest(t *testing.T) {
	path := tokenPath(t)
	tok, _ := CreateToken(path, "100")
	raw, _ := os.ReadFile(path)
	patched := strings.Replace(string(raw), "{", `{"7":{"sha256":"zz","created":"x"},`, 1)
	if err := os.WriteFile(path, []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _, h := newTestStore(path)
	if v, ok := s.Lookup(tok); !ok || v != "100" {
		t.Fatalf("got %q %v", v, ok)
	}
	if h.count(warnUnreadable) != 0 {
		t.Fatal("malformed digest should not warn as unreadable store")
	}
}

func TestTokenMainCreatePrintsTokenOnly(t *testing.T) {
	path := tokenPath(t)
	var out, errb bytes.Buffer
	if code := TokenMain([]string{"-tokens", path, "create", "100"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	tok := out.String()
	if !strings.HasPrefix(tok, "ophp_") || !strings.HasSuffix(tok, "\n") || strings.Count(tok, "\n") != 1 {
		t.Fatalf("stdout %q", tok)
	}
	s, _, _ := newTestStore(path)
	if v, ok := s.Lookup(strings.TrimSpace(tok)); !ok || v != "100" {
		t.Fatalf("stored token lookup %q %v", v, ok)
	}
	out.Reset()
	if code := TokenMain([]string{"-tokens", path, "list"}, &out, &errb); code != 0 {
		t.Fatalf("list exit %d", code)
	}
	if !strings.HasPrefix(out.String(), "100\t") || strings.Contains(out.String(), "sha256") {
		t.Fatalf("list output %q", out.String())
	}
	if code := TokenMain([]string{"-tokens", path, "revoke", "100"}, &out, &errb); code != 0 {
		t.Fatalf("revoke exit %d", code)
	}
	l, _ := ListTokens(path)
	if len(l) != 0 {
		t.Fatal("not revoked")
	}
}

func TestTokenMainUsageErrors(t *testing.T) {
	path := tokenPath(t)
	cases := [][]string{
		{},
		{"-tokens", path},
		{"bogus"},
		{"-tokens", path, "create"},
		{"-tokens", path, "revoke"},
		{"-tokens", path, "create", "abc"},
		{"-tokens", path, "create", "1", "2"},
		{"-tokens", path, "list", "1"},
		{"-nosuchflag"},
	}
	for _, args := range cases {
		var out, errb bytes.Buffer
		if code := TokenMain(args, &out, &errb); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if out.Len() != 0 {
			t.Errorf("%v: stdout not empty", args)
		}
		if errb.Len() == 0 {
			t.Errorf("%v: no stderr message", args)
		}
	}
}

func TestTokenMainIOErrorExit1(t *testing.T) {
	path := tokenPath(t)
	if _, err := CreateToken(path, "100"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := TokenMain([]string{"-tokens", path, "list"}, &out, &errb); code != 1 {
		t.Fatalf("exit %d", code)
	}
}
