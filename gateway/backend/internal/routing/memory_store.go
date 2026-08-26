// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"op-ai-gateway/internal/storeerr"
	"sort"
	"sync"
	"time"
)

var _ Store = (*MemoryStore)(nil)

type agentTokenRecord struct {
	token      AgentToken
	secretHash string
}

type MemoryStore struct {
	mu           sync.RWMutex
	servers      map[string]AIServer
	telemetry    map[string]ServerTelemetry
	hardware     map[string]ServerHardware
	samples      map[string][]TelemetrySample          // per-server, append-ordered by insert
	availSamples map[string][]ServerAvailabilitySample // per-server, append-ordered
	benchmarks   map[string][]BenchmarkRun             // per-mapping, append-ordered by insert
	affinities   map[AffinityKey]RouteAffinity
	owners       map[string]map[string]struct{}
	// serverAdminGroups mirrors server_admin_groups (admin-group permissions
	// Phase B, migration v50): serverID -> groupID -> the time it was linked
	// (used only for a stable, insertion-ordered listing — mirrors the SQL
	// `order by created_at, group_id`), so a re-link is a natural upsert
	// (first-seen time kept, matching the SQL on-conflict-do-nothing).
	serverAdminGroups map[string]map[string]time.Time
	applications      map[string]Application
	mappings          map[string]ModelMapping
	agentTokens       map[string]agentTokenRecord
	groups            map[string]ModelGroup
	groupMembers      map[string][]GroupMember
	modelSettings     map[string]ModelSetting
	// services / serviceDelegates / serviceAllowed mirror the sqlite
	// services/service_delegates/service_allowed_models tables (Phase 1 service
	// accounts). serviceDelegates maps serviceID -> userID -> canManageSettings;
	// serviceAllowed maps serviceID -> a set of allowed gateway model names.
	services         map[string]Service
	serviceDelegates map[string]map[string]bool
	serviceAllowed   map[string]map[string]struct{}
	// serviceAdminGroups mirrors service_admin_groups (admin-group permissions
	// Phase C, migration v52): serviceID -> groupID -> the time it was linked
	// (used only for a stable, insertion-ordered listing — mirrors the SQL
	// `order by created_at, group_id`), so a re-link is a natural upsert
	// (first-seen time kept, matching the SQL on-conflict-do-nothing).
	// Mirrors serverAdminGroups above.
	serviceAdminGroups map[string]map[string]time.Time
	// resourceGroups / resourceGroupAdminGroups / resourceGroupServers mirror
	// the sqlite resource_groups/resource_group_admin_groups/
	// resource_group_servers tables (Resource Groups Phase 1, migration
	// v54). resourceGroupAdminGroups maps resourceGroupID -> groupID -> the
	// time it was linked (mirrors serviceAdminGroups: which admin groups may
	// MANAGE the resource group); resourceGroupServers maps
	// resourceGroupID -> serverID -> the time it was linked (a DISTINCT
	// relationship — which AI-servers are MEMBERS of the resource group).
	// Both use the same first-seen-time-kept upsert semantics as
	// serverAdminGroups/serviceAdminGroups (mirrors the SQL
	// on-conflict-do-nothing + `order by created_at, <id>`).
	resourceGroups           map[string]ResourceGroup
	resourceGroupAdminGroups map[string]map[string]time.Time
	resourceGroupServers     map[string]map[string]time.Time
	// resourceGroupProvisions mirrors resource_group_provisions (Resource
	// Groups Phase 2 provisioning, migration v55): resourceGroupID -> the set
	// of (target_kind, target_id) pairs it is "provisioned for". Unlike
	// resourceGroupAdminGroups/resourceGroupServers above (ordered by
	// first-seen time to mirror `order by created_at, <id>`),
	// ResourceGroupProvisions is ordered by (kind, target_id) directly
	// (mirrors the SQL `order by target_kind, target_id`), so a plain set —
	// keyed by the (comparable, two-string-field) ResourceGroupProvision
	// value itself — suffices; no per-link timestamp is needed.
	resourceGroupProvisions map[string]map[ResourceGroupProvision]struct{}
	// principalLimits mirrors principal_limits: principalType -> principalID ->
	// LimitConfig (Phase 2 of the service-accounts work).
	principalLimits map[string]map[string]LimitConfig
	// certificates mirrors the sqlite certificates table (migration v57): domain
	// -> its Certificate row. Certificate has no slice/pointer fields, so a
	// plain value-map assignment is a full copy — no copyCertificate helper
	// needed (mirrors e.g. ResourceGroup's by-value storage).
	certificates map[string]Certificate
	// runtimeSpecs mirrors agent_runtime_specs (migration 65, T1): spec id ->
	// its RuntimeSpec row. RuntimeSpec has no slice/pointer fields, so a plain
	// value-map assignment is a full copy (mirrors certificates above).
	// mapping_id is a unique key on the SQL side, NOT the map key here (id is
	// the primary key there too) — UpsertRuntimeSpec enforces the "1 spec per
	// mapping" invariant by hand, scanning for an existing entry with the
	// same MappingID.
	runtimeSpecs map[string]RuntimeSpec
	// runtimeSpecGPUs mirrors agent_runtime_spec_gpus: spec id -> its per-GPU
	// VRAM rows (unordered on write; RuntimeSpecGPUs sorts by GPUIndex on
	// read, mirroring the SQL `order by gpu_index`). Deleting a spec cascades
	// deletion of its entry here (mirrors the table's ON DELETE CASCADE).
	runtimeSpecGPUs map[string][]RuntimeSpecGPU
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		servers:                  map[string]AIServer{},
		telemetry:                map[string]ServerTelemetry{},
		hardware:                 map[string]ServerHardware{},
		samples:                  map[string][]TelemetrySample{},
		availSamples:             map[string][]ServerAvailabilitySample{},
		benchmarks:               map[string][]BenchmarkRun{},
		affinities:               map[AffinityKey]RouteAffinity{},
		owners:                   map[string]map[string]struct{}{},
		serverAdminGroups:        map[string]map[string]time.Time{},
		applications:             map[string]Application{},
		mappings:                 map[string]ModelMapping{},
		agentTokens:              map[string]agentTokenRecord{},
		groups:                   map[string]ModelGroup{},
		groupMembers:             map[string][]GroupMember{},
		modelSettings:            map[string]ModelSetting{},
		services:                 map[string]Service{},
		serviceDelegates:         map[string]map[string]bool{},
		serviceAllowed:           map[string]map[string]struct{}{},
		serviceAdminGroups:       map[string]map[string]time.Time{},
		resourceGroups:           map[string]ResourceGroup{},
		resourceGroupAdminGroups: map[string]map[string]time.Time{},
		resourceGroupServers:     map[string]map[string]time.Time{},
		resourceGroupProvisions:  map[string]map[ResourceGroupProvision]struct{}{},
		principalLimits:          map[string]map[string]LimitConfig{},
		certificates:             map[string]Certificate{},
		runtimeSpecs:             map[string]RuntimeSpec{},
		runtimeSpecGPUs:          map[string][]RuntimeSpecGPU{},
	}
}

func (m *MemoryStore) CreateAIServer(_ context.Context, host AIServer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[host.ID]; ok {
		return storeerr.ErrConflict
	}
	m.servers[host.ID] = copyAIServer(host)
	return nil
}

func (m *MemoryStore) UpdateAIServer(_ context.Context, host AIServer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[host.ID]; !ok {
		return storeerr.ErrNotFound
	}
	m.servers[host.ID] = copyAIServer(host)
	return nil
}

func (m *MemoryStore) AIServerByID(_ context.Context, id string) (AIServer, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	host, ok := m.servers[id]
	if !ok {
		return AIServer{}, storeerr.ErrNotFound
	}
	return copyAIServer(host), nil
}

func (m *MemoryStore) AIServers(_ context.Context) ([]AIServer, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.servers))
	for id := range m.servers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	hosts := make([]AIServer, 0, len(ids))
	for _, id := range ids {
		hosts = append(hosts, copyAIServer(m.servers[id]))
	}
	return hosts, nil
}

// SetServerHealth updates only the derived health_status (and updated_at) of a
// server, mirroring the SQLite targeted update. An unknown id is ErrNotFound.
func (m *MemoryStore) SetServerHealth(_ context.Context, serverID, health string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	server, ok := m.servers[serverID]
	if !ok {
		return storeerr.ErrNotFound
	}
	server.HealthStatus = health
	server.UpdatedAt = time.Now().UTC()
	m.servers[serverID] = server
	return nil
}

// UpdateServerNetbirdKey mirrors the SQL targeted UPDATE: it sets only the
// enabled flag + setup-key id + tracking-group id of a server. An unknown id is
// ErrNotFound.
func (m *MemoryStore) UpdateServerNetbirdKey(_ context.Context, id string, enabled bool, setupKeyID, groupID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	server, ok := m.servers[id]
	if !ok {
		return storeerr.ErrNotFound
	}
	server.NetbirdEnabled = enabled
	server.NetbirdSetupKeyID = setupKeyID
	server.NetbirdGroupID = groupID
	m.servers[id] = server
	return nil
}

