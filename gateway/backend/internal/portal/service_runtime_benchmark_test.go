// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"reflect"
	"testing"
	"time"
)

// benchmarkSpecFixture seeds one server_agent application with one mapping
// and a fully-populated launch spec, and returns the service, the recording
// runtime-changed hook, and the stored spec's DTO.
func benchmarkSpecFixture(t *testing.T) (*Service, func() []string, string, RuntimeSpecDTO) {
	t.Helper()
	now := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	calls := recordRuntimeChanged(svc)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen", AppModelName: "qwen"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	spec, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled:                     true,
		Binary:                      "/usr/local/bin/llama-server",
		Args:                        []string{"--model", "/models/q.gguf", "--ctx-size", "8192"},
		Env:                         map[string]string{"HF_TOKEN": "${AGENT_ENV:HF_TOKEN}"},
		WorkDir:                     "/models",
		ListenPort:                  18080,
		HealthPath:                  "/healthz",
		HealthTimeoutSeconds:        7,
		StartupTimeoutSeconds:       240,
		IdleTimeoutSeconds:          900,
		AdmissionWaitTimeoutSeconds: 45,
		Pinned:                      true,
		VRAMLocked:                  true,
		GPUs:                        []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 18000}, {Index: 1, VRAMEstimateMB: 18000}},
		// Deliberately non-default, so a full-document write that quietly
		// re-defaulted these (rather than spreading the loaded document) would
		// be caught by TestSetBenchmarkRuntimeSpecAdminStateIsAFullDocumentWriteThatNotifies.
		APIFlavors:    []string{routing.APIFlavorOpenAI},
		ResponsesMode: string(routing.EndpointModeDisabled),
		MessagesMode:  string(routing.EndpointModeTranslate),
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	// The fixture's own write notified; the tests below care only about what
	// happens after it, so reset by remembering the baseline count.
	return svc, calls, server.ID, spec
}

// TestPutRequestFromDTOCoversEveryWritableField is test-plan item 12, and it
// exists because of a defect this project already paid for: a runtime-spec
// write assembled from a hand-picked field list quietly reset the operator's
// binary path, args, timeouts and GPU rows, while a test asserting only that
// admin_state came out right passed anyway. A Go caller has no `...rest`
// spread, so the mapper IS the spread -- and this test fails the moment
// RuntimeSpecDTO gains a field the mapper does not carry across.
//
// The two DTO fields deliberately NOT in PutRuntimeSpecRequest are named
// explicitly rather than skipped by a wildcard: Configured/ID/MappingID are
// identity, not input, and vram_measured_mb is agent-owned (PutRuntimeSpec
// copies the stored value forward and ignores what a request sends).
func TestPutRequestFromDTOCoversEveryWritableField(t *testing.T) {
	notInRequest := map[string]bool{
		"configured": true, // GetRuntimeSpec's "no spec row yet" signal, never an input
		"id":         true, // the spec's own identity
		"mapping_id": true, // the key the write is addressed by
	}
	dtoTags := map[string]bool{}
	dtoType := reflect.TypeOf(RuntimeSpecDTO{})
	for i := 0; i < dtoType.NumField(); i++ {
		tag := jsonTagName(t, dtoType.Field(i))
		if notInRequest[tag] {
			continue
		}
		dtoTags[tag] = true
	}
	reqTags := map[string]bool{}
	reqType := reflect.TypeOf(PutRuntimeSpecRequest{})
	for i := 0; i < reqType.NumField(); i++ {
		reqTags[jsonTagName(t, reqType.Field(i))] = true
	}
	for tag := range dtoTags {
		if !reqTags[tag] {
			t.Fatalf("RuntimeSpecDTO field %q has no PutRuntimeSpecRequest counterpart: either it is not writable (add it to notInRequest, with a reason) or the request is missing it", tag)
		}
	}
	for tag := range reqTags {
		if !dtoTags[tag] {
			t.Fatalf("PutRuntimeSpecRequest field %q has no RuntimeSpecDTO counterpart, so putRequestFromDTO cannot round-trip it", tag)
		}
	}

	// The mapper itself: every field of a fully-populated DTO must survive.
	dto := RuntimeSpecDTO{
		Configured:                  true,
		ID:                          "rspec_x",
		MappingID:                   "map_x",
		Enabled:                     true,
		Binary:                      "/opt/llama/llama-server",
		Args:                        []string{"--flash-attn"},
		Env:                         map[string]string{"OMP_NUM_THREADS": "8"},
		WorkDir:                     "/opt/llama",
		ListenPort:                  19000,
		HealthPath:                  "/ready",
		HealthTimeoutSeconds:        9,
		StartupTimeoutSeconds:       300,
		IdleTimeoutSeconds:          600,
		AdmissionWaitTimeoutSeconds: 30,
		Pinned:                      true,
		AdminState:                  "force_running",
		VRAMLocked:                  true,
		SetVisibleDevices:           true,
		GPUs:                        []RuntimeSpecGPUDTO{{Index: 3, VRAMEstimateMB: 12000, VRAMMeasuredMB: 11500}},
		APIFlavors:                  []string{routing.APIFlavorOpenAI},
		ResponsesMode:               string(routing.EndpointModeDisabled),
		MessagesMode:                string(routing.EndpointModeTranslate),
	}
	req := putRequestFromDTO(dto)
	want := PutRuntimeSpecRequest{
		Enabled:                     dto.Enabled,
		Binary:                      dto.Binary,
		Args:                        dto.Args,
		Env:                         dto.Env,
		WorkDir:                     dto.WorkDir,
		ListenPort:                  dto.ListenPort,
		HealthPath:                  dto.HealthPath,
		HealthTimeoutSeconds:        dto.HealthTimeoutSeconds,
		StartupTimeoutSeconds:       dto.StartupTimeoutSeconds,
		IdleTimeoutSeconds:          dto.IdleTimeoutSeconds,
		AdmissionWaitTimeoutSeconds: dto.AdmissionWaitTimeoutSeconds,
		Pinned:                      dto.Pinned,
		AdminState:                  dto.AdminState,
		VRAMLocked:                  dto.VRAMLocked,
		SetVisibleDevices:           dto.SetVisibleDevices,
		GPUs:                        dto.GPUs,
		APIFlavors:                  dto.APIFlavors,
		ResponsesMode:               dto.ResponsesMode,
		MessagesMode:                dto.MessagesMode,
	}
	if !reflect.DeepEqual(req, want) {
		t.Fatalf("putRequestFromDTO dropped or altered a field:\n got %#v\nwant %#v", req, want)
	}
}

