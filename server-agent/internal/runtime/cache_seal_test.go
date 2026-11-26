// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const sealTestToken = "sk-upstream-secret-value"

func sealTestKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func sealTestConfig(etag string) Config {
	cfg := emptyConfig()
	cfg.ETag = etag
	cfg.Specs = []Spec{{ID: "spec-a", APIToken: sealTestToken}, {ID: "spec-b"}}
	return cfg
}

func readCacheFile(t *testing.T, path string) ([]byte, Config) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return raw, cfg
}

func TestPersistSealsAPITokenWithKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	s := NewGatewaySourceWithCacheKey("http://gw.invalid", "t", nil, path, sealTestKey(1))
	s.persist(sealTestConfig("e1"))

	raw, cfg := readCacheFile(t, path)
	if bytes.Contains(raw, []byte(sealTestToken)) {
		t.Fatal("cache file contains the plaintext token")
	}
	if !strings.HasPrefix(cfg.Specs[0].APIToken, "sealed:v1:") {
		t.Fatalf("api_token = %q, want sealed prefix", cfg.Specs[0].APIToken)
	}
	if cfg.Specs[1].APIToken != "" {
		t.Fatal("empty token must stay empty")
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}
}

func TestCacheReloadWithSameKeyRestoresToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	NewGatewaySourceWithCacheKey("http://gw.invalid", "t", nil, path, sealTestKey(1)).persist(sealTestConfig("e1"))
	s2 := NewGatewaySourceWithCacheKey("http://gw.invalid", "t", nil, path, sealTestKey(1))
	if got := s2.cached.Specs[0].APIToken; got != sealTestToken {
		t.Fatalf("token = %q, want restored", got)
	}
}

func TestCacheReloadWithOtherKeyDropsTokenKeepsSpec(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	NewGatewaySourceWithCacheKey("http://gw.invalid", "t", nil, path, sealTestKey(1)).persist(sealTestConfig("e1"))
	for _, key := range [][]byte{sealTestKey(2), nil} {
		s2 := NewGatewaySourceWithCacheKey("http://gw.invalid", "t", nil, path, key)
		if len(s2.cached.Specs) != 2 || s2.cached.Specs[0].ID != "spec-a" {
			t.Fatalf("specs not kept: %+v", s2.cached.Specs)
		}
		if s2.cached.Specs[0].APIToken != "" {
			t.Fatalf("token = %q, want dropped", s2.cached.Specs[0].APIToken)
		}
	}
}

func TestPlaintextCacheIsReadAndResealedOnNextPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	NewGatewaySourceWithCacheKey("http://gw.invalid", "t", nil, path, nil).persist(sealTestConfig("e1"))
	raw, _ := readCacheFile(t, path)
	if !bytes.Contains(raw, []byte(sealTestToken)) {
		t.Fatal("precondition: plaintext cache expected")
	}
	s2 := NewGatewaySourceWithCacheKey("http://gw.invalid", "t", nil, path, sealTestKey(1))
	if got := s2.cached.Specs[0].APIToken; got != sealTestToken {
		t.Fatalf("plaintext cache token = %q", got)
	}
	s2.persist(sealTestConfig("e2"))
	raw, _ = readCacheFile(t, path)
	if bytes.Contains(raw, []byte(sealTestToken)) {
		t.Fatal("cache not resealed")
	}
}

func TestNoKeyKeepsPlaintextCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	NewGatewaySourceWithCacheKey("http://gw.invalid", "t", nil, path, nil).persist(sealTestConfig("e1"))
	_, cfg := readCacheFile(t, path)
	if cfg.Specs[0].APIToken != sealTestToken {
		t.Fatalf("token = %q, want plaintext", cfg.Specs[0].APIToken)
	}
}

func TestSealDoesNotMutateInMemoryConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	s := NewGatewaySourceWithCacheKey("http://gw.invalid", "t", nil, path, sealTestKey(1))
	cfg := sealTestConfig("e1")
	s.persist(cfg)
	if s.cached.Specs[0].APIToken != sealTestToken || cfg.Specs[0].APIToken != sealTestToken {
		t.Fatal("persist mutated the in-memory config")
	}
}

