// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package policy

import (
	"context"
	"path/filepath"
	"testing"
)

// TestExamplePoliciesCompile loads every shipped example policy through the
// real engine constructor so a broken example fails CI — without requiring the
// external `opa` binary to be installed.
func TestExamplePoliciesCompile(t *testing.T) {
	examples, err := filepath.Glob("../../../examples/policy/*.rego")
	if err != nil {
		t.Fatalf("glob examples: %v", err)
	}
	if len(examples) == 0 {
		t.Fatal("no example policies found")
	}
	for _, path := range examples {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			e, err := New(context.Background(), Config{File: path})
			if err != nil {
				t.Fatalf("example policy failed to compile: %v", err)
			}
			// Smoke-test evaluation with a representative input so a policy that
			// compiles but errors at eval time is also caught.
			if _, err := e.Eval(context.Background(), Input{
				Operation:   "exec",
				Command:     "id",
				Tags:        map[string]string{"env": "lab"},
				DestPath:    "/etc/myapp/app.conf",
				SrcPath:     "/var/log/app.log",
				ContentSize: 10,
			}); err != nil {
				t.Fatalf("example policy eval errored: %v", err)
			}
		})
	}
}
