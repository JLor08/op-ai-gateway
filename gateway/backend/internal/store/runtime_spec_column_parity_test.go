// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// runtimeSpecParityRows is how many agent_runtime_specs rows the column-parity
// fixture seeds, computed the same way as applicationParityRows: the readers
// carry SEVEN integer-boolean columns (enabled, pinned, set_visible_devices,
// responses_live_timings_enabled, replicable, cluster_enabled,
// lan_exposure_ack), two same-typed columns are only
// distinguishable by value if they differ in at least one seeded row, so each
// needs its own DISTINCT, non-empty pattern. With r rows there are 2^r - 1
// non-empty patterns, so r must satisfy 2^r - 1 >= 7 -- three rows suffice
// (2^3 - 1 = 7), with no pattern to spare.
const runtimeSpecParityRows = 3

// runtimeSpecParityBools is the bit-pattern table, one row per integer-boolean
// column of agent_runtime_specs in scan order (enabled, pinned,
// set_visible_devices, responses_live_timings_enabled, replicable) and one
// array slot per seeded spec. Its two load-bearing properties -- every row true
// somewhere, and every pair of rows distinct -- are pinned by
// TestRuntimeSpecParityFixtureDistinguishesEverySameTypedPair, so the table
// cannot degrade into an all-true fixture that catches nothing. Before this
// fixture existed, TestRoutingStoreRuntimeSpecs flipped ONLY
// responses_live_timings_enabled and left the others true in every row, so a
// swap among those was invisible.
var runtimeSpecParityBools = [7][runtimeSpecParityRows]bool{
	{true, true, false},  // enabled
	{false, true, true},  // pinned
	{false, false, true}, // set_visible_devices
	{true, false, false}, // responses_live_timings_enabled
	{true, false, true},  // replicable
	{false, true, false}, // cluster_enabled
	{true, true, true},   // lan_exposure_ack
}

// runtimeSpecParityBoolNames names the runtimeSpecParityBools rows in scan
// order -- the ONE list of the agent_runtime_specs integer-boolean columns,
// shared by the distinguishability guard and the schema-coverage guard.
var runtimeSpecParityBoolNames = []string{
	"enabled", "pinned", "set_visible_devices", "responses_live_timings_enabled", "replicable",
	"cluster_enabled", "lan_exposure_ack",
}

