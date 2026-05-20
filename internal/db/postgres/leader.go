// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package postgres

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atgreen/dirq/internal/db"
)

// leaderLockKey is the bigint key passed to pg_try_advisory_lock for the
// dirq primary lock. The value is arbitrary but must be stable across
// dirq versions; never change it once deployed.
//
// 0x444952510001 = "DIRQ\0\0\0\1" interpreted as a big-endian int64.
const leaderLockKey int64 = 0x4449_5251_0000_0001

// leaderPollInterval governs how often a standby retries to acquire the
// lock and how often a leader verifies its connection is still alive.
// Short for fast failover; the DB ping is cheap.
const leaderPollInterval = 2 * time.Second

// NewLeader returns a leader-election worker backed by a Postgres
// session-level advisory lock. A single connection is held out of the
// pool while leadership is owned; release happens on Run's return (ctx
// done) or on connection loss, which automatically frees the lock so
// another pod can take over within one poll interval.
func (d *DB) NewLeader(log *slog.Logger) db.Leader {
	return &postgresLeader{
		pool: d.pool,
		log:  log,
	}
}

type postgresLeader struct {
	pool     *pgxpool.Pool
	log      *slog.Logger
	isLeader atomic.Bool
}

func (l *postgresLeader) IsLeader() bool { return l.isLeader.Load() }

func (l *postgresLeader) Run(ctx context.Context) {
	var conn *pgxpool.Conn
	defer func() {
		if conn != nil {
			// Best-effort: explicitly release the lock so a standby
			// can grab it within one poll interval instead of waiting
			// for the DB to notice the TCP close on shutdown.
			unlockCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", leaderLockKey)
			cancel()
			conn.Release()
		}
		l.isLeader.Store(false)
	}()

	ticker := time.NewTicker(leaderPollInterval)
	defer ticker.Stop()

	for {
		// Make sure we have a working connection.
		if conn == nil {
			c, err := l.pool.Acquire(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				l.log.Warn("leader election: pool acquire failed", "error", err)
				if !sleepUntil(ctx, ticker) {
					return
				}
				continue
			}
			conn = c
		}

		if l.isLeader.Load() {
			// We already hold the lock. Verify the underlying connection
			// is still alive; if it died, Postgres released our lock and
			// we must demote.
			if err := conn.Ping(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				l.log.Warn("leader election: lost db connection, demoting", "error", err)
				conn.Release()
				conn = nil
				l.isLeader.Store(false)
			}
		} else {
			// Standby: try to acquire the lock. pg_try_advisory_lock is
			// non-blocking and returns true if we got it. (It's also
			// re-entrant — only call it from this branch so we don't
			// build up a stale hold count.)
			var got bool
			err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", leaderLockKey).Scan(&got)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				l.log.Warn("leader election: lock query failed, dropping connection", "error", err)
				conn.Release()
				conn = nil
			} else if got {
				l.log.Info("acquired primary lock — this pod is now leader")
				l.isLeader.Store(true)
			}
		}

		if !sleepUntil(ctx, ticker) {
			return
		}
	}
}

// sleepUntil waits for either the next tick or ctx cancellation.
// Returns false if ctx is done (caller should exit).
func sleepUntil(ctx context.Context, t *time.Ticker) bool {
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
