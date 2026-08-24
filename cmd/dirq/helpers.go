// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

func parseTagArgs(args []string) (tags map[string]string, whereArgs []string, hostID string) {
	tags = make(map[string]string)

	// Check if the first arg is a host ID (not key=value, not WHERE).
	startIdx := 0
	if len(args) > 0 && !strings.Contains(args[0], "=") && !strings.EqualFold(args[0], "WHERE") {
		hostID = args[0]
		startIdx = 1
	}

	for i := startIdx; i < len(args); i++ {
		if strings.EqualFold(args[i], "WHERE") {
			whereArgs = args[i:]
			break
		}
		parts := strings.SplitN(args[i], "=", 2)
		if len(parts) == 2 {
			tags[parts[0]] = parts[1]
		}
	}
	return
}

// parseUntagArgs splits args into tag keys, WHERE clause args, and an
// optional host ID. If the first arg isn't "WHERE" and later args include
// WHERE, or if no WHERE exists and there are 2+ args, the first arg is
// treated as a host ID (backwards-compatible).
func parseUntagArgs(args []string) (keys []string, whereArgs []string, hostID string) {
	// Find WHERE boundary.
	whereIdx := -1
	for i, a := range args {
		if strings.EqualFold(a, "WHERE") {
			whereIdx = i
			break
		}
	}

	if whereIdx >= 0 {
		whereArgs = args[whereIdx:]
		beforeWhere := args[:whereIdx]
		// If the first arg before WHERE looks like a host ID (UUID-like),
		// treat it as one. Otherwise all args before WHERE are tag keys.
		if len(beforeWhere) > 0 && looksLikeID(beforeWhere[0]) {
			hostID = beforeWhere[0]
			keys = beforeWhere[1:]
		} else {
			keys = beforeWhere
		}
	} else {
		// No WHERE — old syntax: first arg is host ID, rest are keys.
		if len(args) >= 2 {
			hostID = args[0]
			keys = args[1:]
		} else {
			keys = args
		}
	}
	return
}

// looksLikeID returns true if s looks like a UUID or agent ID rather than
// a simple tag key name. Agent IDs contain hyphens and are long.
func looksLikeID(s string) bool {
	return len(s) > 16 && strings.Contains(s, "-")
}

// dataField extracts a nested field from query result data.
// e.g. dataField(data, "os_info", "os") gets data["os_info"]["os"].
func dataField(data map[string]any, module, field string) string {
	if m, ok := data[module].(map[string]any); ok {
		if v, ok := m[field]; ok {
			return fmt.Sprintf("%v", v)
		}
	}
	// Also try flattened dotted key (e.g. "os_info.os").
	if v, ok := data[module+"."+field]; ok {
		return fmt.Sprintf("%v", v)
	}
	return ""
}

// parseAnsibleCoreVersion extracts the version from ansible-playbook output.
// Input: "ansible-playbook [core 2.20.5]" → "2.20.5"
func parseAnsibleCoreVersion(line string) string {
	start := strings.Index(line, "[core ")
	if start < 0 {
		return ""
	}
	start += len("[core ")
	end := strings.Index(line[start:], "]")
	if end < 0 {
		return ""
	}
	return line[start : start+end]
}

// ansibleVersionAtLeast checks if version >= minimum using simple major.minor comparison.
func ansibleVersionAtLeast(version, minimum string) bool {
	vParts := strings.SplitN(version, ".", 3)
	mParts := strings.SplitN(minimum, ".", 3)
	for i := 0; i < len(mParts) && i < len(vParts); i++ {
		v, _ := strconv.Atoi(vParts[i])
		m, _ := strconv.Atoi(mParts[i])
		if v > m {
			return true
		}
		if v < m {
			return false
		}
	}
	return true
}

func hostIDOrName(names []string, idx int, fallback string) string {
	if idx < len(names) {
		return names[idx]
	}
	return fallback
}

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
	tags     map[string]string
	os       string // "linux", "windows", etc.
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

	// Fetch agent details (including tags and OS) for matched hosts.
	type agentInfo struct {
		tags map[string]string
		os   string
	}
	agentDetails := map[string]agentInfo{}
	if hostResp, err := apiRequest("GET", "/api/v1/hosts", nil); err == nil {
		var agents []struct {
			ID   string            `json:"id"`
			Tags map[string]string `json:"tags"`
			OS   string            `json:"os"`
		}
		if json.Unmarshal(hostResp, &agents) == nil {
			for _, a := range agents {
				agentDetails[a.ID] = agentInfo{tags: a.Tags, os: a.OS}
			}
		}
	}

	var hosts []queryHost
	for _, r := range result.Results {
		if r.Success && r.Hostname != "" {
			info := agentDetails[r.AgentID]
			hosts = append(hosts, queryHost{r.Hostname, r.AgentID, info.tags, info.os})
		}
	}
	return hosts, nil
}

