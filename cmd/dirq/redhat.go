// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// ─────────────────────────────────────────────────────────
// Red Hat Security Data API — shared CVE/errata assessment
// used by the cve and errata subcommands and the MCP tools.
// ─────────────────────────────────────────────────────────

// fixedPkg describes the fix state of one package on one RHEL major version.
type fixedPkg struct {
	name       string // RPM source package name
	fullNEVRA  string // full name-epoch:version-release string
	fixVersion string // version-release portion for comparison ("" = no fix)
	rhelVer    string // RHEL major version ("8", "9", etc.)
}

type fixKey struct{ name, rhelVer string }

// cveInfo is the RHEL-relevant part of a Red Hat Security Data CVE record.
type cveInfo struct {
	ID          string
	Severity    string
	Description string
	FixedPkgs   []fixedPkg // deduped per name+RHEL major; kpatch excluded
}

// fetchRedHatCVE looks up a CVE in the Red Hat Security Data API and
// extracts the RHEL packages it affects, with fixed versions where released.
func fetchRedHatCVE(cveID string) (*cveInfo, error) {
	resp, err := http.Get("https://access.redhat.com/hydra/rest/securitydata/cve/" + cveID + ".json")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch CVE data: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, fmt.Errorf("CVE %s not found in Red Hat Security Data", cveID)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Red Hat API returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read CVE response: %w", err)
	}

	var cveData struct {
		Name           string `json:"name"`
		ThreatSeverity string `json:"threat_severity"`
		Bugzilla       struct {
			Description string `json:"description"`
		} `json:"bugzilla"`
		AffectedRelease []struct {
			Package string `json:"package"`
			CPE     string `json:"cpe"`
		} `json:"affected_release"`
		PackageState []struct {
			FixState    string `json:"fix_state"`
			PackageName string `json:"package_name"`
			CPE         string `json:"cpe"`
		} `json:"package_state"`
	}
	if err := json.Unmarshal(body, &cveData); err != nil {
		return nil, fmt.Errorf("parse CVE data: %w", err)
	}

	ci := &cveInfo{
		ID:          cveID,
		Severity:    cveData.ThreatSeverity,
		Description: cveData.Bugzilla.Description,
	}

	seen := map[string]bool{} // "name:rhelVer" dedup key
	for _, ar := range cveData.AffectedRelease {
		if ar.Package == "" || !strings.Contains(ar.CPE, "enterprise_linux") {
			continue
		}
		rhelVer := extractRHELVersion(ar.CPE)
		if rhelVer == "" {
			continue
		}
		name, version := parseRPMNEVRA(ar.Package)
		if name == "" {
			continue
		}
		// Skip kpatch — it's a live-patching workaround, not
		// the actual package fix.
		if strings.HasPrefix(name, "kpatch") {
			continue
		}
		if seen[name+":"+rhelVer] {
			continue
		}
		seen[name+":"+rhelVer] = true
		ci.FixedPkgs = append(ci.FixedPkgs, fixedPkg{
			name:       name,
			fullNEVRA:  ar.Package,
			fixVersion: version,
			rhelVer:    rhelVer,
		})
	}

	// Packages still awaiting a fix ("Affected") have no version to compare.
	for _, ps := range cveData.PackageState {
		if ps.FixState != "Affected" || !strings.Contains(ps.CPE, "enterprise_linux") {
			continue
		}
		rhelVer := extractRHELVersion(ps.CPE)
		if rhelVer == "" || ps.PackageName == "" || strings.HasPrefix(ps.PackageName, "kpatch") {
			continue
		}
		if seen[ps.PackageName+":"+rhelVer] {
			continue
		}
		seen[ps.PackageName+":"+rhelVer] = true
		ci.FixedPkgs = append(ci.FixedPkgs, fixedPkg{name: ps.PackageName, rhelVer: rhelVer})
	}

	return ci, nil
}

// advisoryCVE is one CVE entry covered by a Red Hat advisory.
type advisoryCVE struct {
	ID          string
	Severity    string
	Description string
}

