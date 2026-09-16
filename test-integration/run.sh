#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# End-to-end test: a real server and real agents, in containers, driven
# through the real CLI.
#
# Every Go test in this repo is a unit test — handlers against a mock
# store, dispatchers against fake streams. None of them start a process,
# open a socket, or complete a TLS handshake. This does: it builds the
# shipped images, brings up a server and four agents on a "$RUNTIME" network
# with TLS enabled and token auth on, and drives assertions through the
# `dirq` CLI exactly as an operator would.
#
# What that covers and unit tests cannot: agent registration, role
# assignment and zone-leader election, the gRPC agent stream, mTLS
# issuance during registration, a query reaching an agent and its facts
# coming back, and a command landing on exactly the hosts a query
# selected — plus the CLI itself, which has no other test at all.
#
#   ./test-integration/run.sh          # build, run, tear down
#   KEEP=1 ./test-integration/run.sh   # leave the fleet up to poke at
#
# Exit status is the test result.

set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$PWD"

# podman by default, docker where that is what is installed (CI runners
# vary). Override with CONTAINER_RUNTIME.
RUNTIME="${CONTAINER_RUNTIME:-}"
if [ -z "$RUNTIME" ]; then
  for candidate in podman docker; do
    if command -v "$candidate" >/dev/null 2>&1; then RUNTIME="$candidate"; break; fi
  done
fi
if [ -z "$RUNTIME" ]; then
  echo "neither podman nor docker found" >&2
  exit 69
fi

NET=dirq-itest
SERVER=dirq-server            # must match a SAN on the generated server cert
CERTS="$(mktemp -d)"
BIN="$ROOT/bin/dirq"
FAILURES=0
KEEP="${KEEP:-0}"

AGENTS=(
  # name        tags                          exec
  "web-01       env=prod,role=web             true"
  "web-02       env=prod,role=web             true"
  "db-01        env=staging,role=db           true"
  "locked-01    env=prod,role=web             false"
)

# ── output ───────────────────────────────────────────────

say()  { printf '\n\033[1m── %s\033[0m\n' "$*"; }
pass() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILURES=$((FAILURES + 1)); }

# ── teardown ─────────────────────────────────────────────

# shellcheck disable=SC2329  # invoked via trap
cleanup() {
  local status=$?
  if [ "$status" -ne 0 ] || [ "$FAILURES" -ne 0 ]; then
    say "container logs (test failed)"
    for c in "$SERVER" "${AGENT_NAMES[@]:-}"; do
      [ -n "$c" ] || continue
      printf '\n––– %s –––\n' "$c"
      "$RUNTIME" logs --tail 60 "$c" 2>&1 || true
    done
  fi
  if [ "$KEEP" = "1" ]; then
    say "KEEP=1 — fleet left running. Tear down with:"
    echo "  $RUNTIME rm -f $SERVER ${AGENT_NAMES[*]:-}; $RUNTIME network rm $NET"
    return
  fi
  "$RUNTIME" rm -f "$SERVER" "${AGENT_NAMES[@]:-}" >/dev/null 2>&1 || true
  "$RUNTIME" network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "$CERTS"
}
trap cleanup EXIT

AGENT_NAMES=()
for spec in "${AGENTS[@]}"; do
  read -r name _ _ <<<"$spec"
  AGENT_NAMES+=("$name")
done

# ── build ────────────────────────────────────────────────

say "building images and CLI"
"$RUNTIME" build --quiet --target server -t localhost/dirq-server:itest "$ROOT" >/dev/null
"$RUNTIME" build --quiet --target agent  -t localhost/dirq-agent:itest  "$ROOT" >/dev/null
go build -o "$BIN" ./cmd/dirq
echo "  images and CLI built"

# ── certificates ─────────────────────────────────────────
#
# The generated server cert carries localhost, 127.0.0.1 and dirq-server
# as SANs, so agents can verify it by container DNS name and the CLI can
# verify the same cert over the published port. Both use the real CA —
# nothing here disables verification.

say "generating TLS material"
"$BIN" cert generate --dir "$CERTS" >/dev/null
chmod -R a+rX "$CERTS"
echo "  CA, server and bootstrap agent certs in $CERTS"

# ── bring up the fleet ───────────────────────────────────

"$RUNTIME" rm -f "$SERVER" "${AGENT_NAMES[@]}" >/dev/null 2>&1 || true
"$RUNTIME" network rm "$NET" >/dev/null 2>&1 || true
"$RUNTIME" network create "$NET" >/dev/null

