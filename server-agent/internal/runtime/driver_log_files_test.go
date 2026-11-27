// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Driver honours a spec's log_files only while the gateway declares
// runtime_log_files (#430): an older gateway decodes runtime_log frames without
// their stream, so a file stream would reach its portal mixed into the process
// output. Only file mode can carry log_files to such a gateway's agent -- an
// older gateway never puts them in a document of its own -- but the gate is the
// same in both modes, like rpc_cluster's.

// withheldLogFilesWarn is noteLogFilesWithheld's exact Warn message.
const withheldLogFilesWarn = "runtime: the gateway does not declare runtime_log_files; declared log files are not tailed, from each spec's next start, until it does"

// TestDriverClearsLogFilesWithoutTheGatewayDeclaration pins what the Driver
// hands the manager: without runtime_log_files on the gateway's list every
// spec arrives with its log_files cleared (the source's cached document keeps
// them: a copy is cleared); with it, the document arrives as loaded. The
// declaration alone re-applies the unchanged document, its withdrawal clears
// them again, and the Warn is logged once per transition into that state.
func TestDriverClearsLogFilesWithoutTheGatewayDeclaration(t *testing.T) {
	logs := withCountedLogs(t)
	declared := []LogFile{{Name: "engine", Path: "${CONFIG_DIR}/engine.log"}}
	withFiles := Spec{ID: "S", UpstreamModel: "m", LogFiles: declared}
	plain := Spec{ID: "P", UpstreamModel: "p"}
	src := &fakeSource{cfg: Config{ETag: "e1", Specs: []Spec{withFiles, plain}}}
	mgr := &fakeManager{}
	base := []string{runtimeManagerFeature, gatewayFeatureRuntimeInstances, gatewayFeatureRPCCluster}
	features, setFeatures := newSwitchableFeaturesServer(t, base)
	d := newDriver(mgr, src, features, nil, "")
	ctx := context.Background()

	d.Sync(ctx, nil)
	if n := mgr.applyCount(); n != 1 {
		t.Fatalf("Apply calls = %d after the first Sync, want 1", n)
	}
	got := mgr.lastApplied()
	if len(got.Specs) != 2 || got.Specs[0].LogFiles != nil || got.Specs[1].LogFiles != nil {
		t.Fatalf("applied specs = %+v, want both specs with log_files cleared", got.Specs)
	}
	if got.Specs[0].ID != "S" || got.Specs[0].UpstreamModel != "m" || got.ETag != "e1" {
		t.Fatalf("applied = %+v, want the document otherwise as loaded", got)
	}
	if len(src.cfg.Specs[0].LogFiles) != 1 {
		t.Fatal("the source's cached document lost its log_files: the Driver must clear a copy")
	}
	if n := logs.count(withheldLogFilesWarn); n != 1 {
		t.Fatalf("withheld Warns = %d after the first apply, want 1", n)
	}

	d.Sync(ctx, nil) // nothing changed: nothing applied
	if n := mgr.applyCount(); n != 1 {
		t.Fatalf("Apply calls = %d after an unchanged Sync, want still 1", n)
	}

	setFeatures(append(append([]string(nil), base...), gatewayFeatureRuntimeLogFiles))
	d.Sync(ctx, nil)
	if n := mgr.applyCount(); n != 2 {
		t.Fatalf("Apply calls = %d once the gateway declares runtime_log_files, want 2: the declaration alone re-applies the document", n)
	}
	if got := mgr.lastApplied(); len(got.Specs[0].LogFiles) != 1 || got.Specs[0].LogFiles[0] != declared[0] || got.Specs[1].LogFiles != nil {
		t.Fatalf("applied specs = %+v, want the document as loaded (S declares engine, P nothing)", got.Specs)
	}
	d.Sync(ctx, nil)
	if n := mgr.applyCount(); n != 2 {
		t.Fatalf("Apply calls = %d after an unchanged Sync, want still 2", n)
	}

	setFeatures(base)
	d.Sync(ctx, nil)
	if n := mgr.applyCount(); n != 3 {
		t.Fatalf("Apply calls = %d after the declaration is withdrawn, want 3", n)
	}
	if got := mgr.lastApplied(); got.Specs[0].LogFiles != nil {
		t.Fatalf("applied specs = %+v, want log_files cleared again", got.Specs)
	}
	if n := logs.count(withheldLogFilesWarn); n != 2 {
		t.Fatalf("withheld Warns = %d after a withdrawal, want 2", n)
	}
}