// UpdateServerNetbirdLink mirrors the SQL targeted UPDATE: it sets only the
// enabled flag + peer id and RESETS connected to false. An unknown id is
// ErrNotFound.
func (m *MemoryStore) UpdateServerNetbirdLink(_ context.Context, id string, enabled bool, peerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	server, ok := m.servers[id]
	if !ok {
		return storeerr.ErrNotFound
	}
	server.NetbirdEnabled = enabled
	server.NetbirdPeerID = peerID
	server.NetbirdConnected = false
	m.servers[id] = server
	return nil
}

// UpdateServerNetbirdState mirrors the SQL targeted UPDATE: it sets only the
// domain, peer id, and connected flag. An unknown id is ErrNotFound.
func (m *MemoryStore) UpdateServerNetbirdState(_ context.Context, id, domain, peerID string, connected bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	server, ok := m.servers[id]
	if !ok {
		return storeerr.ErrNotFound
	}
	server.Domain = domain
	server.NetbirdPeerID = peerID
	server.NetbirdConnected = connected
	m.servers[id] = server
	return nil
}

// UpdateServerNetbirdGroups mirrors the SQL targeted UPDATE: it sets only the
// opaque netbird_group_ids JSON mirror. An unknown id is ErrNotFound.
func (m *MemoryStore) UpdateServerNetbirdGroups(_ context.Context, id, groupsJSON string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	server, ok := m.servers[id]
	if !ok {
		return storeerr.ErrNotFound
	}
	server.NetbirdGroupIDs = groupsJSON
	m.servers[id] = server
	return nil
}

// UpdateServerNetbirdPeerManaged mirrors the SQL targeted UPDATE: it sets only the
// netbird_peer_managed provenance flag. An unknown id is ErrNotFound.
func (m *MemoryStore) UpdateServerNetbirdPeerManaged(_ context.Context, id string, managed bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	server, ok := m.servers[id]
	if !ok {
		return storeerr.ErrNotFound
	}
	server.NetbirdPeerManaged = managed
	m.servers[id] = server
	return nil
}

// UpdateServerNetbirdPolicyOverride mirrors the SQL targeted UPDATE: it sets only
// the netbird_policy_override opt-in/opt-out override. An unknown id is ErrNotFound.
func (m *MemoryStore) UpdateServerNetbirdPolicyOverride(_ context.Context, id, override string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	server, ok := m.servers[id]
	if !ok {
		return storeerr.ErrNotFound
	}
	server.NetbirdPolicyOverride = override
	m.servers[id] = server
	return nil
}

// UpdateServerNetbirdAllowPing mirrors the SQL targeted UPDATE: it sets only the
// netbird_allow_ping per-server ping-allow flag. An unknown id is ErrNotFound.
func (m *MemoryStore) UpdateServerNetbirdAllowPing(_ context.Context, id string, allow bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	server, ok := m.servers[id]
	if !ok {
		return storeerr.ErrNotFound
	}
	server.NetbirdAllowPing = allow
	m.servers[id] = server
	return nil
}

// UpdateServerNetbirdPingExclude sets only the netbird_ping_exclude per-server flag.
func (m *MemoryStore) UpdateServerNetbirdPingExclude(_ context.Context, id string, exclude bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	server, ok := m.servers[id]
	if !ok {
		return storeerr.ErrNotFound
	}
	server.NetbirdPingExclude = exclude
	m.servers[id] = server
	return nil
}

// UpdateServerEnergyConfig sets only the five per-server energy-config fields.
func (m *MemoryStore) UpdateServerEnergyConfig(_ context.Context, id string, estimatedWatts, idleWatts, pricePerKwh, pue float64, priceUnit string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	server, ok := m.servers[id]
	if !ok {
		return storeerr.ErrNotFound
	}
	server.EstimatedWatts = estimatedWatts
	server.IdleWatts = idleWatts
	server.PricePerKwh = pricePerKwh
	server.Pue = pue
	server.PriceUnit = priceUnit
	m.servers[id] = server
	return nil
}

func (m *MemoryStore) DeleteAIServer(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[id]; !ok {
		return storeerr.ErrNotFound
	}
	m.deleteApplicationsForServerLocked(id)
	delete(m.servers, id)
	delete(m.owners, id)
	delete(m.agentTokens, id)
	delete(m.serverAdminGroups, id)
	// resourceGroupServers is keyed resourceGroupID -> serverID (the REVERSE
	// direction from serverAdminGroups' serverID -> groupID), so cascading a
	// server delete needs to drop id from EVERY resource group's server set —
	// mirrors the resource_group_servers FK ON DELETE CASCADE on server_id.
	for _, servers := range m.resourceGroupServers {
		delete(servers, id)
	}
	// certificates.server_id carries a real FK ON DELETE CASCADE (migration
	// v57) -- mirror it by dropping any certificate linked to this server.
	for domain, cert := range m.certificates {
		if cert.ServerID == id {
			delete(m.certificates, domain)
		}
	}
	return nil
}

func (m *MemoryStore) SetServerOwners(_ context.Context, serverID string, userIDs []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	set := make(map[string]struct{}, len(userIDs))
	for _, id := range userIDs {
		set[id] = struct{}{}
	}
	m.owners[serverID] = set
	return nil
}

func (m *MemoryStore) ServerOwners(_ context.Context, serverID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.owners[serverID]))
	for id := range m.owners[serverID] {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (m *MemoryStore) ServersByOwner(_ context.Context, userID string) ([]AIServer, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0)
	for serverID, set := range m.owners {
		if _, ok := set[userID]; ok {
			if _, exists := m.servers[serverID]; exists {
				ids = append(ids, serverID)
			}
		}
	}
	sort.Strings(ids)
	out := make([]AIServer, 0, len(ids))
	for _, id := range ids {
		out = append(out, copyAIServer(m.servers[id]))
	}
	return out, nil
}

// UpdateServerSystemGroup mirrors the SQL targeted UPDATE: it sets only
// system_group_id. An unknown id is ErrNotFound.
func (m *MemoryStore) UpdateServerSystemGroup(_ context.Context, serverID, systemGroupID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	server, ok := m.servers[serverID]
	if !ok {
		return storeerr.ErrNotFound
	}
	server.SystemGroupID = systemGroupID
	m.servers[serverID] = server
	return nil
}

// UpdateServerCertificateOverride sets only the certificate_override field.
func (m *MemoryStore) UpdateServerCertificateOverride(_ context.Context, id, override string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	srv, ok := m.servers[id]
	if !ok {
		return storeerr.ErrNotFound
	}
	srv.CertificateOverride = override
	m.servers[id] = srv
	return nil
}

// UpdateServerHTTPSSwitchOverride sets only the https_switch_override field
// (P4's per-server https-auto-switch opt-in/opt-out). Mirrors
// UpdateServerCertificateOverride.
func (m *MemoryStore) UpdateServerHTTPSSwitchOverride(_ context.Context, id, override string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	srv, ok := m.servers[id]
	if !ok {
		return storeerr.ErrNotFound
	}
	srv.HTTPSSwitchOverride = override
	m.servers[id] = srv
	return nil
}

// SetServerAdminGroup mirrors the SQL upsert (on-conflict-do-nothing on the
// (server_id, group_id) pair): a re-link keeps the first-seen time. An
// unknown serverID is ErrNotFound (mirrors the FK violation the SQL store
// surfaces); groupID is an opaque string here — routing.MemoryStore holds no
// user_groups data to validate it against (the SQL store's real FK is the
// authority for that check; the memory driver mirrors the server_owners
// cascade's own reach, which likewise cannot validate a foreign user_id).
func (m *MemoryStore) SetServerAdminGroup(_ context.Context, serverID, groupID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[serverID]; !ok {
		return storeerr.ErrNotFound
	}
	groups, ok := m.serverAdminGroups[serverID]
	if !ok {
		groups = map[string]time.Time{}
		m.serverAdminGroups[serverID] = groups
	}
	if _, had := groups[groupID]; !had {
		groups[groupID] = time.Now().UTC()
	}
	return nil
}

// RemoveServerAdminGroup mirrors the SQL delete: a no-op (non-error) when the
// link does not exist.
func (m *MemoryStore) RemoveServerAdminGroup(_ context.Context, serverID, groupID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if groups, ok := m.serverAdminGroups[serverID]; ok {
		delete(groups, groupID)
	}
	return nil
}

// ServerAdminGroups mirrors the SQL `order by created_at, group_id` listing.
// Always non-nil, empty when none.
func (m *MemoryStore) ServerAdminGroups(_ context.Context, serverID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return sortedByFirstSeen(m.serverAdminGroups[serverID]), nil
}

