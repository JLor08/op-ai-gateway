// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestIngestAcceptsVLLMKind(t *testing.T) {
	srv := NewTestServer()
	ingestBinaries(t, srv, `[
		{"path":"wsl://Ubuntu-24.04/home/ai/venv/bin/vllm","kind":"vllm","version":"0.11.0","version_source":"agent","allowed":true},
		{"path":"/opt/x/tool","kind":"mystery"}]`)
	got := storedBinaries(t, srv)
	if k := got["wsl://Ubuntu-24.04/home/ai/venv/bin/vllm"].Kind; k != "vllm" {
		t.Errorf("vllm kind = %q", k)
	}
	if k := got["/opt/x/tool"].Kind; k != "other" {
		t.Errorf("unknown kind = %q, want other", k)
	}
}

func TestBinariesAPIVLLMAndExecution(t *testing.T) {
	srv := NewTestServer()
	serverID := seedGPUBudgetServer(t, srv)
	base := "/api/portal/servers/" + serverID + "/binaries"
	rec := rpcMemberDo(t, srv, http.MethodPost, base, `{"path":"wsl://Ubuntu-24.04/home/ai/venv/bin/vllm","kind":"vllm"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`"kind":"vllm"`, `"execution":"wsl"`, `"distro":"Ubuntu-24.04"`, `"exec_path":"/home/ai/venv/bin/vllm"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body lacks %s: %s", want, rec.Body.String())
		}
	}
	rec = rpcMemberDo(t, srv, http.MethodPost, base, `{"path":"/usr/bin/llama-server","kind":"llama_server"}`)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"execution":"native"`) || !strings.Contains(rec.Body.String(), `"distro":""`) {
		t.Fatalf("native create = %d %s", rec.Code, rec.Body.String())
	}
	rec = rpcMemberDo(t, srv, http.MethodPost, base, `{"path":"wsl://Ubuntu-24.04","kind":"vllm"}`)
	if rec.Code != http.StatusBadRequest || rpcMemberErrCode(t, rec) != "binary.path_invalid" {
		t.Fatalf("malformed = %d %s", rec.Code, rec.Body.String())
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
	if err := srv.Routes.ReplaceAgentServerBinaries(context.Background(), serverID, []routing.ServerBinary{{Path: "/opt/agent/llama-server", Kind: "llama_server", Version: "v", Allowed: true, RPCProtocol: "10.0.2"}}); err != nil {
		t.Fatal(err)
	}
	rec = rpcMemberDo(t, srv, http.MethodGet, base, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"rpc_protocol":"10.0.2"`) || !strings.Contains(rec.Body.String(), `"rpc_protocol":""`) {
		t.Fatalf("list with protocols = %d %s", rec.Code, rec.Body.String())
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

func TestIngestBinariesNULAndPathCap(t *testing.T) {
	srv := NewTestServer()
	long := "/" + strings.Repeat("a", maxBinaryPathBytes)    // 4097 bytes
	exact := "/" + strings.Repeat("b", maxBinaryPathBytes-1) // 4096 bytes
	ingestBinaries(t, srv, `[
		{"path":"`+long+`","kind":"other"},
		{"path":"`+exact+`","kind":"other"},
		{"path":"/opt/nul\u0000x","kind":"other"},
		{"path":"/opt/v","kind":"llama_server","version":"a\u0000b","build":"1\u00002","commit":"c\u0000d"}]`)
	got := storedBinaries(t, srv)
	if _, ok := got[long]; ok {
		t.Fatal("path over the cap must be dropped")
	}
	if _, ok := got[exact]; !ok {
		t.Fatal("path at the cap must be kept")
	}
	if _, ok := got["/opt/nul\x00x"]; ok {
		t.Fatal("path with NUL must be dropped")
	}
	if v := got["/opt/v"]; v.Version != "ab" || v.Build != "12" || v.Commit != "cd" {
		t.Fatalf("NUL not stripped: %+v", v)
	}
}

// TestIngestBinariesKeepLastKnownVersion: a re-report of the same path
// without a version (a --version probe that failed or timed out) keeps the
// version the inventory already knows, so one failed probe does not withhold
// the automatic clusters on the host until the file changes.
func TestIngestBinariesKeepLastKnownVersion(t *testing.T) {
	srv := NewTestServer()
	ingestBinaries(t, srv, `[{"path":"/opt/a/llama-server","kind":"llama_server","version":"0.9.0 (build 1, commit 2bc46ba)","build":"1","commit":"2bc46ba","version_source":"agent","allowed":true}]`)
	ingestBinaries(t, srv, `[{"path":"/opt/a/llama-server","kind":"llama_server","allowed":true},
		{"path":"/opt/a/rpc-server","kind":"rpc_server","allowed":true}]`)
	got := storedBinaries(t, srv)
	a := got["/opt/a/llama-server"]
	if a.Version != "0.9.0 (build 1, commit 2bc46ba)" || a.Build != "1" || a.Commit != "2bc46ba" || a.VersionSource != "agent" {
		t.Fatalf("re-reported row = %+v, want the last known version kept", a)
	}
	if b := got["/opt/a/rpc-server"]; b.Version != "" || b.Commit != "" {
		t.Fatalf("new row without a version = %+v, want it unversioned", b)
	}
	// A report that carries a version replaces the kept one.
	ingestBinaries(t, srv, `[{"path":"/opt/a/llama-server","kind":"llama_server","version":"0.9.1 (build 2, commit 3cd57cb)","build":"2","commit":"3cd57cb","version_source":"agent","allowed":true}]`)
	if a := storedBinaries(t, srv)["/opt/a/llama-server"]; a.Commit != "3cd57cb" || a.Build != "2" {
		t.Fatalf("row after a versioned report = %+v", a)
	}
}

// notifyCountingPortal counts the cluster notifications a binary write-back
// sends; nothing else of the portal is called on that path.
type notifyCountingPortal struct {
	portal.API
	mu sync.Mutex
	n  int
}

func (p *notifyCountingPortal) NotifyClustersOfServer(context.Context, string) {
	p.mu.Lock()
	p.n++
	p.mu.Unlock()
}

func (p *notifyCountingPortal) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n
}

