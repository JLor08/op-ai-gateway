// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const vendorAccountTestKey = "sk-live-do-not-echo-123"

// newVendorAccountTestService is a memory-store-backed Service with NO cipher
// and a volatile (in-memory) settings store, so the secret seal takes the
// "plain:" path -- the same wiring the RAM-mode gateway uses.
func newVendorAccountTestService(t *testing.T, now time.Time) (*Service, *routing.MemoryStore) {
	t.Helper()
	return newVendorAccountTestServiceWithCipher(t, now, nil, true)
}

// newVendorAccountTestServiceWithCipher is newServerTestServiceWithCipher with
// the vendor_accounts_enabled master flag switched ON: every vendor-account
// service method is refused while it is off (ErrVendorAccountsDisabled), so the
// tests of the methods' own behaviour run with the area enabled. The credential
// validators and the model-discovery fetchers are replaced by all-Unverifiable
// fakes, so no test of the area can reach a vendor over the network; a test that
// cares about the verdicts or the discovered models installs its own with
// installFakeVendorValidators / installFakeVendorDiscoverers.
func newVendorAccountTestServiceWithCipher(t *testing.T, now time.Time, cipher *capture.Cipher, volatile bool) (*Service, *routing.MemoryStore) {
	t.Helper()
	svc, routeStore := newServerTestServiceWithCipher(t, now, cipher, volatile)
	setVendorAccountsEnabled(t, svc, true)
	installFakeVendorValidators(svc)
	installFakeVendorDiscoverers(svc)
	return svc, routeStore
}

// setVendorAccountsEnabled gives svc a fresh in-memory settings store (the
// shared test constructor wires none) holding the master flag.
func setVendorAccountsEnabled(t *testing.T, svc *Service, enabled bool) {
	t.Helper()
	settings := NewMemorySystemSettings()
	if err := settings.SetSystemSetting(context.Background(), vendorAccountsEnabledKey, strconv.FormatBool(enabled), time.Time{}); err != nil {
		t.Fatalf("SetSystemSetting: %v", err)
	}
	svc.settings = settings
}

func createTestVendorAccount(t *testing.T, svc *Service, principal auth.Token, req CreateVendorAccountRequest) VendorAccountDTO {
	t.Helper()
	dto, err := svc.CreateVendorAccount(context.Background(), principal, req)
	if err != nil {
		t.Fatalf("CreateVendorAccount: %v", err)
	}
	return dto
}

func apiKeyAccountRequest(name string) CreateVendorAccountRequest {
	return CreateVendorAccountRequest{
		Vendor:   routing.VendorOpenAI,
		AuthType: routing.VendorAuthAPIKey,
		Name:     name,
		APIKey:   vendorAccountTestKey,
	}
}

func TestCreateVendorAccountAPIKeyRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)

	dto := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("  My OpenAI  "))

	if !strings.HasPrefix(dto.ID, "va_") || len(dto.ID) != len("va_")+32 {
		t.Fatalf("id = %q, want va_ + 32 hex chars", dto.ID)
	}
	if dto.Vendor != routing.VendorOpenAI || dto.AuthType != routing.VendorAuthAPIKey {
		t.Fatalf("vendor/auth_type = %q/%q", dto.Vendor, dto.AuthType)
	}
	if dto.Name != "My OpenAI" {
		t.Fatalf("name = %q, want trimmed", dto.Name)
	}
	if dto.Status != routing.VendorAccountStatusActive {
		t.Fatalf("status default = %q, want active", dto.Status)
	}
	if !dto.APIKeySet || dto.SubscriptionConnected {
		t.Fatalf("api_key_set/subscription_connected = %v/%v, want true/false", dto.APIKeySet, dto.SubscriptionConnected)
	}
	wantModels := vendorAccountModelDTOs(VendorCatalog(routing.VendorOpenAI, routing.VendorAuthAPIKey))
	if !reflect.DeepEqual(dto.Models, wantModels) {
		t.Fatalf("models = %#v, want the OpenAI catalog %#v", dto.Models, wantModels)
	}
	if !dto.CreatedAt.Equal(now) || !dto.UpdatedAt.Equal(now) {
		t.Fatalf("created/updated = %v/%v, want %v", dto.CreatedAt, dto.UpdatedAt, now)
	}

	// The key never leaves the service: not in the DTO, and sealed at rest.
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal dto: %v", err)
	}
	if strings.Contains(string(raw), vendorAccountTestKey) {
		t.Fatalf("dto JSON leaks the api key: %s", raw)
	}
	for _, field := range []string{`"api_key_set":true`, `"subscription_connected":false`, `"api_flavor":"openai"`, `"auth_type":"api_key"`} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("dto JSON %s missing %s", raw, field)
		}
	}
	for _, leaked := range []string{`"api_key":`, `"oauth_tokens":`, `"owner_user_id":`} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("dto JSON %s must not expose %s", raw, leaked)
		}
	}
	row, err := routeStore.VendorAccountByID(context.Background(), dto.ID)
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	if row.OwnerUserID != "usr_owner" {
		t.Fatalf("owner = %q, want the creating principal", row.OwnerUserID)
	}
	if row.APIKey != "plain:"+vendorAccountTestKey {
		t.Fatalf("stored api key = %q, want the sealed plain: envelope", row.APIKey)
	}
	if row.OAuthTokens != "" {
		t.Fatalf("oauth tokens = %q, want empty for an api_key account", row.OAuthTokens)
	}
}

func TestCreateVendorAccountSealsWithCipher(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	cipher := newTestCipher(t)
	svc, routeStore := newVendorAccountTestServiceWithCipher(t, now, cipher, false)

	dto := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Sealed"))

	row, err := routeStore.VendorAccountByID(context.Background(), dto.ID)
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	if !strings.HasPrefix(row.APIKey, "enc:") || strings.Contains(row.APIKey, vendorAccountTestKey) {
		t.Fatalf("stored api key = %q, want an enc: envelope", row.APIKey)
	}
	opened, err := capture.OpenSecret(cipher, row.APIKey)
	if err != nil || opened != vendorAccountTestKey {
		t.Fatalf("OpenSecret = %q, %v, want the original key", opened, err)
	}
}

// A disk-backed store with no cipher must refuse the write instead of ever
// persisting a plaintext key; nothing is written.
func TestCreateVendorAccountKeylessDiskStoreRefuses(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestServiceWithCipher(t, now, nil, false)

	_, err := svc.CreateVendorAccount(context.Background(), ownerToken(), apiKeyAccountRequest("Keyless"))
	if !errors.Is(err, capture.ErrKeyRequired) {
		t.Fatalf("err = %v, want capture.ErrKeyRequired", err)
	}
	if rows, _ := routeStore.VendorAccounts(context.Background()); len(rows) != 0 {
		t.Fatalf("rows = %#v, want none persisted", rows)
	}
}

