// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/netbird"
	"op-ai-gateway/internal/theme"
	"reflect"
	"strconv"
	"strings"
	"time"
)

var (
	ErrThemeInvalid                    = errors.New("system.theme_invalid")
	ErrLanguageInvalid                 = errors.New("system.language_invalid")
	ErrRetentionInvalid                = errors.New("system.retention_invalid")
	ErrHealthCheckIntervalInvalid      = errors.New("system.health_check_interval_invalid")
	ErrAgentPresenceTimeoutInvalid     = errors.New("system.agent_presence_timeout_invalid")
	ErrTotpModeInvalid                 = errors.New("system.totp_mode_invalid")
	ErrVisionProbeModeInvalid          = errors.New("system.vision_probe_mode_invalid")
	ErrRouteAffinitySessionModeInvalid = errors.New("system.route_affinity_session_mode_invalid")
)

// ErrEnergyDefaultInvalid rejects a negative value for any of the three
// system-wide energy-attribution defaults (price_per_kwh/pue/wh_per_token —
// 0 = unset / "no default").
var ErrEnergyDefaultInvalid = errors.New("system.energy_default_invalid")

var (
	ErrSMTPConfigIncomplete = errors.New("system.smtp_config_incomplete")
	ErrSMTPFromInvalid      = errors.New("system.smtp_from_invalid")
	ErrSMTPPortInvalid      = errors.New("system.smtp_port_invalid")
	ErrSMTPTLSModeInvalid   = errors.New("system.smtp_tls_mode_invalid")
	ErrSMTPKeyRequired      = errors.New("system.smtp_key_required")
)

// ErrCertKeyRequired is sealCertSecret/openCertSecret's refusal: a certificate
// private key must be sealed with the CERTIFICATE encryption key
// (OP_AI_GATEWAY_CERT_ENCRYPTION_KEY) and a disk-backed store has none
// configured. It is the certificate analogue of ErrSMTPKeyRequired and
// deliberately a DISTINCT sentinel: the two secrets are sealed with two
// different keys, so telling an operator to set the capture key would be wrong
// advice. It never carries key material.
var ErrCertKeyRequired = errors.New("system.cert_key_required")

var (
	ErrNetbirdConfigIncomplete         = errors.New("system.netbird_config_incomplete")
	ErrNetbirdURLInvalid               = errors.New("system.netbird_url_invalid")
	ErrNetbirdKeyRequired              = errors.New("system.netbird_key_required")
	ErrNetbirdIntervalOrder            = errors.New("system.netbird_interval_order")
	ErrNetbirdTokenRotateBeforeInvalid = errors.New("system.netbird_token_rotate_before_invalid")
	// ErrNetbirdNetworkRangeInvalid is returned by SetNetbirdNetwork when a
	// non-empty network_range / network_range_v6 fails to parse as a CIDR of the
	// expected IP family.
	ErrNetbirdNetworkRangeInvalid = errors.New("system.netbird_network_range_invalid")
)

const (
	defaultTheme    = "default"
	defaultLanguage = "de"
)

const (
	captureRetentionKey            = "capture_retention_days"
	captureEnabledKey              = "capture_enabled"
	captureOverrideKey             = "capture_override"
	healthCheckIntervalKey         = "health_check_interval_seconds"
	agentPresenceTimeoutKey        = "agent_presence_timeout_seconds"
	totpModeKey                    = "totp_mode"
	visionProbeModeKey             = "vision_probe_mode"
	routeAffinitySessionModeKey    = "route_affinity_session_mode"
	resourceProvisioningEnforceKey = "resource_provisioning_enforce"
)

const (
	energyDefaultPricePerKwhKey = "energy_default_price_per_kwh"
	energyDefaultPueKey         = "energy_default_pue"
	energyDefaultWhPerTokenKey  = "energy_default_wh_per_token"
	// energyDefaultPriceUnitKey is the display/entry unit for
	// energy_default_price_per_kwh (one of the shared currency.go units;
	// default eur_cent).
	energyDefaultPriceUnitKey = "energy_default_price_unit"
)

// currencyUsdPerEurKey is the system-wide EUR->USD conversion factor (USD
// per 1 EUR); 0 = unset. Internal cost storage stays canonical EUR — this
// factor is used only to derive a USD display value.
const currencyUsdPerEurKey = "currency_usd_per_eur"

// DefaultTOTPMode is the totp_mode used when the setting is unset, blank, or
// not one of the known modes.
const DefaultTOTPMode = "off"

// DefaultVisionProbeMode is used when the setting is unset, blank, or unknown.
const DefaultVisionProbeMode = "accept"

// DefaultRouteAffinitySessionMode is the route_affinity_session_mode used when
// the setting is unset, blank, or not one of the known modes. "client_session"
// = key affinity on the extracted client session id (the new default);
// "legacy_header" = key on the explicit affinity header (the prior behavior).
const DefaultRouteAffinitySessionMode = "client_session"

const (
	smtpEnabledKey  = "smtp_enabled"
	smtpHostKey     = "smtp_host"
	smtpPortKey     = "smtp_port"
	smtpUsernameKey = "smtp_username"
	smtpPasswordKey = "smtp_password"
	smtpFromKey     = "smtp_from"
	smtpFromNameKey = "smtp_from_name"
	smtpTLSModeKey  = "smtp_tls_mode"
)

const (
	netbirdEnabledKey           = "netbird_enabled"
	netbirdURLKey               = "netbird_url"
	netbirdGroupKey             = "netbird_group"
	netbirdTokenKey             = "netbird_token"
	netbirdOnlyKey              = "netbird_only"
	netbirdAgentDownloadOnlyKey = "netbird_agent_download_only"
	netbirdGatewayPeerIDKey     = "netbird_gateway_peer_id"

	netbirdGatewayPeerNameKey = "netbird_gateway_peer_name"

	netbirdManagePoliciesKey       = "netbird_manage_policies"
	netbirdPolicyScopeKey          = "netbird_policy_scope"
	netbirdDenyByDefaultKey        = "netbird_deny_by_default"
	netbirdDenyByDefaultEnforceKey = "netbird_deny_by_default_enforce"
	netbirdAllowPingGatewayKey     = "netbird_allow_ping_gateway"
	netbirdAllowPingAllServersKey  = "netbird_allow_ping_all_servers"
	netbirdPeerSyncIntervalKey     = "netbird_peer_sync_interval_seconds"
	netbirdReconcileIntervalKey    = "netbird_reconcile_interval_seconds"

	// netbirdTokenIDKey/netbirdTokenExpiresAtKey are runtime-managed (written by
	// the rotation/resolve-on-save flow, not directly editable via the settings
	// form) mirrors of the admin API token's current id + expiry, used to
	// display validity and drive auto-rotation.
	netbirdTokenIDKey        = "netbird_token_id"
	netbirdTokenExpiresAtKey = "netbird_token_expires_at"
	// netbirdTokenRotateBeforeKey is the operator-settable auto-rotation
	// threshold (days before expiry).
	netbirdTokenRotateBeforeKey = "netbird_token_rotate_before_days"
)

const (
	systemAdminModeRequirePasswordKey = "system_admin_mode_require_password"
)

// DefaultNetbirdTokenRotateBeforeDays is the fallback threshold (days before
// expiry) for auto-rotation when neither the KV nor the env override is set.
const DefaultNetbirdTokenRotateBeforeDays = 14

// DefaultNetbirdPolicyScope/DefaultNetbirdPeerSyncIntervalSeconds/
// DefaultNetbirdReconcileIntervalSeconds are the netbird policy-management
// defaults used when the corresponding setting is unset, blank, or invalid.
// MinNetbirdIntervalSeconds is the floor for both interval settings, but the
// two sides treat a violation differently: on WRITE (UpdateSystemSettings) a
// value below the floor is REJECTED outright (ErrNetbirdIntervalOrder, HTTP
// 400) rather than silently clamped; only the READ-side getters
// (NetbirdPeerSyncIntervalSeconds/NetbirdReconcileIntervalSeconds) floor a
// stored value by falling back to the documented default.
const (
	DefaultNetbirdPolicyScope              = "auto"
	DefaultNetbirdPeerSyncIntervalSeconds  = 30
	DefaultNetbirdReconcileIntervalSeconds = 60
	MinNetbirdIntervalSeconds              = 10
)

// DefaultSMTPPort is the submission port used when smtp_port is unset or out of
// range. DefaultSMTPTLSMode is the TLS mode used when smtp_tls_mode is unset or
// not one of the known modes.
const (
	DefaultSMTPPort    = 587
	DefaultSMTPTLSMode = "starttls"
)

// DefaultHealthCheckIntervalSeconds is the probe cadence used when the setting
// is unset or invalid. min/max bound writes and clamp reads.
const (
	DefaultHealthCheckIntervalSeconds = 30
	MinHealthCheckIntervalSeconds     = 5
	MaxHealthCheckIntervalSeconds     = 3600
)

// DefaultAgentPresenceTimeoutSeconds is the system-wide "the agent is
// delivering values" window used when the setting is unset or invalid AND no
// env-derived ServiceDeps default was provided (see
// Service.AgentPresenceTimeoutSeconds). min/max bound writes and clamp reads.
const (
	DefaultAgentPresenceTimeoutSeconds = 15
	MinAgentPresenceTimeoutSeconds     = 3
	MaxAgentPresenceTimeoutSeconds     = 3600
)

// DefaultCaptureRetentionDays is the retention window used when the setting is
// unset or invalid. minCaptureRetentionDays/maxCaptureRetentionDays bound writes.
const (
	DefaultCaptureRetentionDays = 30
	minCaptureRetentionDays     = 1
	maxCaptureRetentionDays     = 365
)

