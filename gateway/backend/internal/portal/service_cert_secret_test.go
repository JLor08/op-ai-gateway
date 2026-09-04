// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/certissue"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

// newTestCertCipher builds a real AES-256-GCM cipher from a key that is
// DELIBERATELY DIFFERENT from newTestCipher's (the capture key). Every test in
// this file wires the two ciphers simultaneously so "certificate keys are sealed
// with their own key" is proven by behaviour, not by naming: material sealed
// with one must be unreadable through the other.
func newTestCertCipher(t *testing.T) *capture.Cipher {
	t.Helper()
	c, err := capture.New(strings.Repeat("cd", 32))
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	return c
}

// TestSealCertSecretWithCertCipherRoundTrips is the happy path: a certificate
// private key seals with the CERTIFICATE cipher and opens again.
func TestSealCertSecretWithCertCipherRoundTrips(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), CertCipher: newTestCertCipher(t), Clock: fixedClock()})
	const keyPEM = "-----BEGIN EC PRIVATE KEY-----\nnot-a-real-key\n-----END EC PRIVATE KEY-----\n"
	sealed, err := svc.sealCertSecret(keyPEM)
	if err != nil {
		t.Fatalf("sealCertSecret: %v", err)
	}
	if !strings.HasPrefix(sealed, "enc:") {
		t.Fatalf("sealed = %q, want the enc: prefix", sealed)
	}
	if strings.Contains(sealed, "not-a-real-key") {
		t.Fatalf("sealed value leaks the plaintext key: %q", sealed)
	}
	got, err := svc.openCertSecret(sealed)
	if err != nil {
		t.Fatalf("openCertSecret: %v", err)
	}
	if got != keyPEM {
		t.Fatalf("openCertSecret = %q, want the original key", got)
	}
}

// TestCertAndCaptureSecretsAreNotInterchangeable is THE separation test: with
// BOTH keys configured and different, neither path can read the other's
// material. This is what makes the split real rather than nominal -- if
// sealCertSecret/openCertSecret ever fell back to (or were re-pointed at) the
// capture cipher, one of these two directions would start succeeding.
func TestCertAndCaptureSecretsAreNotInterchangeable(t *testing.T) {
	svc := NewService(ServiceDeps{
		SystemSettings: NewMemorySystemSettings(),
		Cipher:         newTestCipher(t),     // capture key: SMTP password + NetBird token
		CertCipher:     newTestCertCipher(t), // certificate key: private keys only
		Clock:          fixedClock(),
	})

	// A certificate secret must NOT be openable through the SMTP/NetBird path.
	certSealed, err := svc.sealCertSecret("cert-private-key")
	if err != nil {
		t.Fatalf("sealCertSecret: %v", err)
	}
	if got, err := svc.openSecret(certSealed); err == nil {
		t.Fatalf("openSecret read a certificate secret as %q, want an error (the capture cipher must not decrypt certificate material)", got)
	}

	// An SMTP/NetBird secret must NOT be openable through the certificate path.
	captureSealed, err := svc.sealSecret("smtp-password")
	if err != nil {
		t.Fatalf("sealSecret: %v", err)
	}
	if got, err := svc.openCertSecret(captureSealed); err == nil {
		t.Fatalf("openCertSecret read a capture-sealed secret as %q, want an error (the certificate cipher must not decrypt SMTP/NetBird material)", got)
	}

	// Sanity: each path still reads its OWN material, so the failures above are
	// about the key, not about a broken seal.
	if got, err := svc.openCertSecret(certSealed); err != nil || got != "cert-private-key" {
		t.Fatalf("openCertSecret(own) = %q, %v; want cert-private-key, nil", got, err)
	}
	if got, err := svc.openSecret(captureSealed); err != nil || got != "smtp-password" {
		t.Fatalf("openSecret(own) = %q, %v; want smtp-password, nil", got, err)
	}
}

