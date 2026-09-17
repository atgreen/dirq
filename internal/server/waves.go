// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"sort"

	"github.com/atgreen/dirq/internal/db"
)

// unknownDepth marks a target the in-memory topology has no position for. It
// sorts after every real depth so an unknown node is acted on last, never
// ahead of a node it might sit above.
const unknownDepth = -1

// targetIDWavesByDepthDesc groups target agents into dispatch waves ordered by
// mesh depth, deepest first. Every wave holds the agents at one depth, and a
// child's depth is always its parent's + 1, so a relay's entire subtree lands
// in earlier (deeper) waves than the relay itself.
//
// This is what --bottom-up trades broadcast speed for: because a wave is not
// dispatched until the previous one has reached a terminal state, a relay is
// never acted on while an agent beneath it is still running — a relay is
// rebooted only after its children have.
//
// Agents absent from the topology have unknown position and are placed in the
// final wave (see unknownDepth). The result preserves each target's relative
// order within its wave.
func (s *Server) targetIDWavesByDepthDesc(targets []db.Agent) [][]string {
	byDepth := make(map[int][]string)
	var depths []int
	for _, a := range targets {
		d := unknownDepth
		if n, ok := s.topology.Get(a.ID); ok {
			d = n.Depth
		}
		if _, seen := byDepth[d]; !seen {
			depths = append(depths, d)
		}
		byDepth[d] = append(byDepth[d], a.ID)
	}

	// Deepest first; unknownDepth (-1) is the smallest key, so it lands last.
	sort.Sort(sort.Reverse(sort.IntSlice(depths)))

	waves := make([][]string, 0, len(depths))
	for _, d := range depths {
		waves = append(waves, byDepth[d])
	}
	return waves
}
