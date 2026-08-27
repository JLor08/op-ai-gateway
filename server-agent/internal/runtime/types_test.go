// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// capturedRuntimeConfigJSON is the exact wire document captured from a live
// Service.AgentRuntimeConfig call and recorded field-for-field in
// .superpowers/sdd/2026-08-25-agent-runtime-manager/task-7-report.md (spec
// §11). ParseConfig must accept it byte-for-byte.
const capturedRuntimeConfigJSON = `{
  "router_listen": 9000,
  "max_processes": 3,
  "gpu_budgets": [
    { "index": 0, "budget_mb": 46000 },
    { "index": 1, "budget_mb": 46000 }
  ],
  "specs": [
    {
      "id": "rspec_20f463074a75d7f5d5d4b190233bc8ae",
      "model": "qwen-coder",
      "upstream_model": "qwen2.5-coder-32b",
      "binary": "/usr/bin/vllm",
      "args": ["--tensor-parallel-size", "2"],
      "env": { "HF_TOKEN": "${AGENT_ENV:HF_TOKEN}" },
      "gpus": [
        { "index": 0, "vram_mb": 22500 },
        { "index": 1, "vram_mb": 21500 }
      ],
      "listen_port": 0,
      "health_path": "/health",
      "health_timeout_seconds": 5,
      "startup_timeout_seconds": 180,
      "idle_timeout_seconds": 0,
      "admission_wait_timeout_seconds": 0,
      "pinned": false,
      "admin_state": ""
    },
    {
      "id": "rspec_92c817a691e071648b30fc449fa87c24",
      "model": "llama-small",
      "upstream_model": "llama-3-8b",
      "binary": "/usr/bin/llama-server",
      "args": [],
      "env": {},
      "gpus": [
        { "index": 0, "vram_mb": 8000 }
      ],
      "listen_port": 0,
      "health_path": "/health",
      "health_timeout_seconds": 5,
      "startup_timeout_seconds": 180,
      "idle_timeout_seconds": 900,
      "admission_wait_timeout_seconds": 0,
      "pinned": false,
      "admin_state": ""
    }
  ],
  "coresident": [
    ["rspec_20f463074a75d7f5d5d4b190233bc8ae", "rspec_92c817a691e071648b30fc449fa87c24"]
  ],
  "etag": "5fbae985e19187ce4f7557d62be7181d7b45c6d6047b736efb75feec5322e328"
}`

