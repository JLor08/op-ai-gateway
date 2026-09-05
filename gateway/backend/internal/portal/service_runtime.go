// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"regexp"
	"slices"
	"strings"
	"time"
)

// CodeRuntimeSpecNotFound is ErrRuntimeSpecNotFound's API error code,
// exported so the gateway's error mapper (portal_runtime_endpoints.go) can
// share the exact value instead of re-hardcoding it (mirrors
// CodeApplicationNotFound/CodeMappingNotFound in service_applications.go).
const CodeRuntimeSpecNotFound = "runtime_spec.not_found"

var (
	// ErrRuntimeSpecNotFound is returned by DeleteRuntimeSpec when the
	// mapping has no spec row to delete. GetRuntimeSpec never returns it
	// (an absent spec there is Configured:false, not an error) — see the
	// package's GetRuntimeSpec doc comment.
	ErrRuntimeSpecNotFound          = errors.New(CodeRuntimeSpecNotFound)
	ErrRuntimeSpecBinaryRequired    = errors.New("runtime_spec.binary_required")
	ErrRuntimeSpecArgsInvalid       = errors.New("runtime_spec.args_invalid")
	ErrRuntimeSpecEnvInvalid        = errors.New("runtime_spec.env_invalid")
	ErrRuntimeSpecGPUInvalid        = errors.New("runtime_spec.gpu_invalid")
	ErrRuntimeSpecTuningInvalid     = errors.New("runtime_spec.tuning_invalid")
	ErrRuntimeSpecAdminStateInvalid = errors.New("runtime_spec.admin_state_invalid")
	// ErrRuntimeSpecVisibleDevicesNoGPUs rejects set_visible_devices on a
	// spec with no GPU rows. The agent would emit `CUDA_VISIBLE_DEVICES=`
	// for such a spec, and an EMPTY visibility value does not mean "no
	// restriction" — it means NOTHING is visible, i.e. the model sees no GPU
	// at all. Refused here rather than emitted; the agent refuses it again
	// at launch (see PutRuntimeSpec for why both).
	ErrRuntimeSpecVisibleDevicesNoGPUs = errors.New("runtime_spec.visible_devices_no_gpus")
	// ErrRuntimeSpecVisibleDevicesConflict rejects set_visible_devices on a
	// spec whose env already sets a GPU visibility variable by hand. Two
	// sources for one value, whichever wins, is a configuration an operator
	// cannot read the truth out of.
	ErrRuntimeSpecVisibleDevicesConflict = errors.New("runtime_spec.visible_devices_conflict")
	// ErrRuntimeSpecNotServerAgent rejects a write (PUT or DELETE) targeting
	// a mapping whose owning application is not of type
	// routing.ProviderServerAgent — a runtime spec only makes sense for an
	// agent-managed model process. GET is deliberately permissive (see
	// GetRuntimeSpec) so the portal can ask about any mapping without
	// special-casing the application type.
	ErrRuntimeSpecNotServerAgent = errors.New("runtime_spec.application_not_server_agent")
	// ErrRuntimeSpecServerBenchmarking rejects a launch-spec write while a
	// benchmark run holds that server's reservation. See serverIsBenchmarking.
	ErrRuntimeSpecServerBenchmarking = errors.New("runtime_spec.server_benchmarking")
	// ErrRuntimeSpecAdminStateConflict rejects a
	// SetBenchmarkRuntimeSpecAdminState whose freshly-read admin_state is not
	// the value the caller said it was replacing. This endpoint class is a
	// read-modify-write with no If-Match and no row version, and the VRAM
	// benchmark's deferred restore is the caller that needs the guard: without
	// it, a restore minutes after the drain would hand a concurrent operator's
	// override straight back to "". The caller records the spec as
	// restore-failed instead -- somebody else owns the field now.
	ErrRuntimeSpecAdminStateConflict = errors.New("runtime_spec.admin_state_conflict")
	// ErrRuntimeSpecEndpointModeInvalid rejects a responses_mode/messages_mode
	// that is not one of the three EndpointMode values. HTTP 400.
	ErrRuntimeSpecEndpointModeInvalid = errors.New("runtime_spec.endpoint_mode_invalid")
	// ErrRuntimeSpecFlavorInvalid rejects an api_flavors entry that is not
	// openai/anthropic. HTTP 400.
	ErrRuntimeSpecFlavorInvalid = errors.New("runtime_spec.flavor_invalid")
	// ErrRuntimeSpecVisibleDevicesModeInvalid rejects a visible_devices_mode
	// that is not "env" or "args". HTTP 400.
	ErrRuntimeSpecVisibleDevicesModeInvalid = errors.New("runtime_spec.visible_devices_mode_invalid")
	// ErrRuntimeSpecVisibleDevicesArgsNoPlaceholder rejects set_visible_devices
	// in args mode when none of the three device placeholders
	// (${CUDA_DEVICES}/${VULKAN_DEVICES}/${METAL_DEVICES}) appears in args: the
	// agent would inject no visibility and the selection would be lost. HTTP 400.
	ErrRuntimeSpecVisibleDevicesArgsNoPlaceholder = errors.New("runtime_spec.visible_devices_args_no_placeholder")
	// ErrRuntimeSpecAPITokenModeInvalid rejects an api_token_mode that is not
	// one of off/set/random/app. HTTP 400.
	ErrRuntimeSpecAPITokenModeInvalid = errors.New("runtime_spec.api_token_mode_invalid")
	// ErrRuntimeSpecAPITokenNoPlaceholder rejects mode set/random when the
	// literal "${API_TOKEN}" appears in none of Env's values or Args: the
	// agent would launch the process with no token wired in at all. HTTP 400.
	ErrRuntimeSpecAPITokenNoPlaceholder = errors.New("runtime_spec.api_token_no_placeholder")
	// ErrRuntimeSpecAPITokenPlaceholderWithoutMode rejects mode off when the
	// placeholder is still present in Env/Args: it would expand to nothing at
	// launch, silently leaving the literal string in the child's environment
	// or argv. HTTP 400.
	ErrRuntimeSpecAPITokenPlaceholderWithoutMode = errors.New("runtime_spec.api_token_placeholder_without_mode")
	// ErrRuntimeSpecAPITokenHeaderInvalid rejects an api_token_header_source
	// that is not app/custom, or a custom source whose api_token_header fails
	// checkHeaderName's shape check. HTTP 400.
	ErrRuntimeSpecAPITokenHeaderInvalid = errors.New("runtime_spec.api_token_header_invalid")
)

// Task 6 sentinels: the co-residency matrix, per-GPU VRAM budgets, the
// managed-runtime-only application-create gate, and the server-level
// runtime-process-limit validation. Grouped here (rather than split across
// service.go/service_applications.go, where the request fields they validate
// physically live) because they are one feature cut, mirroring how every
// RuntimeSpec sentinel above lives in this file regardless of which method
// returns it.
var (
	// ErrCoResidencyPairInvalid rejects a SetCoResidency pair for any of:
	// the two mapping ids being identical, either id not belonging to THIS
	// application's own mappings (verified locally, not a global existence
	// check -- a pair naming a mapping from a different application is
	// invalid the same way), or two pairs colliding after canonical
	// (mapping_a_id < mapping_b_id) normalization.
	ErrCoResidencyPairInvalid = errors.New("runtime_coresidency.pair_invalid")
	// ErrGPUBudgetInvalid rejects a SetServerGPUBudgets entry with a negative
	// index, a negative budget_mb, or an index repeated across entries.
	ErrGPUBudgetInvalid = errors.New("server.gpu_budget_invalid")
	// ErrServerManagedRuntimeOnly rejects CreateApplication when the target
	// server is ManagedRuntimeOnly and the requested type is not
	// routing.ProviderServerAgent. HTTP 409 (a state conflict with the
	// server's own configuration, not a malformed request).
	ErrServerManagedRuntimeOnly = errors.New("application.managed_runtime_only")
	// ErrServerRuntimeLimitInvalid rejects a negative
	// CreateServerRequest/UpdateServerRequest.RuntimeMaxProcesses.
	ErrServerRuntimeLimitInvalid = errors.New("server.runtime_limit_invalid")
	// ErrServerAgentApplicationExists rejects a CreateApplication or
	// UpdateApplication that would leave an AI server with more than ONE
	// routing.ProviderServerAgent application. That is a load-bearing
	// invariant, not tidiness: a server's single server_agent Application row
	// backs the agent's single router port, and AgentRuntimeConfig derives
	// the whole runtime-config document from THAT application. With two such
	// rows the document is built from whichever one the store returns first
	// (ids are random hex and the store orders by id), so runtime
	// configuration edited under the other one would persist in the store
	// and never reach the agent -- the portal would accept configuration
	// that silently never takes effect.
	//
	// Enforced on BOTH write paths: create, and update (retyping an existing
	// application to server_agent is the easy way past a create-only gate).
	// HTTP 409 -- a conflict with the server's existing configuration, not a
	// malformed request (mirrors ErrServerManagedRuntimeOnly above).
	ErrServerAgentApplicationExists = errors.New("application.server_agent_exists")
)

const (
	defaultRuntimeSpecHealthPath            = "/health"
	defaultRuntimeSpecHealthTimeoutSeconds  = 5
	defaultRuntimeSpecStartupTimeoutSeconds = 180
)

// runtimeSpecEnvKeyPattern matches a shell-style environment variable name:
// upper-case letters, digits, underscore, not starting with a digit. Values
// are unrestricted (they legitimately carry ${AGENT_ENV:NAME}/${PORT}/${MODEL}
// placeholders the agent resolves at launch time — never validated or
// rewritten here; see the RuntimeSpecDTO.Env doc below).
var runtimeSpecEnvKeyPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// --- binary absoluteness: the gateway-side mirror of the agent's check -----
//
// THE AUTHORITY IS THE AGENT. `spec.binary` is executed on the AGENT's host,
// and the agent decides whether it may run at all: LocalPolicy.Permit
// (server-agent/internal/runtime/policy_local.go) requires
// `filepath.IsAbs(spec.Binary)` and then an exact match against the agent
// operator's own allowlist. `filepath.IsAbs` is compiled per-GOOS, so on a
// windows-amd64/windows-arm64 agent it accepts `C:\llama\llama-server.exe`
// and rejects `/opt/llama/llama-server`, and the reverse on linux/darwin.
//
// What follows is the EARLY-FEEDBACK MIRROR of that rule, nothing more. The
// gateway is OS-agnostic, cannot execute the path and does not know which
// GOOS the spec is destined for, so it accepts a path that `filepath.IsAbs`
// would accept on EITHER platform — its only job is to catch a typo in the
// portal form instead of letting it fail later as a terminal `not_permitted`
// on the agent. It is deliberately no laxer than the agent (a spec the
// portal accepts but the agent refuses is worse than a form error at the
// point of entry) and no stricter (that is the defect this replaced: a
// POSIX-only `strings.HasPrefix(binary, "/")` made a Windows AI server
// impossible to configure at all).
//
// The two predicates below — one per platform, kept separate so a reader can
// see which platform each branch serves instead of meeting one clever
// combined rule — are transcribed from Go's own implementation
// (internal/filepathlite.IsAbs + volumeNameLen, which path/filepath.IsAbs
// delegates to), together with the small helpers those need.

// isAbsPOSIXBinaryPath mirrors filepath.IsAbs as compiled for a unix GOOS,
// where the whole rule is a leading slash.
func isAbsPOSIXBinaryPath(path string) bool {
	return strings.HasPrefix(path, "/")
}

// isAbsWindowsBinaryPath mirrors filepath.IsAbs as compiled for GOOS
// windows. Accepted: a drive letter with either separator (`C:\x`, `c:/x`,
// `C:\`), a UNC share (`\\host\share`, `//host/share/foo`), and the local /
// root-local device forms (`\\?\c:\x`, `\\.\c:\x`, `\??\c:\x`). Rejected:
// the two forms that LOOK rooted but are resolved against per-process state
// Windows will not have here — drive-relative (`C:foo`, `c:`) and
// root-relative (`\foo`, `/foo`, `\`).
func isAbsWindowsBinaryPath(path string) bool {
	volumeLen := windowsVolumeNameLen(path)
	if volumeLen == 0 {
		return false
	}
	// A volume name starting with two separators is a UNC or device path,
	// which is always absolute. (volumeLen != 0 guarantees len(path) >= 2.)
	if isWindowsPathSeparator(path[0]) && isWindowsPathSeparator(path[1]) {
		return true
	}
	path = path[volumeLen:]
	if path == "" {
		return false // bare volume: `C:` names the drive's current directory
	}
	return isWindowsPathSeparator(path[0])
}

