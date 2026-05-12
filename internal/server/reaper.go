// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"time"
)

// startReaper periodically marks agents as offline if they haven't sent a
// heartbeat within the threshold. Also cleans up stale query records.
func (s *Server) startReaper(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
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
// the threshold. An agent that is still connected will refresh last_seen_at
// via heartbeats every 30 seconds, so a 90-second threshold gives it 3
// missed heartbeats before being marked offline.
func (s *Server) reapStaleAgents(ctx context.Context) {
	threshold := 90 * time.Second

	result, err := s.db.MarkStaleAgentsOffline(ctx, threshold)
	if err != nil {
		s.log.Error("reaper: failed to mark stale agents offline", "error", err)
		return
	}
	if result > 0 {
		s.log.Info("reaper: marked stale agents offline", "count", result)
	}
}
