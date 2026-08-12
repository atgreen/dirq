// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// execCmd returns the `dirq exec` command for running commands on remote hosts.
func execCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "exec <hostname> <command>",
		Short: "Execute a command on a remote host and stream output in real time",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			hostname := args[0]
			command := strings.Join(args[1:], " ")

			payload := map[string]string{
				"hostname": hostname,
				"command":  command,
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}

			url := serverURL + "/api/v1/exec"
			req, err := http.NewRequest("POST", url, strings.NewReader(string(body)))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				errBody, _ := io.ReadAll(resp.Body)
				return err
			}

			// Stream stdout/stderr back in chunks for long-running commands
			// instead of waiting for completion.
			_, err = io.Copy(os.Stdout, resp.Body)
			return err
		},
	}
}
