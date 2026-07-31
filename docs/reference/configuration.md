# Configuration

Both the server and agent support configuration via **config files**, **environment variables**, or both. Environment variables always override config file values, which override defaults.

### Config Files

Config files use a simple `key: value` format with optional indented `tags:` block. Comments start with `#`.

**Agent config** — `/etc/dirq/agent.conf` (Linux) or `C:\ProgramData\dirq\agent.conf` (Windows):

```
# DirQ agent configuration
server: grpc.example.com:50051
listen: 0.0.0.0:50052
exec_enabled: true

tags:
  env: prod
  dc: us-east
  role: webserver
```

**Server config** — `/etc/dirq/server.conf` (Linux) or `C:\ProgramData\dirq\server.conf` (Windows):

```
# DirQ server configuration
grpc_addr: :50051
http_addr: :8080
db_url: postgres://dirq:dirq@db.internal:5432/dirq?sslmode=require
max_zone_leaders: 10
max_children: 50
registration_secret: my-fleet-secret

tls_ca: /etc/dirq/certs/ca.crt
tls_cert: /etc/dirq/certs/server.crt
tls_key: /etc/dirq/certs/server.key
```

Override the config file path with `DIRQ_CONFIG`:

```bash
DIRQ_CONFIG=/opt/dirq/custom.conf dirq-agent
```

If the config file doesn't exist, it is silently ignored — all values fall back to environment variables or defaults.

### Config file keys ↔ environment variables

**Priority:** environment variable > config file > default.

#### Server

| Config key | Environment variable | Default | Description |
|-----------|----------|---------|-------------|
| `grpc_addr` | `DIRQ_GRPC_ADDR` | `:50051` | gRPC listen address |
| `http_addr` | `DIRQ_HTTP_ADDR` | `:8080` | REST API listen address |
| `db_url` | `DIRQ_DB_URL` | `sqlite:///var/lib/dirq/dirq.db` | Database URL (SQLite or `postgres://...`) |
| `pod_id` | `DIRQ_POD_ID` | hostname | Unique pod identifier |
| `max_zone_leaders` | `DIRQ_MAX_ZONE_LEADERS` | `5` | Max direct server connections |
| `max_children` | `DIRQ_MAX_CHILDREN` | `50` | Max children per node (fan-out) |
| `flap_window` | `DIRQ_FLAP_WINDOW` | `1h` | Decay half-life of a node's reboot (flap) score; `0` disables decay ([Reboot-Aware Placement](../explanation/reboot-aware-placement.md)) |
| `flap_threshold` | `DIRQ_FLAP_THRESHOLD` | `1.5` | Decayed flap score at which a node goes on probation; `0` disables reboot-aware placement entirely |
| `probation_child_cap` | `DIRQ_PROBATION_CHILD_CAP` | `0` | Max children a probationary node may hold (0 = keep it a leaf) |
| `failure_domain_prefix_v4` | `DIRQ_FAILURE_DOMAIN_PREFIX_V4` | `24` | IPv4 prefix bits used to bucket hosts into failure domains |
| `failure_domain_prefix_v6` | `DIRQ_FAILURE_DOMAIN_PREFIX_V6` | `64` | IPv6 prefix bits used to bucket hosts into failure domains |
| `domain_flap_min_nodes` | `DIRQ_DOMAIN_FLAP_MIN_NODES` | `2` | Flapping members before a failure domain is treated as hot; `0` disables domain correlation |
| `auth_disabled` | `DIRQ_AUTH_DISABLED` | `false` | Disable API auth (not recommended) |
| `require_aap_binding` | `DIRQ_REQUIRE_AAP_BINDING` | `false` | When true, reject write ops whose `aap_user` the token isn't bound to, and forbid unbound tokens from write ops (see [Security](../explanation/security.md)) |
| `registration_secret` | `DIRQ_REGISTRATION_SECRET` | | Pre-shared secret for agent registration (see [Security](../explanation/security.md)) |
| `leader_election` | `DIRQ_LEADER_ELECTION` | `false` | Enable Postgres advisory-lock leader election for multi-pod HA (see [HA.md](../explanation/high-availability.md)) |
| `fact_flush_interval` | `DIRQ_FACT_FLUSH_INTERVAL` | `250ms` | Fact-cache batch flush interval |
| `fact_flush_size` | `DIRQ_FACT_FLUSH_SIZE` | `5000` | Distinct (agent_id, module) keys per flush |
| `fact_stage_cap` | `DIRQ_FACT_STAGE_CAP` | `20000` | Hard cap on staged distinct keys (drops only new keys on saturation) |

#### Agent

