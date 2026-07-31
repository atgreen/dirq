# Set up MCP integration

DirQ includes a built-in [Model Context Protocol](https://modelcontextprotocol.io/) (MCP) server, allowing LLMs like Claude to manage your fleet directly as a tool.

## Setup

Start the MCP server:

```bash
dirq mcp
```

This runs an MCP stdio server that exposes fleet management tools over JSON-RPC 2.0.

### Claude Desktop

Add to `claude_desktop_config.json`:

```json
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
}
```

### Claude Code

Add to your project's `.mcp.json`:

```json
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
}
```

## Available Tools

| Tool | Description |
|------|-------------|
| `dirq_hosts_list` | List all registered hosts, optionally filtered by WHERE clause |
| `dirq_hosts_show` | Show detailed info for a specific host |
| `dirq_hosts_facts` | Get real-time system facts (CPU, memory, disk, packages, etc.) |
| `dirq_hosts_tag` | Add or update tags on hosts |
| `dirq_query` | Run DirQ SELECT queries across the fleet |
| `dirq_exec` | Execute shell commands on targeted hosts |
| `dirq_cve_scan` | Scan RHEL hosts for a specific CVE vulnerability |
| `dirq_errata_check` | Check fleet against a Red Hat advisory |
| `dirq_kb_check` | Check Windows hosts for installed hotfixes |
| `dirq_graph` | Show the fleet mesh topology |

## Example Prompts

With the MCP server configured, you can ask Claude things like:

- "Which hosts in prod have more than 80% disk usage?"
- "Are any of our RHEL hosts vulnerable to CVE-2024-6345?"
- "Tag all Windows hosts with role=iis"
- "Run `uptime` on all Linux hosts in staging"
- "Show me the fleet topology"
