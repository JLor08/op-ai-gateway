// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/certissue"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

type failSystemSettingsReadAt struct {
	SystemSettingsStore
	read    int
	failAt  int
	failErr error
}

type failCertificatesStore struct {
	routing.Store
	err error
}

func (s *failCertificatesStore) Certificates(context.Context) ([]routing.Certificate, error) {
	return nil, s.err
}

func (s *failSystemSettingsReadAt) SystemSettings(ctx context.Context) (map[string]string, error) {
	s.read++
	if s.read == s.failAt {
		return nil, s.failErr
	}
	return s.SystemSettingsStore.SystemSettings(ctx)
}

// storeServerCert puts an ACTIVE server certificate row with real material in the
// store, the way issueAndStore would.
func storeServerCert(t *testing.T, svc *Service, ctx context.Context, domain, serverID string, lifetime time.Duration) certissue.Result {
	t.Helper()
	res := selfSigned(t, domain, time.Now().UTC().Add(-time.Hour), lifetime)
	sealed, err := svc.sealCertSecret(res.KeyPEM)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	now := time.Now().UTC()
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: domain, Kind: "server", ServerID: serverID,
		FullchainPEM: res.FullchainPEM, KeySealed: sealed, Fingerprint: res.Fingerprint,
		NotBefore: res.Leaf.NotBefore, NotAfter: res.Leaf.NotAfter, IssuedAt: now,
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert certificate %s: %v", domain, err)
	}
	return res
}

// enableCertModule turns the module checkbox on WITHOUT completing the issuer
// configuration, so CertSettings' ok is false while cert_enabled is true.
func enableCertModule(t *testing.T, svc *Service, ctx context.Context) {
	t.Helper()
	on := true
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertEnabled: &on}); err != nil {
		t.Fatalf("enable cert module: %v", err)
	}
}

func enableSelfSignedGatewayCertificate(t *testing.T, svc *Service, ctx context.Context, domain string) {
	t.Helper()
	on := true
	mode := IssuerModeSelfSigned
	base := "int.example.test"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:       &on,
		CertIssuerMode:    &mode,
		CertBaseDomain:    &base,
		CertGatewayDomain: &domain,
	}); err != nil {
		t.Fatalf("enable self-signed gateway certificate: %v", err)
	}
}

func TestGatewayMeshCertificateReturnsOneCompleteInternalMaterial(t *testing.T) {
	svc, ctx := certEnv(t)
	const domain = "gateway.int.example.test"
	enableSelfSignedGatewayCertificate(t, svc, ctx, domain)
	now := time.Now().UTC()
	ca, err := svc.newCA(ctx, "int.example.test", now, "")
	if err != nil {
		t.Fatalf("newCA: %v", err)
	}
	issued, err := ca.Issue(domain, 90*24*time.Hour, now)
	if err != nil {
		t.Fatalf("issue gateway leaf: %v", err)
	}
	sealed, err := svc.sealCertSecret(issued.KeyPEM)
	if err != nil {
		t.Fatalf("seal gateway key: %v", err)
	}
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain:            domain,
		Kind:              "gateway",
		FullchainPEM:      issued.FullchainPEM,
		KeySealed:         sealed,
		Fingerprint:       issued.Fingerprint,
		IssuerFingerprint: ca.Fingerprint(),
		NotBefore:         issued.Leaf.NotBefore,
		NotAfter:          issued.Leaf.NotAfter,
		IssuedAt:          now,
		Status:            "active",
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatalf("store gateway certificate: %v", err)
	}
	wantBundle, err := svc.CertificateCABundlePEM(ctx)
	if err != nil {
		t.Fatalf("CertificateCABundlePEM: %v", err)
	}

	got, err := svc.GatewayMeshCertificate(ctx)
	if err != nil {
		t.Fatalf("GatewayMeshCertificate: %v", err)
	}
	if got.Domain != domain || got.FullchainPEM != issued.FullchainPEM || got.KeyPEM != issued.KeyPEM {
		t.Fatalf("gateway material identity mismatch: domain=%q fullchain_match=%v key_match=%v",
			got.Domain, got.FullchainPEM == issued.FullchainPEM, got.KeyPEM == issued.KeyPEM)
	}
	if got.Fingerprint != issued.Fingerprint || got.IssuerFingerprint != ca.Fingerprint() {
		t.Fatalf("gateway fingerprints = leaf %q issuer %q, want leaf %q issuer %q",
			got.Fingerprint, got.IssuerFingerprint, issued.Fingerprint, ca.Fingerprint())
	}
	if !got.NotAfter.Equal(issued.Leaf.NotAfter) {
		t.Fatalf("NotAfter = %s, want %s", got.NotAfter, issued.Leaf.NotAfter)
	}
	if got.CABundlePEM == "" || got.CABundlePEM != wantBundle {
		t.Fatalf("CA bundle is not the stored public trust bundle: got_len=%d want_len=%d", len(got.CABundlePEM), len(wantBundle))
	}
}

