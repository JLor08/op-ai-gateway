// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

// OpenPostgres opens a Postgres-backed SQLStore. It retries the initial connect
// with bounded backoff so a backend starting alongside its database tolerates a
// brief unavailability (belt-and-suspenders on top of compose service_healthy).
func OpenPostgres(ctx context.Context, dsn string) (*SQLStore, error) {
	if dsn == "" {
		return nil, fmt.Errorf("postgres dsn is required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres database: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)

	// Wait-for-db: retry ping with backoff until the context deadline (or ~30s).
	deadline := time.Now().Add(30 * time.Second)
	backoff := 250 * time.Millisecond
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = db.PingContext(pingCtx)
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			_ = db.Close()
			return nil, fmt.Errorf("ping postgres database: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = db.Close()
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 4*time.Second {
			backoff *= 2
		}
	}
	return &SQLStore{db: db, dl: postgresDialect{}}, nil
}
