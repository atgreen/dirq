// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package wire

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// The largest result an agent is allowed to build must fit in one message,
// or the hop carrying it drops the whole stream (dirq-gdj).
func TestLargestPayloadsFitInOneMessage(t *testing.T) {
	cases := map[string]proto.Message{
		"exec response": &pb.AgentMessage{Payload: &pb.AgentMessage_ExecResponse{ExecResponse: &pb.ExecResponse{
			Stdout: make([]byte, MaxOutputBytes), Stderr: make([]byte, MaxOutputBytes),
		}}},
		"fetch response": &pb.AgentMessage{Payload: &pb.AgentMessage_FetchResponse{FetchResponse: &pb.FetchFileResponse{
			Content: make([]byte, MaxFileBytes),
		}}},
		"put file": &pb.ServerMessage{Payload: &pb.ServerMessage_PutFile{PutFile: &pb.PutFileRequest{
			Content: make([]byte, MaxFileBytes),
		}}},
		"deploy": &pb.ServerMessage{Payload: &pb.ServerMessage_DeployRequest{DeployRequest: &pb.DeployRequest{
			Content: make([]byte, MaxFileBytes),
		}}},
	}
	for name, m := range cases {
		if n := proto.Size(m); n > MaxMessageBytes {
			t.Errorf("%s is %d bytes, over MaxMessageBytes %d", name, n, MaxMessageBytes)
		}
	}
}

// A message over gRPC's 4 MiB default must cross a real connection in both
// directions when both ends use these options.
func TestOptionsCarryMessagesPastGRPCDefault(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	echo := func(_ any, stream grpc.ServerStream) error {
		var m wrapperspb.BytesValue
		if err := stream.RecvMsg(&m); err != nil {
			return err
		}
		return stream.SendMsg(&m)
	}
	srv := grpc.NewServer(append(ServerOptions(), grpc.UnknownServiceHandler(echo))...)
	go srv.Serve(lis)
	defer srv.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(),
		append(DialOptions(), grpc.WithTransportCredentials(insecure.NewCredentials()))...)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	in := wrapperspb.Bytes(bytes.Repeat([]byte{'x'}, 2*MaxOutputBytes))
	var out wrapperspb.BytesValue
	if err := conn.Invoke(ctx, "/dirq.test.Echo/Echo", in, &out); err != nil {
		t.Fatalf("%d-byte round trip failed: %v", len(in.Value), err)
	}
	if len(out.Value) != len(in.Value) {
		t.Fatalf("echoed %d bytes, sent %d", len(out.Value), len(in.Value))
	}
}
