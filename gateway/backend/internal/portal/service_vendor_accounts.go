// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"reflect"
	"sort"
	"strings"
	"time"
)

// vendorAccountCleanupTimeout bounds the removal of a vendor account whose model
// catalog could not be seeded (see CreateVendorAccount). The cleanup runs on a
// context detached from the request, so this is its only deadline.
const vendorAccountCleanupTimeout = 5 * time.Second

// vendorAccountModelPrefixMaxLen bounds an account's model prefix. The prefix is
// glued onto every gateway model id the account serves, so a generous but finite
// cap keeps ids (and the URLs and logs they appear in) reasonable.
const vendorAccountModelPrefixMaxLen = 64

// VendorAccountModelDTO is one gateway-model entry a vendor account serves: a
// row of vendor_account_models, seeded from the curated per-vendor catalog
// (VendorCatalog) when the account is created. DisplayName is the vendor's
// human-readable name for the model ("" when it supplied none); it is always
// present on the wire.
type VendorAccountModelDTO struct {
	GatewayModel  string `json:"gateway_model"`
	UpstreamModel string `json:"upstream_model"`
	APIFlavor     string `json:"api_flavor"`
	DisplayName   string `json:"display_name"`
}

// VendorAccountUsageDTO is the portal view of a routing.VendorAccountUsage: the
// latest usage snapshot of a vendor account, scraped off its upstream responses
// and/or pulled from the vendor (an OpenAI subscription's, an OpenRouter or Kilo
// api key's). The rate-limit windows carry percentages and reset times only --
// neither OpenAI nor Anthropic exposes an absolute window cap, so there is
// deliberately no "N of M" for them. A percentage is 0..100, or -1 when that
// window is UNKNOWN (never observed): the portal must not read -1 as a real 0 %
// used. A reset time is nil when the vendor sent none; CreditBalance is the
// vendor's raw credit string ("" = none; Kilo's balance).
//
// The absolute amounts ride in the spend fields and CreditStatus, which a Business
// plan's spend control (#195) and an OpenRouter key's USD limit both fill:
// SpendUnit (e.g. "credit", "USD") and the vendor's raw SpendLimit / SpendUsed /
// SpendRemaining strings (never parsed by the portal, so the exact digits survive),
// SpendUsedPct (0..100, or -1 = unknown, like the windows), SpendResetAt (nil =
// unknown) and CreditStatus ("unlimited" | "has_credits" | "none", "" = unknown;
// an OpenRouter key without a cap is "unlimited" with the spend strings empty).
// Every one is always on the wire; an account with none of this reads "" / -1 / null.
type VendorAccountUsageDTO struct {
	FiveHourPct     float64    `json:"five_hour_pct"`
	FiveHourResetAt *time.Time `json:"five_hour_reset_at"`
	WeeklyPct       float64    `json:"weekly_pct"`
	WeeklyResetAt   *time.Time `json:"weekly_reset_at"`
	CreditBalance   string     `json:"credit_balance"`
	SpendUnit       string     `json:"spend_unit"`
	SpendLimit      string     `json:"spend_limit"`
	SpendUsed       string     `json:"spend_used"`
	SpendRemaining  string     `json:"spend_remaining"`
	SpendUsedPct    float64    `json:"spend_used_pct"`
	SpendResetAt    *time.Time `json:"spend_reset_at"`
	CreditStatus    string     `json:"credit_status"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// VendorAccountDTO is the portal view of a routing.VendorAccount. It carries NO
// credential material: the sealed api key and OAuth token set are reduced to
// the APIKeySet / SubscriptionConnected booleans (write-only secrets), and the
// owner is implicit (every caller sees only their own accounts).
//
// ModelPrefix is the account's optional model-id namespace ("" = none): every
// model the account serves is listed and requested as ModelPrefix + the vendor's
// model id (a model's GatewayModel), while the vendor is still asked for its own
// id (UpstreamModel).
//
// BaseURL is the upstream root of an OpenAI-compatible account (the preset's
// default root, or the one the user typed); "" for openai and anthropic, whose
// hosts are fixed. It is immutable and always on the wire.
//
// Usage is the rate-limit snapshot. The two read endpoints fill it -- the detail
// view's Usage & Limits panel (GetVendorAccount) and the list
// (ListVendorAccounts, so the dashboard's provider usage section reads every
// account's usage in one call); the write endpoints leave it nil. It is also nil
// for an account the gateway has not yet seen a rate-limit header for, and the
// list leaves it nil for a row whose snapshot read failed (fail-soft). A nil
// Usage is omitted from the JSON. The list's per-row snapshot read is bounded and
// adds no new N+1 shape: that loop already reads each account's model rows.
type VendorAccountDTO struct {
	ID                    string                  `json:"id"`
	Vendor                string                  `json:"vendor"`
	AuthType              string                  `json:"auth_type"`
	Name                  string                  `json:"name"`
	Status                string                  `json:"status"`
	ModelPrefix           string                  `json:"model_prefix"`
	BaseURL               string                  `json:"base_url"`
	APIKeySet             bool                    `json:"api_key_set"`
	SubscriptionConnected bool                    `json:"subscription_connected"`
	Models                []VendorAccountModelDTO `json:"models"`
	Usage                 *VendorAccountUsageDTO  `json:"usage,omitempty"`
	CreatedAt             time.Time               `json:"created_at"`
	UpdatedAt             time.Time               `json:"updated_at"`
}

// VendorAccountListResponse is the {data:[...]} envelope of the list endpoint.
type VendorAccountListResponse struct {
	Data []VendorAccountDTO `json:"data"`
}

// CreateVendorAccountRequest creates an account owned by the calling principal.
// APIKey is write-only and only meaningful for AuthType == api_key; a
// subscription account is created unconnected (the OAuth connect flow fills its
// token set later). An empty Status defaults to active. ModelPrefix is optional
// ("" = none); see normalizeVendorAccountModelPrefix for the accepted shape.
// BaseURL is the upstream root of an OpenAI-compatible account (see
// normalizeVendorAccountBaseURL); it is ignored for openai and anthropic and,
// being immutable, is absent from UpdateVendorAccountRequest.
type CreateVendorAccountRequest struct {
	Vendor      string `json:"vendor"`
	AuthType    string `json:"auth_type"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	APIKey      string `json:"api_key"`
	ModelPrefix string `json:"model_prefix"`
	BaseURL     string `json:"base_url"`
}

