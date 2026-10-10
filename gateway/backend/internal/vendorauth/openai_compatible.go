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