// TestSMTPAndNetbirdSecretsStillUseTheCaptureCipher is the collateral-damage
// guard: the SMTP password and the NetBird admin token must keep being sealed
// with the CAPTURE key exactly as before this change, or existing deployments
// could not read what they already stored. Proven twice over -- the values
// round-trip through their normal readers, AND a service holding the
// certificate key as its capture cipher cannot read them (so they demonstrably
// are not sealed with the certificate key).
func TestSMTPAndNetbirdSecretsStillUseTheCaptureCipher(t *testing.T) {
	settings := NewMemorySystemSettings()
	ctx := context.Background()
	captureCipher := newTestCipher(t)
	svc := NewService(ServiceDeps{
		SystemSettings: settings,
		Cipher:         captureCipher,
		CertCipher:     newTestCertCipher(t),
		Clock:          fixedClock(),
	})

	on := true
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		SMTPEnabled:  &on,
		SMTPHost:     strPtr("smtp.example.test"),
		SMTPPort:     intPtr(587),
		SMTPFrom:     strPtr("noreply@example.test"),
		SMTPPassword: strPtr("hunter2"),
		// The module must be ON for NetbirdConfig to report a usable config.
		NetbirdEnabled: &on,
		NetbirdURL:     strPtr("https://api.netbird.io"),
		NetbirdToken:   strPtr("nbp_secret"),
	}); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}

	smtp, err := svc.SMTPRuntimeConfig(ctx)
	if err != nil {
		t.Fatalf("SMTPRuntimeConfig: %v", err)
	}
	if smtp.Password != "hunter2" {
		t.Fatalf("SMTP password = %q, want hunter2 (the capture-cipher round-trip regressed)", smtp.Password)
	}
	nb, ok, err := svc.NetbirdConfig(ctx)
	if err != nil || !ok {
		t.Fatalf("NetbirdConfig ok=%v err=%v, want a usable config", ok, err)
	}
	if nb.Token != "nbp_secret" {
		t.Fatalf("NetBird token = %q, want nbp_secret (the capture-cipher round-trip regressed)", nb.Token)
	}

	// Same stored bytes, read by a service whose CAPTURE cipher is the
	// CERTIFICATE key: both must fail, i.e. the two secrets really are sealed
	// with the capture key and not with the certificate key.
	wrong := NewService(ServiceDeps{SystemSettings: settings, Cipher: newTestCertCipher(t), Clock: fixedClock()})
	if cfg, err := wrong.SMTPRuntimeConfig(ctx); err == nil {
		t.Fatalf("SMTPRuntimeConfig with the certificate key succeeded (password %q); the SMTP password must stay sealed with the capture key", cfg.Password)
	}
	if cfg, _, err := wrong.NetbirdConfig(ctx); err == nil {
		t.Fatalf("NetbirdConfig with the certificate key succeeded (token %q); the NetBird token must stay sealed with the capture key", cfg.Token)
	}
}

// TestOpenCertSecretErrorNamesTheCertificateKey pins the operator-facing wording
// of a failed decrypt. capture.Cipher's own error reads "capture: open: cipher:
// message authentication failed", and that string is what lands in a
// certificate's last_error column -- an operator would go hunting in the CAPTURE
// key after changing the CERTIFICATE key. The message must name the variable that
// actually governs this value, and must still carry no key material.
func TestOpenCertSecretErrorNamesTheCertificateKey(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), CertCipher: newTestCertCipher(t), Clock: fixedClock()})
	sealed, err := svc.sealCertSecret("cert-private-key")
	if err != nil {
		t.Fatalf("sealCertSecret: %v", err)
	}

	// Same stored bytes, DIFFERENT certificate cipher (a rotated key).
	rotated := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), CertCipher: newTestCipher(t), Clock: fixedClock()})
	_, err = rotated.openCertSecret(sealed)
	if err == nil {
		t.Fatal("openCertSecret succeeded with the wrong cipher")
	}
	if !strings.Contains(err.Error(), "OP_AI_GATEWAY_CERT_ENCRYPTION_KEY") {
		t.Fatalf("openCertSecret error = %q, want it to name OP_AI_GATEWAY_CERT_ENCRYPTION_KEY (the raw capture.Cipher text points at the wrong key)", err)
	}
	if strings.Contains(err.Error(), "cert-private-key") {
		t.Fatalf("openCertSecret error leaks plaintext: %q", err)
	}
}

// TestSealCertSecretRefusesOnDiskStoreWithoutTheCertificateKey pins the
// mandatory-where-it-matters rule: on a disk-backed store a missing certificate
// key means REFUSE, never plaintext -- and never a fall back to the capture
// cipher, which is deliberately present here to make that fallback observable
// if anyone adds one.
func TestSealCertSecretRefusesOnDiskStoreWithoutTheCertificateKey(t *testing.T) {
	svc := NewService(ServiceDeps{
		SystemSettings: NewMemorySystemSettings(),
		Cipher:         newTestCipher(t), // capture key present, certificate key absent
		Clock:          fixedClock(),
	})
	sealed, err := svc.sealCertSecret("cert-private-key")
	if !errors.Is(err, ErrCertKeyRequired) {
		t.Fatalf("sealCertSecret error = %v, want ErrCertKeyRequired (no fallback to the capture cipher)", err)
	}
	if sealed != "" {
		t.Fatalf("sealCertSecret returned %q on refusal, want the empty string (nothing must be persisted)", sealed)
	}
	// Reading an already-sealed certificate secret fails the same way rather
	// than being attempted with the capture key.
	if _, err := svc.openCertSecret("enc:AAAA"); !errors.Is(err, ErrCertKeyRequired) {
		t.Fatalf("openCertSecret error = %v, want ErrCertKeyRequired", err)
	}
}

