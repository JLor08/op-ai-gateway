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
			last_used_model, unknown_model_redirect, unknown_model_redirect_blocked, unknown_model_fallback
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
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
		last_used_model, unknown_model_redirect, unknown_model_redirect_blocked, unknown_model_fallback`

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
	res, err := s.exec(ctx, `
		update api_tokens
		set name = ?, scopes = ?, status = ?, updated_at = ?, model_override = ?, model_override_map = ?, log_communication = ?, secret = ?, project_id = ?,
			server_override = ?, server_override_force_unreachable = ?,
			last_used_model = ?, unknown_model_redirect = ?, unknown_model_redirect_blocked = ?, unknown_model_fallback = ?
		where id = ?`,
		token.Name, token.Scopes, token.Status, token.UpdatedAt, token.ModelOverride, token.ModelOverrideMap, token.LogCommunication, token.Secret, nullableTokenRef(token.ProjectID),
		token.ServerOverride, token.ServerOverrideForceUnreachable,
		token.LastUsedModel, token.UnknownModelRedirect, token.UnknownModelRedirectBlocked, token.UnknownModelFallback,
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

func (s *SQLiteStore) DeleteToken(ctx context.Context, id string) error {
	res, err := s.exec(ctx, `delete from api_tokens where id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	return requireAffected(res)
}

func (s *SQLiteStore) LookupBearer(header string) (auth.Token, bool) {
	secret, ok := auth.ExtractBearerSecret(header)
	if !ok {
		return auth.Token{}, false
	}
	hash := auth.HashSecret(secret)
	row := s.queryRow(context.Background(), `
		select `+tokenColumns+`
		from api_tokens
		where secret_hash = ?`, hash)
	record, err := scanToken(row)
	if err != nil {
		return auth.Token{}, false
	}
	if record.Status != TokenStatusActive {
		return auth.Token{}, false
	}
	if record.ExpiresAt != nil && !record.ExpiresAt.After(time.Now().UTC()) {
		return auth.Token{}, false
	}

	// A service token additionally needs its service's name (display) + live
	// disabled-gate + model allowlist. Any read error here fails the whole
	// lookup closed (never falls back to an unrestricted/empty allowlist —
	// that would be a privilege escalation for a token meant to be limited).
	var serviceName string
	var allowedModels []string
	if record.Kind == TokenKindService {
		if record.ServiceID == "" {
			// A service-kind token with no service_id is a data-integrity
			// anomaly; never treat it as a valid, unbound service token.
			return auth.Token{}, false
		}
		var status string
		svcRow := s.queryRow(context.Background(), `select name, status from services where id = ?`, record.ServiceID)
		if err := svcRow.Scan(&serviceName, &status); err != nil {
			return auth.Token{}, false
		}
		if status == routing.ServerStatusDisabled {
			return auth.Token{}, false
		}
		models, err := s.ServiceAllowedModels(context.Background(), record.ServiceID)
		if err != nil {
			return auth.Token{}, false
		}
		allowedModels = models
	}

	// ProjectName is resolved best-effort (unlike serviceName above, which
	// fails the lookup closed): a project attribution is a display/attribution
	// concern, not a security gate, so a stale/deleted project id (a delete
	// sets project_id NULL via the FK anyway; a transient read error here)
	// never blocks an otherwise-valid token from authenticating.
	var projectName string
	if record.ProjectID != "" {
		projRow := s.queryRow(context.Background(), `select name from projects where id = ?`, record.ProjectID)
		_ = projRow.Scan(&projectName)
	}

	usedAt := time.Now().UTC()
	if _, err := s.exec(context.Background(), `update api_tokens set last_used_at = ?, updated_at = ? where id = ?`, usedAt, usedAt, record.ID); err != nil {
		return auth.Token{}, false
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
	}, true
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
	return token, nil
}

func secretPrefix(secret string) string {
	if len(secret) <= 8 {
		return secret
	}
	return secret[:8]
}
