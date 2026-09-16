// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"log/slog"
	"testing"
)

// TestStopBeforeStart pins that shutting down a server that never came up
// is a no-op. Start assigns grpcSv and httpSv partway through, so every
// error path before that point used to turn a deferred Stop into a nil
// pointer dereference — which then buried the startup error that caused
// it. Callers legitimately write "start it, defer stopping it".
func TestStopBeforeStart(t *testing.T) {
	s := New(Config{PodID: "test"}, &mockDB{}, slog.Default())

	// Must not panic.
	s.Stop()
}

// TestStopAfterFailedStart is the case that actually bit: Start returns an
// error and the deferred Stop runs anyway.
func TestStopAfterFailedStart(t *testing.T) {
	// An unroutable gRPC address makes Start fail at bind, after config
	// but before the servers are constructed.
	s := New(Config{
		GRPCAddr: "256.256.256.256:1",
		HTTPAddr: "256.256.256.256:2",
		PodID:    "test",
	}, &mockDB{}, slog.Default())

	if err := s.Start(context.Background()); err == nil {
		t.Skip("Start unexpectedly succeeded; nothing to assert")
	}

	// The whole point: this is what panicked.
	s.Stop()
}
