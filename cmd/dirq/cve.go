// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func cveCmd() *cobra.Command {
	var (
		timeout int
		verbose bool
	)

	cmd := &cobra.Command{
		Use:   "cve [CVE-ID] [WHERE ...]",
		Short: "Scan RHEL systems for a CVE vulnerability",
		Long: `Look up a CVE in the Red Hat Security Data API, then scan the fleet
for RHEL systems running vulnerable package versions.

Examples:
  dirq cve CVE-2024-6345
  dirq cve CVE-2024-6345 WHERE tag.env = 'prod'
  dirq "cve CVE-2024-6345 where tag.env = 'prod'"
  dirq cve CVE-2024-6345 --json`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cveID := strings.ToUpper(args[0])
			if !strings.HasPrefix(cveID, "CVE-") {
				return fmt.Errorf("expected a CVE ID like CVE-2024-1234, got %q", cveID)
			}

			logStep := func(format string, a ...any) {
				if verbose {
					fmt.Fprintf(os.Stderr, "[%s] %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, a...))
				}
			}

			// Fetch CVE data from Red Hat.
			fmt.Fprintf(os.Stderr, "Fetching %s from Red Hat Security Data API...\n", cveID)
			stepStart := time.Now()

			cveURL := "https://access.redhat.com/hydra/rest/securitydata/cve/" + cveID + ".json"
			resp, err := http.Get(cveURL)
			if err != nil {
				return fmt.Errorf("failed to fetch CVE data: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode == 404 {
				return fmt.Errorf("CVE %s not found in Red Hat Security Data", cveID)
			}
			if resp.StatusCode != 200 {
				return fmt.Errorf("Red Hat API returned HTTP %d", resp.StatusCode)
			}

			cveBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read CVE response: %w", err)
			}

			var cveData struct {
				Name           string `json:"name"`
				ThreatSeverity string `json:"threat_severity"`
				Bugzilla       struct {
					Description string `json:"description"`
				} `json:"bugzilla"`
				AffectedRelease []struct {
					ProductName string `json:"product_name"`
					Advisory    string `json:"advisory"`
					Package     string `json:"package"`
					CPE         string `json:"cpe"`
				} `json:"affected_release"`
				PackageState []struct {
					ProductName string `json:"product_name"`
					FixState    string `json:"fix_state"`
					PackageName string `json:"package_name"`
					CPE         string `json:"cpe"`
				} `json:"package_state"`
			}
			if err := json.Unmarshal(cveBody, &cveData); err != nil {
				return fmt.Errorf("parse CVE data: %w", err)
			}

			logStep("CVE data fetched in %s", time.Since(stepStart))

			fmt.Fprintf(os.Stderr, "%s: %s\n", cveID, cveData.Bugzilla.Description)
			fmt.Fprintf(os.Stderr, "Severity: %s\n", cveData.ThreatSeverity)

			// Extract affected package names and fixed versions from affected_release,
			// keyed by RHEL major version (e.g., "8", "9", "10").
			type fixedPkg struct {
				name       string // RPM source package name
				fullNEVRA  string // full name-epoch:version-release string
				fixVersion string // version-release portion for comparison
				rhelVer    string // RHEL major version ("8", "9", etc.)
			}

			var fixedPkgs []fixedPkg
			seenPkgs := map[string]bool{} // "name:rhelVer" dedup key
			allPkgNames := map[string]bool{}

			for _, ar := range cveData.AffectedRelease {
				if ar.Package == "" {
					continue
				}
				if !strings.Contains(ar.CPE, "enterprise_linux") {
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
				dedup := name + ":" + rhelVer
				if seenPkgs[dedup] {
					continue
				}
				seenPkgs[dedup] = true
				allPkgNames[name] = true
				fixedPkgs = append(fixedPkgs, fixedPkg{
					name:       name,
					fullNEVRA:  ar.Package,
					fixVersion: version,
					rhelVer:    rhelVer,
				})
			}

			// Also collect package names from package_state where fix_state is "Affected".
			for _, ps := range cveData.PackageState {
				if ps.FixState != "Affected" {
					continue
				}
				if !strings.Contains(ps.CPE, "enterprise_linux") {
					continue
				}
				rhelVer := extractRHELVersion(ps.CPE)
				if rhelVer == "" {
					continue
				}
				if strings.HasPrefix(ps.PackageName, "kpatch") {
					continue
				}
				dedup := ps.PackageName + ":" + rhelVer
				if !seenPkgs[dedup] {
					seenPkgs[dedup] = true
					allPkgNames[ps.PackageName] = true
					fixedPkgs = append(fixedPkgs, fixedPkg{
						name:    ps.PackageName,
						rhelVer: rhelVer,
					})
				}
			}

			if len(fixedPkgs) == 0 {
				fmt.Println("No RHEL packages associated with this CVE.")
				return nil
			}

			// Build package name list for display and query.
			pkgNames := make([]string, 0, len(allPkgNames))
			for n := range allPkgNames {
				pkgNames = append(pkgNames, n)
			}
			sort.Strings(pkgNames)

			fmt.Fprintf(os.Stderr, "Packages: %s\n", strings.Join(pkgNames, ", "))
			for _, fp := range fixedPkgs {
				if fp.fixVersion != "" {
					fmt.Fprintf(os.Stderr, "  %s (RHEL %s): fixed in %s\n", fp.name, fp.rhelVer, fp.fullNEVRA)
				} else {
					fmt.Fprintf(os.Stderr, "  %s (RHEL %s): no fix available (still affected)\n", fp.name, fp.rhelVer)
				}
			}
			fmt.Fprintln(os.Stderr)

			// Build DirQ query to find RHEL hosts with these packages installed.
			// Filter to RHEL-family only (rhel, centos, rocky, alma, oracle).
			inList := make([]string, len(pkgNames))
			for i, n := range pkgNames {
				inList[i] = "'" + n + "'"
			}
			pkgFilter := "packages.name IN (" + strings.Join(inList, ", ") + ")"
			osFilter := "os_info.distro_family = 'rhel'"

			// Add WHERE clause from remaining args if provided.
			var whereExtra string
			if len(args) > 1 {
				whereExtra = " AND " + strings.Join(args[1:], " ")
				// Strip leading WHERE if the user wrote it.
				whereExtra = strings.Replace(whereExtra, " AND WHERE ", " AND ", 1)
				whereExtra = strings.Replace(whereExtra, " AND where ", " AND ", 1)
			}

			queryStr := fmt.Sprintf("SELECT hostname, os_info.distro_version, os_info.kernel_version, packages.name, packages.version WHERE %s AND %s%s",
				pkgFilter, osFilter, whereExtra)

			logStep("Query: %s", queryStr)
			fmt.Fprintf(os.Stderr, "Scanning fleet...\n\n")
			stepStart = time.Now()

			// Run query.
			body, _ := json.Marshal(map[string]any{
				"query":   queryStr,
				"timeout": timeout,
			})
			queryResp, err := apiRequest("POST", "/api/v1/query", bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("fleet query failed: %w", err)
			}

			var result struct {
				TotalTargets int `json:"total_targets"`
				Received     int `json:"received"`
				Results      []struct {
					Hostname string         `json:"hostname"`
					Success  bool           `json:"success"`
					Error    string         `json:"error"`
					Data     map[string]any `json:"data"`
				} `json:"results"`
			}
			if err := json.Unmarshal(queryResp, &result); err != nil {
				return fmt.Errorf("parse query result: %w", err)
			}

			logStep("Fleet query returned %d results from %d targets in %s",
				result.Received, result.TotalTargets, time.Since(stepStart))

			if jsonOut {
				fmt.Println(string(queryResp))
				return nil
			}

			// Build fixed version lookup keyed by "pkgname:rhelver".
			type fixKey struct{ name, rhelVer string }
			fixedVersionMap := map[fixKey]fixedPkg{}
			for _, fp := range fixedPkgs {
				fixedVersionMap[fixKey{fp.name, fp.rhelVer}] = fp
			}

			vulnerable := 0
			patched := 0
			noFix := 0
			assessedHosts := map[string]bool{}

			// Track kernel packages already reported per host (results
			// contain one row per installed kernel, but we only want one
			// comparison using the running kernel).
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
				assessedHosts[r.Hostname] = true

				// Get running kernel version for kernel package comparisons.
				runningKernel, _ := r.Data["os_info.kernel_version"].(string)
				if runningKernel == "" {
					if oi, ok := r.Data["os_info"].(map[string]any); ok {
						runningKernel, _ = oi["kernel_version"].(string)
					}
				}

				// Extract packages from results.
				pkgs := extractPackageList(r.Data)

				for _, pkg := range pkgs {
					isKernelPkg := pkg.name == "kernel" || pkg.name == "kernel-rt"
					if isKernelPkg {
						dedupKey := r.Hostname + ":" + pkg.name
						if kernelHandled[dedupKey] {
							continue // already reported for this host
						}
						kernelHandled[dedupKey] = true
						if runningKernel == "" {
							continue
						}
						pkg.version = runningKernel
					}

					fp, hasfix := fixedVersionMap[fixKey{pkg.name, hostRHEL}]
					if !hasfix {
						affected := false
						for k := range fixedVersionMap {
							if k.name == pkg.name {
								affected = true
								break
							}
						}
						if affected {
							label := pkg.version
							if isKernelPkg {
								label += " (running)"
							}
							fmt.Printf("  %-24s %-20s %-24s  VULNERABLE (no fix for RHEL %s)\n",
								r.Hostname, pkg.name, label, hostRHEL)
							noFix++
						}
						continue
					}

					if fp.fixVersion == "" {
						label := pkg.version
						if isKernelPkg {
							label += " (running)"
						}
						fmt.Printf("  %-24s %-20s %-24s  VULNERABLE (no fix available)\n",
							r.Hostname, pkg.name, label)
						noFix++
						continue
					}

					label := pkg.version
					if isKernelPkg {
						label += " (running)"
					}
					if rpmVersionCompare(pkg.version, fp.fixVersion) < 0 {
						fmt.Printf("  %-24s %-20s %-24s  VULNERABLE (fixed in %s)\n",
							r.Hostname, pkg.name, label, fp.fullNEVRA)
						vulnerable++
					} else {
						fmt.Printf("  %-24s %-20s %-24s  patched\n",
							r.Hostname, pkg.name, label)
						patched++
					}
				}
			}

			// Split "not assessed" by actual OS so the label doesn't lie.
			// Distinguishing non-RHEL (genuine skip) from RHEL-no-response
			// (mesh / timeout symptom) tells the operator what to investigate.
			nonRHEL, rhelNoResponse := countNotAssessed(assessedHosts)

			fmt.Printf("\n%d vulnerable, %d patched", vulnerable, patched)
			if noFix > 0 {
				fmt.Printf(", %d no fix available", noFix)
			}
			if nonRHEL > 0 {
				fmt.Printf(", %d non-RHEL", nonRHEL)
			}
			if rhelNoResponse > 0 {
				fmt.Printf(", %d RHEL did not respond", rhelNoResponse)
			}
			fmt.Println()

			if vulnerable > 0 || noFix > 0 {
				return fmt.Errorf("vulnerable systems found")
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&timeout, "timeout", 60, "query timeout in seconds")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show timing and query details")
	return cmd
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

// ─────────────────────────────────────────────────────────
// dirq errata
// ─────────────────────────────────────────────────────────
