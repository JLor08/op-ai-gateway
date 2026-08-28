// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/account"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/certissue"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/mail"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/tracing"
	"op-ai-gateway/internal/usage"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

var requestIDCounter uint64

const maxJSONBodyBytes int64 = 1 << 20

// defaultCaptureMaxBytes bounds each captured body when ServerDeps.CaptureMaxBytes
// is unset (0). Mirrors config default OP_AI_GATEWAY_CAPTURE_MAX_BYTES = 1 MB.
const defaultCaptureMaxBytes = 1 << 20

// msgUserNotFound is the shared 404 message below; the API *code* differs by
// call site (portal.user_not_found/admin.user_not_found/limit.user_not_found)
// and is intentionally NOT unified.
const msgUserNotFound = "user not found"

// healthzPath is the unauthenticated liveness path registered on both muxes
// and special-cased (bypasses the access log/tracing) below.
const healthzPath = "/healthz"

// CaptureStore persists an encrypted request/response capture blob (write side).
// It is optional and fail-closed: gateway.New must NOT auto-default it (unlike
// UsageEvents), so a nil Captures leaves the capture write path a no-op.
type CaptureStore interface {
	SaveCapture(ctx context.Context, capture store.Capture) error
}

// userLookup resolves a user id to a store.User for the internal trusted-
// loopback principal. Will be wired to the account/store lookup in a later
// task; a nil value disables the internal-auth branch (fail-closed).
type userLookup interface {
	UserByID(ctx context.Context, id string) (store.User, error)
}

type ServerDeps struct {
	Tokens      auth.BearerStore
	Usage       usage.Store
	UsageEvents *usage.Broker
	Captures    CaptureStore
	Provider    provider.Client
	Routes      routing.Store
	Resolver    *routing.Resolver
	// LastUsedModelWriter persists the effective model of a token's last
	// SUCCESSFULLY routed request (see Server.resolveTarget, the single seam
	// that calls it). Wired in cmd/gateway/main.go to the driver's
	// SetTokenLastUsedModel (store.SQLStore or portal.MemoryDirectory). Nil
	// disables the marker entirely — resolveTarget is nil-safe.
	LastUsedModelWriter func(ctx context.Context, tokenID, model string) error
	Portal              portal.API
	Account             account.API
	CookieSecure        bool
	SessionMaxAge       time.Duration
	PublicURL           string
	StreamIdleTimeout   time.Duration
	// SwapProtectWindow is the recency window for routing swap-protection: a
	// server that served a request within this window is protected from eviction
	// by a request for a different model. <= 0 defaults to 30s in New.
	SwapProtectWindow time.Duration
	// SessionReservationWindow is how long a sticky session holds a reserved concurrency
	// slot on its server for the CP3 capacity cap. <= 0 disables reservation (effectiveCap
	// == max_concurrency).
	SessionReservationWindow time.Duration
	// Capacity-benchmark tuning (consumed by the CP2 capacity ramp engine).
	// CapacityVRAMSafetyMarginPercent is the percent of total VRAM kept free (the
	// ramp stops before crossing it, so it never OOMs); CapacityMaxConcurrency is
	// the ceiling the ramp probes up to; CapacitySettleSeconds is the settle time
	// between ramp steps. Each <= 0 defaults in New (10 / 64 / 5).
	CapacityVRAMSafetyMarginPercent int
	CapacityMaxConcurrency          int
	CapacitySettleSeconds           int
	// AdmissionQueueMaxDepth bounds how many unpinned requests can park in the CP4
	// admission queue at once; the (maxDepth+1)-th waiter is rejected with a 503.
	// <= 0 defaults to 128 in newAdmissionQueue.
	AdmissionQueueMaxDepth int
	// NetbirdSyncInterval is the cadence of the background NetBird peer-sync loop
	// (defined here; consumed by the sync loop wired in a later task).
	NetbirdSyncInterval time.Duration
	// SelfBaseURL is the loopback base URL (e.g. "http://127.0.0.1:8080") the
	// gateway uses to call itself for background chat runs. See
	// loopbackBaseFromAddr in chat_runs.go.
	SelfBaseURL     string
	Cipher          *capture.Cipher
	CaptureMaxBytes int
	CaptureEnabled  func() bool
	CaptureOverride func() bool
	// AppHealth is the per-application reachability registry, populated by the
	// background app-health probe loop and consumed by Phase 3 (routing + model
	// offering). gateway.New copies it; a nil value gets a fresh registry.
	AppHealth *AppHealthRegistry
	// InternalAuthSecret gates the internal trusted-loopback auth branch (see
	// authenticateWeb in auth.go). Empty disables it (fail-closed). Will be
	// wired in a later task.
	InternalAuthSecret string
	// Users resolves the user id carried on internal-auth requests to a
	// store.User so the loopback principal matches the browser cookie path.
	// Will be wired in a later task; nil disables the branch (fail-closed).
	Users userLookup
	// ChatRuns is the in-memory background chat-run registry. The executor
	// (chat_runs.go) registers runs here and evicts them after completion. Wired
	// into cmd/gateway/main.go in a later task.
	ChatRuns *chatRunRegistry
	// ServerPerf is the in-memory per-server telemetry-sample ring + live SSE
	// fan-out (server_perf.go). gateway.New defaults a nil value to a fresh
	// registry so every Server carries a usable one (the type is also nil-safe).
	// May be set explicitly in cmd/gateway/main.go in a later task.
	ServerPerf *serverPerfRegistry
	// LoadedModels tracks which upstream models are currently loaded per
	// application (loaded_models.go). The SAME instance MUST be shared with the
	// app-health probe loop (which writes the gateway-poll source) so this
	// server's agent-telemetry handler (which writes the agent-report source) and
	// the portal read side all see one registry. gateway.New defaults a nil value
	// to a fresh registry; the type is also nil-safe.
	LoadedModels *LoadedModelRegistry
	// AgentPresence tracks per-server ServerAgent telemetry recency
	// (agent_presence.go); nil-safe. Shared with the app-health probe loop.
	AgentPresence *AgentPresenceRegistry
	// AgentCertReports records what each ServerAgent says it has INSTALLED
	// (agent_cert_report.go); nil-safe. ONE shared registry: the agent-telemetry
	// ingest path writes it, the portal reads it (certificate "installed" column +
	// the CA-rotation propagation brake), and the app-health loop prunes it.
	AgentCertReports *AgentCertReportRegistry
	// AgentTransport records the hop (TLS vs. plaintext) of each successful
	// authenticated agent request on the MESH listener (agent_transport.go);
	// nil-safe. ONE shared registry: authenticateAgent stamps it (only when the
	// mesh listener context marker is set), the portal reads it (per-server
	// transport column + the mesh-gate arming precondition), and the app-health
	// loop prunes it.
	AgentTransport *AgentTransportRegistry
	// AgentProxyStatus records what each ServerAgent reports as ACTUALLY
	// running on its local TLS-terminating proxy routes (Certificates P4 Task 9,
	// agent_proxy_status.go); nil-safe. ONE shared registry: the agent-telemetry
	// ingest path writes it from the sample's proxy_routes, the Task 10 switch
	// reconcile reads it to gate a public-listener flip, and the app-health loop
	// prunes it to live servers.
	AgentProxyStatus *AgentProxyStatusRegistry
	// AgentStreams tracks open agent WebSocket connections per server so a
	// certificate reconcile pass can push a cert_update doorbell to every
	// currently-open connection for the server whose certificate just changed
	// (agent_stream_registry.go); nil-safe. gateway.New defaults a nil value to
	// a fresh registry so a bare test Server still carries a usable one. The
	// SAME instance MUST be handed to portal.ServiceDeps.OnCertificateIssued
	// (via a closure calling its NotifyCertUpdate) so the side that registers
	// connections (this field) and the side that pushes to them (the portal
	// hook) agree on which connections exist.
	AgentStreams *AgentStreamRegistry
	// AgentFeatures tracks each connected ServerAgent's last-declared
	// telemetry-capabilities feature set (agent_ingest.go's capabilities
	// parse writes it; Server.PushRuntimeConfig, agent_runtime.go, reads it
	// to gate the runtime_config WS push on the agent actually having
	// declared support -- runtime_registry.go); nil-safe. gateway.New
	// defaults a nil value to a fresh registry so a bare test Server still
	// carries a usable one, mirroring AgentStreams above. ONE shared
	// registry in production: cmd/gateway constructs it
	// (gateway.NewAgentFeaturesRegistry), hands it here, and hands the SAME
	// instance to the app-health loop's end-of-cycle pruning bundle -- if the
	// two diverged, the loop would prune an instance nothing writes to while
	// this one grew for every server ever deleted.
	AgentFeatures *agentFeaturesRegistry
	// RuntimeStatus holds per-server agent-managed-runtime status the
	// gateway gates its own behavior on (runtime_registry.go); nil-safe.
	// Task 8 populates only the file-mode flag Server.PushRuntimeConfig
	// consults; a later task extends the SAME type with the
	// snapshot+subscribe status stream. gateway.New defaults a nil value to
	// a fresh registry.
	RuntimeStatus *runtimeStatusRegistry
	// RuntimeLogs fans live managed-process output out to open portal log
	// views and, from the set of those views, derives what each agent is
	// asked to stream (runtime_logs.go); nil-safe. gateway.New defaults a nil
	// value to a fresh registry and wires its notify hook to AgentStreams.
	RuntimeLogs *runtimeLogRegistry
	// SetRuntimeConfigChangedHook, when non-nil, is called ONCE by
	// cmd/gateway's buildGatewayServer immediately after gateway.New returns,
	// with the just-built Server's PushRuntimeConfig bound as the argument --
	// i.e. it is portal.Service.SetRuntimeConfigChangedHook itself, handed
	// forward through ServerDeps by cmd/gateway's buildRuntime (which already
	// holds the concrete *portal.Service in scope where it builds the Portal
	// field above, before it is wrapped for the ServerDeps.Portal interface
	// value). This field exists ONLY to carry that setter across the
	// construction-order gap: the portal Service must exist before
	// ServerDeps.Portal can be built, but the callback it needs
	// (Server.PushRuntimeConfig) is a method on the Server, which does not
	// exist until gateway.New(deps) returns -- so neither side can wire the
	// other in directly at its own construction time. Server itself never
	// stores or reads this field, and it plays no role in New's construction
	// of *Server; it is read directly off the ServerDeps value still in
	// scope in buildGatewayServer, purely as a wiring conduit. nil is the
	// correct default for any Server built without cmd/gateway (e.g. a bare
	// test Server) -- there is nothing to wire in that case.
	SetRuntimeConfigChangedHook func(func(serverID string))
	// OnAgentReactivated, when set, is invoked with the server id when that server's
	// ServerAgent transitions inactive->active (see AgentPresenceRegistry.
	// ReportReactivated), computed against the server's EFFECTIVE presence window.
	// The gateway fires it from the telemetry ingest path, so the callback MUST be
	// non-blocking (wrap its work in a goroutine). nil = no trigger (the periodic
	// NetBird + app-health loops are the backstop). Wired in cmd/gateway/main.go.
	OnAgentReactivated func(serverID string)
	// Benchmarks is the in-memory per-server benchmark-run registry
	// (benchmark.go). gateway.New defaults a nil value to a fresh registry and
	// wires it as the resolver's ServerBusyChecker so a running benchmark
	// excludes the server from new routing. The type is nil-safe.
	Benchmarks *BenchmarkRegistry
	// Groups is the store-backed model-group registry (group_registry.go). The
	// SAME instance MUST be shared with the portal Service (as its GroupCache) so a
	// group / model-setting CRUD refresh updates the very snapshot the resolver
	// reads on the hot path. gateway.New defaults a nil value to a fresh registry
	// and wires it as the resolver's GroupResolver; the type is nil-safe.
	Groups *GroupRegistry
	// NewMailer builds a Mailer from a mail.Config at send time (so config edits
	// take effect without a restart). nil installs the real net/smtp mailer;
	// tests inject a recording/loopback factory. See smtp.go.
	NewMailer func(mail.Config) Mailer
	// Logs is the in-memory log ring + SSE fan-out + runtime level backing the
	// system "Logs" portal view (logs_endpoints.go). Wired in cmd/gateway/main.go
	// alongside slog.SetDefault/log.SetOutput; nil-safe (a bare test Server leaves
	// it unset and the endpoints nil-guard).
	Logs *logbuffer.Buffer
	// Tracing is the OTel provider handle (tracing_endpoints.go). nil-safe: a nil
	// provider makes the tracing endpoint report disabled and the toggle a no-op.
	Tracing *tracing.Provider
	// TracingOTLPSet reports whether an OTLP endpoint was configured at startup
	// (cosmetic status field). Set from cfg in main.go (a later task).
	TracingOTLPSet bool
	// EnergySettleSeconds/EnergyBackfillWindow/EnergyIdleWindowSeconds tune the
	// energy reconciler (energy_reconciler.go) and its idle tracker
	// (energy_idle.go). Each <=0 defaults in New (10s settle / 168h backfill /
	// 3600s idle window).
	EnergySettleSeconds int
	// EnergyBackfillWindow bounds how far back a persistently-unpriceable usage
	// event is retried; cmd/gateway sets it to the same window as the telemetry
	// retention period (a usage event outside it has no telemetry left anyway).
	EnergyBackfillWindow    time.Duration
	EnergyIdleWindowSeconds int
	// AgentBinaryDir is the directory containing the ServerAgent release
	// manifest.json + platform binaries served by the agent-binary download
	// endpoints (wired in a later task). Empty disables the feature the same way
	// an unreadable manifest at a non-empty path does (see loadAgentManifest).
	AgentBinaryDir string
	// Limiter is the principal (service/user) rate/quota/budget admission gate
	// (principal_limits.go, design spec Phase 2 §6). Consulted at the same
	// pre-Resolve choke point as the service-account model allowlist
	// (admitPrincipal, called exactly once per request from inferencePreflight
	// -- the single gate shared by the three inference handlers and
	// tryProxyNative) and updated post-response from recordUsage. gateway.New
	// defaults a nil value to a fresh limiter backed by deps.Routes, so a bare
	// test Server (or one built before this field existed) still carries a
	// usable, nil-safe one; cmd/gateway builds ONE instance per driver and
	// passes it here explicitly, mirroring Benchmarks/Groups/LoadedModels.
	Limiter *PrincipalLimiter
	// ACMEChallenges is the process-local ACME HTTP-01 challenge store backing
	// GET /.well-known/acme-challenge/{token} on the PUBLIC listener
	// (certificates.go). A nil value makes the challenge route always answer
	// 404 (fail-closed) -- it never panics. Wired to the SAME instance the
	// certissue.ACMEClient uses in a later task.
	ACMEChallenges certissue.ChallengeStore
	// CertEdgeRequireHTTPSDisable is plan B's plaintext-refusal KILL SWITCH
	// (config.Config.CertEdgeRequireHTTPSDisable, from
	// OP_AI_GATEWAY_CERT_EDGE_REQUIRE_HTTPS_DISABLE). It is set here, on
	// ServerDeps, rather than threaded through portal.ServiceDeps, because
	// its consumer -- the plaintext-refusal gate in the serveWith path (a
	// later task) -- lives entirely in this package and must be able to
	// check it as a plain in-process bool on *Server, with NO dependency on
	// s.Portal, the settings store, or anything else that gate might itself
	// be blocking. When true it overrides the stored cert_edge_require_https
	// setting and the gate never refuses plaintext. Default false: an unset
	// kill switch is not engaged.
	CertEdgeRequireHTTPSDisable bool
	// CertMeshRequireTLSDisable is P3's env-only KILL SWITCH for the mesh
	// listener's plaintext-refusal gate (config.Config.CertMeshRequireTLSDisable,
	// from OP_AI_GATEWAY_CERT_MESH_REQUIRE_TLS_DISABLE). When true the mesh gate
	// never refuses plaintext, regardless of the stored cert_mesh_require_tls; it
	// is the only recovery path for an operator who armed a fleet-wide lockout, so
	// like the edge kill switch it is a plain bool, never read through s.Portal.
	CertMeshRequireTLSDisable bool
}

