// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package helper

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	cfg, err := ParseArgs([]string{"-agent-user", "op"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Socket != "/run/op-agent-helper/agent.sock" || cfg.PowercapRoot != "/sys/class/powercap" ||
		cfg.DMITable != "/sys/firmware/dmi/tables/DMI" || cfg.AgentUser != "op" || cfg.Verbose {
		t.Fatalf("defaults: %+v", cfg)
	}
	if _, err := ParseArgs(nil); err == nil {
		t.Fatal("missing -agent-user accepted")
	}
	if _, err := ParseArgs([]string{"-agent-user", "op", "extra"}); err == nil {
		t.Fatal("positional argument accepted")
	}
}

func TestMainRefusesRoot(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"-agent-user", "0"}, &out, &errb); code != 2 {
		t.Fatalf("exit %d, stderr %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "must not be root") {
		t.Fatalf("stderr lacks the root refusal: %s", errb.String())
	}
}