// jsonTagName returns a struct field's json tag name (before any option).
func jsonTagName(t *testing.T, field reflect.StructField) string {
	t.Helper()
	tag := field.Tag.Get("json")
	if tag == "" {
		t.Fatalf("%s has no json tag", field.Name)
	}
	for i := 0; i < len(tag); i++ {
		if tag[i] == ',' {
			return tag[:i]
		}
	}
	return tag
}

// TestSetBenchmarkRuntimeSpecAdminStateIsAFullDocumentWriteThatNotifies is
// test-plan item 5 plus the full-document half of the trap list: admin_state
// is row 4 of the runtime-config document, so this write MUST notify (the
// notification is the sole trigger for the agent push, and a write that does
// not notify reaches a WS-connected agent no sooner than its 60 s poll) -- and
// it must reach the store as a SPREAD of the loaded document, so no other
// field of the operator's launch spec moves.
func TestSetBenchmarkRuntimeSpecAdminStateIsAFullDocumentWriteThatNotifies(t *testing.T) {
	ctx := context.Background()
	svc, calls, serverID, spec := benchmarkSpecFixture(t)
	before := len(calls())

	got, err := svc.SetBenchmarkRuntimeSpecAdminState(ctx, spec.ID, "", "force_stopped")
	if err != nil {
		t.Fatalf("SetBenchmarkRuntimeSpecAdminState: %v", err)
	}
	if got.AdminState != "force_stopped" {
		t.Fatalf("AdminState = %q, want force_stopped", got.AdminState)
	}
	after := calls()
	if len(after) != before+1 || after[len(after)-1] != serverID {
		t.Fatalf("runtime-changed calls = %#v, want exactly one more for %q", after, serverID)
	}

	// Every other field of the launch spec survived the write untouched.
	want := spec
	want.AdminState = "force_stopped"
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the write moved a field it does not own:\n got %#v\nwant %#v", got, want)
	}

	// And the restore notifies too -- the drain and the restore are two writes
	// to the same row of the same document.
	restored, err := svc.SetBenchmarkRuntimeSpecAdminState(ctx, spec.ID, "force_stopped", "")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.AdminState != "" {
		t.Fatalf("restored AdminState = %q, want empty", restored.AdminState)
	}
	if got := calls(); len(got) != before+2 {
		t.Fatalf("runtime-changed calls after the restore = %#v, want one more still", got)
	}
}

