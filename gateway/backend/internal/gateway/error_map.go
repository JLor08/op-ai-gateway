// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"errors"
	"net/http"
	"op-ai-gateway/internal/account"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
)

// errRow is one (sentinel -> HTTP response) mapping row consumed by
// writeMappedError. msg is the static response message; a row that must
// surface the underlying error's own text (rather than a fixed string) sets
// msgFn instead, which is called with the matched error to compute the
// message at write time.
type errRow struct {
	err    error
	status int
	code   string
	msg    string
	msgFn  func(err error) string
}

// sharedErrorMap holds sentinel mappings that are IDENTICAL (same HTTP status
// + apierror code + message) across every write*Error mapper in this package
// that handles them today (GW-7). A sentinel that maps differently in even
// one mapper -- store.ErrNotFound (different code per mapper),
// portal.ErrTokenNotFound (portal.token_not_found /
// service.token_not_found / token.not_found), and
// portal.ErrMappingStatusInvalid (mapping.status_invalid /
// model_group.status_invalid) are the known cases -- MUST NOT appear here;
// it stays a per-mapper row in that mapper's own extra list instead. Order
// does not matter: writeMappedError checks a mapper's extra rows before this
// table, so a mapper can still special-case a sentinel that would otherwise
// hit a shared row.
var sharedErrorMap = []errRow{
	{err: portal.ErrApplicationNotFound, status: http.StatusNotFound, code: portal.CodeApplicationNotFound, msg: msgApplicationNotFound},
	{err: portal.ErrServerNotFound, status: http.StatusNotFound, code: portal.CodeServerNotFound, msg: msgServerNotFound},
	{err: portal.ErrMappingNotFound, status: http.StatusNotFound, code: portal.CodeMappingNotFound, msg: msgMappingNotFound},
	{err: portal.ErrPathSuffixInvalid, status: http.StatusBadRequest, code: "application.path_suffix_invalid", msg: "path suffix must be a path, not a URL"},
	{err: portal.ErrChatNotFound, status: http.StatusNotFound, code: "portal.chat_not_found", msg: "chat not found"},
	{err: portal.ErrChatTooLarge, status: http.StatusBadRequest, code: "portal.chat_too_large", msg: "chat content is too large"},
	{err: account.ErrUserNotFound, status: http.StatusNotFound, code: "admin.user_not_found", msg: "user not found"},
	{err: portal.ErrLimitValidation, status: http.StatusBadRequest, code: "limit.validation_failed", msg: "limit configuration is invalid"},
	{err: portal.ErrGroupNameConflict, status: http.StatusConflict, code: "group.name_conflict", msg: "a group with this name already exists"},
	{err: portal.ErrGroupNameInvalid, status: http.StatusBadRequest, code: "group.name_invalid", msg: "group name is invalid"},
	{err: portal.ErrGroupParentInvalid, status: http.StatusBadRequest, code: "group.parent_invalid", msg: "parent group is invalid"},
	{err: portal.ErrGroupTierInvalid, status: http.StatusBadRequest, code: "group.tier_invalid", msg: "group tier is invalid"},
	{err: portal.ErrGroupForbidden, status: http.StatusForbidden, code: "group.forbidden", msg: "not allowed"},
	{err: portal.ErrProjectNotFound, status: http.StatusNotFound, code: "project.not_found", msg: "project not found"},
	{err: errAgentUnknownServer, status: http.StatusNotFound, code: "agent.unknown_server", msg: "unknown server"},
	{err: portal.ErrPrincipalForbidden, status: http.StatusForbidden, code: portal.CodePrincipalForbidden, msg: notAllowedMsg},
}

// writeMappedError matches err against extra (a mapper's own rows, checked
// first and in order) and then sharedErrorMap, via errors.Is; the first
// matching row's response is written and true is returned.
//
// If nothing matches and fallbackStatus > 0, the generic
// (fallbackStatus, fallbackCode, fallbackMsg) response is written. Passing
// fallbackStatus == 0 writes nothing on a miss instead, so a caller that
// needs a non-trivial fallback -- a dynamic errors.As-typed error, or a side
// effect (logging) that must run only on the unmapped path -- can apply it
// itself after checking the returned bool.
func writeMappedError(w http.ResponseWriter, err error, extra []errRow, fallbackStatus int, fallbackCode, fallbackMsg string) bool {
	for _, row := range extra {
		if errors.Is(err, row.err) {
			msg := row.msg
			if row.msgFn != nil {
				msg = row.msgFn(err)
			}
			writeJSON(w, row.status, apierror.Response(row.code, msg, ""))
			return true
		}
	}
	for _, row := range sharedErrorMap {
		if errors.Is(err, row.err) {
			writeJSON(w, row.status, apierror.Response(row.code, row.msg, ""))
			return true
		}
	}
	if fallbackStatus > 0 {
		writeJSON(w, fallbackStatus, apierror.Response(fallbackCode, fallbackMsg, ""))
	}
	return false
}
