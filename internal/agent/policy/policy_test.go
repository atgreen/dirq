// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package policy

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writePolicy writes src to a temp .rego file and returns its path.
func writePolicy(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.rego")
	if err := os.WriteFile(path, []byte(src), 0600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return path
}

const allowExec = `package dirq.agent

default allow := false
default reason := "denied by default"

allow if {
	input.operation == "exec"
	input.command == "ok"
}

reason := "exec command not allowed" if {
	input.operation == "exec"
	not allow
}
`

func TestNopWhenNoFile(t *testing.T) {
	e, err := New(context.Background(), Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.Enabled() {
		t.Fatal("expected no-op engine to report Enabled() == false")
	}
	dec, err := e.Eval(context.Background(), Input{Operation: "exec", Command: "anything"})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if !dec.Allow {
		t.Fatal("no-op engine must allow everything")
	}
}

func TestMissingFileReturnsError(t *testing.T) {
	_, err := New(context.Background(), Config{File: "/nonexistent/policy.rego"})
	if err == nil {
		t.Fatal("expected error for missing policy file")
	}
}

func TestSyntaxErrorReturnsError(t *testing.T) {
	path := writePolicy(t, "package dirq.agent\nallow if { this is not valid rego ")
	_, err := New(context.Background(), Config{File: path})
	if err == nil {
		t.Fatal("expected compile error for malformed policy")
	}
}

func TestAllowAndDeny(t *testing.T) {
	path := writePolicy(t, allowExec)
	e, err := New(context.Background(), Config{File: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !e.Enabled() {
		t.Fatal("expected Enabled() == true")
	}

	dec, err := e.Eval(context.Background(), Input{Operation: "exec", Command: "ok"})
	if err != nil {
		t.Fatalf("Eval allow: %v", err)
	}
	if !dec.Allow {
		t.Fatal("expected allow for command 'ok'")
	}

	dec, err = e.Eval(context.Background(), Input{Operation: "exec", Command: "rm -rf /"})
	if err != nil {
		t.Fatalf("Eval deny: %v", err)
	}
	if dec.Allow {
		t.Fatal("expected deny for command 'rm -rf /'")
	}
	if dec.Reason != "exec command not allowed" {
		t.Fatalf("unexpected reason: %q", dec.Reason)
	}
}

func TestReasonFallbackWhenAbsent(t *testing.T) {
	// Policy with no reason rule at all: denials carry an empty reason.
	path := writePolicy(t, "package dirq.agent\ndefault allow := false\n")
	e, err := New(context.Background(), Config{File: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dec, err := e.Eval(context.Background(), Input{Operation: "exec"})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if dec.Allow {
		t.Fatal("expected deny")
	}
	if dec.Reason != "" {
		t.Fatalf("expected empty reason, got %q", dec.Reason)
	}
}

func TestFailClosedVsOpenOnError(t *testing.T) {
	closed := &regoEngine{failClosed: true}
	dec, err := closed.onError(context.DeadlineExceeded)
	if err == nil {
		t.Fatal("onError should return the underlying error for logging")
	}
	if dec.Allow {
		t.Fatal("fail-closed must deny on error")
	}
	if dec.Reason != failClosedReason {
		t.Fatalf("unexpected reason: %q", dec.Reason)
	}

	open := &regoEngine{failClosed: false}
	dec, _ = open.onError(context.DeadlineExceeded)
	if !dec.Allow {
		t.Fatal("fail-open must allow on error")
	}
}

func TestReasonQuery(t *testing.T) {
	got, ok := reasonQuery("data.dirq.agent.allow")
	if !ok || got != "data.dirq.agent.reason" {
		t.Fatalf("reasonQuery = %q, %v", got, ok)
	}
	if _, ok := reasonQuery("data.custom.decision"); ok {
		t.Fatal("expected no reason query for non-.allow suffix")
	}
}

func TestCustomQuery(t *testing.T) {
	src := `package custom

default ok := false
ok if { input.operation == "exec" }
`
	path := writePolicy(t, src)
	e, err := New(context.Background(), Config{File: path, Query: "data.custom.ok"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dec, err := e.Eval(context.Background(), Input{Operation: "exec"})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if !dec.Allow {
		t.Fatal("expected allow via custom query")
	}
}

func TestInputRedaction(t *testing.T) {
	in := Input{
		Operation:       "exec",
		Command:         "systemctl restart nginx",
		Script:          true,
		ScriptName:      "deploy.sh",
		ScriptSize:      4812,
		ScriptSHA256:    SHA256Hex([]byte("secret script body")),
		StdinSize:       128,
		EnvironmentKeys: SortedKeys(map[string]string{"FOO": "secret", "BAR": "secret"}),
	}
	m, err := in.toRego()
	if err != nil {
		t.Fatalf("toRego: %v", err)
	}

	// Sensitive material must never appear as a value in the policy input.
	for k, v := range m {
		if s, ok := v.(string); ok {
			if s == "secret script body" || s == "secret" {
				t.Fatalf("sensitive value leaked under key %q", k)
			}
		}
	}

	// Hashes, sizes, and sorted key names must be present.
	if m["script_sha256"] == "" {
		t.Fatal("script_sha256 missing")
	}
	if got := m["environment_keys"]; !reflect.DeepEqual(got, []any{"BAR", "FOO"}) {
		t.Fatalf("environment_keys not sorted: %v", got)
	}
}

func TestSHA256Hex(t *testing.T) {
	// Known vector: sha256("") = e3b0c442...
	if got := SHA256Hex(nil); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("SHA256Hex(nil) = %q", got)
	}
}

func TestSortedKeys(t *testing.T) {
	if got := SortedKeys(nil); got != nil {
		t.Fatalf("SortedKeys(nil) = %v, want nil", got)
	}
	got := SortedKeys(map[string]string{"c": "", "a": "", "b": ""})
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("SortedKeys = %v", got)
	}
}