// ServersByAdminGroups mirrors the SQL join+distinct: every server linked to
// ANY of groupIDs, deduped by server id, sorted by id. An empty groupIDs
// returns an empty slice without scanning the map (mirrors the SQL store's
// no-query short-circuit).
func (m *MemoryStore) ServersByAdminGroups(_ context.Context, groupIDs []string) ([]AIServer, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(groupIDs) == 0 {
		return make([]AIServer, 0), nil
	}
	want := make(map[string]struct{}, len(groupIDs))
	for _, id := range groupIDs {
		want[id] = struct{}{}
	}
	ids := make([]string, 0)
	for serverID, groups := range m.serverAdminGroups {
		if _, exists := m.servers[serverID]; !exists {
			continue
		}
		for groupID := range groups {
			if _, ok := want[groupID]; ok {
				ids = append(ids, serverID)
				break
			}
		}
	}
	sort.Strings(ids)
	out := make([]AIServer, 0, len(ids))
	for _, id := range ids {
		out = append(out, copyAIServer(m.servers[id]))
	}
	return out, nil
}

func (m *MemoryStore) UpsertTelemetry(_ context.Context, telemetry ServerTelemetry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[telemetry.ServerID]; !ok {
		return storeerr.ErrNotFound
	}
	m.telemetry[telemetry.ServerID] = telemetry
	return nil
}

func (m *MemoryStore) TelemetryByServer(_ context.Context, serverID string) (ServerTelemetry, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	telemetry, ok := m.telemetry[serverID]
	return telemetry, ok, nil
}

// UpsertServerHardware stores the latest hardware inventory for its server. An
// unknown ServerID classifies as ErrNotFound (mirrors UpsertTelemetry).
func (m *MemoryStore) UpsertServerHardware(_ context.Context, hw ServerHardware) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[hw.ServerID]; !ok {
		return storeerr.ErrNotFound
	}
	m.hardware[hw.ServerID] = hw
	return nil
}

// ServerHardwareByServer returns the latest hardware inventory for serverID.
func (m *MemoryStore) ServerHardwareByServer(_ context.Context, serverID string) (ServerHardware, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	hw, ok := m.hardware[serverID]
	return hw, ok, nil
}

// InsertTelemetrySample appends one rich telemetry sample for its server. An
// unknown ServerID classifies as ErrNotFound (mirrors UpsertTelemetry). The
// sample's gpu/net slices are deep-copied on store to avoid caller aliasing.
func (m *MemoryStore) InsertTelemetrySample(_ context.Context, sample TelemetrySample) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[sample.ServerID]; !ok {
		return storeerr.ErrNotFound
	}
	m.samples[sample.ServerID] = append(m.samples[sample.ServerID], copyTelemetrySample(sample))
	return nil
}

// TelemetrySamples returns the samples for serverID within [from,to] inclusive,
// ordered ascending by reported_at. When limit > 0 and more rows match, the
// result is decimated to at most limit evenly-spaced points (oldest+newest kept).
// Returned samples are deep copies.
func (m *MemoryStore) TelemetrySamples(_ context.Context, serverID string, from, to time.Time, limit int) ([]TelemetrySample, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	matched := make([]TelemetrySample, 0)
	for _, sample := range m.samples[serverID] {
		if sample.ReportedAt.Before(from) || sample.ReportedAt.After(to) {
			continue
		}
		matched = append(matched, copyTelemetrySample(sample))
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].ReportedAt.Before(matched[j].ReportedAt) })
	return DecimateTelemetrySamples(matched, limit), nil
}

// PruneTelemetrySamples deletes samples older than the cutoff (retention).
func (m *MemoryStore) PruneTelemetrySamples(_ context.Context, before time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for serverID, samples := range m.samples {
		kept := make([]TelemetrySample, 0, len(samples))
		for _, sample := range samples {
			if sample.ReportedAt.Before(before) {
				continue
			}
			kept = append(kept, sample)
		}
		m.samples[serverID] = kept
	}
	return nil
}

// InsertServerAvailabilitySample appends one availability sample for its server.
// An unknown ServerID classifies as ErrNotFound.
func (m *MemoryStore) InsertServerAvailabilitySample(_ context.Context, sample ServerAvailabilitySample) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[sample.ServerID]; !ok {
		return storeerr.ErrNotFound
	}
	m.availSamples[sample.ServerID] = append(m.availSamples[sample.ServerID], sample)
	return nil
}

// ServerAvailabilitySamples returns the samples for serverID within [from,to]
// inclusive, ascending, with same-state runs collapsed (see reduce). limit>0 caps.
func (m *MemoryStore) ServerAvailabilitySamples(_ context.Context, serverID string, from, to time.Time, limit int) ([]ServerAvailabilitySample, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	matched := make([]ServerAvailabilitySample, 0)
	for _, sample := range m.availSamples[serverID] {
		if sample.ReportedAt.Before(from) || sample.ReportedAt.After(to) {
			continue
		}
		matched = append(matched, sample)
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].ReportedAt.Before(matched[j].ReportedAt) })
	return ReduceAvailabilitySamples(matched, AvailabilityGapFloor, limit), nil
}

// PruneServerAvailabilitySamples deletes samples older than the cutoff.
func (m *MemoryStore) PruneServerAvailabilitySamples(_ context.Context, before time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for serverID, samples := range m.availSamples {
		kept := make([]ServerAvailabilitySample, 0, len(samples))
		for _, sample := range samples {
			if sample.ReportedAt.Before(before) {
				continue
			}
			kept = append(kept, sample)
		}
		m.availSamples[serverID] = kept
	}
	return nil
}

// InsertBenchmarkRun appends one benchmark-run history row for its mapping. An
// unknown MappingID classifies as ErrNotFound (mirrors the SQL FK cascade). An
// empty run.ID gets a generated one (mirrors the SQLite store so a memory-backed
// dev deployment surfaces the same non-empty ids).
func (m *MemoryStore) InsertBenchmarkRun(_ context.Context, run BenchmarkRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mappings[run.MappingID]; !ok {
		return storeerr.ErrNotFound
	}
	if run.ID == "" {
		run.ID = newBenchmarkRunID()
	}
	if run.Kind == "" {
		run.Kind = "speed"
	}
	m.benchmarks[run.MappingID] = append(m.benchmarks[run.MappingID], run)
	return nil
}

func newBenchmarkRunID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "bmk_" + hex.EncodeToString(b[:])
}

// BenchmarkRunsByMapping returns the benchmark-run history for mappingID,
// newest-first (created_at desc). A non-positive limit defaults to 50.
func (m *MemoryStore) BenchmarkRunsByMapping(_ context.Context, mappingID string, limit int) ([]BenchmarkRun, error) {
	if limit <= 0 {
		limit = 50
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	runs := append([]BenchmarkRun(nil), m.benchmarks[mappingID]...)
	sort.Slice(runs, func(i, j int) bool { return runs[i].CreatedAt.After(runs[j].CreatedAt) })
	if len(runs) > limit {
		runs = runs[:limit]
	}
	return runs, nil
}

// PruneBenchmarkRuns deletes benchmark-run rows older than the cutoff (retention).
func (m *MemoryStore) PruneBenchmarkRuns(_ context.Context, before time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for mappingID, runs := range m.benchmarks {
		kept := make([]BenchmarkRun, 0, len(runs))
		for _, run := range runs {
			if run.CreatedAt.Before(before) {
				continue
			}
			kept = append(kept, run)
		}
		m.benchmarks[mappingID] = kept
	}
	return nil
}

func (m *MemoryStore) UpsertAffinity(_ context.Context, affinity RouteAffinity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.affinities[affinityKey(affinity)] = affinity
	return nil
}

func (m *MemoryStore) Affinity(_ context.Context, key AffinityKey) (RouteAffinity, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	affinity, ok := m.affinities[key]
	return affinity, ok, nil
}

func (m *MemoryStore) DeleteAffinity(_ context.Context, key AffinityKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.affinities, key)
	return nil
}

func (m *MemoryStore) CreateApplication(_ context.Context, app Application) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.applications[app.ID]; ok {
		return storeerr.ErrConflict
	}
	if _, ok := m.servers[app.ServerID]; !ok {
		return storeerr.ErrNotFound
	}
	if m.applicationPortTakenLocked(app.ServerID, app.Port, "") {
		return storeerr.ErrConflict
	}
	m.applications[app.ID] = copyApplication(app)
	return nil
}

func (m *MemoryStore) UpdateApplication(_ context.Context, app Application) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.applications[app.ID]; !ok {
		return storeerr.ErrNotFound
	}
	if _, ok := m.servers[app.ServerID]; !ok {
		return storeerr.ErrNotFound
	}
	if m.applicationPortTakenLocked(app.ServerID, app.Port, app.ID) {
		return storeerr.ErrConflict
	}
	m.applications[app.ID] = copyApplication(app)
	return nil
}

func (m *MemoryStore) applicationPortTakenLocked(serverID string, port int, excludeID string) bool {
	for id, existing := range m.applications {
		if id == excludeID {
			continue
		}
		if existing.ServerID == serverID && existing.Port == port {
			return true
		}
	}
	return false
}

func (m *MemoryStore) ApplicationByID(_ context.Context, id string) (Application, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	app, ok := m.applications[id]
	if !ok {
		return Application{}, storeerr.ErrNotFound
	}
	return copyApplication(app), nil
}

