// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import "testing"

// Zone-leader slots are filled by new registrations, or by promotion when
// the tree is saturated, and by nothing else. A fleet that lost a leader
// and never registered another agent stayed short for good; if it lost
// the last one, every orphan self-promoted and the mesh went flat
// (dirq-zyc).

// meshWith builds a topology with one zone leader and the named relays,
// each optionally given a child so "already carrying traffic" can be
// expressed.
func meshWith(t *testing.T, zl string, relays map[string]bool) *MeshTopology {
	t.Helper()
	topo := NewMeshTopology(DefaultTopologyConfig())
	topo.AddAgent(zl, zl, "10.0.0.1:50052")
	topo.AssignZoneLeader(zl)
	for id, withChild := range relays {
		topo.AddAgent(id, id, "10.0.1.1:50052")
		if !topo.AssignChild(id, zl) {
			t.Fatalf("could not attach %s", id)
		}
		if withChild {
			kid := id + "-kid"
			topo.AddAgent(kid, kid, "10.0.2.1:50052")
			if !topo.AssignChild(kid, id) {
				t.Fatalf("could not attach %s under %s", kid, id)
			}
		}
	}
	return topo
}

// A relay already carrying a subtree is preferred: its reachability is
// proven, and promoting it moves only its own upstream link.
func TestFindPromotionCandidate_PrefersARelayWithChildren(t *testing.T) {
	topo := meshWith(t, "zl-1", map[string]bool{"lonely": false, "carrier": true})

	got, ok := topo.FindPromotionCandidate()
	if !ok {
		t.Fatal("no candidate found in a mesh full of relays")
	}
	if got != "carrier" {
		t.Errorf("promoted %q, want carrier — the relay with a subtree", got)
	}
}

// Promoting a node that keeps rebooting puts a subtree behind it. The
// flap score exists to say so, and the candidate search must honour it.
func TestFindPromotionCandidate_SkipsFlappingNodes(t *testing.T) {
	topo := meshWith(t, "zl-1", map[string]bool{"steady": true, "flapper": true})

	// A flap is a disappear-then-reappear, and reappearing means
	// re-registering — which is where the score is actually incremented.
	// The default threshold is 1.5, so three cycles puts it well over.
	for i := 0; i < 3; i++ {
		topo.MarkOffline("flapper")
		topo.AddAgent("flapper", "flapper", "10.0.1.1:50052")
	}
	if !topo.IsFlaky("flapper") {
		t.Fatal("setup: the node did not become flaky, so the guard is untested")
	}

	got, _ := topo.FindPromotionCandidate()
	if got == "flapper" {
		t.Error("promoted a node that is flapping")
	}
}

func TestFindPromotionCandidate_IgnoresOfflineAndExistingLeaders(t *testing.T) {
	topo := meshWith(t, "zl-1", map[string]bool{"down": true})
	topo.MarkOffline("down")

	got, ok := topo.FindPromotionCandidate()
	if ok && got == "down" {
		t.Error("promoted an offline agent")
	}
	if ok && got == "zl-1" {
		t.Error("promoted an agent that is already a zone leader")
	}
}

func TestFindPromotionCandidate_EmptyWhenNothingToPromote(t *testing.T) {
	topo := NewMeshTopology(DefaultTopologyConfig())
	topo.AddAgent("zl-1", "zl-1", "10.0.0.1:50052")
	topo.AssignZoneLeader("zl-1")

	if got, ok := topo.FindPromotionCandidate(); ok {
		t.Errorf("found candidate %q in a mesh with only a zone leader", got)
	}
}

// The choice must not depend on map iteration order, or the behaviour is
// untestable and irreproducible in an incident.
func TestFindPromotionCandidate_IsDeterministic(t *testing.T) {
	first, _ := meshWith(t, "zl-1", map[string]bool{"a": true, "b": true, "c": true}).FindPromotionCandidate()
	for i := 0; i < 20; i++ {
		got, _ := meshWith(t, "zl-1", map[string]bool{"a": true, "b": true, "c": true}).FindPromotionCandidate()
		if got != first {
			t.Fatalf("candidate varied between runs: %q then %q", first, got)
		}
	}
}

// ─────────────────────────────────────────────────────────
// The server-side trigger
// ─────────────────────────────────────────────────────────

func TestFillVacantZoneLeaderSlot_PromotesWhenShort(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	s.topoCfg.MaxZoneLeaders = 2
	s.topology = meshWith(t, "zl-1", map[string]bool{"carrier": true})

	if got := s.topology.CountOnlineZoneLeaders(); got != 1 {
		t.Fatalf("setup: %d zone leaders, want 1", got)
	}

	s.fillVacantZoneLeaderSlot()

	if got := s.topology.CountOnlineZoneLeaders(); got != 2 {
		t.Errorf("%d zone leaders after filling a vacant slot, want 2", got)
	}
}

// At the configured count it must do nothing, or every stream close would
// grow the leader set without bound.
func TestFillVacantZoneLeaderSlot_NoopAtCapacity(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	s.topoCfg.MaxZoneLeaders = 1
	s.topology = meshWith(t, "zl-1", map[string]bool{"carrier": true})

	s.fillVacantZoneLeaderSlot()

	if got := s.topology.CountOnlineZoneLeaders(); got != 1 {
		t.Errorf("%d zone leaders, want 1 — already at the configured maximum", got)
	}
}

// One promotion per call. The proactive rebalancer was removed for
// churning the tree; this must not reintroduce that by filling every slot
// at once from a single event.
func TestFillVacantZoneLeaderSlot_PromotesAtMostOne(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	s.topoCfg.MaxZoneLeaders = 5
	s.topology = meshWith(t, "zl-1", map[string]bool{"a": true, "b": true, "c": true})

	s.fillVacantZoneLeaderSlot()

	if got := s.topology.CountOnlineZoneLeaders(); got != 2 {
		t.Errorf("%d zone leaders after one call, want 2 — one promotion per event", got)
	}
}