func TestGatewayMeshCertificateUsesOneSettingsSnapshotForModuleAndCABundle(t *testing.T) {
	svc, ctx := certEnv(t)
	const domain = "gateway.int.example.test"
	enableSelfSignedGatewayCertificate(t, svc, ctx, domain)
	now := time.Now().UTC()
	_, previousPEM, _, err := certissue.NewCA("Previous CA", 24*time.Hour)
	if err != nil {
		t.Fatalf("create previous CA: %v", err)
	}
	_, currentPEM, _, err := certissue.NewCA("Current CA", 48*time.Hour)
	if err != nil {
		t.Fatalf("create current CA: %v", err)
	}
	for key, value := range map[string]string{
		certCACertKey:     currentPEM,
		certCAPrevCertKey: previousPEM,
	} {
		if err := svc.settings.SetSystemSetting(ctx, key, value, now); err != nil {
			t.Fatalf("store %s: %v", key, err)
		}
	}
	issued := selfSigned(t, domain, now.Add(-time.Hour), 24*time.Hour)
	sealed, err := svc.sealCertSecret(issued.KeyPEM)
	if err != nil {
		t.Fatalf("seal gateway key: %v", err)
	}
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: domain, Kind: "gateway", FullchainPEM: issued.FullchainPEM,
		KeySealed: sealed, Fingerprint: issued.Fingerprint,
		NotBefore: issued.Leaf.NotBefore, NotAfter: issued.Leaf.NotAfter,
		IssuedAt: now, Status: "active", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("store gateway row: %v", err)
	}

	settingsReads := &failSystemSettingsReadAt{SystemSettingsStore: svc.settings}
	svc.settings = settingsReads
	got, err := svc.GatewayMeshCertificate(ctx)
	if err != nil {
		t.Fatalf("GatewayMeshCertificate: %v", err)
	}
	if settingsReads.read != 1 {
		t.Fatalf("SystemSettings reads = %d, want one coherent snapshot", settingsReads.read)
	}
	if got.CABundlePEM != currentPEM+previousPEM {
		t.Fatalf("CA bundle composition mismatch: got_len=%d want_len=%d", len(got.CABundlePEM), len(currentPEM+previousPEM))
	}
}

