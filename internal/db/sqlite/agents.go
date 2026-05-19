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

// RegisterAgent inserts a new agent and returns the created record.
func (d *DB) RegisterAgent(ctx context.Context, p db.RegisterAgentParams) (db.Agent, error) {
	tagsJSON, err := json.Marshal(p.Tags)
	if err != nil {
		return db.Agent{}, fmt.Errorf("marshal tags: %w", err)
	}

	caps := p.Capabilities
	if caps == nil {
		caps = []string{}
	}
	capsJSON, err := json.Marshal(caps)
	if err != nil {
		return db.Agent{}, fmt.Errorf("marshal capabilities: %w", err)
	}

	id := generateUUID()
	now := time.Now().UTC().Format(time.RFC3339)

	_, err = d.db.ExecContext(ctx, `
		INSERT INTO agents (id, hostname, os, os_version, arch, agent_version, listen_addr, capabilities, tags, exec_enabled, registered_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (hostname) DO UPDATE SET
			os = excluded.os,
			os_version = excluded.os_version,
			arch = excluded.arch,
			agent_version = excluded.agent_version,
			listen_addr = excluded.listen_addr,
			capabilities = excluded.capabilities,
			tags = json_patch(agents.tags, excluded.tags),
			exec_enabled = excluded.exec_enabled,
			online = 1,
			last_seen_at = ?`,
		id, p.Hostname, p.OS, p.OSVersion, p.Arch, p.AgentVersion, p.ListenAddr,
		string(capsJSON), string(tagsJSON), p.ExecEnabled, now, now,
		now,
	)
	if err != nil {
		return db.Agent{}, err
	}

	return d.GetAgentByHostname(ctx, p.Hostname)
}

// GetAgent retrieves an agent by ID.
func (d *DB) GetAgent(ctx context.Context, id string) (db.Agent, error) {
	row := d.db.QueryRowContext(ctx, `
		SELECT id, hostname, os, os_version, arch, agent_version, listen_addr, role,
		       capabilities, tags, parent_id, server_pod, online, exec_enabled, registered_at, last_seen_at
		FROM agents WHERE id = ?`, id)
	return scanAgent(row)
}

// GetAgentByHostname retrieves an agent by hostname.
func (d *DB) GetAgentByHostname(ctx context.Context, hostname string) (db.Agent, error) {
	row := d.db.QueryRowContext(ctx, `
		SELECT id, hostname, os, os_version, arch, agent_version, listen_addr, role,
		       capabilities, tags, parent_id, server_pod, online, exec_enabled, registered_at, last_seen_at
		FROM agents WHERE hostname = ?`, hostname)
	return scanAgent(row)
}