// Subscription accounts are valid in this milestone: they simply carry no
// tokens yet (the OAuth connect flow is a later milestone).
func TestCreateVendorAccountSubscriptionIsUnconnected(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, _ := newVendorAccountTestService(t, now)

	dto := createTestVendorAccount(t, svc, ownerToken(), CreateVendorAccountRequest{
		Vendor: routing.VendorAnthropic, AuthType: routing.VendorAuthSubscription, Name: "Claude Max",
	})
	if dto.AuthType != routing.VendorAuthSubscription || dto.Vendor != routing.VendorAnthropic {
		t.Fatalf("dto = %#v", dto)
	}
	if dto.APIKeySet || dto.SubscriptionConnected {
		t.Fatalf("api_key_set/subscription_connected = %v/%v, want false/false", dto.APIKeySet, dto.SubscriptionConnected)
	}
}

func TestCreateVendorAccountValidation(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)

	cases := []struct {
		name      string
		principal auth.Token
		mutate    func(*CreateVendorAccountRequest)
		want      error
	}{
		{"blank name", ownerToken(), func(r *CreateVendorAccountRequest) { r.Name = "   " }, ErrVendorAccountNameRequired},
		{"unknown vendor", ownerToken(), func(r *CreateVendorAccountRequest) { r.Vendor = "mistral" }, ErrVendorAccountVendorInvalid},
		{"empty vendor", ownerToken(), func(r *CreateVendorAccountRequest) { r.Vendor = "" }, ErrVendorAccountVendorInvalid},
		{"unknown auth type", ownerToken(), func(r *CreateVendorAccountRequest) { r.AuthType = "password" }, ErrVendorAccountAuthTypeInvalid},
		{"empty auth type", ownerToken(), func(r *CreateVendorAccountRequest) { r.AuthType = "" }, ErrVendorAccountAuthTypeInvalid},
		{"unknown status", ownerToken(), func(r *CreateVendorAccountRequest) { r.Status = "paused" }, ErrVendorAccountStatusInvalid},
		{"needs_reconnect is system-managed", ownerToken(), func(r *CreateVendorAccountRequest) { r.Status = routing.VendorAccountStatusNeedsReconnect }, ErrVendorAccountStatusInvalid},
		{"api key on a subscription account", ownerToken(), func(r *CreateVendorAccountRequest) {
			r.AuthType = routing.VendorAuthSubscription
		}, ErrVendorAccountAPIKeyNotAllowed},
		{"principal without a user", auth.Token{Scopes: []string{"gateway:use"}}, func(*CreateVendorAccountRequest) {}, ErrVendorAccountForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := apiKeyAccountRequest("Acct")
			tc.mutate(&req)
			_, err := svc.CreateVendorAccount(context.Background(), tc.principal, req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if rows, _ := routeStore.VendorAccounts(context.Background()); len(rows) != 0 {
		t.Fatalf("rows = %#v, want none persisted after rejected creates", rows)
	}
}

func TestCreateVendorAccountDisabledStatus(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, _ := newVendorAccountTestService(t, now)

	req := apiKeyAccountRequest("Parked")
	req.Status = routing.VendorAccountStatusDisabled
	if dto := createTestVendorAccount(t, svc, ownerToken(), req); dto.Status != routing.VendorAccountStatusDisabled {
		t.Fatalf("status = %q, want disabled", dto.Status)
	}
}

// Every authenticated principal manages only their OWN accounts: a non-owner
// gets the same ErrVendorAccountNotFound as for an unknown id (404-no-leak),
// on every read and write path. System scope may read everything.
func TestVendorAccountAuthorizationIsOwnerOnly(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	owned := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Owner's"))
	ctx := context.Background()

	if _, err := svc.GetVendorAccount(ctx, otherToken(), owned.ID); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("non-owner Get err = %v, want ErrVendorAccountNotFound", err)
	}
	if _, err := svc.GetVendorAccount(ctx, ownerToken(), "va_missing"); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("unknown Get err = %v, want ErrVendorAccountNotFound", err)
	}
	newName := "Hijacked"
	if _, err := svc.UpdateVendorAccount(ctx, otherToken(), owned.ID, UpdateVendorAccountRequest{Name: &newName}); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("non-owner Update err = %v, want ErrVendorAccountNotFound", err)
	}
	if _, err := svc.DeleteVendorAccount(ctx, otherToken(), owned.ID); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("non-owner Delete err = %v, want ErrVendorAccountNotFound", err)
	}
	// An admin who is not the owner is a stranger too (no admin bypass).
	if _, err := svc.GetVendorAccount(ctx, adminToken(), owned.ID); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("admin non-owner Get err = %v, want ErrVendorAccountNotFound", err)
	}
	row, err := routeStore.VendorAccountByID(ctx, owned.ID)
	if err != nil || row.Name != "Owner's" {
		t.Fatalf("row after rejected writes = %#v, %v, want it untouched", row, err)
	}

	if got, err := svc.GetVendorAccount(ctx, ownerToken(), owned.ID); err != nil || got.ID != owned.ID {
		t.Fatalf("owner Get = %#v, %v", got, err)
	}
	if got, err := svc.GetVendorAccount(ctx, systemToken(), owned.ID); err != nil || got.ID != owned.ID {
		t.Fatalf("system Get = %#v, %v, want read access to every account", got, err)
	}
}

// A detail read carries the scraped rate-limit snapshot when the gateway has
// seen one, and nothing (a nil Usage, no "usage" key) while it has not -- the
// portal then simply hides the Usage & Limits panel.
func TestGetVendorAccountIncludesTheUsageSnapshotWhenOneExists(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	ctx := context.Background()
	acc := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Work"))

	// No snapshot yet: Usage is nil and is left out of the JSON entirely.
	before, err := svc.GetVendorAccount(ctx, ownerToken(), acc.ID)
	if err != nil {
		t.Fatalf("GetVendorAccount: %v", err)
	}
	if before.Usage != nil {
		t.Fatalf("Usage before any snapshot = %#v, want nil", before.Usage)
	}
	rawBefore, err := json.Marshal(before)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var beforeKeys map[string]json.RawMessage
	if err := json.Unmarshal(rawBefore, &beforeKeys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := beforeKeys["usage"]; present {
		t.Fatalf("a snapshot-less account serialized a usage key: %s", rawBefore)
	}

	fiveHourReset := now.Add(2*time.Hour + 14*time.Minute)
	weeklyReset := now.Add(3 * 24 * time.Hour)
	snapshotAt := now.Add(-time.Minute)
	if err := routeStore.UpsertVendorAccountUsage(ctx, routing.VendorAccountUsage{
		AccountID:       acc.ID,
		FiveHourPct:     42.5,
		FiveHourResetAt: &fiveHourReset,
		WeeklyPct:       7,
		WeeklyResetAt:   &weeklyReset,
		CreditBalance:   "12.34",
		UpdatedAt:       snapshotAt,
	}); err != nil {
		t.Fatalf("UpsertVendorAccountUsage: %v", err)
	}

	got, err := svc.GetVendorAccount(ctx, ownerToken(), acc.ID)
	if err != nil {
		t.Fatalf("GetVendorAccount: %v", err)
	}
	if got.Usage == nil {
		t.Fatal("Usage = nil, want the stored snapshot")
	}
	if got.Usage.FiveHourPct != 42.5 || got.Usage.WeeklyPct != 7 || got.Usage.CreditBalance != "12.34" {
		t.Fatalf("Usage = %#v, want 42.5 / 7 / 12.34", got.Usage)
	}
	if got.Usage.FiveHourResetAt == nil || !got.Usage.FiveHourResetAt.Equal(fiveHourReset) ||
		got.Usage.WeeklyResetAt == nil || !got.Usage.WeeklyResetAt.Equal(weeklyReset) {
		t.Fatalf("reset times = %v / %v, want %v / %v", got.Usage.FiveHourResetAt, got.Usage.WeeklyResetAt, fiveHourReset, weeklyReset)
	}
	if !got.Usage.UpdatedAt.Equal(snapshotAt) {
		t.Fatalf("Usage.UpdatedAt = %v, want %v", got.Usage.UpdatedAt, snapshotAt)
	}

	// The wire shape the portal reads (snake_case, nested under "usage").
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"five_hour_pct", "five_hour_reset_at", "weekly_pct", "weekly_reset_at", "credit_balance", "updated_at"} {
		if _, ok := wire.Usage[key]; !ok {
			t.Fatalf("usage JSON lacks %q: %s", key, raw)
		}
	}
	if wire.Usage["five_hour_pct"] != 42.5 || wire.Usage["credit_balance"] != "12.34" {
		t.Fatalf("usage JSON = %v", wire.Usage)
	}

	// The list endpoint stays snapshot-free: reading one row per account would
	// be an N+1 on the list, and the panel lives on the detail view only.
	list, err := svc.ListVendorAccounts(ctx, ownerToken())
	if err != nil || len(list.Data) != 1 {
		t.Fatalf("ListVendorAccounts = %#v, %v", list, err)
	}
	if list.Data[0].Usage != nil {
		t.Fatalf("list Usage = %#v, want nil (detail-only)", list.Data[0].Usage)
	}

	// A system-scope operator reads the same snapshot with the account.
	sys, err := svc.GetVendorAccount(ctx, systemToken(), acc.ID)
	if err != nil || sys.Usage == nil || sys.Usage.FiveHourPct != 42.5 {
		t.Fatalf("system Get = %#v, %v, want the snapshot", sys, err)
	}
}

