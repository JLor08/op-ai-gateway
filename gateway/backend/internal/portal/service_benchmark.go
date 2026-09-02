// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"strings"
	"time"
)

var (
	// ErrBenchmarkNoModels is returned when a benchmark scope authorizes cleanly
	// but resolves to zero mappings to measure.
	ErrBenchmarkNoModels = errors.New("benchmark.no_models")
	// ErrBenchmarkScopeInvalid is returned for an unknown benchmark scope selector.
	ErrBenchmarkScopeInvalid = errors.New("benchmark.scope_invalid")
)

// BenchmarkTargetView is one authorized (server, app, mapping) to benchmark.
type BenchmarkTargetView struct {
	Server  routing.AIServer
	App     routing.Application
	Mapping routing.ModelMapping
}

// BenchmarkRunDTO is one historical benchmark-run row for a mapping.
type BenchmarkRunDTO struct {
	ID                    string             `json:"id"`
	MappingID             string             `json:"mapping_id"`
	ServerID              string             `json:"server_id"`
	CreatedAt             time.Time          `json:"created_at"`
	GenTokensPerSecond    float64            `json:"gen_tokens_per_second"`
	PromptTokensPerSecond float64            `json:"prompt_tokens_per_second"`
	LoadTimeMS            int                `json:"load_time_ms"`
	ContextSize           int                `json:"context_size"`
	Error                 string             `json:"error,omitempty"`
	Kind                  string             `json:"kind"`
	Capacity              *CapacityReportDTO `json:"capacity,omitempty"`
	// VisionCapable is set only for a kind=="vision" row, carrying that run's
	// verdict (false for both a definitive "not capable" AND an inconclusive
	// probe — the caller distinguishes the two via Error, which is empty only on
	// a definitive verdict). Nil for any other kind.
	VisionCapable *bool `json:"vision_capable,omitempty"`
	// VRAM is the decoded per-GPU VRAM result, set only for a kind=="vram" row
	// whose payload parsed (a malformed one is tolerated as nil, exactly like
	// Capacity above). The row is evidence an operator reads, never authority:
	// nothing applies it to a launch spec's vram_estimate_mb or
	// vram_measured_mb.
	VRAM *VRAMReportDTO `json:"vram,omitempty"`
}

// CapacityLevelDTO / CapacityReportDTO are the decoded capacity curve for a
// kind=="capacity" benchmark-history row (decoded from BenchmarkRun.CapacityCurve).
type CapacityLevelDTO struct {
	Concurrency               int     `json:"concurrency"`
	AggregateTokensPerSecond  float64 `json:"aggregate_tokens_per_second"`
	PerRequestTokensPerSecond float64 `json:"per_request_tokens_per_second"`
	MeanLatencyMS             int64   `json:"mean_latency_ms"`
	Successes                 int     `json:"successes"`
	Errors                    int     `json:"errors"`
	VRAMFreePct               float64 `json:"vram_free_pct,omitempty"`
	RAMFreePct                float64 `json:"ram_free_pct,omitempty"`
	RequestsDeferred          int     `json:"requests_deferred,omitempty"`
	RequestsProcessing        int     `json:"requests_processing,omitempty"`
	TotalSlots                int     `json:"total_slots,omitempty"`
	StopReason                string  `json:"stop_reason,omitempty"`
}

type CapacityReportDTO struct {
	MaxConcurrency               int                `json:"max_concurrency"`
	RecommendedConcurrency       int                `json:"recommended_concurrency"`
	GenTokensPerSecondAtCapacity float64            `json:"gen_tokens_per_second_at_capacity"`
	MemoryObserved               bool               `json:"memory_observed"`
	Levels                       []CapacityLevelDTO `json:"levels,omitempty"`
}

