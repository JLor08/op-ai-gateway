// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"reflect"
	"strings"
	"testing"
)

// This file is the field-level authorization contract of the vendor-account
// PATCH and of the credential-operation methods (#183, spec 2026-10-15 section 6).
//
// The PATCH is DENY-BY-DEFAULT: name, status and model_prefix are the only fields
// that ride the operate gate (owner OR CM at shared/full). Anything else a PATCH
// carries -- api_key, and every field somebody adds to the request later -- rides
// the credential gate (owner always; CM at full only; a CM at shared is 403). A
// request that mixes both kinds needs the stricter gate. base_url and auth_type
// are create-only: a PATCH that tries to change them is refused (after the gate,
// so a stranger still gets the 404).
//
// The fall-through this guards against is the quiet one: the credential gate is
// chosen by looking for the fields known to be sensitive, so a field nobody
// classified lands on the weaker gate. The tests below therefore pin the DEFAULT
// (an unclassified field is credential-gated) three ways: by reflection over the
// real request struct, by a synthetic struct standing in for a future field, and
// end to end through UpdateVendorAccount.

const (
	patchOwnerKey = "sk-owner-key-do-not-echo"
	patchNewKey   = "sk-replacement-key-do-not-echo"
)

// newVendorPatchEnv is the authorization fixture with volatile ("plain:") sealing,
// so a PATCH or a connect that IS permitted can actually write a credential.
func newVendorPatchEnv(t *testing.T) *vendorAuthzEnv {
	t.Helper()
	e := newVendorAuthzEnv(t)
	e.svc.settingsVolatile = true
	return e
}

// keyedAccountAt creates an api-key OpenAI account that carries a stored key (so a
// denied key write has a credential to leave untouched) and puts it at level.
func (e *vendorAuthzEnv) keyedAccountAt(level string) routing.VendorAccount {
	e.t.Helper()
	acc := e.newAccountOf(level, CreateVendorAccountRequest{
		Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey, Name: "keyed-" + level, APIKey: patchOwnerKey,
	})
	if acc.APIKey == "" {
		e.t.Fatalf("keyed account at %s has no stored key", level)
	}
	return acc
}

// connectedSubscriptionAt creates an Anthropic subscription account, connects it
// as the owner (so every later call is a RE-connect with a token set to lose) and
// puts it at level. Anthropic keeps the device flow out of reach: a call that
// passes the gate answers ErrVendorAccountDeviceUnsupported instead of dialling
// OpenAI.
func (e *vendorAuthzEnv) connectedSubscriptionAt(level string) routing.VendorAccount {
	e.t.Helper()
	acc := e.newAccountOf(level, CreateVendorAccountRequest{
		Vendor: routing.VendorAnthropic, AuthType: routing.VendorAuthSubscription, Name: "sub-" + level,
	})
	if _, err := e.svc.ConnectVendorAccountImport(e.ctx, token(vendorAuthzOwnerID), acc.ID, ConnectVendorAccountImportRequest{AccessToken: "sk-ant-oat01-owner-token"}); err != nil {
		e.t.Fatalf("owner connect at %s: %v", level, err)
	}
	acc = e.reload(acc.ID)
	if acc.OAuthTokens == "" {
		e.t.Fatalf("subscription account at %s is not connected", level)
	}
	return acc
}

// vendorGateAllowed says who passes a gate at each level; every other principal in
// vendorAuthzPrincipals is denied. Operate: the owner, and a CM (the flagged
// co-manager and the admin-group owner, who counts as one) at shared/full.
// Credential: the owner, and a CM at full only.
func vendorGateAllowed(credential bool, level, principal string) bool {
	switch principal {
	case "owner":
		return true
	case "cm", "admin-group owner":
		if credential {
			return level == routing.VendorShareLevelFull
		}
		return level != routing.VendorShareLevelNone
	}
	return false
}

// vendorGateDenial is the answer a denied principal must get from the gate:
// a CM who can already see the account at shared is told 403, everyone else 404.
func vendorGateDenial(credential bool, level, principal string) authzOutcome {
	if credential && level == routing.VendorShareLevelShared && (principal == "cm" || principal == "admin-group owner") {
		return authzForbidden
	}
	return authzNotFound
}

