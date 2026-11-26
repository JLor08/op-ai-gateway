// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"math"
	"testing"
)

func TestParseProcStatBusyUsec(t *testing.T) {
	stat := "cpu  100 5 50 1000 20 3 2 10 7 1\ncpu0 999 999 999 999 999 999 999 999 999 999\nintr 1\n"
	got, ok := ParseProcStatBusyUsec(stat)
	if !ok || got != 1_700_000 {
		t.Fatalf("got (%d,%v), want (1700000,true)", got, ok)
	}
	if _, ok := ParseProcStatBusyUsec("cpu0 1 2 3 4 5 6 7 8\nintr 1\n"); ok {
		t.Fatal("cpu0-only input must not match the aggregate line")
	}
	if _, ok := ParseProcStatBusyUsec("intr 1\n"); ok {
		t.Fatal("no cpu line must be !ok")
	}
	if _, ok := ParseProcStatBusyUsec("cpu  1 2 3 4 5 6 7\n"); ok {
		t.Fatal("missing steal field must be !ok")
	}
	if _, ok := ParseProcStatBusyUsec("cpu  1 2 x 4 5 6 7 8\n"); ok {
		t.Fatal("non-numeric field must be !ok")
	}
}

func TestParseCgroupUsageUsec(t *testing.T) {
	got, ok := ParseCgroupUsageUsec("usage_usec 123456\nuser_usec 100\n")
	if !ok || got != 123456 {
		t.Fatalf("got (%d,%v)", got, ok)
	}
	if _, ok := ParseCgroupUsageUsec("user_usec 100\n"); ok {
		t.Fatal("missing key must be !ok")
	}
}

func TestSharesPlain(t *testing.T) {
	s := Shares(1_000_000, map[string]uint64{"a": 250_000, "b": 500_000})
	if s["a"] != 0.25 || s["b"] != 0.5 {
		t.Fatalf("got %v", s)
	}
}

func TestSharesNormalisedWhenSumExceedsHost(t *testing.T) {
	s := Shares(1_000_000, map[string]uint64{"a": 800_000, "b": 600_000})
	if math.Abs(s["a"]-4.0/7) > 1e-9 || math.Abs(s["b"]-3.0/7) > 1e-9 {
		t.Fatalf("got %v", s)
	}
	if s["a"]+s["b"] > 1+1e-12 {
		t.Fatalf("sum %v exceeds 1", s["a"]+s["b"])
	}
}

func TestSharesClampedSingleVM(t *testing.T) {
	s := Shares(100, map[string]uint64{"a": 500})
	if s["a"] != 1 {
		t.Fatalf("got %v", s)
	}
}

func TestSharesZeroHostDelta(t *testing.T) {
	s := Shares(0, map[string]uint64{"a": 10})
	if v, ok := s["a"]; !ok || v != 0 || math.IsNaN(v) {
		t.Fatalf("got %v", s)
	}
}

func TestSharesEmpty(t *testing.T) {
	if s := Shares(10, nil); s == nil || len(s) != 0 {
		t.Fatalf("nil input: got %#v", s)
	}
	if s := Shares(10, map[string]uint64{}); s == nil || len(s) != 0 {
		t.Fatalf("empty input: got %#v", s)
	}
}

func TestAttributeUJ(t *testing.T) {
	if got := AttributeUJ(2.5, 0.4); got != 1_000_000 {
		t.Fatalf("got %d", got)
	}
	if got := AttributeUJ(math.NaN(), 0.4); got != 0 {
		t.Fatalf("NaN: got %d", got)
	}
	if got := AttributeUJ(-1, 0.4); got != 0 {
		t.Fatalf("negative: got %d", got)
	}
	if got := AttributeUJ(math.Inf(1), 0.4); got != 0 {
		t.Fatalf("Inf: got %d", got)
	}
}
