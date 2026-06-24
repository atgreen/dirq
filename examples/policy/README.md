# DirQ Agent Policy (OPA/Rego)

This directory holds example agent-side policies and the reference for the
`input` document your Rego rules receive.

Agent-side policy is **defense in depth**. The DirQ server already
authenticates operators, signs control messages, and requires each agent to opt
in with `exec_enabled=true`. Those answer *"is this request validly authorized
and routed by DirQ?"* A policy answers a different question: *"Even if the
request is valid, is this host willing to perform this local action?"* It does
not replace server-side authorization, token scopes, message signing, or mTLS.

## Enabling

In `/etc/dirq/agent.conf` (or via environment variables):

```yaml
exec_enabled: true
policy_file: /etc/dirq/policy.rego
policy_fail_closed: true
```

| Config key | Env var | Default | Description |
|---|---|---|---|
| `policy_file` | `DIRQ_POLICY_FILE` | (unset) | Path to a local Rego policy file. Unset ⇒ no policy, behavior unchanged. |
| `policy_fail_closed` | `DIRQ_POLICY_FAIL_CLOSED` | `true` when `policy_file` is set | Deny when the policy fails to load or evaluate. |
| `policy_query` | `DIRQ_POLICY_QUERY` | `data.dirq.agent.allow` | The Rego decision query. |

The policy is compiled **once at agent startup**. A syntax error in a
fail-closed agent prevents startup; in a fail-open agent it logs a warning and
falls back to allowing (no policy). Changing the policy file requires an agent
restart.

## Decision contract

The agent evaluates two rules:

- **`data.dirq.agent.allow`** (boolean) — the operation proceeds only if this
  is exactly `true`. Anything else (false, undefined) denies.
- **`data.dirq.agent.reason`** (string, optional) — surfaced to the operator
  and logs on denial. If absent, the error is the bare `policy denied`.

A canonical skeleton:

```rego
package dirq.agent

default allow := false
default reason := "denied by default"
```

If you set `policy_query` to a custom path ending in `.allow`, the agent
derives the reason query by swapping the suffix for `.reason`
(e.g. `data.acme.exec.allow` → `data.acme.exec.reason`).

On denial the agent returns a terminal response (no side effect occurs) with an
error prefixed `policy denied:` — exec also reports `Success=false, Rc=-1`,
deploy reports `Phase="policy"`.

## Input reference

Every evaluation receives a JSON `input` document. The canonical source is the
`Input` struct in `internal/agent/policy/input.go`; this table mirrors it.

### Common fields (every operation)

| Field | Type | Notes |
|---|---|---|
| `operation` | string | One of `exec`, `put_file`, `fetch_file`, `deploy`. |
| `request_id` | string | Unique per operation; appears in agent logs. |
| `agent_id` | string | This agent's ID. |
| `hostname` | string | This agent's reported hostname. |
| `os` | string | `runtime.GOOS` — `linux`, `windows`, `darwin`. |
| `tags` | object{string:string} | The agent's configured tags (e.g. `{"env":"prod"}`). |
| `time_unix` | number | Evaluation time, Unix seconds. See the caveat below. |
| `become` | bool | Privilege escalation requested. |
| `become_user` | string | Target user (omitted when empty). |
| `become_method` | string | `sudo`, `su`, etc. (exec only; omitted when empty). |

### `exec`

| Field | Type | Notes |
|---|---|---|
| `command` | string | The command string. Empty for script execs. |
| `script` | bool | `true` when a script body was supplied instead of a command. |
| `script_name` | string | Original filename, e.g. `deploy.sh` (drives temp extension). |
| `script_size` | number | Script length in bytes. |
| `script_sha256` | string | Hex SHA-256 of the script body. |
| `stdin_size` | number | Length of piped stdin in bytes. |
| `environment_keys` | array[string] | **Names only**, sorted. Never values. |
| `timeout_seconds` | number | Requested timeout. |
| `aap_job_id`, `aap_job_template`, `aap_user` | string | Ansible Automation Platform attribution, when present. |

### `put_file`

| Field | Type | Notes |
|---|---|---|
| `dest_path` | string | Cleaned absolute destination path. |
| `content_size` | number | Bytes to be written. |
| `content_sha256` | string | Hex SHA-256 of the content. |
| `mode` | number | Unix file mode (e.g. `416` = `0o640`). Ignored on Windows. |
| `aap_job_id`, `aap_job_template`, `aap_user` | string | AAP attribution, when present. |

### `fetch_file`

| Field | Type | Notes |
|---|---|---|
| `src_path` | string | Cleaned absolute source path. |
| `aap_job_id`, `aap_job_template`, `aap_user` | string | AAP attribution, when present. |

### `deploy`

| Field | Type | Notes |
|---|---|---|
| `dest_path` | string | Cleaned absolute path the package is written to. |
| `content_size` | number | Package size in bytes. |
| `content_sha256` | string | Hex SHA-256 of the package. |
| `mode` | number | Unix file mode for the written package. |
| `install_command` | string | Command run after the package is written. |
| `timeout_seconds` | number | Requested timeout. |

> Fields marked "omitted when empty" use `omitempty`: they are absent from
> `input` rather than present-and-empty. In Rego, reference them defensively
> (e.g. `input.become_user == "alice"` is simply false when the key is absent).

