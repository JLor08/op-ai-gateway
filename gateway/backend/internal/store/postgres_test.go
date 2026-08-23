// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestOpenPostgresPingsAndMigrates(t *testing.T) {
	dsn := os.Getenv("OP_AI_GATEWAY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OP_AI_GATEWAY_TEST_POSTGRES_DSN to run postgres tests")
	}
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
