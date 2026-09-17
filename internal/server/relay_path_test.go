// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/atgreen/dirq/internal/signutil"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// PathFromZoneLeader is the route a single-agent message takes. Everything it
// cannot trace has to come back nil, because the caller reads nil as "no route
// known" and falls back to broadcasting the payload to the whole fleet — the
// exposure this exists to avoid (dirq-632.1).
func TestPathFromZoneLeader(t *testing.T) {
	topo := NewMeshTopology(DefaultTopologyConfig())

	topo.AddAgent("zl", "zl", "10.0.0.1:50052")
	topo.AssignZoneLeader("zl")
	topo.AddAgent("relay", "relay", "10.0.0.2:50052")
	if !topo.AssignChild("relay", "zl") {
		t.Fatal("fixture: could not attach relay to zl")
	}
	topo.AddAgent("leaf", "leaf", "10.0.0.3:50052")
	if !topo.AssignChild("leaf", "relay") {
		t.Fatal("fixture: could not attach leaf to relay")
	}
	// An agent the topology knows but that hangs off nothing.
	topo.AddAgent("orphan", "orphan", "10.0.0.9:50052")

	cases := []struct {
		name string
		id   string
		want []string
	}{
		{"three hops", "leaf", []string{"zl", "relay", "leaf"}},
		{"two hops", "relay", []string{"zl", "relay"}},
		{"the zone leader is its own route", "zl", []string{"zl"}},
		{"orphan has no route", "orphan", nil},
		{"unknown agent has no route", "nobody", nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := topo.PathFromZoneLeader(c.id)
			if len(got) != len(c.want) {
				t.Fatalf("PathFromZoneLeader(%q) = %v, want %v", c.id, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("PathFromZoneLeader(%q) = %v, want %v", c.id, got, c.want)
				}
			}
		})
	}
}

// A cycle must not produce a route, and must not spin looking for one.
func TestPathFromZoneLeaderRefusesACycle(t *testing.T) {
	topo := NewMeshTopology(DefaultTopologyConfig())
	topo.AddAgent("zl", "zl", "10.0.0.1:50052")
	topo.AssignZoneLeader("zl")
	for _, id := range []string{"a", "b"} {
		topo.AddAgent(id, id, "10.0.0.5:50052")
		if !topo.AssignChild(id, "zl") {
			t.Fatalf("fixture: could not attach %s", id)
		}
	}

	// AttachObserved refuses to close a cycle, so reach past it to build one:
	// this is the shape the DB snapshot can persist even though the in-memory
	// topology rejects it (dirq-632.14.1).
	topo.mu.Lock()
	topo.nodes["a"].parentID = "b"
	topo.nodes["b"].parentID = "a"
	topo.mu.Unlock()

	done := make(chan []string, 1)
	go func() { done <- topo.PathFromZoneLeader("a") }()

	select {
	case got := <-done:
		if got != nil {
			t.Fatalf("a cyclic chain produced a route: %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PathFromZoneLeader did not terminate on a cyclic parent chain")
	}
}

// testSigner builds a real signer over a throwaway key so dispatch paths that
// sign can run in a test.
func testSigner(t *testing.T) *signutil.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "signing.key")
	pubFile := filepath.Join(dir, "signing.pub")
	if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(priv)), 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if err := os.WriteFile(pubFile, []byte("unused"), 0600); err != nil {
		t.Fatalf("write pub: %v", err)
	}
	s, err := signutil.LoadSigner(signutil.Config{PrivateKeyFile: keyFile, PublicKeyFile: pubFile})
	if err != nil {
		t.Fatalf("load signer: %v", err)
	}
	return s
}

// A single-agent dispatch must reach the zone leader that owns the target and
// nobody else, carrying the route the relays forward along.
func TestDispatchExecRoutesToTheOwningZoneLeader(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	s.signer = testSigner(t)

	s.topology.AddAgent("zl-a", "zl-a", "10.0.0.1:50052")
	s.topology.AssignZoneLeader("zl-a")
	s.topology.AddAgent("zl-b", "zl-b", "10.0.0.2:50052")
	s.topology.AssignZoneLeader("zl-b")
	s.topology.AddAgent("relay", "relay", "10.0.0.3:50052")
	if !s.topology.AssignChild("relay", "zl-a") {
		t.Fatal("fixture: could not attach relay")
	}
	s.topology.AddAgent("leaf", "leaf", "10.0.0.4:50052")
	if !s.topology.AssignChild("leaf", "relay") {
		t.Fatal("fixture: could not attach leaf")
	}

	streams := map[string]*agentStream{}
	for _, id := range []string{"zl-a", "zl-b"} {
		streams[id] = &agentStream{agentID: id, send: make(chan *pb.ServerMessage, 4)}
		s.streams[id] = streams[id]
	}

	go func() {
		_, _ = s.dispatchExec(context.Background(), "leaf", &pb.ServerMessage{
			Payload: &pb.ServerMessage_ExecRequest{
				ExecRequest: &pb.ExecRequest{RequestId: "exec-1", AgentId: "leaf", Command: "id"},
			},
		}, "exec-1", 2*time.Second)
	}()

	select {
	case msg := <-streams["zl-a"].send:
		want := []string{"zl-a", "relay", "leaf"}
		got := msg.GetExecRequest().GetRelayPath()
		if len(got) != len(want) {
			t.Fatalf("relay_path = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("relay_path = %v, want %v", got, want)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the owning zone leader never received the request")
	}

	if n := len(streams["zl-b"].send); n != 0 {
		t.Errorf("an unrelated zone leader received %d copies of a single-agent request", n)
	}
}
