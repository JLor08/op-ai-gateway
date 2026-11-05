package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const realMemberBanner = `Starting RPC server v8.0.1
  endpoint       : 127.0.0.1:50152
  local cache    : n/a
Devices:
  MTL0: Apple M4 Max (28753 MiB, 28753 MiB free)
  CPU: Apple M4 Max (36864 MiB, 36864 MiB free)
  transport      : TCP (RDMA auto-negotiate enabled)
`

func classifyHeadSpec() Spec {
	return Spec{ID: "h", Cluster: &ClusterHead{Endpoints: []ClusterEndpoint{{Address: "100.64.0.5:47001", DeviceCount: 1}}}}
}

func TestClassifyExitTable(t *testing.T) {
	member := memberSpec("rpcm_a", "rspec_h")
	head := classifyHeadSpec()
	cases := []struct {
		name string
		spec Spec
		tail string
		want exitClass
		ok   bool
	}{
		{"head invalid device", head, "error: invalid device: RPC0", exitClass{"runtime.device_backend_mismatch", true}, true},
		{"member unknown device", member, "unknown device: CUDA0", exitClass{"runtime.device_backend_mismatch", true}, true},
		{"member no devices", member, "No devices found", exitClass{"runtime.device_backend_mismatch", true}, true},
		{"head no RPC build", head, "RPC not supported in this build", exitClass{"runtime.rpc_unsupported_build", true}, true},
		{"member no RPC build", member, "RPC not supported in this build", exitClass{"runtime.rpc_unsupported_build", true}, true},
		{"handshake", head, "RPC handshake failed", exitClass{"runtime.cluster_version_mismatch", true}, true},
		{"version mismatch", member, "protocol version mismatch", exitClass{"runtime.cluster_version_mismatch", true}, true},
		{"unreachable", head, "Failed to connect to 100.64.0.5:47001", exitClass{"runtime.cluster_member_unreachable", false}, true},
		{"unrelated tail", head, "segfault", exitClass{}, false},
		{"member message on a head", head, "unknown device: CUDA0", exitClass{}, false},
		{"head message on a member", member, "invalid device: RPC0", exitClass{}, false},
		{"model spec untouched", Spec{ID: "m"}, "invalid device: RPC0 RPC not supported in this build", exitClass{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := classifyExit(c.spec, c.tail)
			if ok != c.ok || got != c.want {
				t.Fatalf("classifyExit = %+v, %v; want %+v, %v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestParseMemberBannerFromRealOutput(t *testing.T) {
	got, ok := parseMemberBanner(strings.Split(realMemberBanner, "\n"))
	if !ok || got.Protocol != "8.0.1" || len(got.Devices) != 2 || got.Devices[0] != "MTL0" || got.Devices[1] != "CPU" {
		t.Fatalf("banner = %+v, %v; want protocol 8.0.1 with devices MTL0, CPU", got, ok)
	}
}

func TestParseMemberBannerIgnoresInterleavedInitLines(t *testing.T) {
	text := "ggml_metal_init: GPU name: Apple M4 Max\nStarting RPC server v8.0.1\n  endpoint       : 127.0.0.1:1\nDevices:\nggml_metal_init: found device\n  MTL0: Apple M4 Max (1 MiB, 1 MiB free)\n  transport      : TCP\n  CPU: after the block\n"
	got, ok := parseMemberBanner(strings.Split(text, "\n"))
	if !ok || len(got.Devices) != 1 || got.Devices[0] != "MTL0" {
		t.Fatalf("banner = %+v, %v; want one device MTL0", got, ok)
	}
}

func TestParseMemberBannerIncomplete(t *testing.T) {
	if _, ok := parseMemberBanner([]string{"Starting RPC server v8.0.1", "Devices:", "  MTL0: x"}); ok {
		t.Fatal("a banner without its closing transport line must not parse")
	}
}

func memberBannerPrint(devices int) string {
	var b strings.Builder
	b.WriteString(`Starting RPC server v8.0.1\n  endpoint       : 127.0.0.1:1\nDevices:\n`)
	for i := range devices {
		b.WriteString(`  DEV` + strconv.Itoa(i) + `: stub (1 MiB, 1 MiB free)\n`)
	}
	b.WriteString(`  transport      : TCP\n`)
	return b.String()
}

func TestMemberDeviceCountMismatchFailsStart(t *testing.T) {
	shrinkTimings(t)
	spec := memberSpec("rpcm_a", "rspec_h", "-tcp-listen", "-print", memberBannerPrint(1))
	spec.RPC.BindIP = "127.0.0.1"
	spec.RPC.Port = freeTCPPort(t)
	spec.GPUs = []SpecGPU{{Index: 0}, {Index: 1}}
	spec.DeviceBackend = "cuda"
	spec.AdminState = "force_running"

	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})
	waitUntil(t, 5*time.Second, "member refused", func() bool {
		s := statusFor(m, "rpcm_a")
		return s != nil && s.State == StateNotPermitted && s.LastError != nil
	})
	if code := statusFor(m, "rpcm_a").LastError.Code; code != "runtime.rpc_device_count_mismatch" {
		t.Fatalf("code = %q, want runtime.rpc_device_count_mismatch", code)
	}
}

func TestMemberDeviceCountMatchRunsAndReportsProtocol(t *testing.T) {
	shrinkTimings(t)
	spec := memberSpec("rpcm_a", "rspec_h", "-tcp-listen", "-print", memberBannerPrint(1))
	spec.RPC.BindIP = "127.0.0.1"
	spec.RPC.Port = freeTCPPort(t)
	spec.AdminState = "force_running"

	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})
	waitUntil(t, 5*time.Second, "member running", func() bool {
		s := statusFor(m, "rpcm_a")
		return s != nil && s.State == StateRunning
	})
	if got := statusFor(m, "rpcm_a").RPCProtocol; got != "8.0.1" {
		t.Fatalf("protocol = %q, want 8.0.1", got)
	}
}

func headSpecPrinting(text string) Spec {
	spec := Spec{
		ID: "rspec_h", Binary: stubchildPath, UpstreamModel: "m",
		Args:    []string{"-print", text, "-crash-after", "50ms", "-exit-code", "1"},
		Env:     map[string]string{},
		Cluster: &ClusterHead{Endpoints: []ClusterEndpoint{{Address: "100.64.0.5:47001", DeviceCount: 1}}},
	}
	return spec
}

func execCount(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "\n")
}

