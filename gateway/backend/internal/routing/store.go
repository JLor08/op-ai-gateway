// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	ProviderMock      = "mock"
	ProviderOllama    = "ollama"
	ProviderVLLM      = "vllm"
	ProviderLlamaCPP  = "llama_cpp"
	ProviderLlamaSwap = "llama_swap"
	ProviderLiteLLM   = "litellm"
	// ProviderServerAgent marks an application as an agent-managed model
	// runtime (the agent-runtime-manager feature): its launch spec, per-GPU
	// VRAM rows, and co-residency rules live in RuntimeStore. Routing treats
	// it like any other application type otherwise.
	ProviderServerAgent = "server_agent"

	ServerStatusActive      = "active"
	ServerStatusDisabled    = "disabled"
	ServerStatusMaintenance = "maintenance"

	HealthUnknown   = "unknown"
	HealthHealthy   = "healthy"
	HealthDegraded  = "degraded"
	HealthUnhealthy = "unhealthy"

	// HealthCheckMode* select how an application's reachability is determined by
	// the background probe loop:
	//   - always_reachable: never probed, always counted reachable.
	//   - health_path:      HTTP GET of HealthCheckPath must return 2xx.
	//   - model_sync:       the model-discovery endpoint (ListModels) must
	//                        succeed; every successful check also reconciles the
	//                        application's model mappings (same as a manual sync).
	HealthCheckModeAlwaysReachable = "always_reachable"
	HealthCheckModeHealthPath      = "health_path"
	HealthCheckModeModelSync       = "model_sync"

	APIFlavorOpenAI    = "openai"
	APIFlavorAnthropic = "anthropic"

	// PrincipalTypeService / PrincipalTypeUser are the two supported
	// principal_limits owner kinds (Phase 2 of the service-accounts work):
	// PrincipalTypeService matches usage_events.service_id, PrincipalTypeUser
	// matches usage_events.user_id. See LimitConfig + Store.PrincipalLimits.
	PrincipalTypeService = "service"
	PrincipalTypeUser    = "user"
)

