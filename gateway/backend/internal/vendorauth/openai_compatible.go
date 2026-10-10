// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"net/http"
)

// ValidateOpenAICompatibleAPIKey checks an api key against an OpenAI-compatible
// provider's probe URL (GET probeURL, Authorization: Bearer). probeURL is composed
// by the caller (the portal) from the provider-preset registry — this package does
// not know vendor ids or roots. Classification is the shared uniform rule
// (runProbe/classify): 2xx valid, 401 invalid, everything else unverifiable.
func ValidateOpenAICompatibleAPIKey(ctx context.Context, httpClient *http.Client, probeURL, apiKey string) CredentialCheck {
	check, _ := runProbe(ctx, httpClient, probeSpec{
		url:     probeURL,
		headers: map[string]string{"Authorization": authBearerPrefix + apiKey},
		secret:  apiKey,
	})
	return check
}

// DiscoverOpenAICompatibleModels lists the models an api key can use with GET
// modelsURL (Authorization: Bearer). modelsURL is composed by the caller (the
// portal, from the provider-preset registry): this package does not know vendor
// ids or roots. The answer is the shared discovery contract (fetchModelList /
// discoveryResult): a 2xx listing with at least one usable entry is OK, anything
// else (a non-2xx status including 401, a redirect, a transport failure, a body
// that is not a catalog, an empty list) is Unverifiable, never an error. Each
// entry's id is the Slug and its display_name, or failing that its name (OpenRouter
// sends name), the DisplayName; the id stands in when neither is sent. The slugs
// are returned exactly as the provider sent them (a Gemini id still carries its
// "models/" prefix): validating, stripping and capping them is the caller's job.
// The api key is only ever sent, never returned.
func DiscoverOpenAICompatibleModels(ctx context.Context, httpClient *http.Client, modelsURL, apiKey string) ([]DiscoveredModel, DiscoveryStatus) {
	body, ok := fetchModelList(ctx, httpClient, modelListSpec{
		url:     modelsURL,
		headers: map[string]string{"Authorization": authBearerPrefix + apiKey},
	})
	if !ok {
		return nil, DiscoveryUnverifiable
	}
	return discoveryResult(parseDataModels(body, true))
}
