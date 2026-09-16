# End-to-end tests

Every Go test in this repo is a unit test: HTTP handlers against a mock
store, dispatchers against fake streams, resolvers against fixtures. None
of them start a process, open a socket, or complete a TLS handshake.

`run.sh` does. It builds the shipped container images, brings up a DirQ
server and four agents on a container network with TLS enabled, and
drives assertions through the real `dirq` CLI the way an operator would.

## What this covers that unit tests cannot

- Agent registration over gRPC, and that tags, hostname and the
  `exec_enabled` capability survive it
- Role assignment and zone-leader election across a real fleet
- TLS between agent and server against a real CA, and the mTLS client
  certificate each agent is issued during registration
- A query leaving the HTTP API, reaching agents, collecting real facts
  from real hosts, and coming back
- A command landing on exactly the hosts a query selected — and on no
  others, including an agent that never opted in to exec
- A package deploy actually installing, on exactly the targeted hosts,
  verified by asking the fleet for the installed marker file rather than
  trusting the deploy's own report
- An Ansible playbook running through the mesh — a real module, shipped
  to the host and executed by Python there, not `raw` — and a file read
  back off an agent, so both directions of file transfer are covered
- Agent-side Rego policy actually refusing an instruction, on an agent
  configured with a policy file, with the refusal distinguishable from a
  command failure
- `dirq cert rotate agent_cert` reissuing every agent's mTLS certificate
  with the fleet still answering afterwards
- Chaos: a killed leaf accounted for without the dispatcher hanging, a
  restarted agent rejoining, and a killed zone leader leaving every
  survivor reattached with the server's topology following the failover
- Much of the `dirq` CLI, which has no other test at all: `hosts
  list/show/facts/graph/tag/untag`, `select` including aggregates,
  `exec` with and without `--script`, `deploy`, `grep`, `run`, `token
  create/list/delete`, `queries`, `doctor`, `cert generate` and
  `cert rotate`

## Shape of the fleet

Thirteen agents on one network. Four carry the tags the targeting
assertions depend on, eight exist to force a real tree
(`max_zone_leaders=2`, `max_children=2`, so most agents reach the server
through a parent rather than directly), and one runs a restrictive policy.
A flat mesh would leave relaying — the architecture's central claim —
entirely untested.

Order matters: the chaos checks run last because they deliberately break
the fleet. Anything asserted after them is asserting against wreckage.

## Running it

```sh
make test-integration          # build, run, tear down
KEEP=1 ./test-integration/run.sh   # leave the fleet up to poke at
```

Needs `podman` or `docker`, Go, `curl` and `python3`. Exit status is the
test result; on failure the container logs are dumped.

## Notes

Every connection in this test is verified. The CLI trusts the generated
CA via `DIRQ_TLS_CA`, the agents verify against the same CA and are
issued mTLS client certificates during registration, and the `curl
--cacert` health check verifies the server certificate independently.

The CLI is pointed at a generated config file via `DIRQ_CONFIG_FILE`, so
a developer's own `~/.config/dirq/client.conf` — their server URL, token
and `tls_insecure` — cannot leak into the run.

`test-mesh/` and `demo/` are manual harnesses for poking at a fleet by
hand. They are not tests and nothing runs them.
