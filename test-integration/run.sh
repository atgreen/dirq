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

# The four named agents carry the tags every targeting assertion below
# depends on. The workers exist to make the mesh a real tree: with a
# handful of agents they all become zone leaders and nothing is ever
# relayed, so the architecture's central claim — that a broadcast reaches
# a subtree through its parent — goes untested. Tagged env=test so they
# cannot disturb the prod/staging assertions.
AGENTS=(
  # name        tags                          exec
  "web-01       env=prod,role=web             true"
  "web-02       env=prod,role=web             true"
  "db-01        env=staging,role=db           true"
  "locked-01    env=prod,role=web             false"
)
for i in $(seq -w 1 8); do
  AGENTS+=("worker-$i     env=test,role=worker          true")
done

# Forces a multi-level tree out of that fleet: two agents connect directly
# to the server, everyone else reaches it through a parent. The defaults
# (5 zone leaders) would let a fleet this size stay flat.
MAX_ZONE_LEADERS=2
MAX_CHILDREN=2

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

say "building a package fixture"
PKG=""
if command -v rpmbuild >/dev/null 2>&1; then
  RPMTOP="$CERTS/rpmbuild"
  mkdir -p "$RPMTOP"/{SPECS,BUILD,RPMS,SOURCES,SRPMS}
  cat > "$RPMTOP/SPECS/dirq-itest.spec" <<'SPEC'
Name:           dirq-itest
Version:        1.0.0
Release:        1
Summary:        Fixture package for the DirQ end-to-end test
License:        MIT
BuildArch:      noarch
%description
Installs a marker file so a deploy can be proven to have actually run.
%install
mkdir -p %{buildroot}/usr/share/dirq-itest
echo deployed > %{buildroot}/usr/share/dirq-itest/marker
%files
/usr/share/dirq-itest/marker
SPEC
  if rpmbuild --define "_topdir $RPMTOP" -bb "$RPMTOP/SPECS/dirq-itest.spec" >/dev/null 2>&1; then
    PKG="$(find "$RPMTOP/RPMS" -name '*.rpm' | head -1)"
  fi
fi
if [ -n "$PKG" ]; then
  echo "  built $(basename "$PKG")"
else
  echo "  rpmbuild unavailable — deploy checks will be skipped"
fi

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
  -e DIRQ_MAX_ZONE_LEADERS="$MAX_ZONE_LEADERS" \
  -e DIRQ_MAX_CHILDREN="$MAX_CHILDREN" \
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

# Resolve the server's address on the network once, and give every agent a
# static hosts entry for it. The container runtime's own DNS is an extra
# moving part with nothing to do with what this test checks, and on a CI
# runner it has timed out mid-run — agents registered, then could not
# resolve the same name a second later to open their stream. Agents still
# connect by name, so the server certificate is still verified against its
# dirq-server SAN; only the lookup changes.
SERVER_IP="$("$RUNTIME" inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$SERVER")"
if [ -z "$SERVER_IP" ]; then
  fail "could not determine the server's address on $NET"
  exit 1
fi
echo "  server reachable at $SERVER_IP"

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
    --add-host "$SERVER:$SERVER_IP" \
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

# Online is not the same as reachable, and waiting on the wrong one is how
# this suite passed locally and failed in CI with missing=1. Online is
# written when an agent registers; a broadcast travels down the zone
# leader's gRPC stream, which opens afterwards. Between those two points
# every agent reads as online and a query finds nobody home — a window
# that only opens on a machine slow enough to lose the race.
#
# The API reports both now, so wait on the one that matters rather than
# dispatching a query to find out.
say "waiting for the fleet to become reachable"
deadline=$((SECONDS + 120))
while :; do
  reachable="$("$BIN" --json hosts list 2>/dev/null \
    | python3 -c 'import sys,json;d=json.load(sys.stdin);print(sum(1 for h in d if h.get("reachable")))' 2>/dev/null || echo 0)"
  [ "$reachable" = "${#AGENTS[@]}" ] && break
  if [ "$SECONDS" -ge "$deadline" ]; then
    fail "only $reachable of ${#AGENTS[@]} agents are reachable"
    exit 1
  fi
  sleep 1
done
echo "  all ${#AGENTS[@]} agents reachable"

