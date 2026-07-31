// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"testing"
	"time"
)

// reliabilityTopo builds a topology with a controllable clock and small,
// easy-to-reason-about reliability knobs.
func reliabilityTopo() (*MeshTopology, *time.Time) {
	cfg := TopologyConfig{
		MaxChildrenPerNode: 2,
		MaxZoneLeaders:     5,
		FlapWindow:         time.Hour,
		FlapThreshold:      1.5,
		ProbationChildCap:  0,
	}
	t := NewMeshTopology(cfg)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := &clock
	t.now = func() time.Time { return *now }
	return t, now
}

// flap simulates a reboot: the agent is marked offline (as a reaper /
// PeerDisconnected would) and then re-registers via AddAgent with the SAME
// hostname/listen address it had before, exactly as a rebooting host does.
func flap(t *MeshTopology, id string) {
	info, ok := t.Get(id)
	if !ok {
		panic("flap: unknown agent " + id)
	}
	t.MarkOffline(id)
	t.AddAgent(id, info.Hostname, info.ListenAddr)
}

func TestFlapScoreAccumulatesAndTripsProbation(t *testing.T) {
	topo, _ := reliabilityTopo()
	topo.AddAgent("a", "hostA", "10.0.0.1:9000")
	topo.AssignZoneLeader("a")

	if topo.IsFlaky("a") {
		t.Fatal("fresh agent should not be flaky")
	}

	// One reboot: score 1.0, below the 1.5 threshold.
	flap(topo, "a")
	if topo.IsFlaky("a") {
		t.Fatal("one reboot (score 1.0) should not trip probation")
	}

	// Second reboot shortly after: score ~2.0, over threshold.
	flap(topo, "a")
	if !topo.IsFlaky("a") {
		t.Fatal("two close reboots should trip probation")
	}
}

func TestFlapScoreDecaysBackToStable(t *testing.T) {
	topo, now := reliabilityTopo()
	topo.AddAgent("a", "hostA", "10.0.0.1:9000")
	topo.AssignZoneLeader("a")

	flap(topo, "a")
	flap(topo, "a")
	if !topo.IsFlaky("a") {
		t.Fatal("expected flaky after two reboots")
	}

	// After several decay windows of quiet, the score falls below threshold.
	*now = now.Add(3 * time.Hour)
	if topo.IsFlaky("a") {
		t.Fatal("score should have decayed below threshold after 3 windows of quiet")
	}
}

func TestFindParentPrefersStableOverFlaky(t *testing.T) {
	topo, _ := reliabilityTopo()
	// Two zone leaders at equal depth. "flapper" is shallow but unstable;
	// "steady" is equally shallow and stable. With ProbationChildCap=0 the
	// flapper is skipped entirely.
	topo.AddAgent("flapper", "hf", "10.0.0.1:9000")
	topo.AssignZoneLeader("flapper")
	topo.AddAgent("steady", "hs", "10.0.0.2:9000")
	topo.AssignZoneLeader("steady")

	flap(topo, "flapper")
	flap(topo, "flapper")
	if !topo.IsFlaky("flapper") {
		t.Fatal("flapper should be on probation")
	}

	id, _, ok := topo.FindShallowestParentWithRoom()
	if !ok {
		t.Fatal("expected a parent with room")
	}
	if id != "steady" {
		t.Fatalf("expected stable ZL 'steady' as parent, got %q", id)
	}
}

func TestProbationCapBlocksNewChildren(t *testing.T) {
	topo, _ := reliabilityTopo()
	topo.AddAgent("p", "hp", "10.0.0.1:9000")
	topo.AssignZoneLeader("p")

	flap(topo, "p")
	flap(topo, "p")
	if !topo.IsFlaky("p") {
		t.Fatal("p should be on probation")
	}

	topo.AddAgent("c", "hc", "10.0.0.2:9000")
	if topo.AssignChild("c", "p") {
		t.Fatal("a probationary node (cap 0) must not accept new children")
	}
}

