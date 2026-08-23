// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/certissue"
	"op-ai-gateway/internal/routing"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestModeForPicksTheEdgeModePerRow(t *testing.T) {
	set := CertSettings{IssuerMode: IssuerModeACME, EdgeIssuerMode: IssuerModeSelfSigned}
	if got := set.modeFor("edge"); got != IssuerModeSelfSigned {
		t.Fatalf("modeFor(edge) = %q, want self_signed", got)
	}
	for _, kind := range []string{"gateway", "server", "public"} {
		if got := set.modeFor(kind); got != IssuerModeACME {
			t.Fatalf("modeFor(%s) = %q, want acme", kind, got)
		}
	}
	// And the mirror direction: the edge row must follow ITS own mode even when
	// that is the acme one while the internal names are self-signed.
	flipped := CertSettings{IssuerMode: IssuerModeSelfSigned, EdgeIssuerMode: IssuerModeACME}
	if got := flipped.modeFor(certEdgeKind); got != IssuerModeACME {
		t.Fatalf("modeFor(edge) = %q with a self_signed internal mode, want acme", got)
	}
	if got := flipped.modeFor("server"); got != IssuerModeSelfSigned {
		t.Fatalf("modeFor(server) = %q, want self_signed", got)
	}
}

func TestEdgeDesiredIsOffUntilBothTheSwitchAndANameAreSet(t *testing.T) {
	if _, ok := edgeDesired(CertSettings{EdgeEnabled: false, EdgeNames: []string{"edge.lan"}}); ok {
		t.Fatal("the switch being off must yield no edge row")
	}
	if _, ok := edgeDesired(CertSettings{EdgeEnabled: true}); ok {
		t.Fatal("no configured name must yield no edge row")
	}
	if _, ok := edgeDesired(CertSettings{EdgeEnabled: true, EdgeNames: []string{"  ", ""}}); ok {
		t.Fatal("a name list that is blank after trimming must yield no edge row")
	}
	got, ok := edgeDesired(CertSettings{EdgeEnabled: true, EdgeNames: []string{" Edge.LAN ", "10.0.0.5"}})
	if !ok {
		t.Fatal("an enabled switch with names must yield an edge row")
	}
	// The FIRST name is the row's identity (its primary key + Subject CN).
	if got.Domain != "edge.lan" || got.Kind != certEdgeKind {
		t.Fatalf("edgeDesired = %+v, want the normalized first name as the edge row's domain", got)
	}
	if strings.Join(got.Names, ",") != "edge.lan,10.0.0.5" {
		t.Fatalf("edgeDesired names = %v, want the full normalized SAN list in order", got.Names)
	}
}

// certissue.SplitNames ERRORS on a duplicate name, and the settings write path does
// not deduplicate (each entry is individually valid). Without a dedupe here, a list
// like "edge.lan,edge.lan" would fail every issuance with "certissue: duplicate
// name" and sit in backoff forever.
func TestEdgeDesiredDeduplicatesNamesKeepingTheFirst(t *testing.T) {
	got, ok := edgeDesired(CertSettings{
		EdgeEnabled: true,
		EdgeNames:   []string{"edge.lan", " EDGE.LAN ", "10.0.0.5", "edge.lan", "10.0.0.5"},
	})
	if !ok {
		t.Fatal("a duplicated list must still yield an edge row")
	}
	if strings.Join(got.Names, ",") != "edge.lan,10.0.0.5" {
		t.Fatalf("edgeDesired names = %v, want each name exactly once in first-seen order", got.Names)
	}
	if got.Domain != "edge.lan" {
		t.Fatalf("edgeDesired domain = %q, want the first occurrence to stay the primary name", got.Domain)
	}
	// And the deduped list is actually issuable (the whole point).
	if _, _, err := certissue.SplitNames(got.Names); err != nil {
		t.Fatalf("SplitNames on the normalized list: %v", err)
	}
	// A list that is ONLY duplicates of one name still collapses to that one name.
	single, ok := edgeDesired(CertSettings{EdgeEnabled: true, EdgeNames: []string{"a.lan", "a.lan"}})
	if !ok || len(single.Names) != 1 || single.Names[0] != "a.lan" {
		t.Fatalf("edgeDesired = %+v (ok %v), want a single a.lan", single, ok)
	}
}

// enableEdge turns the edge certificate on with the given issuer mode and names.
func enableEdge(t *testing.T, svc *Service, ctx context.Context, mode string, names ...string) {
	t.Helper()
	on := true
	list := names
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEdgeEnabled:    &on,
		CertEdgeIssuerMode: &mode,
		CertEdgeNames:      &list,
	}); err != nil {
		t.Fatalf("enable edge: %v", err)
	}
}

// recordingIssuer wraps svc.cert.issuer so a test can see exactly which rows the
// reconcile tried to issue while still exercising the REAL issuance path
// underneath (so IssuerFingerprint, SANs and validity are the genuine article,
// not a stub's fiction).
func recordingIssuer(svc *Service, seen *[]desiredCert) func(context.Context, CertSettings, desiredCert) (certissue.Result, error) {
	real := svc.issueCertificate
	return func(ctx context.Context, set CertSettings, want desiredCert) (certissue.Result, error) {
		*seen = append(*seen, want)
		return real(ctx, set, want)
	}
}

// 1. The entry case from the spec's table: internal acme + edge self_signed. The
// edge row must be ISSUED (not "skipped: not under base domain"), and it must be
// signed by the internal CA even though the global mode is acme.
func TestReconcileIssuesTheEdgeRowWhileTheInternalModeIsACME(t *testing.T) {
	svc, ctx := certEnv(t)
	enableACME(t, svc, ctx, "all")
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan", "10.0.0.5")
	var seen []desiredCert
	svc.cert.issuer = recordingIssuer(svc, &seen)

	svc.ReconcileCertificates(ctx)

	if len(seen) != 1 {
		t.Fatalf("issued %d rows, want exactly the edge row: %+v", len(seen), seen)
	}
	if seen[0].Kind != certEdgeKind {
		t.Fatalf("issued kind %q, want %q", seen[0].Kind, certEdgeKind)
	}
	if strings.Join(seen[0].Names, ",") != "edge.lan,10.0.0.5" {
		t.Fatalf("issued names %v, want both configured names", seen[0].Names)
	}
	stored, err := svc.routes.CertificateByDomain(ctx, "edge.lan")
	if err != nil {
		t.Fatalf("the edge certificate was not stored: %v", err)
	}
	if stored.Status != "active" {
		t.Fatalf("edge status = %q (last_error %q), want active", stored.Status, stored.LastError)
	}
	// A non-empty issuer fingerprint is the proof it was signed by the INTERNAL CA
	// -- the whole point of a per-row issuer mode. In the acme mode this is "".
	if stored.IssuerFingerprint == "" {
		t.Fatal("the edge leaf carries no issuer fingerprint, so it was not signed by the internal CA")
	}
	// And the leaf really covers both SANs (the IP included).
	leaf := parseLeaf(stored.FullchainPEM)
	if leaf == nil {
		t.Fatal("the stored edge chain does not parse")
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "edge.lan" {
		t.Fatalf("leaf DNS names = %v, want [edge.lan]", leaf.DNSNames)
	}
	if len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "10.0.0.5" {
		t.Fatalf("leaf IP SANs = %v, want [10.0.0.5]", leaf.IPAddresses)
	}
	// No row may claim it was skipped for being outside the base domain: that gate
	// is issuer-mode-blind if it reads the GLOBAL mode instead of the row's.
	certs, err := svc.CertificatesView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range certs {
		if c.Status == "skipped" {
			t.Fatalf("row %q was skipped (%q); the edge row must not be judged by the internal mode", c.Domain, c.LastError)
		}
	}
	// A fully-configured edge must leave NO module-level note. Without the
	// !edgeWanted half of the note's condition, every successful pass would stamp a
	// permanent, false "edge names missing" alert into the portal.
	values, err := svc.settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := CertLastError(values); got != "" {
		t.Fatalf("cert_last_error = %q after a successful edge pass, want it empty", got)
	}
}

// 1b. The other half of the same gate: an edge row whose OWN mode is acme must
// still bypass the base-domain rule (that rule exists for the NetBird wildcard A
// record, which has nothing to do with the gateway's own public edge name) -- so
// the kind carve-out has to be there in addition to the per-row mode.
func TestReconcileDoesNotJudgeTheACMEEdgeRowByTheBaseDomain(t *testing.T) {
	svc, ctx := certEnv(t)
	enableACME(t, svc, ctx, "all")
	// Deliberately NOT under cert_base_domain (int.example.test).
	enableEdge(t, svc, ctx, IssuerModeACME, "gw.public.example.com")
	now := time.Now().UTC()
	var seen []desiredCert
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		seen = append(seen, want)
		return selfSigned(t, want.Domain, now.Add(-time.Hour), 90*24*time.Hour), nil
	}

	svc.ReconcileCertificates(ctx)

	if len(seen) != 1 || seen[0].Kind != certEdgeKind {
		t.Fatalf("issued %+v, want an attempt for the acme edge row rather than a skip", seen)
	}
	stored, err := svc.routes.CertificateByDomain(ctx, "gw.public.example.com")
	if err != nil {
		t.Fatalf("the edge certificate was not stored: %v", err)
	}
	if stored.Status != "active" {
		t.Fatalf("edge status = %q (%q), want active", stored.Status, stored.LastError)
	}
}

// deleteSpyStore fails the test on any certificate delete. A "row still present"
// assertion alone would pass even if the code deleted and re-created it -- and the
// sealed private key would be gone.
type deleteSpyStore struct {
	routing.Store
	t      *testing.T
	domain string
}

func (d deleteSpyStore) DeleteCertificate(ctx context.Context, domain string) error {
	if domain == d.domain {
		d.t.Fatalf("reconcile deleted certificate %q while the edge switch was off", domain)
		return nil
	}
	return d.Store.DeleteCertificate(ctx, domain)
}

func (d deleteSpyStore) UpsertCertificate(ctx context.Context, cert routing.Certificate) error {
	if cert.Domain == d.domain {
		d.t.Fatalf("reconcile rewrote dormant certificate %q while the edge switch was off", cert.Domain)
		return nil
	}
	return d.Store.UpsertCertificate(ctx, cert)
}

