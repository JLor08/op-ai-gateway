// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newImageRunTestServer is the union of newRunTestServerWithProvider (the run
// executor's loopback wiring, chat store, cipher, ChatRuns and selfBaseURL) and
// newImageCapableTestServer's seeding (an image:yes mapping reachable at a real
// httptest address). Neither existing helper can drive an image run on its own.
//
// The provider is the real OpenAI-compatible HTTP client, not provider.Mock:
// the images relay goes out over proxyNative and must actually reach upstream.
//
// ApplicationEndpoint builds the origin from the SERVER's Domain plus the
// APPLICATION's Scheme/Port, so upstreamURL has to be decomposed into those
// fields -- the same requirement newImageCapableTestServer documents.
//
// The directory is returned as well (newImageCapableLoopbackTestServer's own
// shape, for the same reason): a run-as attribution test has to seed a token
// owned by the chat's user, and the directory is the only handle on the store
// AuthorizeRunAsToken reads.
//
// APIFlavors carries APIFlavorOpenAIImages alongside APIFlavorOpenAI: since
// NormalizeAPIFlavor no longer folds openai_images into openai, an application
// must opt into the images flavor explicitly to be an image candidate at all.
func newImageRunTestServer(t *testing.T, upstreamURL string) (*Server, *portal.MemoryDirectory, auth.Token, string) {
	t.Helper()
	return newImageRunTestServerWithChats(t, upstreamURL, store.NewMemoryChatStore(0))
}

// newImageRunTestServerWithChats is newImageRunTestServer over a chat store the
// test holds, for the tests that look at the chat_blobs rows a committed image
// leaves behind -- the transcript itself only carries a reference to them.
func newImageRunTestServerWithChats(t *testing.T, upstreamURL string, chats portal.ChatStore) (*Server, *portal.MemoryDirectory, auth.Token, string) {
	t.Helper()
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	tokens := auth.NewTokenStore()
	tokens.AddPlainToken(auth.Token{
		ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token",
		Active: true, Scopes: []string{"gateway:use"},
	}, "dev-secret")
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	directory := portal.NewMemoryDirectory(auth.NewTokenStore())
	directory.AddUser(store.User{
		ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User",
		Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de",
		CreatedAt: now, UpdatedAt: now,
	})
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()

	ctx := context.Background()
	up, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", upstreamURL, err)
	}
	port, err := strconv.Atoi(up.Port())
	if err != nil {
		t.Fatalf("upstream port %q: %v", up.Port(), err)
	}
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-sd-turbo", Name: "SD Turbo Upstream", Domain: up.Hostname(), Provider: routing.ProviderVLLM, Endpoint: upstreamURL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-sd-turbo", ServerID: "srv-sd-turbo", Type: routing.ProviderVLLM, Port: port, Scheme: up.Scheme, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-sd-turbo", ApplicationID: "app-sd-turbo", GatewayModelName: "sd-turbo", AppModelName: "sd-turbo", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := routeStore.UpsertMappingCapabilities(ctx, "route-sd-turbo", []routing.CapabilityRow{{Capability: routing.CapabilityImage, Verdict: routing.CapabilityYes, Source: "manual", CheckedAt: now}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-sd-turbo", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}

	svc := portal.NewService(portal.ServiceDeps{
		Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore,
		Clock: func() time.Time { return now }, ModelLister: provider.NewMock(),
		Chats: chats, Cipher: cipher,
	})
	srv := New(ServerDeps{
		Tokens:             tokens,
		Usage:              recorder,
		Provider:           provider.NewOpenAICompatibleClient(http.DefaultClient),
		Routes:             routeStore,
		Portal:             svc,
		Captures:           &fakeCaptureStore{},
		Cipher:             cipher,
		InternalAuthSecret: "test-internal-secret",
		Users:              directory,
		ChatRuns:           NewChatRunRegistry(5),
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	srv.selfBaseURL = ts.URL

	owner := auth.Token{UserID: "usr_dev", Scopes: []string{"gateway:use"}}
	created, err := svc.CreateChat(ctx, owner, portal.CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[{"id":"u","role":"user","content":"a cat"}]}`),
	})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	return srv, directory, owner, created.ID
}

// storedImageBlob is a chat_blobs row a transcript's image part points at, with
// its payload opened (the rows are sealed at rest).
type storedImageBlob struct {
	store.ChatBlob
	plain []byte
}

// storedImageBlobs resolves every /api/portal/chat-blobs/{id} image url of a
// stored transcript, in order, against the chat store the run committed to. A
// committed image turn holds those references rather than the bytes.
func storedImageBlobs(t *testing.T, chats portal.ChatStore, content json.RawMessage) []storedImageBlob {
	t.Helper()
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	var doc struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		t.Fatalf("transcript is not JSON: %v", err)
	}
	var out []storedImageBlob
	for _, m := range doc.Messages {
		var parts []struct {
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		if json.Unmarshal(m.Content, &parts) != nil {
			continue
		}
		for _, p := range parts {
			if p.ImageURL == nil {
				continue
			}
			id, ok := strings.CutPrefix(p.ImageURL.URL, "/api/portal/chat-blobs/")
			if !ok {
				t.Fatalf("image url %.80q is not a chat-blobs reference", p.ImageURL.URL)
			}
			row, err := chats.ChatBlobByID(context.Background(), id)
			if err != nil {
				t.Fatalf("ChatBlobByID(%s): %v", id, err)
			}
			plain := row.Blob
			if row.KeyVersion > 0 {
				if plain, err = cipher.Open(row.Blob); err != nil {
					t.Fatalf("open blob %s: %v", id, err)
				}
			}
			out = append(out, storedImageBlob{ChatBlob: row, plain: plain})
		}
	}
	return out
}

// imageRunPrep is the prepared run every image test below starts from: the
// pinned image kind, the image-capable model, and a history whose last user
// message carries the prompt.
//
// The history is NOT optional decoration. The images request body's prompt is
// the last user message's text, so a prep without one describes a run that
// cannot legally reach the endpoint at all (validateImagesRequest answers
// images.prompt_required) -- see TestImageRunKeepsAGatewaySideErrorCode, which
// is the one test that deliberately omits it.
func imageRunPrep() PrepareRunResult {
	return PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"a cat"`)}},
		Settings: portal.ChatRunSettings{Model: "sd-turbo", Kind: chatRunKindImage},
	}
}

// With no image settings the body is exactly the three fixed fields: an image
// thread that set no knob must send nothing extra, so sd-server stays on its own
// defaults and the body is byte-for-byte what it was before this feature.
func TestBuildImagesBodyWithoutSettingsIsTheThreeFixedFields(t *testing.T) {
	raw, err := buildImagesBody(imageRunPrep())
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if len(body) != 3 || body["model"] != "sd-turbo" || body["prompt"] != "a cat" || body["response_format"] != "b64_json" {
		t.Fatalf("want exactly model/prompt/response_format, got: %s", raw)
	}
}

