// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package query

import (
	"testing"
)

// The predicates in this file decide which machines a query, exec, or
// deploy is sent to. A wrong answer here does not surface as an error —
// it silently runs a command on the wrong fleet — so the cases below
// pin both the match and the no-match side of every operator.

// parseWhere parses a full query and returns just its WHERE expression.
func parseWhere(t *testing.T, q string) Expr {
	t.Helper()
	parsed, err := Parse(q)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	return parsed.Where
}

// ─────────────────────────────────────────────────────────
// MatchesAgentRecord — hostname conditions
// ─────────────────────────────────────────────────────────

func TestMatchesAgentRecord_Hostname(t *testing.T) {
	const hostname = "web-01.example.com"
	tags := map[string]string{"env": "prod"}

	tests := []struct {
		query string
		want  bool
	}{
		{`SELECT * WHERE hostname = 'web-01.example.com'`, true},
		{`SELECT * WHERE hostname = 'db-01.example.com'`, false},
		{`SELECT * WHERE hostname != 'db-01.example.com'`, true},
		{`SELECT * WHERE hostname != 'web-01.example.com'`, false},

		{`SELECT * WHERE hostname LIKE 'web-%'`, true},
		{`SELECT * WHERE hostname LIKE 'db-%'`, false},
		{`SELECT * WHERE hostname LIKE 'web-0_.example.com'`, true},
		{`SELECT * WHERE hostname NOT LIKE 'db-%'`, true},
		{`SELECT * WHERE hostname NOT LIKE 'web-%'`, false},

		{`SELECT * WHERE hostname IN ('web-01.example.com', 'web-02.example.com')`, true},
		{`SELECT * WHERE hostname IN ('db-01.example.com')`, false},
		{`SELECT * WHERE hostname NOT IN ('db-01.example.com')`, true},
		{`SELECT * WHERE hostname NOT IN ('web-01.example.com')`, false},

		{`SELECT * WHERE hostname IS NULL`, false},
		{`SELECT * WHERE hostname IS NOT NULL`, true},

		// An ordering operator on a hostname has no meaning; it must
		// exclude the agent rather than quietly match everything.
		{`SELECT * WHERE hostname > 'web-00.example.com'`, false},
		{`SELECT * WHERE hostname <= 'web-99.example.com'`, false},

		// Only the bare "hostname" field is resolvable server-side. A
		// module field that merely ends in "hostname" is agent-reported
		// data and must stay conservatively true.
		{`SELECT * WHERE os_info.hostname = 'db-01.example.com'`, true},
	}

	for _, tt := range tests {
		if got := MatchesAgentRecord(parseWhere(t, tt.query), tags, hostname); got != tt.want {
			t.Errorf("MatchesAgentRecord(%q, hostname=%q) = %v, want %v",
				tt.query, hostname, got, tt.want)
		}
	}
}

func TestMatchesAgentRecord_EmptyHostname(t *testing.T) {
	// An agent that registered without a hostname must not be swept up
	// by a hostname filter meant for a named host.
	tests := []struct {
		query string
		want  bool
	}{
		{`SELECT * WHERE hostname IS NULL`, true},
		{`SELECT * WHERE hostname IS NOT NULL`, false},
		{`SELECT * WHERE hostname = 'web-01'`, false},
		{`SELECT * WHERE hostname LIKE 'web-%'`, false},
		{`SELECT * WHERE hostname IN ('web-01')`, false},
	}

	for _, tt := range tests {
		if got := MatchesAgentRecord(parseWhere(t, tt.query), nil, ""); got != tt.want {
			t.Errorf("MatchesAgentRecord(%q, hostname=%q) = %v, want %v", tt.query, "", got, tt.want)
		}
	}
}

// ─────────────────────────────────────────────────────────
// MatchesAgentRecord — tags, combinations, and the conservative default
// ─────────────────────────────────────────────────────────