// 2. THE most important test of the plan. A dormant edge row (switch off, or its
// name list momentarily empty/mistyped) is STALE, not unwanted: deleting it
// destroys the sealed private key and forces a fresh order against the weekly
// duplicate limit.
func TestReconcileNeverPrunesTheEdgeRow(t *testing.T) {
	svc, ctx := certEnv(t)
	// The certificate module itself stays on with a base domain so the pass really
	// runs; the EDGE switch is never enabled (cert_edge_enabled unset).
	enableACME(t, svc, ctx, "all")
	now := time.Now().UTC()
	seedChain := selfSigned(t, "edge.lan", now.Add(-time.Hour), 90*24*time.Hour).FullchainPEM
	const seedKey = "plain:EDGEKEY"
	// An edge row exists from an earlier, enabled pass ...
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain:       "edge.lan",
		Kind:         certEdgeKind,
		FullchainPEM: seedChain,
		KeySealed:    seedKey,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(90 * 24 * time.Hour),
		Status:       "active",
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A genuinely unwanted NON-edge row proves the exception is narrow rather than
	// a blanket "stop pruning".
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: "gone.int.example.test", Kind: "server", Status: "active",
		FullchainPEM: seedChain, KeySealed: seedKey,
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(60 * 24 * time.Hour),
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc.routes = deleteSpyStore{Store: svc.routes, t: t, domain: "edge.lan"}

	svc.ReconcileCertificates(ctx)

	certs, err := svc.routes.Certificates(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, c := range certs {
		if c.Domain == "edge.lan" {
			found = true
			if c.KeySealed != seedKey || c.FullchainPEM != seedChain {
				t.Fatal("the dormant edge row was rewritten; its material must stay byte-identical")
			}
		}
	}
	if !found {
		t.Fatal("the dormant edge row disappeared")
	}
	if _, err := svc.routes.CertificateByDomain(ctx, "gone.int.example.test"); err == nil {
		t.Fatal("a genuinely unwanted server row must still be pruned")
	}
}

// 3. SAN drift: the stored leaf covers one name, the settings now list two.
func TestReconcileReissuesWhenTheEdgeNamesChanged(t *testing.T) {
	svc, ctx := certEnv(t)
	enableACME(t, svc, ctx, "all")
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan")
	now := time.Now().UTC()
	// A stored leaf covering ONLY edge.lan, with a NotAfter far in the future so
	// renewDue alone would NOT fire (a year of validity, 30-day renew window).
	seed := selfSigned(t, "edge.lan", now.Add(-time.Hour), 365*24*time.Hour)
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: "edge.lan", Kind: certEdgeKind, Status: "active",
		FullchainPEM: seed.FullchainPEM, KeySealed: "plain:EDGEKEY",
		Fingerprint: seed.Fingerprint,
		NotBefore:   seed.Leaf.NotBefore.UTC(), NotAfter: seed.Leaf.NotAfter.UTC(),
		IssuedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Precondition: with the name set UNCHANGED nothing is due, so a pass that does
	// re-issue below can only have been driven by the drift.
	stored, err := svc.routes.CertificateByDomain(ctx, "edge.lan")
	if err != nil {
		t.Fatal(err)
	}
	if renewDue(stored, 30, now, "edge.lan", "") {
		t.Fatal("precondition: the seeded certificate must not already be due on age")
	}
	if sanDrift(stored, []string{"edge.lan"}) {
		t.Fatal("precondition: an unchanged name set must not read as drift")
	}

	// Now the operator adds a second name.
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan", "10.0.0.5")
	var seen []desiredCert
	svc.cert.issuer = recordingIssuer(svc, &seen)

	svc.ReconcileCertificates(ctx)

	if len(seen) != 1 || seen[0].Kind != certEdgeKind {
		t.Fatalf("issued %+v, want the edge row re-issued because its SAN set changed", seen)
	}
	if strings.Join(seen[0].Names, ",") != "edge.lan,10.0.0.5" {
		t.Fatalf("re-issued names %v, want both configured names", seen[0].Names)
	}
	after, err := svc.routes.CertificateByDomain(ctx, "edge.lan")
	if err != nil {
		t.Fatal(err)
	}
	if sanDrift(after, []string{"edge.lan", "10.0.0.5"}) {
		t.Fatal("after the re-issue the stored leaf must cover the configured name set")
	}
}

// 4. An edge-only deployment: no NetBird module, no cert_base_domain. The
// base-domain abort exists for the INTERNAL names (the ACME under-base rule and
// the CA subject suffix) and must not kill a pass that only wants the edge row.
func TestReconcileIssuesTheEdgeRowWithoutABaseDomain(t *testing.T) {
	svc, ctx := certEnv(t)
	on := true
	mode := IssuerModeACME
	email := "ops@example.test"
	base := ""
	gw := ""
	scope := "all"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:       &on,
		CertIssuerMode:    &mode,
		ACMEEmail:         &email,
		CertBaseDomain:    &base,
		CertGatewayDomain: &gw,
		CertServerScope:   &scope,
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan")
	// Precondition: nothing can supply a base domain (NetBird is off).
	if got := svc.netbirdDNSDomain(ctx); got != "" {
		t.Fatalf("precondition: netbirdDNSDomain = %q, want no base domain available", got)
	}
	var seen []desiredCert
	svc.cert.issuer = recordingIssuer(svc, &seen)

	svc.ReconcileCertificates(ctx)

	if len(seen) != 1 || seen[0].Kind != certEdgeKind {
		t.Fatalf("issued %+v, want the edge row despite there being no base domain", seen)
	}
	stored, err := svc.routes.CertificateByDomain(ctx, "edge.lan")
	if err != nil {
		t.Fatalf("the edge certificate was not stored: %v", err)
	}
	if stored.Status != "active" {
		t.Fatalf("edge status = %q (%q), want active", stored.Status, stored.LastError)
	}
	values, err := svc.settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := CertLastError(values); got == certNoBaseDomainMessage {
		t.Fatal("the pass aborted on the base-domain gate even though only the edge row was wanted")
	}
}

// 5. The gap the plan did not cover: CertSettings reports ok=false as soon as the
// INTERNAL mode's mandatory field is missing, and the internal mode defaults to
// acme. An operator who wants ONLY the edge certificate (module on, internal mode
// left at its default, no acme_email, edge self_signed) would otherwise get
// nothing issued, silently. ok=false must mean "the internal names are not
// servable", not "do nothing" -- and the desired set must then contain ONLY the
// edge row, because ordering an internal name without the mandatory field would
// only burn failed orders.
func TestReconcileIssuesTheEdgeRowWhileTheInternalModeIsUnconfigured(t *testing.T) {
	svc, ctx := certEnv(t)
	on := true
	mode := IssuerModeACME
	email := "" // the internal acme mode's mandatory field is MISSING
	base := "int.example.test"
	gw := ""
	scope := "all"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:       &on,
		CertIssuerMode:    &mode,
		ACMEEmail:         &email,
		CertBaseDomain:    &base,
		CertGatewayDomain: &gw,
		CertServerScope:   &scope,
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan")
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	// Precondition: this really is the ok=false state.
	if _, ok, err := svc.CertSettings(ctx); err != nil || ok {
		t.Fatalf("precondition: CertSettings ok = %v (err %v), want false (no acme email)", ok, err)
	}
	var seen []desiredCert
	svc.cert.issuer = recordingIssuer(svc, &seen)

	svc.ReconcileCertificates(ctx)

	if len(seen) != 1 || seen[0].Kind != certEdgeKind {
		t.Fatalf("issued %+v, want ONLY the edge row", seen)
	}
	stored, err := svc.routes.CertificateByDomain(ctx, "edge.lan")
	if err != nil {
		t.Fatalf("the edge certificate was not stored: %v", err)
	}
	if stored.Status != "active" {
		t.Fatalf("edge status = %q (%q), want active", stored.Status, stored.LastError)
	}
	// The internal name must not even be attempted: without the mandatory field it
	// could only produce failed orders.
	if _, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test"); err == nil {
		t.Fatal("an internal row was attempted although its issuer mode is not configured")
	}
}

// 5c. The hazard the ok=false continuation introduces: with the internal names
// left OUT of the desired set, the prune loop would see every existing internal
// row as unwanted and delete it -- destroying its sealed private key. Their
// absence is a consequence of the missing setting, not proof that they became
// unwanted, so they must be kept.
func TestReconcileKeepsInternalRowsWhenTheirIssuerModeIsUnconfigured(t *testing.T) {
	svc, ctx := certEnv(t)
	// Start from a working acme configuration and issue a real internal row ...
	enableACME(t, svc, ctx, "all")
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	now := time.Now().UTC()
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		return selfSigned(t, want.Domain, now.Add(-time.Hour), 90*24*time.Hour), nil
	}
	svc.ReconcileCertificates(ctx)
	before, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatalf("precondition: the internal row must exist first: %v", err)
	}
	// ... then the operator clears the acme email (ok=false) and turns the edge on.
	email := ""
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{ACMEEmail: &email}); err != nil {
		t.Fatalf("clear email: %v", err)
	}
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan")
	if _, ok, err := svc.CertSettings(ctx); err != nil || ok {
		t.Fatalf("precondition: CertSettings ok = %v (err %v), want false", ok, err)
	}
	svc.cert.issuer = svc.issueCertificate

	svc.ReconcileCertificates(ctx)

	after, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatalf("the internal row was pruned although its absence only reflects a missing setting: %v", err)
	}
	if after.FullchainPEM != before.FullchainPEM || after.KeySealed != before.KeySealed {
		t.Fatal("the internal row's material must stay byte-identical")
	}
}

// 5d. The per-row issuer fingerprint. Stamping an ACME row with the internal
// root's fingerprint would make renewDue's issuer-mismatch clause fire on every
// pass (re-ordering forever, straight into the weekly duplicate limit); stamping a
// self_signed row with "" would make a CA rotation go unnoticed.
func TestReconcileStampsTheIssuerFingerprintPerRow(t *testing.T) {
	svc, ctx := certEnv(t)
	enableACME(t, svc, ctx, "all")
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan")
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	now := time.Now().UTC()
	// The edge row goes through the REAL internal CA; the acme row through a stub
	// (there is no live directory here).
	real := svc.issueCertificate
	svc.cert.issuer = func(ctx context.Context, set CertSettings, want desiredCert) (certissue.Result, error) {
		if want.Kind == certEdgeKind {
			return real(ctx, set, want)
		}
		return selfSigned(t, want.Domain, now.Add(-time.Hour), 90*24*time.Hour), nil
	}

	svc.ReconcileCertificates(ctx)

	edge, err := svc.routes.CertificateByDomain(ctx, "edge.lan")
	if err != nil {
		t.Fatal(err)
	}
	if edge.IssuerFingerprint == "" {
		t.Fatal("the self_signed edge row must carry the internal root's fingerprint")
	}
	server, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if server.IssuerFingerprint != "" {
		t.Fatalf("the acme row carries issuer fingerprint %q, want empty -- otherwise the "+
			"issuer-mismatch rule re-orders it on every pass", server.IssuerFingerprint)
	}
	// And the consequence, stated directly: neither row is due again right away.
	ca, err := svc.CertificateCAView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if renewDue(server, 30, now, server.Domain, "") {
		t.Fatal("the fresh acme row must not be due again")
	}
	if renewDue(edge, 30, now, edge.Domain, ca.Fingerprint) {
		t.Fatal("the fresh edge row must not be due again")
	}
}

// 5e. A changed FIRST name renames the row. The superseded row is removed ONLY
// after the new one is safely stored -- and a FAILED issuance must leave the old,
// still-valid certificate (and its sealed key) completely alone.
func TestReconcileRemovesTheSupersededEdgeRowOnlyAfterASuccessfulRename(t *testing.T) {
	svc, ctx := certEnv(t)
	enableACME(t, svc, ctx, "all")
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "old.lan")
	svc.cert.issuer = svc.issueCertificate
	svc.ReconcileCertificates(ctx)
	old, err := svc.routes.CertificateByDomain(ctx, "old.lan")
	if err != nil {
		t.Fatalf("precondition: the first edge row must exist: %v", err)
	}

	// The rename is attempted but issuance FAILS: the old row must survive intact.
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "new.lan")
	svc.cert.issuer = func(context.Context, CertSettings, desiredCert) (certissue.Result, error) {
		return certissue.Result{}, errors.New("ca: temporarily unavailable")
	}
	svc.ReconcileCertificates(ctx)
	kept, err := svc.routes.CertificateByDomain(ctx, "old.lan")
	if err != nil {
		t.Fatalf("a FAILED rename must not remove the old edge row: %v", err)
	}
	if kept.FullchainPEM != old.FullchainPEM || kept.KeySealed != old.KeySealed {
		t.Fatal("a failed rename must not touch the old row's material")
	}

	// Now it succeeds: the old row is superseded and goes away.
	svc.cert.issuer = svc.issueCertificate
	// Clear the failure backoff recorded on the NEW row so the retry actually runs.
	if err := svc.RenewCertificateNow(ctx, systemToken(), "new.lan"); err != nil {
		t.Fatalf("clear backoff: %v", err)
	}
	svc.ReconcileCertificates(ctx)
	fresh, err := svc.routes.CertificateByDomain(ctx, "new.lan")
	if err != nil {
		t.Fatalf("the renamed edge row was not issued: %v", err)
	}
	if fresh.Status != "active" {
		t.Fatalf("new.lan status = %q (%q), want active", fresh.Status, fresh.LastError)
	}
	if _, err := svc.routes.CertificateByDomain(ctx, "old.lan"); err == nil {
		t.Fatal("the superseded edge row must be removed once its replacement is stored")
	}
}

// 5f. The rename prune must never remove a row that is STILL WANTED. A stored row
// can carry kind='edge' while its domain is simultaneously a wanted INTERNAL name:
// edgeDesired is added LAST, so on a collision add's first-wins rule drops the edge
// entry and the stored row keeps kind='edge' until its next actual re-issue -- up to
// a full certificate lifetime. Deleting it would destroy a still-wanted certificate
// and its unrecoverable sealed key.
func TestReconcilePruneOfASupersededEdgeRowSparesAStillWantedName(t *testing.T) {
	svc, ctx := certEnv(t)
	enableACME(t, svc, ctx, "all")
	now := time.Now().UTC()
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		return selfSigned(t, want.Domain, now.Add(-time.Hour), 90*24*time.Hour), nil
	}
	// 1. The operator names the gateway's own hostname as the edge name (obvious
	// intent) while cert_gateway_domain is still empty.
	enableEdge(t, svc, ctx, IssuerModeACME, "gw.int.example.test")
	svc.ReconcileCertificates(ctx)
	seeded, err := svc.routes.CertificateByDomain(ctx, "gw.int.example.test")
	if err != nil {
		t.Fatalf("precondition: the edge row must be issued first: %v", err)
	}
	if seeded.Kind != certEdgeKind {
		t.Fatalf("precondition: stored kind = %q, want %q", seeded.Kind, certEdgeKind)
	}

	// 2. The same name later becomes the WANTED GATEWAY name, and the edge names move
	// elsewhere. The stored row still says kind='edge' (it is fresh, so nothing
	// re-stamps it), which is exactly the trap.
	gw := "gw.int.example.test"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertGatewayDomain: &gw}); err != nil {
		t.Fatalf("set gateway domain: %v", err)
	}
	enableEdge(t, svc, ctx, IssuerModeACME, "other.int.example.test")

	svc.ReconcileCertificates(ctx)

	kept, err := svc.routes.CertificateByDomain(ctx, "gw.int.example.test")
	if err != nil {
		t.Fatalf("a still-wanted name was deleted by the superseded-edge prune: %v", err)
	}
	if kept.FullchainPEM != seeded.FullchainPEM || kept.KeySealed != seeded.KeySealed {
		t.Fatal("the still-wanted row's material must stay byte-identical")
	}
	// And the genuine rename still happened.
	if fresh, err := svc.routes.CertificateByDomain(ctx, "other.int.example.test"); err != nil || fresh.Status != "active" {
		t.Fatalf("the new edge row = %+v (err %v), want active", fresh, err)
	}
}

