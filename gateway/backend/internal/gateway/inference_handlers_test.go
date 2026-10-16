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
	"reflect"
	"testing"
)

// fakeOfferingPortal is a minimal portal.API stand-in (embeds a nil interface,
// per the established internal/gateway test pattern — see
// server_override_test.go) that serves one fixed ModelOffering. It also COUNTS
// calls and records the flavor and the required capabilities it was asked
// for, so the no-extra-work invariant (a token without the redirect must never
// trigger the offering lookup) is directly provable rather than inferred.
type fakeOfferingPortal struct {
	portal.API
	off      portal.ModelOffering
	calls    int
	flavor   string
	required []string
}

func (f *fakeOfferingPortal) ModelOfferingFor(_ context.Context, _ auth.Token, flavor string, required []string) portal.ModelOffering {
	f.calls++
	f.flavor = flavor
	f.required = required
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

// TestPreflightRedirectsACapabilityCarryingRequestOnlyToACapableCandidate
// proves the preflight hands the request's required capabilities to the
// offering and takes only a candidate that carries them. The last-used model
// here is callable for the images flavor but lacks the image capability, so
// the fallback is taken instead.
func TestPreflightRedirectsACapabilityCarryingRequestOnlyToACapableCandidate(t *testing.T) {
	fp := &fakeOfferingPortal{off: offeringWithIncapable([]string{"image-model"}, []string{"text-model"})}
	s := &Server{Portal: fp}
	token := auth.Token{UnknownModelRedirect: true, LastUsedModel: "text-model", UnknownModelFallback: "image-model"}
	pf, handled := s.inferencePreflight(httptest.NewRecorder(), newInferenceRequest(t), token, nil,
		inferenceShape{model: "totally-unknown", apiFlavor: apiFlavorImages, endpoint: endpointImages, requiredCapabilities: []string{routing.CapabilityImage}})
	if handled {
		t.Fatal("preflight refused the request")
	}
	if pf.Req.Model != "image-model" {
		t.Fatalf("effective model = %q, want image-model (text-model lacks the capability)", pf.Req.Model)
	}
	if fp.flavor != routing.APIFlavorOpenAIImages {
		t.Fatalf("offering asked for flavor %q, want %q", fp.flavor, routing.APIFlavorOpenAIImages)
	}
	if len(fp.required) != 1 || fp.required[0] != routing.CapabilityImage {
		t.Fatalf("offering asked for capabilities %v, want [%s]", fp.required, routing.CapabilityImage)
	}
}

// TestSpeechInferenceShapeIsTheSpeechEndpointsIdentity pins the shape
// handleOpenAISpeech hands the ONE preflight gate, through the function the
// handler itself calls (speechInferenceShape): the exact coarse flavor (never a
// fine variant: upstreamPath matches it by string equality), the speech
// endpoint, and the speech capability. The capability list is load-bearing
// twice -- it feeds the capability gate, and it is what keeps a speech request
// off the vendor-account path, which names only the images flavor (see
// TestVendorAccountSkippedForSpeech in internal/routing) -- so a handler that
// forgot it would both skip the gate and become routable to a vendor model of
// the same name.
func TestSpeechInferenceShapeIsTheSpeechEndpointsIdentity(t *testing.T) {
	shape := speechInferenceShape("tts-voice")
	if shape.apiFlavor != "openai_audio" {
		t.Fatalf("apiFlavor = %q, want exactly openai_audio", shape.apiFlavor)
	}
	if shape.endpoint != endpointSpeech {
		t.Fatalf("endpoint = %d, want endpointSpeech (%d)", shape.endpoint, endpointSpeech)
	}
	if shape.model != "tts-voice" || shape.stream {
		t.Fatalf("model/stream = %q/%v, want the routing model and a non-stream request", shape.model, shape.stream)
	}
	if !reflect.DeepEqual(shape.requiredCapabilities, []string{routing.CapabilitySpeech}) {
		t.Fatalf("requiredCapabilities = %v, want [%s]", shape.requiredCapabilities, routing.CapabilitySpeech)
	}
}

// TestPreflightRedirectsASpeechRequestOnlyToACapableCandidate is the images
// redirect test's speech twin: the preflight asks the offering for the speech
// flavor and capability, takes only a candidate carrying it, and the request it
// produces carries the flavor and the capability list on to Resolve.
func TestPreflightRedirectsASpeechRequestOnlyToACapableCandidate(t *testing.T) {
	fp := &fakeOfferingPortal{off: offeringWithIncapable([]string{"voice-model"}, []string{"text-model"})}
	s := &Server{Portal: fp}
	token := auth.Token{UnknownModelRedirect: true, LastUsedModel: "text-model", UnknownModelFallback: "voice-model"}
	pf, handled := s.inferencePreflight(httptest.NewRecorder(), newInferenceRequest(t), token, nil, speechInferenceShape("totally-unknown"))
	if handled {
		t.Fatal("preflight refused the request")
	}
	if pf.Req.Model != "voice-model" {
		t.Fatalf("effective model = %q, want voice-model (text-model lacks the capability)", pf.Req.Model)
	}
	if fp.flavor != routing.APIFlavorOpenAIAudio {
		t.Fatalf("offering asked for flavor %q, want %q", fp.flavor, routing.APIFlavorOpenAIAudio)
	}
	if len(fp.required) != 1 || fp.required[0] != routing.CapabilitySpeech {
		t.Fatalf("offering asked for capabilities %v, want [%s]", fp.required, routing.CapabilitySpeech)
	}
	if pf.Req.APIFlavor != apiFlavorSpeech || !reflect.DeepEqual(pf.Req.RequiredCapabilities, []string{routing.CapabilitySpeech}) {
		t.Fatalf("preflight request = flavor %q, capabilities %v, want %q and [%s]", pf.Req.APIFlavor, pf.Req.RequiredCapabilities, apiFlavorSpeech, routing.CapabilitySpeech)
	}
}

// aliasTargetCall is one ClaudeAliasTarget call a fakeAliasPortal saw.
type aliasTargetCall struct {
	token     auth.Token
	requested string
}

// fakeAliasPortal is a portal.API stand-in for the generated Claude alias
// resolution (issue #225). ClaudeAliasTarget answers from a fixed alias ->
// target table, with no derivation, so a test can pin the preflight's own
// precedence and single-resolution rules independently of the discovery
// derivation (which resolves in the real portal tests); it records every call.
// It embeds the offering fake so a redirect can be exercised beside it.
type fakeAliasPortal struct {
	*fakeOfferingPortal
	targets map[string]string
	calls   []aliasTargetCall
}

func (f *fakeAliasPortal) ClaudeAliasTarget(_ context.Context, token auth.Token, requested string) (string, bool) {
	f.calls = append(f.calls, aliasTargetCall{token: token, requested: requested})
	target, ok := f.targets[requested]
	return target, ok
}

func newFakeAliasPortal(targets map[string]string, offered ...string) *fakeAliasPortal {
	return &fakeAliasPortal{fakeOfferingPortal: newOfferingPortal(offered...), targets: targets}
}

// TestResolveModelOverrideWithGeneratedAliases pins the resolution precedence
// of generated Claude aliases: an explicit model_override_map entry, then the
// generated alias (only for a token with the setting), then the catch-all, then
// the requested name unchanged. The alias is resolved ONCE: its target is a real
// gateway id that no rule and no catch-all rewrites again.
func TestResolveModelOverrideWithGeneratedAliases(t *testing.T) {
	const aliasA, aliasB, aliasZ = "anthropic-local-aaaaaaaaaaaa", "anthropic-local-bbbbbbbbbbbb", "anthropic-local-zzzzzzzzzzzz"
	rules := map[string]auth.ModelOverrideRule{
		aliasA: {To: "manual-target"},
		"real": {To: "elsewhere"}, // a rule keyed on the generated alias's TARGET
	}
	targets := map[string]string{aliasA: "real", aliasB: "real"}
	token := func(aliases bool, catchAll string) auth.Token {
		return auth.Token{ClaudeModelAliases: aliases, ModelOverride: catchAll, ModelOverrideRules: rules}
	}
	tests := []struct {
		name      string
		token     auth.Token
		requested string
		want      string
		wantCalls []string // the requested ids the portal was asked about
	}{
		{"explicit entry wins over the generated alias, which is not even asked for", token(true, "catch-all"), aliasA, "manual-target", nil},
		{"generated alias resolves once to its real target: the rule keyed on that target is not applied", token(true, ""), aliasB, "real", []string{aliasB}},
		{"generated alias beats the catch-all", token(true, "catch-all"), aliasB, "real", []string{aliasB}},
		{"unresolvable alias-shaped id falls to the catch-all", token(true, "catch-all"), aliasZ, "catch-all", []string{aliasZ}},
		{"unresolvable alias-shaped id without catch-all stays as requested", token(true, ""), aliasZ, aliasZ, []string{aliasZ}},
		{"an ordinary name still gets the catch-all", token(true, "catch-all"), "ordinary", "catch-all", []string{"ordinary"}},
		{"an ordinary name with a rule still gets the rule", token(true, "catch-all"), "real", "elsewhere", nil},
		{"setting off: the alias is not resolved, the catch-all applies", token(false, "catch-all"), aliasB, "catch-all", nil},
		{"setting off, no catch-all: the alias stays unresolved", token(false, ""), aliasB, aliasB, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fp := newFakeAliasPortal(targets)
			if got := resolveModelOverride(context.Background(), tt.token, tt.requested, fp); got != tt.want {
				t.Fatalf("resolveModelOverride(%q) = %q, want %q", tt.requested, got, tt.want)
			}
			var asked []string
			for _, call := range fp.calls {
				asked = append(asked, call.requested)
			}
			if !reflect.DeepEqual(asked, tt.wantCalls) {
				t.Fatalf("ClaudeAliasTarget asked about %q, want %q (one lookup of the REQUESTED id, never of a resolved target)", asked, tt.wantCalls)
			}
		})
	}
}