// UpdateVendorAccountRequest is a partial update: a nil field keeps the stored
// value. APIKey is the write-only secret sentinel: nil keeps the sealed key,
// "" clears it, any other value replaces it. ModelPrefix follows the same
// pointer convention: nil keeps the stored prefix, "" clears it, any other value
// replaces it. Vendor, auth type and base URL are immutable.
//
// AUTHORIZATION IS PER FIELD AND DENY-BY-DEFAULT (authorizeVendorAccountPatch):
// Name, Status and ModelPrefix are the only fields that ride the operate gate;
// everything else this struct carries -- APIKey, and any field added later --
// rides the credential gate. The allowlist is isVendorAccountPatchOperateField and
// nothing else, so a new field is credential-gated until someone deliberately
// edits that list. Every field MUST be a pointer (nil = "absent"): a value-typed
// field cannot be told apart from "not sent", so the gate treats it as always
// touched.
//
// BaseURL and AuthType are CREATE-ONLY. They are on the request solely so a PATCH
// that tries to change them is refused with ErrVendorAccountFieldImmutable instead
// of being silently ignored: base_url is where the sealed credential is sent, and
// re-aiming it is a credential operation nobody is granted through a PATCH. They
// are not applied anywhere.
type UpdateVendorAccountRequest struct {
	Name        *string `json:"name"`
	Status      *string `json:"status"`
	APIKey      *string `json:"api_key"`
	ModelPrefix *string `json:"model_prefix"`
	BaseURL     *string `json:"base_url"`
	AuthType    *string `json:"auth_type"`
}

// vendorAccountDTO maps a stored account to its credential-free view, reading
// the model rows the account serves from the store. Models is always a non-nil
// slice so it serializes as [] (never null).
func (s *Service) vendorAccountDTO(ctx context.Context, acc routing.VendorAccount) (VendorAccountDTO, error) {
	rows, err := s.routes.VendorAccountModels(ctx, acc.ID)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	models := make([]VendorAccountModelDTO, 0, len(rows))
	for _, row := range rows {
		models = append(models, VendorAccountModelDTO{
			GatewayModel:  row.GatewayModel,
			UpstreamModel: row.UpstreamModel,
			APIFlavor:     row.APIFlavor,
			DisplayName:   row.DisplayName,
		})
	}
	return VendorAccountDTO{
		ID:                    acc.ID,
		Vendor:                acc.Vendor,
		AuthType:              acc.AuthType,
		Name:                  acc.Name,
		Status:                acc.Status,
		ModelPrefix:           acc.ModelPrefix,
		BaseURL:               acc.BaseURL,
		APIKeySet:             acc.APIKey != "",
		SubscriptionConnected: acc.OAuthTokens != "",
		Models:                models,
		CreatedAt:             acc.CreatedAt,
		UpdatedAt:             acc.UpdatedAt,
	}, nil
}

// vendorAccountUsageDTO reads accountID's rate-limit snapshot and maps it to its
// portal view. A nil result (and a nil error) means no snapshot has been scraped
// for the account yet. The store hands out its own copy of the snapshot, so the
// reset pointers are not shared with stored state.
func (s *Service) vendorAccountUsageDTO(ctx context.Context, accountID string) (*VendorAccountUsageDTO, error) {
	usage, ok, err := s.routes.VendorAccountUsageByID(ctx, accountID)
	if err != nil || !ok {
		return nil, err
	}
	return &VendorAccountUsageDTO{
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
		UpdatedAt:       usage.UpdatedAt,
	}, nil
}

func normalizeVendorAccountVendor(raw string) (string, error) {
	switch vendor := strings.TrimSpace(raw); vendor {
	case routing.VendorOpenAI, routing.VendorAnthropic,
		routing.VendorXAI, routing.VendorOpenRouter, routing.VendorKilo,
		routing.VendorGoogle, routing.VendorGoogleNative, routing.VendorOpenAICompatible:
		return vendor, nil
	default:
		return "", ErrVendorAccountVendorInvalid
	}
}

func normalizeVendorAccountAuthType(raw string) (string, error) {
	switch authType := strings.TrimSpace(raw); authType {
	case routing.VendorAuthAPIKey, routing.VendorAuthSubscription:
		return authType, nil
	default:
		return "", ErrVendorAccountAuthTypeInvalid
	}
}

// normalizeVendorAccountStatus validates a status a USER may set: active or
// disabled. needs_reconnect is set by the system when a subscription's token
// refresh fails, so a user cannot assign it (a PATCH that merely echoes an
// account's current needs_reconnect status is let through by the caller, not here).
func normalizeVendorAccountStatus(raw string) (string, error) {
	switch status := strings.TrimSpace(raw); status {
	case routing.VendorAccountStatusActive, routing.VendorAccountStatusDisabled:
		return status, nil
	default:
		return "", ErrVendorAccountStatusInvalid
	}
}

