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
//   - An OpenAI subscription's refresh also pulls the account's usage snapshot
//     (refreshVendorUsage), best effort and after the models, with the same opened
//     and renewed token set; it never changes the refresh's outcome.
//   - A prefix change re-labels the existing rows (relabelVendorAccountModels)
//     without asking the vendor again.
//   - An access token already past its expiry is renewed through the injected
//     VendorTokenRefresher (the gateway's locked refresh, never the portal's own)
//     before the vendor is asked; with no refresher, or a failing one, the
//     discovery is simply unverifiable.
//   - The two writers of an account's model rows (the discovery's replace and the
//     prefix re-label) run under a per-account lock (accountLocks), so neither
//     undoes the other.
//
// No credential ever reaches a RefreshResult, an error or a log line from here.

const (
	// vendorDiscoveryHTTPTimeout bounds one discovery fetch. Discovery runs only
	// at connect time and on the explicit refresh action, never on a request path.
	vendorDiscoveryHTTPTimeout = 10 * time.Second

	// vendorConnectDiscoveryTimeout bounds the WHOLE best-effort discovery a
	// connect runs after the tokens are stored (token refresh, fetch and write): a
	// vendor that hangs then adds at most this to the connect, and the discovery
	// degrades to its fail-soft outcome (the account keeps its seeded models).
	// Shorter than vendorDiscoveryHTTPTimeout on purpose: the explicit refresh
	// action may wait the full client timeout, a connect response should not.
	vendorConnectDiscoveryTimeout = 5 * time.Second

	// vendorTokenRefreshTimeout bounds a token refresh the discovery asks for. The
	// refresh runs on a context that its caller's cancellation cannot reach (see
	// runVendorTokenRefresh), so it needs a bound of its own: a little over the
	// 30s the vendor token calls themselves may take (vendorauth's default client).
	vendorTokenRefreshTimeout = 45 * time.Second

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

// VendorOpenAIUsageFetcher is the ChatGPT-subscription usage fetch the refresh runs
// alongside the model discovery: it needs the ChatGPT account id as well as the
// access token, and answers the five-hour / weekly windows and the credit balance
// (vendorauth.FetchOpenAISubscriptionUsage has exactly this shape).
type VendorOpenAIUsageFetcher func(ctx context.Context, httpClient *http.Client, accessToken, accountID string) (vendorauth.OpenAISubscriptionUsage, vendorauth.DiscoveryStatus)

// VendorModelDiscoverers is the seam over the vendorauth fetchers a models refresh
// runs: the four model-list fetchers and, riding along with the OpenAI
// subscription's refresh, its usage fetch. Tests inject fakes so no service test
// reaches a vendor over the network. A nil field means the real fetcher (see
// ServiceDeps.VendorDiscoverers).
type VendorModelDiscoverers struct {
	OpenAISubscription    VendorOpenAISubscriptionDiscoverer
	AnthropicSubscription VendorCredentialDiscoverer
	OpenAIAPIKey          VendorCredentialDiscoverer
	AnthropicAPIKey       VendorCredentialDiscoverer
	// OpenAIUsage is the usage fetch refreshVendorUsage runs for an OpenAI
	// subscription account after its models were refreshed.
	OpenAIUsage VendorOpenAIUsageFetcher
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
	if d.OpenAIUsage == nil {
		d.OpenAIUsage = vendorauth.FetchOpenAISubscriptionUsage
	}
	return d
}

// VendorTokenRefresher renews the expired OAuth access token of the subscription
// account accountID and PERSISTS the renewed token set, so that re-reading the
// account yields it. It is the gateway's own locked refresh (the one the dispatch
// uses): the portal must not refresh by itself, because a rotating refresh token
// is single-use and two parties refreshing at once would burn it. A nil error says
// the refresh ran, not that the token is now fresh: the caller re-reads the
// account and checks. The error must carry no credential.
type VendorTokenRefresher func(ctx context.Context, accountID string) error

// vendorDiscoveryState is the model discovery's configuration: the fetchers and
// the bounded http client they share, the optional token refresher and the bound
// on connect-time discovery. Held as a single field on Service
// (Service.vendorDiscovery).
type vendorDiscoveryState struct {
	discoverers VendorModelDiscoverers
	client      *http.Client
	// tokenRefresher is nil when none is wired (fail-soft: an expired token is
	// reported unverifiable). Set through ServiceDeps.VendorTokenRefresher or
	// Service.SetVendorTokenRefresher.
	tokenRefresher VendorTokenRefresher
	// connectTimeout bounds discoverAfterConnect (vendorConnectDiscoveryTimeout);
	// a zero value means that default.
	connectTimeout time.Duration
}

func newVendorDiscoveryState(discoverers VendorModelDiscoverers, refresher VendorTokenRefresher) vendorDiscoveryState {
	return vendorDiscoveryState{
		discoverers:    discoverers.withDefaults(),
		client:         &http.Client{Timeout: vendorDiscoveryHTTPTimeout},
		tokenRefresher: refresher,
		connectTimeout: vendorConnectDiscoveryTimeout,
	}
}

// RefreshVendorAccountModels asks the vendor which models the account's stored
// credential can use and, when it answers with a usable list, replaces the
// account's model rows with it: each model is served as the account's prefix plus
// the vendor's slug, the slug stays the id sent upstream and the vendor's display
// name is kept. It returns the credential-free account view and the outcome.
//
// A subscription token that is already past its expiry is renewed first, through
// the VendorTokenRefresher (the gateway's locked refresh), and the account is
// re-read so the fresh token is the one sent; the portal never refreshes by itself.
//
// FAIL-SOFT: when nothing usable can be had (the credential is missing, an expired
// token cannot be renewed (no refresher wired, or it failed), the vendor is
// unreachable, it answers with an error, a body that is not a catalog, or a list
// with no usable model) the rows are left exactly as they were, the result is
// VendorRefreshUnverifiable and the error is nil. Only a stored credential that
// cannot be opened (ErrVendorAccountCredentialUnreadable) or a store failure is an
// error.
//
// What the vendor sent is untrusted: a slug that is not a plain model id (empty,
// over maxDiscoveredModelIDLen, outside [A-Za-z0-9._~:/@+-], not starting with a
// letter or digit, or containing "..") drops its row, a repeated slug is stored
// once, a display name that is not printable ASCII is stored empty and an
// over-long one is cut, and at most maxDiscoveredModels rows are stored. An OpenAI
// api-key listing is narrowed to chat-capable models first (isChatCapableOpenAIModel).
//
// An OpenAI subscription account also gets its usage snapshot refreshed on the way
// (refreshVendorUsage), with the very token set the model discovery opened and
// renewed. That is purely additive and best effort: it never changes the result or
// the error, never marks the account needs_reconnect, and any failure leaves the
// stored snapshot as it was.
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
	found, tokens, note, err := s.discoverVendorModels(ctx, acc)
	if err != nil {
		return VendorAccountDTO{}, RefreshResult{}, err
	}
	dto, result, err := s.applyVendorModelDiscovery(ctx, acc, found, note)
	if err != nil {
		return VendorAccountDTO{}, RefreshResult{}, err
	}
	// Last, and only for a refresh that succeeded (kept or replaced): a slow usage
	// fetch can then never starve the model write of the connect-time bound, and a
	// vendor that fails it changes nothing above.
	s.refreshVendorUsage(ctx, acc, tokens)
	return dto, result, nil
}

