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

// reassignOrphans hints direct children of a dead parent toward a new
// home.  Called from AgentStream's close defer when a node with a
// direct server stream (typically a zone leader) drops.
//
// Two kinds of action, distinguished by what counts as "committed truth"
// in the topology:
//
//   - Promote-to-ZL: when the tree has no room, the orphan IS now a
//     zone leader; this is committed via AssignZoneLeader.  A PeerUpdate
//     is best-effort dispatched to the agent so it knows to reconnect
//     directly to the server.  (Agent might also discover the same via
//     RequestPeers' tree-saturated path.)
//
//   - Reparent-hint: when a relay slot exists, the orphan is told via
//     PeerUpdate to reconnect to that relay.  No topology rewrite —
//     the relay's RelayStream emits PeerConnected upstream when the
//     child actually attaches, which is what commits the new parent_id
//     and flips the agent online.  Speculatively writing AssignChild
//     here was the source of the "ghost online" failure mode: the
//     reaper then treated reassigned-but-not-yet-attached children as
//     reachable via the new ZL, and new broadcasts targeted them and
//     timed out.
//
// The PeerUpdate is best-effort either way: it can only be delivered
// to children with a live direct server stream.  Most depth-1 relay
// children don't have one, and they re-home via their own connectLoop
// (primary → fallback → RequestPeers) regardless of whether the hint
// arrived.
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
			s.log.Info("reassignOrphans: no parent available, promoting orphan to zone_leader",
				"child", child.Hostname)
			s.topology.AssignZoneLeader(child.ID)
			metricOrphanReassign.WithLabelValues("promote").Inc()
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
		s.log.Info("reassignOrphans: hinting child toward new parent",
			"child", child.Hostname, "candidate_parent", parentNode.Hostname)
		metricOrphanReassign.WithLabelValues("reparent").Inc()

		// Hint only — topology is updated when PeerConnected arrives.
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
