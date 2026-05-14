#!/usr/bin/env bash
# deploy/azure-test-fleet.sh — Provision a mixed Windows/Linux test fleet on Azure.
#
# Prerequisites:
#   - az CLI installed and logged in (az login)
#   - Cross-compiled agent binaries in ./bin/:
#       GOOS=linux  GOARCH=amd64 go build -o bin/dirq-agent-linux   ./cmd/dirq-agent
#       GOOS=windows GOARCH=amd64 go build -o bin/dirq-agent.exe    ./cmd/dirq-agent
#       GOOS=linux  GOARCH=amd64 go build -o bin/dirq-server-linux  ./cmd/dirq-server
#
# Usage:
#   ./deploy/azure-test-fleet.sh up          # create everything
#   ./deploy/azure-test-fleet.sh status       # show fleet status
#   ./deploy/azure-test-fleet.sh down         # tear it all down
#
# Defaults: 3 Linux + 2 Windows VMs. Override with:
#   LINUX_COUNT=5 WIN_COUNT=3 ./deploy/azure-test-fleet.sh up

set -euo pipefail

# ─────────────────────────────────────────────────────────
# Configuration
# ─────────────────────────────────────────────────────────

RG="${DIRQ_RG:-dirq-test}"
LOCATION="${DIRQ_LOCATION:-eastus}"
VM_SIZE="${DIRQ_VM_SIZE:-Standard_B2s}"
LINUX_COUNT="${LINUX_COUNT:-3}"
WIN_COUNT="${WIN_COUNT:-2}"
LINUX_IMAGE="${LINUX_IMAGE:-Ubuntu2404}"
WIN_IMAGE="${WIN_IMAGE:-Win2022Datacenter}"
ADMIN_USER="dirq"
ADMIN_PASS="${DIRQ_WIN_PASSWORD:-DirQ-Test-2026!}"
SERVER_VM="dirq-server"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# ─────────────────────────────────────────────────────────
# Helpers
# ─────────────────────────────────────────────────────────

log()  { echo "==> $*"; }
die()  { echo "ERROR: $*" >&2; exit 1; }

check_prereqs() {
    command -v az >/dev/null || die "az CLI not found. Install: https://aka.ms/install-azure-cli"
    az account show >/dev/null 2>&1 || die "Not logged in. Run: az login"

    for f in "$REPO_DIR/bin/dirq-agent-linux" "$REPO_DIR/bin/dirq-agent.exe" "$REPO_DIR/bin/dirq-server-linux"; do
        [[ -f "$f" ]] || die "Missing $f — build with:\n  GOOS=linux GOARCH=amd64 go build -o bin/dirq-agent-linux ./cmd/dirq-agent\n  GOOS=windows GOARCH=amd64 go build -o bin/dirq-agent.exe ./cmd/dirq-agent\n  GOOS=linux GOARCH=amd64 go build -o bin/dirq-server-linux ./cmd/dirq-server"
    done
}

server_ip() {
    az vm show -g "$RG" -n "$SERVER_VM" --show-details --query publicIps -o tsv 2>/dev/null
}

# ─────────────────────────────────────────────────────────
# up — create everything
# ─────────────────────────────────────────────────────────