func TestGatewayMeshCertificateRejectsOffMissingExpiredAndUnreadableRows(t *testing.T) {
	tests := []struct {
		name      string
		moduleOn  bool
		storeRow  bool
		mutateRow func(*routing.Certificate)
		wantErr   error
	}{
		{name: "module off", storeRow: true, wantErr: ErrCertificateNotFound},
		{name: "missing row", moduleOn: true, wantErr: ErrCertificateNotFound},
		{name: "expired", moduleOn: true, storeRow: true, mutateRow: func(row *routing.Certificate) {
			row.NotAfter = time.Now().UTC().Add(-time.Minute)
		}, wantErr: ErrCertificateNotFound},
		{name: "not yet valid", moduleOn: true, storeRow: true, mutateRow: func(row *routing.Certificate) {
			row.NotBefore = time.Now().UTC().Add(time.Hour)
		}, wantErr: ErrCertificateNotFound},
		{name: "zero not before", moduleOn: true, storeRow: true, mutateRow: func(row *routing.Certificate) {
			row.NotBefore = time.Time{}
		}, wantErr: ErrCertificateNotFound},
		{name: "inactive", moduleOn: true, storeRow: true, mutateRow: func(row *routing.Certificate) {
			row.Status = "error"
		}, wantErr: ErrCertificateNotFound},
		{name: "wrong kind", moduleOn: true, storeRow: true, mutateRow: func(row *routing.Certificate) {
			row.Kind = "server"
		}, wantErr: ErrCertificateNotFound},
		{name: "empty domain", moduleOn: true, storeRow: true, mutateRow: func(row *routing.Certificate) {
			row.Domain = "   "
		}, wantErr: ErrCertificateNotFound},
		{name: "empty fingerprint", moduleOn: true, storeRow: true, mutateRow: func(row *routing.Certificate) {
			row.Fingerprint = "   "
		}, wantErr: ErrCertificateNotFound},
		{name: "empty chain", moduleOn: true, storeRow: true, mutateRow: func(row *routing.Certificate) {
			row.FullchainPEM = ""
		}, wantErr: ErrCertificateNotFound},
		{name: "empty key", moduleOn: true, storeRow: true, mutateRow: func(row *routing.Certificate) {
			row.KeySealed = ""
		}, wantErr: ErrCertificateNotFound},
		{name: "unopenable key", moduleOn: true, storeRow: true, mutateRow: func(row *routing.Certificate) {
			row.KeySealed = "enc:AAAA"
		}, wantErr: ErrCertKeyRequired},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, ctx := certEnv(t)
			const domain = "gateway.int.example.test"
			if tt.moduleOn {
				enableSelfSignedGatewayCertificate(t, svc, ctx, domain)
			}
			if tt.storeRow {
				now := time.Now().UTC()
				issued := selfSigned(t, domain, now.Add(-time.Hour), 24*time.Hour)
				sealed, err := svc.sealCertSecret(issued.KeyPEM)
				if err != nil {
					t.Fatalf("seal gateway key: %v", err)
				}
				row := routing.Certificate{
					Domain: domain, Kind: "gateway", FullchainPEM: issued.FullchainPEM,
					KeySealed: sealed, Fingerprint: issued.Fingerprint,
					NotBefore: issued.Leaf.NotBefore, NotAfter: issued.Leaf.NotAfter,
					IssuedAt: now, Status: "active", CreatedAt: now, UpdatedAt: now,
				}
				if tt.mutateRow != nil {
					tt.mutateRow(&row)
				}
				if err := svc.routes.UpsertCertificate(ctx, row); err != nil {
					t.Fatalf("store gateway row: %v", err)
				}
			}

			got, err := svc.GatewayMeshCertificate(ctx)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("GatewayMeshCertificate error = %v, want %v", err, tt.wantErr)
			}
			if got != (GatewayMeshCertificateMaterial{}) {
				t.Fatal("GatewayMeshCertificate returned non-zero material on error")
			}
		})
	}
}

