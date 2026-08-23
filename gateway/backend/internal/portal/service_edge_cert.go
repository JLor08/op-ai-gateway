// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// certEdgeKind is the certificates.kind value of the ONE row that carries the
// gateway's own nginx certificate. It is deliberately a new value in the existing
// column -- no migration.
const certEdgeKind = "edge"

// certEdgeNamesMissingMessage is the operator-actionable cert_last_error note for
// "the edge switch is on but no name is configured". That combination is
// deliberately ALLOWED (the spec: turning the feature on before finishing its
// configuration must not break anything) -- it simply issues nothing, and this
// note is how the operator finds out why. It names the setting and the remedy and
// never contains key material.
const certEdgeNamesMissingMessage = "certificate.edge_names_missing"

// certPublicKind is the certificates.kind value of a managed public-facing
// domain (desiredCert.Kind == "public"). Named for the same reason
// certEdgeKind is: a stable symbol beats repeating the literal at every call
// site.
const certPublicKind = "public"

// modeFor returns the issuer mode that applies to ONE row. The edge certificate
// has its OWN mode because its constraints are different: it is reached over an
// internal name or a bare IP that a public CA cannot validate, while the internal
// names live under the NetBird base domain. All four combinations are valid and
// switchable in both directions.
//
// The public-domain row ALSO has its own mode (PublicIssuerMode), but unlike
// the edge row it FOLLOWS the global IssuerMode until an operator sets it
// explicitly (PublicIssuerMode == ""). That default is what keeps this
// byte-neutral: a deployment on self_signed for its internal names that also
// manages public domains keeps issuing those self-signed too, exactly as it
// did before PublicIssuerMode existed, until the operator opts a public
// domain into ACME (or self-signed) independently.
func (s CertSettings) modeFor(kind string) string {
	switch kind {
	case certEdgeKind:
		return s.EdgeIssuerMode
	case certPublicKind:
		if s.PublicIssuerMode != "" {
			return s.PublicIssuerMode
		}
		return s.IssuerMode
	default:
		return s.IssuerMode
	}
}

// certAcmeConfigFor returns the ACME (directory, email, weeklyLimit) that an
// issuance context uses: its OWN trio when that context's *ACMEShared is
// false, else the GLOBAL trio (DirectoryURL/Email/ACMEWeeklyLimit) -- the same
// one every kind used before per-context ACME accounts existed, which is what
// makes the default (shared=true, absent) byte-neutral. Only the edge and
// public kinds have a per-context trio; every other kind (gateway, server)
// always resolves to the global one, matching pre-unification behavior.
func (s CertSettings) certAcmeConfigFor(kind string) (directory, email string, weeklyLimit int) {
	switch kind {
	case certEdgeKind:
		if !s.EdgeACMEShared {
			return s.EdgeACMEDirectoryURL, s.EdgeACMEEmail, s.EdgeACMEWeeklyLimit
		}
	case certPublicKind:
		if !s.PublicACMEShared {
			return s.PublicACMEDirectoryURL, s.PublicACMEEmail, s.PublicACMEWeeklyLimit
		}
	}
	return s.DirectoryURL, s.Email, s.ACMEWeeklyLimit
}

// edgeModeUsable reports whether the EDGE row's own issuer mode has everything it
// mandatorily needs. It mirrors CertSettings' ok computation, but for the edge
// mode only: "acme" needs an account email (the CA has no other way to reach the
// operator), "self_signed" needs nothing beyond the module being on.
//
// This exists because CertSettings reports ok=false as soon as the INTERNAL mode's
// mandatory field is missing -- and the internal mode defaults to acme. An operator
// who wants ONLY the edge certificate (module on, internal mode left at its
// default, no acme_email, edge mode self_signed) would otherwise get ok=false and
// the edge certificate would never be issued, silently. ReconcileCertificates
// therefore treats ok=false as "the INTERNAL names are not servable", not as "do
// nothing", whenever the edge row is usable on its own.
//
// The email check reads the edge's EFFECTIVE account email (certAcmeConfigFor),
// not the global set.Email (review round-1 fix): an edge row with its OWN ACME
// account (cert_edge_acme_shared=false, its own email/directory filled in) must
// be usable on its own even when the global acme_email is blank -- checking the
// global email here would defeat the whole point of a per-context ACME account,
// exactly the way it would if this checked the global issuer mode instead of
// set.modeFor(certEdgeKind).
func edgeModeUsable(set CertSettings) bool {
	if set.modeFor(certEdgeKind) == IssuerModeACME {
		_, email, _ := set.certAcmeConfigFor(certEdgeKind)
		return email != ""
	}
	return true
}

// publicModeUsable is edgeModeUsable's public-domain counterpart (review
// round-1 addition): reports whether the PUBLIC row's own EFFECTIVE issuer
// mode (set.modeFor(certPublicKind)) and EFFECTIVE ACME account
// (certAcmeConfigFor(certPublicKind)) have everything they mandatorily need --
// "acme" needs a non-empty effective email, "self_signed" needs nothing
// beyond the module being on. Meaningful only when the caller has already
// established that public domains are actually wanted (ManagePublicDomain
// with a non-empty PublicDomains list); called with neither it harmlessly
// reports usable=true, mirroring edgeModeUsable's identical property when
// EdgeEnabled is false.
//
// This closes the same gap edgeModeUsable was originally written for, extended
// to the public-domain row: without it, ReconcileCertificates' "is anything at
// all usable" gate only ever considered the INTERNAL mode and the EDGE row, so
// a deployment that left the internal/global ACME unconfigured (the default)
// but fully configured a public domain -- either cert_public_issuer_mode=
// self_signed, or acme with its own (non-shared) email+directory -- got
// "no usable issuer" and the fully-configured public domain was never
// attempted, even though nothing about it actually depends on the global
// config.
func publicModeUsable(set CertSettings) bool {
	if set.modeFor(certPublicKind) == IssuerModeACME {
		_, email, _ := set.certAcmeConfigFor(certPublicKind)
		return email != ""
	}
	return true
}

