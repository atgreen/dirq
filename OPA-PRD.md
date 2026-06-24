# OPA/Rego Agent Policy PRD

## Summary

Add optional Open Policy Agent (OPA) / Rego enforcement to `dirq-agent` as a
last-mile policy layer for remote execution, file transfer, and deploy
operations.

DirQ already authenticates operators at the server, signs server-to-agent
control messages, and requires each agent to opt in with `exec_enabled=true`.
Those controls answer "is this request validly authorized and routed by DirQ?"
Agent-side policy answers a different question: "Even if the request is valid,
is this host willing to perform this local action?"

This feature should be implemented as defense in depth. It should not replace
server-side API authorization, token scopes, message signing, or mTLS.

## Problem

The current agent execution gate is coarse:

- `exec_enabled=false` rejects all exec, file, and deploy operations.
- `exec_enabled=true` allows every valid server-dispatched operation the agent
  understands.

That binary model is not enough for mixed-risk fleets. Operators may want
different local rules for production, PCI, lab, Windows, Linux, database, or
edge hosts while still using one DirQ control plane.

Examples:

- Production hosts should allow approved AAP templates but reject ad-hoc shell.
- File writes should be limited to approved directories.
- File reads should reject secrets and host credentials.
- Deploys should only install from expected temporary paths and package tools.
- `become=true` should require stronger conditions than unprivileged exec.
- Break-glass operations should be explicitly logged and easy to detect.

## Goals

- Add optional Rego policy evaluation inside `dirq-agent`.
- Enforce policy before any local side effect happens.
- Cover these operations in the first version:
  - exec command
  - exec script
  - put file
  - fetch file
  - deploy package
- Keep default behavior unchanged when no policy is configured.
- Support fail-closed behavior for production deployments.
- Return clear denial reasons to the server and CLI.
- Log policy decisions locally with request ID, operation, decision, and reason.
- Make policy input stable, documented, and testable.

## Non-Goals

- Do not replace server-side API auth, token scopes, or admin checks.
- Do not use Rego for fleet target resolution.
- Do not attempt to safely parse arbitrary shell syntax.
- Do not distribute policy bundles from the server in the first version.
- Do not add a central policy management UI in the first version.
- Do not make OPA required for agents that only need the current
  `exec_enabled` behavior.

## User Stories

### Production Platform Owner

As a platform owner, I want production agents to reject ad-hoc shell commands
unless they come from an approved AAP job template, so that routine automation
continues to work while emergency shell access is constrained.

Acceptance criteria:

- A prod-tagged agent can allow `aap_job_template=restart-nginx`.
- The same agent can deny `dirq exec WHERE tag.env = 'prod' -- sh`.
- The denial is returned as a failed exec result with a policy reason.
- The agent logs the policy denial with the request ID.

### Security Engineer

As a security engineer, I want hosts to deny file reads from sensitive paths, so
that a compromised server token or overbroad operator action cannot trivially
exfiltrate local credentials.

Acceptance criteria:

- `fetch_file` can be denied for paths such as `/etc/shadow`,
  `/root/.ssh/`, and Windows credential locations.
- `fetch_file` can be allowed for approved diagnostic paths.
- Path checks use cleaned absolute paths, not raw unnormalized user input.

### Application Owner

As an application owner, I want file writes limited to my application's config
directory, so that automation can update app configuration without granting
write access to arbitrary host paths.

Acceptance criteria:

- `put_file` can allow `/etc/myapp/*.conf`.
- `put_file` can deny `/etc/sudoers`, `/usr/bin/*`, and arbitrary temp
  scripts.
- Policy input includes destination path, mode, content size, become flag, and
  become user.

### Windows Administrator

As a Windows administrator, I want Windows agents to enforce separate rules for
PowerShell and user impersonation, so that SYSTEM-level capabilities are not
exposed without explicit policy.

Acceptance criteria:

- Policy input includes OS and operation details.
- Windows policies can deny `become=true` with a non-empty `become_user`.
- Windows policies can restrict commands that invoke PowerShell.

### Fleet Operator

As a fleet operator, I want dry, understandable policy failures in command
output, so that I can fix targeting or request approval without opening agent
logs first.

Acceptance criteria:

- Denied exec returns `Success=false`, `Rc=-1`, and an error beginning with
  `policy denied:`.
- Denied file and deploy operations use their existing response types with a
  policy denial error.
- Broadcast exec reports denied hosts as terminal responses, not missing hosts.

### Agent Administrator

As an agent administrator, I want policy configured locally through the existing
agent config model, so that I can roll it out with existing packaging,
configuration management, or AAP workflows.

Acceptance criteria:

