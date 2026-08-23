// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/certissue"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// selfSigned builds a leaf for the stub issuer with the given lifetime.
func selfSigned(t *testing.T, domain string, notBefore time.Time, lifetime time.Duration) certissue.Result {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
		NotBefore:    notBefore,
		NotAfter:     notBefore.Add(lifetime),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := certissue.MarshalECKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(leaf.Raw)
	return certissue.Result{
		FullchainPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		KeyPEM:       keyPEM,
		Fingerprint:  hex.EncodeToString(sum[:]),
		Leaf:         leaf,
	}
}

// certEnv wires a Service with a memory routing store + a VOLATILE settings
// store (so sealCertSecret takes the "plain:" branch and no encryption key is
// needed) and a stub issuer slot. It deliberately keeps the DEFAULT (real)
// clock: every test below builds its certificate lifetimes from
// time.Now().UTC(), and the renewal-window / backoff assertions only hold when
// the service reads the same wall clock.
func certEnv(t *testing.T) (*Service, context.Context) {
	t.Helper()
	routes := routing.NewMemoryStore()
	svc := NewService(ServiceDeps{
		Routes:           routes,
		SystemSettings:   NewMemorySystemSettings(),
		SettingsVolatile: true,
		ACMEChallenges:   certissue.NewMemoryChallengeStore(),
	})
	return svc, context.Background()
}

// fakeAccountDir is a minimal in-process ACME v2 directory that answers only
// enough of the protocol for certissue.ACMEClient.Register: /directory,
// /new-nonce, and /new-account. accountFor never places an order (that is
// issueCertificate's job, via a SEPARATE ACMEClient built with the resulting
// AccountURI), so unlike certissue's own fakeDir (internal/certissue/fakedir_
// test.go) this needs no CA, no challenge wiring, and no /new-order/finalize
// handling at all.
type fakeAccountDir struct {
	srv *httptest.Server
}

func newFakeAccountDir(t *testing.T) *fakeAccountDir {
	t.Helper()
	d := &fakeAccountDir{}
	d.srv = httptest.NewServer(http.HandlerFunc(d.handle))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeAccountDir) handle(w http.ResponseWriter, r *http.Request) {
	// Every response carries a fresh nonce, mirroring fakeDir in
	// internal/certissue/fakedir_test.go -- the client consumes one per POST.
	w.Header().Set("Replay-Nonce", fmt.Sprintf("nonce-%d", time.Now().UnixNano()))
	switch r.URL.Path {
	case "/directory":
		writeFakeACMEJSON(w, 200, map[string]any{
			"newNonce":   d.srv.URL + "/new-nonce",
			"newAccount": d.srv.URL + "/new-account",
			"newOrder":   d.srv.URL + "/new-order",
		})
	case "/new-nonce":
		w.WriteHeader(200)
	case "/new-account":
		// Always the same account URI: within one test, accountFor's own
		// directory-keyed cache is what prevents a second /new-account call for
		// the SAME directory (see TestAccountForSharesOneAccountPerDirectory), so
		// this fake server is never asked to register two distinct accounts of
		// its own.
		w.Header().Set("Location", d.srv.URL+"/acct/1")
		writeFakeACMEJSON(w, 201, map[string]any{"status": "valid"})
	default:
		http.NotFound(w, r)
	}
}

func writeFakeACMEJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// fakeACMEDirsMu/fakeACMEDirs back fakeACMEDir/fakeACMEDir2: each is a fake
// directory server scoped to (and memoized per) the *testing.T that first asks
// for it, so repeated calls within the SAME test return the identical URL
// instead of spinning up a fresh listener (and therefore a "new" directory)
// every time -- which is exactly what TestAccountForSharesOneAccountPerDirectory
// relies on to prove that calling accountFor twice with "the same directory"
// really does mean the same directory.
var (
	fakeACMEDirsMu sync.Mutex
	fakeACMEDirs   = map[*testing.T][2]string{}
)

// fakeACMEDir returns the directory URL of an in-process fake ACME server
// scoped to t (see fakeACMEDirsMu above).
func fakeACMEDir(t *testing.T) string {
	t.Helper()
	return fakeACMEDirSlot(t, 0)
}

// fakeACMEDir2 is a SECOND fake ACME directory, on its own listener, scoped to
// t exactly like fakeACMEDir -- for tests asserting that two distinct
// directories get two distinct accounts.
func fakeACMEDir2(t *testing.T) string {
	t.Helper()
	return fakeACMEDirSlot(t, 1)
}

func fakeACMEDirSlot(t *testing.T, slot int) string {
	t.Helper()
	fakeACMEDirsMu.Lock()
	defer fakeACMEDirsMu.Unlock()
	if urls, ok := fakeACMEDirs[t]; ok && urls[slot] != "" {
		return urls[slot]
	}
	d := newFakeAccountDir(t)
	urls := fakeACMEDirs[t]
	// d.srv.URL is "http://127.0.0.1:PORT" (httptest.NewServer's default IPv4
	// loopback listener). Rewritten to "localhost" -- which still resolves to
	// the SAME loopback address, so the connection is unaffected -- so this
	// directory URL passes the *_acme_directory_url bare-IP-host rejection
	// (net.ParseIP("localhost") is nil) when a test writes it through
	// UpdateSystemSettings with an acme mode, exactly like any real ACME
	// directory would.
	urls[slot] = strings.Replace(d.srv.URL, "127.0.0.1", "localhost", 1) + "/directory"
	fakeACMEDirs[t] = urls
	t.Cleanup(func() {
		fakeACMEDirsMu.Lock()
		delete(fakeACMEDirs, t)
		fakeACMEDirsMu.Unlock()
	})
	return urls[slot]
}

// seededLegacyURI is the account URI seedLegacyAcmeAccount writes into the
// pre-upgrade global acme_account_uri slot.
const seededLegacyURI = "https://fake-acme.example.test/acct/legacy-1"

// seedLegacyAcmeAccount reproduces exactly the state a PRE-unification gateway
// leaves behind: acme_directory_url configured to dir, and the legacy
// unprefixed acme_account_{key,uri,directory} triple already registered
// against it. It writes directly through the settings store (bypassing
// accountFor/Register entirely), so a test using it can assert that accountFor
// ADOPTS this material verbatim instead of registering a fresh account.
func seedLegacyAcmeAccount(t *testing.T, svc *Service, ctx context.Context, dir string) {
	t.Helper()
	key, err := certissue.GenerateAccountKey()
	if err != nil {
		t.Fatalf("seed legacy account: generate key: %v", err)
	}
	pemStr, err := certissue.MarshalECKeyPEM(key)
	if err != nil {
		t.Fatalf("seed legacy account: marshal key: %v", err)
	}
	sealed, err := svc.sealCertSecret(pemStr)
	if err != nil {
		t.Fatalf("seed legacy account: seal key: %v", err)
	}
	now := time.Now().UTC()
	for _, kv := range [][2]string{
		{acmeDirectoryURLKey, dir},
		{acmeAccountURIKey, seededLegacyURI},
		{acmeAccountDirectoryKey, dir},
		{acmeAccountKeyKey, sealed},
	} {
		if err := svc.settings.SetSystemSetting(ctx, kv[0], kv[1], now); err != nil {
			t.Fatalf("seed legacy account %s: %v", kv[0], err)
		}
	}
}

// TestAccountForSharesOneAccountPerDirectory pins the whole point of the
// directory-keyed refactor: two calls against the SAME directory must reuse one
// registered account (no second /new-account call, same key and URI back), and
// a call against a DIFFERENT directory must get its own distinct account.
func TestAccountForSharesOneAccountPerDirectory(t *testing.T) {
	svc, ctx := certEnv(t)
	// Two calls, same directory: exactly one registration, same key+uri back.
	k1, u1, err := svc.accountFor(ctx, fakeACMEDir(t), "a@example.test")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	k2, u2, err := svc.accountFor(ctx, fakeACMEDir(t), "a@example.test")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if u1 != u2 || !k1.Equal(k2) {
		t.Fatal("same directory must reuse the same account (no re-registration)")
	}
	// A different directory registers a distinct account.
	k3, u3, err := svc.accountFor(ctx, fakeACMEDir2(t), "a@example.test")
	if err != nil {
		t.Fatalf("third: %v", err)
	}
	if u3 == u1 && k3.Equal(k1) {
		t.Fatal("a different directory must get a distinct account")
	}
}

// TestAccountForAdoptsLegacyGlobalSlot pins the byte-neutral upgrade: a
// pre-unification gateway's already-registered account, sitting in the legacy
// unprefixed acme_account_* slot, must be adopted verbatim for the GLOBAL
// directory context -- no re-registration.
func TestAccountForAdoptsLegacyGlobalSlot(t *testing.T) {
	svc, ctx := certEnv(t)
	dir := fakeACMEDir(t)
	// Seed the OLD single-slot keys as if written by a pre-upgrade gateway.
	seedLegacyAcmeAccount(t, svc, ctx, dir)
	k, u, err := svc.accountFor(ctx, dir, "a@example.test")
	if err != nil {
		t.Fatalf("accountFor: %v", err)
	}
	// No new registration: the returned uri equals the seeded legacy uri.
	if u != seededLegacyURI {
		t.Fatalf("legacy global slot not adopted: uri=%q want %q", u, seededLegacyURI)
	}
	_ = k
}

// TestPublicIssuerModeFollowsGlobalUntilSet proves the byte-neutral default: a
// public domain's effective issuer mode tracks the global IssuerMode as long
// as PublicIssuerMode is unset, and an explicit PublicIssuerMode overrides it.
func TestPublicIssuerModeFollowsGlobalUntilSet(t *testing.T) {
	set := CertSettings{IssuerMode: IssuerModeSelfSigned} // PublicIssuerMode empty
	if got := set.modeFor("public"); got != IssuerModeSelfSigned {
		t.Fatalf("empty public mode must follow global: got %q", got)
	}
	set.PublicIssuerMode = IssuerModeACME
	if got := set.modeFor("public"); got != IssuerModeACME {
		t.Fatalf("explicit public mode must override: got %q", got)
	}
}

// TestCertAcmeConfigForSharedVsOwn proves certAcmeConfigFor's per-context
// resolution: a context whose *ACMEShared is false resolves its OWN
// directory/email, one left shared (the default) resolves the GLOBAL pair.
func TestCertAcmeConfigForSharedVsOwn(t *testing.T) {
	set := CertSettings{
		Email: "global@example.test", DirectoryURL: "https://global.example/dir",
		PublicACMEShared: false, PublicACMEEmail: "pub@example.test", PublicACMEDirectoryURL: "https://pub.example/dir",
		EdgeACMEShared: true,
	}
	dir, email, _ := set.certAcmeConfigFor("public")
	if dir != "https://pub.example/dir" || email != "pub@example.test" {
		t.Fatalf("public own = (%q,%q), want its own", dir, email)
	}
	edir, eemail, _ := set.certAcmeConfigFor("edge")
	if edir != "https://global.example/dir" || eemail != "global@example.test" {
		t.Fatalf("edge shared = (%q,%q), want the global", edir, eemail)
	}
}

// TestCertAcmeConfigForWeeklyLimit proves the third return value follows the
// exact same shared/own switch as the directory/email pair -- a context left
// shared reports the GLOBAL ceiling, an own context reports its own.
func TestCertAcmeConfigForWeeklyLimit(t *testing.T) {
	set := CertSettings{
		ACMEWeeklyLimit:       10,
		EdgeACMEShared:        false,
		EdgeACMEWeeklyLimit:   3,
		PublicACMEShared:      true,
		PublicACMEWeeklyLimit: 99, // ignored: public is shared
	}
	if _, _, limit := set.certAcmeConfigFor("edge"); limit != 3 {
		t.Fatalf("edge (own) weekly limit = %d, want 3", limit)
	}
	if _, _, limit := set.certAcmeConfigFor("public"); limit != 10 {
		t.Fatalf("public (shared) weekly limit = %d, want the global 10", limit)
	}
}

// TestIssueCertificateResolvesPerContextACMEAccount is issueCertificate's
// wiring test for certAcmeConfigFor (Step 8): a "public" row configured with
// its OWN (non-shared) ACME directory must register an account against THAT
// directory, not the global one. The fake directory here only answers enough
// of the ACME protocol for accountFor's registration (see fakeAccountDir); the
// subsequent order placement (client.Obtain) is expected to fail against it,
// which is fine -- this test only cares which account got registered before
// that failure.
func TestIssueCertificateResolvesPerContextACMEAccount(t *testing.T) {
	svc, ctx := certEnv(t)
	globalDir := fakeACMEDir(t)
	publicDir := fakeACMEDir2(t)
	set := CertSettings{
		IssuerMode:   IssuerModeACME,
		Email:        "global@example.test",
		DirectoryURL: globalDir,

		PublicACMEShared:       false,
		PublicACMEEmail:        "pub@example.test",
		PublicACMEDirectoryURL: publicDir,
	}
	want := desiredCert{Domain: "pub.example.test", Kind: "public"}
	// Obtain necessarily errors (the fake directory has no order/finalize
	// handling) -- only accountFor's side effect is under test here.
	_, _ = svc.issueCertificate(ctx, set, want)

	values, err := svc.settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _, publicDirK := acmeAccountKeysFor(publicDir, globalDir)
	if values[publicDirK] != publicDir {
		t.Fatalf("account registered against directory %q, want the public context's own %q", values[publicDirK], publicDir)
	}
	if values[acmeAccountDirectoryKey] != "" {
		t.Fatalf("issuing for a non-shared public context must not touch the GLOBAL (legacy-slot) account, got %q", values[acmeAccountDirectoryKey])
	}
}

