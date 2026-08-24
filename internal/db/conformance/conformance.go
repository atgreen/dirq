// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

// Package conformance is a backend-agnostic test suite for the db.DB
// interface. Both the sqlite and postgres backends run the same suite
// (see conformance_test.go in each backend package), so any behavioral
// drift between the two hand-maintained implementations shows up as a
// test failure in one of them.
//
// The suite is intended to be invoked from a normal Go test:
//
//	func TestConformance(t *testing.T) {
//		conformance.Run(t, func(t *testing.T) db.DB { ... })
//	}
//
// The factory is called once per top-level subtest and must return a
// fully migrated, empty store. Cleanup should be registered on t.
package conformance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/atgreen/dirq/internal/db"
)

// Factory returns a fresh, migrated, empty store. It is called once per
// top-level subtest; register cleanup with t.Cleanup.
type Factory func(t *testing.T) db.DB

// Run executes the full conformance suite against stores produced by
// newStore.
func Run(t *testing.T, newStore Factory) {
	t.Run("Health", func(t *testing.T) { testHealth(t, newStore(t)) })
	t.Run("AgentRegistration", func(t *testing.T) { testAgentRegistration(t, newStore(t)) })
	t.Run("AgentListing", func(t *testing.T) { testAgentListing(t, newStore(t)) })
	t.Run("AgentState", func(t *testing.T) { testAgentState(t, newStore(t)) })
	t.Run("AgentTree", func(t *testing.T) { testAgentTree(t, newStore(t)) })
	t.Run("Facts", func(t *testing.T) { testFacts(t, newStore(t)) })
	t.Run("Tokens", func(t *testing.T) { testTokens(t, newStore(t)) })
	t.Run("Queries", func(t *testing.T) { testQueries(t, newStore(t)) })
	t.Run("ExecLog", func(t *testing.T) { testExecLog(t, newStore(t)) })
	t.Run("Topology", func(t *testing.T) { testTopology(t, newStore(t)) })
	t.Run("Imbalance", func(t *testing.T) { testImbalance(t, newStore(t)) })
	t.Run("Peers", func(t *testing.T) { testPeers(t, newStore(t)) })
}

// ─────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────

// must fails the test immediately if err is non-nil.
func must(t *testing.T, err error, op string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
}

// wantNoRows asserts that err is (or wraps) sql.ErrNoRows. pgx.ErrNoRows
// wraps sql.ErrNoRows since pgx v5.5, so this holds for both backends.
func wantNoRows(t *testing.T, err error, op string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a no-rows error, got nil", op)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("%s: expected error wrapping sql.ErrNoRows, got %v", op, err)
	}
}

// agentParams builds a reasonable default registration for hostname.
func agentParams(hostname string) db.RegisterAgentParams {
	return db.RegisterAgentParams{
		Hostname:     hostname,
		OS:           "linux",
		OSVersion:    "9.4",
		Arch:         "x86_64",
		AgentVersion: "0.25.1",
		ListenAddr:   "10.1.2.3:8443",
		Capabilities: []string{"exec", "facts"},
		Tags:         map[string]string{"env": "test"},
		ExecEnabled:  true,
	}
}

// register registers an agent and fails the test on error.
func register(t *testing.T, s db.DB, p db.RegisterAgentParams) db.Agent {
	t.Helper()
	a, err := s.RegisterAgent(context.Background(), p)
	must(t, err, "RegisterAgent("+p.Hostname+")")
	return a
}

// closeTimes reports whether a and b are within 2s of each other, which
// tolerates sqlite's second-granularity RFC3339 storage.
func closeTimes(a, b time.Time) bool {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d <= 2*time.Second
}

// checkJSONEq asserts that raw decodes to exactly want.
func checkJSONEq(t *testing.T, raw json.RawMessage, want map[string]any, op string) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%s: unmarshal %q: %v", op, string(raw), err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: data mismatch\n got: %#v\nwant: %#v", op, got, want)
	}
}

func hostnames(agents []db.Agent) []string {
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.Hostname
	}
	return names
}

// ─────────────────────────────────────────────────────────
// Health
// ─────────────────────────────────────────────────────────

func testHealth(t *testing.T, s db.DB) {
	ctx := context.Background()

	must(t, s.Ping(ctx), "Ping")

	switch s.Kind() {
	case "sqlite", "postgres":
	default:
		t.Errorf("Kind() = %q, want \"sqlite\" or \"postgres\"", s.Kind())
	}

	// The schema is documented as idempotent: re-running migrations on an
	// already-migrated store must succeed.
	must(t, s.RunMigrations(ctx), "RunMigrations (second run, idempotency)")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if l := s.NewLeader(log); l == nil {
		t.Error("NewLeader returned nil")
	}
}

// ─────────────────────────────────────────────────────────
// Agents: registration, get, upsert
// ─────────────────────────────────────────────────────────

