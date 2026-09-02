// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"op-ai-gateway/internal/portal"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestBenchmarkResultVRAMNilVersusInconclusive is the distinction the whole
// result shape exists for: a nil VRAM ("the run never reached the measurement
// phase") and a non-nil VRAM carrying an Inconclusive reason ("it ran and
// reached no number") must be told apart by a consumer that only sees the
// serialized status. This is deliberately NOT VisionCapable's nil-means-both
// contract: "no result" and "no result because the model was already being
// served by something we could not stop" send an operator to two different
// places.
func TestBenchmarkResultVRAMNilVersusInconclusive(t *testing.T) {
	// (1) A run that never measured: the key is absent entirely.
	notRun, err := json.Marshal(BenchmarkResult{MappingID: "map1", Error: "isolation refused"})
	if err != nil {
		t.Fatalf("marshal not-run result: %v", err)
	}
	if strings.Contains(string(notRun), "\"vram\"") {
		t.Fatalf("a result with no VRAM report must omit the key entirely, got %s", notRun)
	}

	// (2) A run that measured nothing, and says why.
	inconclusive, err := json.Marshal(BenchmarkResult{MappingID: "map1", VRAM: &VRAMReport{
		Isolated:          true,
		IsolationEvidence: map[string]string{"spec1": vramEvidenceStoppedAfterWrite},
		DrainedSpecIDs:    []string{"spec1"},
		Inconclusive:      vramInconclusiveAlreadyResident,
		GPUs:              []VRAMGPUItem{},
	}})
	if err != nil {
		t.Fatalf("marshal inconclusive result: %v", err)
	}
	for _, want := range []string{
		`"vram"`, `"isolated":true`, `"inconclusive":"already_resident"`,
		`"isolation_evidence":{"spec1":"stopped_after_write"}`, `"drained_spec_ids":["spec1"]`,
		`"gpus":[]`,
	} {
		if !strings.Contains(string(inconclusive), want) {
			t.Fatalf("inconclusive result JSON missing %s:\n%s", want, inconclusive)
		}
	}

	// (3) The consumer's side of the contract: decoding both tells them apart.
	var back BenchmarkResult
	if err := json.Unmarshal(notRun, &back); err != nil {
		t.Fatalf("unmarshal not-run result: %v", err)
	}
	if back.VRAM != nil {
		t.Fatalf("not-run result decoded VRAM = %#v, want nil", back.VRAM)
	}
	back = BenchmarkResult{}
	if err := json.Unmarshal(inconclusive, &back); err != nil {
		t.Fatalf("unmarshal inconclusive result: %v", err)
	}
	if back.VRAM == nil {
		t.Fatal("inconclusive result decoded VRAM = nil, want a report carrying the reason")
	}
	if back.VRAM.Inconclusive != vramInconclusiveAlreadyResident {
		t.Fatalf("decoded Inconclusive = %q, want %q", back.VRAM.Inconclusive, vramInconclusiveAlreadyResident)
	}

	// (4) A definitive result carries no reason, and every per-GPU field survives.
	definitive, err := json.Marshal(&VRAMReport{
		Isolated: true,
		GPUs: []VRAMGPUItem{{
			Index: 1, Fingerprint: "GPU-abc", FingerprintKind: vramFingerprintUUID,
			BaselineUsedMB: 512, DeltaMB: 21000, MeasuredMB: 20880, Attributable: true,
		}},
	})
	if err != nil {
		t.Fatalf("marshal definitive report: %v", err)
	}
	if strings.Contains(string(definitive), "inconclusive") {
		t.Fatalf("a definitive report must omit inconclusive, got %s", definitive)
	}
	for _, want := range []string{
		`"index":1`, `"fingerprint":"GPU-abc"`, `"fingerprint_kind":"uuid"`,
		`"baseline_used_mb":512`, `"delta_mb":21000`, `"measured_mb":20880`, `"attributable":true`,
	} {
		if !strings.Contains(string(definitive), want) {
			t.Fatalf("definitive report JSON missing %s:\n%s", want, definitive)
		}
	}
}