// normalizeVendorAccountBaseURL validates and defaults an OpenAI-compatible
// account's root url. For a preset vendor an empty value means "use the preset
// default root". For VendorOpenAICompatible (Custom) a value is required. A
// non-empty value must be an https URL with a hostname and no userinfo/query/
// fragment (the key rides on every request, so plaintext/credential-in-URL is
// refused; the host is otherwise unrestricted -- this is an on-prem gateway, see
// ADR-052). net/url hides a bare trailing "?" (ForceQuery, empty RawQuery) and a
// bare "#" (empty Fragment) from the field checks, and ":443" parses as a
// non-empty Host with no hostname, so the raw text is also screened for "?", "#"
// and spaces and the HOSTNAME (not Host) must be non-empty. The stored value has
// any trailing "/" removed.
func normalizeVendorAccountBaseURL(vendor, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		preset, ok := routing.VendorPresetFor(vendor)
		if !ok || preset.DefaultBaseURL == "" {
			return "", ErrVendorAccountBaseURLRequired
		}
		return preset.DefaultBaseURL, nil
	}
	u, err := url.Parse(value)
	if err != nil || strings.ContainsAny(value, "?# ") || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", ErrVendorAccountBaseURLInvalid
	}
	return strings.TrimRight(value, "/"), nil
}

// normalizeVendorAccountModelPrefix trims a requested model prefix and checks it.
// "" (after trimming) means no prefix and is always valid. Otherwise the prefix
// must be at most vendorAccountModelPrefixMaxLen bytes drawn from isModelIDByte
// (ASCII letters and digits plus "-", "_", ".", "~", ":", "/", "@", "+") and hold
// no "..": it is concatenated into the model ids the account serves, which travel
// in URL paths (/v1/models/{id}), JSON bodies and logs, so spaces, control
// characters, non-ASCII text and the characters that start a query, fragment or
// escape ("?", "#", "%") or break out of a string or markup are refused rather
// than escaped.
func normalizeVendorAccountModelPrefix(raw string) (string, error) {
	prefix := strings.TrimSpace(raw)
	if len(prefix) > vendorAccountModelPrefixMaxLen || strings.Contains(prefix, "..") {
		return "", ErrVendorAccountModelPrefixInvalid
	}
	for i := 0; i < len(prefix); i++ {
		if !isModelIDByte(prefix[i]) {
			return "", ErrVendorAccountModelPrefixInvalid
		}
	}
	return prefix, nil
}

// sealVendorAPIKey trims surrounding whitespace (a pasted key often carries a
// trailing newline) and seals the key for storage: "enc:" with a cipher,
// "plain:" on a volatile store, capture.ErrKeyRequired on a keyless disk store.
// An empty key seals to "".
func (s *Service) sealVendorAPIKey(raw string) (string, error) {
	return capture.SealSecret(s.cipher, s.settingsVolatile, strings.TrimSpace(raw))
}

// loadVendorAccount loads the account, mapping the store's not-found to
// ErrVendorAccountNotFound. It decides nothing about WHO may see the account:
// every caller follows it with one of the gates below.
func (s *Service) loadVendorAccount(ctx context.Context, id string) (routing.VendorAccount, error) {
	acc, err := s.routes.VendorAccountByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return routing.VendorAccount{}, ErrVendorAccountNotFound
		}
		return routing.VendorAccount{}, err
	}
	return acc, nil
}

// authorizeVendorAccount loads the account and returns ErrVendorAccountNotFound
// unless the principal OWNS it. With write=false (a READ) system scope is also
// let through, so an operator can inspect any account's credential-free
// metadata; with write=true the check is strictly owner-only. A vendor account
// is a personal credential, not shared infrastructure like a server, so the
// authorizeServer system bypass deliberately does not extend to writes: a
// system-scope non-owner may not rotate or clear another user's key, rename or
// disable the account, or delete it. As with authorizeServer, an unknown id and
// a stranger's account are indistinguishable (404-no-leak), and a plain admin
// who is not the owner is a stranger too.
//
// This is the pre-sharing owner-only gate. It still guards the reads
// (GetVendorAccount and the name lookup widen to CMs in the management read);
// owner-only is the safe side of both new gates, so a site left here is merely
// stricter than the spec, never looser. Writes use authorizeVendorAccountOperate or
// authorizeVendorAccountCredential: the PATCH picks between them per field
// (authorizeVendorAccountPatch) and every credential method, the delete and the
// share-level lower use the latter.
func (s *Service) authorizeVendorAccount(ctx context.Context, principal auth.Token, id string, write bool) (routing.VendorAccount, error) {
	acc, err := s.loadVendorAccount(ctx, id)
	if err != nil {
		return routing.VendorAccount{}, err
	}
	if isVendorAccountOwner(principal, acc) {
		return acc, nil
	}
	if !write && isSystem(principal) {
		return acc, nil
	}
	return routing.VendorAccount{}, ErrVendorAccountNotFound
}

// isVendorAccountOwner reports whether principal is the account's owning user. A
// principal with no user identity (a service token, say) owns nothing.
func isVendorAccountOwner(principal auth.Token, acc routing.VendorAccount) bool {
	return principal.UserID != "" && acc.OwnerUserID == principal.UserID
}

