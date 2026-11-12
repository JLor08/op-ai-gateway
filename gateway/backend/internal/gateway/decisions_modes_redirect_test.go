// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"
)

// The decision-mode fixture's models: one per (systemone_mode, decisions_mode)
// pair that tells the two decision endpoints apart. Each carries both verdicts,
// so only the modes decide which endpoint serves it.
const (
	decModesGood    = "dec-good"   // both endpoints on
	decModesDecOff  = "dec-off"    // decisions_mode disabled, systemone_mode passthrough
	decModesS1Off   = "s1-off"     // decisions_mode auto, systemone_mode disabled
	decModesUnknown = "no-such-ai" // exists nowhere
)

// newDecisionModesTestServer seeds three ordinary vLLM applications declaring
// the decisions flavor, each on a server of its own pointing at upstreamURL,
// and the dev token ("dev-secret") with the caller's redirect settings in tok.
func newDecisionModesTestServer(t *testing.T, upstreamURL string, tok store.TokenRecord) *Server {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, CreatedAt: now, UpdatedAt: now})
	tok.ID, tok.UserID, tok.Name, tok.Status = "tok_dev", "usr_dev", "Dev Token", store.TokenStatusActive
	tok.Scopes, tok.CreatedAt, tok.UpdatedAt = `["gateway:use"]`, now, now
	if err := directory.CreatePlainToken(ctx, tok, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	up, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", upstreamURL, err)
	}
	port, err := strconv.Atoi(up.Port())
	if err != nil {
		t.Fatalf("upstream port %q: %v", up.Port(), err)
	}
	routeStore := routing.NewMemoryStore()
	for _, m := range []struct {
		model               string
		systemOne, decision routing.EndpointMode
	}{
		{decModesGood, routing.EndpointModePassthrough, routing.EndpointModeAuto},
		{decModesDecOff, routing.EndpointModePassthrough, routing.EndpointModeDisabled},
		{decModesS1Off, routing.EndpointModeDisabled, routing.EndpointModeAuto},
	} {
		srvID, appID, mapID := "srv-"+m.model, "app-"+m.model, "route-"+m.model
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: srvID, Name: srvID, Domain: up.Hostname(), Provider: routing.ProviderVLLM, Endpoint: upstreamURL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", srvID, err)
		}
		if err := routeStore.CreateApplication(ctx, routing.Application{ID: appID, ServerID: srvID, Type: routing.ProviderVLLM, Port: port, Scheme: up.Scheme, APIFlavors: []string{routing.APIFlavorDecisions}, SystemOneMode: m.systemOne, DecisionsMode: m.decision, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", appID, err)
		}
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: mapID, ApplicationID: appID, GatewayModelName: m.model, AppModelName: m.model + "_real", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", mapID, err)
		}
		rows := []routing.CapabilityRow{
			{Capability: routing.CapabilityDecision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
			{Capability: routing.CapabilityDecisionOpenAI, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
		}
		if err := routeStore.UpsertMappingCapabilities(ctx, mapID, rows); err != nil {
			t.Fatalf("UpsertMappingCapabilities %s: %v", mapID, err)
		}
		if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: srvID, ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertTelemetry %s: %v", srvID, err)
		}
	}
	recorder := usage.NewRecorder()
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: provider.NewOpenAICompatibleClient(http.DefaultClient),
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

// requireUpstreamModel reads the one upstream call and fails unless it went to
// path with the upstream model of the gateway model want.
func requireUpstreamModel(t *testing.T, calls <-chan systemOneUpstreamCall, path, want string) {
	t.Helper()
	select {
	case call := <-calls:
		var body struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(call.body, &body); err != nil {
			t.Fatalf("upstream body is not JSON: %v; %s", err, call.body)
		}
		if call.path != path || body.Model != want+"_real" {
			t.Fatalf("upstream call = %s with model %q, want %s with %q", call.path, body.Model, path, want+"_real")
		}
	default:
		t.Fatalf("no upstream call, want one to %s for %s", path, want)
	}
}

