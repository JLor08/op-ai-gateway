// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"log/slog"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"regexp"
	"strings"
	"time"
)

// CodeApplicationNotFound/CodeMappingNotFound are ErrApplicationNotFound's/
// ErrMappingNotFound's API error codes, exported so the gateway's error
// mapper/handlers (internal/gateway/portal_application_endpoints.go,
// portal_mapping_endpoints.go) can share the exact value instead of
// re-hardcoding it.
const (
	CodeApplicationNotFound = "application.not_found"
	CodeMappingNotFound     = "mapping.not_found"
)

var (
	ErrApplicationNotFound              = errors.New(CodeApplicationNotFound)
	ErrApplicationTypeInvalid           = errors.New("application.type_invalid")
	ErrApplicationSchemeInvalid         = errors.New("application.scheme_invalid")
	ErrApplicationPortInvalid           = errors.New("application.port_invalid")
	ErrApplicationFlavorInvalid         = errors.New("application.flavor_invalid")
	ErrApplicationStatusInvalid         = errors.New("application.status_invalid")
	ErrApplicationTuningInvalid         = errors.New("application.tuning_invalid")
	ErrApplicationHealthPathInvalid     = errors.New("application.health_path_invalid")
	ErrApplicationHealthIntervalInvalid = errors.New("application.health_interval_invalid")
	ErrApplicationHealthModeInvalid     = errors.New("application.health_mode_invalid")
	// ErrApplicationBenchmarkIntervalInvalid rejects a negative or absurdly large
	// scheduled-benchmark interval (0 = off is allowed).
	ErrApplicationBenchmarkIntervalInvalid = errors.New("application.benchmark_interval_invalid")
	// ErrPathSuffixInvalid rejects a server/app path suffix that looks like a
	// scheme/host rather than a path (it is appended to the origin, so it must be
	// path-only). Shared by the server + application create/update paths. HTTP 400.
	ErrPathSuffixInvalid = errors.New("application.path_suffix_invalid")
	// ErrApplicationTokenHeaderInvalid rejects a custom upstream-token header name
	// that is not a simple HTTP field-name. HTTP 400.
	ErrApplicationTokenHeaderInvalid = errors.New("application.token_header_invalid")
	ErrApplicationConflict           = errors.New("application.port_conflict")
	ErrApplicationSyncFailed         = errors.New("application.sync_failed")
	// ErrApplicationProxyListenPortInvalid rejects a ProxyListenPort outside the
	// valid TCP port range (1..65535); 0 ("auto-assign") is always allowed.
	ErrApplicationProxyListenPortInvalid = errors.New("application.proxy_listen_port_invalid")
	// ErrApplicationProxyListenPortConflict rejects a non-zero ProxyListenPort
	// already used by another application on the same server.
	ErrApplicationProxyListenPortConflict = errors.New("application.proxy_listen_port_conflict")
	// ErrApplicationProxyExcludedPortConflict rejects proxy_excluded:true sent
	// together with an EXPLICIT non-zero proxy_listen_port. Silently zeroing
	// what the caller asked for in the same breath would be a lie; the STORED
	// port is a different matter and IS cleared, because that is the completion
	// of the instruction rather than a contradiction of it.
	ErrApplicationProxyExcludedPortConflict = errors.New("application.proxy_excluded_port_conflict")
	// ErrApplicationProxyEntryScheme rejects an EXPLICIT proxy_excluded:false
	// whose resulting state is https with no proxy port. The agent's proxy
	// forwards decrypted traffic to http://127.0.0.1:<Port>, so a PARTICIPATING
	// application must serve plaintext on its own port. Set scheme http; the
	// gateway flips it to https itself once the agent's TLS listener is
	// confirmed.
	ErrApplicationProxyEntryScheme = errors.New("application.proxy_entry_scheme")

	// CodeMappingNotFound is ErrMappingNotFound's API error code, exported for
	// the same reason as CodeApplicationNotFound above (portal_mapping_endpoints.go).
	ErrMappingNotFound            = errors.New(CodeMappingNotFound)
	ErrMappingGatewayNameRequired = errors.New("mapping.gateway_name_required")
	ErrMappingAppNameRequired     = errors.New("mapping.app_name_required")
	ErrMappingGatewayNameConflict = errors.New("mapping.gateway_name_conflict")
	ErrMappingStatusInvalid       = errors.New("mapping.status_invalid")
	ErrMappingMetricInvalid       = errors.New("mapping.metric_invalid")
)

const (
	defaultApplicationTimeoutMS          = 30000
	defaultApplicationAffinityTTLSeconds = 1800
	defaultApplicationHealthCheckPath    = "/v1/health"
	// defaultServerAgentTimeoutMS is the TimeoutMS default for
	// routing.ProviderServerAgent applications. Application.TimeoutMS is a
	// TOTAL request deadline: it starts when the provider adapter is entered
	// and is never reset by upstream activity, covering the dial, the request
	// write, the silent wait for response headers, and the full body read. An
	// agent-managed runtime's very first request for a cold model must wait
	// for that model process to start and load, which for a large model can
	// take minutes -- with the stock 30s default every cold start would
	// reproducibly fail with 502 provider.timeout.
	defaultServerAgentTimeoutMS = 600000
)

// ApplicationDTO is the portal-facing representation of a routing.Application.
// Endpoint is derived (never stored) from the owning server's domain.
type ApplicationDTO struct {
	ID                 string   `json:"id"`
	ServerID           string   `json:"server_id"`
	Type               string   `json:"type"`
	Port               int      `json:"port"`
	Scheme             string   `json:"scheme"`
	Endpoint           string   `json:"endpoint"`
	APIFlavors         []string `json:"api_flavors"`
	Priority           int      `json:"priority"`
	Weight             int      `json:"weight"`
	TimeoutMS          int      `json:"timeout_ms"`
	AffinityTTLSeconds int      `json:"affinity_ttl_seconds"`
	// AdmissionQueueTimeoutSeconds bounds how long an unpinned request waits in the
	// CP4 admission queue for a free concurrency slot; 0 = wait until the client aborts.
	AdmissionQueueTimeoutSeconds int    `json:"admission_queue_timeout_seconds"`
	Status                       string `json:"status"`
	AlwaysReachable              bool   `json:"always_reachable"`
	HealthCheckPath              string `json:"health_check_path"`
	// HealthCheckMode is the resolved (effective) mode — always_reachable |
	// health_path | model_sync — never empty on the wire even for pre-mode rows.
	HealthCheckMode string `json:"health_check_mode"`
	// HealthCheckIntervalSeconds is the per-application probe cadence in seconds;
	// 0 means "follow the system-wide setting" (Default in the UI), > 0 is Custom.
	HealthCheckIntervalSeconds int `json:"health_check_interval_seconds"`
	// NativeResponses / NativeMessages: when true the gateway proxies the raw
	// client body straight to the upstream's native endpoint (Codex /v1/responses
	// resp. Claude Code /v1/messages) instead of translating it.
	NativeResponses bool `json:"native_responses"`
	NativeMessages  bool `json:"native_messages"`
	// LoadedModelsPath / LoadedModelsFormat configure the optional loaded-model
	// probe: the upstream endpoint path to poll + the response-parser hint
	// ("" / "auto" / "openai" / "llama_swap" / "llama_cpp"). Empty path = off.
	LoadedModelsPath   string `json:"loaded_models_path"`
	LoadedModelsFormat string `json:"loaded_models_format"`
	// ContextProbePath is the optional upstream path GET to learn context size
	// (llama.cpp /props). Empty = off.
	ContextProbePath string `json:"context_probe_path"`
	// CapacityProbePath is the optional upstream path GET to learn the saturation
	// signal used by the capacity benchmark (llama.cpp /metrics, or /props|/slots).
	// Empty = off.
	CapacityProbePath string `json:"capacity_probe_path"`
	// AppPathSuffix is an optional URL path segment appended to the origin (after
	// the server's path suffix) when composing the reachable base URL. Empty = none.
	AppPathSuffix string `json:"app_path_suffix"`
	// APITokenSet reports whether an upstream API token is stored for this
	// application. The token VALUE is write-only and NEVER returned on the wire.
	APITokenSet bool `json:"api_token_set"`
	// APITokenHeader is the optional custom header name used for the upstream token;
	// empty ⇒ "Authorization: Bearer <token>".
	APITokenHeader string `json:"api_token_header"`
	// BenchmarkScheduleEnabled + BenchmarkScheduleIntervalSeconds configure the P5
	// scheduled benchmark mode (0 seconds = unset). OpportunisticMetricsEnabled
	// turns on the P5 opportunistic-EWMA mode. All default false/0 = feature OFF.
	BenchmarkScheduleEnabled         bool `json:"benchmark_schedule_enabled"`
	BenchmarkScheduleIntervalSeconds int  `json:"benchmark_schedule_interval_seconds"`
	OpportunisticMetricsEnabled      bool `json:"opportunistic_metrics_enabled"`
	// ProxyListenPort is the gateway-managed TLS port the agent's local proxy
	// listens on for this application (P4 HTTPS-switch); 0 = not yet assigned
	// (the gateway auto-assigns it once the app needs one). Not user-editable
	// in the portal UI — surfaced read-only for operator visibility.
	ProxyListenPort int `json:"proxy_listen_port"`
	// ProxyExcluded is the operator's opt-out from the gateway-guided TLS proxy
	// (migration 70). Set from the RAW column, never derived: the backfill plus
	// the normalization in Create/UpdateApplication make the column
	// authoritative for every row this service can produce.
	ProxyExcluded bool `json:"proxy_excluded"`
	// Reachable + LastCheckedAt are operational reachability metadata enriched
	// from the app-health registry on the servers/applications endpoints (not on
	// model DTOs). A nil reader or a never-probed application reports
	// Reachable=true with a nil LastCheckedAt.
	Reachable     bool       `json:"reachable"`
	LastCheckedAt *time.Time `json:"last_checked_at"`
	CreatedAt     time.Time  `json:"created_at"`
}

type ApplicationListResponse struct {
	Data []ApplicationDTO `json:"data"`
}

