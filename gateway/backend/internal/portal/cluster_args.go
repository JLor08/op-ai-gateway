// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import "strings"

// The llama.cpp placeholders and flags a cluster head's args and env are
// checked for. The flag spellings are llama-server's own (common/arg.cpp at
// the pinned build): --rpc only has a long form, --device also answers to
// -dev, --tensor-split to -ts and --mmproj-device to -mmdev. Each option can
// also be set through its environment variable.
const (
	placeholderRPCServers = "${RPC_SERVERS}"
	placeholderRPCDevices = "${RPC_DEVICES}"
	placeholderDevices    = "${DEVICES}"
	placeholderFirstLocal = "${FIRST_LOCAL_DEVICE}"

	envLlamaArgRPC         = "LLAMA_ARG_RPC"
	envLlamaArgDevice      = "LLAMA_ARG_DEVICE"
	envLlamaArgTensorSplit = "LLAMA_ARG_TENSOR_SPLIT"
	envMtmdBackendDevice   = "MTMD_BACKEND_DEVICE"
)

var (
	clusterRPCFlags         = []string{"--rpc"}
	clusterDeviceFlags      = []string{"--device", "-dev"}
	clusterTensorSplitFlags = []string{"--tensor-split", "-ts"}
	clusterMMDevFlags       = []string{"--mmproj-device", "-mmdev"}
)

// clusterArgScan is what a head's args and env say about RPC and device
// selection. llama-server parses every environment variable first and then
// argv left to right, and --device resolves its names while it is parsed,
// so where each option is set decides whether the RPC devices exist yet.
type clusterArgScan struct {
	// rpcInArgs / rpcInEnv: --rpc in args, LLAMA_ARG_RPC in env.
	rpcInArgs bool
	rpcInEnv  bool
	// rpcArgIndex is the index of the first --rpc in args, -1 when absent;
	// rpcArgCount counts every --rpc in args.
	rpcArgIndex int
	rpcArgCount int
	// rpcValueHasPlaceholder: the value of --rpc or LLAMA_ARG_RPC carries
	// ${RPC_SERVERS}.
	rpcValueHasPlaceholder bool
	// rpcPlaceholderCount counts ${RPC_SERVERS} across every arg and env value.
	rpcPlaceholderCount int
	// deviceInArgs / deviceInEnv: --device/-dev in args, LLAMA_ARG_DEVICE in
	// env; deviceArgIndex is the first one in args, -1 when absent.
	deviceInArgs   bool
	deviceInEnv    bool
	deviceArgIndex int
	// deviceList is the device list llama-server ends up with: the one in
	// args when given (argv overrides env), else the env one.
	deviceList string
	// deviceHasRPC: deviceList names the RPC devices (${DEVICES} or
	// ${RPC_DEVICES}).
	deviceHasRPC bool
	// tensorSplit: --tensor-split/-ts or LLAMA_ARG_TENSOR_SPLIT is set.
	tensorSplit bool
	// mmdev: --mmproj-device/-mmdev or MTMD_BACKEND_DEVICE is set.
	mmdev bool
	// clusterPlaceholder: ${RPC_SERVERS} or ${RPC_DEVICES} appears anywhere.
	clusterPlaceholder bool
	// equalsForm: one of the options above is written as "--flag=value" or
	// "-flag=value", which llama-server refuses as an invalid argument.
	equalsForm bool
}

// hasDeviceList reports whether a device list is given at all.
func (c clusterArgScan) hasDeviceList() bool { return c.deviceInArgs || c.deviceInEnv }

// deviceListStartsWithRPC reports whether the device list's first entry is
// an RPC device: ${DEVICES} renders the RPC devices first on a head, and
// ${RPC_DEVICES} or a literal RPC<n> are RPC devices themselves. On a head
// without GPU rows ${FIRST_LOCAL_DEVICE} falls back to the first RPC device,
// so it counts too when headHasGPUs is false.
func (c clusterArgScan) deviceListStartsWithRPC(headHasGPUs bool) bool {
	first, _, _ := strings.Cut(c.deviceList, ",")
	first = strings.TrimSpace(first)
	if !headHasGPUs && strings.HasPrefix(first, placeholderFirstLocal) {
		return true
	}
	return strings.HasPrefix(first, placeholderDevices) || strings.HasPrefix(first, placeholderRPCDevices) ||
		strings.HasPrefix(first, "RPC")
}

