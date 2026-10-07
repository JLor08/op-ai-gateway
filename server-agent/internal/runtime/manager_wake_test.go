// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// gatedSpec is baseSpec whose child stays in StateStarting until the test
// creates the returned gate file: its /health answers 503 until then
// (stubchild's -health-gate). The test, not a delay, decides when the load
// ends, so a request it queues behind the load is queued behind it for sure.
func gatedSpec(t *testing.T, id, upstreamModel string) (spec Spec, openGate func()) {
	t.Helper()
	gate := filepath.Join(t.TempDir(), id+".healthy")
	spec = baseSpec(id, upstreamModel)
	spec.Args = []string{"-port", "${PORT}", "-health-gate", gate}
	return spec, func() {
		t.Helper()
		if err := os.WriteFile(gate, nil, 0o644); err != nil {
			t.Fatalf("open the health gate of %s: %v", id, err)
		}
	}
}

// admissionProbe installs a measurer that only reports that it was asked.
// buildSnapshot calls the measurer once per admission decision, on the
// owner, so a receive from the returned channel means one admission attempt
// has been decided. No spec in these tests declares a GPU, so the
// housekeeping dispatch never calls it.
func admissionProbe(m *Manager) <-chan struct{} {
	admissions := make(chan struct{}, 16)
	m.SetMeasurer(func([]int) map[int]map[int]int {
		select {
		case admissions <- struct{}{}:
		default:
		}
		return nil
	})
	return admissions
}