func testAgentRegistration(t *testing.T, s db.DB) {
	ctx := context.Background()

	p := db.RegisterAgentParams{
		Hostname:     "reg-host-1",
		OS:           "linux",
		OSVersion:    "9.4",
		Arch:         "x86_64",
		AgentVersion: "0.25.1",
		ListenAddr:   "10.1.2.3:8443",
		Capabilities: []string{"exec", "facts"},
		Tags:         map[string]string{"env": "prod", "zone": "east"},
		ExecEnabled:  true,
	}

	a := register(t, s, p)

	t.Run("RoundTrip", func(t *testing.T) {
		if a.ID == "" {
			t.Error("ID is empty")
		}
		if a.Hostname != p.Hostname {
			t.Errorf("Hostname = %q, want %q", a.Hostname, p.Hostname)
		}
		if a.OS != p.OS || a.OSVersion != p.OSVersion || a.Arch != p.Arch {
			t.Errorf("OS/OSVersion/Arch = %q/%q/%q, want %q/%q/%q",
				a.OS, a.OSVersion, a.Arch, p.OS, p.OSVersion, p.Arch)
		}
		if a.AgentVersion != p.AgentVersion {
			t.Errorf("AgentVersion = %q, want %q", a.AgentVersion, p.AgentVersion)
		}
		if a.ListenAddr != p.ListenAddr {
			t.Errorf("ListenAddr = %q, want %q", a.ListenAddr, p.ListenAddr)
		}
		if a.Role != "leaf" {
			t.Errorf("Role = %q, want default \"leaf\"", a.Role)
		}
		if !a.Online {
			t.Error("Online = false, want true after registration")
		}
		if a.ExecEnabled != p.ExecEnabled {
			t.Errorf("ExecEnabled = %v, want %v", a.ExecEnabled, p.ExecEnabled)
		}
		if !reflect.DeepEqual(a.Capabilities, p.Capabilities) {
			t.Errorf("Capabilities = %#v, want %#v", a.Capabilities, p.Capabilities)
		}
		if !reflect.DeepEqual(a.Tags, p.Tags) {
			t.Errorf("Tags = %#v, want %#v", a.Tags, p.Tags)
		}
		if a.ParentID != nil {
			t.Errorf("ParentID = %v, want nil", *a.ParentID)
		}
		if a.ServerPod != nil {
			t.Errorf("ServerPod = %v, want nil", *a.ServerPod)
		}
		if a.RegisteredAt.IsZero() || a.LastSeenAt.IsZero() {
			t.Errorf("RegisteredAt/LastSeenAt zero: %v / %v", a.RegisteredAt, a.LastSeenAt)
		}
		if !closeTimes(a.RegisteredAt, time.Now()) {
			t.Errorf("RegisteredAt = %v, not close to now", a.RegisteredAt)
		}
	})

	t.Run("GetAgent", func(t *testing.T) {
		got, err := s.GetAgent(ctx, a.ID)
		must(t, err, "GetAgent")
		if got.ID != a.ID || got.Hostname != a.Hostname {
			t.Errorf("GetAgent = %q/%q, want %q/%q", got.ID, got.Hostname, a.ID, a.Hostname)
		}
		if !reflect.DeepEqual(got.Tags, p.Tags) {
			t.Errorf("Tags = %#v, want %#v", got.Tags, p.Tags)
		}
		if !reflect.DeepEqual(got.Capabilities, p.Capabilities) {
			t.Errorf("Capabilities = %#v, want %#v", got.Capabilities, p.Capabilities)
		}
	})

	t.Run("GetAgentMissing", func(t *testing.T) {
		_, err := s.GetAgent(ctx, "no-such-agent-id")
		wantNoRows(t, err, "GetAgent(missing)")
	})

	t.Run("GetAgentByHostname", func(t *testing.T) {
		got, err := s.GetAgentByHostname(ctx, p.Hostname)
		must(t, err, "GetAgentByHostname")
		if got.ID != a.ID {
			t.Errorf("ID = %q, want %q", got.ID, a.ID)
		}
		_, err = s.GetAgentByHostname(ctx, "no-such-host")
		wantNoRows(t, err, "GetAgentByHostname(missing)")
	})

	t.Run("UpsertSameHostname", func(t *testing.T) {
		p2 := p
		p2.OSVersion = "9.5"
		p2.Arch = "aarch64"
		p2.AgentVersion = "0.26.0"
		p2.ListenAddr = "10.1.2.4:8443"
		p2.Capabilities = []string{"exec"}
		p2.Tags = map[string]string{"env": "dev", "extra": "x"}
		p2.ExecEnabled = false

		b := register(t, s, p2)
		if b.ID != a.ID {
			t.Errorf("upsert changed ID: %q -> %q", a.ID, b.ID)
		}
		if b.OSVersion != "9.5" || b.Arch != "aarch64" || b.AgentVersion != "0.26.0" ||
			b.ListenAddr != "10.1.2.4:8443" {
			t.Errorf("upsert did not update fields: %+v", b)
		}
		if b.ExecEnabled {
			t.Error("ExecEnabled = true, want false after upsert")
		}
		if !b.Online {
			t.Error("Online = false, want true after re-registration")
		}
		if !reflect.DeepEqual(b.Capabilities, []string{"exec"}) {
			t.Errorf("Capabilities = %#v, want [exec]", b.Capabilities)
		}
		// Tags are merged: new keys overwrite, unmentioned old keys survive.
		wantTags := map[string]string{"env": "dev", "zone": "east", "extra": "x"}
		if !reflect.DeepEqual(b.Tags, wantTags) {
			t.Errorf("merged Tags = %#v, want %#v", b.Tags, wantTags)
		}
	})

	t.Run("UpsertNilTags", func(t *testing.T) {
		p4 := p
		p4.Tags = nil
		b, err := s.RegisterAgent(ctx, p4)
		must(t, err, "RegisterAgent(nil tags)")
		if b.ID != a.ID {
			t.Errorf("upsert changed ID: %q -> %q", a.ID, b.ID)
		}
		// The row must still be readable and keep its existing tags.
		got, err := s.GetAgent(ctx, a.ID)
		must(t, err, "GetAgent after nil-tags upsert")
		if len(got.Tags) == 0 {
			t.Errorf("Tags = %#v, want prior tags preserved", got.Tags)
		}
	})

	t.Run("EmptySliceFields", func(t *testing.T) {
		p3 := agentParams("reg-host-empty")
		p3.Capabilities = nil
		p3.Tags = map[string]string{}
		c := register(t, s, p3)
		if c.Capabilities == nil || len(c.Capabilities) != 0 {
			t.Errorf("Capabilities = %#v, want non-nil empty slice", c.Capabilities)
		}
		if c.Tags == nil || len(c.Tags) != 0 {
			t.Errorf("Tags = %#v, want non-nil empty map", c.Tags)
		}
	})
}

// ─────────────────────────────────────────────────────────
// Agents: listing and filters
// ─────────────────────────────────────────────────────────

func testAgentListing(t *testing.T, s db.DB) {
	ctx := context.Background()

	pa := agentParams("list-a")
	pa.Tags = map[string]string{"env": "prod"}
	a := register(t, s, pa)

	pb := agentParams("list-b")
	pb.Tags = map[string]string{"env": "dev", "tier": "web"}
	b := register(t, s, pb)

	pc := agentParams("list-c")
	pc.Tags = map[string]string{"env": "dev"}
	c := register(t, s, pc)
	must(t, s.SetAgentRole(ctx, c.ID, "relay"), "SetAgentRole")
	must(t, s.SetAgentOffline(ctx, c.ID), "SetAgentOffline")
	must(t, s.SetAgentParent(ctx, b.ID, a.ID), "SetAgentParent")

	list := func(f db.ListAgentsFilter) []db.Agent {
		t.Helper()
		agents, err := s.ListAgents(ctx, f)
		must(t, err, "ListAgents")
		return agents
	}

	t.Run("All", func(t *testing.T) {
		got := hostnames(list(db.ListAgentsFilter{}))
		want := []string{"list-a", "list-b", "list-c"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ListAgents = %v, want %v (sorted by hostname)", got, want)
		}
	})

	t.Run("Online", func(t *testing.T) {
		online := true
		got := hostnames(list(db.ListAgentsFilter{Online: &online}))
		if !reflect.DeepEqual(got, []string{"list-a", "list-b"}) {
			t.Errorf("online filter = %v, want [list-a list-b]", got)
		}
		offline := false
		got = hostnames(list(db.ListAgentsFilter{Online: &offline}))
		if !reflect.DeepEqual(got, []string{"list-c"}) {
			t.Errorf("offline filter = %v, want [list-c]", got)
		}
	})

	t.Run("Role", func(t *testing.T) {
		got := hostnames(list(db.ListAgentsFilter{Role: "relay"}))
		if !reflect.DeepEqual(got, []string{"list-c"}) {
			t.Errorf("role filter = %v, want [list-c]", got)
		}
		if got := list(db.ListAgentsFilter{Role: "no-such-role"}); len(got) != 0 {
			t.Errorf("bogus role filter returned %v", hostnames(got))
		}
	})

	t.Run("ParentID", func(t *testing.T) {
		got := hostnames(list(db.ListAgentsFilter{ParentID: a.ID}))
		if !reflect.DeepEqual(got, []string{"list-b"}) {
			t.Errorf("parent filter = %v, want [list-b]", got)
		}
	})

	t.Run("TagExists", func(t *testing.T) {
		got := hostnames(list(db.ListAgentsFilter{Tag: "tier"}))
		if !reflect.DeepEqual(got, []string{"list-b"}) {
			t.Errorf("tag-exists filter = %v, want [list-b]", got)
		}
	})

	t.Run("TagValue", func(t *testing.T) {
		got := hostnames(list(db.ListAgentsFilter{Tag: "env", TagValue: "prod"}))
		if !reflect.DeepEqual(got, []string{"list-a"}) {
			t.Errorf("tag=value filter = %v, want [list-a]", got)
		}
		if got := list(db.ListAgentsFilter{Tag: "env", TagValue: "nope"}); len(got) != 0 {
			t.Errorf("tag=bogus filter returned %v", hostnames(got))
		}
	})
}