// TestDriverWithoutRuntimeLogFilesTailsNothing is the finding's scenario end to
// end, in file mode: the operator's runtime.json declares a ${CONFIG_DIR} log
// file, but the gateway does not declare runtime_log_files (an older gateway).
// The spec runs -- never healthy and crash-looping here, so that several
// generations start and every one leaves a last_error -- and the
// declaration is as absent: the file an earlier run left is not removed
// before exec, nothing of it reaches a file stream (so nothing can reach the
// process view either), and no failure record gains log_file_tails. The
// gateway then declares it: the next Sync applies the SAME document under the
// same ETag with the declaration honoured, and from the next generation start
// on the file is removed before exec, tailed into its own stream, and its end
// attached to the crash's last_error, exactly as without the gate.
func TestDriverWithoutRuntimeLogFilesTailsNothing(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	setLogFileGrace(t, 300*time.Millisecond)
	logs := withCountedLogs(t)
	base := t.TempDir()
	logPath := filepath.Join(base, "spec-gate", "engine.log")
	const leftover = "left over from before\n"
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(leftover), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newLogFileManager(t, allowlistPolicy(), base)

	spec := logFileSpec("spec-gate", "gate-model", "-log-text", `run {pid}\n`,
		"-health-delay", "1m", "-crash-after", "300ms", "-exit-code", "1")
	raw, err := json.Marshal(Config{ETag: "file-v1", MaxProcesses: 1, Specs: []Spec{spec}})
	if err != nil {
		t.Fatal(err)
	}
	docPath := filepath.Join(t.TempDir(), "runtime.json")
	if err := os.WriteFile(docPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	without := []string{runtimeManagerFeature, gatewayFeatureRuntimeInstances, gatewayFeatureRPCCluster}
	features, setFeatures := newSwitchableFeaturesServer(t, without)
	d := NewDriver(m, NewFileSource(docPath), features, nil, "")
	t.Cleanup(d.Close)
	ctx := context.Background()

	d.Sync(ctx, nil)
	d.Sync(ctx, nil)
	// Two generations have written their line: the second start would have
	// ended the first's grace and attached its tails, had it had any tailer.
	var pids []int
	waitUntil(t, 10*time.Second, "two generations wrote their line to the undeclared file", func() bool {
		_, pids = startedGens(m, "spec-gate")
		if len(pids) < 2 {
			return false
		}
		data, err := os.ReadFile(logPath)
		return err == nil && strings.Contains(string(data), "run "+strconv.Itoa(pids[1])+"\n")
	})
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), leftover+"run "+strconv.Itoa(pids[0])+"\n") {
		t.Errorf("log file = %q, want the earlier run's text kept and every generation appending: nothing removes it while the declaration is withheld", data)
	}
	if recs := streamRecords(m, "spec-gate", "engine"); len(recs) != 0 {
		t.Errorf("engine stream = %+v, want none: no file is tailed without runtime_log_files", recs)
	}
	waitUntil(t, 10*time.Second, "a crash recorded in last_error", func() bool {
		s := statusFor(m, "spec-gate")
		return s != nil && s.LastError != nil
	})
	time.Sleep(time.Second) // beyond the grace: tails would have been attached by now
	if s := statusFor(m, "spec-gate"); s == nil || s.LastError == nil || len(s.LastError.LogFileTails) != 0 {
		t.Errorf("status = %+v, want a last_error without log_file_tails", s)
	}
	for _, r := range streamRecords(m, "spec-gate", "") {
		if strings.Contains(r.text, "run ") {
			t.Errorf("the file's text reached the process stream: %+v", r)
		}
	}
	if n := logs.count(withheldLogFilesWarn); n != 1 {
		t.Errorf("withheld Warns = %d after two Syncs, want 1 (once per transition)", n)
	}

	setFeatures(append(append([]string(nil), without...), gatewayFeatureRuntimeLogFiles))
	d.Sync(ctx, nil)
	if got := m.AppliedETag(); got != "file-v1" {
		t.Errorf("AppliedETag() = %q, want the document's own file-v1", got)
	}
	var gens []uint64
	waitUntil(t, 15*time.Second, "a later generation's own line in the engine stream", func() bool {
		gens, pids = startedGens(m, "spec-gate")
		recs := streamRecords(m, "spec-gate", "engine")
		for i := range gens {
			if genText(recs, gens[i]) == "run "+strconv.Itoa(pids[i])+"\n" {
				return true
			}
		}
		return false
	})
	recs := streamRecords(m, "spec-gate", "engine")
	for i := range gens {
		text := genText(recs, gens[i])
		if text == "" {
			continue
		}
		if want := "run " + strconv.Itoa(pids[i]) + "\n"; text != want {
			t.Errorf("generation %d engine text = %q, want exactly its own %q: the file is removed before exec once declared", i+1, text, want)
		}
	}
	le := waitForLastError(t, m, "spec-gate", "last_error with log_file_tails once declared", func(le *LastError) bool {
		return len(le.LogFileTails) > 0
	})
	if len(le.LogFileTails) != 1 || le.LogFileTails[0].Name != "engine" || !strings.HasPrefix(le.LogFileTails[0].Tail, "run ") {
		t.Errorf("log_file_tails = %+v, want the engine file's own run line", le.LogFileTails)
	}
}

