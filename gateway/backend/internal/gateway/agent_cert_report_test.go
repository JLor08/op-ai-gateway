// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"strings"
	"testing"
	"time"
)

const (
	certFPa = "aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa7777bbbb8888"
	certFPb = "1111aaaa2222bbbb3333cccc4444dddd5555eeee6666ffff7777aaaa8888bbbb"
	certFPc = "99990000999900009999000099990000999900009999000099990000abcdefab"
)

func TestAgentCertReportRegistryRoundTrip(t *testing.T) {
	r := NewAgentCertReportRegistry()
	at := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return at }

	notAfter := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	r.Report("s1", AgentCertReport{
		Fingerprint:    certFPa,
		CAFingerprints: []string{certFPb},
		Mode:           "files",
		NotAfter:       notAfter,
	})

	rep, ok := r.Get("s1")
	if !ok {
		t.Fatal("Get after Report: ok = false, want true")
	}
	if rep.Fingerprint != certFPa || rep.Mode != "files" || !rep.NotAfter.Equal(notAfter) {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.CAFingerprints) != 1 || rep.CAFingerprints[0] != certFPb {
		t.Fatalf("ca fingerprints = %v", rep.CAFingerprints)
	}
	if !rep.ReportedAt.Equal(at) {
		t.Fatalf("ReportedAt = %v, want %v (the registry must stamp acceptance time)", rep.ReportedAt, at)
	}
	if _, ok := r.Get("other"); ok {
		t.Fatal("Get for a never-reported server returned ok = true")
	}
}

// A caller must not be able to reach into registry state through the returned
// slice: Get copies, and Report copies what it stores.
func TestAgentCertReportRegistryCopiesCAFingerprints(t *testing.T) {
	r := NewAgentCertReportRegistry()
	src := []string{certFPb, certFPc}
	r.Report("s1", AgentCertReport{Fingerprint: certFPa, CAFingerprints: src})

	// Mutating the caller's own slice after Report must not change the registry.
	src[0] = "tampered"
	rep, _ := r.Get("s1")
	if rep.CAFingerprints[0] != certFPb {
		t.Fatalf("Report kept the caller's backing array: %v", rep.CAFingerprints)
	}
	// Mutating the returned slice must not change the registry either.
	rep.CAFingerprints[0] = "tampered"
	again, _ := r.Get("s1")
	if again.CAFingerprints[0] != certFPb {
		t.Fatalf("Get handed out registry state: %v", again.CAFingerprints)
	}
}

// A report with no usable fingerprint must NOT erase a good one -- it proves
// nothing. mode=="off" is the one exception: that is the agent's explicit "I install
// nothing", so the entry goes away.
func TestAgentCertReportRegistryEmptyReportDoesNotEraseUnlessModeOff(t *testing.T) {
	r := NewAgentCertReportRegistry()
	r.Report("s1", AgentCertReport{Fingerprint: certFPa, Mode: "files"})

	r.Report("s1", AgentCertReport{}) // no fingerprint, no mode
	if rep, ok := r.Get("s1"); !ok || rep.Fingerprint != certFPa {
		t.Fatalf("an empty report erased the entry: ok=%v rep=%+v", ok, rep)
	}

	r.Report("s1", AgentCertReport{Mode: "off"})
	if _, ok := r.Get("s1"); ok {
		t.Fatal(`a mode=="off" report must delete the entry`)
	}
}

func TestAgentRegistryRetainsTrustOnlyReportAndStillShowsNotInstalled(t *testing.T) {
	r := NewAgentCertReportRegistry()
	r.Report("s1", AgentCertReport{Mode: "off", CAFingerprints: []string{certFPb}})
	rep, ok := r.Get("s1")
	if !ok {
		t.Fatal("root-only report was discarded")
	}
	if rep.Fingerprint != "" || rep.Mode != "off" || len(rep.CAFingerprints) != 1 || rep.CAFingerprints[0] != certFPb {
		t.Fatalf("report=%+v", rep)
	}
	leaf, roots, mode, _, _, ok := r.CertReport("s1")
	if !ok || leaf != "" || mode != "off" || len(roots) != 1 {
		t.Fatalf("adapter leaf=%q roots=%v mode=%q ok=%v", leaf, roots, mode, ok)
	}
}

func TestAgentCertReportRegistryRetain(t *testing.T) {
	r := NewAgentCertReportRegistry()
	r.Report("keep", AgentCertReport{Fingerprint: certFPa})
	r.Report("drop", AgentCertReport{Fingerprint: certFPb})

	r.Retain(map[string]struct{}{"keep": {}})
	if _, ok := r.Get("keep"); !ok {
		t.Fatal("Retain evicted a live server")
	}
	if _, ok := r.Get("drop"); ok {
		t.Fatal("Retain kept a server that is no longer live")
	}
}

