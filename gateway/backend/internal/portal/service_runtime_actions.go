// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
)

// RuntimeActionScope is what a manual start or stop of a mapping's managed
// model acts on: the mapping, its application and server, and the stored
// runtime spec.
type RuntimeActionScope struct {
	Server      routing.AIServer
	Application routing.Application
	Mapping     routing.ModelMapping
	Spec        routing.RuntimeSpec
}

// AuthorizeRuntimeAction authorizes a manual start or stop of mappingID's
// managed model and returns its scope. The authorization is the runtime-spec
// PUT's (authorizeMapping: the mapping write rule, collapsed to
// ErrMappingNotFound), and so are the refusals that follow it, in the PUT's
// order:
//   - ErrRuntimeSpecNotServerAgent for a mapping whose application is not an
//     agent's;
//   - ErrRuntimeSpecServerBenchmarking while a benchmark run holds the server:
//     a start or a stop of a sibling inside a run's window contaminates its
//     measurement as much as an override write would;
//   - ErrRuntimeSpecNotFound for a mapping with no spec, which nothing runs.
//
// Neither action writes anything: a start or a stop is transient, and the
// spec's admin_state is the persistent override (PutRuntimeSpec).
func (s *Service) AuthorizeRuntimeAction(ctx context.Context, principal auth.Token, mappingID string) (RuntimeActionScope, error) {
	mapping, app, server, err := s.authorizeMapping(ctx, principal, mappingID)
	if err != nil {
		return RuntimeActionScope{}, err
	}
	if app.Type != routing.ProviderServerAgent {
		return RuntimeActionScope{}, ErrRuntimeSpecNotServerAgent
	}
	if s.serverIsBenchmarking(server.ID) {
		return RuntimeActionScope{}, ErrRuntimeSpecServerBenchmarking
	}
	spec, ok, err := s.routes.RuntimeSpecByMapping(ctx, mapping.ID)
	if err != nil {
		return RuntimeActionScope{}, err
	}
	if !ok {
		return RuntimeActionScope{}, ErrRuntimeSpecNotFound
	}
	return RuntimeActionScope{Server: server, Application: app, Mapping: mapping, Spec: spec}, nil
}
