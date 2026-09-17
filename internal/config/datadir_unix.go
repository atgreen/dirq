// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

//go:build !windows

package config

import (
	"os"
	"syscall"
)

// dirIsPrivate reports whether path is a directory this process can trust to
// hold secrets: a real directory rather than a symlink, owned by this user,
// and not writable by group or other.
//
// It exists because MkdirAll succeeds on a directory that already exists and
// leaves its owner and mode alone. A local user who creates the fallback
// directory first therefore owns the subtree the server then writes its
// signing key, TLS material and bootstrap token into — and owning the parent
// is enough to swap any of them, whatever mode the files themselves carry
// (dirq-632.5).
func dirIsPrivate(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false // unknown platform detail: do not assume it is ours
	}
	return int(st.Uid) == os.Geteuid()
}
