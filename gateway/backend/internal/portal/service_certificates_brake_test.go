// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/certissue"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
	"testing"
	"time"
)

// ----------------------------------------------------------------------
// Phase 2, Task 6: the CA-rotation propagation brake, agentless servers, and
// the "installed" report the portal column reads.
// ----------------------------------------------------------------------

// fakeCertReport/fakeCertReports is a hand-built AgentCertReportReader: the
// portal package must not import internal/gateway, so the registry itself is
// unavailable here (and irrelevant -- what the brake and the DTO consume is the
// six-value reader contract, nothing more).
type fakeCertReport struct {
	fingerprint string
	caFPs       []string
	mode        string
	notAfter    time.Time
	reportedAt  time.Time
}

type fakeCertReports struct {
	byServer map[string]fakeCertReport
}

func (f *fakeCertReports) CertReport(serverID string) (string, []string, string, time.Time, time.Time, bool) {
	rep, ok := f.byServer[serverID]
	if !ok {
		return "", nil, "", time.Time{}, time.Time{}, false
	}
	return rep.fingerprint, rep.caFPs, rep.mode, rep.notAfter, rep.reportedAt, true
}

// fp builds a syntactically valid 64-hex fingerprint out of one filler
// character, so a test can name distinct roots/leaves readably.
func fp(c string) string { return strings.Repeat(c, 64) }

// gatewayBrakeServer seeds exactly the fleet attributes the gateway trust brake
// consumes. Certificate rows are deliberately absent: P3 defines the fleet from
// AI servers + reporting tokens, not from kind=server certificates.
func gatewayBrakeServer(t *testing.T, svc *Service, ctx context.Context, id, name, status string, withToken bool) {
	t.Helper()
	now := time.Now().UTC()
	if err := svc.routes.CreateAIServer(ctx, routing.AIServer{
		ID: id, Name: name, Domain: id + ".int.example.test", Provider: "vllm",
		Status: status, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create gateway-brake server %s: %v", id, err)
	}
	if withToken {
		if err := svc.routes.UpsertAgentToken(ctx, routing.AgentToken{
			ID: "agt-" + id, ServerID: id, SecretPrefix: "test", CreatedAt: now, UpdatedAt: now,
		}, "hash-"+id); err != nil {
			t.Fatalf("create gateway-brake token %s: %v", id, err)
		}
	}
}

type gatewayAIServersErrStore struct{ routing.Store }

func (gatewayAIServersErrStore) AIServers(context.Context) ([]routing.AIServer, error) {
	return nil, errors.New("store: AI-server list unavailable")
}

type gatewayAgentTokenErrStore struct {
	routing.Store
	serverID string
}

func (s gatewayAgentTokenErrStore) AgentTokenByServer(ctx context.Context, serverID string) (routing.AgentToken, bool, error) {
	if serverID == s.serverID {
		return routing.AgentToken{}, false, errors.New("store: agent-token lookup unavailable")
	}
	return s.Store.AgentTokenByServer(ctx, serverID)
}

// TestGatewayCARotationPropagationBrake pins the safe-direction matrix that is
// intentionally DIFFERENT from the Phase-2 server-leaf brake: a token-bearing
// agent with no durable current-root report holds the gateway leaf, including
// cert_mode=off, but time pressure and the bounded overlap always win.
func TestGatewayCARotationPropagationBrake(t *testing.T) {
	now := time.Now().UTC()
	currentRoot, previousRoot := fp("1"), fp("2")
	rotation := caRotation{rotatedAt: now.Add(-time.Minute), prevFingerprint: previousRoot, prevValid: true}
	want := desiredCert{Domain: "gateway.int.example.test", Kind: "gateway"}
	leaf := routing.Certificate{
		Domain: want.Domain, Kind: want.Kind, Status: "active", IssuerFingerprint: previousRoot,
		NotBefore: now.Add(-10 * 24 * time.Hour), NotAfter: now.Add(80 * 24 * time.Hour),
	}
	settings := CertSettings{RenewBeforeDays: 30}

	t.Run("no report holds the still-valid previous-root leaf", func(t *testing.T) {
		svc, ctx := certEnv(t)
		svc.clock = func() time.Time { return now }
		gatewayBrakeServer(t, svc, ctx, "srv-a", "GPU A", routing.ServerStatusActive, true)
		svc.agentCertReports = &fakeCertReports{byServer: map[string]fakeCertReport{}}
		ready, pending := svc.gatewayTrustPropagation(ctx, currentRoot, rotation)
		if ready || len(pending) != 1 || pending[0].ID != "srv-a" {
			t.Fatalf("ready=%v pending=%+v, want srv-a pending", ready, pending)
		}
		if svc.certDue(want, leaf, settings, now, currentRoot, rotation, ready) {
			t.Fatal("gateway leaf must be held while a relevant agent has no report")
		}
	})

	t.Run("old root and RAM-only new root are not durable", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			fps  []string
		}{
			{name: "old root only", fps: []string{previousRoot}},
			// The report contract carries durable roots only. A root held merely in
			// RAM is therefore represented by its absence from caFPs.
			{name: "new root exists only in RAM", fps: nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				svc, ctx := certEnv(t)
				svc.clock = func() time.Time { return now }
				gatewayBrakeServer(t, svc, ctx, "srv-a", "GPU A", routing.ServerStatusActive, true)
				svc.agentCertReports = &fakeCertReports{byServer: map[string]fakeCertReport{
					"srv-a": {mode: "off", caFPs: tc.fps, reportedAt: now},
				}}
				ready, _ := svc.gatewayTrustPropagation(ctx, currentRoot, rotation)
				if ready {
					t.Fatal("a non-durable current root must hold the gateway leaf")
				}
			})
		}
	})

	t.Run("current root in a durable mode-off report releases", func(t *testing.T) {
		svc, ctx := certEnv(t)
		svc.clock = func() time.Time { return now }
		gatewayBrakeServer(t, svc, ctx, "srv-a", "GPU A", routing.ServerStatusActive, true)
		svc.agentCertReports = &fakeCertReports{byServer: map[string]fakeCertReport{
			"srv-a": {mode: "off", caFPs: []string{previousRoot, currentRoot}, reportedAt: now},
		}}
		ready, pending := svc.gatewayTrustPropagation(ctx, currentRoot, rotation)
		if !ready || len(pending) != 0 {
			t.Fatalf("ready=%v pending=%+v, want all ready", ready, pending)
		}
		if !svc.certDue(want, leaf, settings, now, currentRoot, rotation, ready) {
			t.Fatal("issuer mismatch must re-issue once the complete fleet trusts the current root")
		}
	})

	t.Run("no relevant agents releases immediately", func(t *testing.T) {
		svc, ctx := certEnv(t)
		svc.clock = func() time.Time { return now }
		gatewayBrakeServer(t, svc, ctx, "srv-no-token", "No Agent", routing.ServerStatusActive, false)
		gatewayBrakeServer(t, svc, ctx, "srv-disabled", "Disabled", routing.ServerStatusDisabled, true)
		ready, pending := svc.gatewayTrustPropagation(ctx, currentRoot, rotation)
		if !ready || len(pending) != 0 {
			t.Fatalf("ready=%v pending=%+v, want empty relevant fleet to release", ready, pending)
		}
	})

	t.Run("time due bypasses a non-ready fleet", func(t *testing.T) {
		due := leaf
		due.NotBefore = now.Add(-87 * 24 * time.Hour)
		due.NotAfter = now.Add(3 * 24 * time.Hour)
		if !(&Service{}).certDue(want, due, settings, now, currentRoot, rotation, false) {
			t.Fatal("an expiring gateway leaf must never be held by trust propagation")
		}
	})

	for _, tc := range []struct {
		name string
		rot  caRotation
	}{
		{name: "no valid previous root", rot: caRotation{rotatedAt: now.Add(-time.Minute), prevFingerprint: previousRoot}},
		{name: "rotation older than 24h", rot: caRotation{rotatedAt: now.Add(-certCAPropagationTimeout - time.Minute), prevFingerprint: previousRoot, prevValid: true}},
	} {
		t.Run(tc.name+" releases even with a non-ready fleet", func(t *testing.T) {
			if !(&Service{}).certDue(want, leaf, settings, now, currentRoot, tc.rot, false) {
				t.Fatal("the bounded previous-root overlap must release the leaf")
			}
		})
	}
}

