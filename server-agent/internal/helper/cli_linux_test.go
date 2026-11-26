// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package helper

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRunShutdownRemovesSocketAndWarnsOnMissingDMI(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to own the socket directory")
	}
	base, err := os.MkdirTemp("/tmp", "hs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	_ = os.Chmod(base, 0o755)
	sock := base + "/d/agent.sock"

	ctx, cancel := context.WithCancel(context.Background())
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{
			"-agent-user", "1000", "-agent-group", "1000", "-socket", sock,
			"-powercap-root", base, "-dmi-table", base + "/nonexistent",
		}, nil, &stderr)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if fi, err := os.Lstat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket never appeared; stderr: %s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d, stderr %s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket still present after shutdown: %v", err)
	}
	if !strings.Contains(stderr.String(), "DMI table not readable; serving no memory modules") {
		t.Fatalf("missing DMI warning: %s", stderr.String())
	}
}
