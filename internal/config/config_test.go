// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package config

import (
	"os"
	"testing"
)

func TestLoadConfigFile(t *testing.T) {
	content := `# DirQ agent config
server: grpc.example.com:50051
listen: 0.0.0.0:50052
exec_enabled: true

tags:
  env: prod
  group: webservers
  dc: us-east-1
`
	tmp, err := os.CreateTemp("", "dirq-config-*.conf")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	tmp.WriteString(content)
	tmp.Close()

	f, err := Load(tmp.Name())
	if err != nil {
		t.Fatal(err)
	}

	if f.Get("server") != "grpc.example.com:50051" {
		t.Errorf("server: got %q", f.Get("server"))
	}
	if f.Get("listen") != "0.0.0.0:50052" {
		t.Errorf("listen: got %q", f.Get("listen"))
	}
	if f.Get("exec_enabled") != "true" {
		t.Errorf("exec_enabled: got %q", f.Get("exec_enabled"))
	}

	tags := f.GetTags()
	if tags["env"] != "prod" {
		t.Errorf("tag env: got %q", tags["env"])
	}
	if tags["group"] != "webservers" {
		t.Errorf("tag group: got %q", tags["group"])
	}
	if tags["dc"] != "us-east-1" {
		t.Errorf("tag dc: got %q", tags["dc"])
	}
}

func TestLoadMissingFile(t *testing.T) {
	f, err := Load("/nonexistent/dirq.conf")
	if err != nil {
		t.Fatal("missing file should not error")
	}
	if len(f.Values) != 0 {
		t.Error("expected empty values")
	}
}

func TestEnvOverridesFile(t *testing.T) {
	content := "server: file-server:50051\n"
	tmp, _ := os.CreateTemp("", "dirq-config-*.conf")
	defer os.Remove(tmp.Name())
	tmp.WriteString(content)
	tmp.Close()

	f, _ := Load(tmp.Name())

	// No env var set — should use file value.
	result := EnvOr("DIRQ_TEST_NOTSET_12345", f, "server", "fallback")
	if result != "file-server:50051" {
		t.Errorf("expected file value, got %q", result)
	}

	// Set env var — should override.
	os.Setenv("DIRQ_TEST_OVERRIDE_12345", "env-server:50051")
	defer os.Unsetenv("DIRQ_TEST_OVERRIDE_12345")
	result = EnvOr("DIRQ_TEST_OVERRIDE_12345", f, "server", "fallback")
	if result != "env-server:50051" {
		t.Errorf("expected env value, got %q", result)
	}

	// Neither set — should use fallback.
	result = EnvOr("DIRQ_TEST_NOTSET_12345", f, "missing_key", "fallback")
	if result != "fallback" {
		t.Errorf("expected fallback, got %q", result)
	}
}

func TestComments(t *testing.T) {
	content := "# comment\nserver: example.com:50051 # inline comment\n"
	tmp, _ := os.CreateTemp("", "dirq-config-*.conf")
	defer os.Remove(tmp.Name())
	tmp.WriteString(content)
	tmp.Close()

	f, _ := Load(tmp.Name())
	if f.Get("server") != "example.com:50051" {
		t.Errorf("got %q, want example.com:50051", f.Get("server"))
	}
}
