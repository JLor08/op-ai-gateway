// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package runtime is the agent-side vocabulary for the agent-managed model
// runtime: the wire mirror of the gateway's runtime-config document (the
// shipped, field-by-field description is
// docs/architecture/reference/api-surface.md §5.1), the per-spec
// visible-load-lifecycle state machine
// (docs/architecture/cross-cutting/agent-runtime-manager.md §6 "Process
// lifecycle"), and the admission policy (same file, §5 "Admission control";
// the purity rule is §5.5 "One serialized owner") as pure functions over
// snapshots -- no clocks, no I/O, no goroutines. Task 14 owns starting
// processes, opening sockets, and reading files; this package only decides.
//
// NOTE for importers: this package's name shadows the standard library's
// "runtime" package. That is harmless from inside this package, but a
// caller that needs both must import one under an alias -- a later task
// imports this one as "runtimectl".
package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"op-ai-server-agent/internal/wslentry"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// SpecGPU is one GPU a Spec's process is launched to use.
type SpecGPU struct {
	Index  int `json:"index"`
	VRAMMB int `json:"vram_mb"` // 0 = unknown demand, never a real zero-cost claim
}

// ConfigFile is one operator-supplied file the agent materialises for a spec
// before launching its process (#257): a bare file Name and its Content. It is
// the agent-local pairing of the gateway's ConfigFileDTO -- the json tags are
// the wire contract and must match it exactly, and the gateway package is not
// imported (the arch test freezes this package's import graph).
//
// ParseConfig guarantees every Name it lets through is one plain file name
// (validConfigFileName) and unique within its spec; it says nothing about the
// Content, which the gateway bounds (count and size) and the writer treats as
// opaque bytes.
type ConfigFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// Spec is one ENABLED launch spec inside the runtime-config document --
// everything the agent needs to exec and supervise a model process. Mirrors
// the gateway's AgentRuntimeSpecDTO
// (gateway/backend/internal/portal/service_runtime.go) field-for-field.
//
// RAMMB is the spec's effective host-RAM demand in MB, the gateway's
// max(measured, estimate). It is spec-level because RAM is a host resource, not
// a per-card one: it sits beside GPUs and never inside a SpecGPU row. 0 means
// UNKNOWN demand -- the gateway holds neither a measurement nor an estimate, or
// it is an older gateway that never sends the field -- and never "needs no RAM".
// Like SpecGPU.VRAMMB it is an admission DEMAND, not a launch input
// (sameLaunchShape ignores it).
type Spec struct {
	ID                          string            `json:"id"`
	Model                       string            `json:"model"`
	UpstreamModel               string            `json:"upstream_model"`
	Binary                      string            `json:"binary"`
	Args                        []string          `json:"args"`
	Env                         map[string]string `json:"env"`
	ConfigFiles                 []ConfigFile      `json:"config_files"` // #257; non-nil after ParseConfig, names validated there
	WorkDir                     string            `json:"work_dir"`
	GPUs                        []SpecGPU         `json:"gpus"`
	RAMMB                       int               `json:"ram_mb"` // host-RAM demand (max(measured, estimate)); 0 = unknown, never "needs none"
	ListenPort                  int               `json:"listen_port"`
	HealthPath                  string            `json:"health_path"`
	HealthTimeoutSeconds        int               `json:"health_timeout_seconds"`
	StartupTimeoutSeconds       int               `json:"startup_timeout_seconds"`
	IdleTimeoutSeconds          int               `json:"idle_timeout_seconds"`
	AdmissionWaitTimeoutSeconds int               `json:"admission_wait_timeout_seconds"`
	Pinned                      bool              `json:"pinned"`
	// SetVisibleDevices asks the agent to CONSTRAIN this spec's child to the
	// cards in GPUs, by setting the vendor-appropriate visibility variable
	// (VisibleDevicesVar) to those HOST indices. Without it, GPUs is a
	// DECLARATION only -- it drives admission arithmetic and the measurement
	// mapping, and nothing stops the child from landing on a different card,
	// leaving the accounting confidently wrong with no warning anywhere.
	// The rules and the four traps live on ExpandPlaceholders.
	SetVisibleDevices bool `json:"set_visible_devices"`
	// VisibleDevicesMode chooses HOW SetVisibleDevices enforces, and is only
	// meaningful when it is on. "env" (the default) injects the vendor
	// visibility variable, exactly as before. "args" injects NOTHING: the
	// operator selects devices through a ${CUDA_DEVICES}/${VULKAN_DEVICES}/
	// ${METAL_DEVICES} placeholder in Args, and the child sees every card so
	// the placeholder's host-index numbering is what --device consumes. An
	// empty or unknown value is env-mode -- the behavior an older gateway that
	// never sends this field produces.
	VisibleDevicesMode string `json:"visible_devices_mode"`
	AdminState         string `json:"admin_state"` // "" | "force_running" | "force_stopped"
	// APIToken is the DECRYPTED upstream token the gateway resolved for this
	// spec's api_token_mode (empty for off / app-unset). The agent substitutes
	// it wherever the operator wrote ${API_TOKEN} in Env/Args, and masks it in
	// the reported command. Never persisted by the agent; redacted in file-mode
	// reports (report.go).
	APIToken string `json:"api_token"`
	// Type is the RESOLVED effective runtime-server kind for this spec
	// (vllm/llama_cpp/tgi/ollama/custom) -- the gateway's
	// AgentRuntimeSpecDTO.Type, which is always the explicit spec.Type when
	// set, else the type auto-detected from Binary. Never empty on a spec the
	// gateway actually pushes.
	Type string `json:"type"`
	// MetricsPath and ContextProbePath are the RESOLVED effective probe
	// paths the gateway derived for Type -- AgentRuntimeSpecDTO's fields of
	// the same name: the stored per-spec override when set, else Type's own
	// default (which may itself be empty, e.g. custom or ollama's metrics
	// path).
	MetricsPath      string `json:"metrics_path"`
	ContextProbePath string `json:"context_probe_path"`
	// Role is the model's derived functional role, as the gateway pushes it
	// (AgentRuntimeSpecDTO.Role): "asr", "tts", "image", "embedding",
	// "reranker", "chat" or "other". It is carried verbatim -- no vocabulary
	// check here, so a role a newer gateway invents decodes and groups with its
	// own kind -- and an older gateway, or a hand-written file-mode spec, simply
	// omits it, which decodes to "" and is backward-safe. Read it through
	// roleOf, never raw: that is what folds the absent role into RoleOther.
	//
	// Like RAMMB it is an admission input, not a launch input (sameLaunchShape
	// ignores it): the policy reads it to choose WHICH idle model to evict, and
	// it changes nothing about how a process is exec'd.
	Role string `json:"role,omitempty"`
	// Replicable opts the spec into running SEVERAL instances (processes) of
	// itself (#340): instance 0 is the spec's one process as ever, and further
	// instances (k = 1...) are started on demand as burst capacity. False -- the
	// default, and every document an older gateway sends -- keeps exactly the
	// single process of today. Every instance needs its own port, so ParseConfig
	// demotes a spec that pins a fixed ListenPort to non-replicable.
	Replicable bool `json:"replicable,omitempty"`
	// MinInstances is the number of instances a replicable spec keeps up as its
	// protected floor (never reclaimed by idle timeout or eviction); 0 = no
	// floor beyond the pin/force_running baseline. MaxInstances caps how many
	// instances the spec may have; 0 = no cap beyond MaxInstanceIndex. Read both
	// through ProtectedCount and InstanceAllowed, never raw: ParseConfig only
	// sanitises them (negative reads as 0, a floor above the cap is clamped to
	// the cap) and they mean nothing for a non-replicable spec.
	//
	// Like Replicable they decide HOW MANY processes run, never what a process
	// is, so sameLaunchShape ignores all three.
	MinInstances int `json:"min_instances,omitempty"`
	MaxInstances int `json:"max_instances,omitempty"`
	// InstanceOf and Instance mark the per-instance copy of a replicable spec
	// the manager admits and starts for instance k >= 1 (#340): its ID is then
	// the instance key S#k, InstanceOf the base spec's ID S, and Instance k.
	// Instance 0 is the spec itself and keeps both at the zero value, which is
	// also what every spec that never replicates has. Admission reads the base
	// through InstanceOf -- an instance co-resides with its siblings, never
	// evicts them, and inherits its spec's co-residency verdicts (see Admit) --
	// so the copy needs no matrix entries of its own.
	//
	// The agent derives both; they are never on the wire, because the gateway
	// pushes base specs only.
	InstanceOf string `json:"-"`
	Instance   int    `json:"-"`

	// Kind says what the spec runs: SpecKindModel ("", every document an older
	// gateway sends) is an ordinary model process; SpecKindRPCServer is a
	// llama.cpp rpc-server member of another spec's cluster, which serves no
	// model, has no upstream and no health/metrics surface of its own. The
	// agent honours it only while the gateway declares rpc_cluster
	// (withoutRPCCluster).
	Kind string `json:"kind,omitempty"`
	// DeviceBackend names the compute backend an rpc_server member's binary
	// was built for ("", "auto", "cuda", "rocm", "vulkan", "metal", "sycl";
	// "" means "auto"). It decides which device names the member and the head
	// must agree on, so it is launch-affecting.
	DeviceBackend string `json:"device_backend,omitempty"`
	// Cluster, on a model spec, makes it a cluster HEAD: its llama-server
	// offloads to the remote rpc-server endpoints listed here, in order (the
	// order fixes the RPC<n> device numbering, so reordering is a relaunch).
	Cluster *ClusterHead `json:"cluster,omitempty"`
	// RPC, on an rpc_server spec, carries the member's network settings: the
	// head it serves, how it is exposed, where it binds and who may connect.
	RPC *RPCMember `json:"rpc,omitempty"`
	// Execution says WHERE the spec's process runs (#381). nil and mode
	// "native" are the host the agent itself runs on, which is every document
	// an older gateway sends; mode "wsl" runs it inside a WSL2 distribution on
	// a Windows host. It decides what is exec'd and how, so it is
	// launch-affecting (sameLaunchShape). The agent honours a non-native mode
	// only while it declares the matching feature (runtime_exec_wsl), and a
	// spec whose block it cannot run is dropped at parse time
	// (invalidExecutionReason).
	Execution *Execution `json:"execution,omitempty"`
}

