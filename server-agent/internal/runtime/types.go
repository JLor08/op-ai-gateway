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
	Coresident   [][2]string `json:"coresident"` // spec-ID pairs, already translated upstream
	ETag         string      `json:"etag"`
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
		return Config{}, fmt.Errorf("runtime: parse config: %w", err)
	}

	specIDs := make(map[string]bool, len(cfg.Specs))
	for _, spec := range cfg.Specs {
		if specIDs[spec.ID] {
			return Config{}, fmt.Errorf("runtime: duplicate spec id %q", spec.ID)
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
type LastError struct {
	Message    string
	At         time.Time
	ExitCode   int
	Failures   int
	StderrTail string // bounded, ~2 KiB
}

// Status is one spec's current runtime status, as tracked by the process
// manager (a later task) and reported upward.
type Status struct {
	SpecID       string
	Model        string // upstream model name
	State        State
	Since        time.Time
	PID          int
	Port         int
	InFlight     int
	Restarts     int
	MeasuredVRAM map[int]int // gpu index -> MB, when measured
	LastError    *LastError
}