// TestGatewayCARotationPendingServersUsesAllNonDisabledTokenServers proves the
// fleet source and failure direction. No certificate rows are inserted here.
func TestGatewayCARotationPendingServersUsesAllNonDisabledTokenServers(t *testing.T) {
	now := time.Now().UTC()
	currentRoot, previousRoot := fp("3"), fp("4")
	rotation := caRotation{rotatedAt: now.Add(-time.Minute), prevFingerprint: previousRoot, prevValid: true}

	t.Run("active and maintenance token servers are relevant, sorted, and deduplicated", func(t *testing.T) {
		svc, ctx := certEnv(t)
		svc.clock = func() time.Time { return now }
		gatewayBrakeServer(t, svc, ctx, "srv-z", "Zulu GPU", routing.ServerStatusActive, true)
		gatewayBrakeServer(t, svc, ctx, "srv-a", "Alpha GPU", routing.ServerStatusMaintenance, true)
		gatewayBrakeServer(t, svc, ctx, "srv-ready", "Ready GPU", routing.ServerStatusActive, true)
		gatewayBrakeServer(t, svc, ctx, "srv-disabled", "Disabled GPU", routing.ServerStatusDisabled, true)
		gatewayBrakeServer(t, svc, ctx, "srv-no-token", "No Token GPU", routing.ServerStatusActive, false)
		svc.agentCertReports = &fakeCertReports{byServer: map[string]fakeCertReport{
			"srv-ready": {mode: "off", caFPs: []string{currentRoot}, reportedAt: now},
			"srv-z":     {mode: "off", caFPs: []string{previousRoot}, reportedAt: now},
		}}

		ready, pending := svc.gatewayTrustPropagation(ctx, currentRoot, rotation)
		if ready {
			t.Fatal("two relevant agents are not ready")
		}
		want := []CertificateServerRefDTO{{ID: "srv-a", Name: "Alpha GPU"}, {ID: "srv-z", Name: "Zulu GPU"}}
		if len(pending) != len(want) {
			t.Fatalf("pending=%+v, want %+v", pending, want)
		}
		for i := range want {
			if pending[i] != want[i] {
				t.Fatalf("pending[%d]=%+v, want %+v", i, pending[i], want[i])
			}
		}
	})

	t.Run("AI-server list failure holds safely without inventing names", func(t *testing.T) {
		svc, ctx := certEnv(t)
		svc.clock = func() time.Time { return now }
		svc.routes = gatewayAIServersErrStore{Store: svc.routes}
		ready, pending := svc.gatewayTrustPropagation(ctx, currentRoot, rotation)
		if ready || len(pending) != 0 {
			t.Fatalf("ready=%v pending=%+v, want safe hold with no invented refs", ready, pending)
		}
	})

	t.Run("agent-token lookup failure holds and keeps the known server visible", func(t *testing.T) {
		svc, ctx := certEnv(t)
		svc.clock = func() time.Time { return now }
		gatewayBrakeServer(t, svc, ctx, "srv-a", "Alpha GPU", routing.ServerStatusActive, false)
		svc.routes = gatewayAgentTokenErrStore{Store: svc.routes, serverID: "srv-a"}
		ready, pending := svc.gatewayTrustPropagation(ctx, currentRoot, rotation)
		if ready || len(pending) != 1 || pending[0] != (CertificateServerRefDTO{ID: "srv-a", Name: "Alpha GPU"}) {
			t.Fatalf("ready=%v pending=%+v, want the known server pending", ready, pending)
		}
	})
}

// TestGatewayBrakeDoesNotChangeServerLeafBrakeSemantics keeps the complete
// Phase-2 fail-open contract intact while gateway leaves use the stricter fleet
// result. In particular, no report and cert_mode=off remain fail-open for server
// leaves; only a files/proxy agent known to lack the new root is held.
func TestGatewayBrakeDoesNotChangeServerLeafBrakeSemantics(t *testing.T) {
	now := time.Now().UTC()
	currentRoot, previousRoot := fp("5"), fp("6")
	rotation := caRotation{rotatedAt: now.Add(-time.Minute), prevFingerprint: previousRoot, prevValid: true}
	want := desiredCert{Domain: "server.int.example.test", Kind: "server", ServerID: "srv-a"}
	leaf := routing.Certificate{
		Domain: want.Domain, Kind: want.Kind, ServerID: want.ServerID, Status: "active",
		IssuerFingerprint: previousRoot, NotBefore: now.Add(-10 * 24 * time.Hour), NotAfter: now.Add(80 * 24 * time.Hour),
	}
	settings := CertSettings{RenewBeforeDays: 30}

	cases := []struct {
		name    string
		reports map[string]fakeCertReport
		wantDue bool
	}{
		{name: "no report remains fail-open", reports: map[string]fakeCertReport{}, wantDue: true},
		{name: "mode off remains fail-open", reports: map[string]fakeCertReport{"srv-a": {mode: "off", caFPs: []string{previousRoot}, reportedAt: now}}, wantDue: true},
		{name: "files without current root remains held", reports: map[string]fakeCertReport{"srv-a": {mode: "files", fingerprint: fp("9"), caFPs: []string{previousRoot}, reportedAt: now}}, wantDue: false},
		{name: "files with current root releases", reports: map[string]fakeCertReport{"srv-a": {mode: "files", fingerprint: fp("9"), caFPs: []string{previousRoot, currentRoot}, reportedAt: now}}, wantDue: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := certEnv(t)
			svc.agentCertReports = &fakeCertReports{byServer: tc.reports}
			if got := svc.certDue(want, leaf, settings, now, currentRoot, rotation, false); got != tc.wantDue {
				t.Fatalf("certDue=%v, want %v", got, tc.wantDue)
			}
		})
	}
}

