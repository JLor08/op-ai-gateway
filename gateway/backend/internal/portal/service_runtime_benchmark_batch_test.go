// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// replacementSpecID is the id benchmarkWriterRoutes.replace stores the
// re-created spec under.
const replacementSpecID = "rspec_replacement"

// benchmarkWriterRoutes is the MemoryStore under a benchmark-writer test, with
// the faults a test arms one field at a time. Unarmed, it is the MemoryStore.
type benchmarkWriterRoutes struct {
	*routing.MemoryStore
	// mappingByIDErr fails every MappingByID: the store error that
	// resolveMappingChain reports as ErrMappingNotFound.
	mappingByIDErr error
	// upsertErr fails UpsertRuntimeSpec for a spec id, before anything is
	// stored.
	upsertErr map[string]error
	// setGPUsErr fails SetRuntimeSpecGPUs for a spec id, after
	// UpsertRuntimeSpec has already stored the row.
	setGPUsErr map[string]error
	// readBackErr fails the RuntimeSpecGPUs read that follows a successful
	// SetRuntimeSpecGPUs for a spec id: the write has stored, its read-back
	// fails. readBackDue marks the read that is due to fail.
	readBackErr map[string]error
	readBackDue map[string]bool
	// vanish deletes this spec right before the next read by its mapping: a
	// DELETE between a benchmark writer's read by id and the write's re-read.
	vanish string
	// replace does the same and stores the spec again under
	// replacementSpecID: an operator who deleted the spec and created it anew.
	replace string
}

func (r *benchmarkWriterRoutes) MappingByID(ctx context.Context, id string) (routing.ModelMapping, error) {
	if r.mappingByIDErr != nil {
		return routing.ModelMapping{}, r.mappingByIDErr
	}
	return r.MemoryStore.MappingByID(ctx, id)
}

func (r *benchmarkWriterRoutes) UpsertRuntimeSpec(ctx context.Context, spec routing.RuntimeSpec) error {
	if err := r.upsertErr[spec.ID]; err != nil {
		return err
	}
	return r.MemoryStore.UpsertRuntimeSpec(ctx, spec)
}

func (r *benchmarkWriterRoutes) SetRuntimeSpecGPUs(ctx context.Context, specID string, gpus []routing.RuntimeSpecGPU) error {
	if err := r.setGPUsErr[specID]; err != nil {
		return err
	}
	if err := r.MemoryStore.SetRuntimeSpecGPUs(ctx, specID, gpus); err != nil {
		return err
	}
	if r.readBackErr[specID] != nil {
		if r.readBackDue == nil {
			r.readBackDue = map[string]bool{}
		}
		r.readBackDue[specID] = true
	}
	return nil
}

func (r *benchmarkWriterRoutes) RuntimeSpecGPUs(ctx context.Context, specID string) ([]routing.RuntimeSpecGPU, error) {
	if r.readBackDue[specID] {
		delete(r.readBackDue, specID)
		return nil, r.readBackErr[specID]
	}
	return r.MemoryStore.RuntimeSpecGPUs(ctx, specID)
}

func (r *benchmarkWriterRoutes) RuntimeSpecByMapping(ctx context.Context, mappingID string) (routing.RuntimeSpec, bool, error) {
	spec, ok, err := r.MemoryStore.RuntimeSpecByMapping(ctx, mappingID)
	if err != nil || !ok || (spec.ID != r.vanish && spec.ID != r.replace) {
		return spec, ok, err
	}
	if err := r.MemoryStore.DeleteRuntimeSpec(ctx, spec.ID); err != nil {
		return routing.RuntimeSpec{}, false, err
	}
	if spec.ID == r.vanish {
		r.vanish = ""
		return r.MemoryStore.RuntimeSpecByMapping(ctx, mappingID)
	}
	r.replace = ""
	spec.ID = replacementSpecID
	if err := r.MemoryStore.UpsertRuntimeSpec(ctx, spec); err != nil {
		return routing.RuntimeSpec{}, false, err
	}
	return r.MemoryStore.RuntimeSpecByMapping(ctx, mappingID)
}

// benchmarkWriterFixture is a portal Service over a benchmarkWriterRoutes,
// with one server whose server_agent application has three pinned launch
// specs, each on its own mapping.
type benchmarkWriterFixture struct {
	svc      *Service
	routes   *benchmarkWriterRoutes
	calls    func() []string
	now      time.Time
	serverID string
	app      routing.Application
	specs    []RuntimeSpecDTO
}

