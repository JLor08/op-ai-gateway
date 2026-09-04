// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// TestPermitEmptyAllowlistRejectsEverything pins spec decision 2: an operator
// who has configured no allowed binaries at all gets a hard refusal for
// EVERY spec, not a permissive default. The error must name the allowlist
// (so the operator understands WHY, not just THAT it failed) and must name
// the refused binary.
//
// A TABLE, not one spec: "rejects everything" is a claim about the whole
// input space, and a single well-formed spec cannot support it. The rows
// below vary every field that any OTHER branch of Permit looks at -- binary
// shape (absolute, relative, empty, one that would be allowed under a
// configured list), work_dir (set, empty, traversal-shaped), and the
// AllowedDirs half of the policy -- so a future edit that accidentally lets
// any of them reach a permit decision before the empty-allowlist gate fails
// here instead of shipping.
func TestPermitEmptyAllowlistRejectsEverything(t *testing.T) {
	cases := []struct {
		name string
		p    LocalPolicy
		spec Spec
	}{
		{"absolute binary, work_dir set", LocalPolicy{}, Spec{ID: "s1", Binary: "/usr/bin/ollama", WorkDir: "/srv/models"}},
		{"absolute binary, no work_dir", LocalPolicy{}, Spec{ID: "s2", Binary: "/usr/bin/ollama"}},
		{"relative binary", LocalPolicy{}, Spec{ID: "s3", Binary: "ollama"}},
		{"empty binary", LocalPolicy{}, Spec{ID: "s4", Binary: ""}},
		{"traversal-shaped work_dir", LocalPolicy{}, Spec{ID: "s5", Binary: "/usr/bin/ollama", WorkDir: "/srv/models/../../etc"}},
		{"AllowedDirs configured but AllowedBinaries empty", LocalPolicy{AllowedDirs: []string{"/srv/models"}}, Spec{ID: "s6", Binary: "/usr/bin/ollama", WorkDir: "/srv/models"}},
		{"nil AllowedBinaries slice, work_dir inside an allowed dir", LocalPolicy{AllowedBinaries: nil, AllowedDirs: []string{"/srv/models"}}, Spec{ID: "s7", Binary: "/usr/bin/vllm", WorkDir: "/srv/models/llama3"}},
		{"pinned, force_running spec", LocalPolicy{}, Spec{ID: "s8", Binary: "/usr/bin/ollama", WorkDir: "/srv/models", Pinned: true, AdminState: "force_running"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Permit(tc.spec)
			if err == nil {
				t.Fatalf("Permit(%+v) with an empty allowlist = nil, want a refusal", tc.spec)
			}
			if !strings.Contains(err.Error(), "allowlist") {
				t.Errorf("Permit error = %q, want it to mention the allowlist (the operator needs to know WHY, not just THAT)", err.Error())
			}
			if !strings.Contains(err.Error(), tc.spec.Binary) {
				t.Errorf("Permit error = %q, want it to name the refused binary %q", err.Error(), tc.spec.Binary)
			}
			if !strings.Contains(err.Error(), "OP_AGENT_RUNTIME_ALLOWED_BINARIES") {
				t.Errorf("Permit error = %q, want it to name the setting an operator must configure", err.Error())
			}
		})
	}
}

// TestPermitEmptyWorkDirDefaultsToBinaryDir is R2's Permit half, and the
// inversion of the old TestPermitEmptyWorkDirHasItsOwnMessage (which asserted
// an empty work_dir under a configured AllowedDirs was REFUSED). An empty
// work_dir now means "run beside the binary"; the binary's own directory is
// trusted by construction because the binary is on the exact AllowedBinaries
// allowlist, so it is permitted UNCONDITIONALLY -- even when AllowedDirs is
// configured and does NOT contain the binary's directory. This stands alone,
// with no dependency on runtime_allow_binary_dirs (R3): the binary dir needs no
// auto-allow plumbing to be trusted.
func TestPermitEmptyWorkDirDefaultsToBinaryDir(t *testing.T) {
	// AllowedDirs is configured and deliberately does NOT include /usr/bin
	// (where the allowlisted binary lives): the empty work_dir must still pass.
	p := LocalPolicy{
		AllowedBinaries: []string{"/usr/bin/ollama"},
		AllowedDirs:     []string{"/srv/models", "/data/weights"},
	}
	if err := p.Permit(Spec{ID: "s1", Binary: "/usr/bin/ollama", WorkDir: ""}); err != nil {
		t.Fatalf("Permit with an empty work_dir = %v, want nil (an empty work_dir runs beside the binary, which is trusted by construction)", err)
	}

	// And with no AllowedDirs configured at all, unchanged: still permitted.
	bare := LocalPolicy{AllowedBinaries: []string{"/usr/bin/ollama"}}
	if err := bare.Permit(Spec{ID: "s2", Binary: "/usr/bin/ollama", WorkDir: ""}); err != nil {
		t.Fatalf("Permit(empty work_dir, no AllowedDirs) = %v, want nil", err)
	}
}

// TestEffectiveWorkDir pins the pure helper the three R2 touch points share: an
// explicit work_dir is returned verbatim (so the reportable command and cmd.Dir
// keep matching the spec, and command_test's non-empty case stays green), and
// an empty one resolves to the binary's own directory.
func TestEffectiveWorkDir(t *testing.T) {
	if got := effectiveWorkDir(Spec{Binary: "/usr/bin/ollama", WorkDir: "/srv/models"}); got != "/srv/models" {
		t.Errorf("effectiveWorkDir(explicit) = %q, want the spec's work_dir verbatim", got)
	}
	if got := effectiveWorkDir(Spec{Binary: "/usr/bin/ollama", WorkDir: ""}); got != "/usr/bin" {
		t.Errorf("effectiveWorkDir(empty) = %q, want the binary's directory %q", got, "/usr/bin")
	}
	if got := effectiveWorkDir(Spec{Binary: "/opt/vllm/bin/vllm"}); got != "/opt/vllm/bin" {
		t.Errorf("effectiveWorkDir(empty, nested binary) = %q, want %q", got, "/opt/vllm/bin")
	}
}

func TestPermitListedBinaryPasses(t *testing.T) {
	p := LocalPolicy{AllowedBinaries: []string{"/usr/bin/ollama", "/opt/vllm/bin/vllm"}}
	spec := Spec{ID: "s1", Binary: "/usr/bin/ollama"}

	if err := p.Permit(spec); err != nil {
		t.Errorf("Permit(listed binary) = %v, want nil", err)
	}
}

func TestPermitUnlistedBinaryRejects(t *testing.T) {
	p := LocalPolicy{AllowedBinaries: []string{"/usr/bin/ollama"}}
	spec := Spec{ID: "s1", Binary: "/usr/bin/evil"}

	err := p.Permit(spec)
	if err == nil {
		t.Fatal("Permit(unlisted binary) should error")
	}
	if !strings.Contains(err.Error(), spec.Binary) {
		t.Errorf("Permit error = %q, want it to name the rejected binary", err.Error())
	}
}

// TestPermitEmptyBinaryRejected proves an empty spec.Binary is refused even
// against a non-empty allowlist -- Permit never reached the point of
// checking absoluteness, so an empty string (zero value, or a spec that
// simply omitted Binary) must not slip through.
func TestPermitEmptyBinaryRejected(t *testing.T) {
	p := LocalPolicy{AllowedBinaries: []string{"/usr/bin/ollama"}}
	spec := Spec{ID: "s1", Binary: ""}

	if err := p.Permit(spec); err == nil {
		t.Fatal("Permit with an empty Binary should refuse, not match the allowlist")
	}
}

// TestPermitAllowlistWithEmptyStringDoesNotPermitEmptyBinary pins the
// concrete bypass: resolveStringList's env-only empty-entry drop means a
// config FILE value of runtime_allowed_binaries: [""] survives verbatim as
// []string{""}, which has len != 0 and thus passes the empty-allowlist gate.
// Without an absoluteness check, that entry would then exactly match a spec
// with Binary: "".
func TestPermitAllowlistWithEmptyStringDoesNotPermitEmptyBinary(t *testing.T) {
	p := LocalPolicy{AllowedBinaries: []string{""}}
	spec := Spec{ID: "s1", Binary: ""}

	if err := p.Permit(spec); err == nil {
		t.Fatal("Permit must not treat an empty allowlist entry as matching an empty Binary")
	}
}

// TestPermitRelativeBinaryRejected proves a relative Binary is refused even
// when it happens to match an allowlist entry's text -- absolute paths only,
// per the AllowedBinaries doc comment.
func TestPermitRelativeBinaryRejected(t *testing.T) {
	p := LocalPolicy{AllowedBinaries: []string{"ollama"}}
	spec := Spec{ID: "s1", Binary: "ollama"}

	if err := p.Permit(spec); err == nil {
		t.Fatal("Permit with a relative Binary should refuse even if it matches a (relative) allowlist entry")
	}
}

// TestPermitWorkDirContainment pins the exact containment rule: filepath.Clean
// both sides, then require the candidate to equal the allowed dir or sit
// strictly beneath it (a path-segment boundary), never a bare string-prefix
// match. The sibling-prefix case (models-evil vs models) is the one most
// implementations get wrong.
func TestPermitWorkDirContainment(t *testing.T) {
	p := LocalPolicy{
		AllowedBinaries: []string{"/usr/bin/ollama"},
		AllowedDirs:     []string{"/srv/models"},
	}
	base := Spec{ID: "s1", Binary: "/usr/bin/ollama"}

	cases := []struct {
		name    string
		workDir string
		wantOK  bool
	}{
		{"exact match", "/srv/models", true},
		{"nested subdirectory", "/srv/models/llama3", true},
		{"deeply nested subdirectory", "/srv/models/llama3/weights", true},
		{"sibling with shared string prefix", "/srv/models-evil", false},
		{"sibling with shared string prefix, nested", "/srv/models-evil/x", false},
		{"unrelated directory", "/srv/other", false},
		{"dot-dot traversal escaping the allowed dir", "/srv/models/../models-evil", false},
		{"dot-dot traversal staying inside", "/srv/models/llama3/../llama3", true},
		{"dot-dot traversal to parent", "/srv/models/..", false},
		// R2: an empty work_dir is permitted on its own merit (it runs beside
		// the binary), independent of AllowedDirs -- so even here, with
		// /usr/bin/ollama's dir NOT under the configured /srv/models, it passes.
		{"empty work_dir runs beside the binary", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := base
			spec.WorkDir = tc.workDir
			err := p.Permit(spec)
			if tc.wantOK && err != nil {
				t.Errorf("Permit(work_dir=%q) = %v, want nil (permitted)", tc.workDir, err)
			}
			if !tc.wantOK && err == nil {
				t.Errorf("Permit(work_dir=%q) = nil, want an error (not permitted)", tc.workDir)
			}
		})
	}
}

