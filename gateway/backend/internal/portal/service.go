// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/certissue"
	"op-ai-gateway/internal/netbird"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/theme"
	"op-ai-gateway/internal/usage"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Code* constants are the API error codes of the like-named sentinels below,
// exported so the gateway's error mapper/handlers (internal/gateway package)
// can share the exact value instead of re-hardcoding a copy that could drift
// from the sentinel's own .Error() string.
const (
	CodeTokenNotFound   = "portal.token_not_found"
	CodeServerNotFound  = "server.not_found"
	CodeServiceNotFound = "service.not_found"
)

var (
	ErrTokenNameRequired         = errors.New("portal.token_name_required")
	ErrTokenScopeInvalid         = errors.New("portal.token_scope_invalid")
	ErrTokenScopeForbidden       = errors.New("portal.token_scope_forbidden")
	ErrTokenRequired             = errors.New("portal.token_required")
	ErrTokenForbidden            = errors.New("portal.token_forbidden")
	ErrTokenNameConflict         = errors.New("portal.token_name_conflict")
	ErrTokenNotFound             = errors.New(CodeTokenNotFound)
	ErrTokenStatusInvalid        = errors.New("portal.token_status_invalid")
	ErrTokenModelOverrideInvalid = errors.New("portal.token_model_override_invalid")
	ErrTokenNotDeletable         = errors.New("token.not_deletable")

	ErrServerNotFound       = errors.New(CodeServerNotFound)
	ErrServerForbidden      = errors.New("server.forbidden")
	ErrServerNameRequired   = errors.New("server.name_required")
	ErrServerDomainRequired = errors.New("server.domain_required")
	ErrServerStatusInvalid  = errors.New("server.status_invalid")
	ErrServerOwnerInvalid   = errors.New("server.owner_invalid")
	// ErrServerAgentPresenceTimeoutInvalid rejects a negative per-server
	// agent_presence_timeout_seconds override (0 = follow the system default).
	ErrServerAgentPresenceTimeoutInvalid = errors.New("server.agent_presence_timeout_invalid")
	// ErrServerEnergyConfigInvalid rejects a negative value for any of the four
	// per-server energy-config fields (estimated_watts/idle_watts/price_per_kwh/pue —
	// 0 = unset / use default).
	ErrServerEnergyConfigInvalid = errors.New("server.energy_config_invalid")

	// Admin-group linkage (server WRITE path, Phase B, spec 2026-08-10).
	// ErrServerAdminGroupRequired: the (post-dedup) admin_group_ids set is
	// empty -- every server, regardless of the creating/editing principal's
	// scope, must be linked to at least one admin-tier group.
	// ErrServerAdminGroupInvalid: an id does not resolve to an existing
	// ADMIN-tier group, or (for a non-system principal) is not one the
	// principal may manage servers through (serverManageGroupIDs).
	// ErrServerAdminGroupParentMismatch: the chosen groups do not all share
	// ONE parent (system-tier) group, or (system-scope only) contradict an
	// explicitly-supplied SystemGroupID cross-check.
	ErrServerAdminGroupRequired       = errors.New("server.admin_group_required")
	ErrServerAdminGroupInvalid        = errors.New("server.admin_group_invalid")
	ErrServerAdminGroupParentMismatch = errors.New("server.admin_group_parent_mismatch")

	// NetBird sentinels. ModuleDisabled = the module is off / not configured;
	// NotEnabled = the target server is not a NetBird server; NotConfigured is the
	// TestNetbird "no config" case.
	ErrNetbirdModuleDisabled = errors.New("netbird.module_disabled")
	ErrNetbirdNotConfigured  = errors.New("netbird.not_configured")
	ErrNetbirdPeerInUse      = errors.New("netbird.peer_in_use")
	// ErrNetbirdPeerNotManaged blocks a key regenerate on a server whose existing
	// peer is NOT gateway-managed (a manually-linked / externally-used peer) — so
	// the proactive delete-on-regenerate can never delete a foreign peer.
	ErrNetbirdPeerNotManaged = errors.New("netbird.peer_not_managed")
	// ErrNetbirdKeyFileNotConfigured is returned by EnrollGatewaySidecar when no
	// shared-volume key file is configured (OP_AI_GATEWAY_NETBIRD_KEY_FILE unset) —
	// the autonomous sidecar-enroll feature is off. Mapped to a 409 at the handler.
	ErrNetbirdKeyFileNotConfigured = errors.New("netbird.key_file_not_configured")

	ErrPreferenceKeyRequired = errors.New("portal.preference_key_required")

	// Service Accounts (Phase 1). ErrServiceNotFound covers both "no such
	// service" AND "principal is not authorized to see it" (no existence
	// leak, mirrors ErrServerNotFound) — including a token that does not
	// belong to the addressed service (no cross-service leak either).
	// ErrServiceForbidden is CreateService's admin-only gate.
	// ErrServiceValidation covers every request-shape/reference problem
	// (blank/duplicate name or delegate, unknown delegate user id, unknown
	// allowlist model, half-filled rows) — one generic 400-mapped sentinel,
	// mirroring the single ErrTokenModelOverrideInvalid used for the
	// analogous token-override validation.
	ErrServiceNotFound   = errors.New(CodeServiceNotFound)
	ErrServiceForbidden  = errors.New("service.forbidden")
	ErrServiceValidation = errors.New("service.validation_failed")
)

// ChatSessionTokenID is the sentinel id of the synthetic, non-deletable
// ChatSession row prepended to ListTokens. It represents token-less session
// chat and carries the user's profile chat flags; it is never a real token, so
// DeleteToken/UpdateToken reject it.
const ChatSessionTokenID = "chat-session"

type UserReader interface {
	UserByID(ctx context.Context, id string) (store.User, error)
	ListUsers(ctx context.Context) ([]store.User, error)
}

type TokenRepository interface {
	TokensByUser(ctx context.Context, userID string) ([]store.TokenRecord, error)
	// TokensByService lists a Service Account's tokens (kind="service"),
	// newest first (Phase 1 service accounts). Satisfied by *store.SQLiteStore
	// (used for both the sqlite and postgres drivers) and *MemoryDirectory.
	TokensByService(ctx context.Context, serviceID string) ([]store.TokenRecord, error)
	// TokensByProject lists a project's assigned USER tokens, newest first
	// (owner/admin project-tokens view). Satisfied by *store.SQLiteStore
	// (both sqlite and postgres drivers) and *MemoryDirectory.
	TokensByProject(ctx context.Context, projectID string) ([]store.TokenRecord, error)
	TokenByID(ctx context.Context, id string) (store.TokenRecord, error)
	CreatePlainToken(ctx context.Context, token store.TokenRecord, secret string) error
	UpdateTokenMetadata(ctx context.Context, token store.TokenRecord) error
	DeleteToken(ctx context.Context, id string) error
	RotateTokenSecret(ctx context.Context, id, secretHash, secretPrefix string, updatedAt time.Time) error
}

// SystemSettingsStore persists portal-wide system settings as key/value pairs.
type SystemSettingsStore interface {
	SystemSettings(ctx context.Context) (map[string]string, error)
	SetSystemSetting(ctx context.Context, key, value string, now time.Time) error
}

// UIPreferencesStore persists per-user UI preferences as opaque key/JSON-value
// pairs. It mirrors SystemSettingsStore but is scoped to a single user.
type UIPreferencesStore interface {
	UIPreferences(ctx context.Context, userID string) ([]store.UserUIPreference, error)
	SetUIPreference(ctx context.Context, userID, key, valueJSON string) error
}

// ChatStore persists per-user chat-playground conversations: plaintext title +
// timestamps and ONE opaque sealed/plain blob. The store is a dumb byte store —
// sealing/opening (with the shared capture Cipher) happens in the portal chat
// service, so the store stays agnostic to the blob's content. Satisfied by
// *store.SQLiteStore (persistent) and *store.MemoryChatStore (volatile RAM
// fallback when no encryption key is configured).
type ChatStore interface {
	CreateChat(ctx context.Context, chat store.Chat) error
	UpdateChat(ctx context.Context, chat store.Chat) error
	ChatByID(ctx context.Context, id string) (store.ChatRow, error)
	ChatsByUser(ctx context.Context, userID string) ([]store.ChatSummary, error)
	DeleteChat(ctx context.Context, id string) error
}

// CaptureReader reads a persisted capture blob for the detail endpoint (read
// side). Optional and fail-closed: a nil Captures disables capture detail (the
// read path nil-guards it). Decryption is handled by the Service's Cipher,
// added in P6.
type CaptureReader interface {
	Capture(ctx context.Context, usageEventID string) (store.CaptureRow, error)

	// HasCaptures resolves, for a batch of usage-event IDs, the presence
	// (secret + owner) of any stored capture. It is the store-agnostic
	// replacement for the old SQL `exists` column in usage_events Query:
	// Service.Usage calls it once per page instead of the query joining against
	// a specific captures table, so the Activity list's has_capture and
	// capture_locked flags are correct no matter which store backs Captures. An
	// empty ids does no lookup and returns an empty map; an id with no capture
	// is absent from the result.
	HasCaptures(ctx context.Context, ids []string) (map[string]store.CapturePresence, error)

	DeleteCapture(ctx context.Context, usageEventID string) error

	// SetCaptureSecret flips the secret flag on a capture. The owner-only gate
	// lives in Service.SetCaptureSecret; the store call is unconditional.
	SetCaptureSecret(ctx context.Context, usageEventID string, secret bool) error
}

// AppHealthReader exposes per-application reachability to the portal service so
// model offering can exclude applications whose reachability probe is failing,
// and the application DTO can be enriched with reachability metadata. A nil
// reader is lenient: every application is treated reachable and no enrichment
// occurs. Satisfied by *gateway.AppHealthRegistry.
type AppHealthReader interface {
	Reachable(appID string) bool
	ApplicationHealth(appID string) (reachable bool, lastCheckedAt time.Time, known bool)
}

// LoadedModelReader exposes which upstream model names are currently LOADED for
// an application on a server, so the model DTO can mark models that can be
// requested without waiting for a load/swap. A nil reader (or no data) means no
// model is marked loaded. Satisfied by *gateway.LoadedModelRegistry.
type LoadedModelReader interface {
	LoadedAppModels(appID, serverID string) []string
}

// AgentPresenceReader exposes whether a server's ServerAgent reported within a
// given (effective, per-server) window, so the server DTO can derive a live
// agent_status ("active"/"inactive"/"unconfigured"). A nil reader is treated
// as never-reporting (serverDTO falls back to "inactive"/"unconfigured" based
// on whether an agent token is configured). Satisfied by
// *gateway.AgentPresenceRegistry.
type AgentPresenceReader interface {
	ReportingWithin(serverID string, window time.Duration) bool
}

// AgentCertReportReader exposes what a server's ServerAgent last reported about the
// certificate it has INSTALLED (Phase 2 distribution), so the certificate list can
// show an "installed" state and the CA-rotation brake can tell whether a rotated
// root has propagated. ok=false means "never reported" — which every consumer must
// treat as "no claim" (fail open), NOT as "not installed". A nil reader always
// reports ok=false. Satisfied by *gateway.AgentCertReportRegistry.
type AgentCertReportReader interface {
	CertReport(serverID string) (fingerprint string, caFingerprints []string, mode string, notAfter time.Time, reportedAt time.Time, ok bool)
}

// AgentTransportReader exposes the most recent transport hop (TLS vs. plaintext)
// each ServerAgent used against the mesh listener, so the certificate list can
// render a per-server transport column. ok=false means "never observed" -- which
// consumers must render as "—", NOT as "plaintext". A nil reader always returns
// ok=false. Satisfied by *gateway.AgentTransportRegistry.
type AgentTransportReader interface {
	LatestTransport(serverID string) (transport string, at time.Time, ok bool)
}

type ServiceDeps struct {
	Users          UserReader
	Tokens         TokenRepository
	Usage          usage.Store
	Routes         routing.Store
	ModelLister    provider.ModelLister
	SystemSettings SystemSettingsStore
	UIPrefs        UIPreferencesStore
	Captures       CaptureReader
	Chats          ChatStore
	Reachability   AppHealthReader
	LoadedModels   LoadedModelReader
	// AgentPresence lets serverDTO derive a live agent_status per server (see
	// AgentPresenceReader). nil = every server reads as "inactive"/"unconfigured"
	// (never "active") — the same lenient-on-absence pattern as Reachability/LoadedModels.
	AgentPresence AgentPresenceReader
	// AgentCertReports lets the certificate list + the CA-rotation propagation
	// brakes read what each ServerAgent reports as installed/durably trusted (see
	// AgentCertReportReader). nil = never reported: the existing server-leaf brake
	// remains fail-open, while the bounded gateway-leaf fleet brake waits.
	AgentCertReports AgentCertReportReader
	// AgentTransport lets the certificate list surface the last observed mesh
	// hop per server (see AgentTransportReader), and the mesh gate use the same
	// registry as its arming precondition. nil = "never observed" -- the column
	// renders "—" and the gate can never arm.
	AgentTransport AgentTransportReader
	// ProxyStatus lets the https-auto-switch reconcile read what each ServerAgent
	// reports as ACTUALLY running on its local TLS-proxy routes (see
	// ProxyRouteStatusReader). nil = never observed: the reconcile makes no
	// forward and no revert (fail-safe on absence). Satisfied by an adapter over
	// *gateway.AgentProxyStatusRegistry (see cmd/gateway).
	ProxyStatus ProxyRouteStatusReader
	// Groups is the user-groups persistence contract (Task 3). nil is only
	// tolerated by test deps literals that never call VisibleUserIDs; every real
	// driver wires it (see cmd/gateway/main.go).
	Groups GroupStore
	// GroupCache is the in-memory model-group registry the resolver reads on the
	// hot path (built + wired in cmd/gateway). The Service refreshes it after any
	// group / model-setting write. nil = unwired (refresh is a no-op).
	GroupCache GroupCache
	// Projects is the projects persistence contract (Task 3). nil is only
	// tolerated by test deps literals that never call a project method; every
	// real driver wires it (see cmd/gateway/main.go).
	Projects ProjectStore
	Cipher   *capture.Cipher
	// CertCipher seals CERTIFICATE private keys at rest (leaf keys, the ACME
	// account key, the internal CA key) and NOTHING else -- it is built from its
	// own OP_AI_GATEWAY_CERT_ENCRYPTION_KEY, deliberately independent of Cipher
	// (the capture key, which also seals the SMTP password + the NetBird admin
	// token). nil = no certificate key configured: on a disk-backed store
	// sealCertSecret then refuses (ErrCertKeyRequired) rather than write a
	// private key in plaintext, and the reconcile records that in
	// cert_last_error; there is deliberately NO fallback to Cipher.
	CertCipher *capture.Cipher
	// CertEdgeOutputDir is where the gateway writes the edge certificate for its
	// own nginx (fullchain, key, CA root); see config.Config.CertEdgeOutputDir for
	// the full semantics. Empty (the default) means the gateway cannot deliver it
	// locally -- and THAT is what unlocks the key download endpoint. Deliberately
	// a deployment property threaded from config, not an operator setting.
	CertEdgeOutputDir string
	// CertEdgeProbeTarget is the host:port of the gateway's OWN edge (nginx)
	// TLS listener ProbeEdgeTLS dials for the synthetic self-probe; see
	// config.Config.CertEdgeProbeTarget for the full semantics. Empty (the
	// default) means ProbeEdgeTLS returns ErrEdgeProbeNotConfigured --
	// deliberately a deployment property threaded from config, not an operator
	// setting, exactly like CertEdgeOutputDir above.
	CertEdgeProbeTarget string
	// NetbirdKeyFile is the absolute path (OP_AI_GATEWAY_NETBIRD_KEY_FILE) the
	// EnrollGatewaySidecar action writes the minted setup key to for a waiting
	// NetBird sidecar. Empty = the autonomous sidecar-enroll feature is off.
	NetbirdKeyFile string
	// AgentPort is the effective TCP port the agent-ingest listener binds
	// (OP_AI_GATEWAY_AGENT_ADDR's port when set, else OP_AI_GATEWAY_AGENT_PORT,
	// default 8081). The managed op-gw-agent-ingest NetBird policy opens exactly
	// this port on the gateway group so server->gateway telemetry survives
	// deny-by-default. Empty defaults to "8081" in NewService.
	AgentPort string
	// AgentBindHost is the host component of an explicitly configured
	// OP_AI_GATEWAY_AGENT_ADDR. It is kept separate from AgentPort because the
	// self-signed gateway certificate may use an explicit IP as an IP SAN; an
	// empty value means listener addressing still comes from the NetBird peer.
	AgentBindHost string
	// AgentTLSPort is the effective port a later task's separate encrypted
	// agent listener would bind (config.Config.AgentTLSPort/AgentTLSAddr,
	// resolved to an int by cmd/gateway's effectiveAgentTLSPort). Read-only
	// display data surfaced as SystemSettingsDTO.CertMeshTLSPort; the zero
	// value (an omitted test deps literal) surfaces as 0, not the documented
	// "8443" env default -- every real driver in cmd/gateway wires a resolved
	// value explicitly.
	AgentTLSPort int
	// AgentTLSSeparateDefault is the env-fallback intent for the mesh
	// listener's separate-encrypted-port topology (config.Config.
	// AgentTLSSeparate), used by Service.CertMeshTLSSeparateActive whenever
	// the cert_mesh_tls_mode system setting is unset/blank/unknown ("").
	AgentTLSSeparateDefault bool
	// NetbirdTokenRotateBeforeDaysDefault is the env-derived fallback threshold
	// (days before expiry) for NetBird token auto-rotation when the KV is unset.
	// nil = not provided (NewService uses DefaultNetbirdTokenRotateBeforeDays, 14);
	// a non-nil value is used verbatim — including an explicit 0, which disables
	// auto-rotation (config.Load already resolves unset/junk/negative to 14, so any
	// 0 reaching here is an intentional operator opt-out that must not be clamped).
	NetbirdTokenRotateBeforeDaysDefault *int
	// AgentPresenceTimeoutDefault is the env-derived fallback (seconds) for the
	// agent_presence_timeout_seconds system setting when the KV is unset/invalid
	// (config.Config.AgentPresenceTimeoutSeconds). <= 0 (including the zero
	// value, so a test/deps literal that omits it needs no special-casing) falls
	// back to DefaultAgentPresenceTimeoutSeconds (15) in NewService.
	AgentPresenceTimeoutDefault int
	// OnNetbirdDomainChanged, when set, is invoked (best-effort) after a NetBird
	// account dns_domain change so the caller can trigger an immediate peer→domain
	// sync pass. Nil = no trigger (the periodic sync loop is the backstop).
	OnNetbirdDomainChanged func()
	// OnCertSettingsChanged, when set, is invoked (best-effort) after a settings
	// write that actually carried a certificate field (UpdateSystemSettingsRequest.
	// touchesCert), so the caller can trigger an immediate certificate reconcile
	// pass. Nil = no trigger (the periodic cert-reconcile loop is the backstop).
	//
	// WHY this exists (field report): the operator-facing note the certificate
	// panel renders is cert_last_error, and ONLY a reconcile pass writes or clears
	// it (see setCertLastError / clearCertLastError). Nothing else does -- and
	// deliberately so: clearing it on save would claim "all fine" before anything
	// re-derived the desired set, which is exactly the lie the note exists to
	// prevent. So without this hook a CORRECTIVE settings change (e.g. switching
	// cert_issuer_mode from "acme" with no acme_email to "self_signed") leaves the
	// old note standing -- asserting the very state the operator just fixed -- for
	// up to OP_AI_GATEWAY_CERT_RECONCILE_INTERVAL_SECONDS (default 900s).
	//
	// CONTRACT for the implementation: it MUST NOT block (the settings PUT calls
	// it inline, and a reconcile pass holds certMu for its whole duration and may
	// place ACME orders taking minutes) and it MUST NOT use the request's context
	// (cancelled the moment the response is written). cmd/gateway satisfies both
	// with a non-blocking send on a buffered(1) channel the cert-reconcile loop
	// selects on -- mirroring OnNetbirdDomainChanged.
	OnCertSettingsChanged func()
	// OnCertificateIssued, when set, is invoked (best-effort, SYNCHRONOUSLY)
	// right after issueAndStore persists a freshly issued kind="server"
	// certificate, with (ServerID, the leaf Fingerprint) -- the Phase 2
	// distribution doorbell: the caller pushes a cert_update frame to that
	// server's currently-open agent WebSocket connection(s) (see
	// gateway.AgentStreamRegistry.NotifyCertUpdate), so a waiting ServerAgent
	// can fetch its new certificate immediately instead of waiting for its next
	// poll/reconnect. Nil = no push (the agent's own poll/reconnect cadence is
	// the backstop).
	//
	// CONTRACT for the implementation: it MUST NOT block, and it MUST NOT
	// perform a network write inline -- this is called from inside
	// issueAndStore, which runs while ReconcileCertificates holds certMu for
	// the WHOLE reconcile pass (and may itself be mid-ACME-order for another
	// domain at the time). cmd/gateway's implementation only marshals the
	// frame (pure CPU) and does a non-blocking channel send per open
	// connection (AgentStreamRegistry.NotifyCertUpdate) -- never a socket
	// write, mirroring the non-blocking-send contract OnCertSettingsChanged
	// above already documents.
	OnCertificateIssued func(serverID, fingerprint string)
	// OnCABundleChanged rings every connected agent after a complete newCA
	// transaction publishes a new public root bundle. It must be non-blocking;
	// the periodic agent refresh is the delivery backstop.
	OnCABundleChanged func(fingerprint string)
	// ACMEChallenges is the HTTP-01 token store the ACME issuer publishes
	// key authorizations into (the gateway's /.well-known/acme-challenge/
	// handler serves from the same instance). nil = the acme issuer mode cannot
	// place an order (issueCertificate returns an error); the self_signed mode
	// needs no challenge store at all.
	ACMEChallenges certissue.ChallengeStore
	// SettingsVolatile is true only when the SystemSettings store is the
	// volatile in-memory store (memory driver). It gates the plaintext SMTP
	// password fallback: a disk store without a cipher refuses to store a
	// password (ErrSMTPKeyRequired) rather than write plaintext to disk.
	SettingsVolatile bool
	Clock            func() time.Time
	SecretGenerator  func() (string, error)
	IDGenerator      func() string
	// Themes is the registry of externally supplied, deployable theme
	// definitions loaded at startup from config.Config.ThemesDir (see
	// theme.Load, called by cmd/gateway). nil (an omitted test/deps literal,
	// or a startup load error) is treated as an empty registry by NewService
	// -- callers of Service.themes-backed methods never need their own nil
	// check.
	Themes *theme.Registry
}