type Server struct {
	Tokens      auth.BearerStore
	Usage       usage.Store
	UsageEvents *usage.Broker
	Captures    CaptureStore
	Provider    provider.Client
	Routes      routing.Store
	Resolver    *routing.Resolver
	// LastUsedModelWriter mirrors ServerDeps.LastUsedModelWriter — see its doc.
	LastUsedModelWriter func(ctx context.Context, tokenID, model string) error
	Portal              portal.API
	agentBinaryDir      string
	Account             account.API
	CookieSecure        bool
	SessionMaxAge       time.Duration
	PublicURL           string
	streamIdleTimeout   time.Duration
	selfBaseURL         string
	Cipher              *capture.Cipher
	captureMaxBytes     int
	CaptureEnabled      func() bool
	CaptureOverride     func() bool
	Active              *activeRegistry
	AppHealth           *AppHealthRegistry
	// capacity* tune the CP2 capacity ramp engine (set from ServerDeps in New,
	// each defaulted from a non-positive value).
	capacityVRAMMarginPct  int
	capacityMaxConcurrency int
	capacitySettleSeconds  int
	// capacitySettle is the derived settle duration (capacitySettleSeconds seconds)
	// the ramp waits for a fresh telemetry sample between levels. Set in New; a test
	// may override it directly for a sub-second settle (production is always seconds).
	capacitySettle time.Duration
	mux            *http.ServeMux
	// agentMux serves the NetBird agent listener (the second http.Server bound to
	// the gateway's NetBird IP): the UNGATED agent-telemetry route + /healthz. The
	// agent route on the main mux is gated (see routes) so netbird_only can reject
	// the public agent path at runtime; the agent listener always serves it.
	agentMux *http.ServeMux
	// baseCtx is the server shutdown context. handleAgentStream (agent_stream.go)
	// derives each open agent WebSocket's lifetime from it: a shutdown watcher sends
	// 1001 GoingAway when baseCtx is cancelled (SIGTERM), so http.Server.Shutdown
	// finishes promptly and the agent reconnects via the fast clean-close path.
	// Defaults to context.Background() in New; cmd/gateway replaces it via
	// SetBaseContext before serving.
	baseCtx context.Context
	// agentMu guards agentListenerActive/agentListenerAddr below: main() sets them
	// once before serving starts, but the upcoming live-rebind manager (Task 4)
	// updates them at runtime while request handlers (the netbird_only gate) and
	// the status endpoint read them concurrently.
	agentMu sync.RWMutex
	// agentListenerActive is true once the PLAINTEXT agent listener has been
	// successfully bound (in separate mode the dedicated TLS bind tracks its own
	// state in agentListenerTLS below); agentListenerAddr is that plaintext
	// bind's host:port for the status panel. In combined mode the single
	// sniffing bind writes both this plaintext slot and the TLS slot. The
	// public-listener agent reject gates on AgentListenerActive() which is true
	// when EITHER slot is up (the fail-safe: only a total absence of any agent
	// listener leaves the public path open regardless of netbird_only). Access
	// via AgentListenerActive/AgentListenerAddr/AgentListenerStates and
	// SetAgentListener/SetAgentPlainListener — never read/write directly.
	agentListenerActive bool
	agentListenerAddr   string
	agentListenerTLS    AgentListenerTLSState
	// agentDNSMu guards agentDNSVal/agentDNSExp: cachedGatewayPeerDNS (agent_binaries.go)
	// TTL-caches the gateway peer's NetBird DNS resolution (a live NetBird GetPeer call,
	// up to ~15s) so a portal agent-binaries list GET never blocks on it per request.
	//
	// Deliberately NOT on settingCache (ttlcache.go): cachedGatewayPeerDNS holds
	// this mutex ACROSS the resolve call so concurrent misses single-flight into
	// one NetBird round trip, whereas settingCache always releases its lock during
	// load (so a slow load never serialises concurrent readers behind it) --
	// moving this cache onto settingCache would let concurrent misses each fire
	// their own NetBird call instead of coalescing into one.
	agentDNSMu  sync.Mutex
	agentDNSVal string
	agentDNSExp time.Time
	// internalAuthSecret and users back the internal trusted-loopback auth
	// branch in authenticateWeb (auth.go). internalAuthSecret empty disables
	// the branch entirely (fail-closed).
	internalAuthSecret string
	users              userLookup
	// ChatRuns is the in-memory background chat-run registry (chat_runs.go).
	ChatRuns *chatRunRegistry
	// ServerPerf is the in-memory per-server telemetry ring + SSE fan-out
	// (server_perf.go); nil-safe.
	ServerPerf *serverPerfRegistry
	// LoadedModels tracks currently-loaded upstream models per app/server
	// (loaded_models.go); nil-safe. Shared with the app-health probe loop.
	LoadedModels *LoadedModelRegistry
	// AgentPresence tracks per-server ServerAgent telemetry recency
	// (agent_presence.go); nil-safe. Shared with the app-health probe loop.
	AgentPresence *AgentPresenceRegistry
	// AgentCertReports records what each ServerAgent says it has INSTALLED
	// (agent_cert_report.go); nil-safe. Shared with the portal + the app-health
	// probe loop (which prunes it).
	AgentCertReports *AgentCertReportRegistry
	// AgentTransport records the hop (TLS vs. plaintext) of each successful
	// authenticated agent request on the MESH listener (agent_transport.go);
	// nil-safe. Shared with the portal + the app-health probe loop (which prunes it).
	AgentTransport *AgentTransportRegistry
	// AgentProxyStatus records what each ServerAgent reports as ACTUALLY
	// running on its local TLS-terminating proxy routes (agent_proxy_status.go);
	// nil-safe. ingestTelemetrySample writes it; the app-health probe loop
	// prunes it.
	AgentProxyStatus *AgentProxyStatusRegistry
	// AgentStreams tracks open agent WebSocket connections per server
	// (agent_stream_registry.go); nil-safe. handleAgentStream registers and
	// deregisters each connection here; NotifyCertUpdate is the ONLY way
	// anything pushes a frame to an agent.
	AgentStreams *AgentStreamRegistry
	// AgentFeatures tracks each connected ServerAgent's last-declared
	// feature set (runtime_registry.go); nil-safe. Written by
	// ingestTelemetrySample, read by PushRuntimeConfig.
	AgentFeatures *agentFeaturesRegistry
	// RuntimeStatus holds per-server agent-managed-runtime status
	// (runtime_registry.go); nil-safe. PushRuntimeConfig consults its
	// file-mode flag.
	RuntimeStatus *runtimeStatusRegistry
	// RuntimeLogs relays live managed-process output to portal log views
	// (runtime_logs.go); nil-safe. Volatile only -- nothing on that path is
	// ever persisted.
	RuntimeLogs *runtimeLogRegistry
	// onAgentReactivated fires on an inactive->active ServerAgent edge; see
	// ServerDeps.OnAgentReactivated. nil-safe (unset -> no trigger).
	onAgentReactivated func(serverID string)
	// agentPresenceDefault memoizes the system-wide agent-presence-timeout default
	// (a system_settings read) for agentPresenceDefaultTTL (settingCache, TTL-only
	// mode -- ttlcache.go) so the per-telemetry-ingest reactivation-edge check does
	// not query settings on every agent report. Only the fleet-wide default is
	// cached; the per-server override stays exact (read from the server record). A
	// system-default change takes effect within the TTL.
	agentPresenceDefault settingCache[int]
	// Benchmarks is the in-memory per-server benchmark-run registry
	// (benchmark.go); nil-safe. Also installed as the resolver's
	// ServerBusyChecker so a running benchmark excludes the server from routing.
	Benchmarks *BenchmarkRegistry
	// Groups is the store-backed model-group registry (group_registry.go); nil-safe.
	// Installed as the resolver's GroupResolver and shared with the portal Service as
	// its GroupCache (one instance, so a CRUD refresh updates what the resolver reads).
	Groups *GroupRegistry
	// newMailer builds a Mailer per send from the current saved config.
	newMailer func(mail.Config) Mailer
	// Logs is the in-memory log ring + SSE fan-out + runtime level (logbuffer);
	// nil-safe. Backs the system Logs endpoints (logs_endpoints.go).
	Logs *logbuffer.Buffer
	// Tracing is the OTel provider handle (tracing_endpoints.go); nil-safe. Backs
	// the system Tracing toggle endpoint.
	Tracing *tracing.Provider
	// tracingOTLPSet reports whether an OTLP endpoint was configured at startup
	// (cosmetic status field only).
	tracingOTLPSet bool
	// EnergyIdle tracks, per server, a rolling minimum of observed power draw
	// (energy_idle.go) -- an emergent idle-wattage estimate used by the energy
	// reconciler (energy_reconciler.go) when a server has no operator-set
	// IdleWatts override. Fed by ingestTelemetrySample on every agent
	// telemetry report; nil-safe (a nil tracker's Idle always returns 0 =
	// unknown), so a bare test Server that never touches this field is safe.
	EnergyIdle *idleTracker
	// energySettleDelay/energyBackfillWindow tune the energy reconciler
	// (energy_reconciler.go): settleDelay lets a request's telemetry samples
	// and concurrent siblings land before it is priced; backfillWindow bounds
	// how far back a persistently-unpriceable event is retried. Both default
	// (<=0) inside reconcileEnergyOnce itself (10s / 168h), so a Server built
	// directly (bypassing New, e.g. in a test) still behaves sanely.
	energySettleDelay    time.Duration
	energyBackfillWindow time.Duration
	// energyPueDefault memoizes the system-wide energy_default_pue setting for
	// energyPueDefaultTTL (energy_reconciler.go; settingCache, TTL-only mode --
	// ttlcache.go), mirroring agentPresenceDefault above -- so the
	// per-telemetry-ingest idle-tracker Observe hook does not read system_settings
	// on every agent report.
	energyPueDefault settingCache[float64]
	// Limiter is the principal rate/quota/budget admission gate; see
	// ServerDeps.Limiter. Nil-safe (Admit/Record on a nil *PrincipalLimiter
	// always allow/no-op), so a bare &Server{} built without New still works.
	Limiter *PrincipalLimiter
	// acmeChallenges backs the public ACME HTTP-01 challenge route
	// (handleACMEChallenge, certificates.go); see ServerDeps.ACMEChallenges. A
	// nil value makes the route 404 rather than panic.
	acmeChallenges certissue.ChallengeStore
	// edgeScheme records whether encrypted (vs plaintext) traffic is actually
	// arriving from the fronting reverse proxy, via the $scheme hop header nginx
	// sets in every one of its header-setting blocks (edge_scheme.go). This is
	// the precondition the plaintext-refusal switch needs before it can be armed:
	// with an upstream proxy, this is the only way to know whether that proxy
	// speaks TLS to the gateway. Written by noteEdgeScheme on every public
	// request (serveWith) and read by edgeGateVerdict/ArmEdgeRequireHTTPS.
	// Nil-safe (its Note/Seen are no-ops/zero-value on a nil receiver), so a bare
	// &Server{} built without New still works, mirroring Benchmarks/Limiter above.
	edgeScheme *edgeSchemeTracker
	// certEdgeRequireHTTPSDisable is a straight copy of ServerDeps.
	// CertEdgeRequireHTTPSDisable -- see that field's doc comment for why it
	// is a plain bool here rather than something read through s.Portal.
	certEdgeRequireHTTPSDisable bool
	// edgeSwitch is the TTL cache (settingCache, invalidatable mode -- ttlcache.go)
	// edgeRequireHTTPSOn (edge_scheme.go) uses so the plaintext gate does not issue
	// a system_settings store read on EVERY public request. Invalidated explicitly
	// by handleSystemSettings after a PUT that carried the switch, via
	// edgeSwitch.Invalidate() (invalidateEdgeRequireHTTPSCache).
	//
	// settingCache's generation counter is bumped by every Invalidate() and
	// captured by a reader before it leaves the lock, so a read that STARTED
	// before a disarming PUT cannot write its now-stale value back with a fresh
	// TTL afterwards -- see edgeRequireHTTPSOn.
	edgeSwitch settingCache[bool]
	// edgeWarn is the per-path throttle (warnThrottle -- ttlcache.go) for the
	// plaintext gate's refusal log line (shouldLogEdgeGateRefusal, edge_scheme.go).
	// One Warn per refused REQUEST would let a retrying client evict the
	// 2000-entry log ring the locked-out operator needs to read.
	edgeWarn warnThrottle

	// certMeshRequireTLSDisable is a straight copy of ServerDeps.
	// CertMeshRequireTLSDisable -- P3's env-only kill switch for the mesh
	// listener's plaintext-refusal gate (agent_mesh_gate.go).
	certMeshRequireTLSDisable bool
	// meshSwitch is the TTL cache (settingCache, invalidatable mode -- ttlcache.go)
	// meshRequireTLSOn uses so the mesh gate does not read system_settings on EVERY
	// mesh request; shares the same generation-based disarm-race guard as
	// edgeSwitch above.
	meshSwitch settingCache[bool]
	// meshWarn is the per-path throttle (warnThrottle -- ttlcache.go) for the mesh
	// gate's refusal log line (shouldLogMeshGateRefusal).
	meshWarn warnThrottle

	// sourcePeerIP is the TTL cache (settingCache, TTL-only mode via GetTTL --
	// ttlcache.go) cachedGatewayPeerIP (agent_netbird_gate.go) uses for the
	// resolved gateway NetBird peer IP, so the netbird_only agent SOURCE check does
	// not call the live ResolveGatewayPeerIP (a NetBird management-API round trip)
	// on every agent-telemetry/stream request. Unlike the mesh/edge switches,
	// nothing ever calls Invalidate() on this one -- a short TTL alone bounds
	// staleness after an operator changes the selected gateway peer -- and that TTL
	// varies per call (GetTTL), shorter for an error-derived miss so a transient
	// NetBird blip self-heals fast.
	sourcePeerIP settingCache[string]
}