// TestSealCertSecretVolatileStorePlaintextWithoutAKey keeps the dev path
// working: the volatile in-memory settings store still takes the "plain:"
// branch with no key at all (never written to disk, gone on process exit).
func TestSealCertSecretVolatileStorePlaintextWithoutAKey(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), SettingsVolatile: true, Clock: fixedClock()})
	sealed, err := svc.sealCertSecret("cert-private-key")
	if err != nil {
		t.Fatalf("sealCertSecret on the volatile store: %v", err)
	}
	if sealed != "plain:cert-private-key" {
		t.Fatalf("sealed = %q, want %q", sealed, "plain:cert-private-key")
	}
	got, err := svc.openCertSecret(sealed)
	if err != nil || got != "cert-private-key" {
		t.Fatalf("openCertSecret = %q, %v; want cert-private-key, nil", got, err)
	}
}

// TestReconcileOnADiskStoreSealsEveryKeyWithTheCertificateCipher is the
// end-to-end proof for the ISSUANCE sites (the leaf key in issueAndStore, the
// internal CA key in newCA, and reading it back in loadCA). It runs on a
// DISK-backed settings store, where the two seal paths are actually
// distinguishable -- the existing certificate tests all use the VOLATILE
// fixture, where both collapse to "plain:" and cannot tell them apart.
//
// BOTH ciphers are wired, with DIFFERENT keys (like every other test in this
// file). That is what makes the "the leaf key does not open through the capture
// path" assertion below load-bearing: with the capture cipher absent, openSecret
// refuses ANY "enc:" input for want of a key, so the assertion would hold no
// matter what sealed the leaf. With a real, different capture cipher present it
// fails only because the material genuinely is not the capture key's.
// A site reaching for the capture seal path is still caught anyway -- by
// openCertSecret on the leaf key, and by the pass-2 fingerprint check.
func TestReconcileOnADiskStoreSealsEveryKeyWithTheCertificateCipher(t *testing.T) {
	routes := routing.NewMemoryStore()
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{
		Routes:         routes,
		SystemSettings: settings, // SettingsVolatile:false => disk-store rules
		CertCipher:     newTestCertCipher(t),
		Cipher:         newTestCipher(t), // capture key: real, and DIFFERENT
	})
	ctx := context.Background()
	on := true
	mode := IssuerModeSelfSigned
	base := "int.example.test"
	scope := "all"
	gw := ""
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:       &on,
		CertIssuerMode:    &mode,
		CertBaseDomain:    &base,
		CertGatewayDomain: &gw,
		CertServerScope:   &scope,
	}); err != nil {
		t.Fatalf("enable self_signed: %v", err)
	}
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")

	svc.ReconcileCertificates(ctx)

	ca, err := svc.CertificateCAView(ctx)
	if err != nil {
		t.Fatalf("CertificateCAView: %v", err)
	}
	if !ca.Present || ca.LastError != "" {
		t.Fatalf("CA present=%v last_error=%q, want a CA sealed with the certificate key", ca.Present, ca.LastError)
	}

	cert, err := routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatalf("CertificateByDomain: %v", err)
	}
	if cert.Status != "active" || cert.KeySealed == "" {
		t.Fatalf("certificate = %+v, want an active certificate with a sealed key", cert)
	}
	// The LEAF key is readable through the certificate path and only that path.
	keyPEM, err := svc.openCertSecret(cert.KeySealed)
	if err != nil || !strings.Contains(keyPEM, "PRIVATE KEY") {
		t.Fatalf("openCertSecret(leaf key) = %q, %v; want the private key PEM", keyPEM, err)
	}
	if _, err := svc.openSecret(cert.KeySealed); err == nil {
		t.Fatalf("openSecret read the leaf key; it must be sealed with the certificate key only")
	} else if errors.Is(err, ErrSMTPKeyRequired) {
		// The capture cipher must be genuinely present, or the line above proves
		// nothing (any "enc:" input fails for want of a key).
		t.Fatalf("openSecret failed for want of a capture key (%v); this fixture must wire a real, different capture cipher", err)
	}

	// A SECOND pass must LOAD the stored CA (loadCA -> openCertSecret) rather
	// than fail to read it and silently regenerate a fresh root: the fingerprint
	// has to be identical, and no leaf may be re-issued under a new root.
	before := ca.Fingerprint
	svc.ReconcileCertificates(ctx)
	after, err := svc.CertificateCAView(ctx)
	if err != nil {
		t.Fatalf("CertificateCAView (pass 2): %v", err)
	}
	if !after.Present || after.Fingerprint != before {
		t.Fatalf("CA fingerprint pass1=%q pass2=%q (present %v), want the SAME root -- the stored CA key could not be reopened", before, after.Fingerprint, after.Present)
	}
	if after.PreviousFingerprint != "" {
		t.Fatalf("PreviousFingerprint = %q after an uneventful second pass, want empty (an unread CA key caused a rotation)", after.PreviousFingerprint)
	}
}

