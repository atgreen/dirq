#!/bin/bash
# SPDX-License-Identifier: MIT
# Demo 5: Ansible Playbook — run a real playbook through the DirQ mesh

export DIRQ_SERVER_URL=http://localhost:8090
DIRQ="./bin/dirq"

echo "╔══════════════════════════════════════════════════════════════╗"
echo "║  Demo 5: Ansible Playbook Through the Mesh                  ║"
echo "║  No SSH. No WinRM. Existing playbooks just work.             ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""

read -p "Press Enter to run the site playbook against all production hosts..."
echo ""
echo '$ dirq run --query "SELECT os_info.hostname FROM tag:env=prod" --playbook demo/playbooks/site.yml --forks 3'
$DIRQ run --query "SELECT os_info.hostname FROM tag:env=prod" --playbook demo/playbooks/site.yml --forks 3
echo ""

echo "✓ Playbook executed through the DirQ mesh — no SSH connections were made."