func (m *MemoryStore) ApplicationsByServer(_ context.Context, serverID string) ([]Application, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0)
	for id, app := range m.applications {
		if app.ServerID == serverID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	apps := make([]Application, 0, len(ids))
	for _, id := range ids {
		apps = append(apps, copyApplication(m.applications[id]))
	}
	return apps, nil
}

func (m *MemoryStore) DeleteApplication(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.applications[id]; !ok {
		return storeerr.ErrNotFound
	}
	m.deleteMappingsForApplicationLocked(id)
	delete(m.applications, id)
	return nil
}

// deleteApplicationsForServerLocked removes every application owned by
// serverID (and their mappings). Callers must hold m.mu for writing.
func (m *MemoryStore) deleteApplicationsForServerLocked(serverID string) {
	for id, app := range m.applications {
		if app.ServerID == serverID {
			m.deleteMappingsForApplicationLocked(id)
			delete(m.applications, id)
		}
	}
}

// deleteMappingsForApplicationLocked removes every mapping owned by
// applicationID. Callers must hold m.mu for writing.
func (m *MemoryStore) deleteMappingsForApplicationLocked(applicationID string) {
	for id, mapping := range m.mappings {
		if mapping.ApplicationID == applicationID {
			delete(m.mappings, id)
		}
	}
}

func (m *MemoryStore) CreateMapping(_ context.Context, mapping ModelMapping) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mappings[mapping.ID]; ok {
		return storeerr.ErrConflict
	}
	if _, ok := m.applications[mapping.ApplicationID]; !ok {
		return storeerr.ErrNotFound
	}
	m.mappings[mapping.ID] = mapping
	return nil
}

func (m *MemoryStore) UpdateMapping(_ context.Context, mapping ModelMapping) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mappings[mapping.ID]; !ok {
		return storeerr.ErrNotFound
	}
	if _, ok := m.applications[mapping.ApplicationID]; !ok {
		return storeerr.ErrNotFound
	}
	m.mappings[mapping.ID] = mapping
	return nil
}

// UpdateMappingContextProbe sets a mapping's context_size + provenance from a
// context probe, only while it is unlocked. A missing or locked mapping is a
// benign no-op (mirrors the SQL metrics_locked = 0 guard).
func (m *MemoryStore) UpdateMappingContextProbe(_ context.Context, id string, contextSize int, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mapping, ok := m.mappings[id]
	if !ok || mapping.MetricsLocked {
		return nil
	}
	mapping.ContextSize = contextSize
	mapping.MetricsSource = "probe"
	t := at
	mapping.MetricsUpdatedAt = &t
	m.mappings[id] = mapping
	return nil
}

// UpdateMappingVisionCapable sets a mapping's vision_capable flag + provenance
// from a vision-capability check, only while it is unlocked. A missing or
// locked mapping is a benign no-op (mirrors the SQL metrics_locked = 0 guard).
// A definitive "not capable" (false) result can also be written.
func (m *MemoryStore) UpdateMappingVisionCapable(_ context.Context, id string, capable bool, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mapping, ok := m.mappings[id]
	if !ok || mapping.MetricsLocked {
		return nil
	}
	mapping.VisionCapable = capable
	mapping.MetricsSource = "vision"
	t := at
	mapping.MetricsUpdatedAt = &t
	m.mappings[id] = mapping
	return nil
}

// UpdateMappingBenchmarkMetrics sets a mapping's measured throughput + load time +
// provenance from a benchmark run, only while it is unlocked. A missing or locked
// mapping is a benign no-op (mirrors the SQL metrics_locked = 0 guard).
func (m *MemoryStore) UpdateMappingBenchmarkMetrics(_ context.Context, id string, genTPS, promptTPS float64, loadMS int, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mapping, ok := m.mappings[id]
	if !ok || mapping.MetricsLocked {
		return nil
	}
	mapping.GenTokensPerSecond = genTPS
	mapping.PromptTokensPerSecond = promptTPS
	mapping.LoadTimeMS = loadMS
	mapping.MetricsSource = "benchmark"
	t := at
	mapping.MetricsUpdatedAt = &t
	m.mappings[id] = mapping
	return nil
}

// ewma blends a live sample into an existing value: a non-positive sample leaves
// the value unchanged, a stored non-positive value is seeded directly by the first
// positive sample, and otherwise the result is alpha*sample + (1-alpha)*old.
// Mirrors the 3-branch SQL CASE in SQLiteStore.UpdateMappingOpportunisticMetrics.
func ewma(old, sample, alpha float64) float64 {
	switch {
	case sample <= 0:
		return old
	case old <= 0:
		return sample
	default:
		return alpha*sample + (1-alpha)*old
	}
}

// UpdateMappingOpportunisticMetrics EWMA-updates gen/prompt throughput from a live
// usage sample, only while the mapping is unlocked. A missing or locked mapping is a
// benign no-op (mirrors the SQL metrics_locked = 0 guard).
func (m *MemoryStore) UpdateMappingOpportunisticMetrics(_ context.Context, id string, genSample, promptSample, alpha float64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mapping, ok := m.mappings[id]
	if !ok || mapping.MetricsLocked {
		return nil
	}
	mapping.GenTokensPerSecond = ewma(mapping.GenTokensPerSecond, genSample, alpha)
	mapping.PromptTokensPerSecond = ewma(mapping.PromptTokensPerSecond, promptSample, alpha)
	mapping.MetricsSource = "opportunistic"
	t := at
	mapping.MetricsUpdatedAt = &t
	m.mappings[id] = mapping
	return nil
}

// UpdateMappingEnergyEWMA EWMA-blends a mapping's energy_wh_per_token coefficient
// from a live per-request energy sample, only while the mapping is unlocked. A
// missing or locked mapping is a benign no-op (mirrors the SQL
// metrics_locked = 0 guard).
func (m *MemoryStore) UpdateMappingEnergyEWMA(_ context.Context, id string, sampleWhPerToken, alpha float64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mapping, ok := m.mappings[id]
	if !ok || mapping.MetricsLocked {
		return nil
	}
	mapping.EnergyWhPerToken = ewma(mapping.EnergyWhPerToken, sampleWhPerToken, alpha)
	mapping.MetricsSource = "energy"
	t := at
	mapping.MetricsUpdatedAt = &t
	m.mappings[id] = mapping
	return nil
}

// UpdateMappingCapacityMetrics sets a mapping's measured concurrency capacity +
// provenance from a capacity benchmark, only while it is unlocked. A missing or
// locked mapping is a benign no-op (mirrors the SQL metrics_locked = 0 guard).
func (m *MemoryStore) UpdateMappingCapacityMetrics(_ context.Context, id string, maxConcurrency, recommendedConcurrency int, genTPSAtCapacity float64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mapping, ok := m.mappings[id]
	if !ok || mapping.MetricsLocked {
		return nil
	}
	mapping.MaxConcurrency = maxConcurrency
	mapping.RecommendedConcurrency = recommendedConcurrency
	mapping.GenTokensPerSecondAtCapacity = genTPSAtCapacity
	mapping.MetricsSource = "capacity"
	t := at
	mapping.MetricsUpdatedAt = &t
	m.mappings[id] = mapping
	return nil
}

func (m *MemoryStore) MappingByID(_ context.Context, id string) (ModelMapping, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mapping, ok := m.mappings[id]
	if !ok {
		return ModelMapping{}, storeerr.ErrNotFound
	}
	return mapping, nil
}

func (m *MemoryStore) MappingsByApplication(_ context.Context, applicationID string) ([]ModelMapping, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0)
	for id, mapping := range m.mappings {
		if mapping.ApplicationID == applicationID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	mappings := make([]ModelMapping, 0, len(ids))
	for _, id := range ids {
		mappings = append(mappings, m.mappings[id])
	}
	return mappings, nil
}

func (m *MemoryStore) MappingsByServer(_ context.Context, serverID string) ([]ModelMapping, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	appIDs := make(map[string]struct{})
	for id, app := range m.applications {
		if app.ServerID == serverID {
			appIDs[id] = struct{}{}
		}
	}
	ids := make([]string, 0)
	for id, mapping := range m.mappings {
		if _, ok := appIDs[mapping.ApplicationID]; ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	mappings := make([]ModelMapping, 0, len(ids))
	for _, id := range ids {
		mappings = append(mappings, m.mappings[id])
	}
	return mappings, nil
}

func (m *MemoryStore) ActiveMappingsForModel(_ context.Context, gatewayModel string, apiFlavor string) ([]MappingCandidate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.mappings))
	for id := range m.mappings {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]MappingCandidate, 0, len(ids))
	for _, id := range ids {
		mapping := m.mappings[id]
		if mapping.Status != ServerStatusActive || mapping.GatewayModelName != gatewayModel {
			continue
		}
		app, ok := m.applications[mapping.ApplicationID]
		if !ok || app.Status != ServerStatusActive || !applicationHasAPIFlavor(app, apiFlavor) {
			continue
		}
		server, ok := m.servers[app.ServerID]
		if !ok {
			continue
		}
		out = append(out, MappingCandidate{
			Server:      copyAIServer(server),
			Application: copyApplication(app),
			Mapping:     mapping,
		})
	}
	return out, nil
}