// TestSameLogFileDeclarations pins the manager's same-ETag short circuit for
// log_files: a document arriving again under the applied ETag is a no-op only
// while every spec declares the same log files, so the Driver's re-application
// on a change of runtime_log_files is taken.
func TestSameLogFileDeclarations(t *testing.T) {
	engine := []LogFile{{Name: "engine", Path: "${CONFIG_DIR}/engine.log"}}
	other := []LogFile{{Name: "engine", Path: "${CONFIG_DIR}/other.log"}}
	two := []LogFile{engine[0], {Name: "access", Path: "${CONFIG_DIR}/access.log"}}
	spec := func(id string, files []LogFile) Spec { return Spec{ID: id, LogFiles: files} }
	cases := []struct {
		name string
		a, b []Spec
		want bool
	}{
		{"none on either side", []Spec{spec("S", nil)}, []Spec{spec("S", nil)}, true},
		{"the same declaration", []Spec{spec("S", engine), spec("P", nil)}, []Spec{spec("P", nil), spec("S", engine)}, true},
		{"withheld then honoured", []Spec{spec("S", nil)}, []Spec{spec("S", engine)}, false},
		{"honoured then withheld", []Spec{spec("S", engine)}, []Spec{spec("S", nil)}, false},
		{"another path", []Spec{spec("S", engine)}, []Spec{spec("S", other)}, false},
		{"one more file", []Spec{spec("S", engine)}, []Spec{spec("S", two)}, false},
		{"another spec declares it", []Spec{spec("S", engine), spec("P", nil)}, []Spec{spec("S", nil), spec("P", engine)}, false},
		{"empty and nil alike", []Spec{spec("S", []LogFile{})}, []Spec{spec("S", nil)}, true},
	}
	for _, c := range cases {
		if got := sameLogFileDeclarations(c.a, c.b); got != c.want {
			t.Errorf("%s: sameLogFileDeclarations = %v, want %v", c.name, got, c.want)
		}
		if got := sameLogFileDeclarations(c.b, c.a); got != c.want {
			t.Errorf("%s (swapped): sameLogFileDeclarations = %v, want %v", c.name, got, c.want)
		}
	}
}
