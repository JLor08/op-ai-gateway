// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"testing"
)

// TestPutRuntimeSpecRefusesWhileABenchmarkHoldsTheServer closes the door the
// VRAM run's honesty gates could only detect being opened.
//
// A VRAM run's whole promise is that the target loads ALONE: it force-stops
// every launch spec on the server, proves the drain against its own evidence,
// and then measures a delta across two windows. Nothing stopped an operator
// from clicking "Force start" on a drained sibling in the middle of that --
// this endpoint is a full-document replace and admin_state is one of its
// fields, so one click starts a sibling whose allocation lands inside the
// measurement window. The run can now DETECT that (it re-checks the isolation
// at the end and reports `isolation_lost`), but detecting it costs the
// operator the run; refusing the write costs them one message and keeps the
// measurement.
//
// The reservation is the right gate because it is already the fact that
// excludes the server from routing while a run is in flight, so this adds no
// new state -- and it is deliberately checked in PutRuntimeSpec, the
// PRINCIPAL-carrying path, never in the shared putRuntimeSpec body: the run's
// own drain and restore go through SetBenchmarkRuntimeSpecAdminState, and
// gating that would make the run refuse its own writes.
func TestPutRuntimeSpecRefusesWhileABenchmarkHoldsTheServer(t *testing.T) {
	ctx := context.Background()
	svc, calls, serverID, spec := benchmarkSpecFixture(t)
	notifiedBefore := len(calls())
	svc.SetBenchmarkReservationHook(func(id string) bool { return id == serverID })

	body := putRequestFromDTO(spec)
	body.AdminState = "force_running"
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), spec.MappingID, body); !errors.Is(err, ErrRuntimeSpecServerBenchmarking) {
		t.Fatalf("PutRuntimeSpec err = %v, want ErrRuntimeSpecServerBenchmarking", err)
	}
	// Refused means NOTHING written and nobody notified -- a pushed document
	// carrying the override would start the sibling whether the HTTP response
	// was an error or not.
	stored, err := svc.GetRuntimeSpec(ctx, ownerToken(), spec.MappingID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if stored.AdminState != "" {
		t.Fatalf("admin_state = %q after a refused write, want it untouched", stored.AdminState)
	}
	if got := len(calls()); got != notifiedBefore {
		t.Fatalf("a refused write notified the agent: %d calls, want %d", got, notifiedBefore)
	}

	// The RUN's own writer must still work while the run holds the server --
	// it is the caller that took the reservation in the first place.
	if _, err := svc.SetBenchmarkRuntimeSpecAdminState(ctx, spec.ID, "", "force_stopped"); err != nil {
		t.Fatalf("SetBenchmarkRuntimeSpecAdminState while reserved: %v", err)
	}
}

// TestPutRuntimeSpecIsUngatedOnAnotherServersRun: the reservation is per
// server, so a run on one host must not freeze the launch specs of every
// other host in the fleet.
func TestPutRuntimeSpecIsUngatedOnAnotherServersRun(t *testing.T) {
	ctx := context.Background()
	svc, _, _, spec := benchmarkSpecFixture(t)
	svc.SetBenchmarkReservationHook(func(id string) bool { return id == "some-other-server" })

	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), spec.MappingID, putRequestFromDTO(spec)); err != nil {
		t.Fatalf("PutRuntimeSpec with another server reserved: %v", err)
	}
}

// TestPutRuntimeSpecIsUngatedWithNoHook: the hook is wired after construction
// (cmd/gateway builds the benchmark registry with the gateway Server, after
// the portal Service), so a nil hook must mean "no run is holding anything"
// rather than refusing every write.
func TestPutRuntimeSpecIsUngatedWithNoHook(t *testing.T) {
	ctx := context.Background()
	svc, _, _, spec := benchmarkSpecFixture(t)

	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), spec.MappingID, putRequestFromDTO(spec)); err != nil {
		t.Fatalf("PutRuntimeSpec with no reservation hook: %v", err)
	}
}