// TestIngestBinariesNotifyClustersOnlyOnChange: the agent resends its
// inventory unchanged on every reconnect and on the system-report cadence,
// so only a report that changes the stored rows re-derives the clusters of
// the server; a resend, a probe failure whose version is kept, and a path a
// manual row owns change nothing.
func TestIngestBinariesNotifyClustersOnlyOnChange(t *testing.T) {
	srv := NewTestServer()
	p := &notifyCountingPortal{}
	srv.Portal = p
	ctx := context.Background()
	write := func(raw string) {
		t.Helper()
		var samples []agentBinarySample
		if err := json.Unmarshal([]byte(raw), &samples); err != nil {
			t.Fatal(err)
		}
		srv.writeBackBinaries(ctx, binariesTestServer, &samples, time.Now())
	}
	probed := `{"path":"/opt/a/llama-server","kind":"llama_server","version":"0.9.0 (build 1, commit 2bc46ba)","build":"1","commit":"2bc46ba","version_source":"agent","allowed":true}`

	write(`[` + probed + `]`)
	if n := p.count(); n != 1 {
		t.Fatalf("notifications after the first report = %d, want 1", n)
	}
	write(`[` + probed + `]`)
	write(`[{"path":"/opt/a/llama-server","kind":"llama_server","allowed":true}]`)
	if err := srv.Routes.UpsertManualServerBinary(ctx, routing.ServerBinary{ServerID: binariesTestServer, Path: "/opt/m/rpc-server", Kind: "rpc_server", Version: "b1"}); err != nil {
		t.Fatal(err)
	}
	write(`[` + probed + `,{"path":"/opt/m/rpc-server","kind":"rpc_server","allowed":true}]`)
	if n := p.count(); n != 1 {
		t.Fatalf("notifications after unchanged resends = %d, want still 1", n)
	}
	write(`[{"path":"/opt/a/llama-server","kind":"llama_server","version":"0.9.1 (build 2, commit 3cd57cb)","build":"2","commit":"3cd57cb","version_source":"agent","allowed":true}]`)
	if n := p.count(); n != 2 {
		t.Fatalf("notifications after a new version = %d, want 2", n)
	}
	write(`[]`)
	if n := p.count(); n != 3 {
		t.Fatalf("notifications after the binary left = %d, want 3", n)
	}
}

