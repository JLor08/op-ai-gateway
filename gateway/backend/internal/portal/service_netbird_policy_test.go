// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/netbird"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// errAppsStore wraps a MemoryStore but forces ApplicationsByServer to error — used to
// prove the reconcile KEEPS an existing policy on a transient store failure (never
// deletes it as if there were no active apps).
type errAppsStore struct {
	*routing.MemoryStore
}

func (e *errAppsStore) ApplicationsByServer(context.Context, string) ([]routing.Application, error) {
	return nil, errors.New("boom: transient store error")
}

// enableNetbirdPolicies configures the NetBird module AND policy management with an
// explicit scope + deny-by-default flags (so the effective scope is deterministic).
//
// Since UpdateSystemSettingsRequest sets NetbirdManagePolicies/NetbirdPolicyScope/
// NetbirdDenyByDefault here, this call itself now (T5) triggers UpdateSystemSettings'
// background policy-fleet side effect (applyPolicySettingsSideEffects, fired in a
// goroutine so the settings PUT never blocks on NetBird calls). Left unsettled, that
// goroutine can run at ANY point relative to a test's own subsequent seeding +
// reconcile calls — including racing ahead to reconcile a server/policy the test
// seeds moments later — which is a genuine (not merely test-artifact) source of
// nondeterminism for any test whose fake NetBird double does not persist a PUT's
// effect into its own listable state (so a second, unrelated reconcile pass over the
// same drifted policy performs its own redundant update). waitPolicySideEffects
// blocks until that goroutine has definitively finished before this helper returns,
// so every caller's subsequent assertions are deterministic.
func enableNetbirdPolicies(t *testing.T, svc *Service, url, scope string, deny, enforce bool) {
	t.Helper()
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdEnabled:              boolPtr(true),
		NetbirdURL:                  strPtr(url),
		NetbirdGroups:               &[]string{"gateways"},
		NetbirdToken:                strPtr("nbtok"),
		NetbirdManagePolicies:       boolPtr(true),
		NetbirdPolicyScope:          strPtr(scope),
		NetbirdDenyByDefault:        boolPtr(deny),
		NetbirdDenyByDefaultEnforce: boolPtr(enforce),
	}); err != nil {
		t.Fatalf("configure netbird policies: %v", err)
	}
	svc.waitPolicySideEffects()
}