// TestResolveModelOverrideWithoutAPortal: ServerDeps.Portal is optional, so a
// token with the setting on a Server without one resolves like a token without
// it, instead of panicking.
func TestResolveModelOverrideWithoutAPortal(t *testing.T) {
	token := auth.Token{ClaudeModelAliases: true, ModelOverride: "catch-all"}
	if got := resolveModelOverride(context.Background(), token, "anthropic-local-aaaaaaaaaaaa", nil); got != "catch-all" {
		t.Fatalf("resolveModelOverride without a portal = %q, want catch-all", got)
	}
}

// TestPreflightResolvesAGeneratedAliasKeepingTheRequestedName: the preflight's
// effective model is the alias's real target, and the request keeps the alias
// the client sent, which is what the usage events record.
func TestPreflightResolvesAGeneratedAliasKeepingTheRequestedName(t *testing.T) {
	const alias = "anthropic-local-aaaaaaaaaaaa"
	s := &Server{Portal: newFakeAliasPortal(map[string]string{alias: "real"}, "real")}
	pf, handled := s.inferencePreflight(httptest.NewRecorder(), newInferenceRequest(t),
		auth.Token{ClaudeModelAliases: true}, nil, inferenceShape{model: alias, apiFlavor: "anthropic_messages"})
	if handled {
		t.Fatal("preflight refused the request")
	}
	if pf.Req.Model != "real" || pf.Req.RequestedModel != alias {
		t.Fatalf("(Model, RequestedModel) = (%q, %q), want (real, %s)", pf.Req.Model, pf.Req.RequestedModel, alias)
	}
}

