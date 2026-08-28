// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
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

// TestPermitEmptyWorkDirHasItsOwnMessage pins the improved diagnostic: an
// absent work_dir under a configured AllowedDirs must say so, not render as
// `work_dir "" is not within any allowed directory` (which reads like a
// containment near-miss). This text is the only explanation an operator
// gets -- it surfaces verbatim as Status.LastError.Message next to
// StateNotPermitted -- so it must name the agent-side setting, and must NOT
// leak the configured directory VALUES upward to the gateway.
func TestPermitEmptyWorkDirHasItsOwnMessage(t *testing.T) {
	p := LocalPolicy{
		AllowedBinaries: []string{"/usr/bin/ollama"},
		AllowedDirs:     []string{"/srv/models", "/data/weights"},
	}
	err := p.Permit(Spec{ID: "s1", Binary: "/usr/bin/ollama", WorkDir: ""})
	if err == nil {
		t.Fatal("Permit with an empty work_dir under a configured AllowedDirs = nil, want a refusal")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no work_dir") {
		t.Errorf("Permit error = %q, want it to state plainly that the spec sets no work_dir", msg)
	}
	if !strings.Contains(msg, "OP_AGENT_RUNTIME_ALLOWED_DIRS") {
		t.Errorf("Permit error = %q, want it to name the agent-side setting that caused the restriction", msg)
	}
	for _, dir := range p.AllowedDirs {
		if strings.Contains(msg, dir) {
			t.Errorf("Permit error = %q leaks the configured allowed directory %q; this message travels to the gateway", msg, dir)
		}
	}
	if strings.Contains(msg, `work_dir "" is not within`) {
		t.Errorf("Permit error = %q is still the generic containment wording", msg)
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
		{"empty work_dir", "", false},
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

// TestExpandPlaceholdersPort proves ${PORT} in args resolves to the chosen
// listen port, given as a plain decimal string.
func TestExpandPlaceholdersPort(t *testing.T) {
	spec := Spec{
		Args: []string{"serve", "--port", "${PORT}", "--url", "http://127.0.0.1:${PORT}/health"},
	}
	getenv := func(string) string { return "" }

	args, _, err := ExpandPlaceholders(spec, 41123, getenv)
	if err != nil {
		t.Fatalf("ExpandPlaceholders: %v", err)
	}
	want := []string{"serve", "--port", "41123", "--url", "http://127.0.0.1:41123/health"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %#v, want %#v", args, want)
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

	args, env, err := ExpandPlaceholders(spec, 8080, getenv)
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

	_, _, err := ExpandPlaceholders(spec, 8080, getenv)
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

	_, _, err := ExpandPlaceholders(spec, 8080, getenv)
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

	args, _, err := ExpandPlaceholders(spec, 8080, getenv)
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

	_, _, err := ExpandPlaceholders(spec, 8080, getenv)
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

	_, _, err := ExpandPlaceholders(spec, 8080, getenv)
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

	_, _, err := ExpandPlaceholders(spec, 8080, getenv)
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

	_, env, err := ExpandPlaceholders(spec, 9090, getenv)
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
			_, _, err := ExpandPlaceholders(spec, 8080, getenv)
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

	_, _, err := ExpandPlaceholders(spec, 8080, getenv)
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

	_, env, err := ExpandPlaceholders(spec, 1234, getenv)
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
	_, env, err := ExpandPlaceholders(spec, 9090, getenv)
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
	_, env, err := ExpandPlaceholders(spec, 9090, getenv)
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

	_, env, err := ExpandPlaceholders(Spec{}, 1234, getenv)
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

			_, _, err := ExpandPlaceholders(spec, 8080, getenv)
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

			args, _, err := ExpandPlaceholders(spec, 8080, getenv)
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

	args, _, err := ExpandPlaceholders(spec, 8080, getenv)
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
	args, _, err := ExpandPlaceholders(Spec{}, 80, func(string) string { return "" })
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
		_, env, err := ExpandPlaceholders(spec, 80, getenv)
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
				args, env, err := ExpandPlaceholders(spec, 8080, getenv)
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
			_, env, err := ExpandPlaceholders(spec, 8080, getenv)
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
	if _, _, err := ExpandPlaceholders(spec, 8080, getenv); err != nil {
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
	if _, _, err := ExpandPlaceholders(lever, 8080, getenv); err != nil {
		t.Fatalf("ExpandPlaceholders refused a cache/home redirection an operator legitimately needs: %v", err)
	}
}
