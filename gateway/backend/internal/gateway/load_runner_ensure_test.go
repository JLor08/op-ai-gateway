// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// ensureFakeProvider is the agent router's ensure route, for the load core's
// ensure branch. EnsureRuntimeModel answers errs in order, one entry per call
// (nil: the child is running), and repeats the last entry once they run out.
// With block set, every call past the errs entries instead holds until its
// context ends, the way the route holds a call while the child starts, and
// then returns blockErr, or, without one, the provider's own error for how the
// context ended. deadline is the last call's context deadline, deadlines
// every call's. CompleteStream counts its calls and fails: the ensure branch
// must never send a chat completion.
type ensureFakeProvider struct {
	errs      []error
	block     bool
	blockErr  error
	ensures   int
	streams   int
	models    []string
	tokens    []string
	deadline  time.Time
	deadlines []time.Time
	// loaded is what LoadedModels reports; a successful ensure adds its
	// model, so the resident probe and the reflection after a load see it.
	loaded []string
}

func (p *ensureFakeProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (p *ensureFakeProvider) CompleteStream(context.Context, routing.Target, inference.Request, provider.StreamEmit) error {
	p.streams++
	return errors.New("the ensure branch sent a chat completion")
}

func (p *ensureFakeProvider) EnsureRuntimeModel(ctx context.Context, target routing.Target) error {
	p.ensures++
	p.models = append(p.models, target.ProviderModel)
	if auth, ok := provider.UpstreamAuthFrom(ctx); ok {
		p.tokens = append(p.tokens, auth.Token)
	}
	if deadline, ok := ctx.Deadline(); ok {
		p.deadline = deadline
		p.deadlines = append(p.deadlines, deadline)
	}
	if p.block && p.ensures > len(p.errs) {
		<-ctx.Done()
		if p.blockErr != nil {
			return p.blockErr
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w: ensure route: %v", provider.ErrTimeout, ctx.Err())
		}
		return fmt.Errorf("%w: ensure route: %v", provider.ErrUnavailable, ctx.Err())
	}
	var err error
	if len(p.errs) > 0 {
		err = p.errs[min(p.ensures, len(p.errs))-1]
	}
	if err == nil {
		p.loaded = append(p.loaded, target.ProviderModel)
	}
	return err
}

func (p *ensureFakeProvider) LoadedModels(context.Context, routing.Target, string, string) ([]string, error) {
	return append([]string(nil), p.loaded...), nil
}

// ensureOnlyProvider can start a model through the ensure route and do
// nothing else: it is not a provider.StreamingClient.
type ensureOnlyProvider struct{ ensures int }

func (p *ensureOnlyProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (p *ensureOnlyProvider) EnsureRuntimeModel(context.Context, routing.Target) error {
	p.ensures++
	return nil
}

// routerErr is the provider's error for a router envelope: the router's code
// on the status error the provider maps the non-2xx onto.
func routerErr(code string, base error, status int) error {
	return &provider.RouterError{Code: code, Err: fmt.Errorf("%w: upstream status %d", base, status)}
}

// ensureTestTarget is an images-only agent child that loads through the
// ensure route: the target a Load starter hands the core for such a mapping.
func ensureTestTarget() benchmarkTarget {
	tgt := benchTestTarget()
	tgt.app.Type = routing.ProviderServerAgent
	tgt.spec = routing.RuntimeSpec{
		ID: "rspec1", MappingID: tgt.mapping.ID, Enabled: true,
		StartupTimeoutSeconds: 240, APIFlavors: []string{routing.APIFlavorOpenAIImages},
	}
	tgt.loadWithoutGenerating = true
	return tgt
}

// runLoadResult runs runLoadModel for tgt on a fresh reservation and returns
// the run's one result and its terminal error.
func runLoadResult(t *testing.T, srv *Server, tgt benchmarkTarget) (BenchmarkResult, string) {
	t.Helper()
	srv.Benchmarks = NewBenchmarkRegistry()
	run, ok := srv.Benchmarks.TryStart("srv1", "load", "load", 1, time.Now().UTC(), func() {})
	if !ok {
		t.Fatal("TryStart did not start")
	}
	srv.runLoadModel(context.Background(), run, "srv1", tgt)
	st := srv.Benchmarks.Status("srv1")
	if st.Running || len(st.Results) != 1 {
		t.Fatalf("status = %+v, want one result of a finished run", st)
	}
	if srv.Benchmarks.ServerBusy("srv1") {
		t.Fatal("ServerBusy true after the load, want the server freed")
	}
	return st.Results[0], st.Error
}

// TestRunLoadModelEnsuresAnImagesOnlyChildWithoutGenerating: an images-only
// agent child is started through the ensure route, never by a chat
// completion it cannot answer, and the run records it as loaded -- and the
// loaded set is still reflected afterwards.
func TestRunLoadModelEnsuresAnImagesOnlyChildWithoutGenerating(t *testing.T) {
	fake := &ensureFakeProvider{}
	loaded := NewLoadedModelRegistry()
	srv := &Server{Provider: fake, LoadedModels: loaded}
	tgt := ensureTestTarget()
	tgt.app.LoadedModelsPath = "/loaded"

	res, runErr := runLoadResult(t, srv, tgt)
	if !res.Loaded || res.Error != "" || runErr != "" {
		t.Fatalf("result = %+v, run error %q; want Loaded with no error", res, runErr)
	}
	if fake.streams != 0 {
		t.Fatalf("CompleteStream calls = %d, want 0 (an images-only child is not loaded by generating)", fake.streams)
	}
	if fake.ensures != 1 || len(fake.models) != 1 || fake.models[0] != "up-model" {
		t.Fatalf("ensure calls = %d for %v, want one for up-model", fake.ensures, fake.models)
	}
	if got := loaded.LoadedAppModels(tgt.app.ID, tgt.server.ID); len(got) != 1 || got[0] != "up-model" {
		t.Fatalf("LoadedAppModels = %v, want [up-model] reflected after the ensure", got)
	}
}

// TestEnsureLoadKeepsTheResidentShortCircuit: a child the loaded-models probe
// already reports resident is not ensured again.
func TestEnsureLoadKeepsTheResidentShortCircuit(t *testing.T) {
	fake := &ensureFakeProvider{loaded: []string{"up-model"}}
	srv := &Server{Provider: fake}
	tgt := ensureTestTarget()
	tgt.app.LoadedModelsPath = "/loaded"

	alreadyResident, probed, err := srv.ensureResidentForRun(context.Background(), tgt)
	if err != nil || !alreadyResident || !probed {
		t.Fatalf("ensureResidentForRun = (%v, %v, %v), want an answered already-resident", alreadyResident, probed, err)
	}
	if fake.ensures != 0 || fake.streams != 0 {
		t.Fatalf("ensure calls = %d, streams = %d; want neither for a resident model", fake.ensures, fake.streams)
	}
}

// TestEnsureLoadRetriesAnAdmission503: the agent answers 503 until the
// cleared force_stopped reaches it (the VRAM run's window), so a 503 is
// waited out within the loop's bound exactly as on the text branch.
func TestEnsureLoadRetriesAnAdmission503(t *testing.T) {
	blocked := routerErr("runtime.admission_blocked", provider.ErrUpstreamStarting, 503)
	fake := &ensureFakeProvider{errs: []error{blocked, blocked, nil}}
	srv := &Server{Provider: fake}
	tgt := ensureTestTarget()
	shortenColdLoadRetry(t, srv, &tgt, time.Millisecond, 2*time.Second)

	if _, err := ensureResidentWithin(t, srv, tgt, 5*time.Second); err != nil {
		t.Fatalf("ensureResidentForRun err = %v, want nil (retried past the 503s)", err)
	}
	if fake.ensures != 3 || fake.streams != 0 {
		t.Fatalf("ensure calls = %d, streams = %d; want 3 ensures (two 503s, then running) and no stream", fake.ensures, fake.streams)
	}
}

// TestEnsureLoadNamesTheRouterCode: a failed ensure reads
// "<router code>: <hint> (<provider text>)", one hint per router code, the
// start timeout's taken from the target's spec. The provider error stays on
// the chain, so a 503 is still retried and errors.Is still sees the class.
func TestEnsureLoadNamesTheRouterCode(t *testing.T) {
	for _, tc := range []struct {
		code    string
		base    error
		status  int
		want    string
		retried bool
	}{
		{
			code: "runtime.start_timeout", base: provider.ErrUnavailable, status: 504,
			want: "runtime.start_timeout: not healthy within startup_timeout_seconds (240 s) (provider.unavailable: upstream status 504)",
		},
		{
			code: "runtime.start_failed", base: provider.ErrUnavailable, status: 502,
			want: "runtime.start_failed: the process exited or failed to start; see the agent log (provider.unavailable: upstream status 502)",
		},
		{
			code: "runtime.not_permitted", base: provider.ErrUnavailable, status: 502,
			want: "runtime.not_permitted: the agent refused the binary or its arguments (provider.unavailable: upstream status 502)",
		},
		{
			code: "runtime.admission_blocked", base: provider.ErrUpstreamStarting, status: 503,
			want:    "runtime.admission_blocked: the agent did not admit the start (a force-stopped spec, a pinned sibling or no free VRAM) (provider.unavailable (upstream starting): upstream status 503)",
			retried: true,
		},
		{
			code: "runtime.model_not_managed", base: provider.ErrUnavailable, status: 404,
			want: "runtime.model_not_managed: the agent does not manage this model (provider.unavailable: upstream status 404)",
		},
		{
			code: "runtime.upstream_gone", base: provider.ErrUnavailable, status: 502,
			want: "runtime.upstream_gone: the agent stopped while starting the child (provider.unavailable: upstream status 502)",
		},
	} {
		t.Run(tc.code, func(t *testing.T) {
			fake := &ensureFakeProvider{errs: []error{routerErr(tc.code, tc.base, tc.status)}}
			srv := &Server{Provider: fake}
			tgt := ensureTestTarget()
			shortenColdLoadRetry(t, srv, &tgt, time.Millisecond, 100*time.Millisecond)

			_, err := ensureResidentWithin(t, srv, tgt, 5*time.Second)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v\nwant %s", err, tc.want)
			}
			if !errors.Is(err, tc.base) {
				t.Fatalf("err = %v, want the provider's %v on the chain", err, tc.base)
			}
			var routerError *provider.RouterError
			if !errors.As(err, &routerError) || routerError.Code != tc.code {
				t.Fatalf("err = %v, want the router's %s on the chain", err, tc.code)
			}
			if retried := fake.ensures > 1; retried != tc.retried {
				t.Fatalf("ensure calls = %d, want retried = %v", fake.ensures, tc.retried)
			}
		})
	}
}

// TestRunLoadModelRecordsTheEnsureFailure: the run records the worded text in
// both results[].error and the run's own error.
func TestRunLoadModelRecordsTheEnsureFailure(t *testing.T) {
	fake := &ensureFakeProvider{errs: []error{routerErr("runtime.start_failed", provider.ErrUnavailable, 502)}}
	srv := &Server{Provider: fake}
	const want = "runtime.start_failed: the process exited or failed to start; see the agent log (provider.unavailable: upstream status 502)"

	res, runErr := runLoadResult(t, srv, ensureTestTarget())
	if res.Loaded || res.Error != want || runErr != want {
		t.Fatalf("result = %+v, run error %q; want not loaded, with %q in both", res, runErr, want)
	}
}

// TestEnsureLoadWritesErrorsWithoutARouterCodeInFull: an invalid 2xx gets its
// own sentence, and every other error without a router code -- a refused
// credential, a transport error, a code the table does not know -- is the
// provider's own text, unchanged.
func TestEnsureLoadWritesErrorsWithoutARouterCodeInFull(t *testing.T) {
	unknown := routerErr("runtime.something_new", provider.ErrUnavailable, 500)
	for _, tc := range []struct {
		name  string
		err   error
		want  string
		class error
	}{
		{
			name: "an invalid 2xx", err: fmt.Errorf("%w: ensure route answered status %q", provider.ErrInvalidResponse, "starting"),
			want: `provider.invalid_response: the agent's ensure route answered without "running"`, class: provider.ErrInvalidResponse,
		},
		{
			name: "a refused credential", err: fmt.Errorf("%w: upstream status 401", provider.ErrAuthRejected),
			want: "provider.unavailable (upstream rejected the credential): upstream status 401", class: provider.ErrAuthRejected,
		},
		{
			name: "a transport error", err: fmt.Errorf("%w: Post \"http://host.example.test:8100/ensure/up-model\": connection refused", provider.ErrUnavailable),
			want: "provider.unavailable: Post \"http://host.example.test:8100/ensure/up-model\": connection refused", class: provider.ErrUnavailable,
		},
		{name: "an unknown router code", err: unknown, want: unknown.Error(), class: provider.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &ensureFakeProvider{errs: []error{tc.err}}
			srv := &Server{Provider: fake}
			tgt := ensureTestTarget()
			shortenColdLoadRetry(t, srv, &tgt, time.Millisecond, 2*time.Second)

			_, err := ensureResidentWithin(t, srv, tgt, 5*time.Second)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v\nwant %s", err, tc.want)
			}
			if !errors.Is(err, tc.class) {
				t.Fatalf("err = %v, want %v on the chain", err, tc.class)
			}
			if fake.ensures != 1 {
				t.Fatalf("ensure calls = %d, want 1 (none of these is retried)", fake.ensures)
			}
		})
	}
}

