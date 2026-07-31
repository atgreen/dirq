# Build query-based Ansible inventories

The inventory plugin accepts an optional `query` parameter. Only hosts matching the query appear in the inventory:

```yaml
# inventories/vulnerable-openssl.yml
plugin: atgreen.dirq.dirq
server_url: http://dirq-server:8080
query: "SELECT os_info.hostname WHERE packages.name = 'openssl' AND packages.version LIKE '1.%'"

# inventories/disks-full.yml
plugin: atgreen.dirq.dirq
server_url: http://dirq-server:8080
query: "SELECT os_info.hostname WHERE disk.pct_used > 90"
```

In AAP, each file becomes an Inventory Source. Job templates pair each inventory with a remediation playbook:

| Job Template | Inventory Source | Playbook | Targets |
|---|---|---|---|
| Patch OpenSSL | vulnerable-openssl.yml | update-openssl.yml | Hosts with OpenSSL 1.x |
| Fix Full Disks | disks-full.yml | cleanup-disks.yml | Hosts over 90% disk |

The query runs in real time during inventory sync — the host list is always current.

**Standalone:**
```bash
DIRQ_QUERY="SELECT os_info.hostname WHERE disk.pct_used > 90" \
  ansible-playbook -i ansible/dirq_inventory.py cleanup-disks.yml
```

For the full inventory reference (inventory groups, host vars, fact mapping), see [Ansible integration](../reference/ansible.md). For query syntax, see the [query DSL](../reference/query-dsl.md).