// isVendorAccountManager reports whether principal is a CM of acc: a co-manager
// (or the owner) of an admin group the account is assigned to, holding
// can_manage_vendor_accounts there. It is the INTERSECTION of the account's
// vendor_account_admin_groups with vendorAccountManageGroupIDs(principal), both
// read live, so un-assigning the group or demoting the co-manager revokes the
// reach on the very next call.
//
// It says nothing about whether the account is shared: that is the share level's
// job, and the two gates below check it BEFORE they ask. A CM link that outlives
// its level (a torn downgrade, a store written directly) must grant nothing.
// Scope never enters into it: a system-scope token is a CM only if its own user is
// one, exactly like any other token of that user.
func (s *Service) isVendorAccountManager(ctx context.Context, principal auth.Token, acc routing.VendorAccount) (bool, error) {
	if principal.UserID == "" {
		return false, nil
	}
	groupIDs, err := s.routes.VendorAccountAdminGroups(ctx, acc.ID)
	if err != nil {
		return false, err
	}
	if len(groupIDs) == 0 {
		return false, nil
	}
	manageGroups, err := s.vendorAccountManageGroupIDs(ctx, principal)
	if err != nil {
		return false, err
	}
	for _, gid := range groupIDs {
		if manageGroups[gid] {
			return true, nil
		}
	}
	return false, nil
}

// vendorAccountSharedLevel reports whether level is one at which a non-owner can
// reach the account at all (shared or full). Anything else, an unrecognised value
// included, is private: fail closed.
func vendorAccountSharedLevel(level string) bool {
	return level == routing.VendorShareLevelShared || level == routing.VendorShareLevelFull
}

// effectiveVendorAccountShareLevel is the share level an account is treated as
// having RIGHT NOW: VendorShareLevelNone while the vendor_account_sharing_enabled
// master flag is off, the account's stored ShareLevel while it is on (spec
// 2026-10-15 section 13: with the master off "share_level is ignored, every account
// behaves personal"). The flag is read through the same fail-closed accessor as
// requireVendorAccountSharingEnabled, so no settings store, or one that cannot be
// read, reads as off.
//
// Turning the master off is a global PAUSE, not an un-share: nothing stored (the
// level, the root, the management-group and resource-group links) is touched, so
// switching it back on honours the stored level again. Every decision that lets a
// NON-OWNER through on the strength of the share level reads this, not
// acc.ShareLevel: the two write gates below (and with them the delete and the
// share-level lower), and the listings and the routing grant that follow
// must do the same.
func (s *Service) effectiveVendorAccountShareLevel(ctx context.Context, acc routing.VendorAccount) string {
	if !s.VendorAccountSharingEnabled(ctx) {
		return routing.VendorShareLevelNone
	}
	return acc.ShareLevel
}

// authorizeVendorAccountOperate is the gate of the OPERATE operations: status,
// model prefix, rename, model refresh, connection test, usage refresh (spec
// 2026-10-15 section 6). acc is the record as the store returned it; the share level
// it is judged by is the EFFECTIVE one (effectiveVendorAccountShareLevel: the stored
// level while sharing is enabled, `none` while the master flag is off). Allowed for:
//
//   - the owner, at every level;
//   - a CM of the account, while it is `shared` or `full`.
//
// Everyone else, system scope included, gets ErrVendorAccountNotFound
// (404-no-leak): an operate call opens the owner's sealed credential and sends it
// upstream, and system scope never bypasses a write. At `none` the account has no
// management group, so no CM can exist and owner-only falls out; the level check
// comes first anyway so a leftover admin-group link at `none` grants nothing.
//
// This gate says nothing about the credential-bearing operations (see
// authorizeVendorAccountCredential), nor about the subscription exception that
// makes some operate calls owner-only at `shared` for a subscription account.
func (s *Service) authorizeVendorAccountOperate(ctx context.Context, principal auth.Token, acc routing.VendorAccount) error {
	if isVendorAccountOwner(principal, acc) {
		return nil
	}
	if !vendorAccountSharedLevel(s.effectiveVendorAccountShareLevel(ctx, acc)) {
		return ErrVendorAccountNotFound
	}
	manager, err := s.isVendorAccountManager(ctx, principal, acc)
	if err != nil {
		return err
	}
	if manager {
		return nil
	}
	return ErrVendorAccountNotFound
}

// authorizeVendorAccountCredential is the gate of every operation that
// establishes, replaces or clears the account's credential or acts as its
// strongest lever (connect, reconnect, device connect, import, api key set or
// change, and the rows the spec ties to it: delete and downgrade). Allowed for:
//
//   - the owner, at every level;
//   - a CM of the account, at `full` only (complete owner-equivalent delegation).
//
// The two denials differ on purpose:
//
//   - a CM at `shared` gets ErrVendorAccountForbidden (403): they can already see
//     and operate the account, so "not found" would be a lie;
//   - anyone who cannot see the account at all (a stranger, a co-manager without
//     the vendor-account flag, system scope, a principal with no user identity, or a
//     CM whose link outlived the shared level) gets ErrVendorAccountNotFound
//     (404-no-leak), identical to an unknown id.
//
// Like the operate gate it works on the EFFECTIVE share level (the stored level while
// sharing is enabled, `none` while the master flag is off; read once, so one call
// cannot see two answers), and an unrecognised level is treated as private.
func (s *Service) authorizeVendorAccountCredential(ctx context.Context, principal auth.Token, acc routing.VendorAccount) error {
	if isVendorAccountOwner(principal, acc) {
		return nil
	}
	level := s.effectiveVendorAccountShareLevel(ctx, acc)
	if !vendorAccountSharedLevel(level) {
		return ErrVendorAccountNotFound
	}
	manager, err := s.isVendorAccountManager(ctx, principal, acc)
	if err != nil {
		return err
	}
	if !manager {
		return ErrVendorAccountNotFound
	}
	if level == routing.VendorShareLevelFull {
		return nil
	}
	return ErrVendorAccountForbidden
}