// TestConformanceRuntimeSpecReadersAgreeOnEveryColumn is the agent_runtime_specs
// analogue of TestConformanceApplicationReadersAgreeOnEveryColumn, and it exists
// because #84 found agent_runtime_specs had NO column-parity fixture: its
// integer-booleans were guarded only by TestRoutingStoreRuntimeSpecs, which
// flips one of them on one of two rows -- coverage for one column, not a guard
// for the next.
//
// agent_runtime_specs has three SQL readers: RuntimeSpecByMapping and
// RuntimeSpecByID (served by runtimeSpecCols) and RuntimeSpecsByApplication (the
// join, served by the SEPARATELY hand-maintained runtimeSpecColsPrefixed). All
// funnel through the fixed-arity scanRuntimeSpec, so an OMITTED column fails
// loudly at Scan; the silent failures this guards are (a) a column wired to the
// Go struct + memory mirror but NOT into the SQL insert/read (SQL reads back the
// DDL zero -- the field-by-field baseline below catches it, since the row was
// seeded non-zero), and (b) two same-typed columns transposed within either
// column-order list (the wrong value lands in a struct field -- caught by the
// same baseline, and by the two lists having to agree).
//
// Every reader must return the SAME routing.RuntimeSpec for a mapping, and that
// value must carry what was seeded. RuntimeSpecByMapping is the baseline checked
// field-by-field against the seeded spec, so a column missing from BOTH lists
// cannot hide behind readers agreeing on the same zero.
func TestConformanceRuntimeSpecReadersAgreeOnEveryColumn(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_rtcols", Name: "Runtime Column Parity", Domain: "rtcols.example.test",
			Provider: routing.ProviderVLLM, Endpoint: "http://rtcols.example.test:8000",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_rtcols", ServerID: "srv_rtcols", Type: routing.ProviderServerAgent,
			Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI},
			Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800,
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}

		// Distinct-per-row values for the same-typed non-bool columns, so a
		// transposed pair shows as a mismatch and a reader returning the wrong
		// spec is caught too. The three-state enums cycle through their legal
		// non-empty values.
		visibleModes := [runtimeSpecParityRows]routing.VisibleDevicesMode{
			routing.VisibleDevicesModeEnv, routing.VisibleDevicesModeArgs, routing.VisibleDevicesModeEnv,
		}
		responsesModes := [runtimeSpecParityRows]routing.EndpointMode{
			routing.EndpointModeDisabled, routing.EndpointModeTranslate, routing.EndpointModePassthrough,
		}
		messagesModes := [runtimeSpecParityRows]routing.EndpointMode{
			routing.EndpointModeTranslate, routing.EndpointModePassthrough, routing.EndpointModeDisabled,
		}
		// Distinct, all non-empty, so admin_state cannot be confused with the
		// other string columns and an omitted read is caught.
		adminStates := [runtimeSpecParityRows]string{"force_running", "force_stopped", "force_running"}
		types := [runtimeSpecParityRows]string{
			string(routing.RuntimeSpecTypeVLLM), string(routing.RuntimeSpecTypeLlamaCpp), routing.ProviderOllama,
		}
		apiFlavors := [runtimeSpecParityRows][]string{
			{routing.APIFlavorOpenAI},
			{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic},
			{routing.APIFlavorAnthropic},
		}

		// The RPC-cluster string columns carry values distinct per row, none
		// of them the DDL default, so an omitted column cannot read back as
		// the default it would fall back to.
		memberModes := [runtimeSpecParityRows]string{"manual", "automatic", "manual"}
		clusterNetworks := [runtimeSpecParityRows]string{"lan", "mesh", "lan"}
		rpcTransports := [runtimeSpecParityRows]string{"relay", "direct", "relay"}
		deviceBackends := [runtimeSpecParityRows]string{"cuda", "vulkan", "rocm"}

		want := make([]routing.RuntimeSpec, 0, runtimeSpecParityRows)
		for i := 0; i < runtimeSpecParityRows; i++ {
			idx := strconv.Itoa(i)
			if err := s.CreateMapping(ctx, routing.ModelMapping{
				ID: "map_rt_" + idx, ApplicationID: "app_rtcols",
				GatewayModelName: "rt-model-" + idx, AppModelName: "upstream-rt-" + idx,
				Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create mapping %d: %v", i, err)
			}
			want = append(want, routing.RuntimeSpec{
				ID: "rspec_cols_" + idx, MappingID: "map_rt_" + idx,
				Enabled:                     runtimeSpecParityBools[0][i],
				Binary:                      "/opt/bin-" + idx,
				Args:                        `["--arg-` + idx + `"]`,
				Env:                         `{"KEY_` + idx + `":"${AGENT_ENV:V}"}`,
				ConfigFiles:                 `[{"name":"f-` + idx + `.json","content":"c-` + idx + ` ${MODEL}"}]`,
				WorkDir:                     "/work-" + idx,
				ListenPort:                  19000 + i,
				HealthPath:                  "/health-" + idx,
				HealthTimeoutSeconds:        11 + i,
				StartupTimeoutSeconds:       111 + i,
				IdleTimeoutSeconds:          211 + i,
				AdmissionWaitTimeoutSeconds: 31 + i,
				Pinned:                      runtimeSpecParityBools[1][i],
				AdminState:                  adminStates[i],
				SetVisibleDevices:           runtimeSpecParityBools[2][i],
				VisibleDevicesMode:          visibleModes[i],
				APITokenMode:                string(routing.RuntimeAPITokenModeSet),
				APIToken:                    "enc:token-" + idx,
				APITokenHeaderSource:        string(routing.RuntimeAPITokenHeaderSourceCustom),
				APITokenHeader:              "x-tok-" + idx,
				Type:                        types[i],
				MetricsPath:                 "/metrics-" + idx,
				ContextProbePath:            "/ctx-" + idx,
				RAMEstimateMB:               4096 + i,
				RAMMeasuredMB:               6000 + i,
				Replicable:                  runtimeSpecParityBools[4][i],
				MinInstances:                1 + i,
				MaxInstances:                8 + i,
				InstanceParallelism:         4 + i,
				StartupMeasuredMS:           45000 + int64(i),
				ClusterEnabled:              runtimeSpecParityBools[5][i],
				MemberMode:                  memberModes[i],
				ClusterNetwork:              clusterNetworks[i],
				RPCTransport:                rpcTransports[i],
				LANExposureAck:              runtimeSpecParityBools[6][i],
				DeviceBackend:               deviceBackends[i],
				APIFlavors:                  apiFlavors[i],
				ResponsesMode:               responsesModes[i],
				MessagesMode:                messagesModes[i],
				ResponsesLiveTimingsEnabled: runtimeSpecParityBools[3][i],
				CreatedAt:                   now.Add(time.Duration(-10-i) * time.Minute),
				UpdatedAt:                   now.Add(time.Duration(-i) * time.Minute),
			})
			if err := s.UpsertRuntimeSpec(ctx, want[i]); err != nil {
				t.Fatalf("upsert runtime spec %d: %v", i, err)
			}
		}

		byApp, err := s.RuntimeSpecsByApplication(ctx, "app_rtcols")
		if err != nil {
			t.Fatalf("RuntimeSpecsByApplication: %v", err)
		}
		if len(byApp) != runtimeSpecParityRows {
			t.Fatalf("RuntimeSpecsByApplication returned %d specs, want %d", len(byApp), runtimeSpecParityRows)
		}

		for _, expected := range want {
			byMapping, ok, err := s.RuntimeSpecByMapping(ctx, expected.MappingID)
			if err != nil {
				t.Fatalf("RuntimeSpecByMapping(%s): %v", expected.MappingID, err)
			}
			if !ok {
				t.Fatalf("RuntimeSpecByMapping(%s): not found", expected.MappingID)
			}
			// The field-by-field baseline: a column missing from BOTH column
			// lists (a genuinely unwired column) reads back its zero here and
			// mismatches the non-zero seeded value.
			if !reflect.DeepEqual(normalizeRuntimeSpecForCompare(byMapping), normalizeRuntimeSpecForCompare(expected)) {
				t.Fatalf("RuntimeSpecByMapping(%s) lost or reordered a column:\n got  %+v\n want %+v", expected.MappingID, byMapping, expected)
			}

			byID, ok, err := s.RuntimeSpecByID(ctx, expected.ID)
			if err != nil {
				t.Fatalf("RuntimeSpecByID(%s): %v", expected.ID, err)
			}
			if !ok {
				t.Fatalf("RuntimeSpecByID(%s): not found", expected.ID)
			}
			if !reflect.DeepEqual(normalizeRuntimeSpecForCompare(byID), normalizeRuntimeSpecForCompare(byMapping)) {
				t.Fatalf("RuntimeSpecByID disagrees with RuntimeSpecByMapping on %s:\n got  %+v\n want %+v", expected.ID, byID, byMapping)
			}

			fromApp, found := findRuntimeSpecByID(byApp, expected.ID)
			if !found {
				t.Fatalf("RuntimeSpecsByApplication did not return %s at all", expected.ID)
			}
			if !reflect.DeepEqual(normalizeRuntimeSpecForCompare(fromApp), normalizeRuntimeSpecForCompare(byMapping)) {
				t.Fatalf("RuntimeSpecsByApplication (runtimeSpecColsPrefixed) disagrees with RuntimeSpecByMapping on %s:\n got  %+v\n want %+v", expected.ID, fromApp, byMapping)
			}
		}
	})
}