// portalProvisioningGate adapts portal.API's AllowedServerIDs onto the
// routing.ProvisioningGate seam (Resource Groups Phase 2, spec
// 2026-08-12-resource-groups-phase-2-provisioning) so the resolver can filter
// candidate servers by resource-group provisioning without importing
// internal/portal directly into internal/routing.
type portalProvisioningGate struct{ api portal.API }

func (g portalProvisioningGate) AllowedServerIDs(ctx context.Context, p auth.Token, ids []string) (map[string]bool, error) {
	return g.api.AllowedServerIDs(ctx, p, ids)
}

func New(deps ServerDeps) *Server {
	providerClient := deps.Provider
	if providerClient == nil {
		providerClient = provider.NewMock()
	}
	broker := deps.UsageEvents
	if broker == nil {
		broker = usage.NewBroker()
	}
	appHealth := deps.AppHealth
	if appHealth == nil {
		appHealth = NewAppHealthRegistry(broker)
	}
	resolver := deps.Resolver
	if resolver == nil && deps.Routes != nil {
		// The app-health registry gates routing on reachability; it satisfies
		// routing.ReachabilityChecker via its Reachable method (unknown → true,
		// lenient). A pre-built deps.Resolver keeps its own checker.
		resolver = routing.NewResolver(deps.Routes, nil, appHealth)
	}
	captureMaxBytes := deps.CaptureMaxBytes
	if captureMaxBytes <= 0 {
		captureMaxBytes = defaultCaptureMaxBytes
	}
	serverPerf := deps.ServerPerf
	if serverPerf == nil {
		serverPerf = NewServerPerfRegistry()
	}
	loadedModels := deps.LoadedModels
	if loadedModels == nil {
		loadedModels = NewLoadedModelRegistry()
	}
	agentPresence := deps.AgentPresence
	if agentPresence == nil {
		agentPresence = NewAgentPresenceRegistry(0)
	}
	agentCertReports := deps.AgentCertReports
	if agentCertReports == nil {
		agentCertReports = NewAgentCertReportRegistry()
	}
	agentTransport := deps.AgentTransport
	if agentTransport == nil {
		agentTransport = NewAgentTransportRegistry()
	}
	agentProxyStatus := deps.AgentProxyStatus
	if agentProxyStatus == nil {
		agentProxyStatus = NewAgentProxyStatusRegistry()
	}
	agentStreams := deps.AgentStreams
	if agentStreams == nil {
		agentStreams = NewAgentStreamRegistry()
	}
	agentFeatures := deps.AgentFeatures
	if agentFeatures == nil {
		agentFeatures = newAgentFeaturesRegistry()
	}
	runtimeStatus := deps.RuntimeStatus
	if runtimeStatus == nil {
		runtimeStatus = newRuntimeStatusRegistry()
	}
	runtimeLogs := deps.RuntimeLogs
	if runtimeLogs == nil {
		runtimeLogs = newRuntimeLogRegistry()
	}
	// The one place that holds both registries, which is why the "tell the
	// agent what to stream" hook is bound here rather than in either of them:
	// the log registry knows WHICH specs are being watched, the agent-stream
	// registry knows HOW to reach the agent, and neither should have to know
	// the other's type.
	runtimeLogs.setNotify(agentStreams.NotifyRuntimeLogWatch)
	benchmarks := deps.Benchmarks
	if benchmarks == nil {
		benchmarks = NewBenchmarkRegistry()
	}
	// edgeScheme is purely in-process, volatile state (two timestamps) with no
	// external dependency to inject, so -- unlike the registries above -- there
	// is no ServerDeps override for it; New always allocates a fresh one.
	edgeScheme := &edgeSchemeTracker{}
	// The model-group registry feeds the resolver's GroupResolver (group failover) and,
	// as the portal's GroupCache, is refreshed after every group / model-setting write.
	// A nil deps.Groups gets a fresh store-backed registry so a bare test Server still
	// carries a usable one (the type is nil-safe either way).
	groups := deps.Groups
	if groups == nil {
		groups = NewGroupRegistry(deps.Routes)
	}
	// limiter: a nil deps.Limiter gets a fresh instance backed by deps.Routes
	// (routing.Store structurally satisfies the limiter's narrow
	// PrincipalLimitStore via PrincipalLimits/UsageAggregateSince), so a bare
	// test Server still carries a usable, nil-safe limiter (and NewPrincipalLimiter
	// itself tolerates a nil deps.Routes -- see its doc comment).
	limiter := deps.Limiter
	if limiter == nil {
		limiter = NewPrincipalLimiter(deps.Routes, PrincipalLimiterOptions{})
	}
	// The active-request registry is both the Server's in-flight tracker AND the
	// resolver's ServerActivityChecker (swap-protection): the SAME instance so the
	// two views stay consistent. Its Add/Remove feed per-server in-flight + last-
	// completion, which the resolver consults to protect an actively-used loaded
	// model from eviction.
	active := newActiveRegistry(broker)
	// CP4 admission queue: an unpinned request whose every candidate is at its
	// effective concurrency cap parks here until a slot frees. active.Remove feeds
	// the slot-free signal (release) via the attached queue.
	admission := newAdmissionQueue(deps.AdmissionQueueMaxDepth)
	active.setAdmission(admission)
	swapProtectWindow := deps.SwapProtectWindow
	if swapProtectWindow <= 0 {
		swapProtectWindow = 30 * time.Second
	}
	sessionReservationWindow := deps.SessionReservationWindow
	if sessionReservationWindow <= 0 {
		sessionReservationWindow = 60 * time.Second
	}
	capacityVRAMMarginPct := deps.CapacityVRAMSafetyMarginPercent
	if capacityVRAMMarginPct <= 0 {
		capacityVRAMMarginPct = 10
	}
	capacityMaxConcurrency := deps.CapacityMaxConcurrency
	if capacityMaxConcurrency <= 0 {
		capacityMaxConcurrency = 64
	}
	capacitySettleSeconds := deps.CapacitySettleSeconds
	if capacitySettleSeconds <= 0 {
		capacitySettleSeconds = 5
	}
	energySettleDelay := time.Duration(deps.EnergySettleSeconds) * time.Second
	if energySettleDelay <= 0 {
		energySettleDelay = defaultEnergySettleDelay
	}
	energyBackfillWindow := deps.EnergyBackfillWindow
	if energyBackfillWindow <= 0 {
		energyBackfillWindow = defaultEnergyBackfillWindow
	}
	energyIdleWindow := time.Duration(deps.EnergyIdleWindowSeconds) * time.Second
	if energyIdleWindow <= 0 {
		energyIdleWindow = time.Hour
	}
	energyIdle := newIdleTracker(energyIdleWindow)
	// A running benchmark reserves its server in this registry; wiring it as the
	// resolver's ServerBusyChecker excludes that server from new routing while a
	// run is in flight. SetServerBusyChecker accepts the same instance the
	// Server holds so the two views stay consistent.
	//
	// The loaded-model registry is wired as the resolver's LoadedModelChecker so
	// selection can prefer a server that already has the requested model resident
	// (avoiding a cold load/swap). *LoadedModelRegistry satisfies
	// routing.LoadedModelChecker via its LoadedAppModels method; it is the SAME
	// instance the Server holds so the two views stay consistent.
	if resolver != nil {
		resolver.SetServerBusyChecker(benchmarks)
		resolver.SetLoadedModelChecker(loadedModels)
		resolver.SetServerActivityChecker(active, swapProtectWindow)
		resolver.SetSessionReservation(sessionReservationWindow)
		resolver.SetAdmissionController(admission)
		resolver.SetGroupResolver(groups)
		// The resource-group provisioning gate (Resource Groups Phase 2) is wired
		// only when a real portal.API is available: deps.Portal is optional, and a
		// portalProvisioningGate wrapping a nil api would make r.provisioning
		// non-nil while every call panics (nil-interface method call) -- the
		// resolver's "nil gate = no-op" invariant requires a genuinely nil gate,
		// not a non-nil gate around a nil api.
		if deps.Portal != nil {
			resolver.SetProvisioningGate(portalProvisioningGate{api: deps.Portal})
		}
		// SetModelWarmer is deferred until after s is built (the warmer needs the fully-
		// assembled Server for its provider/routes/upstream-auth); see below.
	}
	s := &Server{
		Tokens:                      deps.Tokens,
		Usage:                       deps.Usage,
		UsageEvents:                 broker,
		Captures:                    deps.Captures,
		Provider:                    providerClient,
		Routes:                      deps.Routes,
		Resolver:                    resolver,
		LastUsedModelWriter:         deps.LastUsedModelWriter,
		Portal:                      deps.Portal,
		agentBinaryDir:              deps.AgentBinaryDir,
		Account:                     deps.Account,
		CookieSecure:                deps.CookieSecure,
		SessionMaxAge:               deps.SessionMaxAge,
		PublicURL:                   deps.PublicURL,
		streamIdleTimeout:           deps.StreamIdleTimeout,
		selfBaseURL:                 deps.SelfBaseURL,
		Cipher:                      deps.Cipher,
		captureMaxBytes:             captureMaxBytes,
		CaptureEnabled:              deps.CaptureEnabled,
		CaptureOverride:             deps.CaptureOverride,
		Active:                      active,
		AppHealth:                   appHealth,
		capacityVRAMMarginPct:       capacityVRAMMarginPct,
		capacityMaxConcurrency:      capacityMaxConcurrency,
		capacitySettleSeconds:       capacitySettleSeconds,
		capacitySettle:              time.Duration(capacitySettleSeconds) * time.Second,
		mux:                         http.NewServeMux(),
		agentMux:                    http.NewServeMux(),
		baseCtx:                     context.Background(),
		internalAuthSecret:          deps.InternalAuthSecret,
		users:                       deps.Users,
		ChatRuns:                    deps.ChatRuns,
		ServerPerf:                  serverPerf,
		LoadedModels:                loadedModels,
		AgentPresence:               agentPresence,
		AgentCertReports:            agentCertReports,
		AgentTransport:              agentTransport,
		AgentProxyStatus:            agentProxyStatus,
		AgentStreams:                agentStreams,
		AgentFeatures:               agentFeatures,
		RuntimeStatus:               runtimeStatus,
		RuntimeLogs:                 runtimeLogs,
		onAgentReactivated:          deps.OnAgentReactivated,
		Benchmarks:                  benchmarks,
		Groups:                      groups,
		newMailer:                   deps.NewMailer,
		Logs:                        deps.Logs,
		Tracing:                     deps.Tracing,
		tracingOTLPSet:              deps.TracingOTLPSet,
		EnergyIdle:                  energyIdle,
		energySettleDelay:           energySettleDelay,
		energyBackfillWindow:        energyBackfillWindow,
		Limiter:                     limiter,
		acmeChallenges:              deps.ACMEChallenges,
		edgeScheme:                  edgeScheme,
		certEdgeRequireHTTPSDisable: deps.CertEdgeRequireHTTPSDisable,
		certMeshRequireTLSDisable:   deps.CertMeshRequireTLSDisable,
	}
	if s.newMailer == nil {
		s.newMailer = func(cfg mail.Config) Mailer { return mail.New(cfg) }
	}
	// The climb_up load-ahead warmer needs the fully-built Server (provider, routes,
	// upstream-auth), so it is wired here rather than in the pre-s resolver-checker block.
	if resolver != nil {
		resolver.SetModelWarmer(newModelWarmer(s))
	}
	s.routes()
	return s
}

