// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
	"testing"
	"time"
)

var gpuVendorTestNow = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

// gpuVendorFixture is a server with a server_agent application and one
// mapping, its runtime spec stored with no GPU rows, and an agent that
// declares gpu_vendor_identity (switch it with flag/unflag/forget).
type gpuVendorFixture struct {
	t         *testing.T
	ctx       context.Context
	svc       *Service
	routes    *routing.MemoryStore
	serverID  string
	mappingID string
	specID    string
}

func newGPUVendorFixture(t *testing.T) *gpuVendorFixture {
	t.Helper()
	svc, routes := newServerTestService(t, gpuVendorTestNow)
	f := &gpuVendorFixture{t: t, ctx: context.Background(), svc: svc, routes: routes}
	server := createTestServer(t, svc, "S", "s.example.test")
	f.serverID = server.ID
	app := seedServerAgentApplication(t, routes, server.ID, gpuVendorTestNow)
	mapping, err := svc.CreateMapping(f.ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	f.mappingID = mapping.ID
	f.flag()
	dto, err := svc.PutRuntimeSpec(f.ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{Enabled: true, Binary: "/usr/local/bin/llama-server"})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	f.specID = dto.ID
	return f
}

// flag makes the server's agent declare gpu_vendor_identity.
func (f *gpuVendorFixture) flag() {
	f.svc.agentFeatures = fakeAgentFeatures{f.serverID: {"runtime_manager", AgentFeatureGPUVendorIdentity}}
}

// unflag makes the server's agent known without gpu_vendor_identity.
func (f *gpuVendorFixture) unflag() {
	f.svc.agentFeatures = fakeAgentFeatures{f.serverID: {"runtime_manager"}}
}

// forget makes the server's agent one this gateway has not heard from.
func (f *gpuVendorFixture) forget() {
	f.svc.agentFeatures = fakeAgentFeatures{}
}

// legacy stores vendor as the server's legacy GPU vendor (the backfill's
// write, which also rewrites the server's empty-vendor rows).
func (f *gpuVendorFixture) legacy(vendor string) {
	f.t.Helper()
	if _, err := f.routes.BackfillServerGPUVendor(f.ctx, f.serverID, vendor, false); err != nil {
		f.t.Fatalf("BackfillServerGPUVendor: %v", err)
	}
}

// specGPUs stores the spec's GPU rows directly, as a flagged agent's save
// would have, whatever the agent declares now.
func (f *gpuVendorFixture) specGPUs(rows ...routing.RuntimeSpecGPU) {
	f.t.Helper()
	for i := range rows {
		rows[i].SpecID, rows[i].Position = f.specID, i
	}
	if err := f.routes.SetRuntimeSpecGPUs(f.ctx, f.specID, rows); err != nil {
		f.t.Fatalf("SetRuntimeSpecGPUs: %v", err)
	}
}

func (f *gpuVendorFixture) budgets(rows ...routing.ServerGPUBudget) {
	f.t.Helper()
	for i := range rows {
		rows[i].ServerID, rows[i].CreatedAt, rows[i].UpdatedAt = f.serverID, gpuVendorTestNow, gpuVendorTestNow
	}
	if err := f.routes.SetServerGPUBudgets(f.ctx, f.serverID, rows); err != nil {
		f.t.Fatalf("SetServerGPUBudgets: %v", err)
	}
}

func (f *gpuVendorFixture) config() AgentRuntimeConfigDTO {
	f.t.Helper()
	dto, err := f.svc.AgentRuntimeConfig(f.ctx, f.serverID)
	if err != nil {
		f.t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	if len(dto.Specs) != 1 {
		f.t.Fatalf("specs = %+v, want the fixture's one spec", dto.Specs)
	}
	return dto
}

// put saves the fixture's spec with gpus as the operator.
func (f *gpuVendorFixture) put(gpus ...RuntimeSpecGPUDTO) (RuntimeSpecDTO, error) {
	return f.svc.PutRuntimeSpec(f.ctx, ownerToken(), f.mappingID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server", GPUs: gpus,
	})
}

func (f *gpuVendorFixture) storedVendors() []routing.GPUKey {
	f.t.Helper()
	rows, err := f.routes.RuntimeSpecGPUs(f.ctx, f.specID)
	if err != nil {
		f.t.Fatalf("RuntimeSpecGPUs: %v", err)
	}
	out := make([]routing.GPUKey, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Key())
	}
	return out
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// TestAgentRuntimeConfigSendsVendorOnlyToFlaggedAgents: the GPU rows of a
// spec and the GPU budgets carry their vendor only in the document of an
// agent that declares gpu_vendor_identity. An older agent gets exactly the
// document it got before vendors existed: no "vendor" key anywhere.
func TestAgentRuntimeConfigSendsVendorOnlyToFlaggedAgents(t *testing.T) {
	f := newGPUVendorFixture(t)
	f.legacy("amd")
	if _, err := f.put(RuntimeSpecGPUDTO{Vendor: "amd", Index: 1, VRAMEstimateMB: 4000}); err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if _, err := f.svc.SetServerGPUBudgets(f.ctx, ownerToken(), f.serverID, SetGPUBudgetsRequest{Budgets: []GPUBudgetDTO{{Vendor: "amd", Index: 1, BudgetMB: 9000}}}); err != nil {
		t.Fatalf("SetServerGPUBudgets: %v", err)
	}

	flagged := f.config()
	if want := []AgentRuntimeSpecGPUDTO{{Vendor: "amd", Index: 1, VRAMMB: 4000}}; !slices.Equal(flagged.Specs[0].GPUs, want) {
		t.Fatalf("flagged gpus = %+v, want %+v", flagged.Specs[0].GPUs, want)
	}
	if want := []AgentGPUBudgetDTO{{Vendor: "amd", Index: 1, BudgetMB: 9000}}; !slices.Equal(flagged.GPUBudgets, want) {
		t.Fatalf("flagged budgets = %+v, want %+v", flagged.GPUBudgets, want)
	}
	if raw := mustJSON(t, flagged); !strings.Contains(raw, `"vendor":"amd","index":1,"vram_mb":4000`) || !strings.Contains(raw, `"vendor":"amd","index":1,"budget_mb":9000`) {
		t.Fatalf("flagged document %s lacks the vendor", raw)
	}

	f.unflag()
	unflagged := f.config()
	if want := []AgentRuntimeSpecGPUDTO{{Index: 1, VRAMMB: 4000}}; !slices.Equal(unflagged.Specs[0].GPUs, want) {
		t.Fatalf("unflagged gpus = %+v, want %+v", unflagged.Specs[0].GPUs, want)
	}
	if want := []AgentGPUBudgetDTO{{Index: 1, BudgetMB: 9000}}; !slices.Equal(unflagged.GPUBudgets, want) {
		t.Fatalf("unflagged budgets = %+v, want %+v", unflagged.GPUBudgets, want)
	}
	if raw := mustJSON(t, unflagged); strings.Contains(raw, `"vendor"`) {
		t.Fatalf("unflagged document %s carries a vendor key", raw)
	}
	if unflagged.ETag == flagged.ETag {
		t.Fatal("the two documents share an etag")
	}
}

// TestAgentRuntimeConfigUnflaggedAgentDropsForeignVendorRows: after a
// backfill on a mixed host, a downgraded agent (no gpu_vendor_identity) gets
// only the legacy vendor's rows, without their vendor, so it never sees two
// rows with one index. The legacy vendor is the one persisted on the server:
// the vendor-less hardware report the downgraded agent sent since does not
// change it.
func TestAgentRuntimeConfigUnflaggedAgentDropsForeignVendorRows(t *testing.T) {
	f := newGPUVendorFixture(t)
	f.legacy("nvidia")
	if err := f.routes.UpsertServerHardware(f.ctx, routing.ServerHardware{
		ServerID: f.serverID, CollectedAt: gpuVendorTestNow, UpdatedAt: gpuVendorTestNow,
		ReportJSON: `{"gpus":[{"index":0,"name":"Radeon PRO W7900"}]}`,
	}); err != nil {
		t.Fatalf("UpsertServerHardware: %v", err)
	}
	f.specGPUs(
		routing.RuntimeSpecGPU{GPUVendor: "nvidia", GPUIndex: 0, VRAMEstimateMB: 1000},
		routing.RuntimeSpecGPU{GPUVendor: "amd", GPUIndex: 0, VRAMEstimateMB: 2000},
	)
	f.budgets(
		routing.ServerGPUBudget{GPUVendor: "amd", GPUIndex: 0, BudgetMB: 6000},
		routing.ServerGPUBudget{GPUVendor: "nvidia", GPUIndex: 0, BudgetMB: 5000},
	)
	f.unflag()

	doc := f.config()
	if want := []AgentRuntimeSpecGPUDTO{{Index: 0, VRAMMB: 1000}}; !slices.Equal(doc.Specs[0].GPUs, want) {
		t.Fatalf("gpus = %+v, want only the nvidia row %+v", doc.Specs[0].GPUs, want)
	}
	if want := []AgentGPUBudgetDTO{{Index: 0, BudgetMB: 5000}}; !slices.Equal(doc.GPUBudgets, want) {
		t.Fatalf("budgets = %+v, want only the nvidia budget %+v", doc.GPUBudgets, want)
	}
	if raw := mustJSON(t, doc); strings.Contains(raw, `"vendor"`) {
		t.Fatalf("document %s carries a vendor key", raw)
	}
	if doc.AwaitsAgentFeatures {
		t.Fatal("document held, want it served: the agent's declaration is known")
	}
}

// TestAgentRuntimeConfigUnflaggedAgentWithoutStoredLegacyVendor: with no
// legacy vendor stored, an unflagged agent gets only the rows without a
// vendor -- nothing says which vendor it would read an index as.
func TestAgentRuntimeConfigUnflaggedAgentWithoutStoredLegacyVendor(t *testing.T) {
	f := newGPUVendorFixture(t)
	f.specGPUs(
		routing.RuntimeSpecGPU{GPUVendor: "nvidia", GPUIndex: 0, VRAMEstimateMB: 1000},
		routing.RuntimeSpecGPU{GPUVendor: "amd", GPUIndex: 0, VRAMEstimateMB: 2000},
		routing.RuntimeSpecGPU{GPUVendor: "", GPUIndex: 1, VRAMEstimateMB: 3000},
	)
	f.unflag()
	if want := []AgentRuntimeSpecGPUDTO{{Index: 1, VRAMMB: 3000}}; !slices.Equal(f.config().Specs[0].GPUs, want) {
		t.Fatalf("gpus = %+v, want %+v", f.config().Specs[0].GPUs, want)
	}
}

// TestAgentRuntimeConfigHeldWhileVendorFlagUnknown: a document whose rows an
// unflagged agent would not get all of is held while the agent's declaration
// is unknown (after a gateway start): a flagged agent served the reduced
// document would drop the foreign vendor's GPUs from a running model. A
// document that loses nothing is served -- the legacy vendor's rows without
// their vendor resolve to the same cards on either agent.
func TestAgentRuntimeConfigHeldWhileVendorFlagUnknown(t *testing.T) {
	f := newGPUVendorFixture(t)
	f.legacy("nvidia")
	f.specGPUs(routing.RuntimeSpecGPU{GPUVendor: "nvidia", GPUIndex: 0, VRAMEstimateMB: 1000})
	f.forget()
	if doc := f.config(); doc.AwaitsAgentFeatures {
		t.Fatal("legacy-only document held, want it served")
	}

	f.specGPUs(
		routing.RuntimeSpecGPU{GPUVendor: "nvidia", GPUIndex: 0, VRAMEstimateMB: 1000},
		routing.RuntimeSpecGPU{GPUVendor: "amd", GPUIndex: 0, VRAMEstimateMB: 2000},
	)
	if doc := f.config(); !doc.AwaitsAgentFeatures {
		t.Fatal("document with a foreign vendor's row served to an agent not heard from, want it held")
	}

	f.specGPUs(routing.RuntimeSpecGPU{GPUVendor: "nvidia", GPUIndex: 0, VRAMEstimateMB: 1000})
	f.budgets(routing.ServerGPUBudget{GPUVendor: "amd", GPUIndex: 0, BudgetMB: 6000})
	if doc := f.config(); !doc.AwaitsAgentFeatures {
		t.Fatal("document with a foreign vendor's budget served to an agent not heard from, want it held")
	}
}

// TestAgentRuntimeConfigVendorStableAcrossBackfill pins the gateway half of
// "upgrade without relaunch": a flagged agent's document before and after
// the backfill rewrote the spec's empty vendors to the host's legacy vendor
// differs only by "vendor":"nvidia" on each GPU entry. The agent resolves an
// empty vendor to the same legacy vendor (A2, runtime.GPUKey resolution), so
// both documents name the same cards and the running model is not
// relaunched; TestApplyConfigVendorBackfillDoesNotRelaunch pins that half.
func TestAgentRuntimeConfigVendorStableAcrossBackfill(t *testing.T) {
	f := newGPUVendorFixture(t)
	if _, err := f.put(RuntimeSpecGPUDTO{Index: 0, VRAMEstimateMB: 1000}, RuntimeSpecGPUDTO{Index: 1, VRAMEstimateMB: 2000}); err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	before := f.config().Specs[0]
	f.legacy("nvidia")
	after := f.config().Specs[0]

	if want := []AgentRuntimeSpecGPUDTO{{Index: 0, VRAMMB: 1000}, {Index: 1, VRAMMB: 2000}}; !slices.Equal(before.GPUs, want) {
		t.Fatalf("before = %+v, want %+v", before.GPUs, want)
	}
	if want := []AgentRuntimeSpecGPUDTO{{Vendor: "nvidia", Index: 0, VRAMMB: 1000}, {Vendor: "nvidia", Index: 1, VRAMMB: 2000}}; !slices.Equal(after.GPUs, want) {
		t.Fatalf("after = %+v, want %+v", after.GPUs, want)
	}
	before.GPUs, after.GPUs = nil, nil
	if mustJSON(t, before) != mustJSON(t, after) {
		t.Fatalf("the spec changed beyond its GPU vendors:\nbefore %s\nafter  %s", mustJSON(t, before), mustJSON(t, after))
	}
}

// TestSaveRuntimeSpecVendorRules pins the save-time vendor rules on every
// write that stores GPU rows: the runtime spec, the GPU budgets and the RPC
// member spec.
func TestSaveRuntimeSpecVendorRules(t *testing.T) {
	t.Run("spec", func(t *testing.T) {
		f := newGPUVendorFixture(t)
		dto, err := f.put(RuntimeSpecGPUDTO{Vendor: "AMD", Index: 0})
		if err != nil {
			t.Fatalf("AMD: %v", err)
		}
		if len(dto.GPUs) != 1 || dto.GPUs[0].Vendor != "amd" {
			t.Fatalf("echo = %+v, want vendor amd", dto.GPUs)
		}
		if got := f.storedVendors(); !slices.Equal(got, []routing.GPUKey{{Vendor: "amd", Index: 0}}) {
			t.Fatalf("stored = %v, want amd 0", got)
		}
		if _, err := f.put(RuntimeSpecGPUDTO{Vendor: "intel", Index: 0}); !errors.Is(err, ErrRuntimeSpecGPUVendorInvalid) {
			t.Fatalf("intel: err = %v, want ErrRuntimeSpecGPUVendorInvalid", err)
		}
		if _, err := f.put(RuntimeSpecGPUDTO{Vendor: "nvidia", Index: 0}, RuntimeSpecGPUDTO{Vendor: "amd", Index: 0}); err != nil {
			t.Fatalf("nvidia 0 + amd 0: %v", err)
		}
		if got := f.storedVendors(); !slices.Equal(got, []routing.GPUKey{{Vendor: "nvidia", Index: 0}, {Vendor: "amd", Index: 0}}) {
			t.Fatalf("stored = %v, want nvidia 0 and amd 0 in request order", got)
		}
		if _, err := f.put(RuntimeSpecGPUDTO{Vendor: "amd", Index: 0}, RuntimeSpecGPUDTO{Vendor: "AMD", Index: 0}); !errors.Is(err, ErrRuntimeSpecGPUInvalid) {
			t.Fatalf("amd 0 twice: err = %v, want ErrRuntimeSpecGPUInvalid", err)
		}

		f.legacy("nvidia")
		if _, err := f.put(RuntimeSpecGPUDTO{Index: 1}); err != nil {
			t.Fatalf("empty on flagged: %v", err)
		}
		if got := f.storedVendors(); !slices.Equal(got, []routing.GPUKey{{Vendor: "nvidia", Index: 1}}) {
			t.Fatalf("stored = %v, want the legacy vendor nvidia 1", got)
		}
		if _, err := f.put(RuntimeSpecGPUDTO{Index: 0}, RuntimeSpecGPUDTO{Vendor: "nvidia", Index: 0}); !errors.Is(err, ErrRuntimeSpecGPUInvalid) {
			t.Fatalf("empty 0 + nvidia 0 with legacy nvidia: err = %v, want ErrRuntimeSpecGPUInvalid (one card twice)", err)
		}

		f.unflag()
		if _, err := f.put(RuntimeSpecGPUDTO{Vendor: "amd", Index: 0}); !errors.Is(err, ErrRuntimeSpecGPUVendorUnsupported) {
			t.Fatalf("amd on unflagged with legacy nvidia: err = %v, want ErrRuntimeSpecGPUVendorUnsupported", err)
		}
		// The stored legacy vendor is what the portal echoes back on every
		// re-save after a backfill; an unflagged agent gets the row without
		// it (pushedGPUVendor), so it is accepted and stored as sent.
		if _, err := f.put(RuntimeSpecGPUDTO{Vendor: "nvidia", Index: 0}); err != nil {
			t.Fatalf("legacy vendor nvidia on unflagged: %v", err)
		}
		if got := f.storedVendors(); !slices.Equal(got, []routing.GPUKey{{Vendor: "nvidia", Index: 0}}) {
			t.Fatalf("stored = %v, want nvidia 0 as sent", got)
		}
		if _, err := f.put(RuntimeSpecGPUDTO{Index: 0}); err != nil {
			t.Fatalf("empty on unflagged: %v", err)
		}
		if got := f.storedVendors(); !slices.Equal(got, []routing.GPUKey{{Vendor: "", Index: 0}}) {
			t.Fatalf("stored = %v, want an empty vendor for the unflagged agent", got)
		}

		f.forget()
		if _, err := f.put(RuntimeSpecGPUDTO{Vendor: "amd", Index: 0}); err != nil {
			t.Fatalf("amd for an agent not heard from: %v", err)
		}
	})

	t.Run("spec, unflagged agent without a stored legacy vendor", func(t *testing.T) {
		f := newGPUVendorFixture(t)
		f.unflag()
		if _, err := f.put(RuntimeSpecGPUDTO{Vendor: "nvidia", Index: 0}); !errors.Is(err, ErrRuntimeSpecGPUVendorUnsupported) {
			t.Fatalf("nvidia without a legacy vendor: err = %v, want ErrRuntimeSpecGPUVendorUnsupported", err)
		}
	})

	t.Run("budgets", func(t *testing.T) {
		f := newGPUVendorFixture(t)
		set := func(budgets ...GPUBudgetDTO) ([]GPUBudgetDTO, error) {
			return f.svc.SetServerGPUBudgets(f.ctx, ownerToken(), f.serverID, SetGPUBudgetsRequest{Budgets: budgets})
		}
		got, err := set(GPUBudgetDTO{Vendor: "AMD", Index: 0, BudgetMB: 1}, GPUBudgetDTO{Vendor: "nvidia", Index: 0, BudgetMB: 2})
		if err != nil {
			t.Fatalf("AMD 0 + nvidia 0: %v", err)
		}
		if len(got) != 2 || got[0].Vendor != "amd" || got[1].Vendor != "nvidia" {
			t.Fatalf("budgets = %+v, want amd 0 and nvidia 0", got)
		}
		if _, err := set(GPUBudgetDTO{Vendor: "intel", Index: 0}); !errors.Is(err, ErrRuntimeSpecGPUVendorInvalid) {
			t.Fatalf("intel: err = %v, want ErrRuntimeSpecGPUVendorInvalid", err)
		}
		if _, err := set(GPUBudgetDTO{Vendor: "amd", Index: 0}, GPUBudgetDTO{Vendor: "amd", Index: 0}); !errors.Is(err, ErrGPUBudgetInvalid) {
			t.Fatalf("amd 0 twice: err = %v, want ErrGPUBudgetInvalid", err)
		}
		f.legacy("nvidia")
		if got, err = set(GPUBudgetDTO{Index: 3, BudgetMB: 1}); err != nil || len(got) != 1 || got[0].Vendor != "nvidia" {
			t.Fatalf("empty on flagged = %+v, %v; want the legacy vendor nvidia", got, err)
		}
		f.unflag()
		if _, err := set(GPUBudgetDTO{Vendor: "amd", Index: 0}); !errors.Is(err, ErrRuntimeSpecGPUVendorUnsupported) {
			t.Fatalf("amd on unflagged: err = %v, want ErrRuntimeSpecGPUVendorUnsupported", err)
		}
	})

	t.Run("rpc member spec", func(t *testing.T) {
		f := newGPUVendorFixture(t)
		put := func(gpus ...RuntimeSpecGPUDTO) (RPCMemberSpecDTO, error) {
			req := validRPCMemberRequest()
			req.GPUs = gpus
			return f.svc.PutRPCMemberSpec(f.ctx, ownerToken(), f.serverID, "", req)
		}
		dto, err := put(RuntimeSpecGPUDTO{Vendor: "Nvidia", Index: 0, VRAMEstimateMB: 1}, RuntimeSpecGPUDTO{Vendor: "amd", Index: 0, VRAMEstimateMB: 1})
		if err != nil {
			t.Fatalf("Nvidia 0 + amd 0: %v", err)
		}
		if len(dto.GPUs) != 2 || dto.GPUs[0].Vendor != "nvidia" || dto.GPUs[1].Vendor != "amd" {
			t.Fatalf("member gpus = %+v, want nvidia 0 and amd 0 in request order", dto.GPUs)
		}
		rows, err := f.routes.RPCMemberSpecGPUs(f.ctx, dto.ID)
		if err != nil || len(rows) != 2 || rows[0].GPUVendor != "nvidia" || rows[1].GPUVendor != "amd" {
			t.Fatalf("stored member gpus = %+v, %v", rows, err)
		}
		if _, err := put(RuntimeSpecGPUDTO{Vendor: "intel", Index: 0}); !errors.Is(err, ErrRuntimeSpecGPUVendorInvalid) {
			t.Fatalf("intel: err = %v, want ErrRuntimeSpecGPUVendorInvalid", err)
		}
		if _, err := put(RuntimeSpecGPUDTO{Vendor: "amd", Index: 0, VRAMEstimateMB: 1}, RuntimeSpecGPUDTO{Vendor: "amd", Index: 0, VRAMEstimateMB: 1}); !errors.Is(err, ErrRuntimeSpecGPUInvalid) {
			t.Fatalf("amd 0 twice: err = %v, want ErrRuntimeSpecGPUInvalid", err)
		}
		f.unflag()
		if _, err := put(RuntimeSpecGPUDTO{Vendor: "amd", Index: 0, VRAMEstimateMB: 1}); !errors.Is(err, ErrRuntimeSpecGPUVendorUnsupported) {
			t.Fatalf("amd on unflagged: err = %v, want ErrRuntimeSpecGPUVendorUnsupported", err)
		}
	})
}

// TestClusterMemberGPUsCarryVendor: an automatic member's GPU selection is
// saved with its vendors (normalized like a spec's), read back with them,
// and pushed to the member's agent with them only while it declares
// gpu_vendor_identity.
func TestClusterMemberGPUsCarryVendor(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	features := f.clusterAgents(a.ID)
	features[a.ID] = append(features[a.ID], AgentFeatureGPUVendorIdentity)
	member := autoMember(a.ID, 0, 0)
	member.GPUs[0].Vendor, member.GPUs[1].Vendor = "NVIDIA", "amd"
	head := f.mustPut(clusterHeadRequest(member))

	_, gpus, err := f.routes.ClusterMembers(f.ctx, head.ID)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]routing.GPUKey, 0, len(gpus))
	for _, g := range gpus {
		keys = append(keys, routing.GPUKey{Vendor: g.GPUVendor, Index: g.GPUIndex})
	}
	if want := []routing.GPUKey{{Vendor: "nvidia", Index: 0}, {Vendor: "amd", Index: 0}}; !slices.Equal(keys, want) {
		t.Fatalf("stored member gpus = %v, want %v", keys, want)
	}
	if len(head.ClusterMembers) != 1 || len(head.ClusterMembers[0].GPUs) != 2 || head.ClusterMembers[0].GPUs[1].Vendor != "amd" {
		t.Fatalf("member echo = %+v, want the vendors", head.ClusterMembers)
	}

	ms, ok := specByID(f.config(a.ID), automaticMemberSpecID(head.ID, a.ID))
	if !ok {
		t.Fatal("member spec not derived")
	}
	if want := []AgentRuntimeSpecGPUDTO{{Vendor: "nvidia", Index: 0, VRAMMB: 8000}, {Vendor: "amd", Index: 0, VRAMMB: 8000}}; !slices.Equal(ms.GPUs, want) {
		t.Fatalf("flagged member gpus = %+v, want %+v", ms.GPUs, want)
	}

	features[a.ID] = []string{"runtime_manager", AgentFeatureRPCCluster}
	if _, err := f.routes.BackfillServerGPUVendor(f.ctx, a.ID, "nvidia", false); err != nil {
		t.Fatal(err)
	}
	ms, _ = specByID(f.config(a.ID), automaticMemberSpecID(head.ID, a.ID))
	if want := []AgentRuntimeSpecGPUDTO{{Index: 0, VRAMMB: 8000}}; !slices.Equal(ms.GPUs, want) {
		t.Fatalf("unflagged member gpus = %+v, want only the legacy vendor's %+v", ms.GPUs, want)
	}

	member.GPUs[0].Vendor = "intel"
	if _, err := f.put(ownerToken(), clusterHeadRequest(member)); !errors.Is(err, ErrRuntimeSpecGPUVendorInvalid) {
		t.Fatalf("intel member gpu: err = %v, want ErrRuntimeSpecGPUVendorInvalid", err)
	}
}

