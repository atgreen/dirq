// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/atgreen/dirq/internal/db"
)

// RegisterServerPeer upserts a server peer, updating its address and last_seen_at.
func (d *DB) RegisterServerPeer(ctx context.Context, podID, addr string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO server_peers (pod_id, addr, registered_at, last_seen_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (pod_id) DO UPDATE
		SET addr = excluded.addr, last_seen_at = ?`,
		podID, addr, now, now,
		now,
	)
	return err
}

// ListServerPeers returns all registered server peers.
func (d *DB) ListServerPeers(ctx context.Context) ([]db.ServerPeer, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT pod_id, addr, registered_at, last_seen_at
		FROM server_peers
		ORDER BY pod_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var peers []db.ServerPeer
	for rows.Next() {
		var p db.ServerPeer
		var registeredAt, lastSeenAt string
		if err := rows.Scan(&p.PodID, &p.Addr, &registeredAt, &lastSeenAt); err != nil {
			return nil, err
		}
		p.RegisteredAt, _ = time.Parse(time.RFC3339, registeredAt)
		p.LastSeenAt, _ = time.Parse(time.RFC3339, lastSeenAt)
		peers = append(peers, p)
	}
	return peers, rows.Err()
}

// RemoveServerPeer deletes a server peer by pod ID.
func (d *DB) RemoveServerPeer(ctx context.Context, podID string) error {
	result, err := d.db.ExecContext(ctx, `DELETE FROM server_peers WHERE pod_id = ?`, podID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