// ─────────────────────────────────────────────────────────
// Agents: state transitions
// ─────────────────────────────────────────────────────────

func testAgentState(t *testing.T, s db.DB) {
	ctx := context.Background()

	a := register(t, s, agentParams("state-a"))

	t.Run("OfflineAndHeartbeat", func(t *testing.T) {
		must(t, s.SetAgentOffline(ctx, a.ID), "SetAgentOffline")
		got, err := s.GetAgent(ctx, a.ID)
		must(t, err, "GetAgent")
		if got.Online {
			t.Error("agent still online after SetAgentOffline")
		}

		must(t, s.UpdateAgentHeartbeat(ctx, a.ID), "UpdateAgentHeartbeat")
		got, err = s.GetAgent(ctx, a.ID)
		must(t, err, "GetAgent")
		if !got.Online {
			t.Error("agent not online after UpdateAgentHeartbeat")
		}

		wantNoRows(t, s.UpdateAgentHeartbeat(ctx, "missing"), "UpdateAgentHeartbeat(missing)")
		wantNoRows(t, s.SetAgentOffline(ctx, "missing"), "SetAgentOffline(missing)")
	})

	t.Run("Role", func(t *testing.T) {
		must(t, s.SetAgentRole(ctx, a.ID, "zone_leader"), "SetAgentRole")
		got, err := s.GetAgent(ctx, a.ID)
		must(t, err, "GetAgent")
		if got.Role != "zone_leader" {
			t.Errorf("Role = %q, want zone_leader", got.Role)
		}
		wantNoRows(t, s.SetAgentRole(ctx, "missing", "leaf"), "SetAgentRole(missing)")
	})

	b := register(t, s, agentParams("state-b"))

	t.Run("Parent", func(t *testing.T) {
		must(t, s.SetAgentParent(ctx, b.ID, a.ID), "SetAgentParent")
		got, err := s.GetAgent(ctx, b.ID)
		must(t, err, "GetAgent")
		if got.ParentID == nil || *got.ParentID != a.ID {
			t.Errorf("ParentID = %v, want %q", got.ParentID, a.ID)
		}

		must(t, s.SetAgentParent(ctx, b.ID, ""), "SetAgentParent(clear)")
		got, err = s.GetAgent(ctx, b.ID)
		must(t, err, "GetAgent")
		if got.ParentID != nil {
			t.Errorf("ParentID = %v, want nil after clearing", *got.ParentID)
		}

		wantNoRows(t, s.SetAgentParent(ctx, "missing", a.ID), "SetAgentParent(missing agent)")

		if err := s.SetAgentParent(ctx, b.ID, "no-such-parent"); err == nil {
			t.Error("SetAgentParent with nonexistent parent succeeded, want FK error")
		}
	})

	t.Run("Tags", func(t *testing.T) {
		must(t, s.UpdateAgentTags(ctx, a.ID, map[string]string{"x": "1"}), "UpdateAgentTags")
		got, err := s.GetAgent(ctx, a.ID)
		must(t, err, "GetAgent")
		// UpdateAgentTags replaces (unlike RegisterAgent's merge).
		if !reflect.DeepEqual(got.Tags, map[string]string{"x": "1"}) {
			t.Errorf("Tags = %#v, want map[x:1] (replace semantics)", got.Tags)
		}

		must(t, s.UpdateAgentTags(ctx, a.ID, nil), "UpdateAgentTags(nil)")
		got, err = s.GetAgent(ctx, a.ID)
		must(t, err, "GetAgent")
		if got.Tags == nil || len(got.Tags) != 0 {
			t.Errorf("Tags = %#v, want non-nil empty map after nil update", got.Tags)
		}

		wantNoRows(t, s.UpdateAgentTags(ctx, "missing", map[string]string{"a": "b"}),
			"UpdateAgentTags(missing)")
	})

	t.Run("Delete", func(t *testing.T) {
		must(t, s.DeleteAgent(ctx, b.ID), "DeleteAgent")
		_, err := s.GetAgent(ctx, b.ID)
		wantNoRows(t, err, "GetAgent(deleted)")
		wantNoRows(t, s.DeleteAgent(ctx, b.ID), "DeleteAgent(again)")
	})

	t.Run("DeleteParentClearsChild", func(t *testing.T) {
		p := register(t, s, agentParams("state-parent"))
		ch := register(t, s, agentParams("state-child"))
		must(t, s.SetAgentParent(ctx, ch.ID, p.ID), "SetAgentParent")
		must(t, s.DeleteAgent(ctx, p.ID), "DeleteAgent(parent)")
		got, err := s.GetAgent(ctx, ch.ID)
		must(t, err, "GetAgent(child)")
		if got.ParentID != nil {
			t.Errorf("child ParentID = %v, want nil after parent delete", *got.ParentID)
		}
	})

	t.Run("MarkStaleAgentsOffline", func(t *testing.T) {
		register(t, s, agentParams("state-stale-1"))
		s2 := register(t, s, agentParams("state-stale-2"))
		must(t, s.SetAgentOffline(ctx, s2.ID), "SetAgentOffline")

		// A large threshold puts the cutoff far in the past: nothing is stale.
		n, err := s.MarkStaleAgentsOffline(ctx, time.Hour)
		must(t, err, "MarkStaleAgentsOffline(1h)")
		if n != 0 {
			t.Errorf("MarkStaleAgentsOffline(1h) = %d, want 0", n)
		}

		// A negative threshold puts the cutoff in the future: every online
		// agent is stale. Already-offline agents must not be re-counted.
		online := true
		agents, err := s.ListAgents(ctx, db.ListAgentsFilter{Online: &online})
		must(t, err, "ListAgents(online)")

		n, err = s.MarkStaleAgentsOffline(ctx, -5*time.Second)
		must(t, err, "MarkStaleAgentsOffline(-5s)")
		if n != int64(len(agents)) {
			t.Errorf("MarkStaleAgentsOffline(-5s) = %d, want %d (online count)", n, len(agents))
		}

		agents, err = s.ListAgents(ctx, db.ListAgentsFilter{Online: &online})
		must(t, err, "ListAgents(online)")
		if len(agents) != 0 {
			t.Errorf("agents still online after stale sweep: %v", hostnames(agents))
		}
	})
}

// ─────────────────────────────────────────────────────────
// Agents: subtree operations
// ─────────────────────────────────────────────────────────

func testAgentTree(t *testing.T, s db.DB) {
	ctx := context.Background()

	root := register(t, s, agentParams("tree-root"))
	c1 := register(t, s, agentParams("tree-c1"))
	c2 := register(t, s, agentParams("tree-c2"))
	g1 := register(t, s, agentParams("tree-g1"))
	root2 := register(t, s, agentParams("tree-root2"))

	must(t, s.SetAgentParent(ctx, c1.ID, root.ID), "SetAgentParent(c1)")
	must(t, s.SetAgentParent(ctx, c2.ID, root.ID), "SetAgentParent(c2)")
	must(t, s.SetAgentParent(ctx, g1.ID, c1.ID), "SetAgentParent(g1)")

	must(t, s.TouchAgentTree(ctx, root.ID), "TouchAgentTree")

	n, err := s.MarkAgentTreeOffline(ctx, root.ID)
	must(t, err, "MarkAgentTreeOffline")
	if n != 4 {
		t.Errorf("MarkAgentTreeOffline = %d, want 4 (root + 2 children + grandchild)", n)
	}

	for _, id := range []string{root.ID, c1.ID, c2.ID, g1.ID} {
		got, err := s.GetAgent(ctx, id)
		must(t, err, "GetAgent")
		if got.Online {
			t.Errorf("agent %s still online after MarkAgentTreeOffline", got.Hostname)
		}
	}

	got, err := s.GetAgent(ctx, root2.ID)
	must(t, err, "GetAgent(root2)")
	if !got.Online {
		t.Error("unrelated tree root marked offline")
	}

	n, err = s.MarkAgentTreeOffline(ctx, root.ID)
	must(t, err, "MarkAgentTreeOffline(again)")
	if n != 0 {
		t.Errorf("second MarkAgentTreeOffline = %d, want 0", n)
	}

	n, err = s.MarkAgentTreeOffline(ctx, "no-such-root")
	must(t, err, "MarkAgentTreeOffline(missing)")
	if n != 0 {
		t.Errorf("MarkAgentTreeOffline(missing) = %d, want 0", n)
	}
}

