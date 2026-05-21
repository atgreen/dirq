// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// debugCmd returns the `dirq debug` command tree. Subcommands are
// diagnostic tools meant for troubleshooting specific issues — not
// routine health checks. (Use `dirq doctor` for setup verification and
// `dirq status` for fleet-wide snapshot.)
func debugCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "debug",
		Short: "Diagnostic tools for troubleshooting fleet, mesh, and request issues",
		Long: `Diagnostic tools for troubleshooting.

  inflight              List in-flight exec/query sessions on the server
  path <hostname>       Walk an agent's mesh parent chain (DB chain — fast)
  stream <hostname>     Server's in-memory view of how it would reach an agent
  ping <hostname>       Real mesh round-trip probe to an agent (slow, truthful)

The three lookup tools form a hierarchy of trust:
  path  — fastest, lies when DB is stale vs mesh reality
  stream — fast, lies when a relay's view of its downstreams disagrees with the server's
  ping  — slowest, but the only one that proves a message actually round-trips

Routine health checks live elsewhere: ` + "`dirq doctor`" + ` (operator setup) and
` + "`dirq status`" + ` (fleet snapshot).`,
	}

	cmd.AddCommand(debugInflightCmd())
	cmd.AddCommand(debugPathCmd())
	cmd.AddCommand(debugStreamCmd())
	cmd.AddCommand(debugPingCmd())
	return cmd
}

// ─────────────────────────────────────────────────────────
// dirq debug inflight
// ─────────────────────────────────────────────────────────

func debugInflightCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inflight",
		Short: "List in-flight exec/query sessions the server is currently waiting on",
		Long: `Lists every exec, query, put_file, and fetch_file session the dirq
server is currently coordinating. For broadcast operations the MISSING
column lists the agent IDs that haven't responded yet — answering the
"what is the server waiting for?" question without attaching a
debugger.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := apiRequest("GET", "/api/v1/debug/inflight", nil)
			if err != nil {
				return err
			}

			if jsonOut {
				fmt.Println(string(resp))
				return nil
			}

			var out struct {
				Sessions []struct {
					RequestID string   `json:"request_id"`
					Kind      string   `json:"kind"`
					Targets   int      `json:"targets"`
					Received  int      `json:"received"`
					Missing   []string `json:"missing"`
					ElapsedMS int64    `json:"elapsed_ms"`
					TimeoutMS int64    `json:"timeout_ms"`
				} `json:"sessions"`
			}
			if err := json.Unmarshal(resp, &out); err != nil {
				return err
			}

			if len(out.Sessions) == 0 {
				fmt.Println("No in-flight sessions.")
				return nil
			}

			// Sort longest-elapsed first — hung sessions float to the top.
			sort.Slice(out.Sessions, func(i, j int) bool {
				return out.Sessions[i].ElapsedMS > out.Sessions[j].ElapsedMS
			})

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "REQUEST_ID\tKIND\tTARGETS\tRECVD\tELAPSED\tTIMEOUT\tMISSING")
			for _, s := range out.Sessions {
				missing := "—"
				if len(s.Missing) > 0 {
					missing = formatMissing(s.Missing)
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\t%s\n",
					s.RequestID,
					s.Kind,
					s.Targets,
					s.Received,
					formatDuration(s.ElapsedMS),
					formatDuration(s.TimeoutMS),
					missing,
				)
			}
			w.Flush()
			return nil
		},
	}
}

// formatMissing shows the first few missing agent IDs and a "+N more"
// suffix if the list is long. The full list is in --json output.
func formatMissing(ids []string) string {
	const max = 3
	if len(ids) <= max {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s, +%d more", strings.Join(ids[:max], ", "), len(ids)-max)
}

func formatDuration(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < time.Second {
		return fmt.Sprintf("%dms", ms)
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}

// ─────────────────────────────────────────────────────────
// dirq debug path
// ─────────────────────────────────────────────────────────

func debugPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path <hostname>",
		Short: "Walk an agent's mesh parent chain and highlight unhealthy hops",
		Long: `Walks the agent's parent_id chain from leaf up to the zone leader and
