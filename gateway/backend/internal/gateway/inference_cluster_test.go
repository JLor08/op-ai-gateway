// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync"
	"testing"
	"time"
)

// The gateway side of the cluster gate: the verdict routing reads, the demand
// that starts a cold cluster and wakes the requests parked for it, the retry
// of a request the agent refused because its head was not ready, and the
// voice-chat gate's start of a head.

// fakeClusterHeads answers which mappings are cluster heads.
type fakeClusterHeads map[string]string

func (f fakeClusterHeads) clusterHead(_ context.Context, _, mappingID string) (string, bool) {
	head, ok := f[mappingID]
	return head, ok
}

// activeMembers marks every member server of f's plan active and healthy, as
// the store reports a member routing may use.
func (f *orchFixture) activeMembers() {
	f.portal.mu.Lock()
	defer f.portal.mu.Unlock()
	for i := range f.portal.plan.Members {
		f.portal.plan.Members[i].Server.Status = routing.ServerStatusActive
		f.portal.plan.Members[i].Server.HealthStatus = routing.HealthHealthy
	}
}

func newTestClusterGate(f *orchFixture, busy routing.ServerBusyChecker) *clusterRoutingGate {
	return &clusterRoutingGate{
		heads:    fakeClusterHeads{"map_head": orchHead},
		clusters: func() *clusterOrchestrator { return f.o },
		busy:     busy,
	}
}

