// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestAppliedETagIsEmptyBeforeAnyApply pins the cold-start value: a manager
// that has never been handed a document acknowledges nothing. "" is the
// only honest answer, and it is what the wire field's omitempty then drops.
func TestAppliedETagIsEmptyBeforeAnyApply(t *testing.T) {
	m := newTestManager(t, allowlistPolicy())
	if got := m.AppliedETag(); got != "" {
		t.Fatalf("AppliedETag() before any Apply = %q, want \"\"", got)
	}
}

// TestAppliedETagReportsTheAppliedDocument is the basic contract: after
// Apply returns, the manager names the document it just reconciled -- and
// after a second Apply, the second document, not the first.
func TestAppliedETagReportsTheAppliedDocument(t *testing.T) {
	m := newTestManager(t, allowlistPolicy())

	m.Apply(Config{ETag: "etag-a", Specs: []Spec{baseSpec("spec-a", "model-a")}})
	if got := m.AppliedETag(); got != "etag-a" {
		t.Fatalf("AppliedETag() after applying etag-a = %q, want %q", got, "etag-a")
	}

	m.Apply(Config{ETag: "etag-b", Specs: []Spec{baseSpec("spec-a", "model-a")}})
	if got := m.AppliedETag(); got != "etag-b" {
		t.Fatalf("AppliedETag() after applying etag-b = %q, want %q", got, "etag-b")
	}
}

// TestAppliedETagIsClearedByAnUnversionedApply pins what the driver's own
// drain path (stopAll -> Apply(emptyConfig()), which carries no ETag)
// reports: nothing. A drained agent must not keep acknowledging the last
// gateway document it happened to hold -- the overrides in that document
// are no longer being enforced by anything.
func TestAppliedETagIsClearedByAnUnversionedApply(t *testing.T) {
	m := newTestManager(t, allowlistPolicy())

	m.Apply(Config{ETag: "etag-a", Specs: []Spec{baseSpec("spec-a", "model-a")}})
	m.Apply(emptyConfig())

	if got := m.AppliedETag(); got != "" {
		t.Fatalf("AppliedETag() after an ETag-less Apply = %q, want \"\" -- a drained manager acknowledges nothing", got)
	}
}

// TestAppliedETagSurvivesAnIdempotentReapply pins the interaction with
// applyConfig's own short circuit (a Config whose ETag equals the applied
// one is a no-op): the acknowledgement must still name it. A re-applied
// identical document is the steady state of every poll cycle, so an
// implementation that only recorded the ETag on the CHANGED path would stop
// acknowledging the document it is still enforcing.
func TestAppliedETagSurvivesAnIdempotentReapply(t *testing.T) {
	m := newTestManager(t, allowlistPolicy())
	cfg := Config{ETag: "etag-a", Specs: []Spec{baseSpec("spec-a", "model-a")}}

	m.Apply(cfg)
	m.Apply(cfg)

	if got := m.AppliedETag(); got != "etag-a" {
		t.Fatalf("AppliedETag() after re-applying the same document = %q, want %q", got, "etag-a")
	}
}