func TestHeadInvalidDeviceIsTerminal(t *testing.T) {
	shrinkTimings(t)
	spec := headSpecPrinting(`error: invalid device: RPC0\n`)
	spec.AdminState = "force_running"

	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})
	var got *Status
	waitUntil(t, 5*time.Second, "head refused", func() bool {
		got = statusFor(m, "rspec_h")
		return got != nil && got.State == StateNotPermitted && got.LastError != nil
	})
	le := got.LastError
	if le.Code != "runtime.device_backend_mismatch" || le.Failures != 0 || le.ExitCode != 1 {
		t.Fatalf("LastError = %+v, want code runtime.device_backend_mismatch, no failure count, exit code 1", le)
	}
}

func TestTerminalHeadStaysPutWhenForceRunning(t *testing.T) {
	shrinkTimings(t)
	invocations := filepath.Join(t.TempDir(), "inv")
	spec := headSpecPrinting(`error: invalid device: RPC0\n`)
	spec.Args = append(spec.Args, "-invocation-log", invocations)
	spec.AdminState = "force_running"

	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})
	waitUntil(t, 5*time.Second, "head refused", func() bool {
		s := statusFor(m, "rspec_h")
		return s != nil && s.State == StateNotPermitted
	})
	time.Sleep(500 * time.Millisecond) // many idle ticks and wakes
	if n := execCount(t, invocations); n != 1 {
		t.Fatalf("head exec'd %d times, want 1: a terminal verdict must not be retried", n)
	}
	if s := statusFor(m, "rspec_h"); s.State != StateNotPermitted {
		t.Fatalf("state = %s, want not_permitted", s.State)
	}
}