## Redaction guarantees

Policy input is the subject of policy (commands, paths) plus metadata — never
raw secrets. Specifically, the agent **never** places these in `input`:

- file content or package bytes → only `content_size` + `content_sha256`
- script bodies → only `script_size` + `script_sha256` + `script_name`
- stdin → only `stdin_size`
- environment variable values → only `environment_keys` (names, sorted)

Paths are passed through the agent's normal cleaning and absolute-path
validation before evaluation, so `/etc/myapp/../shadow` is already collapsed.

## Caveats for policy authors

- **Rego is not a shell parser.** For high assurance, allowlist exact commands,
  approved AAP templates, paths, hashes, or operation types rather than relying
  on broad string-pattern matching against `command`.
- **Avoid time-based allow rules.** `time_unix` is provided, but branching on
  it makes decisions non-reproducible and hard to test. Prefer it for logging
  context, not allow/deny logic.
- **`mode` is a bitmask, not a magnitude.** `input.mode <= 416` does not mean
  "no more permissive than `0o640`" — `0o600` is numerically smaller but
  `0o604` grants world read. Compare with bitwise intent
  (e.g. `bits.and(input.mode, 0o077) == 0` to forbid group/other) rather than
  `<=`.
- **Keep `reason` rules mutually exclusive.** `reason` is a complete rule: if
  two `reason := "..."` definitions are simultaneously true with different
  strings, Rego raises an evaluation conflict — which the agent treats as an
  evaluation error (deny under fail-closed). Either guard each rule so only one
  can match, or chain them with `else` (see `aap-banking.rego`). `allow` is
  safe to define many times — its clauses are simply OR-ed.

## AAP attribution patterns (regulated environments)

When DirQ is driven by Ansible Automation Platform, every operation carries
`aap_user`, `aap_job_template`, and `aap_job_id`. These turn the agent policy
into a change-control enforcement point. Common patterns, all demonstrated in
the AAP example files:

- **Mandate attribution.** Deny anything without a job id / template / user, so
  an operator who obtains a raw admin token cannot bypass the automation
  platform. Everything that changes a host must flow through AAP.

  ```rego
  has_attribution if {
  	input.aap_user != ""
  	input.aap_job_template != ""
  	input.aap_job_id != ""
  }
  ```

- **Segregation of duties.** Map each automation service account to the set of
  templates it may run — the patching robot cannot deploy, the deploy robot
  cannot touch databases.

  ```rego
  account_templates := {
  	"svc-ansible-patching": {"patch-os", "restart-services"},
  	"svc-ansible-deploy":   {"deploy-app", "rollback-app"},
  }
  allow if { account_templates[input.aap_user][input.aap_job_template] }
  ```

- **Host-sensitivity tiers.** Classify hosts from agent tags set at
  provisioning (`env=prod`, `scope=pci`) — never from the request — and require
  the production automation account plus a change-approved template on those
  hosts.

- **Restrict privilege escalation.** Allow `become=true` only for named
  automation accounts and/or specific templates.

- **Break-glass with detection.** Permit one sanctioned emergency template for a
  named responder, and give it a distinctive `reason` so your SIEM can alert on
  every use and reviewers can find them.

  ```rego
  reason := "BREAK-GLASS: emergency template used — review required" if {
  	input.aap_job_template == "break-glass-shell"
  }
  ```

Making `aap_user` trustworthy: by default `aap_user` is self-asserted by the
API caller. Bind API tokens to an `aap_user` allowlist and set
`require_aap_binding=true` on the server (off by default) so it rejects any
request whose `aap_user` the token isn't authorized for — *before* signing.
Then the signed
`aap_user` your policy evaluates is an authenticated identity, not a claim. See
[SECURITY.md](../../SECURITY.md#aap-user-binding). Two limits to keep in mind:
this authenticates the automation *account/token*, not the human behind AAP; and
the `deploy` path does not yet carry `aap_user` to the agent, so deploy
attribution is enforced only by the server binding (the example deploy rule
keys on the destination path instead).

## Example policies

| File | Purpose |
|---|---|
| [`minimal.rego`](minimal.rego) | Minimal allowlist: unprivileged non-prod exec and `/var/log` reads. |
| [`production-aap.rego`](production-aap.rego) | Production: only approved AAP job templates may run exact commands. |
| [`file-paths.rego`](file-paths.rego) | Confine writes to an app config dir; deny reads of credential stores. |
| [`aap-least-privilege.rego`](aap-least-privilege.rego) | Segregation of duties: each AAP service account may run only its assigned templates; require attribution; gate `become`. |
| [`aap-banking.rego`](aap-banking.rego) | Regulated-bank policy: prod/PCI hosts accept only change-approved templates from the prod automation account, break-glass is flagged, secrets are unreadable. |

Each example is compiled and smoke-evaluated by
`internal/agent/policy/examples_test.go`, so a broken example fails CI without
needing the external `opa` binary.

## Testing your policy

With the OPA CLI:

```bash
opa eval -d policy.rego -i input.json 'data.dirq.agent.allow'
```

where `input.json` is a document matching the reference above, for example:

```json
{
  "operation": "exec",
  "command": "systemctl restart nginx",
  "tags": {"env": "prod"},
  "aap_job_template": "restart-nginx",
  "become": true,
  "become_user": "root"
}
```