// seedManagedNetbirdServer creates a NetBird-enabled server with a tracking group +
// policy override, directly in the store (bypassing the create hook).
func seedManagedNetbirdServer(t *testing.T, routeStore *routing.MemoryStore, id, name, trackingGroupID, override string, now time.Time) {
	t.Helper()
	srv := routing.AIServer{
		ID: id, Name: name, Status: routing.ServerStatusActive,
		NetbirdEnabled: true, NetbirdGroupID: trackingGroupID, NetbirdPolicyOverride: override,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := routeStore.CreateAIServer(context.Background(), srv); err != nil {
		t.Fatalf("CreateAIServer %s: %v", id, err)
	}
}

// seedApp creates an application on a server with a port + status.
func seedApp(t *testing.T, routeStore *routing.MemoryStore, id, serverID string, port int, status string, now time.Time) {
	t.Helper()
	if err := routeStore.CreateApplication(context.Background(), routing.Application{
		ID: id, ServerID: serverID, Port: port, Status: status, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateApplication %s: %v", id, err)
	}
}

// policyRuleField extracts a rule field from a recorded write-shape policy body:
// the sole rule's ports/sources/destinations, each a []string.
func policyRuleField(t *testing.T, body map[string]any, key string) []string {
	t.Helper()
	rules, _ := body["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("policy body rules = %v, want exactly 1", body["rules"])
	}
	rule, _ := rules[0].(map[string]any)
	raw, _ := rule[key].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// ---- serverManaged matrix (pure) -----------------------------------------------

func TestServerManagedMatrix(t *testing.T) {
	nb := func(enabled bool, group, override string) routing.AIServer {
		return routing.AIServer{NetbirdEnabled: enabled, NetbirdGroupID: group, NetbirdPolicyOverride: override}
	}
	cases := []struct {
		name   string
		server routing.AIServer
		scope  string
		want   bool
	}{
		// all-mode: managed unless "exclude" (and only NetBird+group).
		{"all/default", nb(true, "g", ""), "all", true},
		{"all/include", nb(true, "g", "include"), "all", true},
		{"all/exclude", nb(true, "g", "exclude"), "all", false},
		// selected-mode: managed only if "include".
		{"selected/default", nb(true, "g", ""), "selected", false},
		{"selected/include", nb(true, "g", "include"), "selected", true},
		{"selected/exclude", nb(true, "g", "exclude"), "selected", false},
		// never when not a NetBird peer / no tracking group.
		{"all/not-netbird", nb(false, "g", "include"), "all", false},
		{"all/no-group", nb(true, "", "include"), "all", false},
		{"selected/no-group", nb(true, "", "include"), "selected", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := serverManaged(tc.server, tc.scope); got != tc.want {
				t.Fatalf("serverManaged(%+v, %q) = %v, want %v", tc.server, tc.scope, got, tc.want)
			}
		})
	}
}

// ---- activePortStrings dedup/sort/inactive (pure) ------------------------------

func TestActivePortStringsDedupSortInactive(t *testing.T) {
	apps := []routing.Application{
		{Port: 9090, Status: routing.ServerStatusActive},
		{Port: 8080, Status: routing.ServerStatusActive},
		{Port: 8080, Status: routing.ServerStatusActive},   // duplicate active port -> collapsed
		{Port: 7000, Status: routing.ServerStatusDisabled}, // inactive -> excluded
		{Port: 0, Status: routing.ServerStatusActive},      // non-positive -> excluded
	}
	got := activePortStrings(apps)
	want := []string{"8080", "9090"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("activePortStrings = %v, want %v (sorted, deduped, inactive/zero excluded)", got, want)
	}
}

// TestActivePortStringsIncludesProxyListenPortForProxiedHTTPS pins the P4 fix:
// a proxied-https app (Scheme "https" + non-zero ProxyListenPort) contributes
// BOTH its plaintext Port AND its ProxyListenPort to the managed access-policy
// port set (union), so a forward auto-switch (route -> ProxyListenPort) is
// reachable over the mesh and a later revert (route -> Port) has no policy-lag
// window. A plain-http app and an own-TLS-https app (ProxyListenPort 0)
// contribute only their Port; an inactive app contributes nothing.
func TestActivePortStringsIncludesProxyListenPortForProxiedHTTPS(t *testing.T) {
	apps := []routing.Application{
		{Port: 8080, Scheme: "https", ProxyListenPort: 8600, Status: routing.ServerStatusActive},   // proxied -> both 8080 & 8600
		{Port: 9090, Scheme: "http", Status: routing.ServerStatusActive},                           // plain http -> 9090 only
		{Port: 7000, Scheme: "https", ProxyListenPort: 0, Status: routing.ServerStatusActive},      // own-TLS https -> 7000 only
		{Port: 6000, Scheme: "https", ProxyListenPort: 6001, Status: routing.ServerStatusDisabled}, // inactive -> excluded entirely
	}
	got := activePortStrings(apps)
	want := []string{"7000", "8080", "8600", "9090"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("activePortStrings = %v, want %v (proxied-https opens BOTH Port and ProxyListenPort; inactive excluded)", got, want)
	}
}

// ---- reconcileServerPolicy ------------------------------------------------------

// TestReconcileServerPolicyCreatesForActiveAppPorts: a managed (all-mode) server
// with active apps on 8080 & 9090 (+ an inactive app on 7000) and no existing policy
// yields exactly ONE CreatePolicy with ports ["8080","9090"], source = gateway group,
// destination = tracking group, rule accept/tcp/unidirectional.
func TestReconcileServerPolicyCreatesForActiveAppPorts(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal") // ResolveGroupID finds it -> deterministic source id
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)

	seedManagedNetbirdServer(t, routeStore, "srv1", "GPU", "track-1", "", now)
	seedApp(t, routeStore, "app-8080", "srv1", 8080, routing.ServerStatusActive, now)
	seedApp(t, routeStore, "app-9090", "srv1", 9090, routing.ServerStatusActive, now)
	seedApp(t, routeStore, "app-7000", "srv1", 7000, routing.ServerStatusDisabled, now)

	svc.reconcileServerPolicy(context.Background(), "srv1")

	if fake.policyCreateCount() != 1 {
		t.Fatalf("policy creates = %d, want 1", fake.policyCreateCount())
	}
	if fake.policyUpdateCount() != 0 || fake.deletedPolicyCount() != 0 {
		t.Fatalf("unexpected update/delete: updates=%d deletes=%d", fake.policyUpdateCount(), fake.deletedPolicyCount())
	}
	body := fake.lastCreatedPolicy()
	if body == nil {
		t.Fatalf("no CreatePolicy body recorded")
	}
	if name, _ := body["name"].(string); name != "op-gw-access-srv1" {
		t.Fatalf("policy name = %q, want op-gw-access-srv1", name)
	}
	if enabled, _ := body["enabled"].(bool); !enabled {
		t.Fatalf("policy enabled = false, want true")
	}
	if ports := policyRuleField(t, body, "ports"); !reflect.DeepEqual(ports, []string{"8080", "9090"}) {
		t.Fatalf("policy ports = %v, want [8080 9090]", ports)
	}
	if src := policyRuleField(t, body, "sources"); !reflect.DeepEqual(src, []string{"gw-portal"}) {
		t.Fatalf("policy sources = %v, want [gw-portal] (gateway group)", src)
	}
	if dst := policyRuleField(t, body, "destinations"); !reflect.DeepEqual(dst, []string{"track-1"}) {
		t.Fatalf("policy destinations = %v, want [track-1] (tracking group)", dst)
	}
	rule := body["rules"].([]any)[0].(map[string]any)
	if rule["action"] != "accept" || rule["protocol"] != "tcp" {
		t.Fatalf("rule action/protocol = %v/%v, want accept/tcp", rule["action"], rule["protocol"])
	}
	if bidi, _ := rule["bidirectional"].(bool); bidi {
		t.Fatalf("rule bidirectional = true, want false")
	}
}

// TestReconcileServerPolicyOpensProxyListenPortForProxiedApp is the policy-layer
// guard for the P4 fix: a managed server whose app is proxied-https (Scheme
// "https" + ProxyListenPort 8600 on plaintext Port 8080) yields an
// op-gw-access-<serverID> policy whose ports include BOTH 8080 and 8600 -- so a
// forward auto-switch that routes gateway->server:8600 is not denied by NetBird
// deny-by-default (which would also fail the health probe and drop the app).
func TestReconcileServerPolicyOpensProxyListenPortForProxiedApp(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)

	seedManagedNetbirdServer(t, routeStore, "srvP", "GPU", "track-P", "", now)
	// A proxied-https app: seed directly so Scheme/ProxyListenPort are set.
	if err := routeStore.CreateApplication(context.Background(), routing.Application{
		ID: "app-proxied", ServerID: "srvP", Port: 8080, Scheme: "https", ProxyListenPort: 8600,
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateApplication app-proxied: %v", err)
	}

	svc.reconcileServerPolicy(context.Background(), "srvP")

	if fake.policyCreateCount() != 1 {
		t.Fatalf("policy creates = %d, want 1", fake.policyCreateCount())
	}
	body := fake.lastCreatedPolicy()
	if body == nil {
		t.Fatalf("no CreatePolicy body recorded")
	}
	if ports := policyRuleField(t, body, "ports"); !reflect.DeepEqual(ports, []string{"8080", "8600"}) {
		t.Fatalf("policy ports = %v, want [8080 8600] (plaintext Port AND ProxyListenPort)", ports)
	}
}

// TestReconcileServerPolicyDeletesWhenNotManaged: an existing policy is DELETED when
// the server is not managed — (a) all-mode + override "exclude", and (b) selected-mode
// with no "include" override.
func TestReconcileServerPolicyDeletesWhenNotManaged(t *testing.T) {
	t.Run("all/exclude", func(t *testing.T) {
		now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
		svc, routeStore := newNetbirdServerTestService(t, now)
		fake := newFakeNetbird(t)
		fake.seedGroup("gw-portal", "op-gw-portal")
		enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)
		seedManagedNetbirdServer(t, routeStore, "srvE", "S", "track-E", "exclude", now)
		seedApp(t, routeStore, "app-E", "srvE", 8080, routing.ServerStatusActive, now)
		fake.seedPolicy("pol-E", "op-gw-access-srvE", true, []string{"8080"}, "gw-portal", "track-E")

		svc.reconcileServerPolicy(context.Background(), "srvE")

		if !fake.wasPolicyDeleted("pol-E") {
			t.Fatalf("policy pol-E was not deleted (exclude in all-mode)")
		}
		if fake.policyCreateCount() != 0 || fake.policyUpdateCount() != 0 {
			t.Fatalf("unexpected create/update on delete path: c=%d u=%d", fake.policyCreateCount(), fake.policyUpdateCount())
		}
	})
	t.Run("selected/no-include", func(t *testing.T) {
		now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
		svc, routeStore := newNetbirdServerTestService(t, now)
		fake := newFakeNetbird(t)
		fake.seedGroup("gw-portal", "op-gw-portal")
		enableNetbirdPolicies(t, svc, fake.srv.URL, "selected", false, false)
		seedManagedNetbirdServer(t, routeStore, "srvS", "S", "track-S", "", now)
		seedApp(t, routeStore, "app-S", "srvS", 8080, routing.ServerStatusActive, now)
		fake.seedPolicy("pol-S", "op-gw-access-srvS", true, []string{"8080"}, "gw-portal", "track-S")

		svc.reconcileServerPolicy(context.Background(), "srvS")

		if !fake.wasPolicyDeleted("pol-S") {
			t.Fatalf("policy pol-S was not deleted (selected-mode, no include)")
		}
	})
}

