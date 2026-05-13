# DirQ — Real-Time Endpoint Query & Ansible Execution Platform

DirQ is an agent-based platform for querying and managing large Windows/Linux fleets. Agents form a peer-to-peer relay mesh and report data back to a central server. The server acts as an Ansible Automation Platform (AAP) inventory source, exposing collected data as Ansible facts.

The relay mesh also serves as an **Ansible execution transport** — AAP can run playbooks against managed hosts through the DirQ mesh using `connection: dirq`, replacing SSH/WinRM entirely. No inbound ports required on managed hosts.

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

All links in the mesh are **gRPC over mTLS**. Agents connect outbound — no inbound ports required on managed hosts. Only zone leaders connect directly to the server, keeping OpenShift router load low even at 100k nodes.

### Scaling the Mesh

The server holds a fixed number of zone leader connections (default 5). All
other agents are placed in a tree below those zone leaders, which grows as
deep as needed. There is no fixed depth limit — the tree fills in BFS order
(shallowest available node gets the next child).

Two environment variables control the shape:

| Variable | Default | Description |
|----------|---------|-------------|
| `DIRQ_MAX_ZONE_LEADERS` | `5` | Direct server connections (caps server load) |
| `DIRQ_MAX_CHILDREN` | `50` | Max children per node (fan-out ratio) |

Capacity with defaults (5 ZLs, fan-out 50):

| Fleet size | Tree depth | Server connections |
|-----------|-----------|-------------------|
| 250 | 2 | 5 |
| 12,500 | 3 | 5 |
| 625,000 | 4 | 5 |
| 31,250,000 | 5 | 5 |

The server always holds exactly `DIRQ_MAX_ZONE_LEADERS` connections regardless
of fleet size. To handle more agents, the tree simply gets deeper — not wider
at the server.

```bash
# Default (625k capacity at depth 4)
DIRQ_MAX_ZONE_LEADERS=5 DIRQ_MAX_CHILDREN=50

# More zone leaders for lower latency at scale
DIRQ_MAX_ZONE_LEADERS=10 DIRQ_MAX_CHILDREN=50  # 1.25M at depth 4

# Higher fan-out for flatter tree
DIRQ_MAX_ZONE_LEADERS=5 DIRQ_MAX_CHILDREN=100  # 5M at depth 4
```

The algorithm:
1. First `DIRQ_MAX_ZONE_LEADERS` agents become **zone leaders** (connect to server)
2. All subsequent agents are placed under the shallowest node with room
3. If the entire tree is full, an extra zone leader is added (soft limit)

## Query DSL

DirQ has a SQL-like query language for ad-hoc fleet queries. Queries are parsed on
the server, pushed through the relay mesh to agents, filtered agent-side, and
aggregated server-side. Results stream back in real time.

### Syntax

```
SELECT <fields>
[FROM <scope>]
[WHERE <conditions>]
[GROUP BY <field>]
[ORDER BY <field> [DESC]]
```

Every clause except `SELECT` is optional. When `FROM` is omitted, all online hosts
are targeted.

### Fields

Fields reference data collected by agent modules using dotted notation:

| Field | Module | Description |
|-------|--------|-------------|
| `cpu.physical_cores` | cpu | Number of physical CPU cores |
| `cpu.logical_cores` | cpu | Number of logical CPU cores (with hyperthreading) |
| `cpu.model_name` | cpu | CPU model string |
| `cpu.vendor` | cpu | CPU vendor (e.g. GenuineIntel, AuthenticAMD) |
| `memory.total_bytes` | memory | Total physical RAM in bytes |
| `memory.available_bytes` | memory | Available RAM in bytes |
| `memory.used_bytes` | memory | Used RAM in bytes |
| `memory.pct_used` | memory | Memory utilization percentage |
| `memory.swap_total_bytes` | memory | Total swap in bytes |
| `memory.swap_used_bytes` | memory | Used swap in bytes |
| `disk.partitions` | disk | Array of partition objects (see below) |
| `os_info.hostname` | os_info | System hostname |
| `os_info.os` | os_info | Operating system (`linux` or `windows`) |
| `os_info.os_version` | os_info | OS version string |
| `os_info.arch` | os_info | Architecture (`amd64`, `arm64`) |
| `os_info.uptime_seconds` | os_info | System uptime |
| `os_info.kernel_version` | os_info | Kernel version string |
| `packages.packages` | packages | Array of installed package objects (see below) |
| `network.interfaces` | network | Array of network interface objects (see below) |
| `services.services` | services | Array of system service objects (see below) |

Each disk partition contains: `device`, `mount_point`, `fs_type`, `total_bytes`,
`used_bytes`, `free_bytes`, `pct_used`.

Each package contains: `name`, `version`, `arch`, `source` (rpm, dpkg, or registry).

