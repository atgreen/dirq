// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"strings"
	"testing"
	"time"

	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// When an agent's stream closes mid-broadcast, every dispatcher waiting
// on it has to be told, or it waits out its hard timeout for a reply that
// can never arrive. The notification has to reach all three session kinds
// — query, exec, deploy — because an agent can be in all three at once.

// registerSessions installs one session of each kind targeting the given
// agents, and removes them when the test ends.
func registerSessions(t *testing.T, ids ...string) (*querySession, *execBroadcastSession, *deploySession) {
	t.Helper()

	qs := &querySession{
		queryID:           "q-notify",
		results:           make(chan *pb.QueryResult, len(ids)+1),
		sessionAccounting: newSessionAccounting(ids),
	}
	bs := &execBroadcastSession{
		requestID:         "x-notify",
		results:           make(chan *pb.ExecResponse, len(ids)+1),
		sessionAccounting: newSessionAccounting(ids),
	}
	ds := &deploySession{
		requestID:         "d-notify",
		results:           make(chan *pb.DeployResponse, len(ids)+1),
		sessionAccounting: newSessionAccounting(ids),
	}

	querySessionsMu.Lock()
	querySessions[qs.queryID] = qs
	querySessionsMu.Unlock()
	execBroadcastSessionsMu.Lock()
	execBroadcastSessions[bs.requestID] = bs
	execBroadcastSessionsMu.Unlock()
	deploySessionsMu.Lock()
	deploySessions[ds.requestID] = ds
	deploySessionsMu.Unlock()

	t.Cleanup(func() {
		querySessionsMu.Lock()
		delete(querySessions, qs.queryID)
		querySessionsMu.Unlock()
		execBroadcastSessionsMu.Lock()
		delete(execBroadcastSessions, bs.requestID)
		execBroadcastSessionsMu.Unlock()
		deploySessionsMu.Lock()
		delete(deploySessions, ds.requestID)
		deploySessionsMu.Unlock()
	})
	return qs, bs, ds
}

func TestNotifySessionsAgentGone_RetiresTheAgentEverywhere(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	qs, bs, ds := registerSessions(t, "a1", "a2")

	s.notifySessionsAgentGone("stream closed", "a1")

	for name, remaining := range map[string]int{
		"query": qs.Remaining(), "exec": bs.Remaining(), "deploy": ds.Remaining(),
	} {
		if remaining != 1 {
			t.Errorf("%s session has %d pending, want 1 — a1 should be retired", name, remaining)
		}
	}

	// Each kind gets a synthetic failure carrying the reason, so the
	// operator sees why a host is missing instead of a silent gap.
	select {
	case r := <-qs.results:
		if r.AgentId != "a1" || r.Success || !strings.Contains(r.Error, "stream closed") {
			t.Errorf("query synth = %+v, want a failed a1 mentioning the reason", r)
		}
		if r.QueryId != qs.queryID {
			t.Errorf("query synth carries query_id %q, want %q", r.QueryId, qs.queryID)
		}
	default:
		t.Error("no synthetic result was enqueued on the query session")
	}

	select {
	case r := <-bs.results:
		if r.AgentId != "a1" || r.Success || !strings.Contains(r.Error, "stream closed") {
			t.Errorf("exec synth = %+v, want a failed a1 mentioning the reason", r)
		}
		if r.Rc != -1 {
			t.Errorf("exec synth rc = %d, want -1", r.Rc)
		}
	default:
		t.Error("no synthetic result was enqueued on the exec session")
	}

	select {
	case r := <-ds.results:
		if r.AgentId != "a1" || r.Success || !strings.Contains(r.Error, "stream closed") {
			t.Errorf("deploy synth = %+v, want a failed a1 mentioning the reason", r)
		}
		if r.Phase != "transport" {
			t.Errorf("deploy synth phase = %q, want \"transport\"", r.Phase)
		}
	default:
		t.Error("no synthetic result was enqueued on the deploy session")
	}
}

