package geminiwire

import (
	"encoding/json"
	"op-ai-gateway/internal/inference"
	"testing"
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
	id := encodeToolCallID("get_weather", "sig-xyz", 0)
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
		{Role: inference.RoleAssistant, ToolCalls: []inference.ToolCall{{ID: encodeToolCallID("f", "", 0), Name: "f", Arguments: "not json"}}},
	}}
	got := RenderRequest(req, "gemini-2.5-pro")
	if string(got.Contents[0].Parts[0].FunctionCall.Args) != "{}" {
		t.Fatalf("args = %s", got.Contents[0].Parts[0].FunctionCall.Args)
	}
}

func textMsg(role inference.Role, text string) inference.Message {
	return inference.Message{Role: role, Content: []inference.ContentPart{{Type: inference.ContentText, Text: text}}}
}

func toolResult(id, text string) inference.Message {
	m := textMsg(inference.RoleTool, text)
	m.ToolCallID = id
	return m
}

func assistantCall(id, name string) inference.Message {
	return inference.Message{Role: inference.RoleAssistant, ToolCalls: []inference.ToolCall{{ID: id, Name: name, Arguments: `{}`}}}
}

func TestRenderFunctionResponseNameFromEarlierCall(t *testing.T) {
	// A foreign id (not minted by this package) still resolves its mandatory
	// functionResponse.name from the assistant call that issued it.
	got := RenderRequest(inference.Request{Messages: []inference.Message{
		assistantCall("call_1", "lookup"),
		toolResult("call_1", "found"),
	}}, "gemini-2.5-pro")
	if fr := got.Contents[1].Parts[0].FunctionResponse; fr == nil || fr.Name != "lookup" {
		t.Fatalf("functionResponse = %+v", fr)
	}
}

func TestRenderFunctionResponseNameUsesEarlierCallsOnly(t *testing.T) {
	// A tool result placed BEFORE its call must not borrow the later call's name.
	got := RenderRequest(inference.Request{Messages: []inference.Message{
		toolResult("call_1", "early"),
		assistantCall("call_1", "lookup"),
	}}, "gemini-2.5-pro")
	if fr := got.Contents[0].Parts[0].FunctionResponse; fr == nil || fr.Name != "" {
		t.Fatalf("result before its call: functionResponse = %+v", fr)
	}

	// A reused id resolves to the most recent EARLIER call, not a later one.
	got = RenderRequest(inference.Request{Messages: []inference.Message{
		assistantCall("call_1", "first"),
		toolResult("call_1", "r1"),
		assistantCall("call_1", "second"),
		toolResult("call_1", "r2"),
	}}, "gemini-2.5-pro")
	if len(got.Contents) != 4 {
		t.Fatalf("contents = %+v", got.Contents)
	}
	if n := got.Contents[1].Parts[0].FunctionResponse.Name; n != "first" {
		t.Fatalf("first result name = %q, want first", n)
	}
	if n := got.Contents[3].Parts[0].FunctionResponse.Name; n != "second" {
		t.Fatalf("second result name = %q, want second", n)
	}
}

func TestRenderAssistantForeignToolCallID(t *testing.T) {
	// A foreign id carries no name/signature: the functionCall uses tc.Name and
	// no thoughtSignature is emitted.
	got := RenderRequest(inference.Request{Messages: []inference.Message{
		assistantCall("call_abc", "f"),
	}}, "gemini-3-pro")
	fc := got.Contents[0].Parts[0]
	if fc.FunctionCall == nil || fc.FunctionCall.Name != "f" || fc.ThoughtSignature != "" {
		t.Fatalf("functionCall part = %+v", fc)
	}

	// An encoded id with an empty name falls back to tc.Name but keeps its signature.
	got = RenderRequest(inference.Request{Messages: []inference.Message{
		assistantCall(encodeToolCallID("", "sig-only", 0), "g"),
	}}, "gemini-3-pro")
	fc = got.Contents[0].Parts[0]
	if fc.FunctionCall == nil || fc.FunctionCall.Name != "g" || fc.ThoughtSignature != "sig-only" {
		t.Fatalf("empty-name encoded id part = %+v", fc)
	}
}

