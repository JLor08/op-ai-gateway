// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package certfiles is the single source of truth for the filenames written
// into cert_dir by internal/certinstall and read back from cert_dir by
// internal/proxy (the mesh TLS-terminating proxy's leaf+key) and
// internal/trust (the read-only cert_dir/ca.pem trust source). It is a leaf
// package -- it imports nothing else from this module -- so every package
// that touches the cert-dir layout can depend on it without risking an
// import cycle.
package certfiles

// The names certinstall writes into cert_dir, exported here so a reader in a
// different package never has to spell them out as a fresh string literal.
const (
	// Fullchain is the leaf certificate followed by any intermediates,
	// consumed by internal/proxy's certHolder.LoadFromDir.
	Fullchain = "fullchain.pem"
	// Cert is the leaf certificate alone.
	Cert = "cert.pem"
	// Chain is the intermediate certificates alone (written only when the
	// chain has more than the leaf).
	Chain = "chain.pem"
	// CA is the trust bundle, read independently by internal/trust as a
	// read-only additive root source.
	CA = "ca.pem"
	// Privkey is the leaf's private key, consumed by internal/proxy's
	// certHolder.LoadFromDir.
	Privkey = "privkey.pem"
)
