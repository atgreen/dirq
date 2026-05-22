// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"

	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// This file used to host a proactive rebalancer that ran on a 30 s tick
// and shuffled the tree to fill ZL slots, demote stragglers, and even
// out subtree sizes.  The proactive paths are gone — they caused more
// disruption (mid-broadcast agent moves, IP-diversity violations) than
// they cured.  What remains is purely reactive:
//
//   - reassignOrphans: fires from AgentStream's close defer when a node
//     dies.  The dead node's direct children get reassigned (or
//     promoted to ZL if the tree is saturated) so they keep routing.
//     Deeper descendants don't lose their streams (their immediate
//     parent is still alive, just reconnecting upstream).
//
//   - sendToAgent: shared dispatch helper used by exec, file ops, and
//     PeerUpdate emissions.  Tries the agent's direct stream first,
//     falls back to routing through its zone leader.
//
// Slot maintenance over time happens through three signal paths that
// run without a ticker:
//
//   - New registrations.  The batcher (registration_batcher.go) places
//     fresh agents using source-IP diversity, naturally filling empty
//     ZL slots when new hosts arrive.
//   - Orphan-promotion fallback.  RequestPeers / reassignOrphans
//     promote saturated-tree agents to ZL via the in-memory topology's
//     escape hatch.
//   - Agent reconnect.  Every agent runs connectLoop on stream loss,
//     trying its primary parent, then fallback addresses, then
//     RequestPeers — which always either finds a parent or promotes
//     the agent.

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

// reassignOrphans moves all children of a dead parent to healthy
// nodes.  Called from AgentStream's close defer when a zone leader
// (or any node with a direct server stream) drops.  The direct
// children are the only ones whose upstream is now broken — deeper
// descendants stay connected to their depth-1 parent, which itself is
// reassigning upstream.
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
