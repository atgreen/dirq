// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package agent

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// An agent is detached from its parent more often than it looks: between
// registering and attaching, and for the whole of every reconnect after a
// parent goes away. Meanwhile its children are relaying through it. The
// send path has to survive that rather than dereference a nil stream and
// take the agent — and its entire subtree — off the mesh (dirq-4mn).

func newUpstreamAgent() *Agent {
	return &Agent{
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		agentID: "agent-test",
	}
}

func TestUpstreamSend_WithNoParentReturnsAnError(t *testing.T) {
	a := newUpstreamAgent()

	// The exact situation that crashed: nothing attached upstream.
	err := a.upstreamSend(&pb.AgentMessage{})

	if err == nil {
		t.Fatal("sending with no upstream returned no error")
	}
	if !errors.Is(err, errNoUpstream) {
		t.Errorf("err = %v, want errNoUpstream", err)
	}
}

func TestUpstreamGet_IsNilUntilAStreamIsSet(t *testing.T) {
	a := newUpstreamAgent()

	if a.upstreamGet() != nil {
		t.Fatal("a fresh agent reports an upstream stream")
	}

	fake := &fakeUpstream{}
	a.setUpstreamStream(fake)
	if a.upstreamGet() == nil {
		t.Error("stream was set but upstreamGet reports none")
	}

	// Losing the parent clears it again, and a send must not panic after.
	a.setUpstreamStream(nil)
	if a.upstreamGet() != nil {
		t.Error("stream was cleared but upstreamGet still reports one")
	}
	if err := a.upstreamSend(&pb.AgentMessage{}); !errors.Is(err, errNoUpstream) {
		t.Errorf("after detaching, err = %v, want errNoUpstream", err)
	}
}

func TestUpstreamSend_DeliversWhenAttached(t *testing.T) {
	a := newUpstreamAgent()
	fake := &fakeUpstream{}
	a.setUpstreamStream(fake)

	if err := a.upstreamSend(&pb.AgentMessage{}); err != nil {
		t.Fatalf("upstreamSend: %v", err)
	}
	if got := fake.count(); got != 1 {
		t.Errorf("stream received %d messages, want 1", got)
	}
}

// TestUpstreamSend_RaceWithReconnect runs relays forwarding while the
// connect loop swaps the stream underneath them, which is what happens
// while a tree is forming. Under -race this fails if the field is touched
// without the lock; without the fix it panics outright.
func TestUpstreamSend_RaceWithReconnect(t *testing.T) {
	a := newUpstreamAgent()

	var wg sync.WaitGroup
	const senders, rounds = 8, 200

	// Reconnect loop: attach, detach, attach...
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if i%2 == 0 {
				a.setUpstreamStream(&fakeUpstream{})
			} else {
				a.setUpstreamStream(nil)
			}
		}
	}()

	// Relay goroutines forwarding a child's traffic throughout.
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				// Either outcome is fine; crashing is not.
				_ = a.upstreamSend(&pb.AgentMessage{})
			}
		}()
	}
	wg.Wait()
}

// fakeUpstream is a DirQServer_AgentStreamClient that counts sends.
type fakeUpstream struct {
	pb.DirQServer_AgentStreamClient
	mu   sync.Mutex
	sent int
}

func (f *fakeUpstream) Send(*pb.AgentMessage) error {
	f.mu.Lock()
	f.sent++
	f.mu.Unlock()
	return nil
}

func (f *fakeUpstream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent
}
