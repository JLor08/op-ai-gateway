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
