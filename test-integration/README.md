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
- The `dirq` CLI, which has no other test at all

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
