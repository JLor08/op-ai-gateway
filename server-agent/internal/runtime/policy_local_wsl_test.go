package runtime

import (
	"strings"
	"testing"
)

// wslSpec is a WSL spec that passes every check of wslPolicy, so each test
// breaks exactly one thing.
func wslSpec(binary, workDir string) Spec {
	return Spec{
		Binary:    binary,
		WorkDir:   workDir,
		Execution: &Execution{Mode: ExecutionModeWSL, WSL: &WSLExecution{Distro: "Ubuntu-24.04"}},
	}
}

func wslPolicy() LocalPolicy {
	return LocalPolicy{WSL: WSLPolicy{
		Exe:             `C:\Program Files\WSL\wsl.exe`,
		AllowedDistros:  []string{"Ubuntu-24.04"},
		AllowedBinaries: []string{"/home/ai/vllm/.venv/bin/vllm"},
		User:            "ai",
	}}
}

const wslBin = "/home/ai/vllm/.venv/bin/vllm"

func mustRefuse(t *testing.T, err error, substrs ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a refusal, got nil")
	}
	for _, s := range substrs {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not contain %q", err, s)
		}
	}
}

func TestPermitWSLAllowsFullyConfigured(t *testing.T) {
	if err := wslPolicy().Permit(wslSpec(wslBin, "")); err != nil {
		t.Fatalf("Permit: %v", err)
	}
}

func TestPermitWSLUnconfigured(t *testing.T) {
	p := wslPolicy()
	p.WSL.Exe = ""
	mustRefuse(t, p.Permit(wslSpec(wslBin, "")), "runtime_wsl_exe")
}

func TestPermitWSLRefusesRootUser(t *testing.T) {
	for _, u := range []string{"", "root"} {
		p := wslPolicy()
		p.WSL.User = u
		mustRefuse(t, p.Permit(wslSpec(wslBin, "")), "runtime_wsl_user")
	}
}

func TestPermitWSLDistroAllowlist(t *testing.T) {
	p := wslPolicy()
	p.WSL.AllowedDistros = nil
	mustRefuse(t, p.Permit(wslSpec(wslBin, "")), "runtime_wsl_allowed_distros")

	p.WSL.AllowedDistros = []string{"Debian", "Ubuntu"}
	err := p.Permit(wslSpec(wslBin, ""))
	mustRefuse(t, err, "runtime_wsl_allowed_distros", "2")
	// The refusal names the setting and the count, never a distro name.
	for _, name := range []string{"Debian", "Ubuntu"} {
		if strings.Contains(err.Error(), name) {
			t.Errorf("error leaks distro name %q: %v", name, err)
		}
	}

	p.WSL.AllowedDistros = []string{"ubuntu-24.04"} // exact match only
	mustRefuse(t, p.Permit(wslSpec(wslBin, "")), "runtime_wsl_allowed_distros")

	p.WSL.AllowedDistros = []string{"Debian", "Ubuntu-24.04"}
	if err := p.Permit(wslSpec(wslBin, "")); err != nil {
		t.Errorf("exact entry must permit: %v", err)
	}
}

func TestPermitWSLBinaryAllowlist(t *testing.T) {
	p := wslPolicy()
	p.WSL.AllowedBinaries = nil
	mustRefuse(t, p.Permit(wslSpec(wslBin, "")), "runtime_wsl_allowed_binaries")

	p = wslPolicy()
	p.WSL.AllowedBinaries = []string{"vllm", `C:\x\vllm.exe`, wslBin}
	for _, b := range []string{"vllm", `C:\x\vllm.exe`, ""} {
		err := p.Permit(wslSpec(b, ""))
		mustRefuse(t, err, "runtime_wsl_allowed_binaries")
		if b != "" && strings.Contains(err.Error(), b) {
			t.Errorf("error leaks the binary path %q: %v", b, err)
		}
	}
	mustRefuse(t, p.Permit(wslSpec("/home/ai/vllm/.venv/bin/other", "")), "runtime_wsl_allowed_binaries")
	if err := p.Permit(wslSpec(wslBin, "")); err != nil {
		t.Errorf("exact entry must permit: %v", err)
	}

	// The native allowlist is never consulted for a WSL spec.
	p = wslPolicy()
	p.AllowedBinaries = []string{"/native/vllm"}
	mustRefuse(t, p.Permit(wslSpec("/native/vllm", "")), "runtime_wsl_allowed_binaries")
	p.AllowedBinaries = nil
	if err := p.Permit(wslSpec(wslBin, "")); err != nil {
		t.Errorf("empty native list must not block a WSL spec: %v", err)
	}
}

