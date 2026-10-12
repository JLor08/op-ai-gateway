# Native Google Gemini dialect client — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a native Google Gemini `generateContent` dialect provider so the gateway renders the neutral `inference.Request` to Gemini's own wire and parses Gemini back, as a first-class provider kind alongside the existing OpenAI-compatibility shim.

**Architecture:** A new leaf package `internal/geminiwire` owns the Gemini wire structs and both neutral↔Gemini converters (outbound halves built now; inbound reserved for #204). A new `provider.GeminiClient` (modeled on `AnthropicClient`) owns HTTP/auth/redirect/timeout/capture/SSE and delegates render/parse to `geminiwire`. A distinct vendor id (`google_native`) with a `Dialect` preset field selects the native provider kind (`vendor_gemini`) at resolve time; the shim `google` vendor is untouched.

**Tech Stack:** Go 1.x backend (`gateway/backend`), React + TypeScript frontend (`gateway/frontend`), `net/http`, `encoding/json`, `go test`/`vitest`.

**Spec:** `../specs/2026-10-11-native-gemini-dialect-design.md` (this plan's sibling; the plan argues from it — read both).

## Global Constraints

- **geminiwire imports only `internal/inference`** among internal packages — nothing else, ever. It returns a package-local `Result`, never `provider.Response` (provider imports geminiwire).
- **Credential header is `x-goog-api-key`** (raw, via `applyUpstreamAuth`). Never the `?key=` query param (it leaks into wrapped `*url.Error` text).
- **Thinking-budget values are verbatim:** `reasoning_effort` → Gemini-2.5 `thinkingBudget` minimal/low→`1024`, medium→`8192`, high→`24576`; Gemini-3.x → `thinkingLevel` low/medium/high.
- **No DB schema change → no Postgres leg.** `vendor_accounts.vendor` is free text.
- **Go gates:** `go build ./...`, `go vet ./...`, `go test ./...`, and golangci-lint (`golangci-lint fmt --diff` + `golangci-lint run`) per Go module — gofumpt + gocritic are enforced and are NOT covered by `gofmt`/`go vet`. Run archtest with `-count=1` (it caches and can report stale `ok`).
- **Import grouping:** the module path `op-ai-gateway` is dotless, so gci/gofumpt treats `op-ai-gateway/internal/...` imports as stdlib and sorts them into the SINGLE import block (one sorted group, as in `internal/provider/anthropic_messages.go`). The separate stdlib/internal import groups shown in this plan's code blocks are illustrative — run `golangci-lint fmt` and accept its single-block ordering; a merged sorted import block is correct, not a deviation.
- **Frontend gates:** `npm run test` (vitest) and prettier `npm run format:check` (CI runs format:check, which local test/build/lint miss).
- **Docs gate:** `check-docs` must pass. Do NOT write the branch-local working-docs directory path as a literal string in any committed file; reference this plan's spec only by the relative `../specs/...` path above.
- **Repo-facing text (code comments, docs, commit messages) in English.** Commit bodies are substantive (the repo's squash-merge pre-fills the squash description from the commit message body).
- **No attribution/trailer lines in commit messages.**
- **SonarQube branch-findings must be 0** (new-code / branch-attributed) at the gate.
- **VERIFY-LIVE items** (spec §11) are validated at implementation against a real key where one exists; where none is available in-session, implement the conservative default the spec names and leave a `// VERIFY-LIVE:` comment — they do not block the plan.

## Review Focus

Input classes the spec implies that a reasonable Gemini client will hit but that a happy-path test suite can miss — each is pinned to a test in the owning task:

- **Prompt-blocked response** (HTTP 200, `candidates` empty, `promptFeedback.blockReason` set): the parser must not index `candidates[0]`; it yields empty text + `content_filter`. → Task 3.
- **Tool-result turn with no matching prior assistant tool call** (foreign/truncated history): `functionResponse.name` resolution must fall back without panicking or emitting a wrong name. → Task 2.
- **Stream that EOFs with no `finishReason` chunk** (and no `[DONE]` sentinel exists in Gemini SSE): must surface `ErrUnavailable`, never a silent `Completed`. → Task 5.
- **Absent / non-object / invalid function-call arguments** on both render and parse: must normalize to `{}` rather than fail the turn. → Task 2 (render tool text) and Task 3 (parse args).
- **Cross-host redirect on the translate path**: `x-goog-api-key` must not be carried to the redirect target (net/http only strips `Authorization`). → Task 5.

---

### Task 1: `geminiwire` package — wire structs, tool-call-id codec, archtest edge

**Files:**
- Create: `gateway/backend/internal/geminiwire/wire.go`
- Create: `gateway/backend/internal/geminiwire/toolcallid.go`
- Test: `gateway/backend/internal/geminiwire/toolcallid_test.go`

**Interfaces:**
- Produces (consumed by Tasks 2, 3, 5): all wire structs below; `encodeToolCallID(name, signature string) string` and `decodeToolCallID(id string) (name, signature string, encoded bool)` (package-private — render/parse call them; same-package tests exercise them).
- Consumes: `encoding/json`, `encoding/base64` (stdlib only). **No archtest edit in this task** — `geminiwire` imports no internal package yet, so adding any `allowedDeps` entry would fail `TestAllowlistFrozen` (it checks that every listed edge is actually imported). The `geminiwire → inference` allowlist entry lands in Task 2 (first converter); the `provider → geminiwire` entry lands in Task 5.

- [ ] **Step 1: Write the failing test for the tool-call-id codec**

`gateway/backend/internal/geminiwire/toolcallid_test.go`:

```go
package geminiwire

import "testing"

func TestToolCallIDRoundTrip(t *testing.T) {
	cases := []struct{ name, sig string }{
		{"get_weather", "sig-abc=="},
		{"f", ""},
		{"", "onlysig"},
		{"name/with:odd.chars", "a+b/c=="},
	}
	for _, c := range cases {
		id := encodeToolCallID(c.name, c.sig)
		gotName, gotSig, ok := decodeToolCallID(id)
		if !ok {
			t.Fatalf("decode(%q) ok=false, want true", id)
		}
		if gotName != c.name || gotSig != c.sig {
			t.Fatalf("round-trip %q/%q -> %q/%q", c.name, c.sig, gotName, gotSig)
		}
	}
}

func TestDecodeToolCallIDForeign(t *testing.T) {
	// Ids from other dialects (Anthropic toolu_*, OpenAI call_*) are not ours.
	for _, id := range []string{"", "call_123", "toolu_abc", "gcw1:not-base64!!"} {
		if _, _, ok := decodeToolCallID(id); ok {
			t.Fatalf("decodeToolCallID(%q) ok=true, want false", id)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd gateway/backend && go test ./internal/geminiwire/ -run TestToolCallID -run TestDecodeToolCallID`
Expected: FAIL — package/functions do not exist.

- [ ] **Step 3: Write the wire structs**

`gateway/backend/internal/geminiwire/wire.go`:

```go
// Package geminiwire holds Google Gemini generateContent wire types and the
// converters between them and the neutral inference model. It is a leaf: among
// internal packages it imports only internal/inference, so both the outbound
// client (internal/provider) and the future inbound endpoint (#204) can share
// one translation home. It never imports internal/provider, so it returns a
// package-local Result rather than provider.Response.
package geminiwire

import "encoding/json"

type GenerateContentRequest struct {
	Contents          []Content         `json:"contents,omitempty"`
	SystemInstruction *Content          `json:"systemInstruction,omitempty"`
	Tools             []Tool            `json:"tools,omitempty"`
	ToolConfig        *ToolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *GenerationConfig `json:"generationConfig,omitempty"`
	SafetySettings    []SafetySetting   `json:"safetySettings,omitempty"`
}

type Content struct {
	Role  string `json:"role,omitempty"`
	Parts []Part `json:"parts,omitempty"`
}

type Part struct {
	Text             string            `json:"text,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
	InlineData       *Blob             `json:"inlineData,omitempty"`
	FileData         *FileData         `json:"fileData,omitempty"`
	FunctionCall     *FunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *FunctionResponse `json:"functionResponse,omitempty"`
}

type Blob struct {
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

type FileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri,omitempty"`
}

type FunctionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`
}

type FunctionResponse struct {
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
}

type Tool struct {
	FunctionDeclarations []FunctionDeclaration `json:"functionDeclarations,omitempty"`
}

type FunctionDeclaration struct {
	Name                 string          `json:"name,omitempty"`
	Description          string          `json:"description,omitempty"`
	Parameters           json.RawMessage `json:"parameters,omitempty"`
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
}

type ToolConfig struct {
	FunctionCallingConfig *FunctionCallingConfig `json:"functionCallingConfig,omitempty"`
}

type FunctionCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type GenerationConfig struct {
	Temperature     *float64        `json:"temperature,omitempty"`
	MaxOutputTokens int             `json:"maxOutputTokens,omitempty"`
	StopSequences   []string        `json:"stopSequences,omitempty"`
	ThinkingConfig  *ThinkingConfig `json:"thinkingConfig,omitempty"`
}

type ThinkingConfig struct {
	IncludeThoughts bool   `json:"includeThoughts,omitempty"`
	ThinkingBudget  *int   `json:"thinkingBudget,omitempty"`
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`
}

// SafetySetting is defined for round-trip/#204 completeness; the outbound
// client does not emit it (the neutral model carries no safety settings).
type SafetySetting struct {
	Category  string `json:"category,omitempty"`
	Threshold string `json:"threshold,omitempty"`
}

type GenerateContentResponse struct {
	Candidates     []Candidate     `json:"candidates,omitempty"`
	PromptFeedback *PromptFeedback `json:"promptFeedback,omitempty"`
	UsageMetadata  *UsageMetadata  `json:"usageMetadata,omitempty"`
	ModelVersion   string          `json:"modelVersion,omitempty"`
}

type Candidate struct {
	Content      *Content `json:"content,omitempty"`
	FinishReason string   `json:"finishReason,omitempty"`
	Index        int      `json:"index,omitempty"`
}

type PromptFeedback struct {
	BlockReason string `json:"blockReason,omitempty"`
}

type UsageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount,omitempty"`
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
	CandidatesTokenCount    int `json:"candidatesTokenCount,omitempty"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount,omitempty"`
	TotalTokenCount         int `json:"totalTokenCount,omitempty"`
}
```

- [ ] **Step 4: Write the tool-call-id codec**

`gateway/backend/internal/geminiwire/toolcallid.go`:

```go
package geminiwire

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// toolCallIDPrefix marks a tool-call id this package minted. Gemini functionCall
// parts carry no stable id and the neutral model has no slot for a Gemini
// thoughtSignature, so on parse we fold the function name and the part's
// thoughtSignature into the neutral ToolCall.ID. Every dialect echoes
// tool_call_id / tool_use_id / call_id verbatim, so on a later render we decode
// it to restore functionResponse.name and the model turn's thoughtSignature.
const toolCallIDPrefix = "gcw1:"

type toolCallIDPayload struct {
	N string `json:"n,omitempty"`
	S string `json:"s,omitempty"`
}

func encodeToolCallID(name, signature string) string {
	raw, err := json.Marshal(toolCallIDPayload{N: name, S: signature})
	if err != nil { // unreachable for strings, but stay total
		return name
	}
	return toolCallIDPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

func decodeToolCallID(id string) (name, signature string, encoded bool) {
	rest, ok := strings.CutPrefix(id, toolCallIDPrefix)
	if !ok {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return "", "", false
	}
	var p toolCallIDPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", false
	}
	return p.N, p.S, true
}
```

- [ ] **Step 5: Run the codec tests to verify they pass**

Run: `cd gateway/backend && go test ./internal/geminiwire/`
Expected: PASS.

- [ ] **Step 6: Format, vet, commit**

Run: `cd gateway/backend && gofmt -l internal/geminiwire && go vet ./internal/geminiwire/`

```bash
git add gateway/backend/internal/geminiwire/wire.go gateway/backend/internal/geminiwire/toolcallid.go gateway/backend/internal/geminiwire/toolcallid_test.go
git commit -m "feat(geminiwire): wire structs and tool-call-id codec

