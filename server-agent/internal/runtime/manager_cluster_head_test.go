// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"errors"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// fakeMember runs the stub child as a TCP-only stand-in for a remote
// rpc-server on a free loopback port and returns its address once it accepts
// connections. It is killed when the test ends.
func fakeMember(t *testing.T) string {
	t.Helper()
	port := freeTCPPort(t)
	cmd := exec.Command(stubchildPath, "-tcp-listen", "-H", "127.0.0.1", "-p", strconv.Itoa(port))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake member: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	waitUntil(t, 5*time.Second, "fake member listening", func() bool {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	})
	return addr
}

// unreachableAddr is a loopback address nothing listens on.
func unreachableAddr(t *testing.T) string {
	t.Helper()
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(freeTCPPort(t)))
}

// clusterHead is an on-demand head spec "rspec_h" serving model "m": the stub
// child in its ordinary HTTP mode, offloading to addrs, with every exec
// appended to invocations.
func clusterHead(invocations string, addrs ...string) Spec {
	eps := make([]ClusterEndpoint, len(addrs))
	for i, a := range addrs {
		eps[i] = ClusterEndpoint{Address: a, DeviceCount: 1}
	}
	return Spec{
		ID: "rspec_h", Binary: stubchildPath, UpstreamModel: "m",
		Args:                  stubArgs(0, 0, 0, invocations),
		Env:                   map[string]string{},
		StartupTimeoutSeconds: 5,
		Cluster:               &ClusterHead{Endpoints: eps, Transport: "direct", Network: "mesh"},
	}
}

// startHead brings the head up through the cluster start and releases the
// admission at once, leaving it running and idle.
func startHead(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, release, err := m.EnsureClusterHead(ctx, "m")
	if err != nil {
		t.Fatalf("EnsureClusterHead: %v", err)
	}
	release()
}

func TestColdHeadRefusesPlainEnsures(t *testing.T) {
	shrinkTimings(t)
	inv := filepath.Join(t.TempDir(), "inv")
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{clusterHead(inv, fakeMember(t))}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := m.EnsureRunning(ctx, "m"); !errors.Is(err, ErrClusterNotReady) {
		t.Fatalf("EnsureRunning err = %v, want ErrClusterNotReady", err)
	}
	if _, _, err := m.EnsureServing(ctx, "m"); !errors.Is(err, ErrClusterNotReady) {
		t.Fatalf("EnsureServing err = %v, want ErrClusterNotReady", err)
	}
	time.Sleep(150 * time.Millisecond)
	if n := execCount(t, inv); n != 0 {
		t.Fatalf("head exec'd %d times, want 0", n)
	}
}

func TestApplyNeverStartsPinnedHead(t *testing.T) {
	shrinkTimings(t)
	inv := filepath.Join(t.TempDir(), "inv")
	addr := fakeMember(t)
	pinned := clusterHead(inv, addr)
	pinned.Pinned = true
	forced := clusterHead(inv, addr)
	forced.ID, forced.UpstreamModel, forced.AdminState = "rspec_f", "f", "force_running"

	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{pinned, forced}, ETag: "e1"})
	m.Apply(Config{Specs: []Spec{pinned, forced}, ETag: "e2"})
	time.Sleep(300 * time.Millisecond) // idle ticks, wakes
	if n := execCount(t, inv); n != 0 {
		t.Fatalf("heads exec'd %d times, want 0: Apply never starts a cold cluster head", n)
	}
	for _, id := range []string{"rspec_h", "rspec_f"} {
		if s := statusFor(m, id); s == nil || s.State != StateStopped {
			t.Fatalf("status of %s = %+v, want stopped", id, s)
		}
	}
}

func TestBackoffNeverRestartsHead(t *testing.T) {
	shrinkTimings(t)
	inv := filepath.Join(t.TempDir(), "inv")
	spec := clusterHead(inv, fakeMember(t))
	spec.Args = stubArgs(0, 300*time.Millisecond, 1, inv)
	spec.AdminState = "force_running"

	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})
	startHead(t, m)
	waitUntil(t, 5*time.Second, "head crashed", func() bool {
		s := statusFor(m, "rspec_h")
		return s != nil && s.State == StateCrashed
	})
	time.Sleep(4 * backoffCap) // several crash-loop waits
	if n := execCount(t, inv); n != 1 {
		t.Fatalf("head exec'd %d times, want 1: a crashed head is never restarted on its own", n)
	}
	if s := statusFor(m, "rspec_h"); s.State != StateCrashed {
		t.Fatalf("state = %s, want crashed", s.State)
	}
}

