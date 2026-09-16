// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

//go:build windows

package agent

import (
	"os/exec"
	"strconv"
	"syscall"
)

// withTreeKill makes context cancellation terminate everything the
// command started, not just the command itself.
//
// This is the Windows half of the Unix process-group kill. Windows has no
// process groups to signal, so the two pieces here stand in for one:
//
//   - CREATE_NEW_PROCESS_GROUP puts the child at the root of its own
//     group, so it and its descendants are distinguishable from the
//     agent's own tree.
//   - taskkill /T /F walks that tree and terminates all of it. /T is the
//     whole point — without it only the named process dies and the
//     grandchildren keep running, holding the stdout and stderr pipes
//     they inherited.
//
// Without this, a timed-out exec returned promptly but the work it
// spawned kept running on the host that was told to stop — the exact
// failure the Unix side fixes by signalling the process group.
//
// WaitDelay still backstops anything that escapes: it caps how long Wait
// will hold on for pipes nobody is going to close.
func withTreeKill(cmd *exec.Cmd) *exec.Cmd {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP

	// A nil Cancel means no context is attached, so there is nothing to
	// cancel and overriding it would be misleading.
	if cmd.Cancel != nil {
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return nil
			}
			kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
			kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
			if err := kill.Run(); err != nil {
				// taskkill missing, blocked, or the tree already gone.
				// Killing the immediate child is strictly better than
				// leaving the whole tree running.
				return cmd.Process.Kill()
			}
			return nil
		}
	}
	cmd.WaitDelay = execWaitDelay

	return cmd
}
