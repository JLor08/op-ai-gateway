// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// codeBindMismatch reports a member that cannot listen where the head dials it:
// its bind address is not an address of this host, or its relay never listened.
// Not a terminal verdict: the address may appear later (a mesh coming up).
const codeBindMismatch = "runtime.bind_mismatch"

// interfaceAddrs lists this host's addresses; a variable so tests can make an
// address appear or vanish.
var interfaceAddrs = net.InterfaceAddrs

// memberBindLocal returns nil when the member's bind address can be bound
// here: "0.0.0.0", or an address assigned to this host. Otherwise it returns
// the refusal; an address list that cannot be read counts as not local.
func memberBindLocal(spec Spec) error {
	bind := spec.RPC.BindIP
	if bind == "0.0.0.0" {
		return nil
	}
	if want := net.ParseIP(bind); want != nil {
		if addrs, err := interfaceAddrs(); err == nil {
			for _, a := range addrs {
				var ip net.IP
				switch v := a.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				if ip != nil && ip.Equal(want) {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("runtime: the rpc bind address %q is not an address of this host; the member is not started until it is", bind)
}

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
	case "direct":
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
		if err := markMemberCache(dir); err != nil {
			slog.Warn("runtime: cannot mark the rpc cache for the sweep; without a marker it is never swept, and with an existing marker it may be removed once its head is no longer listed and nothing in it changed for 7 days", "path", dir, "err", err)
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
// to its probe address succeeds and, in relay mode, the relay listens (see
// SetMemberRelayGate). A member serves no HTTP, so there is no health path to
// wait on. It stops silently when the process exits (waitForExit
// reports that) and fails with ErrStartTimeout at the startup deadline.
func (o *owner) pollMemberReady(specID string, proc *runningProc, spec Spec, deadline time.Time) {
	addr := memberReadyAddr(spec, proc.port)
	ticker := time.NewTicker(healthPollInterval)
	defer ticker.Stop()
	for {
		childUp := probeTCP(addr, 2*time.Second)
		var relayErr error
		if childUp {
			relayErr = o.m.relayListening(spec)
		}
		if childUp && relayErr == nil {
			res := cmdStartResult{specID: specID, proc: proc, ok: true}
			if banner, found := awaitMemberBanner(proc, deadline); found {
				res.rpcProtocol = banner.Protocol
				if want := expectedMemberDevices(spec); len(banner.Devices) != want {
					res.deviceMismatch = fmt.Sprintf("runtime: the rpc-server listed %d device(s) (%s) but the spec selects %d",
						len(banner.Devices), strings.Join(banner.Devices, ", "), want)
				}
			}
			o.m.postCmd(res)
			return
		}
		if time.Now().After(deadline) {
			res := cmdStartResult{specID: specID, proc: proc, err: ErrStartTimeout}
			if relayErr != nil {
				res.err = ErrStartFailed
				res.relayDown = fmt.Sprintf("runtime: the rpc-server is up but its relay is not listening on %s: %v",
					net.JoinHostPort(spec.RPC.BindIP, strconv.Itoa(spec.RPC.Port)), relayErr)
			}
			o.m.postCmd(res)
			return
		}
		select {
		case <-proc.exited:
			return
		case <-ticker.C:
		}
	}
}

// memberCacheOrphanAge is how long a cache directory that belongs to no member
// of the current document must have gone unmodified before the sweep removes
// it. A member leaves the document for reasons that pass (a gateway restart
// that has not heard from every cluster host yet, an operator turning the
// cluster off for a while), and its head's cache is what makes the next start
// fast, so only a cache nobody has touched for this long is dropped.
const memberCacheOrphanAge = 7 * 24 * time.Hour

// memberCacheSweepEvery is how often, at most, the cache root is swept, and
// how old a purge tombstone must be before the sweep removes it (a younger one
// may still be under removal by its purge). A variable so tests can shorten
// it.
var memberCacheSweepEvery = time.Hour

// memberCacheMarker is the empty file the agent puts into every member cache
// directory it creates. The sweep removes only directories that carry it (and
// its own purge tombstones): the cache root is <config_files_dir>/rpc, which a
// file-mode spec with the id "rpc" shares for its config files, and anything
// else found there is not the agent's to delete.
const memberCacheMarker = ".op-rpc-member-cache"

// purgeTombstonePrefix starts the name a purged cache directory is moved to
// before it is removed (purgeMemberCache).
const purgeTombstonePrefix = ".purge-"

// purgeChangedHeadCaches removes the cache directory of every member spec in
// before that is still in after but now serves another head, unless another
// member in after still serves the old head. A member that left the document
// keeps its cache: the sweep (memberCacheSweeper) removes it once it has been
// unused long enough. The directories are removed off the owner goroutine: a
// cache can hold many gigabytes.
func (o *owner) purgeChangedHeadCaches(before map[string]string, after []Spec) {
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
		if now, listed := current[id]; !listed || now == head || serving[head] {
			continue
		}
		dir, err := memberCacheDir(o.configFilesDir, head)
		if err != nil {
			continue
		}
		o.m.wg.Go(func() {
			if err := os.RemoveAll(dir); err != nil {
				slog.Warn("runtime: purge rpc cache", "path", dir, "err", err)
			}
		})
	}
}

// markMemberCache puts the sweep's marker into a member cache directory
// (keeping one that is there). Failing only means the sweep never removes
// that directory, and never keeps it young; the caller decides how loudly to
// say so.
func markMemberCache(dir string) error {
	f, err := os.OpenFile(filepath.Join(dir, memberCacheMarker), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// maybeSweepMemberCaches starts a sweep of the cache root against specs, the
// document in force, on the first apply and then at most once per
// memberCacheSweepEvery: from a document apply, and from the owner's
// housekeeping beat once a document has been applied (sweepTick), so a
// listed head's cache is touched while no new document arrives. The sweep runs
// off the owner goroutine.
func (o *owner) maybeSweepMemberCaches(specs []Spec) {
	if o.configFilesDir == "" || o.closing {
		return
	}
	now := time.Now()
	if !o.lastCacheSweep.IsZero() && now.Sub(o.lastCacheSweep) < memberCacheSweepEvery {
		return
	}
	o.lastCacheSweep = now
	keep := make(map[string]bool)
	for _, spec := range specs {
		if spec.IsRPCServer() {
			keep[spec.RPC.HeadSpecID] = true
		}
	}
	root := filepath.Join(o.configFilesDir, "rpc")
	if o.cacheSweeper == nil {
		o.cacheSweeper = newMemberCacheSweeper()
	}
	sw := o.cacheSweeper
	o.m.wg.Go(func() { sw.sweep(root, keep, now) })
}

// sweepTick is the housekeeping beat's sweep: the document in force, but only
// once one has been applied (the first apply sweeps first), so the agent
// never sweeps against the empty set it starts with.
func (o *owner) sweepTick() {
	if o.lastCacheSweep.IsZero() {
		return
	}
	o.maybeSweepMemberCaches(o.cfg.Specs)
}

// memberCacheSweeper sweeps the cache root (sweep). It remembers, across
// sweeps, the heads whose marker it last failed to touch, so that failure is
// logged once per streak rather than every hour. Sweeps may overlap (one
// outliving the interval), so the record is guarded.
type memberCacheSweeper struct {
	// mark is markMemberCache and chtimes os.Chtimes; fields so tests can
	// make either leg of a touch fail.
	mark    func(dir string) error
	chtimes func(name string, atime, mtime time.Time) error

	mu          sync.Mutex
	touchFailed map[string]bool // listed heads in a streak of failed touches
}

func newMemberCacheSweeper() *memberCacheSweeper {
	return &memberCacheSweeper{mark: markMemberCache, chtimes: os.Chtimes, touchFailed: map[string]bool{}}
}

// sweep sweeps the cache root. The cache of every head in keep is touched: its
// marker is created if missing and its modification time set to now, so the
// age the sweep judges is the time since a member last served that head, not
// since its tensors were written (they are written once and then only read).
// The marker is touched rather than the directory because Windows refuses to
// change a directory's times while another process (Explorer, the indexer, a
// virus scanner) holds it open. Every other directory that carries
// memberCacheMarker and whose newest modification -- the marker's above all,
// or anything else inside it, the directory included -- lies more than
// memberCacheOrphanAge before now is removed; this covers caches orphaned
// while the agent was down. A purge tombstone is removed once its purge,
// recorded in its name, lies more than memberCacheSweepEvery back. Anything
// else -- a file, a directory without the marker -- is left alone.
func (sw *memberCacheSweeper) sweep(root string, keep map[string]bool, now time.Time) {
	// A head that is no longer listed ends its streak: should it come back
	// still failing, that is worth a Warn again.
	sw.mu.Lock()
	for head := range sw.touchFailed {
		if !keep[head] {
			delete(sw.touchFailed, head)
		}
	}
	sw.mu.Unlock()
	entries, err := os.ReadDir(root)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("runtime: sweep rpc caches", "path", root, "err", err)
		}
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, name)
		if keep[name] {
			sw.touch(name, dir, now)
			continue
		}
		if at, ok := tombstonePurgedAt(name); ok {
			if now.Sub(at) < memberCacheSweepEvery {
				continue
			}
		} else if _, err := os.Stat(filepath.Join(dir, memberCacheMarker)); err != nil {
			continue
		} else if newest, err := newestModTime(dir); err != nil || now.Sub(newest) < memberCacheOrphanAge {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("runtime: sweep rpc cache", "path", dir, "err", err)
			continue
		}
		slog.Info("runtime: removed an unused rpc cache", "path", dir)
	}
}

// touch marks the cache dir of head and sets its marker's times to now. A
// failure of either step is logged at Warn on the first failing sweep for
// that head only; the next successful touch, or the head leaving the listed
// set, ends the streak.
func (sw *memberCacheSweeper) touch(head, dir string, now time.Time) {
	err := sw.mark(dir)
	if err == nil {
		err = sw.chtimes(filepath.Join(dir, memberCacheMarker), now, now)
	}
	sw.mu.Lock()
	first := err != nil && !sw.touchFailed[head]
	if err != nil {
		sw.touchFailed[head] = true
	} else {
		delete(sw.touchFailed, head)
	}
	sw.mu.Unlock()
	if first {
		slog.Warn("runtime: cannot mark or touch the rpc cache marker; the cache is kept while its head is listed, and may be removed once its head is no longer listed and nothing in it changed for 7 days",
			"path", dir, "err", err)
	}
}

// tombstonePurgedAt reads the purge time out of a tombstone's name
// (".purge-<head>-<unix nanoseconds>"); ok is false for any other name.
func tombstonePurgedAt(name string) (time.Time, bool) {
	rest, found := strings.CutPrefix(name, purgeTombstonePrefix)
	if !found {
		return time.Time{}, false
	}
	i := strings.LastIndexByte(rest, '-')
	if i < 0 {
		return time.Time{}, false
	}
	nanos, err := strconv.ParseInt(rest[i+1:], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(0, nanos), true
}

// newestModTime is the latest modification time of dir and everything below
// it.
func newestModTime(dir string) (time.Time, error) {
	var newest time.Time
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if t := info.ModTime(); t.After(newest) {
			newest = t
		}
		return nil
	})
	return newest, err
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

// SetMemberRelayGate installs the check that holds a relay-mode member's
// readiness until its relay listens. The head can reach a relay member only
// through the relay, so a member whose child is up but whose relay is not
// listening (its bind address is not an address of this host, or the address
// is still in use) is not ready: it stays starting, EnsureMember does not
// return, and the start times out like any other that never became healthy.
// The gate is asked from the member's readiness poll, off the owner goroutine,
// with the member's spec ID, once its child accepts TCP; it may call back into
// the Manager (Status) to bring the relay up. nil removes it, and with none
// installed a relay member is ready on its child alone. Direct members never
// consult it.
func (m *Manager) SetMemberRelayGate(gate func(specID string) error) {
	if gate == nil {
		m.relayGate.Store(nil)
		return
	}
	m.relayGate.Store(&gate)
}

// relayListening returns nil when spec passes the relay gate (always for a
// direct member or with no gate installed), else why its relay is not
// listening.
func (m *Manager) relayListening(spec Spec) error {
	if spec.RPC == nil || spec.RPC.Transport != "relay" {
		return nil
	}
	if gate := m.relayGate.Load(); gate != nil {
		return (*gate)(spec.ID)
	}
	return nil
}
