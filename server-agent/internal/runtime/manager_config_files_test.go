// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Manager-level tests for the config_files facet (#257): the write-before-launch
// step in startProcess and how each kind of failure is classified. They use the
// same real stubchild process as the rest of the manager suite.

// readFileLog polls a stubchild -read-log file until the child has written it
// during its own startup (a read racing the exec would flake).
func readFileLog(t *testing.T, path string) string {
	t.Helper()
	var record string
	waitUntil(t, 5*time.Second, "child to write its read log at "+path, func() bool {
		data, err := os.ReadFile(path)
		if err != nil || len(data) == 0 {
			return false
		}
		record = string(data)
		return true
	})
	return record
}

// configFileStubArgs is stubArgs plus the pair that makes the CHILD report what
// ${CONFIG_DIR}/<name> held at the moment it started.
func configFileStubArgs(name, readLog string) []string {
	return append(stubArgs(0, 0, 0, ""), "-read-file", "${CONFIG_DIR}/"+name, "-read-log", readLog)
}

// listFiles returns every regular file under root, for "nothing was written"
// assertions that must also catch a write that escaped its base.
func listFiles(root string) []string {
	var found []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			found = append(found, path)
		}
		return nil
	})
	return found
}

// TestManagerWritesConfigFilesBeforeLaunch drives the whole path -- NewManager
// with ManagerOptions.ConfigFilesDir -> startProcess -> exec -- and asserts on
// what the CHILD found when it started. The base directory is supplied ONLY
// through ManagerOptions, so cutting the opts.ConfigFilesDir -> owner assignment
// leaves the owner with an empty base: the spec is refused (and nothing lands
// in the right place), and this test goes red.
//
// It also proves the ordering (the file is complete before exec, with ${PORT}
// and ${MODEL} already substituted), that ${CONFIG_DIR} in Args names the same
// directory the file was written to, and the 0600 mode.
func TestManagerWritesConfigFilesBeforeLaunch(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)

	base := t.TempDir()
	readLog := filepath.Join(t.TempDir(), "read")
	spec := baseSpec("spec-a", "model-a")
	spec.ConfigFiles = []ConfigFile{{Name: "server.json", Content: `{"id":"${MODEL}","port":${PORT}}`}}
	spec.Args = configFileStubArgs("server.json", readLog)

	m := NewManager(ManagerOptions{
		Policy:         allowlistPolicy(),
		Getenv:         func(string) string { return "" },
		ConfigFilesDir: base,
	})
	t.Cleanup(m.Close)
	m.Apply(Config{Specs: []Spec{spec}})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	endpoint, release, err := m.EnsureRunning(ctx, "model-a")
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	defer release()

	port := strings.TrimPrefix(endpoint, "http://127.0.0.1:")
	want := `{"id":"model-a","port":` + port + `}`
	if got := readFileLog(t, readLog); got != "content:"+want {
		t.Errorf("the child found %q at ${CONFIG_DIR}/server.json when it started, want %q (written before exec, placeholders expanded)", got, "content:"+want)
	}

	path := filepath.Join(base, "spec-a", "server.json")
	if got, err := os.ReadFile(path); err != nil || string(got) != want {
		t.Errorf("%s = %q (err %v), want %q", path, got, err, want)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("%s stat = %v (err %v), want mode 600", path, info, err)
	}
}

