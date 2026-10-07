// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"log/slog"
	"op-ai-gateway/internal/routing"
	"slices"
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
	liveTimings := dto.ResponsesLiveTimingsEnabled
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
		VisibleDevicesMode:          dto.VisibleDevicesMode,
		GPUs:                        dto.GPUs,
		APIFlavors:                  dto.APIFlavors,
		ResponsesMode:               dto.ResponsesMode,
		MessagesMode:                dto.MessagesMode,
		// A *bool on the request against a bool on the DTO, so this is a
		// conversion rather than a copy -- the one line in this spread that is
		// not a straight assignment. A loaded document always HAS an opinion,
		// so an explicit pointer is the honest spread of it; a nil would be
		// this mapper inventing a non-mention the document does not contain.
		//
		// In the ORDINARY case the pointer changes nothing, and this says so
		// rather than claiming a danger it averts: this mapper's only
		// PRODUCTION caller is resolveBenchmarkSpecWrite, which every
		// benchmark writer goes through (SetBenchmarkRuntimeSpecAdminState,
		// SetBenchmarkRuntimeSpecsPinned and SetBenchmarkRuntimeSpecsAdminState).
		// It resolves the spec by id before calling this mapper, returning
		// ErrRuntimeSpecNotFound when there is no such row, and the write it
		// feeds passes that id as expectSpecID, which refuses the write unless
		// the write's own read by mapping finds that very spec. So that read
		// always finds the existing spec and a nil would take the PRESERVE
		// branch -- putting back exactly what was read. The create-time
		// default is unreachable from here. "Production" is load-bearing:
		// test call sites also spread a document through this mapper, and a
		// SECOND production caller that neither resolved the spec first nor
		// passed expectSpecID would put the create default back in reach.
		//
		// What the pointer changes is the ONE pathological case: a stored true
		// on a kind that cannot honour it. A nil would let the clear arm wipe
		// it silently; the explicit pointer re-ASSERTS it and earns the 400
		// instead -- a failure worth hearing about rather than papering over.
		// Re-asserting is safe because the spread restates Type and Binary
		// from the same document (every benchmark writer re-reads the whole
		// document through this spread and then replaces AdminState or Pinned
		// alone), so the kind the value was stored under is the kind it is
		// re-asserted against, and a 400 out of the restore means the STORED
		// row already violated the invariant.
		ResponsesLiveTimingsEnabled: &liveTimings,
		APITokenMode:                dto.APITokenMode,
		APITokenHeaderSource:        dto.APITokenHeaderSource,
		APITokenHeader:              dto.APITokenHeader,
		Type:                        dto.Type,
		MetricsPath:                 dto.MetricsPath,
		ContextProbePath:            dto.ContextProbePath,
	}
}

// SetBenchmarkRuntimeSpecAdminState sets ONE launch spec's admin_state, keyed
// by the spec's own id, as a compare-and-set against expectedAdminState.
//
// WHO AUTHORIZED THIS. Nobody, here -- deliberately, and this sentence is the
// contract. Like AgentRuntimeConfig, it takes no auth.Token and is authorized
// by its CALLER: its one production caller is the VRAM run's clear of its
// target's override before the load (runVRAMProbe), whose trigger request was
// gated by AuthorizeBenchmarkScope plus that run's own preconditions before a
// single spec was touched. A future caller that cannot point at an equivalent
// gate is inventing an unauthorized write path.
//
// WHY NOT PutRuntimeSpec WITH THE TRIGGER'S TOKEN. That token would compile
// (auth.Token is a plain value struct) and it would reuse this same writer --
// but every authorization here re-derives from STORE ROWS
// (authorizeMapping -> authorizeApplication -> authorizeServer ->
// ServerOwners), so a benchmark's DEFERRED restore -- minutes later, when
// the whole server is force-stopped and a refused restore leaves it so until
// an operator or the next lease reconcile clears it -- could be refused for
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
// endpoint class, and a benchmark's clear runs long after its drain. The
// target's clear passes "force_stopped"; the batched writer
// (SetBenchmarkRuntimeSpecsAdminState) applies the same compare-and-set to the
// drain, which passes "" (the run already refused to start against any
// pre-existing override), and to every restore, which passes "force_stopped".
// A mismatch is ErrRuntimeSpecAdminStateConflict and writes NOTHING --
// somebody else owns the field now, or, at a restore, the benchmark's own
// writes never stored the override (a failed drain or stop) or already
// cleared it (the VRAM run's clear of its target, failed after it stored the
// cleared row).
//
// ErrRuntimeSpecNotFound means the spec is gone, and a caller must read that
// as "the override went with it", not as a failed restore. That includes a
// spec deleted, or deleted and created anew, between this method's read by id
// and the write's re-read by mapping: the write refuses it (expectSpecID on
// putRuntimeSpec), which narrows the race to the gap before the upsert
// without closing it (see priorRuntimeSpec).
func (s *Service) SetBenchmarkRuntimeSpecAdminState(ctx context.Context, specID, expectedAdminState, adminState string) (RuntimeSpecDTO, error) {
	w, err := s.resolveBenchmarkSpecWrite(ctx, specID, expectAdminState(expectedAdminState), withAdminState(adminState))
	if err != nil {
		return RuntimeSpecDTO{}, err
	}
	return s.putRuntimeSpec(ctx, w.mapping, w.app, w.server, w.req, w.specID)
}

