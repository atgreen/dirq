// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package modules

import (
	"github.com/shirou/gopsutil/v4/cpu"
)

// CPUModule collects CPU information.
type CPUModule struct{}

func (c *CPUModule) Name() string { return "cpu" }

func (c *CPUModule) Collect() (map[string]any, error) {
	physicalCores, err := cpu.Counts(false)
	if err != nil {
		return nil, err
	}

	logicalCores, err := cpu.Counts(true)
	if err != nil {
		return nil, err
	}

	infos, err := cpu.Info()
	if err != nil {
		return nil, err
	}

	var modelName, vendor string
	if len(infos) > 0 {
		modelName = infos[0].ModelName
		vendor = infos[0].VendorID
	}

	return map[string]any{
		"physical_cores": physicalCores,
		"logical_cores":  logicalCores,
		"model_name":     modelName,
		"vendor":         vendor,
	}, nil
}