// TestPermitDoesNotResolveSymlinksAcceptedResidualRisk pins the DELIBERATE
// gap withinDir documents: containment is a lexical check, so a symlink
// under an allowed dir that points outside it is permitted. This test exists
// so the acceptance is visible in the suite rather than only in a comment --
// anyone who "fixes" it by adding filepath.EvalSymlinks fails here and is
// sent to withinDir's doc comment, which explains why that fix would trade
// an honest lexical check for a TOCTOU window that merely LOOKS enforced.
func TestPermitDoesNotResolveSymlinksAcceptedResidualRisk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows; the lexical check itself is platform-independent")
	}
	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{allowed, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	escape := filepath.Join(allowed, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("symlinks unavailable in this environment: %v", err)
	}

	p := LocalPolicy{AllowedBinaries: []string{"/usr/bin/ollama"}, AllowedDirs: []string{allowed}}
	if err := p.Permit(Spec{ID: "s1", Binary: "/usr/bin/ollama", WorkDir: escape}); err != nil {
		t.Fatalf("Permit(work_dir=%q, a symlink out of the allowed dir) = %v, want nil -- containment is lexical BY DESIGN (see withinDir's doc comment: EvalSymlinks would introduce a TOCTOU window, and AllowedBinaries, not work_dir, is the boundary). If this failure is intentional, update withinDir's comment and the README's runtime_allowed_dirs entry together with it.", escape, err)
	}
}

// TestPermitNoAllowedDirsMeansAnyWorkDir proves an empty AllowedDirs is a
// distinct configuration from an empty AllowedBinaries: it means "any
// work_dir", not "no work_dir may ever be used".
func TestPermitNoAllowedDirsMeansAnyWorkDir(t *testing.T) {
	p := LocalPolicy{AllowedBinaries: []string{"/usr/bin/ollama"}}
	spec := Spec{ID: "s1", Binary: "/usr/bin/ollama", WorkDir: "/anywhere/at/all"}

	if err := p.Permit(spec); err != nil {
		t.Errorf("Permit with no AllowedDirs configured = %v, want nil (any work_dir permitted)", err)
	}
}

// TestPermitWorkDirWildcardEntry is R4's security-boundary table, driven through
// the PUBLIC Permit path so the untrusted candidate takes the exact route it
// takes in production (filepath.Clean + separator-boundary containment in
// withinDir), never a glob engine. A trailing whole-segment wildcard
// ("<dir>/*") is an EXACTLY EQUIVALENT spelling of the bare subtree entry: it
// must permit the directory and everything strictly beneath it, and must NEVER
// permit a path outside that tree. The reject rows are the whole point -- a
// wildcard that admits a sibling, a "..", or a separator jump is a critical
// defect, not a bug.
//
// The POSIX shape is fully exercised here on the Linux CI runner; the Windows
// backslash/drive/case shape rides on the unchanged withinDir and is pinned in
// the GOOS-gated TestPermitWorkDirWildcardEntryWindows below (plus, for the
// pure string reduction of BOTH separators, TestAllowedDirBase).
func TestPermitWorkDirWildcardEntry(t *testing.T) {
	base := Spec{ID: "s1", Binary: "/usr/bin/ollama"}

	cases := []struct {
		name    string
		entry   string // a single runtime_allowed_dirs entry
		workDir string
		wantOK  bool
	}{
		// A trailing "/*" now permits the subtree -- the operator's own example
		// ("der Ordner und alle Unterordner"). Every one of these is a RED row
		// before allowedDirBase exists (the literal "*" matched nothing useful)
		// and GREEN after.
		{"trailing star permits the dir itself", "/srv/llama_cpp/*", "/srv/llama_cpp", true},
		{"trailing star permits a child", "/srv/llama_cpp/*", "/srv/llama_cpp/models", true},
		{"trailing star permits a deep descendant", "/srv/llama_cpp/*", "/srv/llama_cpp/a/b/c", true},
		{"trailing star tolerates a contained traversal", "/srv/llama_cpp/*", "/srv/llama_cpp/x/../y", true},

		// The reject list. These are GREEN both before and after: the matcher
		// must WIDEN permission to the subtree without EVER widening it to an
		// escape. The candidate is cleaned before any comparison, so a "*"
		// never participates in matching and can never swallow a separator.
		{"trailing star rejects a dot-dot escape", "/srv/llama_cpp/*", "/srv/llama_cpp/../windows", false},
		{"trailing star rejects a sibling with a shared prefix", "/srv/llama_cpp/*", "/srv/llama_cpp-evil", false},
		{"trailing star rejects a nested sibling with a shared prefix", "/srv/llama_cpp/*", "/srv/llama_cpp-evil/x", false},
		{"trailing star rejects an unrelated tree", "/srv/llama_cpp/*", "/srv/other", false},
		{"trailing star rejects a relative candidate", "/srv/llama_cpp/*", "relative/models", false},

		// A glued star is NOT a wildcard: "/srv/models*" ends in "s*", not
		// "/*", so it stays literal and matches only a directory really named
		// that -- it must NOT permit the "/srv/models-evil" sibling. This is
		// the new rule that keeps option (a) safe, and it is fully testable on
		// Linux.
		{"glued star stays literal, rejects the evil sibling", "/srv/models*", "/srv/models-evil", false},
		{"glued star stays literal, rejects the bare dir", "/srv/models*", "/srv/models", false},

		// A bare "*" reduces to "" -> withinDir permits NOTHING (fail closed),
		// never the whole filesystem.
		{"bare star permits nothing", "*", "/srv/anything", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := LocalPolicy{AllowedBinaries: []string{"/usr/bin/ollama"}, AllowedDirs: []string{tc.entry}}
			spec := base
			spec.WorkDir = tc.workDir
			err := p.Permit(spec)
			if tc.wantOK && err != nil {
				t.Errorf("Permit(entry=%q, work_dir=%q) = %v, want nil (permitted)", tc.entry, tc.workDir, err)
			}
			if !tc.wantOK && err == nil {
				t.Errorf("Permit(entry=%q, work_dir=%q) = nil, want an error (a wildcard must never permit a path outside its tree)", tc.entry, tc.workDir)
			}
		})
	}
}

// TestAllowedDirBase pins the pure string reduction directly, which is the ONE
// piece of R4 that is GOOS-independent and therefore the place the Windows
// backslash form is proven on the Linux CI runner (see the function's own doc:
// CI runs no Windows job, so "\*" recognition MUST be observable on a POSIX
// host). It also pins the two properties the security argument leans on: a
// glued star is left verbatim (so a sibling can never be permitted), and a bare
// "*" reduces to "" (fail closed).
func TestAllowedDirBase(t *testing.T) {
	cases := []struct {
		entry string
		want  string
	}{
		// Trailing whole-segment wildcard -> the concrete base, both separators.
		{"/srv/models/*", "/srv/models"},
		{`c:\llama_cpp\*`, `c:\llama_cpp`},
		{"/srv/a/b/*", "/srv/a/b"},
		// A plain (bare) entry is already a subtree, returned unchanged.
		{"/srv/models", "/srv/models"},
		{`c:\llama_cpp`, `c:\llama_cpp`},
		// NOT a whole trailing segment -> verbatim, so withinDir treats "*" as
		// an ordinary path character and the entry matches nothing useful.
		{"/srv/models*", "/srv/models*"},
		{"/srv/a/*/b", "/srv/a/*/b"},
		{"/srv/**", "/srv/**"},
		// A bare "*" fails closed.
		{"*", ""},
		// An operator writing ".." in an entry broadens their OWN trusted
		// allowlist, identical to a bare "/srv/models/.." -- not a
		// candidate-side escape.
		{"/srv/models/../*", "/srv/models/.."},
		// Degenerate roots/volumes an operator should not write, called out so
		// their reduction is a decision, not a surprise.
		{"/*", ""},
		{`c:\*`, "c:"},
	}
	for _, tc := range cases {
		t.Run(tc.entry, func(t *testing.T) {
			if got := allowedDirBase(tc.entry); got != tc.want {
				t.Errorf("allowedDirBase(%q) = %q, want %q", tc.entry, got, tc.want)
			}
		})
	}
}

// TestPermitWorkDirWildcardEntryWindows is the Windows-gated half of R4: on a
// real Windows host the operator's own example, c:\llama_cpp\*, must permit the
// tree and reject every escape, exactly as the POSIX table above does. It adds
// NO new untested containment behaviour -- allowedDirBase only reduces the
// trusted entry, and the untrusted candidate still flows through the unchanged,
// already-audited withinDir, which handles backslashes, drive letters and case
// natively on Windows. CI runs no Windows job (see the skips throughout this
// package), so this documents and locks the behaviour for the deployment
// platform and for `GOOS=windows go vet`.
func TestPermitWorkDirWildcardEntryWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path containment (backslash/drive/case) is GOOS-native in withinDir; the separator-reduction half is proven cross-platform in TestAllowedDirBase")
	}
	base := Spec{ID: "s1", Binary: `c:\bin\ollama.exe`}
	cases := []struct {
		name    string
		entry   string
		workDir string
		wantOK  bool
	}{
		{"permits the dir itself", `c:\llama_cpp\*`, `c:\llama_cpp`, true},
		{"permits a child", `c:\llama_cpp\*`, `c:\llama_cpp\models`, true},
		{"rejects a dot-dot escape", `c:\llama_cpp\*`, `c:\llama_cpp\..\windows`, false},
		{"rejects a sibling with a shared prefix", `c:\llama_cpp\*`, `c:\llama_cpp_evil`, false},
		{"rejects a different volume", `c:\llama_cpp\*`, `d:\llama_cpp\x`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := LocalPolicy{AllowedBinaries: []string{`c:\bin\ollama.exe`}, AllowedDirs: []string{tc.entry}}
			spec := base
			spec.WorkDir = tc.workDir
			err := p.Permit(spec)
			if tc.wantOK && err != nil {
				t.Errorf("Permit(entry=%q, work_dir=%q) = %v, want nil (permitted)", tc.entry, tc.workDir, err)
			}
			if !tc.wantOK && err == nil {
				t.Errorf("Permit(entry=%q, work_dir=%q) = nil, want an error", tc.entry, tc.workDir)
			}
		})
	}
}

