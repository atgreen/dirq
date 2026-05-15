// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package postgres

import (
	"context"

	"github.com/atgreen/dirq/internal/db"
)

// topologyLockID is a fixed advisory lock ID for serializing topology assignments.
const topologyLockID int64 = 0x4469725174706F // "DirQtpo"

// WithTopologyLock runs fn while holding an exclusive advisory lock.
func (d *DB) WithTopologyLock(ctx context.Context, fn func() error) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", topologyLockID)
	if err != nil {
		return err
	}

	if err := fn(); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// FindRelaysWithChildren returns online relay agents that have at least one
// online child.
func (d *DB) FindRelaysWithChildren(ctx context.Context) ([]db.Agent, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT a.id, a.hostname, a.os, a.os_version, a.arch, a.agent_version,
		       a.listen_addr, a.role, a.capabilities, a.tags,
		       a.parent_id, a.server_pod, a.online, a.exec_enabled,
		       a.registered_at, a.last_seen_at
		FROM agents a
		WHERE a.role = 'relay' AND a.online = true AND a.listen_addr != ''
		  AND (SELECT COUNT(*) FROM agents c WHERE c.parent_id = a.id AND c.online = true) > 0
		ORDER BY (SELECT COUNT(*) FROM agents c WHERE c.parent_id = a.id AND c.online = true) DESC
		LIMIT 5`)
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

// FindImbalancedNodes returns the most overloaded node and the most underloaded
// node with room.
func (d *DB) FindImbalancedNodes(ctx context.Context, maxChildren int) (heavy db.NodeLoad, light db.NodeLoad, found bool, err error) {
	row := d.pool.QueryRow(ctx, `
		SELECT a.id, a.hostname, a.os, a.os_version, a.arch, a.agent_version,
		       a.listen_addr, a.role, a.capabilities, a.tags,
		       a.parent_id, a.server_pod, a.online, a.exec_enabled,
		       a.registered_at, a.last_seen_at,
		       (SELECT COUNT(*) FROM agents c WHERE c.parent_id = a.id AND c.online = true) AS child_count
		FROM agents a
		WHERE a.online = true AND a.listen_addr != ''
		  AND (SELECT COUNT(*) FROM agents c WHERE c.parent_id = a.id AND c.online = true) > 1
		ORDER BY child_count DESC
		LIMIT 1`)

	var heavyAgent db.Agent
	var heavyCount int
	var tagsJSON []byte
	scanErr := row.Scan(
		&heavyAgent.ID, &heavyAgent.Hostname, &heavyAgent.OS, &heavyAgent.OSVersion,
		&heavyAgent.Arch, &heavyAgent.AgentVersion, &heavyAgent.ListenAddr, &heavyAgent.Role,
		&heavyAgent.Capabilities, &tagsJSON,
		&heavyAgent.ParentID, &heavyAgent.ServerPod, &heavyAgent.Online, &heavyAgent.ExecEnabled,
		&heavyAgent.RegisteredAt, &heavyAgent.LastSeenAt,
		&heavyCount,
	)
	if scanErr != nil {
		return heavy, light, false, nil
	}

	lightAgent, lightErr := d.FindShallowestParentWithRoom(ctx, maxChildren)
	if lightErr != nil || lightAgent.ID == "" || lightAgent.ID == heavyAgent.ID {
		return heavy, light, false, nil
	}

	lightCount, _ := d.CountChildren(ctx, lightAgent.ID)

	if heavyCount < (lightCount+1)*2 {
		return heavy, light, false, nil
	}

	heavy = db.NodeLoad{Agent: heavyAgent, ChildCount: heavyCount}
	light = db.NodeLoad{Agent: lightAgent, ChildCount: lightCount}
	return heavy, light, true, nil
}

// FindChildOfParent returns one online child of the given parent.
func (d *DB) FindChildOfParent(ctx context.Context, parentID string) (db.Agent, error) {
	row := d.pool.QueryRow(ctx, `
		SELECT a.id, a.hostname, a.os, a.os_version, a.arch, a.agent_version,
		       a.listen_addr, a.role, a.capabilities, a.tags,
		       a.parent_id, a.server_pod, a.online, a.exec_enabled,
		       a.registered_at, a.last_seen_at
		FROM agents a
		WHERE a.parent_id = $1 AND a.online = true AND a.listen_addr != ''
		ORDER BY (SELECT COUNT(*) FROM agents c WHERE c.parent_id = a.id AND c.online = true) DESC
		LIMIT 1`,
		parentID,
	)
	return scanAgent(row)
}

// CountOnlineZoneLeaders returns the number of agents that are both
// role=zone_leader AND online=true.
func (d *DB) CountOnlineZoneLeaders(ctx context.Context) (int, error) {
	var count int
	err := d.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM agents WHERE role = 'zone_leader' AND online = true`,
	).Scan(&count)
	return count, err
}

