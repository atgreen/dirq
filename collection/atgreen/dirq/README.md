# Ansible Collection: atgreen.dirq

DirQ collection for Ansible Automation Platform. Provides:

- **`atgreen.dirq.dirq` connection plugin** — routes playbook execution through the DirQ P2P relay mesh instead of SSH/WinRM
- **`atgreen.dirq.dirq` inventory plugin** — uses DirQ as a dynamic inventory source with live host facts and automatic group hierarchy

## Installation

```bash
# From Galaxy / Automation Hub
ansible-galaxy collection install atgreen.dirq

# From the repo
cd collection/atgreen/dirq
ansible-galaxy collection build
ansible-galaxy collection install atgreen-dirq-1.0.0.tar.gz
```

## Inventory Plugin

Create an inventory source file:

```yaml
# dirq-inventory.yml
plugin: atgreen.dirq.dirq
server_url: http://dirq-server:8080
token: my-api-token
```

Use with Ansible:

```bash
ansible-inventory -i dirq-inventory.yml --graph
ansible-playbook -i dirq-inventory.yml site.yml
```

In AAP, add this as an Inventory Source with source type "Sourced from a Project."

## Connection Plugin

```yaml
- hosts: tag_env_prod
  connection: atgreen.dirq.dirq
  vars:
    dirq_server_url: http://dirq-server:8080
    dirq_token: "{{ lookup('env', 'DIRQ_TOKEN') }}"
  tasks:
    - command: uptime
```

## AAP Setup

### 1. Custom Credential Type

Create a credential type in AAP:

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

### 2. Custom Execution Environment

Build a custom EE that includes this collection:

```yaml
# execution-environment.yml
version: 3
dependencies:
  galaxy:
    collections:
      - name: atgreen.dirq
        version: ">=1.0.0"
```

Build with `ansible-builder`:

```bash
ansible-builder build -t dirq-ee:latest -f execution-environment.yml
```

### 3. Configure in AAP

1. Add the custom EE image to AAP
2. Create a DirQ credential with server URL and token
3. Create an inventory using the `atgreen.dirq.dirq` inventory source
4. Create job templates with `connection: atgreen.dirq.dirq` and attach the DirQ credential

## License

MIT License. Copyright (c) 2026 Anthony Green.