# ── assertions ───────────────────────────────────────────

hosts_json()  { "$BIN" --json hosts list; }
select_json() { "$BIN" --json select "$@"; }

say "registration"
if EXPECTED="${#AGENTS[@]}" python3 - "$(hosts_json)" <<'PY'
import json, os, sys
EXPECTED = int(os.environ["EXPECTED"])
hosts = json.loads(sys.argv[1])
by = {h["hostname"]: h for h in hosts}
want = {"web-01", "web-02", "db-01", "locked-01"}
assert want <= set(by), f"missing: {want - set(by)}"
assert len(hosts) == EXPECTED, f"{len(hosts)} hosts registered, want {EXPECTED}"
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

say "reachability"
if python3 - "$(hosts_json)" <<'ASSERT'
import json, sys
hosts = json.loads(sys.argv[1])
for h in hosts:
    assert h["online"], f"{h['hostname']} is not online"
    assert h["reachable"], f"{h['hostname']} is online but not reachable"
ASSERT
then pass "every agent is reachable, not merely registered"
else fail "reachability"
fi

say "the mesh forms a real tree"
if MAXZL="$MAX_ZONE_LEADERS" MAXCH="$MAX_CHILDREN" python3 - "$(hosts_json)" <<'ASSERT'
import json, os, sys
hosts = json.loads(sys.argv[1])
max_zl = int(os.environ["MAXZL"])
max_children = int(os.environ["MAXCH"])
by_id = {h["id"]: h for h in hosts}

zls = [h for h in hosts if h["role"] == "zone_leader"]
assert zls, "no zone leader was elected"
assert len(zls) <= max_zl, f"{len(zls)} zone leaders, configured maximum is {max_zl}"

# The point of scaling the fleet: most agents must reach the server
# through a parent, not directly. A flat mesh proves nothing about relays.
parented = [h for h in hosts if h.get("parent_id")]
assert parented, "every agent connects directly; nothing is relayed and the tree is flat"

# And the tree must be deeper than one hop somewhere, or a parent is only
# ever a zone leader and forwarding through an intermediate is untested.
deep = [h for h in parented if by_id.get(h["parent_id"], {}).get("role") != "zone_leader"]
assert deep, "no agent sits below a non-zone-leader; the tree is only one level deep"

# Nobody may exceed the configured fan-out.
children = {}
for h in parented:
    children[h["parent_id"]] = children.get(h["parent_id"], 0) + 1
for pid, n in children.items():
    assert n <= max_children, f"{by_id.get(pid, {}).get('hostname', pid)} has {n} children, max is {max_children}"

print(f"  {len(zls)} zone leaders, {len(parented)} agents behind a parent, {len(deep)} at depth 2+")
ASSERT
then pass "a bounded multi-level tree formed, and every agent is in it"
else fail "topology"
fi

say "query reaches every agent, through relays, and returns real facts"
if EXPECTED="${#AGENTS[@]}" python3 - "$(select_json hostname, os_info.os)" <<'PY'
import json, os, sys
EXPECTED = int(os.environ["EXPECTED"])
d = json.loads(sys.argv[1])
assert d.get("missing", 0) == 0, f"missing={d.get('missing')} — agents did not answer"
results = d["results"]
assert len(results) == EXPECTED, f"{len(results)} results, want {EXPECTED}"
for r in results:
    assert r["success"], f"{r.get('hostname')}: {r.get('error')}"
    data = r.get("data") or {}
    assert any("os_info" in k for k in data), f"{r['hostname']} returned no os_info: {data}"
PY
then pass "every agent answered with real facts, most of them through a relay"
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

# ── more of the CLI ──────────────────────────────────────
#
# Everything above drives hosts list, select and exec. The rest of the CLI
# is only meaningful against a real fleet, so this is the only place it can
# be covered at all.

say "hosts show reports live topology, not the stored record"
if "$BIN" --json hosts show web-01 > "$CERTS/show.json" 2>&1 && python3 - "$CERTS/show.json" <<'ASSERT'
import json, sys
h = json.load(open(sys.argv[1]))
assert h["hostname"] == "web-01", h["hostname"]
assert h["tags"].get("env") == "prod", h["tags"]
assert h["online"], "not online"
# Reachable comes from the live topology overlay; the stored record has no
# such field, so a false here means the endpoint skipped enrichment.
assert h["reachable"], "hosts show reports the agent unreachable"
assert h["role"], "no role"
ASSERT
then pass "hosts show returns the host with its live role and reachability"
else fail "hosts show"
fi

