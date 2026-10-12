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
	// Unmarshal into a pointer so a JSON null payload leaves it nil and is
	// rejected, instead of decoding as the legitimate empty encoding ({}).
	var p *toolCallIDPayload
	if err := json.Unmarshal(raw, &p); err != nil || p == nil {
		return "", "", false
	}
	return p.N, p.S, true
}