func TestClusterStartHeaderStartsHeadThroughManager(t *testing.T) {
	shrinkTimings(t)
	inv := filepath.Join(t.TempDir(), "inv")
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{clusterHead(inv, fakeMember(t), fakeMember(t))}})

	startHead(t, m)
	if s := statusFor(m, "rspec_h"); s == nil || s.State != StateRunning {
		t.Fatalf("status = %+v, want running", s)
	}
	// A running head serves every ensure as any model does.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, release, err := m.EnsureServing(ctx, "m")
	if err != nil {
		t.Fatalf("EnsureServing on a running head: %v", err)
	}
	release()
	if n := execCount(t, inv); n != 1 {
		t.Fatalf("head exec'd %d times, want 1", n)
	}
}

func TestProbeRetriesUntilMemberListens(t *testing.T) {
	addr := unreachableAddr(t)
	got := make(chan []byte, 1)
	go func() {
		time.Sleep(time.Second)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			got <- nil
			return
		}
		defer ln.Close()
		c, err := ln.Accept()
		if err != nil {
			got <- nil
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		b, _ := io.ReadAll(c)
		got <- b
	}()

	start := time.Now()
	if err := probeEndpoints(context.Background(), []ClusterEndpoint{{Address: addr, DeviceCount: 1}}, 5*time.Second); err != nil {
		t.Fatalf("probe err = %v, want nil once the member listens", err)
	}
	if d := time.Since(start); d < 900*time.Millisecond {
		t.Fatalf("probe returned after %s, before the member listened", d)
	}
	select {
	case b := <-got:
		if len(b) != 0 {
			t.Fatalf("probe sent %d bytes, want none", len(b))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fake member never saw the probe")
	}
}

func TestProbeGivesUpWithUnreachable(t *testing.T) {
	addr := unreachableAddr(t)
	start := time.Now()
	err := probeEndpoints(context.Background(), []ClusterEndpoint{{Address: fakeMember(t)}, {Address: addr}}, 1200*time.Millisecond)
	if !errors.Is(err, ErrClusterMemberUnreachable) {
		t.Fatalf("probe err = %v, want ErrClusterMemberUnreachable", err)
	}
	if d := time.Since(start); d < time.Second || d > 4*time.Second {
		t.Fatalf("probe gave up after %s, want about its 1.2s bound", d)
	}

	// Through the manager: the head is never exec'd.
	shrinkTimings(t)
	inv := filepath.Join(t.TempDir(), "inv")
	spec := clusterHead(inv, addr)
	spec.StartupTimeoutSeconds = 1
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := m.EnsureClusterHead(ctx, "m"); !errors.Is(err, ErrClusterMemberUnreachable) {
		t.Fatalf("EnsureClusterHead err = %v, want ErrClusterMemberUnreachable", err)
	}
	if n := execCount(t, inv); n != 0 {
		t.Fatalf("head exec'd %d times, want 0 after the probe gave up", n)
	}
}

func TestPinnedHeadStopDrains(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pinned bool
		admin  string
	}{
		{name: "pinned", pinned: true},
		{name: "force_running", admin: "force_running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shrinkTimings(t)
			inv := filepath.Join(t.TempDir(), "inv")
			spec := clusterHead(inv, fakeMember(t))
			spec.Pinned, spec.AdminState = tc.pinned, tc.admin
			m := newTestManager(t, allowlistPolicy())
			m.Apply(Config{Specs: []Spec{spec}})
			startHead(t, m)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			state, err := m.Stop(ctx, "m")
			if err != nil || state != StopStopping {
				t.Fatalf("Stop = %q, %v; want stopping: a kept-up head is drained, not protected", state, err)
			}
			waitUntil(t, 5*time.Second, "head stopped", func() bool {
				s := statusFor(m, "rspec_h")
				return s != nil && s.State == StateStopped
			})
			time.Sleep(300 * time.Millisecond)
			if n := execCount(t, inv); n != 1 {
				t.Fatalf("head exec'd %d times, want 1: a stopped head is not restarted", n)
			}
		})
	}
}