// TestRenewDueDecompositionIsBehaviourNeutral: renewDue must still be exactly
// renewDueByTime || issuerMismatch over the whole (status x time x issuer)
// matrix. This is the guard on the refactor itself -- every existing renewal
// expectation in this package rides on renewDue's unchanged meaning, and the
// reconcile pass now consumes the two halves separately.
func TestRenewDueDecompositionIsBehaviourNeutral(t *testing.T) {
	now := time.Now().UTC()
	const currentCA = "current-root"
	cases := []struct {
		name          string
		cert          routing.Certificate
		caFingerprint string
		wantTime      bool
		wantMismatch  bool
	}{
		{
			name:     "active, far from expiry, no issuer",
			cert:     routing.Certificate{Status: "active", NotBefore: now.Add(-10 * 24 * time.Hour), NotAfter: now.Add(80 * 24 * time.Hour)},
			wantTime: false,
		},
		{
			name:     "active, inside the renewal window",
			cert:     routing.Certificate{Status: "active", NotBefore: now.Add(-70 * 24 * time.Hour), NotAfter: now.Add(20 * 24 * time.Hour)},
			wantTime: true,
		},
		{
			name:     "pending (status != active) is due regardless of timing",
			cert:     routing.Certificate{Status: "pending", NotBefore: now.Add(-10 * 24 * time.Hour), NotAfter: now.Add(80 * 24 * time.Hour)},
			wantTime: true,
		},
		{
			name:     "error status is due",
			cert:     routing.Certificate{Status: "error", NotBefore: now.Add(-10 * 24 * time.Hour), NotAfter: now.Add(80 * 24 * time.Hour)},
			wantTime: true,
		},
		{
			name:     "zero not_after is due",
			cert:     routing.Certificate{Status: "active"},
			wantTime: true,
		},
		{
			name:          "zero not_after with a matching issuer is still due (the status/time half alone)",
			cert:          routing.Certificate{Status: "active", IssuerFingerprint: currentCA},
			caFingerprint: currentCA,
			wantTime:      true,
		},
		{
			name:          "internal leaf of the CURRENT root, far from expiry",
			cert:          routing.Certificate{Status: "active", IssuerFingerprint: currentCA, NotBefore: now.Add(-10 * 24 * time.Hour), NotAfter: now.Add(80 * 24 * time.Hour)},
			caFingerprint: currentCA,
			wantTime:      false,
			wantMismatch:  false,
		},
		{
			name:          "internal leaf of a ROTATED-OUT root, far from expiry",
			cert:          routing.Certificate{Status: "active", IssuerFingerprint: "old-root", NotBefore: now.Add(-10 * 24 * time.Hour), NotAfter: now.Add(80 * 24 * time.Hour)},
			caFingerprint: currentCA,
			wantTime:      false,
			wantMismatch:  true,
		},
		{
			name:          "acme leaf (no issuer fingerprint) under a live internal CA",
			cert:          routing.Certificate{Status: "active", NotBefore: now.Add(-10 * 24 * time.Hour), NotAfter: now.Add(80 * 24 * time.Hour)},
			caFingerprint: currentCA,
			wantTime:      false,
			wantMismatch:  false,
		},
		{
			name:     "internal leaf in the acme mode (no current root at all)",
			cert:     routing.Certificate{Status: "active", IssuerFingerprint: "old-root", NotBefore: now.Add(-10 * 24 * time.Hour), NotAfter: now.Add(80 * 24 * time.Hour)},
			wantTime: false,
		},
		{
			name:          "both halves true at once",
			cert:          routing.Certificate{Status: "active", IssuerFingerprint: "old-root", NotBefore: now.Add(-70 * 24 * time.Hour), NotAfter: now.Add(20 * 24 * time.Hour)},
			caFingerprint: currentCA,
			wantTime:      true,
			wantMismatch:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotTime := renewDueByTime(tc.cert, 30, now, "x.example.test")
			if gotTime != tc.wantTime {
				t.Fatalf("renewDueByTime = %v, want %v", gotTime, tc.wantTime)
			}
			gotMismatch := issuerMismatch(tc.cert, tc.caFingerprint)
			if gotMismatch != tc.wantMismatch {
				t.Fatalf("issuerMismatch = %v, want %v", gotMismatch, tc.wantMismatch)
			}
			// The composition is the contract every existing caller relies on.
			if got, want := renewDue(tc.cert, 30, now, "x.example.test", tc.caFingerprint), tc.wantTime || tc.wantMismatch; got != want {
				t.Fatalf("renewDue = %v, want %v (renewDueByTime %v || issuerMismatch %v)", got, want, gotTime, gotMismatch)
			}
		})
	}
}

