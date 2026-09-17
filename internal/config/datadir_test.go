// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// dirIsPrivate is what stands between the server and writing its signing key
// into a directory somebody else created first (dirq-632.5).
func TestDirIsPrivate(t *testing.T) {
	base := t.TempDir()

	mkdir := func(name string, mode os.FileMode) string {
		t.Helper()
		p := filepath.Join(base, name)
		if err := os.Mkdir(p, mode); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		// Mkdir applies the umask, so set the mode explicitly.
		if err := os.Chmod(p, mode); err != nil {
			t.Fatalf("chmod %s: %v", name, err)
		}
		return p
	}

	t.Run("a private directory we own", func(t *testing.T) {
		if !dirIsPrivate(mkdir("ours", 0o700)) {
			t.Error("rejected a 0700 directory owned by this user")
		}
	})

	t.Run("world-writable is not private", func(t *testing.T) {
		if dirIsPrivate(mkdir("wide-open", 0o777)) {
			t.Error("accepted a world-writable directory for key material")
		}
	})

	t.Run("group-writable is not private", func(t *testing.T) {
		if dirIsPrivate(mkdir("group", 0o770)) {
			t.Error("accepted a group-writable directory for key material")
		}
	})

	t.Run("a symlink is not a directory we trust", func(t *testing.T) {
		target := mkdir("symlink-target", 0o700)
		link := filepath.Join(base, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if dirIsPrivate(link) {
			t.Error("followed a symlink to decide a directory was private")
		}
	})

	t.Run("a file is not a directory", func(t *testing.T) {
		f := filepath.Join(base, "regular-file")
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if dirIsPrivate(f) {
			t.Error("accepted a regular file")
		}
	})

	t.Run("a path that does not exist", func(t *testing.T) {
		if dirIsPrivate(filepath.Join(base, "nope")) {
			t.Error("accepted a nonexistent path")
		}
	})
}

// The whole point: an attacker-owned fallback must not be selected, even
// though MkdirAll on it succeeds.
func TestDataDirRefusesAHostileFallback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	hostile := filepath.Join(tmp, "dirq-data")
	if err := os.Mkdir(hostile, 0o777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(hostile, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	got := DataDir()
	if got == hostile {
		t.Fatal("DataDir chose a world-writable directory to hold key material")
	}
}
