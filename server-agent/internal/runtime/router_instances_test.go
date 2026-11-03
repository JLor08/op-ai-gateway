// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Router tests for instance addresses (#340, design §5.3): a request naming
// "m#k" reaches instance k, and the child -- launched with the base name m --
// sees m in its place, in a JSON body, in a multipart form and in a live
// transcription's query alike. Instance 0, which is every request a spec that
// never replicates ever gets, passes byte for byte as it always has. A plain
// request never starts a burst instance (§5.4 rule 6), and the two burst
// refusals are 409s.

// redirectManager is a REAL manager whose admitted endpoint is swapped for a
// recording child's: resolution, admission and refusals are the manager's own
// -- the instance processes really start, and their rows can be read back --
// while the process the router forwards to is the test's, so what a child
// receives can be read back exactly. EnsureRunning (the /ensure route, which
// forwards nothing) is the manager's own.
type redirectManager struct {
	managerPort
	endpoint string
}

func (r redirectManager) EnsureServing(ctx context.Context, model string) (string, func(), error) {
	_, release, err := r.managerPort.EnsureServing(ctx, model)
	if err != nil {
		return "", nil, err
	}
	return r.endpoint, release, nil
}

// instanceRouter applies specs to a real manager and returns it with a router
// over it whose plain requests reach the returned recording child.
func instanceRouter(t *testing.T, cfg Config) (*Manager, http.Handler, *editChild) {
	t.Helper()
	m := newTestManager(t, allowlistPolicy())
	m.Apply(cfg)
	child := newRecordingChild(t, `{"ok":true}`)
	return m, newRouter(redirectManager{managerPort: m, endpoint: child.srv.URL}), child
}

// ensureThroughRouter starts address through the router's own /ensure route --
// the way the gateway's scaler brings burst capacity up -- and fails the test
// unless it answers 200.
func ensureThroughRouter(t *testing.T, h http.Handler, address string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, ensurePathPrefix+url.PathEscape(address), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /ensure/%s = %d %s, want 200", address, rec.Code, rec.Body.String())
	}
}

