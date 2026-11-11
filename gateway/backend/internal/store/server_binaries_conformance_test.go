// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

func seedBinariesServer(ctx context.Context, t *testing.T, s routing.Store, id string, now time.Time) {
	t.Helper()
	if err := s.CreateAIServer(ctx, routing.AIServer{
		ID: id, Name: id, Provider: routing.ProviderVLLM, Endpoint: "http://" + id + ":8000",
		Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create server %s: %v", id, err)
	}
}

func binaryPaths(rows []routing.ServerBinary) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Path)
	}
	return out
}

// TestConformanceServerBinaries pins the server_binaries inventory contract on
// every routing store: agent rows are replaced wholesale per report, manual
// rows and the operator's version override survive a replace, a manual row
// shadows an agent report of the same path, the listing is path-ordered and
// never nil, and an unknown server is ErrNotFound.
func TestConformanceServerBinaries(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	forEachRoutingStore(t, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		seedBinariesServer(ctx, t, s, "srv_bin", now)
		seedBinariesServer(ctx, t, s, "srv_bin2", now)

		empty, err := s.ServerBinaries(ctx, "srv_bin")
		if err != nil || empty == nil || len(empty) != 0 {
			t.Fatalf("empty listing must be non-nil and empty: rows=%v err=%v", empty, err)
		}
		if err := s.ReplaceAgentServerBinaries(ctx, "srv_nope", nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("replace on unknown server: err=%v, want ErrNotFound", err)
		}

		agent := func(path, kind, version string) routing.ServerBinary {
			return routing.ServerBinary{
				Path: path, Kind: kind, Version: version, Build: "7", Commit: "2bc46ba",
				VersionSource: "agent", Source: "agent", Allowed: true, ReportedAt: now,
			}
		}
		if err := s.ReplaceAgentServerBinaries(ctx, "srv_bin", []routing.ServerBinary{
			agent("/b/llama-server", "llama-server", "b1"),
			agent("/a/ggml-rpc-server", "ggml-rpc-server", "b1"),
		}); err != nil {
			t.Fatalf("first replace: %v", err)
		}
		if err := s.ReplaceAgentServerBinaries(ctx, "srv_bin2", []routing.ServerBinary{agent("/x/llama-server", "llama-server", "b9")}); err != nil {
			t.Fatalf("other server replace: %v", err)
		}
		got, err := s.ServerBinaries(ctx, "srv_bin")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if want := []string{"/a/ggml-rpc-server", "/b/llama-server"}; len(got) != 2 || got[0].Path != want[0] || got[1].Path != want[1] {
			t.Fatalf("listing must be path-ordered, got %v", binaryPaths(got))
		}
		first := got[1]
		if first.ServerID != "srv_bin" || first.Kind != "llama-server" || first.Version != "b1" || first.Build != "7" ||
			first.Commit != "2bc46ba" || first.VersionSource != "agent" || first.Source != "agent" || !first.Allowed ||
			first.VersionOverride != "" || !first.ReportedAt.Equal(now) {
			t.Fatalf("agent row did not round-trip: %+v", first)
		}

		// Operator work: a manual entry, and a version override on an agent row.
		if err := s.UpsertManualServerBinary(ctx, routing.ServerBinary{
			ServerID: "srv_bin", Path: "/m/rpc-server", Kind: "rpc-server", Version: "b5", Build: "5",
			Commit: "abcdef0", ReportedAt: now,
		}); err != nil {
			t.Fatalf("manual upsert: %v", err)
		}
		if err := s.SetServerBinaryVersionOverride(ctx, "srv_bin", "/b/llama-server", "b-custom"); err != nil {
			t.Fatalf("override: %v", err)
		}
		if err := s.SetServerBinaryVersionOverride(ctx, "srv_bin", "/missing", "x"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("override on missing path: err=%v, want ErrNotFound", err)
		}

		// A new report: one path stays (changed version), one vanishes, one is new,
		// and one collides with the manual row.
		if err := s.ReplaceAgentServerBinaries(ctx, "srv_bin", []routing.ServerBinary{
			agent("/b/llama-server", "llama-server", "b2"),
			agent("/c/new-server", "llama-server", "b3"),
			agent("/m/rpc-server", "rpc-server", "agent-says"),
		}); err != nil {
			t.Fatalf("second replace: %v", err)
		}
		got, err = s.ServerBinaries(ctx, "srv_bin")
		if err != nil {
			t.Fatalf("list after second replace: %v", err)
		}
		if p := binaryPaths(got); len(p) != 3 || p[0] != "/b/llama-server" || p[1] != "/c/new-server" || p[2] != "/m/rpc-server" {
			t.Fatalf("after replace want [/b/llama-server /c/new-server /m/rpc-server], got %v", p)
		}
		if got[0].Version != "b2" || got[0].VersionOverride != "b-custom" || got[0].Source != "agent" {
			t.Fatalf("the version override must survive a replace while the report refreshes: %+v", got[0])
		}
		if got[1].VersionOverride != "" {
			t.Fatalf("a new row starts without an override: %+v", got[1])
		}
		if got[2].Source != "manual" || got[2].Version != "b5" || got[2].Kind != "rpc-server" {
			t.Fatalf("a manual row must shadow an agent report of the same path: %+v", got[2])
		}

		// An empty report clears the agent rows and keeps the manual one.
		if err := s.ReplaceAgentServerBinaries(ctx, "srv_bin", nil); err != nil {
			t.Fatalf("empty replace: %v", err)
		}
		got, _ = s.ServerBinaries(ctx, "srv_bin")
		if len(got) != 1 || got[0].Path != "/m/rpc-server" || got[0].Source != "manual" {
			t.Fatalf("empty replace must keep only the manual row, got %+v", got)
		}
		if other, _ := s.ServerBinaries(ctx, "srv_bin2"); len(other) != 1 {
			t.Fatalf("a replace must not touch another server, got %+v", other)
		}

		// Manual upsert updates in place; it refuses a path an agent row owns.
		if err := s.UpsertManualServerBinary(ctx, routing.ServerBinary{
			ServerID: "srv_bin", Path: "/m/rpc-server", Kind: "rpc-server", Version: "b6", ReportedAt: now,
		}); err != nil {
			t.Fatalf("manual re-upsert: %v", err)
		}
		got, _ = s.ServerBinaries(ctx, "srv_bin")
		if len(got) != 1 || got[0].Version != "b6" || got[0].VersionSource != "manual" {
			t.Fatalf("manual upsert must update in place with version_source manual, got %+v", got)
		}
		if err := s.UpsertManualServerBinary(ctx, routing.ServerBinary{
			ServerID: "srv_bin2", Path: "/x/llama-server", Kind: "llama-server", Version: "b1", ReportedAt: now,
		}); !errors.Is(err, ErrConflict) {
			t.Fatalf("manual upsert over an agent row: err=%v, want ErrConflict", err)
		}
		if err := s.UpsertManualServerBinary(ctx, routing.ServerBinary{ServerID: "srv_nope", Path: "/p", Kind: "other", ReportedAt: now}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("manual upsert on unknown server: err=%v, want ErrNotFound", err)
		}

		// Delete removes manual rows only.
		if err := s.DeleteManualServerBinary(ctx, "srv_bin2", "/x/llama-server"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleting an agent row through the manual delete: err=%v, want ErrNotFound", err)
		}
		if err := s.DeleteManualServerBinary(ctx, "srv_bin", "/m/rpc-server"); err != nil {
			t.Fatalf("delete manual: %v", err)
		}
		if got, _ := s.ServerBinaries(ctx, "srv_bin"); len(got) != 0 {
			t.Fatalf("manual row must be gone, got %+v", got)
		}

		// Deleting the server removes its inventory.
		if err := s.DeleteAIServer(ctx, "srv_bin2"); err != nil {
			t.Fatalf("delete server: %v", err)
		}
		if got, err := s.ServerBinaries(ctx, "srv_bin2"); err != nil || len(got) != 0 {
			t.Fatalf("server delete must cascade to server_binaries: rows=%v err=%v", got, err)
		}
	})
}