func TestGatewayMeshCertificateRejectsNilRoutesAndPreservesStoreErrors(t *testing.T) {
	t.Run("routes nil", func(t *testing.T) {
		svc, ctx := certEnv(t)
		enableSelfSignedGatewayCertificate(t, svc, ctx, "gateway.int.example.test")
		svc.routes = nil

		got, err := svc.GatewayMeshCertificate(ctx)
		if !errors.Is(err, ErrCertificateNotFound) {
			t.Fatalf("GatewayMeshCertificate error = %v, want %v", err, ErrCertificateNotFound)
		}
		if got != (GatewayMeshCertificateMaterial{}) {
			t.Fatal("GatewayMeshCertificate returned non-zero material with nil routes")
		}
	})

	t.Run("certificate store error", func(t *testing.T) {
		svc, ctx := certEnv(t)
		enableSelfSignedGatewayCertificate(t, svc, ctx, "gateway.int.example.test")
		storeErr := errors.New("certificates: unavailable")
		svc.routes = &failCertificatesStore{Store: svc.routes, err: storeErr}

		got, err := svc.GatewayMeshCertificate(ctx)
		if !errors.Is(err, storeErr) {
			t.Fatalf("GatewayMeshCertificate error = %v, want store error %v", err, storeErr)
		}
		if got != (GatewayMeshCertificateMaterial{}) {
			t.Fatal("GatewayMeshCertificate returned non-zero material on store error")
		}
	})
}

func TestGatewayMeshCertificateSelectsNewestUsableGatewayRowDeterministically(t *testing.T) {
	for _, tc := range []struct {
		name         string
		olderDomain  string
		newestDomain string
	}{
		{name: "newest sorts first", olderDomain: "z-old-gateway.int.example.test", newestDomain: "a-new-gateway.int.example.test"},
		{name: "newest sorts last", olderDomain: "a-old-gateway.int.example.test", newestDomain: "z-new-gateway.int.example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx := certEnv(t)
			enableSelfSignedGatewayCertificate(t, svc, ctx, "gateway.int.example.test")
			now := time.Now().UTC()

			store := func(domain string, notBefore time.Time) certissue.Result {
				t.Helper()
				issued := selfSigned(t, domain, notBefore, 24*time.Hour)
				sealed, err := svc.sealCertSecret(issued.KeyPEM)
				if err != nil {
					t.Fatalf("seal gateway key for %s: %v", domain, err)
				}
				if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
					Domain: domain, Kind: "gateway", FullchainPEM: issued.FullchainPEM,
					KeySealed: sealed, Fingerprint: issued.Fingerprint,
					NotBefore: issued.Leaf.NotBefore, NotAfter: issued.Leaf.NotAfter,
					IssuedAt: notBefore, Status: "active", CreatedAt: now, UpdatedAt: now,
				}); err != nil {
					t.Fatalf("store gateway row %s: %v", domain, err)
				}
				return issued
			}
			_ = store(tc.olderDomain, now.Add(-2*time.Hour))
			newest := store(tc.newestDomain, now.Add(-time.Hour))

			got, err := svc.GatewayMeshCertificate(ctx)
			if err != nil {
				t.Fatalf("GatewayMeshCertificate: %v", err)
			}
			if got.Domain != tc.newestDomain || got.Fingerprint != newest.Fingerprint {
				t.Fatalf("selected gateway material domain=%q fingerprint=%q, want newest domain=%q fingerprint=%q",
					got.Domain, got.Fingerprint, tc.newestDomain, newest.Fingerprint)
			}
			if got.KeyPEM != newest.KeyPEM {
				t.Fatal("selected gateway material did not open the newest row's key")
			}
		})
	}
}

