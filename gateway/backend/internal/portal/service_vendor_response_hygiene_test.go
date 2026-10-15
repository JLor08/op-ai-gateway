// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"strings"
	"testing"
	"time"
)

// Response hygiene of the CM-reachable operate endpoints (spec 2026-10-15
// section 6): "every CM-reachable operate endpoint (test/refresh) scrubs its
// result and error payloads of any api_key/access/refresh token material -- a CM
// must never see the raw secret even in an echoed upstream error".
//
// RefreshVendorAccountModels and RefreshVendorAccountUsage became CM-reachable with
// the operate gate (the connection test has its own hygiene tests). Today the
// property holds by construction: the vendor fetchers answer a status enum and
// data, the token refresher's error is discarded, and every Detail is a fixed
// English phrase. This test is the regression guard for the day someone
// interpolates a vendor-supplied string or a raw upstream error into a Detail or
// an error.
//
// It plants secrets on every channel such a change could reach:
//
//   - the STORED credential (api key, OAuth access and refresh token) is what
//     the endpoints open and send upstream; none may come back, in a result, in the
//     account/usage data that travels with it, or in an error;
//   - the UPSTREAM side -- the token refresher's raw error, the model names a
//     vendor lists, the credit balance and spend unit it reports -- carries a
//     bearer token and an "echo" string; none may reach a result's Detail or an
//     error. (The model names and usage figures are data the response legitimately
//     carries, so they are checked only on the result and the error, never on the
//     account or usage objects themselves.)
//   - an unreadable stored value that contains a secret: the error that reports
//     it must stay the sentinel's fixed text.
//
// Every endpoint is driven twice: as a CM of a shared account (the new reach) and
// as the owner.
const (
	hygieneAPIKey  = "sk-PLANTED-SECRET-abc123"
	hygieneAccess  = "tok-PLANTED-ACCESS-def456"
	hygieneRefresh = "tok-PLANTED-REFRESH-ghi789"
	hygieneBearer  = "PLANTED-BEARER-jkl012"
	hygieneEcho    = "PLANTED-VENDOR-ECHO-mno345"

	// hygieneUpstreamError is a raw upstream error that echoes every secret, the way
	// a careless token-endpoint error body or HTTP client error can.
	hygieneUpstreamError = "upstream 401 for Bearer " + hygieneBearer + ": refresh_token=" + hygieneRefresh +
		" key=" + hygieneAPIKey + " said " + hygieneEcho
)

// hygieneStoredSecrets are the credentials the account holds.
func hygieneStoredSecrets() []string {
	return []string{hygieneAPIKey, hygieneAccess, hygieneRefresh}
}

// hygieneAllSecrets adds what the planted upstream side echoes.
func hygieneAllSecrets() []string {
	return append(hygieneStoredSecrets(), hygieneBearer, hygieneEcho)
}

// leakedSecrets returns every secret that appears in v: in its full message when v
// is an error (the whole wrap chain), in its JSON form otherwise.
func leakedSecrets(v any, secrets []string) []string {
	var text string
	if err, ok := v.(error); ok {
		text = err.Error()
	} else {
		raw, err := json.Marshal(v)
		if err != nil {
			return []string{"unmarshalable: " + err.Error()}
		}
		text = string(raw)
	}
	var leaked []string
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			leaked = append(leaked, secret)
		}
	}
	return leaked
}

// shareAtShared assigns the account to AG at `shared` (with a containment root),
// the state in which usr_cm may operate it.
func (e *vendorAuthzEnv) shareAtShared(id string) {
	e.t.Helper()
	if err := e.routes.UpdateVendorAccountShareLevel(e.ctx, id, routing.VendorShareLevelShared); err != nil {
		e.t.Fatalf("raise %s: %v", id, err)
	}
	if err := e.routes.SetVendorAccountAdminGroup(e.ctx, id, e.ag.ID); err != nil {
		e.t.Fatalf("assign %s to AG: %v", id, err)
	}
	if err := e.routes.UpdateVendorAccountSystemGroup(e.ctx, id, e.ag.ParentGroupID); err != nil {
		e.t.Fatalf("set root of %s: %v", id, err)
	}
}

// hygieneAccount creates an account owned by usr_va_owner through the service.
func (e *vendorAuthzEnv) hygieneAccount(req CreateVendorAccountRequest) string {
	e.t.Helper()
	dto, err := e.svc.CreateVendorAccount(e.ctx, token(vendorAuthzOwnerID), req)
	if err != nil {
		e.t.Fatalf("CreateVendorAccount(%s): %v", req.Name, err)
	}
	return dto.ID
}

