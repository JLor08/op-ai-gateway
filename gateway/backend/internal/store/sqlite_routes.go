// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"op-ai-gateway/internal/routing"
	"strings"
	"time"
)

var _ routing.Store = (*SQLiteStore)(nil)

func (s *SQLiteStore) CreateAIServer(ctx context.Context, host routing.AIServer) error {
	_, err := s.exec(ctx, `
		insert into ai_servers (
			id, name, domain, server_path_suffix, provider, endpoint, status, health_status, last_seen_at,
			created_at, updated_at,
			netbird_enabled, netbird_setup_key_id, netbird_group_id, netbird_peer_id, netbird_connected,
			netbird_group_ids, netbird_peer_managed, netbird_policy_override, netbird_allow_ping,
			netbird_ping_exclude, agent_presence_timeout_seconds,
			estimated_watts, idle_watts, price_per_kwh, pue, price_unit, system_group_id,
			certificate_override, https_switch_override
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		host.ID,
		host.Name,
		host.Domain,
		host.ServerPathSuffix,
		host.Provider,
		host.Endpoint,
		host.Status,
		host.HealthStatus,
		host.LastSeenAt,
		host.CreatedAt,
		host.UpdatedAt,
		host.NetbirdEnabled,
		host.NetbirdSetupKeyID,
		host.NetbirdGroupID,
		host.NetbirdPeerID,
		host.NetbirdConnected,
		host.NetbirdGroupIDs,
		host.NetbirdPeerManaged,
		host.NetbirdPolicyOverride,
		host.NetbirdAllowPing,
		host.NetbirdPingExclude,
		host.AgentPresenceTimeoutSeconds,
		host.EstimatedWatts,
		host.IdleWatts,
		host.PricePerKwh,
		host.Pue,
		host.PriceUnit,
		host.SystemGroupID,
		host.CertificateOverride,
		host.HTTPSSwitchOverride,
	)
	if err != nil {
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create host: %w", err)
	}
	return nil
}

func (s *SQLiteStore) UpdateAIServer(ctx context.Context, host routing.AIServer) error {
	result, err := s.exec(ctx, `
		update ai_servers
		set name = ?, domain = ?, server_path_suffix = ?, provider = ?, endpoint = ?, status = ?, health_status = ?,
			last_seen_at = ?, updated_at = ?,
			netbird_enabled = ?, netbird_setup_key_id = ?, netbird_group_id = ?, netbird_peer_id = ?, netbird_connected = ?,
			netbird_group_ids = ?, netbird_peer_managed = ?, netbird_policy_override = ?, netbird_allow_ping = ?,
			netbird_ping_exclude = ?, agent_presence_timeout_seconds = ?,
			estimated_watts = ?, idle_watts = ?, price_per_kwh = ?, pue = ?, price_unit = ?, system_group_id = ?,
			certificate_override = ?, https_switch_override = ?
		where id = ?`,
		host.Name,
		host.Domain,
		host.ServerPathSuffix,
		host.Provider,
		host.Endpoint,
		host.Status,
		host.HealthStatus,
		host.LastSeenAt,
		host.UpdatedAt,
		host.NetbirdEnabled,
		host.NetbirdSetupKeyID,
		host.NetbirdGroupID,
		host.NetbirdPeerID,
		host.NetbirdConnected,
		host.NetbirdGroupIDs,
		host.NetbirdPeerManaged,
		host.NetbirdPolicyOverride,
		host.NetbirdAllowPing,
		host.NetbirdPingExclude,
		host.AgentPresenceTimeoutSeconds,
		host.EstimatedWatts,
		host.IdleWatts,
		host.PricePerKwh,
		host.Pue,
		host.PriceUnit,
		host.SystemGroupID,
		host.CertificateOverride,
		host.HTTPSSwitchOverride,
		host.ID,
	)
	if err != nil {
		return fmt.Errorf("update host: %w", err)
	}
	return requireAffected(result)
}

func (s *SQLiteStore) AIServerByID(ctx context.Context, id string) (routing.AIServer, error) {
	row := s.queryRow(ctx, `
		select id, name, domain, server_path_suffix, provider, endpoint, status, health_status, last_seen_at,
			created_at, updated_at,
			netbird_enabled, netbird_setup_key_id, netbird_group_id, netbird_peer_id, netbird_connected,
			netbird_group_ids, netbird_peer_managed, netbird_policy_override, netbird_allow_ping,
			netbird_ping_exclude, agent_presence_timeout_seconds,
			estimated_watts, idle_watts, price_per_kwh, pue, price_unit, system_group_id,
			certificate_override, https_switch_override
		from ai_servers
		where id = ?`, id)
	return scanAIServer(row)
}

func (s *SQLiteStore) AIServers(ctx context.Context) ([]routing.AIServer, error) {
	rows, err := s.query(ctx, `
		select id, name, domain, server_path_suffix, provider, endpoint, status, health_status, last_seen_at,
			created_at, updated_at,
			netbird_enabled, netbird_setup_key_id, netbird_group_id, netbird_peer_id, netbird_connected,
			netbird_group_ids, netbird_peer_managed, netbird_policy_override, netbird_allow_ping,
			netbird_ping_exclude, agent_presence_timeout_seconds,
			estimated_watts, idle_watts, price_per_kwh, pue, price_unit, system_group_id,
			certificate_override, https_switch_override
		from ai_servers
		order by id`)
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	defer rows.Close()
	return scanAIServers(rows)
}

// SetServerHealth writes only the derived health_status (and updated_at) with a
// targeted UPDATE. It deliberately avoids a full-row load-modify-write so it
// cannot race the telemetry handler's own updated_at write on the same row.
func (s *SQLiteStore) SetServerHealth(ctx context.Context, serverID, health string) error {
	result, err := s.exec(ctx, `
		update ai_servers
		set health_status = ?, updated_at = ?
		where id = ?`,
		health,
		time.Now().UTC(),
		serverID,
	)
	if err != nil {
		return fmt.Errorf("set server health: %w", err)
	}
	return requireAffected(result)
}

// UpdateServerNetbirdKey records the NetBird enabled flag + setup-key id +
// tracking-group id with a targeted UPDATE (only those three columns), so it
// cannot race a concurrent full-row or state write on the same row. enabled is
// true from the create hook + enroll/regenerate path, so enrolling a
// non-NetBird server flips netbird_enabled on.
func (s *SQLiteStore) UpdateServerNetbirdKey(ctx context.Context, id string, enabled bool, setupKeyID, groupID string) error {
	result, err := s.exec(ctx, `
		update ai_servers
		set netbird_enabled = ?, netbird_setup_key_id = ?, netbird_group_id = ?
		where id = ?`,
		enabled,
		setupKeyID,
		groupID,
		id,
	)
	if err != nil {
		return fmt.Errorf("update server netbird key: %w", err)
	}
	return requireAffected(result)
}

// UpdateServerNetbirdLink is the system-admin linkage editor: it sets the
// NetBird enabled flag + peer id and RESETS netbird_connected to 0 with a
// targeted UPDATE (only those three columns), so a stale connection state is
// not shown until the sync loop re-confirms from the new peer id.
func (s *SQLiteStore) UpdateServerNetbirdLink(ctx context.Context, id string, enabled bool, peerID string) error {
	result, err := s.exec(ctx, `
		update ai_servers
		set netbird_enabled = ?, netbird_peer_id = ?, netbird_connected = 0
		where id = ?`,
		enabled,
		peerID,
		id,
	)
	if err != nil {
		return fmt.Errorf("update server netbird link: %w", err)
	}
	return requireAffected(result)
}

// UpdateServerNetbirdState writes the peer-synced server state — the domain
// (from the peer's DNS name), the peer id, and the connected flag — with a
// targeted UPDATE (only those three columns), so the peer-sync loop cannot race
// a concurrent full-row write.
func (s *SQLiteStore) UpdateServerNetbirdState(ctx context.Context, id, domain, peerID string, connected bool) error {
	result, err := s.exec(ctx, `
		update ai_servers
		set domain = ?, netbird_peer_id = ?, netbird_connected = ?
		where id = ?`,
		domain,
		peerID,
		connected,
		id,
	)
	if err != nil {
		return fmt.Errorf("update server netbird state: %w", err)
	}
	return requireAffected(result)
}

// UpdateServerNetbirdGroups writes the opaque JSON mirror of the peer's policy
// groups (netbird_group_ids) with a targeted UPDATE (only that one column), so
// it cannot race a concurrent full-row or state write on the same row.
func (s *SQLiteStore) UpdateServerNetbirdGroups(ctx context.Context, id, groupsJSON string) error {
	result, err := s.exec(ctx, `
		update ai_servers
		set netbird_group_ids = ?
		where id = ?`,
		groupsJSON,
		id,
	)
	if err != nil {
		return fmt.Errorf("update server netbird groups: %w", err)
	}
	return requireAffected(result)
}

// UpdateServerNetbirdPeerManaged writes only netbird_peer_managed — the provenance
// flag marking a gateway-created NetBird peer — with a targeted UPDATE (only that
// one column), so it cannot race a concurrent full-row or state write on the same
// row. An unknown id is ErrNotFound.
func (s *SQLiteStore) UpdateServerNetbirdPeerManaged(ctx context.Context, id string, managed bool) error {
	result, err := s.exec(ctx, `
		update ai_servers
		set netbird_peer_managed = ?
		where id = ?`,
		managed,
		id,
	)
	if err != nil {
		return fmt.Errorf("update server netbird peer managed: %w", err)
	}
	return requireAffected(result)
}

// UpdateServerNetbirdPolicyOverride writes only netbird_policy_override — the
// per-server policy opt-in/opt-out override — with a targeted UPDATE (only that
// one column), so it cannot race a concurrent full-row or state write on the
// same row. An unknown id is ErrNotFound.
func (s *SQLiteStore) UpdateServerNetbirdPolicyOverride(ctx context.Context, id, override string) error {
	result, err := s.exec(ctx, `
		update ai_servers
		set netbird_policy_override = ?
		where id = ?`,
		override, id,
	)
	if err != nil {
		return fmt.Errorf("update server netbird policy override: %w", err)
	}
	return requireAffected(result)
}

// UpdateServerNetbirdAllowPing writes only netbird_allow_ping — the per-server
// flag letting the gateway ICMP-ping this server (managed policy op-gw-ping-servers)
// — with a targeted UPDATE (only that one column), so it cannot race a concurrent
// full-row or state write on the same row. An unknown id is ErrNotFound.
func (s *SQLiteStore) UpdateServerNetbirdAllowPing(ctx context.Context, id string, allow bool) error {
	result, err := s.exec(ctx, `
		update ai_servers
		set netbird_allow_ping = ?
		where id = ?`,
		allow, id,
	)
	if err != nil {
		return fmt.Errorf("update server netbird allow ping: %w", err)
	}
	return requireAffected(result)
}

// UpdateServerNetbirdPingExclude writes only netbird_ping_exclude — the per-server
// ping opt-out (a targeted UPDATE that touches only that one column), so it cannot
// race a concurrent full-row or state write on the same row. An unknown id is
// ErrNotFound.
func (s *SQLiteStore) UpdateServerNetbirdPingExclude(ctx context.Context, id string, exclude bool) error {
	result, err := s.exec(ctx, `
		update ai_servers
		set netbird_ping_exclude = ?
		where id = ?`,
		exclude, id,
	)
	if err != nil {
		return fmt.Errorf("update server netbird ping exclude: %w", err)
	}
	return requireAffected(result)
}

// UpdateServerEnergyConfig writes only the five per-server energy-config columns
// (estimated_watts, idle_watts, price_per_kwh, pue, price_unit) with a targeted
// UPDATE, so it cannot race a concurrent full-row write on the same row. An
// unknown id is ErrNotFound.
func (s *SQLiteStore) UpdateServerEnergyConfig(ctx context.Context, id string, estimatedWatts, idleWatts, pricePerKwh, pue float64, priceUnit string) error {
	result, err := s.exec(ctx, `
		update ai_servers
		set estimated_watts = ?, idle_watts = ?, price_per_kwh = ?, pue = ?, price_unit = ?
		where id = ?`,
		estimatedWatts, idleWatts, pricePerKwh, pue, priceUnit, id,
	)
	if err != nil {
		return fmt.Errorf("update server energy config: %w", err)
	}
	return requireAffected(result)
}

func (s *SQLiteStore) DeleteAIServer(ctx context.Context, id string) error {
	res, err := s.exec(ctx, `delete from ai_servers where id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete server: %w", err)
	}
	return requireAffected(res)
}