// TestEnsureLoadEndsAtTheLoopsOwnDeadline: an ensure call the route holds
// past the load loop's bound is ended by the loop's own deadline, and the run
// says so in its own words -- not a stream watchdog text, since no stream ran.
func TestEnsureLoadEndsAtTheLoopsOwnDeadline(t *testing.T) {
	const bound = 200 * time.Millisecond
	fake := &ensureFakeProvider{block: true}
	srv := &Server{Provider: fake}
	tgt := ensureTestTarget()
	shortenColdLoadRetry(t, srv, &tgt, time.Millisecond, bound)
	start := time.Now()

	elapsed, err := ensureResidentWithin(t, srv, tgt, 5*time.Second)
	const want = "provider.timeout: not running within 200ms (the larger of 5 min and the stream budget); a start already under way may still come up"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant %s", err, want)
	}
	if !errors.Is(err, provider.ErrTimeout) {
		t.Fatalf("err = %v, want the provider's ErrTimeout on the chain", err)
	}
	if fake.ensures != 1 || fake.streams != 0 {
		t.Fatalf("ensure calls = %d, streams = %d; want one ensure and no stream", fake.ensures, fake.streams)
	}
	if fake.deadline.IsZero() || fake.deadline.Before(start.Add(bound)) || fake.deadline.After(start.Add(bound+time.Second)) {
		t.Fatalf("ensure deadline = %v, want the loop's bound, about %v after %v", fake.deadline, bound, start)
	}
	if elapsed < bound-50*time.Millisecond {
		t.Fatalf("elapsed = %v, want about the %v bound", elapsed, bound)
	}
}

