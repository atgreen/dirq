# DirQ — Real-Time Endpoint Query & Ansible Inventory Platform

DirQ is an agent-based platform for querying live system state across large Windows/Linux fleets. Agents form a peer-to-peer relay mesh and report data back to a central server. The server acts as an Ansible Automation Platform (AAP) inventory source, exposing collected data as Ansible facts.

Real-time fleet-wide endpoint querying, integrated with the Ansible ecosystem.

## Architecture

```
  ┌─────────┐   ┌─────────┐   ┌─────────┐   ┌─────────┐
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
| `dirq-agent` | Go | Lightweight agent. Runs on managed Linux/Windows servers. Collects system data, relays queries through the P2P mesh. Single static binary, zero dependencies. |
| `dirq` | Go | CLI tool. Submit queries, list hosts, manage API tokens. |
| `dirq_inventory.py` | Python | Ansible dynamic inventory plugin. Connects to the DirQ REST API, exposes hosts and facts to AAP/awx. |

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

## Building

```bash
# Build all binaries
go build -o bin/dirq-server ./cmd/dirq-server
go build -o bin/dirq-agent  ./cmd/dirq-agent
go build -o bin/dirq         ./cmd/dirq

# Cross-compile agent for Windows
GOOS=windows GOARCH=amd64 go build -o bin/dirq-agent.exe ./cmd/dirq-agent

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
  server/               gRPC service, REST API, query dispatch
  agent/                Registration, relay mesh, query execution
  query/                DirQ DSL parser (participle) and evaluator
  modules/              System data collectors (disk, cpu, memory, os_info)
  db/                   PostgreSQL schema and data access layer
ansible/                Ansible dynamic inventory plugin
Containerfile           Multi-stage container build
podman-compose.yml      Dev environment (server + PostgreSQL)
PRD.md                  Product requirements document
```

## License

TBD
