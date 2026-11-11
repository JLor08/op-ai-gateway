// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// The Driver honours a document's replicable flag only while the gateway
// declares runtime_instances (#340): several instances of one spec report
// several status rows under one spec_id, which only such a gateway
// understands. These tests pin that gate -- fail-closed, in file mode too --
// and that a later declaration takes effect without the document changing.

// withheldInstancesWarn is noteInstancesWithheld's exact Warn message; matched
// exactly so a test counting it cannot be satisfied by some other line.
const withheldInstancesWarn = "runtime: the gateway does not declare runtime_instances; every replicable spec runs as a single instance until it does"

// newSwitchableFeaturesServer returns a *FeaturesClient backed by a server
// whose declared feature set the test replaces at will (no ETag, so every
// Fetch sees the current set).
func newSwitchableFeaturesServer(t *testing.T, features []string) (client *FeaturesClient, set func([]string)) {
	t.Helper()
	var mu sync.Mutex
	current := features
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		declared := current
		mu.Unlock()
		body, _ := json.Marshal(struct {
			Features []string `json:"features"`
		}{Features: declared})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return NewFeaturesClient(srv.URL, "tok", nil), func(f []string) {
		mu.Lock()
		current = f
		mu.Unlock()
	}
}

// TestDriverClearsReplicableWithoutTheGatewayDeclaration pins what the Driver
// hands the manager: without runtime_instances on the gateway's list every spec
// arrives with Replicable cleared (and the source's own cached document is left
// untouched -- it is a copy that is cleared); with it, the document arrives as
// loaded. A change of the declaration alone applies the unchanged document
// again.
func TestDriverClearsReplicableWithoutTheGatewayDeclaration(t *testing.T) {
	replicable := Spec{ID: "S", UpstreamModel: "m", Replicable: true, MinInstances: 2}
	plain := Spec{ID: "P", UpstreamModel: "p"}
	src := &fakeSource{cfg: Config{ETag: "e1", Specs: []Spec{replicable, plain}}}
	mgr := &fakeManager{}
	features, setFeatures := newSwitchableFeaturesServer(t, []string{runtimeManagerFeature})
	d := newDriver(mgr, src, features, nil, "")
	ctx := context.Background()

	d.Sync(ctx, nil)
	if n := mgr.applyCount(); n != 1 {
		t.Fatalf("Apply calls = %d after the first Sync, want 1", n)
	}
	got := mgr.lastApplied()
	if got.Specs[0].Replicable || got.Specs[0].MinInstances != 2 || got.Specs[1].Replicable {
		t.Fatalf("applied specs = %+v, want Replicable cleared on every spec (and nothing else changed)", got.Specs)
	}
	if got.ETag != "e1" {
		t.Errorf("applied ETag = %q, want the document's own e1", got.ETag)
	}
	if !src.cfg.Specs[0].Replicable {
		t.Fatal("the source's cached document lost its replicable flag: the Driver must clear a copy")
	}

	d.Sync(ctx, nil) // nothing changed: nothing applied
	if n := mgr.applyCount(); n != 1 {
		t.Fatalf("Apply calls = %d after an unchanged Sync, want still 1", n)
	}

	setFeatures([]string{runtimeManagerFeature, gatewayFeatureRuntimeInstances})
	d.Sync(ctx, nil)
	if n := mgr.applyCount(); n != 2 {
		t.Fatalf("Apply calls = %d once the gateway declares runtime_instances, want 2: the declaration alone re-applies the document", n)
	}
	if got := mgr.lastApplied(); !got.Specs[0].Replicable || got.Specs[1].Replicable {
		t.Fatalf("applied specs = %+v, want the document as loaded (S replicable, P not)", got.Specs)
	}

	d.Sync(ctx, nil)
	if n := mgr.applyCount(); n != 2 {
		t.Fatalf("Apply calls = %d after an unchanged Sync, want still 2", n)
	}
}

