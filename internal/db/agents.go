// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// RegisterAgent inserts a new agent and returns the created record.
func (db *DB) RegisterAgent(ctx context.Context, p RegisterAgentParams) (Agent, error) {
	tagsJSON, err := json.Marshal(p.Tags)
	if err != nil {
		return Agent{}, fmt.Errorf("marshal tags: %w", err)
	}

	caps := p.Capabilities
	if caps == nil {
		caps = []string{}
	}

	row := db.pool.QueryRow(ctx, `
		INSERT INTO agents (hostname, os, os_version, arch, agent_version, listen_addr, capabilities, tags, exec_enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (hostname) DO UPDATE SET
			os = EXCLUDED.os,
			os_version = EXCLUDED.os_version,
			arch = EXCLUDED.arch,
			agent_version = EXCLUDED.agent_version,
			listen_addr = EXCLUDED.listen_addr,
			capabilities = EXCLUDED.capabilities,
			tags = EXCLUDED.tags,
			exec_enabled = EXCLUDED.exec_enabled,
			online = true,
			last_seen_at = now()
		RETURNING id, hostname, os, os_version, arch, agent_version, listen_addr, role,
		          capabilities, tags, parent_id, server_pod, online, exec_enabled, registered_at, last_seen_at`,
		p.Hostname, p.OS, p.OSVersion, p.Arch, p.AgentVersion, p.ListenAddr, caps, tagsJSON, p.ExecEnabled,
	)

	return scanAgent(row)
}

// GetAgent retrieves an agent by ID.
func (db *DB) GetAgent(ctx context.Context, id string) (Agent, error) {
	row := db.pool.QueryRow(ctx, `
		SELECT id, hostname, os, os_version, arch, agent_version, listen_addr, role,
		       capabilities, tags, parent_id, server_pod, online, exec_enabled, registered_at, last_seen_at
		FROM agents WHERE id = $1`, id)
	return scanAgent(row)
}

// GetAgentByHostname retrieves an agent by hostname.
func (db *DB) GetAgentByHostname(ctx context.Context, hostname string) (Agent, error) {
	row := db.pool.QueryRow(ctx, `
		SELECT id, hostname, os, os_version, arch, agent_version, listen_addr, role,
		       capabilities, tags, parent_id, server_pod, online, exec_enabled, registered_at, last_seen_at
		FROM agents WHERE hostname = $1`, hostname)
	return scanAgent(row)
}

