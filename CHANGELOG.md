# Changelog

All notable changes to DirQ will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [0.14.1] - 2026-05-18

### Fixed

- **Server panic when agent registers without client cert** — the mTLS interceptor accessed an empty `VerifiedChains` slice when an agent connected without a client certificate (e.g., during initial registration), crashing the server

## [0.14.0] - 2026-05-17

### Added

- **mTLS for gRPC** — server issues a unique TLS client certificate per agent during registration (CN = agent ID); all subsequent gRPC connections require a valid client cert signed by the server's CA; the registration secret becomes a one-time bootstrap token rather than a persistent credential; activates automatically when the CA key is available (auto-generated or via `DIRQ_TLS_CA_KEY`)
- **Agent identity binding** — server, zone leaders, and relay agents verify that the TLS certificate CN matches the claimed agent ID, preventing agent impersonation even with a stolen session token
- **`dirq hosts list` output capped at 10** — large fleets now show the first 10 hosts with a count summary instead of flooding the terminal; use `--all` to see every host

### Changed

- **Generated `agent.conf` no longer includes shared agent cert/key** — each agent receives its own mTLS cert during registration; only the CA cert is embedded for server verification

## [0.13.0] - 2026-05-17

### Added

- **LLM-powered change review** — `dirq exec`, `dirq run`, and `dirq deploy` can now send proposed actions to an LLM for risk analysis before execution; identifies destructive operations, typos, privilege misuse, and scope concerns; supports Anthropic's native API and any OpenAI-compatible endpoint; configure with `DIRQ_LLM_URL`, `DIRQ_LLM_API_KEY`, and `DIRQ_LLM_MODEL`; disabled by default
- **Playbook content gathering** — for `dirq run`, recursively resolves all referenced task files, roles, and handlers and includes their contents in the review prompt

## [0.12.2] - 2026-05-17

### Added

- **Server signing key pinned in agent.conf** — the server now writes its Ed25519 signing public key into the generated `agent.conf`; agents validate the server's key during registration, preventing MITM attacks at enrollment time

### Fixed

- **Non-PowerShell Ansible commands failed on Windows** — the `/bin/sh -c` wrapper stripping worked for PowerShell commands but the stripped result was not used for the `cmd /c` fallback path, causing `type`, `mkdir`, and other commands to fail

## [0.12.1] - 2026-05-17

### Added

- **Auto-detect Python interpreter** — `dirq run` probes Linux targets for a working Python 3.7+ before invoking Ansible; errors clearly if no compatible Python is found
- **Auto-configure Windows shell type** — `dirq run` automatically sets `ansible_shell_type=powershell` for Windows hosts in the generated inventory, no manual tagging required

### Fixed

- **Ansible commands failed on Windows agents** — the agent now strips the `/bin/sh -c '...'` wrapper that Ansible adds to all commands, which has no meaning on Windows
- **Python 3.6 (platform-python) caused Ansible failures** — the interpreter probe now validates Python >= 3.7 and skips `/usr/libexec/platform-python` which is too old for modern Ansible

## [0.12.0] - 2026-05-16

### Added