// fetchRedHatAdvisory looks up an advisory (RHSA/RHBA/RHEA) and returns the
// CVEs it covers plus the fixed packages keyed by package+RHEL major,
// keeping the highest fix version per combo.
func fetchRedHatAdvisory(advisoryID string) ([]advisoryCVE, map[fixKey]fixedPkg, error) {
	resp, err := http.Get("https://access.redhat.com/hydra/rest/securitydata/cve.json?advisory=" + advisoryID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch advisory data: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, nil, fmt.Errorf("advisory %s not found", advisoryID)
	}
	if resp.StatusCode != 200 {
		return nil, nil, fmt.Errorf("Red Hat API returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read response: %w", err)
	}

	var cveEntries []struct {
		CVE              string   `json:"CVE"`
		Severity         string   `json:"severity"`
		BugzillaDesc     string   `json:"bugzilla_description"`
		AffectedPackages []string `json:"affected_packages"`
	}
	if err := json.Unmarshal(body, &cveEntries); err != nil {
		return nil, nil, fmt.Errorf("parse advisory data: %w", err)
	}

	fixes := map[fixKey]fixedPkg{}
	cves := make([]advisoryCVE, 0, len(cveEntries))
	for _, cve := range cveEntries {
		cves = append(cves, advisoryCVE{ID: cve.CVE, Severity: cve.Severity, Description: cve.BugzillaDesc})
		for _, pkg := range cve.AffectedPackages {
			name, version := parseRPMNEVRA(pkg)
			if name == "" || strings.HasPrefix(name, "kpatch") {
				continue
			}
			rhelVer := detectRHELMajor(version)
			if rhelVer == "" {
				continue
			}
			key := fixKey{name, rhelVer}
			// Keep the highest fix version per package+RHEL combo.
			if existing, ok := fixes[key]; ok && rpmVersionCompare(version, existing.fixVersion) <= 0 {
				continue
			}
			fixes[key] = fixedPkg{
				name:       name,
				fullNEVRA:  pkg,
				fixVersion: version,
				rhelVer:    rhelVer,
			}
		}
	}

	return cves, fixes, nil
}

// fixesMap indexes fix records by package+RHEL major.
func fixesMap(pkgs []fixedPkg) map[fixKey]fixedPkg {
	m := make(map[fixKey]fixedPkg, len(pkgs))
	for _, fp := range pkgs {
		m[fixKey{fp.name, fp.rhelVer}] = fp
	}
	return m
}

// fixesPkgNames returns the sorted unique package names in a fix map.
func fixesPkgNames(fixes map[fixKey]fixedPkg) []string {
	seen := map[string]bool{}
	var names []string
	for k := range fixes {
		if !seen[k.name] {
			seen[k.name] = true
			names = append(names, k.name)
		}
	}
	sort.Strings(names)
	return names
}

// buildPkgScanQuery builds the fleet query for hosts running any of the
// packages, restricted to the RHEL family (rhel, centos, rocky, alma,
// oracle). extraArgs are ANDed onto the WHERE clause; a leading WHERE
// keyword is tolerated.
func buildPkgScanQuery(pkgNames []string, extraArgs []string) string {
	inList := make([]string, len(pkgNames))
	for i, n := range pkgNames {
		inList[i] = "'" + n + "'"
	}
	q := fmt.Sprintf("SELECT hostname, os_info.distro_version, os_info.kernel_version, packages.name, packages.version WHERE packages.name IN (%s) AND os_info.distro_family = 'rhel'",
		strings.Join(inList, ", "))
	if len(extraArgs) > 0 {
		extra := " AND " + strings.Join(extraArgs, " ")
		// Strip leading WHERE if the user wrote it.
		extra = strings.Replace(extra, " AND WHERE ", " AND ", 1)
		extra = strings.Replace(extra, " AND where ", " AND ", 1)
		q += extra
	}
	return q
}

// fleetScanResponse is the /api/v1/query response shape a package scan uses.
type fleetScanResponse struct {
	TotalTargets int `json:"total_targets"`
	Received     int `json:"received"`
	Results      []struct {
		Hostname string         `json:"hostname"`
		Success  bool           `json:"success"`
		Error    string         `json:"error"`
		Data     map[string]any `json:"data"`
	} `json:"results"`
}

// queryFleetPkgs runs a fleet query and returns the parsed response plus the
// raw JSON (for --json output).
func queryFleetPkgs(queryStr string, timeout int) (*fleetScanResponse, []byte, error) {
	body, _ := json.Marshal(map[string]any{
		"query":   queryStr,
		"timeout": timeout,
	})
	raw, err := apiRequest("POST", "/api/v1/query", bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("fleet query failed: %w", err)
	}
	var result fleetScanResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, nil, fmt.Errorf("parse query result: %w", err)
	}
	return &result, raw, nil
}

// scanOutcome tallies a fleet vulnerability scan.
type scanOutcome struct {
	vulnerable int
	patched    int
	noFix      int
	assessed   map[string]bool // hostnames that returned RHEL data
}