prints each hop's online status, time since last heartbeat, and any
broken link in the chain. Useful when a single agent is unreachable or
hanging on exec — the chain shows whether the mesh path itself is
intact.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			start := args[0]
			seen := map[string]bool{}
			depth := 0
			cur := start
			brokenAt := ""

			type hop struct {
				hostname  string
				role      string
				online    bool
				lastSeen  time.Time
				parentID  string
				notFound  bool
			}
			var hops []hop

			for cur != "" {
				if seen[cur] {
					brokenAt = "cycle detected — chain loops back to " + cur
					break
				}
				seen[cur] = true

				resp, err := apiRequest("GET", "/api/v1/hosts/"+url.PathEscape(cur), nil)
				if err != nil {
					hops = append(hops, hop{hostname: cur, notFound: true})
					brokenAt = "agent not found in inventory"
					break
				}
				var h struct {
					Hostname   string    `json:"hostname"`
					Role       string    `json:"role"`
					Online     bool      `json:"online"`
					LastSeenAt time.Time `json:"last_seen_at"`
					ParentID   string    `json:"parent_id"`
					ID         string    `json:"id"`
				}
				if err := json.Unmarshal(resp, &h); err != nil {
					return err
				}
				hops = append(hops, hop{
					hostname: h.Hostname,
					role:     h.Role,
					online:   h.Online,
					lastSeen: h.LastSeenAt,
					parentID: h.ParentID,
				})
				if !h.Online && brokenAt == "" {
					brokenAt = h.Hostname + " is offline"
				}
				cur = h.ParentID
				depth++
				if depth > 50 {
					brokenAt = "chain too deep — bailing out at 50 hops"
					break
				}
			}

			if jsonOut {
				out, _ := json.Marshal(map[string]any{
					"path":      hops,
					"broken_at": brokenAt,
				})
				fmt.Println(string(out))
				return nil
			}

			now := time.Now()
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "STATUS\tHOSTNAME\tROLE\tLAST SEEN")
			for _, h := range hops {
				if h.notFound {
					fmt.Fprintf(w, "✗\t%s\t(not found)\t—\n", h.hostname)
					continue
				}
				status := "✓"
				if !h.online {
					status = "✗"
				}
				role := h.role
				if role == "" {
					role = "leaf"
				}
				lastSeen := "never"
				if !h.lastSeen.IsZero() {
					lastSeen = fmt.Sprintf("%s ago", formatDuration(now.Sub(h.lastSeen).Milliseconds()))
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", status, h.hostname, role, lastSeen)
			}
			w.Flush()

			fmt.Println()
			if brokenAt != "" {
				fmt.Printf("⚠ Broken link: %s\n", brokenAt)
				fmt.Println("  Requests to this agent may not reach it via the mesh until the chain is healed.")
				return nil
			}
			fmt.Printf("✓ Mesh path looks healthy (%d hop(s) per the DB).\n", len(hops))
			fmt.Println("  This is a DB-consistency check. To verify the mesh actually agrees, run:")
			fmt.Printf("    dirq debug ping %s\n", start)
			return nil
		},
	}
}

// ─────────────────────────────────────────────────────────
// dirq debug stream
// ─────────────────────────────────────────────────────────

func debugStreamCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stream <hostname>",
		Short: "Show the server's in-memory view of how it would reach an agent",
		Long: `Reports the server's live stream state for an agent: whether the agent
is directly connected (in s.streams), how full its send buffer is, and
— if it's not direct — which zone leader the server would route
through and whether that zone leader's stream is itself healthy.

Distinct from ` + "`dirq debug path`" + ` (which walks the DB chain): this checks
the in-memory state of the running server process, not the database.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := apiRequest("GET", "/api/v1/debug/stream/"+url.PathEscape(args[0]), nil)
			if err != nil {
				return err
			}
			if jsonOut {
				fmt.Println(string(resp))
				return nil
			}
			var out struct {
				AgentID           string `json:"agent_id"`
				Hostname          string `json:"hostname"`
				DirectlyConnected bool   `json:"directly_connected"`
				SendBufferUsed    int    `json:"send_buffer_used"`
				SendBufferCap     int    `json:"send_buffer_cap"`
				Reassigned        bool   `json:"reassigned"`
				RouteVia          string `json:"route_via"`
				RouteViaHostname  string `json:"route_via_hostname"`
				RouteViaConnected bool   `json:"route_via_connected"`
				Note              string `json:"note"`
			}
			if err := json.Unmarshal(resp, &out); err != nil {
				return err
			}
			fmt.Printf("Agent:    %s  (id %s)\n", out.Hostname, out.AgentID)
			if out.DirectlyConnected {
				fmt.Printf("Status:   ✓ directly connected to this server\n")
				fmt.Printf("Buffer:   %d / %d messages\n", out.SendBufferUsed, out.SendBufferCap)
				if out.Reassigned {
					fmt.Println("          (marked reassigned — disconnect won't flip offline)")
				}
				return nil
			}
			fmt.Printf("Status:   not directly connected\n")
			if out.RouteVia != "" {
				marker := "✓"
				if !out.RouteViaConnected {
					marker = "✗"
				}
				fmt.Printf("Route:    via %s %s  (id %s)\n", out.RouteViaHostname, marker, out.RouteVia)
			}
			if out.Note != "" {
				fmt.Printf("Note:     %s\n", out.Note)
			}
			return nil
		},
	}
}

// ─────────────────────────────────────────────────────────
// dirq debug ping
// ─────────────────────────────────────────────────────────

func debugPingCmd() *cobra.Command {
	var timeoutSec int
	cmd := &cobra.Command{
		Use:   "ping <hostname>",
		Short: "Send a real round-trip probe through the mesh and measure timing",
		Long: `Sends a no-op exec (` + "`true`" + ` on Linux/macOS, ` + "`exit 0`" + ` on Windows) to the
named agent through the mesh and reports round-trip timing. This is
the only diagnostic that proves a message can actually reach the
agent right now — unlike ` + "`path`" + ` and ` + "`stream`" + `, ping doesn't depend on
the DB chain or the server's in-memory state being correct. If the
agent is reachable through any path in the mesh, ping succeeds.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := fmt.Sprintf("/api/v1/debug/ping/%s?timeout=%d", url.PathEscape(args[0]), timeoutSec)
			resp, err := apiRequest("POST", path, nil)
			if err != nil {
				return err
			}
			if jsonOut {
				fmt.Println(string(resp))
				return nil
			}
			var out struct {
				AgentID      string `json:"agent_id"`
				Hostname     string `json:"hostname"`
				Success      bool   `json:"success"`
				RC           int    `json:"rc"`
				ElapsedMS    int64  `json:"elapsed_ms"`
				DispatchPath string `json:"dispatch_path"`
				Error        string `json:"error"`
			}
			if err := json.Unmarshal(resp, &out); err != nil {
				return err
			}
			marker := "✓"
			verdict := "reachable"
			if !out.Success {
				marker = "✗"
				verdict = "unreachable"
			}
			fmt.Printf("%s %s  %s  (%s, via %s dispatch)\n",
				marker, out.Hostname, verdict,
				formatDuration(out.ElapsedMS),
				out.DispatchPath,
			)
			if out.Error != "" {
				fmt.Printf("  error: %s\n", out.Error)
			}
			if out.DispatchPath == "fanout" && out.Success {
				fmt.Println("  (mesh found the agent via fan-out — the direct route may be stale)")
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&timeoutSec, "timeout", 10, "timeout in seconds")
	return cmd
}