// TestAcmeAccountUsesTheCertificateCipher pins the two ACME ACCOUNT-key sites.
// Reading: an account key sealed with the certificate cipher must open. Writing:
// with no certificate key on a disk store, minting a new account key must refuse
// with ErrCertKeyRequired BEFORE any ACME registration is attempted (the seal
// happens before Register, so this needs no network) -- and the capture cipher
// being present must not rescue it.
func TestAcmeAccountUsesTheCertificateCipher(t *testing.T) {
	ctx := context.Background()
	const dir = "https://acme.example.test/directory"

	// Read path: seal an account key with the certificate cipher, then load it.
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, CertCipher: newTestCertCipher(t)})
	key, err := certissue.GenerateAccountKey()
	if err != nil {
		t.Fatal(err)
	}
	pemStr, err := certissue.MarshalECKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := svc.sealCertSecret(pemStr)
	if err != nil {
		t.Fatalf("sealCertSecret(account key): %v", err)
	}
	now := time.Now().UTC()
	for _, kv := range [][2]string{
		// dir is ALSO written as the configured global directory: accountFor keys
		// the legacy unprefixed acme_account_* slot to "directory == the configured
		// acme_directory_url" (see acmeAccountKeysFor), so without this the account
		// below would be filed under a hash-suffixed slot the seeded keys never
		// touch.
		{acmeDirectoryURLKey, dir},
		{acmeAccountKeyKey, sealed},
		{acmeAccountDirectoryKey, dir},
		{acmeAccountURIKey, "https://acme.example.test/acct/1"},
	} {
		if err := settings.SetSystemSetting(ctx, kv[0], kv[1], now); err != nil {
			t.Fatal(err)
		}
	}
	got, uri, err := svc.accountFor(ctx, dir, "")
	if err != nil {
		t.Fatalf("accountFor: %v", err)
	}
	if got == nil || !got.PublicKey.Equal(&key.PublicKey) {
		t.Fatal("accountFor returned a different account key than the stored one")
	}
	if uri != "https://acme.example.test/acct/1" {
		t.Fatalf("account uri = %q", uri)
	}

	// Write path: no certificate key on a disk store -> refuse, no fallback to
	// the capture cipher (which is wired here on purpose), no network call.
	fresh := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Cipher: newTestCipher(t)})
	if _, _, err := fresh.accountFor(ctx, dir, ""); !errors.Is(err, ErrCertKeyRequired) {
		t.Fatalf("accountFor error = %v, want ErrCertKeyRequired before any registration", err)
	}
}

// TestReconcileWithoutTheCertificateKeyRecordsAnActionableError is the operator
// half of the mandatory rule: with the certificate key unset on a disk-backed
// store, a reconcile pass issues NOTHING and says why -- naming
// OP_AI_GATEWAY_CERT_ENCRYPTION_KEY, not the capture key. The capture cipher is
// wired to prove it is not silently used as a substitute.
func TestReconcileWithoutTheCertificateKeyRecordsAnActionableError(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{
		Routes:         routing.NewMemoryStore(),
		SystemSettings: settings,
		Cipher:         newTestCipher(t), // capture key present, certificate key absent
	})
	ctx := context.Background()
	on := true
	mode := IssuerModeSelfSigned
	base := "int.example.test"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:    &on,
		CertIssuerMode: &mode,
		CertBaseDomain: &base,
	}); err != nil {
		t.Fatalf("enable the certificate module: %v", err)
	}

	svc.ReconcileCertificates(ctx)

	values, err := settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := CertLastError(values)
	if got != certSealKeyMessage {
		t.Fatalf("cert_last_error = %q, want the seal-key message %q", got, certSealKeyMessage)
	}
	if !strings.Contains(got, "OP_AI_GATEWAY_CERT_ENCRYPTION_KEY") {
		t.Fatalf("cert_last_error = %q, want it to name OP_AI_GATEWAY_CERT_ENCRYPTION_KEY so the operator knows WHICH variable to set", got)
	}
	if strings.Contains(got, "CAPTURE_ENCRYPTION_KEY") {
		t.Fatalf("cert_last_error = %q, must not point at the capture key -- that would be wrong advice", got)
	}
	// Nothing was issued and no CA exists: a missing certificate key stops the
	// module rather than degrading to an unencrypted key on disk.
	if ca, err := svc.CertificateCAView(ctx); err != nil || ca.Present {
		t.Fatalf("CertificateCAView.Present = %v (err %v), want no CA", ca.Present, err)
	}
	if certs, err := svc.CertificatesView(ctx); err != nil || len(certs) != 0 {
		t.Fatalf("CertificatesView = %+v (err %v), want zero certificates", certs, err)
	}
}

