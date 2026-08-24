// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

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
			// Flatten any quoted multi-word args containing WHERE
			// (e.g., 'where os_info.hostname="fedora"' → where, os_info.hostname="fedora").
			var flatArgs []string
			for _, a := range args {
				if strings.ContainsAny(a, " \t") {
					flatArgs = append(flatArgs, strings.Fields(a)...)
				} else {
					flatArgs = append(flatArgs, a)
				}
			}
			args = flatArgs

			// Determine playbook from first arg (if it's a file, not a WHERE keyword).
			var playbook string
			var whereArgs []string

			if len(args) > 0 && !strings.EqualFold(args[0], "WHERE") {
				// When --module or --command is set, don't consume the first arg
				// as a playbook — it's likely part of the WHERE clause.
				if module != "" || command != "" {
					whereArgs = args
				} else {
					playbook = args[0]
					whereArgs = args[1:]
				}
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
			if len(names) <= 10 {
				fmt.Printf("Query matched %d host(s): %s\n\n", len(hosts), strings.Join(names, ", "))
			} else {
				fmt.Printf("Query matched %d host(s): %s, ... and %d more\n\n", len(hosts), strings.Join(names[:10], ", "), len(hosts)-10)
			}

			// Auto-detect Python interpreters on Linux hosts that don't
			// have ansible_python_interpreter set. Ansible modules need
			// Python on the target.
			if err := discoverPythonInterpreters(hosts); err != nil {
				return err
			}

			invPath, err := writeInventory(hosts)
			if err != nil {
				return err
			}
			defer os.Remove(invPath)

			// LLM change review.
			sampleNames := names
			if len(sampleNames) > 10 {
				sampleNames = sampleNames[:10]
			}
			action := reviewAction{
				ActionType:  "playbook",
				TargetQuery: queryStr,
				TargetCount: len(hosts),
				Targets:     strings.Join(sampleNames, ", "),
				Module:      module,
				ModuleArgs:  moduleArgs,
			}
			if playbook != "" {
				action.PlaybookPath = playbook
				action.PlaybookFiles = gatherPlaybookFiles(playbook)
			}
			if command != "" {
				action.Command = command
			}
			if len(extraArgs) > 0 {
				action.ExtraArgs = strings.Join(extraArgs, " ")
			}
			if err := runReview(action); err != nil {
				return err
			}

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

			// Forward CLI settings to the Ansible connection plugin.
			// These may come from client.conf rather than env vars,
			// so always set them explicitly for the subprocess.
			if serverURL != "" {
				proc.Env = append(proc.Env, "DIRQ_SERVER_URL="+serverURL)
			}
			if apiToken != "" {
				proc.Env = append(proc.Env, "DIRQ_TOKEN="+apiToken)
			}
			if tlsInsecure {
				proc.Env = append(proc.Env, "DIRQ_TLS_INSECURE=true")
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