// applyVendorModelDiscovery turns discoverVendorModels' answer (found models, or a
// note saying why none) into the refresh's outcome: the fail-soft kept-models answer,
// or the rows replaced by what the vendor listed.
func (s *Service) applyVendorModelDiscovery(ctx context.Context, acc routing.VendorAccount, found []vendorauth.DiscoveredModel, note string) (VendorAccountDTO, RefreshResult, error) {
	if note != "" {
		return s.keptVendorModels(ctx, acc.ID, note)
	}
	rows, dropped := vendorModelRows(acc, found)
	if len(rows) == 0 {
		return s.keptVendorModels(ctx, acc.ID, "the vendor listed no usable model")
	}
	acc, rows, err := s.storeDiscoveredVendorModels(ctx, acc.ID, rows)
	if err != nil {
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

// storeDiscoveredVendorModels replaces the account's model rows with rows (the
// prefix not yet applied), under the account's model-write lock. The vendor call
// that produced rows was a network round trip, so the account is re-loaded INSIDE
// the lock: a prefix change made meanwhile is not overwritten (the rows are written
// under the prefix current now, and a re-label that arrives later re-labels these
// rows), and an account deleted meanwhile is reported rather than written to. It
// returns the re-loaded account and the rows as stored.
func (s *Service) storeDiscoveredVendorModels(ctx context.Context, id string, rows []routing.VendorAccountModel) (routing.VendorAccount, []routing.VendorAccountModel, error) {
	unlock, err := s.vendorModelWrites.lock(ctx, id)
	if err != nil {
		return routing.VendorAccount{}, nil, err
	}
	defer unlock()
	acc, err := s.routes.VendorAccountByID(ctx, id)
	if err != nil {
		return routing.VendorAccount{}, nil, notFoundAsVendorAccountNotFound(err)
	}
	rows, _ = relabelVendorModels(rows, acc.ModelPrefix)
	if err := s.routes.SetVendorAccountModels(ctx, acc.ID, rows); err != nil {
		return routing.VendorAccount{}, nil, notFoundAsVendorAccountNotFound(err)
	}
	return acc, rows, nil
}

// notFoundAsVendorAccountNotFound maps the store's not-found to
// ErrVendorAccountNotFound and returns every other error as it is.
func notFoundAsVendorAccountNotFound(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return ErrVendorAccountNotFound
	}
	return err
}

// keptVendorModels is the fail-soft answer: the account's current models,
// untouched, with the unverifiable result and why. The account is RE-LOADED, not
// taken from the caller: the discovery may have changed it since it was loaded (a
// refresh token the vendor rejected flips the account to needs_reconnect), and the
// answer must report the status it has now. An account deleted meanwhile is
// ErrVendorAccountNotFound.
func (s *Service) keptVendorModels(ctx context.Context, id, why string) (VendorAccountDTO, RefreshResult, error) {
	acc, err := s.routes.VendorAccountByID(ctx, id)
	if err != nil {
		return VendorAccountDTO{}, RefreshResult{}, notFoundAsVendorAccountNotFound(err)
	}
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
//
// tokens is the subscription account's opened, current (renewed when it was
// expired) token set, handed on so the usage refresh does not open or renew it a
// second time. It is the zero value for an api_key account and whenever no usable
// token set could be had, and it stays set when the vendor merely failed to list
// models. It lives only in memory: it never reaches a DTO, an error or a log.
func (s *Service) discoverVendorModels(ctx context.Context, acc routing.VendorAccount) (models []vendorauth.DiscoveredModel, tokens vendorauth.TokenSet, note string, err error) {
	var status vendorauth.DiscoveryStatus
	switch acc.AuthType {
	case routing.VendorAuthAPIKey:
		models, status, note, err = s.discoverAPIKeyModels(ctx, acc)
	case routing.VendorAuthSubscription:
		if tokens, note, err = s.currentSubscriptionTokens(ctx, acc); err == nil && note == "" {
			models, status, note = s.discoverSubscriptionModels(ctx, acc.Vendor, tokens)
		}
	default:
		return nil, vendorauth.TokenSet{}, noVendorDiscoveryNote, nil
	}
	if err != nil || note != "" {
		return nil, tokens, note, err
	}
	if status != vendorauth.DiscoveryOK {
		return nil, tokens, "the vendor did not return a usable model list", nil
	}
	return models, tokens, "", nil
}

// discoverAPIKeyModels runs the model-list fetcher for an api_key account. A
// non-empty note (credential-free) or an error short-circuits; otherwise status
// is the fetcher's verdict.
func (s *Service) discoverAPIKeyModels(ctx context.Context, acc routing.VendorAccount) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus, string, error) {
	apiKey, err := s.openVendorAPIKey(acc)
	if err != nil {
		return nil, 0, "", err
	}
	if apiKey == "" {
		return nil, 0, "no API key is set", nil
	}
	fetch := s.apiKeyDiscoverer(acc.Vendor)
	if fetch == nil {
		return nil, 0, noVendorDiscoveryNote, nil
	}
	models, status := fetch(ctx, s.vendorDiscovery.client, apiKey)
	return models, status, "", nil
}

// currentSubscriptionTokens opens a subscription account's sealed token set and
// returns it CURRENT: an expired-but-refreshable token is renewed first through the
// gateway's locked refresher (see refreshedVendorTokenSet). A non-empty note
// (credential-free) means no usable token set could be had and tokens is then the
// zero value, as it is on an error.
func (s *Service) currentSubscriptionTokens(ctx context.Context, acc routing.VendorAccount) (tokens vendorauth.TokenSet, note string, err error) {
	ts, err := s.openVendorTokenSet(acc)
	if err != nil {
		return vendorauth.TokenSet{}, "", err
	}
	if ts.AccessToken == "" {
		return vendorauth.TokenSet{}, "the subscription is not connected", nil
	}
	// Asking the vendor with a token known to be expired only earns a 401. A token
	// with a refresh token to renew it is renewed first, by the gateway's locked
	// refresher (see refreshedVendorTokenSet); one without can never heal, so it is
	// simply tried.
	if ts.RefreshToken != "" && ts.NeedsRefresh(s.clock(), 0) {
		var refreshNote string
		if ts, refreshNote, err = s.refreshedVendorTokenSet(ctx, acc); err != nil || refreshNote != "" {
			return vendorauth.TokenSet{}, refreshNote, err
		}
	}
	return ts, "", nil
}

// discoverSubscriptionModels runs the model-list fetcher of vendor for a
// subscription account with its current token set ts. Same note/status contract as
// discoverAPIKeyModels (the note is only the no-discovery one here: the credential
// questions were settled by currentSubscriptionTokens).
func (s *Service) discoverSubscriptionModels(ctx context.Context, vendor string, ts vendorauth.TokenSet) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus, string) {
	switch vendor {
	case routing.VendorOpenAI:
		models, status := s.vendorDiscovery.discoverers.OpenAISubscription(ctx, s.vendorDiscovery.client, ts.AccessToken, chatGPTAccountID(ts), s.VendorOpenAICodexClientVersion(ctx))
		return models, status, ""
	case routing.VendorAnthropic:
		models, status := s.vendorDiscovery.discoverers.AnthropicSubscription(ctx, s.vendorDiscovery.client, ts.AccessToken)
		return models, status, ""
	default:
		return nil, 0, noVendorDiscoveryNote
	}
}

