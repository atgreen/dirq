# Enable TLS & authentication

This page covers the concrete steps to secure a DirQ deployment: enabling TLS,
issuing and using per-agent mTLS certificates, rotating certificates, creating
API tokens, and setting the registration secret. For the reasoning behind these
mechanisms, see the [security model](../explanation/security.md). Configuration
keys are listed in the [configuration reference](../reference/configuration.md).

## Enable TLS

TLS is **enabled by default** on all gRPC and REST API connections. If no
certificates are configured, self-signed certs are auto-generated at startup.

| TLS vars set | Behavior |
|---|---|
| Nothing | Auto-generate self-signed + mTLS cert issuance per agent |
| `CERT` + `KEY` | TLS with user certs, no mTLS |
| `CERT` + `KEY` + `CA` + `CA_KEY` | Full mTLS with user-supplied CA |
| `DIRQ_TLS_DISABLED=true` | Explicitly insecure (must opt in) |

!!! warning "Production requires TLS and auth"
    The quick start sets `DIRQ_TLS_DISABLED=true` and `DIRQ_AUTH_DISABLED=true`
    for convenience. In production both must be enabled — with them off, API
    tokens and remote-exec payloads cross the network in cleartext, and any host
    that can reach the gRPC port can register or run commands.

## Issue and use per-agent mTLS certificates

When the server has access to the CA private key (auto-generated or via
`DIRQ_TLS_CA_KEY`), it issues a **unique TLS client certificate** to each agent
during registration. The certificate's CN is the agent ID, binding the TLS
identity to the application identity.

After registration:

- All gRPC connections (AgentStream, RequestPeers, relay) require a valid client
  cert signed by the server's CA
- The server and relay agents verify that the cert CN matches the claimed agent ID
- The registration secret becomes a **one-time bootstrap token** — a leaked
  secret can register an agent once, but the cert it receives is bound to that
  specific agent ID

This activates automatically when the CA key is available. On auto-generated
certs, it's always on. For user-supplied certs, set `DIRQ_TLS_CA_KEY`.

Agents persist their issued cert to disk and reuse it across restarts. Certs are
valid for 1 year; agents renew automatically when within 30 days of expiry (no
restart needed).

**Generate certs:**

```bash
# Self-signed CA (quick start)
dirq cert generate --dir ./certs

# Use your own CA
dirq cert generate --ca ./my-ca.crt --ca-key ./my-ca.key --dir ./certs
```

Both generate `server.crt`, `server.key`, `agent.crt`, `agent.key`, and a copy
of `ca.crt` in the output directory.

**Full mTLS with user-supplied CA:**

```bash
# Server (needs CA key to issue per-agent certs)
DIRQ_TLS_CA=./certs/ca.crt DIRQ_TLS_CA_KEY=./certs/ca.key \
DIRQ_TLS_CERT=./certs/server.crt DIRQ_TLS_KEY=./certs/server.key dirq-server

# Agent (only needs CA cert — gets its own cert during registration)
DIRQ_TLS_CA=./certs/ca.crt dirq-agent
```

## Rotate certificates

Rotate certificates across the fleet without downtime:

```bash
dirq cert rotate agent_cert --stagger 3600   # renew all agent certs over 1 hour
dirq cert rotate ca --stagger 3600           # distribute a new CA
dirq cert rotate signing_key                 # roll the message signing key
```

The `--stagger` flag spreads renewals over time to avoid overloading the server.
See [security model](../explanation/security.md) for the full rotation procedure including CA and
signing key rotation.

## API authentication and token creation

API authentication is **required by default**. On first startup, a bootstrap
token is auto-generated and printed to the server log. Save it.

```bash
dirq token create ops-team --scope admin
dirq token create monitoring --scope readonly
export DIRQ_TOKEN=<token>
```

**Token scopes are enforced per-endpoint:**

- `readonly` — queries, host listing, facts, inventory, query history, exec log
- `admin` — all of the above, plus tag management, token management, exec,
  put_file, fetch_file, deploy

Set `DIRQ_AUTH_DISABLED=true` to disable (not recommended).

## Set the registration secret

By default, any client that can reach the server's gRPC port can register as an
agent. For production deployments, set a **registration secret** — a pre-shared
key that agents must present during registration:

```bash
# Server
DIRQ_REGISTRATION_SECRET=my-fleet-secret dirq-server

# Agent
DIRQ_REGISTRATION_SECRET=my-fleet-secret dirq-agent
```

Or in config files:

```
# /etc/dirq/server.conf
registration_secret: my-fleet-secret

# /etc/dirq/agent.conf
registration_secret: my-fleet-secret
```

When configured, the server rejects `Register` calls that don't present the
matching secret. This prevents unauthorized hosts from joining the mesh.

Session tokens issued during registration are Ed25519-signed and time-stamped.
They expire after 24 hours, at which point the agent re-registers automatically
to obtain a fresh token. Relay peers verify session tokens cryptographically
using the server's signing public key — no shared state between relays and the
server is needed.

## See also

- [Security model](../explanation/security.md)
- [Enable exec & OPA/Rego policy](../how-to/agent-exec-policy.md)
- [Configuration reference](../reference/configuration.md)
- [Documentation home](../index.md)