// TestEnsureLoadAttemptsShareTheLoopsOneDeadline tells the loop's deadline
// apart from the bounds it is computed from: a 50ms 503 wait and a 400ms
// timeout_ms make a 400ms bound. The route answers 503 three times and then
// holds the call, so every attempt must run under the one deadline the loop
// set when it began -- not a fresh one per attempt, and not the 503 wait --
// and the error names the loop's own bound.
func TestEnsureLoadAttemptsShareTheLoopsOneDeadline(t *testing.T) {
	const bound = 400 * time.Millisecond
	admission := routerErr("runtime.admission_blocked", provider.ErrUpstreamStarting, 503)
	fake := &ensureFakeProvider{errs: []error{admission, admission, admission}, block: true}
	srv := &Server{Provider: fake}
	tgt := ensureTestTarget()
	shortenColdLoadRetry(t, srv, &tgt, time.Millisecond, 50*time.Millisecond)
	tgt.app.TimeoutMS = int(bound.Milliseconds())
	start := time.Now()

	_, err := ensureResidentWithin(t, srv, tgt, 5*time.Second)
	const want = "provider.timeout: not running within 400ms (the larger of 5 min and the stream budget); a start already under way may still come up"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant %s", err, want)
	}
	if fake.ensures != 4 || len(fake.deadlines) != 4 {
		t.Fatalf("ensure calls = %d with %d deadlines, want 4 of each: three 503s, then the call the deadline ends", fake.ensures, len(fake.deadlines))
	}
	for i, d := range fake.deadlines {
		if !d.Equal(fake.deadlines[0]) {
			t.Fatalf("attempt %d deadline = %v, want the loop's one deadline %v: an attempt must not start a bound of its own", i+1, d, fake.deadlines[0])
		}
	}
	if d := fake.deadlines[0]; d.Before(start.Add(bound)) {
		t.Fatalf("deadline = %v after the loop began, want at least the %v bound", d.Sub(start), bound)
	}
}