// TestIngestBinariesKeepProbedIdentityOverPathFallback: a binary whose path
// carries a build tag still reports that build when its --version probe
// fails, from the path. That fallback must not replace the identity the
// probe found before -- the version would go empty and the build or commit
// could differ, moving the head's generation until the next probe succeeds.
// A row the agent probed again replaces it.
func TestIngestBinariesKeepProbedIdentityOverPathFallback(t *testing.T) {
	srv := NewTestServer()
	ingestBinaries(t, srv, `[
		{"path":"/opt/llama-b5000/llama-server","kind":"llama_server","version":"5000 (5000-a1b2c3d)","build":"5000","commit":"a1b2c3d","version_source":"agent","allowed":true},
		{"path":"/opt/llama-b5000/rpc-server","kind":"rpc_server","version":"5000 (5000-a1b2c3d)","build":"5000","commit":"a1b2c3d","version_source":"agent","allowed":true}]`)
	ingestBinaries(t, srv, `[
		{"path":"/opt/llama-b5000/llama-server","kind":"llama_server","build":"5000","version_source":"path","allowed":true},
		{"path":"/opt/llama-b5000/rpc-server","kind":"rpc_server","build":"5000","version_source":"path","allowed":true}]`)
	for path, b := range storedBinaries(t, srv) {
		if b.Version != "5000 (5000-a1b2c3d)" || b.Build != "5000" || b.Commit != "a1b2c3d" || b.VersionSource != "agent" {
			t.Fatalf("%s after a failed probe = %+v, want the probed identity kept", path, b)
		}
	}
	ingestBinaries(t, srv, `[{"path":"/opt/llama-b5000/llama-server","kind":"llama_server","version":"5001 (5001-0f0f0f0)","build":"5001","commit":"0f0f0f0","version_source":"agent","allowed":true}]`)
	if b := storedBinaries(t, srv)["/opt/llama-b5000/llama-server"]; b.Build != "5001" || b.Commit != "0f0f0f0" {
		t.Fatalf("row after a new probe = %+v, want the new identity", b)
	}
}

// TestIngestBinariesRPCProtocol: a well-formed protocol is stored, every
// malformed one is stored as "" (the portal matching trusts only a valid
// value), and a protocol-only change counts as an inventory change so the
// clusters of the server are re-derived.
func TestIngestBinariesRPCProtocol(t *testing.T) {
	srv := NewTestServer()
	ingestBinaries(t, srv, `[
		{"path":"/a/rpc-server","kind":"rpc_server","rpc_protocol":"10.0.2","allowed":true},
		{"path":"/b/rpc-server","kind":"rpc_server","rpc_protocol":"10.0","allowed":true},
		{"path":"/c/rpc-server","kind":"rpc_server","rpc_protocol":"v10.0.2","allowed":true},
		{"path":"/d/rpc-server","kind":"rpc_server","rpc_protocol":"1000.0.0","allowed":true}]`)
	got := storedBinaries(t, srv)
	if p := got["/a/rpc-server"].RPCProtocol; p != "10.0.2" {
		t.Fatalf("valid protocol stored as %q, want 10.0.2", p)
	}
	for _, path := range []string{"/b/rpc-server", "/c/rpc-server", "/d/rpc-server"} {
		if p := got[path].RPCProtocol; p != "" {
			t.Fatalf("%s: malformed protocol stored as %q, want empty", path, p)
		}
	}

	stored := []routing.ServerBinary{{Path: "/a", Kind: "rpc_server", Source: routing.BinarySourceAgent, RPCProtocol: "10.0.2"}}
	if !binaryInventoryChanged(stored, []routing.ServerBinary{{Path: "/a", Kind: "rpc_server", RPCProtocol: "10.0.3"}}) {
		t.Fatal("a protocol-only change must count as an inventory change")
	}
	if binaryInventoryChanged(stored, []routing.ServerBinary{{Path: "/a", Kind: "rpc_server", RPCProtocol: "10.0.2"}}) {
		t.Fatal("an unchanged protocol must not count as a change")
	}
}

