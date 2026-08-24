// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package sqlite

import (
	"context"
	"log/slog"

	"github.com/atgreen/dirq/internal/db"
)

// NewLeader returns a no-op leader that always reports leadership.
// SQLite is single-instance by construction; there is nothing to elect.
func (d *DB) NewLeader(_ *slog.Logger) db.Leader {
	return sqliteLeader{}
}

type sqliteLeader struct{}

func (sqliteLeader) IsLeader() bool          { return true }
func (sqliteLeader) Run(ctx context.Context) { <-ctx.Done() }
