// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

// The tests in this file are the ONLY ones that carry the opted-IN direction of
// the Responses live-timings switch (issue #81 part 2) through proxyNative and
// look at the body the upstream was handed. TestWantsResponsesLiveTimings has
// opted-in rows of its own, but it builds a Target and calls the predicate
// directly, so there is no relayed body there to inspect and no call site under
// test. That matters more than it sounds: the two standing "the relayed body
// must not grow a timings_per_token flag" assertions in
// passthrough_progress_test.go hold for TWO independent reasons, either of
// which alone would be enough -- newNativeModeTestServerOn seeds no
// live-timings flag and no runtime spec, so
// its resolved target has the flag false AND an effective kind of "custom" (its
// application Type is server_agent, and an absent spec detects to "custom").
// The flavor, the stream and the veto conditions all PASS for those fixtures,
// so none of the three is a reason. A gated injection is therefore invisible to
// the entire pre-existing suite, and a positive path could ship completely
// untested with every test green. Those two assertions are kept, re-scoped to
// "flag off => no injection"; these are the other half.
//
// The body the provider fake was HANDED is where most of this file looks, and
// it is the only place the relayed request is observable at all: the payload
// capture deliberately keeps recording the CLIENT's bytes (part 2's D7), so the
// capture record cannot be used to see what was sent. Two tests below look
// somewhere else on purpose -- one reads the injection off the per-request
// debug line, the other asserts the capture really is still the client's body,
// which is the premise the first sentence rests on.

// liveTimingsSeed describes one seeded deployment. Every field is named at each
// call site rather than relying on the zero value, because the whole hazard this
// file exists for is a fixture that LOOKS opted in and resolves to a target that
// is not.
type liveTimingsSeed struct {
	// appType is the application row's kind (routing.Provider*).
	appType string
	// appLiveTimings is the APPLICATION row's own opt-in. For a server_agent
	// application it is deliberately not what the request path reads.
	appLiveTimings bool
	// spec, when non-nil, is upserted against the mapping. For a server_agent
	// application Resolver.targetFrom then reads the endpoint modes, the API
	// flavors AND the live-timings flag off it, and derives the effective
	// upstream kind from its Type.
	spec *routing.RuntimeSpec
	// liveProgressVerdict, when non-empty, is written as the mapping's
	// "live_progress" capability row (routing.CapabilityYes / CapabilityNo).
	// routing.LiveProgressSupportFromVerdict translates it into the Target's own
	// "" / "supported" / "unsupported" vocabulary on the way through, which is
	// why the seed speaks the ROW's vocabulary and the gate speaks the TARGET's.
	liveProgressVerdict string
}

