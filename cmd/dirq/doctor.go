// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/atgreen/dirq/internal/config"
	"github.com/spf13/cobra"
)

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

			check("Database", func() (string, string) {
				resp, err := apiRequest("GET", "/api/v1/status", nil)
				if err != nil {
					return "fail", err.Error()
				}
				var status struct {
					Database     bool   `json:"database"`
					DatabaseKind string `json:"database_kind"`
				}
				json.Unmarshal(resp, &status)
				kind := status.DatabaseKind
				if kind == "" {
					kind = "unknown"
				}
				if !status.Database {
					return "fail", kind + " connection failed"
				}
				return "ok", kind + " connected"
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
					return "warn", detail + " (none online)"
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

			check("ansible-playbook", func() (string, string) {
				out, err := exec.Command("ansible-playbook", "--version").Output()
				if err != nil {
					return "warn", "not found (dirq run won't work)"
				}
				lines := strings.SplitN(string(out), "\n", 2)
				version := strings.TrimSpace(lines[0])
				if v := parseAnsibleCoreVersion(version); v != "" {
					if !ansibleVersionAtLeast(v, "2.15") {
						return "warn", fmt.Sprintf("%s (minimum 2.15 required)", version)
					}
				}
				return "ok", version
			})

			check("ansible", func() (string, string) {
				out, err := exec.Command("ansible", "--version").Output()
				if err != nil {
					return "warn", "not found (dirq run --module won't work)"
				}
				lines := strings.SplitN(string(out), "\n", 2)
				return "ok", strings.TrimSpace(lines[0])
			})

			check("Connection plugin", func() (string, string) {
				dir := connectionPluginDir()
				if dir == "" {
					return "warn", "not found (dirq run won't work without it)"
				}
				pluginFile := filepath.Join(dir, "dirq.py")
				if _, err := os.Stat(pluginFile); err != nil {
					return "warn", fmt.Sprintf("directory found but dirq.py missing: %s", dir)
				}
				return "ok", pluginFile
			})

			// ── Config file ──

			check("Client config", func() (string, string) {
				validKeys := map[string]bool{
					"server_url":   true,
					"token":        true,
					"tls_insecure": true,
					"llm_url":      true,
					"llm_api_key":  true,
					"llm_model":    true,
				}

				var cfgPath string
				candidates := []string{}
				if home, err := os.UserHomeDir(); err == nil {
					candidates = append(candidates, filepath.Join(home, ".config", "dirq", "client.conf"))
				}
				candidates = append(candidates, "/etc/dirq/client.conf")

				for _, p := range candidates {
					if _, err := os.Stat(p); err == nil {
						cfgPath = p
						break
					}
				}

				if cfgPath == "" {
					return "ok", "no client.conf found (using env vars or defaults)"
				}

				cfg, err := config.Load(cfgPath)
				if err != nil {
					return "fail", fmt.Sprintf("cannot read %s: %v", cfgPath, err)
				}

				var unknown []string
				for k := range cfg.Values {
					if !validKeys[k] {
						unknown = append(unknown, k)
					}
				}

				if len(unknown) > 0 {
					sort.Strings(unknown)
					return "warn", fmt.Sprintf("%s: unknown key(s): %s", cfgPath, strings.Join(unknown, ", "))
				}
				return "ok", cfgPath
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