// -1 (unknown) and a nil reset are passed through as-is: the DTO never turns an
// unknown window into a real 0 %, so the portal can tell "no data yet" from "0 % used".
func TestGetVendorAccountUsagePassesUnknownWindowsThrough(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	ctx := context.Background()
	acc := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Work"))
	if err := routeStore.UpsertVendorAccountUsage(ctx, routing.VendorAccountUsage{
		AccountID:   acc.ID,
		FiveHourPct: 0,
		WeeklyPct:   -1,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("UpsertVendorAccountUsage: %v", err)
	}

	got, err := svc.GetVendorAccount(ctx, ownerToken(), acc.ID)
	if err != nil || got.Usage == nil {
		t.Fatalf("GetVendorAccount = %#v, %v, want a usage snapshot", got, err)
	}
	if got.Usage.FiveHourPct != 0 || got.Usage.WeeklyPct != -1 {
		t.Fatalf("pcts = %v / %v, want 0 / -1", got.Usage.FiveHourPct, got.Usage.WeeklyPct)
	}
	if got.Usage.FiveHourResetAt != nil || got.Usage.WeeklyResetAt != nil || got.Usage.CreditBalance != "" {
		t.Fatalf("Usage = %#v, want nil resets and no credit balance", got.Usage)
	}
	raw, _ := json.Marshal(got)
	var wire struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := wire.Usage["five_hour_reset_at"]; !ok || v != nil {
		t.Fatalf("five_hour_reset_at = %v (present %v), want an explicit null", v, ok)
	}
}

// Another user's account stays a 404-no-leak, snapshot or not.
func TestGetVendorAccountUsageStaysBehindTheOwnerCheck(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	ctx := context.Background()
	acc := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Work"))
	if err := routeStore.UpsertVendorAccountUsage(ctx, routing.VendorAccountUsage{AccountID: acc.ID, FiveHourPct: 10, WeeklyPct: 20, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertVendorAccountUsage: %v", err)
	}
	if _, err := svc.GetVendorAccount(ctx, otherToken(), acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("stranger Get err = %v, want ErrVendorAccountNotFound", err)
	}
}

func TestListVendorAccountsReturnsOnlyThePrincipalsOwn(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, _ := newVendorAccountTestService(t, now)
	a := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("A"))
	b := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("B"))
	createTestVendorAccount(t, svc, otherToken(), apiKeyAccountRequest("Someone else's"))
	ctx := context.Background()

	list, err := svc.ListVendorAccounts(ctx, ownerToken())
	if err != nil {
		t.Fatalf("ListVendorAccounts: %v", err)
	}
	if len(list.Data) != 2 {
		t.Fatalf("owner list = %#v, want exactly the two own accounts", list.Data)
	}
	got := map[string]bool{list.Data[0].ID: true, list.Data[1].ID: true}
	if !got[a.ID] || !got[b.ID] {
		t.Fatalf("owner list ids = %v, want %s and %s", got, a.ID, b.ID)
	}
	for _, dto := range list.Data {
		if len(dto.Models) == 0 {
			t.Fatalf("list entry %s has no models, want the seeded catalog", dto.ID)
		}
	}

	empty, err := svc.ListVendorAccounts(ctx, auth.Token{UserID: "usr_admin", Scopes: []string{"gateway:use"}})
	if err != nil || empty.Data == nil || len(empty.Data) != 0 {
		t.Fatalf("empty list = %#v, %v, want a non-nil empty Data slice", empty, err)
	}
}

func TestUpdateVendorAccountRenameStatusAndKeySemantics(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	created := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Original"))
	ctx := context.Background()
	later := now.Add(time.Hour)
	svc.clock = func() time.Time { return later }

	stored := func() routing.VendorAccount {
		t.Helper()
		row, err := routeStore.VendorAccountByID(ctx, created.ID)
		if err != nil {
			t.Fatalf("VendorAccountByID: %v", err)
		}
		return row
	}

	// Rename only: api_key nil keeps the sealed key untouched.
	renamed := "  Renamed  "
	dto, err := svc.UpdateVendorAccount(ctx, ownerToken(), created.ID, UpdateVendorAccountRequest{Name: &renamed})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if dto.Name != "Renamed" || !dto.APIKeySet || dto.Status != routing.VendorAccountStatusActive {
		t.Fatalf("after rename dto = %#v", dto)
	}
	if !dto.CreatedAt.Equal(now) || !dto.UpdatedAt.Equal(later) {
		t.Fatalf("created/updated = %v/%v, want %v/%v", dto.CreatedAt, dto.UpdatedAt, now, later)
	}
	row := stored()
	if row.APIKey != "plain:"+vendorAccountTestKey {
		t.Fatalf("api key after rename = %q, want kept", row.APIKey)
	}
	if row.OwnerUserID != "usr_owner" || row.Vendor != routing.VendorOpenAI || row.AuthType != routing.VendorAuthAPIKey {
		t.Fatalf("immutable fields changed: %#v", row)
	}

	// Status toggle, key still kept.
	disabled := routing.VendorAccountStatusDisabled
	dto, err = svc.UpdateVendorAccount(ctx, ownerToken(), created.ID, UpdateVendorAccountRequest{Status: &disabled})
	if err != nil || dto.Status != routing.VendorAccountStatusDisabled || dto.Name != "Renamed" || !dto.APIKeySet {
		t.Fatalf("disable = %#v, %v", dto, err)
	}

	// Replace the key with a new value.
	replacement := "sk-rotated-456"
	dto, err = svc.UpdateVendorAccount(ctx, ownerToken(), created.ID, UpdateVendorAccountRequest{APIKey: &replacement})
	if err != nil || !dto.APIKeySet {
		t.Fatalf("replace key = %#v, %v", dto, err)
	}
	if got := stored().APIKey; got != "plain:"+replacement {
		t.Fatalf("api key after replace = %q, want the new sealed value", got)
	}
	raw, _ := json.Marshal(dto)
	if strings.Contains(string(raw), replacement) {
		t.Fatalf("dto JSON leaks the replacement key: %s", raw)
	}

	// An empty string clears the key.
	cleared := ""
	dto, err = svc.UpdateVendorAccount(ctx, ownerToken(), created.ID, UpdateVendorAccountRequest{APIKey: &cleared})
	if err != nil || dto.APIKeySet {
		t.Fatalf("clear key = %#v, %v, want api_key_set=false", dto, err)
	}
	if got := stored().APIKey; got != "" {
		t.Fatalf("api key after clear = %q, want empty", got)
	}

	// An empty request is a no-op that still succeeds.
	if _, err := svc.UpdateVendorAccount(ctx, ownerToken(), created.ID, UpdateVendorAccountRequest{}); err != nil {
		t.Fatalf("empty update: %v", err)
	}
}

