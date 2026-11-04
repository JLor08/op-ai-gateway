// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
)

// TestAgentRuntimeConfigHoldsADocumentThatAwaitsTheAgentsFeatures pins the
// fetch answer for a held document (portal.AgentRuntimeConfigDTO.
// AwaitsAgentFeatures): a 503 with no document and no ETag, whatever the
// agent's If-None-Match. The agent's runtime-config source answers every
// status but 200 by keeping the document it applied, which is the point: a
// held document is one this gateway cannot yet derive for this agent.
func TestAgentRuntimeConfigHoldsADocumentThatAwaitsTheAgentsFeatures(t *testing.T) {
	dto := sampleAgentRuntimeConfigDTO()
	dto.AwaitsAgentFeatures = true
	fake := &fakePortalAgentRuntimeConfig{dto: dto}
	srv := agentRuntimeConfigTestServer(t, fake)
	for _, ifNoneMatch := range []string{"", `"` + dto.ETag + `"`, `"stale"`} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentRuntimeConfigRequest("runtime-secret", ifNoneMatch))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("If-None-Match %q: status = %d, want 503 (body %s)", ifNoneMatch, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("ETag"); got != "" {
			t.Fatalf("If-None-Match %q: ETag = %q on a held document, want none", ifNoneMatch, got)
		}
		if got := rec.Header().Get("Retry-After"); got == "" {
			t.Fatalf("If-None-Match %q: no Retry-After on a held document", ifNoneMatch)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("If-None-Match %q: Cache-Control = %q, want no-store", ifNoneMatch, got)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "runtime_config.agent_features_pending") || strings.Contains(body, `"specs"`) {
			t.Fatalf("If-None-Match %q: body = %s, want the pending code and no document", ifNoneMatch, body)
		}
	}
}

// TestPushRuntimeConfigHoldsADocumentThatAwaitsTheAgentsFeatures: the push
// path delivers no held document either, even to an agent that declared
// runtime_manager.
func TestPushRuntimeConfigHoldsADocumentThatAwaitsTheAgentsFeatures(t *testing.T) {
	const serverID = "mock-host-qwen"
	dto := sampleAgentRuntimeConfigDTO()
	dto.AwaitsAgentFeatures = true
	srv := NewTestServer()
	srv.Portal = &fakePortalAgentRuntimeConfig{dto: dto}
	conn := &agentStreamConn{out: make(chan []byte, agentStreamQueueCapacity)}
	srv.AgentStreams.add(serverID, conn)
	srv.AgentFeatures.Set(serverID, []string{"runtime_manager"})

	srv.PushRuntimeConfig(serverID)
	if raw, ok := confirmNoFrameArrives(conn); ok {
		t.Fatalf("a held document was pushed: %s", raw)
	}
}

// TestAgentRuntimeConfigOfAReplicableSpecWaitsForTheAgentsDeclaration drives
// the gateway-restart case end to end, through the real portal derivation
// wired to this server's own agent-features registry. The agent of a server
// with a replicable spec reconnects and fetches its document before its first
// telemetry sample: the gateway does not know yet whether it runs instances,
// so it holds the document rather than serving it the one without instance
// settings, which would make an agent that runs instances retire every
// instance beyond the first, the protected ones included. Once a sample
// declares runtime_instances the document carries the instance settings; an
// agent that declares only runtime_manager gets them withheld; a sample whose
// capabilities cannot be read holds the document again.
func TestAgentRuntimeConfigOfAReplicableSpecWaitsForTheAgentsDeclaration(t *testing.T) {
	const serverID = "mock-host-qwen"
	ctx := context.Background()
	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_hold", serverID, "hold-secret")
	seedRuntimeIngestSpec(t, srv, "rspec_hold")
	spec, _, err := srv.Routes.RuntimeSpecByID(ctx, "rspec_hold")
	if err != nil {
		t.Fatalf("RuntimeSpecByID: %v", err)
	}
	spec.Replicable, spec.MinInstances, spec.MaxInstances = true, 2, 3
	if err := srv.Routes.UpsertRuntimeSpec(ctx, spec); err != nil {
		t.Fatalf("make the spec replicable: %v", err)
	}
	directory := portal.NewMemoryDirectory(auth.NewTokenStore())
	srv.Portal = portal.NewService(portal.ServiceDeps{
		Users: directory, Tokens: directory, Usage: usage.NewRecorder(), Routes: srv.Routes, AgentFeatures: srv.AgentFeatures,
	})
	fetch := func(t *testing.T, step string) (int, map[string]any) {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentRuntimeConfigRequest("hold-secret", ""))
		if rec.Code != http.StatusOK {
			return rec.Code, nil
		}
		var doc struct {
			Specs []map[string]any `json:"specs"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || len(doc.Specs) != 1 {
			t.Fatalf("%s: document %s: %d specs, err %v", step, rec.Body.String(), len(doc.Specs), err)
		}
		return rec.Code, doc.Specs[0]
	}

	if code, _ := fetch(t, "before the first sample"); code != http.StatusServiceUnavailable {
		t.Fatalf("before the first sample: status = %d, want 503 (held)", code)
	}
	ingestInstanceRows(t, srv, `"runtime_manager","runtime_instances"`)
	code, pushed := fetch(t, "an agent that runs instances")
	if code != http.StatusOK || pushed["replicable"] != true || pushed["min_instances"] != float64(2) || pushed["max_instances"] != float64(3) {
		t.Fatalf("an agent that runs instances: status %d spec %v, want 200 with replicable 2..3", code, pushed)
	}
	ingestInstanceRows(t, srv, `"runtime_manager"`)
	code, pushed = fetch(t, "an older agent")
	if _, present := pushed["replicable"]; code != http.StatusOK || present {
		t.Fatalf("an older agent: status %d spec %v, want 200 without instance keys", code, pushed)
	}
	req, raw := ingestReq(t, `{"host":{"cpu_util_pct":1},"capabilities":"not-an-object"}`)
	if err := srv.ingestTelemetrySample(ctx, serverID, req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if code, _ := fetch(t, "after an unreadable declaration"); code != http.StatusServiceUnavailable {
		t.Fatalf("after an unreadable declaration: status = %d, want 503 (held)", code)
	}
}