say "starting the server"
"$RUNTIME" run -d --name "$SERVER" --network "$NET" \
  -v "$CERTS:/certs:ro,z" \
  -e DIRQ_GRPC_ADDR=":50051" \
  -e DIRQ_HTTP_ADDR=":8080" \
  -e DIRQ_DB_URL="sqlite:///tmp/dirq-itest.db" \
  -e DIRQ_POD_ID="itest" \
  -e DIRQ_TLS_CA=/certs/ca.crt \
  -e DIRQ_TLS_CERT=/certs/server.crt \
  -e DIRQ_TLS_KEY=/certs/server.key \
  -e DIRQ_TLS_CA_KEY=/certs/ca.key \
  -p 127.0.0.1:18080:8080 \
  localhost/dirq-server:itest >/dev/null

# Wait on a health condition, never a sleep.
deadline=$((SECONDS + 90))
until curl -sf --cacert "$CERTS/ca.crt" https://localhost:18080/healthz >/dev/null 2>&1; do
  if [ "$SECONDS" -ge "$deadline" ]; then
    fail "server never became healthy"
    exit 1
  fi
  sleep 1
done
echo "  server healthy on https://localhost:18080"

# Token auth stays ON. The bootstrap token is what a real operator uses
# on day one, so the CLI is exercised through the same path.
TOKEN="$("$RUNTIME" exec "$SERVER" cat /var/lib/dirq/bootstrap-token 2>/dev/null || true)"
if [ -z "$TOKEN" ]; then
  fail "no bootstrap token was issued"
  exit 1
fi
echo "  bootstrap token retrieved"

# Point the CLI at a config file of our own, so a developer's personal
# server URL, token and tls_insecure setting cannot leak into the run.
cat > "$CERTS/client.conf" <<CONF
server_url: https://localhost:18080
token: $TOKEN
tls_ca: $CERTS/ca.crt
CONF
export DIRQ_CONFIG_FILE="$CERTS/client.conf"
# Everything above comes from that file, including tls_ca — so the CLI
# verifies the server against the same CA the agents use. This used to
# need DIRQ_TLS_INSECURE=true because the CLI had no CA option at all
# (dirq-6gr); every assertion below now runs over a verified connection.

say "starting ${#AGENTS[@]} agents"
for spec in "${AGENTS[@]}"; do
  read -r name tags execEnabled <<<"$spec"
  "$RUNTIME" run -d --name "$name" --network "$NET" \
    -v "$CERTS:/certs:ro,z" \
    -e DIRQ_SERVER="$SERVER:50051" \
    -e DIRQ_HOSTNAME="$name" \
    -e DIRQ_TAGS="$tags" \
    -e DIRQ_EXEC_ENABLED="$execEnabled" \
    -e DIRQ_TLS_CA=/certs/ca.crt \
    -e DIRQ_TLS_CERT=/certs/agent.crt \
    -e DIRQ_TLS_KEY=/certs/agent.key \
    localhost/dirq-agent:itest >/dev/null
  echo "  $name (tags=$tags exec=$execEnabled)"
done

say "waiting for the fleet to register"
deadline=$((SECONDS + 120))
while :; do
  online="$("$BIN" --json hosts list 2>/dev/null \
    | python3 -c 'import sys,json;d=json.load(sys.stdin);print(sum(1 for h in d if h.get("online")))' 2>/dev/null || echo 0)"
  [ "$online" = "${#AGENTS[@]}" ] && break
  if [ "$SECONDS" -ge "$deadline" ]; then
    fail "only $online of ${#AGENTS[@]} agents registered"
    exit 1
  fi
  sleep 2
done
echo "  all ${#AGENTS[@]} agents registered and online"

# Registered and online is not the same as reachable. An agent appears in
# the host list as soon as it registers, but a query is dispatched down
# the gRPC agent streams, and a moment passes before a freshly registered
# agent's stream is connected. Waiting on "online" is waiting on a proxy;
# wait on the condition the assertions actually need — that a trivial
# query comes back with nobody missing. Without this the suite passes on
# a fast machine and fails on a loaded CI runner, which is the worst way
# for a test to be wrong.
say "waiting for the fleet to become answerable"
deadline=$((SECONDS + 120))
while :; do
  answered="$("$BIN" --json select hostname 2>/dev/null \
    | python3 -c 'import sys,json;d=json.load(sys.stdin);print(0 if d.get("missing",0) else len(d.get("results",[])))' 2>/dev/null || echo 0)"
  [ "$answered" = "${#AGENTS[@]}" ] && break
  if [ "$SECONDS" -ge "$deadline" ]; then
    fail "only $answered of ${#AGENTS[@]} agents answered a query"
    exit 1
  fi
  sleep 2