// ─────────────────────────────────────────────────────────
// Facts
// ─────────────────────────────────────────────────────────

func testFacts(t *testing.T, s db.DB) {
	ctx := context.Background()

	a1 := register(t, s, agentParams("facts-a"))
	a2 := register(t, s, agentParams("facts-b"))

	cpuData := map[string]any{"cpus": float64(8), "model": "EPYC", "flags": []any{"sse", "avx"}}
	memData := map[string]any{"total_mb": float64(32768)}
	cpuData2 := map[string]any{"cpus": float64(4)}

	must(t, s.UpsertFact(ctx, a1.ID, "cpu", cpuData), "UpsertFact(a1,cpu)")
	must(t, s.UpsertFact(ctx, a1.ID, "memory", memData), "UpsertFact(a1,memory)")
	must(t, s.UpsertFact(ctx, a2.ID, "cpu", cpuData2), "UpsertFact(a2,cpu)")

	t.Run("GetFacts", func(t *testing.T) {
		facts, err := s.GetFacts(ctx, a1.ID)
		must(t, err, "GetFacts")
		if len(facts) != 2 {
			t.Fatalf("GetFacts = %d facts, want 2", len(facts))
		}
		// Ordered by module: cpu before memory.
		if facts[0].Module != "cpu" || facts[1].Module != "memory" {
			t.Errorf("module order = [%s %s], want [cpu memory]", facts[0].Module, facts[1].Module)
		}
		checkJSONEq(t, facts[0].Data, cpuData, "cpu fact")
		checkJSONEq(t, facts[1].Data, memData, "memory fact")
		for _, f := range facts {
			if f.AgentID != a1.ID {
				t.Errorf("AgentID = %q, want %q", f.AgentID, a1.ID)
			}
			if f.CollectedAt.IsZero() || !closeTimes(f.CollectedAt, time.Now()) {
				t.Errorf("CollectedAt = %v, want close to now", f.CollectedAt)
			}
		}
	})

	t.Run("UpsertOverwrites", func(t *testing.T) {
		newData := map[string]any{"cpus": float64(16), "model": "EPYC2"}
		must(t, s.UpsertFact(ctx, a1.ID, "cpu", newData), "UpsertFact(overwrite)")
		facts, err := s.GetFacts(ctx, a1.ID)
		must(t, err, "GetFacts")
		if len(facts) != 2 {
			t.Fatalf("after overwrite GetFacts = %d facts, want 2 (no duplicate row)", len(facts))
		}
		checkJSONEq(t, facts[0].Data, newData, "overwritten cpu fact")
	})

	t.Run("GetFactsByModule", func(t *testing.T) {
		facts, err := s.GetFactsByModule(ctx, "cpu")
		must(t, err, "GetFactsByModule(cpu)")
		if len(facts) != 2 {
			t.Fatalf("cpu facts = %d, want 2", len(facts))
		}
		agents := map[string]bool{}
		for _, f := range facts {
			agents[f.AgentID] = true
		}
		if !agents[a1.ID] || !agents[a2.ID] {
			t.Errorf("cpu facts cover agents %v, want both %q and %q", agents, a1.ID, a2.ID)
		}

		facts, err = s.GetFactsByModule(ctx, "memory")
		must(t, err, "GetFactsByModule(memory)")
		if len(facts) != 1 || facts[0].AgentID != a1.ID {
			t.Errorf("memory facts = %d rows, want exactly 1 for a1", len(facts))
		}
	})

	t.Run("GetAllFacts", func(t *testing.T) {
		facts, err := s.GetAllFacts(ctx)
		must(t, err, "GetAllFacts")
		if len(facts) != 3 {
			t.Errorf("GetAllFacts = %d, want 3", len(facts))
		}
	})

	t.Run("GetFactsUnknownAgent", func(t *testing.T) {
		facts, err := s.GetFacts(ctx, "no-such-agent")
		must(t, err, "GetFacts(unknown)")
		if len(facts) != 0 {
			t.Errorf("GetFacts(unknown) = %d facts, want 0", len(facts))
		}
	})

	t.Run("BulkUpsert", func(t *testing.T) {
		must(t, s.BulkUpsertFacts(ctx, nil), "BulkUpsertFacts(empty)")

		collected := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
		// 205 rows crosses the sqlite 200-row chunk boundary; include an
		// overwrite of a2's existing cpu fact in the same batch.
		rows := make([]db.FactRow, 0, 206)
		for i := 0; i < 205; i++ {
			rows = append(rows, db.FactRow{
				AgentID:     a2.ID,
				Module:      fmt.Sprintf("bulk-%03d", i),
				Data:        []byte(fmt.Sprintf(`{"i":%d}`, i)),
				CollectedAt: collected,
			})
		}
		rows = append(rows, db.FactRow{
			AgentID:     a2.ID,
			Module:      "cpu",
			Data:        []byte(`{"cpus":64}`),
			CollectedAt: collected,
		})
		must(t, s.BulkUpsertFacts(ctx, rows), "BulkUpsertFacts")

		facts, err := s.GetFacts(ctx, a2.ID)
		must(t, err, "GetFacts(a2)")
		if len(facts) != 206 {
			t.Fatalf("a2 facts = %d, want 206 (205 bulk + overwritten cpu)", len(facts))
		}
		byModule := map[string]db.Fact{}
		for _, f := range facts {
			byModule[f.Module] = f
		}
		checkJSONEq(t, byModule["cpu"].Data, map[string]any{"cpus": float64(64)}, "bulk cpu overwrite")
		checkJSONEq(t, byModule["bulk-204"].Data, map[string]any{"i": float64(204)}, "bulk row 204")
		if got := byModule["bulk-204"].CollectedAt; !got.Equal(collected) {
			t.Errorf("bulk CollectedAt = %v, want %v", got, collected)
		}
	})

	t.Run("FactTTL", func(t *testing.T) {
		ttl, err := s.GetFactTTL(ctx, "cpu")
		must(t, err, "GetFactTTL(cpu)")
		if ttl != 3600 {
			t.Errorf("cpu TTL = %d, want 3600", ttl)
		}
		ttl, err = s.GetFactTTL(ctx, "no-such-module")
		must(t, err, "GetFactTTL(unknown)")
		if ttl != 900 {
			t.Errorf("unknown-module TTL = %d, want _default 900", ttl)
		}
	})

	t.Run("CascadeOnAgentDelete", func(t *testing.T) {
		must(t, s.DeleteAgent(ctx, a1.ID), "DeleteAgent(a1)")
		facts, err := s.GetFacts(ctx, a1.ID)
		must(t, err, "GetFacts(deleted agent)")
		if len(facts) != 0 {
			t.Errorf("facts survived agent delete: %d rows", len(facts))
		}
	})
}