type CreateApplicationRequest struct {
	Type                             string   `json:"type"`
	Port                             int      `json:"port"`
	Scheme                           string   `json:"scheme"`
	APIFlavors                       []string `json:"api_flavors"`
	Priority                         int      `json:"priority"`
	Weight                           int      `json:"weight"`
	TimeoutMS                        int      `json:"timeout_ms"`
	AffinityTTLSeconds               int      `json:"affinity_ttl_seconds"`
	AdmissionQueueTimeoutSeconds     int      `json:"admission_queue_timeout_seconds"`
	Status                           string   `json:"status"`
	AlwaysReachable                  bool     `json:"always_reachable"`
	HealthCheckPath                  string   `json:"health_check_path"`
	HealthCheckMode                  string   `json:"health_check_mode"`
	HealthCheckIntervalSeconds       int      `json:"health_check_interval_seconds"`
	NativeResponses                  bool     `json:"native_responses"`
	NativeMessages                   bool     `json:"native_messages"`
	LoadedModelsPath                 string   `json:"loaded_models_path"`
	LoadedModelsFormat               string   `json:"loaded_models_format"`
	ContextProbePath                 string   `json:"context_probe_path"`
	CapacityProbePath                string   `json:"capacity_probe_path"`
	AppPathSuffix                    string   `json:"app_path_suffix"`
	APIToken                         string   `json:"api_token"`
	APITokenHeader                   string   `json:"api_token_header"`
	BenchmarkScheduleEnabled         bool     `json:"benchmark_schedule_enabled"`
	BenchmarkScheduleIntervalSeconds int      `json:"benchmark_schedule_interval_seconds"`
	OpportunisticMetricsEnabled      bool     `json:"opportunistic_metrics_enabled"`
	// ProxyListenPort: 0 = auto-assign (default; gateway-managed). A caller may
	// set it explicitly, validated unique per server + in the TCP port range.
	ProxyListenPort int `json:"proxy_listen_port"`
	// ProxyExcluded opts the application out of the gateway-guided TLS proxy.
	//
	// A POINTER, unlike NativeResponses and the other create-path bools beside
	// it, and deliberately so: the normalization below must distinguish "the
	// caller said false" from "the caller said nothing", which is what keeps a
	// pre-70 API client producing exactly the state it always produced.
	ProxyExcluded *bool `json:"proxy_excluded,omitempty"`
}

type UpdateApplicationRequest struct {
	Type                         *string   `json:"type,omitempty"`
	Port                         *int      `json:"port,omitempty"`
	Scheme                       *string   `json:"scheme,omitempty"`
	APIFlavors                   *[]string `json:"api_flavors,omitempty"`
	Priority                     *int      `json:"priority,omitempty"`
	Weight                       *int      `json:"weight,omitempty"`
	TimeoutMS                    *int      `json:"timeout_ms,omitempty"`
	AffinityTTLSeconds           *int      `json:"affinity_ttl_seconds,omitempty"`
	AdmissionQueueTimeoutSeconds *int      `json:"admission_queue_timeout_seconds,omitempty"`
	Status                       *string   `json:"status,omitempty"`
	AlwaysReachable              *bool     `json:"always_reachable,omitempty"`
	HealthCheckPath              *string   `json:"health_check_path,omitempty"`
	HealthCheckMode              *string   `json:"health_check_mode,omitempty"`
	HealthCheckIntervalSeconds   *int      `json:"health_check_interval_seconds,omitempty"`
	NativeResponses              *bool     `json:"native_responses,omitempty"`
	NativeMessages               *bool     `json:"native_messages,omitempty"`
	LoadedModelsPath             *string   `json:"loaded_models_path,omitempty"`
	LoadedModelsFormat           *string   `json:"loaded_models_format,omitempty"`
	ContextProbePath             *string   `json:"context_probe_path,omitempty"`
	CapacityProbePath            *string   `json:"capacity_probe_path,omitempty"`
	// AppPathSuffix / APIToken / APITokenHeader use the keep/clear/replace sentinel:
	// nil = keep the stored value, "" = clear, a value replaces. The token value is
	// write-only (sealed on write, never returned in the DTO).
	AppPathSuffix                    *string `json:"app_path_suffix,omitempty"`
	APIToken                         *string `json:"api_token,omitempty"`
	APITokenHeader                   *string `json:"api_token_header,omitempty"`
	BenchmarkScheduleEnabled         *bool   `json:"benchmark_schedule_enabled,omitempty"`
	BenchmarkScheduleIntervalSeconds *int    `json:"benchmark_schedule_interval_seconds,omitempty"`
	OpportunisticMetricsEnabled      *bool   `json:"opportunistic_metrics_enabled,omitempty"`
	// ProxyListenPort: nil = keep the stored value (gateway-managed; the portal
	// UI never sends this). 0 resets to auto-assign; a positive value sets it
	// explicitly, validated unique per server + in the TCP port range.
	ProxyListenPort *int `json:"proxy_listen_port,omitempty"`
	// ProxyExcluded: nil = keep the stored value, the house sentinel. See the
	// create request's field for why participation is a pointer on BOTH paths.
	ProxyExcluded *bool `json:"proxy_excluded,omitempty"`
}

// ListApplications returns every application on serverID for an owner-or-admin principal.
func (s *Service) ListApplications(ctx context.Context, principal auth.Token, serverID string) (ApplicationListResponse, error) {
	server, err := s.authorizeServer(ctx, principal, serverID)
	if err != nil {
		return ApplicationListResponse{}, err
	}
	apps, err := s.routes.ApplicationsByServer(ctx, server.ID)
	if err != nil {
		return ApplicationListResponse{}, err
	}
	out := make([]ApplicationDTO, 0, len(apps))
	for _, app := range apps {
		out = append(out, s.enrichReachability(applicationDTO(server, app)))
	}
	return ApplicationListResponse{Data: out}, nil
}

// CreateApplication validates and persists a new application under serverID.
//
// Managed-runtime-only gate (Task 6): a server with ManagedRuntimeOnly set
// may only host agent-managed model processes, so any create whose type is
// not routing.ProviderServerAgent is rejected with ErrServerManagedRuntimeOnly
// (409 -- a conflict with the server's own configuration, not a malformed
// request). The comparison uses the RAW requested type, deliberately checked
// BEFORE normalizeApplicationType below: "server_agent" itself is exempt from
// this gate -- and, since Task 10 registered "server_agent" as a creatable
// type, that exemption now lets a ManagedRuntimeOnly server actually accept
// the server_agent creates the flag exists to allow (before Task 10,
// normalizeApplicationType did not accept "server_agent" at all, so every
// create on such a server still failed one way or another; this ordering
// only guaranteed the failure was ErrApplicationTypeInvalid, never the
// misleading ErrServerManagedRuntimeOnly).
func (s *Service) CreateApplication(ctx context.Context, principal auth.Token, serverID string, req CreateApplicationRequest) (ApplicationDTO, error) {
	server, err := s.authorizeServer(ctx, principal, serverID)
	if err != nil {
		return ApplicationDTO{}, err
	}
	if server.ManagedRuntimeOnly && strings.TrimSpace(req.Type) != routing.ProviderServerAgent {
		return ApplicationDTO{}, ErrServerManagedRuntimeOnly
	}
	appType, err := normalizeApplicationType(req.Type)
	if err != nil {
		return ApplicationDTO{}, err
	}
	scheme, err := normalizeApplicationScheme(req.Scheme)
	if err != nil {
		return ApplicationDTO{}, err
	}
	port, err := normalizeApplicationPort(req.Port)
	if err != nil {
		return ApplicationDTO{}, err
	}
	flavors, err := normalizeApplicationFlavors(req.APIFlavors)
	if err != nil {
		return ApplicationDTO{}, err
	}
	status, err := normalizeApplicationStatus(req.Status)
	if err != nil {
		return ApplicationDTO{}, err
	}
	if err := validateApplicationTuning(req.Priority, req.Weight, req.TimeoutMS, req.AffinityTTLSeconds, req.AdmissionQueueTimeoutSeconds); err != nil {
		return ApplicationDTO{}, err
	}
	healthCheckPath, err := normalizeApplicationHealthCheckPath(req.HealthCheckPath)
	if err != nil {
		return ApplicationDTO{}, err
	}
	healthMode, err := normalizeHealthCheckMode(req.HealthCheckMode)
	if err != nil {
		return ApplicationDTO{}, err
	}
	// Back-compat: a legacy client that sets only always_reachable (no explicit
	// mode) means the always-reachable mode.
	if strings.TrimSpace(req.HealthCheckMode) == "" && req.AlwaysReachable {
		healthMode = routing.HealthCheckModeAlwaysReachable
	}
	if err := validateApplicationHealthCheckInterval(req.HealthCheckIntervalSeconds); err != nil {
		return ApplicationDTO{}, err
	}
	if err := validateApplicationBenchmarkInterval(req.BenchmarkScheduleIntervalSeconds); err != nil {
		return ApplicationDTO{}, err
	}
	appPath, err := checkPathSuffix(req.AppPathSuffix)
	if err != nil {
		return ApplicationDTO{}, err
	}
	apiTokenHeader, err := checkHeaderName(req.APITokenHeader)
	if err != nil {
		return ApplicationDTO{}, err
	}
	proxyListenPort, err := normalizeApplicationProxyListenPort(req.ProxyListenPort)
	if err != nil {
		return ApplicationDTO{}, err
	}
	proxyPortTaken, err := s.proxyListenPortTakenOnServer(ctx, server.ID, proxyListenPort, "")
	if err != nil {
		return ApplicationDTO{}, err
	}
	if proxyPortTaken {
		return ApplicationDTO{}, ErrApplicationProxyListenPortConflict
	}
	if appType == routing.ProviderServerAgent {
		exists, err := s.serverAgentApplicationExistsOnServer(ctx, server.ID, "")
		if err != nil {
			return ApplicationDTO{}, err
		}
		if exists {
			return ApplicationDTO{}, ErrServerAgentApplicationExists
		}
	}
	// Seal the upstream token up front so a disk-store-without-key rejection surfaces
	// BEFORE anything is persisted ("" seals to "" = no token).
	sealedToken, err := capture.SealSecret(s.cipher, s.settingsVolatile, req.APIToken)
	if err != nil {
		return ApplicationDTO{}, err
	}
	now := s.clock().UTC()
	app := routing.Application{
		ID:                               "app_" + compactRandomHex(16),
		ServerID:                         server.ID,
		Type:                             appType,
		Port:                             port,
		Scheme:                           scheme,
		APIFlavors:                       flavors,
		Priority:                         req.Priority,
		Weight:                           req.Weight,
		TimeoutMS:                        normalizeApplicationTimeoutMS(appType, req.TimeoutMS),
		AffinityTTLSeconds:               normalizeApplicationAffinityTTLSeconds(req.AffinityTTLSeconds),
		AdmissionQueueTimeoutSeconds:     req.AdmissionQueueTimeoutSeconds,
		Status:                           status,
		AlwaysReachable:                  healthMode == routing.HealthCheckModeAlwaysReachable,
		HealthCheckPath:                  healthCheckPath,
		HealthCheckMode:                  healthMode,
		HealthCheckIntervalSeconds:       req.HealthCheckIntervalSeconds,
		NativeResponses:                  req.NativeResponses,
		NativeMessages:                   req.NativeMessages,
		LoadedModelsPath:                 strings.TrimSpace(req.LoadedModelsPath),
		LoadedModelsFormat:               normalizeLoadedModelsFormat(req.LoadedModelsFormat),
		ContextProbePath:                 strings.TrimSpace(req.ContextProbePath),
		CapacityProbePath:                strings.TrimSpace(req.CapacityProbePath),
		AppPathSuffix:                    appPath,
		APIToken:                         sealedToken,
		APITokenHeader:                   apiTokenHeader,
		BenchmarkScheduleEnabled:         req.BenchmarkScheduleEnabled,
		BenchmarkScheduleIntervalSeconds: req.BenchmarkScheduleIntervalSeconds,
		OpportunisticMetricsEnabled:      req.OpportunisticMetricsEnabled,
		ProxyListenPort:                  proxyListenPort,
		CreatedAt:                        now,
		UpdatedAt:                        now,
	}
	// LAST, after every other field is settled, so no field ordering above can
	// bypass the invariant it establishes. req.ProxyListenPort is a plain int on
	// this path, so "explicitly requested" is simply "non-zero".
	if err := applyProxyExclusion(&app, req.ProxyExcluded, req.ProxyListenPort); err != nil {
		return ApplicationDTO{}, err
	}
	if err := s.routes.CreateApplication(ctx, app); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return ApplicationDTO{}, s.classifyApplicationWriteConflict(ctx, app, "")
		}
		return ApplicationDTO{}, err
	}
	// Best-effort: a new server_agent application IS the agent's router-port
	// configuration, so tell any connected agent immediately instead of
	// leaving it to the 60 s poll backstop. No previous type (the row did not
	// exist). Fired BEFORE reconcileServerPolicy so a slow NetBird round trip
	// cannot delay the push this fix exists to make prompt.
	s.notifyRuntimeChangedForApplication(server.ID, "", app.Type)
	// Best-effort: a new application changes the server's active port set, so its
	// NetBird access policy (if managed) may need to grow. reconcileServerPolicy
	// gates internally on the module + policy management and never errors.
	s.reconcileServerPolicy(ctx, server.ID)
	return s.enrichReachability(applicationDTO(server, app)), nil
}