// TestEnsureLoadKeepsARouterCodeThatLandsAtTheDeadline: a 503 the route
// answers just as the loop's own deadline ends the call is still the router's
// failure, worded with its code, and not the loop-deadline sentence: only a
// provider timeout at the loop's deadline is the loop's own timeout.
func TestEnsureLoadKeepsARouterCodeThatLandsAtTheDeadline(t *testing.T) {
	fake := &ensureFakeProvider{block: true, blockErr: routerErr("runtime.admission_blocked", provider.ErrUpstreamStarting, 503)}
	srv := &Server{Provider: fake}
	tgt := ensureTestTarget()
	shortenColdLoadRetry(t, srv, &tgt, time.Millisecond, 200*time.Millisecond)

	_, err := ensureResidentWithin(t, srv, tgt, 5*time.Second)
	const want = "runtime.admission_blocked: the agent did not admit the start (a force-stopped spec, a pinned sibling or no free VRAM) (provider.unavailable (upstream starting): upstream status 503)"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant %s", err, want)
	}
	if fake.ensures != 1 {
		t.Fatalf("ensure calls = %d, want 1 (the 503 ended the loop's bound)", fake.ensures)
	}
}

// TestEnsureLoadKeepsTheProvidersTimeoutForARunDeadline: a deadline of the run
// itself, shorter than the loop's bound, is not the loop's own deadline, so
// the provider's timeout keeps its own text instead of naming the bound.
func TestEnsureLoadKeepsTheProvidersTimeoutForARunDeadline(t *testing.T) {
	fake := &ensureFakeProvider{block: true}
	srv := &Server{Provider: fake}
	tgt := ensureTestTarget()
	shortenColdLoadRetry(t, srv, &tgt, time.Millisecond, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, _, err := srv.ensureResidentForRun(ctx, tgt)
	const want = "provider.timeout: ensure route: context deadline exceeded"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant %s", err, want)
	}
	if !errors.Is(err, provider.ErrTimeout) {
		t.Fatalf("err = %v, want the provider's ErrTimeout on the chain", err)
	}
}