// TestBudgetDriftComparesSameVendorCard: a new budget's drift snapshot is
// the card of the budget's own (vendor, index), not another vendor's card
// at that index. A vendor-less sample (an agent without
// gpu_vendor_identity) is the legacy vendor's.
func TestBudgetDriftComparesSameVendorCard(t *testing.T) {
	f := newGPUVendorFixture(t)
	if err := f.routes.InsertTelemetrySample(f.ctx, routing.TelemetrySample{
		ServerID: f.serverID, ReportedAt: gpuVendorTestNow.Add(-time.Minute),
		GPUs: []routing.GPUSample{
			{Vendor: "amd", Index: 0, UUID: "GPU-B", Name: "Radeon PRO W7900"},
			{Vendor: "nvidia", Index: 0, UUID: "GPU-A", Name: "RTX 4090"},
		},
	}); err != nil {
		t.Fatalf("InsertTelemetrySample: %v", err)
	}
	got, err := f.svc.SetServerGPUBudgets(f.ctx, ownerToken(), f.serverID, SetGPUBudgetsRequest{Budgets: []GPUBudgetDTO{{Vendor: "amd", Index: 0, BudgetMB: 1}}})
	if err != nil {
		t.Fatalf("SetServerGPUBudgets: %v", err)
	}
	if len(got) != 1 || got[0].ExpectedUUID != "GPU-B" || got[0].ExpectedName != "Radeon PRO W7900" {
		t.Fatalf("budget = %+v, want the AMD card's snapshot GPU-B", got)
	}

	f.legacy("nvidia")
	if err := f.routes.InsertTelemetrySample(f.ctx, routing.TelemetrySample{
		ServerID: f.serverID, ReportedAt: gpuVendorTestNow,
		GPUs: []routing.GPUSample{{Index: 1, UUID: "GPU-C", Name: "RTX 3090"}},
	}); err != nil {
		t.Fatalf("InsertTelemetrySample: %v", err)
	}
	got, err = f.svc.SetServerGPUBudgets(f.ctx, ownerToken(), f.serverID, SetGPUBudgetsRequest{Budgets: []GPUBudgetDTO{
		{Vendor: "amd", Index: 0, BudgetMB: 1}, {Vendor: "nvidia", Index: 1, BudgetMB: 1},
	}})
	if err != nil {
		t.Fatalf("SetServerGPUBudgets: %v", err)
	}
	if len(got) != 2 || got[0].ExpectedUUID != "GPU-B" || got[1].ExpectedUUID != "GPU-C" {
		t.Fatalf("budgets = %+v, want amd 0 kept at GPU-B and nvidia 1 snapshotted from the vendor-less sample GPU-C", got)
	}
}

