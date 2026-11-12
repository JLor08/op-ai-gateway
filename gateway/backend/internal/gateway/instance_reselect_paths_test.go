// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
	"testing"
	"time"
)

// Every path that sends a request to an agent instance re-selects a request whose
// instance is gone in the same way (instance_reselect.go). These tests run each of
// them -- the buffered and streamed translate paths, the Responses passthrough, speech,
// image generation and edit, and the buffered transcription -- through the same
// scenarios against the fake agent router.

// reselectPath is one way a request reaches an agent instance.
type reselectPath struct {
	name    string
	spec    string // the replicable spec serving the request
	mapping string // its mapping
	up      string // its upstream model: instance 0's address
	// answer is a substring of the 200 answer an instance gives.
	answer string
	// stream is true for the path that answers a failure in an SSE frame of a 200.
	stream bool
	// build makes the request.
	build func(t *testing.T) (path, contentType string, body []byte)
}

func jsonRequest(path, body string) func(*testing.T) (string, string, []byte) {
	return func(*testing.T) (string, string, []byte) { return path, jsonContentType, []byte(body) }
}

func multipartRequest(path string, parts func() []editPart) func(*testing.T) (string, string, []byte) {
	return func(t *testing.T) (string, string, []byte) {
		body, ct := buildEditMultipart(t, parts())
		return path, ct, body
	}
}

func reselectPaths() []reselectPath {
	chat := func(name, body string, stream bool, answer string) reselectPath {
		return reselectPath{name: name, spec: "spec-chat-inst", mapping: liveInstChatMap, up: liveInstChatUp, answer: answer, stream: stream, build: jsonRequest("/v1/chat/completions", body)}
	}
	return []reselectPath{
		chat("buffered chat", `{"model":"chat-inst","messages":[{"role":"user","content":"hi"}]}`, false, "served by chat-up"),
		chat("streamed chat", `{"model":"chat-inst","stream":true,"messages":[{"role":"user","content":"hi"}]}`, true, "served by chat-up"),
		{name: "responses passthrough", spec: "spec-chat-inst", mapping: liveInstChatMap, up: liveInstChatUp, answer: `"model":"chat-up"`, build: jsonRequest("/v1/responses", `{"model":"chat-inst","input":"hi"}`)},
		{name: "speech", spec: liveInstTTSSpec, mapping: liveInstTTSMap, up: liveInstTTSUp, answer: "RIFF", build: jsonRequest("/v1/audio/speech", `{"model":"tts-inst","input":"hello"}`)},
		{name: "systemone", spec: liveInstSysSpec, mapping: liveInstSysMap, up: liveInstSysUp, answer: `"answers"`, build: jsonRequest("/v1/systemone", `{"model":"sys-inst","state":"s","questions":{"q":{"type":"noul","instructions":"x"}}}`)},
		{name: "image generation", spec: liveInstImgSpec, mapping: liveInstImgMap, up: liveInstImgUp, answer: "b64_json", build: jsonRequest("/v1/images/generations", `{"model":"img-inst","prompt":"a cat"}`)},
		{name: "image edit", spec: liveInstImgSpec, mapping: liveInstImgMap, up: liveInstImgUp, answer: "b64_json", build: multipartRequest("/v1/images/edits", func() []editPart { return validEditParts("img-inst") })},
		{name: "transcription", spec: liveInstASRSpec, mapping: liveInstASRMap, up: liveInstASRUp, answer: "hello there", build: multipartRequest("/v1/audio/transcriptions", func() []editPart { return validTranscriptionParts("asr-inst") })},
	}
}

// publishSpecInstances reports spec's instances ks running: instance 0 under load
// and every other idle, so a selection prefers an instance k >= 1.
func publishSpecInstances(srv *Server, spec, up string, ks ...int) {
	rows := []RuntimeStatusDTO{liveInstanceRow(spec, up, 0, 10)}
	for _, k := range ks {
		rows = append(rows, liveInstanceRow(spec, up, k, 0))
	}
	srv.RuntimeStatus.publish(liveInstServer, rows)
}

