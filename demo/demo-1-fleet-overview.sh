#!/bin/bash
# SPDX-License-Identifier: MIT
# Demo 1: Fleet Overview — see what you're managing

export DIRQ_SERVER_URL=http://localhost:8090
DIRQ="./bin/dirq"

echo "╔══════════════════════════════════════════════════════════════╗"
echo "║  Demo 1: Fleet Overview                                     ║"
echo "║  See your entire fleet in seconds — no SSH required          ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""

read -p "Press Enter to list all hosts..."
echo ""
echo "$ dirq hosts list"
$DIRQ hosts list
echo ""

read -p "Press Enter to query CPU and memory across the fleet..."
echo ""
echo '$ dirq query "SELECT os_info.hostname, cpu.logical_cores, memory.pct_used FROM *"'
$DIRQ query "SELECT os_info.hostname, cpu.logical_cores, memory.pct_used FROM *"
echo ""

read -p "Press Enter to see what services are running everywhere..."
echo ""
echo '$ dirq query "SELECT os_info.hostname, services.name, services.state FROM * WHERE services.name IN ('"'"'sshd'"'"', '"'"'nginx'"'"', '"'"'postgresql'"'"')"'
$DIRQ query "SELECT os_info.hostname, services.name, services.state FROM * WHERE services.name IN ('sshd', 'nginx', 'postgresql')"
echo ""

read -p "Press Enter to count hosts by environment..."
echo ""
echo "$ dirq query \"SELECT os_info.os, COUNT(os_info.hostname) FROM * GROUP BY os_info.os\" --json"
$DIRQ query "SELECT os_info.os, COUNT(os_info.hostname) FROM * GROUP BY os_info.os" --json
echo ""

echo "✓ Fleet overview complete — queried $(curl -s $DIRQ_SERVER_URL/api/v1/hosts | python3 -c 'import sys,json; h=json.load(sys.stdin); print(len([x for x in h if x["online"]]))')  hosts in seconds."
