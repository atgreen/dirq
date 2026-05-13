#!/bin/bash
# SPDX-License-Identifier: MIT
# Demo 4: Targeted Execution — run commands against precise host sets

export DIRQ_SERVER_URL=http://localhost:8090
DIRQ="./bin/dirq"

echo "╔══════════════════════════════════════════════════════════════╗"
echo "║  Demo 4: Targeted Execution                                 ║"
echo "║  Run commands against exactly the hosts you care about       ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""

read -p "Press Enter to run 'hostname' on production web servers only..."
echo ""
echo '$ dirq run --query "SELECT os_info.hostname FROM tag:env=prod WHERE packages.name = '"'"'nginx'"'"'" --command "hostname && cat /etc/dirq-role"'
$DIRQ run --query "SELECT os_info.hostname FROM tag:env=prod WHERE packages.name = 'nginx'" --command "hostname && cat /etc/dirq-role"
echo ""

read -p "Press Enter to check disk space on dev hosts..."
echo ""
echo '$ dirq run --query "SELECT os_info.hostname FROM tag:env=dev" --command "df -h /"'
$DIRQ run --query "SELECT os_info.hostname FROM tag:env=dev" --command "df -h /"
echo ""

read -p "Press Enter to run uptime on staging hosts..."
echo ""
echo '$ dirq run --query "SELECT os_info.hostname FROM tag:env=staging" --command "uptime"'
$DIRQ run --query "SELECT os_info.hostname FROM tag:env=staging" --command "uptime"
echo ""

read -p "Press Enter to check what hosts have nginx installed..."
echo ""
echo '$ dirq query "SELECT os_info.hostname, packages.name, packages.version FROM * WHERE packages.name = '"'"'nginx'"'"'"'
$DIRQ query "SELECT os_info.hostname, packages.name, packages.version FROM * WHERE packages.name = 'nginx'"
echo ""

read -p "Press Enter to ping all database servers..."
echo ""
echo '$ dirq run --query "SELECT os_info.hostname FROM tag:role=database" --module ping'
$DIRQ run --query "SELECT os_info.hostname FROM tag:role=database" --module ping
echo ""

echo "✓ Targeted execution complete — every command ran only where it needed to."
