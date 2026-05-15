// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package postgres

import (
	"context"
	"time"

	"github.com/atgreen/dirq/internal/db"
)

// CreateExecLog inserts a new execution log entry and returns it with the generated ID.
func (d *DB) CreateExecLog(ctx context.Context, log db.ExecLog) (db.ExecLog, error) {
	row := d.pool.QueryRow(ctx, `
		INSERT INTO exec_log (request_id, agent_id, hostname, operation, command, dest_path, src_path,
		                      become, become_user, rc, success, error, aap_job_id, aap_job_template,
		                      aap_user, started_at, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		RETURNING id, request_id, agent_id, hostname, operation, command, dest_path, src_path,
		          become, become_user, rc, success, error, aap_job_id, aap_job_template,
		          aap_user, started_at, finished_at, created_at`,
		log.RequestID, log.AgentID, log.Hostname, log.Operation,
		log.Command, log.DestPath, log.SrcPath,
		log.Become, log.BecomeUser,
		log.RC, log.Success, log.Error,
		log.AAPJobID, log.AAPJobTemplate, log.AAPUser,
		log.StartedAt, log.FinishedAt,
	)
	return scanExecLog(row)
}

// UpdateExecLog updates the result fields of an execution log entry.
func (d *DB) UpdateExecLog(ctx context.Context, id string, rc *int, success bool, errMsg string, finishedAt time.Time) error {
	var errPtr *string
	if errMsg != "" {
		errPtr = &errMsg
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE exec_log SET rc = $2, success = $3, error = $4, finished_at = $5
		WHERE request_id = $1`,
		id, rc, success, errPtr, finishedAt,
	)
	return err
}

// ListExecLogs returns the most recent execution log entries.
func (d *DB) ListExecLogs(ctx context.Context, limit int) ([]db.ExecLog, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, request_id, agent_id, hostname, operation, command, dest_path, src_path,
		       become, become_user, rc, success, error, aap_job_id, aap_job_template,
		       aap_user, started_at, finished_at, created_at
		FROM exec_log ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectExecLogs(rows)
}

// ListExecLogsByAgent returns execution log entries for a specific agent.
func (d *DB) ListExecLogsByAgent(ctx context.Context, agentID string, limit int) ([]db.ExecLog, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, request_id, agent_id, hostname, operation, command, dest_path, src_path,
		       become, become_user, rc, success, error, aap_job_id, aap_job_template,
		       aap_user, started_at, finished_at, created_at
		FROM exec_log WHERE agent_id = $1 ORDER BY created_at DESC LIMIT $2`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectExecLogs(rows)
}

// ListExecLogsByJob returns execution log entries for a specific AAP job.
func (d *DB) ListExecLogsByJob(ctx context.Context, aapJobID string) ([]db.ExecLog, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, request_id, agent_id, hostname, operation, command, dest_path, src_path,
		       become, become_user, rc, success, error, aap_job_id, aap_job_template,
		       aap_user, started_at, finished_at, created_at
		FROM exec_log WHERE aap_job_id = $1 ORDER BY created_at DESC`, aapJobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectExecLogs(rows)
}

// scanExecLog scans a single exec_log row.
func scanExecLog(row interface{ Scan(dest ...any) error }) (db.ExecLog, error) {
	var l db.ExecLog
	err := row.Scan(
		&l.ID, &l.RequestID, &l.AgentID, &l.Hostname, &l.Operation,
		&l.Command, &l.DestPath, &l.SrcPath,
		&l.Become, &l.BecomeUser,
		&l.RC, &l.Success, &l.Error,
		&l.AAPJobID, &l.AAPJobTemplate, &l.AAPUser,
		&l.StartedAt, &l.FinishedAt, &l.CreatedAt,
	)
	if err != nil {
		return db.ExecLog{}, err
	}
	return l, nil
}

// collectExecLogs iterates pgx.Rows and collects ExecLog entries.
func collectExecLogs(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]db.ExecLog, error) {
	var logs []db.ExecLog
	for rows.Next() {
		var l db.ExecLog
		err := rows.Scan(
			&l.ID, &l.RequestID, &l.AgentID, &l.Hostname, &l.Operation,
			&l.Command, &l.DestPath, &l.SrcPath,
			&l.Become, &l.BecomeUser,
			&l.RC, &l.Success, &l.Error,
			&l.AAPJobID, &l.AAPJobTemplate, &l.AAPUser,
			&l.StartedAt, &l.FinishedAt, &l.CreatedAt,
		)
		if err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}
