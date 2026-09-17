// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"testing"

	"github.com/atgreen/dirq/internal/db"
)

// buildDepthMesh returns a Server whose topology is a single chain
// zl(0) → relay(1) → leaf(2), plus a second leaf under the relay, so tests
// can exercise same-depth grouping.
func buildDepthMesh(t *testing.T) *Server {
	t.Helper()
	cfg := DefaultTopologyConfig()
	cfg.MaxChildrenPerNode = 4
	topo := NewMeshTopology(cfg)

	topo.AddAgent("zl", "zl", "10.0.0.1:50052")
	topo.AssignZoneLeader("zl") // depth 0

	topo.AddAgent("relay", "relay", "10.0.0.2:50052")
	if !topo.AssignChild("relay", "zl") { // depth 1
		t.Fatal("attach relay under zl")
	}
	for _, id := range []string{"leaf-a", "leaf-b"} {
		topo.AddAgent(id, id, "10.0.0.3:50052")
		if !topo.AssignChild(id, "relay") { // depth 2
			t.Fatalf("attach %s under relay", id)
		}
	}
	return &Server{topology: topo}
}

func agents(ids ...string) []db.Agent {
	out := make([]db.Agent, len(ids))
	for i, id := range ids {
		out[i] = db.Agent{ID: id}
	}
	return out
}

// TestWavesDeepestFirst pins the core ordering guarantee: a relay's whole
// subtree is dispatched in earlier waves than the relay, which is earlier than
// its own parent — regardless of the input order.
func TestWavesDeepestFirst(t *testing.T) {
	s := buildDepthMesh(t)

	// Deliberately shuffled input.
	waves := s.targetIDWavesByDepthDesc(agents("zl", "leaf-a", "relay", "leaf-b"))

	if len(waves) != 3 {
		t.Fatalf("expected 3 depth waves, got %d: %v", len(waves), waves)
	}
	// Wave 0: both leaves (depth 2), in input order.
	if got := waves[0]; len(got) != 2 || got[0] != "leaf-a" || got[1] != "leaf-b" {
		t.Errorf("wave 0 = %v, want [leaf-a leaf-b]", got)
	}
	// Wave 1: the relay (depth 1).
	if got := waves[1]; len(got) != 1 || got[0] != "relay" {
		t.Errorf("wave 1 = %v, want [relay]", got)
	}
	// Wave 2: the zone leader (depth 0).
	if got := waves[2]; len(got) != 1 || got[0] != "zl" {
		t.Errorf("wave 2 = %v, want [zl]", got)
	}

	// Every descendant must appear strictly before its ancestor.
	pos := map[string]int{}
	for wi, w := range waves {
		for _, id := range w {
			pos[id] = wi
		}
	}
	if pos["leaf-a"] >= pos["relay"] || pos["relay"] >= pos["zl"] {
		t.Errorf("ancestor ordering violated: %v", pos)
	}
}

// TestWavesUnknownDepthLast pins that an agent the topology has no position for
// is acted on last, never ahead of a node it might sit above.
func TestWavesUnknownDepthLast(t *testing.T) {
	s := buildDepthMesh(t)

	waves := s.targetIDWavesByDepthDesc(agents("ghost", "leaf-a", "zl"))
	if len(waves) != 3 {
		t.Fatalf("expected 3 waves, got %d: %v", len(waves), waves)
	}
	last := waves[len(waves)-1]
	if len(last) != 1 || last[0] != "ghost" {
		t.Errorf("final wave = %v, want [ghost] (unknown-depth agent acted on last)", last)
	}
}

// TestWavesSingleTargetOneWave is the degenerate case: one target, one wave.
func TestWavesSingleTargetOneWave(t *testing.T) {
	s := buildDepthMesh(t)
	waves := s.targetIDWavesByDepthDesc(agents("leaf-a"))
	if len(waves) != 1 || len(waves[0]) != 1 || waves[0][0] != "leaf-a" {
		t.Fatalf("waves = %v, want [[leaf-a]]", waves)
	}
}
