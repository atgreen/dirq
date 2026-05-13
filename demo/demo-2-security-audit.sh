#!/bin/bash
# SPDX-License-Identifier: MIT
# Demo 2: Security Audit — find vulnerable packages across the fleet

export DIRQ_SERVER_URL=http://localhost:8090
DIRQ="./bin/dirq"

echo "╔══════════════════════════════════════════════════════════════╗"
echo "║  Demo 2: Security Audit                                     ║"
echo "║  Find vulnerable packages across the entire fleet            ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""

read -p "Press Enter to check OpenSSL versions across all hosts..."
echo ""
echo '$ dirq query "SELECT os_info.hostname, packages.name, packages.version WHERE packages.name = '"'"'openssl'"'"'"'
$DIRQ query "SELECT os_info.hostname, packages.name, packages.version WHERE packages.name = 'openssl'"
echo ""

read -p "Press Enter to find hosts missing curl (bare installs)..."
echo ""
echo "Querying for hosts that DO have curl..."
echo '$ dirq query "SELECT os_info.hostname WHERE packages.name = '"'"'curl'"'"'"'
HAVE_CURL=$($DIRQ query "SELECT os_info.hostname WHERE packages.name = 'curl'" --json | python3 -c "import sys,json; r=json.load(sys.stdin); print(', '.join([x['hostname'] for x in r['results']]))" 2>/dev/null)
ALL_HOSTS=$($DIRQ hosts list --json | python3 -c "import sys,json; print(', '.join([h['hostname'] for h in json.load(sys.stdin) if h['online']]))" 2>/dev/null)
echo "  Hosts WITH curl: $HAVE_CURL"
echo "  All hosts:       $ALL_HOSTS"
echo ""

read -p "Press Enter to check what's installed on production web servers..."
echo ""
echo '$ dirq query "SELECT os_info.hostname, packages.name, packages.version WHERE tag.env = '"'"'prod'"'"' AND packages.name IN ('"'"'nginx'"'"', '"'"'openssl'"'"', '"'"'curl'"'"')"'
$DIRQ query "SELECT os_info.hostname, packages.name, packages.version WHERE tag.env = 'prod' AND packages.name IN ('nginx', 'openssl', 'curl')"
echo ""

read -p "Press Enter to check network interfaces on all hosts..."
echo ""
echo '$ dirq query "SELECT os_info.hostname, network.name, network.addresses"'
$DIRQ query "SELECT os_info.hostname, network.name, network.addresses"
echo ""

echo "✓ Security audit complete — scanned the fleet in seconds, no SSH needed."