func TestGatewayMeshCertificateTreatsCABundleReadAsBestEffort(t *testing.T) {
	for _, tc := range []struct {
		name      string
		currentCA string
	}{
		{name: "missing CA"},
		{name: "malformed CA", currentCA: "not a certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx := certEnv(t)
			const domain = "gateway.int.example.test"
			enableSelfSignedGatewayCertificate(t, svc, ctx, domain)
			now := time.Now().UTC()
			if tc.currentCA != "" {
				if err := svc.settings.SetSystemSetting(ctx, certCACertKey, tc.currentCA, now); err != nil {
					t.Fatalf("store malformed CA: %v", err)
				}
			}
			issued := selfSigned(t, domain, now.Add(-time.Hour), 24*time.Hour)
			sealed, err := svc.sealCertSecret(issued.KeyPEM)
			if err != nil {
				t.Fatalf("seal gateway key: %v", err)
			}
			if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
				Domain: domain, Kind: "gateway", FullchainPEM: issued.FullchainPEM,
				KeySealed: sealed, Fingerprint: issued.Fingerprint,
				NotBefore: issued.Leaf.NotBefore, NotAfter: issued.Leaf.NotAfter,
				IssuedAt: now, Status: "active", CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("store gateway row: %v", err)
			}

			got, err := svc.GatewayMeshCertificate(ctx)
			if err != nil {
				t.Fatalf("GatewayMeshCertificate returned optional-CA error %v; leaf/key material must remain usable", err)
			}
			if got.FullchainPEM != issued.FullchainPEM || got.KeyPEM != issued.KeyPEM {
				t.Fatalf("optional CA changed material: fullchain_match=%v key_match=%v",
					got.FullchainPEM == issued.FullchainPEM, got.KeyPEM == issued.KeyPEM)
			}
			if got.CABundlePEM != "" {
				t.Fatalf("CA bundle = %q, want empty best-effort fallback", got.CABundlePEM)
			}
		})
	}
}

func TestGatewayMeshCertificatePreservesSettingsReadErrors(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSignedGatewayCertificate(t, svc, ctx, "gateway.int.example.test")
	storeErr := errors.New("settings: unavailable")
	svc.settings = &failSystemSettingsReadAt{
		SystemSettingsStore: svc.settings,
		failAt:              1,
		failErr:             storeErr,
	}
	if _, err := svc.GatewayMeshCertificate(ctx); !errors.Is(err, storeErr) {
		t.Fatalf("GatewayMeshCertificate error = %v, want settings error %v", err, storeErr)
	}
}

func TestAgentCertificateServesTheServersOwnMaterial(t *testing.T) {
	svc, ctx := certEnv(t)
	enableCertModule(t, svc, ctx)
	mustCreateNetbirdServer(t, svc, ctx, "s1", "a.int.example.test", "")
	res := storeServerCert(t, svc, ctx, "a.int.example.test", "s1", 90*24*time.Hour)

	dto, err := svc.AgentCertificate(ctx, "s1")
	if err != nil {
		t.Fatalf("AgentCertificate: %v", err)
	}
	if dto.Fingerprint != res.Fingerprint {
		t.Fatalf("fingerprint = %q, want %q", dto.Fingerprint, res.Fingerprint)
	}
	if dto.FullchainPEM != res.FullchainPEM {
		t.Fatal("fullchain does not match the stored row")
	}
	if dto.KeyPEM != res.KeyPEM {
		t.Fatal("key was not opened back to the issued private key")
	}
	if dto.ETag == "" || dto.NotAfter == nil {
		t.Fatalf("dto = %+v (etag + not_after must be populated)", dto)
	}
}

// The security property: the server comes from the caller's own id, so a second
// server's material is never reachable. The other server's domain deliberately sorts
// FIRST, so a lookup that ignored the id (or took "the alphabetically first row")
// would hand out the wrong certificate.
func TestAgentCertificateNeverServesAnotherServer(t *testing.T) {
	svc, ctx := certEnv(t)
	enableCertModule(t, svc, ctx)
	mustCreateNetbirdServer(t, svc, ctx, "s1", "zzz.int.example.test", "")
	mustCreateNetbirdServer(t, svc, ctx, "s2", "aaa.int.example.test", "")
	own := storeServerCert(t, svc, ctx, "zzz.int.example.test", "s1", 90*24*time.Hour)
	other := storeServerCert(t, svc, ctx, "aaa.int.example.test", "s2", 90*24*time.Hour)

	dto, err := svc.AgentCertificate(ctx, "s1")
	if err != nil {
		t.Fatalf("AgentCertificate: %v", err)
	}
	if dto.Fingerprint != own.Fingerprint {
		t.Fatalf("fingerprint = %q, want s1's %q", dto.Fingerprint, own.Fingerprint)
	}
	if dto.Fingerprint == other.Fingerprint || dto.KeyPEM == other.KeyPEM {
		t.Fatal("s1 was served s2's material")
	}
	// And the reverse direction, so the test cannot pass by always picking s1.
	dto2, err := svc.AgentCertificate(ctx, "s2")
	if err != nil {
		t.Fatalf("AgentCertificate(s2): %v", err)
	}
	if dto2.Fingerprint != other.Fingerprint {
		t.Fatalf("s2 fingerprint = %q, want %q", dto2.Fingerprint, other.Fingerprint)
	}
}