Each network interface contains: `name`, `mac`, `mtu`, `flags` (array), `addresses`
(array of `{addr, family}`). Loopback interfaces are filtered out.

Each service contains: `name`, `display_name`, `state` (running, stopped, etc.),
`start_type` (enabled, disabled, static, manual).

### FROM — target scope

```sql
FROM *                  -- all online hosts (default if omitted)
FROM tag:prod           -- hosts with tag key "prod"
FROM group:webservers   -- hosts in group "webservers"
```

### WHERE — filtering

Conditions are joined with `AND`. Filtering happens agent-side to minimize
network traffic.

```sql
WHERE disk.pct_used > 80
WHERE cpu.logical_cores >= 8 AND memory.pct_used > 50
WHERE os_info.os = 'linux'
WHERE os_info.kernel_version LIKE '7.0%'
WHERE packages.name IN ('openssl', 'nginx', 'curl')
WHERE services.name = 'sshd' AND services.state = 'stopped'
```

**Operators:** `=`, `!=`, `>`, `<`, `>=`, `<=`, `LIKE`, `IN`

String values must be single-quoted. Numeric values are bare. `LIKE` supports
`%` as a wildcard (leading, trailing, or both). `IN` takes a parenthesized list
of single-quoted strings.

### Array-aware filtering

Modules that return arrays (packages, services, disk, network) support
field-level filtering into the array elements. When a WHERE condition references
a field inside an array module, the agent iterates the array and returns only
matching entries.

```sql
-- Returns only the 3 requested packages, not all 2000 installed
SELECT os_info.hostname, packages.name, packages.version
FROM *
WHERE packages.name IN ('openssl', 'nginx', 'curl')

-- Returns only partitions over 80% full, not all partitions
SELECT os_info.hostname, disk.mount_point, disk.pct_used
FROM *
WHERE disk.pct_used > 80

-- Returns only the matching services
SELECT os_info.hostname, services.name, services.state
FROM *
WHERE services.name = 'sshd' AND services.state = 'stopped'
```

This filtering happens agent-side, so only matching data crosses the network.

### GROUP BY — aggregation

When `GROUP BY` is present, select fields should be either the group key or
aggregation functions. Aggregation runs server-side after collecting results
from all agents.

```sql
SELECT os_info.os, COUNT(os_info.hostname), AVG(memory.total_bytes)
FROM *
GROUP BY os_info.os
```

**Aggregation functions:** `COUNT(field)`, `AVG(field)`, `SUM(field)`, `MIN(field)`, `MAX(field)`

### ORDER BY — sorting

```sql
ORDER BY disk.pct_used DESC    -- descending (highest first)
ORDER BY memory.total_bytes    -- ascending (default)
```

### Examples

```sql
-- Find hosts with disks over 80% full (only matching partitions returned)
SELECT os_info.hostname, disk.mount_point, disk.pct_used
FROM tag:prod
WHERE disk.pct_used > 80
ORDER BY disk.pct_used DESC

-- Check specific package versions across the fleet
SELECT os_info.hostname, packages.name, packages.version
FROM *
WHERE packages.name IN ('openssl', 'nginx', 'curl')

-- Find hosts where sshd is stopped
SELECT os_info.hostname, services.name, services.state
FROM *
WHERE services.name = 'sshd' AND services.state = 'stopped'

-- Count hosts and average RAM by OS
SELECT os_info.os, COUNT(os_info.hostname), AVG(memory.total_bytes)
FROM *
GROUP BY os_info.os

-- Find beefy hosts
SELECT os_info.hostname, cpu.logical_cores, memory.total_bytes
FROM *
WHERE cpu.logical_cores >= 16 AND memory.total_bytes > 34000000000

-- Linux hosts running a specific kernel
SELECT os_info.hostname, os_info.kernel_version
FROM *
WHERE os_info.os = 'linux' AND os_info.kernel_version LIKE '7.0%'

-- Find packages matching a pattern
SELECT os_info.hostname, packages.name, packages.version
FROM *
WHERE packages.name LIKE 'openssl%'

-- Swap usage across the fleet
SELECT os_info.hostname, memory.swap_used_bytes, memory.swap_total_bytes
FROM *
WHERE memory.swap_used_bytes > 0
```

### CLI usage

```bash
dirq query "SELECT os_info.hostname, cpu.logical_cores FROM *"
dirq query "SELECT os_info.hostname, disk.pct_used FROM tag:prod WHERE disk.pct_used > 80" --timeout 30
dirq query "SELECT os_info.os, COUNT(os_info.hostname) FROM * GROUP BY os_info.os" --json
```

## Components

