// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

// Currency display/entry units (shared enum). Euro-cent is the default
// everywhere. Internal storage of any cost/price value stays canonical EUR;
// these units govern only how a value is DISPLAYED or ENTERED.
const (
	UnitEUR     = "eur"
	UnitEURCent = "eur_cent"
	UnitUSD     = "usd"
	UnitUSDCent = "usd_cent"
)

// NormalizePriceUnit is lenient: an unknown/empty unit falls back to
// eur_cent, mirroring the other lenient enum readers in
// service_system_settings.go (e.g. NetbirdPolicyScope).
func NormalizePriceUnit(u string) string {
	switch u {
	case UnitEUR, UnitEURCent, UnitUSD, UnitUSDCent:
		return u
	default:
		return UnitEURCent
	}
}
