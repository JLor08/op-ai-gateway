// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package certissue

import "testing"

func TestSplitNamesSeparatesDNSFromIP(t *testing.T) {
	dns, ips, err := SplitNames([]string{"Gateway.lan", " 10.0.0.5 ", "op-gw.intern", "fe80::1"})
	if err != nil {
		t.Fatalf("SplitNames: %v", err)
	}
	if len(dns) != 2 || dns[0] != "gateway.lan" || dns[1] != "op-gw.intern" {
		t.Fatalf("dns = %v, want [gateway.lan op-gw.intern] (lowercased, order kept)", dns)
	}
	if len(ips) != 2 || ips[0].String() != "10.0.0.5" || ips[1].String() != "fe80::1" {
		t.Fatalf("ips = %v, want [10.0.0.5 fe80::1]", ips)
	}
}

func TestSplitNamesRejectsEmptyAndDuplicates(t *testing.T) {
	if _, _, err := SplitNames(nil); err == nil {
		t.Fatal("SplitNames(nil) must fail: a certificate needs at least one name")
	}
	if _, _, err := SplitNames([]string{"a.test", "A.TEST"}); err == nil {
		t.Fatal("SplitNames must reject a duplicate name (case-insensitive)")
	}
}