func drainAdmissions(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

type queuedEnsure struct {
	endpoint string
	release  func()
	err      error
}

func ensureAsync(ctx context.Context, m *Manager, model string) <-chan queuedEnsure {
	out := make(chan queuedEnsure, 1)
	go func() {
		ep, rel, err := m.EnsureRunning(ctx, model)
		out <- queuedEnsure{endpoint: ep, release: rel, err: err}
	}()
	return out
}

func stateOf(m *Manager, specID string) State {
	if st := statusFor(m, specID); st != nil {
		return st.State
	}
	return ""
}

// TestManagerAdmitsARequestQueuedBehindAStartWithoutAWaiter: a request queued
// behind an unpinned start that has no waiter of its own is admitted when
// that start finishes. The loading neighbour may not be evicted (isEvictable's
// Starting clause), so the request Waits; once the neighbour is healthy and
// idle it is evictable, and nothing but the start's own result can say so --
// no release, exit or Apply follows it. Until agent 0.8.1 the request waited
// for an unrelated release, exit or Apply, and failed at its
// admission_wait_timeout_seconds with admission_blocked if none came first,
// or, when that is 0, waited until one came or its caller gave up.
//
// Two ways a start ends up without a waiter: a force_running spec started by
// Apply, and an on-demand spec whose only caller left while it loaded.
func TestManagerAdmitsARequestQueuedBehindAStartWithoutAWaiter(t *testing.T) {
	skipOnWindows(t)

	for _, tc := range []struct {
		name string
		// start leaves spec-n in StateStarting with no waiter queued on it.
		start func(t *testing.T, m *Manager, n, tgt Spec)
	}{
		{
			name: "force_running started by Apply",
			start: func(t *testing.T, m *Manager, n, tgt Spec) {
				n.AdminState = "force_running"
				m.Apply(Config{Specs: []Spec{n, tgt}})
			},
		},
		{
			name: "on-demand start whose caller left",
			start: func(t *testing.T, m *Manager, n, tgt Spec) {
				m.Apply(Config{Specs: []Spec{n, tgt}})
				ctx, cancel := context.WithCancel(context.Background())
				res := ensureAsync(ctx, m, "model-n")
				waitUntil(t, 3*time.Second, "spec-n starting", func() bool {
					return stateOf(m, "spec-n") == StateStarting
				})
				cancel()
				select {
				case r := <-res:
					if !errors.Is(r.err, context.Canceled) {
						t.Fatalf("EnsureRunning(model-n) after its caller left = %v, want context.Canceled", r.err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("EnsureRunning(model-n) did not return after its caller left")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shrinkTimings(t)
			m := newTestManager(t, allowlistPolicy())
			admissions := admissionProbe(m)

			// No co-residency pair: spec-n and spec-t may not run together.
			n, openGate := gatedSpec(t, "spec-n", "model-n")
			tgt := baseSpec("spec-t", "model-t")
			tgt.AdmissionWaitTimeoutSeconds = 3
			tc.start(t, m, n, tgt)
			if got := stateOf(m, "spec-n"); got != StateStarting {
				t.Fatalf("spec-n is %q, want starting (the test's premise)", got)
			}

			drainAdmissions(admissions)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			res := ensureAsync(ctx, m, "model-t")
			select {
			case <-admissions:
			case <-time.After(3 * time.Second):
				t.Fatal("the request for model-t was never admitted against the loading spec-n")
			}
			select {
			case r := <-res:
				t.Fatalf("EnsureRunning(model-t) returned %v while spec-n was still loading, want it queued", r.err)
			default:
			}

			openGate()

			select {
			case r := <-res:
				if r.err != nil {
					t.Fatalf("EnsureRunning(model-t) = %v, want it admitted once spec-n finished loading -- BUG: handleStartResult's success branch wakes no admission, so a request queued behind a start without a waiter waits for an unrelated event", r.err)
				}
				defer r.release()
				if code, _ := httpEcho(t, r.endpoint, "ping"); code != 200 {
					t.Fatalf("POST %s/v1/echo = %d, want 200", r.endpoint, code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("EnsureRunning(model-t) did not return after spec-n finished loading")
			}
			waitUntil(t, 3*time.Second, "spec-n evicted for spec-t", func() bool {
				return stateOf(m, "spec-n") == StateStopped
			})
		})
	}
}

// TestManagerStartWakeLeavesAForceRunningSpecWithoutAWaiter: the wake after a
// finished start does not retry a force_running spec that has no waiter.
// Two force_running specs that may not run together would otherwise evict
// each other with nothing in between: A comes up, the wake admits B, B evicts
// A, B comes up, the wake admits A, and so on -- one full model load per turn,
// for as long as the agent runs. Such a spec is retried at the next release,
// exit or Apply, and the second half pins that a release still retries it:
// the general wake keeps covering what this one leaves out.
//
// Deterministic without waiting for an absence: spec-a turns Running in the
// same owner command that runs the wake, so the first Status() that no longer
// reads starting already shows what the wake did. The wrong wake has drained
// spec-a by then.
func TestManagerStartWakeLeavesAForceRunningSpecWithoutAWaiter(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	execs := newExecCounter(t)
	m := newTestManager(t, allowlistPolicy())

	a, openGateA := gatedSpec(t, "spec-a", "model-a")
	a.AdminState = "force_running"
	b := baseSpec("spec-b", "model-b")
	m.Apply(Config{Specs: []Spec{a, b}})
	if got := stateOf(m, "spec-a"); got != StateStarting {
		t.Fatalf("spec-a is %q, want starting (the test's premise)", got)
	}
	// Second document: spec-b turns force_running while spec-a loads. Apply
	// admits it, and it Waits behind the loading spec-a.
	b.AdminState = "force_running"
	m.Apply(Config{Specs: []Spec{a, b}, ETag: "second"})
	if got := stateOf(m, "spec-b"); got != StateStopped {
		t.Fatalf("spec-b is %q, want stopped behind the loading spec-a (the test's premise)", got)
	}

	openGateA()
	waitUntil(t, 5*time.Second, "spec-a past starting", func() bool {
		return stateOf(m, "spec-a") != StateStarting
	})
	if got := stateOf(m, "spec-a"); got != StateRunning {
		t.Fatalf("spec-a is %q right after its start finished, want running -- BUG: the wake after the start retried the force_running spec-b, which evicted spec-a; two such specs evict each other without end", got)
	}
	if got := stateOf(m, "spec-b"); got != StateStopped {
		t.Fatalf("spec-b is %q, want stopped", got)
	}
	if n := execs.count("spec-b"); n != 0 {
		t.Fatalf("spec-b was exec'd %d time(s), want 0", n)
	}

	// Left to the next event, not dropped: a release on spec-a retries
	// spec-b, which evicts the now idle spec-a.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, release, err := m.EnsureRunning(ctx, "model-a")
	if err != nil {
		t.Fatalf("EnsureRunning(model-a) = %v", err)
	}
	release()
	waitUntil(t, 5*time.Second, "spec-b running after a release on spec-a -- BUG: the release no longer retries a force_running spec without a waiter", func() bool {
		return stateOf(m, "spec-b") == StateRunning
	})
	if n := execs.count("spec-b"); n != 1 {
		t.Fatalf("spec-b was exec'd %d time(s) after the release, want 1", n)
	}
}

// TestManagerStartWakeAdmitsAPinnedSpec: the wake after a finished start does
// retry a pinned spec without a waiter. A pinned spec is never evicted, so
// its start cannot be undone by the next such wake, and it is the case
// wakeAdmissionCandidates' own rule exists for: without a waiter, nothing
// else retries it until an unrelated event. Here a pinned spec waits behind a
// loading force_running one; once that is up and idle, the pinned spec evicts
// it.
func TestManagerStartWakeAdmitsAPinnedSpec(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	s, openGateS := gatedSpec(t, "spec-s", "model-s")
	s.AdminState = "force_running"
	x := baseSpec("spec-x", "model-x")
	m.Apply(Config{Specs: []Spec{s, x}})
	if got := stateOf(m, "spec-s"); got != StateStarting {
		t.Fatalf("spec-s is %q, want starting (the test's premise)", got)
	}
	x.Pinned = true
	m.Apply(Config{Specs: []Spec{s, x}, ETag: "second"})
	if got := stateOf(m, "spec-x"); got != StateStopped {
		t.Fatalf("pinned spec-x is %q, want stopped behind the loading spec-s (the test's premise)", got)
	}

	openGateS()
	waitUntil(t, 5*time.Second, "pinned spec-x running once spec-s finished loading -- BUG: the wake after a finished start skips a pinned spec without a waiter, which then waits for an unrelated event", func() bool {
		return stateOf(m, "spec-x") == StateRunning
	})
	if got := stateOf(m, "spec-s"); got != StateStopped {
		t.Fatalf("force_running spec-s is %q, want stopped behind the pinned spec-x", got)
	}
}

// TestManagerStartWakeRunsAfterTheStartServesItsOwnWaiters: the wake runs
// after succeedPending, so a start that had a waiter is busy when the wake
// decides. A request queued for another spec then Waits for the release
// instead of evicting the process just handed to the waiter. Run first, the
// wake would drain spec-n in the same command and hand its waiter the
// endpoint of a process it has just signalled.
func TestManagerStartWakeRunsAfterTheStartServesItsOwnWaiters(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	admissions := admissionProbe(m)

	n, openGate := gatedSpec(t, "spec-n", "model-n")
	tgt := baseSpec("spec-t", "model-t")
	tgt.AdmissionWaitTimeoutSeconds = 0 // bounded by ctx only: nothing but the release may end the wait
	m.Apply(Config{Specs: []Spec{n, tgt}})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resN := ensureAsync(ctx, m, "model-n")
	waitUntil(t, 3*time.Second, "spec-n starting", func() bool {
		return stateOf(m, "spec-n") == StateStarting
	})
	drainAdmissions(admissions)
	resT := ensureAsync(ctx, m, "model-t")
	select {
	case <-admissions:
	case <-time.After(3 * time.Second):
		t.Fatal("the request for model-t was never admitted against the loading spec-n")
	}

	openGate()

	var rn queuedEnsure
	select {
	case rn = <-resN:
	case <-time.After(5 * time.Second):
		t.Fatal("EnsureRunning(model-n) did not return after spec-n finished loading")
	}
	if rn.err != nil {
		t.Fatalf("EnsureRunning(model-n) = %v", rn.err)
	}
	st := statusFor(m, "spec-n")
	if st == nil || st.State != StateRunning || st.InFlight != 1 {
		t.Fatalf("spec-n status = %+v right after it served its waiter, want running with one request in flight -- BUG: the wake ran before succeedPending and evicted the process it then handed out", st)
	}
	if code, _ := httpEcho(t, rn.endpoint, "ping"); code != 200 {
		t.Fatalf("POST %s/v1/echo = %d, want 200", rn.endpoint, code)
	}
	select {
	case r := <-resT:
		t.Fatalf("EnsureRunning(model-t) returned %v while spec-n served a request, want it queued until the release", r.err)
	default:
	}

	rn.release()
	select {
	case r := <-resT:
		if r.err != nil {
			t.Fatalf("EnsureRunning(model-t) after spec-n's release = %v", r.err)
		}
		r.release()
	case <-time.After(5 * time.Second):
		t.Fatal("EnsureRunning(model-t) did not return after spec-n's release")
	}
}

// firstQueuedResult waits until one of the queued requests in results returns
// and reports which one did. The specs in these tests share one slot, and the
// winner keeps it busy until the test releases it, so only one can return.
func firstQueuedResult(t *testing.T, results map[string]<-chan queuedEnsure, msg string) (string, queuedEnsure) {
	t.Helper()
	var name string
	var res queuedEnsure
	waitUntil(t, 5*time.Second, msg, func() bool {
		for n, ch := range results {
			select {
			case res = <-ch:
				name = n
				return true
			default:
			}
		}
		return false
	})
	return name, res
}

// TestManagerAdmissionWakeServesTheOldestQueuedRequestFirst: when one freed
// slot cannot take every queued request, the wake hands it to the request
// that has waited longest. Three requests for specs that may not run
// together, nor with spec-h, queue behind a busy spec-h: oldest for spec-c,
// newest for spec-a, the reverse of both the document's order and the spec
// IDs' order, so only the wake's sort by queue age gives the queue's order.
// Each release then frees the one slot, and the oldest request still queued
// must get it: its spec evicts the idle holder, and the holder's exit admits
// the specs it was drained for in the order the wake decided them.
func TestManagerAdmissionWakeServesTheOldestQueuedRequestFirst(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	admissions := admissionProbe(m)

	specs := []Spec{baseSpec("spec-h", "model-h")}
	for _, n := range []string{"a", "b", "c"} {
		s := baseSpec("spec-"+n, "model-"+n)
		s.AdmissionWaitTimeoutSeconds = 0 // bounded by ctx only: nothing but the wake may end the wait
		specs = append(specs, s)
	}
	m.Apply(Config{Specs: specs})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, release, err := m.EnsureRunning(ctx, "model-h")
	if err != nil {
		t.Fatalf("EnsureRunning(model-h) = %v", err)
	}

	// Oldest first. Each request is decided (Wait, behind the busy spec-h)
	// before the next one is sent, so their queue times are in this order.
	queueOrder := []string{"c", "b", "a"}
	results := make(map[string]<-chan queuedEnsure, len(queueOrder))
	for _, n := range queueOrder {
		drainAdmissions(admissions)
		results[n] = ensureAsync(ctx, m, "model-"+n)
		select {
		case <-admissions:
		case <-time.After(3 * time.Second):
			t.Fatalf("the request for model-%s was never admitted against the busy spec-h", n)
		}
	}
	for n, ch := range results {
		select {
		case r := <-ch:
			t.Fatalf("EnsureRunning(model-%s) returned %v while spec-h served a request, want it queued", n, r.err)
		default:
		}
	}

	holder := "spec-h"
	for _, want := range queueOrder {
		release()
		got, r := firstQueuedResult(t, results, "a queued request admitted after "+holder+"'s release")
		if got != want {
			t.Fatalf("after %s's release EnsureRunning(model-%s) returned first, want model-%s, the oldest request still queued -- BUG: the admission wake does not serve the oldest queued request first", holder, got, want)
		}
		if r.err != nil {
			t.Fatalf("EnsureRunning(model-%s) after %s's release = %v", got, holder, r.err)
		}
		delete(results, got)
		release, holder = r.release, "spec-"+got
	}
	release()
}

// TestManagerAdmissionWakeOrdersSpecsWithoutAWaiterLastBySpecID: in the
// wake's order a spec with a queued request comes before every spec without
// a waiter, and specs without a waiter follow in spec ID order, so the order
// never falls back on Go's randomized map iteration. Two pinned specs
// without a waiter, spec-a and spec-b, and a request for spec-c wait behind
// a busy spec-h, and none of them may run with another. spec-c's ID sorts
// last, so only its queued request's place ahead of the specs without a
// waiter gives it the slot spec-h's release frees. Its own release frees the
// slot again, and spec-a's ID gives it the slot ahead of spec-b. A pinned
// spec is never evicted, so spec-b stays stopped behind it.
func TestManagerAdmissionWakeOrdersSpecsWithoutAWaiterLastBySpecID(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	admissions := admissionProbe(m)

	h := baseSpec("spec-h", "model-h")
	a := baseSpec("spec-a", "model-a")
	b := baseSpec("spec-b", "model-b")
	c := baseSpec("spec-c", "model-c")
	c.AdmissionWaitTimeoutSeconds = 0 // bounded by ctx only: nothing but the wake may end the wait
	m.Apply(Config{Specs: []Spec{h, b, a, c}})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, releaseH, err := m.EnsureRunning(ctx, "model-h")
	if err != nil {
		t.Fatalf("EnsureRunning(model-h) = %v", err)
	}
	// Second document: spec-a and spec-b turn pinned while spec-h is busy.
	// Apply admits them, and they Wait behind it.
	a.Pinned, b.Pinned = true, true
	m.Apply(Config{Specs: []Spec{h, b, a, c}, ETag: "second"})
	for _, id := range []string{"spec-a", "spec-b"} {
		if got := stateOf(m, id); got != StateStopped {
			t.Fatalf("pinned %s is %q, want stopped behind the busy spec-h (the test's premise)", id, got)
		}
	}
	drainAdmissions(admissions)
	resC := ensureAsync(ctx, m, "model-c")
	select {
	case <-admissions:
	case <-time.After(3 * time.Second):
		t.Fatal("the request for model-c was never admitted against the busy spec-h")
	}

	releaseH()
	var rc queuedEnsure
	gotC := false
	waitUntil(t, 5*time.Second, "model-c's request or a pinned spec admitted after spec-h's release", func() bool {
		if !gotC {
			select {
			case rc = <-resC:
				gotC = true
			default:
			}
		}
		return gotC || stateOf(m, "spec-a") != StateStopped || stateOf(m, "spec-b") != StateStopped
	})
	if !gotC {
		t.Fatalf("spec-a is %q and spec-b %q after spec-h's release, want both stopped while model-c's request takes the slot -- BUG: the admission wake does not order specs without a waiter after queued requests", stateOf(m, "spec-a"), stateOf(m, "spec-b"))
	}
	if rc.err != nil {
		t.Fatalf("EnsureRunning(model-c) after spec-h's release = %v", rc.err)
	}
	for _, id := range []string{"spec-a", "spec-b"} {
		if got := stateOf(m, id); got != StateStopped {
			t.Fatalf("pinned %s is %q while spec-c serves a request, want stopped", id, got)
		}
	}

	rc.release()
	waitUntil(t, 5*time.Second, "a pinned spec running after spec-c's release", func() bool {
		return stateOf(m, "spec-a") == StateRunning || stateOf(m, "spec-b") == StateRunning
	})
	if got := stateOf(m, "spec-a"); got != StateRunning {
		t.Fatalf("spec-a is %q and spec-b %q after spec-c's release, want spec-a running: of two specs without a waiter, the lower spec ID comes first -- BUG: the admission wake leaves specs without a waiter in map order", got, stateOf(m, "spec-b"))
	}
	if got := stateOf(m, "spec-b"); got != StateStopped {
		t.Fatalf("pinned spec-b is %q, want stopped behind the pinned spec-a", got)
	}
}

// TestLessByQueueAge pins the admission wakes' order on the comparison
// itself, with no map iteration in the way: an older queue time first, a
// spec without a waiter (a zero queue time) after every spec with one, and
// the spec ID between equal queue times. Each pair is checked in both
// argument orders. In the first two cases the spec that must come first has
// the higher ID, so an order by ID alone would get them wrong.
func TestLessByQueueAge(t *testing.T) {
	t0 := time.Now()
	for _, tc := range []struct {
		name          string
		first, second wakeCandidate // first must sort before second
	}{
		{
			name:   "an older queue time first",
			first:  wakeCandidate{id: "spec-b", queuedAt: t0},
			second: wakeCandidate{id: "spec-a", queuedAt: t0.Add(time.Millisecond)},
		},
		{
			name:   "a queued request before a spec without a waiter",
			first:  wakeCandidate{id: "spec-b", queuedAt: t0},
			second: wakeCandidate{id: "spec-a"},
		},
		{
			name:   "equal queue times by spec ID",
			first:  wakeCandidate{id: "spec-a", queuedAt: t0},
			second: wakeCandidate{id: "spec-b", queuedAt: t0},
		},
		{
			name:   "specs without a waiter by spec ID",
			first:  wakeCandidate{id: "spec-a"},
			second: wakeCandidate{id: "spec-b"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !lessByQueueAge(tc.first, tc.second) {
				t.Errorf("lessByQueueAge(%s, %s) = false, want true: %s sorts first", tc.first.id, tc.second.id, tc.first.id)
			}
			if lessByQueueAge(tc.second, tc.first) {
				t.Errorf("lessByQueueAge(%s, %s) = true, want false: %s sorts first", tc.second.id, tc.first.id, tc.first.id)
			}
		})
	}
}