// TestCARotationPropagatedFailsOpenEverywhereButTheOneHeldBackCase walks the
// COMPLETE fail-open list from caRotationPropagated's doc comment plus the one
// case that is actually held back. The fail-open list is the design, not a set
// of edge cases: in the DEFAULT configuration an agent runs cert_mode=off and
// reports nothing, so a brake that held back on "no report" would freeze every
// rotation for 24h on every deployment.
func TestCARotationPropagatedFailsOpenEverywhereButTheOneHeldBackCase(t *testing.T) {
	now := time.Now().UTC()
	currentRoot := fp("1")
	prevRoot := fp("2")
	freshRotation := caRotation{rotatedAt: now.Add(-time.Minute), prevFingerprint: prevRoot, prevValid: true}
	serverRow := routing.Certificate{Domain: "a.int.example.test", IssuerFingerprint: prevRoot}
	serverWant := desiredCert{Domain: "a.int.example.test", Kind: "server", ServerID: "srv-a"}
	// The agent holds ONLY the outgoing root -> the new one has not arrived.
	staleReports := &fakeCertReports{byServer: map[string]fakeCertReport{
		"srv-a": {fingerprint: fp("9"), mode: "files", caFPs: []string{prevRoot}, reportedAt: now},
	}}

	// The one held-back case first, so every fail-open case below is a genuine
	// deviation from a state that DOES hold back.
	t.Run("held back: the agent installs files and does not have the new root", func(t *testing.T) {
		svc, _ := certEnv(t)
		svc.agentCertReports = staleReports
		if svc.caRotationPropagated(serverWant, serverRow, currentRoot, freshRotation, now) {
			t.Fatal("must hold back: the agent's bundle does not contain the current root yet")
		}
	})

	// Every field left nil/zero keeps the HELD-BACK fixture, so each case deviates
	// in exactly ONE dimension. The overrides are pointers because several of them
	// deviate BY BEING EMPTY (an empty previous fingerprint, an empty issuer).
	cases := []struct {
		name       string
		noRegistry bool
		reports    AgentCertReportReader
		want       *desiredCert
		cert       *routing.Certificate
		rotation   *caRotation
		now        time.Time
	}{
		{
			name:       "no report registry wired",
			noRegistry: true,
		},
		// The Kind and ServerID halves are asserted SEPARATELY: these two carry a
		// server id (which no real gateway/edge row does -- the fixture is synthetic
		// precisely so the Kind clause alone has to reject them) ...
		{
			name: "the row is not kind=server (gateway)",
			want: &desiredCert{Domain: "gw.int.example.test", Kind: "gateway", ServerID: "srv-a"},
		},
		{
			name: "the row is not kind=server (edge)",
			want: &desiredCert{Domain: "edge.int.example.test", Kind: certEdgeKind, ServerID: "srv-a"},
		},
		// ... and this one is a server row with the id missing, so only the ServerID
		// clause can reject it. The report map deliberately carries an entry under
		// the EMPTY key too: without the clause the lookup would find it and the
		// brake would hold back a row it cannot even identify.
		{
			name: "a server row with no server id",
			want: &desiredCert{Domain: "a.int.example.test", Kind: "server"},
			reports: &fakeCertReports{byServer: map[string]fakeCertReport{
				"":      {fingerprint: fp("9"), mode: "files", caFPs: []string{prevRoot}, reportedAt: now},
				"srv-a": {fingerprint: fp("9"), mode: "files", caFPs: []string{prevRoot}, reportedAt: now},
			}},
		},
		{
			name:     "no cert_ca_rotated_at timestamp",
			rotation: &caRotation{prevFingerprint: prevRoot, prevValid: true},
		},
		{
			// A zero rotatedAt must be rejected BY ITS OWN clause, not by accident
			// through the timeout arithmetic: with a near-zero clock,
			// now.Sub(rotatedAt) is small and the timeout would happily wave it
			// through as "a rotation seconds ago" -- which never happened at all.
			name:     "no cert_ca_rotated_at timestamp, even on a degenerate clock",
			rotation: &caRotation{prevFingerprint: prevRoot, prevValid: true},
			now:      time.Time{}.Add(time.Minute),
		},
		{
			// prevFingerprint == "" must be rejected BY ITS OWN clause: a leaf that
			// carries no issuer fingerprint either would otherwise "equal" the empty
			// previous root and get held back for a rotation it has nothing to do with.
			name:     "no previous root recorded, and a leaf with no issuer either",
			cert:     &routing.Certificate{Domain: "a.int.example.test", IssuerFingerprint: ""},
			rotation: &caRotation{rotatedAt: now.Add(-time.Minute), prevValid: true},
		},
		{
			name:     "the wait exceeded the 24h propagation timeout",
			rotation: &caRotation{rotatedAt: now.Add(-certCAPropagationTimeout - time.Minute), prevFingerprint: prevRoot, prevValid: true},
		},
		{
			name:     "no previous root recorded",
			rotation: &caRotation{rotatedAt: now.Add(-time.Minute), prevValid: true},
		},
		{
			name:     "the previous root has already expired (it is out of the bundle)",
			rotation: &caRotation{rotatedAt: now.Add(-time.Minute), prevFingerprint: prevRoot, prevValid: false},
		},
		{
			name: "the row's issuer is NOT the previous root",
			cert: &routing.Certificate{Domain: "a.int.example.test", IssuerFingerprint: fp("7")},
		},
		{
			name:    "NO report exists for this server at all",
			reports: &fakeCertReports{byServer: map[string]fakeCertReport{}},
		},
		{
			name: "the last report says cert_mode=off",
			reports: &fakeCertReports{byServer: map[string]fakeCertReport{
				"srv-a": {mode: "off", reportedAt: now},
			}},
		},
		{
			name: "the last report carries no fingerprint",
			reports: &fakeCertReports{byServer: map[string]fakeCertReport{
				"srv-a": {mode: "files", caFPs: []string{prevRoot}, reportedAt: now},
			}},
		},
		{
			name: "the report already carries the CURRENT root (propagated)",
			reports: &fakeCertReports{byServer: map[string]fakeCertReport{
				"srv-a": {fingerprint: fp("9"), mode: "files", caFPs: []string{prevRoot, currentRoot}, reportedAt: now},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := certEnv(t)
			svc.agentCertReports = staleReports
			if tc.noRegistry {
				svc.agentCertReports = nil
			} else if tc.reports != nil {
				svc.agentCertReports = tc.reports
			}
			want := serverWant
			if tc.want != nil {
				want = *tc.want
			}
			cert := serverRow
			if tc.cert != nil {
				cert = *tc.cert
			}
			rotation := freshRotation
			if tc.rotation != nil {
				rotation = *tc.rotation
			}
			at := now
			if !tc.now.IsZero() {
				at = tc.now
			}
			if !svc.caRotationPropagated(want, cert, currentRoot, rotation, at) {
				t.Fatal("must fail OPEN (allow the re-issue) in this state")
			}
		})
	}
}

// TestReadCARotationParsesTheRotationStateAndSpotsAnExpiredPreviousRoot pins the
// per-pass snapshot: the RFC3339 timestamp newCA writes, the previous root's
// fingerprint, and whether that root is still un-expired (i.e. still in the
// published bundle -- CertificateCABundlePEM's own rule).
func TestReadCARotationParsesTheRotationStateAndSpotsAnExpiredPreviousRoot(t *testing.T) {
	svc, ctx := certEnv(t)
	now := time.Now().UTC()

	if got := svc.readCARotation(ctx, now); !got.rotatedAt.IsZero() || got.prevFingerprint != "" || got.prevValid {
		t.Fatalf("empty settings = %+v, want a zero snapshot", got)
	}

	valid := selfSigned(t, "prev.ca.example.test", now.Add(-24*time.Hour), 240*time.Hour)
	rotatedAt := now.Add(-30 * time.Minute)
	if err := svc.settings.SetSystemSetting(ctx, certCARotatedAtKey, rotatedAt.Format(time.RFC3339), now); err != nil {
		t.Fatal(err)
	}
	if err := svc.settings.SetSystemSetting(ctx, certCAPrevCertKey, valid.FullchainPEM, now); err != nil {
		t.Fatal(err)
	}
	got := svc.readCARotation(ctx, now)
	if got.rotatedAt.Unix() != rotatedAt.Unix() {
		t.Fatalf("rotatedAt = %s, want %s", got.rotatedAt, rotatedAt)
	}
	if got.prevFingerprint != certissue.FingerprintPEM(valid.FullchainPEM) {
		t.Fatalf("prevFingerprint = %q, want the stored root's fingerprint", got.prevFingerprint)
	}
	if !got.prevValid {
		t.Fatal("an un-expired previous root must count as still valid (it is still in the bundle)")
	}

	expired := selfSigned(t, "old.ca.example.test", now.Add(-48*time.Hour), 24*time.Hour)
	if err := svc.settings.SetSystemSetting(ctx, certCAPrevCertKey, expired.FullchainPEM, now); err != nil {
		t.Fatal(err)
	}
	got = svc.readCARotation(ctx, now)
	if got.prevFingerprint == "" {
		t.Fatal("an expired previous root still has a fingerprint")
	}
	if got.prevValid {
		t.Fatal("an EXPIRED previous root must not count as valid: the brake may not pin a leaf to a root that left the bundle")
	}
}

// TestCARotationBrakeHoldsBackTheReissueUntilTheNewRootPropagates is the
// end-to-end proof through a real reconcile pass, a real CA rotation and a real
// stored leaf: while the agent's reported bundle lacks the new root, the leaf is
// NOT re-signed; once the report carries it, the very next pass re-issues.
func TestCARotationBrakeHoldsBackTheReissueUntilTheNewRootPropagates(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 90)
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	svc.cert.issuer = svc.issueCertificate
	reports := &fakeCertReports{byServer: map[string]fakeCertReport{}}
	svc.agentCertReports = reports

	svc.ReconcileCertificates(ctx)
	first, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "active" || first.IssuerFingerprint == "" {
		t.Fatalf("precondition: %+v, want an active internal leaf", first)
	}
	oldRoot := first.IssuerFingerprint
	// The agent reports it installed that leaf and holds the CURRENT (soon
	// previous) root -- exactly the state a healthy deployment is in.
	reports.byServer["srv-a"] = fakeCertReport{
		fingerprint: first.Fingerprint, mode: "files", caFPs: []string{oldRoot}, reportedAt: time.Now().UTC(),
	}

	if err := svc.RotateCertificateCA(ctx, systemToken()); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	newCAView, err := svc.CertificateCAView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if newCAView.Fingerprint == oldRoot {
		t.Fatal("precondition: the rotation must produce a NEW root")
	}
	// Without the brake this pass would re-sign immediately (the issuer-mismatch
	// rule), handing the server a leaf its own clients cannot verify yet.
	svc.ReconcileCertificates(ctx)
	held, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if held.Fingerprint != first.Fingerprint {
		t.Fatal("the leaf must NOT be re-issued while the new root has not reached the agent")
	}
	if held.IssuerFingerprint != oldRoot {
		t.Fatalf("issuer = %q, want the still-trusted previous root %q", held.IssuerFingerprint, oldRoot)
	}

	// The agent has now fetched the new bundle.
	reports.byServer["srv-a"] = fakeCertReport{
		fingerprint: first.Fingerprint, mode: "files",
		caFPs:      []string{oldRoot, newCAView.Fingerprint},
		reportedAt: time.Now().UTC(),
	}
	svc.ReconcileCertificates(ctx)
	second, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if second.IssuerFingerprint != newCAView.Fingerprint {
		t.Fatalf("issuer = %q, want the new root %q once it has propagated", second.IssuerFingerprint, newCAView.Fingerprint)
	}
	if second.Fingerprint == first.Fingerprint {
		t.Fatal("the leaf must actually have been re-issued once the root propagated")
	}
}

