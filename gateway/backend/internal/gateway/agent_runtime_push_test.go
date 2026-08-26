// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"op-ai-gateway/internal/portal"
	"testing"
	"time"
)

// waitForFrame polls c.out for up to 2s. PushRuntimeConfig is asynchronous (a
// goroutine), so an immediate post-call assertion would be flaky -- this
// mirrors TestAgentStreamStoresCertReport's identical deadline-poll shape for
// the WS transport (agent_cert_ingest_test.go), and this repository has a
// documented history of fixing exactly this class of flake (task brief). Used
// where a frame IS expected to eventually arrive.
func waitForFrame(c *agentStreamConn) ([]byte, bool) {
	return waitForFrameWithin(c, 2*time.Second)
}

// confirmNoFrameArrives polls c.out for a SHORT window and reports whether a
// frame arrived. Used where the gate under test (undeclared feature / file
// mode) means PushRuntimeConfig's goroutine returns immediately with no I/O,
// so a short window is enough to distinguish "correctly skipped" from "just
// hasn't landed yet" without paying waitForFrame's full 2s on every negative
// assertion.
func confirmNoFrameArrives(c *agentStreamConn) ([]byte, bool) {
	return waitForFrameWithin(c, 200*time.Millisecond)
}

func waitForFrameWithin(c *agentStreamConn, timeout time.Duration) ([]byte, bool) {
	deadline := time.Now().Add(timeout)
	for {
		select {
		case raw := <-c.out:
			return raw, true
		default:
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestPushRuntimeConfigSkipsUndeclaredAgent is the Task 8 feature-gate proof:
// a connected agent that has never declared the runtime_manager feature gets
// NO frame at all, and one that has gets the exact document
// s.Portal.AgentRuntimeConfig would hand back to a Task-7 poll.
func TestPushRuntimeConfigSkipsUndeclaredAgent(t *testing.T) {
	const serverID = "mock-host-qwen"
	dto := sampleAgentRuntimeConfigDTO()
	srv := NewTestServer()
	srv.Portal = &fakePortalAgentRuntimeConfig{dto: dto}

	conn := &agentStreamConn{out: make(chan []byte, agentStreamQueueCapacity)}
	srv.AgentStreams.add(serverID, conn)

	// Undeclared: no push, even after waiting out the async goroutine.
	srv.PushRuntimeConfig(serverID)
	if raw, ok := confirmNoFrameArrives(conn); ok {
		t.Fatalf("an agent that never declared runtime_manager got a push: %s", raw)
	}

	// Declared: the push arrives with the exact document.
	srv.AgentFeatures.Set(serverID, []string{"runtime_manager"})
	srv.PushRuntimeConfig(serverID)
	raw, ok := waitForFrame(conn)
	if !ok {
		t.Fatal("a declared agent never received the runtime_config push")
	}
	var f streamFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if f.Type != "runtime_config" {
		t.Fatalf("type = %q, want runtime_config", f.Type)
	}
	var got portal.AgentRuntimeConfigDTO
	if err := json.Unmarshal(f.Data, &got); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if got.ETag != dto.ETag || got.RouterListen != dto.RouterListen || len(got.Specs) != len(dto.Specs) {
		t.Fatalf("pushed dto = %+v, want %+v", got, dto)
	}
}

// TestPushRuntimeConfigSkipsFileModeAgent: a server whose agent is running in
// file mode manages its runtime from local disk config, not this push/pull
// loop -- PushRuntimeConfig must withhold delivery even though the agent has
// otherwise declared runtime_manager.
func TestPushRuntimeConfigSkipsFileModeAgent(t *testing.T) {
	const serverID = "mock-host-qwen"
	srv := NewTestServer()
	srv.Portal = &fakePortalAgentRuntimeConfig{dto: sampleAgentRuntimeConfigDTO()}
	conn := &agentStreamConn{out: make(chan []byte, agentStreamQueueCapacity)}
	srv.AgentStreams.add(serverID, conn)
	srv.AgentFeatures.Set(serverID, []string{"runtime_manager"})
	srv.RuntimeStatus.SetFileMode(serverID, true)

	srv.PushRuntimeConfig(serverID)
	if raw, ok := confirmNoFrameArrives(conn); ok {
		t.Fatalf("a file-mode agent got a push: %s", raw)
	}
}

// TestPushRuntimeConfigDoesNotBlockCaller proves PushRuntimeConfig returns
// before the portal read/marshal/enqueue work completes -- the documented
// contract that lets the portal write-path hook call it while still holding
// its own lock (see the method's doc comment).
func TestPushRuntimeConfigDoesNotBlockCaller(t *testing.T) {
	const serverID = "mock-host-qwen"
	release := make(chan struct{})
	srv := NewTestServer()
	srv.Portal = &blockingPortalAgentRuntimeConfig{
		fakePortalAgentRuntimeConfig: fakePortalAgentRuntimeConfig{dto: sampleAgentRuntimeConfigDTO()},
		release:                      release,
	}
	conn := &agentStreamConn{out: make(chan []byte, agentStreamQueueCapacity)}
	srv.AgentStreams.add(serverID, conn)
	srv.AgentFeatures.Set(serverID, []string{"runtime_manager"})

	done := make(chan struct{})
	go func() {
		srv.PushRuntimeConfig(serverID)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PushRuntimeConfig did not return promptly -- it must launch its work in a goroutine")
	}
	select {
	case <-conn.out:
		t.Fatal("the frame must not be enqueued before the blocked portal call returns")
	default:
	}
	close(release)
	if _, ok := waitForFrame(conn); !ok {
		t.Fatal("the frame must arrive once the portal call unblocks")
	}
}

// blockingPortalAgentRuntimeConfig wraps fakePortalAgentRuntimeConfig so a
// test can hold PushRuntimeConfig's internal goroutine mid-flight (blocked on
// the portal read) while asserting nothing has reached the wire yet.
type blockingPortalAgentRuntimeConfig struct {
	fakePortalAgentRuntimeConfig
	release chan struct{}
}

func (f *blockingPortalAgentRuntimeConfig) AgentRuntimeConfig(ctx context.Context, serverID string) (portal.AgentRuntimeConfigDTO, error) {
	<-f.release
	return f.fakePortalAgentRuntimeConfig.AgentRuntimeConfig(ctx, serverID)
}

// ctxDoneSignalingPortal's AgentRuntimeConfig blocks until its ctx argument
// is Done, then closes returned. It never returns on its own -- only ctx
// expiring (or cancellation) unblocks it -- so it proves whatever timeout
// PushRuntimeConfig applies to the context it passes in, rather than proving
// anything about the fake's own behavior.
type ctxDoneSignalingPortal struct {
	fakePortalAgentRuntimeConfig
	returned chan struct{}
}

func (f *ctxDoneSignalingPortal) AgentRuntimeConfig(ctx context.Context, _ string) (portal.AgentRuntimeConfigDTO, error) {
	<-ctx.Done()
	close(f.returned)
	return portal.AgentRuntimeConfigDTO{}, ctx.Err()
}

// TestPushRuntimeConfigBoundsThePortalReadWithATimeout is the Important-3
// review fix's covering test: PushRuntimeConfig must never hand
// s.Portal.AgentRuntimeConfig a context.Background() with no deadline --
// one goroutine is spawned per portal write, so a stuck store read
// (lock contention, a wedged query) would otherwise accumulate goroutines
// without bound under sustained write pressure.
//
// pushRuntimeConfigTimeout is shrunk to milliseconds for the duration of
// this test (the established chat_runs.go runCheckpointInterval
// shrink-in-tests pattern, restored via defer) so this test does not need to
// sleep out the real 5s production budget: if PushRuntimeConfig is bounding
// the call correctly, ctxDoneSignalingPortal's ctx.Done() fires almost
// immediately; if a regression reintroduced context.Background(), ctx.Done()
// would never fire and this test would time out waiting for `returned`.
func TestPushRuntimeConfigBoundsThePortalReadWithATimeout(t *testing.T) {
	oldTimeout := pushRuntimeConfigTimeout
	pushRuntimeConfigTimeout = 20 * time.Millisecond
	defer func() { pushRuntimeConfigTimeout = oldTimeout }()

	const serverID = "mock-host-qwen"
	fake := &ctxDoneSignalingPortal{returned: make(chan struct{})}
	srv := NewTestServer()
	srv.Portal = fake
	srv.AgentFeatures.Set(serverID, []string{"runtime_manager"})

	srv.PushRuntimeConfig(serverID)

	select {
	case <-fake.returned:
		// The context PushRuntimeConfig passed in reached Done on its own --
		// proof it carries a deadline, not context.Background().
	case <-time.After(2 * time.Second):
		t.Fatal("s.Portal.AgentRuntimeConfig's context was never canceled within 2s of a 20ms " +
			"pushRuntimeConfigTimeout -- PushRuntimeConfig must bound the call with " +
			"context.WithTimeout, never a bare context.Background()")
	}
}
