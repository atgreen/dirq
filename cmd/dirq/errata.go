// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"fmt"
	"os"
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

			fmt.Fprintf(os.Stderr, "Fetching %s from Red Hat Security Data API...\n", advisoryID)

			cves, fixes, err := fetchRedHatAdvisory(advisoryID)
			if err != nil {
				return err
			}

			if len(cves) == 0 {
				fmt.Println("No CVEs found for this advisory.")
				return nil
			}

			fmt.Fprintf(os.Stderr, "Advisory %s covers %d CVE(s):\n", advisoryID, len(cves))
			for _, cve := range cves {
				fmt.Fprintf(os.Stderr, "  %s (%s): %s\n", cve.ID, cve.Severity, cve.Description)
			}

			if len(fixes) == 0 {
				fmt.Println("No RHEL packages found in this advisory.")
				return nil
			}

			pkgNames := fixesPkgNames(fixes)
			fmt.Fprintf(os.Stderr, "\nPackages: %s\n", strings.Join(pkgNames, ", "))

			queryStr := buildPkgScanQuery(pkgNames, args[1:])
			fmt.Fprintf(os.Stderr, "Scanning fleet...\n\n")

			result, raw, err := queryFleetPkgs(queryStr, timeout)
			if err != nil {
				return err
			}

			if jsonOut {
				fmt.Println(string(raw))
				return nil
			}

			outcome := assessFleetScan(os.Stdout, result, fixes, false)
			writeScanSummary(os.Stdout, outcome)

			if outcome.vulnerable > 0 {
				return fmt.Errorf("vulnerable systems found")
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&timeout, "timeout", 60, "query timeout in seconds")
	return cmd
}
