// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/atgreen/dirq/internal/db"
	"github.com/atgreen/dirq/internal/db/sqlite"
)

// The in-memory topology refuses to record a cycle, but the DB mirror is
// written node by node with no transaction, so a parent swap observed
// mid-snapshot can persist one that never existed in memory. An uncapped
// WITH RECURSIVE ... UNION ALL walk over that cycle never terminates — and
// the caller in closeAgentStream had no deadline to rescue it, so the wedge
// survived a restart because the cycle is still in the table (dirq-632.14).
func TestSubtreeWalksTerminateOnACyclicParentChain(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "cycle-test.db")
	store, err := sqlite.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(store.Close)

	ctx := context.Background()
	a, err := store.RegisterAgent(ctx, db.RegisterAgentParams{
		Hostname: "host-a", OS: "linux", Arch: "amd64", ListenAddr: "10.0.0.1:50052",
	})
	if err != nil {
		t.Fatalf("register host-a: %v", err)
	}
	b, err := store.RegisterAgent(ctx, db.RegisterAgentParams{
		Hostname: "host-b", OS: "linux", Arch: "amd64", ListenAddr: "10.0.0.2:50052",
	})
	if err != nil {
		t.Fatalf("register host-b: %v", err)
	}

	// Nothing in the schema or the write path stops this: parent_id is a
	// plain self-referencing column and SetAgentParent is a bare UPDATE.
	if err := store.SetAgentParent(ctx, a.ID, b.ID); err != nil {
		t.Fatalf("set a.parent = b: %v", err)
	}
	if err := store.SetAgentParent(ctx, b.ID, a.ID); err != nil {
		t.Fatalf("set b.parent = a: %v", err)
	}

	// Each walk gets a deadline so a regression fails the test instead of
	// hanging the suite, and both must finish cleanly well inside it.
	t.Run("MarkAgentTreeOffline", func(t *testing.T) {
		done := make(chan error, 1)
		go func() {
			walkCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := store.MarkAgentTreeOffline(walkCtx, a.ID)
			done <- err
		}()
		assertFinished(t, done)
	})

	t.Run("TouchAgentTree", func(t *testing.T) {
		done := make(chan error, 1)
		go func() {
			walkCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done <- store.TouchAgentTree(walkCtx, a.ID)
		}()
		assertFinished(t, done)
	})
}

func assertFinished(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("walk over a cyclic parent chain returned an error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("walk over a cyclic parent chain did not terminate — the recursive CTE is uncapped")
	}
}
