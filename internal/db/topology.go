// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package db

import (
	"context"
)

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
