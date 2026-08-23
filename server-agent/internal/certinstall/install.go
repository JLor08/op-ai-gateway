// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package certinstall

import (
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Report is what the agent has ACTUALLY installed on disk right now: the leaf
// fingerprint and its not_after (both zero when nothing is installed), the
// fingerprints of every root currently in ca.pem, and the configured
// cert_mode. It is derived FRESH from disk every time it is produced -- never
// cached -- so it stays truthful even when a fetch failed, was refused, or
// found nothing new.
type Report struct {
	Fingerprint    string
	NotAfter       time.Time
	Mode           string
	CAFingerprints []string
}

// Report returns the CURRENT report, read fresh from disk. With ModeOff it
// reports nothing (the installer never touches disk in that mode, so there
// is nothing on disk that belongs to it).
func (in *Installer) Report() Report {
	if in.mode == ModeOff {
		return Report{Mode: ModeOff}
	}
	r := Report{Mode: in.mode}
	if raw, err := os.ReadFile(filepath.Join(in.dir, fullchainFile)); err == nil {
		if leaf := parseLeafCert(raw); leaf != nil {
			r.Fingerprint = fingerprintDER(leaf.Raw)
			r.NotAfter = leaf.NotAfter
		}
	}
	r.CAFingerprints = readCAFingerprints(filepath.Join(in.dir, caFile))
	return r
}

// fingerprintDER MUST match certissue.FingerprintPEM's formula on the gateway
// side byte-for-byte: sha256 over the certificate's DER (cert.Raw), hex,
// lowercase (hex.EncodeToString always produces lowercase). If the two ever
// diverge, EVERY agent restart would see its own already-installed leaf as
// "different" from what the gateway reports, and reinstall + re-run the
// operator's reload command forever.
func fingerprintDER(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// diskState is everything readDiskState can observe about what is currently
// installed: whether there IS a leaf, whether its private key matches it
// (the pairing check), the memo etag (if any), and the raw ca.pem bytes (for
// the bundle-changed comparison in Sync).
type diskState struct {
	leaf            *x509.Certificate
	leafFingerprint string
	keyPaired       bool
	etag            string
	caPEM           []byte
}

// memoValid reports whether the on-disk state is trustworthy enough to drive
// a conditional GET (If-None-Match). All three must hold: a parseable leaf, a
// private key that actually matches it, AND a stored etag to replay. Any one
// missing means the disk state cannot be trusted to represent what that etag
// describes, so the caller fetches unconditionally instead -- this is what
// makes a half-completed rename (a new chain paired with an old key) recover
// on the very next tick instead of getting stuck behind a permanent 304.
func (s diskState) memoValid() bool {
	return s.leaf != nil && s.keyPaired && s.etag != ""
}

// readDiskState reads whatever of the five files currently exists in dir. A
// missing or unparseable file simply leaves its part of the state at its
// zero value -- readDiskState never returns an error, because "nothing
// readable" is itself a legitimate, common state (a fresh cert_dir on first
// run).
func readDiskState(dir string) diskState {
	var st diskState
	if raw, err := os.ReadFile(filepath.Join(dir, fullchainFile)); err == nil {
		if leaf := parseLeafCert(raw); leaf != nil {
			st.leaf = leaf
			st.leafFingerprint = fingerprintDER(leaf.Raw)
			if keyRaw, err := os.ReadFile(filepath.Join(dir, privkeyFile)); err == nil {
				if signer := parsePrivateKey(keyRaw); signer != nil {
					st.keyPaired = publicKeysMatch(leaf, signer)
				}
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(dir, etagFile)); err == nil {
		st.etag = strings.TrimSpace(string(raw))
	}
	if raw, err := os.ReadFile(filepath.Join(dir, caFile)); err == nil {
		st.caPEM = raw
	}
	return st
}

// parseLeafCert decodes the FIRST PEM block and parses it as a certificate.
// Returns nil on anything unusable -- missing file, non-PEM, or an
// unparseable DER -- every caller treats "cannot read it" as "make no claim",
// never as an error to propagate (mirrors the gateway portal's own
// parseLeaf).
func parseLeafCert(raw []byte) *x509.Certificate {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	return cert
}

// parsePrivateKey decodes the FIRST PEM block and tries every private-key
// encoding the gateway could plausibly send: SEC1 ("EC PRIVATE KEY", what
// certissue actually emits today for every certificate it issues), PKCS8
// (generic, in case a future issuer changes format), and PKCS1 ("RSA PRIVATE
// KEY"). Returns nil on total failure.
func parsePrivateKey(raw []byte) crypto.Signer {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if signer, ok := key.(crypto.Signer); ok {
			return signer
		}
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key
	}
	return nil
}

// publicKeyEqualer is satisfied by every standard-library public key type
// (*ecdsa.PublicKey, *rsa.PublicKey, ed25519.PublicKey) since Go 1.15.
type publicKeyEqualer interface {
	Equal(x crypto.PublicKey) bool
}

// publicKeysMatch is the pairing check: does signer's public key match
// leaf's. A half-completed rename (a new chain paired with an old key, or
// vice versa) makes this false, which is exactly what forces readDiskState's
// caller (Sync) to fetch unconditionally and heal the pair on the next tick
// instead of getting stuck behind a 304 forever.
func publicKeysMatch(leaf *x509.Certificate, signer crypto.Signer) bool {
	if leaf == nil || signer == nil {
		return false
	}
	eq, ok := leaf.PublicKey.(publicKeyEqualer)
	if !ok {
		return false
	}
	return eq.Equal(signer.Public())
}

// readCAFingerprints returns the fingerprint of every certificate block found
// in path (ca.pem may hold more than one root during a CA rotation's overlap
// window). A missing file, or a file with no parseable blocks, returns nil.
func readCAFingerprints(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	rest := raw
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		out = append(out, fingerprintDER(cert.Raw))
	}
	return out
}

// splitChain separates a fullchain PEM (leaf first, as certissue.Result
// always produces it) into the standalone leaf PEM and everything after it,
// RE-ENCODED (never the raw remainder bytes) so both outputs are in canonical
// PEM form regardless of the exact whitespace the gateway sent. restPEM is ""
// when the chain is exactly one certificate.
func splitChain(fullchainPEM string) (leafPEM, restPEM string) {
	block, rest := pem.Decode([]byte(fullchainPEM))
	if block == nil {
		return "", ""
	}
	leafPEM = string(pem.EncodeToMemory(block))
	var sb strings.Builder
	for {
		var next *pem.Block
		next, rest = pem.Decode(rest)
		if next == nil {
			break
		}
		_ = pem.Encode(&sb, next) // strings.Builder.Write never errors
	}
	return leafPEM, sb.String()
}

// install atomically writes the five files (fullchain/cert/chain?/ca?/
// privkey -- privkey LAST) for one certResponse. It is called ONLY after
// Sync has already decided the fetched material differs from what is
// installed; install itself does not re-check that.
//
// ca.pem: written only when body.CABundlePEM is non-empty; an empty bundle
// neither writes NOR deletes an existing ca.pem (a transient/misreporting
// server response must never blank a trust bundle a client still needs).
//
// chain.pem: written only when the chain has more than the leaf; if it does
// NOT (a single-certificate chain -- this feature never actually produces
// one today, since both ACME and internal chains carry leaf + issuer, but it
// is handled rather than assumed away), a stale chain.pem left over from a
// PREVIOUS multi-certificate chain is removed, best-effort, after the atomic
// batch commits.
func (in *Installer) install(body certResponse) error {
	if err := os.MkdirAll(in.dir, 0o755); err != nil {
		return err
	}
	leafPEM, restPEM := splitChain(body.FullchainPEM)
	if leafPEM == "" {
		return errors.New("certinstall: fullchain_pem has no parseable certificate")
	}

	aw := &atomicWriter{dir: in.dir}
	if err := aw.stage(fullchainFile, []byte(body.FullchainPEM), 0o644); err != nil {
		return err
	}
	if err := aw.stage(certFile, []byte(leafPEM), 0o644); err != nil {
		return err
	}
	if restPEM != "" {
		if err := aw.stage(chainFile, []byte(restPEM), 0o644); err != nil {
			return err
		}
	}
	if body.CABundlePEM != "" {
		if err := aw.stage(caFile, []byte(body.CABundlePEM), 0o644); err != nil {
			return err
		}
	}
	if err := aw.stage(privkeyFile, []byte(body.KeyPEM), 0o600); err != nil {
		return err
	}
	if err := aw.commit(); err != nil {
		return err
	}

	if restPEM == "" {
		// Best-effort cleanup of a now-stale chain.pem; never fails the install.
		if _, statErr := os.Stat(filepath.Join(in.dir, chainFile)); statErr == nil {
			_ = os.Remove(filepath.Join(in.dir, chainFile))
		}
	}
	return nil
}