// BenchmarkSpecsOutcome is what a batched benchmark writer did, per spec id,
// in input order.
type BenchmarkSpecsOutcome struct {
	Written  []string         // the compare-and-set applied
	Gone     []string         // ErrRuntimeSpecNotFound (deleted, directly or by cascade) or ErrRuntimeSpecNotServerAgent (retyped)
	Conflict []string         // the stored value was not the expected one: nothing written
	Failed   []string         // any other error, ErrMappingNotFound included; a Failed spec may still have been stored and notified (its GPU-row write or read-back failed after the upsert)
	Errs     map[string]error // the error of every spec that is not in Written; nil when every spec was written
	Notified bool             // the batch notified: at least one spec's row was stored, a Failed spec's included
}

// SetBenchmarkRuntimeSpecsPinned sets pinned on every launch spec in specIDs,
// each keyed by the spec's own id and each a compare-and-set against
// expectedPinned: a spec whose stored pinned is not expectedPinned is a
// Conflict (ErrRuntimeSpecPinnedConflict) and is not written. It is the only
// benchmark writer of pinned. The batch notifies after its last write, once
// for each server a write stored to (setBenchmarkRuntimeSpecs). The error
// return is the first Failed spec's error, and nil when nothing failed: a
// Gone or a Conflict is not a failure. specIDs must not repeat an id: the
// batch does not deduplicate, and setBenchmarkRuntimeSpecs says what a repeat
// does.
//
// WHO AUTHORIZED THIS: its callers, exactly as for
// SetBenchmarkRuntimeSpecAdminState. It takes no auth.Token, so a caller has
// to sit behind a gate equivalent to AuthorizeBenchmarkScope, or only undo
// what such a caller wrote. Its callers, and what authorized each:
// beginBenchmarkUnpin (through unpinBenchmarkPins and undoBenchmarkUnpin) and
// endBenchmarkUnpin, for a manual speed or both run behind
// AuthorizeBenchmarkScope in startBenchmark; and
// reconcileBenchmarkOverrideLease, which only undoes, by compare-and-set,
// what such a run wrote. It is not gated by serverIsBenchmarking: the run
// that holds the reservation is the caller, and the reconcile at gateway
// start runs before any run can hold one.
func (s *Service) SetBenchmarkRuntimeSpecsPinned(ctx context.Context, specIDs []string, expectedPinned, pinned bool) (BenchmarkSpecsOutcome, error) {
	out := s.setBenchmarkRuntimeSpecs(ctx, specIDs, expectPinned(expectedPinned), withPinned(pinned))
	return out, out.firstFailure()
}

// SetBenchmarkRuntimeSpecsAdminState sets admin_state on every launch spec in
// specIDs, each keyed by the spec's own id and each a compare-and-set against
// expectedAdminState, both sides trimmed: a mismatch is a Conflict
// (ErrRuntimeSpecAdminStateConflict) and is not written. It is
// SetBenchmarkRuntimeSpecAdminState for a whole set, notifying after the last
// write, once for each server a write stored to (setBenchmarkRuntimeSpecs).
// The error return is the first Failed spec's error, and nil when nothing
// failed. specIDs must not repeat an id, as for SetBenchmarkRuntimeSpecsPinned.
//
// WHO AUTHORIZED THIS: its callers, exactly as for
// SetBenchmarkRuntimeSpecAdminState and SetBenchmarkRuntimeSpecsPinned, and
// it is not gated by serverIsBenchmarking either. Its callers, and what
// authorized each: preStopServer's stop and the stop-all's clear
// (restoreBenchmarkOverrides), for a manual speed or both run behind
// AuthorizeBenchmarkScope in startBenchmark; vramDrain and the VRAM run's
// restore defer (restoreBenchmarkOverrides as well), behind startVRAMProbe's
// gate; and reconcileBenchmarkOverrideLease, which only undoes, by
// compare-and-set, what such a run wrote.
func (s *Service) SetBenchmarkRuntimeSpecsAdminState(ctx context.Context, specIDs []string, expectedAdminState, adminState string) (BenchmarkSpecsOutcome, error) {
	out := s.setBenchmarkRuntimeSpecs(ctx, specIDs, expectAdminState(expectedAdminState), withAdminState(adminState))
	return out, out.firstFailure()
}

