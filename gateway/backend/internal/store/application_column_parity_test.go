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

// applicationParityRows is how many applications rows the column-parity
// fixture seeds, computed the same way (and for the same reason) as
// aiServerParityRows in ai_server_column_parity_test.go: the applications
// select lists carry FOUR integer-boolean columns, two same-typed columns are
// only distinguishable by value if they differ in at least one seeded row, so
// each needs its own DISTINCT pattern across the rows. With r rows there are
// 2^r patterns, so r must satisfy 2^r >= 4 — three rows (native_responses /
// native_messages were replaced by the ResponsesMode / MessagesMode text
// columns below, which the string fixture already varies per row).
const applicationParityRows = 3

// applicationParityBools is the bit-pattern table, one row per integer-boolean
// column in SELECT-LIST order (always_reachable, benchmark_schedule_enabled,
// opportunistic_metrics_enabled, proxy_excluded) and one column per seeded
// application.
//
// Its two load-bearing properties are asserted by
// TestApplicationParityFixtureDistinguishesEverySameTypedPair below, so the
// table cannot silently degrade into an all-true fixture that catches nothing:
//
//   - every PAIR of rows differs in at least one column, so swapping any two of
//     the four bool columns in one reader's select list changes an observable
//     value in at least one seeded application;
//   - every row is true in at least one column, so a column dropped from a
//     reader's select list (coming back as the false zero value) still shows up
//     as a mismatch somewhere.
var applicationParityBools = [4][applicationParityRows]bool{
	{true, true, false},  // always_reachable
	{false, true, true},  // benchmark_schedule_enabled
	{false, true, false}, // opportunistic_metrics_enabled
	{false, false, true}, // proxy_excluded
}

