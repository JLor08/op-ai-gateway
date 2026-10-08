// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/vendorauth"
	"strings"
	"time"
)

// Model discovery: asking the vendor which models an account's credential can
// really use, instead of trusting the static VendorCatalog seed. The seed is a
// guess made when the account is created (and a wrong one for a ChatGPT
// subscription, whose Codex backend serves a newer generation of models than any
// list kept in this repository); discovery replaces it with the vendor's answer.
//
// The fetchers live in internal/vendorauth (one GET per credential kind, never an
// error, a DiscoveryStatus instead). This file is the service around them:
//
//   - RefreshVendorAccountModels opens the account's sealed credential, picks the
//     fetcher for its vendor and auth type, validates and caps what the vendor
//     sent (it becomes model ids and header values), applies the account's
//     prefix and atomically replaces the account's model rows.
//   - It is FAIL-SOFT: a vendor that cannot be asked, answers with an error or
//     lists nothing usable leaves the existing rows exactly as they were and says
//     so in the RefreshResult. Discovery never wipes a working catalog.
//   - The connect flows (token import, code paste, device code) run it best
//     effort once the tokens are stored, so a freshly connected subscription
//     account serves its real models at once; a failing discovery never fails
//     the connect.
//   - A prefix change re-labels the existing rows (relabelVendorAccountModels)
//     without asking the vendor again.
//
// No credential ever reaches a RefreshResult, an error or a log line from here.

const (
	// vendorDiscoveryHTTPTimeout bounds one discovery fetch. Discovery runs only
	// at connect time and on the explicit refresh action, never on a request path.
	vendorDiscoveryHTTPTimeout = 10 * time.Second

	// maxDiscoveredModelIDLen and maxDiscoveredDisplayNameLen cap the two strings
	// the vendor supplies for each model. Real ids are a few dozen characters.
	maxDiscoveredModelIDLen     = 128
	maxDiscoveredDisplayNameLen = 128
	// maxDiscoveredModels caps how many models one discovery stores. The largest
	// real listings are in the low hundreds; the cap bounds what a hostile or
	// runaway answer can make the portal persist.
	maxDiscoveredModels = 500
)

// The wire values of RefreshResult.Status.
const (
	// VendorRefreshOK: the vendor served a usable list and it replaced the
	// account's models.
	VendorRefreshOK = "ok"
	// VendorRefreshUnverifiable: no usable list could be had; the account's models
	// are unchanged. It is no statement about the credential.
	VendorRefreshUnverifiable = "unverifiable"
)

// RefreshResult is the credential-free outcome of RefreshVendorAccountModels.
// Status is VendorRefreshOK or VendorRefreshUnverifiable. Discovered is the number
// of models now served from the discovery (0 when unverifiable). Detail is a short
// human-readable phrase (what was stored, or why nothing was) and never contains
// a credential or vendor text.
type RefreshResult struct {
	Status     string `json:"status"`
	Discovered int    `json:"discovered"`
	Detail     string `json:"detail"`
}

// VendorCredentialDiscoverer is one model-list fetch made with a single credential
// (an api key or an OAuth access token) and a bounded client. The
// vendorauth.Discover* functions of the api-key and Anthropic kinds have exactly
// this shape.
type VendorCredentialDiscoverer func(ctx context.Context, httpClient *http.Client, credential string) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus)

// VendorOpenAISubscriptionDiscoverer is the ChatGPT-subscription fetch, which also
// needs the ChatGPT account id and the Codex client_version to advertise
// (vendorauth.DiscoverOpenAISubscriptionModels has exactly this shape).
type VendorOpenAISubscriptionDiscoverer func(ctx context.Context, httpClient *http.Client, accessToken, accountID, clientVersion string) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus)

// VendorModelDiscoverers is the seam over the four vendorauth discovery fetchers:
// tests inject fakes so no service test reaches a vendor over the network. A nil
// field means the real fetcher (see ServiceDeps.VendorDiscoverers).
type VendorModelDiscoverers struct {
	OpenAISubscription    VendorOpenAISubscriptionDiscoverer
	AnthropicSubscription VendorCredentialDiscoverer
	OpenAIAPIKey          VendorCredentialDiscoverer
	AnthropicAPIKey       VendorCredentialDiscoverer
}

