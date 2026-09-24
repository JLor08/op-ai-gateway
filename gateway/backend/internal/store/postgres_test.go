// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"testing"
	"time"
)

func TestOpenPostgresPingsAndMigrates(t *testing.T) {
	dsn := testPostgresDSN(t)
	// This test already bounded itself; testPostgresDSN adds the lock_timeout
	// the shared helper carries.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.dl.name() != "postgres" {
		t.Fatalf("wrong dialect")
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}
