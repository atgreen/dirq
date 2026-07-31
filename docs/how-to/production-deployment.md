# Deploy a production fleet

The [podman quick start](../tutorials/quick-start.md) is a single-host laptop convenience. A real multi-host fleet has two hard requirements it does not meet — getting either wrong leaves agents stuck re-registering with the mesh unable to route between hosts.

## 1. The server must observe each agent's real, routable IP

During registration the server records the **source IP** of the agent's gRPC connection and advertises it to the rest of the mesh as that agent's relay address (so other agents know where to attach). If the server runs **behind NAT** — most commonly a `podman`/`docker` container with **published ports** (`-p 50051:50051`) — it sees a bridge address (`10.89.0.x`) instead of the agent's host IP and hands that unroutable address to everyone. The symptom is `dial tcp 10.89.0.x:50052: i/o timeout` in agent logs and `dirq debug ping` timing out even though `dirq hosts list` shows the agent "online" (it registered, but never actually attached to its parent — a *ghost-online* node).

Run the server so it sees real client IPs:

- **Native (recommended).** Install the `dirq-server` package and run it as a systemd service on a host with a routable address. This is what the RPM/DEB packaging targets.
- **Containerized.** Give the container **host networking** (`network_mode: host` in compose, or `--network=host`) so it shares the host's network namespace. Do **not** publish the gRPC port with `-p` — that is what masks the source IP. With host networking, point `DIRQ_DB_URL` at the host (`@127.0.0.1:5432`, not a compose service name) and bind the HTTP/gRPC listeners directly (`DIRQ_HTTP_ADDR`, `DIRQ_GRPC_ADDR`).

Either way, open `50052/tcp` **host-to-host** between agents (so they can reach their relay parents) and `50051/tcp` from agents to the server.

## 2. Persist server state across restarts

The server's Ed25519 **signing key**, **CA**, and **bootstrap token** live in `/var/lib/dirq`. If that directory is ephemeral (a container with no volume), recreating or rebuilding the server **regenerates the signing key**, and every already-registered agent then rejects the server's signed messages until you re-distribute the new `agent.conf`. Mount `/var/lib/dirq` on a persistent volume, and persist the Postgres data directory too. After any signing-key change, re-copy the freshly generated `agent.conf` to the agents.

## 3. Agents

Install the `dirq-agent` package (native systemd service) and drop in the server-generated config — see [Deploy agents](../tutorials/quick-start.md). Prefer the packaged unit over a hand-rolled one so config paths, the data directory, and restart behavior match the docs.

## 4. Enable TLS and authentication

In production both TLS and authentication must be enabled — with them off, API tokens and remote-exec payloads cross the network in cleartext. See [Enable TLS & authentication](../how-to/tls-and-auth.md).