func (s *SQLiteStore) SetServerOwners(ctx context.Context, serverID string, userIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set owners tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, s.dl.rebind(`delete from server_owners where server_id = ?`), serverID); err != nil {
		return fmt.Errorf("clear owners: %w", err)
	}
	for _, userID := range userIDs {
		if _, err := tx.ExecContext(ctx, s.dl.rebind(`insert into server_owners (server_id, user_id) values (?, ?) on conflict do nothing`), serverID, userID); err != nil {
			if s.dl.isForeignKeyViolation(err) {
				return ErrNotFound
			}
			if s.dl.isUniqueViolation(err) {
				return ErrConflict
			}
			return fmt.Errorf("insert owner: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit owners: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ServerOwners(ctx context.Context, serverID string) ([]string, error) {
	rows, err := s.query(ctx, `select user_id from server_owners where server_id = ? order by user_id`, serverID)
	if err != nil {
		return nil, fmt.Errorf("list owners: %w", err)
	}
	defer rows.Close()
	owners := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan owner: %w", err)
		}
		owners = append(owners, id)
	}
	return owners, rows.Err()
}

func (s *SQLiteStore) ServersByOwner(ctx context.Context, userID string) ([]routing.AIServer, error) {
	rows, err := s.query(ctx, `
		select s.id, s.name, s.domain, s.server_path_suffix, s.provider, s.endpoint, s.status, s.health_status, s.last_seen_at, s.created_at, s.updated_at,
			s.netbird_enabled, s.netbird_setup_key_id, s.netbird_group_id, s.netbird_peer_id, s.netbird_connected,
			s.netbird_group_ids, s.netbird_peer_managed, s.netbird_policy_override, s.netbird_allow_ping,
			s.netbird_ping_exclude, s.agent_presence_timeout_seconds,
			s.estimated_watts, s.idle_watts, s.price_per_kwh, s.pue, s.price_unit, s.system_group_id,
			s.certificate_override, s.https_switch_override
		from ai_servers s
		join server_owners o on o.server_id = s.id
		where o.user_id = ?
		order by s.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("servers by owner: %w", err)
	}
	defer rows.Close()
	return scanAIServers(rows)
}

// UpdateServerSystemGroup writes only system_group_id — the admin-group
// permissions Phase B containment root (migration v50) — with a targeted
// UPDATE (only that one column), so it cannot race a concurrent full-row or
// state write on the same row. An unknown id is ErrNotFound.
func (s *SQLiteStore) UpdateServerSystemGroup(ctx context.Context, serverID, systemGroupID string) error {
	result, err := s.exec(ctx, `
		update ai_servers
		set system_group_id = ?
		where id = ?`,
		systemGroupID, serverID,
	)
	if err != nil {
		return fmt.Errorf("update server system group: %w", err)
	}
	return requireAffected(result)
}

// UpdateServerCertificateOverride writes only certificate_override (the
// per-server ACME opt-in/opt-out). Unknown id -> ErrNotFound.
func (s *SQLiteStore) UpdateServerCertificateOverride(ctx context.Context, id, override string) error {
	result, err := s.exec(ctx, `
		update ai_servers
		set certificate_override = ?
		where id = ?`, override, id)
	if err != nil {
		return fmt.Errorf("update server certificate override: %w", err)
	}
	return requireAffected(result)
}

// UpdateServerHTTPSSwitchOverride writes only https_switch_override (P4's
// per-server https-auto-switch opt-in/opt-out). Unknown id -> ErrNotFound.
// Mirrors UpdateServerCertificateOverride.
func (s *SQLiteStore) UpdateServerHTTPSSwitchOverride(ctx context.Context, id, override string) error {
	result, err := s.exec(ctx, `
		update ai_servers
		set https_switch_override = ?
		where id = ?`, override, id)
	if err != nil {
		return fmt.Errorf("update server https switch override: %w", err)
	}
	return requireAffected(result)
}

// SetServerAdminGroup links serverID to groupID (server_admin_groups),
// idempotent on the (server_id, group_id) unique pair — mirrors
// SetProjectGroup in sqlite_projects.go. A missing serverID or groupID
// surfaces ErrNotFound (FK violation).
func (s *SQLiteStore) SetServerAdminGroup(ctx context.Context, serverID, groupID string) error {
	_, err := s.exec(ctx, `insert into server_admin_groups (server_id, group_id, created_at)
		values (?, ?, ?) on conflict (server_id, group_id) do nothing`,
		serverID, groupID, time.Now().UTC())
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("set server admin group: %w", err)
	}
	return nil
}

// RemoveServerAdminGroup unlinks serverID from groupID. A no-op (non-error)
// when the link does not exist — mirrors RemoveProjectGroup.
func (s *SQLiteStore) RemoveServerAdminGroup(ctx context.Context, serverID, groupID string) error {
	_, err := s.exec(ctx, `delete from server_admin_groups where server_id = ? and group_id = ?`, serverID, groupID)
	if err != nil {
		return fmt.Errorf("remove server admin group: %w", err)
	}
	return nil
}

// ServerAdminGroups lists every admin-group id linked to serverID, ordered by
// created_at then group_id — mirrors ProjectGroups. Always non-nil, empty
// when none.
func (s *SQLiteStore) ServerAdminGroups(ctx context.Context, serverID string) ([]string, error) {
	rows, err := s.query(ctx, `select group_id from server_admin_groups where server_id = ? order by created_at, group_id`, serverID)
	if err != nil {
		return nil, fmt.Errorf("list server admin groups: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan server admin group: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ServersByAdminGroups returns every server linked to ANY of groupIDs, deduped
// by server id (a server linked to more than one of the given groups is
// returned once) — mirrors ServersByOwner's row build. An empty groupIDs
// returns an empty slice without issuing a query.
func (s *SQLiteStore) ServersByAdminGroups(ctx context.Context, groupIDs []string) ([]routing.AIServer, error) {
	if len(groupIDs) == 0 {
		return make([]routing.AIServer, 0), nil
	}
	placeholders := make([]string, len(groupIDs))
	args := make([]any, len(groupIDs))
	for i, id := range groupIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := s.query(ctx, `
		select distinct s.id, s.name, s.domain, s.server_path_suffix, s.provider, s.endpoint, s.status, s.health_status, s.last_seen_at, s.created_at, s.updated_at,
			s.netbird_enabled, s.netbird_setup_key_id, s.netbird_group_id, s.netbird_peer_id, s.netbird_connected,
			s.netbird_group_ids, s.netbird_peer_managed, s.netbird_policy_override, s.netbird_allow_ping,
			s.netbird_ping_exclude, s.agent_presence_timeout_seconds,
			s.estimated_watts, s.idle_watts, s.price_per_kwh, s.pue, s.price_unit, s.system_group_id,
			s.certificate_override, s.https_switch_override
		from ai_servers s
		join server_admin_groups g on g.server_id = s.id
		where g.group_id in (`+strings.Join(placeholders, ",")+`)
		order by s.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("servers by admin groups: %w", err)
	}
	defer rows.Close()
	return scanAIServers(rows)
}

