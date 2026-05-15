// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/atgreen/dirq/internal/db"
)

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