// VRAMGPUItemDTO / VRAMReportDTO are the decoded per-GPU VRAM result for a
// kind=="vram" benchmark-history row (decoded from BenchmarkRun.VRAMJSON).
// They mirror the gateway's VRAMReport/VRAMGPUItem field-for-field, the way
// CapacityReportDTO mirrors routing.CapacityReport: the store keeps the payload
// opaque, so the two ends agree by shape, and TestVRAMReportDecodesIntoThePortalDTO
// (internal/gateway) fails if one side gains a field the other lacks.
//
// Inconclusive empty = a definitive result. Isolated is the run's own
// evidence-backed claim, auditable through IsolationEvidence (spec id -> why
// this run believes that spec was not running); a spec missing from that map
// was NOT confirmed. MeasuredMB and DeltaMB are different quantities -- an
// attributed per-process measurement vs the model's marginal cost on the card
// -- and must never be averaged; 0 means unknown for both.
type VRAMGPUItemDTO struct {
	Index           int    `json:"index"`
	Fingerprint     string `json:"fingerprint,omitempty"`
	FingerprintKind string `json:"fingerprint_kind,omitempty"`
	UnifiedMemory   bool   `json:"unified_memory,omitempty"`
	BaselineUsedMB  int    `json:"baseline_used_mb"`
	DeltaMB         int    `json:"delta_mb,omitempty"`
	MeasuredMB      int    `json:"measured_mb,omitempty"`
	Attributable    bool   `json:"attributable"`
}

type VRAMReportDTO struct {
	Isolated          bool              `json:"isolated"`
	IsolationEvidence map[string]string `json:"isolation_evidence,omitempty"`
	DrainedSpecIDs    []string          `json:"drained_spec_ids,omitempty"`
	// RestoreFailed is the specs whose override the run could not clear, so
	// they ARE still force_stopped and need clearing by hand. RestoreTakenOver
	// is the disjoint other case: the spec's admin_state was no longer the
	// run's force_stopped when the restore re-read it, because an operator
	// force-started it or cleared the override mid-run -- so the restore wrote
	// nothing and there is nothing to clear. Two fields because they are two
	// instructions, and giving the second one the first one's message tells an
	// operator to stop a model they just started.
	RestoreFailed    []string `json:"restore_failed,omitempty"`
	RestoreTakenOver []string `json:"restore_taken_over,omitempty"`
	Inconclusive     string   `json:"inconclusive,omitempty"`
	// Warnings are conditions that degraded the run's confidence without
	// invalidating its number -- a non-managed neighbouring application on
	// the same server, or an agent with no open WebSocket. The run warns
	// rather than refusing on both.
	Warnings []string `json:"warnings,omitempty"`
	// GPUs is never nil on a decoded report (see MappingBenchmarks): a nil
	// slice marshals as JSON null instead of [], and a client reading it with a
	// `?? []` fallback then renders an eternally empty list with no error.
	GPUs []VRAMGPUItemDTO `json:"gpus"`
}

// MappingBenchmarks returns the benchmark-run history for a mapping, newest-first.
// Access is admin-or-owner via authorizeMapping (any failure collapses to
// ErrMappingNotFound — no existence leak). A non-positive limit defaults in the
// store to 50.
func (s *Service) MappingBenchmarks(ctx context.Context, principal auth.Token, mappingID string, limit int) ([]BenchmarkRunDTO, error) {
	if _, _, _, err := s.authorizeMapping(ctx, principal, mappingID); err != nil {
		return nil, err
	}
	runs, err := s.routes.BenchmarkRunsByMapping(ctx, mappingID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]BenchmarkRunDTO, 0, len(runs))
	for _, r := range runs {
		kind := r.Kind
		if kind == "" {
			kind = "speed"
		}
		dto := BenchmarkRunDTO{
			ID:                    r.ID,
			MappingID:             r.MappingID,
			ServerID:              r.ServerID,
			CreatedAt:             r.CreatedAt,
			GenTokensPerSecond:    r.GenTokensPerSecond,
			PromptTokensPerSecond: r.PromptTokensPerSecond,
			LoadTimeMS:            r.LoadTimeMS,
			ContextSize:           r.ContextSize,
			Error:                 r.Error,
			Kind:                  kind,
		}
		if kind == "capacity" && strings.TrimSpace(r.CapacityCurve) != "" {
			var report CapacityReportDTO
			if err := json.Unmarshal([]byte(r.CapacityCurve), &report); err == nil {
				dto.Capacity = &report
			}
		}
		if kind == "vision" {
			v := r.VisionCapable
			dto.VisionCapable = &v
		}
		// The payload column is read for its OWN kind only, like the capacity
		// curve above: a row of another kind carrying bytes here (a legacy row,
		// or a future writer) must not be served as a VRAM result.
		if kind == "vram" && strings.TrimSpace(r.VRAMJSON) != "" {
			var report VRAMReportDTO
			if err := json.Unmarshal([]byte(r.VRAMJSON), &report); err == nil {
				if report.GPUs == nil {
					report.GPUs = []VRAMGPUItemDTO{}
				}
				dto.VRAM = &report
			}
		}
		out = append(out, dto)
	}
	return out, nil
}

