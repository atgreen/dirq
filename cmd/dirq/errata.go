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

	"github.com/spf13/cobra"
)

func errataCmd() *cobra.Command {
	var timeout int

	cmd := &cobra.Command{
		Use:   "errata [RHSA-ID] [WHERE ...]",
		Short: "Check RHEL hosts against a Red Hat advisory (RHSA/RHBA/RHEA)",
		Long: `Look up a Red Hat advisory and check the fleet for hosts that
are missing the patched packages.

Examples:
  dirq errata RHSA-2026:13578
  dirq errata RHSA-2026:13578 WHERE tag.env = 'prod'
  dirq errata RHBA-2026:1234`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			advisoryID := strings.ToUpper(args[0])
			if !strings.HasPrefix(advisoryID, "RHSA-") &&
				!strings.HasPrefix(advisoryID, "RHBA-") &&
				!strings.HasPrefix(advisoryID, "RHEA-") {
				return fmt.Errorf("expected an advisory ID like RHSA-2026:1234, got %q", advisoryID)
			}

			// Fetch advisory data from Red Hat.
			fmt.Fprintf(os.Stderr, "Fetching %s from Red Hat Security Data API...\n", advisoryID)

			apiURL := "https://access.redhat.com/hydra/rest/securitydata/cve.json?advisory=" + advisoryID
			resp, err := http.Get(apiURL)
			if err != nil {
				return fmt.Errorf("failed to fetch advisory data: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode == 404 {
				return fmt.Errorf("advisory %s not found", advisoryID)
			}
			if resp.StatusCode != 200 {
				return fmt.Errorf("Red Hat API returned HTTP %d", resp.StatusCode)
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("read response: %w", err)
			}

			var cveEntries []struct {
				CVE              string   `json:"CVE"`
				Severity         string   `json:"severity"`
				BugzillaDesc     string   `json:"bugzilla_description"`
				AffectedPackages []string `json:"affected_packages"`
			}
			if err := json.Unmarshal(body, &cveEntries); err != nil {
				return fmt.Errorf("parse advisory data: %w", err)
			}

			if len(cveEntries) == 0 {
				fmt.Println("No CVEs found for this advisory.")
				return nil
			}

			fmt.Fprintf(os.Stderr, "Advisory %s covers %d CVE(s):\n", advisoryID, len(cveEntries))
			for _, cve := range cveEntries {
				fmt.Fprintf(os.Stderr, "  %s (%s): %s\n", cve.CVE, cve.Severity, cve.BugzillaDesc)
			}

			// Collect all fixed packages across all CVEs, keyed by RHEL version.
			type fixKey struct{ name, rhelVer string }
			type fixedPkg struct {
				name       string
				fullNEVRA  string
				fixVersion string
				rhelVer    string
			}
			fixedVersionMap := map[fixKey]fixedPkg{}
			allPkgNames := map[string]bool{}

			for _, cve := range cveEntries {
				for _, pkg := range cve.AffectedPackages {
					name, version := parseRPMNEVRA(pkg)
					if name == "" {
						continue
					}
					if strings.HasPrefix(name, "kpatch") {
						continue
					}
					rhelVer := detectRHELMajor(version)
					if rhelVer == "" {
						continue
					}
					key := fixKey{name, rhelVer}
					// Keep the highest fix version per package+RHEL combo.
					if existing, ok := fixedVersionMap[key]; ok {
						if rpmVersionCompare(version, existing.fixVersion) <= 0 {
							continue
						}
					}
					allPkgNames[name] = true
					fixedVersionMap[key] = fixedPkg{
						name:       name,
						fullNEVRA:  pkg,
						fixVersion: version,
						rhelVer:    rhelVer,
					}
				}
			}

			if len(allPkgNames) == 0 {
				fmt.Println("No RHEL packages found in this advisory.")
				return nil
			}

			pkgNames := make([]string, 0, len(allPkgNames))
			for n := range allPkgNames {
				pkgNames = append(pkgNames, n)
			}
			sort.Strings(pkgNames)

			fmt.Fprintf(os.Stderr, "\nPackages: %s\n", strings.Join(pkgNames, ", "))

			// Build query.
			inList := make([]string, len(pkgNames))
			for i, n := range pkgNames {
				inList[i] = "'" + n + "'"
			}
			pkgFilter := "packages.name IN (" + strings.Join(inList, ", ") + ")"
			osFilter := "os_info.distro_family = 'rhel'"

			var whereExtra string
			if len(args) > 1 {
				whereExtra = " AND " + strings.Join(args[1:], " ")
				whereExtra = strings.Replace(whereExtra, " AND WHERE ", " AND ", 1)
				whereExtra = strings.Replace(whereExtra, " AND where ", " AND ", 1)
			}

			queryStr := fmt.Sprintf("SELECT hostname, os_info.distro_version, os_info.kernel_version, packages.name, packages.version WHERE %s AND %s%s",
				pkgFilter, osFilter, whereExtra)

			fmt.Fprintf(os.Stderr, "Scanning fleet...\n\n")

			qBody, _ := json.Marshal(map[string]any{
				"query":   queryStr,
				"timeout": timeout,
			})
			queryResp, err := apiRequest("POST", "/api/v1/query", bytes.NewReader(qBody))
			if err != nil {
				return fmt.Errorf("fleet query failed: %w", err)
			}

			var result struct {
				Results []struct {
					Hostname string         `json:"hostname"`
					Success  bool           `json:"success"`
					Data     map[string]any `json:"data"`
				} `json:"results"`
			}
			if err := json.Unmarshal(queryResp, &result); err != nil {
				return fmt.Errorf("parse result: %w", err)
			}

			if jsonOut {
				fmt.Println(string(queryResp))
				return nil
			}

			vulnerable := 0
			patched := 0
			assessedHosts := map[string]bool{}
			kernelHandled := map[string]bool{}

			for _, r := range result.Results {
				if !r.Success {
					continue
				}

				distroVer, _ := r.Data["os_info.distro_version"].(string)
				if distroVer == "" {
					if oi, ok := r.Data["os_info"].(map[string]any); ok {
						distroVer, _ = oi["distro_version"].(string)
					}
				}
				hostRHEL := detectRHELMajor(distroVer)
				if hostRHEL == "" {
					continue
				}
				assessedHosts[r.Hostname] = true

				runningKernel, _ := r.Data["os_info.kernel_version"].(string)
				if runningKernel == "" {
					if oi, ok := r.Data["os_info"].(map[string]any); ok {
						runningKernel, _ = oi["kernel_version"].(string)
					}
				}

				pkgs := extractPackageList(r.Data)
				for _, pkg := range pkgs {
					isKernelPkg := pkg.name == "kernel" || pkg.name == "kernel-rt"
					if isKernelPkg {
						dedupKey := r.Hostname + ":" + pkg.name
						if kernelHandled[dedupKey] {
							continue
						}
						kernelHandled[dedupKey] = true
						if runningKernel != "" {
							pkg.version = runningKernel
						}
					}

					fp, hasfix := fixedVersionMap[fixKey{pkg.name, hostRHEL}]
					if !hasfix {
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

			// Split "not assessed" so the label doesn't conflate non-RHEL
			// hosts with RHEL hosts that simply didn't respond.
			nonRHEL, rhelNoResponse := countNotAssessed(assessedHosts)

			fmt.Printf("\n%d vulnerable, %d patched", vulnerable, patched)
			if nonRHEL > 0 {
				fmt.Printf(", %d non-RHEL", nonRHEL)
			}
			if rhelNoResponse > 0 {
				fmt.Printf(", %d RHEL did not respond", rhelNoResponse)
			}
			fmt.Println()

			if vulnerable > 0 {
				return fmt.Errorf("vulnerable systems found")
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&timeout, "timeout", 60, "query timeout in seconds")
	return cmd
}

// ─────────────────────────────────────────────────────────
// dirq kb
// ─────────────────────────────────────────────────────────