// withDefaults returns d with every nil fetcher replaced by its vendorauth function.
func (d VendorModelDiscoverers) withDefaults() VendorModelDiscoverers {
	if d.OpenAISubscription == nil {
		d.OpenAISubscription = vendorauth.DiscoverOpenAISubscriptionModels
	}
	if d.AnthropicSubscription == nil {
		d.AnthropicSubscription = vendorauth.DiscoverAnthropicSubscriptionModels
	}
	if d.OpenAIAPIKey == nil {
		d.OpenAIAPIKey = vendorauth.DiscoverOpenAIAPIKeyModels
	}
	if d.AnthropicAPIKey == nil {
		d.AnthropicAPIKey = vendorauth.DiscoverAnthropicAPIKeyModels
	}
	return d
}

// vendorDiscoveryState is the model discovery's configuration: the fetchers and
// the bounded http client they share. Held as a single field on Service
// (Service.vendorDiscovery).
type vendorDiscoveryState struct {
	discoverers VendorModelDiscoverers
	client      *http.Client
}

func newVendorDiscoveryState(discoverers VendorModelDiscoverers) vendorDiscoveryState {
	return vendorDiscoveryState{
		discoverers: discoverers.withDefaults(),
		client:      &http.Client{Timeout: vendorDiscoveryHTTPTimeout},
	}
}

// RefreshVendorAccountModels asks the vendor which models the account's stored
// credential can use and, when it answers with a usable list, replaces the
// account's model rows with it: each model is served as the account's prefix plus
// the vendor's slug, the slug stays the id sent upstream and the vendor's display
// name is kept. It returns the credential-free account view and the outcome.
//
// FAIL-SOFT: when nothing usable can be had (the credential is missing or an
// expired token cannot be refreshed here, the vendor is unreachable, it answers
// with an error, a body that is not a catalog, or a list with no usable model) the
// rows are left exactly as they were, the result is VendorRefreshUnverifiable and
// the error is nil. Only a stored credential that cannot be opened
// (ErrVendorAccountCredentialUnreadable) or a store failure is an error.
//
// What the vendor sent is untrusted: a slug that is not a plain model id (empty,
// over maxDiscoveredModelIDLen, outside [A-Za-z0-9._~:/@+-], not starting with a
// letter or digit, or containing "..") drops its row, a repeated slug is stored
// once, a display name that is not printable ASCII is stored empty and an
// over-long one is cut, and at most maxDiscoveredModels rows are stored. An OpenAI
// api-key listing is narrowed to chat-capable models first (isChatCapableOpenAIModel).
//
// STRICTLY OWNER-ONLY, system scope included: it opens the owner's sealed
// credential and sends it to the vendor from the gateway, so it is authorized like
// a write (an unknown id and a stranger's account are both
// ErrVendorAccountNotFound). ErrVendorAccountsDisabled while the master flag is off.
func (s *Service) RefreshVendorAccountModels(ctx context.Context, principal auth.Token, id string) (VendorAccountDTO, RefreshResult, error) {
	if err := s.requireVendorAccountsEnabled(ctx); err != nil {
		return VendorAccountDTO{}, RefreshResult{}, err
	}
	acc, err := s.authorizeVendorAccount(ctx, principal, id, true)
	if err != nil {
		return VendorAccountDTO{}, RefreshResult{}, err
	}
	found, note, err := s.discoverVendorModels(ctx, acc)
	if err != nil {
		return VendorAccountDTO{}, RefreshResult{}, err
	}
	if note != "" {
		return s.keptVendorModels(ctx, acc, note)
	}
	rows, dropped := vendorModelRows(acc, found)
	if len(rows) == 0 {
		return s.keptVendorModels(ctx, acc, "the vendor listed no usable model")
	}
	// The vendor call is a network round trip; re-load the account so a prefix
	// change (or a rename) made meanwhile is not overwritten, and so an account
	// deleted meanwhile is reported rather than silently written to.
	acc, err = s.routes.VendorAccountByID(ctx, acc.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return VendorAccountDTO{}, RefreshResult{}, ErrVendorAccountNotFound
		}
		return VendorAccountDTO{}, RefreshResult{}, err
	}
	rows, _ = relabelVendorModels(rows, acc.ModelPrefix)
	if err := s.routes.SetVendorAccountModels(ctx, acc.ID, rows); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return VendorAccountDTO{}, RefreshResult{}, ErrVendorAccountNotFound
		}
		return VendorAccountDTO{}, RefreshResult{}, err
	}
	dto, err := s.vendorAccountDTO(ctx, acc)
	if err != nil {
		return VendorAccountDTO{}, RefreshResult{}, err
	}
	detail := fmt.Sprintf("discovered %d models", len(rows))
	if dropped > 0 {
		detail += fmt.Sprintf("; %d unusable entries were dropped", dropped)
	}
	return dto, RefreshResult{Status: VendorRefreshOK, Discovered: len(rows), Detail: detail}, nil
}

