// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/atgreen/dirq/internal/db"
)

// CreateQuery records a new query and returns the created record.
func (d *DB) CreateQuery(ctx context.Context, rawQuery, submittedBy string, targetCount int) (db.Query, error) {
	id := generateUUID()
	now := time.Now().UTC().Format(time.RFC3339)

	var sub *string
	if submittedBy != "" {
		sub = &submittedBy
	}

	_, err := d.db.ExecContext(ctx, `
		INSERT INTO queries (id, raw_query, submitted_by, submitted_at, target_count)
		VALUES (?, ?, ?, ?, ?)`,
		id, rawQuery, sub, now, targetCount,
	)
	if err != nil {
		return db.Query{}, err
	}

	// Read back the created record.
	row := d.db.QueryRowContext(ctx, `
		SELECT id, raw_query, submitted_by, submitted_at, completed_at,
		       status, target_count, success_count, error_count, timeout_count
		FROM queries WHERE id = ?`, id)
	return scanQuery(row)
}

// UpdateQueryStatus updates a query's status and counters, setting completed_at to now.
func (d *DB) UpdateQueryStatus(ctx context.Context, id, status string, successCount, errorCount, timeoutCount int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := d.db.ExecContext(ctx, `
		UPDATE queries
		SET status = ?, success_count = ?, error_count = ?, timeout_count = ?, completed_at = ?
		WHERE id = ?`,
		status, successCount, errorCount, timeoutCount, now, id,
	)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListQueries returns the most recent queries up to the given limit.
func (d *DB) ListQueries(ctx context.Context, limit int) ([]db.Query, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, raw_query, submitted_by, submitted_at, completed_at,
		       status, target_count, success_count, error_count, timeout_count
		FROM queries
		ORDER BY submitted_at DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var queries []db.Query
	for rows.Next() {
		var q db.Query
		var submittedAt string
		var completedAt sql.NullString
		if err := rows.Scan(
			&q.ID, &q.RawQuery, &q.SubmittedBy, &submittedAt, &completedAt,
			&q.Status, &q.TargetCount, &q.SuccessCount, &q.ErrorCount, &q.TimeoutCount,
		); err != nil {
			return nil, err
		}
		q.SubmittedAt, _ = time.Parse(time.RFC3339, submittedAt)
		if completedAt.Valid {
			t, _ := time.Parse(time.RFC3339, completedAt.String)
			q.CompletedAt = &t
		}
		queries = append(queries, q)
	}
	return queries, rows.Err()
}

// scanQuery scans a single query row.
func scanQuery(row *sql.Row) (db.Query, error) {
	var q db.Query
	var submittedAt string
	var completedAt sql.NullString
	err := row.Scan(
		&q.ID, &q.RawQuery, &q.SubmittedBy, &submittedAt, &completedAt,
		&q.Status, &q.TargetCount, &q.SuccessCount, &q.ErrorCount, &q.TimeoutCount,
	)
	if err != nil {
		return db.Query{}, err
	}
	q.SubmittedAt, _ = time.Parse(time.RFC3339, submittedAt)
	if completedAt.Valid {
		t, _ := time.Parse(time.RFC3339, completedAt.String)
		q.CompletedAt = &t
	}
	return q, nil
}