// Execution is a spec's execution-environment block. Mode is
// ExecutionModeNative ("" reads as native) or ExecutionModeWSL; WSL carries the
// mode's settings and is required exactly when Mode is "wsl".
type Execution struct {
	Mode string        `json:"mode"`
	WSL  *WSLExecution `json:"wsl,omitempty"`
}

// WSLExecution names the WSL2 distribution a "wsl" spec runs in. The name is
// the gateway's choice and reaches a wsl.exe argument, so it is validated
// (wslentry.DistroPattern) rather than trusted.
type WSLExecution struct {
	Distro string `json:"distro"`
}

// ExecutionModeNative and ExecutionModeWSL are the Execution.Mode values this
// agent knows. Any other value is dropped (invalidExecutionReason): a newer
// gateway's mode may need handling this agent lacks.
const (
	ExecutionModeNative = "native"
	ExecutionModeWSL    = "wsl"
)

// IsWSL reports whether the spec runs inside a WSL2 distribution: its
// execution block says "wsl" and names a distro.
func (s Spec) IsWSL() bool {
	return s.Execution != nil && s.Execution.Mode == ExecutionModeWSL && s.Execution.WSL != nil
}

// WSLDistro returns the WSL distribution the spec runs in, "" when it is not a
// WSL spec.
func (s Spec) WSLDistro() string {
	if !s.IsWSL() {
		return ""
	}
	return s.Execution.WSL.Distro
}

