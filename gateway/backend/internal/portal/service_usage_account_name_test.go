// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"sync"
	"testing"
	"time"
)

// countingVendorAccountStore wraps a real routing.Store and counts
// VendorAccountByID lookups per id, so a test can prove the Activity list
// resolves each DISTINCT account once (not once per row) and does not look up
// non-vendor rows at all. When err is set every lookup fails with it.
type countingVendorAccountStore struct {
	routing.Store
	mu    sync.Mutex
	calls map[string]int
	err   error
}

func (s *countingVendorAccountStore) VendorAccountByID(ctx context.Context, id string) (routing.VendorAccount, error) {
	s.mu.Lock()
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[id]++
	s.mu.Unlock()
	if s.err != nil {
		return routing.VendorAccount{}, s.err
	}
	return s.Store.VendorAccountByID(ctx, id)
}

func (s *countingVendorAccountStore) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		n += c
	}
	return n
}

// accountNameFixture seeds one vendor account owned by usr_owner and a usage
// recorder holding a vendor row (account_id set) for that account plus a
// plain non-vendor row, both attributed to usr_owner.
type accountNameFixture struct {
	routes *countingVendorAccountStore
	rec    *usage.Recorder
	svc    *Service
}

func newAccountNameFixture(t *testing.T) *accountNameFixture {
	t.Helper()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	mem := routing.NewMemoryStore()
	if err := mem.CreateVendorAccount(ctx, routing.VendorAccount{
		ID: "vacc_1", OwnerUserID: "usr_owner", Vendor: routing.VendorAnthropic,
		AuthType: routing.VendorAuthAPIKey, Name: "Work Claude",
		Status: routing.VendorAccountStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateVendorAccount: %v", err)
	}
	routes := &countingVendorAccountStore{Store: mem}
	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "row_vendor", UserID: "usr_owner", Host: "anthropic", AccountID: "vacc_1", CreatedAt: now})
	rec.Record(usage.Event{ID: "row_local", UserID: "usr_owner", Host: "srv_local", CreatedAt: now.Add(-time.Minute)})
	// No SystemSettings: the vendor_accounts_enabled master flag is therefore
	// OFF, which proves the name resolution is not gated on it (a historical
	// row must keep resolving after an operator disables the feature).
	svc := NewService(ServiceDeps{Usage: rec, Routes: routes, Clock: func() time.Time { return now }})
	return &accountNameFixture{routes: routes, rec: rec, svc: svc}
}

func usageRowByID(t *testing.T, page usage.Page, id string) usage.Row {
	t.Helper()
	for _, row := range page.Data {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("row %q not in page (%d rows)", id, len(page.Data))
	return usage.Row{}
}

// TestServiceUsageAccountNameOwnerSeesName: the account's owner viewing their
// own list gets the resolved account name on the vendor row, and the non-vendor
// row is left without one.
func TestServiceUsageAccountNameOwnerSeesName(t *testing.T) {
	f := newAccountNameFixture(t)

	page, err := f.svc.Usage(auth.Token{UserID: "usr_owner", Scopes: []string{"gateway:use"}}, usage.Query{Page: 1, Limit: 25})
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	vendor := usageRowByID(t, page, "row_vendor")
	if vendor.AccountName != "Work Claude" {
		t.Fatalf("owner AccountName = %q, want %q", vendor.AccountName, "Work Claude")
	}
	if vendor.AccountID != "vacc_1" {
		t.Fatalf("AccountID = %q, want vacc_1", vendor.AccountID)
	}
	if local := usageRowByID(t, page, "row_local"); local.AccountName != "" || local.AccountID != "" {
		t.Fatalf("non-vendor row = {AccountID:%q AccountName:%q}, want both empty", local.AccountID, local.AccountName)
	}
}

// TestServiceUsageAccountNameSystemAdminSeesName: a system admin (scope
// "system", the same gate authorizeVendorAccount's read path uses) viewing the
// all-scope list sees another user's vendor row WITH the account name.
func TestServiceUsageAccountNameSystemAdminSeesName(t *testing.T) {
	f := newAccountNameFixture(t)

	sys := auth.Token{UserID: "usr_sys", Scopes: []string{"gateway:use", "admin", "system"}}
	page, err := f.svc.Usage(sys, usage.Query{Page: 1, Limit: 25, ScopeAll: true})
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if got := usageRowByID(t, page, "row_vendor").AccountName; got != "Work Claude" {
		t.Fatalf("system admin AccountName = %q, want %q", got, "Work Claude")
	}
}

// TestServiceUsageAccountNameHiddenFromNonOwnerAdmin: a plain admin (scope
// "admin" but not "system") is a stranger to someone else's personal vendor
// credential, exactly as in authorizeVendorAccount: the row's account_id stays
// on the wire but the account NAME is withheld.
func TestServiceUsageAccountNameHiddenFromNonOwnerAdmin(t *testing.T) {
	f := newAccountNameFixture(t)

	admin := auth.Token{UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}}
	page, err := f.svc.Usage(admin, usage.Query{Page: 1, Limit: 25, ScopeAll: true})
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	vendor := usageRowByID(t, page, "row_vendor")
	if vendor.AccountName != "" {
		t.Fatalf("non-owner admin AccountName = %q, want empty", vendor.AccountName)
	}
	if vendor.AccountID != "vacc_1" {
		t.Fatalf("non-owner admin AccountID = %q, want vacc_1 (the id stays on the row)", vendor.AccountID)
	}
}