// TestEnsureLoadPassesAParentCancellationThrough: a run cancelled while the
// route holds its call is not the loop's deadline, so it keeps the
// provider's own text.
func TestEnsureLoadPassesAParentCancellationThrough(t *testing.T) {
	fake := &ensureFakeProvider{block: true}
	srv := &Server{Provider: fake}
	tgt := ensureTestTarget()
	shortenColdLoadRetry(t, srv, &tgt, time.Millisecond, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(50*time.Millisecond, cancel)

	_, _, err := srv.ensureResidentForRun(ctx, tgt)
	const want = "provider.unavailable: ensure route: context canceled"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant %s", err, want)
	}
	if errors.Is(err, provider.ErrTimeout) {
		t.Fatalf("err = %v, want no ErrTimeout for a cancelled run", err)
	}
}

// TestEnsureLoadCarriesTheUpstreamCredential: the ensure call carries the
// application's upstream credential, like every provider request.
func TestEnsureLoadCarriesTheUpstreamCredential(t *testing.T) {
	fake := &ensureFakeProvider{}
	srv := &Server{Provider: fake}
	tgt := ensureTestTarget()
	tgt.app.APIToken = "plain:ensure-secret"

	if _, _, err := srv.ensureResidentForRun(context.Background(), tgt); err != nil {
		t.Fatalf("ensureResidentForRun err = %v", err)
	}
	if len(fake.tokens) != 1 || fake.tokens[0] != "ensure-secret" {
		t.Fatalf("credentials seen = %v, want the application's token on the ensure call", fake.tokens)
	}
}

