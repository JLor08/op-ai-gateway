// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// errRefusalSpecRead is the read failure refusalSpecStore injects.
var errRefusalSpecRead = errors.New("spec read down")

// refusalAppToken is rf_agent's application token, the credential a target
// of one of its mappings carries when it has no spec in token mode set.
const refusalAppToken = "enc:app-token"

// refusalSpecStore fails the runtime-spec read of the mappings in fail,
// answers every other read from the embedded store, and counts the
// runtime-spec reads per mapping, so a test can assert that a starter read a
// spec exactly once, or not at all. onRead, when set, runs at the start of
// every runtime-spec read, outside the lock, so a test can act at exactly the
// point where a starter reads.
type refusalSpecStore struct {
	routing.Store
	mu     sync.Mutex
	fail   map[string]bool
	reads  map[string]int
	onRead func(mappingID string)
}

func (f *refusalSpecStore) RuntimeSpecByMapping(ctx context.Context, mappingID string) (routing.RuntimeSpec, bool, error) {
	f.mu.Lock()
	f.reads[mappingID]++
	failing := f.fail[mappingID]
	onRead := f.onRead
	f.mu.Unlock()
	if onRead != nil {
		onRead(mappingID)
	}
	if failing {
		return routing.RuntimeSpec{}, false, errRefusalSpecRead
	}
	return f.Store.RuntimeSpecByMapping(ctx, mappingID)
}

func (f *refusalSpecStore) failRead(mappingID string) {
	f.mu.Lock()
	f.fail[mappingID] = true
	f.mu.Unlock()
}

func (f *refusalSpecStore) readCount(mappingID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[mappingID]
}

func (f *refusalSpecStore) setOnRead(onRead func(mappingID string)) {
	f.mu.Lock()
	f.onRead = onRead
	f.mu.Unlock()
}

// refusalFixture is newBenchmarkActiveFixture plus, on the owned server:
//   - a server_agent application rf_agent, whose application token is
//     refusalAppToken, with three mappings: rf_text (spec [openai]), rf_sd
//     (spec [openai_images]) and rf_unread (spec [openai], whose spec read
//     fails);
//   - a stable_diffusion_cpp application rf_sdapp ([openai_images]) with two
//     mappings, rf_sdext and rf_sdext2, and no spec.
//
// The fixture captures the default logger at its default level for the whole
// test (logs), so the WARNs rf_unread's failed read logs stay out of the test
// output and a test can assert them.
type refusalFixture struct {
	srv   *Server
	specs *refusalSpecStore
	prov  *coldLister
	logs  *logbuffer.Buffer
}

