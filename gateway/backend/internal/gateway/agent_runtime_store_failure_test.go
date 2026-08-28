// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

// These tests are C1's covering tests, and they deliberately drive a REAL
// portal.Service (not fakePortalAgentRuntimeConfig) over a store whose
// AIServer read fails. The defect lived exactly in that seam: the portal
// collapsed every AIServerByID error into the empty runtime-config document
// WITH err == nil, so neither half of the gateway could tell a transient
// store blip from "this server runs nothing managed". A fake portal can only
// ever pin the gateway's reaction to an error it was handed; it cannot show
// that the error reaches the gateway at all.

// failAIServerByIDStore injects an AIServerByID failure onto an
// otherwise-real routing.Store (the portal package has an identically-shaped
// helper for its own layer's version of this test).
type failAIServerByIDStore struct {
	routing.Store
	err error
}

func (s *failAIServerByIDStore) AIServerByID(context.Context, string) (routing.AIServer, error) {
	return routing.AIServer{}, s.err
}

// ctxBoundAIServerByIDStore blocks the AIServer read until the caller's
// context is done and then returns its error -- what a store read slower than
// pushRuntimeConfigTimeout actually looks like from the portal's side. Using
// the context rather than a sleep keeps the test deterministic and fast.
type ctxBoundAIServerByIDStore struct {
	routing.Store
}

func (s *ctxBoundAIServerByIDStore) AIServerByID(ctx context.Context, _ string) (routing.AIServer, error) {
	<-ctx.Done()
	return routing.AIServer{}, ctx.Err()
}

// runtimeConfigPortalOver builds a real portal.Service reading through routes.
func runtimeConfigPortalOver(t *testing.T, routes routing.Store) portal.API {
	t.Helper()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	dir := portal.NewMemoryDirectory(nil)
	return portal.NewService(portal.ServiceDeps{
		Users:  dir,
		Groups: dir,
		Usage:  usage.NewRecorder(),
		Routes: routes,
		Clock:  func() time.Time { return now },
	})
}

// TestAgentRuntimeConfigStoreFailureIsA500NotTheEmptyDocument is the HTTP half
// of C1. A transient store failure must reach the agent as 500, which
// GatewaySource.Load's default branch answers by KEEPING its last known-good
// config. The 200 + empty document this used to serve is the one answer that
// is worse than any error: it parses, it carries a valid but DIFFERENT ETag,
// so the agent overwrites its disk cache with it, tears down its bound router
// listener and drains every running spec -- one dropped connection costing
// every model on the server.
func TestAgentRuntimeConfigStoreFailureIsA500NotTheEmptyDocument(t *testing.T) {
	transient := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_runtime_c1", "mock-host-qwen", "runtime-secret")
	srv.Portal = runtimeConfigPortalOver(t, &failAIServerByIDStore{Store: routing.NewMemoryStore(), err: transient})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, agentRuntimeConfigRequest("runtime-secret", ""))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d (body %s), want 500 -- a transient store failure must never be served as a well-formed empty runtime-config document", rec.Code, rec.Body.String())
	}
}

// TestPushRuntimeConfigNeverPushesTheEmptyDocumentOnAStoreFailure is the WS
// half of C1, and the more deterministic trigger of the two: PushRuntimeConfig
// bounds the portal read with pushRuntimeConfigTimeout, so a slow store
// yielded context.DeadlineExceeded -- which the portal turned into the empty
// document with err == nil, defeating this method's own explicit
// never-push-on-error guard. No HTTP status is involved on this path at all,
// so the agent's status-based discipline never gets a chance: it receives a
// well-formed document that removes everything.
func TestPushRuntimeConfigNeverPushesTheEmptyDocumentOnAStoreFailure(t *testing.T) {
	oldTimeout := pushRuntimeConfigTimeout
	pushRuntimeConfigTimeout = 20 * time.Millisecond
	defer func() { pushRuntimeConfigTimeout = oldTimeout }()

	const serverID = "mock-host-qwen"
	srv := NewTestServer()
	srv.Portal = runtimeConfigPortalOver(t, &ctxBoundAIServerByIDStore{Store: routing.NewMemoryStore()})
	conn := &agentStreamConn{out: make(chan []byte, agentStreamQueueCapacity)}
	srv.AgentStreams.add(serverID, conn)
	srv.AgentFeatures.Set(serverID, []string{"runtime_manager"})

	srv.PushRuntimeConfig(serverID)

	// Long enough for the shrunk 20ms deadline to fire and the push goroutine
	// to run to completion several times over.
	raw, ok := waitForFrameWithin(conn, 500*time.Millisecond)
	if ok {
		var f streamFrame
		if err := json.Unmarshal(raw, &f); err == nil {
			t.Fatalf("a store read that timed out produced a %q push: %s -- the agent would tear down its router and drain every spec", f.Type, f.Data)
		}
		t.Fatalf("a store read that timed out produced a push: %s", raw)
	}
}
