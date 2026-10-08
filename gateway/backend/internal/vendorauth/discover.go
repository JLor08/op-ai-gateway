// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// This file holds the model-discovery fetchers: one GET per credential kind that
// asks the vendor which models the credential can use, so a portal can offer the
// real catalog instead of a static seed. The URLs, and the REVERSE-ENGINEERED /
// LIVE-CONFIRMED status of the Codex catalog, live in constants.go.
//
// Discovery is advisory and must never block a caller, so it has no error return.
// Every fetcher answers a list plus a DiscoveryStatus: DiscoveryOK means the
// vendor served a usable list, DiscoveryUnverifiable means it could not be had
// (any non-2xx answer including 401, a redirect, a timeout, a transport failure,
// a body that is not the expected catalog, or a catalog with no usable model) and
// the caller keeps whatever it already has. An empty usable list is deliberately
// Unverifiable rather than "OK, no models": it is far more likely a changed
// schema or a filtered-out catalog than a credential with no models, and an empty
// OK would let a caller wipe a working set.
//
// Parsing is tolerant: a body that is not JSON, or has fields of the wrong type,
// never panics. A malformed entry is skipped without losing the well-formed ones
// around it. The slugs themselves are returned exactly as the vendor sent them
// (decoded, in the vendor's order, no deduplication); validating and capping them
// is the caller's job. Only an entry with no slug at all is dropped, since it
// names nothing.
//
// None of the fetchers ever logs or returns the credential, and no vendor text
// other than the decoded slug and display name reaches the caller; transport
// error text is dropped.

// DiscoveredModel is one model a vendor reports for a credential.
type DiscoveredModel struct {
	// Slug is the vendor's model id, exactly as the vendor sent it.
	Slug string
	// DisplayName is the vendor's human-readable name. It is never empty: a vendor
	// that sends none (the OpenAI /v1/models listing never does) gets the Slug.
	DisplayName string
}

// DiscoveryStatus is the verdict of one discovery fetch.
type DiscoveryStatus int

// The zero DiscoveryStatus is deliberately none of these, so a result that was
// never filled in cannot read as a verdict.
const (
	// DiscoveryOK means the vendor served a usable, non-empty model list.
	DiscoveryOK DiscoveryStatus = iota + 1
	// DiscoveryUnverifiable means no usable list could be had. It is NOT a
	// statement about the credential (a 401 lands here too): the caller keeps its
	// existing models.
	DiscoveryUnverifiable
)

// String returns "ok" or "unverifiable" ("unknown" for a value that is neither).
func (s DiscoveryStatus) String() string {
	switch s {
	case DiscoveryOK:
		return "ok"
	case DiscoveryUnverifiable:
		return "unverifiable"
	default:
		return "unknown"
	}
}

const (
	// codexVisibilityList is the visibility value of a model the Codex CLI offers
	// for selection; "hide" and "none" are internal, retired or auto-selected.
	codexVisibilityList = "list"

	// anthropicModelsPageLimit is the largest page /v1/models serves (the default
	// is 20), so the whole catalog arrives in one answer without pagination.
	anthropicModelsPageLimit = "1000"
)

// DiscoverOpenAISubscriptionModels lists the models a ChatGPT (Codex)
// subscription can use, with GET CodexModelsURL?client_version=<clientVersion>,
// the bearer accessToken, the ChatGPT-Account-Id accountID (omitted when empty)
// and the CodexModelsOriginator header. Only entries with visibility "list" AND
// supported_in_api true are returned.
//
// The backend drops every model whose minimal_client_version exceeds
// clientVersion, so a stale version hides new models: the caller passes the
// version it wants advertised. An empty clientVersion falls back to
// CodexModelsClientVersionDefault.
//
// REVERSE-ENGINEERED / LIVE-CONFIRMED.
func DiscoverOpenAISubscriptionModels(ctx context.Context, httpClient *http.Client, accessToken, accountID, clientVersion string) ([]DiscoveredModel, DiscoveryStatus) {
	clientVersion = strings.TrimSpace(clientVersion)
	if clientVersion == "" {
		clientVersion = CodexModelsClientVersionDefault
	}
	headers := map[string]string{
		"Authorization": authBearerPrefix + accessToken,
		"originator":    CodexModelsOriginator,
	}
	if accountID != "" {
		headers["ChatGPT-Account-Id"] = accountID
	}
	body, ok := fetchModelList(ctx, httpClient, modelListSpec{
		url:     CodexModelsURL,
		query:   url.Values{"client_version": {clientVersion}},
		headers: headers,
	})
	if !ok {
		return nil, DiscoveryUnverifiable
	}
	return discoveryResult(parseCodexModels(body))
}

// DiscoverOpenAIAPIKeyModels lists the models an OpenAI API key can see with GET
// OpenAIModelsURL. The listing carries no display name or visibility, so every id
// is returned as both Slug and DisplayName, including non-chat models
// (embeddings, speech, image); narrowing to chat-capable ones is the caller's
// concern.
func DiscoverOpenAIAPIKeyModels(ctx context.Context, httpClient *http.Client, apiKey string) ([]DiscoveredModel, DiscoveryStatus) {
	body, ok := fetchModelList(ctx, httpClient, modelListSpec{
		url:     OpenAIModelsURL,
		headers: map[string]string{"Authorization": authBearerPrefix + apiKey},
	})
	if !ok {
		return nil, DiscoveryUnverifiable
	}
	return discoveryResult(parseDataModels(body, false))
}

