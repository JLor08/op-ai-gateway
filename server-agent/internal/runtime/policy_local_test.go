// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"reflect"
	"strings"
	"testing"
)

// TestPermitEmptyAllowlistRejectsEverything pins spec decision 2: an operator
// who has configured no allowed binaries at all gets a hard refusal for
// EVERY spec, not a permissive default. The error must name the allowlist
// (so the operator understands WHY, not just THAT it failed) and must not
// depend on WorkDir/AllowedDirs at all -- the binary check comes first.
func TestPermitEmptyAllowlistRejectsEverything(t *testing.T) {
	p := LocalPolicy{}
	spec := Spec{ID: "s1", Binary: "/usr/bin/ollama", WorkDir: "/srv/models"}

	err := p.Permit(spec)
	if err == nil {
		t.Fatal("Permit with an empty allowlist should refuse every spec")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("Permit error = %q, want it to mention the allowlist", err.Error())
	}
	if !strings.Contains(err.Error(), spec.Binary) {
		t.Errorf("Permit error = %q, want it to name the binary %q", err.Error(), spec.Binary)
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

// TestExpandPlaceholdersNearMissErrors pins the judgment call: a typo'd
// PORT/AGENT_ENV placeholder is not silently passed through like arbitrary
// templating syntax (a model server's own "${...}" syntax is a real
// possibility this function must not break) -- it is a confusing downstream
// auth or bind failure waiting to happen, so it errors up front naming the
// malformed token instead.
func TestExpandPlaceholdersNearMissErrors(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"typo'd AGENT_ENV prefix", "${AGENT_ENVV:HF_TOKEN}"},
		{"typo'd PORT suffix", "${PORTX}"},
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
// templating syntax is a real possibility.
func TestExpandPlaceholdersUnrelatedPlaceholderPassesThrough(t *testing.T) {
	spec := Spec{Args: []string{"${FOO}"}}
	getenv := func(string) string { return "" }

	args, _, err := ExpandPlaceholders(spec, 8080, getenv)
	if err != nil {
		t.Fatalf("ExpandPlaceholders(${FOO}) should pass through untouched, got error: %v", err)
	}
	if want := []string{"${FOO}"}; !reflect.DeepEqual(args, want) {
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
