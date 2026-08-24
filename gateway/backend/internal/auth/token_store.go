// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// ModelOverrideRule is auth's copy of store.ModelOverrideRule (the target
// model plus the two listing switches, Offer/HideTarget). It is duplicated
// here — not imported — because op-ai-gateway/internal/store already imports
// this package (for auth.Token, used by SQLStore.LookupBearer); importing
// store from here would be an import cycle. store.AuthModelOverrideRules
// converts a decoded map[string]store.ModelOverrideRule into this type at
// every call site that builds an auth.Token from a store.TokenRecord.
type ModelOverrideRule struct {
	To         string
	Offer      bool
	HideTarget bool
}

type Token struct {
	ID        string
	UserID    string
	Name      string
	Active    bool
	Scopes    []string
	ExpiresAt *time.Time
	// ModelOverride is the CATCH-ALL model override: applied to a requested model
	// that has no entry in ModelOverrideRules. Empty = no catch-all.
	ModelOverride string
	// ModelOverrideRules maps a REQUESTED model name -> the rule for it (target
	// plus the two listing switches). Takes precedence over ModelOverride, the
	// catch-all. Empty/nil = no per-model overrides.
	ModelOverrideRules map[string]ModelOverrideRule
	LogCommunication   bool
	Secret             bool
	// ServiceID / ServiceName / Kind identify a SERVICE token (Kind=="service"):
	// it belongs to a routing.Service, not a user (UserID is then empty).
	// ServiceName is resolved at lookup time for display only (never persisted
	// on the token itself, so a service rename is reflected immediately).
	// Kind==""/"user" is a normal user token (ServiceID/ServiceName empty).
	// AllowedModels is the service's model allowlist: empty = every model is
	// allowed (the default); always empty for a user token.
	ServiceID     string
	ServiceName   string
	Kind          string
	AllowedModels []string
	// ProjectID is the optional project a USER token is attributed to for
	// usage attribution ("" = none; a service token never carries one).
	// ProjectName is resolved for display at lookup/construction time (like
	// ServiceName) and is never persisted on the token itself, so a project
	// rename is reflected immediately.
	ProjectID   string
	ProjectName string
	// ServerOverride is the id of an AI-server this token forces every request
	// onto, bypassing provisioning/affinity/maintenance-status ("" = no
	// override). ServerOverrideForceUnreachable, when true, allows the override
	// to route even to an unhealthy/unreachable server.
	ServerOverride                 string
	ServerOverrideForceUnreachable bool
	// LastUsedModel / UnknownModelRedirect / UnknownModelRedirectBlocked /
	// UnknownModelFallback mirror the token record — see store.TokenRecord.
	LastUsedModel               string
	UnknownModelRedirect        bool
	UnknownModelRedirectBlocked bool
	UnknownModelFallback        string
}

// IsService reports whether t is a service token (Kind=="service") — the
// gateway's "LLM-invoke only" principals (see routing.Service, Phase 1).
func (t Token) IsService() bool {
	return t.Kind == "service"
}

// cloneOverrideMap returns a copy of m (nil stays nil), so a stored Token's
// map is never shared by reference with a caller who might mutate it. Each
// ModelOverrideRule value is a plain struct of scalar fields (no nested slice
// or map), so copying the map entries is already a full deep copy — there is
// no further nested structure a caller could reach through to mutate.
func cloneOverrideMap(m map[string]ModelOverrideRule) map[string]ModelOverrideRule {
	if m == nil {
		return nil
	}
	out := make(map[string]ModelOverrideRule, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// cloneStrings returns a shallow copy of s (nil stays nil), mirroring
// cloneOverrideMap — used for AllowedModels so a stored Token's slice is never
// shared by reference with a caller who might mutate it.
func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s...)
}

func (t Token) HasScope(scope string) bool {
	for _, candidate := range t.Scopes {
		if candidate == scope {
			return true
		}
	}
	return false
}

type BearerStore interface {
	LookupBearer(header string) (Token, bool)
}

type TokenStore struct {
	mu     sync.RWMutex
	tokens map[string]Token
}

func NewTokenStore() *TokenStore {
	return &TokenStore{tokens: map[string]Token{}}
}

func (s *TokenStore) AddPlainToken(token Token, secret string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token.Scopes = append([]string(nil), token.Scopes...)
	token.ModelOverrideRules = cloneOverrideMap(token.ModelOverrideRules)
	token.AllowedModels = cloneStrings(token.AllowedModels)
	s.tokens[HashSecret(secret)] = token
}

// UpdateToken replaces the stored token that matches token.ID in place, keeping
// its secret-hash key. It is a no-op if no token with that ID is present.
func (s *TokenStore) UpdateToken(token Token) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for hash, existing := range s.tokens {
		if existing.ID == token.ID {
			token.Scopes = append([]string(nil), token.Scopes...)
			token.ModelOverrideRules = cloneOverrideMap(token.ModelOverrideRules)
			token.AllowedModels = cloneStrings(token.AllowedModels)
			s.tokens[hash] = token
			return
		}
	}
}

// RemoveToken deletes the stored token that matches id. It is a no-op if no
// token with that ID is present.
func (s *TokenStore) RemoveToken(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for hash, existing := range s.tokens {
		if existing.ID == id {
			delete(s.tokens, hash)
			return
		}
	}
}

// RekeyToken re-files the stored token that matches id under newSecretHash,
// dropping its previous secret-hash key so the old secret stops resolving. It
// is a no-op if no token with that ID is present. Used by the memory directory
// to mirror a store-side secret rotation (which supplies only the hash).
func (s *TokenStore) RekeyToken(id, newSecretHash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for hash, existing := range s.tokens {
		if existing.ID == id {
			delete(s.tokens, hash)
			s.tokens[newSecretHash] = existing
			return
		}
	}
}

func (s *TokenStore) LookupBearer(header string) (Token, bool) {
	secret, ok := ExtractBearerSecret(header)
	if !ok {
		return Token{}, false
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	token, ok := s.tokens[HashSecret(secret)]
	if !ok || !token.Active {
		return Token{}, false
	}
	if token.ExpiresAt != nil && !token.ExpiresAt.After(time.Now().UTC()) {
		return Token{}, false
	}
	token.Scopes = append([]string(nil), token.Scopes...)
	token.ModelOverrideRules = cloneOverrideMap(token.ModelOverrideRules)
	token.AllowedModels = cloneStrings(token.AllowedModels)
	return token, true
}

func ExtractBearerSecret(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	secret := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if secret == "" {
		return "", false
	}
	return secret, true
}

func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
