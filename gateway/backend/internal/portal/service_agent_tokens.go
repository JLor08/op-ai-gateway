// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"time"
)

type AgentTokenDTO struct {
	Exists       bool       `json:"exists"`
	SecretPrefix string     `json:"secret_prefix,omitempty"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	CreatedAt    *time.Time `json:"created_at,omitempty"`
	UpdatedAt    *time.Time `json:"updated_at,omitempty"`
}

type GenerateAgentTokenResponse struct {
	Secret string        `json:"secret"`
	Token  AgentTokenDTO `json:"token"`
}

func (s *Service) GenerateAgentToken(ctx context.Context, principal auth.Token, serverID string) (GenerateAgentTokenResponse, error) {
	server, err := s.authorizeServer(ctx, principal, serverID)
	if err != nil {
		return GenerateAgentTokenResponse{}, err
	}
	secret, err := s.secretGenerator()
	if err != nil {
		return GenerateAgentTokenResponse{}, err
	}
	now := s.clock().UTC()
	token := routing.AgentToken{
		ID:           "agt_" + compactRandomHex(16),
		ServerID:     server.ID,
		SecretPrefix: secretPrefix(secret),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.routes.UpsertAgentToken(ctx, token, auth.HashSecret(secret)); err != nil {
		return GenerateAgentTokenResponse{}, err
	}
	stored, _, err := s.routes.AgentTokenByServer(ctx, server.ID)
	if err != nil {
		return GenerateAgentTokenResponse{}, err
	}
	return GenerateAgentTokenResponse{Secret: secret, Token: agentTokenDTO(stored)}, nil
}

func (s *Service) AgentTokenStatus(ctx context.Context, principal auth.Token, serverID string) (AgentTokenDTO, error) {
	server, err := s.authorizeServer(ctx, principal, serverID)
	if err != nil {
		return AgentTokenDTO{}, err
	}
	token, ok, err := s.routes.AgentTokenByServer(ctx, server.ID)
	if err != nil {
		return AgentTokenDTO{}, err
	}
	if !ok {
		return AgentTokenDTO{Exists: false}, nil
	}
	return agentTokenDTO(token), nil
}

func (s *Service) RevokeAgentToken(ctx context.Context, principal auth.Token, serverID string) error {
	server, err := s.authorizeServer(ctx, principal, serverID)
	if err != nil {
		return err
	}
	return s.routes.DeleteAgentTokenByServer(ctx, server.ID)
}

func agentTokenDTO(token routing.AgentToken) AgentTokenDTO {
	created := token.CreatedAt
	updated := token.UpdatedAt
	return AgentTokenDTO{
		Exists:       true,
		SecretPrefix: token.SecretPrefix,
		LastUsedAt:   token.LastUsedAt,
		CreatedAt:    &created,
		UpdatedAt:    &updated,
	}
}
