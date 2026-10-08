// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"strings"
	"sync"
	"testing"
	"time"
)

// The four validator kinds the fake records, one per vendorauth probe.
const (
	kindOpenAISubscription    = "openai-subscription"
	kindAnthropicSubscription = "anthropic-subscription"
	kindOpenAIAPIKey          = "openai-api-key"
	kindAnthropicAPIKey       = "anthropic-api-key"
)

// fakeValidatorCall is one recorded validator invocation.
type fakeValidatorCall struct {
	kind       string
	credential string
	// timeout is the Timeout of the *http.Client the service handed over; hasClient
	// is false when it handed over nil.
	timeout   time.Duration
	hasClient bool
}

// fakeVendorValidators replaces the four vendorauth probes so no service test
// reaches the network. Each kind answers its configured verdict (Unverifiable
// until set) and every call is recorded.
type fakeVendorValidators struct {
	mu     sync.Mutex
	checks map[string]vendorauth.CredentialCheck
	calls  []fakeValidatorCall
}

func newFakeVendorValidators() *fakeVendorValidators {
	f := &fakeVendorValidators{checks: map[string]vendorauth.CredentialCheck{}}
	for _, kind := range []string{kindOpenAISubscription, kindAnthropicSubscription, kindOpenAIAPIKey, kindAnthropicAPIKey} {
		f.checks[kind] = vendorauth.CredentialCheck{Status: vendorauth.StatusUnverifiable, Detail: "fake: not configured"}
	}
	return f
}

// installFakeVendorValidators wires a fresh fake into svc and returns it.
func installFakeVendorValidators(svc *Service) *fakeVendorValidators {
	f := newFakeVendorValidators()
	svc.vendorValidation.validators = f.validators()
	return f
}

func (f *fakeVendorValidators) set(kind string, check vendorauth.CredentialCheck) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks[kind] = check
}

// setAll gives every kind the same verdict.
func (f *fakeVendorValidators) setAll(check vendorauth.CredentialCheck) {
	for _, kind := range []string{kindOpenAISubscription, kindAnthropicSubscription, kindOpenAIAPIKey, kindAnthropicAPIKey} {
		f.set(kind, check)
	}
}

func (f *fakeVendorValidators) record(kind string) func(context.Context, *http.Client, string) vendorauth.CredentialCheck {
	return func(_ context.Context, client *http.Client, credential string) vendorauth.CredentialCheck {
		f.mu.Lock()
		defer f.mu.Unlock()
		call := fakeValidatorCall{kind: kind, credential: credential, hasClient: client != nil}
		if client != nil {
			call.timeout = client.Timeout
		}
		f.calls = append(f.calls, call)
		return f.checks[kind]
	}
}

func (f *fakeVendorValidators) validators() VendorCredentialValidators {
	return VendorCredentialValidators{
		OpenAISubscription:    f.record(kindOpenAISubscription),
		AnthropicSubscription: f.record(kindAnthropicSubscription),
		OpenAIAPIKey:          f.record(kindOpenAIAPIKey),
		AnthropicAPIKey:       f.record(kindAnthropicAPIKey),
	}
}

func (f *fakeVendorValidators) recorded() []fakeValidatorCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeValidatorCall(nil), f.calls...)
}

func (f *fakeVendorValidators) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// onlyCall fails the test unless exactly one validator call was made, and returns it.
func (f *fakeVendorValidators) onlyCall(t *testing.T) fakeValidatorCall {
	t.Helper()
	calls := f.recorded()
	if len(calls) != 1 {
		t.Fatalf("validator calls = %+v, want exactly one", calls)
	}
	return calls[0]
}