// keptVendorModels is the fail-soft answer: acc's current models, untouched, with
// the unverifiable result and why.
func (s *Service) keptVendorModels(ctx context.Context, acc routing.VendorAccount, why string) (VendorAccountDTO, RefreshResult, error) {
	dto, err := s.vendorAccountDTO(ctx, acc)
	if err != nil {
		return VendorAccountDTO{}, RefreshResult{}, err
	}
	return dto, RefreshResult{Status: VendorRefreshUnverifiable, Detail: why + "; the current models were kept"}, nil
}

// discoverVendorModels opens acc's credential and runs the fetcher for its vendor
// and auth type. A non-empty note means no usable list was had (why, in words that
// carry no credential) and the models are nil; an error is only a credential that
// cannot be opened.
func (s *Service) discoverVendorModels(ctx context.Context, acc routing.VendorAccount) (models []vendorauth.DiscoveredModel, note string, err error) {
	var status vendorauth.DiscoveryStatus
	switch acc.AuthType {
	case routing.VendorAuthAPIKey:
		apiKey, err := s.openVendorAPIKey(acc)
		if err != nil {
			return nil, "", err
		}
		if apiKey == "" {
			return nil, "no API key is set", nil
		}
		fetch := s.apiKeyDiscoverer(acc.Vendor)
		if fetch == nil {
			return nil, noVendorDiscoveryNote, nil
		}
		models, status = fetch(ctx, s.vendorDiscovery.client, apiKey)
	case routing.VendorAuthSubscription:
		ts, err := s.openVendorTokenSet(acc)
		if err != nil {
			return nil, "", err
		}
		if ts.AccessToken == "" {
			return nil, "the subscription is not connected", nil
		}
		// The dispatch refreshes an expired token lazily and persists the result;
		// the portal must not race it for the single-use refresh token. Asking the
		// vendor with a token known to be expired only earns a 401, so say so.
		if ts.RefreshToken != "" && ts.NeedsRefresh(s.clock(), 0) {
			return nil, "the access token has expired; it is refreshed automatically on the next request, then refresh the models again", nil
		}
		switch acc.Vendor {
		case routing.VendorOpenAI:
			models, status = s.vendorDiscovery.discoverers.OpenAISubscription(ctx, s.vendorDiscovery.client, ts.AccessToken, chatGPTAccountID(ts), s.VendorOpenAICodexClientVersion(ctx))
		case routing.VendorAnthropic:
			models, status = s.vendorDiscovery.discoverers.AnthropicSubscription(ctx, s.vendorDiscovery.client, ts.AccessToken)
		default:
			return nil, noVendorDiscoveryNote, nil
		}
	default:
		return nil, noVendorDiscoveryNote, nil
	}
	if status != vendorauth.DiscoveryOK {
		return nil, "the vendor did not return a usable model list", nil
	}
	return models, "", nil
}

// noVendorDiscoveryNote is the note for an account whose vendor or auth type has
// no discovery.
const noVendorDiscoveryNote = "model discovery is not available for this account type"

