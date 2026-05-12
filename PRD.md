# PRD: DirQ — Real-Time Endpoint Query & Ansible Inventory Platform

## Problem Statement

Infrastructure teams managing mixed Windows/Linux fleets lack a fast, unified way to
query live system state across hundreds or thousands of endpoints. Today they cobble
together SSH scripts, WinRM calls, WMI queries, and static inventory files — each with
different auth models, output formats, and failure modes. When an incident hits ("which
servers have disk usage above 90%?", "how many hosts are running kernel X?"), answers
take minutes to hours instead of seconds.

Separately, Ansible Automation Platform (AAP) users maintain inventory sources and
fact caches that go stale between playbook runs. There is no first-class way to feed
live endpoint data into AAP as an inventory source with rich, up-to-date facts.

The people who feel this pain most are:
- **Platform / infrastructure engineers** who manage the fleet and respond to incidents.
- **Ansible automation developers** who need accurate, fresh host data to write reliable playbooks.
- **Security / compliance teams** who need to answer audit questions about fleet state quickly.

## Target User

**Primary:** Platform engineers and sysadmins responsible for managing a mixed
Windows/Linux server fleet (1,000–100,000+ nodes) who also use Ansible Automation Platform.

**Secondary:** Security and compliance teams who need rapid fleet-wide queries for
audit and incident response.

**What they do today:** Run ad-hoc SSH/WinRM commands, maintain static YAML/INI
inventory files, rely on periodic fact-caching playbooks, or use expensive commercial
tools that don't integrate natively with Ansible.

## Goals

1. An admin can query any registered endpoint and receive structured results in
   under 5 seconds (single host) or under 60 seconds (fleet-wide, 100k nodes).
2. AAP can consume DirQ as a native inventory source, with all collected data
   exposed as Ansible facts — no manual sync or export step.
3. Agents are lightweight enough to run on production servers without measurable
   impact on workload performance (< 1% CPU, < 50 MB RAM at idle).
4. The system supports both Windows and Linux from day one — not Linux-first with
   Windows bolted on later.
5. Ad-hoc queries return structured, machine-parseable results (JSON) that are
   consistent across operating systems for the same logical query.
6. The server scales horizontally to support 100,000 concurrently connected agents.

## Non-Goals

- **Configuration management / remediation (V1).** DirQ V1 is read-only. It
  collects and reports; it does not change system state. Remediation is Ansible's
  job. (Phase 2 adds an execution transport for AAP — see below — but DirQ
  never decides what to run; AAP retains full orchestration authority.)
- **Log aggregation or streaming telemetry.** This is not a replacement for
  Splunk, Elastic, or Prometheus. DirQ answers point-in-time questions.
- **Agent deployment / orchestration.** How agents get installed on endpoints is
  out of scope. Users bring their own deployment mechanism (Ansible, SCCM, GPO, etc.).
- **Multi-tenancy / SaaS.** V1 is a self-hosted, single-org deployment.
- **Custom agent plugins or extension API.** V1 ships with a fixed set of built-in
  query modules. Extensibility is a future iteration.
- **LDAP/SSO integration.** V1 uses API tokens for admin auth. Enterprise identity
  provider integration is a future iteration.

## User Stories

### P0 (Must have for launch)

- **As a platform engineer, I want to install a lightweight agent on a Linux server,
  so that it registers itself with the DirQ server and becomes queryable.**
  - Acceptance: Agent installs via RPM/DEB/tarball. On startup, it registers with the
    server (hostname, OS, IP). The server's host list shows the new endpoint within
    60 seconds.

- **As a platform engineer, I want to install a lightweight agent on a Windows server,
  so that Windows hosts are first-class citizens alongside Linux.**
  - Acceptance: Agent installs via MSI or standalone exe. Runs as a Windows Service.
    Registers identically to the Linux agent. Same query interface, same fact schema.

- **As an admin, I want to run an ad-hoc query using a query DSL against a single
  host or group of hosts and get structured results in real time, so that I can
  answer operational questions without waiting for a scheduled collection.**
  - Acceptance: CLI or API call specifying target(s) and a DirQ query expression
    returns JSON results. Single-host response < 5 seconds. Fleet-wide query
    (100k hosts) streams results within 60 seconds. Timeout and partial-result
    behavior is well-defined (results stream back as agents respond; final summary
    includes responded/timed-out/unreachable counts).

- **As an admin, I want a query DSL that supports filtering, aggregation, and
  field selection, so that I can ask precise questions across my fleet without
  post-processing raw data.**
  - Acceptance: The DSL supports:
    - Field selection: `SELECT hostname, disk.pct_used, cpu.cores`
    - Filtering: `WHERE disk.pct_used > 90 AND os = 'linux'`
    - Aggregation: `GROUP BY os`, `COUNT`, `AVG`, `MIN`, `MAX`
    - Target scoping: `FROM group:webservers` or `FROM tag:datacenter=us-east`
    - Example query: `SELECT hostname, disk.mount, disk.pct_used FROM tag:prod WHERE disk.pct_used > 80 ORDER BY disk.pct_used DESC`
    - Syntax errors return clear error messages with position indicators.
    - The DSL is documented with examples in CLI help and user docs.

- **As an admin, I want to query disk configurations across my fleet, so that I can
  identify hosts with low disk space or unusual mount layouts.**
  - Acceptance: Query returns filesystem type, mount point, total size, used size,
    available size, and percentage used for each mounted volume. Works on Linux
    (ext4, xfs, etc.) and Windows (NTFS, ReFS) with a unified schema.

- **As an admin, I want to query CPU and memory information, so that I can understand
  capacity and plan for scaling.**
  - Acceptance: Returns core count (physical and logical), CPU model, total RAM,
    available RAM. Consistent schema across OS.

- **As an Ansible developer, I want to configure AAP to use DirQ as an inventory
  source, so that my playbooks always target the current set of live hosts.**
  - Acceptance: DirQ ships an inventory plugin compatible with AAP's inventory source
    interface. `ansible-inventory --list` returns all registered hosts with groups.
    Hosts that have not checked in within a configurable threshold are excluded or
    flagged.

- **As an Ansible developer, I want the data DirQ collects to appear as Ansible
  facts, so that I can use them in playbook conditionals and templates without
  running a separate gather_facts step.**
  - Acceptance: The inventory plugin populates `hostvars` with DirQ-collected data
    under a `dirq_*` namespace (e.g., `dirq_disk`, `dirq_cpu`, `dirq_memory`).
    Facts are usable in Jinja2 templates and `when:` conditionals.

- **As an admin, I want agents to authenticate with the server using mutual TLS or
  pre-shared keys, so that only authorized agents can register and only the
  legitimate server can issue queries.**
  - Acceptance: Agent-to-server communication is encrypted (TLS 1.2+). Agents
    authenticate to the server. Unauthenticated agents are rejected. No cleartext
    credentials on the wire.

- **As an admin, I want a `dirq` CLI tool to submit queries, manage hosts, and
  generate API tokens, so that I can operate DirQ from the terminal and scripts.**
  - Acceptance: CLI supports at minimum: `dirq query <DSL expression>`,
    `dirq hosts list`, `dirq hosts show <hostname>`, `dirq token create`.
    Output defaults to a human-readable table; `--json` flag for machine output.
    CLI authenticates via API token (config file or environment variable).

- **As an admin, I want a web UI with an interactive query console, so that I can
  write queries, see live streaming results, and browse fleet status in a browser.**
  - Acceptance: Web UI includes:
    - Query editor with syntax highlighting and autocomplete for the DirQ DSL.
    - Live result streaming — rows appear as agents respond, with a progress
      indicator (responded / total / timed out).
    - Fleet dashboard: registered hosts, online/offline, last check-in, OS breakdown.
    - Host detail view: all cached facts for a single host.
    - Query history: recent queries with results, re-runnable.
    - Authentication via API token.
    - Web UI is served by the DirQ server process (no separate frontend deployment).

- **As an admin, I want the server to push agent updates to endpoints, so that
  I can roll out new agent versions across 100k nodes without a separate
  deployment workflow.**
  - Acceptance: Admin uploads a new agent binary to the server. Server pushes the
    update to targeted agents (all, group, or staged rollout). Agents verify the
    binary signature before applying. Agent restarts with the new version and
    re-establishes its gRPC stream. Rollback is possible by pushing the previous
    version. Update status is visible per-host (pending/downloading/applied/failed).

### P1 (Should have)

- **As an admin, I want to query installed packages / software inventory, so that
  I can track what's deployed and identify hosts running vulnerable versions.**
  - Acceptance: Returns list of installed packages (name, version, source/repo) on
    Linux (rpm/dpkg) and Windows (Programs and Features / winget). Unified schema.

- **As an admin, I want to query network interface configurations, so that I can
  audit network settings fleet-wide.**
  - Acceptance: Returns interface name, IP addresses, MAC, MTU, link state.

- **As an admin, I want to query running services and their states, so that I can
  verify critical services are running across the fleet.**
  - Acceptance: Returns service name, display name, state (running/stopped/etc.),
    start type. Works with systemd (Linux) and Windows Services.

- **As an admin, I want to define host groups in DirQ (e.g., by tag, OS, location),
  so that I can target queries and Ansible plays at logical groupings.**
  - Acceptance: Hosts can be tagged at registration or via the API. Queries and
    inventory can be scoped to groups. Tags flow through to Ansible inventory groups.

### P2 (Nice to have)

- **As an admin, I want to schedule recurring queries and store historical results,
  so that I can trend system metrics over time.**

- **As an admin, I want to export query results to CSV/JSON files for reporting.**

- **As a security engineer, I want to query OS patch level and pending updates.**

- **As an admin, I want the DirQ server to emit events (webhooks or message queue)
  when agents go offline or query thresholds are breached.**

### Phase 2 — AAP Execution Transport

Phase 2 extends the DirQ agent mesh into an **execution transport for AAP**,
allowing Ansible playbooks to reach managed hosts through the existing DirQ
relay mesh instead of SSH or WinRM. DirQ does not decide what to execute —
AAP retains full orchestration authority (RBAC, credential vault, approval
workflows, audit trail). DirQ provides the connection layer only.

**Why:** In large enterprises, managed hosts sit behind NAT, firewalls, or
bastion layers that make inbound SSH/WinRM difficult or impossible. The DirQ
agent already maintains a persistent, agent-initiated outbound gRPC connection
through the relay mesh. Reusing that connection as an Ansible transport
eliminates the need for inbound firewall rules, SSH credential management,
and WinRM configuration — while preserving AAP's governance model.

#### User Stories (Phase 2)

- **As an Ansible developer, I want to use `connection: dirq` in my playbooks,
  so that AAP reaches managed hosts through the DirQ mesh instead of SSH/WinRM.**
  - Acceptance: A custom Ansible connection plugin (`connection: dirq`) implements
    `exec_command()`, `put_file()`, and `fetch_file()` by routing requests through
    the DirQ server API and relay mesh. Playbooks run identically to SSH-based
    execution. The plugin is packaged in a custom Execution Environment image.

- **As an Ansible developer, I want DirQ-collected facts to be automatically
  available during playbook execution without a `gather_facts` step, so that
  plays start faster and use live data.**
  - Acceptance: When `connection: dirq` is used, the connection plugin injects
    cached DirQ facts into the play's `hostvars` before task execution begins.
    `gather_facts: false` is the recommended default; users can still enable
    standard fact gathering if needed.

- **As a platform engineer, I want to enable or disable execution capability
  per agent, so that I control which hosts accept remote commands.**
  - Acceptance: Agent configuration includes an `exec_enabled` flag (default:
    `false`). Agents with exec disabled reject execution requests and are not
    offered as execution targets. The flag is settable at deploy time and via
    the DirQ server API (with admin token). Agents report their exec capability
    during registration; the inventory plugin exposes this as a fact
    (`dirq_exec_enabled`).

- **As a security engineer, I want every command executed through the DirQ mesh
  to be logged with full attribution, so that I have an audit trail equivalent
  to or better than SSH session logging.**
  - Acceptance: The DirQ server logs each execution request with: timestamp,
    AAP job ID, job template name, target host, requesting user (from AAP),
    command or module invoked, and exit status. Logs are stored in PostgreSQL
    and queryable via the DirQ API. The agent also logs execution events
    locally to syslog / Windows Event Log.

- **As a platform engineer, I want AAP to execute playbooks against hosts that
  are behind NAT or firewalls with no inbound access, so that I don't need
  bastion hosts or VPN tunnels for automation.**
  - Acceptance: A host whose agent connects outbound through the relay mesh
    is reachable by AAP via `connection: dirq` with no inbound ports open.
    Works across NAT boundaries, air-gapped segments (with relay peers in
    the gap), and cloud VPCs.

#### Technical Approach

**Ansible Connection Plugin (`connection: dirq`):**
A Python connection plugin distributed as part of a custom EE image. The
plugin holds a gRPC channel to the DirQ server. When Ansible calls
`exec_command(cmd)`, the plugin sends an `ExecRequest` message to the DirQ
server, which routes it through the relay mesh to the target agent. The
agent executes the command locally and streams stdout/stderr/rc back through
the mesh. `put_file()` and `fetch_file()` work the same way — file content
is streamed through the existing gRPC bidirectional stream.

**Agent Exec Module:**
A new gRPC service on the agent alongside the existing query service. The
exec module handles three operations: run command (with optional become/sudo),
receive file (write to disk), and send file (read from disk). The exec
module is compiled into the same agent binary but only activated when
`exec_enabled: true`. All operations are gated by the server's mTLS
identity — the agent only accepts exec requests that originate from the
DirQ server, never from peer agents.

**Execution flow:**
1. Admin launches a job template in AAP with `connection: dirq`.
2. AAP spins up an EE container with the DirQ connection plugin.
3. The plugin connects to the DirQ server via gRPC.
4. For each target host, Ansible calls `exec_command()` / `put_file()` /
   `fetch_file()` through the plugin.
5. The DirQ server routes each request through the relay mesh to the
   target agent's exec module.
6. The agent executes locally and returns results back through the mesh.
7. The plugin returns results to Ansible as if they came from SSH.
8. AAP records the job result normally — full audit trail preserved.

**Security model:**
- Exec requests are only accepted from the DirQ server's mTLS identity.
  Peer agents cannot send exec requests to each other.
- The agent's `exec_enabled` flag must be true; otherwise requests are
  rejected at the agent.
- AAP's RBAC governs who can launch job templates. DirQ does not duplicate
  this — it trusts the authenticated server-originated request.
- All exec traffic is encrypted end-to-end via the existing mTLS mesh.
- Every execution is logged server-side (PostgreSQL) and agent-side
  (syslog / Event Log) with full attribution to the AAP job and user.
- File transfers are size-limited (configurable, default 100 MB) to
  prevent mesh abuse.

**What this replaces vs. what it doesn't:**
- **Replaces:** SSH/WinRM as the connection transport. Firewall rules for
  inbound access. SSH key distribution and rotation. WinRM HTTPS
  certificate management.
- **Does not replace:** AAP's orchestration, RBAC, credential vault,
  job scheduling, approval workflows, or audit logging. AAP remains the
  control plane; DirQ is the data plane.

## Technical Constraints

- **Agent language:** Go. Compiles to a single static binary for both Linux and
  Windows with excellent cross-compilation support (`GOOS=windows GOARCH=amd64`).
- **Communication protocol:** gRPC over mTLS everywhere — both agent-to-server
  and agent-to-agent links use the same gRPC transport. Provides bidirectional
  streaming, strong typing via protobuf, HTTP/2 multiplexing, and mature Go
  libraries on both platforms. Single port per process.
- **Communication topology: P2P relay mesh with gRPC transport.**
  - Agents form a peer-to-peer relay tree. Each agent acts as both a gRPC
    client (connects to upstream peer or server) and a gRPC server (accepts
    connections from downstream peers).
  - Only a small number of top-tier "zone leader" agents connect directly to
    the DirQ server. The bulk of agents connect to peers, not the server.
  - Queries propagate from the server through the relay tree. Results roll
    back up through the same path.
  - This reduces server-side and OpenShift router connection count from 100k
    to hundreds, solving the scalability bottleneck at the ingress layer.
  - **Peer discovery is server-seeded:** on first boot, an agent connects
    directly to the server to register. The server assigns it a peer list
    (upstream parent + optional sibling peers) based on topology rules
    (subnet, zone, tags). The agent then connects to its assigned peers and
    drops the direct server connection.
  - If all assigned peers are unreachable, the agent falls back to a direct
    server connection until the server can reassign it.
  - The server monitors the health of the relay tree and can rebalance the
    topology (reassign parents, promote new zone leaders) as agents join,
    leave, or fail.
- **Agent binary is both client and server.** Each agent listens on a
  configurable port for downstream peer connections (gRPC server) and
  maintains an outbound connection to its upstream parent (gRPC client).
  The same mTLS certificates are used for both directions.
- Agents must run on: RHEL/CentOS 8+, Ubuntu 20.04+, Windows Server 2016+.
- Agent binary must be statically compiled — no runtime dependencies on the
  managed host. Single binary, zero install prerequisites.
- Server component must run on Linux. Written in Go.
- **Storage backend:** PostgreSQL for all deployments. Stores host registry,
  fact cache, query history, relay topology, and agent update state. No
  embedded DB option — single code path, proven at scale.
- **Admin auth:** API token-based. Admins generate tokens via CLI. Tokens are
  scoped (read-only vs. admin). Stored as hashed values in PostgreSQL.
- **Deployment model:** OpenShift-native for production. DirQ server runs as
  containerized pods that scale horizontally on OpenShift. Because agents
  connect through the P2P relay mesh (not directly to the server), the
  OpenShift router only handles zone leader connections + admin API/UI
  traffic — not 100k agent streams. PostgreSQL is an external service.
  For development and initial testing, the server and PostgreSQL both run
  as containers on Podman (`podman-compose` or individual containers).
  The same container image is used in both environments.
- **Fact cache TTL:** Global default TTL (configurable, e.g., 15 minutes) with
  per-query-type overrides (e.g., `disk: 5m`, `cpu: 1h`). Facts past TTL are
  still returned but marked as stale. Inventory plugin can be configured to
  exclude stale facts.
- **Agent auto-update:** Server can push new agent binaries through the relay
  tree. Binaries must be cryptographically signed (Ed25519). Agents verify
  signatures before applying. Staged rollout support (percentage-based or
  group-based).
- The Ansible inventory plugin must be compatible with AAP 2.x / awx and
  standard `ansible-inventory` CLI. Written in Python.
- All communication (agent-to-agent and agent-to-server) encrypted via mTLS.
- The system must function in air-gapped environments (no phone-home, no cloud
  dependency).

## Architecture Overview (Conceptual)

```
                    P2P Relay Mesh (all links are gRPC/mTLS)
                    ==========================================

  ┌─────────┐   ┌─────────┐   ┌─────────┐   ┌─────────┐
  │ Agent   │   │ Agent   │   │ Agent   │   │ Agent   │
  │ (leaf)  │   │ (leaf)  │   │ (leaf)  │   │ (leaf)  │
  └────┬────┘   └────┬────┘   └────┬────┘   └────┬────┘
       │              │              │              │
       ▼              ▼              ▼              ▼
  ┌──────────────────────┐   ┌──────────────────────┐
  │ Agent (relay peer)   │   │ Agent (relay peer)   │
  │ - gRPC server (down) │   │ - gRPC server (down) │
  │ - gRPC client (up)   │   │ - gRPC client (up)   │
  └──────────┬───────────┘   └──────────┬───────────┘
             │                          │
             ▼                          ▼
        ┌─────────────────────────────────────┐
        │ Agent (zone leader)                 │
        │ - gRPC server (accepts relay peers) │
        │ - gRPC client (connects to server)  │
        └──────────────┬──────────────────────┘
                       │
         ══════════════╪═══════════════  (OpenShift Route)
                       │                 (only zone leaders
                       ▼                  cross this boundary)
  ┌─────────────────────────────────────────────────────────┐
  │              OpenShift / Podman                          │
  │                                                         │
  │  ┌────────────┐ ┌────────────┐ ┌────────────┐          │
  │  │ Server Pod │ │ Server Pod │ │ Server Pod │  ...      │
  │  │  (Go)      │ │  (Go)      │ │  (Go)      │          │
  │  │            │ │            │ │            │          │
  │  │ • Topology │ │ • Topology │ │ • Topology │          │
  │  │   Manager  │ │   Manager  │ │   Manager  │          │
  │  │ • Query Eng│ │ • Query Eng│ │ • Query Eng│          │
  │  │ • REST API │ │ • REST API │ │ • REST API │          │
  │  │ • Web UI   │ │ • Web UI   │ │ • Web UI   │          │
  │  └─────┬──────┘ └─────┬──────┘ └─────┬──────┘          │
  │        └───────────────┼──────────────┘                  │
  └────────────────────────┼────────────────────────────────┘
                           ▼
                    ┌──────────────┐
                    │  PostgreSQL  │  (external)
                    │  host reg,   │
                    │  fact cache, │
                    │  topology,   │
                    │  queries     │
                    └──────────────┘
         │                    │                    │
         ▼                    ▼                    ▼
     DirQ CLI            Web UI              AAP / awx
                                       (Ansible Inventory
                                        Plugin — Python)
```

### Connection & Relay Model

Agents form a P2P relay tree. Every link in the tree is a gRPC/mTLS connection.
Each agent runs both a gRPC server (for downstream peers) and a gRPC client
(for its upstream parent). Only zone leader agents connect directly to the
DirQ server — the rest communicate through the relay mesh.

**Bootstrap flow:**
1. New agent starts, connects directly to the DirQ server (short-lived).
2. Server registers the agent, assigns it a role (leaf, relay, or zone leader)
   and a peer list (upstream parent + optional siblings) based on subnet, zone,
   and topology rules stored in PostgreSQL.
3. Agent connects to its assigned upstream parent via gRPC.
4. Agent drops the direct server connection — it now communicates through the mesh.
5. If all peers are unreachable, agent falls back to direct server connection.

**Query flow:**
1. Admin submits a query via CLI, Web UI, or REST API — hits any server pod.
2. Server pod parses the DirQ query DSL and pushes the query to connected
   zone leaders.
3. Zone leaders relay the query down through the tree to all target agents.
4. Each agent executes the query locally (agent-side filtering) and sends
   results back up through the relay path.
5. Results roll up through the tree to zone leaders, then to the server.
6. Server aggregates results and returns them to the caller.

**Why this works at scale:**
- OpenShift routers handle hundreds of zone leader connections, not 100k.
- Adding agents grows the mesh, not the server connection count.
- Network-local traffic stays local — agents on the same subnet relay
  to each other at LAN speed.
- If a relay agent dies, its children detect the broken stream and fall back
  to the server for reassignment. Server assigns them a new parent.

**Cross-pod coordination:** Server pods discover each other via headless Service
DNS (OpenShift) or a `peers` table in PostgreSQL (Podman). Each pod tracks which
zone leaders it owns. Queries are routed to the correct pod via direct pod-to-pod
gRPC.

**Dev/test environment:** Single DirQ server container + PostgreSQL container
on Podman. Agents on the same laptop or local VMs connect directly (no relay
tree needed at small scale — every agent is effectively a zone leader).

## Decided

1. **Agent language:** Go. Cross-platform static binaries, excellent gRPC support.
2. **Communication protocol:** gRPC with bidirectional streaming over mTLS.
3. **Scale target:** 100,000 managed nodes.
4. **Fact cache TTL:** Global default + per-query-type overrides.
5. **Storage backend:** PostgreSQL for all deployments.
6. **Admin auth:** API tokens (V1). LDAP/SSO deferred.
7. **Server deployment:** OpenShift-native (production), Podman (dev/test). External PG.
8. **Agent updates:** Server-triggered push over gRPC. Signed binaries.
9. **Query model:** Full query DSL with SELECT/WHERE/GROUP BY/aggregation.
10. **Name:** DirQ (Direct Query).

11. **Agent topology:** P2P relay mesh. Agents form a tree; only zone leaders
    connect to the server. All links are gRPC/mTLS. Server-seeded peer discovery.
12. **Cross-pod coordination:** Direct pod-to-pod gRPC. No extra dependencies.
    - **OpenShift:** Headless Service DNS for pod discovery.
    - **Podman (dev/test):** `peers` table in PostgreSQL.
12. **Query execution model:** Agent-side filtering. Agents receive the parsed
    query, apply WHERE clauses locally, and return only matching results.
    Server handles aggregation (GROUP BY, COUNT, AVG, etc.) across the fleet.
    Minimizes network traffic at 100k scale.
13. **Agent binary signing:** Ed25519. Public key embedded in the agent binary
    at compile time. Server signs update binaries with the corresponding
    private key. No external signing infrastructure required. Air-gap friendly.
14. **Schema versioning:** Capability negotiation. Agents report their version
    and supported query modules at registration. Server only sends queries
    the agent can handle. Mixed-version fleets are a first-class scenario.
    Protobuf forward compatibility (additive fields only) provides the
    underlying wire-level safety.

15. **Phase 2 — Execution transport:** The DirQ relay mesh doubles as an
    Ansible connection transport. A custom `connection: dirq` plugin routes
    Ansible exec/put/fetch operations through the mesh to agents with
    `exec_enabled: true`. AAP retains full orchestration authority; DirQ
    provides the data plane only. Exec requests are server-originated
    (mTLS-gated) and fully audited.

## Open Questions

_(All major architectural decisions are resolved. Remaining questions are
implementation-level and will be answered during engineering design.)_

## Success Metrics

1. **Time to answer:** Fleet-wide queries return results in < 60 seconds for a
   100,000-node deployment (p95). Measured via server-side query telemetry.
2. **Inventory freshness:** AAP inventory reflects a new host within 2 minutes of
   agent startup. Measured via integration test.
3. **Agent resource footprint:** Idle agent uses < 1% CPU and < 50 MB RAM on the
   managed host. Measured via benchmark on reference hardware.
4. **Connection capacity:** Server sustains 100,000 concurrent gRPC streams without
   degradation. Measured via load test with simulated agents.
5. **Adoption:** 80% of the managed fleet has agents deployed within 30 days of
   launch (org-specific target).
6. **Fact accuracy:** DirQ-reported facts match `ansible.builtin.setup` output for
   equivalent data points in 99%+ of cases. Validated via automated comparison test.

### Phase 2 Success Metrics

7. **Exec transport parity:** Playbooks produce identical results when run with
   `connection: dirq` vs. `connection: ssh` (Linux) or `connection: winrm`
   (Windows). Validated via side-by-side integration test suite.
8. **Exec latency overhead:** Single-task execution via `connection: dirq` adds
   < 500 ms round-trip overhead compared to direct SSH on the same network.
   Measured end-to-end from Ansible task start to task completion.
9. **NAT/firewall traversal:** Playbook execution succeeds against hosts with
   no inbound ports open, connected only through the outbound relay mesh.
   Validated via integration test with firewall rules blocking all inbound.
10. **Audit completeness:** 100% of exec operations are logged with AAP job ID,
    user, target host, command, and exit status. Validated via audit log query
    after test job run.