// TestConformanceApplicationReadersAgreeOnEveryColumn closes a gap that
// PREDATES the proxy_excluded column: the applications column list is
// hand-maintained in THREE separate queries feeding TWO different scan
// functions — ApplicationByID and ApplicationsByServer (scanApplication) plus
// ActiveMappingsForModel (scanMappingCandidate, the ROUTING path) — and until
// now nothing round-tripped any of the additive columns through all three.
// Verified before this test existed: no *_test.go in this package so much as
// mentioned proxy_listen_port, so migration 59's column had never been read
// back through SQL by any suite at all.
//
// The gap matters because the two failure modes are not equally loud. An
// OMITTED column fails immediately and unmistakably — both scan functions have
// a fixed destination count, so database/sql reports "expected N destination
// arguments in Scan". A REORDERED or wrong-column select list does NOT: two
// same-typed columns swapped in one of the three lists silently returns the
// wrong values from that one reader, and the other two keep agreeing with each
// other. And because every portal test runs on routing.NewMemoryStore(), a
// mistake confined to ActiveMappingsForModel is invisible to the entire portal
// suite while being the single query that decides where live traffic goes.
//
// So rather than repeat a field-by-field round-trip per reader, this asserts
// something stronger and shorter, exactly as its ai_servers sibling does: every
// reader must return the SAME routing.Application value for the same row, and
// that value must carry what was seeded into each field.
//
// WHAT THE FIXTURE GUARANTEES, precisely: fields that CAN hold a distinct value
// per row (strings, ints) do, and each varies per row as well, so a reader
// returning the wrong ROW is caught too. The six integer-booleans carry the
// bit-pattern table above, sized so every pair of them differs in at least one
// seeded application. Together that makes a swapped pair in any one list
// observable wherever the two columns are same-typed — the only case that does
// not already fail loudly at Scan.
func TestConformanceApplicationReadersAgreeOnEveryColumn(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_appcols", Name: "App Column Parity", Domain: "appcols.example.test",
			Provider: routing.ProviderVLLM, Endpoint: "http://appcols.example.test:8000",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}

		// Status is NOT varied: ActiveMappingsForModel filters on
		// a.status = active (and on the mapping's status, and on the api
		// flavor), so a non-active row could not appear in the third reader at
		// all and the comparison would have nothing to make. Every other
		// same-typed column is varied instead.
		types := [applicationParityRows]string{routing.ProviderVLLM, routing.ProviderOllama, routing.ProviderLlamaCPP}
		schemes := [applicationParityRows]string{"http", "http", "https"}
		// proxy_listen_port respects the portal's excluded => port 0 invariant
		// (proxy_excluded is true only in the third row), and still carries two
		// distinct non-zero values so it cannot be confused with the other
		// integer columns, which are all distinct per row as well.
		proxyPorts := [applicationParityRows]int{8601, 8602, 0}
		// responsesModes/messagesModes vary per row and differ from each other
		// (like the retired native_responses/native_messages bools they
		// replaced), so a swapped select-list pair between the two columns
		// remains observable via the DeepEqual comparison.
		responsesModes := [applicationParityRows]routing.EndpointMode{
			routing.EndpointModeDisabled, routing.EndpointModeTranslate, routing.EndpointModePassthrough,
		}
		messagesModes := [applicationParityRows]routing.EndpointMode{
			routing.EndpointModeTranslate, routing.EndpointModePassthrough, routing.EndpointModeDisabled,
		}

		want := make([]routing.Application, 0, applicationParityRows)
		for i := 0; i < applicationParityRows; i++ {
			idx := strconv.Itoa(i)
			want = append(want, routing.Application{
				ID: "app_cols_" + idx, ServerID: "srv_appcols", Type: types[i],
				Port: 9100 + i, Scheme: schemes[i],
				APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic},
				Priority:   11 + i, Weight: 21 + i, TimeoutMS: 31000 + i,
				AffinityTTLSeconds: 411 + i, AdmissionQueueTimeoutSeconds: 51 + i,
				Status:          routing.ServerStatusActive,
				AlwaysReachable: applicationParityBools[0][i],
				HealthCheckPath: "/health-" + idx,
				// health_check_mode is a free string at the store layer; a
				// distinct value per row keeps it from being confused with the
				// other text columns.
				HealthCheckMode:                  "mode-" + idx,
				HealthCheckIntervalSeconds:       61 + i,
				ResponsesMode:                    responsesModes[i],
				MessagesMode:                     messagesModes[i],
				LoadedModelsPath:                 "/loaded-" + idx,
				LoadedModelsFormat:               "format-" + idx,
				ContextProbePath:                 "/context-" + idx,
				CapacityProbePath:                "/capacity-" + idx,
				AppPathSuffix:                    "/suffix-" + idx,
				APIToken:                         "plain:token-" + idx,
				APITokenHeader:                   "x-token-" + idx,
				BenchmarkScheduleEnabled:         applicationParityBools[1][i],
				BenchmarkScheduleIntervalSeconds: 71 + i,
				OpportunisticMetricsEnabled:      applicationParityBools[2][i],
				ProxyListenPort:                  proxyPorts[i],
				ProxyExcluded:                    applicationParityBools[3][i],
				CreatedAt:                        now.Add(time.Duration(-10-i) * time.Minute),
				UpdatedAt:                        now.Add(time.Duration(-i) * time.Minute),
			})
		}

		for _, app := range want {
			if err := s.CreateApplication(ctx, app); err != nil {
				t.Fatalf("create application %s: %v", app.ID, err)
			}
			// One active mapping per application, all under the SAME gateway
			// model name, so ActiveMappingsForModel returns all three rows and
			// must match the right values to the right application.
			if err := s.CreateMapping(ctx, routing.ModelMapping{
				ID: "map_cols_" + app.ID, ApplicationID: app.ID,
				GatewayModelName: "parity-model", AppModelName: "upstream-" + app.ID,
				Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create mapping for %s: %v", app.ID, err)
			}
		}

		byServer, err := s.ApplicationsByServer(ctx, "srv_appcols")
		if err != nil {
			t.Fatalf("ApplicationsByServer: %v", err)
		}
		candidates, err := s.ActiveMappingsForModel(ctx, "parity-model", routing.APIFlavorOpenAI)
		if err != nil {
			t.Fatalf("ActiveMappingsForModel: %v", err)
		}
		joined := make([]routing.Application, 0, len(candidates))
		for _, c := range candidates {
			joined = append(joined, c.Application)
		}
		readers := []struct {
			name string
			got  []routing.Application
		}{
			{"ApplicationsByServer", byServer},
			{"ActiveMappingsForModel", joined},
		}
		for _, reader := range readers {
			if len(reader.got) != applicationParityRows {
				t.Fatalf("%s returned %d applications, want %d", reader.name, len(reader.got), applicationParityRows)
			}
		}

		for _, expected := range want {
			byID, err := s.ApplicationByID(ctx, expected.ID)
			if err != nil {
				t.Fatalf("ApplicationByID(%s): %v", expected.ID, err)
			}
			// The baseline the other readers are compared against is itself
			// checked field-by-field against the seeded value, so a column
			// missing from ALL THREE lists (a genuinely forgotten additive
			// column) cannot hide behind three readers agreeing on the same
			// zero.
			if !reflect.DeepEqual(normalizeApplicationForCompare(byID), normalizeApplicationForCompare(expected)) {
				t.Fatalf("ApplicationByID(%s) lost or reordered a column:\n got  %+v\n want %+v", expected.ID, byID, expected)
			}
			for _, reader := range readers {
				got, ok := findApplicationByID(reader.got, expected.ID)
				if !ok {
					t.Fatalf("%s did not return %s at all", reader.name, expected.ID)
				}
				if !reflect.DeepEqual(normalizeApplicationForCompare(got), normalizeApplicationForCompare(byID)) {
					t.Fatalf("%s disagrees with ApplicationByID on at least one column of %s:\n got  %+v\n want %+v",
						reader.name, expected.ID, got, byID)
				}
			}
		}
	})
}