// apiKeyDiscoverer returns the api-key fetcher for vendor, or nil for a vendor
// with none.
func (s *Service) apiKeyDiscoverer(vendor string) VendorCredentialDiscoverer {
	switch vendor {
	case routing.VendorOpenAI:
		return s.vendorDiscovery.discoverers.OpenAIAPIKey
	case routing.VendorAnthropic:
		return s.vendorDiscovery.discoverers.AnthropicAPIKey
	}
	return nil
}

// chatGPTAccountID is the ChatGPT account id sent as the ChatGPT-Account-Id header:
// the one stored with the token set or, when that is empty, the claim baked into
// the access-token JWT (the same fallback the dispatch makes). A value that is not
// header-safe (vendorIdentityValue) is dropped, and discovery then runs without it.
func chatGPTAccountID(ts vendorauth.TokenSet) string {
	id := ts.AccountID
	if id == "" {
		id, _ = vendorauth.OpenAIClaimsFromJWT(ts.AccessToken)
	}
	id, _ = vendorIdentityValue(id)
	return id
}

// vendorModelRows turns a vendor's discovered models into the rows an account
// serves: the vendor's flavor, UpstreamModel = the slug, the validated display
// name, and the prefix NOT yet applied (GatewayModel = the slug; the caller
// applies the account's current prefix). An OpenAI api-key listing is narrowed to
// chat-capable models. Invalid and repeated slugs and anything beyond
// maxDiscoveredModels are dropped; dropped counts every entry that did not become
// a row.
func vendorModelRows(acc routing.VendorAccount, found []vendorauth.DiscoveredModel) (rows []routing.VendorAccountModel, dropped int) {
	flavor, _ := vendorAPIFlavor(acc.Vendor)
	chatOnly := acc.Vendor == routing.VendorOpenAI && acc.AuthType == routing.VendorAuthAPIKey
	seen := make(map[string]struct{}, len(found))
	rows = make([]routing.VendorAccountModel, 0, len(found))
	for _, m := range found {
		slug, ok := vendorModelSlug(m.Slug)
		if !ok || (chatOnly && !isChatCapableOpenAIModel(slug)) || len(rows) >= maxDiscoveredModels {
			continue
		}
		if _, dup := seen[slug]; dup {
			continue
		}
		seen[slug] = struct{}{}
		rows = append(rows, routing.VendorAccountModel{
			GatewayModel:  slug,
			UpstreamModel: slug,
			APIFlavor:     flavor,
			DisplayName:   vendorModelDisplayName(m.DisplayName),
		})
	}
	return rows, len(found) - len(rows)
}

// isModelIDByte reports whether c may appear in a model id the gateway serves or
// in a model prefix: ASCII letters and digits plus "-", "_", ".", "~", ":", "/",
// "@" and "+". Model ids travel in URL paths (/v1/models/{id}), JSON bodies and
// logs, so this is the URL-path-safe subset of printable ASCII: no space, none of
// the characters that start a query, fragment or escape ("?", "#", "%") and
// no quote, backslash or angle bracket. "/" and ":" stay because real ids and
// prefixes use them ("ft:gpt-4o:org::id", "chatgpt/").
func isModelIDByte(c byte) bool {
	if isASCIIAlnum(c) {
		return true
	}
	switch c {
	case '-', '_', '.', '~', ':', '/', '@', '+':
		return true
	}
	return false
}

func isASCIIAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// vendorModelSlug returns raw when it is a plain model id: 1..maxDiscoveredModelIDLen
// bytes of isModelIDByte characters that starts with a letter or digit and holds
// no "..". The slug is used as sent, with no trimming or case change, because it
// is the id the vendor expects back.
func vendorModelSlug(raw string) (string, bool) {
	if raw == "" || len(raw) > maxDiscoveredModelIDLen || !isASCIIAlnum(raw[0]) || strings.Contains(raw, "..") {
		return "", false
	}
	for i := 1; i < len(raw); i++ {
		if !isModelIDByte(raw[i]) {
			return "", false
		}
	}
	return raw, true
}