// ─────────────────────────────────────────────────────────
// Tokens
// ─────────────────────────────────────────────────────────

func testTokens(t *testing.T, s db.DB) {
	ctx := context.Background()

	tok1, err := s.CreateToken(ctx, "admin-token", "admin", nil)
	must(t, err, "CreateToken(admin)")

	t.Run("PlaintextShape", func(t *testing.T) {
		if len(tok1) != 64 {
			t.Errorf("plaintext length = %d, want 64 hex chars", len(tok1))
		}
		if strings.Trim(tok1, "0123456789abcdef") != "" {
			t.Errorf("plaintext %q is not lowercase hex", tok1)
		}
	})

	t.Run("Validate", func(t *testing.T) {
		v, err := s.ValidateToken(ctx, tok1)
		must(t, err, "ValidateToken")
		if v.Name != "admin-token" || v.Scope != "admin" {
			t.Errorf("token = %q/%q, want admin-token/admin", v.Name, v.Scope)
		}
		if v.ID == "" {
			t.Error("token ID empty")
		}
		if len(v.AAPUsers) != 0 {
			t.Errorf("AAPUsers = %v, want empty for unrestricted token", v.AAPUsers)
		}
		if v.CreatedAt.IsZero() {
			t.Error("CreatedAt is zero, want the creation timestamp")
		}
	})

	tok2, err := s.CreateToken(ctx, "ro-token", "readonly", []string{" alice ", "", "bob"})
	must(t, err, "CreateToken(readonly)")

	t.Run("ScopeAndAAPUsers", func(t *testing.T) {
		v, err := s.ValidateToken(ctx, tok2)
		must(t, err, "ValidateToken(ro)")
		if v.Scope != "readonly" {
			t.Errorf("Scope = %q, want readonly", v.Scope)
		}
		// Entries are trimmed and empties dropped by EncodeAAPUsers.
		if !reflect.DeepEqual(v.AAPUsers, []string{"alice", "bob"}) {
			t.Errorf("AAPUsers = %#v, want [alice bob]", v.AAPUsers)
		}
	})

	t.Run("ValidateRejects", func(t *testing.T) {
		// Too short to even carry a prefix.
		_, err := s.ValidateToken(ctx, "abc")
		wantNoRows(t, err, "ValidateToken(short)")

		// Correct prefix, wrong remainder: found by prefix, rejected by bcrypt.
		bad := tok1[:8] + strings.Repeat("f", 56)
		if bad == tok1 {
			bad = tok1[:8] + strings.Repeat("e", 56)
		}
		_, err = s.ValidateToken(ctx, bad)
		wantNoRows(t, err, "ValidateToken(wrong suffix)")

		// Unknown token entirely.
		_, err = s.ValidateToken(ctx, strings.Repeat("0", 64))
		wantNoRows(t, err, "ValidateToken(unknown)")
	})

	_, err = s.CreateToken(ctx, "unused-token", "admin", nil)
	must(t, err, "CreateToken(unused)")

	t.Run("List", func(t *testing.T) {
		tokens, err := s.ListTokens(ctx)
		must(t, err, "ListTokens")
		if len(tokens) != 3 {
			t.Fatalf("ListTokens = %d, want 3", len(tokens))
		}
		byName := map[string]db.Token{}
		for _, tk := range tokens {
			byName[tk.Name] = tk
		}
		if byName["admin-token"].Scope != "admin" || byName["ro-token"].Scope != "readonly" {
			t.Errorf("scopes wrong in listing: %+v", byName)
		}
		if !reflect.DeepEqual(byName["ro-token"].AAPUsers, []string{"alice", "bob"}) {
			t.Errorf("listed AAPUsers = %#v, want [alice bob]", byName["ro-token"].AAPUsers)
		}
		// Validation stamps last_used.
		if byName["admin-token"].LastUsed == nil {
			t.Error("admin-token LastUsed nil, want set after successful validation")
		}
		if byName["unused-token"].LastUsed != nil {
			t.Errorf("unused-token LastUsed = %v, want nil", byName["unused-token"].LastUsed)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		must(t, s.DeleteToken(ctx, "ro-token"), "DeleteToken")
		_, err := s.ValidateToken(ctx, tok2)
		wantNoRows(t, err, "ValidateToken(deleted)")

		tokens, err := s.ListTokens(ctx)
		must(t, err, "ListTokens")
		if len(tokens) != 2 {
			t.Errorf("ListTokens after delete = %d, want 2", len(tokens))
		}

		wantNoRows(t, s.DeleteToken(ctx, "no-such-token"), "DeleteToken(missing)")
	})
}

// ─────────────────────────────────────────────────────────
// Queries
// ─────────────────────────────────────────────────────────

func testQueries(t *testing.T, s db.DB) {
	ctx := context.Background()

	q1, err := s.CreateQuery(ctx, "os == 'linux'", "ops", 5)
	must(t, err, "CreateQuery")

	t.Run("CreateRoundTrip", func(t *testing.T) {
		if q1.ID == "" {
			t.Error("ID empty")
		}
		if q1.RawQuery != "os == 'linux'" {
			t.Errorf("RawQuery = %q", q1.RawQuery)
		}
		if q1.SubmittedBy == nil || *q1.SubmittedBy != "ops" {
			t.Errorf("SubmittedBy = %v, want ops", q1.SubmittedBy)
		}
		if q1.Status != "running" {
			t.Errorf("Status = %q, want running", q1.Status)
		}
		if q1.TargetCount != 5 {
			t.Errorf("TargetCount = %d, want 5", q1.TargetCount)
		}
		if q1.SuccessCount != 0 || q1.ErrorCount != 0 || q1.TimeoutCount != 0 {
			t.Errorf("counts = %d/%d/%d, want zeros", q1.SuccessCount, q1.ErrorCount, q1.TimeoutCount)
		}
		if q1.CompletedAt != nil {
			t.Errorf("CompletedAt = %v, want nil", q1.CompletedAt)
		}
		if q1.SubmittedAt.IsZero() || !closeTimes(q1.SubmittedAt, time.Now()) {
			t.Errorf("SubmittedAt = %v, want close to now", q1.SubmittedAt)
		}
	})

	t.Run("EmptySubmitter", func(t *testing.T) {
		q2, err := s.CreateQuery(ctx, "hostname == 'x'", "", 0)
		must(t, err, "CreateQuery(empty submitter)")
		if q2.SubmittedBy != nil {
			t.Errorf("SubmittedBy = %v, want nil for empty submitter", *q2.SubmittedBy)
		}
	})

	t.Run("UpdateStatus", func(t *testing.T) {
		must(t, s.UpdateQueryStatus(ctx, q1.ID, "completed", 3, 1, 1), "UpdateQueryStatus")
		queries, err := s.ListQueries(ctx, 10)
		must(t, err, "ListQueries")
		var got *db.Query
		for i := range queries {
			if queries[i].ID == q1.ID {
				got = &queries[i]
			}
		}
		if got == nil {
			t.Fatal("updated query not found in listing")
		}
		if got.Status != "completed" || got.SuccessCount != 3 || got.ErrorCount != 1 || got.TimeoutCount != 1 {
			t.Errorf("after update: %+v", got)
		}
		if got.CompletedAt == nil {
			t.Error("CompletedAt nil after UpdateQueryStatus")
		}

		wantNoRows(t, s.UpdateQueryStatus(ctx, "no-such-query", "failed", 0, 0, 0),
			"UpdateQueryStatus(missing)")
	})

	t.Run("ListLimitAndOrder", func(t *testing.T) {
		_, err := s.CreateQuery(ctx, "third", "ops", 1)
		must(t, err, "CreateQuery(third)")

		queries, err := s.ListQueries(ctx, 2)
		must(t, err, "ListQueries(2)")
		if len(queries) != 2 {
			t.Errorf("ListQueries(2) = %d rows, want 2", len(queries))
		}

		queries, err = s.ListQueries(ctx, 10)
		must(t, err, "ListQueries(10)")
		if len(queries) != 3 {
			t.Errorf("ListQueries(10) = %d rows, want 3", len(queries))
		}
		for i := 1; i < len(queries); i++ {
			if queries[i].SubmittedAt.After(queries[i-1].SubmittedAt) {
				t.Errorf("queries not in descending SubmittedAt order at index %d", i)
			}
		}
	})
}

// ─────────────────────────────────────────────────────────
// Exec log
// ─────────────────────────────────────────────────────────

func strPtr(v string) *string { return &v }

func testExecLog(t *testing.T, s db.DB) {
	ctx := context.Background()

	a := register(t, s, agentParams("exec-a"))
	b := register(t, s, agentParams("exec-b"))

	started := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Minute)
	finished := started.Add(30 * time.Second)
	rc := 7
	success := true

	full := db.ExecLog{
		RequestID:      "req-full",
		AgentID:        a.ID,
		Hostname:       a.Hostname,
		Operation:      "exec_command",
		Command:        strPtr("uname -a"),
		DestPath:       strPtr("/tmp/out"),
		SrcPath:        strPtr("/etc/hosts"),
		Become:         true,
		BecomeUser:     strPtr("root"),
		RC:             &rc,
		Success:        &success,
		Error:          strPtr("partial failure"),
		AAPJobID:       strPtr("job-123"),
		AAPJobTemplate: strPtr("Deploy App"),
		AAPUser:        strPtr("alice"),
		StartedAt:      &started,
		FinishedAt:     &finished,
	}

	created, err := s.CreateExecLog(ctx, full)
	must(t, err, "CreateExecLog(full)")

	t.Run("FullRoundTrip", func(t *testing.T) {
		if created.ID == "" {
			t.Error("ID empty")
		}
		if created.RequestID != "req-full" || created.AgentID != a.ID ||
			created.Hostname != a.Hostname || created.Operation != "exec_command" {
			t.Errorf("core fields: %+v", created)
		}
		checkStr := func(name string, got *string, want string) {
			t.Helper()
			if got == nil || *got != want {
				t.Errorf("%s = %v, want %q", name, got, want)
			}
		}
		checkStr("Command", created.Command, "uname -a")
		checkStr("DestPath", created.DestPath, "/tmp/out")
		checkStr("SrcPath", created.SrcPath, "/etc/hosts")
		checkStr("BecomeUser", created.BecomeUser, "root")
		checkStr("Error", created.Error, "partial failure")
		checkStr("AAPJobID", created.AAPJobID, "job-123")
		checkStr("AAPJobTemplate", created.AAPJobTemplate, "Deploy App")
		checkStr("AAPUser", created.AAPUser, "alice")
		if !created.Become {
			t.Error("Become = false, want true")
		}
		if created.RC == nil || *created.RC != 7 {
			t.Errorf("RC = %v, want 7", created.RC)
		}
		if created.Success == nil || !*created.Success {
			t.Errorf("Success = %v, want true", created.Success)
		}
		if created.StartedAt == nil || !created.StartedAt.Equal(started) {
			t.Errorf("StartedAt = %v, want %v", created.StartedAt, started)
		}
		if created.FinishedAt == nil || !created.FinishedAt.Equal(finished) {
			t.Errorf("FinishedAt = %v, want %v", created.FinishedAt, finished)
		}
		if created.CreatedAt.IsZero() || !closeTimes(created.CreatedAt, time.Now()) {
			t.Errorf("CreatedAt = %v, want close to now", created.CreatedAt)
		}
	})

	t.Run("MinimalRoundTrip", func(t *testing.T) {
		minimal, err := s.CreateExecLog(ctx, db.ExecLog{
			RequestID: "req-min",
			AgentID:   a.ID,
			Hostname:  a.Hostname,
			Operation: "put_file",
		})
		must(t, err, "CreateExecLog(minimal)")
		if minimal.Command != nil || minimal.DestPath != nil || minimal.SrcPath != nil ||
			minimal.BecomeUser != nil || minimal.RC != nil || minimal.Success != nil ||
			minimal.Error != nil || minimal.AAPJobID != nil || minimal.AAPJobTemplate != nil ||
			minimal.AAPUser != nil || minimal.StartedAt != nil || minimal.FinishedAt != nil {
			t.Errorf("optional fields not nil: %+v", minimal)
		}
		if minimal.Become {
			t.Error("Become = true, want false")
		}
		if minimal.CreatedAt.IsZero() {
			t.Error("CreatedAt zero")
		}
	})

	t.Run("UpdateByRequestID", func(t *testing.T) {
		_, err := s.CreateExecLog(ctx, db.ExecLog{
			RequestID: "req-upd", AgentID: a.ID, Hostname: a.Hostname, Operation: "exec_command",
		})
		must(t, err, "CreateExecLog(req-upd)")

		fin := time.Now().UTC().Truncate(time.Second)
		urc := 0
		must(t, s.UpdateExecLog(ctx, "req-upd", &urc, true, "", fin), "UpdateExecLog")

		logs, err := s.ListExecLogsByAgent(ctx, a.ID, 100)
		must(t, err, "ListExecLogsByAgent")
		var got *db.ExecLog
		for i := range logs {
			if logs[i].RequestID == "req-upd" {
				got = &logs[i]
			}
		}
		if got == nil {
			t.Fatal("updated entry not found")
		}
		if got.RC == nil || *got.RC != 0 {
			t.Errorf("RC = %v, want 0", got.RC)
		}
		if got.Success == nil || !*got.Success {
			t.Errorf("Success = %v, want true", got.Success)
		}
		if got.Error != nil {
			t.Errorf("Error = %v, want nil for empty message", *got.Error)
		}
		if got.FinishedAt == nil || !got.FinishedAt.Equal(fin) {
			t.Errorf("FinishedAt = %v, want %v", got.FinishedAt, fin)
		}
	})

	t.Run("UpdateWithError", func(t *testing.T) {
		_, err := s.CreateExecLog(ctx, db.ExecLog{
			RequestID: "req-err", AgentID: a.ID, Hostname: a.Hostname, Operation: "exec_command",
		})
		must(t, err, "CreateExecLog(req-err)")
		must(t, s.UpdateExecLog(ctx, "req-err", nil, false, "kaboom", time.Now().UTC()), "UpdateExecLog(err)")

		logs, err := s.ListExecLogsByAgent(ctx, a.ID, 100)
		must(t, err, "ListExecLogsByAgent")
		for _, l := range logs {
			if l.RequestID != "req-err" {
				continue
			}
			if l.Success == nil || *l.Success {
				t.Errorf("Success = %v, want false", l.Success)
			}
			if l.Error == nil || *l.Error != "kaboom" {
				t.Errorf("Error = %v, want kaboom", l.Error)
			}
			return
		}
		t.Fatal("req-err entry not found")
	})

	t.Run("Listing", func(t *testing.T) {
		_, err := s.CreateExecLog(ctx, db.ExecLog{
			RequestID: "req-b", AgentID: b.ID, Hostname: b.Hostname, Operation: "fetch_file",
		})
		must(t, err, "CreateExecLog(agent b)")

		logs, err := s.ListExecLogs(ctx, 100)
		must(t, err, "ListExecLogs(100)")
		if len(logs) != 5 {
			t.Errorf("ListExecLogs = %d entries, want 5", len(logs))
		}
		for i := 1; i < len(logs); i++ {
			if logs[i].CreatedAt.After(logs[i-1].CreatedAt) {
				t.Errorf("exec logs not in descending CreatedAt order at index %d", i)
			}
		}

		logs, err = s.ListExecLogs(ctx, 2)
		must(t, err, "ListExecLogs(2)")
		if len(logs) != 2 {
			t.Errorf("ListExecLogs(2) = %d entries, want 2", len(logs))
		}

		logs, err = s.ListExecLogsByAgent(ctx, b.ID, 100)
		must(t, err, "ListExecLogsByAgent(b)")
		if len(logs) != 1 || logs[0].RequestID != "req-b" {
			t.Errorf("agent b logs = %+v, want single req-b entry", logs)
		}

		logs, err = s.ListExecLogsByAgent(ctx, a.ID, 2)
		must(t, err, "ListExecLogsByAgent(a, limit 2)")
		if len(logs) != 2 {
			t.Errorf("agent a logs with limit 2 = %d, want 2", len(logs))
		}

		logs, err = s.ListExecLogsByJob(ctx, "job-123")
		must(t, err, "ListExecLogsByJob")
		if len(logs) != 1 || logs[0].RequestID != "req-full" {
			t.Errorf("job-123 logs = %d entries, want single req-full", len(logs))
		}

		logs, err = s.ListExecLogsByJob(ctx, "no-such-job")
		must(t, err, "ListExecLogsByJob(missing)")
		if len(logs) != 0 {
			t.Errorf("unknown job logs = %d, want 0", len(logs))
		}
	})
}