// serveAsync sends p's request on its own goroutine and hands over the answer once it
// is written. The request ends with the test.
func serveAsync(t *testing.T, srv *Server, p reselectPath) <-chan *httptest.ResponseRecorder {
	t.Helper()
	path, ct, body := p.build(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-secret")
	req.Header.Set("Content-Type", ct)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		done <- rec
	}()
	return done
}

// answered waits for an answer from serveAsync.
func answered(t *testing.T, done <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case rec := <-done:
		return rec
	case <-time.After(5 * time.Second):
		t.Fatal("no answer")
		return nil
	}
}

// instanceRows lists the in-flight requests the active registry holds on key.
func instanceRows(srv *Server, key string) []string {
	var ids []string
	for _, row := range srv.Active.Snapshot() {
		if row.InstanceKey == key {
			ids = append(ids, row.ID)
		}
	}
	return ids
}

// A request whose instance is gone leaves it before it is selected again: when the
// re-selection has to wait in the admission queue for the next instance, the lost
// instance no longer counts it, and nothing of it is registered there. Once the next
// instance frees, the request is served there and recorded once. Every path alike.
func TestEveryReselectingPathLeavesTheLostInstanceFirst(t *testing.T) {
	for _, p := range reselectPaths() {
		t.Run(p.name, func(t *testing.T) {
			agent := newInstanceAgentRouter(t)
			srv := newInstanceRouterServer(t, agent, true)
			srv.Active.admission.recheck = 0 // only a release wakes the parked re-selection
			publishSpecInstances(srv, p.spec, p.up, 2)
			agent.setRefusal(routing.InstanceAddress(p.up, 2), http.StatusConflict, "runtime.instance_no_capacity")
			// Instance 0 at its parallelism of one: the request goes to instance 2, and its
			// re-selection waits for instance 0.
			busy := routing.InstanceKey(p.mapping, 0)
			srv.Active.Add(ActiveRequest{ID: "busy0", ServerID: liveInstServer, InstanceKey: busy})
			defer srv.Active.Remove("busy0")

			done := serveAsync(t, srv, p)
			waitForParked(t, srv.Active.admission, 1)
			lost := routing.InstanceKey(p.mapping, 2)
			if n, rows := srv.Active.InstanceActivity(lost), instanceRows(srv, lost); n != 0 || len(rows) != 0 {
				t.Fatalf("while the re-selection waits: %d in flight on the lost %s (rows %v), want none", n, lost, rows)
			}
			srv.Active.Remove("busy0")
			rec := answered(t, done)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), p.answer) || strings.Contains(rec.Body.String(), "runtime.") {
				t.Fatalf("status %d body %.300s; want 200 with %q from instance 0", rec.Code, rec.Body.String(), p.answer)
			}
			if served := agent.servedModels(); !slices.Equal(served, []string{routing.InstanceAddress(p.up, 2), p.up}) {
				t.Fatalf("addresses served = %v, want the lost instance once, then instance 0", served)
			}
			if rows := srv.Usage.All(); len(rows) != 1 || rows[0].Status != "success" || rows[0].RouteID != p.mapping {
				t.Fatalf("usage rows = %+v, want one success row for %s", rows, p.mapping)
			}
		})
	}
}

