// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"reflect"
	"strings"
	"testing"
	"time"
)

// decodeJSONObject decodes raw into a generic object with UseNumber, the same
// way the helpers under test do, so two bodies compare by VALUE and a lossy
// number round-trip shows up as a difference instead of being normalised away.
func decodeJSONObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return obj
}

// TestInjectChatTemplateReasoningEffort walks the helper over every branch of
// the merge contract (spec 4.4/4.5 of #230). Three invariants are asserted for
// EVERY row rather than per-row, because each is the kind of thing a single
// forgotten row would let regress:
//
//   - the INPUT slice is never written to. proxyNative reads rel.raw again after
//     dispatch to build the payload capture, so an in-place edit would put a key
//     the client never sent into the capture;
//   - a changed body is a FRESH slice: scribbling over it afterwards must leave
//     the input untouched (a returned slice that aliased the input would pass the
//     first check and still corrupt the capture on the next write);
//   - an unchanged body is the ORIGINAL bytes, byte for byte.
func TestInjectChatTemplateReasoningEffort(t *testing.T) {
	const bigInt = "9007199254740993" // 2^53+1: a float64 round-trip would make it ...992

	cases := []struct {
		name string
		in   string
		// want is the exact value-equal body expected when the helper injects;
		// empty means the body must come back unchanged.
		want         string
		wantInjected bool
		wantErr      error
	}{
		// --- no effort supplied: forward unchanged, never an error ---
		{name: "no output_config", in: `{"model":"m","messages":[]}`},
		{name: "output_config null", in: `{"model":"m","output_config":null}`},
		{name: "output_config without effort", in: `{"model":"m","output_config":{"format":{"type":"json_schema"}}}`},
		{name: "effort null", in: `{"model":"m","output_config":{"effort":null}}`},
		{name: "effort empty", in: `{"model":"m","output_config":{"effort":""}}`},
		{name: "effort whitespace only", in: `{"model":"m","output_config":{"effort":"  \t"}}`},
		{
			// output_config is a Claude-Code field with no footprint here; a value
			// that is not an object simply carries no effort. It must not turn into
			// a rejection the client never earned.
			name: "output_config is not an object",
			in:   `{"model":"m","output_config":"high"}`,
		},
		{
			// With nothing to inject there is nothing to conflict with, so a
			// malformed chat_template_kwargs is the client's own business.
			name: "no effort and a non-object chat_template_kwargs is left alone",
			in:   `{"model":"m","chat_template_kwargs":"x"}`,
		},

		// --- injection ---
		{
			name:         "effort low with no existing kwargs",
			in:           `{"model":"m","output_config":{"effort":"low"},"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"Read"}]}`,
			want:         `{"model":"m","output_config":{"effort":"low"},"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"Read"}],"chat_template_kwargs":{"reasoning_effort":"low"}}`,
			wantInjected: true,
		},
		{
			name:         "surrounding whitespace is trimmed",
			in:           `{"output_config":{"effort":"  high\n"}}`,
			want:         `{"output_config":{"effort":"  high\n"},"chat_template_kwargs":{"reasoning_effort":"high"}}`,
			wantInjected: true,
		},
		{
			// No Qwen enum and no high->xhigh mapping: the name goes through as the
			// client wrote it, whatever it is.
			name:         "effort name is forwarded verbatim (xhigh)",
			in:           `{"output_config":{"effort":"xhigh"}}`,
			want:         `{"output_config":{"effort":"xhigh"},"chat_template_kwargs":{"reasoning_effort":"xhigh"}}`,
			wantInjected: true,
		},
		{
			name:         "effort name is forwarded verbatim (max)",
			in:           `{"output_config":{"effort":"max"}}`,
			want:         `{"output_config":{"effort":"max"},"chat_template_kwargs":{"reasoning_effort":"max"}}`,
			wantInjected: true,
		},
		{
			name:         "unrelated output_config properties are ignored and kept",
			in:           `{"output_config":{"effort":"medium","format":{"type":"json_schema"}}}`,
			want:         `{"output_config":{"effort":"medium","format":{"type":"json_schema"}},"chat_template_kwargs":{"reasoning_effort":"medium"}}`,
			wantInjected: true,
		},
		{
			name:         "existing unrelated kwargs are merged, not replaced",
			in:           `{"output_config":{"effort":"low"},"chat_template_kwargs":{"enable_thinking":true,"nested":{"a":[1,2]}}}`,
			want:         `{"output_config":{"effort":"low"},"chat_template_kwargs":{"enable_thinking":true,"nested":{"a":[1,2]},"reasoning_effort":"low"}}`,
			wantInjected: true,
		},
		{
			name:         "empty existing kwargs object receives the key",
			in:           `{"output_config":{"effort":"low"},"chat_template_kwargs":{}}`,
			want:         `{"output_config":{"effort":"low"},"chat_template_kwargs":{"reasoning_effort":"low"}}`,
			wantInjected: true,
		},
		{
			// UseNumber is what keeps the rest of the body value-lossless: without
			// it this literal comes back as 9007199254740992.
			name:         "numbers elsewhere in the body stay lossless",
			in:           `{"max_tokens":` + bigInt + `,"temperature":0.30000000000000004,"x":{"y":[1.50,1e3]},"output_config":{"effort":"low"}}`,
			want:         `{"max_tokens":` + bigInt + `,"temperature":0.30000000000000004,"x":{"y":[1.50,1e3]},"output_config":{"effort":"low"},"chat_template_kwargs":{"reasoning_effort":"low"}}`,
			wantInjected: true,
		},

		// --- same value already there: no-op ---
		{
			name: "identical reasoning_effort already present",
			in:   `{"output_config":{"effort":"low"},"chat_template_kwargs":{"reasoning_effort":"low","foo":1}}`,
		},
		{
			// The comparison is against the TRIMMED effort, so the client's padded
			// spelling still matches its own already-present kwarg.
			name: "identical reasoning_effort already present, effort padded",
			in:   `{"output_config":{"effort":" low "},"chat_template_kwargs":{"reasoning_effort":"low"}}`,
		},

		// --- conflicts ---
		{
			name:    "different reasoning_effort already present",
			in:      `{"output_config":{"effort":"low"},"chat_template_kwargs":{"reasoning_effort":"medium"}}`,
			wantErr: errReasoningEffortConflict,
		},
		{
			// A non-string existing value cannot equal the effort, so it conflicts
			// rather than being overwritten.
			name:    "non-string reasoning_effort already present",
			in:      `{"output_config":{"effort":"low"},"chat_template_kwargs":{"reasoning_effort":3}}`,
			wantErr: errReasoningEffortConflict,
		},
		{
			name:    "null reasoning_effort already present",
			in:      `{"output_config":{"effort":"low"},"chat_template_kwargs":{"reasoning_effort":null}}`,
			wantErr: errReasoningEffortConflict,
		},

		// --- non-object chat_template_kwargs ---
		{name: "kwargs is a string", in: `{"output_config":{"effort":"low"},"chat_template_kwargs":"x"}`, wantErr: errChatTemplateKwargsNotObject},
		{name: "kwargs is an array", in: `{"output_config":{"effort":"low"},"chat_template_kwargs":[]}`, wantErr: errChatTemplateKwargsNotObject},
		{name: "kwargs is a number", in: `{"output_config":{"effort":"low"},"chat_template_kwargs":1}`, wantErr: errChatTemplateKwargsNotObject},
		{
			// null is not an object either. Treating it as "absent" would quietly
			// replace a value the client sent; rejecting is the literal reading of
			// the contract and costs nothing for any real client.
			name:    "kwargs is null",
			in:      `{"output_config":{"effort":"low"},"chat_template_kwargs":null}`,
			wantErr: errChatTemplateKwargsNotObject,
		},

		// --- malformed effort ---
		{name: "effort is a number", in: `{"output_config":{"effort":3}}`, wantErr: errOutputConfigEffortNotString},
		{name: "effort is a bool", in: `{"output_config":{"effort":true}}`, wantErr: errOutputConfigEffortNotString},
		{name: "effort is an object", in: `{"output_config":{"effort":{"level":"low"}}}`, wantErr: errOutputConfigEffortNotString},
		{name: "effort is an array", in: `{"output_config":{"effort":["low"]}}`, wantErr: errOutputConfigEffortNotString},

		// --- not a usable object body: same guards as injectTimingsPerToken ---
		{name: "nil body", in: ``},
		{name: "null body", in: `null`},
		{name: "array body", in: `[{"output_config":{"effort":"low"}}]`},
		{name: "truncated body", in: `{"output_config":{"effort":"low"`},
		{name: "not json", in: `hello`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var in []byte
			if tc.in != "" {
				in = []byte(tc.in)
			}
			before := bytes.Clone(in)

			out, injected, err := injectChatTemplateReasoningEffort(in)

			if !bytes.Equal(in, before) {
				t.Fatalf("the input slice was mutated: now %s, was %s", in, before)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if injected != tc.wantInjected {
				t.Fatalf("injected = %v, want %v", injected, tc.wantInjected)
			}

			if tc.want == "" {
				if !bytes.Equal(out, in) {
					t.Fatalf("body changed on a no-op/error path: %s, want the original %s", out, in)
				}
				return
			}

			if len(out) > 0 && len(in) > 0 && &out[0] == &in[0] {
				t.Fatal("an injected body must be a FRESH slice, but it shares the input's backing array")
			}
			if got, want := decodeJSONObject(t, out), decodeJSONObject(t, []byte(tc.want)); !reflect.DeepEqual(got, want) {
				t.Fatalf("body = %s\nwant a value-equal %s", out, tc.want)
			}
			// Scribbling over the returned slice must not reach the input: a
			// returned slice that aliased rel.raw would corrupt the capture the next
			// time anything wrote to the outgoing bytes.
			for i := range out {
				out[i] = 'X'
			}
			if !bytes.Equal(in, before) {
				t.Fatalf("writing to the returned slice changed the input: now %s, was %s", in, before)
			}
		})
	}
}

