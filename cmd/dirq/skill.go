// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func skillCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "skill",
		Short: "Print an AI-readable reference for the DirQ query language",
		Long:  "Outputs a concise prompt that teaches an AI assistant how to use DirQ. Pipe it into your AI tool's context.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Print(skillText)
			return nil
		},
	}
}

const skillText = `# DirQ — Fleet Query & Management Tool

DirQ queries a fleet of agents using a SQL-like DSL. Every agent runs a
lightweight daemon that collects system data (CPU, memory, disk, packages,
services, network) and reports through a relay mesh to a central server.

## Query syntax

    SELECT <fields | *>
    [WHERE <expression>]
    [GROUP BY <field>, ...]
    [ORDER BY <field> [ASC|DESC], ...]
    [LIMIT <n>]

Keywords are case-insensitive. Only SELECT is required.

## Fields

Dotted notation: module.field. Available modules and their fields:

  cpu          — physical_cores, logical_cores, model_name, mhz
  memory       — total_bytes, available_bytes, used_bytes, pct_used
  disk         — (array of partitions) device, mount_point, fs_type,
                 total_bytes, used_bytes, free_bytes, pct_used
  os_info      — hostname, os, os_version, kernel_version, arch, uptime_seconds, distro, distro_version, distro_family
  hotfixes     — (array, Windows only) kb_id, description, installed_on
  packages     — (array) name, version, arch, source
  services     — (array) name, display_name, state, start_type
  network      — (array of interfaces) name, mac, mtu, flags, addresses

Top-level fields (no module prefix): hostname, os, arch, role, online.

Array modules (disk, packages, services, network): WHERE conditions filter
the array elements — only matching entries are returned.

## Tag targeting

Agent tags are available as tag.* fields:

    WHERE tag.env = 'prod'
    WHERE tag.group = 'webservers'
    WHERE tag.env IN ('prod', 'staging')

Tag conditions are evaluated server-side before dispatching. Only matching
agents receive the query.

## WHERE operators

  =  !=  >  <  >=  <=
  LIKE / NOT LIKE     — % matches any chars, _ matches one char
  IN / NOT IN         — field IN ('a', 'b', 'c')
  IS NULL / IS NOT NULL

Combine with AND, OR, NOT, and parentheses. AND binds tighter than OR.

IMPORTANT: Comparison operators (>, <, >=, <=) use string comparison, NOT
numeric or version comparison. They are useful for numeric fields like
cpu.logical_cores or disk.pct_used, but NOT for version strings like
packages.version. To find packages by version, use = or LIKE instead:

    WHERE packages.name = 'openssl' AND packages.version LIKE '1.1%'

Do NOT use: packages.version > '1.0' — this does lexicographic comparison
and will produce incorrect results for version strings.

## Aggregation

  COUNT(field)  SUM(field)  AVG(field)  MIN(field)  MAX(field)

Use with GROUP BY for per-group summaries, or without GROUP BY for a
fleet-wide total (e.g. SELECT COUNT(hostname) WHERE os_info.os = 'linux').

## CLI commands

Shell characters like >, <, *, (, and ) will be interpreted by your
shell if left unquoted. The safest approach is to quote the entire
command and let dirq parse it:

    dirq "select hostname, disk.pct_used where disk.pct_used > 80"
    dirq "select * where (tag.env = 'prod' or tag.env = 'staging') and disk.pct_used > 90"
    dirq "run deploy.yml where tag.env = 'prod'"
    dirq "deploy ./patch.rpm where tag.env = 'prod'"

DirQ splits quoted args by whitespace internally, so this works
identically to typing each word as a separate argument.

### dirq select — query the fleet

    dirq "select hostname, disk.pct_used where disk.pct_used > 80"
    dirq "select * --json"
    dirq select hostname, cpu.cores WHERE tag.env = 'prod'

### dirq exec — execute a command or script across the fleet in parallel

    dirq exec -- uptime
    dirq exec WHERE tag.env = 'prod' -- openssl version
    dirq exec --become WHERE tag.role = 'webserver' -- systemctl status nginx
    dirq exec WHERE tag.env = 'prod' --script ./health-check.sh
    dirq exec WHERE os_info.os = 'windows' --script ./audit.ps1

Fan-out exec: runs the command or script on all matching agents
simultaneously, streaming results back in real time. The remote
command goes after -- so it can contain any flags without conflict.

Use --script to upload and execute a local script file. Without
--script, everything after -- is the command string run on remote
hosts. Linux scripts use their shebang (#!) line. Windows scripts
run as PowerShell (.ps1) or cmd (.bat/.cmd).

### dirq run — run Ansible against matching hosts

    dirq run deploy.yml WHERE tag.env = 'prod'
    dirq run cleanup.yml
    dirq run --command "systemctl restart nginx" WHERE tag.env = 'prod'
    dirq run --module ping WHERE os_info.os = 'linux'

### dirq deploy — deploy packages through the mesh

    dirq deploy ./patch.rpm WHERE tag.env = 'prod'
    dirq deploy ./agent-0.3.0.msi WHERE os_info.os = 'windows'
    dirq deploy ./fix.deb

Broadcast through the mesh — each link carries the package once. Supports .rpm, .deb, .msi.

### dirq ask — natural language queries (requires LLM API key)

    dirq ask "which prod hosts have full disks?"
    dirq ask "how many hosts are running linux?" --dry-run

### dirq cve — scan RHEL systems for CVE vulnerabilities

    dirq cve CVE-2024-6345
    dirq "cve CVE-2024-6345 where tag.env = 'prod'"

Fetches affected packages from Red Hat Security Data API, queries the
fleet for installed versions, and reports which hosts are vulnerable.

### dirq doctor — check deployment health

    dirq doctor

### dirq hosts — manage hosts and tags

    dirq hosts list
    dirq hosts show <agent-id>
    dirq hosts facts <agent-id>
    dirq hosts tag <agent-id> env=prod role=webserver
    dirq hosts untag <agent-id> env

### dirq token — manage API tokens

    dirq token create ops-team --scope admin
    dirq token create monitoring --scope readonly
    dirq token list
    dirq token delete <name>

### dirq skill — print this reference for LLM context

    dirq skill

## Example queries

    -- Full disks in production
    SELECT hostname, disk.mount_point, disk.pct_used
    WHERE tag.env = 'prod' AND disk.pct_used > 80
    ORDER BY disk.pct_used DESC

    -- Hosts running Linux or FreeBSD with many cores
    SELECT hostname, os_info.os, cpu.logical_cores
    WHERE (os_info.os = 'linux' OR os_info.os = 'freebsd') AND cpu.logical_cores >= 16

    -- Check for vulnerable packages
    SELECT hostname, packages.name, packages.version
    WHERE packages.name = 'openssl' AND packages.version LIKE '1.%'

    -- Count hosts by OS
    SELECT os_info.os, COUNT(hostname) GROUP BY os_info.os

    -- Find stopped services
    SELECT hostname, services.name, services.state
    WHERE services.name = 'sshd' AND services.state = 'stopped'

    -- Hosts missing a tag
    SELECT hostname WHERE tag.env IS NULL

    -- Everything about all hosts
    SELECT *
`

// ─────────────────────────────────────────────────────────
// dirq ask
// ─────────────────────────────────────────────────────────
