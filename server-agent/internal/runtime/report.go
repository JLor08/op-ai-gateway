// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// This file builds the agent's upward, file-mode report (design doc
// docs/superpowers/specs/2026-08-25-agent-runtime-manager-design.md §10.2;
// the shipped description of the same contract is
// docs/architecture/cross-cutting/agent-runtime-manager.md §8.3) -- what the
// file-mode driver sends to the gateway over POST
// /api/agent/v1/runtime-report and the WS "runtime_report" frame, both via
// the sender interfaces in internal/client.
//
// REDACTION HAPPENS HERE, AND IT IS THE POINT OF THIS FILE. In file mode the
// agent reads a local config the SERVER OPERATOR owns, which may legitimately
// contain plaintext secrets -- an HF_TOKEN written directly rather than as an
// ${AGENT_ENV:NAME} placeholder the way the gateway-sourced document always
// does. BuildReport replaces every env VALUE with a fixed mask before
// marshaling; keys survive so an operator (or the portal report view) can
// still see WHICH variables a spec sets. The gateway re-masks on ingest as
// defense in depth (sanitizeRuntimeReportConfig) -- that does not excuse this
// file: this redaction is what makes the ENV half of the wire clean in the
// first place.
//
// SCOPE, stated exactly, because the obvious stronger reading is wrong: the
// contract covers env VALUES. It does NOT cover `args`, which are reported
// verbatim even though ${AGENT_ENV:NAME} placeholders are expanded in them
// too -- so a secret placed in an argument reaches the gateway in plaintext.
// That was reviewed and upheld as spec-correct, and the operator guidance
// ("secrets go in env, never in args") is in server-agent/README.md and in
// the risk register. Nor is this the only upward channel: LastError's stderr
// tail carries the child's own output, and a model server that prints its
// command line at startup puts the resolved values there. "No env value ever
// reaches the wire in a report" is true; "no secret ever reaches the wire" is
// not, and a comment here claiming the latter would make the gap invisible
// to exactly the reader best placed to close it.
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
	ParseError  ParseErrorCode  `json:"parse_error,omitempty"`
	Config      json.RawMessage `json:"config"`
}

// BuildReport redacts every env value out of cfg, marshals the result as the
// report's config object, and returns the fully marshaled Report ready to
// hand to Client.PostRuntimeReport or WSSender.PostRuntimeReport verbatim.
// source and parseErr are round-tripped unchanged. parseErr is a
// ParseErrorCode from that type's CLOSED SET, not free text -- the type is
// what makes "free of embedded secrets" a compile-time property here rather
// than a promise the loader is trusted to keep (the C2 fix; the gateway
// still allow-lists the value on ingest as defense in depth). This
// function's own job remains the env map.
func BuildReport(cfg Config, source string, parseErr ParseErrorCode, at time.Time) ([]byte, error) {
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
// every VALUE replaced by envRedactedMask -- keys survive untouched -- and
// every spec's non-empty APIToken is likewise replaced by envRedactedMask (an
// empty token stays "" so absence remains distinguishable on the wire; I4). It
// never mutates cfg's own slices/maps (a fresh Specs slice and a fresh Env
// map per spec) nor cfg's own APIToken values (specs[i] is a struct copy and
// APIToken is a value field), and it re-applies ParseConfig's own nil->empty-collection
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
		// A hand-written file-mode spec (or a gateway-pushed token) can carry a
		// literal api_token; mask it exactly as an env VALUE so it never crosses
		// the report wire in clear. specs[i] is a struct copy (via copy above)
		// and APIToken is a value field, so this assignment does not touch the
		// caller's cfg. An empty token stays empty, keeping absence
		// distinguishable on the wire.
		if specs[i].APIToken != "" {
			specs[i].APIToken = envRedactedMask
		}
	}
	cfg.Specs = specs
	return cfg
}
