// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import "strings"

// joinRemoteCommand turns the arguments after -- into the command string
// sent to the agent, which runs it through a shell.
//
// The arguments used to be joined with plain spaces. That silently
// destroys argument boundaries: `-- sh -c 'printf "a b" > /f'` arrives as
// three arguments, and joining them yields `sh -c printf "a b" > /f`, so
// the inner `sh -c printf` runs printf with no format string at all. The
// agent answered with a usage message and the file was never written
// (dirq-2uf).
//
// The two shapes people write need different handling, and how many
// arguments there are tells them apart:
//
//   - One argument is a command line the caller has already quoted
//     themselves, so it passes through untouched and shell syntax keeps
//     working: `-- "ls /tmp | wc -l"` still pipes.
//   - Several arguments are a command vector, and the boundaries are the
//     information that must survive. Each is quoted so a value containing
//     spaces stays one argument.
//
// The cost of the rule is spreading shell syntax across several arguments
// to get it interpreted — `-- ls /tmp \| wc -l` now passes a literal pipe
// to ls instead of piping. That depended on the boundary-losing join,
// which is the bug; write it as a single quoted argument instead.
func joinRemoteCommand(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = shellQuoteArg(p)
	}
	return strings.Join(quoted, " ")
}

// shellQuoteArg wraps a string in single quotes for safe POSIX shell use,
// the same way the agent does when it builds a become wrapper. Left
// unquoted when the value is a plain word, so an ordinary command still
// reads as itself in logs and in the exec_log.
func shellQuoteArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`|&;<>()*?[]{}!#~=") {
		return s
	}
	var b strings.Builder
	b.WriteByte('\'')
	for _, c := range s {
		if c == '\'' {
			b.WriteString(`'\''`)
		} else {
			b.WriteRune(c)
		}
	}
	b.WriteByte('\'')
	return b.String()
}
