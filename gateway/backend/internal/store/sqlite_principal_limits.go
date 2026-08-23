// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"op-ai-gateway/internal/routing"
	"time"
)

// PrincipalLimits reads a principal's rate/quota/budget config (migration v41,
// principal_limits; Phase 2 of the service-accounts work). ok is false when no
// row exists for (principalType, principalID) — the caller should then treat
// the principal as having no limits (a zero routing.LimitConfig), not an error.
func (s *SQLStore) PrincipalLimits(ctx context.Context, principalType, principalID string) (routing.LimitConfig, bool, error) {
	row := s.queryRow(ctx, `
		select
			rate_limit_requests, rate_limit_window_seconds,
			request_quota_requests, request_quota_period,
			token_quota_tokens, token_quota_period,
			cost_budget_amount, cost_budget_period
		from principal_limits
		where principal_type = ? and principal_id = ?`,
		principalType, principalID)

	var cfg routing.LimitConfig
	err := row.Scan(
		&cfg.RateRequests, &cfg.RateWindowSeconds,
		&cfg.RequestQuota, &cfg.RequestQuotaPeriod,
		&cfg.TokenQuota, &cfg.TokenQuotaPeriod,
		&cfg.CostBudget, &cfg.CostBudgetPeriod,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return routing.LimitConfig{}, false, nil
		}
		return routing.LimitConfig{}, false, fmt.Errorf("principal limits: %w", err)
	}
	return cfg, true, nil
}

// SetPrincipalLimits upserts a principal's limit config (the composite primary
// key principal_type+principal_id determines insert vs. update — mirrors the
// SetUIPreference/SetSystemSetting on-conflict-upsert pattern).
func (s *SQLStore) SetPrincipalLimits(ctx context.Context, principalType, principalID string, cfg routing.LimitConfig) error {
	_, err := s.exec(ctx, `
		insert into principal_limits (
			principal_type, principal_id,
			rate_limit_requests, rate_limit_window_seconds,
			request_quota_requests, request_quota_period,
			token_quota_tokens, token_quota_period,
			cost_budget_amount, cost_budget_period,
			updated_at
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		on conflict(principal_type, principal_id) do update set
			rate_limit_requests = excluded.rate_limit_requests,
			rate_limit_window_seconds = excluded.rate_limit_window_seconds,
			request_quota_requests = excluded.request_quota_requests,
			request_quota_period = excluded.request_quota_period,
			token_quota_tokens = excluded.token_quota_tokens,
			token_quota_period = excluded.token_quota_period,
			cost_budget_amount = excluded.cost_budget_amount,
			cost_budget_period = excluded.cost_budget_period,
			updated_at = excluded.updated_at`,
		principalType, principalID,
		cfg.RateRequests, cfg.RateWindowSeconds,
		cfg.RequestQuota, cfg.RequestQuotaPeriod,
		cfg.TokenQuota, cfg.TokenQuotaPeriod,
		cfg.CostBudget, cfg.CostBudgetPeriod,
		time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("set principal limits: %w", err)
	}
	return nil
}

// DeletePrincipalLimits removes a principal's limit config, if any. A missing
// row is a benign no-op (a 0-row DELETE), mirroring the store's other
// idempotent-on-retry delete methods.
func (s *SQLStore) DeletePrincipalLimits(ctx context.Context, principalType, principalID string) error {
	_, err := s.exec(ctx, `delete from principal_limits where principal_type = ? and principal_id = ?`,
		principalType, principalID)
	if err != nil {
		return fmt.Errorf("delete principal limits: %w", err)
	}
	return nil
}
