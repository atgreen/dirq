// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────
// Invariant test — a chronically-flapping node never accumulates a subtree.
// ─────────────────────────────────────────────────────────

// checkTreeInvariants asserts structural consistency of the whole topology:
// no parent over the hard fan-out cap, parent/child pointers agree, and no
// cycles in any parent chain.  These must hold regardless of the reliability
// features.
func checkTreeInvariants(t *testing.T, topo *MeshTopology) {
	t.Helper()
	topo.mu.RLock()
	defer topo.mu.RUnlock()
	for id, n := range topo.nodes {
		if len(n.children) > topo.cfg.MaxChildrenPerNode {
			t.Fatalf("node %s exceeds MaxChildrenPerNode: %d > %d", id, len(n.children), topo.cfg.MaxChildrenPerNode)
		}
		// Every child points back at this parent.
		for c := range n.children {
			cn, ok := topo.nodes[c]
			if !ok {
				t.Fatalf("node %s lists missing child %s", id, c)
			}
			if cn.parentID != id {
				t.Fatalf("child %s.parentID=%q but is in %s.children", c, cn.parentID, id)
			}
		}
		// Walk to root — must terminate (no cycle) within N steps.
		steps, cur := 0, id
		for cur != "" {
			cn, ok := topo.nodes[cur]
			if !ok {
				break
			}
			cur = cn.parentID
			if steps++; steps > len(topo.nodes) {
				t.Fatalf("parent chain from %s does not terminate (cycle?)", id)
			}
		}
	}
}

// A node held continuously on probation must never be handed children by the
// placement path, no matter how much churn happens around it — and the tree
// stays structurally valid throughout.
func TestInvariant_ChronicFlapperStaysLeaf(t *testing.T) {
	cfg := TopologyConfig{
		MaxChildrenPerNode:    4,
		MaxZoneLeaders:        3,
		FlapWindow:            time.Hour,
		FlapThreshold:         1.5,
		ProbationChildCap:     0,
		FailureDomainPrefixV4: 16,
		DomainFlapMinNodes:    2,
	}
	topo := NewMeshTopology(cfg)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	topo.now = func() time.Time { return clock }

	// Seed zone leaders in distinct domains.
	for z := 0; z < cfg.MaxZoneLeaders; z++ {
		id := fmt.Sprintf("zl%d", z)
		topo.AddAgent(id, id, fmt.Sprintf("10.%d.0.1:9000", z))
		topo.AssignZoneLeader(id)
	}

	// "chronic" is a relay we keep flapping every iteration so its decayed
	// score never falls below threshold.
	topo.AddAgent("chronic", "chronic", "10.0.5.5:9000")
	if pid, _, ok := topo.FindShallowestParentWithRoom(); ok {
		topo.AssignChild("chronic", pid)
	}

	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 400; i++ {
		// A fresh agent arrives and is placed.
		id := fmt.Sprintf("n%d", i)
		topo.AddAgent(id, id, fmt.Sprintf("10.%d.%d.%d:9000", rng.Intn(4), i/256, i%256))
		if pid, _, ok := topo.FindShallowestParentWithRoom(); ok {
			topo.AssignChild(id, pid)
		} else {
			topo.AssignZoneLeader(id)
		}

		// Keep chronic on probation.
		topo.MarkOffline("chronic")
		topo.AddAgent("chronic", "chronic", "10.0.5.5:9000")
		clock = clock.Add(30 * time.Second)

		// Placement must never select chronic, and it must hold no children.
		if pid, _, ok := topo.FindShallowestParentWithRoom(); ok && pid == "chronic" {
			t.Fatalf("iter %d: probationary node 'chronic' was selected as a parent", i)
		}
		if got := topo.CountChildren("chronic"); got != 0 {
			t.Fatalf("iter %d: probationary node 'chronic' accumulated %d children", i, got)
		}
	}
	checkTreeInvariants(t, topo)
}

// ─────────────────────────────────────────────────────────
// Simulation — does reboot-aware placement actually reduce blast radius?
//
// We generate ONE self-exciting reboot trace (a node that just rebooted is
// more likely to reboot again) and replay the identical trace against two
// topologies: reliability ON (defaults) and OFF (thresholds zeroed, so the
// placement is reboot-blind — the pre-feature behaviour).  The score is the
// blast radius × downtime: every time a node reboots, the whole online
// subtree under it is unreachable for the down period.  If the feature works,
// it keeps chronic rebooters near the leaves, so ON accrues materially less
// blast·seconds than OFF on the very same trace.
// ─────────────────────────────────────────────────────────

type simEventKind int

const (
	simArrive simEventKind = iota
	simReboot
)

type simEvent struct {
	kind    simEventKind
	id      string
	downSec int
}

