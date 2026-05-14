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
	"sort"
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
	root.AddCommand(skillCmd())
	root.AddCommand(askCmd())
	root.AddCommand(selectCmd())
	root.AddCommand(deployCmd())
	root.AddCommand(doctorCmd())

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

All commands support arg flattening — quoted multi-word args are split
by whitespace, so dirq "hosts list" works like dirq hosts list.

### dirq select — query the fleet

    dirq select hostname, disk.pct_used WHERE disk.pct_used \> 80
    dirq select \* --json
    dirq "select hostname where memory.pct_used > 90"

### dirq run — run Ansible against matching hosts

    dirq run deploy.yml WHERE tag.env = 'prod'
    dirq run cleanup.yml
    dirq run --command "systemctl restart nginx" WHERE tag.env = 'prod'
    dirq run --module ping WHERE os_info.os = 'linux'

### dirq deploy — deploy packages through the mesh

    dirq deploy ./patch.rpm WHERE tag.env = 'prod'
    dirq deploy ./agent-0.3.0.msi WHERE os_info.os = 'windows'
    dirq deploy ./fix.deb --parallel

Depth-first rolling wave by default. Supports .rpm, .deb, .msi.

### dirq ask — natural language queries (requires LLM API key)

    dirq ask "which prod hosts have full disks?"
    dirq ask "how many hosts are running linux?" --dry-run

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

Examples:
  dirq select hostname, disk.pct_used WHERE disk.pct_used \> 80
  dirq select hostname, cpu.cores WHERE tag.env = 'prod'
  dirq select os_info.os, COUNT(hostname) GROUP BY os_info.os
  dirq "select hostname where memory.pct_used > 90"`,
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
	var (
		parallel bool
		timeout  int
	)

	cmd := &cobra.Command{
		Use:   "deploy [package] [WHERE ...]",
		Short: "Deploy a package across the fleet",
		Long: `Deploy an RPM, DEB, or MSI package to agents through the relay mesh.

Uses rolling wave deployment by default (leaves first, then relays,
then zone leaders). Use --parallel to install on all targets at once.

