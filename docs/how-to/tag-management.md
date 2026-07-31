# Manage tags

Tag hosts individually or in bulk to drive [inventory groups](../reference/query-dsl.md) and query targeting.

```bash
# Tag a single host by ID
dirq hosts tag <agent-id> env=prod role=webserver dc=us-east

# Tag multiple hosts with a WHERE clause
dirq hosts tag env=prod WHERE os_info.os = 'linux'
dirq hosts tag role=webserver WHERE tag.dc = 'us-east'

# Untag by ID or query
dirq hosts untag <agent-id> role dc
dirq hosts untag env WHERE tag.env = 'staging'
```

Tags flow into inventory groups automatically.
