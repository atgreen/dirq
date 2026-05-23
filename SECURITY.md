# DirQ Security Model

This document describes DirQ's security architecture: how components
authenticate, how communication is protected, and what controls exist
for authorization and remote execution.

## Architecture Overview

DirQ has three components that communicate over two protocols:

- **dirq-server** — central server, exposes gRPC (agents) and REST/HTTP (CLI)
- **dirq-agent** — runs on managed hosts, connects to server or relay peer via gRPC
- **dirq** (CLI) — operator tool, talks to server over HTTPS

```
CLI ──HTTPS──▶ Server ◀──gRPC──▶ Zone Leader ◀──gRPC──▶ Relay ◀──gRPC──▶ Leaf
                                     │                    │
                                     └── gRPC ──▶ Leaf    └── gRPC ──▶ Leaf
```

Agents form a relay mesh tree. Zone leaders connect directly to the
server; relays and leaves connect to their assigned parent. All links
are TLS-encrypted.

## Transport Layer Security

### TLS Configuration

All gRPC and HTTP connections use TLS 1.2+ with ECDSA P-256 keys.

DirQ generates its own certificate authority and issues certificates
automatically if none are provided:

| Certificate | CN | Validity | SANs |
|---|---|---|---|
| CA | DirQ CA | 10 years | — |
| Server | DirQ Server | 1 year | localhost, 127.0.0.1, ::1, hostname, all interface IPs |
| Agent (per-agent) | agent ID | 1 year | localhost, 127.0.0.1, ::1 |

Auto-generated certs are stored in `/var/lib/dirq/tls/` (mode 0700).

To generate certs manually, or to use your own CA:

```
dirq cert generate                                      # self-signed CA
dirq cert generate --ca ./my-ca.crt --ca-key ./my-ca.key # your CA
```

Operators can also supply certs directly via `DIRQ_TLS_CERT`,
`DIRQ_TLS_KEY`, and `DIRQ_TLS_CA` environment variables or config
file equivalents.

Inline base64-encoded PEM is also supported (`tls_cert_data`,
`tls_key_data`, `tls_ca_data` in config), which is how the
server-generated `agent.conf` distributes the CA certificate.

### Mutual TLS (mTLS)

When the server has a CA key (auto-generated or via `DIRQ_TLS_CA_KEY`),
it issues a unique TLS client certificate to each agent during
registration. The certificate's Common Name is set to the agent's ID,
creating a cryptographic binding between TLS identity and agent identity.

The server uses `VerifyClientCertIfGiven` at the TLS layer, allowing
both unauthenticated connections (for initial registration) and
mTLS-authenticated connections (for all subsequent RPCs) on the same
port. Per-RPC interceptors enforce the requirement:

- **Register** — mTLS not required (the agent has no cert yet)
- **AgentStream, RequestPeers** — mTLS required; the interceptor
  extracts the certificate CN and rejects the call if it doesn't match
  the claimed agent ID

This prevents an attacker with a stolen session token from
impersonating a different agent — they would also need that agent's
private key.

### MITM Protection

- All links use TLS with CA verification (agents load the CA cert at
  registration)
- Ed25519 message signatures (see below) provide end-to-end integrity
  even if a relay is compromised — relays forward signed messages but
  cannot forge new ones
- Server signing key pinning: the agent can pin the server's Ed25519
  public key at first registration. Subsequent connections verify the
  key matches using constant-time comparison, preventing key
  substitution attacks

## Authentication

### Agent Registration

A new agent authenticates to the server using an optional pre-shared
registration secret (`DIRQ_REGISTRATION_SECRET`). If configured, the
server rejects registration attempts with the wrong secret. If not
configured, registration is open.

The registration flow:

1. Agent sends `RegisterRequest` with hostname, OS, capabilities, and
   the registration secret
2. Server validates the secret (if configured)
3. Server assigns the agent an ID, role, and position in the topology
4. Server issues an Ed25519-signed session token
5. Server issues a per-agent mTLS certificate (CN = agent ID)
6. Server returns the session token, mTLS cert/key, CA cert, and
   signing public key in the `RegisterResponse`
7. Agent persists credentials to disk and connects to its assigned
   parent

### Agent Session Tokens

After registration, agents authenticate to the server (or relay peer)
using a session token on every gRPC stream.

Token format: `base64(ed25519_signature):unix_timestamp`

The signature covers `agentID:timestamp`, binding the token to a
specific agent at a specific time. Verification:

1. Split token into signature and timestamp
2. Reject if expired (older than 24 hours)
3. Reject if timestamp is more than 30 seconds in the future (clock skew)
4. Reconstruct payload (`agentID:timestamp`) and verify Ed25519 signature