func newRefusalFixture(t *testing.T) *refusalFixture {
	t.Helper()
	logs := withCapturedSlogAtTheDefaultLevel(t)
	s := newBenchmarkActiveFixture(t)
	prov := newColdLister(nil)
	s.Provider = prov
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	must("CreateApplication(agent)", s.Routes.CreateApplication(ctx, routing.Application{ID: "rf_agent", ServerID: baOwnedServer, Type: routing.ProviderServerAgent, Port: 9100, Scheme: "http", TimeoutMS: 600000, APIToken: refusalAppToken, APITokenHeader: "Authorization", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must("CreateApplication(sd)", s.Routes.CreateApplication(ctx, routing.Application{ID: "rf_sdapp", ServerID: baOwnedServer, Type: routing.ProviderStableDiffusionCpp, Port: 7860, Scheme: "http", TimeoutMS: 600000, APIFlavors: []string{routing.APIFlavorOpenAIImages}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	mapping := func(appID, id string) {
		t.Helper()
		must("CreateMapping("+id+")", s.Routes.CreateMapping(ctx, routing.ModelMapping{ID: id, ApplicationID: appID, GatewayModelName: "gw-" + id, AppModelName: "up-" + id, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	}
	spec := func(mappingID string, flavors []string) {
		t.Helper()
		must("UpsertRuntimeSpec("+mappingID+")", s.Routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "rs_" + mappingID, MappingID: mappingID, Enabled: true, Binary: "/opt/bin/server", Args: "[]", Env: "{}", HealthPath: "/health", APIFlavors: flavors, CreatedAt: now, UpdatedAt: now}))
	}
	for _, id := range []string{"rf_text", "rf_sd", "rf_unread"} {
		mapping("rf_agent", id)
	}
	mapping("rf_sdapp", "rf_sdext")
	mapping("rf_sdapp", "rf_sdext2")
	spec("rf_text", []string{routing.APIFlavorOpenAI})
	spec("rf_sd", []string{routing.APIFlavorOpenAIImages})
	spec("rf_unread", []string{routing.APIFlavorOpenAI})

	specs := &refusalSpecStore{Store: s.Routes, fail: map[string]bool{"rf_unread": true}, reads: map[string]int{}}
	s.Routes = specs
	return &refusalFixture{srv: s, specs: specs, prov: prov, logs: logs}
}

// assertHistoryRows checks how many benchmark history rows each mapping has.
func (f *refusalFixture) assertHistoryRows(t *testing.T, want map[string]int) {
	t.Helper()
	for mappingID, n := range want {
		runs, err := f.srv.Routes.BenchmarkRunsByMapping(context.Background(), mappingID, 10)
		if err != nil {
			t.Fatalf("BenchmarkRunsByMapping(%s): %v", mappingID, err)
		}
		if len(runs) != n {
			t.Fatalf("history rows for %s = %d, want %d", mappingID, len(runs), n)
		}
	}
}

// assertOneSpecReadEach checks that each mapping's spec was read exactly once.
func (f *refusalFixture) assertOneSpecReadEach(t *testing.T, mappingIDs ...string) {
	t.Helper()
	for _, mappingID := range mappingIDs {
		if n := f.specs.readCount(mappingID); n != 1 {
			t.Fatalf("spec reads for %s = %d, want exactly 1", mappingID, n)
		}
	}
}

// warnsFor returns the WARN records carrying msg that name mappingID.
func (f *refusalFixture) warnsFor(msg, mappingID string) []logbuffer.Record {
	var out []logbuffer.Record
	for _, r := range f.logs.Snapshot() {
		if id, _ := r.Attrs["mapping_id"].(string); r.Level == "WARN" && r.Msg == msg && id == mappingID {
			out = append(out, r)
		}
	}
	return out
}

// post sends an owner-authorized POST and returns the status, the error
// code (empty on success) and the raw body.
func (f *refusalFixture) post(t *testing.T, path string) (status int, code string, body []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer "+baOwnerSecret)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	var errBody apierror.Body
	_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	return rec.Code, errBody.Error.Code, rec.Body.Bytes()
}

// setAdminState stores adminState on mappingID's spec.
func (f *refusalFixture) setAdminState(t *testing.T, mappingID, adminState string) {
	t.Helper()
	ctx := context.Background()
	spec, ok, err := f.specs.Store.RuntimeSpecByMapping(ctx, mappingID)
	if err != nil || !ok {
		t.Fatalf("RuntimeSpecByMapping(%s) = (%v, %v)", mappingID, ok, err)
	}
	spec.AdminState = adminState
	if err := f.specs.Store.UpsertRuntimeSpec(ctx, spec); err != nil {
		t.Fatalf("UpsertRuntimeSpec(%s): %v", mappingID, err)
	}
}

// sealSpecToken puts mappingID's spec in token mode set, with a sealed token
// of its own, and returns that token. A target built from the spec the
// starter read carries it; a target built without that spec falls back to
// refusalAppToken. The env carries the ${API_TOKEN} placeholder that mode
// requires, so the portal's spec writer (which a VRAM run's drain goes
// through) accepts the spec.
func (f *refusalFixture) sealSpecToken(t *testing.T, mappingID string) string {
	t.Helper()
	ctx := context.Background()
	spec := f.storedSpec(t, mappingID)
	spec.APITokenMode = string(routing.RuntimeAPITokenModeSet)
	spec.APIToken = "enc:spec-token-" + mappingID
	spec.Env = `{"LLAMA_API_KEY":"${API_TOKEN}"}`
	if err := f.specs.Store.UpsertRuntimeSpec(ctx, spec); err != nil {
		t.Fatalf("UpsertRuntimeSpec(%s): %v", mappingID, err)
	}
	return spec.APIToken
}

// assertStreamedWithToken fails unless the provider streamed upstreamModel at
// least once, and every stream of it carried token.
func (f *refusalFixture) assertStreamedWithToken(t *testing.T, upstreamModel, token string) {
	t.Helper()
	streams := 0
	for _, target := range f.prov.streamedTargetList() {
		if target.ProviderModel != upstreamModel {
			continue
		}
		streams++
		if target.APIToken != token {
			t.Fatalf("a stream of %s carried APIToken %q, want the spec's %q (the application's is %q): the target was not built from the spec the run read", upstreamModel, target.APIToken, token, refusalAppToken)
		}
	}
	if streams == 0 {
		t.Fatalf("no stream of %s (streamed %v), want at least one", upstreamModel, f.prov.streamedModels())
	}
}

// storedSpec reads mappingID's spec past the failure injection.
func (f *refusalFixture) storedSpec(t *testing.T, mappingID string) routing.RuntimeSpec {
	t.Helper()
	spec, ok, err := f.specs.Store.RuntimeSpecByMapping(context.Background(), mappingID)
	if err != nil || !ok {
		t.Fatalf("RuntimeSpecByMapping(%s) = (%v, %v)", mappingID, ok, err)
	}
	return spec
}

// assertNothingReserved fails unless the owned server has no run at all:
// not busy, and a zero Status, so a refusal left no lingering entry either.
func (f *refusalFixture) assertNothingReserved(t *testing.T) {
	t.Helper()
	if f.srv.Benchmarks.ServerBusy(baOwnedServer) {
		t.Fatal("ServerBusy = true after a refusal, want false (nothing reserved)")
	}
	if st := f.srv.Benchmarks.Status(baOwnedServer); !reflect.DeepEqual(st, BenchmarkStatus{}) {
		t.Fatalf("Status = %+v after a refusal, want the zero value", st)
	}
}

// ensuringColdLister adds provider.RuntimeEnsurer to a coldLister: the agent
// router's ensure route, which records each model it is asked to start and
// streams nothing.
type ensuringColdLister struct {
	*coldLister
	ensured []string
}

func (c *ensuringColdLister) EnsureRuntimeModel(_ context.Context, target routing.Target) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensured = append(c.ensured, target.ProviderModel)
	return nil
}

func (c *ensuringColdLister) ensuredModels() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ensured...)
}

func (f *refusalFixture) waitFinished(t *testing.T) BenchmarkStatus {
	t.Helper()
	waitFor(t, func() bool { return !f.srv.Benchmarks.Status(baOwnedServer).Running })
	return f.srv.Benchmarks.Status(baOwnedServer)
}

func TestBenchmarkRefusalErrorsArePinned(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
		msg  string
	}{
		{errBenchmarkImagesOnly, "benchmark.images_only", "this model, or every model in the run's scope, serves only images, so no chat prompt is sent"},
		{errBenchmarkAgentEnsureUnsupported, "benchmark.agent_ensure_unsupported", "this model serves only images, and starting it without a chat prompt needs the agent feature runtime_ensure (agent 0.8.0 or newer), or the agent has not reported since the gateway restarted"},
		{errBenchmarkSpecForceStopped, "benchmark.spec_force_stopped", "this model's runtime spec is force-stopped; clear the admin override before loading it"},
	} {
		rec := httptest.NewRecorder()
		writeBenchmarkError(rec, tc.err)
		var body apierror.Body
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: decode: %v", tc.code, err)
		}
		if rec.Code != http.StatusConflict || body.Error.Code != tc.code || body.Error.Message != tc.msg {
			t.Fatalf("writeBenchmarkError(%v) = %d %q %q, want 409 %q %q", tc.err, rec.Code, body.Error.Code, body.Error.Message, tc.code, tc.msg)
		}
	}
	rec := httptest.NewRecorder()
	writeBenchmarkRequestFailed(rec)
	var body apierror.Body
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("request_failed: decode: %v", err)
	}
	if rec.Code != http.StatusInternalServerError || body.Error.Code != "benchmark.request_failed" || body.Error.Message != "benchmark request failed" {
		t.Fatalf("writeBenchmarkRequestFailed = %d %q %q, want 500 benchmark.request_failed", rec.Code, body.Error.Code, body.Error.Message)
	}
	if runtimeEnsureFeature != "runtime_ensure" {
		t.Fatalf("runtimeEnsureFeature = %q, want runtime_ensure (the agent declares exactly this name)", runtimeEnsureFeature)
	}
	if benchmarkSkippedImagesOnly != "images_only" {
		t.Fatalf("benchmarkSkippedImagesOnly = %q, want images_only (the portal switches on it)", benchmarkSkippedImagesOnly)
	}
}

// Each Load refusal reason is the suffix of the code its sentinel answers,
// and the set is closed: the model list carries the same strings.
func TestLoadRefusalReasonsNameTheirCodes(t *testing.T) {
	reasons := make([]string, 0, len(loadRefusalErrs))
	for reason, err := range loadRefusalErrs {
		reasons = append(reasons, reason)
		if err.Error() != "benchmark."+reason {
			t.Fatalf("loadRefusalErrs[%q] = %q, want benchmark.%s", reason, err.Error(), reason)
		}
	}
	slices.Sort(reasons)
	if want := []string{"agent_ensure_unsupported", "images_only", "spec_force_stopped"}; !reflect.DeepEqual(reasons, want) {
		t.Fatalf("load refusal reasons = %v, want %v", reasons, want)
	}
}

func TestLoadRefusal(t *testing.T) {
	images := []string{routing.APIFlavorOpenAIImages}
	text := []string{routing.APIFlavorOpenAI}
	agentApp := routing.Application{Type: routing.ProviderServerAgent, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}}
	agentImagesApp := routing.Application{Type: routing.ProviderServerAgent, APIFlavors: images}
	sdApp := routing.Application{Type: routing.ProviderStableDiffusionCpp, APIFlavors: images}
	sdMixedApp := routing.Application{Type: routing.ProviderStableDiffusionCpp, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}}
	spec := func(flavors []string, adminState string) routing.RuntimeSpec {
		return routing.RuntimeSpec{APIFlavors: flavors, AdminState: adminState}
	}
	for _, tc := range []struct {
		name    string
		app     routing.Application
		spec    routing.RuntimeSpec
		hasSpec bool
		ensure  bool
		want    string
	}{
		{"agent images-only child, runtime_ensure declared", agentApp, spec(images, ""), true, true, ""},
		{"agent images-only child, runtime_ensure not declared", agentApp, spec(images, ""), true, false, loadRefusalAgentEnsureUnsupported},
		{"agent images-only force-stopped child without runtime_ensure: the images-only reason first", agentApp, spec(images, "force_stopped"), true, false, loadRefusalAgentEnsureUnsupported},
		{"agent images-only force-stopped child with runtime_ensure", agentApp, spec(images, "force_stopped"), true, true, loadRefusalSpecForceStopped},
		{"agent text child without runtime_ensure", agentApp, spec(text, ""), true, false, ""},
		{"agent force-stopped text child", agentApp, spec(text, "force_stopped"), true, true, loadRefusalSpecForceStopped},
		{"agent force-running text child", agentApp, spec(text, "force_running"), true, false, ""},
		{"agent mapping without a spec, images-only application, no runtime_ensure", agentImagesApp, routing.RuntimeSpec{}, false, false, loadRefusalAgentEnsureUnsupported},
		{"agent mapping without a spec, images-only application, runtime_ensure", agentImagesApp, routing.RuntimeSpec{}, false, true, ""},
		{"agent spec stored as [] under an images-only application", agentImagesApp, spec([]string{}, ""), true, false, ""},
		{"agent mapping without a spec: a spec value handed in is not a spec", agentApp, spec(images, "force_stopped"), false, false, ""},
		{"stable_diffusion_cpp images-only application", sdApp, routing.RuntimeSpec{}, false, false, loadRefusalImagesOnly},
		{"stable_diffusion_cpp images-only application, runtime_ensure declared", sdApp, routing.RuntimeSpec{}, false, true, loadRefusalImagesOnly},
		{"stable_diffusion_cpp application that also serves openai", sdMixedApp, routing.RuntimeSpec{}, false, false, ""},
		{"a spec never applies to a non-agent application", sdMixedApp, spec(images, "force_stopped"), true, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := loadRefusal(tc.app, tc.spec, tc.hasSpec, tc.ensure); got != tc.want {
				t.Fatalf("loadRefusal = %q, want %q", got, tc.want)
			}
		})
	}
}