// refreshVendorUsage pulls an OpenAI subscription account's usage snapshot (the
// five-hour and weekly windows, the credit balance and, for a Business plan, the
// spend control and the credit state) and stores it MERGED over
// the stored one (routing.MergeVendorAccountUsage: a field the pull does not know
// never blanks one the passive header scrape already stored). ts is the account's
// current token set, the one the model discovery opened and, if it was expired,
// renewed through the gateway's locked refresher: this path never opens or renews
// a token itself.
//
// It runs ONLY for an OpenAI subscription account that has an access token (an
// api_key account and every Anthropic account are skipped, the fetcher is never
// called) and is purely ADDITIVE and BEST EFFORT: it returns nothing, so it cannot
// change a refresh's outcome. An unverifiable fetch (a 401 included: only the
// dispatch flips an account to needs_reconnect), a failed read of the stored
// snapshot or a failed write is logged at Debug with the account id (never a token)
// and leaves the stored snapshot exactly as it was.
func (s *Service) refreshVendorUsage(ctx context.Context, acc routing.VendorAccount, ts vendorauth.TokenSet) {
	if acc.AuthType != routing.VendorAuthSubscription || acc.Vendor != routing.VendorOpenAI || ts.AccessToken == "" {
		return
	}
	usage, status := s.vendorDiscovery.discoverers.OpenAIUsage(ctx, s.vendorDiscovery.client, ts.AccessToken, chatGPTAccountID(ts))
	if status != vendorauth.DiscoveryOK {
		slog.Debug("vendor account usage fetch was unverifiable; the stored usage is kept", "account", acc.ID)
		return
	}
	snapshot := routing.VendorAccountUsage{
		AccountID:       acc.ID,
		FiveHourPct:     usage.FiveHourPct,
		FiveHourResetAt: usage.FiveHourResetAt,
		WeeklyPct:       usage.WeeklyPct,
		WeeklyResetAt:   usage.WeeklyResetAt,
		CreditBalance:   usage.CreditBalance,
		SpendUnit:       usage.SpendUnit,
		SpendLimit:      usage.SpendLimit,
		SpendUsed:       usage.SpendUsed,
		SpendRemaining:  usage.SpendRemaining,
		SpendUsedPct:    usage.SpendUsedPct,
		SpendResetAt:    usage.SpendResetAt,
		CreditStatus:    usage.CreditStatus,
		UpdatedAt:       s.clock().UTC(),
	}
	// An account with no stored snapshot is written as fetched: merging with the
	// zero value would turn every unknown percent (-1) into a fabricated 0.
	existing, found, err := s.routes.VendorAccountUsageByID(ctx, acc.ID)
	if err != nil {
		slog.Debug("vendor account usage read for merge failed; the stored usage is kept", "account", acc.ID, "err", err)
		return
	}
	if found {
		snapshot = routing.MergeVendorAccountUsage(existing, snapshot)
	}
	if err := s.routes.UpsertVendorAccountUsage(ctx, snapshot); err != nil {
		slog.Debug("vendor account usage write failed; the stored usage is kept", "account", acc.ID, "err", err)
	}
}