done
echo "  all ${#AGENTS[@]} agents answering queries"

# ── assertions ───────────────────────────────────────────

hosts_json()  { "$BIN" --json hosts list; }
select_json() { "$BIN" --json select "$@"; }

say "registration"
if python3 - "$(hosts_json)" <<'PY'
import json, sys
hosts = json.loads(sys.argv[1])
by = {h["hostname"]: h for h in hosts}
want = {"web-01", "web-02", "db-01", "locked-01"}
assert want <= set(by), f"missing: {want - set(by)}"
assert by["web-01"]["tags"].get("env") == "prod", by["web-01"]["tags"]
assert by["db-01"]["tags"].get("env") == "staging", by["db-01"]["tags"]
assert by["locked-01"]["exec_enabled"] is False, "exec_enabled did not survive registration"
assert by["web-01"]["exec_enabled"] is True
for h in hosts:
    assert h["role"], f"{h['hostname']} has no role"
    assert h["id"], f"{h['hostname']} has no id"
PY
then pass "every agent registered with its own identity, tags and role"
else fail "registration"
fi

say "topology"
if python3 - "$(hosts_json)" <<'PY'
import json, sys
hosts = json.loads(sys.argv[1])
zls = [h["hostname"] for h in hosts if h["role"] == "zone_leader"]
assert zls, f"no zone leader among {[(h['hostname'], h['role']) for h in hosts]}"
PY
then pass "a zone leader was elected"
else fail "zone-leader election"
fi

say "query reaches agents and returns real facts"
if python3 - "$(select_json hostname, os_info.os)" <<'PY'
import json, sys
d = json.loads(sys.argv[1])
assert d.get("missing", 0) == 0, f"missing={d.get('missing')} — agents did not answer"
results = d["results"]
assert len(results) == 4, f"{len(results)} results, want 4"
for r in results:
    assert r["success"], f"{r.get('hostname')}: {r.get('error')}"
    data = r.get("data") or {}
    assert any("os_info" in k for k in data), f"{r['hostname']} returned no os_info: {data}"
PY
then pass "all four agents answered with real facts"
else fail "fleet query"
fi

say "hostname filter narrows the fleet"
if python3 - "$(select_json hostname WHERE hostname = "'web-01'")" <<'PY'
import json, sys
d = json.loads(sys.argv[1])
assert d["total_targets"] == 1, f"total_targets={d['total_targets']}, want 1"
names = [r["hostname"] for r in d["results"]]
assert names == ["web-01"], names
PY
then pass "a hostname filter selected exactly one host"
else fail "hostname targeting"
fi

say "tag filter narrows the fleet"
if python3 - "$(select_json hostname WHERE tag.env = "'staging'")" <<'PY'
import json, sys
d = json.loads(sys.argv[1])
assert d["total_targets"] == 1, f"total_targets={d['total_targets']}, want 1"
names = [r["hostname"] for r in d["results"]]
assert names == ["db-01"], names
PY
then pass "a tag filter selected exactly the tagged host"
else fail "tag targeting"
fi

say "exec runs on the targeted hosts, and only those"
EXEC_OUT="$("$BIN" --json exec WHERE tag.env = "'prod'" -- echo dirq-was-here 2>&1 || true)"
if python3 - "$EXEC_OUT" <<'PY'
import base64, json, sys
lines = [json.loads(l) for l in sys.argv[1].splitlines() if l.strip().startswith("{")]
ran = {l["hostname"]: l for l in lines if l.get("type") != "header" and l.get("hostname")}
# locked-01 carries env=prod but never opted in to exec.
assert "locked-01" not in ran, "a command was dispatched to an agent with exec disabled"
assert "db-01" not in ran, "a command ran on a host the query excluded"
assert set(ran) == {"web-01", "web-02"}, f"ran on {sorted(ran)}, want web-01 and web-02"
for name, l in ran.items():
    assert l.get("success"), f"{name}: {l.get('error')}"
    assert l.get("rc", 0) == 0, f"{name}: rc={l.get('rc')}"
    out = base64.b64decode(l.get("stdout") or "").decode()
    assert "dirq-was-here" in out, f"{name}: stdout={out!r}"
PY
then pass "the command ran on both prod web hosts and nowhere else"
else fail "exec targeting"
fi

# ── result ───────────────────────────────────────────────

say "result"
if [ "$FAILURES" -eq 0 ]; then
  printf '  \033[32mall checks passed\033[0m\n\n'
  exit 0
fi
printf '  \033[31m%d check(s) failed\033[0m\n\n' "$FAILURES"
exit 1
