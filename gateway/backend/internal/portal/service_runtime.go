// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
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
	// ErrRuntimeSpecAdminStateConflict rejects a benchmark admin-state write
	// (SetBenchmarkRuntimeSpecAdminState, SetBenchmarkRuntimeSpecsAdminState)
	// whose freshly-read admin_state is not the value the caller said it was
	// replacing. This endpoint class is a read-modify-write with no If-Match
	// and no row version, and every restore of a benchmark's force_stopped
	// needs the guard: the speed run's stop-all clear, the VRAM benchmark's
	// deferred restore, and the override-lease reconciler. Without it, a
	// restore after the stop would hand a concurrent operator's override
	// straight back to "". The caller reports the spec as taken over instead,
	// not as restore-failed -- somebody else owns the field now, or the
	// caller's own writes never stored the override or already cleared it (a
	// write can fail before it stores its row, or after).
	ErrRuntimeSpecAdminStateConflict = errors.New("runtime_spec.admin_state_conflict")
	// ErrRuntimeSpecPinnedConflict: SetBenchmarkRuntimeSpecsPinned found a
	// stored pinned other than the expected value, and wrote nothing for that
	// spec.
	ErrRuntimeSpecPinnedConflict = errors.New("runtime_spec.pinned_conflict")
	// ErrRuntimeSpecReplicableConflict: SetBenchmarkRuntimeSpecsReplicable
	// found a stored replicable other than the expected value, and wrote
	// nothing for that spec.
	ErrRuntimeSpecReplicableConflict = errors.New("runtime_spec.replicable_conflict")
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
	// ErrRuntimeSpecTypeInvalid rejects a type that is not one of
	// ""/vllm/llama_cpp/tgi/ollama/stable_diffusion_cpp/audio_cpp/custom
	// (design 2026-09-07; see validRuntimeSpecType). "" is valid --
	// it means auto-detect from Binary (routing.EffectiveRuntimeSpecType) --
	// so this is the same "empty is fine, everything else must be a known
	// value" shape as validVisibleDevicesMode, not validRuntimeAPITokenMode's
	// caller-normalizes-empty-first shape.
	ErrRuntimeSpecTypeInvalid = errors.New("runtime_spec.type_invalid")
	// ErrRuntimeSpecMetricsPathInvalid / ErrRuntimeSpecContextProbePathInvalid
	// reject a metrics_path / context_probe_path override that is not a SAFE
	// RELATIVE PATH (safeRelativeProbePath): non-empty and either not rooted at
	// a single "/" (e.g. "@evil:9999/x", "http://evil"), protocol-relative
	// ("//evil"), carrying a scheme ("://"), or containing whitespace/control
	// bytes. The agent builds its probe URL by concatenating the override onto
	// "http://127.0.0.1:PORT"; a value like "@evil:9999/x" would re-parse the
	// port digits as userinfo and resolve Host to the ATTACKER, turning the
	// agent's loopback probe into an outbound (SSRF) request. Rejected at the
	// portal before it can ever reach the agent. HTTP 400.
	ErrRuntimeSpecMetricsPathInvalid      = errors.New("runtime_spec.metrics_path_invalid")
	ErrRuntimeSpecContextProbePathInvalid = errors.New("runtime_spec.context_probe_path_invalid")
	// ErrRuntimeSpecResponsesLiveTimingsUnsupported rejects an EXPLICIT
	// responses_live_timings_enabled:true on a spec whose EFFECTIVE kind
	// (routing.EffectiveRuntimeSpecType: the explicit Type when set, else
	// detected from the binary's basename) is not live-timings capable. HTTP
	// 400, and the message names that effective kind -- which may be a kind
	// the caller never typed, because an empty Type means the binary decided.
	//
	// Refused rather than stored as false because a 200 that stores something
	// other than what it was asked to store is a write that lies about its
	// result. An ABSENT field is never this error: on a first write it takes
	// the kind-dependent default, on a later save it keeps the stored value,
	// and on a document whose kind cannot honour the flag it is CLEARED --
	// which overrides nothing the caller said, since they said nothing.
	//
	// 400 in EVERY shape, and one sentinel is therefore enough here. The
	// application surface splits its refusal (ErrApplication...Unsupported,
	// 400, vs ErrApplication...Conflict, 409) by whether the request supplied
	// the offending type; a spec write is a full document that always states
	// its own Type and Binary, so the refused kind is always one this body
	// supplied -- typed, or named by the binary it is detected from. There is
	// no spec shape whose refusal rests on state the request left alone.
	ErrRuntimeSpecResponsesLiveTimingsUnsupported = errors.New("runtime_spec.responses_live_timings_unsupported")
	// ErrRuntimeSpecConfigFilesInvalid rejects a config_files list that breaks
	// any of validateRuntimeSpecConfigFiles' rules: a name that is not a single
	// path segment, a repeated name, more than runtimeSpecConfigFilesMax files,
	// a file over runtimeSpecConfigFileMaxBytes, or content carrying a secret
	// placeholder (${API_TOKEN} / ${AGENT_ENV:...}). HTTP 400, and the message
	// names the offending file and rule -- never its content, which can hold
	// the very secret that tripped the check.
	ErrRuntimeSpecConfigFilesInvalid = errors.New("runtime_spec.config_files_invalid")
	// ErrRuntimeSpecInstancesInvalid rejects a min_instances above
	// max_instances when both are set (> 0): a floor of warm instances the cap
	// never allows; and either count above maxRuntimeSpecInstances, more
	// instances than the agent can address. Checked whether or not the spec is
	// replicable, so a setting that can never be met is not stored to take
	// effect the day the spec is switched to replicable. HTTP 400.
	ErrRuntimeSpecInstancesInvalid = errors.New("runtime_spec.instances_invalid")
	// ErrRuntimeSpecInstancesNeedEphemeralPort rejects a replicable spec with a
	// fixed listen_port: every instance is its own process on its own port, so
	// a replicable spec has to leave the port to the agent (0). The agent would
	// otherwise demote such a spec to its one instance, a setting that quietly
	// does something else. HTTP 400.
	ErrRuntimeSpecInstancesNeedEphemeralPort = errors.New("runtime_spec.instances_need_ephemeral_port")
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
	// ErrCoResidencyModeInvalid rejects a SetCoResidency whose mode is
	// neither routing.CoResidencyModeOptIn nor routing.CoResidencyModeOptOut.
	// (An EMPTY mode is not invalid: it means "leave the stored mode alone".)
	// HTTP 400, checked before any store call like every other validation here.
	ErrCoResidencyModeInvalid = errors.New("runtime_coresidency.mode_invalid")
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

// runtimeSpecHealthPathDefault is the health path a spec gets when it names
// none, by the spec's EFFECTIVE type (explicit, or detected from the binary).
// sd-server answers GET /health with 404 (measured), and the agent kills a
// child whose health path keeps failing once startup_timeout_seconds
// elapses, so the stock default would make every sd-server spec that leaves
// the field empty unusable. /v1/models is an unconditional 200 on that
// server, registered only after the model has loaded, with no file-path
// dependency (verified against leejet/stable-diffusion.cpp; see
// docs/architecture/cross-cutting/agent-runtime-manager.md).
func runtimeSpecHealthPathDefault(t routing.RuntimeSpecType) string {
	if t == routing.RuntimeSpecTypeStableDiffusionCpp {
		return "/v1/models"
	}
	return defaultRuntimeSpecHealthPath
}

// runtimeSpecEnvKeyPattern matches a shell-style environment variable name:
// upper-case letters, digits, underscore, not starting with a digit. Values
// are unrestricted (they legitimately carry ${AGENT_ENV:NAME}/${PORT}/${MODEL}
// placeholders the agent resolves at launch time — never validated or
// rewritten here; see the RuntimeSpecDTO.Env doc below).
var runtimeSpecEnvKeyPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

const (
	// runtimeSpecConfigFilesMax and runtimeSpecConfigFileMaxBytes bound a
	// spec's config files (#257). They keep the opaque config_files column and
	// the runtime-config document the agent polls small: 16 files of at most
	// 64 KiB each is a megabyte at the very worst, far above any real model
	// server config.
	runtimeSpecConfigFilesMax     = 16
	runtimeSpecConfigFileMaxBytes = 64 << 10
)

// runtimeSpecConfigFileSecretPlaceholders are the two placeholders that
// resolve to a secret at launch time: the per-spec API token and an agent
// environment variable. Config files are written to disk in the clear, so
// neither may appear in their content (see validateRuntimeSpecConfigFiles).
var runtimeSpecConfigFileSecretPlaceholders = []string{"${API_TOKEN}", "${AGENT_ENV:"}

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
	// only signal GetRuntimeSpec ever uses for "not configured". The document
	// is then synthesized: ID is absent and MappingID is set; the spec's own
	// fields are zero values (GPUs/Args/Env/APIFlavors/ConfigFiles non-nil but
	// empty, both endpoint modes ""), except VisibleDevicesMode ("env") and
	// APITokenMode/APITokenHeaderSource ("app"), which carry their defaults;
	// AppAPITokenSet/AppAPITokenHeader echo the parent application; and the
	// resolved echoes are computed from that empty spec: EffectiveType is
	// "custom" (what an empty Binary detects as), so ResolvedMetricsPath and
	// ResolvedContextProbePath are custom's empty defaults.
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
	// SetVisibleDevices turns this spec's GPU list from a declaration into
	// an enforcement — see routing.RuntimeSpec.SetVisibleDevices, and
	// PutRuntimeSpec for the two combinations this API refuses.
	SetVisibleDevices bool                `json:"set_visible_devices"`
	GPUs              []RuntimeSpecGPUDTO `json:"gpus"`
	// RAMEstimateMB is the operator's estimate of this spec's resident host
	// RAM in MB; RAMMeasuredMB is the agent's measurement of it (read-only on
	// the wire: PutRuntimeSpecRequest has no counterpart, the store keeps the
	// stored value across every write). RAM is host-scoped, so both are single
	// scalars on the spec rather than per-GPU rows like the VRAM pair above. 0
	// is the UNKNOWN sentinel -- no estimate, nothing measured yet -- never
	// "needs no RAM", and both keys are always present on the wire.
	RAMEstimateMB int `json:"ram_estimate_mb"`
	RAMMeasuredMB int `json:"ram_measured_mb"`
	// Replicable / MinInstances / MaxInstances / InstanceParallelism are the
	// spec's instance settings (#340; routing.RuntimeSpec carries their
	// meaning): whether the agent may run several instances of it, how many it
	// keeps warm (0 = none), how many it may run (0 = unbounded) and how many
	// requests one instance serves at once (0 = 1). Operator-owned, applied
	// verbatim from PutRuntimeSpecRequest like the fields above.
	Replicable          bool `json:"replicable"`
	MinInstances        int  `json:"min_instances"`
	MaxInstances        int  `json:"max_instances"`
	InstanceParallelism int  `json:"instance_parallelism"`
	// StartupMeasuredMS is the agent's measured startup time of the spec in ms
	// (0 = nothing measured yet), and EffectiveIdleTimeoutSeconds is the idle
	// timeout the runtime-config document pushes for the spec: for a
	// replicable spec on a server whose agent declares runtime_instances (or
	// has not declared its features yet, while its document is held -- see
	// AgentRuntimeConfigDTO.AwaitsAgentFeatures) derived from that measurement
	// and the mapping's benchmark cold start (effectiveIdleTimeout), otherwise
	// the stored idle_timeout_seconds. Both are READ-ONLY echoes with no
	// PutRuntimeSpecRequest counterpart -- the first is agent-owned and the
	// store keeps it across every write, the second is derived on every read
	// and never stored -- so the portal shows the value the agent gets
	// without re-deriving the gateway's factor and floor itself.
	StartupMeasuredMS           int64 `json:"startup_measured_ms"`
	EffectiveIdleTimeoutSeconds int   `json:"effective_idle_timeout_seconds"`
	// InstancesMinExceedsRealtime warns (it never refuses) that MinInstances is
	// above the real-time concurrency the mapping's audio ramp measured as a
	// ceiling (minInstancesExceedRealtime): the scaler keeps a speech model's
	// instances under that ceiling, and the minimum would hold more warm than one
	// host keeps up with. Read-only and derived on every read, like the two echoes
	// above.
	InstancesMinExceedsRealtime bool `json:"instances_min_exceeds_realtime"`
	// APIFlavors / ResponsesMode / MessagesMode are the per-spec snapshot of
	// the three-state endpoint control (mirrors the same trio on
	// ApplicationDTO), stored explicitly on this spec rather than inherited
	// from the parent server_agent application — see PutRuntimeSpec's "no
	// backend inheritance" contract.
	APIFlavors    []string `json:"api_flavors"`
	ResponsesMode string   `json:"responses_mode"`
	MessagesMode  string   `json:"messages_mode"`
	// ResponsesLiveTimingsEnabled is this spec's own copy of the live-timings
	// opt-in (migration 80) -- stored explicitly on the spec rather than
	// inherited from the parent server_agent application, exactly like the trio
	// above, and for a server_agent model the RESOLVED spec's copy is the one
	// the request path reads. Always the STORED value, which is always a value
	// this spec's EFFECTIVE kind can honour: a PUT that asks for true on a kind
	// that cannot is refused with 400, never stored as false.
	ResponsesLiveTimingsEnabled bool `json:"responses_live_timings_enabled"`
	// VisibleDevicesMode is "env" | "args": how set_visible_devices is
	// enforced. Only meaningful when SetVisibleDevices is on; default "env".
	VisibleDevicesMode string `json:"visible_devices_mode"`
	// APITokenMode is "app"|"set"|"random"|"off": where the upstream API token
	// of this spec's child comes from. Default "app" (the application's token).
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
	// Type is the explicit runtime-server kind ("" | "vllm" | "llama_cpp" |
	// "tgi" | "ollama" | "custom"); see routing.RuntimeSpec.Type. "" means
	// auto-detect from Binary -- EffectiveType below is what that resolves to.
	Type string `json:"type"`
	// MetricsPath / ContextProbePath are the operator's raw overrides (""
	// means "use the type's default"); see routing.RuntimeSpec.
	MetricsPath      string `json:"metrics_path"`
	ContextProbePath string `json:"context_probe_path"`
	// ConfigFiles are the stored config files (#257), always non-nil on the
	// wire ([] when there are none). Carried on the read-back so a loaded
	// document spread through putRequestFromDTO keeps them: a benchmark
	// admin-state write is a full-document upsert and would otherwise clear the
	// operator's files.
	ConfigFiles []ConfigFileDTO `json:"config_files"`
	// EffectiveType / ResolvedMetricsPath / ResolvedContextProbePath are
	// READ-ONLY echoes of routing.EffectiveRuntimeSpecType +
	// routing.DeriveProbePaths -- what the agent will actually use once Type/
	// MetricsPath/ContextProbePath resolve (auto-detection and per-type
	// defaults applied). The portal shows these next to the raw fields above
	// so an operator relying on auto-detect can see the outcome without
	// guessing.
	EffectiveType            string `json:"effective_type"`
	ResolvedMetricsPath      string `json:"resolved_metrics_path"`
	ResolvedContextProbePath string `json:"resolved_context_probe_path"`
	// Role is the spec's derived model role -- one of the routing.ModelRole
	// values ("asr", "tts", "image", "embedding", "reranker", "chat", "other"),
	// never empty: routing.DeriveRole over EffectiveType and the yes verdicts of
	// this spec's mapping. It is the portal-facing twin of AgentRuntimeSpecDTO.Role
	// -- the same derivation over the same two inputs -- so the co-residency
	// editor's per-spec grouping and the agent's role-aware eviction read one
	// value. A READ-ONLY echo like EffectiveType: derived on every read (GET and
	// the PUT response), never stored, and deliberately absent from
	// PutRuntimeSpecRequest. It is NOT ModelDTO.Role, which folds every mapping of
	// a model NAME into its best-ranked role; this is the role of one spec.
	Role string `json:"role"`
}