// TestInjectChatTemplateReasoningEffortKeepsLiteralNumbers pins the claim in
// the table above at the byte level: the big integer and the trailing zero of a
// float survive as the client's own text, which a decode into float64 cannot do.
func TestInjectChatTemplateReasoningEffortKeepsLiteralNumbers(t *testing.T) {
	in := []byte(`{"a":9007199254740993,"b":1.50,"output_config":{"effort":"low"}}`)

	out, injected, err := injectChatTemplateReasoningEffort(in)
	if err != nil || !injected {
		t.Fatalf("injected = %v, err = %v, want an injection", injected, err)
	}
	for _, literal := range []string{`"a":9007199254740993`, `"b":1.50`} {
		if !bytes.Contains(out, []byte(literal)) {
			t.Fatalf("relayed body lost the literal %s: %s", literal, out)
		}
	}
}

// ---------------------------------------------------------------------------
// Handler level: the helper through the real server, proxyNative, the capture
// and the usage record.
// ---------------------------------------------------------------------------

// effortSeed describes one seeded Messages-passthrough deployment.
type effortSeed struct {
	// appType is the application row's kind (routing.Provider*); it is what
	// Target.Provider carries for an ordinary application.
	appType string
	// spec, when non-nil, is upserted against the mapping. For a server_agent
	// application it supplies the effective runtime kind.
	spec *routing.RuntimeSpec
	// flag is the token's claude_effort_compatibility switch.
	flag bool
}