- `policy_file` can be set in `/etc/dirq/agent.conf`.
- `DIRQ_POLICY_FILE` overrides the config file.
- `policy_fail_closed` can be set in config or `DIRQ_POLICY_FAIL_CLOSED`.
- If no policy file is configured, behavior is unchanged.

## Proposed Configuration

Agent config:

```yaml
exec_enabled: true
policy_file: /etc/dirq/policy.rego
policy_fail_closed: true
```

Environment variables:

| Environment Variable | Config Key | Default | Description |
|---|---|---|---|
| `DIRQ_POLICY_FILE` | `policy_file` | empty | Path to a local Rego policy file |
| `DIRQ_POLICY_FAIL_CLOSED` | `policy_fail_closed` | `true` when policy is configured | Deny if policy load/eval fails |
| `DIRQ_POLICY_QUERY` | `policy_query` | `data.dirq.agent.allow` | Rego decision query |

First version should support a single local policy file. Bundle download,
signature verification, and server-pushed policy can be added later.

## Policy Decision Model

The agent evaluates a boolean allow decision and an optional reason.

Recommended Rego package:

```rego
package dirq.agent

default allow := false
default reason := "denied by default"
```

The agent should query:

- `data.dirq.agent.allow`
- `data.dirq.agent.reason`

If `allow` is not true, the operation is denied. If `reason` is absent, use
`policy denied`.

## Policy Input

The agent should pass a stable, operation-specific JSON object. Avoid exposing
raw protobuf messages as policy input because generated message shape is not a
good external contract.

Common fields:

```json
{
  "operation": "exec",
  "request_id": "execm-123",
  "agent_id": "agent-123",
  "hostname": "web-01",
  "os": "linux",
  "tags": {"env": "prod", "role": "web"},
  "time_unix": 1782316800
}
```

Exec command fields:

```json
{
  "operation": "exec",
  "command": "systemctl restart nginx",
  "script": false,
  "script_name": "",
  "stdin_size": 0,
  "environment_keys": ["FOO"],
  "become": true,
  "become_user": "root",
  "become_method": "sudo",
  "timeout_seconds": 300,
  "aap_job_id": "12345",
  "aap_job_template": "restart-nginx",
  "aap_user": "alice"
}
```

Script exec fields:

```json
{
  "operation": "exec",
  "command": "",
  "script": true,
  "script_name": "deploy.sh",
  "script_size": 4812,
  "script_sha256": "hex...",
  "become": true,
  "become_user": "root"
}
```

Put file fields:

```json
{
  "operation": "put_file",
  "dest_path": "/etc/myapp/app.conf",
  "content_size": 2048,
  "content_sha256": "hex...",
  "mode": 416,
  "become": true,
  "become_user": "root"
}
```

Fetch file fields:

```json
{
  "operation": "fetch_file",
  "src_path": "/var/log/myapp/app.log",
  "become": false,
  "become_user": ""
}
```

Deploy fields:

```json
{
  "operation": "deploy",
  "dest_path": "/tmp/dirq-deploy/pkg.rpm",
  "content_size": 12000000,
  "content_sha256": "hex...",
  "mode": 384,
  "install_command": "rpm -U /tmp/dirq-deploy/pkg.rpm",
  "become": true,
  "become_user": "root",
  "timeout_seconds": 300
}
```

Path fields must use cleaned absolute paths after the agent's existing
normalization and absolute-path validation.

## Example Policy

```rego
package dirq.agent

default allow := false
default reason := "denied by default"

allow if {
  input.operation == "exec"
  input.tags.env != "prod"
  not input.become
}

allow if {
  input.operation == "exec"
  input.tags.env == "prod"
  input.aap_job_template == "restart-nginx"
  input.command == "systemctl restart nginx"
  input.become
  input.become_user == "root"
}

allow if {
  input.operation == "put_file"
  startswith(input.dest_path, "/etc/myapp/")
  input.content_size <= 1048576
  input.mode <= 0o640
}

allow if {
  input.operation == "fetch_file"
  startswith(input.src_path, "/var/log/")
  not input.become
}

allow if {
  input.operation == "deploy"
  input.tags.env != "prod"
  startswith(input.dest_path, "/tmp/")
}

reason := "prod exec requires an approved AAP template" if {
  input.operation == "exec"
  input.tags.env == "prod"
  not allow
}

reason := "file reads are limited to /var/log without become" if {
  input.operation == "fetch_file"
  not allow
}
```

## Implementation Plan

### Phase 1: Agent Policy Engine

Add a new package:

```text
internal/agent/policy/
```

Responsibilities:

- Load a local Rego file.
- Compile policy at agent startup.
- Expose a small Go interface for operation checks.
- Convert operation data into stable input maps.
- Return allow/deny decisions with reason.
- Support a no-op engine when no policy is configured.

Suggested interface:

