// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/atgreen/dirq/internal/db"
	"github.com/atgreen/dirq/internal/db/conformance"
	"github.com/atgreen/dirq/internal/db/sqlite"
)

// TestConformance runs the shared backend conformance suite against a
// temp-dir SQLite database. sqlite.New runs migrations itself, so each
// factory call yields a fresh, fully migrated store.
func TestConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T) db.DB {
		t.Helper()
		dsn := filepath.Join(t.TempDir(), "dirq-test.db")
		s, err := sqlite.New(context.Background(), dsn)
		if err != nil {
			t.Fatalf("sqlite.New(%s): %v", dsn, err)
		}
		t.Cleanup(s.Close)
		return s
	})
}
