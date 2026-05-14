# DirQ — Direct Query Platform for Fleet Management & Ansible Execution

DirQ ("Direct Query") is an agent-based platform for querying and managing large Windows/Linux fleets. Agents form a peer-to-peer relay mesh and report data back to a central server. The server acts as an Ansible Automation Platform (AAP) inventory source, exposes collected data as structured facts, and can route Ansible execution through the mesh as an alternative to SSH/WinRM connectivity.

The key idea is simple:

- **Query the fleet like a dataset** instead of logging into hosts one by one
- **Keep managed hosts outbound-only** instead of opening SSH/WinRM inbound
- **Reuse Ansible** while replacing the transport underneath
- **Build Ansible inventories from live DirQ query results** instead of static host lists
- **Scale with a relay tree** so the server does not need a direct session to every node

One of the most practical workflows in DirQ is:

1. Query the fleet for exactly the hosts you care about
2. Turn those results into an Ansible inventory
3. Run a playbook only against that live, data-driven target set

Examples:

- Find only hosts with disks over 90%, turn that into an inventory, then run a cleanup or expansion playbook.
- Query for hosts with vulnerable OpenSSL package versions, build an inventory from the result, and patch only those systems.
- Query for hosts where `sshd` or another critical service is stopped, generate an inventory, and run a remediation playbook immediately.

## Why DirQ?

DirQ is useful when traditional fleet access patterns start breaking down:

1. **Large locked-down environments** — managed hosts cannot accept inbound SSH or WinRM.
2. **Segmented enterprise networks** — a single control plane across data centers, edge sites, or heavily firewalled zones.
3. **Real-time fleet troubleshooting** — answer "which prod hosts have disks over 90%?" and act on it immediately.
4. **Query-driven Ansible targeting** — inventories based on live fleet state, not stale static groups.
5. **Ansible without transport pain** — keep your playbooks, drop the SSH/WinRM dependency.
6. **Very large estates** — server connection count stays bounded while the fleet grows.

**What makes DirQ different:**

- **Mesh-first architecture:** agents relay for each other, so the fleet becomes its own transport.
- **Structured query model:** modules return normalized data instead of raw command output.
- **Inventory and execution in one system:** the same platform that knows the fleet can also target it.
- **Ansible compatibility:** DirQ acts as query engine, inventory source, and execution transport.

## Table of Contents

