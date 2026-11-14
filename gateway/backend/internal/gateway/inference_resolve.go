// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strings"
)

// resolveTarget is the single seam through which every inference path
// resolves a routing target (the translate path's complete and beginStream,
// through resolveTranslateTarget, plus tryProxyNative and the two images
// relays), so the last-used-model marker is recorded in exactly one place
// rather than at each of those call sites.
//
// The marker is a property of an api_tokens row (the `last_used_model` column),
// so it is written only for a principal that HAS such a row — i.e. one with a
// non-empty token id. A token-less session principal (sessionPrincipal, auth.go,
// leaves ID == "") has no row to address: the portal-chat run executor
// re-authenticates its loopback /v1/chat/completions hop as exactly that
// principal, so writing here would look up an empty token id, match no row, and
// log a "store: not found" once per chat turn against a row that was never owed
// (issue #27). The guard is on the id, NOT the model, on purpose: a POPULATED
// id that no longer resolves — a token deleted or expired between authentication
// and this write — is a real signal and must still be reported.
//
// Beyond that, the marker is written only when the effective model differs from
// what the token already carries, because a second unconditional write per
// inference request would double the token table's write load for no gain. A
// failed resolve records nothing — "last used" means last SUCCESSFULLY routed,
// so a typo or a dead model never becomes a token's redirect target.
//
// token is a POINTER so a successful write can refresh the caller's own
// token.LastUsedModel in place. That refresh is what keeps the marker written
// once per REQUEST rather than once per RESOLVE on the /v1/responses and
// /v1/messages non-native path, where tryProxyNative resolves (and writes) then
// declines to the translate path, which resolves again: token.LastUsedModel is
// an auth-time snapshot, so without the refresh the change-guard below would see
// the stale value on the second resolve and write the identical marker a second
// time (issue #96). The write itself is idempotent — the row ends at the same
// value — so this only avoids the redundant token-table write, never a wrong
// one. The refresh is a no-op for the single-resolve paths (chat completions,
// native passthrough): nothing resolves again to observe it.
//
// A write error is logged and swallowed: the marker is a convenience, never a
// reason to fail a request that already has a live target.
func (s *Server) resolveTarget(ctx context.Context, token *auth.Token, req inference.Request) (routing.Target, error) {
	target, err := s.Resolver.Resolve(ctx, *token, req)
	if err != nil {
		return target, err
	}
	if token.ID != "" && req.Model != "" && req.Model != token.LastUsedModel && s.LastUsedModelWriter != nil {
		if wErr := s.LastUsedModelWriter(ctx, token.ID, req.Model); wErr != nil {
			slog.Warn("last-used-model write failed", "token_id", token.ID, "user_id", token.UserID, "model", req.Model, "err", wErr)
		} else {
			// Refresh the caller's snapshot ONLY on a successful write, so a later
			// resolve of the SAME request (the non-native passthrough → translate
			// fallthrough) sees the value just persisted and suppresses the
			// redundant write via the guard above. On a failed write the snapshot
			// is left stale on purpose, so that second resolve still retries.
			token.LastUsedModel = req.Model
		}
	}
	return target, nil
}

// resolveTranslateTarget is resolveTarget for the text translate dispatch --
// complete and beginStream, the two functions every /v1/chat/completions
// request goes through, and every /v1/responses or /v1/messages request that
// tryProxyNative handed back or never saw (a body whose model the routing
// probe could not read) -- plus the one spec-flavor refusal that dispatch
// makes: a resolved target that serves images only (targetIsImagesOnly) or
// audio only, speech or transcription (targetIsAudioOnly), or decisions only (targetIsDecisionsOnly), is refused with
// routing.ErrNoModelRoute.
// Every request reaching it is a text request (images, speech and transcription
// have no translate path), so the refusal needs no flavor test of its own. It is
// deliberately not the general effective-served rule; see targetIsImagesOnly
// for why.
//
// Returning the sentinel, rather than answering here, is what makes the
// refusal indistinguishable from candidacy's own no-route answer when this
// was the model's only route: each caller's existing resolve-failure branch
// writes it (404 routing.no_model_route), records the usage row against
// routing.Target{} because no upstream was called, and -- in beginStream --
// does both before any byte of a stream is written.
//
// The refusal is NOT retried against another application that could serve
// the model, the same limitation targetServesFlavor's refusals carry in
// tryProxyNative and relayImages. And, like theirs, it comes after
// resolveTarget, so the token's last-used-model marker already names the
// refused model.
func (s *Server) resolveTranslateTarget(r *http.Request, token *auth.Token, req inference.Request) (routing.Target, error) {
	target, err := s.resolveTarget(r.Context(), token, req)
	if err != nil {
		return target, err
	}
	if targetIsImagesOnly(target) || targetIsAudioOnly(target) || targetIsDecisionsOnly(target) {
		slog.Debug("inference request rejected: resolved target serves images, audio or decisions only",
			"path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model,
			"server", s.serverName(target.ServerID), "route_id", target.RouteID)
		return routing.Target{}, routing.ErrNoModelRoute
	}
	if refusal := mediaURLsRefused(target, req); refusal != nil {
		slog.Warn("inference request rejected: image_url is a reference this target would resolve on the model host",
			"path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model,
			"server", s.serverName(target.ServerID), "route_id", target.RouteID,
			"spec_type", target.LiveProgressSpecType, "field", "image_url.url")
		return routing.Target{}, refusal
	}
	return target, nil
}