// enableCertModuleWithoutGlobalACME turns the certificate module on with the
// internal/global mode left at its UNCONFIGURED default (cert_issuer_mode
// unset -> acme, acme_email unset -> "") -- CertSettings.ok is false, exactly
// the scenario review round-1's servability-gate fix is about: an operator
// who never touched the internal ACME config at all, but wants a public
// domain (or the edge) served independently. cert_base_domain IS set --
// unlike the fix under test, the SEPARATE "no base domain configured" abort
// gate (ReconcileCertificates, `if base == "" && !edgeWanted`) was not part of
// this round's findings and public/edge rows are exempt from the base-domain
// rule regardless (want.Kind != "public" && want.Kind != certEdgeKind), so a
// missing base domain would only obscure this test's actual subject.
func enableCertModuleWithoutGlobalACME(t *testing.T, svc *Service, ctx context.Context) {
	t.Helper()
	on := true
	base := "int.example.test"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:    &on,
		CertBaseDomain: &base,
	}); err != nil {
		t.Fatalf("enable cert module without global ACME: %v", err)
	}
}

// TestReconcilePublicSelfSignedIssuesWithoutGlobalACME is review round-1's
// scenario (a): the global/internal ACME is left unconfigured (the default),
// cert_manage_public_domain is on with one domain set to
// cert_public_issuer_mode=self_signed. Before the fix, ReconcileCertificates'
// servability gate only ever considered the internal mode and the edge row --
// internalServable=false and edgeWanted=false made it abort with "no usable
// issuer" before desiredCertificates was even called a second time, so this
// fully-configured public domain was NEVER attempted. This test FAILS before
// the fix (no certificate row is ever created for the domain) and PASSES
// after (an active, self-signed certificate is issued).
func TestReconcilePublicSelfSignedIssuesWithoutGlobalACME(t *testing.T) {
	svc, ctx := certEnv(t)
	enableCertModuleWithoutGlobalACME(t, svc, ctx)
	manage := true
	domains := []string{"pub.example.net"}
	publicMode := IssuerModeSelfSigned
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertManagePublicDomain: &manage,
		CertPublicDomains:      &domains,
		CertPublicIssuerMode:   &publicMode,
	}); err != nil {
		t.Fatalf("configure public domain: %v", err)
	}
	// Sanity: the internal/global mode is genuinely NOT servable this pass --
	// otherwise this test would prove nothing about the fix under test.
	if set, ok, err := svc.CertSettings(ctx); err != nil || ok {
		t.Fatalf("internal settings unexpectedly usable: ok=%v err=%v set=%+v", ok, err, set)
	}
	svc.cert.issuer = svc.issueCertificate // the real self-signed path

	svc.ReconcileCertificates(ctx)

	got, err := svc.routes.CertificateByDomain(ctx, "pub.example.net")
	if err != nil {
		t.Fatalf("the public domain was never issued: %v", err)
	}
	if got.Status != "active" || got.Kind != "public" {
		t.Fatalf("public cert = %+v (last_error %q), want an active public certificate", got, got.LastError)
	}
}

// TestReconcilePublicOwnACMEAttemptsIssuanceAgainstItsOwnDirectory is review
// round-1's scenario (b): same unconfigured global ACME as (a), but the
// public domain instead uses cert_public_issuer_mode=acme with
// cert_public_acme_shared=false and its own email/directory. Before the fix
// this aborted identically to (a) (still "no usable issuer": publicWanted/
// publicModeUsable did not exist at all). The fake directory here only
// answers enough of the ACME protocol for account REGISTRATION (see
// fakeAccountDir) -- actual order placement necessarily fails, which is fine:
// issueAndStore's failure path still upserts a certificate row (Status
// "error", AttemptCount >= 1), so a row's mere EXISTENCE already proves an
// issuance attempt was actually made instead of the pass aborting early. The
// account-registration side effect additionally proves it was made against
// the PUBLIC's OWN directory, not the (unconfigured) global one -- so this
// test also fails before the fix (no row, no per-directory account) and
// passes after.
func TestReconcilePublicOwnACMEAttemptsIssuanceAgainstItsOwnDirectory(t *testing.T) {
	svc, ctx := certEnv(t)
	enableCertModuleWithoutGlobalACME(t, svc, ctx)
	manage := true
	domains := []string{"pub.example.net"}
	publicMode := IssuerModeACME
	shared := false
	email := "pub@example.test"
	dir := fakeACMEDir2(t)
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertManagePublicDomain:     &manage,
		CertPublicDomains:          &domains,
		CertPublicIssuerMode:       &publicMode,
		CertPublicACMEShared:       &shared,
		CertPublicACMEEmail:        &email,
		CertPublicACMEDirectoryURL: &dir,
	}); err != nil {
		t.Fatalf("configure public domain: %v", err)
	}
	if set, ok, err := svc.CertSettings(ctx); err != nil || ok {
		t.Fatalf("internal settings unexpectedly usable: ok=%v err=%v set=%+v", ok, err, set)
	}
	svc.cert.issuer = svc.issueCertificate // the real ACME path

	svc.ReconcileCertificates(ctx)

	got, err := svc.routes.CertificateByDomain(ctx, "pub.example.net")
	if err != nil {
		t.Fatalf("the public domain was never attempted: %v", err)
	}
	if got.Kind != "public" || got.AttemptCount < 1 {
		t.Fatalf("public cert = %+v, want at least one recorded issuance attempt", got)
	}
	values, err := svc.settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _, dirK := acmeAccountKeysFor(dir, ACMEDirectoryURL(values))
	if values[dirK] != dir {
		t.Fatalf("account registered against %q, want the public context's own directory %q", values[dirK], dir)
	}
}

// TestReconcileEdgeOwnACMEAttemptsIssuanceAgainstItsOwnDirectory is review
// round-1's scenario (c): the edge certificate configured with its OWN
// (non-shared) ACME account while the global acme_email is blank. Before the
// fix, edgeModeUsable checked the GLOBAL set.Email instead of the edge's
// EFFECTIVE email (certAcmeConfigFor(certEdgeKind)) -- so even though the edge
// row had a fully valid account of its own, edgeModeUsable(set) still returned
// false, the whole pass aborted with "no usable issuer", and the edge
// certificate was never attempted. Same reasoning as (b) for why a
// mere-existence-plus-right-directory assertion is the right bar here: the
// fake directory cannot complete a full order.
func TestReconcileEdgeOwnACMEAttemptsIssuanceAgainstItsOwnDirectory(t *testing.T) {
	svc, ctx := certEnv(t)
	enableCertModuleWithoutGlobalACME(t, svc, ctx)
	on := true
	edgeMode := IssuerModeACME
	names := []string{"edge.example.test"}
	shared := false
	email := "edge@example.test"
	dir := fakeACMEDir2(t)
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEdgeEnabled:          &on,
		CertEdgeIssuerMode:       &edgeMode,
		CertEdgeNames:            &names,
		CertEdgeACMEShared:       &shared,
		CertEdgeACMEEmail:        &email,
		CertEdgeACMEDirectoryURL: &dir,
	}); err != nil {
		t.Fatalf("configure edge: %v", err)
	}
	if set, ok, err := svc.CertSettings(ctx); err != nil || ok {
		t.Fatalf("internal settings unexpectedly usable: ok=%v err=%v set=%+v", ok, err, set)
	}
	svc.cert.issuer = svc.issueCertificate // the real ACME path

	svc.ReconcileCertificates(ctx)

	got, err := svc.routes.CertificateByDomain(ctx, "edge.example.test")
	if err != nil {
		t.Fatalf("the edge certificate was never attempted: %v", err)
	}
	if got.Kind != certEdgeKind || got.AttemptCount < 1 {
		t.Fatalf("edge cert = %+v, want at least one recorded issuance attempt", got)
	}
	values, err := svc.settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _, dirK := acmeAccountKeysFor(dir, ACMEDirectoryURL(values))
	if values[dirK] != dir {
		t.Fatalf("account registered against %q, want the edge context's own directory %q", values[dirK], dir)
	}
}

// TestReconcilePublicSelfSignedIssuesWithoutABaseDomain is review round-2's
// fix: a SECOND, separate abort gate in ReconcileCertificates --
// `if base == "" && !edgeWanted { return }`, a few lines below the
// servability gate round-1 fixed -- had the exact same missing-independence
// bug the review already flagged once. Public domains are standalone FQDNs
// the operator already typed out in full; they are not derived from
// cert_base_domain, and the under-base ACME rule a few dozen lines later
// already exempts want.Kind == "public" outright. So a deployment with
// cert_manage_public_domain on and a public domain in a usable mode
// (self_signed here), but NO cert_base_domain configured and no NetBird
// account to derive one from, and no edge row either, still hit this second
// gate and aborted -- even though round-1's fix already let it past the
// FIRST (servability) gate.
//
// Deliberately does NOT call enableCertModuleWithoutGlobalACME (round-1's
// helper): that helper sets cert_base_domain specifically to sidestep THIS
// gate, which is exactly the one under test here.
func TestReconcilePublicSelfSignedIssuesWithoutABaseDomain(t *testing.T) {
	svc, ctx := certEnv(t)
	on := true
	manage := true
	domains := []string{"pub.example.net"}
	publicMode := IssuerModeSelfSigned
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:            &on,
		CertManagePublicDomain: &manage,
		CertPublicDomains:      &domains,
		CertPublicIssuerMode:   &publicMode,
	}); err != nil {
		t.Fatalf("configure public domain: %v", err)
	}
	// Sanity: cert_base_domain is genuinely empty, and there is no NetBird
	// account configured to derive one from either -- otherwise this test
	// would prove nothing about the gate under test.
	set, _, err := svc.CertSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if set.BaseDomain != "" {
		t.Fatalf("BaseDomain = %q, want empty", set.BaseDomain)
	}
	svc.cert.issuer = svc.issueCertificate // the real self-signed path

	svc.ReconcileCertificates(ctx)

	got, err := svc.routes.CertificateByDomain(ctx, "pub.example.net")
	if err != nil {
		t.Fatalf("the public domain was never issued: %v", err)
	}
	if got.Status != "active" || got.Kind != "public" {
		t.Fatalf("public cert = %+v (last_error %q), want an active public certificate", got, got.LastError)
	}
}

