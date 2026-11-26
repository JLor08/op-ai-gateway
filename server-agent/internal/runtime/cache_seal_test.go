// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