// TestParseConfigRoundTrip asserts the captured spec §11 example parses into
// exactly the fields the wire document names, and that an unknown top-level
// field is ignored rather than rejected (forward compatibility with a newer
// gateway).
func TestParseConfigRoundTrip(t *testing.T) {
	cfg, err := ParseConfig([]byte(capturedRuntimeConfigJSON))
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}

	if cfg.RouterListen != 9000 {
		t.Errorf("RouterListen = %d, want 9000", cfg.RouterListen)
	}
	if cfg.MaxProcesses != 3 {
		t.Errorf("MaxProcesses = %d, want 3", cfg.MaxProcesses)
	}
	if len(cfg.GPUBudgets) != 2 {
		t.Fatalf("len(GPUBudgets) = %d, want 2", len(cfg.GPUBudgets))
	}
	if cfg.GPUBudgets[0] != (GPUBudget{Index: 0, BudgetMB: 46000}) {
		t.Errorf("GPUBudgets[0] = %+v", cfg.GPUBudgets[0])
	}
	if cfg.GPUBudgets[1] != (GPUBudget{Index: 1, BudgetMB: 46000}) {
		t.Errorf("GPUBudgets[1] = %+v", cfg.GPUBudgets[1])
	}

	if len(cfg.Specs) != 2 {
		t.Fatalf("len(Specs) = %d, want 2", len(cfg.Specs))
	}
	s0 := cfg.Specs[0]
	if s0.ID != "rspec_20f463074a75d7f5d5d4b190233bc8ae" ||
		s0.Model != "qwen-coder" ||
		s0.UpstreamModel != "qwen2.5-coder-32b" ||
		s0.Binary != "/usr/bin/vllm" ||
		s0.HealthPath != "/health" ||
		s0.HealthTimeoutSeconds != 5 ||
		s0.StartupTimeoutSeconds != 180 ||
		s0.IdleTimeoutSeconds != 0 ||
		s0.AdmissionWaitTimeoutSeconds != 0 ||
		s0.Pinned != false ||
		s0.AdminState != "" ||
		s0.ListenPort != 0 {
		t.Errorf("Specs[0] scalar fields = %+v", s0)
	}
	if len(s0.Args) != 2 || s0.Args[0] != "--tensor-parallel-size" || s0.Args[1] != "2" {
		t.Errorf("Specs[0].Args = %v", s0.Args)
	}
	if s0.Env["HF_TOKEN"] != "${AGENT_ENV:HF_TOKEN}" || len(s0.Env) != 1 {
		t.Errorf("Specs[0].Env = %v", s0.Env)
	}
	if len(s0.GPUs) != 2 ||
		s0.GPUs[0] != (SpecGPU{Index: 0, VRAMMB: 22500}) ||
		s0.GPUs[1] != (SpecGPU{Index: 1, VRAMMB: 21500}) {
		t.Errorf("Specs[0].GPUs = %+v", s0.GPUs)
	}

	s1 := cfg.Specs[1]
	if s1.ID != "rspec_92c817a691e071648b30fc449fa87c24" ||
		s1.Model != "llama-small" ||
		s1.UpstreamModel != "llama-3-8b" ||
		s1.Binary != "/usr/bin/llama-server" ||
		s1.IdleTimeoutSeconds != 900 {
		t.Errorf("Specs[1] scalar fields = %+v", s1)
	}
	if len(s1.Args) != 0 {
		t.Errorf("Specs[1].Args = %v, want empty (non-nil)", s1.Args)
	}
	if s1.Args == nil {
		t.Error("Specs[1].Args must be non-nil")
	}
	if len(s1.Env) != 0 {
		t.Errorf("Specs[1].Env = %v, want empty", s1.Env)
	}
	if s1.Env == nil {
		t.Error("Specs[1].Env must be non-nil")
	}
	if len(s1.GPUs) != 1 || s1.GPUs[0] != (SpecGPU{Index: 0, VRAMMB: 8000}) {
		t.Errorf("Specs[1].GPUs = %+v", s1.GPUs)
	}

	if len(cfg.Coresident) != 1 {
		t.Fatalf("len(Coresident) = %d, want 1", len(cfg.Coresident))
	}
	want := [2]string{"rspec_20f463074a75d7f5d5d4b190233bc8ae", "rspec_92c817a691e071648b30fc449fa87c24"}
	if cfg.Coresident[0] != want {
		t.Errorf("Coresident[0] = %v, want %v", cfg.Coresident[0], want)
	}

	if cfg.ETag != "5fbae985e19187ce4f7557d62be7181d7b45c6d6047b736efb75feec5322e328" {
		t.Errorf("ETag = %q", cfg.ETag)
	}

	// Unknown top-level fields must be tolerated, not rejected.
	var withExtra map[string]json.RawMessage
	if err := json.Unmarshal([]byte(capturedRuntimeConfigJSON), &withExtra); err != nil {
		t.Fatal(err)
	}
	withExtra["a_field_from_the_future"] = json.RawMessage(`"surprise"`)
	extraRaw, err := json.Marshal(withExtra)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseConfig(extraRaw); err != nil {
		t.Errorf("ParseConfig() with an unknown top-level field returned an error: %v", err)
	}
}

// TestParseConfigEmptyDocumentCollectionsAreNeverNil covers the
// fully-empty document a server with no server_agent application yet
// produces (task-7-report.md): every collection must still marshal as `[]`,
// never `null`, so an agent-side consumer never has to nil-check.
func TestParseConfigEmptyDocumentCollectionsAreNeverNil(t *testing.T) {
	const empty = `{"router_listen":0,"max_processes":0,"gpu_budgets":[],"specs":[],"coresident":[],"etag":"deadbeef"}`
	cfg, err := ParseConfig([]byte(empty))
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	if cfg.GPUBudgets == nil {
		t.Error("GPUBudgets must be non-nil")
	}
	if cfg.Specs == nil {
		t.Error("Specs must be non-nil")
	}
	if cfg.Coresident == nil {
		t.Error("Coresident must be non-nil")
	}
}