// newLiveTimingsTestServer seeds one server + application + mapping (gateway
// model "gw-model" -> upstream "upstream-model"), optionally a runtime spec and
// a live_progress capability row, and returns a Server wired to prov.
//
// Both API flavors and both endpoint modes are on passthrough so that ONE seed
// serves the /v1/responses positive and the /v1/messages negative: the gate, not
// the route, has to be what refuses the Anthropic endpoint. A fixture that
// 404'd instead would make that negative pass for the wrong reason, which is
// what postPassthrough's proxyCalls check below exists to catch.
func newLiveTimingsTestServer(t *testing.T, prov provider.Client, seed liveTimingsSeed) *Server {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	ctx := context.Background()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-lt", Name: "Live Timings Upstream", Domain: "lt.example.test", Provider: routing.ProviderLlamaCPP, Endpoint: "http://lt.example.test:8000", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-lt", ServerID: "srv-lt", Type: seed.appType, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, ResponsesMode: routing.EndpointModePassthrough, MessagesMode: routing.EndpointModePassthrough, ResponsesLiveTimingsEnabled: seed.appLiveTimings, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-lt", ApplicationID: "app-lt", GatewayModelName: "gw-model", AppModelName: "upstream-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if seed.spec != nil {
		spec := *seed.spec
		spec.ID, spec.MappingID, spec.CreatedAt, spec.UpdatedAt = "spec-lt", "route-lt", now, now
		if err := routeStore.UpsertRuntimeSpec(ctx, spec); err != nil {
			t.Fatalf("UpsertRuntimeSpec: %v", err)
		}
	}
	if seed.liveProgressVerdict != "" {
		if err := routeStore.UpsertMappingCapabilities(ctx, "route-lt", []routing.CapabilityRow{{Capability: routing.CapabilityLiveProgress, Verdict: seed.liveProgressVerdict, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now}}); err != nil {
			t.Fatalf("UpsertMappingCapabilities: %v", err)
		}
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-lt", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: prov,
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

// llamaCppOptedIn is the ORDINARY-application positive: a plain llama_cpp
// application whose own row carries the opt-in and which has no runtime spec at
// all, so targetFrom never enters its server_agent branch and the application's
// own value is what reaches the Target.
func llamaCppOptedIn() liveTimingsSeed {
	return liveTimingsSeed{appType: routing.ProviderLlamaCPP, appLiveTimings: true}
}

// serverAgentSpecOptedIn is the second positive, and the design insists on it
// separately because BOTH halves of the spec are load-bearing and each fails
// silently on its own:
//
//   - Without spec.ResponsesLiveTimingsEnabled, targetFrom's spec-over-
//     application precedence resolves the flag back to false the moment a spec
//     row exists at all -- a spec is not a partial override.
//   - Without spec.Type, the effective kind is detected from the spec's empty
//     Binary and comes out "custom", because Target.Provider for this
//     application is the literal "server_agent" and says nothing about what
//     actually serves.
//
// appLiveTimings is false ON PURPOSE: with the application row saying no and the
// spec row saying yes, an injection that happens proves the request path read
// the SPEC.
func serverAgentSpecOptedIn() liveTimingsSeed {
	return liveTimingsSeed{
		appType:        routing.ProviderServerAgent,
		appLiveTimings: false,
		spec: &routing.RuntimeSpec{
			Enabled:                     true,
			Type:                        string(routing.RuntimeSpecTypeLlamaCpp),
			APIFlavors:                  []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic},
			ResponsesMode:               routing.EndpointModePassthrough,
			MessagesMode:                routing.EndpointModePassthrough,
			ResponsesLiveTimingsEnabled: true,
		},
	}
}

// terminalOnlyResponsesStream is a one-frame upstream body: enough for the
// passthrough copy + usage scan to complete normally. These tests are about the
// REQUEST, so the response deliberately carries nothing interesting.
const terminalOnlyResponsesStream = "event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"resp_x","usage":{"input_tokens":3,"output_tokens":7,"total_tokens":10}}}` + "\n\n"

// liveTimingsStreamBody is the plain streaming Responses request every positive
// uses: a client that set no timings_per_token of its own.
const liveTimingsStreamBody = `{"model":"gw-model","stream":true,"input":"hi"}`

// postPassthrough drives one request through the whole server and returns the
// body the provider fake was handed.
//
// The proxyCalls check is not decoration. Three of the four negatives assert the
// ABSENCE of a key, and a fixture that failed to route -- a 404 from a disabled
// endpoint, a resolve failure -- would satisfy that assertion with an empty
// body. Requiring exactly one ProxyNative call makes every negative prove the
// request actually reached proxyNative and the GATE is what refused.
func postPassthrough(t *testing.T, srv *Server, prov *recordingProxyProvider, path, body string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s: status = %d, body = %s", path, rec.Code, rec.Body.String())
	}
	if prov.proxyCalls != 1 {
		t.Fatalf("POST %s: ProxyNative calls = %d, want exactly 1 (the request must have reached the native passthrough path, or an absence assertion below proves nothing)", path, prov.proxyCalls)
	}
	return string(prov.gotBody)
}