// TestReconcileServerPolicyNoDiffNoUpdate: a policy already matching the desired port
// set + source + destination triggers NO UpdatePolicy (and no create/delete).
func TestReconcileServerPolicyNoDiffNoUpdate(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)
	seedManagedNetbirdServer(t, routeStore, "srvN", "S", "track-N", "", now)
	seedApp(t, routeStore, "app-a", "srvN", 8080, routing.ServerStatusActive, now)
	seedApp(t, routeStore, "app-b", "srvN", 9090, routing.ServerStatusActive, now)
	// Pre-seed a policy that already matches the desired set (ports order differs on
	// purpose — policyMatches is order-insensitive).
	fake.seedPolicy("pol-N", "op-gw-access-srvN", true, []string{"9090", "8080"}, "gw-portal", "track-N")

	svc.reconcileServerPolicy(context.Background(), "srvN")

	if fake.policyUpdateCount() != 0 {
		t.Fatalf("policy updates = %d, want 0 (no diff)", fake.policyUpdateCount())
	}
	if fake.policyCreateCount() != 0 || fake.deletedPolicyCount() != 0 {
		t.Fatalf("unexpected create/delete: c=%d d=%d", fake.policyCreateCount(), fake.deletedPolicyCount())
	}
}

// TestReconcileServerPolicyUpdatesOnDiff: a managed server whose existing policy has a
// STALE port set is UPDATEd to the current active set (guards the update branch).
func TestReconcileServerPolicyUpdatesOnDiff(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)
	seedManagedNetbirdServer(t, routeStore, "srvU", "S", "track-U", "", now)
	seedApp(t, routeStore, "app-a", "srvU", 8080, routing.ServerStatusActive, now)
	seedApp(t, routeStore, "app-b", "srvU", 9090, routing.ServerStatusActive, now)
	// Stale policy: only 8080.
	fake.seedPolicy("pol-U", "op-gw-access-srvU", true, []string{"8080"}, "gw-portal", "track-U")

	svc.reconcileServerPolicy(context.Background(), "srvU")

	if fake.policyUpdateCount() != 1 {
		t.Fatalf("policy updates = %d, want 1 (stale port set)", fake.policyUpdateCount())
	}
	body := fake.lastUpdatedPolicy()
	if ports := policyRuleField(t, body, "ports"); !reflect.DeepEqual(ports, []string{"8080", "9090"}) {
		t.Fatalf("updated ports = %v, want [8080 9090]", ports)
	}
	if fake.policyCreateCount() != 0 || fake.deletedPolicyCount() != 0 {
		t.Fatalf("unexpected create/delete on update path: c=%d d=%d", fake.policyCreateCount(), fake.deletedPolicyCount())
	}
}

// TestReconcileServerPolicyDeletesWhenNoActivePorts: a managed server with ZERO active
// apps has its existing policy DELETED (no destination to allow).
func TestReconcileServerPolicyDeletesWhenNoActivePorts(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)
	seedManagedNetbirdServer(t, routeStore, "srvZ", "S", "track-Z", "", now)
	// Only an INACTIVE app -> no active ports.
	seedApp(t, routeStore, "app-z", "srvZ", 8080, routing.ServerStatusDisabled, now)
	fake.seedPolicy("pol-Z", "op-gw-access-srvZ", true, []string{"8080"}, "gw-portal", "track-Z")

	svc.reconcileServerPolicy(context.Background(), "srvZ")

	if !fake.wasPolicyDeleted("pol-Z") {
		t.Fatalf("policy pol-Z was not deleted (no active ports)")
	}
	if fake.policyCreateCount() != 0 {
		t.Fatalf("policy creates = %d, want 0", fake.policyCreateCount())
	}
}

// TestReconcileServerPolicyManageOffNoCalls: with policy management OFF, the reconcile
// makes ZERO NetBird calls (returns at the manage-off gate before listing policies).
func TestReconcileServerPolicyManageOffNoCalls(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	// Module ON but policy management OFF.
	enableNetbird(t, svc, fake.srv.URL, true)
	seedManagedNetbirdServer(t, routeStore, "srvM", "S", "track-M", "", now)
	seedApp(t, routeStore, "app-m", "srvM", 8080, routing.ServerStatusActive, now)

	svc.reconcileServerPolicy(context.Background(), "srvM")

	if fake.count() != 0 {
		t.Fatalf("netbird requests = %d, want 0 (policy management off)", fake.count())
	}
}

// TestReconcileServerPolicyBestEffortOnNetbirdError: a NetBird ListPolicies 500 makes
// the reconcile return quietly (best-effort void) — no panic, no create/update/delete.
func TestReconcileServerPolicyBestEffortOnNetbirdError(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.failListPolicies = true
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)
	seedManagedNetbirdServer(t, routeStore, "srvB", "S", "track-B", "", now)
	seedApp(t, routeStore, "app-b", "srvB", 8080, routing.ServerStatusActive, now)

	svc.reconcileServerPolicy(context.Background(), "srvB") // must not panic

	if fake.policyCreateCount() != 0 || fake.policyUpdateCount() != 0 || fake.deletedPolicyCount() != 0 {
		t.Fatalf("expected no policy writes on list error: c=%d u=%d d=%d", fake.policyCreateCount(), fake.policyUpdateCount(), fake.deletedPolicyCount())
	}
}