// etagAwareStub answers like the gateway's runtime-config handler: 304 when
// If-None-Match names the current document, otherwise 200 with it. The
// document carries spec-a's token, so a fetch that returns it refills a token
// the disk cache could not open.
func etagAwareStub(t *testing.T, etag string, inm *[]string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*inm = append(*inm, r.Header.Get("If-None-Match"))
		mu.Unlock()
		w.Header().Set("ETag", `"`+etag+`"`)
		if got := strings.Trim(r.Header.Get("If-None-Match"), `"`); got == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(configJSONWithSpecToken(9000, etag, "spec-a", sealTestToken)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestUnopenableCachedTokenIsRefilledByTheFirstGatewayContact is #61's
// recovery claim (design §4c, ADR-119): after the agent key changed, the
// cached token cannot be opened and is blanked, and "the next gateway contact
// delivers it again". The gateway answers 304 to a conditional GET naming the
// cached document's ETag, so a source that kept that ETag would never receive
// the token: the first fetch must be unconditional, count as changed, and the
// token-less cached document must not carry the ETag either -- the manager
// short-circuits a document whose ETag equals the applied one
// (owner.applyConfig), so the refilled document would be dropped there.
func TestUnopenableCachedTokenIsRefilledByTheFirstGatewayContact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	cfg := emptyConfig()
	cfg.ETag = "e1"
	cfg.RouterListen = 9000
	cfg.MaxProcesses = 1
	cfg.Specs = []Spec{{ID: "spec-a", APIToken: sealTestToken}}
	NewGatewaySourceWithCacheKey("http://gw.invalid", "t", nil, path, sealTestKey(1)).persist(cfg)

	var inm []string
	srv := etagAwareStub(t, "e1", &inm)
	s := NewGatewaySourceWithCacheKey(srv.URL, "t", srv.Client(), path, sealTestKey(2))
	if s.cached.Specs[0].APIToken != "" {
		t.Fatal("precondition: the token sealed with another key must be dropped")
	}
	if s.cached.ETag != "" {
		t.Fatalf("token-less cached document keeps ETag %q; the manager would short-circuit the refilled document under the same ETag", s.cached.ETag)
	}

	got, changed, err := s.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inm) != 1 || inm[0] != "" {
		t.Fatalf("If-None-Match = %q, want one unconditional fetch", inm)
	}
	if !changed {
		t.Fatal("the refilled document must count as changed")
	}
	if got.Specs[0].APIToken != sealTestToken || got.ETag != "e1" {
		t.Fatalf("Load = token %q etag %q, want the gateway's document", got.Specs[0].APIToken, got.ETag)
	}

	// Once refilled, the source is back on conditional GETs.
	if _, changed, _ := s.Load(context.Background()); changed || len(inm) != 2 || inm[1] != "e1" {
		t.Fatalf("second Load: changed=%v If-None-Match=%q, want a 304 on e1", changed, inm)
	}
}

// TestOpenableCacheKeepsTheConditionalFetch pins the other side: when every
// sealed token opens, the cached ETag stays and the first fetch is
// conditional (no needless re-download, no spurious relaunch).
func TestOpenableCacheKeepsTheConditionalFetch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	NewGatewaySourceWithCacheKey("http://gw.invalid", "t", nil, path, sealTestKey(1)).persist(sealTestConfig("e1"))

	var inm []string
	srv := etagAwareStub(t, "e1", &inm)
	s := NewGatewaySourceWithCacheKey(srv.URL, "t", srv.Client(), path, sealTestKey(1))
	if _, changed, err := s.Load(context.Background()); err != nil || changed {
		t.Fatalf("Load: changed=%v err=%v, want an unchanged 304", changed, err)
	}
	if len(inm) != 1 || inm[0] != "e1" {
		t.Fatalf("If-None-Match = %q, want e1", inm)
	}
}