// ConfigFileDTO is one operator-supplied config file of a runtime spec: a
// single-segment file name and its content. The agent writes the files into an
// agent-owned directory before it launches the child (#257). The content is
// stored and shipped in the clear, which is why validateRuntimeSpecConfigFiles
// refuses the secret placeholders in it.
type ConfigFileDTO struct {
	Name    string `json:"name"`
	Content string `json:"content"`
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
	// SetVisibleDevices turns this spec's GPU list from a declaration into
	// an enforcement — see routing.RuntimeSpec.SetVisibleDevices, and
	// PutRuntimeSpec for the two combinations this API refuses.
	SetVisibleDevices bool                `json:"set_visible_devices"`
	GPUs              []RuntimeSpecGPUDTO `json:"gpus"`
	// RAMEstimateMB is the operator's estimate of this spec's resident host
	// RAM in MB (see RuntimeSpecDTO). Applied verbatim like every field of this
	// full-document request: absent means 0, "no estimate", and clears a stored
	// one. Negative is refused (ErrRuntimeSpecTuningInvalid). The measurement
	// has no request field: it is agent-owned, and the store keeps it.
	RAMEstimateMB int `json:"ram_estimate_mb"`
	// Replicable / MinInstances / MaxInstances / InstanceParallelism: see
	// RuntimeSpecDTO's doc. Applied verbatim like every field of this
	// full-document request -- absent means not replicable and 0 -- and checked
	// by validateRuntimeSpecInstances. The DTO's two read-only echoes beside
	// them (startup_measured_ms, effective_idle_timeout_seconds) have no
	// request field, so a body that carries them sets nothing.
	Replicable          bool `json:"replicable"`
	MinInstances        int  `json:"min_instances"`
	MaxInstances        int  `json:"max_instances"`
	InstanceParallelism int  `json:"instance_parallelism"`
	// APIFlavors / ResponsesMode / MessagesMode: see RuntimeSpecDTO's doc.
	// Absent (empty/"") defaults to both/passthrough — see validateRuntimeSpecRequest;
	// the backend does NOT inherit the parent server_agent application's
	// values; the portal form starts a spec's first write from them instead
	// (runtimeSpecTemplate: Create, and Edit of a spec-less mapping).
	APIFlavors    []string `json:"api_flavors"`
	ResponsesMode string   `json:"responses_mode"`
	MessagesMode  string   `json:"messages_mode"`
	// ResponsesLiveTimingsEnabled: a POINTER inside an otherwise
	// apply-verbatim full-document request, the same exception APIToken below
	// already makes (nil = keep the stored value), and it carries two loads.
	// nil must mean "no opinion", so that a FIRST write can get the
	// kind-dependent default (ON for a llama_cpp spec) while a later save
	// of an existing spec keeps whatever the operator last stored; a plain
	// bool would arrive as false on every full-document PUT and the default
	// could never fire. And nil is what separates a NON-MENTION from an
	// ASSERTION: an explicit true on an effective kind that cannot honour it
	// is refused (ErrRuntimeSpecResponsesLiveTimingsUnsupported), while an
	// absent field on such a kind is simply cleared.
	ResponsesLiveTimingsEnabled *bool `json:"responses_live_timings_enabled,omitempty"`
	// VisibleDevicesMode: see RuntimeSpecDTO's doc. Absent (empty/"")
	// defaults to "env" — see validateRuntimeSpecRequest.
	VisibleDevicesMode string `json:"visible_devices_mode"`
	// APITokenMode / APITokenHeaderSource / APITokenHeader: see
	// RuntimeSpecDTO's doc. Absent (empty/"") defaults to "app" for both mode
	// and header source — see validateRuntimeSpecAPIToken.
	APITokenMode         string `json:"api_token_mode"`
	APITokenHeaderSource string `json:"api_token_header_source"`
	APITokenHeader       string `json:"api_token_header"`
	// APIToken is write-only: nil = keep, "" = clear, value = replace-and-seal.
	// Sealing/rotation happens in the write path (runtimeSpecAPIToken) --
	// validateRuntimeSpecAPIToken only validates the shape of the request
	// around it.
	APIToken *string `json:"api_token"`
	// APITokenRotate, when true under mode "random", forces regeneration on
	// write.
	APITokenRotate bool `json:"api_token_rotate"`
	// Type / MetricsPath / ContextProbePath: see RuntimeSpecDTO's doc. Type
	// is validated by validRuntimeSpecType (empty or one of the five
	// routing.RuntimeSpecType values); MetricsPath/ContextProbePath are
	// freeform and only trimmed -- no shape validation.
	Type             string `json:"type"`
	MetricsPath      string `json:"metrics_path"`
	ContextProbePath string `json:"context_probe_path"`
	// ConfigFiles are the files the agent writes before it launches the child
	// (#257). Like every other field of this full-document request, absent
	// means none: a PUT that omits them clears the stored set. Validated by
	// validateRuntimeSpecConfigFiles.
	ConfigFiles []ConfigFileDTO `json:"config_files,omitempty"`
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
		// Type/MetricsPath/ContextProbePath are zero (""), same as every other
		// unconfigured field; EffectiveType/the resolved paths are still
		// computed from that empty spec (an empty Binary detects as
		// RuntimeSpecTypeCustom, whose defaults are empty paths) so the
		// portal's echoes are never left stale relative to what a first save
		// with no overrides would resolve to.
		emptySpec := routing.RuntimeSpec{}
		effectiveType := routing.EffectiveRuntimeSpecType(emptySpec)
		resolvedMetrics, resolvedContext := routing.DeriveProbePaths(effectiveType, emptySpec.MetricsPath, emptySpec.ContextProbePath)
		role, err := s.runtimeSpecRole(ctx, mapping.ID, effectiveType)
		if err != nil {
			return RuntimeSpecDTO{}, err
		}
		return RuntimeSpecDTO{
			MappingID:                mapping.ID,
			Args:                     []string{},
			Env:                      map[string]string{},
			GPUs:                     []RuntimeSpecGPUDTO{},
			ConfigFiles:              []ConfigFileDTO{},
			APIFlavors:               []string{},
			VisibleDevicesMode:       string(routing.VisibleDevicesModeEnv),
			APITokenMode:             string(routing.RuntimeAPITokenModeApp),
			APITokenHeaderSource:     string(routing.RuntimeAPITokenHeaderSourceApp),
			AppAPITokenSet:           app.APIToken != "",
			AppAPITokenHeader:        app.APITokenHeader,
			EffectiveType:            string(effectiveType),
			ResolvedMetricsPath:      resolvedMetrics,
			ResolvedContextProbePath: resolvedContext,
			Role:                     role,
		}, nil
	}
	gpus, err := s.routes.RuntimeSpecGPUs(ctx, spec.ID)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	dto, err := runtimeSpecDTO(spec, gpus, app, mapping, s.effectiveIdleTimeout(app.ServerID, spec, mapping))
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	return s.withRuntimeSpecRole(ctx, dto)
}

// runtimeSpecRole is the derived role of the spec on mappingID whose effective
// type is effectiveType: routing.DeriveRole over that type and the mapping's yes
// verdicts -- the same call AgentRuntimeConfig makes for the pushed spec, here
// for ONE mapping (one capability read) instead of a batch. A failed read is an
// error, never a role guessed from the type alone: the editor groups -- and bulk
// writes -- by this value, and a verdict-blind guess would file an embedding model
// under chat.
func (s *Service) runtimeSpecRole(ctx context.Context, mappingID string, effectiveType routing.RuntimeSpecType) (string, error) {
	rows, err := s.routes.MappingCapabilities(ctx, mappingID)
	if err != nil {
		return "", err
	}
	return string(routing.DeriveRole(effectiveType, routing.YesCapabilities(rows))), nil
}

// withRuntimeSpecRole returns dto with its read-only Role filled in from its own
// MappingID and EffectiveType. runtimeSpecDTO stays pure (it is also the
// benchmark writers' read-modify-write input, which has no use for a role and
// must not pay a store read for one); the two portal-facing responses -- GET and
// the PUT/benchmark-admin-state write-back -- go through here.
func (s *Service) withRuntimeSpecRole(ctx context.Context, dto RuntimeSpecDTO) (RuntimeSpecDTO, error) {
	role, err := s.runtimeSpecRole(ctx, dto.MappingID, routing.RuntimeSpecType(dto.EffectiveType))
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	dto.Role = role
	return dto, nil
}