// A request is re-selected once: when the instance it was re-selected onto is gone
// too, it is not moved again, and the client gets the gateway's own answer for the
// refusal -- provider.unavailable, as a chat completion gets it -- never the agent
// router's envelope; one error row records it. Every path alike.
func TestEveryReselectingPathReselectsOnlyOnce(t *testing.T) {
	for _, p := range reselectPaths() {
		t.Run(p.name, func(t *testing.T) {
			agent := newInstanceAgentRouter(t)
			srv := newInstanceRouterServer(t, agent, true)
			publishSpecInstances(srv, p.spec, p.up, 2, 3)
			gone := []string{routing.InstanceAddress(p.up, 2), routing.InstanceAddress(p.up, 3)}
			for _, addr := range gone {
				agent.setRefusal(addr, http.StatusConflict, "runtime.instance_no_capacity")
			}
			// Instance 0 at its parallelism of one, so both selections land on a gone one.
			srv.Active.Add(ActiveRequest{ID: "busy0", ServerID: liveInstServer, InstanceKey: routing.InstanceKey(p.mapping, 0)})
			defer srv.Active.Remove("busy0")

			rec := answered(t, serveAsync(t, srv, p))
			body := rec.Body.String()
			wantStatus := http.StatusBadGateway
			if p.stream {
				wantStatus = http.StatusOK // the stream had begun: the error is its last frame
			}
			if rec.Code != wantStatus || !strings.Contains(body, "provider.unavailable") || strings.Contains(body, "runtime.") {
				t.Fatalf("status %d body %.300s; want %d with the gateway's provider.unavailable and no agent code", rec.Code, body, wantStatus)
			}
			served := agent.servedModels()
			slices.Sort(served)
			if !slices.Equal(served, gone) {
				t.Fatalf("addresses served = %v, want each gone instance once: %v", served, gone)
			}
			if rows := srv.Usage.All(); len(rows) != 1 || rows[0].Status != "error" || rows[0].ErrorCode != "provider.unavailable" {
				t.Fatalf("usage rows = %+v, want one error row provider.unavailable", rows)
			}
		})
	}
}

// A Responses passthrough whose re-selection cannot be relayed answers with the
// gateway's own error, not the gone instance's agent refusal: the routing refusal when
// the re-resolve is refused, and provider.unavailable when the instance it lands on
// serves the endpoint by translate -- which a relay that already chose to pass the
// client's body through cannot switch to.
func TestVanishedPassthroughThatCannotBeRelayedAgain(t *testing.T) {
	cases := []struct {
		name     string
		onGone   func(t *testing.T, srv *Server)
		status   int
		code     string
		recorded string
	}{
		{
			name: "the re-resolve is refused",
			onGone: func(t *testing.T, srv *Server) {
				server, err := srv.Routes.AIServerByID(context.Background(), liveInstServer)
				if err != nil {
					t.Errorf("AIServerByID: %v", err)
					return
				}
				server.HealthStatus = routing.HealthUnhealthy
				if err := srv.Routes.UpdateAIServer(context.Background(), server); err != nil {
					t.Errorf("UpdateAIServer: %v", err)
				}
			},
			status: http.StatusServiceUnavailable,
			code:   routing.ErrNoHealthyHost.Error(),
		},
		{
			name: "the instance re-selected serves the endpoint by translate",
			onGone: func(t *testing.T, srv *Server) {
				spec, _, err := srv.Routes.RuntimeSpecByMapping(context.Background(), liveInstChatMap)
				if err != nil {
					t.Errorf("RuntimeSpecByMapping: %v", err)
					return
				}
				spec.ResponsesMode = routing.EndpointModeTranslate
				if err := srv.Routes.UpsertRuntimeSpec(context.Background(), spec); err != nil {
					t.Errorf("UpsertRuntimeSpec: %v", err)
				}
			},
			status: http.StatusBadGateway,
			code:   "provider.unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := newInstanceAgentRouter(t)
			srv := newInstanceRouterServer(t, agent, true)
			publishChatInstances(srv, 2)
			agent.setRefusal("chat-up#2", http.StatusConflict, "runtime.instance_no_capacity")
			agent.setOnArrive(func(model string) {
				if model == "chat-up#2" {
					tc.onGone(t, srv)
				}
			})

			rec := postInstanceJSON(t, srv, "/v1/responses", `{"model":"chat-inst","input":"hi"}`)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) || strings.Contains(rec.Body.String(), "runtime.") {
				t.Fatalf("status %d body %s; want %d %s, not the agent's refusal", rec.Code, rec.Body.String(), tc.status, tc.code)
			}
			if served := agent.servedModels(); !slices.Equal(served, []string{"chat-up#2"}) {
				t.Fatalf("addresses served = %v, want only the gone instance", served)
			}
			if rows := srv.Usage.All(); len(rows) != 1 || rows[0].Status != "error" || rows[0].ErrorCode != tc.code {
				t.Fatalf("usage rows = %+v, want one error row %s", rows, tc.code)
			}
		})
	}
}