// noVendorDiscoveryNote is the note for an account whose vendor or auth type has
// no discovery.
const noVendorDiscoveryNote = "model discovery is not available for this account type"

// expiredVendorTokenNote is the note when an expired access token could not be
// renewed (the refresher failed, or left the stored token expired).
const expiredVendorTokenNote = "the access token has expired and could not be refreshed; try again, or reconnect the account if this persists"

// reconnectVendorTokenNote is the note when the vendor rejected the refresh token
// (the refresher marked the account needs_reconnect): retrying cannot help.
const reconnectVendorTokenNote = "the access token has expired and the vendor rejected the refresh token; reconnect the account"

// vendorTokenRefreshPendingNote is the note when the discovery stopped waiting for a
// token refresh that is still running (its own context ended first).
const vendorTokenRefreshPendingNote = "the access token has expired and its refresh is still in progress; refresh the models again in a moment"

// noVendorTokenRefresherNote is the note when an expired access token cannot be
// renewed here because no refresher is wired.
const noVendorTokenRefresherNote = "the access token has expired; it is refreshed automatically on the next request, then refresh the models again"

// refreshedVendorTokenSet renews acc's expired access token through the
// VendorTokenRefresher and returns the token set after the refresh. The refresher
// persists the renewed set, so the account is RE-LOADED and its tokens RE-OPENED:
// the fresh token is what the vendor is then asked with, never the copy read before.
// A non-empty note means no fresh token could be had (and why, with no credential
// in it): there is no refresher, it failed, or the stored token is still expired
// afterwards, or the discovery's own context ended while it was still running
// (runVendorTokenRefresh: the refresh is never cancelled mid-flight). When the
// vendor rejected the refresh token the note says to reconnect. The refresher's own
// error is not surfaced or logged here (it is the gateway's to log; this path only
// needs to know it failed). The error is a
// deleted account (ErrVendorAccountNotFound) or an unreadable renewed credential
// (ErrVendorAccountCredentialUnreadable).
func (s *Service) refreshedVendorTokenSet(ctx context.Context, acc routing.VendorAccount) (ts vendorauth.TokenSet, note string, err error) {
	refresh := s.vendorDiscovery.tokenRefresher
	if refresh == nil {
		return vendorauth.TokenSet{}, noVendorTokenRefresherNote, nil
	}
	switch outcome := runVendorTokenRefresh(ctx, refresh, acc.ID); outcome {
	case vendorTokenRefreshAbandoned:
		return vendorauth.TokenSet{}, vendorTokenRefreshPendingNote, nil
	case vendorTokenRefreshFailed:
		if s.vendorAccountNeedsReconnect(ctx, acc.ID) {
			return vendorauth.TokenSet{}, reconnectVendorTokenNote, nil
		}
		return vendorauth.TokenSet{}, expiredVendorTokenNote, nil
	}
	reloaded, err := s.routes.VendorAccountByID(ctx, acc.ID)
	if err != nil {
		return vendorauth.TokenSet{}, "", notFoundAsVendorAccountNotFound(err)
	}
	ts, err = s.openVendorTokenSet(reloaded)
	if err != nil {
		return vendorauth.TokenSet{}, "", err
	}
	if ts.AccessToken == "" || (ts.RefreshToken != "" && ts.NeedsRefresh(s.clock(), 0)) {
		return vendorauth.TokenSet{}, expiredVendorTokenNote, nil
	}
	return ts, "", nil
}

