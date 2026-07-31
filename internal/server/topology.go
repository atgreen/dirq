// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"time"

	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// TopologyConfig controls how the mesh tree is shaped.
type TopologyConfig struct {
	// MaxChildrenPerNode is the maximum number of children any node can
	// have. This is the fan-out ratio. Default: 50.
	MaxChildrenPerNode int

	// MaxZoneLeaders is the number of agents that connect directly to the
	// server. Once this limit is reached, all new agents are placed in the
	// tree below existing zone leaders. The tree grows as deep as needed.
	// Default: 5.
	MaxZoneLeaders int

	// --- Reboot-aware placement -------------------------------------------
	//
	// A node that just disappeared and came back (a reboot, or a repeatedly
	// dropping stream) is, empirically, more likely to do it again — reboots
	// cluster in time.  Because a parent's failure orphans its entire
	// subtree, the cheapest place for an unreliable node is a leaf, and the
	// worst place is a zone leader.  These knobs let the topology keep
	// recently-flapping nodes near the leaves until they prove stable again.

	// FlapWindow is the decay time-constant for a node's flap score.  Each
	// disappear→reappear bumps the score by 1; the score decays as
	// exp(-elapsed/FlapWindow), so reboots that stop happening fade away.
	// A larger window remembers instability for longer.  Zero disables
	// decay (scores only ever accumulate).  Default: 1 hour.
	FlapWindow time.Duration

	// FlapThreshold is the decayed flap score at or above which a node is
	// treated as "on reboot probation": it is deprioritized as a parent,
	// capped at ProbationChildCap children, and never promoted to zone
	// leader while a stabler candidate exists.  Tuned so a single one-off
	// reboot (score 1.0) does NOT trip it, but two reboots inside the window
	// do.  Zero disables reliability-aware placement entirely.  Default: 1.5.
	FlapThreshold float64

	// ProbationChildCap is the maximum number of children a node on reboot
	// probation may hold.  Default 0 confines a flapping node to a pure leaf
	// — it accepts no new children until its score decays below
	// FlapThreshold.  Existing children are never forcibly evicted (that
	// would be proactive rebalancing, which this design deliberately avoids);
	// the cap only prevents a flaky node from accumulating a subtree in the
	// first place.
	ProbationChildCap int

	// --- Correlated (failure-domain) reboots ------------------------------
	//
	// Reboots cluster in space as well as time: a rack loses power, a
	// hypervisor bounces, a VLAN blips, and every host behind it reboots
	// together.  A host that personally looks stable but sits in a churning
	// failure domain is still a poor place to root a subtree, and a fallback
	// parent in the same domain as the primary is worthless when the whole
	// domain drops at once.  These knobs group agents into failure domains
	// by network prefix and treat a domain as risky when several of its
	// members are individually flapping.

	// FailureDomainPrefixV4 / FailureDomainPrefixV6 are the prefix lengths
	// used to bucket an agent's listen address into a failure domain — a
	// proxy for "same rack / subnet / hypervisor".  Defaults: /24 and /64.
	FailureDomainPrefixV4 int
	FailureDomainPrefixV6 int

	// DomainFlapMinNodes is how many individually-flaky members a failure
	// domain must have before the whole domain is treated as correlated-
	// risky (deprioritized for parent placement, and avoided as a fallback
	// on the same domain as the primary).  Unlike the per-node cap this is a
	// soft steer, never a hard block — a single noisy host can't take its
	// neighbors down with it because it takes MinNodes of them.  Default 2;
	// 0 disables failure-domain correlation entirely.
	DomainFlapMinNodes int
}

// DefaultTopologyConfig returns sensible defaults.
//
// With 5 zone leaders and fan-out 50:
//
//	Depth 2: 5 × 50 = 250 agents
//	Depth 3: 5 × 50² = 12,500 agents
//	Depth 4: 5 × 50³ = 625,000 agents
//
// The tree grows organically — no fixed depth limit.
// Server always holds exactly 5 connections regardless of fleet size.
func DefaultTopologyConfig() TopologyConfig {
	return TopologyConfig{
		MaxChildrenPerNode:    50,
		MaxZoneLeaders:        5,
		FlapWindow:            time.Hour,
		FlapThreshold:         1.5,
		ProbationChildCap:     0,
		FailureDomainPrefixV4: 24,
		FailureDomainPrefixV6: 64,
		DomainFlapMinNodes:    2,
	}
}

// assignment is the result of the topology manager deciding where a new
// agent fits in the tree.  Produced by the registration batcher; consumed
// by the Register hot path.
type assignment struct {
	Role          pb.AgentRole
	ParentID      string   // empty for zone leaders (they connect to the server)
	ParentAddr    string   // listen_addr of the parent, empty for zone leaders
	FallbackAddrs []string // ordered backup parent addresses for failover
}
