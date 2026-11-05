package runtime

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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

	// Leaving the document drops the current head's cache too.
	waitUntil(t, 5*time.Second, "member running on the new head", func() bool {
		_, err := os.Stat(filepath.Join(root, "rpc", "rspec_h2"))
		return err == nil
	})
	m.Apply(Config{ETag: "4"})
	waitUntil(t, 5*time.Second, "departed member cache purged", func() bool {
		_, err := os.Stat(filepath.Join(root, "rpc", "rspec_h2"))
		return os.IsNotExist(err)
	})
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
