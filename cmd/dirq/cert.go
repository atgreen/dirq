// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/atgreen/dirq/internal/tlsutil"
	"github.com/spf13/cobra"
)

func certCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cert",
		Short: "Certificate and key management",
	}

	var (
		dir       string
		caFile    string
		caKeyFile string
	)
	generateCmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate server and agent certificates",
		Long: `Generate TLS certificates for DirQ server and agents.

By default, creates a new self-signed CA and uses it to sign the
server and agent certificates. Use --ca and --ca-key to sign with
your own CA instead.

All files are written to the specified directory:
  ca.crt               — CA certificate (copied from --ca, or generated)
  server.crt, server.key — DirQ server
  agent.crt, agent.key   — Bootstrap agent cert (for initial registration only)

With mTLS enabled (default when CA key is available), each agent receives
a unique client certificate during registration with its agent ID as the CN.
The bootstrap agent.crt is only used for the initial TLS handshake.

Examples:
  dirq cert generate
  dirq cert generate --dir ./certs
  dirq cert generate --ca ./my-ca.crt --ca-key ./my-ca.key`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var result *tlsutil.GenerateResult
			var err error

			if caFile != "" && caKeyFile != "" {
				caCert, caKey, loadErr := tlsutil.LoadCA(caFile, caKeyFile)
				if loadErr != nil {
					return fmt.Errorf("load CA: %w", loadErr)
				}
				result, err = tlsutil.GenerateWithCA(dir, caCert, caKey, caKeyFile)
			} else if caFile != "" || caKeyFile != "" {
				return fmt.Errorf("both --ca and --ca-key are required")
			} else {
				result, err = tlsutil.GenerateSelfSigned(dir)
			}
			if err != nil {
				return err
			}

			fmt.Println("Certificates generated:")
			fmt.Printf("  CA:     %s\n", result.CAFile)
			fmt.Printf("  Server: %s (key: %s)\n", result.ServerCertFile, result.ServerKeyFile)
			fmt.Printf("  Agent:  %s (key: %s)\n", result.AgentCertFile, result.AgentKeyFile)
			return nil
		},
	}
	generateCmd.Flags().StringVar(&dir, "dir", "./certs", "output directory for certificates")
	generateCmd.Flags().StringVar(&caFile, "ca", "", "use existing CA certificate (requires --ca-key)")
	generateCmd.Flags().StringVar(&caKeyFile, "ca-key", "", "use existing CA private key (requires --ca)")

	var stagger int32
	rotateSubCmd := &cobra.Command{
		Use:   "rotate [type]",
		Short: "Trigger certificate or key rotation across the fleet",
		Long: `Rotate certificates or signing keys across the entire fleet.

Types:
  agent_cert    Renew all agent mTLS certificates
  signing_key   Roll the server's message signing key to all agents
  ca            Distribute a new CA to all agents

Use --stagger to spread renewals over time and avoid overloading the
server. Each agent waits a random delay before renewing.

Examples:
  dirq cert rotate agent_cert
  dirq cert rotate agent_cert --stagger 3600
  dirq cert rotate ca --stagger 1800`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rotationType := args[0]
			switch rotationType {
			case "agent_cert", "signing_key", "ca":
			default:
				return fmt.Errorf("unknown rotation type %q: must be agent_cert, signing_key, or ca", rotationType)
			}

			body, _ := json.Marshal(map[string]any{
				"type":            rotationType,
				"stagger_seconds": stagger,
			})
			resp, err := apiRequest("POST", "/api/v1/rotate", bytes.NewReader(body))
			if err != nil {
				return err
			}

			var result map[string]any
			json.Unmarshal(resp, &result)
			zl := result["zone_leaders"]
			fmt.Printf("Rotation %q broadcast to %.0f zone leader(s)", rotationType, zl)
			if stagger > 0 {
				fmt.Printf(" (staggered over %ds)", stagger)
			}
			fmt.Println()
			return nil
		},
	}
	rotateSubCmd.Flags().Int32Var(&stagger, "stagger", 0, "spread rotation over N seconds (recommended for large fleets)")

	cmd.AddCommand(generateCmd, rotateSubCmd)
	return cmd
}

// ─────────────────────────────────────────────────────────
// dirq run
// ─────────────────────────────────────────────────────────