The server caches tokens in memory for fast-path validation. After a
server restart, the cache is empty but cryptographic verification still
works because the signing key is persisted.

Relay agents also verify session tokens from their downstream peers
using the server's public key — no server round-trip needed.

### CLI Authentication

CLI clients authenticate to the REST API using bearer tokens in the
`Authorization` header.

API tokens are generated server-side: 32 random bytes encoded as a
64-character hex string. The first 8 characters serve as a non-secret
prefix for O(1) database lookup; the full token is stored as a bcrypt
hash. Token validation is prefix-lookup then bcrypt-compare, avoiding
a full table scan on every request.

Authentication can be disabled with `DIRQ_AUTH_DISABLED=true` for
development environments.

## Authorization

API tokens carry a scope: **readonly** or **admin**.

| Scope | Allowed Operations |
|---|---|
| readonly | Query fleet, list hosts, view facts, view inventory, view status |
| admin | All readonly operations plus: tag management, token management, remote execution, file transfer, package deploy |

Scope is enforced per-endpoint by the `requireScope` middleware.
A readonly token that attempts to call an admin endpoint receives
HTTP 403.

On the agent side, remote execution is independently gated by the
`exec_enabled` configuration flag. An agent with exec disabled will
reject exec, put_file, and fetch_file requests regardless of the
caller's authorization.

## Message Signing

The server signs all control messages (queries, exec requests, topology
updates) with Ed25519. This provides end-to-end integrity through the
relay mesh.

**Key management:**
- Server generates an Ed25519 keypair at startup, stored in
  `/var/lib/dirq/signing/` (mode 0600)
- The public key is distributed to every agent during registration
- Agents can optionally pin the key in their config for
  trust-on-first-use verification

**Signing process:**
1. Set `signer_key_id` (SHA-256 of public key, truncated), `signed_at_unix`,
   and `expires_at_unix` (5-minute TTL)
2. Marshal the message to canonical protobuf bytes (signature field cleared)
3. Sign with Ed25519

**Verification (performed by every agent that receives the message):**
1. Verify key ID matches the expected server key
2. Reject if expired or if signed-at timestamp is too far in the future
3. Verify Ed25519 signature over canonical bytes

Because relays forward messages without re-signing, a compromised relay
cannot forge server-originated commands. It can drop or delay messages,
but it cannot fabricate new ones.

### Replay Protection

- **Session tokens** expire after 24 hours and include a timestamp in
  the signed payload
- **Server messages** expire after 5 minutes (`expires_at_unix`) and
  include a `signed_at_unix` timestamp; messages with future timestamps
  (beyond 30 seconds of clock skew) are rejected
- **mTLS** binds the transport to a specific agent identity, so a
  replayed token on a different TLS connection is rejected

## Remote Execution Security

Remote execution (exec, put_file, fetch_file, deploy) has multiple
layers of protection:

1. **API authorization** — requires admin-scoped token
2. **Agent opt-in** — agent must have `exec_enabled=true` in its config
3. **Message signing** — exec requests are signed by the server;
   agents verify the signature before executing
4. **Path validation** — `put_file` and `fetch_file` require absolute
   paths and apply `filepath.Clean()` to prevent path traversal
5. **Shell quoting** — command arguments are wrapped in shell-safe
   quoting to prevent injection
6. **Privilege escalation** — sudo (Linux) and scheduled tasks
   (Windows) are used for become-user execution; sudo requires
   NOPASSWD in sudoers
7. **Timeouts** — all exec operations have a configurable timeout
   (default 300 seconds) enforced via context cancellation
8. **File size limits** — put_file and fetch_file reject files larger
   than 100 MB
9. **Audit logging** — all exec operations are logged with request ID,
   agent ID, command, and outcome

## Rate Limiting

The HTTP API uses per-token token-bucket rate limiting. Broadcast and
single-host exec have separate buckets so that an Ansible run with
`--forks N` against many hosts can't be starved by a slow trickle of
broadcast queries:

| Endpoint family | Rate | Burst |
|---|---|---|
| Broadcast (`/api/v1/query`, `/api/v1/exec_multi`, `/api/v1/deploy`) | 10 req/s | 20 |
| Single-host (`/api/v1/exec`, `/api/v1/put_file`, `/api/v1/fetch_file`) | 100 req/s | 500 |

**Key:** API token (or remote IP if auth is disabled). Each API token has
an independent bucket per family, so one client's traffic does not affect
another's. Exceeding the limit returns HTTP 429.