// 5b. The mirror of 5: with the edge row NOT usable on its own either, ok=false
// must keep behaving exactly as before (return immediately, touch nothing).
func TestReconcileStillReturnsEarlyWhenNeitherModeIsUsable(t *testing.T) {
	svc, ctx := certEnv(t)
	on := true
	mode := IssuerModeACME
	email := ""
	base := "int.example.test"
	scope := "all"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:     &on,
		CertIssuerMode:  &mode,
		ACMEEmail:       &email,
		CertBaseDomain:  &base,
		CertServerScope: &scope,
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	// The edge switch is ON with an acme mode and NO email -> the edge mode's own
	// mandatory field is missing too, so nothing is usable.
	enableEdge(t, svc, ctx, IssuerModeACME, "gw.public.example.com")
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		t.Fatalf("issued %q although no issuer mode is usable", want.Domain)
		return certissue.Result{}, nil
	}
	// The store spy fails on ANY certificate access, so this pins "touches nothing"
	// rather than merely "wrote nothing observable".
	svc.routes = certStoreSpy{t: t}
	svc.ReconcileCertificates(ctx)
}

// Step 5b of the brief: the switch on WITHOUT names issues nothing but must leave
// a visible reason rather than failing silently.
func TestReconcileNotesAnEnabledEdgeWithoutNames(t *testing.T) {
	svc, ctx := certEnv(t)
	enableACME(t, svc, ctx, "all")
	on := true
	empty := []string{}
	mode := IssuerModeSelfSigned
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEdgeEnabled:    &on,
		CertEdgeIssuerMode: &mode,
		CertEdgeNames:      &empty,
	}); err != nil {
		t.Fatalf("enable edge without names: %v", err)
	}
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		t.Fatalf("issued %q although the edge name list is empty", want.Domain)
		return certissue.Result{}, nil
	}

	svc.ReconcileCertificates(ctx)

	values, err := svc.settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := CertLastError(values); got != certEdgeNamesMissingMessage {
		t.Fatalf("cert_last_error = %q, want %q -- an enabled-but-unconfigured edge must be visible, not silent",
			got, certEdgeNamesMissingMessage)
	}
}

// issueCertificate must still work for a desiredCert built WITHOUT an explicit SAN
// list. desiredCertificates always fills Names in, but issueAndStore is also called
// directly (tests today, and any later caller constructing a row by hand), and an
// empty list would fail in SplitNames rather than issuing for the row's domain.
func TestIssueCertificateFallsBackToTheRowsDomainWhenNoSANListIsGiven(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 90)
	// Create the internal CA (the reconcile's ensureCA does this).
	svc.cert.issuer = svc.issueCertificate
	svc.ReconcileCertificates(ctx)
	if ca, err := svc.CertificateCAView(ctx); err != nil || !ca.Present {
		t.Fatalf("precondition: an internal CA must exist: %+v (err %v)", ca, err)
	}

	set, ok, err := svc.CertSettings(ctx)
	if err != nil || !ok {
		t.Fatalf("settings not usable: ok=%v err=%v", ok, err)
	}
	res, err := svc.issueCertificate(ctx, set, desiredCert{Domain: "bare.int.example.test", Kind: "server"})
	if err != nil {
		t.Fatalf("issueCertificate without a SAN list: %v", err)
	}
	if res.Leaf == nil {
		t.Fatal("no leaf returned")
	}
	if len(res.Leaf.DNSNames) != 1 || res.Leaf.DNSNames[0] != "bare.int.example.test" {
		t.Fatalf("leaf DNS names = %v, want the row's own domain", res.Leaf.DNSNames)
	}
}

// pruneSupersededEdgeRows' contract for a caller that has no desired set: not
// knowing what is wanted is never a reason to delete a private key.
func TestPruneSupersededEdgeRowsSkipsEverythingWithoutADesiredSet(t *testing.T) {
	svc, ctx := certEnv(t)
	now := time.Now().UTC()
	seed := selfSigned(t, "stale.lan", now.Add(-time.Hour), 90*24*time.Hour)
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: "stale.lan", Kind: certEdgeKind, Status: "active",
		FullchainPEM: seed.FullchainPEM, KeySealed: "plain:EDGEKEY",
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(90 * 24 * time.Hour),
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc.pruneSupersededEdgeRows(ctx, "other.lan", nil)

	if _, err := svc.routes.CertificateByDomain(ctx, "stale.lan"); err != nil {
		t.Fatalf("a nil desired set must prune NOTHING, but the row was deleted: %v", err)
	}
	// With a real (empty) desired set the same call does prune -- the guard keys on
	// nil, not on emptiness, so it cannot silently disable the prune entirely.
	svc.pruneSupersededEdgeRows(ctx, "other.lan", map[string]desiredCert{})
	if _, err := svc.routes.CertificateByDomain(ctx, "stale.lan"); err == nil {
		t.Fatal("with a known desired set the superseded row must still be pruned")
	}
}

func TestSanDriftComparesTheStoredLeafAsASet(t *testing.T) {
	now := time.Now().UTC()
	// A leaf that covers exactly one DNS name.
	one := selfSigned(t, "edge.lan", now.Add(-time.Hour), 90*24*time.Hour)
	cert := routing.Certificate{FullchainPEM: one.FullchainPEM}
	if sanDrift(cert, []string{"edge.lan"}) {
		t.Fatal("an identical single-name set must not read as drift")
	}
	if sanDrift(cert, []string{"EDGE.LAN"}) {
		t.Fatal("the comparison must be case-insensitive")
	}
	if !sanDrift(cert, []string{"edge.lan", "10.0.0.5"}) {
		t.Fatal("an added name must read as drift")
	}
	if !sanDrift(cert, []string{"other.lan"}) {
		t.Fatal("a replaced name must read as drift")
	}
	// Nothing stored (or unparseable) makes no claim -- renewDue handles that row.
	if sanDrift(routing.Certificate{}, []string{"edge.lan"}) {
		t.Fatal("an empty stored chain must not read as drift")
	}
	if sanDrift(routing.Certificate{FullchainPEM: "not pem"}, []string{"edge.lan"}) {
		t.Fatal("an unparseable stored chain must not read as drift")
	}
}

// seedEdgeCertificate stores an active, ready-to-deliver edge row through the
// SAME seal path DeliverEdgeCertificate reads back (svc.sealCertSecret), so
// the test exercises the real seal/open round-trip rather than a hand-rolled
// "plain:" literal.
func seedEdgeCertificate(t *testing.T, svc *Service, ctx context.Context, domain string) certissue.Result {
	t.Helper()
	now := time.Now().UTC()
	res := selfSigned(t, domain, now.Add(-time.Hour), 90*24*time.Hour)
	sealed, err := svc.sealCertSecret(res.KeyPEM)
	if err != nil {
		t.Fatalf("sealCertSecret: %v", err)
	}
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: domain, Kind: certEdgeKind, Status: "active",
		FullchainPEM: res.FullchainPEM, KeySealed: sealed,
		Fingerprint: res.Fingerprint,
		NotBefore:   res.Leaf.NotBefore.UTC(), NotAfter: res.Leaf.NotAfter.UTC(),
		IssuedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed edge certificate: %v", err)
	}
	return res
}