// TestPreflightAllowlistJudgesTheResolvedTargetNotTheAlias: modelAllowed runs on
// the EFFECTIVE model. A service token whose allowlist names the target may
// reach it through the alias although the alias id is not on the list, and an
// allowlist that names only the alias (or some other model) does not let the
// alias through to a target it excludes.
func TestPreflightAllowlistJudgesTheResolvedTargetNotTheAlias(t *testing.T) {
	const alias = "anthropic-local-aaaaaaaaaaaa"
	tests := []struct {
		name        string
		allowed     []string
		wantHandled bool
	}{
		{"target allowed", []string{"real"}, false},
		{"target allowed among others", []string{"other", "real"}, false},
		{"only the alias is listed: the alias id grants nothing", []string{alias}, true},
		{"another model is listed", []string{"other"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{Portal: newFakeAliasPortal(map[string]string{alias: "real"}, "real")}
			token := auth.Token{Kind: "service", ClaudeModelAliases: true, AllowedModels: tt.allowed}
			rec := httptest.NewRecorder()
			pf, handled := s.inferencePreflight(rec, newInferenceRequest(t), token, nil, inferenceShape{model: alias, apiFlavor: "anthropic_messages"})
			if handled != tt.wantHandled {
				t.Fatalf("handled = %v (status %d), want %v", handled, rec.Code, tt.wantHandled)
			}
			if tt.wantHandled && rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 model.not_allowed", rec.Code)
			}
			if !tt.wantHandled && pf.Req.Model != "real" {
				t.Fatalf("effective model = %q, want real", pf.Req.Model)
			}
		})
	}
}

