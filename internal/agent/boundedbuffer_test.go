// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package agent

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

func TestBoundedBuffer_KeepsEverythingUnderTheCap(t *testing.T) {
	b := newBoundedBuffer(100)

	n, err := b.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
	}
	if b.Truncated() {
		t.Error("Truncated() = true for output well under the cap")
	}
	if got := b.String(); got != "hello" {
		t.Errorf("String() = %q, want %q", got, "hello")
	}
}

func TestBoundedBuffer_KeepsExactlyTheCap(t *testing.T) {
	b := newBoundedBuffer(5)
	b.Write([]byte("hello"))

	// Exactly at the cap is not truncation — nothing was dropped.
	if b.Truncated() {
		t.Error("Truncated() = true when the output exactly filled the cap")
	}
	if got := b.String(); got != "hello" {
		t.Errorf("String() = %q, want %q", got, "hello")
	}
}

func TestBoundedBuffer_DropsTheOverflowAndSaysSo(t *testing.T) {
	b := newBoundedBuffer(5)
	b.Write([]byte("hello world"))

	if !b.Truncated() {
		t.Fatal("Truncated() = false after dropping output")
	}
	if got := b.Dropped(); got != 6 {
		t.Errorf("Dropped() = %d, want 6", got)
	}
	got := b.String()
	if !strings.HasPrefix(got, "hello") {
		t.Errorf("String() = %q, want it to start with the retained output", got)
	}
	// A truncated result must not look like a command that stopped talking.
	if !strings.Contains(got, "truncated") {
		t.Errorf("String() = %q, want a truncation notice", got)
	}
	if !strings.Contains(got, "6 further bytes") {
		t.Errorf("String() = %q, want the dropped count", got)
	}
}

// TestBoundedBuffer_NeverReportsAShortWrite is the property that keeps a
// chatty command from becoming a failed one: os/exec aborts the process
// with io.ErrShortWrite if a writer returns n < len(p).
func TestBoundedBuffer_NeverReportsAShortWrite(t *testing.T) {
	b := newBoundedBuffer(4)

	for _, chunk := range [][]byte{
		[]byte("ab"),                    // fits
		[]byte("cdef"),                  // straddles the cap
		[]byte("ghij"),                  // entirely past it
		[]byte(""),                      // empty
		bytes.Repeat([]byte("x"), 1000), // far past it
	} {
		n, err := b.Write(chunk)
		if err != nil {
			t.Fatalf("Write(%d bytes) returned error %v", len(chunk), err)
		}
		if n != len(chunk) {
			t.Fatalf("Write(%d bytes) reported %d — a short write aborts the command", len(chunk), n)
		}
	}

	if got := b.Dropped(); got != 1006 {
		t.Errorf("Dropped() = %d, want 1006", got)
	}
}

func TestBoundedBuffer_AccumulatesAcrossWrites(t *testing.T) {
	b := newBoundedBuffer(10)
	for i := 0; i < 4; i++ {
		b.Write([]byte("abc"))
	}

	// 12 bytes written into a 10-byte cap.
	if got := b.Dropped(); got != 2 {
		t.Errorf("Dropped() = %d, want 2", got)
	}
	if !strings.HasPrefix(b.String(), "abcabcabca") {
		t.Errorf("String() = %q, want the first 10 bytes retained", b.String())
	}
}

func TestBoundedBuffer_ZeroCapKeepsNothingButStillAccepts(t *testing.T) {
	b := newBoundedBuffer(0)

	n, err := b.Write([]byte("anything"))
	if n != 8 || err != nil {
		t.Fatalf("Write = (%d, %v), want (8, nil)", n, err)
	}
	if got := b.Dropped(); got != 8 {
		t.Errorf("Dropped() = %d, want 8", got)
	}
}

func TestBoundedBuffer_BytesIsCleanWhenNothingDropped(t *testing.T) {
	b := newBoundedBuffer(100)
	b.Write([]byte("clean output"))

	// No notice may be appended when nothing was lost — the bytes are the
	// command's, verbatim.
	if got := string(b.Bytes()); got != "clean output" {
		t.Errorf("Bytes() = %q, want the output unchanged", got)
	}
}

// ─────────────────────────────────────────────────────────
// readFileViaCommand — the privileged fetch path's size guard
// ─────────────────────────────────────────────────────────

// The direct-read fetch path stats a file and refuses anything over
// maxFileSize. The privileged path cannot stat — that is why it shells out
// through sudo — so it bounds the read instead. These exercise that guard
// without needing sudo, by running the same shape of command directly.

func TestReadFileViaCommand_ReturnsSmallOutput(t *testing.T) {
	skipOnWindows(t)

	got, err := readFileViaCommand(exec.Command("sh", "-c", "printf 'file contents'"))
	if err != nil {
		t.Fatalf("readFileViaCommand: %v", err)
	}
	if string(got) != "file contents" {
		t.Errorf("content = %q, want %q", got, "file contents")
	}
}

// TestReadFileViaCommand_RefusesOversizeRatherThanTruncating is the point of
// the guard: a truncated file returned as if whole means the caller writes a
// corrupt copy and believes it has the original.
func TestReadFileViaCommand_RefusesOversizeRatherThanTruncating(t *testing.T) {
	skipOnWindows(t)
	if testing.Short() {
		t.Skip("produces maxFileSize+ bytes")
	}

	over := maxFileSize + 1024*1024
	cmd := exec.Command("sh", "-c", fmt.Sprintf("dd if=/dev/zero bs=1024 count=%d 2>/dev/null", over/1024))

	got, err := readFileViaCommand(cmd)
	if err == nil {
		t.Fatalf("readFileViaCommand returned %d bytes and no error, want a refusal", len(got))
	}
	if !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("error = %v, want it to name the size limit", err)
	}
	if got != nil {
		t.Errorf("returned %d bytes alongside the error, want nil", len(got))
	}
}

func TestReadFileViaCommand_ReportsTheCommandsStderr(t *testing.T) {
	skipOnWindows(t)

	_, err := readFileViaCommand(exec.Command("sh", "-c", "echo 'permission denied' >&2; exit 1"))
	if err == nil {
		t.Fatal("a failing command returned no error")
	}
	// Whoever reads the failure needs the reason, not just "exit status 1".
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error = %v, want it to carry the command's stderr", err)
	}
}