func TestMatchesAgentRecord_Tags(t *testing.T) {
	tags := map[string]string{"env": "prod", "group": "webservers"}

	tests := []struct {
		query string
		want  bool
	}{
		{`SELECT * WHERE tag.env = 'prod'`, true},
		{`SELECT * WHERE tag.env = 'staging'`, false},
		{`SELECT * WHERE tag.env != 'staging'`, true},
		{`SELECT * WHERE tag.env LIKE 'pro%'`, true},
		{`SELECT * WHERE tag.env LIKE 'stag%'`, false},
		{`SELECT * WHERE tag.env NOT LIKE 'stag%'`, true},
		{`SELECT * WHERE tag.env IN ('prod', 'staging')`, true},
		{`SELECT * WHERE tag.env IN ('dev')`, false},
		{`SELECT * WHERE tag.env NOT IN ('dev')`, true},
		{`SELECT * WHERE tag.env IS NOT NULL`, true},
		{`SELECT * WHERE tag.env IS NULL`, false},

		// Absent tag: "is it not X" and "is it missing" hold; every
		// positive assertion fails.
		{`SELECT * WHERE tag.region = 'us-east'`, false},
		{`SELECT * WHERE tag.region != 'us-east'`, true},
		{`SELECT * WHERE tag.region LIKE 'us-%'`, false},
		{`SELECT * WHERE tag.region NOT LIKE 'us-%'`, true},
		{`SELECT * WHERE tag.region IN ('us-east')`, false},
		{`SELECT * WHERE tag.region NOT IN ('us-east')`, true},
		{`SELECT * WHERE tag.region IS NULL`, true},
		{`SELECT * WHERE tag.region IS NOT NULL`, false},

		// An ordering operator on a tag is meaningless, as with hostname.
		{`SELECT * WHERE tag.env > 'dev'`, false},
	}

	for _, tt := range tests {
		if got := MatchesAgentRecord(parseWhere(t, tt.query), tags, "web-01"); got != tt.want {
			t.Errorf("MatchesAgentRecord(%q) = %v, want %v", tt.query, got, tt.want)
		}
	}
}

func TestMatchesAgentRecord_Combinations(t *testing.T) {
	tags := map[string]string{"env": "prod"}
	const hostname = "web-01"

	tests := []struct {
		query string
		want  bool
	}{
		{`SELECT * WHERE hostname = 'web-01' AND tag.env = 'prod'`, true},
		{`SELECT * WHERE hostname = 'web-01' AND tag.env = 'staging'`, false},
		{`SELECT * WHERE hostname = 'db-01' AND tag.env = 'prod'`, false},
		{`SELECT * WHERE hostname = 'db-01' OR tag.env = 'prod'`, true},
		{`SELECT * WHERE hostname = 'db-01' OR tag.env = 'staging'`, false},
		{`SELECT * WHERE NOT hostname = 'db-01'`, true},
		{`SELECT * WHERE NOT hostname = 'web-01'`, false},
		{`SELECT * WHERE NOT (hostname = 'db-01' OR tag.env = 'staging')`, true},
		{`SELECT * WHERE (hostname = 'web-01' OR hostname = 'web-02') AND tag.env = 'prod'`, true},
	}

	for _, tt := range tests {
		if got := MatchesAgentRecord(parseWhere(t, tt.query), tags, hostname); got != tt.want {
			t.Errorf("MatchesAgentRecord(%q) = %v, want %v", tt.query, got, tt.want)
		}
	}
}

func TestMatchesAgentRecord_FieldConditionsAreConservativelyTrue(t *testing.T) {
	// Agent-reported fields cannot be resolved from the DB record, so
	// the pre-filter must keep the agent and let the field resolution
	// pass narrow it. Dropping the agent here would lose a real target.
	tags := map[string]string{"env": "prod"}

	tests := []struct {
		query string
		want  bool
	}{
		{`SELECT * WHERE disk.pct_used > 80`, true},
		{`SELECT * WHERE os_info.os LIKE 'linux%'`, true},
		{`SELECT * WHERE os_info.os IN ('linux')`, true},
		{`SELECT * WHERE os_info.os IS NULL`, true},
		{`SELECT * WHERE tag.env = 'prod' AND disk.pct_used > 80`, true},
		// The tag half still decides when it can.
		{`SELECT * WHERE tag.env = 'staging' AND disk.pct_used > 80`, false},
		{`SELECT * WHERE hostname = 'db-01' AND disk.pct_used > 80`, false},
	}

	for _, tt := range tests {
		if got := MatchesAgentRecord(parseWhere(t, tt.query), tags, "web-01"); got != tt.want {
			t.Errorf("MatchesAgentRecord(%q) = %v, want %v", tt.query, got, tt.want)
		}
	}
}

