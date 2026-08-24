// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/spf13/cobra"
)

func graphCmd() *cobra.Command {
	var dotOut bool

	cmd := &cobra.Command{
		Use:   "graph",
		Short: "Show the agent topology tree",
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
				ID       string  `json:"id"`
				Hostname string  `json:"hostname"`
				Role     string  `json:"role"`
				ParentID *string `json:"parent_id"`
				Online   bool    `json:"online"`
			}
			if err := json.Unmarshal(resp, &hosts); err != nil {
				return err
			}

			// Build lookup maps.
			type node struct {
				hostname string
				role     string
				online   bool
				children []string // child IDs, sorted by hostname
			}
			nodes := make(map[string]*node, len(hosts))
			var roots []string
			for _, h := range hosts {
				nodes[h.ID] = &node{hostname: h.Hostname, role: h.Role, online: h.Online}
			}
			for _, h := range hosts {
				if h.ParentID != nil && *h.ParentID != "" {
					if p, ok := nodes[*h.ParentID]; ok {
						p.children = append(p.children, h.ID)
					} else {
						roots = append(roots, h.ID)
					}
				} else {
					roots = append(roots, h.ID)
				}
			}

			// Sort children and roots by hostname.
			sortByHostname := func(ids []string) {
				sort.Slice(ids, func(i, j int) bool {
					return nodes[ids[i]].hostname < nodes[ids[j]].hostname
				})
			}
			sortByHostname(roots)
			for _, n := range nodes {
				sortByHostname(n.children)
			}

			if dotOut {
				fmt.Println("digraph dirq {")
				fmt.Println("  rankdir=LR;")
				fmt.Println("  node [shape=box, style=filled, fontname=\"Helvetica\"];")
				fmt.Println("  \"dirq-server\" [shape=diamond, fillcolor=\"#4a90d9\", fontcolor=white];")
				for id, n := range nodes {
					color := "#90ee90" // green for online
					if !n.online {
						color = "#d3d3d3" // grey for offline
					}
					fmt.Printf("  %q [label=%q, fillcolor=%q];\n", id, n.hostname, color)
				}
				for _, id := range roots {
					fmt.Printf("  \"dirq-server\" -> %q;\n", id)
				}
				for id, n := range nodes {
					for _, childID := range n.children {
						fmt.Printf("  %q -> %q;\n", id, childID)
					}
				}
				fmt.Println("}")
				return nil
			}

			// Print tree.
			fmt.Println("dirq-server")
			var printTree func(ids []string, prefix string)
			printTree = func(ids []string, prefix string) {
				for i, id := range ids {
					n := nodes[id]
					last := i == len(ids)-1

					connector := "├── "
					if last {
						connector = "└── "
					}

					status := "●"
					if !n.online {
						status = "○"
					}

					fmt.Printf("%s%s%s %s\n", prefix, connector, status, n.hostname)

					childPrefix := prefix + "│   "
					if last {
						childPrefix = prefix + "    "
					}
					printTree(n.children, childPrefix)
				}
			}
			printTree(roots, "")

			return nil
		},
	}

	cmd.Flags().BoolVar(&dotOut, "dot", false, "output in Graphviz DOT format")
	return cmd
}

// ─────────────────────────────────────────────────────────
// dirq token
// ─────────────────────────────────────────────────────────