type AIServer struct {
	ID     string
	Name   string
	Domain string
	// ServerPathSuffix is an optional URL path segment appended to the origin
	// (after Domain:Port) when composing the reachable base URL. Empty = none.
	ServerPathSuffix string
	// NetBird integration (migration v18). NetbirdEnabled marks the server as a
	// NetBird network peer whose Domain is auto-managed from the peer's DNS name.
	// NetbirdSetupKeyID + NetbirdGroupID are stored so the setup key can be
	// regenerated and the peer correlated via the per-server tracking group;
	// NetbirdPeerID + NetbirdConnected are the state kept fresh by the sync loop.
	// Routing never reads these (they are absent from the candidate join).
	NetbirdEnabled    bool
	NetbirdSetupKeyID string
	NetbirdGroupID    string
	NetbirdPeerID     string
	NetbirdConnected  bool
	// NetbirdGroupIDs is the portal's mirror of the peer's NetBird POLICY groups
	// (excluding the per-server tracking group) — an OPAQUE JSON string
	// [{"id","name"}] the store treats as a dumb value (like a benchmark's
	// capacity curve). Routing never reads it (absent from the candidate join).
	NetbirdGroupIDs string
	// NetbirdPeerManaged marks a server whose NetBird peer + setup key originated
	// from a gateway-generated setup key (create hook / enroll / regenerate), so the
	// delete-peer checkbox can be pre-checked; a manually-linked peer stays false.
	// Routing never reads it (absent from the candidate join).
	NetbirdPeerManaged bool
	// NetbirdPolicyOverride is the per-server policy opt-in/opt-out override:
	// "" (default) / "include" / "exclude". Meaning depends on the effective
	// policy scope. Routing never reads it (absent from the candidate join).
	NetbirdPolicyOverride string
	// NetbirdAllowPing lets the gateway ICMP-ping this server (managed policy
	// op-gw-ping-servers). Routing never reads it — absent from the candidate join.
	NetbirdAllowPing bool
	// NetbirdPingExclude opts this server OUT of ping when the account-wide
	// "all servers pingable" switch is on (mutually exclusive with NetbirdAllowPing).
	NetbirdPingExclude bool
	// AgentPresenceTimeoutSeconds is the per-server override for "the agent is
	// delivering values" (the ServerAgent-presence window). 0 = follow the
	// system-wide agent_presence_timeout_seconds setting. Routing never reads
	// it (absent from the candidate join).
	AgentPresenceTimeoutSeconds int
	// Energy-attribution config (migration v35, purely additive — no engine
	// consumes these yet; a later phase attributes per-request energy using
	// them). All default 0 = "unset / use default". Routing never reads them
	// (absent from the candidate join).
	//
	// EstimatedWatts is the operator's estimate of the server's typical draw
	// under load, in watts. 0 = no estimate.
	EstimatedWatts float64
	// IdleWatts is the operator's estimate of the server's idle draw, in
	// watts. 0 = auto (a later phase may derive a default).
	IdleWatts float64
	// PricePerKwh is the electricity price used to cost the server's energy
	// use, in currency units per kWh. 0 = unset.
	PricePerKwh float64
	// Pue is the datacenter Power Usage Effectiveness multiplier applied on
	// top of the server's own draw. 0 = use the system-wide default.
	Pue float64
	// PriceUnit is the display unit for PricePerKwh (migration v37, additive
	// display metadata only — the canonical price VALUE stays EUR/kWh; this
	// field never changes what is stored, only how it is shown/entered).
	// portal.NormalizePriceUnit governs valid values; "" reads back as the
	// default "eur_cent". Routing never reads it (absent from the candidate
	// join).
	PriceUnit string
	// SystemGroupID is the admin-group permissions Phase B containment root
	// (migration v50): the id of the system-tier user_group this server
	// belongs to, "" for an ungrouped legacy server. A denormalized pointer
	// (no foreign key, opaque to routing), like NetbirdGroupIDs — a later
	// task (Task 3/4) consumes it for authorization; routing never reads it
	// (absent from the candidate join).
	SystemGroupID string
	// CertificateOverride is the per-server ACME opt-in/opt-out:
	// "" (follow cert_server_scope) / "include" / "exclude". Routing never reads
	// it (absent from the candidate join).
	CertificateOverride string
	// HTTPSSwitchOverride is the per-server https-auto-switch opt-in/opt-out
	// (P4): "" (follow cert_https_switch_mode) / "include" / "exclude". A
	// SINGLE 3-state column (not two booleans) so a global-mode flip can never
	// resurrect a stale opposite flag -- mirrors CertificateOverride exactly.
	// MEANING is mode-dependent (see httpsSwitchInScope): under "auto" this opts
	// a server OUT; under "selected" this opts a server IN. Routing never reads
	// it (absent from the candidate join).
	HTTPSSwitchOverride string
	// RuntimeMaxProcesses caps how many agent-managed runtime processes
	// (server_agent applications, migration 66, Task 3) may run concurrently
	// on this server. 0 = unlimited. Enforced by the agent-runtime-manager
	// admission logic (Task 6+); routing never reads it (absent from the
	// candidate join).
	RuntimeMaxProcesses int
	// ManagedRuntimeOnly restricts this server to agent-managed runtime
	// applications only, gating whether a non-runtime application may be
	// created on it (migration 66, Task 3; enforced in Task 6+). Routing
	// never reads it (absent from the candidate join).
	ManagedRuntimeOnly bool
	Provider           string
	Endpoint           string
	Status             string
	HealthStatus       string
	LastSeenAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Service is a Service Account (Phase 1 service accounts): an autonomous
// principal that owns 0..N service tokens (api_tokens with kind="service"),
// managed like an AI-Server — created by an admin, then administered by its
// delegates (ServiceDelegate) or any admin/system-admin. Status reuses the
// same active/disabled vocabulary as AIServer.Status; a disabled service's
// tokens are all rejected at LookupBearer (no per-token edit needed).
type Service struct {
	ID          string
	Name        string
	Description string
	Status      string // ServerStatusActive | ServerStatusDisabled
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// SystemGroupID is the admin-group permissions Phase C containment root
	// (migration v52): the id of the system-tier user_group this service
	// belongs to, "" for an ungrouped legacy service. A denormalized pointer
	// (no foreign key, opaque to routing), mirroring AIServer.SystemGroupID —
	// a later task consumes it for authorization; routing never reads it.
	SystemGroupID string
}

// ServiceDelegate is one user delegated to manage a Service (service_delegates,
// composite primary key service_id+user_id — no own id). CanManageSettings is
// the delegate STAGE: false = Token-Delegate (tokens + read only), true =
// Full-Delegate (also name/description/status/allowlist/delegate-list/delete).
type ServiceDelegate struct {
	UserID            string
	CanManageSettings bool
}

// ResourceGroup is a management structure (Resource Groups Phase 1, spec
// 2026-08-11 — Task 2, migration v54): a named container linking, via two n:m
// join tables, the admin groups that may MANAGE it
// (resource_group_admin_groups, consumed by a later task) and the AI-servers
// that are MEMBERS of it (resource_group_servers). Status reuses the same
// active/disabled vocabulary as AIServer.Status/Service.Status.
type ResourceGroup struct {
	ID     string
	Name   string
	Status string // ServerStatusActive | ServerStatusDisabled
	// SystemGroupID is the containment root (mirroring AIServer.SystemGroupID /
	// Service.SystemGroupID): the id of the system-tier user_group this
	// resource group belongs to, "" for an ungrouped resource group. A
	// denormalized pointer (no foreign key, opaque to routing) — a later task
	// consumes it for authorization; routing never reads it.
	SystemGroupID string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ResourceGroupProvision is one "provisioned-for" target of a resource group
// (Resource Groups Phase 2 provisioning, migration v55, resource_group_provisions).
// Kind is one of the ProvisionKind* consts; TargetID is the id of a
// user-group / admin-group / user / service depending on Kind. There is no FK
// on TargetID (polymorphic, opaque to the store); a dangling target simply
// never matches.
type ResourceGroupProvision struct {
	Kind     string
	TargetID string
}

const (
	ProvisionKindUserGroup  = "user_group"
	ProvisionKindAdminGroup = "admin_group"
	ProvisionKindUser       = "user"
	ProvisionKindService    = "service"
)

type ServerTelemetry struct {
	ServerID       string
	ReportedAt     time.Time
	AgentVersion   string
	OS             string
	Arch           string
	CPULoad        float64
	RAMUsedBytes   int64
	RAMTotalBytes  int64
	GPUCount       int
	VRAMUsedBytes  int64
	VRAMTotalBytes int64
	ActiveRequests int
	QueueDepth     int
	LatencyMS      int
	ErrorRate      float64
	ProviderHealth string
	Capabilities   string
	RawSummary     string
	UpdatedAt      time.Time
}

// ServerHardware is the latest static hardware inventory reported by a server's
// ServerAgent (server_hardware). 1:1 per server, upsert-overwrite. ReportJSON is a
// validated canonical JSON blob of the agent's SystemReport, opaque to the store —
// it never contains serials, board/chassis UUIDs, or MAC addresses (privacy D4).
type ServerHardware struct {
	ServerID    string
	CollectedAt time.Time
	UpdatedAt   time.Time
	ReportJSON  string
}

// TelemetrySample is one rich per-server performance sample pushed by the
// ServerAgent. It is persisted (server_telemetry_samples) and fanned out live;
// distinct from ServerTelemetry (the single latest routing-scorer summary row).
type TelemetrySample struct {
	ServerID   string
	ReportedAt time.Time
	CPUUtilPct float64
	// CPUCores is per-core utilization %. Live-only: carried in the in-memory
	// perf ring + SSE + DTO, NOT persisted to server_telemetry_samples.
	CPUCores       []float64
	MemUsedBytes   int64
	MemTotalBytes  int64
	SwapUsedBytes  int64
	SwapTotalBytes int64
	Load1          float64
	Load5          float64
	Load15         float64
	ActiveRequests int
	QueueDepth     int
	GPUs           []GPUSample
	Net            []NetSample
	// CPUPowerW / SystemPowerW are best-effort, NULLABLE host-level power draw in
	// watts. Unlike CPUCores, these ARE persisted (server_telemetry_samples,
	// migration v28) so they appear in history windows. nil = not measured.
	CPUPowerW    *float64
	SystemPowerW *float64
	// CPUTempC is the best-effort, NULLABLE CPU package temperature (°C); nil = not measured.
	CPUTempC *float64
}

// ServerAvailabilitySample is one point in a server's availability history: the
// derived health state + whether the ServerAgent was reporting at that time.
// Persisted (server_availability_samples); event-sourced by the health loop.
type ServerAvailabilitySample struct {
	ServerID       string
	ReportedAt     time.Time
	Health         string // routing.Health* value ("unknown" is valid; "" only pre-persist)
	ReachableCount int
	ActiveCount    int
	AgentReporting bool
	// NetbirdConnected is whether the server's linked NetBird peer was connected at
	// this time (the netbird_connected column, written by the sync loop, sampled by
	// the health loop). Constantly false for a server with no NetBird peer.
	NetbirdConnected bool
	// GapBefore is true when this sample's RAW predecessor was more than the gap
	// floor away in time (an observer gap — the gateway was not sampling). It is
	// set only on read by the reduction, never stored (no DB column).
	GapBefore bool
}

// BenchmarkRun is one benchmarked mapping's measured metrics from a single
// benchmark run, appended to the model_mapping_benchmarks history table (one row
// per benchmarked mapping per run). ServerID has no FK (the run survives server
// churn); the mapping FK cascade is enough. A non-empty Error records a failed
// measurement (the metric fields are then their zero "unknown" value).
type BenchmarkRun struct {
	ID                    string
	MappingID             string
	ServerID              string
	CreatedAt             time.Time
	GenTokensPerSecond    float64
	PromptTokensPerSecond float64
	LoadTimeMS            int
	ContextSize           int
	Error                 string
	// Kind distinguishes a speed-history row ("speed", the P3b default and the value
	// for a legacy row where the column is empty) from a capacity-ramp row ("capacity").
	Kind string
	// CapacityCurve is the raw JSON of a CapacityReport for a kind=="capacity" row
	// (empty for a speed row). The store treats it as an opaque string; the gateway
	// marshals it and the portal decodes it — mirroring how `Error` is a plain column.
	CapacityCurve string
	// VisionCapable is the definitive vision-acceptance verdict for a kind=="vision"
	// row (false/unused for any other kind). An inconclusive vision run still writes
	// a row with VisionCapable=false and Error set — the caller distinguishes the two
	// via Error, mirroring how a failed speed row's metric fields are zero/unknown.
	VisionCapable bool
	// VRAMJSON is the raw JSON of a VRAM benchmark's per-GPU result for a
	// kind=="vram" row (empty for every other kind, and empty for a VRAM run
	// that reached no result at all — Error then says why). Its own column
	// (`vram_json`), NOT a second tenant of CapacityCurve: the store treats it
	// as an opaque string, the gateway marshals it and the portal decodes it,
	// exactly like CapacityCurve above.
	//
	// The row is EVIDENCE, never authority. Nothing in admission, scoring or
	// the launch spec reads it: `vram_measured_mb` stays agent-owned and
	// `vram_estimate_mb` operator-owned, and the operator applies a measured
	// number to their own field themselves (see the agent-runtime-manager
	// cross-cutting doc's ownership split).
	VRAMJSON string
}

// CapacityLevel is one concurrency level measured during a capacity ramp. Every
// ATTEMPTED level is recorded (including the one that tripped a stop-check, whose
// StopReason is set), so the history view can show why the ramp stopped.
type CapacityLevel struct {
	Concurrency               int     `json:"concurrency"`
	AggregateTokensPerSecond  float64 `json:"aggregate_tokens_per_second"`
	PerRequestTokensPerSecond float64 `json:"per_request_tokens_per_second"`
	MeanLatencyMS             int64   `json:"mean_latency_ms"`
	Successes                 int     `json:"successes"`
	Errors                    int     `json:"errors"`
	VRAMFreePct               float64 `json:"vram_free_pct,omitempty"`
	RAMFreePct                float64 `json:"ram_free_pct,omitempty"`
	RequestsDeferred          int     `json:"requests_deferred,omitempty"`
	RequestsProcessing        int     `json:"requests_processing,omitempty"`
	TotalSlots                int     `json:"total_slots,omitempty"`
	// StopReason is "" for a level that passed every check, else the name of the
	// signal that stopped the ramp at this level ("error"|"memory"|"queue"|"latency"|"slot_ceiling").
	StopReason string `json:"stop_reason,omitempty"`
}

// CapacityReport is the full result of a capacity ramp: the distilled headline
// scalars plus the per-level curve. It is JSON-serialized into
// BenchmarkRun.CapacityCurve for a kind=="capacity" history row.
type CapacityReport struct {
	MaxConcurrency               int             `json:"max_concurrency"`
	RecommendedConcurrency       int             `json:"recommended_concurrency"`
	GenTokensPerSecondAtCapacity float64         `json:"gen_tokens_per_second_at_capacity"`
	MemoryObserved               bool            `json:"memory_observed"`
	Levels                       []CapacityLevel `json:"levels,omitempty"`
}

type GPUSample struct {
	Index         int
	Name          string
	UUID          string
	UtilPct       float64
	MemUsedBytes  int64
	MemTotalBytes int64
	TempC         int
	VRAMTempC     int
	PowerW        float64
	FanPct        float64
}

type NetSample struct {
	Name    string
	RxBytes int64
	TxBytes int64
}

type RouteAffinity struct {
	ID         string
	APITokenID string
	UserID     string
	Model      string
	// ResolvedModel is the concrete member gateway model a group request was
	// pinned to (sticky/climb_up failover). Empty for a plain single-model pin
	// (Model then IS the resolved model). Used only by the model-group resolver.
	ResolvedModel string
	APIFlavor     string
	SessionID     string
	ApplicationID string
	ServerID      string
	ExpiresAt     time.Time
	LastUsedAt    time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ModelGroup is a named priority-failover group offered to clients as a synthetic
// gateway model. Its members are gateway model names, walked in priority order.
type ModelGroup struct {
	ID               string
	GatewayModelName string // the client-facing name; offered as a model
	DisplayName      string
	Status           string // ServerStatusActive | ServerStatusDisabled
	FailoverMode     string // "sticky" | "climb_up"
	Traversal        string // "depth" | "breadth" | "round_robin" — subgroup expansion order
	CreatedAt        time.Time
	UpdatedAt        time.Time
	// LoadedOnly restricts selection to members with an already-loaded candidate,
	// so serving a request does not trigger a model load. When nothing is loaded
	// the restriction is dropped for that request (never a dead end).
	LoadedOnly bool
	// MemberOrder is how the group's members are ordered for the walk:
	// MemberOrderPriority (the manual order) or MemberOrderSpeed (fastest
	// effective generation speed first). Unknown values fail open to priority.
	MemberOrder string
	// ClimbSpeedMarginPercent is how much faster a member must be before a
	// SPEED-ordered climb_up leaves an available pin. Priority-ordered groups
	// ignore it (no fluctuating measurement is involved).
	ClimbSpeedMarginPercent int
	// MinTokensPerSecond is the minimum effective generation speed a candidate
	// must reach to count as available; 0 disables the floor. An unmeasured
	// candidate (0) never satisfies a floor.
	MinTokensPerSecond float64
	// MinSpeedFallback is what happens when no candidate reaches the floor:
	// MinSpeedFallbackError (ErrNoHealthyHost, 502) or MinSpeedFallbackIgnore
	// (retry without it).
	MinSpeedFallback string
}

const (
	MemberOrderPriority = "priority"
	MemberOrderSpeed    = "speed"

	MinSpeedFallbackError  = "error"
	MinSpeedFallbackIgnore = "ignore"

	// DefaultClimbSpeedMarginPercent is the shipped default margin.
	DefaultClimbSpeedMarginPercent = 20
)

// GroupMember is one ordered member of a ModelGroup: a gateway model NAME (loose
// reference, not a mapping id) and its priority (lower = higher priority / earlier
// failover). Visibility is NOT here — it lives on the model (ModelSetting).
type GroupMember struct {
	ID                string
	GroupID           string
	MemberGatewayName string
	Priority          int
	CreatedAt         time.Time
}

// ModelSetting is per-MODEL metadata keyed by the gateway model NAME (the logical
// model shown in the models list). Holds visibility now; extensible for more info.
type ModelSetting struct {
	GatewayModelName string
	Visibility       string // "shown" (default) | "hidden" | "locked"
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type AffinityKey struct {
	APITokenID string
	Model      string
	APIFlavor  string
	SessionID  string
}

type Application struct {
	ID                 string
	ServerID           string
	Type               string
	Port               int
	Scheme             string
	APIFlavors         []string
	Priority           int
	Weight             int
	TimeoutMS          int
	AffinityTTLSeconds int
	// AdmissionQueueTimeoutSeconds bounds how long an unpinned request waits in the CP4
	// admission queue for a free concurrency slot on this application's server. 0 = wait
	// until the client aborts (no server-side deadline).
	AdmissionQueueTimeoutSeconds int
	Status                       string
	AlwaysReachable              bool
	HealthCheckPath              string
	HealthCheckMode              string
	// HealthCheckIntervalSeconds is the per-application probe cadence. 0 means
	// "follow the system-wide health_check_interval_seconds setting" (and keep
	// following it as that setting changes); a value > 0 is a custom fixed cadence.
	HealthCheckIntervalSeconds int
	// ResponsesMode / MessagesMode are the per-application three-state endpoint
	// controls (design 2026-09-03) that replaced the native_responses /
	// native_messages booleans: for the Codex /v1/responses and Claude Code
	// /v1/messages endpoints respectively — EndpointModeDisabled (not served),
	// EndpointModeTranslate (translate to /v1/chat/completions, the old
	// native_*=false) or EndpointModePassthrough (proxy raw to the upstream's
	// native path, the old native_*=true). Whether the endpoint is served at all
	// also depends on the matching APIFlavors entry (see applicationServesEndpoint).
	ResponsesMode EndpointMode
	MessagesMode  EndpointMode
	// LoadedModelsPath is an optional upstream endpoint path the gateway (and/or the
	// server-agent) polls to learn which model(s) are currently LOADED/RUNNING (e.g.
	// llama-swap "/running", llama.cpp "/props", "/v1/models"). Empty = not tracked.
	// LoadedModelsFormat selects the response parser: "" / "auto" (tolerant,
	// multi-shape), "openai" (/v1/models data[].id), "llama_swap" (/running),
	// "llama_cpp" (/props).
	LoadedModelsPath   string
	LoadedModelsFormat string
	// ContextProbePath is an optional upstream path GET to learn context size
	// (llama.cpp /props); empty = off.
	ContextProbePath string
	// CapacityProbePath is an optional upstream path GET to learn the saturation
	// signal used by the capacity benchmark (llama.cpp /metrics Prometheus, or
	// /props|/slots JSON); empty = off.
	CapacityProbePath string
	// AppPathSuffix is an optional URL path segment appended to the origin (after
	// the server's ServerPathSuffix) when composing the reachable base URL. Empty = none.
	AppPathSuffix string
	// APIToken is the per-application upstream credential the gateway attaches to
	// every upstream call, stored SEALED (enc:/plain: envelope via capture.Cipher);
	// empty = none. Routing NEVER decrypts it — the sealed string only rides Target
	// to the cipher-holding layers (gateway *Server + app-health loop).
	APIToken string
	// APITokenHeader is an optional custom header name for APIToken; empty ⇒ the
	// default "Authorization: Bearer <token>".
	APITokenHeader string
	// BenchmarkScheduleEnabled / BenchmarkScheduleIntervalSeconds configure the P5
	// scheduled benchmark mode: when enabled, the health loop periodically re-runs
	// the model benchmark for this application's mappings on the given cadence
	// (0 = off / unset). OpportunisticMetricsEnabled turns on the P5 opportunistic
	// mode: an EWMA of gen/prompt tok/s derived from real usage events updates the
	// mappings' metrics (honoring metrics_locked). All default 0/false = feature OFF.
	BenchmarkScheduleEnabled         bool
	BenchmarkScheduleIntervalSeconds int
	OpportunisticMetricsEnabled      bool
	// ProxyListenPort is the TLS port the agent's local proxy listens on for this
	// application (P4 gateway-guided HTTPS switch: the app's Port stays the local
	// plaintext upstream the gateway reaches directly; ProxyListenPort is the
	// separate TLS-terminating port the agent exposes and the gateway routes to
	// once the app is switched to proxied HTTPS). 0 = not yet assigned; the
	// gateway auto-assigns it (see AssignProxyListenPort) the first time the app
	// needs one. An operator may also set it explicitly (validated unique per
	// server, same as Port).
	ProxyListenPort int
	// ProxyExcluded is the operator's explicit opt-out from the gateway-guided
	// TLS proxy, orthogonal to Scheme: an excluded application is never given a
	// proxy listener and the gateway never changes its scheme -- the operator
	// runs it on http or https themselves. It is the first expression this
	// system has of "leave this application alone", which no combination of
	// (Scheme, ProxyListenPort) could state for a PLAIN-HTTP application.
	//
	// AUTHORITATIVE: this is the ONLY representation of that decision.
	// Migration 70 backfilled the retired implicit encoding (https with
	// ProxyListenPort == 0) into it, so no reader anywhere needs to consult
	// that encoding to learn participation.
	//
	// INVARIANT: ProxyExcluded == true implies ProxyListenPort == 0, enforced
	// at the end of the mutation block in portal CreateApplication and
	// UpdateApplication. That invariant is why ApplicationEndpoint,
	// activePortStrings, revertScopeExit and HTTPSSwitchUnreachableApps need no
	// knowledge of this field: each of them tests
	// scheme == "https" && ProxyListenPort != 0, which an excluded application
	// can never satisfy.
	ProxyExcluded bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type ModelMapping struct {
	ID               string
	ApplicationID    string
	GatewayModelName string
	AppModelName     string
	Status           string
	// Per-mapping performance metrics used (in later phases) for server
	// selection. A zero value means "unknown": all metrics default to
	// 0/false/'' and no selection behavior depends on them in this phase.
	// Later phases populate them (measured or manually entered) and
	// MetricsSource / MetricsUpdatedAt record provenance.
	GenTokensPerSecond    float64    // generation throughput (tokens/s); 0 = unknown
	PromptTokensPerSecond float64    // prompt (prefill) throughput (tokens/s); 0 = unknown
	LoadTimeMS            int        // model load/swap time in ms; 0 = unknown
	ContextSize           int        // usable context window in tokens; 0 = unknown
	EnergyWhPerToken      float64    // per-token energy coefficient (watt-hours/token); 0 = unknown
	MetricsLocked         bool       // metrics are manually pinned (do not auto-overwrite)
	MetricsUpdatedAt      *time.Time // when the metrics were last set; nil = never
	MetricsSource         string     // provenance of the metrics (e.g. "manual"); '' = unknown
	// A CAPABILITY IS NOT A FIELD HERE. There is deliberately no IsMTP,
	// VisionCapable, LiveProgressSupport or Cap* on this struct: every
	// per-mapping capability verdict is a CapabilityRow in
	// model_mapping_capabilities, read through MappingCapabilities /
	// MappingCapabilitiesForMappings and keyed by CapabilityRowsByName.
	// Migration 79 dropped the eleven columns they used to be (see
	// migration79Up).
	//
	// The shape is not cosmetic; it is what the columns could not express:
	//
	//   - Three states, not a bool. A bool conflates "never probed" with
	//     "no", and the models list AND-aggregates vision fail-closed, so one
	//     unprobed mapping silently disabled a whole model's image
	//     attachment. Absence of a row IS unknown, so nothing has to encode
	//     it.
	//   - Per-capability PROVENANCE, not one mapping-wide MetricsSource every
	//     writer stamped over the last one's. That is what
	//     WritableCapabilityRows' precedence rank is built on, and what
	//     retired the mapping-wide vision SYNC rather than merely fixing it:
	//     with one source of truth per capability there is nothing to sync.
	//   - An OPEN vocabulary. cap_extra existed because the upstream set is
	//     open-ended (Ollama passes manifest-declared capabilities through
	//     unchanged); a row per name needs no escape hatch.
	//
	// Capabilities also sit OUTSIDE the MetricsLocked group this table
	// otherwise guards every automated writer with, which is the other reason
	// they are not simply renamed fields here: see
	// MappingStore.UpsertMappingCapabilities, which carries that argument.

	// Per-mapping concurrency-capacity metrics (later phases populate them).
	// 0 = unknown everywhere.
	MaxConcurrency               int     // max concurrent requests the model can serve; 0 = unknown
	RecommendedConcurrency       int     // recommended concurrent requests before throughput degrades; 0 = unknown
	GenTokensPerSecondAtCapacity float64 // aggregate generation throughput at MaxConcurrency (tokens/s); 0 = unknown
	CreatedAt                    time.Time
	UpdatedAt                    time.Time
}

// LimitConfig is a principal's (a Service or a User — see PrincipalType*)
// optional rate/quota/budget limits (migration v41, principal_limits; Phase 2
// of the service-accounts work). Every field's zero value means "that
// specific limit is off": RateRequests<=0 disables the rate limit,
// RequestQuota<=0/RequestQuotaPeriod=="" disables the request quota,
// TokenQuota<=0/TokenQuotaPeriod=="" disables the token quota,
// CostBudget<=0/CostBudgetPeriod=="" disables the cost budget. A zero-value
// LimitConfig is therefore a full no-op (no limit enforced at all) — the
// convention every store implementation's PrincipalLimits(ok=false) return
// also satisfies (see Store.PrincipalLimits).
//
// TokenQuota is int64 (bigint column), not int, because a monthly token-quota
// threshold — and the UsageAggregateSince sum it is compared against — can
// exceed int32 (the int4-overflow lesson from server_telemetry's byte
// columns, migration4Up).
type LimitConfig struct {
	RateRequests       int
	RateWindowSeconds  int
	RequestQuota       int
	RequestQuotaPeriod string
	TokenQuota         int64
	TokenQuotaPeriod   string
	CostBudget         float64
	CostBudgetPeriod   string
}

type AgentToken struct {
	ID           string
	ServerID     string
	SecretPrefix string
	LastUsedAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// EffectiveHealthCheckIntervalSeconds resolves an application's probe cadence:
// its custom HealthCheckIntervalSeconds when set (> 0), otherwise the supplied
// system-wide default. The custom value is clamped to [min,max] so a stored or
// out-of-range value can never produce a degenerate cadence.
func EffectiveHealthCheckIntervalSeconds(app Application, systemDefault, minSeconds, maxSeconds int) int {
	if app.HealthCheckIntervalSeconds <= 0 {
		return systemDefault
	}
	if app.HealthCheckIntervalSeconds < minSeconds {
		return minSeconds
	}
	if app.HealthCheckIntervalSeconds > maxSeconds {
		return maxSeconds
	}
	return app.HealthCheckIntervalSeconds
}

// EffectiveAgentPresenceTimeoutSeconds resolves a server's agent-presence
// window: its custom AgentPresenceTimeoutSeconds when set (> 0), otherwise the
// supplied system-wide default. The custom value is clamped to [min,max] so a
// stored or out-of-range value can never produce a degenerate window.
func EffectiveAgentPresenceTimeoutSeconds(server AIServer, systemDefault, minSeconds, maxSeconds int) int {
	if server.AgentPresenceTimeoutSeconds <= 0 {
		return systemDefault
	}
	if server.AgentPresenceTimeoutSeconds < minSeconds {
		return minSeconds
	}
	if server.AgentPresenceTimeoutSeconds > maxSeconds {
		return maxSeconds
	}
	return server.AgentPresenceTimeoutSeconds
}

// EffectiveHealthCheckMode resolves an application's health-check mode,
// deriving it from the legacy AlwaysReachable bool when the mode is unset (empty
// string), so pre-mode rows keep their behaviour: always_reachable -> "always
// reachable", otherwise the health-path probe. New rows always store an explicit
// mode.
func EffectiveHealthCheckMode(app Application) string {
	switch app.HealthCheckMode {
	case HealthCheckModeAlwaysReachable, HealthCheckModeHealthPath, HealthCheckModeModelSync:
		return app.HealthCheckMode
	default:
		if app.AlwaysReachable {
			return HealthCheckModeAlwaysReachable
		}
		return HealthCheckModeHealthPath
	}
}

// AssignProxyListenPort resolves the TLS proxy-listen port for app. When app
// already has one (ProxyListenPort != 0) it is returned unchanged — the
// assignment is idempotent, so re-running it against an already-assigned
// application never reassigns or churns its port. Otherwise it picks the
// lowest port >= base that is not already used as ProxyListenPort by another
// application in serverApps (typically every application on app's server,
// e.g. from Store.ApplicationsByServer), so the result is unique per server.
// It does not persist anything; the caller writes the result back via
// UpdateApplication.
func AssignProxyListenPort(serverApps []Application, app Application, base int) int {
	if app.ProxyListenPort != 0 {
		return app.ProxyListenPort
	}
	taken := make(map[int]struct{}, len(serverApps))
	for _, other := range serverApps {
		if other.ID == app.ID {
			continue
		}
		if other.ProxyListenPort != 0 {
			taken[other.ProxyListenPort] = struct{}{}
		}
	}
	port := base
	for {
		if _, used := taken[port]; !used {
			return port
		}
		port++
	}
}

// joinURLPath appends path segments to a base origin, trimming each segment's
// surrounding slashes and dropping empties, so an origin + optional server/app
// suffixes compose into a clean base URL (no doubled or trailing slashes). Empty
// segments ⇒ the origin unchanged.
func joinURLPath(base string, segments ...string) string {
	out := strings.TrimRight(base, "/")
	for _, seg := range segments {
		seg = strings.Trim(strings.TrimSpace(seg), "/")
		if seg == "" {
			continue
		}
		out += "/" + seg
	}
	return out
}

// ApplicationEndpoint returns the reachable base URL for an application: the
// origin (scheme://domain:port) with the server's and application's optional path
// suffixes appended.
//
// P4 proxied-HTTPS branch: when the app is in proxied state -- Scheme "https"
// AND a non-zero ProxyListenPort (the gateway-guided auto-switch flipped it, see
// the switch reconcile) -- the reachable origin is the agent's TLS-terminating
// proxy listener (ProxyListenPort), NOT the plaintext upstream Port the gateway
// reaches directly in the http/normal-https cases. A normal https app
// (ProxyListenPort 0, its own TLS on Port) and an http app are unchanged.
func ApplicationEndpoint(server AIServer, app Application) string {
	scheme := app.Scheme
	if scheme == "" {
		scheme = "http"
	}
	port := app.Port
	if scheme == "https" && app.ProxyListenPort != 0 {
		port = app.ProxyListenPort
	}
	origin := fmt.Sprintf("%s://%s:%d", scheme, server.Domain, port)
	return joinURLPath(origin, server.ServerPathSuffix, app.AppPathSuffix)
}

// MappingCandidate is one routable path for a gateway model: an active model
// mapping, the application that serves it, and that application's server.
type MappingCandidate struct {
	Server      AIServer
	Application Application
	Mapping     ModelMapping
	// LiveProgressSupport is the mapping's "live_progress" capability verdict
	// (model_mapping_capabilities, migration 78), filled by
	// ActiveMappingsForModel's joined query (SQL) or its MemoryStore mirror
	// from the same capability map, via LiveProgressSupportFromVerdict: "" for
	// no row (never determined), "supported" for a "yes" row, "unsupported"
	// for a "no" row -- the same vocabulary Target.LiveProgressSupport speaks.
	//
	// It sits on the CANDIDATE rather than on Mapping because "this came from
	// the join" is then a fact the TYPE carries instead of one a caller has to
	// remember. ModelMapping deliberately has no capability field of its own
	// (migration 79 dropped the columns they were), so a mapping loaded
	// through MappingByID -- which never joins the capability table -- cannot
	// present a plausible-looking but unpopulated verdict. Anything holding
	// only a ModelMapping has to ask for the rows explicitly
	// (MappingCapabilities), which is exactly the reminder the type is there
	// to give.
	//
	// targetFrom reads THIS field when building a Target from a
	// MappingCandidate, whether the candidate came from
	// ActiveMappingsForModel's join or from resolveAffinity, which issues its
	// own keyed MappingCapabilities read because its mapping comes from
	// MappingsByApplication and is therefore unjoined (see resolveAffinity's
	// comment).
	//
	// The benchmark path is the third producer of this verdict and does not
	// go through a MappingCandidate at all: internal/gateway's
	// benchmarkTargetFor reads the row itself
	// (Server.benchmarkLiveProgressSupport) and carries it on
	// benchmarkTarget. All three read the SAME row through the SAME
	// LiveProgressSupportFromVerdict, so none of them can drift into its own
	// spelling of "supported".
	LiveProgressSupport string
}

// LiveProgressSupportFromVerdict translates the "live_progress" capability
// row's Verdict ("yes" / "no" / absent) into the "" / "supported" /
// "unsupported" vocabulary that MappingCandidate.LiveProgressSupport,
// Target.LiveProgressSupport and wantsLiveProgress's three-layer rule speak:
// CapabilityYes -> "supported", CapabilityNo -> "unsupported", anything else
// -- in practice only "", an absent row -- -> "". It is the inverse of
// LiveProgressCapabilityVerdict above (which goes the other way, from the
// probe's support vocabulary to a row's verdict).
//
// Both ActiveMappingsForModel (SQL) and MemoryStore's mirror call this SAME
// function on their respective source of the verdict, which is what keeps
// the conformance suite's cross-driver comparison meaningful: a divergence
// in either implementation's plumbing would surface as a suite failure, not
// a difference in what this conversion itself does.
func LiveProgressSupportFromVerdict(verdict string) string {
	switch verdict {
	case CapabilityYes:
		return "supported"
	case CapabilityNo:
		return "unsupported"
	default:
		return ""
	}
}

// Certificate is one ACME-managed TLS certificate, keyed by its FQDN. Kind is
// "gateway" | "server" | "public"; ServerID is set only for "server". Status is
// "pending" | "active" | "error" | "skipped" — a non-active row carries no PEM
// and zero times, only the reason in LastError. KeySealed holds the private key
// in the shared enc:/plain: sealed form (never the raw key). NotBefore/NotAfter
// are PARSED FROM THE LEAF, never assumed. Routing never reads this type.
type Certificate struct {
	Domain       string
	Kind         string
	ServerID     string
	FullchainPEM string
	KeySealed    string
	Fingerprint  string
	// IssuerFingerprint is the SHA-256 of the signing root (hex), set only in the
	// self_signed mode. The reconcile compares it against the CURRENT CA to spot a
	// leaf that still belongs to a rotated-out root and re-issue it.
	IssuerFingerprint string
	NotBefore         time.Time
	NotAfter          time.Time
	IssuedAt          time.Time
	Status            string
	LastError         string
	AttemptCount      int
	NextAttemptAt     time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// ServerStore is AI-Server CRUD plus the cross-cutting per-server admin
// surfaces (owner list, admin-group links, system-group containment,
// certificate/https-switch overrides, energy config). NetBird peer-sync state
// lives in NetbirdStateStore; agent tokens live in AgentTokenStore.
type ServerStore interface {
	CreateAIServer(ctx context.Context, host AIServer) error
	UpdateAIServer(ctx context.Context, host AIServer) error
	AIServerByID(ctx context.Context, id string) (AIServer, error)
	AIServers(ctx context.Context) ([]AIServer, error)
	SetServerHealth(ctx context.Context, serverID, health string) error
	// UpdateServerEnergyConfig writes only the five per-server energy-config
	// columns (estimated_watts, idle_watts, price_per_kwh, pue, price_unit) — a
	// targeted UPDATE that touches only those columns, so it cannot race a
	// concurrent full-row write. An unknown id is ErrNotFound.
	UpdateServerEnergyConfig(ctx context.Context, id string, estimatedWatts, idleWatts, pricePerKwh, pue float64, priceUnit string) error
	DeleteAIServer(ctx context.Context, id string) error
	SetServerOwners(ctx context.Context, serverID string, userIDs []string) error
	ServerOwners(ctx context.Context, serverID string) ([]string, error)
	ServersByOwner(ctx context.Context, userID string) ([]AIServer, error)
	// UpdateServerSystemGroup writes only system_group_id — the admin-group
	// permissions Phase B containment root (migration v50) — with a targeted
	// UPDATE (only that one column), so it cannot race a concurrent full-row
	// write. An unknown id is ErrNotFound.
	UpdateServerSystemGroup(ctx context.Context, serverID, systemGroupID string) error
	// UpdateServerCertificateOverride writes only certificate_override (the
	// per-server ACME opt-in/opt-out). Unknown id -> ErrNotFound.
	UpdateServerCertificateOverride(ctx context.Context, id, override string) error
	// UpdateServerHTTPSSwitchOverride writes only https_switch_override (the
	// per-server https-auto-switch opt-in/opt-out, P4). Unknown id -> ErrNotFound.
	UpdateServerHTTPSSwitchOverride(ctx context.Context, id, override string) error
	// SetServerAdminGroup links serverID to groupID (a server_admin_groups
	// row). Idempotent (on-conflict-do-nothing on the (server_id, group_id)
	// unique pair, mirrors SetProjectGroup); a missing serverID or groupID is
	// ErrNotFound (FK violation).
	SetServerAdminGroup(ctx context.Context, serverID, groupID string) error
	// RemoveServerAdminGroup unlinks serverID from groupID. A no-op
	// (non-error) when the link does not exist.
	RemoveServerAdminGroup(ctx context.Context, serverID, groupID string) error
	// ServerAdminGroups lists every admin-group id linked to serverID,
	// ordered by created_at then group_id. Always non-nil, empty when none.
	ServerAdminGroups(ctx context.Context, serverID string) ([]string, error)
	// ServersByAdminGroups returns every server linked to ANY of groupIDs
	// (deduped by server id). An empty groupIDs returns an empty slice
	// without issuing a query.
	ServersByAdminGroups(ctx context.Context, groupIDs []string) ([]AIServer, error)
}

// NetbirdStateStore is the set of targeted per-column UPDATEs that keep a
// server's NetBird peer-sync state (enabled flag, setup key, tracking group,
// peer id/domain, policy groups, provenance, ping opt-in/out) fresh without
// racing a concurrent full-row write.
type NetbirdStateStore interface {
	// UpdateServerNetbirdKey records the NetBird enabled flag + setup-key id +
	// tracking-group id for a server (a targeted UPDATE that touches only those
	// three columns), set when the setup key is created/regenerated. enabled is
	// always true from the create hook + enroll/regenerate path (so enrolling a
	// non-NetBird server flips the flag on).
	UpdateServerNetbirdKey(ctx context.Context, id string, enabled bool, setupKeyID, groupID string) error
	// UpdateServerNetbirdLink is the system-admin linkage editor: it sets the
	// NetBird enabled flag + peer id and RESETS netbird_connected (a targeted
	// UPDATE), so a stale connection state is not shown until the sync loop
	// re-confirms from the new peer id.
	UpdateServerNetbirdLink(ctx context.Context, id string, enabled bool, peerID string) error
	// UpdateServerNetbirdState writes the peer-synced server state — the domain
	// (from the peer's DNS name), the peer id, and the connected flag — as a
	// targeted UPDATE (only those three columns), so it cannot race a concurrent
	// full-row write.
	UpdateServerNetbirdState(ctx context.Context, id, domain, peerID string, connected bool) error
	// UpdateServerNetbirdGroups writes only netbird_group_ids — the opaque JSON
	// mirror of the peer's policy groups (a targeted UPDATE that touches only that
	// one column), so it cannot race a concurrent full-row or state write.
	UpdateServerNetbirdGroups(ctx context.Context, id, groupsJSON string) error
	// UpdateServerNetbirdPeerManaged writes only netbird_peer_managed — the
	// provenance flag marking a gateway-created NetBird peer (a targeted UPDATE that
	// touches only that one column), so it cannot race a concurrent full-row write.
	UpdateServerNetbirdPeerManaged(ctx context.Context, id string, managed bool) error
	// UpdateServerNetbirdPolicyOverride writes only netbird_policy_override — the
	// per-server policy opt-in/opt-out override (a targeted UPDATE that touches
	// only that one column), so it cannot race a concurrent full-row write.
	UpdateServerNetbirdPolicyOverride(ctx context.Context, id string, override string) error
	// UpdateServerNetbirdAllowPing writes only netbird_allow_ping — the per-server
	// flag letting the gateway ICMP-ping this server (a targeted UPDATE that touches
	// only that one column), so it cannot race a concurrent full-row write.
	UpdateServerNetbirdAllowPing(ctx context.Context, id string, allow bool) error
	// UpdateServerNetbirdPingExclude writes only netbird_ping_exclude (the per-server
	// ping opt-out). An unknown id is ErrNotFound.
	UpdateServerNetbirdPingExclude(ctx context.Context, id string, exclude bool) error
}

// AgentTokenStore manages the single ServerAgent bearer credential per server
// (agent_tokens: create/rotate, lookup by server, and the reverse hash lookup
// used to authenticate an inbound agent connection).
type AgentTokenStore interface {
	UpsertAgentToken(ctx context.Context, token AgentToken, secretHash string) error
	AgentTokenByServer(ctx context.Context, serverID string) (AgentToken, bool, error)
	DeleteAgentTokenByServer(ctx context.Context, serverID string) error
	LookupAgentToken(ctx context.Context, secretHash string) (serverID string, ok bool, err error)
}

// TelemetryStore holds the single latest routing-scorer summary row per
// server (ServerTelemetry) plus the latest static hardware inventory
// (ServerHardware). Historical per-sample series live in SampleStore.
type TelemetryStore interface {
	UpsertTelemetry(ctx context.Context, telemetry ServerTelemetry) error
	TelemetryByServer(ctx context.Context, serverID string) (ServerTelemetry, bool, error)
	UpsertServerHardware(ctx context.Context, hardware ServerHardware) error
	ServerHardwareByServer(ctx context.Context, serverID string) (ServerHardware, bool, error)
}

// SampleStore holds the two persisted, time-ranged sample histories: rich
// per-server performance samples (TelemetrySample) and derived
// health/availability samples (ServerAvailabilitySample). Both support a
// bounded window read and a prune-before-cutoff for retention.
type SampleStore interface {
	InsertTelemetrySample(ctx context.Context, sample TelemetrySample) error
	TelemetrySamples(ctx context.Context, serverID string, from, to time.Time, limit int) ([]TelemetrySample, error)
	PruneTelemetrySamples(ctx context.Context, before time.Time) error
	InsertServerAvailabilitySample(ctx context.Context, sample ServerAvailabilitySample) error
	ServerAvailabilitySamples(ctx context.Context, serverID string, from, to time.Time, limit int) ([]ServerAvailabilitySample, error)
	PruneServerAvailabilitySamples(ctx context.Context, before time.Time) error
}

// BenchmarkStore is the append-only history of benchmark runs
// (model_mapping_benchmarks) — speed, capacity-ramp, and vision-probe rows —
// keyed by mapping with retention pruning.
type BenchmarkStore interface {
	InsertBenchmarkRun(ctx context.Context, run BenchmarkRun) error
	BenchmarkRunsByMapping(ctx context.Context, mappingID string, limit int) ([]BenchmarkRun, error)
	PruneBenchmarkRuns(ctx context.Context, before time.Time) error
}

// AffinityStore is the sticky-routing pin (route_affinity): upsert on a
// successful resolve, read on the next request from the same
// token/model/session, and best-effort delete when a pin is stale.
type AffinityStore interface {
	UpsertAffinity(ctx context.Context, affinity RouteAffinity) error
	Affinity(ctx context.Context, key AffinityKey) (RouteAffinity, bool, error)
	DeleteAffinity(ctx context.Context, key AffinityKey) error
}

// ApplicationStore is CRUD for an AI-Server's applications (the per-app
// endpoint config: port/scheme, health-check mode, native-passthrough flags,
// upstream credential, and the P4 proxied-HTTPS listen port).
type ApplicationStore interface {
	CreateApplication(ctx context.Context, app Application) error
	UpdateApplication(ctx context.Context, app Application) error
	ApplicationByID(ctx context.Context, id string) (Application, error)
	ApplicationsByServer(ctx context.Context, serverID string) ([]Application, error)
	DeleteApplication(ctx context.Context, id string) error
}

// CapabilityRow is one (mapping, capability) verdict together with WHO
// established it and when -- the unit the model_mapping_capabilities table
// stores, one row per capability.
//
// Verdict is "yes" or "no" and nothing else: "unknown" is the ABSENCE of a
// row, not a third value. That is what makes the never-overwrite-with-unknown
// discipline structural instead of a convention every writer has to remember
// -- there is no empty verdict to accidentally write.
//
// Source names the writer, and it is what makes the operator's rule
// expressible at all: a probe must never overwrite a verdict a human or a
// real measurement established. See CapabilitySource* below and
// UpsertMappingCapabilities.
type CapabilityRow struct {
	Capability string
	Verdict    string
	Source     string
	CheckedAt  time.Time
}

// Capability names. The vocabulary is deliberately OPEN -- an upstream may
// report capabilities this codebase has never heard of (Ollama passes
// manifest-declared names through verbatim), and those are stored and
// displayed as-is rather than dropped. These constants exist only for the
// capabilities the code itself reasons about.
const (
	CapabilityVision       = "vision"
	CapabilityVideo        = "video"
	CapabilityAudio        = "audio"
	CapabilityTools        = "tools"
	CapabilityMTP          = "mtp"
	CapabilityLiveProgress = "live_progress"
)

// Capability verdicts.
const (
	CapabilityYes = "yes"
	CapabilityNo  = "no"
)

// Capability sources. Precedence between them is a strict RANK
// (capabilitySourceRank), not a probe/not-probe split: manual outranks a
// benchmark, a benchmark outranks a probe, and a probe still overwrites its
// own or a migrated guess. manual and vision_benchmark are AUTHORITATIVE in
// the sense that no probe can ever outrank them; the probe sources remain
// overwritable by a later probe of the SAME rank, which is what lets a
// verdict re-establish itself after a build changes.
//
// CapabilitySourceLegacy marks a verdict inherited by migration 78 from a
// pre-78 column whose real origin is unknowable (is_mtp came from a NAME
// HEURISTIC or an operator; vision_capable's provenance was a mapping-wide
// string every writer overwrote). It ranks alongside a probe, deliberately:
// treating a guess as authoritative would freeze it in forever.
//
// CapabilitySourceOllamaAPIShow names the second probe in the vocabulary
// (#54): the agent's POST /api/show read of what Ollama DECLARES about a
// model, as opposed to the /props document a llama.cpp build serves. It is
// named as a constant so a row's provenance is a distinguishable fact
// rather than a plausible-looking lie -- an operator reading "llama_cpp_props"
// on a verdict that came from Ollama's manifest would be reading a false
// provenance, which is the one thing this column exists to prevent. It
// ranks 1 through capabilitySourceRank's DEFAULT branch (no rank-table
// entry, and none is wanted): a probe, never able to overwrite manual (3)
// or vision_benchmark (2), always able to repair its own drift (1 vs 1).
//
// Its verdicts are additionally one-directional at the SOURCE, a property no
// rank can express: Ollama's capability array is not exhaustive, so the
// detector behind this source can only ever produce "yes" or nothing at all
// (see server-agent's collector.detectOllamaCapabilities). A row with this
// source and verdict CapabilityNo could therefore not have come from that
// probe.
const (
	CapabilitySourceManual          = "manual"
	CapabilitySourceVisionBenchmark = "vision_benchmark"
	CapabilitySourceLlamaCppProps   = "llama_cpp_props"
	CapabilitySourceOllamaAPIShow   = "ollama_api_show"
	CapabilitySourceLegacy          = "legacy"
)

// capabilitySourceRank orders a capability row's source into the strict,
// total precedence WritableCapabilityRows enforces: the higher rank always
// wins a write, and an EQUAL rank is always overwritable -- a probe repairs
// its own drift; two sources of the same rank negotiate nothing between
// them.
//
//	3  CapabilitySourceManual          an operator's verdict.
//	2  CapabilitySourceVisionBenchmark a real measurement -- an actual image
//	                                   sent to the actual upstream, an actual
//	                                   answer read back.
//	1  CapabilitySourceLlamaCppProps,  a probe: re-reads the same document
//	   CapabilitySourceOllamaAPIShow,  (llama.cpp's /props, Ollama's
//	   CapabilitySourceLegacy,         /api/show), or a migrated heuristic,
//	   or any unrecognised source      every time. Both probes and an
//	                                   unrecognised source rank here -- fail
//	                                   SAFE toward "treat it as a probe"
//	                                   rather than silently handing an
//	                                   unknown writer manual's immunity,
//	                                   which is also why ollama_api_show
//	                                   needs no case of its own below.
//	0  no stored row at all            "unknown" -- see CapabilityRowsByName.
//
// WritableCapabilityRows is the only caller: a write is permitted iff
// rank(incoming) >= rank(current).
func capabilitySourceRank(source string) int {
	switch source {
	case CapabilitySourceManual:
		return 3
	case CapabilitySourceVisionBenchmark:
		return 2
	case CapabilitySourceLlamaCppProps, CapabilitySourceLegacy:
		return 1
	case "":
		return 0
	default:
		return 1
	}
}

// LiveProgressCapabilityVerdict maps the live-progress verdict vocabulary
// ("supported" / "unsupported", what provider.detectLiveProgressSupport
// produces and what the pre-78 live_progress_support column stored) onto the
// row model's own ("yes" / "no"), returning "" for anything else -- an older
// producer's empty string, or a value neither writer here recognises.
//
// The two vocabularies were never different facts, only different spellings,
// and the row model has exactly one: CapabilityYes or CapabilityNo, with
// "unknown" expressed by the ABSENCE of a row. Both probe write paths (the
// telemetry write-back in internal/gateway and the app-health pass in
// cmd/gateway) translate through this one function rather than each
// re-spelling the switch, so they cannot disagree about what "supported"
// means.
func LiveProgressCapabilityVerdict(support string) string {
	switch strings.TrimSpace(support) {
	case "supported":
		return CapabilityYes
	case "unsupported":
		return CapabilityNo
	default:
		return ""
	}
}

// CapabilityRowsByName keys a mapping's stored capability rows by capability
// name -- the shape every write path compares a fresh probe result against,
// and the memo shape the telemetry write-back folds its own writes back into.
// A capability with no row is simply absent from the map, so a zero-value
// lookup is "unknown": its empty Source is rank 0, below every real writer's,
// which is what makes a first-ever verdict always writable.
func CapabilityRowsByName(rows []CapabilityRow) map[string]CapabilityRow {
	out := make(map[string]CapabilityRow, len(rows))
	for _, r := range rows {
		out[r.Capability] = r
	}
	return out
}

// WritableCapabilityRows answers the one question every capability writer --
// a probe, or the vision benchmark -- has to ask -- which of the verdicts I
// just determined may I actually write, given what is already on file -- and
// is the single place that answer lives. Pure: no I/O, no store access,
// table-testable on its own.
//
// Three rules, in order:
//
//  0. ONE ROW PER CAPABILITY NAME: where reported names the same capability
//     twice, the FIRST occurrence decides and every later one is dropped --
//     whether or not the first was itself written. The capability vocabulary
//     is open on purpose (an upstream may report any name), so a reported
//     name CAN collide with one this code reasons about: Ollama's
//     manifest-declared capabilities literally include "vision", and
//     runtimeSampleCapabilityRows carries a dedicated live-progress verdict
//     beside an agent's open verdict list. Both producers therefore emit
//     their STRUCTURED verdicts first and their open-vocabulary ones after,
//     which is what makes first-wins mean "structured beats unstructured".
//     Before this rule, two rows for one capability both passed and both
//     reached the store, where the upsert loop's ordering made the last one
//     win -- the opposite outcome, arrived at by an emergent property of a
//     loop in another package rather than by a rule anyone had stated.
//
//  1. PRECEDENCE, the operator's rule and the reason this table carries a
//     per-row source at all: a write is permitted iff
//     capabilitySourceRank(incoming) >= capabilitySourceRank(current). Three
//     consequences fall out of that, and every existing caller relies on all
//     three:
//
//     - A probe (rank 1) against another probe's or a legacy row's rank-1
//     verdict is a TIE, and a tie is writable (>= holds at equality) -- so
//     a probe still repairs its own drift after a build changes, exactly
//     as before this rule was generalised from a boolean split.
//     - No stored row at all is rank 0, always lower than any incoming rank,
//     so a first-ever verdict from ANY source is always writable.
//     - The vision benchmark (rank 2) now loses only to a manual verdict
//     (rank 3) -- it still outranks and overwrites a probe or a legacy
//     row exactly as before. manual outranks everything, including the
//     benchmark, so an operator's verdict is permanent with no lock
//     needed at all -- see UpsertMappingCapabilities' own doc for what
//     this table has instead of metrics_locked.
//
//  2. CHANGE DETECTION: a write is dropped when the stored row already
//     agrees on BOTH the verdict and the rank -- i.e. there is nothing left
//     to record. This check runs only AFTER rule 1 permits the write, which
//     matters for one case worth stating rather than leaving a reader to
//     wonder: a benchmark verdict that AGREES with a stored MANUAL row never
//     reaches this rule at all, because rule 1 already blocked it. That
//     means it cannot refresh CheckedAt either. That is correct, not a gap:
//     the operator's row is the record, and a benchmark agreeing with it is
//     not a new measurement of anything the operator didn't already say.
//
//     Absent that interaction, this rule exists because a build capability
//     is stable by nature -- the same child build reports the same verdict
//     every second for its whole life -- so without it every telemetry
//     sample would drive one write per capability per mapping, forever, for
//     a value that cannot change short of an operator swapping the upstream
//     binary. That is why an EQUAL rank plus an equal verdict is dropped:
//     the row would be rewritten with the identical facts.
//
//     The rank is part of the comparison because it is itself a fact worth
//     recording, and one only a WRITE can change. A vision benchmark that
//     CONFIRMS a legacy or probe row's verdict is a real measurement of a
//     value only a document-reader had asserted before: leaving the row at
//     rank 1 would misattribute a measured verdict to a probe in the
//     per-capability tooltip, and -- worse -- leave the row overwritable by
//     the next probe, so a real measurement could still be talked over.
//     Comparing the verdict alone hid that upgrade behind an agreement.
//     This cannot reintroduce write amplification: a rank only ever RISES
//     for a given (mapping, capability), at most twice, and a repeat from
//     the same writer ties.
//
//     An ABSENT row has an empty verdict and rank 0, so a first-ever
//     verdict always passes this rule too.
//
// reported must already hold only the verdicts this writer actually
// determined, each with Verdict CapabilityYes or CapabilityNo and a
// non-empty Capability and Source: "unknown" is the ABSENCE of a row, so
// there is no empty verdict to express it with, and UpsertMappingCapabilities
// rejects a malformed row loudly (ValidateCapabilityRow) rather than
// half-writing a set. Projecting a writer's own answer shape onto rows --
// and dropping whatever it determined nothing about -- is each caller's own
// step, because only the caller knows what it reports and at what rank.
//
// Returns nil (not an empty slice) when nothing may be written, so a caller's
// len(rows) == 0 skips the store call entirely.
func WritableCapabilityRows(reported []CapabilityRow, stored map[string]CapabilityRow) []CapabilityRow {
	var out []CapabilityRow
	// rule 0's bookkeeping. Starts nil and is built on the first row, so a
	// nil or empty reported slice allocates nothing at all -- a nil map
	// reads as empty, which is exactly what "no name claimed yet" means.
	var seen map[string]struct{}
	for _, r := range reported {
		if _, dup := seen[r.Capability]; dup {
			continue // rule 0: the first occurrence of a name decides
		}
		if seen == nil {
			seen = make(map[string]struct{}, len(reported))
		}
		seen[r.Capability] = struct{}{}
		cur := stored[r.Capability]
		incomingRank := capabilitySourceRank(r.Source)
		currentRank := capabilitySourceRank(cur.Source)
		if incomingRank < currentRank {
			continue // outranked -- e.g. a probe (rank 1) against a manual verdict (rank 3)
		}
		if cur.Verdict == r.Verdict && incomingRank == currentRank {
			continue // already on file at this rank, no write amplification
		}
		out = append(out, r)
	}
	return out
}

// ValidateCapabilityRow rejects a CapabilityRow that could not have come from
// a caller behaving correctly: an empty Capability or Source, or a Verdict
// that is neither CapabilityYes nor CapabilityNo. That is not a stricter rule
// than the type already documents -- CapabilityRow's own doc comment says
// "there is no empty verdict to accidentally write" -- it is what makes the
// claim true instead of aspirational.
//
// Both UpsertMappingCapabilities implementations call this exact function
// (rather than each re-stating the check), so the two drivers cannot drift
// apart. Every caller of UpsertMappingCapabilities is best-effort -- it logs
// an error and carries on -- so failing loudly here costs nothing, and a
// caller passing an empty Verdict/Capability/Source has a bug worth
// surfacing rather than hiding.
func ValidateCapabilityRow(r CapabilityRow) error {
	if r.Capability == "" {
		return fmt.Errorf("capability row: capability must not be empty")
	}
	if r.Source == "" {
		return fmt.Errorf("capability row %q: source must not be empty", r.Capability)
	}
	if r.Verdict != CapabilityYes && r.Verdict != CapabilityNo {
		return fmt.Errorf("capability row %q: verdict must be %q or %q, got %q", r.Capability, CapabilityYes, CapabilityNo, r.Verdict)
	}
	return nil
}

// MappingStore is CRUD for model mappings (gateway model name -> app model
// name) plus the family of targeted, metrics_locked-respecting metric updates
// (context probe, benchmark, opportunistic EWMA, capacity, energy EWMA) and
// the routing-candidate lookup (ActiveMappingsForModel).
//
// It also owns the per-capability child rows (model_mapping_capabilities,
// migration 78): one row per (mapping, capability) carrying the verdict, its
// SOURCE and when it was established, where the ABSENCE of a row is
// "unknown". Those are the only writers here that do NOT respect
// metrics_locked, because they record a capability rather than a metric --
// UpsertMappingCapabilities carries the full argument, and it is the one this
// table's capability columns and migrations 76/77/78 all cite. See
// CapabilityRow.
type MappingStore interface {
	CreateMapping(ctx context.Context, mapping ModelMapping) error
	UpdateMapping(ctx context.Context, mapping ModelMapping) error
	// UpdateMappingContextProbe sets a mapping's context_size + provenance from a
	// context probe, atomically and ONLY when metrics_locked is false (a no-op,
	// non-error, when the mapping is missing or locked). It touches only the three
	// columns, so it cannot clobber a concurrent edit of other fields.
	UpdateMappingContextProbe(ctx context.Context, id string, contextSize int, at time.Time) error
	// UpdateMappingBenchmarkMetrics sets a mapping's measured throughput + load time
	// from a benchmark run, atomically and ONLY when metrics_locked is false (a no-op,
	// non-error, when the mapping is missing or locked). Touches only these four
	// columns + provenance, so it cannot clobber a concurrent edit of other fields.
	UpdateMappingBenchmarkMetrics(ctx context.Context, id string, genTPS, promptTPS float64, loadMS int, at time.Time) error
	// UpdateMappingOpportunisticMetrics EWMA-updates gen/prompt tok/s from a live sample,
	// atomically and ONLY when metrics_locked is false (no-op, non-error, when missing or
	// locked). A sample <= 0 leaves that column unchanged; a stored 0 is seeded by the first
	// positive sample. Stamps metrics_source="opportunistic" + metrics_updated_at.
	UpdateMappingOpportunisticMetrics(ctx context.Context, id string, genSample, promptSample, alpha float64, at time.Time) error
	// UpdateMappingCapacityMetrics sets a mapping's measured concurrency capacity + provenance
	// from a capacity benchmark, atomically and ONLY when metrics_locked is false (a no-op,
	// non-error, when the mapping is missing or locked). Touches only the 3 capacity columns +
	// provenance, so it cannot clobber a concurrent edit of other fields.
	UpdateMappingCapacityMetrics(ctx context.Context, id string, maxConcurrency, recommendedConcurrency int, genTPSAtCapacity float64, at time.Time) error
	// UpdateMappingEnergyEWMA EWMA-blends energy_wh_per_token from a live per-request
	// energy sample (watt-hours/token), atomically and ONLY when metrics_locked is
	// false (no-op, non-error, when missing or locked). A sample <= 0 leaves the
	// coefficient unchanged; a stored <= 0 coefficient is seeded by the first
	// positive sample. Stamps metrics_source="energy" + metrics_updated_at. Used by
	// the energy reconciler to calibrate a mapping's coefficient from measured
	// results (Tier 1 in the attribution engine).
	UpdateMappingEnergyEWMA(ctx context.Context, id string, sampleWhPerToken, alpha float64, at time.Time) error
	// MappingCapabilities lists one mapping's capability rows. An absent
	// capability is UNKNOWN and simply has no row.
	MappingCapabilities(ctx context.Context, mappingID string) ([]CapabilityRow, error)
	// MappingCapabilitiesForMappings is the BULK reader: one query for many
	// parents, keyed by mapping id. A mapping with no rows contributes no key.
	//
	// This repo has no other batch child-collection reader -- child collections
	// are N+1 loops in Go. This one is not gratuitous: the model-servers
	// listing already costs ~31 queries at the documented scale, and two
	// multipliers would turn a per-row capability read into a real cost -- the
	// SSE stream recomputes the entire listing on every loaded-registry
	// change, and the model-group endpoint calls the whole listing once per
	// group member.
	MappingCapabilitiesForMappings(ctx context.Context, mappingIDs []string) (map[string][]CapabilityRow, error)
	// UpsertMappingCapabilities writes rows, replacing any row for the same
	// (mapping, capability), atomically as one set -- a caller passing several
	// verdicts must never observe some of them applied and others not. It does
	// NOT apply the precedence rule -- callers do, because only they know what
	// rank their own write is (see capabilitySourceRank, and
	// WritableCapabilityRows for the answer every writer shares -- a probe
	// and the vision benchmark alike). It rejects (ValidateCapabilityRow),
	// without writing anything, a row whose Verdict is neither CapabilityYes
	// nor CapabilityNo or whose Capability or Source is empty.
	//
	// UNLIKE every metric writer above it carries NO metrics_locked guard and
	// never touches MetricsSource / MetricsUpdatedAt, and this doc comment is
	// the canonical statement of why -- the argument migrations 76/77/78, both
	// probe write paths and the ModelMapping capability fields all cite.
	// metrics_locked exists so an operator can pin NUMBERS THEY ANSWER FOR --
	// throughput, context size -- against automation. A capability is not such
	// a number: pinning it could only ever produce a wrong answer, and unlike
	// a pinned throughput a wrong capability has an operational consequence --
	// the feature it gates silently stays off, with no visible reason, until
	// someone thinks to unlock a mapping's metrics. And because it is a
	// capability rather than a metric, writing one must not restamp the metrics
	// provenance columns; doing so would misattribute this mapping's
	// throughput figures to a capability probe.
	//
	// What an operator gets INSTEAD of that lock is provenance, and it is a
	// strictly better trade: a row whose source is CapabilitySourceManual
	// outranks every other source (capabilitySourceRank) and so is never
	// overwritten by anything -- not a probe, and not the vision benchmark's
	// CapabilitySourceVisionBenchmark either, which itself outranks a probe
	// but not an operator. That is a per-capability guarantee rather than a
	// mapping-wide switch over numbers -- which is the whole reason the
	// pre-78 columns needed the lock argument in the first place -- and unlike
	// that lock it needs no separate flag: it falls out of provenance alone.
	//
	// An unknown mappingID is NOT the benign no-op every 0-rows-affected
	// write elsewhere in this file promises: model_mapping_capabilities.
	// mapping_id carries a real `references model_mappings(id) on delete
	// cascade` (migration 78), enforced on both SQL drivers (SQLite runs
	// with foreign_keys=ON), so this insert fails with a foreign-key
	// violation. MemoryStore mirrors that with its own hand-rolled
	// existence check (ErrNotFound), the same convention its sibling
	// mapping-child writers (UpsertRuntimeSpec, InsertBenchmarkRun) already
	// use -- silently fabricating a capabilities entry for a mapping that
	// does not exist was a driver divergence, not a considered choice, and
	// is fixed rather than merely documented. In practice this is reachable
	// only via a TOCTOU (the mapping is deleted between a caller's resolve
	// step and this write); every current caller is best-effort, so the
	// failure surfaces as one log line and never rejects the request or
	// telemetry sample that triggered it.
	UpsertMappingCapabilities(ctx context.Context, mappingID string, rows []CapabilityRow) error
	// DeleteMappingCapability returns one capability to UNKNOWN. Deleting a
	// row is how "not determined" is expressed -- the state the pre-78 bool
	// columns could not represent.
	DeleteMappingCapability(ctx context.Context, mappingID, capability string) error
	MappingByID(ctx context.Context, id string) (ModelMapping, error)
	MappingsByApplication(ctx context.Context, applicationID string) ([]ModelMapping, error)
	MappingsByServer(ctx context.Context, serverID string) ([]ModelMapping, error)
	DeleteMapping(ctx context.Context, id string) error
	ActiveMappingsForModel(ctx context.Context, gatewayModel string, apiFlavor string) ([]MappingCandidate, error)
}

// ModelGroupStore is model groups (migration v22 — a named priority-failover
// group offered as a synthetic gateway model, with its ordered GroupMember
// list) plus per-model settings (currently just visibility) keyed by gateway
// model name.
type ModelGroupStore interface {
	// Model groups (migration v22). A group is offered as a synthetic gateway
	// model and routes to the first available member in priority order.
	CreateModelGroup(ctx context.Context, group ModelGroup) error
	UpdateModelGroup(ctx context.Context, group ModelGroup) error
	ModelGroupByID(ctx context.Context, id string) (ModelGroup, error)
	ModelGroups(ctx context.Context) ([]ModelGroup, error)
	DeleteModelGroup(ctx context.Context, id string) error
	// SetGroupMembers atomically REPLACES a group's members (delete-then-insert).
	// A duplicate MemberGatewayName within the set surfaces ErrConflict; an unknown
	// group id surfaces ErrNotFound.
	SetGroupMembers(ctx context.Context, groupID string, members []GroupMember) error
	GroupMembersByGroup(ctx context.Context, groupID string) ([]GroupMember, error)
	// Per-model settings (visibility). Keyed by gateway model name.
	ModelSettings(ctx context.Context) ([]ModelSetting, error)
	ModelSettingByName(ctx context.Context, name string) (ModelSetting, bool, error)
	UpsertModelSetting(ctx context.Context, setting ModelSetting) error
}

// ServiceStore is Service Account CRUD (Phase 1 service accounts, migration
// v40) plus its per-service admin surfaces (delegates, model allowlist,
// system-group containment, admin-group links) — mirroring ServerStore's
// shape for the Service principal. Service-TOKEN persistence
// (CreatePlainToken with Kind/ServiceID set, TokensByService) lives on the
// store package's plain TokenRecord surface, not here — routing cannot
// import store.TokenRecord (store already imports routing) without a cycle.
type ServiceStore interface {
	CreateService(ctx context.Context, svc Service) error
	UpdateService(ctx context.Context, svc Service) error
	ServiceByID(ctx context.Context, id string) (Service, error)
	Services(ctx context.Context) ([]Service, error)
	// ServicesByDelegate lists the services where userID is a delegate (either
	// stage — Token- or Full-Delegate).
	ServicesByDelegate(ctx context.Context, userID string) ([]Service, error)
	DeleteService(ctx context.Context, id string) error
	// SetServiceDelegates atomically REPLACES a service's delegate list
	// (delete-then-insert). An unknown service id is ErrNotFound (even for an
	// empty set, mirroring SetGroupMembers); a duplicate UserID within the set
	// is ErrConflict (mirrors the service_delegates primary key).
	SetServiceDelegates(ctx context.Context, serviceID string, delegates []ServiceDelegate) error
	ServiceDelegates(ctx context.Context, serviceID string) ([]ServiceDelegate, error)
	// SetServiceAllowedModels atomically REPLACES a service's model allowlist
	// (delete-then-insert). An unknown service id is ErrNotFound (even for an
	// empty set); a duplicate model name within the set is ErrConflict. An
	// empty allowlist means "every model is allowed" (the default) — enforced
	// by the caller (the admission gate), not by the store.
	SetServiceAllowedModels(ctx context.Context, serviceID string, models []string) error
	ServiceAllowedModels(ctx context.Context, serviceID string) ([]string, error)
	// UpdateServiceSystemGroup writes only system_group_id — the admin-group
	// permissions Phase C containment root (migration v52) — with a targeted
	// UPDATE (only that one column), so it cannot race a concurrent full-row
	// write. An unknown id is ErrNotFound.
	UpdateServiceSystemGroup(ctx context.Context, serviceID, systemGroupID string) error
	// SetServiceAdminGroup links serviceID to groupID (a service_admin_groups
	// row). Idempotent (on-conflict-do-nothing on the (service_id, group_id)
	// unique pair, mirrors SetServerAdminGroup); a missing serviceID or
	// groupID is ErrNotFound (FK violation).
	SetServiceAdminGroup(ctx context.Context, serviceID, groupID string) error
	// RemoveServiceAdminGroup unlinks serviceID from groupID. A no-op
	// (non-error) when the link does not exist.
	RemoveServiceAdminGroup(ctx context.Context, serviceID, groupID string) error
	// ServiceAdminGroups lists every admin-group id linked to serviceID,
	// ordered by created_at then group_id. Always non-nil, empty when none.
	ServiceAdminGroups(ctx context.Context, serviceID string) ([]string, error)
	// ServicesByAdminGroups returns every service linked to ANY of groupIDs
	// (deduped by service id). An empty groupIDs returns an empty slice
	// without issuing a query.
	ServicesByAdminGroups(ctx context.Context, groupIDs []string) ([]Service, error)
}

// ResourceGroupStore is Resource Groups (Phase 1 management structure,
// migration v54): CRUD, the admin-group MANAGEMENT linkage
// (resource_group_admin_groups), the server MEMBERSHIP linkage
// (resource_group_servers), and Phase 2 provisioning
// (resource_group_provisions — which principals may USE the group's
// servers, distinct from management/membership).
type ResourceGroupStore interface {
	// CreateResourceGroup/UpdateResourceGroup/DeleteResourceGroup/
	// ResourceGroupByID/ResourceGroups mirror the AI-Server/Service CRUD
	// shape; UpdateResourceGroup writes name/status/updated_at only
	// (created_at and system_group_id are never touched by it — system_group_id
	// is written solely via UpdateResourceGroupSystemGroup).
	CreateResourceGroup(ctx context.Context, rg ResourceGroup) error
	UpdateResourceGroup(ctx context.Context, rg ResourceGroup) error
	DeleteResourceGroup(ctx context.Context, id string) error
	ResourceGroupByID(ctx context.Context, id string) (ResourceGroup, error)
	ResourceGroups(ctx context.Context) ([]ResourceGroup, error)
	// UpdateResourceGroupSystemGroup writes only system_group_id — the
	// containment root — with a targeted UPDATE (only that one column), so it
	// cannot race a concurrent full-row write. An unknown id is ErrNotFound.
	UpdateResourceGroupSystemGroup(ctx context.Context, rgID, systemGroupID string) error
	// SetResourceGroupAdminGroup links rgID to groupID (a
	// resource_group_admin_groups row: groupID may MANAGE the resource
	// group). Idempotent (on-conflict-do-nothing on the (resource_group_id,
	// group_id) unique pair, mirrors SetServiceAdminGroup); a missing rgID or
	// groupID is ErrNotFound (FK violation).
	SetResourceGroupAdminGroup(ctx context.Context, rgID, groupID string) error
	// RemoveResourceGroupAdminGroup unlinks rgID from groupID. A no-op
	// (non-error) when the link does not exist.
	RemoveResourceGroupAdminGroup(ctx context.Context, rgID, groupID string) error
	// ResourceGroupAdminGroups lists every admin-group id linked to rgID,
	// ordered by created_at then group_id. Always non-nil, empty when none.
	ResourceGroupAdminGroups(ctx context.Context, rgID string) ([]string, error)
	// ResourceGroupsByAdminGroups returns every resource group linked to ANY
	// of groupIDs (deduped by resource-group id). An empty groupIDs returns
	// an empty slice without issuing a query.
	ResourceGroupsByAdminGroups(ctx context.Context, groupIDs []string) ([]ResourceGroup, error)
	// SetResourceGroupServer links rgID to serverID (a resource_group_servers
	// row: serverID is a MEMBER of the resource group — membership, not
	// management). Idempotent (on-conflict-do-nothing on the
	// (resource_group_id, server_id) unique pair); a missing rgID or serverID
	// is ErrNotFound (FK violation).
	SetResourceGroupServer(ctx context.Context, rgID, serverID string) error
	// RemoveResourceGroupServer unlinks rgID from serverID. A no-op
	// (non-error) when the link does not exist.
	RemoveResourceGroupServer(ctx context.Context, rgID, serverID string) error
	// ResourceGroupServers lists every server id that is a member of rgID,
	// ordered by created_at then server_id. Always non-nil, empty when none.
	ResourceGroupServers(ctx context.Context, rgID string) ([]string, error)
	// ResourceGroupsByServer returns every resource group serverID is a
	// member of (deduped by resource-group id).
	ResourceGroupsByServer(ctx context.Context, serverID string) ([]ResourceGroup, error)

	// Resource Group provisioning (Phase 2, migration v55,
	// resource_group_provisions) — a resource group's "provisioned for" set:
	// which principals (user-groups/admin-groups/users/services) may USE the
	// resource group's servers. Distinct from the admin-group MANAGEMENT
	// linkage above (resource_group_admin_groups) and the server MEMBERSHIP
	// linkage (resource_group_servers); this is consumption authorization.
	//
	// SetResourceGroupProvision links rgID to one (kind, targetID) pair.
	// Idempotent (on-conflict-do-nothing on the (resource_group_id,
	// target_kind, target_id) unique triple); a missing rgID is ErrNotFound
	// (FK violation) — targetID carries no FK (polymorphic).
	SetResourceGroupProvision(ctx context.Context, rgID, kind, targetID string) error
	// RemoveResourceGroupProvision unlinks rgID from one (kind, targetID)
	// pair. A no-op (non-error) when the link does not exist.
	RemoveResourceGroupProvision(ctx context.Context, rgID, kind, targetID string) error
	// SetResourceGroupProvisions atomically REPLACES the whole provisioned-for
	// set of rgID with provisions (delete-then-insert in one transaction,
	// mirroring SetGroupMembers). An empty provisions clears the set. The
	// resource group must exist (ErrNotFound otherwise).
	SetResourceGroupProvisions(ctx context.Context, rgID string, provisions []ResourceGroupProvision) error
	// ResourceGroupProvisions lists every (kind, target) pair rgID is
	// provisioned for, ordered by (kind, target_id). Always non-nil, empty
	// when none.
	ResourceGroupProvisions(ctx context.Context, rgID string) ([]ResourceGroupProvision, error)
	// ResourceGroupIDsByProvisionTargets returns the ids of every resource
	// group provisioned for ANY of targetIDs under kind (deduped). An empty
	// targetIDs returns an empty result without issuing a query.
	ResourceGroupIDsByProvisionTargets(ctx context.Context, kind string, targetIDs []string) ([]string, error)
	// ProvisionedResourceGroupIDs returns the set of every resource group id
	// that carries at least one provision (of any kind).
	ProvisionedResourceGroupIDs(ctx context.Context) (map[string]bool, error)
}

// LimitsStore is the principal rate/quota/budget limits surface (migration
// v41, principal_limits, Phase 2 of the service-accounts work) — a single,
// principal-generic admission-control config shared by BOTH Services and
// Users (see PrincipalType*). No FK to either target (a principal can be
// deleted while its limit row is orphaned — harmless, since a deleted
// principal never authenticates again; callers best-effort clean up via
// DeletePrincipalLimits on deletion anyway).
type LimitsStore interface {
	// PrincipalLimits reads a principal's config. ok is false when no row
	// exists for (principalType, principalID) — the caller should then treat
	// the principal as having no limits (a zero LimitConfig), not an error.
	PrincipalLimits(ctx context.Context, principalType, principalID string) (LimitConfig, bool, error)
	// SetPrincipalLimits upserts a principal's limit config (the composite
	// primary key principal_type+principal_id determines insert vs. update).
	SetPrincipalLimits(ctx context.Context, principalType, principalID string, cfg LimitConfig) error
	// DeletePrincipalLimits removes a principal's limit config, if any (a
	// missing row is a benign no-op, mirroring the store's other delete
	// methods' idempotent-on-retry convention).
	DeletePrincipalLimits(ctx context.Context, principalType, principalID string) error
	// UsageAggregateSince sums, for the principal identified by
	// (principalType, principalID), the request COUNT, total TOKEN count, and
	// price-weighted COST recorded in usage_events with created_at >= since
	// (inclusive — mirrors the query layer's other time-range filters):
	// principalType PrincipalTypeService matches usage_events.service_id,
	// PrincipalTypeUser matches usage_events.user_id (exact string match).
	// tokens is int64 (a period's token sum can exceed int32, same reasoning
	// as LimitConfig.TokenQuota). cost mirrors the portal layer's per-host
	// price weighting (energy_wh/1000 * that server's own price_per_kwh when
	// set, else the system-wide energy_default_price_per_kwh default), so a
	// CostBudget threshold compares apples-to-apples with the rest of the
	// app's cost displays. Used by the quota/budget admission check
	// (internal/gateway's principal limiter, a later task) to read the
	// current calendar-period aggregate; an unrecognized principalType is an
	// error. The memory store holds no usage_events (usage in memory/dev mode
	// lives in the separate, non-persistent usage.Recorder) and so always
	// returns a zero aggregate — an honest no-op for that driver, consistent
	// with quota/budget enforcement being a persistent-store (sqlite/postgres)
	// feature per the design spec.
	UsageAggregateSince(ctx context.Context, principalType, principalID string, since time.Time) (requests int64, tokens int64, cost float64, err error)
}

// CertificateStore is the ACME/Let's-Encrypt design surface (migration v57,
// certificates) — one row per managed FQDN. server_id carries a real FK ->
// ai_servers(id) ON DELETE CASCADE, so deleting a server takes its
// certificate row with it.
type CertificateStore interface {
	// UpsertCertificate inserts or replaces the row for cert.Domain (insert or
	// on-conflict-update, keyed on the domain primary key); created_at is
	// preserved on an update — only a fresh insert uses cert.CreatedAt.
	UpsertCertificate(ctx context.Context, cert Certificate) error
	// CertificateByDomain returns storeerr.ErrNotFound when no row exists for
	// domain.
	CertificateByDomain(ctx context.Context, domain string) (Certificate, error)
	// CertificateByServer returns storeerr.ErrNotFound when serverID is empty
	// or no row is linked to it.
	CertificateByServer(ctx context.Context, serverID string) (Certificate, error)
	// Certificates lists every managed certificate, ordered by domain. Always
	// non-nil, empty when none exist.
	Certificates(ctx context.Context) ([]Certificate, error)
	// DeleteCertificate removes the row for domain, if any (a missing row is a
	// benign no-op, mirroring the store's other delete methods).
	DeleteCertificate(ctx context.Context, domain string) error
}

// RuntimeSpec is a mapping's agent-managed launch spec (agent_runtime_specs,
// migration 65): 1:1 per ModelMapping (MappingID is unique), owned by the
// agent-runtime-manager feature. It tells a server's ServerAgent how to
// launch, health-check, and (un)load the model process backing that mapping.
type RuntimeSpec struct {
	ID        string
	MappingID string
	Enabled   bool
	Binary    string
	// Args is an opaque JSON array string (the netbird_group_ids pattern):
	// the store never parses it, just round-trips it.
	Args string
	// Env is an opaque JSON object string; values are ${AGENT_ENV:NAME}
	// placeholders resolved by the agent from its own environment — never
	// secrets at rest here.
	Env                         string
	WorkDir                     string
	ListenPort                  int // 0 = agent picks a free loopback port
	HealthPath                  string
	HealthTimeoutSeconds        int
	StartupTimeoutSeconds       int
	IdleTimeoutSeconds          int // 0 = never unload
	AdmissionWaitTimeoutSeconds int // 0 = wait until the client disconnects
	Pinned                      bool
	AdminState                  string // "" | "force_running" | "force_stopped"
	VRAMLocked                  bool
	// SetVisibleDevices asks the agent to CONSTRAIN the child to the cards in
	// this spec's GPU rows, by setting the vendor-appropriate visibility
	// variable (CUDA_VISIBLE_DEVICES on NVIDIA, ROCR_VISIBLE_DEVICES on AMD;
	// nothing on Apple or a host with no recognised GPU stack) from those
	// HOST indices. Off, the GPU rows remain a declaration that drives
	// admission arithmetic and the VRAM measurement mapping while nothing
	// stops the process from landing on a different card.
	//
	// The gateway is hardware-agnostic and never resolves the variable name;
	// it only carries the flag. The two invalid combinations it DOES refuse
	// at save time (on with no GPU rows; on together with a hand-set
	// visibility variable in Env) live in portal.PutRuntimeSpec, and the
	// agent refuses them again at launch — see that method for why both.
	SetVisibleDevices bool
	// VisibleDevicesMode selects HOW SetVisibleDevices is enforced: "env"
	// (inject the vendor visibility variable, today's behavior) or "args" (the
	// agent expands a ${..._DEVICES} placeholder in Args and injects no env
	// var). Only meaningful when SetVisibleDevices is on; default "env".
	VisibleDevicesMode VisibleDevicesMode
	// APITokenMode is "app"|"set"|"random"|"off": where the upstream API token
	// for this mapping's child comes from. Default "app" (reuse the app token).
	APITokenMode string
	// APIToken is the SEALED per-spec token (SealSecret envelope), used only by
	// "set"/"random". Routing NEVER decrypts it — the edge does.
	APIToken string
	// APITokenHeaderSource is "app"|"custom": inherit the app's transmission
	// header or use APITokenHeader below.
	APITokenHeaderSource string
	// APITokenHeader is the custom transmission header (source=="custom" only);
	// empty ⇒ Authorization: Bearer.
	APITokenHeader string
	// Type is the explicit runtime-server kind ("" | "vllm" | "llama_cpp" |
	// "tgi" | "ollama" | "custom"); "" (default, every pre-feature row)
	// auto-detects from Binary. Selects which per-kind metrics/context-probe
	// conventions the agent applies.
	Type string
	// MetricsPath is the path on the child's listen port serving its metrics
	// endpoint (e.g. a Prometheus-style /metrics for vllm); "" = none/the
	// default for Type.
	MetricsPath string
	// ContextProbePath is the path on the child's listen port the agent
	// probes to discover the model's actual context window; "" = none/the
	// default for Type.
	ContextProbePath string
	// APIFlavors / ResponsesMode / MessagesMode are the per-spec snapshot of the
	// API-variant capability + the two coding-agent endpoint modes (design
	// 2026-09-03). For a server_agent mapping the RESOLVED spec is the sole
	// authority for its model's flavors + modes; the parent application's values
	// are only the create-time template and the no-spec fallback. Gateway-side
	// only — never added to AgentRuntimeSpecDTO or the agent wire type.
	APIFlavors    []string
	ResponsesMode EndpointMode
	MessagesMode  EndpointMode
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// RuntimeSpecGPU is one per-GPU VRAM demand row for a RuntimeSpec
// (agent_runtime_spec_gpus, migration 65), keyed by (SpecID, GPUIndex).
// VRAMEstimateMB is operator-owned (the portal writes it); VRAMMeasuredMB is
// agent-owned and written ONLY by UpdateRuntimeSpecGPUMeasured — the two
// never clobber each other.
type RuntimeSpecGPU struct {
	SpecID   string
	GPUIndex int
	// Position is the operator-chosen order of this GPU within its spec
	// (0-based, dense per spec). It is the sole ordering contract: both stores
	// read the rows `order by position`, and the DTO/wire layers keep the
	// resulting ARRAY order (no position field crosses the wire). The portal's
	// putRuntimeSpec sets Position = the index in the received req.GPUs array;
	// SetRuntimeSpecGPUs persists it verbatim (it does not renumber).
	Position       int
	VRAMEstimateMB int
	VRAMMeasuredMB int
}

// CoResidencyRule is one ALLOWED pair of model mappings that may be loaded on
// the same AI server at the same time (agent_coresidency_rules, migration
// 65, composite primary key (application_id, mapping_a_id, mapping_b_id), all
// three columns FK on delete cascade). A row's PRESENCE is the only
// co-residency signal — there is no `allowed` column, so "not co-resident" is
// the structural default and the table holds nothing but allowed pairs.
// Canonical ordering (MappingAID < MappingBID, so every unordered pair has
// exactly one row) is validated by the caller (the portal, Task 6): the
// store stays a dumb pair table and never sorts, rejects, or rewrites a pair.
type CoResidencyRule struct {
	ApplicationID string
	MappingAID    string // canonical: MappingAID < MappingBID, enforced by the caller (portal)
	MappingBID    string
	CreatedAt     time.Time
}

// ServerGPUBudget is a per-GPU VRAM budget for co-residency admission math
// (ai_server_gpu_budgets, migration 66, Task 3), keyed by (ServerID,
// GPUIndex): how much VRAM (MB) the operator has allotted to agent-managed
// runtimes on that GPU. BudgetMB 0 means "no budget for this GPU" =
// unconstrained (also true for a GPU with no row at all) — which is why
// SetServerGPUBudgets accepts 0 and rejects only negatives. This meaning is
// consumed on the far side of the wire by the agent's admission policy:
// runtime.Admit / PolicySnapshot.Budgets in
// server-agent/internal/runtime/policy.go skips any index whose budget is
// <= 0 precisely to honour it. The two comments are a matched pair —
// changing what 0 means here without changing it there gives every model on
// a zero-budget GPU a permanent not_permitted, which is exactly the bug that
// pairing them documents. ExpectedUUID /
// ExpectedName are a purely descriptive drift detector: snapshotted when the
// row is created (by the portal, Task 6) and later compared against live
// telemetry to WARN that a card was renumbered — they never block anything;
// the store just persists them.
type ServerGPUBudget struct {
	ServerID     string
	GPUIndex     int
	BudgetMB     int
	ExpectedUUID string
	ExpectedName string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ServerRuntimeReport is the latest agent-managed runtime configuration
// reported UPWARD by a server's ServerAgent when it is configured from a
// local file on the AI server instead of from the gateway
// (server_runtime_reports, migration 67). 1:1 per server, upsert-overwrite —
// shaped exactly like ServerHardware. ReportJSON is a validated canonical
// JSON blob, opaque to the store: the gateway's ingest layer (a later task)
// parses, sanitizes, and redacts environment-variable values before the
// report ever reaches here. The portal displays it read-only (a later
// task); the store never parses, validates, or inspects it.
type ServerRuntimeReport struct {
	ServerID string
	// CollectedAt is when the agent gathered the report; UpdatedAt is when
	// the gateway wrote it. Both are supplied by the caller.
	CollectedAt time.Time
	ReportJSON  string
	UpdatedAt   time.Time
}

// RuntimeStore is the agent-runtime-manager persistence surface: per-mapping
// launch specs and their per-GPU VRAM demand rows, the pairwise co-residency
// matrix (Task 2), per-GPU VRAM budgets (Task 3), and file-mode runtime
// reports (Task 4) — all on tables created by migration 65
// (specs/GPUs/co-residency), migration 66 (GPU budgets + the ai_servers
// runtime columns), and migration 67 (runtime reports).
type RuntimeStore interface {
	// UpsertRuntimeSpec inserts or replaces the spec for spec.MappingID (1
	// spec per mapping — the unique key is mapping_id, not id): a fresh
	// insert uses spec.CreatedAt, an overwrite preserves the existing
	// created_at. A missing MappingID is ErrNotFound (FK violation).
	UpsertRuntimeSpec(ctx context.Context, spec RuntimeSpec) error
	// RuntimeSpecByMapping returns the spec for mappingID; ok is false when
	// no spec has been created for that mapping yet (not an error).
	RuntimeSpecByMapping(ctx context.Context, mappingID string) (RuntimeSpec, bool, error)
	// RuntimeSpecByID returns the spec for id, keyed by its own primary key
	// rather than its owning mapping (RuntimeSpecByMapping's key). Added for
	// the telemetry VRAM write-back path (agent-runtime-manager Task 9): a
	// runtime sample carries only spec_id, and resolving whether the operator
	// has pinned vram_locked requires a point read by that id. ok is false
	// when no spec has that id (e.g. deleted out from under an in-flight
	// sample) -- not an error, mirroring RuntimeSpecByMapping's absent-read
	// contract.
	RuntimeSpecByID(ctx context.Context, id string) (RuntimeSpec, bool, error)
	// RuntimeSpecsByApplication lists every spec belonging to a mapping of
	// appID, ordered by spec id. Always non-nil, empty when none.
	RuntimeSpecsByApplication(ctx context.Context, appID string) ([]RuntimeSpec, error)
	// DeleteRuntimeSpec removes the spec by id, cascading its GPU rows. An
	// unknown id is ErrNotFound.
	DeleteRuntimeSpec(ctx context.Context, id string) error
	// SetRuntimeSpecGPUs atomically REPLACES the whole set of per-GPU VRAM
	// rows for specID (delete-then-insert in one transaction, mirroring
	// SetGroupMembers). An empty gpus clears the set. specID must exist
	// (ErrNotFound otherwise).
	SetRuntimeSpecGPUs(ctx context.Context, specID string, gpus []RuntimeSpecGPU) error
	// RuntimeSpecGPUs lists specID's per-GPU VRAM rows, ordered by position
	// (the operator order). Always non-nil, empty when none.
	RuntimeSpecGPUs(ctx context.Context, specID string) ([]RuntimeSpecGPU, error)
	// UpdateRuntimeSpecGPUMeasured writes back one agent measurement; ErrNotFound
	// when the (spec,gpu) row does not exist. Callers skip specs with VRAMLocked.
	UpdateRuntimeSpecGPUMeasured(ctx context.Context, specID string, gpuIndex int, measuredMB int) error
	// SetCoResidencyRules atomically REPLACES the whole set of allowed
	// co-residency pairs for appID (delete-then-insert in one transaction,
	// mirroring SetRuntimeSpecGPUs/SetGroupMembers). An empty/nil rules
	// clears the set. appID must exist (ErrNotFound, checked inside the
	// transaction before the delete); a rule naming a mapping id that does
	// not exist is also ErrNotFound (FK violation). The store does NOT
	// enforce or rewrite the MappingAID < MappingBID canonical ordering —
	// that validation belongs to the portal (Task 6).
	SetCoResidencyRules(ctx context.Context, appID string, rules []CoResidencyRule) error
	// CoResidencyRulesByApplication lists appID's allowed co-residency
	// pairs, ordered by mapping_a_id then mapping_b_id. Always non-nil,
	// empty when none.
	CoResidencyRulesByApplication(ctx context.Context, appID string) ([]CoResidencyRule, error)
	// SetServerGPUBudgets atomically REPLACES the whole set of per-GPU VRAM
	// budgets for serverID (delete-then-insert in one transaction, mirroring
	// SetRuntimeSpecGPUs/SetCoResidencyRules above). An empty/nil budgets
	// clears the set. serverID must exist (ErrNotFound, checked inside the
	// transaction before the delete).
	SetServerGPUBudgets(ctx context.Context, serverID string, budgets []ServerGPUBudget) error
	// ServerGPUBudgets lists serverID's per-GPU VRAM budgets, ordered by GPU
	// index. Always non-nil, empty when none.
	ServerGPUBudgets(ctx context.Context, serverID string) ([]ServerGPUBudget, error)
	// UpsertServerRuntimeReport stores the latest file-mode runtime report
	// for its server (1:1, upsert-overwrite — mirrors UpsertServerHardware).
	// An unknown ServerID is ErrNotFound (FK violation).
	UpsertServerRuntimeReport(ctx context.Context, report ServerRuntimeReport) error
	// ServerRuntimeReportByServer returns the latest runtime report for
	// serverID; ok is false when no report has ever been stored for that
	// server (not an error) — mirrors ServerHardwareByServer.
	ServerRuntimeReportByServer(ctx context.Context, serverID string) (ServerRuntimeReport, bool, error)
}

// Store is the full routing persistence surface: the composition of every
// role-scoped sub-interface above, grouped by concern. *MemoryStore (this
// package) and *store.SQLStore implement Store by implementing each
// sub-interface; a consumer that only needs a narrow slice (e.g. the
// routing package's own resolverStore in resolver.go) should depend on that
// narrower interface instead of the full Store — both MemoryStore and
// SQLStore satisfy it structurally, with no change at the call site.
type Store interface {
	ServerStore
	NetbirdStateStore
	AgentTokenStore
	TelemetryStore
	SampleStore
	BenchmarkStore
	AffinityStore
	ApplicationStore
	MappingStore
	ModelGroupStore
	ServiceStore
	ResourceGroupStore
	LimitsStore
	CertificateStore
	RuntimeStore
}

// applicationHasAPIFlavor reports whether the application serves the flavor.
func applicationHasAPIFlavor(app Application, apiFlavor string) bool {
	for _, candidate := range app.APIFlavors {
		if candidate == apiFlavor {
			return true
		}
	}
	return false
}

// applicationServesEndpoint refines applicationHasAPIFlavor with the per-endpoint
// mode for the two coding-agent endpoints. For an openai_responses request an
// ORDINARY app must carry the openai flavor AND not have ResponsesMode==disabled;
// anthropic_messages is the anthropic/MessagesMode analogue. A server_agent app is
// gated on the coarse flavor only here — its authoritative per-model mode lives on
// the resolved RuntimeSpec and is enforced at dispatch (native_passthrough), so
// candidacy must not exclude it on the app-level fallback. Any other flavor value
// (openai_chat_completions, or an already-coarse openai/anthropic) uses the plain
// flavor gate, so plain chat-completions is never removed by a disabled Codex
// endpoint.
func applicationServesEndpoint(app Application, fineFlavor string) bool {
	switch fineFlavor {
	case "openai_responses":
		if !applicationHasAPIFlavor(app, APIFlavorOpenAI) {
			return false
		}
		return app.Type == ProviderServerAgent || app.ResponsesMode != EndpointModeDisabled
	case "anthropic_messages":
		if !applicationHasAPIFlavor(app, APIFlavorAnthropic) {
			return false
		}
		return app.Type == ProviderServerAgent || app.MessagesMode != EndpointModeDisabled
	default:
		return applicationHasAPIFlavor(app, NormalizeAPIFlavor(fineFlavor))
	}
}
