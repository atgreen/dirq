# Deploy packages

Deploy RPM, DEB, or MSI packages across the fleet through the relay mesh.
The package is broadcast down the tree, so each link carries it once
regardless of fleet size.

```bash
# Deploy to all exec-enabled agents
dirq deploy ./patch-2026-05.rpm

# Deploy to specific hosts
dirq deploy ./patch.rpm WHERE tag.env = 'prod'

# Windows packages
dirq deploy ./agent-0.3.0.msi WHERE os_info.os = 'windows'

# Install as the agent's own user instead of escalating
dirq deploy ./patch.rpm --become=false

# Install bottom-up: leaves first, relays last
dirq deploy ./dirq-agent.rpm --bottom-up
```

!!! warning "Deploys are not staged by default"
    Without `--bottom-up`, every targeted agent installs at the same time,
    so a relay can be updated while its children are still mid-install. Take
    care when deploying a package that restarts the agent itself — including
    `dirq-agent` — across a fleet you are relying on to stay connected.

## Bottom-up ordering

`--bottom-up` installs in waves ordered by mesh depth, **deepest first**.
Every agent at one depth reaches a terminal result — install finished,
failed, or its stream dropped — before the next, shallower wave starts.
Because a child always sits deeper than its parent, a relay is never
updated while an agent beneath it is still installing:

```bash
# Roll a new dirq-agent through the fleet leaves-up, so a relay restarts
# only after its whole subtree already has.
dirq deploy ./dirq-agent-0.27.0.rpm --bottom-up
```

The same `--bottom-up` flag works on [`dirq exec`](../reference/cli.md),
which is the safe way to run a fleet-wide `reboot`:

```bash
dirq exec --bottom-up -- reboot -r now
```

A wave that does not fully report (a hard timeout, or you cancel the run)
stops the rollout rather than acting on a relay whose subtree may still be
working. Install *failures* (a non-zero return code) are terminal results
and do not stop it — the run continues and reports them, wave by wave.

## Targeting

A deploy goes only to agents that have exec enabled, and the WHERE clause
selects from those. Tag, hostname and fact conditions all work, and a
fact-based condition is resolved against the fleet first, so
`WHERE os_info.os = 'windows'` targets exactly the Windows hosts.

Agents matching the query but without exec enabled are skipped.

## Privilege escalation

Installing a package normally needs root, so deploy escalates by default.
Where the agent already runs as root — a container image, for instance —
escalation is skipped automatically rather than requiring `sudo` to be
installed.

| Flag | Default | Effect |
|------|---------|--------|
| `--become` | `true` | Run the install with privilege escalation |
| `--become-user` | `root` | User to become |

Package type is detected from the file extension:

- `.rpm` → `rpm -U`
- `.deb` → `dpkg -i`
- `.msi` → `msiexec /i ... /qn`
