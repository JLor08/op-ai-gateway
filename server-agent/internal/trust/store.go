// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package trust owns the ServerAgent's dynamic gateway trust roots.
package trust

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log/slog"
	"op-ai-server-agent/internal/certfiles"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Options configures the additive trust sources. CAFile and cert_dir/ca.pem
// are read-only; only CACacheFile is managed by InstallManagedBundle.
type Options struct {
	CAFile      string
	CertDir     string
	CACacheFile string
	CAPEM       string
	TLSInsecure bool
}

type sourceState struct {
	name         string
	path         string
	hash         [sha256.Size]byte
	loaded       bool
	durable      bool
	certificates []*x509.Certificate
	fingerprints []string
}

// Store combines system roots with every configured source and gives all
// gateway clients transports backed by the same live root state.
type Store struct {
	mu         sync.RWMutex
	installMu  sync.Mutex
	generation uint64

	tlsInsecure bool
	systemPool  *x509.CertPool
	pool        *x509.CertPool

	cache    sourceState
	certDir  sourceState
	inline   sourceState
	operator sourceState
	memory   sourceState

	warnMu sync.Mutex
	warned map[string]time.Time
}

var systemCertPool = x509.SystemCertPool

const warningInterval = time.Minute

// New loads system roots plus the currently available configured sources.
// Missing optional files and invalid optional material do not remove working
// trust; they are skipped and retried on future dials.
func New(opts Options) (*Store, error) {
	certDirCA := ""
	if opts.CertDir != "" {
		certDirCA = filepath.Join(opts.CertDir, certfiles.CA)
	}
	if err := validateManagedCachePath(opts.CACacheFile, opts.CAFile, certDirCA); err != nil {
		return nil, err
	}
	system, err := systemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system certificate roots: %w", err)
	}
	if system == nil {
		system = x509.NewCertPool()
	}
	s := &Store{
		tlsInsecure: opts.TLSInsecure,
		systemPool:  system.Clone(),
		pool:        system.Clone(),
		cache:       sourceState{name: "ca_cache_file", path: opts.CACacheFile},
		certDir:     sourceState{name: "cert_dir/ca.pem"},
		inline:      sourceState{name: "ca_pem"},
		operator:    sourceState{name: "ca_file", path: opts.CAFile},
		memory:      sourceState{name: "managed_memory"},
		warned:      make(map[string]time.Time),
	}
	if certDirCA != "" {
		s.certDir.path = certDirCA
	}
	if opts.CAPEM != "" {
		certs, fingerprints, parseErr := parseBundle([]byte(opts.CAPEM))
		if parseErr != nil {
			s.warnSource(s.inline, parseErr)
		} else {
			s.inline = loadedSource(s.inline, []byte(opts.CAPEM), certs, fingerprints)
		}
	}
	s.refreshFileSources()
	// refreshFileSources rebuilds only when a file changed. An inline-only
	// configuration has no changed file, so fold its bootstrap root in here too.
	s.mu.Lock()
	s.rebuildPoolLocked()
	s.mu.Unlock()
	return s, nil
}

// InstallManagedBundle validates public CA material and makes it available to
// live connections. With a cache path it becomes durable only after the atomic
// replacement succeeds. A persistence failure still permits RAM-only use for
// this process, but is returned so callers can report that it is not durable.
func (s *Store) InstallManagedBundle(pemBytes []byte) error {
	s.installMu.Lock()
	defer s.installMu.Unlock()

	certs, fingerprints, err := parseBundle(pemBytes)
	if err != nil {
		return err
	}

	s.mu.RLock()
	cachePath := s.cache.path
	operatorPath := s.operator.path
	certDirCAPath := s.certDir.path
	s.mu.RUnlock()
	if cachePath == "" {
		s.mu.Lock()
		s.memory = loadedSource(s.memory, pemBytes, certs, fingerprints)
		s.rebuildPoolLocked()
		s.mu.Unlock()
		return nil
	}
	// Recheck immediately before the write. An operator may have introduced a
	// symlink alias after New validated the original configuration.
	if err := validateManagedCachePath(cachePath, operatorPath, certDirCAPath); err != nil {
		return err
	}
	validateRoot := func(root *os.Root, filename string) error {
		return validateManagedCacheRoot(root, filename, operatorPath, certDirCAPath)
	}
	if err := atomicWriteFile(cachePath, pemBytes, validateRoot); err != nil {
		// The atomic helper may have restored the previous cache, left the target
		// absent, or preserved only an operator-recovery backup after rollback
		// failed. Re-read the configured paths while installMu is still held so
		// durability reflects what a restart can actually load. Last-good
		// certificates remain in the live pool on missing/invalid sources.
		s.refreshFileSourcesLocked()
		s.mu.Lock()
		s.memory = loadedSource(s.memory, pemBytes, certs, fingerprints)
		s.rebuildPoolLocked()
		s.mu.Unlock()
		return fmt.Errorf("persist managed CA bundle: %w", err)
	}

	s.mu.Lock()
	s.cache = loadedSource(s.cache, pemBytes, certs, fingerprints)
	s.memory = sourceState{name: "managed_memory"}
	s.rebuildPoolLocked()
	s.mu.Unlock()
	return nil
}

