// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/routing"
	"strings"
)

// putRequestFromDTO spreads a LOADED runtime-spec document into the
// full-document upsert request, so a caller that wants to change one field
// changes exactly one field.
//
// It exists because Go has no `...rest` spread and because this project
// already paid for the alternative: a runtime-spec write assembled from a
// hand-picked field list quietly reset the operator's binary path, args,
// timeouts and GPU rows, and the test that asserted only "admin_state came
// out right" passed anyway. The mapper IS the spread, and
// TestPutRequestFromDTOCoversEveryWritableField fails the moment
// RuntimeSpecDTO gains a field this does not carry across.
//
// GPUs is passed through INCLUDING each row's VRAMMeasuredMB, which the
// upsert then ignores in favour of the stored value (the VRAM ownership rule
// on PutRuntimeSpec). Copying it here rather than blanking it keeps this a
// pure spread with no opinion of its own -- the ownership rule lives in one
// place, and a reader of this function is not invited to think it lives in
// two.
func putRequestFromDTO(dto RuntimeSpecDTO) PutRuntimeSpecRequest {
	return PutRuntimeSpecRequest{
		Enabled:                     dto.Enabled,
		Binary:                      dto.Binary,
		Args:                        dto.Args,
		Env:                         dto.Env,
		WorkDir:                     dto.WorkDir,
		ListenPort:                  dto.ListenPort,
		HealthPath:                  dto.HealthPath,
		HealthTimeoutSeconds:        dto.HealthTimeoutSeconds,
		StartupTimeoutSeconds:       dto.StartupTimeoutSeconds,
		IdleTimeoutSeconds:          dto.IdleTimeoutSeconds,
		AdmissionWaitTimeoutSeconds: dto.AdmissionWaitTimeoutSeconds,
		Pinned:                      dto.Pinned,
		AdminState:                  dto.AdminState,
		VRAMLocked:                  dto.VRAMLocked,
		SetVisibleDevices:           dto.SetVisibleDevices,
		GPUs:                        dto.GPUs,
		APIFlavors:                  dto.APIFlavors,
		ResponsesMode:               dto.ResponsesMode,
		MessagesMode:                dto.MessagesMode,
	}
}

// SetBenchmarkRuntimeSpecAdminState sets ONE launch spec's admin_state, keyed
// by the spec's own id, as a compare-and-set against expectedAdminState.
//
// WHO AUTHORIZED THIS. Nobody, here -- deliberately, and this sentence is the
// contract. Like AgentRuntimeConfig, it takes no auth.Token and is authorized
// by its CALLER: the only caller is the VRAM benchmark's isolation sequence,
// whose trigger request was gated by AuthorizeBenchmarkScope plus that run's
// own preconditions before a single spec was touched. A future caller that
// cannot point at an equivalent gate is inventing an unauthorized write path.
//
// WHY NOT PutRuntimeSpec WITH THE TRIGGER'S TOKEN. That token would compile
// (auth.Token is a plain value struct) and it would reuse this same writer --
// but every authorization here re-derives from STORE ROWS
// (authorizeMapping -> authorizeApplication -> authorizeServer ->
// ServerOwners), so the benchmark's DEFERRED restore -- minutes later, when
// the whole server is force-stopped and the restore is the only thing between
// the operator and clearing every override by hand -- could be refused for
// reasons that have nothing to do with the run: the user removed from the
// server's owners, the mapping or application deleted mid-run. A
// safety-critical restore must not have an authorization failure mode.
// Synthesizing a system principal was the other option and is worse: no
// production code in this tree fabricates one, and adding the first for a
// benchmark creates a privilege surface far larger than the feature.
//
// WHY A FULL-DOCUMENT WRITE. admin_state is row 4 of the runtime-config
// document (see THE RULE on notifyRuntimeChanged), so this write owes a
// notification -- and notifyRuntimeChanged is the SOLE trigger for the
// gateway's PushRuntimeConfig. Routing it through putRuntimeSpec gets the
// notification, the application-type gate and the VRAM ownership rule from
// the one implementation that already has them. The tempting alternative, a
// narrow one-column setter modelled on UpdateRuntimeSpecGPUMeasured, would
// silently inherit that method's "do not notify" exemption -- an exemption
// that exists because it is the AGENT's own write-back, which is exactly what
// this is not.
//
// WHY COMPARE-AND-SET. There is no If-Match and no row version on this
// endpoint class, and the caller's restore runs long after its drain. The
// expectation makes both directions safe: the drain passes "" (the run
// already refused to start against any pre-existing override) and the restore
// passes "force_stopped". A mismatch is ErrRuntimeSpecAdminStateConflict and
// writes NOTHING -- somebody else owns the field now.
//
// ErrRuntimeSpecNotFound means the spec is gone, and a caller must read that
// as "the override went with it", not as a failed restore.
func (s *Service) SetBenchmarkRuntimeSpecAdminState(ctx context.Context, specID, expectedAdminState, adminState string) (RuntimeSpecDTO, error) {
	specID = strings.TrimSpace(specID)
	if specID == "" || s.routes == nil {
		return RuntimeSpecDTO{}, ErrRuntimeSpecNotFound
	}
	spec, ok, err := s.routes.RuntimeSpecByID(ctx, specID)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	if !ok {
		return RuntimeSpecDTO{}, ErrRuntimeSpecNotFound
	}
	if strings.TrimSpace(spec.AdminState) != strings.TrimSpace(expectedAdminState) {
		return RuntimeSpecDTO{}, ErrRuntimeSpecAdminStateConflict
	}
	gpus, err := s.routes.RuntimeSpecGPUs(ctx, spec.ID)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	// The SPREAD: re-read the whole document, replace one field. Never an
	// assembled field list -- see putRequestFromDTO.
	dto, err := runtimeSpecDTO(spec, gpus)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	req := putRequestFromDTO(dto)
	req.AdminState = adminState

	mapping, app, server, err := s.resolveMappingChain(ctx, spec.MappingID)
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	return s.putRuntimeSpec(ctx, mapping, app, server, req)
}

// resolveMappingChain is authorizeMapping with the authorization removed: it
// resolves a mapping to its owning application and AI server, which is what
// putRuntimeSpec needs for its application-type gate and for the server id it
// notifies. Every absent row collapses to ErrMappingNotFound, matching
// authorizeMapping's no-existence-leak posture, so a caller cannot learn
// which link of the chain was missing.
//
// It carries no principal on purpose and must only be reached from a method
// whose own doc block states who authorized it (today:
// SetBenchmarkRuntimeSpecAdminState).
func (s *Service) resolveMappingChain(ctx context.Context, mappingID string) (routing.ModelMapping, routing.Application, routing.AIServer, error) {
	mappingID = strings.TrimSpace(mappingID)
	if mappingID == "" {
		return routing.ModelMapping{}, routing.Application{}, routing.AIServer{}, ErrMappingNotFound
	}
	mapping, err := s.routes.MappingByID(ctx, mappingID)
	if err != nil {
		return routing.ModelMapping{}, routing.Application{}, routing.AIServer{}, ErrMappingNotFound
	}
	app, err := s.routes.ApplicationByID(ctx, mapping.ApplicationID)
	if err != nil {
		return routing.ModelMapping{}, routing.Application{}, routing.AIServer{}, ErrMappingNotFound
	}
	server, err := s.routes.AIServerByID(ctx, app.ServerID)
	if err != nil {
		return routing.ModelMapping{}, routing.Application{}, routing.AIServer{}, ErrMappingNotFound
	}
	return mapping, app, server, nil
}
