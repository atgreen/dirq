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
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func execCmd() *cobra.Command {
	var (
		scriptFile   string
		become       bool
		becomeUser   string
		becomeMethod string
		timeout      int
	)

	cmd := &cobra.Command{
		Use:   "exec [WHERE ...] -- [command ...]",
		Short: "Execute a command or script across the fleet in parallel",
		Long: `Run a command or script on multiple agents simultaneously and stream results.

The command comes after -- so it can contain any flags or arguments
without conflicting with dirq's own flags. An optional WHERE clause
before -- filters which agents are targeted.

Use --script to upload and execute a local script file instead.

Script handling by platform:
  Linux:   Shebang (#!) is honored. Scripts are chmod +x and run directly.
  Windows: .ps1 files run with PowerShell. .bat/.cmd run with cmd.exe.

Examples:
  dirq exec -- uptime
  dirq exec WHERE tag.env = 'prod' -- du -h
  dirq exec WHERE os_info.os = 'windows' -- where myprogram.exe
  dirq exec --become WHERE tag.role = 'webserver' -- systemctl restart nginx
  dirq exec WHERE tag.env = 'prod' --script ./health-check.sh
  dirq exec WHERE os_info.os = 'windows' --script ./audit.ps1`,
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Cobra sets ArgsLenAtDash() to the number of args before --.
			// Everything before -- is the WHERE clause, everything after
			// is the remote command (verbatim, no flag conflicts).
			var whereArgs []string
			var commandParts []string
			dashAt := cmd.ArgsLenAtDash()

			if dashAt >= 0 {
				whereArgs = args[:dashAt]
				commandParts = args[dashAt:]
			} else if scriptFile != "" {
				// No --, all args are the WHERE clause.
				whereArgs = args
			} else {
				return fmt.Errorf("usage: dirq exec [WHERE ...] -- <command>\n\nPut the remote command after -- to avoid flag conflicts")
			}

			if scriptFile == "" && len(commandParts) == 0 {
				return fmt.Errorf("provide a command after -- or use --script <file>")
			}

			commandStr := strings.Join(commandParts, " ")
			queryStr := buildWhereQuery(whereArgs)

			payload := map[string]any{
				"query":         queryStr,
				"become":        become,
				"become_user":   becomeUser,
				"become_method": becomeMethod,
				"timeout":       timeout,
			}

			if scriptFile != "" {
				content, err := os.ReadFile(scriptFile)
				if err != nil {
					return fmt.Errorf("read script %s: %w", scriptFile, err)
				}
				payload["script"] = base64.StdEncoding.EncodeToString(content)
				payload["script_name"] = filepath.Base(scriptFile)
				fmt.Fprintf(os.Stderr, "Script: %s (%.1f KB)\n", filepath.Base(scriptFile), float64(len(content))/1024)
			} else {
				payload["command"] = commandStr
			}

			// LLM change review.
			action := reviewAction{
				ActionType:  "exec",
				Command:     commandStr,
				TargetQuery: queryStr,
				Become:      become,
				BecomeUser:  becomeUser,
			}
			if scriptFile != "" {
				content, _ := os.ReadFile(scriptFile)
				action.ScriptName = filepath.Base(scriptFile)
				action.ScriptBody = string(content)
				action.Command = ""
			}
			if err := runReview(action); err != nil {
				return err
			}

			body, _ := json.Marshal(payload)

			// Use raw HTTP so we can stream the NDJSON response.
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

			// First line is the header with target count.
			var header struct {
				Type              string `json:"type"`
				TotalTargets      int    `json:"total_targets"`
				UnresolvedTargets int    `json:"unresolved_targets"`
			}
			if err := dec.Decode(&header); err != nil {
				return fmt.Errorf("failed to read response header: %w", err)
			}

			if header.TotalTargets == 0 {
				fmt.Println("No hosts matched the query (or none have exec enabled).")
				return nil
			}

			if !jsonOut {
				if header.UnresolvedTargets > 0 {
					// The query that resolved field conditions (e.g.
					// os_info.os = 'linux') returned partial results — exec
					// is only running against the subset that answered.
					fmt.Printf("Targets: %d  (warning: %d host(s) did not respond to the field-resolution query; exec coverage is partial)\n\n",
						header.TotalTargets, header.UnresolvedTargets)
				} else {
					fmt.Printf("Targets: %d\n\n", header.TotalTargets)
				}
			}

			// Stream results as they arrive.
			hasFailures := false
			received := 0

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
					return fmt.Errorf("failed to read result: %w", err)
				}

				if r.Type == "progress" {
					if !jsonOut {
						fmt.Fprintf(os.Stderr, "\r\033[K%d/%d hosts responded...", r.Received, r.TotalTargets)
					}
					continue
				}

				// Clear progress line before printing result.
				if !jsonOut {
					fmt.Fprintf(os.Stderr, "\r\033[K")
				}

				received++

				if jsonOut {
					line, _ := json.Marshal(r)
					fmt.Println(string(line))
					continue
				}

				if !r.Success {
					fmt.Printf("── %s  ERROR ──\n", r.Hostname)
					fmt.Printf("  %s\n\n", r.Error)
					hasFailures = true
					continue
				}

				// Decode base64-encoded stdout/stderr.
				stdout, _ := base64.StdEncoding.DecodeString(r.Stdout)
				stderr, _ := base64.StdEncoding.DecodeString(r.Stderr)

				fmt.Printf("── %s  rc=%d ──\n", r.Hostname, r.RC)
				if len(stdout) > 0 {
					for _, line := range strings.Split(strings.TrimRight(string(stdout), "\n"), "\n") {
						fmt.Printf("  %s\n", line)
					}
				}
				if len(stderr) > 0 {
					fmt.Printf("  (stderr) %s\n", strings.TrimRight(string(stderr), "\n"))
				}
				fmt.Println()

				if r.RC != 0 {
					hasFailures = true
				}
			}

			if !jsonOut {
				if received < header.TotalTargets {
					fmt.Printf("%d/%d responded; %d host(s) did not reply (mesh timeout or unreachable)\n",
						received, header.TotalTargets, header.TotalTargets-received)
				} else {
					fmt.Printf("%d/%d completed\n", received, header.TotalTargets)
				}
				if header.UnresolvedTargets > 0 {
					fmt.Printf("Plus %d host(s) excluded from the broadcast because field-resolution didn't get a response — actual coverage is partial.\n",
						header.UnresolvedTargets)
				}
			}

			if hasFailures {
				return fmt.Errorf("one or more hosts returned errors")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&scriptFile, "script", "", "local script file to upload and execute")
	cmd.Flags().BoolVar(&become, "become", false, "run with privilege escalation (sudo)")
	cmd.Flags().StringVar(&becomeUser, "become-user", "", "user to become (default: root)")
	cmd.Flags().StringVar(&becomeMethod, "become-method", "", "privilege escalation method (default: sudo)")
	cmd.Flags().IntVar(&timeout, "timeout", 300, "timeout in seconds")

	return cmd
}

// ─────────────────────────────────────────────────────────
// dirq skill
// ─────────────────────────────────────────────────────────
