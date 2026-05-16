# DirQ — Direct Query Platform for Fleet Management & Ansible Execution

DirQ ("Direct Query") is an agent-based platform for querying and managing large Windows/Linux fleets. Agents form a peer-to-peer relay mesh and report data back to a central server. The server acts as an Ansible Automation Platform (AAP) inventory source, exposes collected data as structured facts, and can route Ansible execution through the mesh as an alternative to SSH/WinRM connectivity.

The key idea is simple:

- **Query the fleet like a dataset** instead of logging into hosts one by one
- **Keep managed hosts outbound-only** instead of opening SSH/WinRM inbound
- **Reuse Ansible** while replacing the transport underneath
- **Build Ansible inventories from live DirQ query results** instead of static host lists
- **Scale with a relay tree** so the server does not need a direct session to every node
- **Scan for CVEs in real time** — identify every affected host in seconds, not hours
- **Run ad-hoc commands across the fleet** — parallel exec with streaming results

One of the most practical workflows in DirQ is:

1. Query the fleet for exactly the hosts you care about
2. Turn those results into an Ansible inventory
3. Run a playbook only against that live, data-driven target set

Examples:

- Find only hosts with disks over 90%, turn that into an inventory, then run a cleanup or expansion playbook.
- Query for hosts with vulnerable OpenSSL package versions, build an inventory from the result, and patch only those systems.
- A new CVE drops — run `dirq cve CVE-2024-6345` and instantly see which hosts are vulnerable and which are already patched, across the entire fleet.
- Query for hosts where `sshd` or another critical service is stopped, generate an inventory, and run a remediation playbook immediately.
- Quick ad-hoc check: `dirq exec "uptime" WHERE tag.env = 'prod'` to see every prod host's uptime without setting up a playbook.

## Why DirQ?

DirQ is useful when traditional fleet access patterns start breaking down:

1. **Large locked-down environments** — managed hosts cannot accept inbound SSH or WinRM.
2. **Segmented enterprise networks** — a single control plane across data centers, edge sites, or heavily firewalled zones.
3. **Query-driven Ansible targeting** — inventories based on live fleet state, not stale static groups.
4. **Ansible without transport pain** — keep your playbooks, drop the SSH/WinRM dependency.
5. **Real-time CVE response** — a vulnerability drops and you need to know which hosts are affected *now*, not after the next scheduled scan.
6. **Real-time fleet troubleshooting** — answer "which prod hosts have disks over 90%?" and act on it immediately.
7. **Very large estates** — server connection count stays bounded while the fleet grows.

**What makes DirQ different:**

- **Mesh-first architecture:** agents relay for each other, so the fleet becomes its own transport.
- **Structured query model:** modules return normalized data instead of raw command output.
- **Ansible compatibility:** DirQ acts as query engine, inventory source, and execution transport — existing playbooks work without modification.
- **Inventory and execution in one system:** the same platform that knows the fleet can also target it.

## Table of Contents