// TestClusterHiddenMemberKeptDespiteVendorUnsupported: a member the caller
// cannot see is kept as stored on a head re-save, also when its agent no
// longer declares gpu_vendor_identity while its stored GPUs carry a vendor:
// the caller could neither see nor change why the save was refused. A
// visible member's vendor is refused for such an agent like any other save.
func TestClusterHiddenMemberKeptDespiteVendorUnsupported(t *testing.T) {
	f := newClusterFixture(t)
	mine, foreign := f.member("Mine"), f.foreignServer("Foreign")
	features := f.clusterAgents(mine.ID, foreign.ID)
	features[foreign.ID] = append(features[foreign.ID], AgentFeatureGPUVendorIdentity)
	hidden := autoMember(foreign.ID, 0)
	hidden.GPUs[0].Vendor = "nvidia"
	if _, err := f.put(systemToken(), clusterHeadRequest(autoMember(mine.ID, 0), hidden)); err != nil {
		t.Fatalf("system save: %v", err)
	}
	features[foreign.ID] = []string{"runtime_manager", AgentFeatureRPCCluster}

	got, err := f.svc.GetRuntimeSpec(f.ctx, ownerToken(), f.mappingID)
	if err != nil {
		t.Fatal(err)
	}
	resave := putRequestFromDTO(got)
	resave.IdleTimeoutSeconds = 900
	if _, err := f.put(ownerToken(), resave); err != nil {
		t.Fatalf("owner re-save with a hidden vendor-qualified member = %v", err)
	}

	// Dropping the hidden member needs no rights on its server.
	resave.ClusterMembers = resave.ClusterMembers[:1]
	resave.ClusterMembers[0].GPUs[0].Vendor = "nvidia"
	if _, err := f.put(ownerToken(), resave); !errors.Is(err, ErrRuntimeSpecGPUVendorUnsupported) {
		t.Fatalf("visible member vendor on an unflagged agent: err = %v, want ErrRuntimeSpecGPUVendorUnsupported", err)
	}
}

