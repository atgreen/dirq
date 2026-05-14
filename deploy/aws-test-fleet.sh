#!/usr/bin/env bash
# deploy/aws-test-fleet.sh — Provision a mixed Windows/Linux test fleet on AWS.
#
# Prerequisites:
#   - aws CLI installed and configured (aws configure)
#   - Cross-compiled agent binaries in ./bin/:
#       GOOS=linux  GOARCH=amd64 go build -o bin/dirq-agent-linux   ./cmd/dirq-agent
#       GOOS=windows GOARCH=amd64 go build -o bin/dirq-agent.exe    ./cmd/dirq-agent
#       GOOS=linux  GOARCH=amd64 go build -o bin/dirq-server-linux  ./cmd/dirq-server
#
# Usage:
#   ./deploy/aws-test-fleet.sh up          # create everything
#   ./deploy/aws-test-fleet.sh status       # show fleet status
#   ./deploy/aws-test-fleet.sh down         # tear it all down
#
# Defaults: 3 Linux + 2 Windows VMs. Override with:
#   LINUX_COUNT=5 WIN_COUNT=3 ./deploy/aws-test-fleet.sh up

set -euo pipefail

# ─────────────────────────────────────────────────────────
# Configuration
# ─────────────────────────────────────────────────────────

REGION="${AWS_REGION:-us-east-1}"
INSTANCE_TYPE="${DIRQ_INSTANCE_TYPE:-t3.small}"
LINUX_COUNT="${LINUX_COUNT:-3}"
WIN_COUNT="${WIN_COUNT:-2}"
KEY_NAME="${DIRQ_KEY_NAME:-dirq-test}"
TAG_PREFIX="dirq-test"
WIN_ADMIN_PASS="${DIRQ_WIN_PASSWORD:-DirQ-Test-2026!}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
STATE_DIR="$REPO_DIR/.dirq-aws-state"

# ─────────────────────────────────────────────────────────
# Helpers
# ─────────────────────────────────────────────────────────

log()  { echo "==> $*"; }
die()  { echo "ERROR: $*" >&2; exit 1; }

aws_() { aws --region "$REGION" "$@"; }

save_state() { echo "$2" > "$STATE_DIR/$1"; }
load_state() { cat "$STATE_DIR/$1" 2>/dev/null || echo ""; }

check_prereqs() {
    command -v aws >/dev/null || die "aws CLI not found. Install: sudo dnf install awscli2"
    aws_ sts get-caller-identity >/dev/null 2>&1 || die "Not logged in. Run: aws configure"

    for f in "$REPO_DIR/bin/dirq-agent-linux" "$REPO_DIR/bin/dirq-agent.exe" "$REPO_DIR/bin/dirq-server-linux"; do
        [[ -f "$f" ]] || die "Missing $f — build with:
  GOOS=linux GOARCH=amd64 go build -o bin/dirq-agent-linux ./cmd/dirq-agent
  GOOS=windows GOARCH=amd64 go build -o bin/dirq-agent.exe ./cmd/dirq-agent
  GOOS=linux GOARCH=amd64 go build -o bin/dirq-server-linux ./cmd/dirq-server"
    done
}

# Look up latest AMI for a given owner/name pattern.
find_ami() {
    local name_pattern="$1" owner="$2"
    aws_ ec2 describe-images \
        --owners "$owner" \
        --filters "Name=name,Values=$name_pattern" "Name=state,Values=available" \
        --query 'sort_by(Images, &CreationDate)[-1].ImageId' \
        --output text
}

wait_instance_running() {
    local id="$1"
    log "  Waiting for $id to be running..."
    aws_ ec2 wait instance-running --instance-ids "$id"
}

wait_instance_ready() {
    local id="$1"
    log "  Waiting for $id status checks..."
    aws_ ec2 wait instance-status-ok --instance-ids "$id" 2>/dev/null || true
}

