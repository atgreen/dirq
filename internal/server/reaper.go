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
// immediately when their parent detects the stream drop.
func (s *Server) reapStaleAgents(ctx context.Context) {
	threshold := 5 * time.Minute

	result, err := s.db.MarkStaleAgentsOffline(ctx, threshold)
	if err != nil {
		s.log.Error("reaper: failed to mark stale agents offline", "error", err)
		return
	}
	if result > 0 {
		s.log.Info("reaper: safety-net marked stale agents offline", "count", result)
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
