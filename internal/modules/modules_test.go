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
	if len(results) != 4 {
		t.Errorf("expected 4 modules, got %d", len(results))
	}
}