// normalizeLlamaFlag returns the option name of one argv element and the
// value attached with "=", if any. llama-server treats "_" and "-" alike in
// long options, so --tensor_split is --tensor-split. llama-server itself does
// not split "=" (it refuses such an element as an invalid argument); the scan
// splits it only to recognise the option and refuse the form.
func normalizeLlamaFlag(arg string) (name, value string, attached bool) {
	a := strings.TrimSpace(arg)
	if !strings.HasPrefix(a, "-") {
		return a, "", false
	}
	name, value, attached = strings.Cut(a, "=")
	if strings.HasPrefix(name, "--") {
		name = strings.ReplaceAll(name, "_", "-")
	}
	return name, value, attached
}

// flagValue returns the value of the option at args[i]: the attached one,
// else the next element ("" when there is none).
func flagValue(args []string, i int, value string, attached bool) string {
	if attached {
		return value
	}
	if i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func isOneOf(name string, set []string) bool {
	for _, s := range set {
		if name == s {
			return true
		}
	}
	return false
}

// scanClusterArgs reads a spec's args and env for the cluster head rules.
func scanClusterArgs(args []string, env map[string]string) clusterArgScan {
	scan := clusterArgScan{rpcArgIndex: -1, deviceArgIndex: -1}
	for _, a := range args {
		scan.rpcPlaceholderCount += strings.Count(a, placeholderRPCServers)
		if strings.Contains(a, placeholderRPCServers) || strings.Contains(a, placeholderRPCDevices) {
			scan.clusterPlaceholder = true
		}
	}
	for _, v := range env {
		scan.rpcPlaceholderCount += strings.Count(v, placeholderRPCServers)
		if strings.Contains(v, placeholderRPCServers) || strings.Contains(v, placeholderRPCDevices) {
			scan.clusterPlaceholder = true
		}
	}
	argDeviceList := ""
	for i := 0; i < len(args); i++ {
		name, value, attached := normalizeLlamaFlag(args[i])
		scanned := true
		switch {
		case isOneOf(name, clusterRPCFlags):
			scan.rpcInArgs = true
			scan.rpcArgCount++
			if scan.rpcArgIndex < 0 {
				scan.rpcArgIndex = i
			}
			if strings.Contains(flagValue(args, i, value, attached), placeholderRPCServers) {
				scan.rpcValueHasPlaceholder = true
			}
		case isOneOf(name, clusterDeviceFlags):
			if !scan.deviceInArgs {
				scan.deviceArgIndex = i
			}
			scan.deviceInArgs = true
			argDeviceList = flagValue(args, i, value, attached)
		case isOneOf(name, clusterTensorSplitFlags):
			scan.tensorSplit = true
		case isOneOf(name, clusterMMDevFlags):
			scan.mmdev = true
		default:
			scanned = false
		}
		if scanned && attached {
			scan.equalsForm = true
		}
		// Step over the option's value, so a value that looks like an option
		// ("--alias -dev") is not read as one.
		if !attached {
			i += llamaServerValueFlags[name]
		}
	}
	if v, ok := envValue(env, envLlamaArgRPC); ok {
		scan.rpcInEnv = true
		if strings.Contains(v, placeholderRPCServers) {
			scan.rpcValueHasPlaceholder = true
		}
	}
	envDeviceList, envDevice := envValue(env, envLlamaArgDevice)
	scan.deviceInEnv = envDevice
	if _, ok := envValue(env, envLlamaArgTensorSplit); ok {
		scan.tensorSplit = true
	}
	if _, ok := envValue(env, envMtmdBackendDevice); ok {
		scan.mmdev = true
	}
	switch {
	case scan.deviceInArgs:
		scan.deviceList = argDeviceList
	case scan.deviceInEnv:
		scan.deviceList = envDeviceList
	}
	scan.deviceHasRPC = strings.Contains(scan.deviceList, placeholderDevices) ||
		strings.Contains(scan.deviceList, placeholderRPCDevices)
	return scan
}

// envValue looks key up exactly: llama-server reads its variables by their
// exact upper-case names, and the portal only admits upper-case keys.
func envValue(env map[string]string, key string) (string, bool) {
	v, ok := env[key]
	return v, ok
}
