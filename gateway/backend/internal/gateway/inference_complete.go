// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/compat"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"strings"
	"time"
)

const (
	msgStreamIdleTimeout = "stream idle timeout"
	msgProviderError     = "provider error"

	// Responses-API SSE event names emitted by more than one code path below.
	eventResponseOutputItemAdded = "response.output_item.added"
	eventResponseOutputItemDone  = "response.output_item.done"
)

// complete resolves the target, calls the provider, and writes the single JSON
// client body (success envelope via render, or apierror on failure). It serializes
// that body exactly once and hands the same bytes to the capture, so the capture
// is the true client response — never a re-serialization.
func (s *Server) complete(w http.ResponseWriter, r *http.Request, token auth.Token, req inference.Request, raw []byte, render func(provider.Response) any) {
	start := time.Now()
	id := nextRequestID()
	capturing := s.capturingEnabled(token)
	target, err := s.resolveTarget(r.Context(), token, req)
	if err != nil {
		slog.Warn("inference resolve failed", "path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model, "code", completionErrorCode(err), "status", completionHTTPStatus(err), "err", err)
		body := writeCompletionErrorCaptured(w, err)
		s.recordUsage(start, token, req, routing.Target{}, provider.Response{}, completionErrorCode(err), "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: completionHTTPStatus(err), ContentType: "application/json"}, id, buildCaptureInput(capturing, token.UserID, token.Secret, r, raw, w.Header(), body, completionHTTPStatus(err), req.APIFlavor))
		return
	}
	// Register the in-flight request now that routing has resolved the target
	// server, so the running-connections view can label it with the server name
	// (mirrors completeStream, which registers after Resolve). A resolve failure
	// above never reaches a server, so it is intentionally not tracked as active.
	s.Active.Add(ActiveRequest{ID: id, UserID: token.UserID, TokenID: token.ID, TokenName: token.Name, ServiceID: token.ServiceID, ServiceName: token.ServiceName, ServerName: s.serverName(target.ServerID), ServerID: target.ServerID, Model: req.Model, RequestedModel: req.RequestedModel, APIFlavor: req.APIFlavor, ReqPath: r.URL.Path, ProviderPath: upstreamPath(target, req.APIFlavor), ProviderModel: effectiveProviderModel(target, req.Model), SessionID: req.ClientSessionID, SessionSource: req.SessionSource, AgentID: req.AgentID, Stream: false, StartedAt: start})
	defer s.Active.Remove(id)
	providerReq := req
	if target.ProviderModel != "" {
		providerReq.Model = target.ProviderModel
	}
	// When capturing on the translate path, thread a capture sink so the provider
	// records the translated upstream request/response; nil (no sink) otherwise.
	provCtx := r.Context()
	var sink *provider.CaptureSink
	if capturing {
		sink = provider.NewCaptureSink(s.captureMaxBytes)
		provCtx = provider.WithCaptureSink(provCtx, sink)
	}
	// Attach the resolved application's per-app upstream credential (fail-open).
	provCtx = s.upstreamAuthCtx(provCtx, target)
	resp, err := s.Provider.Complete(provCtx, target, providerReq)
	status := "success"
	errorCode := ""
	var body []byte
	if err != nil {
		status = "error"
		errorCode = completionErrorCode(err)
		slog.Error("inference failed", "path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model, "server", s.serverName(target.ServerID), "code", errorCode, "err", err)
		body = writeCompletionErrorCaptured(w, err)
	} else {
		slog.Debug("inference ok", "path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model, "server", s.serverName(target.ServerID), "output_tokens", resp.Usage.OutputTokens, "duration_ms", time.Since(start).Milliseconds())
		body = writeJSONCaptured(w, http.StatusOK, render(resp))
	}
	ci := buildCaptureInput(capturing, token.UserID, token.Secret, r, raw, w.Header(), body, completionHTTPStatus(err), req.APIFlavor)
	attachTranslatedCapture(ci, sink)
	s.recordUsage(start, token, req, target, resp, errorCode, status, usageMeta{ReqPath: r.URL.Path, HTTPStatus: completionHTTPStatus(err), ContentType: "application/json"}, id, ci)
}

func (s *Server) completeStream(w http.ResponseWriter, r *http.Request, token auth.Token, req inference.Request, raw []byte) {
	id := nextRequestID()
	ss, ok := s.beginStream(w, r, token, req, raw, id, func(resp provider.Response) any {
		return compat.OpenAIChatResponse("chatcmpl_mock", req.Model, resp.Text, resp.ToolCalls, resp.Usage)
	})
	if !ok {
		return
	}
	defer ss.close()

	created := time.Now().Unix()
	writeChunk := func(delta map[string]any, finishReason any) error {
		choice := map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}
		payload, _ := json.Marshal(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   req.Model,
			"choices": []map[string]any{choice},
		})
		return ss.emit(fmt.Sprintf("data: %s\n\n", payload))
	}

	var usage inference.Usage
	var toolCalls []inference.ToolCall
	streamErr := ss.stream(func(ev inference.StreamEvent) error {
		switch ev.Type {
		case inference.StreamEventTextDelta:
			delta := map[string]any{}
			if ev.Text != "" {
				delta["content"] = ev.Text
			}
			if ev.Reasoning != "" {
				delta["reasoning_content"] = ev.Reasoning
			}
			if len(delta) == 0 {
				return nil
			}
			return writeChunk(delta, nil)
		case inference.StreamEventToolCall:
			// Buffer assembled tool calls; they are streamed as tool_calls delta
			// chunks after the text, before the terminal finish chunk.
			if ev.ToolCall != nil {
				toolCalls = append(toolCalls, *ev.ToolCall)
			}
			return nil
		case inference.StreamEventCompleted:
			if ev.Usage != nil {
				usage = *ev.Usage
			}
			return nil
		}
		return nil
	})

	status, errorCode, clientGone := ss.terminalStatus(streamErr)
	switch errorCode {
	case "":
		// Stream each assembled tool call as chat.completion.chunk tool_calls deltas
		// (an opening delta with id/name + empty args, then the full-arguments
		// delta — the canonical incremental shape that OpenAI SDK clients accumulate
		// by index), then a terminal chunk whose finish_reason reflects tool use.
		finish := "stop"
		for i, tc := range toolCalls {
			args := tc.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			callID := tc.ID
			if callID == "" {
				callID = fmt.Sprintf("call_%d", i)
			}
			_ = writeChunk(map[string]any{"tool_calls": []map[string]any{{
				"index": i, "id": callID, "type": "function",
				"function": map[string]any{"name": tc.Name, "arguments": ""},
			}}}, nil)
			_ = writeChunk(map[string]any{"tool_calls": []map[string]any{{
				"index":    i,
				"function": map[string]any{"arguments": args},
			}}}, nil)
		}
		if len(toolCalls) > 0 {
			finish = "tool_calls"
		}
		_ = writeChunk(map[string]any{}, finish)
		if req.IncludeUsage {
			usageObj := map[string]any{
				"prompt_tokens":     usage.InputTokens,
				"completion_tokens": usage.OutputTokens,
				"total_tokens":      usage.TotalTokens,
			}
			if usage.CachedTokens > 0 {
				usageObj["prompt_tokens_details"] = map[string]any{"cached_tokens": usage.CachedTokens}
			}
			usagePayload, _ := json.Marshal(map[string]any{
				"id":      id,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   req.Model,
				"choices": []any{},
				"usage":   usageObj,
			})
			_ = ss.emit(fmt.Sprintf("data: %s\n\n", usagePayload))
		}
		slog.Debug("inference stream ok", "path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model, "server", s.serverName(ss.target.ServerID), "output_tokens", usage.OutputTokens, "tool_calls", len(toolCalls), "duration_ms", time.Since(ss.start).Milliseconds())
	case errCodeStreamIdleTimeout:
		payload, _ := json.Marshal(apierror.Response(errorCode, msgStreamIdleTimeout, ""))
		_ = ss.emit(fmt.Sprintf("data: %s\n\n", payload))
	case errCodeClientDisconnected:
		// Client disconnected — the socket is gone, so write no frame.
	default:
		payload, _ := json.Marshal(apierror.Response(errorCode, msgProviderError, ""))
		_ = ss.emit(fmt.Sprintf("data: %s\n\n", payload))
	}
	if !clientGone {
		_ = ss.emit("data: [DONE]\n\n")
	}
	ss.finish(usage, status, errorCode)
}

// completeStreamResponses streams an OpenAI *Responses* API reply as Server-Sent
// Events. It mirrors completeStream's resolve/register/idle-watchdog/write-deadline
// machinery, but the upstream provider stays protocol-agnostic (it yields the same
// internal inference.StreamEvent text deltas) — the difference is purely at the
// edge: instead of chat.completion.chunk frames, this emits the Responses streaming
// event sequence (response.created -> output_item.added -> content_part.added ->
// output_text.delta* -> output_text.done -> content_part.done -> output_item.done ->
// response.completed) that clients such as Codex consume. Each event is a named SSE
// frame (`event: <type>` + `data: <json>`) carrying its own `type` and an
// incrementing `sequence_number`. A Responses stream ends with response.completed
// (or response.failed) and connection close — there is no chat-style `[DONE]`.
func (s *Server) completeStreamResponses(w http.ResponseWriter, r *http.Request, token auth.Token, req inference.Request, raw []byte) {
	id := nextRequestID()
	ss, ok := s.beginStream(w, r, token, req, raw, id, func(resp provider.Response) any {
		return compat.OpenAIResponsesResponse("resp_"+id, req.Model, resp.Text, resp.Reasoning, resp.ToolCalls, resp.Usage)
	})
	if !ok {
		return
	}
	defer ss.close()

	createdAt := time.Now().Unix()
	seq := 0
	emitEvent := func(eventType string, payload map[string]any) error {
		payload["type"] = eventType
		payload["sequence_number"] = seq
		seq++
		data, _ := json.Marshal(payload)
		return ss.emit(fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, data))
	}

	respID := "resp_" + id
	msgID := "msg_" + id
	// responseObject builds the `response` envelope embedded in response.created /
	// .in_progress / .completed / .failed; output and usage are filled only once known.
	responseObject := func(status string, output []any, usage *inference.Usage) map[string]any {
		obj := map[string]any{
			"id":         respID,
			"object":     "response",
			"created_at": createdAt,
			"status":     status,
			"model":      req.Model,
			"output":     output,
		}
		if usage != nil {
			obj["usage"] = map[string]any{
				"input_tokens":  usage.InputTokens,
				"output_tokens": usage.OutputTokens,
				"total_tokens":  usage.TotalTokens,
			}
		}
		return obj
	}
	messageItem := func(status string, content []any) map[string]any {
		return map[string]any{"id": msgID, "type": "message", "status": status, "role": "assistant", "content": content}
	}
	outputTextPart := func(text string) map[string]any {
		return map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
	}
	// functionCallItem builds a Responses function_call output item. call_id is the
	// correlation key the client echoes back in the next function_call_output; the
	// item id is a distinct "fc_"-prefixed id; arguments is passed verbatim (the
	// caller uses "" for the added event and the full JSON string when done).
	functionCallItem := func(status string, tc inference.ToolCall, arguments string) map[string]any {
		callID := tc.ID
		if callID == "" {
			callID = "call_" + tc.Name
		}
		return map[string]any{
			"type":      "function_call",
			"id":        "fc_" + callID,
			"call_id":   callID,
			"name":      tc.Name,
			"arguments": arguments,
			"status":    status,
		}
	}

	reasoningID := "rs_" + id
	// reasoningItem builds a Responses `reasoning` output item, matching llama.cpp's
	// shape: id, empty summary, a reasoning_text content block (empty until known),
	// empty encrypted_content. Codex records it and replays it next turn.
	reasoningItem := func(text string) map[string]any {
		content := []any{}
		if text != "" {
			content = append(content, map[string]any{"type": "reasoning_text", "text": text})
		}
		return map[string]any{"id": reasoningID, "type": "reasoning", "summary": []any{}, "content": content, "encrypted_content": ""}
	}

	// Output items are assigned a contiguous output_index in emission order via
	// nextOut. The reasoning item (if any) opens first (index 0), then the message,
	// then function_calls — each LAZILY so a turn omits the items it doesn't produce
	// (a tool-only turn puts its function_call at index 0, matching the wire shape).
	nextOut := 0
	reasoningOpened := false
	reasoningIdx := 0
	openReasoning := func() {
		if reasoningOpened {
			return
		}
		reasoningOpened = true
		reasoningIdx = nextOut
		nextOut++
		_ = emitEvent(eventResponseOutputItemAdded, map[string]any{"output_index": reasoningIdx, "item": reasoningItem("")})
	}
	messageOpened := false
	msgIdx := 0
	openMessage := func() {
		if messageOpened {
			return
		}
		messageOpened = true
		msgIdx = nextOut
		nextOut++
		_ = emitEvent(eventResponseOutputItemAdded, map[string]any{"output_index": msgIdx, "item": messageItem("in_progress", []any{})})
		_ = emitEvent("response.content_part.added", map[string]any{"item_id": msgID, "output_index": msgIdx, "content_index": 0, "part": outputTextPart("")})
	}

	// Opening frames: the response is created + in_progress. The message item is
	// announced later, on demand (see openMessage).
	_ = emitEvent("response.created", map[string]any{"response": responseObject("in_progress", []any{}, nil)})
	_ = emitEvent("response.in_progress", map[string]any{"response": responseObject("in_progress", []any{}, nil)})

	var full strings.Builder
	var reasoningFull strings.Builder
	var usage inference.Usage
	var toolCalls []inference.ToolCall
	streamErr := ss.stream(func(ev inference.StreamEvent) error {
		switch ev.Type {
		case inference.StreamEventTextDelta:
			// Reasoning (the model's analysis channel) streams as its own Responses
			// `reasoning` item — emitted BEFORE the answer, matching llama.cpp — so
			// Codex records the chain-of-thought and replays it next turn.
			if ev.Reasoning != "" {
				openReasoning()
				reasoningFull.WriteString(ev.Reasoning)
				if err := emitEvent("response.reasoning_text.delta", map[string]any{"item_id": reasoningID, "output_index": reasoningIdx, "content_index": 0, "delta": ev.Reasoning}); err != nil {
					return err
				}
			}
			if ev.Text != "" {
				openMessage() // ensure the message item exists before its first delta
				full.WriteString(ev.Text)
				if err := emitEvent("response.output_text.delta", map[string]any{"item_id": msgID, "output_index": msgIdx, "content_index": 0, "delta": ev.Text}); err != nil {
					return err
				}
			}
			return nil
		case inference.StreamEventToolCall:
			// Buffer assembled tool calls; they are emitted as function_call items
			// after the message item closes (Codex reconstructs the call from the
			// output_item.done item).
			if ev.ToolCall != nil {
				toolCalls = append(toolCalls, *ev.ToolCall)
			}
			return nil
		case inference.StreamEventCompleted:
			if ev.Usage != nil {
				usage = *ev.Usage
			}
			return nil
		}
		return nil
	})

	status, errorCode, _ := ss.terminalStatus(streamErr)
	text := full.String()
	switch errorCode {
	case "":
		outputItems := []any{}
		// Close the reasoning item first (lowest index), when the model produced any.
		if reasoningOpened {
			rtext := reasoningFull.String()
			_ = emitEvent("response.reasoning_text.done", map[string]any{"item_id": reasoningID, "output_index": reasoningIdx, "content_index": 0, "text": rtext})
			reasoningDone := reasoningItem(rtext)
			_ = emitEvent(eventResponseOutputItemDone, map[string]any{"output_index": reasoningIdx, "item": reasoningDone})
			outputItems = append(outputItems, reasoningDone)
		}
		// Emit the message item when there was text, or when there are no tool calls
		// at all (a plain — possibly empty — text turn). A tool-only turn omits it so
		// the function_call takes the message's slot.
		if messageOpened || len(toolCalls) == 0 {
			openMessage() // no-op if already opened; opens an empty item for an empty answer
			_ = emitEvent("response.output_text.done", map[string]any{"item_id": msgID, "output_index": msgIdx, "content_index": 0, "text": text})
			_ = emitEvent("response.content_part.done", map[string]any{"item_id": msgID, "output_index": msgIdx, "content_index": 0, "part": outputTextPart(text)})
			completedItem := messageItem("completed", []any{outputTextPart(text)})
			_ = emitEvent(eventResponseOutputItemDone, map[string]any{"output_index": msgIdx, "item": completedItem})
			outputItems = append(outputItems, completedItem)
		}
		// Then each tool call as a function_call item. Codex reconstructs the call
		// from output_item.done; the arguments delta/done are emitted for other
		// Responses clients that accumulate them (added carries empty arguments).
		for _, tc := range toolCalls {
			outIdx := nextOut
			nextOut++
			args := tc.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			added := functionCallItem("in_progress", tc, "")
			itemID := added["id"]
			_ = emitEvent(eventResponseOutputItemAdded, map[string]any{"output_index": outIdx, "item": added})
			_ = emitEvent("response.function_call_arguments.delta", map[string]any{"item_id": itemID, "output_index": outIdx, "delta": args})
			_ = emitEvent("response.function_call_arguments.done", map[string]any{"item_id": itemID, "output_index": outIdx, "name": tc.Name, "arguments": args})
			doneItem := functionCallItem("completed", tc, args)
			_ = emitEvent(eventResponseOutputItemDone, map[string]any{"output_index": outIdx, "item": doneItem})
			outputItems = append(outputItems, doneItem)
		}
		_ = emitEvent("response.completed", map[string]any{"response": responseObject("completed", outputItems, &usage)})
		slog.Debug("inference stream ok", "path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model, "server", s.serverName(ss.target.ServerID), "output_tokens", usage.OutputTokens, "tool_calls", len(toolCalls), "duration_ms", time.Since(ss.start).Milliseconds())
	case errCodeStreamIdleTimeout:
		failed := responseObject("failed", []any{}, nil)
		failed["error"] = map[string]any{"code": errorCode, "message": msgStreamIdleTimeout}
		_ = emitEvent("response.failed", map[string]any{"response": failed})
	case errCodeClientDisconnected:
		// Client disconnected — the socket is gone, so write no frame.
	default:
		failed := responseObject("failed", []any{}, nil)
		failed["error"] = map[string]any{"code": errorCode, "message": msgProviderError}
		_ = emitEvent("response.failed", map[string]any{"response": failed})
	}
	ss.finish(usage, status, errorCode)
}

// completeStreamAnthropic streams an Anthropic Messages SSE response (the
// translate path — used when native passthrough is OFF and the upstream speaks
// only Chat Completions). It mirrors completeStreamResponses' resolve /
// active-registration / idle-watchdog / write-deadline / capture-tee machinery
// but emits the Anthropic event sequence: message_start → content_block_start /
// _delta / _stop (a text block, then a tool_use block per tool call via
// input_json_delta) → message_delta (stop_reason + cumulative usage) →
// message_stop. There is NO [DONE] sentinel (Anthropic ends on message_stop).
func (s *Server) completeStreamAnthropic(w http.ResponseWriter, r *http.Request, token auth.Token, req inference.Request, raw []byte) {
	id := nextRequestID()
	ss, ok := s.beginStream(w, r, token, req, raw, id, func(resp provider.Response) any {
		return compat.AnthropicMessageResponse("msg_"+id, req.Model, resp.Text, resp.Reasoning, resp.ToolCalls, resp.FinishReason, resp.Usage)
	})
	if !ok {
		return
	}
	defer ss.close()

	// emitEvent is the frame-shaping wrapper for the Anthropic wire format: it sets
	// the payload's `type` (Anthropic events carry a matching `type` in data, no
	// sequence_number) before handing the marshaled named-event frame to ss.emit.
	emitEvent := func(eventType string, payload map[string]any) error {
		payload["type"] = eventType
		data, _ := json.Marshal(payload)
		return ss.emit(fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, data))
	}

	msgID := "msg_" + id
	// message_start: the message envelope with empty content and an initial usage.
	// input_tokens isn't known until the stream completes (it arrives with the
	// completed event), so it is filled in the terminal message_delta instead.
	_ = emitEvent("message_start", map[string]any{
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         req.Model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})

	// Content blocks are assigned a contiguous index in emission order via nextIndex.
	// A thinking block (if the model reasoned) opens first (index 0), then the text
	// block, then tool_use blocks — the Anthropic ordering rule. Each opens LAZILY so
	// a turn omits the blocks it doesn't produce (a tool-only turn puts its first
	// tool_use at index 0). Anthropic streams one block at a time, so the thinking
	// block must be fully STOPPED before the text block starts (see closeThinking).
	nextIndex := 0
	thinkingOpened := false
	thinkingClosed := false
	thinkingIdx := 0
	openThinking := func() {
		if thinkingOpened {
			return
		}
		thinkingOpened = true
		thinkingIdx = nextIndex
		nextIndex++
		_ = emitEvent("content_block_start", map[string]any{"index": thinkingIdx, "content_block": map[string]any{"type": "thinking", "thinking": ""}})
	}
	closeThinking := func() {
		if !thinkingOpened || thinkingClosed {
			return
		}
		thinkingClosed = true
		// Anthropic thinking blocks carry a signature; on the translate path we mint
		// none, so emit an empty signature_delta before the stop (mirroring llama.cpp).
		_ = emitEvent("content_block_delta", map[string]any{"index": thinkingIdx, "delta": map[string]any{"type": "signature_delta", "signature": ""}})
		_ = emitEvent("content_block_stop", map[string]any{"index": thinkingIdx})
	}
	textOpened := false
	textIdx := 0
	openText := func() {
		if textOpened {
			return
		}
		closeThinking() // a thinking block must be fully closed before the text block starts
		textOpened = true
		textIdx = nextIndex
		nextIndex++
		_ = emitEvent("content_block_start", map[string]any{"index": textIdx, "content_block": map[string]any{"type": "text", "text": ""}})
	}

	var usage inference.Usage
	var finishReason string
	var toolCalls []inference.ToolCall
	streamErr := ss.stream(func(ev inference.StreamEvent) error {
		switch ev.Type {
		case inference.StreamEventTextDelta:
			// Reasoning (the model's analysis channel) streams as a thinking block —
			// emitted BEFORE the answer, mirroring llama.cpp — so Claude Code records
			// the chain-of-thought and replays it next turn (which threads back as
			// reasoning_content). The assembled text/reasoning is never re-sent
			// (Anthropic reconstructs from the deltas), so neither is accumulated.
			if ev.Reasoning != "" {
				openThinking() // ensure the thinking block exists before its first delta
				if err := emitEvent("content_block_delta", map[string]any{"index": thinkingIdx, "delta": map[string]any{"type": "thinking_delta", "thinking": ev.Reasoning}}); err != nil {
					return err
				}
			}
			if ev.Text != "" {
				openText() // ensure the text block exists (closing any thinking block first)
				if err := emitEvent("content_block_delta", map[string]any{"index": textIdx, "delta": map[string]any{"type": "text_delta", "text": ev.Text}}); err != nil {
					return err
				}
			}
			return nil
		case inference.StreamEventToolCall:
			// Buffer assembled tool calls; they are emitted as tool_use content blocks
			// after the text block closes.
			if ev.ToolCall != nil {
				toolCalls = append(toolCalls, *ev.ToolCall)
			}
			return nil
		case inference.StreamEventCompleted:
			if ev.Usage != nil {
				usage = *ev.Usage
			}
			finishReason = ev.FinishReason
			return nil
		}
		return nil
	})

	status, errorCode, _ := ss.terminalStatus(streamErr)
	switch errorCode {
	case "":
		// Close any thinking block first (e.g. a reasoning-only turn that never
		// produced text closes it here); no-op once the text block already closed it.
		closeThinking()
		// Close the text block when it was opened, or when there are no tool calls at
		// all (a plain — possibly empty — text turn always carries a text block). A
		// tool-only turn omits it so the first tool_use takes the next free index.
		if textOpened || len(toolCalls) == 0 {
			openText() // no-op if already opened; opens an empty text block for an empty answer
			_ = emitEvent("content_block_stop", map[string]any{"index": textIdx})
		}
		// Each tool call as a tool_use content block: content_block_start carries the
		// id + name + empty input:{}, then a single input_json_delta with the complete
		// arguments JSON string (a lone fragment that is the whole JSON is valid), then
		// content_block_stop. Anthropic's stream parser accumulates partial_json into
		// the block's input object. Indices continue contiguously from nextIndex.
		for _, tc := range toolCalls {
			idx := nextIndex
			nextIndex++
			args := tc.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			_ = emitEvent("content_block_start", map[string]any{"index": idx, "content_block": map[string]any{
				"type":  "tool_use",
				"id":    anthropicStreamToolUseID(tc),
				"name":  tc.Name,
				"input": map[string]any{},
			}})
			_ = emitEvent("content_block_delta", map[string]any{"index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": args}})
			_ = emitEvent("content_block_stop", map[string]any{"index": idx})
		}
		// message_delta carries the final stop_reason (mapped from the upstream
		// finish_reason) and cumulative usage (input_tokens excludes cache reads, which
		// are reported separately in cache_read_input_tokens).
		usageOut := map[string]any{"input_tokens": compat.AnthropicInputTokens(usage), "output_tokens": usage.OutputTokens}
		if usage.CachedTokens > 0 {
			usageOut["cache_read_input_tokens"] = usage.CachedTokens
		}
		_ = emitEvent("message_delta", map[string]any{
			"delta": map[string]any{"stop_reason": compat.AnthropicStopReason(finishReason, len(toolCalls) > 0), "stop_sequence": nil},
			"usage": usageOut,
		})
		_ = emitEvent("message_stop", map[string]any{})
		slog.Debug("inference stream ok", "path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model, "server", s.serverName(ss.target.ServerID), "output_tokens", usage.OutputTokens, "tool_calls", len(toolCalls), "duration_ms", time.Since(ss.start).Milliseconds())
	case errCodeStreamIdleTimeout:
		_ = emitEvent("error", map[string]any{"error": map[string]any{"type": "api_error", "message": msgStreamIdleTimeout}})
	case errCodeClientDisconnected:
		// Client disconnected — the socket is gone, so write no frame.
	default:
		_ = emitEvent("error", map[string]any{"error": map[string]any{"type": "api_error", "message": msgProviderError}})
	}
	ss.finish(usage, status, errorCode)
}

// anthropicStreamToolUseID mirrors compat's tool_use id policy: use the upstream
// call id when present, else synthesize a "toolu_"-prefixed id from the name. The
// follow-up tool_result.tool_use_id must echo whatever id is emitted here.
func anthropicStreamToolUseID(tc inference.ToolCall) string {
	if strings.TrimSpace(tc.ID) != "" {
		return tc.ID
	}
	return "toolu_" + tc.Name
}

// usageMeta carries the per-request facts that only the HTTP call site knows
// about the response it actually wrote: the request path, the HTTP status, and
// the Content-Type on the wire. ContentType is passed rather than derived from
// req.Stream because some streaming call sites fall back to a JSON error body
// (e.g. a pre-stream resolve error), so req.Stream alone would misreport it.
type usageMeta struct {
	ReqPath     string
	HTTPStatus  int
	ContentType string
}

// opportunisticEWMAAlpha weights each live throughput sample against the running
// mapping average when the application opts into opportunistic metric updates.
const opportunisticEWMAAlpha = 0.2

func (s *Server) recordUsage(start time.Time, token auth.Token, req inference.Request, target routing.Target, resp provider.Response, errorCode, status string, meta usageMeta, id string, capture *captureInput) {
	// Gateway-wide accounting convention: the stored usage event splits the prompt
	// tokens into three DISJOINT buckets so they map cleanly onto Anthropic-style
	// read/write pricing — input_tokens = only FRESH (base) tokens, cached_tokens =
	// cache READ, cache_write_tokens = cache WRITE (creation). resp.Usage.InputTokens
	// is the OpenAI-canonical value that INCLUDES both cache subsets (that convention
	// is kept in provider/inference so the protocol-specific CLIENT responses stay
	// wire-correct — OpenAI prompt_tokens includes cached, Anthropic input_tokens
	// excludes it via compat.AnthropicInputTokens). The split happens HERE, at the
	// single accounting choke point, by subtracting both cache buckets. Only the
	// Anthropic format carries a write count, so cache_write_tokens is 0 for
	// OpenAI/Responses/translate traffic. total_tokens stays the full total, so
	// input + cached + write + output == total.
	freshInputTokens := resp.Usage.InputTokens - resp.Usage.CachedTokens - resp.Usage.CacheWriteTokens
	if freshInputTokens < 0 {
		freshInputTokens = 0
	}
	// Best-effort: a Record failure is logged (in addition to whatever the store
	// itself logs internally, see setLastUsageError) but never faults the
	// already-served response, and never skips the unconditional Limiter.Record
	// below — this call inserting a usage_events row is fire-and-forget from the
	// HTTP response's perspective (the client already got its answer by the
	// time recordUsage runs).
	if err := s.Usage.Record(usage.Event{
		ID:               id,
		UserID:           token.UserID,
		TokenID:          token.ID,
		SessionID:        req.ClientSessionID,
		SessionSource:    req.SessionSource,
		AgentID:          req.AgentID,
		APIFlavor:        req.APIFlavor,
		Model:            req.Model,
		RequestedModel:   req.RequestedModel,
		RouteID:          target.RouteID,
		Provider:         target.Provider,
		Host:             target.ServerID,
		InputTokens:      freshInputTokens,
		OutputTokens:     resp.Usage.OutputTokens,
		TotalTokens:      resp.Usage.TotalTokens,
		CachedTokens:     resp.Usage.CachedTokens,
		CacheWriteTokens: resp.Usage.CacheWriteTokens,
		PromptPerSecond:  resp.Usage.PromptPerSecond,
		TokensPerSecond:  resp.Usage.TokensPerSecond,
		HTTPStatus:       meta.HTTPStatus,
		ContentType:      meta.ContentType,
		ReqPath:          meta.ReqPath,
		ProviderPath:     upstreamPath(target, req.APIFlavor),
		ProviderModel:    effectiveProviderModel(target, req.Model),
		Stream:           req.Stream,
		TokenName:        token.Name,
		ServerName:       s.serverName(target.ServerID),
		ServiceID:        token.ServiceID,
		ServiceName:      token.ServiceName,
		ProjectID:        token.ProjectID,
		ProjectName:      token.ProjectName,
		LatencyMS:        time.Since(start).Milliseconds(),
		Status:           status,
		ErrorCode:        errorCode,
		CreatedAt:        time.Now().UTC(),
	}); err != nil {
		slog.Error("usage record failed", "id", id, "err", err)
	}
	// A recorded row means the activity views may be stale; signal subscribers.
	// Fire-and-forget and unconditional: a spurious signal only triggers a refetch
	// that finds nothing new, which is harmless.
	s.UsageEvents.Publish()
	// Opt-in payload capture: capture is already nil unless the caller decided to
	// capture; capturingEnabled re-checks the FULL gate (global switch + opt-in +
	// store/cipher wiring) defensively — capturingEnabled (capture.go) is the single
	// place that logic lives, so this never drifts out of sync with it again.
	// Fire-and-forget (persistCapture logs+discards errors).
	if capture != nil && s.capturingEnabled(token) {
		s.persistCapture(id, capture)
	}
	// Opportunistic metric feedback: on a SUCCESSFUL real inference whose serving
	// application opted in, EWMA-update the mapping's throughput from this sample.
	// Best-effort (the store guards metrics_locked + skips non-positive samples);
	// a failure is Debug-logged, never propagated, never faults the served response.
	if status == "success" && target.OpportunisticMetrics && target.RouteID != "" &&
		(resp.Usage.TokensPerSecond > 0 || resp.Usage.PromptPerSecond > 0) {
		if err := s.Routes.UpdateMappingOpportunisticMetrics(context.Background(), target.RouteID,
			resp.Usage.TokensPerSecond, resp.Usage.PromptPerSecond, opportunisticEWMAAlpha, time.Now().UTC()); err != nil {
			slog.Debug("opportunistic metrics update failed", "mapping", target.RouteID, "err", err)
		}
	}
	// Principal-limit accounting (design spec §6.2/§6.3), always LAST: bump the
	// in-memory aggregate cache for whichever principal (service or user --
	// never both, "kein Stacking", see principalFor) this request's token
	// resolves to, so a dense run of requests from the same principal sees an
	// updated aggregate immediately without waiting on the cache TTL or a fresh
	// store read. Runs UNCONDITIONALLY -- for BOTH a successful response and a
	// resolve/provider error -- mirroring the s.Usage.Record call above: every
	// recordUsage call inserts exactly one usage_events row for this principal
	// regardless of outcome, and UsageAggregateSince's request/token sums are
	// plain COUNT/SUM over that table with no status filter, so Record's
	// population must match it exactly.
	//
	// Cost is passed as 0 here -- not an omission, but the exact value a fresh
	// UsageAggregateSince read would return right now: the usage_events row this
	// call just inserted always has energy_wh == 0 at this instant, because
	// energy is priced ASYNCHRONOUSLY afterwards by the energy reconciler
	// (energy_reconciler.go, after its settle delay), and UsageAggregateSince's
	// cost is derived entirely from summed energy_wh weighted by price-per-kWh
	// (see SQLStore.UsageAggregateSince). Once the reconciler prices the row,
	// the aggregate cache's TTL (or the next calendar-period rollover)
	// transparently reloads the correctly-priced cost from the store on this
	// principal's next Admit -- Record's only job is to keep pace with a dense
	// burst of requests between those reloads, never to be cost's source of
	// truth.
	if p, ok := principalFor(token); ok {
		s.Limiter.Record(p, int64(resp.Usage.TotalTokens), 0)
	}
}

func completionErrorResponse(err error) apierror.Body {
	code := completionErrorCode(err)
	message := "provider unavailable"
	if errors.Is(err, routing.ErrNoModelRoute) {
		message = "no route for requested model"
	}
	if errors.Is(err, routing.ErrNoHealthyHost) {
		message = "no healthy host for requested model"
	}
	if errors.Is(err, routing.ErrAdmissionQueueTimeout) {
		message = "capacity queue timed out"
	}
	if errors.Is(err, routing.ErrAdmissionQueueFull) {
		message = "capacity queue full — server overloaded"
	}
	if errors.Is(err, routing.ErrServerOverrideServerUnavailable) {
		message = "the server-override target is disabled or unreachable"
	}
	if errors.Is(err, routing.ErrServerOverrideModelUnavailable) {
		message = "the server-override target does not offer the requested model"
	}
	return apierror.Response(code, message, "")
}

// writeCompletionErrorCaptured writes the apierror body and returns the exact
// bytes written, for the capture pipeline.
func writeCompletionErrorCaptured(w http.ResponseWriter, err error) []byte {
	return writeJSONCaptured(w, completionHTTPStatus(err), completionErrorResponse(err))
}

// completionHTTPStatus maps a completion error to the HTTP status that was
// (or would be) written for it. Centralizing this lets recordUsage report an
// accurate http_status without duplicating writeCompletionErrorCaptured's mapping.
func completionHTTPStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	if errors.Is(err, routing.ErrAdmissionQueueTimeout) || errors.Is(err, routing.ErrAdmissionQueueFull) {
		return http.StatusServiceUnavailable
	}
	if errors.Is(err, routing.ErrServerOverrideModelUnavailable) {
		return http.StatusNotFound
	}
	if errors.Is(err, routing.ErrServerOverrideServerUnavailable) {
		return http.StatusBadGateway
	}
	return http.StatusBadGateway
}

func completionErrorCode(err error) string {
	switch {
	case errors.Is(err, routing.ErrNoModelRoute):
		return routing.ErrNoModelRoute.Error()
	case errors.Is(err, routing.ErrNoHealthyHost):
		return routing.ErrNoHealthyHost.Error()
	case errors.Is(err, routing.ErrAdmissionQueueTimeout):
		return routing.ErrAdmissionQueueTimeout.Error()
	case errors.Is(err, routing.ErrAdmissionQueueFull):
		return routing.ErrAdmissionQueueFull.Error()
	case errors.Is(err, routing.ErrServerOverrideServerUnavailable):
		return "server_override.server_unavailable"
	case errors.Is(err, routing.ErrServerOverrideModelUnavailable):
		return "server_override.model_unavailable"
	case errors.Is(err, provider.ErrTimeout):
		return provider.ErrTimeout.Error()
	case errors.Is(err, provider.ErrInvalidResponse):
		return provider.ErrInvalidResponse.Error()
	default:
		return provider.ErrUnavailable.Error()
	}
}