func TestTerminalMemberMismatchStaysPutWhenForceRunning(t *testing.T) {
	shrinkTimings(t)
	invocations := filepath.Join(t.TempDir(), "inv")
	spec := memberSpec("rpcm_a", "rspec_h", "-tcp-listen", "-invocation-log", invocations, "-print", memberBannerPrint(1))
	spec.RPC.BindIP = "127.0.0.1"
	spec.RPC.Port = freeTCPPort(t)
	spec.GPUs = []SpecGPU{{Index: 0}, {Index: 1}}
	spec.DeviceBackend = "cuda"
	spec.AdminState = "force_running"

	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})
	waitUntil(t, 5*time.Second, "member refused", func() bool {
		s := statusFor(m, "rpcm_a")
		return s != nil && s.State == StateNotPermitted
	})
	time.Sleep(600 * time.Millisecond)
	if n := execCount(t, invocations); n != 1 {
		t.Fatalf("member exec'd %d times, want 1", n)
	}
}

func TestTerminalOnDemandHeadRefusesEnsureUntilSpecChanges(t *testing.T) {
	shrinkTimings(t)
	orig := notPermittedRetryInterval
	notPermittedRetryInterval = 20 * time.Millisecond
	t.Cleanup(func() { notPermittedRetryInterval = orig })
	invocations := filepath.Join(t.TempDir(), "inv")
	spec := headSpecPrinting(`error: invalid device: RPC0\n`)
	spec.Args = append(spec.Args, "-invocation-log", invocations)

	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := m.EnsureRunning(ctx, "m"); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("first ensure err = %v, want ErrNotPermitted", err)
	}
	time.Sleep(100 * time.Millisecond) // well past the rate-limit interval
	for range 3 {
		if _, _, err := m.EnsureRunning(ctx, "m"); !errors.Is(err, ErrNotPermitted) {
			t.Fatalf("later ensure err = %v, want ErrNotPermitted", err)
		}
	}
	if n := execCount(t, invocations); n != 1 {
		t.Fatalf("head exec'd %d times, want 1", n)
	}

	// A changed spec is a new chance: it is exec'd again.
	changed := spec
	changed.Args = append(append([]string{}, spec.Args...), "-t", "4")
	m.Apply(Config{Specs: []Spec{changed}})
	time.Sleep(60 * time.Millisecond)
	_, _, _ = m.EnsureRunning(ctx, "m")
	if n := execCount(t, invocations); n != 2 {
		t.Fatalf("head exec'd %d times after the spec changed, want 2", n)
	}
}

func TestMemberWithoutBannerPassesUnverified(t *testing.T) {
	shrinkTimings(t)
	spec := memberSpec("rpcm_a", "rspec_h", "-tcp-listen")
	spec.RPC.BindIP = "127.0.0.1"
	spec.RPC.Port = freeTCPPort(t)
	spec.GPUs = []SpecGPU{{Index: 0}, {Index: 1}}
	spec.DeviceBackend = "cuda"
	spec.AdminState = "force_running"

	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})
	waitUntil(t, 5*time.Second, "member running", func() bool {
		s := statusFor(m, "rpcm_a")
		return s != nil && s.State == StateRunning
	})
	if got := statusFor(m, "rpcm_a").RPCProtocol; got != "" {
		t.Fatalf("protocol = %q, want empty for a member that printed no banner", got)
	}
}

func TestExitTailIsScopedToTheCurrentGeneration(t *testing.T) {
	store := NewLogStore(1<<16, 1<<18)
	old := store.newProc("rspec_h")
	old.Write([]byte("error: invalid device: RPC0\n"))
	cur := store.newProc("rspec_h")
	cur.Write([]byte("fresh start\n"))
	if tail := cur.Tail(2048); strings.Contains(tail, "invalid device") {
		t.Fatalf("tail %q carries the previous generation's output", tail)
	}
}

func TestHeadFailedToConnectBacksOffWithCode(t *testing.T) {
	shrinkTimings(t)
	spec := Spec{
		ID: "rspec_h", Binary: stubchildPath, UpstreamModel: "m",
		Args:    []string{"-print", `Failed to connect to 100.64.0.5:47001\n`, "-crash-after", "50ms", "-exit-code", "1"},
		Env:     map[string]string{},
		Cluster: &ClusterHead{Endpoints: []ClusterEndpoint{{Address: "100.64.0.5:47001", DeviceCount: 1}}},
	}
	spec.AdminState = "force_running"

	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{spec}})
	waitUntil(t, 5*time.Second, "head in backoff", func() bool {
		s := statusFor(m, "rspec_h")
		return s != nil && s.State != StateNotPermitted && s.LastError != nil && s.LastError.Code == "runtime.cluster_member_unreachable"
	})
}
