// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/atgreen/dirq/internal/config"
	"github.com/spf13/cobra"
)

var version = "dev"

var (
	serverURL   string
	apiToken    string
	jsonOut     bool
	tlsInsecure bool
	tlsCA       string
)

func main() {
	// Flatten args: split multi-word quoted arguments that look like queries.
	// This lets users write dirq "select hostname where tag.env = 'prod'"
	// instead of dirq select hostname where tag.env = 'prod'
	// Only flatten args that start with SELECT — leave everything else
	// intact so "ls -l" doesn't get split into ls and -l (cobra flag).
	flatArgs := []string{}
	for _, arg := range os.Args[1:] {
		if strings.ContainsAny(arg, " \t") && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(arg)), "SELECT") {
			flatArgs = append(flatArgs, strings.Fields(arg)...)
		} else {
			flatArgs = append(flatArgs, arg)
		}
	}
	os.Args = append([]string{os.Args[0]}, flatArgs...)

	// Load client config file (missing file is fine).
	clientCfg, _ := config.Load(clientConfigPath())

	root := &cobra.Command{
		Use:          "dirq",
		Short:        fmt.Sprintf("DirQ — Real-Time Endpoint Query CLI (%s)", version),
		Version:      version,
		SilenceUsage: true,
	}

	root.PersistentFlags().StringVar(&serverURL, "server",
		config.EnvOr("DIRQ_SERVER_URL", clientCfg, "server_url", ""),
		"DirQ server URL (or set DIRQ_SERVER_URL)")
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// Apply config-file defaults for values not shown in help output.
		if apiToken == "" {
			apiToken = config.EnvOr("DIRQ_TOKEN", clientCfg, "token", "")
		}

		// Load the CA up front so a bad path fails here, with a clear
		// message, rather than surfacing as a confusing handshake error on
		// the first request — and never by silently falling back to the
		// system trust store, which would look like it worked.
		// Only complain about a superseded --tls-insecure when it was
		// asked for deliberately. Inheriting it from a stale config file
		// is the common case, and warning on every invocation for that
		// would train people to ignore the warning.
		insecureAsked := cmd.Root().PersistentFlags().Changed("tls-insecure") ||
			os.Getenv("DIRQ_TLS_INSECURE") != ""
		if err := loadTLSRoots(insecureAsked); err != nil {
			return err
		}

		// Allow tls generate, skill, and ask --dry-run to run without a server URL.
		if cmd.Name() == "generate" || cmd.Name() == "skill" || cmd.Name() == "doctor" || cmd.Name() == "mcp" {
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
	root.PersistentFlags().StringVar(&apiToken, "token", "", "API token")
	root.PersistentFlags().BoolVar(&jsonOut, "json", false, "output raw JSON")
	// LLM review configuration.
	reviewConfig.url = config.EnvOr("DIRQ_LLM_URL", clientCfg, "llm_url", "")
	reviewConfig.key = config.EnvOr("DIRQ_LLM_API_KEY", clientCfg, "llm_api_key", "")
	reviewConfig.model = config.EnvOr("DIRQ_LLM_MODEL", clientCfg, "llm_model", "claude-sonnet-4-5-20250514")

	root.PersistentFlags().BoolVar(&tlsInsecure, "tls-insecure",
		config.EnvOr("DIRQ_TLS_INSECURE", clientCfg, "tls_insecure", "false") == "true",
		"skip TLS certificate verification")
	// The server and agents both verify against a CA file; without the same
	// option here, the only way to talk to a default (self-signed)
	// deployment was to turn verification off entirely.  Same variable name
	// the server and agent use, so one value works across all three.
	// Registered so it shows up in help and is not rejected as unknown;
	// the value is read by clientConfigPath before flags are parsed.
	root.PersistentFlags().String("config", "",
		"client config file (default ~/.config/dirq/client.conf, or set DIRQ_CONFIG_FILE)")
	root.PersistentFlags().StringVar(&tlsCA, "tls-ca",
		config.EnvOr("DIRQ_TLS_CA", clientCfg, "tls_ca", ""),
		"CA certificate file used to verify the server (or set DIRQ_TLS_CA)")

	root.AddCommand(hostsCmd())
	root.AddCommand(tokenCmd())
	root.AddCommand(queriesCmd())
	root.AddCommand(certCmd())
	root.AddCommand(runCmd())
	root.AddCommand(execCmd())
	root.AddCommand(skillCmd())
	root.AddCommand(askCmd())
	root.AddCommand(selectCmd())
	root.AddCommand(deployCmd())
	root.AddCommand(doctorCmd())
	root.AddCommand(cveCmd())
	root.AddCommand(errataCmd())
	root.AddCommand(kbCmd())
	root.AddCommand(grepCmd())
	root.AddCommand(mcpCmd())
	root.AddCommand(debugCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// clientConfigPath decides which client config file to read. The CLI used
// to always read ~/.config/dirq/client.conf with no way to point it
// elsewhere, so anything invoking it inherited whatever the running user
// had configured — a real server URL, a real token, tls_insecure.
//
// Flag defaults are drawn from that file and cobra computes defaults
// before it parses, so this reads argv itself rather than going through
// the flag it registers.
func clientConfigPath() string {
	if p := os.Getenv("DIRQ_CONFIG_FILE"); p != "" {
		return p
	}
	for i, a := range os.Args {
		if a == "--config" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--config="); ok {
			return v
		}
	}
	return config.DefaultClientPath()
}