// failCAWritesSettings fails SetSystemSetting for the three internal-CA columns
// only, so newCA errors with a NON-key cause (a store failure) while the
// cert_last_error write itself still works. Used to prove the key-specific log
// line does not relabel every ensureCA failure as a missing key.
type failCAWritesSettings struct {
	*MemorySystemSettings
	fail bool
}

func (f *failCAWritesSettings) SetSystemSetting(ctx context.Context, key, value string, now time.Time) error {
	if f.fail && (key == certCACertKey || key == certCAKeySealedKey || key == certCAPrevCertKey) {
		return errors.New("store: disk full")
	}
	return f.MemorySystemSettings.SetSystemSetting(ctx, key, value, now)
}

// SetSystemSettings routes the atomic batch back through this fake's fault-aware
// per-key SetSystemSetting, so the CA-column fault still fires in a batch instead
// of the promoted (fault-free) MemorySystemSettings batch method bypassing it.
func (f *failCAWritesSettings) SetSystemSettings(ctx context.Context, values map[string]string, now time.Time) error {
	for key, value := range values {
		if err := f.SetSystemSetting(ctx, key, value, now); err != nil {
			return err
		}
	}
	return nil
}

// TestReconcileSelfSignedLogsTheVariableForAMissingKey pins the LOG channel of
// the self_signed abort gate. The gate's own line is
// `internal CA unavailable; skipping certificate pass err=system.cert_key_required`
// — the bare sentinel, naming no variable — so an operator reading only the logs
// could not tell which key to set (the portal note always did name it). Both docs
// promise the gateway logs the cause at WARN naming the variable, in BOTH modes.
//
// The second half is the guard rail: an ensureCA failure has other causes, and
// those must keep their generic message and NOT be relabelled as a missing key.
func TestReconcileSelfSignedLogsTheVariableForAMissingKey(t *testing.T) {
	ctx := context.Background()

	captureLogs := func(t *testing.T, run func()) string {
		t.Helper()
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		defer slog.SetDefault(prev)
		run()
		return buf.String()
	}

	enable := func(t *testing.T, svc *Service) {
		t.Helper()
		on := true
		mode := IssuerModeSelfSigned
		base := "int.example.test"
		if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
			CertEnabled:    &on,
			CertIssuerMode: &mode,
			CertBaseDomain: &base,
		}); err != nil {
			t.Fatalf("enable self_signed: %v", err)
		}
	}

	// Missing certificate key on a disk store: the emitted WARN must name the
	// variable, and must not leak key material.
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{
		Routes:         routing.NewMemoryStore(),
		SystemSettings: settings,
		Cipher:         newTestCipher(t), // capture key present, certificate key absent
	})
	enable(t, svc)
	out := captureLogs(t, func() { svc.ReconcileCertificates(ctx) })
	if !strings.Contains(out, "OP_AI_GATEWAY_CERT_ENCRYPTION_KEY") {
		t.Fatalf("self_signed log = %q, want a WARN naming OP_AI_GATEWAY_CERT_ENCRYPTION_KEY (the gate alone logs only the bare %q sentinel)", out, ErrCertKeyRequired)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("self_signed log = %q, want the variable named at WARN (Debug is invisible by default)", out)
	}
	if strings.Contains(out, "CAPTURE_ENCRYPTION_KEY") {
		t.Fatalf("self_signed log = %q, must not point at the capture key", out)
	}

	// Guard rail: a NON-key ensureCA failure (the CA writes fail) keeps the
	// generic note and gets no key-specific line.
	failing := &failCAWritesSettings{MemorySystemSettings: NewMemorySystemSettings()}
	svc2 := NewService(ServiceDeps{
		Routes:         routing.NewMemoryStore(),
		SystemSettings: failing,
		CertCipher:     newTestCertCipher(t), // sealing WORKS; the store write is what fails
	})
	enable(t, svc2)
	failing.fail = true
	out2 := captureLogs(t, func() { svc2.ReconcileCertificates(ctx) })
	if strings.Contains(out2, "OP_AI_GATEWAY_CERT_ENCRYPTION_KEY") {
		t.Fatalf("non-key ensureCA failure log = %q, must NOT be relabelled as a missing certificate key", out2)
	}
	values, err := failing.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := CertLastError(values); got != certCAUnavailableMessage {
		t.Fatalf("non-key ensureCA failure: cert_last_error = %q, want the generic CA-unavailable message %q", got, certCAUnavailableMessage)
	}
}

