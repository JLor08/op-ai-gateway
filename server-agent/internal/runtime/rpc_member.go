package runtime

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// errMemberFlagReserved is returned for an rpc_server spec whose operator
// arguments set a flag the agent owns.
var errMemberFlagReserved = errors.New("runtime: -H, -p, -d and -c are set by the agent on rpc_server specs")

// memberCacheMinFreeMB is the free space the cache volume must offer before a
// member is started with -c: the rpc-server cache holds whole model tensors.
const memberCacheMinFreeMB = 5120

// defaultMemberIdleTimeout is how long an rpc_server member stays up once
// nothing holds it, when the spec does not say.
const defaultMemberIdleTimeout = 60 * time.Second

// freeDiskMB reports the free space on a path's volume; a variable so tests
// can simulate a full disk.
var freeDiskMB = diskFreeMB

// reservedMemberFlags are the rpc-server options the agent sets itself: host,
// port, device list and the cache switch, in their short and long spellings.
var reservedMemberFlags = map[string]bool{
	"-H": true, "--host": true,
	"-p": true, "--port": true,
	"-d": true, "--device": true,
	"-c": true, "--cache": true,
}

// memberFlagReserved reports whether args contain a reserved member flag,
// including its "flag=value" form.
func memberFlagReserved(args []string) bool {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		if reservedMemberFlags[name] {
			return true
		}
	}
	return false
}

// memberBind returns the host and port the member's rpc-server process binds.
// Direct exposes the member's own address; relay keeps the child on loopback
// at childPort so only the relay is reachable from outside.
func memberBind(spec Spec, childPort int) (host string, port int, err error) {
	switch spec.RPC.Transport {
	case "", "direct":
		return spec.RPC.BindIP, spec.RPC.Port, nil
	case "relay":
		return "127.0.0.1", childPort, nil
	}
	return "", 0, fmt.Errorf("runtime: unknown rpc transport %q (want direct or relay)", spec.RPC.Transport)
}

// memberArgs is the member's command line: the operator's arguments followed by
// the agent-owned -d (local devices named with the backend prefix, or CPU when
// the spec has no GPUs), -H, -p and, when the cache is on, -c. childPort is the
// loopback port a relay member's child binds; direct members ignore it. A
// member never runs in visibility-variable mode, so device names carry the
// declared host indices.
func memberArgs(spec Spec, childPort int, prefix string) ([]string, error) {
	if memberFlagReserved(spec.Args) {
		return nil, errMemberFlagReserved
	}
	host, port, err := memberBind(spec, childPort)
	if err != nil {
		return nil, err
	}
	devices := "CPU"
	if len(gpuIndices(spec)) > 0 {
		if prefix == "" {
			return nil, errors.New("runtime: an rpc_server member with GPUs needs a device backend")
		}
		devices = strings.Join(localDeviceNames(spec, prefix, false), ",")
	}
	args := make([]string, 0, len(spec.Args)+7)
	args = append(args, spec.Args...)
	args = append(args, "-d", devices, "-H", host, "-p", strconv.Itoa(port))
	if spec.RPC.CacheEnabled {
		args = append(args, "-c")
	}
	return args, nil
}

// memberCacheDir is the member's on-disk tensor cache for one head, under the
// agent-owned root. The head id is a gateway-supplied value that becomes a path
// element, so anything that could leave the directory is refused.
func memberCacheDir(root, headID string) (string, error) {
	if root == "" {
		return "", errors.New("runtime: the rpc cache needs the agent's config_files_dir to be configured")
	}
	if err := validMemberHeadID(headID); err != nil {
		return "", err
	}
	return filepath.Join(root, "rpc", headID), nil
}