// GetApplication returns the application dto for an owner-or-admin principal.
func (s *Service) GetApplication(ctx context.Context, principal auth.Token, appID string) (ApplicationDTO, error) {
	app, server, err := s.authorizeApplication(ctx, principal, appID)
	if err != nil {
		return ApplicationDTO{}, err
	}
	return s.enrichReachability(applicationDTO(server, app)), nil
}

// UpdateApplication partially updates an application, re-validating any changed fields.
func (s *Service) UpdateApplication(ctx context.Context, principal auth.Token, appID string, req UpdateApplicationRequest) (ApplicationDTO, error) {
	app, server, err := s.authorizeApplication(ctx, principal, appID)
	if err != nil {
		return ApplicationDTO{}, err
	}
	// Captured before the mutation block below reassigns app.Type: the runtime
	// notification needs BOTH sides of a retype (see
	// notifyRuntimeChangedForApplication -- retyping AWAY from server_agent
	// must notify too).
	previousType := app.Type
	// Captured for the same reason, and read only by the warning below: once
	// applyProxyExclusion has run, the port it released is gone from app.
	previousProxyListenPort := app.ProxyListenPort
	// Validate everything that can fail BEFORE mutating the loaded application.
	var appType, scheme, status, healthCheckPath string
	var port int
	var flavors []string
	if req.Type != nil {
		appType, err = normalizeApplicationType(*req.Type)
		if err != nil {
			return ApplicationDTO{}, err
		}
	}
	if req.Scheme != nil {
		scheme, err = normalizeApplicationScheme(*req.Scheme)
		if err != nil {
			return ApplicationDTO{}, err
		}
	}
	if req.Port != nil {
		port, err = normalizeApplicationPort(*req.Port)
		if err != nil {
			return ApplicationDTO{}, err
		}
	}
	if req.APIFlavors != nil {
		flavors, err = normalizeApplicationFlavors(*req.APIFlavors)
		if err != nil {
			return ApplicationDTO{}, err
		}
	}
	if req.Status != nil {
		status, err = normalizeApplicationStatus(*req.Status)
		if err != nil {
			return ApplicationDTO{}, err
		}
	}
	if req.Priority != nil && *req.Priority < 0 {
		return ApplicationDTO{}, ErrApplicationTuningInvalid
	}
	if req.Weight != nil && *req.Weight < 0 {
		return ApplicationDTO{}, ErrApplicationTuningInvalid
	}
	if req.TimeoutMS != nil && *req.TimeoutMS < 0 {
		return ApplicationDTO{}, ErrApplicationTuningInvalid
	}
	if req.AffinityTTLSeconds != nil && *req.AffinityTTLSeconds < 0 {
		return ApplicationDTO{}, ErrApplicationTuningInvalid
	}
	if req.AdmissionQueueTimeoutSeconds != nil && *req.AdmissionQueueTimeoutSeconds < 0 {
		return ApplicationDTO{}, ErrApplicationTuningInvalid
	}
	if req.HealthCheckPath != nil {
		healthCheckPath, err = normalizeApplicationHealthCheckPath(*req.HealthCheckPath)
		if err != nil {
			return ApplicationDTO{}, err
		}
	}
	var healthMode string
	if req.HealthCheckMode != nil {
		healthMode, err = normalizeHealthCheckMode(*req.HealthCheckMode)
		if err != nil {
			return ApplicationDTO{}, err
		}
	}
	if req.HealthCheckIntervalSeconds != nil {
		if err := validateApplicationHealthCheckInterval(*req.HealthCheckIntervalSeconds); err != nil {
			return ApplicationDTO{}, err
		}
	}
	if req.BenchmarkScheduleIntervalSeconds != nil {
		if err := validateApplicationBenchmarkInterval(*req.BenchmarkScheduleIntervalSeconds); err != nil {
			return ApplicationDTO{}, err
		}
	}
	var proxyListenPort int
	if req.ProxyListenPort != nil {
		proxyListenPort, err = normalizeApplicationProxyListenPort(*req.ProxyListenPort)
		if err != nil {
			return ApplicationDTO{}, err
		}
		proxyPortTaken, err := s.proxyListenPortTakenOnServer(ctx, server.ID, proxyListenPort, app.ID)
		if err != nil {
			return ApplicationDTO{}, err
		}
		if proxyPortTaken {
			return ApplicationDTO{}, ErrApplicationProxyListenPortConflict
		}
	}
	// The same "at most one server_agent application per server" invariant the
	// create path enforces. Gated on req.Type being present AND resolving to
	// server_agent: an update that does not touch the type is never refused
	// here, so ordinary edits to an application on a server that already
	// violates the invariant (only reachable on a pre-invariant dev database)
	// keep working instead of becoming un-editable. app.ID is excluded, so
	// re-sending the same type on the server's own server_agent application is
	// not a self-collision.
	if req.Type != nil && appType == routing.ProviderServerAgent {
		exists, err := s.serverAgentApplicationExistsOnServer(ctx, server.ID, app.ID)
		if err != nil {
			return ApplicationDTO{}, err
		}
		if exists {
			return ApplicationDTO{}, ErrServerAgentApplicationExists
		}
	}
	if req.Type != nil {
		app.Type = appType
	}
	if req.Scheme != nil {
		app.Scheme = scheme
	}
	if req.Port != nil {
		app.Port = port
	}
	if req.APIFlavors != nil {
		app.APIFlavors = flavors
	}
	if req.Status != nil {
		app.Status = status
	}
	if req.Priority != nil {
		app.Priority = *req.Priority
	}
	if req.Weight != nil {
		app.Weight = *req.Weight
	}
	if req.TimeoutMS != nil {
		// app.Type has already been reassigned above if req.Type != nil, so this
		// picks up the application's OWN (post-mutation) type -- see
		// normalizeApplicationTimeoutMS's doc comment.
		app.TimeoutMS = normalizeApplicationTimeoutMS(app.Type, *req.TimeoutMS)
	}
	if req.AffinityTTLSeconds != nil {
		app.AffinityTTLSeconds = normalizeApplicationAffinityTTLSeconds(*req.AffinityTTLSeconds)
	}
	if req.AdmissionQueueTimeoutSeconds != nil {
		app.AdmissionQueueTimeoutSeconds = *req.AdmissionQueueTimeoutSeconds
	}
	if req.HealthCheckPath != nil {
		app.HealthCheckPath = healthCheckPath
	}
	if req.HealthCheckIntervalSeconds != nil {
		app.HealthCheckIntervalSeconds = *req.HealthCheckIntervalSeconds
	}
	if req.NativeResponses != nil {
		app.NativeResponses = *req.NativeResponses
	}
	if req.NativeMessages != nil {
		app.NativeMessages = *req.NativeMessages
	}
	if req.LoadedModelsPath != nil {
		app.LoadedModelsPath = strings.TrimSpace(*req.LoadedModelsPath)
	}
	if req.LoadedModelsFormat != nil {
		app.LoadedModelsFormat = normalizeLoadedModelsFormat(*req.LoadedModelsFormat)
	}
	if req.ContextProbePath != nil {
		app.ContextProbePath = strings.TrimSpace(*req.ContextProbePath)
	}
	if req.CapacityProbePath != nil {
		app.CapacityProbePath = strings.TrimSpace(*req.CapacityProbePath)
	}
	if req.AppPathSuffix != nil {
		v, err := checkPathSuffix(*req.AppPathSuffix)
		if err != nil {
			return ApplicationDTO{}, err
		}
		app.AppPathSuffix = v
	}
	if req.APITokenHeader != nil {
		h, err := checkHeaderName(*req.APITokenHeader)
		if err != nil {
			return ApplicationDTO{}, err
		}
		app.APITokenHeader = h
	}
	if req.APIToken != nil {
		// nil = keep the loaded (sealed) token untouched; "" seals to "" = clear;
		// a value replaces. Sealing here surfaces a disk-without-key rejection.
		sealed, err := capture.SealSecret(s.cipher, s.settingsVolatile, *req.APIToken)
		if err != nil {
			return ApplicationDTO{}, err
		}
		app.APIToken = sealed
	}
	if req.BenchmarkScheduleEnabled != nil {
		app.BenchmarkScheduleEnabled = *req.BenchmarkScheduleEnabled
	}
	if req.BenchmarkScheduleIntervalSeconds != nil {
		app.BenchmarkScheduleIntervalSeconds = *req.BenchmarkScheduleIntervalSeconds
	}
	if req.OpportunisticMetricsEnabled != nil {
		app.OpportunisticMetricsEnabled = *req.OpportunisticMetricsEnabled
	}
	if req.ProxyListenPort != nil {
		app.ProxyListenPort = proxyListenPort
	}
	// health_check_mode is authoritative; a legacy client sending only
	// always_reachable maps it to the matching mode so the two stay coherent.
	if req.HealthCheckMode != nil {
		app.HealthCheckMode = healthMode
		app.AlwaysReachable = healthMode == routing.HealthCheckModeAlwaysReachable
	} else if req.AlwaysReachable != nil {
		app.AlwaysReachable = *req.AlwaysReachable
		if *req.AlwaysReachable {
			app.HealthCheckMode = routing.HealthCheckModeAlwaysReachable
		} else {
			app.HealthCheckMode = routing.HealthCheckModeHealthPath
		}
	}
	// LAST in the mutation block, after health_check_mode and everything else,
	// so no field ordering above can bypass the invariant it establishes. An
	// explicitly requested port is nil-or-value here, and only a non-zero value
	// contradicts an exclusion.
	explicitProxyListenPort := 0
	if req.ProxyListenPort != nil {
		explicitProxyListenPort = proxyListenPort
	}
	if err := applyProxyExclusion(&app, req.ProxyExcluded, explicitProxyListenPort); err != nil {
		return ApplicationDTO{}, err
	}
	warnProxyExclusionOwnTLS(server, app, previousProxyListenPort)
	app.UpdatedAt = s.clock().UTC()
	if err := s.routes.UpdateApplication(ctx, app); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return ApplicationDTO{}, s.classifyApplicationWriteConflict(ctx, app, app.ID)
		}
		return ApplicationDTO{}, err
	}
	// Best-effort: an edit to a server_agent application changes the agent's
	// runtime config (its Port is router_listen), and retyping one away from
	// server_agent means the agent must tear that router down. Both directions
	// notify; so does an edit that touches no runtime-relevant field at all --
	// see notifyRuntimeChangedForApplication for why over-notifying is the
	// deliberate choice here.
	s.notifyRuntimeChangedForApplication(server.ID, previousType, app.Type)
	// Best-effort: a port/status change may alter the server's active port set, so
	// its NetBird access policy (if managed) may need to be updated. Gates
	// internally on the module + policy management and never errors.
	s.reconcileServerPolicy(ctx, server.ID)
	return s.enrichReachability(applicationDTO(server, app)), nil
}

