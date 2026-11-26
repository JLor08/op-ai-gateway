// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"reflect"
	"strings"
	"testing"
)

type fakeIf struct {
	name  string
	mac   string
	flags net.Flags
	addrs []string
}

const up = net.FlagUp

func newFake(t *testing.T, key []byte, route string, ifs ...fakeIf) *InterfaceCollector {
	t.Helper()
	c := NewInterfaceCollector(key)
	var list []net.Interface
	byName := map[string][]net.Addr{}
	for i, f := range ifs {
		ni := net.Interface{Index: i + 1, Name: f.name, Flags: f.flags}
		if f.mac != "" {
			hw, err := net.ParseMAC(f.mac)
			if err != nil {
				t.Fatal(err)
			}
			ni.HardwareAddr = hw
		}
		list = append(list, ni)
		for _, a := range f.addrs {
			ip, n, err := net.ParseCIDR(a)
			if err != nil {
				t.Fatal(err)
			}
			n.IP = ip
			byName[f.name] = append(byName[f.name], n)
		}
	}
	c.interfaces = func() ([]net.Interface, error) { return list, nil }
	c.addrs = func(i net.Interface) ([]net.Addr, error) { return byName[i.Name], nil }
	c.routeSource = func() (net.IP, error) {
		if route == "" {
			return nil, net.UnknownNetworkError("no route")
		}
		return net.ParseIP(route), nil
	}
	return c
}

func collectT(t *testing.T, c *InterfaceCollector) map[string]int {
	t.Helper()
	out, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]int{}
	for i, x := range out {
		m[x.Name] = i
	}
	return m
}

func TestCollectFiltersLoopbackDownLinkLocalAndNetbird(t *testing.T) {
	c := newFake(t, nil, "",
		fakeIf{"lo", "", up | net.FlagLoopback, []string{"127.0.0.1/8"}},
		fakeIf{"eth0", "02:00:00:00:00:01", up, []string{"10.0.0.5/24"}},
		fakeIf{"eth1", "02:00:00:00:00:02", 0, []string{"10.0.1.5/24"}},
		fakeIf{"eth2", "02:00:00:00:00:03", up, []string{"169.254.1.1/16"}},
		fakeIf{"wt0", "", up, []string{"100.64.0.5/10"}},
		fakeIf{"utun5", "", up, []string{"100.64.0.5/10"}},
		fakeIf{"wt1", "", up, []string{"192.0.2.7/24"}},
	)
	out, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Name != "eth0" {
		t.Fatalf("got %+v", out)
	}
}

func TestCollectOrdersAddressesRouteSourceFirst(t *testing.T) {
	c := newFake(t, nil, "10.0.0.9",
		fakeIf{"eth0", "02:00:00:00:00:01", up, []string{"203.0.113.4/24", "10.0.0.9/24", "10.0.0.3/24", "192.0.2.5/24"}})
	out, _ := c.Collect(context.Background())
	want := []string{"10.0.0.9", "10.0.0.3", "192.0.2.5", "203.0.113.4"}
	if !reflect.DeepEqual(out[0].IPv4, want) {
		t.Fatalf("got %v want %v", out[0].IPv4, want)
	}
	c = newFake(t, nil, "",
		fakeIf{"eth0", "02:00:00:00:00:01", up, []string{"10.0.0.9/24", "10.0.0.3/24"}})
	out, _ = c.Collect(context.Background())
	if !reflect.DeepEqual(out[0].IPv4, []string{"10.0.0.3", "10.0.0.9"}) {
		t.Fatalf("got %v", out[0].IPv4)
	}
}

func TestCollectPrefersRouteSourceThenPhysical(t *testing.T) {
	// route source wins even over a physical RFC1918 interface
	c := newFake(t, nil, "198.51.100.2",
		fakeIf{"docker0", "", up, []string{"198.51.100.2/24"}},
		fakeIf{"eth0", "02:00:00:00:00:01", up, []string{"10.0.0.5/24"}})
	out, _ := c.Collect(context.Background())
	idx := collectT(t, c)
	if !out[idx["docker0"]].Preferred || out[idx["eth0"]].Preferred {
		t.Fatalf("route source not preferred: %+v", out)
	}
	// no route: RFC1918, then non-virtual, then name
	c = newFake(t, nil, "",
		fakeIf{"docker0", "", up, []string{"10.0.0.1/24"}},
		fakeIf{"eth1", "", up, []string{"10.0.1.1/24"}},
		fakeIf{"eth0", "", up, []string{"10.0.2.1/24"}},
		fakeIf{"eth9", "", up, []string{"192.0.2.1/24"}})
	out, _ = c.Collect(context.Background())
	idx = collectT(t, c)
	n := 0
	for _, x := range out {
		if x.Preferred {
			n++
		}
	}
	if n != 1 || !out[idx["eth0"]].Preferred {
		t.Fatalf("want only eth0 preferred: %+v", out)
	}
}