// 1. The core delivery contract: atomic (no leftover temp file), the right
// file modes, and -- the mutation target -- a SECOND delivery of unchanged
// material must not rewrite the file (the nginx reload watcher polls the
// fingerprint; a needless rewrite would trigger a needless reload).
func TestDeliverEdgeCertificateWritesAtomicallyAndOnlyOnChange(t *testing.T) {
	dir := t.TempDir()
	svc, ctx := certEnv(t)
	svc.cert.edgeOutputDir = dir
	seedEdgeCertificate(t, svc, ctx, "edge.lan")

	if err := svc.DeliverEdgeCertificate(ctx); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	chain, err := os.ReadFile(filepath.Join(dir, edgeChainFile))
	if err != nil || !strings.Contains(string(chain), "BEGIN CERTIFICATE") {
		t.Fatalf("fullchain not written: %v", err)
	}
	chainInfo, err := os.Stat(filepath.Join(dir, edgeChainFile))
	if err != nil {
		t.Fatalf("stat fullchain: %v", err)
	}
	if perm := chainInfo.Mode().Perm(); perm != 0o644 {
		t.Fatalf("fullchain mode = %v, want 0644", perm)
	}
	keyInfo, err := os.Stat(filepath.Join(dir, edgeKeyFile))
	if err != nil {
		t.Fatalf("key not written: %v", err)
	}
	if perm := keyInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key mode = %v, want 0600", perm)
	}
	// No leftover temp files -- an atomic write renames, it does not litter.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	// The CA-bundle file must NOT appear: certEnv is in the acme-shaped default
	// (no self_signed CA was ever created), so CertificateCABundlePEM has
	// nothing to hand back.
	if _, err := os.Stat(filepath.Join(dir, edgeCAFile)); !os.IsNotExist(err) {
		t.Fatalf("edge-ca.pem present = %v, want ErrNotExist (no internal CA exists in this env)", err)
	}
	if !svc.EdgeDeliveryCapable() {
		t.Fatal("EdgeDeliveryCapable must be true right after a successful delivery")
	}

	// Second delivery with unchanged material must NOT rewrite (the nginx wrapper
	// polls the fingerprint; a needless rewrite would trigger a needless reload).
	beforeChain, err := os.Stat(filepath.Join(dir, edgeChainFile))
	if err != nil {
		t.Fatal(err)
	}
	beforeKey, err := os.Stat(filepath.Join(dir, edgeKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DeliverEdgeCertificate(ctx); err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	afterChain, err := os.Stat(filepath.Join(dir, edgeChainFile))
	if err != nil {
		t.Fatal(err)
	}
	afterKey, err := os.Stat(filepath.Join(dir, edgeKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if !beforeChain.ModTime().Equal(afterChain.ModTime()) {
		t.Fatal("unchanged fullchain material was rewritten")
	}
	if !beforeKey.ModTime().Equal(afterKey.ModTime()) {
		t.Fatal("unchanged key material was rewritten")
	}
}

// 2. With NO output directory configured, delivery must be a silent, capable-
// reporting no-op: that combination means "the download endpoint is the only
// path", not "something is broken".
func TestDeliverEdgeCertificateIsANoOpWithoutAnOutputDir(t *testing.T) {
	svc, ctx := certEnv(t) // certEdgeOutputDir stays ""
	if err := svc.DeliverEdgeCertificate(ctx); err != nil {
		t.Fatalf("must be a silent no-op, got %v", err)
	}
	if svc.EdgeDeliveryCapable() {
		t.Fatal("EdgeDeliveryCapable must be false without an output dir")
	}
}

// 3. Nothing stored yet (the module is on but no certificate has been issued):
// also a silent no-op, and it must not fabricate an empty file.
func TestDeliverEdgeCertificateIsANoOpWithNothingStoredYet(t *testing.T) {
	dir := t.TempDir()
	svc, ctx := certEnv(t)
	svc.cert.edgeOutputDir = dir

	if err := svc.DeliverEdgeCertificate(ctx); err != nil {
		t.Fatalf("nothing stored yet: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("delivered with nothing stored: %v", entries)
	}
	// A configured-but-so-far-unused directory reads as capable: there is no
	// recorded failure, only "nothing to deliver yet".
	if !svc.EdgeDeliveryCapable() {
		t.Fatal("EdgeDeliveryCapable must be true with a configured dir and no failure recorded")
	}
}

// 4. A key that cannot be opened (ambiguity resolution #4: an unopenable seal,
// e.g. a rotated encryption key, must record the failure and return an error --
// it must NOT delete/blank the stored row, and must NOT write a partial set of
// files). certEnv wires no certificate cipher, so an "enc:"-sealed value is
// exactly this condition (see openCertSecret).
func TestDeliverEdgeCertificateFailsClosedWhenTheKeyCannotBeOpened(t *testing.T) {
	dir := t.TempDir()
	svc, ctx := certEnv(t)
	svc.cert.edgeOutputDir = dir
	now := time.Now().UTC()
	seed := selfSigned(t, "edge.lan", now.Add(-time.Hour), 90*24*time.Hour)
	stored := routing.Certificate{
		Domain: "edge.lan", Kind: certEdgeKind, Status: "active",
		FullchainPEM: seed.FullchainPEM,
		KeySealed:    "enc:" + "cannot-be-opened-without-a-certificate-cipher",
		Fingerprint:  seed.Fingerprint,
		NotBefore:    seed.Leaf.NotBefore.UTC(), NotAfter: seed.Leaf.NotAfter.UTC(),
		IssuedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := svc.routes.UpsertCertificate(ctx, stored); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := svc.DeliverEdgeCertificate(ctx); !errors.Is(err, ErrCertKeyRequired) {
		t.Fatalf("DeliverEdgeCertificate error = %v, want ErrCertKeyRequired", err)
	}
	if svc.EdgeDeliveryCapable() {
		t.Fatal("EdgeDeliveryCapable must go false after a failed delivery")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a failed delivery must not write a partial set of files: %v", entries)
	}
	// The stored row itself must be untouched -- the failure is recorded only in
	// the in-memory edge-delivery status, never by mutating or deleting the row.
	after, err := svc.routes.CertificateByDomain(ctx, "edge.lan")
	if err != nil {
		t.Fatalf("the stored row must survive a failed delivery: %v", err)
	}
	if after.KeySealed != stored.KeySealed || after.FullchainPEM != stored.FullchainPEM {
		t.Fatal("a failed delivery must not mutate the stored certificate row")
	}
}

// 5. The no-op invariance a full reconcile pass must uphold: with the edge
// switch OFF, ReconcileCertificates must write NO file into a configured
// output directory, even though the module itself is on and busy issuing
// internal certificates.
func TestReconcileCertificatesWritesNoEdgeFileWhileTheSwitchIsOff(t *testing.T) {
	dir := t.TempDir()
	svc, ctx := certEnv(t)
	svc.cert.edgeOutputDir = dir
	enableACME(t, svc, ctx, "all")
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	now := time.Now().UTC()
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		return selfSigned(t, want.Domain, now.Add(-time.Hour), 90*24*time.Hour), nil
	}
	// The edge switch is never enabled (cert_edge_enabled unset).

	svc.ReconcileCertificates(ctx)

	// Precondition: the pass genuinely did something (an internal certificate
	// was issued) -- otherwise an empty directory would prove nothing.
	if _, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test"); err != nil {
		t.Fatalf("precondition: the internal certificate must have been issued: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the edge switch was off but the output dir received files: %v", entries)
	}
}

// 5b. The STRICTER half of the same invariance: it is not enough that
// DeliverEdgeCertificate itself has "nothing to deliver" -- ReconcileCertificates
// must not even ATTEMPT delivery while the switch is off, because a DORMANT edge
// row (real material from an earlier, since-disabled pass -- see
// TestReconcileNeverPrunesTheEdgeRow) is still sitting in the store. Without the
// edgeWanted gate at the end of ReconcileCertificates, this scenario would write
// that dormant material to disk even though the operator turned the feature off.
func TestReconcileCertificatesWritesNoEdgeFileForADormantRowWhileTheSwitchIsOff(t *testing.T) {
	dir := t.TempDir()
	svc, ctx := certEnv(t)
	seedEdgeCertificate(t, svc, ctx, "edge.lan") // a dormant row, real material
	svc.cert.edgeOutputDir = dir
	enableACME(t, svc, ctx, "all")
	// The edge switch stays off (cert_edge_enabled unset) for this whole test.

	svc.ReconcileCertificates(ctx)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a dormant edge row was delivered although the switch is off: %v", entries)
	}
}

// 6. The other half of the wiring: once the edge row IS issued by a reconcile
// pass, the files must already be on disk at the end of that SAME pass (the
// issueAndStore hook), without waiting for a second pass.
func TestReconcileCertificatesDeliversTheEdgeRowAfterIssuance(t *testing.T) {
	dir := t.TempDir()
	svc, ctx := certEnv(t)
	svc.cert.edgeOutputDir = dir
	enableACME(t, svc, ctx, "all")
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan")
	svc.cert.issuer = svc.issueCertificate

	svc.ReconcileCertificates(ctx)

	if _, err := os.Stat(filepath.Join(dir, edgeChainFile)); err != nil {
		t.Fatalf("edge-fullchain.pem was not delivered after issuance: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, edgeKeyFile)); err != nil {
		t.Fatalf("edge-key.pem was not delivered after issuance: %v", err)
	}
}

// 7. The restart case the end-of-pass call exists for: a fresh, empty volume
// must be re-filled from a certificate that ALREADY exists in the store, even
// on a pass where nothing needed issuing (renewDue is false).
func TestReconcileCertificatesRefillsAnEmptyVolumeWithoutReissuing(t *testing.T) {
	dir := t.TempDir()
	svc, ctx := certEnv(t)
	enableACME(t, svc, ctx, "all")
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan")
	seedEdgeCertificate(t, svc, ctx, "edge.lan") // NotAfter 90d out -> not due
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		t.Fatalf("issued %q although the seeded certificate is not due", want.Domain)
		return certissue.Result{}, nil
	}
	// The directory is attached only NOW, simulating a fresh volume mounted
	// after the certificate row was already issued in a previous process.
	svc.cert.edgeOutputDir = dir

	svc.ReconcileCertificates(ctx)

	if _, err := os.Stat(filepath.Join(dir, edgeChainFile)); err != nil {
		t.Fatalf("a fresh volume was not re-filled from the already-stored certificate: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Task 6: the five endpoints' service side + the proxy-config generator.
// ---------------------------------------------------------------------------

func containsName(list []string, want string) bool {
	for _, n := range list {
		if n == want {
			return true
		}
	}
	return false
}

// TestEdgeACMEChallengeNamesForwardThePublicDomainOnlyWhenManaged is the
// generator's sharpest conditional: forwarding /.well-known/acme-challenge/ for a
// domain the gateway does NOT order certificates for would hijack that path from
// the upstream proxy's own certbot -- which very likely serves that domain (and
// other foreign ones) itself. So the public domain rides along ONLY when
// cert_manage_public_domain is on, and both directions are pinned here.
func TestEdgeACMEChallengeNamesForwardThePublicDomainOnlyWhenManaged(t *testing.T) {
	svc, ctx := certEnv(t)
	// The internal acme mode is the precondition for any public/internal claim at
	// all (see TestEdgeACMEChallengeNamesRequireTheInternalACMEMode); the gateway
	// domain gives the set a second, mode-gated member to contrast against.
	set := CertSettings{
		IssuerMode:     IssuerModeACME,
		BaseDomain:     "int.example.test",
		GatewayDomain:  "gw.int.example.test",
		PublicDomains:  []string{"gw.public.example.com"},
		EdgeEnabled:    true,
		EdgeIssuerMode: IssuerModeSelfSigned,
		EdgeNames:      []string{"edge.lan"},
	}

	off, _, err := svc.edgeACMEChallengeNames(ctx, set)
	if err != nil {
		t.Fatal(err)
	}
	if containsName(off, "gw.public.example.com") {
		t.Fatalf("cert_manage_public_domain off -> the public domain must NOT be forwarded: %v", off)
	}
	if !containsName(off, "gw.int.example.test") {
		t.Fatalf("the configured gateway domain must be forwarded in acme mode: %v", off)
	}

	set.ManagePublicDomain = true
	on, _, err := svc.edgeACMEChallengeNames(ctx, set)
	if err != nil {
		t.Fatal(err)
	}
	if !containsName(on, "gw.public.example.com") {
		t.Fatalf("cert_manage_public_domain on -> the public domain must be forwarded: %v", on)
	}
}

// TestEdgeACMEChallengeNamesIncludeTheEdgeNamesOnlyInACMEMode: an HTTP-01 order
// for the edge names runs only in the acme edge mode, so only then does the
// upstream proxy have to forward their challenge path. An IP SAN is never
// included -- it can be neither ACME-validated nor matched as a vhost.
func TestEdgeACMEChallengeNamesIncludeTheEdgeNamesOnlyInACMEMode(t *testing.T) {
	svc, ctx := certEnv(t)
	set := CertSettings{
		BaseDomain:     "int.example.test",
		EdgeEnabled:    true,
		EdgeIssuerMode: IssuerModeSelfSigned,
		EdgeNames:      []string{"edge.lan", "10.0.0.5"},
	}
	got, _, err := svc.edgeACMEChallengeNames(ctx, set)
	if err != nil {
		t.Fatal(err)
	}
	if containsName(got, "edge.lan") {
		t.Fatalf("self_signed edge mode places no order -> no forwarding needed: %v", got)
	}
	set.EdgeIssuerMode = IssuerModeACME
	got, _, err = svc.edgeACMEChallengeNames(ctx, set)
	if err != nil {
		t.Fatal(err)
	}
	if !containsName(got, "edge.lan") {
		t.Fatalf("acme edge mode -> the edge name needs forwarding: %v", got)
	}
	if containsName(got, "10.0.0.5") {
		t.Fatalf("an IP SAN must never reach a server_name: %v", got)
	}
	// The edge gate is its OWN switch: the internal mode stayed self_signed here,
	// so the edge name is claimed while no internal name is.
	if containsName(got, "int.example.test") || containsName(got, "*.int.example.test") {
		t.Fatalf("the internal names must stay unclaimed while the internal mode is self_signed: %v", got)
	}
}

// TestEdgeACMEChallengeNamesRejectAnInjectionAttempt is the defensive second
// gate. The settings PUT already refuses these (ValidateEdgeName), so this
// exercises a value that could only exist if it were written some other way --
// and proves the generator omits it rather than interpolating it into a
// configuration destined for a more privileged machine.
func TestEdgeACMEChallengeNamesRejectAnInjectionAttempt(t *testing.T) {
	svc, ctx := certEnv(t)
	// Each payload after the first three isolates ONE forbidden character, so a
	// whitelist widened to accept just that character cannot stay green: the
	// combined payloads above are each rejected for several reasons at once, and a
	// hole for '#', a bare space or a newline would hide behind the ';' or the '}'
	// they also carry (final-review T2.1).
	for _, bad := range []string{
		"evil.lan; return 444",
		"evil.lan}\nserver{listen 81;",
		"evil.lan #comment",
		"edge;lan",
		"edge#lan",
		"edge lan",
		"edge\nlan",
		"edge}lan",
	} {
		set := CertSettings{
			IssuerMode:         IssuerModeACME,
			BaseDomain:         bad,
			GatewayDomain:      bad,
			ManagePublicDomain: true,
			PublicDomains:      []string{bad},
			EdgeEnabled:        true,
			EdgeIssuerMode:     IssuerModeACME,
			EdgeNames:          []string{bad},
		}
		got, _, err := svc.edgeACMEChallengeNames(ctx, set)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("%q survived validation: %v", bad, got)
		}
	}
}

// TestEdgeProxyConfigOmitsTheUnmanagedPublicDomainFromTheChallengeBlock is the
// rendered-output half of the conditional above: with the switch off the public
// domain must not appear in the :80 block at all (it still appears in the :443
// block -- that is where this proxy terminates it), so the assertion is scoped to
// the part of the file before "listen 443". It also pins the no-wildcard rule on
// the rendered output: the claimed server_name is the ENUMERATED managed server
// domain, never `*.<base>` (which would re-admit the excluded public domain).
func TestEdgeProxyConfigOmitsTheUnmanagedPublicDomainFromTheChallengeBlock(t *testing.T) {
	svc, ctx := certEnv(t)
	on, off := true, false
	// Internal mode acme: the precondition for any internal/public claim at all.
	mode := IssuerModeACME
	edgeMode := IssuerModeSelfSigned
	email := "ops@example.test"
	base := "int.example.test"
	scope := "all"
	pub := []string{"gw.public.example.com"}
	names := []string{"edge.lan"}
	mustCreateNetbirdServer(t, svc, ctx, "a", "a.int.example.test", "")
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:            &on,
		CertIssuerMode:         &mode,
		ACMEEmail:              &email,
		CertBaseDomain:         &base,
		CertServerScope:        &scope,
		CertManagePublicDomain: &off,
		CertPublicDomains:      &pub,
		CertEdgeEnabled:        &on,
		CertEdgeIssuerMode:     &edgeMode,
		CertEdgeNames:          &names,
	}); err != nil {
		t.Fatalf("settings: %v", err)
	}

	cfg, err := svc.EdgeProxyConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	challengeBlock, _, found := strings.Cut(cfg, "listen 443")
	if !found {
		t.Fatalf("no :443 block to split at:\n%s", cfg)
	}
	if strings.Contains(challengeBlock, "gw.public.example.com") {
		t.Fatalf("the unmanaged public domain leaked into the challenge block:\n%s", challengeBlock)
	}
	if !strings.Contains(challengeBlock, "server_name a.int.example.test;") {
		t.Fatalf("the managed server domain is missing from the challenge block:\n%s", challengeBlock)
	}
	// Scoped to the DIRECTIVE lines: the surrounding comment mentions the wildcard
	// precisely to explain why it is not used, so a whole-block search would be
	// unfalsifiable.
	for _, line := range strings.Split(challengeBlock, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "*.") {
			t.Fatalf("a wildcard server_name would re-admit the excluded public domain: %q", trimmed)
		}
	}
	if !strings.Contains(challengeBlock, "/.well-known/acme-challenge/") {
		t.Fatalf("no challenge location:\n%s", challengeBlock)
	}

	// ... and with the switch ON it must be there.
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertManagePublicDomain: &on}); err != nil {
		t.Fatalf("settings: %v", err)
	}
	cfg, err = svc.EdgeProxyConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	challengeBlock, _, _ = strings.Cut(cfg, "listen 443")
	if !strings.Contains(challengeBlock, "gw.public.example.com") {
		t.Fatalf("the managed public domain must be forwarded:\n%s", challengeBlock)
	}
}

