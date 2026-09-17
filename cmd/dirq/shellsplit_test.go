// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"fmt"
	"strings"
)

// shellSplit is a minimal POSIX-ish word splitter, used only by tests to
// check that what joinRemoteCommand produces parses back into the vector
// it was given. It handles the two quoting forms that helper emits and
// nothing more.
func shellSplit(s string) ([]string, error) {
	var (
		args []string
		cur  strings.Builder
		open bool
		inSQ bool
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inSQ:
			if c == '\'' {
				inSQ = false
			} else {
				cur.WriteByte(c)
			}
		case c == '\'':
			inSQ = true
			open = true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			open = true
		case c == ' ' || c == '\t':
			if open || cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
				open = false
			}
		default:
			cur.WriteByte(c)
			open = true
		}
	}
	if inSQ {
		return nil, fmt.Errorf("unterminated single quote in %q", s)
	}
	if open || cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args, nil
}