// ─────────────────────────────────────────────────────────
// Topology
// ─────────────────────────────────────────────────────────

func testTopology(t *testing.T, s db.DB) {
	ctx := context.Background()

	mk := func(hostname, listenAddr string) db.Agent {
		p := agentParams(hostname)
		p.ListenAddr = listenAddr
		return register(t, s, p)
	}

	zl := mk("topo-zl", "10.0.0.1:9000")
	relay := mk("topo-relay", "10.0.0.2:9000")
	leaf1 := mk("topo-leaf1", "10.0.0.3:9000")
	leaf2 := mk("topo-leaf2", "10.0.0.4:9000")
	noaddr := mk("topo-noaddr", "")
	orphan := mk("topo-orphan", "10.0.0.5:9000")
	relay2 := mk("topo-relay2", "10.0.0.6:9000")

	must(t, s.SetAgentRole(ctx, zl.ID, "zone_leader"), "SetAgentRole(zl)")
	must(t, s.SetAgentRole(ctx, relay.ID, "relay"), "SetAgentRole(relay)")
	must(t, s.SetAgentRole(ctx, relay2.ID, "relay"), "SetAgentRole(relay2)")
	must(t, s.SetAgentParent(ctx, relay.ID, zl.ID), "SetAgentParent(relay)")
	must(t, s.SetAgentParent(ctx, leaf1.ID, relay.ID), "SetAgentParent(leaf1)")
	must(t, s.SetAgentParent(ctx, leaf2.ID, relay.ID), "SetAgentParent(leaf2)")
	must(t, s.SetAgentParent(ctx, noaddr.ID, zl.ID), "SetAgentParent(noaddr)")
	_ = orphan

	t.Run("Counts", func(t *testing.T) {
		checkCount := func(op string, got int, err error, want int) {
			t.Helper()
			must(t, err, op)
			if got != want {
				t.Errorf("%s = %d, want %d", op, got, want)
			}
		}
		n, err := s.CountChildren(ctx, zl.ID)
		checkCount("CountChildren(zl)", n, err, 2)
		n, err = s.CountChildren(ctx, relay.ID)
		checkCount("CountChildren(relay)", n, err, 2)
		n, err = s.CountChildren(ctx, leaf1.ID)
		checkCount("CountChildren(leaf1)", n, err, 0)

		n, err = s.CountAgentsByRole(ctx, "zone_leader")
		checkCount("CountAgentsByRole(zone_leader)", n, err, 1)
		n, err = s.CountAgentsByRole(ctx, "relay")
		checkCount("CountAgentsByRole(relay)", n, err, 2)
		n, err = s.CountAgentsByRole(ctx, "leaf")
		checkCount("CountAgentsByRole(leaf)", n, err, 4)

		n, err = s.CountOnlineZoneLeaders(ctx)
		checkCount("CountOnlineZoneLeaders", n, err, 1)
	})

	t.Run("FindZoneLeader", func(t *testing.T) {
		got, err := s.FindZoneLeader(ctx, leaf1.ID)
		must(t, err, "FindZoneLeader(leaf1)")
		if got.ID != zl.ID {
			t.Errorf("zone leader of leaf1 = %q, want %q", got.Hostname, zl.Hostname)
		}
		got, err = s.FindZoneLeader(ctx, relay.ID)
		must(t, err, "FindZoneLeader(relay)")
		if got.ID != zl.ID {
			t.Errorf("zone leader of relay = %q, want %q", got.Hostname, zl.Hostname)
		}
		got, err = s.FindZoneLeader(ctx, zl.ID)
		must(t, err, "FindZoneLeader(zl itself)")
		if got.ID != zl.ID {
			t.Errorf("zone leader of zl = %q, want itself", got.Hostname)
		}
		_, err = s.FindZoneLeader(ctx, orphan.ID)
		wantNoRows(t, err, "FindZoneLeader(orphan)")
	})

	t.Run("FindChildOfParent", func(t *testing.T) {
		got, err := s.FindChildOfParent(ctx, zl.ID)
		must(t, err, "FindChildOfParent(zl)")
		// noaddr has no listen_addr, so relay is the only eligible child.
		if got.ID != relay.ID {
			t.Errorf("child of zl = %q, want %q", got.Hostname, relay.Hostname)
		}
		_, err = s.FindChildOfParent(ctx, leaf1.ID)
		wantNoRows(t, err, "FindChildOfParent(childless)")
	})

	t.Run("FindRelaysWithChildren", func(t *testing.T) {
		got, err := s.FindRelaysWithChildren(ctx)
		must(t, err, "FindRelaysWithChildren")
		if len(got) != 1 || got[0].ID != relay.ID {
			t.Errorf("relays with children = %v, want [%s] (relay2 is childless)",
				hostnames(got), relay.Hostname)
		}
	})

	t.Run("FindParentWithRoom", func(t *testing.T) {
		got, err := s.FindParentWithRoom(ctx, "zone_leader", 5)
		must(t, err, "FindParentWithRoom(zone_leader,5)")
		if got.ID != zl.ID {
			t.Errorf("parent with room = %q, want %q", got.Hostname, zl.Hostname)
		}

		// zl already has 2 online children, so maxChildren=2 leaves no room.
		_, err = s.FindParentWithRoom(ctx, "zone_leader", 2)
		wantNoRows(t, err, "FindParentWithRoom(zone_leader,2)")

		// Least-loaded relay wins: relay2 (0 children) over relay (2).
		got, err = s.FindParentWithRoom(ctx, "relay", 5)
		must(t, err, "FindParentWithRoom(relay,5)")
		if got.ID != relay2.ID {
			t.Errorf("relay with room = %q, want least-loaded %q", got.Hostname, relay2.Hostname)
		}
	})

	t.Run("FindShallowestParentWithRoom", func(t *testing.T) {
		got, err := s.FindShallowestParentWithRoom(ctx, 5)
		must(t, err, "FindShallowestParentWithRoom(5)")
		if got.ID != zl.ID {
			t.Errorf("shallowest = %q, want zone leader %q", got.Hostname, zl.Hostname)
		}

		// With maxChildren=2 both zl and relay are full; the shallowest
		// nodes with room are the depth-2 leaves.
		got, err = s.FindShallowestParentWithRoom(ctx, 2)
		must(t, err, "FindShallowestParentWithRoom(2)")
		if got.ID != leaf1.ID && got.ID != leaf2.ID {
			t.Errorf("shallowest with max 2 = %q, want one of the leaves", got.Hostname)
		}
	})

	t.Run("FindFallbackParents", func(t *testing.T) {
		got, err := s.FindFallbackParents(ctx, relay.ID, 5, 10)
		must(t, err, "FindFallbackParents")
		ids := map[string]bool{}
		for _, a := range got {
			ids[a.ID] = true
		}
		if ids[relay.ID] {
			t.Error("fallback parents include the primary parent")
		}
		if ids[noaddr.ID] {
			t.Error("fallback parents include an agent with no listen_addr")
		}
		if !ids[zl.ID] || !ids[leaf1.ID] || !ids[leaf2.ID] {
			t.Errorf("fallback parents = %v, want zl and both leaves", hostnames(got))
		}
		// relay2 and orphan are not reachable from any zone leader.
		if len(got) != 3 {
			t.Errorf("fallback parents = %d entries (%v), want 3", len(got), hostnames(got))
		}

		got, err = s.FindFallbackParents(ctx, relay.ID, 5, 1)
		must(t, err, "FindFallbackParents(count=1)")
		if len(got) != 1 {
			t.Errorf("count=1 returned %d entries", len(got))
		}
	})

	t.Run("WithTopologyLock", func(t *testing.T) {
		ran := false
		must(t, s.WithTopologyLock(ctx, func() error { ran = true; return nil }), "WithTopologyLock")
		if !ran {
			t.Error("fn was not invoked")
		}
		sentinel := errors.New("boom")
		if err := s.WithTopologyLock(ctx, func() error { return sentinel }); !errors.Is(err, sentinel) {
			t.Errorf("WithTopologyLock error = %v, want sentinel", err)
		}
	})

	// Keep last: takes the zone leader offline.
	t.Run("OfflineZoneLeader", func(t *testing.T) {
		must(t, s.SetAgentOffline(ctx, zl.ID), "SetAgentOffline(zl)")
		n, err := s.CountOnlineZoneLeaders(ctx)
		must(t, err, "CountOnlineZoneLeaders")
		if n != 0 {
			t.Errorf("online zone leaders = %d, want 0", n)
		}
		_, err = s.FindParentWithRoom(ctx, "zone_leader", 5)
		wantNoRows(t, err, "FindParentWithRoom(offline zl)")
		_, err = s.FindShallowestParentWithRoom(ctx, 5)
		wantNoRows(t, err, "FindShallowestParentWithRoom(no online zl)")
	})
}