// edgeDesired returns the wanted edge row, or ok=false when the feature is off or
// no name is configured. Note what it does NOT do: it never signals "delete" --
// see the prune exception in ReconcileCertificates.
//
// The list is normalized here and nowhere else: trimmed, lowercased, blanks
// dropped, and DEDUPED keeping the first occurrence. The dedupe is not cosmetic --
// certissue.SplitNames returns an ERROR on a duplicate, so a list like
// "edge.lan,edge.lan" (which the settings write path accepts, since each entry is
// individually valid) would otherwise fail every single issuance and leave the row
// stuck in backoff forever. Keeping the FIRST occurrence also keeps the primary
// name -- the row's identity and Subject CN -- stable.
func edgeDesired(set CertSettings) (desiredCert, bool) {
	if !set.EdgeEnabled || len(set.EdgeNames) == 0 {
		return desiredCert{}, false
	}
	names := make([]string, 0, len(set.EdgeNames))
	seen := make(map[string]bool, len(set.EdgeNames))
	for _, n := range set.EdgeNames {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	if len(names) == 0 {
		return desiredCert{}, false
	}
	return desiredCert{Domain: names[0], Kind: certEdgeKind, Names: names}, true
}

// sanDrift reports whether the STORED certificate covers a different name set
// than the configured one. The truth is the certificate itself, not a parallel
// list that could drift: the leaf is parsed out of the stored chain and its
// DNSNames + IPAddresses are compared as a set.
func sanDrift(cert routing.Certificate, names []string) bool {
	leaf := parseLeaf(cert.FullchainPEM)
	if leaf == nil {
		return false // nothing stored (or unparseable) -> renewDue already handles it
	}
	have := map[string]bool{}
	for _, d := range leaf.DNSNames {
		have[strings.ToLower(d)] = true
	}
	for _, ip := range leaf.IPAddresses {
		have[ip.String()] = true
	}
	want := map[string]bool{}
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		if ip := net.ParseIP(n); ip != nil {
			n = ip.String() // normalize 010.0.0.5 / fe80::0001 spellings
		}
		want[n] = true
	}
	if len(have) != len(want) {
		return true
	}
	for n := range want {
		if !have[n] {
			return true
		}
	}
	return false
}

// parseLeaf decodes the FIRST PEM block of a stored chain into a certificate.
// Returns nil on anything unusable (empty, not PEM, unparseable DER) -- every
// caller treats "cannot read it" as "make no claim about it", never as an error to
// propagate.
func parseLeaf(chainPEM string) *x509.Certificate {
	block, _ := pem.Decode([]byte(chainPEM))
	if block == nil {
		return nil
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	return leaf
}

// edgeChainFile/edgeKeyFile/edgeCAFile are the fixed names DeliverEdgeCertificate
// writes into Service.cert.edgeOutputDir. edgeCAFile is written ONLY in the
// self_signed mode (whenever CertificateCABundlePEM has something to hand back);
// edgeChainFile/edgeKeyFile always accompany a stored edge row.
const (
	edgeChainFile = "edge-fullchain.pem"
	edgeKeyFile   = "edge-key.pem"
	edgeCAFile    = "edge-ca.pem"
)

// EdgeDeliveryCapable reports whether the gateway can hand the certificate to
// its own nginx directly, on disk. This -- NOT the issuer mode, NOT a setting
// -- is what a later endpoint gates a key-download fallback on: the private key
// may only ever leave the process over HTTP when there is no safe local path,
// and an over-optimistic true here would let it do so even though a working
// local delivery was in fact available.
//
// A configured-but-so-far-untried directory reads as capable (edgeWriteErr is
// its zero value, ""): there is no known failure yet, only nothing delivered
// so far, which is not the same thing. Only an EMPTY directory setting, or a
// directory that has just failed a write, reads as not-capable.
func (s *Service) EdgeDeliveryCapable() bool {
	capable, _, _ := s.edgeDeliveryState()
	return capable
}

// edgeDeliveryState reads the three delivery facts -- capability, the last
// successful write and the last write error -- under ONE acquisition of edgeMu.
// They are read together and rendered together (EdgeCertificateDTO), and
// capability is DERIVED from the write error, so two separate acquisitions could
// interleave with a delivery and produce a self-contradictory view (capable=false
// with an empty WriteError, i.e. the portal claiming "no output directory
// configured" while one is configured and its failure is already cleared).
// certEdgeOutputDir is set once at construction and never written again, so
// reading it here needs no separate guard.
func (s *Service) edgeDeliveryState() (capable bool, writtenAt time.Time, writeErr string) {
	s.cert.edgeMu.Lock()
	defer s.cert.edgeMu.Unlock()
	return s.cert.edgeOutputDir != "" && s.cert.writeErr == "", s.cert.written, s.cert.writeErr
}

// configuredEdgePrimary is the normalized FIRST configured edge name -- the edge
// row's identity and its Subject CN. Empty when nothing is configured (or the
// settings cannot be read), which is the ONLY case in which a caller falls back
// to "whatever edge row happens to be stored".
//
// It is deliberately switch-INDEPENDENT (configuredEdgeNames forces the probe on):
// the portal shows the configured names while the feature is off too, and the row
// it describes must be the one those names identify.
func (s *Service) configuredEdgePrimary(ctx context.Context) string {
	set, _, err := s.CertSettings(ctx)
	if err != nil {
		return ""
	}
	if names := configuredEdgeNames(set); len(names) > 0 {
		return names[0]
	}
	return ""
}

// edgeRows lists the stored kind='edge' rows in store order (i.e. by domain).
func (s *Service) edgeRows(ctx context.Context) ([]routing.Certificate, error) {
	certs, err := s.routes.Certificates(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]routing.Certificate, 0, 2)
	for _, c := range certs {
		if c.Kind == certEdgeKind {
			rows = append(rows, c)
		}
	}
	return rows, nil
}

// edgeRowsAndPrimary pairs edgeRows with the configured primary name.
func (s *Service) edgeRowsAndPrimary(ctx context.Context) ([]routing.Certificate, string, error) {
	rows, err := s.edgeRows(ctx)
	if err != nil {
		return nil, "", err
	}
	return rows, s.configuredEdgePrimary(ctx), nil
}