- **`dirq mcp`** — built-in [Model Context Protocol](https://modelcontextprotocol.io/) stdio server, allowing LLMs like Claude to manage the fleet directly as a tool; exposes 10 tools: host inventory, system facts, tagging, fleet queries, remote execution, CVE scanning, errata checks, KB verification, and topology graph
- **Auto-detect Python interpreter** — `dirq run` now probes Linux targets for a working Python before invoking Ansible, checking `/usr/bin/python3`, `/usr/libexec/platform-python`, and versioned `python3.x` paths; errors clearly if no Python is found

## [0.11.4] - 2026-05-16

### Fixed

- **Windows MSI installed to wrong directory** — MSI was built without `-arch x64`, causing the agent to install to `Program Files (x86)` instead of `Program Files`; the AWS provisioning script then failed to find the executable and exited before writing the server-generated config
- **AWS fleet agents couldn't reach server** — the generated `agent.conf` used the server's internal hostname which Windows instances couldn't resolve; the provisioning script now rewrites it to the server's private IP
- **Ansible PowerShell modules failed on Windows agents** — the agent double-wrapped PowerShell commands in another `powershell -Command` layer, breaking Ansible's `-EncodedCommand` execution

## [0.11.3] - 2026-05-16

### Fixed

- **Windows MSI install failed with error 1603** — the WiX `util:ServiceConfig` custom action tried to set service recovery options before the service was created; removed from MSI, recovery is now configured post-install

## [0.11.2] - 2026-05-16

### Added

- **SPDX SBOM** — release workflow now generates an SPDX JSON software bill of materials and attaches it to each GitHub release

### Fixed

- **Windows MSI install failed with error 1603** — the MSI tried to start the agent service during install, which failed if no config file existed yet; service is now registered but not started, letting the config be written first

## [0.11.1] - 2026-05-16

### Fixed

- **Generated agent.conf had garbled server address** — when `grpc_addr` was `0.0.0.0:50051`, the generated config got `hostname0.0.0.0:50051` instead of `hostname:50051`; same for `client.conf` with `http_addr`

## [0.11.0] - 2026-05-16

### Added

- **`dirq errata`** — check the fleet against Red Hat advisories (RHSA/RHBA/RHEA); fetches advisory data, extracts all CVEs and fixed packages, and reports which RHEL hosts are patched or vulnerable
- **`dirq kb`** — check Windows hosts for installed hotfixes; reports which hosts have or are missing specific KBs
- **`hotfixes` module** — collects installed Windows hotfixes via `Get-HotFix` (kb_id, description, installed_on); supports filtered collection for targeted KB queries

### Fixed

- **API token shown in CLI help** — the `--token` flag displayed the token from `client.conf` as its default value; now hidden from help output
- **CVE not-assessed count was always zero** — the count compared against query results (which only included RHEL hosts) instead of the total online fleet

## [0.10.0] - 2026-05-16

### Added

- **`os_info.distro`, `distro_version`, `distro_family`** — new fields from `/etc/os-release` (e.g., `distro=rhel`, `distro_family=rhel`, `distro_version=8.10`); enables clean filtering by distribution
- **Filtered package collection** — when a WHERE clause specifies package names (e.g., `packages.name = 'kernel'`), agents run `rpm -q kernel` instead of `rpm -qa`, dramatically reducing collection time and mesh traffic
- **No-match responses** — agents that don't match a WHERE clause now send a lightweight "no match" response instead of staying silent; the server counts completions and finishes as soon as all targets have answered, eliminating idle timeout waits
- **`dirq cve --verbose`** — timestamped step-by-step output showing CVE fetch time, query string, and fleet query duration for diagnosing slow scans
- **CVE scan summary includes not-assessed count** — shows how many hosts were skipped (non-RHEL)

### Fixed

- **CVE scanner scanned non-RHEL hosts** — Fedora, Ubuntu, and Windows hosts were compared against RHEL fix versions, producing nonsensical results; now filters to `distro_family = 'rhel'` only
- **CVE scanner compared wrong RHEL versions** — RHEL 8 hosts were compared against RHEL 10 fixes; now matches fix versions to the host's specific RHEL major version
- **CVE scanner reported every installed kernel** — hosts with multiple kernels installed showed one line per kernel; now compares only the running kernel (`os_info.kernel_version`) and shows one line per host
- **CVE scanner included kpatch as a fix** — kpatch-patch is a live-patching workaround, not the actual fix; now filtered out entirely

## [0.9.2] - 2026-05-16

### Added

- **Double quotes and unquoted strings in query DSL** — `WHERE hostname = "fedora"` and `WHERE hostname = fedora` now work alongside the existing single-quote syntax; the shell often strips single quotes, so this avoids a common frustration
- **`ansible_*` tags passed through as Ansible host vars** — tags like `ansible_python_interpreter=/usr/bin/python3.12` set via `dirq hosts tag` are now used in the generated inventory, overriding defaults

### Fixed

- **`dirq run --module` treated WHERE as a playbook** — `dirq run --module ping WHERE ...` tried to run `WHERE` as a playbook file; now correctly treats all positional args as the WHERE clause when `--module` or `--command` is set
- **`dirq run` didn't forward server URL and token to Ansible** — when configured via `client.conf` instead of env vars, the Ansible subprocess didn't receive `DIRQ_SERVER_URL` or `DIRQ_TOKEN`

## [0.9.1] - 2026-05-16

### Fixed

- **Auto-generated server cert only had localhost SANs** — agents connecting by the server's real IP (e.g., 192.168.1.10) got TLS verification failures; cert now includes all non-loopback interface IPs and the hostname

## [0.9.0] - 2026-05-15

### Added

- **Server-generated agent config** — server writes `/var/lib/dirq/agent.conf` on startup with server address, registration secret, and base64-encoded TLS certs inline; copy one file to onboard an agent
- **Server-generated client config** — server writes `/var/lib/dirq/client.conf` with server URL and bootstrap token; copy to `~/.config/dirq/client.conf` or `/etc/dirq/client.conf` for zero-config CLI
- **CLI config file support** — `dirq` reads `server_url`, `token`, and `tls_insecure` from `~/.config/dirq/client.conf` (user-local, checked first) or `/etc/dirq/client.conf`; on Windows: `%APPDATA%\dirq\` then `C:\ProgramData\dirq\`
- **Inline TLS certs in config** — `tls_ca_data`, `tls_cert_data`, `tls_key_data` keys accept base64-encoded PEM; agent materializes them to disk on startup

### Fixed

- **`exec` ignored field-based WHERE conditions** — `dirq exec "ls" WHERE os_info.os = 'linux'` sent to all agents; now runs a query first to resolve matching agents before dispatching
- **Arg flattener broke exec commands with dashes** — `dirq exec "ls -l"` was split into `ls` and `-l` (cobra flag error); flattener now only splits args starting with `SELECT`

## [0.8.0] - 2026-05-15

### Added

- **Rate limiting on query and exec endpoints** — per-token token-bucket limiter (10 req/s, burst 20) prevents a single client from flooding the fleet with broadcast queries
- **Real-time exec progress** — `dirq exec` now shows "X/Y hosts responded..." while waiting for results; server emits NDJSON progress heartbeats every 5 seconds during streaming exec

### Fixed

- **Rebalancer DB-before-send in promotions** — `promoteOneRelay` now updates the DB only after successfully delivering the PeerUpdate message, matching the pattern already used for demotions and redistributions
- **Registration defaults to zone_leader on failure** — topology assignment errors now reject the registration instead of silently creating excess zone leaders; the agent retries with backoff
- **Windows exec race conditions** — scheduled task name and output file are now unique per request (UnixNano suffix), preventing collisions on concurrent `become=true` requests
- **Windows PowerShell injection** — switched the become-user execution path to `-EncodedCommand` (UTF-16LE base64), eliminating metacharacter injection via `$`, backtick, and `$()` in command strings
- **Insecure temp files on Windows** — output file now uses a per-request unique path instead of a predictable hardcoded name, preventing symlink privilege escalation
- **Path traversal in agent file transfers** — `filepath.Clean` applied to all paths in `put_file`, `fetch_file`, and `deploy` before use
- **Fact cache storm** — replaced unbounded goroutine-per-result with a bounded 8-worker pool, preventing DB connection exhaustion on large broadcast queries
- **Inventory N+1 queries** — replaced per-agent `GetFacts` calls with a single bulk `GetAllFacts` query, reducing DB round-trips from N+1 to 2

## [0.7.1] - 2026-05-15

### Added

- **Default server config file** — RPM and DEB packages now install `/etc/dirq/server.conf` with all options documented and commented; marked as `config(noreplace)` so upgrades preserve edits

### Changed

- **RPMs built on AlmaLinux 8** — server binary now links against glibc 2.28, making it installable on both RHEL 8 and RHEL 9

## [0.7.0] - 2026-05-15

### Added

- **`dirq graph`** — display the agent topology tree in the terminal; zone leaders marked `[ZL]`, online/offline status shown with filled/hollow dots
- **`dirq graph --dot`** — emit topology in Graphviz DOT format for rendering with `dot -Tpng`
- **`dirq --version`** — CLI now reports its version (injected at build time via `-ldflags`)
- **`RequestPeers` RPC** — agents that lose their parent ask the server for a new assignment instead of falling back to a direct server connection
- **Orphan reassignment** — when a zone leader goes offline, the server immediately reassigns its children to healthy parents

### Fixed

- **Agents couldn't connect to peer relay servers** — TLS verification failed because agent certs only have `localhost` as a SAN; peer connections now override `ServerName` to match
- **`connectUpstream` silently fell back to server** — relay agents that couldn't reach their parent opened a direct `AgentStream`, hiding the failure; now returns an error so fallback and `RequestPeers` paths are tried first
- **Rebalancer thrashing** — agents demoted to relay that bounced back to a direct connection were re-demoted every 30 seconds; added exponential backoff dampening (1m to 30m)
- **Server used agent-reported IP for `ListenAddr`** — incorrect in Docker/NAT environments; server now overrides with the peer IP observed on the gRPC connection
- **`RequestPeers` marked healthy parents offline** — if an agent was freshly reassigned to a new parent but hadn't connected yet, a second `RequestPeers` call would mark the new (healthy) parent offline; now checks for an active server stream first
- **Graph showed stale parent relationships** — agents connected directly to the server still appeared under their old (dead) parent in the topology

## [0.6.0] - 2026-05-15

### Added

- **SQLite backend** — embedded SQLite database as the default, eliminating the PostgreSQL dependency for single-server deployments; set `DIRQ_DB_URL=postgres://...` to use PostgreSQL instead
- **Field projection and tabular output** — `SELECT os_info.os, packages.name` now returns flat rows with only the requested fields; array modules (packages, services, disk, network) are expanded into individual rows; CLI renders results as aligned tables instead of JSON
- **Windows CLI installer** — NSIS installer (`dirq-cli-VERSION-setup.exe`) installs the CLI binary and connection plugin, adds to PATH
- **macOS client packages** — tarballs for amd64 and arm64 with CLI binary, connection plugin, and LICENSE
- **Ansible connection plugin in all packages** — RPM, DEB, and `make install` now include the standalone connection plugin at `/usr/share/dirq/connection_plugins/`; CLI searches standard install paths automatically
- **`make demo`** — local 10-agent demo fleet using `podman kube play` with TLS, auth, and SQLite; prints bootstrap token for copy/paste setup
- **`dirq doctor`** — checks Ansible version (minimum 2.15), `ansible` CLI availability, and verifies the connection plugin file exists

### Changed

- **Default database is SQLite** — `sqlite:///var/lib/dirq/dirq.db` unless `DIRQ_DB_URL` specifies PostgreSQL; server binary now requires CGO
- **Auth disabled skips all validation** — previously validated stale tokens even when `DIRQ_AUTH_DISABLED=true`
- **Demo uses distinctive ports** — 19080 (REST) and 19051 (gRPC) to avoid conflicts

### Fixed

- **`dirq run` failed with self-signed TLS** — CLI now forwards `--tls-insecure` as `DIRQ_TLS_INSECURE=true` to the Ansible subprocess
- **Containerfile installed binaries to `/usr/local/bin`** — moved to `/usr/bin` for consistency with RPM/DEB packages
- **CLI printed usage text on runtime errors** — `SilenceUsage` set globally so command failures don't dump help
- **Server exited immediately if database wasn't ready** — now retries connection for up to 60 seconds

## [0.5.1] - 2026-05-14

### Fixed

- **RPM/DEB packages now build from source** — spec files and debian/rules compile with `go build` inside the build environment instead of copying pre-built binaries; packages are reproducible from source
- **RPM binaries were not executable** — `cp` replaced with `install -m 0755`
- **Packages installed to `/usr/local/bin`** — moved to `/usr/bin` (standard for distribution packages); systemd service files updated to match
- **LICENSE missing from packages** — included via `%license` (RPM) and as `/usr/share/doc/*/copyright` (DEB) in all three packages
- **Ansible connection plugin lost per-host inventory vars** in some AAP execution paths — restored variable manager access for multi-DC routing
- **Ansible fact cache ignored `fact_caching_connection`** from ansible.cfg — now reads the configured URL before falling back to `DIRQ_SERVER_URL`
- **Ansible collection Python client failed on self-signed TLS** — added `DIRQ_TLS_INSECURE` support

### Changed

- **`make` default target** is now `help` — shows all available targets instead of building

## [0.5.0] - 2026-05-14

### Added

- **`dirq exec`** — execute a command or script across the fleet in parallel with streaming NDJSON results; supports `--become`, `--script`, `--container`, `--timeout`
- **Broadcast deploy** — `dirq deploy` now sends the package through the mesh tree once per link instead of once per host; each relay forwards to its children, only targeted agents write and install
- **Broadcast exec** — `dirq exec` broadcasts through the mesh like queries; one message traverses each link regardless of fleet size
- **Session token authentication** — agents receive a signed, time-stamped session token during registration; server and relay peers verify tokens cryptographically before accepting stream connections
- **Registration secret** — optional `DIRQ_REGISTRATION_SECRET` pre-shared key gates who can register agents with the server
- **Config file TLS/signing support** — `tls_ca`, `tls_cert`, `tls_key`, `tls_insecure`, `tls_disabled`, `signing_key`, `signing_pub` all configurable via config file in addition to environment variables
- **WHERE clause for `hosts list/tag/untag`** — operate on multiple hosts by query instead of one-at-a-time by ID

### Changed

- **Deploy is now parallel by default** — broadcast replaces the per-host rolling wave approach; the `--parallel` flag has been removed
- **Auto-generated keys stored in `/var/lib/dirq/`** instead of `/tmp/` for security (0700 permissions; falls back to user-private temp dir)
- **Bootstrap token written to file** (`/var/lib/dirq/bootstrap-token`) instead of server log to prevent credential leakage
- **API tokens accepted only via `Authorization` header** — query string `?token=` parameter removed to prevent log/proxy credential leakage
- **Auto-generated TLS certs use CA verification** instead of forcing `InsecureSkipVerify`; certs are reused if already present so server and agent share the same CA
- **Exec stdout/stderr base64-encoded** on the wire for binary safety across all exec endpoints

### Fixed

- **Concurrent deploys could overwrite each other** — temp filenames now include a unique deploy ID
- **`hosts list WHERE` made N+1 API calls** — now uses query result data directly (single call)
- **CLI created a new TLS transport per request** when `--tls-insecure` was set, preventing connection reuse
- **Ansible connection plugin lost per-host inventory vars** in some AAP execution paths — restored proper variable manager access for multi-DC routing
- **Ansible fact cache ignored `fact_caching_connection`** from ansible.cfg — now reads the configured URL before falling back to `DIRQ_SERVER_URL`
- **Ansible collection Python client failed on self-signed TLS** — added `DIRQ_TLS_INSECURE` support to the shared API client
- **Makefile default target** changed from `build` to `help` — running `make` now shows available targets

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

[0.14.1]: https://github.com/atgreen/dirq/releases/tag/v0.14.1
[0.14.0]: https://github.com/atgreen/dirq/releases/tag/v0.14.0
[0.13.0]: https://github.com/atgreen/dirq/releases/tag/v0.13.0
[0.12.2]: https://github.com/atgreen/dirq/releases/tag/v0.12.2
[0.12.1]: https://github.com/atgreen/dirq/releases/tag/v0.12.1
[0.12.0]: https://github.com/atgreen/dirq/releases/tag/v0.12.0
[0.11.4]: https://github.com/atgreen/dirq/releases/tag/v0.11.4
[0.11.3]: https://github.com/atgreen/dirq/releases/tag/v0.11.3
[0.11.2]: https://github.com/atgreen/dirq/releases/tag/v0.11.2
[0.11.1]: https://github.com/atgreen/dirq/releases/tag/v0.11.1
[0.11.0]: https://github.com/atgreen/dirq/releases/tag/v0.11.0
[0.10.0]: https://github.com/atgreen/dirq/releases/tag/v0.10.0
[0.9.2]: https://github.com/atgreen/dirq/releases/tag/v0.9.2
[0.9.1]: https://github.com/atgreen/dirq/releases/tag/v0.9.1
[0.9.0]: https://github.com/atgreen/dirq/releases/tag/v0.9.0
[0.8.0]: https://github.com/atgreen/dirq/releases/tag/v0.8.0
[0.7.1]: https://github.com/atgreen/dirq/releases/tag/v0.7.1
[0.7.0]: https://github.com/atgreen/dirq/releases/tag/v0.7.0
[0.6.0]: https://github.com/atgreen/dirq/releases/tag/v0.6.0
[0.5.1]: https://github.com/atgreen/dirq/releases/tag/v0.5.1
[0.5.0]: https://github.com/atgreen/dirq/releases/tag/v0.5.0
[0.4.0]: https://github.com/atgreen/dirq/releases/tag/v0.4.0
[0.3.0]: https://github.com/atgreen/dirq/releases/tag/v0.3.0
[0.2.1]: https://github.com/atgreen/dirq/releases/tag/v0.2.1
[0.2.0]: https://github.com/atgreen/dirq/releases/tag/v0.2.0
[0.1.0]: https://github.com/atgreen/dirq/releases/tag/v0.1.0