// TestPermitAllowBinaryDirs is R3's boundary table. With the toggle ON, each
// allowlisted binary's PARENT directory is treated as an allowed dir without the
// operator listing it in AllowedDirs; with it OFF, behaviour is exactly as
// before this field existed. The auto-added dirs go through the SAME withinDir
// subtree check R4's non-wildcard path uses, so R3 and R4 agree on semantics by
// construction -- including the separator boundary that keeps a sibling out.
func TestPermitAllowBinaryDirs(t *testing.T) {
	cases := []struct {
		name    string
		policy  LocalPolicy
		workDir string
		wantOK  bool
	}{
		// Toggle ON, AllowedDirs empty: only the binary subtrees (and R2's
		// empty case) are permitted -- the intended permissive->restrictive
		// flip.
		{"on: work_dir under a binary's own dir", LocalPolicy{AllowedBinaries: []string{"/usr/bin/ollama"}, AllowBinaryDirs: true}, "/usr/bin/models", true},
		{"on: work_dir IS a binary's own dir", LocalPolicy{AllowedBinaries: []string{"/usr/bin/ollama"}, AllowBinaryDirs: true}, "/usr/bin", true},
		{"on: work_dir under a SECOND binary's dir", LocalPolicy{AllowedBinaries: []string{"/usr/bin/ollama", "/opt/vllm/bin/vllm"}, AllowBinaryDirs: true}, "/opt/vllm/bin/run", true},
		{"on: work_dir outside every binary dir is rejected", LocalPolicy{AllowedBinaries: []string{"/usr/bin/ollama"}, AllowBinaryDirs: true}, "/srv/models", false},
		{"on: sibling of a binary dir with a shared prefix is rejected", LocalPolicy{AllowedBinaries: []string{"/usr/bin/ollama"}, AllowBinaryDirs: true}, "/usr/bin-evil", false},
		// Toggle ON, AllowedDirs also set: the effective set is the UNION.
		{"on: work_dir under an explicit AllowedDirs entry still passes", LocalPolicy{AllowedBinaries: []string{"/usr/bin/ollama"}, AllowedDirs: []string{"/srv/models"}, AllowBinaryDirs: true}, "/srv/models/x", true},
		{"on: work_dir under the binary dir passes even when AllowedDirs excludes it", LocalPolicy{AllowedBinaries: []string{"/usr/bin/ollama"}, AllowedDirs: []string{"/srv/models"}, AllowBinaryDirs: true}, "/usr/bin/x", true},
		// A non-absolute allowlist entry contributes no binary dir (it can never
		// permit anything), so it must not widen the set.
		{"on: a relative binary entry contributes no dir", LocalPolicy{AllowedBinaries: []string{"/usr/bin/ollama", "ollama"}, AllowBinaryDirs: true}, "/anywhere", false},

		// Toggle OFF: behaviour is exactly as today. Empty AllowedDirs => any
		// work_dir; a set AllowedDirs => only within it; the binary dir gets NO
		// special treatment.
		{"off: empty AllowedDirs permits any work_dir", LocalPolicy{AllowedBinaries: []string{"/usr/bin/ollama"}}, "/anywhere/at/all", true},
		{"off: work_dir under the binary dir is NOT auto-allowed", LocalPolicy{AllowedBinaries: []string{"/usr/bin/ollama"}, AllowedDirs: []string{"/srv/models"}}, "/usr/bin/x", false},
		{"off: work_dir within AllowedDirs still passes", LocalPolicy{AllowedBinaries: []string{"/usr/bin/ollama"}, AllowedDirs: []string{"/srv/models"}}, "/srv/models/x", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := Spec{ID: "s1", Binary: "/usr/bin/ollama", WorkDir: tc.workDir}
			err := tc.policy.Permit(spec)
			if tc.wantOK && err != nil {
				t.Errorf("Permit(work_dir=%q) = %v, want nil (permitted)", tc.workDir, err)
			}
			if !tc.wantOK && err == nil {
				t.Errorf("Permit(work_dir=%q) = nil, want an error (not permitted)", tc.workDir)
			}
		})
	}
}

// TestExpandPlaceholdersPort proves ${PORT} in args resolves to the chosen
// listen port, given as a plain decimal string.
func TestExpandPlaceholdersPort(t *testing.T) {
	spec := Spec{
		Args: []string{"serve", "--port", "${PORT}", "--url", "http://127.0.0.1:${PORT}/health"},
	}
	getenv := func(string) string { return "" }

	args, _, err := ExpandPlaceholders(spec, 41123, GPUVendorNVIDIA, getenv)
	if err != nil {
		t.Fatalf("ExpandPlaceholders: %v", err)
	}
	want := []string{"serve", "--port", "41123", "--url", "http://127.0.0.1:41123/health"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %#v, want %#v", args, want)
	}
}

// TestExpandPlaceholdersModel proves ${MODEL} resolves in args AND in env
// values -- the two places every other placeholder is expanded -- and that it
// carries the APPLICATION-side name (spec.UpstreamModel, the owning
// mapping's app_model_name), NOT the gateway-facing spec.Model. The two are
// deliberately different strings here: a test where they matched would pass
// against an implementation that read the wrong field.
func TestExpandPlaceholdersModel(t *testing.T) {
	spec := Spec{
		Model:         "gpt-4o-mini", // gateway-facing, must NOT appear
		UpstreamModel: "qwen3-30b-a3b-instruct",
		Args: []string{
			"--alias", "${MODEL}",
			"--model", "/srv/models/${MODEL}/weights.gguf",
			"--served-model-name=${MODEL}",
		},
		Env: map[string]string{"MODEL_TAG": "${MODEL}", "MIXED": "${MODEL}:${PORT}"},
	}

	args, env, err := ExpandPlaceholders(spec, 41123, GPUVendorNVIDIA, func(string) string { return "" })
	if err != nil {
		t.Fatalf("ExpandPlaceholders: %v", err)
	}
	wantArgs := []string{
		"--alias", "qwen3-30b-a3b-instruct",
		"--model", "/srv/models/qwen3-30b-a3b-instruct/weights.gguf",
		"--served-model-name=qwen3-30b-a3b-instruct",
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %#v, want %#v", args, wantArgs)
	}
	wantEnv := map[string]string{
		"MODEL_TAG": "qwen3-30b-a3b-instruct",
		"MIXED":     "qwen3-30b-a3b-instruct:41123",
	}
	if !envContainsExactly(env, wantEnv) {
		t.Errorf("env = %#v, want exactly %#v", env, wantEnv)
	}
	for _, s := range append(append([]string{}, args...), env...) {
		if strings.Contains(s, spec.Model) {
			t.Errorf("%q carries the GATEWAY-facing model name; ${MODEL} must resolve the application-side upstream_model", s)
		}
	}
}

// TestExpandPlaceholdersModelVariantsPassThroughLiterally is the decision
// test for ${MODEL} having NO near-miss rule. ${PORT} refuses anything whose
// inner text starts with "PORT", on the stated ground that nothing plausible
// starts with it except a typo of the placeholder. That ground does not hold
// for MODEL: every row below is a plausible token an operator wants handed to
// a model server that does its own templating, and refusing them would be the
// ${TRANSPORT}/${EXPORT_DIR} defect again under a new name.
//
// The last two rows are the accepted cost, pinned deliberately rather than
// left undiscovered: a typo and a lowercase spelling reach the child as
// literal text instead of erroring.
func TestExpandPlaceholdersModelVariantsPassThroughLiterally(t *testing.T) {
	for _, text := range []string{
		"${MODEL_PATH}",
		"${MODELS_DIR}",
		"${MODEL_ID}",
		"${MODEL_NAME}",
		"${MODELFILE}",
		"${MODEL2}",
		"${MDOEL}", // typo -- accepted cost
		"${model}", // wrong case -- accepted cost
	} {
		t.Run(text, func(t *testing.T) {
			spec := Spec{
				UpstreamModel: "qwen3-30b-a3b-instruct",
				Args:          []string{text},
				Env:           map[string]string{"X": text},
			}
			args, env, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, func(string) string { return "" })
			if err != nil {
				t.Fatalf("%s should pass through untouched, not error: %v", text, err)
			}
			if !reflect.DeepEqual(args, []string{text}) {
				t.Errorf("args = %#v, want the literal %q", args, text)
			}
			if !envContainsExactly(env, map[string]string{"X": text}) {
				t.Errorf("env = %#v, want the literal %q", env, text)
			}
		})
	}
}

// TestExpandPlaceholdersModelEmptyUpstreamErrors pins the empty case: a spec
// that USES ${MODEL} while its upstream_model is empty is a hard error, not
// an empty substitution -- `--alias ""` or a path with a hole in it fails
// somewhere downstream and confusingly, exactly what the unset-${AGENT_ENV}
// error exists to prevent. Expansion failures map to `not_permitted`, so the
// operator sees this text rather than a crash loop.
func TestExpandPlaceholdersModelEmptyUpstreamErrors(t *testing.T) {
	for _, spec := range []Spec{
		{Model: "gpt-4o-mini", Args: []string{"--alias", "${MODEL}"}},
		{Model: "gpt-4o-mini", Env: map[string]string{"TAG": "${MODEL}"}},
	} {
		_, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, func(string) string { return "" })
		if err == nil {
			t.Fatalf("ExpandPlaceholders(%+v) with an empty UpstreamModel should refuse, not substitute an empty string", spec)
		}
		if !strings.Contains(err.Error(), "upstream_model") {
			t.Errorf("error = %q, want it to name upstream_model so the operator knows which field is empty", err.Error())
		}
		if got := strings.Count(err.Error(), "runtime:"); got != 1 {
			t.Errorf("error = %q, want exactly one %q prefix", err.Error(), "runtime:")
		}
	}

	// The refusal is scoped to specs that actually USE the placeholder: an
	// empty upstream_model is not by itself a launch failure.
	unused := Spec{Args: []string{"--port", "${PORT}"}, Env: map[string]string{"MODE": "solo"}}
	if _, _, err := ExpandPlaceholders(unused, 8080, GPUVendorNVIDIA, func(string) string { return "" }); err != nil {
		t.Errorf("a spec that never mentions ${MODEL} must not be refused for an empty upstream_model: %v", err)
	}
}

// TestExpandPlaceholdersAgentEnvResolves proves ${AGENT_ENV:NAME} resolves
// via the injected getenv -- the agent's OWN process environment, never a
// value carried on the wire.
func TestExpandPlaceholdersAgentEnvResolves(t *testing.T) {
	spec := Spec{
		Args: []string{"--hf-token", "${AGENT_ENV:HF_TOKEN}"},
		Env:  map[string]string{"HF_TOKEN": "${AGENT_ENV:HF_TOKEN}"},
	}
	getenv := func(k string) string {
		if k == "HF_TOKEN" {
			return "hf-secret-value"
		}
		return ""
	}

	args, env, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
	if err != nil {
		t.Fatalf("ExpandPlaceholders: %v", err)
	}
	if want := []string{"--hf-token", "hf-secret-value"}; !reflect.DeepEqual(args, want) {
		t.Errorf("args = %#v, want %#v", args, want)
	}
	if !envContainsExactly(env, map[string]string{"HF_TOKEN": "hf-secret-value"}) {
		t.Errorf("env = %#v, want exactly HF_TOKEN=hf-secret-value", env)
	}
}

// TestExpandPlaceholdersAgentEnvRefusesOwnNamespace pins the Critical fix: a
// gateway-supplied spec must never be able to read the agent's OWN
// environment variables (its gateway bearer token, or any other
// agent-internal setting) via ${AGENT_ENV:...} -- that would hand a portal
// administrator the exact credential this whole boundary exists to protect.
// Any name in the agent's own OP_AGENT_* namespace is refused outright,
// regardless of whether getenv would have actually returned a value for it.
func TestExpandPlaceholdersAgentEnvRefusesOwnNamespace(t *testing.T) {
	spec := Spec{Args: []string{"--token", "${AGENT_ENV:OP_AGENT_TOKEN}"}}
	getenv := func(k string) string {
		if k == "OP_AGENT_TOKEN" {
			return "gateway-bearer-secret"
		}
		return ""
	}

	_, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
	if err == nil {
		t.Fatal("ExpandPlaceholders with ${AGENT_ENV:OP_AGENT_TOKEN} should refuse, not resolve the agent's own token")
	}
	if !strings.Contains(err.Error(), "OP_AGENT_TOKEN") {
		t.Errorf("error = %q, want it to name the refused variable OP_AGENT_TOKEN", err.Error())
	}
	if strings.Contains(err.Error(), "gateway-bearer-secret") {
		t.Fatalf("error leaked the secret value: %q", err.Error())
	}
}