// TestVRAMReportUnifiedMemoryIsCarriedPerGPU pins the Apple label: the number
// is unified SYSTEM memory there, not dedicated VRAM, and the flag that says
// so travels with the per-GPU item rather than being re-derived downstream.
func TestVRAMReportUnifiedMemoryIsCarriedPerGPU(t *testing.T) {
	payload, err := json.Marshal(&VRAMReport{GPUs: []VRAMGPUItem{{
		Index: 0, Fingerprint: "Apple M3 Max", FingerprintKind: vramFingerprintNameTotal,
		UnifiedMemory: true, BaselineUsedMB: 9000, DeltaMB: 14000,
	}}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(payload), `"unified_memory":true`) {
		t.Fatalf("unified_memory not carried: %s", payload)
	}
	if !strings.Contains(string(payload), `"fingerprint_kind":"name_total"`) {
		t.Fatalf("fingerprint_kind not carried: %s", payload)
	}
}

// TestVRAMIsolationConfirmedRequiresThisRunsOwnEvidence is the rule the
// file-mode defect produced: Isolated is true only when EVERY enumerated spec
// carries evidence this run produced itself. A 200 from an admin_state write
// is not evidence.
func TestVRAMIsolationConfirmedRequiresThisRunsOwnEvidence(t *testing.T) {
	cases := []struct {
		name       string
		enumerated []string
		evidence   map[string]string
		want       bool
	}{
		{
			name:       "every spec carries this run's own evidence",
			enumerated: []string{"spec_target", "spec_sibling"},
			evidence: map[string]string{
				"spec_target":  vramEvidenceStoppedAfterWrite,
				"spec_sibling": vramEvidenceNoProcessAtWrite,
			},
			want: true,
		},
		{
			name:       "a spec with no entry at all is not isolated",
			enumerated: []string{"spec_target", "spec_sibling"},
			evidence:   map[string]string{"spec_target": vramEvidenceStoppedAfterWrite},
			want:       false,
		},
		{
			name:       "an unrecognized evidence value is not evidence",
			enumerated: []string{"spec_target"},
			evidence:   map[string]string{"spec_target": "write_returned_200"},
			want:       false,
		},
		{
			name:       "no evidence map at all",
			enumerated: []string{"spec_target"},
			evidence:   nil,
			want:       false,
		},
		{
			// Vacuous truth is exactly the file-mode defect's shape: nothing
			// enumerated, nothing awaited, "isolated" claimed for a fleet the
			// run never touched.
			name:       "an empty enumeration proves nothing",
			enumerated: nil,
			evidence:   map[string]string{"spec_target": vramEvidenceStoppedAfterWrite},
			want:       false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := vramIsolationConfirmed(tc.enumerated, tc.evidence); got != tc.want {
				t.Fatalf("vramIsolationConfirmed(%v, %v) = %v, want %v", tc.enumerated, tc.evidence, got, tc.want)
			}
		})
	}
}