func TestUpdateVendorAccountValidation(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	apiKeyAcc := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Keyed"))
	subAcc := createTestVendorAccount(t, svc, ownerToken(), CreateVendorAccountRequest{
		Vendor: routing.VendorAnthropic, AuthType: routing.VendorAuthSubscription, Name: "Sub",
	})
	ctx := context.Background()
	str := func(s string) *string { return &s }

	if _, err := svc.UpdateVendorAccount(ctx, ownerToken(), apiKeyAcc.ID, UpdateVendorAccountRequest{Name: str("   ")}); !errors.Is(err, ErrVendorAccountNameRequired) {
		t.Fatalf("blank name err = %v, want ErrVendorAccountNameRequired", err)
	}
	if _, err := svc.UpdateVendorAccount(ctx, ownerToken(), apiKeyAcc.ID, UpdateVendorAccountRequest{Status: str("paused")}); !errors.Is(err, ErrVendorAccountStatusInvalid) {
		t.Fatalf("bad status err = %v, want ErrVendorAccountStatusInvalid", err)
	}
	if _, err := svc.UpdateVendorAccount(ctx, ownerToken(), apiKeyAcc.ID, UpdateVendorAccountRequest{Status: str(routing.VendorAccountStatusNeedsReconnect)}); !errors.Is(err, ErrVendorAccountStatusInvalid) {
		t.Fatalf("user-set needs_reconnect err = %v, want ErrVendorAccountStatusInvalid", err)
	}
	if _, err := svc.UpdateVendorAccount(ctx, ownerToken(), subAcc.ID, UpdateVendorAccountRequest{APIKey: str("sk-nope")}); !errors.Is(err, ErrVendorAccountAPIKeyNotAllowed) {
		t.Fatalf("api key on subscription err = %v, want ErrVendorAccountAPIKeyNotAllowed", err)
	}
	// Clearing a key on a subscription account is a harmless no-op, not an error.
	if _, err := svc.UpdateVendorAccount(ctx, ownerToken(), subAcc.ID, UpdateVendorAccountRequest{APIKey: str("")}); err != nil {
		t.Fatalf("clear key on subscription: %v", err)
	}
	if _, err := svc.UpdateVendorAccount(ctx, ownerToken(), "va_missing", UpdateVendorAccountRequest{Name: str("x")}); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("unknown id err = %v, want ErrVendorAccountNotFound", err)
	}
	row, err := routeStore.VendorAccountByID(ctx, apiKeyAcc.ID)
	if err != nil || row.Name != "Keyed" || row.Status != routing.VendorAccountStatusActive {
		t.Fatalf("row after rejected updates = %#v, %v, want untouched", row, err)
	}
}