func TestSuccessfulNewCABroadcastsInAllThreeDrivers(t *testing.T) {
	settings := NewMemorySystemSettings()
	var fingerprints []string
	svc := NewService(ServiceDeps{
		Routes:            routing.NewMemoryStore(),
		SystemSettings:    settings,
		SettingsVolatile:  true,
		OnCABundleChanged: func(fingerprint string) { fingerprints = append(fingerprints, fingerprint) },
	})
	now := time.Now().UTC()
	first, err := svc.newCA(context.Background(), "int.example.test", now, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(fingerprints) != 1 || fingerprints[0] != first.Fingerprint() {
		t.Fatalf("callbacks=%v, want first root %q", fingerprints, first.Fingerprint())
	}
	values, err := settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.newCA(context.Background(), "int.example.test", now.Add(time.Hour), values[certCACertKey])
	if err != nil {
		t.Fatal(err)
	}
	if len(fingerprints) != 2 || fingerprints[1] != second.Fingerprint() {
		t.Fatalf("callbacks=%v, want rotation root %q", fingerprints, second.Fingerprint())
	}
}

func TestAgentRegistryRetainsTrustOnlyReportAndStillShowsNotInstalled(t *testing.T) {
	routes := routing.NewMemoryStore()
	now := time.Now().UTC()
	if err := routes.CreateAIServer(context.Background(), routing.AIServer{
		ID: "s1", Name: "Server 1", Domain: "server.example.test", Status: routing.ServerStatusActive,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := routes.UpsertCertificate(context.Background(), routing.Certificate{
		Domain: "server.example.test", Kind: "server", ServerID: "s1", Status: "active",
		Fingerprint: fp("a"), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	reports := &fakeCertReports{byServer: map[string]fakeCertReport{
		"s1": {caFPs: []string{fp("b")}, mode: "off", reportedAt: now},
	}}
	svc := NewService(ServiceDeps{Routes: routes, AgentCertReports: reports})
	view, err := svc.CertificatesView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(view) != 1 {
		t.Fatalf("view=%+v", view)
	}
	if view[0].Installed || view[0].InstalledFingerprint != "" || view[0].InstalledMode != "off" || view[0].InstalledAt == nil {
		t.Fatalf("root-only report must remain readable without claiming a leaf install: %+v", view[0])
	}
}

// enableACME turns the module on with a usable configuration and the given scope.
func enableACME(t *testing.T, svc *Service, ctx context.Context, scope string) {
	t.Helper()
	on := true
	mode := IssuerModeACME
	email := "ops@example.test"
	base := "int.example.test"
	gw := "" // no gateway peer in this env -> no gateway certificate is desired
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:       &on,
		CertIssuerMode:    &mode,
		ACMEEmail:         &email,
		CertBaseDomain:    &base,
		CertGatewayDomain: &gw,
		CertServerScope:   &scope,
	}); err != nil {
		t.Fatalf("enable acme: %v", err)
	}
}

// enableSelfSigned turns the module on in the internal-CA mode with the given
// server scope and leaf validity (days).
func enableSelfSigned(t *testing.T, svc *Service, ctx context.Context, scope string, validityDays int) {
	t.Helper()
	on := true
	mode := IssuerModeSelfSigned
	base := "int.example.test"
	gw := ""
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:                &on,
		CertIssuerMode:             &mode,
		CertBaseDomain:             &base,
		CertGatewayDomain:          &gw,
		CertServerScope:            &scope,
		CertSelfSignedValidityDays: &validityDays,
	}); err != nil {
		t.Fatalf("enable self-signed: %v", err)
	}
}

// mustCreateNetbirdServer inserts a NetBird-enabled server with the given domain
// and per-server certificate override directly through the store, AND mints it a
// reporting-agent token.
//
// The token is not incidental: since Phase 2 a kind=server name only enters the
// desired set when the server has one (there is otherwise no distribution path --
// see serverHasAgentToken), so every certificate expectation in this file depends
// on it. A server deliberately WITHOUT a token is built by
// mustCreateNetbirdServerWithoutAgentToken below.
func mustCreateNetbirdServer(t *testing.T, svc *Service, ctx context.Context, id, domain, override string) {
	t.Helper()
	mustCreateNetbirdServerWithoutAgentToken(t, svc, ctx, id, domain, override)
	now := time.Now().UTC()
	if err := svc.routes.UpsertAgentToken(ctx, routing.AgentToken{
		ID: "agt-" + id, ServerID: id, SecretPrefix: "e2e", CreatedAt: now, UpdatedAt: now,
	}, "hash-"+id); err != nil {
		t.Fatalf("mint agent token for %s: %v", id, err)
	}
}

// mustCreateNetbirdServerWithoutAgentToken is the same insert WITHOUT the agent
// token, i.e. a server that has no ServerAgent and therefore no way to receive a
// certificate.
func mustCreateNetbirdServerWithoutAgentToken(t *testing.T, svc *Service, ctx context.Context, id, domain, override string) {
	t.Helper()
	now := time.Now().UTC()
	if err := svc.routes.CreateAIServer(ctx, routing.AIServer{
		ID: id, Name: id, Domain: domain, Provider: "vllm", Status: "active",
		NetbirdEnabled: true, CertificateOverride: override,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create server %s: %v", id, err)
	}
}

func mustMarshalJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// certStoreSpy satisfies routing.Store but fails the test on every call the
// certificate reconcile could make. The embedded nil interface means a method this
// spy does NOT override panics rather than silently succeeding, so the spy stays
// honest even if the reconcile starts touching something new.
type certStoreSpy struct {
	routing.Store
	t *testing.T
}

func (s certStoreSpy) Certificates(context.Context) ([]routing.Certificate, error) {
	s.t.Fatal("reconcile listed certificates while the module was disabled")
	return nil, nil
}

func (s certStoreSpy) CertificateByDomain(_ context.Context, domain string) (routing.Certificate, error) {
	s.t.Fatalf("reconcile read certificate %q while the module was disabled", domain)
	return routing.Certificate{}, nil
}

func (s certStoreSpy) UpsertCertificate(_ context.Context, cert routing.Certificate) error {
	s.t.Fatalf("reconcile wrote certificate %q while the module was disabled", cert.Domain)
	return nil
}

func (s certStoreSpy) DeleteCertificate(_ context.Context, domain string) error {
	s.t.Fatalf("reconcile deleted certificate %q while the module was disabled", domain)
	return nil
}

func (s certStoreSpy) AIServers(context.Context) ([]routing.AIServer, error) {
	s.t.Fatal("reconcile enumerated servers while the module was disabled")
	return nil, nil
}

// netbirdWorkingDomain configures the NetBird module against a stub account API that
// reports a dns_domain, so cert_base_domain's live fallback (netbirdDNSDomain)
// actually resolves. No gateway peer is selected, so the gateway NAME still resolves
// to ("", nil) and no gateway certificate is desired.
func netbirdWorkingDomain(t *testing.T, svc *Service, ctx context.Context) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/accounts" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"acct-1","settings":{"dns_domain":"mesh.example.test"}}]`))
	}))
	t.Cleanup(srv.Close)
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		NetbirdEnabled: boolPtr(true),
		NetbirdURL:     strPtr(srv.URL),
		NetbirdToken:   strPtr("tok"),
	}); err != nil {
		t.Fatalf("configure netbird: %v", err)
	}
	if got := svc.netbirdDNSDomain(ctx); got != "mesh.example.test" {
		t.Fatalf("precondition: netbirdDNSDomain = %q, want the stub domain", got)
	}
}

func TestBackoffForLadderAndUrgentCap(t *testing.T) {
	const noCert = time.Duration(0) // no stored certificate -> nothing to run out
	for _, tc := range []struct {
		name      string
		attempts  int
		remaining time.Duration
		want      time.Duration
	}{
		{"first failure", 1, noCert, 5 * time.Minute},
		{"second failure", 2, noCert, time.Hour},
		{"third failure", 3, noCert, 6 * time.Hour},
		{"fourth failure", 4, noCert, 24 * time.Hour},
		{"ladder saturates", 12, noCert, 24 * time.Hour},
		{"attempts below one clamps to the first step", 0, noCert, 5 * time.Minute},
		// Plenty of validity left -> the full ladder applies.
		{"comfortable validity keeps 24h", 4, 30 * 24 * time.Hour, 24 * time.Hour},
		// Under 7 days remaining the delay is capped so a stubborn error cannot sit
		// in a 24h backoff while the certificate expires.
		{"urgent caps 24h at 15m", 4, 6 * 24 * time.Hour, 15 * time.Minute},
		{"urgent caps 6h at 15m", 3, time.Hour, 15 * time.Minute},
		// The cap never LENGTHENS a delay that is already shorter.
		{"urgent leaves 5m alone", 1, time.Hour, 5 * time.Minute},
		// Exactly at the threshold is not yet urgent (strict <).
		{"exactly 7 days is not urgent", 4, certUrgentRemaining, 24 * time.Hour},
		// An already-expired certificate has remaining <= 0, which is the "unknown /
		// nothing to protect" case, not an urgent one.
		{"expired certificate uses the ladder", 4, -time.Hour, 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := backoffFor(tc.attempts, tc.remaining); got != tc.want {
				t.Fatalf("backoffFor(%d, %v) = %v, want %v", tc.attempts, tc.remaining, got, tc.want)
			}
		})
	}
}

func TestJitterIsDeterministicNonNegativeAndBounded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window time.Duration
	}{
		{"30d window", 30 * 24 * time.Hour},
		{"3d window", 3 * 24 * time.Hour},
		{"2d window (no room)", 2 * 24 * time.Hour},
		{"1h window (no room)", time.Hour},
		{"zero window", 0},
		{"negative window", -time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, domain := range []string{"a.int.example.test", "b.int.example.test", "", "x"} {
				got := jitter(domain, tc.window)
				if got < 0 {
					t.Fatalf("jitter(%q, %v) = %v, must never be negative (it would SHORTEN the safety window)", domain, tc.window, got)
				}
				if tc.window > 0 && got >= tc.window/3+24*time.Hour {
					t.Fatalf("jitter(%q, %v) = %v, want well under a third of the window", domain, tc.window, got)
				}
				// Deterministic: the same domain always yields the same offset, so a
				// certificate does not drift its renewal date between passes.
				if again := jitter(domain, tc.window); again != got {
					t.Fatalf("jitter(%q) = %v then %v, want a stable per-domain value", domain, got, again)
				}
			}
			if tc.window < 3*24*time.Hour {
				if got := jitter("a.int.example.test", tc.window); got != 0 {
					t.Fatalf("a window with no room must yield no jitter, got %v", got)
				}
			}
		})
	}
	// Different domains spread out (the whole point: de-synchronize a cohort).
	window := 30 * 24 * time.Hour
	seen := map[time.Duration]bool{}
	for _, d := range []string{"a.example.test", "b.example.test", "c.example.test", "d.example.test", "e.example.test"} {
		seen[jitter(d, window)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("jitter collapsed every domain onto %v; a cohort would renew on the same day", seen)
	}
}

func TestJitterOnlyEverRenewsEarlier(t *testing.T) {
	// The documented guarantee: the jitter ADDS to the renewal window, so a
	// certificate becomes due no LATER than it would without jitter. A subtraction
	// would silently shrink the safety margin.
	now := time.Now().UTC()
	for _, remaining := range []time.Duration{
		45 * 24 * time.Hour, 40 * 24 * time.Hour, 35 * 24 * time.Hour,
		31 * 24 * time.Hour, 30 * 24 * time.Hour, 25 * 24 * time.Hour, 5 * 24 * time.Hour,
	} {
		for _, domain := range []string{"a.example.test", "b.example.test", "zz.example.test"} {
			cert := routing.Certificate{
				Status:    "active",
				NotBefore: now.Add(remaining - 90*24*time.Hour),
				NotAfter:  now.Add(remaining),
			}
			// Baseline: the same threshold with the jitter forced to zero.
			window := 30 * 24 * time.Hour
			if lifetime := cert.NotAfter.Sub(cert.NotBefore); lifetime > 0 {
				if third := lifetime / 3; third < window {
					window = third
				}
			}
			plain := now.Add(window).After(cert.NotAfter)
			withJitter := renewDue(cert, 30, now, domain, "")
			if plain && !withJitter {
				t.Fatalf("remaining=%v domain=%q: jitter DELAYED the renewal (plain due, jittered not due)", remaining, domain)
			}
		}
	}
}

func TestReconcileCertificatesNoopWhenDisabled(t *testing.T) {
	svc, ctx := certEnv(t)
	// The module is configured FULLY (self_signed, base domain, scope, a managed
	// server) and then switched off. The "on" half runs first so the configuration
	// is proven live: the second pass can then only be stopped by the cert_enabled
	// gate itself, which is what this test pins.
	//
	// NetBird is configured too, and deliberately so: cert_base_domain has a live
	// fallback (netbirdDNSDomain). Without NetBird, a disabled pass would stop at the
	// empty-base-domain check even with the cert_enabled gate removed, and this test
	// would pass for the wrong reason. With it, the gate is the ONLY thing standing
	// between a disabled module and the store.
	netbirdWorkingDomain(t, svc, ctx)
	enableSelfSigned(t, svc, ctx, "all", 90)
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	now := time.Now().UTC()
	var calls int
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		calls++
		return selfSigned(t, want.Domain, now.Add(-time.Hour), 90*24*time.Hour), nil
	}
	svc.ReconcileCertificates(ctx)
	if calls != 1 {
		t.Fatalf("precondition: enabled pass issued %d certificates, want 1", calls)
	}
	if ca, err := svc.CertificateCAView(ctx); err != nil || !ca.Present {
		t.Fatalf("precondition: enabled self_signed pass must create the CA: %+v (err %v)", ca, err)
	}

	// Reset to a clean slate and switch the module off.
	if err := svc.routes.DeleteCertificate(ctx, "a.int.example.test"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{certCACertKey, certCAKeySealedKey, certCAPrevCertKey} {
		if err := svc.settings.SetSystemSetting(ctx, key, "", now); err != nil {
			t.Fatal(err)
		}
	}
	calls = 0
	off := false
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertEnabled: &off}); err != nil {
		t.Fatalf("disable: %v", err)
	}

	// Swap in a spy that fails the test on ANY store access, so this pins "touches
	// nothing" rather than merely "wrote nothing observable". The real store is
	// restored afterwards for the assertions.
	realRoutes := svc.routes
	svc.routes = certStoreSpy{t: t}
	svc.ReconcileCertificates(ctx)
	svc.routes = realRoutes

	if calls != 0 {
		t.Fatalf("issuer called %d times with certificate management disabled, want 0", calls)
	}
	certs, err := svc.CertificatesView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(certs) != 0 {
		t.Fatalf("certificates written while disabled: %+v", certs)
	}
	ca, err := svc.CertificateCAView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ca.Present {
		t.Fatal("an internal CA must not be created while the module is off")
	}
}

