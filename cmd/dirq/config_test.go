// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The CLI used to always read ~/.config/dirq/client.conf with no way to
// point it elsewhere, so anything invoking it inherited whatever the
// running user had configured — their server URL, their token, their
// tls_insecure. These pin the escape hatch.

func withArgs(t *testing.T, args ...string) {
	t.Helper()
	saved := os.Args
	t.Cleanup(func() { os.Args = saved })
	os.Args = append([]string{"dirq"}, args...)
}

func TestClientConfigPath_DefaultsToTheUserConfig(t *testing.T) {
	withArgs(t)
	t.Setenv("DIRQ_CONFIG_FILE", "")

	// Not asserting the exact path — it is platform- and HOME-dependent —
	// only that something is chosen rather than nothing.
	if got := clientConfigPath(); got == "" {
		t.Error("clientConfigPath() = \"\", want a default path")
	}
}

func TestClientConfigPath_EnvWins(t *testing.T) {
	withArgs(t)
	want := filepath.Join(t.TempDir(), "client.conf")
	t.Setenv("DIRQ_CONFIG_FILE", want)

	if got := clientConfigPath(); got != want {
		t.Errorf("clientConfigPath() = %q, want %q", got, want)
	}
}

func TestClientConfigPath_FlagForms(t *testing.T) {
	want := filepath.Join(t.TempDir(), "client.conf")

	t.Run("separate argument", func(t *testing.T) {
		t.Setenv("DIRQ_CONFIG_FILE", "")
		withArgs(t, "hosts", "list", "--config", want)
		if got := clientConfigPath(); got != want {
			t.Errorf("clientConfigPath() = %q, want %q", got, want)
		}
	})

	t.Run("equals form", func(t *testing.T) {
		t.Setenv("DIRQ_CONFIG_FILE", "")
		withArgs(t, "hosts", "list", "--config="+want)
		if got := clientConfigPath(); got != want {
			t.Errorf("clientConfigPath() = %q, want %q", got, want)
		}
	})

	t.Run("trailing --config with no value is ignored", func(t *testing.T) {
		t.Setenv("DIRQ_CONFIG_FILE", "")
		withArgs(t, "hosts", "list", "--config")
		if got := clientConfigPath(); got == "" {
			t.Error("a valueless --config produced an empty path instead of the default")
		}
	})
}

// The environment variable is the one a test harness or a script sets, so
// it has to beat a flag left over in argv.
func TestClientConfigPath_EnvBeatsFlag(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), "from-env.conf")
	t.Setenv("DIRQ_CONFIG_FILE", envPath)
	withArgs(t, "--config", filepath.Join(t.TempDir(), "from-flag.conf"))

	if got := clientConfigPath(); got != envPath {
		t.Errorf("clientConfigPath() = %q, want the environment value %q", got, envPath)
	}
}