// setBenchmarkRuntimeSpecs is the body both batched benchmark writers share.
// Per spec, in order, it is SetBenchmarkRuntimeSpecAdminState's shape
// (resolveBenchmarkSpecWrite: the read by id, expect, the mapping chain, the
// spread with apply replacing one field), then writeRuntimeSpec with the
// resolved id as expectSpecID, so a spec that is gone or replaced when the
// write re-reads the mapping is Gone and is not written (priorRuntimeSpec
// says what that guard leaves open). A spec's failure is recorded, and the
// loop goes on with the next spec.
//
// Callers pass unique ids. The batch does not deduplicate: a repeated id is
// handled once per occurrence, so after its first write a repeat meets the
// value just written, which is a Conflict unless that value is also the
// expected one (then it is written again), and Errs keeps the last error
// recorded for the id.
//
// After the loop it notifies once for each server a write stored to, in the
// order of their first write: the agent then gets one document with the whole
// batch in it, not one partial document per spec. A burst of partial documents
// can leave an agent before 0.8.1 on one of them, because such an agent drops
// a document that arrives while it is still applying the previous one.
func (s *Service) setBenchmarkRuntimeSpecs(ctx context.Context, specIDs []string, expect func(routing.RuntimeSpec) error, apply func(*PutRuntimeSpecRequest)) BenchmarkSpecsOutcome {
	var out BenchmarkSpecsOutcome
	var storedTo []string
	for _, specID := range specIDs {
		serverID, stored, err := s.setBenchmarkRuntimeSpec(ctx, specID, expect, apply)
		if stored && !slices.Contains(storedTo, serverID) {
			storedTo = append(storedTo, serverID)
		}
		out.record(specID, err)
	}
	for _, serverID := range storedTo {
		s.notifyRuntimeChanged(serverID)
	}
	out.Notified = len(storedTo) > 0
	return out
}

// setBenchmarkRuntimeSpec is one spec of a batch. It never notifies; stored
// says whether the write reached the store (writeRuntimeSpec's stored: the
// row was upserted, also when an error followed), and serverID names the
// server whose notification the batch owes for it.
func (s *Service) setBenchmarkRuntimeSpec(ctx context.Context, specID string, expect func(routing.RuntimeSpec) error, apply func(*PutRuntimeSpecRequest)) (serverID string, stored bool, err error) {
	w, err := s.resolveBenchmarkSpecWrite(ctx, specID, expect, apply)
	if err != nil {
		return "", false, err
	}
	_, stored, err = s.writeRuntimeSpec(ctx, w.mapping, w.app, w.req, w.specID)
	return w.server.ID, stored, err
}

// record files one spec's result in the outcome.
//
// Gone is only what proves the value can no longer act: ErrRuntimeSpecNotFound,
// which a deleted mapping, application or server also produces through the
// foreign-key cascade, and ErrRuntimeSpecNotServerAgent, a retyped
// application, whose specs leave the runtime-config document. A retyped spec
// keeps its stored value, which acts again if the application is retyped
// back, so a Warn names it. ErrMappingNotFound is Failed, not Gone:
// resolveMappingChain returns it for every error of its reads, a store error
// included, and a transient error read as gone would leave an override in
// place that no report names.
func (o *BenchmarkSpecsOutcome) record(specID string, err error) {
	if err == nil {
		o.Written = append(o.Written, specID)
		return
	}
	switch {
	case errors.Is(err, ErrRuntimeSpecNotFound):
		o.Gone = append(o.Gone, specID)
	case errors.Is(err, ErrRuntimeSpecNotServerAgent):
		slog.Warn("benchmark: a launch spec's application is no longer server_agent; its stored pinned and admin_state stay on the row and act again if the application is retyped back", "spec_id", specID)
		o.Gone = append(o.Gone, specID)
	case errors.Is(err, ErrRuntimeSpecPinnedConflict), errors.Is(err, ErrRuntimeSpecAdminStateConflict):
		o.Conflict = append(o.Conflict, specID)
	default:
		o.Failed = append(o.Failed, specID)
	}
	if o.Errs == nil {
		o.Errs = map[string]error{}
	}
	o.Errs[specID] = err
}