// TestSetBenchmarkRuntimeSpecAdminStateIsCompareAndSet is the half of
// test-plan item 6 that makes the deferred restore safe: this endpoint class
// is a read-modify-write with no If-Match and no row version, so the write
// re-reads and refuses when the stored admin_state is no longer the one the
// caller is replacing. Without it a restore would hand a concurrent
// operator's override straight back to "".
func TestSetBenchmarkRuntimeSpecAdminStateIsCompareAndSet(t *testing.T) {
	ctx := context.Background()
	svc, calls, _, spec := benchmarkSpecFixture(t)

	if _, err := svc.SetBenchmarkRuntimeSpecAdminState(ctx, spec.ID, "", "force_stopped"); err != nil {
		t.Fatalf("drain: %v", err)
	}
	// Somebody else takes the field over while the run holds it.
	if _, err := svc.SetBenchmarkRuntimeSpecAdminState(ctx, spec.ID, "force_stopped", "force_running"); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	before := len(calls())

	// The restore's expectation no longer holds: refuse, and write NOTHING.
	if _, err := svc.SetBenchmarkRuntimeSpecAdminState(ctx, spec.ID, "force_stopped", ""); !errors.Is(err, ErrRuntimeSpecAdminStateConflict) {
		t.Fatalf("restore over a taken-over override err = %v, want ErrRuntimeSpecAdminStateConflict", err)
	}
	current, err := svc.GetRuntimeSpec(ctx, ownerToken(), spec.MappingID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if current.AdminState != "force_running" {
		t.Fatalf("AdminState after a refused restore = %q, want the takeover's force_running", current.AdminState)
	}
	if got := calls(); len(got) != before {
		t.Fatalf("a refused write notified: calls = %#v, want no new call", got)
	}
}

// TestSetBenchmarkRuntimeSpecAdminStateRefusesAnOverrideOnAMissingSpec pins
// the two absent cases apart. A spec DELETED mid-run is not a restore failure
// -- its override went with it -- so the caller needs a sentinel it can tell
// from a conflict.
func TestSetBenchmarkRuntimeSpecAdminStateRefusesAnOverrideOnAMissingSpec(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := benchmarkSpecFixture(t)
	if _, err := svc.SetBenchmarkRuntimeSpecAdminState(ctx, "rspec_nope", "force_stopped", ""); !errors.Is(err, ErrRuntimeSpecNotFound) {
		t.Fatalf("err = %v, want ErrRuntimeSpecNotFound", err)
	}
}

// TestSetBenchmarkRuntimeSpecAdminStateNeverWritesTheVRAMFields is
// test-plan item 11 at the layer that would actually do the damage, and it is
// named after the rule it protects: vram_estimate_mb is operator-owned and
// vram_measured_mb is agent-owned, so a benchmark's own write path must move
// neither. It also covers the field that made the ratchet dangerous:
// vram_locked survives, because it is the operator's only escape from being
// governed by a measurement.
func TestSetBenchmarkRuntimeSpecAdminStateNeverWritesTheVRAMFields(t *testing.T) {
	ctx := context.Background()
	svc, _, _, spec := benchmarkSpecFixture(t)

	// An agent measurement lands first, through the one writer that owns it.
	routes := svc.routes
	for _, index := range []int{0, 1} {
		if err := routes.UpdateRuntimeSpecGPUMeasured(ctx, spec.ID, index, 21000+index); err != nil {
			t.Fatalf("UpdateRuntimeSpecGPUMeasured(%d): %v", index, err)
		}
	}

	if _, err := svc.SetBenchmarkRuntimeSpecAdminState(ctx, spec.ID, "", "force_stopped"); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, err := svc.SetBenchmarkRuntimeSpecAdminState(ctx, spec.ID, "force_stopped", ""); err != nil {
		t.Fatalf("restore: %v", err)
	}

	gpus, err := routes.RuntimeSpecGPUs(ctx, spec.ID)
	if err != nil {
		t.Fatalf("RuntimeSpecGPUs: %v", err)
	}
	want := []routing.RuntimeSpecGPU{
		{SpecID: spec.ID, GPUIndex: 0, Position: 0, VRAMEstimateMB: 18000, VRAMMeasuredMB: 21000},
		{SpecID: spec.ID, GPUIndex: 1, Position: 1, VRAMEstimateMB: 18000, VRAMMeasuredMB: 21001},
	}
	if !reflect.DeepEqual(gpus, want) {
		t.Fatalf("a full VRAM run's own writes moved a VRAM number:\n got %#v\nwant %#v", gpus, want)
	}
	current, err := svc.GetRuntimeSpec(ctx, ownerToken(), spec.MappingID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if !current.VRAMLocked {
		t.Fatal("vram_locked was cleared by a benchmark write")
	}
}

// TestSetBenchmarkRuntimeSpecAdminStateGatesTheApplicationType keeps the
// application-type gate the full-document writer already owns: a runtime spec
// only means anything on a server_agent application, and a benchmark write is
// not the place to invent an exception.
func TestSetBenchmarkRuntimeSpecAdminStateGatesTheApplicationType(t *testing.T) {
	now := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen", AppModelName: "qwen"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	spec, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{Enabled: true, Binary: "/usr/bin/llama-server"})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	// Retype the application out from under the spec (UpdateApplication has no
	// gate against this, so it is reachable through ordinary API use).
	stored, err := routeStore.ApplicationByID(ctx, app.ID)
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	stored.Type = routing.ProviderVLLM
	if err := routeStore.UpdateApplication(ctx, stored); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}

	if _, err := svc.SetBenchmarkRuntimeSpecAdminState(ctx, spec.ID, "", "force_stopped"); !errors.Is(err, ErrRuntimeSpecNotServerAgent) {
		t.Fatalf("err = %v, want ErrRuntimeSpecNotServerAgent", err)
	}
}