// AgentListenerActive reports whether ANY NetBird agent listener is currently
// bound — the plaintext bind OR the dedicated TLS bind (separate mode). It stays
// true while either is up so the public-mux netbird_only reject is armed unless
// no agent listener exists at all. Runtime-safe: the reconcile loop may update it
// (SetAgentPlainListener/SetAgentListenerTLSState) while request handlers read it
// (the netbird_only gate).
func (s *Server) AgentListenerActive() bool {
	s.agentMu.RLock()
	defer s.agentMu.RUnlock()
	return s.agentListenerActive || s.agentListenerTLS.Active
}

// AgentListenerAddr returns the bound plaintext agent-listener host:port ("" when
// inactive). The dedicated TLS bind's address (separate mode) is on
// AgentListenerTLSState().Address / AgentListenerStates().
func (s *Server) AgentListenerAddr() string {
	s.agentMu.RLock()
	defer s.agentMu.RUnlock()
	return s.agentListenerAddr
}

// AgentListenerState is one atomic snapshot of the plaintext agent bind, mirroring
// AgentListenerTLSState for the TLS bind. Exposed via AgentListenerStates for the
// portal status panel.
type AgentListenerState struct {
	Active  bool
	Address string
}

// AgentListenerStates returns both observable agent-listener slots at once (the
// plaintext bind and the TLS-carrying bind) under a single read lock, so the
// status panel sees a coherent pair rather than two independently-locked reads.
func (s *Server) AgentListenerStates() (AgentListenerState, AgentListenerTLSState) {
	s.agentMu.RLock()
	defer s.agentMu.RUnlock()
	return AgentListenerState{Active: s.agentListenerActive, Address: s.agentListenerAddr}, s.agentListenerTLS
}

