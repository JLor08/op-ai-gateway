// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"slices"
	"testing"
	"time"
)

// processTreeStrataBinary is a Windows binary path, so a spec on a reported
// Windows agent raises no binary_path_os_mismatch advisory of its own.
const processTreeStrataBinary = `C:\Strata\bin\strata.cmd`

// reportAgentOS stores a telemetry row for serverID with the given GOOS, the
// state an agent that has reported at least once leaves behind.
func reportAgentOS(t *testing.T, routeStore *routing.MemoryStore, serverID, goos string) {
	t.Helper()
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	if err := routeStore.UpsertTelemetry(context.Background(), routing.ServerTelemetry{
		ServerID: serverID, ReportedAt: now, AgentVersion: "0.21.0", OS: goos,
		Arch: "amd64", ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
}

// TestPutRuntimeSpecProcessTreeValidation pins the process_tree rules of a
// spec write on an agent that supports job mode: an omitted value stores
// direct, the closed set is accepted, anything else is refused with
// ErrRuntimeSpecProcessTreeInvalid, and a stored value round-trips through GET
// and through putRequestFromDTO (the benchmark writers' re-save), so a
// benchmark run never resets a job spec to direct.
func TestPutRuntimeSpecProcessTreeValidation(t *testing.T) {
	ctx := context.Background()
	svc, routeStore, mappingID, serverID := instanceSpecFixture(t, nil, nil)
	reportAgentOS(t, routeStore, serverID, "windows")
	svc.agentFeatures = fakeAgentFeatures{serverID: {"runtime_manager", AgentFeatureRuntimeProcessTreeJob}}
	cases := []struct {
		name    string
		value   string
		wantErr error
		want    string
	}{
		{"omitted stores direct", "", nil, "direct"},
		{"explicit direct", "direct", nil, "direct"},
		{"job", "job", nil, "job"},
		{"padded job", " job ", nil, "job"},
		{"bogus", "bogus", ErrRuntimeSpecProcessTreeInvalid, ""},
		{"wrong case", "JOB", ErrRuntimeSpecProcessTreeInvalid, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := PutRuntimeSpecRequest{Enabled: true, Binary: processTreeStrataBinary, ProcessTree: tc.value}
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
			if put.ProcessTree != tc.want {
				t.Fatalf("put echo = %q, want %q", put.ProcessTree, tc.want)
			}
			got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mappingID)
			if err != nil {
				t.Fatalf("GetRuntimeSpec: %v", err)
			}
			if got.ProcessTree != tc.want {
				t.Fatalf("GET = %q, want %q", got.ProcessTree, tc.want)
			}
			if r := putRequestFromDTO(got); r.ProcessTree != tc.want {
				t.Fatalf("putRequestFromDTO dropped process_tree: %q", r.ProcessTree)
			}
		})
	}
	unconfigured, err := svc.GetRuntimeSpec(ctx, ownerToken(), mustSecondMapping(t, svc, mappingID))
	if err != nil || unconfigured.ProcessTree != "direct" {
		t.Fatalf("an unconfigured mapping reads process_tree %q (err %v), want direct", unconfigured.ProcessTree, err)
	}
}