// DurableFingerprints reports configured roots in the fixed fleet-report order:
// managed cache, P2 cert_dir/ca.pem, inline bootstrap, operator ca_file. Roots
// repeated by a later source appear only once.
func (s *Store) DurableFingerprints() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := make(map[string]struct{})
	var out []string
	for _, source := range []*sourceState{&s.cache, &s.certDir, &s.inline, &s.operator} {
		if !source.durable {
			continue
		}
		for _, fingerprint := range source.fingerprints {
			if _, duplicate := seen[fingerprint]; duplicate {
				continue
			}
			seen[fingerprint] = struct{}{}
			out = append(out, fingerprint)
		}
	}
	return out
}

func (s *Store) refreshFileSources() {
	s.installMu.Lock()
	defer s.installMu.Unlock()
	s.refreshFileSourcesLocked()
}

func (s *Store) refreshFileSourcesLocked() {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, source := range []*sourceState{&s.cache, &s.certDir, &s.operator} {
		if source.path == "" {
			continue
		}
		raw, err := os.ReadFile(source.path)
		if err != nil {
			source.durable = false
			if !os.IsNotExist(err) {
				s.warnSource(*source, err)
			}
			continue
		}
		hash := sha256.Sum256(raw)
		if source.loaded && hash == source.hash {
			source.durable = true
			continue
		}
		certs, fingerprints, err := parseBundle(raw)
		if err != nil {
			source.durable = false
			s.warnSource(*source, err)
			continue
		}
		*source = loadedSource(*source, raw, certs, fingerprints)
		changed = true
	}
	if changed {
		s.rebuildPoolLocked()
	}
}

func (s *Store) rebuildPoolLocked() {
	pool := s.systemPool.Clone()
	for _, source := range []*sourceState{&s.operator, &s.certDir, &s.cache, &s.inline, &s.memory} {
		for _, cert := range source.certificates {
			pool.AddCert(cert)
		}
	}
	s.pool = pool
	s.generation++
}

func (s *Store) poolSnapshot() (*x509.CertPool, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pool.Clone(), s.generation
}

func loadedSource(source sourceState, raw []byte, certs []*x509.Certificate, fingerprints []string) sourceState {
	source.hash = sha256.Sum256(raw)
	source.loaded = true
	source.durable = true
	source.certificates = append([]*x509.Certificate(nil), certs...)
	source.fingerprints = append([]string(nil), fingerprints...)
	return source
}

func parseBundle(raw []byte) ([]*x509.Certificate, []string, error) {
	remaining := bytes.TrimSpace(raw)
	if len(remaining) == 0 {
		return nil, nil, fmt.Errorf("CA bundle is empty")
	}
	var certificates []*x509.Certificate
	var fingerprints []string
	hasCA := false
	for len(remaining) > 0 {
		if !bytes.HasPrefix(remaining, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, nil, fmt.Errorf("CA bundle contains non-certificate PEM data")
		}
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, nil, fmt.Errorf("CA bundle contains invalid certificate PEM")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("parse CA certificate: %w", err)
		}
		certificates = append(certificates, cert)
		sum := sha256.Sum256(cert.Raw)
		fingerprints = append(fingerprints, hex.EncodeToString(sum[:]))
		hasCA = hasCA || cert.IsCA
		remaining = bytes.TrimSpace(rest)
	}
	if !hasCA {
		return nil, nil, fmt.Errorf("CA bundle contains no CA certificate")
	}
	return certificates, fingerprints, nil
}

func (s *Store) warnSource(source sourceState, err error) {
	key := source.name + "\x00" + source.path
	now := time.Now()
	s.warnMu.Lock()
	if last := s.warned[key]; !last.IsZero() && now.Sub(last) < warningInterval {
		s.warnMu.Unlock()
		return
	}
	s.warned[key] = now
	s.warnMu.Unlock()
	slog.Warn("gateway trust source unavailable; keeping last good roots",
		"source", source.name, "path", source.path, "err", err)
}