// TestAppliedETagNeverOutrunsReconciliation is the test this whole feature
// exists to make possible, and the one that must not be deleted: the ETag
// the agent reports must never name a document whose reconciliation has not
// happened. The gateway trusts the acknowledgement to mean "the overrides in
// that document are in force"; an ETag published one beat early is exactly
// the lie it would then trust.
//
// The boundary is established STRUCTURALLY rather than by a timing guess:
// owner.applyConfig runs to completion inside a single turn of the owner's
// event loop (owner.run), so a cmdAppliedETag posted mid-apply cannot be
// served until that turn ends. This test blocks the owner in the MIDDLE of
// an apply -- the installed measurer is invoked synchronously on the owner
// from admitAndStart -> buildSnapshot -- and proves the new ETag is not
// observable from another goroutine while it is blocked there.
//
// It fails against every implementation that publishes the ETag outside the
// reconciler: an atomic store at the top of applyConfig, a field the Driver
// sets before calling Apply, or the config source's own fetched-document
// ETag (GatewaySource.etag, which advances at FETCH time and is precisely
// the value that would be a lie).
func TestAppliedETagNeverOutrunsReconciliation(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)

	m := newTestManager(t, allowlistPolicy())

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	releaseNow := func() { releaseOnce.Do(func() { close(release) }) }
	// Registered AFTER newTestManager's own m.Close cleanup, so LIFO order
	// runs this one FIRST: a t.Fatalf below must never leave the owner
	// goroutine parked in the measurer, which would deadlock Close (it
	// waits on the same goroutine) and hang the whole package's test binary
	// until the go-test timeout instead of reporting the failure.
	t.Cleanup(releaseNow)
	m.SetMeasurer(func([]int) map[int]map[int]int {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return nil
	})

	spec := baseSpec("spec-a", "model-a")
	spec.Pinned = true // pinned -> applyConfig calls admitAndStart -> buildSnapshot -> measurer

	applyDone := make(chan struct{})
	go func() {
		defer close(applyDone)
		m.Apply(Config{ETag: "etag-new", Specs: []Spec{spec}})
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the measurer was never invoked; this test needs the owner blocked mid-apply")
	}

	// The owner is now provably inside applyConfig. Ask for the
	// acknowledgement from another goroutine: it may block (the honest
	// outcome -- the command is queued behind the apply) or answer with a
	// pre-apply value, but it must NEVER answer "etag-new".
	got := make(chan string, 1)
	go func() { got <- m.AppliedETag() }()

	select {
	case v := <-got:
		if v == "etag-new" {
			t.Fatalf("AppliedETag() = %q while the owner is still reconciling that very document -- the acknowledgement must not outrun the reconciler", v)
		}
	case <-time.After(200 * time.Millisecond):
		// Queued behind the apply, which is the expected shape.
	}

	releaseNow()
	<-applyDone

	if v := m.AppliedETag(); v != "etag-new" {
		t.Fatalf("AppliedETag() after the apply completed = %q, want %q", v, "etag-new")
	}
}

// TestAppliedETagOfADrainDocumentImpliesNoForceStoppedSpecRuns pins the
// claim the VRAM benchmark's isolation wait rests on, in the exact shape it
// uses: once the acknowledgement names the document that force-stops every
// spec, no force-stopped spec is Running any more -- its drain has at least
// been INITIATED (Draining, or already gone) -- and nothing can start one
// again, because both admission entry points refuse force_stopped outright.
//
// Note the deliberate weakness of the assertion, which is the honest one:
// the acknowledgement means "this document is my desired state and every
// reconciliation decision it implies is committed", NOT "no process is
// alive". A drain's SIGTERM/grace/kill sequence is asynchronous, and the
// per-spec runtimes[].state in the same telemetry sample is what reports
// how far it got. A consumer that reads the acknowledgement as "the GPU is
// free" is reading more than is written here.
func TestAppliedETagOfADrainDocumentImpliesNoForceStoppedSpecRuns(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)

	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-a", "model-a")
	spec.Pinned = true
	m.Apply(Config{ETag: "etag-running", Specs: []Spec{spec}})
	waitUntil(t, 5*time.Second, "spec-a running", func() bool {
		s := statusFor(m, "spec-a")
		return s != nil && s.State == StateRunning
	})

	drained := spec
	drained.AdminState = "force_stopped"
	m.Apply(Config{ETag: "etag-drained", Specs: []Spec{drained}})

	if got := m.AppliedETag(); got != "etag-drained" {
		t.Fatalf("AppliedETag() = %q, want %q", got, "etag-drained")
	}
	s := statusFor(m, "spec-a")
	if s == nil {
		t.Fatal("spec-a vanished from Status()")
	}
	if s.State == StateRunning {
		t.Fatalf("spec-a state = %q while the acknowledged document force-stops it -- the drain must be initiated before the document is acknowledged", s.State)
	}

	// And nothing can bring it back: both admission entry points refuse a
	// force_stopped spec, so the isolation the acknowledgement reports
	// cannot be undone by a router request arriving afterwards.
	if _, _, err := m.EnsureRunning(context.Background(), "model-a"); err == nil {
		t.Fatal("EnsureRunning succeeded for a force_stopped spec; the acknowledged isolation would not hold")
	}
}
