# Multi-datacenter deployment

Run one DirQ server per datacenter. Meshes never span DC boundaries.

!!! note
    The [Production Deployment](production-deployment.md) fundamentals apply to
    every server below — each must observe agents' real source IPs and persist
    `/var/lib/dirq`.

```
  DC us-east                          DC eu-west
  ┌──────────────────────┐            ┌──────────────────────┐
  │ Agents ──► DirQ      │            │ Agents ──► DirQ      │
  │            Server    │            │            Server    │
  │            + PG      │            │            + PG      │
  └──────────┬───────────┘            └──────────┬───────────┘
             │                                   │
             ▼                                   ▼
  ┌──────────────────────────────────────────────────────────┐
  │                AAP Controller                            │
  │  Inventory Source per DC → all merge into one inventory  │
  │  Each host carries dirq_server_url from its DC           │
  └──────────────────────────────────────────────────────────┘
```

The inventory plugin sets `dirq_server_url` per host. The connection plugin reads it automatically — a host from `us-east` routes through `dirq-us-east`, a host from `eu-west` routes through `dirq-eu-west`, even in the same play.

```yaml
- hosts: tag_env_prod          # spans all DCs
  connection: atgreen.dirq.dirq
  tasks:
    - command: uptime          # routed through correct DC per host
```