// CountAgentsByRole returns the number of online agents with the given role.
func (d *DB) CountAgentsByRole(ctx context.Context, role string) (int, error) {
	var count int
	err := d.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM agents WHERE role = $1 AND online = true`, role,
	).Scan(&count)
	return count, err
}

// CountChildren returns the number of online agents whose parent_id is the given agent.
func (d *DB) CountChildren(ctx context.Context, parentID string) (int, error) {
	var count int
	err := d.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM agents WHERE parent_id = $1 AND online = true`, parentID,
	).Scan(&count)
	return count, err
}

// FindZoneLeader returns the zone leader ancestor for a given agent.
func (d *DB) FindZoneLeader(ctx context.Context, agentID string) (db.Agent, error) {
	row := d.pool.QueryRow(ctx, `
		WITH RECURSIVE ancestors AS (
			SELECT id, hostname, os, os_version, arch, agent_version,
			       listen_addr, role, capabilities, tags,
			       parent_id, server_pod, online, exec_enabled,
			       registered_at, last_seen_at, 0 AS depth
			FROM agents WHERE id = $1
			UNION ALL
			SELECT a.id, a.hostname, a.os, a.os_version, a.arch, a.agent_version,
			       a.listen_addr, a.role, a.capabilities, a.tags,
			       a.parent_id, a.server_pod, a.online, a.exec_enabled,
			       a.registered_at, a.last_seen_at, ancestors.depth + 1
			FROM agents a
			JOIN ancestors ON a.id = ancestors.parent_id
			WHERE ancestors.role != 'zone_leader'
		)
		SELECT id, hostname, os, os_version, arch, agent_version,
		       listen_addr, role, capabilities, tags,
		       parent_id, server_pod, online, exec_enabled,
		       registered_at, last_seen_at
		FROM ancestors
		WHERE role = 'zone_leader'
		LIMIT 1`,
		agentID,
	)
	return scanAgent(row)
}

