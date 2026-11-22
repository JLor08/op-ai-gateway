// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"encoding/json"
	"op-ai-gateway/internal/routing"
	"strings"
)

// isStrataTarget reports whether target's effective kind (routing.Target.EffectiveKind)
// is strata: a strata application, or a server_agent spec of type strata.
func isStrataTarget(target routing.Target) bool {
	return target.EffectiveKind() == string(routing.RuntimeSpecTypeStrata)
}

// strataImageRefAllowed reports whether s is an image reference Strata resolves
// without touching the model host's file system: a data:, http:// or https:// URL.
// The prefixes are matched lowercase only, mirroring Strata's own startswith checks
// (terminal/serve/server.py, Vision.load): an odd-cased "FILE://" or "HTTP://x"
// falls to its local-path branch there, so it is refused here.
func strataImageRefAllowed(s string) bool {
	return strings.HasPrefix(s, "data:") || strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// strataPassthroughMediaRefused reports whether a raw /v1/messages or /v1/responses
// body, about to be relayed unexamined to a strata target, names an image Strata would
// resolve on the model host. Strata reads Anthropic image and document blocks from
// source.url and source.path and Responses input_image / image_url parts from
// image_url (terminal/serve/frontend.py, _image_source and responses.py, _content), all
// ending in Vision.load, which opens bare paths and file:// references.
//
// The walk visits every object at any depth, so blocks inside tool_result content and
// Responses function-call outputs are covered. A block is allowed only with
// source.type "base64" or a reference that strataImageRefAllowed accepts; a url or
// path source that carries no usable string is refused too. Malformed JSON is not
// refused here (false): the relay's existing handling owns it.
func strataPassthroughMediaRefused(raw []byte) bool {
	var doc any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if dec.Decode(&doc) != nil {
		return false
	}
	return strataValueRefused(doc)
}

func strataValueRefused(v any) bool {
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			if strataValueRefused(e) {
				return true
			}
		}
	case map[string]any:
		if strataBlockRefused(t) {
			return true
		}
		for _, e := range t {
			if strataValueRefused(e) {
				return true
			}
		}
	}
	return false
}

func strataBlockRefused(m map[string]any) bool {
	kind, _ := m["type"].(string)
	switch kind {
	case "image", "document":
		switch src := m["source"].(type) {
		case string:
			return !strataImageRefAllowed(src)
		case map[string]any:
			st, _ := src["type"].(string)
			if st == "base64" {
				return false
			}
			refused := false
			for _, key := range []string{"url", "path"} {
				if val, present := src[key]; present {
					if s, ok := val.(string); !ok || !strataImageRefAllowed(s) {
						refused = true
					}
				}
			}
			// A url or path source with no reference at all names nothing safe.
			if st == "url" || st == "path" {
				_, hasURL := src["url"]
				_, hasPath := src["path"]
				refused = refused || (!hasURL && !hasPath)
			}
			return refused
		case nil:
			// An image block without a source: the url/path keys may sit on the block (Strata reads "image_url" too).
		}
		return strataImageURLFieldRefused(m)
	case "input_image", "image_url":
		return strataImageURLFieldRefused(m)
	}
	return false
}

// strataImageURLFieldRefused checks a part's image_url, a string or {"url": ...}. An
// absent image_url is not refused: Strata rejects such a part itself.
func strataImageURLFieldRefused(m map[string]any) bool {
	val, present := m["image_url"]
	if !present {
		return false
	}
	switch u := val.(type) {
	case string:
		return !strataImageRefAllowed(u)
	case map[string]any:
		s, _ := u["url"].(string)
		return !strataImageRefAllowed(s)
	case nil:
		return false
	}
	return true
}
