// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/atgreen/dirq/internal/db"
	"github.com/jackc/pgx/v5"
)

// UpsertFact inserts or updates a fact for the given agent and module.
func (d *DB) UpsertFact(ctx context.Context, agentID, module string, data map[string]any) error {
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal fact data: %w", err)
	}

	_, err = d.pool.Exec(ctx, `
		INSERT INTO facts (agent_id, module, data, collected_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (agent_id, module) DO UPDATE
		SET data = EXCLUDED.data, collected_at = EXCLUDED.collected_at`,
		agentID, module, dataJSON,
	)
	return err
}

// GetFacts retrieves all facts for a given agent.
func (d *DB) GetFacts(ctx context.Context, agentID string) ([]db.Fact, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT agent_id, module, data, collected_at
		FROM facts WHERE agent_id = $1
		ORDER BY module`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return collectFacts(rows)
}

// GetFactsByModule retrieves all facts for a given module across all agents.
func (d *DB) GetFactsByModule(ctx context.Context, module string) ([]db.Fact, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT agent_id, module, data, collected_at
		FROM facts WHERE module = $1
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
	err := d.pool.QueryRow(ctx, `
		SELECT ttl_seconds FROM fact_ttl WHERE module = $1`, module).Scan(&ttl)
	if err == pgx.ErrNoRows {
		err = d.pool.QueryRow(ctx, `
			SELECT ttl_seconds FROM fact_ttl WHERE module = '_default'`).Scan(&ttl)
	}
	return ttl, err
}

func collectFacts(rows pgx.Rows) ([]db.Fact, error) {
	var facts []db.Fact
	for rows.Next() {
		var f db.Fact
		if err := rows.Scan(&f.AgentID, &f.Module, &f.Data, &f.CollectedAt); err != nil {
			return nil, err
		}
		facts = append(facts, f)
	}
	return facts, rows.Err()
}