| Component | Language | Description |
|-----------|----------|-------------|
| `dirq-server` | Go | Central server. gRPC service for agents, REST API for admins, Ansible inventory endpoint. Runs on OpenShift (production) or Podman (dev). |
| `dirq-agent` | Go | Lightweight agent. Runs on managed Linux/Windows servers. Collects system data, relays queries, and optionally executes commands through the P2P mesh. Single static binary, zero dependencies. |
| `dirq` | Go | CLI tool. Submit queries, list hosts, manage API tokens. |
| `dirq_inventory.py` | Python | Ansible dynamic inventory plugin. Connects to the DirQ REST API, exposes hosts and facts to AAP/awx. |
| `connection: dirq` | Python | Ansible connection plugin. Routes playbook execution through the DirQ mesh instead of SSH/WinRM. |

## Built-in Query Modules

| Module | Data collected |
|--------|---------------|
| `cpu` | Physical/logical cores, model name, vendor |
| `memory` | Total, available, used bytes; percent used; swap |
| `disk` | Per-partition: device, mount point, filesystem type, total/used/free bytes, percent used |
| `os_info` | Hostname, OS, version, arch, uptime, kernel version |
| `packages` | Installed packages: name, version, arch, source (rpm/dpkg on Linux, registry on Windows) |
| `network` | Network interfaces: name, MAC, MTU, flags, IP addresses with family (loopback filtered) |
| `services` | System services: name, display name, state, start type (systemd on Linux, Windows Services) |

## Quick Start (Podman on Laptop)

### Prerequisites

- Go 1.22+
- Podman and podman-compose
- PostgreSQL client (optional, for direct DB access)

### 1. Start the server and database

```bash
podman-compose up -d
```

This starts PostgreSQL and the DirQ server. The server runs migrations automatically on startup.

- REST API: http://localhost:8080
- gRPC: localhost:50051
- Health check: http://localhost:8080/healthz

### 2. Build and run the agent

```bash
# Build
go build -o bin/dirq-agent ./cmd/dirq-agent

# Run (connects to localhost:50051 by default)
./bin/dirq-agent
```

The agent registers with the server and begins accepting queries. Run multiple agents to simulate a fleet.

**Windows:**

```powershell
# Build
$env:GOOS="windows"; $env:GOARCH="amd64"; go build -o bin\dirq-agent.exe .\cmd\dirq-agent

# Run in foreground
.\bin\dirq-agent.exe

# Install as a Windows Service (runs as SYSTEM)
.\bin\dirq-agent.exe install

# Start/stop the service
sc start DirQAgent
sc stop DirQAgent

# Uninstall the service
.\bin\dirq-agent.exe uninstall
```

When running as a Windows Service, the agent runs as the SYSTEM account, which has full administrative privileges. This is required for remote execution — SYSTEM can launch processes as any local user without password prompts.

### 3. Build and use the CLI

```bash
# Build
go build -o bin/dirq ./cmd/dirq

# List registered hosts
./bin/dirq hosts list

# Run a query
./bin/dirq query "SELECT hostname, cpu.cores, memory.pct_used FROM *"

# Query with filtering
./bin/dirq query "SELECT hostname, disk.mount, disk.pct_used FROM * WHERE disk.pct_used > 50"

# Query packages, network, services
./bin/dirq query "SELECT os_info.hostname, packages.packages FROM *"
./bin/dirq query "SELECT os_info.hostname, network.interfaces FROM *"
./bin/dirq query "SELECT os_info.hostname, services.services FROM *"

# Manage tags on a host
./bin/dirq hosts tag <agent-id> env=prod role=webserver dc=us-east
./bin/dirq hosts untag <agent-id> role dc
```

### 4. Ansible inventory

```bash
# Set environment
export DIRQ_SERVER_URL=http://localhost:8080

# Test the inventory plugin
./ansible/dirq_inventory.py --list

# Use with Ansible
ansible-inventory -i ansible/dirq_inventory.py --list
ansible all -i ansible/dirq_inventory.py -m ping
```

## Configuration

### Server (environment variables)

| Variable | Default | Description |
|----------|---------|-------------|
| `DIRQ_GRPC_ADDR` | `:50051` | gRPC listen address |
| `DIRQ_HTTP_ADDR` | `:8080` | HTTP/REST listen address |
| `DIRQ_DB_URL` | `postgres://dirq:dirq@localhost:5432/dirq?sslmode=disable` | PostgreSQL connection string |
| `DIRQ_POD_ID` | hostname | Unique pod identifier (for multi-pod deployments) |
| `DIRQ_MAX_ZONE_LEADERS` | `5` | Max agents connecting directly to the server |
| `DIRQ_MAX_CHILDREN` | `50` | Max children per relay or zone leader node |

### Agent (environment variables)