// needs_reconnect is set by the system when a subscription's refresh fails. A
// user round-tripping that unchanged status (a form PATCH that echoes it back)
// must not be rejected, and may move the account to active/disabled.
func TestUpdateVendorAccountKeepsSystemManagedStatus(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	ctx := context.Background()
	if err := routeStore.CreateVendorAccount(ctx, routing.VendorAccount{
		ID: "va_stale", OwnerUserID: "usr_owner", Vendor: routing.VendorAnthropic, AuthType: routing.VendorAuthSubscription,
		Name: "Stale", Status: routing.VendorAccountStatusNeedsReconnect, OAuthTokens: "plain:{}", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	same := routing.VendorAccountStatusNeedsReconnect
	name := "Stale renamed"
	dto, err := svc.UpdateVendorAccount(ctx, ownerToken(), "va_stale", UpdateVendorAccountRequest{Name: &name, Status: &same})
	if err != nil {
		t.Fatalf("echoing the unchanged status: %v", err)
	}
	if dto.Status != routing.VendorAccountStatusNeedsReconnect || !dto.SubscriptionConnected || dto.APIKeySet {
		t.Fatalf("dto = %#v, want needs_reconnect + subscription_connected", dto)
	}
	disabled := routing.VendorAccountStatusDisabled
	if dto, err = svc.UpdateVendorAccount(ctx, ownerToken(), "va_stale", UpdateVendorAccountRequest{Status: &disabled}); err != nil || dto.Status != routing.VendorAccountStatusDisabled {
		t.Fatalf("move to disabled = %#v, %v", dto, err)
	}
}

func TestDeleteVendorAccountRemovesTheRow(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	acc := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Doomed"))
	keep := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Survivor"))
	ctx := context.Background()

	deleted, err := svc.DeleteVendorAccount(ctx, ownerToken(), acc.ID)
	if err != nil || !deleted {
		t.Fatalf("DeleteVendorAccount = %v, %v, want true, nil", deleted, err)
	}
	if _, err := routeStore.VendorAccountByID(ctx, acc.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("row after delete err = %v, want store.ErrNotFound", err)
	}
	if _, err := svc.GetVendorAccount(ctx, ownerToken(), acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("Get after delete err = %v, want ErrVendorAccountNotFound", err)
	}
	if _, err := svc.DeleteVendorAccount(ctx, ownerToken(), acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("second delete err = %v, want ErrVendorAccountNotFound", err)
	}
	if _, err := routeStore.VendorAccountByID(ctx, keep.ID); err != nil {
		t.Fatalf("sibling account gone: %v", err)
	}
}

// A vendor account is a PERSONAL credential, unlike shared infra such as a
// server: system scope may READ any account (Get) but must not WRITE another
// user's -- no rotating/clearing their key, renaming/disabling, or deleting.
// A system-scope non-owner gets the same ErrVendorAccountNotFound as any other
// stranger, and the row (including its sealed key) is left exactly as it was.
func TestVendorAccountWritesAreOwnerOnlyEvenForSystemScope(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	owned := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Owner's"))
	ctx := context.Background()
	before, err := routeStore.VendorAccountByID(ctx, owned.ID)
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	str := func(s string) *string { return &s }

	for name, req := range map[string]UpdateVendorAccountRequest{
		"rename":     {Name: str("Hijacked")},
		"disable":    {Status: str(routing.VendorAccountStatusDisabled)},
		"rotate key": {APIKey: str("sk-attacker-key")},
		"clear key":  {APIKey: str("")},
	} {
		if _, err := svc.UpdateVendorAccount(ctx, systemToken(), owned.ID, req); !errors.Is(err, ErrVendorAccountNotFound) {
			t.Fatalf("system %s err = %v, want ErrVendorAccountNotFound", name, err)
		}
	}
	if deleted, err := svc.DeleteVendorAccount(ctx, systemToken(), owned.ID); !errors.Is(err, ErrVendorAccountNotFound) || deleted {
		t.Fatalf("system Delete = %v, %v, want false + ErrVendorAccountNotFound", deleted, err)
	}

	after, err := routeStore.VendorAccountByID(ctx, owned.ID)
	if err != nil {
		t.Fatalf("row after rejected system writes: %v", err)
	}
	if after != before {
		t.Fatalf("row changed by rejected system writes:\n before %#v\n after  %#v", before, after)
	}
	// System read access is unchanged, and the owner can still write.
	if got, err := svc.GetVendorAccount(ctx, systemToken(), owned.ID); err != nil || got.ID != owned.ID {
		t.Fatalf("system Get = %#v, %v, want read access kept", got, err)
	}
	if _, err := svc.UpdateVendorAccount(ctx, ownerToken(), owned.ID, UpdateVendorAccountRequest{Name: str("Mine")}); err != nil {
		t.Fatalf("owner Update: %v", err)
	}
}

// Authorization runs before body validation: a stranger (or an unknown id)
// sending an INVALID update must see the 404 -- never a 400 that would confirm
// the account exists.
func TestUpdateVendorAccountStrangerWithInvalidBodyGets404(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, _ := newVendorAccountTestService(t, now)
	owned := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Owner's"))
	str := func(s string) *string { return &s }

	invalid := map[string]UpdateVendorAccountRequest{
		"blank name":          {Name: str("   ")},
		"unknown status":      {Status: str("paused")},
		"needs_reconnect":     {Status: str(routing.VendorAccountStatusNeedsReconnect)},
		"whitespace-only key": {APIKey: str("   ")},
	}
	for name, req := range invalid {
		for who, principal := range map[string]auth.Token{"stranger": otherToken(), "system": systemToken()} {
			if _, err := svc.UpdateVendorAccount(context.Background(), principal, owned.ID, req); !errors.Is(err, ErrVendorAccountNotFound) {
				t.Fatalf("%s with %s err = %v, want ErrVendorAccountNotFound (authz before validation)", who, name, err)
			}
		}
		if _, err := svc.UpdateVendorAccount(context.Background(), ownerToken(), "va_missing", req); !errors.Is(err, ErrVendorAccountNotFound) {
			t.Fatalf("unknown id with %s err = %v, want ErrVendorAccountNotFound", name, err)
		}
	}
}

// Only an explicit "" clears the api key. A whitespace-only value must not be
// silently turned into a clear (it is almost certainly a paste slip), and must
// leave the stored key alone.
func TestUpdateVendorAccountWhitespaceOnlyKeyIsRejectedNotCleared(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	created := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Keyed"))
	ctx := context.Background()

	for _, blank := range []string{" ", "   ", "\t", "\n", " \r\n "} {
		blank := blank
		if _, err := svc.UpdateVendorAccount(ctx, ownerToken(), created.ID, UpdateVendorAccountRequest{APIKey: &blank}); !errors.Is(err, ErrVendorAccountAPIKeyInvalid) {
			t.Fatalf("api_key %q err = %v, want ErrVendorAccountAPIKeyInvalid", blank, err)
		}
	}
	row, err := routeStore.VendorAccountByID(ctx, created.ID)
	if err != nil || row.APIKey != "plain:"+vendorAccountTestKey {
		t.Fatalf("stored key after rejected whitespace updates = %q, %v, want kept", row.APIKey, err)
	}
	// The exact empty string still clears; nil still keeps.
	if _, err := svc.UpdateVendorAccount(ctx, ownerToken(), created.ID, UpdateVendorAccountRequest{}); err != nil {
		t.Fatalf("nil api_key: %v", err)
	}
	if row, _ := routeStore.VendorAccountByID(ctx, created.ID); row.APIKey == "" {
		t.Fatalf("nil api_key cleared the key")
	}
	empty := ""
	if dto, err := svc.UpdateVendorAccount(ctx, ownerToken(), created.ID, UpdateVendorAccountRequest{APIKey: &empty}); err != nil || dto.APIKeySet {
		t.Fatalf("explicit empty api_key = %#v, %v, want cleared", dto, err)
	}
}

// The seal happens on the UPDATE path too: a disk-backed store with no cipher
// refuses a replacement key with capture.ErrKeyRequired and writes nothing
// (mirrors the create-path assertion).
func TestUpdateVendorAccountKeylessDiskStoreRefusesReplacementKey(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestServiceWithCipher(t, now, nil, false)
	ctx := context.Background()
	// A key-less api_key account has nothing to seal, so it is creatable here.
	created := createTestVendorAccount(t, svc, ownerToken(), CreateVendorAccountRequest{
		Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey, Name: "No key yet",
	})
	before, err := routeStore.VendorAccountByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}

	key := vendorAccountTestKey
	if _, err := svc.UpdateVendorAccount(ctx, ownerToken(), created.ID, UpdateVendorAccountRequest{APIKey: &key}); !errors.Is(err, capture.ErrKeyRequired) {
		t.Fatalf("err = %v, want capture.ErrKeyRequired", err)
	}
	after, err := routeStore.VendorAccountByID(ctx, created.ID)
	if err != nil || after != before {
		t.Fatalf("row after refused update = %#v, %v, want unchanged %#v", after, err, before)
	}
	// Non-secret edits still work on such a store.
	name := "Renamed"
	if dto, err := svc.UpdateVendorAccount(ctx, ownerToken(), created.ID, UpdateVendorAccountRequest{Name: &name}); err != nil || dto.Name != name {
		t.Fatalf("rename on keyless store = %#v, %v", dto, err)
	}
}

// vendorAccountModelDTOs is the DTO view the catalog rows are expected to take.
func vendorAccountModelDTOs(models []routing.VendorAccountModel) []VendorAccountModelDTO {
	out := make([]VendorAccountModelDTO, 0, len(models))
	for _, m := range models {
		out = append(out, VendorAccountModelDTO{GatewayModel: m.GatewayModel, UpstreamModel: m.UpstreamModel, APIFlavor: m.APIFlavor, DisplayName: m.DisplayName})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GatewayModel < out[j].GatewayModel })
	return out
}

