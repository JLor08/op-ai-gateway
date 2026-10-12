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
