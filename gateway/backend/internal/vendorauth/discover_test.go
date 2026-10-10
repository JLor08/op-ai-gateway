// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	discoverAccountID     = "acct-7f3a"
	discoverClientVersion = "26.930.61225"
)

// codexCatalog mirrors the live-confirmed shape of GET codex/models: usable
// entries are visibility "list" AND supported_in_api true; everything else (and
// the many extra fields a real entry carries) must be ignored.
const codexCatalog = `{"models":[
 {"slug":"gpt-6-luna","display_name":"GPT-6-Luna","visibility":"list","supported_in_api":true,"minimal_client_version":"0.155.0","base_instructions":"You are Codex...","priority":1},
 {"slug":"gpt-6-codex","display_name":"GPT-6 Codex","visibility":"list","supported_in_api":true,"minimal_client_version":"0.100.0"},
 {"slug":"gpt-6-hidden","display_name":"GPT-6 Hidden","visibility":"hide","supported_in_api":true},
 {"slug":"gpt-6-none","display_name":"GPT-6 None","visibility":"none","supported_in_api":true},
 {"slug":"gpt-6-chat-only","display_name":"GPT-6 Chat Only","visibility":"list","supported_in_api":false},
 {"slug":"gpt-6-no-flag","display_name":"GPT-6 No Flag","visibility":"list"},
 {"slug":"gpt-6-no-visibility","display_name":"GPT-6 No Visibility","supported_in_api":true}
]}`

var codexWant = []DiscoveredModel{
	{Slug: "gpt-6-luna", DisplayName: "GPT-6-Luna"},
	{Slug: "gpt-6-codex", DisplayName: "GPT-6 Codex"},
}

const openAIModelsBody = `{"object":"list","data":[
 {"id":"gpt-5.1","object":"model","created":1,"owned_by":"system"},
 {"id":"o4-mini","object":"model","created":2,"owned_by":"system"}
]}`

var openAIModelsWant = []DiscoveredModel{
	{Slug: "gpt-5.1", DisplayName: "gpt-5.1"},
	{Slug: "o4-mini", DisplayName: "o4-mini"},
}

const anthropicModelsBody = `{"data":[
 {"type":"model","id":"claude-opus-5","display_name":"Claude Opus 5","created_at":"2026-01-01T00:00:00Z"},
 {"type":"model","id":"claude-haiku-5","display_name":"Claude Haiku 5","created_at":"2026-01-01T00:00:00Z"}
],"has_more":false,"first_id":"claude-opus-5","last_id":"claude-haiku-5"}`

var anthropicModelsWant = []DiscoveredModel{
	{Slug: "claude-opus-5", DisplayName: "Claude Opus 5"},
	{Slug: "claude-haiku-5", DisplayName: "Claude Haiku 5"},
}

// openAICompatibleModelsBody mixes the shapes the compatible providers send: bare
// OpenAI-style entries, an OpenRouter entry with a human "name", and a Gemini
// entry whose id keeps its "models/" prefix (stripping it is the portal's job).
const openAICompatibleModelsBody = `{"object":"list","data":[
 {"id":"grok-4","object":"model","created":1,"owned_by":"xai"},
 {"id":"openai/gpt-5.1","name":"OpenAI: GPT-5.1","context_length":400000},
 {"id":"models/gemini-2.5-pro","object":"model","owned_by":"google"}
]}`

var openAICompatibleModelsWant = []DiscoveredModel{
	{Slug: "grok-4", DisplayName: "grok-4"},
	{Slug: "openai/gpt-5.1", DisplayName: "OpenAI: GPT-5.1"},
	{Slug: "models/gemini-2.5-pro", DisplayName: "models/gemini-2.5-pro"},
}

type discoverCase struct {
	name string
	call func(ctx context.Context, c *http.Client, cred string) ([]DiscoveredModel, DiscoveryStatus)
	host string
	path string
	// query is the exact query the fetch must send.
	query url.Values
	// headers are the exact request headers the fetch must send for cred.
	headers func(cred string) map[string]string
	// absent are headers the fetch must NOT send.
	absent []string
	// okBody answers 2xx with a catalog that must parse to want.
	okBody string
	want   []DiscoveredModel
	// emptyList is a well-formed 2xx body that carries no usable model.
	emptyList string
	// hostile is a 2xx body whose models are fine but whose other fields echo the
	// credential back.
	hostile string
}

