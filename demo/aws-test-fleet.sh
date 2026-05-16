#!/usr/bin/env bash
# deploy/aws-test-fleet.sh — Provision a mixed Windows/Linux test fleet on AWS.
#
# Prerequisites:
#   - aws CLI installed and configured (aws sso login)
#   - GitHub release binaries exist (or override with DIRQ_VERSION)
#
# Usage:
#   ./deploy/aws-test-fleet.sh up          # create everything
#   ./deploy/aws-test-fleet.sh status       # show fleet status
#   ./deploy/aws-test-fleet.sh down         # tear it all down
#
# Defaults: 3 Linux (RHEL 8) + 2 Windows VMs. Override with:
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
REGISTRATION_SECRET="${DIRQ_REGISTRATION_SECRET:-dirq-aws-test-secret}"

# Version for RPM/MSI install. Uses latest GitHub release by default.
DIRQ_VERSION="${DIRQ_VERSION:-}"

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
    aws_ sts get-caller-identity >/dev/null 2>&1 || die "Not logged in. Run: aws sso login"
}

resolve_version() {
    if [[ -n "$DIRQ_VERSION" ]]; then
        return
    fi
    log "Resolving latest DirQ release..."
    DIRQ_VERSION=$(curl -s "https://api.github.com/repos/atgreen/dirq/releases/latest" \
        | grep '"tag_name"' | head -1 | sed 's/.*"v\(.*\)".*/\1/')
    [[ -n "$DIRQ_VERSION" ]] || die "Could not determine latest release version"
    log "  Using version: $DIRQ_VERSION"
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

wait_ssh() {
    local user="$1" ip="$2"
    log "  Waiting for SSH on $ip..."
    for i in $(seq 1 60); do
        ssh_cmd -o BatchMode=yes "$user@$ip" true 2>/dev/null && { sleep 2; return 0; }
        sleep 5
    done
    die "SSH timeout for $ip"
}

# ─────────────────────────────────────────────────────────
# up — create everything
# ─────────────────────────────────────────────────────────

cmd_up() {
    check_prereqs
    resolve_version
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
    linux_ami=$(find_ami "RHEL-8.*_HVM-*-x86_64-*-Hourly*" "309956199498")
    win_ami=$(find_ami "Windows_Server-2022-English-Full-Base-*" "801119661308")
    [[ "$linux_ami" == "None" ]] && die "Could not find RHEL 8 AMI in $REGION"
    [[ "$win_ami" == "None" ]] && die "Could not find Windows Server 2022 AMI in $REGION"
    log "  Linux AMI: $linux_ami (RHEL 8)"
    log "  Windows AMI: $win_ami (Server 2022)"

    # ── Server instance ───────────────────────────────────
    log "Launching server instance (RHEL 8)"
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

    wait_ssh "ec2-user" "$srv_ip"

    # Install and configure the server via RPM.
    log "  Installing dirq-server v$DIRQ_VERSION via RPM"
    ssh_cmd "ec2-user@$srv_ip" bash <<SERVER_SETUP
        set -e
        # Add DirQ RPM repo.
        sudo tee /etc/yum.repos.d/dirq.repo > /dev/null <<'REPO'
[dirq]
name=DirQ
baseurl=https://atgreen.github.io/dirq/rpm-repo/
enabled=1
gpgcheck=0
REPO
        sudo dnf install -y dirq-server dirq-agent dirq

        # Configure server.
        sudo tee /etc/dirq/server.conf > /dev/null <<CONF
grpc_addr: 0.0.0.0:50051
http_addr: 0.0.0.0:8080
db_url: sqlite:///var/lib/dirq/dirq.db
registration_secret: ${REGISTRATION_SECRET}
CONF

        sudo systemctl enable --now dirq-server
        sleep 3
        sudo systemctl is-active dirq-server

        # The server generates /var/lib/dirq/agent.conf with inline TLS certs.
        # Wait for it to appear.
        for i in \$(seq 1 10); do
            [[ -f /var/lib/dirq/agent.conf ]] && break
            sleep 1
        done

        # Configure the local agent using the server-generated config.
        sudo cp /var/lib/dirq/agent.conf /etc/dirq/agent.conf
        echo "exec_enabled: true" | sudo tee -a /etc/dirq/agent.conf > /dev/null
        echo "tags:" | sudo tee -a /etc/dirq/agent.conf > /dev/null
        echo "  env: prod" | sudo tee -a /etc/dirq/agent.conf > /dev/null
        echo "  role: server" | sudo tee -a /etc/dirq/agent.conf > /dev/null
        echo "  fleet: aws-test" | sudo tee -a /etc/dirq/agent.conf > /dev/null
        sudo systemctl enable --now dirq-agent
SERVER_SETUP

    # Download the server-generated configs (root-owned, need sudo).
    log "  Fetching generated configs"
    ssh_cmd "ec2-user@$srv_ip" "sudo cat /var/lib/dirq/agent.conf" > "$STATE_DIR/agent.conf"
    ssh_cmd "ec2-user@$srv_ip" "sudo cat /var/lib/dirq/client.conf" > "$STATE_DIR/client.conf"
    ssh_cmd "ec2-user@$srv_ip" "sudo cat /var/lib/dirq/bootstrap-token" > "$STATE_DIR/bootstrap-token"

    log "  Server running at https://$srv_ip:8080"

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

    # Deploy agent to each Linux instance using the server-generated config.
    for idx in "${!linux_ids[@]}"; do
        local entry="${linux_ids[$idx]}"
        local inst_id="${entry%%:*}"
        local tag_env="${entry##*:}"
        local i=$((idx + 1))
        local vm_ip
        vm_ip=$(get_public_ip "$inst_id")

        log "  Deploying agent to linux-$i ($vm_ip)"
        wait_ssh "ec2-user" "$vm_ip"

        # Copy the server-generated agent.conf (retry — cloud-init may restart sshd).
        for attempt in 1 2 3; do
            scp_cmd "$STATE_DIR/agent.conf" "ec2-user@$vm_ip:/tmp/agent.conf" && break
            sleep 5
        done

        ssh_cmd "ec2-user@$vm_ip" bash <<AGENT_SETUP
            set -e
            # Add DirQ RPM repo and install agent.
            sudo tee /etc/yum.repos.d/dirq.repo > /dev/null <<'REPO'
[dirq]
name=DirQ
baseurl=https://atgreen.github.io/dirq/rpm-repo/
enabled=1
gpgcheck=0
REPO
            sudo dnf install -y dirq-agent

            # Use the server-generated config (has TLS certs inline).
            sudo cp /tmp/agent.conf /etc/dirq/agent.conf
            echo "exec_enabled: true" | sudo tee -a /etc/dirq/agent.conf > /dev/null
            echo "tags:" | sudo tee -a /etc/dirq/agent.conf > /dev/null
            echo "  env: ${tag_env}" | sudo tee -a /etc/dirq/agent.conf > /dev/null
            echo "  role: webserver" | sudo tee -a /etc/dirq/agent.conf > /dev/null
            echo "  fleet: aws-test" | sudo tee -a /etc/dirq/agent.conf > /dev/null

            sudo systemctl enable --now dirq-agent
            sleep 1
            sudo systemctl is-active dirq-agent
AGENT_SETUP
        log "    linux-$i — agent running"
    done

    # ── Windows agent instances ────────────────────────────
    local win_ids=()
    for i in $(seq 1 "$WIN_COUNT"); do
        local name="$TAG_PREFIX-win-$i"
        local tag_env=$( (( i % 2 == 0 )) && echo "staging" || echo "prod" )

        log "Launching Windows agent: $name (env=$tag_env)"

        # Read the agent.conf and base64-encode it for UserData.
        local agent_conf_b64
        agent_conf_b64=$(base64 -w0 "$STATE_DIR/agent.conf")

        local userdata
        userdata=$(cat <<WINEOF
<powershell>
# Log everything for debugging.
Start-Transcript -Path C:\dirq-setup.log -Force

# Set admin password for RDP access.
net user Administrator '${WIN_ADMIN_PASS}' /active:yes

# Open firewall for DirQ relay.
netsh advfirewall firewall add rule name="DirQ Agent" dir=in action=allow protocol=tcp localport=50052

# Wait for network to stabilize.
Start-Sleep -Seconds 15

# Force TLS 1.2 for all downloads.
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

# Download MSI with retries. GitHub redirects can be flaky on fresh Windows.
\$msiUrl = "https://github.com/atgreen/dirq/releases/download/v${DIRQ_VERSION}/dirq-agent-${DIRQ_VERSION}.msi"
\$msiPath = "C:\Windows\Temp\dirq-agent.msi"
\$downloaded = \$false
for (\$i = 1; \$i -le 5; \$i++) {
    try {
        Write-Host "Download attempt \$i..."
        # Use System.Net.WebClient — more reliable than Invoke-WebRequest for redirects.
        (New-Object System.Net.WebClient).DownloadFile(\$msiUrl, \$msiPath)
        if (Test-Path \$msiPath) {
            \$size = (Get-Item \$msiPath).Length
            Write-Host "Downloaded \$size bytes"
            if (\$size -gt 100000) { \$downloaded = \$true; break }
        }
    } catch {
        Write-Host "Download failed: \$_"
    }
    Start-Sleep -Seconds 10
}

if (-not \$downloaded) {
    Write-Host "ERROR: Failed to download MSI after 5 attempts"
    Stop-Transcript
    exit 1
}

# Install MSI.
Write-Host "Installing MSI..."
\$proc = Start-Process msiexec -ArgumentList "/i \$msiPath /qn /l*v C:\dirq-msi.log" -Wait -PassThru
Write-Host "MSI exit code: \$(\$proc.ExitCode)"

# Write the server-generated agent config (has inline TLS certs).
New-Item -ItemType Directory -Force -Path 'C:\ProgramData\dirq' | Out-Null
[System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String('${agent_conf_b64}')) | Set-Content 'C:\ProgramData\dirq\agent.conf' -Encoding UTF8

# Append tags.
Add-Content 'C:\ProgramData\dirq\agent.conf' "exec_enabled: true"
Add-Content 'C:\ProgramData\dirq\agent.conf' "tags:"
Add-Content 'C:\ProgramData\dirq\agent.conf' "  env: ${tag_env}"
Add-Content 'C:\ProgramData\dirq\agent.conf' "  role: iis"
Add-Content 'C:\ProgramData\dirq\agent.conf' "  fleet: aws-test"

# Install and start the agent service.
if (Test-Path "C:\Program Files\DirQ\dirq-agent.exe") {
    & "C:\Program Files\DirQ\dirq-agent.exe" install
    Start-Service DirQAgent
    Write-Host "DirQ agent service started"
} else {
    Write-Host "ERROR: dirq-agent.exe not found after MSI install"
}

Stop-Transcript
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
        log "  $name: $inst_id (installs automatically via UserData)"
    done

    for inst_id in "${win_ids[@]}"; do
        wait_instance_running "$inst_id"
    done

    echo
    log "Fleet deployed!"
    echo
    echo "  Server:  https://$srv_ip:8080"
    echo "  Agents:  $LINUX_COUNT Linux (RHEL 8) + $WIN_COUNT Windows (Server 2022)"
    echo
    echo "  Setup (copy-paste):"
    echo "    cp $STATE_DIR/client.conf ~/.config/dirq/client.conf"
    echo
    echo "  Or manually:"
    echo "    export DIRQ_SERVER_URL=https://$srv_ip:8080"
    echo "    export DIRQ_TLS_INSECURE=true"
    echo "    export DIRQ_TOKEN=$(cat "$STATE_DIR/bootstrap-token")"
    echo
    echo "  Test with:"
    echo "    dirq doctor"
    echo "    dirq hosts list"
    echo "    dirq graph"
    echo "    dirq select hostname, os_info.distro, os_info.distro_version"
    echo "    dirq exec uptime WHERE os_info.os = linux"
    echo "    dirq cve CVE-2026-31431"
    echo
    echo "  Windows VMs take 3-5 minutes for UserData to complete."
    echo "  RDP credentials: Administrator / $WIN_ADMIN_PASS"
    echo
    echo "  Tear down:"
    echo "    make aws-down   (or: $0 down)"
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

    local client_conf="$STATE_DIR/client.conf"
    if [[ -f "$client_conf" ]]; then
        echo
        echo "DirQ fleet status:"
        echo
        dirq --config "$client_conf" hosts list 2>/dev/null || \
            echo "(could not reach server — it may still be starting)"
    fi
}

# ─────────────────────────────────────────────────────────
# down — tear it all down
# ─────────────────────────────────────────────────────────

cmd_down() {
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

    local sg_id
    sg_id=$(load_state sg_id)
    if [[ -n "$sg_id" ]]; then
        log "Deleting security group $sg_id"
        sleep 5
        aws_ ec2 delete-security-group --group-id "$sg_id" 2>/dev/null || \
            log "  (security group may take a moment to delete — retry if needed)"
    fi

    log "Deleting key pair $KEY_NAME"
    aws_ ec2 delete-key-pair --key-name "$KEY_NAME" 2>/dev/null || true

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
        echo "  LINUX_COUNT              Number of Linux VMs (default: 3)"
        echo "  WIN_COUNT                Number of Windows VMs (default: 2)"
        echo "  AWS_REGION               AWS region (default: us-east-1)"
        echo "  DIRQ_INSTANCE_TYPE       Instance type (default: t3.small)"
        echo "  DIRQ_KEY_NAME            EC2 key pair name (default: dirq-test)"
        echo "  DIRQ_WIN_PASSWORD        Windows admin password"
        echo "  DIRQ_REGISTRATION_SECRET Agent registration secret"
        echo "  DIRQ_VERSION             DirQ version (default: latest release)"
        exit 1
        ;;
esac
