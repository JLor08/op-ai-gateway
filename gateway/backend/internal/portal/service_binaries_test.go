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

func TestMatchingRPCBinaryByCommitThenBuild(t *testing.T) {
	ctx := context.Background()
	svc, store := newServerTestService(t, rpcMemberTestNow)
	server := createTestServer(t, svc, "M", "m.example.test")
	rows := []routing.ServerBinary{
		{Path: "/z/rpc-server", Kind: "rpc_server", Commit: "2bc46baa9207d098d03741ac394e7faaf1a955b6", Build: "9"},
		{Path: "/b/rpc-server", Kind: "rpc_server", Commit: "2BC46BA", Build: "1"},
		{Path: "/a/rpc-server", Kind: "rpc_server", Commit: "ffffffff", Build: "1"},
		{Path: "/c/llama-server", Kind: "llama_server", Commit: "2bc46ba"},
		{Path: "/d/rpc-server", Kind: "rpc_server", Build: "b42"},
		{Path: "/e/rpc-server", Kind: "rpc_server"},
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
	if b, ok, err := svc.binaryVersionFor(ctx, server.ID, "/d/rpc-server"); err != nil || !ok || b.Build != "b42" {
		t.Fatalf("binaryVersionFor = %+v %v %v", b, ok, err)
	}
	if _, ok, _ := svc.binaryVersionFor(ctx, server.ID, "/nope"); ok {
		t.Fatal("unknown path must not be found")
	}
}
