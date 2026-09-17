// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package modules

import (
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
)

// validPackageName reports whether s is a well-formed package-name hint safe to
// pass as a positional argument to rpm/dpkg-query. Hints originate from a
// readonly query's WHERE clause (packages.name = '…'), so a hostile value must
// never be parseable as a command-line option: rpm treats a leading-dash
// argument like "--pipe=<cmd>" as an option and runs <cmd> through a shell.
// We require a real package-name shape — [A-Za-z0-9._+-], no leading dash —
// which cannot be mistaken for a flag. This validation and the "--" separator
// in CollectFiltered are belt and braces.
func validPackageName(s string) bool {
	if s == "" || s[0] == '-' {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '+' || r == '-':
		default:
			return false
		}
	}
	return true
}

// filterPackageNames drops any hint that is not a well-formed package name.
func filterPackageNames(hints []string) []string {
	out := hints[:0:0]
	for _, h := range hints {
		if validPackageName(h) {
			out = append(out, h)
		}
	}
	return out
}

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

	// Drop any hint that isn't a well-formed package name. A readonly query
	// controls these values, and an option-shaped hint (e.g. "--pipe=<cmd>")
	// is command execution via rpm. If nothing survives, fall back to a full
	// enumeration rather than running the query tool with no operands.
	nameHints = filterPackageNames(nameHints)
	if len(nameHints) == 0 {
		return p.Collect()
	}

	var packages []any

	if rpmPath, err := exec.LookPath("rpm"); err == nil {
		// rpm -q kernel openssl → only query specific packages. "--" stops
		// option parsing so a hint can never be read as a flag.
		args := append([]string{"-q", "--queryformat", "%{NAME}\t%{VERSION}-%{RELEASE}\t%{ARCH}\n", "--"}, nameHints...)
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
		// dpkg-query -W kernel openssl. "--" stops option parsing so a hint
		// can never be read as a flag.
		args := append([]string{"-W", "-f=${Package}\t${Version}\t${Architecture}\n", "--"}, nameHints...)
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
