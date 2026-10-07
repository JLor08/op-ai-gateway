// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAgentRouterOpts configures newFakeAgentRouter.
type fakeAgentRouterOpts struct {
	// models are the models the router manages. A model in running is
	// managed too.
	models []string
	// running are the models whose child runs when the router starts.
	running []string
	// coldDelay is how long a chat for a model that is not running waits
	// before its first data: the child's load. The model runs after it.
	coldDelay time.Duration
	// nCtx is the n_ctx GET /upstream/{model}/props reports for a running
	// model.
	nCtx int
}

// fakeAgentRequest is one request the fake agent router received: its
// method, its path, its Authorization header, and when it arrived.
type fakeAgentRequest struct {
	method, path, auth string
	at                 time.Time
}

// fakeAgentRouter is an httptest server that answers the gateway the way the
// agent router answers it (server-agent/internal/runtime/router.go):
//   - GET /running lists the running models in llama-swap's shape;
//   - POST /v1/chat/completions streams an answer for a managed model, after
//     coldDelay when the model is not running, and the final chunk carries
//     llama-server's timings;
//   - GET /upstream/{model}/props answers llama-server's /props with n_ctx
//     while the model runs, and 404 runtime.model_not_running while it does
//     not;
//   - everything else, including POST /api/models/unload/{model}, gets 404
//     runtime.model_not_managed: the router has no unload route.
//
// It records every request with its arrival time.
type fakeAgentRouter struct {
	t    *testing.T
	ts   *httptest.Server
	opts fakeAgentRouterOpts

	mu          sync.Mutex
	managed     map[string]bool
	running     map[string]bool
	runningFail bool
	log         []fakeAgentRequest
}

func newFakeAgentRouter(t *testing.T, opts fakeAgentRouterOpts) *fakeAgentRouter {
	t.Helper()
	f := &fakeAgentRouter{t: t, opts: opts, managed: map[string]bool{}, running: map[string]bool{}}
	for _, m := range opts.models {
		f.managed[m] = true
	}
	for _, m := range opts.running {
		f.managed[m] = true
		f.running[m] = true
	}
	f.ts = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.ts.Close)
	return f
}

// newFakeAgentProvider is the production provider shape for a server_agent
// application (cmd/gateway's providerClients): a Multiplexer that sends it to
// the OpenAI-compatible client.
func newFakeAgentProvider() provider.Client {
	return provider.NewMultiplexer(map[string]provider.Client{
		routing.ProviderServerAgent: provider.NewOpenAICompatibleClient(http.DefaultClient),
	}, nil)
}

// point aims server and app at the fake: the host, port and scheme of its
// httptest URL.
func (f *fakeAgentRouter) point(server *routing.AIServer, app *routing.Application) {
	f.t.Helper()
	u, err := url.Parse(f.ts.URL)
	if err != nil {
		f.t.Fatalf("fake agent router: parse %q: %v", f.ts.URL, err)
	}
	host, portText, err := net.SplitHostPort(u.Host)
	if err != nil {
		f.t.Fatalf("fake agent router: split %q: %v", u.Host, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		f.t.Fatalf("fake agent router: port %q: %v", portText, err)
	}
	server.Domain = host
	app.Port = port
	app.Scheme = u.Scheme
}

// requests returns every request received so far, in arrival order.
func (f *fakeAgentRouter) requests() []fakeAgentRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeAgentRequest(nil), f.log...)
}

// setRunning starts or stops model's child without a request.
func (f *fakeAgentRouter) setRunning(model string, running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.managed[model] = true
	f.running[model] = running
}

// failRunning makes GET /running answer 500 while on holds.
func (f *fakeAgentRouter) failRunning(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runningFail = on
}

