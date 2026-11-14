// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"op-ai-gateway/internal/config"
	"testing"
	"time"
)

// The /v1/realtime lifetime (OP_AI_GATEWAY_REALTIME_MAX_SESSION) is a knob whose 0 means
// something: no bound. config.Load keeps an explicit 0 distinct from the 60m default
// (TestLoadRealtimeMaxSession), and gateway.New lifts the bound for RealtimeMaxSessionOff
// (TestRealtimeMaxSessionFromDeps). This pins the hop between them: the real deps builder
// hands the explicit 0 to the Server as RealtimeMaxSessionOff, so "off" cannot be lost to
// the gateway's "zero means default" rule, and a configured bound is passed through as a
// bound.
func TestBuildRuntimeWiresTheRealtimeMaxSession(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "")
	cases := []struct {
		name    string
		cfg     time.Duration
		wantOff bool
	}{
		{"an explicit 0 is off", 0, true},
		{"the default as config loads it is a bound", 60 * time.Minute, false},
		{"a configured bound is a bound", 5 * time.Minute, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, cleanup, err := memoryDeps(config.Config{
				Addr: "127.0.0.1:8080", DBDriver: "memory",
				RealtimeMaxSession: tc.cfg,
			})
			if err != nil {
				t.Fatalf("memoryDeps returned %v", err)
			}
			t.Cleanup(func() { _ = cleanup() })
			if deps.RealtimeMaxSession != tc.cfg || deps.RealtimeMaxSessionOff != tc.wantOff {
				t.Fatalf("ServerDeps = {RealtimeMaxSession:%v RealtimeMaxSessionOff:%v}, want {%v %v}",
					deps.RealtimeMaxSession, deps.RealtimeMaxSessionOff, tc.cfg, tc.wantOff)
			}
		})
	}
}