// The set image knobs are forwarded; an unset knob is omitted (sd-server keeps
// its default) rather than sent as an inert zero.
func TestBuildImagesBodyForwardsOnlyTheSetImageSettings(t *testing.T) {
	compression := 80
	prep := imageRunPrep()
	prep.Settings.Image = &portal.ChatImageSettings{
		Size:              "1024x1024",
		N:                 3,
		OutputFormat:      "webp",
		OutputCompression: &compression,
	}
	raw, err := buildImagesBody(prep)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if body["model"] != "sd-turbo" || body["prompt"] != "a cat" || body["response_format"] != "b64_json" {
		t.Fatalf("base fields changed: %s", raw)
	}
	if body["size"] != "1024x1024" {
		t.Fatalf("size = %v, want 1024x1024: %s", body["size"], raw)
	}
	if n, _ := body["n"].(float64); n != 3 {
		t.Fatalf("n = %v, want 3: %s", body["n"], raw)
	}
	if body["output_format"] != "webp" {
		t.Fatalf("output_format = %v, want webp: %s", body["output_format"], raw)
	}
	if c, _ := body["output_compression"].(float64); c != 80 {
		t.Fatalf("output_compression = %v, want 80: %s", body["output_compression"], raw)
	}

	// Unset knobs are omitted: an image blob carrying only a size sends only size.
	prep.Settings.Image = &portal.ChatImageSettings{Size: "512x512"}
	raw, err = buildImagesBody(prep)
	if err != nil {
		t.Fatal(err)
	}
	body = map[string]any{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if _, ok := body["n"]; ok {
		t.Fatalf("an unset n must be omitted, not sent: %s", raw)
	}
	if _, ok := body["output_format"]; ok {
		t.Fatalf("an unset output_format must be omitted: %s", raw)
	}
	if _, ok := body["output_compression"]; ok {
		t.Fatalf("an unset output_compression must be omitted: %s", raw)
	}
	if body["size"] != "512x512" {
		t.Fatalf("size = %v, want 512x512: %s", body["size"], raw)
	}
}

// promptOf decodes buildImagesBody's output and returns its prompt string.
func promptOf(t *testing.T, raw []byte) string {
	t.Helper()
	var body struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return body.Prompt
}

// The advanced knobs ride in a <sd_cpp_extra_args> block appended to the prompt,
// mapped to sd-server's native schema (sampler -> sample_method, cfg ->
// guidance.txt_cfg, steps -> sample_steps; negative_prompt/seed/clip_skip
// top-level). Only the set knobs appear.
func TestBuildImagesBodyAppendsExtraArgsBlock(t *testing.T) {
	steps, clip, seed := 28, 2, 42
	cfg := 7.5
	prep := imageRunPrep()
	prep.Settings.Image = &portal.ChatImageSettings{
		NegativePrompt: "blurry, low quality",
		Sampler:        "euler_a",
		Scheduler:      "karras",
		Steps:          &steps,
		CFG:            &cfg,
		Seed:           &seed,
		ClipSkip:       &clip,
	}
	raw, err := buildImagesBody(prep)
	if err != nil {
		t.Fatal(err)
	}
	prompt := promptOf(t, raw)
	const openTag, closeTag = "<sd_cpp_extra_args>", "</sd_cpp_extra_args>"
	start := strings.Index(prompt, openTag)
	end := strings.Index(prompt, closeTag)
	if start < 0 || end < 0 {
		t.Fatalf("no extra_args block in prompt: %q", prompt)
	}
	if !strings.HasPrefix(prompt, "a cat") {
		t.Fatalf("the user prompt must come first: %q", prompt)
	}
	payload := prompt[start+len(openTag) : end]
	var got struct {
		NegativePrompt string `json:"negative_prompt"`
		ClipSkip       int    `json:"clip_skip"`
		Seed           int    `json:"seed"`
		SampleParams   struct {
			SampleSteps  int    `json:"sample_steps"`
			SampleMethod string `json:"sample_method"`
			Scheduler    string `json:"scheduler"`
			Guidance     struct {
				TxtCFG float64 `json:"txt_cfg"`
			} `json:"guidance"`
		} `json:"sample_params"`
	}
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("extra_args payload is not valid JSON (%v): %q", err, payload)
	}
	if got.NegativePrompt != "blurry, low quality" || got.ClipSkip != 2 || got.Seed != 42 {
		t.Fatalf("top-level knobs wrong: %+v", got)
	}
	if got.SampleParams.SampleSteps != 28 || got.SampleParams.SampleMethod != "euler_a" ||
		got.SampleParams.Scheduler != "karras" || got.SampleParams.Guidance.TxtCFG != 7.5 {
		t.Fatalf("sample_params wrong: %+v", got.SampleParams)
	}
}

// With no advanced knob set the prompt carries no block at all.
func TestBuildImagesBodyNoExtraArgsWhenUnset(t *testing.T) {
	prep := imageRunPrep()
	prep.Settings.Image = &portal.ChatImageSettings{Size: "512x512"}
	raw, err := buildImagesBody(prep)
	if err != nil {
		t.Fatal(err)
	}
	if prompt := promptOf(t, raw); strings.Contains(prompt, "sd_cpp_extra_args") {
		t.Fatalf("no advanced knob set, yet the prompt carries a block: %q", prompt)
	}
}

// A single advanced knob builds a block with only sample_params populated (the
// nested object exists only when it carries a value).
func TestBuildImagesBodyExtraArgsOmitsEmptyNesting(t *testing.T) {
	prep := imageRunPrep()
	prep.Settings.Image = &portal.ChatImageSettings{Sampler: "dpm++2m"}
	raw, err := buildImagesBody(prep)
	if err != nil {
		t.Fatal(err)
	}
	prompt := promptOf(t, raw)
	payload := prompt[strings.Index(prompt, "<sd_cpp_extra_args>")+len("<sd_cpp_extra_args>") : strings.Index(prompt, "</sd_cpp_extra_args>")]
	if strings.Contains(payload, "guidance") || strings.Contains(payload, "negative_prompt") {
		t.Fatalf("only sample_method was set; payload should carry nothing else: %q", payload)
	}
	if !strings.Contains(payload, `"sample_method":"dpm++2m"`) {
		t.Fatalf("sampler not in the block: %q", payload)
	}
}