// vendorModelDisplayName returns the vendor's display name trimmed, cut to
// maxDiscoveredDisplayNameLen and stored as "" when it holds anything but
// printable ASCII (a control character, a newline, non-ASCII text): an empty name
// only makes the UI fall back to the model id, whereas a hostile one would be
// rendered.
func vendorModelDisplayName(raw string) string {
	name := strings.TrimSpace(raw)
	for i := 0; i < len(name); i++ {
		if name[i] < printableASCIIMin || name[i] > printableASCIIMax {
			return ""
		}
	}
	if len(name) > maxDiscoveredDisplayNameLen {
		name = strings.TrimSpace(name[:maxDiscoveredDisplayNameLen])
	}
	return name
}

// openAINonChatMarkers are the substrings that mark an OpenAI model id as one the
// gateway's chat paths cannot serve, whatever else it looks like: embeddings,
// speech in and out, image generation, moderation, the realtime API and
// completion-only models.
var openAINonChatMarkers = []string{
	"embedding", "whisper", "tts", "transcribe", "dall-e", "gpt-image", "moderation", "realtime", "instruct",
}

// isChatCapableOpenAIModel is the small heuristic that narrows OpenAI's
// /v1/models listing, which carries no capability data and names every model the
// key can reach, to the models a chat or responses request can use: the gpt-*,
// chatgpt-* and o-series (o1, o3, o4-mini, ...) families, and fine-tunes of them
// ("ft:gpt-4o-mini-...:org::id"), minus the non-chat variants listed in
// openAINonChatMarkers. Anything it does not recognise is left out: a model
// wrongly missing is added by a newer rule, a non-chat one wrongly offered
// fails every request routed to it.
func isChatCapableOpenAIModel(id string) bool {
	base, _, _ := strings.Cut(strings.TrimPrefix(id, "ft:"), ":")
	for _, marker := range openAINonChatMarkers {
		if strings.Contains(base, marker) {
			return false
		}
	}
	if strings.HasPrefix(base, "gpt-") || strings.HasPrefix(base, "chatgpt-") {
		return true
	}
	return len(base) >= 2 && base[0] == 'o' && base[1] >= '0' && base[1] <= '9'
}

// relabelVendorModels returns rows with GatewayModel = prefix + UpstreamModel and
// whether that differs from rows. UpstreamModel, flavor and display name are
// untouched. Rows that would collide on the same gateway id keep only the first.
func relabelVendorModels(rows []routing.VendorAccountModel, prefix string) (relabeled []routing.VendorAccountModel, changed bool) {
	relabeled = make([]routing.VendorAccountModel, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		gateway := prefix + row.UpstreamModel
		if _, dup := seen[gateway]; dup {
			changed = true
			continue
		}
		seen[gateway] = struct{}{}
		if row.GatewayModel != gateway {
			changed = true
			row.GatewayModel = gateway
		}
		relabeled = append(relabeled, row)
	}
	return relabeled, changed
}

// relabelVendorAccountModels re-labels acc's stored model rows to acc's current
// model prefix, without asking the vendor. It writes only when some row would
// change, so re-sending the prefix an account already has costs a read.
func (s *Service) relabelVendorAccountModels(ctx context.Context, acc routing.VendorAccount) error {
	rows, err := s.routes.VendorAccountModels(ctx, acc.ID)
	if err != nil {
		return err
	}
	relabeled, changed := relabelVendorModels(rows, acc.ModelPrefix)
	if !changed {
		return nil
	}
	return s.routes.SetVendorAccountModels(ctx, acc.ID, relabeled)
}

// discoverAfterConnect runs a model discovery for the subscription account dto
// that has just been connected, so it serves its real models at once rather than
// the static seed. It is BEST EFFORT: whatever goes wrong is logged (token-free)
// and dto is returned as it was, because the connect itself has succeeded and
// must be reported as such. On success the refreshed view (the discovered models)
// is returned in its place.
func (s *Service) discoverAfterConnect(ctx context.Context, principal auth.Token, dto VendorAccountDTO) VendorAccountDTO {
	refreshed, result, err := s.RefreshVendorAccountModels(ctx, principal, dto.ID)
	if err != nil {
		slog.Warn("vendor model discovery after connect failed; the account keeps its current models", "account", dto.ID, "err", err)
		return dto
	}
	slog.Info("vendor model discovery after connect", "account", dto.ID, "status", result.Status, "discovered", result.Discovered)
	return refreshed
}
