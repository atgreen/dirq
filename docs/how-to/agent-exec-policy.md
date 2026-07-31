# Enable exec & OPA/Rego policy

This page covers enabling remote execution on agents, reading the exec audit
log, and configuring an agent-side OPA/Rego policy that can veto operations
locally. For the reasoning — why exec is fenced this way and how agent-side
policy provides defense in depth — see the
[security model](../explanation/security.md). All policy configuration keys are
in the [configuration reference](../reference/configuration.md).

## Enable exec on agents

Exec is **disabled by default** — opt in per agent:

```bash
DIRQ_EXEC_ENABLED=true ./bin/dirq-agent
```

Default exec timeout is 300 seconds (5 minutes), configurable via
`dirq_exec_timeout` in the connection plugin. Long-running tasks like
`yum update` work without special handling — the broadcast dispatcher has no idle
timeout, so `--timeout 3600` against a slow fleet behaves as written rather than
getting cut off after the first burst of fast responders. Exec responses are
forwarded immediately through the relay chain — they are not batched by the
result aggregator.

## Exec audit log

Every operation is logged in PostgreSQL with AAP job attribution:

```bash
curl "$DIRQ_SERVER_URL/api/v1/exec_log?aap_job_id=42"
```

## Configure an agent-side OPA/Rego policy

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

The policy is controlled by three keys — `policy_file` / `DIRQ_POLICY_FILE`,
`policy_fail_closed` / `DIRQ_POLICY_FAIL_CLOSED` (defaults to `true` when
`policy_file` is set), and `policy_query` / `DIRQ_POLICY_QUERY` (the Rego
decision query, default `data.dirq.agent.allow`). See the
[configuration reference](../reference/configuration.md) for the full table.

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
[security model](../explanation/security.md) for the full model.

## See also

- [Security model](../explanation/security.md)
- [Enable TLS & authentication](../how-to/tls-and-auth.md)
- [Configuration reference](../reference/configuration.md)
- [Documentation home](../index.md)