func TestReconcileCertificatesIssuesForScopedServers(t *testing.T) {
	svc, ctx := certEnv(t)
	enableACME(t, svc, ctx, "all")
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	mustCreateNetbirdServer(t, svc, ctx, "srv-b", "b.int.example.test", "exclude")
	mustCreateNetbirdServer(t, svc, ctx, "srv-c", "c.other.example.net", "")

	now := time.Now().UTC()
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		return selfSigned(t, want.Domain, now.Add(-time.Hour), 90*24*time.Hour), nil
	}
	svc.ReconcileCertificates(ctx)

	byDomain := map[string]CertificateDTO{}
	certs, err := svc.CertificatesView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range certs {
		byDomain[c.Domain] = c
	}
	if got := byDomain["a.int.example.test"]; got.Status != "active" || got.Kind != "server" || got.NotAfter == nil {
		t.Fatalf("a = %+v, want an active server certificate with a parsed not_after", got)
	}
	if _, ok := byDomain["b.int.example.test"]; ok {
		t.Fatal("an excluded server must not get a certificate under scope=all")
	}
	if got := byDomain["c.other.example.net"]; got.Status != "skipped" || got.LastError == "" {
		t.Fatalf("a name outside the base domain must be skipped with a reason, got %+v", got)
	}
}

func TestReconcileCertificatesSelectedScopeNeedsInclude(t *testing.T) {
	svc, ctx := certEnv(t)
	enableACME(t, svc, ctx, "selected")
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	mustCreateNetbirdServer(t, svc, ctx, "srv-b", "b.int.example.test", "include")
	now := time.Now().UTC()
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		return selfSigned(t, want.Domain, now.Add(-time.Hour), 90*24*time.Hour), nil
	}
	svc.ReconcileCertificates(ctx)
	certs, _ := svc.CertificatesView(ctx)
	var domains []string
	for _, c := range certs {
		domains = append(domains, c.Domain)
	}
	if len(domains) != 1 || domains[0] != "b.int.example.test" {
		t.Fatalf("scope=selected issued for %v, want only the opted-in server", domains)
	}
}

func TestReconcileCertificatesRecordsErrorAndBacksOffWithoutDestroyingTheOldCert(t *testing.T) {
	svc, ctx := certEnv(t)
	enableACME(t, svc, ctx, "all")
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	now := time.Now().UTC()
	// First pass: a certificate that is already inside the renewal window.
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		return selfSigned(t, want.Domain, now.Add(-80*24*time.Hour), 90*24*time.Hour), nil
	}
	svc.ReconcileCertificates(ctx)
	first, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	// Second pass: renewal fails. The old certificate must survive.
	svc.cert.issuer = func(context.Context, CertSettings, desiredCert) (certissue.Result, error) {
		return certissue.Result{}, errors.New("acme: upstream said no")
	}
	svc.ReconcileCertificates(ctx)
	after, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if after.FullchainPEM != first.FullchainPEM || after.KeySealed != first.KeySealed {
		t.Fatal("a failed renewal must NOT replace the still-valid certificate")
	}
	if after.NotAfter != first.NotAfter || after.IssuedAt != first.IssuedAt {
		t.Fatalf("a failed renewal must not move the stored times: %+v vs %+v", after, first)
	}
	if after.Status != "error" || after.AttemptCount != 1 || !strings.Contains(after.LastError, "upstream said no") {
		t.Fatalf("expected an error record with a backoff, got %+v", after)
	}
	if after.NextAttemptAt.IsZero() || !after.NextAttemptAt.After(now) {
		t.Fatalf("next_attempt_at = %v, want a future backoff target", after.NextAttemptAt)
	}
}

func TestReconcileCertificatesTreatsAnIncompleteIssuerResultAsAFailure(t *testing.T) {
	// A "success" with no parsed leaf must not nil-panic the reconcile (which runs
	// in a background loop) and must not store an empty certificate as active.
	svc, ctx := certEnv(t)
	enableACME(t, svc, ctx, "all")
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	svc.cert.issuer = func(context.Context, CertSettings, desiredCert) (certissue.Result, error) {
		return certissue.Result{}, nil
	}
	svc.ReconcileCertificates(ctx)
	got, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "error" || got.FullchainPEM != "" || got.NextAttemptAt.IsZero() {
		t.Fatalf("an incomplete issuer result must record an error + backoff, got %+v", got)
	}
}

func TestReconcileCertificatesPrunesUnwantedDomains(t *testing.T) {
	svc, ctx := certEnv(t)
	enableACME(t, svc, ctx, "all")
	now := time.Now().UTC()
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: "gone.int.example.test", Kind: "server", Status: "active",
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(60 * 24 * time.Hour),
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		return selfSigned(t, want.Domain, now.Add(-time.Hour), 90*24*time.Hour), nil
	}
	svc.ReconcileCertificates(ctx)
	if _, err := svc.routes.CertificateByDomain(ctx, "gone.int.example.test"); err == nil {
		t.Fatal("a certificate whose domain left the desired set must be pruned")
	}
}

// netbirdBrokenPeer configures the NetBird module against a server that fails every
// call, so ResolveGatewayPeerDNS returns an error -- the transient failure mode
// (timeout / 401 / vanished peer) that must not be mistaken for "no gateway name".
func netbirdBrokenPeer(t *testing.T, svc *Service, ctx context.Context) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		NetbirdEnabled:       boolPtr(true),
		NetbirdURL:           strPtr(srv.URL),
		NetbirdToken:         strPtr("tok"),
		NetbirdGatewayPeerID: strPtr("peer-gone"),
	}); err != nil {
		t.Fatalf("configure netbird: %v", err)
	}
	// Precondition: the resolution really does fail (otherwise the test below would
	// pass for the wrong reason).
	if _, err := svc.ResolveGatewayPeerDNS(ctx); err == nil {
		t.Fatal("precondition: expected ResolveGatewayPeerDNS to fail")
	}
}

func TestReconcileCertificatesKeepsTheGatewayCertificateWhenItsNameCannotBeResolved(t *testing.T) {
	// cert_gateway_domain is blank (the default), so the gateway's name comes from a
	// LIVE NetBird call. When that call fails the name is missing from the desired
	// set -- but the certificate is still wanted, so pruning it would destroy the
	// sealed key and force a fresh order against the weekly duplicate limit.
	svc, ctx := certEnv(t)
	enableACME(t, svc, ctx, "all")
	now := time.Now().UTC()
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: "gw.int.example.test", Kind: "gateway", Status: "active",
		FullchainPEM: "PEM", KeySealed: "plain:KEY", Fingerprint: "aa",
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(60 * 24 * time.Hour),
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// A server certificate that IS genuinely unwanted must still be pruned -- the
	// guard is narrow, not a blanket "stop pruning".
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: "gone.int.example.test", Kind: "server", Status: "active",
		FullchainPEM: "PEM", KeySealed: "plain:KEY",
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(60 * 24 * time.Hour),
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	netbirdBrokenPeer(t, svc, ctx)
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		return selfSigned(t, want.Domain, now.Add(-time.Hour), 90*24*time.Hour), nil
	}

	svc.ReconcileCertificates(ctx)

	kept, err := svc.routes.CertificateByDomain(ctx, "gw.int.example.test")
	if err != nil {
		t.Fatalf("the gateway certificate was pruned on a transient NetBird failure: %v", err)
	}
	if kept.FullchainPEM != "PEM" || kept.KeySealed != "plain:KEY" {
		t.Fatalf("the gateway certificate material was altered: %+v", kept)
	}
	if _, err := svc.routes.CertificateByDomain(ctx, "gone.int.example.test"); err == nil {
		t.Fatal("a genuinely unwanted server certificate must still be pruned")
	}
}

func TestReconcileCertificatesPrunesTheGatewayRowWhenNoGatewayPeerIsSelected(t *testing.T) {
	// The mirror case: no gateway peer selected at all is a DELIBERATE configuration
	// ("", nil), not a failure -- so a stale gateway row is correctly pruned. This
	// keeps the guard from degenerating into "gateway rows are never pruned".
	svc, ctx := certEnv(t)
	enableACME(t, svc, ctx, "all")
	now := time.Now().UTC()
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: "stale.int.example.test", Kind: "gateway", Status: "active",
		FullchainPEM: "PEM", KeySealed: "plain:KEY",
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(60 * 24 * time.Hour),
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	svc.ReconcileCertificates(ctx)
	if _, err := svc.routes.CertificateByDomain(ctx, "stale.int.example.test"); err == nil {
		t.Fatal("a stale gateway row must be pruned when the gateway name is knowably absent")
	}
}

func TestReconcileCertificatesSuccessClearsThePreviousFailureState(t *testing.T) {
	// After a recovery the row must not keep advertising the old error, and the
	// attempt counter must reset so the next failure starts at the 5m step again.
	svc, ctx := certEnv(t)
	enableACME(t, svc, ctx, "all")
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	now := time.Now().UTC()
	svc.cert.issuer = func(context.Context, CertSettings, desiredCert) (certissue.Result, error) {
		return certissue.Result{}, errors.New("acme: upstream said no")
	}
	svc.ReconcileCertificates(ctx)
	failed, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "error" || failed.AttemptCount != 1 || failed.LastError == "" || failed.NextAttemptAt.IsZero() {
		t.Fatalf("precondition: expected a recorded failure, got %+v", failed)
	}
	// Clear the backoff so the next pass actually retries.
	if err := svc.RenewCertificateNow(ctx, systemToken(), "a.int.example.test"); err != nil {
		t.Fatal(err)
	}
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		return selfSigned(t, want.Domain, now.Add(-time.Hour), 90*24*time.Hour), nil
	}
	svc.ReconcileCertificates(ctx)
	got, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "active" {
		t.Fatalf("status = %q, want active after a successful re-issue", got.Status)
	}
	if got.LastError != "" {
		t.Fatalf("last_error = %q, want it cleared on success (a stale error would linger in the portal forever)", got.LastError)
	}
	if got.AttemptCount != 0 {
		t.Fatalf("attempt_count = %d, want 0 after a success", got.AttemptCount)
	}
	if !got.NextAttemptAt.IsZero() {
		t.Fatalf("next_attempt_at = %v, want cleared after a success", got.NextAttemptAt)
	}
}

func TestReconcileCertificatesOrdersTheMostUrgentNamesFirstUnderTheCap(t *testing.T) {
	// The per-pass cap protects the ACME rate limit, so WHICH names make the cut
	// matters: the ones closest to expiry must be served first.
	svc, ctx := certEnv(t)
	enableACME(t, svc, ctx, "all")
	now := time.Now().UTC()
	// certOrdersPerPass+2 names, each already due, with strictly increasing
	// remaining validity. Names are chosen so alphabetical order is the REVERSE of
	// urgency -- a comparator that sorted the wrong way (or not at all) would pick
	// a different set.
	type seed struct {
		domain    string
		remaining time.Duration
	}
	seeds := make([]seed, 0, certOrdersPerPass+2)
	for i := 0; i < certOrdersPerPass+2; i++ {
		// z... is the most urgent, a... the least.
		name := string(rune('z'-i)) + ".int.example.test"
		seeds = append(seeds, seed{domain: name, remaining: time.Duration(i+1) * 24 * time.Hour})
	}
	for i, s := range seeds {
		mustCreateNetbirdServer(t, svc, ctx, "srv-"+string(rune('a'+i)), s.domain, "")
		if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
			Domain: s.domain, Kind: "server", Status: "active",
			FullchainPEM: "PEM", KeySealed: "plain:KEY", Fingerprint: "old",
			// 90-day lifetime with 1..7 days left: the window is the full 30 days, so
			// EVERY seed is due and only the comparator decides who makes the cut.
			NotBefore: now.Add(s.remaining - 90*24*time.Hour), NotAfter: now.Add(s.remaining),
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var ordered []string
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		ordered = append(ordered, want.Domain)
		return selfSigned(t, want.Domain, now.Add(-time.Hour), 90*24*time.Hour), nil
	}

	svc.ReconcileCertificates(ctx)

	if len(ordered) != certOrdersPerPass {
		t.Fatalf("issued %d certificates (%v), want the per-pass cap of %d", len(ordered), ordered, certOrdersPerPass)
	}
	for i := 1; i < len(ordered); i++ {
		if ordered[i-1] >= ordered[i] {
			continue
		}
		t.Fatalf("issue order %v is not most-urgent-first (urgency runs z..a here)", ordered)
	}
	// The two least urgent names must be exactly the ones left out.
	served := map[string]bool{}
	for _, d := range ordered {
		served[d] = true
	}
	for _, s := range seeds[len(seeds)-2:] {
		if served[s.domain] {
			t.Fatalf("%s (remaining %v) was served ahead of a more urgent name", s.domain, s.remaining)
		}
	}
}