// jwtWithClaims is an unsigned three-segment JWT with the given claims.
func jwtWithClaims(t *testing.T, claims map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

func validCheck(accountID, plan string) vendorauth.CredentialCheck {
	return vendorauth.CredentialCheck{Status: vendorauth.StatusValid, Detail: "credential accepted (HTTP 200)", AccountID: accountID, PlanType: plan}
}

func invalidCheck() vendorauth.CredentialCheck {
	return vendorauth.CredentialCheck{Status: vendorauth.StatusInvalid, Detail: "credential rejected (HTTP 401)"}
}

func unverifiableCheck() vendorauth.CredentialCheck {
	return vendorauth.CredentialCheck{Status: vendorauth.StatusUnverifiable, Detail: "could not verify (HTTP 503)"}
}

// --- import: fail-soft validation ------------------------------------------

// An Invalid verdict is the only one that blocks: nothing is persisted, the
// account stays unconnected and the error carries no token.
func TestConnectVendorAccountImportInvalidCredentialsAreNotPersisted(t *testing.T) {
	for _, vendor := range []string{routing.VendorOpenAI, routing.VendorAnthropic} {
		t.Run(vendor, func(t *testing.T) {
			svc, routeStore, _ := newVendorConnectTestService(t)
			fake := installFakeVendorValidators(svc)
			// A probe that echoes the credentials into its detail must not leak them.
			fake.setAll(vendorauth.CredentialCheck{Status: vendorauth.StatusInvalid, Detail: "rejected " + connectTestAccess + " / " + connectTestRefresh})
			acc := createSubscriptionAccount(t, svc, ownerToken(), vendor, "Dead token")

			_, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess, RefreshToken: connectTestRefresh})
			if !errors.Is(err, ErrVendorAccountConnectInvalidCredentials) {
				t.Fatalf("err = %v, want ErrVendorAccountConnectInvalidCredentials", err)
			}
			for _, secret := range []string{connectTestAccess, connectTestRefresh} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaks a token: %v", err)
				}
			}
			row, getErr := routeStore.VendorAccountByID(context.Background(), acc.ID)
			if getErr != nil {
				t.Fatalf("VendorAccountByID: %v", getErr)
			}
			if row.OAuthTokens != "" {
				t.Fatalf("OAuthTokens = %q, want nothing persisted for an invalid credential", row.OAuthTokens)
			}
			if row.Status != acc.Status || !row.UpdatedAt.Equal(acc.UpdatedAt) {
				t.Fatalf("account changed on a blocked import: %+v", row)
			}
		})
	}
}

// Unverifiable (vendor unreachable, rate limited, an unexpected answer) and the
// zero verdict never block: the import persists exactly as without validation.
func TestConnectVendorAccountImportProceedsWhenTheVendorCannotBeChecked(t *testing.T) {
	verdicts := map[string]vendorauth.CredentialCheck{
		"unverifiable":   unverifiableCheck(),
		"valid":          validCheck("", ""),
		"unknown (zero)": {},
	}
	for _, vendor := range []string{routing.VendorOpenAI, routing.VendorAnthropic} {
		for name, verdict := range verdicts {
			t.Run(vendor+"/"+name, func(t *testing.T) {
				svc, routeStore, _ := newVendorConnectTestService(t)
				fake := installFakeVendorValidators(svc)
				fake.setAll(verdict)
				acc := createSubscriptionAccount(t, svc, ownerToken(), vendor, "Imported")

				dto, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess, RefreshToken: connectTestRefresh})
				if err != nil {
					t.Fatalf("ConnectVendorAccountImport: %v", err)
				}
				if !dto.SubscriptionConnected || dto.Status != routing.VendorAccountStatusActive {
					t.Fatalf("dto = %+v, want a connected active account", dto)
				}
				_, ts := storedTokenSet(t, routeStore, svc, acc.ID)
				if ts.AccessToken != connectTestAccess || ts.RefreshToken != connectTestRefresh {
					t.Fatalf("stored token set = %v, want the pasted tokens", ts)
				}
			})
		}
	}
}

// The import uses ONLY the matching subscription validator -- one call, the
// trimmed access token, a bounded client -- and never an api-key probe.
func TestConnectVendorAccountImportRunsExactlyOneMatchingValidator(t *testing.T) {
	cases := map[string]string{
		routing.VendorOpenAI:    kindOpenAISubscription,
		routing.VendorAnthropic: kindAnthropicSubscription,
	}
	for vendor, wantKind := range cases {
		t.Run(vendor, func(t *testing.T) {
			svc, _, _ := newVendorConnectTestService(t)
			fake := installFakeVendorValidators(svc)
			acc := createSubscriptionAccount(t, svc, ownerToken(), vendor, "Imported")

			if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: "  " + connectTestAccess + "\n", RefreshToken: connectTestRefresh}); err != nil {
				t.Fatalf("ConnectVendorAccountImport: %v", err)
			}
			call := fake.onlyCall(t)
			if call.kind != wantKind || call.credential != connectTestAccess {
				t.Fatalf("call = %+v, want kind %s with the trimmed access token", call, wantKind)
			}
			if !call.hasClient || call.timeout != 10*time.Second {
				t.Fatalf("call = %+v, want a client with a 10s timeout", call)
			}
		})
	}
}

