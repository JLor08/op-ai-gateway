// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"strings"
	"time"
)

func (s *SQLiteStore) CreatePlainToken(ctx context.Context, token TokenRecord, secret string) error {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return fmt.Errorf("create token: secret is required")
	}
	if token.Status == "" {
		token.Status = TokenStatusActive
	}
	if token.Scopes == "" {
		token.Scopes = "[]"
	}
	if token.Kind == "" {
		token.Kind = TokenKindUser
	}
	if token.CreatedAt.IsZero() {
		token.CreatedAt = time.Now().UTC()
	}
	if token.UpdatedAt.IsZero() {
		token.UpdatedAt = token.CreatedAt
	}
	token.SecretHash = auth.HashSecret(secret)
	token.SecretPrefix = secretPrefix(secret)

	// user_id/service_id are mutually NULL-able: a user token has a real
	// user_id and a NULL service_id, a service token the reverse. Both columns
	// carry a `references ... on delete cascade` FK, which — unlike NOT NULL —
	// only exempts a genuine SQL NULL from the reference-existence check, so an
	// empty Go string must become sql.NullString{} (NULL), not "" (which would
	// be checked against the referenced table and rejected).
	_, err := s.exec(ctx, `
		insert into api_tokens (
			id, user_id, name, secret_hash, secret_prefix, status, scopes,
			expires_at, last_used_at, created_at, updated_at, model_override, model_override_map,
			log_communication, secret, service_id, kind, project_id,
			server_override, server_override_force_unreachable,
			last_used_model, unknown_model_redirect, unknown_model_redirect_blocked, unknown_model_fallback,
			vendor_provider_access, claude_model_aliases, claude_effort_compatibility,
			decisions_confidence_normalize, claude_code_usage_display,
			send_usage_limits, codex_model_switch_prompt
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		token.ID,
		nullableTokenRef(token.UserID),
		token.Name,
		token.SecretHash,
		token.SecretPrefix,
		token.Status,
		token.Scopes,
		token.ExpiresAt,
		token.LastUsedAt,
		token.CreatedAt,
		token.UpdatedAt,
		token.ModelOverride,
		token.ModelOverrideMap,
		token.LogCommunication,
		token.Secret,
		nullableTokenRef(token.ServiceID),
		token.Kind,
		nullableTokenRef(token.ProjectID),
		token.ServerOverride,
		token.ServerOverrideForceUnreachable,
		token.LastUsedModel,
		token.UnknownModelRedirect,
		token.UnknownModelRedirectBlocked,
		token.UnknownModelFallback,
		token.VendorProviderAccess,
		token.ClaudeModelAliases,
		token.ClaudeEffortCompatibility,
		token.DecisionsConfidenceNormalize,
		token.ClaudeCodeUsageDisplay,
		token.SendUsageLimits,
		token.CodexModelSwitchPrompt,
	)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create token: %w", err)
	}
	return nil
}

// nullableTokenRef converts an empty string to a genuine SQL NULL (Valid:
// false) and a non-empty string to itself — used for api_tokens.user_id and
// .service_id, whose FK constraints only exempt NULL from the
// reference-existence check (an empty string would be checked against the
// referenced table and rejected).
func nullableTokenRef(v string) sql.NullString {
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

// tokenColumns is the shared column list for a full TokenRecord row, matching
// scanToken's Scan order. user_id/service_id/project_id are read via a
// coalesce-to-empty-string so the Go-side scan stays a plain string
// regardless of which one is NULL for a given row (a service token has NULL
// user_id; a user token has NULL service_id; project_id is NULL whenever a
// token has no project).
const tokenColumns = `id, coalesce(user_id,''), name, secret_hash, secret_prefix, status, scopes,
		expires_at, last_used_at, created_at, updated_at, model_override, model_override_map, log_communication, secret,
		coalesce(service_id,''), kind, coalesce(project_id,''), server_override, server_override_force_unreachable,
		last_used_model, unknown_model_redirect, unknown_model_redirect_blocked, unknown_model_fallback,
		vendor_provider_access, claude_model_aliases, claude_effort_compatibility, decisions_confidence_normalize,
		claude_code_usage_display, send_usage_limits, codex_model_switch_prompt`

func (s *SQLiteStore) TokenByID(ctx context.Context, id string) (TokenRecord, error) {
	row := s.queryRow(ctx, `
		select `+tokenColumns+`
		from api_tokens
		where id = ?`, id)
	return scanToken(row)
}

func (s *SQLiteStore) TokensByUser(ctx context.Context, userID string) ([]TokenRecord, error) {
	rows, err := s.query(ctx, `
		select `+tokenColumns+`
		from api_tokens
		where user_id = ?
		order by created_at desc, id desc`, userID)
	if err != nil {
		return nil, fmt.Errorf("list tokens by user: %w", err)
	}
	defer rows.Close()
	records := make([]TokenRecord, 0)
	for rows.Next() {
		record, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tokens by user: %w", err)
	}
	return records, nil
}

// TokensByService lists a service's tokens (kind="service"), newest first —
// mirrors TokensByUser. A service with no tokens returns an empty (non-nil)
// slice, no error.
func (s *SQLiteStore) TokensByService(ctx context.Context, serviceID string) ([]TokenRecord, error) {
	rows, err := s.query(ctx, `
		select `+tokenColumns+`
		from api_tokens
		where service_id = ?
		order by created_at desc, id desc`, serviceID)
	if err != nil {
		return nil, fmt.Errorf("list tokens by service: %w", err)
	}
	defer rows.Close()
	records := make([]TokenRecord, 0)
	for rows.Next() {
		record, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tokens by service: %w", err)
	}
	return records, nil
}

// TokensByProject lists a project's assigned tokens (any USER token whose
// project_id equals projectID; a service token never has a project), newest
// first -- mirrors TokensByUser/TokensByService. An empty projectID returns
// an empty slice WITHOUT querying, so it can never match a NULL/empty
// project_id column (unassigned tokens are not "the project with id \"\"").
func (s *SQLiteStore) TokensByProject(ctx context.Context, projectID string) ([]TokenRecord, error) {
	if projectID == "" {
		return []TokenRecord{}, nil
	}
	rows, err := s.query(ctx, `
		select `+tokenColumns+`
		from api_tokens
		where project_id = ?
		order by created_at desc, id desc`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list tokens by project: %w", err)
	}
	defer rows.Close()
	records := make([]TokenRecord, 0)
	for rows.Next() {
		record, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tokens by project: %w", err)
	}
	return records, nil
}

func (s *SQLiteStore) UpdateTokenMetadata(ctx context.Context, token TokenRecord) error {
	if token.UpdatedAt.IsZero() {
		token.UpdatedAt = time.Now().UTC()
	}
	// last_used_model is NOT in this SET clause, for the same reason
	// last_used_at is not: both are written by the REQUEST path
	// (SetTokenLastUsedModel and the LookupBearer touch), while a caller here
	// carries a record it read moments earlier. Including either would let an
	// unrelated metadata edit racing an inference request roll the value back.
	// For the marker that matters more than for the timestamp: it is the
	// unknown-model redirect's own first target, so a silent rollback sends the
	// next unknown request to the fallback instead of the model the token
	// actually used last.
	res, err := s.exec(ctx, `
		update api_tokens
		set name = ?, scopes = ?, status = ?, updated_at = ?, model_override = ?, model_override_map = ?, log_communication = ?, secret = ?, project_id = ?,
			server_override = ?, server_override_force_unreachable = ?,
			unknown_model_redirect = ?, unknown_model_redirect_blocked = ?, unknown_model_fallback = ?,
			vendor_provider_access = ?, claude_model_aliases = ?, claude_effort_compatibility = ?, decisions_confidence_normalize = ?,
			send_usage_limits = ?, codex_model_switch_prompt = ?
		where id = ?`,
		token.Name, token.Scopes, token.Status, token.UpdatedAt, token.ModelOverride, token.ModelOverrideMap, token.LogCommunication, token.Secret, nullableTokenRef(token.ProjectID),
		token.ServerOverride, token.ServerOverrideForceUnreachable,
		token.UnknownModelRedirect, token.UnknownModelRedirectBlocked, token.UnknownModelFallback,
		token.VendorProviderAccess, token.ClaudeModelAliases, token.ClaudeEffortCompatibility, token.DecisionsConfidenceNormalize,
		token.SendUsageLimits, token.CodexModelSwitchPrompt,
		token.ID)
	if err != nil {
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("update token: %w", err)
	}
	return requireAffected(res)
}

// SetTokenLastUsedModel records the gateway model or group name of a token's
// last successfully routed request. It is unconditional — callers write only
// when the value actually changes (see gateway.Server.resolveTarget); this
// method itself always writes.
func (s *SQLiteStore) SetTokenLastUsedModel(ctx context.Context, tokenID, model string) error {
	res, err := s.exec(ctx, `
		update api_tokens
		set last_used_model = ?, updated_at = ?
		where id = ?`,
		model, time.Now().UTC(), tokenID)
	if err != nil {
		return fmt.Errorf("set token last used model: %w", err)
	}
	return requireAffected(res)
}

func (s *SQLiteStore) RotateTokenSecret(ctx context.Context, id, secretHash, secretPrefix string, updatedAt time.Time) error {
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	res, err := s.exec(ctx, `
		update api_tokens
		set secret_hash = ?, secret_prefix = ?, updated_at = ?
		where id = ?`,
		secretHash, secretPrefix, updatedAt, id)
	if err != nil {
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("rotate token secret: %w", err)
	}
	return requireAffected(res)
}

// RotateUserTokenSecret is RotateTokenSecret that also writes the token's
// claude_code_usage_display flag in the same statement: the flag decides the
// shape of the secret, so the two must change together or not at all.
func (s *SQLiteStore) RotateUserTokenSecret(ctx context.Context, id, secretHash, secretPrefix string, claudeCodeUsageDisplay bool, updatedAt time.Time) error {
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	res, err := s.exec(ctx, `
		update api_tokens
		set secret_hash = ?, secret_prefix = ?, claude_code_usage_display = ?, updated_at = ?
		where id = ?`,
		secretHash, secretPrefix, claudeCodeUsageDisplay, updatedAt, id)
	if err != nil {
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("rotate token secret: %w", err)
	}
	return requireAffected(res)
}

func (s *SQLiteStore) DeleteToken(ctx context.Context, id string) error {
	res, err := s.exec(ctx, `delete from api_tokens where id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	return requireAffected(res)
}

