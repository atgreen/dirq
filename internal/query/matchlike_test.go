// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package query

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The LIKE matcher runs on every agent, against every element of every array
// module a query touches — once per installed package, per service, per
// interface. The pattern comes from a readonly API token. A matcher whose cost
// is exponential in the wildcard count therefore turns one small query into
// fleet-wide CPU exhaustion, with no rate limit or body cap standing in the
// way (dirq-632.13).

func TestMatchLikeSemantics(t *testing.T) {
	cases := []struct {
		name    string
		s       string
		pattern string
		want    bool
	}{
		{"exact", "kernel", "kernel", true},
		{"exact mismatch", "kernel", "kernal", false},
		{"prefix", "kernel-core", "kernel%", true},
		{"suffix", "kernel-core", "%core", true},
		{"contains", "kernel-core", "%nel-co%", true},
		{"contains mismatch", "kernel-core", "%zzz%", false},
		{"underscore is exactly one char", "web-01", "web-0_", true},
		{"underscore does not match empty", "web-0", "web-0_", false},
		{"underscore does not match two", "web-012", "web-0_", false},
		{"trailing wildcard matches empty", "kernel", "kernel%", true},
		{"consecutive wildcards", "kernel-core", "kernel%%%core", true},
		{"leading and trailing", "kernel", "%kernel%", true},
		{"wildcard only", "anything at all", "%", true},
		{"wildcard only, empty subject", "", "%", true},
		{"empty pattern, empty subject", "", "", true},
		{"empty pattern, non-empty subject", "x", "", false},
		{"case insensitive subject", "KERNEL", "kernel", true},
		{"case insensitive pattern", "kernel", "KERNEL%", true},
		{"interleaved wildcards", "abcdef", "a%c%e%", true},
		{"interleaved wildcards mismatch", "abcdef", "a%c%z%", false},
		{"pattern longer than subject", "ab", "abc%", false},
		{"wildcard spans the whole subject", "openssl-3.2.2-1.el9", "openssl-%-%.el9", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matchLike(c.s, c.pattern); got != c.want {
				t.Errorf("matchLike(%q, %q) = %v, want %v", c.s, c.pattern, got, c.want)
			}
		})
	}
}

// TestMatchLikeDoesNotBacktrackCatastrophically is the regression test for the
// finding. The old matcher recursed over every suffix position per wildcard:
// 8 wildcards took half a second, 12 took a hundred, and each further wildcard
// multiplied that by about four. A linear matcher answers all of these in
// microseconds, so the budget below is enormously generous and still fails the
// old implementation on the very first case.
func TestMatchLikeDoesNotBacktrackCatastrophically(t *testing.T) {
	// A package name shaped like a real one, with a long run of a repeated
	// character for the wildcards to backtrack over.
	subject := strings.Repeat("a", 40) + "-1.0-1.el9"

	for _, wildcards := range []int{8, 12, 16, 24, 40} {
		// The trailing Q never matches, which is what forces the old matcher
		// to explore every placement of every wildcard before giving up.
		pattern := strings.Repeat("%a", wildcards) + "Q"

		done := make(chan bool, 1)
		go func() {
			done <- matchLike(subject, pattern)
		}()

		select {
		case got := <-done:
			if got {
				t.Fatalf("%d wildcards: pattern %q unexpectedly matched", wildcards, pattern)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%d wildcards: matchLike did not finish within 2s — the matcher is backtracking", wildcards)
		}
	}
}

// A linear matcher is still O(len(subject) x len(pattern)) in the worst case,
// and the query body cap allows a pattern far longer than any real one. The
// parser caps it so a single query cannot buy that much work per value.
func TestParseRejectsAnOversizedLikePattern(t *testing.T) {
	huge := strings.Repeat("%a", (maxLikePatternBytes/2)+1)

	if _, err := Parse("SELECT hostname WHERE packages.name LIKE '" + huge + "'"); err == nil {
		t.Fatal("parser accepted a LIKE pattern past the cap")
	}

	ok := strings.Repeat("%a", 16)
	if _, err := Parse("SELECT hostname WHERE packages.name LIKE '" + ok + "'"); err != nil {
		t.Fatalf("parser rejected a realistic LIKE pattern: %v", err)
	}
}

func BenchmarkMatchLikeManyWildcards(b *testing.B) {
	subject := strings.Repeat("a", 40) + "-1.0-1.el9"
	pattern := strings.Repeat("%a", 24) + "Q"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		matchLike(subject, pattern)
	}
}

// Agent-side filtering runs on every managed host, once per element of every
// array module a query touches. It used to take no context, so when the
// server's query timeout expired the agents kept filtering for a query nobody
// was waiting for (dirq-632.16).
func TestFilterCollectedDataHonoursCancellation(t *testing.T) {
	packages := make([]any, 5000)
	for i := range packages {
		packages[i] = map[string]any{"name": "pkg", "version": "1.0"}
	}
	data := map[string]any{
		"packages": map[string]any{"packages": packages},
	}
	conds := []*Condition{{Field: "packages.name", Operator: "=", Value: &Value{String: strPtr("pkg")}}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := FilterCollectedData(ctx, conds, data); err == nil {
		t.Fatal("filtering ran to completion for a cancelled query")
	}

	// And an uncancelled query still filters.
	got, err := FilterCollectedData(context.Background(), conds, data)
	if err != nil {
		t.Fatalf("filtering a live query failed: %v", err)
	}
	if got == nil {
		t.Fatal("filtering a live query returned nothing")
	}
}

func strPtr(s string) *string { return &s }
