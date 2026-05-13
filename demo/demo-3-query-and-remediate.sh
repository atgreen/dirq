#!/bin/bash
# SPDX-License-Identifier: MIT
# Demo 3: Query and Remediate — find a problem, fix it immediately

export DIRQ_SERVER_URL=http://localhost:8090
DIRQ="./bin/dirq"

echo "╔══════════════════════════════════════════════════════════════╗"
echo "║  Demo 3: Query and Remediate                                ║"
echo "║  Find a problem → build an inventory → run a fix             ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""

echo "Scenario: curl is missing on some hosts. Find them and install it."
echo ""

read -p "Press Enter to find hosts WITHOUT curl..."
echo ""
echo "Step 1: Query hosts that DO have curl"
echo '$ dirq query "SELECT os_info.hostname FROM * WHERE packages.name = '"'"'curl'"'"'" --json'
$DIRQ query "SELECT os_info.hostname FROM * WHERE packages.name = 'curl'" --json 2>/dev/null | python3 -c "
import sys, json
r = json.load(sys.stdin)
hosts = [x['hostname'] for x in r['results']]
print(f'  Hosts with curl: {len(hosts)} — {', '.join(hosts)}')
"
echo ""

read -p "Press Enter to install curl on ALL hosts (idempotent — already installed is fine)..."
echo ""
echo 'Step 2: Install curl everywhere'
echo '$ dirq run --query "SELECT os_info.hostname FROM *" --command "dnf install -y curl" --forks 3'
$DIRQ run --query "SELECT os_info.hostname FROM *" --command "dnf install -y curl" --forks 3
echo ""

read -p "Press Enter to verify curl is now everywhere..."
echo ""
echo 'Step 3: Verify'
echo '$ dirq query "SELECT os_info.hostname, packages.name, packages.version FROM * WHERE packages.name = '"'"'curl'"'"'"'
$DIRQ query "SELECT os_info.hostname, packages.name, packages.version FROM * WHERE packages.name = 'curl'"
echo ""

echo "✓ Remediation complete — queried, targeted, fixed, verified."
