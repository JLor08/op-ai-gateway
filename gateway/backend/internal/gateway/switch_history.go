// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync/atomic"
)

// switchTargetClass groups the targets by what they can read of a replayed
// reasoning/thinking item that another target produced.
type switchTargetClass int

const (
	// switchClassNone: no passthrough rules apply (e.g. Gemini, translate-only).
	switchClassNone switchTargetClass = iota
	switchClassOpenAIVendor
	switchClassAnthropicVendor
	// switchClassLocal: every non-vendor provider (on-prem inference servers).
	switchClassLocal
)

// switchTargetClassOf maps a target's provider kind to its switchTargetClass.
func switchTargetClassOf(t routing.Target) switchTargetClass {
	switch t.Provider {
	case routing.ProviderVendorOpenAI, routing.ProviderVendorOpenAISubscription:
		return switchClassOpenAIVendor
	case routing.ProviderVendorAnthropic:
		return switchClassAnthropicVendor
	case routing.ProviderVendorGemini:
		return switchClassNone
	default:
		return switchClassLocal
	}
}

// sanitizeSwitchHistory removes the reasoning/thinking items the target
// provably cannot read from the replayed history of a native passthrough body
// (step 1 of the provider-switch design): dropped counts removed items/blocks,
// stripped counts items that only lost a field. A body that needs no change is
// returned as the SAME slice; raw is never written to. Edited bodies keep every
// untouched value as json.RawMessage, so numbers and unknown fields survive.
func sanitizeSwitchHistory(raw []byte, flavor string, class switchTargetClass) (out []byte, dropped, stripped int) {
	switch {
	case flavor == inference.APIFlavorOpenAIResponses && class == switchClassOpenAIVendor:
		// Ungated gap, accepted: a reasoning item that lacks encrypted_content
		// entirely and has no reasoning_text is not found by this gate.
		if !bytes.Contains(raw, []byte(`"reasoning_text"`)) &&
			!hasKeyWithValue(raw, `"encrypted_content"`, `""`, `null`) {
			return raw, 0, 0
		}
		return rewriteHistory(raw, "input", func(items []json.RawMessage) ([]json.RawMessage, int, int, bool) {
			return sanitizeResponsesItems(items, class)
		})
	case flavor == inference.APIFlavorOpenAIResponses && class == switchClassLocal:
		// The bare word "reasoning" is not a trigger: every Codex body carries
		// the top-level reasoning config.
		if !bytes.Contains(raw, []byte(`"encrypted_content"`)) &&
			!hasKeyWithValue(raw, `"type"`, `"reasoning"`) {
			return raw, 0, 0
		}
		return rewriteHistory(raw, "input", func(items []json.RawMessage) ([]json.RawMessage, int, int, bool) {
			return sanitizeResponsesItems(items, class)
		})
	case flavor == inference.APIFlavorAnthropicMessages && class == switchClassAnthropicVendor:
		// Ungated gap, accepted: a thinking block with no signature key at all.
		if !hasKeyWithValue(raw, `"signature"`, `""`, `null`) {
			return raw, 0, 0
		}
		return rewriteHistory(raw, "messages", sanitizeAnthropicThinking)
	}
	return raw, 0, 0
}

// switchHistoryParses counts body parses, so a test can prove the byte gate
// keeps requests that need nothing from being parsed at all.
var switchHistoryParses atomic.Int64

// hasKeyWithValue reports whether raw contains key, then optional whitespace,
// ':', optional whitespace and one of the literal values.
func hasKeyWithValue(raw []byte, key string, values ...string) bool {
	for off := 0; ; {
		i := bytes.Index(raw[off:], []byte(key))
		if i < 0 {
			return false
		}
		rest := raw[off+i+len(key):]
		off += i + len(key)
		rest = bytes.TrimLeft(rest, " \t\r\n")
		if len(rest) == 0 || rest[0] != ':' {
			continue
		}
		rest = bytes.TrimLeft(rest[1:], " \t\r\n")
		for _, v := range values {
			if bytes.HasPrefix(rest, []byte(v)) {
				return true
			}
		}
	}
}