// The verdict per state: no head, cold (parkable with the cluster's bound:
// the head's startup timeout plus its slowest member's), formed, cooling down
// after a failed start, withheld by the runtime documents, and a member
// server routing may not use.
func TestClusterGateVerdicts(t *testing.T) {
	t.Run("no head", func(t *testing.T) {
		f := newOrchFixture(t, 1, "relay", "lan")
		if v := newTestClusterGate(f, nil).ClusterGateState(context.Background(), orchHeadServer, "map_other"); v.State != routing.ClusterGateNone {
			t.Fatalf("verdict = %+v, want none for a mapping that is no head", v)
		}
	})
	t.Run("cold, then formed", func(t *testing.T) {
		f := newOrchFixture(t, 2, "relay", "lan")
		f.portal.plan.Members[1].StartupTimeoutSeconds = 8
		f.activeMembers()
		g := newTestClusterGate(f, nil)
		v := g.ClusterGateState(context.Background(), orchHeadServer, "map_head")
		// 10 units for the head plus 8 for the slowest member, 10 ms each,
		// and the start's configuration wait (300 ms) and margins (20 + 50 ms).
		if v.State != routing.ClusterGateParkable || v.Head != orchHead || v.ParkBound != 550*time.Millisecond {
			t.Fatalf("cold verdict = %+v, want parkable for %s with a 550ms bound", v, orchHead)
		}
		f.ackAll()
		f.holdOnHeadEnsure(0, 1)
		if err := f.o.Ensure(context.Background(), orchHead); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		if v := g.ClusterGateState(context.Background(), orchHeadServer, "map_head"); v.State != routing.ClusterGateFormed {
			t.Fatalf("formed verdict = %+v, want formed", v)
		}
	})
	t.Run("cooling down", func(t *testing.T) {
		f := newOrchFixture(t, 1, "relay", "lan")
		f.activeMembers()
		f.ackAll()
		f.agent.memberErr[orchMemberSpec(0)] = orchRefusal(http.StatusServiceUnavailable, "runtime.admission_blocked")
		if err := f.o.Ensure(context.Background(), orchHead); err == nil {
			t.Fatal("Ensure succeeded, want the member's refusal")
		}
		v := newTestClusterGate(f, nil).ClusterGateState(context.Background(), orchHeadServer, "map_head")
		if v.State != routing.ClusterGateExcluded || v.Code != codeClusterStartFailed {
			t.Fatalf("verdict = %+v, want excluded with %s", v, codeClusterStartFailed)
		}
		// The verdict names the recorded failure: its cause and the member.
		if v.Cause != "runtime.admission_blocked" || v.Position == nil || *v.Position != 0 || v.AtHead {
			t.Fatalf("verdict = %+v, want the cause runtime.admission_blocked at member 0", v)
		}
	})
	t.Run("cooling down after a lost member", func(t *testing.T) {
		f := newOrchFixture(t, 1, "relay", "lan")
		f.activeMembers()
		f.ackAll()
		f.holdOnHeadEnsure(0)
		if err := f.o.Ensure(context.Background(), orchHead); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		f.o.memberLost(orchHead, 0, evidenceExited, false)
		v := newTestClusterGate(f, nil).ClusterGateState(context.Background(), orchHeadServer, "map_head")
		if v.State != routing.ClusterGateExcluded || v.Code != codeClusterStartFailed || v.Cause != codeClusterMemberLost ||
			v.Position == nil || *v.Position != 0 {
			t.Fatalf("verdict = %+v, want excluded with %s, cause %s at member 0", v, codeClusterStartFailed, codeClusterMemberLost)
		}
		f.o.wait()
	})
	t.Run("blocked by a terminal cause", func(t *testing.T) {
		f := newOrchFixture(t, 1, "relay", "lan")
		f.activeMembers()
		f.ackAll()
		f.agent.memberErr[orchMemberSpec(0)] = orchRefusal(http.StatusConflict, codeClusterVersionMismatch)
		if err := f.o.Ensure(context.Background(), orchHead); err == nil {
			t.Fatal("Ensure succeeded, want the member's refusal")
		}
		v := newTestClusterGate(f, nil).ClusterGateState(context.Background(), orchHeadServer, "map_head")
		if v.State != routing.ClusterGateExcluded || v.Code != codeClusterUnavailable || v.Cause != codeClusterVersionMismatch ||
			v.Position == nil || *v.Position != 0 {
			t.Fatalf("verdict = %+v, want excluded with %s, cause %s at member 0", v, codeClusterUnavailable, codeClusterVersionMismatch)
		}
	})
	t.Run("withheld", func(t *testing.T) {
		f := newOrchFixture(t, 1, "relay", "lan")
		f.portal.plan.WithheldReason = "runtime_config.agent_features_pending"
		v := newTestClusterGate(f, nil).ClusterGateState(context.Background(), orchHeadServer, "map_head")
		if v.State != routing.ClusterGateExcluded || v.Code != codeClusterUnavailable {
			t.Fatalf("verdict = %+v, want excluded with %s", v, codeClusterUnavailable)
		}
	})
	t.Run("member server held by a benchmark", func(t *testing.T) {
		f := newOrchFixture(t, 1, "relay", "lan")
		f.activeMembers()
		v := newTestClusterGate(f, fakeServerBusy{orchMemberServer(0): true}).ClusterGateState(context.Background(), orchHeadServer, "map_head")
		if v.State != routing.ClusterGateExcluded || v.Code != codeClusterUnavailable {
			t.Fatalf("verdict = %+v, want excluded with %s", v, codeClusterUnavailable)
		}
	})
	t.Run("member server not selectable", func(t *testing.T) {
		f := newOrchFixture(t, 1, "relay", "lan")
		f.activeMembers()
		f.portal.plan.Members[0].Server.HealthStatus = routing.HealthUnhealthy
		v := newTestClusterGate(f, nil).ClusterGateState(context.Background(), orchHeadServer, "map_head")
		if v.State != routing.ClusterGateExcluded || v.Code != codeClusterUnavailable {
			t.Fatalf("verdict = %+v, want excluded with %s", v, codeClusterUnavailable)
		}
	})
}

type fakeServerBusy map[string]bool

func (f fakeServerBusy) ServerBusy(serverID string) bool { return f[serverID] }