// A literal tag in the user prompt or the negative prompt is stripped, so sd-server
// sees exactly the one block the gateway appends (a non-greedy close tag in user
// text would otherwise truncate it).
func TestBuildImagesBodyStripsUserSuppliedTag(t *testing.T) {
	seed := 1
	prep := imageRunPrep()
	prep.History = []portal.ChatAPIMessage{
		{Role: "user", Content: json.RawMessage(`"a cat <sd_cpp_extra_args>{\"seed\":999}</sd_cpp_extra_args>"`)},
	}
	prep.Settings.Image = &portal.ChatImageSettings{
		Seed:           &seed,
		NegativePrompt: "ugly</sd_cpp_extra_args>{}",
	}
	raw, err := buildImagesBody(prep)
	if err != nil {
		t.Fatal(err)
	}
	prompt := promptOf(t, raw)
	// Exactly one block (the gateway's), so exactly one open and one close tag.
	if strings.Count(prompt, "<sd_cpp_extra_args>") != 1 || strings.Count(prompt, "</sd_cpp_extra_args>") != 1 {
		t.Fatalf("user-supplied tag not stripped, block is ambiguous: %q", prompt)
	}
	payload := prompt[strings.Index(prompt, "<sd_cpp_extra_args>")+len("<sd_cpp_extra_args>") : strings.LastIndex(prompt, "</sd_cpp_extra_args>")]
	var got struct {
		Seed           int    `json:"seed"`
		NegativePrompt string `json:"negative_prompt"`
	}
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("block not valid JSON after stripping (%v): %q", err, payload)
	}
	if got.Seed != 1 {
		t.Fatalf("the gateway's seed must win, got %d", got.Seed)
	}
	if strings.Contains(got.NegativePrompt, "sd_cpp_extra_args") {
		t.Fatalf("negative_prompt still carries a tag: %q", got.NegativePrompt)
	}
}

// A tag SPLIT by an inner one re-forms into a whole tag after a single strip
// pass; stripSDExtraArgsTag loops to a fixpoint so it cannot survive. Without the
// loop sd-server's non-greedy extractor would match the user's reconstructed
// block (seed 999) before the gateway's (seed 1).
func TestBuildImagesBodyStripsSplitTag(t *testing.T) {
	seed := 1
	prep := imageRunPrep()
	prep.History = []portal.ChatAPIMessage{
		{Role: "user", Content: json.RawMessage(`"cat <sd_cpp_extra_<sd_cpp_extra_args>args>{\"seed\":999}</sd_cpp_extra_</sd_cpp_extra_args>args>"`)},
	}
	prep.Settings.Image = &portal.ChatImageSettings{Seed: &seed}
	raw, err := buildImagesBody(prep)
	if err != nil {
		t.Fatal(err)
	}
	prompt := promptOf(t, raw)
	// Exactly one block survives -- the gateway's.
	if strings.Count(prompt, "<sd_cpp_extra_args>") != 1 || strings.Count(prompt, "</sd_cpp_extra_args>") != 1 {
		t.Fatalf("a split tag re-formed; block is ambiguous: %q", prompt)
	}
	// What sd-server's non-greedy extractor would parse is the FIRST block.
	first := regexp.MustCompile(`(?s)<sd_cpp_extra_args>(.*?)</sd_cpp_extra_args>`).FindStringSubmatch(prompt)
	if first == nil {
		t.Fatalf("no block: %q", prompt)
	}
	var got struct {
		Seed int `json:"seed"`
	}
	if err := json.Unmarshal([]byte(first[1]), &got); err != nil {
		t.Fatalf("block not valid JSON (%v): %q", err, first[1])
	}
	if got.Seed != 1 {
		t.Fatalf("the gateway's seed must win, got %d -- a reconstructed user block leaked through", got.Seed)
	}
}

// An explicit 0 seed/clip_skip is a real value, not "unset": it must appear in
// the block and not be swallowed by the empty-object check.
func TestBuildSDExtraArgsEmitsExplicitZero(t *testing.T) {
	zero := 0
	got := buildSDExtraArgs(&portal.ChatImageSettings{Seed: &zero, ClipSkip: &zero})
	if !strings.Contains(got, `"seed":0`) || !strings.Contains(got, `"clip_skip":0`) {
		t.Fatalf("explicit 0 knobs were dropped: %q", got)
	}
}

// runError reads a terminal run's error message the way the existing run tests
// do: through a subscribe() snapshot rather than the mu-guarded field.
func runError(t *testing.T, run *ChatRun) string {
	t.Helper()
	snap, _, unsub := run.subscribe()
	unsub()
	return snap.Err
}

// capturedUpstream is what the stub upstream saw, handed back over a buffered
// channel so the assertions run on the test's own goroutine (the handler runs
// on the httptest server's). path carries r.URL.Path so a test can assert
// WHICH endpoint the request actually reached, not just that some request
// arrived -- see TestImageRunServerOverrideReachesTheImagesHop, which needs
// to distinguish a request that reached this stub's /v1/images/generations
// from one that never left the chat hop.
type capturedUpstream struct {
	body []byte
	path string
}

// imagesUpstream is a stub /v1/images/generations backend: it answers status
// with body, and reports the request it received on the returned channel.
func imagesUpstream(t *testing.T, status int, body string) (*httptest.Server, <-chan capturedUpstream) {
	t.Helper()
	seen := make(chan capturedUpstream, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		select {
		case seen <- capturedUpstream{body: raw, path: r.URL.Path}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

// stalledImagesUpstream is an images backend that never finishes answering,
// for the run-lifecycle tests that need a run to still be in flight -- or to
// be ended by its own deadline rather than by an upstream reply.
//
// firstChunk, when non-empty, is written together with the 200 status line and
// flushed before the stall, so the stall sits in the run's BODY read rather
// than in its round trip: the gateway's copier forwards and flushes each
// upstream chunk, and that first chunk is what makes the run's own
// http.Client.Do return. An empty firstChunk stalls before the status line,
// where Do itself is what the deadline interrupts. Those are the two distinct
// places a deadline can land on this path.
//
// The returned release is idempotent and MUST be called by the test before it
// returns (a defer, which runs before every t.Cleanup): closing an httptest
// server waits for its in-flight handlers, and the loopback server's own
// cleanup -- registered later by newImageRunTestServer, so it runs FIRST --
// waits for a request that is itself waiting on this one.
func stalledImagesUpstream(t *testing.T, firstChunk string) (*httptest.Server, func()) {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if firstChunk != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(firstChunk))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		<-release
	}))
	t.Cleanup(srv.Close)
	return srv, func() { once.Do(func() { close(release) }) }
}

