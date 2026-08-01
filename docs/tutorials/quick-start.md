# Quick start: a mesh on your laptop

In this tutorial you will stand up a complete DirQ mesh — server, database, an agent, and the CLI — on a single machine using Podman. By the end you will have a working control plane you can query, and you will have run a playbook against your first agent. Follow the steps in order; each one builds on the last.

This runs everything on one machine so you can learn DirQ quickly. When you're ready for a real multi-host fleet, follow [Install DirQ from packages](../how-to/install-packages.md).

## Prerequisites

Before you begin, make sure you have:

- Go 1.26+
- Podman and podman-compose

## Step 1: Start the server and database

Bring up the stack with a single command:

```bash
podman-compose up -d
```

This starts PostgreSQL and the DirQ server and runs the DB migrations. For convenience the dev stack runs with **TLS and authentication disabled** (`DIRQ_TLS_DISABLED` and `DIRQ_AUTH_DISABLED` in `podman-compose.yml`), so there's no token to copy and no certs to manage — you can talk to the server right away. The REST API is on `http://localhost:8090` and the agent gRPC port on `localhost:50051`.

Check it started cleanly:

```bash
podman logs dirq_dirq-server_1 2>&1 | tail
```

## Step 2: Deploy an agent

Build and start an agent on the same machine. Since the dev server has TLS disabled, tell the agent the same so their connection modes match:

```bash
go build -o bin/dirq-agent ./cmd/dirq-agent
DIRQ_TLS_DISABLED=true ./bin/dirq-agent
```

It connects to the server at `localhost:50051`, registers, and joins the mesh. (On real hosts you install the agent package and drop in a server-generated `agent.conf` instead — that's what [Install DirQ from packages](../how-to/install-packages.md) walks through.)

## Step 3: Build and use the CLI

Build the `dirq` binary and point it at the server. With auth disabled you don't need a token, and the REST API is plain HTTP:

```bash
go build -o bin/dirq ./cmd/dirq

export DIRQ_SERVER_URL=http://localhost:8090
dirq doctor
dirq hosts list
dirq select hostname, cpu.logical_cores, memory.pct_used
```

If `dirq doctor` reports healthy and your agent shows up in `dirq hosts list`, your mesh is live.

(On a real deployment the server generates a ready-to-copy `client.conf` with the server URL and an API token — see [Configure the CLI](../how-to/install-packages.md#4-configure-the-cli).)

## Step 4: Test with Ansible

Now put the mesh to work by running a playbook:

```bash
cd test-playbook
DIRQ_SERVER_URL=http://localhost:8090 ansible-playbook test.yml -v
```

## Installing on real hosts

The steps above build from source, which is ideal for this laptop walkthrough.
For real hosts you don't build anything — DirQ ships signed `dirq-server`,
`dirq`, and `dirq-agent` packages (plus a Windows MSI). Follow
[Install DirQ from packages](../how-to/install-packages.md) for the full path:
add the repo, install and start the server, distribute the generated
`agent.conf`, and verify the fleet.

## Windows agent from source

If you'd rather build it yourself instead of using the MSI, cross-compile and run
the agent on the Windows host:

```powershell
GOOS=windows GOARCH=amd64 go build -o bin/dirq-agent.exe ./cmd/dirq-agent

# Run in foreground
.\bin\dirq-agent.exe

# Or install as a Windows Service (runs as SYSTEM)
.\bin\dirq-agent.exe install
sc start DirQAgent
```

You now have a working DirQ mesh on your laptop: a server, a registered agent, a configured CLI, and a playbook run under your belt. When you are ready to graduate to a real, multi-host fleet, continue with [Production Deployment](../how-to/production-deployment.md).