// Nothing is validated for a request the service refuses anyway: a stranger, a
// blank token, an api_key account and a disabled module never reach the vendor.
func TestConnectVendorAccountImportValidatesOnlyAnAcceptedRequest(t *testing.T) {
	svc, _, _ := newVendorConnectTestService(t)
	fake := installFakeVendorValidators(svc)
	sub := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "Sub")
	apiKeyAcc := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Key account"))
	ctx := context.Background()

	if _, err := svc.ConnectVendorAccountImport(ctx, otherToken(), sub.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess}); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("stranger: err = %v, want ErrVendorAccountNotFound", err)
	}
	if _, err := svc.ConnectVendorAccountImport(ctx, ownerToken(), sub.ID, ConnectVendorAccountImportRequest{AccessToken: " \n"}); !errors.Is(err, ErrVendorAccountConnectTokenRequired) {
		t.Fatalf("blank token: err = %v, want ErrVendorAccountConnectTokenRequired", err)
	}
	if _, err := svc.ConnectVendorAccountImport(ctx, ownerToken(), apiKeyAcc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess}); !errors.Is(err, ErrVendorAccountNotSubscription) {
		t.Fatalf("api_key account: err = %v, want ErrVendorAccountNotSubscription", err)
	}
	setVendorAccountsEnabled(t, svc, false)
	if _, err := svc.ConnectVendorAccountImport(ctx, ownerToken(), sub.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess}); !errors.Is(err, ErrVendorAccountsDisabled) {
		t.Fatalf("disabled: err = %v, want ErrVendorAccountsDisabled", err)
	}
	if calls := fake.recorded(); len(calls) != 0 {
		t.Fatalf("validator calls = %+v, want none for a refused request", calls)
	}
}

// The probe is a network round trip: a rename made while it is in flight must
// survive the import's write (the account is re-loaded after the probe).
func TestConnectVendorAccountImportKeepsARenameMadeWhileTheProbeRuns(t *testing.T) {
	svc, routeStore, _ := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Before")
	svc.vendorValidation.validators.AnthropicSubscription = func(ctx context.Context, _ *http.Client, _ string) vendorauth.CredentialCheck {
		row, err := routeStore.VendorAccountByID(ctx, acc.ID)
		if err != nil {
			t.Errorf("VendorAccountByID: %v", err)
			return unverifiableCheck()
		}
		row.Name = "Renamed meanwhile"
		if err := routeStore.UpdateVendorAccount(ctx, row); err != nil {
			t.Errorf("UpdateVendorAccount: %v", err)
		}
		return validCheck("", "")
	}

	dto, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess})
	if err != nil {
		t.Fatalf("ConnectVendorAccountImport: %v", err)
	}
	if dto.Name != "Renamed meanwhile" || !dto.SubscriptionConnected {
		t.Fatalf("dto = %+v, want the concurrent rename kept on the connected account", dto)
	}
	if row, _ := storedTokenSet(t, routeStore, svc, acc.ID); row.Name != "Renamed meanwhile" {
		t.Fatalf("stored name = %q, want the concurrent rename kept", row.Name)
	}
}

// --- import: OpenAI account id backfill ------------------------------------

// A pasted OpenAI access token without the ChatGPT account claim is backfilled
// (id and plan) from the Valid verdict, so dispatch has its chatgpt-account-id.
func TestConnectVendorAccountImportBackfillsTheOpenAIAccountFromTheValidator(t *testing.T) {
	svc, routeStore, _ := newVendorConnectTestService(t)
	fake := installFakeVendorValidators(svc)
	fake.set(kindOpenAISubscription, validCheck("acct-from-check", "plus"))
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "Imported")

	if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: "sk-not-a-jwt", RefreshToken: connectTestRefresh}); err != nil {
		t.Fatalf("ConnectVendorAccountImport: %v", err)
	}
	_, ts := storedTokenSet(t, routeStore, svc, acc.ID)
	if ts.AccountID != "acct-from-check" || ts.PlanType != "plus" {
		t.Fatalf("token set = %v, want the account facts backfilled from the validator", ts)
	}
}