func discoverCases() []discoverCase {
	return []discoverCase{
		{
			name: "openai subscription",
			call: func(ctx context.Context, c *http.Client, cred string) ([]DiscoveredModel, DiscoveryStatus) {
				return DiscoverOpenAISubscriptionModels(ctx, c, cred, discoverAccountID, discoverClientVersion)
			},
			host:  "chatgpt.com",
			path:  "/backend-api/codex/models",
			query: url.Values{"client_version": {discoverClientVersion}},
			headers: func(cred string) map[string]string {
				return map[string]string{
					"Authorization":      "Bearer " + cred,
					"Chatgpt-Account-Id": discoverAccountID,
					"Originator":         "codex_cli_rs",
				}
			},
			absent:    []string{"X-Api-Key", "Anthropic-Version", "Anthropic-Beta"},
			okBody:    codexCatalog,
			want:      codexWant,
			emptyList: `{"models":[{"slug":"x","visibility":"hide","supported_in_api":true}]}`,
			hostile:   `{"models":[{"slug":"gpt-6-luna","display_name":"GPT-6-Luna","visibility":"list","supported_in_api":true,"note":"` + probeSecret + `"}],"echo":"` + probeSecret + `"}`,
		},
		{
			name: "openai api key",
			call: DiscoverOpenAIAPIKeyModels,
			host: "api.openai.com",
			path: "/v1/models",
			headers: func(cred string) map[string]string {
				return map[string]string{"Authorization": "Bearer " + cred}
			},
			absent:    []string{"X-Api-Key", "Anthropic-Version", "Anthropic-Beta", "Chatgpt-Account-Id", "Originator"},
			okBody:    openAIModelsBody,
			want:      openAIModelsWant,
			emptyList: `{"object":"list","data":[]}`,
			hostile:   `{"data":[{"id":"gpt-5.1","owned_by":"` + probeSecret + `"}],"echo":"` + probeSecret + `"}`,
		},
		{
			name: "anthropic api key",
			call: DiscoverAnthropicAPIKeyModels,
			host: "api.anthropic.com",
			path: "/v1/models",
			// Anthropic pages /v1/models at 20 by default; the fetch asks for the
			// maximum page so a catalog beyond 20 is not silently cut.
			query: url.Values{"limit": {"1000"}},
			headers: func(cred string) map[string]string {
				return map[string]string{"X-Api-Key": cred, "Anthropic-Version": "2023-06-01"}
			},
			absent:    []string{"Authorization", "Anthropic-Beta"},
			okBody:    anthropicModelsBody,
			want:      anthropicModelsWant,
			emptyList: `{"data":[],"has_more":false}`,
			hostile:   `{"data":[{"id":"claude-opus-5","display_name":"Claude Opus 5","note":"` + probeSecret + `"}],"echo":"` + probeSecret + `"}`,
		},
		{
			name:  "anthropic subscription",
			call:  DiscoverAnthropicSubscriptionModels,
			host:  "api.anthropic.com",
			path:  "/v1/models",
			query: url.Values{"limit": {"1000"}},
			headers: func(cred string) map[string]string {
				return map[string]string{
					"Authorization":     "Bearer " + cred,
					"Anthropic-Beta":    "oauth-2025-04-20",
					"Anthropic-Version": "2023-06-01",
				}
			},
			absent:    []string{"X-Api-Key"},
			okBody:    anthropicModelsBody,
			want:      anthropicModelsWant,
			emptyList: `{"data":[],"has_more":false}`,
			hostile:   `{"data":[{"id":"claude-opus-5","display_name":"Claude Opus 5","note":"` + probeSecret + `"}],"echo":"` + probeSecret + `"}`,
		},
		{
			// The caller-composed URL: the portal builds {base}{prefix}/models from the
			// provider preset, this package only asks it.
			name: "openai compatible api key",
			call: func(ctx context.Context, c *http.Client, cred string) ([]DiscoveredModel, DiscoveryStatus) {
				return DiscoverOpenAICompatibleModels(ctx, c, "https://api.x.ai/v1/models", cred)
			},
			host: "api.x.ai",
			path: "/v1/models",
			headers: func(cred string) map[string]string {
				return map[string]string{"Authorization": "Bearer " + cred}
			},
			absent:    []string{"X-Api-Key", "Anthropic-Version", "Anthropic-Beta", "Chatgpt-Account-Id", "Originator"},
			okBody:    openAICompatibleModelsBody,
			want:      openAICompatibleModelsWant,
			emptyList: `{"object":"list","data":[]}`,
			hostile:   `{"data":[{"id":"grok-4","owned_by":"` + probeSecret + `"}],"echo":"` + probeSecret + `"}`,
		},
	}
}