// TestRuntimeSpecParityFixtureDistinguishesEverySameTypedPair pins the two
// properties runtimeSpecParityBools must have, exactly as its applications and
// ai_servers siblings do: without it, a "simplification" back to an all-true
// table would restore the very hole -- swaps among the always-true bools
// were invisible -- that this fixture was added to close.
func TestRuntimeSpecParityFixtureDistinguishesEverySameTypedPair(t *testing.T) {
	names := runtimeSpecParityBoolNames
	if len(names) != len(runtimeSpecParityBools) {
		t.Fatalf("names has %d entries for %d bit-pattern rows: widen both together", len(names), len(runtimeSpecParityBools))
	}
	for i := range runtimeSpecParityBools {
		trueSomewhere := false
		for _, v := range runtimeSpecParityBools[i] {
			if v {
				trueSomewhere = true
			}
		}
		if !trueSomewhere {
			t.Errorf("%s is false in every seeded row: an omitted column would read back as the same false", names[i])
		}
		for j := i + 1; j < len(runtimeSpecParityBools); j++ {
			if runtimeSpecParityBools[i] == runtimeSpecParityBools[j] {
				t.Errorf("%s and %s carry the same pattern %v: swapping them in a select list is invisible",
					names[i], names[j], runtimeSpecParityBools[i])
			}
		}
	}
}

