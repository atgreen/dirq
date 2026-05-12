// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package db

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// RegisterServerPeer upserts a server peer, updating its address and last_seen_at.
func (db *DB) RegisterServerPeer(ctx context.Context, podID, addr string) error {
	_, err := db.pool.Exec(ctx, `
		INSERT INTO server_peers (pod_id, addr, registered_at, last_seen_at)
		VALUES ($1, $2, now(), now())
		ON CONFLICT (pod_id) DO UPDATE
		SET addr = EXCLUDED.addr, last_seen_at = now()`,
		podID, addr,
	)
	return err
}

// ListServerPeers returns all registered server peers.
func (db *DB) ListServerPeers(ctx context.Context) ([]ServerPeer, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT pod_id, addr, registered_at, last_seen_at
		FROM server_peers
		ORDER BY pod_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var peers []ServerPeer
	for rows.Next() {
		var p ServerPeer
		if err := rows.Scan(&p.PodID, &p.Addr, &p.RegisteredAt, &p.LastSeenAt); err != nil {
			return nil, err
		}
		peers = append(peers, p)
	}
	return peers, rows.Err()
}

// RemoveServerPeer deletes a server peer by pod ID.
func (db *DB) RemoveServerPeer(ctx context.Context, podID string) error {
	tag, err := db.pool.Exec(ctx, `DELETE FROM server_peers WHERE pod_id = $1`, podID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
