// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"reflect"
	"testing"
)

// nonReconcileFields is the explicit allowlist of UpdateSystemSettingsRequest
// fields that deliberately trigger NO domain reconcile-side-effect hook via
// touchesSMTP/touchesNetbird/touchesCert. This is not "every field outside
// smtp_*/netbird_*/cert_*" — it also includes the 13 NetBird fields that are
// NetBird-prefixed but outside the narrower netbirdSettingsFields (see that
// var's doc comment): they are simple unconditional top-level writes in
// UpdateSystemSettings with their own ad-hoc side-effect triggers (the
// policy-side-effect goroutine, the gateway-peer reconcile), not gated by
// touchesNetbird.
//
// TestEveryRequestFieldIsClassified is the drift-killer this whole test file
// exists for: it walks every field of UpdateSystemSettingsRequest via
// reflection and fails unless the field is named in one of
// smtpSettingsFields / netbirdSettingsFields / certSettingsFields (in
// service_system_settings.go) or right here. A newly added request field
// then fails CI until a developer consciously puts it in one list or the
// other — the forcing function PT-4 asked for, without the full descriptor-
// registry rewrite.
var nonReconcileFields = []string{
	"Theme",
	"Language",
	"CaptureRetentionDays",
	"CaptureEnabled",
	"CaptureOverride",
	"HealthCheckIntervalSeconds",
	"AgentPresenceTimeoutSeconds",
	"TOTPMode",
	"VisionProbeMode",
	"RouteAffinitySessionMode",
	"EnergyDefaultPricePerKwh",
	"EnergyDefaultPue",
	"EnergyDefaultWhPerToken",
	"EnergyDefaultPriceUnit",
	"CurrencyUsdPerEur",
	"SystemAdminModeRequirePassword",
	"NetbirdOnly",
	"NetbirdAgentDownloadOnly",
	"NetbirdGatewayPeerID",
	"NetbirdGatewayPeerName",
	"NetbirdManagePolicies",
	"NetbirdPolicyScope",
	"NetbirdDenyByDefault",
	"NetbirdDenyByDefaultEnforce",
	"NetbirdAllowPingGateway",
	"NetbirdAllowPingAllServers",
	"NetbirdPeerSyncIntervalSeconds",
	"NetbirdReconcileIntervalSeconds",
	"NetbirdTokenRotateBeforeDays",
	"ResourceProvisioningEnforce",
}

// TestSettingsDomainFieldsExist is the reflection-completeness half of the
// guard: every name in smtpSettingsFields/netbirdSettingsFields/
// certSettingsFields must resolve to an actual UpdateSystemSettingsRequest
// field. Catches a rename or removal that would otherwise leave
// requestTouchesAny silently skipping a name that no longer matches anything.
func TestSettingsDomainFieldsExist(t *testing.T) {
	typ := reflect.TypeOf(UpdateSystemSettingsRequest{})
	domains := map[string][]string{
		"smtpSettingsFields":    smtpSettingsFields,
		"netbirdSettingsFields": netbirdSettingsFields,
		"certSettingsFields":    certSettingsFields,
	}
	for listName, fields := range domains {
		for _, name := range fields {
			if _, ok := typ.FieldByName(name); !ok {
				t.Errorf("%s references %q, which is not a field of UpdateSystemSettingsRequest (renamed or removed?)", listName, name)
			}
		}
	}
}

// setNonNilField points the named field of req at a freshly allocated zero
// value of its pointee type, i.e. "this field is carried by the request"
// under the DTO's nil-means-absent convention — regardless of what value it
// points to.
func setNonNilField(t *testing.T, req *UpdateSystemSettingsRequest, fieldName string) {
	t.Helper()
	v := reflect.ValueOf(req).Elem()
	f := v.FieldByName(fieldName)
	if !f.IsValid() {
		t.Fatalf("field %q does not exist on UpdateSystemSettingsRequest", fieldName)
	}
	if f.Kind() != reflect.Pointer {
		t.Fatalf("field %q is not pointer-typed (%s) — the coverage test only knows how to set pointer fields", fieldName, f.Kind())
	}
	f.Set(reflect.New(f.Type().Elem()))
}