// TestEdgeProxyConfigOmitsTheChallengeBlockWhenNothingIsOrdered is the rendered
// half of fix-round finding 1(a): with both issuer modes self_signed the gateway
// places no HTTP-01 order, so the block is left out entirely (with a stated
// reason) rather than claiming server_names on a foreign proxy for nothing.
func TestEdgeProxyConfigOmitsTheChallengeBlockWhenNothingIsOrdered(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 30)
	mustCreateNetbirdServer(t, svc, ctx, "a", "a.int.example.test", "")
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan")

	cfg, err := svc.EdgeProxyConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cfg, "/.well-known/acme-challenge/") {
		t.Fatalf("no order is ever placed -> no challenge forwarding may be claimed:\n%s", cfg)
	}
	if strings.Contains(cfg, "a.int.example.test") {
		t.Fatalf("no name may be claimed on the upstream proxy:\n%s", cfg)
	}
	if !strings.Contains(cfg, "NOT NEEDED with the current settings") {
		t.Fatalf("the omission must state its reason:\n%s", cfg)
	}
	// Switching the internal mode to acme brings the block back.
	acme := IssuerModeACME
	email := "ops@example.test"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertIssuerMode: &acme, ACMEEmail: &email}); err != nil {
		t.Fatalf("settings: %v", err)
	}
	cfg, err = svc.EdgeProxyConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, "/.well-known/acme-challenge/") || !strings.Contains(cfg, "a.int.example.test") {
		t.Fatalf("internal acme mode must produce the forwarding block:\n%s", cfg)
	}
}

// edgeGatewayPeerFixture wires the certificate module (internal acme, NO
// cert_gateway_domain) plus a NetBird module pointed at handler, and returns the
// service and a counter of the requests that handler received. The gateway's own
// mesh FQDN is the one managed name that cannot come from stored settings, so this
// is the fixture for all three resolution cases.
func edgeGatewayPeerFixture(t *testing.T, gatewayDomain string, handler http.HandlerFunc) (*Service, context.Context, *int32) {
	t.Helper()
	var calls int32
	netbird := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		handler(w, r)
	}))
	t.Cleanup(netbird.Close)

	svc, ctx := certEnv(t)
	on := true
	mode := IssuerModeACME
	email := "ops@example.test"
	base := "int.example.test"
	scope := "all"
	gw := gatewayDomain
	names := []string{"edge.lan"}
	mustCreateNetbirdServer(t, svc, ctx, "a", "a.int.example.test", "")
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:          &on,
		CertIssuerMode:       &mode,
		ACMEEmail:            &email,
		CertBaseDomain:       &base,
		CertGatewayDomain:    &gw,
		CertServerScope:      &scope,
		CertEdgeEnabled:      &on,
		CertEdgeIssuerMode:   &mode,
		CertEdgeNames:        &names,
		NetbirdEnabled:       boolPtr(true),
		NetbirdURL:           strPtr(netbird.URL),
		NetbirdToken:         strPtr("nbtok"),
		NetbirdGatewayPeerID: strPtr("peer-1"),
	}); err != nil {
		t.Fatalf("settings: %v", err)
	}
	// A netbird-touching settings PUT fires its policy fleet reconcile in a
	// BACKGROUND goroutine, which makes its own NetBird calls against the very
	// server above. Without this barrier those calls can land AFTER the callers
	// below snapshot `calls`, so a "made no NetBird request" assertion fails for a
	// request the fixture itself issued -- a pre-existing flake in this fixture,
	// load-dependent and therefore intermittent. policySideEffectWG exists exactly
	// for this ("test determinism", see its doc comment).
	svc.waitPolicySideEffects()
	return svc, ctx, &calls
}

// TestEdgeProxyConfigMakesNoNetbirdCallWhenTheGatewayDomainIsConfigured is case (a)
// of the resolution rule: a CONFIGURED cert_gateway_domain is already enumerable
// from stored settings, so the resolver must not be consulted at all -- the GET
// stays network-free in the steady state.
func TestEdgeProxyConfigMakesNoNetbirdCallWhenTheGatewayDomainIsConfigured(t *testing.T) {
	// The handler answers rather than failing: the settings PUT itself talks to
	// NetBird (group/user reads), so only the delta AROUND EdgeProxyConfig is the
	// property under test.
	svc, ctx, calls := edgeGatewayPeerFixture(t, "gw.int.example.test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	before := atomic.LoadInt32(calls)

	cfg, err := svc.EdgeProxyConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(calls) - before; got != 0 {
		t.Fatalf("EdgeProxyConfig made %d NetBird request(s) for an already-configured name", got)
	}
	if !strings.Contains(cfg, "gw.int.example.test") {
		t.Fatalf("the configured gateway domain must be claimed:\n%s", cfg)
	}
	if strings.Contains(cfg, "cert_gateway_domain is unset") {
		t.Fatalf("the unknown-name note must not appear when the name is configured:\n%s", cfg)
	}
}

// TestEdgeProxyConfigResolvesTheGatewayNameWhenUnset is case (b): with
// cert_gateway_domain unset the reconcile resolves that name live and ORDERS a
// certificate for it, so the forwarding block MUST claim it -- omitting it would
// make that order fail every backoff cycle and the gateway's own certificate would
// never come into existence. Resolved through the TTL-cached resolver.
func TestEdgeProxyConfigResolvesTheGatewayNameWhenUnset(t *testing.T) {
	svc, ctx, calls := edgeGatewayPeerFixture(t, "", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/peers/peer-1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "peer-1", "dns_label": "gw.mesh.example.test"})
	})

	cfg, err := svc.EdgeProxyConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(calls) == 0 {
		t.Fatal("the resolver must be consulted when cert_gateway_domain is unset")
	}
	if !strings.Contains(cfg, "server_name") || !strings.Contains(cfg, "gw.mesh.example.test") {
		t.Fatalf("the resolved gateway name must be claimed in server_name:\n%s", cfg)
	}
	if strings.Contains(cfg, "cert_gateway_domain is unset") {
		t.Fatalf("the unknown-name note must be gone once the name resolved:\n%s", cfg)
	}
	// The TTL cache means a second render does not re-resolve.
	before := atomic.LoadInt32(calls)
	if _, err := svc.EdgeProxyConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(calls) - before; got != 0 {
		t.Fatalf("a second GET made %d NetBird request(s); the resolution must be TTL-cached", got)
	}
}

// TestEdgeProxyConfigFallsOpenWhenTheGatewayNameCannotBeResolved is case (c) -- the
// property that must not regress: a NetBird outage may neither fail the endpoint nor
// silently drop the name. The output keeps the honest marker, writes no partial or
// guessed name, and the call still succeeds.
//
// The other assertions target CONDITIONAL markers on purpose (fix-round finding 3):
// the banner and the certbot suggestion carry edgeProxyUnknown unconditionally, so a
// bare Contains(cfg, edgeProxyUnknown) could never fail.
func TestEdgeProxyConfigFallsOpenWhenTheGatewayNameCannotBeResolved(t *testing.T) {
	svc, ctx, calls := edgeGatewayPeerFixture(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	cfg, err := svc.EdgeProxyConfig(ctx)
	if err != nil {
		t.Fatalf("a NetBird outage must not fail the endpoint: %v", err)
	}
	if atomic.LoadInt32(calls) == 0 {
		t.Fatal("precondition: the resolver was never consulted, so nothing failed open")
	}
	if !strings.Contains(cfg, edgeProxyUnknown+": cert_gateway_domain is unset") {
		t.Fatalf("an unresolvable gateway name must be marked unknown:\n%s", cfg)
	}
	// No garbage: the only names claimed are the ones from stored data.
	for _, line := range strings.Split(cfg, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "server_name ") {
			continue
		}
		if strings.Contains(trimmed, "peer-1") || strings.Contains(trimmed, "%") || strings.Contains(trimmed, "error") {
			t.Fatalf("a partial/garbage name reached server_name: %q", trimmed)
		}
	}
	// The :443 block's own conditional placeholder (no public domain configured).
	if !strings.Contains(cfg, "server_name PUBLIC_NAME;") {
		t.Fatalf("an unset public domain must leave a visible placeholder:\n%s", cfg)
	}
}

// TestEdgeProxyConfigTrustAnchorFollowsTheEdgeMode: the upstream leg has to be
// verifiable, and how depends on who signed the edge certificate -- the internal
// CA (download the root) or a public CA (system trust store). With the feature off
// the leg is plain http, which the output says outright rather than implying.
func TestEdgeProxyConfigTrustAnchorFollowsTheEdgeMode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		edgeOn      bool
		mode        string
		wantSubstrs []string
		denySubstrs []string
	}{
		{
			name: "self signed", edgeOn: true, mode: IssuerModeSelfSigned,
			wantSubstrs: []string{"proxy_ssl_verify on;", "op-gateway-ca.pem", "proxy_ssl_name edge.lan;", "https://edge.lan:8443"},
			denySubstrs: []string{"ca-certificates.crt", "UNENCRYPTED"},
		},
		{
			name: "acme", edgeOn: true, mode: IssuerModeACME,
			wantSubstrs: []string{"proxy_ssl_verify on;", "/etc/ssl/certs/ca-certificates.crt", "https://edge.lan:8443"},
			denySubstrs: []string{"op-gateway-ca.pem", "UNENCRYPTED"},
		},
		{
			name: "edge off", edgeOn: false, mode: IssuerModeSelfSigned,
			wantSubstrs: []string{"UNENCRYPTED", "http://edge.lan:8080"},
			denySubstrs: []string{"proxy_ssl_verify", "https://edge.lan"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx := certEnv(t)
			on := true
			edgeOn := tc.edgeOn
			mode := IssuerModeSelfSigned
			edgeMode := tc.mode
			base := "int.example.test"
			email := "ops@example.test"
			names := []string{"edge.lan"}
			if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
				CertEnabled:        &on,
				CertIssuerMode:     &mode,
				CertBaseDomain:     &base,
				ACMEEmail:          &email,
				CertEdgeEnabled:    &edgeOn,
				CertEdgeIssuerMode: &edgeMode,
				CertEdgeNames:      &names,
			}); err != nil {
				t.Fatalf("settings: %v", err)
			}
			cfg, err := svc.EdgeProxyConfig(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.wantSubstrs {
				if !strings.Contains(cfg, want) {
					t.Fatalf("missing %q:\n%s", want, cfg)
				}
			}
			for _, deny := range tc.denySubstrs {
				if strings.Contains(cfg, deny) {
					t.Fatalf("unexpected %q:\n%s", deny, cfg)
				}
			}
			if strings.Contains(cfg, "PRIVATE KEY") {
				t.Fatal("the generated configuration must never contain key material")
			}
			// The challenge forwarding follows whether an HTTP-01 order actually
			// exists, NOT whether the edge feature is on: the internal mode is
			// self_signed in every case here, so only the acme EDGE case needs it.
			wantChallenge := tc.edgeOn && tc.mode == IssuerModeACME
			if got := strings.Contains(cfg, "/.well-known/acme-challenge/"); got != wantChallenge {
				t.Fatalf("challenge forwarding present = %v, want %v (edge mode %q):\n%s", got, wantChallenge, tc.mode, cfg)
			}
		})
	}
}

