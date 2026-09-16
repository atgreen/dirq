// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// sessionAccounting enforces "first terminal event wins per agent" across
// every broadcast dispatcher. Two failures hide here and neither shows up
// as an error: count an agent twice and the dispatcher exits while other
// targets are still working; miss one and it waits out the hard timeout
// for a reply that can never arrive. Both are concurrency bugs, so the
// races get exercised directly rather than reasoned about.

func TestSessionAccounting_InitialState(t *testing.T) {
	a := newSessionAccounting([]string{"a1", "a2", "a3"})

	if got := a.Total(); got != 3 {
		t.Errorf("Total() = %d, want 3", got)
	}
	if got := a.Remaining(); got != 3 {
		t.Errorf("Remaining() = %d, want 3", got)
	}
	if got := a.AccountedCount(); got != 0 {
		t.Errorf("AccountedCount() = %d, want 0", got)
	}
}

// An empty target set is valid — a query matching nothing still builds a
// session, and it must read as already complete rather than hanging.
func TestSessionAccounting_EmptyTargetSetIsComplete(t *testing.T) {
	a := newSessionAccounting(nil)

	if a.Remaining() != 0 || a.Total() != 0 || a.AccountedCount() != 0 {
		t.Errorf("empty session: remaining=%d total=%d accounted=%d, want all 0",
			a.Remaining(), a.Total(), a.AccountedCount())
	}
	if a.ClaimAgent("nobody") {
		t.Error("an empty session claimed an agent")
	}
}

func TestSessionAccounting_FirstClaimWins(t *testing.T) {
	a := newSessionAccounting([]string{"a1", "a2"})

	if !a.ClaimAgent("a1") {
		t.Fatal("first claim on a1 was refused")
	}
	// The second claim is the real response arriving after a synthetic
	// disconnect failure (or the reverse). It must be dropped.
	if a.ClaimAgent("a1") {
		t.Error("a1 was claimed twice — the dispatcher would exit early")
	}
	if got := a.Remaining(); got != 1 {
		t.Errorf("Remaining() = %d, want 1 — a2 has not answered", got)
	}
	if got := a.AccountedCount(); got != 1 {
		t.Errorf("AccountedCount() = %d, want 1", got)
	}
}

// A stream-loss notification sweeps every agent on a lost subtree, which
// routinely includes agents this session never targeted.
func TestSessionAccounting_UnknownAgentIsNotCounted(t *testing.T) {
	a := newSessionAccounting([]string{"a1"})

	if a.ClaimAgent("someone-elses-agent") {
		t.Error("an agent outside the target set was claimed")
	}
	if got := a.Remaining(); got != 1 {
		t.Errorf("Remaining() = %d, want 1 — an unknown agent must not retire a target", got)
	}
	if got := a.AccountedCount(); got != 0 {
		t.Errorf("AccountedCount() = %d, want 0", got)
	}
}

func TestSessionAccounting_FullyAccountedSessionIsDone(t *testing.T) {
	ids := []string{"a1", "a2", "a3"}
	a := newSessionAccounting(ids)

	for _, id := range ids {
		if !a.ClaimAgent(id) {
			t.Fatalf("first claim on %s was refused", id)
		}
	}
	if got := a.Remaining(); got != 0 {
		t.Errorf("Remaining() = %d, want 0", got)
	}
	if got := a.AccountedCount(); got != a.Total() {
		t.Errorf("AccountedCount() = %d, want Total() = %d", got, a.Total())
	}
	if got := len(a.PendingSnapshot()); got != 0 {
		t.Errorf("PendingSnapshot() has %d entries, want 0", got)
	}
}

// TestSessionAccounting_ConcurrentClaimsAccountEachAgentOnce is the test
// this type exists for. Real terminals arrive from the gRPC receive loop
// while synthetic ones arrive from stream-close handling, concurrently,
// for the same agents. Exactly one of each pair may win.
func TestSessionAccounting_ConcurrentClaimsAccountEachAgentOnce(t *testing.T) {
	const agents, claimersPerAgent = 50, 8

	ids := make([]string, agents)
	for i := range ids {
		ids[i] = fmt.Sprintf("a%d", i)
	}
	a := newSessionAccounting(ids)

	var wins int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})

	for _, id := range ids {
		for c := 0; c < claimersPerAgent; c++ {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				<-start // maximise overlap
				if a.ClaimAgent(id) {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}(id)
		}
	}
	close(start)
	wg.Wait()

	if wins != agents {
		t.Errorf("%d claims succeeded across %d agents, want exactly one each", wins, agents)
	}
	if got := a.AccountedCount(); got != agents {
		t.Errorf("AccountedCount() = %d, want %d", got, agents)
	}
	if got := a.Remaining(); got != 0 {
		t.Errorf("Remaining() = %d, want 0", got)
	}
}

func TestSessionAccounting_PendingSnapshotNamesWhatIsOutstanding(t *testing.T) {
	a := newSessionAccounting([]string{"a1", "a2", "a3"})
	a.ClaimAgent("a2")

	got := a.PendingSnapshot()
	if len(got) != 2 {
		t.Fatalf("PendingSnapshot() = %v, want 2 entries", got)
	}
	seen := map[string]bool{got[0]: true, got[1]: true}
	if !seen["a1"] || !seen["a3"] || seen["a2"] {
		t.Errorf("PendingSnapshot() = %v, want a1 and a3 (a2 answered)", got)
	}

	// The snapshot is a copy: /debug/inflight must not be able to edit
	// the live pending set by writing to what it was handed.
	got[0] = "clobbered"
	if after := len(a.PendingSnapshot()); after != 2 {
		t.Errorf("mutating the snapshot changed the session: %d pending", after)
	}
}

func TestSessionAccounting_ArrivalsSinceCountsRecentTerminals(t *testing.T) {
	a := newSessionAccounting([]string{"a1", "a2", "a3"})

	if got := a.ArrivalsSince(time.Minute); got != 0 {
		t.Errorf("ArrivalsSince on a fresh session = %d, want 0", got)
	}

	a.ClaimAgent("a1")
	a.ClaimAgent("a2")

	// /debug/inflight uses this to tell "slow but moving" from "stuck".
	if got := a.ArrivalsSince(time.Minute); got != 2 {
		t.Errorf("ArrivalsSince(1m) = %d, want 2", got)
	}
	// A window that predates every arrival must report none.
	if got := a.ArrivalsSince(0); got != 0 {
		t.Errorf("ArrivalsSince(0) = %d, want 0", got)
	}
	// A refused duplicate claim is not an arrival.
	a.ClaimAgent("a1")
	if got := a.ArrivalsSince(time.Minute); got != 2 {
		t.Errorf("ArrivalsSince(1m) after a duplicate = %d, want 2", got)
	}
}

// TestSessionAccounting_ConcurrentReadersDoNotRaceClaims runs the
// /debug/inflight readers against live claims. Under -race this fails if
// any accessor drops the lock.
func TestSessionAccounting_ConcurrentReadersDoNotRaceClaims(t *testing.T) {
	ids := make([]string, 200)
	for i := range ids {
		ids[i] = fmt.Sprintf("a%d", i)
	}
	a := newSessionAccounting(ids)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for _, id := range ids {
			a.ClaimAgent(id)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < len(ids); i++ {
			_ = a.Remaining()
			_ = a.AccountedCount()
			_ = a.Total()
			_ = a.PendingSnapshot()
			_ = a.ArrivalsSince(time.Second)
		}
	}()
	wg.Wait()

	if got := a.Remaining(); got != 0 {
		t.Errorf("Remaining() = %d, want 0 after every agent was claimed", got)
	}
}