// LookupBearer resolves an Authorization header to the token it authenticates and
// records the use (last_used_at). Every failure -- a refusal (resolveBearer's
// auth.ErrBearerRefused), a failed read or a failed write of the use -- is false,
// so the request path fails closed.
func (s *SQLiteStore) LookupBearer(header string) (auth.Token, bool) {
	ctx := context.Background()
	token, err := s.resolveBearer(ctx, header)
	if err != nil {
		return auth.Token{}, false
	}
	usedAt := time.Now().UTC()
	if _, err := s.exec(ctx, `update api_tokens set last_used_at = ?, updated_at = ? where id = ?`, usedAt, usedAt, token.ID); err != nil {
		return auth.Token{}, false
	}
	return token, true
}

// RecheckBearer implements auth.BearerRechecker: LookupBearer's resolution without
// its write of the use, under ctx. A refusal is auth.ErrBearerRefused; any other
// error is a read that failed (or ctx ending) and says nothing about the token.
func (s *SQLiteStore) RecheckBearer(ctx context.Context, header string) (auth.Token, error) {
	return s.resolveBearer(ctx, header)
}

// resolveBearer is the side-effect-free core LookupBearer and RecheckBearer share:
// it reads the token by the header's secret hash (and, for a service token, the
// service and its model allowlist) under ctx. It returns auth.ErrBearerRefused when
// the credential definitely does not authenticate -- no bearer secret, no token with
// that hash, a token that is not active or has expired, a service token without a
// service id, whose service is gone or disabled -- and any other error when a read
// failed.
func (s *SQLiteStore) resolveBearer(ctx context.Context, header string) (auth.Token, error) {
	record, err := s.readLiveBearerRecord(ctx, header)
	if err != nil {
		return auth.Token{}, err
	}
	serviceName, allowedModels, err := s.resolveBearerService(ctx, record)
	if err != nil {
		return auth.Token{}, err
	}

	// ProjectName is resolved best-effort (unlike serviceName above, which
	// fails the lookup closed): a project attribution is a display/attribution
	// concern, not a security gate, so a stale/deleted project id (a delete
	// sets project_id NULL via the FK anyway; a transient read error here)
	// never blocks an otherwise-valid token from authenticating.
	var projectName string
	if record.ProjectID != "" {
		projRow := s.queryRow(ctx, `select name from projects where id = ?`, record.ProjectID)
		_ = projRow.Scan(&projectName)
	}
	// The best-effort read above swallows its error, so a context that ended during
	// it would otherwise hand back a token whose reads did not all run. A re-check
	// promises that any non-refusal answer is a whole one, so an ended context is
	// the read failure it is. LookupBearer's background context never ends, so its
	// answer is unchanged.
	if err := ctx.Err(); err != nil {
		return auth.Token{}, fmt.Errorf("resolve bearer: %w", err)
	}

	return auth.Token{
		ID:                             record.ID,
		UserID:                         record.UserID,
		Name:                           record.Name,
		Active:                         true,
		Scopes:                         tokenScopes(record.Scopes),
		ExpiresAt:                      record.ExpiresAt,
		ModelOverride:                  record.ModelOverride,
		ModelOverrideRules:             AuthModelOverrideRules(DecodeModelOverrideRules(record.ModelOverrideMap)),
		LogCommunication:               record.LogCommunication,
		Secret:                         record.Secret,
		ServiceID:                      record.ServiceID,
		ServiceName:                    serviceName,
		Kind:                           record.Kind,
		AllowedModels:                  allowedModels,
		ProjectID:                      record.ProjectID,
		ProjectName:                    projectName,
		ServerOverride:                 record.ServerOverride,
		ServerOverrideForceUnreachable: record.ServerOverrideForceUnreachable,
		LastUsedModel:                  record.LastUsedModel,
		UnknownModelRedirect:           record.UnknownModelRedirect,
		UnknownModelRedirectBlocked:    record.UnknownModelRedirectBlocked,
		UnknownModelFallback:           record.UnknownModelFallback,
		VendorAccess:                   DecodeVendorAccess(record.VendorProviderAccess),
		ClaudeModelAliases:             record.ClaudeModelAliases,
		ClaudeEffortCompatibility:      record.ClaudeEffortCompatibility,
		DecisionsConfidenceNormalize:   record.DecisionsConfidenceNormalize,
		SendUsageLimits:                record.SendUsageLimits,
		CodexModelSwitchPrompt:         record.CodexModelSwitchPrompt,
	}, nil
}