say "hosts facts returns what the agent collected"
if "$BIN" --json hosts facts web-01 > "$CERTS/facts.json" 2>&1 && python3 - "$CERTS/facts.json" <<'ASSERT'
import json, sys
facts = json.load(open(sys.argv[1]))
mods = {f["module"] for f in facts}
assert mods, "no facts cached for the host"
# os_info is collected by every agent on every platform.
assert "os_info" in mods, f"os_info missing from {sorted(mods)}"
for f in facts:
    assert f["data"], f"module {f['module']} cached with no data"
ASSERT
then pass "hosts facts returns real collected facts"
else fail "hosts facts"
fi

say "hosts graph draws the mesh"
GRAPH="$("$BIN" hosts graph 2>&1)"
if printf '%s' "$GRAPH" | grep -q web-01 && printf '%s' "$GRAPH" | grep -q db-01; then
  pass "hosts graph shows the fleet"
else
  fail "hosts graph (got: $GRAPH)"
fi

# Tags decide what every later command targets, so a tag write that does
# not take effect silently redirects real commands.
say "tagging changes what targeting selects"
"$BIN" hosts tag tier=edge WHERE hostname = "'web-01'" >/dev/null 2>&1
TAGGED="$("$BIN" --json select hostname WHERE tag.tier = "'edge'" 2>/dev/null \
  | python3 -c 'import sys,json;d=json.load(sys.stdin);print(",".join(sorted(r["hostname"] for r in d.get("results",[]))))' 2>/dev/null || echo ERR)"
"$BIN" hosts untag tier WHERE hostname = "'web-01'" >/dev/null 2>&1
UNTAGGED="$("$BIN" --json select hostname WHERE tag.tier = "'edge'" 2>/dev/null \
  | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d.get("total_targets",-1))' 2>/dev/null || echo ERR)"
if [ "$TAGGED" = "web-01" ] && [ "$UNTAGGED" = "0" ]; then
  pass "a tag is applied, retargets a query, and is removed again"
else
  fail "tag lifecycle (tagged selected '$TAGGED', after untag total_targets=$UNTAGGED)"
fi

say "a created token authenticates, and a deleted one stops"
NEWTOK="$("$BIN" token create itest-ro --scope readonly 2>&1 | awk '/Token:/ {print $2}')"
if [ -z "$NEWTOK" ]; then
  fail "token create returned no token"
else
  # A fresh config using only the new token, so nothing else can satisfy it.
  cat > "$CERTS/tok.conf" <<CONF
server_url: https://localhost:18080
token: $NEWTOK
tls_ca: $CERTS/ca.crt
CONF
  WORKED=no; REVOKED=no
  DIRQ_CONFIG_FILE="$CERTS/tok.conf" "$BIN" --json hosts list >/dev/null 2>&1 && WORKED=yes
  "$BIN" token delete itest-ro >/dev/null 2>&1
  DIRQ_CONFIG_FILE="$CERTS/tok.conf" "$BIN" --json hosts list >/dev/null 2>&1 || REVOKED=yes
  if [ "$WORKED" = yes ] && [ "$REVOKED" = yes ]; then
    pass "a new token works and stops working once deleted"
  else
    fail "token lifecycle (worked=$WORKED revoked-after-delete=$REVOKED)"
  fi
fi

say "query history records what ran"
if "$BIN" --json queries > "$CERTS/queries.json" 2>&1 && python3 - "$CERTS/queries.json" <<'ASSERT'
import json, sys
qs = json.load(open(sys.argv[1]))
assert qs, "no queries recorded despite several having run"
assert any(q["raw_query"].startswith("SELECT") for q in qs), "no SELECT in the history"
assert any(q.get("target_count", 0) > 0 for q in qs), "every recorded query targeted nothing"
ASSERT
then pass "queries lists the history with real target counts"
else fail "query history"
fi