// SpecKindModel is the Kind of an ordinary model spec (the zero value, so a
// document without the field decodes to it); SpecKindRPCServer is the Kind of
// a llama.cpp rpc-server cluster member.
const (
	SpecKindModel     = ""
	SpecKindRPCServer = "rpc_server"
)

// ClusterEndpoint is one remote rpc-server a head offloads to: Address is
// "host:port" as the head's llama-server receives it in --rpc, DeviceCount how
// many devices that server exposes (it contributes RPC<n>.. device names).
type ClusterEndpoint struct {
	Address     string `json:"address"`
	DeviceCount int    `json:"device_count"`
}

// ClusterHead is a head spec's cluster description: the ordered endpoints, how
// the members are reached (Transport "direct" | "relay") and over which network
// ("mesh" | "lan"). It is carried verbatim; the vocabulary is the gateway's.
// Generation is an opaque value the gateway derives from its members'
// configuration (binaries, versions, device counts, transport, network). Being
// part of the cluster description it is launch-affecting: a member-side fix
// changes the head spec, which lifts a terminal verdict the head rests under
// (a version mismatch, say) and relaunches it, although the endpoints read the
// same.
type ClusterHead struct {
	Endpoints  []ClusterEndpoint `json:"endpoints"`
	Transport  string            `json:"transport"`
	Network    string            `json:"network"`
	Generation string            `json:"generation,omitempty"`
}

// RPCMember is an rpc_server spec's network settings. Port is the rpc-server's
// own port (direct) or the relay's listen port (relay). HeadIPs and HeadDomain
// are the connection ALLOWLIST (HeadDomain is resolved by the agent when the
// head's address is only known by name); both are applied live, so changing
// them never relaunches the member (sameLaunchShape).
type RPCMember struct {
	HeadSpecID   string   `json:"head_spec_id"`
	Transport    string   `json:"transport"`
	BindIP       string   `json:"bind_ip"`
	Port         int      `json:"port"`
	HeadIPs      []string `json:"head_ips"`
	HeadDomain   string   `json:"head_domain"`
	CacheEnabled bool     `json:"cache_enabled"`
}

// IsClusterHead reports whether the spec is a cluster head: it lists at least
// one remote rpc-server endpoint.
func (s Spec) IsClusterHead() bool {
	return s.Cluster != nil && len(s.Cluster.Endpoints) > 0
}

// IsRPCServer reports whether the spec is a usable rpc_server member: its kind
// says so and it carries the member settings.
func (s Spec) IsRPCServer() bool {
	return s.Kind == SpecKindRPCServer && s.RPC != nil
}

// RoleOther is the role of a spec that has none the agent recognises: the value
// the gateway derives for a model with no recognisable function (its
// routing.RoleOther), and the value an absent role is read as (roleOf). The
// agent module cannot import the gateway's vocabulary, so this literal is the
// agent's own spelling of it and TestRoleOfNormalizesTheCapturedRole pins it.
const RoleOther = "other"

// roleOf is the one place a spec's captured Role becomes the role the admission
// policy compares: trimmed, lower-cased, and an empty result folded into
// RoleOther. Without the fold, an absent role on one spec next to a pushed
// "other" on another -- a hand-written file-mode document can mix the two --
// would read as two different roles, and the policy compares roles for equality.
// Every other value passes through, so an unknown role is simply its own group.
func roleOf(role string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		return RoleOther
	}
	return role
}

// VisibleDevicesMode wire values. The agent distinguishes only "args" from
// everything else; these constants keep the comparison off a magic string.
const (
	VisibleDevicesModeEnv  = "env"
	VisibleDevicesModeArgs = "args"
)

// GPUBudget is one per-GPU VRAM budget row.
type GPUBudget struct {
	Index    int `json:"index"`
	BudgetMB int `json:"budget_mb"`
}

// Config is the runtime-config document served by
// GET /api/agent/v1/runtime-config: which model processes may run, with
// what command line, on which GPUs, which pairs may be co-resident, and the
// per-GPU VRAM budgets.
type Config struct {
	RouterListen int         `json:"router_listen"`
	MaxProcesses int         `json:"max_processes"`
	GPUBudgets   []GPUBudget `json:"gpu_budgets"`
	Specs        []Spec      `json:"specs"`
	// Coresident holds spec-ID pairs, already translated upstream, in WIRE
	// order (whichever order the gateway happened to send each pair in).
	// Do NOT build a PolicySnapshot.Allowed lookup map directly from this
	// slice -- call AllowedPairs() instead, which canonicalizes every pair
	// via PairKey. A map built from raw pairs looks up correctly only in
	// whichever direction the wire happened to use, which presents as a
	// random, hard-to-reproduce matrix failure.
	Coresident [][2]string `json:"coresident"`
	ETag       string      `json:"etag"`
}

