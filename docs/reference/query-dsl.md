# Query DSL

A SQL-like language for ad-hoc fleet queries. Queries are parsed on the server, pushed through the relay mesh, filtered agent-side, and aggregated server-side.

## Syntax

```
SELECT <fields | *>
[WHERE <expression>]
[GROUP BY <field>, ...]
[ORDER BY <field> [ASC|DESC], ...]
[LIMIT <n>]
```

Every clause except `SELECT` is optional. Queries always target all online hosts;
use `tag.*` conditions in WHERE to narrow the target (see below).
Keywords are case-insensitive (`select`, `SELECT`, and `Select` all work).

## Fields

Fields use dotted notation: `module.field`. See [Query Modules](../reference/query-modules.md) for available modules.

Each disk partition contains: `device`, `mount_point`, `fs_type`, `total_bytes`, `used_bytes`, `free_bytes`, `pct_used`.
Each package contains: `name`, `version`, `arch`, `source`.
Each network interface contains: `name`, `mac`, `mtu`, `flags`, `addresses` (array of `{addr, family}`).
Each service contains: `name`, `display_name`, `state`, `start_type`.

## WHERE — filtering

Conditions support `AND`, `OR`, `NOT`, and parenthesized grouping with proper precedence (`AND` binds tighter than `OR`). Simple AND-only filters are pushed to agents; complex expressions (OR, NOT) are evaluated server-side.

```sql
WHERE disk.pct_used > 80
WHERE cpu.logical_cores >= 8 AND memory.pct_used > 50
WHERE os_info.os = 'linux' OR os_info.os = 'freebsd'
WHERE (os_info.os = 'linux' OR os_info.os = 'freebsd') AND cpu.logical_cores > 4
WHERE NOT os_info.os = 'windows'
WHERE os_info.kernel_version LIKE '7.0%'
WHERE os_info.kernel_version NOT LIKE '%debug%'
WHERE packages.name IN ('openssl', 'nginx', 'curl')
WHERE packages.name NOT IN ('telnet', 'rsh')
WHERE services.name = 'sshd' AND services.state = 'stopped'
WHERE cpu.model IS NOT NULL
```

**Operators:** `=`, `!=`, `>`, `<`, `>=`, `<=`, `LIKE`, `NOT LIKE`, `IN`, `NOT IN`, `IS NULL`, `IS NOT NULL`

## Tag targeting

Agent tags are available as `tag.*` fields in WHERE conditions. The server evaluates tag conditions before dispatching — only matching agents receive the query.

```sql
-- Only prod hosts
WHERE tag.env = 'prod' AND disk.pct_used > 80

-- Multiple environments
WHERE tag.env IN ('prod', 'staging')

-- Group targeting
WHERE tag.group = 'webservers'

-- Complex targeting
WHERE (tag.env = 'prod' OR tag.env = 'staging') AND tag.group = 'webservers'
```

Tag conditions can be freely mixed with data conditions using AND/OR.

## Array-aware filtering

When a WHERE condition references a field inside an array module (packages, services, disk, network), the agent filters the array and returns only matching entries:

```sql
-- Returns only 3 packages, not all 2000 installed
WHERE packages.name IN ('openssl', 'nginx', 'curl')

-- Returns only partitions over 80% full
WHERE disk.pct_used > 80
```

## GROUP BY, ORDER BY, and LIMIT

```sql
SELECT os_info.os, COUNT(os_info.hostname), AVG(memory.total_bytes)
GROUP BY os_info.os

ORDER BY disk.pct_used DESC
ORDER BY os_info.os ASC, os_info.hostname DESC

LIMIT 10
```

**Aggregation functions:** `COUNT`, `AVG`, `SUM`, `MIN`, `MAX`

Aggregates work with or without `GROUP BY`:

```sql
-- Fleet-wide total (bare aggregate)
SELECT COUNT(hostname) WHERE os_info.os = 'linux'

-- Per-group breakdown
SELECT os_info.os, COUNT(hostname) GROUP BY os_info.os
```

## Examples

