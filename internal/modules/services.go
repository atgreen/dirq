// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package modules

import (
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
)

// ServicesModule collects information about running system services.
type ServicesModule struct{}

func (s *ServicesModule) Name() string { return "services" }

func (s *ServicesModule) Collect() (map[string]any, error) {
	var services []any

	switch runtime.GOOS {
	case "linux":
		services = collectLinuxServices()
	case "windows":
		services = collectWindowsServices()
	default:
		services = []any{}
	}

	return map[string]any{
		"services": services,
	}, nil
}

// collectLinuxServices uses systemctl to gather service information.
func collectLinuxServices() []any {
	// Get loaded services and their states.
	out, err := exec.Command(
		"systemctl", "list-units", "--type=service",
		"--no-pager", "--plain", "--no-legend",
	).Output()
	if err != nil {
		return []any{}
	}

	// Build a map of service name -> enabled status by querying
	// list-unit-files once (much cheaper than per-service calls).
	enabledMap := buildEnabledMap()

	var services []any
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		unit := fields[0]                 // e.g. "sshd.service"
		sub := strings.ToLower(fields[3]) // SUB column: running, exited, dead, etc.

		// Derive a friendly state.
		state := sub
		switch sub {
		case "running":
			state = "running"
		case "exited", "dead", "failed":
			state = "stopped"
		}

		// Derive a friendly name by stripping the .service suffix.
		name := strings.TrimSuffix(unit, ".service")

		startType := "unknown"
		if v, ok := enabledMap[unit]; ok {
			startType = v
		}

		services = append(services, map[string]any{
			"name":         name,
			"display_name": name,
			"state":        state,
			"start_type":   startType,
		})
	}

	if services == nil {
		services = []any{}
	}
	return services
}

// buildEnabledMap parses systemctl list-unit-files to map unit names to
// their enabled/disabled/static/masked status.
func buildEnabledMap() map[string]string {
	m := make(map[string]string)
	out, err := exec.Command(
		"systemctl", "list-unit-files", "--type=service",
		"--no-pager", "--plain", "--no-legend",
	).Output()
	if err != nil {
		return m
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		unit := fields[0]
		state := strings.ToLower(fields[1])
		// Normalise common states.
		switch state {
		case "enabled", "enabled-runtime":
			state = "enabled"
		case "disabled":
			state = "disabled"
		case "static":
			state = "static"
		case "masked", "masked-runtime":
			state = "disabled"
		case "indirect":
			state = "manual"
		default:
			// Keep the raw value.
		}
		m[unit] = state
	}
	return m
}

// collectWindowsServices uses PowerShell to gather service information.
func collectWindowsServices() []any {
	out, err := exec.Command(
		"powershell", "-Command",
		"Get-Service | Select-Object Name, DisplayName, Status, StartType | ConvertTo-Json",
	).Output()
	if err != nil {
		return []any{}
	}

	// PowerShell may return a single object (not array) if only one
	// service exists; handle both cases.
	var raw json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		return []any{}
	}

	type winService struct {
		Name        string `json:"Name"`
		DisplayName string `json:"DisplayName"`
		Status      int    `json:"Status"`
		StartType   int    `json:"StartType"`
	}

	var winServices []winService
	if err := json.Unmarshal(out, &winServices); err != nil {
		// Try single object.
		var single winService
		if err2 := json.Unmarshal(out, &single); err2 != nil {
			return []any{}
		}
		winServices = []winService{single}
	}

	statusMap := map[int]string{
		1: "stopped",
		2: "starting",
		3: "stopping",
		4: "running",
		5: "continuing",
		6: "pausing",
		7: "paused",
	}

	startTypeMap := map[int]string{
		0: "boot",
		1: "system",
		2: "enabled",
		3: "manual",
		4: "disabled",
	}

	services := make([]any, 0, len(winServices))
	for _, ws := range winServices {
		state := "unknown"
		if v, ok := statusMap[ws.Status]; ok {
			state = v
		}
		startType := "unknown"
		if v, ok := startTypeMap[ws.StartType]; ok {
			startType = v
		}
		services = append(services, map[string]any{
			"name":         ws.Name,
			"display_name": ws.DisplayName,
			"state":        state,
			"start_type":   startType,
		})
	}
	return services
}
