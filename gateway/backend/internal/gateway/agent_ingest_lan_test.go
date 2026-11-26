// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync"
	"testing"
)

const lanTestNetbirdIP = "192.0.2.77"

// lanCountingStore counts the targeted LAN interface writes.
type lanCountingStore struct {
	routing.Store
	mu     sync.Mutex
	writes int
}

func (c *lanCountingStore) UpdateServerLANInterfaces(ctx context.Context, id, j string) error {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return c.Store.UpdateServerLANInterfaces(ctx, id, j)
}

func lanTestServer(t *testing.T) (*Server, *lanCountingStore, *notifyCountingPortal) {
	t.Helper()
	srv := NewTestServer()
	ctx := context.Background()
	row, err := srv.Routes.AIServerByID(ctx, binariesTestServer)
	if err != nil {
		t.Fatal(err)
	}
	row.Domain, row.LANAddress, row.NetbirdIP = "", "", lanTestNetbirdIP
	if err := srv.Routes.UpdateAIServer(ctx, row); err != nil {
		t.Fatal(err)
	}
	cs := &lanCountingStore{Store: srv.Routes}
	srv.Routes = cs
	p := &notifyCountingPortal{}
	srv.Portal = p
	return srv, cs, p
}

func lanIfaces(t *testing.T, raw string) *[]agentNetInterface {
	t.Helper()
	var l []agentNetInterface
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		t.Fatal(err)
	}
	return &l
}

func TestSanitizeNetInterfacesDropsNetbirdAndJunk(t *testing.T) {
	long := strings.Repeat("n", 70)
	in := lanIfaces(t, `[
		{"id":"b","id_kind":"keyed","name":"`+long+`","ipv4":["100.64.1.2","192.0.2.77","fe80::1","not-an-ip","127.0.0.1","169.254.1.1","198.51.100.5"],"preferred":true},
		{"id":"c","id_kind":"keyed","name":"nb","ipv4":["100.64.1.2"]},
		{"id":"d","id_kind":"bogus","name":"x","ipv4":["198.51.100.6"]},
		{"id":"a","id_kind":"name","name":"eth0","ipv4":["10.0.0.4"],"preferred":true}]`)
	got := sanitizeNetInterfaces(*in, lanTestNetbirdIP)
	if len(got) != 2 {
		t.Fatalf("got %+v, want 2 interfaces", got)
	}
	if got[0].Name != "eth0" || got[1].Name != strings.Repeat("n", 64) {
		t.Fatalf("names/order = %q, %q", got[0].Name, got[1].Name)
	}
	if len(got[1].IPv4) != 1 || got[1].IPv4[0] != "198.51.100.5" {
		t.Fatalf("ipv4 = %v, want only 198.51.100.5", got[1].IPv4)
	}
	if !got[0].Preferred || got[1].Preferred {
		t.Fatalf("preferred = %v, %v, want only the first", got[0].Preferred, got[1].Preferred)
	}
}

func TestSanitizeNetInterfacesCaps(t *testing.T) {
	var in []agentNetInterface
	for i := range 40 {
		in = append(in, agentNetInterface{ID: strings.Repeat("i", i+1), IDKind: "name", Name: "n", IPv4: []string{
			"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5", "10.0.0.6", "10.0.0.7", "10.0.0.8", "10.0.0.9", "10.0.0.10",
		}})
	}
	got := sanitizeNetInterfaces(in, "")
	if len(got) != 32 || len(got[0].IPv4) != 8 {
		t.Fatalf("len=%d ips=%d, want 32 and 8", len(got), len(got[0].IPv4))
	}
}