// errMediaURLUnsupported is the refusal of a request whose image reference an
// sglang_omni target cannot be given safely (anything but an inline data: URL);
// completionHTTPStatus answers it 400 with codeMediaURLUnsupported. Like the other
// resolve sentinels it is recorded as a row without an upstream call.
var errMediaURLUnsupported = errors.New(codeMediaURLUnsupported)

// errMediaPathUnsupported is errMediaURLUnsupported's sglang twin: the refusal of an
// image reference that is neither a data: nor an http(s) URL. It wraps
// errMediaURLUnsupported, so it shares its status and code; only the message
// (completionErrorResponse) differs, saying what an sglang target accepts.
var errMediaPathUnsupported = fmt.Errorf("%w: not a data: or http(s) URL", errMediaURLUnsupported)

// codeMediaURLUnsupported is API surface: clients branch on it.
const codeMediaURLUnsupported = "chat.media_url_unsupported"

// mediaURLsRefused returns the refusal of the first image of req whose reference
// target's effective runtime spec type would resolve on the model host, nil when
// there is none. It runs in resolveTranslateTarget, the one place every request that
// reaches a provider as inference messages (chat, and the translated Responses and
// Messages) passes after routing. Every other target is unchanged.
//
//   - sglang_omni (v0.2.0): only an inline data: URL is relayed
//     (errMediaURLUnsupported). sglang-omni reads a bare image_url path from the model
//     host, fetches any URL (private and metadata addresses included) and
//     authenticates nothing. The upstream's top-level audios, images and videos
//     fields never reach it: the chat body is parsed into a typed request
//     (compat.openAIChatRequest) and rebuilt from known fields only
//     (provider.OpenAICompatibleClient.Complete and streamRequestBody).
//   - sglang (v0.5.23): a data:, http:// or https:// URL is relayed, the scheme
//     matched case-insensitively, and everything else is refused
//     (errMediaPathUnsupported). SGLang reads a reference that starts with "/" or
//     "file://" from the model host (python/sglang/srt/utils/common.py,
//     get_image_bytes); no gateway client has a reason to name a model-host path. An
//     http(s) URL stays allowed, as it is for vLLM: blocking it would break the
//     ordinary vision-model use.
func mediaURLsRefused(target routing.Target, req inference.Request) error {
	var allowed func(string) bool
	var refusal error
	switch target.LiveProgressSpecType {
	case string(routing.RuntimeSpecTypeSGLangOmni):
		allowed, refusal = isDataURL, errMediaURLUnsupported
	case string(routing.RuntimeSpecTypeSGLang):
		allowed, refusal = isDataOrHTTPURL, errMediaPathUnsupported
	default:
		return nil
	}
	for _, m := range req.Messages {
		for _, p := range m.Content {
			if p.Type == inference.ContentImage && !allowed(p.ImageURL) {
				return refusal
			}
		}
	}
	return nil
}

// isDataOrHTTPURL reports whether s is a data:, http:// or https:// URL (the scheme is
// case-insensitive).
func isDataOrHTTPURL(s string) bool {
	if isDataURL(s) {
		return true
	}
	for _, scheme := range []string{"http://", "https://"} {
		if len(s) >= len(scheme) && strings.EqualFold(s[:len(scheme)], scheme) {
			return true
		}
	}
	return false
}
