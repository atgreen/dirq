// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"time"

	"github.com/atgreen/dirq/internal/db"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// demoteRecord tracks demotion history for an agent so the rebalancer
// can back off when an agent keeps bouncing back to a direct connection.
type demoteRecord struct {
	lastDemote time.Time
	failures   int // consecutive bounce-backs
}

const (
	demoteBaseBackoff = 1 * time.Minute
	demoteMaxBackoff  = 30 * time.Minute
)

// startRebalancer periodically checks the topology and makes minimal
// adjustments to keep the mesh healthy. Only ONE action per cycle to
// avoid feedback loops.
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

	// Count online zone leaders (from DB, not streams — streams include fallbacks).
	onlineZLs, err := s.db.CountOnlineZoneLeaders(ctx)
	if err != nil {
		return
	}

	// Count non-ZL agents connected directly to the server (fallback connections).
	excessDirect := 0
	s.mu.RLock()
	for agentID := range s.streams {
		agent, err := s.db.GetAgent(ctx, agentID)
		if err == nil && agent.Role != "zone_leader" {
			excessDirect++
		}
	}
	s.mu.RUnlock()

	// Priority 1: Not enough zone leaders — promote ONE relay.
	// Only if we actually have fewer than desired.
	if onlineZLs < cfg.MaxZoneLeaders {
		s.log.Info("rebalancer: need more zone leaders",
			"online", onlineZLs, "desired", cfg.MaxZoneLeaders)
		s.promoteOneRelay(ctx)
		return // one action per cycle
	}

	// Priority 2: Excess fallback connections — demote ONE back under a ZL.
	if excessDirect > 0 {
		s.log.Info("rebalancer: excess direct connections", "excess", excessDirect)
		s.demoteOne(ctx)
		return // one action per cycle
	}

	// Housekeeping: clear cooldown records for agents that are no longer
	// connected directly (they successfully moved under a ZL).
	s.demoteMu.Lock()
	s.mu.RLock()
	for id := range s.demoteCooldown {
		if _, direct := s.streams[id]; !direct {
			delete(s.demoteCooldown, id)
		}
	}
	s.mu.RUnlock()
	s.demoteMu.Unlock()

	// Priority 3: Tree imbalance — redistribute ONE subtree.
	s.redistributeOne(ctx)
}

// promoteOneRelay finds the relay with the most children and promotes it.
func (s *Server) promoteOneRelay(ctx context.Context) {
	candidates, err := s.db.FindRelaysWithChildren(ctx)
	if err != nil || len(candidates) == 0 {
		s.log.Info("rebalancer: no relay with children to promote")
		return
	}

	candidate := candidates[0]
	childCount, _ := s.db.CountChildren(ctx, candidate.ID)

	msg := &pb.ServerMessage{
		Payload: &pb.ServerMessage_PeerUpdate{
			PeerUpdate: &pb.PeerUpdate{
				TargetAgentId: candidate.ID,
				NewRole:       pb.AgentRole_AGENT_ROLE_ZONE_LEADER,
				NewParentAddr: "",
			},
		},
	}
	if s.signer != nil {
		s.signServerMessage(msg)
	}

	// Only update DB after successfully sending the message.
	if s.sendToAgent(ctx, candidate.ID, msg) {
		s.db.SetAgentRole(ctx, candidate.ID, "zone_leader")
		s.db.SetAgentParent(ctx, candidate.ID, "")
		s.log.Info("rebalancer: promoted relay to zone leader",
			"agent", candidate.Hostname,
			"children", childCount,
		)
	}
}

// demoteOne moves one non-ZL agent from a direct server connection to a ZL.
func (s *Server) demoteOne(ctx context.Context) {
	cfg := s.topoCfg
	now := time.Now()

	// Find one non-ZL agent connected directly, skipping any still in cooldown.
	var agentID string
	s.mu.RLock()
	s.demoteMu.Lock()
	for id := range s.streams {
		agent, err := s.db.GetAgent(ctx, id)
		if err != nil || agent.Role == "zone_leader" {
			continue
		}
		if rec, ok := s.demoteCooldown[id]; ok {
			backoff := demoteBaseBackoff * (1 << rec.failures)
			if backoff > demoteMaxBackoff {
				backoff = demoteMaxBackoff
			}
			if now.Before(rec.lastDemote.Add(backoff)) {
				continue // still cooling down
			}
		}
		agentID = id
		break
	}
	s.demoteMu.Unlock()
	s.mu.RUnlock()

	if agentID == "" {
		return
	}

	parent, err := s.db.FindShallowestParentWithRoom(ctx, cfg.MaxChildrenPerNode)
	if err != nil || parent.ID == "" || parent.ID == agentID {
		return
	}

	var fallbacks []string
	fbAgents, _ := s.db.FindFallbackParents(ctx, parent.ID, cfg.MaxChildrenPerNode, 2)
	for _, fb := range fbAgents {
		fallbacks = append(fallbacks, fb.ListenAddr)
	}

	msg := &pb.ServerMessage{
		Payload: &pb.ServerMessage_PeerUpdate{
			PeerUpdate: &pb.PeerUpdate{
				TargetAgentId:    agentID,
				NewRole:          pb.AgentRole_AGENT_ROLE_RELAY,
				NewParentAddr:    parent.ListenAddr,
				NewFallbackAddrs: fallbacks,
			},
		},
	}
	if s.signer != nil {
		s.signServerMessage(msg)
	}

	s.mu.Lock()
	as, ok := s.streams[agentID]
	if ok {
		as.reassigned = true
	}
	s.mu.Unlock()
	if !ok {
		return
	}

	s.markReassigning(agentID)

	select {
	case as.send <- msg:
		s.db.SetAgentRole(ctx, agentID, "relay")
		s.db.SetAgentParent(ctx, agentID, parent.ID)

		s.demoteMu.Lock()
		rec := s.demoteCooldown[agentID]
		rec.lastDemote = time.Now()
		rec.failures++
		s.demoteCooldown[agentID] = rec
		s.demoteMu.Unlock()

		s.log.Info("rebalancer: demoted to relay",
			"agent_id", agentID, "new_parent", parent.Hostname,
			"demotion_attempt", rec.failures)
	default:
	}
}