// TestTouchesSMTPFieldCoverage asserts, one field at a time, that setting
// ANY single smtpSettingsFields member makes touchesSMTP() true, and that an
// entirely empty request makes it false. Combined with
// TestEveryRequestFieldIsClassified this pins touchesSMTP's truth table.
func TestTouchesSMTPFieldCoverage(t *testing.T) {
	for _, name := range smtpSettingsFields {
		var req UpdateSystemSettingsRequest
		setNonNilField(t, &req, name)
		if !req.touchesSMTP() {
			t.Errorf("touchesSMTP() = false with only %s set, want true", name)
		}
	}
	if (UpdateSystemSettingsRequest{}).touchesSMTP() {
		t.Error("touchesSMTP() = true for a fully empty request, want false")
	}
}

// TestTouchesNetbirdFieldCoverage mirrors TestTouchesSMTPFieldCoverage for
// the (deliberately narrow) NetBird domain.
func TestTouchesNetbirdFieldCoverage(t *testing.T) {
	for _, name := range netbirdSettingsFields {
		var req UpdateSystemSettingsRequest
		setNonNilField(t, &req, name)
		if !req.touchesNetbird() {
			t.Errorf("touchesNetbird() = false with only %s set, want true", name)
		}
	}
	if (UpdateSystemSettingsRequest{}).touchesNetbird() {
		t.Error("touchesNetbird() = true for a fully empty request, want false")
	}
}

// TestTouchesCertFieldCoverage mirrors TestTouchesSMTPFieldCoverage for the
// certificate domain (all 30 cert_*/acme_* fields).
func TestTouchesCertFieldCoverage(t *testing.T) {
	for _, name := range certSettingsFields {
		var req UpdateSystemSettingsRequest
		setNonNilField(t, &req, name)
		if !req.touchesCert() {
			t.Errorf("touchesCert() = false with only %s set, want true", name)
		}
	}
	if (UpdateSystemSettingsRequest{}).touchesCert() {
		t.Error("touchesCert() = true for a fully empty request, want false")
	}
}

// TestEveryRequestFieldIsClassified is THE key guard (PT-4): every field of
// UpdateSystemSettingsRequest must be named in exactly one of
// smtpSettingsFields / netbirdSettingsFields / certSettingsFields /
// nonReconcileFields. A newly added request field that appears in none of
// them fails here — the same forcing function a full descriptor registry
// would have provided, without that rewrite.
//
// It also flags a field double-classified across lists: that would mean two
// domain predicates (or a domain predicate and the "no hook" allowlist)
// disagree about who owns the field, which is exactly the kind of drift this
// test exists to catch.
func TestEveryRequestFieldIsClassified(t *testing.T) {
	owner := make(map[string]string)
	record := func(listName string, fields []string) {
		for _, name := range fields {
			if prev, dup := owner[name]; dup {
				t.Errorf("field %q is classified in both %s and %s — pick one", name, prev, listName)
				continue
			}
			owner[name] = listName
		}
	}
	record("smtpSettingsFields", smtpSettingsFields)
	record("netbirdSettingsFields", netbirdSettingsFields)
	record("certSettingsFields", certSettingsFields)
	record("nonReconcileFields", nonReconcileFields)

	typ := reflect.TypeOf(UpdateSystemSettingsRequest{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if _, ok := owner[name]; !ok {
			t.Errorf("UpdateSystemSettingsRequest.%s is not classified in smtpSettingsFields, netbirdSettingsFields, "+
				"certSettingsFields, or nonReconcileFields — a newly added request field must be consciously assigned "+
				"to a domain's reconcile-trigger list or to nonReconcileFields (in "+
				"service_system_settings_touches_test.go) before this test passes", name)
		}
	}
}