// noSpecReadStore fails the test on any runtime-spec read and answers every
// other read from the embedded store, so a test can assert that a target is
// built from the spec it is handed, with a clear failure instead of a nil
// store's panic.
type noSpecReadStore struct {
	routing.Store
	t *testing.T
}

func (s noSpecReadStore) RuntimeSpecByMapping(context.Context, string) (routing.RuntimeSpec, bool, error) {
	s.t.Helper()
	s.t.Fatal("a runtime spec was read, want the target built from the spec it was handed")
	return routing.RuntimeSpec{}, false, nil
}

// benchmarkTargetFor keeps a spec only where routing.Resolver.targetFrom
// applies one: a server_agent mapping that has a spec. It reads no spec.
func TestBenchmarkTargetForKeepsOnlyAnAgentMappingsSpec(t *testing.T) {
	ctx := context.Background()
	srv := &Server{Routes: noSpecReadStore{Store: routing.NewMemoryStore(), t: t}}
	spec := routing.RuntimeSpec{ID: "rs1", APIToken: "sealed", APIFlavors: []string{routing.APIFlavorOpenAI}}
	agent := routing.Application{Type: routing.ProviderServerAgent}
	plain := routing.Application{Type: routing.ProviderMock}
	if got := srv.benchmarkTargetFor(ctx, routing.AIServer{}, agent, routing.ModelMapping{ID: "m1"}, spec, true).spec; !reflect.DeepEqual(got, spec) {
		t.Fatalf("agent mapping with a spec: tgt.spec = %+v, want %+v", got, spec)
	}
	if got := srv.benchmarkTargetFor(ctx, routing.AIServer{}, agent, routing.ModelMapping{ID: "m1"}, spec, false).spec; !reflect.DeepEqual(got, routing.RuntimeSpec{}) {
		t.Fatalf("agent mapping without a spec: tgt.spec = %+v, want zero", got)
	}
	if got := srv.benchmarkTargetFor(ctx, routing.AIServer{}, plain, routing.ModelMapping{ID: "m1"}, spec, true).spec; !reflect.DeepEqual(got, routing.RuntimeSpec{}) {
		t.Fatalf("non-agent mapping: tgt.spec = %+v, want zero", got)
	}
}