// writeRawCache writes a cache document whose spec-a carries tok verbatim.
func writeRawCache(t *testing.T, path, specID, tok string) {
	t.Helper()
	cfg := emptyConfig()
	cfg.ETag = "e1"
	cfg.Specs = []Spec{{ID: specID, APIToken: tok}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCorruptSealedTokenIsDroppedAndClearsETag covers a sealed:v1: payload
// that is not base64, is shorter than a nonce, or has a flipped byte: each
// is dropped like a wrong key, the spec is kept, and the ETag goes so the
// gateway refills the token.
func TestCorruptSealedTokenIsDroppedAndClearsETag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	sealed, err := sealCacheTokens(sealTestConfig("e1"), sealTestKey(1))
	if err != nil {
		t.Fatal(err)
	}
	good := sealed.Specs[0].APIToken
	payload := []byte(strings.TrimPrefix(good, sealedTokenPrefix))
	flipped := append([]byte(nil), payload...)
	flipped[len(flipped)-1] ^= 0x01
	if flipped[len(flipped)-1] == payload[len(payload)-1] {
		t.Fatal("flip did not change the payload")
	}
	cases := map[string]string{
		"not base64": sealedTokenPrefix + "!!!not-base64!!!",
		"truncated":  sealedTokenPrefix + string(payload[:8]),
		"empty":      sealedTokenPrefix,
		"tampered":   sealedTokenPrefix + string(flipped),
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			writeRawCache(t, path, "spec-a", tok)
			s := NewGatewaySourceWithCacheKey("http://gw.invalid", "t", nil, path, sealTestKey(1))
			if len(s.cached.Specs) != 1 || s.cached.Specs[0].ID != "spec-a" {
				t.Fatalf("spec not kept: %+v", s.cached.Specs)
			}
			if s.cached.Specs[0].APIToken != "" {
				t.Fatalf("token = %q, want dropped", s.cached.Specs[0].APIToken)
			}
			if s.cached.ETag != "" || s.etag != "" {
				t.Fatalf("etag = %q/%q, want cleared", s.cached.ETag, s.etag)
			}
		})
	}
}

// TestSealedTokenMovedToAnotherSpecDoesNotOpen pins the AAD: a token sealed
// for spec-a, copied into spec-b's entry, must not open there.
func TestSealedTokenMovedToAnotherSpecDoesNotOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	sealed, err := sealCacheTokens(sealTestConfig("e1"), sealTestKey(1))
	if err != nil {
		t.Fatal(err)
	}
	writeRawCache(t, path, "spec-b", sealed.Specs[0].APIToken)
	s := NewGatewaySourceWithCacheKey("http://gw.invalid", "t", nil, path, sealTestKey(1))
	if got := s.cached.Specs[0].APIToken; got != "" {
		t.Fatalf("token moved to spec-b opened as %q, want dropped", got)
	}
	if s.cached.ETag != "" {
		t.Fatalf("etag = %q, want cleared", s.cached.ETag)
	}
}

// TestUnopenableTokenWarnsWithoutTheValue: the drop is logged per spec at
// Warn, naming the spec, never carrying the token or its sealed form.
func TestUnopenableTokenWarnsWithoutTheValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	NewGatewaySourceWithCacheKey("http://gw.invalid", "t", nil, path, sealTestKey(1)).persist(sealTestConfig("e1"))
	_, onDisk := readCacheFile(t, path)
	sealedForm := strings.TrimPrefix(onDisk.Specs[0].APIToken, sealedTokenPrefix)

	h := withCountedLogs(t)
	NewGatewaySourceWithCacheKey("http://gw.invalid", "t", nil, path, sealTestKey(2))
	const msg = "runtime: cached api_token could not be opened; waiting for the gateway"
	if got := h.count(msg); got != 1 {
		t.Fatalf("warn logged %d times, want 1", got)
	}
	if !h.containsText("spec-a") {
		t.Fatal("warn does not name the spec")
	}
	if h.containsText(sealTestToken) || h.containsText(sealedForm) {
		t.Fatal("warn carries the token value")
	}
}