// corruptCredential overwrites the stored credential columns with values the
// sealing format does not recognise, each carrying a secret.
func (e *vendorAuthzEnv) corruptCredential(id, apiKey, oauthTokens string) {
	e.t.Helper()
	row := e.reload(id)
	row.APIKey, row.OAuthTokens = apiKey, oauthTokens
	if err := e.routes.UpdateVendorAccount(e.ctx, row); err != nil {
		e.t.Fatalf("UpdateVendorAccount(%s): %v", id, err)
	}
}

// hygieneCase is one planted-secret scenario for one operate endpoint.
type hygieneCase struct {
	name string
	// seed creates the account (key/tokens planted) and arranges the fakes. after,
	// when non-nil, asserts the planted secret actually reached the vendor seam, so
	// a scenario cannot go green by never exercising the path it names.
	seed func(t *testing.T, e *vendorAuthzEnv) (id string, after func(t *testing.T))
	// call drives the endpoint. answers are the result structs (checked against
	// every planted secret); data are the account/usage objects that travel with
	// them (checked against the stored credentials only).
	call func(e *vendorAuthzEnv, principal auth.Token, id string) (answers, data []any, err error)
	// wantErr is the sentinel the call must fail with; nil means it must succeed.
	wantErr error
}

func hygieneModelsCall(e *vendorAuthzEnv, principal auth.Token, id string) (answers, data []any, err error) {
	dto, res, err := e.svc.RefreshVendorAccountModels(e.ctx, principal, id)
	return []any{res}, []any{dto}, err
}

func hygieneUsageCall(e *vendorAuthzEnv, principal auth.Token, id string) (answers, data []any, err error) {
	usage, res, err := e.svc.RefreshVendorAccountUsage(e.ctx, principal, id, 0)
	return []any{res}, []any{usage}, err
}

func hygieneSubscription(vendor string) CreateVendorAccountRequest {
	return CreateVendorAccountRequest{Vendor: vendor, AuthType: routing.VendorAuthSubscription, Name: "Planted subscription"}
}