// SetAgentPlainListener updates ONLY the plaintext agent-listener slot, leaving
// the TLS slot untouched — the separate-mode plaintext bind owns the plain slot
// while the dedicated TLS bind owns the TLS slot independently. Mutex-guarded,
// mirroring SetAgentListener.
func (s *Server) SetAgentPlainListener(active bool, addr string) {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	s.agentListenerActive = active
	s.agentListenerAddr = addr
}

// SetAgentListener updates the plaintext slot and, when going inactive, also
// clears the TLS slot. It is the combined-mode convenience (one socket owns both
// slots) and is retained for callers/tests that drive a single listener; the
// separate-mode binds use SetAgentPlainListener + SetAgentListenerTLSState so the
// two slots move independently.
func (s *Server) SetAgentListener(active bool, addr string) {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	s.agentListenerActive = active
	s.agentListenerAddr = addr
	if !active {
		s.agentListenerTLS = AgentListenerTLSState{}
	}
}

// AgentListenerTLSState is one atomic snapshot of the certificate the running
// mesh listener can actually serve. It is intentionally runtime state, not a
// projection of a certificate database row.
type AgentListenerTLSState struct {
	Active      bool
	Address     string
	Fingerprint string
	NotAfter    time.Time
}

func (s *Server) AgentListenerTLSState() AgentListenerTLSState {
	s.agentMu.RLock()
	defer s.agentMu.RUnlock()
	return s.agentListenerTLS
}

func (s *Server) SetAgentListenerTLSState(state AgentListenerTLSState) {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	s.agentListenerTLS = state
}

// SetBaseContext installs the server shutdown context. cmd/gateway calls it with a
// context cancelled on SIGTERM so open agent WebSocket handlers close cleanly.
func (s *Server) SetBaseContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.baseCtx = ctx
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// public=true: this is the listener the fronting reverse proxy talks to, so
	// it is the only one the plaintext gate guards.
	s.serveWith(w, r, s.mux, true)
}

// AgentHandler is the handler for the SECOND (NetBird) listener main() starts when
// an agent bind address is configured. It routes through the agent mux (the
// ungated agent-telemetry route + /healthz) with the SAME access-log wrapping as
// the main listener, so the agent path gets identical logging/flush behavior.
func (s *Server) AgentHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// public=false: the NetBird agent listener is NOT behind the fronting
		// reverse proxy (it binds the mesh IP directly and carries no hop header),
		// and gating it would be the agent lockout that is explicitly deferred to a
		// later phase.
		s.serveWith(w, r, s.agentMux, false)
	})
}

// serveWith applies the shared access-log wrapping and dispatches to the given mux
// (the main mux for the public listener, the agent mux for the NetBird listener).
//
// public says which listener called: it is an explicit parameter rather than a
// pointer comparison against s.mux so that every call site has to state it (a new
// caller cannot compile without deciding) and so wrapping a mux in middleware can
// never silently disengage the plaintext gate.
func (s *Server) serveWith(w http.ResponseWriter, r *http.Request, mux http.Handler, public bool) {
	// /healthz is polled every few seconds by the frontend connection gate — logging
	// it would drown the Logs view, so it bypasses the access log AND tracing. It is
	// also one of the plaintext gate's four unconditionally-open paths, so returning
	// here is consistent with (and belt-and-braces on top of) edgeGateOpenPath.
	if r.URL.Path == healthzPath {
		mux.ServeHTTP(w, r)
		return
	}
	// The plaintext gate. Its verdict is computed here -- BEFORE authentication, so
	// an unauthenticated attempt to POST credentials to /api/auth/login in the clear
	// is refused too -- but it is APPLIED at the dispatch point below instead of
	// returning early, so a refusal still gets a span and an access-log line. An
	// operator who has locked themselves out needs to see that in the Logs view.
	gateRedirect, gateRefuse := false, false
	meshRefuse, sourceRefuse := false, false
	if public {
		// Record the hop BEFORE the verdict, so a refused plaintext request still
		// counts as plaintext evidence for the operator's "last unencrypted request"
		// display. countsAsObservation decides what may count at all.
		s.noteEdgeScheme(r, time.Now())
		gateRedirect, gateRefuse = s.edgeGateVerdict(r)
	} else {
		// The mesh plaintext-refusal gate guards ONLY the agent listener. Like the
		// edge gate the verdict is computed here (before auth) but applied at dispatch
		// so a refusal still gets a span and an access-log line.
		meshRefuse = s.meshGateRefuses(r)
		// The netbird_only SOURCE gate (agent_netbird_gate.go) refuses a
		// host-published agent bind (plain or TLS) whose connection did not arrive
		// over the NetBird mesh: the request's LOCAL address must equal the
		// gateway's own mesh peer IP. A mesh-bound listener's local address IS the
		// mesh peer IP, so this is a no-op there. Verdict computed here (before
		// auth) but applied at dispatch, like the mesh gate above, so a refusal
		// still gets a span and an access-log line.
		sourceRefuse = s.agentSourceRefused(r)
	}
	start := time.Now()

	// Open a request span. When tracing is disabled this is a cheap non-recording
	// span; ctx flows down so every ctx-carrying call joins the same trace. The
	// span is renamed to the matched route pattern after dispatch (r.Pattern is
	// populated by ServeMux on the same *Request).
	ctx, span := tracing.Start(r.Context(), r.Method)
	defer span.End()
	if !public {
		ctx = withAgentListenerContext(ctx)
	}
	r = r.WithContext(ctx)

	// Surface the trace id to inference clients (before any body is written) so a
	// Codex/Claude client can correlate its request — only when the span is real.
	if span.SpanContext().IsSampled() && isInferencePath(r.URL.Path) {
		w.Header().Set("X-Trace-Id", span.SpanContext().TraceID().String())
	}

	base := &accessLogResponseWriter{ResponseWriter: w, status: http.StatusOK}
	// Preserve the underlying writer's http.Flusher capability EXACTLY: streaming
	// code (completeStream et al.) does `w.(http.Flusher)` both to stream SSE and to
	// choose its no-flusher fallback. A wrapper that unconditionally implements
	// Flush would mask a non-flushing writer and break that fallback, so we only
	// expose Flush when the underlying writer actually has it. SetWriteDeadline is
	// reached separately via Unwrap + http.NewResponseController.
	var handler http.ResponseWriter = base
	if _, ok := w.(http.Flusher); ok {
		handler = flushingAccessLogResponseWriter{base}
	}
	switch {
	case gateRedirect || gateRefuse:
		// Make the reason visible at the default log level (the access-log line below
		// is Debug), but THROTTLED to one line per path per interval: a retrying
		// client would otherwise evict the 2000-entry log ring this record exists for.
		// The first refusal of any path is always logged immediately. Method, path and
		// remote only -- never a header value, so no bearer token or hop secret can
		// reach the log.
		if s.shouldLogEdgeGateRefusal(r.URL.Path, start) {
			slog.WarnContext(ctx, "plaintext request refused by the edge https gate",
				"method", r.Method,
				"path", r.URL.Path,
				"remote_addr", r.RemoteAddr,
				"redirect", gateRedirect)
		}
		writeEdgeGateRefusal(handler, r, gateRedirect)
	case meshRefuse:
		// Same throttle discipline as the edge gate: one Warn per path per interval,
		// method/path/remote only -- never a header value, so no bearer token leaks.
		if s.shouldLogMeshGateRefusal(r.URL.Path, start) {
			slog.WarnContext(ctx, "plaintext request refused by the mesh tls gate",
				"method", r.Method,
				"path", r.URL.Path,
				"remote_addr", r.RemoteAddr)
		}
		writeJSON(handler, http.StatusForbidden, apierror.Response("certificate.mesh_tls_required",
			"the mesh agent listener requires TLS; set the ServerAgent's gateway_url to https", ""))
	case sourceRefuse:
		// Same code/shape as the public-mux netbird_only gates (routes()): a
		// host-published agent bind refuses a request that did not arrive over the
		// NetBird mesh when netbird_only is on.
		writeJSON(handler, http.StatusForbidden, apierror.Response("netbird.only",
			"agent endpoint is restricted to the NetBird network", ""))
	default:
		mux.ServeHTTP(handler, r)
	}

	// r.Pattern is now the matched route (Go 1.22+ ServeMux sets it on r); rename
	// the span so it reads "POST /v1/chat/completions" not just "POST". Guarded by
	// IsRecording so the route-name string + attribute slice aren't built on the
	// disabled (default) path (SetName/SetAttributes are no-ops there anyway).
	if span.IsRecording() {
		routeName := r.Method
		if r.Pattern != "" {
			routeName = r.Method + " " + r.Pattern
		}
		span.SetName(routeName)
		span.SetAttributes(
			attribute.String("http.request.method", r.Method),
			attribute.String("http.route", r.Pattern),
			attribute.Int("http.response.status_code", base.status),
		)
	}

	// One line per request so ANY client↔gateway exchange leaves a trace in the
	// portal Logs view; DebugContext carries the trace id via the ctx.
	slog.DebugContext(ctx, "http request",
		"method", r.Method,
		"path", r.URL.Path,
		"status", base.status,
		"bytes", base.bytes,
		"duration_ms", time.Since(start).Milliseconds(),
		"remote_addr", r.RemoteAddr)
}

// isInferencePath reports whether p is one of the client-facing inference paths
// (where an X-Trace-Id response header helps a client correlate its request).
func isInferencePath(p string) bool {
	return strings.HasPrefix(p, "/v1/") ||
		strings.HasPrefix(p, "/openai/") ||
		strings.HasPrefix(p, "/anthropic/")
}