Introduce the leaf geminiwire package that will hold the Gemini
generateContent wire types and the neutral<->Gemini converters shared by
the outbound native client and the future inbound endpoint.

Add the generateContent request/response wire structs and a reversible
tool-call-id codec that folds the function name and a part's
thoughtSignature into the neutral ToolCall.ID, since the neutral model
has no slot for either. The package imports only stdlib so far; its
architecture-test edge to internal/inference lands with the first
converter in the next task."
```

---

### Task 2: `geminiwire.RenderRequest` — neutral → Gemini

**Files:**
- Create: `gateway/backend/internal/geminiwire/render.go`
- Test: `gateway/backend/internal/geminiwire/render_test.go`
- Modify: `gateway/backend/internal/archtest/arch_test.go` (add the `internal/geminiwire → internal/inference` allowlist entry + a forbidden-edges sub-test — this is the first task in which `geminiwire` imports an internal package, so the archtest edge becomes real here)

**Interfaces:**
- Consumes: Task 1 wire structs, `encodeToolCallID`/`decodeToolCallID`; `internal/inference` (`Request`, `Message`, `RoleSystem`/`RoleDeveloper`/`RoleAssistant`/`RoleTool`, `ContentPart`, `ContentImage`, `Tool`, `ToolCall`, `Message.Text()`).
- Produces (consumed by Task 5): `func RenderRequest(req inference.Request, model string) GenerateContentRequest`.

Rendering is total and tolerant (bad tool args → `{}`, unsupported media → dropped), mirroring `provider.anthropicMessages`; the model string is passed in (it travels in the URL, not the body), and `req.Stream` is ignored (the client encodes stream in the URL action).

- [ ] **Step 1: Write the failing tests**

`gateway/backend/internal/geminiwire/render_test.go`:

```go
package geminiwire

import (
	"encoding/json"
	"testing"

	"op-ai-gateway/internal/inference"
)

func fv(f float64) *float64 { return &f }

func TestRenderSystemAndRoleMerge(t *testing.T) {
	req := inference.Request{Messages: []inference.Message{
		{Role: inference.RoleSystem, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "be terse"}}},
		{Role: inference.RoleDeveloper, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "use JSON"}}},
		{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}},
		{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "there"}}},
	}}
	got := RenderRequest(req, "gemini-2.5-pro")
	if got.SystemInstruction == nil || len(got.SystemInstruction.Parts) != 1 || got.SystemInstruction.Parts[0].Text != "be terse\n\nuse JSON" {
		t.Fatalf("systemInstruction = %+v", got.SystemInstruction)
	}
	if len(got.Contents) != 1 || got.Contents[0].Role != "user" || len(got.Contents[0].Parts) != 2 {
		t.Fatalf("contents = %+v", got.Contents)
	}
}

func TestRenderAssistantToolCallRestoresSignature(t *testing.T) {
	id := encodeToolCallID("get_weather", "sig-xyz")
	req := inference.Request{Messages: []inference.Message{
		{Role: inference.RoleAssistant, ToolCalls: []inference.ToolCall{{ID: id, Name: "get_weather", Arguments: `{"city":"NYC"}`}}},
		{Role: inference.RoleTool, ToolCallID: id, Content: []inference.ContentPart{{Type: inference.ContentText, Text: `{"temp":9}`}}},
	}}
	got := RenderRequest(req, "gemini-3-pro")
	// model turn: functionCall with restored name + signature
	if got.Contents[0].Role != "model" || got.Contents[0].Parts[0].FunctionCall == nil {
		t.Fatalf("model turn = %+v", got.Contents[0])
	}
	fc := got.Contents[0].Parts[0]
	if fc.FunctionCall.Name != "get_weather" || fc.ThoughtSignature != "sig-xyz" {
		t.Fatalf("functionCall part = %+v", fc)
	}
	// tool turn: user role, functionResponse with the name resolved from the id
	if got.Contents[1].Role != "user" || got.Contents[1].Parts[0].FunctionResponse == nil {
		t.Fatalf("tool turn = %+v", got.Contents[1])
	}
	if got.Contents[1].Parts[0].FunctionResponse.Name != "get_weather" {
		t.Fatalf("functionResponse name = %q", got.Contents[1].Parts[0].FunctionResponse.Name)
	}
}

func TestRenderFunctionResponseNameFallback(t *testing.T) {
	// Foreign/truncated history: a tool result whose id we did not mint and
	// with no preceding assistant tool call. Must not panic; name resolves to
	// empty (VERIFY-LIVE) rather than a wrong value, and response wraps text.
	req := inference.Request{Messages: []inference.Message{
		{Role: inference.RoleTool, ToolCallID: "call_foreign", Content: []inference.ContentPart{{Type: inference.ContentText, Text: "plain text result"}}},
	}}
	got := RenderRequest(req, "gemini-2.5-flash")
	fr := got.Contents[0].Parts[0].FunctionResponse
	if fr == nil || fr.Name != "" {
		t.Fatalf("functionResponse = %+v", fr)
	}
	var wrapped map[string]any
	if err := json.Unmarshal(fr.Response, &wrapped); err != nil || wrapped["result"] != "plain text result" {
		t.Fatalf("wrapped response = %s (err %v)", fr.Response, err)
	}
}

func TestRenderToolsAndChoice(t *testing.T) {
	req := inference.Request{
		Tools:      []inference.Tool{{Name: "f", Description: "d", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}},
		ToolChoice: "required",
	}
	got := RenderRequest(req, "gemini-2.5-pro")
	if len(got.Tools) != 1 || len(got.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("tools = %+v", got.Tools)
	}
	// json.Marshal sorts map keys, so "properties" precedes "type".
	if string(got.Tools[0].FunctionDeclarations[0].ParametersJSONSchema) != `{"properties":{},"type":"object"}` {
		t.Fatalf("schema = %s", got.Tools[0].FunctionDeclarations[0].ParametersJSONSchema)
	}
	if got.ToolConfig == nil || got.ToolConfig.FunctionCallingConfig.Mode != "ANY" {
		t.Fatalf("toolConfig = %+v", got.ToolConfig)
	}
}

func TestRenderToolChoiceNamed(t *testing.T) {
	req := inference.Request{
		Tools:      []inference.Tool{{Name: "f", Parameters: map[string]any{"type": "object"}}},
		ToolChoice: map[string]any{"type": "function", "function": map[string]any{"name": "f"}},
	}
	got := RenderRequest(req, "gemini-2.5-pro")
	cfg := got.ToolConfig.FunctionCallingConfig
	if cfg.Mode != "ANY" || len(cfg.AllowedFunctionNames) != 1 || cfg.AllowedFunctionNames[0] != "f" {
		t.Fatalf("functionCallingConfig = %+v", cfg)
	}
}

func TestRenderImageInlineData(t *testing.T) {
	req := inference.Request{Messages: []inference.Message{{
		Role: inference.RoleUser,
		Content: []inference.ContentPart{
			{Type: inference.ContentText, Text: "look"},
			{Type: inference.ContentImage, ImageURL: "data:image/png;base64,QUJD"},
		},
	}}}
	got := RenderRequest(req, "gemini-2.5-pro")
	parts := got.Contents[0].Parts
	if len(parts) != 2 || parts[1].InlineData == nil || parts[1].InlineData.MimeType != "image/png" || parts[1].InlineData.Data != "QUJD" {
		t.Fatalf("parts = %+v", parts)
	}
}

func TestRenderGenerationConfigAndThinking(t *testing.T) {
	req := inference.Request{
		MaxTokens:       256,
		Temperature:     fv(0.7),
		Stop:            []string{"STOP"},
		ReasoningEffort: "high",
	}
	// 2.5 family -> thinkingBudget
	g25 := RenderRequest(req, "gemini-2.5-pro").GenerationConfig
	if g25.MaxOutputTokens != 256 || g25.Temperature == nil || *g25.Temperature != 0.7 || len(g25.StopSequences) != 1 {
		t.Fatalf("generationConfig = %+v", g25)
	}
	if g25.ThinkingConfig == nil || g25.ThinkingConfig.ThinkingBudget == nil || *g25.ThinkingConfig.ThinkingBudget != 24576 {
		t.Fatalf("2.5 thinking = %+v", g25.ThinkingConfig)
	}
	// 3.x family -> thinkingLevel
	g3 := RenderRequest(req, "gemini-3-pro").GenerationConfig
	if g3.ThinkingConfig == nil || g3.ThinkingConfig.ThinkingLevel != "high" || g3.ThinkingConfig.ThinkingBudget != nil {
		t.Fatalf("3.x thinking = %+v", g3.ThinkingConfig)
	}
}

func TestRenderBadToolArgsBecomeEmptyObject(t *testing.T) {
	req := inference.Request{Messages: []inference.Message{
		{Role: inference.RoleAssistant, ToolCalls: []inference.ToolCall{{ID: encodeToolCallID("f", ""), Name: "f", Arguments: "not json"}}},
	}}
	got := RenderRequest(req, "gemini-2.5-pro")
	if string(got.Contents[0].Parts[0].FunctionCall.Args) != "{}" {
		t.Fatalf("args = %s", got.Contents[0].Parts[0].FunctionCall.Args)
	}
}
```

> Verified against the source (authoritative): the module import path is `op-ai-gateway/internal/inference` (module `op-ai-gateway`, rooted at `gateway/backend`). **`inference.Tool.Parameters` is a `map[string]any`, NOT `json.RawMessage`** — so `toolSchema` marshals the map into a `json.RawMessage` for `parametersJsonSchema`. Field reference: `inference.Tool{Name, Description string; Parameters map[string]any}`; `inference.ToolCall{ID, Name, Arguments string}` (Arguments is a JSON string); `inference.Message{Role Role; Content []ContentPart; ToolCallID string; ToolCalls []ToolCall}`; `inference.ContentPart{Type ContentType; Text, ImageURL string}`; `inference.Message.Text()` joins the `ContentText` parts. Roles: `RoleSystem`/`RoleDeveloper`/`RoleUser`/`RoleAssistant`/`RoleTool`. Content types: `ContentText`/`ContentImage`/`ContentToolResult`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd gateway/backend && go test ./internal/geminiwire/ -run TestRender`
Expected: FAIL — `RenderRequest` undefined.

- [ ] **Step 3: Implement `RenderRequest` and its helpers**

`gateway/backend/internal/geminiwire/render.go`:

```go
package geminiwire

import (
	"encoding/json"
	"strings"

	"op-ai-gateway/internal/inference"
)

// RenderRequest converts a neutral request into a Gemini generateContent body.
// The model travels in the URL (not the body) and stream is encoded in the URL
// action, so neither is read here. Rendering is total: malformed tool arguments
// become {} and unsupported media is dropped rather than failing the request.
func RenderRequest(req inference.Request, model string) GenerateContentRequest {
	system, contents := renderMessages(req.Messages)
	out := GenerateContentRequest{Contents: contents}
	if system != "" {
		out.SystemInstruction = &Content{Parts: []Part{{Text: system}}}
	}
	if tools := renderTools(req.Tools); len(tools) > 0 {
		out.Tools = tools
		out.ToolConfig = renderToolConfig(req.ToolChoice)
	}
	out.GenerationConfig = renderGenerationConfig(req, model)
	return out
}

func renderMessages(messages []inference.Message) (system string, contents []Content) {
	var sys []string
	nameByID := toolNameByID(messages)
	for _, msg := range messages {
		switch msg.Role {
		case inference.RoleSystem, inference.RoleDeveloper:
			if t := msg.Text(); t != "" {
				sys = append(sys, t)
			}
		case inference.RoleTool:
			contents = appendTurn(contents, "user", []Part{functionResponsePart(msg, nameByID)})
		case inference.RoleAssistant:
			contents = appendTurn(contents, "model", assistantParts(msg))
		default:
			contents = appendTurn(contents, "user", userParts(msg))
		}
	}
	return strings.Join(sys, "\n\n"), contents
}

// toolNameByID maps a tool-call id to the function name declared by the
// assistant that issued it, so a functionResponse can recover its mandatory
// name even when the id is foreign (not minted by this package).
func toolNameByID(messages []inference.Message) map[string]string {
	m := map[string]string{}
	for _, msg := range messages {
		if msg.Role != inference.RoleAssistant {
			continue
		}
		for _, tc := range msg.ToolCalls {
			if tc.ID != "" && tc.Name != "" {
				m[tc.ID] = tc.Name
			}
		}
	}
	return m
}

func appendTurn(turns []Content, role string, parts []Part) []Content {
	if len(parts) == 0 {
		return turns
	}
	if n := len(turns); n > 0 && turns[n-1].Role == role {
		turns[n-1].Parts = append(turns[n-1].Parts, parts...)
		return turns
	}
	return append(turns, Content{Role: role, Parts: parts})
}

func userParts(msg inference.Message) []Part {
	var parts []Part
	for _, p := range msg.Content {
		switch p.Type {
		case inference.ContentText:
			if p.Text != "" {
				parts = append(parts, Part{Text: p.Text})
			}
		case inference.ContentImage:
			if ip := imagePart(p.ImageURL); ip != nil {
				parts = append(parts, *ip)
			}
		}
	}
	return parts
}

func assistantParts(msg inference.Message) []Part {
	var parts []Part
	for _, p := range msg.Content {
		if p.Type == inference.ContentText && p.Text != "" {
			parts = append(parts, Part{Text: p.Text})
		}
	}
	for _, tc := range msg.ToolCalls {
		name, sig, encoded := decodeToolCallID(tc.ID)
		if !encoded {
			name, sig = tc.Name, ""
		}
		if name == "" {
			name = tc.Name
		}
		part := Part{FunctionCall: &FunctionCall{Name: name, Args: toolArgs(tc.Arguments)}}
		if sig != "" {
			part.ThoughtSignature = sig
		}
		parts = append(parts, part)
	}
	return parts
}

func functionResponsePart(msg inference.Message, nameByID map[string]string) Part {
	name, _, encoded := decodeToolCallID(msg.ToolCallID)
	if !encoded || name == "" {
		name = nameByID[msg.ToolCallID] // "" when foreign/absent; VERIFY-LIVE
	}
	return Part{FunctionResponse: &FunctionResponse{Name: name, Response: wrapToolResult(msg.Text())}}
}

// wrapToolResult makes a Gemini functionResponse.response object. Gemini wants
// a JSON object; a neutral tool result is free text, so a non-object is wrapped
// as {"result": <text>}.
func wrapToolResult(text string) json.RawMessage {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	raw, err := json.Marshal(map[string]string{"result": text})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

func toolArgs(arguments string) json.RawMessage {
	trimmed := strings.TrimSpace(arguments)
	if !strings.HasPrefix(trimmed, "{") || !json.Valid([]byte(trimmed)) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(trimmed)
}

// imagePart maps a neutral image URL to an inlineData (data: URI) or fileData
// (http(s) URL) part. Anything else is dropped (nil), never an error.
func imagePart(url string) *Part {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		meta, data, found := strings.Cut(rest, ",")
		mediaType, params, _ := strings.Cut(meta, ";")
		marker := params
		if i := strings.LastIndex(params, ";"); i >= 0 {
			marker = params[i+1:]
		}
		if !found || !strings.EqualFold(strings.TrimSpace(marker), "base64") || mediaType == "" || data == "" {
			return nil
		}
		return &Part{InlineData: &Blob{MimeType: mediaType, Data: data}}
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		// VERIFY-LIVE: whether generativelanguage accepts an external fileUri;
		// if not, drop (return nil) rather than fail the request.
		return &Part{FileData: &FileData{FileURI: url}}
	}
	return nil
}

func renderTools(tools []inference.Tool) []Tool {
	if len(tools) == 0 {
		return nil
	}
	decls := make([]FunctionDeclaration, 0, len(tools))
	for _, t := range tools {
		decls = append(decls, FunctionDeclaration{
			Name:                 t.Name,
			Description:          t.Description,
			ParametersJSONSchema: toolSchema(t.Parameters),
		})
	}
	return []Tool{{FunctionDeclarations: decls}}
}

// toolSchema marshals a neutral tool's JSON-schema parameters (a map) into a
// parametersJsonSchema value. An empty/absent map becomes a bare object schema.
func toolSchema(params map[string]any) json.RawMessage {
	if len(params) == 0 {
		return json.RawMessage(`{"type":"object"}`)
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return raw
}

func renderToolConfig(choice any) *ToolConfig {
	mode, names := functionCallingMode(choice)
	if mode == "" {
		return nil
	}
	cfg := &FunctionCallingConfig{Mode: mode}
	if len(names) > 0 {
		cfg.AllowedFunctionNames = names
	}
	return &ToolConfig{FunctionCallingConfig: cfg}
}

// functionCallingMode maps the neutral (OpenAI-shaped) tool_choice to Gemini's
// functionCallingConfig. Unrecognized -> ("", nil) meaning omit toolConfig.
func functionCallingMode(choice any) (mode string, names []string) {
	switch v := choice.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "auto":
			return "AUTO", nil
		case "none":
			return "NONE", nil
		case "required", "any":
			return "ANY", nil
		}
	case map[string]any:
		// {type:function, function:{name}} and the Responses-flat {type:function, name}
		if name := namedFunction(v); name != "" {
			return "ANY", []string{name}
		}
	}
	return "", nil
}

func namedFunction(v map[string]any) string {
	if fn, ok := v["function"].(map[string]any); ok {
		if name, ok := fn["name"].(string); ok {
			return name
		}
	}
	if name, ok := v["name"].(string); ok {
		return name
	}
	return ""
}

func renderGenerationConfig(req inference.Request, model string) *GenerationConfig {
	cfg := &GenerationConfig{}
	empty := true
	if req.MaxTokens > 0 {
		cfg.MaxOutputTokens = req.MaxTokens
		empty = false
	}
	if req.Temperature != nil {
		t := *req.Temperature
		cfg.Temperature = &t
		empty = false
	}
	for _, s := range req.Stop {
		if s != "" {
			cfg.StopSequences = append(cfg.StopSequences, s)
			empty = false
		}
	}
	if tc := thinkingConfig(model, req.ReasoningEffort); tc != nil {
		cfg.ThinkingConfig = tc
		empty = false
	}
	if empty {
		return nil
	}
	return cfg
}

// thinkingConfig maps reasoning_effort to a Gemini thinking directive. The 3.x
// family takes a thinkingLevel; the 2.5 family takes a thinkingBudget. The
// family is sniffed from the model name. VERIFY-LIVE: thinkingLevel field name
// and values, and per-model budget ceilings.
func thinkingConfig(model, effort string) *ThinkingConfig {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		return nil
	}
	if strings.Contains(strings.ToLower(model), "gemini-3") {
		level := "low"
		switch effort {
		case "medium":
			level = "medium"
		case "high":
			level = "high"
		}
		return &ThinkingConfig{IncludeThoughts: true, ThinkingLevel: level}
	}
	budget := 1024
	switch effort {
	case "medium":
		budget = 8192
	case "high":
		budget = 24576
	}
	return &ThinkingConfig{IncludeThoughts: true, ThinkingBudget: &budget}
}
```

> `inference.ContentText` / `inference.ContentImage` / `inference.RoleUser` and the `Message.Content` / `Message.ToolCalls` / `Message.ToolCallID` / `Message.Text()` / `Tool.Parameters` / `ToolCall.Arguments` names come from `internal/inference/types.go`; confirm each against the source and the task brief.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd gateway/backend && go test ./internal/geminiwire/`
Expected: PASS.

- [ ] **Step 5: Add the `geminiwire → inference` archtest edge**

In `gateway/backend/internal/archtest/arch_test.go`:

1. Add to the `allowedDeps` map a new entry (keep the map's existing formatting/ordering style):

```go
	"internal/geminiwire": {
		"internal/inference",
	},
```

2. Add a forbidden-edge sub-test inside `TestForbiddenEdges`, mirroring the existing "compat stays contained to inference" sub-test:

```go
	t.Run("geminiwire stays contained to inference", func(t *testing.T) {
		// internal/geminiwire holds the Gemini wire types and the
		// Gemini<->neutral converters shared by the outbound client and the
		// future inbound endpoint. Like internal/compat it must stay
		// contained: among internal packages it may depend only on
		// internal/inference's shared types.
		allowOnlyAmongInternal(t, "internal/geminiwire", []string{"internal/inference"},
			"internal/geminiwire (Gemini translation) must depend on nothing internal except internal/inference")
	})
```

- [ ] **Step 6: Run the arch tests to verify they pass**

Run: `cd gateway/backend && go test ./internal/archtest/`
Expected: PASS (`TestAllowlistFrozen` + `TestForbiddenEdges`). If `TestAllowlistFrozen` fails, the new `allowedDeps` entry is missing, misformatted, or does not match the actual import (confirm `render.go` imports `internal/inference`).

- [ ] **Step 7: Format, vet, commit**

Run: `cd gateway/backend && gofmt -l internal/geminiwire internal/archtest && go vet ./internal/geminiwire/ ./internal/archtest/`

```bash
git add gateway/backend/internal/geminiwire/render.go gateway/backend/internal/geminiwire/render_test.go gateway/backend/internal/archtest/arch_test.go
git commit -m "feat(geminiwire): render neutral request to Gemini generateContent

Translate the neutral inference.Request into a Gemini request body:
system/developer text to systemInstruction, user/assistant to
user/model turns with consecutive-same-role merge, tool results to a
user functionResponse turn, images to inlineData/fileData, tools to
functionDeclarations with a full JSON schema, tool_choice to
functionCallingConfig, and generationConfig incl. a reasoning_effort to
thinking mapping (budget for 2.5, level for 3.x). Restore each model
turn's thoughtSignature and each functionResponse name from the
tool-call id, falling back to a scan of prior assistant tool calls for a
foreign id. Malformed tool arguments render as {}; unsupported media is
dropped. Pin internal/geminiwire to internal/inference in the
architecture test, now that the first converter imports it."
```

---

### Task 3: `geminiwire.ParseResponse` + `ParseStreamChunk` — Gemini → neutral

**Files:**
- Create: `gateway/backend/internal/geminiwire/parse.go`
- Test: `gateway/backend/internal/geminiwire/parse_test.go`

**Interfaces:**
- Consumes: Task 1 wire structs + `encodeToolCallID`; `internal/inference` (`ToolCall`, `Usage`).
- Produces (consumed by Task 5):
  - `type Result struct { Text string; Reasoning string; ToolCalls []inference.ToolCall; Usage inference.Usage; FinishReason string }`
  - `func ParseResponse(raw []byte) (Result, error)` — error only on JSON decode failure.
  - `func ParseStreamChunk(raw []byte) (res Result, done bool, err error)` — `done` is true when the chunk carries a `finishReason`; `res.Text`/`res.Reasoning` are that chunk's deltas; `res.ToolCalls` are whole function calls in that chunk; `res.Usage` is that chunk's `usageMetadata` (zero value when absent); error only on JSON decode failure.

- [ ] **Step 1: Write the failing tests**

`gateway/backend/internal/geminiwire/parse_test.go`:

```go
package geminiwire