| Config key | Environment variable | Default | Description |
|-----------|----------|---------|-------------|
| `server` | `DIRQ_SERVER` | `localhost:50051` | DirQ server gRPC address |
| `listen` | `DIRQ_LISTEN` | `:50052` | Relay listener (always enabled) |
| `exec_enabled` | `DIRQ_EXEC_ENABLED` | `false` | Enable remote execution |
| `registration_secret` | `DIRQ_REGISTRATION_SECRET` | | Must match server's registration secret |
| `tags:` block | `DIRQ_TAGS` | | Tags: `env=prod,dc=us-east` |
| `hostname` | `DIRQ_HOSTNAME` | (autodetected) | Override the hostname the agent reports |
| `virtual_hosts` | `DIRQ_VIRTUAL_HOSTS` | `0` | Spawn N in-process virtual hosts for fleet emulation (Linux only) |
| `hostname_prefix` | `DIRQ_HOSTNAME_PREFIX` | | Prefix for synthesized virtual-host names (`<prefix>-NNNNN`) |
| `registration_jitter_seconds` | `DIRQ_REGISTRATION_JITTER_SECONDS` | (auto for multi-VH) | Cap on random startup delay before first `Register`; smooths thundering-herd boot |
| `policy_file` | `DIRQ_POLICY_FILE` | | Path to a local OPA/Rego policy evaluated before exec/file/deploy side effects (see [Agent-side policy](#agent-side-policy-oparego)) |
| `policy_fail_closed` | `DIRQ_POLICY_FAIL_CLOSED` | `true` when `policy_file` is set | Deny if the policy fails to load or evaluate |
| `policy_query` | `DIRQ_POLICY_QUERY` | `data.dirq.agent.allow` | Rego decision query |

Tags can be set in the config file as an indented block under `tags:`, or via the `DIRQ_TAGS` environment variable as comma-separated `key=value` pairs. Both sources are merged, with environment variables taking precedence for duplicate keys.

#### Agent-side policy (OPA/Rego)

An optional Rego policy lets each agent refuse local operations even when the
server validly authorized them — defense in depth, not a replacement for
server-side authorization. Set `policy_file` and the agent compiles the policy
at startup and evaluates it before every `exec`, `put_file`, `fetch_file`, and
`deploy` side effect. Denied operations return a terminal `policy denied: …`
error and run nothing locally.

```
exec_enabled: true
policy_file: /etc/dirq/policy.rego
policy_fail_closed: true
```

The policy queries `data.dirq.agent.allow` (boolean) and an optional
`data.dirq.agent.reason` (string). Input is a stable, documented JSON document
per operation — never raw file content, script bodies, or environment values
(those are reduced to sizes, SHA-256 hashes, and key names). For example:

```rego
package dirq.agent

default allow := false
default reason := "denied by default"

# Prod hosts: only an approved AAP template may restart nginx.
allow if {
	input.operation == "exec"
	input.tags.env == "prod"
	input.aap_job_template == "restart-nginx"
	input.command == "systemctl restart nginx"
}

# Writes limited to one app's config directory.
allow if {
	input.operation == "put_file"
	startswith(input.dest_path, "/etc/myapp/")
	input.content_size <= 1048576
}
```

Ready-to-adapt examples (minimal allowlist, production AAP-only, file-path
restrictions) ship under [`examples/policy/`](https://github.com/atgreen/dirq/tree/main/examples/policy). With no
`policy_file` configured, agent behavior is unchanged. See
[SECURITY.md](../explanation/security.md) for the full model.

#### TLS (server and agent)

| Config key | Environment variable | Default | Description |
|-----------|----------|---------|-------------|
| `tls_ca` | `DIRQ_TLS_CA` | | CA certificate path |
| `tls_ca_key` | `DIRQ_TLS_CA_KEY` | | CA private key path (server only — enables per-agent mTLS cert issuance) |
| `tls_cert` | `DIRQ_TLS_CERT` | | This process's certificate path |
| `tls_key` | `DIRQ_TLS_KEY` | | This process's private key path |
| `tls_insecure` | `DIRQ_TLS_INSECURE` | `false` | Skip cert verification (agent only) |
| `tls_disabled` | `DIRQ_TLS_DISABLED` | `false` | Disable TLS entirely (not recommended) |

Example agent config with TLS and registration secret:

```
server: grpc.example.com:50051
exec_enabled: true
registration_secret: my-fleet-secret

tls_ca: /etc/dirq/certs/ca.crt
tls_cert: /etc/dirq/certs/agent.crt
tls_key: /etc/dirq/certs/agent.key

tags:
  env: prod
```

#### Signing (server only)

| Config key | Environment variable | Default | Description |
|-----------|----------|---------|-------------|
| `signing_key` | `DIRQ_SIGNING_KEY` | | Ed25519 private key file |
| `signing_pub` | `DIRQ_SIGNING_PUB` | | Ed25519 public key file |

#### Inline TLS certs (agent config)

Config files support inline base64-encoded PEM certs, so a single file contains everything an agent needs. The server generates these automatically in `/var/lib/dirq/agent.conf`.

| Config key | Environment variable | Description |
|-----------|----------|-------------|
| `tls_ca_data` | `DIRQ_TLS_CA_DATA` | Base64-encoded CA certificate PEM |
| `tls_cert_data` | `DIRQ_TLS_CERT_DATA` | Base64-encoded agent certificate PEM |
| `tls_key_data` | `DIRQ_TLS_KEY_DATA` | Base64-encoded agent private key PEM |

When `tls_ca_data`/`tls_cert_data`/`tls_key_data` are set and no file paths are given, the agent materializes them to `/var/lib/dirq/tls/` on startup.