// TestUnreadableCAWithoutTheKeyClaimsNoNewRoot pins that the recovery log tells
// the TRUTH. A stored internal CA that cannot be OPENED (its key was rotated or
// removed) sends ensureCA down the "mint a fresh root" branch -- but newCA's
// sealCertSecret refuses BEFORE any SetSystemSetting when the certificate key is
// missing, so NOTHING is minted and the stored root stays byte-identical.
// Announcing a new root as fact there would tell an operator mid-incident that
// every client has to re-trust a bundle that never changed. The line must state
// the INTENT, the outcome must come from the paths that actually know it, and the
// missing-key guidance (naming the variable) must still be reached.
func TestUnreadableCAWithoutTheKeyClaimsNoNewRoot(t *testing.T) {
	ctx := context.Background()
	settings := NewMemorySystemSettings()

	// Pass 1: a working service mints and stores a real internal CA.
	svc := NewService(ServiceDeps{
		Routes:         routing.NewMemoryStore(),
		SystemSettings: settings,
		CertCipher:     newTestCertCipher(t),
	})
	on := true
	mode := IssuerModeSelfSigned
	base := "int.example.test"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:    &on,
		CertIssuerMode: &mode,
		CertBaseDomain: &base,
	}); err != nil {
		t.Fatalf("enable self_signed: %v", err)
	}
	svc.ReconcileCertificates(ctx)
	before, err := settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before[certCACertKey] == "" {
		t.Fatal("no internal CA stored after the first pass; the fixture cannot exercise the unreadable-CA branch")
	}

	// Pass 2: same stored CA, certificate key GONE (rotated away) on a disk store.
	rotated := NewService(ServiceDeps{
		Routes:         routing.NewMemoryStore(),
		SystemSettings: settings,
		Cipher:         newTestCipher(t), // capture key present, certificate key absent
	})
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	rotated.ReconcileCertificates(ctx)
	slog.SetDefault(prev)
	out := buf.String()

	// Nothing was minted, so nothing may claim it was.
	if strings.Contains(out, "internal CA created") {
		t.Fatalf("log = %q, want no \"internal CA created\" line -- newCA refused before writing anything", out)
	}
	recovery := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "could not be opened") {
			recovery = line
			break
		}
	}
	if recovery == "" {
		t.Fatalf("log = %q, want a line about the stored internal CA that could not be opened", out)
	}
	if !strings.Contains(recovery, "attempting") {
		t.Fatalf("recovery line = %q, want it to state the INTENT (\"attempting to mint\") -- nothing was minted, and the stored root is unchanged", recovery)
	}

	// The missing-key guidance must still be reached, naming the variable.
	if !strings.Contains(out, "OP_AI_GATEWAY_CERT_ENCRYPTION_KEY") || !strings.Contains(out, "level=WARN") {
		t.Fatalf("log = %q, want a WARN naming OP_AI_GATEWAY_CERT_ENCRYPTION_KEY", out)
	}
	after, err := settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := CertLastError(after); got != certSealKeyMessage {
		t.Fatalf("cert_last_error = %q, want the missing-certificate-key message", got)
	}
	// The stored root really is untouched -- the claim would have been false.
	for _, key := range []string{certCACertKey, certCAKeySealedKey, certCAPrevCertKey} {
		if after[key] != before[key] {
			t.Fatalf("setting %q changed while nothing could be minted", key)
		}
	}
}

