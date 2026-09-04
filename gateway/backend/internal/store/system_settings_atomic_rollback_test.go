// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// This file proves the ONE property SetSystemSettings exists for: a failure
// PART WAY through the batch leaves NONE of it applied. A cancelled-context test
// cannot show this (it fails at BeginTx, before any write), so we drive a real
// SQLStore over a fault-injecting sqlite driver that lets the first upsert commit
// inside the transaction and then fails the second -- a naive per-key loop would
// leave the first key written; the transactional batch must roll it back.

var (
	execFaultArmed  bool
	execFaultSeen   int
	execFaultFailAt int
)

func armExecFault(failAt int) { execFaultArmed, execFaultSeen, execFaultFailAt = true, 0, failAt }
func disarmExecFault()        { execFaultArmed = false }

type faultDriver struct{ inner driver.Driver }

func (d faultDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &faultConn{Conn: c}, nil
}

// faultConn wraps the real sqlite conn and, once armed, fails the Nth ExecContext
// (the write path a batch upsert takes). Every other capability delegates to the
// embedded conn so migrations, reads and transaction begin/commit are unaffected.
type faultConn struct{ driver.Conn }

func (c *faultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if execFaultArmed {
		execFaultSeen++
		if execFaultSeen == execFaultFailAt {
			return nil, errors.New("injected exec failure")
		}
	}
	if ec, ok := c.Conn.(driver.ExecerContext); ok {
		return ec.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if qc, ok := c.Conn.(driver.QueryerContext); ok {
		return qc.QueryContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *faultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if bt, ok := c.Conn.(driver.ConnBeginTx); ok {
		return bt.BeginTx(ctx, opts)
	}
	return c.Conn.Begin() //nolint:staticcheck // fallback only when the driver lacks ConnBeginTx
}

func init() {
	// Grab the modernc "sqlite" driver.Driver by opening a throwaway handle, then
	// register a fault-injecting wrapper around it under a distinct name.
	base, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		panic("open sqlite to capture driver: " + err.Error())
	}
	inner := base.Driver()
	_ = base.Close()
	sql.Register("sqlite-execfault", faultDriver{inner: inner})
}

func TestSetSystemSettingsRollsBackPartialWrite(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite-execfault", sqliteDSN(filepath.Join(t.TempDir(), "fault.db")))
	if err != nil {
		t.Fatalf("open fault db: %v", err)
	}
	// One connection so the whole test hits the single fault-injecting conn and
	// the global counter is deterministic.
	db.SetMaxOpenConns(1)
	st := &SQLStore{db: db, dl: sqliteDialect{}}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)

	// Baseline (written before arming) that must survive the failed batch.
	if err := st.SetSystemSetting(ctx, "cert_issuer_mode", "acme", now); err != nil {
		t.Fatalf("seed baseline: %v", err)
	}

	// Fail the SECOND upsert of the next batch: the first has already executed
	// inside the transaction, so only a real rollback can undo it.
	armExecFault(2)
	err = st.SetSystemSettings(ctx, map[string]string{
		"cert_enabled":     "true",
		"cert_issuer_mode": "self_signed",
		"cert_base_domain": "int.example.test",
	}, now)
	disarmExecFault()
	if err == nil {
		t.Fatalf("SetSystemSettings returned nil, want the injected failure")
	}

	got, err := st.SystemSettings(ctx)
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if _, ok := got["cert_enabled"]; ok {
		t.Fatalf("cert_enabled survived a failed batch -- the partial write was NOT rolled back: %#v", got)
	}
	if _, ok := got["cert_base_domain"]; ok {
		t.Fatalf("cert_base_domain survived a failed batch -- the partial write was NOT rolled back: %#v", got)
	}
	if got["cert_issuer_mode"] != "acme" {
		t.Fatalf("cert_issuer_mode = %q, want the untouched baseline %q (the batch's overwrite must have rolled back)", got["cert_issuer_mode"], "acme")
	}
}