// A Load that passed loadRefusal loads an images-only mapping without
// generating, and a text mapping by generating, from the spec the starter
// read. It reads no spec.
func TestLoadTargetForMarksAnImagesOnlyChild(t *testing.T) {
	ctx := context.Background()
	srv := &Server{Routes: noSpecReadStore{Store: routing.NewMemoryStore(), t: t}}
	app := routing.Application{Type: routing.ProviderServerAgent, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}}
	view := portal.BenchmarkTargetView{App: app, Mapping: routing.ModelMapping{ID: "m1"}}
	sd := routing.RuntimeSpec{ID: "rs_sd", APIFlavors: []string{routing.APIFlavorOpenAIImages}}
	tgt := srv.loadTargetFor(ctx, view, sd, true)
	if !tgt.loadWithoutGenerating || tgt.spec.ID != "rs_sd" {
		t.Fatalf("images-only child: loadWithoutGenerating = %v, spec = %q; want true, rs_sd", tgt.loadWithoutGenerating, tgt.spec.ID)
	}
	txt := routing.RuntimeSpec{ID: "rs_text", APIFlavors: []string{routing.APIFlavorOpenAI}}
	if tgt := srv.loadTargetFor(ctx, view, txt, true); tgt.loadWithoutGenerating {
		t.Fatal("text child: loadWithoutGenerating = true, want false (it loads by generating)")
	}
}