// A demand naming a head starts its cluster in the background -- one start
// however often it is signalled -- and once the start ended the requests
// parked on the head's server are woken. A head cooling down is not started.
func TestClusterDemandStartsTheClusterAndWakesParkedRequests(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	f.ackAll()
	gate := make(chan struct{})
	f.agent.memberGate[orchMemberSpec(0)] = gate
	f.portal.plan.Members[0].StartupTimeoutSeconds = 500
	f.holdOnHeadEnsure(0)
	woke := make(chan string, 4)
	f.o.deps.wake = func(serverID string) { woke <- serverID }

	f.o.Signal(routing.DemandSignal{Model: orchHeadModel, ClusterHead: orchHead})
	f.o.Signal(routing.DemandSignal{Model: orchHeadModel, ClusterHead: orchHead})
	if f.o.State(orchHead) != clusterForming {
		t.Fatalf("state = %v, want forming right after the signal", f.o.State(orchHead))
	}
	close(gate)
	select {
	case id := <-woke:
		if id != orchHeadServer {
			t.Fatalf("woke %q, want the head's server", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parked requests were never woken")
	}
	f.o.wait()
	if f.o.State(orchHead) != clusterFormed || f.agent.count("head:ensure:") != 1 {
		t.Fatalf("state %v, head ensures %d; want formed by one start", f.o.State(orchHead), f.agent.count("head:ensure:"))
	}

	// A failed start cools the head down; a signal then starts nothing.
	g := newOrchFixture(t, 1, "relay", "lan")
	g.ackAll()
	g.agent.memberErr[orchMemberSpec(0)] = orchRefusal(http.StatusServiceUnavailable, "runtime.admission_blocked")
	if err := g.o.Ensure(context.Background(), orchHead); err == nil {
		t.Fatal("Ensure succeeded, want the member's refusal")
	}
	before := g.agent.count("member:ensure:")
	g.o.Signal(routing.DemandSignal{Model: orchHeadModel, ClusterHead: orchHead})
	g.o.wait()
	if got := g.agent.count("member:ensure:"); got != before {
		t.Fatalf("member ensures %d -> %d after a signal during the cooldown, want none", before, got)
	}
}

// The resolver's one demand sink sends a cluster demand to the orchestration
// and every other demand to the instance scaler.
func TestDemandSinkFansOutByClusterHead(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	scaler := &recordingDemandSink{}
	sink := demandFanout{scaler: scaler, clusters: func() *clusterOrchestrator { return f.o }}

	sink.Signal(routing.DemandSignal{Model: "small", MappingIDs: []string{"map_small"}})
	sink.Signal(routing.DemandSignal{Model: orchHeadModel, ClusterHead: orchHead})
	if got := scaler.signals(); len(got) != 1 || got[0].Model != "small" {
		t.Fatalf("scaler got %+v, want the instance demand only", got)
	}
	if f.o.State(orchHead) != clusterForming {
		t.Fatalf("state = %v, want the cluster demand to start the head", f.o.State(orchHead))
	}
	_ = f.o.Stop(context.Background(), orchHead, false) // the start only needs to end
}

// clusterNotReadyProvider answers its first refusals calls -- buffered or
// streamed -- with the agent's 503 runtime.cluster_not_ready, as a head that
// stopped between the selection and the dispatch does, and serves after that.
type clusterNotReadyProvider struct {
	mu       sync.Mutex
	refusals int
	calls    int
}

func (p *clusterNotReadyProvider) refuse() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.calls <= p.refusals
}

func (p *clusterNotReadyProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *clusterNotReadyProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	if p.refuse() {
		return provider.Response{}, orchRefusal(http.StatusServiceUnavailable, codeClusterNotReady)
	}
	return provider.Response{Text: "served by the head", Usage: inference.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}}, nil
}

func (p *clusterNotReadyProvider) CompleteStream(_ context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	if p.refuse() {
		return orchRefusal(http.StatusServiceUnavailable, codeClusterNotReady)
	}
	if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: "served by the head"}); err != nil {
		return err
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted})
}

// clusterHeadServer is the stream test server with a cluster head seeded --
// map_head on srv_head, its spec a head -- and the orchestration replaced by
// f's, whose cluster has formed.
func clusterHeadServer(t *testing.T, prov provider.Client) (*Server, *orchFixture) {
	t.Helper()
	srv, f := coldClusterHeadServer(t, prov)
	if err := f.o.Ensure(context.Background(), orchHead); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	return srv, f
}