```go
type Decision struct {
	Allow  bool
	Reason string
}

type Engine interface {
	Eval(ctx context.Context, input Input) (Decision, error)
}
```

Use `github.com/open-policy-agent/opa/rego` as the embedded evaluator.

### Phase 2: Agent Config Wiring

Extend `agent.Config` with:

```go
PolicyFile       string
PolicyFailClosed bool
PolicyQuery      string
```

Read values in `cmd/dirq-agent/main.go` using the existing config helper
pattern:

- `DIRQ_POLICY_FILE` / `policy_file`
- `DIRQ_POLICY_FAIL_CLOSED` / `policy_fail_closed`
- `DIRQ_POLICY_QUERY` / `policy_query`

Construct the policy engine in `agent.New` or `Agent.Run`. Prefer startup
compilation so syntax errors are detected before the agent advertises itself as
ready for exec.

### Phase 3: Enforcement Hooks

Add policy checks before side effects:

- `handleExecRequest`: after `exec_enabled` and timeout normalization, before
  script temp file creation or command construction.
- `handlePutFile`: after path cleaning, absolute-path check, and size check,
  before `MkdirAll`, `sudo tee`, or `WriteFile`.
- `handleFetchFile`: after path cleaning and absolute-path check, before
  `Stat`, `ReadFile`, or `sudo cat`.
- `handleDeploy`: after timeout and destination path validation, before
  directory creation and package write.

Denied operations should return normal terminal responses:

- Exec: `ExecResponse{Success:false, Rc:-1, Error:"policy denied: <reason>"}`
- Put file: `FileChunk{Success:false, Error:"policy denied: <reason>"}`
- Fetch file: `FetchFileResponse{Success:false, Error:"policy denied: <reason>"}`
- Deploy: `DeployResponse{Success:false, Phase:"policy", Error:"policy denied: <reason>"}`

### Phase 4: Logging and Observability

Agent logs should include:

- request ID
- operation
- decision
- reason
- policy file path
- evaluation error, if any

Do not log full script bodies, stdin, file content, or environment values.

Optional metrics for later:

- `dirq_agent_policy_decisions_total{operation,decision}`
- `dirq_agent_policy_eval_seconds{operation}`
- `dirq_agent_policy_errors_total`

### Phase 5: Documentation

Update:

- `README.md` configuration reference
- `SECURITY.md` remote execution controls
- `packaging/agent.conf` with commented examples

Include:

- minimal allowlist policy
- production AAP-only policy
- file path restriction policy
- fail-open vs fail-closed explanation

### Phase 6: Tests

Unit tests:

- Policy loads and compiles.
- Missing policy returns no-op allow behavior.
- Syntax error fails startup or denies based on fail-closed setting.
- Allow and deny decisions parse correctly.
- Deny reason fallback works.
- Input generation excludes sensitive content and includes hashes/sizes.

Agent handler tests:

- Denied exec does not call command construction or create temp script files.
- Denied put file does not create directories or write files.
- Denied fetch file does not stat or read files.
- Denied deploy does not write package content.
- Broadcast exec receives policy denials as responses, not timeouts.

Policy examples:

- Validate example policies with `opa test` if the OPA CLI is available.
- Add Go tests that compile example policy strings directly so CI does not
  require the external OPA binary.

## Rollout Plan

1. Ship the feature disabled by default.
2. Add documentation and example policies.
3. Test on lab agents with `policy_fail_closed=false` only if operators need a
   discovery period.
4. Move production policies to `policy_fail_closed=true`.
5. Treat policy changes as configuration changes managed by the same deployment
   mechanism as `agent.conf`.

## Security Considerations

- Policy files must be local and protected by filesystem permissions.
- Policy should fail closed by default when configured.
- Policy input must avoid raw secrets:
  - include environment variable names, not values
  - include stdin size/hash, not stdin content
  - include script size/hash/name, not script body
  - include file content size/hash, not file content
- Rego should not be used as a shell parser. Policies that need high assurance
  should allow exact commands, approved templates, paths, hashes, or operation
  types rather than broad string pattern checks.
- Agent policy is not a substitute for least-privilege operator tokens and
  server-side authorization.

## Open Questions

- Should policy denial responses be written into the existing exec audit log
  with a distinct status?
- Should agents advertise a `policy_enabled` capability during registration?
- Should server/API responses expose policy state in host inventory?
- Should policy support multiple files or OPA bundles in v2?
- Should deploy support signed package metadata so policy can validate artifact
  identity more strongly than path and hash?

## Success Metrics

- Operators can roll out per-host policy without changing server behavior.
- Existing deployments with no policy configured continue to behave exactly as
  before.
- Policy denial produces an immediate terminal result for each targeted host.
- No denied operation performs local side effects before evaluation.
- Example policies cover the common production, file, and deploy restrictions.