// discoverPythonInterpreters probes Linux hosts that lack an
// ansible_python_interpreter tag to find a working Python. If no Python
// is found on a host, it returns an error listing the failing hosts.
func discoverPythonInterpreters(hosts []queryHost) error {
	// Collect Linux hosts that need probing.
	var needProbe []int // indices into hosts
	for i, h := range hosts {
		if strings.EqualFold(h.os, "windows") {
			// Windows hosts use PowerShell, no Python needed.
			continue
		}
		if _, ok := h.tags["ansible_python_interpreter"]; ok {
			// Already configured.
			continue
		}
		needProbe = append(needProbe, i)
	}

	if len(needProbe) == 0 {
		return nil
	}

	// Build a hostname filter for the probe.
	hostnames := make([]string, len(needProbe))
	for i, idx := range needProbe {
		hostnames[i] = "'" + hosts[idx].hostname + "'"
	}

	fmt.Fprintf(os.Stderr, "Detecting Python on %d non-Windows host(s)...\n", len(needProbe))

	// Probe common Python paths. The first one found wins.
	// Prefer newer versions but accept Python 3.8+ (minimum for modern Ansible).
	// RHEL 8 ships Python 3.6 as /usr/bin/python3 which is too old; install python39.
	probeCmd := `for p in /usr/bin/python3.12 /usr/bin/python3.11 /usr/bin/python3.9 /usr/bin/python3; do [ -x "$p" ] && "$p" -c "import sys; sys.exit(0 if sys.version_info >= (3,8) else 1)" 2>/dev/null && echo "$p" && exit 0; done; echo "NONE"; exit 1`

	payload := map[string]any{
		"query":   "SELECT hostname WHERE os_info.hostname IN (" + strings.Join(hostnames, ", ") + ")",
		"command": probeCmd,
		"timeout": 15,
	}

	body, _ := json.Marshal(payload)
	resp, err := apiStreamRequest("POST", "/api/v1/exec_multi", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("python probe failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("python probe failed: HTTP %d: %s", resp.StatusCode, string(data))
	}

	// Parse streamed results.
	dec := json.NewDecoder(resp.Body)

	// Skip header.
	var header json.RawMessage
	if err := dec.Decode(&header); err != nil {
		return fmt.Errorf("python probe: read header: %w", err)
	}

	// Map hostname → discovered python path.
	discovered := map[string]string{}
	for dec.More() {
		var r struct {
			Type     string `json:"type"`
			Hostname string `json:"hostname"`
			RC       int    `json:"rc"`
			Stdout   string `json:"stdout"`
			Success  bool   `json:"success"`
		}
		if err := dec.Decode(&r); err != nil {
			break
		}
		if r.Type != "result" || r.Hostname == "" {
			continue
		}
		if r.Success && r.RC == 0 {
			stdout, _ := base64.StdEncoding.DecodeString(r.Stdout)
			path := strings.TrimSpace(string(stdout))
			if path != "" && path != "NONE" {
				discovered[r.Hostname] = path
			}
		}
	}

	// Apply discovered interpreters and collect failures. Summarize by
	// interpreter path so output stays bounded at large fleet sizes — one
	// line per distinct interpreter, not one per host.
	var noPython []string
	pathCounts := map[string]int{}
	for _, idx := range needProbe {
		h := &hosts[idx]
		if path, ok := discovered[h.hostname]; ok {
			if h.tags == nil {
				h.tags = map[string]string{}
			}
			h.tags["ansible_python_interpreter"] = path
			pathCounts[path]++
		} else {
			noPython = append(noPython, h.hostname)
		}
	}
	paths := make([]string, 0, len(pathCounts))
	for p := range pathCounts {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(i, j int) bool {
		if pathCounts[paths[i]] != pathCounts[paths[j]] {
			return pathCounts[paths[i]] > pathCounts[paths[j]]
		}
		return paths[i] < paths[j]
	})
	for _, p := range paths {
		fmt.Fprintf(os.Stderr, "  %s (%d host(s))\n", p, pathCounts[p])
	}

	if len(noPython) > 0 {
		return fmt.Errorf("no Python 3.8+ found on %d host(s): %s\nInstall python39 (RHEL 8) or python3 (RHEL 9+) or set the ansible_python_interpreter tag",
			len(noPython), strings.Join(noPython, ", "))
	}

	persistPythonInterpreterTags(discovered)

	fmt.Fprintln(os.Stderr)
	return nil
}

// persistPythonInterpreterTags writes discovered Python interpreter paths back
// to the server as host tags so subsequent runs skip the probe entirely.
// Failures are silent: a missed write just means we re-probe next time.
func persistPythonInterpreterTags(discovered map[string]string) {
	if len(discovered) == 0 {
		return
	}
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for hostname, path := range discovered {
		wg.Add(1)
		sem <- struct{}{}
		go func(hostname, path string) {
			defer wg.Done()
			defer func() { <-sem }()
			body, _ := json.Marshal(map[string]string{"ansible_python_interpreter": path})
			_, _ = apiRequest("PATCH", "/api/v1/hosts/"+url.PathEscape(hostname)+"/tags", bytes.NewReader(body))
		}(hostname, path)
	}
	wg.Wait()
}

// yamlScalar renders s as a double-quoted YAML scalar with metacharacters
// escaped, so an attacker-controlled value (an agent-supplied tag, hostname,
// or agent ID) cannot break out of its line and inject additional inventory
// keys such as ansible_connection or ansible_python_interpreter.
func yamlScalar(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// writeInventory creates a temporary YAML inventory file for Ansible.
func writeInventory(hosts []queryHost) (string, error) {
	tmpInv, err := os.CreateTemp("", "dirq-inventory-*.yml")
	if err != nil {
		return "", fmt.Errorf("create temp inventory: %w", err)
	}

	fmt.Fprintf(tmpInv, "all:\n  hosts:\n")
	for _, h := range hosts {
		// hostname, agent_id and tag values originate from agent-supplied
		// data. Emit them as quoted YAML scalars so a malicious value cannot
		// break out of its line and inject inventory host vars such as
		// ansible_connection or ansible_python_interpreter.
		fmt.Fprintf(tmpInv, "    %s:\n", yamlScalar(h.hostname))
		fmt.Fprintf(tmpInv, "      dirq_agent_id: %s\n", yamlScalar(h.agentID))
		fmt.Fprintf(tmpInv, "      dirq_server_url: %s\n", yamlScalar(serverURL))
		fmt.Fprintf(tmpInv, "      ansible_connection: dirq\n")

		isWindows := strings.EqualFold(h.os, "windows")

		if isWindows {
			// Windows hosts need PowerShell shell type for Ansible.
			shellType := "powershell"
			if v, ok := h.tags["ansible_shell_type"]; ok {
				shellType = v
			}
			fmt.Fprintf(tmpInv, "      ansible_shell_type: %s\n", yamlScalar(shellType))
		} else {
			// Use ansible_python_interpreter from tag or auto-detected value.
			pythonInterp := "/usr/bin/python3"
			if v, ok := h.tags["ansible_python_interpreter"]; ok {
				pythonInterp = v
			}
			fmt.Fprintf(tmpInv, "      ansible_python_interpreter: %s\n", yamlScalar(pythonInterp))
		}

		// Pass through any other ansible_* tags as host vars. Keys are
		// constrained to the ansible_ prefix; values are quoted to prevent
		// YAML/host-var injection. The server already strips agent
		// self-reported ansible_* tags at registration, so these reach here
		// only when set by an operator through the admin tag API.
		for k, v := range h.tags {
			if strings.HasPrefix(k, "ansible_") && k != "ansible_python_interpreter" && k != "ansible_shell_type" {
				fmt.Fprintf(tmpInv, "      %s: %s\n", k, yamlScalar(v))
			}
		}
	}
	tmpInv.Close()
	return tmpInv.Name(), nil
}

// connectionPluginDir returns the path to the DirQ Ansible connection plugin.
func connectionPluginDir() string {
	// Check standard install paths first, then dev tree relative to binary.
	candidates := []string{
		"/usr/share/dirq/connection_plugins",
		"/usr/local/share/dirq/connection_plugins",
	}
	exePath, _ := os.Executable()
	if exePath != "" {
		exeDir := filepath.Dir(exePath)
		// Windows installer: connection_plugins/ next to dirq.exe
		candidates = append(candidates, filepath.Join(exeDir, "connection_plugins"))
		// Dev tree: ../ansible/connection_plugins/ relative to bin/
		candidates = append(candidates, filepath.Join(exeDir, "..", "ansible", "connection_plugins"))
		// PREFIX/share/dirq/connection_plugins/
		candidates = append(candidates, filepath.Join(exeDir, "..", "share", "dirq", "connection_plugins"))
	}
	for _, dir := range candidates {
		if absDir, err := filepath.Abs(dir); err == nil {
			if _, err := os.Stat(absDir); err == nil {
				return absDir
			}
		}
	}
	return ""
}
