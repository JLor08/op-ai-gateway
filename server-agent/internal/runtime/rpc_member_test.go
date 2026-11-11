// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func memberSpec(id, head string, args ...string) Spec {
	return Spec{
		ID:                    id,
		Kind:                  SpecKindRPCServer,
		Binary:                stubchildPath,
		Args:                  args,
		Env:                   map[string]string{},
		StartupTimeoutSeconds: 5,
		RPC: &RPCMember{
			HeadSpecID: head,
			Transport:  "direct",
			BindIP:     "100.64.0.5",
			Port:       47001,
		},
	}
}

func TestMemberArgsDirectGPU(t *testing.T) {
	spec := memberSpec("rpcm_a", "rspec_h", "-t", "8")
	spec.GPUs = []SpecGPU{{Index: 3}, {Index: 2}}
	got, err := memberArgs(spec, 0, "CUDA")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-t", "8", "-d", "CUDA3,CUDA2", "-H", "100.64.0.5", "-p", "47001"}
	if !slices.Equal(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestMemberArgsCPUMember(t *testing.T) {
	got, err := memberArgs(memberSpec("rpcm_a", "rspec_h"), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-d", "CPU", "-H", "100.64.0.5", "-p", "47001"}
	if !slices.Equal(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestMemberArgsCacheSwitch(t *testing.T) {
	spec := memberSpec("rpcm_a", "rspec_h")
	spec.RPC.CacheEnabled = true
	got, err := memberArgs(spec, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if got[len(got)-1] != "-c" {
		t.Fatalf("args = %v, want a trailing -c", got)
	}
}

func TestMemberArgsRelayBindsLoopback(t *testing.T) {
	spec := memberSpec("rpcm_a", "rspec_h")
	spec.RPC.Transport = "relay"
	got, err := memberArgs(spec, 51234, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-d", "CPU", "-H", "127.0.0.1", "-p", "51234"}
	if !slices.Equal(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestMemberArgsGPUWithoutBackendPrefixFails(t *testing.T) {
	spec := memberSpec("rpcm_a", "rspec_h")
	spec.GPUs = []SpecGPU{{Index: 0}}
	if _, err := memberArgs(spec, 0, ""); err == nil {
		t.Fatal("a GPU member with no backend prefix must be refused")
	}
}

func TestMemberRefusesReservedFlags(t *testing.T) {
	for _, flag := range []string{"-H", "--host", "-p", "--port", "-d", "--device", "-c", "--cache", "--port=1", "--device=CPU", "-p=2"} {
		t.Run(flag, func(t *testing.T) {
			spec := memberSpec("rpcm_a", "rspec_h", "-t", "8", flag, "x")
			if _, err := memberArgs(spec, 0, ""); !errors.Is(err, errMemberFlagReserved) {
				t.Fatalf("memberArgs err = %v, want errMemberFlagReserved", err)
			}
			if err := (LocalPolicy{AllowedBinaries: []string{stubchildPath}}).Permit(spec); !errors.Is(err, errMemberFlagReserved) {
				t.Fatalf("Permit err = %v, want errMemberFlagReserved", err)
			}
		})
	}
}

func TestMemberReservedFlagYieldsNotPermitted(t *testing.T) {
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	spec := memberSpec("rpcm_a", "rspec_h", "-p", "9")
	spec.AdminState = "force_running"
	m.Apply(Config{Specs: []Spec{spec}})
	waitUntil(t, 5*time.Second, "member not_permitted", func() bool {
		s := statusFor(m, "rpcm_a")
		return s != nil && s.State == StateNotPermitted
	})
}

func TestMemberEnvModeRefused(t *testing.T) {
	policy := LocalPolicy{AllowedBinaries: []string{stubchildPath}}
	spec := memberSpec("rpcm_a", "rspec_h")
	spec.GPUs = []SpecGPU{{Index: 0}}
	spec.SetVisibleDevices = true
	if err := policy.Permit(spec); err == nil {
		t.Fatal("env-mode set_visible_devices on a member must be refused")
	}
	spec.VisibleDevicesMode = VisibleDevicesModeArgs
	if err := policy.Permit(spec); err != nil {
		t.Fatalf("args-mode is harmless on a member: %v", err)
	}
}

func TestMemberEnv(t *testing.T) {
	spec := memberSpec("rpcm_a", "rspec_h")
	env := memberEnv(spec, "/var/cache")
	if !slices.Contains(env, "GGML_RPC_NO_RDMA=1") {
		t.Fatalf("env = %v, want GGML_RPC_NO_RDMA=1", env)
	}
	for _, e := range env {
		if strings.HasPrefix(e, "LLAMA_CACHE=") {
			t.Fatalf("env = %v, no cache requested", env)
		}
	}
}

func TestMemberCacheDirPerHead(t *testing.T) {
	root := filepath.Join("/var", "cache")
	for _, head := range []string{"rspec_h1", "rspec_h2"} {
		spec := memberSpec("rpcm_a", head)
		spec.RPC.CacheEnabled = true
		want := "LLAMA_CACHE=" + filepath.Join(root, "rpc", head)
		if env := memberEnv(spec, root); !slices.Contains(env, want) {
			t.Fatalf("env = %v, want %s", env, want)
		}
	}
	for _, bad := range []string{"", "..", "a/b", `a\b`} {
		if _, err := memberCacheDir(root, bad); err == nil {
			t.Errorf("head id %q must be refused", bad)
		}
	}
}

func TestHeadGetsNoComm(t *testing.T) {
	spec := clusterHeadSpec()
	_, env, err := ExpandPlaceholders(spec, 1, GPUVendorNone, nil, func(string) string { return "" }, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "GGML_RPC_NO_COMM=1") {
		t.Fatalf("env = %v, want GGML_RPC_NO_COMM=1 on a head", env)
	}
	_, env, _ = ExpandPlaceholders(Spec{}, 1, GPUVendorNone, nil, func(string) string { return "" }, "")
	if slices.Contains(env, "GGML_RPC_NO_COMM=1") {
		t.Fatalf("env = %v, a plain model spec must not get GGML_RPC_NO_COMM", env)
	}
}

func TestProbeTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if !probeTCP(addr, time.Second) {
		t.Fatal("probeTCP must connect to a listener")
	}
	ln.Close()
	if probeTCP(addr, 200*time.Millisecond) {
		t.Fatal("probeTCP must fail once the listener is gone")
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	p, err := grabEphemeralPort()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRPCServerReadyOnTCPConnect(t *testing.T) {
	shrinkTimings(t)
	port := freeTCPPort(t)
	argvLog := filepath.Join(t.TempDir(), "argv")
	spec := memberSpec("rpcm_a", "rspec_h", "-tcp-listen", "-argv-log", argvLog, "-t", "8")
	spec.RPC.BindIP = "127.0.0.1"
	spec.RPC.Port = port
	spec.AdminState = "force_running"

	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})
	waitUntil(t, 5*time.Second, "member running", func() bool {
		s := statusFor(m, "rpcm_a")
		return s != nil && s.State == StateRunning
	})
	if s := statusFor(m, "rpcm_a"); s.Kind != SpecKindRPCServer || s.Port != port {
		t.Fatalf("status = %+v, want kind rpc_server on port %d", s, port)
	}
	got, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if want := "-t 8 -d CPU -H 127.0.0.1 -p " + strconv.Itoa(port); !strings.HasSuffix(string(got), want) {
		t.Fatalf("argv = %q, want suffix %q", got, want)
	}
}

func TestRPCServerRelayReadyOnChildPort(t *testing.T) {
	shrinkTimings(t)
	spec := memberSpec("rpcm_a", "rspec_h", "-tcp-listen")
	spec.RPC.Transport = "relay"
	spec.RPC.BindIP = "127.0.0.1"  // must be an address of this host
	spec.RPC.Port = freeTCPPort(t) // the relay's port, not the child's
	spec.AdminState = "force_running"

	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})
	waitUntil(t, 5*time.Second, "relay member running", func() bool {
		s := statusFor(m, "rpcm_a")
		return s != nil && s.State == StateRunning
	})
	s := statusFor(m, "rpcm_a")
	if s.Port == 0 || s.Port == spec.RPC.Port {
		t.Fatalf("status port = %d, want an ephemeral child port distinct from the relay port %d", s.Port, spec.RPC.Port)
	}
}

func TestRPCServerNotInUpstreamIndex(t *testing.T) {
	shrinkTimings(t)
	spec := memberSpec("rpcm_a", "rspec_h", "-tcp-listen")
	spec.RPC.BindIP = "127.0.0.1"
	spec.RPC.Port = freeTCPPort(t)
	spec.AdminState = "force_running"

	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})
	waitUntil(t, 5*time.Second, "member running", func() bool {
		s := statusFor(m, "rpcm_a")
		return s != nil && s.State == StateRunning
	})
	if got := m.LoadedModels(); len(got) != 0 {
		t.Fatalf("LoadedModels = %v, want none: a member serves no model", got)
	}
	if base, _, ok := m.ResolveAddress(""); ok {
		t.Fatalf("ResolveAddress(\"\") resolved to %q, a member must never be indexed under an empty upstream", base)
	}
}

func TestMemberIdleDefault(t *testing.T) {
	if got := memberIdleTimeout(memberSpec("a", "h")); got != 60*time.Second {
		t.Fatalf("default idle = %v, want 60s", got)
	}
	spec := memberSpec("a", "h")
	spec.IdleTimeoutSeconds = 5
	if got := memberIdleTimeout(spec); got != 5*time.Second {
		t.Fatalf("idle = %v, want 5s", got)
	}
}

func cacheTestManager(t *testing.T, root string) *Manager {
	t.Helper()
	m := NewManager(ManagerOptions{Policy: allowlistPolicy(), Getenv: func(string) string { return "" }, ConfigFilesDir: root})
	t.Cleanup(m.Close)
	return m
}

func cachingMember(t *testing.T, id, head string) Spec {
	spec := memberSpec(id, head, "-tcp-listen")
	spec.RPC.BindIP = "127.0.0.1"
	spec.RPC.Port = freeTCPPort(t)
	spec.RPC.CacheEnabled = true
	spec.AdminState = "force_running"
	return spec
}

func TestMemberCachePurgedOnHeadChange(t *testing.T) {
	shrinkTimings(t)
	root := t.TempDir()
	origFree := freeDiskMB
	freeDiskMB = func(string) (int, error) { return 1 << 20, nil }
	t.Cleanup(func() { freeDiskMB = origFree })

	m := cacheTestManager(t, root)
	spec := cachingMember(t, "rpcm_a", "rspec_h1")
	m.Apply(Config{Specs: []Spec{spec}, ETag: "1"})
	dir1 := filepath.Join(root, "rpc", "rspec_h1")
	waitUntil(t, 5*time.Second, "member running with its cache dir", func() bool {
		s := statusFor(m, "rpcm_a")
		_, err := os.Stat(dir1)
		return s != nil && s.State == StateRunning && err == nil
	})
	if err := os.WriteFile(filepath.Join(dir1, "blob"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Same head again: nothing is purged.
	m.Apply(Config{Specs: []Spec{spec}, ETag: "2"})
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir1, "blob")); err != nil {
		t.Fatalf("cache purged although the head did not change: %v", err)
	}

	// A different head drops the old head's cache.
	moved := spec
	rpc := *spec.RPC
	rpc.HeadSpecID = "rspec_h2"
	rpc.Port = freeTCPPort(t)
	moved.RPC = &rpc
	m.Apply(Config{Specs: []Spec{moved}, ETag: "3"})
	waitUntil(t, 5*time.Second, "old head cache purged", func() bool {
		_, err := os.Stat(dir1)
		return os.IsNotExist(err)
	})

	// Leaving the document keeps the cache: a member is withheld for reasons
	// that pass (a gateway restart that has not heard from every cluster host
	// yet), and its head's cache is what makes the next start fast.
	dir2 := filepath.Join(root, "rpc", "rspec_h2")
	waitUntil(t, 5*time.Second, "member running on the new head", func() bool {
		_, err := os.Stat(dir2)
		return err == nil
	})
	m.Apply(Config{ETag: "4"})
	waitUntil(t, 5*time.Second, "departed member stopped", func() bool {
		return statusFor(m, "rpcm_a") == nil
	})
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(dir2); err != nil {
		t.Fatalf("cache of a member that left the document was purged: %v", err)
	}
}

// TestMemberCacheSweepRemovesStaleOrphans: the sweep of the cache root removes
// what belongs to no member of the current document and has not changed for
// memberCacheOrphanAge -- caches orphaned while the agent was down included --
// and keeps a recent orphan, every cache a listed member serves however old it
// is, and anything that is not a directory. A purge tombstone left behind goes
// once its purge is older than memberCacheSweepEvery.
func TestMemberCacheSweepRemovesStaleOrphans(t *testing.T) {
	shrinkTimings(t)
	root := t.TempDir()
	old := time.Now().Add(-memberCacheOrphanAge - time.Hour)
	mk := func(name string, mtime time.Time) string {
		dir := filepath.Join(root, "rpc", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		blob := filepath.Join(dir, "blob")
		if err := os.WriteFile(blob, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{blob, dir} {
			if err := os.Chtimes(p, mtime, mtime); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	staleOrphan := mk("rspec_gone", old)
	freshOrphan := mk("rspec_recent", time.Now().Add(-time.Hour))
	listedOld := mk("rspec_listed", old)
	// An old directory with one recent file in it is in use, not stale.
	touched := mk("rspec_touched", old)
	if err := os.WriteFile(filepath.Join(touched, "new"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(touched, old, old); err != nil {
		t.Fatal(err)
	}
	oldTomb := mk(".purge-rspec_x-"+strconv.FormatInt(time.Now().Add(-2*memberCacheSweepEvery).UnixNano(), 10), time.Now())
	newTomb := mk(".purge-rspec_y-"+strconv.FormatInt(time.Now().UnixNano(), 10), old)
	file := filepath.Join(root, "rpc", "note.txt")
	if err := os.WriteFile(file, []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}

	m := cacheTestManager(t, root)
	listed := memberSpec("rpcm_l", "rspec_listed")
	listed.AdminState = "force_stopped"
	m.Apply(Config{Specs: []Spec{listed}, ETag: "1"})

	waitUntil(t, 5*time.Second, "stale orphan and old tombstone swept", func() bool {
		_, e1 := os.Stat(staleOrphan)
		_, e2 := os.Stat(oldTomb)
		return os.IsNotExist(e1) && os.IsNotExist(e2)
	})
	for _, keep := range []string{freshOrphan, listedOld, touched, newTomb, file} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s was swept: %v", filepath.Base(keep), err)
		}
	}
}

// TestMemberCacheSweepThrottled: the sweep runs on the first document and then
// at most once per memberCacheSweepEvery, however many documents arrive.
func TestMemberCacheSweepThrottled(t *testing.T) {
	shrinkTimings(t)
	root := t.TempDir()
	old := time.Now().Add(-memberCacheOrphanAge - time.Hour)
	staleDir := func(name string) string {
		dir := filepath.Join(root, "rpc", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	first := staleDir("rspec_first")
	m := cacheTestManager(t, root)
	m.Apply(Config{ETag: "1"})
	// Its removal also proves the first sweep has listed the root.
	waitUntil(t, 5*time.Second, "first sweep ran", func() bool {
		_, err := os.Stat(first)
		return os.IsNotExist(err)
	})

	second := staleDir("rspec_second")
	m.Apply(Config{ETag: "2"})
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("a second document inside the sweep interval swept again: %v", err)
	}
}

func TestMemberCacheFreeDiskFloor(t *testing.T) {
	shrinkTimings(t)
	root := t.TempDir()
	origFree := freeDiskMB
	freeDiskMB = func(string) (int, error) { return memberCacheMinFreeMB - 1, nil }
	t.Cleanup(func() { freeDiskMB = origFree })

	m := cacheTestManager(t, root)
	m.Apply(Config{Specs: []Spec{cachingMember(t, "rpcm_a", "rspec_h1")}})
	waitUntil(t, 5*time.Second, "member refused for low disk", func() bool {
		s := statusFor(m, "rpcm_a")
		return s != nil && s.State == StateNotPermitted
	})
	s := statusFor(m, "rpcm_a")
	if s.LastError == nil || !strings.Contains(s.LastError.Message, filepath.Join(root, "rpc", "rspec_h1")) {
		t.Fatalf("last error = %+v, want a message naming the cache path", s.LastError)
	}
}

// TestRelayMemberReadyOnlyOnceRelayListens: a relay-mode member is not ready
// (so EnsureMember does not return, and its state stays starting) until the
// relay gate reports the relay listening, even though the child accepts TCP;
// the gate is asked for that member, and a direct member never consults it.
func TestRelayMemberReadyOnlyOnceRelayListens(t *testing.T) {
	shrinkTimings(t)
	spec := memberSpec("rpcm_a", "rspec_h", "-tcp-listen")
	spec.RPC.Transport = "relay"
	spec.RPC.BindIP = "127.0.0.1"
	spec.RPC.Port = freeTCPPort(t)
	spec.AdminState = "force_running"
	direct := onDemandMember(t, "rpcm_d")
	direct.AdminState = "force_running"

	m := newTestManager(t, allowlistPolicy())
	var listening atomic.Bool
	var asked sync.Map
	m.SetMemberRelayGate(func(specID string) error {
		asked.Store(specID, true)
		if listening.Load() {
			return nil
		}
		return errors.New("not yet")
	})
	m.Apply(Config{Specs: []Spec{spec, direct}})

	waitUntil(t, 5*time.Second, "direct member running", func() bool {
		s := statusFor(m, "rpcm_d")
		return s != nil && s.State == StateRunning
	})
	waitUntil(t, 5*time.Second, "relay gate asked for the relay member", func() bool {
		_, ok := asked.Load("rpcm_a")
		return ok
	})
	time.Sleep(200 * time.Millisecond) // many poll ticks with the child up
	if s := statusFor(m, "rpcm_a"); s.State != StateStarting {
		t.Fatalf("relay member state = %s while its relay is down, want starting", s.State)
	}
	if _, ok := asked.Load("rpcm_d"); ok {
		t.Fatal("the relay gate was consulted for a direct member")
	}
	if s := statusFor(m, "rpcm_a"); s.RPCMember == nil || s.RPCMember.Transport != "relay" || s.RPCMember.Port != spec.RPC.Port {
		t.Fatalf("status RPCMember = %+v, want the member's relay settings", s.RPCMember)
	}

	listening.Store(true)
	waitUntil(t, 5*time.Second, "relay member running once its relay listens", func() bool {
		return statusFor(m, "rpcm_a").State == StateRunning
	})
}

// withHostAddrs makes the host's address list addrs for the test.
func withHostAddrs(t *testing.T, addrs func() ([]net.Addr, error)) {
	t.Helper()
	orig := interfaceAddrs
	interfaceAddrs = addrs
	t.Cleanup(func() { interfaceAddrs = orig })
}

// TestMemberBindNotLocalIsNotExecuted: a member whose bind_ip is not an
// address of this host is never exec'd, in either transport. It is refused
// with runtime.bind_mismatch, fast and without a retry loop, and starts on the
// first ensure after the address has appeared.
func TestMemberBindNotLocalIsNotExecuted(t *testing.T) {
	for _, transport := range []string{"direct", "relay"} {
		t.Run(transport, func(t *testing.T) {
			shrinkTimings(t)
			origRetry := notPermittedRetryInterval
			notPermittedRetryInterval = 300 * time.Millisecond
			t.Cleanup(func() { notPermittedRetryInterval = origRetry })
			var local atomic.Bool
			withHostAddrs(t, func() ([]net.Addr, error) {
				if !local.Load() {
					return []net.Addr{&net.IPNet{IP: net.ParseIP("10.9.8.7"), Mask: net.CIDRMask(8, 32)}}, nil
				}
				return []net.Addr{&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)}}, nil
			})
			argvLog := filepath.Join(t.TempDir(), "argv")
			spec := onDemandMember(t, "rpcm_a")
			spec.Args = []string{"-tcp-listen", "-argv-log", argvLog}
			spec.RPC.Transport = transport

			m := newTestManager(t, allowlistPolicy())
			m.Apply(Config{Specs: []Spec{spec}})
			ctx := t.Context()
			if err := m.EnsureMember(ctx, "rpcm_a", 0); !errors.Is(err, ErrNotPermitted) {
				t.Fatalf("EnsureMember = %v, want ErrNotPermitted", err)
			}
			s := statusFor(m, "rpcm_a")
			if s.State != StateNotPermitted || s.LastError == nil || s.LastError.Code != "runtime.bind_mismatch" ||
				!strings.Contains(s.LastError.Message, "127.0.0.1") {
				t.Fatalf("status = %+v, last_error %+v; want not_permitted with runtime.bind_mismatch naming the address", s, s.LastError)
			}
			if s.RPC == nil || !s.RPC.BindMismatch {
				t.Fatalf("rpc status = %+v, want bind_mismatch set", s.RPC)
			}
			start := time.Now()
			if err := m.EnsureMember(ctx, "rpcm_a", 0); !errors.Is(err, ErrNotPermitted) || time.Since(start) > 100*time.Millisecond {
				t.Fatalf("second EnsureMember = %v after %v, want a fast ErrNotPermitted", err, time.Since(start))
			}
			if _, err := os.Stat(argvLog); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the member was exec'd (argv log: %v)", err)
			}

			local.Store(true)
			time.Sleep(notPermittedRetryInterval + 50*time.Millisecond)
			ensureMember(t, m, "rpcm_a", 0)
			s = statusFor(m, "rpcm_a")
			if s.State != StateRunning || s.LastError != nil || s.RPC.BindMismatch {
				t.Fatalf("status = %+v, rpc %+v; want running with the mismatch cleared", s, s.RPC)
			}
		})
	}
}

// TestRelayMemberNotListeningAtDeadlineIsBindMismatch: a relay member whose
// child came up but whose relay never listened fails with runtime.bind_mismatch
// and the relay's own error, not the generic start timeout.
func TestRelayMemberNotListeningAtDeadlineIsBindMismatch(t *testing.T) {
	shrinkTimings(t)
	spec := onDemandMember(t, "rpcm_a")
	spec.RPC.Transport = "relay"
	spec.StartupTimeoutSeconds = 1
	m := newTestManager(t, allowlistPolicy())
	m.SetMemberRelayGate(func(string) error {
		return errors.New("listen tcp4 127.0.0.1:47001: bind: address already in use")
	})
	m.Apply(Config{Specs: []Spec{spec}})

	err := m.EnsureMember(t.Context(), "rpcm_a", 0)
	if err == nil || errors.Is(err, ErrStartTimeout) {
		t.Fatalf("EnsureMember = %v, want a failure other than the generic start timeout", err)
	}
	s := statusFor(m, "rpcm_a")
	if s.LastError == nil || s.LastError.Code != "runtime.bind_mismatch" || !strings.Contains(s.LastError.Message, "address already in use") {
		t.Fatalf("last_error = %+v, want runtime.bind_mismatch carrying the relay's listen error", s.LastError)
	}
	if s.RPC == nil || !s.RPC.BindMismatch {
		t.Fatalf("rpc status = %+v, want bind_mismatch set", s.RPC)
	}
}

func TestRPCServerStatusReportsDeviceCount(t *testing.T) {
	shrinkTimings(t)
	spec := memberSpec("rpcm_a", "rspec_h", "-tcp-listen")
	spec.RPC.BindIP = "127.0.0.1"
	spec.RPC.Port = freeTCPPort(t)
	spec.AdminState = "force_running"

	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})
	waitUntil(t, 5*time.Second, "member running", func() bool {
		s := statusFor(m, "rpcm_a")
		return s != nil && s.State == StateRunning
	})
	s := statusFor(m, "rpcm_a")
	if s.RPC == nil || s.RPC.DeviceCount != 1 {
		t.Fatalf("status rpc = %+v, want device_count 1 (a CPU member exposes one device)", s.RPC)
	}
}