// accessLogResponseWriter records the final status code and byte count for the
// access log. It exposes the underlying writer via Unwrap so http.NewResponseController
// can still reach SetWriteDeadline on the real connection. It deliberately does NOT
// implement http.Flusher — see flushingAccessLogResponseWriter and ServeHTTP.
//
// Known, benign trade-off: net/http's MaxBytesReader probes the ResponseWriter for
// its UNEXPORTED requestTooLarge() hook by a direct type assertion (no Unwrap
// traversal). Because that method's identifier is package-private to net/http, no
// wrapper in another package can satisfy it — so on an oversized body to a
// body-limited (control-plane) endpoint the early "close this keep-alive
// connection" optimization is skipped. The request is still correctly rejected
// (413 via *http.MaxBytesError) and net/http still closes the connection when it
// can't drain the body, so this is a lost micro-optimization, not a correctness
// bug. Inference/streaming endpoints read the body unlimited and never hit it.
type accessLogResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

func (w *accessLogResponseWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *accessLogResponseWriter) Write(b []byte) (int, error) {
	w.wrote = true
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *accessLogResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// flushingAccessLogResponseWriter is the variant used only when the underlying
// writer is itself an http.Flusher, so the wrapper advertises Flusher iff the real
// writer does (preserving streaming capability detection).
type flushingAccessLogResponseWriter struct {
	*accessLogResponseWriter
}

func (w flushingAccessLogResponseWriter) Flush() {
	if f, ok := w.accessLogResponseWriter.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("/openai/v1/chat/completions", s.handleOpenAIChat)
	s.mux.HandleFunc("/v1/chat/completions", s.handleOpenAIChat)
	s.mux.HandleFunc("/openai/v1/responses", s.handleOpenAIResponses)
	s.mux.HandleFunc("/v1/responses", s.handleOpenAIResponses)
	s.mux.HandleFunc("/openai/v1/models", s.handleOpenAIModels)
	s.mux.HandleFunc("/v1/models", s.handleOpenAIModels)
	s.mux.HandleFunc("/api/v0/models", s.handleLMStudioModels)
	s.mux.HandleFunc("/anthropic/v1/messages", s.handleAnthropicMessages)
	s.mux.HandleFunc("/v1/messages", s.handleAnthropicMessages)
	s.mux.HandleFunc("/anthropic/v1/messages/count_tokens", s.handleAnthropicCountTokens)
	s.mux.HandleFunc("/v1/messages/count_tokens", s.handleAnthropicCountTokens)
	s.mux.HandleFunc("/anthropic/v1/models", s.handleAnthropicModels)
	s.mux.HandleFunc("/api/auth/login", s.handleAuthLogin)
	s.mux.HandleFunc("/api/auth/logout", s.handleAuthLogout)
	s.mux.HandleFunc("/api/auth/set-password", s.handleAuthSetPassword)
	s.mux.HandleFunc("/api/auth/session", s.handleAuthSession)
	s.mux.HandleFunc("/api/portal/password", s.handlePortalPassword)
	s.mux.HandleFunc("/api/portal/system-admin-mode", s.handleSystemAdminMode)
	s.mux.HandleFunc("/api/portal/language", s.handlePortalLanguage)
	s.mux.HandleFunc("/api/portal/chat-settings", s.handlePortalChatSettings)
	s.mux.HandleFunc("/api/portal/preferences", s.handlePortalPreferences)
	s.mux.HandleFunc("/api/portal/preferences/", s.handlePortalPreferenceItem)
	s.mux.HandleFunc("/api/usage", s.handleUsage)
	s.mux.HandleFunc("/api/portal/me", s.handlePortalMe)
	s.mux.HandleFunc("/api/portal/tokens", s.handlePortalTokens)
	s.mux.HandleFunc("/api/portal/tokens/", s.handlePortalTokenItem)
	s.mux.HandleFunc("/api/portal/chats", s.handlePortalChats)
	s.mux.HandleFunc("/api/portal/chats/", s.handlePortalChatItem)
	s.mux.HandleFunc("/api/portal/usage", s.handlePortalUsage)
	s.mux.HandleFunc("/api/portal/usage/stats", s.handlePortalUsageStats)
	s.mux.HandleFunc("/api/portal/usage/groups", s.handlePortalUsageGroups)
	s.mux.HandleFunc("/api/portal/usage/timeseries", s.handlePortalUsageTimeSeries)
	s.mux.HandleFunc("/api/portal/usage/events", s.handlePortalUsageEvents)
	s.mux.HandleFunc("/api/portal/usage/active", s.handlePortalUsageActive)
	s.mux.HandleFunc("/api/portal/benchmarks/active", s.handlePortalBenchmarksActive)
	s.mux.HandleFunc("/api/portal/usage/captures/", s.handlePortalUsageCapture)
	s.mux.HandleFunc("/api/portal/dashboard", s.handlePortalDashboard)
	s.mux.HandleFunc("/api/portal/models", s.handlePortalModels)
	s.mux.HandleFunc("/api/portal/model-servers", s.handlePortalModelServers)
	s.mux.HandleFunc("/api/portal/model-servers/events", s.handlePortalModelServersEvents)
	s.mux.HandleFunc("/api/portal/model-group-servers", s.handlePortalModelGroupServers)
	s.mux.HandleFunc("/api/portal/servers", s.handlePortalServers)
	s.mux.HandleFunc("/api/portal/servers/", s.handlePortalServerItem)
	s.mux.HandleFunc("/api/portal/server-admin-group-candidates", s.handleServerAdminGroupCandidates)
	s.mux.HandleFunc("/api/portal/service-admin-group-candidates", s.handleServiceAdminGroupCandidates)
	s.mux.HandleFunc("/api/portal/resource-group-admin-group-candidates", s.handleResourceGroupAdminGroupCandidates)
	s.mux.HandleFunc("/api/portal/resource-groups", s.handlePortalResourceGroups)
	s.mux.HandleFunc("/api/portal/resource-groups/", s.handlePortalResourceGroupItem)
	s.mux.HandleFunc("/api/portal/services", s.handlePortalServices)
	s.mux.HandleFunc("/api/portal/services/", s.handlePortalServiceItem)
	s.mux.HandleFunc("/api/portal/applications/", s.handlePortalApplicationItem)
	s.mux.HandleFunc("/api/portal/mappings/", s.handlePortalMappingItem)
	s.mux.HandleFunc("/api/portal/model-groups", s.handlePortalModelGroups)
	s.mux.HandleFunc("/api/portal/model-groups/", s.handlePortalModelGroupItem)
	s.mux.HandleFunc("/api/portal/model-settings/", s.handlePortalModelSettingItem)
	s.mux.HandleFunc("/api/portal/admin/users/", s.handlePortalAdminUserLimits)
	s.mux.HandleFunc("/api/portal/groups", s.handlePortalGroups)
	s.mux.HandleFunc("/api/portal/groups/invitations", s.handlePortalGroupInvitations)
	s.mux.HandleFunc("/api/portal/groups/", s.handlePortalGroupItem)
	s.mux.HandleFunc("/api/portal/admin-owner-candidates", s.handleAdminOwnerCandidates)
	s.mux.HandleFunc("/api/portal/projects", s.handlePortalProjects)
	s.mux.HandleFunc("/api/portal/projects/mine", s.handlePortalProjectsMine)
	s.mux.HandleFunc("/api/portal/projects/", s.handlePortalProjectItem)
	s.mux.HandleFunc("/api/admin/users", s.handleAdminUsers)
	s.mux.HandleFunc("/api/admin/users/", s.handleAdminUserItem)
	s.mux.HandleFunc("/api/system/theme", s.handleSystemTheme)
	s.mux.HandleFunc("/api/system/themes/", s.handleSystemThemeAsset)
	s.mux.HandleFunc("/api/system/settings", s.handleSystemSettings)
	s.mux.HandleFunc("/api/system/smtp/test", s.handleSystemSMTPTest)
	s.mux.HandleFunc("/api/system/netbird/test", s.handleSystemNetbirdTest)
	s.mux.HandleFunc("/api/system/netbird/network", s.handleSystemNetbirdNetwork)
	s.mux.HandleFunc("/api/system/netbird/groups", s.handleSystemNetbirdGroups)
	s.mux.HandleFunc("/api/system/netbird/peers", s.handleSystemNetbirdPeers)
	s.mux.HandleFunc("/api/system/netbird/gateway-setup-key", s.handleSystemNetbirdGatewaySetupKey)
	s.mux.HandleFunc("/api/system/netbird/enroll-sidecar", s.handleSystemNetbirdEnrollSidecar)
	s.mux.HandleFunc("/api/system/servers/", s.handleSystemServerNetbird)
	s.mux.HandleFunc("/api/system/netbird/status", s.handleSystemNetbirdStatus)
	s.mux.HandleFunc("/api/system/netbird/token-status", s.handleSystemNetbirdTokenStatus)
	s.mux.HandleFunc("/api/system/netbird/rotate-token", s.handleSystemNetbirdRotateToken)
	s.mux.HandleFunc("/api/portal/netbird/enabled", s.handlePortalNetbirdEnabled)
	s.mux.HandleFunc("/api/portal/health-check-interval", s.handlePortalHealthCheckInterval)
	s.mux.HandleFunc("/api/portal/agent-presence-timeout", s.handlePortalAgentPresenceTimeout)
	s.mux.HandleFunc("/api/portal/currency", s.handlePortalCurrency)
	s.mux.HandleFunc("/api/system/logs", s.handleSystemLogs)
	s.mux.HandleFunc("/api/system/logs/events", s.handleSystemLogEvents)
	s.mux.HandleFunc("/api/system/logs/level", s.handleSystemLogLevel)
	s.mux.HandleFunc("/api/system/tracing", s.handleSystemTracing)
	s.mux.HandleFunc("/api/portal/totp", s.handlePortalTOTP)
	s.mux.HandleFunc("/api/portal/totp/", s.handlePortalTOTPItem)
	s.mux.HandleFunc("/api/portal/agent-binaries", s.handlePortalAgentBinaries)
	s.mux.HandleFunc("/api/portal/agent-binaries/", s.handlePortalAgentBinaryDownload)
	// The ACME challenge route is PUBLIC + unauthenticated (Let's Encrypt
	// validates anonymously) and deliberately NOT gated by netbird_only --
	// public reachability is this feature's precondition, so it must stay
	// reachable regardless of the NetBird-only transport toggle. It lives ONLY
	// on the public mux, never on agentMux.
	s.mux.HandleFunc("/.well-known/acme-challenge/", s.handleACMEChallenge)
	s.mux.HandleFunc("/api/portal/certificates/enabled", s.handlePortalCertificatesEnabled)
	s.mux.HandleFunc("/api/system/certificates", s.handleSystemCertificates)
	s.mux.HandleFunc("/api/system/certificates/renew", s.handleSystemCertificateRenew)
	s.mux.HandleFunc("/api/system/certificates/ca", s.handleSystemCertificateCA)
	s.mux.HandleFunc("/api/system/certificates/ca/rotate", s.handleSystemCertificateCARotate)
	s.mux.HandleFunc("/api/system/certificates/reissue-all", s.handleSystemCertificateReissueAll)
	s.mux.HandleFunc("/api/system/certificates/edge", s.handleSystemEdgeCertificate)
	s.mux.HandleFunc("/api/system/certificates/edge/reissue", s.handleSystemEdgeCertificateReissue)
	s.mux.HandleFunc("/api/system/certificates/edge/bundle", s.handleSystemEdgeCertificateBundle)
	s.mux.HandleFunc("/api/system/certificates/edge/key", s.handleSystemEdgeCertificateKey)
	s.mux.HandleFunc("/api/system/certificates/edge/proxy-config", s.handleSystemEdgeProxyConfig)
	s.mux.HandleFunc("/api/system/certificates/edge/probe", s.handleSystemEdgeCertificateProbe)
	// P3 public-domain export: a {domain} wildcard is a single path segment, so a
	// slash/traversal can never reach the handler (the service applies the
	// managed + kind=public gates).
	s.mux.HandleFunc("/api/system/certificates/public/{domain}/bundle", s.handleSystemPublicCertificateBundle)
	s.mux.HandleFunc("/api/system/certificates/public/{domain}/key", s.handleSystemPublicCertificateKey)
	// Every agent endpoint below is registered on BOTH muxes: bare on s.agentMux
	// (the NetBird listener -- reaching it already means the request came over
	// the mesh, so no gate is needed there) and gated on s.mux (the PUBLIC
	// listener), via gatedAgentRoute. The public-listener gate is runtime: when a
	// NetBird agent listener actually exists AND the relevant setting is ON, it
	// rejects with 403 netbird.only so agents must use the NetBird listener
	// instead. The gate runs BEFORE the handler's own auth, so an unauthenticated
	// probe on the public path is also rejected when isolated. The fail-safe is
	// AgentListenerActive: with no agent listener the public path always serves
	// (a UI toggle can never cut off ALL agent reporting). Every row uses the
	// netbird_only setting/message except the binary download, which is gated on
	// the dedicated netbird_agent_download_only setting (agents fetch their own
	// binary over the mesh) with its own rejection message.
	const agentGateMessage = "agent endpoint is restricted to the NetBird network"
	agentRoutes := []struct {
		path    string
		handler http.HandlerFunc
		gate    func(context.Context) bool
		message string
	}{
		{"/api/agent/v1/telemetry", s.handleAgentTelemetry, func(ctx context.Context) bool { return s.Portal.NetbirdOnly(ctx) }, agentGateMessage},
		{"/api/agent/v1/stream", s.handleAgentStream, func(ctx context.Context) bool { return s.Portal.NetbirdOnly(ctx) }, agentGateMessage},
		{"/api/agent/v1/system-report", s.handleAgentSystemReport, func(ctx context.Context) bool { return s.Portal.NetbirdOnly(ctx) }, agentGateMessage},
		{"/api/agent/v1/download/", s.handleAgentDownload, func(ctx context.Context) bool { return s.Portal.NetbirdAgentDownloadOnly(ctx) }, "agent download is restricted to the NetBird network"},
		{"/api/agent/v1/certificate", s.handleAgentCertificate, func(ctx context.Context) bool { return s.Portal.NetbirdOnly(ctx) }, agentGateMessage},
		{"/api/agent/v1/ca", s.handleAgentCA, func(ctx context.Context) bool { return s.Portal.NetbirdOnly(ctx) }, agentGateMessage},
		{"/api/agent/v1/proxy-routes", s.handleAgentProxyRoutes, func(ctx context.Context) bool { return s.Portal.NetbirdOnly(ctx) }, agentGateMessage},
		{"/api/agent/v1/features", s.handleAgentFeatures, func(ctx context.Context) bool { return s.Portal.NetbirdOnly(ctx) }, agentGateMessage},
		{"/api/agent/v1/runtime-config", s.handleAgentRuntimeConfig, func(ctx context.Context) bool { return s.Portal.NetbirdOnly(ctx) }, agentGateMessage},
		{"/api/agent/v1/runtime-report", s.handleAgentRuntimeReport, func(ctx context.Context) bool { return s.Portal.NetbirdOnly(ctx) }, agentGateMessage},
	}
	for _, rt := range agentRoutes {
		s.mux.HandleFunc(rt.path, s.gatedAgentRoute(rt.handler, rt.gate, rt.message))
		s.agentMux.HandleFunc(rt.path, rt.handler)
	}
	s.mux.HandleFunc(healthzPath, s.handleHealthz)
	s.mux.HandleFunc("/", s.handleNotFound)

	// /healthz also needs to answer directly on the agent mux (the SECOND,
	// NetBird, listener) so an orchestrator can health-check that listener too.
	s.agentMux.HandleFunc(healthzPath, s.handleHealthz)
}

// gatedAgentRoute wraps an agent handler for registration on the PUBLIC mux:
// it applies the shared AgentListenerActive/Portal-nil-check guard and, only
// once that passes, consults gate to decide whether the request must be
// rejected with the netbird.only 403 (message) instead of reaching h. gate is
// evaluated per request (not captured at registration time) so it always
// reads the live s.Portal, matching the closures this replaces.
func (s *Server) gatedAgentRoute(h http.HandlerFunc, gate func(context.Context) bool, message string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.AgentListenerActive() && s.Portal != nil && gate(r.Context()) {
			writeJSON(w, http.StatusForbidden, apierror.Response("netbird.only", message, ""))
			return
		}
		h(w, r)
	}
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (auth.Token, bool) {
	if s.Tokens == nil {
		writeJSON(w, http.StatusUnauthorized, apierror.Response("auth.invalid_token", "invalid bearer token", ""))
		return auth.Token{}, false
	}
	token, ok := s.Tokens.LookupBearer(r.Header.Get("Authorization"))
	if !ok {
		writeJSON(w, http.StatusUnauthorized, apierror.Response("auth.invalid_token", "invalid bearer token", ""))
		return auth.Token{}, false
	}
	return token, true
}

func (s *Server) requireScope(w http.ResponseWriter, r *http.Request, scope string) (auth.Token, bool) {
	token, ok := s.authenticate(w, r)
	if !ok {
		return auth.Token{}, false
	}
	if !token.HasScope(scope) {
		writeJSON(w, http.StatusForbidden, apierror.Response("auth.insufficient_scope", "insufficient token scope", ""))
		return auth.Token{}, false
	}
	return token, true
}

// requireAnyScope resolves a bearer-only principal (see authenticate) and
// requires it to carry AT LEAST ONE of the given scopes. Used by /v1/responses
// and /v1/messages (service accounts, Phase 1 §5.2), which must accept EITHER a
// normal gateway:use token OR a service token's sole llm:invoke scope.
func (s *Server) requireAnyScope(w http.ResponseWriter, r *http.Request, scopes ...string) (auth.Token, bool) {
	token, ok := s.authenticate(w, r)
	if !ok {
		return auth.Token{}, false
	}
	if !hasAnyScope(token, scopes) {
		writeJSON(w, http.StatusForbidden, apierror.Response("auth.insufficient_scope", "insufficient token scope", ""))
		return auth.Token{}, false
	}
	return token, true
}

func (s *Server) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	// Model discovery is reachable to a service token too (scope llm:invoke) —
	// per spec §13 the allowlist restricts invocation, not discovery, and the
	// list below is intentionally UNFILTERED by the service's allowlist (v1) —
	// it IS filtered by resource-group provisioning visibility (Phase 2 T4).
	token, ok := s.requireAnyScope(w, r, scopeGatewayUse, scopeLLMInvoke)
	if !ok {
		return
	}
	ids := s.Portal.ModelsForFlavor(r.Context(), token, routing.APIFlavorOpenAI)
	data := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]string{"id": id, "object": "model", "owned_by": "op-ai-gateway"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// lmStudioModelsFromDTOs maps the portal model listing into LM Studio's
// GET /api/v0/models item shape so opencode's lmstudio provider can auto-detect
// each model's context window (max_context_length). Chat still flows over the
// OpenAI-compatible /v1/chat/completions path; only the metadata is emulated.
func lmStudioModelsFromDTOs(models []portal.ModelDTO) []map[string]any {
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		state := "not-loaded"
		if m.Loaded {
			state = "loaded"
		}
		item := map[string]any{
			"id":     m.ID,
			"object": "model",
			"type":   "llm",
			"state":  state,
		}
		if m.ContextSize > 0 {
			item["max_context_length"] = m.ContextSize
			if m.Loaded {
				item["loaded_context_length"] = m.ContextSize
			}
		}
		out = append(out, item)
	}
	return out
}

