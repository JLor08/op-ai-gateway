// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"golang.org/x/crypto/bcrypt"
)

// ErrPasswordTooWeak is returned when a password violates the length policy.
var ErrPasswordTooWeak = errors.New("auth.password_too_weak")

const (
	passwordMinLength = 10
	passwordMaxLength = 200
	bcryptCost        = 12
)

// bcryptDigest pre-hashes the password so bcrypt's 72-byte input limit does not
// reject longer passwords within the policy range. The SHA-256 sum is
// base64-encoded so the value is ASCII with no NUL bytes (bcrypt stops at NUL).
func bcryptDigest(plain string) []byte {
	sum := sha256.Sum256([]byte(plain))
	return []byte(base64.StdEncoding.EncodeToString(sum[:]))
}

// HashPassword returns a bcrypt hash for the given plaintext password.
func HashPassword(plain string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword(bcryptDigest(plain), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// VerifyPassword reports whether plain matches the stored bcrypt hash.
func VerifyPassword(hash string, plain string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), bcryptDigest(plain)) == nil
}

// ValidatePasswordPolicy enforces the minimum and maximum length policy.
func ValidatePasswordPolicy(plain string) error {
	if len(plain) < passwordMinLength || len(plain) > passwordMaxLength {
		return ErrPasswordTooWeak
	}
	return nil
}