// The unknown-model redirect for POST /v1/decisions judges a candidate by the
// decisions mode, and for POST /v1/systemone by the systemone mode: a
// LastUsedModel whose endpoint is disabled is skipped for the fallback, and one
// whose OTHER endpoint is disabled is taken.
func TestRedirectJudgesDecisionCandidatesByTheEndpointsMode(t *testing.T) {
	cases := []struct {
		name, path, lastUsed, want string
		body                       func(string) string
	}{
		{"decisions skips a decisions-disabled candidate", "/v1/decisions", decModesDecOff, decModesGood, decisionsRequestFor},
		{"decisions takes a systemone-disabled candidate", "/v1/decisions", decModesS1Off, decModesS1Off, decisionsRequestFor},
		{"systemone skips a systemone-disabled candidate", "/v1/systemone", decModesS1Off, decModesGood, systemOneRequestFor},
		{"systemone takes a decisions-disabled candidate", "/v1/systemone", decModesDecOff, decModesDecOff, systemOneRequestFor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newDecisionsUpstream(t, defaultDecisionsReplies())
			srv := newDecisionModesTestServer(t, up.URL, store.TokenRecord{UnknownModelRedirect: true, LastUsedModel: tc.lastUsed, UnknownModelFallback: decModesGood})

			rec := postBearer(t, srv, "dev-secret", tc.path, tc.body(decModesUnknown))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (redirected to %s); body = %s", rec.Code, tc.want, rec.Body.String())
			}
			requireUpstreamModel(t, calls, tc.path, tc.want)
		})
	}
}

// Under UnknownModelRedirectBlocked a requested model whose endpoint is
// disabled is the "exists but you cannot call it" case and is redirected; the
// same model on its other, enabled endpoint is callable and keeps its name.
func TestRedirectBlockedTakesADisabledDecisionEndpointElsewhere(t *testing.T) {
	cases := []struct {
		name, path, requested, want string
		body                        func(string) string
	}{
		{"decisions-disabled model on /v1/decisions", "/v1/decisions", decModesDecOff, decModesGood, decisionsRequestFor},
		{"systemone-disabled model on /v1/decisions", "/v1/decisions", decModesS1Off, decModesS1Off, decisionsRequestFor},
		{"systemone-disabled model on /v1/systemone", "/v1/systemone", decModesS1Off, decModesGood, systemOneRequestFor},
		{"decisions-disabled model on /v1/systemone", "/v1/systemone", decModesDecOff, decModesDecOff, systemOneRequestFor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newDecisionsUpstream(t, defaultDecisionsReplies())
			srv := newDecisionModesTestServer(t, up.URL, store.TokenRecord{UnknownModelRedirect: true, UnknownModelRedirectBlocked: true, UnknownModelFallback: decModesGood})

			rec := postBearer(t, srv, "dev-secret", tc.path, tc.body(tc.requested))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (served by %s); body = %s", rec.Code, tc.want, rec.Body.String())
			}
			requireUpstreamModel(t, calls, tc.path, tc.want)
		})
	}
}

// GET /v1/systemone/models lists a model only while its systemone endpoint is
// on: a decision model with systemone_mode disabled is absent although it keeps
// the decisions flavor (POST /v1/decisions still serves it).
func TestSystemOneModelsOmitsASystemOneDisabledModel(t *testing.T) {
	up, _ := newDecisionsUpstream(t, defaultDecisionsReplies())
	srv := newDecisionModesTestServer(t, up.URL, store.TokenRecord{})

	rec := getSystemOneModels(t, srv, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body = %s", err, rec.Body.String())
	}
	ids := make([]string, 0, len(got.Data))
	for _, m := range got.Data {
		ids = append(ids, m.ID)
	}
	slices.Sort(ids)
	if want := []string{decModesGood, decModesDecOff}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("listed = %v, want %v (%s has systemone_mode disabled)", ids, want, decModesS1Off)
	}
}

// systemOneRequestFor is a minimal System One request for model.
func systemOneRequestFor(model string) string {
	return `{"model":"` + model + `","state":"Ein Text","questions":{"ok":{"type":"noul","instructions":"Is it ok?"}}}`
}