get_public_ip() {
    local id="$1"
    aws_ ec2 describe-instances --instance-ids "$id" \
        --query 'Reservations[0].Instances[0].PublicIpAddress' --output text
}

get_private_ip() {
    local id="$1"
    aws_ ec2 describe-instances --instance-ids "$id" \
        --query 'Reservations[0].Instances[0].PrivateIpAddress' --output text
}

ssh_cmd() {
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o IdentitiesOnly=yes -o ConnectTimeout=10 \
        -i "$STATE_DIR/$KEY_NAME.pem" "$@"
}

scp_cmd() {
    scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o IdentitiesOnly=yes -o ConnectTimeout=10 \
        -i "$STATE_DIR/$KEY_NAME.pem" "$@"
}

# ─────────────────────────────────────────────────────────
# up — create everything
# ─────────────────────────────────────────────────────────

cmd_up() {
    check_prereqs
    mkdir -p "$STATE_DIR"

    # ── SSH key pair ───────────────────────────────────────
    if [[ ! -f "$STATE_DIR/$KEY_NAME.pem" ]]; then
        log "Creating EC2 key pair: $KEY_NAME"
        aws_ ec2 delete-key-pair --key-name "$KEY_NAME" 2>/dev/null || true
        aws_ ec2 create-key-pair --key-name "$KEY_NAME" \
            --query 'KeyMaterial' --output text > "$STATE_DIR/$KEY_NAME.pem"
        chmod 600 "$STATE_DIR/$KEY_NAME.pem"
    fi

    # ── VPC (use default) ─────────────────────────────────
    local vpc_id
    vpc_id=$(aws_ ec2 describe-vpcs --filters "Name=isDefault,Values=true" \
        --query 'Vpcs[0].VpcId' --output text)
    [[ "$vpc_id" == "None" || -z "$vpc_id" ]] && die "No default VPC found in $REGION"
    log "Using default VPC: $vpc_id"

    # ── Security group ────────────────────────────────────
    local sg_id
    sg_id=$(aws_ ec2 describe-security-groups \
        --filters "Name=group-name,Values=$TAG_PREFIX-sg" "Name=vpc-id,Values=$vpc_id" \
        --query 'SecurityGroups[0].GroupId' --output text 2>/dev/null || echo "None")

    if [[ "$sg_id" == "None" || -z "$sg_id" ]]; then
        log "Creating security group"
        sg_id=$(aws_ ec2 create-security-group \
            --group-name "$TAG_PREFIX-sg" \
            --description "DirQ test fleet" \
            --vpc-id "$vpc_id" \
            --query 'GroupId' --output text)

        # SSH, RDP, DirQ gRPC, DirQ REST, relay port
        aws_ ec2 authorize-security-group-ingress --group-id "$sg_id" \
            --ip-permissions \
            "IpProtocol=tcp,FromPort=22,ToPort=22,IpRanges=[{CidrIp=0.0.0.0/0}]" \
            "IpProtocol=tcp,FromPort=3389,ToPort=3389,IpRanges=[{CidrIp=0.0.0.0/0}]" \
            "IpProtocol=tcp,FromPort=8080,ToPort=8080,IpRanges=[{CidrIp=0.0.0.0/0}]" \
            "IpProtocol=tcp,FromPort=50051,ToPort=50052,IpRanges=[{CidrIp=0.0.0.0/0}]" \
            --output text > /dev/null
    fi
    save_state sg_id "$sg_id"
    log "Security group: $sg_id"

    # ── Find AMIs ─────────────────────────────────────────
    log "Looking up AMIs"
    local linux_ami win_ami
    linux_ami=$(find_ami "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*" "099720109477")
    win_ami=$(find_ami "Windows_Server-2022-English-Full-Base-*" "801119661308")
    [[ "$linux_ami" == "None" ]] && die "Could not find Ubuntu 24.04 AMI in $REGION"
    [[ "$win_ami" == "None" ]] && die "Could not find Windows Server 2022 AMI in $REGION"
    log "  Linux AMI: $linux_ami"
    log "  Windows AMI: $win_ami"

    # ── Server instance ───────────────────────────────────
    log "Launching server instance"
    local srv_id
    srv_id=$(aws_ ec2 run-instances \
        --image-id "$linux_ami" \
        --instance-type "$INSTANCE_TYPE" \
        --key-name "$KEY_NAME" \
        --security-group-ids "$sg_id" \
        --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=$TAG_PREFIX-server},{Key=dirq-fleet,Value=$TAG_PREFIX}]" \
        --query 'Instances[0].InstanceId' --output text)
    save_state server_id "$srv_id"
    wait_instance_running "$srv_id"

    local srv_ip srv_priv_ip
    srv_ip=$(get_public_ip "$srv_id")
    srv_priv_ip=$(get_private_ip "$srv_id")
    save_state server_ip "$srv_ip"
    save_state server_private_ip "$srv_priv_ip"
    log "Server: $srv_id ($srv_ip / $srv_priv_ip)"

    # Wait for SSH to be available.
    log "  Waiting for SSH (can take 1-2 min)..."
    for i in $(seq 1 40); do
        ssh_cmd -o BatchMode=yes "ubuntu@$srv_ip" true 2>/dev/null && break
        sleep 5
    done

    # Upload and start the server.
    log "  Deploying dirq-server"
    scp_cmd "$REPO_DIR/bin/dirq-server-linux" "ubuntu@$srv_ip:/tmp/dirq-server"

    ssh_cmd "ubuntu@$srv_ip" bash <<'SERVER_SETUP'
        set -e
        sudo apt-get update -qq
        sudo apt-get install -y -qq postgresql > /dev/null 2>&1

        sudo -u postgres createuser dirq 2>/dev/null || true
        sudo -u postgres createdb -O dirq dirq 2>/dev/null || true
        sudo -u postgres psql -c "ALTER USER dirq PASSWORD 'dirq';" > /dev/null

        sudo mv /tmp/dirq-server /usr/local/bin/dirq-server
        sudo chmod +x /usr/local/bin/dirq-server

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

    log "  Server running at http://$srv_ip:8080"

    # ── Linux agent instances ─────────────────────────────
    local linux_ids=()
    for i in $(seq 1 "$LINUX_COUNT"); do
        local name="$TAG_PREFIX-linux-$i"
        local tag_env=$( (( i % 2 == 0 )) && echo "staging" || echo "prod" )

        log "Launching Linux agent: $name (env=$tag_env)"
        local inst_id
        inst_id=$(aws_ ec2 run-instances \
            --image-id "$linux_ami" \
            --instance-type "$INSTANCE_TYPE" \
            --key-name "$KEY_NAME" \
            --security-group-ids "$sg_id" \
            --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=$name},{Key=dirq-fleet,Value=$TAG_PREFIX},{Key=dirq-env,Value=$tag_env}]" \
            --query 'Instances[0].InstanceId' --output text)
        linux_ids+=("$inst_id:$tag_env")
        save_state "linux_${i}_id" "$inst_id"
    done

    # Wait for all Linux instances.
    for entry in "${linux_ids[@]}"; do
        local inst_id="${entry%%:*}"
        wait_instance_running "$inst_id"
    done

    # Deploy agent to each Linux instance.
    for idx in "${!linux_ids[@]}"; do
        local entry="${linux_ids[$idx]}"
        local inst_id="${entry%%:*}"
        local tag_env="${entry##*:}"
        local i=$((idx + 1))
        local vm_ip
        vm_ip=$(get_public_ip "$inst_id")

        log "  Deploying agent to linux-$i ($vm_ip)"

        # Wait for SSH (can take 2-3 min on cold starts).
        for attempt in $(seq 1 40); do
            ssh_cmd -o BatchMode=yes "ubuntu@$vm_ip" true 2>/dev/null && break
            sleep 5
        done

        scp_cmd "$REPO_DIR/bin/dirq-agent-linux" "ubuntu@$vm_ip:/tmp/dirq-agent"

        ssh_cmd "ubuntu@$vm_ip" bash <<AGENT_SETUP
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
Environment=DIRQ_SERVER=$srv_priv_ip:50051
Environment=DIRQ_LISTEN=0.0.0.0:50052
Environment=DIRQ_TAGS=env=$tag_env,os=linux,fleet=aws-test
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
        log "    linux-$i — agent running"
    done

    # ── Windows agent instances ────────────────────────────

    if (( WIN_COUNT > 0 )); then
        # Upload the Windows binary to the server for download.
        scp_cmd "$REPO_DIR/bin/dirq-agent.exe" "ubuntu@$srv_ip:/tmp/dirq-agent.exe"
        # Start a temporary HTTP server for Windows to pull from.
        ssh_cmd "ubuntu@$srv_ip" \
            "cd /tmp && nohup timeout 600 python3 -m http.server 9999 > /dev/null 2>&1 &"
        sleep 1
    fi

    local win_ids=()
    for i in $(seq 1 "$WIN_COUNT"); do
        local name="$TAG_PREFIX-win-$i"
        local tag_env=$( (( i % 2 == 0 )) && echo "staging" || echo "prod" )

        log "Launching Windows agent: $name (env=$tag_env)"

        # UserData script runs on first boot as Administrator.
        local userdata
        userdata=$(cat <<WINEOF
<powershell>
\$ErrorActionPreference = 'Stop'

# Set admin password for RDP access.
net user Administrator '${WIN_ADMIN_PASS}' /active:yes

# Open firewall for DirQ relay.
netsh advfirewall firewall add rule name="DirQ Agent" dir=in action=allow protocol=tcp localport=50052

# Wait for network.
Start-Sleep -Seconds 10

# Create install directory.
New-Item -ItemType Directory -Force -Path 'C:\dirq' | Out-Null

# Download agent from server.
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
Invoke-WebRequest -Uri 'http://${srv_priv_ip}:9999/dirq-agent.exe' -OutFile 'C:\dirq\dirq-agent.exe' -UseBasicParsing

# Write config file (avoids Windows service env var issues).
New-Item -ItemType Directory -Force -Path 'C:\ProgramData\dirq' | Out-Null
@"
server: ${srv_priv_ip}:50051
listen: 0.0.0.0:50052
exec_enabled: true

tags:
  env: ${tag_env}
  os: windows
  fleet: aws-test
"@ | Set-Content 'C:\ProgramData\dirq\agent.conf' -Encoding UTF8

# Set TLS insecure via env var (TLS config is handled separately).
[Environment]::SetEnvironmentVariable('DIRQ_TLS_INSECURE', 'true', 'Machine')

# Install and start as Windows service.
C:\dirq\dirq-agent.exe install
Start-Service DirQAgent
</powershell>
WINEOF
)
        local inst_id
        inst_id=$(aws_ ec2 run-instances \
            --image-id "$win_ami" \
            --instance-type "$INSTANCE_TYPE" \
            --key-name "$KEY_NAME" \
            --security-group-ids "$sg_id" \
            --user-data "$userdata" \
            --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=$name},{Key=dirq-fleet,Value=$TAG_PREFIX},{Key=dirq-env,Value=$tag_env}]" \
            --query 'Instances[0].InstanceId' --output text)
        win_ids+=("$inst_id")
        save_state "win_${i}_id" "$inst_id"
        log "  $name: $inst_id (agent installs automatically via UserData)"
    done

    # Wait for Windows instances to be running.
    for inst_id in "${win_ids[@]}"; do
        wait_instance_running "$inst_id"
    done

    # Stop the temp HTTP server.
    if (( WIN_COUNT > 0 )); then
        ssh_cmd "ubuntu@$srv_ip" "pkill -f 'python3 -m http.server 9999'" 2>/dev/null || true
    fi

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
    echo "    dirq query \"SELECT hostname WHERE tag.os = 'windows'\""
    echo
    echo "  Windows VMs take 3-5 minutes for UserData to complete."
    echo "  RDP credentials: Administrator / $WIN_ADMIN_PASS"
    echo
    echo "  Tear down:"
    echo "    $0 down"
    echo
    echo "  Estimated cost: ~\$0.10/hr for the whole fleet"
    echo
}