// TestCARotationBrakeNeverDelaysAGenuineRenewal is the load-bearing guard: the
// brake gates ONLY the issuer-mismatch reason. A leaf that is genuinely about to
// expire is renewed even though the agent's report proves it does not have the
// new root -- being stuck on an EXPIRED certificate is worse than being handed a
// leaf whose root is still propagating.
//
// Mutation check: make the brake cover the time case too (e.g. wrap the whole of
// certDue's result in caRotationPropagated) and this test fails.
func TestCARotationBrakeNeverDelaysAGenuineRenewal(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 90)
	mustCreateNetbirdServer(t, svc, ctx, "srv-a", "a.int.example.test", "")
	svc.cert.issuer = svc.issueCertificate
	reports := &fakeCertReports{byServer: map[string]fakeCertReport{}}
	svc.agentCertReports = reports

	svc.ReconcileCertificates(ctx)
	first, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	oldRoot := first.IssuerFingerprint
	reports.byServer["srv-a"] = fakeCertReport{
		fingerprint: first.Fingerprint, mode: "files", caFPs: []string{oldRoot}, reportedAt: time.Now().UTC(),
	}
	if err := svc.RotateCertificateCA(ctx, systemToken()); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// Age the stored leaf so it expires in 3 days: now BOTH reasons apply, and the
	// time reason may not be held back.
	now := time.Now().UTC()
	aged := first
	aged.NotBefore = now.Add(-87 * 24 * time.Hour)
	aged.NotAfter = now.Add(3 * 24 * time.Hour)
	aged.UpdatedAt = now
	if err := svc.routes.UpsertCertificate(ctx, aged); err != nil {
		t.Fatal(err)
	}

	svc.ReconcileCertificates(ctx)
	renewed, err := svc.routes.CertificateByDomain(ctx, "a.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Fingerprint == first.Fingerprint {
		t.Fatal("a certificate expiring in 3 days MUST be re-issued regardless of what the agent reported")
	}
	if renewed.NotAfter.Sub(now) < 80*24*time.Hour {
		t.Fatalf("re-issued not_after = %s, want a fresh ~90-day lifetime", renewed.NotAfter)
	}
}

// recordingSettings wraps a SystemSettingsStore and records the ORDER of writes,
// so a test can pin the write sequence a crash-safety argument depends on.
type recordingSettings struct {
	SystemSettingsStore
	order []string
}

func (r *recordingSettings) SetSystemSetting(ctx context.Context, key, value string, now time.Time) error {
	r.order = append(r.order, key)
	return r.SystemSettingsStore.SetSystemSetting(ctx, key, value, now)
}

// TestNewCARecordsTheRotationTimestampBeforeTheRootItself pins both halves of
// the brake's precondition: a rotation ALWAYS leaves cert_ca_rotated_at behind,
// and it is written BEFORE the new root becomes referenceable. A rotation
// without a timestamp would silently disable the brake; a timestamp without a
// rotation is inert (the stored root is unchanged, so every agent already
// reports it).
func TestNewCARecordsTheRotationTimestampBeforeTheRootItself(t *testing.T) {
	svc, ctx := certEnv(t)
	rec := &recordingSettings{SystemSettingsStore: svc.settings}
	svc.settings = rec
	enableSelfSigned(t, svc, ctx, "all", 90)
	rec.order = nil

	svc.ReconcileCertificates(ctx)
	values, err := svc.settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.TrimSpace(values[certCARotatedAtKey])
	if raw == "" {
		t.Fatal("creating the internal CA must record cert_ca_rotated_at")
	}
	if _, perr := time.Parse(time.RFC3339, raw); perr != nil {
		t.Fatalf("cert_ca_rotated_at = %q, want RFC3339: %v", raw, perr)
	}
	assertCAWriteOrder(t, rec.order)

	rec.order = nil
	before := values[certCARotatedAtKey]
	time.Sleep(1100 * time.Millisecond) // RFC3339 second resolution
	if err := svc.RotateCertificateCA(ctx, systemToken()); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	values, err = svc.settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if values[certCARotatedAtKey] == before {
		t.Fatalf("cert_ca_rotated_at = %q unchanged after a rotation", values[certCARotatedAtKey])
	}
	assertCAWriteOrder(t, rec.order)
}

// assertCAWriteOrder checks that within one newCA write block the rotation
// timestamp comes FIRST and the sealed key LAST.
func assertCAWriteOrder(t *testing.T, order []string) {
	t.Helper()
	idx := func(key string) int {
		for i, k := range order {
			if k == key {
				return i
			}
		}
		return -1
	}
	rotated, cert, sealed := idx(certCARotatedAtKey), idx(certCACertKey), idx(certCAKeySealedKey)
	if rotated < 0 || cert < 0 || sealed < 0 {
		t.Fatalf("write order %v is missing one of the CA keys", order)
	}
	if rotated > cert {
		t.Fatalf("write order %v: cert_ca_rotated_at must be written BEFORE the new root, else an aborted rotation leaves a new root with no timestamp and the brake is silently off", order)
	}
	if sealed != len(order)-1 && sealed < cert {
		t.Fatalf("write order %v: the sealed key must stay the LAST write of the block", order)
	}
}

