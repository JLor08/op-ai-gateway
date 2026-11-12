// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"net/http"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

const (
	decisionsYesBody   = `{"answers":[{"type":"predicate","name":"probe","value":true}]}`
	systemOneYesBody   = `{"answers":{"probe":{"type":"noul","noul":0.9}}}`
	systemOnePathValue = "/v1/systemone"
	decisionsPathValue = "/v1/decisions"
)

// pathsSeen returns the request paths the upstream saw, in order.
func pathsSeen(up *systemOneUpstream) []string {
	var out []string
	for _, r := range up.requests() {
		out = append(out, r.path)
	}
	return out
}

func countPath(up *systemOneUpstream, path string) int {
	n := 0
	for _, r := range up.requests() {
		if r.path == path {
			n++
		}
	}
	return n
}

// TestDecisionsPassProbesAnExternalVLLMAppBothWays: an external vllm app that
// declares decisions is asked both the System One and the OpenAI Decisions
// question, and both rows land under their own source.
func TestDecisionsPassProbesAnExternalVLLMAppBothWays(t *testing.T) {
	up := newSystemOneUpstream(t)
	up.answerPath(systemOnePathValue, http.StatusOK, systemOneYesBody)
	up.answerPath(decisionsPathValue, http.StatusOK, decisionsYesBody)
	_, port := up.hostPort(t)
	runner, st, _ := systemOneRunner(t, up, systemOneApp(routing.ProviderVLLM, port, routing.APIFlavorDecisions), systemOneMapping("m1", "decider"))

	runCapabilityCycle(runner)

	st.assertCapability(t, "m1", routing.CapabilityDecision, routing.CapabilityYes, routing.CapabilitySourceSystemOneProbe)
	st.assertCapability(t, "m1", routing.CapabilityDecisionOpenAI, routing.CapabilityYes, routing.CapabilitySourceDecisionsProbe)
	if countPath(up, systemOnePathValue) != 1 || countPath(up, decisionsPathValue) != 1 {
		t.Fatalf("upstream saw %v, want one request per path", pathsSeen(up))
	}
}

// TestDecisionsPassSkipsOutOfScopeTypes: an external llama_cpp app only gets
// the System One probe.
func TestDecisionsPassSkipsOutOfScopeTypes(t *testing.T) {
	up := newSystemOneUpstream(t)
	_, port := up.hostPort(t)
	runner, st, _ := systemOneRunner(t, up, systemOneApp(routing.ProviderLlamaCPP, port, routing.APIFlavorDecisions), systemOneMapping("m1", "decider"))

	runCapabilityCycle(runner)

	if countPath(up, decisionsPathValue) != 0 {
		t.Fatalf("llama_cpp saw %v, want no /v1/decisions", pathsSeen(up))
	}
	if _, ok := st.capabilityOf("m1", routing.CapabilityDecisionOpenAI); ok {
		t.Fatal("a decision_openai row was written for llama_cpp")
	}
	st.assertCapability(t, "m1", routing.CapabilityDecision, routing.CapabilityYes, routing.CapabilitySourceSystemOneProbe)
}

// TestDecisionsPassForAgentSpecTypes: a server_agent mapping gets the OpenAI
// probe for spec type vllm or custom, only while loaded; llama_cpp never.
func TestDecisionsPassForAgentSpecTypes(t *testing.T) {
	cases := []struct {
		specType string
		want     int
	}{
		{string(routing.RuntimeSpecTypeCustom), 1},
		{string(routing.RuntimeSpecTypeVLLM), 1},
		{string(routing.RuntimeSpecTypeLlamaCpp), 0},
	}
	for _, tc := range cases {
		t.Run(tc.specType, func(t *testing.T) {
			up := newSystemOneUpstream(t)
			up.answerPath(decisionsPathValue, http.StatusOK, decisionsYesBody)
			runner, st, loaded := agentSystemOneRunner(t, up)
			st.runtimeSpecs[0].Type = tc.specType

			runCapabilityCycle(runner)
			if n := len(up.requests()); n != 0 {
				t.Fatalf("unloaded: the router saw %v, want nothing", pathsSeen(up))
			}

			loaded.SetGatewayProbe("a1", []string{"decider"})
			runCapabilityCycle(runner)
			if got := countPath(up, decisionsPathValue); got != tc.want {
				t.Fatalf("loaded %s: %d /v1/decisions requests, want %d (%v)", tc.specType, got, tc.want, pathsSeen(up))
			}
			if countPath(up, systemOnePathValue) != 1 {
				t.Fatalf("the System One probe must still run once, saw %v", pathsSeen(up))
			}
			_, ok := st.capabilityOf("m1", routing.CapabilityDecisionOpenAI)
			if ok != (tc.want == 1) {
				t.Fatalf("decision_openai row present = %v, want %v", ok, tc.want == 1)
			}
		})
	}
}

