// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package modules

import (
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
)

// HotfixesModule collects installed Windows hotfixes (KBs).
type HotfixesModule struct{}

func (h *HotfixesModule) Name() string { return "hotfixes" }

func (h *HotfixesModule) Collect() (map[string]any, error) {
	if runtime.GOOS != "windows" {
		return map[string]any{"hotfixes": []any{}}, nil
	}
	return collectHotfixes()
}

// CollectFiltered queries only the named KBs instead of enumerating all.
func (h *HotfixesModule) CollectFiltered(nameHints []string) (map[string]any, error) {
	if runtime.GOOS != "windows" {
		return map[string]any{"hotfixes": []any{}}, nil
	}
	if len(nameHints) == 0 {
		return collectHotfixes()
	}

	// Build a PowerShell filter for specific KBs.
	quoted := make([]string, len(nameHints))
	for i, kb := range nameHints {
		quoted[i] = "'" + strings.TrimPrefix(strings.ToUpper(kb), "KB") + "'"
	}
	filter := "$kbs = @(" + strings.Join(quoted, ",") + "); "
	psCmd := filter + "Get-HotFix | Where-Object { $kbs -contains ($_.HotFixID -replace 'KB','') } | Select-Object HotFixID, Description, InstalledOn | ConvertTo-Json"

	cmd := exec.Command("powershell", "-NoProfile", "-Command", psCmd)
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return map[string]any{"hotfixes": []any{}}, nil
	}
	return parseHotfixJSON(out)
}

func collectHotfixes() (map[string]any, error) {
	cmd := exec.Command("powershell", "-NoProfile", "-Command",
		"Get-HotFix | Select-Object HotFixID, Description, InstalledOn | ConvertTo-Json")
	out, err := cmd.Output()
	if err != nil {
		return map[string]any{"hotfixes": []any{}}, nil
	}
	return parseHotfixJSON(out)
}

func parseHotfixJSON(out []byte) (map[string]any, error) {
	var entries []struct {
		HotFixID    string `json:"HotFixID"`
		Description string `json:"Description"`
		InstalledOn struct {
			Value string `json:"value"`
		} `json:"InstalledOn"`
	}
	if err := json.Unmarshal(out, &entries); err != nil {
		// Try single object.
		var single struct {
			HotFixID    string `json:"HotFixID"`
			Description string `json:"Description"`
			InstalledOn struct {
				Value string `json:"value"`
			} `json:"InstalledOn"`
		}
		if err := json.Unmarshal(out, &single); err != nil {
			return map[string]any{"hotfixes": []any{}}, nil
		}
		entries = append(entries, single)
	}

	var hotfixes []any
	for _, e := range entries {
		if e.HotFixID == "" {
			continue
		}
		hotfixes = append(hotfixes, map[string]any{
			"kb_id":        e.HotFixID,
			"description":  e.Description,
			"installed_on": e.InstalledOn.Value,
		})
	}
	if hotfixes == nil {
		hotfixes = []any{}
	}
	return map[string]any{"hotfixes": hotfixes}, nil
}