func TestCollectMarksVirtualInterfaces(t *testing.T) {
	c := newFake(t, nil, "",
		fakeIf{"vEthernet (WSL)", "", up, []string{"10.0.0.1/24"}},
		fakeIf{"docker0", "", up, []string{"10.0.1.1/24"}},
		fakeIf{"Ethernet", "", up, []string{"10.0.2.1/24"}})
	out, _ := c.Collect(context.Background())
	idx := collectT(t, c)
	if !out[idx["vEthernet (WSL)"]].Virtual || !out[idx["docker0"]].Virtual || out[idx["Ethernet"]].Virtual {
		t.Fatalf("%+v", out)
	}
}

func TestCollectKeyedIDsStableAndHideMAC(t *testing.T) {
	mk := func(key []byte) string {
		c := newFake(t, key, "", fakeIf{"eth0", "02:00:00:00:00:01", up, []string{"10.0.0.5/24"}})
		out, _ := c.Collect(context.Background())
		if out[0].IDKind != "keyed" {
			t.Fatalf("kind %q", out[0].IDKind)
		}
		return out[0].ID
	}
	a, b := mk([]byte("key-one")), mk([]byte("key-one"))
	if a != b || len(a) != 32 {
		t.Fatalf("unstable or wrong length: %q %q", a, b)
	}
	if strings.Contains(a, "020000000001") {
		t.Fatalf("id leaks MAC: %s", a)
	}
	if mk([]byte("key-two")) == a {
		t.Fatal("different keys gave same id")
	}
	// no MAC: falls back to hashing the name
	c := newFake(t, []byte("key-one"), "", fakeIf{"eth0", "", up, []string{"10.0.0.5/24"}})
	out, _ := c.Collect(context.Background())
	if out[0].IDKind != "keyed" || out[0].ID == a {
		t.Fatalf("%+v", out[0])
	}
}

func TestCollectNameIDsWithoutKey(t *testing.T) {
	c := newFake(t, nil, "", fakeIf{"eth0", "02:00:00:00:00:01", up, []string{"10.0.0.5/24"}})
	out, _ := c.Collect(context.Background())
	if out[0].IDKind != "name" || out[0].ID != "eth0" {
		t.Fatalf("%+v", out[0])
	}
}

func TestCollectSortedByName(t *testing.T) {
	c := newFake(t, nil, "",
		fakeIf{"eth1", "", up, []string{"10.0.0.1/24"}},
		fakeIf{"eth0", "", up, []string{"10.0.1.1/24"}})
	out, _ := c.Collect(context.Background())
	if out[0].Name != "eth0" || out[1].Name != "eth1" {
		t.Fatalf("%+v", out)
	}
}

func TestCollectKeyedIDsDistinctForSharedMAC(t *testing.T) {
	key := []byte("key-one")
	const shared, lone = "02:00:00:00:00:01", "02:00:00:00:00:02"
	c := newFake(t, key, "",
		fakeIf{"eth0", shared, up, []string{"10.0.0.5/24"}},
		fakeIf{"eth0.10", shared, up, []string{"10.0.10.5/24"}},
		fakeIf{"eth1", lone, up, []string{"10.0.1.5/24"}})
	out, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, x := range out {
		ids[x.Name] = x.ID
	}
	if ids["eth0"] == ids["eth0.10"] {
		t.Fatalf("shared-MAC interfaces share an id: %s", ids["eth0"])
	}
	mac := func(s string) []byte { hw, _ := net.ParseMAC(s); return hw }
	h := func(parts ...[]byte) string {
		m := hmac.New(sha256.New, key)
		for _, p := range parts {
			m.Write(p)
		}
		return hex.EncodeToString(m.Sum(nil)[:16])
	}
	if want := h(mac(lone)); ids["eth1"] != want {
		t.Fatalf("lone id %s, want MAC-only %s", ids["eth1"], want)
	}
	if want := h(mac(shared), []byte{0}, []byte("eth0.10")); ids["eth0.10"] != want {
		t.Fatalf("shared id %s, want MAC+name %s", ids["eth0.10"], want)
	}
}
