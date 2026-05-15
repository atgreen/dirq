// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/atgreen/dirq/internal/tlsutil"
	"github.com/spf13/cobra"
)

var (
	serverURL   string
	apiToken    string
	jsonOut     bool
	tlsInsecure bool
)

func main() {
	// Flatten args: split any multi-word quoted arguments by whitespace.
	// This lets users write dirq "select hostname where tag.env = 'prod'"
	// instead of dirq select hostname where tag.env = 'prod'
	flatArgs := []string{}
	for _, arg := range os.Args[1:] {
		if strings.ContainsAny(arg, " \t") && !strings.HasPrefix(arg, "-") {
			flatArgs = append(flatArgs, strings.Fields(arg)...)
		} else {
			flatArgs = append(flatArgs, arg)
		}
	}
	os.Args = append([]string{os.Args[0]}, flatArgs...)

	root := &cobra.Command{
		Use:   "dirq",
		Short: "DirQ — Real-Time Endpoint Query CLI",
	}

	root.PersistentFlags().StringVar(&serverURL, "server", os.Getenv("DIRQ_SERVER_URL"), "DirQ server URL (or set DIRQ_SERVER_URL)")
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// Allow tls generate, skill, and ask --dry-run to run without a server URL.
		if cmd.Name() == "generate" || cmd.Name() == "skill" || cmd.Name() == "doctor" {
			return nil
		}
		if cmd.Name() == "ask" && serverURL == "" {
			return nil // ask can run with --dry-run without a server
		}
		if serverURL == "" {
			return fmt.Errorf("DIRQ_SERVER_URL is not set. Use --server or export DIRQ_SERVER_URL=http://your-dirq-server:8080")
		}
		return nil
	}
	root.PersistentFlags().StringVar(&apiToken, "token", os.Getenv("DIRQ_TOKEN"), "API token")
	root.PersistentFlags().BoolVar(&jsonOut, "json", false, "output raw JSON")
	root.PersistentFlags().BoolVar(&tlsInsecure, "tls-insecure", os.Getenv("DIRQ_TLS_INSECURE") == "true", "skip TLS certificate verification")

	root.AddCommand(hostsCmd())
	root.AddCommand(tokenCmd())
	root.AddCommand(queriesCmd())
	root.AddCommand(tlsCmd())
	root.AddCommand(runCmd())
	root.AddCommand(execCmd())
	root.AddCommand(skillCmd())
	root.AddCommand(askCmd())
	root.AddCommand(selectCmd())
	root.AddCommand(deployCmd())
	root.AddCommand(doctorCmd())
	root.AddCommand(cveCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// ─────────────────────────────────────────────────────────
// dirq hosts
// ─────────────────────────────────────────────────────────

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
			fmt.Fprintln(w, "HOSTNAME\tOS\tVERSION\tARCH\tROLE\tONLINE\tLAST SEEN")
			for _, h := range hosts {
				status := "yes"
				if !h.Online {
					status = "no"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					h.Hostname, h.OS, h.OSVersion, h.Arch, h.Role, status,
					h.LastSeenAt.Format("2006-01-02 15:04:05"))
			}
			w.Flush()
			return nil
		},
	}

	showCmd := &cobra.Command{
		Use:   "show [id]",
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
		Use:   "facts [id]",
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

	cmd.AddCommand(listCmd, showCmd, factsCmd, tagCmd, untagCmd)
	return cmd
}

// ─────────────────────────────────────────────────────────
// dirq token
// ─────────────────────────────────────────────────────────

func tokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage API tokens",
	}

	var scope string
	createCmd := &cobra.Command{
		Use:   "create [name]",
		Short: "Create a new API token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, _ := json.Marshal(map[string]string{
				"name":  args[0],
				"scope": scope,
			})
			resp, err := apiRequest("POST", "/api/v1/tokens", bytes.NewReader(body))
			if err != nil {
				return err
			}
			var result struct {
				Name  string `json:"name"`
				Token string `json:"token"`
				Scope string `json:"scope"`
			}
			json.Unmarshal(resp, &result)
			fmt.Printf("Token created:\n  Name:  %s\n  Scope: %s\n  Token: %s\n\nSave this token — it cannot be retrieved later.\n", result.Name, result.Scope, result.Token)
			return nil
		},
	}
	createCmd.Flags().StringVar(&scope, "scope", "admin", "token scope (admin or readonly)")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List API tokens",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := apiRequest("GET", "/api/v1/tokens", nil)
			if err != nil {
				return err
			}
			if jsonOut {
				fmt.Println(string(resp))
				return nil
			}
			var tokens []struct {
				Name      string    `json:"name"`
				Scope     string    `json:"scope"`
				CreatedAt time.Time `json:"created_at"`
			}
			json.Unmarshal(resp, &tokens)
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSCOPE\tCREATED")
			for _, t := range tokens {
				fmt.Fprintf(w, "%s\t%s\t%s\n", t.Name, t.Scope, t.CreatedAt.Format("2006-01-02 15:04:05"))
			}
			w.Flush()
			return nil
		},
	}

	deleteCmd := &cobra.Command{
		Use:   "delete [name]",
		Short: "Delete an API token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := apiRequest("DELETE", "/api/v1/tokens/"+args[0], nil)
			if err != nil {
				return err
			}
			fmt.Printf("Token '%s' deleted.\n", args[0])
			return nil
		},
	}

	cmd.AddCommand(createCmd, listCmd, deleteCmd)
	return cmd
}

// ─────────────────────────────────────────────────────────
// dirq queries
// ─────────────────────────────────────────────────────────

func queriesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "queries",
		Short: "List recent queries",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := apiRequest("GET", "/api/v1/queries", nil)
			if err != nil {
				return err
			}
			if jsonOut {
				fmt.Println(string(resp))
				return nil
			}
			var queries []struct {
				ID           string    `json:"id"`
				RawQuery     string    `json:"raw_query"`
				Status       string    `json:"status"`
				TargetCount  int       `json:"target_count"`
				SuccessCount int       `json:"success_count"`
				SubmittedAt  time.Time `json:"submitted_at"`
			}
			json.Unmarshal(resp, &queries)
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tSTATUS\tTARGETS\tSUCCESS\tQUERY\tSUBMITTED")
			for _, q := range queries {
				query := q.RawQuery
				if len(query) > 60 {
					query = query[:57] + "..."
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\n",
					q.ID[:8], q.Status, q.TargetCount, q.SuccessCount,
					query, q.SubmittedAt.Format("15:04:05"))
			}
			w.Flush()
			return nil
		},
	}
}

// ─────────────────────────────────────────────────────────
// dirq tls
// ─────────────────────────────────────────────────────────

func tlsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tls",
		Short: "TLS certificate management",
	}

	var dir string
	generateCmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate self-signed CA, server, and agent certificates",
		Long: `Generate a self-signed CA and use it to sign server and agent certificates.

All files are written to the specified directory:
  ca.crt, ca.key       — Certificate Authority
  server.crt, server.key — DirQ server
  agent.crt, agent.key   — DirQ agent

Usage:
  DIRQ_TLS_CA=./certs/ca.crt DIRQ_TLS_CERT=./certs/server.crt DIRQ_TLS_KEY=./certs/server.key dirq-server
  DIRQ_TLS_CA=./certs/ca.crt DIRQ_TLS_CERT=./certs/agent.crt DIRQ_TLS_KEY=./certs/agent.key dirq-agent

For self-signed certs without distributing the CA, agents can use:
  DIRQ_TLS_INSECURE=true dirq-agent`,
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := tlsutil.GenerateSelfSigned(dir)
			if err != nil {
				return err
			}
			fmt.Println("Certificates generated:")
			fmt.Printf("  CA:     %s (key: %s)\n", result.CAFile, result.CAKeyFile)
			fmt.Printf("  Server: %s (key: %s)\n", result.ServerCertFile, result.ServerKeyFile)
			fmt.Printf("  Agent:  %s (key: %s)\n", result.AgentCertFile, result.AgentKeyFile)
			fmt.Println()
			fmt.Println("Server:")
			fmt.Printf("  DIRQ_TLS_CA=%s DIRQ_TLS_CERT=%s DIRQ_TLS_KEY=%s\n",
				result.CAFile, result.ServerCertFile, result.ServerKeyFile)
			fmt.Println()
			fmt.Println("Agent:")
			fmt.Printf("  DIRQ_TLS_CA=%s DIRQ_TLS_CERT=%s DIRQ_TLS_KEY=%s\n",
				result.CAFile, result.AgentCertFile, result.AgentKeyFile)
			return nil
		},
	}

	generateCmd.Flags().StringVar(&dir, "dir", "./certs", "output directory for certificates")
	cmd.AddCommand(generateCmd)
	return cmd
}

// ─────────────────────────────────────────────────────────
// dirq run
// ─────────────────────────────────────────────────────────

func runCmd() *cobra.Command {
	var (
		module     string
		moduleArgs string
		command    string
		forks      int
		extraArgs  []string
	)

	cmd := &cobra.Command{
		Use:   "run [playbook.yml] [WHERE ...]",
		Short: "Run an Ansible playbook or command against the fleet",
		Long: `Query the fleet and run Ansible against matching hosts.

The first argument is a playbook file (or use --module/--command).
An optional WHERE clause filters which agents are targeted.
Without WHERE, all online agents are targeted.

Examples:
  dirq run deploy.yml WHERE tag.env = 'prod'
  dirq run cleanup.yml
  dirq run --command "yum update -y openssl" WHERE packages.name = 'openssl'
  dirq run --module ping WHERE os_info.os = 'linux'
  dirq "run deploy.yml where tag.env = 'prod'"`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Determine playbook from first arg (if it's a file, not a WHERE keyword).
			var playbook string
			var whereArgs []string

			if len(args) > 0 && !strings.EqualFold(args[0], "WHERE") {
				playbook = args[0]
				whereArgs = args[1:]
			} else {
				whereArgs = args
			}

			if playbook == "" && module == "" && command == "" {
				return fmt.Errorf("provide a playbook file as the first argument, or use --module or --command")
			}

			queryStr := buildWhereQuery(whereArgs)

			hosts, err := runQuery(queryStr, 60)
			if err != nil {
				return err
			}

			if len(hosts) == 0 {
				fmt.Println("No hosts matched the query.")
				return nil
			}

			names := make([]string, len(hosts))
			for i, h := range hosts {
				names[i] = h.hostname
			}
			fmt.Printf("Query matched %d host(s): %s\n\n", len(hosts), strings.Join(names, ", "))

			invPath, err := writeInventory(hosts)
			if err != nil {
				return err
			}
			defer os.Remove(invPath)

			// Build the ansible command.
			var ansibleCmd []string

			if playbook != "" {
				ansibleCmd = []string{"ansible-playbook", "-i", invPath, playbook}
			} else if module != "" {
				ansibleCmd = []string{"ansible", "all", "-i", invPath, "-m", module}
				if moduleArgs != "" {
					ansibleCmd = append(ansibleCmd, "-a", moduleArgs)
				}
			} else {
				ansibleCmd = []string{"ansible", "all", "-i", invPath, "-m", "raw", "-a", command}
			}

			if forks > 0 {
				ansibleCmd = append(ansibleCmd, "-f", fmt.Sprintf("%d", forks))
			}

			ansibleCmd = append(ansibleCmd, extraArgs...)

			fmt.Printf("Running: %s\n\n", strings.Join(ansibleCmd, " "))

			proc := exec.Command(ansibleCmd[0], ansibleCmd[1:]...)
			proc.Stdout = os.Stdout
			proc.Stderr = os.Stderr
			proc.Stdin = os.Stdin
			proc.Env = os.Environ()

			if pluginDir := connectionPluginDir(); pluginDir != "" {
				proc.Env = append(proc.Env, "ANSIBLE_CONNECTION_PLUGINS="+pluginDir)
			}

			return proc.Run()
		},
	}

	cmd.Flags().StringVar(&module, "module", "", "Ansible module to run")
	cmd.Flags().StringVar(&moduleArgs, "module-args", "", "Arguments for the module")
	cmd.Flags().StringVar(&command, "command", "", "Ad-hoc command to run (uses raw module)")
	cmd.Flags().IntVar(&forks, "forks", 0, "Number of parallel processes (default: Ansible default)")
	cmd.Flags().StringArrayVar(&extraArgs, "extra", nil, "Extra arguments passed to ansible/ansible-playbook")

	return cmd
}

// ─────────────────────────────────────────────────────────
// dirq exec
// ─────────────────────────────────────────────────────────

