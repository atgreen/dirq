# Why DirQ?

DirQ ("Direct Query") is an agent-based platform for querying and managing large Windows/Linux fleets. Agents form a peer-to-peer relay mesh and report data back to a central server. The server acts as an Ansible Automation Platform (AAP) inventory source, exposes collected data as structured facts, and can route Ansible execution through the mesh as an alternative to SSH/WinRM connectivity.

## The key idea

The core idea is simple:

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

For example:

- Find only hosts with disks over 90%, turn that into an inventory, then run a cleanup or expansion playbook.
- Query for hosts with vulnerable OpenSSL package versions, build an inventory from the result, and patch only those systems.
- A new CVE drops — run `dirq cve CVE-2024-6345` and instantly see which hosts are vulnerable and which are already patched, across the entire fleet.
- Query for hosts where `sshd` or another critical service is stopped, generate an inventory, and run a remediation playbook immediately.
- Quick ad-hoc check: `dirq exec WHERE tag.env = 'prod' -- uptime` to see every prod host's uptime without setting up a playbook.

## When DirQ helps

DirQ is useful when traditional fleet access patterns start breaking down:

1. **Large locked-down environments** — managed hosts cannot accept inbound SSH or WinRM.
2. **Segmented enterprise networks** — a single control plane across data centers, edge sites, or heavily firewalled zones.
3. **Query-driven Ansible targeting** — inventories based on live fleet state, not stale static groups.
4. **Ansible without transport pain** — keep your playbooks, drop the SSH/WinRM dependency.
5. **Real-time CVE response** — a vulnerability drops and you need to know which hosts are affected *now*, not after the next scheduled scan.
6. **Real-time fleet troubleshooting** — answer "which prod hosts have disks over 90%?" and act on it immediately.
7. **Very large estates** — server connection count stays bounded while the fleet grows.

## What makes DirQ different

- **Mesh-first architecture:** agents relay for each other, so the fleet becomes its own transport.
- **Structured query model:** modules return normalized data instead of raw command output.
- **Ansible compatibility:** DirQ acts as query engine, inventory source, and execution transport — existing playbooks work without modification.
- **Inventory and execution in one system:** the same platform that knows the fleet can also target it.
- **Agent-side policy enforcement (OPA/Rego):** each host can locally allow or deny exec/file/deploy operations with a Rego policy — defense in depth even for validly-authorized requests. Express segregation of duties, break-glass, and per-AAP-user authorization for regulated fleets. See [Agent-side exec policy](../how-to/agent-exec-policy.md).

## Where to go next

- [Quick Start](../tutorials/quick-start.md) — run DirQ locally in a few minutes.
- [Architecture](../explanation/architecture.md) — how the mesh works and how it scales.
- [Query DSL](../reference/query-dsl.md) — the fleet query language.
- [Security](../explanation/security.md) — TLS, authentication, exec safety, and agent-side policy.
- Back to the [documentation home](../index.md).