// Creating an account seeds its vendor's whole curated catalog into
// vendor_account_models, and every read of the account (Create / Get / List /
// Update) reports it in the DTO with the vendor's own api_flavor.
func TestCreateVendorAccountSeedsTheVendorCatalog(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		vendor, flavor string
	}{
		{routing.VendorOpenAI, routing.APIFlavorOpenAI},
		{routing.VendorAnthropic, routing.APIFlavorAnthropic},
	} {
		t.Run(tc.vendor, func(t *testing.T) {
			svc, routeStore := newVendorAccountTestService(t, now)
			ctx := context.Background()
			created := createTestVendorAccount(t, svc, ownerToken(), CreateVendorAccountRequest{
				Vendor: tc.vendor, AuthType: routing.VendorAuthAPIKey, Name: "Seeded", APIKey: vendorAccountTestKey,
			})

			catalog := VendorCatalog(tc.vendor, routing.VendorAuthAPIKey)
			if len(catalog) == 0 {
				t.Fatalf("%s has an empty catalog", tc.vendor)
			}
			want := vendorAccountModelDTOs(catalog)
			if !reflect.DeepEqual(created.Models, want) {
				t.Fatalf("create DTO models = %#v, want %#v", created.Models, want)
			}
			for _, m := range created.Models {
				if m.APIFlavor != tc.flavor {
					t.Fatalf("model %q api_flavor = %q, want %q", m.GatewayModel, m.APIFlavor, tc.flavor)
				}
			}

			// The rows are really in the store, under this account.
			rows, err := routeStore.VendorAccountModels(ctx, created.ID)
			if err != nil || len(rows) != len(catalog) {
				t.Fatalf("stored models = %+v, %v, want %d rows", rows, err, len(catalog))
			}
			for _, row := range rows {
				if row.AccountID != created.ID {
					t.Fatalf("stored row %+v not under account %s", row, created.ID)
				}
			}

			got, err := svc.GetVendorAccount(ctx, ownerToken(), created.ID)
			if err != nil || !reflect.DeepEqual(got.Models, want) {
				t.Fatalf("get models = %#v, %v, want %#v", got.Models, err, want)
			}
			list, err := svc.ListVendorAccounts(ctx, ownerToken())
			if err != nil || len(list.Data) != 1 || !reflect.DeepEqual(list.Data[0].Models, want) {
				t.Fatalf("list = %#v, %v, want one account carrying %#v", list.Data, err, want)
			}
			newName := "Renamed"
			updated, err := svc.UpdateVendorAccount(ctx, ownerToken(), created.ID, UpdateVendorAccountRequest{Name: &newName})
			if err != nil || !reflect.DeepEqual(updated.Models, want) {
				t.Fatalf("update models = %#v, %v, want %#v", updated.Models, err, want)
			}
		})
	}
}

// A subscription account is created unconnected but is seeded with its vendor's
// catalog, so it is routable the moment its OAuth connect completes. Anthropic's
// OAuth path serves the same ids as its api key.
func TestCreateVendorAccountSubscriptionAlsoSeedsTheCatalog(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, _ := newVendorAccountTestService(t, now)
	dto := createTestVendorAccount(t, svc, ownerToken(), CreateVendorAccountRequest{
		Vendor: routing.VendorAnthropic, AuthType: routing.VendorAuthSubscription, Name: "Claude Max",
	})
	if want := vendorAccountModelDTOs(VendorCatalog(routing.VendorAnthropic, routing.VendorAuthSubscription)); !reflect.DeepEqual(dto.Models, want) {
		t.Fatalf("models = %#v, want %#v", dto.Models, want)
	}
}

// An OpenAI subscription account is served by the Codex ChatGPT backend, which
// does not serve gpt-4.1 / o3: it is seeded with exactly gpt-5 and gpt-5-mini --
// in the create DTO and in the stored rows -- while an OpenAI api-key account
// (api.openai.com) is seeded with all four. Seeding a model the backend cannot
// serve would guarantee a model error on a perfectly good credential.
func TestCreateVendorAccountSeedsTheCatalogByAuthType(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		req  CreateVendorAccountRequest
		want []string
	}{
		{
			"subscription",
			CreateVendorAccountRequest{Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthSubscription, Name: "ChatGPT Plus"},
			[]string{"gpt-5", "gpt-5-mini"},
		},
		{
			"api_key",
			CreateVendorAccountRequest{Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey, Name: "Platform", APIKey: vendorAccountTestKey},
			[]string{"gpt-4.1", "gpt-5", "gpt-5-mini", "o3"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, routeStore := newVendorAccountTestService(t, now)
			dto := createTestVendorAccount(t, svc, ownerToken(), tc.req)

			var dtoIDs []string
			for _, m := range dto.Models {
				dtoIDs = append(dtoIDs, m.GatewayModel)
				if m.UpstreamModel != m.GatewayModel || m.APIFlavor != routing.APIFlavorOpenAI {
					t.Errorf("model %+v, want a transparent openai-flavor pass-through", m)
				}
			}
			slices.Sort(dtoIDs)
			if !slices.Equal(dtoIDs, tc.want) {
				t.Fatalf("create DTO models = %q, want exactly %q", dtoIDs, tc.want)
			}

			rows, err := routeStore.VendorAccountModels(context.Background(), dto.ID)
			if err != nil {
				t.Fatalf("VendorAccountModels: %v", err)
			}
			var rowIDs []string
			for _, row := range rows {
				rowIDs = append(rowIDs, row.GatewayModel)
			}
			slices.Sort(rowIDs)
			if !slices.Equal(rowIDs, tc.want) {
				t.Fatalf("stored models = %q, want exactly %q", rowIDs, tc.want)
			}
		})
	}
}

// Deleting the account takes its seeded model rows with it.
func TestDeleteVendorAccountRemovesItsModelRows(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	ctx := context.Background()
	acc := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Doomed"))
	keep := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Survivor"))

	if _, err := svc.DeleteVendorAccount(ctx, ownerToken(), acc.ID); err != nil {
		t.Fatalf("DeleteVendorAccount: %v", err)
	}
	if rows, err := routeStore.VendorAccountModels(ctx, acc.ID); err != nil || len(rows) != 0 {
		t.Fatalf("models of the deleted account = %+v, %v, want none", rows, err)
	}
	if rows, err := routeStore.VendorAccountModels(ctx, keep.ID); err != nil || len(rows) == 0 {
		t.Fatalf("models of the sibling account = %+v, %v, want the seeded catalog", rows, err)
	}
}

// failingModelsStore wraps a routing.Store so SetVendorAccountModels fails, and
// models a SQL driver's context handling: DeleteVendorAccount refuses a context
// that is already done (the in-memory store ignores its context). deleteErr, when
// set, makes the delete itself fail so a double fault can be observed.
type failingModelsStore struct {
	routing.Store
	err       error
	deleteErr error
}

func (f failingModelsStore) SetVendorAccountModels(context.Context, string, []routing.VendorAccountModel) error {
	return f.err
}

func (f failingModelsStore) DeleteVendorAccount(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return f.Store.DeleteVendorAccount(ctx, id)
}

// An account whose catalog could not be seeded would be silently unroutable, so
// a seeding failure fails the create and leaves no half-made account behind.
func TestCreateVendorAccountSeedFailureLeavesNoAccount(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	boom := errors.New("seed boom")
	svc.routes = failingModelsStore{Store: routeStore, err: boom}

	if _, err := svc.CreateVendorAccount(context.Background(), ownerToken(), apiKeyAccountRequest("Unseedable")); !errors.Is(err, boom) {
		t.Fatalf("create err = %v, want the seeding error", err)
	}
	accounts, err := routeStore.VendorAccountsByOwner(context.Background(), "usr_owner")
	if err != nil || len(accounts) != 0 {
		t.Fatalf("accounts after a failed seed = %+v, %v, want none", accounts, err)
	}
}

