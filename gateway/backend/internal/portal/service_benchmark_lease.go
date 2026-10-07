// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// benchmarkOverrideLeaseKeyPrefix keys one server's override lease in
// system_settings: benchmark_override_lease:<server_id>. The row is internal
// and runtime-managed, like netbird_token_id: SystemSettingsView ignores the
// key, no route exposes it, and it is not an input of the runtime-config
// document, so writing it owes no notification.
const benchmarkOverrideLeaseKeyPrefix = "benchmark_override_lease:"

// BenchmarkOverrideLease is what a benchmark run on one server still owes the
// store if the process dies: the specs to pin again, and the transient
// force_stopped overrides to clear. Both lists are sorted.
type BenchmarkOverrideLease struct {
	Repin             []string `json:"repin,omitempty"`
	ClearForceStopped []string `json:"clear_force_stopped,omitempty"`
}

// Empty reports whether the lease owes nothing.
func (l BenchmarkOverrideLease) Empty() bool {
	return len(l.Repin) == 0 && len(l.ClearForceStopped) == 0
}

// SetBenchmarkOverrideLease replaces serverID's whole lease row; an empty lease
// releases it, which writes the empty string. system_settings is an upsert
// with no delete, so the empty string is what "no lease" means.
//
// WHO AUTHORIZED THIS. Its callers, as for SetBenchmarkRuntimeSpecAdminState:
// a benchmark run whose trigger passed AuthorizeBenchmarkScope, and the
// gateway's lease reconciler, which only settles what such a run recorded. The
// row names launch-spec ids and nothing else.
//
// A Service without a settings store (a minimal test Service) holds no lease:
// the write is a no-op.
func (s *Service) SetBenchmarkOverrideLease(ctx context.Context, serverID string, lease BenchmarkOverrideLease) error {
	if s.settings == nil {
		return nil
	}
	value := ""
	if !lease.Empty() {
		raw, err := json.Marshal(lease)
		if err != nil {
			return fmt.Errorf("encode the benchmark override lease of server %s: %w", serverID, err)
		}
		value = string(raw)
	}
	if err := s.settings.SetSystemSetting(ctx, benchmarkOverrideLeaseKeyPrefix+serverID, value, s.clock().UTC()); err != nil {
		return fmt.Errorf("write the benchmark override lease of server %s: %w", serverID, err)
	}
	return nil
}

// BenchmarkOverrideLeases returns every held lease, by server id. A released
// row (the empty string) and a lease that owes nothing are left out. A value
// that does not parse is logged at Warn and skipped, never guessed at, not
// even the half that reads: its row stays as it is until a run on that server
// writes it again. A Service without a settings store holds no lease.
func (s *Service) BenchmarkOverrideLeases(ctx context.Context) (map[string]BenchmarkOverrideLease, error) {
	leases := map[string]BenchmarkOverrideLease{}
	if s.settings == nil {
		return leases, nil
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the benchmark override leases: %w", err)
	}
	for key, value := range values {
		serverID, isLease := strings.CutPrefix(key, benchmarkOverrideLeaseKeyPrefix)
		if !isLease || value == "" {
			continue
		}
		var lease BenchmarkOverrideLease
		if err := json.Unmarshal([]byte(value), &lease); err != nil {
			slog.Warn("benchmark: an override lease does not parse and is skipped", "server_id", serverID, "err", err)
			continue
		}
		if !lease.Empty() {
			leases[serverID] = lease
		}
	}
	return leases, nil
}
