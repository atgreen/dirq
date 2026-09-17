// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package signutil

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// relay_path was added inside the signed ServerMessage, which puts a
// constraint on it and on every field added after it.
//
// An agent that does not know a field keeps it as an unknown field, and the
// marshaller appends unknown fields after the known ones. So the bytes an old
// agent signs over match the bytes a new server signed only while the new
// field sorts LAST in its message. Get that wrong and every old agent rejects
// every signature — a fleet-wide outage during a rollout, with "invalid
// signature" as the only clue.
//
// This is also why relay_path lives on ExecRequest rather than on
// ServerMessage: a oneof member is itself marshalled last, so nothing added
// to the envelope can ever sort after it. The first attempt put it there and
// this test failed, which is the only reason it isn't in the release.
func TestRelayPathSurvivesAnOlderAgent(t *testing.T) {
	req := &pb.ExecRequest{
		RequestId: "exec-1",
		AgentId:   "leaf",
		Command:   "id",
		RelayPath: []string{"zl-a", "relay", "leaf"},
	}

	// What a new server produces for the nested request.
	full, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// What an old agent reconstructs: the fields it knows, then the field it
	// does not, re-emitted verbatim at the end of the same message.
	known := proto.Clone(req).(*pb.ExecRequest)
	known.RelayPath = nil
	knownBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(known)
	if err != nil {
		t.Fatalf("marshal without relay_path: %v", err)
	}
	unknownBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(
		&pb.ExecRequest{RelayPath: req.GetRelayPath()})
	if err != nil {
		t.Fatalf("marshal relay_path alone: %v", err)
	}

	reconstructed := append(append([]byte{}, knownBytes...), unknownBytes...)
	if !bytes.Equal(full, reconstructed) {
		t.Fatalf("an agent that does not know relay_path would compute different signing bytes:\n new = %x\n old = %x", full, reconstructed)
	}
}

// And the same property at the level that actually gets signed: an envelope
// carrying a route must produce the canonical bytes an older agent would
// compute for it, or signature verification fails across versions.
func TestCanonicalBytesMatchAcrossVersions(t *testing.T) {
	withRoute := &pb.ServerMessage{
		SignerKeyId:   "key-1",
		SignedAtUnix:  1700000000,
		ExpiresAtUnix: 1700000300,
		Payload: &pb.ServerMessage_ExecRequest{
			ExecRequest: &pb.ExecRequest{
				RequestId: "exec-1",
				AgentId:   "leaf",
				Command:   "id",
				RelayPath: []string{"zl-a", "relay", "leaf"},
			},
		},
	}

	signed, err := canonicalMessageBytes(withRoute)
	if err != nil {
		t.Fatalf("canonical bytes: %v", err)
	}

	// Round-tripping through the wire is what an agent does before verifying;
	// an old agent's copy differs only in that relay_path sits in its unknown
	// fields. Re-deriving the canonical form must land on the same bytes.
	wire, err := proto.Marshal(withRoute)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var received pb.ServerMessage
	if err := proto.Unmarshal(wire, &received); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	roundTripped, err := canonicalMessageBytes(&received)
	if err != nil {
		t.Fatalf("canonical bytes after round trip: %v", err)
	}

	if !bytes.Equal(signed, roundTripped) {
		t.Fatalf("canonical bytes changed across a wire round trip:\n signed = %x\n got    = %x", signed, roundTripped)
	}
}
