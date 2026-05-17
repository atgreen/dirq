// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

func mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Start MCP (Model Context Protocol) stdio server",
		Long: `Start an MCP server over stdio, exposing dirq fleet management
as tools that LLMs (Claude, etc.) can call directly.

Configure in Claude Desktop's claude_desktop_config.json:

  {
    "mcpServers": {
      "dirq": {
        "command": "dirq",
        "args": ["mcp"],
        "env": {
          "DIRQ_SERVER_URL": "https://your-server:8080",
          "DIRQ_TOKEN": "your-token"
        }
      }
    }
  }`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMCPServer()
		},
	}
}

func runMCPServer() error {
	s := mcpserver.NewMCPServer("dirq", version,
		mcpserver.WithToolCapabilities(true),
	)

	s.AddTool(mcpHostsListTool(), handleMCPHostsList)
	s.AddTool(mcpHostsShowTool(), handleMCPHostsShow)
	s.AddTool(mcpHostsFactsTool(), handleMCPHostsFacts)
	s.AddTool(mcpHostsTagTool(), handleMCPHostsTag)
	s.AddTool(mcpQueryTool(), handleMCPQuery)
	s.AddTool(mcpExecTool(), handleMCPExec)
	s.AddTool(mcpCVETool(), handleMCPCVE)
	s.AddTool(mcpErrataTool(), handleMCPErrata)
	s.AddTool(mcpKBTool(), handleMCPKB)
	s.AddTool(mcpGraphTool(), handleMCPGraph)

	return mcpserver.ServeStdio(s)
}

// ─────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────

// mcpAPIGet makes a GET request and returns the JSON response as a pretty string.
func mcpAPIGet(path string) (string, error) {
	resp, err := apiRequest("GET", path, nil)
	if err != nil {
		return "", err
	}
	return mcpPrettyJSON(resp), nil
}

// mcpAPIPost makes a POST request and returns the JSON response as a pretty string.
func mcpAPIPost(path string, payload any) (string, error) {
	body, _ := json.Marshal(payload)
	resp, err := apiRequest("POST", path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	return mcpPrettyJSON(resp), nil
}

// mcpStreamPost makes a POST request and collects all streamed JSON lines.
func mcpStreamPost(path string, payload any) (string, error) {
	body, _ := json.Marshal(payload)
	resp, err := apiStreamRequest("POST", path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
	}

	// Collect all JSON lines from the stream.
	var results []json.RawMessage
	dec := json.NewDecoder(resp.Body)
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			break
		}
		results = append(results, raw)
	}

	out, _ := json.MarshalIndent(results, "", "  ")
	return string(out), nil
}

func mcpPrettyJSON(data []byte) string {
	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil {
		return string(data)
	}
	return out.String()
}

func mcpCheckServer() error {
	if serverURL == "" {
		return fmt.Errorf("DIRQ_SERVER_URL is not set. Set it in the environment or in client.conf")
	}
	return nil
}

