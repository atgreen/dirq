// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// doctor's client.conf check is an allow-list, so a key the CLI starts
// reading without being added there turns every correct config into a
// warning. tls_ca shipped that way. Scan the CLI's own reads of
// clientCfg and require each key to be on the list.
func TestDoctorKnowsEveryClientConfigKey(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	read := regexp.MustCompile(`clientCfg,\s*"([a-z_]+)"`)
	seen := 0
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range read.FindAllStringSubmatch(string(src), -1) {
			seen++
			if !clientConfigKeys[m[1]] {
				t.Errorf("%s reads client.conf key %q, but doctor would flag it as unknown", f, m[1])
			}
		}
	}
	if seen == 0 {
		t.Fatal("found no clientCfg reads; the scan pattern no longer matches the code")
	}
}
