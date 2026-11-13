package runtime

import (
	"slices"
	"strings"
	"testing"
)

func testWSLLaunch(env []string) wslLaunch {
	return buildWSLLaunch(wslPolicy().WSL, wslLaunchInput{
		UID: 1000, Distro: "Ubuntu-24.04", Unit: "opagent-ab12cd34-m-7", WorkDir: "/home/ai/vllm",
		Binary: wslBin, Args: []string{"serve", "m", "--port", "41234"}, Env: env,
	})
}

func TestBuildWSLLaunchArgv(t *testing.T) {
	l := testWSLLaunch(nil)
	want := []string{
		"-d", "Ubuntu-24.04", "-u", "ai", "--cd", "/home/ai/vllm", "--exec",
		"systemd-run", "--user", "--scope", "--quiet", "--unit=opagent-ab12cd34-m-7", "--",
		"/home/ai/vllm/.venv/bin/vllm", "serve", "m", "--port", "41234",
	}
	if !slices.Equal(l.Args, want) {
		t.Fatalf("args = %q\nwant   %q", l.Args, want)
	}
	if l.PrefixLen != 14 {
		t.Errorf("PrefixLen = %d, want 14", l.PrefixLen)
	}
	if l.Exe != wslPolicy().WSL.Exe || l.Unit != "opagent-ab12cd34-m-7" {
		t.Errorf("Exe/Unit = %q/%q", l.Exe, l.Unit)
	}
}

func TestBuildWSLLaunchEnv(t *testing.T) {
	in := []string{`PATH=C:\x`, `SYSTEMROOT=C:\Windows`, "HF_TOKEN=x", "CUDA_VISIBLE_DEVICES=1"}
	l := testWSLLaunch(in)
	for _, w := range []string{"CUDA_DEVICE_ORDER=PCI_BUS_ID", "PYTHONUNBUFFERED=1", "XDG_RUNTIME_DIR=/run/user/1000", "WSL_UTF8=1"} {
		if !slices.Contains(l.Env, w) {
			t.Errorf("env lacks %q: %v", w, l.Env)
		}
	}
	var wslenv []string
	for _, e := range l.Env {
		if strings.HasPrefix(e, "WSLENV=") {
			wslenv = append(wslenv, e)
		}
	}
	want := "WSLENV=CUDA_DEVICE_ORDER/u:CUDA_VISIBLE_DEVICES/u:HF_TOKEN/u:PYTHONUNBUFFERED/u:XDG_RUNTIME_DIR/u"
	if len(wslenv) != 1 || wslenv[0] != want {
		t.Errorf("WSLENV entries = %q, want exactly %q", wslenv, want)
	}
	if !slices.Equal(l.Env[:len(in)], in) {
		t.Errorf("input env must stay a prefix: %v", l.Env)
	}
}

func TestBuildWSLLaunchSpecPythonUnbufferedWins(t *testing.T) {
	l := testWSLLaunch([]string{"PYTHONUNBUFFERED=0"})
	n := 0
	for _, e := range l.Env {
		if strings.HasPrefix(e, "PYTHONUNBUFFERED=") {
			n++
			if e != "PYTHONUNBUFFERED=0" {
				t.Errorf("got %q", e)
			}
		}
	}
	if n != 1 {
		t.Errorf("PYTHONUNBUFFERED appears %d times", n)
	}
}

func TestWSLUnitName(t *testing.T) {
	if got := wslUnitName("ab12cd34", "m#2", 7); got != "opagent-ab12cd34-m_2-7" {
		t.Errorf("got %q", got)
	}
	if got := wslUnitName("ab12cd34", "a b/c.d", 0); got != "opagent-ab12cd34-a_b_c_d-0" {
		t.Errorf("got %q", got)
	}
	got := wslUnitName("n", strings.Repeat("k", 100), 1)
	if got != "opagent-n-"+strings.Repeat("k", 64)+"-1" {
		t.Errorf("key not truncated to 64: %q", got)
	}
}

// TestParseWSLUnitNameRoundTrips: every name wslUnitName builds from a nonce
// newWSLNonce draws parses back to its parts, so the orphan sweep's shape check
// cannot drift from the builder.
func TestParseWSLUnitNameRoundTrips(t *testing.T) {
	for _, nonce := range []string{newWSLNonce(), "00000000", "ab12cd34"} {
		if !isWSLNonce(nonce) {
			t.Fatalf("nonce %q is not the shape newWSLNonce draws", nonce)
		}
		for _, tc := range []struct {
			key  string
			gen  uint64
			want string // the key as wslUnitName writes it
		}{
			{"S", 1, "S"},
			{"m#2", 7, "m_2"},
			{"a b/c.d", 0, "a_b_c_d"},
			{"my-model-3", 12, "my-model-3"},
			{strings.Repeat("k", 100), 18446744073709551615, strings.Repeat("k", 64)},
		} {
			name := wslUnitName(nonce, tc.key, tc.gen)
			gotNonce, gotKey, gotGen, ok := parseWSLUnitName(name)
			if !ok || gotNonce != nonce || gotKey != tc.want || gotGen != tc.gen {
				t.Errorf("parseWSLUnitName(%q) = %q, %q, %d, %v; want %q, %q, %d, true", name, gotNonce, gotKey, gotGen, ok, nonce, tc.want, tc.gen)
			}
		}
	}
}

