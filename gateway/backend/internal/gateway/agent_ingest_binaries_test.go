// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"net/url"
	"op-ai-gateway/internal/routing"
	"strconv"
	"strings"
	"testing"
)

const binariesTestServer = "mock-host-qwen"

func ingestBinaries(t *testing.T, srv *Server, binariesJSON string) {
	t.Helper()
	body := `{"host":{"cpu_util_pct":1}`
	if binariesJSON != "" {
		body += `,"binaries":` + binariesJSON
	}
	body += `}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), binariesTestServer, req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
}

func storedBinaries(t *testing.T, srv *Server) map[string]routing.ServerBinary {
	t.Helper()
	rows, err := srv.Routes.ServerBinaries(context.Background(), binariesTestServer)
	if err != nil {
		t.Fatalf("ServerBinaries: %v", err)
	}
	out := map[string]routing.ServerBinary{}
	for _, b := range rows {
		out[b.Path] = b
	}
	return out
}

func TestIngestReplacesAgentBinariesKeepsManual(t *testing.T) {
	srv := NewTestServer()
	ctx := context.Background()
	if err := srv.Routes.UpsertManualServerBinary(ctx, routing.ServerBinary{ServerID: binariesTestServer, Path: "/opt/manual/rpc-server", Kind: "rpc_server", Version: "b1"}); err != nil {
		t.Fatal(err)
	}
	ingestBinaries(t, srv, `[
		{"path":"/opt/a/llama-server","kind":"llama_server","version":"0.9.0-dev (build 1, commit 2bc46ba)","build":"1","commit":"2bc46ba","version_source":"agent","allowed":true},
		{"path":"/opt/b/rpc-server","kind":"bogus","version_source":"weird","allowed":false},
		{"path":"","kind":"other"},
		{"path":"/opt/a/llama-server","kind":"other"}]`)
	got := storedBinaries(t, srv)
	if len(got) != 3 {
		t.Fatalf("stored = %+v, want 3 rows (2 agent + 1 manual)", got)
	}
	a := got["/opt/a/llama-server"]
	if a.Kind != "llama_server" || a.Commit != "2bc46ba" || a.Build != "1" || !a.Allowed || a.Source != routing.BinarySourceAgent || a.VersionSource != "agent" {
		t.Fatalf("agent row = %+v", a)
	}
	if b := got["/opt/b/rpc-server"]; b.Kind != "other" || b.VersionSource != "" {
		t.Fatalf("sanitized row = %+v, want kind other and empty version source", b)
	}
	if got["/opt/manual/rpc-server"].Source != routing.BinarySourceManual {
		t.Fatalf("manual row lost: %+v", got)
	}

	// Absent field: inventory untouched.
	ingestBinaries(t, srv, "")
	if n := len(storedBinaries(t, srv)); n != 3 {
		t.Fatalf("after sample without binaries: %d rows, want 3", n)
	}
	// Smaller list: the missing agent row goes, the manual row stays.
	ingestBinaries(t, srv, `[{"path":"/opt/a/llama-server","kind":"llama_server"}]`)
	got = storedBinaries(t, srv)
	if _, ok := got["/opt/b/rpc-server"]; ok || len(got) != 2 {
		t.Fatalf("after shrink = %+v", got)
	}
	// Empty array: agent allows nothing; only the manual row remains.
	ingestBinaries(t, srv, `[]`)
	got = storedBinaries(t, srv)
	if len(got) != 1 || got["/opt/manual/rpc-server"].Source != routing.BinarySourceManual {
		t.Fatalf("after empty array = %+v", got)
	}
}

func TestIngestBinariesCapped(t *testing.T) {
	srv := NewTestServer()
	var sb strings.Builder
	sb.WriteString("[")
	for i := range maxBinariesPerSample + 40 {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"path":"/opt/bin/` + strconv.Itoa(i) + `","kind":"other"}`)
	}
	sb.WriteString("]")
	ingestBinaries(t, srv, sb.String())
	if n := len(storedBinaries(t, srv)); n != maxBinariesPerSample {
		t.Fatalf("stored %d rows, want cap %d", n, maxBinariesPerSample)
	}
}

