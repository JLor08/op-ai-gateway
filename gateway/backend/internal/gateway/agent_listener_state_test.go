// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"sync"
	"testing"
	"time"
)

func TestAgentListenerStateAccessors(t *testing.T) {
	s := &Server{}
	if s.AgentListenerActive() || s.AgentListenerAddr() != "" {
		t.Fatalf("zero Server should report inactive/empty")
	}
	s.SetAgentListener(true, "100.0.0.1:8081")
	if !s.AgentListenerActive() || s.AgentListenerAddr() != "100.0.0.1:8081" {
		t.Fatalf("SetAgentListener(true, addr) not reflected")
	}
	notAfter := time.Date(2027, time.January, 2, 3, 4, 5, 0, time.UTC)
	s.SetAgentListenerTLSState(AgentListenerTLSState{
		Active:      true,
		Address:     "100.0.0.1:8081",
		Fingerprint: "leaf-1",
		NotAfter:    notAfter,
	})
	if got := s.AgentListenerTLSState(); got != (AgentListenerTLSState{
		Active:      true,
		Address:     "100.0.0.1:8081",
		Fingerprint: "leaf-1",
		NotAfter:    notAfter,
	}) {
		t.Fatalf("AgentListenerTLSState = %+v, want one atomic complete snapshot", got)
	}
	s.SetAgentListener(false, "")
	if s.AgentListenerActive() || s.AgentListenerAddr() != "" {
		t.Fatalf("SetAgentListener(false, \"\") not reflected")
	}
	if got := s.AgentListenerTLSState(); got != (AgentListenerTLSState{}) {
		t.Fatalf("SetAgentListener(false, \"\") left TLS state behind: %+v", got)
	}
}

func TestAgentListenerStateRace(t *testing.T) {
	s := &Server{}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			s.SetAgentListener(true, "a:1")
			s.SetAgentListenerTLSState(AgentListenerTLSState{
				Active: true, Address: "a:1", Fingerprint: "leaf", NotAfter: time.Unix(int64(i), 0),
			})
		}(i)
		go func() {
			defer wg.Done()
			_ = s.AgentListenerActive()
			_ = s.AgentListenerAddr()
			_ = s.AgentListenerTLSState()
		}()
	}
	wg.Wait()
}
