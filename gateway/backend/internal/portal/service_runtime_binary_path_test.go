// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// binaryPathForm is one absolute-path form and whether the gateway's
// early-feedback mirror must accept it. "Accepted" means: filepath.IsAbs
// returns true for it on at least one of the platforms an agent ships for
// (linux/darwin or windows), so the agent's LocalPolicy.Permit would not
// refuse it for non-absoluteness.
type binaryPathForm struct {
	name string
	path string
	// windowsAbs/posixAbs are the per-platform expectations, taken from Go's
	// own IsAbs test tables (path/filepath/path_test.go: isabstests and
	// winisabstests) so this table is anchored to the authority rather than
	// to memory.
	posixAbs   bool
	windowsAbs bool
}

// binaryPathForms is the shared table for every test in this file.
//
// The windowsAbs column reproduces Go's winisabstests verbatim where the
// forms overlap; the posixAbs column is filepath.IsAbs's unix rule (a leading
// slash) applied to the same strings.
var binaryPathForms = []binaryPathForm{
	// --- POSIX absolute -----------------------------------------------------
	{name: "posix root", path: `/`, posixAbs: true},
	{name: "posix binary", path: `/opt/llama/llama-server`, posixAbs: true},
	{name: "posix binary with dotdot", path: `/a/../bb`, posixAbs: true},

	// --- Windows absolute ---------------------------------------------------
	// The reported symptom: the operator's own path from the bug report.
	{name: "drive letter backslash (bug report)", path: `C:\llama\llama-server.exe`, windowsAbs: true},
	{name: "drive letter forward slash", path: `c:/llama/llama-server.exe`, windowsAbs: true},
	{name: "drive root", path: `C:\`, windowsAbs: true},
	{name: "unc share", path: `\\host\share`, windowsAbs: true},
	{name: "unc share trailing separator", path: `\\host\share\`, windowsAbs: true},
	{name: "unc path", path: `\\host\share\llama\llama-server.exe`, windowsAbs: true},
	{name: "root local device", path: `\\?\a\b\c`, windowsAbs: true},
	{name: "nt object path", path: `\??\a\b\c`, windowsAbs: true},
	// A UNC path spelled with forward slashes is absolute under BOTH rules
	// (it starts with a slash, and Windows treats / as a separator). It must
	// therefore never be reported as contradicting either OS.
	{name: "unc share forward slashes", path: `//host/share/foo/bar`, posixAbs: true, windowsAbs: true},

	// --- absolute nowhere ---------------------------------------------------
	{name: "empty", path: ``},
	{name: "whitespace only", path: `   `},
	{name: "relative", path: `bin/llama-server`},
	{name: "relative dot", path: `./llama-server`},
	{name: "relative dotdot", path: `..`},
	{name: "bare name", path: `llama-server.exe`},
	// Drive-RELATIVE: resolved against the drive's own current directory,
	// which a freshly spawned process does not meaningfully have.
	{name: "drive relative", path: `c:llama\llama-server.exe`},
	{name: "bare drive", path: `c:`},
	{name: "double colon", path: `c::`},
	{name: "drive letter no separator", path: `c\`},
	// Root-RELATIVE: rooted on the CURRENT drive, so it names no volume.
	{name: "root relative backslash", path: `\`},
	{name: "root relative", path: `\Windows\llama-server.exe`},
}

func (f binaryPathForm) accepted() bool { return f.posixAbs || f.windowsAbs }

// TestBinaryPathAbsolutenessMirrorsGoSemantics pins the per-platform helpers
// against the table above, then cross-checks the POSIX half against the real
// filepath.IsAbs on any non-windows host (which is every CI runner and every
// supported gateway deployment).
func TestBinaryPathAbsolutenessMirrorsGoSemantics(t *testing.T) {
	for _, form := range binaryPathForms {
		t.Run(form.name, func(t *testing.T) {
			if got := isAbsPOSIXBinaryPath(form.path); got != form.posixAbs {
				t.Errorf("isAbsPOSIXBinaryPath(%q) = %v, want %v", form.path, got, form.posixAbs)
			}
			if got := isAbsWindowsBinaryPath(form.path); got != form.windowsAbs {
				t.Errorf("isAbsWindowsBinaryPath(%q) = %v, want %v", form.path, got, form.windowsAbs)
			}
			if got := runtimeSpecBinaryIsAbsolute(form.path); got != form.accepted() {
				t.Errorf("runtimeSpecBinaryIsAbsolute(%q) = %v, want %v", form.path, got, form.accepted())
			}
			if runtime.GOOS != "windows" {
				// The authority itself, for the half of the table this host
				// can evaluate.
				if got := filepath.IsAbs(form.path); got != form.posixAbs {
					t.Errorf("filepath.IsAbs(%q) = %v on %s, want %v -- the POSIX expectation in this table is wrong", form.path, got, runtime.GOOS, form.posixAbs)
				}
			}
		})
	}
}

// TestPutRuntimeSpecAcceptsWindowsAbsolutePaths is the regression test for the
// reported defect: the gateway validated `binary` with a POSIX-only
// `strings.HasPrefix(binary, "/")`, so a Windows AI server -- a platform the
// gateway ships windows-amd64/windows-arm64 agent binaries for -- could not
// be configured through the portal at all.
//
// It drives the real PutRuntimeSpec (not just the helper) so the accepted
// path is also persisted and round-tripped verbatim.
func TestPutRuntimeSpecAcceptsWindowsAbsolutePaths(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)

	for i, form := range binaryPathForms {
		t.Run(form.name, func(t *testing.T) {
			// One mapping per case: PutRuntimeSpec is a full-document upsert
			// per mapping, and a shared mapping would let one case's stored
			// spec colour the next one's read-then-upsert.
			mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{
				GatewayModelName: "m" + strconv.Itoa(i),
				AppModelName:     "m" + strconv.Itoa(i),
			})
			if err != nil {
				t.Fatalf("CreateMapping: %v", err)
			}
			dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{Binary: form.path})
			if !form.accepted() {
				if !errors.Is(err, ErrRuntimeSpecBinaryRequired) {
					t.Fatalf("PutRuntimeSpec(%q) err = %v, want ErrRuntimeSpecBinaryRequired", form.path, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("PutRuntimeSpec(%q): %v", form.path, err)
			}
			// Stored verbatim (modulo the pre-existing TrimSpace): the
			// gateway must never rewrite separators or case in a path the
			// agent will hand to os/exec.
			if dto.Binary != form.path {
				t.Fatalf("stored binary = %q, want %q", dto.Binary, form.path)
			}
		})
	}
}

// TestPutRuntimeSpecTrimsWindowsBinaryWhitespace pins that the pre-existing
// TrimSpace still applies to a Windows path: a value pasted into the portal
// form with a stray trailing space must be accepted and stored trimmed, not
// refused.
func TestPutRuntimeSpecTrimsWindowsBinaryWhitespace(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{Binary: "  C:\\llama\\llama-server.exe  "})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if dto.Binary != `C:\llama\llama-server.exe` {
		t.Fatalf("stored binary = %q, want %q", dto.Binary, `C:\llama\llama-server.exe`)
	}
}

// --- the advisory half: path form vs the OS the agent reports ---------------

// TestRuntimeSpecBinaryContradictsOS is the unit-level truth table for the
// mismatch predicate, over every form and every OS shape that matters.
func TestRuntimeSpecBinaryContradictsOS(t *testing.T) {
	cases := []struct {
		binary     string
		reportedOS string
		want       bool
	}{
		// Windows path on a linux/darwin-reporting server -> mismatch.
		{binary: `C:\llama\llama-server.exe`, reportedOS: "linux", want: true},
		{binary: `c:/llama/llama-server.exe`, reportedOS: "linux", want: true},
		{binary: `\\host\share\llama.exe`, reportedOS: "linux", want: true},
		{binary: `C:\llama\llama-server.exe`, reportedOS: "darwin", want: true},
		// POSIX path on a windows-reporting server -> mismatch.
		{binary: `/opt/llama/llama-server`, reportedOS: "windows", want: true},
		{binary: `/`, reportedOS: "windows", want: true},
		// Matching platform -> silent.
		{binary: `/opt/llama/llama-server`, reportedOS: "linux", want: false},
		{binary: `C:\llama\llama-server.exe`, reportedOS: "windows", want: false},
		// Absolute under BOTH rules -> silent on either OS.
		{binary: `//host/share/llama`, reportedOS: "linux", want: false},
		{binary: `//host/share/llama`, reportedOS: "windows", want: false},
		// Never reported -> silent, whatever the path looks like.
		{binary: `C:\llama\llama-server.exe`, reportedOS: "", want: false},
		{binary: `/opt/llama/llama-server`, reportedOS: "", want: false},
		// An empty binary is refused by PutRuntimeSpec, never advised on.
		{binary: ``, reportedOS: "windows", want: false},
		{binary: ``, reportedOS: "linux", want: false},
	}
	for _, tc := range cases {
		got := runtimeSpecBinaryContradictsOS(tc.binary, tc.reportedOS)
		if got != tc.want {
			t.Errorf("runtimeSpecBinaryContradictsOS(%q, %q) = %v, want %v", tc.binary, tc.reportedOS, got, tc.want)
		}
	}
}

// TestRuntimeWarningsBinaryPathOSMismatch drives the real warnings endpoint
// through all three states an operator can be in: no telemetry at all, a
// contradicting OS, and a matching OS.
func TestRuntimeWarningsBinaryPathOSMismatch(t *testing.T) {
	cases := []struct {
		name   string
		binary string
		// reportedOS "" means: never upsert a telemetry row at all.
		reportedOS string
		want       bool
	}{
		{name: "windows path on linux server", binary: `C:\llama\llama-server.exe`, reportedOS: "linux", want: true},
		{name: "posix path on windows server", binary: `/opt/llama/llama-server`, reportedOS: "windows", want: true},
		{name: "windows path on windows server", binary: `C:\llama\llama-server.exe`, reportedOS: "windows", want: false},
		{name: "posix path on linux server", binary: `/opt/llama/llama-server`, reportedOS: "linux", want: false},
		{name: "windows path, agent never reported", binary: `C:\llama\llama-server.exe`, reportedOS: "", want: false},
		{name: "posix path, agent never reported", binary: `/opt/llama/llama-server`, reportedOS: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
			ctx := context.Background()
			svc, routeStore := newServerTestService(t, now)
			server := createTestServer(t, svc, "S", "s.example.test")
			app := seedServerAgentApplication(t, routeStore, server.ID, now)
			mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
			if err != nil {
				t.Fatalf("CreateMapping: %v", err)
			}
			if tc.reportedOS != "" {
				if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{
					ServerID: server.ID, ReportedAt: now, AgentVersion: "0.2.0", OS: tc.reportedOS,
					Arch: "amd64", ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now,
				}); err != nil {
					t.Fatalf("UpsertTelemetry: %v", err)
				}
			}
			// Enabled:false on purpose -- the mismatch advisory must fire for
			// a spec that has not been enabled yet, which is the state an
			// operator is in while filling the form out.
			if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{Binary: tc.binary, Enabled: false}); err != nil {
				t.Fatalf("PutRuntimeSpec: %v", err)
			}
			warnings, err := svc.RuntimeWarnings(ctx, ownerToken(), app.ID)
			if err != nil {
				t.Fatalf("RuntimeWarnings: %v", err)
			}
			got := false
			for _, w := range warnings {
				if w == "binary_path_os_mismatch" {
					got = true
				}
			}
			if got != tc.want {
				t.Fatalf("warnings = %#v, binary_path_os_mismatch present = %v, want %v", warnings, got, tc.want)
			}
		})
	}
}