// TestPreflightUnresolvedAliasFollowsTheUnknownModelRedirect: a generated alias
// that no longer resolves (setting off, or the model went away) is just an
// unknown name, so the existing unknown-model redirect applies to it exactly as
// to any other unknown name -- and a resolved alias is NOT unknown, so the
// redirect leaves it alone. There is no alias-specific fallback.
func TestPreflightUnresolvedAliasFollowsTheUnknownModelRedirect(t *testing.T) {
	const alias = "anthropic-local-aaaaaaaaaaaa"
	t.Run("unresolved alias is redirected like any unknown name", func(t *testing.T) {
		s := &Server{Portal: newFakeAliasPortal(nil, "last-model")}
		token := auth.Token{ClaudeModelAliases: true, UnknownModelRedirect: true, LastUsedModel: "last-model"}
		pf, handled := s.inferencePreflight(httptest.NewRecorder(), newInferenceRequest(t), token, nil, inferenceShape{model: alias, apiFlavor: "anthropic_messages"})
		if handled {
			t.Fatal("preflight refused the request")
		}
		if pf.Req.Model != "last-model" || pf.Req.RequestedModel != alias {
			t.Fatalf("(Model, RequestedModel) = (%q, %q), want (last-model, %s)", pf.Req.Model, pf.Req.RequestedModel, alias)
		}
	})
	t.Run("a resolved alias is not unknown and is not redirected", func(t *testing.T) {
		s := &Server{Portal: newFakeAliasPortal(map[string]string{alias: "real"}, "real", "last-model")}
		token := auth.Token{ClaudeModelAliases: true, UnknownModelRedirect: true, LastUsedModel: "last-model"}
		pf, handled := s.inferencePreflight(httptest.NewRecorder(), newInferenceRequest(t), token, nil, inferenceShape{model: alias, apiFlavor: "anthropic_messages"})
		if handled {
			t.Fatal("preflight refused the request")
		}
		if pf.Req.Model != "real" {
			t.Fatalf("effective model = %q, want real", pf.Req.Model)
		}
	})
	t.Run("setting off: the alias is not asked for and is redirected as unknown", func(t *testing.T) {
		fp := newFakeAliasPortal(map[string]string{alias: "real"}, "last-model")
		s := &Server{Portal: fp}
		token := auth.Token{UnknownModelRedirect: true, LastUsedModel: "last-model"}
		pf, handled := s.inferencePreflight(httptest.NewRecorder(), newInferenceRequest(t), token, nil, inferenceShape{model: alias, apiFlavor: "anthropic_messages"})
		if handled {
			t.Fatal("preflight refused the request")
		}
		if pf.Req.Model != "last-model" {
			t.Fatalf("effective model = %q, want last-model (the alias must not resolve with the setting off)", pf.Req.Model)
		}
		if len(fp.calls) != 0 {
			t.Fatalf("ClaudeAliasTarget called %d times for a token without the setting, want 0", len(fp.calls))
		}
	})
}
