# Ansible integration

The inventory plugin exposes DirQ fleet data to Ansible as groups and host variables. To build inventories from queries, see [Ansible inventories](../how-to/ansible-inventories.md); for Ansible Automation Platform setup, see [AAP](../how-to/aap.md).

## Inventory Groups

The inventory plugin creates a nested group hierarchy from agent metadata and tags:

```
@all
├── @os_linux / @os_windows
├── @arch_amd64 / @arch_arm64
├── @exec_enabled
├── @tag_env
│   ├── @tag_env_prod
│   └── @tag_env_dev
├── @tag_role
│   ├── @tag_role_webserver
│   └── @tag_role_database
└── @tag_dc
    ├── @tag_dc_us_east
    └── @tag_dc_eu_west
```

Target hosts with standard Ansible patterns:

```yaml
hosts: os_linux
hosts: tag_env_prod
hosts: tag_role_webserver:&os_linux       # intersection
hosts: exec_enabled
```

## Host Variables

All collected data exposed as `dirq_*` hostvars:

```yaml
dirq_agent_id: "abc-123"
dirq_os: "linux"
dirq_cpu: { physical_cores: 8, logical_cores: 16, ... }
dirq_memory: { total_bytes: 34359738368, pct_used: 34.4, ... }
dirq_disk: { partitions: [{ mount_point: "/", pct_used: 67.3, ... }] }
dirq_tag_env: "prod"
dirq_exec_enabled: true
```