// TestParseConfigDropsUnknownPairSpecs asserts a coresident pair naming a
// spec ID that does not appear (or no longer appears) in Specs is dropped
// silently, and the rest of the config -- including the other, valid pairs
// -- remains usable rather than the whole document being rejected.
func TestParseConfigDropsUnknownPairSpecs(t *testing.T) {
	const raw = `{
		"router_listen": 8081,
		"max_processes": 2,
		"gpu_budgets": [],
		"specs": [
			{"id": "spec-a", "model": "m-a", "upstream_model": "u-a", "binary": "/bin/a",
			 "args": [], "env": {}, "gpus": [], "listen_port": 0, "health_path": "/health",
			 "health_timeout_seconds": 5, "startup_timeout_seconds": 60, "idle_timeout_seconds": 0,
			 "admission_wait_timeout_seconds": 0, "pinned": false, "admin_state": ""},
			{"id": "spec-b", "model": "m-b", "upstream_model": "u-b", "binary": "/bin/b",
			 "args": [], "env": {}, "gpus": [], "listen_port": 0, "health_path": "/health",
			 "health_timeout_seconds": 5, "startup_timeout_seconds": 60, "idle_timeout_seconds": 0,
			 "admission_wait_timeout_seconds": 0, "pinned": false, "admin_state": ""}
		],
		"coresident": [["spec-a", "spec-b"], ["spec-a", "spec-ghost"]],
		"etag": "abc"
	}`
	cfg, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	if len(cfg.Specs) != 2 {
		t.Fatalf("len(Specs) = %d, want 2 (valid specs must survive)", len(cfg.Specs))
	}
	if len(cfg.Coresident) != 1 {
		t.Fatalf("len(Coresident) = %d, want 1 (the ghost pair must be dropped)", len(cfg.Coresident))
	}
	want := [2]string{"spec-a", "spec-b"}
	if cfg.Coresident[0] != want {
		t.Errorf("Coresident[0] = %v, want %v", cfg.Coresident[0], want)
	}
}

// TestParseConfigRejectsDuplicateSpecIDs asserts a document with the same
// spec ID appearing twice is structural nonsense the gateway must never
// produce, and is rejected as an error rather than silently picking one.
func TestParseConfigRejectsDuplicateSpecIDs(t *testing.T) {
	const raw = `{
		"router_listen": 8081,
		"max_processes": 2,
		"gpu_budgets": [],
		"specs": [
			{"id": "dup", "model": "m-a", "upstream_model": "u-a", "binary": "/bin/a",
			 "args": [], "env": {}, "gpus": [], "listen_port": 0, "health_path": "/health",
			 "health_timeout_seconds": 5, "startup_timeout_seconds": 60, "idle_timeout_seconds": 0,
			 "admission_wait_timeout_seconds": 0, "pinned": false, "admin_state": ""},
			{"id": "dup", "model": "m-b", "upstream_model": "u-b", "binary": "/bin/b",
			 "args": [], "env": {}, "gpus": [], "listen_port": 0, "health_path": "/health",
			 "health_timeout_seconds": 5, "startup_timeout_seconds": 60, "idle_timeout_seconds": 0,
			 "admission_wait_timeout_seconds": 0, "pinned": false, "admin_state": ""}
		],
		"coresident": [],
		"etag": "abc"
	}`
	if _, err := ParseConfig([]byte(raw)); err == nil {
		t.Fatal("ParseConfig() with duplicate spec IDs: want error, got nil")
	}
}

// TestParseConfigRejectsMalformedJSON asserts a syntactically invalid
// document is a plain error, not a panic or a silently empty Config.
func TestParseConfigRejectsMalformedJSON(t *testing.T) {
	if _, err := ParseConfig([]byte(`{not json`)); err == nil {
		t.Fatal("ParseConfig() with malformed JSON: want error, got nil")
	}
}

// TestConfigAllowedPairsCanonicalizesReversedInput covers the review's
// Minor fix: Coresident is kept in wire order (whatever order the gateway
// happened to emit each pair in), while PolicySnapshot.Allowed is
// documented as a canonical PairKey set. A consumer that inserted the raw
// wire pairs directly would get a lookup that only works in the one
// direction the gateway happened to send -- AllowedPairs must canonicalize
// so that never happens, regardless of the input pair's own order.
func TestConfigAllowedPairsCanonicalizesReversedInput(t *testing.T) {
	cfg := Config{Coresident: [][2]string{{"b", "a"}}}
	allowed := cfg.AllowedPairs()

	if len(allowed) != 1 {
		t.Fatalf("len(AllowedPairs()) = %d, want 1", len(allowed))
	}
	if !allowed[PairKey("a", "b")] {
		t.Error("AllowedPairs() must canonicalize a reversed wire pair so PairKey(a, b) is found")
	}
	if !allowed[PairKey("b", "a")] {
		t.Error("PairKey itself must be order-independent on lookup too")
	}
}