// TestEdgeProxyConfigCarriesTheStreamingAndHeaderRules guards the details that
// are easy to lose and expensive to debug: the WebSocket location (whose own
// proxy_set_header set DISCARDS the inherited one, so all of them are repeated),
// the unbounded body on the inference paths, and the internal headers blanked
// already at the outermost proxy.
func TestEdgeProxyConfigCarriesTheStreamingAndHeaderRules(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 30)
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan")

	cfg, err := svc.EdgeProxyConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"location = /api/agent/v1/stream",
		"proxy_set_header Upgrade $http_upgrade;",
		`proxy_set_header Connection "upgrade";`,
		"client_max_body_size 0;",
		"proxy_read_timeout 3600s;",
		"proxy_buffering off;",
		`proxy_set_header X-OP-Internal-Auth "";`,
		`proxy_set_header X-OP-Internal-User "";`,
		`proxy_set_header X-OP-Server-Override "";`,
		`proxy_set_header X-OP-Server-Override-Force "";`,
	} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("missing %q:\n%s", want, cfg)
		}
	}
	// The four internal headers must be blanked in BOTH the server block and the
	// WebSocket location (the location resets the inherited set).
	if n := strings.Count(cfg, `proxy_set_header X-OP-Internal-Auth "";`); n != 2 {
		t.Fatalf("X-OP-Internal-Auth blanked %d times, want 2 (server block + WebSocket location)", n)
	}
}

func TestEdgeCertificateViewReportsTheDeliveryModeAndCarriesNoKeyMaterial(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dir        string
		wantMode   string
		wantKeyDL  bool
		wantOutDir bool
	}{
		{"no output directory", "", edgeDeliveryDownload, true, false},
		{"an output directory", "SET", edgeDeliveryLocal, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx := certEnv(t)
			if tc.dir == "SET" {
				svc.cert.edgeOutputDir = t.TempDir()
			}
			// The edge certificate is a sub-feature of the certificate module, so the
			// module itself has to be on for CertSettings to resolve anything at all.
			enableSelfSigned(t, svc, ctx, "all", 30)
			enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan", "10.0.0.5")
			res := seedEdgeCertificate(t, svc, ctx, "edge.lan")

			dto, err := svc.EdgeCertificateView(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !dto.Enabled || dto.IssuerMode != IssuerModeSelfSigned {
				t.Fatalf("enabled/mode = %v/%q", dto.Enabled, dto.IssuerMode)
			}
			if len(dto.Names) != 2 || dto.Names[0] != "edge.lan" {
				t.Fatalf("names = %v, want the normalized configured list", dto.Names)
			}
			if dto.Domain != "edge.lan" || dto.Status != "active" || dto.Fingerprint != res.Fingerprint {
				t.Fatalf("stored row not surfaced: %+v", dto)
			}
			if dto.NotAfter == nil || dto.NotBefore == nil || dto.IssuedAt == nil {
				t.Fatalf("timing not surfaced: %+v", dto)
			}
			if dto.DeliveryMode != tc.wantMode {
				t.Fatalf("delivery_mode = %q, want %q", dto.DeliveryMode, tc.wantMode)
			}
			if dto.KeyDownloadAvailable != tc.wantKeyDL {
				t.Fatalf("key_download_available = %v, want %v", dto.KeyDownloadAvailable, tc.wantKeyDL)
			}
			if (dto.OutputDir != "") != tc.wantOutDir {
				t.Fatalf("output_dir = %q, want set=%v", dto.OutputDir, tc.wantOutDir)
			}
			// Nothing delivered yet -> written_at stays null even in "local" mode.
			if dto.WrittenAt != nil {
				t.Fatalf("written_at = %v, want null before the first delivery", dto.WrittenAt)
			}
			blob := mustMarshalJSON(t, dto)
			for _, needle := range []string{"BEGIN", "PRIVATE KEY", "key_sealed", "fullchain", res.KeyPEM} {
				if needle != "" && strings.Contains(blob, needle) {
					t.Fatalf("the edge DTO leaks %q: %s", needle, blob)
				}
			}
		})
	}
}

// TestEdgeCertificateViewReportsAFailedDeliveryAsDownloadable closes the loop
// between the two: a configured directory that cannot be written flips the mode
// back to "download" (and thus opens the key endpoint), with the reason visible.
func TestEdgeCertificateViewReportsAFailedDeliveryAsDownloadable(t *testing.T) {
	svc, ctx := certEnv(t)
	svc.cert.edgeOutputDir = filepath.Join(t.TempDir(), "does-not-exist")
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan")
	seedEdgeCertificate(t, svc, ctx, "edge.lan")

	if err := svc.DeliverEdgeCertificate(ctx); err == nil {
		t.Fatal("delivery into a missing directory must fail")
	}
	dto, err := svc.EdgeCertificateView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dto.DeliveryMode != edgeDeliveryDownload || !dto.KeyDownloadAvailable {
		t.Fatalf("a failed write must read as downloadable: %+v", dto)
	}
	if dto.WriteError == "" {
		t.Fatalf("the failure reason must be visible: %+v", dto)
	}
	if dto.OutputDir == "" {
		t.Fatalf("the configured path must stay visible so the operator can fix it: %+v", dto)
	}
}

// TestEdgeCertificateViewSurfacesANameConflict makes the carried-over known limit
// visible: an edge primary name that is ALSO a managed internal name yields no
// kind='edge' row at all (the desired-set build keeps the internal provenance), so
// the switch appears to do nothing. The status view names it instead.
func TestEdgeCertificateViewSurfacesANameConflict(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 30)
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "a.int.example.test")

	svc.ReconcileCertificates(ctx)

	dto, err := svc.EdgeCertificateView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dto.Domain != "" {
		t.Fatalf("precondition changed: an edge row now exists (%q) -- the conflict case is gone", dto.Domain)
	}
	if dto.NameConflict != "a.int.example.test" {
		t.Fatalf("name_conflict = %q, want the colliding name", dto.NameConflict)
	}

	// A name of its own must NOT be reported as a conflict.
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan")
	dto, err = svc.EdgeCertificateView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dto.NameConflict != "" {
		t.Fatalf("name_conflict = %q, want none for a free name", dto.NameConflict)
	}
}

// TestEdgeCertificateKeyPEMRefusesWhileTheGatewayCanDeliver is the service-level
// half of the endpoint's gate (the HTTP half lives in internal/gateway): the key
// is withheld while a local path exists and handed over when there is none.
func TestEdgeCertificateKeyPEMRefusesWhileTheGatewayCanDeliver(t *testing.T) {
	svc, ctx := certEnv(t)
	svc.cert.edgeOutputDir = t.TempDir()
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan")
	res := seedEdgeCertificate(t, svc, ctx, "edge.lan")

	if _, err := svc.EdgeCertificateKeyPEM(ctx); !errors.Is(err, ErrEdgeKeyManaged) {
		t.Fatalf("err = %v, want ErrEdgeKeyManaged", err)
	}
	// Same service, same row -- only the capability changes.
	svc.cert.edgeOutputDir = ""
	key, err := svc.EdgeCertificateKeyPEM(ctx)
	if err != nil {
		t.Fatalf("without a local path the download must be allowed: %v", err)
	}
	if key != res.KeyPEM {
		t.Fatal("the served key is not the stored one")
	}
	if !strings.Contains(key, "PRIVATE KEY") {
		t.Fatalf("served key does not look like a key: %q", key)
	}
}

func TestEdgeCertificateKeyPEMIsNotFoundWithoutARow(t *testing.T) {
	svc, ctx := certEnv(t)
	if _, err := svc.EdgeCertificateKeyPEM(ctx); !errors.Is(err, ErrCertificateNotFound) {
		t.Fatalf("err = %v, want ErrCertificateNotFound", err)
	}
}

func TestEdgeCertificateBundlePEMIsPublicMaterialOnly(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 30)
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan")
	svc.ReconcileCertificates(ctx)

	bundle, err := svc.EdgeCertificateBundlePEM(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bundle, "PRIVATE KEY") {
		t.Fatal("the edge bundle must never contain a private key")
	}
	// Leaf + internal root: the upstream proxy needs the anchor, not just the leaf.
	if n := strings.Count(bundle, "BEGIN CERTIFICATE"); n < 2 {
		t.Fatalf("bundle carries %d certificates, want the leaf plus the internal root", n)
	}
	if !strings.HasSuffix(bundle, "\n") {
		t.Fatal("the concatenated bundle must be newline-terminated")
	}
}

func TestEdgeCertificateBundlePEMIsNotFoundWithoutARow(t *testing.T) {
	svc, ctx := certEnv(t)
	if _, err := svc.EdgeCertificateBundlePEM(ctx); !errors.Is(err, ErrCertificateNotFound) {
		t.Fatalf("err = %v, want ErrCertificateNotFound", err)
	}
}

// TestReissueEdgeCertificateKeepsTheMaterialAndSparesTheInternalRows: the button
// must not invalidate a working certificate (it keeps serving until its
// replacement arrives) and must not drag the internal rows along.
func TestReissueEdgeCertificateKeepsTheMaterialAndSparesTheInternalRows(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 30)
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan")
	svc.ReconcileCertificates(ctx)

	before, err := svc.routes.CertificateByDomain(ctx, "edge.lan")
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != "active" {
		t.Fatalf("precondition: edge row status = %q, want active", before.Status)
	}

	if err := svc.ReissueEdgeCertificate(ctx, systemToken()); err != nil {
		t.Fatal(err)
	}
	after, err := svc.routes.CertificateByDomain(ctx, "edge.lan")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "pending" || !after.NextAttemptAt.IsZero() {
		t.Fatalf("edge row must be due again: %+v", after)
	}
	if after.FullchainPEM != before.FullchainPEM || after.KeySealed != before.KeySealed || !after.NotAfter.Equal(before.NotAfter) {
		t.Fatal("re-issue must keep the stored material and its timing untouched")
	}
	internal, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if internal.Status != "active" {
		t.Fatalf("internal row status = %q, want it untouched (active)", internal.Status)
	}
}

func TestReissueEdgeCertificateIsNotFoundWithoutARow(t *testing.T) {
	svc, ctx := certEnv(t)
	if err := svc.ReissueEdgeCertificate(ctx, systemToken()); !errors.Is(err, ErrCertificateNotFound) {
		t.Fatalf("err = %v, want ErrCertificateNotFound", err)
	}
}

// TestEdgeProxyConfigNeverWritesAnUnvalidatedPublicDomain is the defense-in-depth
// half of the two-layer name defence. The write path now REFUSES these payloads
// (see TestUpdateSystemSettingsValidatesTheCertificateDomains), so the only way one
// can reach the generator is a writer that bypasses UpdateSystemSettings -- a
// future migration, a manual DB edit, an as-yet-unwritten setter. This seeds the
// raw setting directly to prove the generator still refuses to interpolate it into
// a configuration destined for a more privileged machine.
func TestEdgeProxyConfigNeverWritesAnUnvalidatedPublicDomain(t *testing.T) {
	for _, payload := range []string{
		"evil.example.com; return 444",
		"evil.example.com}\nserver{listen 81;",
		"evil.example.com #x",
		"evil example com",
	} {
		svc, ctx := certEnv(t)
		on := true
		mode := IssuerModeSelfSigned
		base := "int.example.test"
		names := []string{"edge.lan"}
		if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
			CertEnabled:            &on,
			CertIssuerMode:         &mode,
			CertBaseDomain:         &base,
			CertManagePublicDomain: &on,
			CertEdgeEnabled:        &on,
			CertEdgeIssuerMode:     &mode,
			CertEdgeNames:          &names,
		}); err != nil {
			t.Fatalf("settings: %v", err)
		}
		// Bypass the validating write path the way a foreign writer would.
		if err := svc.settings.SetSystemSetting(ctx, certPublicDomainsKey, payload, svc.clock()); err != nil {
			t.Fatal(err)
		}
		// Precondition: the payload really IS stored (otherwise the assertion below
		// would pass vacuously).
		stored, _, err := svc.CertSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(stored.PublicDomains, ","), "evil") {
			t.Fatalf("precondition: the payload was not stored (%v)", stored.PublicDomains)
		}

		cfg, err := svc.EdgeProxyConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(cfg, "evil") {
			t.Fatalf("payload %q reached the generated configuration:\n%s", payload, cfg)
		}
		// It must degrade to the honest placeholder, not silently vanish.
		if !strings.Contains(cfg, "server_name PUBLIC_NAME;") {
			t.Fatalf("a dropped public domain must leave the placeholder:\n%s", cfg)
		}
	}
}