| Variable | Default | Description |
|----------|---------|-------------|
| `DIRQ_SERVER` | `localhost:50051` | DirQ server gRPC address |
| `DIRQ_LISTEN` | `:50052` | Address to listen on for downstream peers |
| `DIRQ_TAGS` | | Comma-separated tags: `env=prod,dc=us-east` |
| `DIRQ_EXEC_ENABLED` | `false` | Enable remote command execution |

### TLS (environment variables — apply to both server and agent)

| Variable | Default | Description |
|----------|---------|-------------|
| `DIRQ_TLS_CA` | | Path to CA certificate (enables mTLS verification) |
| `DIRQ_TLS_CERT` | | Path to this process's TLS certificate |
| `DIRQ_TLS_KEY` | | Path to this process's TLS private key |
| `DIRQ_TLS_INSECURE` | `false` | Skip cert verification (agent only — for self-signed certs) |

When `DIRQ_TLS_CERT` and `DIRQ_TLS_KEY` are set, TLS is enabled. When they
are not set, all gRPC connections run unencrypted (dev mode).

### CLI (environment variables and flags)

| Variable / Flag | Default | Description |
|----------------|---------|-------------|
| `DIRQ_SERVER_URL` / `--server` | `http://localhost:8080` | DirQ server REST URL |
| `DIRQ_TOKEN` / `--token` | | API token |
| `--json` | `false` | Output raw JSON |

## TLS

All gRPC connections (server-to-agent, agent-to-agent relay) support TLS.
Three modes:

### 1. No TLS (development)

Don't set any `DIRQ_TLS_*` variables. All connections are unencrypted.
This is the default for local development.

### 2. Self-signed certificates

Generate a CA, server cert, and agent cert with one command:

```bash
./bin/dirq tls generate --dir ./certs
```

This creates:
```
certs/
  ca.crt, ca.key         — Certificate Authority (10-year validity)
  server.crt, server.key — Server cert (1-year, SANs: localhost, dirq-server)
  agent.crt, agent.key   — Agent cert (1-year)
```

**Server:**
```bash
DIRQ_TLS_CA=./certs/ca.crt \
DIRQ_TLS_CERT=./certs/server.crt \
DIRQ_TLS_KEY=./certs/server.key \
  ./bin/dirq-server
```

**Agent (with CA verification):**
```bash
DIRQ_TLS_CA=./certs/ca.crt \
DIRQ_TLS_CERT=./certs/agent.crt \
DIRQ_TLS_KEY=./certs/agent.key \
  ./bin/dirq-agent
```

