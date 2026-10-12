// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// This file holds the Google Gemini (native generateContent API) counterparts of
// the OpenAI-compatible fetchers in openai_compatible.go. Same contract, same
// caller-composed URL (the portal builds it from the provider-preset registry:
// this package knows no vendor ids or roots), but the credential travels in the
// x-goog-api-key header instead of Authorization: Bearer, and the model listing
// has its own shape and paging.

const (
	// geminiAPIKeyHeader is the header the Gemini API reads the api key from.
	geminiAPIKeyHeader = "x-goog-api-key"

	// geminiModelsPageSize is the largest page models.list serves (the default is
	// 50), so the whole catalog normally arrives in one or two answers.
	geminiModelsPageSize = "1000"

	// maxGeminiModelPages bounds how many pages one discovery follows. A real
	// catalog is a few dozen models; the cap only stops a runaway or hostile
	// listing that keeps answering a nextPageToken.
	maxGeminiModelPages = 20

	// geminiGenerateMethod is the supportedGenerationMethods entry of a model the
	// gateway can chat with; embedding, tuning and other models lack it.
	geminiGenerateMethod = "generateContent"

	// geminiModelsPrefix is the resource-name prefix of a model id in the listing
	// ("models/gemini-2.5-pro"); the id used in a request path is the bare one.
	geminiModelsPrefix = "models/"
)

// ValidateGeminiAPIKey checks an api key against the Gemini API's probe URL (GET
// probeURL, x-goog-api-key). probeURL is composed by the caller (the portal) from
// the provider-preset registry. Classification is the shared uniform rule
// (runProbe/classify): 2xx valid, 401 invalid, everything else unverifiable. A bad
// key may be answered with 400 rather than 401, which then reads "could not
// verify", never "invalid".
func ValidateGeminiAPIKey(ctx context.Context, httpClient *http.Client, probeURL, apiKey string) CredentialCheck {
	check, _ := runProbe(ctx, httpClient, probeSpec{
		url:     probeURL,
		headers: map[string]string{geminiAPIKeyHeader: apiKey},
		secret:  apiKey,
	})
	return check
}

// DiscoverGeminiModels lists the models an api key can chat with, with the paged
// GET modelsURL?pageSize=1000 (x-goog-api-key), following nextPageToken for at most
// maxGeminiModelPages pages. modelsURL is composed by the caller (the portal, from
// the provider-preset registry). Only entries whose supportedGenerationMethods
// contain generateContent are kept (the listing also carries embedding and other
// models), and the "models/" resource prefix is removed from each id, and from the
// display name when that is only the id standing in.
//
// The answer is the shared discovery contract (fetchModelList / discoveryResult): a
// complete listing with at least one usable entry is OK; anything else is
// Unverifiable, never an error. That includes a page that cannot be had (a
// non-2xx status including 401, a redirect, a transport failure) and a listing
// that is still paging after the cap: an incomplete list must not stand in for the
// catalog and silently drop models, so none is returned. The api key is only ever
// sent, never returned.
func DiscoverGeminiModels(ctx context.Context, httpClient *http.Client, modelsURL, apiKey string) ([]DiscoveredModel, DiscoveryStatus) {
	headers := map[string]string{geminiAPIKeyHeader: apiKey}
	var models []DiscoveredModel
	pageToken := ""
	for range maxGeminiModelPages {
		query := url.Values{"pageSize": {geminiModelsPageSize}}
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}
		body, ok := fetchModelList(ctx, httpClient, modelListSpec{url: modelsURL, query: query, headers: headers})
		if !ok {
			return nil, DiscoveryUnverifiable
		}
		page, next := parseGeminiModels(body)
		models = append(models, page...)
		if next == "" {
			return discoveryResult(models)
		}
		pageToken = next
	}
	return nil, DiscoveryUnverifiable
}

// parseGeminiModels reads one page of {"models":[...],"nextPageToken":...} and
// keeps the entries that have a name (beyond the "models/" prefix) and support
// generateContent, with the prefix removed from the slug. It returns the page's nextPageToken ("" on
// the last page). Parsing is tolerant like the other parsers (see parseCodexModels):
// an entry that does not decode fails the filter and is dropped.
func parseGeminiModels(body []byte) (models []DiscoveredModel, nextPageToken string) {
	var env struct {
		Models        []json.RawMessage `json:"models"`
		NextPageToken string            `json:"nextPageToken"`
	}
	_ = json.Unmarshal(body, &env)

	for _, raw := range env.Models {
		var m struct {
			Name        string   `json:"name"`
			DisplayName string   `json:"displayName"`
			Methods     []string `json:"supportedGenerationMethods"`
		}
		_ = json.Unmarshal(raw, &m)
		slug := strings.TrimPrefix(m.Name, geminiModelsPrefix)
		if slug == "" || !slices.Contains(m.Methods, geminiGenerateMethod) {
			continue
		}
		displayName := m.DisplayName
		if displayName == m.Name {
			displayName = slug
		}
		models = append(models, newDiscoveredModel(slug, displayName))
	}
	return models, env.NextPageToken
}