// The JWT-derived account wins: the validator only fills a gap.
func TestConnectVendorAccountImportKeepsTheAccountFromTheJWT(t *testing.T) {
	svc, routeStore, _ := newVendorConnectTestService(t)
	fake := installFakeVendorValidators(svc)
	fake.set(kindOpenAISubscription, validCheck("acct-from-check", "pro"))
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "Imported")

	if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestJWT(t, "acct-from-jwt", "plus")}); err != nil {
		t.Fatalf("ConnectVendorAccountImport: %v", err)
	}
	_, ts := storedTokenSet(t, routeStore, svc, acc.ID)
	if ts.AccountID != "acct-from-jwt" || ts.PlanType != "plus" {
		t.Fatalf("token set = %v, want the JWT-derived account facts kept", ts)
	}
}

// Only a Valid verdict backfills, and only for OpenAI.
func TestConnectVendorAccountImportBackfillsOnlyFromAValidOpenAIVerdict(t *testing.T) {
	t.Run("unverifiable verdict carrying an id", func(t *testing.T) {
		svc, routeStore, _ := newVendorConnectTestService(t)
		fake := installFakeVendorValidators(svc)
		fake.set(kindOpenAISubscription, vendorauth.CredentialCheck{Status: vendorauth.StatusUnverifiable, AccountID: "acct-x", PlanType: "plus"})
		acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "Imported")
		if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: "sk-not-a-jwt"}); err != nil {
			t.Fatalf("ConnectVendorAccountImport: %v", err)
		}
		if _, ts := storedTokenSet(t, routeStore, svc, acc.ID); ts.AccountID != "" || ts.PlanType != "" {
			t.Fatalf("token set = %v, want no backfill from an unverifiable verdict", ts)
		}
	})
	t.Run("anthropic account", func(t *testing.T) {
		svc, routeStore, _ := newVendorConnectTestService(t)
		fake := installFakeVendorValidators(svc)
		fake.set(kindAnthropicSubscription, validCheck("acct-x", "max"))
		acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude")
		if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess}); err != nil {
			t.Fatalf("ConnectVendorAccountImport: %v", err)
		}
		if _, ts := storedTokenSet(t, routeStore, svc, acc.ID); ts.AccountID != "" || ts.PlanType != "" {
			t.Fatalf("token set = %v, want an Anthropic import never to take OpenAI account facts", ts)
		}
	})
}

