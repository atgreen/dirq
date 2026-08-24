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
	"time"

	"github.com/spf13/cobra"
)

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

			// LLM change review.
			if err := runReview(reviewAction{
				ActionType:     "deploy",
				TargetQuery:    queryStr,
				PackagePath:    filepath.Base(pkgPath),
				InstallCommand: installCmd,
				DestPath:       destPath,
				Become:         true,
			}); err != nil {
				return err
			}

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
// dirq grep
// ─────────────────────────────────────────────────────────