func (m *MemoryStore) DeleteMapping(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mappings[id]; !ok {
		return storeerr.ErrNotFound
	}
	delete(m.mappings, id)
	return nil
}

// --- Model groups (migration v22) -----------------------------------------

func (m *MemoryStore) CreateModelGroup(_ context.Context, group ModelGroup) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.groups[group.ID]; ok {
		return storeerr.ErrConflict
	}
	if group.FailoverMode == "" {
		group.FailoverMode = "sticky"
	}
	if group.Traversal == "" {
		group.Traversal = "round_robin"
	}
	if group.MemberOrder == "" {
		group.MemberOrder = MemberOrderPriority
	}
	if group.MinSpeedFallback == "" {
		group.MinSpeedFallback = MinSpeedFallbackError
	}
	m.groups[group.ID] = group
	return nil
}

func (m *MemoryStore) UpdateModelGroup(_ context.Context, group ModelGroup) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.groups[group.ID]
	if !ok {
		return storeerr.ErrNotFound
	}
	if group.FailoverMode == "" {
		group.FailoverMode = "sticky"
	}
	if group.Traversal == "" {
		group.Traversal = "round_robin"
	}
	if group.MemberOrder == "" {
		group.MemberOrder = MemberOrderPriority
	}
	if group.MinSpeedFallback == "" {
		group.MinSpeedFallback = MinSpeedFallbackError
	}
	// Mirror the SQL UPDATE which does not touch created_at.
	group.CreatedAt = existing.CreatedAt
	m.groups[group.ID] = group
	return nil
}

func (m *MemoryStore) ModelGroupByID(_ context.Context, id string) (ModelGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	group, ok := m.groups[id]
	if !ok {
		return ModelGroup{}, storeerr.ErrNotFound
	}
	return group, nil
}

func (m *MemoryStore) ModelGroups(_ context.Context) ([]ModelGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ModelGroup, 0, len(m.groups))
	for _, group := range m.groups {
		out = append(out, group)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GatewayModelName != out[j].GatewayModelName {
			return out[i].GatewayModelName < out[j].GatewayModelName
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *MemoryStore) DeleteModelGroup(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.groups[id]; !ok {
		return storeerr.ErrNotFound
	}
	delete(m.groups, id)
	delete(m.groupMembers, id) // cascade (FK on delete cascade)
	return nil
}

// SetGroupMembers atomically REPLACES the members of a group. An unknown group id
// is ErrNotFound (even for an empty set); a duplicate MemberGatewayName within the
// set is ErrConflict (mirrors the unique(group_id, member_gateway_name) constraint).
func (m *MemoryStore) SetGroupMembers(_ context.Context, groupID string, members []GroupMember) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.groups[groupID]; !ok {
		return storeerr.ErrNotFound
	}
	seen := make(map[string]struct{}, len(members))
	stored := make([]GroupMember, 0, len(members))
	for _, member := range members {
		if _, dup := seen[member.MemberGatewayName]; dup {
			return storeerr.ErrConflict
		}
		seen[member.MemberGatewayName] = struct{}{}
		member.GroupID = groupID
		if member.ID == "" {
			member.ID = newGroupMemberID()
		}
		if member.CreatedAt.IsZero() {
			member.CreatedAt = time.Now().UTC()
		}
		stored = append(stored, member)
	}
	m.groupMembers[groupID] = stored
	return nil
}

func (m *MemoryStore) GroupMembersByGroup(_ context.Context, groupID string) ([]GroupMember, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	members := copyGroupMembers(m.groupMembers[groupID])
	sort.Slice(members, func(i, j int) bool {
		if members[i].Priority != members[j].Priority {
			return members[i].Priority < members[j].Priority
		}
		return members[i].ID < members[j].ID
	})
	return members, nil
}

func (m *MemoryStore) ModelSettings(_ context.Context) ([]ModelSetting, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ModelSetting, 0, len(m.modelSettings))
	for _, setting := range m.modelSettings {
		out = append(out, setting)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GatewayModelName < out[j].GatewayModelName })
	return out, nil
}

func (m *MemoryStore) ModelSettingByName(_ context.Context, name string) (ModelSetting, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	setting, ok := m.modelSettings[name]
	if !ok {
		return ModelSetting{}, false, nil
	}
	return setting, true, nil
}

func (m *MemoryStore) UpsertModelSetting(_ context.Context, setting ModelSetting) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if setting.Visibility == "" {
		setting.Visibility = "shown"
	}
	// Mirror the SQL on-conflict upsert which preserves created_at.
	if existing, ok := m.modelSettings[setting.GatewayModelName]; ok {
		setting.CreatedAt = existing.CreatedAt
	}
	m.modelSettings[setting.GatewayModelName] = setting
	return nil
}

// --- Services (Phase 1 service accounts, migration v40) --------------------

func (m *MemoryStore) CreateService(_ context.Context, svc Service) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.services[svc.ID]; ok {
		return storeerr.ErrConflict
	}
	if svc.Status == "" {
		svc.Status = ServerStatusActive
	}
	m.services[svc.ID] = svc
	return nil
}

func (m *MemoryStore) UpdateService(_ context.Context, svc Service) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.services[svc.ID]
	if !ok {
		return storeerr.ErrNotFound
	}
	if svc.Status == "" {
		svc.Status = ServerStatusActive
	}
	// Mirror the SQL UPDATE, which never touches created_at.
	svc.CreatedAt = existing.CreatedAt
	m.services[svc.ID] = svc
	return nil
}

func (m *MemoryStore) ServiceByID(_ context.Context, id string) (Service, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	svc, ok := m.services[id]
	if !ok {
		return Service{}, storeerr.ErrNotFound
	}
	return svc, nil
}

func (m *MemoryStore) Services(_ context.Context) ([]Service, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.services))
	for id := range m.services {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Service, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.services[id])
	}
	return out, nil
}

func (m *MemoryStore) ServicesByDelegate(_ context.Context, userID string) ([]Service, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0)
	for serviceID, delegates := range m.serviceDelegates {
		if _, ok := delegates[userID]; !ok {
			continue
		}
		if _, exists := m.services[serviceID]; exists {
			ids = append(ids, serviceID)
		}
	}
	sort.Strings(ids)
	out := make([]Service, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.services[id])
	}
	return out, nil
}

func (m *MemoryStore) DeleteService(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.services[id]; !ok {
		return storeerr.ErrNotFound
	}
	delete(m.services, id)
	delete(m.serviceDelegates, id)   // cascade (FK on delete cascade)
	delete(m.serviceAllowed, id)     // cascade (FK on delete cascade)
	delete(m.serviceAdminGroups, id) // cascade (FK on delete cascade)
	return nil
}

// SetServiceDelegates atomically REPLACES a service's delegate list. An
// unknown service id is ErrNotFound (even for an empty set, mirroring
// SetGroupMembers); a duplicate UserID within the set is ErrConflict (mirrors
// the service_delegates primary key).
func (m *MemoryStore) SetServiceDelegates(_ context.Context, serviceID string, delegates []ServiceDelegate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.services[serviceID]; !ok {
		return storeerr.ErrNotFound
	}
	set := make(map[string]bool, len(delegates))
	for _, d := range delegates {
		if _, dup := set[d.UserID]; dup {
			return storeerr.ErrConflict
		}
		set[d.UserID] = d.CanManageSettings
	}
	m.serviceDelegates[serviceID] = set
	return nil
}

func (m *MemoryStore) ServiceDelegates(_ context.Context, serviceID string) ([]ServiceDelegate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.serviceDelegates[serviceID]))
	for userID := range m.serviceDelegates[serviceID] {
		ids = append(ids, userID)
	}
	sort.Strings(ids)
	out := make([]ServiceDelegate, 0, len(ids))
	for _, userID := range ids {
		out = append(out, ServiceDelegate{UserID: userID, CanManageSettings: m.serviceDelegates[serviceID][userID]})
	}
	return out, nil
}

// SetServiceAllowedModels atomically REPLACES a service's model allowlist. An
// unknown service id is ErrNotFound (even for an empty set); a duplicate model
// name within the set is ErrConflict (mirrors the composite primary key).
func (m *MemoryStore) SetServiceAllowedModels(_ context.Context, serviceID string, models []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.services[serviceID]; !ok {
		return storeerr.ErrNotFound
	}
	set := make(map[string]struct{}, len(models))
	for _, name := range models {
		if _, dup := set[name]; dup {
			return storeerr.ErrConflict
		}
		set[name] = struct{}{}
	}
	m.serviceAllowed[serviceID] = set
	return nil
}