// certState groups the certificate/edge-cert subsystem's fields that were
// previously flat on Service (PT-1 stage 1: a pure field-regroup, no method
// extraction -- see the individual field comments below, carried verbatim
// from their prior location on Service). It is held as a single non-pointer
// field (Service.cert): Service is always used via pointer, so mu/edgeMu/
// gwDNSMu here are never copied.
type certState struct {
	// certCipher: the certificate-only cipher (see ServiceDeps.CertCipher). Read
	// ONLY by sealCertSecret/openCertSecret — never a fallback for cipher, never
	// fed by it.
	cipher *capture.Cipher
	// certEdgeOutputDir: the local-delivery directory (see
	// ServiceDeps.CertEdgeOutputDir). Empty = the gateway cannot deliver the edge
	// certificate locally.
	edgeOutputDir string
	// certEdgeProbeTarget: the host:port ProbeEdgeTLS dials (see
	// ServiceDeps.CertEdgeProbeTarget). Empty = ProbeEdgeTLS is disabled
	// (ErrEdgeProbeNotConfigured).
	edgeProbeTarget string
	// acmeChallenges is the HTTP-01 token store shared with the gateway's
	// challenge handler (see ServiceDeps.ACMEChallenges).
	acmeChallenges certissue.ChallengeStore
	// certMu serializes the certificate reconcile pass against a manual CA
	// rotation: both read-then-write the runtime-managed cert_ca_* settings, and
	// an interleaving could publish a root whose key was already replaced.
	mu sync.Mutex
	// edgeMu guards ONLY edgeWritten/edgeWriteErr below. It is a SEPARATE mutex
	// from certMu on purpose: ReconcileCertificates holds certMu for the whole
	// pass and calls DeliverEdgeCertificate from inside it (see
	// service_edge_cert.go), and sync.Mutex is not reentrant -- taking certMu
	// there would deadlock the entire certificate subsystem. edgeMu never calls
	// anything that could itself try to take certMu, so there is no lock-order
	// inversion to worry about.
	edgeMu sync.Mutex
	// edgeWritten is the UTC time of the last successful DeliverEdgeCertificate
	// write, guarded by edgeMu. Zero until the first successful delivery.
	written time.Time
	// edgeWriteErr is the last delivery failure, guarded by edgeMu; "" means the
	// last attempt (if any) succeeded. EdgeDeliveryCapable reads this to decide
	// whether the key-download fallback endpoint may be used.
	writeErr string
	// certGWDNSMu/certGWDNSVal/certGWDNSExp are the TTL cache behind
	// cachedGatewayPeerDNS (service_edge_cert.go): the proxy-config GET needs the
	// gateway peer's live NetBird DNS name, and this keeps that GET free of a
	// NetBird call in the steady state. Mirrors the gateway layer's
	// agentDNSMu/agentDNSVal/agentDNSExp, which does the same for the agent-config
	// download GET. Independent of certMu/edgeMu (it takes neither).
	gwDNSMu  sync.Mutex
	gwDNSVal string
	gwDNSExp time.Time
	// certIssuer is the seam the reconcile obtains certificates through. It is
	// bound to issueCertificate in NewService; tests substitute a stub so the
	// reconcile can be driven without a live ACME directory. It takes the whole
	// desiredCert rather than just a domain because a row may carry SEVERAL names
	// (the edge row's SAN list) and its own issuer mode depends on its kind.
	issuer func(context.Context, CertSettings, desiredCert) (certissue.Result, error)
	// onCertSettingsChanged is the best-effort hook fired after a settings write
	// that carried a certificate field, so an immediate reconcile pass can run
	// instead of waiting for the periodic loop (see ServiceDeps).
	onSettingsChanged func()
	// onCertificateIssued is the best-effort doorbell hook fired after a
	// kind="server" certificate is (re-)issued and stored, so the caller can
	// push the agent a cert_update frame instead of waiting for its poll or
	// reconnect (see ServiceDeps).
	onIssued          func(serverID, fingerprint string)
	onCABundleChanged func(fingerprint string)
}

// netbirdState groups the NetBird-subsystem's fields that were previously flat
// on Service (PT-1 stage 2: a pure field-regroup, no method extraction -- see
// the individual field comments below, carried verbatim from their prior
// location on Service). It is held as a single non-pointer field
// (Service.netbird): Service is always used via pointer, so tokenMu/policyMu
// and the WaitGroups here are never copied.
type netbirdState struct {
	keyFile string
	// onNetbirdDomainChanged is the best-effort hook fired after
	// SetNetbirdNetwork changes the account dns_domain (see ServiceDeps).
	onDomainChanged func()
	// tokenRotateBeforeDefault is the resolved (env-fallback) auto-rotation
	// threshold used by NetbirdTokenRotateBeforeDays when the KV is unset.
	tokenRotateBeforeDefault int
	// netbirdTokenMu serializes RotateNetbirdToken (manual button + the
	// auto-rotation check share it, so a create+verify+switch is never
	// interleaved with a concurrent attempt).
	tokenMu sync.Mutex
	// nextTokenRotateAttempt is the cooldown gate for auto-rotation retries;
	// guarded by netbirdTokenMu.
	nextTokenRotateAttempt time.Time
	// netbirdPolicyMu serializes NetBird access-policy reconcile calls
	// (reconcileServerPolicy / reconcileAllServerPolicies / applyDenyByDefault).
	// These are now reachable from several independent, potentially concurrent
	// triggers (application CRUD hooks, the settings-PUT background fleet
	// reconcile, the per-server linkage editor) that each list-then-write NetBird
	// policies with no server-side uniqueness guard: two overlapping reconciles
	// could both observe "no existing policy named X" and both CreatePolicy it,
	// leaving a duplicate. Serializing the read-modify-write section here removes
	// that race; each call's own NetBird HTTP round trips are still the only cost
	// (no additional cross-goroutine blocking beyond the reconcile itself).
	policyMu sync.Mutex
	// policySideEffectWG tracks the background goroutine UpdateSystemSettings
	// spawns (see applyPolicySettingsSideEffects) so tests can deterministically
	// wait for it via waitPolicySideEffects instead of racing a fire-and-forget
	// goroutine against their own assertions. Production callers never wait on
	// it — the whole point of the goroutine is to not block the settings PUT.
	policySideEffectWG sync.WaitGroup
	// netbirdResolveWG tracks the background goroutine UpdateSystemSettings
	// spawns when a NetBird token is (re)stored (see resolveStoredTokenMeta) so
	// tests can deterministically wait for it via waitNetbirdResolve instead of
	// racing a fire-and-forget goroutine against their own assertions.
	// Production callers never wait on it — the whole point of the goroutine is
	// to not block the settings PUT.
	resolveWG sync.WaitGroup
}

type Service struct {
	users            UserReader
	tokens           TokenRepository
	usage            usage.Store
	routes           routing.Store
	models           provider.ModelLister
	settings         SystemSettingsStore
	uiPrefs          UIPreferencesStore
	captures         CaptureReader
	chats            ChatStore
	reachability     AppHealthReader
	loadedModels     LoadedModelReader
	agentPresence    AgentPresenceReader
	agentCertReports AgentCertReportReader
	agentTransport   AgentTransportReader
	proxyStatus      ProxyRouteStatusReader
	groups           GroupStore
	groupCache       GroupCache
	projects         ProjectStore
	cipher           *capture.Cipher
	// cert groups the certificate/edge-cert subsystem's fields (see certState
	// above): the certificate-only cipher, edge-delivery config/state, the ACME
	// challenge store, the certMu/edgeMu/certGWDNSMu mutexes (and the state
	// each guards), the certIssuer seam, and the onCertSettingsChanged/
	// onCertificateIssued/onCABundleChanged hooks (PT-1 stage 1: field-regroup
	// only, no behavior change).
	cert          certState
	agentPort     string
	agentBindHost string
	// agentTLSPort/agentTLSSeparateDefault mirror agentPort above for the
	// separate-encrypted-agent-port feature (see ServiceDeps.AgentTLSPort/
	// AgentTLSSeparateDefault).
	agentTLSPort            int
	agentTLSSeparateDefault bool
	// netbird groups the NetBird-subsystem's fields (see netbirdState above):
	// the sidecar-enroll key-file path, the dns_domain-changed hook, the
	// token-rotation default + tokenMu/nextTokenRotateAttempt, the
	// policyMu-serialized policy-reconcile section, and the
	// policySideEffectWG/resolveWG test-sync WaitGroups (PT-1 stage 2:
	// field-regroup only, no behavior change).
	netbird netbirdState
	// agentPresenceTimeoutDefault is the resolved (env-fallback) default used by
	// AgentPresenceTimeoutSeconds when the KV is unset/invalid.
	agentPresenceTimeoutDefault int
	settingsVolatile            bool
	clock                       func() time.Time
	secretGenerator             func() (string, error)
	idGenerator                 func() string
	// themes is the loaded external-theme registry (see ServiceDeps.Themes).
	// Always non-nil after NewService -- a nil deps.Themes is defaulted to an
	// empty *theme.Registry so every reader can call its methods unguarded.
	themes *theme.Registry
	// reconcileMu serializes the store-mutating critical section of
	// reconcileApplicationModels across all callers (manual sync + the
	// background model_sync probe loop, which reconciles many applications
	// concurrently). The per-server gateway-name uniqueness check is a
	// check-then-act with no backing DB constraint, so concurrent reconciles
	// would otherwise both see a name as free and both create an ACTIVE mapping,
	// silently losing conflict detection. The network ListModels call runs
	// OUTSIDE this lock, so only the fast local read+write section serializes.
	reconcileMu sync.Mutex
}

// waitPolicySideEffects blocks until any in-flight settings-triggered NetBird
// policy side effect (the background goroutine UpdateSystemSettings spawns when a
// policy-relevant field changes — see applyPolicySettingsSideEffects) has
// finished. A test-only synchronization point: it lets a test that also
// configures policy management via UpdateSystemSettings (e.g. the
// enableNetbirdPolicies helper) settle that background pass BEFORE seeding its
// own servers/policies, so its own subsequent assertions are not racing an
// unrelated, already-legitimate side effect of enabling the feature.
func (s *Service) waitPolicySideEffects() { s.netbird.policySideEffectWG.Wait() }

// waitNetbirdResolve blocks until any in-flight settings-triggered token-meta
// resolve (the background goroutine UpdateSystemSettings spawns when a NetBird
// token is (re)stored — see resolveStoredTokenMeta) has finished. Test-only.
func (s *Service) waitNetbirdResolve() { s.netbird.resolveWG.Wait() }

func NewService(deps ServiceDeps) *Service {
	clock := deps.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	secretGenerator := deps.SecretGenerator
	if secretGenerator == nil {
		secretGenerator = generateSecret
	}
	idGenerator := deps.IDGenerator
	if idGenerator == nil {
		idGenerator = func() string { return "tok_" + compactRandomHex(16) }
	}
	agentPort := deps.AgentPort
	if agentPort == "" {
		agentPort = "8081"
	}
	tokenRotateDefault := DefaultNetbirdTokenRotateBeforeDays
	if deps.NetbirdTokenRotateBeforeDaysDefault != nil {
		tokenRotateDefault = *deps.NetbirdTokenRotateBeforeDaysDefault
	}
	agentPresenceDefault := deps.AgentPresenceTimeoutDefault
	if agentPresenceDefault <= 0 {
		agentPresenceDefault = DefaultAgentPresenceTimeoutSeconds
	}
	themes := deps.Themes
	if themes == nil {
		themes = &theme.Registry{}
	}
	svc := &Service{
		users:            deps.Users,
		tokens:           deps.Tokens,
		usage:            deps.Usage,
		routes:           deps.Routes,
		models:           deps.ModelLister,
		settings:         deps.SystemSettings,
		uiPrefs:          deps.UIPrefs,
		captures:         deps.Captures,
		chats:            deps.Chats,
		reachability:     deps.Reachability,
		loadedModels:     deps.LoadedModels,
		agentPresence:    deps.AgentPresence,
		agentCertReports: deps.AgentCertReports,
		agentTransport:   deps.AgentTransport,
		proxyStatus:      deps.ProxyStatus,
		groups:           deps.Groups,
		groupCache:       deps.GroupCache,
		projects:         deps.Projects,
		cipher:           deps.Cipher,
		cert: certState{
			cipher:            deps.CertCipher,
			edgeOutputDir:     deps.CertEdgeOutputDir,
			edgeProbeTarget:   deps.CertEdgeProbeTarget,
			acmeChallenges:    deps.ACMEChallenges,
			onSettingsChanged: deps.OnCertSettingsChanged,
			onIssued:          deps.OnCertificateIssued,
			onCABundleChanged: deps.OnCABundleChanged,
		},
		netbird: netbirdState{
			keyFile:                  deps.NetbirdKeyFile,
			onDomainChanged:          deps.OnNetbirdDomainChanged,
			tokenRotateBeforeDefault: tokenRotateDefault,
		},
		agentPort:                   agentPort,
		agentBindHost:               deps.AgentBindHost,
		agentTLSPort:                deps.AgentTLSPort,
		agentTLSSeparateDefault:     deps.AgentTLSSeparateDefault,
		agentPresenceTimeoutDefault: agentPresenceDefault,
		settingsVolatile:            deps.SettingsVolatile,
		clock:                       clock,
		secretGenerator:             secretGenerator,
		idGenerator:                 idGenerator,
		themes:                      themes,
	}
	// Bound after construction so the method value carries the finished Service.
	svc.cert.issuer = svc.issueCertificate
	return svc
}

type CurrentUser struct {
	ID                             string `json:"id"`
	Email                          string `json:"email"`
	DisplayName                    string `json:"display_name"`
	Role                           string `json:"role"`
	PreferredLanguage              string `json:"preferred_language"`
	TOTPEnabled                    bool   `json:"totp_enabled"`
	TOTPMode                       string `json:"totp_mode"`
	SystemAdminMode                bool   `json:"system_admin_mode"`
	SystemAdminModeExpiresAt       string `json:"system_admin_mode_expires_at"`
	SystemAdminModeRequirePassword bool   `json:"system_admin_mode_require_password"`
}

// NewCurrentUser is the SINGLE producer of the CurrentUser DTO. Both
// Service.CurrentUser (no session in scope, so the three System-Admin-mode
// fields are always zero) and the gateway's currentUserDTO (has a session,
// so it passes the real elevation state) must build the DTO through this
// constructor rather than a struct literal of their own, or the two will
// drift again (PT-6): a future CurrentUser field added to one producer and
// forgotten in the other would silently default to a zero value, which for
// the System-Admin-mode fields means silently reverting a user's session
// elevation.
//
// Params are positional (not a struct) so that adding a field here forces
// every call site to be revisited at compile time.
//
//   - totpMode: the active TOTP mode string (Service.activeTOTPMode/TOTPMode).
//   - elevated: whether the caller's session is CURRENTLY in System-Admin
//     mode. Callers with no session in scope must pass false.
//   - elevatedUntil: the session's elevation expiry. Only rendered into
//     SystemAdminModeExpiresAt when elevated is true and this is non-zero;
//     ignored otherwise (mirrors the prior inline behavior exactly).
//   - requirePassword: the system_admin_mode_require_password setting (a UI
//     hint, not an authority).
func NewCurrentUser(user store.User, totpMode string, elevated bool, elevatedUntil time.Time, requirePassword bool) CurrentUser {
	dto := CurrentUser{
		ID:                             user.ID,
		Email:                          user.Email,
		DisplayName:                    user.DisplayName,
		Role:                           user.Role,
		PreferredLanguage:              user.PreferredLanguage,
		TOTPEnabled:                    user.TOTPEnabled,
		TOTPMode:                       totpMode,
		SystemAdminMode:                elevated,
		SystemAdminModeRequirePassword: requirePassword,
	}
	if elevated && !elevatedUntil.IsZero() {
		dto.SystemAdminModeExpiresAt = elevatedUntil.UTC().Format(time.RFC3339)
	}
	return dto
}

