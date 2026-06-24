// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package db

import (
	"reflect"
	"testing"
)

func TestAAPUsersRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		wantCol string
		wantOut []string
	}{
		{name: "nil", in: nil, wantCol: "", wantOut: nil},
		{name: "empty slice", in: []string{}, wantCol: "", wantOut: nil},
		{name: "single", in: []string{"svc-prod"}, wantCol: "svc-prod", wantOut: []string{"svc-prod"}},
		{name: "multiple", in: []string{"a", "b"}, wantCol: "a,b", wantOut: []string{"a", "b"}},
		{name: "trims and drops empties", in: []string{" a ", "", "  ", "b"}, wantCol: "a,b", wantOut: []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			col := EncodeAAPUsers(tc.in)
			if col != tc.wantCol {
				t.Fatalf("EncodeAAPUsers = %q, want %q", col, tc.wantCol)
			}
			out := ParseAAPUsers(col)
			if !reflect.DeepEqual(out, tc.wantOut) {
				t.Fatalf("ParseAAPUsers(%q) = %v, want %v", col, out, tc.wantOut)
			}
		})
	}
}