// readLiveBearerRecord reads the token the header's bearer secret names, under ctx, and
// refuses one that cannot authenticate on its own record: no bearer secret, no token with
// that hash, a token that is not active or has expired (auth.ErrBearerRefused). Any other
// error is a read that failed.
func (s *SQLiteStore) readLiveBearerRecord(ctx context.Context, header string) (TokenRecord, error) {
	secret, ok := auth.ExtractBearerSecret(header)
	if !ok {
		return TokenRecord{}, auth.ErrBearerRefused
	}
	row := s.queryRow(ctx, `
		select `+tokenColumns+`
		from api_tokens
		where secret_hash = ?`, auth.HashSecret(secret))
	record, err := scanToken(row)
	if errors.Is(err, ErrNotFound) { // scanToken's sql.ErrNoRows
		return TokenRecord{}, auth.ErrBearerRefused
	}
	if err != nil {
		return TokenRecord{}, fmt.Errorf("read bearer token: %w", err)
	}
	if record.Status != TokenStatusActive {
		return TokenRecord{}, auth.ErrBearerRefused
	}
	if record.ExpiresAt != nil && !record.ExpiresAt.After(time.Now().UTC()) {
		return TokenRecord{}, auth.ErrBearerRefused
	}
	return record, nil
}

// resolveBearerService reads what a SERVICE token additionally needs, under ctx: its
// service's name (display), the live disabled-gate and the model allowlist. Any other
// kind of token needs none of it and gets empty values. A read error fails the whole
// lookup closed (never falls back to an unrestricted/empty allowlist -- that would be a
// privilege escalation for a token meant to be limited); a service-kind token without a
// service id, or whose service is gone or disabled, is refused (auth.ErrBearerRefused).
func (s *SQLiteStore) resolveBearerService(ctx context.Context, record TokenRecord) (serviceName string, allowedModels []string, err error) {
	if record.Kind != TokenKindService {
		return "", nil, nil
	}
	if record.ServiceID == "" {
		// A service-kind token with no service_id is a data-integrity anomaly; never
		// treat it as a valid, unbound service token.
		return "", nil, auth.ErrBearerRefused
	}
	var status string
	svcRow := s.queryRow(ctx, `select name, status from services where id = ?`, record.ServiceID)
	if err := svcRow.Scan(&serviceName, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, auth.ErrBearerRefused
		}
		return "", nil, fmt.Errorf("read bearer token's service: %w", err)
	}
	if status == routing.ServerStatusDisabled {
		return "", nil, auth.ErrBearerRefused
	}
	models, err := s.ServiceAllowedModels(ctx, record.ServiceID)
	if err != nil {
		return "", nil, fmt.Errorf("read bearer token's service allowlist: %w", err)
	}
	return serviceName, models, nil
}

