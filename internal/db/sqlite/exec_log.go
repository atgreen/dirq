// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/atgreen/dirq/internal/db"
)

// CreateExecLog inserts a new execution log entry and returns it with the generated ID.
func (d *DB) CreateExecLog(ctx context.Context, log db.ExecLog) (db.ExecLog, error) {
	id := generateUUID()
	now := time.Now().UTC().Format(time.RFC3339)

	var startedAt, finishedAt *string
	if log.StartedAt != nil {
		s := log.StartedAt.UTC().Format(time.RFC3339)
		startedAt = &s
	}
	if log.FinishedAt != nil {
		s := log.FinishedAt.UTC().Format(time.RFC3339)
		finishedAt = &s
	}

	var become int
	if log.Become {
		become = 1
	}

	var success *int
	if log.Success != nil {
		v := 0
		if *log.Success {
			v = 1
		}
		success = &v
	}

	_, err := d.db.ExecContext(ctx, `
		INSERT INTO exec_log (id, request_id, agent_id, hostname, operation, command, dest_path, src_path,
		                      become, become_user, rc, success, error, aap_job_id, aap_job_template,
		                      aap_user, started_at, finished_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, log.RequestID, log.AgentID, log.Hostname, log.Operation,
		log.Command, log.DestPath, log.SrcPath,
		become, log.BecomeUser,
		log.RC, success, log.Error,
		log.AAPJobID, log.AAPJobTemplate, log.AAPUser,
		startedAt, finishedAt, now,
	)
	if err != nil {
		return db.ExecLog{}, err
	}

	// Read back the created record.
	row := d.db.QueryRowContext(ctx, `
		SELECT id, request_id, agent_id, hostname, operation, command, dest_path, src_path,
		       become, become_user, rc, success, error, aap_job_id, aap_job_template,
		       aap_user, started_at, finished_at, created_at
		FROM exec_log WHERE id = ?`, id)
	return scanExecLog(row)
}

// UpdateExecLog updates the result fields of an execution log entry.
func (d *DB) UpdateExecLog(ctx context.Context, id string, rc *int, success bool, errMsg string, finishedAt time.Time) error {
	var errPtr *string
	if errMsg != "" {
		errPtr = &errMsg
	}
	successInt := 0
	if success {
		successInt = 1
	}
	finishedAtStr := finishedAt.UTC().Format(time.RFC3339)
	_, err := d.db.ExecContext(ctx, `
		UPDATE exec_log SET rc = ?, success = ?, error = ?, finished_at = ?
		WHERE request_id = ?`,
		rc, successInt, errPtr, finishedAtStr, id,
	)
	return err
}

// ListExecLogs returns the most recent execution log entries.
func (d *DB) ListExecLogs(ctx context.Context, limit int) ([]db.ExecLog, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, request_id, agent_id, hostname, operation, command, dest_path, src_path,
		       become, become_user, rc, success, error, aap_job_id, aap_job_template,
		       aap_user, started_at, finished_at, created_at
		FROM exec_log ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectExecLogs(rows)
}

// ListExecLogsByAgent returns execution log entries for a specific agent.
func (d *DB) ListExecLogsByAgent(ctx context.Context, agentID string, limit int) ([]db.ExecLog, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, request_id, agent_id, hostname, operation, command, dest_path, src_path,
		       become, become_user, rc, success, error, aap_job_id, aap_job_template,
		       aap_user, started_at, finished_at, created_at
		FROM exec_log WHERE agent_id = ? ORDER BY created_at DESC LIMIT ?`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectExecLogs(rows)
}

// ListExecLogsByJob returns execution log entries for a specific AAP job.
func (d *DB) ListExecLogsByJob(ctx context.Context, aapJobID string) ([]db.ExecLog, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, request_id, agent_id, hostname, operation, command, dest_path, src_path,
		       become, become_user, rc, success, error, aap_job_id, aap_job_template,
		       aap_user, started_at, finished_at, created_at
		FROM exec_log WHERE aap_job_id = ? ORDER BY created_at DESC`, aapJobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectExecLogs(rows)
}

// scanExecLog scans a single exec_log row from *sql.Row.
func scanExecLog(row *sql.Row) (db.ExecLog, error) {
	var l db.ExecLog
	var become int
	var success sql.NullInt64
	var startedAt, finishedAt, createdAt sql.NullString
	var command, destPath, srcPath, becomeUser, errStr sql.NullString
	var aapJobID, aapJobTemplate, aapUser sql.NullString
	var rc sql.NullInt64

	err := row.Scan(
		&l.ID, &l.RequestID, &l.AgentID, &l.Hostname, &l.Operation,
		&command, &destPath, &srcPath,
		&become, &becomeUser,
		&rc, &success, &errStr,
		&aapJobID, &aapJobTemplate, &aapUser,
		&startedAt, &finishedAt, &createdAt,
	)
	if err != nil {
		return db.ExecLog{}, err
	}

	l.Become = become != 0
	if command.Valid {
		l.Command = &command.String
	}
	if destPath.Valid {
		l.DestPath = &destPath.String
	}
	if srcPath.Valid {
		l.SrcPath = &srcPath.String
	}
	if becomeUser.Valid {
		l.BecomeUser = &becomeUser.String
	}
	if rc.Valid {
		v := int(rc.Int64)
		l.RC = &v
	}
	if success.Valid {
		v := success.Int64 != 0
		l.Success = &v
	}
	if errStr.Valid {
		l.Error = &errStr.String
	}
	if aapJobID.Valid {
		l.AAPJobID = &aapJobID.String
	}
	if aapJobTemplate.Valid {
		l.AAPJobTemplate = &aapJobTemplate.String
	}
	if aapUser.Valid {
		l.AAPUser = &aapUser.String
	}
	if startedAt.Valid {
		t, _ := time.Parse(time.RFC3339, startedAt.String)
		l.StartedAt = &t
	}
	if finishedAt.Valid {
		t, _ := time.Parse(time.RFC3339, finishedAt.String)
		l.FinishedAt = &t
	}
	if createdAt.Valid {
		l.CreatedAt, _ = time.Parse(time.RFC3339, createdAt.String)
	}

	return l, nil
}

// collectExecLogs iterates sql.Rows and collects ExecLog entries.
func collectExecLogs(rows *sql.Rows) ([]db.ExecLog, error) {
	var logs []db.ExecLog
	for rows.Next() {
		var l db.ExecLog
		var become int
		var success sql.NullInt64
		var startedAt, finishedAt, createdAt sql.NullString
		var command, destPath, srcPath, becomeUser, errStr sql.NullString
		var aapJobID, aapJobTemplate, aapUser sql.NullString
		var rc sql.NullInt64

		err := rows.Scan(
			&l.ID, &l.RequestID, &l.AgentID, &l.Hostname, &l.Operation,
			&command, &destPath, &srcPath,
			&become, &becomeUser,
			&rc, &success, &errStr,
			&aapJobID, &aapJobTemplate, &aapUser,
			&startedAt, &finishedAt, &createdAt,
		)
		if err != nil {
			return nil, err
		}

		l.Become = become != 0
		if command.Valid {
			l.Command = &command.String
		}
		if destPath.Valid {
			l.DestPath = &destPath.String
		}
		if srcPath.Valid {
			l.SrcPath = &srcPath.String
		}
		if becomeUser.Valid {
			l.BecomeUser = &becomeUser.String
		}
		if rc.Valid {
			v := int(rc.Int64)
			l.RC = &v
		}
		if success.Valid {
			v := success.Int64 != 0
			l.Success = &v
		}
		if errStr.Valid {
			l.Error = &errStr.String
		}
		if aapJobID.Valid {
			l.AAPJobID = &aapJobID.String
		}
		if aapJobTemplate.Valid {
			l.AAPJobTemplate = &aapJobTemplate.String
		}
		if aapUser.Valid {
			l.AAPUser = &aapUser.String
		}
		if startedAt.Valid {
			t, _ := time.Parse(time.RFC3339, startedAt.String)
			l.StartedAt = &t
		}
		if finishedAt.Valid {
			t, _ := time.Parse(time.RFC3339, finishedAt.String)
			l.FinishedAt = &t
		}
		if createdAt.Valid {
			l.CreatedAt, _ = time.Parse(time.RFC3339, createdAt.String)
		}

		logs = append(logs, l)
	}
	return logs, rows.Err()
}
