// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Error codes the agent attaches to LastError for llama.cpp RPC failures. They
// are stable: the gateway maps each to an operator-facing message.
const (
	codeDeviceBackendMismatch  = "runtime.device_backend_mismatch"
	codeRPCUnsupportedBuild    = "runtime.rpc_unsupported_build"
	codeClusterVersionMismatch = "runtime.cluster_version_mismatch"
	codeClusterMemberUnreach   = "runtime.cluster_member_unreachable"
	codeRPCDeviceCountMismatch = "runtime.rpc_device_count_mismatch"
)

// exitClass is the verdict on a cluster process's failed exit. A Terminal
// verdict is one no retry can change (the configuration has to), so it is
// reported like not_permitted: no backoff, until the spec changes.
type exitClass struct {
	Code     string
	Terminal bool
}

// rpcExitRule maps one llama.cpp message to a verdict. head and member say
// which kind of spec prints it; a message printed by only one side is not
// matched against the other, so an unrelated process is never misread.
type rpcExitRule struct {
	needle   string
	head     bool
	member   bool
	class    exitClass
	describe string
}

// rpcExitRules is a deliberately small, explicit table: free-text matching is
// confined to the llama.cpp RPC messages the cluster mode depends on and to
// cluster heads and rpc_server members, so model specs are never classified.
var rpcExitRules = []rpcExitRule{
	{
		"invalid device: ", true, false,
		exitClass{codeDeviceBackendMismatch, true},
		"the cluster head names a device its llama.cpp build does not provide",
	},
	{
		"unknown device: ", false, true,
		exitClass{codeDeviceBackendMismatch, true},
		"the rpc-server member names a device its llama.cpp build does not provide",
	},
	{
		"No devices found", false, true,
		exitClass{codeDeviceBackendMismatch, true},
		"the rpc-server member found no usable device",
	},
	{
		"RPC not supported in this build", true, true,
		exitClass{codeRPCUnsupportedBuild, true},
		"this llama.cpp build has no RPC backend",
	},
	{
		"RPC handshake failed", true, true,
		exitClass{codeClusterVersionMismatch, true},
		"the RPC handshake between head and member failed",
	},
	{
		"protocol version mismatch", true, true,
		exitClass{codeClusterVersionMismatch, true},
		"head and member speak different RPC protocol versions",
	},
	{
		"Failed to connect", true, false,
		exitClass{codeClusterMemberUnreach, false},
		"the cluster head could not connect to an rpc-server member",
	},
}

// matchExitRule returns the first rule that applies to spec and whose message
// appears in tail. It reports false for anything that is not a cluster head or
// rpc_server member, and for output that matches no rule.
func matchExitRule(spec Spec, tail string) (rpcExitRule, bool) {
	head, member := spec.IsClusterHead(), spec.IsRPCServer()
	if !head && !member {
		return rpcExitRule{}, false
	}
	for _, r := range rpcExitRules {
		if (head && r.head || member && r.member) && strings.Contains(tail, r.needle) {
			return r, true
		}
	}
	return rpcExitRule{}, false
}

// classifyExit reads the tail of a failed cluster process's output.
func classifyExit(spec Spec, tail string) (exitClass, bool) {
	r, ok := matchExitRule(spec, tail)
	return r.class, ok
}

// exitClassMessage is the LastError message for a classified exit.
func exitClassMessage(r rpcExitRule, exitCode int) string {
	return fmt.Sprintf("runtime: %s (exit code %d)", r.describe, exitCode)
}

// memberBanner is what an rpc-server prints at startup: its protocol version
// and the local devices it exposes.
type memberBanner struct {
	Protocol string
	Devices  []string
}

// memberBannerStart begins the line of an rpc-server's banner that carries its
// protocol version.
const memberBannerStart = "Starting RPC server v"

var memberDeviceLine = regexp.MustCompile(`^  ([A-Za-z0-9_]+): `)

// parseMemberBanner extracts the banner from a member's output lines. Lines
// from backend initialisation may be interleaved anywhere; only the "Starting
// RPC server vX" line and the indented "NAME: ..." lines of the "Devices:"
// block, which ends at the "  transport" line, count. It reports false until
// the whole block, including that closing line, has been seen.
func parseMemberBanner(lines []string) (memberBanner, bool) {
	var b memberBanner
	started, inDevices := false, false
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, memberBannerStart):
			b = memberBanner{Protocol: strings.TrimSpace(strings.TrimPrefix(line, memberBannerStart))}
			started, inDevices = true, false
		case !started:
		case strings.HasPrefix(line, "Devices:"):
			inDevices = true
		case inDevices && strings.HasPrefix(line, "  transport"):
			return b, true
		case inDevices:
			if m := memberDeviceLine.FindStringSubmatch(line); m != nil {
				b.Devices = append(b.Devices, m[1])
			}
		}
	}
	return memberBanner{}, false
}

// expectedMemberDevices is how many devices a member's banner must list: one
// per distinct GPU row, or the single CPU device of a member without GPUs.
func expectedMemberDevices(spec Spec) int {
	if n := len(gpuIndices(spec)); n > 0 {
		return n
	}
	return 1
}

// memberBannerGrace bounds how long a member that has started printing its
// banner is given to finish it once its port accepts connections.
var memberBannerGrace = 3 * time.Second

// awaitMemberBanner returns the member's banner once it is complete. It
// returns false at once when the member printed no banner at all (nothing to
// verify, for example a build with different output), and after the grace
// period or deadline when the banner stays incomplete.
func awaitMemberBanner(proc *runningProc, deadline time.Time) (memberBanner, bool) {
	stop := time.Now().Add(memberBannerGrace)
	if deadline.Before(stop) {
		stop = deadline
	}
	for {
		tail := proc.log.Tail(64 << 10)
		if b, ok := parseMemberBanner(strings.Split(tail, "\n")); ok {
			return b, true
		}
		if !strings.Contains(tail, memberBannerStart) || !time.Now().Before(stop) {
			return memberBanner{}, false
		}
		select {
		case <-proc.exited:
			return memberBanner{}, false
		case <-time.After(25 * time.Millisecond):
		}
	}
}
