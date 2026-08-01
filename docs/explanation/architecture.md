# Architecture

```
  ┌──────────┐   ┌──────────┐   ┌──────────┐   ┌──────────┐
  │  Agent   │   │  Agent   │   │  Agent   │   │  Agent   │
  │  (leaf)  │   │  (leaf)  │   │  (leaf)  │   │  (leaf)  │
  └────┬─────┘   └────┬─────┘   └────┬─────┘   └────┬─────┘
       │              │              │              │
       ▼              ▼              ▼              ▼
  ┌───────────────────────┐   ┌───────────────────────┐
  │  Agent (relay peer)   │   │  Agent (relay peer)   │
  └───────────┬───────────┘   └───────────┬───────────┘
              │                           │
              ▼                           ▼
         ┌──────────────────────────────────────┐
         │         Agent (zone leader)          │
         └──────────────────┬───────────────────┘
                            │
              ══════════════╪══════════════
                            │  (OpenShift Route)
                            ▼
         ┌──────────────────────────────────────┐
         │         DirQ Server (Go)             │
         │  REST API · gRPC · Query Engine      │
         └──────────────────┬───────────────────┘
                            │
                            ▼
                  ┌──────────────────┐
                  │ SQLite / PostgreSQL│
                  └──────────────────┘
```

All links are **gRPC over TLS**. Agents connect outbound to the server and to their relay parent, so a managed host needs **no inbound SSH or WinRM**. Because any agent can be a relay parent, agents do accept inbound gRPC from their children on `50052/tcp` within the fleet (see the [ports matrix](../how-to/install-packages.md#ports-and-connectivity)). Only a bounded number of zone leaders connect directly to the server.

## Components

| Component | Language | Description |
|-----------|----------|-------------|
| `dirq-server` | Go | Central server: gRPC, REST API, query engine, Ansible inventory. SQLite by default; PostgreSQL optional. |
| `dirq-agent` | Go | Endpoint agent: collects data, relays queries, optionally executes commands. Single static binary. |
| `dirq` | Go | CLI: submit queries, manage hosts/tags/tokens, run ad-hoc commands, generate and rotate certificates. |
| `atgreen.dirq` | Python | Ansible collection: inventory plugin + connection plugin for AAP. |

## Scaling the mesh

The server holds a fixed number of zone leader connections (default 5). All other agents fill a tree below those zone leaders, growing as deep as needed (BFS fill order).

| Fleet size | Tree depth | Server connections |
|-----------|-----------|-------------------|
| 250 | 2 | 5 |
| 12,500 | 3 | 5 |
| 625,000 | 4 | 5 |

The server always holds exactly `DIRQ_MAX_ZONE_LEADERS` connections regardless of fleet size. The tree deepens — it never widens at the server.

The live mesh shape is held **in memory** by the server (`MeshTopology`, RWMutex-protected maps for nodes, ZLs, parent/child links, depth cache). Registration, fan-out, and dispatch all read this directly — no DB round-trips on hot paths. `agents.role` and `agents.parent_id` are best-effort snapshots persisted every 30 s for operator visibility and rehydrated on restart. The CLI overlays the in-memory view onto DB records before serializing, so `dirq hosts list` always reflects live truth.

Registration arrivals flow through a **burst-aware batcher** (default 200 ms window, 200 max batch). On flush, the assigner prefers one zone leader per distinct source IP — so a thundering herd from a single subnet can't fill all ZL slots from one host, and a two-pass greedy additionally spreads zone leaders across distinct *failure domains* (subnets) before filling remaining slots. There is no proactive rebalancer; reactive recovery (`reassignOrphans` on stream close, fallback parents + orphan promotion via `RequestPeers`) handles every churn case the old proactive paths used to.

## Related

- [Reboot-aware placement](reboot-aware-placement.md) — how flaky hosts are kept near the leaves
- [Completion reporting & aggregation](completion-and-aggregation.md) — how broadcasts finish and results roll up
- [Redundant parents & recovery](redundant-parents.md) — failover and orphan reassignment