func newBenchmarkWriterFixture(t *testing.T) *benchmarkWriterFixture {
	t.Helper()
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	routes := &benchmarkWriterRoutes{MemoryStore: routing.NewMemoryStore()}
	svc := newServerTestServiceWithRoutes(t, now, routes)
	f := &benchmarkWriterFixture{svc: svc, routes: routes, calls: recordRuntimeChanged(svc), now: now}
	f.serverID = createTestServer(t, svc, "S", "s.example.test").ID
	f.app, f.specs = f.addSpecs(t, f.serverID, "qwen", "llama", "gemma")
	return f
}

// addSpecs seeds a server_agent application on serverID and one pinned
// launch spec per model, each on its own mapping, and returns the application
// and the specs as stored.
func (f *benchmarkWriterFixture) addSpecs(t *testing.T, serverID string, models ...string) (routing.Application, []RuntimeSpecDTO) {
	t.Helper()
	ctx := context.Background()
	app := seedServerAgentApplication(t, f.routes.MemoryStore, serverID, f.now)
	specs := make([]RuntimeSpecDTO, 0, len(models))
	for i, model := range models {
		mapping, err := f.svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: model, AppModelName: model})
		if err != nil {
			t.Fatalf("CreateMapping(%s): %v", model, err)
		}
		spec, err := f.svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
			Enabled:            true,
			Binary:             "/usr/local/bin/llama-server",
			Args:               []string{"--model", "/models/" + model + ".gguf"},
			ListenPort:         18080 + i,
			IdleTimeoutSeconds: 900,
			Pinned:             true,
			GPUs:               []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 8000}},
			APIFlavors:         []string{routing.APIFlavorOpenAI},
			ResponsesMode:      string(routing.EndpointModeDisabled),
		})
		if err != nil {
			t.Fatalf("PutRuntimeSpec(%s): %v", model, err)
		}
		specs = append(specs, spec)
	}
	return app, specs
}

// get reads spec's mapping's launch spec through the portal.
func (f *benchmarkWriterFixture) get(t *testing.T, spec RuntimeSpecDTO) RuntimeSpecDTO {
	t.Helper()
	got, err := f.svc.GetRuntimeSpec(context.Background(), ownerToken(), spec.MappingID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec(%s): %v", spec.MappingID, err)
	}
	return got
}

// wantCalls fails unless the runtime-changed notifications since the first
// before calls are exactly want, in order.
func (f *benchmarkWriterFixture) wantCalls(t *testing.T, before int, want ...string) {
	t.Helper()
	if got := f.calls()[before:]; !slices.Equal(got, want) {
		t.Fatalf("runtime-changed calls = %v, want %v", got, want)
	}
}

// wantNoSpec fails when spec's mapping holds a launch spec.
func (f *benchmarkWriterFixture) wantNoSpec(t *testing.T, spec RuntimeSpecDTO) {
	t.Helper()
	got, ok, err := f.routes.MemoryStore.RuntimeSpecByMapping(context.Background(), spec.MappingID)
	if err != nil {
		t.Fatalf("RuntimeSpecByMapping: %v", err)
	}
	if ok {
		t.Fatalf("the write created spec %q for the mapping (pinned %v, admin_state %q), want no spec", got.ID, got.Pinned, got.AdminState)
	}
}

// wantReplacementUntouched fails unless spec's mapping holds the spec that
// benchmarkWriterRoutes.replace created, still pinned and without an override.
func (f *benchmarkWriterFixture) wantReplacementUntouched(t *testing.T, spec RuntimeSpecDTO) {
	t.Helper()
	got, ok, err := f.routes.MemoryStore.RuntimeSpecByMapping(context.Background(), spec.MappingID)
	if err != nil || !ok {
		t.Fatalf("RuntimeSpecByMapping = ok %v, err %v; want the replacement spec", ok, err)
	}
	if got.ID != replacementSpecID || !got.Pinned || got.AdminState != "" {
		t.Fatalf("the mapping's spec = %q (pinned %v, admin_state %q), want %q untouched (pinned true, admin_state \"\")",
			got.ID, got.Pinned, got.AdminState, replacementSpecID)
	}
}