// firstFailure is a batch's error return: the first Failed spec's error, nil
// when none failed.
func (o BenchmarkSpecsOutcome) firstFailure() error {
	if len(o.Failed) == 0 {
		return nil
	}
	return o.Errs[o.Failed[0]]
}

// benchmarkSpecWrite is one benchmark write, resolved: the spec's mapping
// chain, its id, and the full-document request with one field replaced.
type benchmarkSpecWrite struct {
	mapping routing.ModelMapping
	app     routing.Application
	server  routing.AIServer
	specID  string
	req     PutRuntimeSpecRequest
}

// resolveBenchmarkSpecWrite is the read half every benchmark writer shares,
// for one spec id: the read by id (ErrRuntimeSpecNotFound when there is no
// such row), expect against the stored row (a mismatch writes nothing), the
// mapping chain and the GPU rows, and the SPREAD of the whole document with
// apply replacing one field. Never an assembled field list -- see
// putRequestFromDTO.
func (s *Service) resolveBenchmarkSpecWrite(ctx context.Context, specID string, expect func(routing.RuntimeSpec) error, apply func(*PutRuntimeSpecRequest)) (benchmarkSpecWrite, error) {
	specID = strings.TrimSpace(specID)
	if specID == "" || s.routes == nil {
		return benchmarkSpecWrite{}, ErrRuntimeSpecNotFound
	}
	spec, ok, err := s.routes.RuntimeSpecByID(ctx, specID)
	if err != nil {
		return benchmarkSpecWrite{}, err
	}
	if !ok {
		return benchmarkSpecWrite{}, ErrRuntimeSpecNotFound
	}
	if err := expect(spec); err != nil {
		return benchmarkSpecWrite{}, err
	}
	mapping, app, server, err := s.resolveMappingChain(ctx, spec.MappingID)
	if err != nil {
		return benchmarkSpecWrite{}, err
	}
	gpus, err := s.routes.RuntimeSpecGPUs(ctx, spec.ID)
	if err != nil {
		return benchmarkSpecWrite{}, err
	}
	dto, err := runtimeSpecDTO(spec, gpus, app)
	if err != nil {
		return benchmarkSpecWrite{}, err
	}
	req := putRequestFromDTO(dto)
	apply(&req)
	return benchmarkSpecWrite{mapping: mapping, app: app, server: server, specID: spec.ID, req: req}, nil
}

// expectAdminState is the admin-state writers' compare-and-set: the stored
// admin_state must equal expected, both trimmed.
func expectAdminState(expected string) func(routing.RuntimeSpec) error {
	expected = strings.TrimSpace(expected)
	return func(spec routing.RuntimeSpec) error {
		if strings.TrimSpace(spec.AdminState) != expected {
			return ErrRuntimeSpecAdminStateConflict
		}
		return nil
	}
}

// expectPinned is SetBenchmarkRuntimeSpecsPinned's compare-and-set: the
// stored pinned must equal expected.
func expectPinned(expected bool) func(routing.RuntimeSpec) error {
	return func(spec routing.RuntimeSpec) error {
		if spec.Pinned != expected {
			return ErrRuntimeSpecPinnedConflict
		}
		return nil
	}
}

// withAdminState replaces admin_state in a spread request.
func withAdminState(adminState string) func(*PutRuntimeSpecRequest) {
	return func(req *PutRuntimeSpecRequest) { req.AdminState = adminState }
}

// withPinned replaces pinned in a spread request.
func withPinned(pinned bool) func(*PutRuntimeSpecRequest) {
	return func(req *PutRuntimeSpecRequest) { req.Pinned = pinned }
}

// resolveMappingChain is authorizeMapping with the authorization removed: it
// resolves a mapping to its owning application and AI server, which is what
// the runtime-spec write needs for its application-type gate and for the
// server id it notifies. Every error of its three reads collapses to
// ErrMappingNotFound, a store error included, not only an absent row. That
// matches authorizeMapping's no-existence-leak posture, so a caller cannot
// learn which link of the chain was missing, and it is why the batched
// writers count ErrMappingNotFound as Failed rather than Gone: it does not
// prove that the spec is gone.
//
// It carries no principal on purpose and must only be reached from a method
// whose own doc block states who authorized it (today:
// resolveBenchmarkSpecWrite, for SetBenchmarkRuntimeSpecAdminState,
// SetBenchmarkRuntimeSpecsPinned and SetBenchmarkRuntimeSpecsAdminState).
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