// postJSON sends body to /v1/chat/completions through h on the test goroutine.
func postJSON(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// formPart is one part of a multipart body as it travels: its header and its
// exact bytes.
type formPart struct {
	name   string
	header map[string][]string
	data   []byte
}

// formParts reads every part of a multipart body raw -- no transfer decoding --
// so two bodies can be compared part by part, byte for byte.
func formParts(t *testing.T, contentType string, body []byte) []formPart {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("parse Content-Type %q: %v", contentType, err)
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var parts []formPart
	for {
		p, err := mr.NextRawPart()
		if err == io.EOF {
			return parts
		}
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		data, err := io.ReadAll(p)
		if err != nil {
			t.Fatalf("read part %q: %v", p.FormName(), err)
		}
		parts = append(parts, formPart{name: p.FormName(), header: p.Header, data: data})
	}
}

// TestRouterRewritesInstanceModelJSON: a JSON request for m#1 reaches instance 1
// with its top-level model rewritten to m -- on the plain path and the
// streaming one -- and every other value intact, numbers included: 1.50 stays
// 1.50, never re-read as the float 1.5.
func TestRouterRewritesInstanceModelJSON(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m, h, child := instanceRouter(t, Config{Specs: []Spec{replicableSpec("S", "m")}})
	ensureThroughRouter(t, h, "m#1")

	for _, tc := range []struct{ name, body, want string }{
		{"plain", `{"model":"m#1","n":1.50}`, `{"model":"m","n":1.50}`},
		{"streaming", `{"model":"m#1","stream":true,"n":1.50}`, `{"model":"m","n":1.50,"stream":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := postJSON(t, h, tc.body); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if _, _, _, got := child.snapshot(); string(got) != tc.want {
				t.Fatalf("the child received %s, want %s: the base model, every other value as sent", got, tc.want)
			}
		})
	}
	if st := mustInstance(t, m, "S", 1); st.State != StateRunning {
		t.Errorf("instance 1 = %+v, want running", st)
	}
	if st := mustInstance(t, m, "S", 0); st.PID != 0 {
		t.Errorf("instance 0 = %+v, want it never started: the requests named instance 1", st)
	}
}

// TestRouterRewritesInstanceModelMultipart: a multipart request for m#1 reaches
// instance 1 with its model field rewritten to m, and every other part -- its
// headers and its bytes, the image included -- exactly as it was sent, in order.
func TestRouterRewritesInstanceModelMultipart(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m, h, child := instanceRouter(t, Config{Specs: []Spec{replicableSpec("S", "m")}})
	ensureThroughRouter(t, h, "m#1")

	body, contentType := editBody(t, "m#1", "")
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	_, _, gotType, gotBody := child.snapshot()
	sent, got := formParts(t, contentType, body), formParts(t, gotType, gotBody)
	if len(got) != len(sent) {
		t.Fatalf("the child received %d parts, want the %d sent", len(got), len(sent))
	}
	for i := range sent {
		if sent[i].name == "model" {
			if got[i].name != "model" || string(got[i].data) != "m" {
				t.Errorf("part %d = %s %q, want the model field with the base name m", i, got[i].name, got[i].data)
			}
			continue
		}
		if got[i].name != sent[i].name || !bytes.Equal(got[i].data, sent[i].data) || fmt.Sprint(got[i].header) != fmt.Sprint(sent[i].header) {
			t.Errorf("part %d = %s %v %q, want it as sent: %s %v %q", i, got[i].name, got[i].header, got[i].data, sent[i].name, sent[i].header, sent[i].data)
		}
	}
	if st := mustInstance(t, m, "S", 1); st.State != StateRunning {
		t.Errorf("instance 1 = %+v, want running", st)
	}
}

// TestRouterLiveInstanceQueryUsesBase: a live transcription for m#1 reaches
// instance 1's child with model=m in its query, the inbound params unchanged.
func TestRouterLiveInstanceQueryUsesBase(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{replicableSpec("S", "m")}})
	child, capCh := newLiveChild(t)
	h := newRouter(redirectManager{managerPort: m, endpoint: child.URL})
	ensureThroughRouter(t, h, "m#1")
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := srv.Client().Post(srv.URL+"/live/m%231?sample_rate=16000&language=de&model=m%231", "application/octet-stream", strings.NewReader("pcm"))
	if err != nil {
		t.Fatalf("POST /live/m%%231: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read the relayed stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got liveCapture
	select {
	case got = <-capCh:
	case <-time.After(3 * time.Second):
		t.Fatal("the child never reported what it received")
	}
	if got.model != "m" {
		t.Errorf("child query model = %q, want the base name m", got.model)
	}
	if got.sampleRate != "16000" || got.language != "de" {
		t.Errorf("forwarded params = {sample_rate:%q language:%q}, want {16000 de}", got.sampleRate, got.language)
	}
	if st := mustInstance(t, m, "S", 1); st.State != StateRunning {
		t.Errorf("instance 1 = %+v, want running", st)
	}
}

// TestRouterInstanceZeroUntouched: a request for instance 0 of a replicable spec
// -- the address every request of a spec that never replicates has -- is
// forwarded byte for byte: a JSON body whose spacing, key order, number and HTML
// characters a re-serialization would all change, and a multipart body with its
// own Content-Type, boundary and all.
func TestRouterInstanceZeroUntouched(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m, h, child := instanceRouter(t, Config{Specs: []Spec{replicableSpec("S", "m")}})

	const body = `{ "n": 1.50, "model" : "m", "html": "<&>" }`
	if rec := postJSON(t, h, body); rec.Code != http.StatusOK {
		t.Fatalf("JSON status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, _, _, got := child.snapshot(); string(got) != body {
		t.Fatalf("the child received %s, want the body byte for byte: %s", got, body)
	}

	form, contentType := editBody(t, "m", "")
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(form))
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("multipart status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, _, gotType, gotBody := child.snapshot(); gotType != contentType || !bytes.Equal(gotBody, form) {
		t.Fatalf("the child received Content-Type %q and %d bytes, want %q and the %d bytes sent, unchanged", gotType, len(gotBody), contentType, len(form))
	}

	if st := mustInstance(t, m, "S", 0); st.State != StateRunning {
		t.Errorf("instance 0 = %+v, want running: it served both requests", st)
	}
	if st := instanceStatus(m, "S", 1); st != nil {
		t.Errorf("instance 1 = %+v, want none", st)
	}
}

// TestRouterLiteralHashModelWins: foo#2 is the literal upstream model of L,
// while F (upstream foo) is replicable, so foo#2 also parses as F's instance 2.
// The literal name wins: the request reaches L, unchanged, and F has no
// instance 2 (Review Focus 1).
func TestRouterLiteralHashModelWins(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m, h, child := instanceRouter(t, Config{Specs: []Spec{replicableSpec("F", "foo"), baseSpec("L", "foo#2")}})

	const body = `{"model":"foo#2","n":1.50}`
	if rec := postJSON(t, h, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, _, _, got := child.snapshot(); string(got) != body {
		t.Fatalf("the child received %s, want %s unchanged: foo#2 is L's own name, not an address to rewrite", got, body)
	}
	if st := statusFor(m, "L"); st == nil || st.State != StateRunning {
		t.Errorf("L = %+v, want running: it served the request", st)
	}
	if st := instanceStatus(m, "F", 2); st != nil {
		t.Errorf("F instance 2 = %+v, want none", st)
	}
	if st := mustInstance(t, m, "F", 0); st.PID != 0 {
		t.Errorf("F instance 0 = %+v, want it never started", st)
	}
}

// assertConflict fails the test unless rec is a 409 carrying code.
func assertConflict(t *testing.T, status int, body io.Reader, code string) {
	t.Helper()
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
	if got := decodeJSON[errorEnvelope](t, body).Error.Code; got != code {
		t.Fatalf("code = %q, want %s", got, code)
	}
}

// TestRouterBurstNoCapacityIs409: an ensure of a burst instance that does not
// fit into free room -- here the one process slot instance 0 holds, and a
// sibling is never evicted for it -- is 409 runtime.instance_no_capacity, the
// gateway's cue to try another host. Nothing is created, nothing is evicted.
func TestRouterBurstNoCapacityIs409(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{replicableSpec("S", "m")}, MaxProcesses: 1})
	h := NewRouter(m)
	ensureThroughRouter(t, h, "m")
	pid := mustInstance(t, m, "S", 0).PID

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ensure/m%231", nil))
	assertConflict(t, rec.Code, rec.Body, "runtime.instance_no_capacity")

	if st := instanceStatus(m, "S", 1); st != nil {
		t.Errorf("instance 1 = %+v, want none: a refused burst instance leaves no state", st)
	}
	if st := mustInstance(t, m, "S", 0); st.State != StateRunning || st.PID != pid {
		t.Errorf("instance 0 = %+v, want still running as pid %d", st, pid)
	}
}

// TestRouterBurstDemandUnknownIs409: an ensure of a burst instance whose demand
// is unknown -- nothing declared, and no running sibling measured to inherit it
// from -- is 409 runtime.instance_demand_unknown, which the gateway retries
// shortly.
func TestRouterBurstDemandUnknownIs409(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	spec := replicableSpec("S", "m")
	spec.RAMMB = 0 // unknown
	m.Apply(Config{Specs: []Spec{spec}})

	rec := httptest.NewRecorder()
	NewRouter(m).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ensure/m%231", nil))
	assertConflict(t, rec.Code, rec.Body, "runtime.instance_demand_unknown")
	if st := instanceStatus(m, "S", 1); st != nil {
		t.Errorf("instance 1 = %+v, want none", st)
	}
}

// TestSentinelCodeMapsInstanceRefusals pins the two burst refusals' wire codes
// and their status, wrapped or not.
func TestSentinelCodeMapsInstanceRefusals(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{ErrInstanceNoCapacity, "runtime.instance_no_capacity"},
		{ErrInstanceDemandUnknown, "runtime.instance_demand_unknown"},
		{fmt.Errorf("refused: %w", ErrInstanceNoCapacity), "runtime.instance_no_capacity"},
		{fmt.Errorf("refused: %w", ErrInstanceDemandUnknown), "runtime.instance_demand_unknown"},
	} {
		if code, status := sentinelCode(tc.err); code != tc.code || status != http.StatusConflict {
			t.Errorf("sentinelCode(%v) = (%s, %d), want (%s, 409)", tc.err, code, status, tc.code)
		}
	}
}

// TestRouterPlainRequestNeverStartsABurstInstance: a request that is not an
// ensure -- JSON, streaming JSON, multipart, live -- naming a burst instance that
// is not running is refused with 409 runtime.instance_no_capacity, starts
// nothing and reaches no child; the gateway re-selects a running instance. Once
// an ensure has started that instance, the same request is served by it.
func TestRouterPlainRequestNeverStartsABurstInstance(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m, h, child := instanceRouter(t, Config{Specs: []Spec{replicableSpec("S", "m")}})
	srv := httptest.NewServer(h)
	defer srv.Close()

	form, formType := editBody(t, "m#1", "")
	for _, tc := range []struct {
		name, path, contentType string
		body                    []byte
	}{
		{"json", "/v1/chat/completions", "application/json", []byte(`{"model":"m#1"}`)},
		{"streaming json", "/v1/chat/completions", "application/json", []byte(`{"model":"m#1","stream":true}`)},
		{"multipart", "/v1/images/edits", formType, form},
		{"live", "/live/m%231", "application/octet-stream", []byte("pcm")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := srv.Client().Post(srv.URL+tc.path, tc.contentType, bytes.NewReader(tc.body))
			if err != nil {
				t.Fatalf("POST %s: %v", tc.path, err)
			}
			defer resp.Body.Close()
			assertConflict(t, resp.StatusCode, resp.Body, "runtime.instance_no_capacity")
		})
	}
	if hits, _, _, _ := child.snapshot(); hits != 0 {
		t.Errorf("the child was reached %d times, want never", hits)
	}
	if rows := m.Status(); len(rows) != 1 || rows[0].PID != 0 {
		t.Fatalf("Status() = %+v, want instance 0 alone, not started: a plain request starts nothing", rows)
	}

	ensureThroughRouter(t, h, "m#1")
	if rec := postJSON(t, h, `{"model":"m#1"}`); rec.Code != http.StatusOK {
		t.Fatalf("status once instance 1 runs = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, _, _, got := child.snapshot(); string(got) != `{"model":"m"}` {
		t.Errorf("the child received %s, want {\"model\":\"m\"}", got)
	}
}

// TestRouterModelsListsBaseNamesRunningListsInstances: /v1/models names each
// managed spec once, by its base name, whatever instances it runs -- it is the
// gateway's model sync, and an instance is not a model -- while /running lists
// every running instance by its address.
func TestRouterModelsListsBaseNamesRunningListsInstances(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	spec := replicableSpec("S", "m")
	spec.MinInstances = 2
	m.Apply(Config{Specs: []Spec{spec, baseSpec("T", "t")}})
	waitUntil(t, 5*time.Second, "instances 0 and 1 running", func() bool {
		s0, s1 := instanceStatus(m, "S", 0), instanceStatus(m, "S", 1)
		return s0 != nil && s0.State == StateRunning && s1 != nil && s1.State == StateRunning
	})
	h := NewRouter(m)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var ids []string
	for _, e := range decodeJSON[modelsResponse](t, rec.Body).Data {
		ids = append(ids, e.ID)
	}
	sort.Strings(ids)
	if fmt.Sprint(ids) != "[m t]" {
		t.Errorf("/v1/models ids = %v, want [m t]: one entry per spec, by its base name", ids)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/running", nil))
	var running []string
	for _, e := range decodeJSON[runningResponse](t, rec.Body).Running {
		running = append(running, e.Model)
	}
	sort.Strings(running)
	if fmt.Sprint(running) != "[m m#1]" {
		t.Errorf("/running models = %v, want [m m#1]: every running instance by its address", running)
	}
}

// instanceStatusManager is a managerPort fake for the /upstream relay over
// instance rows: Status is fixed, ResolveAddress answers from a fixed table the
// way the manager resolves, and an ensure is only counted -- the relay never
// asks for one.
type instanceStatusManager struct {
	statuses  []Status
	addresses map[string]resolvedAddress
	ensures   atomic.Int64
}

func (m *instanceStatusManager) EnsureRunning(context.Context, string) (string, func(), error) {
	m.ensures.Add(1)
	return "", nil, ErrModelNotManaged
}

func (m *instanceStatusManager) EnsureServing(ctx context.Context, model string) (string, func(), error) {
	return m.EnsureRunning(ctx, model)
}

func (m *instanceStatusManager) ResolveAddress(model string) (string, int, bool) {
	r := m.addresses[model]
	return r.base, r.k, r.ok
}
func (m *instanceStatusManager) LoadedModels() []string { return nil }
func (m *instanceStatusManager) Status() []Status       { return m.statuses }

// TestRouterUpstreamTargetsTheInstance: /upstream/{model} reaches the process of
// exactly the instance the address resolves to, and a query that names the
// model reaches an instance's child with the base name in it. An address the
// spec allows but no process serves is managed and not running; one it does not
// allow is not managed at all.
func TestRouterUpstreamTargetsTheInstance(t *testing.T) {
	type hit struct{ path, query string }
	recording := func() (*httptest.Server, chan hit) {
		hits := make(chan hit, 4)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits <- hit{r.URL.Path, r.URL.RawQuery}
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)
		return srv, hits
	}
	child0, hits0 := recording()
	child1, hits1 := recording()
	m := &instanceStatusManager{
		statuses: []Status{
			{SpecID: "S", Instance: 0, Model: "m", State: StateRunning, Port: childPort(t, child0.URL)},
			{SpecID: "S", Instance: 1, Model: "m#1", State: StateRunning, Port: childPort(t, child1.URL)},
		},
		addresses: map[string]resolvedAddress{
			"m":   {base: "m", k: 0, ok: true},
			"m#1": {base: "m", k: 1, ok: true},
			"m#3": {base: "m", k: 3, ok: true}, // allowed, but no process
		},
	}
	h := newRouter(m)
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	expect := func(hits chan hit, want hit) {
		t.Helper()
		select {
		case got := <-hits:
			if got != want {
				t.Errorf("the child received %+v, want %+v", got, want)
			}
		default:
			t.Errorf("the child was not reached; want %+v", want)
		}
	}

	if rec := get("/upstream/m%231/props?model=m%231"); rec.Code != http.StatusOK {
		t.Fatalf("instance 1 props = %d %s, want 200", rec.Code, rec.Body.String())
	}
	expect(hits1, hit{"/props", "model=m"})
	if rec := get("/upstream/m%231/v1/models?include_params=true"); rec.Code != http.StatusOK {
		t.Fatalf("instance 1 models = %d %s, want 200", rec.Code, rec.Body.String())
	}
	expect(hits1, hit{"/v1/models", "include_params=true"})
	if rec := get("/upstream/m/props?model=m"); rec.Code != http.StatusOK {
		t.Fatalf("instance 0 props = %d %s, want 200", rec.Code, rec.Body.String())
	}
	expect(hits0, hit{"/props", "model=m"})
	if len(hits0)+len(hits1) != 0 {
		t.Errorf("a further child was reached: %d / %d extra hits", len(hits0), len(hits1))
	}

	if rec := get("/upstream/m%233/props"); rec.Code != http.StatusNotFound || decodeErrorCode(t, rec) != "runtime.model_not_running" {
		t.Errorf("an allowed instance with no process = %d %s, want 404 runtime.model_not_running", rec.Code, rec.Body.String())
	}
	if rec := get("/upstream/m%239/props"); rec.Code != http.StatusNotFound || decodeErrorCode(t, rec) != codeModelNotManaged {
		t.Errorf("an address the spec does not allow = %d %s, want 404 %s", rec.Code, rec.Body.String(), codeModelNotManaged)
	}
	if n := m.ensures.Load(); n != 0 {
		t.Errorf("an ensure was asked for %d times, want never: a probe starts nothing", n)
	}
}

// TestRouterInstanceBase pins the rewrite decision every route shares: only an
// address the manager resolves to an instance k >= 1 is rewritten, to its base
// name; instance 0 -- a literal name with '#' in it included -- an unmanaged
// name and a nil manager are left alone. The live relay depends on it although
// its end-to-end test cannot show it: appended to the child's query unrewritten,
// "m#1" would reach the child as model=m anyway, because the '#' starts a URL
// fragment that is never sent.
func TestRouterInstanceBase(t *testing.T) {
	m := &instanceStatusManager{addresses: map[string]resolvedAddress{
		"m":     {base: "m", k: 0, ok: true},
		"m#1":   {base: "m", k: 1, ok: true},
		"foo#2": {base: "foo#2", k: 0, ok: true}, // a literal upstream model
	}}
	for _, tc := range []struct {
		model  string
		base   string
		rewrit bool
	}{
		{"m#1", "m", true},
		{"m", "", false},
		{"foo#2", "", false},
		{"nope#1", "", false},
	} {
		if base, ok := newRouter(m).instanceBase(tc.model); base != tc.base || ok != tc.rewrit {
			t.Errorf("instanceBase(%q) = (%q, %v), want (%q, %v)", tc.model, base, ok, tc.base, tc.rewrit)
		}
	}
	if base, ok := newRouter(nil).instanceBase("m#1"); ok || base != "" {
		t.Errorf("instanceBase with no manager = (%q, %v), want nothing rewritten", base, ok)
	}
}

// TestRewriteModelFieldKeepsEveryOtherValue: only the top-level model changes,
// every other value -- a nested "model", a number's exact digits, a big integer
// -- survives, and a body with nothing to rewrite comes back as the very bytes
// it was.
func TestRewriteModelFieldKeepsEveryOtherValue(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			"top level only", `{"model":"m#1","tools":[{"model":"m#1"}],"n":1.50,"seed":12345678901234567890}`,
			`{"model":"m","n":1.50,"seed":12345678901234567890,"tools":[{"model":"m#1"}]}`,
		},
		{"already the base", `{ "model" : "m" }`, `{ "model" : "m" }`},
		{"not an object", `["m#1"]`, `["m#1"]`},
		{"null", `null`, `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(rewriteModelField([]byte(tc.in), "m")); got != tc.want {
				t.Errorf("rewriteModelField(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestRebuildMultipartModelCopiesPartsRaw: a part is copied as it travelled,
// transfer encoding and all -- read decoded, a quoted-printable part would reach
// the child with its bytes decoded and its Content-Transfer-Encoding header
// gone -- while the model field is replaced, in its place.
func TestRebuildMultipartModelCopiesPartsRaw(t *testing.T) {
	body := crlf(`--b
Content-Disposition: form-data; name="prompt"
Content-Transfer-Encoding: quoted-printable

a=3Db
--b
Content-Disposition: form-data; name="model"

m#1
--b
Content-Disposition: form-data; name="image"; filename="in.png"
Content-Type: application/octet-stream

PNG-bytes
--b--
`)
	out, contentType, err := rebuildMultipartModel(body, "b", "m")
	if err != nil {
		t.Fatalf("rebuildMultipartModel: %v", err)
	}
	sent, got := formParts(t, "multipart/form-data; boundary=b", body), formParts(t, contentType, out)
	if len(got) != 3 || got[0].name != "prompt" || got[1].name != "model" || got[2].name != "image" {
		t.Fatalf("parts = %+v, want prompt, model, image in that order", got)
	}
	for _, i := range []int{0, 2} {
		if !bytes.Equal(got[i].data, sent[i].data) || fmt.Sprint(got[i].header) != fmt.Sprint(sent[i].header) {
			t.Errorf("part %s = %v %q, want it as sent: %v %q", got[i].name, got[i].header, got[i].data, sent[i].header, sent[i].data)
		}
	}
	if string(got[1].data) != "m" {
		t.Errorf("model = %q, want m", got[1].data)
	}
}
