// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// withTreeKill makes context cancellation terminate everything the
// command started, not just the command itself.
//
// os/exec's default cancellation signals the immediate child only. A
// shell that forks — "sh -c 'sleep 30'" under dash, anything that
// backgrounds work under any shell — leaves the grandchild alive,
// holding the stdout and stderr pipes it inherited. cmd.Run then blocks
// in Wait until that grandchild exits on its own, so the timeout bounds
// nothing and the work keeps running on a fleet host that was told to
// stop.
//
// Putting the child in its own process group lets cancellation signal
// the group. WaitDelay backstops anything that escapes it (a deliberate
// setsid, say) by capping how long Wait will hold on for pipes that
// nobody is going to close.
func withTreeKill(cmd *exec.Cmd) *exec.Cmd {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true

	// A nil Cancel means no context is attached, so there is nothing to
	// cancel and overriding it would be misleading.
	if cmd.Cancel != nil {
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return nil
			}
			// Negative PID targets the group; Setpgid above made the
			// child its leader, so its pgid is its pid.
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	cmd.WaitDelay = execWaitDelay

	return cmd
}