func TestIssueAndStoreKeepsMaterialWhenTheKeyCannotBeSealed(t *testing.T) {
	// A disk-backed settings store without an encryption key cannot seal a private
	// key. The certificate must NOT be stored keyless, the previous material must
	// survive, and the failure must carry a backoff -- the order already went
	// upstream, so retrying every pass would burn the rate limit.
	routes := routing.NewMemoryStore()
	svc := NewService(ServiceDeps{
		Routes:         routes,
		SystemSettings: NewMemorySystemSettings(),
		// SettingsVolatile:false + no CertCipher => sealCertSecret refuses.
		ACMEChallenges: certissue.NewMemoryChallengeStore(),
	})
	ctx := context.Background()
	if _, err := svc.sealCertSecret("x"); err == nil {
		t.Fatal("precondition: sealCertSecret must refuse without a certificate key on a disk store")
	}
	enableACME(t, svc, ctx, "all")
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	now := time.Now().UTC()
	if err := routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: "a.int.example.test", Kind: "server", ServerID: "srv-a", Status: "active",
		FullchainPEM: "OLD-PEM", KeySealed: "plain:OLD-KEY", Fingerprint: "old",
		NotBefore: now.Add(-80 * 24 * time.Hour), NotAfter: now.Add(2 * 24 * time.Hour),
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		return selfSigned(t, want.Domain, now.Add(-time.Hour), 90*24*time.Hour), nil
	}

	svc.ReconcileCertificates(ctx)

	got, err := routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if got.FullchainPEM != "OLD-PEM" || got.KeySealed != "plain:OLD-KEY" {
		t.Fatalf("a certificate whose key cannot be sealed must not replace the old material: %+v", got)
	}
	if got.Status != "error" || got.AttemptCount != 1 {
		t.Fatalf("expected a recorded failure, got %+v", got)
	}
	if got.NextAttemptAt.IsZero() {
		t.Fatal("a seal failure must set a backoff (the order already consumed upstream quota)")
	}
}

func TestLoadCARejectsACertificateAndKeyFromDifferentCAs(t *testing.T) {
	// The root and its key are two separate settings writes; a crash between them
	// leaves the NEW certificate next to the OLD key. certissue.LoadCA does not
	// check the pair, so a mismatch would sign leaves nobody can verify.
	svc, _ := certEnv(t)
	_, certA, keyA, err := certissue.NewCA("CA A", selfSignedCAValidity)
	if err != nil {
		t.Fatal(err)
	}
	_, _, keyB, err := certissue.NewCA("CA B", selfSignedCAValidity)
	if err != nil {
		t.Fatal(err)
	}
	sealedA, err := svc.sealCertSecret(keyA)
	if err != nil {
		t.Fatal(err)
	}
	sealedB, err := svc.sealCertSecret(keyB)
	if err != nil {
		t.Fatal(err)
	}
	// Matching pair loads.
	if _, err := svc.loadCA(map[string]string{certCACertKey: certA, certCAKeySealedKey: sealedA}); err != nil {
		t.Fatalf("a matching cert/key pair must load: %v", err)
	}
	// Mismatched pair is refused so ensureCA regenerates a consistent root.
	if _, err := svc.loadCA(map[string]string{certCACertKey: certA, certCAKeySealedKey: sealedB}); err == nil {
		t.Fatal("loadCA accepted a certificate signed by a DIFFERENT key than the one stored")
	}
}

func TestDesiredCertificatesDeduplicatesANameWantedTwice(t *testing.T) {
	// A public domain that is also a server's domain must yield ONE desired entry,
	// otherwise a single pass places two orders for the same name.
	svc, ctx := certEnv(t)
	on := true
	mode := IssuerModeACME
	email := "ops@example.test"
	base := "int.example.test"
	gw := ""
	scope := "all"
	manage := true
	domains := []string{"a.int.example.test"}
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:            &on,
		CertIssuerMode:         &mode,
		ACMEEmail:              &email,
		CertBaseDomain:         &base,
		CertGatewayDomain:      &gw,
		CertServerScope:        &scope,
		CertManagePublicDomain: &manage,
		CertPublicDomains:      &domains,
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	set, ok, err := svc.CertSettings(ctx)
	if err != nil || !ok {
		t.Fatalf("settings not usable: ok=%v err=%v", ok, err)
	}
	desired, _, _, err := svc.desiredCertificates(ctx, set, true)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, d := range desired {
		if d.Domain == "a.int.example.test" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("a.int.example.test appears %d times in %+v, want exactly 1", count, desired)
	}

	// End to end: only ONE order is placed for that name in a pass.
	var calls int
	now := time.Now().UTC()
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		if want.Domain == "a.int.example.test" {
			calls++
		}
		return selfSigned(t, want.Domain, now.Add(-time.Hour), 90*24*time.Hour), nil
	}
	svc.ReconcileCertificates(ctx)
	if calls != 1 {
		t.Fatalf("placed %d orders for the duplicated name, want 1", calls)
	}
}

func gatewayDesiredNames(t *testing.T, svc *Service, set CertSettings) []string {
	t.Helper()
	desired, _, _, err := svc.desiredCertificates(context.Background(), set, true)
	if err != nil {
		t.Fatalf("desiredCertificates: %v", err)
	}
	for _, want := range desired {
		if want.Kind == "gateway" {
			return want.Names
		}
	}
	t.Fatalf("no gateway certificate in desired set: %+v", desired)
	return nil
}

func configureGatewayPeerIP(t *testing.T, svc *Service, ip string) {
	t.Helper()
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)
	fake.mu.Lock()
	fake.peers["peer-gateway"] = map[string]any{
		"id": "peer-gateway", "name": "gateway", "dns_label": "gateway.mesh.test",
		"ip": ip, "connected": true,
	}
	fake.mu.Unlock()
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdGatewayPeerID: strPtr("peer-gateway"),
	}); err != nil {
		t.Fatalf("select gateway peer: %v", err)
	}
}

func TestGatewayDesiredNamesSelfSignedUsesExplicitBindIPBeforePeerIP(t *testing.T) {
	svc, _ := certEnv(t)
	svc = NewService(ServiceDeps{
		Routes:           svc.routes,
		SystemSettings:   svc.settings,
		SettingsVolatile: true,
		AgentBindHost:    "100.64.0.10",
	})
	configureGatewayPeerIP(t, svc, "100.64.0.20")

	got := gatewayDesiredNames(t, svc, CertSettings{
		GatewayDomain: "gateway.mesh.test",
		IssuerMode:    IssuerModeSelfSigned,
	})
	if want := []string{"gateway.mesh.test", "100.64.0.10"}; !slices.Equal(got, want) {
		t.Fatalf("gateway Names = %v, want %v", got, want)
	}
}

func TestGatewayDesiredNamesSelfSignedFallsBackToPeerIP(t *testing.T) {
	svc, _ := certEnv(t)
	configureGatewayPeerIP(t, svc, "100.64.0.20")

	got := gatewayDesiredNames(t, svc, CertSettings{
		GatewayDomain: "gateway.mesh.test",
		IssuerMode:    IssuerModeSelfSigned,
	})
	if want := []string{"gateway.mesh.test", "100.64.0.20"}; !slices.Equal(got, want) {
		t.Fatalf("gateway Names = %v, want %v", got, want)
	}
}

func TestGatewayDesiredNamesIgnoresExplicitBindHostname(t *testing.T) {
	svc, _ := certEnv(t)
	svc = NewService(ServiceDeps{
		Routes:           svc.routes,
		SystemSettings:   svc.settings,
		SettingsVolatile: true,
		AgentBindHost:    "listen-only.mesh.test",
	})
	configureGatewayPeerIP(t, svc, "100.64.0.20")

	got := gatewayDesiredNames(t, svc, CertSettings{
		GatewayDomain: "gateway.mesh.test",
		IssuerMode:    IssuerModeSelfSigned,
	})
	if want := []string{"gateway.mesh.test", "100.64.0.20"}; !slices.Equal(got, want) {
		t.Fatalf("gateway Names = %v, want %v (explicit bind hostname must not become a SAN)", got, want)
	}
}

func TestGatewayDesiredNamesACMERemainsDNSOnly(t *testing.T) {
	svc, _ := certEnv(t)
	svc = NewService(ServiceDeps{
		Routes:           svc.routes,
		SystemSettings:   svc.settings,
		SettingsVolatile: true,
		AgentBindHost:    "100.64.0.10",
	})

	got := gatewayDesiredNames(t, svc, CertSettings{
		GatewayDomain: "gateway.mesh.test",
		IssuerMode:    IssuerModeACME,
	})
	if want := []string{"gateway.mesh.test"}; !slices.Equal(got, want) {
		t.Fatalf("gateway Names = %v, want DNS-only %v", got, want)
	}
}

func TestReconcileDoesNotSelfHealGatewayLeafOnIPNameChange(t *testing.T) {
	svc, ctx := certEnv(t)
	const domain = "gateway.int.example.test"
	enableSelfSignedGatewayCertificate(t, svc, ctx, domain)
	svc.cert.issuer = svc.issueCertificate

	// With no explicit bind host and no usable NetBird resolver, the first real
	// reconcile must issue the exact DNS-only fallback promised by the spec.
	svc.ReconcileCertificates(ctx)
	first, err := svc.routes.CertificateByDomain(ctx, domain)
	if err != nil {
		t.Fatalf("read initial gateway certificate: %v", err)
	}
	firstLeaf := parseLeaf(first.FullchainPEM)
	if firstLeaf == nil {
		t.Fatal("initial gateway certificate has no parseable leaf")
	}
	if !slices.Equal(firstLeaf.DNSNames, []string{domain}) || len(firstLeaf.IPAddresses) != 0 {
		t.Fatalf("initial gateway SANs = DNS %v IP %v, want exactly [%s]", firstLeaf.DNSNames, firstLeaf.IPAddresses, domain)
	}
	if first.IssuerFingerprint == "" {
		t.Fatal("initial gateway leaf must record its internal CA issuer")
	}

	// Design decision (spec §3.5/§13): the gateway leaf is NOT self-healed on a
	// mesh-IP change. The FQDN is the canonical client name and a raw-IP gateway_url
	// is unsupported, so introducing an explicit bind IP -- far from normal renewal
	// -- must NOT trigger a SAN-drift re-issue. The IP-SAN is best-effort and is
	// picked up only at the next time-based renewal (sanDrift is edge-only). This is
	// what prevents re-issue flapping on transient NetBird resolver blips.
	svc.agentBindHost = "100.64.0.10"
	var issued []desiredCert
	svc.cert.issuer = recordingIssuer(svc, &issued)
	svc.ReconcileCertificates(ctx)
	if len(issued) != 0 {
		t.Fatalf("gateway leaf was self-healed on IP introduction (unexpected drift re-issue) = %+v", issued)
	}
	after, err := svc.routes.CertificateByDomain(ctx, domain)
	if err != nil {
		t.Fatalf("read gateway certificate after IP introduction: %v", err)
	}
	if after.Fingerprint != first.Fingerprint {
		t.Fatalf("gateway leaf changed without a renewal: %q -> %q", first.Fingerprint, after.Fingerprint)
	}

	// A subsequent listener-IP change must likewise NOT force a re-issue.
	svc.agentBindHost = "100.64.0.11"
	issued = nil
	svc.ReconcileCertificates(ctx)
	if len(issued) != 0 {
		t.Fatalf("gateway leaf was self-healed on IP change (unexpected drift re-issue) = %+v", issued)
	}
	unchanged, err := svc.routes.CertificateByDomain(ctx, domain)
	if err != nil {
		t.Fatalf("read gateway certificate after IP change: %v", err)
	}
	if unchanged.Fingerprint != first.Fingerprint {
		t.Fatalf("gateway leaf changed on IP change without a renewal: %q -> %q", first.Fingerprint, unchanged.Fingerprint)
	}
}

func TestReconcileKeepsCurrentSelfSignedGatewayIPSANOnPeerResolverError(t *testing.T) {
	svc, ctx := certEnv(t)
	const domain = "gateway.int.example.test"
	enableSelfSignedGatewayCertificate(t, svc, ctx, domain)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)
	fake.mu.Lock()
	fake.peers["peer-gateway"] = map[string]any{
		"id": "peer-gateway", "name": "gateway", "dns_label": domain,
		"ip": "100.64.0.20", "connected": true,
	}
	fake.mu.Unlock()
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		NetbirdGatewayPeerID: strPtr("peer-gateway"),
	}); err != nil {
		t.Fatalf("select gateway peer: %v", err)
	}
	svc.cert.issuer = svc.issueCertificate

	// The first pass resolves the peer IP and issues a real DNS+IP gateway leaf.
	svc.ReconcileCertificates(ctx)
	first, err := svc.routes.CertificateByDomain(ctx, domain)
	if err != nil {
		t.Fatalf("read initial gateway certificate: %v", err)
	}
	if sanDrift(first, []string{domain, "100.64.0.20"}) {
		t.Fatal("initial gateway leaf does not cover the resolved peer IP")
	}
	if first.IssuerFingerprint == "" {
		t.Fatal("initial gateway leaf must record its internal CA issuer")
	}

	// A control-plane outage makes the desired-name pass temporarily see only
	// the FQDN. That incomplete observation must not strip the working IP SAN.
	fake.srv.Close()
	var issued []desiredCert
	svc.cert.issuer = recordingIssuer(svc, &issued)
	svc.ReconcileCertificates(ctx)
	if len(issued) != 0 {
		t.Fatalf("resolver failure triggered a subtractive gateway SAN reissue: %+v", issued)
	}
	after, err := svc.routes.CertificateByDomain(ctx, domain)
	if err != nil {
		t.Fatalf("read gateway certificate after resolver failure: %v", err)
	}
	if after.Fingerprint != first.Fingerprint || sanDrift(after, []string{domain, "100.64.0.20"}) {
		t.Fatal("resolver failure did not preserve the existing DNS+IP gateway leaf")
	}
}

