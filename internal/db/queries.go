package db

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// CreateQuery records a new query and returns the created record.
func (db *DB) CreateQuery(ctx context.Context, rawQuery, submittedBy string, targetCount int) (Query, error) {
	var q Query
	var sub *string
	if submittedBy != "" {
		sub = &submittedBy
	}

	err := db.pool.QueryRow(ctx, `
		INSERT INTO queries (raw_query, submitted_by, target_count)
		VALUES ($1, $2, $3)
		RETURNING id, raw_query, submitted_by, submitted_at, completed_at,
		          status, target_count, success_count, error_count, timeout_count`,
		rawQuery, sub, targetCount,
	).Scan(
		&q.ID, &q.RawQuery, &q.SubmittedBy, &q.SubmittedAt, &q.CompletedAt,
		&q.Status, &q.TargetCount, &q.SuccessCount, &q.ErrorCount, &q.TimeoutCount,
	)
	return q, err
}

// UpdateQueryStatus updates a query's status and counters, setting completed_at to now.
func (db *DB) UpdateQueryStatus(ctx context.Context, id, status string, successCount, errorCount, timeoutCount int) error {
	tag, err := db.pool.Exec(ctx, `
		UPDATE queries
		SET status = $1, success_count = $2, error_count = $3, timeout_count = $4, completed_at = now()
		WHERE id = $5`,
		status, successCount, errorCount, timeoutCount, id,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// ListQueries returns the most recent queries up to the given limit.
func (db *DB) ListQueries(ctx context.Context, limit int) ([]Query, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, raw_query, submitted_by, submitted_at, completed_at,
		       status, target_count, success_count, error_count, timeout_count
		FROM queries
		ORDER BY submitted_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var queries []Query
	for rows.Next() {
		var q Query
		if err := rows.Scan(
			&q.ID, &q.RawQuery, &q.SubmittedBy, &q.SubmittedAt, &q.CompletedAt,
			&q.Status, &q.TargetCount, &q.SuccessCount, &q.ErrorCount, &q.TimeoutCount,
		); err != nil {
			return nil, err
		}
		queries = append(queries, q)
	}
	return queries, rows.Err()
}