// TestAgentRuntimeSpecsSchemaColumnsAllCovered ties the agent_runtime_specs
// parity fixture above to the LIVE migrated schema (issue #84), so a column
// added by a future migration cannot reach the readers and still be absent from
// every fixture row. See assertColumnCoverage.
func TestAgentRuntimeSpecsSchemaColumnsAllCovered(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		assertColumnCoverage(context.Background(), t, s, "agent_runtime_specs", columnCoverage{
			bools: runtimeSpecParityBoolNames,
			seeded: []string{
				"admin_state", "admission_wait_timeout_seconds", "api_flavors", "api_token",
				"api_token_header", "api_token_header_source", "api_token_mode", "args", "binary_path",
				"cluster_network", "config_files", "context_probe_path", "created_at", "env", "health_path", "health_timeout_seconds",
				"device_backend", "id", "idle_timeout_seconds", "instance_parallelism", "listen_port", "mapping_id",
				"max_instances", "member_mode", "messages_mode", "metrics_path", "min_instances", "ram_estimate_mb",
				"ram_measured_mb", "responses_mode", "rpc_transport", "startup_measured_ms", "startup_timeout_seconds",
				"type", "updated_at", "visible_devices_mode", "work_dir",
			},
			ignored: map[string]string{},
		})
	})
}

// findRuntimeSpecByID returns the spec with the given id from a list reader's
// result, matching by id rather than position so a reordering change fails the
// ordering tests that exist for it rather than this one.
func findRuntimeSpecByID(specs []routing.RuntimeSpec, id string) (routing.RuntimeSpec, bool) {
	for _, spec := range specs {
		if spec.ID == id {
			return spec, true
		}
	}
	return routing.RuntimeSpec{}, false
}

// normalizeRuntimeSpecForCompare makes two routing.RuntimeSpec values comparable
// with reflect.DeepEqual across the dialects: postgres returns timestamptz in a
// different *time.Location than the sqlite driver, so every time is compared as a
// UTC wall-clock value. Nothing else is touched -- in particular no field is
// zeroed, which would be the way to accidentally exclude a column from the
// comparison this test exists to make.
func normalizeRuntimeSpecForCompare(in routing.RuntimeSpec) routing.RuntimeSpec {
	out := in
	out.CreatedAt = in.CreatedAt.UTC()
	out.UpdatedAt = in.UpdatedAt.UTC()
	return out
}