// An image run ends with a committed assistant turn holding one image_url part
// per upstream data[] item, with the MIME taken from output_format rather than
// assumed.
//
// The turn is stored as image_url parts that REFERENCE chat_blobs rows (the
// commit moves the bytes out of the document -- see portal.extractChatBlobs),
// so the media type and each item's own bytes are asserted on the rows.
func TestImageRunCommitsAnImagePartPerDataItem(t *testing.T) {
	upstream, _ := imagesUpstream(t, http.StatusOK, `{"created":1,"output_format":"png","data":[{"b64_json":"AA=="},{"b64_json":"BB=="}]}`)

	chats := store.NewMemoryChatStore(0)
	srv, _, owner, chatID := newImageRunTestServerWithChats(t, upstream.URL, chats)
	run, err := srv.startChatRun(owner, chatID, imageRunPrep())
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	if got := run.statusValue(); got != "completed" {
		t.Fatalf("status = %q (err %q), want completed", got, runError(t, run))
	}
	got, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	// One part per data[] item -- the endpoint's own counter takes the billed
	// quantity from the response for exactly this reason, so the UI must not
	// assume a single image.
	if n := strings.Count(string(got.Content), `"type":"image_url"`); n != 2 {
		t.Fatalf("image_url parts = %d, want 2: %s", n, got.Content)
	}
	// The transcript holds references, not the bytes.
	if strings.Contains(string(got.Content), "data:") {
		t.Fatalf("the committed turn must reference chat_blobs, not carry the image inline: %s", got.Content)
	}
	blobs := storedImageBlobs(t, chats, got.Content)
	if len(blobs) != 2 {
		t.Fatalf("image parts reference %d stored blobs, want 2: %s", len(blobs), got.Content)
	}
	// The MIME is REPORTED, not assumed. Hardcoding image/png would be a
	// fabricated measurement of the same class as a fake progress bar.
	for i, blob := range blobs {
		if blob.ContentType != "image/png" {
			t.Fatalf("blob %d content type = %q, must carry the upstream output_format as image/png", i, blob.ContentType)
		}
	}
	// "AA==" and "BB==" decode to different bytes: each part must be its OWN
	// item's image, not data[0]'s twice.
	if !bytes.Equal(blobs[0].plain, []byte{0x00}) || !bytes.Equal(blobs[1].plain, []byte{0x04}) {
		t.Fatalf("each part must carry its OWN item's bytes, not data[0]'s twice: %x, %x", blobs[0].plain, blobs[1].plain)
	}
	if !strings.Contains(string(got.Content), `"status":"complete"`) {
		t.Fatalf("the turn must be committed complete, not left pending: %s", got.Content)
	}
}

// A 2xx that produced no usable image is an ERROR, not an empty success: the
// endpoint already logs a counted zero on a 2xx at Error level, and committing
// an assistant turn with no image would render as a blank bubble the user
// cannot tell apart from a bug.
func TestImageRunTreatsAZeroImageResponseAsAnError(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "empty data array", body: `{"created":1,"output_format":"png","data":[]}`},
		// An item with no base64 payload is not an image either: it would
		// commit a data: URL with nothing behind it, which is the same blank
		// bubble by a different route.
		{name: "item without base64 payload", body: `{"created":1,"output_format":"png","data":[{"b64_json":""}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream, _ := imagesUpstream(t, http.StatusOK, tc.body)
			srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
			run, err := srv.startChatRun(owner, chatID, imageRunPrep())
			if err != nil {
				t.Fatalf("startChatRun: %v", err)
			}
			waitFor(t, func() bool { return run.statusValue() != "running" })

			if got := run.statusValue(); got != "error" {
				t.Fatalf("status = %q, want error for a zero-image 2xx", got)
			}
			if got := runError(t, run); got != imageRunNoImageMessage {
				t.Fatalf("error = %q, want %q", got, imageRunNoImageMessage)
			}
		})
	}
}

// The images body is the endpoint's own shape, not the chat one: model, the
// last user message's text as the prompt, and response_format pinned to the
// only value this relay can consume. None of the chat parameters go with it --
// /v1/images/generations uses none of them, and `n` is not exposed by this
// feature at all, so sending either would be a control the endpoint ignores.
func TestImageRunSendsThePinnedImagesBody(t *testing.T) {
	upstream, seen := imagesUpstream(t, http.StatusOK, `{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`)

	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
	prep := imageRunPrep()
	prep.Settings.Temperature = 0.7
	prep.Settings.MaxTokens = 1234
	prep.History = []portal.ChatAPIMessage{
		{Role: "user", Content: json.RawMessage(`"an older wish"`)},
		{Role: "assistant", Content: json.RawMessage(`"ok"`)},
		// The LAST user message wins, and the parts shape resolves like the
		// plain string one (same extractor deriveChatTitle uses).
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"a cat on a bicycle"}]`)},
	}
	run, err := srv.startChatRun(owner, chatID, prep)
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })
	if got := run.statusValue(); got != "completed" {
		t.Fatalf("status = %q (err %q), want completed", got, runError(t, run))
	}

	var got capturedUpstream
	select {
	case got = <-seen:
	default:
		t.Fatal("upstream saw no request")
	}
	var body map[string]any
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("upstream body %s: %v", got.body, err)
	}
	if body["model"] != "sd-turbo" {
		t.Fatalf("model = %v, want sd-turbo: %s", body["model"], got.body)
	}
	if body["prompt"] != "a cat on a bicycle" {
		t.Fatalf("prompt = %v, want the last user message's text: %s", body["prompt"], got.body)
	}
	if body["response_format"] != "b64_json" {
		t.Fatalf("response_format = %v, want b64_json stated explicitly: %s", body["response_format"], got.body)
	}
	for _, key := range []string{"messages", "stream", "stream_options", "temperature", "max_tokens", "n", "size"} {
		if _, ok := body[key]; ok {
			t.Fatalf("images body must not carry %q, which the endpoint uses for nothing: %s", key, got.body)
		}
	}
}

// A revised_prompt is the only substantive news this endpoint reports about a
// generation, and it arrives as an ORDINARY text part: the existing text
// renderer shows it and buildAPIHistory carries it forward, with no new shape
// for anything to learn.
func TestImageRunEmitsARevisedPromptAsATextPart(t *testing.T) {
	upstream, _ := imagesUpstream(t, http.StatusOK, `{"created":1,"output_format":"webp","data":[{"b64_json":"AA==","revised_prompt":"a tabby cat, studio light"}]}`)

	chats := store.NewMemoryChatStore(0)
	srv, _, owner, chatID := newImageRunTestServerWithChats(t, upstream.URL, chats)
	run, err := srv.startChatRun(owner, chatID, imageRunPrep())
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })
	if got := run.statusValue(); got != "completed" {
		t.Fatalf("status = %q (err %q), want completed", got, runError(t, run))
	}

	got, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got.Content), `{"type":"text","text":"a tabby cat, studio light"}`) {
		t.Fatalf("revised_prompt must be an ordinary text part: %s", got.Content)
	}
	// The media type still comes from output_format -- including when it is
	// not png. The transcript holds a reference; the type lives on the stored
	// blob.
	if !strings.Contains(string(got.Content), `{"type":"image_url","image_url":{"url":"/api/portal/chat-blobs/blob_`) {
		t.Fatalf("the image part must reference chat_blobs, keeping the {type, image_url} shape: %s", got.Content)
	}
	blobs := storedImageBlobs(t, chats, got.Content)
	if len(blobs) != 1 || blobs[0].ContentType != "image/webp" || !bytes.Equal(blobs[0].plain, []byte{0x00}) {
		t.Fatalf("stored blob must carry output_format webp and the decoded bytes: %+v", blobs)
	}
}