// TestReconcileInAcmeModeWithoutTheCertificateKeyRecordsAnActionableError is the
// ACME half of the test above. It matters separately because the self_signed
// abort gate (ensureCA) does NOT exist in the ACME mode: there the refusal
// happens per domain inside issueAndStore, and before the fix the pass therefore
// produced NO module-level note at all (clearCertLastError had already run) --
// only a per-domain row carrying the bare "system.cert_key_required" sentinel,
// which the portal renders raw, plus an invisible Debug line.
//
// The second pass pins the OTHER half: the note must not FLAP. That pass finds
// the domain in its failure backoff, makes no attempt, and so writes no note of
// its own -- an unconditional clear would erase a condition that is still true.
func TestReconcileInAcmeModeWithoutTheCertificateKeyRecordsAnActionableError(t *testing.T) {
	routes := routing.NewMemoryStore()
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{
		Routes:         routes,
		SystemSettings: settings,         // SettingsVolatile:false => disk-store rules
		Cipher:         newTestCipher(t), // capture key present, certificate key absent
		ACMEChallenges: certissue.NewMemoryChallengeStore(),
		Clock:          fixedClock(),
	})
	ctx := context.Background()
	on := true
	mode := IssuerModeACME
	base := "int.example.test"
	scope := "all"
	gw := ""
	// The ACME mode is only "usable" (CertSettings ok) with a contact email, and
	// the directory points at nothing reachable on purpose: the pass must fail at
	// the SEAL, before any network call, so no fake CA is needed here.
	email := "ops@example.test"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:       &on,
		CertIssuerMode:    &mode,
		CertBaseDomain:    &base,
		CertGatewayDomain: &gw,
		CertServerScope:   &scope,
		ACMEEmail:         &email,
	}); err != nil {
		t.Fatalf("enable acme: %v", err)
	}
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")

	// Pass 1 attempts the domain: the ACME account key is sealed with the
	// certificate cipher, so accountFor refuses BEFORE any network call.
	svc.ReconcileCertificates(ctx)

	values, err := settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := CertLastError(values)
	if got != certSealKeyMessage {
		t.Fatalf("acme pass: cert_last_error = %q, want the seal-key message %q (the ACME mode has no ensureCA gate, so this must come from the per-domain refusal)", got, certSealKeyMessage)
	}
	if !strings.Contains(got, "OP_AI_GATEWAY_CERT_ENCRYPTION_KEY") {
		t.Fatalf("cert_last_error = %q, want it to name OP_AI_GATEWAY_CERT_ENCRYPTION_KEY", got)
	}
	if strings.Contains(got, "CAPTURE_ENCRYPTION_KEY") {
		t.Fatalf("cert_last_error = %q, must not point at the capture key", got)
	}
	// The module-level note is the LEGIBLE channel; the row is still recorded as
	// an error (with its backoff) but carries only the raw sentinel.
	cert, err := routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatalf("CertificateByDomain: %v", err)
	}
	if cert.Status != "error" || cert.KeySealed != "" || cert.FullchainPEM != "" {
		t.Fatalf("certificate row = %+v, want status=error with NO material (a key that cannot be sealed must never be stored)", cert)
	}

	// Pass 2: the row is in backoff (fixed clock => NextAttemptAt stays in the
	// future), so no attempt runs and nothing re-writes the note. It must survive.
	svc.ReconcileCertificates(ctx)
	values, err = settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := CertLastError(values); got != certSealKeyMessage {
		t.Fatalf("pass 2 (domain in backoff, no attempt): cert_last_error = %q, want the note to SURVIVE %q -- clearing it makes the alert flap while the cause is unchanged", got, certSealKeyMessage)
	}
}

// TestIssueAndStoreOnlyNotesTheMissingKeyModuleWide pins noteCertKeyRequired's
// discrimination at the exact call site: a missing certificate key is a
// deployment-wide condition and gets promoted to cert_last_error, while any other
// issuer failure stays on its own row (promoting it would make one bad domain
// look like a module-wide outage).
func TestIssueAndStoreOnlyNotesTheMissingKeyModuleWide(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	want := desiredCert{Domain: "a.int.example.test", Kind: "server", ServerID: "srv-a"}

	newEnv := func(t *testing.T, issuerErr error) (*Service, *MemorySystemSettings) {
		t.Helper()
		settings := NewMemorySystemSettings()
		svc := NewService(ServiceDeps{Routes: routing.NewMemoryStore(), SystemSettings: settings})
		svc.cert.issuer = func(context.Context, CertSettings, desiredCert) (certissue.Result, error) {
			return certissue.Result{}, issuerErr
		}
		return svc, settings
	}

	svc, settings := newEnv(t, ErrCertKeyRequired)
	svc.issueAndStore(ctx, CertSettings{IssuerMode: IssuerModeACME}, want, routing.Certificate{}, now, "", nil)
	values, err := settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := CertLastError(values); got != certSealKeyMessage {
		t.Fatalf("cert_last_error = %q, want the seal-key message %q", got, certSealKeyMessage)
	}

	svc, settings = newEnv(t, errors.New("acme: rate limited"))
	svc.issueAndStore(ctx, CertSettings{IssuerMode: IssuerModeACME}, want, routing.Certificate{}, now, "", nil)
	values, err = settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := CertLastError(values); got != "" {
		t.Fatalf("cert_last_error = %q, want it EMPTY for an ordinary per-domain issuer failure", got)
	}
}