// TestDecisionsPassKeepsSettledStateSeparate: a settled System One verdict
// does not stop the OpenAI probe, which backs off and settles on its own.
func TestDecisionsPassKeepsSettledStateSeparate(t *testing.T) {
	up := newSystemOneUpstream(t)
	up.answerPath(systemOnePathValue, http.StatusOK, systemOneYesBody)
	up.answerPath(decisionsPathValue, http.StatusServiceUnavailable, `{}`)
	_, port := up.hostPort(t)
	runner, st, _ := systemOneRunner(t, up, systemOneApp(routing.ProviderVLLM, port, routing.APIFlavorDecisions), systemOneMapping("m1", "decider"))
	current := time.Unix(10_000, 0).UTC()
	runner.now = func() time.Time { return current }

	runCapabilityCycle(runner)
	if _, ok := st.capabilityOf("m1", routing.CapabilityDecisionOpenAI); ok {
		t.Fatal("a 503 produced a decision_openai row")
	}

	up.answerPath(decisionsPathValue, http.StatusOK, decisionsYesBody)
	current = current.Add(time.Hour)
	runCapabilityCycle(runner)
	st.assertCapability(t, "m1", routing.CapabilityDecisionOpenAI, routing.CapabilityYes, routing.CapabilitySourceDecisionsProbe)
	if got := countPath(up, systemOnePathValue); got != 1 {
		t.Fatalf("System One probed %d times, want 1 (settled)", got)
	}

	// And the other way round: the OpenAI probe being settled does not stop a
	// System One probe whose row was cleared.
	st.mu.Lock()
	delete(st.caps["m1"], routing.CapabilityDecision)
	st.mu.Unlock()
	runCapabilityCycle(runner)
	if got := countPath(up, systemOnePathValue); got != 2 {
		t.Fatalf("System One probed %d times after its row was cleared, want 2", got)
	}
	if got := countPath(up, decisionsPathValue); got != 2 {
		t.Fatalf("decisions probed %d times, want 2 (503, then yes; settled since)", got)
	}
}

// TestDecisionsPassReprobesAClearedRow: a cleared decision_openai row is
// probed again.
func TestDecisionsPassReprobesAClearedRow(t *testing.T) {
	up := newSystemOneUpstream(t)
	up.answerPath(decisionsPathValue, http.StatusOK, decisionsYesBody)
	_, port := up.hostPort(t)
	runner, st, _ := systemOneRunner(t, up, systemOneApp(routing.ProviderVLLM, port, routing.APIFlavorDecisions), systemOneMapping("m1", "decider"))

	runCapabilityCycle(runner)
	st.mu.Lock()
	delete(st.caps["m1"], routing.CapabilityDecisionOpenAI)
	st.mu.Unlock()
	runCapabilityCycle(runner)

	if got := countPath(up, decisionsPathValue); got != 2 {
		t.Fatalf("decisions probed %d times, want 2", got)
	}
	st.assertCapability(t, "m1", routing.CapabilityDecisionOpenAI, routing.CapabilityYes, routing.CapabilitySourceDecisionsProbe)
	runCapabilityCycle(runner)
	if got := countPath(up, decisionsPathValue); got != 2 {
		t.Fatalf("with the row back, decisions probed %d times, want still 2", got)
	}
}