// vendorTokenRefreshOutcome is how runVendorTokenRefresh ended.
type vendorTokenRefreshOutcome int

const (
	// vendorTokenRefreshDone: the refresher returned nil.
	vendorTokenRefreshDone vendorTokenRefreshOutcome = iota
	// vendorTokenRefreshFailed: the refresher returned an error (or panicked).
	vendorTokenRefreshFailed
	// vendorTokenRefreshAbandoned: the caller's context ended while the refresher
	// was still running; it keeps running to its own end.
	vendorTokenRefreshAbandoned
)

// runVendorTokenRefresh runs refresh for accountID and waits for it, but never lets
// the CALLER's context cancel it. A refresh token is single-use: if the vendor has
// rotated it and the exchange or the persist is then cut off, only the dead old
// token is left and the account breaks on its next use (invalid_grant, then
// needs_reconnect). So the refresher runs on a context detached from ctx's
// cancellation (a client disconnect, the connect-time bound) under a bound of its
// own (vendorTokenRefreshTimeout), on its own goroutine; when ctx ends first the
// caller stops WAITING (vendorTokenRefreshAbandoned, so the discovery degrades to
// its fail-soft outcome on time) while the refresh runs on and persists. A panic in
// the refresher counts as a failed refresh instead of crashing the process.
func runVendorTokenRefresh(ctx context.Context, refresh VendorTokenRefresher, accountID string) vendorTokenRefreshOutcome {
	done := make(chan error, 1)
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), vendorTokenRefreshTimeout)
	go func() {
		defer cancel()
		defer func() {
			if recover() != nil {
				done <- errVendorTokenRefresherPanicked
			}
		}()
		done <- refresh(rctx, accountID)
	}()
	select {
	case err := <-done:
		if err != nil {
			return vendorTokenRefreshFailed
		}
		return vendorTokenRefreshDone
	case <-ctx.Done():
		return vendorTokenRefreshAbandoned
	}
}