// TestExpandPlaceholdersAgentEnvRefusesOwnNamespaceViaEnv is the
// fix-round-2 R2-3 bundled minor: the round-1 CRITICAL refusal
// (TestExpandPlaceholdersAgentEnvRefusesOwnNamespace above) was exercised
// only through spec.Args. Both spec.Args and spec.Env values run through
// the same `expand` closure today, so there is no live gap -- but nothing
// would catch a future refactor that duplicated the closure and let the
// spec.Env path diverge. This pins the same refusal through spec.Env.
func TestExpandPlaceholdersAgentEnvRefusesOwnNamespaceViaEnv(t *testing.T) {
	spec := Spec{Env: map[string]string{"TOKEN": "${AGENT_ENV:OP_AGENT_TOKEN}"}}
	getenv := func(k string) string {
		if k == "OP_AGENT_TOKEN" {
			return "gateway-bearer-secret"
		}
		return ""
	}

	_, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
	if err == nil {
		t.Fatal("ExpandPlaceholders with spec.Env referencing ${AGENT_ENV:OP_AGENT_TOKEN} should refuse, not resolve the agent's own token")
	}
	if !strings.Contains(err.Error(), "OP_AGENT_TOKEN") {
		t.Errorf("error = %q, want it to name the refused variable OP_AGENT_TOKEN", err.Error())
	}
	if strings.Contains(err.Error(), "gateway-bearer-secret") {
		t.Fatalf("error leaked the secret value: %q", err.Error())
	}
}

// TestExpandPlaceholdersAgentEnvOrdinaryNameStillResolves proves the
// namespace refusal is scoped to OP_AGENT_* -- an ordinary secret name
// unrelated to the agent's own settings still resolves normally.
func TestExpandPlaceholdersAgentEnvOrdinaryNameStillResolves(t *testing.T) {
	spec := Spec{Args: []string{"--hf-token", "${AGENT_ENV:HF_TOKEN}"}}
	getenv := func(k string) string {
		if k == "HF_TOKEN" {
			return "hf-secret-value"
		}
		return ""
	}

	args, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
	if err != nil {
		t.Fatalf("ExpandPlaceholders: %v", err)
	}
	if want := []string{"--hf-token", "hf-secret-value"}; !reflect.DeepEqual(args, want) {
		t.Errorf("args = %#v, want %#v", args, want)
	}
}

// TestExpandPlaceholdersMissingAgentEnvErrors proves a missing agent
// environment variable is a hard error naming the variable, never an empty
// substitution -- launching a model with an empty HF_TOKEN produces a
// confusing downstream auth failure instead of an honest refusal here.
func TestExpandPlaceholdersMissingAgentEnvErrors(t *testing.T) {
	spec := Spec{Args: []string{"--hf-token", "${AGENT_ENV:HF_TOKEN}"}}
	getenv := func(string) string { return "" }

	_, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
	if err == nil {
		t.Fatal("ExpandPlaceholders with an unset AGENT_ENV var should error")
	}
	if !strings.Contains(err.Error(), "HF_TOKEN") {
		t.Errorf("error = %q, want it to name the missing variable HF_TOKEN", err.Error())
	}
}

// TestExpandPlaceholdersMissingAgentEnvErrorHasNoDoubledPrefix pins the
// exact wrapped error text for a missing AGENT_ENV var in an arg: the
// "runtime: " prefix must appear exactly once. expand()'s own errors carry
// no prefix precisely so that the call-site wrap ("runtime: expand arg %d:
// %w") is the only place it is added -- an inner error that also started
// with "runtime: " would double it (e.g. "runtime: expand arg 0: runtime:
// required ...").
func TestExpandPlaceholdersMissingAgentEnvErrorHasNoDoubledPrefix(t *testing.T) {
	spec := Spec{Args: []string{"${AGENT_ENV:HF_TOKEN}"}}
	getenv := func(string) string { return "" }

	_, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
	if err == nil {
		t.Fatal("ExpandPlaceholders with an unset AGENT_ENV var should error")
	}
	if got := strings.Count(err.Error(), "runtime:"); got != 1 {
		t.Errorf("error = %q, want exactly one %q prefix, got %d", err.Error(), "runtime:", got)
	}
}

// TestExpandPlaceholdersMissingAgentEnvInEnvValueErrors proves the same
// missing-variable check applies to spec.Env values, not just spec.Args.
func TestExpandPlaceholdersMissingAgentEnvInEnvValueErrors(t *testing.T) {
	spec := Spec{Env: map[string]string{"HF_TOKEN": "${AGENT_ENV:HF_TOKEN}"}}
	getenv := func(string) string { return "" }

	_, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
	if err == nil {
		t.Fatal("ExpandPlaceholders with an unset AGENT_ENV var in spec.Env should error")
	}
	if !strings.Contains(err.Error(), "HF_TOKEN") {
		t.Errorf("error = %q, want it to name the missing variable HF_TOKEN", err.Error())
	}
	if got := strings.Count(err.Error(), "runtime:"); got != 1 {
		t.Errorf("error = %q, want exactly one %q prefix (no doubling from expand's env wrap), got %d", err.Error(), "runtime:", got)
	}
}

// TestExpandPlaceholdersChildEnvExactSet is the security-boundary test: the
// child process must receive ONLY the expanded spec env plus PATH/HOME taken
// from the agent's own environment -- NEVER the agent's full environment,
// which holds its gateway bearer token and every OTHER model's
// ${AGENT_ENV:...} secret. It asserts the exact resulting set, not merely
// that the wanted variables are present, since "contains what I wanted"
// would also pass while leaking everything else.
func TestExpandPlaceholdersChildEnvExactSet(t *testing.T) {
	spec := Spec{
		Env: map[string]string{
			"HF_TOKEN": "${AGENT_ENV:HF_TOKEN}",
			"MODE":     "production",
		},
	}
	// agentEnv simulates the agent's FULL process environment: PATH, HOME, the
	// secret this spec asks for by name, the gateway's own bearer token, and
	// a DIFFERENT model's secret this spec never referenced. Only the first
	// three may ever reach the child.
	agentEnv := map[string]string{
		"PATH":                          "/usr/local/bin:/usr/bin:/bin",
		"HOME":                          "/home/agent",
		"HF_TOKEN":                      "hf-secret-value",
		"OP_AGENT_TOKEN":                "gateway-bearer-secret",
		"OTHER_MODEL_API_KEY":           "some-other-models-secret",
		"UNRELATED_SHELL_HISTORY_STUFF": "noise",
	}
	getenv := func(k string) string { return agentEnv[k] }

	_, env, err := ExpandPlaceholders(spec, 9090, GPUVendorNVIDIA, getenv)
	if err != nil {
		t.Fatalf("ExpandPlaceholders: %v", err)
	}

	want := map[string]string{
		"PATH":     "/usr/local/bin:/usr/bin:/bin",
		"HOME":     "/home/agent",
		"HF_TOKEN": "hf-secret-value",
		"MODE":     "production",
	}
	if !envContainsExactly(env, want) {
		t.Errorf("child env = %#v, want exactly %#v (no agent-only variables)", env, want)
	}
	for _, kv := range env {
		if strings.Contains(kv, "gateway-bearer-secret") || strings.Contains(kv, "some-other-models-secret") {
			t.Fatalf("child env leaked an agent-only secret: %q", kv)
		}
	}
}

// TestExpandPlaceholdersSpecEnvCannotOverridePathOrHome pins the decision
// recorded in the Task 13 fix-round-1 report: a spec.Env key of PATH or HOME
// is refused outright rather than silently overriding the agent-provided
// base (which previously produced two PATH= entries in the child env, with
// os/exec resolving last-occurrence-wins -- letting a gateway-supplied PATH
// win in the child).
func TestExpandPlaceholdersSpecEnvCannotOverridePathOrHome(t *testing.T) {
	getenv := func(k string) string {
		switch k {
		case "PATH":
			return "/usr/local/bin:/usr/bin:/bin"
		case "HOME":
			return "/home/agent"
		default:
			return ""
		}
	}

	for _, key := range []string{"PATH", "HOME"} {
		t.Run(key, func(t *testing.T) {
			spec := Spec{Env: map[string]string{key: "/attacker-controlled"}}
			_, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
			if err == nil {
				t.Fatalf("ExpandPlaceholders with spec.Env[%q] set should refuse, not override the agent-provided base", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error = %q, want it to name %q", err.Error(), key)
			}
		})
	}
}

// TestExpandPlaceholdersSpecEnvCannotOverridePathOrHomeEvenWhenBaseAbsent
// proves the refusal is unconditional -- it applies even when the agent's
// own environment has no PATH/HOME to collide with, because the rule is
// about which party may ever set these keys, not about detecting an actual
// duplicate.
func TestExpandPlaceholdersSpecEnvCannotOverridePathOrHomeEvenWhenBaseAbsent(t *testing.T) {
	spec := Spec{Env: map[string]string{"PATH": "/attacker-controlled"}}
	getenv := func(string) string { return "" }

	_, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
	if err == nil {
		t.Fatal("ExpandPlaceholders with spec.Env[PATH] set should refuse even when the agent has no PATH of its own")
	}
}

// TestExpandPlaceholdersOmitsAbsentBase proves PATH/HOME are included only
// when the agent's own environment actually defines them -- ExpandPlaceholders
// never invents a value.
func TestExpandPlaceholdersOmitsAbsentBase(t *testing.T) {
	spec := Spec{Env: map[string]string{"MODE": "solo"}}
	getenv := func(string) string { return "" } // no PATH, no HOME

	_, env, err := ExpandPlaceholders(spec, 1234, GPUVendorNVIDIA, getenv)
	if err != nil {
		t.Fatalf("ExpandPlaceholders: %v", err)
	}
	if !envContainsExactly(env, map[string]string{"MODE": "solo"}) {
		t.Errorf("env = %#v, want exactly MODE=solo (no PATH/HOME fabricated)", env)
	}
}

// TestBaseEnvNamesAreUpperCase pins the invariant the reservation depends
// on. The refusal compares strings.ToUpper(specKey) against this list, so a
// lower- or mixed-case entry would be a name that can NEVER match -- i.e. a
// variable the agent copies into the child base while silently accepting a
// spec-supplied override of it, which is the exact failure the shared list
// exists to prevent. It is also the list's emitted spelling, so the check is
// cheap insurance on both uses at once.
func TestBaseEnvNamesAreUpperCase(t *testing.T) {
	for _, name := range baseEnvNames {
		if name != strings.ToUpper(name) {
			t.Errorf("baseEnvNames entry %q is not upper-case; the reservation upper-cases the spec key before comparing, so this entry could never be refused", name)
		}
	}
}

// TestExpandPlaceholdersWindowsBaseEnvironment is the covering test for the
// reported Windows failure: llama-server refusing to start with "failed to
// initialize router models: Failed to determine HF cache directory".
//
// Windows normally defines no HOME, so a base of PATH+HOME handed a Windows
// child NO home indicator at all and every per-user path resolution failed.
// The environment here is Windows-shaped in both senses that matter -- the
// native NAME SPELLINGS ("Path", "SystemRoot", "windir") and the
// case-INSENSITIVE lookup os.Getenv performs there via
// GetEnvironmentVariableW -- which is what makes the whole rule testable on a
// Linux or macOS host, the same seam 68475cd used for the case-folding
// guards. CI compiles nothing for Windows, so this seam is the only place
// this behaviour is observable at all.
func TestExpandPlaceholdersWindowsBaseEnvironment(t *testing.T) {
	getenv := windowsStyleGetenv(map[string]string{
		// Native Windows spellings, none of them upper-case.
		"Path":         `C:\Windows\system32;C:\Windows`,
		"SystemRoot":   `C:\Windows`,
		"windir":       `C:\Windows`,
		"USERPROFILE":  `C:\Users\agent`,
		"LOCALAPPDATA": `C:\Users\agent\AppData\Local`,
		// Deliberately NOT in the base: a Windows host defines these, and
		// they must not reach the child (TEMP/TMP are an operator lever, and
		// the rest are unnecessary) -- see baseEnvNames.
		"TEMP":                   `C:\Users\agent\AppData\Local\Temp`,
		"TMP":                    `C:\Users\agent\AppData\Local\Temp`,
		"APPDATA":                `C:\Users\agent\AppData\Roaming`,
		"ComSpec":                `C:\Windows\system32\cmd.exe`,
		"PATHEXT":                ".COM;.EXE;.BAT;.CMD",
		"NUMBER_OF_PROCESSORS":   "32",
		"PROCESSOR_ARCHITECTURE": "AMD64",
		// Agent-only, must never reach a child.
		"OP_AGENT_TOKEN":      "gateway-bearer-secret",
		"OTHER_MODEL_API_KEY": "some-other-models-secret",
	})

	spec := Spec{Env: map[string]string{"MODE": "production"}}
	_, env, err := ExpandPlaceholders(spec, 9090, GPUVendorNVIDIA, getenv)
	if err != nil {
		t.Fatalf("ExpandPlaceholders: %v", err)
	}

	want := map[string]string{
		"PATH":         `C:\Windows\system32;C:\Windows`,
		"USERPROFILE":  `C:\Users\agent`,
		"LOCALAPPDATA": `C:\Users\agent\AppData\Local`,
		"SYSTEMROOT":   `C:\Windows`,
		"WINDIR":       `C:\Windows`,
		"MODE":         "production",
	}
	if !envContainsExactly(env, want) {
		t.Errorf("child env = %#v, want exactly %#v", env, want)
	}
	// Named individually so a regression reports WHICH guarantee broke
	// rather than only that a map comparison failed.
	for name, why := range map[string]string{
		"USERPROFILE":  "the Windows home indicator; without it the HF cache root (~/.cache/huggingface) cannot be resolved and llama-server fails to start",
		"LOCALAPPDATA": "llama.cpp's fs_get_cache_directory() reads it directly on _WIN32",
		"SYSTEMROOT":   "Winsock initialisation fails with WSAStartup 10107 without it, and every child here is a network server",
	} {
		if !slices.Contains(env, name+"="+want[name]) {
			t.Errorf("child env is missing %s -- %s; env = %#v", name, why, env)
		}
	}
	// HOME is absent from this environment and must not be invented.
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOME=") {
			t.Errorf("child env has %q, but this Windows-shaped environment defines no HOME", kv)
		}
		if strings.Contains(kv, "secret") {
			t.Fatalf("child env leaked an agent-only secret: %q", kv)
		}
	}
}