func TestReconcileDueGatewayPreservesExistingIPSANOnPeerResolverError(t *testing.T) {
	svc, ctx := certEnv(t)
	const domain = "gateway.int.example.test"
	enableSelfSignedGatewayCertificate(t, svc, ctx, domain)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)
	fake.mu.Lock()
	fake.peers["peer-gateway"] = map[string]any{
		"id": "peer-gateway", "name": "gateway", "dns_label": domain,
		"ip": "100.64.0.20", "connected": true,
	}
	fake.mu.Unlock()
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		NetbirdGatewayPeerID: strPtr("peer-gateway"),
	}); err != nil {
		t.Fatalf("select gateway peer: %v", err)
	}
	svc.cert.issuer = svc.issueCertificate

	// Establish a genuine current-CA gateway leaf with the resolved IP SAN.
	svc.ReconcileCertificates(ctx)
	first, err := svc.routes.CertificateByDomain(ctx, domain)
	if err != nil {
		t.Fatalf("read initial gateway certificate: %v", err)
	}
	if sanDrift(first, []string{domain, "100.64.0.20"}) || first.IssuerFingerprint == "" {
		t.Fatal("initial gateway leaf is not a current-CA DNS+IP certificate")
	}

	// Make the row genuinely due, then lose the control plane. Renewal remains
	// necessary, but the incomplete DNS-only observation must not strip the last
	// known-good IP SAN from the replacement leaf.
	now := time.Now().UTC()
	first.NotBefore = now.Add(-time.Hour)
	first.NotAfter = now.Add(time.Minute)
	if err := svc.routes.UpsertCertificate(ctx, first); err != nil {
		t.Fatalf("mark gateway certificate due: %v", err)
	}
	fake.srv.Close()
	var issued []desiredCert
	svc.cert.issuer = recordingIssuer(svc, &issued)
	svc.ReconcileCertificates(ctx)
	if len(issued) != 1 || issued[0].Kind != "gateway" {
		t.Fatalf("issued = %+v, want one due gateway renewal", issued)
	}
	if want := []string{domain, "100.64.0.20"}; !slices.Equal(issued[0].Names, want) {
		t.Fatalf("gateway renewal Names = %v, want preserved names %v", issued[0].Names, want)
	}
	after, err := svc.routes.CertificateByDomain(ctx, domain)
	if err != nil {
		t.Fatalf("read renewed gateway certificate: %v", err)
	}
	if after.Fingerprint == first.Fingerprint || sanDrift(after, []string{domain, "100.64.0.20"}) {
		t.Fatal("due renewal during resolver failure did not preserve the existing IP SAN")
	}
}

func TestReconcileGatewaySANDriftDoesNotReplaceRetainedACMELeaf(t *testing.T) {
	svc, ctx := certEnv(t)
	const domain = "gateway.int.example.test"
	enableSelfSignedGatewayCertificate(t, svc, ctx, domain)
	svc.agentBindHost = "100.64.0.10"
	now := time.Now().UTC()
	seed := selfSigned(t, domain, now.Add(-time.Hour), 365*24*time.Hour)
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: domain, Kind: "gateway", Status: "active",
		FullchainPEM: seed.FullchainPEM, KeySealed: "plain:ACME-KEY",
		Fingerprint: seed.Fingerprint,
		NotBefore:   seed.Leaf.NotBefore.UTC(), NotAfter: seed.Leaf.NotAfter.UTC(),
		IssuedAt: now, CreatedAt: now, UpdatedAt: now,
		// Empty IssuerFingerprint is the stored-leaf truth for ACME, even though
		// the current target mode above is now self_signed.
	}); err != nil {
		t.Fatalf("seed retained ACME gateway leaf: %v", err)
	}

	var issued []desiredCert
	svc.cert.issuer = recordingIssuer(svc, &issued)
	svc.ReconcileCertificates(ctx)
	if len(issued) != 0 {
		t.Fatalf("retained ACME gateway leaf was replaced solely for self-signed SAN drift: %+v", issued)
	}
	after, err := svc.routes.CertificateByDomain(ctx, domain)
	if err != nil {
		t.Fatalf("read retained ACME gateway leaf: %v", err)
	}
	if after.Fingerprint != seed.Fingerprint || after.IssuerFingerprint != "" {
		t.Fatalf("retained ACME gateway leaf changed: %+v", after)
	}
}

func TestRenewDueThresholdHonorsShortLivedCertificates(t *testing.T) {
	now := time.Now().UTC()
	// 90-day certificate, 30-day window: not due at 40 days remaining, due at 20.
	ninety := routing.Certificate{Status: "active", NotBefore: now.Add(-50 * 24 * time.Hour), NotAfter: now.Add(40 * 24 * time.Hour)}
	if renewDue(ninety, 30, now, "x.example.test", "") {
		t.Fatal("40 days remaining on a 90-day certificate must not be due")
	}
	ninety.NotAfter = now.Add(20 * 24 * time.Hour)
	if !renewDue(ninety, 30, now, "x.example.test", "") {
		t.Fatal("20 days remaining must be due")
	}
	// 6-day certificate: the window collapses to lifetime/3 (2 days), so 3 days
	// remaining is NOT due -- with a flat 30-day window it would always be due.
	sixDay := routing.Certificate{Status: "active", NotBefore: now.Add(-3 * 24 * time.Hour), NotAfter: now.Add(3 * 24 * time.Hour)}
	if renewDue(sixDay, 30, now, "x.example.test", "") {
		t.Fatal("a 6-day certificate with 3 days left must not be permanently due")
	}
	sixDay.NotAfter = now.Add(24 * time.Hour)
	if !renewDue(sixDay, 30, now, "x.example.test", "") {
		t.Fatal("a 6-day certificate with 1 day left must be due")
	}
}

func TestRenewDueIgnoresIssuerMismatchForAnACMELeaf(t *testing.T) {
	// The issuer-mismatch rule exists for CA ROTATION only. An ACME leaf carries
	// no issuer fingerprint, so switching into the self_signed mode (a non-empty
	// caFingerprint) must NOT force it to be re-issued -- clients that lack the
	// internal root would break instantly.
	now := time.Now().UTC()
	acmeLeaf := routing.Certificate{Status: "active", NotBefore: now.Add(-time.Hour), NotAfter: now.Add(80 * 24 * time.Hour)}
	if renewDue(acmeLeaf, 30, now, "x.example.test", "cafingerprint") {
		t.Fatal("an acme leaf must not become due just because an internal CA exists")
	}
	internalLeaf := acmeLeaf
	internalLeaf.IssuerFingerprint = "oldcafingerprint"
	if !renewDue(internalLeaf, 30, now, "x.example.test", "cafingerprint") {
		t.Fatal("a leaf of a rotated-out internal root must be due")
	}
	// And in the acme mode (no current CA) a still-valid internal leaf stays put.
	if renewDue(internalLeaf, 30, now, "x.example.test", "") {
		t.Fatal("switching to acme must not force a still-valid internal leaf")
	}
}

func TestReconcileCertificatesSelfSignedIssuesWithoutACME(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 90)
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	// A name OUTSIDE the base domain is fine here: the internal CA can sign anything
	// (unlike HTTP-01, which needs the public wildcard record).
	mustCreateNetbirdServer(t, svc, ctx, "srv-b", "b.other.example.net", "")
	// No issuer stub: this must go through the REAL self-signed path.
	svc.cert.issuer = svc.issueCertificate

	svc.ReconcileCertificates(ctx)

	certs, err := svc.CertificatesView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byDomain := map[string]CertificateDTO{}
	for _, c := range certs {
		byDomain[c.Domain] = c
	}
	for _, domain := range []string{"a.int.example.test", "b.other.example.net"} {
		got := byDomain[domain]
		if got.Status != "active" || got.NotAfter == nil {
			t.Fatalf("%s = %+v, want an active certificate", domain, got)
		}
		// The configured 90-day leaf validity is honored.
		if d := got.NotAfter.Sub(*got.NotBefore); d < 89*24*time.Hour || d > 91*24*time.Hour {
			t.Fatalf("%s lifetime = %v, want ~90 days", domain, d)
		}
	}
	// The stored chain must verify against the CA the portal reports.
	ca, err := svc.CertificateCAView(ctx)
	if err != nil || !ca.Present {
		t.Fatalf("CA view = %+v err = %v, want a present CA", ca, err)
	}
	stored, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if stored.IssuerFingerprint != ca.Fingerprint {
		t.Fatalf("issuer fingerprint %q != CA %q", stored.IssuerFingerprint, ca.Fingerprint)
	}
	bundle, err := svc.CertificateCABundlePEM(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(bundle)) {
		t.Fatal("bundle is not parseable PEM")
	}
	leafBlock, _ := pem.Decode([]byte(stored.FullchainPEM))
	leaf, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "a.int.example.test"}); err != nil {
		t.Fatalf("issued leaf must verify against the published bundle: %v", err)
	}
}

func TestCARotationKeepsTheOldRootInTheBundleAndReissuesLeaves(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 90)
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	svc.cert.issuer = svc.issueCertificate
	svc.ReconcileCertificates(ctx)

	first, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	oldCA, err := svc.CertificateCAView(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.RotateCertificateCA(ctx, systemToken()); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	newCA, err := svc.CertificateCAView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if newCA.Fingerprint == oldCA.Fingerprint {
		t.Fatal("rotation must produce a NEW root")
	}
	if newCA.PreviousFingerprint != oldCA.Fingerprint {
		t.Fatalf("previous fingerprint = %q, want the old root %q", newCA.PreviousFingerprint, oldCA.Fingerprint)
	}
	// The overlap: BOTH roots are in the bundle, so a client that only has the old
	// one keeps working until it has fetched the new bundle.
	bundle, err := svc.CertificateCABundlePEM(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(bundle, "BEGIN CERTIFICATE") != 2 {
		t.Fatalf("bundle must carry new + previous root, got %q", bundle)
	}
	// The still-far-from-expiry leaf is due purely because its issuer changed.
	if !renewDue(first, 30, time.Now().UTC(), first.Domain, newCA.Fingerprint) {
		t.Fatal("a leaf of the rotated-out root must count as due")
	}
	svc.ReconcileCertificates(ctx)
	second, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if second.IssuerFingerprint != newCA.Fingerprint {
		t.Fatalf("re-issued leaf issuer = %q, want the new root %q", second.IssuerFingerprint, newCA.Fingerprint)
	}
	if second.Fingerprint == first.Fingerprint {
		t.Fatal("the leaf must actually have been re-issued")
	}
}

// ----------------------------------------------------------------------
// F1.2 -- RotateCertificateCA must not block for up to the reconcile pass's
// own deadline when a pass currently holds certMu: it should fail fast with
// ErrCertReconcileInProgress instead. This holds the lock directly (as a
// running reconcile would) rather than actually driving a slow reconcile, so
// the assertion is deterministic and does not depend on timing.
// ----------------------------------------------------------------------

func TestRotateCertificateCAFailsFastInsteadOfBlockingWhileReconcileHoldsTheLock(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 90)

	svc.cert.mu.Lock()
	defer svc.cert.mu.Unlock()

	if err := svc.RotateCertificateCA(ctx, systemToken()); !errors.Is(err, ErrCertReconcileInProgress) {
		t.Fatalf("RotateCertificateCA while certMu is held = %v, want ErrCertReconcileInProgress", err)
	}
}

func TestRotateCertificateCAStillSucceedsOnceTheLockIsFree(t *testing.T) {
	// The successful path (lock immediately available) must be byte-identical
	// to before F1.2: a rotation right after the held-lock case above still
	// works once the lock is released -- TryLock never starves a legitimate,
	// uncontended rotation.
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 90)
	svc.cert.mu.Lock()
	svc.cert.mu.Unlock() //nolint:staticcheck // deliberate lock barrier: proves the lock is acquirable and released before testing the now-uncontended rotation

	if err := svc.RotateCertificateCA(ctx, systemToken()); err != nil {
		t.Fatalf("rotate after the lock freed up: %v", err)
	}
}

// ----------------------------------------------------------------------
// F1.3 -- loadCA's crash-recovery must not discard the genuinely-previous
// root when the cert/key pair specifically fails the pairing check (the
// signature of a crash between newCA's two writes): the value that should
// move into the previous-root slot is whatever is ALREADY there (correctly
// recorded by the crashed attempt's first, successful write), not the
// orphaned cert column.
// ----------------------------------------------------------------------

