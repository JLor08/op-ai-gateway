// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package sample

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// wireSample mirrors the gateway's agentTelemetryRequest field tags exactly
// (copied from the Locked shared contract) so the test proves the agent's
// Sample marshals into a payload the gateway decodes byte-for-byte.
type wireSample struct {
	ReportedAt     time.Time       `json:"reported_at"`
	AgentVersion   string          `json:"agent_version"`
	OS             string          `json:"os"`
	Arch           string          `json:"arch"`
	ActiveRequests int             `json:"active_requests"`
	QueueDepth     int             `json:"queue_depth"`
	ProviderHealth json.RawMessage `json:"provider_health"`
	Capabilities   json.RawMessage `json:"capabilities"`
	Host           *wireHost       `json:"host,omitempty"`
	GPUs           []wireGPU       `json:"gpus"`
	// Phase 2 certificate-distribution fields (mirrors the gateway's
	// agentTelemetryRequest CertFingerprint/CertNotAfter/CertMode/
	// CertCAFingerprints tags exactly).
	CertFingerprint    string    `json:"cert_fingerprint"`
	CertNotAfter       time.Time `json:"cert_not_after"`
	CertMode           string    `json:"cert_mode"`
	CertCAFingerprints []string  `json:"cert_ca_fingerprints"`
	// ProxyRoutes mirrors the gateway's agentTelemetryRequest ProxyRoutes tag
	// (Certificates P4 Task 9 ingest, Task 4 agent side).
	ProxyRoutes []wireProxyRoute `json:"proxy_routes,omitempty"`
	// Runtimes mirrors the gateway's agentRuntimeSample tag
	// (gateway/backend/internal/gateway/agent_ingest.go) exactly, field for
	// field (agent-runtime-manager Task 9/18).
	Runtimes []wireRuntimeSample `json:"runtimes,omitempty"`
}

type wireRuntimeGPU struct {
	Index          int `json:"index"`
	VRAMMeasuredMB int `json:"vram_measured_mb"`
}

type wireRuntimeError struct {
	Message    string    `json:"message"`
	At         time.Time `json:"at"`
	ExitCode   int       `json:"exit_code"`
	Failures   int       `json:"failures"`
	StderrTail string    `json:"stderr_tail,omitempty"`
}

type wireRuntimeSample struct {
	SpecID         string    `json:"spec_id"`
	Model          string    `json:"model"`
	State          string    `json:"state"`
	Since          time.Time `json:"since"`
	PID            int       `json:"pid,omitempty"`
	Port           int       `json:"port,omitempty"`
	InFlight       int       `json:"in_flight"`
	Restarts       int       `json:"restarts"`
	ContextSize    int       `json:"context_size"`
	ActiveRequests int       `json:"active_requests"`
	QueueDepth     int       `json:"queue_depth"`
	MetricsProbe   string    `json:"metrics_probe"`
	ContextProbe   string    `json:"context_probe"`
	// LiveProgressSupport mirrors the gateway's agentRuntimeSample tag
	// (task 5's ingest side); additive beside the two probe-state fields
	// above (task 4).
	LiveProgressSupport string            `json:"live_progress_support"`
	GPUs                []wireRuntimeGPU  `json:"gpus,omitempty"`
	LastError           *wireRuntimeError `json:"last_error,omitempty"`
}

type wireProxyRoute struct {
	Listen    int  `json:"listen"`
	TLSActive bool `json:"tls_active"`
}

type wireHost struct {
	CPUUtilPct     float64   `json:"cpu_util_pct"`
	MemUsedBytes   int64     `json:"mem_used_bytes"`
	MemTotalBytes  int64     `json:"mem_total_bytes"`
	SwapUsedBytes  int64     `json:"swap_used_bytes"`
	SwapTotalBytes int64     `json:"swap_total_bytes"`
	Load1          float64   `json:"load1"`
	Load5          float64   `json:"load5"`
	Load15         float64   `json:"load15"`
	Net            []wireNet `json:"net"`
}

