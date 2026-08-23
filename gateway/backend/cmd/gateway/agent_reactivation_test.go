// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"op-ai-gateway/internal/netbird"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// fakeReactStore satisfies reactivationStore.
type fakeReactStore struct {
	servers map[string]routing.AIServer
	err     error
}

func (f *fakeReactStore) AIServerByID(_ context.Context, id string) (routing.AIServer, error) {
	if f.err != nil {
		return routing.AIServer{}, f.err
	}
	return f.servers[id], nil
}

func (f *fakeReactStore) UpdateServerNetbirdState(context.Context, string, string, string, bool) error {
	return nil
}

// fakeNetbirdCfg satisfies netbirdSettings. err, when set, is returned ALONGSIDE
// whatever ok the case wants (not forced false) so a test can isolate the
// coordinator's own `cerr == nil` defensive guard in handleAgentReactivation from
// the `ok` check — the real portal.Service.NetbirdConfig always pairs an error
// with ok=false, but the netbirdSettings interface makes no such guarantee, and
// the coordinator's guard exists precisely to not trust that pairing blindly.
type fakeNetbirdCfg struct {
	ok  bool
	err error
}

func (f *fakeNetbirdCfg) NetbirdConfig(context.Context) (portal.NetbirdConfig, bool, error) {
	if f.err != nil {
		return portal.NetbirdConfig{}, f.ok, f.err
	}
	if !f.ok {
		return portal.NetbirdConfig{}, false, nil
	}
	return portal.NetbirdConfig{URL: "http://nb", Token: "t"}, true, nil
}

// fakeServerReconciler satisfies serverReconciler. (Named distinctly from
// netbird_sync_test.go's fakeReconciler, which fakes the unrelated
// netbirdReconciler interface.)
type fakeServerReconciler struct{ called []string }

func (f *fakeServerReconciler) ReconcileServerNetbird(_ context.Context, id string) {
	f.called = append(f.called, id)
}

func drain(ch chan string) []string {
	var got []string
	for {
		select {
		case v := <-ch:
			got = append(got, v)
		default:
			return got
		}
	}
}

func TestHandleAgentReactivation(t *testing.T) {
	ctx := context.Background()
	peerSrv := routing.AIServer{ID: "p", NetbirdEnabled: true, NetbirdPeerID: "peer-1"}
	noPeerSrv := routing.AIServer{ID: "n", NetbirdEnabled: false}

	cases := []struct {
		name          string
		server        routing.AIServer
		nbOK          bool
		nbErr         error
		syncConnected bool
		wantReconcile bool
		wantTrigger   bool
	}{
		{"no peer -> direct health", noPeerSrv, true, nil, false, false, true},
		{"module off -> direct health", peerSrv, false, nil, false, false, true},
		{"peer online -> reconcile + health", peerSrv, true, nil, true, true, true},
		{"peer offline -> reconcile, no health", peerSrv, true, nil, false, true, false},
		{"netbird config error -> direct health, no reconcile", peerSrv, true, context.DeadlineExceeded, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeReactStore{servers: map[string]routing.AIServer{tc.server.ID: tc.server}}
			rec := &fakeServerReconciler{}
			trigger := make(chan string, 4)
			syncOne := func(context.Context, netbird.Config, time.Duration, netbirdStateStore, routing.AIServer, onlineEventFunc) (bool, bool) {
				return tc.syncConnected, true
			}
			handleAgentReactivation(ctx, tc.server.ID, reactivationDeps{
				store: store, settings: &fakeNetbirdCfg{ok: tc.nbOK, err: tc.nbErr}, reconciler: rec,
				syncOne: syncOne, timeout: time.Second,
			}, trigger)

			gotTrigger := drain(trigger)
			if tc.wantTrigger != (len(gotTrigger) == 1 && gotTrigger[0] == tc.server.ID) {
				t.Fatalf("trigger: want fired=%v, got %v", tc.wantTrigger, gotTrigger)
			}
			if tc.wantReconcile != (len(rec.called) == 1) {
				t.Fatalf("reconcile: want=%v, got %v", tc.wantReconcile, rec.called)
			}
		})
	}
}

func TestHandleAgentReactivationFullChannelDoesNotBlock(t *testing.T) {
	store := &fakeReactStore{servers: map[string]routing.AIServer{"n": {ID: "n"}}}
	trigger := make(chan string, 1)
	trigger <- "prefill" // full
	done := make(chan struct{})
	go func() {
		handleAgentReactivation(context.Background(), "n", reactivationDeps{
			store: store, settings: &fakeNetbirdCfg{}, reconciler: &fakeServerReconciler{},
			syncOne: func(context.Context, netbird.Config, time.Duration, netbirdStateStore, routing.AIServer, onlineEventFunc) (bool, bool) {
				return false, false
			},
			timeout: time.Second,
		}, trigger)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handleAgentReactivation blocked on a full trigger channel")
	}
}

func TestHandleAgentReactivationLookupErrorIsNoop(t *testing.T) {
	store := &fakeReactStore{err: context.DeadlineExceeded}
	trigger := make(chan string, 1)
	handleAgentReactivation(context.Background(), "x", reactivationDeps{
		store: store, settings: &fakeNetbirdCfg{ok: true}, reconciler: &fakeServerReconciler{},
		syncOne: func(context.Context, netbird.Config, time.Duration, netbirdStateStore, routing.AIServer, onlineEventFunc) (bool, bool) {
			return true, true
		},
		timeout: time.Second,
	}, trigger)
	if len(drain(trigger)) != 0 {
		t.Fatal("a lookup error must fire nothing")
	}
}
