// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func serveHelperSocket(t *testing.T, h http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hc")
	noErr(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	noErr(t, err)
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return path
}

func TestHelperClientOverUnixSocket(t *testing.T) {
	var host string
	path := serveHelperSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host = r.Host
		_, _ = w.Write([]byte(`{"domains":[{"zone":"intel-rapl:0","name":"package-0","energy_uj":5,"max_energy_range_uj":9}]}`))
	}))
	var out HelperRAPL
	noErr(t, fetchHelperJSON(context.Background(), NewHelperHTTPClient(path), HelperRAPLPath, 4096, &out))
	eq(t, []HelperRAPLDomain{{Zone: "intel-rapl:0", Name: "package-0", EnergyUJ: 5, MaxEnergyRangeUJ: 9}}, out.Domains)
	eq(t, "op-agent-helper", host)
}

func TestHelperClientIgnoresProxyEnv(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	path := serveHelperSocket(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"domains":[]}`))
	}))
	var out HelperRAPL
	noErr(t, fetchHelperJSON(context.Background(), NewHelperHTTPClient(path), HelperRAPLPath, 4096, &out))
}

func TestHelperClientNoRedirect(t *testing.T) {
	path := serveHelperSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/x", http.StatusFound)
	}))
	var out HelperRAPL
	err := fetchHelperJSON(context.Background(), NewHelperHTTPClient(path), HelperRAPLPath, 4096, &out)
	var se *hostpowerStatusError
	if !errors.As(err, &se) {
		t.Fatalf("want status error, got %v", err)
	}
	eq(t, http.StatusFound, se.code)
}

func TestFetchHelperJSONCapBoundary(t *testing.T) {
	const max = 64
	body := func(n int) string { // valid JSON of exactly n bytes
		return `{"domains":[],"pad":"` + strings.Repeat("a", n-len(`{"domains":[],"pad":""}`)) + `"}`
	}
	for _, tc := range []struct {
		n    int
		fail bool
	}{{max, false}, {max + 1, true}} {
		path := serveHelperSocket(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body(tc.n)))
		}))
		var out HelperRAPL
		err := fetchHelperJSON(context.Background(), NewHelperHTTPClient(path), HelperRAPLPath, max, &out)
		if tc.fail {
			errHas(t, err, "response body too large")
		} else {
			noErr(t, err)
		}
	}
}

func TestFetchHelperMemoryModulesCap(t *testing.T) {
	mods := func(target int) []byte {
		pad := strings.Repeat("x", target-len(`{"modules":[],"pad":""}`))
		b, err := json.Marshal(map[string]any{"modules": []any{}, "pad": pad})
		noErr(t, err)
		eq(t, target, len(b))
		return b
	}
	for _, tc := range []struct {
		size int
		fail bool
	}{{60 * 1024, false}, {65537, true}} {
		b := mods(tc.size)
		path := serveHelperSocket(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(b) }))
		got, err := FetchHelperMemoryModules(context.Background(), NewHelperHTTPClient(path))
		if tc.fail {
			errHas(t, err, "response body too large")
		} else {
			noErr(t, err)
			empty(t, got)
		}
	}
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func eq(t *testing.T, want, got any) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("want %#v, got %#v", want, got)
	}
}

func errHas(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), sub) {
		t.Fatalf("want error containing %q, got %v", sub, err)
	}
}

func empty(t *testing.T, v any) {
	t.Helper()
	if rv := reflect.ValueOf(v); rv.Len() != 0 {
		t.Fatalf("want empty, got %#v", v)
	}
}