// TestOverridesWorkForUnflaggedAgentAfterBackfill: after a backfill gave a
// spec's GPU rows the legacy vendor and the agent was downgraded (no
// gpu_vendor_identity), the portal's Stop, Start and Pin overrides -- each
// re-sends the whole stored spec, vendors included -- still save, and the
// agent's document carries the rows without their vendor.
func TestOverridesWorkForUnflaggedAgentAfterBackfill(t *testing.T) {
	f := newGPUVendorFixture(t)
	if _, err := f.put(RuntimeSpecGPUDTO{Index: 0, VRAMEstimateMB: 1000}); err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	f.legacy("nvidia")
	f.unflag()
	for _, override := range []struct {
		name  string
		apply func(*PutRuntimeSpecRequest)
	}{
		{"stop", func(r *PutRuntimeSpecRequest) { r.AdminState = "force_stopped" }},
		{"start", func(r *PutRuntimeSpecRequest) { r.AdminState = "force_running" }},
		{"pin", func(r *PutRuntimeSpecRequest) { r.AdminState, r.Pinned = "", true }},
	} {
		stored, err := f.svc.GetRuntimeSpec(f.ctx, ownerToken(), f.mappingID)
		if err != nil {
			t.Fatalf("GetRuntimeSpec: %v", err)
		}
		if len(stored.GPUs) != 1 || stored.GPUs[0].Vendor != "nvidia" {
			t.Fatalf("stored gpus = %+v, want the backfilled nvidia 0", stored.GPUs)
		}
		req := putRequestFromDTO(stored)
		override.apply(&req)
		if _, err := f.svc.PutRuntimeSpec(f.ctx, ownerToken(), f.mappingID, req); err != nil {
			t.Fatalf("%s override: %v", override.name, err)
		}
	}
	if want := []AgentRuntimeSpecGPUDTO{{Index: 0, VRAMMB: 1000}}; !slices.Equal(f.config().Specs[0].GPUs, want) {
		t.Fatalf("gpus = %+v, want %+v", f.config().Specs[0].GPUs, want)
	}
}