func tokenScopes(scopesJSON string) []string {
	var scopes []string
	if err := json.Unmarshal([]byte(scopesJSON), &scopes); err != nil {
		return nil
	}
	return append([]string(nil), scopes...)
}

func scanToken(row rowScanner) (TokenRecord, error) {
	var token TokenRecord
	var expiresAt sql.NullTime
	var lastUsedAt sql.NullTime
	var logComm int64
	var secret int64
	var serverOverrideForceUnreachable int64
	var unknownModelRedirect int64
	var unknownModelRedirectBlocked int64
	var claudeModelAliases int64
	var claudeEffortCompatibility int64
	var decisionsConfidenceNormalize int64
	var claudeCodeUsageDisplay int64
	var sendUsageLimits int64
	var codexModelSwitchPrompt int64
	err := row.Scan(
		&token.ID,
		&token.UserID,
		&token.Name,
		&token.SecretHash,
		&token.SecretPrefix,
		&token.Status,
		&token.Scopes,
		&expiresAt,
		&lastUsedAt,
		&token.CreatedAt,
		&token.UpdatedAt,
		&token.ModelOverride,
		&token.ModelOverrideMap,
		&logComm,
		&secret,
		&token.ServiceID,
		&token.Kind,
		&token.ProjectID,
		&token.ServerOverride,
		&serverOverrideForceUnreachable,
		&token.LastUsedModel,
		&unknownModelRedirect,
		&unknownModelRedirectBlocked,
		&token.UnknownModelFallback,
		&token.VendorProviderAccess,
		&claudeModelAliases,
		&claudeEffortCompatibility,
		&decisionsConfidenceNormalize,
		&claudeCodeUsageDisplay,
		&sendUsageLimits,
		&codexModelSwitchPrompt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return TokenRecord{}, ErrNotFound
	}
	if err != nil {
		return TokenRecord{}, fmt.Errorf("scan token: %w", err)
	}
	if expiresAt.Valid {
		token.ExpiresAt = &expiresAt.Time
	}
	if lastUsedAt.Valid {
		token.LastUsedAt = &lastUsedAt.Time
	}
	token.LogCommunication = logComm != 0
	token.Secret = secret != 0
	token.ServerOverrideForceUnreachable = serverOverrideForceUnreachable != 0
	token.UnknownModelRedirect = unknownModelRedirect != 0
	token.UnknownModelRedirectBlocked = unknownModelRedirectBlocked != 0
	token.ClaudeModelAliases = claudeModelAliases != 0
	token.ClaudeEffortCompatibility = claudeEffortCompatibility != 0
	token.DecisionsConfidenceNormalize = decisionsConfidenceNormalize != 0
	token.ClaudeCodeUsageDisplay = claudeCodeUsageDisplay != 0
	token.SendUsageLimits = sendUsageLimits != 0
	token.CodexModelSwitchPrompt = codexModelSwitchPrompt != 0
	return token, nil
}

func secretPrefix(secret string) string {
	if len(secret) <= 8 {
		return secret
	}
	return secret[:8]
}