// CaptureRetentionDays interprets the persisted capture_retention_days value,
// clamping to [1,365] and defaulting to 30 when absent, blank, or unparseable.
// The background prune job and SystemSettingsView both read retention through here.
func CaptureRetentionDays(values map[string]string) int {
	raw, ok := values[captureRetentionKey]
	if !ok {
		return DefaultCaptureRetentionDays
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < minCaptureRetentionDays || n > maxCaptureRetentionDays {
		return DefaultCaptureRetentionDays
	}
	return n
}

// CaptureEnabled interprets the persisted capture_enabled system setting: a
// global kill switch for NEW payload captures (existing captures stay
// viewable/deletable regardless — see docs/superpowers/specs). Defaults to
// true (opt-out) when absent, blank, or unparseable; only an explicit
// "false" turns capturing off. The gateway's ServerDeps.CaptureEnabled hook
// and SystemSettingsView both read the setting through here.
func CaptureEnabled(values map[string]string) bool {
	raw, ok := values[captureEnabledKey]
	if !ok {
		return true
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return true
	}
	return enabled
}

// CaptureOverride interprets the persisted capture_override system setting:
// when true, capture runs for every request even if the token did NOT set
// log_communication (capture_enabled stays the master kill switch). Defaults
// to false (opt-in preserved) when absent, blank, or unparseable.
func CaptureOverride(values map[string]string) bool {
	raw, ok := values[captureOverrideKey]
	if !ok {
		return false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return v
}

// HealthCheckIntervalSeconds interprets the persisted
// health_check_interval_seconds value, defaulting to 30 when absent, blank,
// unparseable, or out of the [5,3600] range. The app-health probe loop reads
// the interval through here each cycle (fail-open 30) and SystemSettingsView
// surfaces it.
func HealthCheckIntervalSeconds(values map[string]string) int {
	raw, ok := values[healthCheckIntervalKey]
	if !ok {
		return DefaultHealthCheckIntervalSeconds
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < MinHealthCheckIntervalSeconds || n > MaxHealthCheckIntervalSeconds {
		return DefaultHealthCheckIntervalSeconds
	}
	return n
}

// AgentPresenceTimeoutSeconds is the effective system-wide default window
// (seconds) for "the agent is delivering values": the KV value when present
// and within [MinAgentPresenceTimeoutSeconds,MaxAgentPresenceTimeoutSeconds],
// else the env-derived default (s.agentPresenceTimeoutDefault, itself
// defaulting to DefaultAgentPresenceTimeoutSeconds — see NewService). Mirrors
// the fallback shape of NetbirdTokenRotateBeforeDays, unlike the package-level
// HealthCheckIntervalSeconds(values) function, because the fallback here is
// per-process (env-configurable), not a fixed constant.
func (s *Service) AgentPresenceTimeoutSeconds(values map[string]string) int {
	raw, ok := values[agentPresenceTimeoutKey]
	if ok {
		if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && n >= MinAgentPresenceTimeoutSeconds && n <= MaxAgentPresenceTimeoutSeconds {
			return n
		}
	}
	return s.agentPresenceTimeoutDefault
}

// activeAgentPresenceTimeoutSeconds reads settings + applies the reader
// above (nil-safe: a missing store or read error falls back to the
// env-derived default, never the hardcoded constant, so an operator's
// OP_AI_GATEWAY_AGENT_PRESENCE_TIMEOUT_SECONDS override still applies).
func (s *Service) activeAgentPresenceTimeoutSeconds(ctx context.Context) int {
	if s.settings == nil {
		return s.agentPresenceTimeoutDefault
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return s.agentPresenceTimeoutDefault
	}
	return s.AgentPresenceTimeoutSeconds(values)
}

// ActiveAgentPresenceTimeoutSeconds is the ctx-carrying public accessor for the
// effective system-wide agent-presence window, exposed on the portal.API
// interface for the /api/portal/agent-presence-timeout endpoint (mirrors
// HealthCheckIntervalSeconds(ctx)). It is named distinctly from the
// values-map method AgentPresenceTimeoutSeconds(values) above — unlike
// HealthCheckIntervalSeconds, which pairs a package-level function with a
// same-named method (legal: different namespaces), AgentPresenceTimeoutSeconds
// here is already a *Service METHOD, so a second method of that exact name
// would collide; ActiveAgentPresenceTimeoutSeconds avoids that.
func (s *Service) ActiveAgentPresenceTimeoutSeconds(ctx context.Context) int {
	return s.activeAgentPresenceTimeoutSeconds(ctx)
}

// SMTPEnabled interprets the smtp_enabled on/off switch. Defaults to false when
// absent, blank, or unparseable.
func SMTPEnabled(values map[string]string) bool {
	raw, ok := values[smtpEnabledKey]
	if !ok {
		return false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return v
}

// SMTPPort interprets smtp_port, defaulting to 587 when absent, blank,
// unparseable, or outside [1,65535].
func SMTPPort(values map[string]string) int {
	raw, ok := values[smtpPortKey]
	if !ok {
		return DefaultSMTPPort
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 || n > 65535 {
		return DefaultSMTPPort
	}
	return n
}

// knownSMTPTLSModes is the registry of valid smtp_tls_mode values.
func knownSMTPTLSModes() []string { return []string{"starttls", "ssl", "none"} }

func isKnownSMTPTLSMode(mode string) bool {
	for _, m := range knownSMTPTLSModes() {
		if m == mode {
			return true
		}
	}
	return false
}

// SMTPTLSMode interprets smtp_tls_mode, defaulting to "starttls" when absent,
// blank, or not one of the known modes.
func SMTPTLSMode(values map[string]string) string {
	raw := strings.TrimSpace(values[smtpTLSModeKey])
	if isKnownSMTPTLSMode(raw) {
		return raw
	}
	return DefaultSMTPTLSMode
}

// NetbirdEnabled interprets the netbird_enabled on/off switch. Defaults to false
// when absent, blank, or unparseable.
func NetbirdEnabled(values map[string]string) bool {
	raw, ok := values[netbirdEnabledKey]
	if !ok {
		return false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return v
}

// NetbirdURL returns the configured NetBird admin-API base URL, trimmed of
// surrounding whitespace and any trailing slash.
func NetbirdURL(values map[string]string) string {
	return strings.TrimRight(strings.TrimSpace(values[netbirdURLKey]), "/")
}

// NetbirdOnly interprets the netbird_only runtime toggle: when true the gateway
// restricts the agent↔gateway and gateway→app planes to the NetBird overlay
// (inbound reject on the public listener + outbound off-mesh refusal — consumed
// in later tasks). Defaults to false when absent, blank, or unparseable.
func NetbirdOnly(values map[string]string) bool {
	raw, ok := values[netbirdOnlyKey]
	if !ok {
		return false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return v
}

// ResourceProvisioningEnforce interprets the resource_provisioning_enforce
// runtime toggle (Resource Groups Phase 2): when false (the default, opt-in
// mode) a server with NO provisioned resource group stays unrestricted
// (today's behavior); when true (deny mode) every server that is a member of
// ANY provisioned resource group requires the caller to be provisioned into
// one of its resource groups — deny-by-default. Consumed by
// Service.AllowedServerIDs. Defaults to false when absent, blank, or
// unparseable.
func ResourceProvisioningEnforce(values map[string]string) bool {
	raw, ok := values[resourceProvisioningEnforceKey]
	if !ok {
		return false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return v
}

// SystemAdminModeRequirePassword interprets the system_admin_mode_require_password
// toggle: when true (the default) entering System-Admin mode requires the
// account password; when explicitly "false" it is a one-click toggle. Defaults
// to true when absent, blank, or unparseable (fail safe — never silently drop
// the step-up password requirement).
func SystemAdminModeRequirePassword(values map[string]string) bool {
	raw, ok := values[systemAdminModeRequirePasswordKey]
	if !ok {
		return true
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return true
	}
	return v
}

// NetbirdAgentDownloadOnly reports whether the agent-token binary download endpoint
// is restricted to the NetBird mesh. Default false.
func NetbirdAgentDownloadOnly(values map[string]string) bool {
	raw, ok := values[netbirdAgentDownloadOnlyKey]
	if !ok {
		return false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return v
}

// NetbirdGatewayPeerID returns the NetBird peer id the operator selected as the
// gateway's own peer (used at startup to resolve the agent-listener bind IP),
// trimmed of surrounding whitespace. Empty when absent.
func NetbirdGatewayPeerID(values map[string]string) string {
	return strings.TrimSpace(values[netbirdGatewayPeerIDKey])
}

// NetbirdGatewayPeerName returns the desired name for the gateway's own NetBird
// peer (applied by the reconcile loop via UpdatePeerName), trimmed. Empty ⇒ no
// rename (the peer keeps its NB_HOSTNAME default).
func NetbirdGatewayPeerName(values map[string]string) string {
	return strings.TrimSpace(values[netbirdGatewayPeerNameKey])
}

// NetbirdManagePolicies interprets the netbird_manage_policies on/off switch:
// when true, the gateway takes over least-privilege NetBird policy management
// (consumed by later tasks). Defaults to false when absent, blank, or unparseable.
func NetbirdManagePolicies(values map[string]string) bool {
	raw, ok := values[netbirdManagePoliciesKey]
	if !ok {
		return false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return v
}

// NetbirdDenyByDefault interprets the netbird_deny_by_default switch: when true,
// gateway-managed policies default new peers to deny access unless explicitly
// selected. Defaults to false when absent, blank, or unparseable.
func NetbirdDenyByDefault(values map[string]string) bool {
	raw, ok := values[netbirdDenyByDefaultKey]
	if !ok {
		return false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return v
}

// NetbirdDenyByDefaultEnforce interprets the netbird_deny_by_default_enforce
// switch: when true, deny-by-default is actively enforced (not merely the
// default posture for new peers). Defaults to false when absent, blank, or
// unparseable.
func NetbirdDenyByDefaultEnforce(values map[string]string) bool {
	raw, ok := values[netbirdDenyByDefaultEnforceKey]
	if !ok {
		return false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return v
}

// NetbirdAllowPingGateway interprets the netbird_allow_ping_gateway switch: when
// true, the gateway is permitted to ICMP-ping managed AI-server peers. Defaults
// to false when absent, blank, or unparseable.
func NetbirdAllowPingGateway(values map[string]string) bool {
	raw, ok := values[netbirdAllowPingGatewayKey]
	if !ok {
		return false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return v
}

// NetbirdAllowPingAllServers interprets the netbird_allow_ping_all_servers
// switch: when true, every managed AI-server peer is ping-allowed regardless of
// its per-server flag. Defaults to false when absent, blank, or unparseable.
func NetbirdAllowPingAllServers(values map[string]string) bool {
	raw, ok := values[netbirdAllowPingAllServersKey]
	if !ok {
		return false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return v
}

// knownNetbirdPolicyScopes is the registry of valid netbird_policy_scope values.
func knownNetbirdPolicyScopes() []string { return []string{"auto", "all", "selected"} }

// NetbirdPolicyScope interprets the persisted netbird_policy_scope system
// setting, defaulting to "auto" when absent, blank, or not one of the known
// scopes.
//
// Deliberately lenient: an unknown/blank value resolves to auto rather than
// erroring — the scope is UI-dropdown-driven (the only producer is the
// System Settings select, never free text) and auto is the safe default, so
// there is no strict-validation sentinel for this field (unlike, say,
// netbird_tls_mode). Do not add one without a corresponding spec decision.
func NetbirdPolicyScope(values map[string]string) string {
	raw := strings.TrimSpace(values[netbirdPolicyScopeKey])
	for _, s := range knownNetbirdPolicyScopes() {
		if raw == s {
			return raw
		}
	}
	return DefaultNetbirdPolicyScope
}

// EffectiveNetbirdPolicyScope resolves the "auto" scope against deny-by-default:
// auto+deny -> "all" (every peer needs an explicit allow), auto+!deny ->
// "selected" (only opted-in peers are managed). An explicit "all"/"selected"
// scope always wins over deny-by-default.
func EffectiveNetbirdPolicyScope(scope string, denyByDefault bool) string {
	switch scope {
	case "all", "selected":
		return scope
	default: // "auto"
		if denyByDefault {
			return "all"
		}
		return "selected"
	}
}

// netbirdIntervalOrDefault parses raw as an interval in seconds, falling back
// to def when raw is blank, unparseable, or below MinNetbirdIntervalSeconds.
func netbirdIntervalOrDefault(raw string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < MinNetbirdIntervalSeconds {
		return def
	}
	return n
}

// NetbirdPeerSyncIntervalSeconds interprets the persisted
// netbird_peer_sync_interval_seconds value, defaulting to 30 when absent,
// blank, unparseable, or below the 10s floor.
func NetbirdPeerSyncIntervalSeconds(values map[string]string) int {
	return netbirdIntervalOrDefault(values[netbirdPeerSyncIntervalKey], DefaultNetbirdPeerSyncIntervalSeconds)
}

// NetbirdReconcileIntervalSeconds interprets the persisted
// netbird_reconcile_interval_seconds value, defaulting to 60 when absent,
// blank, unparseable, or below the 10s floor.
func NetbirdReconcileIntervalSeconds(values map[string]string) int {
	return netbirdIntervalOrDefault(values[netbirdReconcileIntervalKey], DefaultNetbirdReconcileIntervalSeconds)
}

// NetbirdGroups returns the configured module-level NetBird group names. The
// netbird_group KV holds a JSON array of names; the returned list is trimmed,
// with empties dropped and duplicates removed (order preserved). Back-compat: a
// non-JSON non-empty legacy value (a single group name) yields a one-element list;
// an empty/absent value (or an effectively-empty list) yields nil.
func NetbirdGroups(values map[string]string) []string {
	raw := strings.TrimSpace(values[netbirdGroupKey])
	if raw == "" {
		return nil
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err == nil {
		return dedupeNames(list)
	}
	// Legacy single value (not a JSON array).
	return []string{raw}
}

// dedupeNames trims, drops empties, and removes duplicates (preserving first-seen
// order). An effectively-empty result is nil.
func dedupeNames(names []string) []string {
	out := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// encodeNetbirdGroups marshals a (trimmed, non-empty, deduped) group-name list to
// the JSON array stored in the netbird_group KV. An empty effective list encodes
// to "[]".
func encodeNetbirdGroups(names []string) (string, error) {
	out := dedupeNames(names)
	if out == nil {
		out = []string{}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// knownTOTPModes is the registry of valid totp_mode values.
func knownTOTPModes() []string { return []string{"off", "optional", "required"} }

func isKnownTOTPMode(mode string) bool {
	for _, m := range knownTOTPModes() {
		if m == mode {
			return true
		}
	}
	return false
}

// TOTPMode interprets the persisted totp_mode system setting, defaulting to
// "off" when absent, blank, or not one of the known modes. The gateway reads
// the effective mode ONLY via Service.TOTPMode (which calls this).
func TOTPMode(values map[string]string) string {
	raw := strings.TrimSpace(values[totpModeKey])
	if isKnownTOTPMode(raw) {
		return raw
	}
	return DefaultTOTPMode
}

// knownRouteAffinitySessionModes is the registry of valid
// route_affinity_session_mode values.
func knownRouteAffinitySessionModes() []string {
	return []string{"client_session", "legacy_header"}
}

func isKnownRouteAffinitySessionMode(mode string) bool {
	for _, m := range knownRouteAffinitySessionModes() {
		if m == mode {
			return true
		}
	}
	return false
}

// RouteAffinitySessionMode interprets the persisted route_affinity_session_mode
// system setting, defaulting to "client_session" when absent, blank, or not one
// of the known modes.
func RouteAffinitySessionMode(values map[string]string) string {
	raw := strings.TrimSpace(values[routeAffinitySessionModeKey])
	if isKnownRouteAffinitySessionMode(raw) {
		return raw
	}
	return DefaultRouteAffinitySessionMode
}

// knownVisionProbeModes is the registry of valid vision_probe_mode values.
func knownVisionProbeModes() []string { return []string{"accept", "verify"} }

func isKnownVisionProbeMode(mode string) bool {
	for _, m := range knownVisionProbeModes() {
		if m == mode {
			return true
		}
	}
	return false
}

// VisionProbeMode interprets the persisted vision_probe_mode setting, defaulting
// to DefaultVisionProbeMode when unset/blank/unknown (lenient read).
func VisionProbeMode(values map[string]string) string {
	v := strings.TrimSpace(values[visionProbeModeKey])
	if !isKnownVisionProbeMode(v) {
		return DefaultVisionProbeMode
	}
	return v
}

// energyDefaultFloat is the shared lenient reader for the three energy-default
// settings below: an absent/blank/unparseable stored value reads back 0
// ("no default" / "unknown" — a later phase falls back to a hardcoded
// constant when this reads 0). Negative values are rejected only at WRITE
// time (UpdateSystemSettings); a stored negative (should not occur via the
// API) is treated as 0 here, defensively.
func energyDefaultFloat(values map[string]string, key string) float64 {
	raw := strings.TrimSpace(values[key])
	if raw == "" {
		return 0
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

// EnergyDefaultPricePerKwh interprets the persisted
// energy_default_price_per_kwh setting (currency units per kWh); 0 = unset.
func EnergyDefaultPricePerKwh(values map[string]string) float64 {
	return energyDefaultFloat(values, energyDefaultPricePerKwhKey)
}

// EnergyDefaultPue interprets the persisted energy_default_pue setting (data
// center Power Usage Effectiveness multiplier); 0 = unset.
func EnergyDefaultPue(values map[string]string) float64 {
	return energyDefaultFloat(values, energyDefaultPueKey)
}

// EnergyDefaultWhPerToken interprets the persisted
// energy_default_wh_per_token setting (fallback watt-hours per generated
// token when no per-mapping coefficient is known); 0 = unset.
func EnergyDefaultWhPerToken(values map[string]string) float64 {
	return energyDefaultFloat(values, energyDefaultWhPerTokenKey)
}

// EnergyDefaultPriceUnit interprets the persisted energy_default_price_unit
// setting (the display/entry unit for energy_default_price_per_kwh); an
// unknown or blank value normalizes to eur_cent via NormalizePriceUnit.
func EnergyDefaultPriceUnit(values map[string]string) string {
	return NormalizePriceUnit(values[energyDefaultPriceUnitKey])
}

// CurrencyUsdPerEur interprets the persisted currency_usd_per_eur setting
// (USD per 1 EUR conversion factor); 0 = unset. Reuses the same lenient
// energyDefaultFloat reader as the energy defaults above (a blank,
// unparseable, or negative stored value all read back as 0 — negative is
// rejected only at write time, in UpdateSystemSettings).
func CurrencyUsdPerEur(values map[string]string) float64 {
	return energyDefaultFloat(values, currencyUsdPerEurKey)
}

// validateEnergyDefault rejects a negative value for any of the three
// energy-default settings (0 = unset is allowed).
func validateEnergyDefault(v float64) error {
	if v < 0 {
		return ErrEnergyDefaultInvalid
	}
	return nil
}

// ThemeOption is a selectable theme surfaced to the frontend theme picker:
// either a built-in (see builtinThemes) or an externally supplied theme from
// the loaded theme registry (see theme.Registry.Options).
type ThemeOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// builtinThemes is the fixed set of Go-side built-in themes. MIRROR of the
// frontend's gateway/frontend/src/theme/tokens.ts theme names -- "skynet" is
// the Terminator-inspired theme (renames the product to "Skynet AI
// Gateway"). "cgi" is intentionally NOT listed here: it moved to being an
// external, deployable theme only (see internal/theme), never a built-in.
var builtinThemes = []ThemeOption{
	{ID: "default", Name: "Default"},
	{ID: "matrix", Name: "Matrix"},
	{ID: "skynet", Name: "Skynet"},
}

// BuiltinThemeIDs returns the ids of the fixed built-in themes (see
// builtinThemes above). Intended for the loader boundary: pass this as
// theme.Load's reserved ids (see cmd/gateway/main.go's loadThemeRegistry) so
// an external theme directory can never load with the same id as a built-in
// and shadow it -- the built-in always wins, and the collision is skipped
// with a slog.Warn at load time rather than surfacing as a silent duplicate
// in themeOptions/PublicThemeView.
func BuiltinThemeIDs() []string {
	ids := make([]string, len(builtinThemes))
	for i, o := range builtinThemes {
		ids[i] = o.ID
	}
	return ids
}

// themeOptions is the full registry of selectable themes: the built-ins plus
// every externally loaded theme (s.themes is always non-nil -- see
// ServiceDeps.Themes / NewService).
func (s *Service) themeOptions(ctx context.Context) []ThemeOption {
	external := s.themes.Options()
	opts := make([]ThemeOption, 0, len(builtinThemes)+len(external))
	opts = append(opts, builtinThemes...)
	for _, o := range external {
		opts = append(opts, ThemeOption{ID: o.ID, Name: o.Name})
	}
	return opts
}

// isKnownTheme reports whether id is a selectable theme: a built-in or an
// externally loaded one.
func (s *Service) isKnownTheme(ctx context.Context, id string) bool {
	for _, o := range s.themeOptions(ctx) {
		if o.ID == id {
			return true
		}
	}
	return false
}

// KnownLanguages is the registry of selectable UI languages.
func KnownLanguages() []string { return []string{"de", "en"} }

// IsKnownLanguage reports whether id is a selectable UI language.
func IsKnownLanguage(id string) bool {
	for _, l := range KnownLanguages() {
		if l == id {
			return true
		}
	}
	return false
}

type SystemSettingsDTO struct {
	Theme                string        `json:"theme"`
	AvailableThemes      []ThemeOption `json:"available_themes"`
	Language             string        `json:"language"`
	AvailableLanguages   []string      `json:"available_languages"`
	CaptureRetentionDays int           `json:"capture_retention_days"`
	CaptureEnabled       bool          `json:"capture_enabled"`
	CaptureOverride      bool          `json:"capture_override"`

	HealthCheckIntervalSeconds int `json:"health_check_interval_seconds"`

	// AgentPresenceTimeoutSeconds is the effective system-wide default window
	// (seconds) for "the ServerAgent is delivering values"; a per-server
	// override (0 = follow this) mirrors the health-check-interval pattern.
	AgentPresenceTimeoutSeconds int `json:"agent_presence_timeout_seconds"`

	TOTPMode string `json:"totp_mode"`

	VisionProbeMode string `json:"vision_probe_mode"`

	// RouteAffinitySessionMode selects the resolver's affinity session-key
	// source: "client_session" (default) keys on the extracted client session
	// id; "legacy_header" keys on the explicit affinity header.
	RouteAffinitySessionMode string `json:"route_affinity_session_mode"`

	// Energy-attribution defaults (purely additive — no engine consumes these
	// yet; a later phase falls back to them when a per-mapping/per-server
	// value is unknown). All default 0 = "unset / no default".
	EnergyDefaultPricePerKwh float64 `json:"energy_default_price_per_kwh"`
	EnergyDefaultPue         float64 `json:"energy_default_pue"`
	EnergyDefaultWhPerToken  float64 `json:"energy_default_wh_per_token"`
	// EnergyDefaultPriceUnit is the display/entry unit for
	// EnergyDefaultPricePerKwh (one of currency.go's shared units; default
	// eur_cent).
	EnergyDefaultPriceUnit string `json:"energy_default_price_unit"`

	// CurrencyUsdPerEur is the system-wide USD-per-1-EUR conversion factor
	// used to derive a USD display value from an internally-stored EUR
	// cost; 0 = unset (USD display unavailable).
	CurrencyUsdPerEur float64 `json:"currency_usd_per_eur"`

	SMTPEnabled     bool   `json:"smtp_enabled"`
	SMTPHost        string `json:"smtp_host"`
	SMTPPort        int    `json:"smtp_port"`
	SMTPUsername    string `json:"smtp_username"`
	SMTPFrom        string `json:"smtp_from"`
	SMTPFromName    string `json:"smtp_from_name"`
	SMTPTLSMode     string `json:"smtp_tls_mode"`
	SMTPPasswordSet bool   `json:"smtp_password_set"`

	// SystemAdminModeRequirePassword is the system_admin_mode_require_password
	// toggle; default true.
	SystemAdminModeRequirePassword bool `json:"system_admin_mode_require_password"`

	NetbirdEnabled           bool     `json:"netbird_enabled"`
	NetbirdURL               string   `json:"netbird_url"`
	NetbirdGroups            []string `json:"netbird_groups"`
	NetbirdTokenSet          bool     `json:"netbird_token_set"` // NEVER the token value
	NetbirdOnly              bool     `json:"netbird_only"`
	NetbirdAgentDownloadOnly bool     `json:"netbird_agent_download_only"`
	NetbirdGatewayPeerID     string   `json:"netbird_gateway_peer_id"`
	NetbirdGatewayPeerName   string   `json:"netbird_gateway_peer_name"`

	NetbirdManagePolicies           bool   `json:"netbird_manage_policies"`
	NetbirdPolicyScope              string `json:"netbird_policy_scope"`
	NetbirdEffectivePolicyScope     string `json:"netbird_effective_policy_scope"`
	NetbirdDenyByDefault            bool   `json:"netbird_deny_by_default"`
	NetbirdDenyByDefaultEnforce     bool   `json:"netbird_deny_by_default_enforce"`
	NetbirdAllowPingGateway         bool   `json:"netbird_allow_ping_gateway"`
	NetbirdAllowPingAllServers      bool   `json:"netbird_allow_ping_all_servers"`
	NetbirdPeerSyncIntervalSeconds  int    `json:"netbird_peer_sync_interval_seconds"`
	NetbirdReconcileIntervalSeconds int    `json:"netbird_reconcile_interval_seconds"`
	// NetbirdTokenRotateBeforeDays is the operator-settable auto-rotation
	// threshold (days before expiry); 0 disables auto-rotation.
	NetbirdTokenRotateBeforeDays int `json:"netbird_token_rotate_before_days"`

	// ResourceProvisioningEnforce is the resource_provisioning_enforce toggle
	// (Resource Groups Phase 2); default false (opt-in — a server with no
	// provisioned resource group stays unrestricted).
	ResourceProvisioningEnforce bool `json:"resource_provisioning_enforce"`

	// Certificate management (cert_*/acme_*). CertEnabled alone (no email/
	// domain) is a valid, "on but not yet usable" state — see CertSettings.
	CertEnabled                bool     `json:"cert_enabled"`
	CertIssuerMode             string   `json:"cert_issuer_mode"`
	CertSelfSignedValidityDays int      `json:"cert_self_signed_validity_days"`
	CertCARenewBeforeDays      int      `json:"cert_ca_renew_before_days"`
	ACMEEmail                  string   `json:"acme_email"`
	ACMEDirectoryURL           string   `json:"acme_directory_url"`
	CertBaseDomain             string   `json:"cert_base_domain"`
	CertGatewayDomain          string   `json:"cert_gateway_domain"`
	CertServerScope            string   `json:"cert_server_scope"`
	CertManagePublicDomain     bool     `json:"cert_manage_public_domain"`
	CertPublicDomains          []string `json:"cert_public_domains"`
	CertRenewBeforeDays        int      `json:"cert_renew_before_days"`

	// Edge (gateway's own nginx) certificate — a distinct issuance target
	// from the internal/mesh fields above, with its own issuer mode
	// switchable independently of CertIssuerMode.
	CertEdgeEnabled    bool     `json:"cert_edge_enabled"`
	CertEdgeIssuerMode string   `json:"cert_edge_issuer_mode"`
	CertEdgeNames      []string `json:"cert_edge_names"`
	// CertEdgeRequireHTTPS is the plan-B plaintext-refusal switch: when true,
	// (a later task's) gate refuses unencrypted portal/API traffic. This task
	// only stores it -- see config.Config.CertEdgeRequireHTTPSDisable for the
	// env-only kill switch that overrides it, and the arming precondition
	// (recent encrypted traffic observed) a later task adds before this field
	// gets a consumer.
	CertEdgeRequireHTTPS bool `json:"cert_edge_require_https"`
	// CertMeshRequireTLS is P3's plaintext-refusal switch for the mesh agent
	// listener. Exposed so the certificate view can render the toggle; the arming
	// precondition and enforcement live in internal/gateway, and
	// config.Config.CertMeshRequireTLSDisable is the env-only kill switch.
	CertMeshRequireTLS bool `json:"cert_mesh_require_tls"`
	// CertMeshTLSMode is the stored agent-listener TLS-port topology switch:
	// "combined" (today's single-listener behavior) or "separate" (a later
	// task's dedicated encrypted listener), or "" meaning "follow
	// config.Config.AgentTLSSeparate" -- byte-neutral default. See
	// CertMeshTLSMode (the reader) and Service.CertMeshTLSSeparateActive (the
	// effective resolver).
	CertMeshTLSMode string `json:"cert_mesh_tls_mode"`
	// CertMeshTLSPort is a read-only display of the effective port a later
	// task's separate encrypted agent listener would bind (config.Config.
	// AgentTLSPort/AgentTLSAddr, resolved by cmd/gateway and threaded in via
	// ServiceDeps.AgentTLSPort). Never itself writable through this DTO.
	CertMeshTLSPort int `json:"cert_mesh_tls_port"`
	// CertMeshTLSSeparateActive is the read-only EFFECTIVE value of
	// CertMeshTLSMode: the stored mode when it is "combined"/"separate", else
	// the env-fallback default (ServiceDeps.AgentTLSSeparateDefault). See
	// Service.CertMeshTLSSeparateActive.
	CertMeshTLSSeparateActive bool `json:"cert_mesh_tls_separate_active"`

	// CertPublicIssuerMode is the public-domain issuer mode; "" means "follow
	// cert_issuer_mode" -- see CertSettings.modeFor. Byte-neutral default.
	CertPublicIssuerMode string `json:"cert_public_issuer_mode"`
	// ACMEWeeklyLimit is the per-week issuance ceiling for the GLOBAL (shared)
	// ACME account; 0 = no limit set.
	ACMEWeeklyLimit int `json:"acme_weekly_limit"`

	// CertEdgeACMEShared/CertPublicACMEShared: true (the default, absent means
	// shared -- byte-neutral) means that context re-uses the global ACME
	// account; false means it uses its own via the sibling fields below. See
	// CertSettings.certAcmeConfigFor.
	CertEdgeACMEShared       bool   `json:"cert_edge_acme_shared"`
	CertEdgeACMEEmail        string `json:"cert_edge_acme_email"`
	CertEdgeACMEDirectoryURL string `json:"cert_edge_acme_directory_url"`
	CertEdgeACMEWeeklyLimit  int    `json:"cert_edge_acme_weekly_limit"`

	CertPublicACMEShared       bool   `json:"cert_public_acme_shared"`
	CertPublicACMEEmail        string `json:"cert_public_acme_email"`
	CertPublicACMEDirectoryURL string `json:"cert_public_acme_directory_url"`
	CertPublicACMEWeeklyLimit  int    `json:"cert_public_acme_weekly_limit"`

	// CertHTTPSSwitchMode is P4's global https-auto-switch mode: "manual"
	// (default -- the gateway changes no app scheme), "auto" (every managed
	// server in scope except an explicit per-server "exclude"), or "selected"
	// (only a server explicitly overridden "include"). See CertHTTPSSwitchMode
	// (the reader) and httpsSwitchInScope (the per-server resolver).
	CertHTTPSSwitchMode string `json:"cert_https_switch_mode"`
	// CertProxyListenPortBase is the auto-assign floor for a managed
	// application's ProxyListenPort (the TLS port the agent's local proxy
	// listens on) -- the lowest candidate port considered when assigning one.
	// Default 8600; see CertProxyListenPortBase (the reader).
	CertProxyListenPortBase int `json:"cert_proxy_listen_port_base"`
}

type UpdateSystemSettingsRequest struct {
	Theme                *string `json:"theme"`
	Language             *string `json:"language"`
	CaptureRetentionDays *int    `json:"capture_retention_days"`
	CaptureEnabled       *bool   `json:"capture_enabled"`
	CaptureOverride      *bool   `json:"capture_override"`

	HealthCheckIntervalSeconds *int `json:"health_check_interval_seconds"`

	AgentPresenceTimeoutSeconds *int `json:"agent_presence_timeout_seconds"`

	TOTPMode *string `json:"totp_mode"`

	VisionProbeMode *string `json:"vision_probe_mode"`

	RouteAffinitySessionMode *string `json:"route_affinity_session_mode"`

	// Energy-attribution defaults; nil = keep the stored value. Must be >= 0
	// when set (0 resets to "unset / no default").
	EnergyDefaultPricePerKwh *float64 `json:"energy_default_price_per_kwh"`
	EnergyDefaultPue         *float64 `json:"energy_default_pue"`
	EnergyDefaultWhPerToken  *float64 `json:"energy_default_wh_per_token"`
	// EnergyDefaultPriceUnit: nil = keep the stored value. Lenient — any
	// value is normalized via NormalizePriceUnit, never rejected.
	EnergyDefaultPriceUnit *string `json:"energy_default_price_unit,omitempty"`

	// CurrencyUsdPerEur: nil = keep the stored value. Must be >= 0 when set
	// (0 resets to "unset").
	CurrencyUsdPerEur *float64 `json:"currency_usd_per_eur,omitempty"`

	SMTPEnabled  *bool   `json:"smtp_enabled"`
	SMTPHost     *string `json:"smtp_host"`
	SMTPPort     *int    `json:"smtp_port"`
	SMTPUsername *string `json:"smtp_username"`
	// SMTPPassword: nil = keep the stored value, "" = clear, non-empty = replace.
	SMTPPassword *string `json:"smtp_password"`
	SMTPFrom     *string `json:"smtp_from"`
	SMTPFromName *string `json:"smtp_from_name"`
	SMTPTLSMode  *string `json:"smtp_tls_mode"`

	// SystemAdminModeRequirePassword: nil = keep the stored value.
	SystemAdminModeRequirePassword *bool `json:"system_admin_mode_require_password"`

	NetbirdEnabled *bool     `json:"netbird_enabled"`
	NetbirdURL     *string   `json:"netbird_url"`
	NetbirdGroups  *[]string `json:"netbird_groups"`
	// NetbirdToken: nil = keep the stored value, "" = clear, non-empty = replace.
	NetbirdToken             *string `json:"netbird_token"`
	NetbirdOnly              *bool   `json:"netbird_only"`
	NetbirdAgentDownloadOnly *bool   `json:"netbird_agent_download_only"`
	NetbirdGatewayPeerID     *string `json:"netbird_gateway_peer_id"`
	NetbirdGatewayPeerName   *string `json:"netbird_gateway_peer_name"`

	NetbirdManagePolicies           *bool   `json:"netbird_manage_policies"`
	NetbirdPolicyScope              *string `json:"netbird_policy_scope"`
	NetbirdDenyByDefault            *bool   `json:"netbird_deny_by_default"`
	NetbirdDenyByDefaultEnforce     *bool   `json:"netbird_deny_by_default_enforce"`
	NetbirdAllowPingGateway         *bool   `json:"netbird_allow_ping_gateway"`
	NetbirdAllowPingAllServers      *bool   `json:"netbird_allow_ping_all_servers"`
	NetbirdPeerSyncIntervalSeconds  *int    `json:"netbird_peer_sync_interval_seconds"`
	NetbirdReconcileIntervalSeconds *int    `json:"netbird_reconcile_interval_seconds"`
	NetbirdTokenRotateBeforeDays    *int    `json:"netbird_token_rotate_before_days"`

	// ResourceProvisioningEnforce: nil = keep the stored value.
	ResourceProvisioningEnforce *bool `json:"resource_provisioning_enforce"`

	// Certificate management (cert_*/acme_*); nil = keep the stored value.
	// The runtime-managed CA/ACME-account keys (cert_ca_*, acme_account_*)
	// deliberately have NO corresponding field here — only the reconcile /
	// rotation action (a later task) writes them.
	CertEnabled                *bool     `json:"cert_enabled"`
	CertIssuerMode             *string   `json:"cert_issuer_mode"`
	CertSelfSignedValidityDays *int      `json:"cert_self_signed_validity_days"`
	CertCARenewBeforeDays      *int      `json:"cert_ca_renew_before_days"`
	ACMEEmail                  *string   `json:"acme_email"`
	ACMEDirectoryURL           *string   `json:"acme_directory_url"`
	CertBaseDomain             *string   `json:"cert_base_domain"`
	CertGatewayDomain          *string   `json:"cert_gateway_domain"`
	CertServerScope            *string   `json:"cert_server_scope"`
	CertManagePublicDomain     *bool     `json:"cert_manage_public_domain"`
	CertPublicDomains          *[]string `json:"cert_public_domains"`
	CertRenewBeforeDays        *int      `json:"cert_renew_before_days"`

	// Edge (gateway's own nginx) certificate; nil = keep the stored value.
	CertEdgeEnabled    *bool     `json:"cert_edge_enabled"`
	CertEdgeIssuerMode *string   `json:"cert_edge_issuer_mode"`
	CertEdgeNames      *[]string `json:"cert_edge_names"`
	// CertEdgeRequireHTTPS: nil = keep the stored value. An ordinary boolean --
	// no validation, no completeness gate against the other edge fields (the
	// arming precondition lives entirely in the gate a later task adds, not
	// here).
	CertEdgeRequireHTTPS *bool `json:"cert_edge_require_https"`
	// CertMeshRequireTLS: nil = keep the stored value. An ordinary boolean like
	// CertEdgeRequireHTTPS; the arming precondition (recent TLS observed) is enforced
	// in the HTTP handler, not here.
	CertMeshRequireTLS *bool `json:"cert_mesh_require_tls"`
	// CertMeshTLSMode: nil = keep the stored value. "" is itself a legal,
	// explicit value ("follow config.Config.AgentTLSSeparate"); any other
	// value must be exactly "combined" or "separate" or the write is rejected
	// with ErrCertInvalid.
	CertMeshTLSMode *string `json:"cert_mesh_tls_mode"`

	// CertPublicIssuerMode: nil = keep the stored value; "" is itself a legal,
	// explicit value ("follow cert_issuer_mode" -- see CertSettings.modeFor),
	// distinct from nil.
	CertPublicIssuerMode *string `json:"cert_public_issuer_mode"`
	// ACMEWeeklyLimit: nil = keep the stored value. Must be >= 0 when set.
	ACMEWeeklyLimit *int `json:"acme_weekly_limit"`

	// CertEdgeACMEShared/CertPublicACMEShared and their sibling fields; nil =
	// keep the stored value throughout. See SystemSettingsDTO's doc for the
	// shared-by-default (byte-neutral) semantics.
	CertEdgeACMEShared       *bool   `json:"cert_edge_acme_shared"`
	CertEdgeACMEEmail        *string `json:"cert_edge_acme_email"`
	CertEdgeACMEDirectoryURL *string `json:"cert_edge_acme_directory_url"`
	CertEdgeACMEWeeklyLimit  *int    `json:"cert_edge_acme_weekly_limit"`

	CertPublicACMEShared       *bool   `json:"cert_public_acme_shared"`
	CertPublicACMEEmail        *string `json:"cert_public_acme_email"`
	CertPublicACMEDirectoryURL *string `json:"cert_public_acme_directory_url"`
	CertPublicACMEWeeklyLimit  *int    `json:"cert_public_acme_weekly_limit"`

	// CertHTTPSSwitchMode: nil = keep the stored value. "" is itself a legal,
	// explicit value (byte-neutral default -- reads back "manual"); any other
	// value must be exactly "manual", "auto", or "selected", or the write is
	// rejected with ErrCertInvalid.
	CertHTTPSSwitchMode *string `json:"cert_https_switch_mode"`
	// CertProxyListenPortBase: nil = keep the stored value. Must be in
	// [1024,65535] when set, or the write is rejected with ErrCertInvalid.
	CertProxyListenPortBase *int `json:"cert_proxy_listen_port_base"`
}

// smtpSettingsFields is the SINGLE SOURCE for the SMTP domain: every
// UpdateSystemSettingsRequest field name that touchesSMTP() tests. Keeping
// this as the one list (instead of also hand-listing the same fields in
// touchesSMTP's body) is what TestEveryRequestFieldIsClassified in
// service_system_settings_touches_test.go polices against drift — see PT-4.
var smtpSettingsFields = []string{
	"SMTPEnabled", "SMTPHost", "SMTPPort", "SMTPUsername",
	"SMTPPassword", "SMTPFrom", "SMTPFromName", "SMTPTLSMode",
}

// netbirdSettingsFields is the SINGLE SOURCE backing touchesNetbird().
// Deliberately narrower than "every Netbird*-prefixed field": these are only
// the four fields whose write path needs the validateNetbird completeness
// check and secret-sealing below in UpdateSystemSettings. The other NetBird
// fields (netbird_only, the policy-management toggles, the two sync
// intervals, the gateway-peer fields, the rotation threshold, ...) are
// simple unconditional top-level writes with their own ad-hoc side-effect
// triggers elsewhere in UpdateSystemSettings, and are listed in
// nonReconcileFields in service_system_settings_touches_test.go, not here.
var netbirdSettingsFields = []string{
	"NetbirdEnabled", "NetbirdURL", "NetbirdGroups", "NetbirdToken",
}

// certSettingsFields is the SINGLE SOURCE backing touchesCert(): every
// cert_*/acme_* field on UpdateSystemSettingsRequest. Unlike
// netbirdSettingsFields, this domain is (and must stay) EXHAUSTIVE over its
// prefix — see touchesCert's doc comment and OnCertSettingsChanged for the
// stale cert_last_error hazard (up to 900s) that a forgotten field here
// causes.
var certSettingsFields = []string{
	"CertEnabled", "CertIssuerMode", "CertSelfSignedValidityDays", "CertCARenewBeforeDays",
	"ACMEEmail", "ACMEDirectoryURL", "CertBaseDomain", "CertGatewayDomain", "CertServerScope",
	"CertManagePublicDomain", "CertPublicDomains", "CertRenewBeforeDays",
	"CertEdgeEnabled", "CertEdgeIssuerMode", "CertEdgeNames",
	"CertEdgeRequireHTTPS", "CertMeshRequireTLS", "CertMeshTLSMode",
	"CertPublicIssuerMode", "ACMEWeeklyLimit",
	"CertEdgeACMEShared", "CertEdgeACMEEmail", "CertEdgeACMEDirectoryURL", "CertEdgeACMEWeeklyLimit",
	"CertPublicACMEShared", "CertPublicACMEEmail", "CertPublicACMEDirectoryURL", "CertPublicACMEWeeklyLimit",
	"CertHTTPSSwitchMode", "CertProxyListenPortBase",
}

// requestTouchesAny reports whether req carries a value for any of the named
// UpdateSystemSettingsRequest fields, and underlies the
// touchesSMTP/touchesNetbird/touchesCert domain predicates below so each
// domain's field set is declared exactly once (as *SettingsFields above)
// rather than a second time, by hand, in each predicate's body.
//
// Every field named in a domain list is expected to be pointer-typed (nil =
// "not carried by this request", the DTO's convention throughout), in which
// case "touched" means non-nil. A field that turns out to be a non-pointer
// kind is instead treated as touched whenever it is not its type's zero
// value — this keeps the helper correct even if a future non-pointer field
// were ever added to a domain list, though none exist today.
// TestSettingsDomainFieldsExist (service_system_settings_touches_test.go)
// asserts every name here actually resolves to a struct field, so a
// rename/removal fails a test instead of silently never matching.
func requestTouchesAny(req UpdateSystemSettingsRequest, fields []string) bool {
	v := reflect.ValueOf(req)
	for _, name := range fields {
		f := v.FieldByName(name)
		if !f.IsValid() {
			continue // caught by TestSettingsDomainFieldsExist; fail safe here.
		}
		if f.Kind() == reflect.Pointer {
			if !f.IsNil() {
				return true
			}
			continue
		}
		if !f.IsZero() {
			return true
		}
	}
	return false
}

// touchesSMTP reports whether the update carries any SMTP field, so
// UpdateSystemSettings only reads/validates/persists the SMTP block when asked.
func (r UpdateSystemSettingsRequest) touchesSMTP() bool {
	return requestTouchesAny(r, smtpSettingsFields)
}

// touchesNetbird reports whether the update carries any NetBird field, so
// UpdateSystemSettings only reads/validates/persists the NetBird block when asked.
func (r UpdateSystemSettingsRequest) touchesNetbird() bool {
	return requestTouchesAny(r, netbirdSettingsFields)
}

// touchesCert reports whether the update carries any cert_*/acme_* field, so
// UpdateSystemSettings only reads/validates/persists the certificate block
// when asked. Covers both the cert_* fields and the ACME-specific fields
// (issuer mode, self-signed validity, CA renewal window) — everything on
// UpdateSystemSettingsRequest that this task adds.
func (r UpdateSystemSettingsRequest) touchesCert() bool {
	return requestTouchesAny(r, certSettingsFields)
}

func (s *Service) activeTheme(ctx context.Context) string {
	if s.settings == nil {
		return defaultTheme
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return defaultTheme
	}
	if t, ok := values["theme"]; ok && t != "" {
		return t
	}
	return defaultTheme
}

func (s *Service) PublicTheme(ctx context.Context) string { return s.activeTheme(ctx) }

// ThemePublicView is the public, pre-auth payload for GET /api/system/theme:
// the active theme id, whether it resolves to a built-in (the frontend uses
// its own compiled copy) or an externally loaded theme, and -- only for the
// external case -- that theme's full data, so even the pre-auth login screen
// (no session yet) can render it.
type ThemePublicView struct {
	Theme  string       `json:"theme"`
	Source string       `json:"source"`
	Data   *theme.Theme `json:"data"`
}

// PublicThemeView returns the active theme's public view: Source is
// "external" with Data populated when the active theme id resolves in the
// loaded external-theme registry (s.themes); otherwise Source is "builtin"
// with Data nil, and the frontend falls back to its compiled built-in theme.
func (s *Service) PublicThemeView(ctx context.Context) ThemePublicView {
	id := s.activeTheme(ctx)
	if th, ok := s.themes.Get(id); ok {
		return ThemePublicView{Theme: id, Source: "external", Data: th}
	}
	return ThemePublicView{Theme: id, Source: "builtin"}
}

// ExternalThemeAsset resolves the favicon or logo file path for a LOADED
// external theme id. kind must be "favicon" or "logo"; any other kind (or an
// id that never loaded, e.g. a path-traversal attempt) reports ok=false.
//
// id is resolved ONLY against s.themes -- the registry of ids actually
// loaded from disk at startup -- and is NEVER used to build a filesystem
// path from caller input. A caller-supplied id that isn't a real, loaded
// theme simply misses this lookup; it can never reach outside a theme's own
// directory.
func (s *Service) ExternalThemeAsset(id, kind string) (path string, ok bool) {
	th, found := s.themes.Get(id)
	if !found {
		return "", false
	}
	switch kind {
	case "favicon":
		return th.FaviconPath, th.FaviconPath != ""
	case "logo":
		return th.LogoPath, th.LogoPath != ""
	default:
		return "", false
	}
}

func (s *Service) activeLanguage(ctx context.Context) string {
	if s.settings == nil {
		return defaultLanguage
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return defaultLanguage
	}
	if l, ok := values["language"]; ok && l != "" {
		return l
	}
	return defaultLanguage
}

func (s *Service) PublicLanguage(ctx context.Context) string { return s.activeLanguage(ctx) }

func (s *Service) activeCaptureRetentionDays(ctx context.Context) int {
	if s.settings == nil {
		return DefaultCaptureRetentionDays
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return DefaultCaptureRetentionDays
	}
	return CaptureRetentionDays(values)
}

func (s *Service) activeCaptureEnabled(ctx context.Context) bool {
	if s.settings == nil {
		return true
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return true
	}
	return CaptureEnabled(values)
}

func (s *Service) activeCaptureOverride(ctx context.Context) bool {
	if s.settings == nil {
		return false
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return false
	}
	return CaptureOverride(values)
}

func (s *Service) activeTOTPMode(ctx context.Context) string {
	if s.settings == nil {
		return DefaultTOTPMode
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return DefaultTOTPMode
	}
	return TOTPMode(values)
}

// TOTPMode is the totp_mode read for the gateway's login/enrollment flow. The
// gateway reads the effective mode ONLY through here.
func (s *Service) TOTPMode(ctx context.Context) string { return s.activeTOTPMode(ctx) }

// RouteAffinitySessionMode is the concrete accessor for the effective
// route_affinity_session_mode setting. It is intentionally NOT on the
// portal.API interface (so test fakes + the generated tracing decorator stay
// untouched); cmd/gateway reads it off the concrete *Service (or the DTO) to
// wire the resolver. Nil-safe: a missing store or a read error yields the
// default.
func (s *Service) RouteAffinitySessionMode(ctx context.Context) string {
	if s.settings == nil {
		return DefaultRouteAffinitySessionMode
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return DefaultRouteAffinitySessionMode
	}
	return RouteAffinitySessionMode(values)
}

func (s *Service) activeVisionProbeMode(ctx context.Context) string {
	if s.settings == nil {
		return DefaultVisionProbeMode
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return DefaultVisionProbeMode
	}
	return VisionProbeMode(values)
}

// VisionProbeMode is the vision_probe_mode read by the vision benchmark to choose
// its detection method. The gateway reads the effective mode ONLY through here.
func (s *Service) VisionProbeMode(ctx context.Context) string { return s.activeVisionProbeMode(ctx) }

// CurrencyUsdPerEur is the ctx-carrying accessor for the currency_usd_per_eur
// system setting (USD per 1 EUR conversion factor), exposed on the
// portal.API interface for the /api/portal/currency endpoint. Nil-safe: a
// missing store, a read error, an unparseable value, or a negative value all
// read back as 0 (negative is rejected only at write time, in
// UpdateSystemSettings). The method name intentionally mirrors the
// package-level CurrencyUsdPerEur(values) above (method vs function
// namespaces do not collide, same as HealthCheckIntervalSeconds below).
func (s *Service) CurrencyUsdPerEur(ctx context.Context) float64 {
	if s.settings == nil {
		return 0
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return 0
	}
	return CurrencyUsdPerEur(values)
}

// HealthCheckIntervalSeconds is the effective system-wide app-health probe
// cadence, exposed to portal users (below the system scope) so the application
// form can show the live "Standard" interval. Nil-safe default 30. The method
// name intentionally mirrors the package-level HealthCheckIntervalSeconds(values)
// (method vs function namespaces do not collide, same as TOTPMode).
func (s *Service) HealthCheckIntervalSeconds(ctx context.Context) int {
	return s.activeHealthCheckIntervalSeconds(ctx)
}

func (s *Service) activeHealthCheckIntervalSeconds(ctx context.Context) int {
	if s.settings == nil {
		return DefaultHealthCheckIntervalSeconds
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return DefaultHealthCheckIntervalSeconds
	}
	return HealthCheckIntervalSeconds(values)
}

// NetbirdOnly is the runtime netbird_only toggle read by the gateway's inbound
// agent gate + outbound off-mesh refusal. Nil-safe; a missing store or read
// error returns the safe default false (callers must never be blackholed by a
// settings glitch).
func (s *Service) NetbirdOnly(ctx context.Context) bool {
	if s.settings == nil {
		return false
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return false
	}
	return NetbirdOnly(values)
}

// ResourceProvisioningEnforce is the runtime resource_provisioning_enforce
// toggle consulted by AllowedServerIDs. Nil-safe; a missing store or read
// error returns the safe default false (opt-in mode — a settings glitch must
// never silently start rejecting traffic that was never gated before).
func (s *Service) ResourceProvisioningEnforce(ctx context.Context) bool {
	if s.settings == nil {
		return false
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return false
	}
	return ResourceProvisioningEnforce(values)
}

// SystemAdminModeRequirePassword is the runtime system_admin_mode_require_password
// toggle consulted when a system_admin enters System-Admin mode. Nil-safe; a
// missing store or read error returns the safe default true (never silently
// drop the step-up password requirement).
func (s *Service) SystemAdminModeRequirePassword(ctx context.Context) bool {
	if s.settings == nil {
		return true
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return true
	}
	return SystemAdminModeRequirePassword(values)
}

// NetbirdAgentDownloadOnly is the runtime toggle restricting the agent-token
// binary download endpoint to the NetBird mesh. Nil-safe; a missing store or
// read error returns the safe default false.
func (s *Service) NetbirdAgentDownloadOnly(ctx context.Context) bool {
	if s.settings == nil {
		return false
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return false
	}
	return NetbirdAgentDownloadOnly(values)
}

// NetbirdGatewayPeerID is the selected gateway peer id read at startup to
// resolve the agent-listener bind IP. Nil-safe; a missing store or read error
// returns "".
func (s *Service) NetbirdGatewayPeerID(ctx context.Context) string {
	if s.settings == nil {
		return ""
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return ""
	}
	return NetbirdGatewayPeerID(values)
}

// NetbirdTokenRotateBeforeDays is the effective auto-rotation threshold: the KV
// value when a valid non-negative integer is stored, else the env-fallback
// default (deps.NetbirdTokenRotateBeforeDaysDefault, itself defaulting to 14).
func (s *Service) NetbirdTokenRotateBeforeDays(values map[string]string) int {
	raw := strings.TrimSpace(values[netbirdTokenRotateBeforeKey])
	if raw == "" {
		return s.netbird.tokenRotateBeforeDefault
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return s.netbird.tokenRotateBeforeDefault
	}
	return n
}

func (s *Service) SystemSettingsView(ctx context.Context) SystemSettingsDTO {
	dto := SystemSettingsDTO{
		Theme:                s.activeTheme(ctx),
		AvailableThemes:      s.themeOptions(ctx),
		Language:             s.activeLanguage(ctx),
		AvailableLanguages:   KnownLanguages(),
		CaptureRetentionDays: s.activeCaptureRetentionDays(ctx),
		CaptureEnabled:       s.activeCaptureEnabled(ctx),
		CaptureOverride:      s.activeCaptureOverride(ctx),

		HealthCheckIntervalSeconds: s.activeHealthCheckIntervalSeconds(ctx),

		AgentPresenceTimeoutSeconds: s.activeAgentPresenceTimeoutSeconds(ctx),

		TOTPMode: s.activeTOTPMode(ctx),

		VisionProbeMode: s.activeVisionProbeMode(ctx),

		RouteAffinitySessionMode: s.RouteAffinitySessionMode(ctx),

		// SMTP defaults hold when the store is absent/unreadable.
		SMTPPort:    DefaultSMTPPort,
		SMTPTLSMode: DefaultSMTPTLSMode,
	}
	if s.settings != nil {
		if values, err := s.settings.SystemSettings(ctx); err == nil {
			dto.EnergyDefaultPricePerKwh = EnergyDefaultPricePerKwh(values)
			dto.EnergyDefaultPue = EnergyDefaultPue(values)
			dto.EnergyDefaultWhPerToken = EnergyDefaultWhPerToken(values)
			dto.EnergyDefaultPriceUnit = EnergyDefaultPriceUnit(values)

			dto.CurrencyUsdPerEur = CurrencyUsdPerEur(values)

			dto.SMTPEnabled = SMTPEnabled(values)
			dto.SMTPHost = values[smtpHostKey]
			dto.SMTPPort = SMTPPort(values)
			dto.SMTPUsername = values[smtpUsernameKey]
			dto.SMTPFrom = values[smtpFromKey]
			dto.SMTPFromName = values[smtpFromNameKey]
			dto.SMTPTLSMode = SMTPTLSMode(values)
			dto.SMTPPasswordSet = values[smtpPasswordKey] != ""

			dto.NetbirdEnabled = NetbirdEnabled(values)
			dto.NetbirdURL = NetbirdURL(values)
			dto.NetbirdGroups = NetbirdGroups(values)
			dto.NetbirdTokenSet = values[netbirdTokenKey] != ""
			dto.SystemAdminModeRequirePassword = SystemAdminModeRequirePassword(values)

			dto.NetbirdOnly = NetbirdOnly(values)
			dto.NetbirdAgentDownloadOnly = NetbirdAgentDownloadOnly(values)
			dto.NetbirdGatewayPeerID = NetbirdGatewayPeerID(values)
			dto.NetbirdGatewayPeerName = NetbirdGatewayPeerName(values)

			dto.NetbirdManagePolicies = NetbirdManagePolicies(values)
			dto.NetbirdPolicyScope = NetbirdPolicyScope(values)
			dto.NetbirdEffectivePolicyScope = EffectiveNetbirdPolicyScope(dto.NetbirdPolicyScope, NetbirdDenyByDefault(values))
			dto.NetbirdDenyByDefault = NetbirdDenyByDefault(values)
			dto.NetbirdDenyByDefaultEnforce = NetbirdDenyByDefaultEnforce(values)
			dto.NetbirdAllowPingGateway = NetbirdAllowPingGateway(values)
			dto.NetbirdAllowPingAllServers = NetbirdAllowPingAllServers(values)
			dto.NetbirdPeerSyncIntervalSeconds = NetbirdPeerSyncIntervalSeconds(values)
			dto.NetbirdReconcileIntervalSeconds = NetbirdReconcileIntervalSeconds(values)
			dto.NetbirdTokenRotateBeforeDays = s.NetbirdTokenRotateBeforeDays(values)

			dto.ResourceProvisioningEnforce = ResourceProvisioningEnforce(values)

			dto.CertEnabled = CertEnabled(values)
			dto.CertIssuerMode = CertIssuerMode(values)
			dto.CertSelfSignedValidityDays = CertSelfSignedValidityDays(values)
			dto.CertCARenewBeforeDays = CertCARenewBeforeDays(values)
			dto.ACMEEmail = ACMEEmail(values)
			dto.ACMEDirectoryURL = ACMEDirectoryURL(values)
			dto.CertBaseDomain = CertBaseDomain(values)
			dto.CertGatewayDomain = CertGatewayDomain(values)
			dto.CertServerScope = CertServerScope(values)
			dto.CertManagePublicDomain = CertManagePublicDomain(values)
			dto.CertPublicDomains = CertPublicDomains(values)
			dto.CertRenewBeforeDays = CertRenewBeforeDays(values)

			dto.CertEdgeEnabled = CertEdgeEnabled(values)
			dto.CertEdgeIssuerMode = CertEdgeIssuerMode(values)
			dto.CertEdgeNames = CertEdgeNames(values)
			dto.CertEdgeRequireHTTPS = CertEdgeRequireHTTPS(values)
			dto.CertMeshRequireTLS = CertMeshRequireTLS(values)
			dto.CertMeshTLSMode = CertMeshTLSMode(values)
			dto.CertMeshTLSPort = s.CertMeshTLSPort()
			switch CertMeshTLSMode(values) {
			case "separate":
				dto.CertMeshTLSSeparateActive = true
			case "combined":
				dto.CertMeshTLSSeparateActive = false
			default:
				dto.CertMeshTLSSeparateActive = s.agentTLSSeparateDefault
			}

			dto.CertPublicIssuerMode = CertPublicIssuerMode(values)
			dto.ACMEWeeklyLimit = ACMEWeeklyLimit(values)

			dto.CertEdgeACMEShared = CertEdgeACMEShared(values)
			dto.CertEdgeACMEEmail = CertEdgeACMEEmail(values)
			dto.CertEdgeACMEDirectoryURL = CertEdgeACMEDirectoryURL(values)
			dto.CertEdgeACMEWeeklyLimit = CertEdgeACMEWeeklyLimit(values)

			dto.CertPublicACMEShared = CertPublicACMEShared(values)
			dto.CertPublicACMEEmail = CertPublicACMEEmail(values)
			dto.CertPublicACMEDirectoryURL = CertPublicACMEDirectoryURL(values)
			dto.CertPublicACMEWeeklyLimit = CertPublicACMEWeeklyLimit(values)

			dto.CertHTTPSSwitchMode = CertHTTPSSwitchMode(values)
			dto.CertProxyListenPortBase = CertProxyListenPortBase(values)
		}
	}
	return dto
}

// settingWrite is one pending (key, value) pair for UpdateSystemSettings.
// Writes are computed only after every field (including the SMTP block) has
// validated, so a later validation failure can never leave an earlier field
// partially applied to the store.
type settingWrite struct{ key, value string }

func (s *Service) UpdateSystemSettings(ctx context.Context, principal auth.Token, req UpdateSystemSettingsRequest) (SystemSettingsDTO, error) {
	if !isSystem(principal) {
		return SystemSettingsDTO{}, ErrPrincipalForbidden
	}
	var writes []settingWrite

	if req.Theme != nil {
		if !s.isKnownTheme(ctx, *req.Theme) {
			return SystemSettingsDTO{}, ErrThemeInvalid
		}
		writes = append(writes, settingWrite{"theme", *req.Theme})
	}
	if req.Language != nil {
		if !IsKnownLanguage(*req.Language) {
			return SystemSettingsDTO{}, ErrLanguageInvalid
		}
		writes = append(writes, settingWrite{"language", *req.Language})
	}
	if req.CaptureRetentionDays != nil {
		if *req.CaptureRetentionDays < minCaptureRetentionDays || *req.CaptureRetentionDays > maxCaptureRetentionDays {
			return SystemSettingsDTO{}, ErrRetentionInvalid
		}
		writes = append(writes, settingWrite{captureRetentionKey, strconv.Itoa(*req.CaptureRetentionDays)})
	}
	if req.CaptureEnabled != nil {
		writes = append(writes, settingWrite{captureEnabledKey, strconv.FormatBool(*req.CaptureEnabled)})
	}
	if req.CaptureOverride != nil {
		writes = append(writes, settingWrite{captureOverrideKey, strconv.FormatBool(*req.CaptureOverride)})
	}
	if req.HealthCheckIntervalSeconds != nil {
		if *req.HealthCheckIntervalSeconds < MinHealthCheckIntervalSeconds || *req.HealthCheckIntervalSeconds > MaxHealthCheckIntervalSeconds {
			return SystemSettingsDTO{}, ErrHealthCheckIntervalInvalid
		}
		writes = append(writes, settingWrite{healthCheckIntervalKey, strconv.Itoa(*req.HealthCheckIntervalSeconds)})
	}
	if req.AgentPresenceTimeoutSeconds != nil {
		if *req.AgentPresenceTimeoutSeconds < MinAgentPresenceTimeoutSeconds || *req.AgentPresenceTimeoutSeconds > MaxAgentPresenceTimeoutSeconds {
			return SystemSettingsDTO{}, ErrAgentPresenceTimeoutInvalid
		}
		writes = append(writes, settingWrite{agentPresenceTimeoutKey, strconv.Itoa(*req.AgentPresenceTimeoutSeconds)})
	}
	if req.TOTPMode != nil {
		if !isKnownTOTPMode(strings.TrimSpace(*req.TOTPMode)) {
			return SystemSettingsDTO{}, ErrTotpModeInvalid
		}
		writes = append(writes, settingWrite{totpModeKey, strings.TrimSpace(*req.TOTPMode)})
	}
	if req.VisionProbeMode != nil {
		if !isKnownVisionProbeMode(strings.TrimSpace(*req.VisionProbeMode)) {
			return SystemSettingsDTO{}, ErrVisionProbeModeInvalid
		}
		writes = append(writes, settingWrite{visionProbeModeKey, strings.TrimSpace(*req.VisionProbeMode)})
	}
	if req.RouteAffinitySessionMode != nil {
		if !isKnownRouteAffinitySessionMode(strings.TrimSpace(*req.RouteAffinitySessionMode)) {
			return SystemSettingsDTO{}, ErrRouteAffinitySessionModeInvalid
		}
		writes = append(writes, settingWrite{routeAffinitySessionModeKey, strings.TrimSpace(*req.RouteAffinitySessionMode)})
	}
	if req.EnergyDefaultPricePerKwh != nil {
		if err := validateEnergyDefault(*req.EnergyDefaultPricePerKwh); err != nil {
			return SystemSettingsDTO{}, err
		}
		writes = append(writes, settingWrite{energyDefaultPricePerKwhKey, strconv.FormatFloat(*req.EnergyDefaultPricePerKwh, 'f', -1, 64)})
	}
	if req.EnergyDefaultPue != nil {
		if err := validateEnergyDefault(*req.EnergyDefaultPue); err != nil {
			return SystemSettingsDTO{}, err
		}
		writes = append(writes, settingWrite{energyDefaultPueKey, strconv.FormatFloat(*req.EnergyDefaultPue, 'f', -1, 64)})
	}
	if req.EnergyDefaultWhPerToken != nil {
		if err := validateEnergyDefault(*req.EnergyDefaultWhPerToken); err != nil {
			return SystemSettingsDTO{}, err
		}
		writes = append(writes, settingWrite{energyDefaultWhPerTokenKey, strconv.FormatFloat(*req.EnergyDefaultWhPerToken, 'f', -1, 64)})
	}
	if req.EnergyDefaultPriceUnit != nil {
		// Lenient: NormalizePriceUnit never rejects, so this is a simple
		// top-level write (like netbird_policy_scope).
		writes = append(writes, settingWrite{energyDefaultPriceUnitKey, NormalizePriceUnit(*req.EnergyDefaultPriceUnit)})
	}
	if req.CurrencyUsdPerEur != nil {
		// Reuse the energy-default validation: negative rejected, 0 = unset.
		if err := validateEnergyDefault(*req.CurrencyUsdPerEur); err != nil {
			return SystemSettingsDTO{}, err
		}
		writes = append(writes, settingWrite{currencyUsdPerEurKey, strconv.FormatFloat(*req.CurrencyUsdPerEur, 'f', -1, 64)})
	}
	if req.SystemAdminModeRequirePassword != nil {
		writes = append(writes, settingWrite{systemAdminModeRequirePasswordKey, strconv.FormatBool(*req.SystemAdminModeRequirePassword)})
	}
	// resource_provisioning_enforce is non-secret and carries no
	// enable-requirement validation, so it is a simple top-level write (like
	// netbird_only above).
	if req.ResourceProvisioningEnforce != nil {
		writes = append(writes, settingWrite{resourceProvisioningEnforceKey, strconv.FormatBool(*req.ResourceProvisioningEnforce)})
	}
	// netbird_only + netbird_gateway_peer_id are non-secret and carry no
	// enable-requirement validation (unlike the netbird_enabled/url/token block
	// below), so they are simple top-level writes.
	if req.NetbirdOnly != nil {
		writes = append(writes, settingWrite{netbirdOnlyKey, strconv.FormatBool(*req.NetbirdOnly)})
	}
	if req.NetbirdAgentDownloadOnly != nil {
		writes = append(writes, settingWrite{netbirdAgentDownloadOnlyKey, strconv.FormatBool(*req.NetbirdAgentDownloadOnly)})
	}
	if req.NetbirdGatewayPeerID != nil {
		writes = append(writes, settingWrite{netbirdGatewayPeerIDKey, strings.TrimSpace(*req.NetbirdGatewayPeerID)})
	}
	if req.NetbirdGatewayPeerName != nil {
		writes = append(writes, settingWrite{netbirdGatewayPeerNameKey, strings.TrimSpace(*req.NetbirdGatewayPeerName)})
	}
	// The six policy/interval settings below are non-secret and carry no
	// enable-requirement validation (they apply regardless of whether the base
	// netbird_enabled module is on), so — aside from the interval-order check —
	// they are simple top-level writes, like netbird_only above.
	if req.NetbirdPeerSyncIntervalSeconds != nil || req.NetbirdReconcileIntervalSeconds != nil {
		values, err := s.settings.SystemSettings(ctx)
		if err != nil {
			return SystemSettingsDTO{}, err
		}
		peer := NetbirdPeerSyncIntervalSeconds(values)
		if req.NetbirdPeerSyncIntervalSeconds != nil {
			peer = *req.NetbirdPeerSyncIntervalSeconds
		}
		reconcile := NetbirdReconcileIntervalSeconds(values)
		if req.NetbirdReconcileIntervalSeconds != nil {
			reconcile = *req.NetbirdReconcileIntervalSeconds
		}
		if peer < MinNetbirdIntervalSeconds || reconcile < MinNetbirdIntervalSeconds || peer > reconcile {
			return SystemSettingsDTO{}, ErrNetbirdIntervalOrder
		}
	}
	if req.NetbirdManagePolicies != nil {
		writes = append(writes, settingWrite{netbirdManagePoliciesKey, strconv.FormatBool(*req.NetbirdManagePolicies)})
	}
	if req.NetbirdPolicyScope != nil {
		// Lenient: any value is stored as-is; the getter falls back to "auto"
		// for anything not in the known-scope list, so this never rejects.
		writes = append(writes, settingWrite{netbirdPolicyScopeKey, strings.TrimSpace(*req.NetbirdPolicyScope)})
	}
	if req.NetbirdDenyByDefault != nil {
		writes = append(writes, settingWrite{netbirdDenyByDefaultKey, strconv.FormatBool(*req.NetbirdDenyByDefault)})
	}
	if req.NetbirdDenyByDefaultEnforce != nil {
		writes = append(writes, settingWrite{netbirdDenyByDefaultEnforceKey, strconv.FormatBool(*req.NetbirdDenyByDefaultEnforce)})
	}
	if req.NetbirdAllowPingGateway != nil {
		writes = append(writes, settingWrite{netbirdAllowPingGatewayKey, strconv.FormatBool(*req.NetbirdAllowPingGateway)})
	}
	if req.NetbirdAllowPingAllServers != nil {
		writes = append(writes, settingWrite{netbirdAllowPingAllServersKey, strconv.FormatBool(*req.NetbirdAllowPingAllServers)})
	}
	if req.NetbirdPeerSyncIntervalSeconds != nil {
		writes = append(writes, settingWrite{netbirdPeerSyncIntervalKey, strconv.Itoa(*req.NetbirdPeerSyncIntervalSeconds)})
	}
	if req.NetbirdReconcileIntervalSeconds != nil {
		writes = append(writes, settingWrite{netbirdReconcileIntervalKey, strconv.Itoa(*req.NetbirdReconcileIntervalSeconds)})
	}
	// netbird_token_rotate_before_days is non-secret and carries no
	// enable-requirement validation (it applies regardless of whether the base
	// netbird_enabled module is on), so it is a simple top-level write, like
	// netbird_only above; 0 is valid (disables auto-rotation).
	if req.NetbirdTokenRotateBeforeDays != nil {
		if *req.NetbirdTokenRotateBeforeDays < 0 {
			return SystemSettingsDTO{}, ErrNetbirdTokenRotateBeforeInvalid
		}
		writes = append(writes, settingWrite{netbirdTokenRotateBeforeKey, strconv.Itoa(*req.NetbirdTokenRotateBeforeDays)})
	}
	if req.touchesSMTP() {
		values, err := s.settings.SystemSettings(ctx)
		if err != nil {
			return SystemSettingsDTO{}, err
		}
		// Reject invalid provided values directly (the getters clamp to defaults,
		// which would otherwise silently swallow a bad port/mode).
		if req.SMTPPort != nil && (*req.SMTPPort < 1 || *req.SMTPPort > 65535) {
			return SystemSettingsDTO{}, ErrSMTPPortInvalid
		}
		if req.SMTPTLSMode != nil && !isKnownSMTPTLSMode(strings.TrimSpace(*req.SMTPTLSMode)) {
			return SystemSettingsDTO{}, ErrSMTPTLSModeInvalid
		}
		// Build the merged effective map to validate the enable requirements.
		merged := make(map[string]string, len(values))
		for k, v := range values {
			merged[k] = v
		}
		if req.SMTPEnabled != nil {
			merged[smtpEnabledKey] = strconv.FormatBool(*req.SMTPEnabled)
		}
		if req.SMTPHost != nil {
			merged[smtpHostKey] = strings.TrimSpace(*req.SMTPHost)
		}
		if req.SMTPFrom != nil {
			// Normalize to the bare envelope address (addr@host) whenever the
			// input parses as a valid RFC 5322 address, regardless of whether
			// SMTP is enabled by this request. A "Name <addr@host>" input's
			// display name belongs in smtp_from_name — persisting the whole
			// string here would make the SMTP envelope MAIL FROM malformed. A
			// value that fails to parse is left as-is; the enabled-completeness
			// check below rejects it with ErrSMTPFromInvalid when SMTP is on.
			from := strings.TrimSpace(*req.SMTPFrom)
			if from != "" {
				if parsed, err := mail.ParseAddress(from); err == nil {
					from = parsed.Address
				}
			}
			merged[smtpFromKey] = from
		}
		if SMTPEnabled(merged) {
			if merged[smtpHostKey] == "" || merged[smtpFromKey] == "" {
				return SystemSettingsDTO{}, ErrSMTPConfigIncomplete
			}
			if _, err := mail.ParseAddress(merged[smtpFromKey]); err != nil {
				return SystemSettingsDTO{}, ErrSMTPFromInvalid
			}
		}
		// Seal the password up front so a disk-without-key rejection surfaces
		// before any write. nil = keep (no store call); "" = clear; else replace.
		var sealedPassword *string
		if req.SMTPPassword != nil {
			if *req.SMTPPassword == "" {
				empty := ""
				sealedPassword = &empty
			} else {
				sealed, err := s.sealSecret(*req.SMTPPassword)
				if err != nil {
					return SystemSettingsDTO{}, err
				}
				sealedPassword = &sealed
			}
		}
		// Queue the writes. Everything above has already validated, so — as
		// long as no write below is queued until this point, and the ones
		// above it in this function are queued but not yet persisted either —
		// there is no partial write: either every field here and above
		// applies, or (on any validation error) none of them do.
		if req.SMTPEnabled != nil {
			writes = append(writes, settingWrite{smtpEnabledKey, strconv.FormatBool(*req.SMTPEnabled)})
		}
		if req.SMTPHost != nil {
			writes = append(writes, settingWrite{smtpHostKey, strings.TrimSpace(*req.SMTPHost)})
		}
		if req.SMTPPort != nil {
			writes = append(writes, settingWrite{smtpPortKey, strconv.Itoa(*req.SMTPPort)})
		}
		if req.SMTPUsername != nil {
			writes = append(writes, settingWrite{smtpUsernameKey, strings.TrimSpace(*req.SMTPUsername)})
		}
		if req.SMTPFrom != nil {
			// merged[smtpFromKey] already holds the normalized (bare when
			// parseable) address computed above.
			writes = append(writes, settingWrite{smtpFromKey, merged[smtpFromKey]})
		}
		if req.SMTPFromName != nil {
			writes = append(writes, settingWrite{smtpFromNameKey, strings.TrimSpace(*req.SMTPFromName)})
		}
		if req.SMTPTLSMode != nil {
			writes = append(writes, settingWrite{smtpTLSModeKey, strings.TrimSpace(*req.SMTPTLSMode)})
		}
		if sealedPassword != nil {
			writes = append(writes, settingWrite{smtpPasswordKey, *sealedPassword})
		}
	}

	if req.touchesNetbird() {
		values, err := s.settings.SystemSettings(ctx)
		if err != nil {
			return SystemSettingsDTO{}, err
		}
		// Build the merged effective map to validate the enable requirements.
		merged := make(map[string]string, len(values))
		for k, v := range values {
			merged[k] = v
		}
		if req.NetbirdEnabled != nil {
			merged[netbirdEnabledKey] = strconv.FormatBool(*req.NetbirdEnabled)
		}
		if req.NetbirdURL != nil {
			merged[netbirdURLKey] = strings.TrimRight(strings.TrimSpace(*req.NetbirdURL), "/")
		}
		// The module group list is optional (validateNetbird ignores it), so it is
		// not merged for validation; it is JSON-encoded into the write list below.
		if req.NetbirdToken != nil {
			// Raw (pre-seal) value; validateNetbird reads only its presence, and
			// it is NEVER written to the store (the sealed value is).
			merged[netbirdTokenKey] = *req.NetbirdToken
		}
		if err := validateNetbird(merged); err != nil {
			return SystemSettingsDTO{}, err
		}
		// Seal the token up front so a disk-without-key rejection surfaces before
		// any write. nil = keep (no store call); "" = clear; else replace.
		var sealedToken *string
		if req.NetbirdToken != nil {
			if *req.NetbirdToken == "" {
				empty := ""
				sealedToken = &empty
			} else {
				sealed, err := s.sealSecret(*req.NetbirdToken)
				if err != nil {
					if errors.Is(err, ErrSMTPKeyRequired) {
						return SystemSettingsDTO{}, ErrNetbirdKeyRequired
					}
					return SystemSettingsDTO{}, err
				}
				sealedToken = &sealed
			}
		}
		if req.NetbirdEnabled != nil {
			writes = append(writes, settingWrite{netbirdEnabledKey, strconv.FormatBool(*req.NetbirdEnabled)})
		}
		if req.NetbirdURL != nil {
			writes = append(writes, settingWrite{netbirdURLKey, strings.TrimRight(strings.TrimSpace(*req.NetbirdURL), "/")})
		}
		if req.NetbirdGroups != nil {
			encoded, err := encodeNetbirdGroups(*req.NetbirdGroups)
			if err != nil {
				return SystemSettingsDTO{}, err
			}
			writes = append(writes, settingWrite{netbirdGroupKey, encoded})
		}
		if sealedToken != nil {
			writes = append(writes, settingWrite{netbirdTokenKey, *sealedToken})
		}
	}

	if req.touchesCert() {
		// Validate every cert_*/acme_* field before queuing ANY write in this
		// block (mirrors the SMTP/NetBird blocks above): a rejection here must
		// never leave a partial cert_* update applied. Unlike SMTP/NetBird there
		// is deliberately NO "enabled requires X" completeness gate here —
		// cert_enabled=true with no email/domain configured is allowed on
		// purpose (the chicken-and-egg fix: the certificates nav item must be
		// reachable before the module is configured); CertSettings reports
		// ok=false in that state instead of this write being rejected.
		if req.CertServerScope != nil {
			v := strings.TrimSpace(*req.CertServerScope)
			if v != "all" && v != "selected" {
				return SystemSettingsDTO{}, ErrCertInvalid
			}
		}
		if req.CertMeshTLSMode != nil {
			if v := strings.TrimSpace(*req.CertMeshTLSMode); v != "" && v != "combined" && v != "separate" {
				return SystemSettingsDTO{}, ErrCertInvalid
			}
		}
		if req.CertRenewBeforeDays != nil && *req.CertRenewBeforeDays < MinCertRenewBeforeDays {
			return SystemSettingsDTO{}, ErrCertInvalid
		}
		if req.ACMEDirectoryURL != nil {
			if raw := strings.TrimSpace(*req.ACMEDirectoryURL); raw != "" {
				u, err := url.Parse(raw)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
					return SystemSettingsDTO{}, ErrCertInvalid
				}
			}
		}
		if req.ACMEEmail != nil {
			if email := strings.TrimSpace(*req.ACMEEmail); email != "" && !strings.Contains(email, "@") {
				return SystemSettingsDTO{}, ErrCertInvalid
			}
		}
		if req.CertIssuerMode != nil && !isKnownCertIssuerMode(strings.TrimSpace(*req.CertIssuerMode)) {
			return SystemSettingsDTO{}, ErrCertInvalid
		}
		if req.CertSelfSignedValidityDays != nil &&
			(*req.CertSelfSignedValidityDays < MinSelfSignedValidityDays || *req.CertSelfSignedValidityDays > MaxSelfSignedValidityDays) {
			return SystemSettingsDTO{}, ErrCertInvalid
		}
		if req.CertCARenewBeforeDays != nil && *req.CertCARenewBeforeDays < MinCARenewBeforeDays {
			return SystemSettingsDTO{}, ErrCertInvalid
		}
		if req.CertEdgeIssuerMode != nil && !isKnownCertIssuerMode(strings.TrimSpace(*req.CertEdgeIssuerMode)) {
			return SystemSettingsDTO{}, ErrCertInvalid
		}
		// The three certificate DOMAIN settings are validated exactly like an edge
		// name, minus the IP branch. This closes a real pre-existing gap: their
		// setters only trim/lowercase, so a malformed value used to be stored and
		// then (a) became an HTTP-01 order identifier Let's Encrypt rejects --
		// burning a failed order every backoff cycle and pressuring the
		// failed-validation rate limit, plus leaving a junk row keyed by the payload
		// -- or, in self_signed mode, was signed into a leaf's SAN, and (b) is
		// interpolated into generated nginx configuration. An IP is a configuration
		// error for all three (a public CA cannot issue for one, and these names are
		// DNS names by construction). Empty stays legal -- all three are optional.
		for _, field := range []*string{req.CertBaseDomain, req.CertGatewayDomain} {
			if field == nil || strings.TrimSpace(*field) == "" {
				continue
			}
			if err := validateCertDomain(*field); err != nil {
				return SystemSettingsDTO{}, err
			}
		}
		if req.CertPublicDomains != nil {
			for _, d := range *req.CertPublicDomains {
				// An empty entry is dropped at encode time, never an error (mirrors
				// cert_edge_names).
				if strings.TrimSpace(d) == "" {
					continue
				}
				if err := validateCertDomain(d); err != nil {
					return SystemSettingsDTO{}, err
				}
			}
		}
		if req.CertEdgeNames != nil {
			for _, n := range *req.CertEdgeNames {
				// An empty entry is dropped at encode time (mirrors
				// cert_public_domains), never an error -- only a non-empty
				// entry that fails the name rules is.
				if strings.TrimSpace(n) == "" {
					continue
				}
				if err := ValidateEdgeName(n); err != nil {
					return SystemSettingsDTO{}, err
				}
			}
		}
		if req.CertEdgeIssuerMode != nil || req.CertEdgeNames != nil || req.CertEdgeACMEDirectoryURL != nil || req.CertEdgeACMEShared != nil {
			// acme cannot issue for a bare IP address (no domain to validate
			// via HTTP-01). Resolve the EFFECTIVE edge issuer mode and the
			// EFFECTIVE name list -- the value being written by this same PUT
			// when the field is set, otherwise the currently stored one -- so
			// a PUT that switches the mode to acme while an IP is already
			// stored (names untouched by this PUT), OR one that adds an IP
			// while acme is already the stored mode (mode untouched), is
			// rejected exactly like the combined single-write case.
			// req.CertEdgeACMEShared is in the trigger for the same reason: a
			// PUT that flips only cert_edge_acme_shared to false makes an
			// already-stored own directory LIVE, and its bare-IP host must be
			// re-validated at that moment, not slip through unchecked.
			edgeValues, err := s.settings.SystemSettings(ctx)
			if err != nil {
				return SystemSettingsDTO{}, err
			}
			edgeMode := CertEdgeIssuerMode(edgeValues)
			if req.CertEdgeIssuerMode != nil {
				edgeMode = strings.TrimSpace(*req.CertEdgeIssuerMode)
			}
			if edgeMode == IssuerModeACME {
				edgeNames := CertEdgeNames(edgeValues)
				if req.CertEdgeNames != nil {
					edgeNames = *req.CertEdgeNames
				}
				for _, n := range edgeNames {
					if net.ParseIP(strings.TrimSpace(n)) != nil {
						return SystemSettingsDTO{}, fmt.Errorf("%w: acme cannot issue for the IP address %s", ErrCertInvalid, n)
					}
				}
				// The edge's OWN ACME directory (meaningful ONLY when
				// cert_edge_acme_shared is false) is subject to the same rule as
				// the names above: a bare IP host cannot be validated the way
				// HTTP-01 needs. Resolved the same "effective" way -- and gated on
				// the EFFECTIVE shared flag: while the context is shared this
				// stored own directory is inert (issuance uses the global
				// acme_directory_url), so a bare-IP value there must not block a
				// save (final-review fix).
				edgeShared := CertEdgeACMEShared(edgeValues)
				if req.CertEdgeACMEShared != nil {
					edgeShared = *req.CertEdgeACMEShared
				}
				edgeDir := CertEdgeACMEDirectoryURL(edgeValues)
				if req.CertEdgeACMEDirectoryURL != nil {
					edgeDir = strings.TrimSpace(*req.CertEdgeACMEDirectoryURL)
				}
				if !edgeShared && edgeDir != "" && acmeDirectoryHostIsBareIP(edgeDir) {
					return SystemSettingsDTO{}, fmt.Errorf("%w: acme cannot use the IP address host of %s as its directory", ErrCertInvalid, edgeDir)
				}
			}
		}
		if req.CertPublicIssuerMode != nil || req.CertPublicACMEDirectoryURL != nil || req.CertIssuerMode != nil || req.CertPublicACMEShared != nil {
			// The public-domain analogue of the edge check above: a bare IP host
			// in cert_public_acme_directory_url cannot be validated the way
			// HTTP-01 needs, once the EFFECTIVE public issuer mode is acme.
			// "Effective" mode mirrors modeFor("public"): the value this PUT is
			// writing when set, else the stored one, else (when both are "")
			// falls back to the global cert_issuer_mode -- exactly like the
			// reconcile resolves it. req.CertIssuerMode is included in this
			// trigger (not just the body's fallback read) because a PUT that
			// changes ONLY the global mode -- leaving cert_public_issuer_mode
			// unset so public follows it -- must re-validate an ALREADY-STORED
			// bare-IP cert_public_acme_directory_url exactly as if it were being
			// written fresh: switching the effective public mode to acme is what
			// makes that stored value newly invalid, and skipping the check here
			// let it through unvalidated. req.CertPublicACMEShared is in the
			// trigger for the mirror reason: a PUT that flips only
			// cert_public_acme_shared to false makes an already-stored own
			// directory LIVE, so its bare-IP host must be re-validated then too.
			pubValues, err := s.settings.SystemSettings(ctx)
			if err != nil {
				return SystemSettingsDTO{}, err
			}
			pubMode := CertPublicIssuerMode(pubValues)
			if req.CertPublicIssuerMode != nil {
				pubMode = strings.TrimSpace(*req.CertPublicIssuerMode)
			}
			if pubMode == "" {
				globalMode := CertIssuerMode(pubValues)
				if req.CertIssuerMode != nil {
					globalMode = strings.TrimSpace(*req.CertIssuerMode)
				}
				pubMode = globalMode
			}
			if pubMode == IssuerModeACME {
				// Gated on the EFFECTIVE shared flag: while the public context is
				// shared, its stored own directory is inert (issuance uses the
				// global acme_directory_url), so an inert bare-IP value must not
				// block a save -- e.g. a PUT that only flips the global issuer
				// mode to acme, which the trigger above re-validates (final-review
				// fix).
				pubShared := CertPublicACMEShared(pubValues)
				if req.CertPublicACMEShared != nil {
					pubShared = *req.CertPublicACMEShared
				}
				pubDir := CertPublicACMEDirectoryURL(pubValues)
				if req.CertPublicACMEDirectoryURL != nil {
					pubDir = strings.TrimSpace(*req.CertPublicACMEDirectoryURL)
				}
				if !pubShared && pubDir != "" && acmeDirectoryHostIsBareIP(pubDir) {
					return SystemSettingsDTO{}, fmt.Errorf("%w: acme cannot use the IP address host of %s as its directory", ErrCertInvalid, pubDir)
				}
			}
		}
		if req.CertPublicIssuerMode != nil {
			if v := strings.TrimSpace(*req.CertPublicIssuerMode); v != "" && !isKnownCertIssuerMode(v) {
				return SystemSettingsDTO{}, ErrCertInvalid
			}
		}
		for _, n := range []*int{req.ACMEWeeklyLimit, req.CertEdgeACMEWeeklyLimit, req.CertPublicACMEWeeklyLimit} {
			if n != nil && *n < 0 {
				return SystemSettingsDTO{}, ErrCertInvalid
			}
		}
		for _, email := range []*string{req.CertEdgeACMEEmail, req.CertPublicACMEEmail} {
			if email != nil {
				if v := strings.TrimSpace(*email); v != "" && !strings.Contains(v, "@") {
					return SystemSettingsDTO{}, ErrCertInvalid
				}
			}
		}
		for _, dir := range []*string{req.CertEdgeACMEDirectoryURL, req.CertPublicACMEDirectoryURL} {
			if dir != nil {
				if raw := strings.TrimSpace(*dir); raw != "" {
					u, err := url.Parse(raw)
					if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
						return SystemSettingsDTO{}, ErrCertInvalid
					}
				}
			}
		}
		if req.CertHTTPSSwitchMode != nil {
			if v := strings.TrimSpace(*req.CertHTTPSSwitchMode); v != "" && v != "manual" && v != "auto" && v != "selected" {
				return SystemSettingsDTO{}, ErrCertInvalid
			}
		}
		if req.CertProxyListenPortBase != nil &&
			(*req.CertProxyListenPortBase < MinCertProxyListenPortBase || *req.CertProxyListenPortBase > MaxCertProxyListenPortBase) {
			return SystemSettingsDTO{}, ErrCertInvalid
		}

		// Everything above validated; only now queue the writes.
		if req.CertEnabled != nil {
			writes = append(writes, settingWrite{certEnabledKey, strconv.FormatBool(*req.CertEnabled)})
		}
		if req.CertIssuerMode != nil {
			writes = append(writes, settingWrite{certIssuerModeKey, strings.TrimSpace(*req.CertIssuerMode)})
		}
		if req.ACMEEmail != nil {
			writes = append(writes, settingWrite{acmeEmailKey, strings.TrimSpace(*req.ACMEEmail)})
		}
		if req.ACMEDirectoryURL != nil {
			writes = append(writes, settingWrite{acmeDirectoryURLKey, strings.TrimSpace(*req.ACMEDirectoryURL)})
		}
		if req.CertBaseDomain != nil {
			writes = append(writes, settingWrite{certBaseDomainKey, strings.ToLower(strings.TrimSpace(*req.CertBaseDomain))})
		}
		if req.CertGatewayDomain != nil {
			writes = append(writes, settingWrite{certGatewayDomainKey, strings.ToLower(strings.TrimSpace(*req.CertGatewayDomain))})
		}
		if req.CertServerScope != nil {
			writes = append(writes, settingWrite{certServerScopeKey, strings.TrimSpace(*req.CertServerScope)})
		}
		if req.CertManagePublicDomain != nil {
			writes = append(writes, settingWrite{certManagePublicDomainKey, strconv.FormatBool(*req.CertManagePublicDomain)})
		}
		if req.CertPublicDomains != nil {
			writes = append(writes, settingWrite{certPublicDomainsKey, encodeCertPublicDomains(*req.CertPublicDomains)})
		}
		if req.CertRenewBeforeDays != nil {
			writes = append(writes, settingWrite{certRenewBeforeDaysKey, strconv.Itoa(*req.CertRenewBeforeDays)})
		}
		if req.CertSelfSignedValidityDays != nil {
			writes = append(writes, settingWrite{certSelfSignedValidityDaysKey, strconv.Itoa(*req.CertSelfSignedValidityDays)})
		}
		if req.CertCARenewBeforeDays != nil {
			writes = append(writes, settingWrite{certCARenewBeforeDaysKey, strconv.Itoa(*req.CertCARenewBeforeDays)})
		}
		if req.CertEdgeEnabled != nil {
			writes = append(writes, settingWrite{certEdgeEnabledKey, strconv.FormatBool(*req.CertEdgeEnabled)})
		}
		if req.CertEdgeIssuerMode != nil {
			writes = append(writes, settingWrite{certEdgeIssuerModeKey, strings.TrimSpace(*req.CertEdgeIssuerMode)})
		}
		if req.CertEdgeNames != nil {
			writes = append(writes, settingWrite{certEdgeNamesKey, encodeCertEdgeNames(*req.CertEdgeNames)})
		}
		if req.CertEdgeRequireHTTPS != nil {
			// NOTE for a future second writer of this key: two things that belong to
			// switching the plaintext gate on do NOT live here, they live in the HTTP
			// handler (internal/gateway handleSystemSettings) because both need the
			// in-process observation tracker that lives in that package:
			//   1. the ARMING PRECONDITION -- (*gateway.Server).ArmEdgeRequireHTTPS,
			//      which refuses with 400 certificate.edge_https_not_observed unless
			//      encrypted traffic was actually seen. Skipping it lets an operator arm
			//      a total lockout of their own gateway on no evidence at all.
			//   2. the gate's TTL cache invalidation, without which the change takes up
			//      to edgeSchemeSwitchTTL to take effect -- worst in the DISARMING
			//      direction, where the operator is trying to get back in.
			// There is exactly one HTTP writer today, so this is not a defect; a second
			// one (a new endpoint, a CLI, a bulk importer) must call both, or move them
			// in here.
			writes = append(writes, settingWrite{certEdgeRequireHTTPSKey, strconv.FormatBool(*req.CertEdgeRequireHTTPS)})
		}
		if req.CertMeshRequireTLS != nil {
			// Like cert_edge_require_https, the ARMING PRECONDITION (recent TLS observed
			// on the mesh listener) and the gate's cache invalidation live in the HTTP
			// handler (internal/gateway), because both need the in-process
			// AgentTransportRegistry that lives in that package. A future second writer
			// of this key must call both or move them here.
			writes = append(writes, settingWrite{certMeshRequireTLSKey, strconv.FormatBool(*req.CertMeshRequireTLS)})
		}
		if req.CertMeshTLSMode != nil {
			writes = append(writes, settingWrite{certMeshTLSModeKey, strings.TrimSpace(*req.CertMeshTLSMode)})
		}
		if req.CertPublicIssuerMode != nil {
			writes = append(writes, settingWrite{certPublicIssuerModeKey, strings.TrimSpace(*req.CertPublicIssuerMode)})
		}
		if req.ACMEWeeklyLimit != nil {
			writes = append(writes, settingWrite{acmeWeeklyLimitKey, strconv.Itoa(*req.ACMEWeeklyLimit)})
		}
		if req.CertEdgeACMEShared != nil {
			writes = append(writes, settingWrite{certEdgeACMESharedKey, strconv.FormatBool(*req.CertEdgeACMEShared)})
		}
		if req.CertEdgeACMEEmail != nil {
			writes = append(writes, settingWrite{certEdgeACMEEmailKey, strings.TrimSpace(*req.CertEdgeACMEEmail)})
		}
		if req.CertEdgeACMEDirectoryURL != nil {
			writes = append(writes, settingWrite{certEdgeACMEDirectoryURLKey, strings.TrimSpace(*req.CertEdgeACMEDirectoryURL)})
		}
		if req.CertEdgeACMEWeeklyLimit != nil {
			writes = append(writes, settingWrite{certEdgeACMEWeeklyLimitKey, strconv.Itoa(*req.CertEdgeACMEWeeklyLimit)})
		}
		if req.CertPublicACMEShared != nil {
			writes = append(writes, settingWrite{certPublicACMESharedKey, strconv.FormatBool(*req.CertPublicACMEShared)})
		}
		if req.CertPublicACMEEmail != nil {
			writes = append(writes, settingWrite{certPublicACMEEmailKey, strings.TrimSpace(*req.CertPublicACMEEmail)})
		}
		if req.CertPublicACMEDirectoryURL != nil {
			writes = append(writes, settingWrite{certPublicACMEDirectoryURLKey, strings.TrimSpace(*req.CertPublicACMEDirectoryURL)})
		}
		if req.CertPublicACMEWeeklyLimit != nil {
			writes = append(writes, settingWrite{certPublicACMEWeeklyLimitKey, strconv.Itoa(*req.CertPublicACMEWeeklyLimit)})
		}
		if req.CertHTTPSSwitchMode != nil {
			writes = append(writes, settingWrite{certHTTPSSwitchModeKey, strings.TrimSpace(*req.CertHTTPSSwitchMode)})
		}
		if req.CertProxyListenPortBase != nil {
			writes = append(writes, settingWrite{certProxyListenPortBaseKey, strconv.Itoa(*req.CertProxyListenPortBase)})
		}
	}

	// Everything validated above; only now do any of the fields — SMTP or
	// not — actually reach the store, so a rejection from any block leaves
	// the prior state fully intact.
	for _, w := range writes {
		if err := s.settings.SetSystemSetting(ctx, w.key, w.value, s.clock()); err != nil {
			return SystemSettingsDTO{}, err
		}
	}
	// A NetBird token was just (re)stored. Keep netbird_token_id/_expires_at in sync:
	// clearing the token clears them synchronously; a new non-empty token is resolved
	// best-effort (single-token users get instant expiry display + auto-rotation).
	if req.NetbirdToken != nil {
		if *req.NetbirdToken == "" {
			_ = s.settings.SetSystemSetting(ctx, netbirdTokenIDKey, "", s.clock())
			_ = s.settings.SetSystemSetting(ctx, netbirdTokenExpiresAtKey, "", s.clock())
		} else {
			s.netbird.resolveWG.Add(1)
			go func() {
				defer s.netbird.resolveWG.Done()
				s.resolveStoredTokenMeta(context.Background())
			}()
		}
	}
	// A policy-relevant field just changed (manage on/off, scope, or deny-by-
	// default) -> re-derive the whole fleet's access policies (+ the Default
	// catch-all when the deny toggle was in this request) in the background so
	// the settings PUT itself does not block on NetBird calls. Best-effort: both
	// helpers gate internally and never error.
	if req.NetbirdManagePolicies != nil || req.NetbirdPolicyScope != nil || req.NetbirdDenyByDefault != nil ||
		req.NetbirdAllowPingGateway != nil || req.NetbirdAllowPingAllServers != nil {
		s.netbird.policySideEffectWG.Add(1)
		denyReq := req.NetbirdDenyByDefault
		go func() {
			defer s.netbird.policySideEffectWG.Done()
			s.applyPolicySettingsSideEffects(context.Background(), denyReq)
		}()
	}
	// A certificate-relevant field just changed -> ask for an immediate reconcile
	// pass instead of leaving the operator to wait out the periodic loop (default
	// 900s). Gated on touchesCert because this PUT is shared with ~30 unrelated
	// settings; a theme change must not kick the certificate subsystem.
	//
	// This deliberately does NOT touch cert_last_error itself: the pass stays the
	// SINGLE writer of that value (see clearCertLastError). Clearing it here would
	// show "all fine" for the whole duration of the pass even when the
	// configuration is still broken -- and if the trigger never lands, it would
	// show "all fine" forever. So the note is only ever corrected by a pass that
	// actually got past ReconcileCertificates' abort gates.
	//
	// Best-effort in both directions: a nil hook is a no-op (the periodic loop is
	// the backstop), and the hook itself must not block -- see
	// ServiceDeps.OnCertSettingsChanged for that contract. Placed BEFORE the
	// gateway-peer reconcile below so a NetBird round trip cannot delay it.
	if req.touchesCert() && s.cert.onSettingsChanged != nil {
		s.cert.onSettingsChanged()
	}
	// The gateway-peer selection or desired name just changed -> apply it to NetBird
	// NOW (synchronous, best-effort) so the live status the client refetches after
	// this save reflects the rename/selection immediately, instead of waiting up to
	// one reconcile-loop interval (the cause of the "UI shows the old peer name after
	// save" bug). A NetBird error never fails the save — the periodic loop is the
	// backstop.
	if req.NetbirdGatewayPeerID != nil || req.NetbirdGatewayPeerName != nil {
		if _, _, rErr := s.ReconcileGatewayPeer(ctx); rErr != nil {
			slog.Debug("gateway-peer reconcile after settings save failed", "err", rErr)
		}
	}
	return s.SystemSettingsView(ctx), nil
}

// applyPolicySettingsSideEffects runs the best-effort NetBird side effects of a
// policy-relevant settings change: the deny-by-default "Default" apply (only when
// the deny toggle was in the request) + a full policy fleet reconcile. Synchronous;
// callers that must not block the request should invoke it in a goroutine.
func (s *Service) applyPolicySettingsSideEffects(ctx context.Context, denyReq *bool) {
	if denyReq != nil {
		if cfg, ok, err := s.NetbirdConfig(ctx); err == nil && ok {
			s.applyDenyByDefault(ctx, netbird.Config{URL: cfg.URL, Token: cfg.Token}, *denyReq)
		}
	}
	s.reconcileAllServerPolicies(ctx)
}

// plainPrefix marks a volatile, unsealed secret value (see sealSecret/
// openSecret/sealCertSecret/openCertSecret).
const plainPrefix = "plain:"

// sealSecret encodes an SMTP password for storage. With a cipher it seals to
// "enc:"+base64; on the volatile in-memory store (no cipher) it stores
// "plain:"+raw (never written to disk, gone on process exit — same rationale as
// the RAM capture fallback); on a disk store without a cipher it refuses with
// ErrSMTPKeyRequired rather than persist plaintext.
func (s *Service) sealSecret(plain string) (string, error) {
	if s.cipher != nil {
		return "enc:" + base64.StdEncoding.EncodeToString(s.cipher.Seal([]byte(plain))), nil
	}
	if s.settingsVolatile {
		return plainPrefix + plain, nil
	}
	return "", ErrSMTPKeyRequired
}

// openSecret reverses sealSecret. An empty value (no password stored) opens to
// "". An "enc:" value requires the cipher (ErrSMTPKeyRequired if the key was
// removed after sealing); a "plain:" value returns the raw password. Any other
// shape is treated as missing key rather than leaking a corrupt value.
func (s *Service) openSecret(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if strings.HasPrefix(stored, plainPrefix) {
		return strings.TrimPrefix(stored, plainPrefix), nil
	}
	if strings.HasPrefix(stored, "enc:") {
		if s.cipher == nil {
			return "", ErrSMTPKeyRequired
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, "enc:"))
		if err != nil {
			return "", err
		}
		plain, err := s.cipher.Open(raw)
		if err != nil {
			return "", err
		}
		return string(plain), nil
	}
	return "", ErrSMTPKeyRequired
}

// sealCertSecret encodes a CERTIFICATE private key (a leaf key, the ACME account
// key or the internal CA key) for storage. It follows sealSecret's rules
// exactly -- "enc:"+base64 with a cipher; "plain:"+raw only on the volatile
// in-memory store (never written to disk, gone on process exit); refuse rather
// than persist plaintext on a disk store without a key -- but reads the
// CERTIFICATE cipher (s.cert.cipher, from OP_AI_GATEWAY_CERT_ENCRYPTION_KEY)
// instead of the capture cipher, and refuses with ErrCertKeyRequired so the
// operator is pointed at the right variable.
//
// There is deliberately NO fallback to s.cipher: certificate material must be
// readable with the certificate key alone.
func (s *Service) sealCertSecret(plain string) (string, error) {
	if s.cert.cipher != nil {
		return "enc:" + base64.StdEncoding.EncodeToString(s.cert.cipher.Seal([]byte(plain))), nil
	}
	if s.settingsVolatile {
		return plainPrefix + plain, nil
	}
	return "", ErrCertKeyRequired
}

// openCertSecret reverses sealCertSecret. Mirrors openSecret's shapes ("" -> "",
// "plain:" raw, "enc:" via the cipher, anything else treated as missing key
// rather than leaking a corrupt value) against s.cert.cipher, and reports
// ErrCertKeyRequired when the certificate key is absent (e.g. it was removed
// after sealing).
//
// A value sealed with a DIFFERENT key fails inside capture.Cipher.Open. Every
// certificate secret is regenerable, and each caller recovers by regenerating
// rather than by retrying the same bytes -- accountFor registers a fresh
// account, ensureCA mints a new root, and a leaf is simply re-issued -- so a key
// rotation degrades to new material instead of wedging the module.
//
// The decrypt failure is WRAPPED: capture.Cipher's own error text starts with
// "capture: ", which would send an operator hunting in the capture key. This
// value is governed by OP_AI_GATEWAY_CERT_ENCRYPTION_KEY, so the message names
// that variable. The wrapped text carries no key or plaintext material (a MAC
// failure only), and %w keeps errors.Is intact for callers.
func (s *Service) openCertSecret(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if strings.HasPrefix(stored, plainPrefix) {
		return strings.TrimPrefix(stored, plainPrefix), nil
	}
	if strings.HasPrefix(stored, "enc:") {
		if s.cert.cipher == nil {
			return "", ErrCertKeyRequired
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, "enc:"))
		if err != nil {
			return "", err
		}
		plain, err := s.cert.cipher.Open(raw)
		if err != nil {
			return "", fmt.Errorf("portal: this certificate secret cannot be decrypted with the "+
				"configured OP_AI_GATEWAY_CERT_ENCRYPTION_KEY (was that key changed?): %w", err)
		}
		return string(plain), nil
	}
	return "", ErrCertKeyRequired
}

// SMTPRuntimeConfig is the fully-resolved SMTP configuration with the password
// decrypted, for the invite-email/test-email send path. It never leaves the
// backend.
type SMTPRuntimeConfig struct {
	Enabled  bool
	Host     string
	Port     int
	Username string
	Password string
	From     string
	FromName string
	TLSMode  string
}

// SMTPRuntimeConfig reads the persisted SMTP settings and opens the stored
// password. A missing settings store or a decryption failure returns an error;
// an empty/absent password opens to "".
func (s *Service) SMTPRuntimeConfig(ctx context.Context) (SMTPRuntimeConfig, error) {
	if s.settings == nil {
		return SMTPRuntimeConfig{Port: DefaultSMTPPort, TLSMode: DefaultSMTPTLSMode}, nil
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return SMTPRuntimeConfig{}, err
	}
	password, err := s.openSecret(values[smtpPasswordKey])
	if err != nil {
		return SMTPRuntimeConfig{}, err
	}
	return SMTPRuntimeConfig{
		Enabled:  SMTPEnabled(values),
		Host:     values[smtpHostKey],
		Port:     SMTPPort(values),
		Username: values[smtpUsernameKey],
		Password: password,
		From:     values[smtpFromKey],
		FromName: values[smtpFromNameKey],
		TLSMode:  SMTPTLSMode(values),
	}, nil
}

// validateNetbird enforces the enable requirements on the merged effective
// settings: netbird_enabled may be turned on before the url/token are
// configured (the module is then "on" but not yet usable — NetbirdConfig's ok
// stays false until both are present, so nothing downstream treats it as
// configured). A NON-EMPTY netbird_url must still be a valid absolute http(s)
// URL (ErrNetbirdURLInvalid). Disabled ⇒ no requirements.
func validateNetbird(effective map[string]string) error {
	if !NetbirdEnabled(effective) {
		return nil
	}
	raw := NetbirdURL(effective)
	if raw == "" {
		return nil // enabled but not yet configured — allowed; NetbirdConfig ok stays false until url+token
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ErrNetbirdURLInvalid
	}
	return nil
}

// NetbirdConfig is the fully-resolved NetBird configuration with the token
// decrypted, for the client/create/sync paths. It never leaves the backend.
type NetbirdConfig struct {
	URL    string
	Token  string
	Groups []string
}

// NetbirdConfig reads the persisted NetBird settings and opens the stored token.
// ok is false when the module is disabled or the url/token is missing; a
// decryption failure returns an error.
func (s *Service) NetbirdConfig(ctx context.Context) (cfg NetbirdConfig, ok bool, err error) {
	if s.settings == nil {
		return NetbirdConfig{}, false, nil
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return NetbirdConfig{}, false, err
	}
	if !NetbirdEnabled(values) {
		return NetbirdConfig{}, false, nil
	}
	token, err := s.openSecret(values[netbirdTokenKey])
	if err != nil {
		return NetbirdConfig{}, false, err
	}
	cfg = NetbirdConfig{URL: NetbirdURL(values), Token: token, Groups: NetbirdGroups(values)}
	if cfg.URL == "" || cfg.Token == "" {
		return cfg, false, nil
	}
	return cfg, true, nil
}

// NetbirdModuleEnabled is a cheap check for the create-hook + frontend gate: the
// module is enabled AND the url/token are configured.
func (s *Service) NetbirdModuleEnabled(ctx context.Context) bool {
	_, ok, _ := s.NetbirdConfig(ctx)
	return ok
}

// NetbirdModuleChecked reports the RAW netbird_enabled checkbox state (regardless of
// whether url/token are set), for the portal nav gate. Nil-safe → false.
func (s *Service) NetbirdModuleChecked(ctx context.Context) bool {
	if s.settings == nil {
		return false
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return false
	}
	return NetbirdEnabled(values)
}

// NetbirdPolicySettings is a one-shot snapshot of the policy/interval settings
// for the reconcile engine + the two sync loops (later tasks) to consume
// without repeating the getter dance at each call site.
type NetbirdPolicySettings struct {
	ManagePolicies       bool
	Scope                string // raw stored value ("auto"/"all"/"selected", lenient)
	EffectiveScope       string // resolved: "all" or "selected"
	DenyByDefault        bool
	DenyByDefaultEnforce bool
	AllowPingGateway     bool
	AllowPingAllServers  bool
	PeerSyncInterval     time.Duration
	ReconcileInterval    time.Duration
}

// NetbirdPolicySettings reads the policy/interval settings in one shot. It
// never errors — a missing store or a read failure yields the documented
// defaults (fail-open; a settings glitch must never stall the reconcile
// loops). peerEnvFallback is the legacy env-configured peer-sync interval (in
// seconds, e.g. OP_AI_GATEWAY_NETBIRD_SYNC_INTERVAL_SECONDS); it is used ONLY
// when the netbird_peer_sync_interval_seconds KV is unset/blank (so an
// existing deployment keeps its env-configured cadence until the operator
// sets the setting explicitly in the UI) and only when it meets the 10s
// floor. The reconcile interval is defensively clamped to be >= the
// (possibly env-derived) peer interval, preserving the A<=B invariant even
// against a stale/out-of-band KV state.
//
// KNOWN LIMITATION (documented, safe direction): UpdateSystemSettings enforces
// A<=B only against the STORED reconcile value, which does not know about
// peerEnvFallback. So the UI/DTO can show a reconcile value that is, at
// runtime, silently clamped UP to a larger env-derived peer interval here — the
// two loops still never violate A<=B at runtime, but the displayed reconcile
// number can undersell the actual cadence until the operator sets
// netbird_reconcile_interval_seconds explicitly (or the peer KV, retiring the
// env fallback). The backstop only ever widens the interval (runs no more
// often than shown), never narrows it, so this is safe by construction.
func (s *Service) NetbirdPolicySettings(ctx context.Context, peerEnvFallback int) NetbirdPolicySettings {
	var values map[string]string
	if s.settings != nil {
		values, _ = s.settings.SystemSettings(ctx)
	}
	deny := NetbirdDenyByDefault(values)
	scope := NetbirdPolicyScope(values)
	peer := NetbirdPeerSyncIntervalSeconds(values)
	if strings.TrimSpace(values[netbirdPeerSyncIntervalKey]) == "" && peerEnvFallback >= MinNetbirdIntervalSeconds {
		peer = peerEnvFallback
	}
	reconcile := NetbirdReconcileIntervalSeconds(values)
	if reconcile < peer {
		slog.Debug("netbird policy settings: reconcile interval clamped up to peer interval",
			"peer_seconds", peer, "reconcile_kv_seconds", reconcile)
		reconcile = peer
	}
	return NetbirdPolicySettings{
		ManagePolicies:       NetbirdManagePolicies(values),
		Scope:                scope,
		EffectiveScope:       EffectiveNetbirdPolicyScope(scope, deny),
		DenyByDefault:        deny,
		DenyByDefaultEnforce: NetbirdDenyByDefaultEnforce(values),
		AllowPingGateway:     NetbirdAllowPingGateway(values),
		AllowPingAllServers:  NetbirdAllowPingAllServers(values),
		PeerSyncInterval:     time.Duration(peer) * time.Second,
		ReconcileInterval:    time.Duration(reconcile) * time.Second,
	}
}

// -----------------------------------------------------------------------
// Certificate management (cert_*/acme_*) — system settings.
//
// This is data-model-only: the keys, readers, DTO/request fields, and
// validation live here; the reconcile that actually issues/rotates
// certificates is a later task. No consumer reads these yet, so an
// unconfigured deployment (every key absent) behaves exactly as before
// this task landed.
// -----------------------------------------------------------------------

const (
	certEnabledKey            = "cert_enabled"
	acmeEmailKey              = "acme_email"
	acmeDirectoryURLKey       = "acme_directory_url"
	certBaseDomainKey         = "cert_base_domain"
	certGatewayDomainKey      = "cert_gateway_domain"
	certServerScopeKey        = "cert_server_scope"
	certManagePublicDomainKey = "cert_manage_public_domain"
	certPublicDomainsKey      = "cert_public_domains"
	certRenewBeforeDaysKey    = "cert_renew_before_days"

	// acmeAccountKeyKey/acmeAccountURIKey/acmeAccountDirectoryKey are
	// runtime-managed: written by the reconcile (a later task), never by the
	// settings form. They hold the sealed ACME account private key, its
	// account URI, and the directory URL it was registered against — the
	// directory is tracked so a directory switch (e.g. staging -> production)
	// never reuses an account registered with a foreign directory.
	acmeAccountKeyKey       = "acme_account_key"
	acmeAccountURIKey       = "acme_account_uri"
	acmeAccountDirectoryKey = "acme_account_directory"

	// certCACertKey/certCAKeySealedKey/certCAPrevCertKey are runtime-managed
	// (self_signed mode): the current internal-CA root certificate (public,
	// PEM), its sealed private key, and the PREVIOUS root (public only, PEM) —
	// kept around until it expires so a client that has only cached the old
	// root doesn't break mid-rotation. None of the three is settable through
	// UpdateSystemSettingsRequest; only the reconcile/rotation action (a later
	// task) writes them.
	certCACertKey      = "cert_ca_cert_pem"
	certCAKeySealedKey = "cert_ca_key_sealed"
	certCAPrevCertKey  = "cert_ca_prev_cert_pem"

	// certCARotatedAtKey is runtime-managed exactly like the three cert_ca_*
	// keys above (never settable through UpdateSystemSettingsRequest): the
	// RFC3339 timestamp of the most recent newCA write. The Phase 2
	// CA-rotation propagation brake (see caRotationPropagated in
	// service_certificates.go) needs it to answer "how long have we been
	// waiting for the new root to reach the agents?" -- without it the brake
	// could not distinguish a rotation seconds ago from one a week ago, and
	// therefore could not time out. newCA writes it FIRST, before the root
	// itself becomes referenceable: a timestamp without a rotation is inert
	// (the stored root is unchanged, so every agent already reports it and
	// nothing is held back), whereas a rotation without a timestamp would
	// silently disable the brake.
	certCARotatedAtKey = "cert_ca_rotated_at"

	// certLastErrorKey is ALSO runtime-managed, exactly like the acme_account_*/
	// cert_ca_* keys above: it is written by ReconcileCertificates, never by the
	// settings form (it is deliberately absent from UpdateSystemSettingsRequest).
	// It records why the MOST RECENT reconcile pass gave up before it could place
	// or renew a single order -- e.g. a disk store has no encryption key to seal
	// the internal CA's private key with, or no base domain could be resolved --
	// so that state is no longer indistinguishable from "a fresh install that has
	// not reconciled yet" (review finding F1.1). Cleared (written "") once a
	// later pass gets past both abort gates; never contains key/PEM material --
	// see certReconcileAbortMessage, which renders an error into this string.
	certLastErrorKey = "cert_last_error"

	certIssuerModeKey             = "cert_issuer_mode"
	certSelfSignedValidityDaysKey = "cert_self_signed_validity_days"
	certCARenewBeforeDaysKey      = "cert_ca_renew_before_days"

	// certEdgeEnabledKey/certEdgeIssuerModeKey/certEdgeNamesKey configure the
	// gateway's OWN edge (nginx) certificate -- a distinct issuance target from
	// the internal/mesh certificates the rest of this block manages, with its
	// OWN issuer mode switchable independently of cert_issuer_mode. Task 4
	// (a later task) wires the reconcile pass that actually consumes them;
	// here they are pure data-model, like the rest of this file's cert_*
	// keys were before the reconcile landed.
	certEdgeEnabledKey    = "cert_edge_enabled"
	certEdgeIssuerModeKey = "cert_edge_issuer_mode"
	// certEdgeNamesKey is deliberately NOT named "domains": entries may be a
	// bare IP address (see ValidateEdgeName) in addition to a DNS name, which
	// a "domain" is not.
	certEdgeNamesKey = "cert_edge_names"

	// certEdgeRequireHTTPSKey is plan B's plaintext-refusal switch (a later
	// task's gate, in internal/gateway, is the consumer). Unlike the three
	// keys above it has no "on but not yet usable" state to gate against here
	// -- the arming precondition (recent encrypted traffic observed) is a
	// runtime signal that setting store's key/value model cannot express,
	// so it lives entirely in the gate, not in this file.
	certEdgeRequireHTTPSKey = "cert_edge_require_https"
	// certMeshRequireTLSKey is P3's plaintext-refusal switch for the mesh agent
	// listener. Like cert_edge_require_https it is NOT gated by cert_enabled (an
	// operator may decline the gateway's issuance and still require TLS on the mesh
	// hop), so its only reader is Service.CertMeshRequireTLSChecked.
	certMeshRequireTLSKey = "cert_mesh_require_tls"
	// certMeshTLSModeKey is the stored agent-listener TLS-port topology switch:
	// "combined" or "separate", or absent/blank/unknown meaning "follow
	// config.Config.AgentTLSSeparate" -- see CertMeshTLSMode (the reader) and
	// Service.CertMeshTLSSeparateActive (the effective resolver).
	certMeshTLSModeKey = "cert_mesh_tls_mode"

	// certPublicIssuerModeKey is the public-domain issuer mode, switchable
	// independently of cert_issuer_mode -- mirrors cert_edge_issuer_mode, except
	// EMPTY is itself a meaningful (and the default) value: "follow whatever
	// cert_issuer_mode currently says" (see CertSettings.modeFor), so a
	// deployment that never sets this key keeps behaving exactly as it did
	// before this key existed.
	certPublicIssuerModeKey = "cert_public_issuer_mode"

	// acmeWeeklyLimitKey is the per-week issuance ceiling for the GLOBAL (shared)
	// ACME account -- i.e. the account selected by acme_directory_url/acme_email.
	// 0 (absent) means "no limit set here"; nothing in this task enforces it yet,
	// a later task is its first consumer.
	acmeWeeklyLimitKey = "acme_weekly_limit"

	// certEdgeACMESharedKey/certPublicACMESharedKey: true (the default -- absent
	// means shared, so an upgraded deployment keeps using the ONE global ACME
	// account exactly as before this task) means that context re-uses the
	// global ACME account (acme_email/acme_directory_url/acme_weekly_limit);
	// false means it registers and uses its OWN account via the sibling
	// *_acme_email/*_acme_directory_url/*_acme_weekly_limit keys below. See
	// CertSettings.certAcmeConfigFor, the resolver issueCertificate consumes.
	certEdgeACMESharedKey       = "cert_edge_acme_shared"
	certEdgeACMEEmailKey        = "cert_edge_acme_email"
	certEdgeACMEDirectoryURLKey = "cert_edge_acme_directory_url"
	certEdgeACMEWeeklyLimitKey  = "cert_edge_acme_weekly_limit"

	certPublicACMESharedKey       = "cert_public_acme_shared"
	certPublicACMEEmailKey        = "cert_public_acme_email"
	certPublicACMEDirectoryURLKey = "cert_public_acme_directory_url"
	certPublicACMEWeeklyLimitKey  = "cert_public_acme_weekly_limit"

	// certHTTPSSwitchModeKey is P4's global https-auto-switch mode: "manual"
	// (default), "auto", or "selected" -- see CertHTTPSSwitchMode (the reader)
	// and httpsSwitchInScope (the per-server resolver).
	certHTTPSSwitchModeKey = "cert_https_switch_mode"
	// certProxyListenPortBaseKey is the auto-assign floor for
	// Application.ProxyListenPort -- see CertProxyListenPortBase (the reader).
	certProxyListenPortBaseKey = "cert_proxy_listen_port_base"
)

// acmeAccountKeysFor returns the per-directory KV key names for an ACME account.
// The GLOBAL directory (acme_directory_url) deliberately keeps the legacy
// unprefixed slot so a pre-unification gateway's registered account is adopted
// verbatim on upgrade (no re-registration). Any other directory gets a stable
// hash-suffixed slot.
func acmeAccountKeysFor(directory, globalDirectory string) (keyK, uriK, dirK string) {
	if strings.TrimSpace(directory) == strings.TrimSpace(globalDirectory) {
		return acmeAccountKeyKey, acmeAccountURIKey, acmeAccountDirectoryKey
	}
	h := sha256.Sum256([]byte(strings.TrimSpace(directory)))
	suffix := hex.EncodeToString(h[:8])
	return "acme_account_" + suffix + "_key",
		"acme_account_" + suffix + "_uri",
		"acme_account_" + suffix + "_directory"
}

// DefaultACMEDirectoryURL is Let's Encrypt's production ACME directory. The
// rate-limit-free staging directory
// (https://acme-staging-v02.api.letsencrypt.org/directory) is offered in the
// UI for trying a setup without burning production quota.
const DefaultACMEDirectoryURL = "https://acme-v02.api.letsencrypt.org/directory"

// DefaultCertServerScope/DefaultCertRenewBeforeDays/MinCertRenewBeforeDays are
// the defaults/floor for the certificate-management server scope and renewal
// lead time. Reads never fail (an out-of-range stored value falls back to the
// default); writes reject an out-of-range value outright (ErrCertInvalid)
// rather than silently clamping.
const (
	DefaultCertServerScope     = "selected"
	DefaultCertRenewBeforeDays = 30
	MinCertRenewBeforeDays     = 7
)

// Issuer modes: "acme" issues publicly trusted certificates from Let's
// Encrypt (or any RFC 8555 CA) over HTTP-01; "self_signed" issues from the
// gateway's own internal CA, for deployments without public DNS / port 80.
const (
	IssuerModeACME       = "acme"
	IssuerModeSelfSigned = "self_signed"
	// DefaultCertIssuerMode is used when cert_issuer_mode is unset, blank, or
	// not one of the two known modes.
	DefaultCertIssuerMode = IssuerModeACME
)

// DefaultSelfSignedValidityDays/MinSelfSignedValidityDays/
// MaxSelfSignedValidityDays bound the self-signed LEAF certificate lifetime.
// DefaultCARenewBeforeDays/MinCARenewBeforeDays bound how many days before its
// own expiry the internal CA root itself is rotated — that window must leave
// enough time for the new root to propagate to every server before the old
// one expires.
const (
	DefaultSelfSignedValidityDays = 365
	MinSelfSignedValidityDays     = 1
	MaxSelfSignedValidityDays     = 3650
	DefaultCARenewBeforeDays      = 365
	MinCARenewBeforeDays          = 30
)

// DefaultCertHTTPSSwitchMode is P4's global https-auto-switch mode default:
// "manual" (byte-neutral -- the gateway changes no app scheme until an
// operator opts in). DefaultCertProxyListenPortBase/MinCertProxyListenPortBase/
// MaxCertProxyListenPortBase bound the auto-assign floor for
// Application.ProxyListenPort. Reads never fail (an out-of-range stored value
// falls back to the default); writes reject an out-of-range value outright
// (ErrCertInvalid) rather than silently clamping.
const (
	DefaultCertHTTPSSwitchMode     = "manual"
	DefaultCertProxyListenPortBase = 8600
	MinCertProxyListenPortBase     = 1024
	MaxCertProxyListenPortBase     = 65535
)

// ErrCertInvalid marks a rejected cert_*/acme_* settings write (HTTP 400).
// Unlike the NetBird block (which has one sentinel per field), every
// certificate-settings validation failure maps to this single sentinel — the
// per-field detail lives in the returned message context at the HTTP layer,
// not in a proliferation of near-identical Err vars.
var ErrCertInvalid = errors.New("portal: invalid acme settings")

// knownCertIssuerModes is the registry of valid cert_issuer_mode values.
func knownCertIssuerModes() []string { return []string{IssuerModeACME, IssuerModeSelfSigned} }

func isKnownCertIssuerMode(mode string) bool {
	for _, m := range knownCertIssuerModes() {
		if m == mode {
			return true
		}
	}
	return false
}

// ValidateEdgeName accepts an IP address or a strict DNS name and nothing
// else. This is a SECURITY check, not cosmetics: these values are
// interpolated server-side into an nginx configuration that the operator
// pastes onto the UPSTREAM reverse proxy -- a machine that also terminates
// many foreign domains. A value containing ';', '}', '#' or a newline would
// inject arbitrary directives there. The existing CSV settings
// (cert_public_domains) only TrimSpace/ToLower, so they are NOT a precedent
// for what is safe to interpolate here -- this validator is intentionally
// stricter. Errors are wrapped in the existing ErrCertInvalid sentinel (not a
// new one) so the HTTP layer keeps mapping them to 400 exactly like every
// other cert_*/acme_* validation failure.
func ValidateEdgeName(name string) error {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return fmt.Errorf("%w: empty name", ErrCertInvalid)
	}
	if net.ParseIP(n) != nil {
		return nil
	}
	if len(n) > 253 {
		return fmt.Errorf("%w: name too long", ErrCertInvalid)
	}
	for _, label := range strings.Split(n, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("%w: bad label in %q", ErrCertInvalid, n)
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("%w: label may not start or end with '-' in %q", ErrCertInvalid, n)
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-'
			if !ok {
				return fmt.Errorf("%w: illegal character in %q", ErrCertInvalid, n)
			}
		}
	}
	return nil
}

// validateCertDomain is ValidateEdgeName MINUS the IP branch, for the three
// certificate DOMAIN settings (cert_base_domain, cert_gateway_domain and each
// cert_public_domains entry). They differ from cert_edge_names in exactly one way:
// an edge certificate may legitimately carry a bare IP SAN (issued by the internal
// CA for a mesh address), while these three are DNS names by construction -- an IP
// in any of them is a configuration error, not an alternative spelling. Everything
// else (the strict label rules, the ErrCertInvalid wrapping that the HTTP layer
// maps to 400) is delegated so the two validators cannot drift.
func validateCertDomain(name string) error {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return fmt.Errorf("%w: empty name", ErrCertInvalid)
	}
	if net.ParseIP(n) != nil {
		return fmt.Errorf("%w: %q is an IP address; this setting takes a DNS name", ErrCertInvalid, n)
	}
	return ValidateEdgeName(n)
}

// acmeDirectoryHostIsBareIP reports whether rawURL parses and its host is a
// bare IP address literal. Shared by the edge and public-domain "acme cannot
// use a bare-IP directory host" checks in UpdateSystemSettings -- mirrors
// the rationale of ValidateEdgeName's IP branch (HTTP-01 has no way to
// validate an IP), applied to an ACME directory URL's host instead of a SAN
// name. An unparseable rawURL is reported false here -- the caller has
// already validated the URL shape separately, so this helper only answers
// the IP question for an otherwise-well-formed value.
func acmeDirectoryHostIsBareIP(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return net.ParseIP(u.Hostname()) != nil
}

// CertEnabled interprets the cert_enabled on/off switch. Defaults to false
// when absent, blank, or unparseable. Enabling this alone (with no email/
// domain configured) is deliberately allowed — see CertSettings — so the
// certificates nav item becomes reachable before the module is fully
// configured.
func CertEnabled(values map[string]string) bool {
	raw, ok := values[certEnabledKey]
	if !ok {
		return false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return v
}

// CertIssuerMode interprets the persisted cert_issuer_mode setting: "acme"
// (publicly trusted, Let's Encrypt over HTTP-01) or "self_signed" (the
// gateway's own internal CA). Defaults to "acme" when absent, blank, or not
// one of the two known modes — reads never fail.
func CertIssuerMode(values map[string]string) string {
	raw := strings.TrimSpace(values[certIssuerModeKey])
	if isKnownCertIssuerMode(raw) {
		return raw
	}
	return DefaultCertIssuerMode
}

// CertEdgeEnabled interprets the cert_edge_enabled on/off switch, for the
// gateway's OWN edge (nginx) certificate. Mirrors CertEnabled exactly:
// defaults to false when absent, blank, or unparseable.
func CertEdgeEnabled(values map[string]string) bool {
	raw, ok := values[certEdgeEnabledKey]
	if !ok {
		return false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return v
}

// CertEdgeIssuerMode interprets the persisted cert_edge_issuer_mode setting —
// the issuer mode for the gateway's OWN edge certificate, switchable
// independently of CertIssuerMode (the internal/mesh certificates' mode).
// Defaults to "self_signed" when absent, blank, or not one of the two known
// modes — DELIBERATELY the opposite default from CertIssuerMode (which
// defaults to "acme"): an edge name is typically an internal hostname or a
// bare IP address (see ValidateEdgeName), which a public CA cannot validate.
// Reads never fail.
func CertEdgeIssuerMode(values map[string]string) string {
	raw := strings.TrimSpace(values[certEdgeIssuerModeKey])
	if isKnownCertIssuerMode(raw) {
		return raw
	}
	return IssuerModeSelfSigned
}

// ACMEEmail returns the configured ACME account contact email, trimmed.
// Empty when absent — required only in "acme" issuer mode (see CertSettings);
// "self_signed" mode has no registrar and needs no contact at all.
func ACMEEmail(values map[string]string) string {
	return strings.TrimSpace(values[acmeEmailKey])
}

// ACMEDirectoryURL returns the configured ACME directory URL, trimmed,
// defaulting to Let's Encrypt production (DefaultACMEDirectoryURL) when
// absent or blank.
func ACMEDirectoryURL(values map[string]string) string {
	if v := strings.TrimSpace(values[acmeDirectoryURLKey]); v != "" {
		return v
	}
	return DefaultACMEDirectoryURL
}

// CertBaseDomain returns the configured base domain for per-server
// certificates (e.g. server names are issued as "<server>.<base domain>"),
// lower-cased and trimmed. Empty when absent.
func CertBaseDomain(values map[string]string) string {
	return strings.ToLower(strings.TrimSpace(values[certBaseDomainKey]))
}

// CertGatewayDomain returns the configured domain for the gateway's own
// public-facing certificate, lower-cased and trimmed. Empty when absent.
func CertGatewayDomain(values map[string]string) string {
	return strings.ToLower(strings.TrimSpace(values[certGatewayDomainKey]))
}

// CertServerScope interprets the persisted cert_server_scope setting: "all"
// (manage every NetBird-tracked server unless it explicitly opts out) or
// "selected" (manage only servers that explicitly opt in) — mirrors the
// existing netbird_policy_scope shape. Defaults to "selected" when absent,
// blank, or not one of the two known scopes.
func CertServerScope(values map[string]string) string {
	switch strings.TrimSpace(values[certServerScopeKey]) {
	case "all":
		return "all"
	case "selected":
		return "selected"
	default:
		return DefaultCertServerScope
	}
}

// CertManagePublicDomain interprets the cert_manage_public_domain switch:
// when true, the reconcile additionally manages the extra public-facing names
// listed in CertPublicDomains. It does NOT gate the gateway's own certificate —
// desiredCertificates always wants the gateway name (CertGatewayDomain, or the
// live NetBird peer DNS name when that setting is blank), regardless of this
// switch. Defaults to false when absent, blank, or unparseable.
func CertManagePublicDomain(values map[string]string) bool {
	raw, ok := values[certManagePublicDomainKey]
	if !ok {
		return false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return v
}

// decodeCommaSeparatedNameList splits a comma-separated list, trimming,
// lower-casing (both domain names and IP addresses are case/format-
// insensitive here), and dropping empty entries. ALWAYS non-nil, even when
// raw is empty or contains only empty entries (review finding F1.6): the
// value feeds a SystemSettingsDTO field typed string[] on the frontend, and a
// nil Go slice marshals to JSON null — silently breaking that contract for
// any reader that does not itself null-check. Shared by CertPublicDomains and
// CertEdgeNames so the two comma-separated cert-name settings keep
// byte-identical decode semantics.
func decodeCommaSeparatedNameList(raw string) []string {
	out := make([]string, 0)
	for _, part := range strings.Split(raw, ",") {
		if v := strings.ToLower(strings.TrimSpace(part)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// encodeCommaSeparatedNameList normalizes a name list (trim, lower-case, drop
// empties) into the comma-separated string persisted for a cert-name setting
// — the exact inverse of decodeCommaSeparatedNameList, so a round-trip
// through UpdateSystemSettings -> SystemSettingsView is idempotent modulo
// case/whitespace. Shared by CertPublicDomains and CertEdgeNames.
func encodeCommaSeparatedNameList(names []string) string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if v := strings.ToLower(strings.TrimSpace(n)); v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, ",")
}

// CertPublicDomains splits the comma-separated cert_public_domains list. See
// decodeCommaSeparatedNameList for the exact semantics.
func CertPublicDomains(values map[string]string) []string {
	return decodeCommaSeparatedNameList(values[certPublicDomainsKey])
}

// encodeCertPublicDomains is the exact inverse of CertPublicDomains.
func encodeCertPublicDomains(domains []string) string {
	return encodeCommaSeparatedNameList(domains)
}

// CertEdgeNames splits the comma-separated cert_edge_names list -- the SAN
// names for the gateway's OWN edge (nginx) certificate. Uses the exact same
// decode helper as CertPublicDomains (decodeCommaSeparatedNameList): trimmed,
// lower-cased, empties dropped, ALWAYS non-nil. This reader alone does NOT
// validate each entry (that is ValidateEdgeName, applied on the write path in
// UpdateSystemSettings) -- a stored value is trusted to already be clean.
func CertEdgeNames(values map[string]string) []string {
	return decodeCommaSeparatedNameList(values[certEdgeNamesKey])
}

// encodeCertEdgeNames is the exact inverse of CertEdgeNames.
func encodeCertEdgeNames(names []string) string {
	return encodeCommaSeparatedNameList(names)
}

// CertEdgeRequireHTTPS interprets the cert_edge_require_https on/off switch --
// plan B's plaintext-refusal setting for the gateway's OWN edge (nginx)
// listener. Mirrors CertEdgeEnabled exactly: defaults to false when absent,
// blank, or unparseable. This reader alone does not decide whether plaintext
// is actually refused -- that additionally needs the arming precondition (a
// later task) and is not overridden by the env-only kill switch here (see
// config.Config.CertEdgeRequireHTTPSDisable), which the gate consults
// separately and which never touches the settings store.
func CertEdgeRequireHTTPS(values map[string]string) bool {
	raw, ok := values[certEdgeRequireHTTPSKey]
	if !ok {
		return false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return v
}

// CertMeshRequireTLS interprets the cert_mesh_require_tls on/off switch -- P3's
// plaintext-refusal setting for the mesh agent listener. Mirrors
// CertEdgeRequireHTTPS: false when absent, blank, or unparseable. This reader
// alone does not decide whether plaintext is refused -- that additionally needs
// the arming precondition (in internal/gateway) and is overridden by the env-only
// kill switch config.Config.CertMeshRequireTLSDisable, which never touches the
// settings store.
func CertMeshRequireTLS(values map[string]string) bool {
	raw, ok := values[certMeshRequireTLSKey]
	if !ok {
		return false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return v
}

// CertMeshTLSMode returns the stored agent-listener TLS-port mode: "combined"
// or "separate", or "" (absent/blank/unknown) meaning "follow the env
// default" (config.Config.AgentTLSSeparate, threaded in as
// ServiceDeps.AgentTLSSeparateDefault). Reads never fail -- an unknown value
// is treated exactly like an absent one, byte-neutral with every deployment
// that predates this key. See Service.CertMeshTLSSeparateActive for the
// effective (env-fallback-applied) value.
func CertMeshTLSMode(values map[string]string) string {
	v := strings.TrimSpace(values[certMeshTLSModeKey])
	if v == "combined" || v == "separate" {
		return v
	}
	return ""
}

// CertRenewBeforeDays interprets the persisted cert_renew_before_days value
// (how many days before expiry a certificate is renewed), defaulting to 30
// when absent, blank, unparseable, or below the 7-day floor. Mirrors the
// netbird interval readers: reads never fail (fall back to the default),
// only writes (UpdateSystemSettings) reject an out-of-range value outright.
func CertRenewBeforeDays(values map[string]string) int {
	n, err := strconv.Atoi(strings.TrimSpace(values[certRenewBeforeDaysKey]))
	if err != nil || n < MinCertRenewBeforeDays {
		return DefaultCertRenewBeforeDays
	}
	return n
}

// CertSelfSignedValidityDays is the self-signed LEAF certificate lifetime (in
// days). Out-of-range ([1,3650]) or unparseable -> the documented default
// (365) — reads never fail, writes reject.
func CertSelfSignedValidityDays(values map[string]string) int {
	n, err := strconv.Atoi(strings.TrimSpace(values[certSelfSignedValidityDaysKey]))
	if err != nil || n < MinSelfSignedValidityDays || n > MaxSelfSignedValidityDays {
		return DefaultSelfSignedValidityDays
	}
	return n
}

// CertCARenewBeforeDays is how many days before its own expiry the internal
// CA root is rotated. Below the 30-day floor, or unparseable -> the
// documented default (365) — reads never fail, writes reject.
func CertCARenewBeforeDays(values map[string]string) int {
	n, err := strconv.Atoi(strings.TrimSpace(values[certCARenewBeforeDaysKey]))
	if err != nil || n < MinCARenewBeforeDays {
		return DefaultCARenewBeforeDays
	}
	return n
}

// CertPublicIssuerMode interprets the persisted cert_public_issuer_mode
// setting -- the issuer mode for the extra public-facing names in
// CertPublicDomains, switchable independently of CertIssuerMode. Unlike
// CertIssuerMode/CertEdgeIssuerMode it has NO fallback-to-a-known-mode default
// here: absent, blank, or not one of the two known modes all return "" --
// CertSettings.modeFor is the one place "" is interpreted, as "follow
// CertIssuerMode". That is what makes this byte-neutral: a deployment that
// never sets this key keeps whatever CertIssuerMode already governed for its
// public domains.
func CertPublicIssuerMode(values map[string]string) string {
	raw := strings.TrimSpace(values[certPublicIssuerModeKey])
	if isKnownCertIssuerMode(raw) {
		return raw
	}
	return ""
}

// ACMEWeeklyLimit is the per-week issuance ceiling for the GLOBAL (shared)
// ACME account. Reads never fail: absent, blank, unparseable, or negative all
// return 0 ("no limit set here").
func ACMEWeeklyLimit(values map[string]string) int {
	return nonNegativeIntSetting(values[acmeWeeklyLimitKey])
}

// CertHTTPSSwitchMode returns the stored P4 global https-auto-switch mode:
// "auto" or "selected" when explicitly stored, else "manual" (absent, blank,
// or an unknown value) -- byte-neutral: a deployment that never sets this key
// keeps the gateway changing no app scheme, exactly as before this key
// existed. Reads never fail; see httpsSwitchInScope for the per-server
// resolution this mode feeds.
func CertHTTPSSwitchMode(values map[string]string) string {
	v := strings.TrimSpace(values[certHTTPSSwitchModeKey])
	if v == "auto" || v == "selected" {
		return v
	}
	return DefaultCertHTTPSSwitchMode
}

// CertProxyListenPortBase is the auto-assign floor for a managed
// application's ProxyListenPort. Out-of-range ([1024,65535]) or unparseable ->
// the documented default (8600) — reads never fail, writes reject.
func CertProxyListenPortBase(values map[string]string) int {
	n, err := strconv.Atoi(strings.TrimSpace(values[certProxyListenPortBaseKey]))
	if err != nil || n < MinCertProxyListenPortBase || n > MaxCertProxyListenPortBase {
		return DefaultCertProxyListenPortBase
	}
	return n
}

// nonNegativeIntSetting parses raw as a non-negative integer, defaulting to 0
// on any parse failure or a negative value -- shared by ACMEWeeklyLimit and
// its two per-context counterparts (CertEdgeACMEWeeklyLimit,
// CertPublicACMEWeeklyLimit) so the three keep byte-identical decode
// semantics.
func nonNegativeIntSetting(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// CertEdgeACMEShared interprets the cert_edge_acme_shared on/off switch:
// true (the DEFAULT -- absent or unparseable both mean "shared") means the
// gateway's OWN edge certificate re-uses the global ACME account
// (acme_email/acme_directory_url); false means it registers and uses its own
// via CertEdgeACMEEmail/CertEdgeACMEDirectoryURL/CertEdgeACMEWeeklyLimit. The
// default is DELIBERATELY the opposite of CertEdgeRequireHTTPS's (false):
// absent must mean "keep behaving exactly as every pre-unification deployment
// already does" -- one shared account -- not "silently start a second,
// unconfigured one".
func CertEdgeACMEShared(values map[string]string) bool {
	return acmeSharedSetting(values[certEdgeACMESharedKey])
}

// CertPublicACMEShared is CertEdgeACMEShared's public-domain counterpart --
// see its doc for the shared-by-default rationale.
func CertPublicACMEShared(values map[string]string) bool {
	return acmeSharedSetting(values[certPublicACMESharedKey])
}

// acmeSharedSetting parses raw as a boolean, defaulting to TRUE (shared) when
// raw is empty/absent or unparseable -- shared by CertEdgeACMEShared and
// CertPublicACMEShared. This is the inverse default of every other cert_*
// on/off switch in this file (which all default to false when absent): here
// absent must mean "byte-neutral", and byte-neutral IS shared.
func acmeSharedSetting(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return true
	}
	v, err := strconv.ParseBool(trimmed)
	if err != nil {
		return true
	}
	return v
}

// CertEdgeACMEEmail/CertEdgeACMEDirectoryURL return the edge certificate's OWN
// ACME account contact email / directory URL, trimmed, empty when absent.
// Unlike ACMEEmail/ACMEDirectoryURL these have NO Let's-Encrypt-production
// default: they are meaningless (never read by certAcmeConfigFor) while
// CertEdgeACMEShared is true, and an operator who sets shared=false without
// filling them in gets an empty directory/email, not a silent fallback to
// production Let's Encrypt under an unintended account.
func CertEdgeACMEEmail(values map[string]string) string {
	return strings.TrimSpace(values[certEdgeACMEEmailKey])
}

func CertEdgeACMEDirectoryURL(values map[string]string) string {
	return strings.TrimSpace(values[certEdgeACMEDirectoryURLKey])
}

// CertEdgeACMEWeeklyLimit is the per-week issuance ceiling for the edge
// certificate's OWN ACME account (meaningful only when CertEdgeACMEShared is
// false). Reads never fail; see nonNegativeIntSetting.
func CertEdgeACMEWeeklyLimit(values map[string]string) int {
	return nonNegativeIntSetting(values[certEdgeACMEWeeklyLimitKey])
}

// CertPublicACMEEmail/CertPublicACMEDirectoryURL/CertPublicACMEWeeklyLimit are
// CertEdgeACMEEmail/CertEdgeACMEDirectoryURL/CertEdgeACMEWeeklyLimit's
// public-domain counterparts -- see their docs for the no-implicit-default
// rationale.
func CertPublicACMEEmail(values map[string]string) string {
	return strings.TrimSpace(values[certPublicACMEEmailKey])
}

func CertPublicACMEDirectoryURL(values map[string]string) string {
	return strings.TrimSpace(values[certPublicACMEDirectoryURLKey])
}

func CertPublicACMEWeeklyLimit(values map[string]string) int {
	return nonNegativeIntSetting(values[certPublicACMEWeeklyLimitKey])
}

// CertSettings is the fully resolved certificate-management configuration,
// consumed by the reconcile (a later task).
type CertSettings struct {
	IssuerMode         string
	Email              string
	DirectoryURL       string
	BaseDomain         string
	GatewayDomain      string
	ServerScope        string
	ManagePublicDomain bool
	PublicDomains      []string

	RenewBeforeDays        int
	SelfSignedValidityDays int
	CARenewBeforeDays      int

	// EdgeEnabled/EdgeIssuerMode/EdgeNames configure the gateway's OWN edge
	// (nginx) certificate -- a distinct issuance target from the fields
	// above, with its own issuer mode switchable independently of IssuerMode.
	// Task 4 (a later task) is the first consumer.
	EdgeEnabled    bool
	EdgeIssuerMode string
	EdgeNames      []string
	// NOTE: plan B's plaintext-refusal switch (cert_edge_require_https) is
	// deliberately NOT a field here. CertSettings is gated by cert_enabled,
	// which is correct for the three Edge* fields above (they govern the
	// gateway's own issuance) but WRONG for that switch -- an operator may
	// decline the gateway's issuance entirely and still want the gate to act.
	// The only correct reader is Service.CertEdgeRequireHTTPSChecked; a field
	// here would just invite a caller to read the value the design avoids.

	// PublicIssuerMode is the public-domain issuer mode; "" means "follow
	// IssuerMode" -- see modeFor, which is the only place that fallback is
	// resolved. Byte-neutral: an unset key keeps public domains on whatever
	// IssuerMode already governed them.
	PublicIssuerMode string

	// ACMEWeeklyLimit/EdgeACMEShared/EdgeACMEEmail/EdgeACMEDirectoryURL/
	// EdgeACMEWeeklyLimit/PublicACMEShared/PublicACMEEmail/
	// PublicACMEDirectoryURL/PublicACMEWeeklyLimit resolve which ACME account
	// (directory + email) and per-week issuance ceiling each publicly-trusted
	// context (edge, public) uses: the GLOBAL account (DirectoryURL/Email
	// above, ceiling ACMEWeeklyLimit) when that context's *ACMEShared is true
	// (the default -- absent means shared, byte-neutral), or its own trio
	// when false. See certAcmeConfigFor, the resolver issueCertificate calls.
	ACMEWeeklyLimit int

	EdgeACMEShared       bool
	EdgeACMEEmail        string
	EdgeACMEDirectoryURL string
	EdgeACMEWeeklyLimit  int

	PublicACMEShared       bool
	PublicACMEEmail        string
	PublicACMEDirectoryURL string
	PublicACMEWeeklyLimit  int
}

// CertSettings resolves the certificate-management configuration. ok is
// false when the module is off, OR when the active issuer mode's mandatory
// field is missing: "acme" requires an account email (the CA has no other
// way to contact the operator); "self_signed" requires nothing beyond the
// module being on (the internal CA has no registrar). This is the
// "on but not yet usable" state — CertModuleChecked still reports true, so
// the portal nav item stays reachable while ok here stays false until the
// operator finishes configuring the mode they picked.
func (s *Service) CertSettings(ctx context.Context) (CertSettings, bool, error) {
	if s.settings == nil {
		return CertSettings{}, false, nil
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return CertSettings{}, false, err
	}
	if !CertEnabled(values) {
		return CertSettings{}, false, nil
	}
	set := CertSettings{
		IssuerMode:             CertIssuerMode(values),
		Email:                  ACMEEmail(values),
		DirectoryURL:           ACMEDirectoryURL(values),
		BaseDomain:             CertBaseDomain(values),
		GatewayDomain:          CertGatewayDomain(values),
		ServerScope:            CertServerScope(values),
		ManagePublicDomain:     CertManagePublicDomain(values),
		PublicDomains:          CertPublicDomains(values),
		RenewBeforeDays:        CertRenewBeforeDays(values),
		SelfSignedValidityDays: CertSelfSignedValidityDays(values),
		CARenewBeforeDays:      CertCARenewBeforeDays(values),

		EdgeEnabled:    CertEdgeEnabled(values),
		EdgeIssuerMode: CertEdgeIssuerMode(values),
		EdgeNames:      CertEdgeNames(values),

		PublicIssuerMode: CertPublicIssuerMode(values),
		ACMEWeeklyLimit:  ACMEWeeklyLimit(values),

		EdgeACMEShared:       CertEdgeACMEShared(values),
		EdgeACMEEmail:        CertEdgeACMEEmail(values),
		EdgeACMEDirectoryURL: CertEdgeACMEDirectoryURL(values),
		EdgeACMEWeeklyLimit:  CertEdgeACMEWeeklyLimit(values),

		PublicACMEShared:       CertPublicACMEShared(values),
		PublicACMEEmail:        CertPublicACMEEmail(values),
		PublicACMEDirectoryURL: CertPublicACMEDirectoryURL(values),
		PublicACMEWeeklyLimit:  CertPublicACMEWeeklyLimit(values),
	}
	if set.IssuerMode == IssuerModeACME && set.Email == "" {
		return set, false, nil
	}
	return set, true, nil
}

// CertModuleChecked reports the RAW cert_enabled checkbox state (regardless
// of whether the picked issuer mode is fully configured), for the portal nav
// gate — mirrors NetbirdModuleChecked. Nil-safe -> false.
func (s *Service) CertModuleChecked(ctx context.Context) bool {
	if s.settings == nil {
		return false
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return false
	}
	return CertEnabled(values)
}

// CertEdgeRequireHTTPSChecked reports the RAW cert_edge_require_https switch,
// DELIBERATELY bypassing the internal cert_enabled module gate that CertSettings()
// (and therefore CertSettings.EdgeRequireHTTPS) is subject to. That gate is correct
// for EdgeEnabled/EdgeIssuerMode/EdgeNames -- they govern the gateway's OWN
// certificate issuance, which is subordinate to the internal module -- but wrong for
// this switch: an operator may decline the gateway's own issuance entirely
// (cert_enabled=false), terminate TLS with a certificate they installed themselves,
// and still want the plaintext-refusal gate (a later task) to act on this setting.
// Reading it through CertSettings would silently report false in exactly that state,
// regardless of what is actually stored. Nil-safe -> false; mirrors CertModuleChecked's
// shape otherwise.
func (s *Service) CertEdgeRequireHTTPSChecked(ctx context.Context) bool {
	if s.settings == nil {
		return false
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return false
	}
	return CertEdgeRequireHTTPS(values)
}

// CertMeshRequireTLSChecked reports the cert_mesh_require_tls switch. Nil-safe ->
// false, and a store error also yields false, so every failure mode leaves the
// mesh gate DISENGAGED. It is the only correct reader of the switch (mirrors
// CertEdgeRequireHTTPSChecked).
func (s *Service) CertMeshRequireTLSChecked(ctx context.Context) bool {
	if s.settings == nil {
		return false
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return false
	}
	return CertMeshRequireTLS(values)
}

// CertMeshTLSSeparateActive is the EFFECTIVE value of CertMeshTLSMode: the
// stored mode when it is exactly "combined"/"separate", else the env-fallback
// default (ServiceDeps.AgentTLSSeparateDefault, sourced from config.Config.
// AgentTLSSeparate). A later task's mesh listener wiring reads this through
// srv.Portal to decide whether to bind a separate encrypted agent port, so
// unlike CertMeshTLSPort below it is part of the API interface and gets a
// tracing wrapper (see api_tracing_gen.go).
func (s *Service) CertMeshTLSSeparateActive(ctx context.Context) (bool, error) {
	if s.settings == nil {
		return s.agentTLSSeparateDefault, nil
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return false, err
	}
	switch CertMeshTLSMode(values) {
	case "separate":
		return true, nil
	case "combined":
		return false, nil
	default:
		return s.agentTLSSeparateDefault, nil
	}
}

// CertMeshTLSPort is the effective port a later task's separate encrypted
// agent listener would bind (ServiceDeps.AgentTLSPort, resolved by
// cmd/gateway's effectiveAgentTLSPort). It is read-only display data for the
// SystemSettingsDTO builder in this package only -- no other caller needs it
// today, so it stays a plain *Service method instead of joining the API
// interface.
func (s *Service) CertMeshTLSPort() int { return s.agentTLSPort }

// CertLastError returns the reconcile's most recent abort note (see
// certLastErrorKey), trimmed. Empty when no pass has ever aborted at one of
// the two gates ReconcileCertificates guards against, or when a later pass
// got past both of them.
func CertLastError(values map[string]string) string {
	return strings.TrimSpace(values[certLastErrorKey])
}
