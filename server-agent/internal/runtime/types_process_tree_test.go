// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"errors"
	"testing"
	"time"
)

// TestParseConfigProcessTree pins the wire contract of process_tree (#424): an
// absent or empty field is the direct mode every older gateway means, "direct"
// and "job" are kept as they are, and anything else drops the spec, exactly as
// an unknown execution mode does -- a newer gateway's mode may need handling
// this agent lacks, and launching it as direct would quietly run it wrongly.
func TestParseConfigProcessTree(t *testing.T) {
	keep := map[string]struct {
		doc  []byte
		want string
	}{
		"absent":  {execDoc(``), ProcessTreeDirect},
		"empty":   {execDoc(`,"process_tree":""`), ProcessTreeDirect},
		"direct":  {execDoc(`,"process_tree":"direct"`), ProcessTreeDirect},
		"job":     {execDoc(`,"process_tree":"job"`), ProcessTreeJob},
		"wsl job": {execDoc(`,"process_tree":"job","execution":{"mode":"wsl","wsl":{"distro":"Ubuntu"}}`), ProcessTreeJob},
		// An rpc_server member is native too; execDoc's upstream_model would make it invalid.
		"rpc job": {[]byte(`{"etag":"e","specs":[{"id":"S","binary":"/x","process_tree":"job","kind":"rpc_server","rpc":{"head_spec_id":"h","transport":"direct","bind_ip":"127.0.0.1","port":47001}}]}`), ProcessTreeJob},
	}
	for name, tc := range keep {
		t.Run(name, func(t *testing.T) {
			cfg, err := ParseConfig(tc.doc)
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			if len(cfg.Specs) != 1 {
				t.Fatalf("specs = %d, want the spec kept", len(cfg.Specs))
			}
			if got := cfg.Specs[0].ProcessTreeMode(); got != tc.want {
				t.Fatalf("ProcessTreeMode() = %q, want %q", got, tc.want)
			}
		})
	}

	for name, extra := range map[string]string{
		"unknown":    `,"process_tree":"cgroup"`,
		"upper case": `,"process_tree":"JOB"`,
		"padded":     `,"process_tree":" job"`,
	} {
		t.Run("drops "+name, func(t *testing.T) {
			cfg, err := ParseConfig(execDoc(extra))
			if err != nil {
				t.Fatalf("ParseConfig: %v, want the spec dropped without an error", err)
			}
			if len(cfg.Specs) != 0 {
				t.Fatalf("specs = %+v, want the invalid spec dropped", cfg.Specs)
			}
		})
	}
}

// TestSameLaunchShapeTreatsProcessTreeAsLaunchShape pins process_tree as
// launch-affecting (design §4.5, the §5.6 table): it decides how the child is
// created (suspended, in a job, with a console of its own), so a direct<->job
// edit relaunches. The two spellings of direct are the same launch and must
// not bounce a healthy child when a gateway starts sending the explicit form.
func TestSameLaunchShapeTreatsProcessTreeAsLaunchShape(t *testing.T) {
	base := Spec{ID: "s", Binary: "/bin/x", UpstreamModel: "m"}
	withTree := func(tree string) Spec {
		s := base
		s.ProcessTree = tree
		return s
	}
	if sameLaunchShape(withTree(""), withTree(ProcessTreeJob)) {
		t.Error("direct (absent) -> job must relaunch")
	}
	if sameLaunchShape(withTree(ProcessTreeDirect), withTree(ProcessTreeJob)) {
		t.Error("direct -> job must relaunch")
	}
	if sameLaunchShape(withTree(ProcessTreeJob), withTree(ProcessTreeDirect)) {
		t.Error("job -> direct must relaunch")
	}
	if !sameLaunchShape(withTree(""), withTree(ProcessTreeDirect)) {
		t.Error("an absent process_tree and an explicit direct are the same launch")
	}
	if !sameLaunchShape(withTree(ProcessTreeJob), withTree(ProcessTreeJob)) {
		t.Error("an unchanged job spec must not relaunch")
	}
}

// TestCallWithinBoundsADelivery pins the bound the Windows Ctrl-Break delivery
// runs under (callWithin): the owner goroutine waits for the call at most d,
// and a call that does not return in time reads as a failure, so terminate
// falls through to the kill instead of hanging the owner.
func TestCallWithinBoundsADelivery(t *testing.T) {
	if err := callWithin(time.Second, func() error { return nil }); err != nil {
		t.Fatalf("a call that returns nil: err = %v, want nil", err)
	}
	boom := errors.New("boom")
	if err := callWithin(time.Second, func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("a failing call: err = %v, want %v", err, boom)
	}
	release := make(chan struct{})
	defer close(release)
	start := time.Now()
	err := callWithin(50*time.Millisecond, func() error { <-release; return nil })
	if err == nil {
		t.Fatal("a call that outlives the bound must read as a failure")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("callWithin waited %v, want about the 50ms bound", waited)
	}
}