- [Architecture](#architecture) — how the mesh works, scaling
- [Quick Start](#quick-start-podman-on-laptop) — run locally in 5 minutes
- [Query DSL](#query-dsl) — the fleet query language
- [Ansible Integration](#ansible-integration) — inventory, groups, facts, query-based targeting
- [Execution Transport](#execution-transport) — run Ansible through the mesh
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
                     ┌──────────────┐
                     │  PostgreSQL  │
                     └──────────────┘
```

All links are **gRPC over TLS**. Agents connect outbound — no inbound ports required on managed hosts. Only a bounded number of zone leaders connect directly to the server.

### Components

| Component | Language | Description |
|-----------|----------|-------------|
| `dirq-server` | Go | Central server: gRPC, REST API, query engine, Ansible inventory. Runs on OpenShift or Podman. |
| `dirq-agent` | Go | Endpoint agent: collects data, relays queries, optionally executes commands. Single static binary. |
| `dirq` | Go | CLI: submit queries, manage hosts/tags/tokens, generate TLS certs. |
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
3. Fall back to direct server connection (last resort)

This eliminates the thundering herd at the server when a relay dies — orphaned
agents switch to fallback parents locally instead of all re-registering through
the server simultaneously.

### Built-in Query Modules

| Module | Data collected |
|--------|---------------|
| `cpu` | Physical/logical cores, model name, vendor |
| `memory` | Total, available, used bytes; percent used; swap |
| `disk` | Per-partition: device, mount point, fs type, total/used/free bytes, percent used |
| `os_info` | Hostname, OS, version, arch, uptime, kernel version |
| `packages` | Installed packages: name, version, arch, source (rpm/dpkg/registry) |
| `network` | Interfaces: name, MAC, MTU, flags, IP addresses (loopback filtered) |
| `services` | Services: name, display name, state, start type (systemd/Windows Services) |

---

## Quick Start (Podman on Laptop)

### Prerequisites

- Go 1.22+
- Podman and podman-compose

### 1. Start the server and database

```bash
podman-compose up -d
```

The server auto-generates TLS certs, runs DB migrations, and creates a bootstrap API token (printed to the log). Grab the token:

```bash
podman logs dirq_dirq-server_1 2>&1 | grep "DIRQ_TOKEN"
```

### 2. Build and run the agent

```bash
go build -o bin/dirq-agent ./cmd/dirq-agent
DIRQ_TLS_DISABLED=true ./bin/dirq-agent
```

(Use `DIRQ_TLS_DISABLED=true` for local dev since the server's auto-generated certs won't match the agent's.)

### 3. Build and use the CLI

```bash
go build -o bin/dirq ./cmd/dirq
export DIRQ_TOKEN=<bootstrap-token-from-step-1>

# For local dev without TLS on the REST API:
export DIRQ_SERVER_URL=http://localhost:8090

./bin/dirq hosts list
./bin/dirq select os_info.hostname, cpu.logical_cores, memory.pct_used
./bin/dirq select os_info.hostname, packages.name, packages.version WHERE packages.name IN "'openssl', 'curl'"
./bin/dirq hosts tag <agent-id> env=dev role=workstation
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
Uses depth-first rolling deployment by default — deepest nodes first, working
up the tree so a parent is never updated while its children are mid-install.

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

### Arg flattening

Any quoted argument containing spaces is split into individual args before
parsing. This means all DirQ commands work both quoted and unquoted:

```bash
dirq "hosts list"                    # same as: dirq hosts list
dirq "select hostname where tag.env = 'prod'"  # same as: dirq select hostname where ...
dirq "run deploy.yml where tag.env = 'prod'"   # same as: dirq run deploy.yml where ...
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
dirq hosts tag <agent-id> env=prod role=webserver dc=us-east
dirq hosts untag <agent-id> role dc
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

### Server

| Variable | Default | Description |
|----------|---------|-------------|
| `DIRQ_GRPC_ADDR` | `:50051` | gRPC listen address |
| `DIRQ_HTTP_ADDR` | `:8080` | REST API listen address |
| `DIRQ_DB_URL` | `postgres://dirq:dirq@localhost:5432/dirq?sslmode=disable` | PostgreSQL connection string |
| `DIRQ_POD_ID` | hostname | Unique pod identifier |
| `DIRQ_MAX_ZONE_LEADERS` | `5` | Max direct server connections |
| `DIRQ_MAX_CHILDREN` | `50` | Max children per node (fan-out) |
| `DIRQ_AUTH_DISABLED` | `false` | Disable API auth (not recommended) |

### Agent

| Variable | Default | Description |
|----------|---------|-------------|
| `DIRQ_SERVER` | `localhost:50051` | DirQ server gRPC address |
| `DIRQ_LISTEN` | `:50052` | Relay listener (always enabled) |
| `DIRQ_TAGS` | | Tags: `env=prod,dc=us-east` |
| `DIRQ_EXEC_ENABLED` | `false` | Enable remote execution |

### TLS (server and agent)

| Variable | Default | Description |
|----------|---------|-------------|
| `DIRQ_TLS_CA` | | CA certificate (enables mTLS) |
| `DIRQ_TLS_CERT` | | This process's certificate |
| `DIRQ_TLS_KEY` | | This process's private key |
| `DIRQ_TLS_INSECURE` | `false` | Skip cert verification (agent only) |
| `DIRQ_TLS_DISABLED` | `false` | Disable TLS entirely (not recommended) |

### CLI

| Variable / Flag | Default | Description |
|----------------|---------|-------------|
| `DIRQ_SERVER_URL` / `--server` | `http://localhost:8080` | Server REST URL |
| `DIRQ_TOKEN` / `--token` | | API token |
| `--json` | `false` | Raw JSON output |

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
| `POST` | `/api/v1/exec` | Execute command |
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
  db/                     PostgreSQL schema and data access
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