// requireGatePassed fails when err says the caller was turned away at the gate
// (404 or 403). Any other outcome -- success or a later, domain-level refusal such
// as "not a subscription account" -- means the gate let the call through.
func requireGatePassed(t *testing.T, label string, err error) {
	t.Helper()
	if errors.Is(err, ErrVendorAccountNotFound) || errors.Is(err, ErrVendorAccountForbidden) {
		t.Errorf("%s: err = %v, want the gate to let the call through", label, err)
	}
}

func (e *vendorAuthzEnv) patch(p auth.Token, id string, req UpdateVendorAccountRequest) error {
	_, err := e.svc.UpdateVendorAccount(e.ctx, p, id, req)
	return err
}

// TestVendorAccountPatchGateFollowsTheFieldsTouched drives UpdateVendorAccount for
// every kind of principal at every share level with four request shapes, and pins
// which gate each shape rides: operate fields -> operate gate; api_key (set or
// clear) -> credential gate; a mix -> the credential gate (the stricter wins). A
// denied call must answer exactly 404 or 403 and write nothing.
func TestVendorAccountPatchGateFollowsTheFieldsTouched(t *testing.T) {
	e := newVendorPatchEnv(t)

	shapes := []struct {
		name       string
		credential bool
		req        UpdateVendorAccountRequest
		// applied checks that a PERMITTED call really changed what it asked for.
		applied func(t *testing.T, label string, got routing.VendorAccount)
	}{
		{
			name: "operate fields", credential: false,
			req: UpdateVendorAccountRequest{Name: strPtr("Renamed"), Status: strPtr(routing.VendorAccountStatusDisabled), ModelPrefix: strPtr("cm-")},
			applied: func(t *testing.T, label string, got routing.VendorAccount) {
				if got.Name != "Renamed" || got.Status != routing.VendorAccountStatusDisabled || got.ModelPrefix != "cm-" {
					t.Errorf("%s: stored name/status/prefix = %q/%q/%q, want the requested values", label, got.Name, got.Status, got.ModelPrefix)
				}
			},
		},
		{
			name: "api_key set", credential: true,
			req: UpdateVendorAccountRequest{APIKey: strPtr(patchNewKey)},
			applied: func(t *testing.T, label string, got routing.VendorAccount) {
				if got.APIKey != "plain:"+patchNewKey {
					t.Errorf("%s: stored key = %q, want the replacement sealed", label, got.APIKey)
				}
			},
		},
		{
			name: "api_key clear", credential: true,
			req: UpdateVendorAccountRequest{APIKey: strPtr("")},
			applied: func(t *testing.T, label string, got routing.VendorAccount) {
				if got.APIKey != "" {
					t.Errorf("%s: stored key = %q, want it cleared", label, got.APIKey)
				}
			},
		},
		{
			name: "operate field plus api_key", credential: true,
			req: UpdateVendorAccountRequest{Status: strPtr(routing.VendorAccountStatusDisabled), APIKey: strPtr(patchNewKey)},
			applied: func(t *testing.T, label string, got routing.VendorAccount) {
				if got.Status != routing.VendorAccountStatusDisabled || got.APIKey != "plain:"+patchNewKey {
					t.Errorf("%s: stored status/key = %q/%q, want both applied", label, got.Status, got.APIKey)
				}
			},
		},
	}

	for _, level := range []string{routing.VendorShareLevelNone, routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		for _, shape := range shapes {
			for _, p := range vendorAuthzPrincipals() {
				label := shape.name + "/" + level + "/" + p.name
				acc := e.keyedAccountAt(level)
				before := e.snapshot(acc.ID)

				err := e.patch(p.tok, acc.ID, shape.req)
				if vendorGateAllowed(shape.credential, level, p.name) {
					if err != nil {
						t.Errorf("%s: err = %v, want allow", label, err)
						continue
					}
					shape.applied(t, label, e.reload(acc.ID))
					continue
				}
				requireAuthzOutcome(t, label, err, vendorGateDenial(shape.credential, level, p.name))
				e.requireUnchanged(label, before)
			}
		}
	}
}

