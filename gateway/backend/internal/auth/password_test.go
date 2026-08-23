// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct horse")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash == "" || hash == "correct horse" {
		t.Fatalf("hash must be a non-plaintext value, got %q", hash)
	}
	if !VerifyPassword(hash, "correct horse") {
		t.Fatal("verify should succeed for the correct password")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Fatal("verify should fail for a wrong password")
	}
	if VerifyPassword("", "correct horse") {
		t.Fatal("verify should fail for an empty hash")
	}
}

func TestValidatePasswordPolicy(t *testing.T) {
	if err := ValidatePasswordPolicy("0123456789"); err != nil {
		t.Fatalf("10 chars should pass: %v", err)
	}
	if err := ValidatePasswordPolicy("short"); !errors.Is(err, ErrPasswordTooWeak) {
		t.Fatalf("short password should fail with ErrPasswordTooWeak, got %v", err)
	}
	long := make([]byte, 201)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidatePasswordPolicy(string(long)); !errors.Is(err, ErrPasswordTooWeak) {
		t.Fatalf("over-long password should fail, got %v", err)
	}
}

func TestHashPasswordSupportsLongPasswords(t *testing.T) {
	long := strings.Repeat("a", 200)
	hash, err := HashPassword(long)
	if err != nil {
		t.Fatalf("hashing a 200-char password should succeed: %v", err)
	}
	if !VerifyPassword(hash, long) {
		t.Fatal("verify should succeed for the long password")
	}
	if VerifyPassword(hash, strings.Repeat("a", 199)) {
		t.Fatal("verify should fail for a different long password")
	}
}
