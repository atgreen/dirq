# Run playbooks through the mesh

Query the fleet and run Ansible against the results in one step:

```bash
# Run a playbook against hosts matching a WHERE clause
dirq run cleanup-disks.yml WHERE disk.pct_used = 90

# Quoted form
dirq "run deploy.yml where tag.env = 'prod'"

# Ad-hoc command
dirq run --command "yum update -y openssl" WHERE packages.name = 'openssl'

# Ansible module
dirq run --module ping WHERE os_info.os = 'linux'

# All exec-enabled hosts (no WHERE clause)
dirq run deploy.yml
```

## Which hosts are targeted

Ansible needs to execute on the target, so `dirq run` only ever targets
agents with exec enabled. A query matches on tags and facts and knows
nothing about capability, so any matching host without exec is skipped
and named:

```
Skipping 1 host(s) with exec disabled: locked-01
Query matched 2 host(s): web-01, web-02
```

Each targeted Linux host also needs Python 3.8 or newer. `dirq run`
detects the interpreter across the fleet before handing over to Ansible,
and stops with the list of hosts that lack one rather than letting the
play fail partway through. Set an `ansible_python_interpreter` tag on a
host to override the detected path.
