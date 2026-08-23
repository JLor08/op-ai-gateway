// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package capture

import (
	"encoding/base64"
	"errors"
	"strings"
)

// plainPrefix marks a volatile, unsealed secret value (see SealSecret/OpenSecret).
const plainPrefix = "plain:"

// ErrKeyRequired is returned when a sealed ("enc:") value is handled without a cipher, or when a
// plaintext secret would have to be persisted on a disk store that has no encryption key.
var ErrKeyRequired = errors.New("capture: encryption key required")

// SealSecret envelopes plain for storage: with a cipher → "enc:"+base64(seal); on a volatile
// (in-memory) store with no cipher → "plain:"+raw (never hits disk); on a disk store without a
// key → ErrKeyRequired (never persist plaintext). Empty plain seals to "".
func SealSecret(cipher *Cipher, volatile bool, plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	if cipher != nil {
		return "enc:" + base64.StdEncoding.EncodeToString(cipher.Seal([]byte(plain))), nil
	}
	if volatile {
		return plainPrefix + plain, nil
	}
	return "", ErrKeyRequired
}

// OpenSecret reverses SealSecret. "" → ""; "plain:" → raw; "enc:" → decrypt (ErrKeyRequired if no
// cipher); any other shape → ErrKeyRequired (never leak a corrupt value).
func OpenSecret(cipher *Cipher, stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if strings.HasPrefix(stored, plainPrefix) {
		return strings.TrimPrefix(stored, plainPrefix), nil
	}
	if strings.HasPrefix(stored, "enc:") {
		if cipher == nil {
			return "", ErrKeyRequired
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, "enc:"))
		if err != nil {
			return "", err
		}
		plain, err := cipher.Open(raw)
		if err != nil {
			return "", err
		}
		return string(plain), nil
	}
	return "", ErrKeyRequired
}
