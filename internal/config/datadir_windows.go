// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

//go:build windows

package config

import "os"

// dirIsPrivate reports whether path is a directory this process can trust to
// hold secrets. Windows expresses ownership through security descriptors
// rather than a uid and a mode, which this does not inspect: the check here is
// only that the path is a real directory. The finding this guards against
// (dirq-632.5) is a shared-/tmp problem on Unix; the Windows fallback sits
// under a per-user temp directory.
func dirIsPrivate(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.IsDir() && fi.Mode()&os.ModeSymlink == 0
}
