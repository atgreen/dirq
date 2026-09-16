// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

//go:build windows

package agent

import "os/exec"

// withTreeKill bounds how long a cancelled command can hold on.
//
// Windows has no process groups to signal, so a child that outlives its
// parent while holding the inherited pipes is capped by WaitDelay rather
// than killed outright: Wait gives up on the pipes and returns instead of
// blocking indefinitely. Killing the full tree here needs a job object —
// see dirq-2sg.
func withTreeKill(cmd *exec.Cmd) *exec.Cmd {
	cmd.WaitDelay = execWaitDelay
	return cmd
}