// TestVendorAccountPatchCMAtSharedCannotTouchTheCredential is the review-focus
// pin in its plainest form: a CM of a `shared` account may rename it, disable it
// and set its prefix, but a PATCH that carries the api key -- alone, to clear it,
// or smuggled in next to an operate field -- is 403 and writes NOTHING (the
// operate field of a mixed request is not applied either, and the stored key is
// byte-identical).
func TestVendorAccountPatchCMAtSharedCannotTouchTheCredential(t *testing.T) {
	e := newVendorPatchEnv(t)
	cm := token("usr_cm", "admin")
	acc := e.keyedAccountAt(routing.VendorShareLevelShared)

	for name, req := range map[string]UpdateVendorAccountRequest{
		"api_key set":                 {APIKey: strPtr(patchNewKey)},
		"api_key clear":               {APIKey: strPtr("")},
		"status plus api_key":         {Status: strPtr(routing.VendorAccountStatusDisabled), APIKey: strPtr(patchNewKey)},
		"name plus prefix plus a key": {Name: strPtr("Taken over"), ModelPrefix: strPtr("x-"), APIKey: strPtr(patchNewKey)},
	} {
		before := e.snapshot(acc.ID)
		err := e.patch(cm, acc.ID, req)
		requireAuthzOutcome(t, name, err, authzForbidden)
		e.requireUnchanged(name, before)
		if got := e.reload(acc.ID); got.APIKey != before.Account.APIKey || got.APIKey == "" {
			t.Errorf("%s: stored key = %q, want the owner's %q untouched", name, got.APIKey, before.Account.APIKey)
		}
	}

	// The same CM operates normally: each operate field persists, the credential
	// never moves, and a prefix change re-labels the model rows.
	keyBefore := e.reload(acc.ID).APIKey
	for name, req := range map[string]UpdateVendorAccountRequest{
		"rename":       {Name: strPtr("CM renamed")},
		"disable":      {Status: strPtr(routing.VendorAccountStatusDisabled)},
		"model prefix": {ModelPrefix: strPtr("team-")},
	} {
		if err := e.patch(cm, acc.ID, req); err != nil {
			t.Fatalf("CM %s at shared: %v, want allow", name, err)
		}
	}
	got := e.reload(acc.ID)
	if got.Name != "CM renamed" || got.Status != routing.VendorAccountStatusDisabled || got.ModelPrefix != "team-" {
		t.Fatalf("stored name/status/prefix = %q/%q/%q, want the CM's changes", got.Name, got.Status, got.ModelPrefix)
	}
	if got.APIKey != keyBefore || got.OAuthTokens != "" {
		t.Fatalf("credential after the CM's operate PATCHes = %q/%q, want it untouched", got.APIKey, got.OAuthTokens)
	}
	models, err := e.routes.VendorAccountModels(e.ctx, acc.ID)
	if err != nil || len(models) == 0 {
		t.Fatalf("models = %v, %v, want the account's catalog", models, err)
	}
	for _, m := range models {
		if m.GatewayModel != "team-"+m.UpstreamModel {
			t.Errorf("model %q is not re-labelled to the CM's prefix (upstream %q)", m.GatewayModel, m.UpstreamModel)
		}
	}
}

// TestVendorAccountPatchEmptyBodyTouchesNothing: a PATCH that carries no field has
// nothing to authorize beyond operate, so it takes the weaker gate (and, like for
// the owner today, changes nothing but updated_at).
func TestVendorAccountPatchEmptyBodyTouchesNothing(t *testing.T) {
	e := newVendorPatchEnv(t)
	acc := e.keyedAccountAt(routing.VendorShareLevelShared)
	if err := e.patch(token("usr_cm", "admin"), acc.ID, UpdateVendorAccountRequest{}); err != nil {
		t.Fatalf("CM empty PATCH at shared: %v, want allow (operate gate)", err)
	}
	requireAuthzOutcome(t, "stranger empty PATCH", e.patch(token("usr_stranger"), acc.ID, UpdateVendorAccountRequest{}), authzNotFound)
	got := e.reload(acc.ID)
	if got.Name != acc.Name || got.APIKey != acc.APIKey || got.Status != acc.Status {
		t.Fatalf("empty PATCH changed the account: %+v -> %+v", acc, got)
	}
}