// TestVRAMHistoryRowIsEvidenceNotAuthority pins the kind=="vram" history row:
// the per-GPU payload rides in its OWN column (never capacity_curve), the row
// records an outcome the operator reads, and a run that reached no report at
// all still records the failure with an empty payload.
func TestVRAMHistoryRowIsEvidenceNotAuthority(t *testing.T) {
	at := time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC)
	report := &VRAMReport{
		Isolated:          true,
		IsolationEvidence: map[string]string{"spec1": vramEvidenceStoppedAfterWrite},
		DrainedSpecIDs:    []string{"spec1"},
		GPUs: []VRAMGPUItem{{
			Index: 0, Fingerprint: "GPU-abc", FingerprintKind: vramFingerprintUUID,
			BaselineUsedMB: 700, DeltaMB: 22000, MeasuredMB: 21900, Attributable: true,
		}},
	}

	row := vramHistoryRow("map1", "srv1", at, report, "")
	if row.Kind != "vram" {
		t.Fatalf("row.Kind = %q, want %q", row.Kind, "vram")
	}
	if row.MappingID != "map1" || row.ServerID != "srv1" || !row.CreatedAt.Equal(at) {
		t.Fatalf("row identity = %+v", row)
	}
	if row.CapacityCurve != "" {
		t.Fatalf("row.CapacityCurve = %q, want empty (a VRAM payload never reuses the capacity column)", row.CapacityCurve)
	}
	if row.VisionCapable {
		t.Fatalf("row.VisionCapable = true, want false (unused for a VRAM row)")
	}
	if row.Error != "" {
		t.Fatalf("row.Error = %q, want empty", row.Error)
	}
	var decoded VRAMReport
	if err := json.Unmarshal([]byte(row.VRAMJSON), &decoded); err != nil {
		t.Fatalf("decode row.VRAMJSON (%q): %v", row.VRAMJSON, err)
	}
	if !decoded.Isolated || len(decoded.GPUs) != 1 || decoded.GPUs[0].DeltaMB != 22000 ||
		decoded.GPUs[0].MeasuredMB != 21900 || decoded.GPUs[0].FingerprintKind != vramFingerprintUUID {
		t.Fatalf("decoded payload = %+v", decoded)
	}
	if decoded.IsolationEvidence["spec1"] != vramEvidenceStoppedAfterWrite {
		t.Fatalf("decoded isolation evidence = %v", decoded.IsolationEvidence)
	}

	// A run that never reached a report: the row still records the failure, and
	// carries no payload rather than an empty-looking one.
	failed := vramHistoryRow("map1", "srv1", at, nil, "isolation timed out")
	if failed.Kind != "vram" || failed.Error != "isolation timed out" {
		t.Fatalf("failed row = %+v", failed)
	}
	if failed.VRAMJSON != "" {
		t.Fatalf("failed row VRAMJSON = %q, want empty", failed.VRAMJSON)
	}

	// A report with no per-GPU items serializes an empty ARRAY, never a JSON
	// null: a portal that reads gpus with a `?? []` fallback would otherwise
	// render an eternally empty list with no error.
	empty := vramHistoryRow("map1", "srv1", at, &VRAMReport{Inconclusive: vramInconclusiveNoSamples}, "")
	if !strings.Contains(empty.VRAMJSON, `"gpus":[]`) {
		t.Fatalf("empty-report payload = %q, want gpus as an empty array", empty.VRAMJSON)
	}
}