- [Architecture](#architecture) — how the mesh works, scaling
- [Quick Start](#quick-start-podman-on-laptop) — run locally in 5 minutes
- [Query DSL](#query-dsl) — the fleet query language
- [Ansible Integration](#ansible-integration) — inventory, groups, facts, query-based targeting
- [Execution Transport](#execution-transport) — run Ansible through the mesh
- [Fleet Exec](#fleet-exec) — ad-hoc parallel command & script execution
- [Topology Graph](#topology-graph) — visualize the agent mesh tree
- [Security](#security) — TLS, authentication, exec safety
- [Multi-Datacenter Deployment](#multi-datacenter-deployment) — isolated meshes, per-DC routing
- [AAP Integration](#aap-integration) — collection, EE, credentials, setup checklist
- [Configuration Reference](#configuration-reference) — all environment variables
- [REST API](#rest-api) — endpoint reference
- [Building](#building) — compile, cross-compile, container images
- [Project Structure](#project-structure)

---

## Architecture

```
  ┌──────────┐   ┌──────────┐   ┌──────────┐   ┌──────────┐
  │  Agent   │   │  Agent   │   │  Agent   │   │  Agent   │
  │  (leaf)  │   │  (leaf)  │   │  (leaf)  │   │  (leaf)  │
  └────┬─────┘   └────┬─────┘   └────┬─────┘   └────┬─────┘
       │              │              │              │
       ▼              ▼              ▼              ▼
  ┌───────────────────────┐   ┌───────────────────────┐
  │  Agent (relay peer)   │   │  Agent (relay peer)   │
  └───────────┬───────────┘   └───────────┬───────────┘
              │                           │
              ▼                           ▼
         ┌──────────────────────────────────────┐
         │         Agent (zone leader)          │
         └──────────────────┬───────────────────┘
                            │
              ══════════════╪══════════════
                            │  (OpenShift Route)
                            ▼
         ┌──────────────────────────────────────┐
         │         DirQ Server (Go)             │
         │  REST API · gRPC · Query Engine      │
         └──────────────────┬───────────────────┘
                            │
                            ▼
                  ┌──────────────────┐
                  │ SQLite / PostgreSQL│
                  └──────────────────┘
```

All links are **gRPC over TLS**. Agents connect outbound — no inbound ports required on managed hosts. Only a bounded number of zone leaders connect directly to the server.

### Components

| Component | Language | Description |
|-----------|----------|-------------|
| `dirq-server` | Go | Central server: gRPC, REST API, query engine, Ansible inventory. SQLite by default; PostgreSQL optional. |
| `dirq-agent` | Go | Endpoint agent: collects data, relays queries, optionally executes commands. Single static binary. |
| `dirq` | Go | CLI: submit queries, manage hosts/tags/tokens, run ad-hoc commands, generate TLS certs. |
| `atgreen.dirq` | Python | Ansible collection: inventory plugin + connection plugin for AAP. |

### Scaling the Mesh

The server holds a fixed number of zone leader connections (default 5). All other agents fill a tree below those zone leaders, growing as deep as needed (BFS fill order).

| Fleet size | Tree depth | Server connections |
|-----------|-----------|-------------------|
| 250 | 2 | 5 |
| 12,500 | 3 | 5 |
| 625,000 | 4 | 5 |

The server always holds exactly `DIRQ_MAX_ZONE_LEADERS` connections regardless of fleet size. The tree deepens — it never widens at the server.

### Result Aggregation

Query results aggregate in-mesh, not at the server. Each relay buffers results
from its children for 2 seconds, then flushes one `AggregatedQueryResult`
upstream. Zone leaders do the same. The server receives ~5 messages (one per
zone leader) instead of 100k individual responses.

### Redundant Parents

Each non-zone-leader agent receives 2 fallback parent addresses during
registration, chosen from different branches of the tree. On parent failure:

1. Try fallback parent 0 (different branch, sub-second)
2. Try fallback parent 1 (another branch)
3. Ask the server for a new parent assignment via `RequestPeers` RPC

Agents never fall back to direct server connections — they always ask the
server where to go. The server marks the dead parent offline and assigns
a healthy replacement. When a zone leader goes offline, the server
immediately reassigns its orphaned children to other healthy nodes.

### Built-in Query Modules

| Module | Data collected |
|--------|---------------|
| `cpu` | Physical/logical cores, model name, vendor |
| `memory` | Total, available, used bytes; percent used; swap |
| `disk` | Per-partition: device, mount point, fs type, total/used/free bytes, percent used |
| `os_info` | Hostname, OS, version, arch, uptime, kernel version, distro, distro_version, distro_family |
| `packages` | Installed packages: name, version, arch, source (rpm/dpkg/registry) |
| `network` | Interfaces: name, MAC, MTU, flags, IP addresses (loopback filtered) |
| `services` | Services: name, display name, state, start type (systemd/Windows Services) |
| `hotfixes` | Windows hotfixes: kb_id, description, installed_on (Get-HotFix) |

---

## Quick Start (Podman on Laptop)

### Prerequisites

- Go 1.26+
- Podman and podman-compose

### 1. Start the server and database

```bash
podman-compose up -d
```

The server auto-generates TLS certs, runs DB migrations, and creates a bootstrap API token. The token is written to a file (not logged) for security:

```bash
# The server log shows the token file path:
podman logs dirq_dirq-server_1 2>&1 | grep "bootstrap"
# Read the token:
cat /var/lib/dirq/bootstrap-token
```

### 2. Deploy agents

The server writes ready-to-copy config files on startup:

- **`/var/lib/dirq/agent.conf`** — agent config with server address, registration secret, and inline TLS certs (base64-encoded). Copy to `/etc/dirq/agent.conf` on each agent host.
- **`/var/lib/dirq/client.conf`** — CLI config with server URL and bootstrap token. Copy to `/etc/dirq/client.conf` or `~/.config/dirq/client.conf` on any workstation.

```bash
# On the server, copy the generated agent config to a remote host:
scp /var/lib/dirq/agent.conf agent-host:/etc/dirq/agent.conf

# On the agent host:
sudo systemctl enable --now dirq-agent
```

For local dev, build and run the agent directly:

```bash
go build -o bin/dirq-agent ./cmd/dirq-agent
./bin/dirq-agent
```

The agent auto-generates TLS certs into the same directory as the server (`/var/lib/dirq/tls`). When both run on the same machine, they share the auto-generated CA and verify each other automatically.

### 3. Build and use the CLI

```bash
go build -o bin/dirq ./cmd/dirq
```

The CLI reads config from `~/.config/dirq/client.conf` (user-local) or `/etc/dirq/client.conf` (system-wide). Copy the server-generated `client.conf`:

```bash
# Copy from server to your workstation:
scp server:/var/lib/dirq/client.conf ~/.config/dirq/client.conf

# Now just use dirq — no env vars needed:
dirq doctor
dirq hosts list
dirq select hostname, cpu.logical_cores, memory.pct_used
```

Or set env vars directly:

```bash
export DIRQ_SERVER_URL=https://dirq-server:8080
export DIRQ_TOKEN=<bootstrap-token>
export DIRQ_TLS_INSECURE=true  # for self-signed certs
```

### 4. Test with Ansible

```bash
cd test-playbook
DIRQ_SERVER_URL=http://localhost:8090 DIRQ_TOKEN=$DIRQ_TOKEN ansible-playbook test.yml -v
```

### Windows agent

```powershell
GOOS=windows GOARCH=amd64 go build -o bin/dirq-agent.exe ./cmd/dirq-agent

# Run in foreground
.\bin\dirq-agent.exe

# Or install as a Windows Service (runs as SYSTEM)
.\bin\dirq-agent.exe install
sc start DirQAgent
```

---

## Query DSL

A SQL-like language for ad-hoc fleet queries. Queries are parsed on the server, pushed through the relay mesh, filtered agent-side, and aggregated server-side.

### Syntax

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

### Fields

Fields use dotted notation: `module.field`. See [Built-in Query Modules](#built-in-query-modules) for available modules.

Each disk partition contains: `device`, `mount_point`, `fs_type`, `total_bytes`, `used_bytes`, `free_bytes`, `pct_used`.
Each package contains: `name`, `version`, `arch`, `source`.
Each network interface contains: `name`, `mac`, `mtu`, `flags`, `addresses` (array of `{addr, family}`).
Each service contains: `name`, `display_name`, `state`, `start_type`.

### WHERE — filtering

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

### Tag targeting

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

### Array-aware filtering

When a WHERE condition references a field inside an array module (packages, services, disk, network), the agent filters the array and returns only matching entries:

```sql
-- Returns only 3 packages, not all 2000 installed
WHERE packages.name IN ('openssl', 'nginx', 'curl')

-- Returns only partitions over 80% full
WHERE disk.pct_used > 80
```

### GROUP BY, ORDER BY, and LIMIT

```sql
SELECT os_info.os, COUNT(os_info.hostname), AVG(memory.total_bytes)
GROUP BY os_info.os

ORDER BY disk.pct_used DESC
ORDER BY os_info.os ASC, os_info.hostname DESC

LIMIT 10
```

**Aggregation functions:** `COUNT`, `AVG`, `SUM`, `MIN`, `MAX`

### Examples

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

### CLI usage

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

### Natural language queries

Ask questions in plain English — DirQ translates them to queries using an LLM (requires `ANTHROPIC_API_KEY` or `OPENAI_API_KEY`):

```bash
dirq ask "which prod hosts have full disks?"
# Query: SELECT hostname, disk.mount_point, disk.pct_used WHERE tag.env = 'prod' AND disk.pct_used > 80

dirq ask "show me all windows servers"
dirq ask "how many hosts are running linux?"
dirq ask "find hosts with openssl installed"
dirq ask "what packages are on the staging servers?"
```

Use `--dry-run` to see the generated query without executing it:

```bash
dirq ask "hosts with more than 8 cores" --dry-run
```

Use `--provider` and `--model` to choose the LLM:

```bash
dirq ask "stopped services" --provider openai --model gpt-4o
dirq ask "disk usage in prod" --provider anthropic --model claude-sonnet-4-20250514
```

### AI integration

Generate an AI-readable reference for the query language:

```bash
dirq skill            # print to stdout
dirq skill | pbcopy   # copy to clipboard (macOS)
```

### Running playbooks

Query the fleet and run Ansible against the results in one step:

```bash
# Run a playbook against hosts matching a WHERE clause
dirq run cleanup-disks.yml WHERE disk.pct_used = 90

# Quoted form
dirq "run deploy.yml where tag.env = 'prod'"

# Ad-hoc command
dirq run --command "yum update -y openssl" WHERE packages.name = 'openssl'

# Ansible module
dirq run --module ping WHERE os_info.os = 'linux'

# All online hosts (no WHERE clause)
dirq run deploy.yml
```

### Deploying packages

Deploy RPM, DEB, or MSI packages across the fleet through the relay mesh.
Designed primarily for non-disruptive self-updates of the dirq-agent package
itself — the depth-first rolling strategy updates deepest nodes first, working
up the tree so a parent is never updated while its children are mid-install.
This keeps the relay mesh intact throughout the upgrade.

```bash
# Deploy to all agents (rolling wave)
dirq deploy ./patch-2026-05.rpm

# Deploy to specific hosts
dirq deploy ./patch.rpm WHERE tag.env = 'prod'

# Windows packages
dirq deploy ./agent-0.3.0.msi WHERE os_info.os = 'windows'

# Override rolling deployment — install everywhere at once
dirq deploy ./monitoring.rpm --parallel
```

Package type is detected from the file extension:
- `.rpm` → `rpm -U`
- `.deb` → `dpkg -i`
- `.msi` → `msiexec /i ... /qn`

### CVE scanning

Scan RHEL systems for known vulnerabilities. DirQ fetches affected package
data from the Red Hat Security Data API, then queries the fleet to find
hosts running vulnerable versions.

```bash
# Scan all RHEL hosts
dirq cve CVE-2024-6345

# Scan only production
dirq cve CVE-2024-6345 WHERE tag.env = 'prod'

# Machine-readable output
dirq cve CVE-2024-6345 --json
```

Output shows each host's status:

```
CVE-2024-6345: pypa/setuptools: Remote code execution via download functions...
Severity: Important

  web1.prod     python-setuptools    39.2.0-7.el8         VULNERABLE (fixed in 39.2.0-8.el8_10)
  web2.prod     python-setuptools    39.2.0-8.el8_10      patched
  db1.prod      python-setuptools    39.2.0-7.el8         VULNERABLE (fixed in 39.2.0-8.el8_10)

2 vulnerable, 1 patched
```

### Topology graph

Visualize the agent mesh tree:

```bash
dirq graph
```

```
dirq-server
├── ● dirq-agent-01 [ZL]
│   ├── ● dirq-agent-06
│   └── ● dirq-agent-08
├── ● dirq-agent-02 [ZL]
│   └── ● dirq-agent-07
└── ● dirq-agent-03 [ZL]
    └── ● dirq-agent-09
```

`●` = online, `○` = offline, `[ZL]` = zone leader.

Export to Graphviz DOT format for rendering:

```bash
dirq graph --dot | dot -Tpng -o topology.png
```

### Deployment health

Check the health of your DirQ deployment with `dirq doctor`:

```bash
dirq doctor
```

```
  DIRQ_SERVER_URL               ok   https://dirq.example.com:8080
  API token valid                ok   authenticated
  TLS certificate                ok   valid
  PostgreSQL                     ok   connected
  Agents online                  ok   47/50
  Agent version skew             !!   3 agents on v0.2.0 (server is v0.3.0)
  Relay tree                     ok   depth 3, 5 zone leader(s)
  Ansible installed              ok   ansible-playbook [core 2.20.5]
  Connection plugin              ok   /usr/local/ansible/connection_plugins

  9 passed, 1 warnings, 0 failed
```

### Arg flattening

Quoted arguments that start with `SELECT` are automatically split into individual
args before parsing. This lets you write queries as a single quoted string:

```bash
dirq "select hostname where tag.env = 'prod'"  # same as: dirq select hostname where ...
```

Other commands are **not** flattened, so quoted exec commands with dashes work correctly:

```bash
dirq exec "ls -l" WHERE tag.env = 'prod'   # "ls -l" stays as the command
```

---

## Ansible Integration

### Inventory Groups

The inventory plugin creates a nested group hierarchy from agent metadata and tags:

```
@all
├── @os_linux / @os_windows
├── @arch_amd64 / @arch_arm64
├── @exec_enabled
├── @tag_env
│   ├── @tag_env_prod
│   └── @tag_env_dev
├── @tag_role
│   ├── @tag_role_webserver
│   └── @tag_role_database
└── @tag_dc
    ├── @tag_dc_us_east
    └── @tag_dc_eu_west
```

Target hosts with standard Ansible patterns:

```yaml
hosts: os_linux
hosts: tag_env_prod
hosts: tag_role_webserver:&os_linux       # intersection
hosts: exec_enabled
```

### Host Variables

All collected data exposed as `dirq_*` hostvars:

```yaml
dirq_agent_id: "abc-123"
dirq_os: "linux"
dirq_cpu: { physical_cores: 8, logical_cores: 16, ... }
dirq_memory: { total_bytes: 34359738368, pct_used: 34.4, ... }
dirq_disk: { partitions: [{ mount_point: "/", pct_used: 67.3, ... }] }
dirq_tag_env: "prod"
dirq_exec_enabled: true
```

### Query-Based Inventories

The inventory plugin accepts an optional `query` parameter. Only hosts matching the query appear in the inventory:

```yaml
# inventories/vulnerable-openssl.yml
plugin: atgreen.dirq.dirq
server_url: http://dirq-server:8080
query: "SELECT os_info.hostname WHERE packages.name = 'openssl' AND packages.version LIKE '1.%'"

# inventories/disks-full.yml
plugin: atgreen.dirq.dirq
server_url: http://dirq-server:8080
query: "SELECT os_info.hostname WHERE disk.pct_used > 90"
```

In AAP, each file becomes an Inventory Source. Job templates pair each inventory with a remediation playbook:

| Job Template | Inventory Source | Playbook | Targets |
|---|---|---|---|
| Patch OpenSSL | vulnerable-openssl.yml | update-openssl.yml | Hosts with OpenSSL 1.x |
| Fix Full Disks | disks-full.yml | cleanup-disks.yml | Hosts over 90% disk |

The query runs in real time during inventory sync — the host list is always current.

**Standalone:**
```bash
DIRQ_QUERY="SELECT os_info.hostname WHERE disk.pct_used > 90" \
  ansible-playbook -i ansible/dirq_inventory.py cleanup-disks.yml
```

### Tag Management

```bash
# Tag a single host by ID
dirq hosts tag <agent-id> env=prod role=webserver dc=us-east

# Tag multiple hosts with a WHERE clause
dirq hosts tag env=prod WHERE os_info.os = 'linux'
dirq hosts tag role=webserver WHERE tag.dc = 'us-east'

# Untag by ID or query
dirq hosts untag <agent-id> role dc
dirq hosts untag env WHERE tag.env = 'staging'
```

Tags flow into inventory groups automatically.

---

## Execution Transport

The relay mesh doubles as an Ansible connection transport. The inventory plugin
automatically sets `ansible_connection` for exec-enabled hosts, so **existing
playbooks work without modification** — no need to add `connection: dirq` or
`gather_facts: false`.

```yaml
# This just works — no connection: dirq needed.
# The inventory plugin handles it.
- hosts: tag_env_prod
  tasks:
    - command: uptime
    - copy:
        src: app.conf
        dest: /etc/myapp/app.conf
    - fetch:
        src: /var/log/status.log
        dest: /tmp/status.log
        flat: yes
```

The inventory plugin also maps DirQ facts to standard Ansible variables
(`ansible_os_family`, `ansible_distribution`, `ansible_architecture`,
`ansible_processor_vcpus`, `ansible_memtotal_mb`, etc.) and sets OS-specific
shell and interpreter settings (`ansible_shell_type`, `ansible_python_interpreter`
for Linux, `powershell` for Windows). Most existing roles work without changes.

### How It Works

1. AAP launches a job template — the inventory already set `ansible_connection`
2. The connection plugin routes `exec_command` / `put_file` / `fetch_file` to the DirQ server REST API
3. The server pushes through the relay mesh to the target agent
4. The agent executes locally and returns results back through the mesh
5. AAP records the job result normally

### Enabling Exec on Agents

Exec is **disabled by default** — opt in per agent:

```bash
DIRQ_EXEC_ENABLED=true ./bin/dirq-agent
```

Default exec timeout is 300 seconds (5 minutes), configurable via `dirq_exec_timeout`
in the connection plugin. Long-running tasks like `yum update` work without special
handling. Exec responses are forwarded immediately through the relay chain — they
are not batched by the result aggregator.

### Exec Audit Log

Every operation is logged in PostgreSQL with AAP job attribution:

```bash
curl "$DIRQ_SERVER_URL/api/v1/exec_log?aap_job_id=42"
```

---

## Fleet Exec

For quick ad-hoc tasks that don't need a full Ansible playbook, `dirq exec` runs a command or script across matching hosts in parallel and streams results back in real time.

### Commands

```bash
dirq exec "uptime" WHERE tag.env = 'prod'
dirq exec "openssl version" WHERE packages.name = 'openssl'
dirq exec --become "systemctl restart nginx" WHERE tag.role = 'webserver'
dirq exec "hostname -f"
dirq exec "df -h /" --json
```

### Scripts

Upload and execute a local script file with `--script`. Linux scripts honor their shebang. Windows `.ps1` files run with PowerShell.

```bash
dirq exec --script ./health-check.sh WHERE tag.env = 'prod'
dirq exec --script ./audit.ps1 WHERE os_info.os = 'windows'
dirq exec --become --script ./patch.sh WHERE tag.role = 'webserver'
```

### Streaming output

Results stream back as each host responds — fastest hosts appear first:

```
Targets: 3

── web-01  rc=0 ──
   14:23:01 up 42 days,  3:17,  0 users,  load average: 0.12, 0.08, 0.05

── db-01  rc=0 ──
   14:23:01 up 91 days, 12:44,  0 users,  load average: 0.45, 0.38, 0.31

── web-02  rc=0 ──
   14:23:02 up 13 days,  7:02,  0 users,  load average: 0.03, 0.05, 0.01

3/3 completed
```

With `--json`, output is NDJSON (one JSON object per line), suitable for piping.

---

## Security

### TLS

TLS is **enabled by default** on all gRPC and REST API connections. If no certificates are configured, self-signed certs are auto-generated at startup.

| TLS vars set | Behavior |
|---|---|
| Nothing | Auto-generate self-signed, encrypted, no mTLS, log warning |
| `CERT` + `KEY` | TLS with user certs, no mTLS |
| `CERT` + `KEY` + `CA` | Full mTLS (mutual authentication) |
| `DIRQ_TLS_DISABLED=true` | Explicitly insecure (must opt in) |

**Auto-generated certs** protect against passive sniffing but NOT against MITM or rogue impersonation (no shared CA to verify). For production, use user-supplied certs with `DIRQ_TLS_CA` for full mTLS.

**Generate self-signed certs manually:**
```bash
./bin/dirq tls generate --dir ./certs
# Creates: ca.crt, ca.key, server.crt, server.key, agent.crt, agent.key
```

**mTLS:** Set `DIRQ_TLS_CA` on both server and agent. Server rejects agents without a valid cert. Agents reject rogue servers.

### Authentication

API authentication is **required by default**. On first startup, a bootstrap token is auto-generated and printed to the server log. Save it.

```bash
dirq token create ops-team --scope admin
dirq token create monitoring --scope readonly
export DIRQ_TOKEN=<token>
```

**Token scopes are enforced per-endpoint:**
- `readonly` — queries, host listing, facts, inventory, query history, exec log
- `admin` — all of the above, plus tag management, token management, exec, put_file, fetch_file, deploy

Set `DIRQ_AUTH_DISABLED=true` to disable (not recommended).

### Message Signing

Every control message the server sends through the relay mesh — queries, exec requests, file transfers, rebalancer commands — is **signed with Ed25519** before dispatch. Each agent verifies the signature before processing.

This is critical because queries and exec requests flow through relay agents. Without signing, a compromised relay could inject fake commands to downstream agents. With signing:

- **Only the server can originate commands.** Relay agents forward signed messages but cannot forge them.
- **Signatures include an expiry window** (5 minutes), preventing replay attacks.
- **The server's public key is distributed to agents during registration** over the TLS-protected gRPC stream.

The signing key pair is auto-generated on first startup and persisted. To use a pre-generated key, set `DIRQ_SIGNING_KEY`.

### Registration Authentication

By default, any client that can reach the server's gRPC port can register as an agent. For production deployments, set a **registration secret** — a pre-shared key that agents must present during registration:

```bash
# Server
DIRQ_REGISTRATION_SECRET=my-fleet-secret dirq-server

# Agent
DIRQ_REGISTRATION_SECRET=my-fleet-secret dirq-agent
```

Or in config files:

```
# /etc/dirq/server.conf
registration_secret: my-fleet-secret

# /etc/dirq/agent.conf
registration_secret: my-fleet-secret
```

When configured, the server rejects `Register` calls that don't present the matching secret. This prevents unauthorized hosts from joining the mesh.

Session tokens issued during registration are Ed25519-signed and time-stamped. They expire after 24 hours, at which point the agent re-registers automatically to obtain a fresh token. Relay peers verify session tokens cryptographically using the server's signing public key — no shared state between relays and the server is needed.

### Execution Security

- **Server-originated only:** exec requests must come from the server and carry a valid Ed25519 signature. Relay agents forward but cannot forge exec requests.
- **Opt-in per agent:** `exec_enabled` defaults to `false`.
- **Full audit trail:** every operation logged with AAP job ID, user, command, exit status.
- **AAP retains authority:** DirQ is the data plane; AAP controls RBAC, credentials, approvals.
- **File transfer limits:** 100 MB default.
- **Windows:** agent runs as SYSTEM (Windows Service). Become uses PowerShell scheduled tasks.
- **Linux:** become uses `sudo -n` (non-interactive, NOPASSWD required).

---

## Multi-Datacenter Deployment

Run one DirQ server per datacenter. Meshes never span DC boundaries.

```
  DC us-east                          DC eu-west
  ┌──────────────────────┐            ┌──────────────────────┐
  │ Agents ──► DirQ      │            │ Agents ──► DirQ      │
  │            Server    │            │            Server    │
  │            + PG      │            │            + PG      │
  └──────────┬───────────┘            └──────────┬───────────┘
             │                                   │
             ▼                                   ▼
  ┌──────────────────────────────────────────────────────────┐
  │                AAP Controller                            │
  │  Inventory Source per DC → all merge into one inventory  │
  │  Each host carries dirq_server_url from its DC           │
  └──────────────────────────────────────────────────────────┘
```

The inventory plugin sets `dirq_server_url` per host. The connection plugin reads it automatically — a host from `us-east` routes through `dirq-us-east`, a host from `eu-west` routes through `dirq-eu-west`, even in the same play.

```yaml
- hosts: tag_env_prod          # spans all DCs
  connection: atgreen.dirq.dirq
  tasks:
    - command: uptime          # routed through correct DC per host
```

---

## AAP Integration

### Collection

```bash
cd collection/atgreen/dirq
ansible-galaxy collection build
ansible-galaxy collection install atgreen-dirq-1.0.0.tar.gz
```

Includes: `atgreen.dirq.dirq` inventory plugin + connection plugin.

### Execution Environment

```yaml
# execution-environment.yml
version: 3
dependencies:
  galaxy:
    collections:
      - name: atgreen.dirq
```

```bash
ansible-builder build -t dirq-ee:latest
```

### Credential Type

Import from `collection/atgreen/dirq/docs/aap-credential-type.yml` or create manually. Injects `DIRQ_SERVER_URL` and `DIRQ_TOKEN` as environment variables.

### Setup Checklist

1. Build and publish the `atgreen.dirq` collection
2. Build a custom EE and push to your registry
3. Import the DirQ credential type in AAP
4. Create DirQ credentials (one per DC if multi-DC)
5. Add inventory sources using `atgreen.dirq.dirq` plugin
6. Create job templates with `connection: atgreen.dirq.dirq`
7. Attach DirQ credentials to job templates

---

## Configuration Reference

Both the server and agent support configuration via **config files**, **environment variables**, or both. Environment variables always override config file values, which override defaults.

### Config Files

Config files use a simple `key: value` format with optional indented `tags:` block. Comments start with `#`.

**Agent config** — `/etc/dirq/agent.conf` (Linux) or `C:\ProgramData\dirq\agent.conf` (Windows):

```
# DirQ agent configuration
server: grpc.example.com:50051
listen: 0.0.0.0:50052
exec_enabled: true

tags:
  env: prod
  dc: us-east
  role: webserver
```

**Server config** — `/etc/dirq/server.conf` (Linux) or `C:\ProgramData\dirq\server.conf` (Windows):

```
# DirQ server configuration
grpc_addr: :50051
http_addr: :8080
db_url: postgres://dirq:dirq@db.internal:5432/dirq?sslmode=require
max_zone_leaders: 10
max_children: 50
registration_secret: my-fleet-secret

tls_ca: /etc/dirq/certs/ca.crt
tls_cert: /etc/dirq/certs/server.crt
tls_key: /etc/dirq/certs/server.key
```

Override the config file path with `DIRQ_CONFIG`:

```bash
DIRQ_CONFIG=/opt/dirq/custom.conf dirq-agent
```

If the config file doesn't exist, it is silently ignored — all values fall back to environment variables or defaults.

### Config file keys ↔ environment variables

**Priority:** environment variable > config file > default.

#### Server

| Config key | Environment variable | Default | Description |
|-----------|----------|---------|-------------|
| `grpc_addr` | `DIRQ_GRPC_ADDR` | `:50051` | gRPC listen address |
| `http_addr` | `DIRQ_HTTP_ADDR` | `:8080` | REST API listen address |
| `db_url` | `DIRQ_DB_URL` | `sqlite:///var/lib/dirq/dirq.db` | Database URL (SQLite or `postgres://...`) |
| `pod_id` | `DIRQ_POD_ID` | hostname | Unique pod identifier |
| `max_zone_leaders` | `DIRQ_MAX_ZONE_LEADERS` | `5` | Max direct server connections |
| `max_children` | `DIRQ_MAX_CHILDREN` | `50` | Max children per node (fan-out) |
| `auth_disabled` | `DIRQ_AUTH_DISABLED` | `false` | Disable API auth (not recommended) |
| `registration_secret` | `DIRQ_REGISTRATION_SECRET` | | Pre-shared secret for agent registration (see [Security](#registration-authentication)) |

#### Agent

| Config key | Environment variable | Default | Description |
|-----------|----------|---------|-------------|
| `server` | `DIRQ_SERVER` | `localhost:50051` | DirQ server gRPC address |
| `listen` | `DIRQ_LISTEN` | `:50052` | Relay listener (always enabled) |
| `exec_enabled` | `DIRQ_EXEC_ENABLED` | `false` | Enable remote execution |
| `registration_secret` | `DIRQ_REGISTRATION_SECRET` | | Must match server's registration secret |
| `tags:` block | `DIRQ_TAGS` | | Tags: `env=prod,dc=us-east` |

Tags can be set in the config file as an indented block under `tags:`, or via the `DIRQ_TAGS` environment variable as comma-separated `key=value` pairs. Both sources are merged, with environment variables taking precedence for duplicate keys.

#### TLS (server and agent)

| Config key | Environment variable | Default | Description |
|-----------|----------|---------|-------------|
| `tls_ca` | `DIRQ_TLS_CA` | | CA certificate path (enables mTLS) |
| `tls_cert` | `DIRQ_TLS_CERT` | | This process's certificate path |
| `tls_key` | `DIRQ_TLS_KEY` | | This process's private key path |
| `tls_insecure` | `DIRQ_TLS_INSECURE` | `false` | Skip cert verification (agent only) |
| `tls_disabled` | `DIRQ_TLS_DISABLED` | `false` | Disable TLS entirely (not recommended) |

Example agent config with TLS and registration secret:

```
server: grpc.example.com:50051
exec_enabled: true
registration_secret: my-fleet-secret

tls_ca: /etc/dirq/certs/ca.crt
tls_cert: /etc/dirq/certs/agent.crt
tls_key: /etc/dirq/certs/agent.key

tags:
  env: prod
```

#### Signing (server only)

| Config key | Environment variable | Default | Description |
|-----------|----------|---------|-------------|
| `signing_key` | `DIRQ_SIGNING_KEY` | | Ed25519 private key file |
| `signing_pub` | `DIRQ_SIGNING_PUB` | | Ed25519 public key file |

#### Inline TLS certs (agent config)

Config files support inline base64-encoded PEM certs, so a single file contains everything an agent needs. The server generates these automatically in `/var/lib/dirq/agent.conf`.

| Config key | Environment variable | Description |
|-----------|----------|-------------|
| `tls_ca_data` | `DIRQ_TLS_CA_DATA` | Base64-encoded CA certificate PEM |
| `tls_cert_data` | `DIRQ_TLS_CERT_DATA` | Base64-encoded agent certificate PEM |
| `tls_key_data` | `DIRQ_TLS_KEY_DATA` | Base64-encoded agent private key PEM |

When `tls_ca_data`/`tls_cert_data`/`tls_key_data` are set and no file paths are given, the agent materializes them to `/var/lib/dirq/tls/` on startup.

### CLI

**Config file:** `~/.config/dirq/client.conf` (user-local, checked first) or `/etc/dirq/client.conf` (system-wide). On Windows: `%APPDATA%\dirq\client.conf` or `C:\ProgramData\dirq\client.conf`. The server generates a ready-to-copy `client.conf` at `/var/lib/dirq/client.conf`.

```
# ~/.config/dirq/client.conf
server_url: https://dirq-server:8080
token: <your-api-token>
tls_insecure: true
```

| Config key | Variable / Flag | Default | Description |
|-----------|----------------|---------|-------------|
| `server_url` | `DIRQ_SERVER_URL` / `--server` | *(required)* | Server REST URL |
| `token` | `DIRQ_TOKEN` / `--token` | | API token |
| `tls_insecure` | `DIRQ_TLS_INSECURE` / `--tls-insecure` | `false` | Skip TLS verification |
| | `--json` | `false` | Raw JSON output |

---

## REST API

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/query` | Submit a DirQ query |
| `GET` | `/api/v1/hosts` | List hosts |
| `GET` | `/api/v1/hosts/{id}` | Host details |
| `GET` | `/api/v1/hosts/{id}/facts` | Cached facts |
| `PUT` | `/api/v1/hosts/{id}/tags` | Replace tags |
| `PATCH` | `/api/v1/hosts/{id}/tags` | Merge tags |
| `DELETE` | `/api/v1/hosts/{id}/tags/{key}` | Remove tag |
| `GET` | `/api/v1/queries` | Recent queries |
| `POST` | `/api/v1/tokens` | Create token |
| `GET` | `/api/v1/tokens` | List tokens |
| `DELETE` | `/api/v1/tokens/{name}` | Delete token |
| `GET` | `/api/v1/inventory` | Ansible inventory |
| `POST` | `/api/v1/exec` | Execute command (single agent) |
| `POST` | `/api/v1/exec_multi` | Execute command/script across fleet (streaming NDJSON) |
| `POST` | `/api/v1/put_file` | Write file |
| `POST` | `/api/v1/fetch_file` | Read file |
| `GET` | `/api/v1/exec_log` | Exec audit log |
| `GET` | `/healthz` | Health check |

---

## Building

```bash
# All binaries
go build -o bin/dirq-server ./cmd/dirq-server
go build -o bin/dirq-agent  ./cmd/dirq-agent
go build -o bin/dirq         ./cmd/dirq

# Windows agent
GOOS=windows GOARCH=amd64 go build -o bin/dirq-agent.exe ./cmd/dirq-agent

# Tests
go test ./...

# Container images
podman build --target server -t dirq-server .
podman build --target agent  -t dirq-agent .
```

## Project Structure

```
cmd/
  dirq-server/            Server entrypoint
  dirq-agent/             Agent entrypoint (Windows Service support)
  dirq/                   CLI entrypoint
proto/dirq/v1/            Protobuf definitions
internal/
  server/                 gRPC, REST API, query dispatch, exec routing
  agent/                  Registration, relay mesh, query execution, exec
  query/                  DirQ DSL parser and evaluator
  modules/                System data collectors (7 modules)
  db/                     SQLite + PostgreSQL backends and data access
  tlsutil/                TLS configuration, cert generation
  signutil/               Message signing (Ed25519)
collection/atgreen/dirq/  Ansible collection for AAP
  plugins/connection/     connection: atgreen.dirq.dirq
  plugins/inventory/      inventory: atgreen.dirq.dirq
ansible/                  Standalone plugins for CLI Ansible
Containerfile             Multi-stage build
podman-compose.yml        Dev environment
execution-environment.yml EE definition for ansible-builder
```

## License

MIT License. Copyright (c) 2026 Anthony Green. See [LICENSE](LICENSE) for details.
