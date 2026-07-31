# Security model

DirQ manages large, locked-down fleets over an agent relay mesh. Because
control messages and execution requests flow through *other agents* on their way
to a target host, the security model cannot assume the network — or even the
intermediate relays — is trustworthy. This page explains the trust and threat
model and the reasoning behind each defense. For the concrete commands, see
[Enable TLS & authentication](../how-to/tls-and-auth.md) and
[Enable exec & OPA/Rego policy](../how-to/agent-exec-policy.md).

## Trust and threat model

DirQ is a central server fronting an agent relay mesh: agents form a tree, and
only a bounded set of *zone leaders* hold direct server connections. Queries and
exec requests are pushed from the server, relayed hop-by-hop through the mesh,
filtered or executed agent-side, and aggregated back at the server.

That topology defines the threats:

- The transport between any two nodes could be observed or tampered with.
- A relay agent sits in the path of messages bound for its downstream children.
  A compromised relay could try to forge, replay, or inject commands to the
  agents beneath it.
- Any host that can reach the server's gRPC port could try to join the mesh as
  an agent.
- Even a *validly authorized* exec request may be something a given host should
  not run.

DirQ answers each of these with a distinct layer — TLS for the transport,
per-agent mTLS for connection identity, Ed25519 message signing for command
authenticity, a registration secret for mesh admission, and agent-side policy
for local, defense-in-depth veto. The layers are independent, so defeating one
does not defeat the others.

## What per-agent mTLS provides

TLS is enabled by default on all gRPC and REST connections; if no certificates
are configured, self-signed certs are auto-generated at startup. TLS alone
protects the transport, but DirQ goes further and binds *connection identity* to
*application identity*.

When the server has access to the CA private key, it issues a **unique TLS
client certificate to each agent during registration**. The certificate's common
name (CN) is the agent ID, so the TLS identity and the application identity are
the same thing. After registration, every gRPC connection — AgentStream,
RequestPeers, relay — requires a valid client cert signed by the server's CA,
and both the server and relay agents verify that the cert CN matches the claimed
agent ID.

The important consequence is what this does to the registration secret: it
becomes a **one-time bootstrap token**. A leaked secret can register an agent
*once*, but the certificate that agent receives is bound to that specific agent
ID — it cannot be reused to impersonate other hosts across the mesh. Agents
persist their issued cert and renew automatically before expiry, so this
identity is durable without manual rotation.

## Why messages are signed (Ed25519)

Encrypting the transport is not enough when messages pass *through* relay
agents. A relay legitimately forwards traffic for its children, which means a
compromised relay is perfectly positioned to inject fake commands downstream if
authenticity is not enforced end-to-end.

So every control message the server sends through the mesh — queries, exec
requests, file transfers, rebalancer commands — is **signed with Ed25519** before
dispatch, and each agent verifies the signature before acting. This gives three
guarantees:

- **Only the server can originate commands.** Relays forward signed messages but
  cannot forge them.
- **Signatures carry a short expiry window (5 minutes)**, which defeats replay
  of captured messages.
- **The server's public key reaches agents during registration**, over the
  already-TLS-protected gRPC stream, so there is no separate key-distribution
  channel to attack.

The same signing key underpins session tokens: tokens issued at registration are
Ed25519-signed and time-stamped, expire after 24 hours, and are verified
cryptographically by relay peers using the server's public key — no shared state
between relays and the server is required.

## The registration-authentication model

By default, any client that can reach the server's gRPC port can register as an
agent. That is convenient for a laptop, but wrong for production. A **registration
secret** — a pre-shared key agents must present during registration — closes the
mesh: the server rejects `Register` calls that don't present the matching secret,
so unauthorized hosts cannot join.

The registration secret and per-agent mTLS reinforce each other. The secret
gates *admission*; mTLS ensures that even a leaked secret only buys a single,
identity-bound registration rather than a reusable fleet-wide credential.

## Defense in depth: execution security and agent-side policy

Remote execution is the highest-stakes capability in DirQ, so it is fenced on
several sides at once:

- **Server-originated only.** Exec requests must come from the server and carry a
  valid Ed25519 signature; relays forward but cannot forge them.
- **Opt-in per agent.** `exec_enabled` defaults to `false` — a host runs nothing
  remotely until its operator turns exec on.
- **Full audit trail.** Every operation is logged with AAP job ID, user, command,
  and exit status.
- **AAP retains authority.** DirQ is the data plane; AAP owns RBAC, credentials,
  and approvals.
- **Bounded blast radius.** File transfers are capped (100 MB default). On
  Windows the agent runs as SYSTEM and become uses PowerShell scheduled tasks;
  on Linux become uses `sudo -n` (non-interactive, NOPASSWD required).

The final layer is **agent-side OPA/Rego policy**. Everything above establishes
that a request is *authentic and authorized*. Agent-side policy lets each host
additionally decide, locally, whether it is *willing* to carry the request out —
defense in depth even for validly-authorized operations. When a `policy_file` is
configured, the agent compiles the Rego policy at startup and evaluates it before
every `exec`, `put_file`, `fetch_file`, and `deploy` side effect; a denied
operation returns a terminal `policy denied: …` error and runs nothing.

This is what makes DirQ usable for regulated fleets: because the decision is made
on the host with a documented JSON input per operation, policies can express
segregation of duties, break-glass, per-AAP-user authorization, and path
restrictions — enforced even against a request the server considered legitimate.
The policy never sees raw file content, script bodies, or environment values;
those are reduced to sizes, SHA-256 hashes, and key names.

For the mechanics — enabling exec, reading the audit log, and writing a policy —
see [Enable exec & OPA/Rego policy](../how-to/agent-exec-policy.md). The
`policy_file` / `DIRQ_POLICY_FILE` keys and related settings are documented in
the [configuration reference](../reference/configuration.md).

## See also

- [Enable TLS & authentication](../how-to/tls-and-auth.md)
- [Enable exec & OPA/Rego policy](../how-to/agent-exec-policy.md)
- [Configuration reference](../reference/configuration.md)
- [Documentation home](../index.md)
