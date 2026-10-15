// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
)

// codeMessagesInvalidOutputConfig is the stable error code (AGENTS.md: dotted
// lowercase noun.reason; the code string is the wire contract) of the 400 a
// native Messages request earns when Claude Code's effort cannot be delivered
// as chat_template_kwargs.reasoning_effort without silently overriding or
// corrupting something the client sent. One code for all three causes; the
// message says which.
const codeMessagesInvalidOutputConfig = "messages.invalid_output_config"

// The JSON keys of the effort edit, named once.
const (
	outputConfigKey       = "output_config"
	outputConfigEffortKey = "effort"
	chatTemplateKwargsKey = "chat_template_kwargs"
	reasoningEffortKey    = "reasoning_effort"
)

// The three ways the effort edit can find the client's body unusable. Each is
// an invalid REQUEST, not a gateway fault, and each is a refusal to guess:
// the alternative to rejecting is overwriting or discarding a value the client
// sent. The error text is the client-facing message, so it names fields and
// never values.
var (
	errOutputConfigEffortNotString = errors.New("output_config.effort must be a string")
	errChatTemplateKwargsNotObject = errors.New("chat_template_kwargs must be an object when output_config.effort is set")
	errReasoningEffortConflict     = errors.New("chat_template_kwargs.reasoning_effort conflicts with output_config.effort; send one of them or make them equal")
)

// wantsChatTemplateEffort reports whether the native Messages relay for this
// request gets the effort edit (#230): the token opted in, the resolved target
// reads chat_template_kwargs.reasoning_effort, and the request is a Claude Code
// Messages one. Pure, and all three conjuncts are the caller's own state --
// nothing here reads the body, which is why the rejection of a malformed body
// can safely live behind it: a request that fails any conjunct is never
// inspected, so a body carrying a malformed output_config is forwarded exactly
// as it was before this feature existed.
//
// The JSON content-type condition is the caller's, next to the other two edits:
// it is a property of the relay, not of the request's identity.
func wantsChatTemplateEffort(token auth.Token, target routing.Target, apiFlavor string) bool {
	return token.ClaudeEffortCompatibility &&
		apiFlavor == "anthropic_messages" &&
		targetUsesChatTemplateKwargs(target)
}

// injectChatTemplateReasoningEffort delivers Claude Code's effort to a
// chat-template backend: it reads output_config.effort from the Messages body
// and merges it into chat_template_kwargs.reasoning_effort, leaving every other
// field exactly as the client wrote it. It returns the body, whether it changed
// it, and an invalid-request error for the three cases it refuses to guess at.
//
// Like injectTimingsPerToken and rewriteModelField it is a pure function of its
// argument: the input slice is never written to, and a changed body is always a
// FRESH slice from json.Marshal. That is load-bearing at the call site --
// proxyNative reads the CLIENT's bytes (rel.raw) again after dispatch to build
// the payload capture, and the earlier edits return those same bytes unchanged
// from their no-op branches, so an in-place edit here would put a key the client
// never sent into the capture. The body is re-serialized, so it is VALUE-lossless
// rather than byte-identical (keys reordered, <>& HTML-escaped), and UseNumber
// keeps numbers lossless too: without it a literal such as 9007199254740993
// would come back as 9007199254740992.
//
// What the effort is, and what it is not:
//
//   - A JSON string, trimmed of surrounding whitespace and forwarded by NAME.
//     There is no enum and no high->xhigh mapping: the chat template aliases
//     names itself, and mapping here would be the gateway second-guessing a
//     template it cannot see.
//   - Absent, null or blank means the client supplied no effort, so there is
//     nothing to deliver and NO default is invented. An output_config that is
//     not an object has no effort in it either, and is not a reason to reject.
//     Unrelated output_config properties are ignored.
//   - Present but not a string is errOutputConfigEffortNotString.
//
// The merge into chat_template_kwargs, once there is an effort to deliver:
//
//   - absent: the object is created holding only reasoning_effort;
//   - an object: reasoning_effort is added next to whatever else it holds, which
//     is kept as is;
//   - an object already holding the SAME string: the body is returned unchanged
//     (a no-op, with false), because there is nothing to add;
//   - an object already holding anything else under reasoning_effort -- a
//     different string, a non-string, null: errReasoningEffortConflict. The
//     client set that value deliberately, and overwriting it would be the silent
//     rewriting of a client request that this path refuses to do;
//   - anything that is not an object, JSON null included:
//     errChatTemplateKwargsNotObject. Reading null as "absent" would quietly
//     replace a value the client sent; no real client sends it.
//
// The body is returned unchanged, with false and a nil error, when it is not a
// JSON object or decodes to a nil map (a bare `null`). Those are
// injectTimingsPerToken's own guards, and for the same reason: a missing or
// null body is the caller's concern upstream of this edit, and this helper
// neither assumes that guard nor turns it into a rejection. A json.Marshal
// failure returns the body unchanged as well; no value a JSON decode can
// produce is one Marshal rejects.
func injectChatTemplateReasoningEffort(raw []byte) ([]byte, bool, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return raw, false, nil
	}
	if obj == nil {
		return raw, false, nil
	}

	effort, supplied, err := outputConfigEffort(obj)
	if err != nil {
		return raw, false, err
	}
	if !supplied {
		return raw, false, nil
	}

	kwargs := map[string]any{}
	if existing, present := obj[chatTemplateKwargsKey]; present {
		asObject, isObject := existing.(map[string]any)
		if !isObject {
			return raw, false, errChatTemplateKwargsNotObject
		}
		if current, has := asObject[reasoningEffortKey]; has {
			if s, isString := current.(string); isString && s == effort {
				return raw, false, nil
			}
			return raw, false, errReasoningEffortConflict
		}
		kwargs = asObject
	}
	kwargs[reasoningEffortKey] = effort
	obj[chatTemplateKwargsKey] = kwargs

	out, err := json.Marshal(obj)
	if err != nil {
		return raw, false, nil
	}
	return out, true, nil
}