// A 2xx whose output_format is absent leaves the media type UNKNOWN. The run
// fails loudly instead of defaulting to image/png: an invented media type is a
// fabricated measurement, and it would also be indistinguishable from a
// genuine PNG afterwards -- in the transcript, in the download name, and in
// every later request buildAPIHistory carries it into.
func TestImageRunRefusesAResponseWithoutAnOutputFormat(t *testing.T) {
	upstream, _ := imagesUpstream(t, http.StatusOK, `{"created":1,"data":[{"b64_json":"AA=="}]}`)

	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
	run, err := srv.startChatRun(owner, chatID, imageRunPrep())
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	if got := run.statusValue(); got != "error" {
		t.Fatalf("status = %q, want error when no output_format was reported", got)
	}
	if got := runError(t, run); got != imageRunFormatUnknownMessage {
		t.Fatalf("error = %q, want %q", got, imageRunFormatUnknownMessage)
	}
	got, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got.Content), "data:image/png") {
		t.Fatalf("an unknown media type must never be committed as png: %s", got.Content)
	}
}

// A non-2xx from the loopback keeps the error code the images endpoint put in
// its body. Without this the run reports "upstream status 500 Internal Server
// Error" and every code that endpoint was built to return is discarded -- here
// the one the gateway itself synthesises for an sd-server failure.
func TestImageRunKeepsTheUpstreamErrorCode(t *testing.T) {
	upstream, _ := imagesUpstream(t, http.StatusInternalServerError, `{"error":"out of memory"}`)

	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
	run, err := srv.startChatRun(owner, chatID, imageRunPrep())
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	if got := run.statusValue(); got != "error" {
		t.Fatalf("status = %q, want error", got)
	}
	if got := runError(t, run); got != imagesUpstreamErrorCode {
		t.Fatalf("error = %q, want the upstream body's code %q", got, imagesUpstreamErrorCode)
	}
}

// A 2xx body that is not valid JSON (an HTML error page from a proxy in
// front of the upstream, a truncated buffer that still LOOKS successful at
// the status line) must not end the run with json.Unmarshal's own Go error
// text ("invalid character '<' looking for beginning of value") in a portal
// bubble -- a user cannot act on that, and it names an implementation detail.
func TestImageRunNonJSONBodyIsUnreadableNotARawGoError(t *testing.T) {
	upstream, _ := imagesUpstream(t, http.StatusOK, `<html>not json</html>`)

	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
	run, err := srv.startChatRun(owner, chatID, imageRunPrep())
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	if got := run.statusValue(); got != "error" {
		t.Fatalf("status = %q, want error", got)
	}
	if got := runError(t, run); got != imageRunResponseUnreadableMessage {
		t.Fatalf("error = %q, want %q, not a raw Go decode error", got, imageRunResponseUnreadableMessage)
	}
}

// A body the client cannot even finish READING (the upstream declares a
// Content-Length it then does not deliver, so io.ReadAll fails with
// io.ErrUnexpectedEOF) is the other branch imageRunResponseUnreadableMessage
// covers -- and it must be reached with the run's context still LIVE (no
// deadline, no cancel), or this would actually be pinning
// runContextOutcome's branch instead.
func TestImageRunTruncatedBodyIsUnreadableNotARawGoError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "short")
	}))
	t.Cleanup(upstream.Close)

	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
	run, err := srv.startChatRun(owner, chatID, imageRunPrep())
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	if got := run.statusValue(); got != "error" {
		t.Fatalf("status = %q, want error", got)
	}
	if got := runError(t, run); got != imageRunResponseUnreadableMessage {
		t.Fatalf("error = %q, want %q, not a raw Go read error", got, imageRunResponseUnreadableMessage)
	}
}

// The same, for a refusal the GATEWAY itself writes before any upstream call:
// a run whose history carries no user text sends no prompt, and
// validateImagesRequest answers images.prompt_required. The run must say that,
// not "upstream status 400 Bad Request", and it must NOT end as a successful
// empty turn.
func TestImageRunKeepsAGatewaySideErrorCode(t *testing.T) {
	upstream, seen := imagesUpstream(t, http.StatusOK, `{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`)

	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
	prep := imageRunPrep()
	prep.History = nil
	run, err := srv.startChatRun(owner, chatID, prep)
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	if got := run.statusValue(); got != "error" {
		t.Fatalf("status = %q, want error", got)
	}
	if got := runError(t, run); got != "images.prompt_required" {
		t.Fatalf("error = %q, want images.prompt_required", got)
	}
	select {
	case <-seen:
		t.Fatal("a promptless run must not reach the AI server")
	default:
	}
}

// An image run is NEVER checkpointed. There is nothing to checkpoint -- the
// endpoint emits one buffered body and no deltas -- and a checkpoint would
// write a `pending` assistant turn with empty content that the terminal commit
// then has to replace, i.e. a blank bubble for the whole wait.
func TestImageRunIsNeverCheckpointed(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	flush := func(w http.ResponseWriter) {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/chat/completions") {
			// A run that wrongly took the TEXT path must really check-point
			// here, or this test would pass for the exact regression it
			// exists to catch: consumeRunStream starts its ticker only once
			// the response has arrived and a delta is in the buffer, so the
			// stall has to sit mid-stream, not before the status line.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flush(w)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
			flush(w)
			<-release
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		// The images answer: status line first, then a stalled body -- the
		// same window, so the two paths are compared at the same moment.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		flush(w)
		<-release
		_, _ = w.Write([]byte(`{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`))
	}))
	// Closing the server waits for the in-flight request, so the upstream must
	// be released FIRST -- deferred last, and idempotent so the success path
	// can release it early.
	defer upstream.Close()
	defer releaseUpstream()

	old := runCheckpointInterval
	runCheckpointInterval = time.Millisecond
	defer func() { runCheckpointInterval = old }()

	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
	run, err := srv.startChatRun(owner, chatID, imageRunPrep())
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	// Hold the upstream open well past many checkpoint intervals, then read
	// the transcript: a checkpointing run would have written its pending
	// assistant turn several times over by now.
	time.Sleep(50 * time.Millisecond)
	inflight, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(inflight.Content), `"role":"assistant"`) {
		t.Fatalf("an image run must not checkpoint a pending assistant turn: %s", inflight.Content)
	}
	if got := run.statusValue(); got != "running" {
		t.Fatalf("run ended early (%q); the in-flight check above proved nothing", got)
	}

	releaseUpstream()
	waitFor(t, func() bool { return run.statusValue() != "running" })
	if got := run.statusValue(); got != "completed" {
		t.Fatalf("status = %q (err %q), want completed", got, runError(t, run))
	}
	done, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(done.Content), `"status":"pending"`) {
		t.Fatalf("no turn of an image run is ever pending: %s", done.Content)
	}
}