// DeleteApplication removes the application (and, at the store layer, its mappings).
func (s *Service) DeleteApplication(ctx context.Context, principal auth.Token, appID string) error {
	app, server, err := s.authorizeApplication(ctx, principal, appID)
	if err != nil {
		return err
	}
	if err := s.routes.DeleteApplication(ctx, app.ID); err != nil {
		return err
	}
	// Best-effort: deleting the server_agent application empties the server's
	// runtime-config document (AgentRuntimeConfig's "no server_agent
	// application" case), which the agent must act on by tearing its router
	// and every managed process down. No current type (the row is gone).
	s.notifyRuntimeChangedForApplication(server.ID, app.Type, "")
	// Best-effort: removing an application may drop ports from the server's active
	// set, so its NetBird access policy (if managed) may need to shrink or be
	// deleted. server was captured BEFORE the delete; reconcileServerPolicy
	// re-loads it by id and re-derives the active ports from the now-updated
	// application set. Gates internally on the module + policy management and
	// never errors.
	s.reconcileServerPolicy(ctx, server.ID)
	return nil
}

// authorizeApplication loads the application, resolves its owning server, and
// authorizes the principal as admin-or-owner of that server. Any failure
// (missing application, missing server, failed authorization) collapses to
// ErrApplicationNotFound so the caller never learns whether the application
// or its server exists (no existence leak).
func (s *Service) authorizeApplication(ctx context.Context, principal auth.Token, appID string) (routing.Application, routing.AIServer, error) {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return routing.Application{}, routing.AIServer{}, ErrApplicationNotFound
	}
	app, err := s.routes.ApplicationByID(ctx, appID)
	if err != nil {
		return routing.Application{}, routing.AIServer{}, ErrApplicationNotFound
	}
	server, err := s.authorizeServer(ctx, principal, app.ServerID)
	if err != nil {
		return routing.Application{}, routing.AIServer{}, ErrApplicationNotFound
	}
	return app, server, nil
}

func applicationDTO(server routing.AIServer, app routing.Application) ApplicationDTO {
	flavors := make([]string, len(app.APIFlavors))
	copy(flavors, app.APIFlavors)
	return ApplicationDTO{
		ID:                           app.ID,
		ServerID:                     app.ServerID,
		Type:                         app.Type,
		Port:                         app.Port,
		Scheme:                       app.Scheme,
		Endpoint:                     routing.ApplicationEndpoint(server, app),
		APIFlavors:                   flavors,
		Priority:                     app.Priority,
		Weight:                       app.Weight,
		TimeoutMS:                    app.TimeoutMS,
		AffinityTTLSeconds:           app.AffinityTTLSeconds,
		AdmissionQueueTimeoutSeconds: app.AdmissionQueueTimeoutSeconds,
		Status:                       app.Status,
		AlwaysReachable:              app.AlwaysReachable,
		HealthCheckPath:              app.HealthCheckPath,
		HealthCheckMode:              routing.EffectiveHealthCheckMode(app),
		HealthCheckIntervalSeconds:   app.HealthCheckIntervalSeconds,
		NativeResponses:              app.NativeResponses,
		NativeMessages:               app.NativeMessages,
		LoadedModelsPath:             app.LoadedModelsPath,
		LoadedModelsFormat:           app.LoadedModelsFormat,
		ContextProbePath:             app.ContextProbePath,
		CapacityProbePath:            app.CapacityProbePath,
		AppPathSuffix:                app.AppPathSuffix,
		// APITokenSet reports presence only; the token VALUE is never on the wire.
		APITokenSet:                      app.APIToken != "",
		APITokenHeader:                   app.APITokenHeader,
		BenchmarkScheduleEnabled:         app.BenchmarkScheduleEnabled,
		BenchmarkScheduleIntervalSeconds: app.BenchmarkScheduleIntervalSeconds,
		OpportunisticMetricsEnabled:      app.OpportunisticMetricsEnabled,
		ProxyListenPort:                  app.ProxyListenPort,
		ProxyExcluded:                    app.ProxyExcluded,
		// Default to reachable; enrichReachability overrides it only when the
		// registry has actually probed this application.
		Reachable:     true,
		LastCheckedAt: nil,
		CreatedAt:     app.CreatedAt,
	}
}

// enrichReachability fills the reachable + last_checked_at fields from the
// app-health reader, best-effort: a nil reader or an unknown (never-probed)
// application keeps the default (reachable=true, last_checked_at=nil). Only the
// servers/applications DTO endpoints enrich; model DTOs never do.
func (s *Service) enrichReachability(dto ApplicationDTO) ApplicationDTO {
	if s.reachability == nil {
		return dto
	}
	reachable, lastCheckedAt, known := s.reachability.ApplicationHealth(dto.ID)
	if !known {
		return dto
	}
	dto.Reachable = reachable
	checkedAt := lastCheckedAt
	dto.LastCheckedAt = &checkedAt
	return dto
}

// validateApplicationHealthCheckInterval accepts 0 (Default: follow the
// system-wide health_check_interval_seconds) or a custom value within the same
// [min,max] bounds the system setting uses; anything else (incl. negatives) is
// rejected.
func validateApplicationHealthCheckInterval(seconds int) error {
	if seconds == 0 {
		return nil
	}
	if seconds < MinHealthCheckIntervalSeconds || seconds > MaxHealthCheckIntervalSeconds {
		return ErrApplicationHealthIntervalInvalid
	}
	return nil
}

// MaxBenchmarkScheduleIntervalSeconds caps the scheduled-benchmark cadence at 30
// days; anything larger is almost certainly a mistake.
const MaxBenchmarkScheduleIntervalSeconds = 30 * 24 * 3600

// validateApplicationBenchmarkInterval accepts 0 (scheduled benchmark off /
// unset) or any positive value up to MaxBenchmarkScheduleIntervalSeconds; a
// negative or absurdly large value is rejected.
func validateApplicationBenchmarkInterval(seconds int) error {
	if seconds < 0 || seconds > MaxBenchmarkScheduleIntervalSeconds {
		return ErrApplicationBenchmarkIntervalInvalid
	}
	return nil
}

// tokenHeaderNamePattern matches a simple HTTP field-name (letters, digits,
// hyphen) — the allowed shape for a custom upstream-token header.
var tokenHeaderNamePattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// checkPathSuffix trims a URL path suffix and rejects anything containing "://"
// (it is appended to the origin, so it must be a path, not a scheme/host). The
// trimmed value is returned; an empty suffix is valid (⇒ none).
func checkPathSuffix(s string) (string, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "://") {
		return "", ErrPathSuffixInvalid
	}
	return s, nil
}

// checkHeaderName trims a custom upstream-token header name. Empty is allowed
// (⇒ the default "Authorization: Bearer <token>"); a non-empty value must be a
// simple HTTP field-name (letters/digits/hyphen) else ErrApplicationTokenHeaderInvalid.
func checkHeaderName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if !tokenHeaderNamePattern.MatchString(s) {
		return "", ErrApplicationTokenHeaderInvalid
	}
	return s, nil
}

// normalizeLoadedModelsFormat lowercases/trims the loaded-models parser hint and
// coerces anything outside the known set to "auto" (the tolerant multi-shape
// parser). An empty value stays "" (meaning auto at probe time). This is lenient
// by design: a bad value degrades to auto-detect rather than erroring the save.
func normalizeLoadedModelsFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "openai":
		return "openai"
	case "llama_swap":
		return "llama_swap"
	case "llama_cpp":
		return "llama_cpp"
	case "litellm":
		return "litellm"
	case "", "auto":
		return strings.ToLower(strings.TrimSpace(format)) // "" or "auto"
	default:
		return "auto"
	}
}