// A failed re-issue after a rename leaves a second, material-LESS row for the same
// server (recordCertFailure). If its domain sorts first, a naive
// CertificateByServer-style lookup hands the agent that empty row and the working
// certificate becomes invisible -- Plan A's shipped I1 defect. The row with usable
// material must win.
func TestAgentCertificatePrefersTheRowWithMaterial(t *testing.T) {
	svc, ctx := certEnv(t)
	enableCertModule(t, svc, ctx)
	mustCreateNetbirdServer(t, svc, ctx, "s1", "www.int.example.test", "")
	good := storeServerCert(t, svc, ctx, "www.int.example.test", "s1", 90*24*time.Hour)

	now := time.Now().UTC()
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: "aaa.int.example.test", Kind: "server", ServerID: "s1",
		Status: "error", LastError: "issue failed", AttemptCount: 3,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert failed row: %v", err)
	}

	dto, err := svc.AgentCertificate(ctx, "s1")
	if err != nil {
		t.Fatalf("AgentCertificate: %v", err)
	}
	if dto.Fingerprint != good.Fingerprint || dto.Domain != "www.int.example.test" {
		t.Fatalf("served %q/%q, want the row that actually has material", dto.Domain, dto.Fingerprint)
	}
}

func TestAgentCertificateNotFoundCases(t *testing.T) {
	t.Run("module off", func(t *testing.T) {
		svc, ctx := certEnv(t)
		mustCreateNetbirdServer(t, svc, ctx, "s1", "a.int.example.test", "")
		storeServerCert(t, svc, ctx, "a.int.example.test", "s1", 90*24*time.Hour)
		if _, err := svc.AgentCertificate(ctx, "s1"); !errors.Is(err, ErrCertificateNotFound) {
			t.Fatalf("err = %v, want ErrCertificateNotFound (cert_enabled is off)", err)
		}
	})
	t.Run("no certificate", func(t *testing.T) {
		svc, ctx := certEnv(t)
		enableCertModule(t, svc, ctx)
		mustCreateNetbirdServer(t, svc, ctx, "s1", "a.int.example.test", "")
		if _, err := svc.AgentCertificate(ctx, "s1"); !errors.Is(err, ErrCertificateNotFound) {
			t.Fatalf("err = %v, want ErrCertificateNotFound", err)
		}
	})
	t.Run("expired material", func(t *testing.T) {
		svc, ctx := certEnv(t)
		enableCertModule(t, svc, ctx)
		mustCreateNetbirdServer(t, svc, ctx, "s1", "a.int.example.test", "")
		// notBefore 2h ago with a 1h lifetime -> already expired.
		res := selfSigned(t, "a.int.example.test", time.Now().UTC().Add(-2*time.Hour), time.Hour)
		sealed, err := svc.sealCertSecret(res.KeyPEM)
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		now := time.Now().UTC()
		if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
			Domain: "a.int.example.test", Kind: "server", ServerID: "s1",
			FullchainPEM: res.FullchainPEM, KeySealed: sealed, Fingerprint: res.Fingerprint,
			NotBefore: res.Leaf.NotBefore, NotAfter: res.Leaf.NotAfter,
			Status: "active", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if _, err := svc.AgentCertificate(ctx, "s1"); !errors.Is(err, ErrCertificateNotFound) {
			t.Fatalf("err = %v, want ErrCertificateNotFound (expired leaf must not be handed out)", err)
		}
	})
	t.Run("empty server id", func(t *testing.T) {
		svc, ctx := certEnv(t)
		enableCertModule(t, svc, ctx)
		if _, err := svc.AgentCertificate(ctx, ""); !errors.Is(err, ErrCertificateNotFound) {
			t.Fatalf("err = %v, want ErrCertificateNotFound", err)
		}
	})
}