func (m *MemoryStore) ServiceAllowedModels(_ context.Context, serviceID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.serviceAllowed[serviceID]))
	for name := range m.serviceAllowed[serviceID] {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// UpdateServiceSystemGroup mirrors the SQL targeted UPDATE: it sets only
// system_group_id. An unknown id is ErrNotFound.
func (m *MemoryStore) UpdateServiceSystemGroup(_ context.Context, serviceID, systemGroupID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	svc, ok := m.services[serviceID]
	if !ok {
		return storeerr.ErrNotFound
	}
	svc.SystemGroupID = systemGroupID
	m.services[serviceID] = svc
	return nil
}

// SetServiceAdminGroup mirrors the SQL upsert (on-conflict-do-nothing on the
// (service_id, group_id) pair): a re-link keeps the first-seen time. An
// unknown serviceID is ErrNotFound (mirrors the FK violation the SQL store
// surfaces); groupID is an opaque string here — routing.MemoryStore holds no
// user_groups data to validate it against — mirrors SetServerAdminGroup.
func (m *MemoryStore) SetServiceAdminGroup(_ context.Context, serviceID, groupID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.services[serviceID]; !ok {
		return storeerr.ErrNotFound
	}
	groups, ok := m.serviceAdminGroups[serviceID]
	if !ok {
		groups = map[string]time.Time{}
		m.serviceAdminGroups[serviceID] = groups
	}
	if _, had := groups[groupID]; !had {
		groups[groupID] = time.Now().UTC()
	}
	return nil
}

// RemoveServiceAdminGroup mirrors the SQL delete: a no-op (non-error) when
// the link does not exist.
func (m *MemoryStore) RemoveServiceAdminGroup(_ context.Context, serviceID, groupID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if groups, ok := m.serviceAdminGroups[serviceID]; ok {
		delete(groups, groupID)
	}
	return nil
}

// ServiceAdminGroups mirrors the SQL `order by created_at, group_id` listing.
// Always non-nil, empty when none.
func (m *MemoryStore) ServiceAdminGroups(_ context.Context, serviceID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return sortedByFirstSeen(m.serviceAdminGroups[serviceID]), nil
}

// ServicesByAdminGroups mirrors the SQL join+distinct: every service linked
// to ANY of groupIDs, deduped by service id, sorted by id. An empty groupIDs
// returns an empty slice without scanning the map (mirrors the SQL store's
// no-query short-circuit).
func (m *MemoryStore) ServicesByAdminGroups(_ context.Context, groupIDs []string) ([]Service, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(groupIDs) == 0 {
		return make([]Service, 0), nil
	}
	want := make(map[string]struct{}, len(groupIDs))
	for _, id := range groupIDs {
		want[id] = struct{}{}
	}
	ids := make([]string, 0)
	for serviceID, groups := range m.serviceAdminGroups {
		if _, exists := m.services[serviceID]; !exists {
			continue
		}
		for groupID := range groups {
			if _, ok := want[groupID]; ok {
				ids = append(ids, serviceID)
				break
			}
		}
	}
	sort.Strings(ids)
	out := make([]Service, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.services[id])
	}
	return out, nil
}

// --- Resource Groups (Phase 1 management structure, migration v54) --------

func (m *MemoryStore) CreateResourceGroup(_ context.Context, rg ResourceGroup) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.resourceGroups[rg.ID]; ok {
		return storeerr.ErrConflict
	}
	if rg.Status == "" {
		rg.Status = ServerStatusActive
	}
	m.resourceGroups[rg.ID] = rg
	return nil
}

// UpdateResourceGroup mirrors the SQL UPDATE, which never touches created_at
// or system_group_id (system_group_id is written solely via
// UpdateResourceGroupSystemGroup).
func (m *MemoryStore) UpdateResourceGroup(_ context.Context, rg ResourceGroup) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.resourceGroups[rg.ID]
	if !ok {
		return storeerr.ErrNotFound
	}
	if rg.Status == "" {
		rg.Status = ServerStatusActive
	}
	rg.CreatedAt = existing.CreatedAt
	rg.SystemGroupID = existing.SystemGroupID
	m.resourceGroups[rg.ID] = rg
	return nil
}

func (m *MemoryStore) DeleteResourceGroup(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.resourceGroups[id]; !ok {
		return storeerr.ErrNotFound
	}
	delete(m.resourceGroups, id)
	delete(m.resourceGroupAdminGroups, id) // cascade (FK on delete cascade)
	delete(m.resourceGroupServers, id)     // cascade (FK on delete cascade)
	delete(m.resourceGroupProvisions, id)  // cascade (FK on delete cascade)
	return nil
}

func (m *MemoryStore) ResourceGroupByID(_ context.Context, id string) (ResourceGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rg, ok := m.resourceGroups[id]
	if !ok {
		return ResourceGroup{}, storeerr.ErrNotFound
	}
	return rg, nil
}

func (m *MemoryStore) ResourceGroups(_ context.Context) ([]ResourceGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.resourceGroups))
	for id := range m.resourceGroups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ResourceGroup, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.resourceGroups[id])
	}
	return out, nil
}

// UpdateResourceGroupSystemGroup mirrors the SQL targeted UPDATE: it sets
// only system_group_id. An unknown id is ErrNotFound.
func (m *MemoryStore) UpdateResourceGroupSystemGroup(_ context.Context, rgID, systemGroupID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rg, ok := m.resourceGroups[rgID]
	if !ok {
		return storeerr.ErrNotFound
	}
	rg.SystemGroupID = systemGroupID
	m.resourceGroups[rgID] = rg
	return nil
}

// SetResourceGroupAdminGroup mirrors the SQL upsert (on-conflict-do-nothing
// on the (resource_group_id, group_id) pair): a re-link keeps the first-seen
// time. An unknown rgID is ErrNotFound (mirrors the FK violation the SQL
// store surfaces); groupID is an opaque string here — routing.MemoryStore
// holds no user_groups data to validate it against — mirrors
// SetServiceAdminGroup.
func (m *MemoryStore) SetResourceGroupAdminGroup(_ context.Context, rgID, groupID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.resourceGroups[rgID]; !ok {
		return storeerr.ErrNotFound
	}
	groups, ok := m.resourceGroupAdminGroups[rgID]
	if !ok {
		groups = map[string]time.Time{}
		m.resourceGroupAdminGroups[rgID] = groups
	}
	if _, had := groups[groupID]; !had {
		groups[groupID] = time.Now().UTC()
	}
	return nil
}

// RemoveResourceGroupAdminGroup mirrors the SQL delete: a no-op (non-error)
// when the link does not exist.
func (m *MemoryStore) RemoveResourceGroupAdminGroup(_ context.Context, rgID, groupID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if groups, ok := m.resourceGroupAdminGroups[rgID]; ok {
		delete(groups, groupID)
	}
	return nil
}

// ResourceGroupAdminGroups mirrors the SQL `order by created_at, group_id`
// listing. Always non-nil, empty when none.
func (m *MemoryStore) ResourceGroupAdminGroups(_ context.Context, rgID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return sortedByFirstSeen(m.resourceGroupAdminGroups[rgID]), nil
}

// ResourceGroupsByAdminGroups mirrors the SQL join+distinct: every resource
// group linked to ANY of groupIDs, deduped by resource-group id, sorted by
// id. An empty groupIDs returns an empty slice without scanning the map
// (mirrors the SQL store's no-query short-circuit).
func (m *MemoryStore) ResourceGroupsByAdminGroups(_ context.Context, groupIDs []string) ([]ResourceGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(groupIDs) == 0 {
		return make([]ResourceGroup, 0), nil
	}
	want := make(map[string]struct{}, len(groupIDs))
	for _, id := range groupIDs {
		want[id] = struct{}{}
	}
	ids := make([]string, 0)
	for rgID, groups := range m.resourceGroupAdminGroups {
		if _, exists := m.resourceGroups[rgID]; !exists {
			continue
		}
		for groupID := range groups {
			if _, ok := want[groupID]; ok {
				ids = append(ids, rgID)
				break
			}
		}
	}
	sort.Strings(ids)
	out := make([]ResourceGroup, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.resourceGroups[id])
	}
	return out, nil
}

// SetResourceGroupServer mirrors the SQL upsert (on-conflict-do-nothing on
// the (resource_group_id, server_id) pair): a re-link keeps the first-seen
// time. An unknown rgID or serverID is ErrNotFound (mirrors the FK violation
// the SQL store surfaces on either column).
func (m *MemoryStore) SetResourceGroupServer(_ context.Context, rgID, serverID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.resourceGroups[rgID]; !ok {
		return storeerr.ErrNotFound
	}
	if _, ok := m.servers[serverID]; !ok {
		return storeerr.ErrNotFound
	}
	servers, ok := m.resourceGroupServers[rgID]
	if !ok {
		servers = map[string]time.Time{}
		m.resourceGroupServers[rgID] = servers
	}
	if _, had := servers[serverID]; !had {
		servers[serverID] = time.Now().UTC()
	}
	return nil
}

// RemoveResourceGroupServer mirrors the SQL delete: a no-op (non-error) when
// the link does not exist.
func (m *MemoryStore) RemoveResourceGroupServer(_ context.Context, rgID, serverID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if servers, ok := m.resourceGroupServers[rgID]; ok {
		delete(servers, serverID)
	}
	return nil
}