// isWindowsPathSeparator mirrors os.IsPathSeparator on windows: Windows
// accepts the forward slash everywhere it accepts the backslash.
func isWindowsPathSeparator(c byte) bool {
	return c == '\\' || c == '/'
}

// windowsVolumeNameLen returns the length of path's leading volume name
// under Windows rules, or 0 when it has none. Transcription of
// internal/filepathlite.volumeNameLen (windows build).
func windowsVolumeNameLen(path string) int {
	switch {
	case len(path) >= 2 && path[1] == ':':
		// A drive letter. Windows itself does not insist the letter be in
		// A-Z and neither does Go, so neither do we.
		return 2
	case len(path) == 0 || !isWindowsPathSeparator(path[0]):
		return 0
	case windowsPathHasPrefixFold(path, `\\.\UNC`):
		// `\\.\UNC\host\share`: host and share count as the volume.
		return windowsUNCLen(path, len(`\\.\UNC\`))
	case windowsPathHasPrefixFold(path, `\\.`),
		windowsPathHasPrefixFold(path, `\\?`),
		windowsPathHasPrefixFold(path, `\??`):
		// Local Device (`\\.\`) and Root Local Device (`\\?\`, `\??\`)
		// paths: the component after the prefix is part of the volume.
		if len(path) == 3 {
			return 3
		}
		_, rest, ok := cutWindowsPath(path[4:])
		if !ok {
			return len(path)
		}
		return len(path) - len(rest) - 1
	case len(path) >= 2 && isWindowsPathSeparator(path[1]):
		// `\\host\share`: an ordinary UNC path.
		return windowsUNCLen(path, 2)
	}
	return 0
}

// windowsPathHasPrefixFold reports whether path begins with prefix, ignoring
// ASCII case and treating `\` and `/` as equivalent. A longer path must
// continue with a separator, so `\\.foo` does not match prefix `\\.`.
func windowsPathHasPrefixFold(path, prefix string) bool {
	if len(path) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if isWindowsPathSeparator(prefix[i]) {
			if !isWindowsPathSeparator(path[i]) {
				return false
			}
		} else if asciiUpperByte(prefix[i]) != asciiUpperByte(path[i]) {
			return false
		}
	}
	return len(path) == len(prefix) || isWindowsPathSeparator(path[len(prefix)])
}

// windowsUNCLen returns the length of a UNC path's volume prefix
// (`host\share`), where prefixLen is the offset at which the host starts —
// 2 for `\\host\share`, len(`\\.\UNC\`) for the device spelling.
func windowsUNCLen(path string, prefixLen int) int {
	separators := 0
	for i := prefixLen; i < len(path); i++ {
		if isWindowsPathSeparator(path[i]) {
			separators++
			if separators == 2 {
				return i
			}
		}
	}
	return len(path)
}

// cutWindowsPath slices path around its first path separator.
func cutWindowsPath(path string) (before, after string, found bool) {
	for i := 0; i < len(path); i++ {
		if isWindowsPathSeparator(path[i]) {
			return path[:i], path[i+1:], true
		}
	}
	return path, "", false
}

// asciiUpperByte upper-cases one ASCII byte and leaves every other byte
// alone (Windows path prefixes are ASCII).
func asciiUpperByte(c byte) byte {
	if 'a' <= c && c <= 'z' {
		return c - ('a' - 'A')
	}
	return c
}

// runtimeSpecBinaryIsAbsolute reports whether binary is an absolute path for
// at least one of the platforms an agent can run on. The empty string is
// absolute nowhere, so this single check covers both halves of
// ErrRuntimeSpecBinaryRequired ("required" and "must be absolute").
func runtimeSpecBinaryIsAbsolute(binary string) bool {
	return isAbsPOSIXBinaryPath(binary) || isAbsWindowsBinaryPath(binary)
}

// normalizeRuntimeSpecFlavors is normalizeFlavors (service_applications.go)
// scoped to the runtime-spec error code: same defaulting/dedup/validation as
// normalizeApplicationFlavors, but a bad entry reports the honest
// runtime_spec.flavor_invalid rather than the application-scoped code.
func normalizeRuntimeSpecFlavors(raw []string) ([]string, error) {
	return normalizeFlavors(raw, ErrRuntimeSpecFlavorInvalid)
}

// RuntimeSpecGPUDTO is one per-GPU VRAM demand row on the wire.
// VRAMEstimateMB is operator-owned (round-tripped from PutRuntimeSpecRequest
// verbatim); VRAMMeasuredMB is agent-owned and read-only on this API — see
// the VRAM ownership rule on PutRuntimeSpec.
type RuntimeSpecGPUDTO struct {
	Index          int `json:"index"`
	VRAMEstimateMB int `json:"vram_estimate_mb"`
	VRAMMeasuredMB int `json:"vram_measured_mb"`
}

// RuntimeSpecDTO is the portal-facing representation of a mapping's
// agent-managed launch spec (routing.RuntimeSpec + its GPU rows).
type RuntimeSpecDTO struct {
	// Configured is false when the mapping has no runtime spec row yet — the
	// only signal GetRuntimeSpec ever uses for "not configured"; every other
	// field is then a zero value (GPUs/Args/Env still non-nil empty).
	Configured                  bool              `json:"configured"`
	ID                          string            `json:"id,omitempty"`
	MappingID                   string            `json:"mapping_id"`
	Enabled                     bool              `json:"enabled"`
	Binary                      string            `json:"binary"`
	Args                        []string          `json:"args"`
	Env                         map[string]string `json:"env"`
	WorkDir                     string            `json:"work_dir"`
	ListenPort                  int               `json:"listen_port"`
	HealthPath                  string            `json:"health_path"`
	HealthTimeoutSeconds        int               `json:"health_timeout_seconds"`
	StartupTimeoutSeconds       int               `json:"startup_timeout_seconds"`
	IdleTimeoutSeconds          int               `json:"idle_timeout_seconds"`
	AdmissionWaitTimeoutSeconds int               `json:"admission_wait_timeout_seconds"`
	Pinned                      bool              `json:"pinned"`
	AdminState                  string            `json:"admin_state"`
	VRAMLocked                  bool              `json:"vram_locked"`
	// SetVisibleDevices turns this spec's GPU list from a declaration into
	// an enforcement — see routing.RuntimeSpec.SetVisibleDevices, and
	// PutRuntimeSpec for the two combinations this API refuses.
	SetVisibleDevices bool                `json:"set_visible_devices"`
	GPUs              []RuntimeSpecGPUDTO `json:"gpus"`
	// APIFlavors / ResponsesMode / MessagesMode are the per-spec snapshot of
	// the three-state endpoint control (mirrors the same trio on
	// ApplicationDTO), stored explicitly on this spec rather than inherited
	// from the parent server_agent application — see PutRuntimeSpec's "no
	// backend inheritance" contract.
	APIFlavors    []string `json:"api_flavors"`
	ResponsesMode string   `json:"responses_mode"`
	MessagesMode  string   `json:"messages_mode"`
	// VisibleDevicesMode is "env" | "args": how set_visible_devices is
	// enforced. Only meaningful when SetVisibleDevices is on; default "env".
	VisibleDevicesMode string `json:"visible_devices_mode"`
	// APITokenMode is "app"|"set"|"random"|"off" (design §2). Default "app".
	APITokenMode string `json:"api_token_mode"`
	// APITokenSet reports presence of a per-spec token (set/random); the VALUE
	// is never on the wire.
	APITokenSet bool `json:"api_token_set"`
	// APITokenHeaderSource is "app"|"custom"; APITokenHeader is the custom header.
	APITokenHeaderSource string `json:"api_token_header_source"`
	APITokenHeader       string `json:"api_token_header"`
	// AppAPITokenSet / AppAPITokenHeader echo the parent application's token
	// presence and header (read-only) so the portal can render the inherited
	// header and the "app has no token ⇒ auth off" hint under app mode.
	AppAPITokenSet    bool   `json:"app_api_token_set"`
	AppAPITokenHeader string `json:"app_api_token_header"`
}

// PutRuntimeSpecRequest is a full-document upsert (no pointer-patch): every
// field is applied verbatim (after validation/defaulting), never merged
// against the stored row — except VRAMMeasuredMB on each GPU entry, which is
// ALWAYS ignored (agent-owned; see PutRuntimeSpec's VRAM ownership rule).
type PutRuntimeSpecRequest struct {
	Enabled                     bool              `json:"enabled"`
	Binary                      string            `json:"binary"`
	Args                        []string          `json:"args"`
	Env                         map[string]string `json:"env"`
	WorkDir                     string            `json:"work_dir"`
	ListenPort                  int               `json:"listen_port"`
	HealthPath                  string            `json:"health_path"`
	HealthTimeoutSeconds        int               `json:"health_timeout_seconds"`
	StartupTimeoutSeconds       int               `json:"startup_timeout_seconds"`
	IdleTimeoutSeconds          int               `json:"idle_timeout_seconds"`
	AdmissionWaitTimeoutSeconds int               `json:"admission_wait_timeout_seconds"`
	Pinned                      bool              `json:"pinned"`
	AdminState                  string            `json:"admin_state"`
	VRAMLocked                  bool              `json:"vram_locked"`
	// SetVisibleDevices turns this spec's GPU list from a declaration into
	// an enforcement — see routing.RuntimeSpec.SetVisibleDevices, and
	// PutRuntimeSpec for the two combinations this API refuses.
	SetVisibleDevices bool                `json:"set_visible_devices"`
	GPUs              []RuntimeSpecGPUDTO `json:"gpus"`
	// APIFlavors / ResponsesMode / MessagesMode: see RuntimeSpecDTO's doc.
	// Absent (empty/"") defaults to both/passthrough — see putRuntimeSpec;
	// the backend does NOT inherit the parent server_agent application's
	// values, the portal frontend pre-fills the create form instead.
	APIFlavors    []string `json:"api_flavors"`
	ResponsesMode string   `json:"responses_mode"`
	MessagesMode  string   `json:"messages_mode"`
	// VisibleDevicesMode: see RuntimeSpecDTO's doc. Absent (empty/"")
	// defaults to "env" — see putRuntimeSpec.
	VisibleDevicesMode string `json:"visible_devices_mode"`
	// APITokenMode / APITokenHeaderSource / APITokenHeader: see
	// RuntimeSpecDTO's doc. Absent (empty/"") defaults to "app" for both mode
	// and header source — see validateRuntimeSpecAPIToken.
	APITokenMode         string `json:"api_token_mode"`
	APITokenHeaderSource string `json:"api_token_header_source"`
	APITokenHeader       string `json:"api_token_header"`
	// APIToken is write-only: nil = keep, "" = clear, value = replace-and-seal.
	// Sealing/rotation happens in putRuntimeSpec's write path --
	// validateRuntimeSpecAPIToken only validates the shape of the request
	// around it.
	APIToken *string `json:"api_token"`
	// APITokenRotate, when true under mode "random", forces regeneration on
	// write.
	APITokenRotate bool `json:"api_token_rotate"`
}

// GetRuntimeSpec returns mappingID's runtime spec, or Configured:false when
// none has been created yet (not an error). Deliberately permissive about
// the owning application's type — unlike PutRuntimeSpec/DeleteRuntimeSpec,
// it never returns ErrRuntimeSpecNotServerAgent, so the portal can query any
// mapping's runtime configuration without special-casing non-server_agent
// applications.
func (s *Service) GetRuntimeSpec(ctx context.Context, principal auth.Token, mappingID string) (RuntimeSpecDTO, error) {
	mapping, app, _, err := s.authorizeMapping(ctx, principal, mappingID)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	spec, ok, err := s.routes.RuntimeSpecByMapping(ctx, mapping.ID)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	if !ok {
		return RuntimeSpecDTO{
			MappingID:            mapping.ID,
			Args:                 []string{},
			Env:                  map[string]string{},
			GPUs:                 []RuntimeSpecGPUDTO{},
			APIFlavors:           []string{},
			VisibleDevicesMode:   string(routing.VisibleDevicesModeEnv),
			APITokenMode:         string(routing.RuntimeAPITokenModeApp),
			APITokenHeaderSource: string(routing.RuntimeAPITokenHeaderSourceApp),
			AppAPITokenSet:       app.APIToken != "",
			AppAPITokenHeader:    app.APITokenHeader,
		}, nil
	}
	gpus, err := s.routes.RuntimeSpecGPUs(ctx, spec.ID)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	return runtimeSpecDTO(spec, gpus, app)
}

// PutRuntimeSpec validates and upserts mappingID's runtime spec (create on
// first write, full-document replace thereafter).
//
// VRAM ownership rule (one rule, both directions): vram_estimate_mb is
// operator-owned — only this method ever writes it. vram_measured_mb is
// agent-owned — only the telemetry write-back (a later task) writes it, so
// this method PRESERVES the stored measured values verbatim for every GPU
// index that already had a row, regardless of what the request carries, and
// starts a brand-new index at 0. vram_locked never blocks this write: it
// decides which of the two numbers the AGENT is told (agentRuntimeSpecDTO)
// and whether the write-back may replace the measured one, not what the
// operator may store.
func (s *Service) PutRuntimeSpec(ctx context.Context, principal auth.Token, mappingID string, req PutRuntimeSpecRequest) (RuntimeSpecDTO, error) {
	mapping, app, server, err := s.authorizeMapping(ctx, principal, mappingID)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	// Checked AFTER authorization, so a caller with no claim to this mapping
	// learns nothing about the server behind it -- and only on this
	// principal-carrying path, never inside the shared putRuntimeSpec body,
	// which the benchmark run's own drain and restore also go through.
	if s.serverIsBenchmarking(server.ID) {
		return RuntimeSpecDTO{}, ErrRuntimeSpecServerBenchmarking
	}
	return s.putRuntimeSpec(ctx, mapping, app, server, req)
}

// serverIsBenchmarking reports whether a benchmark run currently holds
// serverID's reservation. nil hook = no (see SetBenchmarkReservationHook).
//
// It gates the launch-spec write because that write is a FULL-DOCUMENT
// REPLACE with admin_state among its fields, so it is an override action as
// much as an edit: one "Force start" on a spec a VRAM run has drained starts a
// sibling whose allocation lands inside the measurement window, and the number
// that comes out carries the sibling's memory reported as the target's. The
// run can detect that afterwards and refuse to report a number, but detecting
// it costs the operator the whole run; refusing the write costs them one
// message.
//
// The reservation is the right fact to gate on and not a new one: it is
// already what excludes the server from gateway routing while a run is in
// flight, it is already at most one run per server, and a run that dies
// releases it. What it deliberately does NOT gate is the run's own writer
// (SetBenchmarkRuntimeSpecAdminState, which is the caller that took the
// reservation), a write to any OTHER server, or a DELETE -- deleting a spec
// mid-run drains it rather than starting it, and the restore already treats a
// deleted spec as restored.
func (s *Service) serverIsBenchmarking(serverID string) bool {
	return s.benchmarkReserved != nil && s.benchmarkReserved(serverID)
}

// putRuntimeSpec is PutRuntimeSpec's whole body with the AUTHORIZATION
// removed and the resolved mapping/application/server handed in. It is the
// one implementation of the full-document upsert -- validation, defaulting,
// the VRAM ownership rule, and the notification -- so a second caller cannot
// end up with a subtly different write.
//
// Its callers, and what authorized each: PutRuntimeSpec (the portal
// principal, via authorizeMapping) and SetBenchmarkRuntimeSpecAdminState (the
// benchmark trigger request, gated before the run started -- see that
// method's doc for why the run itself carries no principal).
func (s *Service) putRuntimeSpec(ctx context.Context, mapping routing.ModelMapping, app routing.Application, server routing.AIServer, req PutRuntimeSpecRequest) (RuntimeSpecDTO, error) {
	if app.Type != routing.ProviderServerAgent {
		return RuntimeSpecDTO{}, ErrRuntimeSpecNotServerAgent
	}
	// Validate everything that can fail BEFORE mutating/persisting anything.
	binary := strings.TrimSpace(req.Binary)
	// Absolute on POSIX *or* on Windows -- the agent's own filepath.IsAbs is
	// the authority and this is its early-feedback mirror; see
	// runtimeSpecBinaryIsAbsolute.
	if !runtimeSpecBinaryIsAbsolute(binary) {
		return RuntimeSpecDTO{}, ErrRuntimeSpecBinaryRequired
	}
	if req.ListenPort < 0 || req.HealthTimeoutSeconds < 0 || req.StartupTimeoutSeconds < 0 ||
		req.IdleTimeoutSeconds < 0 || req.AdmissionWaitTimeoutSeconds < 0 {
		return RuntimeSpecDTO{}, ErrRuntimeSpecTuningInvalid
	}
	adminState := strings.TrimSpace(req.AdminState)
	switch adminState {
	case "", "force_running", "force_stopped":
	default:
		return RuntimeSpecDTO{}, ErrRuntimeSpecAdminStateInvalid
	}
	if err := validateRuntimeSpecGPUs(req.GPUs); err != nil {
		return RuntimeSpecDTO{}, err
	}
	for k := range req.Env {
		if !runtimeSpecEnvKeyPattern.MatchString(k) {
			return RuntimeSpecDTO{}, ErrRuntimeSpecEnvInvalid
		}
	}
	if err := validateRuntimeSpecVisibleDevices(req); err != nil {
		return RuntimeSpecDTO{}, err
	}
	if err := validateRuntimeSpecAPIToken(req); err != nil {
		return RuntimeSpecDTO{}, err
	}
	// Endpoint-mode + flavor validation, defaulting absent fields (spec
	// §5.4/§12: the backend does NOT read the parent app to inherit -- the
	// frontend pre-fills the create form; the backend only supplies a sane
	// default when the request omits a field). See
	// TestPutRuntimeSpecDoesNotInheritAppModes.
	respMode := routing.EndpointModePassthrough
	if strings.TrimSpace(req.ResponsesMode) != "" {
		m, ok := validEndpointMode(req.ResponsesMode)
		if !ok {
			return RuntimeSpecDTO{}, ErrRuntimeSpecEndpointModeInvalid
		}
		respMode = m
	}
	msgMode := routing.EndpointModePassthrough
	if strings.TrimSpace(req.MessagesMode) != "" {
		m, ok := validEndpointMode(req.MessagesMode)
		if !ok {
			return RuntimeSpecDTO{}, ErrRuntimeSpecEndpointModeInvalid
		}
		msgMode = m
	}
	flavors, err := normalizeRuntimeSpecFlavors(req.APIFlavors)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	// VisibleDevicesMode: an omitted mode defaults to "env" (today's
	// behavior); a bad value is a LATER task's validation (mode validation
	// is not wired up yet — this resolves the stored typed value only).
	visibleMode := routing.VisibleDevicesModeEnv
	if strings.TrimSpace(req.VisibleDevicesMode) != "" {
		visibleMode = routing.VisibleDevicesMode(strings.TrimSpace(req.VisibleDevicesMode))
	}
	args := req.Args
	if args == nil {
		args = []string{}
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return RuntimeSpecDTO{}, ErrRuntimeSpecArgsInvalid
	}
	env := req.Env
	if env == nil {
		env = map[string]string{}
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return RuntimeSpecDTO{}, ErrRuntimeSpecEnvInvalid
	}
	healthPath := strings.TrimSpace(req.HealthPath)
	if healthPath == "" {
		healthPath = defaultRuntimeSpecHealthPath
	}
	healthTimeout := req.HealthTimeoutSeconds
	if healthTimeout == 0 {
		healthTimeout = defaultRuntimeSpecHealthTimeoutSeconds
	}
	startupTimeout := req.StartupTimeoutSeconds
	if startupTimeout == 0 {
		startupTimeout = defaultRuntimeSpecStartupTimeoutSeconds
	}
	// Read-then-upsert: an existing spec's id/created_at are preserved, and
	// its stored GPU rows are the source of truth for VRAMMeasuredMB (the
	// VRAM ownership rule above) — never what the request sent.
	existing, hadExisting, err := s.routes.RuntimeSpecByMapping(ctx, mapping.ID)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	measuredByIndex := map[int]int{}
	if hadExisting {
		existingGPUs, err := s.routes.RuntimeSpecGPUs(ctx, existing.ID)
		if err != nil {
			return RuntimeSpecDTO{}, err
		}
		for _, g := range existingGPUs {
			measuredByIndex[g.GPUIndex] = g.VRAMMeasuredMB
		}
	}
	// Per-spec API token (design §2): compute the SEALED value to persist
	// STRICTLY BEFORE the store write, so a keyless-disk seal rejection
	// (capture.ErrKeyRequired) returns without persisting anything -- no
	// plaintext token, no half-applied mode. Mirrors service_applications.go's
	// write-only *string sentinel exactly: nil = keep the stored (already
	// sealed) value, "" seals to "" = clear, a value replaces-and-seals. Mode
	// "random" generates a fresh secret on first write or when rotate is asked;
	// modes "app"/"off" store no per-spec token. Validation
	// (validateRuntimeSpecAPIToken, above) has already run -- this only seals.
	mode := req.APITokenMode
	if mode == "" {
		mode = string(routing.RuntimeAPITokenModeApp)
	}
	sealedToken := existing.APIToken // keep by default (existing = the loaded spec, "" when none)
	switch routing.RuntimeAPITokenMode(mode) {
	case routing.RuntimeAPITokenModeRandom:
		if existing.APIToken == "" || req.APITokenRotate {
			rawToken, err := generateSecret()
			if err != nil {
				return RuntimeSpecDTO{}, err
			}
			sealed, err := capture.SealSecret(s.cipher, s.settingsVolatile, rawToken)
			if err != nil {
				return RuntimeSpecDTO{}, err // ErrKeyRequired on a keyless disk store => 400, nothing persisted
			}
			sealedToken = sealed
		}
	case routing.RuntimeAPITokenModeSet:
		if req.APIToken != nil { // nil = keep the stored token untouched
			sealed, err := capture.SealSecret(s.cipher, s.settingsVolatile, *req.APIToken)
			if err != nil {
				return RuntimeSpecDTO{}, err
			}
			sealedToken = sealed // "" seals to "" = cleared
		}
	default: // app, off -- no per-spec token stored
		sealedToken = ""
	}
	headerSource := req.APITokenHeaderSource
	if headerSource == "" {
		headerSource = string(routing.RuntimeAPITokenHeaderSourceApp)
	}
	// APITokenHeader is only meaningful for a custom source (its shape was
	// validated above via checkHeaderName); an "app" source inherits the
	// application's header at request-auth time (routing.SpecUpstreamAuth via
	// effectiveAPITokenHeader) and stores none here.
	headerName := ""
	if routing.RuntimeAPITokenHeaderSource(headerSource) == routing.RuntimeAPITokenHeaderSourceCustom {
		headerName = strings.TrimSpace(req.APITokenHeader)
	}
	now := s.clock().UTC()
	spec := routing.RuntimeSpec{
		ID:                          "rspec_" + compactRandomHex(16),
		MappingID:                   mapping.ID,
		Enabled:                     req.Enabled,
		Binary:                      binary,
		Args:                        string(argsJSON),
		Env:                         string(envJSON),
		WorkDir:                     strings.TrimSpace(req.WorkDir),
		ListenPort:                  req.ListenPort,
		HealthPath:                  healthPath,
		HealthTimeoutSeconds:        healthTimeout,
		StartupTimeoutSeconds:       startupTimeout,
		IdleTimeoutSeconds:          req.IdleTimeoutSeconds,
		AdmissionWaitTimeoutSeconds: req.AdmissionWaitTimeoutSeconds,
		Pinned:                      req.Pinned,
		AdminState:                  adminState,
		VRAMLocked:                  req.VRAMLocked,
		SetVisibleDevices:           req.SetVisibleDevices,
		VisibleDevicesMode:          visibleMode,
		APITokenMode:                mode,
		APIToken:                    sealedToken,
		APITokenHeaderSource:        headerSource,
		APITokenHeader:              headerName,
		APIFlavors:                  flavors,
		ResponsesMode:               respMode,
		MessagesMode:                msgMode,
		CreatedAt:                   now,
		UpdatedAt:                   now,
	}
	if hadExisting {
		spec.ID = existing.ID
		spec.CreatedAt = existing.CreatedAt
	}
	if err := s.routes.UpsertRuntimeSpec(ctx, spec); err != nil {
		return RuntimeSpecDTO{}, err
	}
	gpuRows := make([]routing.RuntimeSpecGPU, 0, len(req.GPUs))
	for i, g := range req.GPUs {
		gpuRows = append(gpuRows, routing.RuntimeSpecGPU{
			SpecID:         spec.ID,
			GPUIndex:       g.Index,
			Position:       i, // request array order becomes the stored order
			VRAMEstimateMB: g.VRAMEstimateMB,
			VRAMMeasuredMB: measuredByIndex[g.Index], // 0 for a brand-new index; preserved otherwise
		})
	}
	if err := s.routes.SetRuntimeSpecGPUs(ctx, spec.ID, gpuRows); err != nil {
		return RuntimeSpecDTO{}, err
	}
	s.notifyRuntimeChanged(server.ID)
	storedGPUs, err := s.routes.RuntimeSpecGPUs(ctx, spec.ID)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	return runtimeSpecDTO(spec, storedGPUs, app)
}

// DeleteRuntimeSpec removes mappingID's runtime spec. ErrRuntimeSpecNotFound
// when none exists. Deliberately does NOT gate on the owning application's
// type the way PutRuntimeSpec does: UpdateApplication lets an operator
// retype a server_agent application to something else with no check against
// its current type, and DeleteApplication does not cascade-clean runtime
// specs — so a spec can end up on a non-server_agent application through
// ordinary API use, not just seeded test state. Removal must always be
// possible regardless of how a dependency became orphaned; only the
// creation of a NEW dependency on server_agent semantics is gated. (An
// earlier version of this method gated DELETE the same way as PUT; that was
// a defect, not a deliberate symmetry — see the fix-round-1 note in the
// task-5 report.)
func (s *Service) DeleteRuntimeSpec(ctx context.Context, principal auth.Token, mappingID string) error {
	mapping, _, server, err := s.authorizeMapping(ctx, principal, mappingID)
	if err != nil {
		return err
	}
	spec, ok, err := s.routes.RuntimeSpecByMapping(ctx, mapping.ID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrRuntimeSpecNotFound
	}
	if err := s.routes.DeleteRuntimeSpec(ctx, spec.ID); err != nil {
		return err
	}
	s.notifyRuntimeChanged(server.ID)
	return nil
}

// runtimeSpecVisibleDevicesVars is the set of environment variables the
// set_visible_devices option OWNS: a spec may not hand-set any of them while
// the option is on. It mirrors the agent's own visibleDevicesOwnedVars
// (server-agent internal/runtime/policy_local.go) name for name, deliberately
// — the two validators must refuse exactly the same inputs, or the portal
// starts rejecting specs the agent would accept (or worse, the other way
// round).
//
// HIP_VISIBLE_DEVICES is in the list although the agent never SETS it: it
// selects from WHAT ROCR_VISIBLE_DEVICES ALREADY LEFT VISIBLE, so combining a
// hand-set HIP list with agent-managed ROCR filtering is the double-filter
// trap in its purest form (the child ends up with no usable device). Runtime-
// specific selectors the agent neither sets nor filters through —
// ONEAPI_DEVICE_SELECTOR, GPU_DEVICE_ORDINAL — are deliberately NOT here:
// composing one of those (built from ${HOST_GPU_IDS}) with the option is the
// documented escape hatch, not a contradiction.
var runtimeSpecVisibleDevicesVars = []string{
	"CUDA_VISIBLE_DEVICES",
	"ROCR_VISIBLE_DEVICES",
	"HIP_VISIBLE_DEVICES",
}

// runtimeSpecDevicePlaceholders are the three llama.cpp --device placeholders
// the agent expands (mirrors the agent's own token set in policy_local.go).
// In args mode at least one must appear in the spec's args, else the
// selection would silently vanish.
var runtimeSpecDevicePlaceholders = []string{
	"${CUDA_DEVICES}",
	"${VULKAN_DEVICES}",
	"${METAL_DEVICES}",
}

func argsHaveDevicePlaceholder(args []string) bool {
	for _, a := range args {
		for _, ph := range runtimeSpecDevicePlaceholders {
			if strings.Contains(a, ph) {
				return true
			}
		}
	}
	return false
}

// validVisibleDevicesMode validates a raw visible_devices_mode at the DTO
// edge (mirrors validEndpointMode). Empty is valid and defaults to "env" in
// putRuntimeSpec; any other non-env/args value is rejected.
func validVisibleDevicesMode(raw string) (routing.VisibleDevicesMode, bool) {
	switch m := routing.VisibleDevicesMode(strings.TrimSpace(raw)); m {
	case "", routing.VisibleDevicesModeEnv, routing.VisibleDevicesModeArgs:
		return m, true
	default:
		return "", false
	}
}

// validateRuntimeSpecVisibleDevices validates visible_devices_mode (always,
// regardless of the flag) and, when set_visible_devices is on, enforces the
// combinations it may not be saved in: an env conflict, an empty GPU list, or
// (in args mode) args with no device placeholder. The flag-gated traps are
// ALSO refused by the agent at launch, and that duplication is deliberate in
// both directions:
//
//   - Only the portal can tell the operator IN THE MOMENT. The agent's refusal
//     surfaces as a terminal `not_permitted` on a spec the operator already
//     saved and walked away from; a form error is the one an operator acts on.
//   - Only the agent covers the file-mode path. An agent configured with
//     OP_AGENT_RUNTIME_SOURCE=file reads a hand-written local document that
//     never passes through this service at all, so the portal alone would
//     leave that whole path unguarded.
//
// The rules themselves are vendor-independent on BOTH sides, which is what
// lets them be identical: this gateway is hardware-agnostic and cannot know
// whether the target host runs NVIDIA, AMD or neither (the spec is authored
// before anyone has necessarily looked at the machine), so a vendor-dependent
// agent rule would be a rule the portal could not mirror.
//
// Runs BEFORE any mutation, like every other check in PutRuntimeSpec, and
// reads req rather than the stored row: this API is a full-document upsert, so
// the request IS the resulting spec.
func validateRuntimeSpecVisibleDevices(req PutRuntimeSpecRequest) error {
	// The mode value is validated regardless of the flag: a malformed enum is
	// a malformed request. Empty defaults to "env" (resolved in putRuntimeSpec).
	mode, ok := validVisibleDevicesMode(req.VisibleDevicesMode)
	if !ok {
		return ErrRuntimeSpecVisibleDevicesModeInvalid
	}
	if !req.SetVisibleDevices {
		return nil
	}
	// Trap 3 before trap 1, matching the agent's order, so a spec that is
	// wrong in both ways reports the same one on both sides.
	for k := range req.Env {
		if slices.Contains(runtimeSpecVisibleDevicesVars, strings.ToUpper(strings.TrimSpace(k))) {
			return ErrRuntimeSpecVisibleDevicesConflict
		}
	}
	// An empty GPU list is not "no restriction" — the agent would emit
	// `CUDA_VISIBLE_DEVICES=`, which hides every card from the model.
	if len(req.GPUs) == 0 {
		return ErrRuntimeSpecVisibleDevicesNoGPUs
	}
	// args mode: at least one device placeholder must be present, else the
	// agent injects nothing and the selection is silently lost.
	if mode == routing.VisibleDevicesModeArgs && !argsHaveDevicePlaceholder(req.Args) {
		return ErrRuntimeSpecVisibleDevicesArgsNoPlaceholder
	}
	return nil
}

// validRuntimeAPITokenMode reports whether s is one of the four
// routing.RuntimeAPITokenMode values. Unlike validVisibleDevicesMode, empty
// is NOT accepted here -- callers normalize "" to "app" themselves before
// calling this (see validateRuntimeSpecAPIToken), the same way the sealing
// logic in putRuntimeSpec needs the resolved mode, not the raw request value.
func validRuntimeAPITokenMode(s string) bool {
	switch routing.RuntimeAPITokenMode(s) {
	case routing.RuntimeAPITokenModeOff, routing.RuntimeAPITokenModeSet,
		routing.RuntimeAPITokenModeRandom, routing.RuntimeAPITokenModeApp:
		return true
	}
	return false
}

// specHasAPITokenPlaceholder reports whether the literal "${API_TOKEN}"
// appears in any Env value or any Args element. Args is included
// deliberately (design decision C1): some upstream binaries only accept a
// bearer token as a CLI flag, not an environment variable, and the portal's
// job here is only to detect the placeholder, not to steer where it is used
// -- the agent is the one that emits a loud warning for the args case.
func specHasAPITokenPlaceholder(env map[string]string, args []string) bool {
	const ph = "${API_TOKEN}"
	for _, v := range env {
		if strings.Contains(v, ph) {
			return true
		}
	}
	for _, a := range args {
		if strings.Contains(a, ph) {
			return true
		}
	}
	return false
}

// validateRuntimeSpecAPIToken validates api_token_mode, the ${API_TOKEN}
// placeholder requirement that mode implies, and api_token_header_source/
// api_token_header. It does NOT look at api_token or api_token_rotate --
// those drive sealing/rotation in putRuntimeSpec's write path, a persistence
// concern this pure request-shape validator has no business with -- and it
// does NOT read the parent application's own token: there is no app_unset
// error, mode "app" is valid regardless of whether the app has a token
// configured (an app with no token simply means auth ends up off for this
// spec, resolved later by resolvePushToken, not a validation failure here).
//
// Runs BEFORE any mutation, like validateRuntimeSpecVisibleDevices, and
// reads req rather than the stored row for the same reason: PutRuntimeSpec
// is a full-document upsert, so the request IS the resulting spec.
func validateRuntimeSpecAPIToken(req PutRuntimeSpecRequest) error {
	mode := req.APITokenMode
	if mode == "" {
		mode = string(routing.RuntimeAPITokenModeApp)
	}
	if !validRuntimeAPITokenMode(mode) {
		return ErrRuntimeSpecAPITokenModeInvalid
	}
	hasPlaceholder := specHasAPITokenPlaceholder(req.Env, req.Args)
	switch routing.RuntimeAPITokenMode(mode) {
	case routing.RuntimeAPITokenModeSet, routing.RuntimeAPITokenModeRandom:
		if !hasPlaceholder {
			return ErrRuntimeSpecAPITokenNoPlaceholder
		}
	case routing.RuntimeAPITokenModeOff:
		if hasPlaceholder {
			return ErrRuntimeSpecAPITokenPlaceholderWithoutMode
		}
	}
	src := req.APITokenHeaderSource
	if src == "" {
		src = string(routing.RuntimeAPITokenHeaderSourceApp)
	}
	switch routing.RuntimeAPITokenHeaderSource(src) {
	case routing.RuntimeAPITokenHeaderSourceApp:
		// header inherited from the app; req.APITokenHeader ignored.
	case routing.RuntimeAPITokenHeaderSourceCustom:
		if _, err := checkHeaderName(req.APITokenHeader); err != nil {
			return ErrRuntimeSpecAPITokenHeaderInvalid
		}
	default:
		return ErrRuntimeSpecAPITokenHeaderInvalid
	}
	return nil
}

// validateRuntimeSpecGPUs rejects a negative GPU index, a negative VRAM
// estimate, or a GPU index repeated across entries — all ErrRuntimeSpecGPUInvalid,
// checked BEFORE any store call (the store's own duplicate-index guard
// returns ErrConflict, a storage-layer concern this method never surfaces).
func validateRuntimeSpecGPUs(gpus []RuntimeSpecGPUDTO) error {
	seen := make(map[int]struct{}, len(gpus))
	for _, g := range gpus {
		if g.Index < 0 || g.VRAMEstimateMB < 0 {
			return ErrRuntimeSpecGPUInvalid
		}
		if _, dup := seen[g.Index]; dup {
			return ErrRuntimeSpecGPUInvalid
		}
		seen[g.Index] = struct{}{}
	}
	return nil
}

// runtimeSpecDTO builds the wire DTO from a stored spec + its GPU rows, plus
// the parent application (for the read-only api-token echoes — see
// RuntimeSpecDTO.AppAPITokenSet/AppAPITokenHeader). Args/Env are opaque JSON
// strings at the store layer (the netbird_group_ids pattern) — an unmarshal
// failure here means the stored row is corrupt, not a client-input problem,
// but still surfaces as the matching domain sentinel
// (ErrRuntimeSpecArgsInvalid / ErrRuntimeSpecEnvInvalid) rather than a raw
// JSON error or a 500.
func runtimeSpecDTO(spec routing.RuntimeSpec, gpus []routing.RuntimeSpecGPU, app routing.Application) (RuntimeSpecDTO, error) {
	var args []string
	if err := json.Unmarshal([]byte(spec.Args), &args); err != nil {
		return RuntimeSpecDTO{}, ErrRuntimeSpecArgsInvalid
	}
	if args == nil {
		args = []string{}
	}
	env := map[string]string{}
	if err := json.Unmarshal([]byte(spec.Env), &env); err != nil {
		return RuntimeSpecDTO{}, ErrRuntimeSpecEnvInvalid
	}
	if env == nil {
		env = map[string]string{}
	}
	gpuDTOs := make([]RuntimeSpecGPUDTO, 0, len(gpus))
	for _, g := range gpus {
		gpuDTOs = append(gpuDTOs, RuntimeSpecGPUDTO{
			Index:          g.GPUIndex,
			VRAMEstimateMB: g.VRAMEstimateMB,
			VRAMMeasuredMB: g.VRAMMeasuredMB,
		})
	}
	return RuntimeSpecDTO{
		Configured:                  true,
		ID:                          spec.ID,
		MappingID:                   spec.MappingID,
		Enabled:                     spec.Enabled,
		Binary:                      spec.Binary,
		Args:                        args,
		Env:                         env,
		WorkDir:                     spec.WorkDir,
		ListenPort:                  spec.ListenPort,
		HealthPath:                  spec.HealthPath,
		HealthTimeoutSeconds:        spec.HealthTimeoutSeconds,
		StartupTimeoutSeconds:       spec.StartupTimeoutSeconds,
		IdleTimeoutSeconds:          spec.IdleTimeoutSeconds,
		AdmissionWaitTimeoutSeconds: spec.AdmissionWaitTimeoutSeconds,
		Pinned:                      spec.Pinned,
		AdminState:                  spec.AdminState,
		VRAMLocked:                  spec.VRAMLocked,
		SetVisibleDevices:           spec.SetVisibleDevices,
		GPUs:                        gpuDTOs,
		APIFlavors:                  append([]string{}, spec.APIFlavors...),
		ResponsesMode:               string(spec.ResponsesMode),
		MessagesMode:                string(spec.MessagesMode),
		VisibleDevicesMode:          string(spec.VisibleDevicesMode),
		APITokenMode:                normalizeRuntimeAPITokenMode(spec.APITokenMode),
		APITokenSet:                 spec.APIToken != "",
		APITokenHeaderSource:        normalizeRuntimeAPITokenHeaderSource(spec.APITokenHeaderSource),
		APITokenHeader:              spec.APITokenHeader,
		AppAPITokenSet:              app.APIToken != "",
		AppAPITokenHeader:           app.APITokenHeader,
	}, nil
}

// normalizeRuntimeAPITokenMode defaults an empty stored/DTO mode to "app" —
// every row created before this API-token support existed has "" here, and
// "" must never reach the wire: it means the same thing as "app"
// (routing.RuntimeAPITokenModeApp) but portal callers should only ever see
// the one canonical spelling.
func normalizeRuntimeAPITokenMode(mode string) string {
	if mode == "" {
		return string(routing.RuntimeAPITokenModeApp)
	}
	return mode
}

// normalizeRuntimeAPITokenHeaderSource is normalizeRuntimeAPITokenMode's
// sibling for the header-source column ("app"|"custom", default "app").
func normalizeRuntimeAPITokenHeaderSource(source string) string {
	if source == "" {
		return string(routing.RuntimeAPITokenHeaderSourceApp)
	}
	return source
}

// --- Task 6: co-residency matrix --------------------------------------------

// CoResidencyDTO is the portal-facing pairwise co-residency matrix for one
// application: every ALLOWED pair of its own mappings that may be loaded on
// the same AI server at the same time. Each pair is always canonical
// (Pairs[i][0] < Pairs[i][1] lexicographically) -- see SetCoResidency.
// Always a non-nil Pairs slice, even when empty.
type CoResidencyDTO struct {
	Pairs [][2]string `json:"pairs"`
}

// SetCoResidencyRequest is a full-document replace (like
// SetRuntimeSpecGPUs/SetServerGPUBudgets): every pair supplied here IS the
// new set. Pair ordering within each element does not matter -- SetCoResidency
// sorts each pair server-side before storing/comparing.
type SetCoResidencyRequest struct {
	Pairs [][2]string `json:"pairs"`
}

// GetCoResidency returns appID's allowed co-residency pairs. authorizeApplication
// gates it (404-no-leak, same collapse as every other application read).
func (s *Service) GetCoResidency(ctx context.Context, principal auth.Token, appID string) (CoResidencyDTO, error) {
	app, _, err := s.authorizeApplication(ctx, principal, appID)
	if err != nil {
		return CoResidencyDTO{}, err
	}
	rules, err := s.routes.CoResidencyRulesByApplication(ctx, app.ID)
	if err != nil {
		return CoResidencyDTO{}, err
	}
	return coResidencyDTO(rules), nil
}

// SetCoResidency validates and atomically replaces appID's whole co-residency
// set. Every pair is validated BEFORE any store call (validate-before-mutate):
// the two ids must be distinct, both must name a mapping belonging to appID
// itself (checked against s.routes.MappingsByApplication, never a bare global
// existence check -- a mapping id that exists but belongs to a DIFFERENT
// application is exactly as invalid as one that does not exist at all), each
// pair is canonicalized by sorting it (mapping_a_id < mapping_b_id
// lexicographically) so the client never has to submit pairs in a particular
// order, and duplicate pairs are rejected AFTER that normalization -- so
// [["a","b"],["b","a"]] is a duplicate, not two distinct entries. The store
// itself (SetCoResidencyRules) is a dumb pair table by design and performs
// none of this; it is entirely this method's job.
func (s *Service) SetCoResidency(ctx context.Context, principal auth.Token, appID string, req SetCoResidencyRequest) (CoResidencyDTO, error) {
	app, server, err := s.authorizeApplication(ctx, principal, appID)
	if err != nil {
		return CoResidencyDTO{}, err
	}
	ownMappings, err := s.routes.MappingsByApplication(ctx, app.ID)
	if err != nil {
		return CoResidencyDTO{}, err
	}
	belongsToApp := make(map[string]bool, len(ownMappings))
	for _, m := range ownMappings {
		belongsToApp[m.ID] = true
	}
	seenPairs := make(map[[2]string]bool, len(req.Pairs))
	rules := make([]routing.CoResidencyRule, 0, len(req.Pairs))
	now := s.clock().UTC()
	for _, pair := range req.Pairs {
		a, b := strings.TrimSpace(pair[0]), strings.TrimSpace(pair[1])
		if a == "" || b == "" || a == b {
			return CoResidencyDTO{}, ErrCoResidencyPairInvalid
		}
		if !belongsToApp[a] || !belongsToApp[b] {
			return CoResidencyDTO{}, ErrCoResidencyPairInvalid
		}
		if a > b {
			a, b = b, a
		}
		key := [2]string{a, b}
		if seenPairs[key] {
			return CoResidencyDTO{}, ErrCoResidencyPairInvalid
		}
		seenPairs[key] = true
		rules = append(rules, routing.CoResidencyRule{ApplicationID: app.ID, MappingAID: a, MappingBID: b, CreatedAt: now})
	}
	if err := s.routes.SetCoResidencyRules(ctx, app.ID, rules); err != nil {
		return CoResidencyDTO{}, err
	}
	s.notifyRuntimeChanged(server.ID)
	stored, err := s.routes.CoResidencyRulesByApplication(ctx, app.ID)
	if err != nil {
		return CoResidencyDTO{}, err
	}
	return coResidencyDTO(stored), nil
}

// coResidencyDTO builds the wire DTO from stored rules; always a non-nil
// Pairs slice (a collection-shaped return must never serialize to JSON
// null -- see SetRuntimeSpecGPUs's equivalent contract).
func coResidencyDTO(rules []routing.CoResidencyRule) CoResidencyDTO {
	pairs := make([][2]string, 0, len(rules))
	for _, r := range rules {
		pairs = append(pairs, [2]string{r.MappingAID, r.MappingBID})
	}
	return CoResidencyDTO{Pairs: pairs}
}

// --- Task 6: per-GPU VRAM budgets -------------------------------------------

// GPUBudgetDTO is one per-GPU VRAM budget row on the wire. ExpectedUUID/
// ExpectedName are a purely descriptive drift detector, snapshotted
// server-side from live telemetry -- see SetServerGPUBudgets; a client's
// request value for either is always ignored (never trusted on the wire),
// both on first creation and on every later PUT.
type GPUBudgetDTO struct {
	Index        int    `json:"index"`
	BudgetMB     int    `json:"budget_mb"`
	ExpectedUUID string `json:"expected_uuid"`
	ExpectedName string `json:"expected_name"`
}

// SetGPUBudgetsRequest is a full-document replace, mirroring SetCoResidencyRequest.
type SetGPUBudgetsRequest struct {
	Budgets []GPUBudgetDTO `json:"budgets"`
}

// GetServerGPUBudgets returns serverID's per-GPU VRAM budgets. authorizeServer
// gates it (404-no-leak).
func (s *Service) GetServerGPUBudgets(ctx context.Context, principal auth.Token, serverID string) ([]GPUBudgetDTO, error) {
	server, err := s.authorizeServer(ctx, principal, serverID)
	if err != nil {
		return nil, err
	}
	budgets, err := s.routes.ServerGPUBudgets(ctx, server.ID)
	if err != nil {
		return nil, err
	}
	return gpuBudgetDTOs(budgets), nil
}

// SetServerGPUBudgets validates and atomically replaces serverID's whole
// per-GPU budget set. index must be >= 0 and unique across the request;
// budget_mb must be >= 0 (both ErrGPUBudgetInvalid, checked BEFORE any store
// call).
//
// expected_uuid/expected_name ownership rule (mirrors PutRuntimeSpec's VRAM
// ownership rule): they are a purely descriptive drift detector, never
// client-writable. For an index that already has a stored row, this method
// PRESERVES its expected_* verbatim regardless of what the request carries --
// drift detection is only meaningful against the ORIGINAL snapshot. For a
// brand-new index, expected_* is snapshotted from the latest telemetry
// sample's GPU list (see latestGPUSnapshotByIndex); when no sample exists
// yet, it is left empty rather than failing the write.
func (s *Service) SetServerGPUBudgets(ctx context.Context, principal auth.Token, serverID string, req SetGPUBudgetsRequest) ([]GPUBudgetDTO, error) {
	server, err := s.authorizeServer(ctx, principal, serverID)
	if err != nil {
		return nil, err
	}
	seenIndex := make(map[int]bool, len(req.Budgets))
	for _, b := range req.Budgets {
		if b.Index < 0 || b.BudgetMB < 0 {
			return nil, ErrGPUBudgetInvalid
		}
		if seenIndex[b.Index] {
			return nil, ErrGPUBudgetInvalid
		}
		seenIndex[b.Index] = true
	}
	existing, err := s.routes.ServerGPUBudgets(ctx, server.ID)
	if err != nil {
		return nil, err
	}
	existingByIndex := make(map[int]routing.ServerGPUBudget, len(existing))
	for _, b := range existing {
		existingByIndex[b.GPUIndex] = b
	}
	now := s.clock().UTC()
	rows := make([]routing.ServerGPUBudget, 0, len(req.Budgets))
	var snapshot map[int]routing.GPUSample // lazily loaded only if a brand-new index needs it
	for _, b := range req.Budgets {
		row := routing.ServerGPUBudget{ServerID: server.ID, GPUIndex: b.Index, BudgetMB: b.BudgetMB, UpdatedAt: now}
		if prior, ok := existingByIndex[b.Index]; ok {
			row.ExpectedUUID = prior.ExpectedUUID
			row.ExpectedName = prior.ExpectedName
			row.CreatedAt = prior.CreatedAt
		} else {
			if snapshot == nil {
				snapshot = s.latestGPUSnapshotByIndex(ctx, server.ID)
			}
			if g, ok := snapshot[b.Index]; ok {
				row.ExpectedUUID = g.UUID
				row.ExpectedName = g.Name
			}
			row.CreatedAt = now
		}
		rows = append(rows, row)
	}
	if err := s.routes.SetServerGPUBudgets(ctx, server.ID, rows); err != nil {
		return nil, err
	}
	s.notifyRuntimeChanged(server.ID)
	stored, err := s.routes.ServerGPUBudgets(ctx, server.ID)
	if err != nil {
		return nil, err
	}
	return gpuBudgetDTOs(stored), nil
}

// latestGPUSnapshotByIndex reads serverID's single most recent telemetry
// sample (TelemetrySamples with limit=1 returns exactly the newest row --
// DecimateTelemetrySamples' limit==1 case) and indexes its GPU list by
// index, for SetServerGPUBudgets' new-row expected_uuid/expected_name
// snapshot. Best-effort: any read error or the absence of a sample yields an
// empty map (the caller then leaves expected_* empty), mirroring the
// "leave the fields empty rather than failing" contract.
func (s *Service) latestGPUSnapshotByIndex(ctx context.Context, serverID string) map[int]routing.GPUSample {
	out := map[int]routing.GPUSample{}
	samples, err := s.routes.TelemetrySamples(ctx, serverID, time.Time{}, s.clock().UTC(), 1)
	if err != nil || len(samples) == 0 {
		return out
	}
	for _, g := range samples[0].GPUs {
		out[g.Index] = g
	}
	return out
}

// gpuBudgetDTOs builds the wire DTOs from stored rows; always a non-nil
// slice.
func gpuBudgetDTOs(budgets []routing.ServerGPUBudget) []GPUBudgetDTO {
	out := make([]GPUBudgetDTO, 0, len(budgets))
	for _, b := range budgets {
		out = append(out, GPUBudgetDTO{Index: b.GPUIndex, BudgetMB: b.BudgetMB, ExpectedUUID: b.ExpectedUUID, ExpectedName: b.ExpectedName})
	}
	return out
}

// --- Task 6: runtime warnings -----------------------------------------------

// runtimeTimeoutBelowStartupWarning: the application's gateway-side upstream
// deadline (TimeoutMS) keeps running while the agent is still starting a
// model process, so a TimeoutMS below the slowest enabled spec's startup
// timeout silently kills every cold start.
const runtimeTimeoutBelowStartupWarning = "timeout_ms_below_startup_timeout"

// runtimeBinaryPathOSMismatchWarning: a spec's binary path is absolute for
// the OTHER platform than the one this server's agent reports in its
// telemetry (a `C:\…` path on a linux-reporting server, or a `/…` path on a
// windows-reporting server). The agent's filepath.IsAbs will refuse it at
// launch with a terminal not_permitted, so it is worth saying now.
//
// Deliberately a WARNING and never a rejection: the reported OS is telemetry,
// which a freshly created server has none of, and a runtime spec must be
// configurable before — or entirely independently of — an agent ever checking
// in. PutRuntimeSpec therefore accepts either platform's absolute form
// unconditionally (runtimeSpecBinaryIsAbsolute) and this advisory carries the
// OS knowledge we happen to have.
const runtimeBinaryPathOSMismatchWarning = "binary_path_os_mismatch"

// RuntimeWarnings is a pure derivation (no store write) of operator-facing
// warnings about appID's current runtime configuration. authorizeApplication
// gates it (404-no-leak). Always a non-nil slice, even when empty.
func (s *Service) RuntimeWarnings(ctx context.Context, principal auth.Token, appID string) ([]string, error) {
	app, server, err := s.authorizeApplication(ctx, principal, appID)
	if err != nil {
		return nil, err
	}
	specs, err := s.routes.RuntimeSpecsByApplication(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	maxStartupSeconds := 0
	for _, spec := range specs {
		if !spec.Enabled {
			continue
		}
		if spec.StartupTimeoutSeconds > maxStartupSeconds {
			maxStartupSeconds = spec.StartupTimeoutSeconds
		}
	}
	osMismatch, err := s.anySpecBinaryContradictsReportedOS(ctx, server.ID, specs)
	if err != nil {
		return nil, err
	}
	warnings := make([]string, 0)
	if maxStartupSeconds > 0 && app.TimeoutMS < maxStartupSeconds*1000 {
		warnings = append(warnings, runtimeTimeoutBelowStartupWarning)
	}
	if osMismatch {
		warnings = append(warnings, runtimeBinaryPathOSMismatchWarning)
	}
	return warnings, nil
}

// anySpecBinaryContradictsReportedOS backs runtimeBinaryPathOSMismatchWarning:
// it reads serverID's latest reported GOOS and reports whether any of specs
// carries a binary path absolute for the other platform.
//
// Unlike the timeout warning, this pass does NOT skip disabled specs. That one
// describes a consequence of RUNNING (a disabled spec never cold-starts); this
// one describes a value the operator just typed, and a spec is routinely
// created disabled and enabled afterwards -- staying silent until then would
// withhold the advisory exactly when it is useful.
//
// A store failure IS propagated: the portal renders a failed warnings read as
// "advisories unavailable" with a retry, which is honest, where swallowing it
// would silently drop a warning. An ABSENT telemetry row is not a failure --
// it is the ordinary "agent has never reported" state, and yields no warning.
func (s *Service) anySpecBinaryContradictsReportedOS(ctx context.Context, serverID string, specs []routing.RuntimeSpec) (bool, error) {
	if len(specs) == 0 {
		return false, nil // nothing to compare against; skip the telemetry read
	}
	telemetry, ok, err := s.routes.TelemetryByServer(ctx, serverID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	reportedOS := strings.ToLower(strings.TrimSpace(telemetry.OS))
	for _, spec := range specs {
		if runtimeSpecBinaryContradictsOS(spec.Binary, reportedOS) {
			return true, nil
		}
	}
	return false, nil
}

// runtimeSpecBinaryContradictsOS reports whether binary's absolute-path FORM
// belongs to a platform other than reportedOS (a Go GOOS string as the agent
// sends it, or "" when the agent has never reported).
//
// Only a path absolute on exactly ONE platform can contradict anything: a
// UNC path spelled with forward slashes (`//host/share/x`) is absolute under
// both rules and never warns, and a path absolute under neither has already
// been refused by PutRuntimeSpec. Every GOOS other than "windows" is treated
// as POSIX-shaped, which is true of all of them.
func runtimeSpecBinaryContradictsOS(binary, reportedOS string) bool {
	if reportedOS == "" {
		return false // never reported: nothing to contradict
	}
	binary = strings.TrimSpace(binary)
	posix := isAbsPOSIXBinaryPath(binary)
	windows := isAbsWindowsBinaryPath(binary)
	if reportedOS == "windows" {
		return posix && !windows
	}
	return windows && !posix
}

// notifyRuntimeChanged best-effort notifies the runtime-config-changed hook
// (ServiceDeps.OnRuntimeConfigChanged / SetRuntimeConfigChangedHook) after a
// successful write. nil-safe: unset in every test that doesn't care, and in
// any driver that predates Task 8's real push wiring.
//
// THE RULE, stated once, for every notification in this service:
//
//	Any successful write that CAN change a server's runtime-config document
//	notifies that server's agent -- and what decides it is the write path's
//	own SCOPE (which row it writes, and for an application-owned row whether
//	that application is the server's server_agent one), never which field the
//	request happened to change.
//
// A new write path can be checked against that one sentence, because
// AgentRuntimeConfig (below) derives the whole document from exactly six
// kinds of row. A path needs a notification if and only if it writes one of
// them:
//
//  1. the AI server row        -> max_processes (AIServer.RuntimeMaxProcesses)
//  2. its server_agent APPLICATION row -> router_listen (Application.Port);
//     that application's id is also the key for rows 3-5
//  3. that application's MAPPINGS -> each spec's model / upstream_model
//     (gateway_model_name / app_model_name -- the agent has no other source)
//  4. those mappings' RUNTIME SPECS and per-spec GPU rows -> specs[]
//  5. that application's CO-RESIDENCY rules -> coresident[]
//  6. the server's per-GPU VRAM BUDGETS -> gpu_budgets[]
//
// Call sites, by row: (1) UpdateServer; (2) CreateApplication /
// UpdateApplication / DeleteApplication, via
// notifyRuntimeChangedForApplication; (3) CreateMapping / UpdateMapping /
// DeleteMapping / reconcileApplicationModels, via
// notifyRuntimeChangedForMapping; (4) PutRuntimeSpec / DeleteRuntimeSpec;
// (5) SetCoResidency; (6) SetServerGPUBudgets.
//
// The "never which field" half is the load-bearing half. A "relevant fields"
// allow-list inside a write path would be a second, uncompiled copy of
// AgentRuntimeConfig's derivation, and it would rot the moment that
// derivation grows a field -- while over-notifying is cheap and idempotent
// (one goroutine, and gateway.Server.PushRuntimeConfig fail-closes on "no
// runtime_manager agent connected for this server" before it reads anything;
// the agent then re-fetches and its driver applies only on a real ETag
// change). Under-notifying is the actual bug.
//
// Only two kinds of write to rows 1-6 deliberately do NOT notify, and both
// are exemptions of a whole write PATH rather than of a field:
//
//   - A writer whose own signature confines it to columns OUTSIDE the
//     document, so there is no field inspection that could rot:
//     persistApplicationSchemeSwitch (Scheme only), AgentProxyRoutes' port
//     assignment (ProxyListenPort only, and itself an agent poll read path),
//     SetServerEnergyConfig (the five energy columns only) and the gateway's
//     telemetry ingest (LastSeenAt/UpdatedAt only).
//   - The agent's OWN writeback: gateway.Server.writeBackRuntimeVRAM ->
//     UpdateRuntimeSpecGPUMeasured. That one genuinely does change the
//     document (a measured VRAM value wins over the operator's estimate), but
//     it changes it FROM the agent, so a push would echo the agent its own
//     measurement -- once per telemetry sample, for a value that keeps
//     moving. The agent already has it; its poll reconciles the rest.
func (s *Service) notifyRuntimeChanged(serverID string) {
	if s.runtimeChanged != nil {
		s.runtimeChanged(serverID)
	}
}

// notifyRuntimeChangedForApplication is notifyRuntimeChanged for the
// APPLICATION write paths (CreateApplication / UpdateApplication /
// DeleteApplication in service_applications.go). The application row is a
// runtime-config input in its own right -- AgentRuntimeConfig derives
// RouterListen from the server's server_agent application's Port, and keys
// every mapping/spec/co-residency lookup off that application's id -- so a
// create, retype or delete changes the document the agent must act on just
// as much as a runtime-spec write does. Without this the agent only learned
// about a brand-new server_agent application on its next 60 s poll, which
// showed up as "the router takes up to a minute to come up" (the app-health
// probe does not special-case server_agent, so the application reads
// unhealthy until the router is bound).
//
// previousType is the application's type BEFORE the write ("" when it is
// being created), currentType the type AFTER it ("" when it is being
// deleted). EITHER side matching routing.ProviderServerAgent notifies:
// gating on currentType alone would miss retyping an application AWAY from
// server_agent, which is precisely when the agent must be told to tear its
// router down and stop managing specs it no longer owns.
//
// Per THE RULE on notifyRuntimeChanged it deliberately does NOT ask whether
// the write touched a runtime-RELEVANT field: an edit that only changes, say,
// the application's weight notifies too.
//
// Best-effort like every other notifyRuntimeChanged call site: it returns no
// error and must never turn a successful write into a failed request.
func (s *Service) notifyRuntimeChangedForApplication(serverID, previousType, currentType string) {
	if previousType != routing.ProviderServerAgent && currentType != routing.ProviderServerAgent {
		return
	}
	s.notifyRuntimeChanged(serverID)
}

// notifyRuntimeChangedForMapping is notifyRuntimeChanged for the MAPPING
// write paths (CreateMapping / UpdateMapping / DeleteMapping and the
// reconcileApplicationModels sync in service_applications.go) -- row 3 of THE
// RULE's list on notifyRuntimeChanged. A mapping is a runtime-config input
// through its owning application's specs: AgentRuntimeSpecDTO's
// Model/UpstreamModel ARE the mapping's gateway_model_name/app_model_name,
// and the agent has no other source for them. So renaming a mapping rewrites
// the agent's document, and until this existed nothing said so: inference
// under the new model name 404s at the agent's router for up to a minute
// (the 60 s runtimePollInterval backstop) while the old name still routes.
// Deleting a mapping cascades its spec, its GPU rows and its co-residency
// pairs at the store layer, so it removes a whole spec from the document.
//
// owningApplicationType is the type of the application the mapping belongs
// to; only routing.ProviderServerAgent notifies. A mapping write on an
// ordinary upstream application is no part of any runtime-config document and
// must not push one.
//
// ONE type, not the previous/current pair notifyRuntimeChangedForApplication
// needs, because the retype/move mirror of that case is not expressible: a
// mapping has no type of its own, and UpdateMappingRequest has no
// application_id -- no portal path reassigns ModelMapping.ApplicationID, so a
// mapping cannot move between applications (and therefore never leaves a
// server_agent application behind). Should a move ever be added, it becomes
// exactly the retype-away case and needs BOTH sides here, for the same
// reason: the losing application's agent must be told too.
//
// Per THE RULE it deliberately does not ask whether the write touched
// gateway_model_name/app_model_name specifically, nor whether the mapping
// currently HAS a spec (which is what decides whether the document really
// changes today -- CreateMapping and the sync's create/disable writes provably
// cannot change it, since a brand-new mapping has no spec row and the document
// never reads mapping.Status). Both of those are the same second copy of
// AgentRuntimeConfig's derivation the field allow-list would be, so the gate
// stays on the row and its application's type alone.
//
// Best-effort like every other notifyRuntimeChanged call site.
func (s *Service) notifyRuntimeChangedForMapping(serverID, owningApplicationType string) {
	if owningApplicationType != routing.ProviderServerAgent {
		return
	}
	s.notifyRuntimeChanged(serverID)
}

// --- Task 7: agent runtime-config assembly ----------------------------------

// AgentRuntimeSpecGPUDTO is one per-GPU VRAM row inside a runtime-config spec
// (GET /api/agent/v1/runtime-config). VRAMMB is the single number the agent
// needs: the MEASURED value when known, else the operator's ESTIMATE -- see
// AgentRuntimeConfig's VRAM-selection rule. 0 means unknown either way,
// mirroring RuntimeSpecGPUDTO's two source fields.
type AgentRuntimeSpecGPUDTO struct {
	Index  int `json:"index"`
	VRAMMB int `json:"vram_mb"`
}

// AgentRuntimeSpecDTO is one ENABLED launch spec inside the runtime-config
// document -- everything the agent needs to exec and supervise the process.
// Model/UpstreamModel fold in the owning ModelMapping's two model-name
// fields (gateway_model_name / app_model_name), since the agent has no other
// way to learn them. Env values legitimately carry ${AGENT_ENV:NAME}/${PORT}/
// ${MODEL} placeholders the agent resolves locally at launch time -- this
// DTO, like RuntimeSpecDTO, passes them through untouched: never validated or
// rewritten here.
type AgentRuntimeSpecDTO struct {
	ID                          string                   `json:"id"`
	Model                       string                   `json:"model"`
	UpstreamModel               string                   `json:"upstream_model"`
	Binary                      string                   `json:"binary"`
	Args                        []string                 `json:"args"`
	Env                         map[string]string        `json:"env"`
	WorkDir                     string                   `json:"work_dir,omitempty"`
	GPUs                        []AgentRuntimeSpecGPUDTO `json:"gpus"`
	ListenPort                  int                      `json:"listen_port"`
	HealthPath                  string                   `json:"health_path"`
	HealthTimeoutSeconds        int                      `json:"health_timeout_seconds"`
	StartupTimeoutSeconds       int                      `json:"startup_timeout_seconds"`
	IdleTimeoutSeconds          int                      `json:"idle_timeout_seconds"`
	AdmissionWaitTimeoutSeconds int                      `json:"admission_wait_timeout_seconds"`
	Pinned                      bool                     `json:"pinned"`
	// SetVisibleDevices tells the agent to set the vendor-appropriate GPU
	// visibility variable for this spec's child from the GPUs above. The
	// gateway is hardware-agnostic and never resolves WHICH variable that
	// is — only the agent knows what stack the host runs.
	SetVisibleDevices bool `json:"set_visible_devices"`
	// VisibleDevicesMode tells the agent whether to enforce visibility via the
	// env variable ("env") or to leave it to a ${..._DEVICES} placeholder the
	// operator put in Args ("args"). Unlike api_flavors/responses_mode, the
	// agent NEEDS this, so it DOES cross the wire.
	VisibleDevicesMode string `json:"visible_devices_mode"`
	AdminState         string `json:"admin_state"`
	// APIToken is the DECRYPTED upstream token the agent substitutes into the
	// child's ${API_TOKEN} for this spec, or "" when the mode is off or no token
	// is set (see resolvePushToken; the agent-side runtime.Spec pairs this exact
	// json tag). Unlike every other field here it is a SECRET in the
	// clear: it rides the already-authenticated agent channel, but that channel
	// is only encrypted when the gateway URL is https. When it is not, this value
	// travels in clear -- the operator-facing insecure-transport warning is
	// surfaced in the portal UI; portal.Service adds no server-side guard of its
	// own here because it does not hold the gateway public URL.
	APIToken string `json:"api_token"`
}

// AgentGPUBudgetDTO is one per-GPU VRAM budget row inside the runtime-config
// document -- mirrors GPUBudgetDTO but drops the portal-only drift-detector
// fields (expected_uuid/expected_name) the agent has no use for.
type AgentGPUBudgetDTO struct {
	Index    int `json:"index"`
	BudgetMB int `json:"budget_mb"`
}

// AgentRuntimeConfigDTO is the desired agent-managed runtime state for ONE
// server (GET /api/agent/v1/runtime-config, spec §11): which model processes
// may run, with what command line, on which GPUs, which pairs may be
// co-resident, and the per-GPU VRAM budgets. ETag is a sha256 hex digest over
// the document with ETag itself blanked (agentRuntimeConfigETag) -- the
// agent's conditional-GET validator, and (per the design spec) doubles as the
// WS push / file-mode schema version.
type AgentRuntimeConfigDTO struct {
	RouterListen int                   `json:"router_listen"`
	MaxProcesses int                   `json:"max_processes"`
	GPUBudgets   []AgentGPUBudgetDTO   `json:"gpu_budgets"`
	Specs        []AgentRuntimeSpecDTO `json:"specs"`
	Coresident   [][2]string           `json:"coresident"`
	ETag         string                `json:"etag"`
}

// AgentRuntimeConfig derives serverID's runtime-config document -- the server
// the caller's agent token is bound to. The caller (the gateway handler) has
// already resolved serverID from that token via authenticateAgent; there is
// deliberately no parameter here that could redirect the lookup, mirroring
// AgentProxyRoutes.
//
// RouterListen is the Port of serverID's server_agent Application (the design
// decision: one Application row per server backs the agent's single router
// port -- see the agent-runtime-manager spec §2). A server with no
// server_agent application -- the agent polling before anything is
// configured -- gets the fully empty document: no router to listen on means
// nothing else in the document is meaningful either, the same safe-empty
// posture as AgentProxyRoutes' out-of-scope case.
//
// Only ENABLED specs are included (a disabled spec is nothing the agent
// should ever run). Coresidency pairs are stored as MAPPING id pairs
// (CoResidencyRule); this method translates each to a SPEC id pair, dropping
// any pair whose either side has no enabled spec -- a pair pointing at a
// disabled or missing spec is meaningless to the agent, which only ever sees
// spec ids, never mapping ids.
//
// Per-GPU VRAMMB: the measured value when known (non-zero), else the
// operator's estimate -- see agentRuntimeSpecDTO. 0 means unknown either way.
//
// The empty document means exactly one thing: "this server is genuinely not
// running anything managed" -- an empty serverID, no such AIServer row
// (store.ErrNotFound), or no server_agent application. EVERY OTHER store
// error is propagated, exactly as the ApplicationsByServer read three lines
// below already does.
//
// That split is the C1 fix from the final review round, and the reasoning is
// worth keeping because the earlier shape looked safer than it was. This
// used to collapse ANY AIServerByID error -- a dropped Postgres connection, a
// SQLite contention window, a context deadline -- into the SAME value the
// genuinely-empty case returns, WITH err == nil, on the stated rationale that
// "an agent must never see a raw 500 for a token whose server row briefly
// failed to read". The consumer needs the exact opposite:
//
//   - On the HTTP path, a fabricated 200 + empty document is well-formed and
//     carries a valid, DIFFERENT ETag, so the agent's GatewaySource.Load
//     takes its StatusOK branch, overwrites its on-disk cache with the empty
//     document, tears down the bound router listener (StartRouter(0)) and
//     drains EVERY running spec. A 500 is the safe answer: Load's default
//     branch keeps the last known-good config, so a blip costs one skipped
//     poll instead of every model on the server.
//   - On the WS push path it is worse: PushRuntimeConfig bounds this call
//     with pushRuntimeConfigTimeout, so a slow store yielded
//     context.DeadlineExceeded -> the empty document -> err == nil, which
//     walked straight through that function's own never-push-on-error guard
//     and marshalled the teardown onto the wire with no HTTP status involved
//     at all.
//
// store.ErrNotFound is a reliable discriminator: scanAIServer maps
// sql.ErrNoRows to it and MemoryStore.AIServerByID returns it directly, so
// "no such server" is never confused with "the read failed".
func (s *Service) AgentRuntimeConfig(ctx context.Context, serverID string) (AgentRuntimeConfigDTO, error) {
	if serverID == "" || s.routes == nil {
		return agentRuntimeConfigDTO(nil, nil, nil, 0, 0), nil
	}
	server, err := s.routes.AIServerByID(ctx, serverID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The genuinely-empty case: no such server row. Same safe empty
			// document as "no server_agent application", and the same
			// err == nil.
			return agentRuntimeConfigDTO(nil, nil, nil, 0, 0), nil
		}
		return AgentRuntimeConfigDTO{}, err
	}
	apps, err := s.routes.ApplicationsByServer(ctx, serverID)
	if err != nil {
		return AgentRuntimeConfigDTO{}, err
	}
	// First match wins. That is not an arbitrary pick: it is DETERMINISTIC
	// only because of the "at most one server_agent application per AI
	// server" invariant. Without it this loop would silently choose by id
	// ordering (ids are random hex, and the store orders by id), so runtime
	// configuration edited under the other application would persist and
	// never reach the agent. Do not relax the invariant without giving this
	// lookup a real tie-break.
	//
	// What actually backs that invariant, per driver -- stated precisely
	// because it is NOT two layers everywhere:
	//
	//   - All three drivers: the portal gate on both write paths
	//     (CreateApplication/UpdateApplication ->
	//     ErrServerAgentApplicationExists). This is the only layer a future
	//     write path that bypasses the portal service escapes, and it is not
	//     race-free -- it reads, releases, then writes (see
	//     serverAgentApplicationExistsOnServer).
	//   - memory: MemoryStore.serverAgentApplicationExistsLocked, always.
	//   - sqlite/postgres: migration 68's partial unique index, but ONLY on a
	//     database that held no duplicates when migration 68 ran. The
	//     migration deliberately SKIPS index creation when duplicates already
	//     exist, and records version 68 anyway, so the skip is never retried.
	//     Such a database has the portal gate alone -- the same single-layer
	//     posture as a store with no constraint at all -- and this lookup IS
	//     order-dependent there until the operator deletes the extra
	//     application. That state is only reachable on a pre-invariant
	//     development database of this branch, because 'server_agent' is not
	//     writable in any released version; no request can create it now.
	var agentApp *routing.Application
	for i := range apps {
		if apps[i].Type == routing.ProviderServerAgent {
			agentApp = &apps[i]
			break
		}
	}
	if agentApp == nil {
		return agentRuntimeConfigDTO(nil, nil, nil, 0, 0), nil
	}
	budgets, err := s.routes.ServerGPUBudgets(ctx, serverID)
	if err != nil {
		return AgentRuntimeConfigDTO{}, err
	}
	mappings, err := s.routes.MappingsByApplication(ctx, agentApp.ID)
	if err != nil {
		return AgentRuntimeConfigDTO{}, err
	}
	mappingByID := make(map[string]routing.ModelMapping, len(mappings))
	for _, m := range mappings {
		mappingByID[m.ID] = m
	}
	specs, err := s.routes.RuntimeSpecsByApplication(ctx, agentApp.ID)
	if err != nil {
		return AgentRuntimeConfigDTO{}, err
	}
	specIDByMapping := make(map[string]string, len(specs))
	specDTOs := make([]AgentRuntimeSpecDTO, 0, len(specs))
	for _, spec := range specs {
		if !spec.Enabled {
			continue
		}
		mapping, ok := mappingByID[spec.MappingID]
		if !ok {
			// An orphaned spec (its mapping was deleted out from under it, if
			// that is ever possible): skip rather than emit a half-populated
			// entry with no model name.
			continue
		}
		gpus, err := s.routes.RuntimeSpecGPUs(ctx, spec.ID)
		if err != nil {
			return AgentRuntimeConfigDTO{}, err
		}
		specDTO, err := agentRuntimeSpecDTO(spec, mapping, gpus)
		if err != nil {
			return AgentRuntimeConfigDTO{}, err
		}
		// The ONLY place the gateway decrypts the per-mapping token, and it does
		// so fail-closed (see resolvePushToken). agentApp is guaranteed non-nil
		// here -- the builder returns the empty document above when it is nil.
		specDTO.APIToken = s.resolvePushToken(spec, *agentApp)
		specIDByMapping[spec.MappingID] = spec.ID
		specDTOs = append(specDTOs, specDTO)
	}
	rules, err := s.routes.CoResidencyRulesByApplication(ctx, agentApp.ID)
	if err != nil {
		return AgentRuntimeConfigDTO{}, err
	}
	coresident := make([][2]string, 0, len(rules))
	for _, rule := range rules {
		specA, okA := specIDByMapping[rule.MappingAID]
		specB, okB := specIDByMapping[rule.MappingBID]
		if !okA || !okB {
			continue
		}
		coresident = append(coresident, [2]string{specA, specB})
	}
	return agentRuntimeConfigDTO(specDTOs, coresident, budgets, agentApp.Port, server.RuntimeMaxProcesses), nil
}

// resolvePushToken returns the DECRYPTED upstream token to inject into the child
// for this spec's mode, or "" for off / app-unset. It is the ONLY place the
// gateway decrypts the runtime-spec token, and it NEVER stores or logs the
// plaintext -- the returned value is used only as the AgentRuntimeSpecDTO.APIToken
// field pushed over the already-authenticated agent channel. On ANY decrypt
// failure it returns "" (fail-closed): the agent then hard-errors at launch on an
// unresolved ${API_TOKEN}, rather than the child booting with a garbled/partial
// secret.
//
// https note: that agent channel is only encrypted when the gateway URL is https.
// When it is not, this decrypted token travels in clear. portal.Service does not
// currently hold the gateway public URL, so it adds no server-side guard of its
// own here; the operator-facing insecure-transport warning is surfaced in the
// portal UI instead.
func (s *Service) resolvePushToken(spec routing.RuntimeSpec, app routing.Application) string {
	mode := spec.APITokenMode
	if mode == "" {
		mode = string(routing.RuntimeAPITokenModeApp)
	}
	var sealed string
	switch routing.RuntimeAPITokenMode(mode) {
	case routing.RuntimeAPITokenModeOff:
		return ""
	case routing.RuntimeAPITokenModeSet, routing.RuntimeAPITokenModeRandom:
		sealed = spec.APIToken
	default: // app, and any unrecognized mode -> app, matching SpecUpstreamAuth
		sealed = app.APIToken
	}
	if sealed == "" {
		return ""
	}
	tok, err := capture.OpenSecret(s.cipher, sealed)
	if err != nil {
		return "" // fail-closed; never log the value
	}
	return tok
}

// agentRuntimeSpecDTO builds one AgentRuntimeSpecDTO from a stored (already
// confirmed enabled) spec, its owning mapping (for Model/UpstreamModel), and
// its GPU rows. Mirrors runtimeSpecDTO's opaque-JSON handling: a corrupt
// stored Args/Env is a store-data problem, not a client-input one, but still
// surfaces as the matching domain sentinel rather than a raw JSON error.
//
// VRAM_LOCKED IS THE OPERATOR'S ESCAPE FROM THE MEASUREMENT RATCHET, and the
// per-GPU choice below is what makes it one. Read that loop first, because it
// is closed and one-way: the agent measures its own child, the write-back
// stores it, this function prefers measured over estimate, the agent reads it
// back AS THE SPEC'S OWN DECLARED DEMAND, and Admit's rule 3 answers a demand
// that exceeds its GPU's budget on its own with a TERMINAL not_permitted.
//
// A 24 GB card budgeted at 20000 MB for headroom, an 18000 MB estimate that
// served fine, and one 22000 MB measurement (llama.cpp with a large KV cache)
// is the whole scenario: from then on every start of a model that had been
// working is refused, with no operator action having occurred anywhere. And it
// could not be undone -- PutRuntimeSpec deliberately copies the stored measured
// value forward and ignores what the request sends, the write-back skips
// values <= 0, and the spec never runs again so no newer measurement is ever
// taken. Raising the budget past what the card physically holds is a
// capitulation rather than a lever, and deleting and re-adding the GPU row
// across two saves is not something an operator can be expected to find.
//
// So vram_locked stops the gateway SERVING the measurement, not merely the
// agent WRITING it: locked, the operator's own estimate is authoritative in
// both directions. That is the plain reading of "locked", it is the only
// reading under which the flag helps someone staring at a spec that will not
// start, and it keeps the agent the owner of the measured number -- the
// operator chooses whether to be GOVERNED by it, never what it says. The
// measurement itself stays on file and stays visible in the portal, because it
// is the evidence that explains why the operator had to intervene.
func agentRuntimeSpecDTO(spec routing.RuntimeSpec, mapping routing.ModelMapping, gpus []routing.RuntimeSpecGPU) (AgentRuntimeSpecDTO, error) {
	var args []string
	if err := json.Unmarshal([]byte(spec.Args), &args); err != nil {
		return AgentRuntimeSpecDTO{}, ErrRuntimeSpecArgsInvalid
	}
	if args == nil {
		args = []string{}
	}
	env := map[string]string{}
	if err := json.Unmarshal([]byte(spec.Env), &env); err != nil {
		return AgentRuntimeSpecDTO{}, ErrRuntimeSpecEnvInvalid
	}
	if env == nil {
		env = map[string]string{}
	}
	gpuDTOs := make([]AgentRuntimeSpecGPUDTO, 0, len(gpus))
	for _, g := range gpus {
		// Measured wins -- unless the operator locked the spec's VRAM, in
		// which case their estimate does. See the doc above for why the lock
		// has to reach this line and not only the write-back.
		vram := g.VRAMMeasuredMB
		if spec.VRAMLocked || vram == 0 {
			vram = g.VRAMEstimateMB
		}
		gpuDTOs = append(gpuDTOs, AgentRuntimeSpecGPUDTO{Index: g.GPUIndex, VRAMMB: vram})
	}
	return AgentRuntimeSpecDTO{
		ID:                          spec.ID,
		Model:                       mapping.GatewayModelName,
		UpstreamModel:               mapping.AppModelName,
		Binary:                      spec.Binary,
		Args:                        args,
		Env:                         env,
		WorkDir:                     spec.WorkDir,
		GPUs:                        gpuDTOs,
		ListenPort:                  spec.ListenPort,
		HealthPath:                  spec.HealthPath,
		HealthTimeoutSeconds:        spec.HealthTimeoutSeconds,
		StartupTimeoutSeconds:       spec.StartupTimeoutSeconds,
		IdleTimeoutSeconds:          spec.IdleTimeoutSeconds,
		AdmissionWaitTimeoutSeconds: spec.AdmissionWaitTimeoutSeconds,
		Pinned:                      spec.Pinned,
		SetVisibleDevices:           spec.SetVisibleDevices,
		VisibleDevicesMode:          string(spec.VisibleDevicesMode),
		AdminState:                  spec.AdminState,
	}, nil
}

// agentRuntimeConfigDTO assembles the wire DTO, normalizing every
// collection-shaped field to non-nil (a nil slice must never marshal as JSON
// null -- this exact defect was caught twice already on this branch) before
// computing the ETag, so an empty configuration gets a STABLE etag across
// calls instead of one that depends on which internal slice happened to be
// nil this time.
func agentRuntimeConfigDTO(specs []AgentRuntimeSpecDTO, coresident [][2]string, budgets []routing.ServerGPUBudget, routerListen, maxProcesses int) AgentRuntimeConfigDTO {
	if specs == nil {
		specs = []AgentRuntimeSpecDTO{}
	}
	if coresident == nil {
		coresident = [][2]string{}
	}
	gpuBudgets := make([]AgentGPUBudgetDTO, 0, len(budgets))
	for _, b := range budgets {
		gpuBudgets = append(gpuBudgets, AgentGPUBudgetDTO{Index: b.GPUIndex, BudgetMB: b.BudgetMB})
	}
	dto := AgentRuntimeConfigDTO{
		RouterListen: routerListen,
		MaxProcesses: maxProcesses,
		GPUBudgets:   gpuBudgets,
		Specs:        specs,
		Coresident:   coresident,
	}
	dto.ETag = agentRuntimeConfigETag(dto)
	return dto
}

// agentRuntimeConfigETag hashes dto with its own ETag field blanked (so the
// etag never depends on its own previous value), mirroring
// agentProxyRoutesETag.
func agentRuntimeConfigETag(dto AgentRuntimeConfigDTO) string {
	dto.ETag = ""
	raw, _ := json.Marshal(dto)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// --- Task 9: file-mode runtime report view ----------------------------------

// ServerRuntimeReportViewDTO is the GET /api/portal/servers/{id}/runtime/report
// body: the operator-facing, read-only view of a file-mode agent's latest
// reported runtime configuration (server_runtime_reports) -- the same
// available/collected_at/report shape as the hardware panel (hardwareDTO in
// the gateway package). Report is the gateway ingest layer's already
// sanitized+redacted canonical blob, passed through opaquely (this method
// never parses it). AgentVersion/AgentFeatures are read from the server's
// LATEST telemetry row regardless of whether a runtime report has ever been
// stored, so a later portal task can render a feature-mismatch banner
// without a new endpoint.
type ServerRuntimeReportViewDTO struct {
	Available     bool            `json:"available"`
	CollectedAt   string          `json:"collected_at,omitempty"`
	UpdatedAt     string          `json:"updated_at,omitempty"`
	Report        json.RawMessage `json:"report,omitempty"`
	AgentVersion  string          `json:"agent_version"`
	AgentFeatures []string        `json:"agent_features"`
}

// ServerRuntimeReportView returns serverID's latest file-mode runtime report
// (Available:false when none has ever been stored -- not an error, mirroring
// ServerHardware's absent-read contract) plus agent_version/agent_features.
// authorizeServer gates the whole read (404-no-leak, same collapse as every
// other server-scoped portal read).
func (s *Service) ServerRuntimeReportView(ctx context.Context, principal auth.Token, serverID string) (ServerRuntimeReportViewDTO, error) {
	server, err := s.authorizeServer(ctx, principal, serverID)
	if err != nil {
		return ServerRuntimeReportViewDTO{}, err
	}
	dto := ServerRuntimeReportViewDTO{AgentFeatures: []string{}}
	if telemetry, ok, err := s.routes.TelemetryByServer(ctx, server.ID); err != nil {
		return ServerRuntimeReportViewDTO{}, err
	} else if ok {
		dto.AgentVersion = telemetry.AgentVersion
		dto.AgentFeatures = parseRuntimeReportAgentFeatures(telemetry.Capabilities)
	}
	report, ok, err := s.routes.ServerRuntimeReportByServer(ctx, server.ID)
	if err != nil {
		return ServerRuntimeReportViewDTO{}, err
	}
	if !ok {
		return dto, nil
	}
	dto.Available = true
	dto.CollectedAt = report.CollectedAt.UTC().Format(time.RFC3339)
	dto.UpdatedAt = report.UpdatedAt.UTC().Format(time.RFC3339)
	if report.ReportJSON == "" {
		dto.Report = json.RawMessage("null")
	} else {
		dto.Report = json.RawMessage(report.ReportJSON)
	}
	return dto, nil
}

// runtimeReportAgentCapabilities is the tolerant subset of a server's latest
// telemetry capabilities object this method understands -- mirrors the
// gateway ingest layer's agentCapabilitiesReport (agent_ingest.go), kept as a
// SEPARATE local copy rather than an import: portal must not depend on the
// gateway package.
type runtimeReportAgentCapabilities struct {
	Features []string `json:"features"`
}

// parseRuntimeReportAgentFeatures tolerantly extracts the declared feature
// list from a server's stored telemetry capabilities JSON. Absent, empty, or
// malformed input all yield a non-nil empty slice -- never an error and
// never a nil slice (a collection-shaped field must never serialize as JSON
// null).
func parseRuntimeReportAgentFeatures(raw string) []string {
	if raw == "" {
		return []string{}
	}
	var caps runtimeReportAgentCapabilities
	if err := json.Unmarshal([]byte(raw), &caps); err != nil || caps.Features == nil {
		return []string{}
	}
	return caps.Features
}