// genRebootTrace builds a deterministic, tree-shape-independent event script
// that mirrors how the reboot-aware placement is meant to pay off — the
// benefit is at PLACEMENT time, so a node has to reveal itself as flaky
// *before* the fleet grows around it. Phases:
//
//  1. Zone leaders arrive (one per failure domain).
//  2. A small set of "fragile" relays arrive and attach shallow (depth 1).
//  3. Those fragile relays flap while the tree is still small — so they have
//     essentially no subtree yet, and the cost of these early reboots is tiny.
//     By the end they are all on probation.
//  4. The bulk of the fleet arrives. Reboot-aware placement now routes these
//     away from the (already-flaky) fragile relays; the reboot-blind policy
//     BFS-fills straight onto them, growing big subtrees under nodes that are
//     about to keep rebooting.
//  5. The fragile relays keep flapping (self-exciting). Every one of these
//     reboots takes down whatever subtree sits under them.
//
// Target selection never consults the tree, so the identical script is valid
// under either placement policy.
func genRebootTrace(seed int64, nArrivals, maxZL, fragile, tailReboots int) []simEvent {
	rng := rand.New(rand.NewSource(seed))
	var events []simEvent
	arrive := func(i int) { events = append(events, simEvent{kind: simArrive, id: fmt.Sprintf("n%d", i)}) }
	down := func() int { return 20 + rng.Intn(100) } // seconds offline
	fragileID := func(k int) string { return fmt.Sprintf("n%d", maxZL+k) }

	// Phase 1+2: zone leaders, then the fragile relays.
	for i := 0; i < maxZL+fragile; i++ {
		arrive(i)
	}
	// Phase 3: flap the fragile relays a few times each, while small.
	for round := 0; round < 3; round++ {
		for k := 0; k < fragile; k++ {
			events = append(events, simEvent{kind: simReboot, id: fragileID(k), downSec: down()})
		}
	}
	// Phase 4: the rest of the fleet arrives.
	for i := maxZL + fragile; i < nArrivals; i++ {
		arrive(i)
	}
	// Phase 5: fragile relays keep rebooting, now with subtrees beneath them.
	for r := 0; r < tailReboots; r++ {
		events = append(events, simEvent{kind: simReboot, id: fragileID(r % fragile), downSec: down()})
	}
	return events
}

// simRun replays an event trace against one topology and accumulates the
// blast radius × downtime.
type simRun struct {
	topo    *MeshTopology
	clock   *time.Time
	cfg     TopologyConfig
	addr    map[string]string
	zlCount int
	blast   int64
}

func newSimRun(cfg TopologyConfig) *simRun {
	topo := NewMeshTopology(cfg)
	clk := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := &simRun{topo: topo, clock: &clk, cfg: cfg, addr: map[string]string{}}
	topo.now = func() time.Time { return *r.clock }
	return r
}

func (r *simRun) idIndex(id string) int {
	var n int
	fmt.Sscanf(id, "n%d", &n)
	return n
}

func (r *simRun) arrive(id string) {
	idx := r.idIndex(id)
	// Spread nodes across 6 /16 failure domains; unique IP per node.
	addr := fmt.Sprintf("10.%d.%d.%d:9000", idx%6, idx/256, idx%256)
	r.addr[id] = addr
	r.topo.AddAgent(id, id, addr)
	if r.zlCount < r.cfg.MaxZoneLeaders {
		r.topo.AssignZoneLeader(id)
		r.zlCount++
		return
	}
	if pid, _, ok := r.topo.FindShallowestParentWithRoom(); ok {
		r.topo.AssignChild(id, pid)
	} else {
		r.topo.AssignZoneLeader(id)
		r.zlCount++
	}
}

func (r *simRun) reboot(id string, downSec int) {
	sub := r.topo.SubtreeIDs(id)
	online := 0
	for _, s := range sub {
		if info, ok := r.topo.Get(s); ok && info.Online {
			online++
		}
	}
	r.blast += int64(online) * int64(downSec)

	r.topo.MarkSubtreeOffline(id)
	*r.clock = r.clock.Add(time.Duration(downSec) * time.Second)
	// The host reconnects (re-register bumps its flap score); its subtree,
	// whose immediate parents are alive again, comes back online too.
	r.topo.AddAgent(id, id, r.addr[id])
	for _, s := range sub {
		if s != id {
			r.topo.MarkOnline(s)
		}
	}
}

func (r *simRun) replay(events []simEvent) {
	for _, e := range events {
		switch e.kind {
		case simArrive:
			r.arrive(e.id)
		case simReboot:
			r.reboot(e.id, e.downSec)
		}
	}
}

func simConfig(reliabilityOn bool) TopologyConfig {
	cfg := TopologyConfig{
		MaxChildrenPerNode:    4,
		MaxZoneLeaders:        6,
		FailureDomainPrefixV4: 16,
	}
	if reliabilityOn {
		cfg.FlapWindow = time.Hour
		cfg.FlapThreshold = 1.5
		cfg.ProbationChildCap = 0
		cfg.DomainFlapMinNodes = 2
	} else {
		// Reboot-blind: thresholds zeroed, flaky nodes keep full capacity.
		cfg.FlapThreshold = 0
		cfg.DomainFlapMinNodes = 0
		cfg.ProbationChildCap = cfg.MaxChildrenPerNode
	}
	return cfg
}

func TestSim_ReliabilityReducesBlastRadius(t *testing.T) {
	events := genRebootTrace(2026, 160, 6, 6, 600)

	on := newSimRun(simConfig(true))
	on.replay(events)

	off := newSimRun(simConfig(false))
	off.replay(events)

	t.Logf("blast·seconds  ON=%d  OFF=%d  reduction=%.1f%%",
		on.blast, off.blast, 100*(1-float64(on.blast)/float64(off.blast)))

	if on.blast >= off.blast {
		t.Fatalf("reboot-aware placement did not reduce blast radius: ON=%d OFF=%d", on.blast, off.blast)
	}
	// Guard against a trivial win — we expect a clear, not marginal, reduction.
	if float64(on.blast) > 0.85*float64(off.blast) {
		t.Fatalf("expected a substantial blast-radius reduction, got only %.1f%% (ON=%d OFF=%d)",
			100*(1-float64(on.blast)/float64(off.blast)), on.blast, off.blast)
	}
}