// redistributeOne moves one subtree from a heavy node to a light node.
func (s *Server) redistributeOne(ctx context.Context) {
	cfg := s.topoCfg

	heavy, light, found, err := s.db.FindImbalancedNodes(ctx, cfg.MaxChildrenPerNode)
	if err != nil || !found {
		return
	}

	child, err := s.db.FindChildOfParent(ctx, heavy.Agent.ID)
	if err != nil || child.ID == "" {
		return
	}

	childChildren, _ := s.db.CountChildren(ctx, child.ID)

	var fallbacks []string
	fbAgents, _ := s.db.FindFallbackParents(ctx, light.Agent.ID, cfg.MaxChildrenPerNode, 2)
	for _, fb := range fbAgents {
		fallbacks = append(fallbacks, fb.ListenAddr)
	}

	msg := &pb.ServerMessage{
		Payload: &pb.ServerMessage_PeerUpdate{
			PeerUpdate: &pb.PeerUpdate{
				TargetAgentId:    child.ID,
				NewRole:          pb.AgentRole_AGENT_ROLE_RELAY,
				NewParentAddr:    light.Agent.ListenAddr,
				NewFallbackAddrs: fallbacks,
			},
		},
	}
	if s.signer != nil {
		s.signServerMessage(msg)
	}

	s.markReassigning(child.ID)

	if s.sendToAgent(ctx, child.ID, msg) {
		s.db.SetAgentParent(ctx, child.ID, light.Agent.ID)
		s.log.Info("rebalancer: redistributed subtree",
			"child", child.Hostname,
			"subtree_size", childChildren,
			"from", heavy.Agent.Hostname,
			"to", light.Agent.Hostname,
		)
	}
}

// sendToAgent sends a message to an agent — tries direct stream first,
// then routes through the agent's zone leader.
func (s *Server) sendToAgent(ctx context.Context, agentID string, msg *pb.ServerMessage) bool {
	s.mu.RLock()
	if as, ok := s.streams[agentID]; ok {
		s.mu.RUnlock()
		select {
		case as.send <- msg:
			return true
		default:
			return false
		}
	}
	s.mu.RUnlock()

	// Route through zone leader.
	zl, err := s.db.FindZoneLeader(ctx, agentID)
	if err != nil || zl.ID == "" {
		return false
	}
	s.mu.RLock()
	as, ok := s.streams[zl.ID]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	select {
	case as.send <- msg:
		return true
	default:
		return false
	}
}

// reassignOrphans moves all children of a dead parent to healthy nodes.
// Called when a zone leader's stream closes.
func (s *Server) reassignOrphans(ctx context.Context, deadParentID string) {
	children, err := s.db.ListAgents(ctx, db.ListAgentsFilter{ParentID: deadParentID})
	if err != nil || len(children) == 0 {
		return
	}

	cfg := s.topoCfg
	for _, child := range children {
		parent, err := s.db.FindShallowestParentWithRoom(ctx, cfg.MaxChildrenPerNode)
		if err != nil || parent.ID == "" || parent.ID == child.ID {
			s.log.Warn("reassignOrphans: no parent available", "child", child.Hostname)
			s.db.SetAgentParent(ctx, child.ID, "")
			continue
		}

		s.db.SetAgentParent(ctx, child.ID, parent.ID)
		s.log.Info("reassignOrphans: moved child to new parent",
			"child", child.Hostname, "new_parent", parent.Hostname)

		// If the child is connected directly to the server, tell it
		// to reconnect to its new parent.
		var fallbacks []string
		if fbAgents, err := s.db.FindFallbackParents(ctx, parent.ID, cfg.MaxChildrenPerNode, 2); err == nil {
			for _, fb := range fbAgents {
				fallbacks = append(fallbacks, fb.ListenAddr)
			}
		}

		msg := &pb.ServerMessage{
			Payload: &pb.ServerMessage_PeerUpdate{
				PeerUpdate: &pb.PeerUpdate{
					TargetAgentId:    child.ID,
					NewRole:          pb.AgentRole_AGENT_ROLE_RELAY,
					NewParentAddr:    parent.ListenAddr,
					NewFallbackAddrs: fallbacks,
				},
			},
		}
		if s.signer != nil {
			s.signServerMessage(msg)
		}

		s.mu.Lock()
		if as, ok := s.streams[child.ID]; ok {
			as.reassigned = true
			select {
			case as.send <- msg:
			default:
			}
		}
		s.mu.Unlock()
	}
}

// markReassigning records that an agent is being intentionally moved by the
// rebalancer. When its old parent reports the disconnect via PeerDisconnected,
// the server will skip marking it offline.
func (s *Server) markReassigning(agentID string) {
	s.reassigningMu.Lock()
	s.reassigning[agentID] = time.Now()
	s.reassigningMu.Unlock()
}