// errVendorTokenRefresherPanicked stands for a refresher that panicked; it carries
// no panic value, which could hold anything.
var errVendorTokenRefresherPanicked = errors.New("the vendor token refresher panicked")

// vendorAccountNeedsReconnect reports whether the account is now marked
// needs_reconnect (the gateway's refresher does that when the vendor rejects the
// refresh token). A failed read is "no": the generic note then applies.
func (s *Service) vendorAccountNeedsReconnect(ctx context.Context, id string) bool {
	acc, err := s.routes.VendorAccountByID(ctx, id)
	return err == nil && acc.Status == routing.VendorAccountStatusNeedsReconnect
}

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

// relabelVendorAccountModels re-labels the stored model rows of the account id to
// the account's CURRENT model prefix, without asking the vendor. It writes only
// when some row would change, so re-sending the prefix an account already has costs
// a read. It runs under the account's model-write lock and reads the account and
// its rows inside it, so it re-labels whatever a concurrent discovery wrote, and a
// discovery that follows writes under the prefix this one used.
func (s *Service) relabelVendorAccountModels(ctx context.Context, id string) error {
	unlock, err := s.vendorModelWrites.lock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	acc, err := s.routes.VendorAccountByID(ctx, id)
	if err != nil {
		return notFoundAsVendorAccountNotFound(err)
	}
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
//
// The whole discovery runs under its own short bound (vendorConnectDiscoveryTimeout,
// shared by the token refresh, the fetch and the write), so a vendor that hangs
// adds at most that to the connect and the discovery degrades to its fail-soft
// outcome (the seeded models are kept).
func (s *Service) discoverAfterConnect(ctx context.Context, principal auth.Token, dto VendorAccountDTO) VendorAccountDTO {
	timeout := s.vendorDiscovery.connectTimeout
	if timeout <= 0 {
		timeout = vendorConnectDiscoveryTimeout
	}
	requestCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	refreshed, result, err := s.RefreshVendorAccountModels(ctx, principal, dto.ID)
	if err != nil {
		if errors.Is(err, ErrVendorAccountCredentialUnreadable) {
			// A FIXED line, never the cause chain: the chain of a stored blob that
			// cannot be decoded can quote part of what it could not decode.
			slog.Warn("vendor model discovery after connect skipped: the stored credential could not be read; the account keeps its current models", "account", dto.ID)
		} else {
			slog.Warn("vendor model discovery after connect failed; the account keeps its current models", "account", dto.ID, "err", err)
		}
		return s.currentConnectedVendorAccount(requestCtx, dto)
	}
	slog.Info("vendor model discovery after connect", "account", dto.ID, "status", result.Status, "discovered", result.Discovered)
	return refreshed
}

// currentConnectedVendorAccount re-reads the account for the connect response after
// a discovery that failed: it may have changed since dto was built (a status the
// discovery's token refresh set). Best effort on the REQUEST's context, not the
// discovery's (which may be the very thing that ended): any failure returns dto as
// it was, the connect itself having succeeded.
func (s *Service) currentConnectedVendorAccount(ctx context.Context, dto VendorAccountDTO) VendorAccountDTO {
	acc, err := s.routes.VendorAccountByID(ctx, dto.ID)
	if err != nil {
		return dto
	}
	current, err := s.vendorAccountDTO(ctx, acc)
	if err != nil {
		return dto
	}
	return current
}
