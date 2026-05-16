// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package modules

import (
	"os"
	"runtime"

	"github.com/shirou/gopsutil/v4/host"
)

// OSInfoModule collects operating system information.
type OSInfoModule struct{}

func (o *OSInfoModule) Name() string { return "os_info" }

func (o *OSInfoModule) Collect() (map[string]any, error) {
	hostname, _ := os.Hostname()

	info, err := host.Info()
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"hostname":       hostname,
		"os":             runtime.GOOS,
		"os_version":     info.PlatformVersion,
		"arch":           runtime.GOARCH,
		"uptime_seconds": info.Uptime,
		"kernel_version": info.KernelVersion,
		"distro":         info.Platform,        // e.g., "rhel", "fedora", "ubuntu", "centos"
		"distro_version": info.PlatformVersion, // e.g., "8.10", "43", "22.04"
		"distro_family":  info.PlatformFamily,  // e.g., "rhel", "debian", "suse"
	}, nil
}