// TestReconcileCertificatesSkipsAServerWithoutAnAgentToken: no agent token means
// no distribution path, so no order is placed. A row that still carries valid
// material is KEPT (it runs out normally) with the reason recorded so the
// omission is visible; a row without material is pruned like any other unwanted
// name.
func TestReconcileCertificatesSkipsAServerWithoutAnAgentToken(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 90)
	mustCreateNetbirdServer(t, svc, ctx, "srv-with", "with.int.example.test", "")
	mustCreateNetbirdServerWithoutAgentToken(t, svc, ctx, "srv-without", "without.int.example.test", "")
	ordered := map[string]int{}
	now := time.Now().UTC()
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		ordered[want.Domain]++
		return selfSigned(t, want.Domain, now.Add(-time.Hour), 90*24*time.Hour), nil
	}

	svc.ReconcileCertificates(ctx)

	if ordered["with.int.example.test"] != 1 {
		t.Fatalf("the server WITH a token got %d orders, want 1", ordered["with.int.example.test"])
	}
	if n := ordered["without.int.example.test"]; n != 0 {
		t.Fatalf("the agentless server got %d orders, want 0 (nothing could ever fetch the material)", n)
	}
	if _, err := svc.routes.CertificateByDomain(ctx, "without.int.example.test"); err == nil {
		t.Fatal("an agentless server with no stored material must not get a row at all")
	}

	// Now the same server DOES have valid material (it had a token earlier, or the
	// token was revoked afterwards): the row survives, carrying the reason.
	valid := selfSigned(t, "without.int.example.test", now.Add(-time.Hour), 90*24*time.Hour)
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: "without.int.example.test", Kind: "server", ServerID: "srv-without",
		FullchainPEM: valid.FullchainPEM, KeySealed: "plain:key", Fingerprint: valid.Fingerprint,
		NotBefore: valid.Leaf.NotBefore, NotAfter: valid.Leaf.NotAfter, IssuedAt: now,
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	svc.ReconcileCertificates(ctx)
	kept, err := svc.routes.CertificateByDomain(ctx, "without.int.example.test")
	if err != nil {
		t.Fatalf("a still-valid certificate must NOT be pruned just because the agent token is gone: %v", err)
	}
	if kept.FullchainPEM != valid.FullchainPEM || kept.KeySealed == "" {
		t.Fatal("the kept row must retain its material (and its sealed key)")
	}
	// The reason is recorded, but the row keeps its OWN status and attempt count:
	// flipping a still-valid row to a non-"active" status would make
	// renewDueByTime's first clause permanently true, so the moment a token appears
	// the row would be re-issued on the spot -- burning an order for a certificate
	// with most of its life left, and resetting the backoff ladder.
	if kept.LastError != certNoAgentTokenReason {
		t.Fatalf("kept row last_error = %q, want %q so the omission is visible", kept.LastError, certNoAgentTokenReason)
	}
	if kept.Status != "active" {
		t.Fatalf("kept row status = %q, want the row's own \"active\" preserved", kept.Status)
	}
	if n := ordered["without.int.example.test"]; n != 0 {
		t.Fatalf("the agentless server got %d orders on the second pass, want 0", n)
	}

	// A row WITHOUT material has nothing to preserve -> pruned.
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: "without.int.example.test", Kind: "server", ServerID: "srv-without",
		Status: "error", LastError: "boom", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	svc.ReconcileCertificates(ctx)
	if _, err := svc.routes.CertificateByDomain(ctx, "without.int.example.test"); err == nil {
		t.Fatal("a material-less row for an agentless server must be pruned")
	}
}

// A row that sat out a while without an agent token must NOT be re-issued the
// instant the token comes back: its material is still valid, and re-issuing would
// burn an order (Let's Encrypt allows 5 duplicates per week) for nothing.
func TestAgentlessRowIsNotReissuedWhenTheTokenReturns(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 30)
	mustCreateNetbirdServerWithoutAgentToken(t, svc, ctx, "srv-x", "x.int.example.test", "")
	now := time.Now().UTC()
	valid := selfSigned(t, "x.int.example.test", now.Add(-time.Hour), 90*24*time.Hour)
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: "x.int.example.test", Kind: "server", ServerID: "srv-x",
		FullchainPEM: valid.FullchainPEM, KeySealed: "plain:key", Fingerprint: valid.Fingerprint,
		NotBefore: valid.Leaf.NotBefore, NotAfter: valid.Leaf.NotAfter, IssuedAt: now,
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	orders := 0
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		orders++
		return selfSigned(t, want.Domain, now.Add(-time.Hour), 90*24*time.Hour), nil
	}

	svc.ReconcileCertificates(ctx) // agentless: the row is kept, the reason recorded

	// The operator generates an agent token -> the server is managed again.
	tokenNow := time.Now().UTC()
	if err := svc.routes.UpsertAgentToken(ctx, routing.AgentToken{
		ID: "agt-srv-x", ServerID: "srv-x", SecretPrefix: "e2e", CreatedAt: tokenNow, UpdatedAt: tokenNow,
	}, "hash-srv-x"); err != nil {
		t.Fatalf("mint agent token: %v", err)
	}
	svc.ReconcileCertificates(ctx)

	if orders != 0 {
		t.Fatalf("orders = %d, want 0: a still-valid certificate must not be re-issued just because the token came back", orders)
	}
	row, err := svc.routes.CertificateByDomain(ctx, "x.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if row.Fingerprint != valid.Fingerprint {
		t.Fatal("the certificate was replaced although it was still valid")
	}
}

// agentTokenErrStore fails ONLY the agent-token lookup and delegates everything
// else, so a test can drive the reconcile's fail-direction on a store blip.
type agentTokenErrStore struct {
	routing.Store
}

func (agentTokenErrStore) AgentTokenByServer(context.Context, string) (routing.AgentToken, bool, error) {
	return routing.AgentToken{}, false, errors.New("store: transient failure")
}

// TestAgentTokenLookupFailureKeepsTheServerManaged: a transient read failure must
// never silently stop certificate management for a server that is in fact fine.
// The agentless exclusion is an "I know there is no agent" statement, and a store
// error is not that knowledge.
func TestAgentTokenLookupFailureKeepsTheServerManaged(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 90)
	mustCreateNetbirdServerWithoutAgentToken(t, svc, ctx, "srv-a", "a.int.example.test", "")
	svc.routes = agentTokenErrStore{Store: svc.routes}
	ordered := 0
	now := time.Now().UTC()
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		ordered++
		return selfSigned(t, want.Domain, now.Add(-time.Hour), 90*24*time.Hour), nil
	}

	svc.ReconcileCertificates(ctx)

	if ordered != 1 {
		t.Fatalf("placed %d orders, want 1: an agent-token lookup ERROR must keep the server managed", ordered)
	}
}