# ─────────────────────────────────────────────────────────
# status — show fleet status
# ─────────────────────────────────────────────────────────

cmd_status() {
    echo "AWS instances tagged dirq-fleet=$TAG_PREFIX:"
    echo
    aws_ ec2 describe-instances \
        --filters "Name=tag:dirq-fleet,Values=$TAG_PREFIX" "Name=instance-state-name,Values=running,pending" \
        --query 'Reservations[].Instances[].{ID:InstanceId,Name:Tags[?Key==`Name`]|[0].Value,Type:InstanceType,IP:PublicIpAddress,PrivIP:PrivateIpAddress,State:State.Name}' \
        --output table

    local srv_ip
    srv_ip=$(load_state server_ip)
    if [[ -n "$srv_ip" ]]; then
        echo
        echo "DirQ agents (http://$srv_ip:8080):"
        echo
        DIRQ_SERVER_URL="http://$srv_ip:8080" "$REPO_DIR/bin/dirq" hosts list 2>/dev/null || \
            curl -s "http://$srv_ip:8080/api/v1/hosts" 2>/dev/null | python3 -m json.tool 2>/dev/null || \
            echo "(could not reach server — it may still be starting)"
    fi
}

# ─────────────────────────────────────────────────────────
# down — tear it all down
# ─────────────────────────────────────────────────────────

