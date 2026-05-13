#!/bin/bash
# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Anthony Green <green@moxielogic.com>
#
# Set up the demo fleet — builds images, starts server, launches agents
# with different roles and configurations.
#
# Usage: ./demo/setup.sh
# Teardown: ./demo/setup.sh teardown

set -e
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$SCRIPT_DIR/.."

if [ "$1" = "teardown" ]; then
    echo "=== Tearing down demo ==="
    podman kube down "$SCRIPT_DIR/server-pod.yml" 2>/dev/null || true
    for pod in $(podman pod ls --format '{{.Name}}' | grep '^demo-'); do
        podman pod rm -f "$pod" 2>/dev/null || true
    done
    echo "=== Done ==="
    exit 0
fi

# ── Build base images ──────────────────────────────────

echo "=== Building base images ==="
cd "$PROJECT_DIR"
podman build --target server -t localhost/dirq_dirq-server:latest -f Containerfile .
podman build --target agent -t localhost/dirq_dirq-agent:latest -f Containerfile .

# ── Build demo agent images ────────────────────────────

echo "=== Building demo agent images ==="
for img in healthy vulnerable full-disk webserver database bare; do
    echo "  Building dirq-demo-$img..."
    podman build -t localhost/dirq-demo-$img:latest -f "$SCRIPT_DIR/images/Containerfile.$img" .
done

# ── Tear down any existing demo ────────────────────────

"$0" teardown 2>/dev/null || true

# ── Start server ───────────────────────────────────────

echo "=== Starting server ==="
podman kube play "$SCRIPT_DIR/server-pod.yml"

echo -n "Waiting for server"
for i in $(seq 1 30); do
    if curl -sf http://localhost:8090/healthz > /dev/null 2>&1; then
        echo " ready!"
        break
    fi
    echo -n "."
    sleep 1
done

SERVER_IP=$(podman inspect demo-server-pod-dirq-server \
    --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' 2>/dev/null || echo "")
if [ -z "$SERVER_IP" ]; then
    SERVER_IP="host.containers.internal"
fi
echo "Server IP: $SERVER_IP"

# ── Launch demo fleet ──────────────────────────────────

launch_agent() {
    local name=$1
    local image=$2
    local tags=$3

    local yml=$(mktemp /tmp/demo-agent-XXXXXX.yml)
    cat > "$yml" <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: demo-${name}
  labels:
    app: dirq-demo
spec:
  containers:
    - name: agent
      image: localhost/dirq-demo-${image}:latest
      env:
        - name: DIRQ_SERVER
          value: "${SERVER_IP}:50051"
        - name: DIRQ_LISTEN
          value: ":50052"
        - name: DIRQ_TAGS
          value: "${tags}"
        - name: DIRQ_EXEC_ENABLED
          value: "true"
        - name: DIRQ_TLS_DISABLED
          value: "true"
YAML
    podman kube play "$yml" > /dev/null 2>&1
    rm -f "$yml"
    echo "  $name ($image) — tags: $tags"
}

echo "=== Launching demo fleet ==="

# Production web servers
launch_agent "web-prod-1"  webserver  "env=prod,role=webserver,dc=us-east"
launch_agent "web-prod-2"  webserver  "env=prod,role=webserver,dc=us-east"
launch_agent "web-prod-3"  webserver  "env=prod,role=webserver,dc=eu-west"

# Production databases
launch_agent "db-prod-1"   database   "env=prod,role=database,dc=us-east"
launch_agent "db-prod-2"   database   "env=prod,role=database,dc=eu-west"

# Staging — some vulnerable
launch_agent "web-stg-1"   vulnerable "env=staging,role=webserver,dc=us-east"
launch_agent "app-stg-1"   vulnerable "env=staging,role=appserver,dc=us-east"

# Dev — some bare, some with full disks
launch_agent "dev-1"       bare       "env=dev,role=devbox,dc=us-east"
launch_agent "dev-2"       full-disk  "env=dev,role=devbox,dc=us-east"
launch_agent "dev-3"       healthy    "env=dev,role=devbox,dc=eu-west"

# Healthy prod app servers
launch_agent "app-prod-1"  healthy    "env=prod,role=appserver,dc=us-east"
launch_agent "app-prod-2"  healthy    "env=prod,role=appserver,dc=eu-west"
launch_agent "app-prod-3"  healthy    "env=prod,role=appserver,dc=us-east"
launch_agent "app-prod-4"  healthy    "env=prod,role=appserver,dc=eu-west"

# Extra nodes to force a deeper tree (3 ZLs × 3 children = 9 at depth 1, rest overflow to depth 2)
launch_agent "worker-1"    healthy    "env=prod,role=worker,dc=us-east"
launch_agent "worker-2"    healthy    "env=prod,role=worker,dc=us-east"
launch_agent "worker-3"    healthy    "env=prod,role=worker,dc=eu-west"
launch_agent "worker-4"    healthy    "env=prod,role=worker,dc=eu-west"
launch_agent "worker-5"    healthy    "env=prod,role=worker,dc=us-east"
launch_agent "worker-6"    healthy    "env=prod,role=worker,dc=eu-west"

echo ""
sleep 3
echo "=== Demo fleet ready: $(curl -s http://localhost:8090/api/v1/hosts | python3 -c 'import sys,json; print(len(json.load(sys.stdin)))') agents ==="
echo ""
echo "  Server: http://localhost:8090"
echo "  export DIRQ_SERVER_URL=http://localhost:8090"
echo ""
echo "  Now run: ./demo/demo-1-fleet-overview.sh"
echo "           ./demo/demo-2-security-audit.sh"
echo "           ./demo/demo-3-query-and-remediate.sh"
echo "           ./demo/demo-4-targeted-exec.sh"
echo ""
echo "  Teardown: $0 teardown"