// ParseErrorCode is the CLOSED set of classification codes the agent may
// report for a failed local runtime-config load. It is a wire contract, not
// an internal enum: the value travels upward in a file-mode report's
// parse_error field, the gateway allow-lists it verbatim
// (redactRuntimeReportParseError in
// gateway/backend/internal/gateway/agent_runtime.go), and the portal maps it
// to a localized sentence.
//
// WHY A CODE AND NOT THE ERROR TEXT (the C2 fix). parse_error exists so an
// operator learns roughly WHY their local file was not adopted, and the
// gateway must never store text an agent chose freely -- a config-loader
// error routinely quotes the offending source line, which in this schema can
// be a plaintext secret. The gateway used to reconcile those two by keeping
// only the text before the first ':' when it looked like a bare
// classification token. Against THIS producer that rule had exactly one
// reachable output: every error ParseConfig can return begins "runtime: ",
// so every operator, for every malformed file, read the single word
// "runtime" -- a token that looks like a meaningful subsystem tag and
// carries no information at all. Redaction and diagnosis were fighting each
// other, and redaction won completely.
//
// A closed set ends that fight: the agent states what the field may contain,
// the gateway allow-lists exactly those values (anything else degrades to
// its generic constant, so a buggy or compromised agent still cannot put
// free text in front of an operator), and the portal owns the wording.
//
// THE SET IS WIDER THAN ITS NAME. parse_error is the wire field's name and
// stays that way (renaming it would break the gateway and every stored
// report), but the subject is "why the local file was not adopted", and two
// of the codes below -- file_missing, read_failed -- are raised before any
// parsing happens. FileSource.Load once returned those two cases silently,
// which put a missing runtime.json and a server with no specs at all on the
// same blank screen.
//
// ADDING A CODE IS A THREE-SIDED CHANGE. A new code must be added here, to
// the gateway's allow-list, and to the portal's i18n label map (German and
// English together) -- a code the gateway does not know degrades to the
// generic message, which is safe but silently drops the diagnosis this
// mechanism exists to deliver. TestParseConfigErrorsAreAllClassified pins
// that every error ParseConfig can actually produce carries one of these,
// and pins the literal wire value of every code in the set.
type ParseErrorCode string

const (
	// ParseErrorJSONSyntax: the bytes are not valid JSON, or a value does
	// not fit its field's type. Every stdlib json.Unmarshal failure --
	// SyntaxError and UnmarshalTypeError alike -- lands here: the two are
	// not worth separating for an operator who has to open the file either
	// way, and Config/Spec/GPUBudget declare no custom UnmarshalJSON, so
	// those two are the whole surface.
	ParseErrorJSONSyntax ParseErrorCode = "json_syntax"
	// ParseErrorDuplicateSpecID: two specs share an id. Structural, and
	// deliberately fatal to the whole document -- see ParseConfig.
	ParseErrorDuplicateSpecID ParseErrorCode = "duplicate_spec_id"
	// ParseErrorInvalidConfigFileName: a spec's config_files carries a name
	// that is not one plain file name (path separator, "." / "..", drive or
	// stream marker, control character, blank) or repeats another name in the
	// same spec, compared case-insensitively. Structural and fatal to the
	// whole document, like a duplicate spec id: the gateway enforces the same
	// rules at submit time, so only a hand-authored file-mode runtime.json can
	// reach it.
	ParseErrorInvalidConfigFileName ParseErrorCode = "invalid_config_file_name"
	// ParseErrorFileMissing: nothing exists at the configured path. The
	// file never got parsed, so this is not a parse failure at all -- it
	// rides the same field because the field's real subject is "why the
	// local file was not adopted", and the alternative was the silence
	// this code replaced (FileSource.Load used to swallow every stat and
	// read failure, so a missing file was indistinguishable in the portal
	// from a server with no specs at all).
	//
	// Kept SEPARATE from ParseErrorReadFailed because the two point an
	// operator at different things: a missing file is a path typo, a
	// failed deploy, or a legitimately fresh install, while a file that is
	// there but unreadable is an ownership/permission fault that is never
	// legitimate.
	ParseErrorFileMissing ParseErrorCode = "file_missing"
	// ParseErrorReadFailed: the path resolves to something, but the agent
	// could not stat or read it -- permissions, a non-directory component,
	// an I/O error, a file replaced by a directory. Every access failure
	// that is not "not there" lands here; no attempt is made to split it
	// further, because from the operator's side the next step is the same
	// one (look at the file and its directory on the host).
	ParseErrorReadFailed ParseErrorCode = "read_failed"
	// ParseErrorUnclassified is the defensive floor for a load failure that
	// carries no *ParseError at all. Nothing produces it today (every
	// ParseConfig return path is classified, and a test pins that), and it
	// is deliberately NOT in the gateway's allow-list: if it ever appears
	// it degrades to the gateway's generic message, which is the honest
	// rendering of "the agent could not say why".
	ParseErrorUnclassified ParseErrorCode = "unclassified"
)

// classifyFileAccessError maps a stat/read failure from FileSource.Load onto
// the closed set. It never looks at err's TEXT -- the text names the path and
// may name more, and the whole point of the closed set is that the wire
// carries no agent-chosen prose (see ParseErrorCode).
func classifyFileAccessError(err error) ParseErrorCode {
	if err == nil {
		return ""
	}
	if errors.Is(err, fs.ErrNotExist) {
		return ParseErrorFileMissing
	}
	return ParseErrorReadFailed
}

// ParseError is the error ParseConfig returns: a Code from the closed set
// above for the wire, plus the full underlying detail for LOCAL diagnosis
// only. Err is never sent upward -- it is exactly the text that may quote
// the operator's file.
type ParseError struct {
	Code ParseErrorCode
	Err  error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("runtime: parse config [%s]: %v", e.Code, e.Err)
}

func (e *ParseError) Unwrap() error { return e.Err }

// ClassifyParseError extracts the wire code from a config-load error. Any
// error that is not (and does not wrap) a *ParseError classifies as
// ParseErrorUnclassified rather than leaking its text.
func ClassifyParseError(err error) ParseErrorCode {
	if err == nil {
		return ""
	}
	var pe *ParseError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ParseErrorUnclassified
}

