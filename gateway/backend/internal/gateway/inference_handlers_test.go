// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"testing"
)

// fakeOfferingPortal is a minimal portal.API stand-in (embeds a nil interface,
// per the established internal/gateway test pattern — see
// server_override_test.go) that serves one fixed ModelOffering. It also COUNTS
// calls and records the flavor it was asked for, so the no-extra-work
// invariant (a token without the redirect must never trigger the offering
// lookup) is directly provable rather than inferred.
type fakeOfferingPortal struct {
	portal.API
	off    portal.ModelOffering
	calls  int
	flavor string
}

func (f *fakeOfferingPortal) ModelOfferingFor(_ context.Context, _ auth.Token, flavor string) portal.ModelOffering {
	f.calls++
	f.flavor = flavor
	return f.off
}

// newOfferingPortal offers (and knows to exist) exactly the given names.
func newOfferingPortal(names ...string) *fakeOfferingPortal {
	return &fakeOfferingPortal{off: offering(names, names)}
}

// serverWithSuppressedOffering builds a Server whose portal reports `offered`
// as listed-and-callable and `suppressed` as callable-but-not-listed — the
// model_settings hidden/locked shape (see offeringWithSuppressed).
func serverWithSuppressedOffering(offered, suppressed []string) *Server {
	return &Server{Portal: &fakeOfferingPortal{off: offeringWithSuppressed(offered, suppressed)}}
}

// serverWithOffering builds a Server whose Portal reports a ModelOffering with
// exactly these names in both Callable and Existing. Everything else on the
// Server stays zero: inferencePreflight only reaches the portal through the
// override re-authorization (skipped when no server override is configured)
// and the redirect, and s.Limiter.Admit is nil-safe.
func serverWithOffering(names ...string) *Server {
	return &Server{Portal: newOfferingPortal(names...)}
}

func newInferenceRequest(t *testing.T) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
}

func TestPreflightCatchAllWinsOverTheRedirect(t *testing.T) {
	// The catch-all is an explicit instruction; turning the redirect on must not
	// silently change what an existing token does.
	token := auth.Token{
		ModelOverride:        "catchall-model",
		UnknownModelRedirect: true,
		LastUsedModel:        "last-model",
	}
	pf, handled := serverWithOffering("catchall-model", "last-model").
		inferencePreflight(httptest.NewRecorder(), newInferenceRequest(t), token, nil,
			inferenceShape{model: "totally-unknown", apiFlavor: "openai"})
	if handled {
		t.Fatal("preflight refused the request")
	}
	if pf.Req.Model != "catchall-model" {
		t.Fatalf("effective model = %q, want catchall-model", pf.Req.Model)
	}
}

func TestPreflightRedirectTargetStillFacesTheAllowlist(t *testing.T) {
	// The invariant: a redirect changes WHICH name is requested, never what a
	// token may reach. A service token whose allowlist excludes the redirect
	// target must be refused, not served.
	token := auth.Token{
		Kind:                 "service",
		AllowedModels:        []string{"allowed-model"},
		UnknownModelRedirect: true,
		LastUsedModel:        "forbidden-model",
	}
	rec := httptest.NewRecorder()
	_, handled := serverWithOffering("allowed-model", "forbidden-model").
		inferencePreflight(rec, newInferenceRequest(t), token, nil,
			inferenceShape{model: "unknown", apiFlavor: "openai"})
	if !handled || rec.Code != http.StatusForbidden {
		t.Fatalf("redirect reached a model outside the allowlist: handled=%v code=%d", handled, rec.Code)
	}
}

// TestPreflightCatchAllWinsOverTheRedirectEvenWhenSuppressed is the case
// TestPreflightCatchAllWinsOverTheRedirect cannot reach: there the catch-all
// target is listed, so the redirect declines for the ordinary reason. Point the
// catch-all at a model that model_settings hides while leaving it callable, and
// a redirect that judged by the LISTING would find req.Model unlisted and, in
// widened mode, reroute away from the operator's explicit instruction. The
// catch-all must always win, suppressed target or not.
func TestPreflightCatchAllWinsOverTheRedirectEvenWhenSuppressed(t *testing.T) {
	token := auth.Token{
		ModelOverride:               "hidden-catchall",
		UnknownModelRedirect:        true,
		UnknownModelRedirectBlocked: true,
		LastUsedModel:               "last-model",
	}
	pf, handled := serverWithSuppressedOffering([]string{"last-model"}, []string{"hidden-catchall"}).
		inferencePreflight(httptest.NewRecorder(), newInferenceRequest(t), token, nil,
			inferenceShape{model: "totally-unknown", apiFlavor: "openai"})
	if handled {
		t.Fatal("preflight refused the request")
	}
	if pf.Req.Model != "hidden-catchall" {
		t.Fatalf("effective model = %q, want hidden-catchall (the catch-all must win)", pf.Req.Model)
	}
}

// TestPreflightRedirectNeverPicksAnAllowlistBlockedTarget is the sharp edge of
// the same invariant. Here the name the client asked for passes the allowlist
// (it is simply not offered anywhere), so nothing downstream would ever look at
// the redirect's target on this request's behalf: modelAllowed sees only the
// EFFECTIVE model, and if the redirect swapped in a blocked one the 403 would
// name a model the client never sent. The redirect must therefore refuse the
// candidate itself (callableFor) and leave the request's model alone.
func TestPreflightRedirectNeverPicksAnAllowlistBlockedTarget(t *testing.T) {
	token := auth.Token{
		Kind:                 "service",
		AllowedModels:        []string{"ghost-model"},
		UnknownModelRedirect: true,
		LastUsedModel:        "forbidden-model",
	}
	pf, handled := serverWithOffering("forbidden-model").
		inferencePreflight(httptest.NewRecorder(), newInferenceRequest(t), token, nil,
			inferenceShape{model: "ghost-model", apiFlavor: "openai"})
	if handled {
		t.Fatal("preflight refused a request whose own model the allowlist permits")
	}
	if pf.Req.Model != "ghost-model" {
		t.Fatalf("effective model = %q, want ghost-model (the blocked candidate must not be taken)", pf.Req.Model)
	}
}

