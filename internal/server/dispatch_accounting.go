// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"sync"
	"time"
)

// transportGrace is the slack a dispatcher gives between the command's
// configured timeout and its own hard timeout.  When the agent kills
// the runaway command at command-timeout, it still needs to flush an
// ExecResponse back up the mesh — without this grace the server can
// drop the imminent reply and report a false "did not respond".
const transportGrace = 30 * time.Second

// sessionAccounting is the shared per-broadcast bookkeeper for query,
// exec, and deploy dispatchers.  It enforces "first terminal event
// wins per agent" — whether the terminal is a real response from the
// agent or a synthetic failure injected because the agent's stream
// closed mid-broadcast.  Without this single gate the dispatcher could
// count an agent twice (real response + synthetic failure) and exit
// before another target has replied.
//
// Embed this in each dispatcher's session struct.  Use ClaimAgent
// before doing anything visible (enqueue to result channel, write to
// HTTP stream) so receivers can rely on each agent producing exactly
// one accounted event.
//
// arrivals is a small ring buffer of recent terminal timestamps used by
// /debug/inflight to surface "where is this query stuck right now" —
// quickly counting how many responses landed in the last {1, 5, 30}
// seconds tells you whether a slow path is slow-but-progressing or
// actually stuck.
type sessionAccounting struct {
	mu        sync.Mutex
	pending   map[string]bool // agent IDs we're still waiting on
	total     int             // size of the original target set
	accounted int             // monotonic counter; total - accounted == len(pending)
	arrivals  []time.Time     // ring buffer of recent ClaimAgent times
	arrivalsI int             // ring write index
}

// arrivalsCapacity caps how many recent terminal timestamps each
// session keeps for diagnostics.  Sized for a 30-second window at
// roughly the highest rate we'd care about (a few thousand per second).
const arrivalsCapacity = 4096

// newSessionAccounting builds an accounting struct seeded with the
// initial target set.  Empty target sets are valid (caller will exit
// immediately).
func newSessionAccounting(targetIDs []string) *sessionAccounting {
	pending := make(map[string]bool, len(targetIDs))
	for _, id := range targetIDs {
		pending[id] = true
	}
	return &sessionAccounting{
		pending: pending,
		total:   len(targetIDs),
	}
}

// ClaimAgent attempts to record a terminal event for agentID.  Returns
// true iff this is the first terminal for that agent — caller should
// then proceed to enqueue / record / display the result.  Returns
// false if the agent has already been accounted (deduped silently).
//
// Pass an unknown agent ID (one not in the original target set) and
// you get false back; that handles the case where a stream-loss
// notification covers more agents than this session targets.
func (a *sessionAccounting) ClaimAgent(agentID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.pending[agentID] {
		return false
	}
	delete(a.pending, agentID)
	a.accounted++
	if a.arrivals == nil {
		a.arrivals = make([]time.Time, arrivalsCapacity)
	}
	a.arrivals[a.arrivalsI%arrivalsCapacity] = time.Now()
	a.arrivalsI++
	return true
}

// ArrivalsSince returns how many terminal events landed in the
// given lookback window.  Used by /debug/inflight to surface
// "responses-in-last-Ns" gauges that distinguish a slow-but-moving
// dispatcher from one that's actually stuck.  Capped at
// arrivalsCapacity entries kept (older entries get overwritten).
func (a *sessionAccounting) ArrivalsSince(window time.Duration) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := time.Now().Add(-window)
	count := 0
	scan := arrivalsCapacity
	if a.arrivalsI < arrivalsCapacity {
		scan = a.arrivalsI
	}
	for i := 0; i < scan; i++ {
		if a.arrivals[i].After(cutoff) {
			count++
		}
	}
	return count
}

// Remaining returns the number of targets still pending.
func (a *sessionAccounting) Remaining() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending)
}

// AccountedCount returns how many targets have produced a terminal
// event (real or synthetic).
func (a *sessionAccounting) AccountedCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accounted
}

// Total returns the original target-set size.
func (a *sessionAccounting) Total() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.total
}

// PendingSnapshot returns a copy of the still-pending agent IDs.
// Used by /debug/inflight to show what the dispatcher is still
// waiting on.
func (a *sessionAccounting) PendingSnapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.pending))
	for id := range a.pending {
		out = append(out, id)
	}
	return out
}
