// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strings"
)

// agentErrorFramePrefix is how the server agent's router opens the one terminal
// frame it ends a committed stream with when the request fails after 200 was sent
// (server-agent/internal/runtime/router.go, writeSSEError and the package doc's
// ACCEPTED TRADE-OFF): `data: {"error":{"code":"runtime.…","message":"…"}}\n\n`,
// with no `event:` line and no `type` field. An event is held back from the client
// only while its bytes are still compatible with this prefix, so every other frame
// -- the agent's `: keepalive` comments, the model server's own events -- reaches
// the client as soon as it arrives.
const agentErrorFramePrefix = `data: {"error":`

// agentErrorFrameMaxBytes bounds the one event the filter holds back. The agent's
// frame is a code and a one-line message; an event that opens like it and grows
// past this is released as it stands, and passes unchanged.
const agentErrorFrameMaxBytes = 64 << 10

// agentEventInspectMaxBytes bounds the copy of a forwarded Responses event the
// filter keeps to learn the stream's response id and sequence number.
const agentEventInspectMaxBytes = 64 << 10

// agentCapacityErrorCodes are the agent codes that say the runtime had no room for
// the request rather than that it broke. An Anthropic client is told
// overloaded_error for them (it backs off and retries) and api_error for the rest.
var agentCapacityErrorCodes = map[string]bool{
	"runtime.admission_blocked":    true,
	"runtime.instance_busy":        true,
	"runtime.instance_no_capacity": true,
	"runtime.cluster_not_ready":    true,
	"runtime.start_timeout":        true,
}

// agentStreamFailure is the agent's terminal error frame, as recognized.
type agentStreamFailure struct {
	code    string
	message string
}

// agentStreamErrorFilter sits between the upstream body and the client on a
// streamed native passthrough to an agent-managed (server_agent) target. It finds
// the agent's terminal error frame and replaces it with the error event of the
// client's own API flavor, so a client sees its own protocol's failure instead of
// a frame it cannot parse (Claude Code fell back to a non-streaming retry on a
// stream without message_start; Codex saw no terminal response event). The
// failure it found is what proxyNative records the request's usage row with.
//
// Everything else passes byte for byte. The one change to the framing is that an
// event opening like the agent's frame is written once complete rather than as its
// pieces arrive.
//
// Recognized is exactly the agent's shape: a single `data:` line (no `event:`
// line) whose JSON is an object with one key, error, holding exactly a string
// code beginning "runtime." and a string message. A model server's own
// `data: {"error":…}` (OpenAI's chat stream error, llama.cpp's) carries no
// runtime. code and is never touched.
type agentStreamErrorFilter struct {
	flavor string

	// held is true while the current event's bytes are kept in heldBuf rather
	// than forwarded: from the event's first byte until it stops matching
	// agentErrorFramePrefix, grows past agentErrorFrameMaxBytes, or ends.
	held    bool
	heldBuf []byte
	// lineLen counts the current line's bytes other than CR; an LF that ends an
	// empty line ends the event.
	lineLen int

	// inspect is a bounded copy of the current forwarded event (Responses only),
	// read when it ends for the stream's response id and sequence number.
	inspect          []byte
	inspectTruncated bool
	responseID       string
	// fallbackResponseID names the response.failed of a stream that relayed no
	// response.created ("resp_" + the gateway's request id).
	fallbackResponseID string
	lastSeq            int
	seqSeen            bool

	failure *agentStreamFailure
}

// newAgentStreamErrorFilter answers the filter for a streamed request to an
// agent-managed target in a flavor whose stream has an error event of its own to
// translate to, and nil for every other relay (the copier then relays exactly as
// before). Chat completions never reaches it: a chat request takes the translate
// path, not native passthrough, and OpenAI's own chat stream error already is the
// agent's `data: {"error":…}` shape.
func newAgentStreamErrorFilter(target routing.Target, apiFlavor string, stream bool, requestID string) *agentStreamErrorFilter {
	if !stream || target.Provider != routing.ProviderServerAgent {
		return nil
	}
	switch apiFlavor {
	case inference.APIFlavorAnthropicMessages, inference.APIFlavorOpenAIResponses:
		return &agentStreamErrorFilter{flavor: apiFlavor, held: true, fallbackResponseID: "resp_" + requestID}
	}
	return nil
}

// push takes one upstream chunk and answers the bytes to write to the client now.
func (f *agentStreamErrorFilter) push(chunk []byte) []byte {
	var out []byte
	from := 0 // first byte of chunk not yet written or held
	for i, b := range chunk {
		if f.held {
			f.heldBuf = append(f.heldBuf, b)
			if !f.candidate() {
				out = append(out, f.heldBuf...)
				f.observe(f.heldBuf)
				f.heldBuf = nil
				f.held = false
			}
			from = i + 1
		} else if f.flavor == inference.APIFlavorOpenAIResponses {
			f.observe(chunk[i : i+1])
		}
		if !f.endOfEvent(b) {
			continue
		}
		if f.held {
			out = append(out, f.finishHeld()...)
		} else {
			out = append(out, chunk[from:i+1]...)
			f.finishForwarded()
		}
		from = i + 1
		f.held = true
		f.heldBuf = nil
	}
	if !f.held {
		out = append(out, chunk[from:]...)
	}
	return out
}

// recordedFailure answers the agent failure the stream ended with, or nil
// (nil-safe, so a relay without a filter asks it too).
func (f *agentStreamErrorFilter) recordedFailure() *agentStreamFailure {
	if f == nil {
		return nil
	}
	return f.failure
}

// flush answers the bytes still held when the upstream body ends: a partial event
// that opened like the agent's frame but never ended, relayed as it stands.
func (f *agentStreamErrorFilter) flush() []byte {
	if f == nil {
		return nil
	}
	out := f.heldBuf
	f.heldBuf = nil
	return out
}