// discoverCompatibleAt adapts DiscoverOpenAICompatibleModels, which takes the
// caller-composed models URL, to the credential-only call shape of the table tests.
func discoverCompatibleAt(modelsURL string) func(ctx context.Context, c *http.Client, cred string) ([]DiscoveredModel, DiscoveryStatus) {
	return func(ctx context.Context, c *http.Client, cred string) ([]DiscoveredModel, DiscoveryStatus) {
		return DiscoverOpenAICompatibleModels(ctx, c, modelsURL, cred)
	}
}

func assertNoSecret(t *testing.T, label string, models []DiscoveredModel) {
	t.Helper()
	for _, m := range models {
		if strings.Contains(m.Slug, probeSecret) || strings.Contains(m.DisplayName, probeSecret) {
			t.Errorf("%s: model %+v contains the credential", label, m)
		}
	}
}

func assertUnverifiable(t *testing.T, label string, got []DiscoveredModel, status DiscoveryStatus) {
	t.Helper()
	if status != DiscoveryUnverifiable {
		t.Errorf("%s: status = %v, want unverifiable", label, status)
	}
	if len(got) != 0 {
		t.Errorf("%s: models = %+v, want none", label, got)
	}
}

func TestDiscoveryStatusString(t *testing.T) {
	tests := map[DiscoveryStatus]string{
		DiscoveryOK:           "ok",
		DiscoveryUnverifiable: "unverifiable",
		DiscoveryStatus(0):    "unknown",
	}
	for status, want := range tests {
		if got := status.String(); got != want {
			t.Errorf("DiscoveryStatus(%d).String() = %q, want %q", int(status), got, want)
		}
	}
}

func TestDiscoverRequestShape(t *testing.T) {
	for _, tc := range discoverCases() {
		t.Run(tc.name, func(t *testing.T) {
			client, rec := newProbeClient(t, respondWith(http.StatusOK, tc.okBody))
			tc.call(context.Background(), client, probeSecret)

			reqs := rec.all()
			if len(reqs) != 1 {
				t.Fatalf("requests = %d, want exactly 1", len(reqs))
			}
			got := reqs[0]
			if got.Method != http.MethodGet {
				t.Errorf("method = %s, want GET", got.Method)
			}
			if got.Host != tc.host {
				t.Errorf("host = %q, want %q", got.Host, tc.host)
			}
			if got.Path != tc.path {
				t.Errorf("path = %q, want %q", got.Path, tc.path)
			}
			wantQuery := tc.query
			if wantQuery == nil {
				wantQuery = url.Values{}
			}
			if !reflect.DeepEqual(got.Query, wantQuery) {
				t.Errorf("query = %v, want %v", got.Query, wantQuery)
			}
			if got.Body != "" {
				t.Errorf("body = %q, want none", got.Body)
			}
			for name, want := range tc.headers(probeSecret) {
				if v := got.Header.Get(name); v != want {
					t.Errorf("header %s = %q, want %q", name, v, want)
				}
			}
			for _, name := range tc.absent {
				if v := got.Header.Get(name); v != "" {
					t.Errorf("header %s = %q, want it absent", name, v)
				}
			}
		})
	}
}

