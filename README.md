# DirQ — Direct Query Platform for Fleet Management & Ansible Execution

DirQ ("Direct Query") is an agent-based platform for querying and managing large Windows/Linux fleets. Agents form a peer-to-peer relay mesh and report data back to a central server. The server acts as an Ansible Automation Platform (AAP) inventory source, exposes collected data as structured facts, and can route Ansible execution through the mesh as an alternative to SSH/WinRM connectivity.

- **Query the fleet like a dataset** instead of logging into hosts one by one
- **Keep managed hosts outbound-only** instead of opening SSH/WinRM inbound
- **Reuse Ansible** while replacing the transport underneath
- **Build Ansible inventories from live DirQ query results** instead of static host lists
- **Scale with a relay tree** so the server never needs a direct session to every node
- **Scan for CVEs in real time** — identify every affected host in seconds
- **Run ad-hoc commands across the fleet** — parallel exec with streaming results

## 📖 Documentation

**Full documentation lives at [atgreen.github.io/dirq](https://atgreen.github.io/dirq/).**

The docs follow the [Diátaxis](https://diataxis.fr/) framework:

| | |
|---|---|
| 📚 **[Tutorial](https://atgreen.github.io/dirq/tutorials/quick-start/)** | Get a mesh running on your laptop in five minutes |
| 🔧 **[How-to guides](https://atgreen.github.io/dirq/how-to/production-deployment/)** | Deploy a fleet, build inventories, run playbooks, scan for CVEs, set up MCP/AAP, and more |
| 📖 **[Reference](https://atgreen.github.io/dirq/reference/query-dsl/)** | Query DSL, configuration keys, REST API, CLI, metrics |
| 💡 **[Explanation](https://atgreen.github.io/dirq/explanation/why-dirq/)** | Why DirQ exists; how the mesh, reboot-aware placement, and recovery work |

## Install the agent

DirQ publishes signed RPM and DEB packages — see the
[install instructions](https://atgreen.github.io/dirq/#install-the-agent). In short:

```bash
# Fedora / RHEL / AlmaLinux
sudo tee /etc/yum.repos.d/dirq.repo <<'EOF'
[dirq]
name=DirQ
baseurl=https://atgreen.github.io/dirq/rpm-repo
gpgcheck=1
gpgkey=https://atgreen.github.io/dirq/rpm-repo/RPM-GPG-KEY-dirq
enabled=1
EOF
sudo dnf install dirq-agent
```

## Building from source

```bash
# All binaries
go build -o bin/dirq-server ./cmd/dirq-server
go build -o bin/dirq-agent  ./cmd/dirq-agent
go build -o bin/dirq         ./cmd/dirq

# Windows agent
GOOS=windows GOARCH=amd64 go build -o bin/dirq-agent.exe ./cmd/dirq-agent

# Tests
go test ./...

# Container images
podman build --target server -t dirq-server .
podman build --target agent  -t dirq-agent .
```

### Building the documentation

The docs are [MkDocs](https://www.mkdocs.org/) with the Material theme. Sources are in `docs/`:

```bash
pip install mkdocs-material
mkdocs serve     # live preview at http://127.0.0.1:8000
mkdocs build     # render to site/
```

## Project structure

```
cmd/
  dirq-server/            Server entrypoint
  dirq-agent/             Agent entrypoint (Windows Service support)
  dirq/                   CLI entrypoint
proto/dirq/v1/            Protobuf definitions
internal/
  server/                 gRPC, REST API, query dispatch, exec routing
  agent/                  Registration, relay mesh, query execution, exec
  query/                  DirQ DSL parser and evaluator
  modules/                System data collectors
  db/                     SQLite + PostgreSQL backends and data access
  tlsutil/                TLS configuration, cert generation
  signutil/               Message signing (Ed25519)
collection/atgreen/dirq/  Ansible collection for AAP
ansible/                  Standalone plugins for CLI Ansible
docs/                     Documentation sources (MkDocs)
Containerfile             Multi-stage build
```

## Security

To report a vulnerability, see [SECURITY.md](SECURITY.md). For the security
*model* (mTLS, message signing, agent-side policy), see the
[Security model](https://atgreen.github.io/dirq/explanation/security/) docs.

## License

MIT License. Copyright (c) 2026 Anthony Green. See [LICENSE](LICENSE) for details.
