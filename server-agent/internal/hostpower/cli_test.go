// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"op-ai-server-agent/internal/collector"
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

func TestParseArgsTLSNeedsListen(t *testing.T) {
	_, err := ParseArgs([]string{"-tls"})
	if err == nil || err.Error() != "hostpower: -tls needs -listen" {
		t.Fatalf("err = %v", err)
	}
	var buf bytes.Buffer
	if code := Main([]string{"-tls"}, io.Discard, &buf); code != 2 || !strings.Contains(buf.String(), "-tls needs -listen") {
		t.Fatalf("code = %d stderr=%q", code, buf.String())
	}
	// The path flags alone are only read with -tls.
	if _, err := ParseArgs([]string{"-tls-cert", "/x.crt", "-tls-key", "/x.key"}); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestParseArgsTLSDefaults(t *testing.T) {
	cfg, err := ParseArgs([]string{"-listen", "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLS || cfg.TLSCert != DefaultTLSCertPath || cfg.TLSKey != DefaultTLSKeyPath {
		t.Fatalf("without -tls: %+v", cfg)
	}
	cfg, err = ParseArgs([]string{"-listen", "127.0.0.1:0", "-tls"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.TLS || cfg.TLSCert != DefaultTLSCertPath || cfg.TLSKey != DefaultTLSKeyPath {
		t.Fatalf("-tls: %+v", cfg)
	}
	cfg, err = ParseArgs([]string{"-listen", "127.0.0.1:0", "-tls", "-tls-cert", "/x.crt", "-tls-key", "/x.key"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.TLS || cfg.TLSCert != "/x.crt" || cfg.TLSKey != "/x.key" {
		t.Fatalf("custom paths: %+v", cfg)
	}
}

func TestMainHelpListsAllFlags(t *testing.T) {
	var buf bytes.Buffer
	if code := Main([]string{"-h"}, io.Discard, &buf); code != 0 {
		t.Fatalf("code = %d", code)
	}
	for _, want := range []string{
		"usage: server-agent hostpower [-out dir] [-interval duration] [-listen host:port] [-auth mac,token] [-tokens path] [-tls] [-tls-cert path] [-tls-key path] [-v]",
		"       server-agent hostpower token [-tokens path] create|revoke|list [vmid]",
		"       server-agent hostpower tls [-tls-cert path] fingerprint",
		"-listen", "-auth", "-tokens", "-tls-cert", "-tls-key",
		"serve the -listen endpoint over HTTPS (TLS 1.3)",
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

func TestMainTLSFingerprint(t *testing.T) {
	certPath, keyPath := certPaths(t.TempDir())
	cert, _, err := LoadOrCreateCert(certPath, keyPath, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Main([]string{"tls", "-tls-cert", certPath, "fingerprint"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d stderr=%q", code, errb.String())
	}
	if want := collector.FormatHostpowerFingerprint(cert.Certificate[0]) + "\n"; out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
}

func TestMainTLSFingerprintSymlinkedCert(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	certPath, keyPath := certPaths(t.TempDir())
	cert, _, err := LoadOrCreateCert(certPath, keyPath, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "live.crt")
	if err := os.Symlink(certPath, link); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Main([]string{"tls", "-tls-cert", link, "fingerprint"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d stderr=%q", code, errb.String())
	}
	if want := collector.FormatHostpowerFingerprint(cert.Certificate[0]) + "\n"; out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
}

func TestMainTLSFingerprintNonRegularExits1(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFOs and symlinks")
	}
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo.crt")
	if err := mkfifo(fifo); err != nil {
		t.Fatal(err)
	}
	fifoLink := filepath.Join(dir, "fifo-link.crt")
	dangling := filepath.Join(dir, "dangling.crt")
	for link, target := range map[string]string{fifoLink: fifo, dangling: filepath.Join(dir, "missing.crt")} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{fifo, fifoLink, dangling} {
		var out, errb bytes.Buffer
		done := make(chan int, 1)
		go func() { done <- Main([]string{"tls", "-tls-cert", path, "fingerprint"}, &out, &errb) }()
		select {
		case code := <-done:
			if code != 1 || !strings.Contains(errb.String(), path) {
				t.Errorf("%s: code = %d stderr=%q", path, code, errb.String())
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: fingerprint blocked on a non-regular file", path)
		}
	}
}

func TestMainTLSFingerprintUnreadableExits1(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.crt")
	garbage := filepath.Join(dir, "garbage.crt")
	if err := os.WriteFile(garbage, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{missing, garbage} {
		var out, errb bytes.Buffer
		if code := Main([]string{"tls", "-tls-cert", path, "fingerprint"}, &out, &errb); code != 1 {
			t.Errorf("%s: code = %d, want 1", path, code)
		}
		if out.Len() != 0 || !strings.Contains(errb.String(), path) {
			t.Errorf("%s: stdout=%q stderr=%q", path, out.String(), errb.String())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "tls.key")); err == nil {
		t.Fatal("fingerprint verb generated a key")
	}
}

func TestMainTLSUsageErrorsExit2(t *testing.T) {
	for _, args := range [][]string{
		{"tls"},
		{"tls", "frob"},
		{"tls", "fingerprint", "extra"},
		{"tls", "-nosuchflag", "fingerprint"},
	} {
		var out, errb bytes.Buffer
		if code := Main(args, &out, &errb); code != 2 {
			t.Errorf("%v: code = %d, want 2", args, code)
		}
		if out.Len() != 0 || !strings.Contains(errb.String(), "usage: server-agent hostpower tls") {
			t.Errorf("%v: stdout=%q stderr=%q", args, out.String(), errb.String())
		}
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

// freeAddr reserves a free loopback port and releases it for run to bind (the
// listen address is the only handle the test has).
func freeAddr(t *testing.T) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()
	return addr
}

func TestServeTLSTokenMode(t *testing.T) {
	dir := t.TempDir()
	tokens := filepath.Join(dir, "tokens.json")
	token, err := CreateToken(tokens, "100")
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := certPaths(filepath.Join(dir, "tls"))
	addr := freeAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr lockedBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{
			"-out", filepath.Join(dir, "out"), "-interval", "100ms",
			"-listen", addr, "-auth", "token", "-tokens", tokens,
			"-tls", "-tls-cert", certPath, "-tls-key", keyPath,
		}, io.Discard, &stderr)
	}()

	// The certificate is written before the listener opens.
	var der []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		if der, err = readCertDER(certPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no certificate: %v; stderr=%s", err, stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	fp := collector.FormatHostpowerFingerprint(der)
	pin, err := collector.ParseHostpowerFingerprint(fp)
	if err != nil {
		t.Fatal(err)
	}

	do := func(client *http.Client, url string) int {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		deadline := time.Now().Add(5 * time.Second)
		for {
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				return resp.StatusCode
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %v; stderr=%s", url, err, stderr.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if code := do(collector.NewHostpowerHTTPClient(pin), "https://"+addr+"/v1/energy"); code != http.StatusServiceUnavailable && code != http.StatusOK {
		t.Fatalf("pinned https: code = %d", code)
	}
	if code := do(http.DefaultClient, "http://"+addr+"/v1/energy"); code != http.StatusBadRequest {
		t.Fatalf("plain http: code = %d, want 400", code)
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
	logs := stderr.String()
	for _, want := range []string{
		"hostpower: tls certificate", "cert=" + certPath, "sha256=" + fp, "generated=true",
		"hostpower: serving", "tls=true",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
}

func TestServePlainLogsTLSFalse(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	var stderr lockedBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{
			"-out", filepath.Join(dir, "out"), "-interval", "100ms",
			"-listen", "127.0.0.1:0", "-auth", "token", "-tokens", filepath.Join(dir, "tokens.json"),
		}, io.Discard, &stderr)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(stderr.String(), "hostpower: serving") {
		if time.Now().After(deadline) {
			t.Fatalf("no serving log: %s", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit = %d stderr=%s", code, stderr.String())
	}
	if logs := stderr.String(); !strings.Contains(logs, "tls=false") || strings.Contains(logs, "hostpower: tls certificate") {
		t.Fatalf("log = %s", logs)
	}
}

func TestServeTLSCertFailureExits2BeforeListening(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := certPaths(filepath.Join(dir, "tls"))
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("half a pair"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	code := Main([]string{
		"-out", filepath.Join(dir, "out"), "-listen", "127.0.0.1:0", "-auth", "token",
		"-tokens", filepath.Join(dir, "tokens.json"), "-tls", "-tls-cert", certPath, "-tls-key", keyPath,
	}, io.Discard, &buf)
	if code != 2 {
		t.Fatalf("code = %d stderr=%q", code, buf.String())
	}
	if !strings.Contains(buf.String(), "server-agent: hostpower: tls key ") || !strings.Contains(buf.String(), certPath) ||
		strings.Contains(buf.String(), "hostpower: serving") {
		t.Fatalf("stderr = %q", buf.String())
	}
	if _, err := os.Lstat(certPath); err == nil {
		t.Fatal("certificate written next to a lone key")
	}
}

func TestEnsureOutDirTightensExistingDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	dir := filepath.Join(t.TempDir(), "out")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // independent of the umask
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	if code := run(ctx, []string{"-out", dir}, io.Discard, &buf); code != 0 {
		t.Fatalf("code = %d stderr=%q", code, buf.String())
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("out dir mode = %o, want 700", perm)
	}

	created := filepath.Join(t.TempDir(), "new", "out")
	if err := ensureOutDir(created); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(created); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("created out dir: %v, %v", fi, err)
	}
}

func TestMainOutDirNotADirectoryExits2(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if code := Main([]string{"-out", file}, io.Discard, &buf); code != 2 || !strings.Contains(buf.String(), "not a directory") {
		t.Fatalf("file: code = %d stderr=%q", code, buf.String())
	}

	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	buf.Reset()
	if code := Main([]string{"-out", link}, io.Discard, &buf); code != 2 || !strings.Contains(buf.String(), "symlink") {
		t.Fatalf("symlink: code = %d stderr=%q", code, buf.String())
	}
}
