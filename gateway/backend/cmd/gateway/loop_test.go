// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// TestStartCancellableCancelWaitsForTheLoopGoroutine pins the whole point of
// startCancellable: when the cancel func RETURNS, the goroutine is gone.
//
// buildGatewayServer's cleanup() cancels every background loop and THEN closes
// the store, so a cancel that only signalled left a pass querying a store being
// closed underneath it. That reached CI as a red job whose test body passed:
// "TempDir RemoveAll cleanup: unlinkat .../001: directory not empty", because
// database/sql calls connector.Connect outside db.mu (and openNewConnection
// calls it before checking db.closed) and sqlite's Connect RE-CREATES the
// database file — after t.TempDir()'s RemoveAll had already scanned the
// directory empty, so its final rmdir failed.
//
// The Gosched loop is what makes this deterministic in BOTH directions: a
// signal-only cancel returns while the goroutine is still yielding and observes
// exited == false, while a waiting cancel cannot return until after the store.
func TestStartCancellableCancelWaitsForTheLoopGoroutine(t *testing.T) {
	var exited atomic.Bool
	started := make(chan struct{})
	cancel := startCancellable(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		for range 1000 {
			runtime.Gosched()
		}
		exited.Store(true)
	})
	<-started

	cancel()

	if !exited.Load() {
		t.Fatal("cancel() returned while the loop goroutine was still running -- " +
			"cleanup() would then close the store underneath a pass in flight")
	}
}

// TestStartCancellableCancelIsRepeatable: cleanup() is folded from several
// layers and is reachable from both main's defer and a test's, so the cancel it
// calls must tolerate being called again (and concurrently) without blocking or
// panicking. Context cancellation is idempotent and a receive on a closed
// channel returns immediately; this pins that, since the wait added above is
// exactly the kind of thing that turns a harmless second call into a deadlock.
func TestStartCancellableCancelIsRepeatable(t *testing.T) {
	cancel := startCancellable(func(ctx context.Context) { <-ctx.Done() })
	cancel()
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		cancel()
	}()
	<-done
}

// TestNoStarterReturnsARawCancelFunc is the tripwire for the "fixed here, left
// there" failure mode: the flake above came from SIX independent start*Loop
// helpers that each hand-rolled the same signal-only shape, all folded into the
// same cleanup() that closes the store. Fixing five and leaving one would have
// left the bug fully intact.
//
// The signature of that shape is handing a caller the raw CancelFunc that
// context.WithCancel returned, so that `cancel()` means "signalled" rather than
// "stopped". After the fix no non-test file in this package returns one:
// startCancellable returns a closure that also waits. A seventh loop added later
// with `return cancel` trips this.
func TestNoStarterReturnsARawCancelFunc(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	rawCancel := regexp.MustCompile(`\breturn cancel\b`)
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		checked++
		if rawCancel.Match(src) {
			t.Errorf("%s hands back the raw CancelFunc from context.WithCancel "+
				"instead of going through startCancellable: that cancel would only "+
				"SIGNAL, so cleanup() could close the store underneath a pass still "+
				"in flight -- see startCancellable's doc in loop.go", name)
		}
	}
	// Positive precondition: a glob that matched nothing would pass vacuously.
	if checked < 5 {
		t.Fatalf("scanned only %d non-test sources in this package, expected many more", checked)
	}
}