// coldClusterHeadServer is clusterHeadServer with the cluster still cold.
func coldClusterHeadServer(t *testing.T, prov provider.Client) (*Server, *orchFixture) {
	t.Helper()
	srv := newStreamTestServerWithProvider(prov)
	ctx := context.Background()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	must("CreateAIServer", srv.Routes.CreateAIServer(ctx, routing.AIServer{ID: orchHeadServer, Name: "Head", Domain: "head.example.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	must("UpsertTelemetry", srv.Routes.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: orchHeadServer, ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}))
	must("CreateApplication", srv.Routes.CreateApplication(ctx, routing.Application{ID: "app_head", ServerID: orchHeadServer, Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must("CreateMapping", srv.Routes.CreateMapping(ctx, routing.ModelMapping{ID: "map_head", ApplicationID: "app_head", GatewayModelName: orchHeadModel, AppModelName: orchHeadModel, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must("UpsertRuntimeSpec", srv.Routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: orchHead, MappingID: "map_head", Enabled: true, ClusterEnabled: true, StartupTimeoutSeconds: 10, Binary: "/opt/bin/llama-server", Args: "[]", Env: "{}", APIFlavors: []string{routing.APIFlavorOpenAI}, CreatedAt: now, UpdatedAt: now}))

	srv.AgentFeatures.Set(orchHeadServer, []string{rpcClusterFeature})
	f := newOrchFixture(t, 1, "relay", "lan")
	f.activeMembers()
	f.ackAll()
	f.holdOnHeadEnsure(0)
	srv.clusters = f.o
	return srv, f
}

// A request for a cold cluster head parks, its demand forms the cluster
// members first, and it is served by the head once the cluster formed -- the
// head is never sent a request before.
func TestColdClusterRequestParksUntilFormed(t *testing.T) {
	prov := &clusterNotReadyProvider{}
	srv, f := coldClusterHeadServer(t, prov)
	gate := make(chan struct{})
	f.agent.memberGate[orchMemberSpec(0)] = gate
	f.portal.plan.Members[0].StartupTimeoutSeconds = 500
	f.portal.plan.Head.Spec.StartupTimeoutSeconds = 500

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- postChat(srv, `{"model":"`+orchHeadModel+`","messages":[{"role":"user","content":"hi"}]}`)
	}()
	waitFor(t, func() bool { return f.o.State(orchHead) == clusterForming })
	if prov.callCount() != 0 {
		t.Fatalf("provider calls = %d while the cluster forms, want none", prov.callCount())
	}
	close(gate)
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "served by the head") {
			t.Fatalf("answer = %d %s, want 200 from the head", rec.Code, rec.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the parked request was never served")
	}
	if prov.callCount() != 1 || f.agent.count("head:ensure:"+orchHeadModel+":cluster") != 1 {
		t.Fatalf("provider calls %d, cluster starts %d; want one each", prov.callCount(), f.agent.count("head:ensure:"))
	}
}

func postChat(srv *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-secret")
	req.Header.Set("Content-Type", jsonContentType)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// A request the agent refuses with runtime.cluster_not_ready -- the head
// stopped between the selection and the dispatch -- has the cluster started
// again and is retried on the same head once: never more, buffered and
// streamed alike.
func TestCompletionRetriesOnceOnClusterNotReady(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		stream bool
	}{
		{"buffered", `{"model":"` + orchHeadModel + `","messages":[{"role":"user","content":"hi"}]}`, false},
		{"streamed", `{"model":"` + orchHeadModel + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`, true},
	} {
		t.Run(tc.name+": retried once the cluster formed again", func(t *testing.T) {
			prov := &clusterNotReadyProvider{refusals: 1}
			srv, f := clusterHeadServer(t, prov)

			rec := postChat(srv, tc.body)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "served by the head") {
				t.Fatalf("answer = %d %s, want 200 from the head", rec.Code, rec.Body.String())
			}
			if prov.callCount() != 2 {
				t.Fatalf("provider calls = %d, want the refused one and its one retry", prov.callCount())
			}
			if got := f.agent.count("head:ensure:" + orchHeadModel + ":cluster"); got != 2 {
				t.Fatalf("cluster-start ensures = %d, want the first formation and the one the retry ran", got)
			}
		})
		t.Run(tc.name+": never more than once", func(t *testing.T) {
			prov := &clusterNotReadyProvider{refusals: 10}
			srv, _ := clusterHeadServer(t, prov)

			rec := postChat(srv, tc.body)
			assertClusterAnswer(t, rec, tc.stream, codeClusterNotReady)
			if prov.callCount() != 2 {
				t.Fatalf("provider calls = %d, want exactly one retry", prov.callCount())
			}
		})
		t.Run(tc.name+": the cluster fails to form again", func(t *testing.T) {
			prov := &clusterNotReadyProvider{refusals: 1}
			srv, f := clusterHeadServer(t, prov)
			f.agent.mu.Lock()
			f.agent.memberErr[orchMemberSpec(0)] = orchRefusal(http.StatusServiceUnavailable, "runtime.admission_blocked")
			f.agent.mu.Unlock()

			rec := postChat(srv, tc.body)
			assertClusterAnswer(t, rec, tc.stream, codeClusterStartFailed)
			if prov.callCount() != 1 {
				t.Fatalf("provider calls = %d, want no retry after the failed start", prov.callCount())
			}
		})
	}
}

// assertClusterAnswer checks a request answered with code: a buffered request
// as a 503 with that code, a streamed one in its error frame. Nothing was
// served.
func assertClusterAnswer(t *testing.T, rec *httptest.ResponseRecorder, stream bool, code string) {
	t.Helper()
	if strings.Contains(rec.Body.String(), "served by the head") {
		t.Fatalf("answer = %s, want no served content", rec.Body.String())
	}
	if stream {
		if !strings.Contains(rec.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("stream = %s, want an error frame with %s", rec.Body.String(), code)
		}
		return
	}
	var body apierror.Body
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if rec.Code != http.StatusServiceUnavailable || body.Error.Code != code {
		t.Fatalf("answer = %d %s, want 503 %s", rec.Code, rec.Body.String(), code)
	}
}

// The agent's refusals of a cluster head a request reached without a retry --
// a relay that does not retry, a server override the gate does not judge, the
// second refusal after the one retry -- are 503 with the agent's code, not 502
// provider.unavailable. A relay forwards the agent's envelope at its status;
// the images relay keeps that envelope rather than renaming its code.
func TestClusterRefusalsAnswer503WithTheirCode(t *testing.T) {
	for _, code := range []string{codeClusterNotReady, codeClusterMemberUnreachable} {
		err := orchRefusal(http.StatusServiceUnavailable, code)
		if got := completionHTTPStatus(err); got != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", code, got)
		}
		if got := completionErrorCode(err); got != code {
			t.Fatalf("%s: code = %q, want the agent's", code, got)
		}
		if body := completionErrorResponse(err); body.Error.Code != code || body.Error.Message == "provider unavailable" {
			t.Fatalf("%s: body = %+v, want the code with its own message", code, body)
		}
		envelope := []byte(`{"error":{"code":"` + code + `","message":"` + code + `"}}`)
		if body, ok := normalizeImagesUpstreamError(http.StatusServiceUnavailable, envelope); !ok || body.Error.Code != code {
			t.Fatalf("%s: images relay body = %+v, %v; want the agent's envelope kept", code, body, ok)
		}
		if body, ok := normalizeSpeechUpstreamError(envelope, http.StatusServiceUnavailable); !ok || !bytes.Equal(body, envelope) {
			t.Fatalf("%s: speech relay body = %s, %v; want the agent's envelope kept", code, body, ok)
		}
	}
	failed := &routing.ClusterUnavailableError{Code: codeClusterStartFailed, Cause: codeClusterMemberUnreachable, Head: orchHead}
	if completionHTTPStatus(failed) != http.StatusServiceUnavailable || completionErrorCode(failed) != codeClusterStartFailed ||
		!strings.Contains(completionErrorResponse(failed).Error.Message, codeClusterMemberUnreachable) {
		t.Fatalf("start failure answer = %d %q %q, want 503 %s naming the cause", completionHTTPStatus(failed), completionErrorCode(failed), completionErrorResponse(failed).Error.Message, codeClusterStartFailed)
	}
}

// A request for a model whose only server is a cluster head cooling down
// after a failed start is answered with the cluster's own code, a 503, not
// no_healthy_host.
func TestCompletionAnswersTheClusterCode(t *testing.T) {
	prov := &clusterNotReadyProvider{}
	srv, f := clusterHeadServer(t, prov)
	f.agent.memberErr[orchMemberSpec(0)] = orchRefusal(http.StatusServiceUnavailable, "runtime.admission_blocked")
	_ = f.o.Stop(context.Background(), orchHead, false) // leaves the head cold
	if err := f.o.Ensure(context.Background(), orchHead); err == nil {
		t.Fatal("Ensure succeeded, want the member's refusal")
	}

	rec := postChat(srv, `{"model":"`+orchHeadModel+`","messages":[{"role":"user","content":"hi"}]}`)
	var body apierror.Body
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if rec.Code != http.StatusServiceUnavailable || body.Error.Code != codeClusterStartFailed {
		t.Fatalf("answer = %d %s, want 503 %s", rec.Code, rec.Body.String(), codeClusterStartFailed)
	}
	// The message names the recorded failure: its cause and the member.
	if !strings.Contains(body.Error.Message, "runtime.admission_blocked") || !strings.Contains(body.Error.Message, "member 0") {
		t.Fatalf("message = %q, want the cause runtime.admission_blocked at member 0", body.Error.Message)
	}
	if prov.callCount() != 0 {
		t.Fatalf("provider calls = %d, want none", prov.callCount())
	}
}

// A request for a cluster head that cools down after the watchdog stopped it
// for a lost member is refused with the stable code, and its message says the
// cluster lost the member rather than that a start failed.
func TestCompletionNamesTheLostMember(t *testing.T) {
	prov := &clusterNotReadyProvider{}
	srv, f := clusterHeadServer(t, prov)
	f.o.memberLost(orchHead, 0, evidenceExited, false)
	f.o.wait()

	rec := postChat(srv, `{"model":"`+orchHeadModel+`","messages":[{"role":"user","content":"hi"}]}`)
	var body apierror.Body
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if rec.Code != http.StatusServiceUnavailable || body.Error.Code != codeClusterStartFailed {
		t.Fatalf("answer = %d %s, want 503 %s", rec.Code, rec.Body.String(), codeClusterStartFailed)
	}
	msg := body.Error.Message
	if !strings.Contains(msg, "lost a member") || strings.Contains(msg, "failed to start") ||
		!strings.Contains(msg, codeClusterMemberLost) || !strings.Contains(msg, "member 0") {
		t.Fatalf("message = %q, want the lost member 0 named with %s", msg, codeClusterMemberLost)
	}
	if prov.callCount() != 0 {
		t.Fatalf("provider calls = %d, want none", prov.callCount())
	}
}

// The message of a cluster refusal names where the recorded failure happened:
// the member's position, or the head.
func TestClusterUnavailableMessageNamesWhere(t *testing.T) {
	pos := 2
	for _, tc := range []struct {
		err  routing.ClusterUnavailableError
		want string
	}{
		{routing.ClusterUnavailableError{Code: codeClusterStartFailed, Cause: codeRuntimeStartTimeout, Position: &pos}, "(cause: runtime.start_timeout at member 2)"},
		{routing.ClusterUnavailableError{Code: codeClusterStartFailed, Cause: codeRuntimeStartFailed, AtHead: true}, "(cause: runtime.start_failed at the head)"},
		{routing.ClusterUnavailableError{Code: codeClusterStartFailed, Cause: codeClusterMemberUnreachable}, "(cause: runtime.cluster_member_unreachable)"},
		{routing.ClusterUnavailableError{Code: codeClusterStartFailed}, "retry shortly"},
	} {
		if got := clusterUnavailableMessage(&tc.err); !strings.HasSuffix(got, tc.want) {
			t.Fatalf("message of %+v = %q, want it to end with %q", tc.err, got, tc.want)
		}
	}
}

// recordingEnsurer is a plain ensure route that records what it was asked.
type recordingEnsurer struct {
	mu      sync.Mutex
	ensured []string
}

func (e *recordingEnsurer) EnsureRuntimeModel(_ context.Context, target routing.Target) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensured = append(e.ensured, target.ProviderModel)
	return nil
}

// The voice-chat gate starts a cluster head through the orchestration --
// members first, the head with the cluster-start mark -- never with the plain
// ensure, which the agent refuses for a head.
func TestVoiceGateUsesOrchestratorForHeads(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	f.ackAll()
	f.holdOnHeadEnsure(0)
	mem := routing.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	if err := mem.CreateAIServer(ctx, routing.AIServer{ID: orchHeadServer, Name: "Head", Domain: "head.example.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := mem.CreateApplication(ctx, routing.Application{ID: "app_head", ServerID: orchHeadServer, Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: "map_head", ApplicationID: "app_head", GatewayModelName: orchHeadModel, AppModelName: orchHeadModel, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := mem.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: orchHead, MappingID: "map_head", Enabled: true, ClusterEnabled: true, StartupTimeoutSeconds: 10, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertRuntimeSpec: %v", err)
	}
	srv := &Server{Routes: mem, clusters: f.o}
	plain := &recordingEnsurer{}
	target := routing.Target{RouteID: "map_head", ServerID: orchHeadServer, Provider: routing.ProviderServerAgent, ProviderModel: orchHeadModel, Timeout: time.Second}

	if err := srv.ensureVoiceTarget(context.Background(), plain, target); err != nil {
		t.Fatalf("ensureVoiceTarget: %v", err)
	}
	if len(plain.ensured) != 0 {
		t.Fatalf("plain ensures = %q, want none for a head", plain.ensured)
	}
	if f.agent.count("head:ensure:"+orchHeadModel+":cluster") != 1 || f.o.State(orchHead) != clusterFormed {
		t.Fatalf("calls %q, state %v; want one members-first start that formed", f.agent.recorded(), f.o.State(orchHead))
	}

	// A head whose start fails answers the cluster's error.
	g := newOrchFixture(t, 1, "relay", "lan")
	g.ackAll()
	g.agent.memberErr[orchMemberSpec(0)] = orchRefusal(http.StatusServiceUnavailable, "runtime.admission_blocked")
	srv.clusters = g.o
	err := srv.ensureVoiceTarget(context.Background(), plain, target)
	var ce *clusterError
	if !errors.As(err, &ce) || ce.Code != codeClusterStartFailed {
		t.Fatalf("ensureVoiceTarget err = %v, want the cluster's start failure", err)
	}
}

// singleSpecReader answers one spec for every mapping and counts the reads.
type singleSpecReader struct {
	mu    sync.Mutex
	spec  routing.RuntimeSpec
	reads int
}

func (c *singleSpecReader) RuntimeSpecByMapping(context.Context, string) (routing.RuntimeSpec, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	return c.spec, true, nil
}

// The cluster gate asks for a mapping's spec only on an agent that declares
// rpc_cluster: no other agent runs a head, and a mapping there costs no read.
func TestClusterHeadLookupNeedsRPCCluster(t *testing.T) {
	ctx := context.Background()
	store := &singleSpecReader{spec: routing.RuntimeSpec{ID: orchHead, ClusterEnabled: true}}
	features := newAgentFeaturesRegistry()
	src := newInstanceSource(store, features, newRuntimeStatusRegistry())

	if _, ok := src.clusterHead(ctx, orchHeadServer, "map_head"); ok || store.reads != 0 {
		t.Fatalf("head %v after %d reads, want no head and no read on an agent without rpc_cluster", ok, store.reads)
	}
	features.Set(orchHeadServer, []string{rpcClusterFeature})
	if head, ok := src.clusterHead(ctx, orchHeadServer, "map_head"); !ok || head != orchHead || store.reads != 1 {
		t.Fatalf("head %q, %v after %d reads, want %s from one read", head, ok, store.reads, orchHead)
	}
}

// blockingPlanPortal holds every ClusterPlan read until release closes, and
// counts them.
type blockingPlanPortal struct {
	*orchPortal
	release chan struct{}
	mu      sync.Mutex
	reads   int
}

func (p *blockingPlanPortal) ClusterPlan(ctx context.Context, head string) (portal.ClusterPlan, error) {
	p.mu.Lock()
	p.reads++
	p.mu.Unlock()
	<-p.release
	return p.orchPortal.ClusterPlan(ctx, head)
}

func (p *blockingPlanPortal) readCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reads
}

// The first gate view of a head is read once however many requests ask while
// that read is in flight: they wait for it rather than read the plan too.
func TestClusterGateViewFirstReadIsSingleFlight(t *testing.T) {
	f := newOrchFixture(t, 1, "relay", "lan")
	blocking := &blockingPlanPortal{orchPortal: f.portal, release: make(chan struct{})}
	f.o.deps.portal = blocking
	waiting := make(chan string, 8)
	f.o.viewWaitHook = func(head string) { waiting <- head }

	const askers = 5
	views := make(chan clusterGateView, askers)
	go func() { views <- f.o.gateView(context.Background(), orchHead) }()
	waitFor(t, func() bool { return blocking.readCount() == 1 })
	for range askers - 1 {
		go func() { views <- f.o.gateView(context.Background(), orchHead) }()
	}
	for range askers - 1 {
		<-waiting
	}
	close(blocking.release)
	for range askers {
		if v := <-views; v.bound != 520*time.Millisecond {
			t.Fatalf("view bound = %v, want the plan's 520ms", v.bound)
		}
	}
	if n := blocking.readCount(); n != 1 {
		t.Fatalf("plan reads = %d, want one", n)
	}
}