// TestDriverWithoutRuntimeInstancesRunsOneInstancePerSpec is the finding's
// scenario end to end, in file mode: the operator's document opts a spec into
// three protected instances, but the gateway does not declare
// runtime_instances (an older gateway), so the spec runs as the single process
// it would be without the opt-in and reports exactly one row -- never two rows
// under one spec_id, which that gateway would read as one spec flapping between
// them. The gateway then declares it: the next Sync applies the SAME document,
// under the same etag, with the opt-in honoured, and the protected instances
// come up. Withdrawn again, the further instances drain away and instance 0
// stays. The Warn is logged once per transition into the withheld state, not on
// every Sync.
func TestDriverWithoutRuntimeInstancesRunsOneInstancePerSpec(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	logs := withCountedLogs(t)
	m := newTestManager(t, allowlistPolicy())

	spec := replicableSpec("S", "m")
	spec.MinInstances = 3
	raw, err := json.Marshal(Config{Specs: []Spec{spec}, ETag: "file-v1"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "runtime.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	features, setFeatures := newSwitchableFeaturesServer(t, []string{runtimeManagerFeature})
	d := NewDriver(m, NewFileSource(path), features, nil, "")
	t.Cleanup(d.Close)
	ctx := context.Background()

	d.Sync(ctx, nil)
	d.Sync(ctx, nil)
	_, release := ensureModel(t, m, "m")
	release()
	if rows := m.Status(); len(rows) != 1 || rows[0].SpecID != "S" || rows[0].Instance != 0 || rows[0].State != StateRunning {
		t.Fatalf("Status() = %+v, want exactly one row, S instance 0 running: without runtime_instances on the gateway the spec is one process", rows)
	}
	if _, err := ensureErr(t, m, "m#1"); !errors.Is(err, ErrModelNotManaged) {
		t.Fatalf("EnsureRunning(m#1) = %v, want ErrModelNotManaged: no instance address exists while the opt-in is withheld", err)
	}
	if n := logs.count(withheldInstancesWarn); n != 1 {
		t.Fatalf("withheld-instances warnings = %d after two Syncs, want 1 (once per transition)", n)
	}
	pid0 := mustInstance(t, m, "S", 0).PID

	setFeatures([]string{runtimeManagerFeature, gatewayFeatureRuntimeInstances})
	d.Sync(ctx, nil)
	waitUntil(t, 5*time.Second, "instances 0, 1 and 2 running once the gateway declares runtime_instances", func() bool {
		for k := 0; k < 3; k++ {
			if st := instanceStatus(m, "S", k); st == nil || st.State != StateRunning {
				return false
			}
		}
		return true
	})
	if st := mustInstance(t, m, "S", 0); st.PID != pid0 {
		t.Errorf("instance 0 = %+v, want it untouched (pid %d): honouring the opt-in changes no launch shape", st, pid0)
	}
	if got := m.AppliedETag(); got != "file-v1" {
		t.Errorf("AppliedETag() = %q, want the document's own file-v1", got)
	}

	setFeatures([]string{runtimeManagerFeature})
	d.Sync(ctx, nil)
	waitUntil(t, 5*time.Second, "the further instances drained and removed once the declaration is gone", func() bool {
		return instanceStatus(m, "S", 1) == nil && instanceStatus(m, "S", 2) == nil
	})
	if st := mustInstance(t, m, "S", 0); st.State != StateRunning || st.PID != pid0 {
		t.Errorf("instance 0 = %+v, want still running as pid %d", st, pid0)
	}
	if n := logs.count(withheldInstancesWarn); n != 2 {
		t.Errorf("withheld-instances warnings = %d, want 2 (the second transition into the withheld state)", n)
	}
}

// TestApplySameETagWithAChangedOptInIsApplied pins the manager half of that
// re-application: a document arriving again under the ETag already applied is a
// no-op -- unless it opts a different set of specs into instances, which is
// exactly the shape a change in the gateway's runtime_instances declaration
// gives it.
func TestApplySameETagWithAChangedOptInIsApplied(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := replicableSpec("S", "m")
	spec.MinInstances = 2
	withheld := spec
	withheld.Replicable = false
	m.Apply(Config{Specs: []Spec{withheld}, ETag: "e1"})
	if rows := m.Status(); len(rows) != 1 {
		t.Fatalf("Status() = %+v, want one row while the opt-in is withheld", rows)
	}

	m.Apply(Config{Specs: []Spec{spec}, ETag: "e1"})
	waitUntil(t, 5*time.Second, "both protected instances running after the same document arrived with the opt-in", func() bool {
		s0, s1 := instanceStatus(m, "S", 0), instanceStatus(m, "S", 1)
		return s0 != nil && s0.State == StateRunning && s1 != nil && s1.State == StateRunning
	})

	// The same document once more, opt-in unchanged: a no-op.
	pid1 := mustInstance(t, m, "S", 1).PID
	m.Apply(Config{Specs: []Spec{spec}, ETag: "e1"})
	if st := mustInstance(t, m, "S", 1); st.State != StateRunning || st.PID != pid1 {
		t.Fatalf("instance 1 = %+v after an identical re-apply, want untouched (pid %d)", st, pid1)
	}
}

func TestParseConfigDropsRPCKindWithoutGatewayFeature(t *testing.T) {
	member := Spec{ID: "rpcm_1", Kind: SpecKindRPCServer, RPC: &RPCMember{Port: 47001}}
	head := Spec{ID: "rspec_h", UpstreamModel: "big", Cluster: &ClusterHead{Endpoints: []ClusterEndpoint{{Address: "a:1", DeviceCount: 1}}}}
	plain := Spec{ID: "P", UpstreamModel: "p"}
	src := &fakeSource{cfg: Config{ETag: "e1", Specs: []Spec{member, head, plain}, Coresident: [][2]string{{"rpcm_1", "P"}, {"P", "rspec_h"}}}}
	mgr := &fakeManager{}
	features, setFeatures := newSwitchableFeaturesServer(t, []string{runtimeManagerFeature, gatewayFeatureRuntimeInstances})
	d := newDriver(mgr, src, features, nil, "")
	ctx := context.Background()

	d.Sync(ctx, nil)
	got := mgr.lastApplied()
	if len(got.Specs) != 1 || got.Specs[0].ID != "P" {
		t.Fatalf("applied specs = %+v, want only the plain model without rpc_cluster", got.Specs)
	}
	if len(got.Coresident) != 0 {
		t.Fatalf("applied coresident = %v, want pairs naming dropped specs removed", got.Coresident)
	}
	if len(src.cfg.Specs) != 3 {
		t.Fatal("the source's cached document lost specs: the Driver must filter a copy")
	}

	d.Sync(ctx, nil)
	if n := mgr.applyCount(); n != 1 {
		t.Fatalf("Apply calls = %d after an unchanged Sync, want 1", n)
	}

	setFeatures([]string{runtimeManagerFeature, gatewayFeatureRuntimeInstances, gatewayFeatureRPCCluster})
	d.Sync(ctx, nil)
	if n := mgr.applyCount(); n != 2 {
		t.Fatalf("Apply calls = %d once the gateway declares rpc_cluster, want 2: the declaration alone re-applies", n)
	}
	if got := mgr.lastApplied(); len(got.Specs) != 3 || len(got.Coresident) != 2 {
		t.Fatalf("applied = %+v, want member, head and plain model with the declaration", got)
	}

	setFeatures([]string{runtimeManagerFeature, gatewayFeatureRuntimeInstances})
	d.Sync(ctx, nil)
	if n := mgr.applyCount(); n != 3 {
		t.Fatalf("Apply calls = %d after the declaration is withdrawn, want 3", n)
	}
	if got := mgr.lastApplied(); len(got.Specs) != 1 {
		t.Fatalf("applied specs = %+v, want the rpc specs dropped again", got.Specs)
	}
}

// withheldRPCWarn is the Driver's exact Warn for a document whose cluster specs
// it withholds because the gateway does not declare rpc_cluster.
const withheldRPCWarn = "runtime: the gateway does not declare rpc_cluster; rpc_server members and cluster heads are not run until it does"

// TestDriverWarnsOncePerRPCWithheldTransition: re-applying documents while the
// gateway still does not declare rpc_cluster logs the Warn once, not on every
// apply; a declaration ends the streak and a withdrawal starts a new one.
func TestDriverWarnsOncePerRPCWithheldTransition(t *testing.T) {
	logs := withCountedLogs(t)
	member := Spec{ID: "rpcm_1", Kind: SpecKindRPCServer, RPC: &RPCMember{Transport: "direct", BindIP: "127.0.0.1", Port: 47001}}
	plain := Spec{ID: "P", UpstreamModel: "p"}
	src := &fakeSource{cfg: Config{ETag: "e1", Specs: []Spec{member, plain}}, changed: true}
	mgr := &fakeManager{}
	features, setFeatures := newSwitchableFeaturesServer(t, []string{runtimeManagerFeature, gatewayFeatureRuntimeInstances})
	d := newDriver(mgr, src, features, nil, "")
	ctx := context.Background()

	for i := range 3 {
		src.mu.Lock()
		src.cfg.ETag = "e" + strconv.Itoa(i+1)
		src.mu.Unlock()
		d.Sync(ctx, nil)
	}
	if n := mgr.applyCount(); n != 3 {
		t.Fatalf("Apply calls = %d, want 3 (each document is new)", n)
	}
	if n := logs.count(withheldRPCWarn); n != 1 {
		t.Fatalf("withheld Warns = %d after three applies, want 1", n)
	}

	setFeatures([]string{runtimeManagerFeature, gatewayFeatureRuntimeInstances, gatewayFeatureRPCCluster})
	d.Sync(ctx, nil)
	setFeatures([]string{runtimeManagerFeature, gatewayFeatureRuntimeInstances})
	d.Sync(ctx, nil)
	if n := logs.count(withheldRPCWarn); n != 2 {
		t.Fatalf("withheld Warns = %d after a withdrawal, want 2", n)
	}
}