func TestPermitWSLWorkDir(t *testing.T) {
	p := wslPolicy()
	if err := p.Permit(wslSpec(wslBin, "")); err != nil {
		t.Errorf("empty work_dir: %v", err)
	}
	mustRefuse(t, p.Permit(wslSpec(wslBin, "relative")), "work_dir")

	p.WSL.AllowedDirs = []string{"/home/ai/*"}
	if err := p.Permit(wslSpec(wslBin, "/home/ai/vllm")); err != nil {
		t.Errorf("/home/ai/vllm: %v", err)
	}
	if err := p.Permit(wslSpec(wslBin, "/home/ai")); err != nil {
		t.Errorf("/home/ai itself: %v", err)
	}
	mustRefuse(t, p.Permit(wslSpec(wslBin, "/home/aix")), "runtime_wsl_allowed_dirs")
	err := p.Permit(wslSpec(wslBin, "/home/ai/../etc"))
	mustRefuse(t, err, "runtime_wsl_allowed_dirs")
	if strings.Contains(err.Error(), "/etc") {
		t.Errorf("error leaks the path: %v", err)
	}
	// An empty work_dir stays permitted even with a narrow allowlist.
	if err := p.Permit(wslSpec(wslBin, "")); err != nil {
		t.Errorf("empty work_dir under AllowedDirs: %v", err)
	}
}

func TestWithinPosixDir(t *testing.T) {
	for _, tc := range []struct {
		cand, dir string
		want      bool
	}{
		{"/home/ai", "/home/ai", true},
		{"/home/ai/x/y", "/home/ai", true},
		{"/home/aix", "/home/ai", false},
		{"/home/ai/../etc", "/home/ai", false},
		{"/home/ai/./x", "/home/ai/", true},
		{"", "/home/ai", false},
		{"/home/ai", "", false},
		{"/anything", "/", true},
	} {
		if got := withinPosixDir(tc.cand, tc.dir); got != tc.want {
			t.Errorf("withinPosixDir(%q, %q) = %v, want %v", tc.cand, tc.dir, got, tc.want)
		}
	}
}

func TestEffectiveWSLWorkDir(t *testing.T) {
	if got := effectiveWSLWorkDir(wslSpec(wslBin, "")); got != "/home/ai/vllm/.venv/bin" {
		t.Errorf("empty work_dir -> %q", got)
	}
	if got := effectiveWSLWorkDir(wslSpec(wslBin, "/srv/x")); got != "/srv/x" {
		t.Errorf("explicit work_dir -> %q", got)
	}
}

func TestCheckSpecEnvRefusesWSLOwnedNames(t *testing.T) {
	for _, k := range []string{"WSLENV", "wsl_utf8", "Xdg_Runtime_Dir"} {
		if err := checkSpecEnv(wslSpec(wslBin, ""), []string{k}, ""); err == nil {
			t.Errorf("WSL spec env %q must be refused", k)
		}
	}
	native := Spec{Binary: "/bin/x"}
	if err := checkSpecEnv(native, []string{"XDG_RUNTIME_DIR"}, ""); err != nil {
		t.Errorf("native spec XDG_RUNTIME_DIR must be accepted: %v", err)
	}
	if err := checkSpecEnv(wslSpec(wslBin, ""), []string{"HF_HOME"}, ""); err != nil {
		t.Errorf("unrelated env must be accepted: %v", err)
	}
}