// normalizeApplicationHealthCheckPath trims the raw path, defaults an empty
// value to "/v1/health", and requires an absolute path (must start with "/").
func normalizeApplicationHealthCheckPath(raw string) (string, error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return defaultApplicationHealthCheckPath, nil
	}
	if !strings.HasPrefix(path, "/") {
		return "", ErrApplicationHealthPathInvalid
	}
	return path, nil
}

// normalizeHealthCheckMode trims the raw mode, defaults an empty value to the
// health-path mode (today's default reachability check), and rejects anything
// that is not one of the three known modes.
func normalizeHealthCheckMode(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case "":
		return routing.HealthCheckModeHealthPath, nil
	case routing.HealthCheckModeAlwaysReachable:
		return routing.HealthCheckModeAlwaysReachable, nil
	case routing.HealthCheckModeHealthPath:
		return routing.HealthCheckModeHealthPath, nil
	case routing.HealthCheckModeModelSync:
		return routing.HealthCheckModeModelSync, nil
	default:
		return "", ErrApplicationHealthModeInvalid
	}
}

func normalizeApplicationType(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case routing.ProviderOllama:
		return routing.ProviderOllama, nil
	case routing.ProviderVLLM:
		return routing.ProviderVLLM, nil
	case routing.ProviderLlamaCPP:
		return routing.ProviderLlamaCPP, nil
	case routing.ProviderLlamaSwap:
		return routing.ProviderLlamaSwap, nil
	case routing.ProviderLiteLLM:
		return routing.ProviderLiteLLM, nil
	case routing.ProviderServerAgent:
		return routing.ProviderServerAgent, nil
	default:
		return "", ErrApplicationTypeInvalid
	}
}

func normalizeApplicationScheme(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case "http":
		return "http", nil
	case "https":
		return "https", nil
	default:
		return "", ErrApplicationSchemeInvalid
	}
}

func normalizeApplicationPort(port int) (int, error) {
	if port < 1 || port > 65535 {
		return 0, ErrApplicationPortInvalid
	}
	return port, nil
}

// applyProxyExclusion resolves the operator's PARTICIPATION decision and
// establishes the invariant the rest of this change rests on:
//
//	ProxyExcluded == true  =>  ProxyListenPort == 0
//
// It is called LAST in the mutation block of BOTH CreateApplication and
// UpdateApplication -- one function rather than two copies, so the two paths
// cannot drift -- and it is the ONLY place in the tree that writes
// Application.ProxyExcluded outside a direct store write.
//
// The invariant is what keeps this change small. Every other spelling of
// "proxied" in the tree tests scheme == "https" && ProxyListenPort != 0 --
// routing.ApplicationEndpoint, activePortStrings, revertScopeExit,
// HTTPSSwitchUnreachableApps -- and an excluded application can satisfy none of
// them, so none of them needs to learn about this field.
//
// requested is the caller's proxy_excluded (nil = said nothing).
// explicitProxyListenPort is the port the SAME request explicitly asked for (0
// when it asked for none), which is not the same thing as the port already
// stored on app.
func applyProxyExclusion(app *routing.Application, requested *bool, explicitProxyListenPort int) error {
	switch {
	case requested != nil && *requested:
		// RULE 1 -- EXPLICIT EXCLUSION. Refuse a request that asks for both a
		// non-participating application and a listener for it: silently zeroing
		// a port the caller named in the same breath would be a lie. Clearing
		// the STORED port is a different matter and is done here, because that
		// is the completion of the instruction rather than a contradiction of
		// it -- and it is what releases the port back to the free pool.
		if explicitProxyListenPort != 0 {
			return ErrApplicationProxyExcludedPortConflict
		}
		app.ProxyExcluded = true
		app.ProxyListenPort = 0
	case requested != nil:
		// RULE 2 -- EXPLICIT PARTICIPATION. Validated before it is applied, so a
		// refused request leaves nothing half-written. A participating
		// application must serve PLAINTEXT on its own port: the agent's proxy
		// terminates TLS and forwards to http://127.0.0.1:<Port>, so https with
		// no proxy port describes an application the proxy cannot front. Only
		// the gateway assigns a proxy port, and only to candidates, so http is
		// the only re-entry.
		if effectiveScheme(*app) == "https" && app.ProxyListenPort == 0 {
			return ErrApplicationProxyEntryScheme
		}
		app.ProxyExcluded = false
	default:
		// RULE 3 -- NORMALIZATION, the one back-compat translation, and the ONLY
		// place the retired implicit encoding is read anywhere in the tree. A
		// caller that says NOTHING about participation and asks for https with
		// no proxy port is describing the own-TLS state, which is exactly what
		// the flag now names -- the same shape of translation the
		// always_reachable -> health_check_mode back-compat above carries.
		//
		// Applying it on every write (rather than inferring at the API edge
		// forever) is what keeps the column authoritative for every stored row:
		// a pre-70 client can go on speaking the old encoding and still produce
		// a row every reader understands without re-deriving anything.
		if effectiveScheme(*app) == "https" && app.ProxyListenPort == 0 && !app.ProxyExcluded {
			app.ProxyExcluded = true
		}
	}

	// RULE 4 -- THE POST-STATE CHECK, and the reason it cannot be folded into the
	// arms above: every one of them branches on the SHAPE OF THE REQUEST, and the
	// invariant is a property of the RESOLVED ROW. Rule 1 refuses an explicit port
	// only when the same request also excludes; rule 3 is guarded on
	// !app.ProxyExcluded and therefore does nothing at all for an already-excluded
	// row. So two ordinary PATCHes -- exclude, then set a port while saying
	// nothing about participation -- reached ProxyExcluded=true WITH a port, and
	// the earlier claim that only a direct store write could do that was wrong.
	//
	// That state is a silent total outage rather than an inconsistency:
	// ApplicationEndpoint routes to https://<domain>:<port> while
	// isProxySwitchCandidate is false, so no route is published, no listener
	// exists there, and HTTPSSwitchUnreachableApps cannot even name the row --
	// its filter begins with the candidate predicate. Nothing reports it and
	// nothing repairs it short of a later scope exit.
	//
	// Refuse when the caller NAMED the port (rule 1's own reasoning: zeroing a
	// port someone asked for in the same breath would be a lie), clear it
	// otherwise (the completion of an instruction, not a contradiction of one).
	if app.ProxyExcluded && app.ProxyListenPort != 0 {
		if explicitProxyListenPort != 0 {
			return ErrApplicationProxyExcludedPortConflict
		}
		app.ProxyListenPort = 0
	}
	return nil
}

// warnProxyExclusionOwnTLS is loud about the one genuinely dangerous shape this
// feature can produce: an application that WAS behind the gateway's TLS proxy,
// is now excluded, and is left on https. The gateway addresses it on its own
// port from this moment on and expects it to terminate TLS there itself -- and
// nothing reverts that, by design (the gateway never writes a scheme on the
// exclusion path; see ADR-030).
//
// A refusal was considered and rejected: the portal always sends scheme, so a
// "participation change must carry a scheme" rule would be ceremony there, and
// an API caller could only satisfy it by re-sending what it already has. Being
// loud on the dangerous shape is this branch's posture instead.
//
// It also names the RELEASED port, which is not decoration: the port goes back
// to the free pool immediately (a sibling can draw it on the next routes fetch)
// and re-including this application later draws a fresh lowest-free port, not
// this one. An operator with a hand-written firewall rule pinned to it has to
// know the number.
func warnProxyExclusionOwnTLS(server routing.AIServer, app routing.Application, previousProxyListenPort int) {
	if !app.ProxyExcluded || previousProxyListenPort == 0 || effectiveScheme(app) != "https" {
		return
	}
	slog.Warn("application excluded from the gateway TLS proxy while staying on https; the gateway now addresses it on its OWN port and expects it to terminate TLS there",
		"server", server.ID,
		"server_name", server.Name,
		"app", app.ID,
		"app_type", app.Type,
		"released_proxy_listen_port", previousProxyListenPort,
		"app_port", app.Port,
		"action", "serve TLS on this application's own port with a leaf valid for "+server.Domain+", trusted by the system store or the gateway's internal CA; the released proxy port returns to the free pool and re-including the application later draws a fresh one")
}

// normalizeApplicationProxyListenPort validates the gateway-managed TLS
// proxy-listen port: 0 ("auto-assign", the default) is always allowed; a
// non-zero value must be a valid TCP port (1..65535).
func normalizeApplicationProxyListenPort(port int) (int, error) {
	if port == 0 {
		return 0, nil
	}
	if port < 1 || port > 65535 {
		return 0, ErrApplicationProxyListenPortInvalid
	}
	return port, nil
}

// proxyListenPortTakenOnServer reports whether a non-zero port is already
// used as another application's ProxyListenPort on serverID, excluding
// excludeAppID (used when updating an application to keep its own port).
// port == 0 ("auto-assign"/unassigned) never conflicts — many applications
// may share the unassigned state before the gateway auto-assigns each one.
func (s *Service) proxyListenPortTakenOnServer(ctx context.Context, serverID string, port int, excludeAppID string) (bool, error) {
	if port == 0 {
		return false, nil
	}
	apps, err := s.routes.ApplicationsByServer(ctx, serverID)
	if err != nil {
		return false, err
	}
	for _, app := range apps {
		if app.ID == excludeAppID {
			continue
		}
		if app.ProxyListenPort == port {
			return true, nil
		}
	}
	return false, nil
}

// serverAgentApplicationExistsOnServer reports whether serverID already has a
// routing.ProviderServerAgent application other than excludeAppID (pass "" on
// the create path; the updated application's own id on the update path, so a
// no-op retype of the server's own server_agent application is not a
// self-collision).
//
// Mirrors proxyListenPortTakenOnServer above: one ApplicationsByServer read,
// no store-layer support needed. Callers gate on the requested type first, so
// the extra read only happens for a write that actually targets
// server_agent. See ErrServerAgentApplicationExists for why the invariant
// matters.
//
// NOT race-free, by construction: this reads, returns, and the caller then
// calls Create/UpdateApplication in no transaction, so two concurrent POSTs
// can both pass it. This is the gate that produces the HONEST error code, not
// the one that guarantees the invariant -- that is the store's job (migration
// 68's partial unique index on SQL, MemoryStore's
// serverAgentApplicationExistsLocked on memory), and the loser of the race
// gets its ErrConflict classified back into the honest sentinel by
// classifyApplicationWriteConflict.
func (s *Service) serverAgentApplicationExistsOnServer(ctx context.Context, serverID, excludeAppID string) (bool, error) {
	apps, err := s.routes.ApplicationsByServer(ctx, serverID)
	if err != nil {
		return false, err
	}
	for _, app := range apps {
		if app.ID == excludeAppID {
			continue
		}
		if app.Type == routing.ProviderServerAgent {
			return true, nil
		}
	}
	return false, nil
}

