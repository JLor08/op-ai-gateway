// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"testing"
)

func TestBinariesForeignServer404(t *testing.T) {
	ctx := context.Background()
	svc, _ := newServerTestService(t, rpcMemberTestNow)
	server := createTestServer(t, svc, "M", "m.example.test")
	req := PutServerBinaryRequest{Path: "/opt/rpc-server"}
	if _, err := svc.ServerBinaries(ctx, otherToken(), server.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("list = %v, want ErrServerNotFound", err)
	}
	if _, err := svc.CreateManualServerBinary(ctx, otherToken(), server.ID, req); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("create = %v", err)
	}
	if _, err := svc.UpdateServerBinary(ctx, otherToken(), server.ID, req); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("update = %v", err)
	}
	if err := svc.DeleteManualServerBinary(ctx, otherToken(), server.ID, "/opt/rpc-server"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("delete = %v", err)
	}
}

func TestValidBinaryPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/opt/rpc-server": true, `C:\llama\rpc-server.exe`: true, "D:/x/rpc": true, `\\host\share\rpc`: true,
		"": false, "rpc-server": false, "./rpc": false, "C:rpc": false, "/a\x00b": false,
	} {
		if got := validBinaryPath(p); got != want {
			t.Errorf("validBinaryPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestValidBinaryPathWSL(t *testing.T) {
	for p, want := range map[string]bool{
		"wsl://Ubuntu-24.04/home/ai/venv/bin/vllm": true,
		"wsl://Ubuntu-24.04":                       false,
		"WSL://Ubuntu-24.04/x":                     false,
		"wsl://-x/y":                               false,
		`wsl://Ubuntu-24.04\x`:                     false,
		"wsl://Ubuntu-24.04/a\x00b":                false,
		`wsl://Ubuntu-24.04/a\b`:                   false,
	} {
		if got := validBinaryPath(p); got != want {
			t.Errorf("validBinaryPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestServerBinaryDTOExecution(t *testing.T) {
	native := serverBinaryDTO(routing.ServerBinary{Path: "/usr/bin/llama-server", Kind: BinaryKindLlamaServer})
	if native.Execution != "native" || native.Distro != "" || native.ExecPath != "/usr/bin/llama-server" {
		t.Errorf("native = %+v", native)
	}
	wsl := serverBinaryDTO(routing.ServerBinary{Path: "wsl://Ubuntu-24.04/home/ai/venv/bin/vllm", Kind: BinaryKindVLLM})
	if wsl.Execution != "wsl" || wsl.Distro != "Ubuntu-24.04" || wsl.ExecPath != "/home/ai/venv/bin/vllm" {
		t.Errorf("wsl = %+v", wsl)
	}
	bad := serverBinaryDTO(routing.ServerBinary{Path: "wsl://Ubuntu-24.04", Kind: BinaryKindOther})
	if bad.Execution != "native" || bad.Distro != "" || bad.ExecPath != "wsl://Ubuntu-24.04" {
		t.Errorf("malformed = %+v", bad)
	}
}

func TestMatchingRPCBinaryByCommitThenBuild(t *testing.T) {
	ctx := context.Background()
	svc, store := newServerTestService(t, rpcMemberTestNow)
	server := createTestServer(t, svc, "M", "m.example.test")
	rows := []routing.ServerBinary{
		{Path: "/z/rpc-server", Kind: "rpc_server", Allowed: true, Commit: "2bc46baa9207d098d03741ac394e7faaf1a955b6", Build: "9"},
		{Path: "/b/rpc-server", Kind: "rpc_server", Allowed: true, Commit: "2BC46BA", Build: "1"},
		{Path: "/a/rpc-server", Kind: "rpc_server", Allowed: true, Commit: "ffffffff", Build: "1"},
		{Path: "/c/llama-server", Kind: "llama_server", Allowed: true, Commit: "2bc46ba"},
		{Path: "/d/rpc-server", Kind: "rpc_server", Allowed: true, Build: "b42"},
		{Path: "/e/rpc-server", Kind: "rpc_server", Allowed: true},
	}
	if err := store.ReplaceAgentServerBinaries(ctx, server.ID, rows); err != nil {
		t.Fatal(err)
	}
	match := func(head routing.ServerBinary) (string, bool) {
		t.Helper()
		b, ok, err := svc.MatchingRPCBinary(ctx, server.ID, head)
		if err != nil {
			t.Fatal(err)
		}
		return b.Path, ok
	}
	// Commit wins over a differing build; abbreviated hash matches the full
	// one case-insensitively; several matches resolve to the first by path.
	if p, ok := match(routing.ServerBinary{Commit: "2bc46ba", Build: "123"}); !ok || p != "/b/rpc-server" {
		t.Fatalf("commit match = %q %v, want /b/rpc-server", p, ok)
	}
	// A prefix shorter than 7 characters is not a match.
	if _, ok := match(routing.ServerBinary{Commit: "2bc46"}); ok {
		t.Fatal("short commit prefix must not match")
	}
	// No commit on the head: build equality ("b" prefix ignored).
	if p, ok := match(routing.ServerBinary{Build: "42"}); !ok || p != "/d/rpc-server" {
		t.Fatalf("build match = %q %v, want /d/rpc-server", p, ok)
	}
	// Unknown versions never match.
	if _, ok := match(routing.ServerBinary{}); ok {
		t.Fatal("unknown head version must not match")
	}
	if _, ok := match(routing.ServerBinary{Commit: "1234567", Build: "1"}); ok {
		t.Fatal("different commits must not fall back to build")
	}
	// An override on a member binary is what matching compares.
	if err := store.SetServerBinaryVersionOverride(ctx, server.ID, "/e/rpc-server", "0.9.0 (build 77, commit abcdef012)"); err != nil {
		t.Fatal(err)
	}
	if p, ok := match(routing.ServerBinary{Commit: "abcdef012"}); !ok || p != "/e/rpc-server" {
		t.Fatalf("override match = %q %v", p, ok)
	}
	// binaryVersionFor looks one row up by path.
	if b, ok, err := svc.BinaryVersionFor(ctx, server.ID, "/d/rpc-server"); err != nil || !ok || b.Build != "b42" {
		t.Fatalf("binaryVersionFor = %+v %v %v", b, ok, err)
	}
	if _, ok, _ := svc.BinaryVersionFor(ctx, server.ID, "/nope"); ok {
		t.Fatal("unknown path must not be found")
	}
}

func TestMatchingRPCBinaryEligibility(t *testing.T) {
	ctx := context.Background()
	svc, store := newServerTestService(t, rpcMemberTestNow)
	server := createTestServer(t, svc, "M", "m.example.test")
	head := routing.ServerBinary{Commit: "2bc46ba"}
	if err := store.ReplaceAgentServerBinaries(ctx, server.ID, []routing.ServerBinary{
		{Path: "/a/rpc-server", Kind: "rpc_server", Commit: "2bc46ba", Allowed: false},
		{Path: "/b/rpc-server", Kind: "rpc_server", Commit: "2bc46ba", Allowed: true},
	}); err != nil {
		t.Fatal(err)
	}
	b, ok, err := svc.MatchingRPCBinary(ctx, server.ID, head)
	if err != nil || !ok || b.Path != "/b/rpc-server" {
		t.Fatalf("match = %+v %v %v, want the allowed /b/rpc-server", b, ok, err)
	}
	if err := store.UpsertManualServerBinary(ctx, routing.ServerBinary{ServerID: server.ID, Path: "/0/rpc-server", Kind: "rpc_server", Commit: "2bc46ba"}); err != nil {
		t.Fatal(err)
	}
	if b, ok, _ = svc.MatchingRPCBinary(ctx, server.ID, head); !ok || b.Path != "/0/rpc-server" {
		t.Fatalf("manual row must stay eligible, got %+v %v", b, ok)
	}
	// Only a disallowed agent row left: no match.
	if err := store.DeleteManualServerBinary(ctx, server.ID, "/0/rpc-server"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceAgentServerBinaries(ctx, server.ID, []routing.ServerBinary{{Path: "/a/rpc-server", Kind: "rpc_server", Commit: "2bc46ba"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ = svc.MatchingRPCBinary(ctx, server.ID, head); ok {
		t.Fatal("disallowed agent row must not match")
	}
}

func TestBinaryIdentityOverrideParsing(t *testing.T) {
	for override, want := range map[string][2]string{
		"20240115":                        {"", ""},
		"b99":                             {"", "99"},
		"0.9.0 (build 5, commit 2bc46ba)": {"2bc46ba", "5"},
		"b12 2BC46BA9":                    {"2bc46ba9", "12"},
	} {
		c, b := binaryIdentity(routing.ServerBinary{VersionOverride: override, Commit: "ignored", Build: "ignored"})
		if c != want[0] || b != want[1] {
			t.Errorf("override %q = (%q,%q), want %v", override, c, b, want)
		}
	}
}

// An automatic cluster member runs natively, and a wsl:// rpc-server row can
// never be launched that way: it is never a match, whatever its version.
func TestMatchingRPCBinarySkipsWSLRows(t *testing.T) {
	ctx := context.Background()
	svc, store := newServerTestService(t, rpcMemberTestNow)
	server := createTestServer(t, svc, "M", "m.example.test")
	head := routing.ServerBinary{Commit: "2bc46ba"}
	wsl := routing.ServerBinary{Path: "wsl://Ubuntu-24.04/home/ai/llama/rpc-server", Kind: "rpc_server", Commit: "2bc46ba", Allowed: true}
	odd := routing.ServerBinary{Path: "WSL://Ubuntu-24.04/home/ai/llama/rpc-server", Kind: "rpc_server", Commit: "2bc46ba", Allowed: true}
	if err := store.ReplaceAgentServerBinaries(ctx, server.ID, []routing.ServerBinary{wsl, odd}); err != nil {
		t.Fatal(err)
	}
	if b, ok, err := svc.MatchingRPCBinary(ctx, server.ID, head); err != nil || ok {
		t.Fatalf("only wsl:// rows: match = %+v %v %v, want none", b, ok, err)
	}
	// A native row that sorts after the wsl:// ones is still chosen.
	native := routing.ServerBinary{Path: `z:\llama\rpc-server.exe`, Kind: "rpc_server", Commit: "2bc46ba", Allowed: true}
	if err := store.ReplaceAgentServerBinaries(ctx, server.ID, []routing.ServerBinary{wsl, odd, native}); err != nil {
		t.Fatal(err)
	}
	if b, ok, err := svc.MatchingRPCBinary(ctx, server.ID, head); err != nil || !ok || b.Path != native.Path {
		t.Fatalf("match = %+v %v %v, want the native row", b, ok, err)
	}
}

// The vectors of the agent's wslentry TestParse (server-agent
// internal/wslentry/wslentry_test.go): both twins must agree on them.
func TestParseWSLBinaryPathTwinVectors(t *testing.T) {
	for _, tc := range []struct {
		entry, distro, path string
		ok                  bool
	}{
		{"wsl://Ubuntu-24.04/home/ai/vllm-env/bin/vllm", "Ubuntu-24.04", "/home/ai/vllm-env/bin/vllm", true},
		{"wsl://Ubuntu-24.04/", "Ubuntu-24.04", "/", true},
		{"WSL://Ubuntu-24.04/x", "", "", false},
		{"wsl:/Ubuntu/x", "", "", false},
		{"wsl://Ubuntu-24.04", "", "", false},
		{"wsl:///x", "", "", false},
		{"wsl://-bad/x", "", "", false},
		{`wsl://Ubuntu-24.04\home\x`, "", "", false},
		{`wsl://Ubuntu-24.04/home\x`, "", "", false},
		{"wsl://Ubuntu/a\x00b", "", "", false},
		{"/usr/bin/x", "", "", false},
		{`C:\x.exe`, "", "", false},
	} {
		d, p, ok := parseWSLBinaryPath(tc.entry)
		if d != tc.distro || p != tc.path || ok != tc.ok {
			t.Errorf("parseWSLBinaryPath(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.entry, d, p, ok, tc.distro, tc.path, tc.ok)
		}
	}
}

// TestRPCCompatibility is spec decision 5: two protocols decide alone (full
// X.Y.Z equality), the build identity only refines a protocol match into an
// identical one; without a protocol on either side the build identity rules
// as before (commit, else build), and an unknown/0 identity is absent.
func TestRPCCompatibility(t *testing.T) {
	const c1, c2 = "2bc46ba", "9999fff"
	for _, tc := range []struct {
		name         string
		head, member routing.ServerBinary
		want         rpcCompat
	}{
		{"equal protocols, different commits", routing.ServerBinary{Commit: c1, RPCProtocol: "10.0.2"}, routing.ServerBinary{Commit: c2, RPCProtocol: "10.0.2"}, rpcCompatProtocol},
		{"equal protocols, equal commit", routing.ServerBinary{Commit: c1, RPCProtocol: "10.0.2"}, routing.ServerBinary{Commit: c1, RPCProtocol: "10.0.2"}, rpcCompatIdentical},
		{"patch differs", routing.ServerBinary{Commit: c1, RPCProtocol: "10.0.2"}, routing.ServerBinary{Commit: c1, RPCProtocol: "10.0.1"}, rpcMismatchProtocol},
		{"major differs, no identity", routing.ServerBinary{RPCProtocol: "9.0.0"}, routing.ServerBinary{RPCProtocol: "10.0.0"}, rpcMismatchProtocol},
		{"tarball builds, equal protocol", routing.ServerBinary{Commit: "unknown", Build: "0", RPCProtocol: "10.0.2"}, routing.ServerBinary{Commit: "unknown", Build: "0", RPCProtocol: "10.0.2"}, rpcCompatProtocol},
		{"head no protocol, same commit", routing.ServerBinary{Commit: c1}, routing.ServerBinary{Commit: c1, RPCProtocol: "10.0.2"}, rpcCompatIdentical},
		{"head no protocol, different commit", routing.ServerBinary{Commit: c1}, routing.ServerBinary{Commit: c2, RPCProtocol: "10.0.2"}, rpcMismatchBuild},
		{"head no protocol, member unknown/0", routing.ServerBinary{Commit: c1, Build: "5"}, routing.ServerBinary{Commit: "unknown", Build: "0", RPCProtocol: "10.0.2"}, rpcCompatUnknown},
		{"member no protocol, same build", routing.ServerBinary{Build: "42", RPCProtocol: "10.0.2"}, routing.ServerBinary{Build: "b42"}, rpcCompatIdentical},
		{"malformed protocol is absent", routing.ServerBinary{Commit: c1, RPCProtocol: "10.0"}, routing.ServerBinary{Commit: c1, RPCProtocol: "10.0.2"}, rpcCompatIdentical},
		// Old rows: no protocol on either side behaves as the build match did,
		// except that commit "unknown" and build 0 count as absent.
		{"old rows, same commit", routing.ServerBinary{Commit: c1, Build: "1"}, routing.ServerBinary{Commit: "2BC46BAA9207", Build: "2"}, rpcCompatIdentical},
		{"old rows, different commits", routing.ServerBinary{Commit: c1, Build: "1"}, routing.ServerBinary{Commit: c2, Build: "1"}, rpcMismatchBuild},
		{"old rows, same build", routing.ServerBinary{Build: "7"}, routing.ServerBinary{Build: "b7"}, rpcCompatIdentical},
		{"old rows, different builds", routing.ServerBinary{Build: "7"}, routing.ServerBinary{Build: "8"}, rpcMismatchBuild},
		{"old rows, commit vs build", routing.ServerBinary{Commit: c1}, routing.ServerBinary{Build: "8"}, rpcCompatUnknown},
		{"old rows, nothing known", routing.ServerBinary{}, routing.ServerBinary{}, rpcCompatUnknown},
		{"old rows, both commit unknown", routing.ServerBinary{Commit: "unknown", Build: "0"}, routing.ServerBinary{Commit: "Unknown", Build: "0"}, rpcCompatUnknown},
	} {
		if got := rpcCompatibility(tc.head, tc.member); got != tc.want {
			t.Errorf("%s: rpcCompatibility = %v, want %v", tc.name, got, tc.want)
		}
	}
	for c, want := range map[rpcCompat][2]bool{
		rpcCompatIdentical:  {true, false},
		rpcCompatProtocol:   {true, false},
		rpcMismatchProtocol: {false, true},
		rpcMismatchBuild:    {false, true},
		rpcCompatUnknown:    {false, false},
	} {
		if c.compatible() != want[0] || c.mismatch() != want[1] {
			t.Errorf("%v: compatible/mismatch = %v/%v, want %v", c, c.compatible(), c.mismatch(), want)
		}
	}
}

func TestBinaryIdentityUnknownHygiene(t *testing.T) {
	for _, tc := range []struct {
		name                string
		b                   routing.ServerBinary
		wantCommit, wantBld string
	}{
		{"fields", routing.ServerBinary{Commit: "Unknown", Build: "0"}, "", ""},
		{"b0 build", routing.ServerBinary{Commit: "2bc46ba", Build: "b0"}, "2bc46ba", ""},
		{"override build 0, commit unknown", routing.ServerBinary{VersionOverride: "1.1.0-dev (build 0, commit unknown)", Commit: "2bc46ba", Build: "5"}, "", ""},
		{"override b0", routing.ServerBinary{VersionOverride: "b0", Build: "5"}, "", ""},
		{"override build 0 with a commit", routing.ServerBinary{VersionOverride: "1.1.0 (build 0, commit 2bc46ba)"}, "2bc46ba", ""},
		{"build 5, commit unknown", routing.ServerBinary{Commit: "unknown", Build: "5"}, "", "5"},
		{"override build 5, commit unknown", routing.ServerBinary{VersionOverride: "1.1.0-dev (build 5, commit unknown)", Commit: "2bc46ba", Build: "7"}, "", "5"},
	} {
		c, b := binaryIdentity(tc.b)
		if c != tc.wantCommit || b != tc.wantBld {
			t.Errorf("%s: binaryIdentity = (%q,%q), want (%q,%q)", tc.name, c, b, tc.wantCommit, tc.wantBld)
		}
	}
	tarball := routing.ServerBinary{Commit: "unknown", Build: "0"}
	if binaryVersionKnown(tarball) {
		t.Error("a build 0, commit unknown row must not count as known")
	}
	tarball.RPCProtocol = "10.0.2"
	if !binaryVersionKnown(tarball) {
		t.Error("a probed protocol makes the binary's version known")
	}
	tarball.RPCProtocol = "garbage"
	if binaryVersionKnown(tarball) {
		t.Error("a malformed protocol must not count as known")
	}
}

// Review Focus 3: a member with one protocol-matching and one build-identical
// rpc-server picks the build-identical one, whatever the path order.
func TestMatchingRPCBinaryPrefersIdentical(t *testing.T) {
	ctx := context.Background()
	svc, store := newServerTestService(t, rpcMemberTestNow)
	server := createTestServer(t, svc, "M", "m.example.test")
	head := routing.ServerBinary{Commit: "2bc46ba", Build: "5", RPCProtocol: "10.0.2"}
	for name, rows := range map[string][]routing.ServerBinary{
		"both with protocol": {
			{Path: "/a/rpc-server", Kind: "rpc_server", Allowed: true, Commit: "9999fff", RPCProtocol: "10.0.2"},
			{Path: "/b/rpc-server", Kind: "rpc_server", Allowed: true, Commit: "2bc46ba", RPCProtocol: "10.0.2"},
		},
		"identical row without protocol": {
			{Path: "/a/rpc-server", Kind: "rpc_server", Allowed: true, Commit: "unknown", Build: "0", RPCProtocol: "10.0.2"},
			{Path: "/b/rpc-server", Kind: "rpc_server", Allowed: true, Commit: "2bc46ba"},
		},
	} {
		if err := store.ReplaceAgentServerBinaries(ctx, server.ID, rows); err != nil {
			t.Fatal(err)
		}
		b, ok, err := svc.MatchingRPCBinary(ctx, server.ID, head)
		if err != nil || !ok || b.Path != "/b/rpc-server" {
			t.Errorf("%s: match = %q %v %v, want the build-identical /b/rpc-server", name, b.Path, ok, err)
		}
	}
	// Two protocol matches: the first by path.
	if err := store.ReplaceAgentServerBinaries(ctx, server.ID, []routing.ServerBinary{
		{Path: "/d/rpc-server", Kind: "rpc_server", Allowed: true, Commit: "9999fff", RPCProtocol: "10.0.2"},
		{Path: "/c/rpc-server", Kind: "rpc_server", Allowed: true, Commit: "8888eee", RPCProtocol: "10.0.2"},
	}); err != nil {
		t.Fatal(err)
	}
	if b, ok, err := svc.MatchingRPCBinary(ctx, server.ID, head); err != nil || !ok || b.Path != "/c/rpc-server" {
		t.Fatalf("protocol matches: match = %q %v %v, want /c/rpc-server", b.Path, ok, err)
	}
}

// Review Focus 3: one mismatching and one matching row pick the matching
// one; a protocol mismatch is no match even with the same commit.
func TestMatchingRPCBinarySkipsProtocolMismatch(t *testing.T) {
	ctx := context.Background()
	svc, store := newServerTestService(t, rpcMemberTestNow)
	server := createTestServer(t, svc, "M", "m.example.test")
	head := routing.ServerBinary{Commit: "2bc46ba", RPCProtocol: "10.0.2"}
	if err := store.ReplaceAgentServerBinaries(ctx, server.ID, []routing.ServerBinary{
		{Path: "/a/rpc-server", Kind: "rpc_server", Allowed: true, Commit: "2bc46ba", RPCProtocol: "10.0.1"},
		{Path: "/b/rpc-server", Kind: "rpc_server", Allowed: true, Commit: "unknown", Build: "0", RPCProtocol: "10.0.2"},
	}); err != nil {
		t.Fatal(err)
	}
	if b, ok, err := svc.MatchingRPCBinary(ctx, server.ID, head); err != nil || !ok || b.Path != "/b/rpc-server" {
		t.Fatalf("match = %q %v %v, want the protocol-matching /b/rpc-server", b.Path, ok, err)
	}
	if err := store.ReplaceAgentServerBinaries(ctx, server.ID, []routing.ServerBinary{
		{Path: "/a/rpc-server", Kind: "rpc_server", Allowed: true, Commit: "2bc46ba", RPCProtocol: "10.0.1"},
	}); err != nil {
		t.Fatal(err)
	}
	if b, ok, err := svc.MatchingRPCBinary(ctx, server.ID, head); err != nil || ok {
		t.Fatalf("protocol mismatch: match = %q %v %v, want none", b.Path, ok, err)
	}
}

// scanRPCBinaries explains a server without a match, in the order
// no_rpc_binary, protocol_mismatch, build_mismatch, version_unknown.
func TestRPCBinaryRejection(t *testing.T) {
	ctx := context.Background()
	svc, store := newServerTestService(t, rpcMemberTestNow)
	server := createTestServer(t, svc, "M", "m.example.test")
	head := routing.ServerBinary{Commit: "2bc46ba", RPCProtocol: "10.0.2"}
	for _, tc := range []struct {
		name         string
		rows         []routing.ServerBinary
		head         routing.ServerBinary
		reason, prot string
	}{
		{"none", nil, head, rpcRejectNoBinary, ""},
		{"only ineligible rows", []routing.ServerBinary{
			{Path: "/a/llama-server", Kind: "llama_server", Allowed: true, Commit: "2bc46ba"},
			{Path: "/b/rpc-server", Kind: "rpc_server", Allowed: false, Commit: "2bc46ba"},
			{Path: "wsl://Ubuntu-24.04/opt/rpc-server", Kind: "rpc_server", Allowed: true, Commit: "2bc46ba"},
		}, head, rpcRejectNoBinary, ""},
		{"protocol before build mismatch, first by path", []routing.ServerBinary{
			{Path: "/c/rpc-server", Kind: "rpc_server", Allowed: true, Commit: "9999fff"},
			{Path: "/e/rpc-server", Kind: "rpc_server", Allowed: true, RPCProtocol: "9.0.0"},
			{Path: "/d/rpc-server", Kind: "rpc_server", Allowed: true, RPCProtocol: "10.0.1"},
		}, head, rpcRejectProtocolMismatch, "10.0.1"},
		{"build mismatch", []routing.ServerBinary{
			{Path: "/c/rpc-server", Kind: "rpc_server", Allowed: true, Commit: "9999fff"},
			{Path: "/d/rpc-server", Kind: "rpc_server", Allowed: true},
		}, head, rpcRejectBuildMismatch, ""},
		{"version unknown", []routing.ServerBinary{
			{Path: "/c/rpc-server", Kind: "rpc_server", Allowed: true, Commit: "unknown", Build: "0"},
		}, head, rpcRejectVersionUnknown, ""},
		{"head unknown", []routing.ServerBinary{
			{Path: "/c/rpc-server", Kind: "rpc_server", Allowed: true, Commit: "2bc46ba"},
		}, routing.ServerBinary{Commit: "unknown"}, rpcRejectHeadVersionUnknown, ""},
	} {
		if err := store.ReplaceAgentServerBinaries(ctx, server.ID, tc.rows); err != nil {
			t.Fatal(err)
		}
		scan, err := svc.scanRPCBinaries(ctx, server.ID, tc.head)
		if err != nil || scan.matched || scan.reason != tc.reason || scan.memberProtocol != tc.prot {
			t.Errorf("%s: scan = %+v %v, want reason %q protocol %q", tc.name, scan, err, tc.reason, tc.prot)
		}
	}
}

func TestServerBinaryDTORPCProtocol(t *testing.T) {
	dto := serverBinaryDTO(routing.ServerBinary{Path: "/opt/rpc-server", Kind: BinaryKindRPCServer, RPCProtocol: "10.0.2"})
	if dto.RPCProtocol != "10.0.2" {
		t.Fatalf("dto.RPCProtocol = %q", dto.RPCProtocol)
	}
}
