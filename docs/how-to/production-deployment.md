# Deploy a production fleet

The [podman quick start](../tutorials/quick-start.md) is a single-host laptop
convenience. This page is the ordered checklist for a real, multi-host fleet. It
assumes you install from packages — see [Install DirQ from packages](install-packages.md)
for the commands — and focuses on the four things a production deployment must get
right that the quick start does not. Getting the first two wrong leaves agents
stuck re-registering with the mesh unable to route between hosts.

## 0. Prerequisites

- A control host with a **routable** address for `dirq-server` (see step 1).
- **PostgreSQL** if you're running more than a trial or want [HA](../explanation/high-availability.md);
  SQLite is fine for a single small server but can't back multiple pods.
- Firewall rules for the [ports matrix](install-packages.md#ports-and-connectivity):
  `50051/tcp` agents→server, `50052/tcp` host-to-host between agents, `8080/tcp`
  admin→server, `5432/tcp` server→Postgres if external.

## 1. The server must observe each agent's real, routable IP

During registration the server records the **source IP** of the agent's gRPC
connection and advertises it to the rest of the mesh as that agent's relay
address (so other agents know where to attach). If the server runs **behind NAT** —
most commonly a `podman`/`docker` container with **published ports**
(`-p 50051:50051`) — it sees a bridge address (`10.89.0.x`) instead of the agent's
host IP and hands that unroutable address to everyone. The symptom is `dial tcp
10.89.0.x:50052: i/o timeout` in agent logs and `dirq debug ping` timing out even
though `dirq hosts list` shows the agent "online" (it registered, but never
actually attached to its parent — a *ghost-online* node).

Run the server so it sees real client IPs:

- **Native (recommended).** Install the `dirq-server` package and run it as a
  systemd service on a host with a routable address. This is what the RPM/DEB
  packaging targets.
- **Containerized.** Give the container **host networking** (`network_mode: host`
  in compose, or `--network=host`) so it shares the host's network namespace. Do
  **not** publish the gRPC port with `-p` — that is what masks the source IP. With
  host networking, point `DIRQ_DB_URL` at the host (`@127.0.0.1:5432`, not a
  compose service name) and bind the HTTP/gRPC listeners directly (`DIRQ_HTTP_ADDR`,
  `DIRQ_GRPC_ADDR`).

## 2. Persist server state across restarts

The server's Ed25519 **signing key**, **CA**, and **bootstrap token** live in
`/var/lib/dirq`. If that directory is ephemeral (a container with no volume),
recreating or rebuilding the server **regenerates the signing key**, and every
already-registered agent then rejects the server's signed messages until you
re-distribute the new `agent.conf`. Mount `/var/lib/dirq` on a persistent volume,
and persist the Postgres data directory too. After any signing-key change,
re-copy the freshly generated `agent.conf` to the agents.

## 3. Enable TLS and authentication

In production both TLS and authentication must be enabled — with them off, API
tokens and remote-exec payloads cross the network in cleartext, and any host that
can reach the gRPC port can register or run commands. Set a **registration
secret** so only hosts that present it can join. See
[Enable TLS & authentication](tls-and-auth.md).

## 4. Install and verify agents

Install the `dirq-agent` package (native systemd service) and drop in the
server-generated config — see [Install agents](install-packages.md#5-install-agents).
Prefer the packaged unit over a hand-rolled one so config paths, the data
directory, and restart behavior match the docs.

Then confirm each agent actually attached, not just registered:

```bash
dirq hosts list                 # host shows online
dirq debug ping <hostname>      # end-to-end proof it's reachable through the mesh
```

If `hosts list` shows online but `ping` times out, you have a ghost-online node —
revisit step 1. See [Diagnose the mesh](diagnostics.md) for the full symptom table.

## Scaling and HA

- Tune fan-out and placement with `max_children`, `max_zone_leaders`, and the
  [reboot-aware placement](../explanation/reboot-aware-placement.md) knobs.
- Run multiple server pods behind Postgres with `leader_election` for
  [high availability](../explanation/high-availability.md).
