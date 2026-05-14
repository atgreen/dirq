# Changelog

All notable changes to DirQ will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [0.4.0] - 2026-05-14

### Added

- **`dirq cve`** — scan RHEL systems for CVE vulnerabilities using the Red Hat Security Data API; compares installed package versions against fixed versions with RPM version comparison

## [0.3.0] - 2026-05-14

### Added

- **`dirq select`** — query the fleet with natural syntax (`dirq select hostname WHERE tag.env = 'prod'`), replacing `dirq query`
- **`dirq deploy`** — deploy RPM, DEB, or MSI packages through the relay mesh with depth-first rolling waves by default
- **`dirq doctor`** — validate deployment health: connectivity, auth, database, fleet status, agent version skew, tree topology, local tooling
- **`dirq run` with WHERE syntax** — playbook as first arg with optional WHERE clause (`dirq run deploy.yml WHERE tag.env = 'prod'`), replacing `--query` flag
- **Arg flattening** — quoted multi-word args are split by whitespace, so `dirq "select hostname where tag.env = 'prod'"` works naturally
- **Server status endpoint** — `GET /api/v1/status` returns database health, agent counts, version distribution, and topology stats
- **Makefile** — `make build`, `make test`, `make install`, `make cross`, `make proto`, `make collection`
- **DEB packaging** — Debian packages for dirq, dirq-server, and dirq-agent alongside existing RPMs

### Changed

- **`dirq run`** now takes playbook as first positional arg with optional WHERE clause instead of `--query` flag
- **CLI examples** recommend quoting queries to avoid shell interpretation of `>`, `<`, `*`, `(`, `)`
- **`dirq skill`** output now documents all CLI commands

### Removed

- **`dirq query`** — replaced by `dirq select`

### Fixed

- **GROUP BY on nested module fields returned null** — server-side aggregation now flattens nested module data into dotted keys before GROUP BY

## [0.2.1] - 2026-05-13

### Fixed

- **Exec audit log updates never matched** — `UpdateExecLog` queried by auto-generated ID instead of request ID, leaving audit rows permanently incomplete
- **Token scopes not enforced** — readonly tokens could access exec, tag mutation, and token management endpoints; scopes are now checked per-route
- **GROUP BY aggregation errors silently discarded** — partial failures now return HTTP 500 instead of misleading results
- **Token validation scaled poorly** — every API request scanned all tokens with bcrypt; now uses an indexed prefix for O(1) lookup

## [0.2.0] - 2026-05-13

### Added

- **`dirq ask` command** — natural language queries via LLM, translating plain English to DirQ DSL
- **`dirq skill` command** — run reusable query-and-act recipes
- **`dirq run` command** — query the fleet and run Ansible playbooks in one step
- **Config file support** — YAML configuration for server, agent, and CLI
- **`--tls-insecure` flag** — skip TLS verification for dev/test environments
- **Interactive demo suite** — 20-agent fleet with varied personalities for testing
- **Tree rebalancing** — detect imbalanced relay trees and redistribute subtrees
- **Redundant parent fallback** — agents reconnect through alternate parents for mesh resilience
- **In-mesh result aggregation** — snowball aggregation through the relay tree
- **Windows and Linux packaging** — RPM builds and Windows installer
- **GitHub Actions release pipeline** — CI, cross-platform binary builds, and RPM yum repo

### Changed

- **Query DSL rewrite** — hand-rolled parser replaces previous implementation; `FROM` clause removed (breaking change)
- **Stream-based liveness detection** replaces heartbeat polling for connection health
- **Container base images** switched from Alpine to UBI9

### Fixed

- Relay agent heartbeats not reaching the server
- `ENHANCE_YOUR_CALM` keepalive disconnects
- Topology assignment race condition (now uses PostgreSQL advisory lock)
- Rebalancer feedback loop — limited to one action per cycle
- Agent IP resolution and signed message forwarding in mesh relay

## [0.1.0] - 2026-05-13

### Added

- **Query engine** with SQL-like DSL: SELECT, FROM, WHERE, GROUP BY, ORDER BY
- **7 query modules:** cpu, memory, disk, os_info, packages, network, services
- **IN operator** and array-aware filtering (packages, services, disk, network)
- **P2P relay mesh** with automatic topology management (zone leaders, BFS fill)
- **Ansible inventory plugin** (`atgreen.dirq.dirq`) with nested group hierarchy
- **Query-based inventory filtering** — build Ansible inventories from live DirQ queries
- **Ansible connection plugin** (`connection: atgreen.dirq.dirq`) — run playbooks through the mesh
- **Ansible collection** (`atgreen.dirq`) for AAP with inventory + connection plugins
- **Remote execution** (exec_command, put_file, fetch_file) through the relay mesh
- **Exec audit logging** with AAP job attribution (job ID, template, user)
- **TLS by default** — auto-generated self-signed certs, user-supplied certs, mTLS support
- **API authentication** — required by default, bootstrap token on first startup
- **Message signing** (Ed25519) for server-originated control messages
- **Agent reconnection** with exponential backoff on connection loss
- **gRPC keepalive** and server-side reaper for dead connection detection
- **Host tag management** — REST API and CLI for add/remove/merge tags
- **Multi-datacenter support** — per-DC servers with automatic per-host routing
- **Windows agent** — Windows Service support, PowerShell privilege escalation
- **CLI tool** (`dirq`) — query, hosts, tokens, tags, TLS cert generation
- **Containerfile** — multi-stage build for server and agent images
- **podman-compose** — dev environment with server + PostgreSQL
- **Execution Environment definition** for ansible-builder
- **AAP credential type definition**

### Components

- `dirq-server` — Go, gRPC + REST API, PostgreSQL-backed
- `dirq-agent` — Go, single static binary, Linux + Windows
- `dirq` — Go, CLI tool
- `atgreen.dirq` — Python, Ansible collection

[0.4.0]: https://github.com/atgreen/dirq/releases/tag/v0.4.0
[0.3.0]: https://github.com/atgreen/dirq/releases/tag/v0.3.0
[0.2.1]: https://github.com/atgreen/dirq/releases/tag/v0.2.1
[0.2.0]: https://github.com/atgreen/dirq/releases/tag/v0.2.0
[0.1.0]: https://github.com/atgreen/dirq/releases/tag/v0.1.0