// ResourceGroupServers mirrors the SQL `order by created_at, server_id`
// listing. Always non-nil, empty when none.
func (m *MemoryStore) ResourceGroupServers(_ context.Context, rgID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return sortedByFirstSeen(m.resourceGroupServers[rgID]), nil
}

// ResourceGroupsByServer mirrors the SQL join: every resource group serverID
// is a member of, deduped by resource-group id, sorted by id.
func (m *MemoryStore) ResourceGroupsByServer(_ context.Context, serverID string) ([]ResourceGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0)
	for rgID, servers := range m.resourceGroupServers {
		if _, exists := m.resourceGroups[rgID]; !exists {
			continue
		}
		if _, ok := servers[serverID]; ok {
			ids = append(ids, rgID)
		}
	}
	sort.Strings(ids)
	out := make([]ResourceGroup, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.resourceGroups[id])
	}
	return out, nil
}

// SetResourceGroupProvision mirrors the SQL upsert (on-conflict-do-nothing on
// the (resource_group_id, target_kind, target_id) unique triple): idempotent,
// non-error on a repeat. A missing rgID is ErrNotFound (mirrors the FK
// violation) — targetID carries no FK (polymorphic).
func (m *MemoryStore) SetResourceGroupProvision(_ context.Context, rgID, kind, targetID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.resourceGroups[rgID]; !ok {
		return storeerr.ErrNotFound
	}
	provs, ok := m.resourceGroupProvisions[rgID]
	if !ok {
		provs = map[ResourceGroupProvision]struct{}{}
		m.resourceGroupProvisions[rgID] = provs
	}
	provs[ResourceGroupProvision{Kind: kind, TargetID: targetID}] = struct{}{}
	return nil
}

// RemoveResourceGroupProvision mirrors the SQL delete: a no-op (non-error)
// when the link does not exist.
func (m *MemoryStore) RemoveResourceGroupProvision(_ context.Context, rgID, kind, targetID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if provs, ok := m.resourceGroupProvisions[rgID]; ok {
		delete(provs, ResourceGroupProvision{Kind: kind, TargetID: targetID})
	}
	return nil
}

// SetResourceGroupProvisions atomically REPLACES the whole provisioned-for
// set of rgID (mirrors the SQL delete-then-insert transaction in
// SetResourceGroupProvisions). The resource group must exist (an empty
// provisions on an unknown rgID is still ErrNotFound). A duplicate (kind,
// targetID) pair within provisions collapses naturally (set semantics).
func (m *MemoryStore) SetResourceGroupProvisions(_ context.Context, rgID string, provisions []ResourceGroupProvision) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.resourceGroups[rgID]; !ok {
		return storeerr.ErrNotFound
	}
	set := make(map[ResourceGroupProvision]struct{}, len(provisions))
	for _, p := range provisions {
		set[ResourceGroupProvision{Kind: p.Kind, TargetID: p.TargetID}] = struct{}{}
	}
	m.resourceGroupProvisions[rgID] = set
	return nil
}

// ResourceGroupProvisions mirrors the SQL `order by target_kind, target_id`
// listing. Always non-nil, empty when none.
func (m *MemoryStore) ResourceGroupProvisions(_ context.Context, rgID string) ([]ResourceGroupProvision, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	provs := m.resourceGroupProvisions[rgID]
	out := make([]ResourceGroupProvision, 0, len(provs))
	for p := range provs {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].TargetID < out[j].TargetID
	})
	return out, nil
}

// ResourceGroupIDsByProvisionTargets mirrors the SQL
// `select distinct resource_group_id ... where target_kind=? and target_id
// in (...)`. An empty targetIDs returns (nil, nil) without scanning — mirrors
// the SQL store's no-query short-circuit.
func (m *MemoryStore) ResourceGroupIDsByProvisionTargets(_ context.Context, kind string, targetIDs []string) ([]string, error) {
	if len(targetIDs) == 0 {
		return nil, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	wanted := make(map[string]struct{}, len(targetIDs))
	for _, id := range targetIDs {
		wanted[id] = struct{}{}
	}
	ids := make([]string, 0)
	for rgID, provs := range m.resourceGroupProvisions {
		if _, exists := m.resourceGroups[rgID]; !exists {
			continue
		}
		for p := range provs {
			if p.Kind != kind {
				continue
			}
			if _, ok := wanted[p.TargetID]; ok {
				ids = append(ids, rgID)
				break
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// ProvisionedResourceGroupIDs returns the set of every resource group id that
// carries at least one provision (of any kind).
func (m *MemoryStore) ProvisionedResourceGroupIDs(_ context.Context) (map[string]bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]bool{}
	for rgID, provs := range m.resourceGroupProvisions {
		if len(provs) == 0 {
			continue
		}
		if _, exists := m.resourceGroups[rgID]; !exists {
			continue
		}
		out[rgID] = true
	}
	return out, nil
}

// --- Principal rate/quota/budget limits (migration v41, Phase 2) -----------

func (m *MemoryStore) PrincipalLimits(_ context.Context, principalType, principalID string) (LimitConfig, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	byID, ok := m.principalLimits[principalType]
	if !ok {
		return LimitConfig{}, false, nil
	}
	cfg, ok := byID[principalID]
	if !ok {
		return LimitConfig{}, false, nil
	}
	return cfg, true, nil
}

func (m *MemoryStore) SetPrincipalLimits(_ context.Context, principalType, principalID string, cfg LimitConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	byID, ok := m.principalLimits[principalType]
	if !ok {
		byID = map[string]LimitConfig{}
		m.principalLimits[principalType] = byID
	}
	byID[principalID] = cfg
	return nil
}

func (m *MemoryStore) DeletePrincipalLimits(_ context.Context, principalType, principalID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if byID, ok := m.principalLimits[principalType]; ok {
		delete(byID, principalID)
	}
	return nil
}

// UsageAggregateSince always returns a zero aggregate: the memory store holds
// no usage_events (usage in memory/dev mode is tracked by the separate,
// non-persistent usage.Recorder — see routing.Store.UsageAggregateSince's
// doc). This is an honest no-op, not a stub bug: quota/budget enforcement is a
// persistent-store (sqlite/postgres) feature per the design spec, and the
// memory driver's usage never survives a restart anyway.
func (m *MemoryStore) UsageAggregateSince(_ context.Context, principalType, _ string, _ time.Time) (int64, int64, float64, error) {
	if principalType != PrincipalTypeService && principalType != PrincipalTypeUser {
		return 0, 0, 0, fmt.Errorf("usage aggregate: invalid principal type %q", principalType)
	}
	return 0, 0, 0, nil
}

func newGroupMemberID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "gmem_" + hex.EncodeToString(b[:])
}

func copyGroupMembers(members []GroupMember) []GroupMember {
	return append([]GroupMember(nil), members...)
}

func (m *MemoryStore) UpsertAgentToken(_ context.Context, token AgentToken, secretHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[token.ServerID]; !ok {
		return storeerr.ErrNotFound
	}
	for sid, rec := range m.agentTokens {
		if sid != token.ServerID && rec.secretHash == secretHash {
			return storeerr.ErrConflict
		}
	}
	if existing, ok := m.agentTokens[token.ServerID]; ok {
		token.CreatedAt = existing.token.CreatedAt // preserve created_at on rotate
	}
	token.LastUsedAt = nil // reset on create/rotate
	m.agentTokens[token.ServerID] = agentTokenRecord{token: copyAgentToken(token), secretHash: secretHash}
	return nil
}

func (m *MemoryStore) AgentTokenByServer(_ context.Context, serverID string) (AgentToken, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.agentTokens[serverID]
	if !ok {
		return AgentToken{}, false, nil
	}
	return copyAgentToken(rec.token), true, nil
}

func (m *MemoryStore) DeleteAgentTokenByServer(_ context.Context, serverID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.agentTokens, serverID)
	return nil
}

func (m *MemoryStore) LookupAgentToken(_ context.Context, secretHash string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for serverID, rec := range m.agentTokens {
		if rec.secretHash == secretHash {
			now := time.Now().UTC()
			rec.token.LastUsedAt = &now
			m.agentTokens[serverID] = rec
			return serverID, true, nil
		}
	}
	return "", false, nil
}

func affinityKey(affinity RouteAffinity) AffinityKey {
	return AffinityKey{
		APITokenID: affinity.APITokenID,
		Model:      affinity.Model,
		APIFlavor:  affinity.APIFlavor,
		SessionID:  affinity.SessionID,
	}
}

// sortedByFirstSeen returns byID's keys ordered by their recorded time (ties
// broken by id), mirroring the SQL `order by created_at, <id>` convention used
// by ServerAdminGroups/ProjectGroups. Ranging over a nil map (an unknown
// serverID) is a documented no-op, so this returns an empty slice rather than
// erroring.
func sortedByFirstSeen(byID map[string]time.Time) []string {
	out := make([]string, 0, len(byID))
	for id := range byID {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := byID[out[i]], byID[out[j]]
		if ti.Equal(tj) {
			return out[i] < out[j]
		}
		return ti.Before(tj)
	})
	return out
}

func copyAIServer(host AIServer) AIServer {
	host.LastSeenAt = copyTimePtr(host.LastSeenAt)
	return host
}

func copyAgentToken(token AgentToken) AgentToken {
	token.LastUsedAt = copyTimePtr(token.LastUsedAt)
	return token
}

func copyTelemetrySample(sample TelemetrySample) TelemetrySample {
	sample.CPUCores = append([]float64(nil), sample.CPUCores...)
	sample.GPUs = append([]GPUSample(nil), sample.GPUs...)
	sample.Net = append([]NetSample(nil), sample.Net...)
	return sample
}

func copyApplication(app Application) Application {
	app.APIFlavors = append([]string(nil), app.APIFlavors...)
	return app
}

func copyTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

// UpsertCertificate inserts or replaces the row for cert.Domain, preserving the
// stored CreatedAt on update (mirrors the SQL on-conflict clause). A non-empty
// ServerID naming no known server mirrors the SQL store's foreign-key
// violation (review finding F1.5): without this check, the memory store
// accepted a certificate row for a server that does not exist, which the real
// SQLStore.UpsertCertificate has always rejected -- exactly the class of bug
// that passes every memory-backed test and only fails against a real
// database (see the memory-mode-e2e-misses-fk-and-usage-store-bugs pattern
// elsewhere in this repo).
func (m *MemoryStore) UpsertCertificate(_ context.Context, cert Certificate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cert.ServerID != "" {
		if _, ok := m.servers[cert.ServerID]; !ok {
			return storeerr.ErrNotFound
		}
	}
	if prev, ok := m.certificates[cert.Domain]; ok {
		cert.CreatedAt = prev.CreatedAt
	}
	m.certificates[cert.Domain] = cert
	return nil
}

// CertificateByDomain returns storeerr.ErrNotFound when no row exists for
// domain.
func (m *MemoryStore) CertificateByDomain(_ context.Context, domain string) (Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cert, ok := m.certificates[domain]
	if !ok {
		return Certificate{}, storeerr.ErrNotFound
	}
	return cert, nil
}

// CertificateByServer returns storeerr.ErrNotFound when serverID is empty or
// no row is linked to it. With more than one row linked to the same server
// (there is no unique constraint on server_id) the lowest domain wins, always
// (review finding F1.4) — a map iteration order is otherwise random, so
// without this the pick would differ from call to call and mirror the SQL
// store's own pre-fix nondeterminism, not fix it.
func (m *MemoryStore) CertificateByServer(_ context.Context, serverID string) (Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if serverID == "" {
		return Certificate{}, storeerr.ErrNotFound
	}
	best := Certificate{}
	found := false
	for _, cert := range m.certificates {
		if cert.ServerID != serverID {
			continue
		}
		if !found || cert.Domain < best.Domain {
			best = cert
			found = true
		}
	}
	if !found {
		return Certificate{}, storeerr.ErrNotFound
	}
	return best, nil
}

// Certificates mirrors the SQL `order by domain`. Always non-nil.
func (m *MemoryStore) Certificates(_ context.Context) ([]Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Certificate, 0, len(m.certificates))
	for _, cert := range m.certificates {
		out = append(out, cert)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out, nil
}

// DeleteCertificate removes the row for domain, if any (a missing row is a
// benign no-op, mirroring the store's other idempotent-on-retry delete
// methods).
func (m *MemoryStore) DeleteCertificate(_ context.Context, domain string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.certificates, domain)
	return nil
}