// ListAgents returns agents matching the given filter.
func (d *DB) ListAgents(ctx context.Context, f db.ListAgentsFilter) ([]db.Agent, error) {
	var conditions []string
	var args []any

	if f.Online != nil {
		if *f.Online {
			conditions = append(conditions, "online = 1")
		} else {
			conditions = append(conditions, "online = 0")
		}
	}
	if f.Role != "" {
		conditions = append(conditions, "role = ?")
		args = append(args, f.Role)
	}
	if f.ParentID != "" {
		conditions = append(conditions, "parent_id = ?")
		args = append(args, f.ParentID)
	}
	if f.Tag != "" {
		if f.TagValue != "" {
			conditions = append(conditions, "json_extract(tags, '$.' || ?) = ?")
			args = append(args, f.Tag, f.TagValue)
		} else {
			conditions = append(conditions, "json_type(tags, '$.' || ?) IS NOT NULL")
			args = append(args, f.Tag)
		}
	}

	query := `SELECT id, hostname, os, os_version, arch, agent_version, listen_addr, role,
	                 capabilities, tags, parent_id, server_pod, online, exec_enabled, registered_at, last_seen_at
	          FROM agents`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY hostname"

	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var agents []db.Agent
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
func (d *DB) UpdateAgentHeartbeat(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := d.db.ExecContext(ctx, `
		UPDATE agents SET last_seen_at = ?, online = 1 WHERE id = ?`, now, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkStaleAgentsOffline marks agents as offline if their last heartbeat was
// longer ago than the given threshold.
func (d *DB) MarkStaleAgentsOffline(ctx context.Context, threshold time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-threshold).Format(time.RFC3339)
	result, err := d.db.ExecContext(ctx,
		`UPDATE agents SET online = 0 WHERE online = 1 AND last_seen_at < ?`,
		cutoff,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// TouchAgentTree refreshes last_seen_at for an agent and all its descendants.
func (d *DB) TouchAgentTree(ctx context.Context, rootID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.db.ExecContext(ctx, `
		WITH RECURSIVE subtree AS (
			SELECT id FROM agents WHERE id = ?
			UNION ALL
			SELECT a.id FROM agents a JOIN subtree s ON a.parent_id = s.id
		)
		UPDATE agents SET last_seen_at = ?
		WHERE id IN (SELECT id FROM subtree) AND online = 1`, rootID, now)
	return err
}

// MarkAgentTreeOffline marks an agent and all its descendants offline.
func (d *DB) MarkAgentTreeOffline(ctx context.Context, rootID string) (int64, error) {
	result, err := d.db.ExecContext(ctx, `
		WITH RECURSIVE subtree AS (
			SELECT id FROM agents WHERE id = ?
			UNION ALL
			SELECT a.id FROM agents a JOIN subtree s ON a.parent_id = s.id
		)
		UPDATE agents SET online = 0
		WHERE id IN (SELECT id FROM subtree) AND online = 1`, rootID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// SetAgentOffline marks an agent as offline.
func (d *DB) SetAgentOffline(ctx context.Context, id string) error {
	result, err := d.db.ExecContext(ctx, `UPDATE agents SET online = 0 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetAgentRole updates the role of an agent.
func (d *DB) SetAgentRole(ctx context.Context, id string, role string) error {
	result, err := d.db.ExecContext(ctx, `UPDATE agents SET role = ? WHERE id = ?`, role, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetAgentParent sets the parent_id for an agent. Empty string clears it.
func (d *DB) SetAgentParent(ctx context.Context, id string, parentID string) error {
	var pid any = parentID
	if parentID == "" {
		pid = nil
	}
	result, err := d.db.ExecContext(ctx, `UPDATE agents SET parent_id = ? WHERE id = ?`, pid, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateAgentTags replaces the tags for an agent.
func (d *DB) UpdateAgentTags(ctx context.Context, id string, tags map[string]string) error {
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return fmt.Errorf("marshal tags: %w", err)
	}
	result, err := d.db.ExecContext(ctx, `UPDATE agents SET tags = ? WHERE id = ?`, string(tagsJSON), id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteAgent removes an agent by ID.
func (d *DB) DeleteAgent(ctx context.Context, id string) error {
	result, err := d.db.ExecContext(ctx, `DELETE FROM agents WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// scanAgent scans a single agent row.
func scanAgent(row *sql.Row) (db.Agent, error) {
	var a db.Agent
	var capsJSON, tagsJSON string
	var online, execEnabled int
	var registeredAt, lastSeenAt string
	var parentID, serverPod sql.NullString
	err := row.Scan(
		&a.ID, &a.Hostname, &a.OS, &a.OSVersion, &a.Arch, &a.AgentVersion,
		&a.ListenAddr, &a.Role, &capsJSON, &tagsJSON,
		&parentID, &serverPod, &online, &execEnabled, &registeredAt, &lastSeenAt,
	)
	if err != nil {
		return db.Agent{}, err
	}

	a.Online = online != 0
	a.ExecEnabled = execEnabled != 0
	if parentID.Valid {
		a.ParentID = &parentID.String
	}
	if serverPod.Valid {
		a.ServerPod = &serverPod.String
	}

	if err := json.Unmarshal([]byte(capsJSON), &a.Capabilities); err != nil {
		return db.Agent{}, fmt.Errorf("unmarshal capabilities: %w", err)
	}
	if err := json.Unmarshal([]byte(tagsJSON), &a.Tags); err != nil {
		return db.Agent{}, fmt.Errorf("unmarshal tags: %w", err)
	}
	if a.Tags == nil {
		a.Tags = make(map[string]string)
	}
	if a.Capabilities == nil {
		a.Capabilities = []string{}
	}

	a.RegisteredAt, _ = time.Parse(time.RFC3339, registeredAt)
	a.LastSeenAt, _ = time.Parse(time.RFC3339, lastSeenAt)

	return a, nil
}

// scanAgentRows scans a single agent from sql.Rows.
func scanAgentRows(rows *sql.Rows) (db.Agent, error) {
	var a db.Agent
	var capsJSON, tagsJSON string
	var online, execEnabled int
	var registeredAt, lastSeenAt string
	var parentID, serverPod sql.NullString
	err := rows.Scan(
		&a.ID, &a.Hostname, &a.OS, &a.OSVersion, &a.Arch, &a.AgentVersion,
		&a.ListenAddr, &a.Role, &capsJSON, &tagsJSON,
		&parentID, &serverPod, &online, &execEnabled, &registeredAt, &lastSeenAt,
	)
	if err != nil {
		return db.Agent{}, err
	}

	a.Online = online != 0
	a.ExecEnabled = execEnabled != 0
	if parentID.Valid {
		a.ParentID = &parentID.String
	}
	if serverPod.Valid {
		a.ServerPod = &serverPod.String
	}

	if err := json.Unmarshal([]byte(capsJSON), &a.Capabilities); err != nil {
		return db.Agent{}, fmt.Errorf("unmarshal capabilities: %w", err)
	}
	if err := json.Unmarshal([]byte(tagsJSON), &a.Tags); err != nil {
		return db.Agent{}, fmt.Errorf("unmarshal tags: %w", err)
	}
	if a.Tags == nil {
		a.Tags = make(map[string]string)
	}
	if a.Capabilities == nil {
		a.Capabilities = []string{}
	}

	a.RegisteredAt, _ = time.Parse(time.RFC3339, registeredAt)
	a.LastSeenAt, _ = time.Parse(time.RFC3339, lastSeenAt)

	return a, nil
}