// TestUnflaggedAgentResavesStoredForeignVendorRows: a flagged agent saved
// AMD rows and was then downgraded. The portal's overrides re-send the
// whole stored spec, and a budget or member-spec re-save re-sends the
// stored rows: a (vendor, index) already stored on that spec, budget list
// or member spec is accepted. A new foreign-vendor row is still refused.
func TestUnflaggedAgentResavesStoredForeignVendorRows(t *testing.T) {
	f := newGPUVendorFixture(t)
	f.legacy("nvidia")
	if _, err := f.put(RuntimeSpecGPUDTO{Vendor: "nvidia", Index: 0, VRAMEstimateMB: 1000}, RuntimeSpecGPUDTO{Vendor: "amd", Index: 0, VRAMEstimateMB: 2000}); err != nil {
		t.Fatalf("flagged save: %v", err)
	}
	budgets, err := f.svc.SetServerGPUBudgets(f.ctx, ownerToken(), f.serverID, SetGPUBudgetsRequest{Budgets: []GPUBudgetDTO{{Vendor: "amd", Index: 0, BudgetMB: 1}}})
	if err != nil {
		t.Fatalf("flagged budgets: %v", err)
	}
	memberReq := validRPCMemberRequest()
	memberReq.GPUs = []RuntimeSpecGPUDTO{{Vendor: "amd", Index: 0, VRAMEstimateMB: 1}}
	member, err := f.svc.PutRPCMemberSpec(f.ctx, ownerToken(), f.serverID, "", memberReq)
	if err != nil {
		t.Fatalf("flagged member spec: %v", err)
	}
	f.unflag()

	for _, override := range []struct {
		name  string
		apply func(*PutRuntimeSpecRequest)
	}{
		{"stop", func(r *PutRuntimeSpecRequest) { r.AdminState = "force_stopped" }},
		{"start", func(r *PutRuntimeSpecRequest) { r.AdminState = "force_running" }},
		{"pin", func(r *PutRuntimeSpecRequest) { r.AdminState, r.Pinned = "", true }},
	} {
		stored, err := f.svc.GetRuntimeSpec(f.ctx, ownerToken(), f.mappingID)
		if err != nil {
			t.Fatalf("GetRuntimeSpec: %v", err)
		}
		req := putRequestFromDTO(stored)
		override.apply(&req)
		if _, err := f.svc.PutRuntimeSpec(f.ctx, ownerToken(), f.mappingID, req); err != nil {
			t.Fatalf("%s override with a stored amd row: %v", override.name, err)
		}
	}
	if got := f.storedVendors(); !slices.Equal(got, []routing.GPUKey{{Vendor: "nvidia", Index: 0}, {Vendor: "amd", Index: 0}}) {
		t.Fatalf("stored = %v, want the rows kept", got)
	}
	if _, err := f.put(RuntimeSpecGPUDTO{Vendor: "nvidia", Index: 0}, RuntimeSpecGPUDTO{Vendor: "amd", Index: 1}); !errors.Is(err, ErrRuntimeSpecGPUVendorUnsupported) {
		t.Fatalf("new amd 1 row: err = %v, want ErrRuntimeSpecGPUVendorUnsupported", err)
	}

	if _, err := f.svc.SetServerGPUBudgets(f.ctx, ownerToken(), f.serverID, SetGPUBudgetsRequest{Budgets: []GPUBudgetDTO{{Vendor: budgets[0].Vendor, Index: 0, BudgetMB: 2}}}); err != nil {
		t.Fatalf("budget re-save with the stored amd row: %v", err)
	}
	if _, err := f.svc.SetServerGPUBudgets(f.ctx, ownerToken(), f.serverID, SetGPUBudgetsRequest{Budgets: []GPUBudgetDTO{{Vendor: "amd", Index: 1, BudgetMB: 2}}}); !errors.Is(err, ErrRuntimeSpecGPUVendorUnsupported) {
		t.Fatalf("new amd 1 budget: err = %v, want ErrRuntimeSpecGPUVendorUnsupported", err)
	}

	memberReq.GPUs = member.GPUs
	if _, err := f.svc.PutRPCMemberSpec(f.ctx, ownerToken(), f.serverID, member.ID, memberReq); err != nil {
		t.Fatalf("member spec re-save with the stored amd row: %v", err)
	}
	memberReq.GPUs = []RuntimeSpecGPUDTO{{Vendor: "amd", Index: 1, VRAMEstimateMB: 1}}
	if _, err := f.svc.PutRPCMemberSpec(f.ctx, ownerToken(), f.serverID, member.ID, memberReq); !errors.Is(err, ErrRuntimeSpecGPUVendorUnsupported) {
		t.Fatalf("new amd 1 member gpu: err = %v, want ErrRuntimeSpecGPUVendorUnsupported", err)
	}
	// A new member spec has nothing stored.
	memberReq.GPUs = []RuntimeSpecGPUDTO{{Vendor: "amd", Index: 0, VRAMEstimateMB: 1}}
	if _, err := f.svc.PutRPCMemberSpec(f.ctx, ownerToken(), f.serverID, "", memberReq); !errors.Is(err, ErrRuntimeSpecGPUVendorUnsupported) {
		t.Fatalf("amd 0 on a new member spec: err = %v, want ErrRuntimeSpecGPUVendorUnsupported", err)
	}
}

