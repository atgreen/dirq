# Diagnose the mesh

Diagnostic recipes for inspecting topology, checking deployment health, and tracking down problems in the agent mesh.

## Topology graph

Visualize the agent mesh tree:

```bash
dirq hosts graph
```

```
dirq-server
├── ● dirq-agent-01 [ZL]
│   ├── ● dirq-agent-06
│   └── ● dirq-agent-08
├── ● dirq-agent-02 [ZL]
│   └── ● dirq-agent-07
└── ● dirq-agent-03 [ZL]
    └── ● dirq-agent-09
```

`●` = online, `○` = offline, `[ZL]` = zone leader.

Export to Graphviz DOT format for rendering (left-to-right layout fits large fleet trees on screen):

```bash
dirq hosts graph --dot | dot -Tpng -o topology.png
```

## Deployment health

Check the health of your DirQ deployment with `dirq doctor`:

```bash
dirq doctor
```

```
  DIRQ_SERVER_URL               ok   https://dirq.example.com:8080
  API token valid                ok   authenticated
  TLS certificate                ok   valid
  Database                       ok   postgres connected
  Agents online                  ok   1247/1250
  Agent version skew             !!   3 agents on v0.21.x (server is v0.22.3)
  Relay tree                     ok   depth 4, 5 zone leader(s)
  Ansible installed              ok   ansible-playbook [core 2.20.5]
  Connection plugin              ok   /usr/local/ansible/connection_plugins

  9 passed, 1 warnings, 0 failed
```

## Debug subcommands

`dirq debug` covers diagnostic tools used when something looks wrong in the mesh. All endpoints are admin-scoped.

| Command | Purpose |
|---|---|
| `dirq debug inflight` | List every exec / query / deploy session the server is currently coordinating, with the still-missing agent set, arrivals-in-the-last-1/5/30 s, and a per-zone-leader breakdown (`subtree`, `pending`, `send_buf`). Marks the chokepoint ZL with `← bottleneck (send_buf full)` when its stream-send buffer is at capacity. |
| `dirq debug path <hostname>` | Walk the agent's mesh parent chain from the DB snapshot. Flags broken links. Fastest, DB-only. |
| `dirq debug stream <hostname>` | Show the server's in-memory view of how it would currently reach this agent (directly connected vs. routed through a zone leader). |
| `dirq debug ping <hostname>` | Send a no-op exec through the mesh and report round-trip timing. Slowest of the three lookup tools but the only one that proves a message actually reaches the agent right now. |

The three lookup tools form a hierarchy of trust — `path` (DB), then `stream` (live process state), then `ping` (end-to-end proof).

## Common symptoms

| Symptom | Likely cause | Fix |
|---|---|---|
| Agents show **online** in `dirq hosts list` but `dirq debug ping` times out; agent logs loop on `dial tcp 10.89.0.x:50052: i/o timeout` | The server is advertising an unroutable relay address — it observed a NAT/bridge source IP at registration (typically the server running in a container with **published ports**). The agent registered but never attached to its parent (*ghost-online*). | Run the server so it sees real agent IPs (native or host networking, no `-p` on the gRPC port) and open `50052/tcp` host-to-host. See [Production Deployment](../how-to/production-deployment.md). |
| Exec / query / ping to agents start timing out **after a server restart**; agent logs show `rejected unsigned or invalid server message` | The server's signing key changed — an ephemeral `/var/lib/dirq` regenerated it on restart while agents still trust the old key. | Persist `/var/lib/dirq`; re-distribute the regenerated `agent.conf` and restart the affected agents. See [Production Deployment](../how-to/production-deployment.md). |
| Registration never succeeds: `tls: first record does not look like a TLS handshake` | TLS mode mismatch — one side speaks TLS, the other plaintext. `DIRQ_TLS_INSECURE` skips cert verification but **still uses TLS**; `DIRQ_TLS_DISABLED` turns TLS off entirely. | Make the mode identical on the server and every agent. |