// TestCertificatesViewReportsWhatTheAgentInstalled pins the three states the
// portal "Installiert" column renders: ✓ (the reported leaf fingerprint EQUALS
// the issued one), ✗ (a report exists but differs) and — (never reported, or a
// kind that has no agent at all).
func TestCertificatesViewReportsWhatTheAgentInstalled(t *testing.T) {
	svc, ctx := certEnv(t)
	now := time.Now().UTC()
	reportedAt := now.Add(-2 * time.Minute)
	rows := []routing.Certificate{
		{Domain: "match.int.example.test", Kind: "server", ServerID: "srv-match", Status: "active", Fingerprint: fp("a"), CreatedAt: now, UpdatedAt: now},
		{Domain: "stale.int.example.test", Kind: "server", ServerID: "srv-stale", Status: "active", Fingerprint: fp("b"), CreatedAt: now, UpdatedAt: now},
		{Domain: "quiet.int.example.test", Kind: "server", ServerID: "srv-quiet", Status: "active", Fingerprint: fp("c"), CreatedAt: now, UpdatedAt: now},
		// A gateway row carrying the SAME fingerprint the srv-match report claims AND
		// (deliberately synthetic -- no real gateway row has one) that server's id, so
		// only the kind=server gate can stop it from reading as installed. Nothing but
		// a server has an agent, and an "installed ✓" on a gateway/edge/public row
		// would be a claim about a machine that never reported anything.
		{Domain: "gw.int.example.test", Kind: "gateway", ServerID: "srv-match", Status: "active", Fingerprint: fp("a"), CreatedAt: now, UpdatedAt: now},
	}
	// The certificates table has an FK on server_id (mirrored by the memory store),
	// so the referenced servers have to exist.
	for _, row := range rows {
		if row.ServerID != "" {
			if _, err := svc.routes.AIServerByID(ctx, row.ServerID); err != nil {
				mustCreateNetbirdServerWithoutAgentToken(t, svc, ctx, row.ServerID, row.Domain, "")
			}
		}
		if err := svc.routes.UpsertCertificate(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	svc.agentCertReports = &fakeCertReports{byServer: map[string]fakeCertReport{
		"srv-match": {fingerprint: fp("a"), mode: "files", reportedAt: reportedAt},
		"srv-stale": {fingerprint: fp("d"), mode: "proxy", reportedAt: reportedAt},
	}}

	view, err := svc.CertificatesView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byDomain := map[string]CertificateDTO{}
	for _, c := range view {
		byDomain[c.Domain] = c
	}

	match := byDomain["match.int.example.test"]
	if !match.Installed {
		t.Fatalf("matching fingerprint = %+v, want installed", match)
	}
	if match.InstalledFingerprint != fp("a") || match.InstalledMode != "files" {
		t.Fatalf("installed report = %+v, want the reported fingerprint + mode", match)
	}
	if match.InstalledAt == nil || match.InstalledAt.Unix() != reportedAt.Unix() {
		t.Fatalf("installed_at = %v, want the report time %s (the tooltip shows the age)", match.InstalledAt, reportedAt)
	}

	stale := byDomain["stale.int.example.test"]
	if stale.Installed {
		t.Fatal("a report for a DIFFERENT leaf must not read as installed")
	}
	if stale.InstalledFingerprint != fp("d") || stale.InstalledAt == nil || stale.InstalledMode != "proxy" {
		t.Fatalf("stale report = %+v, want the reported values so the column can render ✗ with a tooltip", stale)
	}

	quiet := byDomain["quiet.int.example.test"]
	if quiet.Installed || quiet.InstalledFingerprint != "" || quiet.InstalledAt != nil || quiet.InstalledMode != "" {
		t.Fatalf("never-reported row = %+v, want all four installed fields at their zero values", quiet)
	}

	gw := byDomain["gw.int.example.test"]
	if gw.Installed || gw.InstalledAt != nil {
		t.Fatalf("a non-server kind = %+v, want no installed claim at all", gw)
	}
}

// TestCertificatesViewWithoutAReportRegistryReportsNothingInstalled: nil reader
// (every deployment before Phase 2 distribution, and every test fixture) leaves
// the four fields alone -- "never reported", never "not installed".
func TestCertificatesViewWithoutAReportRegistryReportsNothingInstalled(t *testing.T) {
	svc, ctx := certEnv(t)
	now := time.Now().UTC()
	mustCreateNetbirdServerWithoutAgentToken(t, svc, ctx, "srv-a", "a.int.example.test", "")
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: "a.int.example.test", Kind: "server", ServerID: "srv-a", Status: "active",
		Fingerprint: fp("a"), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	view, err := svc.CertificatesView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(view) != 1 {
		t.Fatalf("view = %+v, want one row", view)
	}
	if view[0].Installed || view[0].InstalledFingerprint != "" || view[0].InstalledAt != nil || view[0].InstalledMode != "" {
		t.Fatalf("row = %+v, want no installed claim without a report registry", view[0])
	}
}

// fakeAgentTransport is a hand-built AgentTransportReader for the portal-side
// certificate list tests; the concrete registry lives in internal/gateway and
// the portal package must not import it.
type fakeAgentTransport struct {
	byServer map[string]struct {
		transport string
		at        time.Time
	}
}

func (f *fakeAgentTransport) LatestTransport(serverID string) (string, time.Time, bool) {
	if f == nil {
		return "", time.Time{}, false
	}
	rep, ok := f.byServer[serverID]
	if !ok {
		return "", time.Time{}, false
	}
	return rep.transport, rep.at, true
}

// TestMeshTLSPendingServersListsNonTLSTokenServers pins the "who would this lock
// out" list the mesh-gate arming dialog shows: every non-disabled AI server that
// has a reporting-agent token but whose latest mesh hop is not TLS (plain, or
// never observed). A server without a token cannot reach the mesh listener, so it
// is never pending; a disabled server is excluded; a TLS-observed server drops off.
func TestMeshTLSPendingServersListsNonTLSTokenServers(t *testing.T) {
	svc, ctx := certEnv(t)
	now := time.Now().UTC()

	// srv-tls: has token, last hop TLS  -> NOT pending
	// srv-plain: has token, last hop plain -> pending
	// srv-quiet: has token, never observed -> pending
	// srv-notoken: no token -> never pending (can't reach the mesh listener)
	mustCreateNetbirdServer(t, svc, ctx, "srv-tls", "tls.int.example.test", "")
	mustCreateNetbirdServer(t, svc, ctx, "srv-plain", "plain.int.example.test", "")
	mustCreateNetbirdServer(t, svc, ctx, "srv-quiet", "quiet.int.example.test", "")
	mustCreateNetbirdServerWithoutAgentToken(t, svc, ctx, "srv-notoken", "notoken.int.example.test", "")

	// A disabled server with a token must still be excluded.
	mustCreateNetbirdServer(t, svc, ctx, "srv-disabled", "disabled.int.example.test", "")
	if err := svc.routes.UpdateAIServer(ctx, routing.AIServer{
		ID: "srv-disabled", Name: "srv-disabled", Domain: "disabled.int.example.test",
		Provider: "vllm", Status: routing.ServerStatusDisabled, NetbirdEnabled: true, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("disable server: %v", err)
	}

	svc.agentTransport = &fakeAgentTransport{byServer: map[string]struct {
		transport string
		at        time.Time
	}{
		"srv-tls":   {transport: "tls", at: now.Add(-time.Minute)},
		"srv-plain": {transport: "plain", at: now.Add(-time.Minute)},
	}}

	pending := svc.MeshTLSPendingServers(ctx)
	got := make([]string, 0, len(pending))
	for _, ref := range pending {
		got = append(got, ref.ID)
	}
	want := []string{"srv-plain", "srv-quiet"} // normalized: name-then-id sort
	if !slices.Equal(got, want) {
		t.Fatalf("MeshTLSPendingServers = %v, want %v", got, want)
	}
}

// TestCertificatesViewReportsAgentTransport pins how the portal certificate
// list surfaces the observed mesh hop: only a kind=server row picks up the
// observation, a newer TLS stamp beats an older plain stamp (and vice versa),
// and an unobserved row leaves both fields at their zero values so the portal
// renders "—" rather than "plain".
func TestCertificatesViewReportsAgentTransport(t *testing.T) {
	svc, ctx := certEnv(t)
	now := time.Now().UTC()
	tlsAt := now.Add(-30 * time.Second)
	plainAt := now.Add(-2 * time.Minute)
	rows := []routing.Certificate{
		{Domain: "tls.int.example.test", Kind: "server", ServerID: "srv-tls", Status: "active", Fingerprint: fp("1"), CreatedAt: now, UpdatedAt: now},
		{Domain: "plain.int.example.test", Kind: "server", ServerID: "srv-plain", Status: "active", Fingerprint: fp("2"), CreatedAt: now, UpdatedAt: now},
		{Domain: "quiet.int.example.test", Kind: "server", ServerID: "srv-quiet", Status: "active", Fingerprint: fp("3"), CreatedAt: now, UpdatedAt: now},
		// A gateway row is deliberately NOT observable via this column, even if it
		// carries a server_id that has an observation -- only a kind=server row is
		// an agent-hop story.
		{Domain: "gw.int.example.test", Kind: "gateway", ServerID: "srv-tls", Status: "active", Fingerprint: fp("4"), CreatedAt: now, UpdatedAt: now},
	}
	for _, row := range rows {
		if row.ServerID != "" {
			if _, err := svc.routes.AIServerByID(ctx, row.ServerID); err != nil {
				mustCreateNetbirdServerWithoutAgentToken(t, svc, ctx, row.ServerID, row.Domain, "")
			}
		}
		if err := svc.routes.UpsertCertificate(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	svc.agentTransport = &fakeAgentTransport{byServer: map[string]struct {
		transport string
		at        time.Time
	}{
		"srv-tls":   {transport: "tls", at: tlsAt},
		"srv-plain": {transport: "plain", at: plainAt},
	}}

	view, err := svc.CertificatesView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byDomain := map[string]CertificateDTO{}
	for _, c := range view {
		byDomain[c.Domain] = c
	}

	tlsRow := byDomain["tls.int.example.test"]
	if tlsRow.Transport != "tls" || tlsRow.TransportAt == nil || !tlsRow.TransportAt.Equal(tlsAt) {
		t.Fatalf("tls row transport=%q at=%v, want tls at %s", tlsRow.Transport, tlsRow.TransportAt, tlsAt)
	}
	plainRow := byDomain["plain.int.example.test"]
	if plainRow.Transport != "plain" || plainRow.TransportAt == nil || !plainRow.TransportAt.Equal(plainAt) {
		t.Fatalf("plain row transport=%q at=%v, want plain at %s", plainRow.Transport, plainRow.TransportAt, plainAt)
	}
	quiet := byDomain["quiet.int.example.test"]
	if quiet.Transport != "" || quiet.TransportAt != nil {
		t.Fatalf("never-observed row = %+v, want empty transport fields (render '—')", quiet)
	}
	gw := byDomain["gw.int.example.test"]
	if gw.Transport != "" || gw.TransportAt != nil {
		t.Fatalf("non-server row = %+v, want no transport observation at all", gw)
	}
}

// TestCertificatesViewWithoutATransportReaderReportsNothing: nil reader (every
// test fixture that only cares about installed status, and every rolling
// upgrade before the transport registry is wired) leaves the two fields alone.
func TestCertificatesViewWithoutATransportReaderReportsNothing(t *testing.T) {
	svc, ctx := certEnv(t)
	now := time.Now().UTC()
	mustCreateNetbirdServerWithoutAgentToken(t, svc, ctx, "srv-a", "a.int.example.test", "")
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: "a.int.example.test", Kind: "server", ServerID: "srv-a", Status: "active",
		Fingerprint: fp("a"), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	view, err := svc.CertificatesView(ctx)
	if err != nil || len(view) != 1 {
		t.Fatalf("view = %+v (err %v), want one row", view, err)
	}
	if view[0].Transport != "" || view[0].TransportAt != nil {
		t.Fatalf("row = %+v, want no transport claim without a reader", view[0])
	}
}

// --- Task 9: public-domain certificate export ---

func publicExportEnv(t *testing.T, manage bool, publicDomains string) (*Service, context.Context) {
	t.Helper()
	svc, ctx := certEnv(t)
	enabled := true
	mode := IssuerModeSelfSigned
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:            &enabled,
		CertIssuerMode:         &mode,
		CertManagePublicDomain: &manage,
		CertPublicDomains:      &[]string{publicDomains},
	}); err != nil {
		t.Fatalf("seed public settings: %v", err)
	}
	return svc, ctx
}

func TestPublicCertificateExportRequiresManagedConfiguredDomain(t *testing.T) {
	// Management off -> not managed, even for a configured domain with material.
	svc, ctx := publicExportEnv(t, false, "pub.example.test")
	seedPublicCertRow(t, svc, ctx, "pub.example.test")
	if _, err := svc.PublicCertificateBundlePEM(ctx, "pub.example.test"); !errors.Is(err, ErrPublicCertificateNotManaged) {
		t.Fatalf("bundle with management off = %v, want ErrPublicCertificateNotManaged", err)
	}

	// Management on, but the domain is not in the configured list.
	svc2, ctx2 := publicExportEnv(t, true, "pub.example.test")
	seedPublicCertRow(t, svc2, ctx2, "other.example.test")
	if _, err := svc2.PublicCertificateKeyPEM(ctx2, "other.example.test"); !errors.Is(err, ErrPublicCertificateNotManaged) {
		t.Fatalf("key for unconfigured domain = %v, want ErrPublicCertificateNotManaged", err)
	}
}

func TestPublicCertificateExportGatesOnPublicKindRow(t *testing.T) {
	svc, ctx := publicExportEnv(t, true, "pub.example.test")
	// A row EXISTS for the domain but with kind=gateway (a name collision): the
	// export must NOT hand out that mesh/edge key -- 404, never a leak (spec §7).
	now := time.Now().UTC()
	leaf := selfSigned(t, "pub.example.test", now.Add(-time.Hour), 365*24*time.Hour)
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: "pub.example.test", Kind: "gateway", Status: "active",
		FullchainPEM: leaf.FullchainPEM, KeySealed: "plain:GATEWAY-KEY", Fingerprint: leaf.Fingerprint,
		NotBefore: leaf.Leaf.NotBefore.UTC(), NotAfter: leaf.Leaf.NotAfter.UTC(),
		IssuedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed colliding gateway row: %v", err)
	}
	if _, err := svc.PublicCertificateBundlePEM(ctx, "pub.example.test"); !errors.Is(err, ErrCertificateNotFound) {
		t.Fatalf("bundle for a kind=gateway row = %v, want ErrCertificateNotFound", err)
	}
	if _, err := svc.PublicCertificateKeyPEM(ctx, "pub.example.test"); !errors.Is(err, ErrCertificateNotFound) {
		t.Fatalf("key for a kind=gateway row = %v, want ErrCertificateNotFound (no mesh/edge key leak)", err)
	}
}

func TestPublicCertificateBundleAndKeyReturnMaterial(t *testing.T) {
	svc, ctx := publicExportEnv(t, true, "pub.example.test")
	seedPublicCertRow(t, svc, ctx, "pub.example.test")

	bundle, err := svc.PublicCertificateBundlePEM(ctx, "pub.example.test")
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if !strings.Contains(bundle, "BEGIN CERTIFICATE") {
		t.Fatalf("bundle has no certificate block: %q", bundle)
	}
	if strings.Contains(bundle, "PRIVATE KEY") {
		t.Fatalf("bundle leaked a private key")
	}
	key, err := svc.PublicCertificateKeyPEM(ctx, "pub.example.test")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	if key != "PUBLIC-KEY" {
		t.Fatalf("key = %q, want the opened plaintext PUBLIC-KEY", key)
	}
}

func seedPublicCertRow(t *testing.T, svc *Service, ctx context.Context, domain string) {
	t.Helper()
	now := time.Now().UTC()
	leaf := selfSigned(t, domain, now.Add(-time.Hour), 365*24*time.Hour)
	if err := svc.routes.UpsertCertificate(ctx, routing.Certificate{
		Domain: domain, Kind: "public", Status: "active",
		FullchainPEM: leaf.FullchainPEM, KeySealed: "plain:PUBLIC-KEY", Fingerprint: leaf.Fingerprint,
		NotBefore: leaf.Leaf.NotBefore.UTC(), NotAfter: leaf.Leaf.NotAfter.UTC(),
		IssuedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed public cert row: %v", err)
	}
}
