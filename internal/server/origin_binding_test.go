// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"testing"

	"github.com/atgreen/dirq/internal/db"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// A zone leader's stream carries its whole subtree, so the mTLS CN
// authenticates the zone leader and nothing else. Every message riding that
// stream names its own origin in a payload field, and until these checks
// existed the server believed the field. One compromised agent could answer
// for any host in the fleet, overwrite any host's facts, and mark any host
// offline (dirq-632.2).
//
// The fixture is two zones that share nothing, so "came from the wrong zone"
// is unambiguous:
//
//	zl-a ── leaf-a          zl-b ── leaf-b
//
// Every test below has zone A's stream say something about zone B.

func originTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(&mockDB{}, true)
	s.cfg.AgentOriginChecks = OriginEnforce
	s.factStage = make(map[factKey]db.FactRow)
	s.factFlushSignal = make(chan struct{}, 1)

	for _, zl := range []string{"zl-a", "zl-b"} {
		s.topology.AddAgent(zl, zl, "10.0.0.1:50052")
		s.topology.AssignZoneLeader(zl)
	}
	for _, leaf := range []struct{ id, parent string }{
		{"leaf-a", "zl-a"},
		{"leaf-b", "zl-b"},
	} {
		s.topology.AddAgent(leaf.id, leaf.id, "10.0.0.2:50052")
		if !s.topology.AssignChild(leaf.id, leaf.parent) {
			t.Fatalf("fixture: could not attach %s to %s", leaf.id, leaf.parent)
		}
		s.topology.MarkOnline(leaf.id)
	}
	return s
}

// newExecSession registers a single-host exec session targeting one agent,
// mirroring what dispatchExec sets up before it sends the request.
func (s *Server) newExecSession(t *testing.T, requestID, targetAgentID string) *execSession {
	t.Helper()
	es := &execSession{
		requestID:     requestID,
		targetAgentID: targetAgentID,
		result:        make(chan any, 1),
	}
	s.execMu.Lock()
	s.execSessions[requestID] = es
	s.execMu.Unlock()
	return es
}

// ─────────────────────────────────────────────────────────
// Manifestation 1 — answering for another host
// ─────────────────────────────────────────────────────────

func TestExecResponseFromAForeignZoneIsRejected(t *testing.T) {
	s := originTestServer(t)
	es := s.newExecSession(t, "exec-1", "leaf-b")

	// zone A's stream answers for a host in zone B.
	s.handleExecResponse("zl-a", &pb.ExecResponse{
		RequestId: "exec-1",
		AgentId:   "leaf-b",
		Stdout:    []byte("openssl-3.2.2-1 (patched)"),
		Success:   true,
	})

	select {
	case got := <-es.result:
		t.Fatalf("forged exec response was delivered to the operator: %+v", got)
	default:
	}
}

func TestExecResponseNamingADifferentAgentIsRejected(t *testing.T) {
	s := originTestServer(t)
	es := s.newExecSession(t, "exec-1", "leaf-b")

	// leaf-a answers on its own legitimate stream, but for someone else's
	// request. The origin is honest; the target is not.
	s.handleExecResponse("zl-a", &pb.ExecResponse{
		RequestId: "exec-1",
		AgentId:   "leaf-a",
		Success:   true,
	})

	select {
	case got := <-es.result:
		t.Fatalf("response for the wrong agent satisfied the session: %+v", got)
	default:
	}
}

func TestExecResponseFromTheTargetIsAccepted(t *testing.T) {
	s := originTestServer(t)
	es := s.newExecSession(t, "exec-1", "leaf-b")

	s.handleExecResponse("zl-b", &pb.ExecResponse{
		RequestId: "exec-1",
		AgentId:   "leaf-b",
		Success:   true,
	})

	select {
	case <-es.result:
	default:
		t.Fatal("the target's own response was rejected")
	}
}

func TestFetchResponseFromAForeignZoneIsRejected(t *testing.T) {
	s := originTestServer(t)
	es := s.newExecSession(t, "fetch-1", "leaf-b")

	s.handleFetchResponse("zl-a", &pb.FetchFileResponse{
		RequestId: "fetch-1",
		AgentId:   "leaf-b",
		Content:   []byte("attacker-supplied file body"),
		Success:   true,
	})

	select {
	case got := <-es.result:
		t.Fatalf("forged fetch_file content reached the operator: %+v", got)
	default:
	}
}