type wireNet struct {
	Name    string `json:"name"`
	RxBytes int64  `json:"rx_bytes"`
	TxBytes int64  `json:"tx_bytes"`
}

type wireGPU struct {
	Index         int     `json:"index"`
	Name          string  `json:"name"`
	UUID          string  `json:"uuid"`
	UtilPct       float64 `json:"util_pct"`
	MemUsedBytes  int64   `json:"mem_used_bytes"`
	MemTotalBytes int64   `json:"mem_total_bytes"`
	TempC         int     `json:"temp_c"`
	VRAMTempC     int     `json:"vram_temp_c"`
	PowerW        float64 `json:"power_w"`
	FanPct        float64 `json:"fan_pct"`
}

func TestSampleMarshalMatchesWireContract(t *testing.T) {
	s := Sample{
		ReportedAt:   time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
		AgentVersion: "0.1.0",
		OS:           "linux",
		Arch:         "amd64",
		Host: &Host{
			CPUUtilPct:     42.5,
			MemUsedBytes:   8_000_000_000,
			MemTotalBytes:  16_000_000_000,
			SwapUsedBytes:  100,
			SwapTotalBytes: 200,
			Load1:          1.5,
			Load5:          1.0,
			Load15:         0.5,
			Net: []Net{
				{Name: "total", RxBytes: 111, TxBytes: 222},
			},
		},
		GPUs: []GPU{
			{
				Index:         0,
				Name:          "NVIDIA GeForce RTX 4090",
				UUID:          "GPU-aaaa-0",
				UtilPct:       88,
				MemUsedBytes:  12_000 * 1024 * 1024,
				MemTotalBytes: 24_564 * 1024 * 1024,
				TempC:         71,
				VRAMTempC:     0,
				PowerW:        320.5,
				FanPct:        60,
			},
		},
	}

	s.Normalize()

	raw, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Round-trip into a struct mirroring the gateway's tags.
	var got wireSample
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal into wire struct: %v", err)
	}

	if got.Host == nil {
		t.Fatal("host round-tripped as nil")
	}
	if got.Host.CPUUtilPct != 42.5 {
		t.Errorf("host.cpu_util_pct = %v, want 42.5", got.Host.CPUUtilPct)
	}
	if got.Host.MemUsedBytes != 8_000_000_000 {
		t.Errorf("host.mem_used_bytes = %v, want 8000000000", got.Host.MemUsedBytes)
	}
	if len(got.Host.Net) != 1 || got.Host.Net[0].RxBytes != 111 {
		t.Errorf("host.net[0].rx_bytes round-trip failed: %+v", got.Host.Net)
	}
	if len(got.GPUs) != 1 {
		t.Fatalf("gpus len = %d, want 1", len(got.GPUs))
	}
	if got.GPUs[0].UUID != "GPU-aaaa-0" {
		t.Errorf("gpus[0].uuid = %q, want GPU-aaaa-0", got.GPUs[0].UUID)
	}
	if got.GPUs[0].PowerW != 320.5 {
		t.Errorf("gpus[0].power_w = %v, want 320.5", got.GPUs[0].PowerW)
	}

	// Assert the raw JSON carries the empty-object defaults and never a null slice.
	if !bytes.Contains(raw, []byte(`"provider_health":{}`)) {
		t.Errorf("raw JSON missing provider_health:{}; got %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"capabilities":{}`)) {
		t.Errorf("raw JSON missing capabilities:{}; got %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"gpus":[`)) {
		t.Errorf("raw JSON gpus not an array; got %s", raw)
	}
	if bytes.Contains(raw, []byte(`null`)) {
		t.Errorf("raw JSON contains null; got %s", raw)
	}
}

func TestMergeUniqueStringsDeduplicatesInStableOrder(t *testing.T) {
	got := MergeUniqueStrings([]string{"a", "b", ""}, []string{" b ", "c", "a"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got=%v want=%v", got, want)
		}
	}
}

// TestCertFieldsRoundTripAndCAFingerprintsDefaultToEmptyNotNull is the sample
// half of the Task 5b sample round-trip requirement: the four Phase 2
// certificate fields marshal into the gateway's exact wire tags, and an unset
// CertCAFingerprints defaults (via Normalize) to "[]", never to a JSON null --
// the same invariant TestSampleMarshalMatchesWireContract already pins for
// every other slice field on Sample.
func TestCertFieldsRoundTripAndCAFingerprintsDefaultToEmptyNotNull(t *testing.T) {
	notAfter := time.Date(2027, 3, 4, 5, 6, 7, 0, time.UTC)
	s := Sample{
		CertFingerprint:    "abc123",
		CertNotAfter:       notAfter,
		CertMode:           "files",
		CertCAFingerprints: []string{"root-a", "root-b"},
	}
	s.Normalize()

	raw, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got wireSample
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal into wire struct: %v", err)
	}
	if got.CertFingerprint != "abc123" {
		t.Errorf("cert_fingerprint = %q, want abc123", got.CertFingerprint)
	}
	if !got.CertNotAfter.Equal(notAfter) {
		t.Errorf("cert_not_after = %v, want %v", got.CertNotAfter, notAfter)
	}
	if got.CertMode != "files" {
		t.Errorf("cert_mode = %q, want files", got.CertMode)
	}
	if len(got.CertCAFingerprints) != 2 || got.CertCAFingerprints[0] != "root-a" || got.CertCAFingerprints[1] != "root-b" {
		t.Errorf("cert_ca_fingerprints = %+v, want [root-a root-b]", got.CertCAFingerprints)
	}

	// A zero Sample must default CertCAFingerprints to "[]", not "null" --
	// mirroring gpus/loaded_models.
	var zero Sample
	zero.Normalize()
	rawZero, err := json.Marshal(&zero)
	if err != nil {
		t.Fatalf("marshal zero: %v", err)
	}
	if !bytes.Contains(rawZero, []byte(`"cert_ca_fingerprints":[]`)) {
		t.Errorf("raw JSON missing cert_ca_fingerprints:[]; got %s", rawZero)
	}
	if bytes.Contains(rawZero, []byte(`null`)) {
		t.Errorf("raw JSON contains null; got %s", rawZero)
	}
}