func (s *Server) handleLMStudioModels(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	models := s.Portal.Models(r.Context(), token).Data
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": lmStudioModelsFromDTOs(models)})
}

func (s *Server) handleAnthropicModels(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	ids := s.Portal.ModelsForFlavor(r.Context(), token, routing.APIFlavorAnthropic)
	// created_at is a fixed placeholder: model mappings carry no per-model
	// creation time, and the Anthropic models contract only requires the field.
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "type": "model", "display_name": id, "created_at": "2026-07-09T00:00:00Z"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (s *Server) handlePortalMe(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	user, err := s.Portal.CurrentUser(r.Context(), token)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, apierror.Response("portal.user_not_found", msgUserNotFound, ""))
			return
		}
		writeJSON(w, http.StatusInternalServerError, apierror.Response("portal.user_lookup_failed", "user lookup failed", ""))
		return
	}
	// System-Admin step-up: portal.CurrentUser cannot see the session, so
	// overlay the elevation state here exactly like handleAuthSession, or a
	// loadPortalData refetch would revert the client's elevated view.
	user.SystemAdminModeRequirePassword = s.Portal.SystemAdminModeRequirePassword(r.Context())
	if cookie, cerr := r.Cookie(sessionCookieName); s.Account != nil && cerr == nil && strings.TrimSpace(cookie.Value) != "" {
		if _, session, serr := s.Account.ResolveSessionDetail(r.Context(), cookie.Value); serr == nil && sessionElevated(session) {
			user.SystemAdminMode = true
			user.SystemAdminModeExpiresAt = session.ElevatedUntil.UTC().Format(time.RFC3339)
		}
	}
	writeJSON(w, http.StatusOK, user)
}

