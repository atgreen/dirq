# Run ad-hoc fleet commands

For quick ad-hoc tasks that don't need a full Ansible playbook, `dirq exec` runs a command or script across matching hosts in parallel and streams results back in real time.

!!! note "Enabling exec and auditing it"
    Exec is disabled by default and must be opted into per agent, and every operation is logged with AAP job attribution and governed by OPA policy. See [Agent exec policy](../how-to/agent-exec-policy.md) for enabling exec on agents, the audit log, and policy configuration.

## Commands

```bash
dirq exec -- uptime
dirq exec WHERE tag.env = 'prod' -- openssl version
dirq exec --become WHERE tag.role = 'webserver' -- systemctl restart nginx
dirq exec -- hostname -f
dirq exec --json -- df -h /
```

## Scripts

Upload and execute a local script file with `--script`. Linux scripts honor their shebang. Windows `.ps1` files run with PowerShell.

```bash
dirq exec WHERE tag.env = 'prod' --script ./health-check.sh
dirq exec WHERE os_info.os = 'windows' --script ./audit.ps1
dirq exec WHERE tag.role = 'webserver' --become --script ./patch.sh
```

With `--script`, no `--` separator is needed since the script path is a dirq flag, not a remote command.

## Fleet grep

Search log files across the fleet without a centralized logging stack. Uses `grep` on Linux and `Select-String` on Windows.

```bash
dirq grep "Out of memory" /var/log/messages
dirq grep -i "error|timeout" /var/log/nginx/error.log WHERE tag.env = 'prod'
dirq grep "FATAL" /var/log/app.log --tail 1000
dirq grep "Failed password" /var/log/secure --become
```

Results are formatted as a table with matches grouped by host:

```
HOST                 LINE  MATCH
web-prod-01          4821  Jan 15 03:22:41 kernel: Out of memory: Killed process 1234 (java)
web-prod-01          6103  Jan 15 08:14:02 kernel: Out of memory: Killed process 5678 (python3)
db-prod-02          11042  Jan 14 22:01:18 kernel: Out of memory: Killed process 891 (mysqld)

3 matches across 2 hosts (15 hosts searched)
```

Use `--tail N` to search only the last N lines of a file (avoids scanning multi-GB logs). Use `--become` for files that require root access (e.g. `/var/log/secure`).

## Streaming output

Results stream back as each host responds — fastest hosts appear first:

```
Targets: 3

── web-01  rc=0 ──
   14:23:01 up 42 days,  3:17,  0 users,  load average: 0.12, 0.08, 0.05

── db-01  rc=0 ──
   14:23:01 up 91 days, 12:44,  0 users,  load average: 0.45, 0.38, 0.31

── web-02  rc=0 ──
   14:23:02 up 13 days,  7:02,  0 users,  load average: 0.03, 0.05, 0.01

3/3 completed
```

With `--json`, output is NDJSON (one JSON object per line), suitable for piping.

## How execution routing works

The relay mesh doubles as an Ansible connection transport. The inventory plugin
automatically sets `ansible_connection` for exec-enabled hosts, so **existing
playbooks work without modification** — no need to add `connection: dirq` or
`gather_facts: false`.

```yaml
# This just works — no connection: dirq needed.
# The inventory plugin handles it.
- hosts: tag_env_prod
  tasks:
    - command: uptime
    - copy:
        src: app.conf
        dest: /etc/myapp/app.conf
    - fetch:
        src: /var/log/status.log
        dest: /tmp/status.log
        flat: yes
```

The inventory plugin also maps DirQ facts to standard Ansible variables
(`ansible_os_family`, `ansible_distribution`, `ansible_architecture`,
`ansible_processor_vcpus`, `ansible_memtotal_mb`, etc.) and sets OS-specific
shell and interpreter settings (`ansible_shell_type`, `ansible_python_interpreter`
for Linux, `powershell` for Windows). Most existing roles work without changes.

1. AAP launches a job template — the inventory already set `ansible_connection`
2. The connection plugin routes `exec_command` / `put_file` / `fetch_file` to the DirQ server REST API
3. The server pushes through the relay mesh to the target agent
4. The agent executes locally and returns results back through the mesh
5. AAP records the job result normally
