// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"net/http"
	"testing"

	"github.com/atgreen/dirq/internal/db"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// Online and reachable are different questions, and conflating them is how
// an operator ends up querying a fleet that looks healthy and getting
// partial results back. Online is written when an agent registers; a
// broadcast travels down the zone leader's stream, which opens afterwards.

// connectStream registers a live agent stream, as AgentStream would.
func connectStream(s *Server, agentID string) {
	s.mu.Lock()
	s.streams[agentID] = &agentStream{agentID: agentID, send: make(chan *pb.ServerMessage, 1)}
	s.mu.Unlock()
}

func TestReachable_ZoneLeaderWithAStreamIsReachable(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	s.topology.AddAgent("zl-1", "zl-1", "10.0.0.1:50052")
	s.topology.AssignZoneLeader("zl-1")
	connectStream(s, "zl-1")

	agents := []db.Agent{{ID: "zl-1", Hostname: "zl-1", Online: true}}
	s.enrichWithTopology(agents)

	if !agents[0].Reachable {
		t.Error("a zone leader with a live stream is not reachable")
	}
}

// The case that bites: registered, online, and nothing can get to it.
func TestReachable_ZoneLeaderWithoutAStreamIsNot(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	s.topology.AddAgent("zl-1", "zl-1", "10.0.0.1:50052")
	s.topology.AssignZoneLeader("zl-1")
	// No stream: the agent registered but has not connected yet.

	agents := []db.Agent{{ID: "zl-1", Hostname: "zl-1", Online: true}}
	s.enrichWithTopology(agents)

	if agents[0].Reachable {
		t.Error("an agent that has registered but not connected reports reachable")
	}
	if !agents[0].Online {
		t.Error("online was disturbed; the two must stay independent")
	}
}

// A leaf is reachable through its zone leader, not on its own account.
func TestReachable_ChildFollowsItsZoneLeader(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	s.topology.AddAgent("zl-1", "zl-1", "10.0.0.1:50052")
	s.topology.AssignZoneLeader("zl-1")
	s.topology.AddAgent("leaf-1", "leaf-1", "10.0.0.2:50052")
	if !s.topology.AssignChild("leaf-1", "zl-1") {
		t.Fatal("could not attach leaf-1 to zl-1")
	}

	agents := []db.Agent{{ID: "leaf-1", Hostname: "leaf-1", Online: true}}

	// Zone leader down: the leaf is unreachable however healthy it is.
	s.enrichWithTopology(agents)
	if agents[0].Reachable {
		t.Error("a leaf reports reachable while its zone leader has no stream")
	}

	// Zone leader up: the path exists.
	connectStream(s, "zl-1")
	s.enrichWithTopology(agents)
	if !agents[0].Reachable {
		t.Error("a leaf is not reachable despite its zone leader having a stream")
	}
}

// Another zone leader's stream says nothing about this one's subtree.
func TestReachable_AnotherZoneLeadersStreamDoesNotCount(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	for _, id := range []string{"zl-1", "zl-2"} {
		s.topology.AddAgent(id, id, "10.0.0.1:50052")
		s.topology.AssignZoneLeader(id)
	}
	connectStream(s, "zl-2")

	agents := []db.Agent{{ID: "zl-1", Hostname: "zl-1", Online: true}}
	s.enrichWithTopology(agents)

	if agents[0].Reachable {
		t.Error("zl-1 reports reachable because an unrelated zone leader is connected")
	}
}

// An agent the topology has never heard of cannot be reachable.
func TestReachable_UnknownAgentIsNotReachable(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	connectStream(s, "zl-1")

	agents := []db.Agent{{ID: "ghost", Hostname: "ghost", Online: true}}
	s.enrichWithTopology(agents)

	if agents[0].Reachable {
		t.Error("an agent absent from the topology reports reachable")
	}
}

// TestGetHostIsEnrichedLikeTheList pins that the single-host endpoint
// applies the same live overlay as the list. Without it, hosts show
// returns the stored record: a stale role, and reachable false for every
// host regardless of the truth — a field that is always wrong is worse
// than one that is absent.
func TestGetHostIsEnrichedLikeTheList(t *testing.T) {
	stored := db.Agent{ID: "zl-1", Hostname: "zl-1", Online: true, Role: "leaf"}
	s := newTestServer(&mockDB{agents: []db.Agent{stored}}, true)
	s.topology.AddAgent("zl-1", "zl-1", "10.0.0.1:50052")
	s.topology.AssignZoneLeader("zl-1")
	connectStream(s, "zl-1")

	rec := callHandler(s.handleGetHost, "GET", "", map[string]string{"id": "zl-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	got := decodeAgent(t, rec)
	if !got.Reachable {
		t.Error("hosts show reports the agent unreachable while its stream is live")
	}
	if got.Role != "zone_leader" {
		t.Errorf("role = %q, want the live topology role zone_leader, not the stored %q", got.Role, stored.Role)
	}
}