// validMemberHeadID refuses a head id that is not a single, plain path element.
func validMemberHeadID(headID string) error {
	if headID == "" || headID == "." || headID == ".." || strings.ContainsAny(headID, `/\`) || filepath.Base(headID) != headID {
		return fmt.Errorf("runtime: invalid head spec id %q for the rpc cache", headID)
	}
	return nil
}

// memberEnv is the environment the agent adds to a member's launch: RDMA is
// switched off (the transport is plain TCP, and an RDMA probe stalls the
// start), and, with the cache on, the per-head cache directory.
func memberEnv(spec Spec, cacheRoot string) []string {
	env := []string{"GGML_RPC_NO_RDMA=1"}
	if spec.RPC != nil && spec.RPC.CacheEnabled {
		if dir, err := memberCacheDir(cacheRoot, spec.RPC.HeadSpecID); err == nil {
			env = append(env, "LLAMA_CACHE="+dir)
		}
	}
	return env
}

// probeTCP reports whether a TCP connection to addr succeeds within timeout.
func probeTCP(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// memberReadyAddr is where a started member is probed: its own bind address
// when direct (loopback if it binds every interface), and the child's loopback
// port when relayed.
func memberReadyAddr(spec Spec, port int) string {
	host := "127.0.0.1"
	if spec.RPC.Transport != "relay" {
		if ip := spec.RPC.BindIP; ip != "" && ip != "0.0.0.0" {
			host = ip
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// memberIdleTimeout is the idle timeout a member runs under: the spec's own, or
// the default when it says 0.
func memberIdleTimeout(spec Spec) time.Duration {
	if spec.IdleTimeoutSeconds > 0 {
		return time.Duration(spec.IdleTimeoutSeconds) * time.Second
	}
	return defaultMemberIdleTimeout
}

// prepareMember resolves everything a member launch needs beyond the shared
// expansion: the child's port, its final argv and environment, and (with the
// cache on) the cache directory with its free-space check. A returned error is
// a verdict the spec can never launch as written (not_permitted); the cache
// directory is created here, before anything is exec'd.
func (o *owner) prepareMember(spec Spec, expandedArgs, expandedEnv []string) (args, env []string, port int, err error) {
	prefix := ""
	if len(gpuIndices(spec)) > 0 {
		vendors := newDeviceContext(spec, o.gpuVendor, o.gpuVendors).vendors
		if prefix, err = ResolveDeviceBackend(spec, vendors); err != nil {
			return nil, nil, 0, fmt.Errorf("runtime: %w", err)
		}
	}
	if spec.RPC.Transport == "relay" {
		if port, err = grabEphemeralPort(); err != nil {
			return nil, nil, 0, fmt.Errorf("runtime: grab ephemeral port: %w", err)
		}
	}
	withArgs := spec
	withArgs.Args = expandedArgs
	if args, err = memberArgs(withArgs, port, prefix); err != nil {
		return nil, nil, 0, err
	}
	if spec.RPC.Transport != "relay" {
		port = spec.RPC.Port
	}
	if spec.RPC.CacheEnabled {
		dir, err := memberCacheDir(o.configFilesDir, spec.RPC.HeadSpecID)
		if err != nil {
			return nil, nil, 0, err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, nil, 0, fmt.Errorf("runtime: create rpc cache %s: %w", dir, err)
		}
		if free, err := freeDiskMB(dir); err != nil {
			slog.Warn("runtime: cannot measure free space for the rpc cache", "path", dir, "err", err)
		} else if free < memberCacheMinFreeMB {
			return nil, nil, 0, fmt.Errorf("runtime: rpc cache volume of %s has %d MB free, need at least %d MB", dir, free, memberCacheMinFreeMB)
		}
	}
	env = append(append([]string{}, expandedEnv...), memberEnv(spec, o.configFilesDir)...)
	return args, env, port, nil
}

// pollMemberReady is the member's pollHealth: ready as soon as a TCP connect
// to its probe address succeeds. A member serves no HTTP, so there is no
// health path to wait on. It stops silently when the process exits (waitForExit
// reports that) and fails with ErrStartTimeout at the startup deadline.
func (o *owner) pollMemberReady(specID string, proc *runningProc, spec Spec, deadline time.Time) {
	addr := memberReadyAddr(spec, proc.port)
	ticker := time.NewTicker(healthPollInterval)
	defer ticker.Stop()
	for {
		if probeTCP(addr, 2*time.Second) {
			o.m.postCmd(cmdStartResult{specID: specID, proc: proc, ok: true})
			return
		}
		if time.Now().After(deadline) {
			o.m.postCmd(cmdStartResult{specID: specID, proc: proc, err: ErrStartTimeout})
			return
		}
		select {
		case <-proc.exited:
			return
		case <-ticker.C:
		}
	}
}

// purgeDepartedMemberCaches removes the cache directory of every member spec in
// before whose head changed in after or that left it, unless another member in
// after still serves that head. The directories are removed off the owner
// goroutine: a cache can hold many gigabytes.
func (o *owner) purgeDepartedMemberCaches(before map[string]string, after []Spec) {
	if o.configFilesDir == "" {
		return
	}
	current := make(map[string]string, len(after))
	serving := make(map[string]bool, len(after))
	for _, spec := range after {
		if spec.IsRPCServer() {
			current[spec.ID] = spec.RPC.HeadSpecID
			serving[spec.RPC.HeadSpecID] = true
		}
	}
	for id, head := range before {
		if current[id] == head || serving[head] {
			continue
		}
		dir, err := memberCacheDir(o.configFilesDir, head)
		if err != nil {
			continue
		}
		o.m.wg.Add(1)
		go func() {
			defer o.m.wg.Done()
			if err := os.RemoveAll(dir); err != nil {
				slog.Warn("runtime: purge rpc cache", "path", dir, "err", err)
			}
		}()
	}
}

// permitMember is the member-specific half of LocalPolicy.Permit: the agent
// owns the member's host, port, devices and cache switch, so operator
// arguments may not set them, and a device-visibility variable would renumber
// the devices the agent names in -d.
func permitMember(spec Spec) error {
	if memberFlagReserved(spec.Args) {
		return errMemberFlagReserved
	}
	if spec.SetVisibleDevices && spec.VisibleDevicesMode != VisibleDevicesModeArgs {
		return errors.New("runtime: set_visible_devices in env mode is not supported on rpc_server specs: the agent names the member's devices by their host indices")
	}
	if spec.RPC.CacheEnabled {
		return validMemberHeadID(spec.RPC.HeadSpecID)
	}
	return nil
}
