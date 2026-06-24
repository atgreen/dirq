// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"sync"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schemaSQL string

// DB wraps a database/sql connection for SQLite operations.
type DB struct {
	db *sql.DB

	// topologyMu serializes topology assignments (replaces pg_advisory_xact_lock).
	topologyMu sync.Mutex
}

// New creates a new SQLite DB instance.
func New(ctx context.Context, dsn string) (*DB, error) {
	// Strip sqlite:// prefix if present.
	dsn = strings.TrimPrefix(dsn, "sqlite://")

	sqlDB, err := sql.Open("sqlite3", dsn+"?_journal_mode=WAL&_foreign_keys=ON")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	// Enable WAL mode and foreign keys via pragmas.
	if _, err := sqlDB.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := sqlDB.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	d := &DB{db: sqlDB}

	if err := d.RunMigrations(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return d, nil
}

// Ping verifies the database connection is alive.
func (d *DB) Ping(ctx context.Context) error {
	return d.db.PingContext(ctx)
}

// Close shuts down the database connection.
func (d *DB) Close() {
	d.db.Close()
}

// Kind returns the backend name for status reporting.
func (d *DB) Kind() string {
	return "sqlite"
}

// RunMigrations executes the embedded schema.sql against the database, then
// applies idempotent column additions for databases created by older versions.
func (d *DB) RunMigrations(ctx context.Context) error {
	if _, err := d.db.ExecContext(ctx, schemaSQL); err != nil {
		return err
	}
	// SQLite lacks ADD COLUMN IF NOT EXISTS, so run each addition and tolerate
	// the duplicate-column error when the column already exists.
	for _, stmt := range []string{
		`ALTER TABLE api_tokens ADD COLUMN aap_users TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := d.db.ExecContext(ctx, stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	return nil
}

// generateUUID creates a random UUID v4 string.
func generateUUID() string {
	var uuid [16]byte
	rand.Read(uuid[:])
	uuid[6] = (uuid[6] & 0x0f) | 0x40 // version 4
	uuid[8] = (uuid[8] & 0x3f) | 0x80 // variant 2
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16])
}