func (s *SQLiteStore) UpsertTelemetry(ctx context.Context, telemetry routing.ServerTelemetry) error {
	_, err := s.exec(ctx, `
		insert into server_telemetry (
			server_id, reported_at, agent_version, os, arch, cpu_load,
			ram_used_bytes, ram_total_bytes, gpu_count, vram_used_bytes,
			vram_total_bytes, active_requests, queue_depth, latency_ms,
			error_rate, provider_health, capabilities, raw_summary, updated_at
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		on conflict(server_id) do update set
			reported_at = excluded.reported_at,
			agent_version = excluded.agent_version,
			os = excluded.os,
			arch = excluded.arch,
			cpu_load = excluded.cpu_load,
			ram_used_bytes = excluded.ram_used_bytes,
			ram_total_bytes = excluded.ram_total_bytes,
			gpu_count = excluded.gpu_count,
			vram_used_bytes = excluded.vram_used_bytes,
			vram_total_bytes = excluded.vram_total_bytes,
			active_requests = excluded.active_requests,
			queue_depth = excluded.queue_depth,
			latency_ms = excluded.latency_ms,
			error_rate = excluded.error_rate,
			provider_health = excluded.provider_health,
			capabilities = excluded.capabilities,
			raw_summary = excluded.raw_summary,
			updated_at = excluded.updated_at`,
		telemetry.ServerID,
		telemetry.ReportedAt,
		telemetry.AgentVersion,
		telemetry.OS,
		telemetry.Arch,
		telemetry.CPULoad,
		telemetry.RAMUsedBytes,
		telemetry.RAMTotalBytes,
		telemetry.GPUCount,
		telemetry.VRAMUsedBytes,
		telemetry.VRAMTotalBytes,
		telemetry.ActiveRequests,
		telemetry.QueueDepth,
		telemetry.LatencyMS,
		telemetry.ErrorRate,
		telemetry.ProviderHealth,
		telemetry.Capabilities,
		telemetry.RawSummary,
		telemetry.UpdatedAt,
	)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("upsert telemetry: %w", err)
	}
	return nil
}

