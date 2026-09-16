// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package agent

import (
	"bytes"
	"fmt"
)

// maxOutputBytes caps how much stdout or stderr the agent keeps from any
// one command. Output is accumulated in memory before being sent upstream,
// so without a cap a command that prints without stopping — a runaway loop,
// a debug build left verbose, a deliberately hostile one — grows the
// agent's heap until the host runs out of memory. The agent is the process
// that must survive to report the problem, so it is the process that must
// refuse to hold everything.
//
// 10 MB is far past any output a person reads and far below anything that
// threatens a modest host.
const maxOutputBytes = 10 * 1024 * 1024

// boundedBuffer is an io.Writer that keeps at most max bytes and counts
// what it drops.
type boundedBuffer struct {
	buf     bytes.Buffer
	max     int
	dropped int
}

func newBoundedBuffer(max int) *boundedBuffer { return &boundedBuffer{max: max} }

// Write always reports the full write, even when it kept nothing. Returning
// n < len(p) would make os/exec abort the command with io.ErrShortWrite,
// turning "this command is chatty" into "this command failed" — a far worse
// outcome than dropping output nobody was going to read.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) <= room {
			b.buf.Write(p)
			return len(p), nil
		}
		b.buf.Write(p[:room])
		b.dropped += len(p) - room
		return len(p), nil
	}
	b.dropped += len(p)
	return len(p), nil
}

// Truncated reports whether anything was dropped.
func (b *boundedBuffer) Truncated() bool { return b.dropped > 0 }

// Dropped returns how many bytes were discarded.
func (b *boundedBuffer) Dropped() int { return b.dropped }

// Bytes returns the captured output, with a trailing notice when output was
// dropped. Without the notice a truncated result is indistinguishable from a
// command that simply stopped talking, which is how a person reading it
// reaches a confident wrong conclusion.
func (b *boundedBuffer) Bytes() []byte {
	if b.dropped == 0 {
		return b.buf.Bytes()
	}
	out := make([]byte, 0, b.buf.Len()+80)
	out = append(out, b.buf.Bytes()...)
	return append(out, fmt.Sprintf(
		"\n[dirq: output truncated at %d bytes; %d further bytes discarded]\n",
		b.max, b.dropped)...)
}

// String is Bytes as a string, notice included.
func (b *boundedBuffer) String() string { return string(b.Bytes()) }
