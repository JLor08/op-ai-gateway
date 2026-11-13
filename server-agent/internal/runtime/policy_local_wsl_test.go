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

const wslBin = "/home/ai/vllm/.venv/bin/vllm"

// wslPolicy allowlists one native and one WSL binary; the WSL entry names
// Ubuntu-24.04 and wslBin.
func wslPolicy() LocalPolicy {
	return LocalPolicy{
		AllowedBinaries: []string{"/usr/bin/native", "wsl://Ubuntu-24.04" + wslBin},
		WSL:             WSLPolicy{Exe: `C:\Program Files\WSL\wsl.exe`, User: "ai"},
	}
}

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
	for _, u := range []string{"", "root", "0", "00"} {
		p := wslPolicy()
		p.WSL.User = u
		mustRefuse(t, p.Permit(wslSpec(wslBin, "")), "runtime_wsl_user")
	}
}

func TestPermitWSLDistroMustBeNamedByAnEntry(t *testing.T) {
	// No distro named: empty distro, and a native-only allowlist.
	p := wslPolicy()
	s := wslSpec(wslBin, "")
	s.Execution.WSL.Distro = ""
	mustRefuse(t, p.Permit(s), "runtime_allowed_binaries")

	p.AllowedBinaries = []string{"/usr/bin/native"}
	mustRefuse(t, p.Permit(wslSpec(wslBin, "")), "runtime_allowed_binaries", "wsl://")

	// Another distro's entry never permits this distro, even for the same path.
	p.AllowedBinaries = []string{"wsl://Debian" + wslBin}
	err := p.Permit(wslSpec(wslBin, ""))
	mustRefuse(t, err, "runtime_allowed_binaries")
	for _, name := range []string{"Debian", "Ubuntu-24.04"} {
		if strings.Contains(err.Error(), name) {
			t.Errorf("error leaks distro name %q: %v", name, err)
		}
	}

	// Exact, case-sensitive distro match.
	p.AllowedBinaries = []string{"wsl://ubuntu-24.04" + wslBin}
	mustRefuse(t, p.Permit(wslSpec(wslBin, "")), "runtime_allowed_binaries")

	p.AllowedBinaries = []string{"wsl://Debian" + wslBin, "wsl://Ubuntu-24.04" + wslBin}
	if err := p.Permit(wslSpec(wslBin, "")); err != nil {
		t.Errorf("exact entry must permit: %v", err)
	}
}

func TestPermitWSLMalformedEntryPermitsNothing(t *testing.T) {
	for _, e := range []string{"WSL://Ubuntu-24.04" + wslBin, "wsl://Ubuntu-24.04", `wsl://Ubuntu-24.04\home\x`, "wsl://-bad" + wslBin} {
		p := wslPolicy()
		p.AllowedBinaries = []string{e}
		mustRefuse(t, p.Permit(wslSpec(wslBin, "")), "runtime_allowed_binaries")
	}
}

func TestPermitWSLBinaryAllowlist(t *testing.T) {
	p := wslPolicy()
	p.AllowedBinaries = []string{"wsl://Ubuntu-24.04/vllm", `wsl://Ubuntu-24.04/x\y`, "wsl://Ubuntu-24.04" + wslBin}
	for _, b := range []string{"vllm", `C:\x\vllm.exe`, ""} {
		err := p.Permit(wslSpec(b, ""))
		mustRefuse(t, err, "runtime_allowed_binaries")
		if b != "" && strings.Contains(err.Error(), b) {
			t.Errorf("error leaks the binary path %q: %v", b, err)
		}
	}
	mustRefuse(t, p.Permit(wslSpec("/home/ai/vllm/.venv/bin/other", "")), "runtime_allowed_binaries")
	if err := p.Permit(wslSpec(wslBin, "")); err != nil {
		t.Errorf("exact entry must permit: %v", err)
	}
	// A native entry with the same path never permits a WSL spec.
	p.AllowedBinaries = []string{wslBin}
	mustRefuse(t, p.Permit(wslSpec(wslBin, "")), "runtime_allowed_binaries")
}

