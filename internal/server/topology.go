// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"fmt"

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
func (s *Server) assignRole(ctx context.Context, agentID string) (assignment, error) {
	cfg := s.topoCfg

	// Step 0: If this agent is re-registering and was previously promoted to
	// zone leader (e.g., by reassignOrphans or RequestPeers when the tree
	// saturated), keep that role.  Otherwise the recomputed assignment would
	// undo the promotion and re-orphan the agent on the next churn cycle.
	if agentID != "" {
		if existing, err := s.db.GetAgent(ctx, agentID); err == nil &&
			existing.Role == "zone_leader" &&
			(existing.ParentID == nil || *existing.ParentID == "") {
			s.log.Info("topology: preserving existing zone_leader assignment on re-register",
				"agent_id", agentID)
			return assignment{
				Role: pb.AgentRole_AGENT_ROLE_ZONE_LEADER,
			}, nil
		}
	}

	// Step 1: Do we need more zone leaders?
	zoneLeaderCount, err := s.db.CountAgentsByRole(ctx, "zone_leader")
	if err != nil {
		return assignment{}, fmt.Errorf("count zone leaders: %w", err)
	}

	if zoneLeaderCount < cfg.MaxZoneLeaders {
		s.log.Info("topology: assigning as zone_leader",
			"current_zone_leaders", zoneLeaderCount,
			"max", cfg.MaxZoneLeaders,
		)
		return assignment{
			Role: pb.AgentRole_AGENT_ROLE_ZONE_LEADER,
		}, nil
	}

	// Step 2: Find any node with room, preferring shallowest (BFS fill).
	parent, err := s.db.FindShallowestParentWithRoom(ctx, cfg.MaxChildrenPerNode)
	if err == nil && parent.ID != "" {
		// Find fallback parents on different branches for fault isolation.
		var fallbacks []string
		fbAgents, err := s.db.FindFallbackParents(ctx, parent.ID, cfg.MaxChildrenPerNode, 2)
		if err == nil {
			for _, fb := range fbAgents {
				fallbacks = append(fallbacks, fb.ListenAddr)
			}
		}

		s.log.Info("topology: assigning under parent",
			"parent", parent.Hostname,
			"parent_id", parent.ID,
			"fallbacks", len(fallbacks),
		)
		return assignment{
			Role:          pb.AgentRole_AGENT_ROLE_RELAY,
			ParentID:      parent.ID,
			ParentAddr:    parent.ListenAddr,
			FallbackAddrs: fallbacks,
		}, nil
	}

	// Step 3: Tree is full. Add an extra zone leader.
	s.log.Warn("topology: tree is full, adding extra zone_leader",
		"zone_leaders", zoneLeaderCount,
		"max", cfg.MaxZoneLeaders,
	)
	return assignment{
		Role: pb.AgentRole_AGENT_ROLE_ZONE_LEADER,
	}, nil
}