func TestFileChunkFromAForeignZoneIsRejected(t *testing.T) {
	s := originTestServer(t)
	es := s.newExecSession(t, "put-1", "leaf-b")

	s.handleFileChunk("zl-a", &pb.FileChunk{
		RequestId: "put-1",
		AgentId:   "leaf-b",
		Success:   true,
	})

	select {
	case got := <-es.result:
		t.Fatalf("forged put_file ack reached the operator: %+v", got)
	default:
	}
}

// ─────────────────────────────────────────────────────────
// Manifestation 2 — claiming the victim's accounting slot
// ─────────────────────────────────────────────────────────

func TestForgedBroadcastResponseDoesNotClaimTheVictimsSlot(t *testing.T) {
	s := originTestServer(t)

	bs := &execBroadcastSession{
		requestID:         "bcast-1",
		results:           make(chan *pb.ExecResponse, 4),
		sessionAccounting: newSessionAccounting([]string{"leaf-b"}),
	}
	execBroadcastSessionsMu.Lock()
	execBroadcastSessions["bcast-1"] = bs
	execBroadcastSessionsMu.Unlock()
	defer func() {
		execBroadcastSessionsMu.Lock()
		delete(execBroadcastSessions, "bcast-1")
		execBroadcastSessionsMu.Unlock()
	}()

	// Zone A forges leaf-b's answer first.
	s.handleExecBroadcastResponse("zl-a", &pb.ExecResponse{
		RequestId: "bcast-1",
		AgentId:   "leaf-b",
		Success:   true,
		Stdout:    []byte("all good, nothing to patch"),
	})

	// The real host answers afterwards. First-terminal-wins means that if the
	// forgery was allowed to claim the slot, this is silently discarded — the
	// operator sees the attacker's output and never learns of the real one.
	s.handleExecBroadcastResponse("zl-b", &pb.ExecResponse{
		RequestId: "bcast-1",
		AgentId:   "leaf-b",
		Success:   false,
		Stdout:    []byte("openssl-1.1.1k-9 (vulnerable)"),
	})

	select {
	case got := <-bs.results:
		if got.Success {
			t.Fatal("the forged broadcast response won the victim's slot")
		}
	default:
		t.Fatal("the victim's own response never arrived")
	}
}

// ─────────────────────────────────────────────────────────
// Manifestation 3 — poisoning another host's facts
// ─────────────────────────────────────────────────────────

func TestForgedQueryResultDoesNotPoisonAnotherHostsFacts(t *testing.T) {
	s := originTestServer(t)

	data, err := structpb.NewStruct(map[string]any{
		"packages": map[string]any{"packages": []any{}},
	})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}

	s.handleQueryResult("zl-a", &pb.QueryResult{
		QueryId: "q-1",
		AgentId: "leaf-b",
		Success: true,
		Data:    data,
	})

	s.factStageMu.Lock()
	_, staged := s.factStage[factKey{agentID: "leaf-b", module: "packages"}]
	s.factStageMu.Unlock()

	if staged {
		t.Fatal("zone A wrote facts for a host in zone B")
	}
}

func TestQueryResultFromTheAgentsOwnZoneIsStaged(t *testing.T) {
	s := originTestServer(t)

	data, err := structpb.NewStruct(map[string]any{
		"packages": map[string]any{"packages": []any{}},
	})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}

	s.handleQueryResult("zl-b", &pb.QueryResult{
		QueryId: "q-1",
		AgentId: "leaf-b",
		Success: true,
		Data:    data,
	})

	s.factStageMu.Lock()
	_, staged := s.factStage[factKey{agentID: "leaf-b", module: "packages"}]
	s.factStageMu.Unlock()

	if !staged {
		t.Fatal("an honest agent's facts were dropped")
	}
}

// ─────────────────────────────────────────────────────────
// Manifestation 4 — topology and liveness claims
// ─────────────────────────────────────────────────────────

func TestPeerDisconnectedForAForeignAgentIsIgnored(t *testing.T) {
	s := originTestServer(t)

	// A relay can only lose a child it actually had. zone A reporting the
	// death of a zone B host is never legitimate — and it is the cheapest
	// way to drop a host out of every fleet-wide patch wave.
	s.handlePeerDisconnected(context.Background(), "zl-a", &pb.PeerDisconnected{
		AgentId: "leaf-b",
	})

	n, ok := s.topology.Get("leaf-b")
	if !ok {
		t.Fatal("leaf-b vanished from the topology")
	}
	if !n.Online {
		t.Fatal("zone A marked a zone B host offline")
	}
}

