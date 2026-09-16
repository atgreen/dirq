// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"testing"

	"github.com/atgreen/dirq/internal/db"
)

// AttachObserved records what the mesh has already done. AssignChild
// decides what it should do. Conflating the two is how agents ended up
// recorded under a dead parent while answering queries through a live
// one (dirq-613).

// fullParent builds a parent already holding its maximum children.
func fullParent(t *testing.T) (*MeshTopology, string) {
	t.Helper()
	cfg := DefaultTopologyConfig()
	cfg.MaxChildrenPerNode = 2
	topo := NewMeshTopology(cfg)

	topo.AddAgent("zl", "zl", "10.0.0.1:50052")
	topo.AssignZoneLeader("zl")
	for _, id := range []string{"kid-1", "kid-2"} {
		topo.AddAgent(id, id, "10.0.0.9:50052")
		if !topo.AssignChild(id, "zl") {
			t.Fatalf("could not fill zl with %s", id)
		}
	}
	return topo, "zl"
}

// TestAssignChildStillRefusesAnOverfullParent pins that placement keeps
// its budget — the fix must not turn capacity into a suggestion.
func TestAssignChildStillRefusesAnOverfullParent(t *testing.T) {
	topo, parent := fullParent(t)
	topo.AddAgent("newcomer", "newcomer", "10.0.0.20:50052")

	if topo.AssignChild("newcomer", parent) {
		t.Error("AssignChild placed a third child on a parent with capacity 2")
	}
}

// TestAttachObservedRecordsBeyondCapacity is the fix: an agent that has
// already failed over must be recorded there, budget or not. Refusing does
// not undo the connection, it only makes the topology lie.
func TestAttachObservedRecordsBeyondCapacity(t *testing.T) {
	topo, parent := fullParent(t)
	topo.AddAgent("failed-over", "failed-over", "10.0.0.20:50052")

	if !topo.AttachObserved("failed-over", parent) {
		t.Fatal("AttachObserved refused an attachment that already exists in the mesh")
	}

	n, ok := topo.Get("failed-over")
	if !ok {
		t.Fatal("agent vanished from the topology")
	}
	if n.ParentID != parent {
		t.Errorf("parent = %q, want %q", n.ParentID, parent)
	}
}

// TestAttachObservedMovesAnAgentOffADeadParent is the scenario from the
// chaos run: the old parent is gone and the agent reports its new one.
func TestAttachObservedMovesAnAgentOffADeadParent(t *testing.T) {
	topo := NewMeshTopology(DefaultTopologyConfig())
	for _, id := range []string{"old-zl", "new-zl"} {
		topo.AddAgent(id, id, "10.0.0.1:50052")
		topo.AssignZoneLeader(id)
	}
	topo.AddAgent("orphan", "orphan", "10.0.0.5:50052")
	if !topo.AssignChild("orphan", "old-zl") {
		t.Fatal("setup: could not attach orphan to old-zl")
	}
	topo.MarkOffline("old-zl")

	if !topo.AttachObserved("orphan", "new-zl") {
		t.Fatal("AttachObserved refused to move an agent off a dead parent")
	}
	n, _ := topo.Get("orphan")
	if n.ParentID != "new-zl" {
		t.Errorf("parent = %q, want new-zl — the agent is still recorded under the dead node", n.ParentID)
	}
	// The old parent must not still claim it, or subtree walks double-count.
	for _, c := range topo.ChildrenOf("old-zl") {
		if c.ID == "orphan" {
			t.Error("the dead parent still lists the agent as its child")
		}
	}
}

// A cycle would make the zone-leader walk and every subtree traversal loop
// forever, so it must be refused however insistently it is reported.
func TestAttachObservedRefusesCycles(t *testing.T) {
	topo := NewMeshTopology(DefaultTopologyConfig())
	topo.AddAgent("zl", "zl", "10.0.0.1:50052")
	topo.AssignZoneLeader("zl")
	topo.AddAgent("mid", "mid", "10.0.0.2:50052")
	topo.AddAgent("leaf", "leaf", "10.0.0.3:50052")
	if !topo.AssignChild("mid", "zl") || !topo.AssignChild("leaf", "mid") {
		t.Fatal("setup failed")
	}

	if topo.AttachObserved("zl", "leaf") {
		t.Error("recorded an attachment that puts an ancestor under its own descendant")
	}
	if topo.AttachObserved("mid", "mid") {
		t.Error("recorded an agent as its own parent")
	}
}

func TestAttachObservedRejectsUnknownNodes(t *testing.T) {
	topo := NewMeshTopology(DefaultTopologyConfig())
	topo.AddAgent("zl", "zl", "10.0.0.1:50052")
	topo.AssignZoneLeader("zl")

	if topo.AttachObserved("ghost", "zl") {
		t.Error("attached an agent the topology has never seen")
	}
	if topo.AttachObserved("zl", "ghost") {
		t.Error("attached an agent to a parent the topology has never seen")
	}
}

// TestAttachObservedFixesReachability ties it back to why it matters: the
// reachable field walks up to a zone leader, so an agent recorded under a
// dead parent reads as unreachable even while it answers.
func TestAttachObservedFixesReachability(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	for _, id := range []string{"dead-zl", "live-zl"} {
		s.topology.AddAgent(id, id, "10.0.0.1:50052")
		s.topology.AssignZoneLeader(id)
	}
	s.topology.AddAgent("orphan", "orphan", "10.0.0.5:50052")
	if !s.topology.AssignChild("orphan", "dead-zl") {
		t.Fatal("setup failed")
	}
	connectStream(s, "live-zl") // only the survivor holds a stream

	agents := []db.Agent{{ID: "orphan", Hostname: "orphan", Online: true}}
	s.enrichWithTopology(agents)
	if agents[0].Reachable {
		t.Fatal("setup: the orphan should start unreachable under the dead zone leader")
	}

	// It fails over and its new parent reports the attachment.
	if !s.topology.AttachObserved("orphan", "live-zl") {
		t.Fatal("AttachObserved refused the failover")
	}
	agents = []db.Agent{{ID: "orphan", Hostname: "orphan", Online: true}}
	s.enrichWithTopology(agents)
	if !agents[0].Reachable {
		t.Error("still unreachable after failing over to a live zone leader")
	}
}