// assessFleetScan compares each host's package versions against the fix map,
// writing one row per assessed package to w. When reportNoFix is set,
// packages that are affected but have no fixed version are reported as
// vulnerable (used for CVEs; advisory fixes always carry a version).
func assessFleetScan(w io.Writer, result *fleetScanResponse, fixes map[fixKey]fixedPkg, reportNoFix bool) *scanOutcome {
	o := &scanOutcome{assessed: map[string]bool{}}

	// Track kernel packages already reported per host (results contain one
	// row per installed kernel, but we only want one comparison using the
	// running kernel).
	kernelHandled := map[string]bool{} // "hostname:pkgname" → reported

	for _, r := range result.Results {
		if !r.Success {
			continue
		}

		// Detect RHEL major version from distro_version (e.g., "8.10" → "8").
		distroVer, _ := r.Data["os_info.distro_version"].(string)
		if distroVer == "" {
			if oi, ok := r.Data["os_info"].(map[string]any); ok {
				distroVer, _ = oi["distro_version"].(string)
			}
		}
		hostRHEL := detectRHELMajor(distroVer)
		if hostRHEL == "" {
			continue // not RHEL-family, skip
		}
		o.assessed[r.Hostname] = true

		// Get running kernel version for kernel package comparisons.
		runningKernel, _ := r.Data["os_info.kernel_version"].(string)
		if runningKernel == "" {
			if oi, ok := r.Data["os_info"].(map[string]any); ok {
				runningKernel, _ = oi["kernel_version"].(string)
			}
		}

		for _, pkg := range extractPackageList(r.Data) {
			isKernelPkg := pkg.name == "kernel" || pkg.name == "kernel-rt"
			if isKernelPkg {
				dedupKey := r.Hostname + ":" + pkg.name
				if kernelHandled[dedupKey] {
					continue // already reported for this host
				}
				kernelHandled[dedupKey] = true
				if runningKernel == "" {
					continue // can't assess without the running version
				}
				pkg.version = runningKernel
			}

			label := pkg.version
			if isKernelPkg {
				label += " (running)"
			}

			fp, hasFix := fixes[fixKey{pkg.name, hostRHEL}]
			if !hasFix {
				if !reportNoFix {
					continue
				}
				// Affected on some RHEL major but no fix for this one.
				affected := false
				for k := range fixes {
					if k.name == pkg.name {
						affected = true
						break
					}
				}
				if affected {
					fmt.Fprintf(w, "  %-24s %-20s %-24s  VULNERABLE (no fix for RHEL %s)\n",
						r.Hostname, pkg.name, label, hostRHEL)
					o.noFix++
				}
				continue
			}

			if fp.fixVersion == "" {
				if reportNoFix {
					fmt.Fprintf(w, "  %-24s %-20s %-24s  VULNERABLE (no fix available)\n",
						r.Hostname, pkg.name, label)
					o.noFix++
				}
				continue
			}

			if rpmVersionCompare(pkg.version, fp.fixVersion) < 0 {
				fmt.Fprintf(w, "  %-24s %-20s %-24s  VULNERABLE (fixed in %s)\n",
					r.Hostname, pkg.name, label, fp.fullNEVRA)
				o.vulnerable++
			} else {
				fmt.Fprintf(w, "  %-24s %-20s %-24s  patched\n",
					r.Hostname, pkg.name, label)
				o.patched++
			}
		}
	}

	return o
}

// writeScanSummary prints the roll-up line. Unassessed hosts are split into
// non-RHEL (genuine skip) vs RHEL-no-response (mesh/timeout symptom) so the
// operator knows what to investigate.
func writeScanSummary(w io.Writer, o *scanOutcome) {
	nonRHEL, rhelNoResponse := countNotAssessed(o.assessed)

	fmt.Fprintf(w, "\n%d vulnerable, %d patched", o.vulnerable, o.patched)
	if o.noFix > 0 {
		fmt.Fprintf(w, ", %d no fix available", o.noFix)
	}
	if nonRHEL > 0 {
		fmt.Fprintf(w, ", %d non-RHEL", nonRHEL)
	}
	if rhelNoResponse > 0 {
		fmt.Fprintf(w, ", %d RHEL did not respond", rhelNoResponse)
	}
	fmt.Fprintln(w)
}

// countNotAssessed inspects the fleet's online agent records and returns
// (non_rhel, rhel_but_did_not_respond) — assessedHosts is the set of
// hostnames that returned data for a RHEL-targeted query.
func countNotAssessed(assessedHosts map[string]bool) (nonRHEL, rhelNoResponse int) {
	hostsResp, err := apiRequest("GET", "/api/v1/hosts", nil)
	if err != nil {
		return 0, 0
	}
	var agents []struct {
		Hostname string `json:"hostname"`
		OS       string `json:"os"`
		Online   bool   `json:"online"`
	}
	if json.Unmarshal(hostsResp, &agents) != nil {
		return 0, 0
	}
	for _, a := range agents {
		if !a.Online {
			continue
		}
		// Match the RHEL-family set used by the query filter.
		isRHEL := false
		switch strings.ToLower(a.OS) {
		case "rhel", "redhat", "red hat enterprise linux", "centos", "rocky", "almalinux", "alma", "oracle":
			isRHEL = true
		}
		if !isRHEL {
			nonRHEL++
			continue
		}
		if !assessedHosts[a.Hostname] {
			rhelNoResponse++
		}
	}
	return nonRHEL, rhelNoResponse
}