import "testing"

func TestParseResponseTextAndThought(t *testing.T) {
	raw := []byte(`{"candidates":[{"content":{"role":"model","parts":[
		{"text":"thinking...","thought":true},
		{"text":"the answer"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":4,"candidatesTokenCount":5,"thoughtsTokenCount":3,"totalTokenCount":18}}`)
	r, err := ParseResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if r.Text != "the answer" || r.Reasoning != "thinking..." {
		t.Fatalf("text/reasoning = %q / %q", r.Text, r.Reasoning)
	}
	if r.FinishReason != "stop" {
		t.Fatalf("finish = %q", r.FinishReason)
	}
	// InputTokens = prompt (incl. cache); Output = candidates + thoughts; Total kept.
	if r.Usage.InputTokens != 10 || r.Usage.CachedTokens != 4 || r.Usage.OutputTokens != 8 || r.Usage.TotalTokens != 18 {
		t.Fatalf("usage = %+v", r.Usage)
	}
}

func TestParseResponseFunctionCall(t *testing.T) {
	raw := []byte(`{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"get_weather","args":{"city":"NYC"}},"thoughtSignature":"sig1"}]},
		"finishReason":"STOP"}]}`)
	r, err := ParseResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "get_weather" || r.ToolCalls[0].Arguments != `{"city":"NYC"}` {
		t.Fatalf("toolcalls = %+v", r.ToolCalls)
	}
	// finishReason becomes tool_calls when a functionCall is present
	if r.FinishReason != "tool_calls" {
		t.Fatalf("finish = %q", r.FinishReason)
	}
	// id round-trips the name + signature
	name, sig, ok := decodeToolCallID(r.ToolCalls[0].ID)
	if !ok || name != "get_weather" || sig != "sig1" {
		t.Fatalf("id decode = %q/%q/%v", name, sig, ok)
	}
}

func TestParseResponseFinishReasonTable(t *testing.T) {
	cases := map[string]string{
		"STOP": "stop", "MAX_TOKENS": "length", "SAFETY": "content_filter",
		"RECITATION": "content_filter", "PROHIBITED_CONTENT": "content_filter",
		"SPII": "content_filter", "BLOCKLIST": "content_filter",
		"MALFORMED_FUNCTION_CALL": "stop", "OTHER": "stop", "": "",
	}
	for in, want := range cases {
		if got := mapFinishReason(in, false); got != want {
			t.Fatalf("mapFinishReason(%q,false) = %q, want %q", in, got, want)
		}
	}
}