func (s *SQLiteStore) TelemetryByServer(ctx context.Context, serverID string) (routing.ServerTelemetry, bool, error) {
	row := s.queryRow(ctx, `
		select server_id, reported_at, agent_version, os, arch, cpu_load,
			ram_used_bytes, ram_total_bytes, gpu_count, vram_used_bytes,
			vram_total_bytes, active_requests, queue_depth, latency_ms,
			error_rate, provider_health, capabilities, raw_summary, updated_at
		from server_telemetry
		where server_id = ?`, serverID)
	telemetry, err := scanTelemetry(row)
	if errors.Is(err, ErrNotFound) {
		return routing.ServerTelemetry{}, false, nil
	}
	if err != nil {
		return routing.ServerTelemetry{}, false, err
	}
	return telemetry, true, nil
}

func (s *SQLiteStore) UpsertServerHardware(ctx context.Context, hw routing.ServerHardware) error {
	_, err := s.exec(ctx, `
		insert into server_hardware (server_id, collected_at, report_json, updated_at)
		values (?, ?, ?, ?)
		on conflict(server_id) do update set
			collected_at = excluded.collected_at,
			report_json = excluded.report_json,
			updated_at = excluded.updated_at`,
		hw.ServerID,
		hw.CollectedAt,
		hw.ReportJSON,
		hw.UpdatedAt,
	)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("upsert server hardware: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ServerHardwareByServer(ctx context.Context, serverID string) (routing.ServerHardware, bool, error) {
	row := s.queryRow(ctx, `
		select server_id, collected_at, report_json, updated_at
		from server_hardware
		where server_id = ?`, serverID)
	hw, err := scanServerHardware(row)
	if errors.Is(err, ErrNotFound) {
		return routing.ServerHardware{}, false, nil
	}
	if err != nil {
		return routing.ServerHardware{}, false, err
	}
	return hw, true, nil
}

func scanServerHardware(row rowScanner) (routing.ServerHardware, error) {
	var hw routing.ServerHardware
	err := row.Scan(&hw.ServerID, &hw.CollectedAt, &hw.ReportJSON, &hw.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.ServerHardware{}, ErrNotFound
	}
	if err != nil {
		return routing.ServerHardware{}, fmt.Errorf("scan server hardware: %w", err)
	}
	return hw, nil
}

func (s *SQLiteStore) UpsertAffinity(ctx context.Context, affinity routing.RouteAffinity) error {
	// affinity.UserID is "" for a service-token affinity (a service has no
	// user). user_id keeps its `references users(id) on delete cascade` FK,
	// which — unlike NOT NULL — only exempts a genuine SQL NULL from the
	// reference-existence check, so an empty Go string must become
	// sql.NullString{} (NULL), not "" (which would be checked against the
	// users table and rejected — the exact cause of the service-token 502).
	// nullableTokenRef is the same helper CreatePlainToken uses for
	// api_tokens.user_id/.service_id.
	_, err := s.exec(ctx, `
		insert into route_affinity (
			id, api_token_id, user_id, model, api_flavor, session_id,
			application_id, server_id, expires_at, last_used_at, created_at, updated_at,
			resolved_model
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		on conflict(api_token_id, model, api_flavor, session_id) do update set
			id = excluded.id,
			user_id = excluded.user_id,
			application_id = excluded.application_id,
			server_id = excluded.server_id,
			expires_at = excluded.expires_at,
			last_used_at = excluded.last_used_at,
			updated_at = excluded.updated_at,
			resolved_model = excluded.resolved_model`,
		affinity.ID,
		affinity.APITokenID,
		nullableTokenRef(affinity.UserID),
		affinity.Model,
		affinity.APIFlavor,
		affinity.SessionID,
		affinity.ApplicationID,
		affinity.ServerID,
		affinity.ExpiresAt,
		affinity.LastUsedAt,
		affinity.CreatedAt,
		affinity.UpdatedAt,
		affinity.ResolvedModel,
	)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("upsert affinity: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Affinity(ctx context.Context, key routing.AffinityKey) (routing.RouteAffinity, bool, error) {
	// coalesce(user_id,'') mirrors tokenColumns: user_id is nullable (a
	// service-token affinity has none), so the Go-side field stays a plain
	// string regardless of NULL vs a real user id.
	row := s.queryRow(ctx, `
		select id, api_token_id, coalesce(user_id,''), model, api_flavor, session_id,
			application_id, server_id, expires_at, last_used_at, created_at, updated_at,
			resolved_model
		from route_affinity
		where api_token_id = ? and model = ? and api_flavor = ? and session_id = ?`,
		key.APITokenID,
		key.Model,
		key.APIFlavor,
		key.SessionID,
	)
	affinity, err := scanAffinity(row)
	if errors.Is(err, ErrNotFound) {
		return routing.RouteAffinity{}, false, nil
	}
	if err != nil {
		return routing.RouteAffinity{}, false, err
	}
	return affinity, true, nil
}

func (s *SQLiteStore) DeleteAffinity(ctx context.Context, key routing.AffinityKey) error {
	_, err := s.exec(ctx, `
		delete from route_affinity
		where api_token_id = ? and model = ? and api_flavor = ? and session_id = ?`,
		key.APITokenID,
		key.Model,
		key.APIFlavor,
		key.SessionID,
	)
	if err != nil {
		return fmt.Errorf("delete affinity: %w", err)
	}
	return nil
}

func requireAffected(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func encodeAPIFlavors(apiFlavors []string) (string, error) {
	if apiFlavors == nil {
		apiFlavors = []string{}
	}
	data, err := json.Marshal(apiFlavors)
	if err != nil {
		return "", fmt.Errorf("encode api flavors: %w", err)
	}
	return string(data), nil
}

func scanAIServer(row rowScanner) (routing.AIServer, error) {
	var host routing.AIServer
	var lastSeenAt sql.NullTime
	var netbirdEnabled, netbirdConnected, netbirdPeerManaged, netbirdAllowPing, netbirdPingExclude int64
	err := row.Scan(
		&host.ID,
		&host.Name,
		&host.Domain,
		&host.ServerPathSuffix,
		&host.Provider,
		&host.Endpoint,
		&host.Status,
		&host.HealthStatus,
		&lastSeenAt,
		&host.CreatedAt,
		&host.UpdatedAt,
		&netbirdEnabled,
		&host.NetbirdSetupKeyID,
		&host.NetbirdGroupID,
		&host.NetbirdPeerID,
		&netbirdConnected,
		&host.NetbirdGroupIDs,
		&netbirdPeerManaged,
		&host.NetbirdPolicyOverride,
		&netbirdAllowPing,
		&netbirdPingExclude,
		&host.AgentPresenceTimeoutSeconds,
		&host.EstimatedWatts,
		&host.IdleWatts,
		&host.PricePerKwh,
		&host.Pue,
		&host.PriceUnit,
		&host.SystemGroupID,
		&host.CertificateOverride,
		&host.HTTPSSwitchOverride,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.AIServer{}, ErrNotFound
	}
	if err != nil {
		return routing.AIServer{}, fmt.Errorf("scan host: %w", err)
	}
	if lastSeenAt.Valid {
		host.LastSeenAt = &lastSeenAt.Time
	}
	host.NetbirdEnabled = netbirdEnabled != 0
	host.NetbirdConnected = netbirdConnected != 0
	host.NetbirdPeerManaged = netbirdPeerManaged != 0
	host.NetbirdAllowPing = netbirdAllowPing != 0
	host.NetbirdPingExclude = netbirdPingExclude != 0
	return host, nil
}

func scanAIServers(rows *sql.Rows) ([]routing.AIServer, error) {
	hosts := make([]routing.AIServer, 0)
	for rows.Next() {
		host, err := scanAIServer(rows)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, host)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate hosts: %w", err)
	}
	return hosts, nil
}

func scanTelemetry(row rowScanner) (routing.ServerTelemetry, error) {
	var telemetry routing.ServerTelemetry
	err := row.Scan(
		&telemetry.ServerID,
		&telemetry.ReportedAt,
		&telemetry.AgentVersion,
		&telemetry.OS,
		&telemetry.Arch,
		&telemetry.CPULoad,
		&telemetry.RAMUsedBytes,
		&telemetry.RAMTotalBytes,
		&telemetry.GPUCount,
		&telemetry.VRAMUsedBytes,
		&telemetry.VRAMTotalBytes,
		&telemetry.ActiveRequests,
		&telemetry.QueueDepth,
		&telemetry.LatencyMS,
		&telemetry.ErrorRate,
		&telemetry.ProviderHealth,
		&telemetry.Capabilities,
		&telemetry.RawSummary,
		&telemetry.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.ServerTelemetry{}, ErrNotFound
	}
	if err != nil {
		return routing.ServerTelemetry{}, fmt.Errorf("scan telemetry: %w", err)
	}
	return telemetry, nil
}

func scanAffinity(row rowScanner) (routing.RouteAffinity, error) {
	var affinity routing.RouteAffinity
	err := row.Scan(
		&affinity.ID,
		&affinity.APITokenID,
		&affinity.UserID,
		&affinity.Model,
		&affinity.APIFlavor,
		&affinity.SessionID,
		&affinity.ApplicationID,
		&affinity.ServerID,
		&affinity.ExpiresAt,
		&affinity.LastUsedAt,
		&affinity.CreatedAt,
		&affinity.UpdatedAt,
		&affinity.ResolvedModel,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.RouteAffinity{}, ErrNotFound
	}
	if err != nil {
		return routing.RouteAffinity{}, fmt.Errorf("scan affinity: %w", err)
	}
	return affinity, nil
}