type TokenDTO struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	SecretPrefix  string     `json:"secret_prefix"`
	SecretHash    string     `json:"-"`
	Status        string     `json:"status"`
	Scopes        []string   `json:"scopes"`
	ExpiresAt     *time.Time `json:"expires_at"`
	LastUsedAt    *time.Time `json:"last_used_at"`
	CreatedAt     time.Time  `json:"created_at"`
	ModelOverride string     `json:"model_override"`
	// ModelOverrideMap maps a requested model name -> the gateway model to use
	// (takes precedence over ModelOverride, which is the catch-all). Empty = none.
	ModelOverrideMap map[string]string `json:"model_override_map,omitempty"`
	LogCommunication bool              `json:"log_communication"`
	Secret           bool              `json:"secret"`
	IsChatSession    bool              `json:"is_chat_session"`
	Deletable        bool              `json:"deletable"`
	// ProjectID/ProjectName is the project this token is attributed to for
	// usage attribution ("" = none). ProjectName is resolved for display and
	// is empty whenever ProjectID is empty or the project no longer exists.
	ProjectID   string `json:"project_id,omitempty"`
	ProjectName string `json:"project_name,omitempty"`
	// ServerOverride is the id of an AI-server this token forces every
	// request onto, bypassing resource-group provisioning/affinity/
	// maintenance-status ("" = no override). Self-healed on every
	// create/update against the owner's CURRENT server-manage rights — a
	// stale id the owner can no longer manage silently reads back "" rather
	// than the create/update failing. ServerOverrideForceUnreachable, when
	// true, allows the override to route even to an unhealthy/unreachable
	// server; it is always false whenever ServerOverride is "".
	ServerOverride                 string `json:"server_override,omitempty"`
	ServerOverrideForceUnreachable bool   `json:"server_override_force_unreachable,omitempty"`
}

type TokenListResponse struct {
	Data []TokenDTO `json:"data"`
}

type CreateTokenRequest struct {
	Name             string            `json:"name"`
	Scopes           []string          `json:"scopes"`
	ExpiresAt        *time.Time        `json:"expires_at"`
	ModelOverride    string            `json:"model_override"`
	ModelOverrideMap map[string]string `json:"model_override_map"`
	LogCommunication bool              `json:"log_communication"`
	Secret           bool              `json:"secret"`
	// ProjectID optionally attributes the token to a project the owner is a
	// member of ("" = none). Enforced by CreateToken via isProjectMember.
	ProjectID string `json:"project_id"`
	// ServerOverride optionally forces every request on this token onto one
	// AI-server ("" = none). Self-healed (not rejected) against the owner's
	// server-manage rights — see validateServerOverride. ForceUnreachable is
	// ignored (persisted as false) whenever the resulting override is "".
	ServerOverride                 string `json:"server_override"`
	ServerOverrideForceUnreachable bool   `json:"server_override_force_unreachable"`
}

type CreateTokenResponse struct {
	Token  TokenDTO `json:"token"`
	Secret string   `json:"secret"`
}

type UpdateTokenRequest struct {
	Name             *string            `json:"name,omitempty"`
	Scopes           *[]string          `json:"scopes,omitempty"`
	Status           *string            `json:"status,omitempty"`
	ModelOverride    *string            `json:"model_override,omitempty"`
	ModelOverrideMap *map[string]string `json:"model_override_map,omitempty"`
	LogCommunication *bool              `json:"log_communication,omitempty"`
	Secret           *bool              `json:"secret,omitempty"`
	// ProjectID: nil = keep the current project attribution, "" = clear it
	// (always allowed), a non-empty id = re-attribute (membership-checked via
	// isProjectMember, like CreateToken).
	ProjectID *string `json:"project_id,omitempty"`
	// ServerOverride: nil = keep the current value (still re-validated/
	// self-healed on this update, see UpdateToken), "" = clear it, a
	// non-empty id = replace it (self-healed via validateServerOverride, not
	// rejected). ServerOverrideForceUnreachable: nil = keep the current value.
	ServerOverride                 *string `json:"server_override,omitempty"`
	ServerOverrideForceUnreachable *bool   `json:"server_override_force_unreachable,omitempty"`
}

type DashboardResponse struct {
	Metrics DashboardMetrics `json:"metrics"`
	Routes  []RouteDTO       `json:"routes"`
}

type DashboardMetrics struct {
	Requests24h  int    `json:"requests_24h"`
	Tokens24h    int    `json:"tokens_24h"`
	HealthyHosts string `json:"healthy_hosts"`
	LatencyP95MS int64  `json:"latency_p95_ms"`
}

type RouteDTO struct {
	ID       string `json:"id,omitempty"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Host     string `json:"host"`
	Status   string `json:"status"`
}

type ModelsResponse struct {
	Data []ModelDTO `json:"data"`
}

type ModelDTO struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name"`
	Flavors     []string `json:"flavors"`
	// Loaded is true when this model is currently loaded on at least one
	// reachable application (so it can be requested without waiting for a load).
	Loaded bool `json:"loaded"`
	// LoadedOn lists the server names where this model is currently loaded
	// (empty unless Loaded). Sorted, deduped.
	LoadedOn []string `json:"loaded_on,omitempty"`
	// OfferedOnCount is how many servers currently OFFER this model (an active
	// mapping on an active + reachable application). Always ≥ the loaded count.
	OfferedOnCount int `json:"offered_on_count"`
	// Visibility is the model's global visibility from model_settings
	// ("shown" | "hidden" | "locked"); "shown" when no setting row exists.
	Visibility string `json:"visibility"`
	// IsGroup marks a synthetic model that is actually a model group: its
	// members are real models and a request for it fails over across them.
	// Omitted for ordinary models.
	IsGroup bool `json:"is_group,omitempty"`
	// ContextSize is the usable context window in tokens for this model — the
	// MINIMUM known (>0) context_size across the offering mappings (conservative:
	// never overstates when servers differ). 0 = unknown (no mapping reports one).
	ContextSize int `json:"context_size"`
	// Vision is true only when EVERY offering mapping (or, for a group, every
	// offerable member) is vision-capable (AND aggregation, fail-closed): a
	// model/group is advertised as vision-capable only when it can be trusted to
	// accept image inputs no matter which offering server serves the request.
	Vision bool `json:"vision"`
}

type ServerOwnerDTO struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
}

// NetbirdGroupRefDTO is a NetBird policy-group reference (id + display name)
// surfaced on ServerDTO. Ids/names are non-secret references.
type NetbirdGroupRefDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ServerDTO struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Domain string `json:"domain"`
	// ServerPathSuffix is an optional URL path segment appended to the origin
	// (scheme://domain:port) when composing the reachable base URL. Empty = none.
	ServerPathSuffix string           `json:"server_path_suffix"`
	Status           string           `json:"status"`
	HealthStatus     string           `json:"health_status"`
	Owners           []ServerOwnerDTO `json:"owners"`
	LastSeenAt       *time.Time       `json:"last_seen_at"`
	CreatedAt        time.Time        `json:"created_at"`
	// NetBird integration. NetbirdEnabled marks the server as a NetBird peer;
	// NetbirdSetupKeyID / NetbirdGroupID / NetbirdPeerID / NetbirdConnected surface
	// the enrolled + synced state (a non-secret reference — the tracking-group id is
	// the NetBird group the enrolling peer is added to, safe to display).
	NetbirdEnabled    bool   `json:"netbird_enabled"`
	NetbirdSetupKeyID string `json:"netbird_setup_key_id"`
	NetbirdGroupID    string `json:"netbird_group_id"`
	NetbirdPeerID     string `json:"netbird_peer_id"`
	NetbirdConnected  bool   `json:"netbird_connected"`
	// NetbirdGroupIDs is the peer's NetBird POLICY group membership (excluding the
	// tracking group), tolerantly decoded from the opaque netbird_group_ids column
	// (the sync mirror). Always a non-nil slice ([] when none / malformed).
	NetbirdGroupIDs []NetbirdGroupRefDTO `json:"netbird_group_ids"`
	// NetbirdPeerManaged marks a server whose NetBird peer + setup key originated
	// from a gateway-generated setup key (create hook / enroll / regenerate) —
	// "portal-created". A peer bound MANUALLY via the linkage editor is NOT managed.
	// The delete-server checkbox pre-checks the peer/key cleanup for managed servers.
	NetbirdPeerManaged bool `json:"netbird_peer_managed"`
	// NetbirdPolicyOverride is the per-server policy opt-in/opt-out override:
	// "" (follow the effective scope) / "include" / "exclude". Non-secret; the
	// frontend policy panel reads + writes it.
	NetbirdPolicyOverride string `json:"netbird_policy_override"`
	// NetbirdAllowPing lets the gateway ICMP-ping this server (adds its tracking group
	// to the op-gw-ping-servers destination set when the account-wide allow-all switch
	// is off). Non-secret; the frontend reads + writes it.
	NetbirdAllowPing bool `json:"netbird_allow_ping"`
	// NetbirdPingExclude is the per-server ping opt-out (see routing.AIServer).
	NetbirdPingExclude bool `json:"netbird_ping_exclude"`
	// CertificateOverride is the per-server certificate-management opt-in/opt-out:
	// "" (follow cert_server_scope) / "include" / "exclude". Non-secret; the
	// certificates view reads + writes it via SetServerCertificateOverride.
	CertificateOverride string `json:"certificate_override"`
	// HTTPSSwitchOverride is the per-server https-auto-switch opt-in/opt-out
	// (P4): "" (follow cert_https_switch_mode) / "include" / "exclude". Mirrors
	// CertificateOverride; non-secret. Written via SetServerHTTPSSwitchOverride;
	// its MEANING is mode-dependent, resolved by httpsSwitchInScope.
	HTTPSSwitchOverride string `json:"https_switch_override"`
	// NetbirdSetupKey is the freshly-generated setup-key VALUE, returned ONLY by
	// CreateServer (display-once) — it is never persisted and never present on a
	// List/Get DTO (omitempty). NetbirdError carries a best-effort create-hook
	// failure so the server is still created but the operator is told the key
	// could not be generated (mirrors the SMTP invite email_sent/email_error).
	NetbirdSetupKey string `json:"netbird_setup_key,omitempty"`
	// NetbirdSetupCommand is the ready-to-paste `netbird up` console command
	// (contains the display-once setup key). Returned ONLY alongside NetbirdSetupKey
	// by CreateServer/RegenerateNetbirdKey — display-once, never persisted, never on
	// a List/Get DTO (omitempty).
	NetbirdSetupCommand string `json:"netbird_setup_command,omitempty"`
	NetbirdError        string `json:"netbird_error,omitempty"`
	// AgentStatus is the live derived ServerAgent presence: "active" (reporting
	// within the effective window), "inactive" (an agent token is configured but
	// not currently reporting), or "unconfigured" (no agent token at all).
	AgentStatus string `json:"agent_status"`
	// AgentPresenceTimeoutSeconds is the per-server override (seconds) for "the
	// agent is delivering values"; 0 = follow the system-wide default.
	AgentPresenceTimeoutSeconds int `json:"agent_presence_timeout_seconds"`
	// Energy-attribution config (purely additive — no engine consumes these
	// yet). All default 0 = "unset / use default".
	EstimatedWatts float64 `json:"estimated_watts"`
	IdleWatts      float64 `json:"idle_watts"`
	PricePerKwh    float64 `json:"price_per_kwh"`
	Pue            float64 `json:"pue"`
	// PriceUnit is the display unit for PricePerKwh (migration v37, additive
	// display metadata — the canonical price stays EUR/kWh; the frontend
	// converts for display/entry). Always a normalized value (see
	// NormalizePriceUnit).
	PriceUnit string `json:"price_unit"`
	// AdminGroups is the server's linked admin-group set (id+name; migration
	// v50, server_admin_groups), the containment/authorization basis
	// authorizeServer/ListServers consume (Phase B, spec 2026-08-10). Always a
	// non-nil slice ([] when ungrouped); a group that vanished between the
	// link write and this read is skipped (best-effort name resolution).
	AdminGroups []GroupRefDTO `json:"admin_groups"`
	// SystemGroupID/SystemGroupName are the server's containment root -- the
	// system-tier group every linked admin group must be a child of ("" for
	// an ungrouped legacy server). SystemGroupName is a best-effort lookup
	// (empty when SystemGroupID is "" or the group has since vanished). No
	// secret in either field.
	SystemGroupID   string `json:"system_group_id"`
	SystemGroupName string `json:"system_group_name"`
}

type ServerListResponse struct {
	Data []ServerDTO `json:"data"`
}

type CreateServerRequest struct {
	Name             string   `json:"name"`
	Domain           string   `json:"domain"`
	ServerPathSuffix string   `json:"server_path_suffix"`
	Status           string   `json:"status"`
	OwnerIDs         []string `json:"owner_ids"`
	// NetbirdEnabled flags the server as a NetBird network peer (honored only when
	// the NetBird module is enabled; otherwise forced false). On create it triggers
	// the best-effort setup-key generation hook.
	NetbirdEnabled bool `json:"netbird_enabled"`
	// NetbirdPolicyOverride sets the per-server NetBird policy opt-in/opt-out
	// ('' | 'include' | 'exclude'). Enforced for a non-system-admin (see CreateServer).
	NetbirdPolicyOverride string `json:"netbird_policy_override"`
	// AgentPresenceTimeoutSeconds is the per-server agent-presence-window
	// override (seconds); nil = follow the system default (stored as 0). Must be
	// >= 0 when set.
	AgentPresenceTimeoutSeconds *int `json:"agent_presence_timeout_seconds,omitempty"`
	// Energy-attribution config (purely additive — no engine consumes these
	// yet). nil = unset (stored as 0). Must be >= 0 when set.
	EstimatedWatts *float64 `json:"estimated_watts,omitempty"`
	IdleWatts      *float64 `json:"idle_watts,omitempty"`
	PricePerKwh    *float64 `json:"price_per_kwh,omitempty"`
	Pue            *float64 `json:"pue,omitempty"`
	// PriceUnit is the display unit for PricePerKwh; nil/"" = default
	// ("eur_cent" via NormalizePriceUnit).
	PriceUnit *string `json:"price_unit,omitempty"`
	// AdminGroupIDs is the set of ADMIN-tier groups the new server is linked
	// to (server_admin_groups, migration v50) -- mandatory for EVERY caller,
	// including system-scope (Phase B, spec 2026-08-10). Every chosen group
	// must share one parent (system-tier) group, which becomes the server's
	// SystemGroupID containment root; see validateAdminGroupIDs.
	AdminGroupIDs []string `json:"admin_group_ids"`
	// SystemGroupID is an optional system-admin convenience cross-check: when
	// set (system-scope only), every chosen AdminGroupIDs entry's parent must
	// equal it, or the create is rejected as a parent mismatch.
	SystemGroupID string `json:"system_group_id"`
}

type UpdateServerRequest struct {
	Name             *string   `json:"name,omitempty"`
	Domain           *string   `json:"domain,omitempty"`
	ServerPathSuffix *string   `json:"server_path_suffix,omitempty"`
	Status           *string   `json:"status,omitempty"`
	OwnerIDs         *[]string `json:"owner_ids,omitempty"`
	// NetbirdEnabled toggles the NetBird flag (honored only when the module is
	// enabled). UpdateServer sets the flag but does NOT generate a setup key.
	NetbirdEnabled *bool `json:"netbird_enabled,omitempty"`
	// AgentPresenceTimeoutSeconds is the per-server agent-presence-window
	// override (seconds); a supplied 0 resets to "follow the system default".
	// Must be >= 0 when set.
	AgentPresenceTimeoutSeconds *int `json:"agent_presence_timeout_seconds,omitempty"`
	// Energy-attribution config (purely additive — no engine consumes these
	// yet); a supplied 0 resets to "unset". Must be >= 0 when set.
	EstimatedWatts *float64 `json:"estimated_watts,omitempty"`
	IdleWatts      *float64 `json:"idle_watts,omitempty"`
	PricePerKwh    *float64 `json:"price_per_kwh,omitempty"`
	Pue            *float64 `json:"pue,omitempty"`
	// PriceUnit is the display unit for PricePerKwh; a supplied "" normalizes
	// to the default ("eur_cent" via NormalizePriceUnit).
	PriceUnit *string `json:"price_unit,omitempty"`
}

func (s *Service) CurrentUser(ctx context.Context, token auth.Token) (CurrentUser, error) {
	user, err := s.users.UserByID(ctx, token.UserID)
	if err != nil {
		return CurrentUser{}, err
	}
	// No session is in scope here, so the three System-Admin-mode fields are
	// always zero-valued (elevated=false, expiresAt=zero, requirePassword=
	// false) — see NewCurrentUser. Callers that DO have a session overlay the
	// real elevation state afterward (e.g. gateway's handlePortalMe) or build
	// the DTO via gateway's currentUserDTO instead, which passes the real
	// values straight through to NewCurrentUser.
	return NewCurrentUser(user, s.activeTOTPMode(ctx), false, time.Time{}, false), nil
}

// DisplayNames resolves user display names for the given ids, best-effort:
// unknown/errored ids are simply absent from the map. Used by the gateway's
// running-connections endpoint to label rows with the user (like the usage
// table's denormalised user_name).
func (s *Service) DisplayNames(ctx context.Context, ids []string) map[string]string {
	out := make(map[string]string)
	seen := make(map[string]bool)
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if u, err := s.users.UserByID(ctx, id); err == nil && u.DisplayName != "" {
			out[id] = u.DisplayName
		}
	}
	return out
}

// UserPreferences returns the user's stored UI preferences as a key -> raw JSON
// map (an empty, non-nil map when none are stored). Each stored value is returned
// opaquely as json.RawMessage.
func (s *Service) UserPreferences(ctx context.Context, userID string) (map[string]json.RawMessage, error) {
	prefs, err := s.uiPrefs.UIPreferences(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(prefs))
	for _, pref := range prefs {
		out[pref.Key] = json.RawMessage(pref.ValueJSON)
	}
	return out, nil
}

// SetUserPreference upserts a single opaque JSON value under key for the user.
// An empty key is rejected.
func (s *Service) SetUserPreference(ctx context.Context, userID, key string, value json.RawMessage) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return ErrPreferenceKeyRequired
	}
	return s.uiPrefs.SetUIPreference(ctx, userID, key, string(value))
}

func (s *Service) ListTokens(ctx context.Context, token auth.Token) (TokenListResponse, error) {
	return s.tokensForUser(ctx, token.UserID)
}

// UserTokens is the admin-scoped variant of ListTokens: it lists any user's
// tokens (incl. the synthetic ChatSession row) — admin-sensitive since it
// exposes another user's tokens, even though it is itself a read. The HTTP
// layer already sits behind requireWebScope("admin"); as of PT-2 Part 2 this
// also checks isAdmin(principal) itself (ErrPrincipalForbidden otherwise) as
// defense-in-depth against a future internal caller that bypasses the HTTP
// gate.
func (s *Service) UserTokens(ctx context.Context, principal auth.Token, userID string) (TokenListResponse, error) {
	if !isAdmin(principal) {
		return TokenListResponse{}, ErrPrincipalForbidden
	}
	return s.tokensForUser(ctx, userID)
}

