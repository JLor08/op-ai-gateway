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