// TestExpandPlaceholdersPosixBaseEnvironmentUnchanged is the
// no-regression half of the OS-appropriate base: every deployment today is
// Linux, and a POSIX-shaped agent environment must produce exactly the child
// environment it produced before the Windows names joined the list. The
// union list achieves that by presence alone -- a Linux host defines none of
// USERPROFILE, LOCALAPPDATA, SYSTEMROOT, WINDIR -- so this test is what
// proves the union is not silently widening the unix contract.
func TestExpandPlaceholdersPosixBaseEnvironmentUnchanged(t *testing.T) {
	agentEnv := map[string]string{
		"PATH": "/usr/local/bin:/usr/bin:/bin",
		"HOME": "/home/agent",
		// Ordinary unix noise a service inherits, none of it base material.
		"TMPDIR":         "/tmp",
		"LANG":           "en_US.UTF-8",
		"XDG_CACHE_HOME": "/home/agent/.cache",
		"SHELL":          "/bin/bash",
		"OP_AGENT_TOKEN": "gateway-bearer-secret",
	}
	getenv := func(k string) string { return agentEnv[k] } // case-SENSITIVE, as on unix

	spec := Spec{Env: map[string]string{"MODE": "production"}}
	_, env, err := ExpandPlaceholders(spec, 9090, GPUVendorNVIDIA, getenv)
	if err != nil {
		t.Fatalf("ExpandPlaceholders: %v", err)
	}

	want := map[string]string{
		"PATH": "/usr/local/bin:/usr/bin:/bin",
		"HOME": "/home/agent",
		"MODE": "production",
	}
	if !envContainsExactly(env, want) {
		t.Errorf("child env = %#v, want exactly %#v (the unix base must be unchanged by the Windows names)", env, want)
	}
}

// TestExpandPlaceholdersOmitsAbsentWindowsBase is the Windows half of
// TestExpandPlaceholdersOmitsAbsentBase: a base variable the agent's own
// environment does not define is simply not passed, never passed empty. An
// empty LOCALAPPDATA= in the child is worse than an absent one -- a consumer
// that checks only for presence would build a path from "" and write into
// the child's working directory.
func TestExpandPlaceholdersOmitsAbsentWindowsBase(t *testing.T) {
	// A Windows service account with no LOCALAPPDATA and no WINDIR.
	getenv := windowsStyleGetenv(map[string]string{
		"Path":        `C:\Windows\system32`,
		"SystemRoot":  `C:\Windows`,
		"USERPROFILE": `C:\Users\agent`,
	})

	_, env, err := ExpandPlaceholders(Spec{}, 1234, GPUVendorNVIDIA, getenv)
	if err != nil {
		t.Fatalf("ExpandPlaceholders: %v", err)
	}
	want := map[string]string{
		"PATH":        `C:\Windows\system32`,
		"SYSTEMROOT":  `C:\Windows`,
		"USERPROFILE": `C:\Users\agent`,
	}
	if !envContainsExactly(env, want) {
		t.Errorf("child env = %#v, want exactly %#v (absent base variables are omitted, not emitted empty)", env, want)
	}
	for _, kv := range env {
		if strings.HasSuffix(kv, "=") {
			t.Errorf("child env carries an empty entry %q; an absent base variable must be omitted", kv)
		}
	}
}

// TestExpandPlaceholdersNearMissErrors pins the corrected near-miss rule
// from the Task 13 fix-round-2 findings: classification happens per
// placeholder, on the ORIGINAL text, using a PREFIX match on the
// upper-cased inner token -- never substring containment. This is the
// "near-miss" half of the worked case table; the pass-through half is
// TestExpandPlaceholdersUnrelatedPlaceholderPassesThrough below. Each row
// here would have been (wrongly) accepted or (wrongly) rejected by the
// round-1 containment-based scan.
func TestExpandPlaceholdersNearMissErrors(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"typo'd AGENT_ENV prefix", "${AGENT_ENVV:HF_TOKEN}"},
		{"typo'd PORT suffix", "${PORTX}"},
		{"PORT with trailing suffix", "${PORT_1}"},
		{"lowercase port", "${port}"},
		{"empty AGENT_ENV name", "${AGENT_ENV:}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := Spec{Args: []string{tc.value}}
			getenv := func(string) string { return "" }

			_, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
			if err == nil {
				t.Fatalf("ExpandPlaceholders(%q) should error as a malformed near-miss placeholder", tc.value)
			}
		})
	}
}

// TestExpandPlaceholdersUnrelatedPlaceholderPassesThrough proves arbitrary
// "${...}" text unrelated to PORT/AGENT_ENV still passes through untouched --
// ExpandPlaceholders only owns those two tokens, and a model server's own
// templating syntax is a real possibility. Every one of these MERELY
// CONTAINS "PORT" or "AGENT_ENV" as a substring (round 1's bug: it used
// strings.Contains and refused all of these); the corrected rule only
// refuses a token whose upper-cased inner text STARTS WITH one of the two
// prefixes, so none of these should error.
func TestExpandPlaceholdersUnrelatedPlaceholderPassesThrough(t *testing.T) {
	cases := []string{
		"${FOO}",
		"${TRANSPORT}",
		"${EXPORT_DIR}",
		"${REPORT_INTERVAL}",
		"${IMPORT_PATH}",
		"${SUPPORT_EMAIL}",
		"${MY_AGENT_ENVIRONMENT}",
	}
	for _, value := range cases {
		t.Run(value, func(t *testing.T) {
			spec := Spec{Args: []string{value}}
			getenv := func(string) string { return "" }

			args, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
			if err != nil {
				t.Fatalf("ExpandPlaceholders(%s) should pass through untouched, got error: %v", value, err)
			}
			if want := []string{value}; !reflect.DeepEqual(args, want) {
				t.Errorf("args = %#v, want %#v", args, want)
			}
		})
	}
}

// TestExpandPlaceholdersNearMissScanIgnoresResolvedSecretValue pins the
// fix-round-2 fix for R2-2: the near-miss classification runs over the
// ORIGINAL text during a single substitution pass, never over the
// substituted RESULT. So a secret whose resolved value happens to contain a
// "${...}"-shaped substring that would itself look like a near-miss (e.g. a
// connection string or JSON blob carrying a stray "${AGENT_ENV:...}" or
// "${PORTX}" fragment) must NOT be rescanned, must NOT produce an error, and
// the secret must therefore never be echoed into any error message. getenv
// fails the test outright if ExpandPlaceholders ever asks it to resolve
// anything other than the one variable the spec actually referenced --
// proving no second scan/resolve pass ever happens over resolved output.
func TestExpandPlaceholdersNearMissScanIgnoresResolvedSecretValue(t *testing.T) {
	const secretValue = `postgres://user:pass@host/db?token=${AGENT_ENV:LOOKS_LIKE_A_NAME}&mode=${PORTX}`
	spec := Spec{Args: []string{"${AGENT_ENV:CONN_STRING}"}}
	getenv := func(k string) string {
		if k == "CONN_STRING" {
			return secretValue
		}
		// ExpandPlaceholders always probes the base names; anything else is
		// a second resolve pass over substituted output.
		if slices.Contains(baseEnvNames, k) {
			return ""
		}
		t.Fatalf("getenv called with unexpected key %q -- the near-miss scan must not rescan/re-resolve the substituted result", k)
		return ""
	}

	args, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
	if err != nil {
		t.Fatalf("ExpandPlaceholders should not error just because a RESOLVED secret value contains near-miss-shaped text: %v", err)
	}
	if want := []string{secretValue}; !reflect.DeepEqual(args, want) {
		t.Errorf("args = %#v, want %#v", args, want)
	}
}

// envContainsExactly reports whether env (os/exec-shaped "KEY=value"
// strings) contains EXACTLY the keys/values in want -- no more, no fewer --
// and no duplicate keys.
func envContainsExactly(env []string, want map[string]string) bool {
	if len(env) != len(want) {
		return false
	}
	got := make(map[string]string, len(env))
	for _, kv := range env {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			return false
		}
		if _, dup := got[parts[0]]; dup {
			return false // duplicate key
		}
		got[parts[0]] = parts[1]
	}
	return reflect.DeepEqual(got, want)
}

// TestExpandPlaceholdersArgsNeverNil proves an empty Args slice comes back
// non-nil, matching the module-wide rule that any collection a caller might
// range over or re-marshal must never be nil.
func TestExpandPlaceholdersArgsNeverNil(t *testing.T) {
	args, _, err := ExpandPlaceholders(Spec{}, 80, GPUVendorNVIDIA, func(string) string { return "" })
	if err != nil {
		t.Fatalf("ExpandPlaceholders: %v", err)
	}
	if args == nil {
		t.Error("args = nil, want a non-nil empty slice")
	}
}