// Every manual starter refuses a busy server before it reads the spec: a
// VRAM run's drain has force-stopped every spec, so the read would answer a
// refusal about that run's own override. The read-failing mapping proves
// the order: had any starter read first, it would answer 500.
func TestStartersRefuseABusyServerBeforeReadingTheSpec(t *testing.T) {
	f := newRefusalFixture(t)
	if _, ok := f.srv.Benchmarks.TryStart(baOwnedServer, "vram-probe", "vram", 1, time.Now(), func() {}); !ok {
		t.Fatal("pre-reserve TryStart failed")
	}
	for _, path := range []string{
		"/api/portal/mappings/rf_unread/benchmark?mode=speed",
		"/api/portal/mappings/rf_unread/probe-context",
		"/api/portal/mappings/rf_unread/load",
		"/api/portal/mappings/rf_unread/probe-vram",
		"/api/portal/applications/rf_agent/benchmark",
	} {
		if status, code, _ := f.post(t, path); status != http.StatusConflict || code != codeBenchmarkAlreadyRunning {
			t.Fatalf("POST %s on a busy server = %d %q, want 409 %s", path, status, code, codeBenchmarkAlreadyRunning)
		}
	}
	if n := f.specs.readCount("rf_unread"); n != 0 {
		t.Fatalf("spec reads on a busy server = %d, want 0", n)
	}
	if !f.srv.Benchmarks.ServerBusy(baOwnedServer) {
		t.Fatal("the running reservation was released by a refused starter")
	}
}