// tokensForUser lists userID's real tokens with the synthetic, non-deletable
// ChatSession row prepended, carrying that user's profile chat flags. A user
// lookup failure falls back to false/false rather than failing the whole list;
// the frontend localizes the display name via is_chat_session.
func (s *Service) tokensForUser(ctx context.Context, userID string) (TokenListResponse, error) {
	records, err := s.tokens.TokensByUser(ctx, userID)
	if err != nil {
		return TokenListResponse{}, err
	}
	var chatLog, chatSecret bool
	if s.users != nil {
		if user, err := s.users.UserByID(ctx, userID); err == nil {
			chatLog = user.ChatLogCommunication
			chatSecret = user.ChatSecret
		}
	}
	chat := TokenDTO{
		ID:               ChatSessionTokenID,
		Name:             ChatSessionTokenID,
		Status:           store.TokenStatusActive,
		Scopes:           []string{},
		LogCommunication: chatLog,
		Secret:           chatSecret,
		IsChatSession:    true,
		Deletable:        false,
	}
	out := make([]TokenDTO, 0, len(records)+1)
	out = append(out, chat)
	for _, record := range records {
		out = append(out, s.tokenDTO(ctx, record))
	}
	return TokenListResponse{Data: out}, nil
}

func (s *Service) CreateToken(ctx context.Context, owner auth.Token, req CreateTokenRequest) (CreateTokenResponse, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return CreateTokenResponse{}, ErrTokenNameRequired
	}
	scopes, err := validateTokenScopes(owner, req.Scopes)
	if err != nil {
		return CreateTokenResponse{}, err
	}
	taken, err := s.tokenNameTaken(ctx, owner.UserID, name, "")
	if err != nil {
		return CreateTokenResponse{}, err
	}
	if taken {
		return CreateTokenResponse{}, ErrTokenNameConflict
	}
	override, err := s.validateModelOverride(ctx, owner, req.ModelOverride)
	if err != nil {
		return CreateTokenResponse{}, err
	}
	overrideMap, err := s.validateModelOverrideMap(ctx, owner, req.ModelOverrideMap)
	if err != nil {
		return CreateTokenResponse{}, err
	}
	projectID, err := s.assignTokenProject(ctx, owner.UserID, req.ProjectID)
	if err != nil {
		return CreateTokenResponse{}, err
	}
	serverOverride := s.validateServerOverride(ctx, owner, req.ServerOverride)
	serverOverrideForce := req.ServerOverrideForceUnreachable && serverOverride != ""
	now := s.clock().UTC()
	secret, err := s.secretGenerator()
	if err != nil {
		return CreateTokenResponse{}, err
	}
	scopesJSON, err := json.Marshal(scopes)
	if err != nil {
		return CreateTokenResponse{}, err
	}
	record := store.TokenRecord{
		ID:                             s.idGenerator(),
		UserID:                         owner.UserID,
		Name:                           name,
		Status:                         store.TokenStatusActive,
		Scopes:                         string(scopesJSON),
		ExpiresAt:                      req.ExpiresAt,
		CreatedAt:                      now,
		UpdatedAt:                      now,
		ModelOverride:                  override,
		ModelOverrideMap:               store.EncodeModelOverrideRules(modelOverrideMapToRules(overrideMap)),
		LogCommunication:               req.LogCommunication,
		Secret:                         req.Secret,
		ProjectID:                      projectID,
		ServerOverride:                 serverOverride,
		ServerOverrideForceUnreachable: serverOverrideForce,
	}
	if err := s.tokens.CreatePlainToken(ctx, record, secret); err != nil {
		return CreateTokenResponse{}, err
	}
	record.SecretPrefix = secretPrefix(secret)
	return CreateTokenResponse{Token: s.tokenDTO(ctx, record), Secret: secret}, nil
}

func (s *Service) UpdateToken(ctx context.Context, owner auth.Token, tokenID string, req UpdateTokenRequest) (TokenDTO, error) {
	tokenID = strings.TrimSpace(tokenID)
	if tokenID == "" {
		return TokenDTO{}, ErrTokenNotFound
	}
	if tokenID == ChatSessionTokenID {
		return TokenDTO{}, ErrTokenNotDeletable
	}
	record, err := s.tokens.TokenByID(ctx, tokenID)
	if err != nil || record.UserID != owner.UserID {
		return TokenDTO{}, ErrTokenNotFound
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return TokenDTO{}, ErrTokenNameRequired
		}
		taken, err := s.tokenNameTaken(ctx, owner.UserID, name, record.ID)
		if err != nil {
			return TokenDTO{}, err
		}
		if taken {
			return TokenDTO{}, ErrTokenNameConflict
		}
		record.Name = name
	}
	if req.Scopes != nil {
		scopes, err := validateTokenScopes(owner, *req.Scopes)
		if err != nil {
			return TokenDTO{}, err
		}
		scopesJSON, err := json.Marshal(scopes)
		if err != nil {
			return TokenDTO{}, err
		}
		record.Scopes = string(scopesJSON)
	}
	if req.Status != nil {
		status := strings.TrimSpace(*req.Status)
		if status != store.TokenStatusActive && status != store.TokenStatusDisabled {
			return TokenDTO{}, ErrTokenStatusInvalid
		}
		record.Status = status
	}
	if req.ModelOverride != nil {
		override, err := s.validateModelOverride(ctx, owner, *req.ModelOverride)
		if err != nil {
			return TokenDTO{}, err
		}
		record.ModelOverride = override
	}
	if req.ModelOverrideMap != nil {
		overrideMap, err := s.validateModelOverrideMap(ctx, owner, *req.ModelOverrideMap)
		if err != nil {
			return TokenDTO{}, err
		}
		record.ModelOverrideMap = store.EncodeModelOverrideRules(modelOverrideMapToRules(overrideMap))
	}
	if req.LogCommunication != nil {
		record.LogCommunication = *req.LogCommunication
	}
	if req.Secret != nil {
		record.Secret = *req.Secret
	}
	if req.ProjectID != nil {
		projectID, err := s.assignTokenProject(ctx, owner.UserID, *req.ProjectID)
		if err != nil {
			return TokenDTO{}, err
		}
		record.ProjectID = projectID
	}
	if req.ServerOverride != nil {
		record.ServerOverride = strings.TrimSpace(*req.ServerOverride)
	}
	if req.ServerOverrideForceUnreachable != nil {
		record.ServerOverrideForceUnreachable = *req.ServerOverrideForceUnreachable
	}
	// Self-heal the EFFECTIVE server override (whatever this update leaves it
	// at — a freshly requested value, or the value already stored and
	// untouched by this request) against the owner's CURRENT server-manage
	// rights on EVERY update, not only when the request itself touches
	// ServerOverride: the owner may have lost manage rights on a previously
	// valid override since it was set, and any update is an opportunity to
	// notice and clear it rather than leaving a silently-unenforceable
	// override sitting in the record. validateServerOverride is a cheap no-op
	// (no AuthorizeServerManage call) when the effective value is already "".
	record.ServerOverride = s.validateServerOverride(ctx, owner, record.ServerOverride)
	if record.ServerOverride == "" {
		record.ServerOverrideForceUnreachable = false
	}
	record.UpdatedAt = s.clock().UTC()
	if err := s.tokens.UpdateTokenMetadata(ctx, record); err != nil {
		return TokenDTO{}, err
	}
	return s.tokenDTO(ctx, record), nil
}

func (s *Service) DeleteToken(ctx context.Context, owner auth.Token, tokenID string) error {
	tokenID = strings.TrimSpace(tokenID)
	if tokenID == "" {
		return ErrTokenNotFound
	}
	if tokenID == ChatSessionTokenID {
		return ErrTokenNotDeletable
	}
	record, err := s.tokens.TokenByID(ctx, tokenID)
	if err != nil || record.UserID != owner.UserID {
		return ErrTokenNotFound
	}
	return s.tokens.DeleteToken(ctx, record.ID)
}

// RotateToken replaces an existing token's secret in place, keeping its id,
// name, scopes, and settings (and therefore its usage history). The old secret
// stops working immediately; the fresh secret is returned once, exactly like
// CreateToken, so the frontend reuses the reveal flow. The chat-session pseudo
// id has no stored secret and is rejected as not-found. Owner-scoped: a foreign
// or missing token is ErrTokenNotFound with no existence leak.
func (s *Service) RotateToken(ctx context.Context, owner auth.Token, id string) (CreateTokenResponse, error) {
	id = strings.TrimSpace(id)
	if id == "" || id == ChatSessionTokenID {
		return CreateTokenResponse{}, ErrTokenNotFound
	}
	record, err := s.tokens.TokenByID(ctx, id)
	if err != nil || record.UserID != owner.UserID {
		return CreateTokenResponse{}, ErrTokenNotFound
	}
	secret, err := s.secretGenerator()
	if err != nil {
		return CreateTokenResponse{}, err
	}
	now := s.clock().UTC()
	if err := s.tokens.RotateTokenSecret(ctx, record.ID, auth.HashSecret(secret), secretPrefix(secret), now); err != nil {
		return CreateTokenResponse{}, err
	}
	record.SecretPrefix = secretPrefix(secret)
	record.UpdatedAt = now
	return CreateTokenResponse{Token: s.tokenDTO(ctx, record), Secret: secret}, nil
}

func (s *Service) tokenNameTaken(ctx context.Context, userID, name, excludeID string) (bool, error) {
	records, err := s.tokens.TokensByUser(ctx, userID)
	if err != nil {
		return false, err
	}
	target := strings.ToLower(strings.TrimSpace(name))
	for _, record := range records {
		if record.ID == excludeID {
			continue
		}
		if strings.ToLower(strings.TrimSpace(record.Name)) == target {
			return true, nil
		}
	}
	return false, nil
}

// validateModelOverride trims the requested override and, when non-empty,
// requires it to be a currently-active gateway model. Empty = override off.
func (s *Service) validateModelOverride(ctx context.Context, owner auth.Token, model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", nil
	}
	for _, m := range s.Models(ctx, owner).Data {
		if m.ID == model {
			return model, nil
		}
	}
	return "", ErrTokenModelOverrideInvalid
}

// validateModelOverrideMap trims and validates a per-requested-model override map:
// each entry's requested-model KEY is free text (arbitrary client model name, only
// required to be non-empty), and each VALUE must be a currently-active gateway
// model (like the catch-all). Fully-empty rows are dropped; a row with only one
// side filled, or a value that is not a known model, is rejected with
// ErrTokenModelOverrideInvalid. Returns nil for an empty/all-dropped map. The
// active-model set is fetched once (not per entry).
func (s *Service) validateModelOverrideMap(ctx context.Context, owner auth.Token, raw map[string]string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	known := make(map[string]struct{})
	for _, m := range s.Models(ctx, owner).Data {
		known[m.ID] = struct{}{}
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		key := strings.TrimSpace(k)
		val := strings.TrimSpace(v)
		if key == "" && val == "" {
			continue // fully-empty row: drop
		}
		if key == "" || val == "" {
			return nil, ErrTokenModelOverrideInvalid // half-filled row
		}
		if _, ok := known[val]; !ok {
			return nil, ErrTokenModelOverrideInvalid // target must be an active gateway model
		}
		out[key] = val
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// modelOverrideMapToRules mechanically lifts a validated requested->model DTO
// map into the rules codec (Task 1's store.ModelOverrideRule), so it can be
// persisted via store.EncodeModelOverrideRules: each entry becomes a rule with
// only To set (Offer/HideTarget false) — the DTO wire format itself is
// untouched by this task, a later task owns exposing the two listing switches
// on it. nil in, nil out.
func modelOverrideMapToRules(m map[string]string) map[string]store.ModelOverrideRule {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]store.ModelOverrideRule, len(m))
	for k, v := range m {
		out[k] = store.ModelOverrideRule{To: v}
	}
	return out
}

// modelOverrideRulesToMap mechanically projects decoded rules back down to the
// DTO's requested->model map (rule.To only, dropping Offer/HideTarget) — the
// inverse of modelOverrideMapToRules, used wherever a TokenDTO/ServiceTokenDTO
// is rendered. nil in, nil out.
func modelOverrideRulesToMap(rules map[string]store.ModelOverrideRule) map[string]string {
	if len(rules) == 0 {
		return nil
	}
	out := make(map[string]string, len(rules))
	for k, rule := range rules {
		out[k] = rule.To
	}
	return out
}

func validateTokenScopes(owner auth.Token, requested []string) ([]string, error) {
	scopes := requested
	if len(scopes) == 0 {
		scopes = []string{"gateway:use"}
	}
	out := make([]string, 0, len(scopes))
	for _, raw := range scopes {
		scope := strings.TrimSpace(raw)
		switch scope {
		case "gateway:use":
			if !hasGatewayUse(owner) && !isAdmin(owner) {
				return nil, ErrTokenScopeForbidden
			}
		case "admin":
			if !isAdmin(owner) {
				return nil, ErrTokenScopeForbidden
			}
		default:
			return nil, ErrTokenScopeInvalid
		}
		out = append(out, scope)
	}
	return out, nil
}

// Usage returns the paged, filtered activity list for the principal. scope=all is
// admin-gated (HasScope("admin") — admin or system_admin); anyone else is pinned to
// their own rows. HasCapture/CaptureLocked are resolved store-agnostically here —
// not by the usage store's SQL — via one best-effort CaptureReader.HasCaptures
// lookup over the page's row IDs. Per viewer (SP-2e): the owner (or an admin on a
// non-secret row) gets has_capture; an admin on another's secret row gets
// capture_locked (existence only, no content). A nil Captures, an empty page, or a
// lookup error all leave both flags false rather than failing the whole list.
//
// A non-nil error means the underlying store query failed — the caller (the
// gateway handler) must map it to a 500 rather than render the accompanying
// zero-value Page as "no matching rows" (see usage.Store.Query).
func (s *Service) Usage(principal auth.Token, q usage.Query) (usage.Page, error) {
	s.applyUsageScope(context.Background(), &q, principal)
	page, err := s.usage.Query(q)
	if err != nil {
		return page, fmt.Errorf("usage: %w", err)
	}
	s.attachUsageCost(context.Background(), page.Data)
	if len(page.Data) == 0 || s.captures == nil {
		return page, nil
	}
	ids := make([]string, len(page.Data))
	for i, row := range page.Data {
		ids[i] = row.ID
	}
	presence, err := s.captures.HasCaptures(context.Background(), ids)
	if err != nil {
		log.Printf("portal: has_captures lookup failed for %d rows: %v", len(ids), err)
		return page, nil
	}
	admin := isAdmin(principal)
	for i := range page.Data {
		p, ok := presence[page.Data[i].ID]
		if !ok {
			continue
		}
		isOwner := principal.UserID == p.OwnerUserID
		switch {
		case isOwner || (!p.Secret && admin):
			page.Data[i].HasCapture = true
		case p.Secret && admin:
			page.Data[i].CaptureLocked = true
		}
	}
	return page, nil
}

// UsageStats returns the aggregate tiles + speed histograms under the same scope
// rule. Totals.TotalEnergyWh is a plain SUM from the store; Totals.TotalCostEUR
// is derived here in the portal layer from EnergyByServer (per-server energy)
// weighted by each server's resolved price (P3 §8) — the usage store itself
// never resolves pricing (it has no routing/settings dependency). A
// EnergyByServer error degrades TotalCostEUR to 0 rather than failing the
// whole stats response (cost is a best-effort derived display value).
//
// A non-nil error means the underlying store aggregate failed — the caller
// must map it to a 500 rather than render the accompanying zero-value Stats
// as "genuinely no matching rows" (see usage.Store.Stats).
func (s *Service) UsageStats(principal auth.Token, q usage.Query) (usage.Stats, error) {
	s.applyUsageScope(context.Background(), &q, principal)
	stats, err := s.usage.Stats(q)
	if err != nil {
		return stats, fmt.Errorf("usage stats: %w", err)
	}
	ctx := context.Background()
	if byServer, err := s.usage.EnergyByServer(ctx, q); err == nil {
		sysDefault := s.systemDefaultPricePerKwh(ctx)
		var totalCost float64
		for host, wh := range byServer {
			totalCost += wh / 1000 * s.resolveUsagePrice(ctx, host, sysDefault)
		}
		stats.Totals.TotalCostEUR = totalCost
	}
	return stats, nil
}

// UsageTimeSeries returns the bucketed activity time-series under the same scope
// rule as UsageStats (scope=all is admin-gated via applyUsageScope). A non-nil
// error means the underlying store aggregate failed — the caller must map it
// to a 500 rather than render the accompanying zero-value/empty TimeSeries as
// "genuinely no matching rows" (see usage.Store.TimeSeries).
func (s *Service) UsageTimeSeries(principal auth.Token, q usage.Query, bucketSecs int) (usage.TimeSeries, error) {
	s.applyUsageScope(context.Background(), &q, principal)
	ts, err := s.usage.TimeSeries(q, bucketSecs)
	if err != nil {
		return ts, fmt.Errorf("usage timeseries: %w", err)
	}
	return ts, nil
}

// systemDefaultPricePerKwh reads energy_default_price_per_kwh from the
// settings store (0 = unset). A nil settings store or a read error degrades
// to 0 (fail-open — cost is a best-effort derived display value, never an
// error path).
func (s *Service) systemDefaultPricePerKwh(ctx context.Context) float64 {
	if s.settings == nil {
		return 0
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return 0
	}
	return EnergyDefaultPricePerKwh(values)
}

// resolveUsagePrice returns the effective electricity price (currency per
// kWh) to cost host's (an AI-server id, usage.Event.Host) energy
// attribution: the server's own PricePerKwh when set (>0), else sysDefault
// (already resolved by the caller once per call, not once per host). A nil
// routes store, a lookup error (e.g. the server was since deleted), or a
// zero server price all fall through to sysDefault — never an error, never a
// negative/derived cost.
func (s *Service) resolveUsagePrice(ctx context.Context, host string, sysDefault float64) float64 {
	if s.routes != nil {
		if server, err := s.routes.AIServerByID(ctx, host); err == nil && server.PricePerKwh > 0 {
			return server.PricePerKwh
		}
	}
	return sysDefault
}

// attachUsageCost sets CostEUR on each row: (EnergyWh/1000) * price(Host),
// where price is resolved via resolveUsagePrice ONCE per DISTINCT host (not
// once per row) — O(distinct servers), not O(rows). CostEUR is a transient
// display field: it is never persisted and never read back from a store.
func (s *Service) attachUsageCost(ctx context.Context, rows []usage.Row) {
	if len(rows) == 0 {
		return
	}
	sysDefault := s.systemDefaultPricePerKwh(ctx)
	priceByHost := make(map[string]float64)
	for i := range rows {
		host := rows[i].Host
		price, cached := priceByHost[host]
		if !cached {
			price = s.resolveUsagePrice(ctx, host, sysDefault)
			priceByHost[host] = price
		}
		rows[i].CostEUR = rows[i].EnergyWh / 1000 * price
	}
}