// TestSampleCarriesProxyRoutes is the sample half of Certificates P4 Task 4:
// a sample built with the proxy manager reporting one active route marshals
// its listen/tls_active exactly, and a Sample with no ProxyRoutes at all
// (the off/files/no-manager case) omits the field entirely -- never a null
// or empty-array proxy_routes key.
func TestSampleCarriesProxyRoutes(t *testing.T) {
	s := Sample{
		ProxyRoutes: []ProxyRouteSample{{Listen: 8600, TLSActive: true}},
	}
	s.Normalize()

	raw, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"proxy_routes":[{"listen":8600,"tls_active":true}]`)) {
		t.Errorf("raw JSON missing proxy_routes:[{listen:8600,tls_active:true}]; got %s", raw)
	}

	var got wireSample
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal into wire struct: %v", err)
	}
	if len(got.ProxyRoutes) != 1 || got.ProxyRoutes[0].Listen != 8600 || !got.ProxyRoutes[0].TLSActive {
		t.Errorf("proxy_routes round-trip = %+v, want [{8600 true}]", got.ProxyRoutes)
	}

	// Absent manager (the zero value every off/files/pre-existing agent
	// produces): proxy_routes must be OMITTED, not null or [].
	var zero Sample
	zero.Normalize()
	rawZero, err := json.Marshal(&zero)
	if err != nil {
		t.Fatalf("marshal zero: %v", err)
	}
	if bytes.Contains(rawZero, []byte(`proxy_routes`)) {
		t.Errorf("raw JSON should omit proxy_routes when unset; got %s", rawZero)
	}
}

// TestSampleCarriesRuntimes is the sample half of Task 18's agent-managed
// runtime status wire contract: a populated Runtimes entry (including a
// measured GPU and a last_error) marshals into the gateway's exact tags,
// and a Sample with no Runtimes at all (every pre-Task-18 agent, and a
// Task-18 agent that never negotiated runtime_manager) OMITS the key
// entirely -- never a null or an empty array.
func TestSampleCarriesRuntimes(t *testing.T) {
	since := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	failedAt := time.Date(2026, 8, 20, 9, 59, 0, 0, time.UTC)
	s := Sample{
		Runtimes: []RuntimeSample{
			{
				SpecID:   "rspec_a",
				Model:    "qwen-coder",
				State:    "running",
				Since:    since,
				PID:      4242,
				Port:     9001,
				InFlight: 2,
				Restarts: 1,
				GPUs:     []RuntimeGPUSample{{Index: 0, VRAMMeasuredMB: 21234}},
				LastError: &RuntimeErrorSample{
					Message:    "runtime: process exited unexpectedly (exit code 1)",
					At:         failedAt,
					ExitCode:   1,
					Failures:   3,
					StderrTail: "CUDA error: out of memory",
				},
			},
		},
	}
	s.Normalize()

	raw, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got wireSample
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal into wire struct: %v", err)
	}
	if len(got.Runtimes) != 1 {
		t.Fatalf("runtimes len = %d, want 1", len(got.Runtimes))
	}
	rt := got.Runtimes[0]
	if rt.SpecID != "rspec_a" || rt.Model != "qwen-coder" || rt.State != "running" {
		t.Errorf("runtimes[0] identity fields = %+v", rt)
	}
	if !rt.Since.Equal(since) {
		t.Errorf("runtimes[0].since = %v, want %v", rt.Since, since)
	}
	if rt.PID != 4242 || rt.Port != 9001 || rt.InFlight != 2 || rt.Restarts != 1 {
		t.Errorf("runtimes[0] counters = %+v", rt)
	}
	if len(rt.GPUs) != 1 || rt.GPUs[0].Index != 0 || rt.GPUs[0].VRAMMeasuredMB != 21234 {
		t.Errorf("runtimes[0].gpus = %+v", rt.GPUs)
	}
	if rt.LastError == nil {
		t.Fatal("runtimes[0].last_error round-tripped as nil")
	}
	if rt.LastError.ExitCode != 1 || rt.LastError.Failures != 3 || rt.LastError.StderrTail != "CUDA error: out of memory" {
		t.Errorf("runtimes[0].last_error = %+v", rt.LastError)
	}

	// A Sample with no Runtimes at all -- the byte-neutrality case a nil
	// RuntimeDriver must produce -- must OMIT the "runtimes" key entirely,
	// not marshal a null or an empty array.
	var zero Sample
	zero.Normalize()
	rawZero, err := json.Marshal(&zero)
	if err != nil {
		t.Fatalf("marshal zero: %v", err)
	}
	if bytes.Contains(rawZero, []byte(`runtimes`)) {
		t.Errorf("raw JSON should omit runtimes entirely when unset; got %s", rawZero)
	}
}

// TestRuntimeSampleContextAndMetricsFieldsRoundTrip is the sample half of
// Task 7: ContextSize/ActiveRequests/QueueDepth mirror the gateway ingest's
// exact tags (context_size/active_requests/queue_depth) and, like
// InFlight/Restarts, are always-present counters -- NOT omitempty -- so a
// spec left at the zero value still marshals all three keys as present with
// value 0, never omitting them.
func TestRuntimeSampleContextAndMetricsFieldsRoundTrip(t *testing.T) {
	since := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	s := Sample{
		Runtimes: []RuntimeSample{
			{
				SpecID:         "rspec_ctx",
				Model:          "qwen-coder",
				State:          "running",
				Since:          since,
				ContextSize:    32768,
				ActiveRequests: 3,
				QueueDepth:     7,
			},
		},
	}
	s.Normalize()

	raw, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got wireSample
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal into wire struct: %v", err)
	}
	if len(got.Runtimes) != 1 {
		t.Fatalf("runtimes len = %d, want 1", len(got.Runtimes))
	}
	rt := got.Runtimes[0]
	if rt.ContextSize != 32768 || rt.ActiveRequests != 3 || rt.QueueDepth != 7 {
		t.Errorf("runtimes[0] context/metrics fields = %+v, want {32768 3 7}", rt)
	}

	// A spec left at the zero value for all three fields must still carry
	// them PRESENT in the wire JSON (not omitempty) -- mirroring
	// InFlight/Restarts above. Probe the raw JSON directly (rather than
	// bytes.Contains) because Sample itself already has its own top-level
	// active_requests/queue_depth keys with the same names; only a
	// key-presence check scoped to the runtimes entry proves this field's
	// own omitempty behavior.
	var zero Sample
	zero.Runtimes = []RuntimeSample{{SpecID: "rspec_zero"}}
	zero.Normalize()
	rawZero, err := json.Marshal(&zero)
	if err != nil {
		t.Fatalf("marshal zero: %v", err)
	}
	var probe struct {
		Runtimes []map[string]json.RawMessage `json:"runtimes"`
	}
	if err := json.Unmarshal(rawZero, &probe); err != nil {
		t.Fatalf("unmarshal probe: %v", err)
	}
	if len(probe.Runtimes) != 1 {
		t.Fatalf("probe runtimes len = %d, want 1", len(probe.Runtimes))
	}
	for _, key := range []string{"context_size", "active_requests", "queue_depth"} {
		v, ok := probe.Runtimes[0][key]
		if !ok {
			t.Errorf("runtimes[0] missing %q key; must not be omitempty", key)
			continue
		}
		if string(v) != "0" {
			t.Errorf("runtimes[0].%s = %s, want 0", key, v)
		}
	}
}

// TestRuntimeSampleProbeFieldsRoundTrip is the sample half of Task 1: MetricsProbe
// and ContextProbe mirror the gateway ingest's exact tags (metrics_probe /
// context_probe) and, like ContextSize/ActiveRequests/QueueDepth, are always-
// present string fields -- NOT omitempty -- so a spec left at the zero value
// still marshals both keys as present with value "", never omitting them.
func TestRuntimeSampleProbeFieldsRoundTrip(t *testing.T) {
	since := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	s := Sample{
		Runtimes: []RuntimeSample{
			{
				SpecID:       "rspec_probe",
				Model:        "qwen-coder",
				State:        "running",
				Since:        since,
				MetricsProbe: "ok",
				ContextProbe: "unreachable",
			},
		},
	}
	s.Normalize()

	raw, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got wireSample
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal into wire struct: %v", err)
	}
	if len(got.Runtimes) != 1 {
		t.Fatalf("runtimes len = %d, want 1", len(got.Runtimes))
	}
	rt := got.Runtimes[0]
	if rt.MetricsProbe != "ok" {
		t.Errorf("runtimes[0].metrics_probe = %q, want ok", rt.MetricsProbe)
	}
	if rt.ContextProbe != "unreachable" {
		t.Errorf("runtimes[0].context_probe = %q, want unreachable", rt.ContextProbe)
	}

	// A spec left at the zero value for probe fields must still carry
	// them PRESENT in the wire JSON (not omitempty) as empty strings --
	// mirroring ContextSize/ActiveRequests/QueueDepth. Probe the raw JSON
	// to verify the fields are present with value "".
	var zero Sample
	zero.Runtimes = []RuntimeSample{{SpecID: "rspec_zero_probe"}}
	zero.Normalize()
	rawZero, err := json.Marshal(&zero)
	if err != nil {
		t.Fatalf("marshal zero: %v", err)
	}
	var probe struct {
		Runtimes []map[string]json.RawMessage `json:"runtimes"`
	}
	if err := json.Unmarshal(rawZero, &probe); err != nil {
		t.Fatalf("unmarshal probe: %v", err)
	}
	if len(probe.Runtimes) != 1 {
		t.Fatalf("probe runtimes len = %d, want 1", len(probe.Runtimes))
	}
	for _, key := range []string{"metrics_probe", "context_probe"} {
		v, ok := probe.Runtimes[0][key]
		if !ok {
			t.Errorf("runtimes[0] missing %q key; must not be omitempty", key)
			continue
		}
		if string(v) != `""` {
			t.Errorf("runtimes[0].%s = %s, want \"\"", key, v)
		}
	}
}

// TestRuntimeSampleLiveProgressSupportRoundTrip is the sample half of task 4
// (#51): LiveProgressSupport mirrors the gateway ingest's exact tag
// (live_progress_support) beside MetricsProbe/ContextProbe, and -- like
// them -- is an always-present string field, NOT omitempty, so an
// undetermined verdict marshals as the key present with value "" (which
// IS the documented "unknown" state) rather than being dropped from the
// wire entirely.
func TestRuntimeSampleLiveProgressSupportRoundTrip(t *testing.T) {
	since := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	s := Sample{
		Runtimes: []RuntimeSample{
			{
				SpecID:              "rspec_capability",
				Model:               "qwen-coder",
				State:               "running",
				Since:               since,
				LiveProgressSupport: "supported",
			},
		},
	}
	s.Normalize()

	raw, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got wireSample
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal into wire struct: %v", err)
	}
	if len(got.Runtimes) != 1 {
		t.Fatalf("runtimes len = %d, want 1", len(got.Runtimes))
	}
	if rt := got.Runtimes[0]; rt.LiveProgressSupport != "supported" {
		t.Errorf("runtimes[0].live_progress_support = %q, want supported", rt.LiveProgressSupport)
	}

	// A spec left at the zero value (undetermined) must still carry the key
	// PRESENT in the wire JSON -- not omitempty -- as an empty string.
	var zero Sample
	zero.Runtimes = []RuntimeSample{{SpecID: "rspec_capability_zero"}}
	zero.Normalize()
	rawZero, err := json.Marshal(&zero)
	if err != nil {
		t.Fatalf("marshal zero: %v", err)
	}
	var probe struct {
		Runtimes []map[string]json.RawMessage `json:"runtimes"`
	}
	if err := json.Unmarshal(rawZero, &probe); err != nil {
		t.Fatalf("unmarshal probe: %v", err)
	}
	if len(probe.Runtimes) != 1 {
		t.Fatalf("probe runtimes len = %d, want 1", len(probe.Runtimes))
	}
	v, ok := probe.Runtimes[0]["live_progress_support"]
	if !ok {
		t.Fatalf("runtimes[0] missing \"live_progress_support\" key; must not be omitempty")
	}
	if string(v) != `""` {
		t.Errorf("runtimes[0].live_progress_support = %s, want \"\"", v)
	}
}

// TestNormalizeForcesVerdictsNonNil proves Normalize forces a non-nil
// Capabilities' Verdicts to marshal as [] rather than null -- the same rule
// Normalize already applies to Net/GPUs/ProxyRoutes on this wire. A
// RuntimeSample can carry a non-nil Capabilities (detection ran, determined
// nothing) whose Verdicts was never explicitly set (a nil slice, Go's zero
// value); nothing else on this path guarantees it round-trips as an empty
// array instead of JSON's null, and null would break a decoder that treats
// the key's presence as this capability's row-absence-means-unknown model.
func TestNormalizeForcesVerdictsNonNil(t *testing.T) {
	s := Sample{
		Runtimes: []RuntimeSample{
			{
				SpecID:       "rspec_verdicts_nil",
				Capabilities: &Capabilities{},
			},
		},
	}
	s.Normalize()

	raw, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var probe struct {
		Runtimes []struct {
			Capabilities struct {
				Verdicts json.RawMessage `json:"verdicts"`
			} `json:"capabilities"`
		} `json:"runtimes"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal probe: %v", err)
	}
	if len(probe.Runtimes) != 1 {
		t.Fatalf("probe runtimes len = %d, want 1", len(probe.Runtimes))
	}
	if got := string(probe.Runtimes[0].Capabilities.Verdicts); got != "[]" {
		t.Errorf("runtimes[0].capabilities.verdicts = %s, want [] (never null)", got)
	}
}