func TestRenderAssistantTextArgsAndToolResultPassthrough(t *testing.T) {
	asst := inference.Message{
		Role:    inference.RoleAssistant,
		Content: []inference.ContentPart{{Type: inference.ContentText, Text: "checking"}},
		ToolCalls: []inference.ToolCall{{
			ID: encodeToolCallID("f", "", 0), Name: "f", Arguments: ` {"city":"NYC"} `,
		}},
	}
	got := RenderRequest(inference.Request{Messages: []inference.Message{
		asst,
		toolResult(encodeToolCallID("f", "", 0), `{"temp":9}`),
	}}, "gemini-2.5-pro")
	parts := got.Contents[0].Parts
	if len(parts) != 2 || parts[0].Text != "checking" || parts[1].FunctionCall == nil {
		t.Fatalf("model parts = %+v", parts)
	}
	if string(parts[1].FunctionCall.Args) != `{"city":"NYC"}` {
		t.Fatalf("args = %s", parts[1].FunctionCall.Args)
	}
	if resp := got.Contents[1].Parts[0].FunctionResponse.Response; string(resp) != `{"temp":9}` {
		t.Fatalf("object result must pass through unwrapped, got %s", resp)
	}
}

func TestRenderImageURLs(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantURI string // non-empty: expect a fileData part with this uri
	}{
		{name: "http", url: "http://example.com/a.png", wantURI: "http://example.com/a.png"},
		{name: "https", url: "https://example.com/a.png", wantURI: "https://example.com/a.png"},
		{name: "non-base64 data uri dropped", url: "data:image/png,QUJD"},
		{name: "data uri without media type dropped", url: "data:;base64,QUJD"},
		{name: "data uri without payload dropped", url: "data:image/png;base64,"},
		{name: "data uri without comma dropped", url: "data:image/png;base64"},
		{name: "unknown scheme dropped", url: "ftp://example.com/a.png"},
		{name: "empty dropped", url: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderRequest(inference.Request{Messages: []inference.Message{{
				Role: inference.RoleUser,
				Content: []inference.ContentPart{
					{Type: inference.ContentText, Text: "look"},
					{Type: inference.ContentImage, ImageURL: tc.url},
				},
			}}}, "gemini-2.5-pro")
			parts := got.Contents[0].Parts
			if tc.wantURI == "" {
				if len(parts) != 1 || parts[0].Text != "look" {
					t.Fatalf("image must be dropped, parts = %+v", parts)
				}
				return
			}
			if len(parts) != 2 || parts[1].FileData == nil || parts[1].FileData.FileURI != tc.wantURI || parts[1].InlineData != nil {
				t.Fatalf("parts = %+v", parts)
			}
		})
	}
}

func TestRenderToolChoiceModes(t *testing.T) {
	tools := []inference.Tool{{Name: "f", Parameters: map[string]any{"type": "object"}}}
	tests := []struct {
		name      string
		choice    any
		wantMode  string // "" means no toolConfig at all
		wantNames []string
	}{
		{name: "auto", choice: "auto", wantMode: "AUTO"},
		{name: "none", choice: "none", wantMode: "NONE"},
		{name: "any", choice: " Any ", wantMode: "ANY"},
		{name: "required", choice: "required", wantMode: "ANY"},
		{name: "flat named", choice: map[string]any{"type": "function", "name": "f"}, wantMode: "ANY", wantNames: []string{"f"}},
		{name: "unrecognized string", choice: "bogus"},
		{name: "unnamed object", choice: map[string]any{"type": "function"}},
		{name: "nil", choice: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderRequest(inference.Request{Tools: tools, ToolChoice: tc.choice}, "gemini-2.5-pro")
			if tc.wantMode == "" {
				if got.ToolConfig != nil {
					t.Fatalf("toolConfig = %+v, want nil", got.ToolConfig)
				}
				return
			}
			cfg := got.ToolConfig.FunctionCallingConfig
			if cfg.Mode != tc.wantMode || len(cfg.AllowedFunctionNames) != len(tc.wantNames) {
				t.Fatalf("functionCallingConfig = %+v", cfg)
			}
			for i, n := range tc.wantNames {
				if cfg.AllowedFunctionNames[i] != n {
					t.Fatalf("allowedFunctionNames = %v", cfg.AllowedFunctionNames)
				}
			}
		})
	}

	// tool_choice without any declared tool emits no toolConfig.
	if got := RenderRequest(inference.Request{ToolChoice: "auto"}, "gemini-2.5-pro"); got.ToolConfig != nil || got.Tools != nil {
		t.Fatalf("tools/toolConfig = %+v / %+v", got.Tools, got.ToolConfig)
	}
}