func TestStartLoadModelRefusals(t *testing.T) {
	t.Run("images-only agent child without runtime_ensure", func(t *testing.T) {
		f := newRefusalFixture(t)
		before := f.storedSpec(t, "rf_sd")
		if status, code, _ := f.post(t, "/api/portal/mappings/rf_sd/load"); status != http.StatusConflict || code != codeBenchmarkAgentEnsureUnsupported {
			t.Fatalf("status = %d code = %q, want 409 %s", status, code, codeBenchmarkAgentEnsureUnsupported)
		}
		f.assertNothingReserved(t)
		if after := f.storedSpec(t, "rf_sd"); !reflect.DeepEqual(after, before) {
			t.Fatalf("spec changed by a refused Load:\n before %+v\n after  %+v", before, after)
		}
		if got := f.prov.streamedModels(); len(got) != 0 {
			t.Fatalf("streamed %v, want no chat prompt", got)
		}
	})
	t.Run("images-only application that is not agent-managed", func(t *testing.T) {
		f := newRefusalFixture(t)
		f.srv.AgentFeatures.Set(baOwnedServer, []string{runtimeEnsureFeature})
		if status, code, _ := f.post(t, "/api/portal/mappings/rf_sdext/load"); status != http.StatusConflict || code != codeBenchmarkImagesOnly {
			t.Fatalf("status = %d code = %q, want 409 %s", status, code, codeBenchmarkImagesOnly)
		}
		f.assertNothingReserved(t)
	})
	t.Run("spec read error", func(t *testing.T) {
		f := newRefusalFixture(t)
		logs := withCapturedSlogAtTheDefaultLevel(t)
		if status, code, _ := f.post(t, "/api/portal/mappings/rf_unread/load"); status != http.StatusInternalServerError || code != codeBenchmarkRequestFailed {
			t.Fatalf("status = %d code = %q, want 500 %s", status, code, codeBenchmarkRequestFailed)
		}
		f.assertNothingReserved(t)
		logged := false
		for _, r := range logs.Snapshot() {
			if mappingID, _ := r.Attrs["mapping_id"].(string); r.Level == "WARN" && r.Msg == "benchmark: runtime spec read failed; run refused" && mappingID == "rf_unread" {
				logged = true
			}
		}
		if !logged {
			t.Fatalf("no WARN naming the refused spec read of rf_unread; records = %+v", logs.Snapshot())
		}
	})
	t.Run("force-stopped spec", func(t *testing.T) {
		f := newRefusalFixture(t)
		f.srv.AgentFeatures.Set(baOwnedServer, []string{runtimeEnsureFeature})
		f.setAdminState(t, "rf_text", "force_stopped")
		if status, code, _ := f.post(t, "/api/portal/mappings/rf_text/load"); status != http.StatusConflict || code != codeBenchmarkSpecForceStopped {
			t.Fatalf("status = %d code = %q, want 409 %s", status, code, codeBenchmarkSpecForceStopped)
		}
		f.assertNothingReserved(t)
		if got := f.storedSpec(t, "rf_text").AdminState; got != "force_stopped" {
			t.Fatalf("admin_state = %q after a refused Load, want force_stopped kept", got)
		}
	})
	t.Run("during a VRAM run", func(t *testing.T) {
		f := newRefusalFixture(t)
		if _, ok := f.srv.Benchmarks.TryStart(baOwnedServer, "vram-probe", "vram", 1, time.Now(), func() {}); !ok {
			t.Fatal("pre-reserve TryStart failed")
		}
		f.setAdminState(t, "rf_text", "force_stopped") // what the run's drain stores
		if status, code, _ := f.post(t, "/api/portal/mappings/rf_text/load"); status != http.StatusConflict || code != codeBenchmarkAlreadyRunning {
			t.Fatalf("status = %d code = %q, want 409 %s", status, code, codeBenchmarkAlreadyRunning)
		}
		if !f.srv.Benchmarks.ServerBusy(baOwnedServer) {
			t.Fatal("the VRAM run's reservation was released by a refused Load")
		}
	})
	t.Run("images-only agent child with runtime_ensure starts", func(t *testing.T) {
		f := newRefusalFixture(t)
		ensurer := &ensuringColdLister{coldLister: f.prov}
		f.srv.Provider = ensurer
		f.srv.AgentFeatures.Set(baOwnedServer, []string{runtimeEnsureFeature})
		if status, code, _ := f.post(t, "/api/portal/mappings/rf_sd/load"); status != http.StatusAccepted {
			t.Fatalf("status = %d code = %q, want 202", status, code)
		}
		st := f.waitFinished(t)
		if len(st.Results) != 1 || !st.Results[0].Loaded {
			t.Fatalf("results = %+v, want one Loaded result", st.Results)
		}
		if got := ensurer.ensuredModels(); !slices.Equal(got, []string{"up-rf_sd"}) {
			t.Fatalf("ensured %v, want [up-rf_sd] (the Load starts the child through the ensure route)", got)
		}
		if got := f.prov.streamedModels(); slices.Contains(got, "up-rf_sd") {
			t.Fatalf("streamed %v, want no chat prompt for the images-only child", got)
		}
	})
	t.Run("a text mapping still streams, from one spec read", func(t *testing.T) {
		f := newRefusalFixture(t)
		f.srv.AgentFeatures.Set(baOwnedServer, []string{runtimeEnsureFeature})
		if status, code, _ := f.post(t, "/api/portal/mappings/rf_text/load"); status != http.StatusAccepted {
			t.Fatalf("status = %d code = %q, want 202", status, code)
		}
		st := f.waitFinished(t)
		if len(st.Results) != 1 || !st.Results[0].Loaded {
			t.Fatalf("results = %+v, want one Loaded result", st.Results)
		}
		if got := f.prov.streamedModels(); !slices.Contains(got, "up-rf_text") {
			t.Fatalf("streamed %v, want the text mapping's model", got)
		}
		if n := f.specs.readCount("rf_text"); n != 1 {
			t.Fatalf("spec reads = %d, want exactly 1 (the target is built from the starter's read)", n)
		}
	})
}

