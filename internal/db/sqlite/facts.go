// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/atgreen/dirq/internal/db"
)

// sqliteFactBulkChunk caps the rows per bulk INSERT to stay under
// SQLite's default 999-parameter ceiling (4 params × 200 = 800).
const sqliteFactBulkChunk = 200

// UpsertFact inserts or updates a fact for the given agent and module.
func (d *DB) UpsertFact(ctx context.Context, agentID, module string, data map[string]any) error {
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal fact data: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = d.db.ExecContext(ctx, `
		INSERT INTO facts (agent_id, module, data, collected_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (agent_id, module) DO UPDATE
		SET data = excluded.data, collected_at = excluded.collected_at`,
		agentID, module, string(dataJSON), now,
	)
	return err
}

// BulkUpsertFacts writes rows in chunked multi-row INSERTs inside a
// single transaction, so SQLite acquires the writer lock once per flush
// instead of once per row.
func (d *DB) BulkUpsertFacts(ctx context.Context, rows []db.FactRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin fact bulk tx: %w", err)
	}
	defer tx.Rollback()

	for start := 0; start < len(rows); start += sqliteFactBulkChunk {
		end := start + sqliteFactBulkChunk
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)*4)
		for i, r := range chunk {
			placeholders[i] = "(?, ?, ?, ?)"
			args = append(args, r.AgentID, r.Module, string(r.Data),
				r.CollectedAt.UTC().Format(time.RFC3339))
		}
		stmt := `INSERT INTO facts (agent_id, module, data, collected_at) VALUES ` +
			strings.Join(placeholders, ", ") +
			` ON CONFLICT (agent_id, module) DO UPDATE
			   SET data = excluded.data, collected_at = excluded.collected_at`
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			return fmt.Errorf("bulk fact upsert chunk: %w", err)
		}
	}
	return tx.Commit()
}

// GetFacts retrieves all facts for a given agent.
func (d *DB) GetFacts(ctx context.Context, agentID string) ([]db.Fact, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT agent_id, module, data, collected_at
		FROM facts WHERE agent_id = ?
		ORDER BY module`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return collectFacts(rows)
}

// GetAllFacts retrieves all facts for all agents in a single query.
func (d *DB) GetAllFacts(ctx context.Context) ([]db.Fact, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT agent_id, module, data, collected_at
		FROM facts
		ORDER BY agent_id, module`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return collectFacts(rows)
}

// GetFactsByModule retrieves all facts for a given module across all agents.
func (d *DB) GetFactsByModule(ctx context.Context, module string) ([]db.Fact, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT agent_id, module, data, collected_at
		FROM facts WHERE module = ?
		ORDER BY agent_id`, module)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return collectFacts(rows)
}

// GetFactTTL returns the TTL in seconds for a given module.
func (d *DB) GetFactTTL(ctx context.Context, module string) (int, error) {
	var ttl int
	err := d.db.QueryRowContext(ctx, `
		SELECT ttl_seconds FROM fact_ttl WHERE module = ?`, module).Scan(&ttl)
	if err == sql.ErrNoRows {
		err = d.db.QueryRowContext(ctx, `
			SELECT ttl_seconds FROM fact_ttl WHERE module = '_default'`).Scan(&ttl)
	}
	return ttl, err
}

func collectFacts(rows *sql.Rows) ([]db.Fact, error) {
	var facts []db.Fact
	for rows.Next() {
		var f db.Fact
		var dataStr string
		var collectedAt string
		if err := rows.Scan(&f.AgentID, &f.Module, &dataStr, &collectedAt); err != nil {
			return nil, err
		}
		f.Data = json.RawMessage(dataStr)
		f.CollectedAt, _ = time.Parse(time.RFC3339, collectedAt)
		facts = append(facts, f)
	}
	return facts, rows.Err()
}
