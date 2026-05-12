package modules

import (
	"github.com/shirou/gopsutil/v4/disk"
)

// DiskModule collects partition and usage information.
type DiskModule struct{}

func (d *DiskModule) Name() string { return "disk" }

func (d *DiskModule) Collect() (map[string]any, error) {
	partitions, err := disk.Partitions(false)
	if err != nil {
		return nil, err
	}

	var parts []any
	for _, p := range partitions {
		usage, err := disk.Usage(p.Mountpoint)
		if err != nil {
			continue
		}
		parts = append(parts, map[string]any{
			"device":      p.Device,
			"mount_point": p.Mountpoint,
			"fs_type":     p.Fstype,
			"total_bytes": float64(usage.Total),
			"used_bytes":  float64(usage.Used),
			"free_bytes":  float64(usage.Free),
			"pct_used":    usage.UsedPercent,
		})
	}

	return map[string]any{
		"partitions": parts,
	}, nil
}