func TestStartContextProbeRefusesAnImagesOnlyMapping(t *testing.T) {
	for _, tc := range []struct {
		mapping string
		status  int
		code    string
	}{
		{"rf_sd", http.StatusConflict, codeBenchmarkImagesOnly},
		{"rf_sdext", http.StatusConflict, codeBenchmarkImagesOnly},
		{"rf_unread", http.StatusInternalServerError, codeBenchmarkRequestFailed},
	} {
		t.Run(tc.mapping, func(t *testing.T) {
			f := newRefusalFixture(t)
			f.srv.AgentFeatures.Set(baOwnedServer, []string{runtimeEnsureFeature})
			if status, code, _ := f.post(t, "/api/portal/mappings/"+tc.mapping+"/probe-context"); status != tc.status || code != tc.code {
				t.Fatalf("status = %d code = %q, want %d %s", status, code, tc.status, tc.code)
			}
			f.assertNothingReserved(t)
		})
	}
}

func TestStartBenchmarkMappingScopeRefusesAnImagesOnlyMapping(t *testing.T) {
	for _, mode := range []string{"speed", "capacity", "both", "vision"} {
		for _, tc := range []struct {
			mapping string
			status  int
			code    string
		}{
			{"rf_sd", http.StatusConflict, codeBenchmarkImagesOnly},
			{"rf_sdext", http.StatusConflict, codeBenchmarkImagesOnly},
			{"rf_unread", http.StatusInternalServerError, codeBenchmarkRequestFailed},
		} {
			t.Run(mode+"/"+tc.mapping, func(t *testing.T) {
				f := newRefusalFixture(t)
				if status, code, _ := f.post(t, "/api/portal/mappings/"+tc.mapping+"/benchmark?mode="+mode); status != tc.status || code != tc.code {
					t.Fatalf("status = %d code = %q, want %d %s", status, code, tc.status, tc.code)
				}
				f.assertNothingReserved(t)
				if got := f.prov.streamedModels(); len(got) != 0 {
					t.Fatalf("streamed %v, want no chat prompt", got)
				}
			})
		}
	}
}