// TestStatusMarshalJSONNeverEmitsNullMeasuredVRAM pins the Task 18 debt
// paydown: a Status with a nil MeasuredVRAM (the common case -- no
// measurer, or not yet running) must marshal that field as "{}", never as
// Go's default "null" for a nil map. A populated map must still round-trip
// its entries untouched.
func TestStatusMarshalJSONNeverEmitsNullMeasuredVRAM(t *testing.T) {
	nilCase := Status{SpecID: "s1", State: StateRunning}
	raw, err := json.Marshal(nilCase)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"measured_vram":{}`) {
		t.Errorf("raw = %s, want measured_vram:{} for a nil map", raw)
	}
	if strings.Contains(string(raw), "null") {
		t.Errorf("raw = %s, must never contain null", raw)
	}

	populated := Status{SpecID: "s2", State: StateRunning, MeasuredVRAM: map[int]int{0: 21234, 1: 8000}}
	raw2, err := json.Marshal(populated)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw2, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var vram map[string]int
	if err := json.Unmarshal(decoded["measured_vram"], &vram); err != nil {
		t.Fatalf("unmarshal measured_vram: %v", err)
	}
	if vram["0"] != 21234 || vram["1"] != 8000 || len(vram) != 2 {
		t.Errorf("measured_vram = %v, want {0:21234 1:8000}", vram)
	}
}

// TestStatusMarshalJSONAppliesToCollections is the receiver check B2 asked
// for: Status.MarshalJSON has a VALUE receiver, and this pins that the
// normalization therefore survives every placement Status can appear in --
// not just a bare Status.
//
// []Status is the one that matters in practice (Manager.Status() returns
// exactly that), but a slice alone cannot distinguish a value receiver from
// a pointer receiver: slice elements are addressable, so encoding/json's
// addrMarshalerEncoder would call a *Status method too. The map case is what
// actually discriminates -- a map VALUE is not addressable, so a pointer
// receiver is skipped entirely there and measured_vram silently regresses to
// "null", the exact nil-vs-null defect this MarshalJSON exists to prevent.
// Verified by probe: with a pointer receiver, map[string]T marshals the nil
// map as null while []T still normalizes correctly.
func TestStatusMarshalJSONAppliesToCollections(t *testing.T) {
	// Manager.Status()'s own shape.
	rawSlice, err := json.Marshal([]Status{
		{SpecID: "s1", State: StateRunning},
		{SpecID: "s2", State: StateStopped, MeasuredVRAM: map[int]int{0: 4096}},
	})
	if err != nil {
		t.Fatalf("marshal []Status: %v", err)
	}
	if strings.Contains(string(rawSlice), "null") {
		t.Errorf("[]Status marshalled to %s; the value receiver must normalize every element, so null must never appear", rawSlice)
	}
	if strings.Count(string(rawSlice), `"measured_vram"`) != 2 {
		t.Errorf("[]Status marshalled to %s, want measured_vram on both elements", rawSlice)
	}

	// A non-addressable placement: this is where a pointer receiver would
	// have been skipped and emitted null.
	rawMap, err := json.Marshal(map[string]Status{"s1": {SpecID: "s1", State: StateRunning}})
	if err != nil {
		t.Fatalf("marshal map[string]Status: %v", err)
	}
	if !strings.Contains(string(rawMap), `"measured_vram":{}`) {
		t.Errorf("map[string]Status marshalled to %s, want measured_vram:{} -- a pointer receiver would be skipped for a non-addressable map value and emit null", rawMap)
	}
	if strings.Contains(string(rawMap), "null") {
		t.Errorf("map[string]Status marshalled to %s; null must never appear", rawMap)
	}
}

// TestStatusAndLastErrorJSONTags pins the exact wire tags Status/LastError
// carry now that Task 18 puts them on the wire (via
// sample.RuntimeSample/RuntimeErrorSample, built field-by-field from these
// types in internal/agent.collectOnce).
func TestStatusAndLastErrorJSONTags(t *testing.T) {
	since := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	failedAt := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	st := Status{
		SpecID:   "rspec_a",
		Model:    "qwen-coder",
		State:    StateCrashed,
		Since:    since,
		PID:      111,
		Port:     9001,
		InFlight: 2,
		Restarts: 1,
		LastError: &LastError{
			Message:    "boom",
			At:         failedAt,
			ExitCode:   1,
			Failures:   3,
			StderrTail: "oom",
		},
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		`"spec_id":"rspec_a"`, `"model":"qwen-coder"`, `"state":"crashed"`,
		`"pid":111`, `"port":9001`, `"in_flight":2`, `"restarts":1`,
	} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("raw = %s, missing %s", raw, key)
		}
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var le map[string]json.RawMessage
	if err := json.Unmarshal(decoded["last_error"], &le); err != nil {
		t.Fatalf("unmarshal last_error: %v", err)
	}
	for _, key := range []string{"message", "at", "exit_code", "failures", "stderr_tail"} {
		if _, ok := le[key]; !ok {
			t.Errorf("last_error missing key %q in %s", key, decoded["last_error"])
		}
	}
}