// TestExpandPlaceholdersEnvSortedDeterministic proves the spec.Env portion of
// the result is emitted in a deterministic sorted-BY-KEY order -- not merely
// the same SET of entries across calls. Sorting both sides before comparing
// (as an earlier version of this test did) only proves set equality: it
// would pass just as well against a randomized-order implementation, since
// sorting erases the very order it claims to pin. Comparing the raw,
// unsorted env slice against a fixed expected order catches that.
func TestExpandPlaceholdersEnvSortedDeterministic(t *testing.T) {
	spec := Spec{Env: map[string]string{"ZKEY": "z", "AKEY": "a", "MKEY": "m"}}
	getenv := func(string) string { return "" }

	want := []string{"AKEY=a", "MKEY=m", "ZKEY=z"}
	for i := 0; i < 5; i++ {
		_, env, err := ExpandPlaceholders(spec, 80, GPUVendorNVIDIA, getenv)
		if err != nil {
			t.Fatalf("ExpandPlaceholders: %v", err)
		}
		if !reflect.DeepEqual(env, want) {
			t.Fatalf("iteration %d env = %#v, want %#v (sorted-by-key order)", i, env, want)
		}
	}
}

// windowsStyleGetenv is a case-INSENSITIVE environment lookup: exactly what
// os.Getenv does on Windows, where it goes through
// GetEnvironmentVariableW. Injecting it here is what makes S1 and S2
// testable on any host -- CI compiles nothing for Windows, so without this
// seam the entire class is unobservable from a Linux or macOS runner.
func windowsStyleGetenv(vars map[string]string) func(string) string {
	folded := make(map[string]string, len(vars))
	for k, v := range vars {
		folded[strings.ToUpper(k)] = v
	}
	return func(name string) string { return folded[strings.ToUpper(name)] }
}

// TestExpandPlaceholdersAgentEnvRefusalIsCaseInsensitive is S1's covering
// test. The refusal that keeps a gateway-supplied spec out of the agent's
// own OP_AGENT_* namespace was a case-SENSITIVE prefix test in front of a
// lookup that is case-INSENSITIVE on Windows, so every non-uppercase
// spelling walked past the guard and resolved the agent's gateway bearer
// token -- the credential that authenticates the certificate endpoint which
// issues a private key.
//
// The value has a path back to the gateway even from a model server with no
// network egress of its own: a child that echoes its argv (vLLM and
// llama.cpp both do at startup) and then exits non-zero has that output
// captured into LastError.StderrTail and reported upward in telemetry. The
// operator-controlled binary allowlist therefore narrows this, but does not
// close it.
func TestExpandPlaceholdersAgentEnvRefusalIsCaseInsensitive(t *testing.T) {
	const secret = "gateway-bearer-secret"
	getenv := windowsStyleGetenv(map[string]string{"OP_AGENT_TOKEN": secret})

	for _, name := range []string{
		"OP_AGENT_TOKEN",
		"op_agent_token",
		"Op_Agent_Token",
		"oP_aGeNt_ToKeN",
		"op_AGENT_token",
		"Op_agent_TOKEN",
	} {
		t.Run(name, func(t *testing.T) {
			for _, spec := range []Spec{
				{Args: []string{"--api-key", "${AGENT_ENV:" + name + "}"}},
				{Env: map[string]string{"TOKEN": "${AGENT_ENV:" + name + "}"}},
			} {
				args, env, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
				if err == nil {
					t.Fatalf("${AGENT_ENV:%s} resolved instead of being refused: args=%v env=%v", name, args, env)
				}
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaked the secret value: %q", err.Error())
				}
				for _, s := range append(append([]string{}, args...), env...) {
					if strings.Contains(s, secret) {
						t.Fatalf("the agent's own token reached the child: %q", s)
					}
				}
			}
		})
	}
}

// TestExpandPlaceholdersReservedEnvKeysAreCaseInsensitive is S2's covering
// test, the sibling of S1 with the same root cause and the same blast
// radius on Windows: os/exec deduplicates the child environment
// case-insensitively there, so a spec key spelled "Path" (the native
// Windows spelling) passed the case-sensitive reservation AND then won
// against the agent-provided PATH -- handing a gateway-supplied spec control
// over where the child resolves shared libraries and helper binaries, the
// exact class of risk Permit's absolute-binary-path rule exists to close.
//
// Extended with every name the OS-appropriate base added, because the
// reservation has to grow with the base or the new entries are override-able
// by any spec. SystemRoot is the sharpest of them: it is spelled in mixed
// case on every Windows host, it is part of the system DLL search path, and
// a spec-supplied one is the same DLL-resolution hijack as a spec-supplied
// Path. The table below is deliberately written in the spellings an operator
// would actually type.
func TestExpandPlaceholdersReservedEnvKeysAreCaseInsensitive(t *testing.T) {
	getenv := windowsStyleGetenv(map[string]string{
		"PATH":         `C:\Windows\System32`,
		"HOME":         `C:\Users\agent`,
		"USERPROFILE":  `C:\Users\agent`,
		"LOCALAPPDATA": `C:\Users\agent\AppData\Local`,
		"SystemRoot":   `C:\Windows`,
		"windir":       `C:\Windows`,
	})

	for _, key := range []string{
		"PATH", "Path", "path", "pAtH",
		"HOME", "Home", "home", "hOmE",
		"USERPROFILE", "UserProfile", "userprofile", "UsErPrOfIlE",
		"LOCALAPPDATA", "LocalAppData", "localappdata", "lOcAlAppDaTa",
		"SYSTEMROOT", "SystemRoot", "systemroot", "sYsTeMrOoT",
		"WINDIR", "windir", "WinDir", "wInDiR",
	} {
		t.Run(key, func(t *testing.T) {
			spec := Spec{Env: map[string]string{key: `C:\attacker\bin`}}
			_, env, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
			if err == nil {
				t.Fatalf("a spec env key %q was accepted; the child environment would be %v", key, env)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error = %q, want it to name the refused key %q", err.Error(), key)
			}
		})
	}

	// The reservation is scoped to those names, not to anything that merely
	// resembles them: a spec may still set its own PATH-adjacent variables.
	spec := Spec{Env: map[string]string{"LD_LIBRARY_PATH": "/opt/cuda/lib64", "HOMEBREW_PREFIX": "/opt/homebrew", "PATHOLOGY": "x", "USERPROFILE_BACKUP": "x", "SYSTEMROOTS": "x"}}
	if _, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv); err != nil {
		t.Fatalf("ExpandPlaceholders refused a legitimate env key: %v", err)
	}

	// The operator lever the reservation must NOT close. Because HOME is
	// reserved in every spelling, a Windows operator cannot redirect a
	// child's home or cache that way -- so the variables deliberately left
	// OUT of the base (baseEnvNames documents each) have to stay settable,
	// or the reservation would take away the last workaround. Pinned as a
	// contract, not left to the negative space of the list above.
	lever := Spec{Env: map[string]string{
		"HOMEDRIVE":      "D:",
		"HOMEPATH":       `\models\home`,
		"TEMP":           `D:\models\tmp`,
		"TMP":            `D:\models\tmp`,
		"APPDATA":        `D:\models\roaming`,
		"HF_HOME":        `D:\models\huggingface`,
		"HF_HUB_CACHE":   `D:\models\huggingface\hub`,
		"LLAMA_CACHE":    `D:\models\llama.cpp`,
		"XDG_CACHE_HOME": "/mnt/models/.cache",
	}}
	if _, _, err := ExpandPlaceholders(lever, 8080, GPUVendorNVIDIA, getenv); err != nil {
		t.Fatalf("ExpandPlaceholders refused a cache/home redirection an operator legitimately needs: %v", err)
	}
}

// --- set_visible_devices / ${HOST_GPU_IDS} -----------------------------------

// visibleDevicesSpec is the standard fixture for the tests below: a spec on
// two NON-CONTIGUOUS, DESCENDING-ordered host GPUs. Both properties are
// deliberate. Non-contiguous and not starting at 0 means the emitted value
// ("5,2") can never be confused with the child-side numbering the same list
// produces (0,1) -- trap 4. Descending in the declaration proves the declared
// order is preserved rather than accidentally agreeing with a sort.
func visibleDevicesSpec() Spec {
	return Spec{
		ID:                "s1",
		Model:             "gw-model",
		UpstreamModel:     "app-model",
		Binary:            "/usr/bin/llama-server",
		Args:              []string{"--port", "${PORT}"},
		Env:               map[string]string{"LLAMA_CACHE": "/mnt/models"},
		GPUs:              []SpecGPU{{Index: 5, VRAMMB: 18000}, {Index: 2, VRAMMB: 18000}},
		SetVisibleDevices: true,
	}
}