// TestPassthroughResponsesInjectsTimingsForAnOptedInLlamaCppApplication is the
// ordinary case the feature exists for: an operator switched the per-application
// opt-in on for a llama.cpp upstream, and a streaming /v1/responses request
// therefore reaches that upstream carrying timings_per_token.
func TestPassthroughResponsesInjectsTimingsForAnOptedInLlamaCppApplication(t *testing.T) {
	prov := &recordingProxyProvider{respBody: terminalOnlyResponsesStream}
	srv := newLiveTimingsTestServer(t, prov, llamaCppOptedIn())

	got := postPassthrough(t, srv, prov, "/v1/responses", liveTimingsStreamBody)

	if !strings.Contains(got, `"timings_per_token":true`) {
		t.Fatalf("relayed body carries no timings_per_token flag: %s", got)
	}
	// The two body edits COMPOSE, in that order: the injection runs over
	// rewriteModelField's OUTPUT. A wiring that built the outgoing body from the
	// client's raw bytes instead of chaining loses the mapped model name here,
	// loudly, rather than sending the gateway-side name upstream in silence.
	if !strings.Contains(got, `"model":"upstream-model"`) {
		t.Fatalf("relayed body lost the mapped provider model: %s", got)
	}
	if !strings.Contains(got, `"input":"hi"`) {
		t.Fatalf("relayed body lost a field the client sent: %s", got)
	}
}

// TestPassthroughResponsesInjectsTimingsForAnOptedInServerAgentSpec is the same
// request against a server_agent application whose OWN row has the opt-in off
// and whose runtime spec has it on, with an explicit llama_cpp spec type. It
// fails for a gate that asks Target.Provider about the kind (that field is the
// literal "server_agent") and for a resolution that reads the application's flag
// instead of the spec's.
func TestPassthroughResponsesInjectsTimingsForAnOptedInServerAgentSpec(t *testing.T) {
	prov := &recordingProxyProvider{respBody: terminalOnlyResponsesStream}
	srv := newLiveTimingsTestServer(t, prov, serverAgentSpecOptedIn())

	got := postPassthrough(t, srv, prov, "/v1/responses", liveTimingsStreamBody)

	if !strings.Contains(got, `"timings_per_token":true`) {
		t.Fatalf("relayed body carries no timings_per_token flag: %s", got)
	}
	if !strings.Contains(got, `"model":"upstream-model"`) {
		t.Fatalf("relayed body lost the mapped provider model: %s", got)
	}
}