// rewriteHistory decodes the top level of raw, applies edit to the array under
// key and re-marshals only when edit reports a change. Anything that does not
// parse as expected leaves the body alone.
func rewriteHistory(raw []byte, key string, edit func([]json.RawMessage) ([]json.RawMessage, int, int, bool)) ([]byte, int, int) {
	switchHistoryParses.Add(1)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return raw, 0, 0
	}
	var items []json.RawMessage
	if err := json.Unmarshal(top[key], &items); err != nil {
		return raw, 0, 0
	}
	newItems, dropped, stripped, changed := edit(items)
	if !changed {
		return raw, 0, 0
	}
	arr, err := marshalNoEscape(newItems)
	if err != nil {
		return raw, 0, 0
	}
	top[key] = arr
	out, err := marshalNoEscape(top)
	if err != nil {
		return raw, 0, 0
	}
	return out, dropped, stripped
}

// marshalNoEscape marshals v without HTML escaping and without the trailing
// newline Encoder adds.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func isNonEmptyJSONString(r json.RawMessage) bool {
	var s string
	return json.Unmarshal(r, &s) == nil && s != "" && len(r) > 0 && r[0] == '"'
}

func isNonEmptyJSONArray(r json.RawMessage) bool {
	var a []json.RawMessage
	return json.Unmarshal(r, &a) == nil && len(a) > 0
}

// hasReadableReasoningText reports whether any entry of the content or summary
// arrays carries a non-empty "text" string.
func hasReadableReasoningText(item map[string]json.RawMessage) bool {
	for _, k := range []string{"content", "summary"} {
		var parts []map[string]json.RawMessage
		if json.Unmarshal(item[k], &parts) != nil {
			continue
		}
		for _, p := range parts {
			var t string
			if json.Unmarshal(p["text"], &t) == nil && t != "" {
				return true
			}
		}
	}
	return false
}

func sanitizeResponsesItems(items []json.RawMessage, class switchTargetClass) ([]json.RawMessage, int, int, bool) {
	var dropped, stripped int
	out := make([]json.RawMessage, 0, len(items))
	for _, it := range items {
		var m map[string]json.RawMessage
		if json.Unmarshal(it, &m) != nil || string(m["type"]) != `"reasoning"` {
			out = append(out, it)
			continue
		}
		strip := ""
		if class == switchClassOpenAIVendor {
			if !isNonEmptyJSONString(m["encrypted_content"]) {
				dropped++
				continue
			}
			if isNonEmptyJSONArray(m["content"]) {
				strip = "content"
			}
		} else {
			if !hasReadableReasoningText(m) {
				dropped++
				continue
			}
			if _, ok := m["encrypted_content"]; ok {
				strip = "encrypted_content"
			}
		}
		if strip == "" {
			out = append(out, it)
			continue
		}
		delete(m, strip)
		nb, err := marshalNoEscape(m)
		if err != nil {
			out = append(out, it)
			continue
		}
		stripped++
		out = append(out, nb)
	}
	return out, dropped, stripped, dropped+stripped > 0
}

// sanitizeAnthropicThinking removes unsigned thinking blocks from assistant
// turns, then removes turns left empty and merges the user turns that meet.
func sanitizeAnthropicThinking(msgs []json.RawMessage) ([]json.RawMessage, int, int, bool) {
	var dropped int
	out := make([]json.RawMessage, 0, len(msgs))
	for _, msg := range msgs {
		var m map[string]json.RawMessage
		if json.Unmarshal(msg, &m) != nil || string(m["role"]) != `"assistant"` {
			out = append(out, msg)
			continue
		}
		var blocks []json.RawMessage
		if json.Unmarshal(m["content"], &blocks) != nil {
			out = append(out, msg)
			continue
		}
		kept := make([]json.RawMessage, 0, len(blocks))
		for _, b := range blocks {
			var bm map[string]json.RawMessage
			if json.Unmarshal(b, &bm) == nil && string(bm["type"]) == `"thinking"` &&
				!isNonEmptyJSONString(bm["signature"]) {
				dropped++
				continue
			}
			kept = append(kept, b)
		}
		if len(kept) == len(blocks) {
			out = append(out, msg)
			continue
		}
		nc, err := marshalNoEscape(kept)
		if err != nil {
			out = append(out, msg)
			continue
		}
		m["content"] = nc
		nb, err := marshalNoEscape(m)
		if err != nil {
			out = append(out, msg)
			continue
		}
		out = append(out, nb)
	}
	if dropped == 0 {
		return msgs, 0, 0, false
	}
	return dropEmptyAssistantTurnsAndMergeUsers(out), dropped, 0, true
}

