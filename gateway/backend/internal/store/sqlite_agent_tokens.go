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

func (s *SQLiteStore) UpsertAgentToken(ctx context.Context, token routing.AgentToken, secretHash string) error {
	// Pre-check the server so the common case returns a clean ErrNotFound; the
	// insert ... on conflict FK-constraint fallback below is the TOCTOU backstop.
	if _, err := s.AIServerByID(ctx, token.ServerID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("validate agent token server: %w", err)
	}
	_, err := s.exec(ctx, `
		insert into agent_tokens (
			id, server_id, secret_hash, secret_prefix, last_used_at, created_at, updated_at
		) values (?, ?, ?, ?, NULL, ?, ?)
		on conflict(server_id) do update set
			id = excluded.id,
			secret_hash = excluded.secret_hash,
			secret_prefix = excluded.secret_prefix,
			last_used_at = NULL,
			updated_at = excluded.updated_at`,
		token.ID, token.ServerID, secretHash, token.SecretPrefix, token.CreatedAt, token.UpdatedAt,
	)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("upsert agent token: %w", err)
	}
	return nil
}

func (s *SQLiteStore) AgentTokenByServer(ctx context.Context, serverID string) (routing.AgentToken, bool, error) {
	row := s.queryRow(ctx, `
		select id, server_id, secret_prefix, last_used_at, created_at, updated_at
		from agent_tokens where server_id = ?`, serverID)
	token, err := scanAgentToken(row)
	if errors.Is(err, ErrNotFound) {
		return routing.AgentToken{}, false, nil
	}
	if err != nil {
		return routing.AgentToken{}, false, err
	}
	return token, true, nil
}

func (s *SQLiteStore) DeleteAgentTokenByServer(ctx context.Context, serverID string) error {
	if _, err := s.exec(ctx, `delete from agent_tokens where server_id = ?`, serverID); err != nil {
		return fmt.Errorf("delete agent token: %w", err)
	}
	return nil
}

func (s *SQLiteStore) LookupAgentToken(ctx context.Context, secretHash string) (string, bool, error) {
	var serverID string
	err := s.queryRow(ctx, `select server_id from agent_tokens where secret_hash = ?`, secretHash).Scan(&serverID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("lookup agent token: %w", err)
	}
	if _, err := s.exec(ctx, `update agent_tokens set last_used_at = ? where secret_hash = ?`, time.Now().UTC(), secretHash); err != nil {
		return "", false, fmt.Errorf("bump agent token last_used_at: %w", err)
	}
	return serverID, true, nil
}

func scanAgentToken(row rowScanner) (routing.AgentToken, error) {
	var token routing.AgentToken
	var lastUsed sql.NullTime
	err := row.Scan(&token.ID, &token.ServerID, &token.SecretPrefix, &lastUsed, &token.CreatedAt, &token.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.AgentToken{}, ErrNotFound
	}
	if err != nil {
		return routing.AgentToken{}, fmt.Errorf("scan agent token: %w", err)
	}
	if lastUsed.Valid {
		t := lastUsed.Time
		token.LastUsedAt = &t
	}
	return token, nil
}