// An application-scope run measures its runnable mapping and records the
// skipped and the unreadable one first: no history row for either, no
// prompt to either, and both counted in Total and Done.
func TestStartBenchmarkApplicationScopeRecordsSkipsFirst(t *testing.T) {
	f := newRefusalFixture(t)
	status, code, body := f.post(t, "/api/portal/applications/rf_agent/benchmark?mode=speed")
	if status != http.StatusAccepted {
		t.Fatalf("status = %d code = %q, want 202", status, code)
	}
	if !strings.Contains(string(body), `"skipped":"images_only"`) {
		t.Fatalf("202 body = %s, want a result carrying \"skipped\":\"images_only\"", body)
	}
	var initial BenchmarkStatus
	if err := json.Unmarshal(body, &initial); err != nil {
		t.Fatalf("decode 202 body: %v", err)
	}
	wantRecorded := []BenchmarkResult{
		{MappingID: "rf_sd", GatewayModelName: "gw-rf_sd", Skipped: benchmarkSkippedImagesOnly},
		{MappingID: "rf_unread", GatewayModelName: "gw-rf_unread", Error: "runtime spec unreadable; not benchmarked (no chat prompt sent): spec read down"},
	}
	if !(initial.Total == 3 && len(initial.Results) >= 2 && reflect.DeepEqual(initial.Results[:2], wantRecorded)) {
		t.Fatalf("initial status: total %d results %+v; want total 3 with its first two results %+v (a status read right after the go statement is timing-dependent past that point)", initial.Total, initial.Results, wantRecorded)
	}

	st := f.waitFinished(t)
	if st.Total != 3 || st.Done != 3 || len(st.Results) != 3 {
		t.Fatalf("final status: total %d done %d results %d; want 3/3/3", st.Total, st.Done, len(st.Results))
	}
	if !reflect.DeepEqual(st.Results[:2], wantRecorded) {
		t.Fatalf("first results = %+v, want the recorded ones %+v", st.Results[:2], wantRecorded)
	}
	if got := st.Results[2]; got.MappingID != "rf_text" || got.Error != "" || got.Skipped != "" {
		t.Fatalf("measured result = %+v, want rf_text measured without error", got)
	}
	if got := f.prov.streamedModels(); slices.Contains(got, "up-rf_sd") || slices.Contains(got, "up-rf_unread") || !slices.Contains(got, "up-rf_text") {
		t.Fatalf("streamed %v, want only up-rf_text", got)
	}
	f.assertHistoryRows(t, map[string]int{"rf_sd": 0, "rf_unread": 0, "rf_text": 1})
	f.assertOneSpecReadEach(t, "rf_sd", "rf_text", "rf_unread")
	if warns := f.warnsFor("benchmark: runtime spec read failed; mapping not benchmarked", "rf_unread"); len(warns) != 1 {
		t.Fatalf("WARNs naming the unbenchmarked rf_unread = %d, want exactly 1; records = %+v", len(warns), f.logs.Snapshot())
	}
}

func TestStartBenchmarkApplicationScopeWithNothingRunnable(t *testing.T) {
	t.Run("every view images-only", func(t *testing.T) {
		f := newRefusalFixture(t)
		if status, code, _ := f.post(t, "/api/portal/applications/rf_sdapp/benchmark?mode=speed"); status != http.StatusConflict || code != codeBenchmarkImagesOnly {
			t.Fatalf("status = %d code = %q, want 409 %s", status, code, codeBenchmarkImagesOnly)
		}
		f.assertNothingReserved(t)
		for _, mappingID := range []string{"rf_sdext", "rf_sdext2"} {
			if n := f.specs.readCount(mappingID); n != 0 {
				t.Fatalf("spec reads for %s = %d, want 0 (a mapping of a non-agent application has no spec)", mappingID, n)
			}
		}
	})
	t.Run("every view unreadable or images-only", func(t *testing.T) {
		f := newRefusalFixture(t)
		f.specs.failRead("rf_text")
		if status, code, _ := f.post(t, "/api/portal/applications/rf_agent/benchmark?mode=speed"); status != http.StatusInternalServerError || code != codeBenchmarkRequestFailed {
			t.Fatalf("status = %d code = %q, want 500 %s", status, code, codeBenchmarkRequestFailed)
		}
		f.assertNothingReserved(t)
		if got := f.prov.streamedModels(); len(got) != 0 {
			t.Fatalf("streamed %v, want no chat prompt", got)
		}
	})
}

// The VRAM probe reads the target's spec fail-closed too: a read error
// answers 500 before the plan runs, instead of planning a run whose load
// would carry the wrong credential.
func TestStartVRAMProbeRefusesAnUnreadableSpec(t *testing.T) {
	f := newRefusalFixture(t)
	f.srv.AgentFeatures.Set(baOwnedServer, []string{"runtime_manager"})
	if status, code, _ := f.post(t, "/api/portal/mappings/rf_unread/probe-vram"); status != http.StatusInternalServerError || code != codeBenchmarkRequestFailed {
		t.Fatalf("status = %d code = %q, want 500 %s", status, code, codeBenchmarkRequestFailed)
	}
	f.assertNothingReserved(t)
}