// TestUpdateSystemSettingsValidatesTheCertificateDomains is the write-path half:
// cert_base_domain, cert_gateway_domain and every cert_public_domains entry are
// validated like an edge name MINUS the IP branch. Before this, a malformed value
// was stored and became an HTTP-01 order identifier the CA rejects (burning a
// failed order every backoff cycle) or a nonsense SAN in a self-signed leaf -- and
// it reached generated nginx configuration.
func TestUpdateSystemSettingsValidatesTheCertificateDomains(t *testing.T) {
	svc, ctx := certEnv(t)
	on := true
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertEnabled: &on}); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		"evil.example.com; return 444",
		"evil.example.com}\nserver{listen 81;",
		"evil.example.com #x",
		"evil example com",
		"10.0.0.5",      // an IP is a configuration error for all three
		"2001:db8::1",   // ... including IPv6
		"-lead.example", // the strict label rules still apply
	}
	for _, v := range bad {
		val := v
		if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertBaseDomain: &val}); !errors.Is(err, ErrCertInvalid) {
			t.Fatalf("cert_base_domain %q err = %v, want ErrCertInvalid", v, err)
		}
		if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertGatewayDomain: &val}); !errors.Is(err, ErrCertInvalid) {
			t.Fatalf("cert_gateway_domain %q err = %v, want ErrCertInvalid", v, err)
		}
		list := []string{"ok.example.com", val}
		if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertPublicDomains: &list}); !errors.Is(err, ErrCertInvalid) {
			t.Fatalf("cert_public_domains %q err = %v, want ErrCertInvalid", v, err)
		}
	}
	// Nothing was persisted by any of the rejected writes.
	stored, _, err := svc.CertSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.BaseDomain != "" || stored.GatewayDomain != "" || len(stored.PublicDomains) != 0 {
		t.Fatalf("a rejected write must persist nothing: %+v", stored)
	}
	// The legal shapes still pass: empty (all three are optional), a plain DNS name,
	// an uppercase spelling (lower-cased on the way in), and a multi-entry list with
	// a blank that the encoder drops.
	empty := ""
	good := "GW.Int.Example.Test"
	list := []string{"a.example.com", "", "b.example.com"}
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertBaseDomain:    &empty,
		CertGatewayDomain: &good,
		CertPublicDomains: &list,
	}); err != nil {
		t.Fatalf("a legal write must be accepted: %v", err)
	}
	stored, _, err = svc.CertSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.GatewayDomain != "gw.int.example.test" {
		t.Fatalf("gateway domain = %q, want it stored lower-cased", stored.GatewayDomain)
	}
	if len(stored.PublicDomains) != 2 {
		t.Fatalf("public domains = %v, want the blank dropped and both names kept", stored.PublicDomains)
	}
}

// TestEdgeCertificateViewOffersNoKeyDownloadWithoutAStoredKey pins the second half
// of KeyDownloadAvailable: local delivery being impossible is not enough -- with
// nothing issued there is nothing to download, and the portal must not offer a
// button that 404s.
func TestEdgeCertificateViewOffersNoKeyDownloadWithoutAStoredKey(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 30)
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan")
	// No output directory -> not capable -> the capability half is satisfied.
	dto, err := svc.EdgeCertificateView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dto.DeliveryMode != edgeDeliveryDownload {
		t.Fatalf("precondition: delivery_mode = %q, want download", dto.DeliveryMode)
	}
	if dto.KeyDownloadAvailable {
		t.Fatal("key_download_available must be false while nothing is issued")
	}
	// The endpoint agrees.
	if _, err := svc.EdgeCertificateKeyPEM(ctx); !errors.Is(err, ErrCertificateNotFound) {
		t.Fatalf("err = %v, want ErrCertificateNotFound", err)
	}

	seedEdgeCertificate(t, svc, ctx, "edge.lan")
	dto, err = svc.EdgeCertificateView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !dto.KeyDownloadAvailable {
		t.Fatal("key_download_available must flip to true once a key is stored")
	}
}

// TestEdgeACMEChallengeNamesRequireTheInternalACMEMode is fix-round finding 1(a):
// internal HTTP-01 orders happen ONLY in the acme internal mode (the reconcile's
// under-base rule keys on set.modeFor(kind)), so with both modes self_signed the
// gateway never places one and nothing needs forwarding. Emitting the block anyway
// hands a foreign proxy a path this gateway answers 404 for.
func TestEdgeACMEChallengeNamesRequireTheInternalACMEMode(t *testing.T) {
	svc, ctx := certEnv(t)
	mustCreateNetbirdServer(t, svc, ctx, "a.int.example.test", "a.int.example.test", "")
	set := CertSettings{
		IssuerMode:         IssuerModeSelfSigned,
		BaseDomain:         "int.example.test",
		GatewayDomain:      "gw.int.example.test",
		ServerScope:        "all",
		ManagePublicDomain: true,
		PublicDomains:      []string{"gw.public.example.com"},
		EdgeEnabled:        true,
		EdgeIssuerMode:     IssuerModeSelfSigned,
		EdgeNames:          []string{"edge.lan"},
	}

	got, _, err := svc.edgeACMEChallengeNames(ctx, set)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("self_signed everywhere -> no HTTP-01 order exists -> forward nothing, got %v", got)
	}

	set.IssuerMode = IssuerModeACME
	got, _, err = svc.edgeACMEChallengeNames(ctx, set)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gw.int.example.test", "a.int.example.test", "gw.public.example.com"} {
		if !containsName(got, want) {
			t.Fatalf("internal acme mode must forward %q: %v", want, got)
		}
	}
}

// TestEdgeACMEChallengeNamesNeverClaimAWildcard is fix-round finding 1(b): a
// wildcard under the base domain silently swallows every sibling name -- including
// a public domain the operator deliberately EXCLUDED (cert_manage_public_domain
// off) and that this proxy very likely renews itself. nginx ranks a wildcard
// (priority 2) above a catch-all/default_server (priority 5), so such a block
// hijacks every :80 request for the whole zone. The managed names are enumerated
// from stored data instead -- no wildcard, and no NetBird call.
func TestEdgeACMEChallengeNamesNeverClaimAWildcard(t *testing.T) {
	svc, ctx := certEnv(t)
	mustCreateNetbirdServer(t, svc, ctx, "a.ai-gw.net", "a.ai-gw.net", "")
	// The realistic collateral case: one registered domain, mesh names beneath it,
	// and the public name excluded from certificate management.
	set := CertSettings{
		IssuerMode:         IssuerModeACME,
		BaseDomain:         "ai-gw.net",
		ServerScope:        "all",
		ManagePublicDomain: false,
		PublicDomains:      []string{"op.ai-gw.net"},
	}
	got, _, err := svc.edgeACMEChallengeNames(ctx, set)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range got {
		if strings.HasPrefix(n, "*") {
			t.Fatalf("a wildcard server_name re-admits the excluded public domain: %v", got)
		}
	}
	if containsName(got, "op.ai-gw.net") {
		t.Fatalf("the excluded public domain must not be forwarded: %v", got)
	}
	if !containsName(got, "a.ai-gw.net") {
		t.Fatalf("the managed server domain must be forwarded: %v", got)
	}
}

// TestEdgeACMEChallengeNamesHonourTheServerScope: the enumeration reuses the same
// scope x override matrix the reconcile's desired set uses, so an excluded server
// is not claimed on the upstream proxy either.
func TestEdgeACMEChallengeNamesHonourTheServerScope(t *testing.T) {
	svc, ctx := certEnv(t)
	mustCreateNetbirdServer(t, svc, ctx, "kept", "kept.int.example.test", "")
	mustCreateNetbirdServer(t, svc, ctx, "dropped", "dropped.int.example.test", "exclude")
	set := CertSettings{
		IssuerMode:  IssuerModeACME,
		BaseDomain:  "int.example.test",
		ServerScope: "all",
	}
	got, _, err := svc.edgeACMEChallengeNames(ctx, set)
	if err != nil {
		t.Fatal(err)
	}
	if !containsName(got, "kept.int.example.test") || containsName(got, "dropped.int.example.test") {
		t.Fatalf("the scope/override matrix must decide which server domains are claimed: %v", got)
	}
}

// TestEdgeCertificateBundleOmitsTheInternalRootInACMEEdgeMode is fix-round finding
// 4, and its combination is reachable: the edge mode is acme while the INTERNAL
// mode is self_signed, so an internal CA exists (and is deliberately never deleted
// on a mode switch) but is NOT this certificate's trust anchor. Shipping it anyway
// would have an operator paste an unrelated root into
// proxy_ssl_trusted_certificate, making that proxy also accept an
// internal-CA-signed certificate for the edge name -- quietly widening what acme
// mode guarantees.
func TestEdgeCertificateBundleOmitsTheInternalRootInACMEEdgeMode(t *testing.T) {
	svc, ctx := certEnv(t)
	// Internal self_signed => the reconcile mints an internal CA...
	enableSelfSigned(t, svc, ctx, "all", 30)
	// ... while the EDGE row's own mode is acme.
	enableEdge(t, svc, ctx, IssuerModeACME, "gw.public.example.com")
	svc.ReconcileCertificates(ctx)
	if ca, err := svc.CertificateCAView(ctx); err != nil || !ca.Present {
		t.Fatalf("precondition: an internal CA must exist (present=%v, err=%v)", ca.Present, err)
	}
	// The stored edge leaf itself is irrelevant to the rule (only the mode is), so a
	// seeded row keeps the test independent of a live ACME server.
	seedEdgeCertificate(t, svc, ctx, "gw.public.example.com")

	bundle, err := svc.EdgeCertificateBundlePEM(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(bundle, "BEGIN CERTIFICATE"); n != 1 {
		t.Fatalf("acme edge mode bundle carries %d certificates, want the leaf chain only:\n%s", n, bundle)
	}
	caBundle, err := svc.CertificateCABundlePEM(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bundle, strings.TrimSpace(caBundle)) {
		t.Fatal("the internal root is not this certificate's anchor in acme mode and must not be shipped")
	}

	// Flipping ONLY the edge mode brings the anchor back (the self_signed contract).
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "gw.public.example.com")
	bundle, err = svc.EdgeCertificateBundlePEM(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(bundle, "BEGIN CERTIFICATE"); n != 2 {
		t.Fatalf("self_signed edge mode bundle carries %d certificates, want leaf + internal root:\n%s", n, bundle)
	}
}

// TestEdgeProxyConfigMarksAnIPOnlyEdgeAsUnverifiable: with only IP SANs configured
// there is no host name for nginx to match proxy_ssl_name against (it never matches
// an IP SAN), so the generated configuration says so instead of emitting a directive
// that silently never matches. The placeholder behaviour itself is unchanged.
func TestEdgeProxyConfigMarksAnIPOnlyEdgeAsUnverifiable(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 30)
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "10.0.0.5", "10.0.0.6")

	cfg, err := svc.EdgeProxyConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, "every configured edge name is an IP address") {
		t.Fatalf("an IP-only edge configuration must be called out as unverifiable:\n%s", cfg)
	}
	if !strings.Contains(cfg, edgeProxyHostPlaceholder) {
		t.Fatalf("the host placeholder behaviour must be unchanged:\n%s", cfg)
	}
	// A DNS name among the IPs removes the note and is used as the verified name.
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "10.0.0.5", "edge.lan")
	cfg, err = svc.EdgeProxyConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cfg, "every configured edge name is an IP address") {
		t.Fatalf("with a DNS name present the note must not appear:\n%s", cfg)
	}
	if !strings.Contains(cfg, "proxy_ssl_name edge.lan;") {
		t.Fatalf("the DNS name must be used for verification:\n%s", cfg)
	}
}

// TestEdgeProxyConfigNeverWritesAnUnvalidatedEdgeName covers fix-round finding 2:
// the edge host lands in proxy_pass and twice in proxy_ssl_name, and it comes from
// configuredEdgeNames -> edgeDesired, which only trims/lowercases/dedupes and never
// validates. cert_edge_names IS validated on its write path today, so -- exactly
// like the public-domain case -- the payload is seeded past that writer to prove the
// generator does not depend on remaining the only one.
func TestEdgeProxyConfigNeverWritesAnUnvalidatedEdgeName(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 30)
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan")
	if err := svc.settings.SetSystemSetting(ctx, certEdgeNamesKey, "evil.lan; return 444", svc.clock()); err != nil {
		t.Fatal(err)
	}
	// Precondition: the payload really is the configured primary name.
	stored, _, err := svc.CertSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := configuredEdgeNames(stored); len(got) == 0 || !strings.Contains(got[0], "evil") {
		t.Fatalf("precondition: the payload is not the primary edge name: %v", got)
	}

	cfg, err := svc.EdgeProxyConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cfg, "evil") {
		t.Fatalf("an unvalidated edge name reached proxy_pass/proxy_ssl_name:\n%s", cfg)
	}
	if !strings.Contains(cfg, edgeProxyHostPlaceholder) {
		t.Fatalf("a dropped edge host must degrade to the placeholder:\n%s", cfg)
	}
}

