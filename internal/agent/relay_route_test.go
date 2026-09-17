// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package agent

import (
	"log/slog"
	"testing"

	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// An exec request carries a command, a script and stdin — for Ansible runs,
// module arguments holding whatever the playbook decrypted. Relaying that to
// every downstream peer hands it to agents that have nothing to do with the
// target, which is how one compromised host reads the credentials being
// pushed to every other host (dirq-632.1).

func relayTestAgent(t *testing.T, id string, children ...string) *Agent {
	t.Helper()
	a := &Agent{
		agentID:     id,
		log:         slog.Default(),
		downstreams: map[string]*downstreamPeer{},
	}
	for _, c := range children {
		a.downstreams[c] = &downstreamPeer{agentID: c, send: make(chan *pb.ServerMessage, 4)}
	}
	return a
}

// delivered reports which peers received a copy.
func delivered(a *Agent) map[string]int {
	got := map[string]int{}
	for id, ds := range a.downstreams {
		got[id] = len(ds.send)
	}
	return got
}

func TestRelayForwardsOnlyToTheNextHop(t *testing.T) {
	// relay-1 sits between the zone leader and two children; the route runs
	// through leaf-a, so leaf-b must not see the payload.
	a := relayTestAgent(t, "relay-1", "leaf-a", "leaf-b")

	a.relayToDownstreams(&pb.ServerMessage{
		Payload: &pb.ServerMessage_ExecRequest{
			ExecRequest: &pb.ExecRequest{
				AgentId:   "leaf-a",
				Command:   "rpm -q openssl",
				RelayPath: []string{"zl", "relay-1", "leaf-a"},
			},
		},
	})

	got := delivered(a)
	if got["leaf-a"] != 1 {
		t.Errorf("the next hop got %d copies, want 1", got["leaf-a"])
	}
	if got["leaf-b"] != 0 {
		t.Errorf("a peer off the route got %d copies of a payload meant for another host", got["leaf-b"])
	}
}

func TestRelayFloodsWhenThereIsNoRoute(t *testing.T) {
	// A broadcast carries no route. Every child must still get it.
	a := relayTestAgent(t, "relay-1", "leaf-a", "leaf-b")

	a.relayToDownstreams(&pb.ServerMessage{
		Payload: &pb.ServerMessage_QueryRequest{
			QueryRequest: &pb.QueryRequest{QueryId: "q-1"},
		},
	})

	for id, n := range delivered(a) {
		if n != 1 {
			t.Errorf("%s got %d copies of a broadcast, want 1", id, n)
		}
	}
}

func TestRelayDoesNotForwardWhenTheRouteEndsHere(t *testing.T) {
	// The target itself never relays onward: handleServerMessage runs the
	// execute branch, but if the relay path is ever consulted for the last
	// hop it must not copy the payload into the target's own subtree.
	a := relayTestAgent(t, "leaf-a", "grandchild")

	a.relayToDownstreams(&pb.ServerMessage{
		Payload: &pb.ServerMessage_PutFile{
			PutFile: &pb.PutFileRequest{
				AgentId:   "leaf-a",
				DestPath:  "/etc/app/secret.conf",
				RelayPath: []string{"zl", "relay-1", "leaf-a"},
			},
		},
	})

	if n := delivered(a)["grandchild"]; n != 0 {
		t.Errorf("the route ended here but %d copies went further down", n)
	}
}

func TestRelayOffTheRouteForwardsNothing(t *testing.T) {
	// A relay that is not named in the route was not supposed to receive
	// this at all. Flooding "just in case" would reintroduce the exposure.
	a := relayTestAgent(t, "relay-9", "leaf-x", "leaf-y")

	a.relayToDownstreams(&pb.ServerMessage{
		Payload: &pb.ServerMessage_ExecRequest{
			ExecRequest: &pb.ExecRequest{
				AgentId:   "leaf-a",
				Command:   "id",
				RelayPath: []string{"zl", "relay-1", "leaf-a"},
			},
		},
	})

	for id, n := range delivered(a) {
		if n != 0 {
			t.Errorf("%s got %d copies from a relay that is not on the route", id, n)
		}
	}
}

func TestRelayFallsBackToTheSubtreeWhenTheNextHopIsGone(t *testing.T) {
	// The route names a child that is no longer connected — it reattached
	// elsewhere, or has not attached yet. Dropping the message would fail an
	// exec that a flood would have delivered, so this floods, bounded to
	// this agent's own subtree.
	a := relayTestAgent(t, "relay-1", "leaf-b")

	a.relayToDownstreams(&pb.ServerMessage{
		Payload: &pb.ServerMessage_ExecRequest{
			ExecRequest: &pb.ExecRequest{
				AgentId:   "leaf-a",
				Command:   "id",
				RelayPath: []string{"zl", "relay-1", "leaf-a"},
			},
		},
	})

	if n := delivered(a)["leaf-b"]; n != 1 {
		t.Errorf("subtree fallback delivered %d copies, want 1", n)
	}
}

func TestNextHop(t *testing.T) {
	a := relayTestAgent(t, "relay-1")

	cases := []struct {
		name       string
		path       []string
		wantNext   string
		wantRouted bool
	}{
		{"no route means flood", nil, "", false},
		{"empty route means flood", []string{}, "", false},
		{"middle of the route", []string{"zl", "relay-1", "leaf-a"}, "leaf-a", true},
		{"head of the route", []string{"relay-1", "leaf-a"}, "leaf-a", true},
		{"end of the route", []string{"zl", "relay-1"}, "", true},
		{"not on the route", []string{"zl", "relay-2", "leaf-a"}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			next, routed := a.nextHop(c.path)
			if next != c.wantNext || routed != c.wantRouted {
				t.Errorf("nextHop(%v) = (%q, %v), want (%q, %v)",
					c.path, next, routed, c.wantNext, c.wantRouted)
			}
		})
	}
}
