// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"time"

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

	// Counts come from the in-memory topology, not the DB.
	onlineZLs := s.topology.CountOnlineZoneLeaders()

	// Count non-ZL agents connected directly to the server (fallback connections).
	excessDirect := 0
	s.mu.RLock()
	for agentID := range s.streams {
		if n, ok := s.topology.Get(agentID); ok && n.Role != "zone_leader" {
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
	candidateID, _, hostname, ok := s.topology.FindRelayToPromote()
	if !ok {
		s.log.Info("rebalancer: no relay with children to promote")
		return
	}

	msg := &pb.ServerMessage{
		Payload: &pb.ServerMessage_PeerUpdate{
			PeerUpdate: &pb.PeerUpdate{
				TargetAgentId: candidateID,
				NewRole:       pb.AgentRole_AGENT_ROLE_ZONE_LEADER,
				NewParentAddr: "",
			},
		},
	}
	if s.signer != nil {
		s.signServerMessage(msg)
	}

	// Only update topology after successfully sending the message.
	if s.sendToAgent(ctx, candidateID, msg) {
		s.topology.AssignZoneLeader(candidateID)
		s.log.Info("rebalancer: promoted relay to zone leader",
			"agent", hostname,
			"children", s.topology.CountChildren(candidateID),
		)
	}
}

// demoteOne moves one non-ZL agent from a direct server connection to a ZL.
func (s *Server) demoteOne(ctx context.Context) {
	now := time.Now()

	// Find one non-ZL agent connected directly, skipping any still in cooldown.
	var agentID string
	s.mu.RLock()
	s.demoteMu.Lock()
	for id := range s.streams {
		if n, ok := s.topology.Get(id); !ok || n.Role == "zone_leader" {
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

	parentID, parentAddr, ok := s.topology.FindShallowestParentWithRoom()
	if !ok || parentID == agentID {
		return
	}
	parentNode, _ := s.topology.Get(parentID)

	var fallbacks []string
	for _, fb := range s.topology.FindFallbackParents(parentID, 2) {
		fallbacks = append(fallbacks, fb.ListenAddr)
	}

	msg := &pb.ServerMessage{
		Payload: &pb.ServerMessage_PeerUpdate{
			PeerUpdate: &pb.PeerUpdate{
				TargetAgentId:    agentID,
				NewRole:          pb.AgentRole_AGENT_ROLE_RELAY,
				NewParentAddr:    parentAddr,
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
		s.topology.AssignChild(agentID, parentID)

		s.demoteMu.Lock()
		rec := s.demoteCooldown[agentID]
		rec.lastDemote = time.Now()
		rec.failures++
		s.demoteCooldown[agentID] = rec
		s.demoteMu.Unlock()

		s.log.Info("rebalancer: demoted to relay",
			"agent_id", agentID, "new_parent", parentNode.Hostname,
			"demotion_attempt", rec.failures)
	default:
	}
}

// redistributeOne moves one subtree from a heavy node to a light node.
func (s *Server) redistributeOne(ctx context.Context) {
	heavy, light, found := s.topology.FindImbalanced()
	if !found {
		return
	}

	child, ok := s.topology.PickChildOf(heavy.Agent.ID)
	if !ok || child.ID == "" {
		return
	}

	childChildren := s.topology.CountChildren(child.ID)

	var fallbacks []string
	for _, fb := range s.topology.FindFallbackParents(light.Agent.ID, 2) {
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
		s.topology.AssignChild(child.ID, light.Agent.ID)
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
func (s *Server) sendToAgent(_ context.Context, agentID string, msg *pb.ServerMessage) bool {
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
	zlID, ok := s.topology.FindZoneLeader(agentID)
	if !ok {
		return false
	}
	s.mu.RLock()
	as, ok := s.streams[zlID]
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
func (s *Server) reassignOrphans(_ context.Context, deadParentID string) {
	children := s.topology.ChildrenOf(deadParentID)
	if len(children) == 0 {
		return
	}

	for _, child := range children {
		parentID, parentAddr, ok := s.topology.FindShallowestParentWithRoom()
		if !ok || parentID == child.ID {
			// Tree has no room to absorb this orphan — promote it to zone
			// leader rather than leaving it dangling with no parent.
			// The PeerUpdate is best-effort (delivered only if the orphan
			// still has a live stream).
			s.log.Info("reassignOrphans: no parent available, promoting orphan to zone_leader",
				"child", child.Hostname)
			s.topology.AssignZoneLeader(child.ID)
			promoteMsg := &pb.ServerMessage{
				Payload: &pb.ServerMessage_PeerUpdate{
					PeerUpdate: &pb.PeerUpdate{
						TargetAgentId: child.ID,
						NewRole:       pb.AgentRole_AGENT_ROLE_ZONE_LEADER,
						NewParentAddr: "",
					},
				},
			}
			if s.signer != nil {
				s.signServerMessage(promoteMsg)
			}
			s.mu.Lock()
			if as, ok := s.streams[child.ID]; ok {
				select {
				case as.send <- promoteMsg:
				default:
				}
			}
			s.mu.Unlock()
			continue
		}

		parentNode, _ := s.topology.Get(parentID)
		s.topology.AssignChild(child.ID, parentID)
		s.log.Info("reassignOrphans: moved child to new parent",
			"child", child.Hostname, "new_parent", parentNode.Hostname)

		// If the child is connected directly to the server, tell it
		// to reconnect to its new parent.
		var fallbacks []string
		for _, fb := range s.topology.FindFallbackParents(parentID, 2) {
			fallbacks = append(fallbacks, fb.ListenAddr)
		}

		msg := &pb.ServerMessage{
			Payload: &pb.ServerMessage_PeerUpdate{
				PeerUpdate: &pb.PeerUpdate{
					TargetAgentId:    child.ID,
					NewRole:          pb.AgentRole_AGENT_ROLE_RELAY,
					NewParentAddr:    parentAddr,
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
