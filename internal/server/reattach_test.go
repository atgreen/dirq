// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"testing"

	"github.com/atgreen/dirq/internal/db"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// A stream close marks the dying node's whole subtree offline, which is
// right at that instant. Getting them back was the problem: only a direct
// stream or a parent's PeerConnected marks an agent online, and a
// descendant whose own parent survived never reattaches, so nothing ever
// reported it. It stayed offline forever while running fine (dirq-zwn).

// chain builds zl → mid → leaf and returns the server holding it.
func chain(t *testing.T) *Server {
	t.Helper()
	agents := []db.Agent{
		{ID: "zl", Hostname: "zl", Online: true},
		{ID: "mid", Hostname: "mid", Online: true},
		{ID: "leaf", Hostname: "leaf", Online: true},
	}
	s := newTestServer(&mockDB{agents: agents}, true)
	for _, a := range agents {
		s.topology.AddAgent(a.ID, a.Hostname, "10.0.0.1:50052")
	}
	s.topology.AssignZoneLeader("zl")
	if !s.topology.AssignChild("mid", "zl") || !s.topology.AssignChild("leaf", "mid") {
		t.Fatal("setup: could not build the chain")
	}
	return s
}

func onlineIn(s *Server, id string) bool {
	n, ok := s.topology.Get(id)
	return ok && n.Online
}

// TestPeerConnectedRestoresTheWholeSubtree is the fix: reporting one child
// back must bring everything behind it back too, because those agents
// never moved.
func TestPeerConnectedRestoresTheWholeSubtree(t *testing.T) {
	s := chain(t)

	// A zone leader dies and its subtree is swept offline.
	s.topology.MarkSubtreeOffline("zl")
	if onlineIn(s, "mid") || onlineIn(s, "leaf") {
		t.Fatal("setup: the subtree should be offline after the sweep")
	}

	// mid reattaches elsewhere and its new parent reports it.
	s.handlePeerConnected(context.Background(), &pb.PeerConnected{
		AgentId:  "mid",
		ParentId: "zl",
	})

	if !onlineIn(s, "mid") {
		t.Error("the reported agent was not marked online")
	}
	// The one that used to be stranded forever.
	if !onlineIn(s, "leaf") {
		t.Error("a descendant that never lost its stream is still offline")
	}
}

// Reporting a leaf must not resurrect anything above it — the sweep
// direction is downward only.
func TestPeerConnectedDoesNotRestoreUpwards(t *testing.T) {
	s := chain(t)
	s.topology.MarkSubtreeOffline("zl")

	s.handlePeerConnected(context.Background(), &pb.PeerConnected{
		AgentId:  "leaf",
		ParentId: "mid",
	})

	if !onlineIn(s, "leaf") {
		t.Error("the reported leaf was not marked online")
	}
	if onlineIn(s, "mid") {
		t.Error("reporting a child marked its parent online; nothing proved the parent is reachable")
	}
}

func TestPeerConnectedIgnoresIncompleteReports(t *testing.T) {
	s := chain(t)
	s.topology.MarkSubtreeOffline("zl")

	for _, pc := range []*pb.PeerConnected{
		{AgentId: "", ParentId: "zl"},
		{AgentId: "mid", ParentId: ""},
	} {
		s.handlePeerConnected(context.Background(), pc)
	}
	if onlineIn(s, "mid") {
		t.Error("an incomplete PeerConnected was acted on")
	}
}