func execCmd() *cobra.Command {
	var (
		scriptFile   string
		become       bool
		becomeUser   string
		becomeMethod string
		timeout      int
	)

	cmd := &cobra.Command{
		Use:   "exec [command] [WHERE ...]",
		Short: "Execute a command or script across the fleet in parallel",
		Long: `Run a command or script on multiple agents simultaneously and stream results.

The first argument is the command to execute on each target agent.
Use --script to upload and execute a local script file instead.

Commands with dashes must be quoted so the shell doesn't interpret
them as flags. Simple commands work unquoted.

Script handling by platform:
  Linux:   Shebang (#!) is honored. Scripts are chmod +x and run directly.
  Windows: .ps1 files run with PowerShell. .bat/.cmd run with cmd.exe.

An optional WHERE clause filters which agents are targeted.

Examples:
  dirq exec uptime
  dirq exec "du -h" WHERE tag.env = 'prod'
  dirq exec "df -h /"
  dirq exec --become "systemctl status nginx"
  dirq exec --script ./health-check.sh WHERE tag.env = 'prod'
  dirq exec --script ./audit.ps1 WHERE os_info.os = 'windows'`,
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if scriptFile == "" && len(args) == 0 {
				return fmt.Errorf("provide a command string or use --script <file>")
			}

			// Split args at WHERE — everything before is the command,
			// everything from WHERE onward is the query filter.
			// This allows unquoted commands: dirq exec du -h WHERE ...
			var commandParts []string
			var whereArgs []string
			if scriptFile == "" {
				for i, a := range args {
					if strings.EqualFold(a, "WHERE") {
						whereArgs = args[i:]
						break
					}
					commandParts = append(commandParts, a)
				}
			} else {
				whereArgs = args
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

			if !jsonOut {
				fmt.Printf("Targets: %d\n\n", header.TotalTargets)
			}

			// Stream results as they arrive.
			hasFailures := false
			received := 0

			for dec.More() {
				var r struct {
					Type     string `json:"type"`
					Hostname string `json:"hostname"`
					RC       int    `json:"rc"`
					Stdout   string `json:"stdout"`
					Stderr   string `json:"stderr"`
					Success  bool   `json:"success"`
					Error    string `json:"error"`
				}
				if err := dec.Decode(&r); err != nil {
					return fmt.Errorf("failed to read result: %w", err)
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
				fmt.Printf("%d/%d completed\n", received, header.TotalTargets)
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

func skillCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "skill",
		Short: "Print an AI-readable reference for the DirQ query language",
		Long:  "Outputs a concise prompt that teaches an AI assistant how to use DirQ. Pipe it into your AI tool's context.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Print(skillText)
			return nil
		},
	}
}

const skillText = `# DirQ — Fleet Query & Management Tool

DirQ queries a fleet of agents using a SQL-like DSL. Every agent runs a
lightweight daemon that collects system data (CPU, memory, disk, packages,
services, network) and reports through a relay mesh to a central server.

## Query syntax

    SELECT <fields | *>
    [WHERE <expression>]
    [GROUP BY <field>, ...]
    [ORDER BY <field> [ASC|DESC], ...]
    [LIMIT <n>]

Keywords are case-insensitive. Only SELECT is required.

## Fields

Dotted notation: module.field. Available modules and their fields:

  cpu          — physical_cores, logical_cores, model_name, mhz
  memory       — total_bytes, available_bytes, used_bytes, pct_used
  disk         — (array of partitions) device, mount_point, fs_type,
                 total_bytes, used_bytes, free_bytes, pct_used
  os_info      — hostname, os, os_version, kernel_version, arch, uptime_seconds
  packages     — (array) name, version, arch, source
  services     — (array) name, display_name, state, start_type
  network      — (array of interfaces) name, mac, mtu, flags, addresses

Top-level fields (no module prefix): hostname, os, arch, role, online.

Array modules (disk, packages, services, network): WHERE conditions filter
the array elements — only matching entries are returned.

## Tag targeting

Agent tags are available as tag.* fields:

    WHERE tag.env = 'prod'
    WHERE tag.group = 'webservers'
    WHERE tag.env IN ('prod', 'staging')

Tag conditions are evaluated server-side before dispatching. Only matching
agents receive the query.

## WHERE operators

  =  !=  >  <  >=  <=
  LIKE / NOT LIKE     — % matches any chars, _ matches one char
  IN / NOT IN         — field IN ('a', 'b', 'c')
  IS NULL / IS NOT NULL

Combine with AND, OR, NOT, and parentheses. AND binds tighter than OR.

## Aggregation

  COUNT(field)  SUM(field)  AVG(field)  MIN(field)  MAX(field)

Used with GROUP BY for fleet-wide summaries.

## CLI commands

Shell characters like >, <, *, (, and ) will be interpreted by your
shell if left unquoted. The safest approach is to quote the entire
command and let dirq parse it:

    dirq "select hostname, disk.pct_used where disk.pct_used > 80"
    dirq "select * where (tag.env = 'prod' or tag.env = 'staging') and disk.pct_used > 90"
    dirq "run deploy.yml where tag.env = 'prod'"
    dirq "deploy ./patch.rpm where tag.env = 'prod'"

DirQ splits quoted args by whitespace internally, so this works
identically to typing each word as a separate argument.

### dirq select — query the fleet

    dirq "select hostname, disk.pct_used where disk.pct_used > 80"
    dirq "select * --json"
    dirq select hostname, cpu.cores WHERE tag.env = 'prod'

### dirq exec — execute a command or script across the fleet in parallel

    dirq exec "uptime" WHERE tag.env = 'prod'
    dirq exec "openssl version" WHERE packages.name = 'openssl'
    dirq exec --become "systemctl status nginx" WHERE tag.role = 'webserver'
    dirq exec --script ./health-check.sh WHERE tag.env = 'prod'
    dirq exec --script ./audit.ps1 WHERE os_info.os = 'windows'
    dirq exec "df -h /" --json

Fan-out exec: runs the command or script on all matching agents
simultaneously, streaming results back in real time.

Use --script to upload and execute a local script file. Without
--script, the argument is a command string run on the remote hosts.
Linux scripts use their shebang (#!) line. Windows scripts run as
PowerShell (.ps1) or cmd (.bat/.cmd).

### dirq run — run Ansible against matching hosts

    dirq run deploy.yml WHERE tag.env = 'prod'
    dirq run cleanup.yml
    dirq run --command "systemctl restart nginx" WHERE tag.env = 'prod'
    dirq run --module ping WHERE os_info.os = 'linux'

### dirq deploy — deploy packages through the mesh

    dirq deploy ./patch.rpm WHERE tag.env = 'prod'
    dirq deploy ./agent-0.3.0.msi WHERE os_info.os = 'windows'
    dirq deploy ./fix.deb

Broadcast through the mesh — each link carries the package once. Supports .rpm, .deb, .msi.

### dirq ask — natural language queries (requires LLM API key)

    dirq ask "which prod hosts have full disks?"
    dirq ask "how many hosts are running linux?" --dry-run

### dirq cve — scan RHEL systems for CVE vulnerabilities

    dirq cve CVE-2024-6345
    dirq "cve CVE-2024-6345 where tag.env = 'prod'"

Fetches affected packages from Red Hat Security Data API, queries the
fleet for installed versions, and reports which hosts are vulnerable.

### dirq doctor — check deployment health

    dirq doctor

### dirq hosts — manage hosts and tags

    dirq hosts list
    dirq hosts show <agent-id>
    dirq hosts facts <agent-id>
    dirq hosts tag <agent-id> env=prod role=webserver
    dirq hosts untag <agent-id> env

### dirq token — manage API tokens

    dirq token create ops-team --scope admin
    dirq token create monitoring --scope readonly
    dirq token list
    dirq token delete <name>

### dirq skill — print this reference for LLM context

    dirq skill

## Example queries

    -- Full disks in production
    SELECT hostname, disk.mount_point, disk.pct_used
    WHERE tag.env = 'prod' AND disk.pct_used > 80
    ORDER BY disk.pct_used DESC

    -- Hosts running Linux or FreeBSD with many cores
    SELECT hostname, os_info.os, cpu.logical_cores
    WHERE (os_info.os = 'linux' OR os_info.os = 'freebsd') AND cpu.logical_cores >= 16

    -- Check for vulnerable packages
    SELECT hostname, packages.name, packages.version
    WHERE packages.name = 'openssl' AND packages.version LIKE '1.%'

    -- Count hosts by OS
    SELECT os_info.os, COUNT(hostname) GROUP BY os_info.os

    -- Find stopped services
    SELECT hostname, services.name, services.state
    WHERE services.name = 'sshd' AND services.state = 'stopped'

    -- Hosts missing a tag
    SELECT hostname WHERE tag.env IS NULL

    -- Everything about all hosts
    SELECT *
`

// ─────────────────────────────────────────────────────────
// dirq ask
// ─────────────────────────────────────────────────────────

func askCmd() *cobra.Command {
	var dryRun bool
	var timeout int
	var model string
	var provider string

	cmd := &cobra.Command{
		Use:   "ask [natural language question]",
		Short: "Ask a question in plain English and query the fleet",
		Long: `Translates a natural language question into a DirQ query using an LLM,
then executes it against the fleet.

Requires ANTHROPIC_API_KEY or OPENAI_API_KEY to be set.

Examples:
  dirq ask "which prod hosts have full disks?"
  dirq ask "show me all windows servers"
  dirq ask "how many hosts are running linux?" --dry-run
  dirq ask "find hosts with openssl installed" --provider openai`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			question := args[0]

			// Auto-detect provider from available API keys.
			if provider == "" {
				if os.Getenv("ANTHROPIC_API_KEY") != "" {
					provider = "anthropic"
				} else if os.Getenv("OPENAI_API_KEY") != "" {
					provider = "openai"
				} else {
					return fmt.Errorf("set ANTHROPIC_API_KEY or OPENAI_API_KEY, or use --provider")
				}
			}

			// Call LLM to translate question to DirQ query.
			query, err := llmTranslate(provider, model, question)
			if err != nil {
				return fmt.Errorf("LLM translation failed: %w", err)
			}

			fmt.Fprintf(os.Stderr, "Query: %s\n\n", query)

			if dryRun {
				return nil
			}

			if serverURL == "" {
				return fmt.Errorf("DIRQ_SERVER_URL is not set — use --dry-run to see the generated query without executing")
			}

			// Execute the query.
			body, _ := json.Marshal(map[string]any{
				"query":   query,
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

			fmt.Printf("Status: %s | Targets: %d | Received: %d\n\n", result.Status, result.TotalTargets, result.Received)

			for _, r := range result.Results {
				if !r.Success {
					fmt.Printf("  %s: ERROR: %s\n", r.Hostname, r.Error)
					continue
				}
				formatted, _ := json.MarshalIndent(r.Data, "  ", "  ")
				fmt.Printf("  %s:\n  %s\n\n", r.Hostname, string(formatted))
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the generated query without executing it")
	cmd.Flags().IntVar(&timeout, "timeout", 60, "query timeout in seconds")
	cmd.Flags().StringVar(&model, "model", "", "LLM model (default: claude-sonnet-4-20250514 or gpt-4o)")
	cmd.Flags().StringVar(&provider, "provider", "", "LLM provider: anthropic or openai (auto-detected from API key)")

	return cmd
}

const askSystemPrompt = skillText + `
You are a DirQ query generator. Given a natural language question about a
fleet of servers, output ONLY the DirQ query — no explanation, no markdown
fences, no commentary. Just the raw query on a single line.

If the question is ambiguous, make reasonable assumptions and generate the
most useful query. If the question cannot be answered with a DirQ query,
respond with just: SELECT *
`

func llmTranslate(provider, model, question string) (string, error) {
	switch provider {
	case "anthropic":
		return llmAnthropic(model, question)
	case "openai":
		return llmOpenAI(model, question)
	default:
		return "", fmt.Errorf("unknown provider %q (use anthropic or openai)", provider)
	}
}

func llmAnthropic(model, question string) (string, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return "", fmt.Errorf("ANTHROPIC_API_KEY is not set")
	}
	if model == "" {
		model = "claude-sonnet-4-20250514"
	}

	reqBody, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 256,
		"system":     askSystemPrompt,
		"messages": []map[string]string{
			{"role": "user", "content": question},
		},
	})

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("Anthropic API error %d: %s", resp.StatusCode, string(data))
	}

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", err
	}
	if len(result.Content) == 0 {
		return "", fmt.Errorf("empty response from Anthropic")
	}

	return cleanQuery(result.Content[0].Text), nil
}

func llmOpenAI(model, question string) (string, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return "", fmt.Errorf("OPENAI_API_KEY is not set")
	}
	if model == "" {
		model = "gpt-4o"
	}

	reqBody, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 256,
		"messages": []map[string]string{
			{"role": "system", "content": askSystemPrompt},
			{"role": "user", "content": question},
		},
	})

	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("OpenAI API error %d: %s", resp.StatusCode, string(data))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", err
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("empty response from OpenAI")
	}

	return cleanQuery(result.Choices[0].Message.Content), nil
}

