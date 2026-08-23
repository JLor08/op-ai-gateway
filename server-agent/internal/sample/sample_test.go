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