func TestEnsureCARecoveryPreservesTheGenuinePreviousRootOnAKeyMismatch(t *testing.T) {
	svc, ctx := certEnv(t)
	now := svc.clock().UTC()

	// Simulate the exact crash window: a rotation from CA_A to CA_B wrote the
	// previous-root column (CA_A, correctly) and the new cert column (CA_B),
	// then crashed BEFORE the key column caught up -- so cert_ca_key_sealed
	// still holds key_A, which does not pair with CA_B.
	_, certA, keyA, err := certissue.NewCA("CA-A", 10*365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, certB, _, err := certissue.NewCA("CA-B", 10*365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sealedA, err := svc.sealCertSecret(keyA)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{
		{certCAPrevCertKey, certA},
		{certCACertKey, certB},
		{certCAKeySealedKey, sealedA},
	} {
		if err := svc.settings.SetSystemSetting(ctx, kv[0], kv[1], now); err != nil {
			t.Fatal(err)
		}
	}

	// Precondition: this really is the pairing-mismatch failure, not some
	// other loadCA error, so the fix's guard actually fires.
	values, err := svc.settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.loadCA(values); !errors.Is(err, errCAKeyMismatch) {
		t.Fatalf("precondition: loadCA err = %v, want errCAKeyMismatch", err)
	}

	if _, err := svc.ensureCA(ctx, CertSettings{IssuerMode: IssuerModeSelfSigned}, "int.example.test"); err != nil {
		t.Fatalf("ensureCA recovery: %v", err)
	}

	bundle, err := svc.CertificateCABundlePEM(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bundle, certA) {
		t.Fatalf("recovery lost the genuine previous root CA_A from the trust bundle:\n%s", bundle)
	}
	if strings.Contains(bundle, certB) {
		t.Fatal("the orphaned, never-used CA_B must not end up in the trust bundle either")
	}
}

func TestRotateCertificateCAAlsoPreservesThePreviousRootOnAKeyMismatch(t *testing.T) {
	// The manual "rotate now" button shares the exact same vulnerability as
	// ensureCA's automatic recovery (both call newCA with a chosen previous-
	// root value) -- prove it independently rather than assuming the shared
	// helper is exercised the same way from both call sites.
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 90)
	now := svc.clock().UTC()

	_, certA, keyA, err := certissue.NewCA("CA-A", 10*365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, certB, _, err := certissue.NewCA("CA-B", 10*365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sealedA, err := svc.sealCertSecret(keyA)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{
		{certCAPrevCertKey, certA},
		{certCACertKey, certB},
		{certCAKeySealedKey, sealedA},
	} {
		if err := svc.settings.SetSystemSetting(ctx, kv[0], kv[1], now); err != nil {
			t.Fatal(err)
		}
	}

	if err := svc.RotateCertificateCA(ctx, systemToken()); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	bundle, err := svc.CertificateCABundlePEM(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bundle, certA) {
		t.Fatalf("manual rotate recovery lost the genuine previous root CA_A:\n%s", bundle)
	}
}

// TestEnsureCARecoveryOnAnUnparseableCertKeepsTodaysBehavior pins the
// documented carve-out in resolvePreviousCertPEM: when the stored cert
// column cannot even be parsed, loadCA never reaches the pairing check (a
// different error fires first), so the fix must not change behavior here --
// the unparseable value is still forwarded verbatim as before, and newCA's
// own defensive fingerprint check simply drops it (rather than crashing or
// silently fabricating a "previous" root out of garbage).
func TestEnsureCARecoveryOnAnUnparseableCertKeepsTodaysBehavior(t *testing.T) {
	svc, ctx := certEnv(t)
	now := svc.clock().UTC()
	if err := svc.settings.SetSystemSetting(ctx, certCACertKey, "not a pem certificate", now); err != nil {
		t.Fatal(err)
	}
	if err := svc.settings.SetSystemSetting(ctx, certCAKeySealedKey, "plain:not-a-key-either", now); err != nil {
		t.Fatal(err)
	}

	ca, err := svc.ensureCA(ctx, CertSettings{IssuerMode: IssuerModeSelfSigned}, "int.example.test")
	if err != nil {
		t.Fatalf("ensureCA must recover by generating a fresh CA: %v", err)
	}
	if ca == nil || ca.Cert == nil {
		t.Fatal("ensureCA returned no usable CA")
	}
	view, err := svc.CertificateCAView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.PreviousFingerprint != "" {
		t.Fatalf("previous_fingerprint = %q, want empty -- the unparseable value must be dropped, not surfaced as a root", view.PreviousFingerprint)
	}
}

func TestModeSwitchDoesNotReplaceValidCertificates(t *testing.T) {
	// Direction 1: acme -> self_signed. A valid ACME leaf must live out its term;
	// forcing an internal-CA leaf here would break every client that has not yet
	// imported the root.
	svc, ctx := certEnv(t)
	enableACME(t, svc, ctx, "all")
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	now := time.Now().UTC()
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		return selfSigned(t, want.Domain, now.Add(-time.Hour), 90*24*time.Hour), nil
	}
	svc.ReconcileCertificates(ctx)
	acmeLeaf, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if acmeLeaf.IssuerFingerprint != "" {
		t.Fatalf("an acme leaf must carry no issuer fingerprint, got %q", acmeLeaf.IssuerFingerprint)
	}

	enableSelfSigned(t, svc, ctx, "all", 90)
	svc.cert.issuer = svc.issueCertificate
	svc.ReconcileCertificates(ctx)

	after, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if after.Fingerprint != acmeLeaf.Fingerprint {
		t.Fatal("switching to self_signed must NOT re-issue a still-valid acme certificate")
	}
	// The CA is created anyway, so its root is published BEFORE any internal leaf
	// exists (anchor before leaf).
	ca, err := svc.CertificateCAView(ctx)
	if err != nil || !ca.Present {
		t.Fatalf("the switch must create the CA up front: %+v (err %v)", ca, err)
	}

	// Direction 2: self_signed -> acme. The internal leaf keeps its term as well,
	// and the CA is NOT deleted (its root must stay in the bundle while that leaf
	// lives).
	svc2, ctx2 := certEnv(t)
	enableSelfSigned(t, svc2, ctx2, "all", 90)
	mustCreateNetbirdServer(t, svc2, ctx2, "srv-b", "b.int.example.test", "")
	svc2.cert.issuer = svc2.issueCertificate
	svc2.ReconcileCertificates(ctx2)
	internalLeaf, err := svc2.routes.CertificateByDomain(ctx2, "b.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if internalLeaf.IssuerFingerprint == "" {
		t.Fatal("an internal leaf must record its issuer fingerprint")
	}
	caBefore, err := svc2.CertificateCAView(ctx2)
	if err != nil {
		t.Fatal(err)
	}

	enableACME(t, svc2, ctx2, "all")
	svc2.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		t.Fatalf("the acme issuer must not be called for a still-valid internal leaf (%s)", want.Domain)
		return certissue.Result{}, nil
	}
	svc2.ReconcileCertificates(ctx2)
	stillThere, err := svc2.routes.CertificateByDomain(ctx2, "b.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if stillThere.Fingerprint != internalLeaf.Fingerprint {
		t.Fatal("switching to acme must NOT re-issue a still-valid internal certificate")
	}
	caAfter, err := svc2.CertificateCAView(ctx2)
	if err != nil {
		t.Fatal(err)
	}
	if !caAfter.Present || caAfter.Fingerprint != caBefore.Fingerprint {
		t.Fatal("the internal CA must survive a switch to acme (its root is still needed)")
	}
}

func TestModeSwitchKeepsACertificateTheNewIssuerCannotServe(t *testing.T) {
	// A name outside the base domain: the internal CA can sign it, HTTP-01 never
	// can. After the switch it must keep its valid certificate and only gain a
	// reason -- not lose the material.
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 90)
	mustCreateNetbirdServer(t, svc, ctx, "srv-out", "b.other.example.net", "")
	svc.cert.issuer = svc.issueCertificate
	svc.ReconcileCertificates(ctx)
	before, err := svc.routes.CertificateByDomain(ctx, "b.other.example.net")
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != "active" {
		t.Fatalf("precondition: %+v", before)
	}

	enableACME(t, svc, ctx, "all")
	svc.ReconcileCertificates(ctx)

	after, err := svc.routes.CertificateByDomain(ctx, "b.other.example.net")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "active" || after.FullchainPEM != before.FullchainPEM {
		t.Fatalf("a name the new issuer cannot serve must keep its valid certificate, got %+v", after)
	}
	if after.LastError == "" {
		t.Fatal("the reason must be recorded so the list can explain it")
	}
}

func TestReissueAllCertificatesForcesTheSwitch(t *testing.T) {
	svc, ctx := certEnv(t)
	enableACME(t, svc, ctx, "all")
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	now := time.Now().UTC()
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		return selfSigned(t, want.Domain, now.Add(-time.Hour), 90*24*time.Hour), nil
	}
	svc.ReconcileCertificates(ctx)
	first, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}

	enableSelfSigned(t, svc, ctx, "all", 90)
	svc.cert.issuer = svc.issueCertificate
	if err := svc.ReissueAllCertificates(ctx, systemToken()); err != nil {
		t.Fatal(err)
	}
	// The material still stands until the replacement arrives.
	marked, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if marked.FullchainPEM != first.FullchainPEM {
		t.Fatal("marking for re-issue must not drop the current material")
	}
	if marked.AttemptCount != first.AttemptCount {
		t.Fatalf("attempt count = %d, want it preserved (rate-limit guard)", marked.AttemptCount)
	}
	svc.ReconcileCertificates(ctx)
	after, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if after.IssuerFingerprint == "" || after.Fingerprint == first.Fingerprint {
		t.Fatalf("after an explicit re-issue the certificate must come from the internal CA, got %+v", after)
	}
}

func TestRenewCertificateNowKeepsTheStoredMaterial(t *testing.T) {
	svc, ctx := certEnv(t)
	now := time.Now().UTC()
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: "gw.int.example.test", Kind: "gateway", Status: "active",
		FullchainPEM: "PEM", KeySealed: "plain:KEY", Fingerprint: "aa",
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(60 * 24 * time.Hour),
		AttemptCount: 2, NextAttemptAt: now.Add(6 * time.Hour), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RenewCertificateNow(ctx, systemToken(), "GW.int.example.test"); err != nil {
		t.Fatalf("renew now: %v", err)
	}
	got, err := svc.routes.CertificateByDomain(ctx, "gw.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if got.FullchainPEM != "PEM" || got.KeySealed != "plain:KEY" || got.NotAfter.IsZero() {
		t.Fatalf("manual renew must not touch the stored certificate: %+v", got)
	}
	if got.Status != "pending" || !got.NextAttemptAt.IsZero() {
		t.Fatalf("manual renew must clear the backoff: %+v", got)
	}
	if got.AttemptCount != 2 {
		t.Fatalf("attempt count = %d, want it preserved (rate-limit guard)", got.AttemptCount)
	}
	if err := svc.RenewCertificateNow(ctx, systemToken(), "unknown.example.test"); !errors.Is(err, ErrCertificateNotFound) {
		t.Fatalf("unknown domain err = %v, want ErrCertificateNotFound", err)
	}
}

// TestRenewCertificateNowForbidsNonSystem proves the PT-2 Part 2b internal authz
// guard: a principal without the "system" scope is rejected with
// ErrPrincipalForbidden and the stored certificate row is left completely
// untouched -- the HTTP-level gate (requireWebScope("system") in
// handleSystemCertificateRenew) is defense-in-depth on TOP of this, not instead
// of it.
func TestRenewCertificateNowForbidsNonSystem(t *testing.T) {
	svc, ctx := certEnv(t)
	now := time.Now().UTC()
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: "gw2.int.example.test", Kind: "gateway", Status: "active",
		FullchainPEM: "PEM", KeySealed: "plain:KEY", Fingerprint: "aa",
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(60 * 24 * time.Hour),
		AttemptCount: 2, NextAttemptAt: now.Add(6 * time.Hour), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		tok  auth.Token
	}{
		{"plain admin (no system scope)", adminToken()},
		{"owner", ownerToken()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := svc.RenewCertificateNow(ctx, tc.tok, "gw2.int.example.test"); !errors.Is(err, ErrPrincipalForbidden) {
				t.Fatalf("RenewCertificateNow(%s) err = %v, want ErrPrincipalForbidden", tc.name, err)
			}
		})
	}
	got, err := svc.routes.CertificateByDomain(ctx, "gw2.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "active" || got.NextAttemptAt.IsZero() {
		t.Fatalf("certificate mutated despite ErrPrincipalForbidden: %+v", got)
	}

	// The flip side: a system-scoped principal succeeds exactly as before the
	// guard was added.
	if err := svc.RenewCertificateNow(ctx, systemToken(), "gw2.int.example.test"); err != nil {
		t.Fatalf("RenewCertificateNow(system): %v", err)
	}
	got, err = svc.routes.CertificateByDomain(ctx, "gw2.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" || !got.NextAttemptAt.IsZero() {
		t.Fatalf("system principal must still be able to renew: %+v", got)
	}
}

