// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"testing"
	"time"
)

// liveTimingsSpec marks a fixture row as HAVING a runtime spec whose
// responses-live-timings flag is v. nil is a different fixture entirely: no
// spec row at all, which is targetFrom's documented app fallback (resolver.go's
// targetFrom doc) and not the same thing as a spec whose flag is false.
func liveTimingsSpec(v bool) *bool { return &v }

// seedLiveTimingsStore builds a one-server / one-app / one-mapping MemoryStore
// for a single precedence row, optionally with a runtime spec.
//
// appOpportunistic exists to defeat a TRANSPOSITION, not to test
// OpportunisticMetrics: Target.ResponsesLiveTimingsEnabled is inserted directly
// beside Target.OpportunisticMetrics, the two are the same type, and swapping
// their assignments in the Target literal is a silent
// mistake if both destinations ever hold the same value. Every row below
// therefore seeds appOpportunistic to the NEGATION of its expected live-timings
// result, and the assertions pin both fields -- so the two Target bools differ
// in every row and a swap fails every row.
func seedLiveTimingsStore(t *testing.T, now time.Time, appType string, appLiveTimings, appOpportunistic bool, specLiveTimings *bool) *MemoryStore {
	t.Helper()
	ctx := context.Background()
	store := NewMemoryStore()
	must(t, store.CreateAIServer(ctx, AIServer{ID: "srv", Name: "srv", Domain: "srv.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	must(t, store.CreateApplication(ctx, Application{
		ID: "app", ServerID: "srv", Type: appType, Port: 8000, Scheme: "http",
		APIFlavors: []string{APIFlavorOpenAI}, ResponsesMode: EndpointModePassthrough,
		Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800,
		Status:                      ServerStatusActive,
		ResponsesLiveTimingsEnabled: appLiveTimings,
		OpportunisticMetricsEnabled: appOpportunistic,
		CreatedAt:                   now, UpdatedAt: now,
	}))
	must(t, store.CreateMapping(ctx, ModelMapping{ID: "map", ApplicationID: "app", GatewayModelName: "m", AppModelName: "m", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv", ReportedAt: now, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}))
	if specLiveTimings != nil {
		must(t, store.UpsertRuntimeSpec(ctx, RuntimeSpec{
			ID: "spec", MappingID: "map", APIFlavors: []string{APIFlavorOpenAI},
			ResponsesMode:               EndpointModePassthrough,
			ResponsesLiveTimingsEnabled: *specLiveTimings,
			CreatedAt:                   now, UpdatedAt: now,
		}))
	}
	return store
}

// TestTargetResponsesLiveTimingsPrecedence pins that targetFrom resolves
// Target.ResponsesLiveTimingsEnabled with the SAME precedence ResponsesMode and
// MessagesMode already use: the resolved RuntimeSpec's value for a server_agent
// mapping that has a spec, the resolved application's value otherwise.
//
// The two server_agent-with-spec rows are the load-bearing ones, and they are
// deliberately opposed:
//
//   - app false / spec true -> true rules out "the application always wins" and
//     "the field is hard-wired false".
//   - app TRUE / spec FALSE -> false is the only row that distinguishes "the
//     spec's value is PREFERRED" from "the two are OR-ed together". A field
//     seeded unconditionally from app.ResponsesLiveTimingsEnabled -- the
//     app-only shape Target.OpportunisticMetrics uses, which is the mistake
//     most likely to be made here -- passes every other row in this table and
//     fails only this one.
//
// The server_agent row with NO spec pins the documented fallback: an absent
// spec leaves the application's value in place, which is why it expects true
// against an app that says true.
//
// No production code reads this field in part 1 of issue #81; the gate, the
// timings_per_token injection and the retry are part 2. This test is the whole
// of the field's current contract.
func TestTargetResponsesLiveTimingsPrecedence(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		// appType is the parent application's provider type; only
		// ProviderServerAgent makes targetFrom consult a spec at all.
		appType        string
		appLiveTimings bool
		// spec is nil for "no spec row", else the spec's own flag value.
		spec *bool
		want bool
	}{
		{
			// Alone, catches: the application's true not reaching Target on
			// the ordinary (non-server_agent) path -- a hard-wired false, an
			// unassigned field, or a seed that ignores the app.
			name:           "ordinary application, flag on",
			appType:        ProviderLlamaCPP,
			appLiveTimings: true,
			spec:           nil,
			want:           true,
		},
		{
			// Alone, catches: the application's false being reported as true.
			// The ONLY row that catches that on the ordinary path -- a seed of
			// literal true with the spec override left intact passes rows 1,
			// 3, 4 and 5 and fails only here.
			name:           "ordinary application, flag off",
			appType:        ProviderLlamaCPP,
			appLiveTimings: false,
			spec:           nil,
			want:           false,
		},
		{
			// Alone, catches: the spec's value being ignored outright -- the
			// app-only OpportunisticMetrics shape, or a missing
			// `liveTimings = spec.ResponsesLiveTimingsEnabled` override. It
			// CANNOT distinguish "the spec is preferred" from "the two are
			// OR-ed"; that is the next row's job.
			name:           "server_agent, spec overrides app OFF to ON",
			appType:        ProviderServerAgent,
			appLiveTimings: false,
			spec:           liveTimingsSpec(true),
			want:           true,
		},
		{
			// Alone, catches: the spec's value being OR-ed with (rather than
			// preferred over) the application's, as well as the app-only
			// shape. The ONLY row in the table where "the spec wins" and
			// "either one wins" disagree -- `liveTimings = liveTimings ||
			// spec.ResponsesLiveTimingsEnabled` fails here and nowhere else.
			name:           "server_agent, spec overrides app ON to OFF",
			appType:        ProviderServerAgent,
			appLiveTimings: true,
			spec:           liveTimingsSpec(false),
			want:           false,
		},
		{
			// Alone, catches: a no-spec server_agent mapping being forced to
			// the spec zero value (false) instead of falling back to the
			// application. The ONLY row that catches the override escaping
			// `if ok {` -- moved one level out it reads the zero-valued spec
			// and fails here alone.
			name:           "server_agent, no spec at all",
			appType:        ProviderServerAgent,
			appLiveTimings: true,
			spec:           nil,
			want:           true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			// See seedLiveTimingsStore: the neighbouring same-typed Target
			// bool is seeded opposite, so a transposition cannot pass.
			appOpportunistic := !tc.want
			store := seedLiveTimingsStore(t, now, tc.appType, tc.appLiveTimings, appOpportunistic, tc.spec)
			resolver := NewResolver(store, func() time.Time { return now }, nil)
			target, err := resolver.Resolve(ctx, auth.Token{ID: "tok", UserID: "u", Active: true}, inference.Request{Model: "m", APIFlavor: "openai_responses"})
			must(t, err)
			if target.ResponsesLiveTimingsEnabled != tc.want {
				t.Fatalf("target.ResponsesLiveTimingsEnabled = %v, want %v (app=%v spec=%v)",
					target.ResponsesLiveTimingsEnabled, tc.want, tc.appLiveTimings, tc.spec)
			}
			if target.OpportunisticMetrics != appOpportunistic {
				t.Fatalf("target.OpportunisticMetrics = %v, want %v -- the two neighbouring Target bools look transposed",
					target.OpportunisticMetrics, appOpportunistic)
			}
		})
	}
}