**Agent (skip verification — when you don't want to distribute the CA):**
```bash
DIRQ_TLS_CERT=./certs/agent.crt \
DIRQ_TLS_KEY=./certs/agent.key \
DIRQ_TLS_INSECURE=true \
  ./bin/dirq-agent
```

### 3. User-supplied certificates

Point the environment variables at your own CA-signed certificates.
Any standard x509 certs work — from your corporate CA, Let's Encrypt, etc.

```bash
# Server
DIRQ_TLS_CA=/etc/pki/dirq/ca-bundle.crt \
DIRQ_TLS_CERT=/etc/pki/dirq/server.crt \
DIRQ_TLS_KEY=/etc/pki/dirq/server.key \
  ./bin/dirq-server

# Agent
DIRQ_TLS_CA=/etc/pki/dirq/ca-bundle.crt \
DIRQ_TLS_CERT=/etc/pki/dirq/agent.crt \
DIRQ_TLS_KEY=/etc/pki/dirq/agent.key \
  ./bin/dirq-agent
```

### mTLS (mutual TLS)

When `DIRQ_TLS_CA` is set on the **server**, it enforces mutual TLS —
agents must present a valid certificate signed by that CA. This prevents
unauthorized agents from joining the mesh.

When `DIRQ_TLS_CA` is set on the **agent**, it verifies the server's
certificate against the CA. This prevents agents from connecting to a
rogue server.

For full mutual authentication, set `DIRQ_TLS_CA` on both sides.

## API Tokens

```bash
# Create a token
./bin/dirq token create my-token --scope admin

# List tokens
./bin/dirq token list

# Use with CLI
export DIRQ_TOKEN=<token-value>
./bin/dirq hosts list

# Use with curl
curl -H "Authorization: Bearer <token>" http://localhost:8080/api/v1/hosts
```

## REST API

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/query` | Submit a DirQ query |
| `GET` | `/api/v1/hosts` | List registered hosts |
| `GET` | `/api/v1/hosts/{id}` | Get host details |
| `GET` | `/api/v1/hosts/{id}/facts` | Get cached facts for a host |
| `PUT` | `/api/v1/hosts/{id}/tags` | Replace all tags on a host |
| `PATCH` | `/api/v1/hosts/{id}/tags` | Merge tags (add/update without removing) |
| `DELETE` | `/api/v1/hosts/{id}/tags/{key}` | Remove a single tag |
| `GET` | `/api/v1/queries` | List recent queries |
| `POST` | `/api/v1/tokens` | Create an API token |
| `GET` | `/api/v1/tokens` | List tokens |
| `DELETE` | `/api/v1/tokens/{name}` | Delete a token |
| `GET` | `/api/v1/inventory` | Ansible dynamic inventory (JSON) |
| `POST` | `/api/v1/exec` | Execute a command on an agent |
| `POST` | `/api/v1/put_file` | Write a file to an agent |
| `POST` | `/api/v1/fetch_file` | Read a file from an agent |
| `GET` | `/api/v1/exec_log` | Query execution audit log |
| `GET` | `/healthz` | Health check |

## Ansible Integration

### Inventory Groups

The DirQ inventory plugin automatically creates a nested group hierarchy from
agent metadata and tags. Set tags on agents with `DIRQ_TAGS=env=prod,role=webserver,dc=us-east`.

```
@all
├── @os_linux
├── @os_windows
├── @arch_amd64
├── @arch_arm64
├── @exec_enabled           (hosts accepting remote execution)
├── @tag_env                (parent group for all env=* tags)
│   ├── @tag_env_prod
│   ├── @tag_env_staging
│   └── @tag_env_dev
├── @tag_role
│   ├── @tag_role_webserver
│   └── @tag_role_database
└── @tag_dc
    ├── @tag_dc_us_east
    └── @tag_dc_eu_west
```

Target hosts in playbooks using standard Ansible patterns:

```yaml
# All online hosts
hosts: all

# By operating system
hosts: os_linux
hosts: os_windows

# By architecture
hosts: arch_amd64

# By tag value
hosts: tag_env_prod
hosts: tag_role_webserver
hosts: tag_dc_us_east

# By tag parent (all values for that key)
hosts: tag_env

# Only exec-capable hosts
hosts: exec_enabled

# Intersections
hosts: tag_role_webserver:&os_linux       # linux webservers only
hosts: tag_env_prod:&exec_enabled         # prod hosts that accept exec
hosts: tag_dc_us_east:&tag_role_database  # databases in us-east

# Specific host by name
hosts: fedora
```

### Host Variables (Facts)

DirQ exposes all collected data as Ansible facts under the `dirq_*` namespace:

```yaml
# Available in playbooks as hostvars:
dirq_agent_id: "abc-123"
dirq_os: "linux"
dirq_os_version: "RHEL 9.2"
dirq_arch: "amd64"
dirq_exec_enabled: true
dirq_role: "zone_leader"
dirq_online: true
dirq_last_seen: "2026-05-12T20:15:50Z"
dirq_cpu:
  physical_cores: 8
  logical_cores: 16
  model_name: "Intel Xeon..."
  vendor: "GenuineIntel"
dirq_memory:
  total_bytes: 34359738368
  available_bytes: 21710409728
  used_bytes: 11384057856
  pct_used: 34.4
  swap_total_bytes: 8589930496
  swap_used_bytes: 0
dirq_disk:
  partitions:
    - device: "/dev/sda1"
      mount_point: "/"
      fs_type: "ext4"
      total_bytes: 107374182400
      used_bytes: 72132345856
      free_bytes: 35241836544
      pct_used: 67.3
dirq_tag_env: "prod"
dirq_tag_dc: "us-east"
dirq_tag_role: "webserver"
```

Use in playbook conditionals:

```yaml
- name: Alert on high disk usage
  hosts: all
  tasks:
    - debug:
        msg: "Disk alert on {{ inventory_hostname }}"
      when: dirq_disk.partitions | selectattr('pct_used', '>', 80) | list | length > 0

- name: Warn about low memory
  hosts: os_linux
  tasks:
    - debug:
        msg: "{{ inventory_hostname }} has {{ dirq_memory.available_bytes | human_readable }} free"
      when: dirq_memory.pct_used > 90

- name: Only run on exec-capable hosts
  hosts: tag_env_prod:&exec_enabled
  connection: dirq
  tasks:
    - command: systemctl status myapp
```

### Query-Based Inventories

The inventory plugin accepts an optional `query` parameter that filters which
hosts appear in the inventory. Only hosts matching the DirQ query are included.
The query runs in real time against the agent mesh — agents filter locally and
only matches respond.

**Inventory source files:**

```yaml
# inventories/all-hosts.yml — all online hosts
plugin: atgreen.dirq.dirq
server_url: http://dirq-server:8080

# inventories/vulnerable-openssl.yml — only hosts with OpenSSL 1.x
plugin: atgreen.dirq.dirq
server_url: http://dirq-server:8080
query: "SELECT os_info.hostname FROM * WHERE packages.name = 'openssl' AND packages.version LIKE '1.%'"

# inventories/disks-full.yml — only hosts with disks over 90%
plugin: atgreen.dirq.dirq
server_url: http://dirq-server:8080
query: "SELECT os_info.hostname FROM * WHERE disk.pct_used > 90"

# inventories/sshd-stopped.yml — only hosts where sshd is down
plugin: atgreen.dirq.dirq
server_url: http://dirq-server:8080
query: "SELECT os_info.hostname FROM * WHERE services.name = 'sshd' AND services.state = 'stopped'"
```

**Using with AAP:**

Each YAML file becomes an Inventory Source in AAP (type: "Sourced from a Project").
Create job templates that pair each inventory with a remediation playbook:

| Job Template | Inventory Source | Playbook | Targets |
|---|---|---|---|
| Patch OpenSSL | vulnerable-openssl.yml | update-openssl.yml | Hosts with OpenSSL 1.x |
| Fix Full Disks | disks-full.yml | cleanup-disks.yml | Hosts over 90% disk |
| Restart SSHD | sshd-stopped.yml | restart-sshd.yml | Hosts where sshd is down |

When AAP syncs the inventory, the DirQ query runs against the mesh in real time.
The resulting host list is always current — after patching OpenSSL, the next sync
returns fewer hosts because the query no longer matches the patched ones.

**The flow:**

```
AAP Inventory Sync
  │
  ▼
Inventory plugin runs DirQ query through the mesh
  → Agents check locally (packages, disk, services)
  → Only matching agents respond
  │
  ▼
AAP inventory contains exactly the matching hosts
  (e.g. 3 hosts out of 10,000 with vulnerable OpenSSL)
  │
  ▼
Job template runs playbook against only those 3 hosts
  → connection: atgreen.dirq.dirq (through the mesh, no SSH)
```

**Why this is better than `hosts: all` with `when:` conditionals:**

- Traditional approach: AAP connects to all 10,000 hosts via SSH just to check
  a condition, then skips 9,997 of them. Slow, wasteful, requires SSH access to
  every host.
- Query-based inventory: The DirQ query runs in seconds across the mesh. Agents
  filter locally. AAP only connects to the 3 matching hosts. No SSH needed for
  the inventory sync — it's an HTTP call to the DirQ server.

**Standalone usage (without AAP):**

```bash
# Only target hosts with full disks
DIRQ_QUERY="SELECT os_info.hostname FROM * WHERE disk.pct_used > 90" \
  ansible-playbook -i ansible/dirq_inventory.py cleanup-disks.yml

# Only target hosts with a specific package
DIRQ_QUERY="SELECT os_info.hostname FROM * WHERE packages.name = 'nginx'" \
  ansible-playbook -i ansible/dirq_inventory.py update-nginx.yml
```

## Execution Transport

The DirQ relay mesh doubles as an **Ansible connection transport**. AAP runs playbooks against managed hosts through the mesh — no SSH, no WinRM, no inbound firewall rules.

### How It Works

```
AAP Job Template (connection: dirq)
  │
  ▼
Execution Environment (with DirQ connection plugin)
  │
  ▼  REST API
DirQ Server
  │
  ▼  gRPC relay mesh
Zone Leader → Relay Peer → Target Agent
                            │
                            ▼
                    Executes command locally
                    Returns stdout/stderr/rc
```

1. Admin launches a job template in AAP with `connection: dirq`.
2. AAP spins up an EE container with the DirQ connection plugin.
3. For each task, Ansible calls `exec_command()`, `put_file()`, or `fetch_file()`.
4. The plugin routes each request to the DirQ server REST API.
5. The server pushes it through the relay mesh to the target agent.
6. The agent executes locally and returns results back through the mesh.
7. AAP records the job result normally — full audit trail preserved.

### Using `connection: dirq` in Playbooks

```yaml
- name: Manage hosts via DirQ mesh
  hosts: all
  connection: dirq
  gather_facts: false
  vars:
    dirq_server_url: http://dirq-server:8080
    dirq_token: "{{ lookup('env', 'DIRQ_TOKEN') }}"
  tasks:
    - name: Check uptime
      command: uptime

    - name: Copy config file
      copy:
        src: app.conf
        dest: /etc/myapp/app.conf
        mode: '0644'

    - name: Read remote file
      fetch:
        src: /var/log/myapp/status.log
        dest: /tmp/status.log
        flat: yes
```

Playbooks work identically to SSH-based execution. The connection plugin transparently routes everything through the DirQ mesh.

### Enabling Exec on Agents

Exec is **disabled by default** — agents must opt in:

```bash
# Enable via environment variable
DIRQ_EXEC_ENABLED=true ./bin/dirq-agent

# Or via the server API (admin token required)
curl -X PATCH -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/api/v1/hosts/$AGENT_ID \
  -d '{"exec_enabled": true}'
```

Agents with `exec_enabled: false` reject all exec/file requests. The inventory plugin exposes this as `dirq_exec_enabled` so playbooks can check it.

### Security Model

- **Exec requests are server-originated only.** The DirQ server's mTLS identity is required. Peer agents cannot send exec requests to each other — no lateral movement through the mesh.
- **Opt-in per agent.** `exec_enabled` defaults to `false`. Admins explicitly enable it on hosts that should accept remote commands.
- **Windows agents run as SYSTEM.** Installed as a Windows Service, the agent has full privileges. Become as a different user uses a PowerShell scheduled-task mechanism — no stored passwords needed.
- **Linux agents use passwordless sudo.** The `sudo -n` (non-interactive) flag is used. The agent's service account must have NOPASSWD sudo configured for the required commands.
- **Full audit trail.** Every exec operation is logged in PostgreSQL with: timestamp, AAP job ID, job template, target host, requesting user, command/path, and exit status. Agents also log locally to syslog / Windows Event Log.
- **AAP retains orchestration authority.** DirQ does not decide what to execute. AAP's RBAC, credential vault, approval workflows, and job scheduling remain in control. DirQ is the data plane only.
- **File transfer limits.** Put/fetch operations are capped at 100 MB by default.

### Exec Audit Log

```bash
# View recent exec operations
curl http://localhost:8080/api/v1/exec_log

# Filter by AAP job
curl "http://localhost:8080/api/v1/exec_log?aap_job_id=42"

# Filter by agent
curl "http://localhost:8080/api/v1/exec_log?agent_id=abc-123"
```

Each entry includes: operation type, command/path, become user, rc, success/error, AAP job attribution, and timestamps.

### What This Replaces vs. What It Doesn't

**Replaces:**
- SSH/WinRM as the connection transport
- Inbound firewall rules for Ansible access
- SSH key distribution and rotation
- WinRM HTTPS certificate management
- Bastion hosts / jump boxes for reaching isolated hosts

**Does not replace:**
- AAP's orchestration, RBAC, credential vault, job scheduling
- AAP's approval workflows and audit logging
- Ansible's module system, playbook language, or roles

## Multi-Datacenter Deployment

DirQ supports multi-DC deployments where each datacenter has its own isolated
mesh. Agent meshes never span DC boundaries — all P2P relay traffic stays local.

### Architecture

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
  │                                                          │
  │  Inventory Source 1 ──► dirq-us-east:8080                │
  │  Inventory Source 2 ──► dirq-eu-west:8080                │
  │  Inventory Source 3 ──► dirq-ap-south:8080               │
  │  Inventory Source 4 ──► dirq-us-west:8080                │
  │                                                          │
  │  All sources merge into one inventory.                   │
  │  Each host carries dirq_server_url pointing              │
  │  to its DC's server.                                     │
  └──────────────────────────────────────────────────────────┘
```

### Setup

**1. Deploy one DirQ server per datacenter:**

Each DC gets its own DirQ server and PostgreSQL instance. Agents in that DC
connect to the local server. The meshes are completely independent.

```
DC us-east:  dirq-us-east.internal:8080   (server)  :50051 (gRPC)
DC eu-west:  dirq-eu-west.internal:8080   (server)  :50051 (gRPC)
DC ap-south: dirq-ap-south.internal:8080  (server)  :50051 (gRPC)
DC us-west:  dirq-us-west.internal:8080   (server)  :50051 (gRPC)
```

Tag agents with their DC:

```bash
# On agents in us-east:
DIRQ_SERVER=dirq-us-east.internal:50051 DIRQ_TAGS=dc=us-east,env=prod

# On agents in eu-west:
DIRQ_SERVER=dirq-eu-west.internal:50051 DIRQ_TAGS=dc=eu-west,env=prod
```

**2. Create one AAP inventory source per DC:**

```yaml
# dirq-us-east.yml
plugin: atgreen.dirq.dirq
server_url: http://dirq-us-east.internal:8080
token: us-east-token

# dirq-eu-west.yml
plugin: atgreen.dirq.dirq
server_url: http://dirq-eu-west.internal:8080
token: eu-west-token
```

Add each as an Inventory Source in AAP. All sources merge into one inventory.

**3. Routing happens automatically:**

The inventory plugin sets `dirq_server_url` as a per-host variable on every host
it discovers. When the connection plugin runs a task against a host, it reads that
host's `dirq_server_url` and routes through the correct DC's server and mesh.

```yaml
# This playbook runs against hosts in all DCs.
# The connection plugin routes each host through its own DC's mesh.
- hosts: tag_env_prod
  connection: atgreen.dirq.dirq
  tasks:
    - command: uptime

# Target a specific DC
- hosts: tag_dc_us_east:&tag_role_webserver
  connection: atgreen.dirq.dirq
  tasks:
    - command: systemctl status nginx
```

No per-host connection configuration needed. A host from `us-east` routes through
`dirq-us-east`, a host from `eu-west` routes through `dirq-eu-west` — even when
they appear in the same play.

**4. Queries are per-DC:**

DirQ queries run against a single server (one mesh). Use the CLI `--server` flag
or `DIRQ_SERVER_URL` to target a specific DC:

```bash
# Query us-east fleet
dirq --server http://dirq-us-east:8080 query "SELECT os_info.hostname, cpu.logical_cores FROM *"

# Query eu-west fleet
dirq --server http://dirq-eu-west:8080 query "SELECT os_info.hostname, packages.name, packages.version FROM * WHERE packages.name = 'openssl'"
```

## AAP Integration

### Ansible Collection

DirQ ships as an Ansible collection (`atgreen.dirq`) for AAP compatibility:

```bash
# Build the collection
cd collection/atgreen/dirq
ansible-galaxy collection build

# Install locally
ansible-galaxy collection install atgreen-dirq-1.0.0.tar.gz
```

The collection includes:
- `atgreen.dirq.dirq` inventory plugin (class-based, AAP-compatible)
- `atgreen.dirq.dirq` connection plugin
- Shared `module_utils` HTTP client

### Custom Execution Environment

Build an EE that includes the DirQ collection:

```yaml
# execution-environment.yml
version: 3
dependencies:
  galaxy:
    collections:
      - name: atgreen.dirq
        version: ">=1.0.0"
```

```bash
ansible-builder build -t dirq-ee:latest -f execution-environment.yml
```

Push to your container registry and add as an EE in AAP.

### Custom Credential Type

Import the credential type from `collection/atgreen/dirq/docs/aap-credential-type.yml`,
or create it manually in AAP under Administration > Credential Types:

**Input Configuration:**
```yaml
fields:
  - id: dirq_server_url
    type: string
    label: DirQ Server URL
  - id: dirq_token
    type: string
    label: DirQ API Token
    secret: true
required:
  - dirq_server_url
  - dirq_token
```

**Injector Configuration:**
```yaml
env:
  DIRQ_SERVER_URL: "{{ dirq_server_url }}"
  DIRQ_TOKEN: "{{ dirq_token }}"
```

### AAP Setup Checklist

1. Build and publish the `atgreen.dirq` collection to Automation Hub or Galaxy
2. Build a custom EE with the collection and push to your registry
3. Import the DirQ credential type in AAP
4. Create DirQ credentials (one per DC server if multi-DC)
5. Add inventory sources using `atgreen.dirq.dirq` plugin (one per DC)
6. Create job templates with `connection: atgreen.dirq.dirq`
7. Attach the appropriate DirQ credential to each job template

## Building

```bash
# Build all binaries
go build -o bin/dirq-server ./cmd/dirq-server
go build -o bin/dirq-agent  ./cmd/dirq-agent
go build -o bin/dirq         ./cmd/dirq

# Cross-compile agent for Windows
GOOS=windows GOARCH=amd64 go build -o bin/dirq-agent.exe ./cmd/dirq-agent

# Cross-compile agent for Windows ARM64
GOOS=windows GOARCH=arm64 go build -o bin/dirq-agent-arm64.exe ./cmd/dirq-agent

# Run tests
go test ./...

# Build container images
podman build --target server -t dirq-server .
podman build --target agent  -t dirq-agent .
```

## Project Structure

```
cmd/
  dirq-server/            Server entrypoint
  dirq-agent/             Agent entrypoint
  dirq/                   CLI entrypoint
proto/dirq/v1/            Protobuf definitions (gRPC services + messages)
internal/
  server/                 gRPC service, REST API, query dispatch, exec routing
  agent/                  Registration, relay mesh, query execution, remote exec
  query/                  DirQ DSL parser (participle) and evaluator
  modules/                System data collectors (7 modules)
  db/                     PostgreSQL schema and data access layer
collection/atgreen/dirq/  Ansible collection (atgreen.dirq) for AAP
  plugins/connection/     connection: atgreen.dirq.dirq
  plugins/inventory/      inventory: atgreen.dirq.dirq
  docs/                   AAP credential type definition
ansible/                  Standalone plugins for CLI Ansible (non-AAP)
execution-environment.yml EE definition for ansible-builder
Containerfile             Multi-stage container build
podman-compose.yml        Dev environment (server + PostgreSQL)
PRD.md                    Product requirements document
```

## License

MIT License. Copyright (c) 2026 Anthony Green. See [LICENSE](LICENSE) for details.
