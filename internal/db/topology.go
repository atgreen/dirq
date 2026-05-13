// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package db

import (
	"context"
)

// topologyLockID is a fixed advisory lock ID for serializing topology assignments.
const topologyLockID int64 = 0x4469725174706F // "DirQtpo"

// WithTopologyLock runs fn while holding an exclusive advisory lock.
// This serializes all topology assignments so concurrent registrations can't
// race on zone leader counts or parent child counts. The lock is released
// when the transaction commits.
func (db *DB) WithTopologyLock(ctx context.Context, fn func() error) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Acquire advisory lock — blocks until available.
	_, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", topologyLockID)
	if err != nil {
		return err
	}

	// fn runs its own queries against the pool. The advisory lock ensures
	// only one fn runs at a time across all connections.
	if err := fn(); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// FindRelaysWithChildren returns online relay agents that have at least one
// online child. These are candidates for promotion to zone leader — promoting
// them splits a branch mid-tree and the children stay connected.
// Ordered by child count descending (prefer relays with more children to
// maximize the subtree size brought to the new zone leader).
func (db *DB) FindRelaysWithChildren(ctx context.Context) ([]Agent, error) {
	rows, err := db.pool.Query(ctx, `
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

// SetAgentParent sets the parent_id for an agent. Empty string clears it.
// (Note: this overload handles empty string → NULL for zone leader promotion.)

// NodeLoad represents an agent and its child count.
type NodeLoad struct {
	Agent      Agent
	ChildCount int
	Depth      int
}

// FindImbalancedNodes returns the most overloaded node and the most underloaded
// node with room. Used by the rebalancer to redistribute subtrees.
// Returns (heavy, light, found). If the imbalance ratio is < 2x, found is false.
func (db *DB) FindImbalancedNodes(ctx context.Context, maxChildren int) (heavy NodeLoad, light NodeLoad, found bool, err error) {
	// Find the node with the most children.
	row := db.pool.QueryRow(ctx, `
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

	var heavyAgent Agent
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
		return heavy, light, false, nil // no nodes with children
	}

	// Find the shallowest node with the fewest children that has room.
	lightAgent, lightErr := db.FindShallowestParentWithRoom(ctx, maxChildren)
	if lightErr != nil || lightAgent.ID == "" || lightAgent.ID == heavyAgent.ID {
		return heavy, light, false, nil
	}

	lightCount, _ := db.CountChildren(ctx, lightAgent.ID)

	// Only rebalance if the heavy node has at least 2x the children of the light node.
	if heavyCount < (lightCount+1)*2 {
		return heavy, light, false, nil
	}

	heavy = NodeLoad{Agent: heavyAgent, ChildCount: heavyCount}
	light = NodeLoad{Agent: lightAgent, ChildCount: lightCount}
	return heavy, light, true, nil
}

// FindChildOfParent returns one online child of the given parent, preferring
// children that themselves have children (moving a subtree is more efficient
// than moving a leaf).
func (db *DB) FindChildOfParent(ctx context.Context, parentID string) (Agent, error) {
	row := db.pool.QueryRow(ctx, `
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

// CountAgentsByRole returns the number of online agents with the given role.
func (db *DB) CountAgentsByRole(ctx context.Context, role string) (int, error) {
	var count int
	err := db.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM agents WHERE role = $1 AND online = true`, role,
	).Scan(&count)
	return count, err
}

// CountChildren returns the number of online agents whose parent_id is the given agent.
func (db *DB) CountChildren(ctx context.Context, parentID string) (int, error) {
	var count int
	err := db.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM agents WHERE parent_id = $1 AND online = true`, parentID,
	).Scan(&count)
	return count, err
}

// FindZoneLeader returns the zone leader ancestor for a given agent.
// If the agent IS a zone leader, returns itself.
// Follows the parent_id chain up until it finds a zone_leader.
func (db *DB) FindZoneLeader(ctx context.Context, agentID string) (Agent, error) {
	// Use a recursive CTE to walk up the parent chain.
	row := db.pool.QueryRow(ctx, `
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
// excluding the primary parent and preferring agents on different branches
// (different zone leader subtrees) for fault isolation.
func (db *DB) FindFallbackParents(ctx context.Context, primaryParentID string, maxChildren int, count int) ([]Agent, error) {
	// Find the zone leader ancestor of the primary parent so we can
	// prefer fallbacks under a DIFFERENT zone leader.
	rows, err := db.pool.Query(ctx, `
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
		  -- Prefer nodes under a different zone leader than the primary parent
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

// FindParentWithRoom finds an online agent with the given role that has fewer
// than maxChildren children. Returns the agent, or an empty Agent if none found.
// Prefers agents with the fewest children (balances the tree).
func (db *DB) FindParentWithRoom(ctx context.Context, role string, maxChildren int) (Agent, error) {
	row := db.pool.QueryRow(ctx, `
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
		return Agent{}, err
	}
	return agent, nil
}

// FindShallowestParentWithRoom finds the shallowest node in the tree that has
// room for another child. "Shallowest" means fewest hops from a zone leader
// (BFS fill order). This keeps the tree balanced and minimizes depth.
//
// Uses a recursive CTE to compute depth from zone leaders, then picks the
// shallowest node with child_count < maxChildren.
func (db *DB) FindShallowestParentWithRoom(ctx context.Context, maxChildren int) (Agent, error) {
	row := db.pool.QueryRow(ctx, `
		WITH RECURSIVE tree AS (
			-- Zone leaders are depth 0
			SELECT id, hostname, os, os_version, arch, agent_version,
			       listen_addr, role, capabilities, tags,
			       parent_id, server_pod, online, exec_enabled,
			       registered_at, last_seen_at, 0 AS depth
			FROM agents
			WHERE role = 'zone_leader' AND online = true AND listen_addr != ''
			UNION ALL
			-- Children are depth + 1
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
		return Agent{}, err
	}
	return agent, nil
}
