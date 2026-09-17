// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package modules

import (
	"testing"
)

func TestRegistry(t *testing.T) {
	reg := Registry()
	expected := []string{"disk", "cpu", "memory", "os_info"}
	for _, name := range expected {
		if _, ok := reg[name]; !ok {
			t.Errorf("Registry missing module %q", name)
		}
	}
}

func TestDiskModule(t *testing.T) {
	mod := &DiskModule{}
	if mod.Name() != "disk" {
		t.Fatalf("expected name 'disk', got %q", mod.Name())
	}
	data, err := mod.Collect()
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if _, ok := data["partitions"]; !ok {
		t.Error("missing key 'partitions'")
	}
}

func TestCPUModule(t *testing.T) {
	mod := &CPUModule{}
	if mod.Name() != "cpu" {
		t.Fatalf("expected name 'cpu', got %q", mod.Name())
	}
	data, err := mod.Collect()
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	for _, key := range []string{"physical_cores", "logical_cores", "model_name", "vendor"} {
		if _, ok := data[key]; !ok {
			t.Errorf("missing key %q", key)
		}
	}
}

func TestMemoryModule(t *testing.T) {
	mod := &MemoryModule{}
	if mod.Name() != "memory" {
		t.Fatalf("expected name 'memory', got %q", mod.Name())
	}
	data, err := mod.Collect()
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	for _, key := range []string{"total_bytes", "available_bytes", "used_bytes", "pct_used", "swap_total_bytes", "swap_used_bytes"} {
		if _, ok := data[key]; !ok {
			t.Errorf("missing key %q", key)
		}
	}
}

func TestOSInfoModule(t *testing.T) {
	mod := &OSInfoModule{}
	if mod.Name() != "os_info" {
		t.Fatalf("expected name 'os_info', got %q", mod.Name())
	}
	data, err := mod.Collect()
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	for _, key := range []string{"hostname", "os", "os_version", "arch", "uptime_seconds", "kernel_version"} {
		if _, ok := data[key]; !ok {
			t.Errorf("missing key %q", key)
		}
	}
}

func TestCollectModules(t *testing.T) {
	results := CollectModules([]string{"cpu", "memory"})
	if _, ok := results["cpu"]; !ok {
		t.Error("CollectModules missing 'cpu'")
	}
	if _, ok := results["memory"]; !ok {
		t.Error("CollectModules missing 'memory'")
	}
	if _, ok := results["disk"]; ok {
		t.Error("CollectModules should not include 'disk' when not requested")
	}
}

func TestCollectModulesAll(t *testing.T) {
	results := CollectModules(nil)
	expected := len(Registry())
	if len(results) != expected {
		t.Errorf("expected %d modules, got %d", expected, len(results))
	}
}

// TestValidPackageName guards the fix for dirq-6e4: a readonly query's
// packages.name filter value flows into the rpm/dpkg-query argv, and an
// option-shaped hint like "--pipe=<cmd>" is command execution via rpm.
func TestValidPackageName(t *testing.T) {
	valid := []string{"kernel", "openssl", "glibc-common", "python3.11", "gcc-c++", "lib_foo.bar+baz"}
	for _, s := range valid {
		if !validPackageName(s) {
			t.Errorf("validPackageName(%q) = false, want true", s)
		}
	}
	// Every rejected value is a way to smuggle an option or shell payload.
	invalid := []string{
		"",
		"--pipe=sh -c \"id\"", // the rpm RCE vector
		"-qa",                 // leading-dash option
		"--define=_foo bar",   // rpm macro option
		"kernel; rm -rf /",    // shell metacharacters
		"kernel openssl",      // embedded space (arg splitting)
		"foo`id`",             // backtick
		"foo$(id)",            // command substitution
		"foo\nbar",            // newline
	}
	for _, s := range invalid {
		if validPackageName(s) {
			t.Errorf("validPackageName(%q) = true, want false", s)
		}
	}
}

func TestFilterPackageNames(t *testing.T) {
	in := []string{"kernel", "--pipe=sh -c \"id\"", "openssl", "-qa"}
	got := filterPackageNames(in)
	want := []string{"kernel", "openssl"}
	if len(got) != len(want) {
		t.Fatalf("filterPackageNames(%v) = %v, want %v", in, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("filterPackageNames[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestValidKBName guards the fix for dirq-7zf: a readonly query's hotfixes.name
// filter value is interpolated into a single-quoted PowerShell string literal,
// so a quote-bearing value injects statement-level PowerShell.
func TestValidKBName(t *testing.T) {
	valid := []string{"KB5001234", "kb5001234", "5001234", "KB0"}
	for _, s := range valid {
		if !validKBName(s) {
			t.Errorf("validKBName(%q) = false, want true", s)
		}
	}
	invalid := []string{
		"",
		"KB",                              // prefix with no digits
		"x'); Start-Process calc.exe; ('", // the PowerShell injection vector
		"KB123'; iex($x); '",              // quote breakout
		"KB12.3",                          // non-digit
		"KB 123",                          // space
		"123abc",                          // trailing letters
	}
	for _, s := range invalid {
		if validKBName(s) {
			t.Errorf("validKBName(%q) = true, want false", s)
		}
	}
}

func TestFilterKBNames(t *testing.T) {
	in := []string{"KB5001234", "x'); calc; ('", "5001234", "KB"}
	got := filterKBNames(in)
	want := []string{"KB5001234", "5001234"}
	if len(got) != len(want) {
		t.Fatalf("filterKBNames(%v) = %v, want %v", in, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("filterKBNames[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
