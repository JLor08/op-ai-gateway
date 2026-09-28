// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// parityFlavors is every coarse flavor an application or a runtime spec may
// list, in the order the parity matrix enumerates them.
var parityFlavors = []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic, routing.APIFlavorOpenAIImages}

// flavorSubsets returns every subset of flavors, the empty one first, each in
// flavors' order and each a non-nil slice (a runtime spec stored as []).
func flavorSubsets(flavors []string) [][]string {
	out := make([][]string, 0, 1<<len(flavors))
	for mask := 0; mask < 1<<len(flavors); mask++ {
		subset := []string{}
		for i, f := range flavors {
			if mask&(1<<i) != 0 {
				subset = append(subset, f)
			}
		}
		out = append(out, subset)
	}
	return out
}

// servedBy sends body to path as the dev token and reports whether the
// gateway served it: true when the fake upstream was called once and the
// answer is 200, false when the gateway refused the request itself -- 404,
// an error code among refusals, and no upstream call. Anything else fails the
// test, so a fixture fault never passes for a refusal.
func servedBy(t *testing.T, srv *Server, prov *countingTranslateProvider, path, body string, refusals ...string) bool {
	t.Helper()
	before := prov.calls()
	rec := postBearer(t, srv, "dev-secret", path, body)
	calls := prov.calls() - before
	if rec.Code == http.StatusOK && calls == 1 {
		return true
	}
	if rec.Code == http.StatusNotFound && calls == 0 && slices.Contains(refusals, errorBodyOf(t, rec)) {
		return false
	}
	t.Fatalf("POST %s: status = %d, upstream calls = %d, body = %s; want 200 with one upstream call, or 404 with one of %q and none",
		path, rec.Code, calls, rec.Body.String(), refusals)
	return false
}

// portalModelRow returns the portal Models() row of name for the dev token.
func portalModelRow(t *testing.T, srv *Server, name string) portal.ModelDTO {
	t.Helper()
	token, ok := srv.Tokens.LookupBearer("Bearer dev-secret")
	if !ok {
		t.Fatal("dev token not found")
	}
	for _, row := range srv.Portal.Models(context.Background(), token).Data {
		if row.ID == name {
			return row
		}
	}
	t.Fatalf("Models() has no row for %q", name)
	return portal.ModelDTO{}
}

// TestListingsAdvertiseExactlyWhatDispatchServes is the parity matrix between
// the listings and dispatch for an agent-launched child. For every
// application flavor set, every runtime spec flavor set (the empty one
// included) and every spec endpoint mode, requireListingsMatchDispatch asks
// the three dispatch endpoints whether they serve the model and requires every
// listing to say exactly that.
//
// There is no /v1/responses leg: the listing's openai stands for chat
// completions, and /v1/responses refuses a spec that lacks openai but is not
// images-only, which no listing reflects (#150). The application sets start
// at one flavor, because the portal stores an application written without
// flavors as [openai, anthropic].
func TestListingsAdvertiseExactlyWhatDispatchServes(t *testing.T) {
	modes := []routing.EndpointMode{routing.EndpointModePassthrough, routing.EndpointModeTranslate, routing.EndpointModeDisabled}
	for _, appFlavors := range flavorSubsets(parityFlavors)[1:] {
		for _, specFlavors := range flavorSubsets(parityFlavors) {
			for _, mode := range modes {
				name := "app=" + strings.Join(appFlavors, "+") + "/spec=" + strings.Join(specFlavors, "+") + "/mode=" + string(mode)
				t.Run(name, func(t *testing.T) { requireListingsMatchDispatch(t, appFlavors, specFlavors, mode) })
			}
		}
	}
}

// requireListingsMatchDispatch builds newServerAgentSpecTestServer with the
// given application flavors, spec flavors and spec mode, gives the mapping an
// image=yes verdict, and asks chat completions, /v1/messages (with a model the
// routing probe reads) and the images relay whether they serve gw-model. The
// portal Models() row must then list exactly the served flavors and carry the
// images relay's answer as its image flag, and /v1/models, /api/v0/models and
// /anthropic/v1/models must list gw-model exactly when its flavor is served.
func requireListingsMatchDispatch(t *testing.T, appFlavors, specFlavors []string, mode routing.EndpointMode) {
	t.Helper()
	const (
		chatBody     = `{"model":"gw-model","messages":[{"role":"user","content":"hi"}]}`
		messagesBody = `{"model":"gw-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
		imagesBody   = `{"model":"gw-model","prompt":"a cat","n":1}`
	)
	prov := &countingTranslateProvider{recordingProxyProvider: recordingProxyProvider{respBody: imagesRelayResponse}}
	srv := newServerAgentSpecTestServer(t, prov, appFlavors, specFlavors, mode)
	seedCapabilityRow(t, srv, "route-native-spec", routing.CapabilityImage, routing.CapabilityYes, "manual")

	served := map[string]bool{
		routing.APIFlavorOpenAI:       servedBy(t, srv, prov, "/v1/chat/completions", chatBody, "routing.no_model_route"),
		routing.APIFlavorAnthropic:    servedBy(t, srv, prov, "/v1/messages", messagesBody, "routing.no_model_route", "messages.endpoint_disabled"),
		routing.APIFlavorOpenAIImages: servedBy(t, srv, prov, "/v1/images/generations", imagesBody, "routing.no_model_route"),
	}
	// The served flavors in the sorted order a Models() row lists them.
	want := []string{}
	for _, f := range []string{routing.APIFlavorAnthropic, routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages} {
		if served[f] {
			want = append(want, f)
		}
	}

	row := portalModelRow(t, srv, "gw-model")
	if !reflect.DeepEqual(row.Flavors, want) {
		t.Errorf("Models() flavors = %q, want the served set %q", row.Flavors, want)
	}
	if row.Image != served[routing.APIFlavorOpenAIImages] {
		t.Errorf("Models() image = %v, want %v (the images relay's answer)", row.Image, served[routing.APIFlavorOpenAIImages])
	}
	listings := []struct {
		path   string
		flavor string
	}{
		{"/v1/models", routing.APIFlavorOpenAI},
		{"/api/v0/models", routing.APIFlavorOpenAI},
		{"/anthropic/v1/models", routing.APIFlavorAnthropic},
	}
	for _, l := range listings {
		if got := slices.Contains(listedIDs(t, srv, "dev-secret", l.path), "gw-model"); got != served[l.flavor] {
			t.Errorf("%s lists gw-model = %v, want %v (what %s dispatch answers)", l.path, got, served[l.flavor], l.flavor)
		}
	}
}