// agentSpec is a server_agent runtime spec of the given kind with Messages on
// passthrough.
func agentSpec(kind routing.RuntimeSpecType) *routing.RuntimeSpec {
	return &routing.RuntimeSpec{
		Enabled:       true,
		Type:          string(kind),
		APIFlavors:    []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic},
		ResponsesMode: routing.EndpointModePassthrough,
		MessagesMode:  routing.EndpointModePassthrough,
	}
}

// newEffortCompatTestServer seeds one upstream + application + mapping (gateway
// model "gw-model" -> upstream "upstream-model") with the Messages endpoint on
// passthrough, and a token whose ClaudeEffortCompatibility is seed.flag. The
// token flag is set on the STORE RECORD, so the whole request path -- bearer
// lookup, auth.Token, nativeRelay -- is what carries it to proxyNative.
func newEffortCompatTestServer(t *testing.T, prov provider.Client, seed effortSeed) *Server {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, ClaudeEffortCompatibility: seed.flag, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	ctx := context.Background()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-ec", Name: "Effort Upstream", Domain: "ec.example.test", Provider: routing.ProviderLlamaCPP, Endpoint: "http://ec.example.test:8000", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-ec", ServerID: "srv-ec", Type: seed.appType, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, ResponsesMode: routing.EndpointModePassthrough, MessagesMode: routing.EndpointModePassthrough, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-ec", ApplicationID: "app-ec", GatewayModelName: "gw-model", AppModelName: "upstream-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if seed.spec != nil {
		spec := *seed.spec
		spec.ID, spec.MappingID, spec.CreatedAt, spec.UpdatedAt = "spec-ec", "route-ec", now, now
		if err := routeStore.UpsertRuntimeSpec(ctx, spec); err != nil {
			t.Fatalf("UpsertRuntimeSpec: %v", err)
		}
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-ec", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: prov,
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

// anthropicTerminalStream is a minimal Messages stream: enough for the copy and
// the usage scan to finish normally. These tests are about the REQUEST.
const anthropicTerminalStream = "event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}` + "\n\n" +
	"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"

// claudeCodeBody is a Claude-Code-shaped Messages request carrying effort:
// thinking, output_config, tools, a big integer (to prove the re-serialization
// is lossless) and a stream flag. extra is spliced in as additional top-level
// fields, each written with its leading comma.
func claudeCodeBody(stream bool, effortJSON, extra string) string {
	return fmt.Sprintf(`{"model":"gw-model","max_tokens":4096,"stream":%t,"thinking":{"type":"adaptive"},"output_config":{"effort":%s},"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"metadata":{"big":9007199254740993}%s}`, stream, effortJSON, extra)
}

// postMessages drives one /v1/messages request through the whole server.
func postMessages(srv *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// relayMessages posts body and requires that it reached the upstream exactly
// once, returning the body the provider fake was handed. The proxyCalls check
// is what keeps every "unchanged" assertion honest: a fixture that failed to
// route would satisfy a "no injection" assertion with an empty body.
func relayMessages(t *testing.T, srv *Server, prov *recordingProxyProvider, body string) []byte {
	t.Helper()
	rec := postMessages(srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/messages: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if prov.proxyCalls != 1 {
		t.Fatalf("ProxyNative calls = %d, want exactly 1 (the request must have reached the native passthrough path)", prov.proxyCalls)
	}
	if prov.gotPath != "/v1/messages" {
		t.Fatalf("upstream path = %q, want /v1/messages (a Messages body, not a translated OpenAI one)", prov.gotPath)
	}
	return prov.gotBody
}

// TestNativeMessagesInjectsChatTemplateReasoningEffort is the primary case of
// #230: an opted-in token, a chat_template_kwargs-capable passthrough target and
// an effort in the client's output_config. The upstream receives the SAME
// Messages body with the effort also delivered as chat_template_kwargs and
// nothing else changed apart from the mapped model name.
//
// The expectation is built from the CLIENT's own body by value, so a dropped
// field, a lossy number or a rewritten sibling fails it without the test having
// to list every field. Every kind of capable target is a row: direct llama.cpp
// and vLLM (matched through Target.Provider) and agent-managed llama.cpp and
// vLLM (matched through the resolved spec's type).
func TestNativeMessagesInjectsChatTemplateReasoningEffort(t *testing.T) {
	seeds := []struct {
		name string
		seed effortSeed
	}{
		{"direct llama.cpp", effortSeed{appType: routing.ProviderLlamaCPP, flag: true}},
		{"direct vLLM", effortSeed{appType: routing.ProviderVLLM, flag: true}},
		{"agent-managed llama.cpp", effortSeed{appType: routing.ProviderServerAgent, spec: agentSpec(routing.RuntimeSpecTypeLlamaCpp), flag: true}},
		{"agent-managed vLLM", effortSeed{appType: routing.ProviderServerAgent, spec: agentSpec(routing.RuntimeSpecTypeVLLM), flag: true}},
	}
	for _, sc := range seeds {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", sc.name, stream), func(t *testing.T) {
				prov := &recordingProxyProvider{respBody: anthropicTerminalStream}
				srv := newEffortCompatTestServer(t, prov, sc.seed)
				client := claudeCodeBody(stream, `"medium"`, ``)

				got := relayMessages(t, srv, prov, client)

				want := decodeJSONObject(t, []byte(client))
				want["model"] = "upstream-model"
				want["chat_template_kwargs"] = map[string]any{"reasoning_effort": "medium"}
				if upstream := decodeJSONObject(t, got); !reflect.DeepEqual(upstream, want) {
					t.Fatalf("upstream body = %s\nwant the client body with model=upstream-model and chat_template_kwargs.reasoning_effort=medium", got)
				}
				if !bytes.Contains(got, []byte(`9007199254740993`)) {
					t.Fatalf("a number elsewhere in the body lost precision: %s", got)
				}
			})
		}
	}
}

