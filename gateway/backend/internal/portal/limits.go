// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"log/slog"
	"op-ai-gateway/internal/routing"
	"time"
)

var (
	// ErrLimitValidation covers every rate/quota/budget config shape problem:
	// a negative value, an unrecognized period, or a threshold set without its
	// period (or vice versa) — design spec §7.3. Shared by both the Service
	// (§7.1, via UpdateService/CreateService) and User (§7.2) limit paths.
	ErrLimitValidation = errors.New("limit.validation_failed")
	// ErrLimitUserNotFound is UserLimits/SetUserLimits' "no such user"
	// sentinel. The caller has already passed the admin-scope gate at the
	// HTTP layer (there is no self-service path at all — §7.2), so there is
	// no principal-visibility concern here; this is a plain 404, not a
	// no-existence-leak mapping like ErrServiceNotFound.
	ErrLimitUserNotFound = errors.New("limit.user_not_found")
)

// LimitConfigDTO is a principal's (Service or User) optional rate/quota/
// budget configuration — design spec §7.3. Every field's zero value means
// that specific limit is off, mirroring routing.LimitConfig exactly (this DTO
// is a 1:1 wire copy of it — see limitConfigDTO/validateLimitConfig).
type LimitConfigDTO struct {
	RateRequests       int     `json:"rate_requests"`
	RateWindowSeconds  int     `json:"rate_window_seconds"`
	RequestQuota       int     `json:"request_quota"`
	RequestQuotaPeriod string  `json:"request_quota_period"`
	TokenQuota         int64   `json:"token_quota"`
	TokenQuotaPeriod   string  `json:"token_quota_period"`
	CostBudget         float64 `json:"cost_budget"`
	CostBudgetPeriod   string  `json:"cost_budget_period"`
}

// LimitUsageDTO is a principal's CURRENT calendar-period usage against each
// of its configured limits — read-only, computed from
// routing.Store.UsageAggregateSince at the period the corresponding limit is
// aligned to (design spec §7.3). A field for a limit that has no period
// configured is always 0 (nothing to compare against, and no store read is
// made for it — see limitUsage).
type LimitUsageDTO struct {
	RequestsThisPeriod int64   `json:"requests_this_period"`
	TokensThisPeriod   int64   `json:"tokens_this_period"`
	CostThisPeriod     float64 `json:"cost_this_period"`
}

// limitConfigDTO maps a routing.LimitConfig onto its wire shape (the read
// direction — see validateLimitConfig for the write/validate direction).
func limitConfigDTO(cfg routing.LimitConfig) LimitConfigDTO {
	return LimitConfigDTO{
		RateRequests:       cfg.RateRequests,
		RateWindowSeconds:  cfg.RateWindowSeconds,
		RequestQuota:       cfg.RequestQuota,
		RequestQuotaPeriod: cfg.RequestQuotaPeriod,
		TokenQuota:         cfg.TokenQuota,
		TokenQuotaPeriod:   cfg.TokenQuotaPeriod,
		CostBudget:         cfg.CostBudget,
		CostBudgetPeriod:   cfg.CostBudgetPeriod,
	}
}