// TestAcmeAccountRegistersFreshWhenTheStoredKeyCannotBeOpened pins the rotation
// recovery: an account key sealed under a DIFFERENT certificate key must not
// wedge the ACME mode forever. Before the fix accountFor returned the open
// error, so every later pass failed identically on the same stored bytes and
// recovery required manual DB surgery.
func TestAcmeAccountRegistersFreshWhenTheStoredKeyCannotBeOpened(t *testing.T) {
	ctx := context.Background()

	// The stored account directory MUST equal the one being asked for, otherwise
	// accountFor skips the stored-key branch entirely (directory change => new
	// account) and the test would pass no matter what that branch does. So the
	// fake directory is created first and its URL is what gets stored.
	//
	// It answers 200 with a MALFORMED directory body on purpose: a 5xx would be
	// retried with the client's exponential backoff (see
	// retryBackoffExceptRateLimit) and hang, while an unparseable directory fails
	// on the first attempt. Registration itself is not faked (a JWS-speaking ACME
	// server is out of scope for a unit test) -- the DISCRIMINATOR is which error
	// comes back: pre-fix the decrypt error naming the certificate key, post-fix a
	// directory error, which is only reachable by falling through.
	dirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not-a-directory"))
	}))
	defer dirSrv.Close()
	dir := dirSrv.URL + "/directory"

	// Seal an account key with cipher A...
	settings := NewMemorySystemSettings()
	sealer := NewService(ServiceDeps{SystemSettings: settings, CertCipher: newTestCertCipher(t)})
	key, err := certissue.GenerateAccountKey()
	if err != nil {
		t.Fatal(err)
	}
	pemStr, err := certissue.MarshalECKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealer.sealCertSecret(pemStr)
	if err != nil {
		t.Fatalf("sealCertSecret: %v", err)
	}
	now := time.Now().UTC()
	for _, kv := range [][2]string{
		// dir is ALSO written as the configured global directory: accountFor keys
		// the legacy unprefixed acme_account_* slot to "directory == the configured
		// acme_directory_url" (see acmeAccountKeysFor), so without this the account
		// below would be filed under a hash-suffixed slot the seeded keys never
		// touch.
		{acmeDirectoryURLKey, dir},
		{acmeAccountKeyKey, sealed},
		{acmeAccountDirectoryKey, dir},
		{acmeAccountURIKey, "https://acme.example.test/acct/1"},
	} {
		if err := settings.SetSystemSetting(ctx, kv[0], kv[1], now); err != nil {
			t.Fatal(err)
		}
	}

	// ...then read it with cipher B (the rotated key). The stored bytes cannot be
	// opened, so the call must MOVE ON to minting + registering a fresh account
	// rather than returning the decrypt error. The deadline is belt-and-braces in
	// case the client's retry policy ever widens to cover this response too.
	regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rotated := NewService(ServiceDeps{SystemSettings: settings, CertCipher: newTestCipher(t)})
	_, _, err = rotated.accountFor(regCtx, dir, "")
	if err == nil {
		t.Fatal("accountFor succeeded against a failing directory; the test cannot discriminate")
	}
	if strings.Contains(err.Error(), "OP_AI_GATEWAY_CERT_ENCRYPTION_KEY") {
		t.Fatalf("accountFor returned the decrypt error %v -- an unopenable stored account key must be treated as ABSENT and a fresh account registered, otherwise a certificate-key rotation wedges the ACME mode permanently", err)
	}
	// Nothing was persisted on that failed path: the stored (unopenable) key is
	// untouched, so there is no half-written account referencing a key that never
	// registered.
	values, err := settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if values[acmeAccountKeyKey] != sealed {
		t.Fatal("the stored account key changed even though registration failed; the key must be persisted only after a successful Register")
	}

	// The same fall-through with the ORIGINAL cipher still short-circuits: a
	// readable stored key is reused, never re-registered.
	if got, uri, err := sealer.accountFor(ctx, dir, ""); err != nil || got == nil || !got.PublicKey.Equal(&key.PublicKey) || uri != "https://acme.example.test/acct/1" {
		t.Fatalf("accountFor with the correct cipher = (%v, %q, %v), want the STORED account reused", got, uri, err)
	}
}