// TestVendorAccountPatchRejectsChangingBaseURLOrAuthType pins the invariant: base_url
// and auth_type are create-only. A PATCH that tries to change either is refused with
// ErrVendorAccountFieldImmutable for everyone allowed to PATCH credential fields
// (the owner; a CM at full), nothing written even when the request also carries a
// valid change. The gate still comes first: a CM at shared is 403 and a stranger is
// 404, so the refusal cannot be used to probe an account. A value that merely echoes
// what is stored is not a change.
func TestVendorAccountPatchRejectsChangingBaseURLOrAuthType(t *testing.T) {
	e := newVendorPatchEnv(t)
	const baseURL = "https://gw.example.test"
	newCompat := func(level string) routing.VendorAccount {
		return e.newAccountOf(level, CreateVendorAccountRequest{
			Vendor: routing.VendorOpenAICompatible, AuthType: routing.VendorAuthAPIKey, Name: "compat-" + level, APIKey: patchOwnerKey, BaseURL: baseURL,
		})
	}

	attempts := map[string]UpdateVendorAccountRequest{
		"base_url":                    {BaseURL: strPtr("https://other.example.test")},
		"auth_type":                   {AuthType: strPtr(routing.VendorAuthSubscription)},
		"base_url plus a valid name":  {Name: strPtr("Renamed"), BaseURL: strPtr("https://other.example.test")},
		"auth_type plus a valid name": {Name: strPtr("Renamed"), AuthType: strPtr(routing.VendorAuthSubscription)},
		"both":                        {BaseURL: strPtr("https://other.example.test"), AuthType: strPtr(routing.VendorAuthSubscription)},
		"base_url cleared":            {BaseURL: strPtr("")},
	}
	for _, level := range []string{routing.VendorShareLevelNone, routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		acc := newCompat(level)
		for name, req := range attempts {
			for _, p := range vendorAuthzPrincipals() {
				label := name + "/" + level + "/" + p.name
				before := e.snapshot(acc.ID)
				err := e.patch(p.tok, acc.ID, req)
				if vendorGateAllowed(true, level, p.name) {
					if !errors.Is(err, ErrVendorAccountFieldImmutable) {
						t.Errorf("%s: err = %v, want ErrVendorAccountFieldImmutable", label, err)
					}
				} else {
					requireAuthzOutcome(t, label, err, vendorGateDenial(true, level, p.name))
				}
				e.requireUnchanged(label, before)
			}
		}
	}

	// An echo of the stored values is no attempt to change them: it is a no-op that
	// does not block the rest of the request.
	acc := newCompat(routing.VendorShareLevelNone)
	echo := UpdateVendorAccountRequest{Name: strPtr("Echoed"), BaseURL: strPtr(baseURL), AuthType: strPtr(routing.VendorAuthAPIKey)}
	if err := e.patch(token(vendorAuthzOwnerID), acc.ID, echo); err != nil {
		t.Fatalf("owner PATCH echoing base_url/auth_type: %v, want allow", err)
	}
	if got := e.reload(acc.ID); got.Name != "Echoed" || got.BaseURL != baseURL || got.AuthType != routing.VendorAuthAPIKey {
		t.Fatalf("after the echo: name/base_url/auth_type = %q/%q/%q", got.Name, got.BaseURL, got.AuthType)
	}
}