// ParseConfig unmarshals and validates raw into a Config.
//
// Tolerant of unknown top-level fields (forward compatibility with a newer
// gateway -- plain json.Unmarshal already ignores fields a struct has no tag
// for, so no DisallowUnknownFields is used here).
//
// Intolerant of structural nonsense:
//   - a duplicate spec ID is rejected as an error. The gateway is not
//     supposed to ever emit one; silently keeping one of the two would mean
//     launching the wrong process for whichever mapping lost.
//   - a config_files entry whose name is not one plain file name, or that
//     repeats another name in the same spec (case-insensitively), is rejected
//     as an error too (ParseErrorInvalidConfigFileName). This is the ONLY
//     guard on the file-mode path -- a hand-authored runtime.json never went
//     through the gateway's submit-time validation -- and it applies the same
//     rules the gateway does (validConfigFileName), no stricter: this is the
//     single parser for both modes, so a name the gateway stored must never
//     fail here and take the whole document down. Platform-specific
//     restrictions (Windows device names, trailing dot or space) belong to the
//     writer, which fails just the one spec. The file count and size limits are
//     the gateway's and are not re-checked here.
//   - a coresident pair naming a spec ID that is not (or no longer) present
//     in Specs is dropped silently, and the rest of the document -- other
//     specs, other pairs -- remains usable. This is reachable from an
//     ordinary stale/racy read on the gateway side, not a defect worth
//     failing the whole config over.
//   - a spec whose ID contains the instance separator '#' is dropped, with a
//     Warn naming it, and the rest of the document stays usable (#340):
//     "S#k" is the agent's key for instance k of spec S (InstanceKey), so such
//     an ID would name another spec's instance -- its state, its config
//     directory, its admission identity -- and could never be told apart from
//     it. A gateway-issued spec ID never contains '#'; only a hand-written
//     file-mode document can reach this, so one bad spec costs that spec, not
//     the whole document. Its coresident pairs go with it.
//
// Every collection-shaped field (Specs, GPUBudgets, Coresident, and each
// Spec's Args/Env/GPUs/ConfigFiles) is normalized to non-nil, so a caller that
// ranges over it or re-marshals it (file mode) never has to nil-check first.
func ParseConfig(raw []byte) (Config, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, &ParseError{Code: ParseErrorJSONSyntax, Err: err}
	}

	specIDs := make(map[string]bool, len(cfg.Specs))
	for _, spec := range cfg.Specs {
		if specIDs[spec.ID] {
			return Config{}, &ParseError{Code: ParseErrorDuplicateSpecID, Err: fmt.Errorf("duplicate spec id %q", spec.ID)}
		}
		specIDs[spec.ID] = true
	}
	cfg.Specs = withoutReservedSpecIDs(cfg.Specs)
	for id := range specIDs {
		if reservedSpecID(id) {
			delete(specIDs, id)
		}
	}

	var invalid []string
	cfg.Specs, invalid = withoutInvalidKinds(cfg.Specs)
	for _, id := range invalid {
		delete(specIDs, id)
	}

	if cfg.Specs == nil {
		cfg.Specs = []Spec{}
	}
	if cfg.GPUBudgets == nil {
		cfg.GPUBudgets = []GPUBudget{}
	}

	filteredPairs := make([][2]string, 0, len(cfg.Coresident))
	for _, pair := range cfg.Coresident {
		if specIDs[pair[0]] && specIDs[pair[1]] {
			filteredPairs = append(filteredPairs, pair)
		}
	}
	cfg.Coresident = filteredPairs

	for i := range cfg.Specs {
		if cfg.Specs[i].Args == nil {
			cfg.Specs[i].Args = []string{}
		}
		if cfg.Specs[i].Env == nil {
			cfg.Specs[i].Env = map[string]string{}
		}
		if cfg.Specs[i].GPUs == nil {
			cfg.Specs[i].GPUs = []SpecGPU{}
		}
		if cfg.Specs[i].ConfigFiles == nil {
			cfg.Specs[i].ConfigFiles = []ConfigFile{}
		}
		if err := checkConfigFileNames(cfg.Specs[i]); err != nil {
			return Config{}, err
		}
		sanitizeInstanceFields(&cfg.Specs[i])
	}

	return cfg, nil
}

// reservedSpecID reports whether id cannot be a spec ID because it contains
// the instance separator: "S#k" is the agent's key for instance k of spec S
// (InstanceKey), so a spec with such an ID would collide with another spec's
// instance. The gateway never issues one (its spec IDs are "rspec_" plus hex).
func reservedSpecID(id string) bool {
	return strings.Contains(id, instanceSeparator)
}

// withoutReservedSpecIDs returns specs without any whose ID is reserved
// (reservedSpecID), logging a Warn naming each one it drops. It is the one rule
// both entry points apply: ParseConfig, for every document a Source parses, and
// the manager's applyConfig, for a Config handed to Apply directly. specs
// itself is never modified; when nothing is dropped it is returned as is.
func withoutReservedSpecIDs(specs []Spec) []Spec {
	dropped := false
	for _, spec := range specs {
		if reservedSpecID(spec.ID) {
			dropped = true
			break
		}
	}
	if !dropped {
		return specs
	}
	kept := make([]Spec, 0, len(specs))
	for _, spec := range specs {
		if reservedSpecID(spec.ID) {
			slog.Warn("runtime: spec id contains '#', which the agent reserves for instance keys (S#k); ignoring the spec",
				"spec", spec.ID)
			continue
		}
		kept = append(kept, spec)
	}
	return kept
}

// withoutInvalidKinds returns specs without any whose Kind the agent cannot run
// safely, logging a Warn naming each one, and the IDs it dropped. It never
// fails the document (one bad spec costs that spec, like a reserved ID), and a
// dropped spec's coresident pairs go with it (the caller removes its ID from
// the known set). An rpc_server member is invalid without member settings, with
// a port outside 1..65535, without a transport the agent knows (direct or
// relay: the relay and the connection observer each act on exactly one, so a
// member with any other value would run and never be held), with a bind_ip
// that is not an IP address, or with fields that only make sense on a model
// (listen_port, upstream_model): starting it as the model it half-describes
// would exec the rpc-server binary behind a proxy route. An unknown kind is
// dropped too, since a newer gateway's kind may need handling this agent lacks
// and falling back to "a model" would launch it wrongly.
func withoutInvalidKinds(specs []Spec) ([]Spec, []string) {
	var dropped []string
	kept := make([]Spec, 0, len(specs))
	for _, spec := range specs {
		if reason := invalidKindReason(spec); reason != "" {
			slog.Warn("runtime: ignoring spec with an invalid kind configuration", "spec", spec.ID, "kind", spec.Kind, "reason", reason)
			dropped = append(dropped, spec.ID)
			continue
		}
		if reason := invalidExecutionReason(spec); reason != "" {
			slog.Warn("runtime: ignoring spec with an invalid execution configuration", "spec", spec.ID, "reason", reason)
			dropped = append(dropped, spec.ID)
			continue
		}
		kept = append(kept, spec)
	}
	if len(dropped) == 0 {
		return specs, nil
	}
	return kept, dropped
}