func TestCertificateCAViewCarriesNoKeyMaterial(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 30)
	svc.cert.issuer = svc.issueCertificate
	svc.ReconcileCertificates(ctx)
	ca, err := svc.CertificateCAView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	blob := mustMarshalJSON(t, ca)
	for _, needle := range []string{"PRIVATE KEY", "key", "sealed"} {
		if strings.Contains(strings.ToLower(blob), strings.ToLower(needle)) {
			t.Fatalf("CA DTO leaks %q: %s", needle, blob)
		}
	}
	// The published bundle is public certificates only.
	bundle, err := svc.CertificateCABundlePEM(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bundle, "PRIVATE KEY") {
		t.Fatal("the CA bundle must never contain a private key")
	}
}

func TestCertificateDTOCarriesNoKeyMaterial(t *testing.T) {
	svc, ctx := certEnv(t)
	now := time.Now().UTC()
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: "gw.int.example.test", Kind: "gateway", Status: "active",
		FullchainPEM: "-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n",
		KeySealed:    "enc:VERY-SECRET", Fingerprint: "ff",
		NotBefore: now, NotAfter: now.Add(time.Hour), IssuedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	certs, err := svc.CertificatesView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	blob := mustMarshalJSON(t, certs)
	for _, needle := range []string{"VERY-SECRET", "BEGIN CERTIFICATE", "key_sealed", "fullchain"} {
		if strings.Contains(blob, needle) {
			t.Fatalf("certificate DTO leaks %q: %s", needle, blob)
		}
	}
}

func TestSetServerCertificateOverrideRejectsAnUnknownValueAndAnUnknownServer(t *testing.T) {
	svc, ctx := certEnv(t)
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	system := systemAdminToken()
	if _, err := svc.SetServerCertificateOverride(ctx, system, "srv-a", "maybe"); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("bad override err = %v, want ErrCertInvalid", err)
	}
	if _, err := svc.SetServerCertificateOverride(ctx, system, "nope", "include"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("unknown server err = %v, want ErrServerNotFound (404-no-leak)", err)
	}
	// A caller with neither the system scope nor ownership gets the SAME
	// 404-no-leak, never a 403 that would confirm the server exists.
	if _, err := svc.SetServerCertificateOverride(ctx, auth.Token{UserID: "u-other"}, "srv-a", "include"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("outsider err = %v, want ErrServerNotFound (404-no-leak)", err)
	}
	stored, err := svc.routes.AIServerByID(ctx, "srv-a")
	if err != nil {
		t.Fatal(err)
	}
	if stored.CertificateOverride != "" {
		t.Fatalf("a rejected write must not persist: %q", stored.CertificateOverride)
	}
}

// TestHTTPSSwitchOverride3State pins P4's per-server override
// (AIServer.HTTPSSwitchOverride) as a SINGLE 3-state column, not two
// booleans: setting "include" must clear a stored "exclude" and vice versa
// (there is only ever one stored value to begin with), it round-trips exactly
// as ""|"include"|"exclude" through SetServerHTTPSSwitchOverride ->
// GetServer, an unknown value is rejected (ErrCertInvalid), and an
// unknown/unauthorized server is the same 404-no-leak ErrServerNotFound as
// SetServerCertificateOverride.
func TestHTTPSSwitchOverride3State(t *testing.T) {
	svc, ctx := certEnv(t)
	mustCreateNetbirdServer(t, svc, ctx, "srv-h", "h.int.example.test", "")
	system := systemAdminToken()

	dto, err := svc.SetServerHTTPSSwitchOverride(ctx, system, "srv-h", "include")
	if err != nil {
		t.Fatalf("set include: %v", err)
	}
	if dto.HTTPSSwitchOverride != "include" {
		t.Fatalf("dto.HTTPSSwitchOverride = %q, want include", dto.HTTPSSwitchOverride)
	}
	stored, err := svc.routes.AIServerByID(ctx, "srv-h")
	if err != nil {
		t.Fatal(err)
	}
	if stored.HTTPSSwitchOverride != "include" {
		t.Fatalf("stored HTTPSSwitchOverride = %q, want include", stored.HTTPSSwitchOverride)
	}

	// Flipping to "exclude" must CLEAR the stored "include" -- one column, no
	// stale opposite flag left behind for a later mode flip to resurrect.
	if _, err := svc.SetServerHTTPSSwitchOverride(ctx, system, "srv-h", "exclude"); err != nil {
		t.Fatalf("set exclude: %v", err)
	}
	stored, err = svc.routes.AIServerByID(ctx, "srv-h")
	if err != nil {
		t.Fatal(err)
	}
	if stored.HTTPSSwitchOverride != "exclude" {
		t.Fatalf("stored HTTPSSwitchOverride = %q, want exclude (include must not survive)", stored.HTTPSSwitchOverride)
	}

	// Back to "" (follow the global mode).
	if _, err := svc.SetServerHTTPSSwitchOverride(ctx, system, "srv-h", ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	stored, err = svc.routes.AIServerByID(ctx, "srv-h")
	if err != nil {
		t.Fatal(err)
	}
	if stored.HTTPSSwitchOverride != "" {
		t.Fatalf("stored HTTPSSwitchOverride = %q, want empty", stored.HTTPSSwitchOverride)
	}

	// An unknown value is rejected.
	if _, err := svc.SetServerHTTPSSwitchOverride(ctx, system, "srv-h", "maybe"); !errors.Is(err, ErrCertInvalid) {
		t.Fatalf("bad override err = %v, want ErrCertInvalid", err)
	}
	// An unknown server is the 404-no-leak, mirroring SetServerCertificateOverride.
	if _, err := svc.SetServerHTTPSSwitchOverride(ctx, system, "nope", "include"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("unknown server err = %v, want ErrServerNotFound (404-no-leak)", err)
	}
	if _, err := svc.SetServerHTTPSSwitchOverride(ctx, auth.Token{UserID: "u-other"}, "srv-h", "include"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("outsider err = %v, want ErrServerNotFound (404-no-leak)", err)
	}
}

// TestHTTPSSwitchInScope tables httpsSwitchInScope over every mode x override
// combination: "manual" is never in scope regardless of override; "auto" is
// in scope unless explicitly excluded (opt-out); "selected" is in scope only
// when explicitly included (opt-in).
func TestHTTPSSwitchInScope(t *testing.T) {
	tests := []struct {
		mode     string
		override string
		want     bool
	}{
		{"manual", "", false},
		{"manual", "include", false},
		{"manual", "exclude", false},
		{"auto", "", true},
		{"auto", "include", true},
		{"auto", "exclude", false},
		{"selected", "", false},
		{"selected", "include", true},
		{"selected", "exclude", false},
	}
	for _, tt := range tests {
		srv := routing.AIServer{ID: "s", HTTPSSwitchOverride: tt.override}
		if got := httpsSwitchInScope(srv, tt.mode); got != tt.want {
			t.Errorf("httpsSwitchInScope(override=%q, mode=%q) = %v, want %v", tt.override, tt.mode, got, tt.want)
		}
	}
}

// ----------------------------------------------------------------------
// F1.1 -- cert_last_error: a reconcile pass that aborts at one of its two
// silent gates (no base domain resolvable, or the internal CA's key cannot
// be sealed) must leave an operator-actionable trace instead of looking
// identical to "never ran". This walks through both abort reasons AND the
// eventual successful pass, in ONE flowing scenario, so it also pins the
// non-obvious ordering property: a later, DIFFERENT abort reason overwrites
// an earlier one (never prematurely clears while still stuck), and only a
// pass that gets past BOTH gates clears the note.
// ----------------------------------------------------------------------

func TestReconcileCertificatesLastErrorLifecycle(t *testing.T) {
	// A disk-backed settings store with NO certificate cipher: sealCertSecret
	// refuses (the exact precondition for the CA-seal-failure gate below).
	routes := routing.NewMemoryStore()
	svc := NewService(ServiceDeps{
		Routes:         routes,
		SystemSettings: NewMemorySystemSettings(),
		ACMEChallenges: certissue.NewMemoryChallengeStore(),
	})
	ctx := context.Background()
	if _, err := svc.sealCertSecret("x"); err == nil {
		t.Fatal("precondition: sealCertSecret must refuse without a certificate key on a disk store")
	}

	// Phase 1: self_signed mode, no base domain configured, no NetBird module
	// to fall back to -> gate 1 (base domain) aborts the pass.
	on := true
	mode := IssuerModeSelfSigned
	blank := ""
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:    &on,
		CertIssuerMode: &mode,
		CertBaseDomain: &blank,
	}); err != nil {
		t.Fatalf("enable with no base domain: %v", err)
	}
	svc.ReconcileCertificates(ctx)
	values, err := svc.settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := CertLastError(values); got != certNoBaseDomainMessage {
		t.Fatalf("phase 1: cert_last_error = %q, want the no-base-domain message %q", got, certNoBaseDomainMessage)
	}
	if ca, err := svc.CertificateCAView(ctx); err != nil || ca.LastError != certNoBaseDomainMessage {
		t.Fatalf("phase 1: CertificateCAView.LastError = %q (err %v), want the no-base-domain message", ca.LastError, err)
	}

	// Phase 2: fix the base domain, but the seal key is STILL missing -> the
	// pass now gets past gate 1 and aborts at gate 2 (the internal CA cannot
	// be created because its key cannot be sealed). The note must move to
	// this NEW, more current reason -- it must NOT stay on the phase-1
	// message, and it must NOT have been prematurely cleared by phase 1
	// merely resolving its own gate.
	base := "int.example.test"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertBaseDomain: &base}); err != nil {
		t.Fatalf("set base domain: %v", err)
	}
	svc.ReconcileCertificates(ctx)
	values, err = svc.settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := CertLastError(values); got != certSealKeyMessage {
		t.Fatalf("phase 2: cert_last_error = %q, want the seal-key message %q (stale or premature clear?)", got, certSealKeyMessage)
	}
	// "stays empty" (per the F1.1 spec wording): a pass that cannot seal the
	// CA key must not have persisted any CA material.
	if ca, err := svc.CertificateCAView(ctx); err != nil || ca.Present {
		t.Fatalf("phase 2: CertificateCAView.Present = %v (err %v), want no CA to exist", ca.Present, err)
	}
	if certs, err := svc.CertificatesView(ctx); err != nil || len(certs) != 0 {
		t.Fatalf("phase 2: CertificatesView = %+v (err %v), want zero certificates", certs, err)
	}

	// Phase 3: give the service a working seal path (the CERTIFICATE cipher --
	// the capture cipher would not help) and reconcile again: now BOTH gates
	// pass, and the note must clear.
	svc.cert.cipher = newTestCipher(t)
	svc.ReconcileCertificates(ctx)
	values, err = svc.settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := CertLastError(values); got != "" {
		t.Fatalf("phase 3: cert_last_error = %q, want empty after a pass that gets past both gates", got)
	}
	if ca, err := svc.CertificateCAView(ctx); err != nil || !ca.Present || ca.LastError != "" {
		t.Fatalf("phase 3: CertificateCAView = %+v (err %v), want a present CA with no last_error", ca, err)
	}
}

// TestAcmeHTTPClientHasABoundedTimeout pins the one guarantee behind
// acmeHTTPClient (extracted so both certissue.ACMEClient construction sites
// in issueCertificate and accountFor share it): the returned client is
// non-nil and carries a POSITIVE timeout equal to acmeHTTPClientTimeout.
//
// This does NOT re-test that net/http honors http.Client.Timeout -- that is
// the standard library's own contract, not this code's -- it tests only that
// THIS package's client-construction site actually sets one. Without it, a
// future edit that drops the HTTPClient field (or passes &http.Client{}, or
// any other bare client with no Timeout) would silently fall back to
// certissue.ACMEClient's own zero-value behavior -- http.DefaultClient,
// which has NO timeout at all (see acmeHTTPClientTimeout's doc comment) --
// and a single wedged TCP connection to the ACME directory could then stall
// a reconcile pass for as long as its context allows, with nothing in this
// package's own tests pointing at the cause.
func TestAcmeHTTPClientHasABoundedTimeout(t *testing.T) {
	client := acmeHTTPClient()
	if client == nil {
		t.Fatal("acmeHTTPClient() = nil, want a non-nil *http.Client")
	}
	if client.Timeout <= 0 {
		t.Fatalf("acmeHTTPClient().Timeout = %s, want > 0 (http.DefaultClient-equivalent, i.e. unbounded, is not acceptable)", client.Timeout)
	}
	if client.Timeout != acmeHTTPClientTimeout {
		t.Fatalf("acmeHTTPClient().Timeout = %s, want the package constant acmeHTTPClientTimeout (%s)", client.Timeout, acmeHTTPClientTimeout)
	}
}
