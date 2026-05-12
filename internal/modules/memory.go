package modules

import (
	"github.com/shirou/gopsutil/v4/mem"
)

// MemoryModule collects memory and swap information.
type MemoryModule struct{}

func (m *MemoryModule) Name() string { return "memory" }

func (m *MemoryModule) Collect() (map[string]any, error) {
	vm, err := mem.VirtualMemory()
	if err != nil {
		return nil, err
	}

	sw, err := mem.SwapMemory()
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"total_bytes":      vm.Total,
		"available_bytes":  vm.Available,
		"used_bytes":       vm.Used,
		"pct_used":         vm.UsedPercent,
		"swap_total_bytes": sw.Total,
		"swap_used_bytes":  sw.Used,
	}, nil
}