// TestPassthroughResponsesDoesNotInjectTimingsWhenTheGateRefuses walks the four
// refusals that are invisible everywhere else in the suite. Each runs on a seed
// whose opt-in IS on, so "no injection" here is the gate's doing and not the
// fixture's -- which is exactly what separates these from the two standing
// assertions in passthrough_progress_test.go, whose fixture has the opt-in off.
//
// Two of the four are about the CALL SITE rather than the predicate, and are the
// only tests in the repository that can catch a wrong argument there: the
// predicate takes the FINE api flavor and the stream flag as parameters, and a
// proxyNative that passed a hardcoded "openai_responses", or the literal true
// for stream, leaves TestWantsResponsesLiveTimings entirely green.
func TestPassthroughResponsesDoesNotInjectTimingsWhenTheGateRefuses(t *testing.T) {
	recordedRejection := llamaCppOptedIn()
	recordedRejection.liveProgressVerdict = routing.CapabilityNo

	for _, tc := range []struct {
		name string
		seed liveTimingsSeed
		path string
		body string
		// wantRelayedFlag is what the relayed body must say about the key:
		// "" means the key must not appear AT ALL; a non-empty value is the
		// exact JSON fragment that must appear. The client-false row spells the
		// VALUE out on purpose -- asserting mere presence would pass against an
		// injection that overwrote the client's false with true, which is the
		// one way this rule regresses silently.
		wantRelayedFlag string
	}{
		{
			name: "the Anthropic Messages endpoint",
			seed: llamaCppOptedIn(),
			path: "/v1/messages",
			body: `{"model":"gw-model","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "a non-streaming /v1/responses request",
			seed: llamaCppOptedIn(),
			path: "/v1/responses",
			body: `{"model":"gw-model","input":"hi"}`,
		},
		{
			name: "a recorded live-progress rejection vetoes the opt-in",
			seed: recordedRejection,
			path: "/v1/responses",
			body: liveTimingsStreamBody,
		},
		{
			// Condition 2, END TO END. The unit table already covers the kind
			// check (responses_live_timings_test.go), but nothing drove it
			// through a real request: deleting the LiveTimingsCapableKind call
			// from the gate left every request-path test in this package green.
			//
			// The seed is exactly the "restored dump or direct write" case the
			// gate's doc comment cites as its reason for re-checking the kind at
			// request time. The store is policy-free on purpose -- the PORTAL
			// refuses a true on an incapable kind, no store path does -- so this
			// row is seedable precisely because production can hold such a row.
			name: "an opted-in application whose kind cannot answer the flag",
			seed: liveTimingsSeed{appType: routing.ProviderVLLM, appLiveTimings: true},
			path: "/v1/responses",
			body: liveTimingsStreamBody,
		},
		{
			// The same, via the server_agent branch: the SPEC's kind is what
			// decides there, and Target.Provider is the literal "server_agent".
			// Keeping the spec's opt-in true is what makes the refusal condition
			// 2's rather than condition 1's.
			name: "an opted-in runtime spec whose kind cannot answer the flag",
			seed: func() liveTimingsSeed {
				seed := serverAgentSpecOptedIn()
				seed.spec.Type = string(routing.RuntimeSpecTypeVLLM)
				return seed
			}(),
			path: "/v1/responses",
			body: liveTimingsStreamBody,
		},
		{
			name:            "a client's own explicit false survives",
			seed:            llamaCppOptedIn(),
			path:            "/v1/responses",
			body:            `{"model":"gw-model","stream":true,"input":"hi","timings_per_token":false}`,
			wantRelayedFlag: `"timings_per_token":false`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: terminalOnlyResponsesStream}
			srv := newLiveTimingsTestServer(t, prov, tc.seed)

			got := postPassthrough(t, srv, prov, tc.path, tc.body)

			if tc.wantRelayedFlag == "" {
				if strings.Contains(got, "timings_per_token") {
					t.Fatalf("relayed body grew a timings_per_token flag the gate had to refuse: %s", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantRelayedFlag) {
				t.Fatalf("relayed body does not carry %s: %s", tc.wantRelayedFlag, got)
			}
		})
	}
}

// TestPassthroughNativeDebugLineRecordsTheInjection pins spec D9, whose whole
// purpose is to close a gap the other deferrals leave open together. The
// payload capture deliberately keeps recording the CLIENT's bytes, so an
// operator opening a 400 from a flagged request sees a body that would NOT have
// earned that 400, with nothing anywhere saying the gateway added a key. The
// per-request debug line is that "anywhere".
//
// The field is asserted in BOTH directions on purpose. A field emitted only when
// true is indistinguishable, to an operator grepping a log, from a build that
// never had the field -- and the false cases are the ones they are actually
// debugging: "the switch is on and the panel is still blank, did the gateway
// ask?". The client-already-sent-it row is the sharpest of those, because the
// gate said yes and the key still was not added.
func TestPassthroughNativeDebugLineRecordsTheInjection(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed liveTimingsSeed
		body string
		want bool
	}{
		{
			name: "the gate allowed and the key was added",
			seed: llamaCppOptedIn(),
			body: liveTimingsStreamBody,
			want: true,
		},
		{
			name: "the client had already sent the key, so nothing was added",
			seed: llamaCppOptedIn(),
			body: `{"model":"gw-model","stream":true,"input":"hi","timings_per_token":false}`,
			want: false,
		},
		{
			name: "the operator never switched it on",
			seed: liveTimingsSeed{appType: routing.ProviderLlamaCPP},
			body: liveTimingsStreamBody,
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			defer restore()
			prov := &recordingProxyProvider{respBody: terminalOnlyResponsesStream}
			srv := newLiveTimingsTestServer(t, prov, tc.seed)

			postPassthrough(t, srv, prov, "/v1/responses", tc.body)

			found := false
			for _, rec := range buf.Snapshot() {
				if rec.Msg != "inference request (native passthrough)" {
					continue
				}
				found = true
				got, ok := rec.Attrs["timings_per_token_injected"]
				if !ok {
					t.Fatalf("the per-request debug line carries no timings_per_token_injected field: attrs = %+v", rec.Attrs)
				}
				if got != tc.want {
					t.Fatalf("timings_per_token_injected = %#v, want %v", got, tc.want)
				}
			}
			if !found {
				t.Fatalf("no %q debug record was emitted at all", "inference request (native passthrough)")
			}
		})
	}
}

// TestPassthroughResponsesCaptureStillRecordsTheClientsBody pins the premise the
// rest of this file rests on, and that the block comment at proxyNative's
// body-building step states as fact: the payload capture records the bytes the
// CLIENT sent, not the bytes the gateway relayed (part 2's D7).
//
// Nothing else in the repository pinned it. proxyNative is package-private, so
// its capture argument can be switched from the client's bytes to the outgoing
// ones -- a one-token edit that reads like an improvement, "show what was
// actually sent" -- with the whole package still green. D7 would then invert:
// the capture would show a timings_per_token the client never sent, an operator
// would read it as the client's own request, and the confusion the debug field
// exists to prevent would become the default.
//
// The fixture is an opted-in one, so the injection really happens and the two
// bodies really differ. The relayed body is re-asserted here rather than taken
// on trust, because a version of this test that compared two identical bodies
// would pass for the wrong reason.
func TestPassthroughResponsesCaptureStillRecordsTheClientsBody(t *testing.T) {
	captures := store.NewMemoryCaptureStore(0)
	prov := &recordingProxyProvider{respBody: terminalOnlyResponsesStream}
	srv := newLiveTimingsTestServer(t, prov, llamaCppOptedIn())
	// Capture is per-token opt-in; the override is the one switch that turns it
	// on for this fixture without changing the shared seed's token. No cipher is
	// wired, so persistCapture takes its RAM fallback and the blob is plain gzip.
	srv.Captures = captures
	srv.CaptureOverride = func() bool { return true }

	relayed := postPassthrough(t, srv, prov, "/v1/responses", liveTimingsStreamBody)

	if !strings.Contains(relayed, `"timings_per_token":true`) {
		t.Fatalf("the relayed body carries no injected flag, so this test would be comparing two identical bodies and would prove nothing: %s", relayed)
	}
	events := srv.Usage.ByUser("usr_dev")
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want exactly 1 (the capture is keyed by that event's id)", len(events))
	}
	row, err := captures.Capture(context.Background(), events[0].ID)
	if err != nil {
		t.Fatalf("Capture(%q): %v", events[0].ID, err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(row.Blob))
	if err != nil {
		t.Fatalf("capture blob is not plain gzip (no cipher is wired, so it must be): %v", err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read capture blob: %v", err)
	}
	var env captureEnvelope
	if err := json.Unmarshal(plain, &env); err != nil {
		t.Fatalf("unmarshal capture envelope: %v", err)
	}
	if strings.Contains(env.ReqBody, timingsPerTokenKey) {
		t.Fatalf("the capture recorded the gateway's injected key, so an operator reading it as the client's request sees a body the client never sent: %s", env.ReqBody)
	}
	// Exact, not merely "carries no injected key": the capture is the CLIENT's
	// request, so the mapped provider model must not have reached it either.
	if env.ReqBody != liveTimingsStreamBody {
		t.Fatalf("captured request body = %s, want the client's own bytes %s", env.ReqBody, liveTimingsStreamBody)
	}
}