func TestNotifySessionsAgentGone_RetiresEveryListedAgent(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	qs, bs, ds := registerSessions(t, "a1", "a2", "a3")

	// A lost zone leader takes its whole subtree with it.
	s.notifySessionsAgentGone("fanout to ZL failed", "a1", "a2", "a3")

	if qs.Remaining() != 0 || bs.Remaining() != 0 || ds.Remaining() != 0 {
		t.Errorf("pending after a subtree loss: query=%d exec=%d deploy=%d, want 0",
			qs.Remaining(), bs.Remaining(), ds.Remaining())
	}
}

// A notification covering agents this session never targeted must leave
// it alone — stream-loss sweeps are broader than any one broadcast.
func TestNotifySessionsAgentGone_IgnoresUntargetedAgents(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	qs, bs, ds := registerSessions(t, "a1")

	s.notifySessionsAgentGone("stream closed", "someone-else")

	if qs.Remaining() != 1 || bs.Remaining() != 1 || ds.Remaining() != 1 {
		t.Errorf("an untargeted agent retired a target: query=%d exec=%d deploy=%d, want 1 each",
			qs.Remaining(), bs.Remaining(), ds.Remaining())
	}
	if len(qs.results) != 0 || len(bs.results) != 0 || len(ds.results) != 0 {
		t.Error("a synthetic result was enqueued for an agent the session never targeted")
	}
}

// The first terminal wins across sources too: once a real response has
// been accounted, a later stream-loss notice must not count again.
func TestNotifySessionsAgentGone_DoesNotDoubleCountAnAnsweredAgent(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	qs, bs, ds := registerSessions(t, "a1", "a2")

	qs.ClaimAgent("a1")
	bs.ClaimAgent("a1")
	ds.ClaimAgent("a1")

	s.notifySessionsAgentGone("stream closed", "a1")

	if qs.Remaining() != 1 || bs.Remaining() != 1 || ds.Remaining() != 1 {
		t.Errorf("a gone notice double-counted an answered agent: query=%d exec=%d deploy=%d, want 1 each",
			qs.Remaining(), bs.Remaining(), ds.Remaining())
	}
	if len(qs.results) != 0 || len(bs.results) != 0 || len(ds.results) != 0 {
		t.Error("a synthetic result was enqueued for an already-accounted agent")
	}
}

func TestNotifySessionsAgentGone_NoAgentsIsANoOp(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	qs, bs, ds := registerSessions(t, "a1")

	s.notifySessionsAgentGone("stream closed")

	if qs.Remaining() != 1 || bs.Remaining() != 1 || ds.Remaining() != 1 {
		t.Error("an empty notification retired a target")
	}
}

// TestNotifySessionsAgentGone_SurvivesAFullResultChannel pins the
// best-effort enqueue: accounting must still be updated when the channel
// has no room, so the dispatcher converges on its next drain instead of
// waiting out the hard timeout.
func TestNotifySessionsAgentGone_SurvivesAFullResultChannel(t *testing.T) {
	s := newTestServer(&mockDB{}, true)

	qs := &querySession{
		queryID:           "q-full",
		results:           make(chan *pb.QueryResult), // unbuffered: never accepts
		sessionAccounting: newSessionAccounting([]string{"a1"}),
	}
	querySessionsMu.Lock()
	querySessions[qs.queryID] = qs
	querySessionsMu.Unlock()
	t.Cleanup(func() {
		querySessionsMu.Lock()
		delete(querySessions, qs.queryID)
		querySessionsMu.Unlock()
	})

	done := make(chan struct{})
	go func() {
		s.notifySessionsAgentGone("stream closed", "a1")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("notifySessionsAgentGone blocked on a result channel with no room")
	}

	if got := qs.Remaining(); got != 0 {
		t.Errorf("Remaining() = %d, want 0 — accounting must update even when the synth is dropped", got)
	}
}
