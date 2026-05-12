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
	}, nil
}
