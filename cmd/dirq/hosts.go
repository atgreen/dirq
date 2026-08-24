// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func hostsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hosts",
		Short: "Manage hosts",
	}

	listCmd := &cobra.Command{
		Use:   "list [WHERE ...]",
		Short: "List registered hosts",
		Long: `List registered hosts, optionally filtered by a WHERE clause.

Examples:
  dirq hosts list
  dirq hosts list WHERE tag.env = 'prod'
  dirq hosts list WHERE os_info.os = 'linux'`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// If a WHERE clause is provided, query for matching hosts
			// and display those. Otherwise list all hosts from the API.
			if len(args) > 0 {
				queryStr := "SELECT hostname, os_info.os, os_info.os_version, os_info.arch " + strings.Join(args, " ")

				body, _ := json.Marshal(map[string]any{
					"query":   queryStr,
					"timeout": 60,
				})
				resp, err := apiRequest("POST", "/api/v1/query", bytes.NewReader(body))
				if err != nil {
					return err
				}

				var result struct {
					Results []struct {
						Hostname string         `json:"hostname"`
						Success  bool           `json:"success"`
						Data     map[string]any `json:"data"`
					} `json:"results"`
				}
				if err := json.Unmarshal(resp, &result); err != nil {
					return err
				}

				if len(result.Results) == 0 {
					fmt.Println("No hosts matched the query.")
					return nil
				}

				if jsonOut {
					fmt.Println(string(resp))
					return nil
				}

				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "HOSTNAME\tOS\tVERSION\tARCH")
				for _, r := range result.Results {
					if !r.Success {
						continue
					}
					os := dataField(r.Data, "os_info", "os")
					ver := dataField(r.Data, "os_info", "os_version")
					arch := dataField(r.Data, "os_info", "arch")
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Hostname, os, ver, arch)
				}
				w.Flush()
				return nil
			}

			resp, err := apiRequest("GET", "/api/v1/hosts", nil)
			if err != nil {
				return err
			}

			if jsonOut {
				fmt.Println(string(resp))
				return nil
			}

			var hosts []struct {
				ID           string    `json:"id"`
				Hostname     string    `json:"hostname"`
				OS           string    `json:"os"`
				OSVersion    string    `json:"os_version"`
				Arch         string    `json:"arch"`
				AgentVersion string    `json:"agent_version"`
				Role         string    `json:"role"`
				Online       bool      `json:"online"`
				LastSeenAt   time.Time `json:"last_seen_at"`
			}
			if err := json.Unmarshal(resp, &hosts); err != nil {
				return err
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "HOSTNAME\tOS\tVERSION\tARCH\tONLINE\tLAST SEEN")
			for _, h := range hosts {
				status := "yes"
				if !h.Online {
					status = "no"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					h.Hostname, h.OS, h.OSVersion, h.Arch, status,
					h.LastSeenAt.Local().Format("2006-01-02 15:04:05"))
			}
			w.Flush()
			return nil
		},
	}

	showCmd := &cobra.Command{
		Use:   "show [host]",
		Short: "Show details for a host",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := apiRequest("GET", "/api/v1/hosts/"+args[0], nil)
			if err != nil {
				return err
			}
			var out bytes.Buffer
			json.Indent(&out, resp, "", "  ")
			fmt.Println(out.String())
			return nil
		},
	}

	factsCmd := &cobra.Command{
		Use:   "facts [host]",
		Short: "Show cached facts for a host",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := apiRequest("GET", "/api/v1/hosts/"+args[0]+"/facts", nil)
			if err != nil {
				return err
			}
			var out bytes.Buffer
			json.Indent(&out, resp, "", "  ")
			fmt.Println(out.String())
			return nil
		},
	}

	tagCmd := &cobra.Command{
		Use:   "tag [key=value ...] [WHERE ...]",
		Short: "Add or update tags on hosts",
		Long: `Add or update tags on one or more hosts.

Use a WHERE clause to target multiple hosts by query, or pass a host ID
as the first argument to tag a single host.

Examples:
  dirq hosts tag abc-123 env=prod role=webserver
  dirq hosts tag env=prod WHERE os_info.os = 'linux'
  dirq hosts tag role=webserver WHERE tag.dc = 'us-east'`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tags, whereArgs, hostID := parseTagArgs(args)
			if len(tags) == 0 {
				return fmt.Errorf("no key=value tags provided")
			}

			// Determine target host(s).
			var hostIDs []string
			var hostNames []string

			if hostID != "" {
				// Single host by ID.
				hostIDs = []string{hostID}
			} else if len(whereArgs) > 0 {
				// Query for matching hosts.
				queryStr := buildWhereQuery(whereArgs)
				hosts, err := runQuery(queryStr, 60)
				if err != nil {
					return err
				}
				if len(hosts) == 0 {
					fmt.Println("No hosts matched the query.")
					return nil
				}
				for _, h := range hosts {
					hostIDs = append(hostIDs, h.agentID)
					hostNames = append(hostNames, h.hostname)
				}
				fmt.Printf("Tagging %d host(s): %s\n\n", len(hosts), strings.Join(hostNames, ", "))
			} else {
				return fmt.Errorf("provide a host ID or a WHERE clause")
			}

			body, _ := json.Marshal(tags)
			for i, id := range hostIDs {
				resp, err := apiRequest("PATCH", "/api/v1/hosts/"+id+"/tags", bytes.NewReader(body))
				if err != nil {
					fmt.Fprintf(os.Stderr, "  %s: FAILED: %v\n", hostIDOrName(hostNames, i, id), err)
					continue
				}
				var agent struct {
					Hostname string            `json:"hostname"`
					Tags     map[string]string `json:"tags"`
				}
				json.Unmarshal(resp, &agent)
				fmt.Printf("  %s: tags updated\n", agent.Hostname)
			}
			return nil
		},
	}

	untagCmd := &cobra.Command{
		Use:   "untag [key ...] [WHERE ...]",
		Short: "Remove tags from hosts",
		Long: `Remove tags by key from one or more hosts.

Use a WHERE clause to target multiple hosts by query, or pass a host ID
as the first argument to untag a single host.

Examples:
  dirq hosts untag abc-123 env role
  dirq hosts untag env WHERE tag.env = 'staging'
  dirq hosts untag role WHERE os_info.os = 'windows'`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			keys, whereArgs, hostID := parseUntagArgs(args)
			if len(keys) == 0 {
				return fmt.Errorf("no tag keys provided")
			}

			var hostIDs []string
			var hostNames []string

			if hostID != "" {
				hostIDs = []string{hostID}
			} else if len(whereArgs) > 0 {
				queryStr := buildWhereQuery(whereArgs)
				hosts, err := runQuery(queryStr, 60)
				if err != nil {
					return err
				}
				if len(hosts) == 0 {
					fmt.Println("No hosts matched the query.")
					return nil
				}
				for _, h := range hosts {
					hostIDs = append(hostIDs, h.agentID)
					hostNames = append(hostNames, h.hostname)
				}
				fmt.Printf("Untagging %d host(s): %s\n\n", len(hosts), strings.Join(hostNames, ", "))
			} else {
				return fmt.Errorf("provide a host ID or a WHERE clause")
			}

			for i, id := range hostIDs {
				for _, key := range keys {
					_, err := apiRequest("DELETE", "/api/v1/hosts/"+id+"/tags/"+key, nil)
					if err != nil {
						fmt.Fprintf(os.Stderr, "  %s: FAILED to remove %s: %v\n", hostIDOrName(hostNames, i, id), key, err)
						continue
					}
				}
				fmt.Printf("  %s: removed %s\n", hostIDOrName(hostNames, i, id), strings.Join(keys, ", "))
			}
			return nil
		},
	}

	cmd.AddCommand(listCmd, showCmd, factsCmd, tagCmd, untagCmd, graphCmd())
	return cmd
}

// ─────────────────────────────────────────────────────────
// dirq graph
// ─────────────────────────────────────────────────────────