say "aggregates run across the fleet"
if "$BIN" --json select "COUNT(hostname)" > "$CERTS/agg.json" 2>&1 && EXPECTED="${#AGENTS[@]}" python3 - "$CERTS/agg.json" <<'ASSERT'
import json, os, sys
EXPECTED = int(os.environ["EXPECTED"])
d = json.load(open(sys.argv[1]))
rows = d["results"]
assert len(rows) == 1, f"aggregate returned {len(rows)} rows, want 1"
assert rows[0]["data"]["COUNT(hostname)"] == EXPECTED, rows[0]["data"]
ASSERT
then pass "COUNT aggregates the whole fleet to a single row"
else fail "aggregate query"
fi

say "exec --script uploads and runs a script"
# shellcheck disable=SC2016  # $(hostname) must stay literal: it runs on the agent
printf '#!/bin/sh\necho script-ran-on-$(hostname)\n' > "$CERTS/probe.sh"
SCRIPTOUT="$("$BIN" --json exec WHERE hostname = "'web-01'" --script "$CERTS/probe.sh" 2>&1 || true)"
if python3 - "$SCRIPTOUT" <<'ASSERT'
import base64, json, sys
lines = [json.loads(l) for l in sys.argv[1].splitlines() if l.strip().startswith("{")]
ran = [l for l in lines if l.get("hostname")]
assert len(ran) == 1, f"script ran on {len(ran)} hosts, want 1"
assert ran[0]["success"], ran[0].get("error")
out = base64.b64decode(ran[0].get("stdout") or "").decode()
assert "script-ran-on-" in out, f"stdout={out!r}"
ASSERT
then pass "a local script is uploaded, executed, and its output returned"
else fail "exec --script"
fi

say "doctor reports a healthy deployment"
if "$BIN" doctor > "$CERTS/doctor.txt" 2>&1 && grep -q "Agents online" "$CERTS/doctor.txt"; then
  pass "doctor exits clean and sees the fleet"
else
  fail "doctor ($(tail -3 "$CERTS/doctor.txt" 2>/dev/null))"
fi

say "deploy installs on exactly the targeted hosts"
if [ -z "$PKG" ]; then
  echo "  skipped: no rpmbuild on this machine"
else
  DEPLOY_OUT="$("$BIN" deploy "$PKG" WHERE tag.env = "'prod'" --timeout 60 2>&1 || true)"
  # Which hosts had the package installed, asked of the fleet rather than
  # taken from the deploy's own report.
  INSTALLED="$("$BIN" --json exec -- cat /usr/share/dirq-itest/marker 2>/dev/null \
    | python3 -c '
import sys, json
names = []
for line in sys.stdin:
    line = line.strip()
    if not line.startswith("{"):
        continue
    d = json.loads(line)
    if d.get("hostname") and d.get("success") and d.get("rc") == 0:
        names.append(d["hostname"])
print(",".join(sorted(names)))' 2>/dev/null || echo ERR)"
  if printf '%s' "$DEPLOY_OUT" | grep -q "Broadcasting to 2 host(s)" && [ "$INSTALLED" = "web-01,web-02" ]; then
    pass "the package installed on both prod hosts and on no others"
  else
    fail "deploy (broadcast line: $(printf '%s' "$DEPLOY_OUT" | grep -i broadcast); marker found on: $INSTALLED)"
  fi
fi

say "a playbook runs through the mesh"
if ! command -v ansible-playbook >/dev/null 2>&1; then
  echo "  skipped: ansible-playbook not installed"
else
  cat > "$CERTS/play.yml" <<'YML'
---
- name: DirQ end-to-end playbook
  hosts: all
  gather_facts: false
  tasks:
    - name: Create a marker proving the playbook ran here
      ansible.builtin.copy:
        content: "played\n"
        dest: /tmp/dirq-playbook-marker
        mode: "0644"
YML
  # A real module, not raw: it has to be shipped to the host and executed
  # by Python there, which is the whole point of the connection plugin.
  PLAY_OUT="$("$BIN" run "$CERTS/play.yml" WHERE tag.env = "'prod'" 2>&1 || true)"
  # Ask the fleet what happened rather than trusting the play recap.
  PLAYED="$("$BIN" --json exec -- cat /tmp/dirq-playbook-marker 2>/dev/null \
    | python3 -c '