// TestManagerConfigFilesWriteFailureIsStartFailed: a genuine filesystem failure
// is a start failure -- counted, backed off, retried -- and NOT a not_permitted
// verdict. A transient disk problem can heal on retry; a bad config cannot, and
// the two must not be reported as each other.
func TestManagerConfigFilesWriteFailureIsStartFailed(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	setBackoffWindow(t, 5*time.Second) // wide enough to observe the backoff state

	// A base UNDER a regular file: MkdirAll fails for every user, root included.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	invocationLog := filepath.Join(t.TempDir(), "invocations.log")
	spec := baseSpec("spec-a", "model-a")
	spec.ConfigFiles = []ConfigFile{{Name: "server.json", Content: "{}"}}
	spec.Args = stubArgs(0, 0, 0, invocationLog)

	m := NewManager(ManagerOptions{
		Policy:         allowlistPolicy(),
		Getenv:         func(string) string { return "" },
		ConfigFilesDir: filepath.Join(blocker, "configs"),
	})
	t.Cleanup(m.Close)
	m.Apply(Config{Specs: []Spec{spec}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := m.EnsureRunning(ctx, "model-a")
	if !errors.Is(err, ErrStartFailed) {
		t.Fatalf("EnsureRunning error = %v, want ErrStartFailed", err)
	}
	if errors.Is(err, ErrNotPermitted) {
		t.Fatalf("EnsureRunning error = %v: a filesystem failure must not read as not_permitted", err)
	}
	st := statusFor(m, "spec-a")
	if st == nil || st.State != StateBackoff {
		t.Fatalf("Status()[spec-a] = %+v, want backoff (start_failed -> rate-limited retry)", st)
	}
	if st.LastError == nil || st.LastError.Failures != 1 || !strings.Contains(st.LastError.Message, "config file") {
		t.Errorf("LastError = %+v, want one counted failure whose message names the config files", st.LastError)
	}
	if n := countInvocations(t, invocationLog); n != 0 {
		t.Errorf("the child was exec'd %d times although its config files could not be written", n)
	}
}

// TestManagerConfigFileFaultsAreNotPermitted: the DETERMINISTIC config faults --
// a secret or malformed placeholder in content, an OS-unsafe file name, a spec
// ID that escapes the base -- are caught by the pre-port dry run and surface
// exactly like a bad Args/Env placeholder: not_permitted (rate-limited), no
// counted failure, no backoff, no child, nothing written.
func TestManagerConfigFileFaultsAreNotPermitted(t *testing.T) {
	cases := []struct {
		name    string
		specID  string
		file    ConfigFile
		message string
	}{
		{"api token in content", "spec-a", ConfigFile{Name: "a.json", Content: `{"k":"${API_TOKEN}"}`}, "API_TOKEN"},
		{"agent env in content", "spec-a", ConfigFile{Name: "a.json", Content: `{"k":"${AGENT_ENV:HF_TOKEN}"}`}, "AGENT_ENV"},
		{"malformed placeholder in content", "spec-a", ConfigFile{Name: "a.json", Content: `port=${PORTX}`}, "PORTX"},
		{"windows device name", "spec-a", ConfigFile{Name: "CON", Content: "x"}, "CON"},
		{"trailing dot", "spec-a", ConfigFile{Name: "a.json.", Content: "x"}, "a.json."},
		{"trailing space", "spec-a", ConfigFile{Name: "a.json ", Content: "x"}, "a.json"},
		{"bidi override in name", "spec-a", ConfigFile{Name: "a\u202ejson", Content: "x"}, "format character"},
		{"spec id escapes the base", "../evil", ConfigFile{Name: "a.json", Content: "x"}, "outside"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skipOnWindows(t)
			shrinkTimings(t)
			origInterval := notPermittedRetryInterval
			notPermittedRetryInterval = 2 * time.Second
			defer func() { notPermittedRetryInterval = origInterval }()

			root := t.TempDir()
			invocationLog := filepath.Join(t.TempDir(), "invocations.log")
			spec := baseSpec(tc.specID, "model-a")
			spec.ConfigFiles = []ConfigFile{tc.file}
			spec.Args = stubArgs(0, 0, 0, invocationLog)

			m := NewManager(ManagerOptions{
				Policy:         allowlistPolicy(),
				Getenv:         func(string) string { return "shh" }, // a secret placeholder must still be refused
				ConfigFilesDir: filepath.Join(root, "base"),
			})
			t.Cleanup(m.Close)
			m.Apply(Config{Specs: []Spec{spec}})

			for i := 0; i < 3; i++ { // rapid requests inside the retry interval see the cached verdict
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, _, err := m.EnsureRunning(ctx, "model-a")
				cancel()
				if !errors.Is(err, ErrNotPermitted) {
					t.Fatalf("attempt %d: EnsureRunning error = %v, want ErrNotPermitted", i, err)
				}
			}
			st := statusFor(m, tc.specID)
			if st == nil || st.State != StateNotPermitted {
				t.Fatalf("Status()[%s] = %+v, want not_permitted", tc.specID, st)
			}
			if st.LastError == nil || !strings.Contains(st.LastError.Message, tc.message) {
				t.Errorf("LastError = %+v, want a message containing %q", st.LastError, tc.message)
			}
			if st.LastError != nil && st.LastError.Failures != 0 {
				t.Errorf("LastError.Failures = %d, want 0: a deterministic config fault is not a counted start failure", st.LastError.Failures)
			}
			if n := countInvocations(t, invocationLog); n != 0 {
				t.Errorf("the child was exec'd %d times for a refused spec", n)
			}
			if found := listFiles(root); len(found) != 0 {
				t.Errorf("files were written for a refused spec: %v", found)
			}
		})
	}
}

// TestSpecWithoutConfigFilesNeedsNoConfigFilesDir: the facet is opt-in. A spec
// with no config_files launches on a manager that has no base directory at all
// -- the posture of every spec that predates the facet.
func TestSpecWithoutConfigFilesNeedsNoConfigFilesDir(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy()) // ConfigFilesDir unset

	m.Apply(Config{Specs: []Spec{baseSpec("spec-a", "model-a")}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, release, err := m.EnsureRunning(ctx, "model-a")
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	release()
}

// TestConfigFileContentChangeForcesRelaunch: config_files is NOT on
// sameLaunchShape's metadata strip list, so a content-only edit reads as a
// different launch shape and a running child is relaunched (the child reads its
// files at start, so a stale child would otherwise keep the old content).
func TestConfigFileContentChangeForcesRelaunch(t *testing.T) {
	base := Spec{ID: "s", Binary: "/bin/x", ConfigFiles: []ConfigFile{{Name: "server.json", Content: "v1"}}}
	if !sameLaunchShape(base, base) {
		t.Fatal("a spec must have the same launch shape as itself")
	}
	mutations := map[string]func(*Spec){
		"content only": func(s *Spec) { s.ConfigFiles = []ConfigFile{{Name: "server.json", Content: "v2"}} },
		"name only":    func(s *Spec) { s.ConfigFiles = []ConfigFile{{Name: "other.json", Content: "v1"}} },
		"file added":   func(s *Spec) { s.ConfigFiles = append([]ConfigFile{{Name: "b.json", Content: "x"}}, s.ConfigFiles...) },
		"file removed": func(s *Spec) { s.ConfigFiles = []ConfigFile{} },
	}
	for name, mut := range mutations {
		changed := base
		changed.ConfigFiles = append([]ConfigFile(nil), base.ConfigFiles...)
		mut(&changed)
		if sameLaunchShape(base, changed) {
			t.Errorf("config_files change (%s) must force a relaunch, but sameLaunchShape returned true", name)
		}
	}
}

// TestApplyRelaunchesRunningChildOnConfigFileContentChange is the end-to-end
// half: a running pinned child whose ONLY change is a config file's content is
// relaunched, and the new child finds the new content on disk at start. Both
// generations report to the same read log (each start overwrites it), so the
// log reading v2 can only mean a second generation started.
func TestApplyRelaunchesRunningChildOnConfigFileContentChange(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)

	readLog := filepath.Join(t.TempDir(), "read")
	m := NewManager(ManagerOptions{
		Policy:         allowlistPolicy(),
		Getenv:         func(string) string { return "" },
		ConfigFilesDir: t.TempDir(),
	})
	t.Cleanup(m.Close)

	spec := baseSpec("spec-a", "model-a")
	spec.Pinned = true
	spec.ConfigFiles = []ConfigFile{{Name: "server.json", Content: "v1"}}
	spec.Args = configFileStubArgs("server.json", readLog)
	m.Apply(Config{ETag: "e1", Specs: []Spec{spec}})
	waitUntil(t, 5*time.Second, "spec-a running on v1", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateRunning
	})
	if got := readFileLog(t, readLog); got != "content:v1" {
		t.Fatalf("first generation read %q, want content:v1", got)
	}

	changed := spec
	changed.ConfigFiles = []ConfigFile{{Name: "server.json", Content: "v2"}}
	m.Apply(Config{ETag: "e2", Specs: []Spec{changed}})

	waitUntil(t, 10*time.Second, "a relaunched generation to read v2", func() bool {
		data, err := os.ReadFile(readLog)
		return err == nil && string(data) == "content:v2"
	})
}
