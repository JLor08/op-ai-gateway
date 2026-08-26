// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// This file builds the agent's upward, file-mode report (design doc
// docs/superpowers/specs/2026-08-25-agent-runtime-manager-design.md §10.2,
// wire contract captured field-for-field in
// .superpowers/sdd/2026-08-25-agent-runtime-manager/task-9-report.md's
// "Agent-side (Tasks 17/18) wire contract" section) -- what Task 18's file
// mode driver sends to the gateway over POST /api/agent/v1/runtime-report
// and the WS "runtime_report" frame, both via the sender interfaces added
// to internal/client in this same task.
//
// REDACTION HAPPENS HERE, AND IT IS THE POINT OF THIS FILE. In file mode the
// agent reads a local config the SERVER OPERATOR owns, which may legitimately
// contain plaintext secrets -- an HF_TOKEN written directly rather than as an
// ${AGENT_ENV:NAME} placeholder the way the gateway-sourced document always
// does. BuildReport replaces every env VALUE with a fixed mask before
// marshaling; keys survive so an operator (or the portal report view) can
// still see WHICH variables a spec sets, but no value ever reaches the wire.
// The gateway re-masks on ingest as defense in depth (task-9-report.md,
// sanitizeRuntimeReportConfig) -- that does not excuse this file: this
// redaction is what makes the wire clean in the first place.
package runtime

import (
	"encoding/json"
	"fmt"
	"time"
)

// envRedactedMask replaces every env value before a Config leaves this
// process as a report. Three U+2022 BULLET characters, matching the
// gateway's own defense-in-depth mask byte-for-byte (task-9-report.md) --
// not that the two need to agree (the agent's mask never reaches the
// gateway's re-masking path, since nothing plaintext survives this far) but
// so a human reading either side's output sees the identical, recognizable
// placeholder.
const envRedactedMask = "•••"

// Report is the agent's upward, file-mode status report: what local runtime
// config this agent is currently running from, and whether the last attempt
// to load it failed. Source is "file" when this server's runtime is
// configured from a local file, "gateway" otherwise (task-9-report.md: this
// exact string flips the gateway's RuntimeStatus.IsFileMode, which gates
// whether it keeps pushing runtime_config WS frames at this agent -- an
// agent that is not actually in file mode must never send "file" here).
// Config is always a JSON object on the wire, even when parseErr is
// non-empty and there is nothing meaningful to report (BuildReport then
// marshals whatever zero-value Config it was given, which still renders as
// a valid, if empty, object -- never a bare "null").
type Report struct {
	Source      string          `json:"source"`
	CollectedAt time.Time       `json:"collected_at"`
	ParseError  string          `json:"parse_error,omitempty"`
	Config      json.RawMessage `json:"config"`
}

// BuildReport redacts every env value out of cfg, marshals the result as the
// report's config object, and returns the fully marshaled Report ready to
// hand to Client.PostRuntimeReport or WSSender.PostRuntimeReport verbatim.
// source and parseErr are round-tripped unchanged (parseErr is expected to
// already be free of embedded secrets -- Task 18's local file loader is
// responsible for that, matching the gateway's own belt-and-suspenders
// redactRuntimeReportParseError on ingest; this function's job is the env
// map only).
func BuildReport(cfg Config, source string, parseErr string, at time.Time) ([]byte, error) {
	cfgRaw, err := json.Marshal(redactConfigEnv(cfg))
	if err != nil {
		return nil, fmt.Errorf("runtime: marshal report config: %w", err)
	}
	raw, err := json.Marshal(Report{
		Source:      source,
		CollectedAt: at,
		ParseError:  parseErr,
		Config:      cfgRaw,
	})
	if err != nil {
		return nil, fmt.Errorf("runtime: marshal report: %w", err)
	}
	return raw, nil
}

// redactConfigEnv returns a copy of cfg in which every spec's Env map has
// every VALUE replaced by envRedactedMask -- keys survive untouched. It
// never mutates cfg's own slices/maps (a fresh Specs slice and a fresh Env
// map per spec), and it re-applies ParseConfig's own nil->empty-collection
// normalization to every collection-shaped field: BuildReport can be called
// with a Config that never went through ParseConfig (e.g. a zero-value
// Config on a parse-error report), and the wire contract requires
// specs/gpu_budgets/coresident/args/gpus/env to always be non-nil arrays and
// objects, never JSON null.
func redactConfigEnv(cfg Config) Config {
	if cfg.GPUBudgets == nil {
		cfg.GPUBudgets = []GPUBudget{}
	}
	if cfg.Coresident == nil {
		cfg.Coresident = [][2]string{}
	}
	specs := make([]Spec, len(cfg.Specs))
	copy(specs, cfg.Specs)
	for i := range specs {
		if specs[i].Args == nil {
			specs[i].Args = []string{}
		}
		if specs[i].GPUs == nil {
			specs[i].GPUs = []SpecGPU{}
		}
		masked := make(map[string]string, len(specs[i].Env))
		for k := range specs[i].Env {
			masked[k] = envRedactedMask
		}
		specs[i].Env = masked
	}
	cfg.Specs = specs
	return cfg
}