// TestReconcileAllServerNetbird: a fleet pass creates the managed server's policy,
// deletes the excluded server's policy, skips the non-NetBird server, and lists
// policies exactly ONCE for the whole pass.
func TestReconcileAllServerNetbird(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)
	// enableNetbirdPolicies already settled its own background fleet-policy side
	// effect (against the still-empty server list) before returning, so this
	// baseline is a deterministic starting point for the list-count delta below.
	listBaseline := fake.policyListCount()

	// Managed server (all-mode default) with an active app -> CreatePolicy.
	seedManagedNetbirdServer(t, routeStore, "srvMgd", "Managed", "track-mgd", "", now)
	seedApp(t, routeStore, "app-mgd", "srvMgd", 8080, routing.ServerStatusActive, now)
	// Excluded server with an existing policy -> DeletePolicy.
	seedManagedNetbirdServer(t, routeStore, "srvExc", "Excluded", "track-exc", "exclude", now)
	seedApp(t, routeStore, "app-exc", "srvExc", 9090, routing.ServerStatusActive, now)
	fake.seedPolicy("pol-exc", "op-gw-access-srvExc", true, []string{"9090"}, "gw-portal", "track-exc")
	// Non-NetBird server -> skipped entirely.
	if err := routeStore.CreateAIServer(context.Background(), routing.AIServer{ID: "srvPlain", Name: "Plain", Domain: "p.local", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer plain: %v", err)
	}
	seedApp(t, routeStore, "app-plain", "srvPlain", 7000, routing.ServerStatusActive, now)

	svc.ReconcileAllServerNetbird(context.Background())

	if got := fake.policyListCount() - listBaseline; got != 1 {
		t.Fatalf("policy lists (this pass) = %d, want 1 (single list for the whole pass)", got)
	}
	// Exactly one per-server access policy for the managed server (the fleet pass
	// also creates the account-wide op-gw-agent-ingest policy, so assert on the
	// access-policy name rather than the total create count).
	if got := fake.policyCreateCountByName("op-gw-access-srvMgd"); got != 1 {
		t.Fatalf("op-gw-access-srvMgd creates = %d, want 1 (managed server)", got)
	}
	if !fake.wasPolicyDeleted("pol-exc") {
		t.Fatalf("excluded server's policy pol-exc was not deleted")
	}
	// The access policy must belong to the managed server, never the plain one.
	if fake.createdPolicyByName("op-gw-access-srvMgd") == nil {
		t.Fatalf("no op-gw-access-srvMgd CreatePolicy body recorded")
	}
	if fake.policyCreateCountByName("op-gw-access-srvPlain") != 0 {
		t.Fatalf("a policy was created for the non-NetBird server, want none")
	}
}

// TestSetServerNetbirdPersistsPolicyOverride: SetServerNetbird persists the override
// (round-trip via AIServerByID + on the DTO) AND runs the policy reconcile — proven by
// an existing policy being deleted for the now-excluded server.
func TestSetServerNetbirdPersistsPolicyOverride(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)

	// A NetBird server with a tracking group + an active app + an existing policy.
	seedManagedNetbirdServer(t, routeStore, "srvO", "S", "track-O", "", now)
	seedApp(t, routeStore, "app-o", "srvO", 8080, routing.ServerStatusActive, now)
	fake.seedPolicy("pol-O", "op-gw-access-srvO", true, []string{"8080"}, "gw-portal", "track-O")

	dto, err := svc.SetServerNetbird(context.Background(), systemToken(), "srvO", true, "", nil, false, "exclude", false, false)
	if err != nil {
		t.Fatalf("SetServerNetbird(override=exclude): %v", err)
	}
	if dto.NetbirdPolicyOverride != "exclude" {
		t.Fatalf("dto.NetbirdPolicyOverride = %q, want exclude", dto.NetbirdPolicyOverride)
	}
	stored, _ := routeStore.AIServerByID(context.Background(), "srvO")
	if stored.NetbirdPolicyOverride != "exclude" {
		t.Fatalf("stored NetbirdPolicyOverride = %q, want exclude", stored.NetbirdPolicyOverride)
	}
	// The reconcile ran: the now-excluded server's existing policy was deleted.
	if !fake.wasPolicyDeleted("pol-O") {
		t.Fatalf("policy pol-O was not deleted — reconcile did not run on the override change")
	}
}

// TestSetServerNetbirdPolicyOverrideNormalized: a junk override normalizes to "".
func TestSetServerNetbirdPolicyOverrideNormalized(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	seedManagedNetbirdServer(t, routeStore, "srvJ", "S", "track-J", "include", now)
	// Give it a domain so a later disable wouldn't trip the guard (not needed here).
	dto, err := svc.SetServerNetbird(context.Background(), systemToken(), "srvJ", true, "", nil, false, "garbage", false, false)
	if err != nil {
		t.Fatalf("SetServerNetbird(garbage override): %v", err)
	}
	if dto.NetbirdPolicyOverride != "" {
		t.Fatalf("dto.NetbirdPolicyOverride = %q, want \"\" (junk normalized)", dto.NetbirdPolicyOverride)
	}
}

// TestNetbirdPolicySettingsFeedsStatus: the policy settings the status endpoint
// surfaces (manage/scope/effective/deny/enforce) come straight from the persisted
// settings — a service-level check of the values that back the status DTO.
func TestNetbirdPolicySettingsFeedsStatus(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbirdPolicies(t, svc, fake.srv.URL, "auto", true, true) // auto + deny -> effective "all"

	view := svc.SystemSettingsView(context.Background())
	if !view.NetbirdManagePolicies {
		t.Fatalf("NetbirdManagePolicies = false, want true")
	}
	if view.NetbirdPolicyScope != "auto" {
		t.Fatalf("NetbirdPolicyScope = %q, want auto", view.NetbirdPolicyScope)
	}
	if view.NetbirdEffectivePolicyScope != "all" {
		t.Fatalf("NetbirdEffectivePolicyScope = %q, want all (auto+deny)", view.NetbirdEffectivePolicyScope)
	}
	if !view.NetbirdDenyByDefault || !view.NetbirdDenyByDefaultEnforce {
		t.Fatalf("deny/enforce = %v/%v, want true/true", view.NetbirdDenyByDefault, view.NetbirdDenyByDefaultEnforce)
	}
}