func TestPeerDisconnectedForAnOwnChildIsHonoured(t *testing.T) {
	s := originTestServer(t)

	s.handlePeerDisconnected(context.Background(), "zl-b", &pb.PeerDisconnected{
		AgentId: "leaf-b",
	})

	n, ok := s.topology.Get("leaf-b")
	if !ok {
		t.Fatal("leaf-b vanished from the topology")
	}
	if n.Online {
		t.Fatal("a relay's report about its own child was ignored")
	}
}

func TestPeerConnectedAboutAForeignParentIsIgnored(t *testing.T) {
	s := originTestServer(t)

	// zone A describing zone B's internal shape. A relay only ever reports
	// attachments to itself or to something beneath it.
	s.handlePeerConnected(context.Background(), "zl-a", &pb.PeerConnected{
		AgentId:  "leaf-a",
		ParentId: "zl-b",
	})

	n, ok := s.topology.Get("leaf-a")
	if !ok {
		t.Fatal("leaf-a vanished from the topology")
	}
	if n.ParentID != "zl-a" {
		t.Fatalf("zone A reparented a host into zone B: parent is now %q", n.ParentID)
	}
}

func TestPeerConnectedAboutAnOwnChildIsHonoured(t *testing.T) {
	s := originTestServer(t)
	s.topology.AddAgent("mover", "mover", "10.0.0.3:50052")
	if !s.topology.AssignChild("mover", "zl-b") {
		t.Fatal("fixture: could not attach mover to zl-b")
	}

	// A genuine failover: mover reattached to zl-a, and zl-a reports it.
	s.handlePeerConnected(context.Background(), "zl-a", &pb.PeerConnected{
		AgentId:  "mover",
		ParentId: "zl-a",
	})

	n, ok := s.topology.Get("mover")
	if !ok {
		t.Fatal("mover vanished from the topology")
	}
	if n.ParentID != "zl-a" {
		t.Fatalf("a legitimate reattachment was refused: parent is %q", n.ParentID)
	}
}

// ─────────────────────────────────────────────────────────
// Observe mode
// ─────────────────────────────────────────────────────────

func TestObserveModeCountsButDoesNotReject(t *testing.T) {
	s := originTestServer(t)
	s.cfg.AgentOriginChecks = OriginObserve
	es := s.newExecSession(t, "exec-1", "leaf-b")

	s.handleExecResponse("zl-a", &pb.ExecResponse{
		RequestId: "exec-1",
		AgentId:   "leaf-b",
		Success:   true,
	})

	select {
	case <-es.result:
	default:
		t.Fatal("observe mode rejected a message; it must only count and log")
	}
}

// The target check is not topology-derived, so it carries no reattachment-lag
// risk and is enforced in every mode — including off.
func TestTargetMismatchIsRejectedEvenWhenChecksAreOff(t *testing.T) {
	s := originTestServer(t)
	s.cfg.AgentOriginChecks = OriginOff
	es := s.newExecSession(t, "exec-1", "leaf-b")

	s.handleExecResponse("zl-a", &pb.ExecResponse{
		RequestId: "exec-1",
		AgentId:   "leaf-a",
		Success:   true,
	})

	select {
	case got := <-es.result:
		t.Fatalf("a response for the wrong agent satisfied the session: %+v", got)
	default:
	}
}

// ─────────────────────────────────────────────────────────
// Subtree predicate
// ─────────────────────────────────────────────────────────

func TestIsWithinSubtree(t *testing.T) {
	s := originTestServer(t)
	topo := s.topology

	cases := []struct {
		ancestor, id string
		want         bool
	}{
		{"zl-a", "leaf-a", true},
		{"zl-a", "zl-a", true}, // a node is within its own subtree
		{"zl-a", "leaf-b", false},
		{"zl-b", "leaf-a", false},
		{"zl-a", "nobody", false},
		{"nobody", "leaf-a", false},
	}
	for _, c := range cases {
		if got := topo.IsWithinSubtree(c.ancestor, c.id); got != c.want {
			t.Errorf("IsWithinSubtree(%q, %q) = %v, want %v", c.ancestor, c.id, got, c.want)
		}
	}
}