// envValue returns the value of name in an os/exec-shaped environment, and
// whether it was present at all. Deliberately reports PRESENCE separately from
// value: "the variable is absent" and "the variable is set to the empty
// string" are the two states this whole feature exists to keep apart (trap 1),
// so a helper that collapsed them would make its own tests unable to see the
// bug.
func envValue(env []string, name string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

// TestParseGPUVendorMapsCollectorNames pins the seam between
// collector.GPUCollector.Name() and GPUVendor. The three names are the exact
// strings the collectors return; anything else -- an unrecognised vendor, an
// empty name, a case variant -- is GPUVendorNone, which makes
// set_visible_devices a no-op rather than an error on that host.
func TestParseGPUVendorMapsCollectorNames(t *testing.T) {
	cases := []struct {
		name string
		want GPUVendor
	}{
		{"nvidia", GPUVendorNVIDIA},
		{"amd", GPUVendorAMD},
		{"apple", GPUVendorApple},
		{"", GPUVendorNone},
		{"intel", GPUVendorNone},
		{"NVIDIA", GPUVendorNone}, // the collectors are lower-case; nothing else is claimed
		{"nvidia-smi", GPUVendorNone},
	}
	for _, tc := range cases {
		t.Run("name="+tc.name, func(t *testing.T) {
			if got := ParseGPUVendor(tc.name); got != tc.want {
				t.Errorf("ParseGPUVendor(%q) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestVisibleDevicesVarPerVendor pins the vendor -> variable mapping,
// including the two no-op vendors.
//
// The AMD row is the one worth reading: it asserts ROCR_VISIBLE_DEVICES and
// NOTHING ELSE. HIP_VISIBLE_DEVICES indexes WITHIN what ROCR already filtered,
// so setting both to the same host list leaves the child with no usable device
// at all -- see VisibleDevicesVar. This test is what fails if a later reader
// "completes" the mapping by adding the second variable.
func TestVisibleDevicesVarPerVendor(t *testing.T) {
	cases := []struct {
		vendor GPUVendor
		want   string
	}{
		{GPUVendorNVIDIA, "CUDA_VISIBLE_DEVICES"},
		{GPUVendorAMD, "ROCR_VISIBLE_DEVICES"},
		{GPUVendorApple, ""},
		{GPUVendorNone, ""},
		{GPUVendor("intel"), ""},
	}
	for _, tc := range cases {
		t.Run("vendor="+string(tc.vendor), func(t *testing.T) {
			if got := VisibleDevicesVar(tc.vendor); got != tc.want {
				t.Errorf("VisibleDevicesVar(%q) = %q, want %q", tc.vendor, got, tc.want)
			}
			if tc.vendor == GPUVendorAMD && VisibleDevicesVar(tc.vendor) == "HIP_VISIBLE_DEVICES" {
				t.Fatal("AMD must select the ROCR level only: HIP_VISIBLE_DEVICES indexes within what ROCR already filtered")
			}
		})
	}
}

// TestExpandPlaceholdersSetsVisibleDevicesPerVendor is the feature's main
// behavioural pin: with set_visible_devices on, the child's environment
// carries the vendor-appropriate variable set to the spec's own HOST indices,
// in the operator's declared order and comma-separated -- and on Apple or a
// host with no recognised GPU stack it carries NO visibility variable at all,
// with no error.
func TestExpandPlaceholdersSetsVisibleDevicesPerVendor(t *testing.T) {
	getenv := func(string) string { return "" }

	cases := []struct {
		vendor  GPUVendor
		wantVar string // "" = nothing must be set
	}{
		{GPUVendorNVIDIA, "CUDA_VISIBLE_DEVICES"},
		{GPUVendorAMD, "ROCR_VISIBLE_DEVICES"},
		{GPUVendorApple, ""},
		{GPUVendorNone, ""},
	}
	for _, tc := range cases {
		t.Run("vendor="+string(tc.vendor), func(t *testing.T) {
			_, env, err := ExpandPlaceholders(visibleDevicesSpec(), 8080, tc.vendor, getenv)
			if err != nil {
				t.Fatalf("ExpandPlaceholders: %v", err)
			}
			// Whatever this vendor does, it must never set a variable that
			// belongs to another one, and never the HIP level.
			for _, name := range []string{"CUDA_VISIBLE_DEVICES", "ROCR_VISIBLE_DEVICES", "HIP_VISIBLE_DEVICES"} {
				value, present := envValue(env, name)
				switch {
				case name == tc.wantVar:
					if !present {
						t.Fatalf("env %v is missing %s", env, name)
					}
					if value != "5,2" {
						t.Errorf("%s = %q, want %q (the spec's own HOST indices, in declared order)", name, value, "5,2")
					}
				case present:
					t.Errorf("env %v sets %s=%q; only %q may be set on vendor %q", env, name, value, tc.wantVar, tc.vendor)
				}
			}
			// The no-op vendors must be a SUCCESS, not an error, and must
			// leave the rest of the environment untouched.
			if v, ok := envValue(env, "LLAMA_CACHE"); !ok || v != "/mnt/models" {
				t.Errorf("env %v lost the spec's own entries", env)
			}
		})
	}
}

// TestExpandPlaceholdersVisibleDevicesEmptyGPUListRefused is trap 1: an EMPTY
// visibility value does not mean "no restriction", it means NOTHING IS
// VISIBLE. The combination must be refused rather than emitted -- on EVERY
// vendor, including the two that would have set nothing anyway, because a spec
// document that is silently fine on a laptop and hides every card on the GPU
// box is the worst available behaviour.
func TestExpandPlaceholdersVisibleDevicesEmptyGPUListRefused(t *testing.T) {
	getenv := func(string) string { return "" }

	for _, gpus := range [][]SpecGPU{nil, {}} {
		for _, vendor := range []GPUVendor{GPUVendorNVIDIA, GPUVendorAMD, GPUVendorApple, GPUVendorNone} {
			t.Run(fmt.Sprintf("vendor=%s/gpus=%v", vendor, gpus), func(t *testing.T) {
				spec := visibleDevicesSpec()
				spec.GPUs = gpus
				_, env, err := ExpandPlaceholders(spec, 8080, vendor, getenv)
				if err == nil {
					t.Fatalf("set_visible_devices with no gpus was accepted; the child environment would be %v", env)
				}
				if !strings.Contains(err.Error(), "set_visible_devices") {
					t.Errorf("error = %q, want it to name the option the operator has to turn off", err.Error())
				}
				if !strings.Contains(err.Error(), "no gpus") {
					t.Errorf("error = %q, want it to state the actual cause (the spec declares no gpus)", err.Error())
				}
			})
		}
	}
}

// TestExpandPlaceholdersVisibleDevicesConflictRefused is trap 3: the option
// and a hand-set visibility variable in the same spec are refused, never
// silently resolved in either direction. Case-folded, like every other env-key
// rule here, because Windows resolves environment names case-insensitively.
//
// HIP_VISIBLE_DEVICES is in the refused set although this agent never SETS it:
// it double-filters what ROCR_VISIBLE_DEVICES already filtered, which is the
// trap in its purest form.
func TestExpandPlaceholdersVisibleDevicesConflictRefused(t *testing.T) {
	getenv := func(string) string { return "" }

	for _, key := range []string{
		"CUDA_VISIBLE_DEVICES", "cuda_visible_devices", "Cuda_Visible_Devices",
		"ROCR_VISIBLE_DEVICES", "rocr_visible_devices",
		"HIP_VISIBLE_DEVICES", "Hip_Visible_Devices",
	} {
		// Refused on EVERY vendor, not only the one that would have set this
		// particular variable: the portal enforces the identical rule at save
		// time and cannot know the agent's hardware, and two validators that
		// disagree are the defect this option exists to remove.
		for _, vendor := range []GPUVendor{GPUVendorNVIDIA, GPUVendorAMD, GPUVendorApple, GPUVendorNone} {
			t.Run(fmt.Sprintf("%s/vendor=%s", key, vendor), func(t *testing.T) {
				spec := visibleDevicesSpec()
				spec.Env = map[string]string{key: "0,1"}
				_, env, err := ExpandPlaceholders(spec, 8080, vendor, getenv)
				if err == nil {
					t.Fatalf("set_visible_devices alongside a hand-set %s was accepted; the child environment would be %v", key, env)
				}
				if !strings.Contains(err.Error(), key) {
					t.Errorf("error = %q, want it to name the conflicting key %q", err.Error(), key)
				}
			})
		}
	}

	// The SAME keys are perfectly fine when the option is OFF -- the refusal
	// is about the ambiguity between two sources, not about the variables
	// themselves. An operator who wants to pin devices by hand still can.
	for _, key := range []string{"CUDA_VISIBLE_DEVICES", "ROCR_VISIBLE_DEVICES", "HIP_VISIBLE_DEVICES"} {
		t.Run(key+"/option off", func(t *testing.T) {
			spec := visibleDevicesSpec()
			spec.SetVisibleDevices = false
			spec.Env = map[string]string{key: "0,1"}
			_, env, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
			if err != nil {
				t.Fatalf("a hand-set %s with the option OFF must still be accepted: %v", key, err)
			}
			if v, ok := envValue(env, key); !ok || v != "0,1" {
				t.Errorf("env %v did not carry the operator's own %s verbatim", env, key)
			}
		})
	}

	// NOT owned, and that has to stay true: ONEAPI_DEVICE_SELECTOR (and any
	// other runtime-specific selector) is the ${HOST_GPU_IDS} escape hatch's
	// territory. Refusing it would break the very composition the placeholder
	// exists for.
	t.Run("ONEAPI_DEVICE_SELECTOR composes with the option", func(t *testing.T) {
		spec := visibleDevicesSpec()
		spec.Env = map[string]string{"ONEAPI_DEVICE_SELECTOR": "level_zero:${HOST_GPU_IDS}"}
		_, env, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
		if err != nil {
			t.Fatalf("ONEAPI_DEVICE_SELECTOR must compose with set_visible_devices, got: %v", err)
		}
		if v, _ := envValue(env, "ONEAPI_DEVICE_SELECTOR"); v != "level_zero:5,2" {
			t.Errorf("ONEAPI_DEVICE_SELECTOR = %q, want %q", v, "level_zero:5,2")
		}
		if v, _ := envValue(env, "CUDA_VISIBLE_DEVICES"); v != "5,2" {
			t.Errorf("CUDA_VISIBLE_DEVICES = %q, want the option still to have set %q", v, "5,2")
		}
	})
}

// TestExpandPlaceholdersVisibleDevicesOffIsUnchanged is the every-existing-
// deployment guard: a spec with set_visible_devices FALSE must receive exactly
// the environment it received before this option existed -- same entries, same
// order, byte for byte -- on every vendor, including a spec that declares GPUs
// (the case where the feature has something it COULD have emitted).
//
// The expected list is written out literally rather than derived, so a future
// edit that adds, reorders or reformats an entry has to change this test
// deliberately instead of silently agreeing with itself.
func TestExpandPlaceholdersVisibleDevicesOffIsUnchanged(t *testing.T) {
	getenv := func(name string) string {
		if name == "PATH" {
			return "/usr/bin"
		}
		return ""
	}
	want := []string{
		"PATH=/usr/bin",
		"LLAMA_CACHE=/mnt/models",
		"NCCL_DEBUG=INFO",
	}

	for _, vendor := range []GPUVendor{GPUVendorNVIDIA, GPUVendorAMD, GPUVendorApple, GPUVendorNone} {
		t.Run("vendor="+string(vendor), func(t *testing.T) {
			spec := visibleDevicesSpec()
			spec.SetVisibleDevices = false
			spec.Env = map[string]string{"LLAMA_CACHE": "/mnt/models", "NCCL_DEBUG": "INFO"}

			args, env, err := ExpandPlaceholders(spec, 41123, vendor, getenv)
			if err != nil {
				t.Fatalf("ExpandPlaceholders: %v", err)
			}
			if !reflect.DeepEqual(env, want) {
				t.Errorf("env = %#v, want %#v (a spec with the option off must be byte-identical to the pre-feature behaviour)", env, want)
			}
			if !reflect.DeepEqual(args, []string{"--port", "41123"}) {
				t.Errorf("args = %#v, want the unchanged pre-feature expansion", args)
			}
		})
	}
}

// TestHostGPUIDsPreservesOrderAndDedups pins the order contract: the value is
// spec.GPUs in the operator's DECLARED array order, deduplicated
// (first-occurrence wins), NOT sorted. The gateway now persists an explicit
// position column, so the array order is the operator's choice and must reach
// the visibility variable and ${HOST_GPU_IDS} intact.
func TestHostGPUIDsPreservesOrderAndDedups(t *testing.T) {
	cases := []struct {
		name string
		gpus []SpecGPU
		want string
	}{
		{"single", []SpecGPU{{Index: 3}}, "3"},
		{"declared order preserved, not sorted", []SpecGPU{{Index: 5}, {Index: 2}}, "5,2"},
		{"three cards keep operator order", []SpecGPU{{Index: 7}, {Index: 0}, {Index: 4}}, "7,0,4"},
		{"duplicate collapses, first occurrence wins", []SpecGPU{{Index: 3}, {Index: 2}, {Index: 3}}, "3,2"},
		{"no gpus is empty", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostGPUIDs(Spec{GPUs: tc.gpus}); got != tc.want {
				t.Errorf("hostGPUIDs(%v) = %q, want %q", tc.gpus, got, tc.want)
			}
		})
	}
}

// TestExpandPlaceholdersHostGPUIDs pins the placeholder itself: it resolves in
// args AND in env values, in the operator's declared order, deduplicated,
// comma-separated.
//
// Deduplication is not cosmetic: CUDA stops parsing the visible-devices list
// at the first repeated or invalid entry, so "1,1,2" would silently yield ONE
// visible device rather than three. The gateway refuses a duplicate index at
// save time; a hand-written file-mode document has no such gate.
func TestExpandPlaceholdersHostGPUIDs(t *testing.T) {
	getenv := func(string) string { return "" }

	cases := []struct {
		name string
		gpus []SpecGPU
		want string
	}{
		{"single", []SpecGPU{{Index: 3}}, "3"},
		{"declared order preserved, not sorted", []SpecGPU{{Index: 5}, {Index: 2}}, "5,2"},
		{"three cards", []SpecGPU{{Index: 7}, {Index: 0}, {Index: 4}}, "7,0,4"},
		{"duplicates collapse", []SpecGPU{{Index: 1}, {Index: 1}, {Index: 2}}, "1,2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := Spec{
				Args: []string{"--devices", "${HOST_GPU_IDS}"},
				Env:  map[string]string{"ONEAPI_DEVICE_SELECTOR": "level_zero:${HOST_GPU_IDS}"},
				GPUs: tc.gpus,
			}
			args, env, err := ExpandPlaceholders(spec, 8080, GPUVendorNone, getenv)
			if err != nil {
				t.Fatalf("ExpandPlaceholders: %v", err)
			}
			if args[1] != tc.want {
				t.Errorf("args[1] = %q, want %q", args[1], tc.want)
			}
			if v, _ := envValue(env, "ONEAPI_DEVICE_SELECTOR"); v != "level_zero:"+tc.want {
				t.Errorf("ONEAPI_DEVICE_SELECTOR = %q, want %q", v, "level_zero:"+tc.want)
			}
		})
	}

	// Empty GPU list: the same "an empty value means nothing is visible"
	// reasoning as the checkbox, and the same reasoning as ${MODEL} on an
	// empty upstream_model -- refuse where the placeholder is USED.
	t.Run("no gpus is a hard error", func(t *testing.T) {
		spec := Spec{Env: map[string]string{"ONEAPI_DEVICE_SELECTOR": "level_zero:${HOST_GPU_IDS}"}}
		if _, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNone, getenv); err == nil {
			t.Fatal("${HOST_GPU_IDS} on a spec with no gpus should refuse, not substitute an empty string")
		}
	})
	t.Run("unused on a spec with no gpus is fine", func(t *testing.T) {
		spec := Spec{Args: []string{"--port", "${PORT}"}}
		if _, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNone, getenv); err != nil {
			t.Fatalf("a spec that never mentions ${HOST_GPU_IDS} must be unaffected by having no gpus: %v", err)
		}
	})

	// EXACT MATCH ONLY, the ${MODEL} decision applied for the same reason:
	// ${GPU_IDS_FILE} and friends are plausible tokens a model server's own
	// templating expands, and a prefix rule would refuse every one of them.
	// The accepted cost is stated here rather than left to be discovered: a
	// near-miss reaches the child as literal text.
	for _, value := range []string{
		"${GPU_IDS}", "${HOST_GPU_ID}", "${HOST_GPU_IDS_JSON}", "${host_gpu_ids}", "${GPU_IDS_FILE}",
	} {
		t.Run("passes through literally: "+value, func(t *testing.T) {
			spec := Spec{Args: []string{value}, GPUs: []SpecGPU{{Index: 1}}}
			args, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNone, getenv)
			if err != nil {
				t.Fatalf("ExpandPlaceholders(%s) should pass through untouched, got error: %v", value, err)
			}
			if args[0] != value {
				t.Errorf("args[0] = %q, want the literal %q", args[0], value)
			}
		})
	}
}

// TestExpandPlaceholdersHostGPUIDsAreHostIndices is trap 4, pinned as its own
// test because it is the claim the NAME of the placeholder makes.
//
// The value is the spec's declared indices as the HOST sees them. The child
// launched with those indices renumbers from 0 -- with CUDA_VISIBLE_DEVICES=4,6
// it enumerates devices 0 and 1 -- so the same digits mean different cards on
// the two sides of the exec boundary. Anything that ever made this emit
// child-side indices (0,1,2,... for n GPUs) would break the admission
// arithmetic and the VRAM measurement mapping silently, since both are keyed
// on host indices.
func TestExpandPlaceholdersHostGPUIDsAreHostIndices(t *testing.T) {
	getenv := func(string) string { return "" }
	spec := Spec{
		Args:              []string{"--devices", "${HOST_GPU_IDS}"},
		GPUs:              []SpecGPU{{Index: 4}, {Index: 6}},
		SetVisibleDevices: true,
	}
	args, env, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
	if err != nil {
		t.Fatalf("ExpandPlaceholders: %v", err)
	}
	const wantHost = "4,6"
	const childSide = "0,1" // what the CHILD will enumerate; never what we emit
	if args[1] != wantHost {
		t.Errorf("${HOST_GPU_IDS} = %q, want the HOST indices %q", args[1], wantHost)
	}
	if args[1] == childSide {
		t.Fatalf("${HOST_GPU_IDS} resolved to the CHILD-side numbering %q; admission budgets and VRAM measurements are keyed on HOST indices", childSide)
	}
	if v, _ := envValue(env, "CUDA_VISIBLE_DEVICES"); v != wantHost {
		t.Errorf("CUDA_VISIBLE_DEVICES = %q, want the HOST indices %q", v, wantHost)
	}
}

// TestExpandPlaceholdersDeviceLists pins the three llama.cpp --device
// placeholders: each emits <Backend><host index> per selected GPU, in the
// operator's declared order, deduplicated, comma-joined -- the same ordered
// index list as ${HOST_GPU_IDS}, only backend-prefixed.
func TestExpandPlaceholdersDeviceLists(t *testing.T) {
	getenv := func(string) string { return "" }
	cases := []struct {
		placeholder string
		want        string
	}{
		{"${CUDA_DEVICES}", "CUDA3,CUDA2"},
		{"${VULKAN_DEVICES}", "Vulkan3,Vulkan2"},
		{"${METAL_DEVICES}", "MTL3,MTL2"},
	}
	for _, tc := range cases {
		t.Run(tc.placeholder, func(t *testing.T) {
			spec := Spec{
				Args: []string{"--device", tc.placeholder},
				GPUs: []SpecGPU{{Index: 3}, {Index: 2}}, // non-ascending: proves order, not sort
			}
			args, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
			if err != nil {
				t.Fatalf("ExpandPlaceholders: %v", err)
			}
			if args[1] != tc.want {
				t.Errorf("%s = %q, want %q (<prefix><host index> in operator order)", tc.placeholder, args[1], tc.want)
			}
		})
		t.Run(tc.placeholder+"/no gpus refused", func(t *testing.T) {
			spec := Spec{Args: []string{"--device", tc.placeholder}}
			if _, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv); err == nil {
				t.Fatalf("%s on a spec with no gpus must refuse, not substitute an empty device list", tc.placeholder)
			}
		})
	}

	t.Run("dedup keeps first-occurrence order", func(t *testing.T) {
		spec := Spec{Args: []string{"${CUDA_DEVICES}"}, GPUs: []SpecGPU{{Index: 3}, {Index: 2}, {Index: 3}}}
		args, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
		if err != nil {
			t.Fatalf("ExpandPlaceholders: %v", err)
		}
		if args[0] != "CUDA3,CUDA2" {
			t.Errorf("args[0] = %q, want CUDA3,CUDA2", args[0])
		}
	})

	// A near-miss passes through literally, same rule as ${HOST_GPU_IDS}: these
	// tokens are exact-match, and neither starts with PORT/AGENT_ENV.
	for _, value := range []string{"${CUDA_DEVICE}", "${cuda_devices}", "${METAL_DEVICES_JSON}"} {
		t.Run("passes through literally: "+value, func(t *testing.T) {
			spec := Spec{Args: []string{value}, GPUs: []SpecGPU{{Index: 1}}}
			args, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
			if err != nil {
				t.Fatalf("ExpandPlaceholders(%s) should pass through untouched, got: %v", value, err)
			}
			if args[0] != value {
				t.Errorf("args[0] = %q, want the literal %q", args[0], value)
			}
		})
	}
}