cmd_down() {
    # Find all instances with our tag.
    local instance_ids
    instance_ids=$(aws_ ec2 describe-instances \
        --filters "Name=tag:dirq-fleet,Values=$TAG_PREFIX" \
                  "Name=instance-state-name,Values=running,stopped,pending" \
        --query 'Reservations[].Instances[].InstanceId' --output text)

    if [[ -n "$instance_ids" ]]; then
        log "Terminating instances: $instance_ids"
        aws_ ec2 terminate-instances --instance-ids $instance_ids --output text > /dev/null
        log "Waiting for termination..."
        aws_ ec2 wait instance-terminated --instance-ids $instance_ids
    else
        log "No running instances found"
    fi

    # Delete security group (may need a moment after instances terminate).
    local sg_id
    sg_id=$(load_state sg_id)
    if [[ -n "$sg_id" ]]; then
        log "Deleting security group $sg_id"
        sleep 5
        aws_ ec2 delete-security-group --group-id "$sg_id" 2>/dev/null || \
            log "  (security group may take a moment to delete — retry if needed)"
    fi

    # Delete key pair.
    log "Deleting key pair $KEY_NAME"
    aws_ ec2 delete-key-pair --key-name "$KEY_NAME" 2>/dev/null || true

    # Clean up local state.
    rm -rf "$STATE_DIR"

    log "Done. All resources cleaned up."
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
        echo "  LINUX_COUNT          Number of Linux VMs (default: 3)"
        echo "  WIN_COUNT            Number of Windows VMs (default: 2)"
        echo "  AWS_REGION           AWS region (default: us-east-1)"
        echo "  DIRQ_INSTANCE_TYPE   Instance type (default: t3.small)"
        echo "  DIRQ_KEY_NAME        EC2 key pair name (default: dirq-test)"
        echo "  DIRQ_WIN_PASSWORD    Windows admin password (default: DirQ-Test-2026!)"
        exit 1
        ;;
esac
