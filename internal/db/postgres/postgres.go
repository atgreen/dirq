// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package postgres

import (
	"context"
	_ "embed"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// DB wraps a pgxpool connection pool for all database operations.
type DB struct {
	pool *pgxpool.Pool
}

// New creates a new DB instance with a connection pool.
func New(ctx context.Context, connString string) (*DB, error) {
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &DB{pool: pool}, nil
}

// Ping verifies the database connection is alive.
func (db *DB) Ping(ctx context.Context) error {
	return db.pool.Ping(ctx)
}

// Close shuts down the connection pool.
func (db *DB) Close() {
	db.pool.Close()
}

// RunMigrations executes the embedded schema.sql against the database.
func (db *DB) RunMigrations(ctx context.Context) error {
	_, err := db.pool.Exec(ctx, schemaSQL)
	return err
}