// DiscoverAnthropicAPIKeyModels lists the models an Anthropic API key can use
// with GET AnthropicModelsURL, returning each data[].id as the Slug and its
// display_name (falling back to the id when absent) as the DisplayName.
func DiscoverAnthropicAPIKeyModels(ctx context.Context, httpClient *http.Client, apiKey string) ([]DiscoveredModel, DiscoveryStatus) {
	return discoverAnthropic(ctx, httpClient, map[string]string{
		"x-api-key":         apiKey,
		"anthropic-version": AnthropicAPIVersion,
	})
}

// DiscoverAnthropicSubscriptionModels asks the same /v1/models listing with a
// Claude (Pro/Max) OAuth access token: a bearer plus the anthropic-beta OAuth
// opt-in (AnthropicBeta) and anthropic-version headers. Whether a consumer
// bearer is served a model list at all is UNCONFIRMED: when the endpoint does not
// return a usable list (401, 403, 404, an empty or foreign body, anything), the
// answer is an empty list and DiscoveryUnverifiable, never an error, and the
// caller keeps its static seed.
//
// REVERSE-ENGINEERED / VERIFY LIVE.
func DiscoverAnthropicSubscriptionModels(ctx context.Context, httpClient *http.Client, accessToken string) ([]DiscoveredModel, DiscoveryStatus) {
	return discoverAnthropic(ctx, httpClient, map[string]string{
		"Authorization":     authBearerPrefix + accessToken,
		"anthropic-beta":    AnthropicBeta,
		"anthropic-version": AnthropicAPIVersion,
	})
}

// discoverAnthropic is the shared GET AnthropicModelsURL: the two Anthropic
// credential kinds differ only in their auth headers.
func discoverAnthropic(ctx context.Context, httpClient *http.Client, headers map[string]string) ([]DiscoveredModel, DiscoveryStatus) {
	body, ok := fetchModelList(ctx, httpClient, modelListSpec{
		url:     AnthropicModelsURL,
		query:   url.Values{"limit": {anthropicModelsPageLimit}},
		headers: headers,
	})
	if !ok {
		return nil, DiscoveryUnverifiable
	}
	return discoveryResult(parseDataModels(body, true))
}

// modelListSpec describes one discovery GET.
type modelListSpec struct {
	url     string
	query   url.Values
	headers map[string]string
}

// fetchModelList sends the GET and returns the body of a 2xx answer. Any other
// outcome (a non-2xx status, a redirect, a transport failure, a request that
// could not be built) returns false; the error text is not kept because it may
// quote the request, and the credential rides in a header. The caller's
// httpClient owns the timeout and is left untouched; a nil client falls back to
// defaultHTTPClient (30s). Redirects are not followed (see withoutRedirects).
func fetchModelList(ctx context.Context, httpClient *http.Client, s modelListSpec) ([]byte, bool) {
	target := s.url
	if len(s.query) > 0 {
		target = appendQuery(target, s.query)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Accept", "application/json")
	for name, value := range s.headers {
		req.Header.Set(name, value)
	}
	status, body, err := send(withoutRedirects(httpClient), req)
	if err != nil || status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, false
	}
	return body, true
}

// discoveryResult turns parsed models into the public answer: a non-empty list
// is OK, an empty one Unverifiable (see the file comment).
func discoveryResult(models []DiscoveredModel) ([]DiscoveredModel, DiscoveryStatus) {
	if len(models) == 0 {
		return nil, DiscoveryUnverifiable
	}
	return models, DiscoveryOK
}

// newDiscoveredModel builds a model, giving it the slug as display name when the
// vendor sent none.
func newDiscoveredModel(slug, displayName string) DiscoveredModel {
	if displayName == "" {
		displayName = slug
	}
	return DiscoveredModel{Slug: slug, DisplayName: displayName}
}

// parseCodexModels reads {"models":[...]} and keeps the entries that have a slug,
// visibility "list" and supported_in_api true. Anything that does not decode
// leaves the field at its zero value, which fails the filter, so an unreadable
// entry is dropped and a missing flag never counts as true.
func parseCodexModels(body []byte) []DiscoveredModel {
	var env struct {
		Models []json.RawMessage `json:"models"`
	}
	// A decode error (not JSON, a field of the wrong type) is deliberately
	// ignored here and per entry: json.Unmarshal keeps every field it could
	// decode, and anything it could not stays zero and is filtered out below.
	_ = json.Unmarshal(body, &env)

	var out []DiscoveredModel
	for _, raw := range env.Models {
		var m struct {
			Slug           string `json:"slug"`
			DisplayName    string `json:"display_name"`
			Visibility     string `json:"visibility"`
			SupportedInAPI bool   `json:"supported_in_api"`
		}
		_ = json.Unmarshal(raw, &m)
		if m.Slug == "" || m.Visibility != codexVisibilityList || !m.SupportedInAPI {
			continue
		}
		out = append(out, newDiscoveredModel(m.Slug, m.DisplayName))
	}
	return out
}

// parseDataModels reads the OpenAI/Anthropic shape {"data":[{"id":...}]} and keeps
// every entry that has an id. withDisplayName selects whether the entry's
// display_name is carried (Anthropic) or the id is used for both fields (OpenAI,
// which sends no display name).
func parseDataModels(body []byte, withDisplayName bool) []DiscoveredModel {
	var env struct {
		Data []json.RawMessage `json:"data"`
	}
	// Errors are ignored for the reason given in parseCodexModels.
	_ = json.Unmarshal(body, &env)

	var out []DiscoveredModel
	for _, raw := range env.Data {
		var m struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		}
		_ = json.Unmarshal(raw, &m)
		if m.ID == "" {
			continue
		}
		displayName := ""
		if withDisplayName {
			displayName = m.DisplayName
		}
		out = append(out, newDiscoveredModel(m.ID, displayName))
	}
	return out
}
