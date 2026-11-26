// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
)

// sealedTokenPrefix marks an api_token in the runtime-config disk cache that
// is AES-256-GCM sealed with a subkey of the agent key. A token without the
// prefix is an old (or key-less) plaintext cache entry.
const sealedTokenPrefix = "sealed:v1:"

// sealCacheTokens returns a deep copy of cfg whose non-empty Specs[i].APIToken
// are sealed with key (AES-256-GCM, random 12-byte nonce, AAD = spec ID). The
// input is never mutated: the in-memory config keeps the plaintext token.
func sealCacheTokens(cfg Config, key []byte) (Config, error) {
	out := cfg
	out.Specs = make([]Spec, len(cfg.Specs))
	copy(out.Specs, cfg.Specs)
	if len(key) == 0 {
		return out, nil
	}
	gcm, err := newCacheGCM(key)
	if err != nil {
		return Config{}, err
	}
	for i := range out.Specs {
		tok := out.Specs[i].APIToken
		if tok == "" {
			continue
		}
		nonce := make([]byte, gcm.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			return Config{}, err
		}
		sealed := gcm.Seal(nonce, nonce, []byte(tok), []byte(out.Specs[i].ID))
		out.Specs[i].APIToken = sealedTokenPrefix + base64.RawStdEncoding.EncodeToString(sealed)
	}
	return out, nil
}

// openCacheTokens reverses sealCacheTokens on a config read from the cache.
// A token without the prefix is kept (an old plaintext cache). A prefixed
// token that cannot be opened, or one met with no key, becomes "" so the spec
// stays and the gateway's next document refills the token. It logs per spec,
// never the value.
func openCacheTokens(cfg Config, key []byte) Config {
	var gcm cipher.AEAD
	if len(key) > 0 {
		gcm, _ = newCacheGCM(key)
	}
	for i := range cfg.Specs {
		tok := cfg.Specs[i].APIToken
		if !strings.HasPrefix(tok, sealedTokenPrefix) {
			continue
		}
		plain, ok := openCacheToken(gcm, tok, cfg.Specs[i].ID)
		if !ok {
			slog.Warn("runtime: cached api_token could not be opened; waiting for the gateway", "spec_id", cfg.Specs[i].ID)
		}
		cfg.Specs[i].APIToken = plain
	}
	return cfg
}

func openCacheToken(gcm cipher.AEAD, tok, specID string) (string, bool) {
	if gcm == nil {
		return "", false
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(tok, sealedTokenPrefix))
	if err != nil || len(raw) < gcm.NonceSize() {
		return "", false
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], []byte(specID))
	if err != nil {
		return "", false
	}
	return string(plain), true
}

func newCacheGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("runtime: cache key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
