// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"testing"
)

// TestPutRuntimeSpecExecutionValidation pins the execution_mode / wsl_distro
// rules of a spec write: an omitted mode stores native, a native spec stores no
// distro whatever the request carried, and a wsl spec needs a plain distro, a
// Linux binary and work_dir, and cannot be a cluster head. A valid wsl spec
// round-trips through GET.
func TestPutRuntimeSpecExecutionValidation(t *testing.T) {
	ctx := context.Background()
	svc, _, mappingID, _ := instanceSpecFixture(t, nil, nil)
	cases := []struct {
		name       string
		mutate     func(*PutRuntimeSpecRequest)
		wantErr    error
		wantMode   string
		wantDistro string
	}{
		{"omitted mode stores native", func(r *PutRuntimeSpecRequest) {}, nil, "native", ""},
		{"explicit native", func(r *PutRuntimeSpecRequest) { r.ExecutionMode = "native" }, nil, "native", ""},
		{"unknown mode", func(r *PutRuntimeSpecRequest) { r.ExecutionMode = "docker" }, ErrRuntimeSpecExecutionModeInvalid, "", ""},
		{"wsl without a distro", func(r *PutRuntimeSpecRequest) { r.ExecutionMode = "wsl" }, ErrRuntimeSpecWSLDistroInvalid, "", ""},
		{"wsl with a spaced distro", func(r *PutRuntimeSpecRequest) { r.ExecutionMode, r.WSLDistro = "wsl", "a b" }, ErrRuntimeSpecWSLDistroInvalid, "", ""},
		{"wsl with an option-like distro", func(r *PutRuntimeSpecRequest) { r.ExecutionMode, r.WSLDistro = "wsl", "-d" }, ErrRuntimeSpecWSLDistroInvalid, "", ""},
		{"wsl with a leading-dash distro", func(r *PutRuntimeSpecRequest) { r.ExecutionMode, r.WSLDistro = "wsl", "-Ubuntu" }, ErrRuntimeSpecWSLDistroInvalid, "", ""},
		{"wsl with a 64 char distro", func(r *PutRuntimeSpecRequest) {
			r.ExecutionMode, r.WSLDistro = "wsl", "a123456789a123456789a123456789a123456789a123456789a123456789a123"
		}, nil, "wsl", "a123456789a123456789a123456789a123456789a123456789a123456789a123"},
		{"wsl with a 65 char distro", func(r *PutRuntimeSpecRequest) {
			r.ExecutionMode, r.WSLDistro = "wsl", "a123456789a123456789a123456789a123456789a123456789a123456789a1234"
		}, ErrRuntimeSpecWSLDistroInvalid, "", ""},
		{"wsl with a windows binary", func(r *PutRuntimeSpecRequest) {
			r.ExecutionMode, r.WSLDistro, r.Binary = "wsl", "Ubuntu-24.04", `C:\x\vllm.exe`
		}, ErrRuntimeSpecWSLPathNotLinux, "", ""},
		{"wsl with a relative work_dir", func(r *PutRuntimeSpecRequest) {
			r.ExecutionMode, r.WSLDistro, r.WorkDir = "wsl", "Ubuntu-24.04", "relative"
		}, ErrRuntimeSpecWSLPathNotLinux, "", ""},
		{"wsl cluster head", func(r *PutRuntimeSpecRequest) {
			r.ExecutionMode, r.WSLDistro, r.ClusterEnabled = "wsl", "Ubuntu-24.04", true
		}, ErrRuntimeSpecWSLClusterUnsupported, "", ""},
		{"native drops a stray distro", func(r *PutRuntimeSpecRequest) { r.WSLDistro = "Ubuntu-24.04" }, nil, "native", ""},
		{"valid wsl", func(r *PutRuntimeSpecRequest) {
			r.ExecutionMode, r.WSLDistro, r.WorkDir = "wsl", "Ubuntu-24.04", "/home/op/work"
		}, nil, "wsl", "Ubuntu-24.04"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := PutRuntimeSpecRequest{Enabled: true, Binary: "/usr/local/bin/llama-server"}
			tc.mutate(&req)
			put, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, req)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("PutRuntimeSpec: %v", err)
			}
			if put.ExecutionMode != tc.wantMode || put.WSLDistro != tc.wantDistro {
				t.Fatalf("put echo = %q/%q, want %q/%q", put.ExecutionMode, put.WSLDistro, tc.wantMode, tc.wantDistro)
			}
			got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mappingID)
			if err != nil {
				t.Fatalf("GetRuntimeSpec: %v", err)
			}
			if got.ExecutionMode != tc.wantMode || got.WSLDistro != tc.wantDistro {
				t.Fatalf("GET = %q/%q, want %q/%q", got.ExecutionMode, got.WSLDistro, tc.wantMode, tc.wantDistro)
			}
			if r := putRequestFromDTO(got); r.ExecutionMode != tc.wantMode || r.WSLDistro != tc.wantDistro {
				t.Fatalf("putRequestFromDTO dropped the execution pair: %q/%q", r.ExecutionMode, r.WSLDistro)
			}
		})
	}
	unconfigured, err := svc.GetRuntimeSpec(ctx, ownerToken(), mustSecondMapping(t, svc, mappingID))
	if err != nil || unconfigured.ExecutionMode != "native" {
		t.Fatalf("an unconfigured mapping reads execution_mode %q (err %v), want native", unconfigured.ExecutionMode, err)
	}
}