// TestPutRuntimeSpecProcessTreeAgentNegotiation pins when an operator's job
// write is refused: whenever the gateway KNOWS the spec's agent cannot honour
// it -- its reported OS is not Windows (the flag is declared on every OS, so
// the flag alone proves nothing), or it has declared its features without
// runtime_process_tree_job. An agent that has not reported its OS or its
// features yet cannot be judged: the save succeeds and RuntimeWarnings carries
// process_tree_agent_unknown. direct is never refused, and a refused body
// that is also invalid for an older reason keeps the older answer.
func TestPutRuntimeSpecProcessTreeAgentNegotiation(t *testing.T) {
	flagged := []string{"runtime_manager", AgentFeatureRuntimeProcessTreeJob}
	unflagged := []string{"runtime_manager"}
	cases := []struct {
		name        string
		os          string   // "" = no telemetry row
		features    []string // nil = the agent has not declared its features
		value       string
		wantErr     error
		wantWarning bool
	}{
		{"linux agent with the flag", "linux", flagged, "job", ErrRuntimeSpecProcessTreeUnsupported, false},
		{"darwin agent with the flag", "darwin", flagged, "job", ErrRuntimeSpecProcessTreeUnsupported, false},
		{"linux agent, features not declared yet", "linux", nil, "job", ErrRuntimeSpecProcessTreeUnsupported, false},
		{"windows agent without the flag", "windows", unflagged, "job", ErrRuntimeSpecProcessTreeUnsupported, false},
		{"no telemetry, features without the flag", "", unflagged, "job", ErrRuntimeSpecProcessTreeUnsupported, false},
		{"windows agent with the flag", "windows", flagged, "job", nil, false},
		{"agent never reported", "", nil, "job", nil, true},
		{"windows agent, features not declared yet", "windows", nil, "job", nil, true},
		{"no telemetry, features with the flag", "", flagged, "job", nil, true},
		{"direct on a linux agent", "linux", unflagged, "direct", nil, false},
		{"direct on an agent never reported", "", nil, "", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc, routeStore, mappingID, serverID := instanceSpecFixture(t, nil, nil)
			if tc.os != "" {
				reportAgentOS(t, routeStore, serverID, tc.os)
			}
			if tc.features == nil {
				svc.agentFeatures = fakeAgentFeatures{"srv_other": nil}
			} else {
				svc.agentFeatures = fakeAgentFeatures{serverID: tc.features}
			}
			_, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
				Enabled: true, Binary: processTreeStrataBinary, ProcessTree: tc.value,
			})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if _, ok, _ := routeStore.RuntimeSpecByMapping(ctx, mappingID); ok {
					t.Fatal("a refused write stored a spec")
				}
				return
			}
			if err != nil {
				t.Fatalf("PutRuntimeSpec: %v", err)
			}
			_, app, _, err := svc.authorizeMapping(ctx, ownerToken(), mappingID)
			if err != nil {
				t.Fatalf("authorizeMapping: %v", err)
			}
			warnings, err := svc.RuntimeWarnings(ctx, ownerToken(), app.ID)
			if err != nil {
				t.Fatalf("RuntimeWarnings: %v", err)
			}
			if got := slices.Contains(warnings, "process_tree_agent_unknown"); got != tc.wantWarning {
				t.Fatalf("warnings = %#v, process_tree_agent_unknown present = %v, want %v", warnings, got, tc.wantWarning)
			}
		})
	}

	t.Run("an older refusal keeps its answer", func(t *testing.T) {
		ctx := context.Background()
		svc, routeStore, mappingID, serverID := instanceSpecFixture(t, nil, nil)
		reportAgentOS(t, routeStore, serverID, "linux")
		_, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{Enabled: true, Binary: "", ProcessTree: "job"})
		if !errors.Is(err, ErrRuntimeSpecBinaryRequired) {
			t.Fatalf("err = %v, want %v", err, ErrRuntimeSpecBinaryRequired)
		}
	})
}

// TestAgentRuntimeDocProcessTree pins the push: a job spec carries
// "process_tree":"job" in the agent document, a direct spec carries no key at
// all (so its document, ETag included, is the one it was before the field
// existed and an agent reads an absent key as direct), and switching a spec
// between the two changes the document's ETag -- the change reaches the agent,
// whose launch-shape comparison treats the field as launch-affecting and
// relaunches a running child.
func TestAgentRuntimeDocProcessTree(t *testing.T) {
	ctx := context.Background()
	svc, routeStore, mappingID, serverID := instanceSpecFixture(t, nil, nil)
	reportAgentOS(t, routeStore, serverID, "windows")
	svc.agentFeatures = fakeAgentFeatures{serverID: {"runtime_manager", AgentFeatureRuntimeInstances, AgentFeatureRuntimeProcessTreeJob}}

	put := func(value string) AgentRuntimeConfigDTO {
		t.Helper()
		if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
			Enabled: true, Binary: processTreeStrataBinary, ProcessTree: value,
		}); err != nil {
			t.Fatalf("PutRuntimeSpec %q: %v", value, err)
		}
		doc, err := svc.AgentRuntimeConfig(ctx, serverID)
		if err != nil || len(doc.Specs) != 1 {
			t.Fatalf("AgentRuntimeConfig: %d specs, err %v", len(doc.Specs), err)
		}
		return doc
	}

	direct := put("direct")
	if direct.Specs[0].ProcessTree != "" {
		t.Fatalf("a direct spec pushes process_tree %q, want none", direct.Specs[0].ProcessTree)
	}
	if _, present := wireKeys(t, direct.Specs[0])["process_tree"]; present {
		t.Fatal("a direct spec carries a process_tree key")
	}

	job := put("job")
	if job.Specs[0].ProcessTree != "job" {
		t.Fatalf("a job spec pushes process_tree %q, want job", job.Specs[0].ProcessTree)
	}
	if got := string(wireKeys(t, job.Specs[0])["process_tree"]); got != `"job"` {
		t.Fatalf("wire process_tree = %s, want \"job\"", got)
	}
	if job.ETag == direct.ETag {
		t.Fatal("switching direct -> job left the document's ETag unchanged; the agent would not relaunch")
	}
	if again := put("direct"); again.ETag != direct.ETag {
		t.Fatal("switching back to direct did not restore the original document")
	}
}
