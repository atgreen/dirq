// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package modules

import (
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
)

// PackagesModule collects installed package information.
type PackagesModule struct{}

func (p *PackagesModule) Name() string { return "packages" }

func (p *PackagesModule) Collect() (map[string]any, error) {
	var packages []any

	switch runtime.GOOS {
	case "linux":
		packages = collectLinuxPackages()
	case "windows":
		packages = collectWindowsPackages()
	}

	if packages == nil {
		packages = []any{}
	}

	return map[string]any{
		"packages": packages,
	}, nil
}

// CollectFiltered queries only the named packages instead of enumerating all
// installed packages. Falls back to full collection on any error.
func (p *PackagesModule) CollectFiltered(nameHints []string) (map[string]any, error) {
	if len(nameHints) == 0 || runtime.GOOS != "linux" {
		return p.Collect()
	}

	var packages []any

	if rpmPath, err := exec.LookPath("rpm"); err == nil {
		// rpm -q kernel openssl → only query specific packages.
		args := append([]string{"-q", "--queryformat", "%{NAME}\t%{VERSION}-%{RELEASE}\t%{ARCH}\n"}, nameHints...)
		cmd := exec.Command(rpmPath, args...)
		out, err := cmd.Output()
		if err != nil {
			// Some packages may not be installed — that's fine, rpm -q
			// returns non-zero but still outputs found packages.
			if len(out) == 0 {
				return map[string]any{"packages": []any{}}, nil
			}
		}
		packages = parseTabSeparated(string(out), "rpm")
	} else if dpkgPath, err := exec.LookPath("dpkg-query"); err == nil {
		// dpkg-query -W kernel openssl
		args := append([]string{"-W", "-f=${Package}\t${Version}\t${Architecture}\n"}, nameHints...)
		cmd := exec.Command(dpkgPath, args...)
		out, err := cmd.Output()
		if err != nil {
			if len(out) == 0 {
				return map[string]any{"packages": []any{}}, nil
			}
		}
		packages = parseTabSeparated(string(out), "dpkg")
	} else {
		return p.Collect()
	}

	if packages == nil {
		packages = []any{}
	}
	return map[string]any{"packages": packages}, nil
}

func collectLinuxPackages() []any {
	// Try rpm first
	if rpmPath, err := exec.LookPath("rpm"); err == nil {
		return collectRPM(rpmPath)
	}
	// Fall back to dpkg
	if dpkgPath, err := exec.LookPath("dpkg-query"); err == nil {
		return collectDPKG(dpkgPath)
	}
	return nil
}

func collectRPM(rpmPath string) []any {
	cmd := exec.Command(rpmPath, "-qa", "--queryformat", "%{NAME}\t%{VERSION}-%{RELEASE}\t%{ARCH}\n")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseTabSeparated(string(out), "rpm")
}

func collectDPKG(dpkgPath string) []any {
	cmd := exec.Command(dpkgPath, "-W", "-f=${Package}\t${Version}\t${Architecture}\n")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseTabSeparated(string(out), "dpkg")
}

func parseTabSeparated(output, source string) []any {
	var packages []any
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 3 {
			continue
		}
		packages = append(packages, map[string]any{
			"name":    fields[0],
			"version": fields[1],
			"arch":    fields[2],
			"source":  source,
		})
	}
	return packages
}

func collectWindowsPackages() []any {
	cmd := exec.Command("powershell", "-Command",
		"Get-ItemProperty HKLM:\\Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\* | Select-Object DisplayName, DisplayVersion | ConvertTo-Json")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	// The output may be a single object or an array
	var entries []struct {
		DisplayName    string `json:"DisplayName"`
		DisplayVersion string `json:"DisplayVersion"`
	}
	if err := json.Unmarshal(out, &entries); err != nil {
		// Try as single object
		var single struct {
			DisplayName    string `json:"DisplayName"`
			DisplayVersion string `json:"DisplayVersion"`
		}
		if err := json.Unmarshal(out, &single); err != nil {
			return nil
		}
		entries = append(entries, single)
	}

	var packages []any
	for _, e := range entries {
		if e.DisplayName == "" {
			continue
		}
		packages = append(packages, map[string]any{
			"name":    e.DisplayName,
			"version": e.DisplayVersion,
			"arch":    "",
			"source":  "registry",
		})
	}
	return packages
}
