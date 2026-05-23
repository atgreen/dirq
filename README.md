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
- Quick ad-hoc check: `dirq exec WHERE tag.env = 'prod' -- uptime` to see every prod host's uptime without setting up a playbook.

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
- [Fleet Exec](#fleet-exec) — ad-hoc parallel command, script, and grep execution
- [Topology Graph](#topology-graph) — visualize the agent mesh tree
- [Fleet-Scale Emulation](#fleet-scale-emulation) — one agent process hosting N virtual hosts
- [Debug & Diagnostics](#debug--diagnostics) — `dirq debug` subcommands for in-flight sessions and mesh reachability
- [Security](#security) — TLS, authentication, exec safety
- [High Availability](HA.md) — multi-pod deployment on OpenShift/Kubernetes
- [Multi-Datacenter Deployment](#multi-datacenter-deployment) — isolated meshes, per-DC routing
- [AAP Integration](#aap-integration) — collection, EE, credentials, setup checklist
- [MCP Integration](#mcp-integration) — use dirq as an AI tool via Model Context Protocol
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
| `dirq` | Go | CLI: submit queries, manage hosts/tags/tokens, run ad-hoc commands, generate and rotate certificates. |
| `atgreen.dirq` | Python | Ansible collection: inventory plugin + connection plugin for AAP. |

### Scaling the Mesh

The server holds a fixed number of zone leader connections (default 5). All other agents fill a tree below those zone leaders, growing as deep as needed (BFS fill order).

| Fleet size | Tree depth | Server connections |
|-----------|-----------|-------------------|
| 250 | 2 | 5 |
| 12,500 | 3 | 5 |
| 625,000 | 4 | 5 |

The server always holds exactly `DIRQ_MAX_ZONE_LEADERS` connections regardless of fleet size. The tree deepens — it never widens at the server.

The live mesh shape is held **in memory** by the server (`MeshTopology`, RWMutex-protected maps for nodes, ZLs, parent/child links, depth cache). Registration, fan-out, and dispatch all read this directly — no DB round-trips on hot paths. `agents.role` and `agents.parent_id` are best-effort snapshots persisted every 30 s for operator visibility and rehydrated on restart. The CLI overlays the in-memory view onto DB records before serializing, so `dirq hosts list` always reflects live truth.

Registration arrivals flow through a **burst-aware batcher** (default 200 ms window, 200 max batch). On flush, the assigner prefers one zone leader per distinct source IP — so a thundering herd from a single subnet can't fill all ZL slots from one host. There is no proactive rebalancer; reactive recovery (`reassignOrphans` on stream close, fallback parents + orphan promotion via `RequestPeers`) handles every churn case the old proactive paths used to.

### Honest completion reporting

Broadcast dispatchers (query, exec, deploy) use **per-target accounting** instead of an idle timeout. A session's loop runs until every target is accounted — either by a real response or a synthetic disconnect failure synthesized from one of four mesh-state signals (zone-leader stream close, `PeerDisconnected` from a relay, the periodic reaper, or a fanout-buffer-full at dispatch time). All four paths funnel through one first-terminal-wins gate (`ClaimAgent`) so no agent is counted twice.

The hard timeout is `command_timeout + 30 s` and is a true safety net — rarely the actual completion driver. Practical consequence: `dirq exec --timeout 3600 -- yum upgrade -y` doesn't get cut off at 30 s of silence between fast and slow responders. When a dispatcher can't account for everyone, the CLI prints `Status: incomplete | Targets: N | Received: M | Missing: K` instead of claiming completion.

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

Aggregates work with or without `GROUP BY`:

```sql
-- Fleet-wide total (bare aggregate)
SELECT COUNT(hostname) WHERE os_info.os = 'linux'

-- Per-group breakdown
SELECT os_info.os, COUNT(hostname) GROUP BY os_info.os
```

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
dirq hosts graph
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

Export to Graphviz DOT format for rendering (left-to-right layout fits large fleet trees on screen):

```bash
dirq hosts graph --dot | dot -Tpng -o topology.png
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
  Database                       ok   postgres connected
  Agents online                  ok   1247/1250
  Agent version skew             !!   3 agents on v0.21.x (server is v0.22.3)
  Relay tree                     ok   depth 4, 5 zone leader(s)
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

Other commands are **not** flattened. For `dirq exec`, the remote command goes after `--` so flags and special characters pass through without conflict:

```bash
dirq exec WHERE tag.env = 'prod' -- ls -l   # everything after -- is the remote command
```

---

## Fleet-Scale Emulation

For testing mesh behavior at fleet scale without provisioning one VM per host, a single `dirq-agent` process can host **N virtual hosts** in-process. Each VH presents itself to the server as an independent agent with its own ID, session token, mTLS client cert, upstream gRPC connection, and downstream relay listen port.

```bash
DIRQ_VIRTUAL_HOSTS=25 \
DIRQ_HOSTNAME_PREFIX=dirq-test-linux-1 \
DIRQ_REGISTRATION_JITTER_SECONDS=30 \
./bin/dirq-agent
```

Synthesized hostnames are `<prefix>-NNNNN`. Per-instance mTLS material lives under `$DATA_DIR/tls/instances/<hostname>/` so siblings can't clobber each other. The relay listener binds synchronously in `Run()` before registration, so port collisions surface as a startup error instead of silently failing later.

The AWS test fleet (`make aws`) exposes this via `DIRQ_REPLICAS_PER_VM`:

```bash
LINUX_COUNT=50 DIRQ_REPLICAS_PER_VM=1000 make aws    # 50,000 emulated hosts on 50 VMs
```

The userdata script auto-widens the SG relay port range to `50052..50051+N`, reserves the ephemeral-port block via `net.ipv4.ip_local_reserved_ports` so concurrent `dnf install` doesn't collide with VH listen sockets, and picks a sensible registration-jitter default (N/4 s, clamped to 5–60 s) when running with >1 VH.

Multi-VH is **Linux-only** (Windows VMs stay single-tenant).

**Per-VM density caveat:** every emulated VH runs its own gRPC stream + state, but they all share the host kernel, CPU, and memory. Running heavy workloads (a real `dnf install`, large package syncs) at 25 VHs/VM on a t3.small saturates the CPU enough that gRPC heartbeats time out and dirq honestly reports VHs as `peer disconnected`. That's a property of the emulation density, not the mesh — production deployments with 1 agent per real host don't have it. For heavy-workload emulation, prefer CPU-rich instance types (`c6i.large`+) or drop density to ~10 VHs/VM.

---

## Debug & Diagnostics

`dirq debug` covers diagnostic tools used when something looks wrong in the mesh. All endpoints are admin-scoped.

| Command | Purpose |
|---|---|
| `dirq debug inflight` | List every exec / query / deploy session the server is currently coordinating, with the still-missing agent set, arrivals-in-the-last-1/5/30 s, and a per-zone-leader breakdown (`subtree`, `pending`, `send_buf`). Marks the chokepoint ZL with `← bottleneck (send_buf full)` when its stream-send buffer is at capacity. |
| `dirq debug path <hostname>` | Walk the agent's mesh parent chain from the DB snapshot. Flags broken links. Fastest, DB-only. |
| `dirq debug stream <hostname>` | Show the server's in-memory view of how it would currently reach this agent (directly connected vs. routed through a zone leader). |
| `dirq debug ping <hostname>` | Send a no-op exec through the mesh and report round-trip timing. Slowest of the three lookup tools but the only one that proves a message actually reaches the agent right now. |

The three lookup tools form a hierarchy of trust — `path` (DB), then `stream` (live process state), then `ping` (end-to-end proof).

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
handling — the broadcast dispatcher has no idle timeout, so `--timeout 3600` against
a slow fleet behaves as written rather than getting cut off after the first burst of
fast responders. Exec responses are forwarded immediately through the relay chain —
they are not batched by the result aggregator.

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
dirq exec -- uptime
dirq exec WHERE tag.env = 'prod' -- openssl version
dirq exec --become WHERE tag.role = 'webserver' -- systemctl restart nginx
dirq exec -- hostname -f
dirq exec --json -- df -h /
```

### Scripts

Upload and execute a local script file with `--script`. Linux scripts honor their shebang. Windows `.ps1` files run with PowerShell.

```bash
dirq exec WHERE tag.env = 'prod' --script ./health-check.sh
dirq exec WHERE os_info.os = 'windows' --script ./audit.ps1
dirq exec WHERE tag.role = 'webserver' --become --script ./patch.sh
```

With `--script`, no `--` separator is needed since the script path is a dirq flag, not a remote command.

### Fleet Grep

Search log files across the fleet without a centralized logging stack. Uses `grep` on Linux and `Select-String` on Windows.

```bash
dirq grep "Out of memory" /var/log/messages
dirq grep -i "error|timeout" /var/log/nginx/error.log WHERE tag.env = 'prod'
dirq grep "FATAL" /var/log/app.log --tail 1000
dirq grep "Failed password" /var/log/secure --become
```

Results are formatted as a table with matches grouped by host:

```
HOST                 LINE  MATCH
web-prod-01          4821  Jan 15 03:22:41 kernel: Out of memory: Killed process 1234 (java)
web-prod-01          6103  Jan 15 08:14:02 kernel: Out of memory: Killed process 5678 (python3)
db-prod-02          11042  Jan 14 22:01:18 kernel: Out of memory: Killed process 891 (mysqld)

3 matches across 2 hosts (15 hosts searched)
```

Use `--tail N` to search only the last N lines of a file (avoids scanning multi-GB logs). Use `--become` for files that require root access (e.g. `/var/log/secure`).

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
| Nothing | Auto-generate self-signed + mTLS cert issuance per agent |
| `CERT` + `KEY` | TLS with user certs, no mTLS |
| `CERT` + `KEY` + `CA` + `CA_KEY` | Full mTLS with user-supplied CA |
| `DIRQ_TLS_DISABLED=true` | Explicitly insecure (must opt in) |

#### Per-agent mTLS certificates

When the server has access to the CA private key (auto-generated or via `DIRQ_TLS_CA_KEY`), it issues a **unique TLS client certificate** to each agent during registration. The certificate's CN is the agent ID, binding the TLS identity to the application identity.

After registration:
- All gRPC connections (AgentStream, RequestPeers, relay) require a valid client cert signed by the server's CA
- The server and relay agents verify that the cert CN matches the claimed agent ID
- The registration secret becomes a **one-time bootstrap token** — a leaked secret can register an agent once, but the cert it receives is bound to that specific agent ID

This activates automatically when the CA key is available. On auto-generated certs, it's always on. For user-supplied certs, set `DIRQ_TLS_CA_KEY`.

Agents persist their issued cert to disk and reuse it across restarts. Certs are valid for 1 year; agents renew automatically when within 30 days of expiry (no restart needed).

**Generate certs:**
```bash
# Self-signed CA (quick start)
dirq cert generate --dir ./certs

# Use your own CA
dirq cert generate --ca ./my-ca.crt --ca-key ./my-ca.key --dir ./certs
```

Both generate `server.crt`, `server.key`, `agent.crt`, `agent.key`, and a copy of `ca.crt` in the output directory.

**Full mTLS with user-supplied CA:**
```bash
# Server (needs CA key to issue per-agent certs)
DIRQ_TLS_CA=./certs/ca.crt DIRQ_TLS_CA_KEY=./certs/ca.key \
DIRQ_TLS_CERT=./certs/server.crt DIRQ_TLS_KEY=./certs/server.key dirq-server

# Agent (only needs CA cert — gets its own cert during registration)
DIRQ_TLS_CA=./certs/ca.crt dirq-agent
```

#### Certificate rotation

Rotate certificates across the fleet without downtime:

```bash
dirq cert rotate agent_cert --stagger 3600   # renew all agent certs over 1 hour
dirq cert rotate ca --stagger 3600           # distribute a new CA
dirq cert rotate signing_key                 # roll the message signing key
```

The `--stagger` flag spreads renewals over time to avoid overloading the server. See [SECURITY.md](SECURITY.md) for the full rotation procedure including CA and signing key rotation.

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

## MCP Integration

DirQ includes a built-in [Model Context Protocol](https://modelcontextprotocol.io/) (MCP) server, allowing LLMs like Claude to manage your fleet directly as a tool.

### Setup

Start the MCP server:

```bash
dirq mcp
```

This runs an MCP stdio server that exposes fleet management tools over JSON-RPC 2.0.

#### Claude Desktop

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

#### Claude Code

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

### Available Tools

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

### Example Prompts

With the MCP server configured, you can ask Claude things like:

- "Which hosts in prod have more than 80% disk usage?"
- "Are any of our RHEL hosts vulnerable to CVE-2024-6345?"
- "Tag all Windows hosts with role=iis"
- "Run `uptime` on all Linux hosts in staging"
- "Show me the fleet topology"

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
| `leader_election` | `DIRQ_LEADER_ELECTION` | `false` | Enable Postgres advisory-lock leader election for multi-pod HA (see [HA.md](HA.md)) |
| `fact_flush_interval` | `DIRQ_FACT_FLUSH_INTERVAL` | `250ms` | Fact-cache batch flush interval |
| `fact_flush_size` | `DIRQ_FACT_FLUSH_SIZE` | `5000` | Distinct (agent_id, module) keys per flush |
| `fact_stage_cap` | `DIRQ_FACT_STAGE_CAP` | `20000` | Hard cap on staged distinct keys (drops only new keys on saturation) |

#### Agent

| Config key | Environment variable | Default | Description |
|-----------|----------|---------|-------------|
| `server` | `DIRQ_SERVER` | `localhost:50051` | DirQ server gRPC address |
| `listen` | `DIRQ_LISTEN` | `:50052` | Relay listener (always enabled) |
| `exec_enabled` | `DIRQ_EXEC_ENABLED` | `false` | Enable remote execution |
| `registration_secret` | `DIRQ_REGISTRATION_SECRET` | | Must match server's registration secret |
| `tags:` block | `DIRQ_TAGS` | | Tags: `env=prod,dc=us-east` |
| `hostname` | `DIRQ_HOSTNAME` | (autodetected) | Override the hostname the agent reports |
| `virtual_hosts` | `DIRQ_VIRTUAL_HOSTS` | `0` | Spawn N in-process virtual hosts for fleet emulation (Linux only) |
| `hostname_prefix` | `DIRQ_HOSTNAME_PREFIX` | | Prefix for synthesized virtual-host names (`<prefix>-NNNNN`) |
| `registration_jitter_seconds` | `DIRQ_REGISTRATION_JITTER_SECONDS` | (auto for multi-VH) | Cap on random startup delay before first `Register`; smooths thundering-herd boot |

Tags can be set in the config file as an indented block under `tags:`, or via the `DIRQ_TAGS` environment variable as comma-separated `key=value` pairs. Both sources are merged, with environment variables taking precedence for duplicate keys.

#### TLS (server and agent)

| Config key | Environment variable | Default | Description |
|-----------|----------|---------|-------------|
| `tls_ca` | `DIRQ_TLS_CA` | | CA certificate path |
| `tls_ca_key` | `DIRQ_TLS_CA_KEY` | | CA private key path (server only — enables per-agent mTLS cert issuance) |
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
| `llm_url` | `DIRQ_LLM_URL` | | LLM API base URL (Anthropic or OpenAI-compatible) |
| `llm_api_key` | `DIRQ_LLM_API_KEY` | | LLM API key |
| `llm_model` | `DIRQ_LLM_MODEL` | `claude-sonnet-4-20250514` | LLM model name |
| | `--json` | `false` | Raw JSON output |

For `dirq ask`, if `DIRQ_LLM_*` is not configured, falls back to `ANTHROPIC_API_KEY` with Anthropic's native API.

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
| `GET` | `/api/v1/debug/inflight` | In-flight broadcast sessions with per-ZL breakdown (admin) |
| `GET` | `/api/v1/status` | Fleet status (agent counts, ZLs, tree depth, database kind) |
| `GET` | `/healthz` | Liveness — process is up |
| `GET` | `/readyz` | Readiness — this pod is the active leader (200) or a standby (503); always 200 when leader election is disabled |

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