// The validator's strings are unvalidated vendor input and the account id is sent
// back as a request header: a value that is not 1..128 printable ASCII characters
// is dropped (left empty) instead of stored. The import itself still succeeds.
func TestConnectVendorAccountImportDropsAJunkBackfilledIdentity(t *testing.T) {
	cases := []struct {
		name   string
		value  string
		stored bool
	}{
		{"uuid", "5f3c2a1e-0000-4000-8000-123456789abc", true},
		{"punctuation", "org-AbC_123.x:y", true},
		{"exactly the cap", strings.Repeat("a", 128), true},
		{"one over the cap", strings.Repeat("a", 129), false},
		{"one MiB", strings.Repeat("a", 1<<20), false},
		{"empty", "", false},
		{"non-ascii", "acct-é", false},
		{"control character", "acct-\x01", false},
		{"newline (header injection)", "acct\r\nX-Evil: 1", false},
		{"delete character", "acct\x7f", false},
		{"leading space", " acct", false},
		{"trailing space", "acct ", false},
	}
	for _, tc := range cases {
		t.Run("account id/"+tc.name, func(t *testing.T) {
			svc, routeStore, _ := newVendorConnectTestService(t)
			fake := installFakeVendorValidators(svc)
			fake.set(kindOpenAISubscription, validCheck(tc.value, "plus"))
			acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "Imported")

			if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: "sk-not-a-jwt"}); err != nil {
				t.Fatalf("ConnectVendorAccountImport: %v", err)
			}
			_, ts := storedTokenSet(t, routeStore, svc, acc.ID)
			wantID, wantPlan := "", ""
			if tc.stored {
				wantID, wantPlan = tc.value, "plus"
			}
			if ts.AccountID != wantID || ts.PlanType != wantPlan {
				t.Fatalf("stored account id %d bytes / plan %q, want id %d bytes / plan %q", len(ts.AccountID), ts.PlanType, len(wantID), wantPlan)
			}
		})
		t.Run("plan type/"+tc.name, func(t *testing.T) {
			svc, routeStore, _ := newVendorConnectTestService(t)
			fake := installFakeVendorValidators(svc)
			fake.set(kindOpenAISubscription, validCheck("acct-ok", tc.value))
			acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "Imported")

			if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: "sk-not-a-jwt"}); err != nil {
				t.Fatalf("ConnectVendorAccountImport: %v", err)
			}
			_, ts := storedTokenSet(t, routeStore, svc, acc.ID)
			wantPlan := ""
			if tc.stored {
				wantPlan = tc.value
			}
			// A junk plan never costs the (valid) account id.
			if ts.AccountID != "acct-ok" || ts.PlanType != wantPlan {
				t.Fatalf("stored account id %q / plan %d bytes, want acct-ok / %d bytes", ts.AccountID, len(ts.PlanType), len(wantPlan))
			}
		})
	}
}

// --- import: never-refresh fix ---------------------------------------------

func TestConnectVendorAccountImportDerivesAMissingExpiryFromTheJWT(t *testing.T) {
	exp := time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)
	jwt := jwtWithClaims(t, map[string]any{"exp": exp.Unix(), "sub": "user-1"})

	for _, vendor := range []string{routing.VendorOpenAI, routing.VendorAnthropic} {
		t.Run(vendor, func(t *testing.T) {
			svc, routeStore, _ := newVendorConnectTestService(t)
			installFakeVendorValidators(svc)
			acc := createSubscriptionAccount(t, svc, ownerToken(), vendor, "Imported")

			if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: jwt, RefreshToken: connectTestRefresh}); err != nil {
				t.Fatalf("ConnectVendorAccountImport: %v", err)
			}
			_, ts := storedTokenSet(t, routeStore, svc, acc.ID)
			if !ts.ExpiresAt.Equal(exp) {
				t.Fatalf("ExpiresAt = %v, want the JWT exp %v", ts.ExpiresAt, exp)
			}
			if ts.RefreshToken != connectTestRefresh {
				t.Fatalf("RefreshToken = %q, want the pasted refresh token kept", ts.RefreshToken)
			}
			if !ts.NeedsRefresh(exp.Add(time.Minute), 0) || ts.NeedsRefresh(exp.Add(-time.Hour), time.Minute) {
				t.Fatal("the derived expiry must let NeedsRefresh fire once the token is stale")
			}
		})
	}
}

func TestConnectVendorAccountImportKeepsAnExplicitExpiryOverTheJWT(t *testing.T) {
	svc, routeStore, _ := newVendorConnectTestService(t)
	installFakeVendorValidators(svc)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "Imported")
	explicit := time.Date(2026, 10, 9, 1, 2, 3, 0, time.FixedZone("x", 3600))
	jwt := jwtWithClaims(t, map[string]any{"exp": time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC).Unix()})

	if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: jwt, ExpiresAt: explicit}); err != nil {
		t.Fatalf("ConnectVendorAccountImport: %v", err)
	}
	if _, ts := storedTokenSet(t, routeStore, svc, acc.ID); !ts.ExpiresAt.Equal(explicit) {
		t.Fatalf("ExpiresAt = %v, want the explicit %v", ts.ExpiresAt, explicit)
	}
}