func TestManualVersionOverrideSurvivesRescan(t *testing.T) {
	srv := NewTestServer()
	sample := `[{"path":"/opt/a/rpc-server","kind":"rpc_server","version":"detected","build":"7","version_source":"path","allowed":true}]`
	ingestBinaries(t, srv, sample)
	if err := srv.Routes.SetServerBinaryVersionOverride(context.Background(), binariesTestServer, "/opt/a/rpc-server", "b99"); err != nil {
		t.Fatal(err)
	}
	ingestBinaries(t, srv, sample)
	row := storedBinaries(t, srv)["/opt/a/rpc-server"]
	if row.VersionOverride != "b99" || row.EffectiveVersion() != "b99" || row.Version != "detected" {
		t.Fatalf("row after rescan = %+v", row)
	}
}

func TestBinariesAPI(t *testing.T) {
	srv := NewTestServer()
	serverID := seedGPUBudgetServer(t, srv)
	base := "/api/portal/servers/" + serverID + "/binaries"

	rec := rpcMemberDo(t, srv, http.MethodGet, base, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatalf("empty list = %d %s", rec.Code, rec.Body.String())
	}
	rec = rpcMemberDo(t, srv, http.MethodPost, base, `{"path":"/opt/rpc-server","kind":"rpc_server","version":"b5","build":"5"}`)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"source":"manual"`) {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	for body, code := range map[string]string{
		`{"path":"relative/rpc","kind":"rpc_server"}`: "binary.path_invalid",
		`{"path":"/opt/x","kind":"nope"}`:             "binary.kind_invalid",
	} {
		rec = rpcMemberDo(t, srv, http.MethodPost, base, body)
		if rec.Code != http.StatusBadRequest || rpcMemberErrCode(t, rec) != code {
			t.Fatalf("POST %s = %d %s, want %s", body, rec.Code, rec.Body.String(), code)
		}
	}
	rec = rpcMemberDo(t, srv, http.MethodPut, base, `{"path":"/opt/rpc-server","commit":"abcdef1","version_override":"b6"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"effective_version":"b6"`) || !strings.Contains(rec.Body.String(), `"commit":"abcdef1"`) {
		t.Fatalf("put = %d %s", rec.Code, rec.Body.String())
	}
	rec = rpcMemberDo(t, srv, http.MethodPut, base, `{"path":"/opt/missing","version_override":"b6"}`)
	if rec.Code != http.StatusNotFound || rpcMemberErrCode(t, rec) != "binary.not_found" {
		t.Fatalf("put unknown = %d %s", rec.Code, rec.Body.String())
	}

	// An agent row: override allowed, manual fields and delete refused.
	if err := srv.Routes.ReplaceAgentServerBinaries(context.Background(), serverID, []routing.ServerBinary{{Path: "/opt/agent/llama-server", Kind: "llama_server", Version: "v", Allowed: true}}); err != nil {
		t.Fatal(err)
	}
	rec = rpcMemberDo(t, srv, http.MethodPut, base, `{"path":"/opt/agent/llama-server","version_override":"b8"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"effective_version":"b8"`) {
		t.Fatalf("override agent row = %d %s", rec.Code, rec.Body.String())
	}
	rec = rpcMemberDo(t, srv, http.MethodPut, base, `{"path":"/opt/agent/llama-server","kind":"other"}`)
	if rec.Code != http.StatusConflict || rpcMemberErrCode(t, rec) != "binary.agent_owned" {
		t.Fatalf("edit agent row = %d %s", rec.Code, rec.Body.String())
	}
	rec = rpcMemberDo(t, srv, http.MethodPost, base, `{"path":"/opt/agent/llama-server","kind":"other"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("create over agent row = %d %s", rec.Code, rec.Body.String())
	}
	del := base + "?path=" + url.QueryEscape("/opt/agent/llama-server")
	if rec = rpcMemberDo(t, srv, http.MethodDelete, del, ""); rec.Code != http.StatusConflict {
		t.Fatalf("delete agent row = %d", rec.Code)
	}
	del = base + "?path=" + url.QueryEscape("/opt/rpc-server")
	if rec = rpcMemberDo(t, srv, http.MethodDelete, del, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete manual = %d %s", rec.Code, rec.Body.String())
	}
	if rec = rpcMemberDo(t, srv, http.MethodDelete, del, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("delete again = %d", rec.Code)
	}
	if rec = rpcMemberDo(t, srv, http.MethodPatch, base, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("patch = %d", rec.Code)
	}
	rec = rpcMemberDo(t, srv, http.MethodGet, "/api/portal/servers/srv_nope/binaries", "")
	if rec.Code != http.StatusNotFound || rpcMemberErrCode(t, rec) != "server.not_found" {
		t.Fatalf("unknown server = %d %s", rec.Code, rec.Body.String())
	}
}