// --- RuntimeStore (agent-runtime-manager, migration 65, T1) -----------------

// UpsertRuntimeSpec inserts or replaces the launch spec for spec.MappingID.
// MappingID must reference an existing mapping — a hand-rolled FK existence
// check against m.mappings, mirroring CreateApplication/CreateMapping's
// server_id/application_id checks above. Mirrors the SQL upsert's `on
// conflict(mapping_id) do update set ...`: an existing spec for this mapping
// is updated IN PLACE (keeping its stored id and CreatedAt — the SQL update
// set list never touches id or created_at); a spec.ID reused for a DIFFERENT
// mapping is the primary-key collision the SQL insert would raise as a unique
// violation, so it is rejected here too.
func (m *MemoryStore) UpsertRuntimeSpec(_ context.Context, spec RuntimeSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mappings[spec.MappingID]; !ok {
		return storeerr.ErrNotFound
	}
	for existingID, existing := range m.runtimeSpecs {
		if existing.MappingID == spec.MappingID {
			spec.ID = existingID
			spec.CreatedAt = existing.CreatedAt
			m.runtimeSpecs[existingID] = spec
			return nil
		}
	}
	if existing, ok := m.runtimeSpecs[spec.ID]; ok && existing.MappingID != spec.MappingID {
		return storeerr.ErrConflict
	}
	m.runtimeSpecs[spec.ID] = spec
	return nil
}

// RuntimeSpecByMapping returns the spec for mappingID, if any. RuntimeSpec has
// no slice/pointer fields, so the map value is already a safe copy.
func (m *MemoryStore) RuntimeSpecByMapping(_ context.Context, mappingID string) (RuntimeSpec, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, spec := range m.runtimeSpecs {
		if spec.MappingID == mappingID {
			return spec, true, nil
		}
	}
	return RuntimeSpec{}, false, nil
}

// RuntimeSpecsByApplication lists every spec whose mapping belongs to appID,
// ordered by spec id (mirrors the SQL join's `order by s.id`).
func (m *MemoryStore) RuntimeSpecsByApplication(_ context.Context, appID string) ([]RuntimeSpec, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]RuntimeSpec, 0)
	for _, spec := range m.runtimeSpecs {
		if mapping, ok := m.mappings[spec.MappingID]; ok && mapping.ApplicationID == appID {
			out = append(out, spec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// DeleteRuntimeSpec removes the spec by id, cascading deletion of its GPU
// rows (mirrors agent_runtime_spec_gpus' ON DELETE CASCADE).
func (m *MemoryStore) DeleteRuntimeSpec(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runtimeSpecs[id]; !ok {
		return storeerr.ErrNotFound
	}
	delete(m.runtimeSpecs, id)
	delete(m.runtimeSpecGPUs, id)
	return nil
}

// SetRuntimeSpecGPUs atomically replaces specID's whole set of per-GPU VRAM
// rows (mirrors the SQL delete-then-insert transaction; the in-memory
// assignment is already atomic under m.mu). specID must exist.
func (m *MemoryStore) SetRuntimeSpecGPUs(_ context.Context, specID string, gpus []RuntimeSpecGPU) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runtimeSpecs[specID]; !ok {
		return storeerr.ErrNotFound
	}
	stored := copyRuntimeSpecGPUs(gpus)
	for i := range stored {
		stored[i].SpecID = specID
	}
	m.runtimeSpecGPUs[specID] = stored
	return nil
}

// RuntimeSpecGPUs returns specID's per-GPU VRAM rows ordered by GPU index
// (mirrors the SQL `order by gpu_index`). The slice is deep-copied so the
// caller cannot alias internal state.
func (m *MemoryStore) RuntimeSpecGPUs(_ context.Context, specID string) ([]RuntimeSpecGPU, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := copyRuntimeSpecGPUs(m.runtimeSpecGPUs[specID])
	sort.Slice(out, func(i, j int) bool { return out[i].GPUIndex < out[j].GPUIndex })
	return out, nil
}

// UpdateRuntimeSpecGPUMeasured writes back one agent measurement, touching
// only VRAMMeasuredMB (VRAMEstimateMB, operator-owned, is never written
// here). ErrNotFound when the (specID, gpuIndex) row does not exist.
func (m *MemoryStore) UpdateRuntimeSpecGPUMeasured(_ context.Context, specID string, gpuIndex int, measuredMB int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.runtimeSpecGPUs[specID]
	for i := range rows {
		if rows[i].GPUIndex == gpuIndex {
			rows[i].VRAMMeasuredMB = measuredMB
			return nil
		}
	}
	return storeerr.ErrNotFound
}

// copyRuntimeSpecGPUs returns an ALWAYS-non-nil copy of gpus: RuntimeSpecGPUs'
// documented contract (store.go) is "always non-nil, empty when none", which
// the SQL side satisfies via `make([]routing.RuntimeSpecGPU, 0)`. Building
// this with `append([]RuntimeSpecGPU(nil), gpus...)` would silently return a
// bare nil for a nil OR an already-empty gpus (append onto a nil slice
// literal with zero elements to append always yields nil in Go), diverging
// from the SQL store and surfacing downstream as a JSON `null` vs `[]`
// difference. `make` + `copy` always allocates, even for length 0.
func copyRuntimeSpecGPUs(gpus []RuntimeSpecGPU) []RuntimeSpecGPU {
	out := make([]RuntimeSpecGPU, len(gpus))
	copy(out, gpus)
	return out
}