func (f *fakeAgentRouter) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.log = append(f.log, fakeAgentRequest{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), at: time.Now()})
	f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/running":
		f.serveRunning(w)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/upstream/") && strings.HasSuffix(r.URL.Path, "/props"):
		f.serveProps(w, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/upstream/"), "/props"))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		f.serveChat(w, r)
	default:
		fakeAgentError(w, http.StatusNotFound, "runtime.model_not_managed", "request body does not name a managed model")
	}
}

func (f *fakeAgentRouter) serveRunning(w http.ResponseWriter) {
	f.mu.Lock()
	fail := f.runningFail
	var names []string
	for m, on := range f.running {
		if on {
			names = append(names, m)
		}
	}
	f.mu.Unlock()
	if fail {
		http.Error(w, "fake agent router: /running fails", http.StatusInternalServerError)
		return
	}
	slices.Sort(names)
	type entry struct {
		Model string `json:"model"`
		State string `json:"state"`
	}
	entries := make([]entry, 0, len(names))
	for _, n := range names {
		entries = append(entries, entry{Model: n, State: "ready"})
	}
	fakeAgentJSON(w, http.StatusOK, map[string]any{"running": entries})
}

func (f *fakeAgentRouter) serveProps(w http.ResponseWriter, model string) {
	f.mu.Lock()
	managed, running := f.managed[model], f.running[model]
	f.mu.Unlock()
	switch {
	case !managed:
		fakeAgentError(w, http.StatusNotFound, "runtime.model_not_managed", "no active launch spec for this model")
	case !running:
		fakeAgentError(w, http.StatusNotFound, "runtime.model_not_running", "model is managed but not running; this probe never starts a child")
	default:
		fakeAgentJSON(w, http.StatusOK, map[string]any{
			"default_generation_settings": map[string]any{"n_ctx": f.opts.nCtx},
			"model_path":                  "/models/" + model + ".gguf",
			"total_slots":                 1,
		})
	}
}

func (f *fakeAgentRouter) serveChat(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var peek struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)
	f.mu.Lock()
	managed, running := f.managed[peek.Model], f.running[peek.Model]
	f.mu.Unlock()
	if !managed {
		fakeAgentError(w, http.StatusNotFound, "runtime.model_not_managed", "request body does not name a managed model")
		return
	}
	if !running {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(f.opts.coldDelay):
		}
		f.setRunning(peek.Model, true)
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, frame := range []string{
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":20,"total_tokens":32},"timings":{"prompt_n":12,"prompt_per_second":900,"predicted_n":20,"predicted_per_second":42}}`,
		`[DONE]`,
	} {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func fakeAgentError(w http.ResponseWriter, status int, code, message string) {
	fakeAgentJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func fakeAgentJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// portalShapedAgentTarget is a server_agent target in the shape the portal
// stores: loaded_models_path, loaded_models_format and context_probe_path
// empty. Its mapping map1 serves qwen under the llama_cpp runtime spec rs_map1
// on server srv1, and it is aimed at f.
func portalShapedAgentTarget(f *fakeAgentRouter) benchmarkTarget {
	f.t.Helper()
	tgt := benchmarkTarget{
		server:  routing.AIServer{ID: "srv1", Name: "Host"},
		app:     routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderServerAgent, APIFlavors: []string{routing.APIFlavorOpenAI}, TimeoutMS: 30000, Status: routing.ServerStatusActive},
		mapping: routing.ModelMapping{ID: "map1", ApplicationID: "app1", GatewayModelName: "gw-qwen", AppModelName: "qwen", Status: routing.ServerStatusActive},
		spec:    routing.RuntimeSpec{ID: "rs_map1", MappingID: "map1", Enabled: true, Type: string(routing.RuntimeSpecTypeLlamaCpp), APIFlavors: []string{routing.APIFlavorOpenAI}},
	}
	f.point(&tgt.server, &tgt.app)
	return tgt
}

// fakeAgentPaths is "METHOD path" for each request, in arrival order.
func fakeAgentPaths(reqs []fakeAgentRequest) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.method+" "+r.path)
	}
	return out
}