func TestParseResponsePromptBlockedNoCandidates(t *testing.T) {
	raw := []byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`)
	r, err := ParseResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Text != "" || r.FinishReason != "content_filter" {
		t.Fatalf("blocked result = %+v", r)
	}
}

func TestParseResponseDecodeError(t *testing.T) {
	if _, err := ParseResponse([]byte("not json")); err == nil {
		t.Fatal("want decode error")
	}
}

func TestParseStreamChunkDeltasAndDone(t *testing.T) {
	r1, done1, err := ParseStreamChunk([]byte(`{"candidates":[{"content":{"parts":[{"text":"Hel"}]}}]}`))
	if err != nil || done1 || r1.Text != "Hel" {
		t.Fatalf("chunk1 = %+v done=%v err=%v", r1, done1, err)
	}
	r2, done2, err := ParseStreamChunk([]byte(`{"candidates":[{"content":{"parts":[{"text":"lo"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`))
	if err != nil || !done2 || r2.Text != "lo" || r2.FinishReason != "stop" {
		t.Fatalf("chunk2 = %+v done=%v err=%v", r2, done2, err)
	}
	if r2.Usage.InputTokens != 3 || r2.Usage.OutputTokens != 2 || r2.Usage.TotalTokens != 5 {
		t.Fatalf("chunk2 usage = %+v", r2.Usage)
	}
}

func TestParseStreamChunkFunctionCall(t *testing.T) {
	r, done, err := ParseStreamChunk([]byte(`{"candidates":[{"content":{"parts":[
		{"functionCall":{"name":"f","args":{}}}]},"finishReason":"STOP"}]}`))
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "f" || r.ToolCalls[0].Arguments != "{}" {
		t.Fatalf("toolcalls = %+v", r.ToolCalls)
	}
	if r.FinishReason != "tool_calls" {
		t.Fatalf("finish = %q", r.FinishReason)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd gateway/backend && go test ./internal/geminiwire/ -run TestParse`
Expected: FAIL — `ParseResponse`/`ParseStreamChunk`/`Result`/`mapFinishReason` undefined.

- [ ] **Step 3: Implement the parsers**

`gateway/backend/internal/geminiwire/parse.go`:

```go
package geminiwire

import (
	"bytes"
	"encoding/json"
	"strings"

	"op-ai-gateway/internal/inference"
)

// Result is the neutral outcome of a Gemini response or stream chunk. It is
// package-local because geminiwire must not import internal/provider; the
// client copies it into provider.Response.
type Result struct {
	Text         string
	Reasoning    string
	ToolCalls    []inference.ToolCall
	Usage        inference.Usage
	FinishReason string
}

// ParseResponse decodes a buffered generateContent response. It errors only on
// malformed JSON; a prompt-blocked reply (no candidates, promptFeedback set)
// is a valid, billed result with empty text and a content_filter finish.
func ParseResponse(raw []byte) (Result, error) {
	var resp GenerateContentResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Result{}, err
	}
	var res Result
	hasToolCall := false
	finish := ""
	if len(resp.Candidates) > 0 {
		cand := resp.Candidates[0]
		finish = cand.FinishReason
		if cand.Content != nil {
			text, reasoning, calls := partsToNeutral(cand.Content.Parts)
			res.Text, res.Reasoning, res.ToolCalls = text, reasoning, calls
			hasToolCall = len(calls) > 0
		}
	} else if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
		finish = "SAFETY" // map to content_filter below
	}
	res.FinishReason = mapFinishReason(finish, hasToolCall)
	if resp.UsageMetadata != nil {
		res.Usage = canonicalUsage(resp.UsageMetadata)
	}
	return res, nil
}

// ParseStreamChunk decodes one SSE data payload. res carries that chunk's text
// and reasoning deltas, any whole function calls in it, and its (cumulative)
// usageMetadata; done is true when the chunk reports a finishReason.
func ParseStreamChunk(raw []byte) (res Result, done bool, err error) {
	var resp GenerateContentResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Result{}, false, err
	}
	hasToolCall := false
	finish := ""
	if len(resp.Candidates) > 0 {
		cand := resp.Candidates[0]
		finish = cand.FinishReason
		if cand.Content != nil {
			res.Text, res.Reasoning, res.ToolCalls = partsToNeutral(cand.Content.Parts)
			hasToolCall = len(res.ToolCalls) > 0
		}
	} else if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
		finish = "SAFETY"
	}
	if finish != "" {
		res.FinishReason = mapFinishReason(finish, hasToolCall)
		done = true
	} else if hasToolCall {
		// tool calls can arrive on a chunk that also carries the finishReason;
		// when they do not, the finishReason chunk still reflects tool_calls.
		res.FinishReason = ""
	}
	if resp.UsageMetadata != nil {
		res.Usage = canonicalUsage(resp.UsageMetadata)
	}
	return res, done, nil
}

func partsToNeutral(parts []Part) (text, reasoning string, calls []inference.ToolCall) {
	var tb, rb strings.Builder
	for _, p := range parts {
		switch {
		case p.FunctionCall != nil:
			calls = append(calls, inference.ToolCall{
				ID:        encodeToolCallID(p.FunctionCall.Name, p.ThoughtSignature),
				Name:      p.FunctionCall.Name,
				Arguments: compactArgs(p.FunctionCall.Args),
			})
		case p.Thought:
			rb.WriteString(p.Text)
		case p.Text != "":
			tb.WriteString(p.Text)
		}
	}
	return tb.String(), rb.String(), calls
}

func compactArgs(args json.RawMessage) string {
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		return "{}"
	}
	return buf.String()
}

// mapFinishReason folds a Gemini finishReason to the OpenAI vocabulary the
// neutral model uses. A present functionCall makes a plain stop a tool_calls.
func mapFinishReason(reason string, hasToolCall bool) string {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "":
		return ""
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "LANGUAGE",
		"IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION", "IMAGE_OTHER":
		return "content_filter"
	default: // STOP, MALFORMED_FUNCTION_CALL, MISSING_THOUGHT_SIGNATURE, OTHER, newer
		if hasToolCall {
			return "tool_calls"
		}
		return "stop"
	}
}

// canonicalUsage maps Gemini usageMetadata to neutral Usage in the OpenAI
// convention: InputTokens already includes cached; OutputTokens includes
// reasoning (thoughts); input+output stays consistent with the reported total.
func canonicalUsage(u *UsageMetadata) inference.Usage {
	return inference.Usage{
		InputTokens:  u.PromptTokenCount,
		CachedTokens: u.CachedContentTokenCount,
		OutputTokens: u.CandidatesTokenCount + u.ThoughtsTokenCount,
		TotalTokens:  u.TotalTokenCount,
	}
}
```

> Confirm the `inference.Usage` field names (`InputTokens`, `OutputTokens`, `TotalTokens`, `CachedTokens`) against `internal/inference/types.go`; the digest lists them but the brief is authoritative.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd gateway/backend && go test ./internal/geminiwire/`
Expected: PASS (all geminiwire tests, incl. Tasks 1–2).

- [ ] **Step 5: Format, vet, commit**

Run: `cd gateway/backend && gofmt -l internal/geminiwire && go vet ./internal/geminiwire/`

```bash
git add gateway/backend/internal/geminiwire/parse.go gateway/backend/internal/geminiwire/parse_test.go
git commit -m "feat(geminiwire): parse Gemini response and stream chunks to neutral

Decode a buffered generateContent response and individual
streamGenerateContent SSE chunks into a neutral Result: split text,
thought (reasoning) and functionCall parts, fold the thoughtSignature
into the tool-call id, map finishReason to the OpenAI vocabulary
(tool_calls when a functionCall is present, content_filter for the
safety family), and canonicalize usageMetadata (prompt incl. cache as
input, candidates+thoughts as output). A prompt-blocked reply with no
candidates is a valid content_filter result, not an error; only
malformed JSON errors."
```

---

### Task 4: Routing — provider kind, native vendor id, `Dialect` preset, predicate split, resolver branch

**Files:**
- Modify: `gateway/backend/internal/routing/store.go` (add `ProviderVendorGemini`, `VendorGoogleNative` consts)
- Modify: `gateway/backend/internal/routing/vendor_presets.go` (add `Dialect` field, `VendorGoogleNative` preset, split predicates)
- Modify: `gateway/backend/internal/routing/resolver.go` (native branch in `vendorAccountTarget`; fail-closed in `vendorAccountModelTarget` → `IsPresetVendor`)
- Test: `gateway/backend/internal/routing/resolver_vendor_account_test.go` (add cases), `gateway/backend/internal/routing/vendor_presets_test.go` (predicate cases — create if absent)

**Interfaces:**
- Produces (consumed by Tasks 5, 6, 7):
  - `routing.ProviderVendorGemini = "vendor_gemini"`, `routing.VendorGoogleNative = "google_native"`
  - `VendorPreset.Dialect string`
  - `func IsPresetVendor(v string) bool`, `func IsOpenAICompatibleVendor(v string) bool` (now `IsPresetVendor && Dialect==""`), `func IsNativeGeminiVendor(v string) bool`
  - A `google_native` account resolves to `Target{Provider: ProviderVendorGemini, Endpoint: acc.BaseURL, APITokenHeader: "x-goog-api-key", APIFlavors: [openai, anthropic]}` with both endpoint modes zero (translate).
- Consumes: nothing new.

- [ ] **Step 1: Write the failing predicate + resolver tests**

Add to `gateway/backend/internal/routing/vendor_presets_test.go` (create the file in `package routing` if it does not exist):

```go
func TestVendorPredicateSplit(t *testing.T) {
	if !IsPresetVendor(VendorGoogle) || !IsPresetVendor(VendorGoogleNative) {
		t.Fatal("both google ids must be preset vendors")
	}
	if !IsOpenAICompatibleVendor(VendorGoogle) {
		t.Fatal("shim google must be OpenAI-compatible")
	}
	if IsOpenAICompatibleVendor(VendorGoogleNative) {
		t.Fatal("native google must NOT be OpenAI-compatible")
	}
	if !IsNativeGeminiVendor(VendorGoogleNative) {
		t.Fatal("native google must be a native Gemini vendor")
	}
	if IsNativeGeminiVendor(VendorGoogle) || IsNativeGeminiVendor(VendorOpenAI) {
		t.Fatal("only google_native is native Gemini")
	}
}

func TestVendorGoogleNativePreset(t *testing.T) {
	p, ok := VendorPresetFor(VendorGoogleNative)
	if !ok {
		t.Fatal("missing google_native preset")
	}
	if p.Dialect != "gemini" || p.DefaultBaseURL != "https://generativelanguage.googleapis.com" || p.StripModelsPrefix != "models/" {
		t.Fatalf("preset = %+v", p)
	}
}
```

Add to `gateway/backend/internal/routing/resolver_vendor_account_test.go` (mirror the existing vendor-account target cases; confirm the helper/constructor names used by the sibling tests):

```go
func TestVendorAccountTargetNativeGemini(t *testing.T) {
	acc := VendorAccount{Vendor: VendorGoogleNative, AuthType: "api_key", BaseURL: "https://generativelanguage.googleapis.com"}
	tgt := vendorAccountTarget(acc, VendorAccountModel{}, "gemini-2.5-pro", "openai", "openai_chat_completions")
	if tgt.Provider != ProviderVendorGemini {
		t.Fatalf("provider = %q", tgt.Provider)
	}
	if tgt.Endpoint != "https://generativelanguage.googleapis.com" {
		t.Fatalf("endpoint = %q", tgt.Endpoint)
	}
	if tgt.APITokenHeader != "x-goog-api-key" {
		t.Fatalf("token header = %q", tgt.APITokenHeader)
	}
	if tgt.MessagesMode != "" || tgt.ResponsesMode != "" {
		t.Fatalf("expected translate modes, got messages=%q responses=%q", tgt.MessagesMode, tgt.ResponsesMode)
	}
}

func TestVendorAccountModelTargetNativeFailClosed(t *testing.T) {
	acc := VendorAccount{Vendor: VendorGoogleNative, AuthType: "api_key", BaseURL: ""}
	// vendorAccountModelTarget(acc, m, req, apiFlavor) returns (Target, ok bool);
	// ok==false means "fail closed, fall through" (not an error return).
	if _, ok := vendorAccountModelTarget(acc, VendorAccountModel{}, inference.Request{Model: "gemini-2.5-pro"}, "openai"); ok {
		t.Fatal("empty BaseURL for a preset vendor must fail closed")
	}
}
```

> Verified signatures (authoritative) in `resolver.go`: `func vendorAccountTarget(acc VendorAccount, m VendorAccountModel, model, apiFlavor, fineFlavor string) Target` (returns a `Target`); `func vendorAccountModelTarget(acc VendorAccount, m VendorAccountModel, req inference.Request, apiFlavor string) (Target, bool)` (the `bool` is `ok`; `false` = fail-closed/fall-through, NOT an error). The current fail-closed guard is `if IsOpenAICompatibleVendor(acc.Vendor) && strings.TrimSpace(acc.BaseURL) == "" { return Target{}, false }` — change its predicate to `IsPresetVendor`. The native branch goes in `vendorAccountTarget` between the `acc.Vendor == VendorAnthropic` branch and the `else if IsOpenAICompatibleVendor(acc.Vendor)` branch. `Target` fields the test asserts: `Provider`, `Endpoint`, `APITokenHeader`, `MessagesMode`, `ResponsesMode` (both the latter stay zero/`""` for a translate target), `APIFlavors` (`[]string{APIFlavorOpenAI, APIFlavorAnthropic}`). `VendorPreset` already has `DefaultBaseURL, PathPrefix, ValidateVia, ValidatePath, StripModelsPrefix, UsageVia, UsagePath` — add `Dialect string`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd gateway/backend && go test ./internal/routing/ -run 'TestVendor|TestVendorAccount'`
Expected: FAIL — consts/predicates/preset not yet present.

- [ ] **Step 3: Add the provider kind and vendor id consts**

In `gateway/backend/internal/routing/store.go`, add `ProviderVendorGemini = "vendor_gemini"` to the provider-kind const block (after `ProviderVendorOpenAISubscription`) with a short comment, and `VendorGoogleNative = "google_native"` to the vendor-id const block (near `VendorGoogle`). Do NOT add `ProviderVendorGemini` to `IsOpenAIVendorProvider`.

```go
	// ProviderVendorGemini selects the native Google Gemini dialect client,
	// which speaks generateContent directly (translate) rather than the
	// OpenAI-compatibility shim used by VendorGoogle.
	ProviderVendorGemini = "vendor_gemini"
```

```go
	// VendorGoogleNative is a Google account served by the native Gemini
	// dialect. VendorGoogle ("google") remains the OpenAI-shim account.
	VendorGoogleNative = "google_native"
```

- [ ] **Step 4: Add the `Dialect` field, the preset, and split the predicates**

In `gateway/backend/internal/routing/vendor_presets.go`:

1. Add `Dialect string` to the `VendorPreset` struct with a doc comment (`"" = OpenAI wire, "gemini" = native generateContent`).

2. Add the `VendorGoogleNative` entry to the `vendorPresets` map:

```go
	VendorGoogleNative: {
		DefaultBaseURL:    "https://generativelanguage.googleapis.com",
		PathPrefix:        "/v1beta", // portal discovery/validation only
		ValidateVia:       "models",
		StripModelsPrefix: "models/",
		Dialect:           "gemini",
	},
```

3. Replace the single `IsOpenAICompatibleVendor` with the split (keep the exact current body as `IsPresetVendor`):

```go
// IsPresetVendor reports whether v is a preset (configurable-root, api-key-only)
// vendor account, regardless of wire dialect.
func IsPresetVendor(v string) bool {
	_, ok := vendorPresets[v]
	return ok
}

// IsOpenAICompatibleVendor reports whether v is served by the OpenAI-compatible
// client (a preset vendor whose wire dialect is the OpenAI one).
func IsOpenAICompatibleVendor(v string) bool {
	p, ok := vendorPresets[v]
	return ok && p.Dialect == ""
}

// IsNativeGeminiVendor reports whether v is served by the native Gemini client.
func IsNativeGeminiVendor(v string) bool {
	p, ok := vendorPresets[v]
	return ok && p.Dialect == "gemini"
}
```

- [ ] **Step 5: Add the resolver native branch and fix the fail-closed predicate**

In `gateway/backend/internal/routing/resolver.go`, `vendorAccountTarget`: add the native branch **before** the generic `else if IsOpenAICompatibleVendor(...)` branch:

```go
	} else if IsNativeGeminiVendor(acc.Vendor) {
		// Native Google Gemini: the generateContent client against the
		// account's own root URL, authenticating with x-goog-api-key (not
		// the Bearer default). Translate-only (both endpoint modes stay zero).
		provider = ProviderVendorGemini
		endpoint = acc.BaseURL
		tokenHeader = "x-goog-api-key"
		// pathPrefix stays "" — the client owns /v1beta/models/...:generateContent
	} else if IsOpenAICompatibleVendor(acc.Vendor) {
```

In `vendorAccountModelTarget`, change the empty-BaseURL fail-closed guard from `IsOpenAICompatibleVendor(acc.Vendor)` to `IsPresetVendor(acc.Vendor)` (so native accounts with an empty root also fail closed).

- [ ] **Step 6: Run the routing tests**

Run: `cd gateway/backend && go test ./internal/routing/`
Expected: PASS (new cases + all existing resolver/preset tests — the shim `google` path must stay green).

- [ ] **Step 7: Format, vet, commit**

Run: `cd gateway/backend && gofmt -l internal/routing && go vet ./internal/routing/`

```bash
git add gateway/backend/internal/routing/store.go gateway/backend/internal/routing/vendor_presets.go gateway/backend/internal/routing/resolver.go gateway/backend/internal/routing/vendor_presets_test.go gateway/backend/internal/routing/resolver_vendor_account_test.go
git commit -m "feat(routing): native Gemini provider kind, vendor id, and resolver branch

Add the vendor_gemini provider kind and the google_native vendor id, and
a Dialect field on VendorPreset so the registry distinguishes the native
generateContent wire from the OpenAI shim. Split the registry predicate
into IsPresetVendor (any preset account), IsOpenAICompatibleVendor
(preset + OpenAI dialect) and IsNativeGeminiVendor, and resolve a
google_native account to a vendor_gemini target with an x-goog-api-key
auth header against the account root, checked before the generic
OpenAI-compatible branch. The empty-base-URL fail-closed guard now
covers every preset vendor. The shim google account is unchanged."
```

---

### Task 5: `provider.GeminiClient` + registration

**Files:**
- Create: `gateway/backend/internal/provider/gemini.go`
- Test: `gateway/backend/internal/provider/gemini_test.go`
- Modify: `gateway/backend/cmd/gateway/main.go` (`providerClients` map entry)
- Modify: `gateway/backend/cmd/gateway/main_test.go` (`TestProviderClientsWireVendorProviderKinds` row)
- Modify: `gateway/backend/internal/archtest/arch_test.go` (add `internal/geminiwire` to `allowedDeps["internal/provider"]` — `gemini.go` is the first provider file to import it)

**Interfaces:**
- Consumes: `geminiwire.RenderRequest` / `ParseResponse` / `ParseStreamChunk` / `Result` (Tasks 2–3); `routing.ProviderVendorGemini` (Task 4); existing provider plumbing — `endpointURL`, `providerModel`, `unavailableStatus`, `ErrUnavailable`/`ErrTimeout`/`ErrInvalidResponse`, `contentTypeHeader`/`jsonContentType`, `applyUpstreamAuth`, `CaptureSinkFrom`, `StreamActivityFrom`, `streamLineData`, `doNativeProxy`, `withoutRedirects`, `readUpstreamErrorSnippet`, `Response`, `StreamEmit`.
- Produces: `func NewGeminiClient(httpClient *http.Client) *GeminiClient` implementing `Client`, `StreamingClient`, `NativeProxyClient`.

- [ ] **Step 1: Write the failing client tests**

`gateway/backend/internal/provider/gemini_test.go` (mirror `anthropic_messages_test.go` httptest conventions — a capture struct, a target helper, `collectStream`):

```go
package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
)

func TestGeminiClientInterfaces(t *testing.T) {
	var _ Client = (*GeminiClient)(nil)
	var _ StreamingClient = (*GeminiClient)(nil)
	var _ NativeProxyClient = (*GeminiClient)(nil)
}

func TestGeminiCompletePathAuthAndParse(t *testing.T) {
	var gotPath, gotKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],
			"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`))
	}))
	defer srv.Close()

	ctx := WithUpstreamAuthHeaders(context.Background(), "x-goog-api-key", "secret-key", nil)
	c := NewGeminiClient(srv.Client())
	tgt := routing.Target{Endpoint: srv.URL, ProviderModel: "gemini-2.5-pro", Timeout: 5 * time.Second}
	resp, err := c.Complete(ctx, tgt, inference.Request{Messages: []inference.Message{textMessage("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotKey != "secret-key" || gotAuth != "" {
		t.Fatalf("auth: x-goog-api-key=%q authorization=%q", gotKey, gotAuth)
	}
	if resp.Text != "hi" || resp.FinishReason != "stop" || resp.Usage.TotalTokens != 3 {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestGeminiCompleteStripsModelsPrefixAndEscapes(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"STOP"}]}`))
	}))
	defer srv.Close()
	c := NewGeminiClient(srv.Client())
	tgt := routing.Target{Endpoint: srv.URL, ProviderModel: "models/gemini-2.5-pro"}
	if _, err := c.Complete(context.Background(), tgt, inference.Request{Messages: []inference.Message{textMessage("x")}}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestGeminiComplete400SurfacesSnippet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Invalid thinkingConfig","status":"INVALID_ARGUMENT"}}`))
	}))
	defer srv.Close()
	c := NewGeminiClient(srv.Client())
	tgt := routing.Target{Endpoint: srv.URL, ProviderModel: "gemini-2.5-pro"}
	_, err := c.Complete(context.Background(), tgt, inference.Request{Messages: []inference.Message{textMessage("x")}})
	if err == nil || !strings.Contains(err.Error(), "Invalid thinkingConfig") {
		t.Fatalf("err = %v, want snippet", err)
	}
}

func TestGeminiCompleteStreamSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, ":streamGenerateContent") || r.URL.Query().Get("alt") != "sse" {
			t.Errorf("stream path/query = %q ? %q", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hel\"}]}}]}\n\n"))
		fl.Flush()
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"lo\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":2,\"totalTokenCount\":4}}\n\n"))
		fl.Flush()
	}))
	defer srv.Close()
	c := NewGeminiClient(srv.Client())
	tgt := routing.Target{Endpoint: srv.URL, ProviderModel: "gemini-2.5-pro"}
	text, events := collectGeminiStream(t, c, tgt, inference.Request{Messages: []inference.Message{textMessage("hi")}})
	if text != "Hello" {
		t.Fatalf("text = %q", text)
	}
	last := events[len(events)-1]
	if last.Type != inference.StreamEventCompleted || last.FinishReason != "stop" || last.Usage == nil || last.Usage.TotalTokens != 4 {
		t.Fatalf("completed = %+v", last)
	}
}

func TestGeminiCompleteStreamEndsEarly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}\n\n"))
		fl.Flush()
		// no finishReason chunk, connection just closes
	}))
	defer srv.Close()
	c := NewGeminiClient(srv.Client())
	tgt := routing.Target{Endpoint: srv.URL, ProviderModel: "gemini-2.5-pro"}
	err := c.CompleteStream(context.Background(), tgt, inference.Request{Messages: []inference.Message{textMessage("hi")}}, func(inference.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("want ErrUnavailable on a stream that ends without finishReason")
	}
}

func TestGeminiProxyNativeRelaysVerbatim(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("relayed"))
	}))
	defer srv.Close()
	c := NewGeminiClient(srv.Client())
	tgt := routing.Target{Endpoint: srv.URL}
	raw := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	pr, err := c.ProxyNative(context.Background(), tgt, "/v1beta/models/gemini-2.5-pro:generateContent", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Body.Close()
	if pr.StatusCode != http.StatusTeapot || gotBody != string(raw) {
		t.Fatalf("status=%d body=%q", pr.StatusCode, gotBody)
	}
}

func TestGeminiTranslateDoesNotFollowRedirect(t *testing.T) {
	var hitCount int
	var gotKeyAtTarget string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitCount++
		if r.URL.Path == "/redirected" {
			gotKeyAtTarget = r.Header.Get("x-goog-api-key")
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"leaked"}]},"finishReason":"STOP"}]}`))
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	ctx := WithUpstreamAuthHeaders(context.Background(), "x-goog-api-key", "secret", nil)
	c := NewGeminiClient(srv.Client())
	tgt := routing.Target{Endpoint: srv.URL, ProviderModel: "gemini-2.5-pro"}
	_, err := c.Complete(ctx, tgt, inference.Request{Messages: []inference.Message{textMessage("hi")}})
	if err == nil {
		t.Fatal("want a non-2xx (the 307 relayed as the answer), not a followed redirect")
	}
	if gotKeyAtTarget != "" {
		t.Fatalf("x-goog-api-key followed the redirect: %q", gotKeyAtTarget)
	}
}
```

Add a `collectGeminiStream` helper in the test file (mirror `collectStream` in `anthropic_messages_test.go`) that runs `CompleteStream`, concatenates `StreamEventTextDelta` `Text`, and returns the concatenated text plus the full event slice. Add the missing imports (`io`). Reuse the existing `textMessage` helper if the package test files already define one; otherwise add a local one.

> Confirm `WithUpstreamAuthHeaders`, `textMessage`, `inference.StreamEventCompleted`/`StreamEventTextDelta`, and `StreamEvent.Usage` being a `*inference.Usage` against the existing provider tests and `internal/inference/types.go`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd gateway/backend && go test ./internal/provider/ -run TestGemini`
Expected: FAIL — `GeminiClient`/`NewGeminiClient` undefined.

- [ ] **Step 3: Implement `GeminiClient`**

`gateway/backend/internal/provider/gemini.go`:

```go
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"op-ai-gateway/internal/geminiwire"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
)

const (
	geminiAPIVersion       = "/v1beta"
	geminiErrorBodyLimit   = 4 << 10
	geminiStreamLineMaxLen = 1 << 20
)

// GeminiClient speaks Google's native generateContent wire. It translates the
// neutral request/response via internal/geminiwire and owns HTTP, auth
// (x-goog-api-key), redirect policy, timeout, capture and SSE scanning. Like
// AnthropicClient it deliberately implements no ModelLister/Prober (vendor
// model discovery lives in the portal).
type GeminiClient struct {
	http       *http.Client
	noRedirect *http.Client
}

func NewGeminiClient(httpClient *http.Client) *GeminiClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &GeminiClient{http: httpClient, noRedirect: withoutRedirects(httpClient)}
}

var (
	_ Client            = (*GeminiClient)(nil)
	_ StreamingClient   = (*GeminiClient)(nil)
	_ NativeProxyClient = (*GeminiClient)(nil)
)

func geminiModelPath(model, action string) string {
	model = strings.TrimPrefix(model, "models/")
	return geminiAPIVersion + "/models/" + url.PathEscape(model) + ":" + action
}

func (c *GeminiClient) Complete(ctx context.Context, target routing.Target, req inference.Request) (Response, error) {
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	body, err := c.renderBody(target, req)
	if err != nil {
		return Response{}, err
	}
	path := geminiModelPath(providerModel(target, req), "generateContent")
	httpResp, err := c.post(ctx, target, path, body)
	if err != nil {
		return Response{}, err
	}
	defer httpResp.Body.Close()
	respBytes, err := io.ReadAll(httpResp.Body)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Response{}, ErrTimeout
		}
		return Response{}, fmt.Errorf("%w: read response: %v", ErrInvalidResponse, err)
	}
	CaptureSinkFrom(ctx).WriteResponse(respBytes)
	res, err := geminiwire.ParseResponse(respBytes)
	if err != nil {
		return Response{}, fmt.Errorf("%w: decode response: %v", ErrInvalidResponse, err)
	}
	if res.Text == "" && res.Reasoning == "" && len(res.ToolCalls) == 0 && res.FinishReason == "" {
		return Response{}, fmt.Errorf("%w: missing response content", ErrInvalidResponse)
	}
	return geminiResponse(res), nil
}

func (c *GeminiClient) CompleteStream(ctx context.Context, target routing.Target, req inference.Request, emit StreamEmit) error {
	body, err := c.renderBody(target, req)
	if err != nil {
		return err
	}
	path := geminiModelPath(providerModel(target, req), "streamGenerateContent") + "?alt=sse"
	httpResp, err := c.post(ctx, target, path, body)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()

	var reader io.Reader = httpResp.Body
	if rw := CaptureSinkFrom(ctx).ResponseWriter(); rw != nil {
		reader = io.TeeReader(httpResp.Body, rw)
	}

	activity := StreamActivityFrom(ctx)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), geminiStreamLineMaxLen)

	var (
		toolCalls []inference.ToolCall
		usage     inference.Usage
		sawUsage  bool
		finish    string
		done      bool
	)
	for scanner.Scan() {
		data, ok := streamLineData(strings.TrimSpace(scanner.Text()), activity)
		if !ok {
			continue
		}
		if streamIsError(data) {
			return fmt.Errorf("%w: upstream stream error: %s", ErrUnavailable, string(data))
		}
		res, chunkDone, err := geminiwire.ParseStreamChunk(data)
		if err != nil {
			continue // tolerate a non-JSON frame
		}
		if res.Text != "" {
			if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: res.Text}); err != nil {
				return err
			}
		}
		if res.Reasoning != "" {
			if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Reasoning: res.Reasoning}); err != nil {
				return err
			}
		}
		toolCalls = append(toolCalls, res.ToolCalls...)
		if res.Usage != (inference.Usage{}) {
			usage, sawUsage = res.Usage, true
		}
		if chunkDone {
			finish, done = res.FinishReason, true
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ErrTimeout
		}
		return fmt.Errorf("%w: read stream: %v", ErrUnavailable, err)
	}
	if !done {
		return fmt.Errorf("%w: stream ended before a finish reason", ErrUnavailable)
	}
	for _, tc := range toolCalls {
		if err := emit(inference.StreamEvent{Type: inference.StreamEventToolCall, ToolCall: &tc}); err != nil {
			return err
		}
	}
	var usagePtr *inference.Usage
	if sawUsage {
		u := usage
		usagePtr = &u
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: usagePtr, FinishReason: finish})
}

func (c *GeminiClient) ProxyNative(ctx context.Context, target routing.Target, path string, body []byte) (*ProxyResponse, error) {
	return doNativeProxy(ctx, c.noRedirect, target, path, body)
}

func (c *GeminiClient) renderBody(target routing.Target, req inference.Request) ([]byte, error) {
	model := strings.TrimPrefix(providerModel(target, req), "models/")
	body, err := json.Marshal(geminiwire.RenderRequest(req, model))
	if err != nil {
		return nil, fmt.Errorf("%w: encode request", ErrInvalidResponse)
	}
	return body, nil
}