func hygieneCases() []hygieneCase {
	connected := vendorauth.TokenSet{AccessToken: hygieneAccess, RefreshToken: hygieneRefresh, AccountID: discoveryTestAccount}
	expired := vendorauth.TokenSet{
		AccessToken: hygieneAccess, RefreshToken: hygieneRefresh, AccountID: discoveryTestAccount,
		ExpiresAt: discoveryTestNow.Add(-time.Hour),
	}
	// seedExpiredWithFailingRefresher is the shared "raw upstream error" scenario: an
	// expired subscription whose renewal fails with an error echoing every secret.
	seedExpiredWithFailingRefresher := func(t *testing.T, e *vendorAuthzEnv) (string, func(*testing.T)) {
		id := e.hygieneAccount(hygieneSubscription(routing.VendorOpenAI))
		setDiscoveryTokens(t, e.svc, e.routes, id, expired)
		refresher := installTokenRefresher(t, e.svc, e.routes)
		refresher.err = errors.New(hygieneUpstreamError)
		return id, func(t *testing.T) {
			t.Helper()
			if len(refresher.recorded()) != 1 {
				t.Fatalf("refresher calls = %v, want exactly one (the planted error was never produced)", refresher.recorded())
			}
		}
	}

	return []hygieneCase{
		{
			name: "models: vendor lists model names that echo secrets",
			seed: func(t *testing.T, e *vendorAuthzEnv) (string, func(*testing.T)) {
				id := e.hygieneAccount(CreateVendorAccountRequest{
					Vendor: routing.VendorAnthropic, AuthType: routing.VendorAuthAPIKey, Name: "Planted key", APIKey: hygieneAPIKey,
				})
				e.discoverers.ok(kindAnthropicAPIKey, discovered("claude-"+hygieneEcho, "Echo Bearer "+hygieneBearer))
				return id, func(t *testing.T) {
					t.Helper()
					if call := e.discoverers.onlyCall(t); call.credential != hygieneAPIKey {
						t.Fatalf("discoverer credential = %q, want the planted api key", call.credential)
					}
				}
			},
			call: hygieneModelsCall,
		},
		{
			name: "models: vendor cannot be asked",
			seed: func(t *testing.T, e *vendorAuthzEnv) (string, func(*testing.T)) {
				id := e.hygieneAccount(CreateVendorAccountRequest{
					Vendor: routing.VendorAnthropic, AuthType: routing.VendorAuthAPIKey, Name: "Planted key", APIKey: hygieneAPIKey,
				})
				return id, func(t *testing.T) {
					t.Helper()
					if call := e.discoverers.onlyCall(t); call.credential != hygieneAPIKey {
						t.Fatalf("discoverer credential = %q, want the planted api key", call.credential)
					}
				}
			},
			call: hygieneModelsCall,
		},
		{
			name: "models: subscription renewal fails with a raw upstream error",
			seed: seedExpiredWithFailingRefresher,
			call: hygieneModelsCall,
		},
		{
			name: "models: unreadable api key",
			seed: func(t *testing.T, e *vendorAuthzEnv) (string, func(*testing.T)) {
				id := e.hygieneAccount(CreateVendorAccountRequest{
					Vendor: routing.VendorAnthropic, AuthType: routing.VendorAuthAPIKey, Name: "Planted key",
				})
				e.corruptCredential(id, "garbage-"+hygieneAPIKey, "")
				return id, nil
			},
			call:    hygieneModelsCall,
			wantErr: ErrVendorAccountCredentialUnreadable,
		},
		{
			name: "models: unreadable token set",
			seed: func(t *testing.T, e *vendorAuthzEnv) (string, func(*testing.T)) {
				id := e.hygieneAccount(hygieneSubscription(routing.VendorOpenAI))
				e.corruptCredential(id, "", "garbage-"+hygieneAccess+"-"+hygieneRefresh)
				return id, nil
			},
			call:    hygieneModelsCall,
			wantErr: ErrVendorAccountCredentialUnreadable,
		},
		{
			name: "usage: subscription pull reports a balance that echoes a secret",
			seed: func(t *testing.T, e *vendorAuthzEnv) (string, func(*testing.T)) {
				id := e.hygieneAccount(hygieneSubscription(routing.VendorOpenAI))
				setDiscoveryTokens(t, e.svc, e.routes, id, connected)
				e.discoverers.okUsage(vendorauth.OpenAISubscriptionUsage{
					FiveHourPct: 23, WeeklyPct: 61, CreditBalance: hygieneEcho, SpendUnit: hygieneBearer, SpendUsedPct: -1,
				})
				return id, func(t *testing.T) {
					t.Helper()
					if call := e.discoverers.onlyUsageCall(t); call.accessToken != hygieneAccess {
						t.Fatalf("usage fetch token = %q, want the planted access token", call.accessToken)
					}
				}
			},
			call: hygieneUsageCall,
		},
		{
			name: "usage: subscription pull unverifiable",
			seed: func(t *testing.T, e *vendorAuthzEnv) (string, func(*testing.T)) {
				id := e.hygieneAccount(hygieneSubscription(routing.VendorOpenAI))
				setDiscoveryTokens(t, e.svc, e.routes, id, connected)
				e.discoverers.failUsage(vendorauth.OpenAISubscriptionUsage{
					FiveHourPct: -1, WeeklyPct: -1, CreditBalance: hygieneEcho, SpendUnit: hygieneBearer, SpendUsedPct: -1,
				})
				return id, func(t *testing.T) {
					t.Helper()
					if call := e.discoverers.onlyUsageCall(t); call.accessToken != hygieneAccess {
						t.Fatalf("usage fetch token = %q, want the planted access token", call.accessToken)
					}
				}
			},
			call: hygieneUsageCall,
		},
		{
			name: "usage: subscription renewal fails with a raw upstream error",
			seed: seedExpiredWithFailingRefresher,
			call: hygieneUsageCall,
		},
		{
			name: "usage: api-key pull reports figures that echo a secret",
			seed: func(t *testing.T, e *vendorAuthzEnv) (string, func(*testing.T)) {
				id := e.hygieneAccount(CreateVendorAccountRequest{
					Vendor: routing.VendorOpenRouter, AuthType: routing.VendorAuthAPIKey, Name: "Planted key", APIKey: hygieneAPIKey,
				})
				// The create may have made its own discovery and usage calls; only the
				// refresh's own call is of interest.
				e.discoverers.reset()
				e.svc.vendorUsagePulls.forget(id)
				e.discoverers.okCompatUsage(vendorauth.OpenAISubscriptionUsage{
					FiveHourPct: -1, WeeklyPct: -1, CreditBalance: hygieneEcho, SpendUnit: hygieneBearer,
					SpendLimit: "100", SpendUsed: "40", SpendRemaining: "60", SpendUsedPct: 40,
				})
				return id, func(t *testing.T) {
					t.Helper()
					if call := e.discoverers.onlyCompatUsageCall(t); call.apiKey != hygieneAPIKey {
						t.Fatalf("usage fetch key = %q, want the planted api key", call.apiKey)
					}
				}
			},
			call: hygieneUsageCall,
		},
		{
			name: "usage: unreadable api key",
			seed: func(t *testing.T, e *vendorAuthzEnv) (string, func(*testing.T)) {
				id := e.hygieneAccount(CreateVendorAccountRequest{
					Vendor: routing.VendorOpenRouter, AuthType: routing.VendorAuthAPIKey, Name: "Planted key",
				})
				e.corruptCredential(id, "garbage-"+hygieneAPIKey, "")
				e.svc.vendorUsagePulls.forget(id)
				return id, nil
			},
			call:    hygieneUsageCall,
			wantErr: ErrVendorAccountCredentialUnreadable,
		},
		{
			name: "usage: unreadable token set",
			seed: func(t *testing.T, e *vendorAuthzEnv) (string, func(*testing.T)) {
				id := e.hygieneAccount(hygieneSubscription(routing.VendorOpenAI))
				e.corruptCredential(id, "", "garbage-"+hygieneAccess+"-"+hygieneRefresh)
				return id, nil
			},
			call:    hygieneUsageCall,
			wantErr: ErrVendorAccountCredentialUnreadable,
		},
	}
}

