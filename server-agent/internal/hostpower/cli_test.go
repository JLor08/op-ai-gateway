// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseArgsDefaults(t *testing.T) {
	opts, interval, verbose, err := ParseArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.OutDir != "/var/lib/op-hostpower" || interval != 250*time.Millisecond || verbose {
		t.Fatalf("got %+v %v %v", opts, interval, verbose)
	}
}

func TestParseArgsCustom(t *testing.T) {
	opts, interval, verbose, err := ParseArgs([]string{"-out", "/tmp/x", "-interval", "500ms", "-v"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.OutDir != "/tmp/x" || interval != 500*time.Millisecond || !verbose {
		t.Fatalf("got %+v %v %v", opts, interval, verbose)
	}
}

func TestParseArgsIntervalFloor(t *testing.T) {
	_, _, _, err := ParseArgs([]string{"-interval", "50ms"})
	if err == nil || !strings.Contains(err.Error(), "at least 100ms") {
		t.Fatalf("err = %v", err)
	}
}

func TestMainRejectsBadInterval(t *testing.T) {
	var buf bytes.Buffer
	if code := Main([]string{"-interval", "1ms"}, &buf); code != 2 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(buf.String(), "at least 100ms") {
		t.Fatalf("stderr = %q", buf.String())
	}
}

func TestParseArgsRejectsPositional(t *testing.T) {
	_, _, _, err := ParseArgs([]string{"-out", "/tmp/x", "extra"})
	if err == nil || err.Error() != `hostpower: unexpected argument "extra"` {
		t.Fatalf("err = %v", err)
	}
}

func TestMainRejectsPositional(t *testing.T) {
	var buf bytes.Buffer
	if code := Main([]string{"run"}, &buf); code != 2 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(buf.String(), `unexpected argument "run"`) {
		t.Fatalf("stderr = %q", buf.String())
	}
}

func warnOpts(t *testing.T) (Options, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cgroup", "qemu.slice"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Options{PowercapRoot: filepath.Join(root, "powercap"), CgroupRoot: filepath.Join(root, "cgroup")}, root
}

func TestStartupWarningsReadablePackageIsQuiet(t *testing.T) {
	opts, root := warnOpts(t)
	d := filepath.Join(root, "powercap", "intel-rapl:0")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	for n, c := range map[string]string{"name": "package-0\n", "energy_uj": "1\n"} {
		if err := os.WriteFile(filepath.Join(d, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if w := startupWarnings(opts, false); len(w) != 0 {
		t.Fatalf("warnings = %v, want none", w)
	}
	if w := startupWarnings(opts, true); !slices.Equal(w, []string{msgNotReadable}) {
		t.Fatalf("not root: warnings = %v", w)
	}
}

func TestStartupWarningsUnreadablePackage(t *testing.T) {
	opts, root := warnOpts(t)
	// A package domain without a readable energy_uj (the non-root EACCES path)
	// next to a readable core subdomain, which does not count.
	pkg, core := filepath.Join(root, "powercap", "intel-rapl:0"), filepath.Join(root, "powercap", "intel-rapl:0:0")
	for d, files := range map[string]map[string]string{
		pkg:  {"name": "package-0\n"},
		core: {"name": "core\n", "energy_uj": "1\n"},
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		for n, c := range files {
			if err := os.WriteFile(filepath.Join(d, n), []byte(c), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if w := startupWarnings(opts, false); !slices.Equal(w, []string{msgNotReadable}) {
		t.Fatalf("warnings = %v", w)
	}
}

func TestStartupWarningsNoRAPLNoSlice(t *testing.T) {
	root := t.TempDir()
	w := startupWarnings(Options{PowercapRoot: filepath.Join(root, "p"), CgroupRoot: filepath.Join(root, "c")}, false)
	if len(w) != 2 || !strings.Contains(w[0], "no RAPL package domain") || !strings.Contains(w[1], "no qemu.slice") {
		t.Fatalf("warnings = %v", w)
	}
}