// isVendorAccountPatchOperateField is the OPERATE ALLOWLIST of the account PATCH:
// the names (Go field names of UpdateVendorAccountRequest) of the only fields a CM
// of a `shared` account may change. It is the single place a PATCH field is granted
// the weaker gate; there is deliberately no matching list of "credential fields",
// because the credential gate is the default and a list of what is sensitive is
// exactly what an overlooked field falls out of. Widening this list widens what a
// co-manager can do to someone else's account, so a test restates it literally
// (TestVendorAccountPatchOperateAllowlistIsExactlyNameStatusModelPrefix) and any
// edit is a visible, reviewed one.
func isVendorAccountPatchOperateField(field string) bool {
	switch field {
	case "Name", "Status", "ModelPrefix":
		return true
	}
	return false
}

// patchTouchesOutsideOperate reports whether the PATCH body (a request struct, by
// value or pointer) carries anything the operate allowlist does not name, i.e.
// whether it needs the credential gate. It fails CLOSED at every step:
//
//   - it walks the struct's fields by reflection, so a field added to the request
//     later is examined without anyone touching this function;
//   - a field outside the allowlist counts as touched when it is present (a non-nil
//     pointer, slice, map or interface), so an api_key sent as "" (clear) is touched;
//   - a field of a value type cannot be "absent", so it always counts as touched:
//     such a field makes every PATCH credential-gated, which fails loudly in the
//     tests rather than quietly granting it the operate gate;
//   - anything that is not a plain struct (including nil) is touched.
//
// Allowlisted fields are skipped here: whether and how they apply is the apply
// functions' business, and they are operate-grade by definition.
func patchTouchesOutsideOperate(body any) bool {
	v := reflect.ValueOf(body)
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return true
	}
	typ := v.Type()
	for i := range typ.NumField() {
		if isVendorAccountPatchOperateField(typ.Field(i).Name) {
			continue
		}
		switch field := v.Field(i); field.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
			if !field.IsNil() {
				return true
			}
		default:
			return true
		}
	}
	return false
}

// authorizeVendorAccountPatch authorizes an account PATCH FIELD BY FIELD, deny by
// default (spec 2026-10-15 section 6): a request whose every present field is on
// the operate allowlist (name, status, model_prefix) takes the operate gate; a
// request that carries ANY other field -- api_key, base_url, auth_type, or one nobody
// has classified yet -- takes the credential gate, and so does a request that mixes
// both kinds (the stricter wins; a CM at `shared` who sneaks an api_key in next to a
// status change is turned away with nothing applied). The decision depends only on
// which fields are present, never on their values: whether an api_key is "really"
// different is not knowable without opening the sealed one.
//
// credentialPatch is patchTouchesOutsideOperate(req), computed ONCE by the caller:
// the same decision that picks the gate also picks the write
// (saveVendorAccountPatch), so an operate-only PATCH can never be authorized as
// one thing and written as another.
func (s *Service) authorizeVendorAccountPatch(ctx context.Context, principal auth.Token, acc routing.VendorAccount, credentialPatch bool) error {
	if credentialPatch {
		return s.authorizeVendorAccountCredential(ctx, principal, acc)
	}
	return s.authorizeVendorAccountOperate(ctx, principal, acc)
}

// rejectVendorAccountImmutableChange enforces the create-only invariant of
// base_url and auth_type: a PATCH may not change either, for any caller. It runs
// AFTER the gate (so a stranger still gets the 404 and a CM at `shared` the 403,
// never a 400 that confirms the account) and BEFORE anything is applied, so a
// request that also carries a valid change writes nothing. A value equal to the
// stored one is not an attempt to change it (a client that round-trips the account
// it read is not refused).
func rejectVendorAccountImmutableChange(acc routing.VendorAccount, req UpdateVendorAccountRequest) error {
	if req.BaseURL != nil && strings.TrimSpace(*req.BaseURL) != acc.BaseURL {
		return fmt.Errorf("%w: base_url", ErrVendorAccountFieldImmutable)
	}
	if req.AuthType != nil && strings.TrimSpace(*req.AuthType) != acc.AuthType {
		return fmt.Errorf("%w: auth_type", ErrVendorAccountFieldImmutable)
	}
	return nil
}