// TestServiceUsageAccountNameHiddenFromProjectMember: a project member (the
// one non-admin path that sees other users' rows, via the project drill-down
// widening) viewing a teammate's vendor row does not get the account name.
func TestServiceUsageAccountNameHiddenFromProjectMember(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_member", "user")
	owner := token("usr_owner")
	member := token("usr_member")
	p := e.mustCreateProject(owner, "Alpha", "")
	if err := e.dir.SetProjectMember(e.ctx, p.ID, "usr_member"); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if err := e.svc.routes.CreateVendorAccount(e.ctx, routing.VendorAccount{
		ID: "vacc_1", OwnerUserID: "usr_owner", Vendor: routing.VendorOpenAI,
		AuthType: routing.VendorAuthAPIKey, Name: "Owner Personal", Status: routing.VendorAccountStatusActive,
		CreatedAt: e.now, UpdatedAt: e.now,
	}); err != nil {
		t.Fatalf("CreateVendorAccount: %v", err)
	}
	e.rec.Record(usage.Event{ID: "row_vendor", UserID: "usr_owner", ProjectID: p.ID, AccountID: "vacc_1", CreatedAt: e.now})

	q := usage.Query{Page: 1, Limit: 25, HasProjectIDExact: true, ProjectIDExact: p.ID}
	page, err := e.svc.Usage(member, q)
	if err != nil {
		t.Fatalf("member Usage: %v", err)
	}
	vendor := usageRowByID(t, page, "row_vendor")
	if vendor.AccountID != "vacc_1" || vendor.AccountName != "" {
		t.Fatalf("project member row = {AccountID:%q AccountName:%q}, want {vacc_1, empty}", vendor.AccountID, vendor.AccountName)
	}

	// Control: the owner viewing the same project drill-down sees the name.
	page, err = e.svc.Usage(owner, q)
	if err != nil {
		t.Fatalf("owner Usage: %v", err)
	}
	if got := usageRowByID(t, page, "row_vendor").AccountName; got != "Owner Personal" {
		t.Fatalf("owner AccountName = %q, want %q", got, "Owner Personal")
	}
}

// TestServiceUsageAccountNameDeletedAccountIsEmpty: usage_events.account_id has
// no foreign key (deleted accounts leave dangling ids by design), so a row
// whose account is gone keeps its id, gets no name, and the list still returns
// without error.
func TestServiceUsageAccountNameDeletedAccountIsEmpty(t *testing.T) {
	f := newAccountNameFixture(t)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	f.rec.Record(usage.Event{ID: "row_gone", UserID: "usr_owner", Host: "anthropic", AccountID: "vacc_deleted", CreatedAt: now.Add(-time.Hour)})

	page, err := f.svc.Usage(auth.Token{UserID: "usr_owner", Scopes: []string{"gateway:use"}}, usage.Query{Page: 1, Limit: 25})
	if err != nil {
		t.Fatalf("Usage returned err for a dangling account id: %v", err)
	}
	if len(page.Data) != 3 {
		t.Fatalf("page rows = %d, want 3 (the list must still be returned)", len(page.Data))
	}
	gone := usageRowByID(t, page, "row_gone")
	if gone.AccountName != "" || gone.AccountID != "vacc_deleted" {
		t.Fatalf("deleted-account row = {AccountID:%q AccountName:%q}, want {vacc_deleted, empty}", gone.AccountID, gone.AccountName)
	}
	// The live account on the same page still resolves.
	if got := usageRowByID(t, page, "row_vendor").AccountName; got != "Work Claude" {
		t.Fatalf("live account AccountName = %q, want %q", got, "Work Claude")
	}
}

