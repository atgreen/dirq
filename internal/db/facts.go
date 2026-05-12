package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// UpsertFact inserts or updates a fact for the given agent and module.
func (db *DB) UpsertFact(ctx context.Context, agentID, module string, data map[string]any) error {
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal fact data: %w", err)
	}

	_, err = db.pool.Exec(ctx, `
		INSERT INTO facts (agent_id, module, data, collected_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (agent_id, module) DO UPDATE
		SET data = EXCLUDED.data, collected_at = EXCLUDED.collected_at`,
		agentID, module, dataJSON,
	)
	return err
}

// GetFacts retrieves all facts for a given agent.
func (db *DB) GetFacts(ctx context.Context, agentID string) ([]Fact, error) {
	rows, err := db.pool.Query(ctx, `
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
func (db *DB) GetFactsByModule(ctx context.Context, module string) ([]Fact, error) {
	rows, err := db.pool.Query(ctx, `
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
// Falls back to the _default TTL if the module has no specific entry.
func (db *DB) GetFactTTL(ctx context.Context, module string) (int, error) {
	var ttl int
	err := db.pool.QueryRow(ctx, `
		SELECT ttl_seconds FROM fact_ttl WHERE module = $1`, module).Scan(&ttl)
	if err == pgx.ErrNoRows {
		err = db.pool.QueryRow(ctx, `
			SELECT ttl_seconds FROM fact_ttl WHERE module = '_default'`).Scan(&ttl)
	}
	return ttl, err
}

func collectFacts(rows pgx.Rows) ([]Fact, error) {
	var facts []Fact
	for rows.Next() {
		var f Fact
		if err := rows.Scan(&f.AgentID, &f.Module, &f.Data, &f.CollectedAt); err != nil {
			return nil, err
		}
		facts = append(facts, f)
	}
	return facts, rows.Err()
}