// The executor sets the session override header to the chat id, and the
// extractor reads that header BEFORE its per-endpoint switch -- so an image
// run's usage row is tagged as a chat session exactly like a text run's, even
// though /v1/images/generations has no per-endpoint session signal of its own.
func TestImageRunUsageRowIsTaggedAsAChatSession(t *testing.T) {
	upstream, _ := imagesUpstream(t, http.StatusOK, `{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`)

	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
	run, err := srv.startChatRun(owner, chatID, imageRunPrep())
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })
	if got := run.statusValue(); got != "completed" {
		t.Fatalf("status = %q (err %q), want completed", got, runError(t, run))
	}

	ev := lastUsageEvent(t, srv)
	if ev.SessionID != chatID {
		t.Fatalf("SessionID = %q, want the chat id %q", ev.SessionID, chatID)
	}
	if ev.SessionSource != "chat" {
		t.Fatalf("SessionSource = %q, want chat", ev.SessionSource)
	}
	if ev.BillingUnit != usage.BillingUnitImage {
		t.Fatalf("BillingUnit = %q, want %q", ev.BillingUnit, usage.BillingUnitImage)
	}
}

// A chat configured to run as a specific API token attributes its IMAGE turns
// to that token too. The handler already honors X-OP-Run-As-Token; this pins
// that the run executor actually sends it, which is what makes the attribution
// work end to end rather than only in the handler's own tests.
func TestImageRunAttributionFollowsTheRunAsToken(t *testing.T) {
	upstream, _ := imagesUpstream(t, http.StatusOK, `{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`)

	srv, dir, owner, chatID := newImageRunTestServer(t, upstream.URL)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{
		ID: "tok_ra_img", UserID: "usr_dev", Name: "RA Img", Status: store.TokenStatusActive,
		Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now,
	}, "ra-secret"); err != nil {
		t.Fatalf("seed run-as token: %v", err)
	}

	prep := imageRunPrep()
	prep.Settings.RunAsTokenID = "tok_ra_img"
	run, err := srv.startChatRun(owner, chatID, prep)
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })
	if got := run.statusValue(); got != "completed" {
		t.Fatalf("status = %q (err %q), want completed", got, runError(t, run))
	}

	ev := lastUsageEvent(t, srv)
	if ev.TokenID != "tok_ra_img" {
		t.Fatalf("TokenID = %q, want tok_ra_img (the run-as token, not the bare loopback principal)", ev.TokenID)
	}
	// The row this asserts on has to be the IMAGE request's own -- the chat
	// hop already carries the run-as header, so without this the test would
	// hold for a run that never reached the images endpoint at all.
	if ev.ReqPath != "/v1/images/generations" {
		t.Fatalf("ReqPath = %q, want /v1/images/generations", ev.ReqPath)
	}
}