// TestVendorAccountPatchOperateAllowlistIsExactlyNameStatusModelPrefix walks the
// real request struct. The operate allowlist is the ONLY place a field is granted
// the weaker gate, so it is asserted literally: a change to it fails here and must
// be a deliberate edit of this list, in review. Every other field -- including one
// added tomorrow -- must come out credential-gated, and every field must be
// nillable (a pointer), because a value-typed field cannot say "absent" and so
// could never be left out of a PATCH.
func TestVendorAccountPatchOperateAllowlistIsExactlyNameStatusModelPrefix(t *testing.T) {
	operate := map[string]bool{"Name": true, "Status": true, "ModelPrefix": true}
	typ := reflect.TypeFor[UpdateVendorAccountRequest]()
	seen := map[string]bool{}
	for i := range typ.NumField() {
		field := typ.Field(i)
		seen[field.Name] = true
		if field.Type.Kind() != reflect.Pointer {
			t.Errorf("field %s is a %s: PATCH fields must be pointers so an absent field is distinguishable", field.Name, field.Type.Kind())
			continue
		}
		if got := isVendorAccountPatchOperateField(field.Name); got != operate[field.Name] {
			t.Errorf("isVendorAccountPatchOperateField(%s) = %v, want %v", field.Name, got, operate[field.Name])
		}

		// Set ONLY this field: it must select the gate its classification names.
		req := reflect.New(typ).Elem()
		req.Field(i).Set(reflect.New(field.Type.Elem()))
		touchesBeyondOperate := patchTouchesOutsideOperate(req.Interface())
		if touchesBeyondOperate == operate[field.Name] {
			t.Errorf("a PATCH carrying only %s: credential gate = %v, want %v", field.Name, touchesBeyondOperate, !operate[field.Name])
		}
	}
	for name := range operate {
		if !seen[name] {
			t.Errorf("the allowlist names %s, which is not a field of UpdateVendorAccountRequest", name)
		}
	}
	if patchTouchesOutsideOperate(UpdateVendorAccountRequest{}) {
		t.Error("an empty PATCH must not need the credential gate")
	}
}