// ListVendorAccounts returns the calling principal's OWN accounts, oldest first,
// each with its rate-limit usage snapshot when one exists (see VendorAccountDTO).
// Like every vendor-account method it is refused with ErrVendorAccountsDisabled
// (before anything else) while the vendor_accounts_enabled master flag is off.
// System scope does not widen the list: the page is a personal one and the DTO
// carries no owner, so a cross-user list would be unattributed.
func (s *Service) ListVendorAccounts(ctx context.Context, principal auth.Token) (VendorAccountListResponse, error) {
	if err := s.requireVendorAccountsEnabled(ctx); err != nil {
		return VendorAccountListResponse{}, err
	}
	out := make([]VendorAccountDTO, 0)
	if principal.UserID == "" {
		return VendorAccountListResponse{Data: out}, nil
	}
	accounts, err := s.routes.VendorAccountsByOwner(ctx, principal.UserID)
	if err != nil {
		return VendorAccountListResponse{}, err
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		if !accounts[i].CreatedAt.Equal(accounts[j].CreatedAt) {
			return accounts[i].CreatedAt.Before(accounts[j].CreatedAt)
		}
		return accounts[i].ID < accounts[j].ID
	})
	for _, acc := range accounts {
		dto, err := s.vendorAccountDTO(ctx, acc)
		if err != nil {
			return VendorAccountListResponse{}, err
		}
		// The snapshot only decorates the row: a failed read leaves Usage nil
		// (the row renders without a usage panel) rather than failing the whole
		// Providers list. GetVendorAccount, the detail read, surfaces the error.
		if dto.Usage, err = s.vendorAccountUsageDTO(ctx, acc.ID); err != nil {
			slog.Debug("vendor accounts list: usage snapshot unavailable, listing the account without it",
				"account_id", acc.ID, "err", err)
			dto.Usage = nil
		}
		out = append(out, dto)
	}
	return VendorAccountListResponse{Data: out}, nil
}

// GetVendorAccount returns one account the principal owns (404-no-leak
// otherwise); system scope may read any account. It carries the account's
// rate-limit usage snapshot (Usage; nil until one has been scraped) and, unlike
// the list, fails when that snapshot cannot be read. ErrVendorAccountsDisabled
// while the master flag is off.
func (s *Service) GetVendorAccount(ctx context.Context, principal auth.Token, id string) (VendorAccountDTO, error) {
	if err := s.requireVendorAccountsEnabled(ctx); err != nil {
		return VendorAccountDTO{}, err
	}
	acc, err := s.authorizeVendorAccount(ctx, principal, id, false)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	dto, err := s.vendorAccountDTO(ctx, acc)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	if dto.Usage, err = s.vendorAccountUsageDTO(ctx, acc.ID); err != nil {
		return VendorAccountDTO{}, err
	}
	return dto, nil
}

// CreateVendorAccount creates an account owned by the calling principal.
// ErrVendorAccountsDisabled while the master flag is off, and
// ErrVendorSubscriptionsDisabled for a subscription account while
// vendor_subscriptions_enabled is off. Any authenticated user may create their
// own (no admin requirement); a principal with no user identity cannot own one.
// The api key is sealed BEFORE the store write, so a keyless disk store fails
// with capture.ErrKeyRequired and persists nothing.
func (s *Service) CreateVendorAccount(ctx context.Context, principal auth.Token, req CreateVendorAccountRequest) (VendorAccountDTO, error) {
	if err := s.requireVendorAccountsEnabled(ctx); err != nil {
		return VendorAccountDTO{}, err
	}
	if principal.UserID == "" {
		return VendorAccountDTO{}, ErrVendorAccountForbidden
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return VendorAccountDTO{}, ErrVendorAccountNameRequired
	}
	vendor, err := normalizeVendorAccountVendor(req.Vendor)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	authType, err := normalizeVendorAccountAuthType(req.AuthType)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	// A preset vendor (OpenAI-compatible or native Gemini) is api-key only (it has
	// no subscription/OAuth flow) and carries an immutable root url. openai and
	// anthropic keep their fixed hosts: a supplied base_url is ignored and BaseURL
	// stays "".
	baseURL := ""
	if routing.IsPresetVendor(vendor) {
		if authType != routing.VendorAuthAPIKey {
			return VendorAccountDTO{}, ErrVendorAccountAuthTypeInvalid
		}
		if baseURL, err = normalizeVendorAccountBaseURL(vendor, req.BaseURL); err != nil {
			return VendorAccountDTO{}, err
		}
	}
	status := strings.TrimSpace(req.Status)
	if status == "" {
		status = routing.VendorAccountStatusActive
	}
	if status, err = normalizeVendorAccountStatus(status); err != nil {
		return VendorAccountDTO{}, err
	}
	modelPrefix, err := normalizeVendorAccountModelPrefix(req.ModelPrefix)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	apiKey := strings.TrimSpace(req.APIKey)
	if apiKey != "" && authType != routing.VendorAuthAPIKey {
		return VendorAccountDTO{}, ErrVendorAccountAPIKeyNotAllowed
	}
	// vendor_subscriptions_enabled: while off, a SUBSCRIPTION account cannot be
	// created (an api_key one is unaffected). It is deliberately the LAST check,
	// after every request validation, so it masks no other answer: a malformed
	// create (bad status, bad model_prefix, a supplied api_key) keeps its specific
	// 400, and the preset block above has already answered a preset vendor with
	// ErrVendorAccountAuthTypeInvalid (a 400) whatever the switch says, so only a
	// well-formed openai or anthropic subscription create reaches it. Checked
	// before the key is sealed or anything is stored, so a refusal persists nothing.
	if authType == routing.VendorAuthSubscription && !s.VendorSubscriptionsEnabled(ctx) {
		return VendorAccountDTO{}, ErrVendorSubscriptionsDisabled
	}
	sealedKey, err := s.sealVendorAPIKey(apiKey)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	now := s.clock().UTC()
	acc := routing.VendorAccount{
		ID:          "va_" + compactRandomHex(16),
		OwnerUserID: principal.UserID,
		Vendor:      vendor,
		AuthType:    authType,
		Name:        name,
		Status:      status,
		APIKey:      sealedKey,
		ModelPrefix: modelPrefix,
		BaseURL:     baseURL,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.routes.CreateVendorAccount(ctx, acc); err != nil {
		return VendorAccountDTO{}, err
	}
	// Seed the vendor's curated model catalog (under the account's prefix) so the
	// account is routable from the start. It is only a fallback: a connected
	// account replaces it with what the vendor reports (RefreshVendorAccountModels).
	// An account without its rows would silently serve nothing, so a seeding
	// failure fails the create and removes the half-made account.
	//
	// The most plausible reason a seed fails on a SQL driver is that ctx died
	// (the client went away, the request timed out); a cleanup on that same ctx
	// would fail identically and strand an unroutable account no one backfills.
	// So the cleanup runs on a context of its own: severed from ctx's
	// cancellation and bounded by vendorAccountCleanupTimeout. If it fails too,
	// the account is left behind unseeded and BOTH faults are returned, so the
	// double fault is visible rather than dropped.
	seed, _ := relabelVendorModels(VendorCatalog(vendor, authType), modelPrefix)
	if err := s.routes.SetVendorAccountModels(ctx, acc.ID, seed); err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), vendorAccountCleanupTimeout)
		defer cancel()
		if delErr := s.routes.DeleteVendorAccount(cctx, acc.ID); delErr != nil {
			err = errors.Join(err, fmt.Errorf("remove unseeded vendor account %s: %w", acc.ID, delErr))
		}
		return VendorAccountDTO{}, err
	}
	dto, err := s.vendorAccountDTO(ctx, acc)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	// A preset account (OpenAI-compatible or native Gemini) has no static seed (its
	// catalog is empty), so without a discovery it would serve nothing until the
	// user pressed Refresh: ask the provider once now, best effort, so a new account
	// is usable at once. It needs the key just stored. It never fails the create
	// (the account exists and Refresh re-runs it) and is bounded by the connect-time
	// discovery timeout.
	if routing.IsPresetVendor(vendor) && apiKey != "" {
		dto = s.discoverAfterConnect(ctx, principal, dto)
	}
	return dto, nil
}

