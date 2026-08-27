// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package runtime is the agent-side vocabulary for the agent-managed model
// runtime: the wire mirror of the gateway's runtime-config document
// (design doc docs/superpowers/specs/2026-08-25-agent-runtime-manager-design.md
// §11; captured field-for-field in
// .superpowers/sdd/2026-08-25-agent-runtime-manager/task-7-report.md), the
// per-spec visible-load-lifecycle state machine (design doc §7), and the
// admission policy (design doc §5) as pure functions over snapshots -- no
// clocks, no I/O, no goroutines. Task 14 owns starting processes, opening
// sockets, and reading files; this package only decides.
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
	"time"
)

// SpecGPU is one GPU a Spec's process is launched to use.
type SpecGPU struct {
	Index  int `json:"index"`
	VRAMMB int `json:"vram_mb"` // 0 = unknown demand, never a real zero-cost claim
}

// Spec is one ENABLED launch spec inside the runtime-config document --
// everything the agent needs to exec and supervise a model process. Mirrors
// the gateway's AgentRuntimeSpecDTO
// (gateway/backend/internal/portal/service_runtime.go) field-for-field.
type Spec struct {
	ID                          string            `json:"id"`
	Model                       string            `json:"model"`
	UpstreamModel               string            `json:"upstream_model"`
	Binary                      string            `json:"binary"`
	Args                        []string          `json:"args"`
	Env                         map[string]string `json:"env"`
	WorkDir                     string            `json:"work_dir"`
	GPUs                        []SpecGPU         `json:"gpus"`
	ListenPort                  int               `json:"listen_port"`
	HealthPath                  string            `json:"health_path"`
	HealthTimeoutSeconds        int               `json:"health_timeout_seconds"`
	StartupTimeoutSeconds       int               `json:"startup_timeout_seconds"`
	IdleTimeoutSeconds          int               `json:"idle_timeout_seconds"`
	AdmissionWaitTimeoutSeconds int               `json:"admission_wait_timeout_seconds"`
	Pinned                      bool              `json:"pinned"`
	AdminState                  string            `json:"admin_state"` // "" | "force_running" | "force_stopped"
}

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
// ADDING A CODE IS A THREE-SIDED CHANGE. A new code must be added here, to
// the gateway's allow-list, and to the portal's i18n label map (German and
// English together) -- a code the gateway does not know degrades to the
// generic message, which is safe but silently drops the diagnosis this
// mechanism exists to deliver. TestParseConfigErrorsAreAllClassified pins
// that every error ParseConfig can actually produce carries one of these.
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
	// ParseErrorUnclassified is the defensive floor for a load failure that
	// carries no *ParseError at all. Nothing produces it today (every
	// ParseConfig return path is classified, and a test pins that), and it
	// is deliberately NOT in the gateway's allow-list: if it ever appears
	// it degrades to the gateway's generic message, which is the honest
	// rendering of "the agent could not say why".
	ParseErrorUnclassified ParseErrorCode = "unclassified"
)

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
//   - a coresident pair naming a spec ID that is not (or no longer) present
//     in Specs is dropped silently, and the rest of the document -- other
//     specs, other pairs -- remains usable. This is reachable from an
//     ordinary stale/racy read on the gateway side, not a defect worth
//     failing the whole config over.
//
// Every collection-shaped field (Specs, GPUBudgets, Coresident, and each
// Spec's Args/Env/GPUs) is normalized to non-nil, so a caller that ranges
// over it or re-marshals it (file mode) never has to nil-check first.
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
	}

	return cfg, nil
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

// State is a Spec's visible load-lifecycle stage (design doc §7).
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
// (design doc §7): a spec can be StateStopped and still show "last load
// attempt failed, yesterday 14:32, exit code 1".
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
}

// Status is one spec's current runtime status, as tracked by the process
// manager and reported upward. JSON tags mirror sample.RuntimeSample
// field-for-field, EXCEPT MeasuredVRAM: the sample maps it to
// RuntimeSample.GPUs ([]RuntimeGPUSample), so "measured_vram" here is not
// itself part of that wire contract -- the tag exists so Status marshals
// sensibly wherever it IS put on the wire directly (a status
// stderr/debugging dump, a future endpoint), consistent with the rest of
// this type.
type Status struct {
	SpecID       string      `json:"spec_id"`
	Model        string      `json:"model"` // upstream model name
	State        State       `json:"state"`
	Since        time.Time   `json:"since"`
	PID          int         `json:"pid,omitempty"`
	Port         int         `json:"port,omitempty"`
	InFlight     int         `json:"in_flight"`
	Restarts     int         `json:"restarts"`
	MeasuredVRAM map[int]int `json:"measured_vram"` // gpu index -> MB, when measured
	LastError    *LastError  `json:"last_error,omitempty"`
}

// statusAlias is Status under a different name, used only to marshal
// through Status's own MarshalJSON below without recursing into it.
type statusAlias Status

// MarshalJSON normalizes a nil MeasuredVRAM to an empty JSON object ("{}")
// instead of Go's default "null" for a nil map. A nil-versus-null defect on
// a field the gateway parses has already been caught six times elsewhere on
// this branch (task-18-brief.md); Status.MeasuredVRAM is nil on every spec
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
