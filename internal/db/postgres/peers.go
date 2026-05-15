// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package postgres

import (
	"context"

	"github.com/atgreen/dirq/internal/db"
	"github.com/jackc/pgx/v5"
)

// RegisterServerPeer upserts a server peer, updating its address and last_seen_at.
func (d *DB) RegisterServerPeer(ctx context.Context, podID, addr string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO server_peers (pod_id, addr, registered_at, last_seen_at)
		VALUES ($1, $2, now(), now())
		ON CONFLICT (pod_id) DO UPDATE
		SET addr = EXCLUDED.addr, last_seen_at = now()`,
		podID, addr,
	)
	return err
}

// ListServerPeers returns all registered server peers.
func (d *DB) ListServerPeers(ctx context.Context) ([]db.ServerPeer, error) {
	rows, err := d.pool.Query(ctx, `
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
		if err := rows.Scan(&p.PodID, &p.Addr, &p.RegisteredAt, &p.LastSeenAt); err != nil {
			return nil, err
		}
		peers = append(peers, p)
	}
	return peers, rows.Err()
}

// RemoveServerPeer deletes a server peer by pod ID.
func (d *DB) RemoveServerPeer(ctx context.Context, podID string) error {
	tag, err := d.pool.Exec(ctx, `DELETE FROM server_peers WHERE pod_id = $1`, podID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
