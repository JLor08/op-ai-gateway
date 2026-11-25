// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseArgsDefaults(t *testing.T) {
	cfg, err := ParseArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OutDir != "/var/lib/op-hostpower" || cfg.Interval != 250*time.Millisecond || cfg.Verbose {
		t.Fatalf("got %+v", cfg)
	}
}

func TestParseArgsCustom(t *testing.T) {
	cfg, err := ParseArgs([]string{"-out", "/tmp/x", "-interval", "500ms", "-v"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OutDir != "/tmp/x" || cfg.Interval != 500*time.Millisecond || !cfg.Verbose {
		t.Fatalf("got %+v", cfg)
	}
}

func TestParseArgsIntervalFloor(t *testing.T) {
	_, err := ParseArgs([]string{"-interval", "50ms"})
	if err == nil || !strings.Contains(err.Error(), "at least 100ms") {
		t.Fatalf("err = %v", err)
	}
}

func TestMainRejectsBadInterval(t *testing.T) {
	var buf bytes.Buffer
	if code := Main([]string{"-interval", "1ms"}, io.Discard, &buf); code != 2 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(buf.String(), "at least 100ms") {
		t.Fatalf("stderr = %q", buf.String())
	}
}

func TestParseArgsRejectsPositional(t *testing.T) {
	_, err := ParseArgs([]string{"-out", "/tmp/x", "extra"})
	if err == nil || err.Error() != `hostpower: unexpected argument "extra"` {
		t.Fatalf("err = %v", err)
	}
}

func TestMainRejectsPositional(t *testing.T) {
	var buf bytes.Buffer
	if code := Main([]string{"run"}, io.Discard, &buf); code != 2 {
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

func TestParseArgsHTTPDefaults(t *testing.T) {
	cfg, err := ParseArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "" || cfg.Auth != (AuthModes{MAC: true, Token: true}) || cfg.TokensPath != DefaultTokensPath {
		t.Fatalf("got %+v", cfg)
	}
}

func TestParseArgsHTTPCustom(t *testing.T) {
	cfg, err := ParseArgs([]string{"-listen", "127.0.0.1:0", "-auth", "token", "-tokens", "/tmp/t.json"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:0" || cfg.Auth != (AuthModes{Token: true}) || cfg.TokensPath != "/tmp/t.json" {
		t.Fatalf("got %+v", cfg)
	}
}

func TestParseArgsBadAuth(t *testing.T) {
	if _, err := ParseArgs([]string{"-listen", "127.0.0.1:0", "-auth", "foo"}); err == nil {
		t.Fatal("want error")
	}
	// -auth is only validated when HTTP is enabled.
	if _, err := ParseArgs([]string{"-auth", "foo"}); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestMainHelpListsAllFlags(t *testing.T) {
	var buf bytes.Buffer
	if code := Main([]string{"-h"}, io.Discard, &buf); code != 0 {
		t.Fatalf("code = %d", code)
	}
	for _, want := range []string{
		"usage: server-agent hostpower [-out dir] [-interval duration] [-listen host:port] [-auth mac,token] [-tokens path] [-v]",
		"       server-agent hostpower token [-tokens path] create|revoke|list [vmid]",
		"-listen", "-auth", "-tokens",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("usage lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestMainTokenSubcommand(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tokens.json")
	var out, errb bytes.Buffer
	if code := Main([]string{"token", "-tokens", p, "create", "100"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d stderr=%q", code, errb.String())
	}
	if !strings.HasPrefix(strings.TrimSpace(out.String()), "ophp_") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestMainListenFailureExits2(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var buf bytes.Buffer
	code := Main([]string{"-out", t.TempDir(), "-listen", ln.Addr().String(), "-auth", "token"}, io.Discard, &buf)
	if code != 2 {
		t.Fatalf("code = %d stderr=%q", code, buf.String())
	}
}

func TestMainMACModeOnNonLinuxExits2(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("MAC mode is supported on Linux")
	}
	var buf bytes.Buffer
	code := Main([]string{"-out", t.TempDir(), "-listen", "127.0.0.1:0", "-auth", "mac"}, io.Discard, &buf)
	if code != 2 || !strings.Contains(buf.String(), "needs Linux") {
		t.Fatalf("code = %d stderr=%q", code, buf.String())
	}
}

func TestMainMACModeWithoutPrivilegeExits2(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() == 0 {
		t.Skip("needs Linux and an unprivileged user")
	}
	var buf bytes.Buffer
	code := Main([]string{"-out", t.TempDir(), "-listen", "127.0.0.1:0", "-auth", "mac"}, io.Discard, &buf)
	if code != 2 || !strings.Contains(buf.String(), "capture") {
		t.Fatalf("code = %d stderr=%q", code, buf.String())
	}
}

// lockedBuffer is a bytes.Buffer safe for the logger goroutines and the test.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestRunServesTokenModeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	tokens := filepath.Join(dir, "tokens.json")
	token, err := CreateToken(tokens, "100")
	if err != nil {
		t.Fatal(err)
	}
	// Reserve a free port, release it and let run bind it (the listen address is
	// the only handle the test has).
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var stderr lockedBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{
			"-out", filepath.Join(dir, "out"), "-interval", "100ms",
			"-listen", addr, "-auth", "token", "-tokens", tokens,
		}, io.Discard, &stderr)
	}()

	get := func(auth string) int {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/energy", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
				return resp.StatusCode
			}
			if time.Now().After(deadline) {
				t.Fatalf("server not reachable: %v; stderr=%s", err, stderr.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if code := get("Bearer " + token); code != http.StatusServiceUnavailable {
		t.Fatalf("with token: code = %d", code)
	}
	if code := get(""); code != http.StatusUnauthorized {
		t.Fatalf("without header (mac off): code = %d", code)
	}
	if code := get("Bearer ophp_wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: code = %d", code)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit = %d stderr=%s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	if !strings.Contains(stderr.String(), "hostpower: serving") {
		t.Fatalf("no serving log: %s", stderr.String())
	}
}