// TestAgentRuntimeConfigWithholdsSpecWithAllGPUsDropped: a spec with GPU
// rows of which an unflagged agent gets none is withheld from its document
// -- an empty GPU list would run it with no VRAM reservation, invisible to
// admission. A spec that keeps at least one row is pushed with it. While the
// agent's declaration is unknown the document is held instead, so the spec
// is never served and then withdrawn.
func TestAgentRuntimeConfigWithholdsSpecWithAllGPUsDropped(t *testing.T) {
	f := newGPUVendorFixture(t)
	f.legacy("nvidia")
	f.specGPUs(routing.RuntimeSpecGPU{GPUVendor: "amd", GPUIndex: 0, VRAMEstimateMB: 2000})
	f.unflag()
	doc, err := f.svc.AgentRuntimeConfig(f.ctx, f.serverID)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Specs) != 0 || doc.AwaitsAgentFeatures {
		t.Fatalf("specs = %+v held=%v, want the amd-only spec withheld and the document served", doc.Specs, doc.AwaitsAgentFeatures)
	}

	f.forget()
	if doc, err = f.svc.AgentRuntimeConfig(f.ctx, f.serverID); err != nil || !doc.AwaitsAgentFeatures {
		t.Fatalf("unknown declaration: held=%v err=%v, want the document held", doc.AwaitsAgentFeatures, err)
	}

	f.unflag()
	f.specGPUs(
		routing.RuntimeSpecGPU{GPUVendor: "nvidia", GPUIndex: 0, VRAMEstimateMB: 1000},
		routing.RuntimeSpecGPU{GPUVendor: "amd", GPUIndex: 0, VRAMEstimateMB: 2000},
	)
	if want := []AgentRuntimeSpecGPUDTO{{Index: 0, VRAMMB: 1000}}; !slices.Equal(f.config().Specs[0].GPUs, want) {
		t.Fatalf("gpus = %+v, want %+v", f.config().Specs[0].GPUs, want)
	}

	f.flag()
	if want := []AgentRuntimeSpecGPUDTO{{Vendor: "nvidia", Index: 0, VRAMMB: 1000}, {Vendor: "amd", Index: 0, VRAMMB: 2000}}; !slices.Equal(f.config().Specs[0].GPUs, want) {
		t.Fatalf("flagged gpus = %+v, want %+v", f.config().Specs[0].GPUs, want)
	}
}

