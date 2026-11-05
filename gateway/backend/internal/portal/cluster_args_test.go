// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import "testing"

func TestScanClusterArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want clusterArgScan
	}{
		{
			name: "rpc then device in args",
			args: []string{"-m", "/m.gguf", "--rpc", "${RPC_SERVERS}", "--device", "${DEVICES}"},
			want: clusterArgScan{
				rpcInArgs: true, rpcArgIndex: 2, rpcArgCount: 1, rpcValueHasPlaceholder: true, rpcPlaceholderCount: 1,
				deviceInArgs: true, deviceArgIndex: 4, deviceList: "${DEVICES}", deviceHasRPC: true, clusterPlaceholder: true,
			},
		},
		{
			name: "attached values and the short device flag",
			args: []string{"-dev", "CUDA0", "--rpc=${RPC_SERVERS}", "--tensor_split=1,1", "-mmdev", "CUDA0"},
			want: clusterArgScan{
				rpcInArgs: true, rpcArgIndex: 2, rpcArgCount: 1, rpcValueHasPlaceholder: true, rpcPlaceholderCount: 1,
				deviceInArgs: true, deviceArgIndex: 0, deviceList: "CUDA0", tensorSplit: true, mmdev: true, clusterPlaceholder: true,
				equalsForm: true,
			},
		},
		{
			name: "env rpc and env device",
			env:  map[string]string{"LLAMA_ARG_RPC": "${RPC_SERVERS}", "LLAMA_ARG_DEVICE": "${RPC_DEVICES},CUDA0", "LLAMA_ARG_TENSOR_SPLIT": "1,1", "MTMD_BACKEND_DEVICE": "CUDA0"},
			want: clusterArgScan{
				rpcInEnv: true, rpcArgIndex: -1, rpcValueHasPlaceholder: true, rpcPlaceholderCount: 1,
				deviceInEnv: true, deviceArgIndex: -1, deviceList: "${RPC_DEVICES},CUDA0", deviceHasRPC: true, tensorSplit: true, mmdev: true, clusterPlaceholder: true,
			},
		},
		{
			name: "args device list wins over env",
			args: []string{"--device", "CUDA0"},
			env:  map[string]string{"LLAMA_ARG_DEVICE": "${DEVICES}"},
			want: clusterArgScan{rpcArgIndex: -1, deviceInArgs: true, deviceInEnv: true, deviceArgIndex: 0, deviceList: "CUDA0"},
		},
		{
			name: "placeholder outside an rpc value",
			args: []string{"--alias", "${RPC_SERVERS}", "--rpc", "10.0.0.1:50052", "--rpc", "x"},
			want: clusterArgScan{rpcInArgs: true, rpcArgIndex: 2, rpcArgCount: 2, rpcPlaceholderCount: 1, deviceArgIndex: -1, clusterPlaceholder: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scanClusterArgs(tc.args, tc.env); got != tc.want {
				t.Fatalf("scan =\n %+v\nwant\n %+v", got, tc.want)
			}
		})
	}
}

func TestDeviceListStartsWithRPC(t *testing.T) {
	for _, tc := range []struct {
		list        string
		headHasGPUs bool
		want        bool
	}{
		{"${DEVICES}", true, true},
		{"${RPC_DEVICES},CUDA0", true, true},
		{"RPC0,RPC1,CUDA0", true, true},
		{"CUDA0,${RPC_DEVICES}", true, false},
		{"${FIRST_LOCAL_DEVICE},${RPC_DEVICES}", true, false},
		// Without GPU rows ${FIRST_LOCAL_DEVICE} falls back to RPC0.
		{"${FIRST_LOCAL_DEVICE},${RPC_DEVICES}", false, true},
	} {
		if got := (clusterArgScan{deviceList: tc.list}).deviceListStartsWithRPC(tc.headHasGPUs); got != tc.want {
			t.Errorf("deviceListStartsWithRPC(%q, gpus=%v) = %v, want %v", tc.list, tc.headHasGPUs, got, tc.want)
		}
	}
}

func TestScanClusterArgsStepsOverValues(t *testing.T) {
	// "-dev" is the value of --alias, "--rpc" the value of --api-key: neither
	// is an option.
	scan := scanClusterArgs([]string{"--alias", "-dev", "--api-key", "--rpc", "-m", "/m.gguf", "--rpc", "${RPC_SERVERS}"}, nil)
	if scan.deviceInArgs || scan.rpcArgCount != 1 || scan.rpcArgIndex != 6 || !scan.rpcValueHasPlaceholder {
		t.Fatalf("scan = %+v", scan)
	}
	// A boolean option takes no value: the next element is an option again.
	scan = scanClusterArgs([]string{"--jinja", "-dev", "${DEVICES}"}, nil)
	if !scan.deviceInArgs || scan.deviceList != "${DEVICES}" {
		t.Fatalf("scan after a boolean option = %+v", scan)
	}
}

func TestScanClusterArgsEqualsForm(t *testing.T) {
	for _, args := range [][]string{
		{"--rpc=${RPC_SERVERS}"}, {"--device=${DEVICES}"}, {"-dev=${DEVICES}"}, {"--tensor-split=1,1"}, {"-mmdev=CUDA0"},
	} {
		if !scanClusterArgs(args, nil).equalsForm {
			t.Errorf("%v: equalsForm = false", args)
		}
	}
	if scanClusterArgs([]string{"--alias=x", "--rpc", "${RPC_SERVERS}"}, nil).equalsForm {
		t.Error("an unscanned option's = form is not the head's concern")
	}
}

func TestLlamaServerValueFlags(t *testing.T) {
	for flag, want := range map[string]int{"--rpc": 1, "--device": 1, "-dev": 1, "-ts": 1, "-mmdev": 1, "--alias": 1, "-m": 1, "--jinja": 0, "-h": 0} {
		if got := llamaServerValueFlags[flag]; got != want {
			t.Errorf("llamaServerValueFlags[%q] = %d, want %d", flag, got, want)
		}
	}
}