// The most plausible reason a seed fails on a SQL driver is that the request
// context died (client disconnect, timeout). The cleanup must not share that
// context, or it fails the same way and strands an account with no models. The
// context here is already cancelled, and the wrapper's delete refuses a done
// context like a SQL driver does, so only a detached cleanup can remove the row.
func TestCreateVendorAccountSeedFailureCleanupSurvivesACancelledContext(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	boom := errors.New("seed boom")
	svc.routes = failingModelsStore{Store: routeStore, err: boom}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := svc.CreateVendorAccount(ctx, ownerToken(), apiKeyAccountRequest("Doomed by its context"))
	if !errors.Is(err, boom) {
		t.Fatalf("create err = %v, want the original seeding error", err)
	}
	accounts, listErr := routeStore.VendorAccountsByOwner(context.Background(), "usr_owner")
	if listErr != nil || len(accounts) != 0 {
		t.Fatalf("accounts after a failed seed on a cancelled context = %+v, %v, want none (the cleanup must run detached)", accounts, listErr)
	}
}

// When the cleanup fails too, both faults are visible in the returned error: the
// account stays behind unseeded, and the caller must be able to tell.
func TestCreateVendorAccountSeedAndCleanupFailureAreBothReported(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	boom := errors.New("seed boom")
	stuck := errors.New("delete boom")
	svc.routes = failingModelsStore{Store: routeStore, err: boom, deleteErr: stuck}

	_, err := svc.CreateVendorAccount(context.Background(), ownerToken(), apiKeyAccountRequest("Double fault"))
	if !errors.Is(err, boom) || !errors.Is(err, stuck) {
		t.Fatalf("create err = %v, want both the seeding and the cleanup error", err)
	}
}

// With the master flag OFF every vendor-account method refuses with
// ErrVendorAccountsDisabled before it reads, writes or authorizes anything --
// including for a principal who owns an existing account and for a missing id
// (a disabled area must not even confirm whether an account exists).
func TestVendorAccountMethodsRefuseWhileTheMasterFlagIsOff(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	existing := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Created while enabled"))
	setVendorAccountsEnabled(t, svc, false)

	ctx := context.Background()
	name := "renamed"
	calls := map[string]func() error{
		"create": func() error {
			_, err := svc.CreateVendorAccount(ctx, ownerToken(), apiKeyAccountRequest("Should not exist"))
			return err
		},
		"list": func() error {
			_, err := svc.ListVendorAccounts(ctx, ownerToken())
			return err
		},
		"get": func() error {
			_, err := svc.GetVendorAccount(ctx, ownerToken(), existing.ID)
			return err
		},
		"get unknown id": func() error {
			_, err := svc.GetVendorAccount(ctx, ownerToken(), "va_missing")
			return err
		},
		"update": func() error {
			_, err := svc.UpdateVendorAccount(ctx, ownerToken(), existing.ID, UpdateVendorAccountRequest{Name: &name})
			return err
		},
		"delete": func() error {
			_, err := svc.DeleteVendorAccount(ctx, ownerToken(), existing.ID)
			return err
		},
		"create without a user identity": func() error {
			_, err := svc.CreateVendorAccount(ctx, auth.Token{}, apiKeyAccountRequest("Anonymous"))
			return err
		},
	}
	for label, call := range calls {
		if err := call(); !errors.Is(err, ErrVendorAccountsDisabled) {
			t.Fatalf("%s while disabled: err = %v, want ErrVendorAccountsDisabled", label, err)
		}
	}

	// Nothing was written or removed: the one account is intact and unrenamed.
	accounts, err := routeStore.VendorAccountsByOwner(ctx, "usr_owner")
	if err != nil {
		t.Fatalf("VendorAccountsByOwner: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ID != existing.ID || accounts[0].Name != "Created while enabled" {
		t.Fatalf("accounts after refused calls = %+v, want the single untouched account", accounts)
	}

	// Switching the flag back on restores the area, and the account is still there.
	setVendorAccountsEnabled(t, svc, true)
	list, err := svc.ListVendorAccounts(ctx, ownerToken())
	if err != nil || len(list.Data) != 1 || list.Data[0].ID != existing.ID {
		t.Fatalf("list after re-enabling = %+v, %v, want the existing account", list, err)
	}
}

// A service with no settings store (the flag cannot be read) is disabled too:
// the area is opt-in, so an unreadable flag must fail closed.
func TestVendorAccountMethodsRefuseWithoutASettingsStore(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestServiceWithCipher(t, now, nil, true)
	if _, err := svc.CreateVendorAccount(context.Background(), ownerToken(), apiKeyAccountRequest("No store")); !errors.Is(err, ErrVendorAccountsDisabled) {
		t.Fatalf("create without a settings store: err = %v, want ErrVendorAccountsDisabled", err)
	}
}

// The default (flag never written) is OFF, so a fresh deployment exposes nothing.
func TestVendorAccountMethodsRefuseByDefault(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestServiceWithCipher(t, now, nil, true)
	svc.settings = NewMemorySystemSettings()
	if _, err := svc.ListVendorAccounts(context.Background(), ownerToken()); !errors.Is(err, ErrVendorAccountsDisabled) {
		t.Fatalf("list with the flag unset: err = %v, want ErrVendorAccountsDisabled", err)
	}
}

// The model prefix is an optional per-account namespace for the account's
// gateway-model ids. This task only stores and exposes it (the dispatch path
// applies it later), so the contract pinned here is: trimmed, "" = none, and a
// printable-ASCII, space-free value of at most vendorAccountModelPrefixMaxLen
// characters, on Create and on Update alike.
func TestCreateVendorAccountStoresAndExposesTheModelPrefix(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	ctx := context.Background()

	plain := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("No prefix"))
	if plain.ModelPrefix != "" {
		t.Fatalf("default model_prefix = %q, want empty", plain.ModelPrefix)
	}

	req := apiKeyAccountRequest("Work")
	req.ModelPrefix = "  work/  "
	dto := createTestVendorAccount(t, svc, ownerToken(), req)
	if dto.ModelPrefix != "work/" {
		t.Fatalf("model_prefix = %q, want the trimmed value", dto.ModelPrefix)
	}
	row, err := routeStore.VendorAccountByID(ctx, dto.ID)
	if err != nil || row.ModelPrefix != "work/" {
		t.Fatalf("stored account = %#v, %v, want ModelPrefix work/", row, err)
	}

	// Every read of the account reports it: Get and List.
	got, err := svc.GetVendorAccount(ctx, ownerToken(), dto.ID)
	if err != nil || got.ModelPrefix != "work/" {
		t.Fatalf("Get = %#v, %v, want model_prefix work/", got, err)
	}
	list, err := svc.ListVendorAccounts(ctx, ownerToken())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	prefixes := map[string]string{}
	for _, acc := range list.Data {
		prefixes[acc.ID] = acc.ModelPrefix
	}
	if prefixes[dto.ID] != "work/" || prefixes[plain.ID] != "" {
		t.Fatalf("list prefixes = %v, want work/ for the prefixed account and empty for the other", prefixes)
	}

	raw, _ := json.Marshal(dto)
	if !strings.Contains(string(raw), `"model_prefix":"work/"`) {
		t.Fatalf("dto JSON %s missing model_prefix", raw)
	}
	rawPlain, _ := json.Marshal(plain)
	if !strings.Contains(string(rawPlain), `"model_prefix":""`) {
		t.Fatalf("dto JSON %s must carry an empty model_prefix, not omit it", rawPlain)
	}
}