// dropEmptyAssistantTurnsAndMergeUsers removes every assistant message whose
// content is an empty array and merges each user message that thereby meets a
// preceding user message into it (content arrays concatenated in order; a string
// content becomes one text block), so strict role alternation survives. Message
// pairs that were already adjacent users are left alone. Unparseable messages
// are kept as they are.
func dropEmptyAssistantTurnsAndMergeUsers(msgs []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(msgs))
	removedSincePrev := false
	for _, msg := range msgs {
		var m map[string]json.RawMessage
		if json.Unmarshal(msg, &m) != nil {
			out = append(out, msg)
			removedSincePrev = false
			continue
		}
		role := string(m["role"])
		if role == `"assistant"` {
			var blocks []json.RawMessage
			if json.Unmarshal(m["content"], &blocks) == nil && len(blocks) == 0 {
				removedSincePrev = true
				continue
			}
		}
		if role == `"user"` && removedSincePrev && len(out) > 0 {
			if merged, ok := mergeUserTurns(out[len(out)-1], m); ok {
				out[len(out)-1] = merged
				removedSincePrev = false
				continue
			}
		}
		out = append(out, msg)
		removedSincePrev = false
	}
	return out
}

// mergeUserTurns appends next's content blocks to prev when prev is a user
// message; ok is false when prev is not (or either fails to parse).
func mergeUserTurns(prev json.RawMessage, next map[string]json.RawMessage) (json.RawMessage, bool) {
	var pm map[string]json.RawMessage
	if json.Unmarshal(prev, &pm) != nil || string(pm["role"]) != `"user"` {
		return nil, false
	}
	a, ok1 := contentBlocks(pm["content"])
	b, ok2 := contentBlocks(next["content"])
	if !ok1 || !ok2 {
		return nil, false
	}
	nc, err := marshalNoEscape(append(a, b...))
	if err != nil {
		return nil, false
	}
	pm["content"] = nc
	nb, err := marshalNoEscape(pm)
	if err != nil {
		return nil, false
	}
	return nb, true
}

func contentBlocks(c json.RawMessage) ([]json.RawMessage, bool) {
	var blocks []json.RawMessage
	if json.Unmarshal(c, &blocks) == nil {
		return blocks, true
	}
	var s string
	if json.Unmarshal(c, &s) != nil {
		return nil, false
	}
	tb, err := marshalNoEscape(map[string]string{"type": "text", "text": s})
	if err != nil {
		return nil, false
	}
	return []json.RawMessage{tb}, true
}

// blobHash identifies an opaque reasoning blob (encrypted_content, signature,
// redacted data) by the SHA-256 of its string value; the blob itself is never
// kept or logged.
type blobHash [32]byte

func hashBlob(raw json.RawMessage) (blobHash, bool) {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil || s == "" {
		return blobHash{}, false
	}
	return sha256.Sum256([]byte(s)), true
}

// blobOf returns the opaque blob of a history item or content block, or false
// when the item carries none.
func blobOf(flavor string, m map[string]json.RawMessage) (blobHash, bool) {
	typ := string(m["type"])
	switch {
	case flavor == inference.APIFlavorOpenAIResponses && typ == `"reasoning"`:
		return hashBlob(m["encrypted_content"])
	case flavor == inference.APIFlavorAnthropicMessages && typ == `"thinking"`:
		return hashBlob(m["signature"])
	case flavor == inference.APIFlavorAnthropicMessages && typ == `"redacted_thinking"`:
		return hashBlob(m["data"])
	}
	return blobHash{}, false
}

func historyKey(flavor string) string {
	if flavor == inference.APIFlavorOpenAIResponses {
		return "input"
	}
	return "messages"
}