// cleanQuery strips markdown fences and whitespace from LLM output.
func cleanQuery(s string) string {
	s = strings.TrimSpace(s)
	// Strip ```sql ... ``` or ``` ... ```
	if strings.HasPrefix(s, "```") {
		lines := strings.Split(s, "\n")
		// Remove first and last lines (fences).
		if len(lines) >= 3 {
			lines = lines[1 : len(lines)-1]
		}
		s = strings.Join(lines, " ")
	}
	// Collapse multi-line queries to single line.
	s = strings.Join(strings.Fields(s), " ")
	return s
}

// ─────────────────────────────────────────────────────────
// dirq select
// ─────────────────────────────────────────────────────────

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
			fmt.Printf("Status: %s | Targets: %d | Received: %d\n\n", result.Status, result.TotalTargets, result.Received)

			if len(result.Results) == 0 {
				fmt.Println("No results.")
				return nil
			}

			for _, r := range result.Results {
				if r.Success {
					data, _ := json.MarshalIndent(r.Data, "  ", "  ")
					fmt.Printf("  %s:\n  %s\n\n", r.Hostname, string(data))
				} else {
					fmt.Printf("  %s: ERROR: %s\n\n", r.Hostname, r.Error)
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

func deployCmd() *cobra.Command {
	var timeout int

	cmd := &cobra.Command{
		Use:   "deploy [package] [WHERE ...]",
		Short: "Deploy a package across the fleet",
		Long: `Deploy an RPM, DEB, or MSI package to agents through the relay mesh.

The package is broadcast through the mesh tree — each link carries it
exactly once, regardless of fleet size. Targeted agents write the file
and run the install command; non-targeted relays just forward.

Examples:
  dirq deploy ./patch-2026-05.rpm
  dirq deploy ./patch.rpm WHERE tag.env = 'prod'
  dirq deploy ./agent-0.3.0.msi WHERE os_info.os = 'windows'`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pkgPath := args[0]

			// Validate the package file exists.
			info, err := os.Stat(pkgPath)
			if err != nil {
				return fmt.Errorf("package file not found: %s", pkgPath)
			}

			// Determine install command and dest path from extension.
			ext := strings.ToLower(filepath.Ext(pkgPath))
			deployID := fmt.Sprintf("%d", time.Now().UnixNano())

			var installCmd, destPath string
			switch ext {
			case ".rpm":
				destPath = fmt.Sprintf("/tmp/_dirq_deploy_%s%s", deployID, ext)
				installCmd = fmt.Sprintf("rpm -U --force %s", destPath)
			case ".deb":
				destPath = fmt.Sprintf("/tmp/_dirq_deploy_%s%s", deployID, ext)
				installCmd = fmt.Sprintf("dpkg -i %s", destPath)
			case ".msi":
				destPath = fmt.Sprintf(`C:\Windows\Temp\_dirq_deploy_%s.msi`, deployID)
				installCmd = fmt.Sprintf(`msiexec /i %s /qn`, destPath)
			default:
				return fmt.Errorf("unsupported package type %q (expected .rpm, .deb, or .msi)", ext)
			}

			// Read package content.
			pkgContent, err := os.ReadFile(pkgPath)
			if err != nil {
				return fmt.Errorf("read package: %w", err)
			}

			fmt.Printf("Package: %s (%.1f MB)\n", filepath.Base(pkgPath), float64(info.Size())/(1024*1024))

			// Build query from remaining args.
			queryStr := buildWhereQuery(args[1:])

			// Single broadcast request to the server.
			body, _ := json.Marshal(map[string]any{
				"query":           queryStr,
				"dest_path":       destPath,
				"content":         base64.StdEncoding.EncodeToString(pkgContent),
				"mode":            0644,
				"install_command": installCmd,
				"become":          true,
				"timeout":         timeout,
			})

			resp, err := apiStreamRequest("POST", "/api/v1/deploy", bytes.NewReader(body))
			if err != nil {
				return err
			}
			defer resp.Body.Close()

			if resp.StatusCode >= 400 {
				data, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
			}

			dec := json.NewDecoder(resp.Body)

			// First line: header with target count.
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

			fmt.Printf("Broadcasting to %d host(s)...\n\n", header.TotalTargets)

			// Stream results.
			totalSuccess := 0
			totalFail := 0

			for dec.More() {
				var r struct {
					Type     string `json:"type"`
					Hostname string `json:"hostname"`
					Success  bool   `json:"success"`
					Error    string `json:"error"`
					Phase    string `json:"phase"`
					RC       int    `json:"rc"`
					Stderr   string `json:"stderr"`
				}
				if err := dec.Decode(&r); err != nil {
					return fmt.Errorf("failed to read result: %w", err)
				}

				if r.Success {
					fmt.Printf("  %s: OK\n", r.Hostname)
					totalSuccess++
				} else {
					errMsg := r.Error
					if errMsg == "" && r.Stderr != "" {
						stderr, _ := base64.StdEncoding.DecodeString(r.Stderr)
						errMsg = string(stderr)
					}
					fmt.Printf("  %s: FAILED (%s, rc=%d: %s)\n", r.Hostname, r.Phase, r.RC, errMsg)
					totalFail++
				}
			}

			fmt.Printf("\nDeploy complete: %d succeeded, %d failed\n", totalSuccess, totalFail)
			if totalFail > 0 {
				return fmt.Errorf("%d deployment(s) failed", totalFail)
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&timeout, "timeout", 300, "timeout in seconds for each operation")

	return cmd
}

// ─────────────────────────────────────────────────────────
// dirq doctor
// ─────────────────────────────────────────────────────────

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "doctor",
		Short:         "Check the health of your DirQ deployment",
		Long:          "Validates connectivity, authentication, server health, fleet status, and local environment.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			pass := 0
			warn := 0
			fail := 0

			check := func(name string, fn func() (string, string)) {
				status, detail := fn()
				switch status {
				case "ok":
					fmt.Printf("  %-28s  ok   %s\n", name, detail)
					pass++
				case "warn":
					fmt.Printf("  %-28s  !!   %s\n", name, detail)
					warn++
				case "fail":
					fmt.Printf("  %-28s  FAIL %s\n", name, detail)
					fail++
				}
			}

			fmt.Println()

			// ── Local environment ──

			check("DIRQ_SERVER_URL", func() (string, string) {
				if serverURL == "" {
					return "fail", "not set"
				}
				return "ok", serverURL
			})

			check("DIRQ_TOKEN", func() (string, string) {
				if apiToken == "" {
					return "warn", "not set (ok if auth is disabled)"
				}
				return "ok", "configured"
			})

			// ── Server connectivity ──

			check("Server reachable", func() (string, string) {
				if serverURL == "" {
					return "fail", "no server URL"
				}
				_, err := apiRequest("GET", "/healthz", nil)
				if err != nil {
					return "fail", err.Error()
				}
				return "ok", serverURL
			})

			// ── TLS ──

			check("TLS certificate", func() (string, string) {
				if serverURL == "" {
					return "fail", "no server URL"
				}
				if !strings.HasPrefix(serverURL, "https://") {
					return "warn", "using plain HTTP"
				}
				if tlsInsecure {
					return "warn", "verification disabled (--tls-insecure)"
				}
				// Try connecting to check cert validity.
				_, err := apiRequest("GET", "/healthz", nil)
				if err != nil {
					return "fail", err.Error()
				}
				return "ok", "valid"
			})

			// ── Authentication ──

			check("API token valid", func() (string, string) {
				if serverURL == "" {
					return "fail", "no server URL"
				}
				// Try an authenticated endpoint. If auth is disabled, this still works.
				resp, err := apiRequest("GET", "/api/v1/hosts", nil)
				if err != nil {
					if strings.Contains(err.Error(), "401") {
						return "fail", "invalid or expired token"
					}
					return "fail", err.Error()
				}
				_ = resp
				return "ok", "authenticated"
			})

			// ── Server status (requires auth) ──

			check("PostgreSQL", func() (string, string) {
				resp, err := apiRequest("GET", "/api/v1/status", nil)
				if err != nil {
					return "fail", err.Error()
				}
				var status struct {
					Database bool `json:"database"`
				}
				json.Unmarshal(resp, &status)
				if !status.Database {
					return "fail", "connection failed"
				}
				return "ok", "connected"
			})

			var statusData struct {
				AgentsTotal    int            `json:"agents_total"`
				AgentsOnline   int            `json:"agents_online"`
				ZoneLeaders    int            `json:"zone_leaders"`
				MaxTreeDepth   int            `json:"max_tree_depth"`
				OrphanedAgents int            `json:"orphaned_agents"`
				AgentVersions  map[string]int `json:"agent_versions"`
				Topology       struct {
					MaxZoneLeaders     int `json:"max_zone_leaders"`
					MaxChildrenPerNode int `json:"max_children_per_node"`
				} `json:"topology"`
			}
			statusResp, statusErr := apiRequest("GET", "/api/v1/status", nil)
			if statusErr == nil {
				json.Unmarshal(statusResp, &statusData)
			}

			check("Agents online", func() (string, string) {
				if statusErr != nil {
					return "fail", statusErr.Error()
				}
				detail := fmt.Sprintf("%d/%d", statusData.AgentsOnline, statusData.AgentsTotal)
				if statusData.AgentsOnline == 0 && statusData.AgentsTotal > 0 {
					return "warn", detail+" (none online)"
				}
				if statusData.AgentsOnline == 0 {
					return "warn", "no agents registered"
				}
				return "ok", detail
			})

			check("Agent version skew", func() (string, string) {
				if statusErr != nil {
					return "fail", statusErr.Error()
				}
				if len(statusData.AgentVersions) <= 1 {
					for v := range statusData.AgentVersions {
						return "ok", "all on " + v
					}
					return "ok", "no agents"
				}
				parts := []string{}
				for v, count := range statusData.AgentVersions {
					parts = append(parts, fmt.Sprintf("%d on %s", count, v))
				}
				return "warn", strings.Join(parts, ", ")
			})

			check("Relay tree", func() (string, string) {
				if statusErr != nil {
					return "fail", statusErr.Error()
				}
				detail := fmt.Sprintf("depth %d, %d zone leader(s)", statusData.MaxTreeDepth, statusData.ZoneLeaders)
				if statusData.OrphanedAgents > 0 {
					return "warn", fmt.Sprintf("%s, %d orphaned", detail, statusData.OrphanedAgents)
				}
				return "ok", detail
			})

			// ── Local tools ──

			check("Ansible installed", func() (string, string) {
				out, err := exec.Command("ansible-playbook", "--version").Output()
				if err != nil {
					return "warn", "not found (dirq run/deploy won't work)"
				}
				// First line is "ansible-playbook [core X.Y.Z]"
				lines := strings.SplitN(string(out), "\n", 2)
				return "ok", strings.TrimSpace(lines[0])
			})

			check("Connection plugin", func() (string, string) {
				dir := connectionPluginDir()
				if dir == "" {
					return "warn", "not found relative to binary"
				}
				return "ok", dir
			})

			// ── Summary ──

			fmt.Println()
			fmt.Printf("  %d passed, %d warnings, %d failed\n\n", pass, warn, fail)

			if fail > 0 {
				return fmt.Errorf("%d check(s) failed", fail)
			}
			return nil
		},
	}
}

