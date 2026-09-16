// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

//go:build windows

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// TestHandleExecRequestTimeoutKillsChildrenWindows is the Windows twin of
// TestHandleExecRequestTimeoutKillsChildren (dirq-2sg). Before the tree
// kill, a timed-out exec returned promptly while the work it spawned kept
// running on the host that had been told to stop — WaitDelay bounded how
// long the agent waited, but killed nothing.
//
// The marker file is the evidence: the grandchild only creates it after
// outliving the timeout, so a marker that never appears means the tree
// really died.
func TestHandleExecRequestTimeoutKillsChildrenWindows(t *testing.T) {
	a, cs := newExecAgent(t)

	marker := filepath.Join(t.TempDir(), "child-survived.txt")

	// A detached grandchild that waits well past the timeout, then writes.
	// `start /b` backgrounds it so the immediate child can exit first,
	// which is the case a non-/T taskkill would miss.
	command := fmt.Sprintf(
		`start /b cmd /c "timeout /t 6 /nobreak >nul & echo survived > %s" & timeout /t 8 /nobreak >nul`,
		marker)

	start := time.Now()
	a.handleExecRequest(context.Background(), &pb.ExecRequest{
		RequestId:      "exec-timeout-children-win",
		AgentId:        a.agentID,
		Command:        command,
		TimeoutSeconds: 1,
	})
	elapsed := time.Since(start)

	// WaitDelay is 5s, so anything under that proves the kill landed
	// rather than the backstop expiring.
	if elapsed > 4*time.Second {
		t.Fatalf("exec was not killed by its 1s timeout (took %v)", elapsed)
	}
	execResponse(t, cs)

	// Outlive the grandchild's own wait: if the tree really died, nothing
	// is left to create the marker.
	time.Sleep(8 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("grandchild survived the timeout and created %s; the process tree was not killed", marker)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", marker, err)
	}
}

// A command that finishes on its own must be unaffected by the tree-kill
// wiring — the cancel path should never fire.
func TestWithTreeKillLeavesAFastCommandAlone(t *testing.T) {
	a, cs := newExecAgent(t)

	a.handleExecRequest(context.Background(), &pb.ExecRequest{
		RequestId:      "exec-fast-win",
		AgentId:        a.agentID,
		Command:        "echo hello",
		TimeoutSeconds: 30,
	})

	resp := execResponse(t, cs)
	if !resp.Success || resp.Rc != 0 {
		t.Fatalf("success=%v rc=%d error=%q", resp.Success, resp.Rc, resp.Error)
	}
	if len(resp.Stdout) == 0 {
		t.Error("no stdout captured")
	}
}