// classifyApplicationWriteConflict turns the store's opaque ErrConflict from
// Create/UpdateApplication into the sentinel that names the condition which
// actually holds. `app` is the application as it was written (Type/Port/
// ServerID post-mutation on the update path); excludeAppID is "" on create
// and the application's own id on update.
//
// Two constraints on `applications` can produce ErrConflict, and they mean
// completely different things to an operator: unique(server_id, port), and
// migration 68's partial unique index on (server_id) where type =
// 'server_agent' (MemoryStore.serverAgentApplicationExistsLocked on the
// memory driver). Reporting the port code for the second one told the
// operator "application port already in use" on a request where no port
// collided.
//
// Classified by RE-READING the server's applications rather than by parsing
// the driver's error text: sqlite, postgres and memory all surface the same
// bare store.ErrConflict with no constraint name, and the text that does
// exist is dialect-specific. The read is only paid on a request that has
// already failed.
//
// Port first, deliberately: it is the constraint MemoryStore checks first,
// SQL leaves the order undefined when both hold, and a request that really
// does collide on a port must keep hearing so. When neither condition is
// visible (a duplicate id -- unreachable with 32 hex of randomness -- or a
// read failure) the answer stays ErrApplicationConflict, the behaviour before
// this classification existed.
func (s *Service) classifyApplicationWriteConflict(ctx context.Context, app routing.Application, excludeAppID string) error {
	apps, err := s.routes.ApplicationsByServer(ctx, app.ServerID)
	if err != nil {
		return ErrApplicationConflict
	}
	serverAgentTaken := false
	for _, existing := range apps {
		if existing.ID == excludeAppID {
			continue
		}
		if existing.Port == app.Port {
			return ErrApplicationConflict
		}
		if existing.Type == routing.ProviderServerAgent {
			serverAgentTaken = true
		}
	}
	if app.Type == routing.ProviderServerAgent && serverAgentTaken {
		return ErrServerAgentApplicationExists
	}
	return ErrApplicationConflict
}

func normalizeApplicationFlavors(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}, nil
	}
	out := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for _, candidate := range raw {
		flavor := strings.TrimSpace(candidate)
		switch flavor {
		case routing.APIFlavorOpenAI, routing.APIFlavorAnthropic:
		default:
			return nil, ErrApplicationFlavorInvalid
		}
		if _, dup := seen[flavor]; dup {
			continue
		}
		seen[flavor] = struct{}{}
		out = append(out, flavor)
	}
	return out, nil
}

func normalizeApplicationStatus(raw string) (string, error) {
	status := strings.TrimSpace(raw)
	if status == "" {
		return routing.ServerStatusActive, nil
	}
	switch status {
	case routing.ServerStatusActive, routing.ServerStatusDisabled:
		return status, nil
	default:
		return "", ErrApplicationStatusInvalid
	}
}

// validateApplicationTuning rejects negative routing-tuning values. Zero is
// permitted (create/update apply the established defaults for timeout/affinity;
// 0 admission-queue timeout is a valid "wait until the client aborts").
func validateApplicationTuning(priority, weight, timeoutMS, affinityTTLSeconds, admissionQueueTimeoutSeconds int) error {
	if priority < 0 || weight < 0 || timeoutMS < 0 || affinityTTLSeconds < 0 || admissionQueueTimeoutSeconds < 0 {
		return ErrApplicationTuningInvalid
	}
	return nil
}

// normalizeApplicationTimeoutMS maps a zero TimeoutMS to the type-appropriate
// default: defaultServerAgentTimeoutMS for a server_agent application (cold
// model loads can take minutes -- see defaultServerAgentTimeoutMS), or
// defaultApplicationTimeoutMS for every other type. A non-zero value is
// always preserved as given. Both CreateApplication and UpdateApplication
// call this -- UpdateApplication passes the application's own (already
// mutated, if req.Type changed in the same request) type so that a PATCH
// combining a retype to/from server_agent with timeout_ms:0 applies the
// NEW type's default rather than a stale one.
func normalizeApplicationTimeoutMS(appType string, timeoutMS int) int {
	if timeoutMS != 0 {
		return timeoutMS
	}
	if appType == routing.ProviderServerAgent {
		return defaultServerAgentTimeoutMS
	}
	return defaultApplicationTimeoutMS
}

func normalizeApplicationAffinityTTLSeconds(affinityTTLSeconds int) int {
	if affinityTTLSeconds == 0 {
		return defaultApplicationAffinityTTLSeconds
	}
	return affinityTTLSeconds
}

// ModelMappingDTO is the portal-facing representation of a routing.ModelMapping.
type ModelMappingDTO struct {
	ID                           string     `json:"id"`
	ApplicationID                string     `json:"application_id"`
	GatewayModelName             string     `json:"gateway_model_name"`
	AppModelName                 string     `json:"app_model_name"`
	Status                       string     `json:"status"`
	GenTokensPerSecond           float64    `json:"gen_tokens_per_second"`
	PromptTokensPerSecond        float64    `json:"prompt_tokens_per_second"`
	LoadTimeMS                   int        `json:"load_time_ms"`
	ContextSize                  int        `json:"context_size"`
	MaxConcurrency               int        `json:"max_concurrency"`
	RecommendedConcurrency       int        `json:"recommended_concurrency"`
	GenTokensPerSecondAtCapacity float64    `json:"gen_tokens_per_second_at_capacity"`
	IsMtp                        bool       `json:"is_mtp"`
	VisionCapable                bool       `json:"vision_capable"`
	EnergyWhPerToken             float64    `json:"energy_wh_per_token"`
	MetricsLocked                bool       `json:"metrics_locked"`
	MetricsSource                string     `json:"metrics_source"`
	MetricsUpdatedAt             *time.Time `json:"metrics_updated_at,omitempty"`
	CreatedAt                    time.Time  `json:"created_at"`
}

type MappingListResponse struct {
	Data []ModelMappingDTO `json:"data"`
}

type CreateMappingRequest struct {
	GatewayModelName             string  `json:"gateway_model_name"`
	AppModelName                 string  `json:"app_model_name"`
	Status                       string  `json:"status"`
	GenTokensPerSecond           float64 `json:"gen_tokens_per_second"`
	PromptTokensPerSecond        float64 `json:"prompt_tokens_per_second"`
	LoadTimeMS                   int     `json:"load_time_ms"`
	ContextSize                  int     `json:"context_size"`
	MaxConcurrency               int     `json:"max_concurrency"`
	RecommendedConcurrency       int     `json:"recommended_concurrency"`
	GenTokensPerSecondAtCapacity float64 `json:"gen_tokens_per_second_at_capacity"`
	IsMTP                        bool    `json:"is_mtp"`
	VisionCapable                bool    `json:"vision_capable"`
	EnergyWhPerToken             float64 `json:"energy_wh_per_token"`
	MetricsLocked                bool    `json:"metrics_locked"`
}

type UpdateMappingRequest struct {
	GatewayModelName             *string  `json:"gateway_model_name,omitempty"`
	AppModelName                 *string  `json:"app_model_name,omitempty"`
	Status                       *string  `json:"status,omitempty"`
	GenTokensPerSecond           *float64 `json:"gen_tokens_per_second,omitempty"`
	PromptTokensPerSecond        *float64 `json:"prompt_tokens_per_second,omitempty"`
	LoadTimeMS                   *int     `json:"load_time_ms,omitempty"`
	ContextSize                  *int     `json:"context_size,omitempty"`
	MaxConcurrency               *int     `json:"max_concurrency,omitempty"`
	RecommendedConcurrency       *int     `json:"recommended_concurrency,omitempty"`
	GenTokensPerSecondAtCapacity *float64 `json:"gen_tokens_per_second_at_capacity,omitempty"`
	IsMTP                        *bool    `json:"is_mtp,omitempty"`
	VisionCapable                *bool    `json:"vision_capable,omitempty"`
	EnergyWhPerToken             *float64 `json:"energy_wh_per_token,omitempty"`
	MetricsLocked                *bool    `json:"metrics_locked,omitempty"`
}

// SyncResultDTO summarizes a SyncApplicationModels reconciliation.
type SyncResultDTO struct {
	Added      int `json:"added"`
	Disabled   int `json:"disabled"`
	Unchanged  int `json:"unchanged"`
	Conflicted int `json:"conflicted"`
}

// ListMappings returns every mapping under appID for an owner-or-admin principal.
func (s *Service) ListMappings(ctx context.Context, principal auth.Token, appID string) (MappingListResponse, error) {
	app, _, err := s.authorizeApplication(ctx, principal, appID)
	if err != nil {
		return MappingListResponse{}, err
	}
	mappings, err := s.routes.MappingsByApplication(ctx, app.ID)
	if err != nil {
		return MappingListResponse{}, err
	}
	out := make([]ModelMappingDTO, 0, len(mappings))
	for _, mapping := range mappings {
		out = append(out, mappingDTO(mapping))
	}
	return MappingListResponse{Data: out}, nil
}