// ---- item 1: transient store error keeps the policy ----------------------------

// TestReconcileServerPolicyKeepsPolicyOnStoreError: when ApplicationsByServer errors
// (a transient DB failure), the reconcile MUST NOT delete an existing healthy policy
// (reconcile-loop invariant). Only a genuine empty-active-set deletes.
func TestReconcileServerPolicyKeepsPolicyOnStoreError(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	dir := NewMemoryDirectory(auth.NewTokenStore())
	if err := dir.CreateUser(context.Background(), store.User{ID: "usr_owner", Email: "o@example.test", DisplayName: "o", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	mem := routing.NewMemoryStore()
	wrapped := &errAppsStore{MemoryStore: mem}
	svc := NewService(ServiceDeps{Users: dir, Routes: wrapped, SystemSettings: NewMemorySystemSettings(), Cipher: newTestCipher(t), Clock: func() time.Time { return now }})
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)

	// A managed server (all-mode) with an EXISTING policy.
	if err := mem.CreateAIServer(context.Background(), routing.AIServer{ID: "srvT", Name: "S", Status: routing.ServerStatusActive, NetbirdEnabled: true, NetbirdGroupID: "track-T", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	fake.seedPolicy("pol-T", "op-gw-access-srvT", true, []string{"8080"}, "gw-portal", "track-T")

	svc.reconcileServerPolicy(context.Background(), "srvT")

	if fake.wasPolicyDeleted("pol-T") {
		t.Fatalf("policy pol-T was DELETED on a transient store error — must be kept")
	}
	if fake.policyCreateCount() != 0 || fake.policyUpdateCount() != 0 {
		t.Fatalf("unexpected create/update on store-error skip: c=%d u=%d", fake.policyCreateCount(), fake.policyUpdateCount())
	}
}

// ---- item 2: policyMatches self-heals a top-level-disabled policy --------------

// TestReconcileServerPolicySelfHealsDisabledPolicy: an existing policy that matches
// the desired rule but is top-level DISABLED is UPDATEd back to enabled:true.
func TestReconcileServerPolicySelfHealsDisabledPolicy(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)
	seedManagedNetbirdServer(t, routeStore, "srvH", "S", "track-H", "", now)
	seedApp(t, routeStore, "app-h", "srvH", 8080, routing.ServerStatusActive, now)
	// Rule matches, but the policy is DISABLED at the top level (drift).
	fake.seedPolicy("pol-H", "op-gw-access-srvH", false, []string{"8080"}, "gw-portal", "track-H")

	svc.reconcileServerPolicy(context.Background(), "srvH")

	if fake.policyUpdateCount() != 1 {
		t.Fatalf("policy updates = %d, want 1 (self-heal disabled policy)", fake.policyUpdateCount())
	}
	body := fake.lastUpdatedPolicy()
	if enabled, _ := body["enabled"].(bool); !enabled {
		t.Fatalf("updated policy enabled = false, want true (re-enabled)")
	}
	if fake.policyCreateCount() != 0 || fake.deletedPolicyCount() != 0 {
		t.Fatalf("unexpected create/delete on self-heal: c=%d d=%d", fake.policyCreateCount(), fake.deletedPolicyCount())
	}
}

// TestPolicyMatchesRejectsPortRanges: a gateway-managed policy never uses port
// ranges (only discrete active-app ports). An out-of-band-added port range on the
// rule must be treated as drift (policyMatches -> false) so the reconcile issues an
// UpdatePolicy that re-sends the desired discrete-ports rule, self-healing the
// over-grant. Guards the least-privilege blindspot the final verification flagged.
func TestPolicyMatchesRejectsPortRanges(t *testing.T) {
	desc := managedPolicyDescription(netbirdAccessPolicyPurpose)
	base := netbird.Policy{
		Enabled:     true,
		Description: desc,
		Rules: []netbird.PolicyRule{{
			Enabled: true, Action: "accept", Bidirectional: false, Protocol: "tcp",
			Description:  desc,
			Ports:        []string{"8080"},
			Sources:      []netbird.GroupRef{{ID: "gw"}},
			Destinations: []netbird.GroupRef{{ID: "track"}},
		}},
	}
	// Baseline (no ranges) matches.
	if !policyMatches(base, "gw", "track", []string{"8080"}) {
		t.Fatalf("baseline policy (discrete ports, no ranges) should match")
	}
	// An out-of-band port range widens the grant -> must NOT match.
	withRange := base
	withRange.Rules = []netbird.PolicyRule{base.Rules[0]}
	withRange.Rules[0].PortRanges = []netbird.PortRange{{Start: 1, End: 65535}}
	if policyMatches(withRange, "gw", "track", []string{"8080"}) {
		t.Fatalf("policy with an out-of-band port range must NOT match (least-privilege self-heal)")
	}
}

// TestManagedPoliciesCarryDescription (op-gw-access-<id>): the desired PolicyRequest
// built by desiredServerAccessPolicyRequest carries the English "Managed by the OP AI
// Gateway" Description on both the policy and its sole rule, so an operator can tell
// at a glance in the NetBird admin UI that the policy is gateway-owned.
func TestManagedPoliciesCarryDescriptionAccess(t *testing.T) {
	req := desiredServerAccessPolicyRequest("op-gw-access-srvX", "gw", "track", []string{"8080"})
	if !strings.Contains(req.Description, "Managed by the OP AI Gateway") {
		t.Fatalf("policy description missing: %q", req.Description)
	}
	if len(req.Rules) == 0 || !strings.Contains(req.Rules[0].Description, "Managed by the OP AI Gateway") {
		t.Fatalf("rule description missing")
	}
}

// TestManagedPolicyDescriptionDriftIsCorrected (op-gw-access-<id>): a stored policy
// identical to the desired one except for an empty Description (a manual edit that
// wiped the marker) must NOT match, so the reconcile issues an UpdatePolicy that
// restores it.
func TestManagedPolicyDescriptionDriftIsCorrectedAccess(t *testing.T) {
	desired := desiredServerAccessPolicyRequest("op-gw-access-srvX", "gw", "track", []string{"8080"})
	stored := netbird.Policy{
		Name:        desired.Name,
		Description: "",
		Enabled:     true,
		Rules: []netbird.PolicyRule{{
			Enabled: true, Action: "accept", Bidirectional: false, Protocol: "tcp",
			Description:  "",
			Ports:        []string{"8080"},
			Sources:      []netbird.GroupRef{{ID: "gw"}},
			Destinations: []netbird.GroupRef{{ID: "track"}},
		}},
	}
	if policyMatches(stored, "gw", "track", []string{"8080"}) {
		t.Fatal("a policy with an empty description must be treated as drift")
	}
}

// ---- item 3: empty account (non-error list) still creates ----------------------

// TestReconcileAllServerNetbirdEmptyAccountCreates: a fleet pass against an EMPTY
// policy account (ListPolicies returns [], no error) with a managed server + active
// app must CREATE the policy (an empty list is not a list error).
func TestReconcileAllServerNetbirdEmptyAccountCreates(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t) // no policies seeded => empty account
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)
	// enableNetbirdPolicies already settled its own background fleet side effect
	// (against the still-empty server list), so this is a deterministic baseline.
	listBaseline := fake.policyListCount()
	seedManagedNetbirdServer(t, routeStore, "srvEA", "S", "track-EA", "", now)
	seedApp(t, routeStore, "app-ea", "srvEA", 8080, routing.ServerStatusActive, now)

	svc.ReconcileAllServerNetbird(context.Background())

	if got := fake.policyListCount() - listBaseline; got != 1 {
		t.Fatalf("policy lists (this pass) = %d, want 1", got)
	}
	if got := fake.policyCreateCountByName("op-gw-access-srvEA"); got != 1 {
		t.Fatalf("op-gw-access-srvEA creates = %d, want 1 (empty account must still create)", got)
	}
}