// applyVendorAccountName applies a name change when the request carries one; an
// all-whitespace name is rejected.
func applyVendorAccountName(acc *routing.VendorAccount, name *string) error {
	if name == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*name)
	if trimmed == "" {
		return ErrVendorAccountNameRequired
	}
	acc.Name = trimmed
	return nil
}

// applyVendorAccountStatus applies a status change when the request carries one
// that differs from the stored value. A status equal to the stored one is a no-op,
// which lets a form echo back a system-managed needs_reconnect without tripping the
// user-settable check.
func applyVendorAccountStatus(acc *routing.VendorAccount, status *string) error {
	if status == nil || strings.TrimSpace(*status) == acc.Status {
		return nil
	}
	normalized, err := normalizeVendorAccountStatus(*status)
	if err != nil {
		return err
	}
	acc.Status = normalized
	return nil
}

// applyVendorAccountModelPrefix applies a model-prefix change when the request
// carries one: nil keeps the stored prefix, "" clears it, any other value is
// validated (normalizeVendorAccountModelPrefix) and replaces it.
func applyVendorAccountModelPrefix(acc *routing.VendorAccount, raw *string) error {
	if raw == nil {
		return nil
	}
	prefix, err := normalizeVendorAccountModelPrefix(*raw)
	if err != nil {
		return err
	}
	acc.ModelPrefix = prefix
	return nil
}

// applyVendorAccountAPIKey seals and applies an api-key change when the request
// carries one. Only the exact empty string clears the key; a value that is blank
// after trimming is a paste slip and must not silently wipe it. A key may be set
// only on an api_key account.
func (s *Service) applyVendorAccountAPIKey(acc *routing.VendorAccount, raw *string) error {
	if raw == nil {
		return nil
	}
	apiKey := strings.TrimSpace(*raw)
	if *raw != "" && apiKey == "" {
		return ErrVendorAccountAPIKeyInvalid
	}
	if apiKey != "" && acc.AuthType != routing.VendorAuthAPIKey {
		return ErrVendorAccountAPIKeyNotAllowed
	}
	sealedKey, err := s.sealVendorAPIKey(apiKey)
	if err != nil {
		return err
	}
	acc.APIKey = sealedKey
	return nil
}

// saveVendorAccountPatch persists a PATCH that has been authorized and applied to
// acc (the copy loaded before, mutated in Go), and returns the account as it is
// stored afterwards, for the response.
//
// The write follows the gate. A credentialPatch (the credential gate passed:
// api_key or another non-operate field is present) writes the whole row, as it
// always has: its actor may change the credential columns. An operate-only PATCH
// writes ONLY the operate columns it was asked to change and updated_at
// (UpdateVendorAccountOperateFields), because the load-modify-write is not atomic
// and a full-row write would carry the credential columns of its stale copy back
// into the row: a CM of a `shared` account sending a status change would revert
// the owner's api key rotation, a dispatch-time refresh-token rotation or the
// needs_reconnect flip, writing a credential column without ever passing the
// credential gate. Name and model_prefix are written when the request carries
// them (the request is the intent); status only when it differs from the loaded
// one (a status that merely echoes the stored value, which applyVendorAccountStatus
// treats as a no-op, must not be written back over a concurrent change).
//
// loaded is the record as it was read, applied the one after the apply* calls. A
// field added to the operate allowlist must be added to the patch built here as
// well; forgetting only means it is not persisted, never that a credential column
// is written.
func (s *Service) saveVendorAccountPatch(ctx context.Context, loaded, applied routing.VendorAccount, req UpdateVendorAccountRequest, credentialPatch bool) (routing.VendorAccount, error) {
	if credentialPatch {
		if err := s.routes.UpdateVendorAccount(ctx, applied); err != nil {
			return routing.VendorAccount{}, notFoundAsVendorAccountNotFound(err)
		}
		return applied, nil
	}
	patch := routing.VendorAccountOperatePatch{UpdatedAt: applied.UpdatedAt}
	if req.Name != nil {
		patch.Name = &applied.Name
	}
	if applied.Status != loaded.Status {
		patch.Status = &applied.Status
	}
	if req.ModelPrefix != nil {
		patch.ModelPrefix = &applied.ModelPrefix
	}
	if err := s.routes.UpdateVendorAccountOperateFields(ctx, applied.ID, patch); err != nil {
		return routing.VendorAccount{}, notFoundAsVendorAccountNotFound(err)
	}
	// The row is not the stale copy any more (a credential may have moved since the
	// load), so describe what is actually stored.
	current, err := s.routes.VendorAccountByID(ctx, applied.ID)
	if err != nil {
		return routing.VendorAccount{}, notFoundAsVendorAccountNotFound(err)
	}
	return current, nil
}