Examples:
  dirq deploy ./patch-2026-05.rpm
  dirq deploy ./patch.rpm WHERE tag.env = 'prod'
  dirq deploy ./agent-0.3.0.msi WHERE os_info.os = 'windows'
  dirq deploy ./dirq-agent-0.3.0.rpm --parallel`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pkgPath := args[0]

			// Validate the package file exists.
			info, err := os.Stat(pkgPath)
			if err != nil {
				return fmt.Errorf("package file not found: %s", pkgPath)
			}

			// Determine install command from extension.
			ext := strings.ToLower(filepath.Ext(pkgPath))
			var installCmd string
			switch ext {
			case ".rpm":
				installCmd = "rpm -U --force /tmp/_dirq_deploy_pkg" + ext
			case ".deb":
				installCmd = "dpkg -i /tmp/_dirq_deploy_pkg" + ext
			case ".msi":
				installCmd = `msiexec /i C:\Windows\Temp\_dirq_deploy_pkg.msi /qn`
			default:
				return fmt.Errorf("unsupported package type %q (expected .rpm, .deb, or .msi)", ext)
			}

			// Read package content.
			pkgContent, err := os.ReadFile(pkgPath)
			if err != nil {
				return fmt.Errorf("read package: %w", err)
			}

			fmt.Printf("Package: %s (%.1f MB)\n", filepath.Base(pkgPath), float64(info.Size())/(1024*1024))

			// Build query from remaining args (after the package path).
			queryStr := buildWhereQuery(args[1:])

			// Query for target hosts.
			hosts, err := runQuery(queryStr, timeout)
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
			fmt.Printf("Targets: %d host(s): %s\n", len(hosts), strings.Join(names, ", "))

			if parallel {
				fmt.Println("Mode: parallel (all at once)")
			} else {
				fmt.Println("Mode: depth-first (deepest nodes first)")
			}
			fmt.Println()

			// Fetch topology for each target to determine depth.
			type targetHost struct {
				queryHost
				parentID string
				depth    int
			}

			targetSet := make(map[string]bool)
			for _, h := range hosts {
				targetSet[h.agentID] = true
			}

			var targets []targetHost
			for _, h := range hosts {
				resp, err := apiRequest("GET", "/api/v1/hosts/"+h.agentID, nil)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  warning: could not fetch agent %s: %v\n", h.hostname, err)
					targets = append(targets, targetHost{h, "", 0})
					continue
				}
				var agent struct {
					ParentID *string `json:"parent_id"`
				}
				json.Unmarshal(resp, &agent)
				pid := ""
				if agent.ParentID != nil {
					pid = *agent.ParentID
				}
				targets = append(targets, targetHost{h, pid, 0})
			}

			// Compute depth for each target by walking parent chains.
			// Build a parent lookup from all agents (not just targets).
			allResp, err := apiRequest("GET", "/api/v1/hosts", nil)
			if err == nil {
				var allAgents []struct {
					ID       string  `json:"id"`
					ParentID *string `json:"parent_id"`
				}
				json.Unmarshal(allResp, &allAgents)

				parentMap := make(map[string]string) // agentID → parentID
				for _, a := range allAgents {
					if a.ParentID != nil {
						parentMap[a.ID] = *a.ParentID
					}
				}

				for i := range targets {
					depth := 0
					cur := targets[i].agentID
					seen := make(map[string]bool)
					for {
						pid, ok := parentMap[cur]
						if !ok || pid == "" || seen[cur] {
							break
						}
						seen[cur] = true
						depth++
						cur = pid
					}
					targets[i].depth = depth
				}
			}

			// Sort by depth descending (deepest first).
			// Targets at the same depth are deployed in parallel within a wave.
			sort.Slice(targets, func(i, j int) bool {
				return targets[i].depth > targets[j].depth
			})

			// Group into waves by depth level.
			var waves [][]targetHost
			if parallel {
				waves = [][]targetHost{targets}
			} else {
				currentDepth := -1
				for _, t := range targets {
					if t.depth != currentDepth {
						waves = append(waves, []targetHost{})
						currentDepth = t.depth
					}
					waves[len(waves)-1] = append(waves[len(waves)-1], t)
				}
			}

			// Deploy wave by wave (deepest first).
			b64Content := base64.StdEncoding.EncodeToString(pkgContent)
			destPath := "/tmp/_dirq_deploy_pkg" + ext
			if ext == ".msi" {
				destPath = `C:\Windows\Temp\_dirq_deploy_pkg.msi`
			}

			totalSuccess := 0
			totalFail := 0

			for wi, wave := range waves {
				if !parallel {
					waveNames := make([]string, len(wave))
					for i, t := range wave {
						waveNames[i] = t.hostname
					}
					fmt.Printf("Wave %d/%d (depth %d): %s\n", wi+1, len(waves), wave[0].depth, strings.Join(waveNames, ", "))
				}

				for _, t := range wave {
					fmt.Printf("  %s: uploading... ", t.hostname)

					// Put file.
					putBody, _ := json.Marshal(map[string]any{
						"agent_id":  t.agentID,
						"dest_path": destPath,
						"content":   b64Content,
						"mode":      0644,
						"timeout":   timeout,
					})
					putResp, err := apiRequest("POST", "/api/v1/put_file", bytes.NewReader(putBody))
					if err != nil {
						fmt.Printf("FAILED (upload: %v)\n", err)
						totalFail++
						continue
					}
					var putResult struct {
						Success bool   `json:"success"`
						Error   string `json:"error"`
					}
					json.Unmarshal(putResp, &putResult)
					if !putResult.Success {
						fmt.Printf("FAILED (upload: %s)\n", putResult.Error)
						totalFail++
						continue
					}

					fmt.Printf("installing... ")

					// Exec install command.
					execBody, _ := json.Marshal(map[string]any{
						"agent_id": t.agentID,
						"command":  installCmd,
						"become":   true,
						"timeout":  timeout,
					})
					execResp, err := apiRequest("POST", "/api/v1/exec", bytes.NewReader(execBody))
					if err != nil {
						fmt.Printf("FAILED (install: %v)\n", err)
						totalFail++
						continue
					}
					var execResult struct {
						RC      int    `json:"rc"`
						Stdout  string `json:"stdout"`
						Stderr  string `json:"stderr"`
						Success bool   `json:"success"`
						Error   string `json:"error"`
					}
					json.Unmarshal(execResp, &execResult)
					if !execResult.Success || execResult.RC != 0 {
						errMsg := execResult.Error
						if errMsg == "" {
							errMsg = execResult.Stderr
						}
						fmt.Printf("FAILED (rc=%d: %s)\n", execResult.RC, errMsg)
						totalFail++
						continue
					}

					fmt.Println("OK")
					totalSuccess++
				}

				// Wait between waves for agents to reconnect (if not parallel and not last wave).
				if !parallel && wi < len(waves)-1 {
					fmt.Printf("  Waiting for wave %d agents to reconnect...\n", wi+1)
					time.Sleep(10 * time.Second)
				}
			}

			fmt.Printf("\nDeploy complete: %d succeeded, %d failed\n", totalSuccess, totalFail)
			if totalFail > 0 {
				return fmt.Errorf("%d deployment(s) failed", totalFail)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&parallel, "parallel", false, "deploy to all targets at once instead of rolling waves")
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
// Shared query helpers
// ─────────────────────────────────────────────────────────

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

	client := http.DefaultClient
	if tlsInsecure {
		client = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}
	}
	resp, err := client.Do(req)
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