func TestDomainOfMasksToPrefix(t *testing.T) {
	topo, _ := reliabilityTopo() // V4 prefix defaults are 24/64 in reliabilityTopo? set explicitly
	topo.cfg.FailureDomainPrefixV4 = 24
	topo.cfg.FailureDomainPrefixV6 = 64
	cases := map[string]string{
		"10.0.5.7:9000":      "10.0.5.0/24",
		"10.0.5.200:9001":    "10.0.5.0/24",
		"10.0.6.7:9000":      "10.0.6.0/24",
		"[2001:db8::1]:9000": "2001:db8::/64",
	}
	for addr, want := range cases {
		if got := topo.domainOf(addr); got != want {
			t.Errorf("domainOf(%q) = %q, want %q", addr, got, want)
		}
	}
	// An unparseable host is its own singleton domain.
	if got := topo.domainOf("not-an-ip:9000"); got != "not-an-ip" {
		t.Errorf("domainOf(non-ip) = %q, want %q", got, "not-an-ip")
	}
}

// A correlated reboot: when enough distinct hosts in one /24 flap, the whole
// domain is treated as risky, so a personally-quiet host in that domain loses
// to a stable host in a different domain during parent selection.
func TestCorrelatedDomainDeprioritizesQuietNeighbor(t *testing.T) {
	topo, _ := reliabilityTopo()
	topo.cfg.FailureDomainPrefixV4 = 24
	topo.cfg.DomainFlapMinNodes = 2

	// Domain A = 10.0.5.0/24: three hosts, two of which flap twice each.
	topo.AddAgent("a1", "a1", "10.0.5.1:9000")
	topo.AssignZoneLeader("a1")
	topo.AddAgent("a2", "a2", "10.0.5.2:9000")
	topo.AssignZoneLeader("a2")
	// a3 is a quiet host in the SAME hot domain — never flaps personally.
	topo.AddAgent("a3", "a3", "10.0.5.3:9000")
	topo.AssignZoneLeader("a3")

	// Domain B = 10.0.6.0/24: one stable host.
	topo.AddAgent("b1", "b1", "10.0.6.1:9000")
	topo.AssignZoneLeader("b1")

	// Two hosts in domain A each reboot twice → domain A is correlated-hot.
	for _, id := range []string{"a1", "a2"} {
		flap(topo, id)
		flap(topo, id)
	}

	hot := func() map[string]bool {
		topo.mu.RLock()
		defer topo.mu.RUnlock()
		return topo.hotDomainsLocked()
	}()
	if !hot["10.0.5.0/24"] {
		t.Fatalf("expected 10.0.5.0/24 to be correlated-hot, got %v", hot)
	}

	// a3 is personally stable but in the hot domain; b1 is stable in a cool
	// domain. Selection must prefer b1 even though a3 is equally shallow.
	id, _, ok := topo.FindShallowestParentWithRoom()
	if !ok {
		t.Fatal("expected a parent")
	}
	if id != "b1" {
		t.Fatalf("expected stable-domain ZL 'b1', got %q (a3 is in a hot domain)", id)
	}
}

// The failure-domain signal is soft: if EVERY candidate is in a hot domain,
// selection still returns one rather than starving the tree.
func TestHotDomainDoesNotStarveSelection(t *testing.T) {
	topo, _ := reliabilityTopo()
	topo.cfg.FailureDomainPrefixV4 = 24
	topo.cfg.DomainFlapMinNodes = 2
	topo.cfg.ProbationChildCap = 5 // let flaky nodes still parent, to prove softness

	topo.AddAgent("a1", "a1", "10.0.5.1:9000")
	topo.AssignZoneLeader("a1")
	topo.AddAgent("a2", "a2", "10.0.5.2:9000")
	topo.AssignZoneLeader("a2")
	for _, id := range []string{"a1", "a2"} {
		flap(topo, id)
		flap(topo, id)
	}

	if _, _, ok := topo.FindShallowestParentWithRoom(); !ok {
		t.Fatal("hot domain must not starve selection when it's the only option")
	}
}

// A node that gains children while stable keeps them across a later reboot —
// probation only prevents *new* accumulation, it does not evict.
func TestProbationDoesNotEvictExistingChildren(t *testing.T) {
	topo, _ := reliabilityTopo()
	topo.AddAgent("p", "hp", "10.0.0.1:9000")
	topo.AssignZoneLeader("p")
	topo.AddAgent("c", "hc", "10.0.0.2:9000")
	if !topo.AssignChild("c", "p") {
		t.Fatal("stable parent should accept a child")
	}

	flap(topo, "p")
	flap(topo, "p")

	if got := topo.CountChildren("p"); got != 1 {
		t.Fatalf("existing child should be retained across reboot, got %d children", got)
	}
}
