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

# All online hosts (no WHERE clause)
dirq run deploy.yml
```