// TestPutRuntimeSpecNotifiesWhenTheReadBackFails pins that a failed read-back
// does not cancel a stored write's notification. Once the row and its GPU
// rows are stored, the runtime-config document has changed, so a failure of
// the read-back after them still owes the agent its notification, while the
// PUT reports the error. TestPutRuntimeSpecNotifiesOnceTheRowIsStored pins
// where that obligation starts.
func TestPutRuntimeSpecNotifiesWhenTheReadBackFails(t *testing.T) {
	ctx := context.Background()
	f := newBenchmarkWriterFixture(t)
	spec := f.specs[0]
	readBack := errors.New("read-back failed")
	f.routes.readBackErr = map[string]error{spec.ID: readBack}
	before := len(f.calls())

	req := putRequestFromDTO(spec)
	req.Pinned = false
	if _, err := f.svc.PutRuntimeSpec(ctx, ownerToken(), spec.MappingID, req); !errors.Is(err, readBack) {
		t.Fatalf("PutRuntimeSpec err = %v, want the read-back's error", err)
	}
	f.wantCalls(t, before, f.serverID)
	if f.get(t, spec).Pinned {
		t.Fatal("the spec is still pinned: the write did not store")
	}
}

// TestPutRuntimeSpecNotifiesOnceTheRowIsStored pins where a runtime-spec
// write starts to owe its notification: at the upsert. A write whose
// UpsertRuntimeSpec fails has stored nothing, reports the store's error and
// notifies nothing. A write whose SetRuntimeSpecGPUs fails after the upsert
// reports that error too, but the upserted row is already part of the
// runtime-config document, so the agent is notified of it.
func TestPutRuntimeSpecNotifiesOnceTheRowIsStored(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		arm    func(r *benchmarkWriterRoutes, specID string, err error)
		stored bool
	}{
		{"the upsert fails: nothing is stored or notified", func(r *benchmarkWriterRoutes, specID string, err error) {
			r.upsertErr = map[string]error{specID: err}
		}, false},
		{"the GPU-row write fails: the stored row is notified", func(r *benchmarkWriterRoutes, specID string, err error) {
			r.setGPUsErr = map[string]error{specID: err}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBenchmarkWriterFixture(t)
			spec := f.specs[0]
			storeErr := errors.New("store write failed")
			tc.arm(f.routes, spec.ID, storeErr)
			before := len(f.calls())

			req := putRequestFromDTO(spec)
			req.Pinned = false
			if _, err := f.svc.PutRuntimeSpec(ctx, ownerToken(), spec.MappingID, req); !errors.Is(err, storeErr) {
				t.Fatalf("PutRuntimeSpec err = %v, want the store's error", err)
			}
			f.routes.upsertErr, f.routes.setGPUsErr = nil, nil
			if stored := !f.get(t, spec).Pinned; stored != tc.stored {
				t.Fatalf("after the failed write the row is stored = %v (pinned %v), want %v", stored, !stored, tc.stored)
			}
			if tc.stored {
				f.wantCalls(t, before, f.serverID)
			} else {
				f.wantCalls(t, before)
			}
		})
	}
}