// TestIngestBinariesKeepKnownRPCProtocol: a re-report of an rpc_server row
// without a protocol (the probe failed) keeps the stored one; a later report
// that carries one replaces it; a row of any other kind never keeps one.
func TestIngestBinariesKeepKnownRPCProtocol(t *testing.T) {
	srv := NewTestServer()
	ingestBinaries(t, srv, `[{"path":"/a/rpc-server","kind":"rpc_server","rpc_protocol":"10.0.2","allowed":true},
		{"path":"/a/llama-server","kind":"llama_server","allowed":true}]`)
	ingestBinaries(t, srv, `[{"path":"/a/rpc-server","kind":"rpc_server","allowed":true},
		{"path":"/a/llama-server","kind":"llama_server","allowed":true}]`)
	if p := storedBinaries(t, srv)["/a/rpc-server"].RPCProtocol; p != "10.0.2" {
		t.Fatalf("re-report without protocol stored %q, want the known 10.0.2 kept", p)
	}
	ingestBinaries(t, srv, `[{"path":"/a/rpc-server","kind":"rpc_server","rpc_protocol":"10.0.3","allowed":true}]`)
	if p := storedBinaries(t, srv)["/a/rpc-server"].RPCProtocol; p != "10.0.3" {
		t.Fatalf("re-report with protocol stored %q, want 10.0.3", p)
	}

	// A llama_server row never keeps a protocol, even one it somehow holds.
	rows := []routing.ServerBinary{{Path: "/a/llama-server", Kind: "llama_server"}}
	keepKnownBinaryVersions(rows, []routing.ServerBinary{{Path: "/a/llama-server", Kind: "llama_server", Source: routing.BinarySourceAgent, RPCProtocol: "10.0.2"}})
	if rows[0].RPCProtocol != "" {
		t.Fatalf("a llama_server row kept protocol %q", rows[0].RPCProtocol)
	}
}

// TestIngestBinariesRPCProtocolRetentionNeedsSameIdentity: the stored
// rpc_server protocol is kept for a protocol-less re-report only while the
// re-reported version, build and commit equal the stored row's. A binary
// replaced at the same path (another identity) whose first protocol probe
// fails loses the protocol rather than keep the replaced binary's.
func TestIngestBinariesRPCProtocolRetentionNeedsSameIdentity(t *testing.T) {
	srv := NewTestServer()
	ingestBinaries(t, srv, `[
		{"path":"/a/llama-server","kind":"llama_server","version":"0.9.0 (build 1, commit 2bc46ba)","build":"1","commit":"2bc46ba","version_source":"agent","rpc_protocol":"10.0.2","allowed":true},
		{"path":"/a/rpc-server","kind":"rpc_server","version":"0.9.0 (build 1, commit 2bc46ba)","build":"1","commit":"2bc46ba","version_source":"agent","rpc_protocol":"10.0.2","allowed":true}]`)
	// Same identity, failed protocol probe: kept.
	ingestBinaries(t, srv, `[
		{"path":"/a/llama-server","kind":"llama_server","version":"0.9.0 (build 1, commit 2bc46ba)","build":"1","commit":"2bc46ba","version_source":"agent","allowed":true},
		{"path":"/a/rpc-server","kind":"rpc_server","version":"0.9.0 (build 1, commit 2bc46ba)","build":"1","commit":"2bc46ba","version_source":"agent","allowed":true}]`)
	if p := storedBinaries(t, srv)["/a/rpc-server"].RPCProtocol; p != "10.0.2" {
		t.Fatalf("same identity: protocol %q, want 10.0.2 kept", p)
	}
	// Replaced binary (new identity), failed protocol probe: dropped.
	ingestBinaries(t, srv, `[
		{"path":"/a/llama-server","kind":"llama_server","version":"0.9.1 (build 2, commit 3cd57cb)","build":"2","commit":"3cd57cb","version_source":"agent","allowed":true},
		{"path":"/a/rpc-server","kind":"rpc_server","version":"0.9.1 (build 2, commit 3cd57cb)","build":"2","commit":"3cd57cb","version_source":"agent","allowed":true}]`)
	got := storedBinaries(t, srv)
	if p := got["/a/rpc-server"].RPCProtocol; p != "" {
		t.Fatalf("replaced binary: rpc protocol %q, want none", p)
	}
	if p := got["/a/llama-server"].RPCProtocol; p != "" {
		t.Fatalf("replaced binary: llama protocol %q, want none", p)
	}
}