func TestDiscoverParsesTheCatalog(t *testing.T) {
	for _, tc := range discoverCases() {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newProbeClient(t, respondWith(http.StatusOK, tc.okBody))
			got, status := tc.call(context.Background(), client, probeSecret)
			if status != DiscoveryOK {
				t.Fatalf("status = %v, want ok", status)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("models = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDiscoverOpenAISubscriptionKeepsOnlyListedAPIModels(t *testing.T) {
	// Only visibility=="list" AND supported_in_api==true are usable chat models:
	// hide and none are internal or retired, supported_in_api=false is not
	// reachable through the Responses API, and a missing flag is not a "true".
	client, _ := newProbeClient(t, respondWith(http.StatusOK, codexCatalog))
	got, status := DiscoverOpenAISubscriptionModels(context.Background(), client, probeSecret, discoverAccountID, discoverClientVersion)
	if status != DiscoveryOK {
		t.Fatalf("status = %v, want ok", status)
	}
	if !reflect.DeepEqual(got, codexWant) {
		t.Errorf("models = %+v, want %+v", got, codexWant)
	}
	for _, m := range got {
		switch m.Slug {
		case "gpt-6-hidden", "gpt-6-none", "gpt-6-chat-only", "gpt-6-no-flag", "gpt-6-no-visibility":
			t.Errorf("model %q is not a listed API model but was kept", m.Slug)
		}
	}
}

func TestDiscoverOpenAISubscriptionSendsTheClientVersion(t *testing.T) {
	// The backend FILTERS OUT every model whose minimal_client_version exceeds the
	// client_version sent, so the caller's value must reach the wire verbatim.
	tests := []struct {
		name    string
		version string
		want    string
	}{
		{name: "caller value", version: "0.155.0", want: "0.155.0"},
		{name: "newer caller value", version: "99.1.2", want: "99.1.2"},
		{name: "empty falls back to the default", version: "", want: CodexModelsClientVersionDefault},
		{name: "blank falls back to the default", version: "  ", want: CodexModelsClientVersionDefault},
		{name: "query metacharacters are escaped", version: "1.0&x=1 #y", want: "1.0&x=1 #y"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, rec := newProbeClient(t, respondWith(http.StatusOK, codexCatalog))
			DiscoverOpenAISubscriptionModels(context.Background(), client, probeSecret, discoverAccountID, tc.version)
			reqs := rec.all()
			if len(reqs) != 1 {
				t.Fatalf("requests = %d, want 1", len(reqs))
			}
			if got := reqs[0].Query.Get("client_version"); got != tc.want {
				t.Errorf("client_version = %q, want %q", got, tc.want)
			}
			if len(reqs[0].Query) != 1 {
				t.Errorf("query = %v, want only client_version (no injected parameter)", reqs[0].Query)
			}
		})
	}
}

func TestDiscoverOpenAISubscriptionWithoutAccountIDOmitsTheHeader(t *testing.T) {
	client, rec := newProbeClient(t, respondWith(http.StatusOK, codexCatalog))
	DiscoverOpenAISubscriptionModels(context.Background(), client, probeSecret, "", discoverClientVersion)
	reqs := rec.all()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	if _, present := reqs[0].Header["Chatgpt-Account-Id"]; present {
		t.Errorf("ChatGPT-Account-Id = %q, want the header omitted rather than sent empty", reqs[0].Header.Get("ChatGPT-Account-Id"))
	}
	if reqs[0].Header.Get("Authorization") != "Bearer "+probeSecret {
		t.Error("the bearer token must still be sent")
	}
}

func TestDiscoverDisplayNameFallsBackToTheSlug(t *testing.T) {
	tests := []struct {
		name string
		call func(ctx context.Context, c *http.Client, cred string) ([]DiscoveredModel, DiscoveryStatus)
		body string
		want []DiscoveredModel
	}{
		{
			name: "openai subscription without display_name",
			call: func(ctx context.Context, c *http.Client, cred string) ([]DiscoveredModel, DiscoveryStatus) {
				return DiscoverOpenAISubscriptionModels(ctx, c, cred, discoverAccountID, discoverClientVersion)
			},
			body: `{"models":[{"slug":"a","visibility":"list","supported_in_api":true},{"slug":"b","display_name":"","visibility":"list","supported_in_api":true}]}`,
			want: []DiscoveredModel{{Slug: "a", DisplayName: "a"}, {Slug: "b", DisplayName: "b"}},
		},
		{
			name: "openai api key is always the id",
			call: DiscoverOpenAIAPIKeyModels,
			body: `{"data":[{"id":"a","display_name":"ignored"}]}`,
			want: []DiscoveredModel{{Slug: "a", DisplayName: "a"}},
		},
		{
			name: "openai compatible: display_name, then name, then the id",
			call: func(ctx context.Context, c *http.Client, cred string) ([]DiscoveredModel, DiscoveryStatus) {
				return DiscoverOpenAICompatibleModels(ctx, c, "https://gw.example.test/v1/models", cred)
			},
			body: `{"data":[{"id":"a"},{"id":"b","name":"B Name"},{"id":"c","display_name":"C Display","name":"C Name"},{"id":"d","display_name":"","name":""},{"id":"e","display_name":"","name":"E Name"}]}`,
			want: []DiscoveredModel{
				{Slug: "a", DisplayName: "a"},
				{Slug: "b", DisplayName: "B Name"},
				{Slug: "c", DisplayName: "C Display"},
				{Slug: "d", DisplayName: "d"},
				{Slug: "e", DisplayName: "E Name"},
			},
		},
		{
			// OpenAI's listing is always the id, whatever else an entry carries: the name
			// fallback must not leak into it.
			name: "openai api key ignores name",
			call: DiscoverOpenAIAPIKeyModels,
			body: `{"data":[{"id":"a","name":"Ignored Name"}]}`,
			want: []DiscoveredModel{{Slug: "a", DisplayName: "a"}},
		},
		{
			name: "anthropic api key without display_name",
			call: DiscoverAnthropicAPIKeyModels,
			body: `{"data":[{"id":"a"},{"id":"b","display_name":""},{"id":"c","display_name":"C"}]}`,
			want: []DiscoveredModel{{Slug: "a", DisplayName: "a"}, {Slug: "b", DisplayName: "b"}, {Slug: "c", DisplayName: "C"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newProbeClient(t, respondWith(http.StatusOK, tc.body))
			got, status := tc.call(context.Background(), client, probeSecret)
			if status != DiscoveryOK {
				t.Fatalf("status = %v, want ok", status)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("models = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDiscoverSkipsEntriesWithoutASlug(t *testing.T) {
	bodies := map[string]string{
		"openai subscription": `{"models":[{"display_name":"No Slug","visibility":"list","supported_in_api":true},{"slug":"","visibility":"list","supported_in_api":true},{"slug":"ok","visibility":"list","supported_in_api":true}]}`,
		"openai api key":      `{"data":[{"object":"model"},{"id":""},{"id":"ok"}]}`,
		"anthropic api key":   `{"data":[{"display_name":"No Id"},{"id":""},{"id":"ok"}]}`,
		"openai compatible":   `{"data":[{"name":"No Id"},{"id":""},{"id":"ok"}]}`,
	}
	calls := map[string]func(ctx context.Context, c *http.Client, cred string) ([]DiscoveredModel, DiscoveryStatus){
		"openai subscription": func(ctx context.Context, c *http.Client, cred string) ([]DiscoveredModel, DiscoveryStatus) {
			return DiscoverOpenAISubscriptionModels(ctx, c, cred, discoverAccountID, discoverClientVersion)
		},
		"openai api key":    DiscoverOpenAIAPIKeyModels,
		"anthropic api key": DiscoverAnthropicAPIKeyModels,
		"openai compatible": discoverCompatibleAt("https://gw.example.test/v1/models"),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			client, _ := newProbeClient(t, respondWith(http.StatusOK, body))
			got, status := calls[name](context.Background(), client, probeSecret)
			if status != DiscoveryOK {
				t.Fatalf("status = %v, want ok", status)
			}
			if len(got) != 1 || got[0].Slug != "ok" {
				t.Errorf("models = %+v, want only the entry that has a slug", got)
			}
		})
	}
}

func TestDiscoverOneMalformedEntryDoesNotSinkTheRest(t *testing.T) {
	bodies := map[string]string{
		"openai subscription": `{"models":[42,"str",null,{"slug":7},{"slug":"bad","visibility":"list","supported_in_api":"yes"},{"slug":"ok","display_name":"OK","visibility":"list","supported_in_api":true}]}`,
		"openai api key":      `{"data":[42,"str",null,{"id":7},{"id":"ok"}]}`,
		"anthropic api key":   `{"data":[42,"str",null,{"id":7},{"id":"ok","display_name":"OK"}]}`,
		"openai compatible":   `{"data":[42,"str",null,{"id":7},{"id":"ok","name":"OK"}]}`,
	}
	calls := map[string]func(ctx context.Context, c *http.Client, cred string) ([]DiscoveredModel, DiscoveryStatus){
		"openai subscription": func(ctx context.Context, c *http.Client, cred string) ([]DiscoveredModel, DiscoveryStatus) {
			return DiscoverOpenAISubscriptionModels(ctx, c, cred, discoverAccountID, discoverClientVersion)
		},
		"openai api key":    DiscoverOpenAIAPIKeyModels,
		"anthropic api key": DiscoverAnthropicAPIKeyModels,
		"openai compatible": discoverCompatibleAt("https://gw.example.test/v1/models"),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			client, _ := newProbeClient(t, respondWith(http.StatusOK, body))
			got, status := calls[name](context.Background(), client, probeSecret)
			if status != DiscoveryOK {
				t.Fatalf("status = %v, want ok", status)
			}
			if len(got) != 1 || got[0].Slug != "ok" {
				t.Errorf("models = %+v, want exactly the one well-formed entry", got)
			}
		})
	}
}

func TestDiscoverNon2xxIsUnverifiable(t *testing.T) {
	// A 401 is NOT reported as a bad credential here: discovery only ever says
	// "got a list" or "could not verify", so a caller keeps its static seed.
	statuses := []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest, http.StatusNotFound,
		http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
	}
	for _, tc := range discoverCases() {
		for _, code := range statuses {
			// The body is a perfectly good catalog: the status alone decides.
			client, _ := newProbeClient(t, respondWith(code, tc.okBody))
			got, status := tc.call(context.Background(), client, probeSecret)
			assertUnverifiable(t, tc.name+" / HTTP "+http.StatusText(code), got, status)
		}
	}
}

func TestDiscoverJunkBodiesAreUnverifiable(t *testing.T) {
	junk := map[string]string{
		"empty":               ``,
		"html":                `<html><body>Just a moment...</body></html>`,
		"truncated json":      `{"models":[{"slug":"gpt-6-luna","visi`,
		"null":                `null`,
		"number":              `42`,
		"string":              `"models"`,
		"array at the top":    `[{"slug":"a","visibility":"list","supported_in_api":true},{"id":"a"}]`,
		"empty object":        `{}`,
		"foreign object":      `{"object":"error","message":"nope"}`,
		"models is a string":  `{"models":"gpt-6-luna","data":"gpt-5.1"}`,
		"models is an object": `{"models":{"a":1},"data":{"a":1}}`,
		"models is null":      `{"models":null,"data":null}`,
		"error envelope":      `{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
		"nothing but junk":    `{"models":[1,2,3],"data":[1,2,3]}`,
	}
	for _, tc := range discoverCases() {
		for name, body := range junk {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				client, _ := newProbeClient(t, respondWith(http.StatusOK, body))
				got, status := tc.call(context.Background(), client, probeSecret)
				assertUnverifiable(t, tc.name+" / "+name, got, status)
			})
		}
	}
}

func TestDiscoverAnEmptyUsableListIsUnverifiable(t *testing.T) {
	// A 2xx that yields no usable model (an empty catalog, or every entry filtered
	// out) is not a list a caller may replace its models with: it could be a
	// changed schema, and an empty result would wipe the working catalog.
	for _, tc := range discoverCases() {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newProbeClient(t, respondWith(http.StatusOK, tc.emptyList))
			got, status := tc.call(context.Background(), client, probeSecret)
			assertUnverifiable(t, tc.name, got, status)
		})
	}
}

func TestDiscoverTransportFailuresAreUnverifiable(t *testing.T) {
	for _, tc := range discoverCases() {
		t.Run(tc.name+"/client timeout", func(t *testing.T) {
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			client, _ := newProbeClient(t, func(_ http.ResponseWriter, r *http.Request) {
				select {
				case <-release:
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
			})
			client.Timeout = 50 * time.Millisecond
			got, status := tc.call(context.Background(), client, probeSecret)
			assertUnverifiable(t, "client timeout", got, status)
		})

		t.Run(tc.name+"/context deadline", func(t *testing.T) {
			client, _ := newProbeClient(t, func(_ http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			got, status := tc.call(ctx, client, probeSecret)
			assertUnverifiable(t, "context deadline", got, status)
		})

		t.Run(tc.name+"/context canceled", func(t *testing.T) {
			client, _ := newProbeClient(t, respondWith(http.StatusOK, tc.okBody))
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			got, status := tc.call(ctx, client, probeSecret)
			assertUnverifiable(t, "context canceled", got, status)
		})

		t.Run(tc.name+"/transport error", func(t *testing.T) {
			// The transport error text embeds the credential: nothing the caller
			// gets back may carry it.
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial failed for " + probeSecret)
			})}
			got, status := tc.call(context.Background(), client, probeSecret)
			assertUnverifiable(t, "transport error", got, status)
			assertNoSecret(t, "transport error", got)
		})

		t.Run(tc.name+"/connection refused", func(t *testing.T) {
			srv := httptest.NewServer(http.NotFoundHandler())
			target, _ := url.Parse(srv.URL)
			srv.Close() // nothing listens on target any more
			client := &http.Client{Transport: rewriteTransport{target: target}}
			got, status := tc.call(context.Background(), client, probeSecret)
			assertUnverifiable(t, "connection refused", got, status)
		})
	}
}

func TestDiscoverDoesNotFollowRedirects(t *testing.T) {
	// x-api-key is not stripped by net/http on a cross-host redirect, so a
	// redirect must end the fetch rather than carry the credential elsewhere.
	for _, tc := range discoverCases() {
		t.Run(tc.name, func(t *testing.T) {
			client, rec := newProbeClient(t, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "https://elsewhere.example.test/steal", http.StatusFound)
			})
			got, status := tc.call(context.Background(), client, probeSecret)
			if n := len(rec.all()); n != 1 {
				t.Errorf("requests = %d, want 1 (the redirect must not be followed)", n)
			}
			assertUnverifiable(t, tc.name, got, status)
		})
	}
}

func TestDiscoverLeavesCallersClientUntouched(t *testing.T) {
	client, _ := newProbeClient(t, respondWith(http.StatusOK, `{}`))
	client.Timeout = 7 * time.Second
	for _, tc := range discoverCases() {
		tc.call(context.Background(), client, probeSecret)
	}
	if client.CheckRedirect != nil {
		t.Error("discovery mutated the caller's http.Client.CheckRedirect")
	}
	if client.Timeout != 7*time.Second {
		t.Errorf("client.Timeout = %v, want it untouched", client.Timeout)
	}
}

func TestDiscoverNeverReturnsTheCredential(t *testing.T) {
	// A hostile or careless vendor that echoes the credential in the fields around
	// the models must not get it into what the caller receives.
	for _, tc := range discoverCases() {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newProbeClient(t, respondWith(http.StatusOK, tc.hostile))
			got, status := tc.call(context.Background(), client, probeSecret)
			if status != DiscoveryOK || len(got) != 1 {
				t.Fatalf("got %+v / %v, want the one model with status ok", got, status)
			}
			assertNoSecret(t, tc.name, got)
		})
	}
}

func TestDiscoverTrustsTheVendorsSlugsAsSent(t *testing.T) {
	// Slug validation (length, charset, count) is the service's job, not this
	// package's: whatever the vendor sent is returned, decoded safely.
	long := strings.Repeat("m", 500)
	body := `{"data":[{"id":"` + long + `"},{"id":"weird slug/with:chars"},{"id":"dup"},{"id":"dup"}]}`
	client, _ := newProbeClient(t, respondWith(http.StatusOK, body))
	got, status := DiscoverOpenAIAPIKeyModels(context.Background(), client, probeSecret)
	if status != DiscoveryOK {
		t.Fatalf("status = %v, want ok", status)
	}
	want := []DiscoveredModel{
		{Slug: long, DisplayName: long},
		{Slug: "weird slug/with:chars", DisplayName: "weird slug/with:chars"},
		{Slug: "dup", DisplayName: "dup"},
		{Slug: "dup", DisplayName: "dup"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("models = %+v, want the vendor's list untouched", got)
	}
}

func TestDiscoverPreservesTheVendorsOrder(t *testing.T) {
	client, _ := newProbeClient(t, respondWith(http.StatusOK, `{"data":[{"id":"z"},{"id":"a"},{"id":"m"}]}`))
	got, _ := DiscoverOpenAIAPIKeyModels(context.Background(), client, probeSecret)
	var slugs []string
	for _, m := range got {
		slugs = append(slugs, m.Slug)
	}
	if want := []string{"z", "a", "m"}; !reflect.DeepEqual(slugs, want) {
		t.Errorf("slugs = %v, want %v", slugs, want)
	}
}

// The compatible discoverer asks exactly the URL the caller composed, wherever it
// points (a Custom endpoint has an arbitrary root and path), and never adds the
// OpenAI/Anthropic query or headers of the fixed-URL fetchers.
func TestDiscoverOpenAICompatibleRequestsTheGivenURL(t *testing.T) {
	cases := []struct {
		name, modelsURL, wantPath string
	}{
		{"x.ai", "https://api.x.ai/v1/models", "/v1/models"},
		{"kilo gateway prefix", "https://api.kilo.ai/api/gateway/models", "/api/gateway/models"},
		{"gemini openai prefix", "https://generativelanguage.googleapis.com/v1beta/openai/models", "/v1beta/openai/models"},
		{"custom root", "https://gw.example.test/root/v1/models", "/root/v1/models"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, rec := newProbeClient(t, respondWith(http.StatusOK, openAICompatibleModelsBody))
			got, status := DiscoverOpenAICompatibleModels(context.Background(), client, tc.modelsURL, probeSecret)
			if status != DiscoveryOK || !reflect.DeepEqual(got, openAICompatibleModelsWant) {
				t.Fatalf("got %+v / %v, want the catalog with status ok", got, status)
			}
			reqs := rec.all()
			if len(reqs) != 1 {
				t.Fatalf("requests = %d, want exactly 1", len(reqs))
			}
			parsed, err := url.Parse(tc.modelsURL)
			if err != nil {
				t.Fatal(err)
			}
			wantHost := parsed.Host
			if reqs[0].Host != wantHost || reqs[0].Path != tc.wantPath || len(reqs[0].Query) != 0 {
				t.Errorf("request = host %q path %q query %v, want %q %q and no query", reqs[0].Host, reqs[0].Path, reqs[0].Query, wantHost, tc.wantPath)
			}
			if v := reqs[0].Header.Get("Authorization"); v != "Bearer "+probeSecret {
				t.Errorf("Authorization = %q, want the bearer key", v)
			}
		})
	}
}

func TestDiscoverOpenAICompatibleWithAnUnbuildableURLIsUnverifiable(t *testing.T) {
	client, rec := newProbeClient(t, respondWith(http.StatusOK, openAICompatibleModelsBody))
	got, status := DiscoverOpenAICompatibleModels(context.Background(), client, "http://[::1", probeSecret)
	assertUnverifiable(t, "unbuildable url", got, status)
	if n := len(rec.all()); n != 0 {
		t.Errorf("requests = %d, want none for a URL that cannot be built", n)
	}
}

func TestCodexModelsConstants(t *testing.T) {
	if CodexModelsURL != "https://chatgpt.com/backend-api/codex/models" {
		t.Errorf("CodexModelsURL = %q", CodexModelsURL)
	}
	if CodexModelsOriginator != "codex_cli_rs" {
		t.Errorf("CodexModelsOriginator = %q", CodexModelsOriginator)
	}
	if CodexModelsClientVersionDefault != "26.930.61225" {
		t.Errorf("CodexModelsClientVersionDefault = %q", CodexModelsClientVersionDefault)
	}
}

// padCatalog returns body (a JSON object) with a junk field appended so the whole
// answer is at least size bytes: the catalog entries stay where they were and the
// weight sits after them, like the many fields of a real Codex catalog entry.
func padCatalog(body string, size int) string {
	const open, closing = `,"padding":"`, `"}`
	head := strings.TrimSuffix(strings.TrimSpace(body), "}")
	pad := size - len(head) - len(open) - len(closing)
	if pad < 0 {
		pad = 0
	}
	return head + open + strings.Repeat("x", pad) + closing
}

func TestDiscoverParsesACatalogBeyondTheValidatorBodyCap(t *testing.T) {
	// The live codex/models body is ~0.6 MiB (613191 bytes) and grows with every
	// model; the 1 MiB cap the credential validators share would soon truncate it
	// into an unparseable body, i.e. a silent "unverifiable" for a working
	// credential. Discovery has its own, larger limit, so a catalog between the two
	// caps still parses.
	if maxDiscoveryResponseBytes <= maxResponseBytes {
		t.Errorf("maxDiscoveryResponseBytes = %d, want it larger than the validator cap %d", maxDiscoveryResponseBytes, maxResponseBytes)
	}
	for _, tc := range discoverCases() {
		for _, size := range []int{maxResponseBytes + 1, 2 << 20, 6 << 20} {
			t.Run(tc.name+"/"+sizeLabel(size), func(t *testing.T) {
				body := padCatalog(tc.okBody, size)
				if len(body) < size {
					t.Fatalf("test body is %d bytes, want at least %d", len(body), size)
				}
				client, _ := newProbeClient(t, respondWith(http.StatusOK, body))
				got, status := tc.call(context.Background(), client, probeSecret)
				if status != DiscoveryOK {
					t.Fatalf("status = %v, want ok for a %d byte catalog", status, len(body))
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Errorf("models = %+v, want %+v", got, tc.want)
				}
			})
		}
	}
}

func TestDiscoverBodyBeyondItsOwnCapIsUnverifiable(t *testing.T) {
	// The discovery limit is larger, not absent: a body past it is cut off,
	// no longer parses, and is Unverifiable (the caller keeps its models).
	for _, tc := range discoverCases() {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newProbeClient(t, respondWith(http.StatusOK, padCatalog(tc.okBody, maxDiscoveryResponseBytes+4096)))
			got, status := tc.call(context.Background(), client, probeSecret)
			assertUnverifiable(t, tc.name, got, status)
		})
	}
}

func TestSendKeepsTheValidatorBodyCap(t *testing.T) {
	// The discovery limit must not leak into the shared send() the credential
	// validators and the token endpoints use: that one stays at maxResponseBytes.
	client, _ := newProbeClient(t, respondWith(http.StatusOK, strings.Repeat("x", maxResponseBytes+4096)))
	req, err := http.NewRequest(http.MethodGet, "https://probe.test/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	status, body, err := send(client, req)
	if err != nil || status != http.StatusOK {
		t.Fatalf("send = %d, %v", status, err)
	}
	if len(body) != maxResponseBytes {
		t.Fatalf("send read %d bytes, want the %d byte validator cap", len(body), maxResponseBytes)
	}
}

func sizeLabel(size int) string {
	switch {
	case size <= maxResponseBytes+1:
		return "just over 1MiB"
	case size <= 2<<20:
		return "2MiB"
	default:
		return "6MiB"
	}
}