// TestNativeMessagesEffortMergeKeepsUnrelatedKwargsAndNoOpsOnAMatch covers the
// two non-error merge rules end to end: unrelated chat_template_kwargs survive
// the merge, and a kwarg that already carries the same value leaves the body
// exactly as it would have been without the feature.
func TestNativeMessagesEffortMergeKeepsUnrelatedKwargsAndNoOpsOnAMatch(t *testing.T) {
	t.Run("unrelated kwargs are preserved", func(t *testing.T) {
		prov := &recordingProxyProvider{respBody: anthropicTerminalStream}
		srv := newEffortCompatTestServer(t, prov, effortSeed{appType: routing.ProviderLlamaCPP, flag: true})
		client := claudeCodeBody(false, `"high"`, `,"chat_template_kwargs":{"enable_thinking":true}`)

		got := relayMessages(t, srv, prov, client)

		want := decodeJSONObject(t, []byte(client))
		want["model"] = "upstream-model"
		want["chat_template_kwargs"] = map[string]any{"enable_thinking": true, "reasoning_effort": "high"}
		if upstream := decodeJSONObject(t, got); !reflect.DeepEqual(upstream, want) {
			t.Fatalf("upstream body = %s\nwant enable_thinking preserved next to reasoning_effort=high", got)
		}
	})

	t.Run("a matching reasoning_effort is a no-op", func(t *testing.T) {
		prov := &recordingProxyProvider{respBody: anthropicTerminalStream}
		srv := newEffortCompatTestServer(t, prov, effortSeed{appType: routing.ProviderLlamaCPP, flag: true})
		client := claudeCodeBody(false, `"high"`, `,"chat_template_kwargs":{"reasoning_effort":"high"}`)

		got := relayMessages(t, srv, prov, client)

		if want := rewriteModelField([]byte(client), "upstream-model"); !bytes.Equal(got, want) {
			t.Fatalf("upstream body = %s\nwant exactly the model-rewritten client body %s", got, want)
		}
	})
}