// TestIngestBinariesLlamaServerProtocolFromSibling: after an agent restart
// whose rpc-server protocol probe fails, the rpc_server row keeps its
// protocol (retention), and the llama_server row of the same directory takes
// it from there, so a protocol-only cluster (a head and member of different
// builds, e.g. git vs source tarball) stays matchable. A llama_server whose
// directory has no rpc_server row, or rpc_server rows of different
// protocols, gets none.
func TestIngestBinariesLlamaServerProtocolFromSibling(t *testing.T) {
	srv := NewTestServer()
	const tarball = `"version":"version: 1.1.0-dev (build 0, commit unknown)","build":"0","commit":"unknown","version_source":"agent"`
	ingestBinaries(t, srv, `[
		{"path":"/a/llama-server","kind":"llama_server",`+tarball+`,"rpc_protocol":"10.0.2","allowed":true},
		{"path":"/a/rpc-server","kind":"rpc_server",`+tarball+`,"rpc_protocol":"10.0.2","allowed":true},
		{"path":"C:\\llama\\llama-server.exe","kind":"llama_server",`+tarball+`,"rpc_protocol":"10.0.2","allowed":true},
		{"path":"C:\\llama\\rpc-server.exe","kind":"rpc_server",`+tarball+`,"rpc_protocol":"10.0.2","allowed":true}]`)
	// The transient failure: the agent restarted and its protocol probes failed.
	ingestBinaries(t, srv, `[
		{"path":"/a/llama-server","kind":"llama_server",`+tarball+`,"allowed":true},
		{"path":"/a/rpc-server","kind":"rpc_server",`+tarball+`,"allowed":true},
		{"path":"C:\\llama\\llama-server.exe","kind":"llama_server",`+tarball+`,"allowed":true},
		{"path":"C:\\llama\\rpc-server.exe","kind":"rpc_server",`+tarball+`,"allowed":true},
		{"path":"/b/llama-server","kind":"llama_server",`+tarball+`,"allowed":true}]`)
	got := storedBinaries(t, srv)
	for _, path := range []string{"/a/llama-server", "/a/rpc-server", `C:\llama\llama-server.exe`, `C:\llama\rpc-server.exe`} {
		if p := got[path].RPCProtocol; p != "10.0.2" {
			t.Errorf("%s after a failed probe: protocol %q, want 10.0.2", path, p)
		}
	}
	if p := got["/b/llama-server"].RPCProtocol; p != "" {
		t.Errorf("llama without rpc sibling: protocol %q", p)
	}

	// The sibling left: the llama_server row has no protocol any more.
	ingestBinaries(t, srv, `[{"path":"/a/llama-server","kind":"llama_server",`+tarball+`,"allowed":true}]`)
	if p := storedBinaries(t, srv)["/a/llama-server"].RPCProtocol; p != "" {
		t.Fatalf("sibling gone: protocol %q, want none", p)
	}

	// Siblings of different protocols: none, even when one is only kept.
	ingestBinaries(t, srv, `[
		{"path":"/c/ggml-rpc-server","kind":"rpc_server",`+tarball+`,"rpc_protocol":"10.0.2","allowed":true}]`)
	ingestBinaries(t, srv, `[
		{"path":"/c/llama-server","kind":"llama_server",`+tarball+`,"allowed":true},
		{"path":"/c/ggml-rpc-server","kind":"rpc_server",`+tarball+`,"allowed":true},
		{"path":"/c/rpc-server","kind":"rpc_server",`+tarball+`,"rpc_protocol":"9.0.0","allowed":true}]`)
	if p := storedBinaries(t, srv)["/c/llama-server"].RPCProtocol; p != "" {
		t.Fatalf("disagreeing siblings: protocol %q, want none", p)
	}
}

func TestBinarySiblingDirKey(t *testing.T) {
	for in, want := range map[string]string{
		"/opt/a/llama-server":           "/opt/a",
		`C:\llama\rpc-server.exe`:       `C:\llama`,
		"C:/llama/rpc-server.exe":       "C:/llama",
		"wsl://Ubuntu/opt/l/rpc-server": "wsl://Ubuntu/opt/l",
		"wsl://Debian/opt/l/rpc-server": "wsl://Debian/opt/l",
		"rpc-server":                    "",
	} {
		if got := binarySiblingDirKey(in); got != want {
			t.Errorf("binarySiblingDirKey(%q) = %q, want %q", in, got, want)
		}
	}
}