// applyUsageScope is the single server-side authority gate for cross-user
// visibility. Cross-user (all-scope) access requires HasScope("admin") — i.e.
// role admin OR system_admin (sessionPrincipal grants "admin" to both).
// Everyone else — a plain gateway:use principal — is pinned to their own
// UserID EXCEPT for one additive widening (design spec §8, the feature's only
// behavior change on existing surface): a PROJECT-SCOPED query (group-by
// "project", or an exact project_id_exact filter) may instead be scoped to the
// caller's MEMBER projects (owner ∪ direct member ∪ member via an assigned
// group — see memberProjectIDs / Task 3), never wider. This method is on
// *Service (not a free function) because the project-scope path needs
// s.memberProjectIDs, which requires the ProjectStore/GroupStore deps.
//
// ScopeAll (not just an empty UserID) is what actually drops the user_id pin
// in every store (usage.Recorder.matchUsage / SQLiteStore's usageWhere/
// TimeSeries all gate the user_id predicate on `!q.ScopeAll`) — so the
// project-scope widening below sets q.ScopeAll = true alongside q.UserID = ""
// wherever it opens visibility; every other path returns with ScopeAll=false
// and UserID pinned, matching the pre-existing non-admin behavior exactly.
func (s *Service) applyUsageScope(ctx context.Context, q *usage.Query, principal auth.Token) {
	if isAdmin(principal) {
		if q.ScopeAll {
			q.ScopeAll = true
			q.UserID = ""
		} else {
			q.ScopeAll = false
			q.UserID = principal.UserID
		}
		// Admin-only "Bestimmter Nutzer" pin: overrides the all-scope so the list
		// shows just that user. HasTokenFilter/TokenID are left untouched here and
		// applied by the stores regardless of scope.
		if q.FilterUserID != "" {
			q.UserID = q.FilterUserID
			q.ScopeAll = false
		}
		return
	}

	// Non-admin. A FilterUserID smuggled by a plain user is ignored everywhere
	// below — every non-admin path pins UserID to the caller's own id (or, in
	// the project-scope widening, drops the pin entirely — never to another
	// specific user).
	projectScoped := q.GroupBy == "project" || q.HasProjectIDExact
	if projectScoped {
		member, err := s.memberProjectIDs(ctx, principal.UserID)
		if err == nil {
			if q.HasProjectIDExact {
				// Drill-down into one project (design spec §8): member of P -> full
				// visibility of P's rows (no user pin); P=="" (the no-project
				// bucket) is never in the member set, so it naturally falls through
				// to the own-rows fallback below, matching "a non-member sees only
				// their own P-rows".
				if member[q.ProjectIDExact] {
					q.ScopeAll = true
					q.UserID = ""
					return
				}
			} else {
				// Top-level group-by-project (no exact filter): scope to every
				// member project, never wider. A caller who is a member of ZERO
				// projects gets a non-nil EMPTY ProjectIDs, which both stores
				// enforce as "match zero rows" (never falls back to all-scope).
				q.ScopeAll = true
				q.UserID = ""
				q.ProjectIDs = sortedProjectIDs(member)
				return
			}
		}
		// Fail-open on a memberProjectIDs store error, or "not a member of the
		// filtered project": fall through to the own-rows pin below — the
		// project-scope widening NEVER activates on an error (never accidentally
		// broaden visibility because a dependency failed).
	}
	q.ScopeAll = false
	q.UserID = principal.UserID
}