// TestCMReachableRefreshEndpointsNeverEchoCredentialMaterial is the section 6
// response-hygiene regression for model refresh and usage refresh (see the file
// comment). A leak fails the case that produced it, naming the secret.
func TestCMReachableRefreshEndpointsNeverEchoCredentialMaterial(t *testing.T) {
	principals := []struct {
		name string
		tok  auth.Token
	}{
		{"cm", token("usr_cm", "admin")},
		{"owner", token(vendorAuthzOwnerID)},
	}
	for _, tc := range hygieneCases() {
		for _, p := range principals {
			t.Run(tc.name+"/"+p.name, func(t *testing.T) {
				e := newVendorAuthzEnv(t)
				e.svc.settingsVolatile = true // seal the planted secrets as "plain:" like RAM mode
				setUsageClock(e.svc, discoveryTestNow)
				id, after := tc.seed(t, e)
				e.shareAtShared(id)

				answers, data, err := tc.call(e, p.tok, id)

				if tc.wantErr != nil {
					if !errors.Is(err, tc.wantErr) {
						t.Fatalf("err = %v, want %v", err, tc.wantErr)
					}
				} else if err != nil {
					t.Fatalf("err = %v, want success", err)
				}
				if after != nil {
					after(t)
				}
				if err != nil {
					if leaked := leakedSecrets(err, hygieneAllSecrets()); len(leaked) > 0 {
						t.Errorf("the error leaks %v: %v", leaked, err)
					}
				}
				for _, answer := range answers {
					if leaked := leakedSecrets(answer, hygieneAllSecrets()); len(leaked) > 0 {
						t.Errorf("the %T result leaks %v: %+v", answer, leaked, answer)
					}
				}
				for _, d := range data {
					if leaked := leakedSecrets(d, hygieneStoredSecrets()); len(leaked) > 0 {
						t.Errorf("the %T that travels with the result leaks %v: %+v", d, leaked, d)
					}
				}
			})
		}
	}
}

// TestHygieneCheckDetectsALeak keeps the check above honest: it must flag a secret
// in a result's Detail, in an error's message (through a wrap chain), and in
// serialized account data, and must stay quiet on a clean answer.
func TestHygieneCheckDetectsALeak(t *testing.T) {
	all := hygieneAllSecrets()
	if got := leakedSecrets(RefreshResult{Status: VendorRefreshOK, Detail: "first model " + hygieneEcho}, all); len(got) != 1 || got[0] != hygieneEcho {
		t.Errorf("a secret in RefreshResult.Detail was reported as %v", got)
	}
	if got := leakedSecrets(VendorUsageRefreshResult{Status: VendorUsageRefreshOK, Detail: "pulled with " + hygieneAccess}, all); len(got) != 1 || got[0] != hygieneAccess {
		t.Errorf("a secret in VendorUsageRefreshResult.Detail was reported as %v", got)
	}
	wrapped := errors.Join(ErrVendorAccountCredentialUnreadable, errors.New("open key "+hygieneAPIKey))
	if got := leakedSecrets(wrapped, all); len(got) != 1 || got[0] != hygieneAPIKey {
		t.Errorf("a secret in a wrapped error was reported as %v", got)
	}
	if got := leakedSecrets(RefreshResult{Status: VendorRefreshOK, Discovered: 2, Detail: "discovered 2 models"}, all); len(got) != 0 {
		t.Errorf("a clean result was reported as leaking %v", got)
	}
}