func TestWindowsPathToWSL(t *testing.T) {
	for in, want := range map[string]string{
		`C:\a\b`: "/mnt/c/a/b", "d:/x": "/mnt/d/x", "/already/posix": "/already/posix",
		`C:`: "/mnt/c", `C:\`: "/mnt/c", `D:\x\`: "/mnt/d/x", `C:\a\..\b`: "/mnt/c/b", `C:\a\..`: "/mnt/c",
	} {
		got, err := windowsPathToWSL(in)
		if err != nil || got != want {
			t.Errorf("windowsPathToWSL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		`\\server\share\x`, `relative\x`, "",
		// A drive-relative path names the drive's current directory, which
		// the agent cannot know.
		`c:foo`,
		// Forward-slash UNC: POSIX would read it as /srv/x, a different file.
		"//srv/x", `/\srv\x`,
		// A .. run that climbs out of the drive's mount must not land on
		// /mnt/<other> or the distro's own tree.
		`C:\a\..\..\x`, `C:\a\..\..\..`, `C:\..`, "c:/../etc",
	} {
		if got, err := windowsPathToWSL(in); err == nil {
			t.Errorf("windowsPathToWSL(%q) = %q, want an error", in, got)
		}
	}
}

func TestResolvedCommandMasksSecretUnderWSLWrapper(t *testing.T) {
	spec := wslSpec(wslBin, "/home/ai/vllm")
	spec.Args = []string{"serve", "--api-key", "${API_TOKEN}"}
	spec.APIToken = "tok-secret"
	ex, err := expandSpec(spec, 41234, GPUVendorNone, nil, func(string) string { return "" }, "")
	if err != nil {
		t.Fatal(err)
	}
	l := buildWSLLaunch(wslPolicy().WSL, wslLaunchInput{
		UID: 1000, Distro: "Ubuntu-24.04", Unit: "opagent-n-m-1", WorkDir: "/home/ai/vllm",
		Binary: spec.Binary, Args: ex.args, Env: ex.env,
	})
	w := ex.withWrapper(l)
	if !slices.Equal(w.args, l.Args) {
		t.Fatalf("wrapped args = %q, want the full launch argv %q", w.args, l.Args)
	}
	cmd := w.resolvedWSLCommand(spec, l.Exe, false)
	if cmd.Binary != l.Exe || cmd.WorkDir != "/home/ai/vllm" {
		t.Errorf("Binary/WorkDir = %q/%q", cmd.Binary, cmd.WorkDir)
	}
	if !slices.Equal(cmd.Args[:l.PrefixLen], l.Args[:l.PrefixLen]) {
		t.Errorf("wrapper args not verbatim: %q", cmd.Args)
	}
	if strings.Contains(strings.Join(cmd.Args, " "), "tok-secret") || !cmd.Masked {
		t.Errorf("secret leaked or not flagged: %q masked=%v", cmd.Args, cmd.Masked)
	}
	if cmd.Args[l.PrefixLen+2] != "${API_TOKEN}" {
		t.Errorf("mask at shifted index: %q", cmd.Args)
	}
	if len(ex.env) > len(cmd.Env) {
		t.Errorf("reported env lost entries: %v", cmd.Env)
	}
}

func TestConfigDirPlaceholderIsLinuxPathForWSL(t *testing.T) {
	spec := wslSpec(wslBin, "")
	spec.ID = "spec-1"
	spec.Args = []string{"${CONFIG_DIR}/m.ini"}
	args, _, err := ExpandPlaceholders(spec, 1, GPUVendorNone, nil, func(string) string { return "" }, `C:\agent\cfg`)
	if err != nil || args[0] != "/mnt/c/agent/cfg/spec-1/m.ini" {
		t.Errorf("args = %q, err = %v", args, err)
	}
	// The refusal travels upward (a not_permitted message, a last_error), so
	// it names the setting, never the agent-local directory itself.
	for _, dir := range []string{`\\fileserver\cfg`, `C:\agent\..\..\cfg`} {
		_, _, err := ExpandPlaceholders(spec, 1, GPUVendorNone, nil, func(string) string { return "" }, dir)
		if err == nil {
			t.Errorf("config dir %q translated, want a refusal", dir)
			continue
		}
		if msg := err.Error(); !strings.Contains(msg, "config_files_dir") || strings.Contains(msg, "fileserver") || strings.Contains(msg, `\`) {
			t.Errorf("refusal for config dir %q = %q, want it to name config_files_dir and not the path", dir, msg)
		}
	}
	native := Spec{ID: "spec-1", Binary: "/b", Args: []string{"${CONFIG_DIR}"}}
	args, _, err = ExpandPlaceholders(native, 1, GPUVendorNone, nil, func(string) string { return "" }, "/agent/cfg")
	if err != nil || args[0] != "/agent/cfg/spec-1" {
		t.Errorf("native args = %q, err = %v", args, err)
	}
}