// validateLimitConfig validates dto against design spec §7.3's rules and
// returns the equivalent routing.LimitConfig, ready to persist via
// SetPrincipalLimits:
//
//   - every numeric field must be >= 0 (a negative "limit" is meaningless);
//   - every *_period field must be on the routing.ValidLimitPeriod whitelist
//     (empty string included — "this limit is off");
//   - each threshold/period PAIR is all-or-nothing: rate_window_seconds > 0
//     iff rate_requests > 0, and *_period != "" iff its paired threshold > 0
//     (a period with no threshold, or a threshold with no period, is an
//     inconsistent half-configured limit and is rejected).
//
// A fully-zero dto (every limit off) passes validation — it is the "clear all
// limits" input; the caller always persists the result (see
// SetPrincipalLimits's own doc: a zero-value LimitConfig is already a
// documented full no-op, so storing it is behaviorally identical to having no
// row at all — Task 4's zero-config-clears decision).
func validateLimitConfig(dto LimitConfigDTO) (routing.LimitConfig, error) {
	if dto.RateRequests < 0 || dto.RateWindowSeconds < 0 ||
		dto.RequestQuota < 0 || dto.TokenQuota < 0 || dto.CostBudget < 0 {
		return routing.LimitConfig{}, ErrLimitValidation
	}
	if (dto.RateRequests > 0) != (dto.RateWindowSeconds > 0) {
		return routing.LimitConfig{}, ErrLimitValidation
	}
	if !routing.ValidLimitPeriod(dto.RequestQuotaPeriod) || (dto.RequestQuota > 0) != (dto.RequestQuotaPeriod != "") {
		return routing.LimitConfig{}, ErrLimitValidation
	}
	if !routing.ValidLimitPeriod(dto.TokenQuotaPeriod) || (dto.TokenQuota > 0) != (dto.TokenQuotaPeriod != "") {
		return routing.LimitConfig{}, ErrLimitValidation
	}
	if !routing.ValidLimitPeriod(dto.CostBudgetPeriod) || (dto.CostBudget > 0) != (dto.CostBudgetPeriod != "") {
		return routing.LimitConfig{}, ErrLimitValidation
	}
	return routing.LimitConfig{
		RateRequests:       dto.RateRequests,
		RateWindowSeconds:  dto.RateWindowSeconds,
		RequestQuota:       dto.RequestQuota,
		RequestQuotaPeriod: dto.RequestQuotaPeriod,
		TokenQuota:         dto.TokenQuota,
		TokenQuotaPeriod:   dto.TokenQuotaPeriod,
		CostBudget:         dto.CostBudget,
		CostBudgetPeriod:   dto.CostBudgetPeriod,
	}, nil
}

// principalLimits reads a principal's stored rate/quota/budget config,
// defaulting to the zero LimitConfig (no limits) when no row exists — this is
// exactly the convention documented on routing.Store.PrincipalLimits's ok
// return, made explicit here so every caller (ServiceDTO's serviceDTO,
// UserLimits) gets it for free rather than re-deriving it.
func principalLimits(ctx context.Context, routes routing.Store, principalType, principalID string) (routing.LimitConfig, error) {
	cfg, ok, err := routes.PrincipalLimits(ctx, principalType, principalID)
	if err != nil {
		return routing.LimitConfig{}, err
	}
	if !ok {
		return routing.LimitConfig{}, nil
	}
	return cfg, nil
}

// limitUsage computes a principal's LimitUsageDTO for cfg's CONFIGURED
// periods only (design spec §7.3) — a limit whose period is unset costs zero
// store reads, and the request-/token-quota commonly SHARE a period (e.g.
// both "day"), in which case they share one UsageAggregateSince read rather
// than two.
//
// A store-read error is Debug-logged and fails OPEN for that field (its usage
// reads 0) — mirrors the admission path's fail-open contract (design spec
// §11): a transient read glitch must never itself block reading a service's
// or user's settings.
func limitUsage(ctx context.Context, routes routing.Store, principalType, principalID string, cfg routing.LimitConfig, now time.Time) LimitUsageDTO {
	type aggregate struct {
		requests int64
		tokens   int64
		cost     float64
	}
	cache := make(map[string]aggregate, 3)
	read := func(period string) aggregate {
		if v, ok := cache[period]; ok {
			return v
		}
		reqs, toks, cost, err := routes.UsageAggregateSince(ctx, principalType, principalID, routing.PeriodStart(period, now))
		if err != nil {
			slog.Debug("limit usage: aggregate read failed, showing zero",
				"principal_type", principalType, "principal_id", principalID, "period", period, "err", err)
			reqs, toks, cost = 0, 0, 0
		}
		v := aggregate{requests: reqs, tokens: toks, cost: cost}
		cache[period] = v
		return v
	}
	var out LimitUsageDTO
	if cfg.RequestQuotaPeriod != "" {
		out.RequestsThisPeriod = read(cfg.RequestQuotaPeriod).requests
	}
	if cfg.TokenQuotaPeriod != "" {
		out.TokensThisPeriod = read(cfg.TokenQuotaPeriod).tokens
	}
	if cfg.CostBudgetPeriod != "" {
		out.CostThisPeriod = read(cfg.CostBudgetPeriod).cost
	}
	return out
}
