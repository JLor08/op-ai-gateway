// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"
)

// Shared entry point for every PostgreSQL-backed test in this package.
//
// What prompted it: a `Go (backend + agent)` run failed with `panic: test
// timed out after 10m0s` / `FAIL op-ai-gateway/internal/store 600.007s` while
// the SAME commit passed in a second run with `ok … 150.997s`.
//
// That run turned out NOT to be a hang, and the distinction matters enough to
// record so the next reader does not repeat the wrong diagnosis. Go's timeout
// panic names the test that is running and prints how long it has been
// running; a test parked in an unbounded wait is named with the full elapsed
// time (verified: a deliberately blocked subtest under `-timeout 30s` is
// reported as `(30s)`). CI reported `(6s)` and `(1s)`. So the named subtest
// was a healthy one-second-old query, and the package had simply spent its
// budget on everything before it — the same run's disk-heavy packages were
// ~4x slower while CPU-bound ones were unaffected, which is a degraded runner,
// not a lock. The package timeout is raised in CI for that (.github/workflows/
// ci.yml); it is the deadline, not a hang, exactly as
// development-and-quality.md already says about the race leg.
//
// What this file fixes is the LATENT fragility that investigation exposed
// rather than the run that exposed it. Nothing bounded a lock wait anywhere:
// not the pool (postgres.go sets only pool knobs), not the CI DSN, not the
// setup context (`context.Background()`). A conflicting lock therefore waits
// forever — reproduced deterministically by holding an ACCESS SHARE lock on
// `users` in another session, which reproduces the CI stack line for line
// through dropAllTables. With 138 postgres subtests per run, each requesting
// an ACCESS EXCLUSIVE lock on the whole public schema, that is a real exposure
// even though it is not what happened here.
//
// Two bounds, chosen so that WHICH one fires is itself the diagnosis:
//
//   - lock_timeout fails a wait for a conflicting lock as
//     `ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)`. It can
//     fire only while blocked behind another session's lock, which is never
//     legitimate here.
//   - a setup context deadline bounds everything else — a wedged server, a
//     stalled socket — as `context deadline exceeded`.
//
// Neither is a statement_timeout: migrations legitimately run long statements,
// and capping those would trade a rare hang for a recurring false failure.
const (
	// Far above any legitimate wait (there is no concurrent writer — the
	// package's tests are serialized and hold the schema one at a time), and
	// far below the 10-minute package alarm.
	testPostgresLockTimeout = 15 * time.Second

	// Bounds open + clean slate + migrate only, never the test body, which is
	// free to take as long as it takes. Setup is well under a second locally;
	// 90s leaves ~100x headroom so an ordinarily slow CI runner cannot trip
	// it.
	testPostgresSetupTimeout = 90 * time.Second
)

// testPostgresDSN returns the DSN for this package's PostgreSQL leg, skipping
// the calling subtest when the env var is unset — the documented convention
// (persistence.md), so a developer without a database still gets a green
// sqlite run. The returned DSN carries lock_timeout.
func testPostgresDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("OP_AI_GATEWAY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OP_AI_GATEWAY_TEST_POSTGRES_DSN to run postgres conformance tests")
	}
	return withTestLockTimeout(dsn)
}

// withTestLockTimeout adds lock_timeout to a URL-form DSN as a libpq `options`
// parameter, which applies to every connection the pool opens rather than to
// one session the way a bare `SET` would.
//
// A DSN that is not URL-form, or that already carries `options`, is returned
// untouched: overwriting a caller's explicit backend settings would be worse
// than leaving the bound off, and the setup deadline still applies either way.
func withTestLockTimeout(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return dsn
	}
	q := u.Query()
	if q.Has("options") {
		return dsn
	}
	// A bare number is milliseconds, which is the unit lock_timeout takes.
	q.Set("options", "-c lock_timeout="+strconv.Itoa(int(testPostgresLockTimeout/time.Millisecond)))
	u.RawQuery = q.Encode()
	return u.String()
}

// postgresSetupContext bounds the open/clean-slate/migrate phase of a
// PostgreSQL subtest. The test body must NOT use it.
func postgresSetupContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testPostgresSetupTimeout)
	t.Cleanup(cancel)
	return ctx
}