cmd_up() {
    check_prereqs

    log "Creating resource group $RG in $LOCATION"
    az group create -n "$RG" -l "$LOCATION" -o none

    # Network security group: allow gRPC (50051), REST (8080), SSH (22), RDP (3389)
    log "Creating network security group"
    az network nsg create -g "$RG" -n dirq-nsg -o none
    az network nsg rule create -g "$RG" --nsg-name dirq-nsg -n AllowDirQ \
        --priority 1000 --destination-port-ranges 50051 50052 8080 22 3389 \
        --access Allow --protocol Tcp -o none

    # ── Server VM ──────────────────────────────────────────
    log "Creating server VM ($SERVER_VM)"
    az vm create -g "$RG" -n "$SERVER_VM" \
        --image "$LINUX_IMAGE" --size "$VM_SIZE" \
        --admin-username "$ADMIN_USER" --generate-ssh-keys \
        --nsg dirq-nsg --public-ip-sku Standard \
        -o none

    local srv_ip
    srv_ip=$(server_ip)
    log "Server IP: $srv_ip"

    # Upload server binary and start it.
    log "Deploying server to $srv_ip"
    scp -o StrictHostKeyChecking=no \
        "$REPO_DIR/bin/dirq-server-linux" \
        "$ADMIN_USER@$srv_ip:/tmp/dirq-server"

    # Install postgres, start server.
    ssh -o StrictHostKeyChecking=no "$ADMIN_USER@$srv_ip" bash <<'SERVER_SETUP'
        set -e
        sudo apt-get update -qq
        sudo apt-get install -y -qq postgresql > /dev/null

        # Create database.
        sudo -u postgres createuser dirq 2>/dev/null || true
        sudo -u postgres createdb -O dirq dirq 2>/dev/null || true
        sudo -u postgres psql -c "ALTER USER dirq PASSWORD 'dirq';" > /dev/null

        # Install and start server.
        sudo mv /tmp/dirq-server /usr/local/bin/dirq-server
        sudo chmod +x /usr/local/bin/dirq-server

        sudo tee /etc/sysconfig/dirq-server > /dev/null 2>/dev/null <<EOF || true
DIRQ_GRPC_ADDR=0.0.0.0:50051
DIRQ_HTTP_ADDR=0.0.0.0:8080
DIRQ_DB_URL=postgres://dirq:dirq@localhost:5432/dirq?sslmode=disable
DIRQ_TLS_INSECURE=true
DIRQ_AUTH_DISABLED=true
EOF
        sudo mkdir -p /etc/sysconfig

        sudo tee /etc/systemd/system/dirq-server.service > /dev/null <<EOF
[Unit]
Description=DirQ Server
After=network-online.target postgresql.service

[Service]
Type=simple
ExecStart=/usr/local/bin/dirq-server
Environment=DIRQ_GRPC_ADDR=0.0.0.0:50051
Environment=DIRQ_HTTP_ADDR=0.0.0.0:8080
Environment=DIRQ_DB_URL=postgres://dirq:dirq@localhost:5432/dirq?sslmode=disable
Environment=DIRQ_TLS_INSECURE=true
Environment=DIRQ_AUTH_DISABLED=true
Restart=on-failure
RestartSec=3
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
        sudo systemctl daemon-reload
        sudo systemctl enable --now dirq-server
        sleep 2
        sudo systemctl is-active dirq-server
SERVER_SETUP

    log "Server running at $srv_ip (REST: http://$srv_ip:8080, gRPC: $srv_ip:50051)"

    # ── Linux agent VMs ────────────────────────────────────
    for i in $(seq 1 "$LINUX_COUNT"); do
        local name="linux-$i"
        local tag_env=$( (( i % 2 == 0 )) && echo "staging" || echo "prod" )

        log "Creating Linux VM: $name (tag.env=$tag_env)"
        az vm create -g "$RG" -n "$name" \
            --image "$LINUX_IMAGE" --size "$VM_SIZE" \
            --admin-username "$ADMIN_USER" --generate-ssh-keys \
            --nsg dirq-nsg --public-ip-sku Standard \
            -o none

        local vm_ip
        vm_ip=$(az vm show -g "$RG" -n "$name" --show-details --query publicIps -o tsv)

        scp -o StrictHostKeyChecking=no \
            "$REPO_DIR/bin/dirq-agent-linux" \
            "$ADMIN_USER@$vm_ip:/tmp/dirq-agent"

        ssh -o StrictHostKeyChecking=no "$ADMIN_USER@$vm_ip" bash <<AGENT_SETUP
            set -e
            sudo mv /tmp/dirq-agent /usr/local/bin/dirq-agent
            sudo chmod +x /usr/local/bin/dirq-agent

            sudo tee /etc/systemd/system/dirq-agent.service > /dev/null <<EOF
[Unit]
Description=DirQ Agent
After=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/dirq-agent
Environment=DIRQ_SERVER=$srv_ip:50051
Environment=DIRQ_LISTEN=0.0.0.0:50052
Environment=DIRQ_TAGS=env=$tag_env,os=linux,fleet=azure-test
Environment=DIRQ_EXEC_ENABLED=true
Environment=DIRQ_TLS_INSECURE=true
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
            sudo systemctl daemon-reload
            sudo systemctl enable --now dirq-agent
            sleep 1
            sudo systemctl is-active dirq-agent
AGENT_SETUP

        log "  $name ($vm_ip) — agent running"
    done

    # ── Windows agent VMs ──────────────────────────────────
    for i in $(seq 1 "$WIN_COUNT"); do
        local name="win-$i"
        local tag_env=$( (( i % 2 == 0 )) && echo "staging" || echo "prod" )

        log "Creating Windows VM: $name (tag.env=$tag_env)"
        az vm create -g "$RG" -n "$name" \
            --image "$WIN_IMAGE" --size "$VM_SIZE" \
            --admin-username "$ADMIN_USER" --admin-password "$ADMIN_PASS" \
            --nsg dirq-nsg --public-ip-sku Standard \
            -o none

        log "  Installing agent on $name via RunPowerShellScript"
        az vm run-command invoke -g "$RG" -n "$name" \
            --command-id RunPowerShellScript \
            --scripts "
                \$ErrorActionPreference = 'Stop'

                # Create install directory.
                New-Item -ItemType Directory -Force -Path 'C:\dirq' | Out-Null

                # Download agent binary from the server VM.
                # (In production you'd host this on a blob/artifact server.)
                # For now we use az vm run-command to copy it inline.
            " -o none

        # Upload the agent binary via SCP to the server, then pull from Windows.
        # Simpler approach: use az vm run-command with base64-encoded binary.
        # But that has size limits. Instead, serve it from the server VM temporarily.

        # Copy Windows binary to server VM for download.
        scp -o StrictHostKeyChecking=no \
            "$REPO_DIR/bin/dirq-agent.exe" \
            "$ADMIN_USER@$srv_ip:/tmp/dirq-agent.exe"

        # Serve it with a one-shot python HTTP server on the server VM.
        ssh -o StrictHostKeyChecking=no "$ADMIN_USER@$srv_ip" \
            "cd /tmp && timeout 60 python3 -m http.server 9999 &" 2>/dev/null || true
        sleep 2

        az vm run-command invoke -g "$RG" -n "$name" \
            --command-id RunPowerShellScript \
            --scripts "
                \$ErrorActionPreference = 'Stop'
                New-Item -ItemType Directory -Force -Path 'C:\dirq' | Out-Null

                # Download agent.
                [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
                Invoke-WebRequest -Uri 'http://${srv_ip}:9999/dirq-agent.exe' -OutFile 'C:\dirq\dirq-agent.exe'

                # Set environment variables for the service.
                [Environment]::SetEnvironmentVariable('DIRQ_SERVER', '${srv_ip}:50051', 'Machine')
                [Environment]::SetEnvironmentVariable('DIRQ_LISTEN', '0.0.0.0:50052', 'Machine')
                [Environment]::SetEnvironmentVariable('DIRQ_TAGS', 'env=${tag_env},os=windows,fleet=azure-test', 'Machine')
                [Environment]::SetEnvironmentVariable('DIRQ_EXEC_ENABLED', 'true', 'Machine')
                [Environment]::SetEnvironmentVariable('DIRQ_TLS_INSECURE', 'true', 'Machine')

                # Install as Windows service.
                C:\dirq\dirq-agent.exe install

                # Start the service.
                Start-Service DirQAgent
                Get-Service DirQAgent | Format-Table -Property Name, Status
            " -o none

        log "  $name — agent installed as Windows service"
    done

    # Clean up the temp HTTP server.
    ssh -o StrictHostKeyChecking=no "$ADMIN_USER@$srv_ip" \
        "pkill -f 'python3 -m http.server 9999'" 2>/dev/null || true

    echo
    log "Fleet deployed!"
    echo
    echo "  Server:  http://$srv_ip:8080"
    echo "  Agents:  $LINUX_COUNT Linux + $WIN_COUNT Windows"
    echo
    echo "  Test with:"
    echo "    export DIRQ_SERVER_URL=http://$srv_ip:8080"
    echo "    dirq hosts list"
    echo "    dirq query \"SELECT hostname, os_info.os, cpu.logical_cores\""
    echo "    dirq query \"SELECT hostname WHERE tag.env = 'prod'\""
    echo "    dirq query \"SELECT hostname, packages.name WHERE packages.name LIKE 'open%'\""
    echo
    echo "  Tear down:"
    echo "    $0 down"
    echo
}

# ─────────────────────────────────────────────────────────
# status — show fleet status
# ─────────────────────────────────────────────────────────

cmd_status() {
    local srv_ip
    srv_ip=$(server_ip) || die "Server VM not found. Run '$0 up' first."

    echo "Server: http://$srv_ip:8080"
    echo
    echo "Azure VMs:"
    az vm list -g "$RG" -d --query "[].{Name:name, OS:storageProfile.osDisk.osType, IP:publicIps, State:powerState}" -o table 2>/dev/null || true
    echo
    echo "DirQ agents:"
    DIRQ_SERVER_URL="http://$srv_ip:8080" "$REPO_DIR/bin/dirq" hosts list 2>/dev/null || \
        curl -s "http://$srv_ip:8080/api/v1/hosts" | python3 -m json.tool 2>/dev/null || \
        echo "(could not reach server)"
}

# ─────────────────────────────────────────────────────────
# down — tear it all down
# ─────────────────────────────────────────────────────────

cmd_down() {
    log "Deleting resource group $RG (this takes a few minutes)"
    az group delete -n "$RG" --yes --no-wait
    log "Deletion started (running in background). Resources will be gone in ~5 minutes."
}

# ─────────────────────────────────────────────────────────
# Main
# ─────────────────────────────────────────────────────────

case "${1:-}" in
    up)     cmd_up ;;
    status) cmd_status ;;
    down)   cmd_down ;;
    *)
        echo "Usage: $0 {up|status|down}"
        echo
        echo "Environment variables:"
        echo "  LINUX_COUNT     Number of Linux VMs (default: 3)"
        echo "  WIN_COUNT       Number of Windows VMs (default: 2)"
        echo "  DIRQ_RG         Azure resource group name (default: dirq-test)"
        echo "  DIRQ_LOCATION   Azure region (default: eastus)"
        echo "  DIRQ_VM_SIZE    VM size (default: Standard_B2s)"
        exit 1
        ;;
esac
