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
// agent fits in the tree.
type assignment struct {
	Role          pb.AgentRole
	ParentID      string   // empty for zone leaders (they connect to the server)
	ParentAddr    string   // listen_addr of the parent, empty for zone leaders
	FallbackAddrs []string // ordered backup parent addresses for failover
}

// assignRole decides the role and parent for a newly registered agent.
//
// Algorithm:
//  1. If we have fewer than MaxZoneLeaders, assign as zone leader.
//  2. Otherwise, find any node in the tree with room for another child
//     (preferring the shallowest available node to keep the tree balanced).
//  3. If the entire tree is full, add an extra zone leader.
//
// There is no relay/leaf distinction — every non-ZL node is simply a "node"
// in the tree. Nodes with children automatically relay traffic; nodes without
// children are effectively leafs. The agent binary handles both cases.
//
// All decisions read in-memory MeshTopology state — no DB round-trips on
// the registration hot path.  See meshtopology.go for the rationale.
func (s *Server) assignRole(agentID string) assignment {
	cfg := s.topoCfg

	// Step 0: re-registration of a previously-promoted orphan.  Keep the
	// zone_leader role so the orphan-promotion fix doesn't get undone.
	if existing, ok := s.topology.Get(agentID); ok &&
		existing.Role == "zone_leader" && existing.ParentID == "" {
		return assignment{Role: pb.AgentRole_AGENT_ROLE_ZONE_LEADER}
	}

	// Step 1: Need more zone leaders?
	if s.topology.CountOnlineZoneLeaders() < cfg.MaxZoneLeaders {
		return assignment{Role: pb.AgentRole_AGENT_ROLE_ZONE_LEADER}
	}

	// Step 2: BFS-fill — shallowest online parent with room.
	if parentID, parentAddr, ok := s.topology.FindShallowestParentWithRoom(); ok {
		fbAgents := s.topology.FindFallbackParents(parentID, 2)
		fallbacks := make([]string, 0, len(fbAgents))
		for _, fb := range fbAgents {
			fallbacks = append(fallbacks, fb.ListenAddr)
		}
		return assignment{
			Role:          pb.AgentRole_AGENT_ROLE_RELAY,
			ParentID:      parentID,
			ParentAddr:    parentAddr,
			FallbackAddrs: fallbacks,
		}
	}

	// Step 3: Tree saturated — add an extra zone leader so this agent
	// has a route instead of being orphaned.
	return assignment{Role: pb.AgentRole_AGENT_ROLE_ZONE_LEADER}
}
