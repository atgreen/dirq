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
	// MaxChildrenPerNode is the maximum number of children any single node
	// (zone leader or relay) can have. This is the fan-out ratio.
	// Default: 50
	MaxChildrenPerNode int

	// MaxZoneLeaders is the maximum number of zone leaders that connect
	// directly to the server. Once this limit is reached, new agents are
	// assigned as relays or leafs under existing zone leaders.
	// Default: 50
	MaxZoneLeaders int
}

// DefaultTopologyConfig returns sensible defaults for up to ~125k agents.
//
//	Level 0: Server (1)
//	Level 1: up to 50 zone leaders (connect to server)
//	Level 2: up to 50 relays per zone leader (2,500 relay nodes)
//	Level 3: up to 50 leafs per relay (125,000 leaf agents)
//
// Max hops from any leaf to the server: 3.
func DefaultTopologyConfig() TopologyConfig {
	return TopologyConfig{
		MaxChildrenPerNode: 50,
		MaxZoneLeaders:     50,
	}
}

// assignment is the result of the topology manager deciding where a new
// agent fits in the tree.
type assignment struct {
	Role     pb.AgentRole
	ParentID string // empty for zone leaders (they connect to the server)
	ParentAddr string // listen_addr of the parent, empty for zone leaders
}

// assignRole decides the role and parent for a newly registered agent.
//
// Algorithm:
//  1. If we have fewer than MaxZoneLeaders zone leaders, make this agent
//     a zone leader. It connects directly to the server.
//  2. Otherwise, find a zone leader with room for more children. Make this
//     agent a relay under that zone leader.
//  3. If all zone leaders are full, find a relay with room. Make this agent
//     a leaf under that relay.
//  4. If everything is full, fall back to zone leader (exceeds the soft
//     limit rather than rejecting the agent).
func (s *Server) assignRole(ctx context.Context) (assignment, error) {
	cfg := s.topoCfg

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

	// Step 2: Find a zone leader with room for a relay child.
	zl, err := s.db.FindParentWithRoom(ctx, "zone_leader", cfg.MaxChildrenPerNode)
	if err == nil && zl.ID != "" {
		s.log.Info("topology: assigning as relay",
			"parent", zl.Hostname,
			"parent_id", zl.ID,
		)
		return assignment{
			Role:       pb.AgentRole_AGENT_ROLE_RELAY,
			ParentID:   zl.ID,
			ParentAddr: zl.ListenAddr,
		}, nil
	}

	// Step 3: All zone leaders full. Find a relay with room for a leaf child.
	relay, err := s.db.FindParentWithRoom(ctx, "relay", cfg.MaxChildrenPerNode)
	if err == nil && relay.ID != "" {
		s.log.Info("topology: assigning as leaf",
			"parent", relay.Hostname,
			"parent_id", relay.ID,
		)
		return assignment{
			Role:       pb.AgentRole_AGENT_ROLE_LEAF,
			ParentID:   relay.ID,
			ParentAddr: relay.ListenAddr,
		}, nil
	}

	// Step 4: Everything is full. Exceed the zone leader limit rather than
	// rejecting the agent. Log a warning.
	s.log.Warn("topology: tree is full, adding extra zone_leader",
		"zone_leaders", zoneLeaderCount,
		"max", cfg.MaxZoneLeaders,
	)
	return assignment{
		Role: pb.AgentRole_AGENT_ROLE_ZONE_LEADER,
	}, nil
}
