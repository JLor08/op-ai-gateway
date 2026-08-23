// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"os"
	"strings"
	"testing"
)

// Since CMP-1, memoryDeps/sqliteDeps/postgresDeps share ONE body
// (buildRuntime, reached directly by memoryDeps and via sqlDeps by
// sqliteDeps/postgresDeps) instead of each inlining this wiring separately,
// so there is now exactly one portal.NewService call site rather than three.
// The relative counts below (per "driver", i.e. per portal.NewService call
// site) still hold unchanged; this test additionally pins each driver's call
// chain into that shared body so a driver that stopped reaching it would
// still be caught.
func TestAllThreeDriversWireCAUpdateBroadcast(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	drivers := strings.Count(body, "portal.NewService(portal.ServiceDeps{")
	if drivers != 1 {
		t.Fatalf("drivers=%d, want 1 (buildRuntime, shared by memory/sqlite/postgres)", drivers)
	}
	// OnCABundleChanged is a closure (not a bare func value) so it can ALSO
	// refresh the outbound gateway->app CA trust pool (Task 8, certificates-p4)
	// alongside the original agent-stream broadcast -- assert both fire once
	// per driver, plus the initial prime once portalService exists (the pool
	// starts system-roots-only, since it must be built before portalService --
	// see cmd/gateway/app_transport.go's newOutboundAppCAClient).
	if got := strings.Count(body, "OnCABundleChanged: func(fingerprint string) {"); got != drivers {
		t.Fatalf("CA update hook wired in %d/%d drivers", got, drivers)
	}
	if got := strings.Count(body, "agentStreams.NotifyCAUpdate(fingerprint)"); got != drivers {
		t.Fatalf("CA update broadcast wired in %d/%d drivers", got, drivers)
	}
	if want, got := drivers*2, strings.Count(body, "refreshOutboundAppCAPool(context.Background(), portalService, appCAPool)"); got != want {
		t.Fatalf("outbound-app CA pool refresh wired %d times, want %d (rotation hook + initial prime, per driver)", got, want)
	}

	assertAllDriversReachBuildRuntime(t)
}
