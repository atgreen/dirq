# Ansible Collection: atgreen.dirq

DirQ collection for Ansible Automation Platform. Provides:

- **`atgreen.dirq.dirq` inventory plugin** — uses DirQ as a dynamic inventory source with live host facts, automatic group hierarchy, and auto-set `ansible_connection`
- **`atgreen.dirq.dirq` connection plugin** — routes playbook execution through the DirQ P2P relay mesh instead of SSH/WinRM
- **`atgreen.dirq.dirq` fact cache plugin** — serves DirQ-collected facts instantly, making `gather_facts: true` near-instant

## Installation

```bash
# From Galaxy / Automation Hub
ansible-galaxy collection install atgreen.dirq

# From the repo
cd collection/atgreen/dirq
ansible-galaxy collection build
ansible-galaxy collection install atgreen-dirq-1.0.0.tar.gz
```

## Quick Start

The inventory plugin auto-sets `ansible_connection` for exec-enabled hosts, so existing playbooks work without modification:

```yaml
# dirq-inventory.yml
plugin: atgreen.dirq.dirq
server_url: http://dirq-server:8080
```

```bash
export DIRQ_TOKEN=<your-token>
ansible-playbook -i dirq-inventory.yml site.yml
```

No `connection: dirq` needed in your playbooks — the inventory plugin handles it.

## Inventory Plugin

Create an inventory source file:

```yaml
# dirq-inventory.yml
plugin: atgreen.dirq.dirq
server_url: http://dirq-server:8080
```

### Options

| Option | Env var | Default | Description |
|--------|---------|---------|-------------|
| `server_url` | `DIRQ_SERVER_URL` | *(required)* | DirQ server REST API URL |
| `token` | `DIRQ_TOKEN` | | API token |
| `query` | | | DirQ query to filter hosts |
| `auto_connection` | | `true` | Auto-set `ansible_connection` for exec-enabled hosts |

### Query-filtered inventories

Only include hosts matching a DirQ query:

```yaml
# Only hosts with vulnerable OpenSSL
plugin: atgreen.dirq.dirq
server_url: http://dirq-server:8080
query: "SELECT os_info.hostname WHERE packages.name = 'openssl' AND packages.version LIKE '1.%'"
```

### Auto-generated groups

The plugin creates groups from agent metadata and tags:

```
@os_linux, @os_windows
@arch_amd64, @arch_arm64
@exec_enabled
@tag_env_prod, @tag_env_staging
@tag_role_webserver, @tag_role_database
```

### Mapped Ansible variables

DirQ facts are mapped to standard Ansible variables automatically:

- `ansible_os_family`, `ansible_distribution`, `ansible_architecture`
- `ansible_processor_vcpus`, `ansible_processor_cores`, `ansible_memtotal_mb`
- `ansible_hostname`, `ansible_fqdn`
- `ansible_shell_type`, `ansible_python_interpreter`, `ansible_become_method`

## Connection Plugin

The connection plugin routes `exec_command`, `put_file`, and `fetch_file` through the DirQ server and relay mesh. It is normally set automatically by the inventory plugin — you only need to configure it manually if you're not using the DirQ inventory:

```yaml
- hosts: all
  connection: atgreen.dirq.dirq
  vars:
    dirq_server_url: http://dirq-server:8080
  tasks:
    - command: uptime
```

### Options

| Option | Env var | Default | Description |
|--------|---------|---------|-------------|
| `dirq_server_url` | `DIRQ_SERVER_URL` | `http://localhost:8080` | DirQ server URL |
| `dirq_token` | `DIRQ_TOKEN` | | API token |
| `dirq_exec_timeout` | | `300` | Exec timeout (seconds) |
| `dirq_file_timeout` | | `300` | File transfer timeout (seconds) |

### TLS

For servers with self-signed certificates, set `DIRQ_TLS_INSECURE=true` in the environment. This applies to both the connection and inventory plugins.

## Fact Cache Plugin

The fact cache serves DirQ-collected facts instead of running the `setup` module on each host. This makes `gather_facts: true` near-instant — facts come from the DirQ server's cache, not from SSH/exec to every host.

### Setup

```ini
# ansible.cfg
[defaults]
fact_caching = atgreen.dirq.dirq
fact_caching_connection = http://dirq-server:8080
```

Or set via environment:

```bash
export DIRQ_SERVER_URL=http://dirq-server:8080
export DIRQ_TOKEN=<your-token>
```

### What it provides

The cache maps DirQ agent data to standard Ansible facts:

- **CPU**: `ansible_processor_vcpus`, `ansible_processor_cores`, `ansible_processor`
- **Memory**: `ansible_memtotal_mb`, `ansible_memfree_mb`, `ansible_swaptotal_mb`
- **OS**: `ansible_kernel`, `ansible_uptime_seconds`, `ansible_distribution_version`
- **Network**: `ansible_interfaces`, `ansible_default_ipv4`, `ansible_all_ipv4_addresses`
- **Packages**: `ansible_facts.packages` (same format as `package_facts`)
- **Services**: `ansible_facts.services` (same format as `service_facts`)

## AAP Setup

### 1. Custom Credential Type

Import from `docs/aap-credential-type.yml` or create manually in AAP:

**Name:** DirQ Credential

**Input Configuration:**
```yaml
fields:
  - id: dirq_server_url
    type: string
    label: DirQ Server URL
  - id: dirq_token
    type: string
    label: DirQ API Token
    secret: true
required:
  - dirq_server_url
  - dirq_token
```

**Injector Configuration:**
```yaml
env:
  DIRQ_SERVER_URL: "{{ dirq_server_url }}"
  DIRQ_TOKEN: "{{ dirq_token }}"
```

### 2. Execution Environment

```yaml
# execution-environment.yml
version: 3
dependencies:
  galaxy:
    collections:
      - name: atgreen.dirq
        version: ">=1.0.0"
```

```bash
ansible-builder build -t dirq-ee:latest -f execution-environment.yml
```

### 3. Configure in AAP

1. Add the custom EE image to AAP
2. Create a DirQ credential with server URL and token
3. Create an inventory using the `atgreen.dirq.dirq` inventory source
4. Create job templates — `ansible_connection` is set automatically by the inventory plugin, so no connection override is needed in templates

## License

MIT License. Copyright (c) 2026 Anthony Green.