// ---- item 5: deny-by-default -----------------------------------------------------

func TestApplyDenyByDefault(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	t.Run("enable-disables-default", func(t *testing.T) {
		svc, _ := newNetbirdServerTestService(t, now)
		fake := newFakeNetbird(t)
		enableNetbirdPolicies(t, svc, fake.srv.URL, "all", true, true)
		fake.seedDefaultPolicy("def", true)
		ncfg := netbird.Config{URL: fake.srv.URL, Token: "nbtok"}

		svc.applyDenyByDefault(context.Background(), ncfg, true) // deny ON => disable Default

		if fake.policyUpdateCount() != 1 {
			t.Fatalf("updates = %d, want 1 (disable Default)", fake.policyUpdateCount())
		}
		if enabled, _ := fake.lastUpdatedPolicy()["enabled"].(bool); enabled {
			t.Fatalf("Default enabled = true after deny ON, want false")
		}
	})

	t.Run("disable-reenables-default", func(t *testing.T) {
		svc, _ := newNetbirdServerTestService(t, now)
		fake := newFakeNetbird(t)
		enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)
		fake.seedDefaultPolicy("def", false)
		ncfg := netbird.Config{URL: fake.srv.URL, Token: "nbtok"}

		svc.applyDenyByDefault(context.Background(), ncfg, false) // deny OFF => re-enable Default

		if fake.policyUpdateCount() != 1 {
			t.Fatalf("updates = %d, want 1 (re-enable Default)", fake.policyUpdateCount())
		}
		if enabled, _ := fake.lastUpdatedPolicy()["enabled"].(bool); !enabled {
			t.Fatalf("Default enabled = false after deny OFF, want true")
		}
	})

	t.Run("missing-default-no-call", func(t *testing.T) {
		svc, _ := newNetbirdServerTestService(t, now)
		fake := newFakeNetbird(t) // no Default policy seeded
		enableNetbirdPolicies(t, svc, fake.srv.URL, "all", true, true)
		ncfg := netbird.Config{URL: fake.srv.URL, Token: "nbtok"}

		svc.applyDenyByDefault(context.Background(), ncfg, true)

		if fake.policyUpdateCount() != 0 {
			t.Fatalf("updates = %d, want 0 (no Default policy present)", fake.policyUpdateCount())
		}
	})

	t.Run("already-in-state-no-call", func(t *testing.T) {
		svc, _ := newNetbirdServerTestService(t, now)
		fake := newFakeNetbird(t)
		enableNetbirdPolicies(t, svc, fake.srv.URL, "all", true, true)
		fake.seedDefaultPolicy("def", false) // already disabled; deny ON wants disabled
		ncfg := netbird.Config{URL: fake.srv.URL, Token: "nbtok"}

		svc.applyDenyByDefault(context.Background(), ncfg, true)

		if fake.policyUpdateCount() != 0 {
			t.Fatalf("updates = %d, want 0 (already in desired state)", fake.policyUpdateCount())
		}
	})
}

// TestReconcileAllServerNetbirdEnforcesDenyByDefault: a fleet pass with deny+enforce
// ON re-disables a drifted (enabled) Default policy.
func TestReconcileAllServerNetbirdEnforcesDenyByDefault(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", true, true)
	// enableNetbirdPolicies already settled its own background deny-by-default
	// apply against the (still policy-less) account before returning, so seeding
	// the drifted "Default" policy here happens strictly after that pass, not
	// racing it.
	fake.seedDefaultPolicy("def", true) // drifted: enabled while deny-by-default is on

	svc.ReconcileAllServerNetbird(context.Background())

	if fake.policyUpdateCount() != 1 {
		t.Fatalf("updates = %d, want 1 (enforce re-disables Default)", fake.policyUpdateCount())
	}
	if enabled, _ := fake.lastUpdatedPolicy()["enabled"].(bool); enabled {
		t.Fatalf("Default still enabled after enforce, want disabled")
	}
}

// ---- item 5: group mirror --------------------------------------------------------