// testImbalance uses its own store to engineer a deterministic imbalance.
func testImbalance(t *testing.T, s db.DB) {
	ctx := context.Background()

	p := agentParams("imb-zl")
	p.ListenAddr = "10.9.0.1:9000"
	zl := register(t, s, p)
	must(t, s.SetAgentRole(ctx, zl.ID, "zone_leader"), "SetAgentRole(zl)")

	for i := 0; i < 6; i++ {
		lp := agentParams(fmt.Sprintf("imb-leaf-%d", i))
		lp.ListenAddr = fmt.Sprintf("10.9.0.%d:9000", i+2)
		leaf := register(t, s, lp)
		must(t, s.SetAgentParent(ctx, leaf.ID, zl.ID), "SetAgentParent(leaf)")
	}

	t.Run("Found", func(t *testing.T) {
		// zl has 6 children and maxChildren=4, so zl is heavy and has no
		// room; the shallowest node with room is one of its leaves.
		heavy, light, found, err := s.FindImbalancedNodes(ctx, 4)
		must(t, err, "FindImbalancedNodes(4)")
		if !found {
			t.Fatal("found = false, want true for 6-vs-0 imbalance")
		}
		if heavy.Agent.ID != zl.ID || heavy.ChildCount != 6 {
			t.Errorf("heavy = %q with %d children, want zl with 6", heavy.Agent.Hostname, heavy.ChildCount)
		}
		if light.Agent.ID == zl.ID {
			t.Error("light node is the same as heavy")
		}
		if light.ChildCount != 0 {
			t.Errorf("light ChildCount = %d, want 0", light.ChildCount)
		}
	})

	t.Run("NotFoundWhenRoomAtTop", func(t *testing.T) {
		// With maxChildren=10 the zone leader itself still has room, so the
		// light node equals the heavy node and no rebalance is suggested.
		_, _, found, err := s.FindImbalancedNodes(ctx, 10)
		must(t, err, "FindImbalancedNodes(10)")
		if found {
			t.Error("found = true, want false when the heavy node still has room")
		}
	})
}