// ─────────────────────────────────────────────────────────
// dirq cve
// ─────────────────────────────────────────────────────────

func cveCmd() *cobra.Command {
	var timeout int

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

			// Fetch CVE data from Red Hat.
			fmt.Fprintf(os.Stderr, "Fetching %s from Red Hat Security Data API...\n", cveID)

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

			fmt.Fprintf(os.Stderr, "%s: %s\n", cveID, cveData.Bugzilla.Description)
			fmt.Fprintf(os.Stderr, "Severity: %s\n", cveData.ThreatSeverity)

			// Extract affected package names and fixed versions from affected_release.
			// Package format: "name-epoch:version-release.el9" or similar.
			type fixedPkg struct {
				name       string // RPM source package name
				fullNEVRA  string // full name-epoch:version-release string
				fixVersion string // version-release portion for comparison
			}

			var fixedPkgs []fixedPkg
			seenPkgs := map[string]bool{}

			for _, ar := range cveData.AffectedRelease {
				if ar.Package == "" {
					continue
				}
				// Only care about RHEL (not middleware, containers, etc.)
				if !strings.Contains(ar.CPE, "enterprise_linux") {
					continue
				}
				name, version := parseRPMNEVRA(ar.Package)
				if name == "" {
					continue
				}
				if seenPkgs[name] {
					continue
				}
				seenPkgs[name] = true
				fixedPkgs = append(fixedPkgs, fixedPkg{
					name:       name,
					fullNEVRA:  ar.Package,
					fixVersion: version,
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
				if !seenPkgs[ps.PackageName] {
					seenPkgs[ps.PackageName] = true
					fixedPkgs = append(fixedPkgs, fixedPkg{
						name: ps.PackageName,
					})
				}
			}

			if len(fixedPkgs) == 0 {
				fmt.Println("No RHEL packages associated with this CVE.")
				return nil
			}

			// Build package name list for display and query.
			pkgNames := make([]string, len(fixedPkgs))
			for i, fp := range fixedPkgs {
				pkgNames[i] = fp.name
			}

			fmt.Fprintf(os.Stderr, "Packages: %s\n", strings.Join(pkgNames, ", "))
			for _, fp := range fixedPkgs {
				if fp.fixVersion != "" {
					fmt.Fprintf(os.Stderr, "  %s: fixed in %s\n", fp.name, fp.fullNEVRA)
				} else {
					fmt.Fprintf(os.Stderr, "  %s: no fix available (still affected)\n", fp.name)
				}
			}
			fmt.Fprintln(os.Stderr)

			// Build DirQ query to find RHEL hosts with these packages installed.
			inList := make([]string, len(pkgNames))
			for i, n := range pkgNames {
				inList[i] = "'" + n + "'"
			}
			pkgFilter := "packages.name IN (" + strings.Join(inList, ", ") + ")"

			// Add WHERE clause from remaining args if provided.
			var whereExtra string
			if len(args) > 1 {
				whereExtra = " AND " + strings.Join(args[1:], " ")
				// Strip leading WHERE if the user wrote it.
				whereExtra = strings.Replace(whereExtra, " AND WHERE ", " AND ", 1)
				whereExtra = strings.Replace(whereExtra, " AND where ", " AND ", 1)
			}

			queryStr := fmt.Sprintf("SELECT hostname, os_info.os, os_info.os_version, packages.name, packages.version WHERE %s%s",
				pkgFilter, whereExtra)

			fmt.Fprintf(os.Stderr, "Scanning fleet...\n\n")

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

			if jsonOut {
				fmt.Println(string(queryResp))
				return nil
			}

			// Build fixed version lookup.
			fixedVersionMap := map[string]string{} // pkg name → fixed version string
			for _, fp := range fixedPkgs {
				if fp.fixVersion != "" {
					fixedVersionMap[fp.name] = fp.fixVersion
				}
			}

			vulnerable := 0
			patched := 0
			noFix := 0

			for _, r := range result.Results {
				if !r.Success {
					continue
				}

				// Check OS — skip non-RHEL.
				osName, _ := r.Data["os_info.os"].(string)
				if osName == "" {
					// Try nested format.
					if oi, ok := r.Data["os_info"].(map[string]any); ok {
						osName, _ = oi["os"].(string)
					}
				}

				// Extract packages from results.
				pkgs := extractPackageList(r.Data)
				for _, pkg := range pkgs {
					fixedVer, hasfix := fixedVersionMap[pkg.name]
					if !hasfix {
						// Package is affected but no fix available.
						fmt.Printf("  %-24s %-20s %-20s  VULNERABLE (no fix available)\n",
							r.Hostname, pkg.name, pkg.version)
						noFix++
						continue
					}

					if rpmVersionCompare(pkg.version, fixedVer) < 0 {
						fmt.Printf("  %-24s %-20s %-20s  VULNERABLE (fixed in %s)\n",
							r.Hostname, pkg.name, pkg.version, fixedVer)
						vulnerable++
					} else {
						fmt.Printf("  %-24s %-20s %-20s  patched\n",
							r.Hostname, pkg.name, pkg.version)
						patched++
					}
				}
			}

			fmt.Printf("\n%d vulnerable, %d patched", vulnerable, patched)
			if noFix > 0 {
				fmt.Printf(", %d no fix available", noFix)
			}
			fmt.Println()

			if vulnerable > 0 || noFix > 0 {
				return fmt.Errorf("vulnerable systems found")
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&timeout, "timeout", 60, "query timeout in seconds")
	return cmd
}

type pkgInfo struct {
	name    string
	version string
}

// extractPackageList pulls package name/version pairs from query result data.
func extractPackageList(data map[string]any) []pkgInfo {
	var pkgs []pkgInfo

	// The data may have packages as an array under "packages" key
	// or as flattened "packages.name" / "packages.version" fields.
	if nameVal, ok := data["packages.name"]; ok {
		// Flattened single package.
		name, _ := nameVal.(string)
		version, _ := data["packages.version"].(string)
		if name != "" {
			pkgs = append(pkgs, pkgInfo{name, version})
		}
		return pkgs
	}

	if pkgData, ok := data["packages"]; ok {
		switch v := pkgData.(type) {
		case []any:
			for _, item := range v {
				if m, ok := item.(map[string]any); ok {
					name, _ := m["name"].(string)
					version, _ := m["version"].(string)
					if name != "" {
						pkgs = append(pkgs, pkgInfo{name, version})
					}
				}
			}
		case map[string]any:
			name, _ := v["name"].(string)
			version, _ := v["version"].(string)
			if name != "" {
				pkgs = append(pkgs, pkgInfo{name, version})
			}
		}
	}

	return pkgs
}

// parseRPMNEVRA extracts the package name and version-release from an RPM NEVRA string.
// Input: "python3-setuptools-0:68.2.2-4.el8_10" or "openssl-1:3.0.7-27.el9"
// Returns: name="python3-setuptools", version="0:68.2.2-4.el8_10"
func parseRPMNEVRA(nevra string) (name, version string) {
	// RPM NEVRA: name-[epoch:]version-release.arch
	// We need to find the boundary between name and epoch:version.
	// The epoch contains a colon, which helps locate it.
	colonIdx := strings.Index(nevra, ":")
	if colonIdx < 0 {
		// No epoch — find the last two hyphens (name-version-release).
		lastDash := strings.LastIndex(nevra, "-")
		if lastDash < 0 {
			return "", ""
		}
		secondLast := strings.LastIndex(nevra[:lastDash], "-")
		if secondLast < 0 {
			return nevra[:lastDash], nevra[lastDash+1:]
		}
		return nevra[:secondLast], nevra[secondLast+1:]
	}

	// Find the dash before the epoch digit.
	epochStart := colonIdx
	for epochStart > 0 && nevra[epochStart-1] != '-' {
		epochStart--
	}
	if epochStart == 0 {
		return "", ""
	}
	return nevra[:epochStart-1], nevra[epochStart:]
}

// rpmVersionCompare compares two RPM version strings.
// Returns -1, 0, or 1 like strcmp.
func rpmVersionCompare(a, b string) int {
	// Strip epoch if present — compare epoch first, then version-release.
	ae, av := splitEpoch(a)
	be, bv := splitEpoch(b)

	if ae != be {
		if ae < be {
			return -1
		}
		return 1
	}

	return rpmVerCmp(av, bv)
}

func splitEpoch(v string) (int, string) {
	if idx := strings.Index(v, ":"); idx >= 0 {
		e := 0
		fmt.Sscanf(v[:idx], "%d", &e)
		return e, v[idx+1:]
	}
	return 0, v
}

// rpmVerCmp implements RPM's version comparison algorithm.
func rpmVerCmp(a, b string) int {
	if a == b {
		return 0
	}

	segA := rpmSegments(a)
	segB := rpmSegments(b)

	for i := 0; i < len(segA) && i < len(segB); i++ {
		sa := segA[i]
		sb := segB[i]

		// Both numeric — compare as integers.
		aNum := isNumeric(sa)
		bNum := isNumeric(sb)

		if aNum && bNum {
			// Strip leading zeros for numeric comparison.
			sa = strings.TrimLeft(sa, "0")
			sb = strings.TrimLeft(sb, "0")
			if len(sa) != len(sb) {
				if len(sa) < len(sb) {
					return -1
				}
				return 1
			}
			if sa < sb {
				return -1
			}
			if sa > sb {
				return 1
			}
			continue
		}

		// Numeric beats alpha.
		if aNum {
			return 1
		}
		if bNum {
			return -1
		}

		// Both alpha — strcmp.
		if sa < sb {
			return -1
		}
		if sa > sb {
			return 1
		}
	}

	if len(segA) < len(segB) {
		return -1
	}
	if len(segA) > len(segB) {
		return 1
	}
	return 0
}

func rpmSegments(v string) []string {
	var segs []string
	i := 0
	for i < len(v) {
		// Skip non-alphanumeric separators.
		for i < len(v) && !isAlnum(v[i]) {
			i++
		}
		if i >= len(v) {
			break
		}
		start := i
		if v[i] >= '0' && v[i] <= '9' {
			for i < len(v) && v[i] >= '0' && v[i] <= '9' {
				i++
			}
		} else {
			for i < len(v) && isAlpha(v[i]) {
				i++
			}
		}
		segs = append(segs, v[start:i])
	}
	return segs
}

func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

func isAlnum(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// ─────────────────────────────────────────────────────────
// Shared query helpers
// ─────────────────────────────────────────────────────────

// parseTagArgs splits args into key=value tags, WHERE clause args, and an
// optional host ID. If the first arg doesn't contain "=" and isn't "WHERE",
// it's treated as a host ID (backwards-compatible with the old syntax).
func parseTagArgs(args []string) (tags map[string]string, whereArgs []string, hostID string) {
	tags = make(map[string]string)

	// Check if the first arg is a host ID (not key=value, not WHERE).
	startIdx := 0
	if len(args) > 0 && !strings.Contains(args[0], "=") && !strings.EqualFold(args[0], "WHERE") {
		hostID = args[0]
		startIdx = 1
	}

	for i := startIdx; i < len(args); i++ {
		if strings.EqualFold(args[i], "WHERE") {
			whereArgs = args[i:]
			break
		}
		parts := strings.SplitN(args[i], "=", 2)
		if len(parts) == 2 {
			tags[parts[0]] = parts[1]
		}
	}
	return
}

// parseUntagArgs splits args into tag keys, WHERE clause args, and an
// optional host ID. If the first arg isn't "WHERE" and later args include
// WHERE, or if no WHERE exists and there are 2+ args, the first arg is
// treated as a host ID (backwards-compatible).
func parseUntagArgs(args []string) (keys []string, whereArgs []string, hostID string) {
	// Find WHERE boundary.
	whereIdx := -1
	for i, a := range args {
		if strings.EqualFold(a, "WHERE") {
			whereIdx = i
			break
		}
	}

	if whereIdx >= 0 {
		whereArgs = args[whereIdx:]
		beforeWhere := args[:whereIdx]
		// If the first arg before WHERE looks like a host ID (UUID-like),
		// treat it as one. Otherwise all args before WHERE are tag keys.
		if len(beforeWhere) > 0 && looksLikeID(beforeWhere[0]) {
			hostID = beforeWhere[0]
			keys = beforeWhere[1:]
		} else {
			keys = beforeWhere
		}
	} else {
		// No WHERE — old syntax: first arg is host ID, rest are keys.
		if len(args) >= 2 {
			hostID = args[0]
			keys = args[1:]
		} else {
			keys = args
		}
	}
	return
}

// looksLikeID returns true if s looks like a UUID or agent ID rather than
// a simple tag key name. Agent IDs contain hyphens and are long.
func looksLikeID(s string) bool {
	return len(s) > 16 && strings.Contains(s, "-")
}

// dataField extracts a nested field from query result data.
// e.g. dataField(data, "os_info", "os") gets data["os_info"]["os"].
func dataField(data map[string]any, module, field string) string {
	if m, ok := data[module].(map[string]any); ok {
		if v, ok := m[field]; ok {
			return fmt.Sprintf("%v", v)
		}
	}
	// Also try flattened dotted key (e.g. "os_info.os").
	if v, ok := data[module+"."+field]; ok {
		return fmt.Sprintf("%v", v)
	}
	return ""
}

func hostIDOrName(names []string, idx int, fallback string) string {
	if idx < len(names) {
		return names[idx]
	}
	return fallback
}

// buildSelectQuery reconstructs a SELECT query from positional args.
// Args like ["hostname,", "cpu.count", "WHERE", "tag.env", "=", "'prod'"]
// become "SELECT hostname, cpu.count WHERE tag.env = 'prod'"
func buildSelectQuery(args []string) string {
	return "SELECT " + strings.Join(args, " ")
}

// buildWhereQuery reconstructs a "SELECT hostname WHERE ..." from args
// that follow the first positional arg (a file path). If no WHERE is
// found, returns "SELECT hostname" (all online agents).
func buildWhereQuery(args []string) string {
	if len(args) == 0 {
		return "SELECT hostname"
	}
	return "SELECT hostname " + strings.Join(args, " ")
}

type queryHost struct {
	hostname string
	agentID  string
}

// runQuery executes a DirQ query and returns matching hosts.
func runQuery(queryStr string, timeout int) ([]queryHost, error) {
	body, _ := json.Marshal(map[string]any{
		"query":   queryStr,
		"timeout": timeout,
	})
	resp, err := apiRequest("POST", "/api/v1/query", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}

	var result struct {
		Results []struct {
			AgentID  string `json:"agent_id"`
			Hostname string `json:"hostname"`
			Success  bool   `json:"success"`
		} `json:"results"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("parse query result: %w", err)
	}

	var hosts []queryHost
	for _, r := range result.Results {
		if r.Success && r.Hostname != "" {
			hosts = append(hosts, queryHost{r.Hostname, r.AgentID})
		}
	}
	return hosts, nil
}

// writeInventory creates a temporary YAML inventory file for Ansible.
func writeInventory(hosts []queryHost) (string, error) {
	tmpInv, err := os.CreateTemp("", "dirq-inventory-*.yml")
	if err != nil {
		return "", fmt.Errorf("create temp inventory: %w", err)
	}

	fmt.Fprintf(tmpInv, "all:\n  hosts:\n")
	for _, h := range hosts {
		fmt.Fprintf(tmpInv, "    %s:\n", h.hostname)
		fmt.Fprintf(tmpInv, "      dirq_agent_id: %s\n", h.agentID)
		fmt.Fprintf(tmpInv, "      dirq_server_url: %s\n", serverURL)
		fmt.Fprintf(tmpInv, "      ansible_connection: dirq\n")
		fmt.Fprintf(tmpInv, "      ansible_python_interpreter: /usr/bin/python3\n")
	}
	tmpInv.Close()
	return tmpInv.Name(), nil
}

// connectionPluginDir returns the path to the DirQ Ansible connection plugin.
func connectionPluginDir() string {
	exePath, _ := os.Executable()
	pluginDir := filepath.Join(filepath.Dir(exePath), "..", "ansible", "connection_plugins")
	if absDir, err := filepath.Abs(pluginDir); err == nil {
		if _, err := os.Stat(absDir); err == nil {
			return absDir
		}
	}
	return ""
}

// ─────────────────────────────────────────────────────────
// HTTP client helpers
// ─────────────────────────────────────────────────────────

// httpClient returns a shared HTTP client, creating it once on first use.
// When --tls-insecure is set, the client skips certificate verification
// but still reuses connections (unlike the old code which created a new
// client + transport on every request).
var _httpClient *http.Client

func httpClient() *http.Client {
	if _httpClient == nil {
		if tlsInsecure {
			_httpClient = &http.Client{
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				},
			}
		} else {
			_httpClient = http.DefaultClient
		}
	}
	return _httpClient
}

// apiStreamRequest returns the raw HTTP response for streaming (caller must close Body).
func apiStreamRequest(method, path string, body io.Reader) (*http.Response, error) {
	url := strings.TrimRight(serverURL, "/") + path
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+apiToken)
	}

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	return resp, nil
}

func apiRequest(method, path string, body io.Reader) ([]byte, error) {
	url := strings.TrimRight(serverURL, "/") + path
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+apiToken)
	}

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
	}

	return data, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
