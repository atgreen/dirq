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

The server auto-generates TLS certs, runs DB migrations, and creates a bootstrap API token. The token is written to a file (not logged) for security:

```bash
# The server log shows the token file path:
podman logs dirq_dirq-server_1 2>&1 | grep "bootstrap"
# Read the token:
cat /var/lib/dirq/bootstrap-token
```

Keep that token handy — you will use it to authenticate the CLI in a moment.

## Step 2: Deploy an agent

The server writes ready-to-copy config files on startup — `/var/lib/dirq/agent.conf` for agents and `/var/lib/dirq/client.conf` for the CLI. For this laptop run, just build and start the agent directly:

```bash
go build -o bin/dirq-agent ./cmd/dirq-agent
./bin/dirq-agent
```

Because the agent and server share the same machine, they also share the auto-generated CA and verify each other automatically. (On real hosts you copy `agent.conf` to each machine instead — that's what [Install DirQ from packages](../how-to/install-packages.md) walks through.)

## Step 3: Build and use the CLI

Build the `dirq` binary:

```bash
go build -o bin/dirq ./cmd/dirq
```

The CLI reads config from `~/.config/dirq/client.conf` (user-local) or `/etc/dirq/client.conf` (system-wide). Copy the server-generated `client.conf`:

```bash
# Copy from server to your workstation:
scp server:/var/lib/dirq/client.conf ~/.config/dirq/client.conf

# Now just use dirq — no env vars needed:
dirq doctor
dirq hosts list
dirq select hostname, cpu.logical_cores, memory.pct_used
```

Or set env vars directly:

```bash
export DIRQ_SERVER_URL=https://dirq-server:8080
export DIRQ_TOKEN=<bootstrap-token>
export DIRQ_TLS_INSECURE=true  # for self-signed certs
```

If `dirq doctor` reports healthy and your agent shows up in `dirq hosts list`, your mesh is live.

## Step 4: Test with Ansible

Now put the mesh to work by running a playbook:

```bash
cd test-playbook
DIRQ_SERVER_URL=http://localhost:8090 DIRQ_TOKEN=$DIRQ_TOKEN ansible-playbook test.yml -v
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
