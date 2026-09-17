// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import "testing"

// The arguments after -- become a command string the agent runs through a
// shell. Joining them with plain spaces destroyed argument boundaries, so
// any value containing a space was silently re-split (dirq-2uf).

func TestJoinRemoteCommand_DocumentedExamplesAreUnchanged(t *testing.T) {
	// Every example in `dirq exec --help`. These are what people copy, so
	// the fix must not alter any of them.
	tests := []struct {
		parts []string
		want  string
	}{
		{[]string{"uptime"}, "uptime"},
		{[]string{"du", "-h"}, "du -h"},
		{[]string{"where", "myprogram.exe"}, "where myprogram.exe"},
		{[]string{"systemctl", "restart", "nginx"}, "systemctl restart nginx"},
	}
	for _, tt := range tests {
		if got := joinRemoteCommand(tt.parts); got != tt.want {
			t.Errorf("joinRemoteCommand(%q) = %q, want %q", tt.parts, got, tt.want)
		}
	}
}

// TestJoinRemoteCommand_KeepsArgumentsWithSpacesWhole is the bug: the
// third argument here is a whole shell program and must arrive as one
// argument, not be re-split into words.
func TestJoinRemoteCommand_KeepsArgumentsWithSpacesWhole(t *testing.T) {
	got := joinRemoteCommand([]string{"sh", "-c", `printf 'a b\n' > /tmp/f`})

	want := `sh -c 'printf '\''a b\n'\'' > /tmp/f'`
	if got != want {
		t.Errorf("joinRemoteCommand = %q,\n                 want %q", got, want)
	}
}

// A single argument is a command line the caller quoted themselves, so
// shell syntax in it has to keep working.
func TestJoinRemoteCommand_SingleArgumentPassesThrough(t *testing.T) {
	tests := []string{
		"ls /tmp | wc -l",
		"cat /etc/os-release && echo done",
		`printf 'x y\n' > /tmp/f`,
		"echo $HOME",
	}
	for _, in := range tests {
		if got := joinRemoteCommand([]string{in}); got != in {
			t.Errorf("joinRemoteCommand([%q]) = %q, want it unchanged", in, got)
		}
	}
}

func TestJoinRemoteCommand_Empty(t *testing.T) {
	if got := joinRemoteCommand(nil); got != "" {
		t.Errorf("joinRemoteCommand(nil) = %q, want empty", got)
	}
}

// TestJoinRemoteCommand_QuotesSurviveARoundTrip checks the property that
// matters rather than the exact spelling: feeding the result to a shell
// must reproduce the original argument vector.
func TestJoinRemoteCommand_QuotesSurviveARoundTrip(t *testing.T) {
	vectors := [][]string{
		{"echo", "hello world"},
		{"sh", "-c", "echo 'nested quotes'"},
		{"grep", "-e", "a|b", "/tmp/f"},
		{"printf", `%s\n`, "with $dollar and `backtick`"},
		{"touch", "/tmp/file with spaces"},
		{"echo", `it's quoted`},
	}
	for _, v := range vectors {
		joined := joinRemoteCommand(v)
		got, err := shellSplit(joined)
		if err != nil {
			t.Errorf("splitting %q: %v", joined, err)
			continue
		}
		if len(got) != len(v) {
			t.Errorf("%q -> %q: got %d args, want %d", v, joined, len(got), len(v))
			continue
		}
		for i := range v {
			if got[i] != v[i] {
				t.Errorf("%q -> %q: arg %d = %q, want %q", v, joined, i, got[i], v[i])
			}
		}
	}
}

func TestShellQuoteArg(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", "plain"},
		{"", "''"},
		{"two words", "'two words'"},
		{"it's", `'it'\''s'`},
		{"a|b", "'a|b'"},
		{"$HOME", "'$HOME'"},
	}
	for _, tt := range tests {
		if got := shellQuoteArg(tt.in); got != tt.want {
			t.Errorf("shellQuoteArg(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