// TestPreflightRedirectRunsBeforeTheAllowlist pins the ORDER, and is what makes
// the service allowlist reachable by widened mode at all. The wish here is
// blocked ONLY by the allowlist — the canonical "exists but you may not use it"
// case UnknownModelRedirectBlocked is for. A redirect placed AFTER modelAllowed
// would never see this request: the 403 would already have been written.
func TestPreflightRedirectRunsBeforeTheAllowlist(t *testing.T) {
	token := auth.Token{
		Kind:                        "service",
		AllowedModels:               []string{"allowed-model"},
		UnknownModelRedirect:        true,
		UnknownModelRedirectBlocked: true,
		LastUsedModel:               "allowed-model",
	}
	rec := httptest.NewRecorder()
	pf, handled := serverWithOffering("allowed-model", "forbidden-model").
		inferencePreflight(rec, newInferenceRequest(t), token, nil,
			inferenceShape{model: "forbidden-model", apiFlavor: "openai"})
	if handled {
		t.Fatalf("preflight refused the request instead of redirecting it: code=%d", rec.Code)
	}
	if pf.Req.Model != "allowed-model" {
		t.Fatalf("effective model = %q, want allowed-model", pf.Req.Model)
	}
	if pf.Req.RequestedModel != "forbidden-model" {
		t.Fatalf("requested model = %q, want the client's original wish", pf.Req.RequestedModel)
	}
}

// TestPreflightRedirectKeepsTheClientsOriginalWish proves a redirect rewrites
// only the effective model: RequestedModel keeps what the client asked for, so
// the usage events that already carry it stay a record of the redirect.
func TestPreflightRedirectKeepsTheClientsOriginalWish(t *testing.T) {
	token := auth.Token{UnknownModelRedirect: true, LastUsedModel: "last-model"}
	pf, handled := serverWithOffering("last-model").
		inferencePreflight(httptest.NewRecorder(), newInferenceRequest(t), token, nil,
			inferenceShape{model: "totally-unknown", apiFlavor: "openai"})
	if handled {
		t.Fatal("preflight refused the request")
	}
	if pf.Req.Model != "last-model" {
		t.Fatalf("effective model = %q, want last-model", pf.Req.Model)
	}
	if pf.Req.RequestedModel != "totally-unknown" {
		t.Fatalf("RequestedModel = %q, want the client's original totally-unknown", pf.Req.RequestedModel)
	}
}

// TestPreflightWithoutRedirectDoesNoOfferingLookup proves the strict no-op
// invariant: a token that did not opt in pays a single boolean test and no
// store work at all, so its path stays bit-for-bit what it is today.
func TestPreflightWithoutRedirectDoesNoOfferingLookup(t *testing.T) {
	fp := newOfferingPortal("last-model")
	s := &Server{Portal: fp}
	pf, handled := s.inferencePreflight(httptest.NewRecorder(), newInferenceRequest(t),
		auth.Token{LastUsedModel: "last-model"}, nil,
		inferenceShape{model: "totally-unknown", apiFlavor: "openai"})
	if handled {
		t.Fatal("preflight refused the request")
	}
	if pf.Req.Model != "totally-unknown" {
		t.Fatalf("effective model = %q, want the untouched totally-unknown", pf.Req.Model)
	}
	if fp.calls != 0 {
		t.Fatalf("ModelOfferingFor calls = %d, want 0 (no-op without the switch)", fp.calls)
	}
}

// TestPreflightAsksTheOfferingForTheNormalizedFlavor proves the wire flavor is
// normalized before the lookup — the offering is keyed by routing's canonical
// flavor names, so passing a raw handler-supplied variant through would answer
// about a flavor that has no models at all.
func TestPreflightAsksTheOfferingForTheNormalizedFlavor(t *testing.T) {
	fp := newOfferingPortal("last-model")
	s := &Server{Portal: fp}
	token := auth.Token{UnknownModelRedirect: true, LastUsedModel: "last-model"}
	if _, handled := s.inferencePreflight(httptest.NewRecorder(), newInferenceRequest(t), token, nil,
		inferenceShape{model: "totally-unknown", apiFlavor: "openai-chat"}); handled {
		t.Fatal("preflight refused the request")
	}
	if fp.flavor != routing.APIFlavorOpenAI {
		t.Fatalf("offering asked for flavor %q, want the normalized %q", fp.flavor, routing.APIFlavorOpenAI)
	}
}

// TestPreflightRedirectSurvivesAServerWithoutAPortal proves the redirect is
// nil-safe like the rest of the preflight: a Server built without a portal
// (ServerDeps.Portal is optional) declines the redirect instead of panicking.
func TestPreflightRedirectSurvivesAServerWithoutAPortal(t *testing.T) {
	token := auth.Token{UnknownModelRedirect: true, LastUsedModel: "last-model"}
	pf, handled := (&Server{}).inferencePreflight(httptest.NewRecorder(), newInferenceRequest(t), token, nil,
		inferenceShape{model: "totally-unknown", apiFlavor: "openai"})
	if handled {
		t.Fatal("preflight refused the request")
	}
	if pf.Req.Model != "totally-unknown" {
		t.Fatalf("effective model = %q, want the untouched totally-unknown", pf.Req.Model)
	}
}