func TestWriteBackLANInterfacesSkipsUnchanged(t *testing.T) {
	srv, cs, p := lanTestServer(t)
	ctx := context.Background()
	rep := lanIfaces(t, `[{"id":"a","id_kind":"name","name":"eth0","ipv4":["10.0.0.4"],"preferred":true}]`)
	srv.writeBackLANInterfaces(ctx, binariesTestServer, rep)
	if cs.writes != 1 || p.count() != 1 {
		t.Fatalf("first report: writes=%d notifies=%d, want 1 and 1", cs.writes, p.count())
	}
	srv.writeBackLANInterfaces(ctx, binariesTestServer, rep)
	if cs.writes != 1 || p.count() != 1 {
		t.Fatalf("resend: writes=%d notifies=%d, want still 1 and 1", cs.writes, p.count())
	}
}

func TestWriteBackLANInterfacesNotifiesOnlyOnEffectiveChange(t *testing.T) {
	srv, cs, p := lanTestServer(t)
	ctx := context.Background()
	srv.writeBackLANInterfaces(ctx, binariesTestServer, lanIfaces(t, `[{"id":"a","id_kind":"name","name":"eth0","ipv4":["10.0.0.4"],"preferred":true}]`))
	base := p.count()
	// A new interface adds an IPv4 to the head identity (every reported
	// address), so the clusters are re-derived.
	second := `{"id":"b","id_kind":"name","name":"eth1","ipv4":["198.51.100.9"]}`
	srv.writeBackLANInterfaces(ctx, binariesTestServer, lanIfaces(t, `[
		{"id":"a","id_kind":"name","name":"eth0","ipv4":["10.0.0.4"],"preferred":true},`+second+`]`))
	if cs.writes != 2 || p.count() != base+1 {
		t.Fatalf("added interface: writes=%d notifies=%d, want 2 and %d", cs.writes, p.count(), base+1)
	}
	// Renaming the non-preferred interface changes the stored list but not
	// the address, the identity or the fallback state: write, no notify.
	srv.writeBackLANInterfaces(ctx, binariesTestServer, lanIfaces(t, `[
		{"id":"a","id_kind":"name","name":"eth0","ipv4":["10.0.0.4"],"preferred":true},
		{"id":"b","id_kind":"name","name":"eth2","ipv4":["198.51.100.9"]}]`))
	if cs.writes != 3 || p.count() != base+1 {
		t.Fatalf("renamed interface: writes=%d notifies=%d, want 3 and %d", cs.writes, p.count(), base+1)
	}
	// The preferred interface's address changes the effective address.
	srv.writeBackLANInterfaces(ctx, binariesTestServer, lanIfaces(t, `[
		{"id":"a","id_kind":"name","name":"eth0","ipv4":["10.0.0.5"],"preferred":true},
		{"id":"b","id_kind":"name","name":"eth2","ipv4":["198.51.100.9"]}]`))
	if cs.writes != 4 || p.count() != base+2 {
		t.Fatalf("changed preferred ip: writes=%d notifies=%d, want 4 and %d", cs.writes, p.count(), base+2)
	}
}

func TestTelemetryWithoutInterfacesLeavesStoredList(t *testing.T) {
	srv, cs, _ := lanTestServer(t)
	srv.Portal = nil
	ctx := context.Background()
	body := `{"host":{"cpu_util_pct":1},"interfaces":[{"id":"a","id_kind":"name","name":"eth0","ipv4":["10.0.0.4"],"preferred":true}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(ctx, binariesTestServer, req, raw); err != nil {
		t.Fatal(err)
	}
	stored := func() string {
		row, err := srv.Routes.AIServerByID(ctx, binariesTestServer)
		if err != nil {
			t.Fatal(err)
		}
		return row.LANInterfaces
	}
	want := stored()
	if !strings.Contains(want, `"10.0.0.4"`) {
		t.Fatalf("stored = %q, want the report", want)
	}
	req, raw = ingestReq(t, `{"host":{"cpu_util_pct":1}}`)
	if err := srv.ingestTelemetrySample(ctx, binariesTestServer, req, raw); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got != want || cs.writes != 1 {
		t.Fatalf("after sample without interfaces: stored=%q writes=%d, want unchanged and 1", got, cs.writes)
	}
}