func TestPermitWSLWorkDir(t *testing.T) {
	p := wslPolicy()
	if err := p.Permit(wslSpec(wslBin, "/anywhere")); err != nil {
		t.Errorf("no dir entries: any absolute work_dir: %v", err)
	}
	if err := p.Permit(wslSpec(wslBin, "")); err != nil {
		t.Errorf("empty work_dir: %v", err)
	}
	mustRefuse(t, p.Permit(wslSpec(wslBin, "relative")), "work_dir")

	p.AllowedDirs = []string{"wsl://Ubuntu-24.04/srv/models/*"}
	if err := p.Permit(wslSpec(wslBin, "/srv/models/a")); err != nil {
		t.Errorf("/srv/models/a: %v", err)
	}
	if err := p.Permit(wslSpec(wslBin, "/srv/models")); err != nil {
		t.Errorf("/srv/models itself: %v", err)
	}
	mustRefuse(t, p.Permit(wslSpec(wslBin, "/srv/modelsx")), "runtime_allowed_dirs")
	err := p.Permit(wslSpec(wslBin, "/srv/models/../etc"))
	mustRefuse(t, err, "runtime_allowed_dirs")
	if strings.Contains(err.Error(), "/etc") {
		t.Errorf("error leaks the path: %v", err)
	}
	mustRefuse(t, p.Permit(wslSpec(wslBin, "/srv/other")), "runtime_allowed_dirs")
	// An empty work_dir stays permitted even with a narrow allowlist.
	if err := p.Permit(wslSpec(wslBin, "")); err != nil {
		t.Errorf("empty work_dir under dir entries: %v", err)
	}

	// Another distro's dir entry does not restrict this distro; a native dir
	// entry neither.
	p.AllowedDirs = []string{"wsl://Debian/srv", "/native/only"}
	if err := p.Permit(wslSpec(wslBin, "/tmp/x")); err != nil {
		t.Errorf("a Debian dir entry must not restrict Ubuntu-24.04: %v", err)
	}
}

func TestPermitWSLAllowBinaryDirs(t *testing.T) {
	p := wslPolicy()
	p.AllowBinaryDirs = true
	if err := p.Permit(wslSpec(wslBin, "/home/ai/vllm/.venv/bin/sub")); err != nil {
		t.Errorf("binary dir subtree: %v", err)
	}
	mustRefuse(t, p.Permit(wslSpec(wslBin, "/tmp")), "runtime_allowed_dirs")
	// The native binary's dir does not count for a WSL spec.
	mustRefuse(t, p.Permit(wslSpec(wslBin, "/usr/bin")), "runtime_allowed_dirs")
}

func TestPermitNativeIgnoresWSLEntries(t *testing.T) {
	native := Spec{Binary: "/usr/bin/native"}
	// Only wsl:// entries: the native allowlist is empty.
	p := LocalPolicy{AllowedBinaries: []string{"wsl://Ubuntu-24.04" + wslBin, "wsl://Ubuntu-24.04/x\\y"}}
	mustRefuse(t, p.Permit(native), "allowlist is empty")

	// A wsl:// dir entry alone configures no native containment.
	p = LocalPolicy{AllowedBinaries: []string{"/usr/bin/native"}, AllowedDirs: []string{"wsl://Ubuntu-24.04/srv"}}
	n := native
	n.WorkDir = "/anything"
	if err := p.Permit(n); err != nil {
		t.Errorf("wsl:// dir entry must not restrict native: %v", err)
	}

	// A native work_dir spelled wsl://... never passes through a wsl:// dir entry.
	p.AllowedDirs = []string{"/srv", "wsl://Ubuntu-24.04/srv"}
	n.WorkDir = "wsl://Ubuntu-24.04/srv/x"
	mustRefuse(t, p.Permit(n), "work_dir")
}

func TestPermitWSLDistroAllowed(t *testing.T) {
	p := wslPolicy()
	if !p.wslDistroAllowed("Ubuntu-24.04") || p.wslDistroAllowed("Debian") || p.wslDistroAllowed("") {
		t.Errorf("wslDistroAllowed wrong for %v", p.AllowedBinaries)
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
