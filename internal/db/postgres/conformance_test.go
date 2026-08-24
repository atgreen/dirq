// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package postgres_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/atgreen/dirq/internal/db"
	"github.com/atgreen/dirq/internal/db/conformance"
	"github.com/atgreen/dirq/internal/db/postgres"
)

// schemaSeq disambiguates schemas created by successive factory calls
// within one test process.
var schemaSeq atomic.Int64

// TestConformance runs the shared backend conformance suite against a
// PostgreSQL server named by DIRQ_TEST_POSTGRES_URL (a postgres:// URL).
// When the variable is unset the test is skipped, so plain `go test
// ./...` passes without any external services. Each factory call gets a
// fresh throwaway schema (via search_path) that is dropped on cleanup.
//
// Example:
//
//	DIRQ_TEST_POSTGRES_URL=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable \
//	  go test ./internal/db/postgres/
func TestConformance(t *testing.T) {
	base := os.Getenv("DIRQ_TEST_POSTGRES_URL")
	if base == "" {
		t.Skip("DIRQ_TEST_POSTGRES_URL not set; skipping postgres conformance tests")
	}

	conformance.Run(t, func(t *testing.T) db.DB {
		t.Helper()
		ctx := context.Background()

		schema := fmt.Sprintf("dirq_conformance_%d_%d", os.Getpid(), schemaSeq.Add(1))

		admin, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Fatalf("connect to %s: %v", base, err)
		}
		if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
			admin.Close(ctx)
			t.Fatalf("create schema %s: %v", schema, err)
		}
		t.Cleanup(func() {
			_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
			admin.Close(context.Background())
		})

		// public stays on the search path so extension functions (e.g.
		// pgcrypto's gen_random_uuid on PG < 13) resolve; unqualified
		// CREATE TABLE statements land in the throwaway schema.
		sep := "?"
		if strings.Contains(base, "?") {
			sep = "&"
		}
		dsn := base + sep + "search_path=" + schema + ",public"

		s, err := postgres.New(ctx, dsn)
		if err != nil {
			t.Fatalf("postgres.New: %v", err)
		}
		t.Cleanup(s.Close)

		if err := s.RunMigrations(ctx); err != nil {
			t.Fatalf("RunMigrations: %v", err)
		}
		return s
	})
}
