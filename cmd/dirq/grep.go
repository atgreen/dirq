// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func grepCmd() *cobra.Command {
	var (
		ignoreCase bool
		tail       int
		become     bool
		timeout    int
	)

	cmd := &cobra.Command{
		Use:   "grep [pattern] [file] [WHERE ...]",
		Short: "Search log files across the fleet",
		Long: `Search for a pattern in a file across multiple hosts in parallel.

Uses grep on Linux and Select-String on Windows. Results are formatted
as a table with hostname, line number, and matching text.

Examples:
  dirq grep "Out of memory" /var/log/messages
  dirq grep -i "error|timeout" /var/log/nginx/error.log WHERE tag.env = 'prod'
  dirq grep "FATAL" /var/log/app.log --tail 1000
  dirq grep "Failed password" /var/log/secure --become`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := args[0]
			filePath := args[1]
			whereArgs := args[2:]
			queryStr := buildWhereQuery(whereArgs)

			// Build the grep command per platform.
			// Linux: grep -n [-i] "pattern" file  (or tail -N file | grep -n [-i] "pattern")
			// Windows: Select-String [-CaseSensitive] -Pattern "pattern" -Path file
			//   (or Get-Content -Tail N file | Select-String ...)
			//
			// We can't know the OS ahead of time in a mixed fleet, so we
			// send a polyglot command that uses shell detection.
			var linuxCmd, winCmd string

			grepFlags := "-n"
			if ignoreCase {
				grepFlags = "-in"
			}

			sqPattern := "'" + strings.ReplaceAll(pattern, "'", "'\\''") + "'"
			sqFile := "'" + strings.ReplaceAll(filePath, "'", "'\\''") + "'"

			if tail > 0 {
				linuxCmd = fmt.Sprintf("tail -%d %s | grep %s %s",
					tail, sqFile, grepFlags, sqPattern)
			} else {
				linuxCmd = fmt.Sprintf("grep %s %s %s",
					grepFlags, sqPattern, sqFile)
			}

			csFlag := " -CaseSensitive"
			if ignoreCase {
				csFlag = ""
			}
			escapedPattern := strings.ReplaceAll(pattern, "'", "''")
			escapedPath := strings.ReplaceAll(filePath, "'", "''")

			if tail > 0 {
				winCmd = fmt.Sprintf("Get-Content -Tail %d '%s' | Select-String%s -Pattern '%s'",
					tail, escapedPath, csFlag, escapedPattern)
			} else {
				winCmd = fmt.Sprintf("Select-String%s -Pattern '%s' -Path '%s'",
					csFlag, escapedPattern, escapedPath)
			}

			commandStr := fmt.Sprintf(
				"if [ -f /etc/os-release ] 2>/dev/null; then %s; else powershell -NoProfile -Command \"%s\"; fi",
				linuxCmd, strings.ReplaceAll(winCmd, "\"", "\\\""))

			payload := map[string]any{
				"query":   queryStr,
				"command": commandStr,
				"become":  become,
				"timeout": timeout,
			}

			body, _ := json.Marshal(payload)
			resp, err := apiStreamRequest("POST", "/api/v1/exec_multi", bytes.NewReader(body))
			if err != nil {
				return err
			}
			defer resp.Body.Close()

			if resp.StatusCode >= 400 {
				data, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
			}

			dec := json.NewDecoder(resp.Body)

			var header struct {
				Type         string `json:"type"`
				TotalTargets int    `json:"total_targets"`
			}
			if err := dec.Decode(&header); err != nil {
				return fmt.Errorf("failed to read response header: %w", err)
			}

			if header.TotalTargets == 0 {
				fmt.Println("No hosts matched the query (or none have exec enabled).")
				return nil
			}

			if jsonOut {
				fmt.Printf("{\"total_targets\":%d}\n", header.TotalTargets)
			}

			// Collect and format results.
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			if !jsonOut {
				fmt.Fprintln(w, "HOST\tLINE\tMATCH")
			}

			totalMatches := 0
			hostsWithMatches := 0
			hostsSearched := 0

			for dec.More() {
				var r struct {
					Type         string `json:"type"`
					Hostname     string `json:"hostname"`
					RC           int    `json:"rc"`
					Stdout       string `json:"stdout"`
					Stderr       string `json:"stderr"`
					Success      bool   `json:"success"`
					Error        string `json:"error"`
					Received     int    `json:"received"`
					TotalTargets int    `json:"total_targets"`
				}
				if err := dec.Decode(&r); err != nil {
					break
				}

				if r.Type == "progress" {
					if !jsonOut {
						fmt.Fprintf(os.Stderr, "\r\033[K%d/%d hosts searched...", r.Received, r.TotalTargets)
					}
					continue
				}

				if !jsonOut {
					fmt.Fprintf(os.Stderr, "\r\033[K")
				}

				hostsSearched++

				if !r.Success || r.RC != 0 {
					// rc=1 means no matches (normal for grep), skip silently.
					// Other errors: log them.
					if r.RC != 1 && r.Error != "" {
						fmt.Fprintf(os.Stderr, "%s: %s\n", r.Hostname, r.Error)
					}
					continue
				}

				stdout, _ := base64.StdEncoding.DecodeString(r.Stdout)
				lines := strings.Split(strings.TrimRight(string(stdout), "\n"), "\n")
				if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
					continue
				}

				hostsWithMatches++
				hostMatches := 0

				for _, line := range lines {
					if line == "" {
						continue
					}
					totalMatches++
					hostMatches++

					if jsonOut {
						jl, _ := json.Marshal(map[string]string{
							"host": r.Hostname,
							"line": line,
						})
						fmt.Println(string(jl))
						continue
					}

					// Parse "linenum:text" from grep -n output.
					lineNum := "-"
					matchText := line
					if idx := strings.IndexByte(line, ':'); idx > 0 {
						if _, err := strconv.Atoi(line[:idx]); err == nil {
							lineNum = line[:idx]
							matchText = line[idx+1:]
						}
					}
					fmt.Fprintf(w, "%s\t%s\t%s\n", r.Hostname, lineNum, matchText)
				}
			}

			if !jsonOut {
				w.Flush()
				if totalMatches > 0 {
					fmt.Println()
				}
				fmt.Printf("%d matches across %d hosts (%d hosts searched)\n",
					totalMatches, hostsWithMatches, hostsSearched)
			}
			return nil
		},
	}

	cmd.Flags().BoolVarP(&ignoreCase, "ignore-case", "i", false, "case-insensitive search")
	cmd.Flags().IntVar(&tail, "tail", 0, "search only the last N lines of the file")
	cmd.Flags().BoolVar(&become, "become", false, "run with privilege escalation (sudo)")
	cmd.Flags().IntVar(&timeout, "timeout", 300, "timeout in seconds")

	return cmd
}

// ─────────────────────────────────────────────────────────
// dirq doctor
// ─────────────────────────────────────────────────────────