import sys, json
names = []
for line in sys.stdin:
    line = line.strip()
    if not line.startswith("{"):
        continue
    d = json.loads(line)
    if d.get("hostname") and d.get("success") and d.get("rc") == 0:
        names.append(d["hostname"])
print(",".join(sorted(names)))' 2>/dev/null || echo ERR)"
  if [ "$PLAYED" = "web-01,web-02" ]; then
    pass "an Ansible module ran over the mesh on exactly the targeted hosts"
  else
    fail "playbook (marker found on: $PLAYED)"
  fi
  # locked-01 matches tag.env=prod but has exec disabled. It must be
  # skipped with a reason, not dragged in to fail the run (dirq-az3).
  if printf '%s' "$PLAY_OUT" | grep -q "exec disabled: locked-01"; then
    pass "an agent with exec disabled is skipped by name, not silently targeted"
  else
    fail "exec-disabled host not reported as skipped"
  fi
fi

# ── chaos ────────────────────────────────────────────────
#
# Everything above assumes the fleet stays up. The mesh's resilience
# machinery — stream-loss notification, reparenting, fallback parents,
# reconnect — is the most intricate code here and the happy path never
# touches it. These run last because killing agents degrades the fleet
# for anything after them.
#
# Known gaps deliberately not asserted here, so this stays green and
# honest rather than encoding broken behaviour as correct: the server's
# topology is not updated when an agent fails over (dirq-613), and killing
# a zone leader strands part of the fleet permanently with no replacement
# promoted (dirq-zyc). What IS asserted is the part that must hold
# regardless: the dispatcher never hangs on an agent that has gone, and a
# restarted agent rejoins.

# pickLeaf names an online agent that nobody uses as a parent, so killing
# it disturbs no one else. Chosen at runtime because the tree shape
# differs from run to run.
pickLeaf() {
  "$BIN" --json hosts list 2>/dev/null | python3 -c '
import sys, json
hosts = json.load(sys.stdin)
parents = {h.get("parent_id") for h in hosts if h.get("parent_id")}
for h in hosts:
    if h["online"] and h["id"] not in parents and h["hostname"].startswith("worker-"):
        print(h["hostname"])
        break
'
}

# answeringCount runs a fleet query and reports how many answered, or
# ERR. --timeout 10 so a hang is visible as a slow check rather than a
# minute of silence.
answeringCount() {
  "$BIN" --json select hostname --timeout 10 2>/dev/null \
    | python3 -c 'import sys,json;d=json.load(sys.stdin);print(len(d.get("results",[])))' 2>/dev/null || echo ERR
}

say "chaos: a killed leaf is accounted for, not waited on"
LEAF="$(pickLeaf)"
if [ -z "$LEAF" ]; then
  fail "could not find a leaf agent to kill"
else
  echo "  killing $LEAF"
  "$RUNTIME" kill "$LEAF" >/dev/null 2>&1
  # The dispatcher must stop counting on it promptly. Poll rather than
  # sleep a fixed amount, and bound it well under the 60s hard timeout so
  # "it eventually timed out" cannot pass for "it noticed".
  DEADLINE=$((SECONDS + 90)); SAW=no
  while [ "$SECONDS" -lt "$DEADLINE" ]; do
    GONE="$("$BIN" --json hosts list 2>/dev/null \
      | python3 -c "
import sys, json
hosts = json.load(sys.stdin)
print('yes' if any(h['hostname'] == '$LEAF' and not h['online'] for h in hosts) else 'no')" 2>/dev/null || echo no)"
    [ "$GONE" = yes ] && { SAW=yes; break; }
    sleep 2
  done

  # And a query must come back quickly, without the dead agent in it.
  START=$SECONDS
  ANSWERED="$(answeringCount)"
  ELAPSED=$((SECONDS - START))
  STILL="$("$BIN" --json select hostname --timeout 10 2>/dev/null \
    | python3 -c "
import sys, json
d = json.load(sys.stdin)
print('yes' if any(r['hostname'] == '$LEAF' for r in d.get('results', [])) else 'no')" 2>/dev/null || echo yes)"

  if [ "$SAW" = yes ] && [ "$STILL" = no ] && [ "$ELAPSED" -lt 30 ]; then
    pass "the fleet noticed $LEAF was gone and queries returned in ${ELAPSED}s without it"
  else
    fail "dead-leaf handling (noticed=$SAW still-answering=$STILL query took ${ELAPSED}s, answered=$ANSWERED)"
  fi