// CreateMapping validates and persists a new mapping under appID.
func (s *Service) CreateMapping(ctx context.Context, principal auth.Token, appID string, req CreateMappingRequest) (ModelMappingDTO, error) {
	app, server, err := s.authorizeApplication(ctx, principal, appID)
	if err != nil {
		return ModelMappingDTO{}, err
	}
	gatewayName := strings.TrimSpace(req.GatewayModelName)
	if gatewayName == "" {
		return ModelMappingDTO{}, ErrMappingGatewayNameRequired
	}
	appModelName := strings.TrimSpace(req.AppModelName)
	if appModelName == "" {
		return ModelMappingDTO{}, ErrMappingAppNameRequired
	}
	status, err := normalizeMappingStatus(req.Status)
	if err != nil {
		return ModelMappingDTO{}, err
	}
	if err := validateMappingMetrics(mappingMetrics{
		genTPS: req.GenTokensPerSecond, promptTPS: req.PromptTokensPerSecond,
		loadMS: req.LoadTimeMS, contextSize: req.ContextSize,
		maxConcurrency: req.MaxConcurrency, recommendedConcurrency: req.RecommendedConcurrency,
		genTPSAtCapacity: req.GenTokensPerSecondAtCapacity, energyWhPerToken: req.EnergyWhPerToken,
	}); err != nil {
		return ModelMappingDTO{}, err
	}
	taken, err := s.gatewayNameTakenOnServer(ctx, server.ID, gatewayName, "")
	if err != nil {
		return ModelMappingDTO{}, err
	}
	if taken {
		return ModelMappingDTO{}, ErrMappingGatewayNameConflict
	}
	// A mapping must not take the name of a model group (global models ∪ groups
	// namespace uniqueness — a group name is offered as a synthetic model).
	groupTaken, err := s.groupNameExists(ctx, gatewayName)
	if err != nil {
		return ModelMappingDTO{}, err
	}
	if groupTaken {
		return ModelMappingDTO{}, ErrMappingGatewayNameConflict
	}
	now := s.clock().UTC()
	// Default IsMTP from the model NAME when the caller did not explicitly set it.
	// A name-derived default is NOT a measurement, so it must NOT drive the
	// provenance stamp below (metricValuesPresent keeps seeing the RAW req.IsMTP).
	isMTP := req.IsMTP
	if !isMTP {
		isMTP = routing.IsMTPModelName(appModelName)
	}
	mapping := routing.ModelMapping{
		ID:                           "map_" + compactRandomHex(16),
		ApplicationID:                app.ID,
		GatewayModelName:             gatewayName,
		AppModelName:                 appModelName,
		Status:                       status,
		GenTokensPerSecond:           req.GenTokensPerSecond,
		PromptTokensPerSecond:        req.PromptTokensPerSecond,
		LoadTimeMS:                   req.LoadTimeMS,
		ContextSize:                  req.ContextSize,
		MaxConcurrency:               req.MaxConcurrency,
		RecommendedConcurrency:       req.RecommendedConcurrency,
		GenTokensPerSecondAtCapacity: req.GenTokensPerSecondAtCapacity,
		IsMTP:                        isMTP,
		VisionCapable:                req.VisionCapable,
		EnergyWhPerToken:             req.EnergyWhPerToken,
		MetricsLocked:                req.MetricsLocked,
		CreatedAt:                    now,
		UpdatedAt:                    now,
	}
	if metricValuesPresent(mappingMetrics{
		genTPS: req.GenTokensPerSecond, promptTPS: req.PromptTokensPerSecond,
		loadMS: req.LoadTimeMS, contextSize: req.ContextSize,
		maxConcurrency: req.MaxConcurrency, recommendedConcurrency: req.RecommendedConcurrency,
		genTPSAtCapacity: req.GenTokensPerSecondAtCapacity, energyWhPerToken: req.EnergyWhPerToken,
	}, req.IsMTP, req.VisionCapable) {
		mapping.MetricsSource = "manual"
		mapping.MetricsUpdatedAt = &now
	}
	if err := s.routes.CreateMapping(ctx, mapping); err != nil {
		return ModelMappingDTO{}, err
	}
	// Best-effort, after the successful store write: a mapping under the
	// server_agent application is a runtime-config input (its two model-name
	// fields are a spec's model/upstream_model). See
	// notifyRuntimeChangedForMapping -- the gate is the owning application's
	// type, not which field this request set.
	s.notifyRuntimeChangedForMapping(server.ID, app.Type)
	return mappingDTO(mapping), nil
}

// UpdateMapping partially updates a mapping, re-validating any changed fields.
func (s *Service) UpdateMapping(ctx context.Context, principal auth.Token, mappingID string, req UpdateMappingRequest) (ModelMappingDTO, error) {
	mapping, app, server, err := s.authorizeMapping(ctx, principal, mappingID)
	if err != nil {
		return ModelMappingDTO{}, err
	}
	// Validate everything that can fail BEFORE mutating the loaded mapping.
	var gatewayName, appModelName, status string
	if req.GatewayModelName != nil {
		gatewayName = strings.TrimSpace(*req.GatewayModelName)
		if gatewayName == "" {
			return ModelMappingDTO{}, ErrMappingGatewayNameRequired
		}
	}
	if req.AppModelName != nil {
		appModelName = strings.TrimSpace(*req.AppModelName)
		if appModelName == "" {
			return ModelMappingDTO{}, ErrMappingAppNameRequired
		}
	}
	if req.Status != nil {
		status, err = normalizeMappingStatus(*req.Status)
		if err != nil {
			return ModelMappingDTO{}, err
		}
	}
	if req.GatewayModelName != nil {
		taken, err := s.gatewayNameTakenOnServer(ctx, server.ID, gatewayName, mapping.ID)
		if err != nil {
			return ModelMappingDTO{}, err
		}
		if taken {
			return ModelMappingDTO{}, ErrMappingGatewayNameConflict
		}
		// A mapping must not take the name of a model group (global uniqueness).
		groupTaken, err := s.groupNameExists(ctx, gatewayName)
		if err != nil {
			return ModelMappingDTO{}, err
		}
		if groupTaken {
			return ModelMappingDTO{}, ErrMappingGatewayNameConflict
		}
		mapping.GatewayModelName = gatewayName
	}
	if req.AppModelName != nil {
		mapping.AppModelName = appModelName
	}
	if req.Status != nil {
		mapping.Status = status
	}
	// Compute the effective post-patch numeric metrics (start from the loaded
	// mapping, override with any supplied pointer), validate them, then apply.
	effGen := mapping.GenTokensPerSecond
	effPrompt := mapping.PromptTokensPerSecond
	effLoad := mapping.LoadTimeMS
	effCtx := mapping.ContextSize
	effMaxConc := mapping.MaxConcurrency
	effRecConc := mapping.RecommendedConcurrency
	effGenCap := mapping.GenTokensPerSecondAtCapacity
	effEnergy := mapping.EnergyWhPerToken
	if req.GenTokensPerSecond != nil {
		effGen = *req.GenTokensPerSecond
	}
	if req.PromptTokensPerSecond != nil {
		effPrompt = *req.PromptTokensPerSecond
	}
	if req.LoadTimeMS != nil {
		effLoad = *req.LoadTimeMS
	}
	if req.ContextSize != nil {
		effCtx = *req.ContextSize
	}
	if req.MaxConcurrency != nil {
		effMaxConc = *req.MaxConcurrency
	}
	if req.RecommendedConcurrency != nil {
		effRecConc = *req.RecommendedConcurrency
	}
	if req.GenTokensPerSecondAtCapacity != nil {
		effGenCap = *req.GenTokensPerSecondAtCapacity
	}
	if req.EnergyWhPerToken != nil {
		effEnergy = *req.EnergyWhPerToken
	}
	if err := validateMappingMetrics(mappingMetrics{
		genTPS: effGen, promptTPS: effPrompt,
		loadMS: effLoad, contextSize: effCtx,
		maxConcurrency: effMaxConc, recommendedConcurrency: effRecConc,
		genTPSAtCapacity: effGenCap, energyWhPerToken: effEnergy,
	}); err != nil {
		return ModelMappingDTO{}, err
	}
	mapping.GenTokensPerSecond = effGen
	mapping.PromptTokensPerSecond = effPrompt
	mapping.LoadTimeMS = effLoad
	mapping.ContextSize = effCtx
	mapping.MaxConcurrency = effMaxConc
	mapping.RecommendedConcurrency = effRecConc
	mapping.GenTokensPerSecondAtCapacity = effGenCap
	mapping.EnergyWhPerToken = effEnergy
	if req.IsMTP != nil {
		mapping.IsMTP = *req.IsMTP
	}
	if req.VisionCapable != nil {
		mapping.VisionCapable = *req.VisionCapable
	}
	if req.MetricsLocked != nil {
		mapping.MetricsLocked = *req.MetricsLocked
	}
	// A MEASURED-value change stamps provenance; a MetricsLocked-only change (policy,
	// not a measurement) deliberately does NOT.
	metricValueChanged := req.GenTokensPerSecond != nil || req.PromptTokensPerSecond != nil || req.LoadTimeMS != nil || req.ContextSize != nil || req.MaxConcurrency != nil || req.RecommendedConcurrency != nil || req.GenTokensPerSecondAtCapacity != nil || req.EnergyWhPerToken != nil || req.IsMTP != nil || req.VisionCapable != nil
	if metricValueChanged {
		now2 := s.clock().UTC()
		mapping.MetricsSource = "manual"
		mapping.MetricsUpdatedAt = &now2
	}
	mapping.UpdatedAt = s.clock().UTC()
	if err := s.routes.UpdateMapping(ctx, mapping); err != nil {
		return ModelMappingDTO{}, err
	}
	// Best-effort, after the successful store write: renaming a mapping under
	// the server_agent application rewrites its spec's model/upstream_model in
	// the agent's document, and without this the new gateway model name 404s at
	// the agent's router for up to a minute while the old one still routes. See
	// notifyRuntimeChangedForMapping.
	s.notifyRuntimeChangedForMapping(server.ID, app.Type)
	return mappingDTO(mapping), nil
}

// DeleteMapping removes the mapping.
func (s *Service) DeleteMapping(ctx context.Context, principal auth.Token, mappingID string) error {
	// app and server are captured here, BEFORE the delete: the notification
	// below needs the owning application's type to gate on, and after the row
	// is gone there is nothing left to resolve it from (authorizeMapping walks
	// mapping -> application -> server).
	mapping, app, server, err := s.authorizeMapping(ctx, principal, mappingID)
	if err != nil {
		return err
	}
	if err := s.routes.DeleteMapping(ctx, mapping.ID); err != nil {
		return err
	}
	// Best-effort, after the successful store write: the store cascades the
	// mapping's runtime spec, its GPU rows and its co-residency pairs, so
	// deleting a mapping under the server_agent application removes a whole
	// spec from the agent's document. See notifyRuntimeChangedForMapping.
	s.notifyRuntimeChangedForMapping(server.ID, app.Type)
	return nil
}

// SyncApplicationModels calls the ModelLister for appID's upstream and
// reconciles the application's mappings against it. On any lister/fetch
// error, NO mapping changes are made and ErrApplicationSyncFailed is
// returned.
func (s *Service) SyncApplicationModels(ctx context.Context, principal auth.Token, appID string) (SyncResultDTO, error) {
	app, server, err := s.authorizeApplication(ctx, principal, appID)
	if err != nil {
		return SyncResultDTO{}, err
	}
	return s.reconcileApplicationModels(ctx, server, app)
}

// SyncApplicationModelsForApp reconciles an application's model mappings without
// a principal, for the background app-health probe loop (model_sync mode). The
// caller supplies the already-loaded server + application, so there is no
// re-load and no ownership check — it is a trusted, process-internal path. It
// returns ErrApplicationSyncFailed when the upstream model listing fails (no
// mapping changes in that case), mirroring the authorized SyncApplicationModels.
func (s *Service) SyncApplicationModelsForApp(ctx context.Context, server routing.AIServer, app routing.Application) (SyncResultDTO, error) {
	return s.reconcileApplicationModels(ctx, server, app)
}