// mustSecondMapping creates another mapping on the same application as
// mappingID and returns its id.
func mustSecondMapping(t *testing.T, svc *Service, mappingID string) string {
	t.Helper()
	m, _, _, err := svc.authorizeMapping(context.Background(), ownerToken(), mappingID)
	if err != nil {
		t.Fatalf("authorizeMapping: %v", err)
	}
	created, err := svc.CreateMapping(context.Background(), ownerToken(), m.ApplicationID, CreateMappingRequest{GatewayModelName: "second", AppModelName: "second-upstream"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	return created.ID
}

// TestAgentRuntimeDocWSLSpecOnlyForAnAgentThatDeclaresIt pins which agents get a
// wsl spec: only one that declares runtime_exec_wsl. One that is known without
// it gets the spec withheld (an older agent would ignore the execution block
// and run the Linux binary natively), with co-residency pairs naming it
// dropped; one not heard from yet holds the document; a reader-less service
// pushes the spec as stored.
func TestAgentRuntimeDocWSLSpecOnlyForAnAgentThatDeclaresIt(t *testing.T) {
	ctx := context.Background()
	svc, _, mappingID, serverID := instanceSpecFixture(t, nil, nil)
	wslSpec, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/bin/llama-server", ExecutionMode: "wsl", WSLDistro: "Ubuntu-24.04",
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec wsl: %v", err)
	}
	nativeMapping := mustSecondMapping(t, svc, mappingID)
	nativeSpec, err := svc.PutRuntimeSpec(ctx, ownerToken(), nativeMapping, PutRuntimeSpecRequest{Enabled: true, Binary: "/usr/bin/llama-server"})
	if err != nil {
		t.Fatalf("PutRuntimeSpec native: %v", err)
	}
	cases := []struct {
		name     string
		features AgentFeatureReader
		wsl      bool // the wsl spec is in the document
		held     bool
	}{
		{"an agent that declares it", fakeAgentFeatures{serverID: {"runtime_manager", AgentFeatureRuntimeExecWSL}}, true, false},
		{"a known agent without it", fakeAgentFeatures{serverID: {"runtime_manager"}}, false, false},
		{"an agent not heard from yet", fakeAgentFeatures{"srv_other": nil}, true, true},
		{"no feature reader", nil, true, false},
	}
	for _, tc := range cases {
		svc.agentFeatures = tc.features
		doc, err := svc.AgentRuntimeConfig(ctx, serverID)
		if err != nil {
			t.Fatalf("%s: AgentRuntimeConfig: %v", tc.name, err)
		}
		if doc.AwaitsAgentFeatures != tc.held {
			t.Errorf("%s: AwaitsAgentFeatures = %v, want %v", tc.name, doc.AwaitsAgentFeatures, tc.held)
		}
		var gotWSL *AgentRuntimeSpecDTO
		sawNative := false
		for i := range doc.Specs {
			switch doc.Specs[i].ID {
			case wslSpec.ID:
				gotWSL = &doc.Specs[i]
			case nativeSpec.ID:
				sawNative = true
			}
		}
		if !sawNative {
			t.Errorf("%s: the native spec is missing from the document", tc.name)
		}
		if (gotWSL != nil) != tc.wsl {
			t.Errorf("%s: wsl spec present = %v, want %v", tc.name, gotWSL != nil, tc.wsl)
			continue
		}
		if gotWSL != nil {
			if gotWSL.Execution == nil || gotWSL.Execution.Mode != "wsl" || gotWSL.Execution.WSL == nil || gotWSL.Execution.WSL.Distro != "Ubuntu-24.04" {
				t.Errorf("%s: execution = %+v, want {wsl, Ubuntu-24.04}", tc.name, gotWSL.Execution)
			}
			keys := wireKeys(t, *gotWSL)
			if string(keys["execution"]) != `{"mode":"wsl","wsl":{"distro":"Ubuntu-24.04"}}` {
				t.Errorf("%s: wire execution = %s", tc.name, keys["execution"])
			}
		}
		pairs := 0
		for _, p := range doc.Coresident {
			if p[0] == wslSpec.ID || p[1] == wslSpec.ID {
				pairs++
			}
		}
		if (pairs > 0) != tc.wsl {
			t.Errorf("%s: coresident pairs naming the wsl spec = %d, want present=%v", tc.name, pairs, tc.wsl)
		}
	}
}

// TestAgentRuntimeDocNativeSpecHasNoExecutionKey pins that a native spec's
// document is the one it was before the wsl mode existed: no execution key.
func TestAgentRuntimeDocNativeSpecHasNoExecutionKey(t *testing.T) {
	ctx := context.Background()
	svc, _, mappingID, serverID := instanceSpecFixture(t, nil, nil)
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{Enabled: true, Binary: "/usr/bin/llama-server"}); err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	doc, err := svc.AgentRuntimeConfig(ctx, serverID)
	if err != nil || len(doc.Specs) != 1 {
		t.Fatalf("AgentRuntimeConfig: %d specs, err %v", len(doc.Specs), err)
	}
	if _, present := wireKeys(t, doc.Specs[0])["execution"]; present {
		t.Fatal("a native spec carries an execution key")
	}
}
