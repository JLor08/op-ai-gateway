// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package certissue wraps golang.org/x/crypto/acme with the single operation
// the gateway needs: obtain a publicly trusted certificate for ONE FQDN over
// HTTP-01. It knows nothing about the DB, the portal, or NetBird.
package certissue

import "sync"

// ChallengeStore holds in-flight HTTP-01 key authorizations keyed by token. The
// gateway's public /.well-known/acme-challenge/{token} handler reads from it;
// Obtain writes and removes entries around one order.
type ChallengeStore interface {
	Put(token, keyAuth string)
	Get(token string) (string, bool)
	Delete(token string)
}

// MemoryChallengeStore is the process-local ChallengeStore. Tokens live only for
// the duration of an order, so there is nothing to persist and nothing to prune.
type MemoryChallengeStore struct {
	mu     sync.RWMutex
	tokens map[string]string
}

// NewMemoryChallengeStore returns an empty MemoryChallengeStore.
func NewMemoryChallengeStore() *MemoryChallengeStore {
	return &MemoryChallengeStore{tokens: map[string]string{}}
}

// Put records the key authorization Obtain must serve for token, so the
// gateway's public challenge handler can answer it while the order is open.
func (s *MemoryChallengeStore) Put(token, keyAuth string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = keyAuth
}

// Get returns the key authorization for token, if any is currently in flight.
func (s *MemoryChallengeStore) Get(token string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.tokens[token]
	return v, ok
}

// Delete removes token once its order has finished (success or failure), so
// the public handler stops answering for a challenge that is no longer live.
func (s *MemoryChallengeStore) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, token)
}