// UpdateVendorAccount renames an account, changes its status or model prefix,
// and/or replaces or clears its api key. The store keeps id, owner, vendor and
// created_at immutable, so this loads the row, mutates only the requested fields
// and writes them back.
//
// Authorization is field-level and deny-by-default (authorizeVendorAccountPatch):
// name, status and model_prefix need the operate gate (the owner, or a CM of a
// shared/full account); a request carrying anything else -- the api key above all
// -- needs the credential gate (the owner, or a CM of a `full` account; a CM of a
// `shared` one is ErrVendorAccountForbidden and nothing is written). System scope
// is a stranger to both. base_url and auth_type are create-only: a request that
// tries to change either is ErrVendorAccountFieldImmutable. Authorization runs
// first, so a stranger gets 404 even for an invalid body.
//
// The write follows the gate (saveVendorAccountPatch): an operate-only PATCH
// writes only the operate columns it changes and never a credential column, so it
// cannot revert a concurrent credential write.
//
// ErrVendorAccountsDisabled while the master flag is off.
func (s *Service) UpdateVendorAccount(ctx context.Context, principal auth.Token, id string, req UpdateVendorAccountRequest) (VendorAccountDTO, error) {
	if err := s.requireVendorAccountsEnabled(ctx); err != nil {
		return VendorAccountDTO{}, err
	}
	acc, err := s.loadVendorAccount(ctx, id)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	credentialPatch := patchTouchesOutsideOperate(req)
	if err := s.authorizeVendorAccountPatch(ctx, principal, acc, credentialPatch); err != nil {
		return VendorAccountDTO{}, err
	}
	if err := rejectVendorAccountImmutableChange(acc, req); err != nil {
		return VendorAccountDTO{}, err
	}
	loaded := acc
	if err := applyVendorAccountName(&acc, req.Name); err != nil {
		return VendorAccountDTO{}, err
	}
	if err := applyVendorAccountStatus(&acc, req.Status); err != nil {
		return VendorAccountDTO{}, err
	}
	if err := applyVendorAccountModelPrefix(&acc, req.ModelPrefix); err != nil {
		return VendorAccountDTO{}, err
	}
	if err := s.applyVendorAccountAPIKey(&acc, req.APIKey); err != nil {
		return VendorAccountDTO{}, err
	}
	acc.UpdatedAt = s.clock().UTC()
	saved, err := s.saveVendorAccountPatch(ctx, loaded, acc, req, credentialPatch)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	// A request that carries a prefix re-labels the account's model rows to it
	// (prefix + upstream id, no re-discovery). It is keyed on the request, not on
	// a changed value, so re-sending the stored prefix heals rows a failed
	// re-label left behind; relabelVendorAccountModels writes only when a row
	// actually differs.
	if req.ModelPrefix != nil {
		if err := s.relabelVendorAccountModels(ctx, saved.ID); err != nil {
			return VendorAccountDTO{}, err
		}
	}
	return s.vendorAccountDTO(ctx, saved)
}

// DeleteVendorAccount removes an account; the store cascades its dependent rows
// (the model catalog, the management-group and resource-group links). It is the
// CREDENTIAL gate's row (spec 2026-10-15 section 6): the owner at every level, a CM
// of a `full` account (complete owner-equivalent delegation); a CM of a `shared`
// account is ErrVendorAccountForbidden (they can see it, so "not found" would be a
// lie) and everyone else -- system scope included -- ErrVendorAccountNotFound
// (404-no-leak). The bool reports that a row was removed (always true on a nil
// error): there is no best-effort side effect to flag, unlike DeleteServer.
// ErrVendorAccountsDisabled while the module flag is off. Deleting is not a sharing
// endpoint, so the sharing flag does not REFUSE it; but the credential gate reads the
// effective share level, so while vendor_account_sharing_enabled is off a CM is a
// stranger (404) and only the owner can delete.
func (s *Service) DeleteVendorAccount(ctx context.Context, principal auth.Token, id string) (bool, error) {
	if err := s.requireVendorAccountsEnabled(ctx); err != nil {
		return false, err
	}
	acc, err := s.loadVendorAccount(ctx, id)
	if err != nil {
		return false, err
	}
	if err := s.authorizeVendorAccountCredential(ctx, principal, acc); err != nil {
		return false, err
	}
	if err := s.routes.DeleteVendorAccount(ctx, acc.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, ErrVendorAccountNotFound
		}
		return false, err
	}
	s.vendorUsagePulls.forget(acc.ID)
	return true, nil
}