// FindFallbackParents returns up to `count` online agents with room for children,
// excluding the primary parent.
func (d *DB) FindFallbackParents(ctx context.Context, primaryParentID string, maxChildren int, count int) ([]db.Agent, error) {
	rows, err := d.pool.Query(ctx, `
		WITH RECURSIVE tree AS (
			SELECT id, hostname, os, os_version, arch, agent_version,
			       listen_addr, role, capabilities, tags,
			       parent_id, server_pod, online, exec_enabled,
			       registered_at, last_seen_at, 0 AS depth,
			       id AS root_zl
			FROM agents
			WHERE role = 'zone_leader' AND online = true AND listen_addr != ''
			UNION ALL
			SELECT a.id, a.hostname, a.os, a.os_version, a.arch, a.agent_version,
			       a.listen_addr, a.role, a.capabilities, a.tags,
			       a.parent_id, a.server_pod, a.online, a.exec_enabled,
			       a.registered_at, a.last_seen_at, tree.depth + 1,
			       tree.root_zl
			FROM agents a
			JOIN tree ON a.parent_id = tree.id
			WHERE a.online = true AND a.listen_addr != ''
		)
		SELECT t.id, t.hostname, t.os, t.os_version, t.arch, t.agent_version,
		       t.listen_addr, t.role, t.capabilities, t.tags,
		       t.parent_id, t.server_pod, t.online, t.exec_enabled,
		       t.registered_at, t.last_seen_at
		FROM tree t
		WHERE t.id != $1
		  AND (SELECT COUNT(*) FROM agents c WHERE c.parent_id = t.id AND c.online = true) < $2
		ORDER BY
		  CASE WHEN t.root_zl = (
		    SELECT root_zl FROM tree WHERE id = $1 LIMIT 1
		  ) THEN 1 ELSE 0 END ASC,
		  t.depth ASC,
		  (SELECT COUNT(*) FROM agents c WHERE c.parent_id = t.id AND c.online = true) ASC
		LIMIT $3`,
		primaryParentID, maxChildren, count,
	)
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

// FindParentWithRoom finds an online agent with the given role that has fewer
// than maxChildren children.
func (d *DB) FindParentWithRoom(ctx context.Context, role string, maxChildren int) (db.Agent, error) {
	row := d.pool.QueryRow(ctx, `
		SELECT a.id, a.hostname, a.os, a.os_version, a.arch, a.agent_version,
		       a.listen_addr, a.role, a.capabilities, a.tags,
		       a.parent_id, a.server_pod, a.online, a.exec_enabled,
		       a.registered_at, a.last_seen_at
		FROM agents a
		WHERE a.role = $1 AND a.online = true AND a.listen_addr != ''
		  AND (SELECT COUNT(*) FROM agents c WHERE c.parent_id = a.id AND c.online = true) < $2
		ORDER BY (SELECT COUNT(*) FROM agents c WHERE c.parent_id = a.id AND c.online = true) ASC
		LIMIT 1`,
		role, maxChildren,
	)
	agent, err := scanAgent(row)
	if err != nil {
		return db.Agent{}, err
	}
	return agent, nil
}

// FindShallowestParentWithRoom finds the shallowest node in the tree that has
// room for another child.
func (d *DB) FindShallowestParentWithRoom(ctx context.Context, maxChildren int) (db.Agent, error) {
	row := d.pool.QueryRow(ctx, `
		WITH RECURSIVE tree AS (
			SELECT id, hostname, os, os_version, arch, agent_version,
			       listen_addr, role, capabilities, tags,
			       parent_id, server_pod, online, exec_enabled,
			       registered_at, last_seen_at, 0 AS depth
			FROM agents
			WHERE role = 'zone_leader' AND online = true AND listen_addr != ''
			UNION ALL
			SELECT a.id, a.hostname, a.os, a.os_version, a.arch, a.agent_version,
			       a.listen_addr, a.role, a.capabilities, a.tags,
			       a.parent_id, a.server_pod, a.online, a.exec_enabled,
			       a.registered_at, a.last_seen_at, tree.depth + 1
			FROM agents a
			JOIN tree ON a.parent_id = tree.id
			WHERE a.online = true AND a.listen_addr != ''
		)
		SELECT t.id, t.hostname, t.os, t.os_version, t.arch, t.agent_version,
		       t.listen_addr, t.role, t.capabilities, t.tags,
		       t.parent_id, t.server_pod, t.online, t.exec_enabled,
		       t.registered_at, t.last_seen_at
		FROM tree t
		WHERE (SELECT COUNT(*) FROM agents c WHERE c.parent_id = t.id AND c.online = true) < $1
		ORDER BY t.depth ASC,
		         (SELECT COUNT(*) FROM agents c WHERE c.parent_id = t.id AND c.online = true) ASC
		LIMIT 1`,
		maxChildren,
	)
	agent, err := scanAgent(row)
	if err != nil {
		return db.Agent{}, err
	}
	return agent, nil
}
