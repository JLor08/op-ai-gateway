// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"op-ai-gateway/internal/routing"
	"time"
)

// UpsertCertificate inserts or replaces the row for cert.Domain (insert or
// on-conflict-update, keyed on the domain primary key). created_at is
// preserved on an update — the caller's CreatedAt only applies to a fresh
// insert (mirrors the SetPrincipalLimits on-conflict-upsert pattern, minus the
// insert-only column). The opaque KeySealed/Fingerprint values are stored
// verbatim — this layer never seals, unseals, or inspects them.
func (s *SQLStore) UpsertCertificate(ctx context.Context, cert routing.Certificate) error {
	_, err := s.exec(ctx, `
		insert into certificates (
			domain, kind, server_id, fullchain_pem, key_sealed, fingerprint, issuer_fingerprint,
			not_before, not_after, issued_at, status, last_error, attempt_count,
			next_attempt_at, created_at, updated_at
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		on conflict(domain) do update set
			kind = excluded.kind,
			server_id = excluded.server_id,
			fullchain_pem = excluded.fullchain_pem,
			key_sealed = excluded.key_sealed,
			fingerprint = excluded.fingerprint,
			issuer_fingerprint = excluded.issuer_fingerprint,
			not_before = excluded.not_before,
			not_after = excluded.not_after,
			issued_at = excluded.issued_at,
			status = excluded.status,
			last_error = excluded.last_error,
			attempt_count = excluded.attempt_count,
			next_attempt_at = excluded.next_attempt_at,
			updated_at = excluded.updated_at`,
		cert.Domain,
		cert.Kind,
		nullableString(cert.ServerID),
		cert.FullchainPEM,
		cert.KeySealed,
		cert.Fingerprint,
		cert.IssuerFingerprint,
		nullableTime(cert.NotBefore),
		nullableTime(cert.NotAfter),
		nullableTime(cert.IssuedAt),
		cert.Status,
		cert.LastError,
		cert.AttemptCount,
		nullableTime(cert.NextAttemptAt),
		cert.CreatedAt,
		cert.UpdatedAt,
	)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("upsert certificate: %w", err)
	}
	return nil
}

// certificateColumns is the shared select list for CertificateByDomain,
// CertificateByServer, and Certificates — its ordinal order MUST match
// scanCertificate's Scan targets exactly.
const certificateColumns = `domain, kind, server_id, fullchain_pem, key_sealed, fingerprint,
	issuer_fingerprint, not_before, not_after, issued_at, status, last_error, attempt_count,
	next_attempt_at, created_at, updated_at`

// CertificateByDomain returns storeerr.ErrNotFound when no row exists for
// domain.
func (s *SQLStore) CertificateByDomain(ctx context.Context, domain string) (routing.Certificate, error) {
	row := s.queryRow(ctx, `select `+certificateColumns+` from certificates where domain = ?`, domain)
	return scanCertificate(row)
}

// CertificateByServer returns storeerr.ErrNotFound when serverID is empty or
// no row is linked to it — the empty-string short-circuit avoids a query that
// would otherwise match a NULL server_id row via a wrong ANSI-NULL
// expectation (NULL never equals ”, so this is belt-and-suspenders, but it
// documents the intent and skips a pointless round trip). `order by domain
// limit 1` (review finding F1.4) makes the pick deterministic: there is no
// unique constraint on server_id, so with two rows linked to the same server
// the result would otherwise be dialect- and call-dependent (whichever row
// the engine happens to return first) -- the lowest domain always wins,
// matching MemoryStore's CertificateByServer.
func (s *SQLStore) CertificateByServer(ctx context.Context, serverID string) (routing.Certificate, error) {
	if serverID == "" {
		return routing.Certificate{}, ErrNotFound
	}
	row := s.queryRow(ctx, `select `+certificateColumns+` from certificates where server_id = ? order by domain limit 1`, serverID)
	return scanCertificate(row)
}

// Certificates lists every managed certificate, ordered by domain. Always
// non-nil, empty when none exist.
func (s *SQLStore) Certificates(ctx context.Context) ([]routing.Certificate, error) {
	rows, err := s.query(ctx, `select `+certificateColumns+` from certificates order by domain`)
	if err != nil {
		return nil, fmt.Errorf("list certificates: %w", err)
	}
	defer rows.Close()
	out := make([]routing.Certificate, 0)
	for rows.Next() {
		cert, err := scanCertificate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cert)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate certificates: %w", err)
	}
	return out, nil
}

// DeleteCertificate removes the row for domain, if any (a missing row is a
// benign no-op, mirroring the store's other idempotent-on-retry delete
// methods).
func (s *SQLStore) DeleteCertificate(ctx context.Context, domain string) error {
	if _, err := s.exec(ctx, `delete from certificates where domain = ?`, domain); err != nil {
		return fmt.Errorf("delete certificate: %w", err)
	}
	return nil
}

// scanCertificate reads one certificates row (via certificateColumns) into a
// routing.Certificate, translating sql.ErrNoRows to the shared ErrNotFound and
// NULL server_id/time columns back to their Go zero values.
func scanCertificate(row rowScanner) (routing.Certificate, error) {
	var cert routing.Certificate
	var serverID sql.NullString
	var notBefore, notAfter, issuedAt, nextAttempt sql.NullTime
	err := row.Scan(
		&cert.Domain, &cert.Kind, &serverID, &cert.FullchainPEM, &cert.KeySealed, &cert.Fingerprint,
		&cert.IssuerFingerprint, &notBefore, &notAfter, &issuedAt, &cert.Status, &cert.LastError, &cert.AttemptCount,
		&nextAttempt, &cert.CreatedAt, &cert.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.Certificate{}, ErrNotFound
	}
	if err != nil {
		return routing.Certificate{}, fmt.Errorf("scan certificate: %w", err)
	}
	cert.ServerID = serverID.String
	if notBefore.Valid {
		cert.NotBefore = notBefore.Time
	}
	if notAfter.Valid {
		cert.NotAfter = notAfter.Time
	}
	if issuedAt.Valid {
		cert.IssuedAt = issuedAt.Time
	}
	if nextAttempt.Valid {
		cert.NextAttemptAt = nextAttempt.Time
	}
	return cert, nil
}

// nullableString converts an empty Go string to a SQL NULL (for an optional
// text column, e.g. certificates.server_id) and any other value to itself, so
// the driver stores NULL rather than the empty string "".
func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// nullableTime converts a zero time.Time to a SQL NULL (for an optional
// timestamp column, e.g. a skipped/errored certificate's not_before/not_after)
// and any other value to itself.
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