func TestPinnedHeadIsNotIdleUnloaded(t *testing.T) {
	shrinkTimings(t)
	inv := filepath.Join(t.TempDir(), "inv")
	spec := clusterHead(inv, fakeMember(t))
	spec.AdminState = "force_running"
	spec.IdleTimeoutSeconds = 1
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})
	startHead(t, m)
	time.Sleep(1500 * time.Millisecond)
	if s := statusFor(m, "rspec_h"); s == nil || s.State != StateRunning {
		t.Fatalf("status = %+v, want running: the gateway keeps a force_running head up", s)
	}
}

func TestConditionalStopRefusesBusyHead(t *testing.T) {
	shrinkTimings(t)
	inv := filepath.Join(t.TempDir(), "inv")
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{clusterHead(inv, fakeMember(t))}})
	startHead(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, release, err := m.EnsureServing(ctx, "m") // one request in flight
	if err != nil {
		t.Fatalf("EnsureServing: %v", err)
	}
	if _, err := m.StopIfEvictable(ctx, "m"); !errors.Is(err, ErrInstanceBusy) {
		t.Fatalf("StopIfEvictable err = %v, want ErrInstanceBusy with a request in flight", err)
	}
	release()
	if s := statusFor(m, "rspec_h"); s == nil || s.State != StateRunning {
		t.Fatalf("status = %+v, want still running", s)
	}
}

func TestConditionalStopRefusesForceRunningHead(t *testing.T) {
	shrinkTimings(t)
	inv := filepath.Join(t.TempDir(), "inv")
	spec := clusterHead(inv, fakeMember(t))
	spec.AdminState = "force_running"
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})
	startHead(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.StopIfEvictable(ctx, "m"); !errors.Is(err, ErrInstanceBusy) {
		t.Fatalf("StopIfEvictable err = %v, want ErrInstanceBusy: a force_running head counts as pinned", err)
	}
}

func TestConditionalStopStopsIdleHead(t *testing.T) {
	shrinkTimings(t)
	inv := filepath.Join(t.TempDir(), "inv")
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{clusterHead(inv, fakeMember(t))}})
	startHead(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state, err := m.StopIfEvictable(ctx, "m")
	if err != nil || state != StopStopping {
		t.Fatalf("StopIfEvictable = %q, %v; want stopping", state, err)
	}
	waitUntil(t, 5*time.Second, "head stopped", func() bool {
		s := statusFor(m, "rspec_h")
		return s != nil && s.State == StateStopped
	})
	if state, err := m.StopIfEvictable(ctx, "m"); err != nil || state != StopStopped {
		t.Fatalf("second StopIfEvictable = %q, %v; want stopped", state, err)
	}
}

// A head the eviction grace still protects is refused, like any process the
// agent's own eviction would not pick.
func TestConditionalStopHonoursEvictionGrace(t *testing.T) {
	shrinkTimings(t)
	inv := filepath.Join(t.TempDir(), "inv")
	m := NewManager(ManagerOptions{Policy: allowlistPolicy(), Getenv: func(string) string { return "" }, EvictionGrace: time.Minute})
	t.Cleanup(m.Close)
	m.Apply(Config{Specs: []Spec{clusterHead(inv, fakeMember(t))}})
	startHead(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.StopIfEvictable(ctx, "m"); !errors.Is(err, ErrInstanceBusy) {
		t.Fatalf("StopIfEvictable err = %v, want ErrInstanceBusy inside the eviction grace", err)
	}
}

func TestStatusReportsHeadLaunchedEndpoints(t *testing.T) {
	shrinkTimings(t)
	inv := filepath.Join(t.TempDir(), "inv")
	addr := fakeMember(t)
	spec := clusterHead(inv, addr)
	spec.AdminState = "force_running"

	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})
	if s := statusFor(m, "rspec_h"); s == nil || s.Cluster != nil {
		t.Fatalf("a cold head reports no launch facts, got %+v", s)
	}
	startHead(t, m)
	s := statusFor(m, "rspec_h")
	if s == nil || s.Cluster == nil || len(s.Cluster.Launched) != 1 || s.Cluster.Launched[0] != addr {
		t.Fatalf("status cluster = %+v, want launched [%s]", s, addr)
	}
}