// The continuity guarantee behind gating on the RAW cert_enabled instead of
// CertSettings' ok: an incomplete issuer configuration (acme with no acme_email) must
// NOT withdraw an already-issued, still-valid certificate from a running agent.
func TestAgentCertificateServesDespiteIncompleteIssuerConfig(t *testing.T) {
	svc, ctx := certEnv(t)
	on := true
	mode := IssuerModeACME
	empty := ""
	base := "int.example.test"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled: &on, CertIssuerMode: &mode, ACMEEmail: &empty, CertBaseDomain: &base,
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	if _, ok, err := svc.CertSettings(ctx); err != nil || ok {
		t.Fatalf("precondition: CertSettings ok = %v (want false), err = %v", ok, err)
	}
	mustCreateNetbirdServer(t, svc, ctx, "s1", "a.int.example.test", "")
	res := storeServerCert(t, svc, ctx, "a.int.example.test", "s1", 90*24*time.Hour)

	dto, err := svc.AgentCertificate(ctx, "s1")
	if err != nil {
		t.Fatalf("AgentCertificate: %v -- an incomplete issuer configuration must not withdraw a valid certificate", err)
	}
	if dto.Fingerprint != res.Fingerprint {
		t.Fatalf("fingerprint = %q, want %q", dto.Fingerprint, res.Fingerprint)
	}
}

// THE blocker regression: a CA rotation publishes the new root BEFORE re-signing any
// leaf, so at that moment every leaf fingerprint is unchanged. An ETag over the leaf
// alone would answer 304 and the new root would never reach the agent -- which would
// also make the CA-rotation propagation brake unsatisfiable. The validator must
// therefore change when the BUNDLE changes.
func TestAgentCertificateETagCoversTheTrustBundle(t *testing.T) {
	svc, ctx := certEnv(t)
	enableCertModule(t, svc, ctx)
	mustCreateNetbirdServer(t, svc, ctx, "s1", "a.int.example.test", "")
	storeServerCert(t, svc, ctx, "a.int.example.test", "s1", 90*24*time.Hour)

	before, err := svc.AgentCertificate(ctx, "s1")
	if err != nil {
		t.Fatalf("AgentCertificate: %v", err)
	}

	// Mint an internal CA: the leaf row is untouched, only the bundle changes.
	if _, err := svc.newCA(ctx, "int.example.test", time.Now().UTC(), ""); err != nil {
		t.Fatalf("newCA: %v", err)
	}
	after, err := svc.AgentCertificate(ctx, "s1")
	if err != nil {
		t.Fatalf("AgentCertificate after CA change: %v", err)
	}
	if after.Fingerprint != before.Fingerprint {
		t.Fatalf("precondition broken: the leaf changed (%q -> %q), so this test would pass for the wrong reason",
			before.Fingerprint, after.Fingerprint)
	}
	if after.CABundlePEM == before.CABundlePEM {
		t.Fatal("precondition broken: the bundle did not change")
	}
	if after.ETag == before.ETag {
		t.Fatal("the ETag did not change although the trust bundle did -- a CA rotation would answer 304 and never reach the agent")
	}
}

