// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"strings"
	"testing"
)

// The startup line that announces the database backend used to print the DSN
// verbatim, password and all, on every boot — to a log read by people and
// systems that are not DirQ admins (dirq-632.4).
func TestRedactDSN(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{
			"postgres with password",
			"postgres://dirq:hunter2@db.internal:5432/dirq?sslmode=require", // notsecret
			"postgres://dirq:xxxxx@db.internal:5432/dirq?sslmode=require",   // notsecret
		},
		{
			"postgresql scheme",
			"postgresql://svc:s3cr3t@10.0.0.5/dirq",
			"postgresql://svc:xxxxx@10.0.0.5/dirq",
		},
		{
			"user but no password is not a secret",
			"postgres://dirq@db.internal:5432/dirq",
			"postgres://dirq@db.internal:5432/dirq",
		},
		{
			"sqlite path passes through",
			"sqlite:///var/lib/dirq/dirq.db",
			"sqlite:///var/lib/dirq/dirq.db",
		},
		{
			"bare path passes through",
			"/var/lib/dirq/dirq.db",
			"/var/lib/dirq/dirq.db",
		},
		{
			"empty",
			"",
			"",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactDSN(c.dsn)
			if got != c.want {
				t.Errorf("redactDSN(%q) = %q, want %q", c.dsn, got, c.want)
			}
		})
	}
}

// Whatever else redaction does, the password must not survive it. Checked
// separately from the exact-output table so a future change to the
// placeholder or to query-parameter ordering cannot quietly reintroduce the
// leak while the table is updated to match.
func TestRedactDSNNeverEmitsThePassword(t *testing.T) {
	const password = "correct-horse-battery-staple"
	dsn := "postgres://dirq:" + password + "@db.internal:5432/dirq"

	if got := redactDSN(dsn); strings.Contains(got, password) {
		t.Errorf("redactDSN leaked the password: %q", got)
	}
}
