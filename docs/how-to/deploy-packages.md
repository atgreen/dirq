# Deploy packages

Deploy RPM, DEB, or MSI packages across the fleet through the relay mesh.
Designed primarily for non-disruptive self-updates of the dirq-agent package
itself — the depth-first rolling strategy updates deepest nodes first, working
up the tree so a parent is never updated while its children are mid-install.
This keeps the relay mesh intact throughout the upgrade.

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
