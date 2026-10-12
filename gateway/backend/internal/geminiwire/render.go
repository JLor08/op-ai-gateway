package geminiwire

import (
	"encoding/json"
	"op-ai-gateway/internal/inference"
	"strings"
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
	// names maps a tool-call id to the function name of the assistant call that
	// issued it, built from the assistant messages seen so far. A tool result is
	// resolved only against EARLIER calls, so a result placed before its call (or
	// a reused foreign id) never picks up a later, unrelated function's name.
	names := map[string]string{}
	for _, msg := range messages {
		switch msg.Role {
		case inference.RoleSystem, inference.RoleDeveloper:
			if t := msg.Text(); t != "" {
				sys = append(sys, t)
			}
		case inference.RoleTool:
			contents = appendTurn(contents, "user", []Part{functionResponsePart(msg, names)})
		case inference.RoleAssistant:
			for _, tc := range msg.ToolCalls {
				if tc.ID != "" && tc.Name != "" {
					names[tc.ID] = tc.Name
				}
			}
			contents = appendTurn(contents, "model", assistantParts(msg))
		default:
			contents = appendTurn(contents, "user", userParts(msg))
		}
	}
	return strings.Join(sys, "\n\n"), contents
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
		// A foreign id decodes to ("", "", false), so it falls back to tc.Name
		// with no signature; an encoded id restores both name and signature.
		name, sig, _ := decodeToolCallID(tc.ID)
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

func functionResponsePart(msg inference.Message, names map[string]string) Part {
	name, _, _ := decodeToolCallID(msg.ToolCallID)
	if name == "" {
		name = names[msg.ToolCallID] // "" when foreign with no earlier call; VERIFY-LIVE
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