// The bundle is empty exactly when no internal root is stored -- NOT "when the mode
// is acme". certEnv has no CA, so the bundle is empty here even in acme mode.
func TestAgentCertificateBundleEmptyWithoutInternalCA(t *testing.T) {
	svc, ctx := certEnv(t)
	enableCertModule(t, svc, ctx)
	mustCreateNetbirdServer(t, svc, ctx, "s1", "a.int.example.test", "")
	storeServerCert(t, svc, ctx, "a.int.example.test", "s1", 90*24*time.Hour)

	dto, err := svc.AgentCertificate(ctx, "s1")
	if err != nil {
		t.Fatalf("AgentCertificate: %v", err)
	}
	if dto.CABundlePEM != "" {
		t.Fatalf("ca bundle = %q, want empty (no internal root is stored)", dto.CABundlePEM)
	}
}

// The DTO carries the key on purpose, but nothing else may: a stored row whose key
// cannot be opened must fail rather than produce a chain without its key.
func TestAgentCertificateRefusesUnopenableKey(t *testing.T) {
	svc, ctx := certEnv(t)
	enableCertModule(t, svc, ctx)
	mustCreateNetbirdServer(t, svc, ctx, "s1", "a.int.example.test", "")
	res := selfSigned(t, "a.int.example.test", time.Now().UTC().Add(-time.Hour), 90*24*time.Hour)
	now := time.Now().UTC()
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: "a.int.example.test", Kind: "server", ServerID: "s1",
		FullchainPEM: res.FullchainPEM, KeySealed: "enc:not-base64!", Fingerprint: res.Fingerprint,
		NotBefore: res.Leaf.NotBefore, NotAfter: res.Leaf.NotAfter,
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	dto, err := svc.AgentCertificate(ctx, "s1")
	if err == nil {
		t.Fatalf("expected an error, got dto = %+v", dto)
	}
	if dto.KeyPEM != "" || dto.FullchainPEM != "" {
		t.Fatalf("a failed key open still returned material: %+v", dto)
	}
}

// Two simultaneously-valid rows for one server are reachable: the prune's delete is
// best-effort (a failure is only Debug-logged), so a rename can leave the old row
// behind. Ordering by domain alone would then deterministically serve the STALE
// certificate -- the agent would install a chain for a name the server no longer
// answers to. The freshest material must win; the domain is only the final tiebreak.
func TestAgentCertificatePrefersTheFreshestRow(t *testing.T) {
	svc, ctx := certEnv(t)
	enableCertModule(t, svc, ctx)
	mustCreateNetbirdServer(t, svc, ctx, "s1", "new.int.example.test", "")

	now := time.Now().UTC()
	// The stale row's domain sorts FIRST, so a domain-ordered pick would take it.
	stale := selfSigned(t, "aaa-old.int.example.test", now.Add(-30*24*time.Hour), 90*24*time.Hour)
	fresh := selfSigned(t, "new.int.example.test", now.Add(-time.Hour), 90*24*time.Hour)
	for _, row := range []struct {
		domain string
		res    certissue.Result
	}{{"aaa-old.int.example.test", stale}, {"new.int.example.test", fresh}} {
		sealed, err := svc.sealCertSecret(row.res.KeyPEM)
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
			Domain: row.domain, Kind: "server", ServerID: "s1",
			FullchainPEM: row.res.FullchainPEM, KeySealed: sealed, Fingerprint: row.res.Fingerprint,
			NotBefore: row.res.Leaf.NotBefore, NotAfter: row.res.Leaf.NotAfter,
			IssuedAt: row.res.Leaf.NotBefore, Status: "active", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}

	dto, err := svc.AgentCertificate(ctx, "s1")
	if err != nil {
		t.Fatalf("AgentCertificate: %v", err)
	}
	if dto.Domain != "new.int.example.test" || dto.Fingerprint != fresh.Fingerprint {
		t.Fatalf("served %q (%s), want the freshest row new.int.example.test -- the stale row's "+
			"domain sorts first, so this is exactly the ordering trap", dto.Domain, dto.Fingerprint)
	}
}
