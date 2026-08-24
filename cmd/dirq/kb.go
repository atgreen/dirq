// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func kbCmd() *cobra.Command {
	var timeout int

	cmd := &cobra.Command{
		Use:   "kb [KB-ID...] [WHERE ...]",
		Short: "Check Windows hosts for installed hotfixes (KBs)",
		Long: `Query Windows hosts to check whether specific KB hotfixes are installed.

Examples:
  dirq kb KB5029263
  dirq kb KB5029263 KB5028166
  dirq kb KB5029263 WHERE tag.env = 'prod'`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Split args into KB IDs and WHERE clause.
			var kbIDs []string
			var whereArgs []string
			for i, a := range args {
				if strings.EqualFold(a, "WHERE") {
					whereArgs = args[i:]
					break
				}
				kbIDs = append(kbIDs, strings.ToUpper(a))
			}

			if len(kbIDs) == 0 {
				return fmt.Errorf("provide at least one KB ID (e.g., KB5029263)")
			}

			// Normalize: ensure KB prefix.
			for i, kb := range kbIDs {
				if !strings.HasPrefix(kb, "KB") {
					kbIDs[i] = "KB" + kb
				}
			}

			// Build query: check hotfixes on Windows hosts.
			inList := make([]string, len(kbIDs))
			for i, kb := range kbIDs {
				inList[i] = "'" + kb + "'"
			}
			kbFilter := "hotfixes.kb_id IN (" + strings.Join(inList, ", ") + ")"
			osFilter := "os_info.os = 'windows'"

			var whereExtra string
			if len(whereArgs) > 0 {
				whereExtra = " AND " + strings.Join(whereArgs[1:], " ") // skip "WHERE"
			}

			queryStr := fmt.Sprintf("SELECT hostname, hotfixes.kb_id, hotfixes.description, hotfixes.installed_on WHERE %s AND %s%s",
				kbFilter, osFilter, whereExtra)

			fmt.Fprintf(os.Stderr, "Checking %s on Windows hosts...\n\n", strings.Join(kbIDs, ", "))

			body, _ := json.Marshal(map[string]any{
				"query":   queryStr,
				"timeout": timeout,
			})
			queryResp, err := apiRequest("POST", "/api/v1/query", bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("query failed: %w", err)
			}

			var result struct {
				TotalTargets int `json:"total_targets"`
				Received     int `json:"received"`
				Results      []struct {
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

			// Collect which KBs each host has installed.
			type hostKB struct {
				hostname    string
				kb          string
				description string
				installedOn string
			}
			var found []hostKB
			hostsWithKB := map[string]map[string]bool{} // hostname → set of installed KB IDs

			for _, r := range result.Results {
				if !r.Success {
					continue
				}
				// Extract hotfix data (may be flattened or nested).
				kbID, _ := r.Data["hotfixes.kb_id"].(string)
				desc, _ := r.Data["hotfixes.description"].(string)
				installed, _ := r.Data["hotfixes.installed_on"].(string)
				if kbID != "" {
					found = append(found, hostKB{r.Hostname, kbID, desc, installed})
					if hostsWithKB[r.Hostname] == nil {
						hostsWithKB[r.Hostname] = map[string]bool{}
					}
					hostsWithKB[r.Hostname][kbID] = true
				}
			}

			// Get all Windows hosts to find those missing the KBs.
			allWindows := map[string]bool{}
			if hostsResp, err := apiRequest("GET", "/api/v1/hosts", nil); err == nil {
				var agents []struct {
					Hostname string `json:"hostname"`
					OS       string `json:"os"`
					Online   bool   `json:"online"`
				}
				if json.Unmarshal(hostsResp, &agents) == nil {
					for _, a := range agents {
						if a.Online && a.OS == "windows" {
							allWindows[a.Hostname] = true
						}
					}
				}
			}

			if len(allWindows) == 0 {
				fmt.Println("No online Windows hosts in the fleet.")
				return nil
			}

			// Print installed KBs.
			if len(found) > 0 {
				for _, h := range found {
					fmt.Printf("  %-24s %-14s %-20s  installed (%s)\n",
						h.hostname, h.kb, h.description, h.installedOn)
				}
			}

			// Print missing KBs.
			missing := 0
			for hostname := range allWindows {
				for _, kb := range kbIDs {
					if hostsWithKB[hostname] == nil || !hostsWithKB[hostname][kb] {
						fmt.Printf("  %-24s %-14s %-20s  MISSING\n",
							hostname, kb, "")
						missing++
					}
				}
			}

			installed := len(found)
			fmt.Printf("\n%d installed, %d missing\n", installed, missing)

			if missing > 0 {
				return fmt.Errorf("missing hotfixes found")
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&timeout, "timeout", 60, "query timeout in seconds")
	return cmd
}