// handlePortalHealthCheckInterval reports the effective system-wide app-health
// probe cadence to any portal user (gateway:use scope, GET-only) so the
// application form can display the live "Standard" interval without the
// system-scoped settings endpoint. Returns only the non-secret integer.
func (s *Server) handlePortalHealthCheckInterval(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebScope(w, r, scopeGatewayUse); !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"health_check_interval_seconds": s.Portal.HealthCheckIntervalSeconds(r.Context()),
	})
}

// handlePortalAgentPresenceTimeout reports the effective system-wide
// agent-presence-timeout default to any portal user (gateway:use scope,
// GET-only) so the per-server "Default" field can display the live value
// without the system-scoped settings endpoint. Returns only the non-secret
// integer. Mirrors handlePortalHealthCheckInterval.
func (s *Server) handlePortalAgentPresenceTimeout(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebScope(w, r, scopeGatewayUse); !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"seconds": s.Portal.ActiveAgentPresenceTimeoutSeconds(r.Context()),
	})
}

// handlePortalCurrency reports the system-wide EUR->USD conversion factor
// (usd_per_eur; 0 = unset) so any portal user's client can derive a USD
// display value from an internally-stored EUR cost.
func (s *Server) handlePortalCurrency(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebScope(w, r, scopeGatewayUse); !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"usd_per_eur": s.Portal.CurrencyUsdPerEur(r.Context())})
}

func (s *Server) handlePortalDashboard(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.Portal.Dashboard(r.Context(), token))
}

// handlePortalAdminUserLimits backs GET/PUT /api/portal/admin/users/{id}/limits
// (design spec §7.2): a user's rate/quota/budget limits, admin-only. The gate
// is here, at the HTTP layer (requireWebScope(..., "admin")) — there is
// deliberately NO self-service path: this is the ONLY route that reaches
// portal.Service.UserLimits/SetUserLimits, and it always requires the admin
// scope regardless of whose {id} is addressed, so a normal user can never
// view or change even their OWN limits through this endpoint. A non-`system`
// caller is additionally scoped to ManageableUserIDs (Task 3 fix-round 1,
// per-Admin-Group co-manager permissions, spec 2026-08-10) -- mirrors the
// identical gate in handleAdminUserItem (admin_users.go): checked ONCE,
// before the method switch, so both GET and PUT 404-no-leak identically on a
// target outside the caller's manageable set. A `system`-scope caller (who
// manages everyone) skips this entirely.
func (s *Server) handlePortalAdminUserLimits(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, "admin")
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/portal/admin/users/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "limits" {
		writeJSON(w, http.StatusNotFound, apierror.Response("admin.user_not_found", msgUserNotFound, ""))
		return
	}
	userID := parts[0]
	if !token.HasScope("system") {
		manageable, err := s.Portal.ManageableUserIDs(r.Context(), token)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, apierror.Response("admin.user_list_failed", "user list failed", ""))
			return
		}
		if !manageable[userID] {
			writeJSON(w, http.StatusNotFound, apierror.Response("admin.user_not_found", msgUserNotFound, ""))
			return
		}
	}
	switch r.Method {
	case http.MethodGet:
		dto, err := s.Portal.UserLimits(r.Context(), userID)
		if err != nil {
			writePortalLimitError(w, err, "limit.user_limits_failed")
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodPut:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.LimitConfigDTO
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.SetUserLimits(r.Context(), token, userID, req)
		if err != nil {
			writePortalLimitError(w, err, "limit.set_user_limits_failed")
			return
		}
		writeJSON(w, http.StatusOK, dto)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPut)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// writePortalLimitError maps portal.Service's rate/quota/budget limit
// sentinels (design spec §7) to their HTTP status: ErrLimitValidation -> 400,
// ErrLimitUserNotFound -> 404, else the generic 500 defaultCode. Used by
// handlePortalAdminUserLimits (§7.2's user-limits route); §7.1's Service
// limits ride the existing writePortalServiceError, which already maps
// ErrLimitValidation.
// portalLimitErrRows is writePortalLimitError's one mapper-specific row;
// portal.ErrLimitValidation maps identically in writePortalServiceError and
// lives in sharedErrorMap instead.
var portalLimitErrRows = []errRow{
	{err: portal.ErrLimitUserNotFound, status: http.StatusNotFound, code: "limit.user_not_found", msg: msgUserNotFound},
}

func writePortalLimitError(w http.ResponseWriter, err error, defaultCode string) {
	writeMappedError(w, err, portalLimitErrRows, http.StatusInternalServerError, defaultCode, "limit request failed")
}

func pathID(path string, prefix string) string {
	id := strings.Trim(strings.TrimPrefix(path, prefix), "/")
	if strings.Contains(id, "/") {
		return ""
	}
	return id
}

// usageErrorReader is the optional soft interface a usage.Store may satisfy to
// expose its last recorded DB error (currently only *store.SQLiteStore, via
// the lastUsageError side-channel setLastUsageError maintains). Checked via a
// type assertion rather than added to usage.Store itself, so this stays purely
// additive: no usage.Store implementer (including test doubles) is required to
// grow this method.
type usageErrorReader interface {
	LastUsageError() error
}

// handleHealthz always reports HTTP 200 with "ok": true — the Docker
// HEALTHCHECK (runHealthcheck) and orchestrator probes key off the STATUS
// CODE only, so a usage-store hiccup (a failed insert/query is not the
// gateway being down) must never flip this to a non-200/non-ok response and
// trigger a restart. When the usage store exposes a LastUsageError (see
// usageErrorReader), a non-nil value is surfaced as ADDITIONAL fields so an
// operator polling /healthz (or a script/monitor parsing the body) can see a
// degraded usage store — a failed usage_events insert or a failed Query/
// Stats/TimeSeries read otherwise has no operator-visible signal at all (see
// setLastUsageError in internal/store/sqlite_usage.go for the underlying
// error log).
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	resp := map[string]any{"ok": true}
	if reader, ok := s.Usage.(usageErrorReader); ok {
		if err := reader.LastUsageError(); err != nil {
			resp["usage_store_degraded"] = true
			resp["usage_store_error"] = err.Error()
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, apierror.Response("request.not_found", "not found", ""))
}

// upstreamAuthCtx decorates ctx with the per-application upstream credential
// decrypted from target.APIToken (sealed enc:/plain:) so the provider layer
// attaches it to the upstream request. Fail-open: an empty token or a decrypt
// error logs at Debug and returns ctx unchanged (the upstream will 401 on a real
// misconfiguration; we never crash or refuse the request over it). The CLIENT
// bearer token is never involved — this is a separate gateway-held credential.
func (s *Server) upstreamAuthCtx(ctx context.Context, target routing.Target) context.Context {
	if target.APIToken == "" {
		return ctx
	}
	token, err := capture.OpenSecret(s.Cipher, target.APIToken)
	if err != nil || token == "" {
		if err != nil {
			slog.Debug("upstream api token decrypt failed; proceeding without auth", "route", target.RouteID, "err", err)
		}
		return ctx
	}
	return provider.WithUpstreamAuth(ctx, target.APITokenHeader, token)
}

// serverName resolves a routing target's server ID to its human-readable
// name for usage attribution. Best-effort: an empty string means the lookup
// failed (unknown/unset server) rather than aborting usage recording.
func (s *Server) serverName(serverID string) string {
	if s.Routes == nil || serverID == "" {
		return ""
	}
	server, err := s.Routes.AIServerByID(context.Background(), serverID)
	if err != nil {
		return ""
	}
	return server.Name
}

func nextRequestID() string {
	return fmt.Sprintf("req_%d_%d", time.Now().UTC().UnixNano(), atomic.AddUint64(&requestIDCounter, 1))
}

// liftInferenceDeadlines removes the per-connection read/write deadlines that
// newHTTPServer sets (30s) so uncapped inference bodies can upload slowly and SSE
// streams can run longer than the control-plane window. Control-plane handlers never
// call this and keep the 30s bound. Non-streaming inference responses are therefore
// written with no write deadline by design — the upstream call is still bounded by
// target.Timeout via Complete, while streaming re-arms the write deadline per flush.
// Best-effort: a ResponseWriter without deadline
// support (e.g. httptest.NewRecorder) returns an error that is safely ignored; the
// streaming idle watchdog re-arms the write deadline per flush.
func liftInferenceDeadlines(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
}

func readRawJSON(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	return readRawJSONLimit(w, r, maxJSONBodyBytes)
}

// readRawJSONUnlimited reads exactly one JSON value with no size cap. Used by the
// inference endpoints, which carry base64 image data — matching llama-swap, which
// proxies request bodies without a size limit. Bodies are still read fully into memory.
func readRawJSONUnlimited(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	return readRawJSONLimit(w, r, 0)
}

// readRawJSONLimit reads exactly one JSON value. limit <= 0 disables the size cap.
func readRawJSONLimit(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	var reader io.Reader = r.Body
	if limit > 0 {
		mb := http.MaxBytesReader(w, r.Body, limit)
		defer mb.Close()
		reader = mb
	}
	var raw json.RawMessage
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(&raw); err != nil {
		writeJSONDecodeError(w, err)
		return nil, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, apierror.Response("request.body_too_large", "request body too large", ""))
			return nil, false
		}
		message := "request body must contain exactly one JSON value"
		if err != nil {
			message = err.Error()
		}
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, message, ""))
		return nil, false
	}
	return raw, true
}

func writeJSONDecodeError(w http.ResponseWriter, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		writeJSON(w, http.StatusRequestEntityTooLarge, apierror.Response("request.body_too_large", "request body too large", ""))
		return
	}
	writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
}

func writeRequestError(w http.ResponseWriter, err error) {
	var inferenceErr *inference.Error
	if errors.As(err, &inferenceErr) {
		writeJSON(w, http.StatusBadRequest, apierror.Response(inferenceErr.Code, inferenceErr.Message, ""))
		return
	}
	writeJSON(w, http.StatusBadRequest, apierror.Response("request.invalid", err.Error(), ""))
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	return false
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeJSONCaptured marshals body once, writes it, and returns the exact bytes so
// the same serialization can be handed to the capture pipeline without re-encoding.
func writeJSONCaptured(w http.ResponseWriter, status int, body any) []byte {
	payload, err := json.Marshal(body)
	if err != nil {
		payload = []byte(`{}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
	return payload
}