// A live session whose instance is gone leaves it -- its live slot and its active row
// -- before it is selected again, so while the re-selection waits for the next slot
// the lost instance is neither held nor counted; once a slot frees the session is
// served there with all its audio.
func TestVanishedLiveSessionLeavesItsInstanceFirst(t *testing.T) {
	agent := newInstanceAgentRouter(t)
	srv := newInstanceRouterServer(t, agent, true)
	srv.Active.admission.recheck = 0 // only a release wakes the parked re-selection
	publishSpecInstances(srv, liveInstASRSpec, liveInstASRUp, 2)
	agent.setRefusal("asr-up#2", http.StatusNotFound, "runtime.model_not_running")
	release0, _ := srv.liveSessions.acquire("route-asr-inst#0")
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	t.Cleanup(func() { // before the server closes: a session still parked is let go
		release0()
		wakeAdmission(srv)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	done := make(chan liveSessionResult, 1)
	go func() { done <- startLiveSession(ctx, ts, "asr-inst") }()
	waitForParked(t, srv.Active.admission, 1)
	const lost = "route-asr-inst#2"
	if srv.liveSessions.LiveHeld(lost) || srv.Active.InstanceActivity(lost) != 0 || len(instanceRows(srv, lost)) != 0 {
		t.Fatalf("while the re-selection waits: slot held %v, %d in flight on the lost %s; want neither", srv.liveSessions.LiveHeld(lost), srv.Active.InstanceActivity(lost), lost)
	}
	release0()
	wakeAdmission(srv)
	var s liveSessionResult
	select {
	case s = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the re-selected session was not admitted after instance 0's slot freed")
	}
	if s.err != nil || s.resp.StatusCode != http.StatusOK {
		t.Fatalf("re-selected session: err %v status %v, want 200", s.err, s.resp)
	}
	finishLiveSession(t, s)
	if served := agent.servedModels(); !slices.Equal(served, []string{"asr-up#2", "asr-up"}) {
		t.Fatalf("live addresses served = %v, want [asr-up#2 asr-up]", served)
	}
}

// A live session is re-selected once: when the instance it moved to is gone too, the
// client gets the gateway's provider.unavailable, not the agent's refusal, and one
// error row records it.
func TestVanishedLiveSessionReselectedOnlyOnce(t *testing.T) {
	agent := newInstanceAgentRouter(t)
	srv := newInstanceRouterServer(t, agent, true)
	publishSpecInstances(srv, liveInstASRSpec, liveInstASRUp, 2, 3)
	agent.setRefusal("asr-up#2", http.StatusNotFound, "runtime.model_not_running")
	agent.setRefusal("asr-up#3", http.StatusNotFound, "runtime.model_not_running")
	release0, _ := srv.liveSessions.acquire("route-asr-inst#0")
	defer release0()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	s := startLiveSession(ctx, ts, "asr-inst")
	if s.err != nil {
		t.Fatalf("live session: %v", s.err)
	}
	body := readAllClose(s.resp)
	_ = s.pw.Close()
	if s.resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "provider.unavailable") || strings.Contains(body, "runtime.") {
		t.Fatalf("status %d body %s; want 502 provider.unavailable, not the agent's refusal", s.resp.StatusCode, body)
	}
	served := agent.servedModels()
	slices.Sort(served)
	if !slices.Equal(served, []string{"asr-up#2", "asr-up#3"}) {
		t.Fatalf("live addresses served = %v, want each gone instance once", served)
	}
	waitForUsageRows(t, srv, 1)
	if rows := srv.Usage.All(); len(rows) != 1 || rows[0].Status != "error" || rows[0].ErrorCode != "provider.unavailable" {
		t.Fatalf("usage rows = %+v, want one error row provider.unavailable", rows)
	}
}

// readAllClose reads resp's whole body and closes it.
func readAllClose(resp *http.Response) string {
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return buf.String()
}

// waitForUsageRows waits until srv has recorded n usage rows: a live session's row is
// written by its relay's teardown, after the client has its answer.
func waitForUsageRows(t *testing.T, srv *Server, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(srv.Usage.All()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("usage rows = %d, want %d", len(srv.Usage.All()), n)
		}
		time.Sleep(time.Millisecond)
	}
}