// TestCapabilitiesSourceRidesTheWireWithoutCollapsingNilVsEmpty pins the
// three properties Capabilities.Source has to have at once (#54):
//
//  1. a reported source rides the wire under the key "source", verbatim --
//     the gateway attributes its capability rows to it, and the two modules
//     share no code, so the STRING is the whole contract;
//  2. an UNREPORTED source omits the key (omitempty) while "verdicts" stays
//     present, so a non-nil-but-empty Capabilities still says "detection ran
//     and determined nothing" rather than decoding as the nil an agent that
//     predates capability detection sends. That nil-vs-empty distinction is
//     what RuntimeSample.Capabilities is a POINTER for, and a new field must
//     not be able to erase it;
//  3. Normalize invents nothing here. An empty Source carries the fact that
//     the producer never named one, which is the cue the gateway's default
//     reads; defaulting it agent-side would destroy that.
func TestCapabilitiesSourceRidesTheWireWithoutCollapsingNilVsEmpty(t *testing.T) {
	s := Sample{
		Runtimes: []RuntimeSample{
			{SpecID: "rspec_sourced", Capabilities: &Capabilities{Source: CapabilitySourceOllamaAPIShow}},
			{SpecID: "rspec_unsourced", Capabilities: &Capabilities{}},
			{SpecID: "rspec_no_detection"},
		},
	}
	s.Normalize()

	if got := s.Runtimes[1].Capabilities.Source; got != "" {
		t.Errorf("Normalize set an unreported Source to %q, want it left empty -- the gateway's own default reads that emptiness", got)
	}

	raw, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var probe struct {
		Runtimes []map[string]json.RawMessage `json:"runtimes"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal probe: %v", err)
	}
	if len(probe.Runtimes) != 3 {
		t.Fatalf("probe runtimes len = %d, want 3", len(probe.Runtimes))
	}

	sourced := map[string]json.RawMessage{}
	if err := json.Unmarshal(probe.Runtimes[0]["capabilities"], &sourced); err != nil {
		t.Fatalf("unmarshal sourced capabilities: %v", err)
	}
	if got := string(sourced["source"]); got != `"ollama_api_show"` {
		t.Errorf("runtimes[0].capabilities.source = %s, want \"ollama_api_show\" (the literal the gateway matches on)", got)
	}

	unsourced := map[string]json.RawMessage{}
	if err := json.Unmarshal(probe.Runtimes[1]["capabilities"], &unsourced); err != nil {
		t.Fatalf("unmarshal unsourced capabilities: %v", err)
	}
	if _, ok := unsourced["source"]; ok {
		t.Errorf("runtimes[1].capabilities carries a \"source\" key (%s), want it omitted", probe.Runtimes[1]["capabilities"])
	}
	if _, ok := unsourced["verdicts"]; !ok {
		t.Errorf("runtimes[1].capabilities = %s, want a present \"verdicts\" key -- without it a non-nil-but-empty Capabilities would be indistinguishable from an absent one", probe.Runtimes[1]["capabilities"])
	}
	if _, ok := probe.Runtimes[2]["capabilities"]; ok {
		t.Errorf("runtimes[2] carries a \"capabilities\" key (%s), want it omitted entirely -- that absence is how an agent predating capability detection is told apart from one that detected nothing", probe.Runtimes[2]["capabilities"])
	}
}

func TestNormalizeDefaultsEmpty(t *testing.T) {
	var s Sample
	s.Normalize()

	raw, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if !bytes.Contains(raw, []byte(`"gpus":[]`)) {
		t.Errorf("raw JSON missing gpus:[]; got %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"provider_health":{}`)) {
		t.Errorf("raw JSON missing provider_health:{}; got %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"capabilities":{}`)) {
		t.Errorf("raw JSON missing capabilities:{}; got %s", raw)
	}
	if bytes.Contains(raw, []byte(`"host"`)) {
		t.Errorf("raw JSON should omit host on a zero Sample; got %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"loaded_models":[]`)) {
		t.Errorf("raw JSON missing loaded_models:[]; got %s", raw)
	}
}

func TestHostPowerFieldsOmittedWhenNil(t *testing.T) {
	// Nil power scalars must be OMITTED from the wire JSON (so the gateway's
	// *float64 stays nil = "not measured"), never serialized as 0.
	s := Sample{Host: &Host{CPUUtilPct: 10}}
	s.Normalize()
	raw, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(raw, []byte(`cpu_power_w`)) || bytes.Contains(raw, []byte(`system_power_w`)) {
		t.Fatalf("nil power must be omitted; got %s", raw)
	}
}

func TestHostPowerFieldsPresentWhenSet(t *testing.T) {
	cpu := 65.5
	sys := 180.0
	s := Sample{Host: &Host{CPUUtilPct: 10, CPUPowerW: &cpu, SystemPowerW: &sys}}
	s.Normalize()
	raw, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"cpu_power_w":65.5`)) {
		t.Fatalf("cpu_power_w not serialized; got %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"system_power_w":180`)) {
		t.Fatalf("system_power_w not serialized; got %s", raw)
	}
}