// opaqueBlobHashes returns the hashes of every opaque reasoning blob in the
// replayed history of raw (responses: encrypted_content of reasoning items;
// messages: signature of thinking and data of redacted_thinking blocks in
// assistant turns). An unparseable body or an unknown flavor yields nil.
func opaqueBlobHashes(raw []byte, flavor string) []blobHash {
	if flavor != inference.APIFlavorOpenAIResponses && flavor != inference.APIFlavorAnthropicMessages {
		return nil
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return nil
	}
	var items []json.RawMessage
	if json.Unmarshal(top[historyKey(flavor)], &items) != nil {
		return nil
	}
	var out []blobHash
	for _, it := range items {
		var m map[string]json.RawMessage
		if json.Unmarshal(it, &m) != nil {
			continue
		}
		if flavor == inference.APIFlavorOpenAIResponses {
			if h, ok := blobOf(flavor, m); ok {
				out = append(out, h)
			}
			continue
		}
		if string(m["role"]) != `"assistant"` {
			continue
		}
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(m["content"], &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if h, ok := blobOf(flavor, b); ok {
				out = append(out, h)
			}
		}
	}
	return out
}

// removeBlobs removes the reasoning items/blocks whose blob hash is in drop and
// applies the empty-assistant-turn merge for the messages flavor. Nothing
// matching returns the SAME slice; raw is never written to.
func removeBlobs(raw []byte, flavor string, drop map[blobHash]struct{}) (out []byte, removed int) {
	if len(drop) == 0 {
		return raw, 0
	}
	switch flavor {
	case inference.APIFlavorOpenAIResponses:
		out, removed, _ = rewriteHistory(raw, "input", func(items []json.RawMessage) ([]json.RawMessage, int, int, bool) {
			kept := make([]json.RawMessage, 0, len(items))
			n := 0
			for _, it := range items {
				var m map[string]json.RawMessage
				if json.Unmarshal(it, &m) == nil {
					if h, ok := blobOf(flavor, m); ok {
						if _, hit := drop[h]; hit {
							n++
							continue
						}
					}
				}
				kept = append(kept, it)
			}
			return kept, n, 0, n > 0
		})
	case inference.APIFlavorAnthropicMessages:
		out, removed, _ = rewriteHistory(raw, "messages", func(msgs []json.RawMessage) ([]json.RawMessage, int, int, bool) {
			return removeAnthropicBlobs(msgs, drop)
		})
	default:
		return raw, 0
	}
	return out, removed
}

func removeAnthropicBlobs(msgs []json.RawMessage, drop map[blobHash]struct{}) ([]json.RawMessage, int, int, bool) {
	removed := 0
	out := make([]json.RawMessage, 0, len(msgs))
	for _, msg := range msgs {
		var m map[string]json.RawMessage
		if json.Unmarshal(msg, &m) != nil || string(m["role"]) != `"assistant"` {
			out = append(out, msg)
			continue
		}
		var blocks []json.RawMessage
		if json.Unmarshal(m["content"], &blocks) != nil {
			out = append(out, msg)
			continue
		}
		kept := make([]json.RawMessage, 0, len(blocks))
		for _, b := range blocks {
			var bm map[string]json.RawMessage
			if json.Unmarshal(b, &bm) == nil {
				if h, ok := blobOf(inference.APIFlavorAnthropicMessages, bm); ok {
					if _, hit := drop[h]; hit {
						removed++
						continue
					}
				}
			}
			kept = append(kept, b)
		}
		if len(kept) == len(blocks) {
			out = append(out, msg)
			continue
		}
		nc, err := marshalNoEscape(kept)
		if err != nil {
			out = append(out, msg)
			continue
		}
		m["content"] = nc
		nb, err := marshalNoEscape(m)
		if err != nil {
			out = append(out, msg)
			continue
		}
		out = append(out, nb)
	}
	if removed == 0 {
		return msgs, 0, 0, false
	}
	return dropEmptyAssistantTurnsAndMergeUsers(out), removed, 0, true
}

// isForeignReasoningRefusal reports whether an upstream answer is the 400 a
// target gives for a reasoning blob it cannot verify: OpenAI error code
// invalid_encrypted_content, or Anthropic's "Invalid `signature` in `thinking`
// block" message. Only a 400 with a parseable JSON error object matches.
func isForeignReasoningRefusal(status int, body []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	var env struct {
		Error struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil {
		return false
	}
	if code, ok := env.Error.Code.(string); ok && code == "invalid_encrypted_content" {
		return true
	}
	return strings.Contains(env.Error.Message, "Invalid `signature` in `thinking` block")
}