// edgeRow returns the stored kind='edge' row the CONFIGURED primary name points
// at -- the row the operator is looking at, downloading and re-issuing.
//
// More than one edge row exists in ordinary operation, so "the first one" is NOT
// a safe answer: renaming the primary name creates a row for the NEW name, and a
// failed issuance persists that row (recordCertFailure) with status=error and NO
// material, while the old, still-valid row is deliberately kept (the prune
// exception: an edge row is never deleted until its replacement is stored). An
// abandoned rename (a -> b -> a) therefore leaves a stray b-row behind until the
// live certificate's next renewal -- and both stores return the rows ordered by
// domain, so picking the first would hand every consumer the stray row whenever
// it happens to sort earlier.
//
// Falling back to the first match when the primary has no row is what keeps a
// rename readable: the panel then shows the still-serving superseded row and
// EdgeCertificateView's Domain != Names[0] comparison surfaces that. No row at
// all is ErrCertificateNotFound -- every caller decides for itself whether that
// is an error (the download endpoints) or a no-op (the delivery).
func (s *Service) edgeRow(ctx context.Context) (routing.Certificate, error) {
	rows, primary, err := s.edgeRowsAndPrimary(ctx)
	if err != nil {
		return routing.Certificate{}, err
	}
	return pickEdgeRow(rows, primary)
}

// edgeRowFor is edgeRow with the primary name supplied by a caller that has
// already read the settings, so the rendered name list and the rendered row can
// never come from two different reads.
func (s *Service) edgeRowFor(ctx context.Context, primary string) (routing.Certificate, error) {
	rows, err := s.edgeRows(ctx)
	if err != nil {
		return routing.Certificate{}, err
	}
	return pickEdgeRow(rows, primary)
}

// pickEdgeRow implements the resolution rule: the configured primary's row wins,
// else the first stored edge row.
func pickEdgeRow(rows []routing.Certificate, primary string) (routing.Certificate, error) {
	if len(rows) == 0 {
		return routing.Certificate{}, ErrCertificateNotFound
	}
	if primary != "" {
		for _, c := range rows {
			if c.Domain == primary {
				return c, nil
			}
		}
	}
	return rows[0], nil
}

// edgeDeliveryRow returns the edge row whose material belongs on disk. This is
// the ONE place that deliberately differs from edgeRow: what nginx must serve is
// governed by CONTINUITY, not by what is configured. During a failed rename the
// configured primary's row carries no material at all (recordCertFailure keeps
// the row and its error, nothing else) while the superseded row is still the
// certificate in force -- delivering "the configured row" would then stop
// re-materializing a working certificate, so a fresh (or reset) volume would keep
// nginx on its untrusted bootstrap pair until the rename finally succeeds.
//
// Preference order: the configured primary WITH material, else any other edge row
// with material (at most one in practice -- a successful issuance prunes the
// superseded row), else the row edgeRow would have picked, so the caller's own
// "nothing to deliver yet" branch reports it.
func (s *Service) edgeDeliveryRow(ctx context.Context) (routing.Certificate, error) {
	rows, primary, err := s.edgeRowsAndPrimary(ctx)
	if err != nil {
		return routing.Certificate{}, err
	}
	fallback := -1
	for i, c := range rows {
		if c.FullchainPEM == "" || c.KeySealed == "" {
			continue
		}
		if primary != "" && c.Domain == primary {
			return c, nil
		}
		if fallback < 0 {
			fallback = i
		}
	}
	if fallback >= 0 {
		return rows[fallback], nil
	}
	return pickEdgeRow(rows, primary)
}

// DeliverEdgeCertificate materializes the STORED edge certificate row as files
// for the gateway's own nginx to read. It is atomic (a temp file in the SAME
// directory, chmod'd, then renamed into place -- mirroring the NetBird
// setup-key write elsewhere in this package) and writes a file ONLY when its
// content actually changed, so the nginx entrypoint wrapper's fingerprint poll
// never reloads for nothing.
//
// Called from two places: right after a successful issuance of the edge row
// (issueAndStore), and unconditionally at the end of every reconcile pass
// whenever the edge row is wanted (ReconcileCertificates) -- the second call is
// what re-fills a fresh, empty volume after a restart even on a pass where
// nothing needed (re-)issuing.
//
// A missing output directory, or nothing stored yet, are both first-class
// no-ops (nil error) -- see EdgeDeliveryCapable's doc comment for why an empty
// directory is not itself a failure. Any other failure -- most notably the
// stored private key failing to open, e.g. after a rotated
// OP_AI_GATEWAY_CERT_ENCRYPTION_KEY -- is recorded via noteEdgeWriteError and
// returned; the stored row itself is never touched (not deleted, not blanked)
// on this path, and no file is written at all rather than a partial set.
func (s *Service) DeliverEdgeCertificate(ctx context.Context) error {
	if s.cert.edgeOutputDir == "" {
		return nil // no local delivery configured -> the download path applies
	}
	row, err := s.edgeDeliveryRow(ctx)
	if errors.Is(err, ErrCertificateNotFound) {
		return nil // nothing stored yet
	}
	if err != nil {
		return err
	}
	if row.FullchainPEM == "" || row.KeySealed == "" {
		return nil // nothing to deliver yet
	}
	keyPEM, err := s.openCertSecret(row.KeySealed)
	if err != nil {
		s.noteEdgeWriteError(err)
		return err
	}
	type edgeFile struct {
		name    string
		content string
		mode    os.FileMode
	}
	files := []edgeFile{
		{edgeChainFile, row.FullchainPEM, 0o644},
		{edgeKeyFile, keyPEM, 0o600},
	}
	if bundle, err := s.CertificateCABundlePEM(ctx); err == nil && bundle != "" {
		files = append(files, edgeFile{edgeCAFile, bundle, 0o644})
	}
	for _, f := range files {
		if err := writeFileAtomic(filepath.Join(s.cert.edgeOutputDir, f.name), f.content, f.mode); err != nil {
			s.noteEdgeWriteError(err)
			return err
		}
	}
	s.cert.edgeMu.Lock()
	s.cert.written = s.clock().UTC()
	s.cert.writeErr = ""
	s.cert.edgeMu.Unlock()
	return nil
}

