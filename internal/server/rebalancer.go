// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"time"

	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// startRebalancer periodically checks the topology and makes minimal
// adjustments to keep the mesh healthy.
//
// Two scenarios:
//
// 1. Zone leader died → promote a relay from a surviving branch.
//    The promoted relay keeps its children — they don't reconnect.
//    Only the promoted relay itself reconnects (to the server).
//    This is a single-agent disruption.
//
// 2. Too many direct connections (excess fallback connections after
//    recovery) → demote excess agents back under zone leaders.
//    Move at most 2 per cycle.
func (s *Server) startRebalancer(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.rebalanceOnce(ctx)
		}
	}
}

func (s *Server) rebalanceOnce(ctx context.Context) {
	cfg := s.topoCfg

	onlineZLs := 0
	s.mu.RLock()
	for agentID := range s.streams {
		agent, err := s.db.GetAgent(ctx, agentID)
		if err == nil && agent.Role == "zone_leader" {
			onlineZLs++
		}
	}
	directConns := len(s.streams)
	s.mu.RUnlock()

	// Scenario 1: Not enough zone leaders — promote a relay.
	if onlineZLs < cfg.MaxZoneLeaders {
		s.promoteRelay(ctx, cfg.MaxZoneLeaders-onlineZLs)
	}

	// Scenario 2: Too many direct connections — demote excess back under ZLs.
	if directConns > cfg.MaxZoneLeaders {
		s.demoteExcess(ctx, directConns-cfg.MaxZoneLeaders)
	}
}

// promoteRelay finds a relay agent with children and promotes it to zone
// leader. The relay's children stay connected to it — zero disruption for
// them. Only the promoted relay reconnects (from its old parent to the server).
func (s *Server) promoteRelay(ctx context.Context, needed int) {
	if needed <= 0 {
		return
	}

	// Promote at most 1 per cycle to keep things gentle.
	if needed > 1 {
		needed = 1
	}

	// Find a relay that has children — splitting mid-branch means
	// the promoted node brings its subtree with it.
	candidates, err := s.db.FindRelaysWithChildren(ctx)
	if err != nil || len(candidates) == 0 {
		s.log.Info("rebalancer: no relay with children to promote")
		return
	}

	for i := 0; i < needed && i < len(candidates); i++ {
		candidate := candidates[i]

		// Count its children for logging.
		childCount, _ := s.db.CountChildren(ctx, candidate.ID)

		// Update DB: promote to zone leader, clear parent.
		s.db.SetAgentRole(ctx, candidate.ID, "zone_leader")
		s.db.SetAgentParent(ctx, candidate.ID, "")

		// Tell the agent to reconnect to the server as a zone leader.
		msg := &pb.ServerMessage{
			Payload: &pb.ServerMessage_PeerUpdate{
				PeerUpdate: &pb.PeerUpdate{
					NewRole:       pb.AgentRole_AGENT_ROLE_ZONE_LEADER,
					NewParentAddr: "", // empty = connect to server
				},
			},
		}

		if s.signer != nil {
			s.signServerMessage(msg)
		}

		// The agent might be connected to the server (fallback) or to its
		// parent relay. Try the direct stream first.
		sent := false
		s.mu.RLock()
		if as, ok := s.streams[candidate.ID]; ok {
			select {
			case as.send <- msg:
				sent = true
			default:
			}
		}
		s.mu.RUnlock()

		if sent {
			s.log.Info("rebalancer: promoted relay to zone leader (direct)",
				"agent", candidate.Hostname,
				"agent_id", candidate.ID,
				"children", childCount,
			)
		} else {
			// Agent isn't directly connected — it's behind a relay.
			// Route the PeerUpdate through its zone leader.
			zl, err := s.db.FindZoneLeader(ctx, candidate.ID)
			if err == nil && zl.ID != "" {
				s.mu.RLock()
				if as, ok := s.streams[zl.ID]; ok {
					// Set the agent_id so relays forward it to the right target.
					// We reuse the exec routing: the relay checks agent_id
					// and forwards to the matching downstream.
					select {
					case as.send <- msg:
						s.log.Info("rebalancer: promoted relay to zone leader (via mesh)",
							"agent", candidate.Hostname,
							"agent_id", candidate.ID,
							"via_zl", zl.Hostname,
							"children", childCount,
						)
					default:
					}
				}
				s.mu.RUnlock()
			}
		}
	}
}

// demoteExcess moves agents that are connected directly to the server
// (as fallback connections) back under zone leaders. Moves at most 2
// per cycle.
func (s *Server) demoteExcess(ctx context.Context, excess int) {
	if excess <= 0 {
		return
	}

	toMove := excess
	if toMove > 2 {
		toMove = 2
	}

	cfg := s.topoCfg
	moved := 0

	// Find non-zone-leader agents connected directly to the server.
	s.mu.RLock()
	var candidates []string
	for agentID := range s.streams {
		agent, err := s.db.GetAgent(ctx, agentID)
		if err != nil {
			continue
		}
		if agent.Role != "zone_leader" {
			candidates = append(candidates, agentID)
		}
	}
	s.mu.RUnlock()

	for _, agentID := range candidates {
		if moved >= toMove {
			break
		}

		parent, err := s.db.FindShallowestParentWithRoom(ctx, cfg.MaxChildrenPerNode)
		if err != nil || parent.ID == "" || parent.ID == agentID {
			continue
		}

		var fallbacks []string
		fbAgents, _ := s.db.FindFallbackParents(ctx, parent.ID, cfg.MaxChildrenPerNode, 2)
		for _, fb := range fbAgents {
			fallbacks = append(fallbacks, fb.ListenAddr)
		}

		msg := &pb.ServerMessage{
			Payload: &pb.ServerMessage_PeerUpdate{
				PeerUpdate: &pb.PeerUpdate{
					NewRole:          pb.AgentRole_AGENT_ROLE_RELAY,
					NewParentAddr:    parent.ListenAddr,
					NewFallbackAddrs: fallbacks,
				},
			},
		}

		if s.signer != nil {
			s.signServerMessage(msg)
		}

		s.mu.RLock()
		as, ok := s.streams[agentID]
		s.mu.RUnlock()

		if !ok {
			continue
		}

		select {
		case as.send <- msg:
			s.db.SetAgentRole(ctx, agentID, "relay")
			s.db.SetAgentParent(ctx, agentID, parent.ID)

			s.log.Info("rebalancer: moved agent under zone leader",
				"agent_id", agentID,
				"new_parent", parent.Hostname,
			)
			moved++
		default:
		}
	}
}