// outputConfigEffort reads output_config.effort out of a decoded Messages body:
// the trimmed effort and whether the client supplied a usable one. An absent or
// null output_config, one that is not an object, and an effort that is absent,
// null or blank after trimming all answer ("", false, nil) -- "no effort
// supplied" -- and only an effort that is present and not a string is an error.
func outputConfigEffort(body map[string]any) (string, bool, error) {
	config, _ := body[outputConfigKey].(map[string]any)
	value, present := config[outputConfigEffortKey]
	if !present || value == nil {
		return "", false, nil
	}
	s, isString := value.(string)
	if !isString {
		return "", false, errOutputConfigEffortNotString
	}
	s = strings.TrimSpace(s)
	return s, s != "", nil
}

// rejectInvalidOutputConfig answers an effort-edit failure: HTTP 400 with
// codeMessagesInvalidOutputConfig and the failing rule's own message, recorded
// as a failed usage row against the resolved target and CAPTURED. It is
// proxyNative's own pre-dispatch rejection and takes the same shape as
// tryProxyNative's endpoint-disabled one for the same reason: writeRequestError
// writes an uncaptured response, and a 400 the operator cannot see in
// Activity/Logs is the one they cannot explain to the user who got it.
//
// It runs BEFORE the active row is added and before anything is sent upstream,
// so the rejection leaves no running-connections row and consumes no upstream
// capacity. ex.raw is the client's own bytes, which is what the capture keeps.
func (s *Server) rejectInvalidOutputConfig(w http.ResponseWriter, r *http.Request, ex nativeExchange, cause error) {
	slog.Warn("inference native passthrough rejected: invalid output_config",
		"path", r.URL.Path, "api_flavor", ex.req.APIFlavor, "model", ex.req.Model,
		"server", ex.serverName, "code", codeMessagesInvalidOutputConfig, "reason", cause.Error())
	body := writeJSONCaptured(w, http.StatusBadRequest, apierror.Response(codeMessagesInvalidOutputConfig, cause.Error(), ""))
	s.recordUsage(ex.start, ex.token, ex.req, ex.target, provider.Response{}, codeMessagesInvalidOutputConfig, "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: http.StatusBadRequest, ContentType: jsonContentType, BillingUnit: billingUnitFor(ex.req.APIFlavor), Endpoint: ex.endpoint}, ex.id, buildCaptureInput(ex.capturing, ex.token.UserID, ex.token.Secret, r, ex.raw, w.Header(), body, http.StatusBadRequest, ex.req.APIFlavor), nil)
}