// TestServiceUsageAccountNameLookupErrorIsFailSoft: any lookup failure other
// than not-found (e.g. the database is down) leaves the name empty and never
// fails the list.
func TestServiceUsageAccountNameLookupErrorIsFailSoft(t *testing.T) {
	f := newAccountNameFixture(t)
	f.routes.err = errors.New("db down")

	page, err := f.svc.Usage(auth.Token{UserID: "usr_owner", Scopes: []string{"gateway:use"}}, usage.Query{Page: 1, Limit: 25})
	if err != nil {
		t.Fatalf("Usage returned err on a lookup failure: %v", err)
	}
	vendor := usageRowByID(t, page, "row_vendor")
	if vendor.AccountName != "" || vendor.AccountID != "vacc_1" {
		t.Fatalf("row = {AccountID:%q AccountName:%q}, want {vacc_1, empty}", vendor.AccountID, vendor.AccountName)
	}
}

// TestServiceUsageAccountNameNoRoutesStoreIsFailSoft: a Service without a
// routing store neither panics nor errors; the name is simply empty.
func TestServiceUsageAccountNameNoRoutesStoreIsFailSoft(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "row_vendor", UserID: "usr_owner", AccountID: "vacc_1", CreatedAt: now})
	svc := NewService(ServiceDeps{Usage: rec, Clock: func() time.Time { return now }})

	page, err := svc.Usage(auth.Token{UserID: "usr_owner", Scopes: []string{"gateway:use"}}, usage.Query{Page: 1, Limit: 25})
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].AccountName != "" {
		t.Fatalf("page = %+v, want one row with an empty AccountName", page.Data)
	}
}

// TestServiceUsageAccountNameNoLookupWithoutVendorRows: a page with only
// non-vendor rows performs no vendor-account lookup at all.
func TestServiceUsageAccountNameNoLookupWithoutVendorRows(t *testing.T) {
	f := newAccountNameFixture(t)

	// Pin to the non-vendor row through a user that only owns local rows.
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	f.rec.Record(usage.Event{ID: "row_other", UserID: "usr_plain", Host: "srv_local", CreatedAt: now})

	page, err := f.svc.Usage(auth.Token{UserID: "usr_plain", Scopes: []string{"gateway:use"}}, usage.Query{Page: 1, Limit: 25})
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].AccountName != "" {
		t.Fatalf("page = %+v, want one non-vendor row with an empty AccountName", page.Data)
	}
	if n := f.routes.total(); n != 0 {
		t.Fatalf("VendorAccountByID called %d times for a page without vendor rows, want 0", n)
	}
}

// TestServiceUsageAccountNameLooksUpEachDistinctIDOnce: many rows sharing one
// account id cost ONE lookup (no N+1), and a second distinct id costs exactly
// one more.
func TestServiceUsageAccountNameLooksUpEachDistinctIDOnce(t *testing.T) {
	f := newAccountNameFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if err := f.routes.Store.CreateVendorAccount(ctx, routing.VendorAccount{
		ID: "vacc_2", OwnerUserID: "usr_owner", Vendor: routing.VendorOpenAI,
		AuthType: routing.VendorAuthSubscription, Name: "Plus Subscription",
		Status: routing.VendorAccountStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateVendorAccount vacc_2: %v", err)
	}
	for i, id := range []string{"a", "b", "c", "d"} {
		f.rec.Record(usage.Event{ID: "row_dup1_" + id, UserID: "usr_owner", AccountID: "vacc_1", CreatedAt: now.Add(-time.Duration(i+2) * time.Minute)})
	}
	f.rec.Record(usage.Event{ID: "row_dup2", UserID: "usr_owner", AccountID: "vacc_2", CreatedAt: now.Add(-10 * time.Minute)})

	page, err := f.svc.Usage(auth.Token{UserID: "usr_owner", Scopes: []string{"gateway:use"}}, usage.Query{Page: 1, Limit: 25})
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	// 1 original vendor row + 4 duplicates for vacc_1, 1 row for vacc_2, 1 local.
	if len(page.Data) != 7 {
		t.Fatalf("page rows = %d, want 7", len(page.Data))
	}
	for _, row := range page.Data {
		switch row.AccountID {
		case "vacc_1":
			if row.AccountName != "Work Claude" {
				t.Fatalf("row %s AccountName = %q, want Work Claude", row.ID, row.AccountName)
			}
		case "vacc_2":
			if row.AccountName != "Plus Subscription" {
				t.Fatalf("row %s AccountName = %q, want Plus Subscription", row.ID, row.AccountName)
			}
		case "":
			if row.AccountName != "" {
				t.Fatalf("non-vendor row %s AccountName = %q, want empty", row.ID, row.AccountName)
			}
		}
	}
	if c := f.routes.calls["vacc_1"]; c != 1 {
		t.Fatalf("VendorAccountByID(vacc_1) called %d times for 5 rows, want 1", c)
	}
	if c := f.routes.calls["vacc_2"]; c != 1 {
		t.Fatalf("VendorAccountByID(vacc_2) called %d times, want 1", c)
	}
	if n := f.routes.total(); n != 2 {
		t.Fatalf("total VendorAccountByID calls = %d, want 2", n)
	}
}
