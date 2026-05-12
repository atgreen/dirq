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

## Query DSL

DirQ has a SQL-like query language for ad-hoc fleet queries:

```sql
-- Find hosts with disks over 80% full
SELECT hostname, disk.mount, disk.pct_used
FROM tag:prod
WHERE disk.pct_used > 80
ORDER BY disk.pct_used DESC

-- Count hosts by OS
SELECT os, COUNT(hostname), AVG(memory.total_bytes)
FROM *
GROUP BY os

-- List CPU info for a group
SELECT hostname, cpu.cores, cpu.model_name
FROM group:webservers
WHERE cpu.cores >= 8
```

Queries are parsed on the server, pushed through the relay mesh to agents, filtered agent-side, and aggregated server-side. Results stream back in real time.

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

### Agent (environment variables)

| Variable | Default | Description |
|----------|---------|-------------|
| `DIRQ_SERVER` | `localhost:50051` | DirQ server gRPC address |
| `DIRQ_LISTEN` | `:50052` | Address to listen on for downstream peers |
| `DIRQ_TAGS` | | Comma-separated tags: `env=prod,dc=us-east` |
| `DIRQ_EXEC_ENABLED` | `false` | Enable remote command execution |

### CLI (environment variables and flags)

| Variable / Flag | Default | Description |
|----------------|---------|-------------|
| `DIRQ_SERVER_URL` / `--server` | `http://localhost:8080` | DirQ server REST URL |
| `DIRQ_TOKEN` / `--token` | | API token |
| `--json` | `false` | Output raw JSON |

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

DirQ exposes all collected data as Ansible facts under the `dirq_*` namespace:

```yaml
# Available in playbooks as hostvars:
dirq_agent_id: "abc-123"
dirq_os: "linux"
dirq_os_version: "RHEL 9.2"
dirq_arch: "amd64"
dirq_cpu:
  physical_cores: 8
  logical_cores: 16
  model_name: "Intel Xeon..."
dirq_memory:
  total_bytes: 34359738368
  pct_used: 42.5
dirq_disk:
  partitions:
    - mount_point: "/"
      pct_used: 67.3
dirq_tag_env: "prod"
dirq_tag_dc: "us-east"
```

Use in playbooks:

```yaml
- name: Alert on high disk usage
  hosts: all
  tasks:
    - debug:
        msg: "Disk alert on {{ inventory_hostname }}"
      when: dirq_disk.partitions | selectattr('pct_used', '>', 80) | list | length > 0
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
  dirq-server/          Server entrypoint
  dirq-agent/           Agent entrypoint
  dirq/                 CLI entrypoint
proto/dirq/v1/          Protobuf definitions (gRPC services + messages)
internal/
  server/               gRPC service, REST API, query dispatch, exec routing
  agent/                Registration, relay mesh, query execution, remote exec
  query/                DirQ DSL parser (participle) and evaluator
  modules/              System data collectors (disk, cpu, memory, os_info)
  db/                   PostgreSQL schema and data access layer (incl. exec audit)
ansible/
  dirq_inventory.py     Dynamic inventory plugin for AAP/awx
  connection_plugins/
    dirq.py             Ansible connection plugin (connection: dirq)
Containerfile           Multi-stage container build
podman-compose.yml      Dev environment (server + PostgreSQL)
PRD.md                  Product requirements document
```

## License

MIT License. Copyright (c) 2026 Anthony Green. See [LICENSE](LICENSE) for details.