func TestCreateVendorAccountRejectsAnInvalidModelPrefix(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)

	for _, tc := range []struct {
		name   string
		prefix string
	}{
		{"too long", strings.Repeat("a", vendorAccountModelPrefixMaxLen+1)},
		{"non-ASCII letter", "w\u00f6rk/"},
		{"non-ASCII symbol", "work\u2192"},
		{"embedded space", "my work/"},
		{"embedded tab", "my\twork/"},
		{"embedded newline", "work\n/"},
		{"control character", "work\x01/"},
		{"DEL", "work\x7f/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := apiKeyAccountRequest("Acct")
			req.ModelPrefix = tc.prefix
			if _, err := svc.CreateVendorAccount(context.Background(), ownerToken(), req); !errors.Is(err, ErrVendorAccountModelPrefixInvalid) {
				t.Fatalf("err = %v, want ErrVendorAccountModelPrefixInvalid", err)
			}
		})
	}
	if rows, _ := routeStore.VendorAccounts(context.Background()); len(rows) != 0 {
		t.Fatalf("rows = %#v, want none persisted after rejected creates", rows)
	}

	// The limit is inclusive, and every printable ASCII symbol is allowed.
	for _, prefix := range []string{
		strings.Repeat("a", vendorAccountModelPrefixMaxLen),
		"a-b_c.d:e/f+g@h~!",
	} {
		req := apiKeyAccountRequest("Edge")
		req.ModelPrefix = prefix
		if dto, err := svc.CreateVendorAccount(context.Background(), ownerToken(), req); err != nil || dto.ModelPrefix != prefix {
			t.Fatalf("prefix %q = %#v, %v, want it accepted verbatim", prefix, dto, err)
		}
	}

	// All-whitespace trims to empty, which means "no prefix", not an error.
	req := apiKeyAccountRequest("Blank")
	req.ModelPrefix = "   "
	if dto, err := svc.CreateVendorAccount(context.Background(), ownerToken(), req); err != nil || dto.ModelPrefix != "" {
		t.Fatalf("blank prefix = %#v, %v, want accepted as no prefix", dto, err)
	}
}

func TestUpdateVendorAccountModelPrefixSemantics(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	req := apiKeyAccountRequest("Prefixed")
	req.ModelPrefix = "work/"
	created := createTestVendorAccount(t, svc, ownerToken(), req)
	ctx := context.Background()
	str := func(s string) *string { return &s }
	stored := func() routing.VendorAccount {
		t.Helper()
		row, err := routeStore.VendorAccountByID(ctx, created.ID)
		if err != nil {
			t.Fatalf("VendorAccountByID: %v", err)
		}
		return row
	}

	// nil keeps the stored prefix (a rename must not wipe it).
	dto, err := svc.UpdateVendorAccount(ctx, ownerToken(), created.ID, UpdateVendorAccountRequest{Name: str("Renamed")})
	if err != nil || dto.ModelPrefix != "work/" || stored().ModelPrefix != "work/" {
		t.Fatalf("rename = %#v, %v, stored prefix %q, want the prefix kept", dto, err, stored().ModelPrefix)
	}

	// A value replaces it, trimmed.
	dto, err = svc.UpdateVendorAccount(ctx, ownerToken(), created.ID, UpdateVendorAccountRequest{ModelPrefix: str("  home-  ")})
	if err != nil || dto.ModelPrefix != "home-" || stored().ModelPrefix != "home-" {
		t.Fatalf("set = %#v, %v, stored prefix %q, want home-", dto, err, stored().ModelPrefix)
	}
	if dto.Name != "Renamed" || !dto.APIKeySet {
		t.Fatalf("setting the prefix touched other fields: %#v", dto)
	}

	// An invalid value is rejected and leaves the row untouched.
	for _, bad := range []string{strings.Repeat("a", vendorAccountModelPrefixMaxLen+1), "w\u00f6rk", "has space"} {
		if _, err := svc.UpdateVendorAccount(ctx, ownerToken(), created.ID, UpdateVendorAccountRequest{ModelPrefix: str(bad)}); !errors.Is(err, ErrVendorAccountModelPrefixInvalid) {
			t.Fatalf("prefix %q err = %v, want ErrVendorAccountModelPrefixInvalid", bad, err)
		}
	}
	if got := stored().ModelPrefix; got != "home-" {
		t.Fatalf("stored prefix after rejected updates = %q, want home- untouched", got)
	}

	// The empty string clears it.
	dto, err = svc.UpdateVendorAccount(ctx, ownerToken(), created.ID, UpdateVendorAccountRequest{ModelPrefix: str("")})
	if err != nil || dto.ModelPrefix != "" || stored().ModelPrefix != "" {
		t.Fatalf("clear = %#v, %v, stored prefix %q, want empty", dto, err, stored().ModelPrefix)
	}
}

// Each model row's display name rides on the model DTO under display_name, on
// every read; a row without one reports "" (the field is always present).
func TestVendorAccountModelDTOExposesTheDisplayName(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	created := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Named models"))
	ctx := context.Background()

	if err := routeStore.SetVendorAccountModels(ctx, created.ID, []routing.VendorAccountModel{
		{GatewayModel: "gpt-4o", UpstreamModel: "gpt-4o", APIFlavor: routing.APIFlavorOpenAI, DisplayName: "GPT-4o"},
		{GatewayModel: "o3", UpstreamModel: "o3", APIFlavor: routing.APIFlavorOpenAI},
	}); err != nil {
		t.Fatalf("SetVendorAccountModels: %v", err)
	}
	want := []VendorAccountModelDTO{
		{GatewayModel: "gpt-4o", UpstreamModel: "gpt-4o", APIFlavor: routing.APIFlavorOpenAI, DisplayName: "GPT-4o"},
		{GatewayModel: "o3", UpstreamModel: "o3", APIFlavor: routing.APIFlavorOpenAI},
	}

	got, err := svc.GetVendorAccount(ctx, ownerToken(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got.Models, want) {
		t.Fatalf("Get models = %#v, want %#v", got.Models, want)
	}
	list, err := svc.ListVendorAccounts(ctx, ownerToken())
	if err != nil || len(list.Data) != 1 || !reflect.DeepEqual(list.Data[0].Models, want) {
		t.Fatalf("List = %#v, %v, want the same models", list, err)
	}
	str := "Renamed"
	updated, err := svc.UpdateVendorAccount(ctx, ownerToken(), created.ID, UpdateVendorAccountRequest{Name: &str})
	if err != nil || !reflect.DeepEqual(updated.Models, want) {
		t.Fatalf("Update = %#v, %v, want the same models", updated, err)
	}

	raw, _ := json.Marshal(got.Models)
	for _, field := range []string{`"display_name":"GPT-4o"`, `"display_name":""`} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("models JSON %s missing %s", raw, field)
		}
	}
}
