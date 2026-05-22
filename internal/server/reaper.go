// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"time"
)

// startReaper is a safety net that catches agents whose disconnection was
// not propagated via PeerDisconnected (e.g. server restart, network
// partition where the parent also died). Runs infrequently with a long
// threshold — normal disconnections are handled immediately via stream
// close (zone leaders) or PeerDisconnected (relay children).
func (s *Server) startReaper(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reapStaleAgents(ctx)
		}
	}
}

// reapStaleAgents marks agents offline if their last_seen_at is older than
// 5 minutes. This is a safety net only — most agents are marked offline
// immediately when their parent detects the stream drop.  After marking
// them offline in the DB, walks the in-memory topology to find any
// online nodes whose zone-leader ancestor no longer has a live stream
// (server-side proof of mesh disconnection); those get marked offline
// in topology and broadcast dispatchers are notified so they stop
// waiting.
func (s *Server) reapStaleAgents(ctx context.Context) {
	// Refresh last_seen_at for all agents with active server streams
	// (zone leaders) and their entire subtrees. An open stream proves
	// the agent is alive — this keeps the safety-net reaper from
	// marking connected agents as stale.
	s.mu.RLock()
	streamIDs := make([]string, 0, len(s.streams))
	for id := range s.streams {
		streamIDs = append(streamIDs, id)
	}
	liveStreams := make(map[string]bool, len(streamIDs))
	for _, id := range streamIDs {
		liveStreams[id] = true
	}
	s.mu.RUnlock()

	for _, id := range streamIDs {
		if err := s.db.TouchAgentTree(ctx, id); err != nil {
			s.log.Error("reaper: failed to touch agent tree", "agent_id", id, "error", err)
		}
	}

	threshold := 5 * time.Minute

	result, err := s.db.MarkStaleAgentsOffline(ctx, threshold)
	if err != nil {
		s.log.Error("reaper: failed to mark stale agents offline", "error", err)
		return
	}
	if result > 0 {
		s.log.Info("reaper: safety-net marked stale agents offline", "count", result)
	}

	// Walk the in-memory topology and notify dispatchers about any
	// online agents whose mesh route is gone — their ZL ancestor
	// doesn't have a live stream to us.  This catches the case the
	// DB-side reap can't see: an agent that's still recently-seen but
	// effectively unreachable because its routing chain broke.
	var unreachable []string
	for _, id := range s.topology.allNodeIDs() {
		n, ok := s.topology.Get(id)
		if !ok || !n.Online {
			continue
		}
		if liveStreams[id] {
			continue // direct connection alive
		}
		zlID, ok := s.topology.FindZoneLeader(id)
		if !ok || !liveStreams[zlID] {
			unreachable = append(unreachable, id)
		}
	}
	if len(unreachable) > 0 {
		s.log.Info("reaper: notifying dispatchers about unreachable agents", "count", len(unreachable))
		s.notifySessionsAgentGone("reaper: mesh route gone", unreachable...)
	}

	// Clean up stale reassigning entries (agent never reconnected).
	s.reassigningMu.Lock()
	for id, t := range s.reassigning {
		if time.Since(t) > 2*time.Minute {
			delete(s.reassigning, id)
		}
	}
	s.reassigningMu.Unlock()
}