// TestBenchmarkWritersRefuseAGoneOrReplacedSpec pins the expectSpecID guard's
// scope. A benchmark writer reads the spec by id and then re-reads it by
// mapping, and a spec deleted, or deleted and created anew, between those two
// reads is ErrRuntimeSpecNotFound: nothing is stored and nothing is notified,
// so the upsert cannot create a new spec under a new id -- a pinned one, for
// a re-pin, which the agent would start right after the operator deleted it.
// A DELETE after the re-read and before the upsert is the guard's accepted
// gap (priorRuntimeSpec) and is not covered here. PutRuntimeSpec still
// creates a spec on a mapping's first write and updates an existing one.
func TestBenchmarkWritersRefuseAGoneOrReplacedSpec(t *testing.T) {
	ctx := context.Background()

	t.Run("SetBenchmarkRuntimeSpecAdminState, the spec deleted", func(t *testing.T) {
		f := newBenchmarkWriterFixture(t)
		spec := f.specs[0]
		f.routes.vanish = spec.ID
		before := len(f.calls())
		if _, err := f.svc.SetBenchmarkRuntimeSpecAdminState(ctx, spec.ID, "", "force_stopped"); !errors.Is(err, ErrRuntimeSpecNotFound) {
			t.Errorf("err = %v, want ErrRuntimeSpecNotFound", err)
		}
		f.wantNoSpec(t, spec)
		f.wantCalls(t, before)
	})

	t.Run("SetBenchmarkRuntimeSpecAdminState, the spec deleted and created anew", func(t *testing.T) {
		f := newBenchmarkWriterFixture(t)
		spec := f.specs[0]
		f.routes.replace = spec.ID
		before := len(f.calls())
		if _, err := f.svc.SetBenchmarkRuntimeSpecAdminState(ctx, spec.ID, "", "force_stopped"); !errors.Is(err, ErrRuntimeSpecNotFound) {
			t.Errorf("err = %v, want ErrRuntimeSpecNotFound", err)
		}
		f.wantReplacementUntouched(t, spec)
		f.wantCalls(t, before)
	})

	for _, w := range batchWriters() {
		t.Run(w.name+", the spec deleted", func(t *testing.T) {
			f := newBenchmarkWriterFixture(t)
			spec := f.specs[0]
			f.routes.vanish = spec.ID
			before := len(f.calls())
			out, err := w.forward(ctx, f.svc, specIDs(spec))
			if err != nil {
				t.Errorf("err = %v, want nil (gone is not a failure)", err)
			}
			wantOutcome(t, out, BenchmarkSpecsOutcome{Gone: specIDs(spec)})
			wantErrs(t, out, ErrRuntimeSpecNotFound, spec.ID)
			f.wantNoSpec(t, spec)
			f.wantCalls(t, before)
		})

		t.Run(w.name+", the spec deleted and created anew", func(t *testing.T) {
			f := newBenchmarkWriterFixture(t)
			spec := f.specs[0]
			f.routes.replace = spec.ID
			before := len(f.calls())
			out, err := w.forward(ctx, f.svc, specIDs(spec))
			if err != nil {
				t.Errorf("err = %v, want nil (gone is not a failure)", err)
			}
			wantOutcome(t, out, BenchmarkSpecsOutcome{Gone: specIDs(spec)})
			wantErrs(t, out, ErrRuntimeSpecNotFound, spec.ID)
			f.wantReplacementUntouched(t, spec)
			f.wantCalls(t, before)
		})
	}

	t.Run("PutRuntimeSpec still creates a spec and updates it", func(t *testing.T) {
		f := newBenchmarkWriterFixture(t)
		mapping, err := f.svc.CreateMapping(ctx, ownerToken(), f.app.ID, CreateMappingRequest{GatewayModelName: "phi", AppModelName: "phi"})
		if err != nil {
			t.Fatalf("CreateMapping: %v", err)
		}
		before := len(f.calls())
		created, err := f.svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{Enabled: true, Binary: "/usr/local/bin/llama-server"})
		if err != nil {
			t.Fatalf("PutRuntimeSpec (create): %v", err)
		}
		if created.ID == "" || created.MappingID != mapping.ID {
			t.Fatalf("created spec = id %q, mapping %q; want a new spec for %q", created.ID, created.MappingID, mapping.ID)
		}
		req := putRequestFromDTO(created)
		req.Pinned = true
		updated, err := f.svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, req)
		if err != nil {
			t.Fatalf("PutRuntimeSpec (update): %v", err)
		}
		if updated.ID != created.ID || !updated.Pinned {
			t.Fatalf("updated spec = id %q, pinned %v; want %q pinned", updated.ID, updated.Pinned, created.ID)
		}
		f.wantCalls(t, before, f.serverID, f.serverID)
	})
}

// batchWriter is one batched benchmark writer under test. forward passes the
// fixture's specs' compare-and-set (pinned true, admin_state "") and writes
// the other value; backward is its inverse. takeOver is an operator's write
// that fails forward's expectation, takeBack one that fails backward's, and
// after is a spec's DTO once forward has written it.
type batchWriter struct {
	name     string
	forward  func(ctx context.Context, svc *Service, ids []string) (BenchmarkSpecsOutcome, error)
	backward func(ctx context.Context, svc *Service, ids []string) (BenchmarkSpecsOutcome, error)
	conflict error
	takeOver func(req *PutRuntimeSpecRequest)
	takeBack func(req *PutRuntimeSpecRequest)
	after    func(dto RuntimeSpecDTO) RuntimeSpecDTO
}