// writeFileAtomic writes content to path only when it differs from what is
// already there (a no-op comparison read; a missing/unreadable file just
// proceeds to the write), then swaps it in with a rename so a concurrent
// reader -- nginx's master process, which may reopen this exact path at any
// moment -- can never observe a partially-written file.
//
// The temp file lives in the SAME directory as path (a filesystem rename is
// only atomic within one filesystem/mount) and is named with a leading dot so
// it never collides with, or is mistaken for, one of the fixed edge file
// names. It is removed on every exit path except a successful rename (the
// deferred os.Remove after a successful os.Rename is a harmless no-op, since
// the file no longer exists at that path).
func writeFileAtomic(path, content string, mode os.FileMode) error {
	if old, err := os.ReadFile(path); err == nil && string(old) == content {
		return nil
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// noteEdgeWriteError records a delivery failure under edgeMu and logs it at
// Warn (this is otherwise invisible at the default log level, and the
// nginx-facing consequence -- a stale or missing certificate file -- is worth
// surfacing). It never logs key/PEM material: err here is either a plain I/O
// error naming only the path, or ErrCertKeyRequired (a bare sentinel), never
// the certificate content itself.
func (s *Service) noteEdgeWriteError(err error) {
	s.cert.edgeMu.Lock()
	s.cert.writeErr = err.Error()
	s.cert.edgeMu.Unlock()
	slog.Warn("edge certificate delivery failed", "dir", s.cert.edgeOutputDir, "err", err)
}

// ErrEdgeKeyManaged is returned by EdgeCertificateKeyPEM when the gateway can
// hand the edge private key to its own nginx on disk. It is NOT a permission
// error: the caller has every right to ask, there is simply a safe local path
// and the key must then not travel over HTTP at all. The gateway maps it to
// HTTP 409 certificate.edge_key_managed.
var ErrEdgeKeyManaged = errors.New("portal: the gateway delivers the edge certificate key to its own nginx")

// edgeDeliveryLocal/edgeDeliveryDownload are the two values of
// EdgeCertificateDTO.DeliveryMode -- the wire contract the portal renders as
// plain text ("writes to <path>" vs "download, no output path configured").
const (
	edgeDeliveryLocal    = "local"
	edgeDeliveryDownload = "download"
)

// EdgeCertificateDTO is the portal view of the gateway's OWN edge (nginx)
// certificate: its configuration, its timing, and -- the part no other
// certificate has -- HOW it reaches nginx. It carries NO key material and NO
// PEM, exactly like CertificateDTO.
//
// DeliveryMode is the honest reading of EdgeDeliveryCapable(), including its
// one counter-intuitive cell: a configured-but-never-yet-written directory
// reads as "local" (and therefore refuses the key download) because nothing has
// failed yet -- WrittenAt is then still null, which is how the portal can tell
// "not delivered yet" from "delivered". WriteError carries the reason a
// configured directory flipped back to "download".
type EdgeCertificateDTO struct {
	Enabled    bool   `json:"enabled"`
	IssuerMode string `json:"issuer_mode,omitempty"`
	// Names is the configured SAN list, normalized exactly as the issuance path
	// normalizes it (trimmed, lowercased, deduped, first occurrence kept) and in
	// that order -- Names[0] is the row's identity and its Subject CN. Never nil.
	Names []string `json:"names"`

	// Domain..LastError describe the STORED row; all empty/null when nothing has
	// been issued yet.
	Domain      string     `json:"domain,omitempty"`
	Status      string     `json:"status,omitempty"`
	Fingerprint string     `json:"fingerprint,omitempty"`
	NotBefore   *time.Time `json:"not_before,omitempty"`
	NotAfter    *time.Time `json:"not_after,omitempty"`
	IssuedAt    *time.Time `json:"issued_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`

	// DeliveryMode is "local" or "download" (never anything else).
	DeliveryMode string     `json:"delivery_mode"`
	OutputDir    string     `json:"output_dir,omitempty"`
	WrittenAt    *time.Time `json:"written_at,omitempty"`
	WriteError   string     `json:"write_error,omitempty"`
	// KeyDownloadAvailable is true exactly when EdgeCertificateKeyPEM would
	// SUCCEED: local delivery is impossible AND a key is actually stored. The
	// portal gates its download button on this one bit instead of re-deriving the
	// rule (and drifting from it) or offering a button that 404s.
	KeyDownloadAvailable bool `json:"key_download_available"`

	// NameConflict names the configured primary name when it is ALSO a name the
	// gateway manages an internal certificate for. In that state the reconcile
	// keeps the internal provenance and produces no edge row at all (a known
	// limit of the desired-set build) -- this field is what makes that visible
	// instead of leaving the operator with a switch that silently does nothing.
	// Best-effort and positives-only: it is derived from STORED data (server
	// rows, the configured gateway/public domains) because a status GET must not
	// make a live NetBird call, so a gateway name that is resolved live is not
	// covered. Empty means "no conflict found", not "no conflict possible".
	NameConflict string `json:"name_conflict,omitempty"`

	// RequireHTTPS/HTTPSObserved/LastEncryptedAt/LastPlainAt describe the
	// plaintext-refusal gate (cert_edge_require_https, Plan B) -- NOT filled by
	// EdgeCertificateView itself (the arming precondition's observation tracker
	// is in-process state on *gateway.Server, which this portal-layer method
	// cannot see), but by the gateway handler AFTER calling this method, which
	// holds both the tracker and this Service. See
	// internal/gateway/edge_certificates.go's handleSystemEdgeCertificate.
	//
	// RequireHTTPS mirrors the stored cert_edge_require_https switch.
	// HTTPSObserved is whether an encrypted hop was seen within the arming
	// window (the SAME fact ArmEdgeRequireHTTPS gates on) -- the portal disables
	// the switch's "on" transition while this is false. LastEncryptedAt/
	// LastPlainAt are the raw last-seen timestamps for either hop (nil = never
	// observed), so the operator can see whether a plaintext client is still
	// active even once the gate is armed.
	RequireHTTPS    bool       `json:"require_https"`
	HTTPSObserved   bool       `json:"https_observed"`
	LastEncryptedAt *time.Time `json:"last_encrypted_at,omitempty"`
	LastPlainAt     *time.Time `json:"last_plain_at,omitempty"`
}

// EdgeCertificateView reports the edge certificate's configuration, its stored
// row and its delivery state. Read-only: no store write, no network call, no
// file write.
func (s *Service) EdgeCertificateView(ctx context.Context) (EdgeCertificateDTO, error) {
	// The ok flag is deliberately ignored: it reports whether the INTERNAL mode is
	// fully configured, which says nothing about the edge row (whose mode is
	// independent). With the module off CertSettings returns a zero struct, so
	// Enabled reads false and every other field stays empty.
	set, _, err := s.CertSettings(ctx)
	if err != nil {
		return EdgeCertificateDTO{}, err
	}
	dto := EdgeCertificateDTO{
		Enabled:    set.EdgeEnabled,
		IssuerMode: set.EdgeIssuerMode,
		Names:      configuredEdgeNames(set),
	}
	// One acquisition for all three (see edgeDeliveryState): DeliveryMode is
	// derived from the same write error WriteError reports, so reading them
	// separately could render a contradiction.
	capable, writtenAt, writeErr := s.edgeDeliveryState()
	dto.DeliveryMode = edgeDeliveryDownload
	if capable {
		dto.DeliveryMode = edgeDeliveryLocal
	}
	dto.OutputDir = s.cert.edgeOutputDir
	dto.WrittenAt = timePtr(writtenAt)
	dto.WriteError = writeErr

	// Resolved against the SAME name list this DTO renders (dto.Names), so the
	// panel can never show one name and describe a different row's certificate.
	primary := ""
	if len(dto.Names) > 0 {
		primary = dto.Names[0]
	}
	row, err := s.edgeRowFor(ctx, primary)
	keyStored := false
	switch {
	case err == nil:
		dto.Domain = row.Domain
		dto.Status = row.Status
		dto.Fingerprint = row.Fingerprint
		dto.NotBefore = timePtr(row.NotBefore)
		dto.NotAfter = timePtr(row.NotAfter)
		dto.IssuedAt = timePtr(row.IssuedAt)
		dto.LastError = row.LastError
		keyStored = row.KeySealed != ""
	case errors.Is(err, ErrCertificateNotFound):
		// Nothing issued yet -- not an error for a status view.
	default:
		return EdgeCertificateDTO{}, err
	}
	// Both conditions of the key endpoint, in the same order it applies them.
	dto.KeyDownloadAvailable = !capable && keyStored
	// Only worth looking when the feature is on AND no row exists under the
	// configured primary name (either no row at all, or still the superseded one
	// from before a rename) -- that is exactly the symptom a collision produces.
	if set.EdgeEnabled && len(dto.Names) > 0 && dto.Domain != dto.Names[0] {
		dto.NameConflict = s.edgeNameConflict(ctx, set, dto.Names[0])
	}
	return dto, nil
}

// configuredEdgeNames is the normalized edge name list REGARDLESS of the on/off
// switch, so a status view can still show what is configured while the feature
// is off. It reuses edgeDesired -- the single normalization site -- by asking it
// about a copy with the switch forced on, so the list can never drift from the
// one the issuance path actually uses.
func configuredEdgeNames(set CertSettings) []string {
	probe := set
	probe.EdgeEnabled = true
	if d, ok := edgeDesired(probe); ok {
		return d.Names
	}
	return []string{}
}

// edgeNameConflict returns primary when it is also a name the gateway manages an
// internal certificate for, else "". See EdgeCertificateDTO.NameConflict for why
// this is positives-only: it consults STORED data exclusively (no NetBird call on
// a GET), so a live-resolved gateway name is not covered.
func (s *Service) edgeNameConflict(ctx context.Context, set CertSettings, primary string) string {
	if primary == "" {
		return ""
	}
	if set.GatewayDomain != "" && strings.EqualFold(set.GatewayDomain, primary) {
		return primary
	}
	if set.ManagePublicDomain {
		for _, d := range set.PublicDomains {
			if strings.EqualFold(strings.TrimSpace(d), primary) {
				return primary
			}
		}
	}
	servers, err := s.routes.AIServers(ctx)
	if err != nil {
		return "" // cannot tell -> claim nothing
	}
	for _, srv := range servers {
		domain := strings.ToLower(strings.TrimSpace(srv.Domain))
		if !srv.NetbirdEnabled || domain == "" {
			continue
		}
		if !certManaged(set.ServerScope, srv.CertificateOverride) {
			continue
		}
		if domain == primary {
			return primary
		}
	}
	return ""
}

// EdgeCertificateBundlePEM is the PUBLIC material of the edge certificate: its
// full chain plus, in the self_signed mode, the internal root(s) the upstream
// proxy has to trust. No private key, so this needs no capability gate -- unlike
// EdgeCertificateKeyPEM.
func (s *Service) EdgeCertificateBundlePEM(ctx context.Context) (string, error) {
	row, err := s.edgeRow(ctx)
	if err != nil {
		return "", err
	}
	if row.FullchainPEM == "" {
		return "", ErrCertificateNotFound
	}
	bundle := row.FullchainPEM
	if !strings.HasSuffix(bundle, "\n") {
		bundle += "\n"
	}
	// The internal root is appended ONLY when the EDGE row's own mode is
	// self_signed, i.e. when that root really is this certificate's trust anchor
	// (fix-round finding 4). An internal CA can exist while the edge row is acme
	// (the internal mode is independent, and the CA is never deleted on a switch) --
	// appending it there would ship a publicly trusted chain PLUS an unrelated root,
	// and an operator pasting that into proxy_ssl_trusted_certificate would make the
	// proxy accept an internal-CA-signed certificate for the edge name too, quietly
	// widening what acme mode guarantees.
	set, _, setErr := s.CertSettings(ctx)
	if setErr != nil {
		return "", setErr
	}
	if set.modeFor(certEdgeKind) == IssuerModeSelfSigned {
		// Best-effort: with no CA yet this returns ErrCertificateNotFound, which is not
		// a failure of THIS call -- the leaf is still the thing being downloaded.
		if ca, caErr := s.CertificateCABundlePEM(ctx); caErr == nil && ca != "" {
			bundle += ca
		}
	}
	return bundle, nil
}

// EdgeCertificateKeyPEM hands out the edge certificate's PRIVATE KEY -- the one
// place in this gateway that ever returns key material over HTTP.
//
// The gate is capability-driven and comes FIRST, before the row is even read:
// whenever the gateway can write the key to its own nginx (an output directory
// is configured and its last write did not fail) the key must not travel, and
// the answer is ErrEdgeKeyManaged. Only a deployment where local delivery is
// impossible -- no directory at all (k8s, where the web pod has no writable
// volume) or a directory that just failed -- may download it.
//
// The caller (the HTTP handler) is responsible for the audit line; this method
// is also reachable from tests and must not log a download that never happened.
func (s *Service) EdgeCertificateKeyPEM(ctx context.Context) (string, error) {
	if s.EdgeDeliveryCapable() {
		return "", ErrEdgeKeyManaged
	}
	row, err := s.edgeRow(ctx)
	if err != nil {
		return "", err
	}
	if row.KeySealed == "" {
		return "", ErrCertificateNotFound
	}
	key, err := s.openCertSecret(row.KeySealed)
	if err != nil {
		return "", err
	}
	if key == "" {
		return "", ErrCertificateNotFound
	}
	return key, nil
}

// ReissueEdgeCertificate marks ONLY the edge row due, so the next reconcile pass
// re-issues it with the currently configured edge issuer mode. Material, times
// and the attempt counter are preserved (markCertificateDue), so the stored
// certificate keeps serving -- and keeps being delivered -- until its replacement
// actually arrives. The internal rows are untouched; ReissueAllCertificates is
// the separate, fleet-wide action.
//
// The route is system-scoped (requireWebScope("system") at the handler); as of
// PT-2 Part 2b this also checks isSystem(principal) itself (ErrPrincipalForbidden
// otherwise) as defense-in-depth against a future internal caller that bypasses
// the HTTP gate.
func (s *Service) ReissueEdgeCertificate(ctx context.Context, principal auth.Token) error {
	if !isSystem(principal) {
		return ErrPrincipalForbidden
	}
	row, err := s.edgeRow(ctx)
	if err != nil {
		return err
	}
	if err := s.markCertificateDue(ctx, row.Domain); err != nil {
		return err
	}
	slog.Info("edge certificate marked for re-issue", "domain", row.Domain)
	return nil
}

// edgeProxyUnknown is the exact marker the generated configuration uses for a
// value the stored settings cannot answer. It is a COMMENT, never a directive
// value, so the output stays parseable and the placeholder stays obvious.
const edgeProxyUnknown = "# unbekannt - bitte eintragen"

// edgeProxyHostPlaceholder is the stand-in host written into a proxy_pass when
// no edge name is configured to derive one from.
const edgeProxyHostPlaceholder = "GATEWAY_EDGE_HOST"

// certGatewayDNSCacheTTL bounds how often the proxy-config generator re-resolves
// the gateway peer's NetBird DNS name through a live admin-API call.
const certGatewayDNSCacheTTL = 60 * time.Second

// cachedGatewayPeerDNS returns the gateway peer's NetBird DNS name, refreshing at
// most once per certGatewayDNSCacheTTL. It exists because ResolveGatewayPeerDNS is
// itself UNCACHED (it issues a GetPeer every call) -- the 60s TTL that makes such a
// resolution acceptable on a GET lives in the CALLER, and the precedent is the
// gateway layer's own cachedGatewayPeerDNS for the agent-config download. This is
// that same wrapper on the portal side.
//
// Best-effort in both directions: an error caches "" for the TTL as well, so a
// NetBird outage cannot turn one GET into a per-request hammer on the admin API,
// and the caller falls open to naming the value unknown. The lock is held across
// the (netbirdCallTimeout-bounded) resolve so concurrent callers single-flight one
// NetBird call rather than stampeding it.
func (s *Service) cachedGatewayPeerDNS(ctx context.Context) string {
	s.cert.gwDNSMu.Lock()
	defer s.cert.gwDNSMu.Unlock()
	if time.Now().Before(s.cert.gwDNSExp) {
		return s.cert.gwDNSVal
	}
	dns, err := s.ResolveGatewayPeerDNS(ctx)
	if err != nil {
		slog.Debug("proxy config: gateway peer name unresolvable", "err", err)
		dns = ""
	}
	s.cert.gwDNSVal = strings.ToLower(strings.TrimSpace(dns))
	s.cert.gwDNSExp = time.Now().Add(certGatewayDNSCacheTTL)
	return s.cert.gwDNSVal
}

// edgeACMEChallengeNames is the set of names whose /.well-known/acme-challenge/
// path the UPSTREAM proxy must forward to the gateway. It claims a name ONLY when
// this gateway really places an HTTP-01 order for it, because the output is pasted
// onto a proxy that terminates other, foreign domains and every claimed name is a
// server_name block taken away from whatever serves it today.
//
// Three gates, all keyed on the same rule the reconcile applies per row:
//   - the INTERNAL names (the configured gateway domain plus every managed NetBird
//     server domain) and the public domains ride on set.IssuerMode == acme. In the
//     self_signed internal mode the internal CA signs locally and NO order is ever
//     placed, so forwarding would hand a foreign proxy a path this gateway answers
//     404 for;
//   - the public domains additionally require cert_manage_public_domain. That is
//     the sharpest condition: claiming the challenge path of a domain the gateway
//     does NOT order for would break the upstream proxy's own certbot, which very
//     likely renews that very domain;
//   - the edge names ride on the EDGE mode being acme (its own switch), where
//     HTTP-01 must reach the gateway under exactly those names. An IP SAN is
//     skipped: it can be neither ACME-validated nor usefully vhost-matched.
//
// It deliberately does NOT emit a wildcard. `*.<base>` would look tidy and cover
// mesh names that do not exist yet, but it silently swallows every sibling of the
// zone -- including a public domain the operator explicitly EXCLUDED -- and nginx
// ranks a wildcard (priority 2) above a catch-all/default_server (priority 5), so
// such a block would hijack every :80 request for the whole domain on that proxy.
// The managed names are therefore ENUMERATED.
//
// The GATEWAY's own name is the one that cannot come from stored settings alone:
// with cert_gateway_domain unset, desiredCertificates resolves it live and ORDERS
// it, so leaving it out of the forwarding would make that order fail every backoff
// cycle -- in acme mode the gateway's own certificate would never come into
// existence. It is therefore resolved here too, through the TTL-cached
// cachedGatewayPeerDNS (so a GET stays call-free in the steady state), and
// FAIL-OPEN: on any error, timeout or empty result the second return value reports
// the name as unresolved and EdgeProxyConfig prints the honest "unknown" marker
// instead. A NetBird outage can neither fail this endpoint nor silently drop a name.
//
// Every entry is re-validated with ValidateEdgeName before it is returned -- the
// resolved name included, since that one arrives from an external API. This is the
// last place before interpolation into that configuration.
func (s *Service) edgeACMEChallengeNames(ctx context.Context, set CertSettings) (names []string, gatewayUnresolved bool, err error) {
	out := make([]string, 0, 4)
	add := func(n string) {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || ValidateEdgeName(n) != nil {
			return
		}
		for _, have := range out {
			if have == n {
				return
			}
		}
		out = append(out, n)
	}
	if set.IssuerMode == IssuerModeACME {
		if set.GatewayDomain != "" {
			add(set.GatewayDomain) // stored -> no call needed
		} else if dns := s.cachedGatewayPeerDNS(ctx); dns != "" && ValidateEdgeName(dns) == nil {
			add(dns)
		} else {
			gatewayUnresolved = true
		}
		servers, err := s.routes.AIServers(ctx)
		if err != nil {
			return nil, false, err
		}
		for _, srv := range servers {
			if !srv.NetbirdEnabled {
				continue
			}
			if !certManaged(set.ServerScope, srv.CertificateOverride) {
				continue
			}
			add(srv.Domain)
		}
		if set.ManagePublicDomain {
			for _, d := range set.PublicDomains {
				add(d)
			}
		}
	}
	if set.EdgeEnabled && set.modeFor(certEdgeKind) == IssuerModeACME {
		for _, n := range configuredEdgeNames(set) {
			if net.ParseIP(n) != nil {
				continue
			}
			add(n)
		}
	}
	return out, gatewayUnresolved, nil
}

// EdgeProxyConfig renders an nginx configuration for the reverse proxy that sits
// IN FRONT of the gateway. It is generated server-side because every input lives
// here and because a test can then pin the result.
//
// Three properties are load-bearing:
//   - it reads stored settings, plus ONE cached resolution: the gateway's own mesh
//     FQDN when cert_gateway_domain is unset and the internal mode is acme (via
//     cachedGatewayPeerDNS, 60s TTL, fail-open -- see edgeACMEChallengeNames for why
//     that name cannot simply be omitted). Nothing else is resolved live: no
//     filesystem access, no issuance state, and an unset base/public domain is
//     printed as the edgeProxyUnknown marker rather than looked up;
//   - every interpolated name has passed ValidateEdgeName (again) immediately
//     before being written, and an invalid one is omitted rather than written --
//     the output is pasted onto a more privileged machine, so a name carrying
//     ';', '}', '#' or a newline would be a directive injection;
//   - it contains no key material of any kind.
//
// It always renders the parts that match the CURRENT settings -- including the
// /.well-known/ forwarding when the edge feature itself is off, because the
// internal ACME names need it either way.
func (s *Service) EdgeProxyConfig(ctx context.Context) (string, error) {
	set, _, err := s.CertSettings(ctx)
	if err != nil {
		return "", err
	}
	names := configuredEdgeNames(set)
	// edgeHost lands in proxy_pass and proxy_ssl_name, so it goes through the SAME
	// defensive re-validation as every other interpolated name (fix-round finding 2).
	// configuredEdgeNames only trims/lowercases/dedupes -- today cert_edge_names IS
	// validated on its write path, but this function must not depend on that staying
	// the only writer.
	edgeHost := edgeProxyHostPlaceholder
	edgeHostKnown := false
	ipOnlyEdge := false
	for _, n := range names {
		if net.ParseIP(n) != nil {
			continue
		}
		if ValidateEdgeName(n) != nil {
			continue
		}
		edgeHost, edgeHostKnown = n, true
		break
	}
	if !edgeHostKnown && len(names) > 0 {
		// Every configured name is an IP (or unusable): there is no name to verify the
		// upstream leg against -- called out in the trust-anchor comment below.
		ipOnlyEdge = true
	}
	// The edge leg is https only when the gateway actually has an edge certificate
	// to present. With the feature off the leg is plain http -- stated outright
	// rather than implied.
	tls := set.EdgeEnabled && len(names) > 0
	target := "http://" + edgeHost + ":8080"
	if tls {
		target = "https://" + edgeHost + ":8443"
	}

	var b strings.Builder
	b.WriteString(`# =============================================================================
# nginx configuration for the reverse proxy IN FRONT OF the OP AI Gateway.
#
# Generated by the gateway from its own stored settings. It contains no key
# material. Read it before pasting: the machine it goes onto is more privileged
# than this one and very likely terminates other, foreign domains too -- add
# these blocks ALONGSIDE those, never in place of them.
#
# Anything the stored settings cannot answer is marked
#   ` + edgeProxyUnknown + `
# and left as an obvious UPPERCASE placeholder. Ports are the ones the bundled
# docker-compose publishes (80 -> 8080, 443 -> 8443); adjust them to your own
# topology.
# =============================================================================

# -----------------------------------------------------------------------------
# 1) ACME HTTP-01 forwarding -- plain :80, NEVER redirected to https.
#
# Let's Encrypt fetches these tokens anonymously over port 80. Redirecting this
# path to https kills the renewal of the very certificate that makes the redirect
# possible.
`)
	acmeNames, gatewayUnresolved, err := s.edgeACMEChallengeNames(ctx, set)
	if err != nil {
		return "", err
	}
	switch {
	case len(acmeNames) == 0:
		// Nothing is ordered over HTTP-01 right now, so claiming any server_name here
		// would take a name away from whatever serves it on that proxy for no gain.
		b.WriteString(`#
# NOT NEEDED with the current settings: no name is issued over ACME right now
# (both issuer modes are "self_signed", or no name is configured), so this
# gateway never places an HTTP-01 order and nothing has to be forwarded. Switch
# an issuer mode to "acme" and regenerate this configuration to get the block.
# -----------------------------------------------------------------------------

`)
	default:
		b.WriteString(`#
# The server_name list below is EXACTLY the set of names this gateway currently
# orders certificates for -- enumerated, never a wildcard: `)
		b.WriteString("`*." + "<domain>`" + ` would also
# swallow every sibling name on this proxy (nginx ranks a wildcard above a
# catch-all), including domains it renews itself.
# -----------------------------------------------------------------------------
server {
    listen 80;
`)
		fmt.Fprintf(&b, "    server_name %s;\n", strings.Join(acmeNames, " "))
		if gatewayUnresolved {
			// cert_gateway_domain is unset AND the cached live resolution could not
			// supply the gateway's own mesh FQDN (NetBird unreachable, no gateway peer
			// selected, or an unusable name). The reconcile DOES order that name, so
			// say so rather than silently omitting it.
			fmt.Fprintf(&b, "    %s: cert_gateway_domain is unset and the gateway's own mesh FQDN\n"+
				"    # could not be resolved from NetBird just now -- this gateway DOES order a\n"+
				"    # certificate for it, so add that name to server_name above (or set\n"+
				"    # cert_gateway_domain) or its renewal will keep failing.\n", edgeProxyUnknown)
		}
		if !set.ManagePublicDomain && len(set.PublicDomains) > 0 {
			b.WriteString("    # cert_manage_public_domain is OFF, so the configured public domain(s) are\n" +
				"    # deliberately absent: this gateway does not order certificates for them, and\n" +
				"    # forwarding their challenge path would break this proxy's own certbot.\n")
		}
		if !edgeHostKnown {
			fmt.Fprintf(&b, "    %s: the address this proxy reaches the gateway's nginx at\n", edgeProxyUnknown)
		}
		fmt.Fprintf(&b, `
    location /.well-known/acme-challenge/ {
        proxy_pass http://%s:8080;
        proxy_set_header Host $host;
    }
}

`, edgeHost)
	}

	b.WriteString(`# -----------------------------------------------------------------------------
# 2) The public entry point: TLS terminated here, re-encrypted to the gateway.
# -----------------------------------------------------------------------------
server {
    listen 443 ssl;
    # http2 on;   # nginx >= 1.25.1; on older builds append "http2" to the listen line
`)
	// certName is only used inside the certbot PATH SUGGESTION below, and it is a
	// validated name (validatedNameList) or a literal placeholder -- never raw input.
	certName := "PUBLIC_NAME"
	if pub := validatedNameList(set.PublicDomains); len(pub) > 0 {
		fmt.Fprintf(&b, "    server_name %s;\n", strings.Join(pub, " "))
		certName = pub[0]
	} else {
		fmt.Fprintf(&b, "    %s: the public name(s) clients use (cert_public_domains is not set)\n", edgeProxyUnknown)
		b.WriteString("    server_name PUBLIC_NAME;\n")
	}
	fmt.Fprintf(&b, `
    %s: THIS proxy's own certificate for the names above. The two paths below
    # are certbot's default layout -- a suggestion, not a claim about this machine,
    # which very likely serves further domains from the same store.
    ssl_certificate     /etc/letsencrypt/live/%s/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/%s/privkey.pem;

`, edgeProxyUnknown, certName, certName)
	if tls && ipOnlyEdge {
		// An IP-only edge configuration cannot be verified by name on this leg, and
		// saying so beats emitting a proxy_ssl_name that silently never matches.
		fmt.Fprintf(&b, `    %s: every configured edge name is an IP address, so the
    # upstream leg below CANNOT be verified by name -- nginx matches proxy_ssl_name
    # against the certificate's host names, never against an IP SAN. Either add a DNS
    # name to cert_edge_names (and to this proxy's resolution for the gateway), or
    # accept that proxy_ssl_verify has to be off on this leg.

`, edgeProxyUnknown)
	}
	if tls {
		if set.modeFor(certEdgeKind) == IssuerModeSelfSigned {
			fmt.Fprintf(&b, `    # The gateway presents a certificate from its OWN internal CA, so verify
    # against that root: download it in the portal (Certificates -> CA bundle)
    # and place it on this proxy.
    proxy_ssl_verify on;
    proxy_ssl_verify_depth 2;
    %s: where you put the downloaded root on THIS proxy.
    proxy_ssl_trusted_certificate /etc/nginx/ssl/op-gateway-ca.pem;
    proxy_ssl_name %s;
    proxy_ssl_server_name on;

`, edgeProxyUnknown, edgeHost)
		} else {
			fmt.Fprintf(&b, `    # The gateway's edge certificate is publicly trusted (Let's Encrypt), so the
    # system trust store is enough.
    proxy_ssl_verify on;
    proxy_ssl_verify_depth 2;
    # Debian/Ubuntu path; on RHEL: /etc/pki/tls/certs/ca-bundle.crt
    proxy_ssl_trusted_certificate /etc/ssl/certs/ca-certificates.crt;
    proxy_ssl_name %s;
    proxy_ssl_server_name on;

`, edgeHost)
		}
	} else {
		b.WriteString(`    # cert_edge_enabled is OFF: the gateway's nginx speaks plain http, so the leg
    # from this proxy to the gateway is UNENCRYPTED. Turn the edge certificate on
    # (Certificates -> gateway nginx) and regenerate this configuration to
    # encrypt it.

`)
	}
	fmt.Fprintf(&b, `    # Streaming-safe defaults, inherited by the locations below.
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    # Defense in depth: these headers are internal to the gateway (loopback auth
    # and the per-request server override). Blank them at the OUTERMOST proxy too
    # so a client can never inject them.
    proxy_set_header X-OP-Internal-Auth "";
    proxy_set_header X-OP-Internal-User "";
    proxy_set_header X-OP-Server-Override "";
    proxy_set_header X-OP-Server-Override-Force "";
    proxy_buffering off;
    proxy_read_timeout 3600s;

    # Everything else: portal, /api/, /healthz.
    location / { proxy_pass %[1]s; }

    # WebSocket agent-telemetry stream. Setting any proxy_set_header inside a
    # location DISCARDS the inherited set, so all of them are repeated here.
    location = /api/agent/v1/stream {
        proxy_pass %[1]s;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-OP-Internal-Auth "";
        proxy_set_header X-OP-Internal-User "";
        proxy_set_header X-OP-Server-Override "";
        proxy_set_header X-OP-Server-Override-Force "";
        proxy_buffering off;
        proxy_read_timeout 3600s;
    }

    # Inference endpoints carry base64 image data -- no body cap (the gateway
    # keeps its control-plane endpoints at 1 MiB itself).
    location /v1/        { client_max_body_size 0; proxy_pass %[1]s; }
    location /openai/    { client_max_body_size 0; proxy_pass %[1]s; }
    location /anthropic/ { client_max_body_size 0; proxy_pass %[1]s; }
}
`, target)
	return b.String(), nil
}

// validatedNameList drops every entry that is not an IP or a strict DNS name and
// lowercases/dedupes the rest. Nothing invalid is ever interpolated into the
// generated configuration -- see EdgeProxyConfig's doc comment.
func validatedNameList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, raw := range in {
		n := strings.ToLower(strings.TrimSpace(raw))
		if n == "" || ValidateEdgeName(n) != nil {
			continue
		}
		dup := false
		for _, have := range out {
			if have == n {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, n)
		}
	}
	return out
}