// TestEdgeProxyConfigRejectsAnUnusableResolvedGatewayName covers requirement 3 of the
// resolution rule: the resolved name arrives from an EXTERNAL API (the NetBird admin
// API's dns_label), so it goes through the same ValidateEdgeName gate as every other
// interpolated name -- and an unusable one falls open to the marker rather than being
// written into a configuration destined for a more privileged machine.
func TestEdgeProxyConfigRejectsAnUnusableResolvedGatewayName(t *testing.T) {
	for _, bad := range []string{"evil.lan; return 444", "evil.lan}\nserver{listen 81;", "evil lan"} {
		label := bad
		svc, ctx, _ := edgeGatewayPeerFixture(t, "", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/peers/peer-1" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "peer-1", "dns_label": label})
		})
		cfg, err := svc.EdgeProxyConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(cfg, "evil") {
			t.Fatalf("an unusable resolved name reached the configuration (%q):\n%s", bad, cfg)
		}
		if !strings.Contains(cfg, edgeProxyUnknown+": cert_gateway_domain is unset") {
			t.Fatalf("a rejected resolved name must fall open to the marker (%q):\n%s", bad, cfg)
		}
	}
}

// Final-review I1: every edge consumer must resolve the row by the CONFIGURED
// primary name, never by "the first stored edge row". A second edge row arises in
// ORDINARY operation -- renaming the primary name creates a row for the new name,
// and a failed issuance persists it (recordCertFailure) with status=error and no
// material, while the old, still-valid row is deliberately kept (the prune
// exception). Both stores return the rows ordered by domain, so with "new" sorting
// before "old" the pre-fix code handed every consumer the EMPTY error row: the
// panel showed it, the bundle download 404'd, and -- the damaging one -- delivery
// stopped re-materializing the working certificate, so a fresh volume left nginx
// on its untrusted bootstrap pair.
//
// Two phases, because they pull in opposite directions and both must hold:
//
//	phase 1 -- the rename is configured and failing: the panel/bundle/reissue speak
//	           about the CONFIGURED name (new.lan, honestly reported as errored),
//	           while DELIVERY keeps writing the superseded-but-still-valid material
//	           (continuity: what nginx serves is not a configuration question);
//	phase 2 -- the rename is abandoned (a -> b -> a): the configured name is the
//	           VALID row again while the stray error row still sorts first -- now
//	           every consumer, delivery included, must be back on the valid row.
func TestEdgeConsumersResolveTheConfiguredPrimaryNotTheFirstStoredRow(t *testing.T) {
	dir := t.TempDir()
	svc, ctx := certEnv(t)
	svc.cert.edgeOutputDir = dir
	enableACME(t, svc, ctx, "all")
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "old.lan")
	svc.cert.issuer = svc.issueCertificate
	svc.ReconcileCertificates(ctx)
	old, err := svc.routes.CertificateByDomain(ctx, "old.lan")
	if err != nil {
		t.Fatalf("precondition: the first edge row must exist: %v", err)
	}

	// Phase 1. Rename to a name that sorts FIRST, and let the issuance fail.
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "new.lan")
	svc.cert.issuer = func(context.Context, CertSettings, desiredCert) (certissue.Result, error) {
		return certissue.Result{}, errors.New("ca: temporarily unavailable")
	}
	// Wipe the delivered files first: this is the "fresh/reset volume" case, the
	// one where a delivery no-op is not merely cosmetic but leaves nginx serving
	// the bootstrap pair.
	for _, name := range []string{edgeChainFile, edgeKeyFile} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatalf("precondition: clear the delivered %s: %v", name, err)
		}
	}
	svc.ReconcileCertificates(ctx)

	// Precondition for the whole test: two edge rows, and the new one sorts first.
	rows, primary, err := svc.edgeRowsAndPrimary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Domain != "new.lan" {
		t.Fatalf("precondition: rows = %+v, want new.lan first and old.lan kept", rows)
	}
	if primary != "new.lan" {
		t.Fatalf("precondition: configured primary = %q, want new.lan", primary)
	}
	if rows[0].FullchainPEM != "" || rows[0].KeySealed != "" {
		t.Fatal("precondition: the failed row must carry no material")
	}

	view, err := svc.EdgeCertificateView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.Domain != "new.lan" || view.Status != "error" {
		t.Fatalf("panel shows %q/%q, want the CONFIGURED name new.lan and its honest error status",
			view.Domain, view.Status)
	}
	if _, err := svc.EdgeCertificateBundlePEM(ctx); !errors.Is(err, ErrCertificateNotFound) {
		t.Fatalf("bundle err = %v, want ErrCertificateNotFound -- the configured name has no certificate yet", err)
	}
	if err := svc.ReissueEdgeCertificate(ctx, systemToken()); err != nil {
		t.Fatalf("reissue: %v", err)
	}
	due, err := svc.routes.CertificateByDomain(ctx, "new.lan")
	if err != nil {
		t.Fatal(err)
	}
	if due.Status != "pending" {
		t.Fatalf("reissue marked %q due instead of the configured new.lan (status %q)", "old.lan", due.Status)
	}
	if untouched, err := svc.routes.CertificateByDomain(ctx, "old.lan"); err != nil || untouched.Status != "active" {
		t.Fatalf("reissue must not touch the superseded row: %+v (err %v)", untouched, err)
	}
	// ... while delivery re-materialized the certificate that is actually in force.
	chain, err := os.ReadFile(filepath.Join(dir, edgeChainFile))
	if err != nil {
		t.Fatalf("the still-valid certificate was not re-delivered onto an empty volume: %v", err)
	}
	if string(chain) != old.FullchainPEM {
		t.Fatal("delivery wrote something other than the certificate currently in force")
	}
	if _, err := os.Stat(filepath.Join(dir, edgeKeyFile)); err != nil {
		t.Fatalf("the matching key was not re-delivered: %v", err)
	}

	// Phase 2. The operator gives up on the rename and configures the old name
	// again. The stray error row survives (nothing prunes it until the live
	// certificate is re-issued), and it still sorts first.
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "old.lan")
	if rows, _, err := svc.edgeRowsAndPrimary(ctx); err != nil || len(rows) != 2 || rows[0].Domain != "new.lan" {
		t.Fatalf("precondition: the stray row must survive and still sort first: %+v (err %v)", rows, err)
	}
	view, err = svc.EdgeCertificateView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.Domain != "old.lan" || view.Status != "active" {
		t.Fatalf("panel shows %q/%q, want the configured, valid old.lan -- not the stray row that sorts first",
			view.Domain, view.Status)
	}
	bundle, err := svc.EdgeCertificateBundlePEM(ctx)
	if err != nil || !strings.Contains(bundle, "BEGIN CERTIFICATE") {
		t.Fatalf("bundle = %q (err %v), want the configured row's chain", bundle, err)
	}
	if err := svc.ReissueEdgeCertificate(ctx, systemToken()); err != nil {
		t.Fatalf("reissue: %v", err)
	}
	if reissued, err := svc.routes.CertificateByDomain(ctx, "old.lan"); err != nil || reissued.Status != "pending" {
		t.Fatalf("reissue targeted the wrong row: old.lan = %+v (err %v)", reissued, err)
	}
}

// Final-review Minor 6: writeFileAtomic must create its temp file in the TARGET
// directory. os.CreateTemp("", ...) would pass every existing assertion (the
// output directory ends up with the same files, and the "no leftover temp file"
// scan only looks at that directory), yet on the real deployment -- a named
// volume shared with the web container, whose /tmp is a different filesystem --
// every single delivery would fail with EXDEV on the rename.
//
// The discriminator: point the SYSTEM temp directory at something unusable. A
// same-directory temp file does not care; an os.CreateTemp("", ...) cannot even
// create its file.
func TestWriteFileAtomicUsesATempFileInTheTargetDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(dir, "no-such-system-temp-dir"))

	if err := writeFileAtomic(filepath.Join(dir, "edge-fullchain.pem"), "content\n", 0o644); err != nil {
		t.Fatalf("writeFileAtomic must not depend on the system temp directory: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "edge-fullchain.pem"))
	if err != nil || string(got) != "content\n" {
		t.Fatalf("file = %q (err %v), want the written content", got, err)
	}
	// And nothing was left behind next to it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// Final-review Minor 5: edge-ca.pem is the file BOTH compose variants mount and
// the file the generated proxy_ssl_trusted_certificate points at, yet suppressing
// its delivery entirely kept the whole suite green. In the self_signed edge mode
// it must be written (with the CA bundle, world-readable like the chain); in the
// acme mode it must NOT appear -- shipping an unrelated internal root next to a
// publicly trusted chain would quietly widen what that mode guarantees.
func TestDeliverEdgeCertificateWritesTheCABundleOnlyInTheSelfSignedMode(t *testing.T) {
	dir := t.TempDir()
	svc, ctx := certEnv(t)
	svc.cert.edgeOutputDir = dir
	enableSelfSigned(t, svc, ctx, "all", 30)
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan")
	svc.cert.issuer = svc.issueCertificate
	svc.ReconcileCertificates(ctx)

	if _, err := svc.routes.CertificateByDomain(ctx, "edge.lan"); err != nil {
		t.Fatalf("precondition: the edge row must be issued: %v", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, edgeCAFile))
	if err != nil {
		t.Fatalf("%s not delivered in the self_signed mode: %v", edgeCAFile, err)
	}
	bundle, err := svc.CertificateCABundlePEM(ctx)
	if err != nil {
		t.Fatalf("CA bundle: %v", err)
	}
	if string(caPEM) != bundle {
		t.Fatalf("%s content = %q, want the CA bundle the portal hands out", edgeCAFile, caPEM)
	}
	info, err := os.Stat(filepath.Join(dir, edgeCAFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		// The upstream proxy's nginx reads it; it is public material.
		t.Fatalf("%s mode = %v, want 0644", edgeCAFile, perm)
	}
}

// Final-review Minor 2: the FIRST reconcile gate was a silent dead end. With the
// module on, the internal mode "acme" and no acme_email, and the edge row unable
// to carry the pass on its own, ReconcileCertificates returns BEFORE anything is
// written -- nothing is issued and, unlike its sibling (edge on without names),
// nothing said why. It must leave the operator-actionable note instead.
func TestReconcileNotesThatNoIssuerModeIsUsable(t *testing.T) {
	svc, ctx := certEnv(t)
	on := true
	mode := IssuerModeACME
	email := ""
	base := "int.example.test"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:    &on,
		CertIssuerMode: &mode,
		ACMEEmail:      &email,
		CertBaseDomain: &base,
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	// The edge row is on with names, but its OWN mode is acme with the same
	// missing email -> nothing is usable (the exact combination the reviewer named).
	enableEdge(t, svc, ctx, IssuerModeACME, "gw.public.example.com")
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		t.Fatalf("issued %q although no issuer mode is usable", want.Domain)
		return certissue.Result{}, nil
	}

	svc.ReconcileCertificates(ctx)

	values, err := svc.settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := CertLastError(values); got != certNoUsableIssuerMessage {
		t.Fatalf("cert_last_error = %q, want %q -- an unusable issuer mode must be visible, not silent",
			got, certNoUsableIssuerMessage)
	}
	// And the note names the setting to change (it is the whole point of writing it).
	if !strings.Contains(certNoUsableIssuerMessage, "acme_email") {
		t.Fatal("the note must name the missing setting")
	}

	// Once the operator supplies the email the pass gets past the gate and the note
	// is cleared -- it must not linger and outlive its cause.
	fixed := "ops@example.test"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{ACMEEmail: &fixed}); err != nil {
		t.Fatalf("set acme_email: %v", err)
	}
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		return selfSigned(t, want.Domain, time.Now().UTC().Add(-time.Hour), 90*24*time.Hour), nil
	}
	svc.ReconcileCertificates(ctx)
	values, err = svc.settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := CertLastError(values); got != "" {
		t.Fatalf("cert_last_error = %q after the cause was fixed, want it cleared", got)
	}
}

// The no-op half of the note above: with the MODULE off the same gate is reached
// (CertSettings returns the zero struct), and the pass must stay a strict no-op --
// no issuance, no store write, and no note either. cert_enabled=false is the
// default, so a note here would appear on every gateway that never enabled the
// module at all.
func TestReconcileWritesNoNoteWhileTheModuleIsOff(t *testing.T) {
	svc, ctx := certEnv(t)
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		t.Fatalf("issued %q although the module is off", want.Domain)
		return certissue.Result{}, nil
	}
	svc.routes = certStoreSpy{t: t}

	svc.ReconcileCertificates(ctx)

	values, err := svc.settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := CertLastError(values); got != "" {
		t.Fatalf("cert_last_error = %q with the module off, want nothing written at all", got)
	}
}