// candidate reports whether the held bytes may still become the agent's frame.
func (f *agentStreamErrorFilter) candidate() bool {
	if len(f.heldBuf) > agentErrorFrameMaxBytes {
		return false
	}
	if len(f.heldBuf) <= len(agentErrorFramePrefix) {
		return strings.HasPrefix(agentErrorFramePrefix, string(f.heldBuf))
	}
	return bytes.HasPrefix(f.heldBuf, []byte(agentErrorFramePrefix))
}

// endOfEvent advances the line state by one byte and reports whether it ended the
// event (an LF ending an empty line; a CR does not count as content).
func (f *agentStreamErrorFilter) endOfEvent(b byte) bool {
	switch b {
	case '\n':
		ended := f.lineLen == 0
		f.lineLen = 0
		return ended
	case '\r':
		return false
	default:
		f.lineLen++
		return false
	}
}

// observe keeps the bounded copy of a forwarded Responses event.
func (f *agentStreamErrorFilter) observe(b []byte) {
	if f.flavor != inference.APIFlavorOpenAIResponses || f.inspectTruncated {
		return
	}
	if len(f.inspect)+len(b) > agentEventInspectMaxBytes {
		f.inspectTruncated = true
		return
	}
	f.inspect = append(f.inspect, b...)
}

// finishForwarded reads a forwarded Responses event for the response id (from
// response.created) and the sequence number, then clears the copy.
func (f *agentStreamErrorFilter) finishForwarded() {
	defer func() { f.inspect, f.inspectTruncated = nil, false }()
	if f.flavor != inference.APIFlavorOpenAIResponses || f.inspectTruncated {
		return
	}
	for _, payload := range jsonPayloads(f.inspect) {
		var probe struct {
			Type           string `json:"type"`
			SequenceNumber *int   `json:"sequence_number"`
			Response       struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if json.Unmarshal(payload, &probe) != nil {
			continue
		}
		if probe.SequenceNumber != nil {
			f.lastSeq, f.seqSeen = *probe.SequenceNumber, true
		}
		if probe.Type == "response.created" && probe.Response.ID != "" {
			f.responseID = probe.Response.ID
		}
	}
}

// finishHeld answers the client's bytes for a complete held event: the flavor's
// own error event when it is the agent's frame, the event unchanged otherwise.
func (f *agentStreamErrorFilter) finishHeld() []byte {
	raw := f.heldBuf
	failure, ok := parseAgentErrorFrame(raw)
	if !ok {
		return raw
	}
	if f.failure == nil {
		f.failure = &failure
	}
	if f.flavor == inference.APIFlavorAnthropicMessages {
		return anthropicAgentErrorEvent(failure)
	}
	return f.responsesAgentErrorEvents(failure)
}

// parseAgentErrorFrame recognizes the agent's terminal frame in one complete event.
func parseAgentErrorFrame(event []byte) (agentStreamFailure, bool) {
	text := strings.TrimRight(string(event), "\r\n")
	if strings.ContainsAny(text, "\r\n") {
		return agentStreamFailure{}, false // more than the one data: line
	}
	data, ok := strings.CutPrefix(text, "data:")
	if !ok {
		return agentStreamFailure{}, false
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &envelope) != nil || len(envelope) != 1 {
		return agentStreamFailure{}, false
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(envelope["error"], &body) != nil || len(body) != 2 {
		return agentStreamFailure{}, false
	}
	var failure agentStreamFailure
	if json.Unmarshal(body["code"], &failure.code) != nil || json.Unmarshal(body["message"], &failure.message) != nil {
		return agentStreamFailure{}, false
	}
	if !strings.HasPrefix(failure.code, "runtime.") {
		return agentStreamFailure{}, false
	}
	return failure, true
}

// anthropicAgentErrorEvent is the Anthropic Messages stream's error event for the
// agent's failure.
func anthropicAgentErrorEvent(failure agentStreamFailure) []byte {
	errType := "api_error"
	if agentCapacityErrorCodes[failure.code] {
		errType = "overloaded_error"
	}
	data, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": failure.code + ": " + failure.message},
	})
	return fmt.Appendf(nil, "event: error\ndata: %s\n\n", data)
}

// responsesAgentErrorEvents is the Responses stream's `error` event for the
// agent's failure followed by the response.failed that closes the response --
// shaped as the translate path's own response.failed (completeStreamResponses),
// with the agent's code as the error code. The response.failed is sent even when
// no response.created came first: Codex ignores a bare `error` event (it reported
// "stream closed before response.completed" and dropped the agent's message), and
// reads only response.failed's error object. Its id is the stream's own from
// response.created when one was relayed, else the gateway's (fallbackResponseID).
func (f *agentStreamErrorFilter) responsesAgentErrorEvents(failure agentStreamFailure) []byte {
	next := 0
	if f.seqSeen {
		next = f.lastSeq + 1
	}
	errData, _ := json.Marshal(map[string]any{
		"type":            "error",
		"sequence_number": next,
		"code":            failure.code,
		"message":         failure.message,
		"param":           nil,
	})
	out := fmt.Appendf(nil, "event: error\ndata: %s\n\n", errData)
	id := f.responseID
	if id == "" {
		id = f.fallbackResponseID
	}
	failedData, _ := json.Marshal(map[string]any{
		"type":            "response.failed",
		"sequence_number": next + 1,
		"response": map[string]any{
			"id":     id,
			"object": "response",
			"status": "failed",
			"error":  map[string]any{"code": failure.code, "message": failure.message},
		},
	})
	return fmt.Appendf(out, "event: response.failed\ndata: %s\n\n", failedData)
}
