// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

// Package wire holds the size limits the server and every agent must agree
// on. A payload limit and the transport's message limit are one decision:
// when they drift apart, the hop that receives an oversized message drops
// the whole stream, taking every other in-flight result with it.
package wire

import "google.golang.org/grpc"

// MaxFileBytes caps a single file moved through the mesh: put_file,
// fetch_file, and a broadcast deploy's package.
const MaxFileBytes = 100 * 1024 * 1024

// MaxOutputBytes caps how much stdout or stderr an agent keeps from one
// command.
const MaxOutputBytes = 10 * 1024 * 1024

// MaxMessageBytes is the largest gRPC message any DirQ endpoint accepts:
// the biggest payload above plus room for the envelope around it. gRPC's
// own default is 4 MiB, which every file and long command output exceeds.
const MaxMessageBytes = MaxFileBytes + 4*1024*1024

// ServerOptions raises the receive limit on a gRPC server: the DirQ server
// and each agent's relay listener.
func ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{grpc.MaxRecvMsgSize(MaxMessageBytes)}
}

// DialOptions raises the receive limit on a connection an agent dials, to
// the server or to its parent relay.
func DialOptions() []grpc.DialOption {
	return []grpc.DialOption{grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxMessageBytes))}
}