// ListAgents returns agents matching the given filter.
func (db *DB) ListAgents(ctx context.Context, f ListAgentsFilter) ([]Agent, error) {
	var conditions []string
	var args []any
	argIdx := 1

	if f.Online != nil {
		conditions = append(conditions, fmt.Sprintf("online = $%d", argIdx))
		args = append(args, *f.Online)
		argIdx++
	}
	if f.Role != "" {
		conditions = append(conditions, fmt.Sprintf("role = $%d", argIdx))
		args = append(args, f.Role)
		argIdx++
	}
	if f.ParentID != "" {
		conditions = append(conditions, fmt.Sprintf("parent_id = $%d", argIdx))
		args = append(args, f.ParentID)
		argIdx++
	}
	if f.Tag != "" {
		if f.TagValue != "" {
			// Match specific tag key=value: tags->>'env' = 'prod'
			conditions = append(conditions, fmt.Sprintf("tags->>$%d = $%d", argIdx, argIdx+1))
			args = append(args, f.Tag, f.TagValue)
			argIdx += 2
		} else {
			// Match tag key exists (any value): tags ? 'env'
			conditions = append(conditions, fmt.Sprintf("tags ? $%d", argIdx))
			args = append(args, f.Tag)
			argIdx++
		}
	}

	query := `SELECT id, hostname, os, os_version, arch, agent_version, listen_addr, role,
	                 capabilities, tags, parent_id, server_pod, online, exec_enabled, registered_at, last_seen_at
	          FROM agents`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY hostname"

	rows, err := db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var agents []Agent
	for rows.Next() {
		a, err := scanAgentRows(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// UpdateAgentHeartbeat sets the agent's last_seen_at to now and marks it online.
func (db *DB) UpdateAgentHeartbeat(ctx context.Context, id string) error {
	tag, err := db.pool.Exec(ctx, `
		UPDATE agents SET last_seen_at = now(), online = true WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// MarkStaleAgentsOffline marks agents as offline if their last heartbeat was
// longer ago than the given threshold. Returns the number of agents affected.
func (db *DB) MarkStaleAgentsOffline(ctx context.Context, threshold time.Duration) (int64, error) {
	tag, err := db.pool.Exec(ctx,
		`UPDATE agents SET online = false WHERE online = true AND last_seen_at < now() - $1::interval`,
		threshold.String(),
	)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// TouchAgentTree refreshes last_seen_at for an agent and all its descendants.
// Called by the reaper for agents with active server streams — their open
// connection proves the whole subtree is reachable.
func (db *DB) TouchAgentTree(ctx context.Context, rootID string) error {
	_, err := db.pool.Exec(ctx, `
		WITH RECURSIVE subtree AS (
			SELECT id FROM agents WHERE id = $1
			UNION ALL
			SELECT a.id FROM agents a JOIN subtree s ON a.parent_id = s.id
		)
		UPDATE agents SET last_seen_at = now()
		WHERE id IN (SELECT id FROM subtree) AND online = true`, rootID)
	return err
}

// MarkAgentTreeOffline marks an agent and all its descendants offline using a
// recursive CTE. Returns the number of agents affected. This replaces
// heartbeat-based reaping — when a parent detects a child disconnect, it
// propagates up to the server which marks the whole subtree in one query.
func (db *DB) MarkAgentTreeOffline(ctx context.Context, rootID string) (int64, error) {
	tag, err := db.pool.Exec(ctx, `
		WITH RECURSIVE subtree AS (
			SELECT id FROM agents WHERE id = $1
			UNION ALL
			SELECT a.id FROM agents a JOIN subtree s ON a.parent_id = s.id
		)
		UPDATE agents SET online = false
		WHERE id IN (SELECT id FROM subtree) AND online = true`, rootID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// SetAgentOffline marks an agent as offline.
func (db *DB) SetAgentOffline(ctx context.Context, id string) error {
	tag, err := db.pool.Exec(ctx, `UPDATE agents SET online = false WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// SetAgentRole updates the role of an agent.
func (db *DB) SetAgentRole(ctx context.Context, id string, role string) error {
	tag, err := db.pool.Exec(ctx, `UPDATE agents SET role = $1 WHERE id = $2`, role, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// SetAgentParent sets the parent_id for an agent. Empty string clears it (sets NULL).
func (db *DB) SetAgentParent(ctx context.Context, id string, parentID string) error {
	var pid any = parentID
	if parentID == "" {
		pid = nil
	}
	tag, err := db.pool.Exec(ctx, `UPDATE agents SET parent_id = $1 WHERE id = $2`, pid, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// UpdateAgentTags replaces the tags JSONB for an agent.
func (db *DB) UpdateAgentTags(ctx context.Context, id string, tags map[string]string) error {
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return fmt.Errorf("marshal tags: %w", err)
	}
	cmdTag, err := db.pool.Exec(ctx, `UPDATE agents SET tags = $1 WHERE id = $2`, tagsJSON, id)
	if err != nil {
		return err
	}
	if cmdTag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// DeleteAgent removes an agent by ID.
func (db *DB) DeleteAgent(ctx context.Context, id string) error {
	tag, err := db.pool.Exec(ctx, `DELETE FROM agents WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// scanAgent scans a single agent row from a pgx.Row.
func scanAgent(row pgx.Row) (Agent, error) {
	var a Agent
	var tagsJSON []byte
	err := row.Scan(
		&a.ID, &a.Hostname, &a.OS, &a.OSVersion, &a.Arch, &a.AgentVersion,
		&a.ListenAddr, &a.Role, &a.Capabilities, &tagsJSON,
		&a.ParentID, &a.ServerPod, &a.Online, &a.ExecEnabled, &a.RegisteredAt, &a.LastSeenAt,
	)
	if err != nil {
		return Agent{}, err
	}
	if tagsJSON != nil {
		if err := json.Unmarshal(tagsJSON, &a.Tags); err != nil {
			return Agent{}, fmt.Errorf("unmarshal tags: %w", err)
		}
	}
	if a.Tags == nil {
		a.Tags = make(map[string]string)
	}
	return a, nil
}

// scanAgentRows scans a single agent from pgx.Rows (used inside iteration).
func scanAgentRows(rows pgx.Rows) (Agent, error) {
	var a Agent
	var tagsJSON []byte
	err := rows.Scan(
		&a.ID, &a.Hostname, &a.OS, &a.OSVersion, &a.Arch, &a.AgentVersion,
		&a.ListenAddr, &a.Role, &a.Capabilities, &tagsJSON,
		&a.ParentID, &a.ServerPod, &a.Online, &a.ExecEnabled, &a.RegisteredAt, &a.LastSeenAt,
	)
	if err != nil {
		return Agent{}, err
	}
	if tagsJSON != nil {
		if err := json.Unmarshal(tagsJSON, &a.Tags); err != nil {
			return Agent{}, fmt.Errorf("unmarshal tags: %w", err)
		}
	}
	if a.Tags == nil {
		a.Tags = make(map[string]string)
	}
	return a, nil
}