func batchWriters() []batchWriter {
	return []batchWriter{
		{
			name: "SetBenchmarkRuntimeSpecsPinned",
			forward: func(ctx context.Context, svc *Service, ids []string) (BenchmarkSpecsOutcome, error) {
				return svc.SetBenchmarkRuntimeSpecsPinned(ctx, ids, true, false)
			},
			backward: func(ctx context.Context, svc *Service, ids []string) (BenchmarkSpecsOutcome, error) {
				return svc.SetBenchmarkRuntimeSpecsPinned(ctx, ids, false, true)
			},
			conflict: ErrRuntimeSpecPinnedConflict,
			takeOver: func(req *PutRuntimeSpecRequest) { req.Pinned = false },
			takeBack: func(req *PutRuntimeSpecRequest) { req.Pinned = true },
			after:    func(dto RuntimeSpecDTO) RuntimeSpecDTO { dto.Pinned = false; return dto },
		},
		{
			name: "SetBenchmarkRuntimeSpecsAdminState",
			forward: func(ctx context.Context, svc *Service, ids []string) (BenchmarkSpecsOutcome, error) {
				return svc.SetBenchmarkRuntimeSpecsAdminState(ctx, ids, "", "force_stopped")
			},
			backward: func(ctx context.Context, svc *Service, ids []string) (BenchmarkSpecsOutcome, error) {
				return svc.SetBenchmarkRuntimeSpecsAdminState(ctx, ids, "force_stopped", "")
			},
			conflict: ErrRuntimeSpecAdminStateConflict,
			takeOver: func(req *PutRuntimeSpecRequest) { req.AdminState = "force_running" },
			takeBack: func(req *PutRuntimeSpecRequest) { req.AdminState = "force_running" },
			after:    func(dto RuntimeSpecDTO) RuntimeSpecDTO { dto.AdminState = "force_stopped"; return dto },
		},
	}
}

// specIDs returns the specs' ids, in order.
func specIDs(specs ...RuntimeSpecDTO) []string {
	ids := make([]string, 0, len(specs))
	for _, spec := range specs {
		ids = append(ids, spec.ID)
	}
	return ids
}

// operatorPut writes spec through the principal-carrying PutRuntimeSpec with
// change applied to the spread of its current document.
func (f *benchmarkWriterFixture) operatorPut(t *testing.T, spec RuntimeSpecDTO, change func(req *PutRuntimeSpecRequest)) {
	t.Helper()
	req := putRequestFromDTO(f.get(t, spec))
	change(&req)
	if _, err := f.svc.PutRuntimeSpec(context.Background(), ownerToken(), spec.MappingID, req); err != nil {
		t.Fatalf("operator PutRuntimeSpec(%s): %v", spec.ID, err)
	}
}

// etag is the ETag of the fixture server's runtime-config document.
func (f *benchmarkWriterFixture) etag(t *testing.T) string {
	t.Helper()
	doc, err := f.svc.AgentRuntimeConfig(context.Background(), f.serverID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	return doc.ETag
}

// wantSpecs fails unless each spec reads back as want(spec).
func (f *benchmarkWriterFixture) wantSpecs(t *testing.T, want func(RuntimeSpecDTO) RuntimeSpecDTO, specs ...RuntimeSpecDTO) {
	t.Helper()
	for _, spec := range specs {
		if got, w := f.get(t, spec), want(spec); !reflect.DeepEqual(got, w) {
			t.Fatalf("spec %s:\n got %#v\nwant %#v", spec.ID, got, w)
		}
	}
}

// unchanged is the identity: a spec that reads back as it was stored.
func unchanged(dto RuntimeSpecDTO) RuntimeSpecDTO { return dto }

// wantOutcome fails unless out holds exactly want's four id lists and its
// Notified, and Errs holds one error for each id outside Written and is nil
// when there is none.
func wantOutcome(t *testing.T, out, want BenchmarkSpecsOutcome) {
	t.Helper()
	lists := []struct {
		name      string
		got, want []string
	}{
		{"Written", out.Written, want.Written},
		{"Gone", out.Gone, want.Gone},
		{"Conflict", out.Conflict, want.Conflict},
		{"Failed", out.Failed, want.Failed},
	}
	for _, l := range lists {
		if !slices.Equal(l.got, l.want) {
			t.Errorf("%s = %v, want %v", l.name, l.got, l.want)
		}
	}
	if out.Notified != want.Notified {
		t.Errorf("Notified = %v, want %v", out.Notified, want.Notified)
	}
	n := len(want.Gone) + len(want.Conflict) + len(want.Failed)
	if len(out.Errs) != n {
		t.Errorf("Errs = %v, want one error for each of the %d ids outside Written", out.Errs, n)
	}
	if n == 0 && out.Errs != nil {
		t.Errorf("Errs = %#v, want nil when every spec was written", out.Errs)
	}
	if t.Failed() {
		t.FailNow()
	}
}

// wantErrs fails unless Errs names each id with an error that is target.
func wantErrs(t *testing.T, out BenchmarkSpecsOutcome, target error, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := out.Errs[id]; !errors.Is(err, target) {
			t.Fatalf("Errs[%s] = %v, want %v", id, err, target)
		}
	}
}