func TestAgentCertReportRegistryNilSafe(t *testing.T) {
	var r *AgentCertReportRegistry
	r.Report("s1", AgentCertReport{Fingerprint: certFPa}) // must not panic
	r.Retain(map[string]struct{}{})                       // must not panic
	if _, ok := r.Get("s1"); ok {
		t.Fatal("nil registry reported a value")
	}
	if _, _, _, _, _, ok := r.CertReport("s1"); ok {
		t.Fatal("nil registry CertReport reported ok = true")
	}
	// An empty server id is ignored rather than creating a "" entry.
	real := NewAgentCertReportRegistry()
	real.Report("", AgentCertReport{Fingerprint: certFPa})
	if _, ok := real.Get(""); ok {
		t.Fatal("an empty server id was stored")
	}
}

func TestAgentCertReportRegistryCertReportAdapter(t *testing.T) {
	r := NewAgentCertReportRegistry()
	notAfter := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	r.Report("s1", AgentCertReport{Fingerprint: certFPa, CAFingerprints: []string{certFPb}, Mode: "proxy", NotAfter: notAfter})

	fp, cas, mode, na, at, ok := r.CertReport("s1")
	if !ok || fp != certFPa || mode != "proxy" || !na.Equal(notAfter) || at.IsZero() {
		t.Fatalf("CertReport = %q %v %q %v %v %v", fp, cas, mode, na, at, ok)
	}
	if len(cas) != 1 || cas[0] != certFPb {
		t.Fatalf("CertReport ca fingerprints = %v", cas)
	}
}

func TestSanitizeAgentCertReport(t *testing.T) {
	notAfter := time.Date(2026, 9, 9, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		fp         string
		notAfter   time.Time
		mode       string
		cas        []string
		wantFP     string
		wantMode   string
		wantCAs    []string
		wantNoTime bool
	}{
		{
			name: "uppercase and padded fingerprints are normalized",
			fp:   "  " + strings.ToUpper(certFPa) + "  ", notAfter: notAfter, mode: " FILES ",
			cas: []string{strings.ToUpper(certFPb)}, wantFP: certFPa, wantMode: "files", wantCAs: []string{certFPb},
		},
		{
			name: "non-hex fingerprint is dropped",
			fp:   strings.Repeat("z", 64), mode: "files", wantMode: "files", wantNoTime: true,
		},
		{
			name: "wrong-length fingerprint is dropped",
			fp:   certFPa[:63], mode: "files", wantMode: "files", wantNoTime: true,
		},
		{
			name: "unknown mode is dropped",
			fp:   certFPa, mode: "banana", wantFP: certFPa, wantNoTime: true,
		},
		{
			name: "zero not_after stays zero",
			fp:   certFPa, mode: "files", wantFP: certFPa, wantMode: "files", wantNoTime: true,
		},
		{
			name: "bad ca entries are skipped, duplicates collapse",
			fp:   certFPa, mode: "files", cas: []string{certFPb, "nope", certFPb, strings.ToUpper(certFPc)},
			wantFP: certFPa, wantMode: "files", wantCAs: []string{certFPb, certFPc}, wantNoTime: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeAgentCertReport(tc.fp, tc.notAfter, tc.mode, tc.cas)
			if got.Fingerprint != tc.wantFP {
				t.Fatalf("fingerprint = %q, want %q", got.Fingerprint, tc.wantFP)
			}
			if got.Mode != tc.wantMode {
				t.Fatalf("mode = %q, want %q", got.Mode, tc.wantMode)
			}
			if len(got.CAFingerprints) != len(tc.wantCAs) {
				t.Fatalf("ca fingerprints = %v, want %v", got.CAFingerprints, tc.wantCAs)
			}
			for i := range tc.wantCAs {
				if got.CAFingerprints[i] != tc.wantCAs[i] {
					t.Fatalf("ca fingerprints = %v, want %v", got.CAFingerprints, tc.wantCAs)
				}
			}
			if tc.wantNoTime && !got.NotAfter.IsZero() {
				t.Fatalf("not_after = %v, want zero", got.NotAfter)
			}
			if !tc.wantNoTime && !got.NotAfter.Equal(notAfter.UTC()) {
				t.Fatalf("not_after = %v, want %v", got.NotAfter, notAfter.UTC())
			}
		})
	}
}

// A hostile/oversized bundle list is capped, so one agent cannot grow the registry.
func TestSanitizeAgentCertReportCapsCAFingerprints(t *testing.T) {
	many := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		// 64 distinct hex digests: vary the last two characters.
		many = append(many, certFPa[:62]+string("0123456789abcdefghij"[i])+"0")
	}
	got := sanitizeAgentCertReport(certFPa, time.Time{}, "files", many)
	if len(got.CAFingerprints) > maxAgentCertCAFingerprints {
		t.Fatalf("kept %d ca fingerprints, cap is %d", len(got.CAFingerprints), maxAgentCertCAFingerprints)
	}
}