// sortedProjectIDs returns the keys of a project-membership set in a stable
// sorted order, so Query.ProjectIDs (and thus the SQL IN-list's
// positional args) is deterministic.
func sortedProjectIDs(member map[string]bool) []string {
	out := make([]string, 0, len(member))
	for id := range member {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (s *Service) Dashboard(ctx context.Context, token auth.Token) DashboardResponse {
	now := s.clock().UTC()
	cutoff := now.Add(-24 * time.Hour)
	events := s.usage.ByUser(token.UserID)
	latencies := make([]int64, 0)
	requests := 0
	tokens := 0
	for _, event := range events {
		if event.CreatedAt.Before(cutoff) {
			continue
		}
		requests++
		tokens += event.TotalTokens
		latencies = append(latencies, event.LatencyMS)
	}
	healthyHosts := "mock"
	routes := []RouteDTO{{Model: "qwen-coder", Provider: "mock", Host: "mock-host", Status: "active"}}
	if s.routes != nil {
		healthyHosts, routes = s.dashboardRouteData(ctx, token)
	}
	return DashboardResponse{
		Metrics: DashboardMetrics{
			Requests24h:  requests,
			Tokens24h:    tokens,
			HealthyHosts: healthyHosts,
			LatencyP95MS: percentile95(latencies),
		},
		Routes: routes,
	}
}

// Models returns the offered-model listing for the inference /v1/models list,
// the chat picker, and override validation: hidden/locked models are SUPPRESSED
// from the standalone listing (active groups are still added).
func (s *Service) Models(ctx context.Context, token auth.Token) ModelsResponse {
	return s.modelsResponse(ctx, token, true)
}

// ManageModels returns the UNSUPPRESSED offered-model listing for the admin
// management surface (the ModelList visibility editor + the ModelGroupSection
// member picker): every active gateway model is INCLUDED regardless of its
// model_settings.visibility (each carries its Visibility so a hidden/locked
// model can be reverted or added to a group), plus the active groups. It never
// feeds the inference /v1/models list or the chat picker.
func (s *Service) ManageModels(ctx context.Context, token auth.Token) ModelsResponse {
	return s.modelsResponse(ctx, token, false)
}

// modelsResponse builds the offered-model listing. When suppress is true the
// hidden/locked models are dropped from the standalone listing (Models(); the
// inference/chat path); when false they are retained (ManageModels(); the admin
// management path). Group additions and the Visibility/IsGroup fields are
// unconditional in both modes.
func (s *Service) modelsResponse(ctx context.Context, token auth.Token, suppress bool) ModelsResponse {
	if s.routes != nil {
		// suppress==true is exactly the two usage-facing callers (Models()) —
		// apply the resource-group provisioning visibility filter (Resource
		// Groups Phase 2) there. suppress==false is ManageModels(), the admin
		// management surface, which stays UNFILTERED by design.
		fetch := s.activeMappingViews
		if suppress {
			fetch = func(ctx context.Context) ([]mappingView, error) { return s.visibleMappingViews(ctx, token) }
		}
		if views, err := fetch(ctx); err == nil {
			// Derive both the per-model flavor set and the loaded-state from a
			// single pass over the active mapping views (one store round-trip).
			flavors := make(map[string]map[string]struct{})
			// loadedOn: gateway model name -> set of server names where a mapping's
			// upstream (app) model name is currently reported loaded.
			loadedOn := make(map[string]map[string]struct{})
			// offeredOn: gateway model name -> set of server NAMES that offer it (an active
			// mapping on an active + reachable app). Server names mirror loadedOn.
			offeredOn := make(map[string]map[string]struct{})
			// contextSizeOn: gateway model name -> MIN known (>0) mapping context_size; 0 = unknown.
			contextSizeOn := make(map[string]int)
			// visionOn: gateway model name -> AND of vision_capable across ALL of the
			// model's offering mappings; fail-closed (a model starts true on first
			// sight and only ever gets AND'd down, never up).
			visionOn := make(map[string]bool)
			for _, view := range views {
				name := view.mapping.GatewayModelName
				if _, ok := flavors[name]; !ok {
					flavors[name] = make(map[string]struct{})
					visionOn[name] = true
				}
				if _, ok := offeredOn[name]; !ok {
					offeredOn[name] = make(map[string]struct{})
				}
				offeredOn[name][view.server.Name] = struct{}{}
				visionOn[name] = visionOn[name] && view.mapping.VisionCapable
				if cs := view.mapping.ContextSize; cs > 0 {
					if cur, ok := contextSizeOn[name]; !ok || cs < cur {
						contextSizeOn[name] = cs
					}
				}
				for _, flavor := range view.app.APIFlavors {
					if isKnownAPIFlavor(flavor) {
						flavors[name][flavor] = struct{}{}
					}
				}
				if s.loadedModels != nil && view.mapping.AppModelName != "" {
					for _, loaded := range s.loadedModels.LoadedAppModels(view.app.ID, view.server.ID) {
						if loaded == view.mapping.AppModelName {
							if _, ok := loadedOn[name]; !ok {
								loadedOn[name] = make(map[string]struct{})
							}
							loadedOn[name][view.server.Name] = struct{}{}
							break
						}
					}
				}
			}
			// Per-model visibility from model_settings (default "shown" when no row).
			visibility := make(map[string]string)
			if settings, sErr := s.routes.ModelSettings(ctx); sErr == nil {
				for _, st := range settings {
					if st.Visibility != "" {
						visibility[st.GatewayModelName] = st.Visibility
					}
				}
			}
			// Model-group offering overlay (spec §4a/§4b): add active groups as
			// synthetic models, and drop hidden/locked models from the standalone
			// listing. Fails open (proceed without the overlay) on a store error.
			isGroup := make(map[string]struct{})
			if entries, suppressSet, gErr := s.modelGroupOverlay(ctx, flavors); gErr == nil {
				// A group is "loaded" iff its highest-priority OFFERABLE member is
				// loaded -- except for a loaded_only group, where ANY loaded member
				// is what will actually be served, so any of them makes it loaded
				// (LoadedOn is then the union of those members' servers). Capture
				// that BEFORE suppressing hidden/locked names — a hidden member is
				// still a full group member.
				groupLoaded := make(map[string]map[string]struct{}, len(entries))
				for _, e := range entries {
					if len(e.OrderedOfferableMembers) == 0 {
						continue
					}
					if e.LoadedOnly {
						union := make(map[string]struct{})
						for _, member := range e.OrderedOfferableMembers {
							for srv := range loadedOn[member] {
								union[srv] = struct{}{}
							}
						}
						if len(union) > 0 {
							groupLoaded[e.Name] = union
						}
						continue
					}
					if servers := loadedOn[e.OrderedOfferableMembers[0]]; len(servers) > 0 {
						groupLoaded[e.Name] = servers
					}
				}
				// A group is "offered on" the UNION of its offerable members' servers.
				// Captured before suppression — a hidden member is still a full group member.
				groupOffered := make(map[string]map[string]struct{}, len(entries))
				for _, e := range entries {
					union := make(map[string]struct{})
					for _, member := range e.OrderedOfferableMembers {
						for srv := range offeredOn[member] {
							union[srv] = struct{}{}
						}
					}
					if len(union) > 0 {
						groupOffered[e.Name] = union
					}
				}
				// A group's context_size is the MIN known (>0) context_size across its
				// offerable members (same conservative rule as a plain model, applied
				// over the member set). Captured before suppression, mirroring groupOffered.
				groupContext := make(map[string]int, len(entries))
				for _, e := range entries {
					minContext := 0
					for _, member := range e.OrderedOfferableMembers {
						if cs := contextSizeOn[member]; cs > 0 && (minContext == 0 || cs < minContext) {
							minContext = cs
						}
					}
					if minContext > 0 {
						groupContext[e.Name] = minContext
					}
				}
				// A group's vision flag is the AND of its offerable members' vision
				// flags (same fail-closed rule as a plain model); an EMPTY offerable
				// member set is false (fail-closed — never claim vision on a group
				// with nothing to serve it). Captured before suppression, mirroring
				// groupOffered/groupContext.
				groupVision := make(map[string]bool, len(entries))
				for _, e := range entries {
					all := len(e.OrderedOfferableMembers) > 0
					for _, member := range e.OrderedOfferableMembers {
						if !visionOn[member] {
							all = false
							break
						}
					}
					groupVision[e.Name] = all
				}
				// Suppress hidden/locked models from the standalone listing (the
				// inference/chat path). The admin management path (suppress==false)
				// retains them so they stay editable / group-addable.
				if suppress {
					for name := range suppressSet {
						delete(flavors, name)
						delete(loadedOn, name)
					}
				}
				// Add the group synthetic models (name → union flavors + top-member
				// loaded state) so the shared assembly below renders them uniformly.
				// In the suppressed path (Models(); inference/chat) a hidden/locked
				// group is dropped from the standalone listing, mirroring a non-shown
				// MODEL; the admin management path (suppress==false; ManageModels())
				// retains it so a hidden/locked group stays visible + revertible. Its
				// per-name Visibility on the DTO is filled below from the settings map.
				for _, e := range entries {
					if suppress && e.Visibility != "shown" {
						continue
					}
					flavors[e.Name] = e.Flavors
					if servers := groupOffered[e.Name]; servers != nil {
						offeredOn[e.Name] = servers
					}
					if servers := groupLoaded[e.Name]; servers != nil {
						loadedOn[e.Name] = servers
					}
					if cs := groupContext[e.Name]; cs > 0 {
						contextSizeOn[e.Name] = cs
					}
					visionOn[e.Name] = groupVision[e.Name]
					isGroup[e.Name] = struct{}{}
				}
			}
			ids := make([]string, 0, len(flavors))
			for id := range flavors {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			out := make([]ModelDTO, 0, len(ids))
			for _, id := range ids {
				dto := ModelDTO{ID: id, DisplayName: id, Flavors: sortedFlavorSet(flavors[id]), Visibility: "shown"}
				if servers := loadedOn[id]; len(servers) > 0 {
					dto.Loaded = true
					dto.LoadedOn = sortedStringSet(servers)
				}
				dto.OfferedOnCount = len(offeredOn[id])
				dto.ContextSize = contextSizeOn[id]
				dto.Vision = visionOn[id]
				if v, ok := visibility[id]; ok {
					dto.Visibility = v
				}
				if _, ok := isGroup[id]; ok {
					dto.IsGroup = true
				}
				out = append(out, dto)
			}
			return ModelsResponse{Data: out}
		}
	}
	out := make([]ModelDTO, 0, len(seedModelNames))
	for _, name := range seedModelNames {
		out = append(out, ModelDTO{ID: name, DisplayName: name, Flavors: allKnownFlavorsSorted(), Visibility: "shown"})
	}
	return ModelsResponse{Data: out}
}

func (s *Service) dashboardRouteData(ctx context.Context, token auth.Token) (string, []RouteDTO) {
	servers, err := s.routes.AIServers(ctx)
	if err != nil {
		return "0/0", []RouteDTO{}
	}
	healthy := 0
	for _, server := range servers {
		if server.Status == routing.ServerStatusActive && server.HealthStatus == routing.HealthHealthy {
			healthy++
		}
	}
	metric := strconv.Itoa(healthy) + "/" + strconv.Itoa(len(servers))
	// visibleMappingViews (not the token-less activeMappingViews) so a
	// non-provisioned principal's "Live Model Routes" table never surfaces a
	// {model, host} pair on a server they may not use (Resource Groups Phase 2
	// — Fix round 1; GET /api/portal/dashboard is reachable to ANY gateway:use
	// token, not admin-gated, so this table was the one remaining leak of what
	// Models()/ModelServers() already hide). The healthy/total server COUNT
	// above is left unscoped — it names no model or host, only an aggregate
	// fleet-wide tally.
	views, err := s.visibleMappingViews(ctx, token)
	if err != nil {
		return metric, []RouteDTO{}
	}
	// Hidden/locked suppression (security fix — closes the gap the
	// VISIBILITY-SURFACE MATRIX below used to flag as a TODO): a non-admin
	// must not see a hidden/locked model's route here either, mirroring
	// modelsResponse(suppress=true)'s drop from Models(). Computed once per
	// call, and only for a non-admin — an admin (isAdmin) sees every route
	// unfiltered, same as ManageModels(). For an admin visByLower stays nil, so
	// the nil-map lookup below returns "" and isHiddenOrLocked never suppresses.
	// For a non-admin it fails CLOSED on a ModelSettings store error: rather
	// than fall back to no suppression and leak hidden/locked routes during a
	// blip, drop all routes (same empty-on-error shape this function already
	// returns for a visibleMappingViews failure just above, and consistent with
	// modelGroupOverlay, which propagates the identical error out of Models()).
	var visByLower map[string]string
	if !isAdmin(token) {
		var visErr error
		visByLower, visErr = s.modelVisibilityByLower(ctx)
		if visErr != nil {
			return metric, []RouteDTO{}
		}
	}
	out := make([]RouteDTO, 0, len(views))
	for _, view := range views {
		if isHiddenOrLocked(visByLower[strings.ToLower(strings.TrimSpace(view.mapping.GatewayModelName))]) {
			continue
		}
		out = append(out, RouteDTO{
			ID:       view.mapping.ID,
			Model:    view.mapping.GatewayModelName,
			Provider: view.app.Type,
			Host:     view.server.Name,
			Status:   routing.ServerStatusActive,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model == out[j].Model {
			return out[i].ID < out[j].ID
		}
		return out[i].Model < out[j].Model
	})
	return metric, out
}

// ListServers returns every server the principal may manage: system-scope
// sees ALL (unconditional bypass); anyone else sees the union of
// ServersByAdminGroups(serverManageGroupIDs) and ServersByOwner(principal),
// deduped by server id (first occurrence wins: group-linked before owned).
// An ungrouped legacy server (no server_admin_groups row) surfaces for a
// non-system caller ONLY via ServerOwners -- Phase B, spec 2026-08-10.
func (s *Service) ListServers(ctx context.Context, principal auth.Token) (ServerListResponse, error) {
	var servers []routing.AIServer
	if isSystem(principal) {
		all, err := s.routes.AIServers(ctx)
		if err != nil {
			return ServerListResponse{}, err
		}
		servers = all
	} else {
		manageGroups, err := s.serverManageGroupIDs(ctx, principal)
		if err != nil {
			return ServerListResponse{}, err
		}
		groupIDs := make([]string, 0, len(manageGroups))
		for gid := range manageGroups {
			groupIDs = append(groupIDs, gid)
		}
		var byGroup []routing.AIServer
		if len(groupIDs) > 0 {
			byGroup, err = s.routes.ServersByAdminGroups(ctx, groupIDs)
			if err != nil {
				return ServerListResponse{}, err
			}
		}
		byOwner, err := s.routes.ServersByOwner(ctx, principal.UserID)
		if err != nil {
			return ServerListResponse{}, err
		}
		seen := make(map[string]bool, len(byGroup)+len(byOwner))
		servers = make([]routing.AIServer, 0, len(byGroup)+len(byOwner))
		for _, list := range [][]routing.AIServer{byGroup, byOwner} {
			for _, srv := range list {
				if seen[srv.ID] {
					continue
				}
				seen[srv.ID] = true
				servers = append(servers, srv)
			}
		}
	}
	out := make([]ServerDTO, 0, len(servers))
	for _, srv := range servers {
		dto, err := s.serverDTO(ctx, srv)
		if err != nil {
			return ServerListResponse{}, err
		}
		out = append(out, dto)
	}
	return ServerListResponse{Data: out}, nil
}

// CreateServer creates an AI-server. Authorization (Phase B, spec
// 2026-08-10): allowed for a system-scope principal OR one who may manage
// servers through at least one admin group (serverManageGroupIDs); a
// principal with neither gets ErrServerForbidden. Every create -- regardless
// of scope -- must additionally link the server to >=1 existing admin-tier
// group (req.AdminGroupIDs, validated by validateAdminGroupIDs); a rejection
// there happens BEFORE the server row is created, so a rejected create never
// leaves an orphan.
func (s *Service) CreateServer(ctx context.Context, principal auth.Token, req CreateServerRequest) (ServerDTO, error) {
	if !isSystem(principal) {
		manageGroups, err := s.serverManageGroupIDs(ctx, principal)
		if err != nil {
			return ServerDTO{}, err
		}
		if len(manageGroups) == 0 {
			return ServerDTO{}, ErrServerForbidden
		}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return ServerDTO{}, ErrServerNameRequired
	}
	// NetBird is honored only when the module is enabled + configured; otherwise
	// the flag is forced false (defensive). A NetBird server's domain is
	// auto-managed by the peer-sync loop, so the required-domain check is skipped
	// only when NetBird will actually manage it.
	netbirdCfg, netbirdOK, _ := s.NetbirdConfig(ctx)
	netbirdEnabled := req.NetbirdEnabled && netbirdOK
	// NetBird-only transport: a normal admin cannot create an off-mesh server (it would be
	// unreachable via the netbird_only outbound refusal). Force the flag on. A system-admin
	// (the "system" scope) may deliberately override, so it is exempt.
	if netbirdOK && !isSystem(principal) && s.NetbirdOnly(ctx) {
		netbirdEnabled = true
	}
	// NetBird policy override + enforcement. A non-system-admin can never opt a
	// server OUT of management, and under deny-by-default + "selected" scope a
	// non-included server has no access policy → force opt-in. System-admins exempt.
	policyOverride := normalizeNetbirdPolicyOverride(req.NetbirdPolicyOverride)
	if netbirdEnabled && !isSystem(principal) {
		pc := s.NetbirdPolicyContext(ctx)
		if pc.ManagePolicies {
			if policyOverride == "exclude" {
				policyOverride = ""
			}
			if pc.EffectivePolicyScope == "selected" && pc.DenyByDefault {
				policyOverride = "include"
			}
		}
	}
	domain := strings.TrimSpace(req.Domain)
	if domain == "" && !netbirdEnabled {
		return ServerDTO{}, ErrServerDomainRequired
	}
	status, err := normalizeServerStatus(req.Status)
	if err != nil {
		return ServerDTO{}, err
	}
	ownerIDs, err := s.validateOwnerIDs(ctx, req.OwnerIDs)
	if err != nil {
		return ServerDTO{}, err
	}
	serverPath, err := checkPathSuffix(req.ServerPathSuffix)
	if err != nil {
		return ServerDTO{}, err
	}
	// Admin-group linkage (Phase B, spec 2026-08-10): validated LAST among the
	// request-shape checks, still strictly BEFORE the server row is created --
	// see the function doc.
	adminGroupIDs, systemGroupID, err := s.validateAdminGroupIDs(ctx, principal, req.AdminGroupIDs, req.SystemGroupID)
	if err != nil {
		return ServerDTO{}, err
	}
	agentPresenceTimeout := 0
	if req.AgentPresenceTimeoutSeconds != nil {
		if *req.AgentPresenceTimeoutSeconds < 0 {
			return ServerDTO{}, ErrServerAgentPresenceTimeoutInvalid
		}
		agentPresenceTimeout = *req.AgentPresenceTimeoutSeconds
	}
	var estimatedWatts, idleWatts, pricePerKwh, pue float64
	if req.EstimatedWatts != nil {
		if *req.EstimatedWatts < 0 {
			return ServerDTO{}, ErrServerEnergyConfigInvalid
		}
		estimatedWatts = *req.EstimatedWatts
	}
	if req.IdleWatts != nil {
		if *req.IdleWatts < 0 {
			return ServerDTO{}, ErrServerEnergyConfigInvalid
		}
		idleWatts = *req.IdleWatts
	}
	if req.PricePerKwh != nil {
		if *req.PricePerKwh < 0 {
			return ServerDTO{}, ErrServerEnergyConfigInvalid
		}
		pricePerKwh = *req.PricePerKwh
	}
	if req.Pue != nil {
		if *req.Pue < 0 {
			return ServerDTO{}, ErrServerEnergyConfigInvalid
		}
		pue = *req.Pue
	}
	priceUnit := ""
	if req.PriceUnit != nil {
		priceUnit = *req.PriceUnit
	}
	priceUnit = NormalizePriceUnit(priceUnit)
	now := s.clock().UTC()
	server := routing.AIServer{
		ID: "srv_" + compactRandomHex(16), Name: name, Domain: domain, ServerPathSuffix: serverPath,
		Status: status, HealthStatus: routing.HealthUnknown, NetbirdEnabled: netbirdEnabled,
		NetbirdPolicyOverride:       policyOverride,
		AgentPresenceTimeoutSeconds: agentPresenceTimeout,
		EstimatedWatts:              estimatedWatts,
		IdleWatts:                   idleWatts,
		PricePerKwh:                 pricePerKwh,
		Pue:                         pue,
		PriceUnit:                   priceUnit,
		CreatedAt:                   now, UpdatedAt: now,
	}
	if err := s.routes.CreateAIServer(ctx, server); err != nil {
		return ServerDTO{}, err
	}
	if err := s.routes.SetServerOwners(ctx, server.ID, ownerIDs); err != nil {
		return ServerDTO{}, err
	}
	// Admin-group linkage persist (Phase B, spec 2026-08-10): AFTER the server
	// row exists (server_admin_groups.server_id is an FK), BEFORE returning.
	if err := s.routes.UpdateServerSystemGroup(ctx, server.ID, systemGroupID); err != nil {
		return ServerDTO{}, err
	}
	server.SystemGroupID = systemGroupID
	for _, gid := range adminGroupIDs {
		if err := s.routes.SetServerAdminGroup(ctx, server.ID, gid); err != nil {
			return ServerDTO{}, err
		}
	}
	// Best-effort NetBird setup-key generation. The server ALREADY exists; any
	// failure is surfaced via NetbirdError and NEVER fails the create.
	if netbirdEnabled {
		sk, hookErr := s.generateNetbirdSetupKey(ctx, netbirdCfg, server, "")
		if hookErr != nil {
			dto, dtoErr := s.serverDTO(ctx, server)
			if dtoErr != nil {
				return ServerDTO{}, dtoErr
			}
			dto.NetbirdError = hookErr.Error()
			return dto, nil
		}
		server.NetbirdSetupKeyID = sk.ID
		// Mirror the hook's store writes onto the local struct so the create response
		// DTO reflects them (the setup key marks the peer portal-managed).
		server.NetbirdPeerManaged = true
		dto, dtoErr := s.serverDTO(ctx, server)
		if dtoErr != nil {
			return ServerDTO{}, dtoErr
		}
		dto.NetbirdSetupKey = sk.Key
		dto.NetbirdSetupCommand = netbirdSetupCommand(netbirdCfg.URL, sk.Key)
		return dto, nil
	}
	return s.serverDTO(ctx, server)
}

func (s *Service) GetServer(ctx context.Context, principal auth.Token, id string) (ServerDTO, error) {
	server, err := s.authorizeServer(ctx, principal, id)
	if err != nil {
		return ServerDTO{}, err
	}
	return s.serverDTO(ctx, server)
}

func (s *Service) UpdateServer(ctx context.Context, principal auth.Token, id string, req UpdateServerRequest) (ServerDTO, error) {
	server, err := s.authorizeServer(ctx, principal, id)
	if err != nil {
		return ServerDTO{}, err
	}
	if req.OwnerIDs != nil && !isAdmin(principal) {
		return ServerDTO{}, ErrServerForbidden
	}
	// Validate everything that can fail BEFORE persisting anything.
	var ownerIDs []string
	if req.OwnerIDs != nil {
		ownerIDs, err = s.validateOwnerIDs(ctx, *req.OwnerIDs)
		if err != nil {
			return ServerDTO{}, err
		}
	}
	nameChanged := false
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return ServerDTO{}, ErrServerNameRequired
		}
		nameChanged = name != server.Name
		server.Name = name
	}
	if req.Domain != nil {
		domain := strings.TrimSpace(*req.Domain)
		if domain == "" {
			return ServerDTO{}, ErrServerDomainRequired
		}
		server.Domain = domain
	}
	if req.ServerPathSuffix != nil {
		v, err := checkPathSuffix(*req.ServerPathSuffix)
		if err != nil {
			return ServerDTO{}, err
		}
		server.ServerPathSuffix = v
	}
	if req.Status != nil {
		status, err := normalizeServerStatus(*req.Status)
		if err != nil {
			return ServerDTO{}, err
		}
		server.Status = status
	}
	if req.NetbirdEnabled != nil {
		// Turning the flag ON is honored only when the module is enabled +
		// configured; turning it OFF is always allowed. UpdateServer never
		// generates a setup key (that is create/regenerate only).
		server.NetbirdEnabled = *req.NetbirdEnabled && s.NetbirdModuleEnabled(ctx)
	}
	if req.AgentPresenceTimeoutSeconds != nil {
		if *req.AgentPresenceTimeoutSeconds < 0 {
			return ServerDTO{}, ErrServerAgentPresenceTimeoutInvalid
		}
		server.AgentPresenceTimeoutSeconds = *req.AgentPresenceTimeoutSeconds
	}
	if req.EstimatedWatts != nil {
		if *req.EstimatedWatts < 0 {
			return ServerDTO{}, ErrServerEnergyConfigInvalid
		}
		server.EstimatedWatts = *req.EstimatedWatts
	}
	if req.IdleWatts != nil {
		if *req.IdleWatts < 0 {
			return ServerDTO{}, ErrServerEnergyConfigInvalid
		}
		server.IdleWatts = *req.IdleWatts
	}
	if req.PricePerKwh != nil {
		if *req.PricePerKwh < 0 {
			return ServerDTO{}, ErrServerEnergyConfigInvalid
		}
		server.PricePerKwh = *req.PricePerKwh
	}
	if req.Pue != nil {
		if *req.Pue < 0 {
			return ServerDTO{}, ErrServerEnergyConfigInvalid
		}
		server.Pue = *req.Pue
	}
	if req.PriceUnit != nil {
		server.PriceUnit = NormalizePriceUnit(*req.PriceUnit)
	}
	// A non-NetBird server must have a domain (mirrors the create-path guard). Only
	// a NetBird-enabled server may have an empty domain (auto-managed by the sync
	// loop); disabling NetBird on an as-yet-unsynced (empty-domain) server without
	// supplying one would otherwise leave a domainless server the sync then ignores.
	if !server.NetbirdEnabled && server.Domain == "" {
		return ServerDTO{}, ErrServerDomainRequired
	}
	server.UpdatedAt = s.clock().UTC()
	if err := s.routes.UpdateAIServer(ctx, server); err != nil {
		return ServerDTO{}, err
	}
	if req.OwnerIDs != nil {
		if err := s.routes.SetServerOwners(ctx, server.ID, ownerIDs); err != nil {
			return ServerDTO{}, err
		}
	}
	// A NetBird-linked server's name IS its peer's DNS name: on a rename, rename the
	// peer NOW and pull the domain to the peer's new dns_label so both take effect
	// immediately (the periodic sync loop is otherwise up to one interval behind).
	// Best-effort: any NetBird error leaves the stored name saved + the domain as-is;
	// the sync loop reconciles later.
	if nameChanged && server.NetbirdEnabled && server.NetbirdPeerID != "" {
		server.Domain, server.NetbirdConnected = s.reconcileServerPeerName(ctx, server.ID, server.NetbirdPeerID, server.Name, server.Domain, server.NetbirdConnected)
	}
	return s.serverDTO(ctx, server)
}

// SetServerAdminGroups replaces a server's linked admin-group set (Phase B,
// spec 2026-08-10 -- the linkage editor's write path). authorizeServer gates
// FIRST (404-no-leak: only a current owner/manager/system principal may see
// or edit the linkage at all), THEN the new set is validated by the SAME
// rules CreateServer uses (validateAdminGroupIDs: each id existing
// ADMIN-tier +, for a non-system caller, in serverManageGroupIDs; every
// chosen group sharing one parent; >=1 required). The delta vs the server's
// CURRENT admin groups is applied (SetServerAdminGroup for additions,
// RemoveServerAdminGroup for removals).
//
// Containment root is IMMUTABLE once set (spec non-goal: "Kein Reparenting
// der System-Gruppe eines Servers ueber verschiedene Tenants"): for an
// ALREADY-grouped server (server.SystemGroupID != "") the new set's derived
// common parent must equal the server's CURRENT root, or the call is
// rejected as ErrServerAdminGroupParentMismatch -- this is checked
// EXPLICITLY below, independent of the caller's scope (NOT via
// validateAdminGroupIDs's systemGroupHint parameter, which only applies its
// cross-check under system-scope -- a plain admin who happens to
// own/co-manage admin groups in two different tenants would otherwise be
// able to swap a grouped server's linked groups for ones under a DIFFERENT
// system group and thereby relocate its containment root; that is exactly
// the scenario this guard closes, for EVERY principal, including system).
// UpdateServerSystemGroup therefore fires ONLY the very first time an
// UNGROUPED legacy server (SystemGroupID=="") gets its first link -- once
// set, the guard above holds it fixed on every later call.
func (s *Service) SetServerAdminGroups(ctx context.Context, principal auth.Token, id string, groupIDs []string) (ServerDTO, error) {
	server, err := s.authorizeServer(ctx, principal, id)
	if err != nil {
		return ServerDTO{}, err
	}
	ids, systemGroupID, err := s.validateAdminGroupIDs(ctx, principal, groupIDs, "")
	if err != nil {
		return ServerDTO{}, err
	}
	if server.SystemGroupID != "" && systemGroupID != server.SystemGroupID {
		return ServerDTO{}, ErrServerAdminGroupParentMismatch
	}
	current, err := s.routes.ServerAdminGroups(ctx, id)
	if err != nil {
		return ServerDTO{}, err
	}
	currentSet := make(map[string]bool, len(current))
	for _, gid := range current {
		currentSet[gid] = true
	}
	wantSet := make(map[string]bool, len(ids))
	for _, gid := range ids {
		wantSet[gid] = true
	}
	for _, gid := range ids {
		if !currentSet[gid] {
			if err := s.routes.SetServerAdminGroup(ctx, id, gid); err != nil {
				return ServerDTO{}, err
			}
		}
	}
	for _, gid := range current {
		if !wantSet[gid] {
			if err := s.routes.RemoveServerAdminGroup(ctx, id, gid); err != nil {
				return ServerDTO{}, err
			}
		}
	}
	// Reached ONLY on the first grouping of a previously-ungrouped server
	// (server.SystemGroupID=="" here): the immutability guard above already
	// rejected any attempt to move an ALREADY-grouped server's root, so this
	// branch can no longer fire on a later call for the same server.
	if systemGroupID != server.SystemGroupID {
		if err := s.routes.UpdateServerSystemGroup(ctx, id, systemGroupID); err != nil {
			return ServerDTO{}, err
		}
		server.SystemGroupID = systemGroupID
	}
	return s.serverDTO(ctx, server)
}

// ServerAdminGroupCandidates lists the admin-tier groups the caller may
// create/link a server into (drives the create-server / linkage-editor
// picker's auto-select-one / mandatory-choose-many / no-groups-hint logic).
// A system-scope principal gets EVERY admin-tier group (may link into any of
// them, per validateAdminGroupIDs); anyone else gets exactly the groups
// serverManageGroupIDs returns (owner or can_manage_servers co-manager).
func (s *Service) ServerAdminGroupCandidates(ctx context.Context, principal auth.Token) ([]AdminGroupCandidateDTO, error) {
	var groups []store.UserGroup
	if isSystem(principal) {
		all, err := s.groups.ListUserGroupsByTier(ctx, store.GroupTierAdmin)
		if err != nil {
			return nil, err
		}
		groups = all
	} else {
		manageGroups, err := s.serverManageGroupIDs(ctx, principal)
		if err != nil {
			return nil, err
		}
		for gid := range manageGroups {
			g, err := s.groups.UserGroupByID(ctx, gid)
			if err != nil {
				// linked group vanished between the enumeration and this
				// lookup; skip rather than fail the whole candidate list.
				continue
			}
			groups = append(groups, g)
		}
	}
	out := make([]AdminGroupCandidateDTO, 0, len(groups))
	for _, g := range groups {
		parentName := ""
		if g.ParentGroupID != "" {
			if parent, err := s.groups.UserGroupByID(ctx, g.ParentGroupID); err == nil {
				parentName = parent.Name
			}
		}
		out = append(out, AdminGroupCandidateDTO{
			ID: g.ID, Name: g.Name, ParentGroupID: g.ParentGroupID, ParentGroupName: parentName,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// AdminGroupCandidateDTO is a no-leak reference to an admin-tier group the
// caller may create/link a server into, plus its containment root's id/name
// (the server's future SystemGroupID) -- Phase B, spec 2026-08-10.
type AdminGroupCandidateDTO struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	ParentGroupID   string `json:"parent_group_id"`
	ParentGroupName string `json:"parent_group_name"`
}

// reconcileServerPeerName is the synchronous, best-effort NetBird reconcile run when
// a linked server's name changes: it renames the server's peer to match serverName
// (when it differs) and updates the stored domain to the peer's (new) dns_label +
// connected. It returns the (domain, connected) it persisted so the caller can
// refresh its in-memory server for the response DTO; on ANY NetBird error (module
// off, GetPeer/rename/persist failure) it returns the passed-in (domain, connected)
// unchanged — the sync loop is the backstop, and a rename failure never fails the
// edit.
func (s *Service) reconcileServerPeerName(ctx context.Context, id, peerID, serverName, domain string, connected bool) (string, bool) {
	cfg, ok, cErr := s.NetbirdConfig(ctx)
	if !ok || cErr != nil {
		return domain, connected
	}
	ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
	peer, gErr := netbird.GetPeer(ctx, ncfg, netbirdCallTimeout, peerID)
	if gErr != nil {
		slog.Debug("netbird: server-rename reconcile: get peer failed", "server_id", id, "error", gErr)
		return domain, connected
	}
	if peer.Name != serverName {
		if renamed, rErr := netbird.UpdatePeerName(ctx, ncfg, netbirdCallTimeout, peer, serverName); rErr == nil {
			peer = renamed
		} else {
			slog.Debug("netbird: server-rename reconcile: rename failed", "server_id", id, "error", rErr)
		}
	}
	newDomain := domain
	if peer.DNSLabel != "" {
		newDomain = peer.DNSLabel
	}
	if uErr := s.routes.UpdateServerNetbirdState(ctx, id, newDomain, peer.ID, peer.Connected); uErr != nil {
		slog.Debug("netbird: server-rename reconcile: persist state failed", "server_id", id, "error", uErr)
		return domain, connected
	}
	return newDomain, peer.Connected
}

// DeleteServer removes an AI-server row. It always best-effort deletes the
// per-server tracking group ("op-gw-<id>") from NetBird first (unchanged). When
// deletePeer is set AND the module is configured, it additionally deletes the
// NetBird PEER + SETUP KEY (opt-in, from the delete-server checkbox). Every
// NetBird call is best-effort + timeout-bounded: a failure is reported via the
// returned netbirdCleanupFailed flag but NEVER fails the row delete.
func (s *Service) DeleteServer(ctx context.Context, principal auth.Token, id string, deletePeer bool) (bool, error) {
	server, err := s.authorizeServer(ctx, principal, id)
	if err != nil {
		return false, err
	}
	// Best-effort: delete the per-server tracking group ("op-gw-<id>") from NetBird
	// before removing the row. A NetBird error must NOT fail the delete (log + drop).
	if server.NetbirdGroupID != "" {
		if cfg, ok, cErr := s.NetbirdConfig(ctx); ok && cErr == nil {
			ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
			if dErr := netbird.DeleteGroup(ctx, ncfg, netbirdCallTimeout, server.NetbirdGroupID); dErr != nil {
				slog.Debug("netbird: delete tracking group on server delete failed", "server_id", server.ID, "group_id", server.NetbirdGroupID, "error", dErr)
			}
			// Best-effort: also delete the server's access policy ("op-gw-access-<id>").
			// A NetBird error is logged inside deleteServerPolicy and never fails the delete.
			s.deleteServerPolicy(ctx, ncfg, server.ID)
		}
	}
	// Opt-in peer + setup-key cleanup (best-effort, module permitting). A failure of
	// either is recorded in netbirdCleanupFailed but never aborts the row delete.
	netbirdCleanupFailed := false
	if deletePeer {
		if cfg, ok, cErr := s.NetbirdConfig(ctx); ok && cErr == nil {
			ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
			if server.NetbirdPeerID != "" {
				if dErr := netbird.DeletePeer(ctx, ncfg, netbirdCallTimeout, server.NetbirdPeerID); dErr != nil {
					netbirdCleanupFailed = true
					slog.Debug("netbird: delete peer on server delete failed", "server_id", server.ID, "peer_id", server.NetbirdPeerID, "error", dErr)
				}
			}
			if server.NetbirdSetupKeyID != "" {
				if dErr := netbird.DeleteSetupKey(ctx, ncfg, netbirdCallTimeout, server.NetbirdSetupKeyID); dErr != nil {
					netbirdCleanupFailed = true
					slog.Debug("netbird: delete setup key on server delete failed", "server_id", server.ID, "setup_key_id", server.NetbirdSetupKeyID, "error", dErr)
				}
			}
		}
	}
	if err := s.routes.DeleteAIServer(ctx, server.ID); err != nil {
		return false, err
	}
	return netbirdCleanupFailed, nil
}

// netbirdCallTimeout bounds every NetBird admin-API call from the portal
// (create hook + regenerate + test). NetBird calls must never use
// http.DefaultClient (no timeout) or block a request indefinitely.
const netbirdCallTimeout = 15 * time.Second

// SetServerEnergyConfig writes ONLY the five per-server energy-config columns
// (estimated_watts, idle_watts, price_per_kwh, pue, price_unit) — the backing
// call for the dedicated Save button on the server edit form's "Energy & cost"
// section. Owner/admin-scoped via authorizeServer (an unknown/unauthorized id
// returns ErrServerNotFound → 404 no-leak). Each numeric value must be >= 0
// (else ErrServerEnergyConfigInvalid → 400); priceUnit is normalized via
// NormalizePriceUnit (unknown/empty → the default). A full-replace of the five
// columns (the section always edits them together), so it never clobbers a
// sibling column.
func (s *Service) SetServerEnergyConfig(ctx context.Context, principal auth.Token, id string, estimatedWatts, idleWatts, pricePerKwh, pue float64, priceUnit string) (ServerDTO, error) {
	if _, err := s.authorizeServer(ctx, principal, id); err != nil {
		return ServerDTO{}, err
	}
	if estimatedWatts < 0 || idleWatts < 0 || pricePerKwh < 0 || pue < 0 {
		return ServerDTO{}, ErrServerEnergyConfigInvalid
	}
	if err := s.routes.UpdateServerEnergyConfig(ctx, id, estimatedWatts, idleWatts, pricePerKwh, pue, NormalizePriceUnit(priceUnit)); err != nil {
		return ServerDTO{}, err
	}
	server, err := s.routes.AIServerByID(ctx, id)
	if err != nil {
		return ServerDTO{}, err
	}
	return s.serverDTO(ctx, server)
}

// authorizeServer loads the server and returns ErrServerNotFound unless the
// principal is system-scoped, an owner (ServerOwners; unchanged, orthogonal
// to group membership), or a can_manage_servers owner/co-manager of one of
// the server's linked admin groups (serverManageGroupIDs) -- Phase B, spec
// 2026-08-10. This REPLACES the prior "any admin manages every server"
// global bypass: a plain admin (no "system" scope) who is neither an owner
// nor a can_manage_servers manager of a linked group now gets the SAME
// ErrServerNotFound (404-no-leak) as a non-member. This is THE single
// server choke-point -- every server sub-feature that calls authorizeServer
// (perf/availability/netbird/ping/energy/agent-tokens, plus applications and
// mappings via authorizeApplication/authorizeMapping) is re-scoped uniformly
// by this rewrite.
func (s *Service) authorizeServer(ctx context.Context, principal auth.Token, id string) (routing.AIServer, error) {
	server, err := s.routes.AIServerByID(ctx, id)
	if err != nil {
		return routing.AIServer{}, ErrServerNotFound
	}
	if isSystem(principal) {
		return server, nil
	}
	owners, err := s.routes.ServerOwners(ctx, id)
	if err != nil {
		return routing.AIServer{}, err
	}
	for _, ownerID := range owners {
		if ownerID == principal.UserID {
			return server, nil
		}
	}
	groupIDs, err := s.routes.ServerAdminGroups(ctx, id)
	if err != nil {
		return routing.AIServer{}, err
	}
	if len(groupIDs) > 0 {
		manageGroups, err := s.serverManageGroupIDs(ctx, principal)
		if err != nil {
			return routing.AIServer{}, err
		}
		for _, gid := range groupIDs {
			if manageGroups[gid] {
				return server, nil
			}
		}
	}
	return routing.AIServer{}, ErrServerNotFound
}

// serverPerfHistoryCap bounds how many telemetry samples a single history read
// returns; the store decimates the window down to this many evenly-spaced points.
const serverPerfHistoryCap = 2000

// ServerPerfHistory returns the persisted telemetry samples for a server over
// the window, owner/admin-gated via authorizeServer (no existence leak). The
// store decimates to serverPerfHistoryCap; the handler decimates no further.
func (s *Service) ServerPerfHistory(ctx context.Context, principal auth.Token, serverID string, window time.Duration) ([]routing.TelemetrySample, error) {
	if _, err := s.authorizeServer(ctx, principal, serverID); err != nil {
		return nil, err
	}
	if window <= 0 {
		window = 15 * time.Minute
	}
	to := s.clock().UTC()
	from := to.Add(-window)
	return s.routes.TelemetrySamples(ctx, serverID, from, to, serverPerfHistoryCap)
}

// serverAvailabilityHistoryCap bounds the reduced availability samples a single
// read returns (pathological flapping only; a steady 30d/5min server is ~2 rows).
const serverAvailabilityHistoryCap = 20000

// ServerAvailability returns a server's availability history over the window,
// owner/admin-gated via authorizeServer (no existence leak). The store applies a
// transition-preserving reduction, so the result is small.
func (s *Service) ServerAvailability(ctx context.Context, principal auth.Token, serverID string, window time.Duration) ([]routing.ServerAvailabilitySample, error) {
	if _, err := s.authorizeServer(ctx, principal, serverID); err != nil {
		return nil, err
	}
	if window <= 0 {
		window = time.Hour
	}
	to := s.clock().UTC()
	from := to.Add(-window)
	return s.routes.ServerAvailabilitySamples(ctx, serverID, from, to, serverAvailabilityHistoryCap)
}

// ServerHardware returns a server's latest static hardware inventory,
// owner/admin-gated via authorizeServer (no existence leak). found==false means
// no ServerAgent has reported hardware yet.
func (s *Service) ServerHardware(ctx context.Context, principal auth.Token, serverID string) (routing.ServerHardware, bool, error) {
	if _, err := s.authorizeServer(ctx, principal, serverID); err != nil {
		return routing.ServerHardware{}, false, err
	}
	return s.routes.ServerHardwareByServer(ctx, serverID)
}

func (s *Service) validateOwnerIDs(ctx context.Context, ids []string) ([]string, error) {
	out := make([]string, 0, len(ids))
	seen := map[string]struct{}{}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		if _, err := s.users.UserByID(ctx, id); err != nil {
			return nil, ErrServerOwnerInvalid
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// validateAdminGroupIDs validates the admin-group set a server is (or is
// being re-)linked into -- shared by CreateServer and SetServerAdminGroups
// (Phase B, spec 2026-08-10). rawIDs is trimmed + deduped first; an empty
// result is ErrServerAdminGroupRequired (every server needs >=1 admin group,
// regardless of the caller's scope). Each remaining id must resolve to an
// EXISTING ADMIN-tier group (else ErrServerAdminGroupInvalid); for a
// non-system principal, each must ALSO be one they may manage servers
// through (serverManageGroupIDs -- a system-scope principal skips this
// check and may link into ANY admin-tier group). Every chosen group must
// share exactly ONE ParentGroupID -- the server's containment root -- or the
// call is rejected as ErrServerAdminGroupParentMismatch; when the caller is
// system-scope and supplied a non-empty systemGroupHint (a convenience
// cross-check, CreateServerRequest.SystemGroupID), that resolved root must
// equal it too. Returns the deduped ids (order preserved) and the resolved
// systemGroupID.
func (s *Service) validateAdminGroupIDs(ctx context.Context, principal auth.Token, rawIDs []string, systemGroupHint string) ([]string, string, error) {
	return s.validateAdminGroupScope(ctx, principal, rawIDs, systemGroupHint, s.serverManageGroupIDs, adminGroupSentinels{
		Required:       ErrServerAdminGroupRequired,
		Invalid:        ErrServerAdminGroupInvalid,
		ParentMismatch: ErrServerAdminGroupParentMismatch,
	})
}

// adminGroupSentinels bundles the three domain-specific error values a
// validateAdminGroupScope caller must supply, since validateAdminGroupIDs,
// validateServiceAdminGroupIDs, and validateResourceGroupAdminGroupIDs each
// return their own sentinels for the same three failure cases (Required: no
// non-empty id survived trimming; Invalid: an id doesn't resolve to an
// existing admin-tier group the caller may manage into; ParentMismatch: the
// chosen groups don't share exactly one ParentGroupID, or contradict a
// system-scope caller's systemGroupHint).
type adminGroupSentinels struct {
	Required       error
	Invalid        error
	ParentMismatch error
}

// validateAdminGroupScope is the shared implementation behind
// validateAdminGroupIDs (server), validateServiceAdminGroupIDs, and
// validateResourceGroupAdminGroupIDs -- see their doc comments for the
// policy this implements. rawIDs is trimmed + deduped first; an empty
// result is errs.Required (every caller requires >=1 admin group,
// regardless of scope). Each remaining id must resolve to an EXISTING
// ADMIN-tier group (else errs.Invalid); for a non-system principal, each
// must ALSO be one manage(ctx, principal) reports as manageable (a
// system-scope principal skips this check and may link into ANY admin-tier
// group). Every chosen group must share exactly ONE ParentGroupID -- the
// containment root -- or the call is rejected as errs.ParentMismatch; when
// the caller is system-scope and supplied a non-empty systemGroupHint (a
// convenience cross-check), that resolved root must equal it too. Returns
// the deduped ids (order preserved) and the resolved systemGroupID.
func (s *Service) validateAdminGroupScope(ctx context.Context, principal auth.Token, rawIDs []string, systemGroupHint string, manage func(context.Context, auth.Token) (map[string]bool, error), errs adminGroupSentinels) ([]string, string, error) {
	ids := make([]string, 0, len(rawIDs))
	seen := map[string]struct{}{}
	for _, raw := range rawIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, "", errs.Required
	}
	var manageGroups map[string]bool
	if !isSystem(principal) {
		var err error
		manageGroups, err = manage(ctx, principal)
		if err != nil {
			return nil, "", err
		}
	}
	systemGroupID := ""
	for i, gid := range ids {
		g, err := s.groups.UserGroupByID(ctx, gid)
		if err != nil || g.Tier != store.GroupTierAdmin {
			return nil, "", errs.Invalid
		}
		if manageGroups != nil && !manageGroups[gid] {
			return nil, "", errs.Invalid
		}
		if i == 0 {
			systemGroupID = g.ParentGroupID
		} else if g.ParentGroupID != systemGroupID {
			return nil, "", errs.ParentMismatch
		}
	}
	if isSystem(principal) {
		if hint := strings.TrimSpace(systemGroupHint); hint != "" && hint != systemGroupID {
			return nil, "", errs.ParentMismatch
		}
	}
	return ids, systemGroupID, nil
}

func (s *Service) serverDTO(ctx context.Context, server routing.AIServer) (ServerDTO, error) {
	ownerIDs, err := s.routes.ServerOwners(ctx, server.ID)
	if err != nil {
		return ServerDTO{}, err
	}
	owners := make([]ServerOwnerDTO, 0, len(ownerIDs))
	for _, id := range ownerIDs {
		user, err := s.users.UserByID(ctx, id)
		if err != nil {
			// owner user vanished; skip rather than fail the whole DTO
			continue
		}
		owners = append(owners, ServerOwnerDTO{ID: user.ID, Email: user.Email, DisplayName: user.DisplayName})
	}
	groupIDs, err := s.routes.ServerAdminGroups(ctx, server.ID)
	if err != nil {
		return ServerDTO{}, err
	}
	adminGroups := make([]GroupRefDTO, 0, len(groupIDs))
	for _, gid := range groupIDs {
		g, err := s.groups.UserGroupByID(ctx, gid)
		if err != nil {
			// linked group vanished; skip rather than fail the whole DTO
			continue
		}
		adminGroups = append(adminGroups, GroupRefDTO{ID: g.ID, Name: g.Name})
	}
	systemGroupName := ""
	if server.SystemGroupID != "" {
		if g, err := s.groups.UserGroupByID(ctx, server.SystemGroupID); err == nil {
			systemGroupName = g.Name
		}
	}
	return ServerDTO{
		ID: server.ID, Name: server.Name, Domain: server.Domain, ServerPathSuffix: server.ServerPathSuffix, Status: server.Status,
		HealthStatus: server.HealthStatus, Owners: owners, LastSeenAt: server.LastSeenAt, CreatedAt: server.CreatedAt,
		NetbirdEnabled: server.NetbirdEnabled, NetbirdSetupKeyID: server.NetbirdSetupKeyID,
		NetbirdGroupID: server.NetbirdGroupID,
		NetbirdPeerID:  server.NetbirdPeerID, NetbirdConnected: server.NetbirdConnected,
		NetbirdGroupIDs:             decodeNetbirdGroupIDs(server.NetbirdGroupIDs),
		NetbirdPeerManaged:          server.NetbirdPeerManaged,
		NetbirdPolicyOverride:       server.NetbirdPolicyOverride,
		NetbirdAllowPing:            server.NetbirdAllowPing,
		NetbirdPingExclude:          server.NetbirdPingExclude,
		CertificateOverride:         server.CertificateOverride,
		HTTPSSwitchOverride:         server.HTTPSSwitchOverride,
		AgentStatus:                 s.agentStatus(ctx, server),
		AgentPresenceTimeoutSeconds: server.AgentPresenceTimeoutSeconds,
		EstimatedWatts:              server.EstimatedWatts,
		IdleWatts:                   server.IdleWatts,
		PricePerKwh:                 server.PricePerKwh,
		Pue:                         server.Pue,
		PriceUnit:                   NormalizePriceUnit(server.PriceUnit),
		AdminGroups:                 adminGroups,
		SystemGroupID:               server.SystemGroupID,
		SystemGroupName:             systemGroupName,
		// NetbirdSetupKey / NetbirdSetupCommand are display-once and deliberately
		// NOT set here — a Get/List DTO must never carry them.
	}, nil
}

// agentStatus derives the live 3-state agent_status for a server: "active"
// (the AgentPresenceReader says it reported within the effective per-server
// window), "inactive" (an agent token is configured but not reporting), or
// "unconfigured" (no agent token at all). Nil-safe throughout — a nil
// AgentPresence reader or an AgentTokenByServer error/miss never escalates
// past "unconfigured"/"inactive".
func (s *Service) agentStatus(ctx context.Context, server routing.AIServer) string {
	status := "unconfigured"
	if _, hasToken, _ := s.routes.AgentTokenByServer(ctx, server.ID); hasToken {
		status = "inactive"
	}
	if s.agentPresence != nil {
		sysDefault := s.activeAgentPresenceTimeoutSeconds(ctx)
		window := time.Duration(routing.EffectiveAgentPresenceTimeoutSeconds(server, sysDefault, MinAgentPresenceTimeoutSeconds, MaxAgentPresenceTimeoutSeconds)) * time.Second
		if s.agentPresence.ReportingWithin(server.ID, window) {
			status = "active"
		}
	}
	return status
}

// decodeNetbirdGroupIDs tolerantly decodes the opaque netbird_group_ids column
// (canonical JSON [{"id","name"}]) into DTO refs. It ALWAYS returns a non-nil
// slice — an empty/malformed value yields [] (never an error, never null), so a
// bad blob can never fail the whole server DTO.
func decodeNetbirdGroupIDs(raw string) []NetbirdGroupRefDTO {
	out := make([]NetbirdGroupRefDTO, 0)
	if strings.TrimSpace(raw) == "" {
		return out
	}
	var refs []NetbirdGroupRefDTO
	if err := json.Unmarshal([]byte(raw), &refs); err != nil {
		return out
	}
	out = append(out, refs...)
	return out
}

func normalizeServerStatus(raw string) (string, error) {
	status := strings.TrimSpace(raw)
	if status == "" {
		return routing.ServerStatusActive, nil
	}
	switch status {
	case routing.ServerStatusActive, routing.ServerStatusDisabled, routing.ServerStatusMaintenance:
		return status, nil
	default:
		return "", ErrServerStatusInvalid
	}
}

type mappingView struct {
	server  routing.AIServer
	app     routing.Application
	mapping routing.ModelMapping
}

// activeMappingViews returns every active mapping whose application is active
// and whose server is selectable (active + not unhealthy).
func (s *Service) activeMappingViews(ctx context.Context) ([]mappingView, error) {
	servers, err := s.routes.AIServers(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]mappingView, 0)
	for _, server := range servers {
		if server.Status != routing.ServerStatusActive || server.HealthStatus == routing.HealthUnhealthy {
			continue
		}
		apps, err := s.routes.ApplicationsByServer(ctx, server.ID)
		if err != nil {
			return nil, err
		}
		for _, app := range apps {
			if app.Status != routing.ServerStatusActive {
				continue
			}
			// Exclude applications whose reachability probe is currently
			// failing. This single gate makes Models(), ModelsForFlavor() (all
			// /v1/models flavors), dashboardRouteData(), and ManageModels() drop
			// the unreachable app's models together, since all read (directly or
			// via visibleMappingViews) activeMappingViews.
			if s.reachability != nil && !s.reachability.Reachable(app.ID) {
				continue
			}
			mappings, err := s.routes.MappingsByApplication(ctx, app.ID)
			if err != nil {
				return nil, err
			}
			for _, mapping := range mappings {
				if mapping.Status != routing.ServerStatusActive {
					continue
				}
				views = append(views, mappingView{server: server, app: app, mapping: mapping})
			}
		}
	}
	return views, nil
}

// visibleMappingViews returns activeMappingViews() filtered down to the
// mappings whose offering server the given principal is allowed to USE under
// resource-group provisioning (Resource Groups Phase 2 — Task 4). It is a
// thin wrapper: activeMappingViews itself stays token-less (it still feeds the
// ONE admin/internal caller that must NOT be principal-filtered —
// ManageModels(), the admin management surface — directly), and this wrapper
// is consulted by every OTHER, principal-reachable usage-facing surface
// (Models(), ModelsForFlavor(), dashboardRouteData() — the last one fixed in
// Fix round 1 below, since it leaked the exact model-name+host pair the other
// two surfaces hide, to any gateway:use token via GET /api/portal/dashboard)
// so a model/route offered exclusively by a server the principal may not use
// disappears from what it sees — BEFORE the model-group overlay + the
// shown/hidden/locked suppression run (for Models()/ModelsForFlavor()), so
// both cascade correctly (a group whose only viable member sits on a
// disallowed server loses that member too).
//
// One AllowedServerIDs call per invocation (deduped server ids across the
// whole view set) — no N+1, via the generic filterByAllowedServers (see
// service_visibility_filter.go; also used by filterAllowedModelServerRows and
// FilterAllowedGroupModelServerRows). A store error from the underlying
// activeMappingViews propagates unchanged; a store error from
// AllowedServerIDs itself propagates here too (failOpen=false) — it already
// fails safe per its own documented direction (opt-in leans allow,
// deny-by-default leans deny), so it is never treated as fatal by its
// callers either.
//
// VISIBILITY-SURFACE MATRIX — every model/server-facing surface reachable by
// a non-admin gateway:use principal, and which of the two independent
// filters it applies: (a) the resource-group AllowedServerIDs filter this
// function (or a sibling built on the same filterByAllowedServers core)
// implements, and (b) the model_settings hidden/locked suppression —
// modelGroupOverlay's suppress set for Models()/ModelsForFlavor() (applies to
// plain models AND groups alike), and the same underlying read
// (modelVisibilityByLower) applied directly, principal-gated on
// !isAdmin(...), by dashboardRouteData() and ModelServers():
//
//	Surface                                        (a) resource-group                          (b) hidden/locked
//	Models()                                       yes — visibleMappingViews                   yes — modelsResponse(suppress=true)
//	ModelsForFlavor() (/v1/models, per flavor)     yes — visibleMappingViews                   yes — modelFlavorSets (unconditional)
//	ModelServers() + its SSE variant                yes — filterAllowedModelServerRows          yes — principal-aware, admin bypass
//	  (handlePortalModelServers /
//	   handlePortalModelServersEvents)
//	model-group-servers                             yes — per-member ModelServers, PLUS         yes — inherited: every row comes
//	  (handlePortalModelGroupServers)                    FilterAllowedGroupModelServerRows           from a per-member ModelServers call
//	                                                     (defense-in-depth re-check)
//	dashboardRouteData()                            yes — visibleMappingViews                   yes — principal-aware, admin bypass
//	  (GET /api/portal/dashboard, "Live Model
//	   Routes" table)
//
// (ManageModels(), the admin-only management surface, applies NEITHER filter
// by design: an admin managing visibility/groups must see every active
// mapping regardless of resource-group provisioning or its own hidden/locked
// state — see ManageModels's own doc-comment.)
//
// CLOSED (security-policy — this row used to be a TODO here): ModelServers,
// its SSE variant, and model-group-servers used to apply ONLY the
// resource-group filter, never the hidden/locked suppression
// Models()/ModelsForFlavor() apply — a gateway:use principal who already knew
// (or guessed) a hidden or locked model's exact name could still list its
// serving rows via GET /api/portal/model-servers?name=<model>, even though
// that same model was dropped from the listing/picker they would otherwise
// discover it through. dashboardRouteData() had the identical gap. Both now
// apply the SAME suppression, gated on !isAdmin(principal): a non-admin gets
// an empty ModelServers slice (indistinguishable from an unknown model) and
// no dashboard route for a hidden/locked model name; an admin (the
// ModelServersSection management flow) is completely unaffected, exactly like
// ManageModels(). This is a portal-listing-surface fix only — the separate
// routing/resolver enforcement layer (hidden still resolves on a direct
// request, locked still ErrNoModelRoute) is untouched.
func (s *Service) visibleMappingViews(ctx context.Context, token auth.Token) ([]mappingView, error) {
	views, err := s.activeMappingViews(ctx)
	if err != nil {
		return nil, err
	}
	return filterByAllowedServers(ctx, s.AllowedServerIDs, token, views, func(v mappingView) string { return v.server.ID }, false)
}

// knownAPIFlavors is the sorted set of API flavors the gateway exposes.
var knownAPIFlavors = []string{routing.APIFlavorAnthropic, routing.APIFlavorOpenAI}

func isKnownAPIFlavor(flavor string) bool {
	for _, known := range knownAPIFlavors {
		if flavor == known {
			return true
		}
	}
	return false
}

// modelFlavorSets maps each active gateway model name to the set of known API
// flavors that expose it (union of app.APIFlavors across its active mapping
// views VISIBLE to the given principal — see visibleMappingViews). Drives
// per-flavor discovery (ModelsForFlavor(), i.e. /v1/models et al.); the portal
// Models() overview builds its own view via modelsResponse.
func (s *Service) modelFlavorSets(ctx context.Context, token auth.Token) (map[string]map[string]struct{}, error) {
	views, err := s.visibleMappingViews(ctx, token)
	if err != nil {
		return nil, err
	}
	sets := make(map[string]map[string]struct{})
	for _, view := range views {
		name := view.mapping.GatewayModelName
		if _, ok := sets[name]; !ok {
			sets[name] = make(map[string]struct{})
		}
		for _, flavor := range view.app.APIFlavors {
			if isKnownAPIFlavor(flavor) {
				sets[name][flavor] = struct{}{}
			}
		}
	}
	// Model-group offering overlay (spec §4a/§4b): add active groups (flavor
	// union) and drop hidden/locked models, so /v1/models + per-flavor discovery
	// include groups and hide non-shown models. This is ALWAYS the inference list,
	// so a hidden/locked GROUP is skipped here too (mirrors the suppressed path in
	// modelsResponse). Fails open on a store error.
	if entries, suppress, gErr := s.modelGroupOverlay(ctx, sets); gErr == nil {
		for name := range suppress {
			delete(sets, name)
		}
		for _, e := range entries {
			if e.Visibility != "shown" {
				continue
			}
			sets[e.Name] = e.Flavors
		}
	}
	return sets, nil
}

// modelVisibilityByLower returns every model_settings row's visibility keyed
// by the lowercased, trimmed gateway_model_name (case-insensitive lookup).
// This is the ONE place that reads model_settings for hidden/locked
// suppression purposes; modelGroupOverlay (Models()/ModelsForFlavor()'s
// suppress set), dashboardRouteData(), and ModelServers() all go through it —
// see the VISIBILITY-SURFACE MATRIX doc-comment above (on visibleMappingViews).
func (s *Service) modelVisibilityByLower(ctx context.Context) (map[string]string, error) {
	settings, err := s.routes.ModelSettings(ctx)
	if err != nil {
		return nil, err
	}
	visByLower := make(map[string]string, len(settings))
	for _, st := range settings {
		visByLower[strings.ToLower(strings.TrimSpace(st.GatewayModelName))] = st.Visibility
	}
	return visByLower, nil
}

// isHiddenOrLocked reports whether a model_settings visibility value is one
// of the two values a non-admin, usage-facing surface must suppress (a
// missing/empty value, e.g. no settings row, is "shown" and never matches).
func isHiddenOrLocked(visibility string) bool {
	switch visibility {
	case "hidden", "locked":
		return true
	default:
		return false
	}
}

// groupOverlayEntry is one active model group's contribution to the offered
// listing: the synthetic gateway model NAME to add, the UNION of its offerable
// members' flavor sets, its offerable members in priority order (the first is the
// highest-priority available member, used for group loaded-state), and the group
// NAME's own visibility from model_settings ("shown" | "hidden" | "locked";
// "shown" when no setting row exists). A hidden/locked group is dropped from the
// suppressed (inference/chat) listing but stays a full computed entry so the admin
// management path (suppress==false) still sees it.
type groupOverlayEntry struct {
	Name                    string
	Flavors                 map[string]struct{}
	OrderedOfferableMembers []string
	Visibility              string
	// LoadedOnly carries the group's loaded_only selection setting so the
	// group's reported loaded-state (see groupLoaded in modelsResponse) can
	// use the union of ALL offerable members' loaded state, not just the
	// highest-priority one -- because for a loaded_only group ANY of them is
	// what will actually be served.
	LoadedOnly bool
}

// modelGroupOverlay computes the model-group additions and the model-visibility
// suppression set to overlay onto the offered-model listing (spec §4a/§4b).
//
// perNameFlavors is the per-model flavor map already built from
// activeMappingViews; its keys are exactly the currently OFFERABLE gateway model
// names. It is read-only (never mutated here).
//
// Returns, for every ACTIVE group with at least one offerable member, an entry
// with the flavor UNION and the offerable members in priority order; plus
// suppress, the set of offerable names whose model_settings.visibility is
// "hidden" or "locked" (matched case-insensitively; the returned names are the
// actual keys as they appear in perNameFlavors). A member's own visibility does
// NOT affect a group's flavors/availability — a hidden/locked model is still a
// full member.
//
// On any store read error it returns (nil, nil, err) so callers can fail open
// (proceed WITHOUT groups/suppression); an offering glitch must never blank the
// model list.
func (s *Service) modelGroupOverlay(ctx context.Context, perNameFlavors map[string]map[string]struct{}) ([]groupOverlayEntry, map[string]struct{}, error) {
	groups, err := s.routes.ModelGroups(ctx)
	if err != nil {
		return nil, nil, err
	}
	// Per-model visibility keyed by lowercased name (case-insensitive lookup) —
	// the shared read modelVisibilityByLower also backs the other
	// principal-aware hidden/locked suppression sites (dashboardRouteData,
	// ModelServers; see the VISIBILITY-SURFACE MATRIX above, on
	// visibleMappingViews).
	visByLower, err := s.modelVisibilityByLower(ctx)
	if err != nil {
		return nil, nil, err
	}
	// suppress: offerable names whose visibility is hidden or locked. Keyed by
	// the actual name as it appears in perNameFlavors so callers can delete it.
	suppress := make(map[string]struct{})
	for name := range perNameFlavors {
		if isHiddenOrLocked(visByLower[strings.ToLower(strings.TrimSpace(name))]) {
			suppress[name] = struct{}{}
		}
	}
	// Build the flatten graph once from ALL groups (active flag preserved
	// per-group) plus each group's ordered members, keyed by lowercased
	// gateway_model_name — so a nested subgroup member is recognizable as a
	// group and flattens via ITS OWN traversal strategy (routing.FlattenGroup).
	graph := make(map[string]routing.FlatGroup, len(groups))
	for _, g := range groups {
		ms, err := s.routes.GroupMembersByGroup(ctx, g.ID)
		if err != nil {
			return nil, nil, err
		}
		graph[strings.ToLower(strings.TrimSpace(g.GatewayModelName))] = routing.FlatGroup{
			Traversal: g.Traversal,
			Members:   ms,
			Active:    g.Status == routing.ServerStatusActive,
		}
	}
	entries := make([]groupOverlayEntry, 0)
	for _, group := range groups {
		if group.Status != routing.ServerStatusActive {
			continue
		}
		// Defensive: a group NAME is globally unique against models, but never
		// double-add a name that already keys the model map.
		if _, exists := perNameFlavors[group.GatewayModelName]; exists {
			continue
		}
		// Flatten the group's (possibly nested) subgroups into the ordered,
		// de-duplicated leaf-MODEL member names reachable from it.
		flat := routing.FlattenGroup(group.GatewayModelName, graph)
		flavors := make(map[string]struct{})
		ordered := make([]string, 0, len(flat))
		for _, memberName := range flat {
			memberFlavors, ok := perNameFlavors[memberName]
			if !ok {
				continue // leaf member is not currently offerable
			}
			ordered = append(ordered, memberName)
			for f := range memberFlavors {
				flavors[f] = struct{}{}
			}
		}
		if len(ordered) == 0 {
			continue // no offerable member → the group is not offered
		}
		// The group NAME's own visibility (a group name lives in model_settings just
		// like a model). Default "shown" when no setting row exists.
		vis := "shown"
		if v := visByLower[strings.ToLower(strings.TrimSpace(group.GatewayModelName))]; v != "" {
			vis = v
		}
		entries = append(entries, groupOverlayEntry{
			Name:                    group.GatewayModelName,
			Flavors:                 flavors,
			OrderedOfferableMembers: ordered,
			Visibility:              vis,
			LoadedOnly:              group.LoadedOnly,
		})
	}
	return entries, suppress, nil
}

func sortedFlavorSet(set map[string]struct{}) []string {
	return sortedStringSet(set)
}

// sortedStringSet returns the keys of a string set as a sorted slice.
func sortedStringSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func allKnownFlavorsSorted() []string {
	return append([]string(nil), knownAPIFlavors...)
}

// seedModelNames are the fallback models when no routing store is configured.
var seedModelNames = []string{"gpt-oss-20b", "qwen-coder"}

// ModelsForFlavor returns the sorted gateway model names routable on the given
// API flavor (whose application declares that flavor), filtered to the models
// the given principal is allowed to see under resource-group provisioning
// (Resource Groups Phase 2). Falls back to the seed models (which expose every
// known flavor) when no routing store is configured.
func (s *Service) ModelsForFlavor(ctx context.Context, token auth.Token, flavor string) []string {
	if s.routes != nil {
		if sets, err := s.modelFlavorSets(ctx, token); err == nil {
			ids := make([]string, 0, len(sets))
			for id, set := range sets {
				if _, ok := set[flavor]; ok {
					ids = append(ids, id)
				}
			}
			sort.Strings(ids)
			return ids
		}
	}
	if isKnownAPIFlavor(flavor) {
		return append([]string(nil), seedModelNames...)
	}
	return []string{}
}

// AuthorizeRunAsToken verifies the given token id belongs to the principal, is
// active, and has the gateway:use scope, returning a run-as auth token.
func (s *Service) AuthorizeRunAsToken(ctx context.Context, principal auth.Token, tokenID string) (auth.Token, error) {
	tokenID = strings.TrimSpace(tokenID)
	if tokenID == "" {
		return auth.Token{}, ErrTokenRequired
	}
	record, err := s.tokens.TokenByID(ctx, tokenID)
	if err != nil {
		return auth.Token{}, ErrTokenForbidden
	}
	if record.UserID != principal.UserID || record.Status != store.TokenStatusActive {
		return auth.Token{}, ErrTokenForbidden
	}
	if record.ExpiresAt != nil && !record.ExpiresAt.After(s.clock().UTC()) {
		return auth.Token{}, ErrTokenForbidden
	}
	scopes := parseScopes(record.Scopes)
	runAs := auth.Token{
		ID: record.ID, UserID: record.UserID, Name: record.Name, Active: true, Scopes: scopes,
		ModelOverride:                  record.ModelOverride,
		ModelOverrideRules:             store.AuthModelOverrideRules(store.DecodeModelOverrideRules(record.ModelOverrideMap)),
		LogCommunication:               record.LogCommunication,
		Secret:                         record.Secret,
		ProjectID:                      record.ProjectID,
		ProjectName:                    s.resolveProjectName(ctx, record.ProjectID),
		ServerOverride:                 record.ServerOverride,
		ServerOverrideForceUnreachable: record.ServerOverrideForceUnreachable,
		LastUsedModel:                  record.LastUsedModel,
		UnknownModelRedirect:           record.UnknownModelRedirect,
		UnknownModelRedirectBlocked:    record.UnknownModelRedirectBlocked,
		UnknownModelFallback:           record.UnknownModelFallback,
	}
	if !hasGatewayUse(runAs) {
		return auth.Token{}, ErrTokenForbidden
	}
	return runAs, nil
}

// tokenDTO renders record, resolving its ProjectName (best-effort) via
// resolveProjectName — mirrors how ServiceName is resolved for a service
// token at bearer-lookup time.
func (s *Service) tokenDTO(ctx context.Context, record store.TokenRecord) TokenDTO {
	return TokenDTO{
		ID:                             record.ID,
		Name:                           record.Name,
		SecretPrefix:                   record.SecretPrefix,
		Status:                         record.Status,
		Scopes:                         parseScopes(record.Scopes),
		ExpiresAt:                      record.ExpiresAt,
		LastUsedAt:                     record.LastUsedAt,
		CreatedAt:                      record.CreatedAt,
		ModelOverride:                  record.ModelOverride,
		ModelOverrideMap:               modelOverrideRulesToMap(store.DecodeModelOverrideRules(record.ModelOverrideMap)),
		LogCommunication:               record.LogCommunication,
		Secret:                         record.Secret,
		IsChatSession:                  false,
		Deletable:                      true,
		ProjectID:                      record.ProjectID,
		ProjectName:                    s.resolveProjectName(ctx, record.ProjectID),
		ServerOverride:                 record.ServerOverride,
		ServerOverrideForceUnreachable: record.ServerOverrideForceUnreachable,
	}
}

// resolveProjectName returns projectID's display name, or "" when projectID
// is empty, the project no longer exists, or no ProjectStore is wired
// (best-effort — a stale/unresolvable project id never fails the caller, it
// just yields an empty display name; mirrors the ServiceName resolution at
// bearer-lookup time in SQLiteStore.LookupBearer).
func (s *Service) resolveProjectName(ctx context.Context, projectID string) string {
	if projectID == "" || s.projects == nil {
		return ""
	}
	p, err := s.projects.ProjectByID(ctx, projectID)
	if err != nil {
		return ""
	}
	return p.Name
}

// assignTokenProject resolves and membership-checks a token's desired project
// attribution (spec §4/§6, Task 5). An empty projectID always clears the
// attribution (no check needed — a token owner may always detach). A
// non-empty projectID must both EXIST (else ErrProjectNotFound) and have
// ownerUserID as a member of it per §4 — owner, direct project_members row,
// or a group assigned to the project (else ErrProjectNotMember) — enforced
// here so the frontend picker is advisory, not the security boundary; a raw
// API call cannot attribute a token to a project its owner cannot see.
func (s *Service) assignTokenProject(ctx context.Context, ownerUserID, projectID string) (string, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return "", nil
	}
	if _, err := s.projects.ProjectByID(ctx, projectID); err != nil {
		return "", ErrProjectNotFound
	}
	member, err := s.isProjectMember(ctx, ownerUserID, projectID)
	if err != nil {
		return "", err
	}
	if !member {
		return "", ErrProjectNotMember
	}
	return projectID, nil
}

func sortTokenRecords(records []store.TokenRecord) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].ID > records[j].ID
		}
		return records[i].CreatedAt.After(records[j].CreatedAt)
	})
}

func parseScopes(raw string) []string {
	var scopes []string
	if err := json.Unmarshal([]byte(raw), &scopes); err != nil {
		return []string{}
	}
	return scopes
}

func percentile95(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	index := (len(values)*95 + 99) / 100
	if index < 1 {
		index = 1
	}
	return values[index-1]
}

func generateSecret() (string, error) {
	value, err := compactRandomHexWithError(32)
	if err != nil {
		return "", err
	}
	return "opaigw_" + value, nil
}

func compactRandomHex(length int) string {
	value, err := compactRandomHexWithError(length)
	if err != nil {
		return "fallback"
	}
	return value
}

func compactRandomHexWithError(length int) (string, error) {
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func secretPrefix(secret string) string {
	if len(secret) <= 8 {
		return secret
	}
	return secret[:8]
}
