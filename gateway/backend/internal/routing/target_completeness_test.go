// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// targetFromMayLeaveZero names Target fields that targetFrom is allowed to
// leave at their zero value even for a FULLY-populated, fully-resolved
// server_agent candidate -- each with a reason. It is deliberately EMPTY:
// targetFrom is the canonical builder and, given a candidate whose every
// source is non-zero (a server_agent app with a resolved spec, a token, both
// endpoint modes, opportunistic metrics on, a capability verdict, and a
// non-empty apiFlavor argument), it populates all 17 Target fields. If a
// future field is genuinely unset by targetFrom on a real resolved target,
// add it here WITH a reason rather than deleting the assertion.
var targetFromMayLeaveZero = map[string]bool{}

// TestTargetFromPopulatesEveryField is the routing.Target analogue of
// TestPutRequestFromDTOCoversEveryWritableField (internal/portal): it exists
// because Target is assembled by hand at several sites and a Go struct literal
// has no "...rest" spread, so a field added to Target and wired at only one
// site compiles, passes, and silently carries a zero value everywhere else.
// ResponsesLiveTimingsEnabled (#81) and the live-progress inputs before it each
// had to be threaded into targetFrom by hand; the only thing that stood between
// a missed site and a silent wrong-default was that someone remembered.
//
// This guard removes the remembering for the canonical builder. It resolves a
// Target through targetFrom from a candidate whose every field-source is a
// distinct non-zero value, then reflects over the result and fails on any field
// left at its zero value. Because the walk is reflective it AUTO-EXTENDS: adding
// a field to Target makes this fail until targetFrom populates it (or it is
// named in targetFromMayLeaveZero with a reason) -- and the failure message
// points at the OTHER construction sites the same new field must be taught,
// which no compiler will flag: benchmarkTargetReq
// (internal/gateway/benchmark_runner.go, itself guarded by
// TestBenchmarkTargetReqSetsEveryFieldOrDocumentsOmission) and the deliberately
// partial probe targets (the pt targets in benchmark_runner.go and the
// model-discovery/sync target in internal/portal/service_applications.go).
func TestTargetFromPopulatesEveryField(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

	// A server_agent candidate is used deliberately: LiveProgressSpecType is set
	// only on the server_agent branch of targetFrom, so any other app type would
	// leave it zero for a legitimate reason and defeat the "every field non-zero"
	// assertion. Every source below is a distinct non-zero value.
	store := NewMemoryStore()
	must(t, store.CreateAIServer(ctx, AIServer{ID: "srv", Domain: "srv.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	must(t, store.CreateApplication(ctx, Application{
		ID: "app", ServerID: "srv", Type: ProviderServerAgent, Port: 8000, Scheme: "http",
		APIFlavors: []string{APIFlavorOpenAI}, TimeoutMS: 30000, APITokenHeader: "Authorization",
		// OpportunisticMetrics is read from the APP even for a server_agent child
		// (the spec never overrides it), so it must be set on the app here.
		OpportunisticMetricsEnabled: true,
		Priority:                    10, Weight: 50, AffinityTTLSeconds: 1800,
		Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}))
	must(t, store.CreateMapping(ctx, ModelMapping{ID: "map", ApplicationID: "app", GatewayModelName: "gw-model", AppModelName: "app-model", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	// The resolved spec supplies the fields targetFrom reads from the spec for a
	// server_agent mapping: APIFlavors/ResponsesMode/MessagesMode, the live-timings
	// opt-in, and the sealed token + custom header (mode "set", source "custom").
	// A named Type keeps EffectiveRuntimeSpecType (LiveProgressSpecType) explicit.
	must(t, store.UpsertRuntimeSpec(ctx, RuntimeSpec{
		ID: "spec", MappingID: "map", Type: string(RuntimeSpecTypeVLLM),
		APIFlavors:                  []string{APIFlavorOpenAI},
		ResponsesMode:               EndpointModePassthrough,
		MessagesMode:                EndpointModeTranslate,
		ResponsesLiveTimingsEnabled: true,
		APITokenMode:                string(RuntimeAPITokenModeSet),
		APIToken:                    "enc:spec",
		APITokenHeaderSource:        string(RuntimeAPITokenHeaderSourceCustom),
		APITokenHeader:              "X-Api-Key",
		CreatedAt:                   now, UpdatedAt: now,
	}))

	candidate := MappingCandidate{
		Server:      AIServer{ID: "srv", Domain: "srv.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now},
		Application: Application{ID: "app", ServerID: "srv", Type: ProviderServerAgent, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, TimeoutMS: 30000, APITokenHeader: "Authorization", OpportunisticMetricsEnabled: true, Priority: 10, Weight: 50, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now},
		Mapping:     ModelMapping{ID: "map", ApplicationID: "app", GatewayModelName: "gw-model", AppModelName: "app-model", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now},
		// LiveProgressSupport is the candidate-carried capability verdict; a live
		// resolve fills it from the joined row / a keyed read.
		LiveProgressSupport: "supported",
	}

	resolver := NewResolver(store, func() time.Time { return now }, nil)
	// apiFlavor is a targetFrom ARGUMENT, not a candidate field -- it must be
	// non-empty here or Target.APIFlavor would be zero for a reason unrelated to
	// the builder forgetting it.
	target, err := resolver.targetFrom(ctx, candidate, "openai_chat_completions")
	must(t, err)

	tv := reflect.ValueOf(target)
	tt := tv.Type()
	for i := 0; i < tt.NumField(); i++ {
		name := tt.Field(i).Name
		if targetFromMayLeaveZero[name] {
			continue
		}
		if tv.Field(i).IsZero() {
			t.Fatalf("targetFrom left Target.%s at its zero value for a fully-populated server_agent candidate.\n"+
				"A field added to Target must be wired into targetFrom (internal/routing/resolver.go, the Target{...} literal). Then, because no compiler flags a hand-assembled struct, teach the SAME field to every OTHER construction site that should carry it:\n"+
				"  - benchmarkTargetReq (internal/gateway/benchmark_runner.go) -- guarded by TestBenchmarkTargetReqSetsEveryFieldOrDocumentsOmission;\n"+
				"  - the deliberately partial probe targets (the pt targets in benchmark_runner.go and the model-discovery/sync target in internal/portal/service_applications.go).\n"+
				"If this field is set by targetFrom but from a source this test does not populate, set that source in the fixture above. If targetFrom legitimately leaves it zero on a real resolved target, add %q to targetFromMayLeaveZero with a reason.", name, name)
		}
	}
}