// PutRuntimeSpec validates and upserts mappingID's runtime spec (create on
// first write, full-document replace thereafter).
//
// VRAM ownership rule (one rule, both directions): vram_estimate_mb is
// operator-owned — only this method ever writes it. vram_measured_mb is
// agent-owned — only the telemetry write-back (a later task) writes it, so
// this method PRESERVES the stored measured values verbatim for every GPU
// index that already had a row, regardless of what the request carries, and
// starts a brand-new index at 0. Which number the AGENT is told is not decided
// here: agentRuntimeSpecDTO pushes max(measured, estimate), so the operator may
// store any estimate and a measurement can only ever raise the demand, never
// lower it.
func (s *Service) PutRuntimeSpec(ctx context.Context, principal auth.Token, mappingID string, req PutRuntimeSpecRequest) (RuntimeSpecDTO, error) {
	mapping, app, server, err := s.authorizeMapping(ctx, principal, mappingID)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	// Checked AFTER authorization, so a caller with no claim to this mapping
	// learns nothing about the server behind it -- and only on this
	// principal-carrying path, never inside the shared write
	// (putRuntimeSpec, writeRuntimeSpec), which the benchmark run's own
	// writers also go through.
	if s.serverIsBenchmarking(server.ID) {
		return RuntimeSpecDTO{}, ErrRuntimeSpecServerBenchmarking
	}
	return s.putRuntimeSpec(ctx, mapping, app, server, req, "")
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
// releases it. What it deliberately does NOT gate is the run's own writers
// (SetBenchmarkRuntimeSpecAdminState, SetBenchmarkRuntimeSpecsAdminState and
// SetBenchmarkRuntimeSpecsPinned: the run that calls them is the one that took
// the reservation), a write to any OTHER server, or a DELETE -- deleting a spec
// mid-run drains it rather than starting it, and the restore already treats a
// deleted spec as restored.
func (s *Service) serverIsBenchmarking(serverID string) bool {
	return s.benchmarkReserved != nil && s.benchmarkReserved(serverID)
}

// putRuntimeSpec is PutRuntimeSpec's whole body with the AUTHORIZATION
// removed and the resolved mapping/application/server handed in: the shared
// full-document upsert (writeRuntimeSpec) and the notification it owes. Keeping
// both in one place means a second caller cannot end up with a subtly
// different write.
//
// Its callers, and what authorized each: PutRuntimeSpec (the portal
// principal, via authorizeMapping) and SetBenchmarkRuntimeSpecAdminState (the
// benchmark trigger request, gated before the run started -- see that
// method's doc for why the run itself carries no principal). writeRuntimeSpec
// has one more caller, setBenchmarkRuntimeSpecs, the shared body of the two
// batched benchmark writers, which notifies after the batch's last write, once
// for each server a write stored to, instead of once per spec.
//
// expectSpecID is "" for PutRuntimeSpec, which creates the spec on a
// mapping's first write. Every benchmark writer passes the id it resolved the
// spec by, and then the write refuses a spec that is gone or replaced when it
// re-reads the mapping: when that read finds no spec, or one with another id,
// it returns ErrRuntimeSpecNotFound and stores nothing. Without that, a DELETE
// between the writer's read by id and this read by mapping would let the
// upsert create a new spec under a new id -- a pinned one, for a re-pin, which
// the agent would start right after the operator deleted it. The guard
// narrows that window to the gap between the re-read and the upsert, and does
// not close it (see priorRuntimeSpec).
//
// The notification fires exactly when writeRuntimeSpec reports stored: once
// UpsertRuntimeSpec has succeeded, also when the GPU-row write or the
// read-back after it fails. The returned document carries the derived Role
// (withRuntimeSpecRole), which writeRuntimeSpec's batch callers have no use for
// and so never pay a read for.
func (s *Service) putRuntimeSpec(ctx context.Context, mapping routing.ModelMapping, app routing.Application, server routing.AIServer, req PutRuntimeSpecRequest, expectSpecID string) (RuntimeSpecDTO, error) {
	dto, stored, err := s.writeRuntimeSpec(ctx, mapping, app, req, expectSpecID)
	if stored {
		s.notifyRuntimeChanged(server.ID)
	}
	if err != nil {
		return dto, err
	}
	// The write-back is the document the portal commits into the same per-mapping
	// cache a GET fills, so it carries the derived role like a GET does. Read AFTER
	// the notification: a failed read reports the error but the row IS stored and
	// announced, exactly like a failed GPU read-back inside writeRuntimeSpec.
	return s.withRuntimeSpecRole(ctx, dto)
}

// writeRuntimeSpec is the one implementation of the full-document upsert:
// the application-type gate, every request validation and default
// (validateRuntimeSpecRequest), the read of the stored spec with the VRAM
// ownership rule and the expectSpecID guard (priorRuntimeSpec), the row
// (runtimeSpecRow) and its GPU rows (runtimeSpecGPURows).
//
// It never notifies. Every caller owes the notification for what it stored
// (THE RULE on notifyRuntimeChanged), and stored says whether there is one to
// owe: it is true as soon as UpsertRuntimeSpec has succeeded, because the
// upserted row is already part of the runtime-config document, so a failed
// SetRuntimeSpecGPUs or a failed read-back after it reports stored alongside
// its error. An error before the upsert stored nothing.
func (s *Service) writeRuntimeSpec(ctx context.Context, mapping routing.ModelMapping, app routing.Application, req PutRuntimeSpecRequest, expectSpecID string) (dto RuntimeSpecDTO, stored bool, err error) {
	if app.Type != routing.ProviderServerAgent {
		return RuntimeSpecDTO{}, false, ErrRuntimeSpecNotServerAgent
	}
	// Validate everything that can fail BEFORE mutating/persisting anything.
	fields, err := validateRuntimeSpecRequest(req)
	if err != nil {
		return RuntimeSpecDTO{}, false, err
	}
	prior, err := s.priorRuntimeSpec(ctx, mapping.ID, expectSpecID)
	if err != nil {
		return RuntimeSpecDTO{}, false, err
	}
	spec, err := s.runtimeSpecRow(mapping.ID, req, fields, prior)
	if err != nil {
		return RuntimeSpecDTO{}, false, err
	}
	if err := s.routes.UpsertRuntimeSpec(ctx, spec); err != nil {
		return RuntimeSpecDTO{}, false, err
	}
	if err := s.routes.SetRuntimeSpecGPUs(ctx, spec.ID, runtimeSpecGPURows(spec.ID, req.GPUs, prior.measuredByIndex)); err != nil {
		return RuntimeSpecDTO{}, true, err
	}
	storedGPUs, err := s.routes.RuntimeSpecGPUs(ctx, spec.ID)
	if err != nil {
		return RuntimeSpecDTO{}, true, err
	}
	dto, err = runtimeSpecDTO(spec, storedGPUs, app, mapping, s.effectiveIdleTimeout(app.ServerID, spec, mapping))
	return dto, true, err
}

// runtimeSpecFields is a PutRuntimeSpecRequest after
// validateRuntimeSpecRequest: every column of the stored row that the request
// alone decides, trimmed, defaulted and encoded.
type runtimeSpecFields struct {
	binary             string
	adminState         string
	specType           string
	metricsPath        string
	contextProbePath   string
	responsesMode      routing.EndpointMode
	messagesMode       routing.EndpointMode
	flavors            []string
	liveTimingsCapable bool // the spec's EFFECTIVE kind can honour responses_live_timings_enabled
	visibleMode        routing.VisibleDevicesMode
	args               string // JSON
	env                string // JSON
	configFiles        string // JSON array of ConfigFileDTO; "" when there are none
	healthPath         string
	healthTimeout      int
	startupTimeout     int
}

// validateRuntimeSpecRequest runs every request validation of a runtime-spec
// write, in a fixed order, and resolves the request-only columns of the row.
// It reads no store, so a body it refuses has persisted nothing. The order is
// part of the API: a doubly-invalid body reports the first refusal it meets,
// so a check moved within this function or validateRuntimeSpecShape changes
// an answer.
func validateRuntimeSpecRequest(req PutRuntimeSpecRequest) (runtimeSpecFields, error) {
	f, err := validateRuntimeSpecShape(req)
	if err != nil {
		return runtimeSpecFields{}, err
	}
	// Endpoint-mode + flavor validation, defaulting absent fields (spec
	// §5.4/§12: the backend does NOT read the parent app to inherit -- the
	// portal form starts a spec's first write from the parent; the backend
	// only supplies a sane default when a field is absent or empty, on every
	// write). See TestPutRuntimeSpecDoesNotInheritAppModes.
	f.responsesMode, f.messagesMode, err = runtimeSpecEndpointModes(req)
	if err != nil {
		return runtimeSpecFields{}, err
	}
	f.flavors, err = normalizeRuntimeSpecFlavors(req.APIFlavors)
	if err != nil {
		return runtimeSpecFields{}, err
	}
	// The spec's OWN effective kind decides whether the live-timings opt-in
	// can be honest here: the explicit Type when set, else detected from the
	// binary's basename. Same resolver the inference path consults through
	// Target.LiveProgressSpecType, so the portal and the gateway cannot
	// disagree about what actually serves.
	//
	// EffectiveRuntimeSpecType, NOT specType: validRuntimeSpecType accepts ""
	// as a real value ("auto-detect from Binary", not a collapsed default) and
	// LiveTimingsCapableKind("") is false, so asking about the raw type would
	// refuse an explicit true on a {"binary": ".../llama-server"} spec -- the
	// commonest managed configuration there is -- and, in the resolution
	// (runtimeSpecRow), silently default it OFF. It would also refuse the VRAM
	// benchmark's own deferred restore, which replays a stored Type of ""
	// verbatim through putRequestFromDTO alongside an explicit true.
	//
	// An EXPLICIT true on a kind that cannot honour it is refused, naming that
	// kind. Judged over THIS document's kind rather than over a type change:
	// a PUT is a full document and carries no retype signal -- the read of the
	// stored row says a row was there, never that its kind changed -- and a
	// first write asserting true on an incapable kind has to be refused too,
	// though no transition is involved in it at all.
	//
	// POSITION, deliberately: LAST of the request validations. Every check a
	// body could already fail on -- the binary, the tuning values,
	// admin_state, the GPU rows, the env keys, the four visible-devices
	// checks, the four api_token shapes, validRuntimeSpecType, both probe
	// paths (all in validateRuntimeSpecShape), both endpoint modes and the
	// flavors -- runs above this and RETURNS rather than falling through, so
	// this brand-new check can never rewrite the answer to a body that was
	// already invalid for a SHIPPED reason. Two refusals that turn on STATE
	// rather than on the body are above it as well, so they are safe for the
	// same reason without belonging to that list: writeRuntimeSpec's opening
	// server_agent gate on the parent application's type, and PutRuntimeSpec's
	// benchmark-reservation 409, which never enters the shared write at all.
	//
	// Placed beside the type check instead, a doubly-invalid body reported this
	// 400 where it used to report runtime_spec.metrics_path_invalid,
	// runtime_spec.endpoint_mode_invalid or runtime_spec.flavor_invalid.
	//
	// The config_files validation (validateRuntimeSpecConfigFiles, #257) also
	// runs after it, below the args/env encoding, for the same reason: it is
	// newer than every check above, so it cannot rewrite the answer to a body
	// that was already invalid for a shipped reason. The instance settings
	// (validateRuntimeSpecInstances, #340) run after the config files, last of
	// all, for that reason again -- their negative counts included, although
	// those answer with the shipped ErrRuntimeSpecTuningInvalid: checked beside
	// the other tuning values in validateRuntimeSpecShape, a body that also
	// carried a bad admin_state would answer tuning_invalid where it answered
	// admin_state_invalid while encoding/json still dropped the counts unread.
	//
	// Apart from those two, exactly one REQUEST-REFUSING rejection is left
	// after it on the write path: capture.SealSecret's keyless-store failure
	// (capture.ErrKeyRequired -> 400 runtime_spec.api_token_key_required),
	// returned from either token branch of runtimeSpecAPIToken that seals,
	// "random" or "set". It is
	// write-path preparation rather than a request validation, and it is also
	// the ONE place the two surfaces order things differently -- recorded here
	// rather than smoothed over: CreateApplication and UpdateApplication both
	// put their live-timings refusal BELOW their own capture.SealSecret call,
	// so a body pairing an impossible true with an api_token on a keyless store
	// reports the seal failure there and this 400 here. Which of the two such a
	// body gets is cosmetic -- neither path persists anything either way -- and
	// the request-SHAPE validation of the token pair has already run above.
	//
	// The two json.Marshal error returns for args/env below are not a second
	// rejection: no request body can reach them, because a []string and a
	// map[string]string have no shape encoding/json rejects. Every other return
	// after this function -- the store reads and writes, the expectSpecID
	// guard's ErrRuntimeSpecNotFound (the spec a benchmark writer resolved is
	// gone), the secret generation, the DTO mapper's corrupt-stored-row pair --
	// reports a failure of the SERVER or of an already-stored row, never of
	// this request.
	//
	// One sentinel, 400 in every shape. Because the document always carries
	// the Type/Binary the kind is resolved from, the refused kind is always one
	// THIS request supplied, so there is no well-formed-but-conflicting shape
	// here for the application side's 409 sentinel to answer. The stored row
	// is read later (priorRuntimeSpec), for the default-versus-preserve
	// decision only, never here.
	effectiveSpecKind := routing.EffectiveRuntimeSpecType(routing.RuntimeSpec{Type: f.specType, Binary: f.binary})
	f.liveTimingsCapable = routing.LiveTimingsCapableKind(string(effectiveSpecKind))
	if req.ResponsesLiveTimingsEnabled != nil && *req.ResponsesLiveTimingsEnabled && !f.liveTimingsCapable {
		return runtimeSpecFields{}, fmt.Errorf("%w: responses_live_timings_enabled cannot be true for a runtime spec of effective type %q",
			ErrRuntimeSpecResponsesLiveTimingsUnsupported, string(effectiveSpecKind))
	}
	// VisibleDevicesMode: an omitted mode defaults to "env" (today's
	// behavior); a bad value is a LATER task's validation (mode validation
	// is not wired up yet — this resolves the stored typed value only).
	f.visibleMode = routing.VisibleDevicesModeEnv
	if strings.TrimSpace(req.VisibleDevicesMode) != "" {
		f.visibleMode = routing.VisibleDevicesMode(strings.TrimSpace(req.VisibleDevicesMode))
	}
	args := req.Args
	if args == nil {
		args = []string{}
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return runtimeSpecFields{}, ErrRuntimeSpecArgsInvalid
	}
	env := req.Env
	if env == nil {
		env = map[string]string{}
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return runtimeSpecFields{}, ErrRuntimeSpecEnvInvalid
	}
	f.args, f.env = string(argsJSON), string(envJSON)
	f.configFiles, err = validateRuntimeSpecConfigFiles(req.ConfigFiles)
	if err != nil {
		return runtimeSpecFields{}, err
	}
	if err = validateRuntimeSpecInstances(req); err != nil {
		return runtimeSpecFields{}, err
	}
	f.healthPath = strings.TrimSpace(req.HealthPath)
	if f.healthPath == "" {
		f.healthPath = runtimeSpecHealthPathDefault(effectiveSpecKind)
	}
	f.healthTimeout = req.HealthTimeoutSeconds
	if f.healthTimeout == 0 {
		f.healthTimeout = defaultRuntimeSpecHealthTimeoutSeconds
	}
	f.startupTimeout = req.StartupTimeoutSeconds
	if f.startupTimeout == 0 {
		f.startupTimeout = defaultRuntimeSpecStartupTimeoutSeconds
	}
	return f, nil
}

// validateRuntimeSpecShape is the first half of validateRuntimeSpecRequest:
// the binary, the tuning values, admin_state, the GPU rows, the env keys, the
// visible-devices and api_token shapes, the type and both probe paths, in that
// order. It returns the trimmed values it validated.
func validateRuntimeSpecShape(req PutRuntimeSpecRequest) (runtimeSpecFields, error) {
	binary := strings.TrimSpace(req.Binary)
	// Absolute on POSIX *or* on Windows -- the agent's own filepath.IsAbs is
	// the authority and this is its early-feedback mirror; see
	// runtimeSpecBinaryIsAbsolute.
	if !runtimeSpecBinaryIsAbsolute(binary) {
		return runtimeSpecFields{}, ErrRuntimeSpecBinaryRequired
	}
	// ram_estimate_mb joins the non-negative tuning values: a negative RAM
	// estimate has no meaning (0 is "no estimate"), and the later admission
	// arithmetic must never be handed a negative demand. Same refusal as a
	// negative vram_estimate_mb earns on a GPU row, in the sentinel this block
	// already owns.
	if req.ListenPort < 0 || req.HealthTimeoutSeconds < 0 || req.StartupTimeoutSeconds < 0 ||
		req.IdleTimeoutSeconds < 0 || req.AdmissionWaitTimeoutSeconds < 0 || req.RAMEstimateMB < 0 {
		return runtimeSpecFields{}, ErrRuntimeSpecTuningInvalid
	}
	adminState := strings.TrimSpace(req.AdminState)
	switch adminState {
	case "", "force_running", "force_stopped":
	default:
		return runtimeSpecFields{}, ErrRuntimeSpecAdminStateInvalid
	}
	if err := validateRuntimeSpecGPUs(req.GPUs); err != nil {
		return runtimeSpecFields{}, err
	}
	for k := range req.Env {
		if !runtimeSpecEnvKeyPattern.MatchString(k) {
			return runtimeSpecFields{}, ErrRuntimeSpecEnvInvalid
		}
	}
	if err := validateRuntimeSpecVisibleDevices(req); err != nil {
		return runtimeSpecFields{}, err
	}
	if err := validateRuntimeSpecAPIToken(req); err != nil {
		return runtimeSpecFields{}, err
	}
	specType := strings.TrimSpace(req.Type)
	if !validRuntimeSpecType(specType) {
		return runtimeSpecFields{}, ErrRuntimeSpecTypeInvalid
	}
	metricsPath := strings.TrimSpace(req.MetricsPath)
	contextProbePath := strings.TrimSpace(req.ContextProbePath)
	// SSRF guard: an operator-supplied probe-path override is only ever
	// appended to the agent's own "http://127.0.0.1:PORT" loopback base, so it
	// MUST be a safe relative path. Reject anything that could re-anchor the
	// URL's Host (@userinfo, //authority, a scheme) or smuggle whitespace/
	// control bytes -- see safeRelativeProbePath and the two sentinels' docs.
	if !safeRelativeProbePath(metricsPath) {
		return runtimeSpecFields{}, ErrRuntimeSpecMetricsPathInvalid
	}
	if !safeRelativeProbePath(contextProbePath) {
		return runtimeSpecFields{}, ErrRuntimeSpecContextProbePathInvalid
	}
	return runtimeSpecFields{
		binary:           binary,
		adminState:       adminState,
		specType:         specType,
		metricsPath:      metricsPath,
		contextProbePath: contextProbePath,
	}, nil
}

// maxRuntimeSpecInstances is how many instances one replicable spec can have:
// the agent addresses them 0 to routing.MaxInstanceIndex, so a count above it
// names instances that cannot exist.
const maxRuntimeSpecInstances = routing.MaxInstanceIndex + 1

// validateRuntimeSpecInstances checks the four instance settings -- last of
// the request validations, see validateRuntimeSpecRequest for why -- in this
// order: a negative count is ErrRuntimeSpecTuningInvalid like every other
// negative tuning value, a count above maxRuntimeSpecInstances and a floor
// above the cap (both set) are ErrRuntimeSpecInstancesInvalid, and a
// replicable spec with a fixed port is
// ErrRuntimeSpecInstancesNeedEphemeralPort. The agent repairs the same shapes
// when a hand-written file-mode document carries them (a negative count reads
// as 0, the floor is clamped to the cap and to the addressable range, a fixed
// port demotes the spec to one instance), but a repair the operator never sees
// is a setting that silently does something else, so the portal refuses them
// where they are typed.
func validateRuntimeSpecInstances(req PutRuntimeSpecRequest) error {
	if req.MinInstances < 0 || req.MaxInstances < 0 || req.InstanceParallelism < 0 {
		return ErrRuntimeSpecTuningInvalid
	}
	if req.MinInstances > maxRuntimeSpecInstances || req.MaxInstances > maxRuntimeSpecInstances {
		return ErrRuntimeSpecInstancesInvalid
	}
	if req.MaxInstances > 0 && req.MinInstances > req.MaxInstances {
		return ErrRuntimeSpecInstancesInvalid
	}
	if req.Replicable && req.ListenPort != 0 {
		return ErrRuntimeSpecInstancesNeedEphemeralPort
	}
	return nil
}

// runtimeSpecEndpointModes resolves responses_mode and messages_mode, in that
// order: an absent or empty mode is passthrough, and any other value must be
// one of the three EndpointMode values (ErrRuntimeSpecEndpointModeInvalid).
func runtimeSpecEndpointModes(req PutRuntimeSpecRequest) (responses, messages routing.EndpointMode, err error) {
	responses = routing.EndpointModePassthrough
	if strings.TrimSpace(req.ResponsesMode) != "" {
		m, ok := validEndpointMode(req.ResponsesMode)
		if !ok {
			return "", "", ErrRuntimeSpecEndpointModeInvalid
		}
		responses = m
	}
	messages = routing.EndpointModePassthrough
	if strings.TrimSpace(req.MessagesMode) != "" {
		m, ok := validEndpointMode(req.MessagesMode)
		if !ok {
			return "", "", ErrRuntimeSpecEndpointModeInvalid
		}
		messages = m
	}
	return responses, messages, nil
}

// runtimeSpecPrior is what a runtime-spec write keeps from the mapping's
// stored spec: the row itself (ok is false when there is none -- its id,
// created_at and measured RAM carry forward) and its measured VRAM per GPU
// index.
type runtimeSpecPrior struct {
	spec            routing.RuntimeSpec
	ok              bool
	measuredByIndex map[int]int
}

// priorRuntimeSpec is the read half of the read-then-upsert: an existing
// spec's id/created_at are preserved, and its stored GPU rows are the source
// of truth for VRAMMeasuredMB (the VRAM ownership rule on PutRuntimeSpec) —
// never what the request sent.
//
// With expectSpecID set (a benchmark writer, see putRuntimeSpec), the mapping
// must still hold that very spec when this read runs; otherwise the write is
// ErrRuntimeSpecNotFound and stores nothing. A missing row reads as the zero
// spec (the store's absent-read contract), whose empty id never equals a
// resolved one, so the one comparison refuses a deleted spec and one created
// anew alike.
//
// The guard narrows the race and does not close it, and that is an accepted
// limit: a DELETE of the spec that lands after this read and before
// writeRuntimeSpec's upsert -- across the GPU-row read below, the token seal
// and the clock read -- lets the upsert, which keys on mapping_id, store the
// spec again under the resolved id, and a delete-and-recreate in that gap has
// its new spec overwritten. Closing it needs a conditional update-by-id store
// write, which the store does not have.
func (s *Service) priorRuntimeSpec(ctx context.Context, mappingID, expectSpecID string) (runtimeSpecPrior, error) {
	existing, hadExisting, err := s.routes.RuntimeSpecByMapping(ctx, mappingID)
	if err != nil {
		return runtimeSpecPrior{}, err
	}
	if expectSpecID != "" && existing.ID != expectSpecID {
		return runtimeSpecPrior{}, ErrRuntimeSpecNotFound
	}
	prior := runtimeSpecPrior{spec: existing, ok: hadExisting, measuredByIndex: map[int]int{}}
	if !hadExisting {
		return prior, nil
	}
	existingGPUs, err := s.routes.RuntimeSpecGPUs(ctx, existing.ID)
	if err != nil {
		return runtimeSpecPrior{}, err
	}
	for _, g := range existingGPUs {
		prior.measuredByIndex[g.GPUIndex] = g.VRAMMeasuredMB
	}
	return prior, nil
}

// runtimeSpecRow builds the row writeRuntimeSpec upserts: the request's
// validated fields, the live-timings opt-in and the sealed API token (both
// depend on the stored row), and the stored row's id and created_at when
// there is one.
func (s *Service) runtimeSpecRow(mappingID string, req PutRuntimeSpecRequest, f runtimeSpecFields, prior runtimeSpecPrior) (routing.RuntimeSpec, error) {
	// The live-timings opt-in. !prior.ok is what "create" means on a
	// full-document upsert, so a first write takes the kind-dependent default
	// while a later save of an existing spec that omits the key keeps the
	// stored value -- re-saving a llama.cpp spec never re-enables a flag the
	// operator turned off, nor clears one they turned on.
	//
	// The two arms below are the pointer's whole point. A non-nil value is the
	// caller's, already validated (a true here implies a capable kind, or
	// validateRuntimeSpecRequest has already refused it). A nil against a
	// document whose kind cannot honour the flag CLEARS it, so a PUT that
	// retypes an llama_cpp spec to ollama cannot leave a stale true behind --
	// and since the caller said nothing about the flag, the clear contradicts
	// nothing they asked for. That is the asymmetry of this feature: the
	// assertion is refused, the non-mention is normalised.
	//
	// f.liveTimingsCapable is what the refusal computed, from the EFFECTIVE
	// kind. Do not recompute it from specType here: that is how an
	// auto-detect llama-server spec ends up defaulting off.
	liveTimings := f.liveTimingsCapable
	if prior.ok {
		liveTimings = prior.spec.ResponsesLiveTimingsEnabled
	}
	switch {
	case req.ResponsesLiveTimingsEnabled != nil:
		liveTimings = *req.ResponsesLiveTimingsEnabled
	case !f.liveTimingsCapable:
		liveTimings = false
	}
	mode, sealedToken, err := s.runtimeSpecAPIToken(req, prior.spec.APIToken)
	if err != nil {
		return routing.RuntimeSpec{}, err
	}
	headerSource := req.APITokenHeaderSource
	if headerSource == "" {
		headerSource = string(routing.RuntimeAPITokenHeaderSourceApp)
	}
	// APITokenHeader is only meaningful for a custom source (its shape was
	// validated by validateRuntimeSpecAPIToken via checkHeaderName); an "app"
	// source inherits the application's header at request-auth time
	// (routing.SpecUpstreamAuth via effectiveAPITokenHeader) and stores none
	// here.
	headerName := ""
	if routing.RuntimeAPITokenHeaderSource(headerSource) == routing.RuntimeAPITokenHeaderSourceCustom {
		headerName = strings.TrimSpace(req.APITokenHeader)
	}
	now := s.clock().UTC()
	spec := routing.RuntimeSpec{
		ID:                          "rspec_" + compactRandomHex(16),
		MappingID:                   mappingID,
		Enabled:                     req.Enabled,
		Binary:                      f.binary,
		Args:                        f.args,
		Env:                         f.env,
		ConfigFiles:                 f.configFiles,
		WorkDir:                     strings.TrimSpace(req.WorkDir),
		ListenPort:                  req.ListenPort,
		HealthPath:                  f.healthPath,
		HealthTimeoutSeconds:        f.healthTimeout,
		StartupTimeoutSeconds:       f.startupTimeout,
		IdleTimeoutSeconds:          req.IdleTimeoutSeconds,
		AdmissionWaitTimeoutSeconds: req.AdmissionWaitTimeoutSeconds,
		Pinned:                      req.Pinned,
		AdminState:                  f.adminState,
		RAMEstimateMB:               req.RAMEstimateMB,
		Replicable:                  req.Replicable,
		MinInstances:                req.MinInstances,
		MaxInstances:                req.MaxInstances,
		InstanceParallelism:         req.InstanceParallelism,
		SetVisibleDevices:           req.SetVisibleDevices,
		VisibleDevicesMode:          f.visibleMode,
		APITokenMode:                mode,
		APIToken:                    sealedToken,
		APITokenHeaderSource:        headerSource,
		APITokenHeader:              headerName,
		APIFlavors:                  f.flavors,
		ResponsesMode:               f.responsesMode,
		MessagesMode:                f.messagesMode,
		ResponsesLiveTimingsEnabled: liveTimings,
		Type:                        f.specType,
		MetricsPath:                 f.metricsPath,
		ContextProbePath:            f.contextProbePath,
		CreatedAt:                   now,
		UpdatedAt:                   now,
	}
	if prior.ok {
		spec.ID = prior.spec.ID
		spec.CreatedAt = prior.spec.CreatedAt
		// The agent's two measurements, RAM and startup, are the stored values,
		// never the request's (the request has no such fields -- the VRAM
		// ownership rule, spec-level). The store's upsert already keeps both
		// columns across a conflict, so this is not what protects them; it
		// keeps the row this method hands back honest, because writeRuntimeSpec
		// builds its response DTO from this row rather than re-reading the
		// store, and a bare 0 here would report "nothing measured" for a spec
		// that is measured -- and, for a replicable spec, an effective idle
		// timeout the runtime-config document does not push.
		spec.RAMMeasuredMB = prior.spec.RAMMeasuredMB
		spec.StartupMeasuredMS = prior.spec.StartupMeasuredMS
	}
	return spec, nil
}

// runtimeSpecAPIToken resolves the per-spec API token: the mode,
// "app" when absent, and the SEALED value to persist, computed STRICTLY
// BEFORE the store write, so a keyless-disk seal rejection
// (capture.ErrKeyRequired) returns without persisting anything -- no
// plaintext token, no half-applied mode. stored is the stored spec's
// (already sealed) token, "" when there is none. Mirrors
// service_applications.go's write-only *string sentinel exactly: nil = keep
// the stored value, "" seals to "" = clear, a value replaces-and-seals. Mode
// "random" generates a fresh secret on first write or when rotate is asked;
// modes "app"/"off" store no per-spec token. Validation
// (validateRuntimeSpecAPIToken) has already run -- this only seals.
func (s *Service) runtimeSpecAPIToken(req PutRuntimeSpecRequest, stored string) (mode, sealed string, err error) {
	mode = req.APITokenMode
	if mode == "" {
		mode = string(routing.RuntimeAPITokenModeApp)
	}
	switch routing.RuntimeAPITokenMode(mode) {
	case routing.RuntimeAPITokenModeRandom:
		if stored != "" && !req.APITokenRotate {
			return mode, stored, nil
		}
		rawToken, err := generateSecret()
		if err != nil {
			return "", "", err
		}
		sealed, err = capture.SealSecret(s.cipher, s.settingsVolatile, rawToken)
		return mode, sealed, err // ErrKeyRequired on a keyless disk store => 400, nothing persisted
	case routing.RuntimeAPITokenModeSet:
		if req.APIToken == nil { // nil = keep the stored token untouched
			return mode, stored, nil
		}
		sealed, err = capture.SealSecret(s.cipher, s.settingsVolatile, *req.APIToken)
		return mode, sealed, err // "" seals to "" = cleared
	default: // app, off -- no per-spec token stored
		return mode, "", nil
	}
}

// runtimeSpecGPURows builds a spec's GPU rows from the request: the request
// array order becomes the stored Position, and VRAMMeasuredMB is the stored
// value for an index that already had a row and 0 for a brand-new one (the
// VRAM ownership rule on PutRuntimeSpec).
func runtimeSpecGPURows(specID string, gpus []RuntimeSpecGPUDTO, measuredByIndex map[int]int) []routing.RuntimeSpecGPU {
	rows := make([]routing.RuntimeSpecGPU, 0, len(gpus))
	for i, g := range gpus {
		rows = append(rows, routing.RuntimeSpecGPU{
			SpecID:         specID,
			GPUIndex:       g.Index,
			Position:       i,
			VRAMEstimateMB: g.VRAMEstimateMB,
			VRAMMeasuredMB: measuredByIndex[g.Index],
		})
	}
	return rows
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
// a defect, not a deliberate symmetry.)
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
// validateRuntimeSpecRequest; any other non-env/args value is rejected.
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
	// a malformed request. Empty defaults to "env" (resolved in validateRuntimeSpecRequest).
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
// logic in runtimeSpecAPIToken needs the resolved mode, not the raw request value.
func validRuntimeAPITokenMode(s string) bool {
	switch routing.RuntimeAPITokenMode(s) {
	case routing.RuntimeAPITokenModeOff, routing.RuntimeAPITokenModeSet,
		routing.RuntimeAPITokenModeRandom, routing.RuntimeAPITokenModeApp:
		return true
	}
	return false
}

// validRuntimeSpecType reports whether s is "" (auto-detect from Binary, the
// default and every pre-feature row's stored value) or one of the seven
// routing.RuntimeSpecType values (vllm, llama_cpp, tgi, ollama,
// stable_diffusion_cpp, audio_cpp, custom). Unlike validRuntimeAPITokenMode,
// empty IS accepted directly here (mirrors validVisibleDevicesMode) -- callers
// pass req.Type through untouched, they don't normalize "" to a concrete value
// first, because "" is itself a legitimate stored/wire value (auto), not a
// default that collapses into one of the seven kinds.
func validRuntimeSpecType(s string) bool {
	switch routing.RuntimeSpecType(s) {
	case "", routing.RuntimeSpecTypeVLLM, routing.RuntimeSpecTypeLlamaCpp,
		routing.RuntimeSpecTypeTGI, routing.RuntimeSpecTypeOllama,
		routing.RuntimeSpecTypeStableDiffusionCpp, routing.RuntimeSpecTypeAudioCpp,
		routing.RuntimeSpecTypeCustom:
		return true
	}
	return false
}

// safeRelativeProbePath reports whether p is a safe relative probe path: empty
// (the operator left the override blank -- routing.DeriveProbePaths then
// supplies the per-type default), or a single-"/"-rooted path carrying no
// scheme, no protocol-relative "//" authority, and no whitespace/control
// bytes. It is the SSRF guard on metrics_path/context_probe_path: the agent
// concatenates the override onto "http://127.0.0.1:PORT", so a value like
// "@evil:9999/x", "//evil", or "http://evil" would otherwise re-parse to an
// off-loopback Host. Valid overrides such as "/metrics", "/v1/models",
// "/props" and "/api/show" all pass. Callers pass the already-TrimSpace'd
// value; the byte scan additionally rejects any INTERIOR whitespace/control
// byte (e.g. "/foo bar"), which trimming does not remove.
func safeRelativeProbePath(p string) bool {
	if p == "" {
		return true
	}
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return false
	}
	if strings.Contains(p, "://") {
		return false
	}
	for i := 0; i < len(p); i++ {
		// Reject every byte at or below ASCII space (control chars, tab, CR,
		// NL, and space itself) and DEL.
		if b := p[i]; b <= 0x20 || b == 0x7f {
			return false
		}
	}
	return true
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
// those drive sealing/rotation in the write path (runtimeSpecAPIToken), a persistence
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

// validateRuntimeSpecConfigFiles validates a spec's config files (#257) and
// returns the opaque JSON column value for them: "" for an empty or nil list
// (the same "nothing" encoding an empty Args gets), else the marshalled list in
// request order. Every refusal wraps ErrRuntimeSpecConfigFilesInvalid and names
// the file and the rule it broke; it never quotes content.
//
// The rules, all checked before anything is stored:
//   - at most runtimeSpecConfigFilesMax files;
//   - each name is a single path segment (configFileNameIsSingleSegment);
//   - names are unique, compared case-insensitively;
//   - each content is at most runtimeSpecConfigFileMaxBytes bytes;
//   - no content carries a secret placeholder: the agent writes these files in
//     the clear (0600, but still a plain file next to the model), so a
//     ${API_TOKEN} or ${AGENT_ENV:NAME} in one would put a secret at rest on
//     disk. Secrets reach the child through Env/Args, never through a file.
//
// The agent re-checks the names itself before writing -- the gateway is not
// the authority over the agent's filesystem, this is the early-feedback layer.
func validateRuntimeSpecConfigFiles(files []ConfigFileDTO) (string, error) {
	if len(files) == 0 {
		return "", nil
	}
	if len(files) > runtimeSpecConfigFilesMax {
		return "", fmt.Errorf("%w: at most %d config files are allowed, got %d",
			ErrRuntimeSpecConfigFilesInvalid, runtimeSpecConfigFilesMax, len(files))
	}
	seen := make(map[string]struct{}, len(files))
	for _, f := range files {
		if !configFileNameIsSingleSegment(f.Name) {
			return "", fmt.Errorf("%w: config file name %q must be a single file name (no path separators, \"..\", drive or stream marker, or control characters)",
				ErrRuntimeSpecConfigFilesInvalid, f.Name)
		}
		key := strings.ToLower(f.Name)
		if _, dup := seen[key]; dup {
			return "", fmt.Errorf("%w: duplicate config file name %q (names are compared case-insensitively)",
				ErrRuntimeSpecConfigFilesInvalid, f.Name)
		}
		seen[key] = struct{}{}
		if len(f.Content) > runtimeSpecConfigFileMaxBytes {
			return "", fmt.Errorf("%w: config file %q is larger than %d bytes",
				ErrRuntimeSpecConfigFilesInvalid, f.Name, runtimeSpecConfigFileMaxBytes)
		}
		for _, placeholder := range runtimeSpecConfigFileSecretPlaceholders {
			if strings.Contains(f.Content, placeholder) {
				return "", fmt.Errorf("%w: config file %q must not contain the secret placeholder %s -- files are written in the clear, pass secrets through env or args instead",
					ErrRuntimeSpecConfigFilesInvalid, f.Name, placeholder)
			}
		}
	}
	encoded, err := json.Marshal(files)
	if err != nil {
		// Unreachable for a slice of two-string structs, but never swallow it.
		return "", fmt.Errorf("%w: %w", ErrRuntimeSpecConfigFilesInvalid, err)
	}
	return string(encoded), nil
}

// configFileNameIsSingleSegment reports whether name is one plain file name the
// agent can safely join under its config directory: non-blank, no path
// separator of either OS, not "." or "..", no volume or alternate-data-stream
// marker (":"), and no control character.
//
// The separators and the colon are rejected explicitly rather than left to
// filepath.Base: Base is per-GOOS, the gateway is OS-agnostic, and the agent
// that writes the file may run on Windows while this check runs on Linux
// (the same reasoning as runtimeSpecBinaryIsAbsolute). filepath.Base is kept
// as an extra guard, so a form the explicit rules missed still has to survive
// the local OS's own idea of a base name.
func configFileNameIsSingleSegment(name string) bool {
	if strings.TrimSpace(name) == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\:`) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return false
	}
	return filepath.Base(name) == name && !filepath.IsAbs(name)
}

// decodeRuntimeSpecConfigFiles is the inverse of validateRuntimeSpecConfigFiles'
// encoding: the stored column back to a list, always non-nil. "" is the stored
// form of "no files" -- every row written before #257 has it -- so unlike Args
// and Env it is not an error. Anything else that fails to parse is a corrupt
// stored row, not a client-input problem, and surfaces as the domain sentinel
// rather than a raw JSON error (the Args/Env convention in runtimeSpecDTO).
func decodeRuntimeSpecConfigFiles(stored string) ([]ConfigFileDTO, error) {
	files := []ConfigFileDTO{}
	if stored == "" {
		return files, nil
	}
	if err := json.Unmarshal([]byte(stored), &files); err != nil {
		return nil, ErrRuntimeSpecConfigFilesInvalid
	}
	if files == nil {
		files = []ConfigFileDTO{}
	}
	return files, nil
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

// minInstancesExceedRealtime reports whether a spec's minimum instance count is
// above the real-time concurrency of its mapping's audio ramp
// (RuntimeSpecDTO.InstancesMinExceedsRealtime). Only a ramp stopped by performance
// measured a ceiling, and its count counts even at 0 (not real-time with one
// stream); a ramp stopped by resources or the cap left a lower bound, which no
// minimum is known to exceed, and no ramp at all leaves nothing to compare.
func minInstancesExceedRealtime(minInstances int, mapping routing.ModelMapping) bool {
	return mapping.AudioConcurrencyLimit == routing.AudioLimitPerformance && minInstances > mapping.AudioRealtimeConcurrency
}

// runtimeSpecDTO builds the wire DTO from a stored spec + its GPU rows, plus
// the parent application (for the read-only api-token echoes — see
// RuntimeSpecDTO.AppAPITokenSet/AppAPITokenHeader), its mapping (for the
// real-time warning, minInstancesExceedRealtime) and the spec's effective
// idle timeout, which its callers derive with effectiveIdleTimeout, the one
// derivation the pushed document uses too. Args/Env are opaque JSON
// strings at the store layer (the netbird_group_ids pattern) — an unmarshal
// failure here means the stored row is corrupt, not a client-input problem,
// but still surfaces as the matching domain sentinel
// (ErrRuntimeSpecArgsInvalid / ErrRuntimeSpecEnvInvalid) rather than a raw
// JSON error or a 500.
func runtimeSpecDTO(spec routing.RuntimeSpec, gpus []routing.RuntimeSpecGPU, app routing.Application, mapping routing.ModelMapping, effectiveIdleSeconds int) (RuntimeSpecDTO, error) {
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
	configFiles, err := decodeRuntimeSpecConfigFiles(spec.ConfigFiles)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	gpuDTOs := make([]RuntimeSpecGPUDTO, 0, len(gpus))
	for _, g := range gpus {
		gpuDTOs = append(gpuDTOs, RuntimeSpecGPUDTO{
			Index:          g.GPUIndex,
			VRAMEstimateMB: g.VRAMEstimateMB,
			VRAMMeasuredMB: g.VRAMMeasuredMB,
		})
	}
	effectiveType := routing.EffectiveRuntimeSpecType(spec)
	resolvedMetrics, resolvedContext := routing.DeriveProbePaths(effectiveType, spec.MetricsPath, spec.ContextProbePath)
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
		SetVisibleDevices:           spec.SetVisibleDevices,
		GPUs:                        gpuDTOs,
		RAMEstimateMB:               spec.RAMEstimateMB,
		RAMMeasuredMB:               spec.RAMMeasuredMB,
		Replicable:                  spec.Replicable,
		MinInstances:                spec.MinInstances,
		MaxInstances:                spec.MaxInstances,
		InstanceParallelism:         spec.InstanceParallelism,
		StartupMeasuredMS:           spec.StartupMeasuredMS,
		EffectiveIdleTimeoutSeconds: effectiveIdleSeconds,
		InstancesMinExceedsRealtime: minInstancesExceedRealtime(spec.MinInstances, mapping),
		APIFlavors:                  append([]string{}, spec.APIFlavors...),
		ResponsesMode:               string(spec.ResponsesMode),
		MessagesMode:                string(spec.MessagesMode),
		ResponsesLiveTimingsEnabled: spec.ResponsesLiveTimingsEnabled,
		VisibleDevicesMode:          string(spec.VisibleDevicesMode),
		APITokenMode:                normalizeRuntimeAPITokenMode(spec.APITokenMode),
		APITokenSet:                 spec.APIToken != "",
		APITokenHeaderSource:        normalizeRuntimeAPITokenHeaderSource(spec.APITokenHeaderSource),
		APITokenHeader:              spec.APITokenHeader,
		AppAPITokenSet:              app.APIToken != "",
		AppAPITokenHeader:           app.APITokenHeader,
		Type:                        spec.Type,
		MetricsPath:                 spec.MetricsPath,
		ContextProbePath:            spec.ContextProbePath,
		ConfigFiles:                 configFiles,
		EffectiveType:               string(effectiveType),
		ResolvedMetricsPath:         resolvedMetrics,
		ResolvedContextProbePath:    resolvedContext,
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

// CoResidencyPairDTO is one EXPLICIT co-residency setting on the wire: a pair
// of mapping ids and the verdict an operator gave it. On input the two ids may
// come in either order (SetCoResidency sorts them); the returned pair is always
// canonical (A < B lexicographically).
//
// Verdict is routing.CoResidencyVerdictAllow or routing.CoResidencyVerdictForbid.
// A pair with NO entry is not listed at all -- what it means is the DTO's Mode.
// On input an empty Verdict is an allow (what a pair meant before verdicts
// existed, and what the store normalises it to).
type CoResidencyPairDTO struct {
	A       string `json:"a"`
	B       string `json:"b"`
	Verdict string `json:"verdict"`
}

// CoResidencyDTO is the portal-facing co-residency setting of one application
// (one server_agent, hence one server): its Mode, and every EXPLICIT pair of its
// own mappings with its verdict -- the pairs an operator has actually set, both
// allows and forbids, not the effective matrix. Mode says what a pair that is
// NOT listed means: routing.CoResidencyModeOptOut allows it (only forbids need
// listing), routing.CoResidencyModeOptIn forbids it (only allows need listing).
// Switching the mode never touches the listed pairs. The effective pair list the
// agent receives is derived from both (effectiveAllowedPairs) and is not part of
// this DTO. Always a non-nil Pairs slice, even when empty.
type CoResidencyDTO struct {
	Mode  string               `json:"mode"`
	Pairs []CoResidencyPairDTO `json:"pairs"`
}

// SetCoResidencyRequest is a full-document replace (like
// SetRuntimeSpecGPUs/SetServerGPUBudgets): every pair supplied here IS the
// new set of explicit pairs. Pair ordering within each element does not matter --
// SetCoResidency sorts each pair server-side before storing/comparing.
//
// Mode is routing.CoResidencyModeOptIn or routing.CoResidencyModeOptOut; an
// empty Mode leaves the stored mode unchanged (a request that does not name a
// mode must never flip it), anything else is ErrCoResidencyModeInvalid.
//
// Pairs is always the WHOLE set: a request that omits it, or sends [], removes
// every explicit pair. A client that only wants to switch the mode therefore
// re-sends the pairs it read -- the switch itself never touches them.
type SetCoResidencyRequest struct {
	Mode  string               `json:"mode"`
	Pairs []CoResidencyPairDTO `json:"pairs"`
}

// GetCoResidency returns appID's co-residency mode and explicit pairs, each with
// its verdict. authorizeApplication gates it (404-no-leak, same collapse as
// every other application read).
func (s *Service) GetCoResidency(ctx context.Context, principal auth.Token, appID string) (CoResidencyDTO, error) {
	app, _, err := s.authorizeApplication(ctx, principal, appID)
	if err != nil {
		return CoResidencyDTO{}, err
	}
	return s.storedCoResidency(ctx, app.ID)
}

// storedCoResidency reads appID's stored mode and explicit pairs into the DTO.
// It does no authorization: both callers have already run authorizeApplication.
func (s *Service) storedCoResidency(ctx context.Context, appID string) (CoResidencyDTO, error) {
	mode, err := s.routes.GetCoResidencyMode(ctx, appID)
	if err != nil {
		return CoResidencyDTO{}, err
	}
	rules, err := s.routes.CoResidencyRulesByApplication(ctx, appID)
	if err != nil {
		return CoResidencyDTO{}, err
	}
	return coResidencyDTO(mode, rules), nil
}

// SetCoResidency validates and atomically replaces appID's whole set of explicit
// co-residency pairs, and sets its mode when the request names one. Everything
// is validated BEFORE any store call (validate-before-mutate): the mode must be
// a known one, every verdict must be a known one, the two ids of a pair must be
// distinct, both must name a mapping belonging to appID
// itself (checked against s.routes.MappingsByApplication, never a bare global
// existence check -- a mapping id that exists but belongs to a DIFFERENT
// application is exactly as invalid as one that does not exist at all), each
// pair is canonicalized by sorting it (mapping_a_id < mapping_b_id
// lexicographically) so the client never has to submit pairs in a particular
// order, and duplicate pairs are rejected AFTER that normalization -- so
// [["a","b"],["b","a"]] is a duplicate, not two distinct entries (whatever
// verdicts they carry). The store itself (SetCoResidencyRules) is a dumb pair
// table by design and performs none of this beyond the verdict; it is
// entirely this method's job.
//
// The mode and the pairs are two store writes, not one transaction. The
// effective list the agent receives depends on both, and SetCoResidencyMode does
// not bump updated_at, so the agent is notified after the writes -- and also when
// the pairs write fails after the mode write landed, because the stored mode
// (hence the effective list) HAS changed by then. A failure of the first write
// leaves the pairs untouched, so nothing was changed and nothing is announced.
func (s *Service) SetCoResidency(ctx context.Context, principal auth.Token, appID string, req SetCoResidencyRequest) (CoResidencyDTO, error) {
	app, server, err := s.authorizeApplication(ctx, principal, appID)
	if err != nil {
		return CoResidencyDTO{}, err
	}
	mode := routing.CoResidencyMode(req.Mode)
	if req.Mode != "" && !mode.Valid() {
		return CoResidencyDTO{}, ErrCoResidencyModeInvalid
	}
	ownMappings, err := s.routes.MappingsByApplication(ctx, app.ID)
	if err != nil {
		return CoResidencyDTO{}, err
	}
	belongsToApp := make(map[string]bool, len(ownMappings))
	for _, m := range ownMappings {
		belongsToApp[m.ID] = true
	}
	rules, err := coResidencyRulesFromPairs(app.ID, req.Pairs, belongsToApp, s.clock().UTC())
	if err != nil {
		return CoResidencyDTO{}, err
	}
	modeWritten := false
	if req.Mode != "" {
		if err := s.routes.SetCoResidencyMode(ctx, app.ID, mode); err != nil {
			return CoResidencyDTO{}, err
		}
		modeWritten = true
	}
	if err := s.routes.SetCoResidencyRules(ctx, app.ID, rules); err != nil {
		if modeWritten {
			s.notifyRuntimeChanged(server.ID)
		}
		return CoResidencyDTO{}, err
	}
	s.notifyRuntimeChanged(server.ID)
	return s.storedCoResidency(ctx, app.ID)
}

// coResidencyRulesFromPairs validates a SetCoResidency request's pairs and turns
// them into the store's rules: each pair names two distinct mappings of the
// application (belongsToApp), carries a known verdict (empty is allow), and is
// stored in canonical order (a < b), so a duplicate is detected after that
// normalization. Any violation is ErrCoResidencyPairInvalid and nothing is
// returned, so the caller never writes from a half-validated set.
func coResidencyRulesFromPairs(appID string, pairs []CoResidencyPairDTO, belongsToApp map[string]bool, createdAt time.Time) ([]routing.CoResidencyRule, error) {
	seenPairs := make(map[[2]string]bool, len(pairs))
	rules := make([]routing.CoResidencyRule, 0, len(pairs))
	for _, pair := range pairs {
		a, b := strings.TrimSpace(pair.A), strings.TrimSpace(pair.B)
		if a == "" || b == "" || a == b {
			return nil, ErrCoResidencyPairInvalid
		}
		if !belongsToApp[a] || !belongsToApp[b] {
			return nil, ErrCoResidencyPairInvalid
		}
		verdict, ok := routing.NormalizeCoResidencyVerdict(pair.Verdict)
		if !ok {
			return nil, ErrCoResidencyPairInvalid
		}
		if a > b {
			a, b = b, a
		}
		key := [2]string{a, b}
		if seenPairs[key] {
			return nil, ErrCoResidencyPairInvalid
		}
		seenPairs[key] = true
		rules = append(rules, routing.CoResidencyRule{ApplicationID: appID, MappingAID: a, MappingBID: b, Verdict: verdict, CreatedAt: createdAt})
	}
	return rules, nil
}

// coResidencyDTO builds the wire DTO from the stored mode and explicit rules;
// always a non-nil Pairs slice (a collection-shaped return must never serialize
// to JSON null -- see SetRuntimeSpecGPUs's equivalent contract). Every rule is
// listed with its own verdict: a forbid is never folded into an allow.
func coResidencyDTO(mode routing.CoResidencyMode, rules []routing.CoResidencyRule) CoResidencyDTO {
	pairs := make([]CoResidencyPairDTO, 0, len(rules))
	for _, r := range rules {
		pairs = append(pairs, CoResidencyPairDTO{A: r.MappingAID, B: r.MappingBID, Verdict: r.Verdict})
	}
	return CoResidencyDTO{Mode: string(mode), Pairs: pairs}
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

// --- runtime warnings -------------------------------------------------------

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

// runtimeFlavorNotOnApplicationWarning: a runtime spec of a server_agent
// application lists an API flavor that the application does not declare.
// Candidacy admits a mapping on the application's flavors
// (routing.applicationServesEndpoint), so a request of that flavor never
// reaches the spec, and the flavor has no effect. An advisory, not an error:
// an application from which a flavor was removed on purpose leaves the same
// harmless extra on its specs.
const runtimeFlavorNotOnApplicationWarning = "api_flavors_not_on_application"

// runtimeTextOnStableDiffusionWarning: a runtime spec whose effective type
// (routing.EffectiveRuntimeSpecType) is stable_diffusion_cpp lists flavors
// that are not images-only (routing.FlavorsAreImagesOnly). The gateway then
// treats the model as a text model under each text flavor the served rule
// (routing.MappingServesAPIFlavor) grants it, openai whenever its application
// declares openai: the listings offer it under that flavor, and text requests
// of that flavor are dispatched to it. sd-server has no chat endpoint, so
// those requests fail.
const runtimeTextOnStableDiffusionWarning = "api_flavors_text_on_stable_diffusion"

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
	if anySpecListsFlavorNotOnApplication(app, specs) {
		warnings = append(warnings, runtimeFlavorNotOnApplicationWarning)
	}
	if anyStableDiffusionSpecNotImagesOnly(app, specs) {
		warnings = append(warnings, runtimeTextOnStableDiffusionWarning)
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

// anySpecListsFlavorNotOnApplication backs
// runtimeFlavorNotOnApplicationWarning: it reports whether any of specs lists
// a flavor that app does not declare. Only a server_agent application's specs
// are read for flavors (routing.EffectiveFields), so any other application
// answers false. Disabled specs count, as in anySpecBinaryContradictsReportedOS:
// dispatch reads a spec's flavors whatever its Enabled flag, and a spec is
// routinely created disabled and enabled afterwards.
func anySpecListsFlavorNotOnApplication(app routing.Application, specs []routing.RuntimeSpec) bool {
	if app.Type != routing.ProviderServerAgent {
		return false
	}
	for _, spec := range specs {
		for _, flavor := range spec.APIFlavors {
			if !slices.Contains(app.APIFlavors, flavor) {
				return true
			}
		}
	}
	return false
}

// anyStableDiffusionSpecNotImagesOnly backs
// runtimeTextOnStableDiffusionWarning: it reports whether any of specs has the
// effective type stable_diffusion_cpp (the explicit Type, else the type
// detected from the binary) and flavors that are not images-only. A spec with
// an empty flavor list counts: an empty list is never images-only, so the
// gateway treats that spec as a text model too. Same scope as
// anySpecListsFlavorNotOnApplication: a server_agent application only,
// disabled specs included.
func anyStableDiffusionSpecNotImagesOnly(app routing.Application, specs []routing.RuntimeSpec) bool {
	if app.Type != routing.ProviderServerAgent {
		return false
	}
	for _, spec := range specs {
		if routing.EffectiveRuntimeSpecType(spec) == routing.RuntimeSpecTypeStableDiffusionCpp && !routing.FlavorsAreImagesOnly(spec.APIFlavors) {
			return true
		}
	}
	return false
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
// AgentRuntimeConfig (below) derives the whole document from exactly seven
// kinds of row. A path needs a notification if and only if it writes one of
// them:
//
//  1. the AI server row        -> max_processes (AIServer.RuntimeMaxProcesses)
//  2. its server_agent APPLICATION row -> router_listen (Application.Port);
//     that application's id is also the key for rows 3-5
//  3. that application's MAPPINGS -> each spec's model / upstream_model
//     (gateway_model_name / app_model_name -- the agent has no other source)
//  4. those mappings' RUNTIME SPECS and per-spec GPU rows -> specs[]
//  5. that application's CO-RESIDENCY mode and explicit pair rows ->
//     coresident[] (the effective allowed-pair list, effectiveAllowedPairs)
//  6. the server's per-GPU VRAM BUDGETS -> gpu_budgets[]
//  7. those mappings' CAPABILITY VERDICTS (model_mapping_capabilities, one
//     batched read) -> each spec's role (routing.DeriveRole). The role is a
//     document field like any other, so a verdict write moves the document
//     and its ETag
//
// Call sites, by row: (1) UpdateServer; (2) CreateApplication /
// UpdateApplication / DeleteApplication, via
// notifyRuntimeChangedForApplication; (3) CreateMapping / UpdateMapping /
// DeleteMapping / reconcileApplicationModels, via
// notifyRuntimeChangedForMapping; (4) PutRuntimeSpec / DeleteRuntimeSpec, and
// the benchmark run's own launch-spec writers: SetBenchmarkRuntimeSpecAdminState
// once per stored write, through putRuntimeSpec, and
// SetBenchmarkRuntimeSpecsPinned / SetBenchmarkRuntimeSpecsAdminState from
// setBenchmarkRuntimeSpecs, after the batch's last write, once for each server
// a write stored to; (5) SetCoResidency; (6) SetServerGPUBudgets; (7) the
// operator's verdicts are written by CreateMapping / UpdateMapping, which are
// already row 3's call sites and notify after the capability write has landed.
//
// The "never which field" half is the load-bearing half. A "relevant fields"
// allow-list inside a write path would be a second, uncompiled copy of
// AgentRuntimeConfig's derivation, and it would rot the moment that
// derivation grows a field -- while over-notifying is cheap and idempotent
// (at most one coalesced pass of the server's push worker, which
// gateway.Server.PushRuntimeConfig runs, and that pass fail-closes on "no
// runtime_manager agent connected for this server" before it reads anything;
// the agent then re-fetches and its driver applies only on a real ETag
// change). Under-notifying is the actual bug.
//
// Only three kinds of write to rows 1-7 deliberately do NOT notify, and each
// is an exemption of a whole write PATH rather than of a field:
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
//   - The AUTOMATED capability writers (the agent's telemetry ingest in
//     agent_ingest.go, the benchmark runner, the health probe and the relay's
//     observation row). They write row 7, so they can move a spec's role and
//     the document's ETag, but they are background probes and observations,
//     not an operator action, and nothing safety-relevant waits on them: the
//     role only ORDERS eviction victims that are already evictable, it never
//     decides whether one is. The agent's poll reconciles the new role.
//
// Two writes outside this service DO push, because each moves a document
// field the agent does not already hold: a replicable spec's pushed idle
// timeout is derived (effectiveIdleTimeoutSeconds) from the spec's
// startup_measured_ms (row 4) and its mapping's load_time_ms (row 3), and
// neither figure is the agent's own value for that field. The telemetry
// ingest writes startup_measured_ms (gateway.Server.writeBackRuntimeStartup,
// only on a move of more than 10 %, so jitter pushes nothing) and the speed
// benchmark writes load_time_ms (gateway.Server.measureSpeedTarget); each
// pushes that server's document after a write for a replicable spec, through
// gateway.Server.PushRuntimeConfig -- the push this hook runs -- and only for
// a replicable spec, since the derived timeout is the one field either moves.
// Any other writer of load_time_ms, an audio benchmark included, pushes the
// same way.
//
// One input of the document is not a row: whether the server's agent declares
// the runtime_instances feature (AgentFeatureReader, Service.specAsPushed),
// which decides whether a replicable spec goes out with its instance settings,
// and whether the gateway knows that declaration at all -- a document that
// depends on it is held until it is known (AgentRuntimeConfigDTO.
// AwaitsAgentFeatures). The agent declares it in its telemetry, outside this
// service, so the gateway's ingest pushes that server's document itself when
// the declaration becomes known or changes (gateway.Server.
// ingestTelemetrySample).
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
// needs: max(measured, estimate) of the row's two stored figures -- see
// agentRuntimeSpecDTO. 0 means unknown (both source fields are 0), never
// "needs no VRAM", mirroring RuntimeSpecGPUDTO's two source fields.
type AgentRuntimeSpecGPUDTO struct {
	Index  int `json:"index"`
	VRAMMB int `json:"vram_mb"`
}

// AgentRuntimeSpecDTO is one ENABLED launch spec inside the runtime-config
// document -- everything the agent needs to exec and supervise the process.
// RAMMB is the spec's host-RAM demand: spec-level (RAM is a host resource, so
// it sits beside GPUs and never on a per-GPU row) and, like each GPU's VRAMMB,
// max(measured, estimate) -- see agentRuntimeSpecDTO. 0 is unknown demand.
// Model/UpstreamModel fold in the owning ModelMapping's two model-name
// fields (gateway_model_name / app_model_name), since the agent has no other
// way to learn them. Env values legitimately carry ${AGENT_ENV:NAME}/${PORT}/
// ${MODEL} placeholders the agent resolves locally at launch time -- this
// DTO, like RuntimeSpecDTO, passes them through untouched: never validated or
// rewritten here. IdleTimeoutSeconds is the EFFECTIVE idle timeout
// (effectiveIdleTimeout): the stored value for a spec that is not replicable
// or goes to an agent without the runtime_instances feature, and for a
// replicable one at least a floor, derived from its measured
// startup -- never 0 there, so a replicable spec's unprotected instances
// always idle out. The instances it protects (min_instances, and instance 0
// while pinned) never idle out, whatever this says.
type AgentRuntimeSpecDTO struct {
	ID                          string                   `json:"id"`
	Model                       string                   `json:"model"`
	UpstreamModel               string                   `json:"upstream_model"`
	Binary                      string                   `json:"binary"`
	Args                        []string                 `json:"args"`
	Env                         map[string]string        `json:"env"`
	WorkDir                     string                   `json:"work_dir,omitempty"`
	GPUs                        []AgentRuntimeSpecGPUDTO `json:"gpus"`
	RAMMB                       int                      `json:"ram_mb"` // host-RAM demand, max(measured, estimate); 0 = unknown, never "needs none"
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
	// Type is the RESOLVED effective runtime-server kind for this spec --
	// routing.EffectiveRuntimeSpecType(spec): the explicit spec.Type when
	// set, else the type auto-detected from Binary. Never the raw stored
	// spec.Type, which may be "" -- the agent always receives a concrete
	// kind it can act on directly.
	Type string `json:"type"`
	// Role is the spec's derived model role -- one of the routing.ModelRole
	// values ("asr", "tts", "image", "embedding", "reranker", "chat", "other"),
	// never empty: routing.DeriveRole over the RESOLVED Type above and the yes
	// verdicts of the spec's mapping. It is the ONE derivation the portal's
	// ModelDTO.Role also uses, shipped here so the agent's role-aware eviction
	// and the editor's role grouping read the same value rather than each
	// re-deriving it. It is derived on every build, never stored, so an
	// operator's verdict change moves it (and the document's ETag) on the next
	// push.
	Role string `json:"role"`
	// MetricsPath and ContextProbePath are the RESOLVED effective probe
	// paths for Type -- routing.DeriveProbePaths(Type, spec.MetricsPath,
	// spec.ContextProbePath): the stored per-spec override when set, else
	// Type's own default (which may itself be empty, e.g. custom or
	// ollama's metrics path). Never the raw stored override alone.
	MetricsPath      string `json:"metrics_path"`
	ContextProbePath string `json:"context_probe_path"`
	// ConfigFiles are the operator-supplied files the agent writes into the
	// spec's per-spec directory before it launches the child (#257; the
	// agent-side runtime.Spec pairs this exact json tag). Always non-nil, so a
	// spec with none ships "config_files":[] and never null. Content is
	// operator text that validateRuntimeSpecConfigFiles already vetted at
	// submit time (no secret placeholders), and like Env it may carry
	// ${PORT}/${MODEL}-style placeholders the agent resolves locally --
	// passed through here untouched.
	ConfigFiles []ConfigFileDTO `json:"config_files"`
	// Replicable / MinInstances / MaxInstances are the spec's instance
	// settings (#340; the agent-side runtime.Spec pairs these exact json
	// tags): whether the agent may run several instances of the spec, how
	// many it keeps up (0 = none) and how many it may run (0 = unbounded).
	// All three are omitempty and set only for a replicable spec pushed to an
	// agent that declares the runtime_instances feature (Service.specAsPushed;
	// a document held for an agent not heard from yet carries them too, and
	// reaches no agent), so the document of a spec that is not replicable, and
	// every document an older agent gets, is the one it was before instances
	// existed. An agent with the feature honours replicable only while this
	// gateway declares that feature too. The per-instance parallelism is the
	// gateway's own cap and is never pushed.
	Replicable   bool `json:"replicable,omitempty"`
	MinInstances int  `json:"min_instances,omitempty"`
	MaxInstances int  `json:"max_instances,omitempty"`
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
	// AwaitsAgentFeatures marks a document that depends on what serverID's
	// agent declares while the gateway has not heard that declaration: an
	// enabled replicable spec, whose instance settings and idle timeout differ
	// between an agent that runs instances and one that does not, on a server
	// whose agent has not declared its features since this gateway started (or
	// whose last declaration could not be read). Such a document is HELD, not
	// delivered: the gateway answers the agent's fetch with a 503, which the
	// agent meets by keeping the document it applied, and the push path sends
	// nothing; the telemetry ingest pushes the document once the declaration is
	// known. Serving the older agent's document instead would make an agent
	// that runs instances retire every instance beyond the first -- the
	// protected ones included -- until the declaration arrived.
	//
	// The specs of a held document are derived as stored, as for an agent that
	// runs instances (Service.specAsPushed). The flag itself is never on the
	// wire, and so not in the ETag: the document an instance agent gets once
	// the declaration is known is the held one, byte for byte.
	AwaitsAgentFeatures bool `json:"-"`
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
// should ever run). Coresident is the EFFECTIVE allowed-pair list: the
// application's co-residency mode and its explicit pair rows (stored as MAPPING
// id pairs, each allow or forbid) resolve to the SPEC id pairs that may be
// resident together -- see effectiveAllowedPairs. A pair whose either side has
// no enabled spec is dropped: it is meaningless to the agent, which only ever
// sees spec ids, never mapping ids, modes or verdicts.
//
// Per-GPU VRAMMB: max(measured, estimate) -- see agentRuntimeSpecDTO. 0 means
// unknown (both source fields are 0).
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
	// One batch capability read for every enabled spec's mapping, for the role
	// each spec is pushed with (no per-spec N+1). A store error is PROPAGATED like
	// every other read here: the agent keeps its last known-good config for one
	// skipped poll, where a role guessed from the type alone would flip, say, an
	// embedding model to chat for one push.
	mappingIDs := make([]string, 0, len(specs))
	for _, spec := range specs {
		if _, ok := mappingByID[spec.MappingID]; ok && spec.Enabled {
			mappingIDs = append(mappingIDs, spec.MappingID)
		}
	}
	capsByMapping, err := s.routes.MappingCapabilitiesForMappings(ctx, mappingIDs)
	if err != nil {
		return AgentRuntimeConfigDTO{}, err
	}
	specIDByMapping := make(map[string]string, len(specs))
	specDTOs := make([]AgentRuntimeSpecDTO, 0, len(specs))
	// A replicable spec makes the document depend on the agent's
	// runtime_instances declaration, so while that is unknown the document is
	// held (AgentRuntimeConfigDTO.AwaitsAgentFeatures).
	_, featuresKnown := s.agentRunsInstances(serverID)
	awaitsFeatures := false
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
		// The spec as this server's agent can run it: without the instance
		// settings for an agent that does not declare runtime_instances.
		pushed := s.specAsPushed(serverID, spec)
		awaitsFeatures = awaitsFeatures || (spec.Replicable && !featuresKnown)
		specDTO, err := agentRuntimeSpecDTO(pushed, mapping, gpus, s.effectiveIdleTimeout(serverID, spec, mapping))
		if err != nil {
			return AgentRuntimeConfigDTO{}, err
		}
		// The ONLY place the gateway decrypts the per-mapping token, and it does
		// so fail-closed (see resolvePushToken). agentApp is guaranteed non-nil
		// here -- the builder returns the empty document above when it is nil.
		specDTO.APIToken = s.resolvePushToken(spec, *agentApp)
		// Push the RESOLVED effective type + probe paths, not the raw stored
		// spec.Type/MetricsPath/ContextProbePath -- the agent needs a concrete
		// kind and concrete paths to probe, not "figure out the default
		// yourself" (see AgentRuntimeSpecDTO.Type doc).
		et := routing.EffectiveRuntimeSpecType(spec)
		mp, cp := routing.DeriveProbePaths(et, spec.MetricsPath, spec.ContextProbePath)
		specDTO.Type = string(et)
		specDTO.Role = string(routing.DeriveRole(et, routing.YesCapabilities(capsByMapping[spec.MappingID])))
		specDTO.MetricsPath = mp
		specDTO.ContextProbePath = cp
		specIDByMapping[spec.MappingID] = spec.ID
		specDTOs = append(specDTOs, specDTO)
	}
	// The co-residency setting is two inputs, the application's mode and its
	// explicit pair rows, read here like every other input (a failed read is an
	// error, never a guessed mode -- see the C1 note above). The agent still gets
	// ONE plain list of allowed pairs, computed from both.
	mode, err := s.routes.GetCoResidencyMode(ctx, agentApp.ID)
	if err != nil {
		return AgentRuntimeConfigDTO{}, err
	}
	rules, err := s.routes.CoResidencyRulesByApplication(ctx, agentApp.ID)
	if err != nil {
		return AgentRuntimeConfigDTO{}, err
	}
	coresident := effectiveAllowedPairs(mode, specIDByMapping, rules)
	dto := agentRuntimeConfigDTO(specDTOs, coresident, budgets, agentApp.Port, server.RuntimeMaxProcesses)
	dto.AwaitsAgentFeatures = awaitsFeatures
	return dto, nil
}

// effectiveAllowedPairs computes the list of spec-id pairs the agent may keep
// resident together -- the existing `coresident` wire field, whose shape
// ([][2]string of spec ids) is unchanged.
//
// The effective verdict of a pair of ENABLED specs is the design's tri-state
// rule: an explicit rule row decides (allow -> allowed, forbid -> not); a pair
// with no row follows the application's mode (opt_out -> allowed, opt_in ->
// not). Every pair that comes out allowed is ENUMERATED here, including all the
// opt_out defaults, rather than the mode being sent to the agent: an agent that
// predates the mode only reads this plain list, and an opt_out it could not see
// would degrade to "nothing may co-locate". An agent that does know about
// modes gets the same list, so neither needs the mode or the verdicts.
//
// specIDByMapping holds exactly the ENABLED specs, mapping id -> spec id, and is
// both the set of pairs considered and the translation out of the mapping-id
// space the rules are stored in: a rule that names a mapping with no enabled
// spec matches nothing, and a pair touching such a mapping is never emitted.
// Anything that is not unmistakably permissive resolves to "not allowed": a mode
// other than opt_out behaves as opt_in, a verdict other than allow behaves as
// forbid, because default-allow is the direction that can put two models on one
// GPU that do not fit. (The store validates both, so this only matters for a
// value the store should never return.)
//
// The result is deterministic -- pairs run over the mapping ids in ascending
// order -- and so is its orientation: each pair is [spec of the lower mapping
// id, spec of the higher], exactly what the former per-row translation emitted
// for a canonical row, so an opt_in application's document (and its ETag) is
// byte-identical to what it was before modes existed. The result is never nil.
func effectiveAllowedPairs(mode routing.CoResidencyMode, specIDByMapping map[string]string, rules []routing.CoResidencyRule) [][2]string {
	explicit := make(map[[2]string]bool, len(rules))
	for _, r := range rules {
		a, b := r.MappingAID, r.MappingBID
		if a > b {
			a, b = b, a
		}
		verdict, ok := routing.NormalizeCoResidencyVerdict(r.Verdict)
		explicit[[2]string{a, b}] = ok && verdict == routing.CoResidencyVerdictAllow
	}
	defaultAllowed := mode == routing.CoResidencyModeOptOut

	mappingIDs := slices.Sorted(maps.Keys(specIDByMapping))
	pairs := make([][2]string, 0)
	for i, a := range mappingIDs {
		for _, b := range mappingIDs[i+1:] {
			allowed, isExplicit := explicit[[2]string{a, b}]
			if !isExplicit {
				allowed = defaultAllowed
			}
			if allowed {
				pairs = append(pairs, [2]string{specIDByMapping[a], specIDByMapping[b]})
			}
		}
	}
	return pairs
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
// stored Args/Env/ConfigFiles is a store-data problem, not a client-input one, but still
// surfaces as the matching domain sentinel rather than a raw JSON error.
//
// The per-GPU VRAM demand it pushes is max(measured, estimate): the operator's
// estimate is a FLOOR and the agent's measurement raises it when reality is
// higher, never the other way round. That is the "never under-reserve" rule,
// and it matters here more than anywhere else because this value is not just
// displayed -- the agent reads it back AS THE SPEC'S OWN DECLARED DEMAND and
// feeds it into Admit, so a number that is too low is a co-resident pair that
// reaches an OOM.
//
// A 0 on either side is UNKNOWN, never "needs none": a measured 0 (never
// measured) yields the estimate, an estimate of 0 yields the measurement, and
// both 0 stays 0 so the agent still sees an unknown demand and bootstraps it.
// Neither stored figure is rewritten by this -- the estimate stays the
// operator's and the measurement stays the agent's.
//
// The spec's host-RAM demand (RAMMB) follows the identical rule over the spec's
// own two figures, RAMMeasuredMB and RAMEstimateMB: RAM is host-scoped, so it is
// ONE number on the spec rather than one per GPU row.
//
// idleSeconds is the idle timeout to push, the caller's effectiveIdleTimeout of
// the spec, and the instance settings are pushed for a replicable spec only.
func agentRuntimeSpecDTO(spec routing.RuntimeSpec, mapping routing.ModelMapping, gpus []routing.RuntimeSpecGPU, idleSeconds int) (AgentRuntimeSpecDTO, error) {
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
	configFiles, err := decodeRuntimeSpecConfigFiles(spec.ConfigFiles)
	if err != nil {
		return AgentRuntimeSpecDTO{}, err
	}
	gpuDTOs := make([]AgentRuntimeSpecGPUDTO, 0, len(gpus))
	for _, g := range gpus {
		// max(measured, estimate): the estimate is the floor, a larger
		// measurement raises it. A measured 0 is below any positive estimate
		// so it falls through to the estimate, and both 0 stays 0 (unknown).
		vram := g.VRAMMeasuredMB
		if vram < g.VRAMEstimateMB {
			vram = g.VRAMEstimateMB
		}
		gpuDTOs = append(gpuDTOs, AgentRuntimeSpecGPUDTO{Index: g.GPUIndex, VRAMMB: vram})
	}
	// Host-RAM demand: the same max(measured, estimate) floor as the VRAM above,
	// over the spec-level pair. Both 0 stays 0 (unknown).
	ram := max(spec.RAMMeasuredMB, spec.RAMEstimateMB)
	dto := AgentRuntimeSpecDTO{
		ID:                          spec.ID,
		Model:                       mapping.GatewayModelName,
		UpstreamModel:               mapping.AppModelName,
		Binary:                      spec.Binary,
		Args:                        args,
		Env:                         env,
		WorkDir:                     spec.WorkDir,
		GPUs:                        gpuDTOs,
		RAMMB:                       ram,
		ListenPort:                  spec.ListenPort,
		HealthPath:                  spec.HealthPath,
		HealthTimeoutSeconds:        spec.HealthTimeoutSeconds,
		StartupTimeoutSeconds:       spec.StartupTimeoutSeconds,
		IdleTimeoutSeconds:          idleSeconds,
		AdmissionWaitTimeoutSeconds: spec.AdmissionWaitTimeoutSeconds,
		Pinned:                      spec.Pinned,
		SetVisibleDevices:           spec.SetVisibleDevices,
		VisibleDevicesMode:          string(spec.VisibleDevicesMode),
		AdminState:                  spec.AdminState,
		ConfigFiles:                 configFiles,
	}
	// Counts stored on a spec that is not replicable mean nothing and are not
	// pushed: its document stays byte-for-byte the pre-instances one, ETag
	// included, and no agent is handed a count it would have to ignore.
	if spec.Replicable {
		dto.Replicable = true
		dto.MinInstances = spec.MinInstances
		dto.MaxInstances = spec.MaxInstances
	}
	return dto, nil
}

const (
	// DefaultInstanceIdleFactor and DefaultInstanceIdleFloorSeconds are the
	// factor and floor a replicable spec's idle timeout is derived with when
	// the service is built without them; they mirror the defaults of
	// OP_AI_GATEWAY_INSTANCE_IDLE_FACTOR and
	// OP_AI_GATEWAY_INSTANCE_IDLE_FLOOR_SECONDS (config.Config).
	DefaultInstanceIdleFactor       = 5.0
	DefaultInstanceIdleFloorSeconds = 120
	// maxDerivedIdleTimeoutSeconds caps the idle timeout pushed for a
	// replicable spec, every term of it: the startup-derived one, the manual
	// value and the floor. The agent turns the pushed seconds into a
	// time.Duration, which wraps a little past 292 years, and a wrapped timeout
	// is an instance unloaded at once; the cap, about 68 years, is "never" in
	// practice and leaves that arithmetic room. Only an absurd measurement,
	// factor or setting gets near it.
	maxDerivedIdleTimeoutSeconds = math.MaxInt32
)

// instanceIdleDefaults is NewService's fallback for the two idle knobs: a
// factor that is not a positive, finite number, or a floor that is not
// positive -- the zero value of a deps literal that omits them included --
// uses DefaultInstanceIdleFactor or DefaultInstanceIdleFloorSeconds. A floor
// above maxDerivedIdleTimeoutSeconds is held at it, for the reason that cap
// exists.
func instanceIdleDefaults(factor float64, floorSec int) (float64, int) {
	if math.IsNaN(factor) || math.IsInf(factor, 0) || factor <= 0 {
		factor = DefaultInstanceIdleFactor
	}
	if floorSec <= 0 {
		floorSec = DefaultInstanceIdleFloorSeconds
	}
	return factor, min(floorSec, maxDerivedIdleTimeoutSeconds)
}

// effectiveIdleTimeoutSeconds is the idle timeout the runtime-config document
// pushes for spec, whose mapping's benchmark cold start is loadTimeMS. A spec
// that is not replicable gets its stored IdleTimeoutSeconds as it is, 0 still
// meaning "never unload". A replicable one gets the largest of its stored
// value -- where 0 means "no manual override", not "never" -- factor times its
// startup in seconds, rounded up, and floorSec. The startup is the slower of
// the agent's measured startup and the benchmark's cold start, so an instance
// that is slow to bring up is kept longer once it runs, and the floor holds
// every instance at least that long, also while nothing is measured: a
// replicable spec is never pushed 0, so its unprotected instances always idle
// out. The result is held at maxDerivedIdleTimeoutSeconds.
func effectiveIdleTimeoutSeconds(spec routing.RuntimeSpec, loadTimeMS int64, factor float64, floorSec int) int {
	if !spec.Replicable {
		return spec.IdleTimeoutSeconds
	}
	derivedSec := derivedIdleSeconds(factor, max(spec.StartupMeasuredMS, loadTimeMS))
	return min(max(spec.IdleTimeoutSeconds, derivedSec, floorSec), maxDerivedIdleTimeoutSeconds)
}

// derivedIdleSeconds is factor times startupMS in whole seconds, rounded up,
// held at maxDerivedIdleTimeoutSeconds.
//
// The product is taken to the nearest whole millisecond first and the
// rounding up to seconds is integer arithmetic. A factor such as 1.1 has no
// exact binary form, so in floating point 1.1 x 200000 ms is
// 220000.00000000003, and rounding that up to seconds directly answers 221
// where the exact answer is 220. A millisecond is far finer than anything a
// startup measurement can tell apart, so the rounding costs nothing real.
//
// The saturation comes before the conversion, because Go leaves the int value
// of an out-of-range float to the platform.
func derivedIdleSeconds(factor float64, startupMS int64) int {
	if startupMS <= 0 {
		return 0
	}
	productMS := math.Round(factor * float64(startupMS))
	if productMS >= float64(maxDerivedIdleTimeoutSeconds)*1000 {
		return maxDerivedIdleTimeoutSeconds
	}
	ms := int64(productMS)
	return int((ms + 999) / 1000)
}

// AgentFeatureRuntimeInstances is the feature an agent declares when it runs
// several instances of a replicable spec and serves them as "m#k". It is the
// module's one spelling of the name: the gateway package declares its own
// runtime_instances feature and reads the agent's declaration by this
// constant (its runtimeInstancesFeature is defined as it), so the document
// built here and the declaration the ingest records cannot drift apart.
const AgentFeatureRuntimeInstances = "runtime_instances"

// agentRunsInstances reports whether serverID's agent declares
// AgentFeatureRuntimeInstances, and whether this gateway knows its declaration
// at all (AgentFeatureReader.Known). A nil reader knows every agent, as one
// without the feature.
func (s *Service) agentRunsInstances(serverID string) (runs, known bool) {
	if s.agentFeatures == nil {
		return false, true
	}
	if !s.agentFeatures.Known(serverID) {
		return false, false
	}
	return s.agentFeatures.Has(serverID, AgentFeatureRuntimeInstances), true
}

// specAsPushed is spec as serverID's runtime-config document carries it:
// unchanged for an agent that declares AgentFeatureRuntimeInstances, and for
// an agent known to lack it with Replicable cleared, so the document has no
// instance keys and pushes the stored manual idle timeout -- exactly the
// document it was before instances existed. An agent without the feature runs
// every spec as one process whatever the document says, so a derived idle
// timeout would only unload that one process after a span the operator never
// set, where a manual 0 meant "never".
//
// An agent the gateway has not heard from yet (after a gateway start, until
// its first telemetry sample) is neither, and keeps the spec as stored: a
// document with a replicable spec is then held rather than served
// (AgentRuntimeConfigDTO.AwaitsAgentFeatures), and the portal's echo of the
// effective idle timeout shows the value the operator's settings derive. Its
// first telemetry sample makes the declaration known, and the ingest pushes
// the document then (gateway.Server.ingestTelemetrySample).
//
// The stored spec, and the portal's view of its settings, keep Replicable as
// the operator saved it; only the derived values follow this.
func (s *Service) specAsPushed(serverID string, spec routing.RuntimeSpec) routing.RuntimeSpec {
	if runs, known := s.agentRunsInstances(serverID); known && !runs {
		spec.Replicable = false
	}
	return spec
}

// effectiveIdleTimeout is effectiveIdleTimeoutSeconds for spec on mapping, as
// serverID's document pushes it (specAsPushed), under this gateway's configured
// factor and floor. The pushed document and the portal's read-only echo both
// derive the value here, so they cannot disagree.
func (s *Service) effectiveIdleTimeout(serverID string, spec routing.RuntimeSpec, mapping routing.ModelMapping) int {
	return effectiveIdleTimeoutSeconds(s.specAsPushed(serverID, spec), int64(mapping.LoadTimeMS), s.instanceIdleFactor, s.instanceIdleFloorSeconds)
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
