# DirQ Demos

Interactive demos showing DirQ's key capabilities. Each demo is a
self-guided walkthrough with pauses between steps.

## Setup

```bash
# Build images and launch the demo fleet (12 agents with different roles)
./demo/setup.sh

# Rebuild the CLI
go build -o bin/dirq ./cmd/dirq
```

The fleet includes:
- 3 production web servers (nginx installed)
- 2 production databases (postgresql installed)
- 2 staging servers (with "vulnerable" packages)
- 3 dev boxes (one bare, one with full disk, one healthy)
- 2 production app servers (healthy)

## Demos

| Demo | Script | Shows |
|------|--------|-------|
| 1. Fleet Overview | `./demo/demo-1-fleet-overview.sh` | Query CPU, memory, services across all hosts |
| 2. Security Audit | `./demo/demo-2-security-audit.sh` | Find vulnerable packages, missing software |
| 3. Query & Remediate | `./demo/demo-3-query-and-remediate.sh` | Find a problem → fix it → verify |
| 4. Targeted Exec | `./demo/demo-4-targeted-exec.sh` | Run commands against precise host sets |
| 5. Ansible Playbook | `./demo/demo-5-ansible-playbook.sh` | Run a playbook through the mesh |

## Teardown

```bash
./demo/setup.sh teardown
```
