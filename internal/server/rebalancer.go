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
		var fallbackIDs []string
		for _, fb := range s.topology.FindFallbackParents(parentID, 2) {
			fallbacks = append(fallbacks, fb.ListenAddr)
			fallbackIDs = append(fallbackIDs, fb.ID)
		}

		// Only pin when per-agent certs (CN = agent ID) are issued — see Register.
		pinParentID := ""
		var pinFallbackIDs []string
		if s.mtlsEnabled {
			pinParentID = parentID
			pinFallbackIDs = fallbackIDs
		}
		msg := &pb.ServerMessage{
			Payload: &pb.ServerMessage_PeerUpdate{
				PeerUpdate: &pb.PeerUpdate{
					TargetAgentId:    child.ID,
					NewRole:          pb.AgentRole_AGENT_ROLE_RELAY,
					NewParentAddr:    parentAddr,
					NewParentId:      pinParentID,
					NewFallbackAddrs: fallbacks,
					NewFallbackIds:   pinFallbackIDs,
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

// fillVacantZoneLeaderSlot promotes one relay when a zone leader has been
// lost and the fleet is now below MaxZoneLeaders.
//
// This is the piece registration_batcher.go's comment assumed existed:
// slots left open by a diversity-constrained batch were said to be filled
// by "the rebalancer's promote a relay with children path". That path went
// away with the proactive rebalancer and nothing replaced it, so a fleet
// that lost a zone leader and never registered a new agent stayed a leader
// short for good — and if it lost the last one, every orphan self-promoted
// through RequestPeers and the mesh went flat.
//
// Deliberately event-driven and not a ticker. The proactive rebalancer was
// removed because periodic reshuffling moved agents mid-broadcast and
// violated IP diversity; this fires at most once per zone-leader death,
// promotes at most one agent, and prefers a relay that already has
// children so only that agent's upstream link moves while its subtree
// stays where it is.
func (s *Server) fillVacantZoneLeaderSlot() {
	want := s.topoCfg.MaxZoneLeaders
	if have := s.topology.CountOnlineZoneLeaders(); have >= want {
		return
	}

	id, ok := s.topology.FindPromotionCandidate()
	if !ok {
		s.log.Info("zone-leader slot open but no suitable candidate to promote",
			"online_zone_leaders", s.topology.CountOnlineZoneLeaders(), "max", want)
		return
	}

	node, _ := s.topology.Get(id)
	s.topology.AssignZoneLeader(id)
	metricOrphanReassign.WithLabelValues("promote_slot").Inc()
	s.log.Info("promoted a relay to fill a vacant zone-leader slot",
		"agent_id", id, "hostname", node.Hostname,
		"online_zone_leaders", s.topology.CountOnlineZoneLeaders(), "max", want)

	// Tell the agent, so it reconnects straight to the server.
	//
	// This has to go out through every zone-leader stream rather than to
	// the agent's own, the way reassignOrphans does. The candidate we
	// prefer is a relay carrying children, and a relay by definition holds
	// no direct server stream — so a direct send could never reach exactly
	// the agent we most want to promote. Agents relay a PeerUpdate aimed at
	// someone else on down the tree, so a broadcast finds it wherever it
	// sits. Without this the topology records a promotion the agent never
	// hears about, and its subtree reads as unreachable because their new
	// zone leader has no stream.
	msg := &pb.ServerMessage{
		Payload: &pb.ServerMessage_PeerUpdate{
			PeerUpdate: &pb.PeerUpdate{
				TargetAgentId: id,
				NewRole:       pb.AgentRole_AGENT_ROLE_ZONE_LEADER,
				NewParentAddr: "",
			},
		},
	}
	if s.signer != nil {
		s.signServerMessage(msg)
	}
	s.mu.Lock()
	for _, as := range s.streams {
		select {
		case as.send <- msg:
		default:
			s.log.Warn("zone leader send buffer full while broadcasting a promotion",
				"zone_leader", as.agentID, "target", id)
		}
	}
	s.mu.Unlock()
}