// TestBenchmarkBatchedWriters pins the two batched benchmark writers. Each
// writes one field per spec as a compare-and-set, through the full-document
// write, and notifies once per batch for every server a write stored to, so
// the agent gets one document with the whole batch in it instead of a burst
// of partial ones. A spec that is gone (deleted, by cascade too, or on a
// retyped application) is Gone; a stored value other than the expected one is
// a Conflict and writes nothing; every other error is Failed, a mapping-chain
// store error included. The error return is the first Failed spec's error.
func TestBenchmarkBatchedWriters(t *testing.T) {
	ctx := context.Background()
	for _, w := range batchWriters() {
		t.Run(w.name+": one write per spec, one notification, no other field moves", func(t *testing.T) {
			f := newBenchmarkWriterFixture(t)
			etag := f.etag(t)
			before := len(f.calls())
			out, err := w.forward(ctx, f.svc, specIDs(f.specs...))
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			wantOutcome(t, out, BenchmarkSpecsOutcome{Written: specIDs(f.specs...), Notified: true})
			f.wantCalls(t, before, f.serverID)
			f.wantSpecs(t, w.after, f.specs...)
			if f.etag(t) == etag {
				t.Fatal("the runtime-config ETag did not change")
			}
		})

		t.Run(w.name+": the inverse batch restores the document and its ETag", func(t *testing.T) {
			f := newBenchmarkWriterFixture(t)
			etag := f.etag(t)
			if _, err := w.forward(ctx, f.svc, specIDs(f.specs...)); err != nil {
				t.Fatalf("forward: %v", err)
			}
			before := len(f.calls())
			out, err := w.backward(ctx, f.svc, specIDs(f.specs...))
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			wantOutcome(t, out, BenchmarkSpecsOutcome{Written: specIDs(f.specs...), Notified: true})
			f.wantCalls(t, before, f.serverID)
			f.wantSpecs(t, unchanged, f.specs...)
			if got := f.etag(t); got != etag {
				t.Fatalf("ETag after the inverse batch = %s, want the original %s", got, etag)
			}
		})

		t.Run(w.name+": a batch that stores nothing does not notify", func(t *testing.T) {
			f := newBenchmarkWriterFixture(t)
			before := len(f.calls())
			out, err := w.backward(ctx, f.svc, specIDs(f.specs...))
			if err != nil {
				t.Fatalf("err = %v, want nil (a conflict is not a failure)", err)
			}
			wantOutcome(t, out, BenchmarkSpecsOutcome{Conflict: specIDs(f.specs...)})
			wantErrs(t, out, w.conflict, specIDs(f.specs...)...)
			out, err = w.forward(ctx, f.svc, nil)
			if err != nil {
				t.Fatalf("empty batch err = %v, want nil", err)
			}
			wantOutcome(t, out, BenchmarkSpecsOutcome{})
			f.wantCalls(t, before)
			f.wantSpecs(t, unchanged, f.specs...)
		})

		t.Run(w.name+": conflict, gone and retyped", func(t *testing.T) {
			f := newBenchmarkWriterFixture(t)
			written, conflict, cascaded := f.specs[0], f.specs[1], f.specs[2]
			otherID := createTestServer(t, f.svc, "T", "t.example.test").ID
			otherApp, other := f.addSpecs(t, otherID, "mistral")
			retyped := other[0]
			otherApp.Type = routing.ProviderVLLM
			if err := f.routes.UpdateApplication(ctx, otherApp); err != nil {
				t.Fatalf("retype the application: %v", err)
			}
			f.operatorPut(t, conflict, w.takeOver)
			if err := f.routes.DeleteMapping(ctx, cascaded.MappingID); err != nil {
				t.Fatalf("DeleteMapping: %v", err)
			}
			before := len(f.calls())
			ids := []string{written.ID, conflict.ID, "rspec_missing", cascaded.ID, retyped.ID}

			var out BenchmarkSpecsOutcome
			var err error
			logged := captureSlog(t, func() { out, err = w.forward(ctx, f.svc, ids) })
			if err != nil {
				t.Fatalf("err = %v, want nil (gone and conflict are not failures)", err)
			}
			wantOutcome(t, out, BenchmarkSpecsOutcome{
				Written:  []string{written.ID},
				Conflict: []string{conflict.ID},
				Gone:     []string{"rspec_missing", cascaded.ID, retyped.ID},
				Notified: true,
			})
			wantErrs(t, out, w.conflict, conflict.ID)
			wantErrs(t, out, ErrRuntimeSpecNotFound, "rspec_missing", cascaded.ID)
			wantErrs(t, out, ErrRuntimeSpecNotServerAgent, retyped.ID)
			f.wantCalls(t, before, f.serverID)
			f.wantSpecs(t, w.after, written)

			const warn = `level=WARN msg="benchmark: a launch spec's application is no longer server_agent; its stored pinned and admin_state stay on the row and act again if the application is retyped back" spec_id=`
			if strings.Count(logged, warn) != 1 || !strings.Contains(logged, warn+retyped.ID) {
				t.Fatalf("log = %q, want exactly one Warn naming %s", logged, retyped.ID)
			}
			stored, ok, err := f.routes.RuntimeSpecByID(ctx, retyped.ID)
			if err != nil || !ok || !stored.Pinned || stored.AdminState != "" {
				t.Fatalf("retyped spec = %+v (ok %v, err %v), want it stored unchanged", stored, ok, err)
			}
		})

		t.Run(w.name+": a mapping-chain store error is Failed, not Gone", func(t *testing.T) {
			f := newBenchmarkWriterFixture(t)
			f.routes.mappingByIDErr = errors.New("store unavailable")
			before := len(f.calls())
			ids := specIDs(f.specs[0], f.specs[1])
			out, err := w.forward(ctx, f.svc, ids)
			if !errors.Is(err, ErrMappingNotFound) {
				t.Fatalf("err = %v, want ErrMappingNotFound", err)
			}
			wantOutcome(t, out, BenchmarkSpecsOutcome{Failed: ids})
			wantErrs(t, out, ErrMappingNotFound, ids...)
			f.wantCalls(t, before)
			f.routes.mappingByIDErr = nil
			f.wantSpecs(t, unchanged, f.specs...)
		})

		t.Run(w.name+": a failed GPU write is Failed with its row already stored, and the first failure is the error", func(t *testing.T) {
			f := newBenchmarkWriterFixture(t)
			first, second := errors.New("gpu write 1 failed"), errors.New("gpu write 2 failed")
			f.routes.setGPUsErr = map[string]error{f.specs[0].ID: first, f.specs[2].ID: second}
			before := len(f.calls())
			out, err := w.forward(ctx, f.svc, specIDs(f.specs...))
			if !errors.Is(err, first) || errors.Is(err, second) {
				t.Fatalf("err = %v, want the first failed spec's error %q", err, first)
			}
			wantOutcome(t, out, BenchmarkSpecsOutcome{Written: specIDs(f.specs[1]), Failed: specIDs(f.specs[0], f.specs[2]), Notified: true})
			wantErrs(t, out, first, f.specs[0].ID)
			wantErrs(t, out, second, f.specs[2].ID)
			f.wantCalls(t, before, f.serverID)
			f.routes.setGPUsErr = nil
			f.wantSpecs(t, w.after, f.specs...)
		})

		t.Run(w.name+": a write whose row was stored although its GPU rows failed is Failed and notified", func(t *testing.T) {
			f := newBenchmarkWriterFixture(t)
			gpuWrite := errors.New("gpu write failed")
			f.routes.setGPUsErr = map[string]error{f.specs[0].ID: gpuWrite}
			before := len(f.calls())
			out, err := w.forward(ctx, f.svc, specIDs(f.specs[0]))
			if !errors.Is(err, gpuWrite) {
				t.Fatalf("err = %v, want the GPU write's error", err)
			}
			wantOutcome(t, out, BenchmarkSpecsOutcome{Failed: specIDs(f.specs[0]), Notified: true})
			wantErrs(t, out, gpuWrite, f.specs[0].ID)
			f.wantCalls(t, before, f.serverID)
			f.routes.setGPUsErr = nil
			f.wantSpecs(t, w.after, f.specs[0])
		})

		t.Run(w.name+": a failed read-back is Failed, and its stored write is notified", func(t *testing.T) {
			f := newBenchmarkWriterFixture(t)
			readBack := errors.New("read-back failed")
			f.routes.readBackErr = map[string]error{f.specs[0].ID: readBack}
			before := len(f.calls())
			out, err := w.forward(ctx, f.svc, specIDs(f.specs[0]))
			if !errors.Is(err, readBack) {
				t.Fatalf("err = %v, want the read-back's error", err)
			}
			wantOutcome(t, out, BenchmarkSpecsOutcome{Failed: specIDs(f.specs[0]), Notified: true})
			f.wantCalls(t, before, f.serverID)
			f.wantSpecs(t, w.after, f.specs[0])
		})

		t.Run(w.name+": a restore after an operator took the field back is a conflict", func(t *testing.T) {
			f := newBenchmarkWriterFixture(t)
			spec := f.specs[0]
			if _, err := w.forward(ctx, f.svc, specIDs(spec)); err != nil {
				t.Fatalf("forward: %v", err)
			}
			f.operatorPut(t, spec, w.takeBack)
			operators := f.get(t, spec)
			before := len(f.calls())
			out, err := w.backward(ctx, f.svc, specIDs(spec))
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			wantOutcome(t, out, BenchmarkSpecsOutcome{Conflict: specIDs(spec)})
			wantErrs(t, out, w.conflict, spec.ID)
			f.wantCalls(t, before)
			f.wantSpecs(t, func(RuntimeSpecDTO) RuntimeSpecDTO { return operators }, spec)
		})

		t.Run(w.name+": the run's own batch is not refused while a run holds the server", func(t *testing.T) {
			f := newBenchmarkWriterFixture(t)
			f.svc.SetBenchmarkReservationHook(func(id string) bool { return id == f.serverID })
			req := putRequestFromDTO(f.specs[0])
			w.takeOver(&req)
			if _, err := f.svc.PutRuntimeSpec(ctx, ownerToken(), f.specs[0].MappingID, req); !errors.Is(err, ErrRuntimeSpecServerBenchmarking) {
				t.Fatalf("operator PutRuntimeSpec err = %v, want ErrRuntimeSpecServerBenchmarking", err)
			}
			out, err := w.forward(ctx, f.svc, specIDs(f.specs...))
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			wantOutcome(t, out, BenchmarkSpecsOutcome{Written: specIDs(f.specs...), Notified: true})
		})

		t.Run(w.name+": a batch over two servers notifies each once", func(t *testing.T) {
			f := newBenchmarkWriterFixture(t)
			otherID := createTestServer(t, f.svc, "T", "t.example.test").ID
			_, other := f.addSpecs(t, otherID, "mistral", "phi")
			before := len(f.calls())
			ids := specIDs(f.specs[0], other[0], f.specs[1], other[1])
			out, err := w.forward(ctx, f.svc, ids)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			wantOutcome(t, out, BenchmarkSpecsOutcome{Written: ids, Notified: true})
			f.wantCalls(t, before, f.serverID, otherID)
		})
	}

	t.Run("the admin_state compare-and-set trims the expected value", func(t *testing.T) {
		f := newBenchmarkWriterFixture(t)
		ids := specIDs(f.specs...)
		if _, err := f.svc.SetBenchmarkRuntimeSpecsAdminState(ctx, ids, "", "force_stopped"); err != nil {
			t.Fatalf("stop: %v", err)
		}
		before := len(f.calls())
		out, err := f.svc.SetBenchmarkRuntimeSpecsAdminState(ctx, ids, " force_stopped ", "")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		wantOutcome(t, out, BenchmarkSpecsOutcome{Written: ids, Notified: true})
		f.wantCalls(t, before, f.serverID)
		f.wantSpecs(t, unchanged, f.specs...)
	})

	t.Run("a later admin_state write keeps pinned, and a later pinned write keeps admin_state", func(t *testing.T) {
		f := newBenchmarkWriterFixture(t)
		ids := specIDs(f.specs...)
		if _, err := f.svc.SetBenchmarkRuntimeSpecsPinned(ctx, ids, true, false); err != nil {
			t.Fatalf("unpin: %v", err)
		}
		if _, err := f.svc.SetBenchmarkRuntimeSpecsAdminState(ctx, ids, "", "force_stopped"); err != nil {
			t.Fatalf("stop: %v", err)
		}
		f.wantSpecs(t, func(dto RuntimeSpecDTO) RuntimeSpecDTO {
			dto.Pinned, dto.AdminState = false, "force_stopped"
			return dto
		}, f.specs...)
		if _, err := f.svc.SetBenchmarkRuntimeSpecsPinned(ctx, ids, false, true); err != nil {
			t.Fatalf("re-pin: %v", err)
		}
		f.wantSpecs(t, func(dto RuntimeSpecDTO) RuntimeSpecDTO {
			dto.AdminState = "force_stopped"
			return dto
		}, f.specs...)
	})
}