// TestPatchTouchesOutsideOperateFailsClosed is the fall-through guard, aimed at the
// MECHANISM rather than at today's struct: bodies shaped like a future request, with
// fields nobody has classified. Only a field the allowlist names may stay on the
// operate gate; a new field that is set, a value-typed field (which cannot be
// "absent", so it counts as always touched) and anything that is not a plain
// request struct all demand the credential gate.
func TestPatchTouchesOutsideOperateFailsClosed(t *testing.T) {
	type known struct {
		Name        *string
		Status      *string
		ModelPrefix *string
	}
	type withNewPointer struct {
		Name        *string
		Status      *string
		ModelPrefix *string
		Region      *string // a field added later, never classified
	}
	type withNewValue struct {
		Name *string
		Flag bool // a value-typed field added later
	}
	type withNewSlice struct {
		Name    *string
		Headers []string // a nillable non-pointer field added later
	}
	type withNewMap struct {
		Name  *string
		Extra map[string]string
	}

	tests := []struct {
		name string
		body any
		want bool
	}{
		{"known fields all set", known{strPtr("n"), strPtr("s"), strPtr("p")}, false},
		{"known fields none set", known{}, false},
		{"pointer to the known struct", &known{Name: strPtr("n")}, false},
		{"new pointer field unset", withNewPointer{Name: strPtr("n")}, false},
		{"new pointer field set", withNewPointer{Name: strPtr("n"), Region: strPtr("eu")}, true},
		{"new pointer field set alone", withNewPointer{Region: strPtr("")}, true},
		{"new value field at its zero value", withNewValue{Name: strPtr("n")}, true},
		{"new value field set", withNewValue{Flag: true}, true},
		{"new slice field nil", withNewSlice{Name: strPtr("n")}, false},
		{"new slice field empty but present", withNewSlice{Headers: []string{}}, true},
		{"new map field nil", withNewMap{Name: strPtr("n")}, false},
		{"new map field present", withNewMap{Extra: map[string]string{}}, true},
		{"not a struct", "name", true},
		{"nil", nil, true},
		{"nil pointer to a struct", (*known)(nil), true},
	}
	for _, tc := range tests {
		if got := patchTouchesOutsideOperate(tc.body); got != tc.want {
			t.Errorf("%s: patchTouchesOutsideOperate = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestVendorAccountPatchEveryNonOperateFieldIsCredentialGated is the end-to-end
// half of the fall-through pin. It builds, by reflection, a PATCH for EACH field of
// the real request struct that the operate allowlist does not name, and sends it
// as a CM of a shared account (403, nothing written), as a CM of a full account
// (through the gate) and as a stranger (404). A field added to the struct later is
// picked up here automatically, so if it were ever granted the operate gate by
// mistake this test fails for it.
func TestVendorAccountPatchEveryNonOperateFieldIsCredentialGated(t *testing.T) {
	e := newVendorPatchEnv(t)
	shared := e.keyedAccountAt(routing.VendorShareLevelShared)
	full := e.keyedAccountAt(routing.VendorShareLevelFull)
	cm := token("usr_cm", "admin")
	stranger := token("usr_stranger")

	typ := reflect.TypeFor[UpdateVendorAccountRequest]()
	checked := 0
	for i := range typ.NumField() {
		field := typ.Field(i)
		if isVendorAccountPatchOperateField(field.Name) {
			continue
		}
		if field.Type.Kind() != reflect.Pointer {
			t.Fatalf("field %s is not a pointer; see TestVendorAccountPatchOperateAllowlistIsExactlyNameStatusModelPrefix", field.Name)
		}
		checked++
		only := func() UpdateVendorAccountRequest {
			req := reflect.New(typ).Elem()
			v := reflect.New(field.Type.Elem())
			if v.Elem().Kind() == reflect.String {
				v.Elem().SetString("x")
			}
			req.Field(i).Set(v)
			return req.Interface().(UpdateVendorAccountRequest)
		}

		before := e.snapshot(shared.ID)
		requireAuthzOutcome(t, field.Name+"/CM at shared", e.patch(cm, shared.ID, only()), authzForbidden)
		e.requireUnchanged(field.Name+"/CM at shared", before)

		beforeStranger := e.snapshot(full.ID)
		requireAuthzOutcome(t, field.Name+"/stranger at full", e.patch(stranger, full.ID, only()), authzNotFound)
		e.requireUnchanged(field.Name+"/stranger at full", beforeStranger)

		// A CM of a full account clears the gate; what happens after is the field's own
		// business (the key is replaced, base_url and auth_type are refused as immutable).
		requireGatePassed(t, field.Name+"/CM at full", e.patch(cm, full.ID, only()))
	}
	if checked < 3 {
		t.Fatalf("only %d non-operate fields were exercised, want at least api_key, base_url and auth_type", checked)
	}
}

// credentialOps is the EXHAUSTIVE list of the service methods that establish,
// replace or clear an account's credential (spec section 6): the token import (the
// connect, and a reconnect of an already connected account), the code-paste
// begin/complete pair, the device-code begin/poll pair, and the api key set/change
// (and clear) through the PATCH. A new method of that kind belongs here.
func (e *vendorAuthzEnv) credentialOps() []struct {
	name    string
	account func(level string) routing.VendorAccount
	call    func(p auth.Token, id string) error
} {
	subscription := e.connectedSubscriptionAt
	apiKey := e.keyedAccountAt
	return []struct {
		name    string
		account func(level string) routing.VendorAccount
		call    func(p auth.Token, id string) error
	}{
		{"ConnectVendorAccountImport", subscription, func(p auth.Token, id string) error {
			_, err := e.svc.ConnectVendorAccountImport(e.ctx, p, id, ConnectVendorAccountImportRequest{AccessToken: "sk-ant-oat01-replacement"})
			return err
		}},
		{"BeginVendorAccountConnect", subscription, func(p auth.Token, id string) error {
			_, err := e.svc.BeginVendorAccountConnect(e.ctx, p, id)
			return err
		}},
		{"CompleteVendorAccountConnect", subscription, func(p auth.Token, id string) error {
			_, err := e.svc.CompleteVendorAccountConnect(e.ctx, p, id, "pasted-code")
			return err
		}},
		{"BeginVendorAccountDeviceConnect", subscription, func(p auth.Token, id string) error {
			_, _, err := e.svc.BeginVendorAccountDeviceConnect(e.ctx, p, id)
			return err
		}},
		{"PollVendorAccountDeviceConnect", subscription, func(p auth.Token, id string) error {
			_, err := e.svc.PollVendorAccountDeviceConnect(e.ctx, p, id)
			return err
		}},
		{"UpdateVendorAccount api_key set", apiKey, func(p auth.Token, id string) error {
			return e.patch(p, id, UpdateVendorAccountRequest{APIKey: strPtr(patchNewKey)})
		}},
		{"UpdateVendorAccount api_key clear", apiKey, func(p auth.Token, id string) error {
			return e.patch(p, id, UpdateVendorAccountRequest{APIKey: strPtr("")})
		}},
	}
}

// TestVendorAccountCredentialMethodsUseTheCredentialGate runs every credential
// operation as every kind of principal at every level: the owner always passes, a
// CM passes at full only and is 403 at shared (it can see the account, so "not
// found" would be a lie), everyone else -- system scope included -- is the 404, and
// a denied call leaves the account (credentials, groups, models, usage) exactly as
// it was.
func TestVendorAccountCredentialMethodsUseTheCredentialGate(t *testing.T) {
	e := newVendorPatchEnv(t)
	for _, op := range e.credentialOps() {
		for _, level := range []string{routing.VendorShareLevelNone, routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
			for _, p := range vendorAuthzPrincipals() {
				label := op.name + "/" + level + "/" + p.name
				acc := op.account(level)
				before := e.snapshot(acc.ID)
				err := op.call(p.tok, acc.ID)
				if vendorGateAllowed(true, level, p.name) {
					requireGatePassed(t, label, err)
					continue
				}
				requireAuthzOutcome(t, label, err, vendorGateDenial(true, level, p.name))
				e.requireUnchanged(label, before)
			}
		}
	}
}

// TestVendorAccountCMAtFullReplacesTheCredential shows the permitted side end to
// end: at `full` a CM really does connect (re-connect) a subscription and replace
// an api key, while at `shared` the very same calls leave the stored credential
// byte-identical.
func TestVendorAccountCMAtFullReplacesTheCredential(t *testing.T) {
	e := newVendorPatchEnv(t)
	cm := token("usr_cm", "admin")

	for _, level := range []string{routing.VendorShareLevelShared, routing.VendorShareLevelFull} {
		sub := e.connectedSubscriptionAt(level)
		_, importErr := e.svc.ConnectVendorAccountImport(e.ctx, cm, sub.ID, ConnectVendorAccountImportRequest{AccessToken: "sk-ant-oat01-cm-token"})
		keyed := e.keyedAccountAt(level)
		patchErr := e.patch(cm, keyed.ID, UpdateVendorAccountRequest{APIKey: strPtr(patchNewKey)})

		subAfter, keyedAfter := e.reload(sub.ID), e.reload(keyed.ID)
		if level == routing.VendorShareLevelShared {
			requireAuthzOutcome(t, "CM import at shared", importErr, authzForbidden)
			requireAuthzOutcome(t, "CM api_key at shared", patchErr, authzForbidden)
			if subAfter.OAuthTokens != sub.OAuthTokens || keyedAfter.APIKey != keyed.APIKey {
				t.Errorf("shared: credentials moved although the CM was refused")
			}
			continue
		}
		if importErr != nil || patchErr != nil {
			t.Fatalf("full: CM import = %v, CM api_key = %v, want both allowed", importErr, patchErr)
		}
		if subAfter.OAuthTokens == sub.OAuthTokens || subAfter.OAuthTokens == "" {
			t.Errorf("full: the CM's import did not replace the token set")
		}
		if keyedAfter.APIKey != "plain:"+patchNewKey {
			t.Errorf("full: stored key = %q, want the CM's replacement", keyedAfter.APIKey)
		}
	}
}

// TestVendorAccountConnectReauthorizesAfterTheVendorRoundTrip: the code exchange
// is a network call, and the connect re-loads the account afterwards so as not to
// overwrite a concurrent change. That second read goes through the credential gate
// too, live: a CM whose reach is withdrawn while the exchange is in flight must not
// get to write the token set.
func TestVendorAccountConnectReauthorizesAfterTheVendorRoundTrip(t *testing.T) {
	e := newVendorPatchEnv(t)
	stub := newVendorTokenStub(t)
	e.svc.vendorConnect.anthropic = stub.anthropicEndpoints()
	cm := token("usr_cm", "admin")
	acc := e.connectedSubscriptionAt(routing.VendorShareLevelFull)

	if _, err := e.svc.BeginVendorAccountConnect(e.ctx, cm, acc.ID); err != nil {
		t.Fatalf("CM begin at full: %v", err)
	}
	stub.onRequest(func(*http.Request) {
		if err := e.routes.RemoveVendorAccountAdminGroup(e.ctx, acc.ID, e.ag.ID); err != nil {
			t.Errorf("withdraw the CM's reach: %v", err)
		}
	})
	before := e.snapshot(acc.ID)
	_, err := e.svc.CompleteVendorAccountConnect(e.ctx, cm, acc.ID, "pasted-code")
	requireAuthzOutcome(t, "CM complete after losing reach", err, authzNotFound)
	// Only the withdrawn group differs from the snapshot; the credential is intact.
	if got := e.reload(acc.ID); got.OAuthTokens != before.Account.OAuthTokens {
		t.Errorf("token set moved although the CM lost reach mid-exchange")
	}
}

// TestEveryVendorAccountServiceMethodIsClassified keeps the credential-method list
// EXHAUSTIVE rather than illustrative. Every exported Service method that has
// "VendorAccount" in its name must be filed below under what it does to an
// account; a method that is not filed fails the test, so a credential method added
// later cannot slip past the gate unnoticed -- its author has to say what it is
// and, if it is a credential operation, put it in credentialOps (and behind
// authorizeVendorAccountCredential). The reverse holds as well: everything filed as
// "credential" must be exercised by TestVendorAccountCredentialMethodsUseTheCredentialGate,
// and every name below must still exist.
func TestEveryVendorAccountServiceMethodIsClassified(t *testing.T) {
	class := map[string]string{
		// Reads and the owner's own collection.
		"ListVendorAccounts": "read",
		"GetVendorAccount":   "read",
		// Creating an account is owner self-service: there is no account to gate yet.
		"CreateVendorAccount": "create",
		// The PATCH picks its gate per field (authorizeVendorAccountPatch).
		"UpdateVendorAccount": "patch",
		// Delete is owner-only for now (the safe side of the spec's credential-gate
		// row); widening it to a CM at `full` belongs to its own step.
		"DeleteVendorAccount": "delete",
		// Operate gate (authorizeVendorAccountOperate).
		"TestVendorAccountConnection": "operate",
		"RefreshVendorAccountModels":  "operate",
		"RefreshVendorAccountUsage":   "operate",
		// Credential gate (authorizeVendorAccountCredential), via connectableVendorAccount.
		"ConnectVendorAccountImport":      "credential",
		"BeginVendorAccountConnect":       "credential",
		"CompleteVendorAccountConnect":    "credential",
		"BeginVendorAccountDeviceConnect": "credential",
		"PollVendorAccountDeviceConnect":  "credential",
		// Switch readers: they touch no account.
		"VendorAccountsEnabled":       "setting",
		"VendorAccountSharingEnabled": "setting",
		"VendorAccountRoutingMode":    "setting",
	}

	typ := reflect.TypeFor[*Service]()
	have := map[string]bool{}
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		if !strings.Contains(name, "VendorAccount") {
			continue
		}
		have[name] = true
		if _, ok := class[name]; !ok {
			t.Errorf("Service.%s is not classified: say what it does to an account in this table; a method that sets, replaces or clears a credential must use authorizeVendorAccountCredential and be added to credentialOps", name)
		}
	}
	for name := range class {
		if !have[name] {
			t.Errorf("%s is classified here but is not a Service method (renamed or removed?)", name)
		}
	}

	e := newVendorPatchEnv(t)
	exercised := map[string]bool{}
	for _, op := range e.credentialOps() {
		method, _, _ := strings.Cut(op.name, " ")
		exercised[method] = true
		if kind := class[method]; kind != "credential" && kind != "patch" {
			t.Errorf("credentialOps exercises %s, which is classified %q", method, kind)
		}
	}
	for name, kind := range class {
		if (kind == "credential" || kind == "patch") && !exercised[name] {
			t.Errorf("%s (%s) is not exercised by credentialOps", name, kind)
		}
	}
}