// TestVRAMReportDecodesIntoThePortalDTO guards the one seam this feature has
// no compiler for: the store keeps `vram_json` opaque, so the gateway's
// VRAMReport (the writer) and portal.VRAMReportDTO (the reader) agree only by
// shape. A field added to one and not the other is silently dropped on decode
// -- a measured number the history view simply never shows -- so this test
// compares the JSON tag sets structurally AND round-trips a fully-populated
// value through the wire.
func TestVRAMReportDecodesIntoThePortalDTO(t *testing.T) {
	jsonTags := func(typ reflect.Type) []string {
		out := make([]string, 0, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			tag := typ.Field(i).Tag.Get("json")
			if tag == "" {
				t.Fatalf("%s.%s has no json tag", typ.Name(), typ.Field(i).Name)
			}
			out = append(out, tag)
		}
		return out
	}
	for _, pair := range []struct{ mine, theirs reflect.Type }{
		{reflect.TypeOf(VRAMReport{}), reflect.TypeOf(portal.VRAMReportDTO{})},
		{reflect.TypeOf(VRAMGPUItem{}), reflect.TypeOf(portal.VRAMGPUItemDTO{})},
	} {
		mine, theirs := jsonTags(pair.mine), jsonTags(pair.theirs)
		if !reflect.DeepEqual(mine, theirs) {
			t.Fatalf("%s and %s disagree on the wire:\n gateway: %v\n portal:  %v",
				pair.mine.Name(), pair.theirs.Name(), mine, theirs)
		}
	}

	// Every field non-zero, so a dropped one shows up as a zero on the far side.
	report := VRAMReport{
		Isolated:          true,
		IsolationEvidence: map[string]string{"spec1": vramEvidenceNoProcessAtWrite},
		DrainedSpecIDs:    []string{"spec1", "spec2"},
		RestoreFailed:     []string{"spec2"},
		Inconclusive:      vramInconclusiveBelowFloor,
		GPUs: []VRAMGPUItem{{
			Index: 2, Fingerprint: "GPU-abc", FingerprintKind: vramFingerprintUUID,
			UnifiedMemory: true, BaselineUsedMB: 700, DeltaMB: 22000,
			MeasuredMB: 21900, Attributable: true,
		}},
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	var dto portal.VRAMReportDTO
	if err := json.Unmarshal(encoded, &dto); err != nil {
		t.Fatalf("decode into the portal DTO: %v", err)
	}
	if dto.Isolated != report.Isolated || dto.Inconclusive != report.Inconclusive ||
		dto.IsolationEvidence["spec1"] != vramEvidenceNoProcessAtWrite ||
		len(dto.DrainedSpecIDs) != 2 || len(dto.RestoreFailed) != 1 {
		t.Fatalf("decoded report = %#v", dto)
	}
	if len(dto.GPUs) != 1 {
		t.Fatalf("decoded GPUs = %#v", dto.GPUs)
	}
	got, want := dto.GPUs[0], report.GPUs[0]
	if got.Index != want.Index || got.Fingerprint != want.Fingerprint ||
		got.FingerprintKind != want.FingerprintKind || got.UnifiedMemory != want.UnifiedMemory ||
		got.BaselineUsedMB != want.BaselineUsedMB || got.DeltaMB != want.DeltaMB ||
		got.MeasuredMB != want.MeasuredMB || got.Attributable != want.Attributable {
		t.Fatalf("decoded GPU item = %#v, want %#v", got, want)
	}
}

// TestVRAMResultVocabularyIsPinned pins the exact strings of the three closed
// sets a VRAM result reports with. They are not internal names: each one is
// persisted verbatim inside a kind=="vram" history row's payload, and each is
// what the portal switches on to render an actionable message (with its own
// German and English i18n key). Renaming one silently reclassifies every
// stored row that already carries it and leaves the portal with a value it has
// no message for -- the same discipline as a stable error code.
func TestVRAMResultVocabularyIsPinned(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		// Why a run reached no number. The operator's next action differs per
		// value, which is why this is a vocabulary and not a boolean.
		{vramInconclusiveIsolationTimeout, "isolation_timeout"},
		{vramInconclusiveBaselineUnstable, "baseline_unstable"},
		{vramInconclusivePostLoadUnstable, "post_load_unstable"},
		{vramInconclusiveAlreadyResident, "already_resident"},
		{vramInconclusiveBelowFloor, "below_floor"},
		{vramInconclusiveNoSamples, "no_samples"},
		// Why this run believes a spec was not running. Nothing else counts.
		{vramEvidenceStoppedAfterWrite, "stopped_after_write"},
		{vramEvidenceNoProcessAtWrite, "no_process_at_write"},
		// What identified the card a number is attributed to.
		{vramFingerprintUUID, "uuid"},
		{vramFingerprintNameTotal, "name_total"},
		// The history-row discriminator, alongside speed/capacity/vision.
		{benchmarkKindVRAM, "vram"},
	} {
		if tc.got != tc.want {
			t.Errorf("vocabulary drift: got %q, want %q", tc.got, tc.want)
		}
	}
}

// TestVRAMReportNormalizeGPUs pins the one wire rule a producer can silently
// get wrong: `gpus` has no omitempty (a client must always see an array), so a
// nil slice would serialize as JSON null.
func TestVRAMReportNormalizeGPUs(t *testing.T) {
	var nilReport *VRAMReport
	nilReport.normalizeGPUs() // nil-safe: must not panic

	report := &VRAMReport{Inconclusive: vramInconclusiveBaselineUnstable}
	report.normalizeGPUs()
	if report.GPUs == nil {
		t.Fatal("normalizeGPUs left GPUs nil")
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"gpus":[]`) {
		t.Fatalf("payload = %s, want gpus as an empty array", encoded)
	}

	// Idempotent, and it never disturbs a populated slice.
	populated := &VRAMReport{GPUs: []VRAMGPUItem{{Index: 1, DeltaMB: 100}}}
	populated.normalizeGPUs()
	populated.normalizeGPUs()
	if len(populated.GPUs) != 1 || populated.GPUs[0].DeltaMB != 100 {
		t.Fatalf("populated GPUs = %#v", populated.GPUs)
	}
}
