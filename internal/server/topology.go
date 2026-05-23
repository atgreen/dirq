// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
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
		MaxChildrenPerNode: 50,
		MaxZoneLeaders:     5,
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