// TestMirrorServerGroupsWritesCanonical: mirroring a peer whose Groups include the
// tracking group + a policy group writes canonical JSON equal to
// netbird.CanonicalGroupsJSON of the NON-tracking groups.
func TestMirrorServerGroupsWritesCanonical(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)
	fake.setPeerWithGroups("peer-m", "gpu", "gpu.netbird.io", true, []map[string]any{
		{"id": "track-M", "name": "op-gw-srvM"}, // tracking group -> excluded
		{"id": "grp-prod", "name": "prod"},      // policy group -> mirrored
	})
	if err := routeStore.CreateAIServer(context.Background(), routing.AIServer{ID: "srvM", Name: "S", Status: routing.ServerStatusActive, NetbirdEnabled: true, NetbirdGroupID: "track-M", NetbirdPeerID: "peer-m", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	server, _ := routeStore.AIServerByID(context.Background(), "srvM")
	ncfg := netbird.Config{URL: fake.srv.URL, Token: "nbtok"}

	svc.mirrorServerGroups(context.Background(), ncfg, server)

	want, err := netbird.CanonicalGroupsJSON([]netbird.GroupRef{{ID: "grp-prod", Name: "prod"}})
	if err != nil {
		t.Fatalf("CanonicalGroupsJSON: %v", err)
	}
	stored, _ := routeStore.AIServerByID(context.Background(), "srvM")
	if stored.NetbirdGroupIDs != want {
		t.Fatalf("mirror wrote %q, want canonical %q (tracking excluded)", stored.NetbirdGroupIDs, want)
	}
}

// TestMirrorServerGroupsNoWriteWhenUnchanged: when the computed canonical JSON equals
// the server's already-stored mirror, no write occurs. Proven by seeding the STORE to
// a sentinel distinct from the (matching) struct field: a skip leaves the sentinel.
func TestMirrorServerGroupsNoWriteWhenUnchanged(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)
	fake.setPeerWithGroups("peer-u", "gpu", "gpu.netbird.io", true, []map[string]any{
		{"id": "track-U", "name": "op-gw-srvU"},
		{"id": "grp-prod", "name": "prod"},
	})
	canonical, _ := netbird.CanonicalGroupsJSON([]netbird.GroupRef{{ID: "grp-prod", Name: "prod"}})
	// Store a sentinel; pass a struct whose mirror field already equals the canonical
	// form the mirror will compute -> mirror must skip the write (store keeps sentinel).
	if err := routeStore.CreateAIServer(context.Background(), routing.AIServer{ID: "srvU", Name: "S", Status: routing.ServerStatusActive, NetbirdEnabled: true, NetbirdGroupID: "track-U", NetbirdPeerID: "peer-u", NetbirdGroupIDs: "SENTINEL", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	server, _ := routeStore.AIServerByID(context.Background(), "srvU")
	server.NetbirdGroupIDs = canonical // the loaded-server value already matches

	svc.mirrorServerGroups(context.Background(), netbird.Config{URL: fake.srv.URL, Token: "nbtok"}, server)

	stored, _ := routeStore.AIServerByID(context.Background(), "srvU")
	if stored.NetbirdGroupIDs != "SENTINEL" {
		t.Fatalf("mirror wrote %q on a no-change (want SENTINEL untouched)", stored.NetbirdGroupIDs)
	}
}

// ---- T5: settings-PUT policy-relevant fields trigger a fleet side-effect pass --

// TestPolicySettingsSideEffectsReconcilesFleet tests the extracted synchronous
// helper directly (UpdateSystemSettings only fires it in a background goroutine,
// which would make asserting on the fake's counters racy). With policy management
// on + a managed server, calling it reconciles the fleet (one CreatePolicy for the
// managed server); when the deny-by-default toggle is part of the request, it also
// flips NetBird's "Default" catch-all policy via applyDenyByDefault.
func TestPolicySettingsSideEffectsReconcilesFleet(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	t.Run("with deny request", func(t *testing.T) {
		svc, routeStore := newNetbirdServerTestService(t, now)
		fake := newFakeNetbird(t)
		fake.seedGroup("gw-portal", "op-gw-portal")
		enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)
		seedManagedNetbirdServer(t, routeStore, "srv-pse", "S", "track-pse", "", now)
		seedApp(t, routeStore, "app-pse", "srv-pse", 8080, routing.ServerStatusActive, now)
		fake.seedDefaultPolicy("def", true) // drifted: enabled while deny is being turned on

		deny := true
		svc.applyPolicySettingsSideEffects(context.Background(), &deny)

		if got := fake.policyCreateCountByName("op-gw-access-srv-pse"); got != 1 {
			t.Fatalf("op-gw-access-srv-pse creates = %d, want 1 (fleet reconcile for the managed server)", got)
		}
		if fake.policyUpdateCount() != 1 {
			t.Fatalf("policy updates = %d, want 1 (Default flipped by the deny request)", fake.policyUpdateCount())
		}
		if enabled, _ := fake.lastUpdatedPolicy()["enabled"].(bool); enabled {
			t.Fatalf("Default still enabled after deny-on side effect, want disabled")
		}
	})

	t.Run("without deny request", func(t *testing.T) {
		svc, routeStore := newNetbirdServerTestService(t, now)
		fake := newFakeNetbird(t)
		fake.seedGroup("gw-portal", "op-gw-portal")
		enableNetbirdPolicies(t, svc, fake.srv.URL, "all", true, true)
		seedManagedNetbirdServer(t, routeStore, "srv-pse2", "S", "track-pse2", "", now)
		seedApp(t, routeStore, "app-pse2", "srv-pse2", 8080, routing.ServerStatusActive, now)
		fake.seedDefaultPolicy("def2", true) // would need flipping, but no deny request this time

		svc.applyPolicySettingsSideEffects(context.Background(), nil)

		if got := fake.policyCreateCountByName("op-gw-access-srv-pse2"); got != 1 {
			t.Fatalf("op-gw-access-srv-pse2 creates = %d, want 1 (fleet reconcile still runs)", got)
		}
		if fake.policyUpdateCount() != 0 {
			t.Fatalf("policy updates = %d, want 0 (deny toggle not in this request -> Default untouched)", fake.policyUpdateCount())
		}
	})
}

// TestMirrorServerGroupsNoClearWhenPeerUnresolvable: when the peer cannot be resolved
// (no peer id + a tracking group the account doesn't have), the mirror never clears
// the stored value.
func TestMirrorServerGroupsNoClearWhenPeerUnresolvable(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)
	if err := routeStore.CreateAIServer(context.Background(), routing.AIServer{ID: "srvG", Name: "S", Status: routing.ServerStatusActive, NetbirdEnabled: true, NetbirdGroupID: "ghost-group", NetbirdGroupIDs: "KEEP", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	server, _ := routeStore.AIServerByID(context.Background(), "srvG")

	svc.mirrorServerGroups(context.Background(), netbird.Config{URL: fake.srv.URL, Token: "nbtok"}, server)

	stored, _ := routeStore.AIServerByID(context.Background(), "srvG")
	if stored.NetbirdGroupIDs != "KEEP" {
		t.Fatalf("mirror cleared/changed the value to %q on an unresolvable peer (want KEEP)", stored.NetbirdGroupIDs)
	}
}

// ---- ReconcileAllServerNetbird's policy section must be netbirdPolicyMu-serialized ----

// TestReconcileAllServerNetbirdConcurrentWithAppCRUDNoDuplicate: two goroutines racing
// on a FRESH managed server with one active app — one running the fleet pass
// (ReconcileAllServerNetbird, the T6 sync-loop entry point) and one running the
// app-CRUD/settings-PUT path (reconcileServerPolicy) — must never both observe "no
// existing op-gw-access-<id> policy" and both CreatePolicy it. This is the regression
// test for the gap: ReconcileAllServerNetbird's policy section (ListPolicies + the
// per-server reconcileServerPolicyWith loop) must be serialized by the SAME
// netbirdPolicyMu reconcileServerPolicy holds, listing AND creating as one atomic
// section. Run with -race -count=20: -race catches data races, the exactly-one-create
// assertion catches the logical TOCTOU duplicate-create race (which -race alone would
// not, since the fake's internal state is itself mutex-guarded).
func TestReconcileAllServerNetbirdConcurrentWithAppCRUDNoDuplicate(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)
	seedManagedNetbirdServer(t, routeStore, "srv-conc", "S", "track-conc", "", now)
	seedApp(t, routeStore, "app-conc", "srv-conc", 8080, routing.ServerStatusActive, now)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		svc.ReconcileAllServerNetbird(context.Background())
	}()
	go func() {
		defer wg.Done()
		svc.reconcileServerPolicy(context.Background(), "srv-conc")
	}()
	wg.Wait()

	// The fleet pass also creates the account-wide op-gw-agent-ingest policy, so
	// scope the duplicate-create assertion to the per-server access policy name.
	if got := fake.policyCreateCountByName("op-gw-access-srv-conc"); got != 1 {
		t.Fatalf("op-gw-access-srv-conc creates = %d, want exactly 1 (duplicate-create race)", got)
	}
}