// TestDecisionsPassDoesNotOverwriteAManualYes: a probe no never replaces an
// operator's yes.
func TestDecisionsPassDoesNotOverwriteAManualYes(t *testing.T) {
	up := newSystemOneUpstream(t)
	up.answerPath(decisionsPathValue, http.StatusNotFound, `{"error":"not found"}`)
	_, port := up.hostPort(t)
	runner, st, _ := systemOneRunner(t, up, systemOneApp(routing.ProviderVLLM, port, routing.APIFlavorDecisions), systemOneMapping("m1", "decider"))
	st.seedCapability("m1", routing.CapabilityDecisionOpenAI, routing.CapabilityYes, routing.CapabilitySourceManual)

	runCapabilityCycle(runner)

	if got := countPath(up, decisionsPathValue); got != 1 {
		t.Fatalf("decisions probed %d times, want 1", got)
	}
	st.assertCapability(t, "m1", routing.CapabilityDecisionOpenAI, routing.CapabilityYes, routing.CapabilitySourceManual)
}

// TestDecisionsPassRefusalEscalatesIndependently: a refusal of the OpenAI
// probe opens its own escalating window and does not touch System One's.
func TestDecisionsPassRefusalEscalatesIndependently(t *testing.T) {
	up := newSystemOneUpstream(t)
	up.answerPath(systemOnePathValue, http.StatusBadRequest, `{}`)
	up.answerPath(decisionsPathValue, http.StatusBadRequest, `{}`)
	_, port := up.hostPort(t)
	runner, _, _ := systemOneRunner(t, up, systemOneApp(routing.ProviderVLLM, port, routing.APIFlavorDecisions), systemOneMapping("m1", "decider"))
	current := time.Unix(10_000, 0).UTC()
	runner.now = func() time.Time { return current }

	runCapabilityCycle(runner)
	for i, window := range []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute} {
		current = current.Add(window - time.Second)
		runCapabilityCycle(runner)
		if countPath(up, decisionsPathValue) != i+1 || countPath(up, systemOnePathValue) != i+1 {
			t.Fatalf("step %d: inside the window saw %v", i, pathsSeen(up))
		}
		current = current.Add(time.Second)
		runCapabilityCycle(runner)
		if countPath(up, decisionsPathValue) != i+2 || countPath(up, systemOnePathValue) != i+2 {
			t.Fatalf("step %d: at the window end saw %v, want both probed once more", i, pathsSeen(up))
		}
	}
}

// countDecisionProbeUnseals swaps the decision probes' credential unseal for a
// counting one for the test's duration.
func countDecisionProbeUnseals(t *testing.T) *int {
	t.Helper()
	orig := decisionProbeOpenSecret
	n := 0
	decisionProbeOpenSecret = func(c *capture.Cipher, stored string) (string, error) {
		n++
		return orig(c, stored)
	}
	t.Cleanup(func() { decisionProbeOpenSecret = orig })
	return &n
}

// TestDecisionPassUnsealsTheCredentialOnlyWhenAProbeIsDue: once both probes of
// a mapping settled, a later pass sends nothing and must not unseal the
// application's credential either; the first pass, which probes, unseals it
// once for both probes.
func TestDecisionPassUnsealsTheCredentialOnlyWhenAProbeIsDue(t *testing.T) {
	up := newSystemOneUpstream(t)
	up.answerPath(systemOnePathValue, http.StatusOK, systemOneYesBody)
	up.answerPath(decisionsPathValue, http.StatusOK, decisionsYesBody)
	_, port := up.hostPort(t)
	app := systemOneApp(routing.ProviderVLLM, port, routing.APIFlavorDecisions)
	app.APIToken = "plain:app-tok"
	runner, _, _ := systemOneRunner(t, up, app, systemOneMapping("m1", "decider"))
	unseals := countDecisionProbeUnseals(t)

	runCapabilityCycle(runner)
	if *unseals != 1 || len(up.requests()) != 2 {
		t.Fatalf("first pass: unseals = %d, upstream saw %v; want one unseal and both probes", *unseals, pathsSeen(up))
	}

	*unseals = 0
	runCapabilityCycle(runner)
	if len(up.requests()) != 2 {
		t.Fatalf("second pass probed again: upstream saw %v", pathsSeen(up))
	}
	if *unseals != 0 {
		t.Fatalf("second pass: unseals = %d, want 0 -- no probe was due", *unseals)
	}
}