// heldAnswerTransport is an agent router whose ensure route answers 200 and
// then holds the body. The first read of that body cancels the run, and the
// read ends with the request's cancellation, as a real transport's does.
type heldAnswerTransport struct{ cancelRun context.CancelFunc }

func (h heldAnswerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body := heldAnswerBody{ctx: req.Context(), cancelRun: h.cancelRun}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body, Request: req}, nil
}

type heldAnswerBody struct {
	ctx       context.Context
	cancelRun context.CancelFunc
}

func (b heldAnswerBody) Read([]byte) (int, error) {
	b.cancelRun()
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (heldAnswerBody) Close() error { return nil }

// TestEnsureLoadReportsACancellationDuringTheAnswerAsACancellation: a run
// cancelled while the provider reads the ensure route's 2xx answer reads as
// the provider's cancellation, never as an answer without "running".
func TestEnsureLoadReportsACancellationDuringTheAnswerAsACancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := provider.NewOpenAICompatibleClient(&http.Client{Transport: heldAnswerTransport{cancelRun: cancel}})
	srv := &Server{Provider: client}

	_, _, err := srv.ensureResidentForRun(ctx, ensureTestTarget())
	const want = "provider.unavailable: read ensure answer: context canceled"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant %s", err, want)
	}
	if errors.Is(err, provider.ErrInvalidResponse) {
		t.Fatalf("err = %v, want no ErrInvalidResponse for a cancelled run", err)
	}
}

// TestEnsureLoadAssertsOnlyTheCapabilityItUses: the ensure branch needs a
// provider.RuntimeEnsurer and no StreamingClient, and a provider without the
// ensure call gets its own error instead of a chat completion.
func TestEnsureLoadAssertsOnlyTheCapabilityItUses(t *testing.T) {
	ensureOnly := &ensureOnlyProvider{}
	srv := &Server{Provider: ensureOnly}
	if _, _, err := srv.ensureResidentForRun(context.Background(), ensureTestTarget()); err != nil {
		t.Fatalf("ensureResidentForRun err = %v, want nil (the ensure branch needs no StreamingClient)", err)
	}
	if ensureOnly.ensures != 1 {
		t.Fatalf("ensure calls = %d, want 1", ensureOnly.ensures)
	}

	streamer := &unavailableThenOKProvider{}
	srv = &Server{Provider: streamer}
	if _, _, err := srv.ensureResidentForRun(context.Background(), ensureTestTarget()); !errors.Is(err, errBenchmarkNoRuntimeEnsure) {
		t.Fatalf("err = %v, want errBenchmarkNoRuntimeEnsure", err)
	}
	if streamer.calls != 0 {
		t.Fatalf("CompleteStream calls = %d, want 0 (no fallback to a chat Load)", streamer.calls)
	}
}
