// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"fmt"
	"os"
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

			fmt.Fprintf(os.Stderr, "Fetching %s from Red Hat Security Data API...\n", cveID)
			stepStart := time.Now()

			ci, err := fetchRedHatCVE(cveID)
			if err != nil {
				return err
			}

			logStep("CVE data fetched in %s", time.Since(stepStart))

			fmt.Fprintf(os.Stderr, "%s: %s\n", cveID, ci.Description)
			fmt.Fprintf(os.Stderr, "Severity: %s\n", ci.Severity)

			if len(ci.FixedPkgs) == 0 {
				fmt.Println("No RHEL packages associated with this CVE.")
				return nil
			}

			fixes := fixesMap(ci.FixedPkgs)
			pkgNames := fixesPkgNames(fixes)

			fmt.Fprintf(os.Stderr, "Packages: %s\n", strings.Join(pkgNames, ", "))
			for _, fp := range ci.FixedPkgs {
				if fp.fixVersion != "" {
					fmt.Fprintf(os.Stderr, "  %s (RHEL %s): fixed in %s\n", fp.name, fp.rhelVer, fp.fullNEVRA)
				} else {
					fmt.Fprintf(os.Stderr, "  %s (RHEL %s): no fix available (still affected)\n", fp.name, fp.rhelVer)
				}
			}
			fmt.Fprintln(os.Stderr)

			queryStr := buildPkgScanQuery(pkgNames, args[1:])
			logStep("Query: %s", queryStr)
			fmt.Fprintf(os.Stderr, "Scanning fleet...\n\n")
			stepStart = time.Now()

			result, raw, err := queryFleetPkgs(queryStr, timeout)
			if err != nil {
				return err
			}

			logStep("Fleet query returned %d results from %d targets in %s",
				result.Received, result.TotalTargets, time.Since(stepStart))

			if jsonOut {
				fmt.Println(string(raw))
				return nil
			}

			outcome := assessFleetScan(os.Stdout, result, fixes, true)
			writeScanSummary(os.Stdout, outcome)

			if outcome.vulnerable > 0 || outcome.noFix > 0 {
				return fmt.Errorf("vulnerable systems found")
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&timeout, "timeout", 60, "query timeout in seconds")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show timing and query details")
	return cmd
}