// invalidExecutionReason names why spec's execution block is unusable, or ""
// when it is fine (absent, native, or a complete wsl block). A wsl spec needs a
// distro that matches wslentry.DistroPattern, and cannot be an rpc_server member or a
// cluster head: the cluster transports bind and dial host addresses the agent
// manages natively, which a process inside the WSL VM does not share. An
// unknown mode is dropped like an unknown kind, since falling back to "native"
// would launch a spec the gateway meant for somewhere else on the host.
func invalidExecutionReason(spec Spec) string {
	if spec.Execution == nil {
		return ""
	}
	switch spec.Execution.Mode {
	case "", ExecutionModeNative:
		return ""
	case ExecutionModeWSL:
		switch {
		case spec.Execution.WSL == nil:
			return "wsl execution has no wsl settings"
		case !wslentry.ValidDistro(spec.Execution.WSL.Distro):
			return "wsl execution distro is empty, longer than 64 characters or has characters outside A-Z a-z 0-9 . _ -"
		case spec.IsRPCServer() || spec.Cluster != nil:
			return "wsl execution does not support rpc cluster members or heads"
		}
		return ""
	default:
		return "unknown execution mode"
	}
}

// invalidKindReason names why spec's kind configuration is unusable, or ""
// when it is fine. See withoutInvalidKinds.
func invalidKindReason(spec Spec) string {
	switch spec.Kind {
	case SpecKindModel:
		return ""
	case SpecKindRPCServer:
		switch {
		case spec.RPC == nil:
			return "rpc_server spec has no rpc settings"
		case spec.RPC.Port <= 0 || spec.RPC.Port > 65535:
			return "rpc_server spec has an invalid rpc port"
		case spec.RPC.Transport != "direct" && spec.RPC.Transport != "relay":
			return "rpc_server spec has no known rpc transport (want direct or relay)"
		case net.ParseIP(spec.RPC.BindIP) == nil:
			return "rpc_server spec's rpc bind_ip is not an IP address"
		case spec.ListenPort != 0 || spec.UpstreamModel != "":
			return "rpc_server spec sets listen_port or upstream_model, which only a model has"
		}
		return ""
	default:
		return "unknown spec kind"
	}
}

// sanitizeInstanceFields normalises a spec's multi-instance opt-in (#340) so
// nothing downstream has to defend against a contradictory document. It never
// fails the document: every repair is a demotion to the safer, more
// conservative reading, logged at Warn so the operator can learn why the spec
// does not scale the way they wrote it (the gateway rejects the same inputs at
// submit time; only a hand-written file-mode document, or a gateway that
// predates the rule, can reach here).
//
//   - A replicable spec with a fixed listen_port is demoted to non-replicable:
//     every instance needs its own port, and one pinned port can serve one
//     process.
//   - A negative min_instances or max_instances reads as unset (0).
//   - A min_instances above a set max_instances is clamped to max_instances.
//   - A min_instances above the addressable range (MaxInstanceIndex+1
//     instances, 0..MaxInstanceIndex) is clamped to it, so the protected floor
//     (ProtectedCount) can never name an instance that cannot exist.
func sanitizeInstanceFields(spec *Spec) {
	if spec.Replicable && spec.ListenPort != 0 {
		slog.Warn("runtime: spec is replicable but pins a fixed listen_port; treating it as non-replicable (every instance needs its own port)",
			"spec", spec.ID, "listen_port", spec.ListenPort)
		spec.Replicable = false
	}
	if spec.MinInstances < 0 || spec.MaxInstances < 0 {
		slog.Warn("runtime: spec has a negative min_instances/max_instances; reading it as unset",
			"spec", spec.ID, "min_instances", spec.MinInstances, "max_instances", spec.MaxInstances)
		spec.MinInstances = max(spec.MinInstances, 0)
		spec.MaxInstances = max(spec.MaxInstances, 0)
	}
	if spec.MaxInstances > 0 && spec.MinInstances > spec.MaxInstances {
		slog.Warn("runtime: spec has min_instances above max_instances; clamping min_instances to max_instances",
			"spec", spec.ID, "min_instances", spec.MinInstances, "max_instances", spec.MaxInstances)
		spec.MinInstances = spec.MaxInstances
	}
	if limit := MaxInstanceIndex + 1; spec.MinInstances > limit {
		slog.Warn("runtime: spec has min_instances above the instance limit; clamping it to the limit",
			"spec", spec.ID, "min_instances", spec.MinInstances, "limit", limit)
		spec.MinInstances = limit
	}
}

// checkConfigFileNames validates spec's config_files names: each one plain
// file name (validConfigFileName) and unique within the spec, compared
// case-insensitively (the agent may write to a case-insensitive filesystem, so
// "App.json" and "app.json" would collide there). The error names the spec and
// the offending file name -- a name is an identifier -- and never the content.
// It is local-only detail: the wire carries just the ParseErrorCode.
func checkConfigFileNames(spec Spec) error {
	seen := make(map[string]struct{}, len(spec.ConfigFiles))
	for _, f := range spec.ConfigFiles {
		if !validConfigFileName(f.Name) {
			return &ParseError{Code: ParseErrorInvalidConfigFileName, Err: fmt.Errorf(
				"spec %q: config file name %q must be a single file name (no path separators, \"..\", drive or stream marker, or control characters)",
				spec.ID, f.Name)}
		}
		key := strings.ToLower(f.Name)
		if _, dup := seen[key]; dup {
			return &ParseError{Code: ParseErrorInvalidConfigFileName, Err: fmt.Errorf(
				"spec %q: duplicate config file name %q (names are compared case-insensitively)",
				spec.ID, f.Name)}
		}
		seen[key] = struct{}{}
	}
	return nil
}