// TestAgentRuntimeConfigWithholdsMemberWithAllGPUsDropped: the same for an
// RPC member spec in a member server's document.
func TestAgentRuntimeConfigWithholdsMemberWithAllGPUsDropped(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	features := f.clusterAgents(a.ID)
	features[a.ID] = append(features[a.ID], AgentFeatureGPUVendorIdentity)
	member := autoMember(a.ID, 0)
	member.GPUs[0].Vendor = "amd"
	head := f.mustPut(clusterHeadRequest(member))
	if _, ok := specByID(f.config(a.ID), automaticMemberSpecID(head.ID, a.ID)); !ok {
		t.Fatal("flagged member spec not derived")
	}
	features[a.ID] = []string{"runtime_manager", AgentFeatureRPCCluster}
	if _, err := f.routes.BackfillServerGPUVendor(f.ctx, a.ID, "nvidia", false); err != nil {
		t.Fatal(err)
	}
	if ms, ok := specByID(f.config(a.ID), automaticMemberSpecID(head.ID, a.ID)); ok {
		t.Fatalf("member spec %+v pushed with every GPU dropped, want it withheld", ms)
	}
}

// TestClusterHeadOverrideKeepsStoredMemberVendor: a visible automatic
// member whose stored GPU carries a vendor its agent no longer reads is kept
// on a head override (the whole head is re-sent); a new foreign-vendor GPU
// on it is refused.
func TestClusterHeadOverrideKeepsStoredMemberVendor(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	features := f.clusterAgents(a.ID)
	features[a.ID] = append(features[a.ID], AgentFeatureGPUVendorIdentity)
	member := autoMember(a.ID, 0)
	member.GPUs[0].Vendor = "amd"
	f.mustPut(clusterHeadRequest(member))
	features[a.ID] = []string{"runtime_manager", AgentFeatureRPCCluster}

	got, err := f.svc.GetRuntimeSpec(f.ctx, ownerToken(), f.mappingID)
	if err != nil {
		t.Fatal(err)
	}
	req := putRequestFromDTO(got)
	req.AdminState = "force_stopped"
	if _, err := f.put(ownerToken(), req); err != nil {
		t.Fatalf("stop override with a stored amd member gpu: %v", err)
	}
	req.ClusterMembers[0].GPUs = append(req.ClusterMembers[0].GPUs, ClusterMemberGPUDTO{Vendor: "amd", Index: 1, VRAMEstimateMB: 8000})
	if _, err := f.put(ownerToken(), req); !errors.Is(err, ErrRuntimeSpecGPUVendorUnsupported) {
		t.Fatalf("new amd 1 member gpu: err = %v, want ErrRuntimeSpecGPUVendorUnsupported", err)
	}
}