Both the standalone Ansible connection plugin and the collection's shared
client retry HTTP 429 with exponential backoff + jitter so transient
rate-limit bursts don't fail a playbook task.

## LLM Security (`dirq ask`)

The `dirq ask` command sends natural-language questions to an LLM,
which calls DirQ fleet management tools in a loop to gather data
before answering.

**Tool restrictions:**
- Only read-only tools are exposed (query, hosts list, facts, CVE
  scan, graph)
- Exec and tag mutation tools are excluded
- The LLM suggests `dirq exec` commands when changes are needed but
  cannot execute them

**Prompt injection hardening:**
- The system prompt restricts the LLM to fleet-related questions only;
  general knowledge, coding help, and conversation are refused
- All tool output is treated as untrusted data — the LLM is instructed
  to ignore instructions embedded in hostnames, tags, error messages,
  or query results
- Tool results are summarized, never interpreted as instructions

## Certificate and Key Rotation

DirQ supports zero-downtime rotation of TLS certificates, the CA, and
the Ed25519 signing key. Existing gRPC streams survive all rotation
operations — TLS certs only matter at handshake time.

### Agent Certificate Renewal

Agent mTLS certificates (1-year validity) are renewed automatically:

- Every 12 hours, the agent checks if its cert expires within 30 days
- If so, it calls the `RenewCert` RPC using its existing (still-valid)
  mTLS connection
- The server validates the CN matches, issues a new cert, and returns
  it along with the current CA bundle and signing keys
- The agent persists the new cert to disk; the next connection uses it
- No re-registration, no topology reset

### Forced Rotation

The server can push a rotation command through the mesh to all agents:

```
dirq cert rotate agent_cert --stagger 3600
```

The `--stagger` flag spreads the load: each agent waits a random
delay in `[0, stagger)` seconds before calling `RenewCert`. For a
fleet of 1M agents with `--stagger 3600`, the server sees ~278
renewals/second instead of a thundering herd. The command is relayed
through the mesh immediately; only the local action is delayed. If
omitted, agents act immediately.

Three rotation types:

| Type | Effect |
|---|---|
| `agent_cert` | All agents call `RenewCert` immediately |
| `signing_key` | Agents update their signing key verifier from the command payload |
| `ca` | Agents call `RenewCert` to get the new CA bundle |

The `RotateCommand` is signed by the server and relayed through the
mesh like a query. Each agent verifies the signature, acts on it, and
relays it to its children.

### CA Rotation

To rotate the CA without disruption:

1. Generate a new CA keypair
2. Configure the server with both CAs: `tls_ca` (new) and
   `tls_ca_old` (old)
3. Restart the server — it loads both CAs into its trust pool and
   issues new certs under the new CA
4. Trigger forced rotation: `dirq cert rotate ca --stagger 3600`
5. All agents renew their certs (signed by the new CA) and receive
   both CAs in their trust bundle
6. After all agents have renewed, remove `tls_ca_old` from the config

During the transition window, certs signed by either CA are accepted.

### Signing Key Rotation

To rotate the Ed25519 signing key:

1. Generate a new key, configure as `signing_key` / `signing_pub`
2. Move the old key to `signing_key_old` / `signing_pub_old`
3. Restart the server — it signs with the new key but accepts tokens
   from both keys
4. Trigger forced rotation: `dirq cert rotate signing_key`
5. Agents update their verifier to trust both keys (new key from the
   command payload, old key preserved)
6. After all agents have updated (24 hours max, since session tokens
   expire in 24h), remove the old key config

### Server TLS Certificate Hot Reload

The server dynamically reloads its TLS certificate from disk:

- Every 60 seconds, the cert is re-read from `tls_cert` / `tls_key`
- Send `SIGHUP` to trigger an immediate reload
- No restart required — only new TLS handshakes use the new cert;
  existing connections are unaffected

To rotate the server's TLS cert, replace the files on disk and either
wait 60 seconds or send `SIGHUP`.

## Configuration Security

- **Bootstrap token** — written to `/var/lib/dirq/bootstrap-token`
  (mode 0600) instead of server logs to prevent credential leakage
- **API tokens** — accepted only via `Authorization` header; the
  query-string `?token=` parameter was removed to prevent
  log/proxy credential leakage
- **Auto-generated keys** — stored in `/var/lib/dirq/` with 0700
  directory permissions; falls back to a user-private temp directory
- **Config file** — `client.conf` and `server.conf` support all
  sensitive values (tokens, TLS material); `dirq doctor` validates
  config files for unknown keys to catch typos