// validConfigFileName reports whether name is one plain file name safe to join
// under a per-spec config directory: non-blank, not "." or "..", no path
// separator of either OS, no volume or alternate-data-stream marker (":"), and
// no control character.
//
// It is an agent-local copy of the gateway's configFileNameIsSingleSegment
// (gateway/backend/internal/portal/service_runtime.go) and MUST accept and
// reject exactly the same names -- not more, not fewer. The gateway applies
// that ruleset at submit time; ParseConfig is the single parser for gateway-
// and file-mode documents, so a stricter check here would fail a document the
// gateway already accepted and drop the whole runtime config, while a laxer one
// would let a hand-written file-mode name through. Keep the two in lockstep.
// (Not shared by import: the arch test freezes this package's import graph.)
//
// The separators and the colon are rejected explicitly rather than left to
// filepath.Base: Base is per-GOOS and the document may have been authored for
// the other OS. filepath.Base is kept as an extra guard, so a form the
// explicit rules missed still has to survive this OS's own idea of a base name.
func validConfigFileName(name string) bool {
	if strings.TrimSpace(name) == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\:`) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return false
	}
	return filepath.Base(name) == name && !filepath.IsAbs(name)
}

// AllowedPairs returns c.Coresident as a canonical PairKey set, ready to
// use as PolicySnapshot.Allowed. Coresident is kept in wire order (whatever
// order the gateway happened to send each pair in); Allowed is documented
// as a canonical (a<=b) set, so a consumer that instead inserted raw pairs
// verbatim would get a silently one-directional lookup -- Admit would find
// PairKey(candidate, running) missing exactly when the wire pair happened
// to name the two spec IDs in the other order, and would look like a
// random, hard-to-reproduce matrix failure. Going through this method
// instead of hand-rolling the map makes that class of bug impossible.
func (c Config) AllowedPairs() map[[2]string]bool {
	allowed := make(map[[2]string]bool, len(c.Coresident))
	for _, pair := range c.Coresident {
		allowed[PairKey(pair[0], pair[1])] = true
	}
	return allowed
}

// State is a Spec's visible load-lifecycle stage -- the nine-state wire
// contract in docs/architecture/cross-cutting/agent-runtime-manager.md §6
// "Process lifecycle".
type State string

const (
	StateStopped            State = "stopped"
	StateStarting           State = "starting" // loading: process up, health not yet green
	StateRunning            State = "running"  // loaded
	StateDraining           State = "draining"
	StateBackoff            State = "backoff" // crash-loop wait
	StateStartFailed        State = "start_failed"
	StateCrashed            State = "crashed"
	StatePendingVRAMUnknown State = "pending_vram_unknown"
	StateNotPermitted       State = "not_permitted"
)

// LastError records the most recent failed start or crash for a spec. It is
// cleared only by the NEXT SUCCESSFUL start, never merely by a state change
// (docs/architecture/cross-cutting/agent-runtime-manager.md §6): a spec can
// be StateStopped and still show "last load attempt failed, yesterday 14:32,
// exit code 1".
//
// JSON tags mirror sample.RuntimeErrorSample field-for-field (Task 18 is the
// first caller to put this on the wire, via Driver.Status ->
// internal/agent.collectOnce -> sample.RuntimeSample.LastError).
type LastError struct {
	Message    string    `json:"message"`
	At         time.Time `json:"at"`
	ExitCode   int       `json:"exit_code"`
	Failures   int       `json:"failures"`
	StderrTail string    `json:"stderr_tail,omitempty"` // bounded, ~2 KiB
	// Code is a stable machine-readable error code (for example
	// runtime.device_backend_mismatch) for lifecycle failures the gateway maps
	// to a specific operator message; empty for a plain start failure or crash.
	Code string `json:"code,omitempty"`
}

// Status is one INSTANCE's current runtime status, as tracked by the process
// manager and reported upward: one row per process the manager keeps (#340).
// A row is identified by the pair (SpecID, Instance), never by SpecID alone --
// SpecID is the BASE spec's ID on every instance's row, so a replicable spec
// running several instances reports several rows with one SpecID, told apart
// by Instance (and Model). A spec that never replicates has exactly one row,
// Instance 0, exactly as before instances existed. JSON tags mirror
// sample.RuntimeSample field-for-field, EXCEPT MeasuredVRAM and the
// Type/MetricsPath/
// ContextProbePath trio below: the sample maps MeasuredVRAM to
// RuntimeSample.GPUs ([]RuntimeGPUSample), so "measured_vram" here is not
// itself part of that wire contract -- the tag exists so Status marshals
// sensibly wherever it IS put on the wire directly (a status
// stderr/debugging dump, a future endpoint), consistent with the rest of
// this type.
//
// Type, MetricsPath, and ContextProbePath are likewise NOT part of the
// Status<->RuntimeSample mirror: they are the agent's per-child PROBE
// CONFIG (the resolved effective values copied straight from spec.Type/
// spec.MetricsPath/spec.ContextProbePath by snapshotStatus, a pure struct
// copy with no I/O), not lifecycle state. internal/agent's collectOnce
// reads them to know WHERE to probe a running child for metrics/context; it
// does not put them on RuntimeSample itself -- only the PROBE RESULTS
// (ContextSize/ActiveRequests/QueueDepth) land there. This is a second,
// documented exception to the "mirrors sample.RuntimeSample field-for-field"
// contract, alongside MeasuredVRAM above. MeasuredRAMMB, by contrast, IS part of
// the mirror: it is a per-row scalar -- one row's process -- that rides
// RuntimeSample.RAMMeasuredMB under the same JSON key (internal/agent copies it
// across).
type Status struct {
	// SpecID is the BASE spec's ID, the same on every instance row of a spec
	// (#340); a row is keyed by (SpecID, Instance).
	SpecID string `json:"spec_id"`
	// Model is the row's INSTANCE ADDRESS (InstanceAddress): the spec's upstream
	// model name for instance 0, "m#k" for instance k >= 1 -- the name that
	// reaches that very process through the router.
	Model        string      `json:"model"`
	State        State       `json:"state"`
	Since        time.Time   `json:"since"`
	PID          int         `json:"pid,omitempty"`
	Port         int         `json:"port,omitempty"`
	InFlight     int         `json:"in_flight"`
	Restarts     int         `json:"restarts"`
	MeasuredVRAM map[int]int `json:"measured_vram"` // gpu index -> MB, when measured
	// MeasuredRAMMB is the row's current process's resident host RAM in MB, as
	// the recurring RAM measurement last observed it; 0 means unknown (never
	// measured, no RAM measurer, or the process has exited) and is never "needs
	// no RAM". A per-row scalar -- RAM has no per-GPU axis, and one process
	// backs one row (one instance, #340) -- carried to the wire as
	// RuntimeSample.RAMMeasuredMB under the same JSON key.
	MeasuredRAMMB int        `json:"ram_measured_mb,omitempty"`
	LastError     *LastError `json:"last_error,omitempty"`
	// Instance is this row's instance index within its spec (#340): 0 for the
	// spec's one ordinary process, k >= 1 for a further (burst) instance. It is
	// always on the wire, 0 included.
	Instance int `json:"instance"`
	// StartupMS is how long this process took to become healthy, in
	// milliseconds, measured by the agent on its last successful start; 0 means
	// unknown (never started, or a start that did not finish). It is the
	// gateway's cold-start figure for the instance.
	StartupMS int `json:"startup_ms,omitempty"`

	// RPCProtocol is the RPC protocol version a running rpc_server member
	// announced in its startup banner; empty otherwise. It is not part of the
	// status wire form itself: telemetry reports it under the member's rpc block.
	RPCProtocol string `json:"-"`
	// RPCMember is a copy of an rpc_server member's current settings, the
	// allowlist included as last applied: the agent builds the member's relay
	// from it. Not part of the status wire form.
	RPCMember *RPCMember `json:"-"`
	// RPCReleasedAt is when the gateway last released this rpc_server member
	// (Manager.ReleaseMember), zero when it never did. The direct-mode
	// observer reads it to tell the head connections a release ended from a
	// head's new one. Not part of the status wire form.
	RPCReleasedAt time.Time `json:"-"`

	// Type, MetricsPath, and ContextProbePath are the agent's per-child
	// probe config, resolved effective values copied from the spec by
	// snapshotStatus -- see the type doc above. Not lifecycle state, and
	// not part of the Status<->RuntimeSample wire mirror.
	Type             string `json:"type"`
	MetricsPath      string `json:"metrics_path"`
	ContextProbePath string `json:"context_probe_path"`

	// Kind mirrors the spec's Kind (omitted for a model). RPC is an rpc_server
	// member's live state, Cluster a head's launch facts, and AdmissionBlockedBy
	// names, on a waiting spec, the held member that blocks its admission. All
	// four ride RuntimeSample under the same JSON keys.
	Kind               string         `json:"kind,omitempty"`
	RPC                *RPCStatus     `json:"rpc,omitempty"`
	Cluster            *ClusterStatus `json:"cluster,omitempty"`
	AdmissionBlockedBy string         `json:"admission_blocked_by,omitempty"`
}

// RPCStatus is an rpc_server member's live state: which head holds it and since
// when, the end of an armed reservation, the rpc-server protocol version, the
// device count it announced, whether it listens on an address other than the
// configured one, peers that connected without being allowlisted, the on-disk
// tensor cache size, and whether the member has stopped making progress.
type RPCStatus struct {
	HeldBy        string      `json:"held_by,omitempty"`
	HeldSince     time.Time   `json:"held_since,omitzero"`
	ReservedUntil time.Time   `json:"reserved_until,omitzero"`
	Protocol      string      `json:"protocol,omitempty"`
	DeviceCount   int         `json:"device_count"`
	BindMismatch  bool        `json:"bind_mismatch"`
	Breaches      []RPCBreach `json:"breaches,omitempty"`
	CacheMB       int         `json:"cache_mb"`
	Stalled       bool        `json:"stalled"`
}

// RPCBreach records one connection from a peer outside the member's allowlist.
type RPCBreach struct {
	Peer string    `json:"peer"`
	At   time.Time `json:"at"`
}

// ClusterStatus is what a head's last launch used: the endpoint addresses it
// was started against.
type ClusterStatus struct {
	Launched []string `json:"launched"`
}

// statusAlias is Status under a different name, used only to marshal
// through Status's own MarshalJSON below without recursing into it.
type statusAlias Status

// MarshalJSON normalizes a nil MeasuredVRAM to an empty JSON object ("{}")
// instead of Go's default "null" for a nil map. A nil-versus-null defect on
// a field the gateway parses is an easy mistake to repeat across this
// package's several JSON-facing types; Status.MeasuredVRAM is nil on every spec
// that has never been measured (no measurer installed, or not yet running),
// which is the common case, so this is not a corner this type can leave
// unguarded.
func (s Status) MarshalJSON() ([]byte, error) {
	a := statusAlias(s)
	if a.MeasuredVRAM == nil {
		a.MeasuredVRAM = map[int]int{}
	}
	return json.Marshal(a)
}