// AuthorizeBenchmarkScope authorizes principal for {scope,id} and returns the
// target server + the (app, mapping) pairs to benchmark — ALL on ONE server.
// scope is "mapping" | "application" | "server". Any authorize failure collapses
// to the matching not-found sentinel (no existence leak), mirroring
// authorizeApplication. Returns ErrBenchmarkNoModels when the scope resolves to
// zero mappings to measure, and ErrBenchmarkScopeInvalid for an unknown scope.
//
// A "mapping" scope benchmarks that single mapping regardless of its status; the
// "application" and "server" scopes filter to ACTIVE mappings (and, for server,
// ACTIVE applications) so a disabled model is not measured wholesale.
func (s *Service) AuthorizeBenchmarkScope(ctx context.Context, principal auth.Token, scope, id string) (routing.AIServer, []BenchmarkTargetView, error) {
	switch scope {
	case "mapping":
		mapping, app, server, err := s.authorizeMapping(ctx, principal, id)
		if err != nil {
			return routing.AIServer{}, nil, err
		}
		return server, []BenchmarkTargetView{{Server: server, App: app, Mapping: mapping}}, nil
	case "application":
		app, server, err := s.authorizeApplication(ctx, principal, id)
		if err != nil {
			return routing.AIServer{}, nil, err
		}
		views, err := s.benchmarkViewsForApp(ctx, server, app)
		if err != nil {
			return routing.AIServer{}, nil, err
		}
		if len(views) == 0 {
			return routing.AIServer{}, nil, ErrBenchmarkNoModels
		}
		return server, views, nil
	case "server":
		server, err := s.authorizeServer(ctx, principal, id)
		if err != nil {
			return routing.AIServer{}, nil, err
		}
		apps, err := s.routes.ApplicationsByServer(ctx, server.ID)
		if err != nil {
			return routing.AIServer{}, nil, err
		}
		var views []BenchmarkTargetView
		for _, app := range apps {
			if app.Status != routing.ServerStatusActive {
				continue
			}
			appViews, err := s.benchmarkViewsForApp(ctx, server, app)
			if err != nil {
				return routing.AIServer{}, nil, err
			}
			views = append(views, appViews...)
		}
		if len(views) == 0 {
			return routing.AIServer{}, nil, ErrBenchmarkNoModels
		}
		return server, views, nil
	default:
		return routing.AIServer{}, nil, ErrBenchmarkScopeInvalid
	}
}

// benchmarkViewsForApp loads app's ACTIVE mappings and wraps each as a
// BenchmarkTargetView on server.
func (s *Service) benchmarkViewsForApp(ctx context.Context, server routing.AIServer, app routing.Application) ([]BenchmarkTargetView, error) {
	mappings, err := s.routes.MappingsByApplication(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	views := make([]BenchmarkTargetView, 0, len(mappings))
	for _, m := range mappings {
		if m.Status != routing.ServerStatusActive {
			continue
		}
		views = append(views, BenchmarkTargetView{Server: server, App: app, Mapping: m})
	}
	return views, nil
}