// A token with no derivable expiry (opaque, or a JWT without a usable exp) keeps
// the unknown (zero) expiry rather than a made-up one.
func TestConnectVendorAccountImportLeavesAnUnderivableExpiryUnknown(t *testing.T) {
	tokens := map[string]string{
		"opaque anthropic-style token": connectTestAccess,
		"jwt without exp":              jwtWithClaims(t, map[string]any{"sub": "user-1"}),
		"jwt with a string exp":        jwtWithClaims(t, map[string]any{"exp": "tomorrow"}),
		"jwt with a negative exp":      jwtWithClaims(t, map[string]any{"exp": -5}),
	}
	for name, token := range tokens {
		t.Run(name, func(t *testing.T) {
			svc, routeStore, _ := newVendorConnectTestService(t)
			installFakeVendorValidators(svc)
			acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Imported")
			if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: token, RefreshToken: connectTestRefresh}); err != nil {
				t.Fatalf("ConnectVendorAccountImport: %v", err)
			}
			if _, ts := storedTokenSet(t, routeStore, svc, acc.ID); !ts.ExpiresAt.IsZero() {
				t.Fatalf("ExpiresAt = %v, want the unknown (zero) expiry", ts.ExpiresAt)
			}
		})
	}
}

// --- test connection --------------------------------------------------------

func TestTestVendorAccountConnectionAPIKey(t *testing.T) {
	cases := map[string]string{
		routing.VendorOpenAI:    kindOpenAIAPIKey,
		routing.VendorAnthropic: kindAnthropicAPIKey,
	}
	for vendor, wantKind := range cases {
		t.Run(vendor, func(t *testing.T) {
			svc, _, _ := newVendorConnectTestService(t)
			fake := installFakeVendorValidators(svc)
			fake.set(wantKind, validCheck("", ""))
			req := apiKeyAccountRequest("Key account")
			req.Vendor = vendor
			acc := createTestVendorAccount(t, svc, ownerToken(), req)

			got, err := svc.TestVendorAccountConnection(context.Background(), ownerToken(), acc.ID)
			if err != nil {
				t.Fatalf("TestVendorAccountConnection: %v", err)
			}
			if got.Status != "valid" || got.Detail != "credential accepted (HTTP 200)" || !got.CheckedAt.Equal(svc.clock()) {
				t.Fatalf("check = %+v, want valid with the validator's detail stamped with the clock", got)
			}
			call := fake.onlyCall(t)
			if call.kind != wantKind || call.credential != vendorAccountTestKey {
				t.Fatalf("call = %+v, want kind %s with the stored (opened) key", call, wantKind)
			}
			if !call.hasClient || call.timeout != 10*time.Second {
				t.Fatalf("call = %+v, want a client with a 10s timeout", call)
			}
		})
	}
}

func TestTestVendorAccountConnectionSubscription(t *testing.T) {
	cases := map[string]string{
		routing.VendorOpenAI:    kindOpenAISubscription,
		routing.VendorAnthropic: kindAnthropicSubscription,
	}
	for vendor, wantKind := range cases {
		t.Run(vendor, func(t *testing.T) {
			svc, _, _ := newVendorConnectTestService(t)
			fake := installFakeVendorValidators(svc)
			acc := createSubscriptionAccount(t, svc, ownerToken(), vendor, "Sub")
			if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess, RefreshToken: connectTestRefresh}); err != nil {
				t.Fatalf("ConnectVendorAccountImport: %v", err)
			}
			fake.reset()
			fake.set(wantKind, invalidCheck())

			got, err := svc.TestVendorAccountConnection(context.Background(), ownerToken(), acc.ID)
			if err != nil {
				t.Fatalf("TestVendorAccountConnection: %v", err)
			}
			if got.Status != "invalid" || got.Detail != "credential rejected (HTTP 401)" || !got.CheckedAt.Equal(svc.clock()) {
				t.Fatalf("check = %+v, want invalid with the validator's detail", got)
			}
			call := fake.onlyCall(t)
			if call.kind != wantKind || call.credential != connectTestAccess {
				t.Fatalf("call = %+v, want kind %s with the stored access token (not the refresh token)", call, wantKind)
			}
			if !call.hasClient || call.timeout != 10*time.Second {
				t.Fatalf("call = %+v, want a client with a 10s timeout", call)
			}
		})
	}
}

