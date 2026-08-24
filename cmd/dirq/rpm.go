// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"fmt"
	"strings"
)

type pkgInfo struct {
	name    string
	version string
}

// extractPackageList pulls package name/version pairs from query result data.
func extractPackageList(data map[string]any) []pkgInfo {
	var pkgs []pkgInfo

	// The data may have packages as an array under "packages" key
	// or as flattened "packages.name" / "packages.version" fields.
	if nameVal, ok := data["packages.name"]; ok {
		// Flattened single package.
		name, _ := nameVal.(string)
		version, _ := data["packages.version"].(string)
		if name != "" {
			pkgs = append(pkgs, pkgInfo{name, version})
		}
		return pkgs
	}

	if pkgData, ok := data["packages"]; ok {
		switch v := pkgData.(type) {
		case []any:
			for _, item := range v {
				if m, ok := item.(map[string]any); ok {
					name, _ := m["name"].(string)
					version, _ := m["version"].(string)
					if name != "" {
						pkgs = append(pkgs, pkgInfo{name, version})
					}
				}
			}
		case map[string]any:
			name, _ := v["name"].(string)
			version, _ := v["version"].(string)
			if name != "" {
				pkgs = append(pkgs, pkgInfo{name, version})
			}
		}
	}

	return pkgs
}

// parseRPMNEVRA extracts the package name and version-release from an RPM NEVRA string.
// extractRHELVersion extracts the RHEL major version from a CPE string.
// e.g., "cpe:/o:redhat:enterprise_linux:8" → "8"
//
//	"cpe:/o:redhat:enterprise_linux:9::baseos" → "9"
func extractRHELVersion(cpe string) string {
	// CPE format: cpe:/o:redhat:enterprise_linux:VERSION...
	parts := strings.Split(cpe, ":")
	for i, p := range parts {
		if p == "enterprise_linux" && i+1 < len(parts) {
			ver := parts[i+1]
			// Strip sub-parts (e.g., "8::baseos" → "8")
			if idx := strings.IndexAny(ver, ":."); idx >= 0 {
				ver = ver[:idx]
			}
			return ver
		}
	}
	return ""
}

// detectRHELMajor extracts the RHEL major version from an OS version string
// or kernel version by looking for "elN" patterns.
// e.g., "8.10" → "8", "4.18.0-553.33.1.el8_10" → "8", "9.4" → "9"
func detectRHELMajor(osVersion string) string {
	// Look for .elN pattern in the version string (common in kernel/package versions).
	idx := strings.Index(osVersion, ".el")
	if idx >= 0 {
		rest := osVersion[idx+3:]
		var ver string
		for _, ch := range rest {
			if ch >= '0' && ch <= '9' {
				ver += string(ch)
			} else {
				break
			}
		}
		if ver != "" {
			return ver
		}
	}
	// Try simple major version (e.g., "8.10" → "8", "9.4" → "9").
	if dot := strings.Index(osVersion, "."); dot > 0 {
		return osVersion[:dot]
	}
	return ""
}

// Input: "python3-setuptools-0:68.2.2-4.el8_10" or "openssl-1:3.0.7-27.el9"
// Returns: name="python3-setuptools", version="0:68.2.2-4.el8_10"
func parseRPMNEVRA(nevra string) (name, version string) {
	// RPM NEVRA: name-[epoch:]version-release.arch
	// We need to find the boundary between name and epoch:version.
	// The epoch contains a colon, which helps locate it.
	colonIdx := strings.Index(nevra, ":")
	if colonIdx < 0 {
		// No epoch — find the last two hyphens (name-version-release).
		lastDash := strings.LastIndex(nevra, "-")
		if lastDash < 0 {
			return "", ""
		}
		secondLast := strings.LastIndex(nevra[:lastDash], "-")
		if secondLast < 0 {
			return nevra[:lastDash], nevra[lastDash+1:]
		}
		return nevra[:secondLast], nevra[secondLast+1:]
	}

	// Find the dash before the epoch digit.
	epochStart := colonIdx
	for epochStart > 0 && nevra[epochStart-1] != '-' {
		epochStart--
	}
	if epochStart == 0 {
		return "", ""
	}
	return nevra[:epochStart-1], nevra[epochStart:]
}

// rpmVersionCompare compares two RPM version strings.
// Returns -1, 0, or 1 like strcmp.
func rpmVersionCompare(a, b string) int {
	// Strip epoch if present — compare epoch first, then version-release.
	ae, av := splitEpoch(a)
	be, bv := splitEpoch(b)

	if ae != be {
		if ae < be {
			return -1
		}
		return 1
	}

	return rpmVerCmp(av, bv)
}

func splitEpoch(v string) (int, string) {
	if idx := strings.Index(v, ":"); idx >= 0 {
		e := 0
		fmt.Sscanf(v[:idx], "%d", &e)
		return e, v[idx+1:]
	}
	return 0, v
}

// rpmVerCmp implements RPM's version comparison algorithm.
func rpmVerCmp(a, b string) int {
	if a == b {
		return 0
	}

	segA := rpmSegments(a)
	segB := rpmSegments(b)

	for i := 0; i < len(segA) && i < len(segB); i++ {
		sa := segA[i]
		sb := segB[i]

		// Both numeric — compare as integers.
		aNum := isNumeric(sa)
		bNum := isNumeric(sb)

		if aNum && bNum {
			// Strip leading zeros for numeric comparison.
			sa = strings.TrimLeft(sa, "0")
			sb = strings.TrimLeft(sb, "0")
			if len(sa) != len(sb) {
				if len(sa) < len(sb) {
					return -1
				}
				return 1
			}
			if sa < sb {
				return -1
			}
			if sa > sb {
				return 1
			}
			continue
		}

		// Numeric beats alpha.
		if aNum {
			return 1
		}
		if bNum {
			return -1
		}

		// Both alpha — strcmp.
		if sa < sb {
			return -1
		}
		if sa > sb {
			return 1
		}
	}

	if len(segA) < len(segB) {
		return -1
	}
	if len(segA) > len(segB) {
		return 1
	}
	return 0
}

func rpmSegments(v string) []string {
	var segs []string
	i := 0
	for i < len(v) {
		// Skip non-alphanumeric separators.
		for i < len(v) && !isAlnum(v[i]) {
			i++
		}
		if i >= len(v) {
			break
		}
		start := i
		if v[i] >= '0' && v[i] <= '9' {
			for i < len(v) && v[i] >= '0' && v[i] <= '9' {
				i++
			}
		} else {
			for i < len(v) && isAlpha(v[i]) {
				i++
			}
		}
		segs = append(segs, v[start:i])
	}
	return segs
}

func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

func isAlnum(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// ─────────────────────────────────────────────────────────
// Shared query helpers
// ─────────────────────────────────────────────────────────

// parseTagArgs splits args into key=value tags, WHERE clause args, and an
// optional host ID. If the first arg doesn't contain "=" and isn't "WHERE",
// it's treated as a host ID (backwards-compatible with the old syntax).
