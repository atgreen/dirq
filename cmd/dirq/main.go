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
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/atgreen/dirq/internal/tlsutil"
	"github.com/spf13/cobra"
)

var (
	serverURL string
	apiToken  string
	jsonOut   bool
)

func main() {
	root := &cobra.Command{
		Use:   "dirq",
		Short: "DirQ — Real-Time Endpoint Query CLI",
	}

	root.PersistentFlags().StringVar(&serverURL, "server", os.Getenv("DIRQ_SERVER_URL"), "DirQ server URL (or set DIRQ_SERVER_URL)")
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// Allow tls generate to run without a server URL.
		if cmd.Name() == "generate" {
			return nil
		}
		if serverURL == "" {
			return fmt.Errorf("DIRQ_SERVER_URL is not set. Use --server or export DIRQ_SERVER_URL=http://your-dirq-server:8080")
		}
		return nil
	}
	root.PersistentFlags().StringVar(&apiToken, "token", os.Getenv("DIRQ_TOKEN"), "API token")
	root.PersistentFlags().BoolVar(&jsonOut, "json", false, "output raw JSON")

	root.AddCommand(queryCmd())
	root.AddCommand(hostsCmd())
	root.AddCommand(tokenCmd())
	root.AddCommand(queriesCmd())
	root.AddCommand(tlsCmd())
	root.AddCommand(runCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// ─────────────────────────────────────────────────────────
// dirq query
// ─────────────────────────────────────────────────────────

func queryCmd() *cobra.Command {
	var timeout int

	cmd := &cobra.Command{
		Use:   "query [DirQ expression]",
		Short: "Run an ad-hoc query across the fleet",
		Long: `Run a DirQ query and display results.

Examples:
  dirq query "SELECT hostname, disk.pct_used FROM * WHERE disk.pct_used > 80"
  dirq query "SELECT hostname, cpu.cores FROM tag:prod"
  dirq query "SELECT os, COUNT(hostname) FROM * GROUP BY os"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, _ := json.Marshal(map[string]any{
				"query":   args[0],
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

			// Print results as formatted JSON per host.
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
// dirq hosts
// ─────────────────────────────────────────────────────────

func hostsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hosts",
		Short: "Manage hosts",
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List all registered hosts",
		RunE: func(cmd *cobra.Command, args []string) error {
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
		Use:   "tag [id] [key=value ...]",
		Short: "Add or update tags on a host",
		Long:  "Add or update tags. Example: dirq hosts tag abc-123 env=prod role=webserver",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			tags := make(map[string]string)
			for _, kv := range args[1:] {
				parts := strings.SplitN(kv, "=", 2)
				if len(parts) != 2 {
					return fmt.Errorf("invalid tag format %q, expected key=value", kv)
				}
				tags[parts[0]] = parts[1]
			}
			body, _ := json.Marshal(tags)
			resp, err := apiRequest("PATCH", "/api/v1/hosts/"+id+"/tags", bytes.NewReader(body))
			if err != nil {
				return err
			}
			var agent struct {
				Hostname string            `json:"hostname"`
				Tags     map[string]string `json:"tags"`
			}
			json.Unmarshal(resp, &agent)
			fmt.Printf("Tags updated for %s:\n", agent.Hostname)
			for k, v := range agent.Tags {
				fmt.Printf("  %s=%s\n", k, v)
			}
			return nil
		},
	}

	untagCmd := &cobra.Command{
		Use:   "untag [id] [key ...]",
		Short: "Remove tags from a host",
		Long:  "Remove tags by key. Example: dirq hosts untag abc-123 env role",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			for _, key := range args[1:] {
				_, err := apiRequest("DELETE", "/api/v1/hosts/"+id+"/tags/"+key, nil)
				if err != nil {
					return fmt.Errorf("failed to remove tag %q: %w", key, err)
				}
				fmt.Printf("Removed tag: %s\n", key)
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
		queryStr    string
		playbook    string
		module      string
		moduleArgs  string
		command     string
		forks       int
		extraArgs   []string
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Query the fleet and run Ansible against the results",
		Long: `Run a DirQ query to select hosts, then execute an Ansible playbook,
module, or ad-hoc command against exactly those hosts.

Examples:
  # Run a playbook against hosts with full disks
  dirq run --query "SELECT os_info.hostname FROM * WHERE disk.pct_used > 90" \
    --playbook cleanup-disks.yml

  # Ad-hoc command against hosts with a vulnerable package
  dirq run --query "SELECT os_info.hostname FROM * WHERE packages.name = 'openssl' AND packages.version LIKE '1.%%'" \
    --command "yum update -y openssl"

  # Run a module against hosts where sshd is stopped
  dirq run --query "SELECT os_info.hostname FROM * WHERE services.name = 'sshd' AND services.state = 'stopped'" \
    --module service --module-args "name=sshd state=started"

  # Ping all linux hosts
  dirq run --query "SELECT os_info.hostname FROM * WHERE os_info.os = 'linux'" \
    --module ping`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if queryStr == "" {
				return fmt.Errorf("--query is required")
			}
			if playbook == "" && module == "" && command == "" {
				return fmt.Errorf("one of --playbook, --module, or --command is required")
			}

			// Run the query to get matching hostnames.
			body, _ := json.Marshal(map[string]any{
				"query":   queryStr,
				"timeout": 60,
			})
			resp, err := apiRequest("POST", "/api/v1/query", bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("query failed: %w", err)
			}

			var result struct {
				Results []struct {
					Hostname string `json:"hostname"`
					Success  bool   `json:"success"`
				} `json:"results"`
			}
			if err := json.Unmarshal(resp, &result); err != nil {
				return fmt.Errorf("parse query result: %w", err)
			}

			var hosts []string
			for _, r := range result.Results {
				if r.Success && r.Hostname != "" {
					hosts = append(hosts, r.Hostname)
				}
			}

			if len(hosts) == 0 {
				fmt.Println("No hosts matched the query.")
				return nil
			}

			fmt.Printf("Query matched %d host(s): %s\n\n", len(hosts), strings.Join(hosts, ", "))

			// Write ephemeral inventory file.
			tmpInv, err := os.CreateTemp("", "dirq-inventory-*.ini")
			if err != nil {
				return fmt.Errorf("create temp inventory: %w", err)
			}
			defer os.Remove(tmpInv.Name())

			for _, h := range hosts {
				fmt.Fprintf(tmpInv, "%s dirq_server_url=%s\n", h, serverURL)
			}
			tmpInv.Close()

			// Build the ansible command.
			var ansibleCmd []string

			if playbook != "" {
				ansibleCmd = []string{"ansible-playbook", "-i", tmpInv.Name(), playbook}
			} else if module != "" {
				ansibleCmd = []string{"ansible", "all", "-i", tmpInv.Name(), "-m", module}
				if moduleArgs != "" {
					ansibleCmd = append(ansibleCmd, "-a", moduleArgs)
				}
			} else {
				ansibleCmd = []string{"ansible", "all", "-i", tmpInv.Name(), "-m", "raw", "-a", command}
			}

			if forks > 0 {
				ansibleCmd = append(ansibleCmd, "-f", fmt.Sprintf("%d", forks))
			}

			ansibleCmd = append(ansibleCmd, extraArgs...)

			fmt.Printf("Running: %s\n\n", strings.Join(ansibleCmd, " "))

			// Execute ansible.
			proc := exec.Command(ansibleCmd[0], ansibleCmd[1:]...)
			proc.Stdout = os.Stdout
			proc.Stderr = os.Stderr
			proc.Stdin = os.Stdin

			// Pass through DirQ env vars.
			proc.Env = os.Environ()

			return proc.Run()
		},
	}

	cmd.Flags().StringVar(&queryStr, "query", "", "DirQ query to select hosts (required)")
	cmd.Flags().StringVar(&playbook, "playbook", "", "Ansible playbook to run")
	cmd.Flags().StringVar(&module, "module", "", "Ansible module to run")
	cmd.Flags().StringVar(&moduleArgs, "module-args", "", "Arguments for the module")
	cmd.Flags().StringVar(&command, "command", "", "Ad-hoc command to run (uses raw module)")
	cmd.Flags().IntVar(&forks, "forks", 0, "Number of parallel processes (default: Ansible default)")
	cmd.Flags().StringArrayVar(&extraArgs, "extra", nil, "Extra arguments passed to ansible/ansible-playbook")

	return cmd
}

// ─────────────────────────────────────────────────────────
// HTTP client helpers
// ─────────────────────────────────────────────────────────

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

	resp, err := http.DefaultClient.Do(req)
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