// reconcileApplicationModels is the principal-free core shared by the authorized
// SyncApplicationModels and the system-level SyncApplicationModelsForApp: it
// lists the application's upstream models and reconciles the store mappings
// against them (add new, disable vanished). On any lister/fetch error it makes
// NO mapping changes and returns ErrApplicationSyncFailed (fail-closed).
func (s *Service) reconcileApplicationModels(ctx context.Context, server routing.AIServer, app routing.Application) (SyncResultDTO, error) {
	if s.models == nil {
		return SyncResultDTO{}, ErrApplicationSyncFailed
	}
	target := routing.Target{Provider: app.Type, Endpoint: routing.ApplicationEndpoint(server, app)}
	// Attach the app's stored upstream API token to the model-discovery call so an
	// auth-required upstream (model_sync mode OR the manual "Sync models" button) doesn't
	// 401 and drop the server out of routing. Fail-open: a decrypt failure yields an empty
	// token and WithUpstreamAuth no-ops (same tolerance as the gateway/probe paths).
	discoveryToken, _ := capture.OpenSecret(s.cipher, app.APIToken)
	ctx = provider.WithUpstreamAuth(ctx, app.APITokenHeader, discoveryToken)
	upstream, err := s.models.ListModels(ctx, target)
	if err != nil {
		return SyncResultDTO{}, ErrApplicationSyncFailed
	}
	// De-duplicate upstream model ids (preserve first-seen order, drop exact
	// repeats) so a malformed /v1/models or /api/tags response that lists the
	// same model twice does not create two mappings for one upstream model.
	// Model ids are case-sensitive identifiers, so compare by exact string.
	seen := make(map[string]struct{}, len(upstream))
	deduped := make([]string, 0, len(upstream))
	for _, name := range upstream {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		deduped = append(deduped, name)
	}
	upstream = deduped
	// Best-effort runtime notification for the FOURTH mapping write path (the
	// manual "Sync models" button and the background model_sync probe loop both
	// land here). Registered BEFORE the lock below so LIFO defer order runs it
	// AFTER reconcileMu is released, and deferred rather than tail-placed so a
	// reconcile that fails halfway still announces the writes it DID make --
	// under-notifying is the bug this whole rule exists to prevent. Gated on
	// having written anything at all: Added and Conflicted each mean one
	// CreateMapping, Disabled one UpdateMapping, Unchanged no write. See
	// notifyRuntimeChangedForMapping (including why this notifies even though
	// neither of the two writes this path makes can change the document today).
	var result SyncResultDTO
	defer func() {
		if result.Added+result.Conflicted+result.Disabled > 0 {
			s.notifyRuntimeChangedForMapping(server.ID, app.Type)
		}
	}()
	// Serialize the store-mutating critical section: the per-server gateway-name
	// uniqueness check below (gatewayNameTakenOnServer -> CreateMapping) is a
	// check-then-act with no DB constraint behind it, and the background
	// model_sync loop reconciles many applications concurrently. Without this,
	// two apps on one server serving the same model id would both see the name as
	// free and both create an ACTIVE mapping. ListModels already ran (above),
	// outside the lock, so only the fast local read+write serializes.
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	existing, err := s.routes.MappingsByApplication(ctx, app.ID)
	if err != nil {
		return SyncResultDTO{}, err
	}
	existingByAppName := make(map[string]routing.ModelMapping, len(existing))
	for _, mapping := range existing {
		existingByAppName[mapping.AppModelName] = mapping
	}
	upstreamSet := make(map[string]struct{}, len(upstream))
	for _, model := range upstream {
		upstreamSet[model] = struct{}{}
	}

	now := s.clock().UTC()
	for _, model := range upstream {
		if _, ok := existingByAppName[model]; ok {
			result.Unchanged++
			continue
		}
		taken, err := s.gatewayNameTakenOnServer(ctx, server.ID, model, "")
		if err != nil {
			return SyncResultDTO{}, err
		}
		// A discovered upstream model that collides with a model-group name is
		// created disabled (soft) rather than shadowing the group (global
		// uniqueness). Still under s.reconcileMu; ModelGroups is a fast local read.
		if !taken {
			groupTaken, err := s.groupNameExists(ctx, model)
			if err != nil {
				return SyncResultDTO{}, err
			}
			taken = groupTaken
		}
		status := routing.ServerStatusActive
		if taken {
			status = routing.ServerStatusDisabled
			result.Conflicted++
		} else {
			result.Added++
		}
		mapping := routing.ModelMapping{
			ID:               "map_" + compactRandomHex(16),
			ApplicationID:    app.ID,
			GatewayModelName: model,
			AppModelName:     model,
			Status:           status,
			IsMTP:            routing.IsMTPModelName(model),
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if err := s.routes.CreateMapping(ctx, mapping); err != nil {
			return SyncResultDTO{}, err
		}
	}
	for _, mapping := range existing {
		if mapping.Status != routing.ServerStatusActive {
			continue
		}
		if _, ok := upstreamSet[mapping.AppModelName]; ok {
			continue
		}
		mapping.Status = routing.ServerStatusDisabled
		mapping.UpdatedAt = now
		if err := s.routes.UpdateMapping(ctx, mapping); err != nil {
			return SyncResultDTO{}, err
		}
		result.Disabled++
	}
	return result, nil
}

// authorizeMapping loads the mapping, resolves its owning application and
// server, and authorizes the principal as admin-or-owner of that server. Any
// failure (missing mapping, missing application, missing server, failed
// authorization) collapses to ErrMappingNotFound so the caller never learns
// whether the mapping, its application, or its server exists (no existence
// leak).
func (s *Service) authorizeMapping(ctx context.Context, principal auth.Token, mappingID string) (routing.ModelMapping, routing.Application, routing.AIServer, error) {
	mappingID = strings.TrimSpace(mappingID)
	if mappingID == "" {
		return routing.ModelMapping{}, routing.Application{}, routing.AIServer{}, ErrMappingNotFound
	}
	mapping, err := s.routes.MappingByID(ctx, mappingID)
	if err != nil {
		return routing.ModelMapping{}, routing.Application{}, routing.AIServer{}, ErrMappingNotFound
	}
	app, server, err := s.authorizeApplication(ctx, principal, mapping.ApplicationID)
	if err != nil {
		return routing.ModelMapping{}, routing.Application{}, routing.AIServer{}, ErrMappingNotFound
	}
	return mapping, app, server, nil
}

// gatewayNameTakenOnServer reports whether gatewayName is already used by
// another mapping on serverID (case-insensitive, trimmed), excluding
// excludeMappingID (used when updating a mapping to keep its own name).
func (s *Service) gatewayNameTakenOnServer(ctx context.Context, serverID string, gatewayName string, excludeMappingID string) (bool, error) {
	mappings, err := s.routes.MappingsByServer(ctx, serverID)
	if err != nil {
		return false, err
	}
	target := strings.ToLower(strings.TrimSpace(gatewayName))
	for _, mapping := range mappings {
		if mapping.ID == excludeMappingID {
			continue
		}
		if strings.ToLower(strings.TrimSpace(mapping.GatewayModelName)) == target {
			return true, nil
		}
	}
	return false, nil
}

func mappingDTO(mapping routing.ModelMapping) ModelMappingDTO {
	return ModelMappingDTO{
		ID:                           mapping.ID,
		ApplicationID:                mapping.ApplicationID,
		GatewayModelName:             mapping.GatewayModelName,
		AppModelName:                 mapping.AppModelName,
		Status:                       mapping.Status,
		GenTokensPerSecond:           mapping.GenTokensPerSecond,
		PromptTokensPerSecond:        mapping.PromptTokensPerSecond,
		LoadTimeMS:                   mapping.LoadTimeMS,
		ContextSize:                  mapping.ContextSize,
		MaxConcurrency:               mapping.MaxConcurrency,
		RecommendedConcurrency:       mapping.RecommendedConcurrency,
		GenTokensPerSecondAtCapacity: mapping.GenTokensPerSecondAtCapacity,
		IsMtp:                        mapping.IsMTP,
		VisionCapable:                mapping.VisionCapable,
		EnergyWhPerToken:             mapping.EnergyWhPerToken,
		MetricsLocked:                mapping.MetricsLocked,
		MetricsSource:                mapping.MetricsSource,
		MetricsUpdatedAt:             mapping.MetricsUpdatedAt,
		CreatedAt:                    mapping.CreatedAt,
	}
}

// mappingMetrics bundles the eight numeric per-mapping metric values shared by
// validateMappingMetrics and metricValuesPresent below. CreateMapping builds
// one straight from the request; UpdateMapping builds one from its already
// pointer-merged "effective" values. isMTP/visionCapable stay separate
// parameters on the functions below rather than bundled fields here, because
// metricValuesPresent's caller deliberately passes the RAW req.IsMTP there
// (never the name-defaulted value written onto the mapping) -- see its call
// site's comment.
type mappingMetrics struct {
	genTPS, promptTPS                      float64
	loadMS, contextSize                    int
	maxConcurrency, recommendedConcurrency int
	genTPSAtCapacity, energyWhPerToken     float64
}

// validateMappingMetrics rejects negative metric values (zero = "unknown" is allowed).
func validateMappingMetrics(m mappingMetrics) error {
	if m.genTPS < 0 || m.promptTPS < 0 || m.loadMS < 0 || m.contextSize < 0 || m.maxConcurrency < 0 || m.recommendedConcurrency < 0 || m.genTPSAtCapacity < 0 || m.energyWhPerToken < 0 {
		return ErrMappingMetricInvalid
	}
	return nil
}

// metricValuesPresent reports whether any MEASURED value was supplied (the lock flag
// alone is policy, not a measurement, so it does not stamp provenance).
func metricValuesPresent(m mappingMetrics, isMTP, visionCapable bool) bool {
	return m.genTPS != 0 || m.promptTPS != 0 || m.loadMS != 0 || m.contextSize != 0 || m.maxConcurrency != 0 || m.recommendedConcurrency != 0 || m.genTPSAtCapacity != 0 || m.energyWhPerToken != 0 || isMTP || visionCapable
}

func normalizeMappingStatus(raw string) (string, error) {
	status := strings.TrimSpace(raw)
	if status == "" {
		return routing.ServerStatusActive, nil
	}
	switch status {
	case routing.ServerStatusActive, routing.ServerStatusDisabled:
		return status, nil
	default:
		return "", ErrMappingStatusInvalid
	}
}