func TestTestVendorAccountConnectionMapsEveryVerdict(t *testing.T) {
	cases := []struct {
		name    string
		verdict vendorauth.CredentialCheck
		want    string
	}{
		{"valid", validCheck("", ""), "valid"},
		{"invalid", invalidCheck(), "invalid"},
		{"unverifiable", unverifiableCheck(), "unverifiable"},
		// A verdict that was never filled in is no statement about the credential.
		{"unknown zero value", vendorauth.CredentialCheck{Detail: "?"}, "unverifiable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _ := newVendorConnectTestService(t)
			fake := installFakeVendorValidators(svc)
			fake.setAll(tc.verdict)
			acc := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Key account"))

			got, err := svc.TestVendorAccountConnection(context.Background(), ownerToken(), acc.ID)
			if err != nil {
				t.Fatalf("TestVendorAccountConnection: %v", err)
			}
			if got.Status != tc.want {
				t.Fatalf("status = %q, want %q", got.Status, tc.want)
			}
		})
	}
}

// The result is token-free even when a validator misbehaves and echoes the
// credential into its Detail, and its JSON carries exactly the three fields.
func TestTestVendorAccountConnectionResultCarriesNoToken(t *testing.T) {
	svc, _, _ := newVendorConnectTestService(t)
	fake := installFakeVendorValidators(svc)
	fake.setAll(vendorauth.CredentialCheck{Status: vendorauth.StatusInvalid, Detail: "rejected: " + vendorAccountTestKey + " is not a key"})
	acc := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Key account"))

	got, err := svc.TestVendorAccountConnection(context.Background(), ownerToken(), acc.ID)
	if err != nil {
		t.Fatalf("TestVendorAccountConnection: %v", err)
	}
	if got.Status != "invalid" {
		t.Fatalf("status = %q, want invalid (the verdict survives the scrub)", got.Status)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), vendorAccountTestKey) {
		t.Fatalf("result leaks the credential: %s", raw)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 || fields["status"] == nil || fields["detail"] == nil || fields["checked_at"] == nil {
		t.Fatalf("result JSON = %s, want exactly status, detail and checked_at", raw)
	}
}

// Nothing to test is not a statement about the vendor: no probe, an unverifiable
// answer that says what is missing.
func TestTestVendorAccountConnectionWithoutACredentialIsUnverifiable(t *testing.T) {
	svc, _, _ := newVendorConnectTestService(t)
	fake := installFakeVendorValidators(svc)
	fake.setAll(validCheck("", ""))
	noKey := createTestVendorAccount(t, svc, ownerToken(), CreateVendorAccountRequest{Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey, Name: "No key yet"})
	unconnected := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Not connected")

	for name, id := range map[string]string{"api key not set": noKey.ID, "subscription not connected": unconnected.ID} {
		got, err := svc.TestVendorAccountConnection(context.Background(), ownerToken(), id)
		if err != nil {
			t.Fatalf("%s: TestVendorAccountConnection: %v", name, err)
		}
		if got.Status != "unverifiable" || got.Detail == "" || got.CheckedAt.IsZero() {
			t.Fatalf("%s: check = %+v, want unverifiable with a detail and a time", name, got)
		}
	}
	if calls := fake.recorded(); len(calls) != 0 {
		t.Fatalf("validator calls = %+v, want none without a credential", calls)
	}
}

// An access token that is past its expiry but can be refreshed is NOT probed: the
// vendor would answer 401 for a perfectly healthy account that the next request
// refreshes, and "invalid" would be a false alarm.
func TestTestVendorAccountConnectionDoesNotProbeAnExpiredButRefreshableToken(t *testing.T) {
	svc, _, _ := newVendorConnectTestService(t)
	fake := installFakeVendorValidators(svc)
	expired := svc.clock().Add(-time.Hour)

	refreshable := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Refreshable")
	if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), refreshable.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess, RefreshToken: connectTestRefresh, ExpiresAt: expired}); err != nil {
		t.Fatalf("import: %v", err)
	}
	deadEnd := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "No refresh token")
	if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), deadEnd.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess, ExpiresAt: expired}); err != nil {
		t.Fatalf("import: %v", err)
	}
	// A probe would reject the stale token, so the verdict must never be asked for.
	fake.setAll(invalidCheck())
	fake.reset()

	got, err := svc.TestVendorAccountConnection(context.Background(), ownerToken(), refreshable.ID)
	if err != nil {
		t.Fatalf("refreshable: %v", err)
	}
	if got.Status != "unverifiable" || got.Detail == "" {
		t.Fatalf("refreshable: check = %+v, want unverifiable explaining the pending refresh", got)
	}
	if calls := fake.recorded(); len(calls) != 0 {
		t.Fatalf("validator calls = %+v, want none for an expired but refreshable token", calls)
	}

	// Without a refresh token an expired token can never heal, so it is probed.
	got, err = svc.TestVendorAccountConnection(context.Background(), ownerToken(), deadEnd.ID)
	if err != nil {
		t.Fatalf("no refresh token: %v", err)
	}
	if got.Status != "invalid" || len(fake.recorded()) != 1 {
		t.Fatalf("no refresh token: check = %+v, calls = %+v, want one probe answering invalid", got, fake.recorded())
	}
}