fi

say "chaos: a restarted agent rejoins the mesh"
if [ -z "${LEAF:-}" ]; then
  echo "  skipped: no agent was killed"
else
  "$RUNTIME" start "$LEAF" >/dev/null 2>&1
  DEADLINE=$((SECONDS + 120)); BACK=no
  while [ "$SECONDS" -lt "$DEADLINE" ]; do
    BACK="$("$BIN" --json select hostname --timeout 10 2>/dev/null \
      | python3 -c "
import sys, json
d = json.load(sys.stdin)
print('yes' if any(r['hostname'] == '$LEAF' for r in d.get('results', [])) else 'no')" 2>/dev/null || echo no)"
    [ "$BACK" = yes ] && break
    sleep 3
  done
  if [ "$BACK" = yes ]; then
    pass "$LEAF re-registered and answered a query again"
  else
    fail "$LEAF did not rejoin within 120s"
  fi
fi

say "chaos: killing a zone leader does not hang the dispatcher"
ZL="$("$BIN" --json hosts list 2>/dev/null | python3 -c '
import sys, json
for h in json.load(sys.stdin):
    if h["online"] and h["role"] == "zone_leader":
        print(h["hostname"]); break
')"
if [ -z "$ZL" ]; then
  fail "no zone leader to kill"
else
  echo "  killing zone leader $ZL"
  "$RUNTIME" kill "$ZL" >/dev/null 2>&1
  sleep 10
  # The surviving fleet must still answer, and promptly. Agents stranded
  # by dirq-zyc are counted offline, so this asserts that whoever the
  # server still believes is online actually responds — no silent partial,
  # no waiting out the hard timeout.
  START=$SECONDS
  ONLINE="$("$BIN" --json hosts list 2>/dev/null \
    | python3 -c 'import sys,json;print(sum(1 for h in json.load(sys.stdin) if h["online"]))' 2>/dev/null || echo ERR)"
  "$BIN" --json select hostname --timeout 15 > "$CERTS/chaos.json" 2>/dev/null || true
  RESULT="$(python3 - "$CERTS/chaos.json" <<'ASSERT' 2>/dev/null || echo ERR
import json, sys
d = json.load(open(sys.argv[1]))
print(f"{len(d.get('results', []))}/{d.get('total_targets', -1)}/{d.get('missing', 0)}")
ASSERT
)"
  ELAPSED=$((SECONDS - START))
  ANSWERED="${RESULT%%/*}"
  if [ "$RESULT" != ERR ] && [ "$ANSWERED" -gt 0 ] && [ "$ELAPSED" -lt 30 ]; then
    pass "after losing $ZL the fleet still answered ($RESULT answered/targeted/missing) in ${ELAPSED}s"
  else
    fail "zone-leader loss (online=$ONLINE result=$RESULT took ${ELAPSED}s)"
  fi

  # The server's model of the mesh must agree with what it can actually
  # reach. It used not to: agents that failed over to a fallback stayed
  # recorded under the dead parent, so they read as online-but-unreachable
  # while answering queries perfectly well (dirq-613). Reachability is
  # derived from the topology, so this is the assertion that catches the
  # topology going stale after a failover.
  "$BIN" --json hosts list > "$CERTS/postchaos.json" 2>/dev/null || true
  if python3 - "$CERTS/postchaos.json" <<'ASSERT'
import json, sys
hosts = json.load(open(sys.argv[1]))
online = [h for h in hosts if h["online"]]
assert online, "no agents online at all after the zone leader died"
stranded = [h["hostname"] for h in online if not h.get("reachable")]
assert not stranded, f"online but unreachable after failover: {stranded}"
print(f"  {len(online)} online, all of them reachable")
ASSERT
  then pass "every surviving agent reattached and the topology followed it"
  else fail "topology went stale after the failover"
  fi
fi

say "result"
if [ "$FAILURES" -eq 0 ]; then
  printf '  \033[32mall checks passed\033[0m\n\n'
  exit 0
fi
printf '  \033[31m%d check(s) failed\033[0m\n\n' "$FAILURES"
exit 1