```sql
-- Hosts with full disks in prod (only matching partitions returned)
SELECT os_info.hostname, disk.mount_point, disk.pct_used
WHERE tag.env = 'prod' AND disk.pct_used > 80 ORDER BY disk.pct_used DESC

-- Check specific package versions
SELECT os_info.hostname, packages.name, packages.version
WHERE packages.name IN ('openssl', 'nginx', 'curl')

-- Find hosts where sshd is stopped
SELECT os_info.hostname, services.name, services.state
WHERE services.name = 'sshd' AND services.state = 'stopped'

-- Count hosts by OS
SELECT os_info.os, COUNT(os_info.hostname), AVG(memory.total_bytes)
GROUP BY os_info.os

-- Find beefy hosts
SELECT os_info.hostname, cpu.logical_cores, memory.total_bytes
WHERE cpu.logical_cores >= 16

-- Packages matching a pattern
SELECT os_info.hostname, packages.name, packages.version
WHERE packages.name LIKE 'openssl%'

-- OR and parentheses
SELECT os_info.hostname, os_info.os
WHERE (os_info.os = 'linux' OR os_info.os = 'freebsd') AND cpu.logical_cores > 4

-- Exclude specific packages, limit results
SELECT os_info.hostname, packages.name
WHERE packages.name NOT IN ('telnet', 'rsh') LIMIT 50

-- Everything about all hosts
SELECT *
```

## CLI usage

```bash
# Natural syntax — no quoting needed for simple queries
dirq select os_info.hostname, cpu.logical_cores
dirq select os_info.hostname, disk.pct_used WHERE disk.pct_used = 80

# Quoted form — avoids shell interpretation of > < etc.
dirq "select os_info.hostname, disk.pct_used where disk.pct_used > 80"

# Flags
dirq select os_info.os, COUNT(os_info.hostname) GROUP BY os_info.os --json
dirq "select * where tag.env = 'prod'" --timeout 30
```

## Natural language queries

Ask questions in plain English — an LLM uses DirQ's fleet tools to gather data and compose an answer. The LLM can call multiple tools and iterate until it has enough information.

```bash
dirq ask "which prod hosts have full disks?"
dirq ask "how many hosts are running linux?"
dirq ask "what versions of openssl are installed?"
dirq ask "are any hosts vulnerable to CVE-2024-6345?"
```

Tool calls are shown as the LLM works:

```
$ dirq ask "how many linux servers do I have?"
  [dirq_query] SELECT COUNT(hostname) WHERE os_info.os = 'linux'
You have 4 Linux servers, all running RHEL 8.10.
```

!!! note "Read-only"
    The LLM is **read-only** — it can query and inspect but cannot execute commands or modify hosts. If you ask it to make changes, it will suggest the `dirq exec` command to run.

**Configuration:** Uses `DIRQ_LLM_URL` + `DIRQ_LLM_API_KEY` + `DIRQ_LLM_MODEL`, or falls back to `ANTHROPIC_API_KEY`. Supports both Anthropic's native API and any OpenAI-compatible endpoint.

```bash
# Anthropic (direct)
export ANTHROPIC_API_KEY=sk-ant-...

# OpenAI-compatible (any provider)
export DIRQ_LLM_URL=https://api.openai.com/v1
export DIRQ_LLM_API_KEY=sk-...
export DIRQ_LLM_MODEL=gpt-4o
```

Use `--model` to override the model for a single query:

```bash
dirq ask "disk usage in prod" --model claude-sonnet-4-20250514
```

## AI integration

Generate an AI-readable reference for the query language:

```bash
dirq skill            # print to stdout
dirq skill | pbcopy   # copy to clipboard (macOS)
```

## Arg flattening

Quoted arguments that start with `SELECT` are automatically split into individual
args before parsing. This lets you write queries as a single quoted string:

```bash
dirq "select hostname where tag.env = 'prod'"  # same as: dirq select hostname where ...
```

Other commands are **not** flattened. For `dirq exec`, the remote command goes after `--` so flags and special characters pass through without conflict:

```bash
dirq exec WHERE tag.env = 'prod' -- ls -l   # everything after -- is the remote command
```