func mcpStringArg(req mcp.CallToolRequest, key string) string {
	args := req.GetArguments()
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func mcpIntArg(req mcp.CallToolRequest, key string, defaultVal int) int {
	args := req.GetArguments()
	if v, ok := args[key].(float64); ok {
		return int(v)
	}
	return defaultVal
}

func mcpBoolArg(req mcp.CallToolRequest, key string) bool {
	args := req.GetArguments()
	if v, ok := args[key].(bool); ok {
		return v
	}
	return false
}

// ─────────────────────────────────────────────────────────
// Tool: hosts_list
// ─────────────────────────────────────────────────────────

func mcpHostsListTool() mcp.Tool {
	return mcp.NewTool("dirq_hosts_list",
		mcp.WithDescription("List all registered hosts in the fleet. Returns hostname, OS, online status, tags, and agent ID for each host. Optionally filter with a WHERE clause."),
		mcp.WithString("where", mcp.Description("Optional DirQ WHERE clause to filter hosts, e.g. \"tag.env = 'prod'\" or \"os_info.os = 'windows'\"")),
	)
}

func handleMCPHostsList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := mcpCheckServer(); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	where := mcpStringArg(req, "where")
	path := "/api/v1/hosts"
	if where != "" {
		path += "?q=" + where
	}

	result, err := mcpAPIGet(path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(result), nil
}

// ─────────────────────────────────────────────────────────
// Tool: hosts_show
// ─────────────────────────────────────────────────────────

func mcpHostsShowTool() mcp.Tool {
	return mcp.NewTool("dirq_hosts_show",
		mcp.WithDescription("Show detailed information about a specific host including system facts, tags, connectivity, and zone leader status."),
		mcp.WithString("host_id", mcp.Required(), mcp.Description("The agent ID or hostname of the host to show")),
	)
}

func handleMCPHostsShow(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := mcpCheckServer(); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	hostID := mcpStringArg(req, "host_id")
	result, err := mcpAPIGet("/api/v1/hosts/" + hostID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(result), nil
}

// ─────────────────────────────────────────────────────────
// Tool: hosts_facts
// ─────────────────────────────────────────────────────────

func mcpHostsFactsTool() mcp.Tool {
	return mcp.NewTool("dirq_hosts_facts",
		mcp.WithDescription("Get real-time system facts for a host: CPU, memory, disk, network, OS details, installed packages, and more."),
		mcp.WithString("host_id", mcp.Required(), mcp.Description("The agent ID or hostname of the host")),
	)
}

func handleMCPHostsFacts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := mcpCheckServer(); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	hostID := mcpStringArg(req, "host_id")
	result, err := mcpAPIGet("/api/v1/hosts/" + hostID + "/facts")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(result), nil
}

// ─────────────────────────────────────────────────────────
// Tool: hosts_tag
// ─────────────────────────────────────────────────────────

func mcpHostsTagTool() mcp.Tool {
	return mcp.NewTool("dirq_hosts_tag",
		mcp.WithDescription("Add or update tags on hosts. Tags are key=value pairs used for fleet organization and targeting. Use either a host_id for a single host or a where clause for bulk tagging."),
		mcp.WithObject("tags", mcp.Required(), mcp.Description("Tags to set as key-value pairs, e.g. {\"env\": \"prod\", \"role\": \"webserver\"}")),
		mcp.WithString("host_id", mcp.Description("Agent ID of a single host to tag")),
		mcp.WithString("where", mcp.Description("DirQ WHERE clause to select hosts for bulk tagging")),
	)
}

func handleMCPHostsTag(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := mcpCheckServer(); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	tagsRaw, ok := req.GetArguments()["tags"].(map[string]any)
	if !ok || len(tagsRaw) == 0 {
		return mcp.NewToolResultError("tags parameter is required and must be a JSON object"), nil
	}

	tags := make(map[string]string)
	for k, v := range tagsRaw {
		tags[k] = fmt.Sprintf("%v", v)
	}

	hostID := mcpStringArg(req, "host_id")
	where := mcpStringArg(req, "where")

	if hostID == "" && where == "" {
		return mcp.NewToolResultError("provide either host_id or where clause"), nil
	}

	var hostIDs []string
	var hostNames []string

	if hostID != "" {
		hostIDs = []string{hostID}
	} else {
		hosts, err := runQuery(where, 60)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if len(hosts) == 0 {
			return mcp.NewToolResultText("No hosts matched the query."), nil
		}
		for _, h := range hosts {
			hostIDs = append(hostIDs, h.agentID)
			hostNames = append(hostNames, h.hostname)
		}
	}

	body, _ := json.Marshal(tags)
	var results []string
	for i, id := range hostIDs {
		resp, err := apiRequest("PATCH", "/api/v1/hosts/"+id+"/tags", bytes.NewReader(body))
		if err != nil {
			name := id
			if i < len(hostNames) {
				name = hostNames[i]
			}
			results = append(results, fmt.Sprintf("%s: FAILED: %v", name, err))
			continue
		}
		var agent struct {
			Hostname string            `json:"hostname"`
			Tags     map[string]string `json:"tags"`
		}
		json.Unmarshal(resp, &agent)
		results = append(results, fmt.Sprintf("%s: tags updated", agent.Hostname))
	}

	return mcp.NewToolResultText(strings.Join(results, "\n")), nil
}

// ─────────────────────────────────────────────────────────
// Tool: query (select)
// ─────────────────────────────────────────────────────────

func mcpQueryTool() mcp.Tool {
	return mcp.NewTool("dirq_query",
		mcp.WithDescription(`Query the fleet using the DirQ query language. Returns structured data from hosts matching the query.

Example queries:
  SELECT hostname, os_info.os WHERE tag.env = 'prod'
  SELECT hostname, cpu.cores, memory.total_mb WHERE os_info.os = 'linux'
  SELECT hostname, packages.name, packages.version WHERE packages.name = 'openssl'
  SELECT hostname, disk.mount, disk.pct_used WHERE disk.pct_used > 80`),
		mcp.WithString("query", mcp.Required(), mcp.Description("DirQ SELECT query string")),
		mcp.WithNumber("timeout", mcp.Description("Query timeout in seconds (default 60)")),
	)
}

func handleMCPQuery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := mcpCheckServer(); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	query := mcpStringArg(req, "query")
	timeout := mcpIntArg(req, "timeout", 60)

	result, err := mcpStreamPost("/api/v1/query", map[string]any{
		"query":   query,
		"timeout": timeout,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(result), nil
}

// ─────────────────────────────────────────────────────────
// Tool: exec
// ─────────────────────────────────────────────────────────

func mcpExecTool() mcp.Tool {
	return mcp.NewTool("dirq_exec",
		mcp.WithDescription("Execute a shell command across fleet hosts. Returns stdout, stderr, and exit code from each host. On Linux hosts the command runs via sh, on Windows via cmd (or PowerShell if the command starts with 'powershell')."),
		mcp.WithString("command", mcp.Required(), mcp.Description("Shell command to execute")),
		mcp.WithString("where", mcp.Description("DirQ WHERE clause to target specific hosts, e.g. \"tag.env = 'prod'\"")),
		mcp.WithBoolean("become", mcp.Description("Run with privilege escalation (sudo on Linux, SYSTEM on Windows)")),
		mcp.WithNumber("timeout", mcp.Description("Command timeout in seconds (default 60)")),
	)
}

func handleMCPExec(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := mcpCheckServer(); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	command := mcpStringArg(req, "command")
	where := mcpStringArg(req, "where")
	become := mcpBoolArg(req, "become")
	timeout := mcpIntArg(req, "timeout", 60)

	payload := map[string]any{
		"command": command,
		"become":  become,
		"timeout": timeout,
	}
	if where != "" {
		payload["query"] = where
	}

	result, err := mcpStreamPost("/api/v1/exec", payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(result), nil
}

// ─────────────────────────────────────────────────────────
// Tool: cve
// ─────────────────────────────────────────────────────────

func mcpCVETool() mcp.Tool {
	return mcp.NewTool("dirq_cve_scan",
		mcp.WithDescription("Scan the fleet for a specific CVE vulnerability. Fetches CVE data from Red Hat Security Data API, identifies affected packages and fixed versions, then checks which RHEL hosts in the fleet are running vulnerable versions."),
		mcp.WithString("cve_id", mcp.Required(), mcp.Description("CVE identifier, e.g. CVE-2024-6345")),
		mcp.WithString("where", mcp.Description("DirQ WHERE clause to limit scan scope")),
		mcp.WithNumber("timeout", mcp.Description("Scan timeout in seconds (default 60)")),
	)
}

func handleMCPCVE(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := mcpCheckServer(); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	cveID := strings.ToUpper(mcpStringArg(req, "cve_id"))
	where := mcpStringArg(req, "where")
	timeout := mcpIntArg(req, "timeout", 60)

	// Fetch CVE data from Red Hat.
	cveURL := "https://access.redhat.com/hydra/rest/securitydata/cve/" + cveID + ".json"
	resp, err := http.Get(cveURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to fetch CVE data: %v", err)), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return mcp.NewToolResultError(fmt.Sprintf("CVE %s not found in Red Hat Security Data", cveID)), nil
	}
	if resp.StatusCode != 200 {
		return mcp.NewToolResultError(fmt.Sprintf("Red Hat API returned HTTP %d", resp.StatusCode)), nil
	}

	cveBody, _ := io.ReadAll(resp.Body)

	var cveData struct {
		Name           string `json:"name"`
		ThreatSeverity string `json:"threat_severity"`
		Bugzilla       struct {
			Description string `json:"description"`
		} `json:"bugzilla"`
		AffectedRelease []struct {
			Package string `json:"package"`
			CPE     string `json:"cpe"`
		} `json:"affected_release"`
		PackageState []struct {
			FixState    string `json:"fix_state"`
			PackageName string `json:"package_name"`
			CPE         string `json:"cpe"`
		} `json:"package_state"`
	}
	if err := json.Unmarshal(cveBody, &cveData); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("parse CVE data: %v", err)), nil
	}

	// Build summary with CVE info.
	var sb strings.Builder
	fmt.Fprintf(&sb, "CVE: %s\n", cveID)
	fmt.Fprintf(&sb, "Description: %s\n", cveData.Bugzilla.Description)
	fmt.Fprintf(&sb, "Severity: %s\n\n", cveData.ThreatSeverity)

	// Extract affected package names.
	pkgNames := map[string]bool{}
	for _, ar := range cveData.AffectedRelease {
		if ar.Package != "" && strings.Contains(ar.CPE, "enterprise_linux") {
			name, _ := parseRPMNEVRA(ar.Package)
			if name != "" && !strings.HasPrefix(name, "kpatch") {
				pkgNames[name] = true
			}
		}
	}
	for _, ps := range cveData.PackageState {
		if ps.FixState == "Affected" && strings.Contains(ps.CPE, "enterprise_linux") {
			if ps.PackageName != "" && !strings.HasPrefix(ps.PackageName, "kpatch") {
				pkgNames[ps.PackageName] = true
			}
		}
	}

	if len(pkgNames) == 0 {
		sb.WriteString("No RHEL packages associated with this CVE.\n")
		return mcp.NewToolResultText(sb.String()), nil
	}

	names := make([]string, 0, len(pkgNames))
	for n := range pkgNames {
		names = append(names, n)
	}
	fmt.Fprintf(&sb, "Affected packages: %s\n\n", strings.Join(names, ", "))

	// Query the fleet for these packages.
	inList := make([]string, len(names))
	for i, n := range names {
		inList[i] = "'" + n + "'"
	}
	query := fmt.Sprintf("SELECT hostname, packages.name, packages.version WHERE packages.name IN (%s)", strings.Join(inList, ", "))
	if where != "" {
		query += " AND " + where
	}

	fleetResult, err := mcpStreamPost("/api/v1/query", map[string]any{
		"query":   query,
		"timeout": timeout,
	})
	if err != nil {
		fmt.Fprintf(&sb, "Fleet scan failed: %v\n", err)
	} else {
		fmt.Fprintf(&sb, "Fleet scan results:\n%s\n", fleetResult)
	}

	return mcp.NewToolResultText(sb.String()), nil
}

// ─────────────────────────────────────────────────────────
// Tool: errata
// ─────────────────────────────────────────────────────────

func mcpErrataTool() mcp.Tool {
	return mcp.NewTool("dirq_errata_check",
		mcp.WithDescription("Check the fleet against a Red Hat advisory (RHSA/RHBA/RHEA). Fetches advisory data, identifies CVEs and fixed packages, and reports which RHEL hosts are patched or vulnerable."),
		mcp.WithString("advisory_id", mcp.Required(), mcp.Description("Red Hat advisory ID, e.g. RHSA-2024:1234")),
		mcp.WithString("where", mcp.Description("DirQ WHERE clause to limit check scope")),
		mcp.WithNumber("timeout", mcp.Description("Check timeout in seconds (default 60)")),
	)
}

func handleMCPErrata(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := mcpCheckServer(); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	advisoryID := strings.ToUpper(mcpStringArg(req, "advisory_id"))
	where := mcpStringArg(req, "where")
	timeout := mcpIntArg(req, "timeout", 60)

	payload := map[string]any{
		"advisory_id": advisoryID,
		"timeout":     timeout,
	}
	if where != "" {
		payload["query"] = where
	}

	result, err := mcpStreamPost("/api/v1/errata", payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(result), nil
}

// ─────────────────────────────────────────────────────────
// Tool: kb
// ─────────────────────────────────────────────────────────

func mcpKBTool() mcp.Tool {
	return mcp.NewTool("dirq_kb_check",
		mcp.WithDescription("Check Windows hosts for installed hotfixes (KB articles). Reports which hosts have or are missing specific KBs."),
		mcp.WithString("kb_ids", mcp.Required(), mcp.Description("Comma-separated KB IDs, e.g. \"KB5034441,KB5034439\"")),
		mcp.WithString("where", mcp.Description("DirQ WHERE clause to limit check scope")),
		mcp.WithNumber("timeout", mcp.Description("Check timeout in seconds (default 60)")),
	)
}

func handleMCPKB(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := mcpCheckServer(); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	kbIDsStr := mcpStringArg(req, "kb_ids")
	where := mcpStringArg(req, "where")
	timeout := mcpIntArg(req, "timeout", 60)

	kbIDs := strings.Split(kbIDsStr, ",")
	for i := range kbIDs {
		kbIDs[i] = strings.TrimSpace(kbIDs[i])
	}

	payload := map[string]any{
		"kb_ids":  kbIDs,
		"timeout": timeout,
	}
	if where != "" {
		payload["query"] = where
	}

	result, err := mcpStreamPost("/api/v1/kb", payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(result), nil
}

// ─────────────────────────────────────────────────────────
// Tool: graph
// ─────────────────────────────────────────────────────────

func mcpGraphTool() mcp.Tool {
	return mcp.NewTool("dirq_graph",
		mcp.WithDescription("Show the fleet topology as a tree. Displays the hierarchical mesh structure: server -> zone leaders -> relay agents -> leaf agents."),
	)
}

func handleMCPGraph(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := mcpCheckServer(); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	result, err := mcpAPIGet("/api/v1/graph")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(result), nil
}