func TestMatchesAgentRecord_NilExprMatchesEverything(t *testing.T) {
	// A query with no WHERE targets the whole fleet by design.
	if !MatchesAgentRecord(nil, nil, "") {
		t.Error("MatchesAgentRecord(nil, ...) = false, want true")
	}
}

// ─────────────────────────────────────────────────────────
// Condition classification — which resolution path a query takes
// ─────────────────────────────────────────────────────────

func TestHasHostnameCondition(t *testing.T) {
	tests := []struct {
		query string
		want  bool
	}{
		{`SELECT *`, false},
		{`SELECT * WHERE hostname = 'web-01'`, true},
		{`SELECT * WHERE hostname != 'web-01'`, true},
		{`SELECT * WHERE hostname LIKE 'web-%'`, true},
		{`SELECT * WHERE hostname IN ('web-01')`, true},
		{`SELECT * WHERE hostname IS NULL`, true},
		{`SELECT * WHERE NOT hostname = 'web-01'`, true},
		{`SELECT * WHERE tag.env = 'prod' AND hostname = 'web-01'`, true},
		{`SELECT * WHERE tag.env = 'prod' OR hostname = 'web-01'`, true},
		{`SELECT * WHERE tag.env = 'prod'`, false},
		{`SELECT * WHERE disk.pct_used > 80`, false},
		// Not the bare hostname field.
		{`SELECT * WHERE os_info.hostname = 'web-01'`, false},
	}

	for _, tt := range tests {
		if got := HasHostnameCondition(parseWhere(t, tt.query)); got != tt.want {
			t.Errorf("HasHostnameCondition(%q) = %v, want %v", tt.query, got, tt.want)
		}
	}
}

func TestHasTagConditions(t *testing.T) {
	tests := []struct {
		query string
		want  bool
	}{
		{`SELECT *`, false},
		{`SELECT * WHERE tag.env = 'prod'`, true},
		{`SELECT * WHERE tag.env LIKE 'pro%'`, true},
		{`SELECT * WHERE tag.env IN ('prod')`, true},
		{`SELECT * WHERE tag.env IS NULL`, true},
		{`SELECT * WHERE NOT tag.env = 'prod'`, true},
		{`SELECT * WHERE disk.pct_used > 80 OR tag.env = 'prod'`, true},
		{`SELECT * WHERE hostname = 'web-01'`, false},
		{`SELECT * WHERE disk.pct_used > 80`, false},
	}

	for _, tt := range tests {
		if got := HasTagConditions(parseWhere(t, tt.query)); got != tt.want {
			t.Errorf("HasTagConditions(%q) = %v, want %v", tt.query, got, tt.want)
		}
	}
}

func TestHasFieldConditions(t *testing.T) {
	tests := []struct {
		query string
		want  bool
	}{
		{`SELECT *`, false},
		{`SELECT * WHERE disk.pct_used > 80`, true},
		{`SELECT * WHERE os_info.os LIKE 'linux%'`, true},
		{`SELECT * WHERE os_info.os IN ('linux')`, true},
		{`SELECT * WHERE os_info.os IS NULL`, true},
		{`SELECT * WHERE NOT disk.pct_used > 80`, true},
		{`SELECT * WHERE tag.env = 'prod' AND disk.pct_used > 80`, true},
		// A tag reference is resolved from the DB, never from the agent.
		{`SELECT * WHERE tag.env = 'prod'`, false},
		// A bare field has no module prefix, so there is nothing to ask
		// an agent for.
		{`SELECT * WHERE hostname = 'web-01'`, false},
	}

	for _, tt := range tests {
		if got := HasFieldConditions(parseWhere(t, tt.query)); got != tt.want {
			t.Errorf("HasFieldConditions(%q) = %v, want %v", tt.query, got, tt.want)
		}
	}
}