func TestNormalizeLeavesCPUTempNil(t *testing.T) {
	// CPUTempC is best-effort NULLABLE; Normalize must never default it to a real 0.
	s := Sample{Host: &Host{CPUUtilPct: 10}}
	s.Normalize()
	if s.Host.CPUTempC != nil {
		t.Fatalf("CPUTempC = %v, want nil after Normalize", *s.Host.CPUTempC)
	}
}

func TestHostCPUTempFieldOmittedWhenNil(t *testing.T) {
	// Nil CPUTempC must be OMITTED from the wire JSON (so the gateway's *float64
	// stays nil = "not measured"), never serialized as 0. Mirrors the power fields.
	s := Sample{Host: &Host{CPUUtilPct: 10}}
	s.Normalize()
	raw, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(raw, []byte(`cpu_temp_c`)) {
		t.Fatalf("nil CPUTempC must be omitted; got %s", raw)
	}
}

func TestHostCPUTempFieldPresentWhenSet(t *testing.T) {
	temp := 58.5
	s := Sample{Host: &Host{CPUUtilPct: 10, CPUTempC: &temp}}
	s.Normalize()
	raw, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"cpu_temp_c":58.5`)) {
		t.Fatalf("cpu_temp_c not serialized; got %s", raw)
	}
}

// TestSampleRuntimeConfigAppliedETagIsOmittedWhenEmpty pins the wire tag and the
// omitempty on the acknowledgement field. Absent-not-empty is the contract
// the gateway's fallback keys on: an agent that acknowledges nothing --
// because it is older, because it is in file mode, or because it has
// applied no gateway document yet -- must produce the pre-feature sample
// shape, byte for byte.
func TestSampleRuntimeConfigAppliedETagIsOmittedWhenEmpty(t *testing.T) {
	s := Sample{ReportedAt: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)}
	s.Normalize()
	raw, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(raw, []byte("runtime_config_applied_etag")) {
		t.Fatalf("raw JSON carries runtime_config_applied_etag with nothing to acknowledge: %s", raw)
	}

	s.RuntimeConfigAppliedETag = "e1"
	raw, err = json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"runtime_config_applied_etag":"e1"`)) {
		t.Fatalf("raw JSON missing runtime_config_applied_etag:\"e1\": %s", raw)
	}
}
