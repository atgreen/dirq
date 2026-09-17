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
```

!!! warning "Deploys are not staged"
    Every targeted agent installs at the same time. There is no rolling
    or depth-first ordering, so a relay can be updated while its children
    are mid-install. Take care when deploying a package that restarts the
    agent itself — including `dirq-agent` — across a fleet you are relying
    on to stay connected.

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