// TestActivePortStringsIgnoresTheInvariantViolatingRow documents what the ONE
// residue this design accepts actually does, rather than leaving it to be
// discovered. A row with ProxyExcluded=true, https AND a non-zero
// ProxyListenPort is unreachable through the API by construction — the portal's
// applyProxyExclusion clears the port on every write that sets the flag — so it
// can only come from a direct store write.
//
// If one exists, activePortStrings still opens its proxy port, because it
// derives from (scheme, ProxyListenPort) and knows nothing about the flag. That
// is the deliberate trade: the flag is worth exactly as much as the invariant
// that backs it, and the alternative — teaching four separate derivations about
// participation — is the cost this design avoids. The repair path is
// revertScopeExit, which is left unguarded precisely so it can rescue this row.
func TestActivePortStringsIgnoresTheInvariantViolatingRow(t *testing.T) {
	apps := []routing.Application{
		// Reachable only by a direct store write; the flag is ignored here.
		{Port: 8080, Scheme: "https", ProxyListenPort: 8601, ProxyExcluded: true, Status: routing.ServerStatusActive},
		// The shape the API can actually produce: excluded => port 0.
		{Port: 9090, Scheme: "https", ProxyListenPort: 0, ProxyExcluded: true, Status: routing.ServerStatusActive},
		{Port: 7070, Scheme: "http", ProxyListenPort: 0, ProxyExcluded: true, Status: routing.ServerStatusActive},
	}
	got := activePortStrings(apps)
	want := []string{"7070", "8080", "8601", "9090"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("activePortStrings = %v, want %v", got, want)
	}
}

// TestExcludingAnApplicationClosesItsProxyPortSynchronously drives the
// exclusion through UpdateApplication so the SAME synchronous
// reconcileServerPolicy the real update path runs actually fires, and asserts
// the door closes BEFORE the listener goes rather than after: the released
// proxy port drops out of the managed op-gw-access policy on the update call
// itself, while the agent only stops listening on its next certificate poll (up
// to 15 minutes over POST, up to 6 hours over WebSocket). The orphaned listener
// is harmless precisely because this ordering holds.
func TestExcludingAnApplicationClosesItsProxyPortSynchronously(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")

	server := createTestServer(t, svc, "Proxy Server", "proxy.example.test")
	stored, err := routeStore.AIServerByID(ctx, server.ID)
	if err != nil {
		t.Fatalf("read server: %v", err)
	}
	stored.NetbirdEnabled = true
	stored.NetbirdGroupID = "track-proxy"
	if err := routeStore.UpdateAIServer(ctx, stored); err != nil {
		t.Fatalf("mark server managed: %v", err)
	}

	app, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8080, Scheme: "http",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	// The proxied steady state, as the gateway itself would have produced it.
	row, err := routeStore.ApplicationByID(ctx, app.ID)
	if err != nil {
		t.Fatalf("read app: %v", err)
	}
	row.Scheme = "https"
	row.ProxyListenPort = 8601
	if err := routeStore.UpdateApplication(ctx, row); err != nil {
		t.Fatalf("seed proxied state: %v", err)
	}

	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)
	svc.reconcileServerPolicy(ctx, server.ID)
	policyName := "op-gw-access-" + server.ID
	before := fake.createdPolicyByName(policyName)
	if before == nil {
		t.Fatalf("no %s policy created for the proxied application", policyName)
	}
	if ports := policyRuleField(t, before, "ports"); !reflect.DeepEqual(ports, []string{"8080", "8601"}) {
		t.Fatalf("proxied policy ports = %v, want [8080 8601]", ports)
	}

	scheme := "http"
	if _, err := svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{
		Scheme: &scheme, ProxyExcluded: boolPtr(true),
	}); err != nil {
		t.Fatalf("exclude: %v", err)
	}

	after := fake.updatedPolicyByName(policyName)
	if after == nil {
		t.Fatalf("the exclusion did not update %s at all", policyName)
	}
	ports := policyRuleField(t, after, "ports")
	if !reflect.DeepEqual(ports, []string{"8080"}) {
		t.Fatalf("policy ports after exclusion = %v, want [8080] -- the released proxy port must close at once", ports)
	}
}
