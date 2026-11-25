// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"bytes"
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