// TestExpandPlaceholdersVisibleDevicesArgsModeSkipsEnvInjection pins Part B:
// in "args" mode the agent injects NO visibility env var (the child sees every
// card and --device does the selecting in host numbering); in "env" mode, and
// for an empty/unknown mode (an older gateway never sends the field), it injects
// the vendor variable exactly as before -- now order-preserving.
func TestExpandPlaceholdersVisibleDevicesArgsModeSkipsEnvInjection(t *testing.T) {
	getenv := func(string) string { return "" }
	base := func() Spec {
		return Spec{
			Binary:            "/usr/bin/llama-server",
			Args:              []string{"--device", "${CUDA_DEVICES}"},
			GPUs:              []SpecGPU{{Index: 3}, {Index: 2}},
			SetVisibleDevices: true,
		}
	}

	t.Run("args mode injects no visibility variable", func(t *testing.T) {
		spec := base()
		spec.VisibleDevicesMode = VisibleDevicesModeArgs
		args, env, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
		if err != nil {
			t.Fatalf("ExpandPlaceholders: %v", err)
		}
		if v, present := envValue(env, "CUDA_VISIBLE_DEVICES"); present {
			t.Errorf("env injected CUDA_VISIBLE_DEVICES=%q in args mode; the child must see every card so --device numbering stays the host's", v)
		}
		if args[1] != "CUDA3,CUDA2" {
			t.Errorf("args[1] = %q, want the expanded device list CUDA3,CUDA2", args[1])
		}
	})

	for _, mode := range []string{VisibleDevicesModeEnv, ""} {
		t.Run("mode="+mode+" injects the visibility variable in operator order", func(t *testing.T) {
			spec := base()
			spec.VisibleDevicesMode = mode
			_, env, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv)
			if err != nil {
				t.Fatalf("ExpandPlaceholders: %v", err)
			}
			if v, ok := envValue(env, "CUDA_VISIBLE_DEVICES"); !ok || v != "3,2" {
				t.Errorf("CUDA_VISIBLE_DEVICES = %q (present=%v), want 3,2 (env mode, declared order)", v, ok)
			}
		})
	}
}

// TestExpandPlaceholdersVisibleDevicesConflictHoldsInBothModes pins that the
// trap-3 refusal (a hand-set visibility var alongside the checkbox) is NOT
// weakened by args mode: a hand-set CUDA_VISIBLE_DEVICES in args mode would
// remap the CUDA namespace and break the child's --device numbering, so it is
// refused there too.
func TestExpandPlaceholdersVisibleDevicesConflictHoldsInBothModes(t *testing.T) {
	getenv := func(string) string { return "" }
	for _, mode := range []string{VisibleDevicesModeEnv, VisibleDevicesModeArgs} {
		t.Run("mode="+mode, func(t *testing.T) {
			spec := Spec{
				Args:               []string{"--device", "${CUDA_DEVICES}"},
				Env:                map[string]string{"CUDA_VISIBLE_DEVICES": "0,1"},
				GPUs:               []SpecGPU{{Index: 3}, {Index: 2}},
				SetVisibleDevices:  true,
				VisibleDevicesMode: mode,
			}
			if _, _, err := ExpandPlaceholders(spec, 8080, GPUVendorNVIDIA, getenv); err == nil {
				t.Fatalf("a hand-set CUDA_VISIBLE_DEVICES must be refused in %q mode too", mode)
			}
		})
	}
}