// TestApplicationParityFixtureDistinguishesEverySameTypedPair pins the two
// properties applicationParityBools must have for the fixture to be able to
// catch a swapped pair at all. Without it, someone "simplifying" the table back
// to all-true would silently restore the exact hole the fixture exists to
// close, and every test would stay green — which, per its ai_servers sibling's
// own history, is how that hole got there the first time.
func TestApplicationParityFixtureDistinguishesEverySameTypedPair(t *testing.T) {
	names := []string{
		"always_reachable",
		"benchmark_schedule_enabled", "opportunistic_metrics_enabled", "proxy_excluded",
	}
	for i := range applicationParityBools {
		trueSomewhere := false
		for _, v := range applicationParityBools[i] {
			if v {
				trueSomewhere = true
			}
		}
		if !trueSomewhere {
			t.Errorf("%s is false in every seeded row: an omitted column would read back as the same false", names[i])
		}
		for j := i + 1; j < len(applicationParityBools); j++ {
			if applicationParityBools[i] == applicationParityBools[j] {
				t.Errorf("%s and %s carry the same pattern %v: swapping them in a select list is invisible",
					names[i], names[j], applicationParityBools[i])
			}
		}
	}
}

// findApplicationByID returns the application with the given id from a list
// reader's result. Matching by id rather than by position keeps the comparison
// independent of each reader's ORDER BY, so a reordering change fails the
// ordering tests that exist for it rather than this one.
func findApplicationByID(apps []routing.Application, id string) (routing.Application, bool) {
	for _, app := range apps {
		if app.ID == id {
			return app, true
		}
	}
	return routing.Application{}, false
}

// normalizeApplicationForCompare makes two routing.Application values
// comparable with reflect.DeepEqual across the dialects: postgres returns
// timestamptz in a different *time.Location than the sqlite driver, so every
// time is compared as a UTC wall-clock value. Nothing else is touched — in
// particular no field is zeroed, which would be the way to accidentally exclude
// a column from the comparison this test exists to make.
func normalizeApplicationForCompare(in routing.Application) routing.Application {
	out := in
	out.CreatedAt = in.CreatedAt.UTC()
	out.UpdatedAt = in.UpdatedAt.UTC()
	return out
}