func (c *GeminiClient) post(ctx context.Context, target routing.Target, path string, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL(target.Endpoint, path), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	httpReq.Header.Set(contentTypeHeader, jsonContentType)
	applyUpstreamAuth(ctx, httpReq)
	sink := CaptureSinkFrom(ctx)
	sink.RecordRequest(httpReq.Header, body)
	httpResp, err := c.noRedirect.Do(httpReq)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrTimeout
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		snippet := readUpstreamErrorSnippet(httpResp.Body, geminiErrorBodyLimit)
		httpResp.Body.Close()
		sink.WriteResponse([]byte(snippet))
		return nil, upstreamStatusError(httpResp.StatusCode, snippet)
	}
	sink.RecordResponseHeaders(httpResp.Header)
	return httpResp, nil
}

func geminiResponse(r geminiwire.Result) Response {
	return Response{
		Text:         r.Text,
		Reasoning:    r.Reasoning,
		ToolCalls:    r.ToolCalls,
		Usage:        r.Usage,
		FinishReason: r.FinishReason,
	}
}

func streamIsError(data []byte) bool {
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	return len(probe.Error) > 0
}
```

> `upstreamStatusError` and `readUpstreamErrorSnippet` are package-level helpers in `openai_responses.go` (reuse them; do not reimplement). Confirm `doNativeProxy` exists (vs `doNativeProxyWithDefaults`) in `proxy.go`; if only the `WithDefaults` form is exported, call it with a nil defaults map. Confirm `StreamEvent.ToolCall` is a `*inference.ToolCall` field and the `StreamEventToolCall` constant name against `internal/inference/types.go`.

- [ ] **Step 4: Register the client**

In `gateway/backend/cmd/gateway/main.go`, `providerClients`, add after the `ProviderVendorAnthropic` entry:

```go
		routing.ProviderVendorGemini: provider.NewGeminiClient(appHTTPClient),
```

- [ ] **Step 5: Add the wiring test row**

In `gateway/backend/cmd/gateway/main_test.go`, `TestProviderClientsWireVendorProviderKinds`, add a row asserting that `routing.ProviderVendorGemini` dispatches to a client that POSTs to a `:generateContent` path with the `x-goog-api-key` header (match the existing rows' table shape — provider kind, expected path substring, expected header).

- [ ] **Step 6: Add the `provider → geminiwire` archtest edge**

In `gateway/backend/internal/archtest/arch_test.go`, add `"internal/geminiwire"` to the string slice of `allowedDeps["internal/provider"]` (so it becomes, in the file's own sort order, `{"internal/geminiwire", "internal/inference", "internal/routing", "internal/tracing"}`). `gemini.go` is the first `internal/provider` file to import `internal/geminiwire`, so the edge becomes real now.

Run: `cd gateway/backend && go test ./internal/archtest/`
Expected: PASS. If `TestAllowlistFrozen` fails, the slice edit is missing or `gemini.go` does not actually import `internal/geminiwire`.

- [ ] **Step 7: Run the provider + cmd tests**

Run: `cd gateway/backend && go test ./internal/provider/ ./cmd/gateway/`
Expected: PASS.

- [ ] **Step 8: Format, vet, commit**

Run: `cd gateway/backend && gofmt -l internal/provider internal/archtest cmd/gateway && go vet ./internal/provider/ ./internal/archtest/ ./cmd/gateway/`

```bash
git add gateway/backend/internal/provider/gemini.go gateway/backend/internal/provider/gemini_test.go gateway/backend/cmd/gateway/main.go gateway/backend/cmd/gateway/main_test.go gateway/backend/internal/archtest/arch_test.go
git commit -m "feat(provider): native Gemini generateContent client

Add GeminiClient, a Client/StreamingClient/NativeProxyClient that speaks
Gemini's generateContent and streamGenerateContent wire, delegating
render/parse to internal/geminiwire. It authenticates with x-goog-api-key,
puts the model (with a stripped models/ prefix and URL-escaped) in the
path, surfaces a bounded body snippet on non-2xx for 400 diagnostics,
scans alt=sse streams that carry no [DONE] (erroring when a stream ends
before a finish reason), and refuses to follow cross-host redirects so
the custom credential header never leaks. Register it for the
vendor_gemini provider kind and allow the provider->geminiwire import
edge in the architecture test."
```

---

### Task 6: Portal — native discovery, validation, vendor-id allowlist, flavor

**Files:**
- Create: `gateway/backend/internal/vendorauth/gemini.go` (`DiscoverGeminiModels`, `ValidateGeminiAPIKey`)
- Test: `gateway/backend/internal/vendorauth/gemini_test.go`
- Modify: `gateway/backend/internal/portal/service_vendor_discovery.go` (native branch + seam field)
- Modify: `gateway/backend/internal/portal/service_vendor_validation.go` (native branch)
- Modify: `gateway/backend/internal/portal/service_vendor_accounts.go` (`normalizeVendorAccountVendor` allowlist; `CreateVendorAccount` uses `IsPresetVendor`)
- Modify: `gateway/backend/internal/portal/vendor_catalog.go` (`vendorAPIFlavor` native case)
- Test: the portal `_test.go` siblings for discovery/validation/accounts

**Interfaces:**
- Consumes: `routing.VendorGoogleNative`, `IsNativeGeminiVendor`, `IsPresetVendor`, the `VendorGoogleNative` preset (Task 4).
- Produces:
  - `func DiscoverGeminiModels(ctx context.Context, httpClient *http.Client, baseURL, apiKey string) ([]vendorauth.Model, error)` — paged `GET {base}/v1beta/models?pageSize=1000`, `x-goog-api-key`, keep entries whose `supportedGenerationMethods` contains `generateContent`, strip `models/` from the id.
  - `func ValidateGeminiAPIKey(ctx context.Context, httpClient *http.Client, baseURL, apiKey string) error` — `x-goog-api-key` GET to `{base}/v1beta/models`, uniform classification.
  - A `google_native` account: `CreateVendorAccount` forces `api_key`, normalizes the base URL, discovers on create; `vendorAPIFlavor` returns the right dialect.

> Match `DiscoverGeminiModels`'s return type and the `Model` shape to what the existing `vendorauth` discoverers return, and the portal seam (`discoverAPIKeyModels` dispatch + any discoverer struct fields) to how the OpenAI-compatible native path is wired in `service_vendor_discovery.go`. Read those two files before writing; the exact field/type names there are authoritative.

- [ ] **Step 1: Write the failing vendorauth tests**

`gateway/backend/internal/vendorauth/gemini_test.go`:

```go
package vendorauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiscoverGeminiModelsPagesFiltersStrips(t *testing.T) {
	var pages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "k" {
			t.Errorf("missing api key header")
		}
		if r.URL.Query().Get("pageToken") == "" {
			pages++
			_, _ = w.Write([]byte(`{"models":[
				{"name":"models/gemini-2.5-pro","displayName":"Gemini 2.5 Pro","supportedGenerationMethods":["generateContent"]},
				{"name":"models/embedding-001","displayName":"Embedding","supportedGenerationMethods":["embedContent"]}],
				"nextPageToken":"p2"}`))
			return
		}
		pages++
		_, _ = w.Write([]byte(`{"models":[
			{"name":"models/gemini-2.5-flash","supportedGenerationMethods":["generateContent"]}]}`))
	}))
	defer srv.Close()
	models, err := DiscoverGeminiModels(context.Background(), srv.Client(), srv.URL, "k")
	if err != nil {
		t.Fatal(err)
	}
	if pages != 2 {
		t.Fatalf("pages = %d, want 2 (followed nextPageToken)", pages)
	}
	ids := map[string]bool{}
	for _, m := range models {
		ids[m.ID] = true // adapt .ID to the real Model field name
	}
	if !ids["gemini-2.5-pro"] || !ids["gemini-2.5-flash"] || ids["embedding-001"] {
		t.Fatalf("models = %+v (want generateContent-only, models/ stripped)", models)
	}
}

func TestValidateGeminiAPIKeyOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "k" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	if err := ValidateGeminiAPIKey(context.Background(), srv.Client(), srv.URL, "k"); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd gateway/backend && go test ./internal/vendorauth/ -run Gemini`
Expected: FAIL — functions undefined.

- [ ] **Step 3: Implement `DiscoverGeminiModels` + `ValidateGeminiAPIKey`**

Write `gateway/backend/internal/vendorauth/gemini.go` following the existing `openai_compatible.go` structure (bounded response reads via `io.LimitReader`, the same `Model` return type, the same error-classification helper the other validators use). The native listing shape is `{"models":[{"name":"models/…","displayName":…,"supportedGenerationMethods":[…]}],"nextPageToken":…}`; request `{base}/v1beta/models?pageSize=1000`, follow `nextPageToken` with a bounded page cap (e.g. 20), keep only entries whose methods contain `generateContent`, strip a leading `models/` from the id (and from the display name when it equals the id). Use `x-goog-api-key` for both functions. Classify validation uniformly: 2xx → ok; non-2xx or transport error → the package's "unverifiable"/invalid error (VERIFY-LIVE: a bad native key may be 401 rather than 400 — still unverifiable under the uniform rule).

- [ ] **Step 4: Wire the portal native branches**

- `service_vendor_discovery.go`: add a seam field for the native api-key path (mirror the OpenAI-compatible one) and, in `discoverAPIKeyModels`, branch on `IsNativeGeminiVendor(vendor)` **before** the OpenAI-compatible branch, calling `DiscoverGeminiModels` with the account base URL (default from the preset) and key. Keep applying the preset `StripModelsPrefix` to Slug/DisplayName.
- `service_vendor_validation.go`: branch on `IsNativeGeminiVendor` before the OpenAI-compatible branch, calling `ValidateGeminiAPIKey`.
- `service_vendor_accounts.go`: add `routing.VendorGoogleNative` to the `normalizeVendorAccountVendor` allowed-id switch; ensure `CreateVendorAccount`'s force-api-key + base-URL-normalize + discover-on-create path keys off `IsPresetVendor` (so it covers native too — confirm whether it already calls `IsOpenAICompatibleVendor` and switch that call to `IsPresetVendor`).
- `vendor_catalog.go`: in `vendorAPIFlavor`, return the correct result for a native vendor (it must not fall through the `IsOpenAICompatibleVendor`→openai mapping; a native Gemini account serves `[openai, anthropic]` like the other preset accounts — match `service_vendor_listing.go:vendorAccountServedFlavors`).

- [ ] **Step 5: Add/extend portal tests**

Add a native-discovery test (paging/filter/strip through the portal seam), a native-validate test, a `normalizeVendorAccountVendor` case accepting `google_native`, and an assertion that a `google_native` create forces `api_key`. Add a regression assertion that a shim `google` account still discovers/validates through the OpenAI-compatible path unchanged.

- [ ] **Step 6: Run the vendorauth + portal tests**

Run: `cd gateway/backend && go test ./internal/vendorauth/ ./internal/portal/`
Expected: PASS.

- [ ] **Step 7: Format, vet, commit**

Run: `cd gateway/backend && gofmt -l internal/vendorauth internal/portal && go vet ./internal/vendorauth/ ./internal/portal/`

```bash
git add gateway/backend/internal/vendorauth/gemini.go gateway/backend/internal/vendorauth/gemini_test.go gateway/backend/internal/portal/service_vendor_discovery.go gateway/backend/internal/portal/service_vendor_validation.go gateway/backend/internal/portal/service_vendor_accounts.go gateway/backend/internal/portal/vendor_catalog.go
git commit -m "feat(portal): native Gemini model discovery and key validation