// TestConditionClassesAreIndependent pins the invariant that a caller
// which gates target filtering on HasTagConditions alone will skip the
// filter entirely for hostname-only and field-only queries — and so
// must check all three. This is the shape of dirq-8cp, where
// handleBroadcastDeploy tested only for tag conditions and silently
// deployed to the whole fleet.
func TestConditionClassesAreIndependent(t *testing.T) {
	tests := []struct {
		query                     string
		tagCond, hostCond, fields bool
	}{
		{`SELECT * WHERE tag.env = 'prod'`, true, false, false},
		{`SELECT * WHERE hostname = 'web-01'`, false, true, false},
		{`SELECT * WHERE os_info.os = 'linux'`, false, false, true},
		{`SELECT * WHERE hostname = 'web-01' AND os_info.os = 'linux'`, false, true, true},
		{`SELECT * WHERE tag.env = 'prod' AND os_info.os = 'linux'`, true, false, true},
	}

	for _, tt := range tests {
		where := parseWhere(t, tt.query)
		if got := HasTagConditions(where); got != tt.tagCond {
			t.Errorf("HasTagConditions(%q) = %v, want %v", tt.query, got, tt.tagCond)
		}
		if got := HasHostnameCondition(where); got != tt.hostCond {
			t.Errorf("HasHostnameCondition(%q) = %v, want %v", tt.query, got, tt.hostCond)
		}
		if got := HasFieldConditions(where); got != tt.fields {
			t.Errorf("HasFieldConditions(%q) = %v, want %v", tt.query, got, tt.fields)
		}
	}
}

// ─────────────────────────────────────────────────────────
// StripTagFields — what survives the trip to the agent
// ─────────────────────────────────────────────────────────

func TestStripTagFields_AllTagNodeKinds(t *testing.T) {
	// Every tag node kind strips to nil on its own; the agent was
	// already pre-filtered on tags and cannot evaluate them.
	for _, q := range []string{
		`SELECT * WHERE tag.env = 'prod'`,
		`SELECT * WHERE tag.env LIKE 'pro%'`,
		`SELECT * WHERE tag.env IN ('prod')`,
		`SELECT * WHERE tag.env IS NULL`,
	} {
		if got := StripTagFields(parseWhere(t, q)); got != nil {
			t.Errorf("StripTagFields(%q) = %T, want nil", q, got)
		}
	}
}

func TestStripTagFields_NilAndNot(t *testing.T) {
	if got := StripTagFields(nil); got != nil {
		t.Errorf("StripTagFields(nil) = %v, want nil", got)
	}

	// NOT over a tag-only expression has nothing left to evaluate.
	if got := StripTagFields(parseWhere(t, `SELECT * WHERE NOT tag.env = 'prod'`)); got != nil {
		t.Errorf("StripTagFields(NOT tag-only) = %T, want nil", got)
	}

	// NOT over a data expression is preserved, negation intact.
	got := StripTagFields(parseWhere(t, `SELECT * WHERE NOT disk.pct_used > 80`))
	not, ok := got.(*NotExpr)
	if !ok {
		t.Fatalf("StripTagFields(NOT data) = %T, want *NotExpr", got)
	}
	cmp, ok := not.Expr.(*CompareExpr)
	if !ok || cmp.Field != "disk.pct_used" {
		t.Errorf("inner expr = %T %+v, want disk.pct_used compare", not.Expr, not.Expr)
	}
}

func TestStripTagFields_KeepsDataSideOfAnd(t *testing.T) {
	// Both orderings, so a one-sided bug cannot hide.
	for _, q := range []string{
		`SELECT * WHERE tag.env = 'prod' AND disk.pct_used > 80`,
		`SELECT * WHERE disk.pct_used > 80 AND tag.env = 'prod'`,
	} {
		got := StripTagFields(parseWhere(t, q))
		cmp, ok := got.(*CompareExpr)
		if !ok || cmp.Field != "disk.pct_used" {
			t.Errorf("StripTagFields(%q) = %T %+v, want disk.pct_used compare", q, got, got)
		}
	}
}
