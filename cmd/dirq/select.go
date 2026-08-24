// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func selectCmd() *cobra.Command {
	var timeout int

	cmd := &cobra.Command{
		Use:   "select [fields] [WHERE ...]",
		Short: "Query the fleet",
		Long: `Run a DirQ query with natural syntax.

Quote the query to avoid shell interpretation of >, <, *, (, ):

Examples:
  dirq "select hostname, disk.pct_used where disk.pct_used > 80"
  dirq "select * where (tag.env = 'prod' or tag.env = 'staging')"
  dirq select hostname, cpu.cores WHERE tag.env = 'prod'
  dirq select os_info.os, COUNT(hostname) GROUP BY os_info.os`,
		Args:               cobra.MinimumNArgs(1),
		DisableFlagParsing: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			queryStr := buildSelectQuery(args)

			body, _ := json.Marshal(map[string]any{
				"query":   queryStr,
				"timeout": timeout,
			})

			resp, err := apiRequest("POST", "/api/v1/query", bytes.NewReader(body))
			if err != nil {
				return err
			}

			if jsonOut {
				fmt.Println(string(resp))
				return nil
			}

			var result struct {
				QueryID      string `json:"query_id"`
				Status       string `json:"status"`
				TotalTargets int    `json:"total_targets"`
				Received     int    `json:"received"`
				Missing      int    `json:"missing"`
				Results      []struct {
					Hostname string         `json:"hostname"`
					Success  bool           `json:"success"`
					Error    string         `json:"error"`
					Data     map[string]any `json:"data"`
				} `json:"results"`
			}
			if err := json.Unmarshal(resp, &result); err != nil {
				return err
			}

			fmt.Printf("Query: %s\n", result.QueryID)
			if result.Missing > 0 {
				fmt.Printf("Status: %s | Targets: %d | Received: %d | Missing: %d (mesh timeout or unreachable)\n\n",
					result.Status, result.TotalTargets, result.Received, result.Missing)
			} else {
				fmt.Printf("Status: %s | Targets: %d | Received: %d\n\n",
					result.Status, result.TotalTargets, result.Received)
			}

			if len(result.Results) == 0 {
				fmt.Println("No results.")
				return nil
			}

			// Detect flat projected results (all values are scalars, no nested maps).
			// If flat, render as a table. Otherwise render as indented JSON.
			isFlat := false
			if len(result.Results) > 0 && result.Results[0].Data != nil {
				isFlat = true
				for _, v := range result.Results[0].Data {
					if _, nested := v.(map[string]any); nested {
						isFlat = false
						break
					}
					if _, arr := v.([]any); arr {
						isFlat = false
						break
					}
				}
			}

			if isFlat {
				// Collect column names from first result.
				var columns []string
				for k := range result.Results[0].Data {
					columns = append(columns, k)
				}
				// Stable order: sort, but put hostname first if present.
				sort.Strings(columns)
				for i, c := range columns {
					if c == "hostname" {
						columns = append(columns[:i], columns[i+1:]...)
						columns = append([]string{"hostname"}, columns...)
						break
					}
				}

				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				// Header.
				fmt.Fprintln(w, strings.Join(columns, "\t"))
				// Rows.
				for _, r := range result.Results {
					if !r.Success {
						continue
					}
					vals := make([]string, len(columns))
					for i, col := range columns {
						if v, ok := r.Data[col]; ok {
							vals[i] = fmt.Sprintf("%v", v)
						}
					}
					fmt.Fprintln(w, strings.Join(vals, "\t"))
				}
				w.Flush()
			} else {
				for _, r := range result.Results {
					if r.Success {
						data, _ := json.MarshalIndent(r.Data, "  ", "  ")
						fmt.Printf("  %s:\n  %s\n\n", r.Hostname, string(data))
					} else {
						fmt.Printf("  %s: ERROR: %s\n\n", r.Hostname, r.Error)
					}
				}
			}

			return nil
		},
	}

	cmd.Flags().IntVar(&timeout, "timeout", 60, "query timeout in seconds")
	return cmd
}

// ─────────────────────────────────────────────────────────
// dirq deploy
// ─────────────────────────────────────────────────────────