// TestImageRunServerOverrideReachesTheImagesHop is the server-override
// counterpart to TestImageRunAttributionFollowsTheRunAsToken immediately
// above: the header fix was structural (one shared setRunLoopbackHeaders),
// and the run-as header already had its own end-to-end pin, but the
// server-override pair did not. This proves an image thread's per-run
// server_override actually steers the /v1/images/generations hop to the
// OVERRIDDEN server -- not merely that a header was attached to some
// intermediate request.
//
// The alt server is seeded UNHEALTHY on purpose, and the override run sets
// ServerOverrideForceUnreachable. Without that, giving both servers the SAME
// gateway model would make this test pass whether or not the override header
// actually reached the images hop: normal (non-override) routing could pick
// EITHER candidate by ordinary scoring, which would pin nothing. An unhealthy
// server is never selected by the ordinary path, so the negative-control run
// below (no override) proves it is unreachable by luck of candidate order,
// and only the explicit force-override bypass in resolveServerOverride can
// land a request there -- which is exactly the mechanism under test.
func TestImageRunServerOverrideReachesTheImagesHop(t *testing.T) {
	defaultUpstream, defaultSeen := imagesUpstream(t, http.StatusOK, `{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`)
	overrideUpstream, overrideSeen := imagesUpstream(t, http.StatusOK, `{"created":1,"output_format":"png","data":[{"b64_json":"BB=="}]}`)

	srv, _, owner, chatID := newImageRunTestServer(t, defaultUpstream.URL)

	ctx := context.Background()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	up, err := url.Parse(overrideUpstream.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	port, err := strconv.Atoi(up.Port())
	if err != nil {
		t.Fatalf("upstream port %q: %v", up.Port(), err)
	}
	if err := srv.Routes.CreateAIServer(ctx, routing.AIServer{ID: "srv-sd-alt", Name: "SD Alt Upstream", Domain: up.Hostname(), Provider: routing.ProviderVLLM, Endpoint: overrideUpstream.URL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthUnhealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := srv.Routes.CreateApplication(ctx, routing.Application{ID: "app-sd-alt", ServerID: "srv-sd-alt", Type: routing.ProviderVLLM, Port: port, Scheme: up.Scheme, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := srv.Routes.CreateMapping(ctx, routing.ModelMapping{ID: "route-sd-alt", ApplicationID: "app-sd-alt", GatewayModelName: "sd-turbo", AppModelName: "sd-turbo", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := srv.Routes.UpsertMappingCapabilities(ctx, "route-sd-alt", []routing.CapabilityRow{{Capability: routing.CapabilityImage, Verdict: routing.CapabilityYes, Source: "manual", CheckedAt: now}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	if err := srv.Routes.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-sd-alt", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	// resolveServerOverride re-authorizes the caller for the TARGET server
	// (authorizeServer): a plain admin does not automatically manage every
	// server (Phase B), so the override needs explicit ownership here.
	if err := srv.Routes.SetServerOwners(ctx, "srv-sd-alt", []string{"usr_dev"}); err != nil {
		t.Fatalf("SetServerOwners: %v", err)
	}

	// Negative control: with NO override, the unhealthy alt is unreachable by
	// the ordinary path, so the run must land on the default upstream.
	run1, err := srv.startChatRun(owner, chatID, imageRunPrep())
	if err != nil {
		t.Fatalf("startChatRun (no override): %v", err)
	}
	waitFor(t, func() bool { return run1.statusValue() != "running" })
	if got := run1.statusValue(); got != "completed" {
		t.Fatalf("status = %q (err %q), want completed", got, runError(t, run1))
	}
	select {
	case <-defaultSeen:
	default:
		t.Fatal("negative control: the default upstream never saw the un-overridden run")
	}

	// The run under test: WITH the override (+ force, since the target is
	// unhealthy), the SAME chat's next run must reach the OVERRIDDEN server.
	prep := imageRunPrep()
	prep.Settings.ServerOverride = "srv-sd-alt"
	prep.Settings.ServerOverrideForceUnreachable = true
	run2, err := srv.startChatRun(owner, chatID, prep)
	if err != nil {
		t.Fatalf("startChatRun (override): %v", err)
	}
	waitFor(t, func() bool { return run2.statusValue() != "running" })
	if got := run2.statusValue(); got != "completed" {
		t.Fatalf("status = %q (err %q), want completed", got, runError(t, run2))
	}
	select {
	case got := <-overrideSeen:
		// Mirrors TestImageRunAttributionFollowsTheRunAsToken's own ReqPath
		// assertion, for the same reason: the image kind branches to a
		// DIFFERENT request than the chat hop, so without this the "OVERRIDE
		// upstream saw A request" check above would hold even for a request
		// that never carried the images shape at all. Not vacuous TODAY --
		// executeImageRun's branch happens before either hop is reached, so
		// the chat hop is structurally unreachable for this run -- but the
		// guard is what keeps that true if the branching ever moves.
		if got.path != "/v1/images/generations" {
			t.Fatalf("path = %q, want /v1/images/generations", got.path)
		}
	default:
		t.Fatal("the OVERRIDE upstream never saw the request -- the server override did not reach the images hop")
	}
}

// imageMediaType is the one place an upstream-controlled string is
// interpolated into a URL the browser loads and the download names a file
// from, so what it accepts is pinned directly rather than only through the
// end-to-end cases above.
func TestImageMediaTypeAcceptsOnlyABareSubtype(t *testing.T) {
	cases := []struct {
		name         string
		outputFormat string
		want         string
		wantErr      bool
	}{
		{name: "the value sd-server reports", outputFormat: "png", want: "image/png"},
		{name: "another real value", outputFormat: "jpeg", want: "image/jpeg"},
		{name: "surrounding space", outputFormat: "  webp  ", want: "image/webp"},
		{name: "case is normalised, a media type is case-insensitive", outputFormat: "PNG", want: "image/png"},
		// Absent: the media type is unknown, and image/png would be invented.
		{name: "absent", outputFormat: "", wantErr: true},
		// A full media type is refused rather than repaired: accepting one
		// would let an upstream choose the whole type, and "text/html" in a
		// data: URL is saved and opened as markup.
		{name: "a full media type", outputFormat: "image/png", wantErr: true},
		{name: "a forged type", outputFormat: "text/html", wantErr: true},
		// Anything that could break out of the data: URL's own grammar.
		{name: "url punctuation", outputFormat: `png;base64,AAAA`, wantErr: true},
		{name: "a comma", outputFormat: "png,x", wantErr: true},
		// The length is bounded (32) independently of the alphabet check: an
		// upstream-controlled string this long, interpolated into every data:
		// URL the part produces, has no legitimate reason to exceed a real
		// media subtype's length -- see imageSubtypePattern's own doc comment.
		{name: "at the 32-char length cap", outputFormat: strings.Repeat("a", 32), want: "image/" + strings.Repeat("a", 32)},
		{name: "one character past the cap is refused", outputFormat: strings.Repeat("a", 33), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := imageMediaType(tc.outputFormat)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("imageMediaType(%q) = %q, want an error", tc.outputFormat, got)
				}
				if err.Error() != imageRunFormatUnknownMessage {
					t.Fatalf("error = %q, want %q", err, imageRunFormatUnknownMessage)
				}
				return
			}
			if err != nil {
				t.Fatalf("imageMediaType(%q): %v", tc.outputFormat, err)
			}
			if got != tc.want {
				t.Fatalf("imageMediaType(%q) = %q, want %q", tc.outputFormat, got, tc.want)
			}
		})
	}
}

// A user Stop on an image run stays a CANCEL: status canceled with an EMPTY
// message, which is what the UI reads as "the user pressed Stop". The image
// path has its own copy of that branch (its round trip and its body read both
// end on a context error), so the invariant the text path pins in
// TestUserCancelStaysACancelUnderADeadline needs pinning here too.
func TestImageRunUserCancelStaysACancel(t *testing.T) {
	upstream, release := stalledImagesUpstream(t, "")
	defer release()

	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
	run, err := srv.startChatRun(owner, chatID, imageRunPrep())
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	if !srv.ChatRuns.cancelChat(owner.UserID, chatID) {
		t.Fatal("cancelChat found no active run to cancel")
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	if got := run.statusValue(); got != "canceled" {
		t.Fatalf("status = %q (err %q), want canceled", got, runError(t, run))
	}
	if got := runError(t, run); got != "" {
		t.Fatalf("cancel error = %q, want empty -- a non-empty message is what a TIMEOUT looks like", got)
	}
}

// TestImageRunDoneEventCarriesTheCommittedImageParts is the fix for the
// whole-branch review's finding 1/2, and it lives in the SEAM two separately
// reviewed tasks left between them.
//
// The run's `content` buffer is the STREAMED TEXT one. An image run never
// writes a byte into it -- its whole output is the structured content the
// terminal commit stores -- so before content_parts existed the terminal event
// of a perfectly successful image run carried no content at all. The browser
// dutifully set the bubble to "" and its own empty-tail prune then deleted the
// turn the run had just generated and persisted; only the post-`done` refetch
// of the whole (multi-megabyte) document put it back, and when THAT failed the
// portal saved the pruned transcript over the server's good one.
//
// So the assertion is two-sided on purpose: Content must still be empty (this
// is not a regression that started stuffing the text buffer) AND ContentParts
// must carry exactly what the turn committed -- the image the transcript now
// holds by reference, so the browser can render it from the event itself.
func TestImageRunDoneEventCarriesTheCommittedImageParts(t *testing.T) {
	// The whole body is written as the first chunk and the handler then stalls,
	// so the run is reliably still `running` when we subscribe and the `done`
	// below is genuinely observed over the stream rather than reconstructed
	// from a late snapshot.
	upstream, release := stalledImagesUpstream(t, `{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`)
	defer release()

	chats := store.NewMemoryChatStore(0)
	srv, _, owner, chatID := newImageRunTestServerWithChats(t, upstream.URL, chats)
	run, err := srv.startChatRun(owner, chatID, imageRunPrep())
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	snap, ch, unsub := run.subscribe()
	defer unsub()
	if snap.Status != "running" {
		t.Fatalf("run status = %q before release, want running -- the stalled upstream must still be holding it", snap.Status)
	}
	release()

	var done runEvent
	for ev := range ch {
		if ev.Event == "done" {
			done = ev
		}
	}
	if done.Event != "done" {
		t.Fatal("the stream closed without a done event")
	}
	if done.Status != "completed" {
		t.Fatalf("done.Status = %q (err %q), want completed", done.Status, done.Err)
	}
	if done.Content != "" {
		t.Fatalf("done.Content = %q, want empty -- an image run streams no text and must not fabricate any", done.Content)
	}
	if !strings.Contains(string(done.ContentParts), `data:image/png;base64,AA==`) {
		t.Fatalf("done.ContentParts = %s, want the committed image part", done.ContentParts)
	}
	// ...and what it carries is what was STORED, not a second rendering of it.
	// The transcript now holds that image BY REFERENCE (the commit moved the
	// bytes into chat_blobs), so "the same image" is read off the blob the
	// transcript points at: it must hold exactly the bytes the event carries.
	stored, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	blobs := storedImageBlobs(t, chats, stored.Content)
	if len(blobs) != 1 || blobs[0].ContentType != "image/png" || !bytes.Equal(blobs[0].plain, []byte{0x00}) {
		t.Fatalf("done.ContentParts = %s is not what the transcript holds: %s -> %+v", done.ContentParts, stored.Content, blobs)
	}
}

// A LATE subscriber -- one that reconnects inside the 30s eviction grace --
// is served the terminal state as a `snapshot` and never sees a `done` at
// all (subscribe closes the channel for an already-terminal run). It has
// exactly the same need for the committed content, so the parts ride on
// snapshotLocked rather than being bolted onto the done event.
func TestImageRunTerminalSnapshotCarriesTheCommittedImageParts(t *testing.T) {
	upstream, _ := imagesUpstream(t, http.StatusOK, `{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`)

	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
	run, err := srv.startChatRun(owner, chatID, imageRunPrep())
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	snap, _, unsub := run.subscribe()
	unsub()
	if snap.Status != "completed" {
		t.Fatalf("snapshot status = %q (err %q), want completed", snap.Status, snap.Err)
	}
	if !strings.Contains(string(snap.ContentParts), `data:image/png;base64,AA==`) {
		t.Fatalf("terminal snapshot ContentParts = %s, want the committed image part", snap.ContentParts)
	}
}

// deletingChatStore deletes the chat the instant a run commit tries to save it:
// DELETE /chats/{id} landing between the run's read of the chat and its write.
// Armed after the chat exists, so only the commit under test meets the race.
type deletingChatStore struct {
	portal.ChatStore
	armed   atomic.Bool
	deleted atomic.Bool
}

func (s *deletingChatStore) SaveChatWithBlobs(ctx context.Context, chat store.Chat, blobs []store.ChatBlob) error {
	if s.armed.Load() {
		_ = s.ChatStore.DeleteChat(ctx, chat.ID)
		s.deleted.Store(true)
	}
	return s.ChatStore.SaveChatWithBlobs(ctx, chat, blobs)
}

// A generated image is stored by the commit through SaveChatWithBlobs, which
// can now meet a chat that was deleted mid-run on the blob insert / chat update
// itself. That has to come back as the same "the chat is gone" the executor
// already tells apart from a failed write: a run that was ending "canceled" (by
// that very DELETE) stays canceled, one that was about to report "completed"
// is turned into the generic commit error because its image was never saved --
// and neither is a 500, and the image is never offered as stored.
func TestImageRunBlobCommitOnADeletedChatIsTheChatGonePath(t *testing.T) {
	for _, tc := range []struct{ status, wantStatus, wantErr string }{
		{"canceled", "canceled", ""},
		{"completed", "error", chatRunCommitFailedMessage},
	} {
		t.Run(tc.status, func(t *testing.T) {
			upstream, _ := imagesUpstream(t, http.StatusOK, `{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`)
			chats := &deletingChatStore{ChatStore: store.NewMemoryChatStore(0)}
			srv, _, owner, chatID := newImageRunTestServerWithChats(t, upstream.URL, chats)
			chats.armed.Store(true)

			run, err := srv.ChatRuns.add(owner.UserID, chatID, func() {})
			if err != nil {
				t.Fatalf("add: %v", err)
			}
			parts := json.RawMessage(`[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]`)
			srv.finishRunWithParts(context.Background(), owner, run, tc.status, "", parts)

			if !chats.deleted.Load() {
				t.Fatal("the commit never reached SaveChatWithBlobs, so the race was not exercised")
			}
			if got := run.statusValue(); got != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got, tc.wantStatus)
			}
			if got := runError(t, run); got != tc.wantErr {
				t.Fatalf("error = %q, want %q", got, tc.wantErr)
			}
			snap, _, unsub := run.subscribe()
			unsub()
			if len(snap.ContentParts) != 0 {
				t.Fatalf("ContentParts = %s, want none: the image was not stored", snap.ContentParts)
			}
		})
	}
}

// The counterpart invariant, and the one finding 3's wording depends on: the
// terminal event may only claim what the store ACCEPTED. portal.ErrChatTooLarge
// is the case that matters -- the image was generated and the commit refused --
// and handing the browser the parts anyway would recreate "rendered, then
// vanished on reload" in its other direction, with the error label telling the
// user to download an image that is about to disappear.
func TestImageRunTerminalEventWithholdsPartsWhenTheCommitFailed(t *testing.T) {
	upstream, _ := imagesUpstream(t, http.StatusOK, `{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`)

	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
	srv.Portal = &commitFailingPortal{API: srv.Portal, err: portal.ErrChatTooLarge}
	run, err := srv.startChatRun(owner, chatID, imageRunPrep())
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	snap, _, unsub := run.subscribe()
	unsub()
	if snap.Status != "error" || snap.Err != "portal.chat_too_large" {
		t.Fatalf("terminal = (%q, %q), want (error, portal.chat_too_large)", snap.Status, snap.Err)
	}
	if len(snap.ContentParts) != 0 {
		t.Fatalf("ContentParts = %s, want none -- nothing was stored, so the event must claim nothing", snap.ContentParts)
	}
}

// A gateway that cannot reach ITSELF is a real operator failure (the loopback
// listener is down, the port moved) and the user's copy of it used to be the
// raw Go transport error -- which embeds the request URL, so the portal bubble
// read `Post "http://127.0.0.1:<port>/v1/images/generations": dial tcp ...:
// connection refused`: an internal address and a transport detail, about
// something no user can act on.
//
// Pointed at a port that is closed rather than a stub that 500s, because the
// defect is specifically in the branch where Do itself returns an error; a
// non-2xx takes upstreamErrorCode and was never affected.
func TestImageRunDispatchFailureIsACodeNotARawGoError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)

	// A listener that is bound and then closed: the port is real and refuses.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	deadURL := "http://" + dead.Addr().String()
	if err := dead.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	srv.selfBaseURL = deadURL

	run, err := srv.startChatRun(owner, chatID, imageRunPrep())
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	if got := run.statusValue(); got != "error" {
		t.Fatalf("status = %q, want error", got)
	}
	got := runError(t, run)
	if got != imageRunDispatchFailedMessage {
		t.Fatalf("error = %q, want the mapped code %q", got, imageRunDispatchFailedMessage)
	}
	// The two properties the code exists for, asserted on the message the
	// browser is actually handed.
	if strings.Contains(got, deadURL) || strings.Contains(got, "127.0.0.1") {
		t.Fatalf("the terminal message leaks the internal base URL: %q", got)
	}
	if strings.Contains(got, "connection refused") || strings.Contains(got, "dial tcp") {
		t.Fatalf("the terminal message is a raw Go transport error: %q", got)
	}
}
