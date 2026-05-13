#!/bin/bash
# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Anthony Green <green@moxielogic.com>
#
# Spin up a local DirQ mesh for testing.
#
# Usage:
#   ./run-mesh.sh 10          # 10 agents
#   ./run-mesh.sh 50          # 50 agents
#   ./run-mesh.sh teardown    # clean up everything

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SKIP_BUILD=0
FORCE_BUILD=0
NUM_AGENTS=10

for arg in "$@"; do
    case "$arg" in
        teardown) ;;
        --no-build) SKIP_BUILD=1 ;;
        --build) FORCE_BUILD=1 ;;
        [0-9]*) NUM_AGENTS="$arg" ;;
    esac
done

# ── Build images if needed ──────────────────────────────

build_images() {
    # Skip build if images exist and --no-build wasn't passed.
    if [ "$SKIP_BUILD" = "1" ]; then
        echo "=== Skipping build (--no-build) ==="
        return
    fi

    # Check if images already exist.
    if podman image exists localhost/dirq_dirq-server:latest && \
       podman image exists localhost/dirq_dirq-agent:latest && \
       [ "$FORCE_BUILD" != "1" ]; then
        echo "=== Images already exist (use --build to force rebuild) ==="
        return
    fi

    echo "=== Building container images ==="
    cd "$SCRIPT_DIR/.."

    podman build --target server -t localhost/dirq_dirq-server:latest -f Containerfile .
    podman build --target agent -t localhost/dirq_dirq-agent:latest -f Containerfile .

    echo "=== Images built ==="
}

# ── Teardown ────────────────────────────────────────────

teardown() {
    echo "=== Tearing down mesh ==="
    podman kube down "$SCRIPT_DIR/mesh.yml" 2>/dev/null || true

    for pod in $(podman pod ls --format '{{.Name}}' | grep '^dirq-agent-'); do
        podman pod rm -f "$pod" 2>/dev/null || true
    done

    echo "=== Mesh torn down ==="
}

if [ "$1" = "teardown" ]; then
    teardown
    exit 0
fi

# ── Start server ────────────────────────────────────────

build_images
teardown

echo "=== Starting server pod ==="
podman kube play "$SCRIPT_DIR/mesh.yml"

# Wait for server to be ready.
echo -n "Waiting for server"
for i in $(seq 1 30); do
    if curl -sf http://localhost:8090/healthz > /dev/null 2>&1; then
        echo " ready!"
        break
    fi
    echo -n "."
    sleep 1
done

# Get the server pod's IP for agents to connect to.
SERVER_IP=$(podman inspect dirq-server-pod --format '{{.NetworkSettings.IPAddress}}' 2>/dev/null || \
            podman inspect dirq-server-pod-dirq-server --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' 2>/dev/null || \
            echo "")

if [ -z "$SERVER_IP" ]; then
    # Fallback: try the pod network
    SERVER_IP=$(podman pod inspect dirq-server-pod --format '{{range .InfraContainerID}}{{end}}' 2>/dev/null || echo "")
    if [ -z "$SERVER_IP" ]; then
        # Last resort: use host
        SERVER_IP="host.containers.internal"
    fi
fi

echo "Server IP: $SERVER_IP"

# ── Start agents ────────────────────────────────────────

echo "=== Starting $NUM_AGENTS agent pods ==="

for i in $(seq 1 "$NUM_AGENTS"); do
    AGENT_YML=$(mktemp /tmp/dirq-agent-XXXXXX.yml)
    sed -e "s/AGENT_NUM/$i/g" -e "s/DIRQ_SERVER_ADDR/$SERVER_IP/g" \
        "$SCRIPT_DIR/agent-pod.yml" > "$AGENT_YML"
    podman kube play "$AGENT_YML" > /dev/null 2>&1
    rm -f "$AGENT_YML"
    echo "  agent-$i started"
done

echo ""
echo "=== Mesh running: 1 server + $NUM_AGENTS agents ==="
echo ""
echo "  Server API:  http://localhost:8090"
echo "  Health:      http://localhost:8090/healthz"
echo ""
echo "  Query:       dirq --server http://localhost:8090 query \"SELECT os_info.hostname FROM *\""
echo "  Hosts:       dirq --server http://localhost:8090 hosts list"
echo ""
echo "  Teardown:    $0 teardown"
echo ""

# Wait a moment for agents to register, then show the fleet.
sleep 3
echo "=== Fleet status ==="
curl -s http://localhost:8090/api/v1/hosts 2>/dev/null | \
    python3 -c "
import sys, json
hosts = json.load(sys.stdin)
print(f'Registered: {len(hosts)} agents')
roles = {}
for h in hosts:
    r = h.get('role', 'unknown')
    roles[r] = roles.get(r, 0) + 1
for r, c in sorted(roles.items()):
    print(f'  {r}: {c}')
" 2>/dev/null || echo "(waiting for agents to register...)"