// ─────────────────────────────────────────────────────────
// Server peers
// ─────────────────────────────────────────────────────────

func testPeers(t *testing.T, s db.DB) {
	ctx := context.Background()

	must(t, s.RegisterServerPeer(ctx, "pod-a", "10.0.0.1:8080"), "RegisterServerPeer(a)")
	must(t, s.RegisterServerPeer(ctx, "pod-b", "10.0.0.2:8080"), "RegisterServerPeer(b)")

	t.Run("List", func(t *testing.T) {
		peers, err := s.ListServerPeers(ctx)
		must(t, err, "ListServerPeers")
		if len(peers) != 2 {
			t.Fatalf("ListServerPeers = %d, want 2", len(peers))
		}
		if peers[0].PodID != "pod-a" || peers[1].PodID != "pod-b" {
			t.Errorf("peer order = [%s %s], want [pod-a pod-b]", peers[0].PodID, peers[1].PodID)
		}
		if peers[0].Addr != "10.0.0.1:8080" {
			t.Errorf("pod-a addr = %q", peers[0].Addr)
		}
		for _, p := range peers {
			if p.RegisteredAt.IsZero() || p.LastSeenAt.IsZero() {
				t.Errorf("peer %s has zero timestamps: %+v", p.PodID, p)
			}
		}
	})

	t.Run("UpsertUpdatesAddr", func(t *testing.T) {
		must(t, s.RegisterServerPeer(ctx, "pod-a", "10.0.0.9:8080"), "RegisterServerPeer(update)")
		peers, err := s.ListServerPeers(ctx)
		must(t, err, "ListServerPeers")
		if len(peers) != 2 {
			t.Fatalf("re-registration duplicated peer: %d rows", len(peers))
		}
		if peers[0].Addr != "10.0.0.9:8080" {
			t.Errorf("pod-a addr = %q, want updated 10.0.0.9:8080", peers[0].Addr)
		}
	})

	t.Run("Remove", func(t *testing.T) {
		must(t, s.RemoveServerPeer(ctx, "pod-a"), "RemoveServerPeer")
		peers, err := s.ListServerPeers(ctx)
		must(t, err, "ListServerPeers")
		if len(peers) != 1 || peers[0].PodID != "pod-b" {
			t.Errorf("after remove: %+v", peers)
		}
		wantNoRows(t, s.RemoveServerPeer(ctx, "pod-a"), "RemoveServerPeer(again)")
	})
}