func TestTestVendorAccountConnectionIsOwnerOnly(t *testing.T) {
	svc, _, _ := newVendorConnectTestService(t)
	fake := installFakeVendorValidators(svc)
	fake.setAll(validCheck("", ""))
	acc := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Key account"))

	for label, principal := range map[string]auth.Token{
		"another user":       otherToken(),
		"no user identity":   {},
		"another user (adm)": {UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}},
	} {
		if _, err := svc.TestVendorAccountConnection(context.Background(), principal, acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
			t.Fatalf("%s: err = %v, want ErrVendorAccountNotFound", label, err)
		}
	}
	if _, err := svc.TestVendorAccountConnection(context.Background(), ownerToken(), "va_missing"); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("unknown id: err = %v, want ErrVendorAccountNotFound", err)
	}
	if calls := fake.recorded(); len(calls) != 0 {
		t.Fatalf("validator calls = %+v, want a refused principal never to reach the vendor", calls)
	}
}

func TestTestVendorAccountConnectionRefusesWhileTheMasterFlagIsOff(t *testing.T) {
	svc, _, _ := newVendorConnectTestService(t)
	fake := installFakeVendorValidators(svc)
	acc := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Key account"))
	setVendorAccountsEnabled(t, svc, false)

	if _, err := svc.TestVendorAccountConnection(context.Background(), ownerToken(), acc.ID); !errors.Is(err, ErrVendorAccountsDisabled) {
		t.Fatalf("err = %v, want ErrVendorAccountsDisabled", err)
	}
	if _, err := svc.TestVendorAccountConnection(context.Background(), ownerToken(), "va_missing"); !errors.Is(err, ErrVendorAccountsDisabled) {
		t.Fatalf("unknown id: err = %v, want ErrVendorAccountsDisabled (a disabled area confirms nothing)", err)
	}
	if calls := fake.recorded(); len(calls) != 0 {
		t.Fatalf("validator calls = %+v, want none while disabled", calls)
	}
}

// --- wiring -----------------------------------------------------------------

func TestNewServiceDefaultsAndInjectsTheVendorValidators(t *testing.T) {
	defaults := NewService(ServiceDeps{})
	v := defaults.vendorValidation.validators
	if v.OpenAISubscription == nil || v.AnthropicSubscription == nil || v.OpenAIAPIKey == nil || v.AnthropicAPIKey == nil {
		t.Fatalf("default validators = %+v, want every probe defaulted to its vendorauth function", v)
	}
	if c := defaults.vendorValidation.client; c == nil || c.Timeout != 10*time.Second {
		t.Fatalf("default validation client = %+v, want one with a 10s timeout", c)
	}

	// A partial injection replaces only the named probes.
	called := false
	injected := NewService(ServiceDeps{VendorValidators: VendorCredentialValidators{
		OpenAIAPIKey: func(context.Context, *http.Client, string) vendorauth.CredentialCheck {
			called = true
			return validCheck("", "")
		},
	}})
	iv := injected.vendorValidation.validators
	if iv.AnthropicAPIKey == nil || iv.OpenAISubscription == nil || iv.AnthropicSubscription == nil {
		t.Fatalf("injected validators = %+v, want the unnamed probes defaulted", iv)
	}
	if got := iv.OpenAIAPIKey(context.Background(), nil, "k"); got.Status != vendorauth.StatusValid || !called {
		t.Fatal("the injected OpenAIAPIKey probe was not used")
	}
}