func (f *gpuVendorFixture) hardware(report string) {
	f.t.Helper()
	if err := f.routes.UpsertServerHardware(f.ctx, routing.ServerHardware{ServerID: f.serverID, CollectedAt: gpuVendorTestNow, UpdatedAt: gpuVendorTestNow, ReportJSON: report}); err != nil {
		f.t.Fatalf("UpsertServerHardware: %v", err)
	}
}

// putSpec saves the fixture's spec as the operator, with req's device and
// visibility fields over a valid base.
func (f *gpuVendorFixture) putSpec(backend string, setVisible bool, mode string, env map[string]string, gpus ...RuntimeSpecGPUDTO) error {
	_, err := f.svc.PutRuntimeSpec(f.ctx, ownerToken(), f.mappingID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server", GPUs: gpus, DeviceBackend: backend,
		SetVisibleDevices: setVisible, VisibleDevicesMode: mode, Env: env,
		Args: []string{"--device", "${VULKAN_DEVICES}"},
	})
	return err
}

var (
	specNVIDIA0 = RuntimeSpecGPUDTO{Vendor: "nvidia", Index: 0}
	specAMD0    = RuntimeSpecGPUDTO{Vendor: "amd", Index: 0}
)

func TestSaveSpecRefusesVendorBackendMismatch(t *testing.T) {
	f := newGPUVendorFixture(t)
	f.hardware(mixedHostReport(`["rocm","vulkan"]`))
	for _, tc := range []struct {
		backend string
		gpus    []RuntimeSpecGPUDTO
		want    error
	}{
		{"cuda", []RuntimeSpecGPUDTO{specNVIDIA0, specAMD0}, ErrRuntimeSpecGPUVendorBackendMismatch},
		{"rocm", []RuntimeSpecGPUDTO{specNVIDIA0}, ErrRuntimeSpecGPUVendorBackendMismatch},
		{"metal", []RuntimeSpecGPUDTO{specAMD0}, ErrRuntimeSpecGPUVendorBackendMismatch},
		{"sycl", []RuntimeSpecGPUDTO{specNVIDIA0, specAMD0}, ErrRuntimeSpecGPUVendorBackendMismatch},
		{"cuda", []RuntimeSpecGPUDTO{specNVIDIA0}, nil},
		{"auto", []RuntimeSpecGPUDTO{specAMD0}, nil},
	} {
		if err := f.putSpec(tc.backend, true, "env", nil, tc.gpus...); !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Errorf("%s %v: err = %v, want %v", tc.backend, tc.gpus, err, tc.want)
		}
	}
}

func TestSaveSpecRefusesVulkanWithoutEnvVisibilityOnMixedHost(t *testing.T) {
	f := newGPUVendorFixture(t)
	f.hardware(mixedHostReport(`["rocm","vulkan"]`))
	both := []RuntimeSpecGPUDTO{specNVIDIA0, specAMD0}
	for _, tc := range []struct {
		name, backend string
		setVisible    bool
		mode          string
		gpus          []RuntimeSpecGPUDTO
		want          error
	}{
		{"vulkan without visibility", "vulkan", false, "", both, ErrRuntimeSpecVulkanVisibleDevicesRequired},
		{"auto over both vendors without visibility", "auto", false, "", both, ErrRuntimeSpecVulkanVisibleDevicesRequired},
		{"vulkan on one card without visibility", "vulkan", false, "", []RuntimeSpecGPUDTO{specAMD0}, ErrRuntimeSpecVulkanVisibleDevicesRequired},
		{"vulkan in args mode", "vulkan", true, "args", both, ErrRuntimeSpecVulkanVisibleDevicesRequired},
		{"vulkan in env mode", "vulkan", true, "env", both, nil},
		{"vulkan in default mode", "vulkan", true, "", both, nil},
		{"vulkan without gpus", "vulkan", false, "", nil, nil},
		{"rocm on the amd card without visibility", "rocm", false, "", []RuntimeSpecGPUDTO{specAMD0}, nil},
	} {
		if err := f.putSpec(tc.backend, tc.setVisible, tc.mode, nil, tc.gpus...); !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}

	// A single-vendor host has no loader-order ambiguity.
	f.hardware(`{"gpus":[{"index":0,"name":"AMD Radeon RX 7900 XT","vendor":"amd"},{"index":1,"name":"AMD Radeon RX 7900 XT","vendor":"amd"}]}`)
	if err := f.putSpec("vulkan", false, "", nil, specAMD0, RuntimeSpecGPUDTO{Vendor: "amd", Index: 1}); err != nil {
		t.Fatalf("single-vendor host: %v", err)
	}
	// The host turns mixed: a re-save of the stored spec (an override)
	// still saves; the agent reports the refusal at launch.
	f.hardware(mixedHostReport(`["rocm","vulkan"]`))
	if err := f.putSpec("vulkan", false, "", nil, specAMD0, RuntimeSpecGPUDTO{Vendor: "amd", Index: 1}); err != nil {
		t.Fatalf("re-save of a stored spec on a host turned mixed: %v", err)
	}
}

func TestSaveSpecRefusesHandSetGGMLVKVisibleDevices(t *testing.T) {
	f := newGPUVendorFixture(t)
	err := f.putSpec("vulkan", true, "env", map[string]string{"GGML_VK_VISIBLE_DEVICES": "0"}, specAMD0)
	if !errors.Is(err, ErrRuntimeSpecVisibleDevicesConflict) {
		t.Fatalf("err = %v, want ErrRuntimeSpecVisibleDevicesConflict", err)
	}
}
