// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"strings"
	"testing"
)

// These tests pin the RESOLVED COMMAND: the reportable, provenance-masked view
// of what a managed process was actually launched with (command.go).
//
// Both halves matter equally, and the second is the one that is easy to lose:
//
//   - A secret resolved from ${AGENT_ENV:NAME} must be masked WHEREVER it
//     landed -- in an argument as much as in an env value. Arguments were never
//     masked before this existed, and that was the branch's documented gap.
//   - A value the agent computed or the operator wrote must NOT be masked.
//     `--port 54331`, `--ctx-size 262144` and `CUDA_VISIBLE_DEVICES=2,3` are
//     the entire reason an operator opens the log view; a blanket mask would be
//     trivially "safe" and completely useless.

// commandFor is the common setup: expand spec against getenv and build the
// reportable command, failing the test on any expansion error.
func commandFor(t *testing.T, spec Spec, port int, vendor GPUVendor, getenv func(string) string, fromFile bool) ResolvedCommand {
	t.Helper()
	ex, err := expandSpec(spec, port, vendor, getenv)
	if err != nil {
		t.Fatalf("expandSpec: %v", err)
	}
	return ex.resolvedCommand(spec, fromFile)
}

func joinArgs(args []string) string { return strings.Join(args, " ") }

// TestResolvedCommandReportsPlaceholdersExpanded is the feature's whole point:
// the operator typed a template, and what they get back is what RAN. Every one
// of the four placeholders is resolved here, in an argument and in an env
// value, because a panel that echoed the template would answer nothing.
func TestResolvedCommandReportsPlaceholdersExpanded(t *testing.T) {
	spec := Spec{
		Binary:        "/opt/llama/llama-server",
		WorkDir:       "/srv/models",
		UpstreamModel: "qwen-coder-32b",
		Args: []string{
			"--port", "${PORT}",
			"--alias", "${MODEL}",
			"--ctx-size", "262144",
		},
		Env:               map[string]string{"ONEAPI_DEVICE_SELECTOR": "level_zero:${HOST_GPU_IDS}"},
		GPUs:              []SpecGPU{{Index: 3}, {Index: 2}},
		SetVisibleDevices: true,
	}
	cmd := commandFor(t, spec, 54331, GPUVendorNVIDIA, func(string) string { return "" }, false)

	if cmd.Binary != "/opt/llama/llama-server" {
		t.Errorf("Binary = %q, want the spec's binary verbatim", cmd.Binary)
	}
	if cmd.WorkDir != "/srv/models" {
		t.Errorf("WorkDir = %q, want the spec's work_dir verbatim", cmd.WorkDir)
	}
	got := joinArgs(cmd.Args)
	for _, want := range []string{"--port 54331", "--alias qwen-coder-32b", "--ctx-size 262144"} {
		if !strings.Contains(got, want) {
			t.Errorf("args = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "${PORT}") || strings.Contains(got, "${MODEL}") {
		t.Errorf("args = %q still carry an unresolved placeholder -- this reports the template, not the command", got)
	}
	// ${HOST_GPU_IDS} in an env value, and the agent-computed visibility
	// variable beside it: HOST indices, in the operator's declared order.
	if v, ok := envValue(cmd.Env, "ONEAPI_DEVICE_SELECTOR"); !ok || v != "level_zero:3,2" {
		t.Errorf("ONEAPI_DEVICE_SELECTOR = %q (present=%v), want level_zero:3,2", v, ok)
	}
	if v, ok := envValue(cmd.Env, VisibleDevicesVar(GPUVendorNVIDIA)); !ok || v != "3,2" {
		t.Errorf("%s = %q (present=%v), want 3,2", VisibleDevicesVar(GPUVendorNVIDIA), v, ok)
	}
	if cmd.Masked {
		t.Error("Masked = true for a command containing no ${AGENT_ENV:...} value")
	}
}

// TestResolvedCommandEmptyWorkDirReportsBinaryDir is R2's reporting half: the
// log panel's ResolvedCommand.WorkDir must be the directory the child ACTUALLY
// runs in, so an empty spec.WorkDir is reported as the binary's own directory
// (effectiveWorkDir), matching cmd.Dir at exec -- not as an empty string, which
// would tell an operator the child ran in the agent's cwd when it did not.
func TestResolvedCommandEmptyWorkDirReportsBinaryDir(t *testing.T) {
	spec := Spec{Binary: "/opt/llama/llama-server"} // no WorkDir
	cmd := commandFor(t, spec, 8080, GPUVendorNVIDIA, func(string) string { return "" }, false)
	if cmd.WorkDir != "/opt/llama" {
		t.Errorf("WorkDir = %q, want the binary's directory %q (an empty work_dir runs beside the binary)", cmd.WorkDir, "/opt/llama")
	}
}

// TestResolvedCommandMasksASecretInAnArgument is the narrowing this feature
// exists to deliver. ${AGENT_ENV:NAME} resolves in args exactly as it does in
// env, and until now nothing masked args at all -- so a secret in an argument
// travelled upward in the clear.
func TestResolvedCommandMasksASecretInAnArgument(t *testing.T) {
	const secret = "hf_super_secret_value_9f2c"
	spec := Spec{
		Binary: "/opt/vllm/vllm",
		Args:   []string{"--api-key", "${AGENT_ENV:HF_TOKEN}", "--port", "${PORT}"},
		Env:    map[string]string{},
	}
	getenv := func(k string) string {
		if k == "HF_TOKEN" {
			return secret
		}
		return ""
	}
	cmd := commandFor(t, spec, 8081, GPUVendorNone, getenv, false)

	got := joinArgs(cmd.Args)
	if strings.Contains(got, secret) {
		t.Fatalf("args = %q carry the resolved secret -- a secret in an ARGUMENT must be masked too", got)
	}
	if !strings.Contains(got, "--api-key ${AGENT_ENV:HF_TOKEN}") {
		t.Errorf("args = %q, want the masked argument to read as its own placeholder, naming the variable", got)
	}
	// The mask must not cost the useful half of the same command.
	if !strings.Contains(got, "--port 8081") {
		t.Errorf("args = %q, want the resolved port still visible beside the mask", got)
	}
	if !cmd.Masked {
		t.Error("Masked = false although an argument was masked -- a reader cannot then say so")
	}
}

// TestResolvedCommandMasksASecretInAnEnvValue is the same rule on the field
// that already had a blanket mask in the upward report: here the KEY stays
// visible and only the resolved value is replaced, so an operator still sees
// WHICH variable the spec sets.
func TestResolvedCommandMasksASecretInAnEnvValue(t *testing.T) {
	const secret = "hf_super_secret_value_9f2c"
	spec := Spec{
		Binary: "/opt/vllm/vllm",
		Args:   []string{},
		Env:    map[string]string{"HF_TOKEN": "${AGENT_ENV:HF_TOKEN}"},
	}
	getenv := func(k string) string {
		if k == "HF_TOKEN" {
			return secret
		}
		return ""
	}
	cmd := commandFor(t, spec, 8081, GPUVendorNone, getenv, false)

	v, ok := envValue(cmd.Env, "HF_TOKEN")
	if !ok {
		t.Fatalf("env = %v, want the HF_TOKEN key to survive masking", cmd.Env)
	}
	if strings.Contains(v, secret) {
		t.Fatalf("HF_TOKEN = %q carries the resolved secret", v)
	}
	if v != "${AGENT_ENV:HF_TOKEN}" {
		t.Errorf("HF_TOKEN = %q, want the placeholder that produced it", v)
	}
	if !cmd.Masked {
		t.Error("Masked = false although an env value was masked")
	}
}

// TestResolvedCommandMasksASecretEmbeddedInALongerValue: a secret is rarely the
// whole string. Only the substituted SPAN may be replaced -- the surrounding
// text the operator wrote is not a secret and is exactly the context that makes
// the line readable.
func TestResolvedCommandMasksASecretEmbeddedInALongerValue(t *testing.T) {
	spec := Spec{
		Binary: "/opt/vllm/vllm",
		Args:   []string{"--auth", "Bearer ${AGENT_ENV:TOK_A}:${AGENT_ENV:TOK_B}/v1"},
		Env:    map[string]string{},
	}
	getenv := func(k string) string {
		switch k {
		case "TOK_A":
			return "aaaa-secret"
		case "TOK_B":
			return "bbbb-secret"
		}
		return ""
	}
	cmd := commandFor(t, spec, 1, GPUVendorNone, getenv, false)

	want := "Bearer ${AGENT_ENV:TOK_A}:${AGENT_ENV:TOK_B}/v1"
	if cmd.Args[1] != want {
		t.Fatalf("arg = %q, want %q -- two spans in one string, with the operator's own text intact between and around them", cmd.Args[1], want)
	}
}

// TestResolvedCommandDoesNotMaskWhatTheAgentComputedOrTheOperatorWrote is the
// half that makes the panel worth having, asserted as its own case so a future
// "let's just mask everything, to be safe" cannot pass the suite.
func TestResolvedCommandDoesNotMaskWhatTheAgentComputedOrTheOperatorWrote(t *testing.T) {
	spec := Spec{
		Binary:            "/opt/llama/llama-server",
		UpstreamModel:     "qwen-coder-32b",
		Args:              []string{"--port", "${PORT}", "--ctx-size", "262144", "--model", "/srv/models/qwen.gguf"},
		Env:               map[string]string{"GGML_CUDA_FORCE_MMQ": "1"},
		GPUs:              []SpecGPU{{Index: 2}, {Index: 3}},
		SetVisibleDevices: true,
	}
	getenv := func(k string) string {
		if k == "PATH" {
			return "/usr/bin:/bin"
		}
		return ""
	}
	// Gateway-sourced specs: the gateway already holds every literal in this
	// document, so masking it would hide nothing from anyone.
	cmd := commandFor(t, spec, 54331, GPUVendorNVIDIA, getenv, false)

	for _, want := range []string{"--port 54331", "--ctx-size 262144", "--model /srv/models/qwen.gguf"} {
		if got := joinArgs(cmd.Args); !strings.Contains(got, want) {
			t.Errorf("args = %q, want %q visible: this is what the operator opened the panel for", got, want)
		}
	}
	if v, _ := envValue(cmd.Env, "CUDA_VISIBLE_DEVICES"); v != "2,3" {
		t.Errorf("CUDA_VISIBLE_DEVICES = %q, want 2,3 -- 'the visibility variable was actually set, to these cards' is among the most useful things this can say", v)
	}
	if v, _ := envValue(cmd.Env, "GGML_CUDA_FORCE_MMQ"); v != "1" {
		t.Errorf("GGML_CUDA_FORCE_MMQ = %q, want 1 (a literal the gateway already holds)", v)
	}
	if v, _ := envValue(cmd.Env, "PATH"); v != "/usr/bin:/bin" {
		t.Errorf("PATH = %q, want the agent-provided base value: the agent computed it, so it is not a secret", v)
	}
	if cmd.Masked {
		t.Error("Masked = true although nothing came from ${AGENT_ENV:...}")
	}
}

// TestResolvedCommandFileModeMasksSpecEnvValues pins the ONE source-dependent
// case, and its reason: in file mode a spec's env VALUES are the single thing
// the agent deliberately withholds from the gateway (report.go's
// redactConfigEnv), because a local runtime.json is the operator's own document
// and may hold a plaintext secret. A panel that showed them would quietly undo
// that guarantee, so the panel inherits the report's line rather than drawing a
// new one.
//
// Everything the report already sends verbatim -- binary, args, work_dir, env
// KEYS -- stays visible, and so do the values the agent itself computed.
func TestResolvedCommandFileModeMasksSpecEnvValues(t *testing.T) {
	spec := Spec{
		Binary:            "/opt/llama/llama-server",
		Args:              []string{"--port", "${PORT}", "--ctx-size", "262144"},
		Env:               map[string]string{"HF_TOKEN": "hf_written_in_plaintext_locally", "GGML_CUDA_FORCE_MMQ": "1"},
		GPUs:              []SpecGPU{{Index: 5}},
		SetVisibleDevices: true,
	}
	getenv := func(k string) string {
		if k == "PATH" {
			return "/usr/bin:/bin"
		}
		return ""
	}
	cmd := commandFor(t, spec, 54331, GPUVendorNVIDIA, getenv, true)

	if v, ok := envValue(cmd.Env, "HF_TOKEN"); !ok || v != envRedactedMask {
		t.Errorf("HF_TOKEN = %q (present=%v), want the report's own mask %q in file mode", v, ok, envRedactedMask)
	}
	if v, _ := envValue(cmd.Env, "GGML_CUDA_FORCE_MMQ"); v != envRedactedMask {
		t.Errorf("GGML_CUDA_FORCE_MMQ = %q, want the mask: in file mode the rule is the field, not a guess about which value looks secret", v)
	}
	// EnvRedacted, and NOT Masked: the two flags are two different reasons for
	// withholding, and the portal renders a different sentence for each. Raising
	// Masked here would tell a file-mode operator to go and check an
	// ${AGENT_ENV:NAME} variable that nothing in this spec ever named.
	if !cmd.EnvRedacted {
		t.Error("EnvRedacted = false although file mode withheld the spec's env values")
	}
	if cmd.Masked {
		t.Error("Masked = true although nothing in this spec came from ${AGENT_ENV:...}: that flag is the placeholder rule, and this mask is not a placeholder")
	}
	// The keys, and everything the report already carries, stay visible.
	if _, ok := envValue(cmd.Env, "HF_TOKEN"); !ok {
		t.Error("the HF_TOKEN key did not survive: an operator must still see WHICH variables the spec sets")
	}
	if got := joinArgs(cmd.Args); !strings.Contains(got, "--port 54331") || !strings.Contains(got, "--ctx-size 262144") {
		t.Errorf("args = %q, want them visible in file mode too: the report already sends args verbatim, so masking them here would protect nothing", got)
	}
	// Agent-computed values are the agent's own, not the operator's document.
	if v, _ := envValue(cmd.Env, "PATH"); v != "/usr/bin:/bin" {
		t.Errorf("PATH = %q, want the agent's own base value visible in file mode", v)
	}
	if v, _ := envValue(cmd.Env, "CUDA_VISIBLE_DEVICES"); v != "5" {
		t.Errorf("CUDA_VISIBLE_DEVICES = %q, want 5: the agent computed it from local hardware, and in file mode it is the value the operator CANNOT read off their own file", v)
	}
}

// TestResolvedCommandFileModeStillMasksASecretInAnArgument: the file-mode env
// rule is ADDITIONAL to the placeholder rule, never a replacement for it. A
// resolved ${AGENT_ENV:...} value is masked in both modes and in both fields.
func TestResolvedCommandFileModeStillMasksASecretInAnArgument(t *testing.T) {
	spec := Spec{
		Binary: "/opt/vllm/vllm",
		Args:   []string{"--api-key", "${AGENT_ENV:HF_TOKEN}"},
		Env:    map[string]string{},
	}
	getenv := func(k string) string {
		if k == "HF_TOKEN" {
			return "hf_secret"
		}
		return ""
	}
	for _, fromFile := range []bool{false, true} {
		cmd := commandFor(t, spec, 1, GPUVendorNone, getenv, fromFile)
		if got := joinArgs(cmd.Args); strings.Contains(got, "hf_secret") {
			t.Fatalf("fromFile=%v: args = %q carry the resolved secret", fromFile, got)
		}
		if !cmd.Masked {
			t.Errorf("fromFile=%v: Masked = false although an argument came from ${AGENT_ENV:HF_TOKEN}", fromFile)
		}
	}
}

// TestResolvedCommandReportsBothWithholdingReasonsAtOnce: Masked and
// EnvRedacted are independent, and a file-mode spec that ALSO resolves a
// placeholder into an argument raises both. One flag could not say that, and
// the portal stacks one sentence per reason -- the operator has a host variable
// to go and check AND a local document whose env values are being withheld.
func TestResolvedCommandReportsBothWithholdingReasonsAtOnce(t *testing.T) {
	spec := Spec{
		Binary: "/opt/vllm/vllm",
		Args:   []string{"--api-key", "${AGENT_ENV:HF_TOKEN}"},
		Env:    map[string]string{"HF_HOME": "/srv/cache"},
	}
	getenv := func(k string) string {
		if k == "HF_TOKEN" {
			return "hf_secret"
		}
		return ""
	}
	cmd := commandFor(t, spec, 1, GPUVendorNone, getenv, true)

	if !cmd.Masked {
		t.Error("Masked = false although the argument came from ${AGENT_ENV:HF_TOKEN}")
	}
	if !cmd.EnvRedacted {
		t.Error("EnvRedacted = false although file mode withheld the spec's own env value")
	}
	if v, ok := envValue(cmd.Env, "HF_HOME"); !ok || v != envRedactedMask {
		t.Errorf("HF_HOME = %q (present=%v), want the report's own mask: the file-mode env rule is ADDITIONAL to the placeholder rule", v, ok)
	}
}

// TestResolvedCommandTruncatesBeyondItsBudget: the retention bound LogStore
// advertises has to stay true even for a pathological document, and a shortened
// list must SAY it is shortened -- the same rule as a gap in the output.
func TestResolvedCommandTruncatesBeyondItsBudget(t *testing.T) {
	huge := strings.Repeat("x", maxResolvedCommandBytes/2)
	spec := Spec{
		Binary: "/opt/vllm/vllm",
		Args:   []string{huge, huge, huge, "--port", "${PORT}"},
		Env:    map[string]string{},
	}
	cmd := commandFor(t, spec, 8080, GPUVendorNone, func(string) string { return "" }, false)

	if !cmd.Truncated {
		t.Fatal("Truncated = false although the command exceeded maxResolvedCommandBytes")
	}
	total := 0
	for _, a := range cmd.Args {
		total += len(a)
	}
	for _, e := range cmd.Env {
		total += len(e)
	}
	if total > maxResolvedCommandBytes {
		t.Fatalf("retained command is %d bytes, want at most %d", total, maxResolvedCommandBytes)
	}
	// Whole entries are dropped, never shortened: half an argument is a value
	// that looks real and is not.
	for _, a := range cmd.Args {
		if len(a) != len(huge) && a != "--port" && a != "8080" {
			t.Fatalf("arg %q is neither a whole retained entry nor dropped -- entries must never be shortened", a)
		}
	}
}

// TestMaskSecretSpansIgnoresNonsensicalSpans: a span outside the string, or one
// that runs backwards, can only come from a bug in this package -- never from a
// spec. It must be skipped, because this code runs on the launch path and must
// not be the thing that panics there.
func TestMaskSecretSpansIgnoresNonsensicalSpans(t *testing.T) {
	cases := []struct {
		name  string
		spans []secretSpan
	}{
		{"past the end", []secretSpan{{start: 2, end: 99, name: "X"}}},
		{"backwards", []secretSpan{{start: 5, end: 2, name: "X"}}},
		{"overlapping the previous one", []secretSpan{{start: 0, end: 4, name: "A"}, {start: 2, end: 5, name: "B"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Must not panic, and must not produce a shorter string than the
			// masking it did apply implies.
			got, _ := maskSecretSpans("abcdefgh", c.spans)
			if got == "" {
				t.Fatalf("maskSecretSpans returned an empty string for %+v", c.spans)
			}
		})
	}
}

// TestExpandSpecKeepsTheChildsRealValues guards the refactor that made
// provenance observable: expandSpec's args/env are still exactly what the child
// receives -- the plaintext secret, not the mask. Masking is a property of the
// REPORT, not of the launch, and confusing the two would launch a model server
// with the literal text "${AGENT_ENV:HF_TOKEN}" as its API key.
func TestExpandSpecKeepsTheChildsRealValues(t *testing.T) {
	const secret = "hf_real_value"
	spec := Spec{
		Binary: "/opt/vllm/vllm",
		Args:   []string{"--api-key", "${AGENT_ENV:HF_TOKEN}"},
		Env:    map[string]string{"HF_TOKEN": "${AGENT_ENV:HF_TOKEN}"},
	}
	getenv := func(k string) string {
		if k == "HF_TOKEN" {
			return secret
		}
		return ""
	}
	args, env, err := ExpandPlaceholders(spec, 8080, GPUVendorNone, getenv)
	if err != nil {
		t.Fatalf("ExpandPlaceholders: %v", err)
	}
	if args[1] != secret {
		t.Errorf("args[1] = %q, want the REAL secret %q -- the child must receive the value, not the mask", args[1], secret)
	}
	if v, _ := envValue(env, "HF_TOKEN"); v != secret {
		t.Errorf("env HF_TOKEN = %q, want the REAL secret", v)
	}
}
