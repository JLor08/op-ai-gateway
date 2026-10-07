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
	"sort"
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
	return newServerTestServiceWithCipher(t, now, nil, true)
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
	wantModels := vendorAccountModelDTOs(VendorCatalog(routing.VendorOpenAI))
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
	svc, routeStore := newServerTestServiceWithCipher(t, now, cipher, false)

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
	svc, routeStore := newServerTestServiceWithCipher(t, now, nil, false)

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
	svc, routeStore := newServerTestServiceWithCipher(t, now, nil, false)
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
		out = append(out, VendorAccountModelDTO{GatewayModel: m.GatewayModel, UpstreamModel: m.UpstreamModel, APIFlavor: m.APIFlavor})
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

			catalog := VendorCatalog(tc.vendor)
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

// A subscription account is created unconnected but serves the same vendor
// catalog, so it is routable the moment its OAuth connect completes.
func TestCreateVendorAccountSubscriptionAlsoSeedsTheCatalog(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, _ := newVendorAccountTestService(t, now)
	dto := createTestVendorAccount(t, svc, ownerToken(), CreateVendorAccountRequest{
		Vendor: routing.VendorAnthropic, AuthType: routing.VendorAuthSubscription, Name: "Claude Max",
	})
	if want := vendorAccountModelDTOs(VendorCatalog(routing.VendorAnthropic)); !reflect.DeepEqual(dto.Models, want) {
		t.Fatalf("models = %#v, want %#v", dto.Models, want)
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

// failingModelsStore wraps a routing.Store so SetVendorAccountModels fails.
type failingModelsStore struct {
	routing.Store
	err error
}

func (f failingModelsStore) SetVendorAccountModels(context.Context, string, []routing.VendorAccountModel) error {
	return f.err
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
