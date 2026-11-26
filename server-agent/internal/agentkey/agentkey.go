// Package agentkey holds the agent's local secret (agent_key) and derives
// per-purpose subkeys from it. The key never leaves the host: it is not sent to
// the gateway, logged, or placed in a URL. Each use derives its own subkey with
// HKDF so no two purposes share key material.
package agentkey

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const (
	// PurposeInterfaceID derives the key behind keyed network-interface ids.
	PurposeInterfaceID = "op-ai-gateway/interface-id/v1"
	// PurposeRuntimeCache derives the key that seals the runtime cache token.
	PurposeRuntimeCache = "op-ai-gateway/runtime-cache/v1"

	keyBytes = 32
)

// Key is the parsed 32-byte master key.
type Key struct {
	master []byte
}

// Parse decodes a 64-character hex key. An empty string means "no key" and
// returns nil, nil; anything else that is not exactly 64 hex characters is an
// error.
func Parse(hexKey string) (*Key, error) {
	if hexKey == "" {
		return nil, nil
	}
	if len(hexKey) != keyBytes*2 {
		return nil, fmt.Errorf("agent_key must be 64 hex characters (32 bytes)")
	}
	b, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("agent_key must be 64 hex characters (32 bytes)")
	}
	return &Key{master: b}, nil
}

// Subkey derives a 32-byte key bound to purpose. A nil key returns nil.
func (k *Key) Subkey(purpose string) []byte {
	if k == nil {
		return nil
	}
	out, err := hkdf.Key(sha256.New, k.master, nil, purpose, keyBytes)
	if err != nil {
		// Unreachable for a 32-byte output from SHA-256.
		return nil
	}
	return out
}