Add vendorauth.DiscoverGeminiModels (paged GET /v1beta/models with
x-goog-api-key, generateContent-only filter, models/ prefix strip) and
ValidateGeminiAPIKey, and branch the portal discovery, validation,
vendor-id allowlist, create flow and api-flavor on IsNativeGeminiVendor
ahead of the OpenAI-compatible path. google_native accounts are forced
api-key and discovered on create; the shim google path is unchanged."
```

---

### Task 7: Gateway integration — capture redaction + usage-row label

**Files:**
- Modify: `gateway/backend/internal/gateway/capture.go` (`redactedCaptureHeaders`)
- Test: `gateway/backend/internal/gateway/capture_serial_test.go` (extend `TestRedactCaptureHeaders`)
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` (`upstreamPath` case)
- Test: `gateway/backend/internal/gateway/native_passthrough_test.go` (add the row)

**Interfaces:**
- Consumes: `routing.ProviderVendorGemini` (Task 4).
- Produces: `x-goog-api-key` redacted in payload captures; `upstreamPath` returns a Gemini label for the `vendor_gemini` kind.

- [ ] **Step 1: Write the failing tests**

Extend `TestRedactCaptureHeaders` (in `capture_serial_test.go`) to assert `x-goog-api-key` is redacted:

```go
	// among the existing header assertions:
	{name: "x-goog-api-key", in: "secret", wantRedacted: true},
```

(Adapt to the test's actual table shape.)

Add a `native_passthrough_test.go` case asserting `upstreamPath(routing.Target{Provider: routing.ProviderVendorGemini}, "openai_chat_completions")` returns the Gemini label (not `/v1/chat/completions`):

```go
	{provider: routing.ProviderVendorGemini, want: "/v1beta/models/*:generateContent"},
```

(Match the existing `upstreamPath` test's table shape and field names.)

- [ ] **Step 2: Run to verify they fail**

Run: `cd gateway/backend && go test ./internal/gateway/ -run 'TestRedactCaptureHeaders|upstreamPath'`
Expected: FAIL.

- [ ] **Step 3: Implement**

- In `capture.go`, add `"x-goog-api-key": {}` to `redactedCaptureHeaders` (SECURITY: the provider's `CaptureSink.RecordRequest` records outbound headers, so an unredacted native key would be stored in clear in payload captures).
- In `native_passthrough.go`, add a `case routing.ProviderVendorGemini: return "/v1beta/models/*:generateContent"` to the `upstreamPath` provider switch (cosmetic usage-row label; the function takes no model/stream argument, so the label is a fixed string).

- [ ] **Step 4: Run to verify they pass**

Run: `cd gateway/backend && go test ./internal/gateway/`
Expected: PASS.

- [ ] **Step 5: Format, vet, commit**

Run: `cd gateway/backend && gofmt -l internal/gateway && go vet ./internal/gateway/`

```bash
git add gateway/backend/internal/gateway/capture.go gateway/backend/internal/gateway/capture_serial_test.go gateway/backend/internal/gateway/native_passthrough.go gateway/backend/internal/gateway/native_passthrough_test.go
git commit -m "feat(gateway): redact x-goog-api-key and label native Gemini usage rows

Add x-goog-api-key to the capture redaction allowlist so a native Gemini
key is never stored in clear in payload captures, and give the
vendor_gemini provider kind its own upstreamPath label so Activity rows
are not mislabeled as /v1/chat/completions."
```

---

### Task 8: Frontend — vendor id, preset tuple, labels, i18n

**Files:**
- Modify: `gateway/frontend/src/api/vendorAccounts.ts` (vendor union)
- Modify: `gateway/frontend/src/components/shared/vendorPresets.ts` (`OPENAI_COMPATIBLE_VENDORS` tuple + `DEFAULT_BASE_URLS`)
- Modify: `gateway/frontend/src/components/shared/vendorLabel.ts` (`google_native` + `gemini` cases)
- Test: `gateway/frontend/src/components/shared/vendorLabel.test.ts` (and `vendorServerLabel.test.ts` if present)
- Modify: `gateway/frontend/src/i18n.ts` (de + en `vendorGoogleNative`)

**Interfaces:**
- Consumes: the backend vendor id `google_native` and the provider kind `vendor_gemini` (derived to `gemini` by `vendorServerLabel`).
- Produces: the create form (base-URL + api-key-only) works for `google_native`; Activity rows and the account list show "Google Gemini (native)".

- [ ] **Step 1: Write the failing label test**

In `vendorLabel.test.ts`, add:

```ts
it('labels native Gemini vendor id and provider-derived gemini', () => {
  expect(vendorLabel(t, 'google_native')).toBe(t.vendorGoogleNative);
  expect(vendorLabel(t, 'gemini')).toBe(t.vendorGoogleNative);
});
```

(Use the file's existing `t` fixture.)

- [ ] **Step 2: Run to verify it fails**

Run: `cd gateway/frontend && npx vitest run src/components/shared/vendorLabel.test.ts`
Expected: FAIL.

- [ ] **Step 3: Implement**

- `api/vendorAccounts.ts`: add `'google_native'` to the vendor string union.
- `vendorPresets.ts`: add `'google_native'` to the `OPENAI_COMPATIBLE_VENDORS` tuple (this drives the base-URL + api-key-only create form) and a `DEFAULT_BASE_URLS['google_native'] = 'https://generativelanguage.googleapis.com'` entry.
- `vendorLabel.ts`: add `case 'google_native':` and `case 'gemini':` both returning `t.vendorGoogleNative`.
- `i18n.ts`: add `vendorGoogleNative` — de `'Google Gemini (nativ)'`, en `'Google Gemini (native)'` — next to each `vendorGoogle` key.

- [ ] **Step 4: Run frontend tests + format check**

Run: `cd gateway/frontend && npx vitest run src/components/shared/ && npm run format:check`
Expected: PASS (and prettier clean — run `npm run format` if format:check flags the edits).

- [ ] **Step 5: Commit**

```bash
git add gateway/frontend/src/api/vendorAccounts.ts gateway/frontend/src/components/shared/vendorPresets.ts gateway/frontend/src/components/shared/vendorLabel.ts gateway/frontend/src/components/shared/vendorLabel.test.ts gateway/frontend/src/i18n.ts
git commit -m "feat(frontend): native Google Gemini vendor option

Add the google_native vendor id to the vendor union, the
OpenAI-compatible preset tuple (base-URL + api-key create form) with its
default base URL, and a Google Gemini (native) label for both the
google_native vendor id and the vendor_gemini-derived gemini server
label, with de/en i18n."
```

---

### Task 9: Docs — ADR-054 and architecture updates

**Files:**
- Modify: `gateway/.../docs/architecture/09-architecture-decisions.md` (add ADR-054)
- Modify: `docs/architecture/cross-cutting/external-vendor-accounts.md`
- Modify: `docs/architecture/reference/api-surface.md`
- Modify: `docs/architecture/03-context-and-scope.md`
- Modify: `docs/architecture/05-building-block-view.md`
- Modify: `docs/architecture/cross-cutting/compatibility-and-inference.md`

> Use the repo-root-relative `docs/architecture/...` paths. Do NOT write the branch-local working-docs directory as a literal anywhere.

**Interfaces:** documentation only; no code depends on this task.

- [ ] **Step 1: Write ADR-054**

Add **ADR-054 "Native Google Gemini dialect"** after ADR-053, superseding ADR-052(e)'s "native Gemini deferred". Record: the `vendor_gemini` provider kind + `google_native` vendor id + the `Dialect` preset field as the native-vs-shim selector (no DB migration); `internal/geminiwire` as the shared, leaf translation home (imports only `inference`); `x-goog-api-key` auth + capture redaction; the thoughtSignature-and-function-name-via-tool-call-id scheme; translate-only scope, with passthrough deferred to #204 and broad translate fidelity to #208.

- [ ] **Step 2: Update `external-vendor-accounts.md`**

- §1 vendor table: add `google_native` (native Gemini) alongside `google` (shim).
- §4 provider-kinds list: add the fourth kind `vendor_gemini` and describe its two paths (translate now; passthrough via #204).
- §4.4 / registry table: add the `VendorGoogleNative` preset row and the `Dialect` field; note the predicate split.
- §6.1 discovery: add the native `/v1beta/models` discovery + `x-goog-api-key`.
- served matrix: `google_native` serves `[openai, anthropic]`.
- §10 VERIFY-LIVE: fold in the spec §11 items.

- [ ] **Step 3: Update the remaining docs**

- `api-surface.md`: add `google_native` to the vendor-id lists.
- `03-context-and-scope.md`: note the native Gemini dialect as a supported outbound vendor.
- `05-building-block-view.md`: add `internal/geminiwire` and the `GeminiClient` to the provider building-block description.
- `compatibility-and-inference.md` §8 provider-clients table: add the native Gemini client row.

- [ ] **Step 4: Run the docs gate**

Run: the repo's `check-docs` (e.g. `make check-docs` — confirm the exact target).
Expected: PASS. If it fails on a forbidden literal, remove the branch-local working-docs directory string from the offending file.

- [ ] **Step 5: Commit**

```bash
git add docs/architecture/
git commit -m "docs: native Google Gemini dialect (ADR-054 + architecture)

Record the native Gemini dialect: the vendor_gemini provider kind, the
google_native vendor id, the Dialect preset selector, internal/geminiwire
as the shared translation home, x-goog-api-key auth and capture
redaction, the thoughtSignature-via-tool-call-id scheme, and the
translate-only scope (passthrough -> #204, broad fidelity -> #208).
Supersede ADR-052(e). Update the external-vendor-accounts, api-surface,
context, building-block and compatibility docs."
```

---

## Final verification (before finishing the branch)

Run from the worktree root:

```bash
cd gateway/backend && go build ./... && go vet ./... && go test ./...
```

```bash
cd gateway/backend && golangci-lint fmt --diff && golangci-lint run
```

```bash
cd gateway/frontend && npm run test && npm run format:check && npm run build
```

- Run `check-docs`.
- Run the repo's Sonar gate (`make sonar-gate` / branch-findings) and confirm 0 branch-attributed / new-code findings.
- No Postgres leg is required (no schema change).
- Walk the spec §11 VERIFY-LIVE list: either validated against a real key, or left as a conservative default with a `// VERIFY-LIVE:` marker.
- As the LAST step before the PR, remove the branch-local working-docs directory (this plan + its spec) so it never reaches the PR, then push over SSH and open the PR (English title/body; substantive commit body already in place).

## Self-review notes

- **Spec coverage:** §2 scope → Tasks 1–9 (no neutral-model surgery; passthrough via `ProxyNative` in Task 5, broad fidelity deferred). §3 provider kind/vendor id/selection/resolver → Task 4. §4 geminiwire package + archtest → Tasks 1–3. §5 translation rules → Tasks 2 (render) + 3 (parse). §6 client → Task 5. §7 discovery/validation/capture/labels/frontend → Tasks 6, 7, 8. §8 name collisions → documented in Task 9 (external-vendor-accounts) and the model-prefix advice. §9 testing → each task's tests + the final verification. §10 docs → Task 9. §11 VERIFY-LIVE → carried as `// VERIFY-LIVE:` markers and the final-verification walk.
- **Type consistency:** `RenderRequest` returns a `GenerateContentRequest` (no error; total/tolerant like `anthropicMessages`) and the client marshals it — a deliberate, recorded deviation from the spec's "(+error)" wording, since render cannot fail and the only marshal error is the client's `ErrInvalidResponse`. `Result`, `ParseResponse`, `ParseStreamChunk`, `encodeToolCallID`/`decodeToolCallID` signatures are identical across Tasks 1→2→3→5. `IsPresetVendor`/`IsOpenAICompatibleVendor`/`IsNativeGeminiVendor` are defined in Task 4 and consumed in Tasks 5–8.
- **Review Focus:** all five pinned (prompt-block → T3; functionResponse name fallback → T2; stream-ends-early → T5; bad tool args → T2/T3; redirect → T5).