func TestRenderToolSchemaEmptyParameters(t *testing.T) {
	got := RenderRequest(inference.Request{Tools: []inference.Tool{{Name: "f"}}}, "gemini-2.5-pro")
	if s := string(got.Tools[0].FunctionDeclarations[0].ParametersJSONSchema); s != `{"type":"object"}` {
		t.Fatalf("schema = %s", s)
	}
	if got.ToolConfig != nil {
		t.Fatalf("no tool_choice must leave toolConfig unset, got %+v", got.ToolConfig)
	}
}

func TestRenderThinkingTiers(t *testing.T) {
	tests := []struct {
		model, effort string
		wantBudget    int    // 0 means no budget expected
		wantLevel     string // "" means no level expected
	}{
		{model: "gemini-2.5-pro", effort: "minimal", wantBudget: 1024},
		{model: "gemini-2.5-pro", effort: "low", wantBudget: 1024},
		{model: "gemini-2.5-flash", effort: "medium", wantBudget: 8192},
		{model: "gemini-2.5-pro", effort: " HIGH ", wantBudget: 24576},
		{model: "gemini-3-pro", effort: "low", wantLevel: "low"},
		{model: "gemini-3-pro", effort: "medium", wantLevel: "medium"},
		{model: "Gemini-3.1-Pro", effort: "high", wantLevel: "high"},
		{model: "gemini-3-pro", effort: "minimal", wantLevel: "low"},
	}
	for _, tc := range tests {
		t.Run(tc.model+"/"+tc.effort, func(t *testing.T) {
			cfg := RenderRequest(inference.Request{ReasoningEffort: tc.effort}, tc.model).GenerationConfig
			if cfg == nil || cfg.ThinkingConfig == nil {
				t.Fatalf("generationConfig = %+v", cfg)
			}
			th := cfg.ThinkingConfig
			if tc.wantBudget != 0 && (th.ThinkingBudget == nil || *th.ThinkingBudget != tc.wantBudget || th.ThinkingLevel != "") {
				t.Fatalf("thinking = %+v, want budget %d", th, tc.wantBudget)
			}
			if tc.wantLevel != "" && (th.ThinkingLevel != tc.wantLevel || th.ThinkingBudget != nil) {
				t.Fatalf("thinking = %+v, want level %s", th, tc.wantLevel)
			}
		})
	}
}

func TestRenderEmptyRequest(t *testing.T) {
	got := RenderRequest(inference.Request{}, "gemini-2.5-pro")
	if got.Contents != nil || got.SystemInstruction != nil || got.Tools != nil || got.ToolConfig != nil || got.GenerationConfig != nil {
		t.Fatalf("empty request must render empty, got %+v", got)
	}

	// Empty stop strings and an unset effort leave generationConfig absent.
	got = RenderRequest(inference.Request{Stop: []string{""}}, "gemini-2.5-pro")
	if got.GenerationConfig != nil {
		t.Fatalf("generationConfig = %+v, want nil", got.GenerationConfig)
	}
}

func TestRenderImageDataURIWithExtraParams(t *testing.T) {
	got := RenderRequest(inference.Request{Messages: []inference.Message{{
		Role:    inference.RoleUser,
		Content: []inference.ContentPart{{Type: inference.ContentImage, ImageURL: "data:image/png;charset=utf-8;base64,QUJD"}},
	}}}, "gemini-2.5-pro")
	if len(got.Contents) != 1 || len(got.Contents[0].Parts) != 1 {
		t.Fatalf("contents = %+v", got.Contents)
	}
	if blob := got.Contents[0].Parts[0].InlineData; blob == nil || blob.MimeType != "image/png" || blob.Data != "QUJD" {
		t.Fatalf("inlineData = %+v", blob)
	}
}

func TestRenderTurnWithNoRenderablePartsIsDropped(t *testing.T) {
	// A user message whose only content is an unsupported image renders no
	// parts, so no empty turn is emitted.
	got := RenderRequest(inference.Request{Messages: []inference.Message{{
		Role:    inference.RoleUser,
		Content: []inference.ContentPart{{Type: inference.ContentImage, ImageURL: "ftp://example.com/a.png"}},
	}}}, "gemini-2.5-pro")
	if got.Contents != nil {
		t.Fatalf("contents = %+v, want nil", got.Contents)
	}
}

func TestRenderToolSchemaUnmarshalableFallsBackToObject(t *testing.T) {
	got := RenderRequest(inference.Request{Tools: []inference.Tool{
		{Name: "f", Parameters: map[string]any{"bad": make(chan int)}},
	}}, "gemini-2.5-pro")
	if s := string(got.Tools[0].FunctionDeclarations[0].ParametersJSONSchema); s != `{"type":"object"}` {
		t.Fatalf("schema = %s", s)
	}
}