// TestNativeMessagesEffortLeavesTheBodyUnchanged is the "true no-op" half: the
// relayed body is the client's bytes with ONLY the model rewritten, compared
// byte for byte against rewriteModelField's own output. A row passing means the
// third edit either did not run or declined to change anything.
//
// Every refusal here is on a fixture where the OTHER conditions hold, so the one
// under test is what refuses: flag off on a capable target; flag on but a target
// that does not read the kwarg (llama_swap, ollama, an agent-managed ollama);
// and flag on with a capable target but no usable effort to deliver.
func TestNativeMessagesEffortLeavesTheBodyUnchanged(t *testing.T) {
	llama := effortSeed{appType: routing.ProviderLlamaCPP, flag: true}
	cases := []struct {
		name string
		seed effortSeed
		body string
	}{
		{"flag off on a llama.cpp target", effortSeed{appType: routing.ProviderLlamaCPP, flag: false}, claudeCodeBody(true, `"medium"`, ``)},
		{"flag off on a vLLM target", effortSeed{appType: routing.ProviderVLLM, flag: false}, claudeCodeBody(true, `"medium"`, ``)},
		{"flag on, llama_swap target", effortSeed{appType: routing.ProviderLlamaSwap, flag: true}, claudeCodeBody(true, `"medium"`, ``)},
		{"flag on, ollama target", effortSeed{appType: routing.ProviderOllama, flag: true}, claudeCodeBody(true, `"medium"`, ``)},
		{"flag on, agent-managed ollama", effortSeed{appType: routing.ProviderServerAgent, spec: agentSpec(routing.RuntimeSpecTypeOllama), flag: true}, claudeCodeBody(true, `"medium"`, ``)},
		{"flag on, capable target, blank effort", llama, claudeCodeBody(true, `"   "`, ``)},
		{"flag on, capable target, null effort", llama, claudeCodeBody(true, `null`, ``)},
		{"flag on, capable target, no output_config", llama, `{"model":"gw-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: anthropicTerminalStream}
			srv := newEffortCompatTestServer(t, prov, tc.seed)

			got := relayMessages(t, srv, prov, tc.body)

			if want := rewriteModelField([]byte(tc.body), "upstream-model"); !bytes.Equal(got, want) {
				t.Fatalf("upstream body = %s\nwant exactly the model-rewritten client body %s", got, want)
			}
			if bytes.Contains(got, []byte("chat_template_kwargs")) {
				t.Fatalf("the relayed body grew chat_template_kwargs although the gate had to refuse: %s", got)
			}
		})
	}
}

// TestNativeMessagesEffortIsNeverRejectedWhereItDoesNotApply pins the standing
// rule that the new validation exists only on an applicable, opted-in route. The
// three malformed bodies are exactly the ones that are rejected when everything
// applies (below); here every one of them must instead reach the upstream
// untouched, with the flag off and with a target that does not read the kwarg.
func TestNativeMessagesEffortIsNeverRejectedWhereItDoesNotApply(t *testing.T) {
	bodies := map[string]string{
		"effort is not a string":       claudeCodeBody(false, `3`, ``),
		"kwargs is not an object":      claudeCodeBody(false, `"low"`, `,"chat_template_kwargs":"x"`),
		"conflicting reasoning_effort": claudeCodeBody(false, `"low"`, `,"chat_template_kwargs":{"reasoning_effort":"medium"}`),
	}
	seeds := map[string]effortSeed{
		"flag off":        {appType: routing.ProviderLlamaCPP, flag: false},
		"non-capable":     {appType: routing.ProviderOllama, flag: true},
		"llama_swap":      {appType: routing.ProviderLlamaSwap, flag: true},
		"flag off, vLLM":  {appType: routing.ProviderVLLM, flag: false},
		"flag off, agent": {appType: routing.ProviderServerAgent, spec: agentSpec(routing.RuntimeSpecTypeLlamaCpp), flag: false},
		"agent ollama on": {appType: routing.ProviderServerAgent, spec: agentSpec(routing.RuntimeSpecTypeOllama), flag: true},
	}
	for seedName, seed := range seeds {
		for bodyName, body := range bodies {
			t.Run(seedName+"/"+bodyName, func(t *testing.T) {
				prov := &recordingProxyProvider{respBody: anthropicTerminalStream}
				srv := newEffortCompatTestServer(t, prov, seed)

				got := relayMessages(t, srv, prov, body)

				if want := rewriteModelField([]byte(body), "upstream-model"); !bytes.Equal(got, want) {
					t.Fatalf("upstream body = %s\nwant exactly the model-rewritten client body %s", got, want)
				}
			})
		}
	}
}

// captureEnvelopeFor loads and decodes the payload capture stored for a usage
// event. No cipher is wired, so persistCapture takes its RAM fallback and the
// blob is plain gzip.
func captureEnvelopeFor(t *testing.T, captures *store.MemoryCaptureStore, eventID string) captureEnvelope {
	t.Helper()
	row, err := captures.Capture(context.Background(), eventID)
	if err != nil {
		t.Fatalf("Capture(%q): %v", eventID, err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(row.Blob))
	if err != nil {
		t.Fatalf("capture blob is not plain gzip: %v", err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read capture blob: %v", err)
	}
	var env captureEnvelope
	if err := json.Unmarshal(plain, &env); err != nil {
		t.Fatalf("unmarshal capture envelope: %v", err)
	}
	return env
}

// TestNativeMessagesEffortRejectsInvalidRequestsBeforeDispatch drives the three
// invalid-request cases through the real server on an applicable, opted-in
// route. Each must answer 400 with the stable code and a distinct message, send
// NOTHING upstream, leave no running-connections row behind (the rejection
// happens before the active row is added), and be recorded and CAPTURED like the
// endpoint-disabled rejection -- which is the whole reason this does not go
// through writeRequestError: an uncaptured 400 would be invisible in
// Activity/Logs.
func TestNativeMessagesEffortRejectsInvalidRequestsBeforeDispatch(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantMessage string
	}{
		{"effort is not a string", claudeCodeBody(true, `3`, ``), "output_config.effort must be a string"},
		{"kwargs is not an object", claudeCodeBody(true, `"low"`, `,"chat_template_kwargs":"x"`), "chat_template_kwargs must be an object"},
		{"conflicting reasoning_effort", claudeCodeBody(true, `"low"`, `,"chat_template_kwargs":{"reasoning_effort":"medium"}`), "conflicts with output_config.effort"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captures := store.NewMemoryCaptureStore(0)
			prov := &recordingProxyProvider{respBody: anthropicTerminalStream}
			srv := newEffortCompatTestServer(t, prov, effortSeed{appType: routing.ProviderLlamaCPP, flag: true})
			srv.Captures = captures
			srv.CaptureOverride = func() bool { return true }

			rec := postMessages(srv, tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
			}
			if code := errorBodyOf(t, rec); code != "messages.invalid_output_config" {
				t.Fatalf("error code = %q, want messages.invalid_output_config", code)
			}
			if !strings.Contains(rec.Body.String(), tc.wantMessage) {
				t.Fatalf("error body = %s, want it to say %q", rec.Body.String(), tc.wantMessage)
			}
			if prov.proxyCalls != 0 {
				t.Fatalf("ProxyNative calls = %d, want 0 (an invalid request must never be dispatched)", prov.proxyCalls)
			}
			if rows := srv.Active.Snapshot(); len(rows) != 0 {
				t.Fatalf("running-connections rows = %+v, want none (the rejection precedes Active.Add)", rows)
			}

			events := srv.Usage.All()
			if len(events) != 1 {
				t.Fatalf("usage events = %d, want exactly 1 (the rejection is recorded)", len(events))
			}
			ev := events[0]
			if ev.Status != "error" || ev.ErrorCode != "messages.invalid_output_config" || ev.HTTPStatus != http.StatusBadRequest {
				t.Fatalf("usage event = %+v, want status=error error_code=messages.invalid_output_config http_status=400", ev)
			}
			if ev.Model != "gw-model" {
				t.Fatalf("usage event model = %q, want gw-model", ev.Model)
			}

			env := captureEnvelopeFor(t, captures, ev.ID)
			if env.ReqBody != tc.body {
				t.Fatalf("captured request body = %s, want the client's own bytes %s", env.ReqBody, tc.body)
			}
			if !strings.Contains(env.RespBody, "messages.invalid_output_config") {
				t.Fatalf("captured response body = %s, want the 400 the client received", env.RespBody)
			}
		})
	}
}

// TestNativeMessagesEffortCaptureStillRecordsTheClientsBody is the injection's
// counterpart of TestPassthroughResponsesCaptureStillRecordsTheClientsBody, and
// the integration-level proof that rel.raw is never written to: proxyNative
// reads it AFTER dispatch to build the capture, so an in-place edit by the
// helper would put chat_template_kwargs -- a key the client never sent -- into
// the record an operator reads as the client's request.
//
// The relayed body is re-asserted first, because comparing two identical bodies
// would pass for the wrong reason.
func TestNativeMessagesEffortCaptureStillRecordsTheClientsBody(t *testing.T) {
	captures := store.NewMemoryCaptureStore(0)
	prov := &recordingProxyProvider{respBody: anthropicTerminalStream}
	srv := newEffortCompatTestServer(t, prov, effortSeed{appType: routing.ProviderLlamaCPP, flag: true})
	srv.Captures = captures
	srv.CaptureOverride = func() bool { return true }
	client := claudeCodeBody(false, `"medium"`, ``)

	relayed := relayMessages(t, srv, prov, client)

	if !bytes.Contains(relayed, []byte(`"reasoning_effort":"medium"`)) {
		t.Fatalf("the relayed body carries no injected effort, so this test would compare two identical bodies: %s", relayed)
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want exactly 1 (the capture is keyed by that event's id)", len(events))
	}
	env := captureEnvelopeFor(t, captures, events[0].ID)
	if strings.Contains(env.ReqBody, "chat_template_kwargs") {
		t.Fatalf("the capture recorded the gateway's injected key, so it no longer shows what the client sent: %s", env.ReqBody)
	}
	if env.ReqBody != client {
		t.Fatalf("captured request body = %s, want the client's own bytes %s", env.ReqBody, client)
	}
}

// TestNativeMessagesDebugLineRecordsTheEffortInjection mirrors the
// timings_per_token debug-line test: the capture records the CLIENT's bytes, so
// the per-request debug line is the one place an operator debugging an upstream
// 400 learns that the gateway added chat_template_kwargs at all. Asserted in
// both directions on purpose -- a field emitted only when true is
// indistinguishable from a build that never had it.
func TestNativeMessagesDebugLineRecordsTheEffortInjection(t *testing.T) {
	cases := []struct {
		name string
		seed effortSeed
		body string
		want bool
	}{
		{"the gate allowed and the effort was delivered", effortSeed{appType: routing.ProviderLlamaCPP, flag: true}, claudeCodeBody(false, `"low"`, ``), true},
		{"the flag is off", effortSeed{appType: routing.ProviderLlamaCPP, flag: false}, claudeCodeBody(false, `"low"`, ``), false},
		{"no effort to deliver", effortSeed{appType: routing.ProviderLlamaCPP, flag: true}, claudeCodeBody(false, `""`, ``), false},
		{"the client had already sent the same value", effortSeed{appType: routing.ProviderLlamaCPP, flag: true}, claudeCodeBody(false, `"low"`, `,"chat_template_kwargs":{"reasoning_effort":"low"}`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			defer restore()
			prov := &recordingProxyProvider{respBody: anthropicTerminalStream}
			srv := newEffortCompatTestServer(t, prov, tc.seed)

			relayMessages(t, srv, prov, tc.body)

			found := false
			for _, rec := range buf.Snapshot() {
				if rec.Msg != "inference request (native passthrough)" {
					continue
				}
				found = true
				got, ok := rec.Attrs["reasoning_effort_injected"]
				if !ok {
					t.Fatalf("the per-request debug line carries no reasoning_effort_injected field: attrs = %+v", rec.Attrs)
				}
				if got != tc.want {
					t.Fatalf("reasoning_effort_injected = %#v, want %v", got, tc.want)
				}
			}
			if !found {
				t.Fatalf("no %q debug record was emitted at all", "inference request (native passthrough)")
			}
		})
	}
}

// TestNativeResponsesRequestNeverGetsTheMessagesEffortEdit is the only test
// that can catch a gate which forgot the flavor: the Messages tests above reach
// proxyNative with the Anthropic flavor by construction, so a
// wantsChatTemplateEffort that dropped its flavor conjunct would stay green
// there. A /v1/responses request on an opted-in token and a capable target has
// the other two conjuncts true, so the flavor is what must refuse -- even for a
// body that happens to carry an output_config.
func TestNativeResponsesRequestNeverGetsTheMessagesEffortEdit(t *testing.T) {
	prov := &recordingProxyProvider{respBody: terminalOnlyResponsesStream}
	srv := newEffortCompatTestServer(t, prov, effortSeed{appType: routing.ProviderLlamaCPP, flag: true})
	body := `{"model":"gw-model","input":"hi","output_config":{"effort":"low"},"chat_template_kwargs":"x"}`

	got := postPassthrough(t, srv, prov, "/v1/responses", body)

	if want := rewriteModelField([]byte(body), "upstream-model"); got != string(want) {
		t.Fatalf("upstream body = %s\nwant exactly the model-rewritten client body %s", got, want)
	}
}
