# Built-in query modules

These are the modules an agent collects and exposes to the [Query DSL](query-dsl.md).

| Module | Data collected |
|--------|---------------|
| `cpu` | Physical/logical cores, model name, vendor |
| `memory` | Total, available, used bytes; percent used; swap |
| `disk` | Per-partition: device, mount point, fs type, total/used/free bytes, percent used |
| `os_info` | Hostname, OS, version, arch, uptime, kernel version, distro, distro_version, distro_family |
| `packages` | Installed packages: name, version, arch, source (rpm/dpkg/registry) |
| `network` | Interfaces: name, MAC, MTU, flags, IP addresses (loopback filtered) |
| `services` | Services: name, display name, state, start type (systemd/Windows Services) |
| `hotfixes` | Windows hotfixes: kb_id, description, installed_on (Get-HotFix) |
